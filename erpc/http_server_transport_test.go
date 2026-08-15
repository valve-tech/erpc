package erpc

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

// A request that never becomes a readable JSON-RPC call must be refused at the
// transport edge, and it must be refused BEFORE anything is forwarded. That
// second half is the part a status-code assertion cannot see: eRPC pays a
// provider per upstream call, so a malformed body that still reaches an upstream
// is a request the operator is billed for and a slot in the upstream's rate
// budget spent on nothing. Every test below therefore counts upstream calls, not
// just the answer the client got.

// countingUpstream is a real JSON-RPC endpoint that records how many times each
// method reached it. Local (127.0.0.1) endpoints bypass gock, so this counts
// actual HTTP requests rather than mock matches.
type countingUpstream struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  map[string]int
}

func newCountingUpstream(t *testing.T) *countingUpstream {
	t.Helper()
	u := &countingUpstream{calls: map[string]int{}}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		var single map[string]interface{}
		var items []map[string]interface{}
		if len(body) > 0 && body[0] == '[' {
			_ = json.Unmarshal(body, &items)
		} else if json.Unmarshal(body, &single) == nil {
			items = []map[string]interface{}{single}
		}

		out := make([]map[string]interface{}, 0, len(items))
		for _, item := range items {
			method, _ := item["method"].(string)
			u.mu.Lock()
			u.calls[method]++
			u.mu.Unlock()
			out = append(out, map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      item["id"],
				"result":  resultForMethod(method),
			})
		}

		w.Header().Set("Content-Type", "application/json")
		if len(body) > 0 && body[0] == '[' {
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		if len(out) == 1 {
			_ = json.NewEncoder(w).Encode(out[0])
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"empty"}}`))
	}))
	t.Cleanup(u.server.Close)
	return u
}

// resultForMethod answers the state-poller probes an upstream must satisfy
// before eRPC will route to it, plus the one method the tests actually call.
func resultForMethod(method string) interface{} {
	switch method {
	case "eth_chainId":
		return "0x7b"
	case "eth_getBlockByNumber":
		return map[string]interface{}{
			"number": "0x11118888", "hash": "0xb10c", "timestamp": "0x6702a8f0",
		}
	case "eth_syncing":
		return false
	case "eth_getBalance":
		return "0xabc123"
	}
	return "0x1"
}

func (u *countingUpstream) count(method string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls[method]
}

// transportTestConfig points a single project at the counting upstream.
func transportTestConfig(u *countingUpstream) *common.Config {
	return &common.Config{
		Server: &common.ServerConfig{ListenV4: util.BoolPtr(true)},
		Projects: []*common.ProjectConfig{
			{
				Id: "test_http",
				Networks: []*common.NetworkConfig{
					{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}},
				},
				Upstreams: []*common.UpstreamConfig{
					{
						Id:       "counting",
						Type:     common.UpstreamTypeEvm,
						Endpoint: u.server.URL,
						Evm:      &common.EvmUpstreamConfig{ChainId: 123},
					},
				},
			},
		},
		RateLimiters: &common.RateLimiterConfig{},
	}
}

// startTransportServer boots eRPC in front of the counting upstream and returns
// the base URL of the network endpoint.
func startTransportServer(t *testing.T, cfg *common.Config) string {
	t.Helper()
	addr, cleanup := setupTestERPCServer(t, cfg)
	t.Cleanup(cleanup)
	return fmt.Sprintf("http://%s/test_http/evm/123", addr)
}

// postRaw sends one request with a clean transport, so nothing is shared with
// another test's connection pool.
func postRaw(t *testing.T, url string, body []byte, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Transport: &http.Transport{}, Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(respBody)
}

const balanceCall = `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]}`

// TestHttpServer_ServesAGzippedRequestBody is the positive control for the
// request-decompression path. Clients on metered links compress their request
// bodies; if eRPC stopped decompressing them, every such client would see its
// calls rejected as malformed JSON while the header said otherwise.
func TestHttpServer_ServesAGzippedRequestBody(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	up := newCountingUpstream(t)
	url := startTransportServer(t, transportTestConfig(up))

	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	_, err := gz.Write([]byte(balanceCall))
	require.NoError(t, err)
	require.NoError(t, gz.Close())

	resp, body := postRaw(t, url, compressed.Bytes(), map[string]string{
		"Content-Encoding": "gzip",
	})

	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)
	require.Contains(t, body, "0xabc123")
	require.Equal(t, 1, up.count("eth_getBalance"),
		"a decompressed request must reach the upstream exactly once")
}

// TestHttpServer_RefusesABodyThatIsNotTheGzipItClaimsToBe covers the header that
// lies. The decompressor must fail closed at the edge: the bytes behind it are
// not JSON-RPC, so forwarding anything derived from them would send an upstream
// a request the client never made.
func TestHttpServer_RefusesABodyThatIsNotTheGzipItClaimsToBe(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	up := newCountingUpstream(t)
	url := startTransportServer(t, transportTestConfig(up))

	resp, body := postRaw(t, url, []byte(balanceCall), map[string]string{
		"Content-Encoding": "gzip",
	})

	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "body was %s", body)
	require.Contains(t, body, "invalid gzip body")
	require.Zero(t, up.count("eth_getBalance"),
		"an undecodable body must not be forwarded to a paid upstream")
}

// TestHttpServer_RefusesAGzipStreamThatStopsHalfway covers truncation rather
// than a bad header: the magic bytes and the gzip header are valid, so the
// reader opens successfully and only fails once the body is consumed. That is a
// different branch from the one above — the failure surfaces during the body
// read (util.ReadAll), not at reader construction.
//
// Note the status this pins. The bad-header case above is wrapped in
// NewErrInvalidRequest and answers 400; this one reaches handleErrorResponse
// unwrapped, matches none of its error-code cases, and falls through to the
// default 200 with a -32603 "internal" body. Two malformed uploads, two
// different transport verdicts, and the truncated one is the milder-looking of
// the pair. The assertion below records today's behaviour rather than the
// desired one, so the difference is visible instead of undocumented.
func TestHttpServer_RefusesAGzipStreamThatStopsHalfway(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	up := newCountingUpstream(t)
	url := startTransportServer(t, transportTestConfig(up))

	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	_, err := gz.Write([]byte(balanceCall))
	require.NoError(t, err)
	require.NoError(t, gz.Close())

	truncated := compressed.Bytes()[:compressed.Len()-6]
	resp, body := postRaw(t, url, truncated, map[string]string{
		"Content-Encoding": "gzip",
	})

	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(body), &parsed), "got %s", body)
	require.NotNil(t, parsed["error"], "a truncated upload is not a served request")
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"pinning today's verdict: the read error is not classified, so it answers 200")
	require.Zero(t, up.count("eth_getBalance"),
		"whatever the status, a body that never finished decompressing must not be forwarded")
}

// TestHttpServer_RefusesABatchEnvelopeItCannotParse covers the array path. A
// batch is parsed as a whole before any entry is routed, so a broken envelope
// must produce one transport error — not a partial batch where some entries were
// forwarded and the rest silently vanished.
func TestHttpServer_RefusesABatchEnvelopeItCannotParse(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	up := newCountingUpstream(t)
	url := startTransportServer(t, transportTestConfig(up))

	// Valid opening entry, then the array is cut short.
	resp, body := postRaw(t, url,
		[]byte(`[{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]},`), nil)

	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "body was %s", body)
	require.Zero(t, up.count("eth_getBalance"),
		"no entry of an unparseable batch may be forwarded")
}

// TestHttpServer_AnswersAnEmptyBodyWithoutForwardingIt covers the degenerate
// POST some health probes and misconfigured clients send. It must come back as a
// JSON-RPC error the client can read, and it must cost nothing upstream.
func TestHttpServer_AnswersAnEmptyBodyWithoutForwardingIt(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	up := newCountingUpstream(t)
	url := startTransportServer(t, transportTestConfig(up))

	resp, body := postRaw(t, url, nil, nil)

	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(body), &parsed),
		"the answer must still be readable JSON; got %s", body)
	require.NotNil(t, parsed["error"], "an empty body is not a request")
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Zero(t, up.count("eth_getBalance"))
}

// TestHttpServer_ABlockedMethodEchoesTheCallersIdOverHttp covers the refusal
// `ignoreMethods` produces. A refusal is only useful if the caller can pair it
// with the call it made, and every JSON-RPC client pairs by id. A null id is
// unpairable: the caller sits on the request until its own timeout instead of
// reading the refusal that is right there in the body.
//
// The HTTP path used to test `err != nil` before copying the id from the parsed
// request (http_server.go:580), which is inverted — and on failure
// JsonRpcRequest returns a nil pointer, so that body would have panicked had it
// ever run. It never ran, because Validate() caches the parse first, so the id
// silently stayed nil. The WebSocket path always tested `err == nil`, so the
// same project config behaved differently over the two transports.
func TestHttpServer_ABlockedMethodEchoesTheCallersIdOverHttp(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	up := newCountingUpstream(t)
	cfg := transportTestConfig(up)
	cfg.Projects[0].IgnoreMethods = []string{"debug_*"}
	url := startTransportServer(t, cfg)

	resp, body := postRaw(t, url,
		[]byte(`{"jsonrpc":"2.0","id":77,"method":"debug_traceTransaction","params":["0xabc"]}`), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)

	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(body), &parsed), "got %s", body)

	errObj, ok := parsed["error"].(map[string]interface{})
	require.True(t, ok, "a blocked method must be refused; got %s", body)
	require.Contains(t, errObj["message"], "method not supported")
	require.Zero(t, up.count("debug_traceTransaction"),
		"a blocked method must never reach the node")

	require.EqualValues(t, 77, parsed["id"],
		"the caller sent id 77 and must get id 77 back, or it cannot pair the refusal")
	require.Equal(t, "2.0", parsed["jsonrpc"])
}

// TestHttpServer_ABlockedMethodInABatchEchoesEachId is the discriminating half.
// A single call still arrives on the connection the client is waiting on, so a
// null id there is survivable. In a batch, the id is the ONLY link between an
// entry and its answer: one blocked entry with a null id makes the whole batch
// unmatchable. The batch below mixes a served call with two blocked ones, so
// the test fails if the server echoes one id for all of them.
func TestHttpServer_ABlockedMethodInABatchEchoesEachId(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	up := newCountingUpstream(t)
	cfg := transportTestConfig(up)
	cfg.Projects[0].IgnoreMethods = []string{"debug_*"}
	url := startTransportServer(t, cfg)

	resp, body := postRaw(t, url, []byte(`[
		{"jsonrpc":"2.0","id":11,"method":"debug_traceTransaction","params":["0xabc"]},
		{"jsonrpc":"2.0","id":12,"method":"eth_getBalance","params":["0xaaaa","latest"]},
		{"jsonrpc":"2.0","id":13,"method":"debug_traceBlock","params":["0xdef"]}
	]`), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)

	var entries []map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(body), &entries), "got %s", body)
	require.Len(t, entries, 3, "a batch answers every entry; got %s", body)

	byId := map[float64]map[string]interface{}{}
	for _, e := range entries {
		id, ok := e["id"].(float64)
		require.True(t, ok, "every entry must carry back its own id; got %s", body)
		byId[id] = e
	}

	for _, id := range []float64{11, 13} {
		entry, ok := byId[id]
		require.True(t, ok, "the refusal for id %v is unpairable; got %s", id, body)
		errObj, isErr := entry["error"].(map[string]interface{})
		require.True(t, isErr, "id %v must be refused; got %s", id, body)
		require.Contains(t, errObj["message"], "method not supported")
	}

	served, ok := byId[12]
	require.True(t, ok, "the allowed entry must keep its id too; got %s", body)
	require.Equal(t, "0xabc123", served["result"],
		"blocking two siblings must not disturb the entry that is allowed")
	require.Equal(t, 1, up.count("eth_getBalance"))
	require.Zero(t, up.count("debug_traceTransaction"))
	require.Zero(t, up.count("debug_traceBlock"))
}

// TestGzipHandler_StepsAsideForAnUpgradeBeforeTouchingAnyHeader pins the order
// of the two things gzipHandler does first. The WebSocket bypass must come
// before the Vary line, so an upgrade request reaches the next handler with the
// ORIGINAL writer and a response-header map this handler has not written to.
//
// Order matters because gorilla's Upgrade() type-asserts http.Hijacker on the
// writer it is handed, with no unwrap fallback: any wrapping turns the upgrade
// into a 500. Setting Vary first is harmless on its own, which is exactly the
// risk — a reorder that puts header work ahead of the bypass reads as a no-op
// and is one edit away from wrapping the writer too.
func TestGzipHandler_StepsAsideForAnUpgradeBeforeTouchingAnyHeader(t *testing.T) {
	type observation struct {
		wrapped bool
		vary    string
	}
	var seen observation

	handler := gzipHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, seen.wrapped = w.(*conditionalGzipWriter)
		seen.vary = w.Header().Get("Vary")
		w.WriteHeader(http.StatusOK)
	}))

	upgrade := httptest.NewRequest(http.MethodGet, "/test_http/evm/123", nil)
	upgrade.Header.Set("Connection", "Upgrade")
	upgrade.Header.Set("Upgrade", "websocket")
	upgrade.Header.Set("Accept-Encoding", "gzip")
	handler.ServeHTTP(httptest.NewRecorder(), upgrade)

	require.False(t, seen.wrapped,
		"an upgrade must reach the next handler on the original writer, or gorilla cannot hijack it")
	require.Empty(t, seen.vary,
		"gzipHandler must bypass before it writes Vary; a header written here means the bypass moved")

	// The same request without the upgrade tokens takes the normal path, which
	// proves the assertions above describe the bypass and not the handler's
	// behaviour in general.
	plain := httptest.NewRequest(http.MethodPost, "/test_http/evm/123", strings.NewReader("{}"))
	plain.Header.Set("Accept-Encoding", "gzip")
	handler.ServeHTTP(httptest.NewRecorder(), plain)

	require.True(t, seen.wrapped, "a compressible response is wrapped")
	require.Equal(t, "Accept-Encoding", seen.vary,
		"a cacheable response must declare that it varies on the encoding")
}
