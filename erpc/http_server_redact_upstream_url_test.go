package erpc

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An upstream whose API key sits in the URL path, as Infura's does. The tests
// below drive it through the real HTTP server and check every surface that
// leaves the process: the response body a client gets, the log output and
// the exported metrics.
const (
	redactE2ESentinel = "SENTINEL_KEY_123"
	redactE2EHost     = "example.invalid"
	redactE2EEndpoint = "https://" + redactE2EHost + "/v3/" + redactE2ESentinel
	redactE2EUpstream = "redact-e2e-ups"
)

type redactLogSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *redactLogSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *redactLogSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// redactRouter stands in for DNS and the network. It sends every request for
// example.invalid to a healthy node, except the method under test, which goes
// to the failing one. The upstream therefore bootstraps (chain id check, state
// poller) and only the client's call meets the failure. Every other host, such
// as the fixture's own client, goes out untouched.
//
// perAttempt reproduces net/http's own Client.Timeout: the request fails with
// a *url.Error that wraps context.DeadlineExceeded while the request's own
// context is still live. That is the shape that leaked the key in production.
// Test-mode clients run on http.DefaultTransport with no Client.Timeout, so
// this package cannot set the real one.
type redactRouter struct {
	healthy, failing, other http.RoundTripper
	failMethod              string
	perAttempt              time.Duration
}

func (rr *redactRouter) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Hostname() != redactE2EHost {
		return rr.other.RoundTrip(r)
	}
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	if !strings.Contains(string(body), `"`+rr.failMethod+`"`) {
		return rr.healthy.RoundTrip(r)
	}
	if rr.perAttempt == 0 {
		return rr.failing.RoundTrip(r)
	}
	ctx, cancel := context.WithTimeout(r.Context(), rr.perAttempt)
	resp, err := rr.failing.RoundTrip(r.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// dialTo builds a transport that connects to addr whatever the URL says.
func dialTo(t *testing.T, addr string, verifyTLS bool) *http.Transport {
	t.Helper()
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
		// The test servers' certificates do not name example.invalid, so the
		// "good TLS" paths skip verification. The TLS-error mode keeps it.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: !verifyTLS}, //nolint:gosec
	}
	t.Cleanup(tr.CloseIdleConnections)
	return tr
}

// healthyNode answers what bootstrap and the state poller ask for.
func healthyNode(t *testing.T) string {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		result := `"0x0"`
		switch {
		case strings.Contains(string(body), `"eth_chainId"`):
			result = `"0x7b"`
		case strings.Contains(string(body), `"eth_blockNumber"`):
			result = `"0x100"`
		case strings.Contains(string(body), `"eth_syncing"`):
			result = `false`
		case strings.Contains(string(body), `"eth_getBlockByNumber"`):
			result = `{"number":"0x100","hash":"0x` + strings.Repeat("ab", 32) + `","timestamp":"0x6553f100"}`
		}
		id := "1"
		if i := strings.Index(string(body), `"id":`); i >= 0 {
			rest := string(body)[i+5:]
			if j := strings.IndexAny(rest, ",}"); j > 0 {
				id = rest[:j]
			}
		}
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":`+id+`,"result":`+result+`}`)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

func redactE2EConfig() *common.Config {
	fs := []*common.FailsafeConfig{{
		MatchMethod: "*",
		Timeout:     &common.TimeoutPolicyConfig{Duration: common.NewStaticDuration(3 * time.Second)},
	}}
	return &common.Config{
		Server: &common.ServerConfig{MaxTimeout: common.Duration(10 * time.Second).Ptr()},
		Projects: []*common.ProjectConfig{{
			Id: "test_project",
			Networks: []*common.NetworkConfig{{
				Architecture: "evm",
				Evm:          &common.EvmNetworkConfig{ChainId: 123},
				Failsafe:     fs,
			}},
			Upstreams: []*common.UpstreamConfig{{
				Id:       redactE2EUpstream,
				Endpoint: redactE2EEndpoint,
				Type:     common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{
					ChainId:             123,
					StatePollerInterval: common.Duration(10 * time.Second),
				},
				Failsafe: fs,
			}},
		}},
	}
}

func gatherMetricsText(t *testing.T) string {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	var sb strings.Builder
	for _, mf := range mfs {
		sb.WriteString(mf.String())
		sb.WriteByte('\n')
	}
	return sb.String()
}

func TestHttpServer_UpstreamEndpointKeyNeverLeaves(t *testing.T) {
	type mode struct {
		name string
		// dial is where the transport connects instead of example.invalid.
		dial func(t *testing.T) string
		// verifyTLS keeps certificate checks on; the test server's
		// certificate does not name example.invalid, so that fails.
		verifyTLS  bool
		perAttempt time.Duration
		// bodyControl must appear in the client's response body. It proves
		// the upstream failure reached the client, so a missing sentinel is
		// a redaction and not an error that never mentioned the upstream.
		bodyControl string
	}

	tlsServer := func(h http.HandlerFunc) func(t *testing.T) string {
		return func(t *testing.T) string {
			srv := httptest.NewTLSServer(h)
			t.Cleanup(srv.Close)
			return srv.Listener.Addr().String()
		}
	}
	slow := tlsServer(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})
	status := func(code int, body string) func(t *testing.T) string {
		return tlsServer(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_, _ = io.WriteString(w, body)
		})
	}

	modes := []mode{
		{name: "timeout", dial: slow, perAttempt: 300 * time.Millisecond, bodyControl: redactE2EHost},
		{name: "connection refused", dial: func(t *testing.T) string {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			addr := l.Addr().String()
			require.NoError(t, l.Close())
			return addr
		}, bodyControl: redactE2EHost},
		{name: "tls error", dial: tlsServer(func(http.ResponseWriter, *http.Request) {}), verifyTLS: true, bodyControl: redactE2EHost},
		{name: "http 401", dial: status(http.StatusUnauthorized,
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"invalid project id MARKER401"}}`),
			bodyControl: "MARKER401"},
		{name: "http 500", dial: status(http.StatusInternalServerError, `upstream exploded MARKER500`),
			bodyControl: "MARKER500"},
	}

	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()

			// Test-mode clients take http.DefaultTransport when built, so it
			// must be in place before the fixture starts.
			prevTransport := http.DefaultTransport
			http.DefaultTransport = &redactRouter{
				healthy:    dialTo(t, healthyNode(t), false),
				failing:    dialTo(t, m.dial(t), m.verifyTLS),
				other:      prevTransport,
				failMethod: "eth_getBalance",
				perAttempt: m.perAttempt,
			}
			t.Cleanup(func() { http.DefaultTransport = prevTransport })

			logs := &redactLogSink{}
			prevLogger := log.Logger
			prevLevel := zerolog.GlobalLevel()
			log.Logger = zerolog.New(logs).Level(zerolog.TraceLevel)
			zerolog.SetGlobalLevel(zerolog.TraceLevel)
			t.Cleanup(func() {
				log.Logger = prevLogger
				zerolog.SetGlobalLevel(prevLevel)
			})

			sendRequest, _, _, shutdown, _ := createServerTestFixtures(redactE2EConfig(), t)
			defer shutdown()

			_, _, body := sendRequest(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x0000000000000000000000000000000000000001","0x1"]}`, nil, nil)

			metrics := gatherMetricsText(t)
			logText := logs.String()

			for surface, s := range map[string]string{
				"response body": body,
				"logs":          logText,
				"metrics":       metrics,
			} {
				assert.NotContains(t, s, redactE2ESentinel, "%s leaks the endpoint key", surface)
				assert.NotContains(t, s, redactE2EHost+"/v3", "%s leaks the endpoint path", surface)
			}

			// Controls: each surface really carried this upstream's failure.
			assert.Contains(t, body, m.bodyControl, "the response body must carry the upstream failure: %s", body)
			assert.Contains(t, logText, redactE2EHost, "the logs must still name the host")
			assert.Contains(t, metrics, redactE2EUpstream, "the metrics must carry this upstream")
		})
	}
}
