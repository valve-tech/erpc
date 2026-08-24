// Command valve-relay serves the valve billing path in front of an eRPC.
//
// It is deliberately thin. Everything worth testing lives in valverelay; this
// file selects a backend, wires the billing module, and maps one HTTP request
// onto one call to valverelay.Forward.
//
// Two backends, one billed path:
//
//	valve-relay -backend embedded -config erpc.yaml -project main
//	valve-relay -backend http -upstream http://127.0.0.1:4000 -project main
//
// Billing is off unless VALVE_BILLING_ENABLED is set. With it clear the binary
// is a passthrough proxy: no Redis connection is opened and no pricing file is
// read.
//
// # What this binary is NOT
//
// It is not the production relay's edge. It does not authenticate anybody: it
// reads the paying account and the hashed key from request headers and trusts
// them, because auth and API-key resolution are not ported into this fork (see
// the valverelay package doc). Put it behind something that sets those headers
// and strips whatever a client sent, or do not expose it.
//
// It also applies no tiering policy. Every request carries zero rate limits,
// which leaves the script's rate gates disabled; the credit-balance gate still
// applies.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/valvebilling"
	"github.com/erpc/erpc/valverelay"
	"github.com/rs/zerolog"
	"github.com/spf13/afero"
)

// The headers that stand in for the auth layer this fork does not have.
const (
	headerAccountID = "X-Valve-Account-Id"
	headerKeyID     = "X-Valve-Key-Id"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "valve-relay: %v\n", err)
		os.Exit(1)
	}
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func run() error {
	backendKind := flag.String("backend", env("VALVE_RELAY_BACKEND", "http"), "where requests go: embedded | http")
	listen := flag.String("listen", env("VALVE_RELAY_LISTEN", ":8081"), "listen address")
	projectID := flag.String("project", env("VALVE_RELAY_PROJECT", ""), "eRPC project id (required)")
	configPath := flag.String("config", env("VALVE_RELAY_CONFIG", ""), "eRPC config file (embedded backend)")
	upstream := flag.String("upstream", env("VALVE_RELAY_UPSTREAM", ""), "eRPC base URL (http backend)")
	timeout := flag.Duration("timeout", 30*time.Second, "per-request upstream timeout (http backend)")
	pricesPath := flag.String("prices", env("VALVE_BILLING_PRICES", ""), "pricing rows export (required when billing is enabled)")
	methodCUPath := flag.String("method-cu", env("VALVE_BILLING_METHOD_CU", ""), "compute-unit export (required when billing is enabled)")
	flag.Parse()

	logger := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}).With().Timestamp().Logger()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	billing, err := newBilling(ctx, *pricesPath, *methodCUPath)
	if err != nil {
		return err
	}
	defer func() { _ = billing.Close() }()
	logger.Info().Bool("billing", billing.Enabled()).Msg("billing module")

	backend, err := newBackend(ctx, &logger, *backendKind, *projectID, *configPath, *upstream, *timeout)
	if err != nil {
		return err
	}
	defer func() { _ = backend.Close() }()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /evm/{chainId}", handler(&logger, billing, backend))

	srv := &http.Server{Addr: *listen, Handler: mux}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	logger.Info().Str("listen", *listen).Str("backend", *backendKind).Msg("valve-relay listening")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// newBilling builds the module, or returns nil when the flag is clear.
//
// The pricing files are read only when billing is enabled, so a passthrough
// deployment needs neither of them and cannot be broken by a stale one.
func newBilling(ctx context.Context, pricesPath, methodCUPath string) (*valvebilling.Module, error) {
	cfg, err := valvebilling.LoadConfigFromEnv()
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, nil
	}
	if pricesPath == "" || methodCUPath == "" {
		return nil, fmt.Errorf("billing is enabled but -prices and -method-cu are not both set; refusing to guess prices")
	}
	prices, err := valverelay.LoadPriceTable(pricesPath, methodCUPath)
	if err != nil {
		return nil, err
	}
	return valvebilling.New(ctx, cfg, prices)
}

func newBackend(ctx context.Context, logger *zerolog.Logger, kind, projectID, configPath, upstream string, timeout time.Duration) (valverelay.Backend, error) {
	switch kind {
	case "embedded":
		if configPath == "" {
			return nil, fmt.Errorf("the embedded backend needs -config")
		}
		cfg, err := common.LoadConfig(afero.NewOsFs(), configPath, &common.DefaultOptions{})
		if err != nil {
			return nil, fmt.Errorf("loading %s: %w", configPath, err)
		}
		return valverelay.NewEmbeddedBackend(ctx, logger, cfg, projectID)
	case "http":
		if upstream == "" {
			return nil, fmt.Errorf("the http backend needs -upstream")
		}
		return valverelay.NewHTTPBackend(upstream, projectID, timeout)
	default:
		return nil, fmt.Errorf("unknown backend %q: use embedded or http", kind)
	}
}

func handler(logger *zerolog.Logger, billing *valvebilling.Module, backend valverelay.Backend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chainID, err := strconv.ParseInt(r.PathValue("chainId"), 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "chain id must be a number")
			return
		}
		body, err := readBody(w, r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		method, err := methodOf(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		req := valverelay.Request{
			ChainID:   chainID,
			Method:    method,
			AccountID: r.Header.Get(headerAccountID),
			KeyID:     r.Header.Get(headerKeyID),
		}
		if billing.Enabled() && (req.AccountID == "" || req.KeyID == "") {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("billing is enabled: %s and %s are required", headerAccountID, headerKeyID))
			return
		}

		// Forwarding stands alone; billing wraps it. Dropping the wrapper —
		// if the billing path ever moves off the hot path — is a change to
		// this one call, not to the backend or to the handler around it.
		res, err := valverelay.Bill(r.Context(), billing, req, func(ctx context.Context) ([]byte, error) {
			return backend.Forward(ctx, chainID, body)
		})
		if err != nil {
			var rejected *valverelay.RejectedError
			if errors.As(err, &rejected) {
				// One status for every rejection. The set of codes lives in
				// the Lua script and grows there, so a table mapping each one
				// to its own status would go stale silently. The code itself
				// is in the body.
				writeError(w, http.StatusPaymentRequired, rejected.Verdict.Code)
				return
			}
			logger.Error().Err(err).Int64("chain", chainID).Str("method", method).Msg("request failed")
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		if res.CaptureErr != nil {
			// The customer has their answer. This is a ledger fault, not a
			// request fault, and it is logged with the billed weight that
			// actually landed: zero.
			logger.Error().Err(res.CaptureErr).
				Str("account", req.AccountID).
				Str("method", method).
				Msg("capture failed; the answer was still delivered and nothing was billed")
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(res.Body)
	}
}

// maxRequestBytes bounds what one request may post.
const maxRequestBytes = 8 << 20

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()
	return io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
}

// methodOf reads the JSON-RPC method name.
//
// A batch is refused rather than billed. Pricing a batch means resolving a
// cost per element and the per-method buckets that go with it, and this seam
// does not do that — answering "the batch costs one call" would undercharge
// silently.
func methodOf(body []byte) (string, error) {
	trimmed := strings.TrimLeft(string(body), " \t\r\n")
	if strings.HasPrefix(trimmed, "[") {
		return "", errors.New("batch requests are not billed by this relay")
	}
	var req struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return "", fmt.Errorf("body is not a JSON-RPC request: %w", err)
	}
	if req.Method == "" {
		return "", errors.New("body has no method")
	}
	return req.Method, nil
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
