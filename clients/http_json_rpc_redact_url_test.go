package clients

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// The upstream endpoint carries its API key in the path, as Infura does. Every
// surface below must hide the key and still show the host.
const (
	redactSentinel = "SENTINEL_KEY_123"
	redactHost     = "example.invalid"
	redactEndpoint = "https://" + redactHost + "/v3/" + redactSentinel
)

// lockedBuffer is a log sink that the client's goroutines can share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// redactClient builds a client for redactEndpoint whose transport dials
// dialAddr instead of resolving example.invalid. The URL, and so every error
// net/http builds, still names the sentinel endpoint.
func redactClient(t *testing.T, dialAddr string, verifyTLS bool, clientTimeout time.Duration, batch bool, logs io.Writer) *GenericHttpJsonRpcClient {
	t.Helper()
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	prevLevel := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.TraceLevel)
	t.Cleanup(func() { zerolog.SetGlobalLevel(prevLevel) })
	lg := zerolog.New(logs).Level(zerolog.TraceLevel)

	u, err := url.Parse(redactEndpoint)
	require.NoError(t, err)

	var cfg *common.JsonRpcUpstreamConfig
	if batch {
		cfg = &common.JsonRpcUpstreamConfig{
			SupportsBatch: util.BoolPtr(true),
			BatchMaxSize:  10,
			BatchMaxWait:  common.Duration(5 * time.Millisecond),
		}
	}

	ups := common.NewFakeUpstream("rpc-redact")
	ups.Config().Type = common.UpstreamTypeEvm
	ups.Config().Endpoint = redactEndpoint
	ups.Config().JsonRpc = cfg

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	c, err := NewGenericHttpJsonRpcClient(ctx, &lg, "prj1", ups, u, cfg, nil, &noopErrorExtractor{})
	require.NoError(t, err)
	gc := c.(*GenericHttpJsonRpcClient)

	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, dialAddr)
		},
		// The test server's certificate does not name example.invalid, so the
		// "good TLS" cases skip verification. The TLS-error case keeps it.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: !verifyTLS}, //nolint:gosec
	}
	t.Cleanup(tr.CloseIdleConnections)
	gc.httpClient = &http.Client{Transport: tr, Timeout: clientTimeout}
	return gc
}

// closedAddr returns a loopback address that refuses connections.
func closedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

// slowTLSServer holds every request until the client gives up.
func slowTLSServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(1 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// assertRedacted checks every surface an upstream error reaches. Each check
// pairs the absence of the key with the presence of the host, so a dropped or
// empty message cannot pass by accident.
func assertRedacted(t *testing.T, err error, logs string, wantCode common.ErrorCode, hostInError bool) {
	t.Helper()
	require.Error(t, err)

	se, ok := err.(common.StandardError)
	require.True(t, ok, "the client must return a StandardError, got %T", err)
	assert.Equal(t, wantCode, se.Base().Code)

	// Guard control: the raw input really did carry the key. Without this the
	// assertions below could pass on an error that never saw the URL.
	var ue *url.Error
	if errors.As(err, &ue) {
		require.Contains(t, ue.URL, redactSentinel, "the unredacted *url.Error must still be reachable through Unwrap")
	}

	surfaces := map[string]string{
		"Error()":        err.Error(),
		"DeepestMessage": se.DeepestMessage(),
		"ErrorSummary":   common.ErrorSummary(err),
	}
	body, merr := common.SonicCfg.Marshal(err)
	require.NoError(t, merr)
	surfaces["MarshalJSON"] = string(body)

	var zl bytes.Buffer
	zlg := zerolog.New(&zl)
	zlg.Error().Err(err).Object("obj", se).Msg("upstream failed")
	surfaces["zerolog"] = zl.String()
	surfaces["client logs"] = logs

	for name, s := range surfaces {
		assert.NotContains(t, s, redactSentinel, "%s leaks the endpoint key", name)
		assert.NotContains(t, s, "/v3/", "%s leaks the endpoint path", name)
	}
	// Control: the host survives. The client logs name it on every path.
	// The error names it whenever net/http built the cause; when the caller's
	// own deadline ends the request, the cause is the bare context error.
	assert.Contains(t, logs, redactHost, "the client logs must still name the host")
	if hostInError {
		assert.Contains(t, err.Error(), redactHost, "Error() must still name the host")
		assert.Contains(t, string(body), redactHost, "the JSON must still name the host")
	}
}

func TestHttpClient_RedactsEndpointKey(t *testing.T) {
	type tc struct {
		name     string
		batch    bool
		setup    func(t *testing.T) (addr string, verifyTLS bool, clientTimeout time.Duration)
		ctxDur   time.Duration
		wantCode common.ErrorCode
		noHost   bool
	}
	cases := []tc{}
	for _, batch := range []bool{false, true} {
		suffix := "/single"
		if batch {
			suffix = "/batch"
		}
		cases = append(cases,
			// net/http's own Client.Timeout: a *url.Error that wraps
			// context.DeadlineExceeded while the request context is still
			// live. This is the production timeout path that leaked the key.
			tc{name: "client timeout" + suffix, batch: batch, ctxDur: 10 * time.Second,
				wantCode: common.ErrCodeEndpointRequestTimeout,
				setup: func(t *testing.T) (string, bool, time.Duration) {
					return slowTLSServer(t).Listener.Addr().String(), false, 200 * time.Millisecond
				}},
			// The caller's deadline: the request context ends first.
			tc{name: "context deadline" + suffix, batch: batch, ctxDur: 200 * time.Millisecond,
				wantCode: common.ErrCodeEndpointRequestTimeout, noHost: true,
				setup: func(t *testing.T) (string, bool, time.Duration) {
					return slowTLSServer(t).Listener.Addr().String(), false, 0
				}},
			tc{name: "connection refused" + suffix, batch: batch, ctxDur: 10 * time.Second,
				wantCode: common.ErrCodeEndpointTransportFailure,
				setup: func(t *testing.T) (string, bool, time.Duration) {
					return closedAddr(t), false, 0
				}},
			tc{name: "tls error" + suffix, batch: batch, ctxDur: 10 * time.Second,
				wantCode: common.ErrCodeEndpointTransportFailure,
				setup: func(t *testing.T) (string, bool, time.Duration) {
					srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
					t.Cleanup(srv.Close)
					return srv.Listener.Addr().String(), true, 0
				}},
		)
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			addr, verifyTLS, clientTimeout := c.setup(t)
			logs := &lockedBuffer{}
			client := redactClient(t, addr, verifyTLS, clientTimeout, c.batch, logs)

			ctx, cancel := context.WithTimeout(context.Background(), c.ctxDur)
			defer cancel()
			_, err := client.SendRequest(ctx, newBlockNumberRequest())
			assertRedacted(t, err, logs.String(), c.wantCode, !c.noHost)
		})
	}
}

// TestHttpClient_RedactsEndpointKeyInBuildError covers the request-build path:
// a URL net/http cannot use comes back as a parse error that quotes it whole.
func TestHttpClient_RedactsEndpointKeyInBuildError(t *testing.T) {
	logs := &lockedBuffer{}
	client := redactClient(t, closedAddr(t), false, 0, false, logs)
	// A control character makes http.NewRequest reject the URL.
	bad := &url.URL{Scheme: "https", Host: redactHost, Path: "/v3/" + redactSentinel + "\x7f"}
	client.Url = bad

	_, err := client.SendRequest(context.Background(), newBlockNumberRequest())
	require.Error(t, err)
	body, merr := common.SonicCfg.Marshal(err)
	require.NoError(t, merr)
	for name, s := range map[string]string{"Error()": err.Error(), "MarshalJSON": string(body), "logs": logs.String()} {
		assert.NotContains(t, s, redactSentinel, "%s leaks the endpoint key", name)
	}
	assert.True(t, strings.Contains(err.Error(), redactHost), "Error() must still name the host: %s", err.Error())
}

// TestHttpClient_RedactsCredentialHeadersInDebugLog covers the request debug
// log, which prints the outgoing headers. Alchemy's key travels as
// "Authorization: Bearer <key>"; other vendors use x-api-key and similar.
func TestHttpClient_RedactsCredentialHeadersInDebugLog(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":"0x1"}`)
	}))
	t.Cleanup(srv.Close)

	headers := map[string]string{
		"Authorization":       "Bearer " + redactSentinel,
		"Proxy-Authorization": "Basic " + redactSentinel,
		"Cookie":              "session=" + redactSentinel,
		"X-Api-Key":           redactSentinel,
		"X-Auth-Token":        redactSentinel,
		"X-Client-Secret":     redactSentinel,
		"X-Visible":           "VISIBLE_CONTROL",
	}
	for _, batch := range []bool{false, true} {
		name := "single"
		if batch {
			name = "batch"
		}
		t.Run(name, func(t *testing.T) {
			logs := &lockedBuffer{}
			client := redactClient(t, srv.Listener.Addr().String(), false, 0, batch, logs)
			client.headers = headers

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := client.SendRequest(ctx, newBlockNumberRequest())
			require.NoError(t, err)

			out := logs.String()
			assert.NotContains(t, out, redactSentinel, "the debug log leaks a credential header")
			// Controls: the log line printed the headers, and kept the names.
			require.Contains(t, out, "sending json rpc POST request")
			for k := range headers {
				assert.Contains(t, out, k, "the header name %s must still appear", k)
			}
			assert.Contains(t, out, "VISIBLE_CONTROL", "a harmless header value must still appear")
		})
	}
}

// TestHttpClient_SpanErrorRedactsEndpoint checks the error a trace span
// records for a failed request.
func TestHttpClient_SpanErrorRedactsEndpoint(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	common.SetTracerProviderForTest(tp)
	t.Cleanup(func() { common.IsTracingEnabled = false })

	logs := &lockedBuffer{}
	client := redactClient(t, closedAddr(t), false, 0, false, logs)
	_, err := client.SendRequest(context.Background(), newBlockNumberRequest())
	require.Error(t, err)
	require.NoError(t, tp.ForceFlush(context.Background()))

	var recorded []string
	for _, s := range exp.GetSpans() {
		recorded = append(recorded, s.Status.Description)
		for _, ev := range s.Events {
			for _, a := range ev.Attributes {
				recorded = append(recorded, a.Value.String())
			}
		}
		for _, a := range s.Attributes {
			recorded = append(recorded, a.Value.String())
		}
	}
	all := strings.Join(recorded, "\n")
	assert.NotContains(t, all, redactSentinel, "a span leaks the endpoint key")
	// Control: a span recorded this failure, and it names the host.
	assert.Contains(t, all, "https://"+redactHost+"/"+util.RedactedURLPlaceholder)
}

// TestWsClient_DialFailureRedactsEndpoint covers the WebSocket dial. The
// client logs the dial error on its first attempt.
func TestWsClient_DialFailureRedactsEndpoint(t *testing.T) {
	cases := map[string]*url.URL{
		// A refused dial: the error itself names only the address.
		"connection refused": {Scheme: "ws", Host: closedAddr(t), Path: "/v3/" + redactSentinel},
		// A URL the dialer re-parses and rejects: url.Parse quotes it whole.
		"unparseable url": {Scheme: "wss", Opaque: "//" + redactHost + "/v3/" + redactSentinel + "\x7f"},
	}
	for name, u := range cases {
		t.Run(name, func(t *testing.T) {
			logs := &lockedBuffer{}
			prevLevel := zerolog.GlobalLevel()
			zerolog.SetGlobalLevel(zerolog.TraceLevel)
			t.Cleanup(func() { zerolog.SetGlobalLevel(prevLevel) })
			lg := zerolog.New(logs).Level(zerolog.TraceLevel)

			ups := common.NewFakeUpstream("ws-redact")
			ups.Config().Type = common.UpstreamTypeEvm
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			_, err := NewWsJsonRpcClient(ctx, &lg, "prj1", ups, u, nil, &noopErrorExtractor{})
			require.NoError(t, err)
			cancel()

			out := logs.String()
			assert.NotContains(t, out, redactSentinel, "the dial failure log leaks the endpoint key")
			// Control: the first dial failed and was logged.
			assert.Contains(t, out, "initial websocket connection failed")
		})
	}
}
