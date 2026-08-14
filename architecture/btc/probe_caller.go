package btc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/erpc/erpc/common"
)

// maxProbeBody caps how much of a probe response is read.
//
// A probe talks to an endpoint that may be misconfigured, wrong, or hostile —
// a URL pointing at something that streams forever would otherwise pin memory
// on every poll tick. getblockchaininfo is well under a kilobyte.
const maxProbeBody = 1 << 20 // 1 MiB

// HttpProbeCaller issues probe calls to a bitcoind JSON-RPC endpoint over
// HTTP POST.
//
// Separate from eRPC's request-path client on purpose. The request path
// carries failsafe policies, rate-limit budgets, cordoning and metrics — all
// of which a health probe must NOT trip. A probe that consumed the upstream's
// rate-limit budget or opened its circuit breaker would turn the health check
// into the outage it is meant to detect.
type HttpProbeCaller struct {
	// Endpoint is the bitcoind RPC URL, including credentials if the node
	// uses HTTP basic auth (bitcoind's default).
	Endpoint string
	// Client is the HTTP client to use. Required — the caller owns the
	// timeout, and a probe without one can hang a poll loop forever.
	Client *http.Client
}

// NewProbeCaller builds the probe transport for a bitcoind endpoint, which is
// how Family satisfies common.ProbeTransportFactory.
//
// The upstream layer owns the HTTP client (and therefore the timeout) but must
// not own the DIALECT — the JSON-RPC 1.0 envelope and the HTTP-500-with-an-
// error-body rule below are bitcoind's, not eRPC's. Building the caller here is
// what keeps them out of chain-agnostic code.
func (f *Family) NewProbeCaller(endpoint string, client *http.Client) common.ProbeCaller {
	return &HttpProbeCaller{Endpoint: endpoint, Client: client}
}

// jsonRpcEnvelope is bitcoind's response shape.
type jsonRpcEnvelope struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// CallJsonRpc posts one JSON-RPC call and returns the raw `result`.
func (h *HttpProbeCaller) CallJsonRpc(ctx context.Context, method string, params []interface{}) ([]byte, error) {
	if h == nil || h.Client == nil {
		return nil, fmt.Errorf("btc probe caller: nil client (a probe without a timeout can hang the poll loop)")
	}
	if params == nil {
		// bitcoind rejects a null params field on some versions; send [].
		params = []interface{}{}
	}
	body, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "1.0",
		"id":      "erpc-probe",
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return nil, fmt.Errorf("btc probe: encode %s: %w", method, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("btc probe: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("btc probe: %s: %w", method, err)
	}
	defer func() {
		// Drain before closing so the connection can be reused; a probe runs
		// on a tick, and leaking a connection per tick exhausts the pool.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxProbeBody))
		_ = resp.Body.Close()
	}()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeBody))
	if err != nil {
		return nil, fmt.Errorf("btc probe: read %s: %w", method, err)
	}

	// bitcoind answers 500 WITH a valid JSON-RPC error body for RPC-level
	// failures, so the status alone is not the verdict — decode first and
	// prefer the error message, which names the actual problem.
	var env jsonRpcEnvelope
	decodeErr := json.Unmarshal(raw, &env)
	if decodeErr == nil && env.Error != nil {
		return nil, fmt.Errorf("btc probe: %s: rpc error %d: %s", method, env.Error.Code, env.Error.Message)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("btc probe: %s: http %d", method, resp.StatusCode)
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("btc probe: decode %s envelope: %w", method, decodeErr)
	}
	if len(env.Result) == 0 {
		return nil, fmt.Errorf("btc probe: %s: empty result", method)
	}
	return env.Result, nil
}

// CallREST always fails. Bitcoin's family is JSON-RPC; a REST call here means
// a caller reached for the wrong transport, and returning a plausible zero
// value would hide that.
func (h *HttpProbeCaller) CallREST(context.Context, string, string) (int, []byte, error) {
	return 0, nil, fmt.Errorf("btc: REST transport is not supported by this family")
}
