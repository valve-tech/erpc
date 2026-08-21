package erpc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// A WebSocket client keeps one long-lived connection and multiplexes everything
// over it. That changes what a bug costs: an HTTP request that is mishandled
// affects one call, but a WebSocket connection that is closed, starved or fed a
// misaligned batch takes every in-flight call and every live subscription on
// that connection with it. The tests here cover the edges of that connection —
// who may open one, what a malformed frame does to it, and what a batch is
// allowed to contain.

// wsTestConfig points a project at the counting upstream and exposes the WS
// endpoint, so a WebSocket test can count upstream calls the same way the HTTP
// tests do.
func wsTestConfig(u *countingUpstream) *common.Config {
	return transportTestConfig(u)
}

// dialWsProject opens a client WebSocket against the test_http project, with
// optional extra headers, and returns the handshake response so a refused
// upgrade can be inspected.
func dialWsProject(addr string, header http.Header) (*websocket.Conn, *http.Response, error) {
	return websocket.DefaultDialer.Dial(
		fmt.Sprintf("ws://%s/test_http/evm/123", addr), header)
}

// wsRoundTrip sends one frame and reads one answer, bounded so a hung server
// fails the test instead of hanging it.
func wsRoundTrip(t *testing.T, conn *websocket.Conn, payload string) []byte {
	t.Helper()
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(payload)))
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	_, msg, err := conn.ReadMessage()
	require.NoError(t, err)
	return msg
}

// TestWebSocket_RefusesAnUpgradeFromAnOriginTheCorsPolicyDoesNotAllow covers
// the one place CORS is a real access control rather than a browser hint. An
// HTTP call from a disallowed origin is still served (the browser blocks the
// answer); a WebSocket upgrade from one is refused outright, because a granted
// upgrade is a persistent, browser-readable channel that no later header can
// take back.
func TestWebSocket_RefusesAnUpgradeFromAnOriginTheCorsPolicyDoesNotAllow(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	up := newCountingUpstream(t)
	cfg := wsTestConfig(up)
	cfg.Projects[0].CORS = &common.CORSConfig{
		AllowedOrigins: []string{"https://app.example.com"},
		AllowedMethods: []string{"POST", "OPTIONS"},
	}
	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()

	header := http.Header{}
	header.Set("Origin", "https://elsewhere.example.com")
	conn, resp, err := dialWsProject(addr, header)
	if conn != nil {
		defer conn.Close()
	}
	if resp != nil {
		defer resp.Body.Close()
	}

	require.Error(t, err, "a disallowed origin must not get a live socket")
	require.NotNil(t, resp)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// TestWebSocket_AcceptsAnUpgradeFromAnAllowedOriginAndFromNoOriginAtAll is the
// pair of positive controls. Without them the refusal above is satisfied by a
// server that refuses every upgrade — and the no-Origin case is how every
// non-browser client connects, so it must stay open when CORS is configured.
func TestWebSocket_AcceptsAnUpgradeFromAnAllowedOriginAndFromNoOriginAtAll(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	up := newCountingUpstream(t)
	cfg := wsTestConfig(up)
	cfg.Projects[0].CORS = &common.CORSConfig{
		AllowedOrigins: []string{"https://*.example.com"},
		AllowedMethods: []string{"POST", "OPTIONS"},
	}
	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()

	allowed := http.Header{}
	allowed.Set("Origin", "https://app.example.com")
	conn, resp, err := dialWsProject(addr, allowed)
	require.NoError(t, err, "a wildcard-matched origin must be allowed to upgrade")
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	conn.Close()

	bare, resp2, err := dialWsProject(addr, nil)
	require.NoError(t, err, "a client that sends no Origin must still be able to connect")
	require.Equal(t, http.StatusSwitchingProtocols, resp2.StatusCode)
	bare.Close()
}

// TestWebSocket_AnswersAMalformedBatchEnvelopeWithOneParseError covers the
// array path on the WebSocket. There is no HTTP status code here to carry the
// verdict, so the parse error has to arrive as a JSON-RPC body — and it must be
// a single object, not an array, because the client has no entries to align it
// against.
func TestWebSocket_AnswersAMalformedBatchEnvelopeWithOneParseError(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	up := newCountingUpstream(t)
	addr, cleanup := setupTestERPCServer(t, wsTestConfig(up))
	defer cleanup()

	conn, _, err := dialWsProject(addr, nil)
	require.NoError(t, err)
	defer conn.Close()

	msg := wsRoundTrip(t, conn, `[{"jsonrpc":"2.0","id":1,"method":"eth_getBalance"},`)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(msg, &resp), "got %s", msg)
	errObj, ok := resp["error"].(map[string]interface{})
	require.True(t, ok, "a broken batch must answer with an error object; got %s", msg)
	require.Equal(t, float64(-32700), errObj["code"], "parse error is -32700 to a JSON-RPC client")
	require.Zero(t, up.count("eth_getBalance"),
		"no entry of an unparseable batch may be forwarded")

	// The connection must survive it: a client that pipelined other calls
	// behind this one would otherwise lose all of them.
	after := wsRoundTrip(t, conn, balanceCall)
	require.Contains(t, string(after), "0xabc123")
}

// TestWebSocket_RejectsSubscriptionMethodsInsideABatchWithoutDroppingTheEntry
// covers the one method class a batch cannot carry. eth_subscribe needs the
// connection as its delivery channel, and a batch entry has no way to name one,
// so it is refused — but the refusal must occupy its own slot in the array, or
// every answer after it pairs with the wrong call.
func TestWebSocket_RejectsSubscriptionMethodsInsideABatchWithoutDroppingTheEntry(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	up := newCountingUpstream(t)
	addr, cleanup := setupTestERPCServer(t, wsTestConfig(up))
	defer cleanup()

	conn, _, err := dialWsProject(addr, nil)
	require.NoError(t, err)
	defer conn.Close()

	msg := wsRoundTrip(t, conn, `[`+
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]},`+
		`{"jsonrpc":"2.0","id":2,"method":"eth_subscribe","params":["newHeads"]},`+
		`{"jsonrpc":"2.0","id":3,"method":"eth_getBalance","params":["0xbbbb","latest"]}]`)

	var entries []map[string]interface{}
	require.NoError(t, json.Unmarshal(msg, &entries), "got %s", msg)
	require.Len(t, entries, 3, "the rejected entry must still take its slot")

	require.Equal(t, float64(1), entries[0]["id"])
	require.Equal(t, "0xabc123", entries[0]["result"])

	require.Equal(t, float64(2), entries[1]["id"])
	errObj, ok := entries[1]["error"].(map[string]interface{})
	require.True(t, ok, "the subscribe entry must carry an error; got %s", msg)
	require.Contains(t, errObj["message"], "not supported in batch requests")

	require.Equal(t, float64(3), entries[2]["id"],
		"the entry after the refusal must keep its own id")
	require.Equal(t, "0xabc123", entries[2]["result"])
}

// TestWebSocket_AppliesTheMethodFilterToBatchEntriesToo covers the filter on
// the batch path, which is a separate code path from the single-request one. An
// operator who blocks debug_* to keep node cost down would otherwise find the
// block trivially bypassable by wrapping the call in an array.
func TestWebSocket_AppliesTheMethodFilterToBatchEntriesToo(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	up := newCountingUpstream(t)
	cfg := wsTestConfig(up)
	cfg.Projects[0].IgnoreMethods = []string{"debug_*"}
	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()

	conn, _, err := dialWsProject(addr, nil)
	require.NoError(t, err)
	defer conn.Close()

	msg := wsRoundTrip(t, conn, `[`+
		`{"jsonrpc":"2.0","id":1,"method":"debug_traceTransaction","params":["0xabc"]},`+
		`{"jsonrpc":"2.0","id":2,"method":"eth_getBalance","params":["0xaaaa","latest"]}]`)

	var entries []map[string]interface{}
	require.NoError(t, json.Unmarshal(msg, &entries), "got %s", msg)
	require.Len(t, entries, 2)

	errObj, ok := entries[0]["error"].(map[string]interface{})
	require.True(t, ok, "a blocked method must be refused inside a batch; got %s", msg)
	require.Contains(t, errObj["message"], "method not supported")
	require.Zero(t, up.count("debug_traceTransaction"),
		"a blocked method must never reach the node, batched or not")

	require.Equal(t, "0xabc123", entries[1]["result"],
		"one blocked entry must not take the rest of the batch with it")
}

// TestWebSocket_AnAllowRuleReadmitsAMethodTheIgnoreRuleBlocked covers the
// second half of the filter. allowMethods is evaluated after ignoreMethods and
// can re-admit; an operator uses that to block a family and carve out one call.
func TestWebSocket_AnAllowRuleReadmitsAMethodTheIgnoreRuleBlocked(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	up := newCountingUpstream(t)
	cfg := wsTestConfig(up)
	cfg.Projects[0].IgnoreMethods = []string{"debug_*"}
	cfg.Projects[0].AllowMethods = []string{"debug_traceBlockByNumber"}
	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()

	conn, _, err := dialWsProject(addr, nil)
	require.NoError(t, err)
	defer conn.Close()

	carvedOut := wsRoundTrip(t, conn,
		`{"jsonrpc":"2.0","id":1,"method":"debug_traceBlockByNumber","params":["0x1"]}`)
	require.NotContains(t, string(carvedOut), "method not supported",
		"an explicitly allowed method must survive the ignore rule; got %s", carvedOut)

	stillBlocked := wsRoundTrip(t, conn,
		`{"jsonrpc":"2.0","id":2,"method":"debug_traceTransaction","params":["0xabc"]}`)
	require.Contains(t, string(stillBlocked), "method not supported",
		"the rest of the blocked family must stay blocked")
}

// TestWebSocket_ForwardsTheConfiguredHeadersFromTheUpgradeRequest covers the
// header plumbing that only WebSocket has to work for. An HTTP call carries its
// headers per request; a WebSocket carries them once, on the upgrade, and every
// later frame has to inherit them. If that inheritance breaks, an operator whose
// upstream authenticates on a forwarded header sees every WebSocket client fail
// while the same credentials work over HTTP.
func TestWebSocket_ForwardsTheConfiguredHeadersFromTheUpgradeRequest(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	var sawHeader atomic.Value
	sawHeader.Store("")

	up := newCountingUpstream(t)
	// Wrap the upstream handler so the forwarded header can be observed on the
	// request eRPC actually made.
	base := up.server.Config.Handler
	up.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get("X-Tenant-Id"); v != "" {
			sawHeader.Store(v)
		}
		base.ServeHTTP(w, r)
	})

	cfg := wsTestConfig(up)
	cfg.Projects[0].ForwardHeaders = []string{"X-Tenant-Id"}
	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()

	header := http.Header{}
	header.Set("X-Tenant-Id", "tenant-42")
	conn, _, err := dialWsProject(addr, header)
	require.NoError(t, err)
	defer conn.Close()

	msg := wsRoundTrip(t, conn, balanceCall)
	require.Contains(t, string(msg), "0xabc123")

	require.Equal(t, "tenant-42", sawHeader.Load(),
		"the header from the upgrade must ride along on every frame's upstream call")
}

// TestWebSocket_AnswersTheClientsCloseFrameWithOne covers the close handshake.
// A client that closes cleanly waits for the server's close frame before
// releasing the socket; without the echo it waits out its own timeout instead,
// which at scale leaves a pile of half-closed connections on both sides.
func TestWebSocket_AnswersTheClientsCloseFrameWithOne(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	up := newCountingUpstream(t)
	addr, cleanup := setupTestERPCServer(t, wsTestConfig(up))
	defer cleanup()

	conn, _, err := dialWsProject(addr, nil)
	require.NoError(t, err)
	defer conn.Close()

	// Prove the connection is live before closing it.
	require.Contains(t, string(wsRoundTrip(t, conn, balanceCall)), "0xabc123")

	require.NoError(t, conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseGoingAway, "client leaving"),
		time.Now().Add(5*time.Second),
	))

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	_, _, err = conn.ReadMessage()
	require.Error(t, err, "the read must end once the peer closes")

	closeErr, ok := err.(*websocket.CloseError)
	require.True(t, ok, "the server must answer with a close frame, not a bare hangup: %v", err)
	require.Equal(t, websocket.CloseGoingAway, closeErr.Code,
		"the echoed close must mirror the code the client sent")
}

// TestWebSocket_KeepsTheConnectionAliveWithPings covers the keepalive loop. It
// is the only thing holding a WebSocket open through an idle-timeout proxy, and
// nothing else in the system notices when it stops — the connection simply dies
// after a few minutes of quiet and every subscription on it goes silent.
func TestWebSocket_KeepsTheConnectionAliveWithPings(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	up := newCountingUpstream(t)
	cfg := wsTestConfig(up)
	cfg.Server.WebSocket = &common.WebSocketServerConfig{
		PingInterval: durationPtr(150 * time.Millisecond),
	}
	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()

	conn, _, err := dialWsProject(addr, nil)
	require.NoError(t, err)
	defer conn.Close()

	var pings int32
	conn.SetPingHandler(func(appData string) error {
		atomic.AddInt32(&pings, 1)
		return conn.WriteControl(websocket.PongMessage, []byte(appData),
			time.Now().Add(5*time.Second))
	})

	// Control frames are only delivered while a read is in flight, so drive one
	// read and let it time out after several ping intervals have passed.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(1200*time.Millisecond)))
	_, _, err = conn.ReadMessage()
	require.Error(t, err, "no data frame is expected on an idle connection")

	require.GreaterOrEqual(t, atomic.LoadInt32(&pings), int32(2),
		"an idle connection must be pinged on the configured interval")
}

// TestWebSocket_ClosesAConnectionThatSendsAnOversizedFrame covers the read
// limit. It is the only bound on how much memory one client can make the server
// hold for a single message, so it must actually terminate the connection —
// and it must do so with the 1009 code, which is how a client learns to split
// its batch rather than retry the same oversized frame forever.
func TestWebSocket_ClosesAConnectionThatSendsAnOversizedFrame(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	up := newCountingUpstream(t)
	cfg := wsTestConfig(up)
	cfg.Server.WebSocket = &common.WebSocketServerConfig{MaxMessageSize: 512}
	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()

	conn, _, err := dialWsProject(addr, nil)
	require.NoError(t, err)
	defer conn.Close()

	// Well under the limit: served normally.
	require.Contains(t, string(wsRoundTrip(t, conn, balanceCall)), "0xabc123")

	oversized := make([]byte, 4096)
	for i := range oversized {
		oversized[i] = 'a'
	}
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, oversized))

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	_, _, err = conn.ReadMessage()
	require.Error(t, err, "an oversized frame must end the connection")

	closeErr, ok := err.(*websocket.CloseError)
	require.True(t, ok, "the client must be told why, not just disconnected: %v", err)
	require.Equal(t, websocket.CloseMessageTooBig, closeErr.Code)
	require.Zero(t, up.count("eth_sendRawTransaction"),
		"nothing from an over-limit frame may be routed")
}

// TestWebSocket_AnswersAnUnreadableEntryInsideABatchInPlace covers the
// per-entry validation. One entry that is not a JSON-RPC request must produce
// one error entry, not abort the batch — the sibling calls are valid and the
// client is waiting on all of them.
func TestWebSocket_AnswersAnUnreadableEntryInsideABatchInPlace(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	up := newCountingUpstream(t)
	addr, cleanup := setupTestERPCServer(t, wsTestConfig(up))
	defer cleanup()

	conn, _, err := dialWsProject(addr, nil)
	require.NoError(t, err)
	defer conn.Close()

	msg := wsRoundTrip(t, conn, `[`+
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]},`+
		`"this entry is a bare string",`+
		`{"jsonrpc":"2.0","id":9},`+
		`{"jsonrpc":"2.0","id":3,"method":"eth_getBalance","params":["0xbbbb","latest"]}]`)

	var entries []map[string]interface{}
	require.NoError(t, json.Unmarshal(msg, &entries), "got %s", msg)
	require.Len(t, entries, 4, "an invalid entry must still take its slot")

	// Entry 3 parses as JSON but names no method. Nothing can route it, so it
	// must come back refused and it must not turn into an upstream call for a
	// request eRPC cannot even describe.
	require.NotNil(t, entries[2]["error"], "a method-less entry must be refused; got %s", msg)
	require.Zero(t, up.count(""),
		"a request with no method must never be sent to an upstream")

	require.Equal(t, "0xabc123", entries[0]["result"])

	errObj, ok := entries[1]["error"].(map[string]interface{})
	require.True(t, ok, "the invalid entry must carry an error; got %s", msg)
	// The code has to say "your payload did not parse", not "the server
	// failed". A client reading a server-fault code retries a request that can
	// never succeed; a client reading -32700 fixes its payload instead.
	//
	// Measured note for whoever refactors handleBatchItem: the early
	// nq.Validate() there is defence in depth, not the source of this code.
	// Removing it leaves this assertion green, because the path below classifies
	// the same failure identically and still contacts no upstream. What this
	// test does detect is the entry losing its slot — see the length and id
	// checks around it.
	require.Equal(t, float64(common.JsonRpcErrorParseException), errObj["code"],
		"a malformed entry must be reported as a parse error; got %s", msg)

	require.Equal(t, float64(3), entries[3]["id"],
		"the entry after the invalid ones must keep its own id")
	require.Equal(t, "0xabc123", entries[3]["result"])
}
