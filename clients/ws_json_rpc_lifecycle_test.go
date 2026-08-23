package clients

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/gorilla/websocket"
)

// normalizeIDKey turns a JSON-RPC id into the map key that matches a response
// to its waiting caller. JSON decoding makes every integer a float64, and
// fmt's default float formatting renders large ones in scientific notation —
// "1.51e+09" would never match the "1510000000" the request was filed under,
// so the caller waits out its whole timeout while the answer sits unclaimed.
func TestNormalizeIDKey_LargeIntegerIdsNeverBecomeScientificNotation(t *testing.T) {
	cases := []struct {
		name string
		id   interface{}
		want string
	}{
		{"float64 from JSON decoding", float64(1510000000), "1510000000"},
		{"small float64", float64(1), "1"},
		{"native int", int(42), "42"},
		{"native int64", int64(9007199254740992), "9007199254740992"},
		{"string id", "abc-123", "abc-123"},
		{"unexpected type falls back to its printed form", true, "true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeIDKey(tc.id); got != tc.want {
				t.Fatalf("normalizeIDKey(%v) = %q, want %q", tc.id, got, tc.want)
			}
		})
	}
}

// A caller's own JSON-RPC id must come back unchanged. The client rewrites the
// id on the wire so two concurrent callers using id 1 cannot collide; if the
// rewrite leaked out, every client library would reject the response as an
// answer to a request it never sent.
func TestWsSendRequest_RestoresTheCallersOwnJsonRpcId(t *testing.T) {
	srv := newFakeWsServer(t)
	c := newTestWsClient(t, srv.wsURL(t))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`))
	resp, err := c.SendRequest(ctx, req)
	if err != nil {
		t.Fatalf("SendRequest: %v", err)
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		t.Fatalf("JsonRpcResponse: %v", err)
	}
	got := jrr.ID()
	if got != int64(1) {
		t.Fatalf("response id = %v (%T), want the caller's own 1", got, got)
	}
}

// idRecordingWsServer echoes every request and reports the id it saw ON THE
// WIRE, which is the only place the rewrite is observable.
func idRecordingWsServer(t *testing.T) (*url.URL, <-chan float64) {
	t.Helper()
	seen := make(chan float64, 8)
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		go func() {
			for {
				_, msg, err := conn.ReadMessage()
				if err != nil {
					return
				}
				var req struct {
					ID float64 `json:"id"`
				}
				if err := common.SonicCfg.Unmarshal(msg, &req); err != nil {
					continue
				}
				seen <- req.ID
				resp, _ := common.SonicCfg.Marshal(map[string]interface{}{
					"jsonrpc": "2.0", "id": req.ID, "result": "0xsub1",
				})
				_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
				_ = conn.WriteMessage(websocket.TextMessage, resp)
			}
		}()
	}))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	u.Scheme = "ws"
	return u, seen
}

// The id ON THE WIRE must NOT be the caller's. Without the rewrite, two
// concurrent callers that both use id 1 collide on the pending map and one of
// them gets the other's answer.
func TestWsSendRequest_RewritesTheOnWireIdOutOfTheClientRange(t *testing.T) {
	u, seen := idRecordingWsServer(t)
	c := newTestWsClient(t, u)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`))
		_, _ = c.SendRequest(ctx, req)
	}()

	select {
	case wireID := <-seen:
		if wireID == 1 {
			t.Fatal("the caller's own id 1 went out on the wire; two callers sharing an id would collide")
		}
		// The counter is seeded above the client-traffic range so an operator
		// reading upstream logs can tell eRPC's internal ids from a client's
		// small integers.
		if wireID <= float64(wireIDOffset) {
			t.Fatalf("wire id = %v, want it above the %d internal-traffic offset", wireID, wireIDOffset)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the server never saw the request")
	}
	<-done
}

// A subscription handler must stop firing once it is unregistered. A handler
// that survives teardown keeps a dead subscriber's callback alive on a
// long-lived connection, and the callbacks accumulate for the process
// lifetime.
func TestWsSubscriptionHandlers_UnregisterStopsDelivery(t *testing.T) {
	srv := newFakeWsServer(t)
	c := newTestWsClient(t, srv.wsURL(t))

	var conn *fakeWsConn
	select {
	case conn = <-srv.newConn:
	case <-time.After(5 * time.Second):
		t.Fatal("the client never connected")
	}

	subID := subscribeNewHeads(t, c)

	fired := make(chan struct{}, 4)
	c.RegisterSubscriptionHandler(subID, func(string, []byte) { fired <- struct{}{} })

	conn.sendNewHead(subID, "0x1")
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("a registered handler never received its notification")
	}

	c.UnregisterSubscriptionHandler(subID)
	conn.sendNewHead(subID, "0x2")
	select {
	case <-fired:
		t.Fatal("an unregistered handler still received a notification")
	case <-time.After(300 * time.Millisecond):
	}
}

// Disconnect and reconnect callbacks are keyed so a subscription can drop its
// own on teardown. If Remove did nothing, every torn-down subscription would
// keep firing its callback on every future reconnect.
func TestWsCallbacks_RemoveDeregistersByKey(t *testing.T) {
	srv := newFakeWsServer(t)
	c := newTestWsClient(t, srv.wsURL(t))

	c.SetOnDisconnect("sub-a", func() {})
	c.SetOnDisconnect("sub-b", func() {})
	c.SetOnReconnect("sub-a", func() {})
	c.SetOnReconnect("sub-b", func() {})

	c.RemoveOnDisconnect("sub-a")
	c.RemoveOnReconnect("sub-a")

	c.onDisconnectMu.RLock()
	_, aDisc := c.onDisconnectCbs["sub-a"]
	_, bDisc := c.onDisconnectCbs["sub-b"]
	c.onDisconnectMu.RUnlock()

	c.onReconnectMu.RLock()
	_, aRec := c.onReconnectCbs["sub-a"]
	_, bRec := c.onReconnectCbs["sub-b"]
	c.onReconnectMu.RUnlock()

	if aDisc {
		t.Error("RemoveOnDisconnect left the callback registered")
	}
	if aRec {
		t.Error("RemoveOnReconnect left the callback registered")
	}
	if !bDisc || !bRec {
		t.Error("removing one key also removed another subscription's callbacks")
	}
}

func TestWsGetType_IsWsJsonRpc(t *testing.T) {
	srv := newFakeWsServer(t)
	if got := newTestWsClient(t, srv.wsURL(t)).GetType(); got != ClientTypeWsJsonRpc {
		t.Fatalf("GetType() = %v, want %v", got, ClientTypeWsJsonRpc)
	}
}

// A live connection must report itself connected, or the ping loop skips it
// and a real dead-peer never gets detected.
func TestWsIsConnected_TrueOnALiveConnection(t *testing.T) {
	srv := newFakeWsServer(t)
	c := newTestWsClient(t, srv.wsURL(t))

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.IsConnected() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("a live websocket client never reported itself connected")
}

// A notification for a subscription nobody registered must be dropped, not
// delivered to another subscriber's handler.
func TestWsHandleNotification_UnknownSubscriptionReachesNoHandler(t *testing.T) {
	srv := newFakeWsServer(t)
	c := newTestWsClient(t, srv.wsURL(t))

	var conn *fakeWsConn
	select {
	case conn = <-srv.newConn:
	case <-time.After(5 * time.Second):
		t.Fatal("the client never connected")
	}

	subID := subscribeNewHeads(t, c)
	fired := make(chan struct{}, 4)
	c.RegisterSubscriptionHandler(subID, func(string, []byte) { fired <- struct{}{} })

	conn.sendNewHead("0xsomeoneelsessubscription", "0x1")
	select {
	case <-fired:
		t.Fatal("a notification for another subscription was delivered to this handler")
	case <-time.After(300 * time.Millisecond):
	}

	// And the registered subscription must still work afterwards — the unknown
	// notification must not have torn anything down.
	conn.sendNewHead(subID, "0x2")
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("the registered handler stopped working after an unknown notification")
	}
}
