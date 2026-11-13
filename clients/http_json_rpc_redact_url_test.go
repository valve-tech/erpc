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
