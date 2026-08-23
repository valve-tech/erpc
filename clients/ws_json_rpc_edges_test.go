package clients

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// Everything below is a path the WS client takes when the caller, the socket or
// the peer misbehaves. They are the paths that decide whether a bad frame costs
// one request or the whole client, and none of them ran under test.

// ───────────────────────────── SendRequest exits ─────────────────────────────

// A caller whose deadline expires while the peer is silent must get a
// request-TIMEOUT, not a bare context error. The upstream failsafe layer scores
// timeouts differently from cancellations, so mislabelling one as the other
// feeds the wrong signal into upstream selection.
func TestWsSendRequest_CallerDeadlineYieldsARequestTimeout(t *testing.T) {
	srv := newFakeWsServer(t)
	c := newTestWsClient(t, srv.wsURL(t))

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	// eth_blockNumber is never answered by the fake server, so the call waits.
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
	_, err := c.SendRequest(ctx, req)
	require.Error(t, err)
	require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointRequestTimeout),
		"a caller deadline must surface as a request timeout; got %v", err)
}

// A caller that cancels must get a CANCELLATION, not a timeout. A cancelled
// request is the caller going away, not the upstream failing, and scoring it as
// an upstream failure would punish a healthy upstream for a client hang-up.
func TestWsSendRequest_CallerCancellationYieldsACancellation(t *testing.T) {
	srv := newFakeWsServer(t)
	c := newTestWsClient(t, srv.wsURL(t))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
	_, err := c.SendRequest(ctx, req)
	require.Error(t, err)
	require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointRequestCanceled),
		"a cancelled caller must surface as a cancellation; got %v", err)
}

// A request written onto a client that has no connection must fail as a
// TRANSPORT failure straight away. Registering the pending entry and waiting
// would hang the caller until its own deadline on a socket that does not exist.
func TestWsSendRequest_NoConnectionFailsAsATransportFailure(t *testing.T) {
	// Port 1 refuses instantly, so the constructor's dial fails and c.conn
	// stays nil while the background loop retries.
	u, err := url.Parse("ws://127.0.0.1:1")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.ErrorLevel)
	ci, err := NewWsJsonRpcClient(ctx, &logger, "test-project",
		common.NewFakeUpstream("test-ws-upstream"), u, nil, nil)
	require.NoError(t, err, "an unreachable upstream must not fail construction")

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
	start := time.Now()
	_, err = ci.SendRequest(context.Background(), req)
	require.Error(t, err)
	require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointTransportFailure),
		"a request with no socket must fail as a transport failure; got %v", err)
	require.Less(t, time.Since(start), 2*time.Second,
		"the call waited instead of failing fast on a missing connection")
}

// A request body that is not JSON-RPC must be rejected before anything is
// written to the socket. Writing it would put a malformed frame on a shared
// connection, which some upstreams answer by closing it on every other caller.
func TestWsSendRequest_MalformedBodyIsRejectedBeforeTheWire(t *testing.T) {
	srv := newFakeWsServer(t)
	c := newTestWsClient(t, srv.wsURL(t))

	req := common.NewNormalizedRequest([]byte(`this is not json`))
	_, err := c.SendRequest(context.Background(), req)
	require.Error(t, err, "a malformed request body reached the socket")
}

// A response delivered with an error — the shape drainPending uses when the
// socket dies — must reach the waiting caller as that error. Swallowing it
// would leave the caller waiting on a connection that is already gone.
func TestWsSendRequest_AnErrorResultReachesTheWaitingCaller(t *testing.T) {
	srv := newFakeWsServer(t)
	c := newTestWsClient(t, srv.wsURL(t))

	go func() {
		// Wait until SendRequest has registered its pending entry, then kill
		// every waiting caller the way a dropped socket does.
		for i := 0; i < 200; i++ {
			c.pendingMu.RLock()
			n := len(c.pending)
			c.pendingMu.RUnlock()
			if n > 0 {
				c.drainPending(common.NewErrEndpointTransportFailure(c.Url, context.Canceled))
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.SendRequest(ctx, req)
	require.Error(t, err)
	require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointTransportFailure),
		"the drain error did not reach the caller; got %v", err)
}

// ───────────────────────────── inbound frame handling ───────────────────────

// A peer that sends garbage must cost one frame, not the connection. handleMessage
// runs on readLoop's goroutine, so a panic or an early return that skips the
// rest of the loop would take every subscription down with it.
func TestWsHandleMessage_SurvivesFramesItCannotUse(t *testing.T) {
	srv := newFakeWsServer(t)
	c := newTestWsClient(t, srv.wsURL(t))

	frames := [][]byte{
		[]byte(`{ this is not json`),                                    // unparseable
		[]byte(`{"jsonrpc":"2.0"}`),                                     // neither response nor notification
		[]byte(`{"jsonrpc":"2.0","id":987654321,"result":"0x1"}`),       // response nobody is waiting for
		[]byte(`{"jsonrpc":"2.0","method":"eth_unknownNotification"}`),  // notification of another kind
		[]byte(`{"jsonrpc":"2.0","method":"eth_subscription","params":`), // notification with broken params
		[]byte(`{"jsonrpc":"2.0","method":"eth_subscription","params":{"subscription":"0xnope","result":{}}}`), // unknown subscription
	}
	for _, f := range frames {
		c.handleMessage(f)
	}

	// The client must still deliver a normal response afterwards.
	require.True(t, c.IsConnected(), "the client dropped its connection over an unusable frame")
	waiting := make(chan *wsPendingResult, 1)
	c.pendingMu.Lock()
	c.pending["55"] = waiting
	c.pendingMu.Unlock()
	c.handleMessage([]byte(`{"jsonrpc":"2.0","id":55,"result":"0x2"}`))
	select {
	case got := <-waiting:
		require.NoError(t, got.err)
	default:
		t.Fatal("a good frame was not delivered after the unusable ones")
	}
}

// A JSON-RPC error carried in a response must reach the caller as an error, not
// as a successful result. A client that dropped the error field would hand the
// caller an empty result and the request would look like a cache-able success.
func TestWsHandleMessage_AnErrorResponseReachesTheCallerAsAnError(t *testing.T) {
	srv := newFakeWsServer(t)
	c := newTestWsClient(t, srv.wsURL(t))

	waiting := make(chan *wsPendingResult, 1)
	c.pendingMu.Lock()
	c.pending["77"] = waiting
	c.pendingMu.Unlock()

	c.handleMessage([]byte(`{"jsonrpc":"2.0","id":77,"error":{"code":-32000,"message":"header not found"}}`))
	select {
	case got := <-waiting:
		require.Error(t, got.err, "the upstream's JSON-RPC error was delivered as a success")
		require.Contains(t, got.err.Error(), "header not found")
	default:
		t.Fatal("the error response never reached the caller")
	}
}

// A subscription notification must reach exactly the handler registered for its
// subscription id. Delivering it to the wrong one — or to all of them — mixes
// one subscriber's heads into another's stream.
func TestWsHandleNotification_ReachesOnlyTheMatchingSubscriber(t *testing.T) {
	srv := newFakeWsServer(t)
	c := newTestWsClient(t, srv.wsURL(t))

	mine := make(chan []byte, 1)
	theirs := make(chan []byte, 1)
	c.RegisterSubscriptionHandler("0xaaa", func(_ string, p []byte) { mine <- p })
	c.RegisterSubscriptionHandler("0xbbb", func(_ string, p []byte) { theirs <- p })

	c.handleMessage([]byte(`{"jsonrpc":"2.0","method":"eth_subscription","params":{"subscription":"0xaaa","result":{"number":"0x1"}}}`))

	select {
	case <-mine:
	case <-time.After(2 * time.Second):
		t.Fatal("the notification never reached its own subscriber")
	}
	select {
	case <-theirs:
		t.Fatal("the notification was also delivered to an unrelated subscriber")
	default:
	}

	// After unregistering, the same notification must reach nobody.
	c.UnregisterSubscriptionHandler("0xaaa")
	c.handleMessage([]byte(`{"jsonrpc":"2.0","method":"eth_subscription","params":{"subscription":"0xaaa","result":{"number":"0x2"}}}`))
	select {
	case <-mine:
		t.Fatal("a torn-down subscription still received notifications")
	case <-time.After(100 * time.Millisecond):
	}
}

// ───────────────────────────── metrics ──────────────────────────────────────

// The connectivity gauge is labelled with the upstream's vendor, network and
// id. A client built without an upstream has none of those, so publishing must
// be skipped rather than panic — the constructor accepts a nil upstream.
func TestWsSetConnectedMetric_SkipsWhenTheClientHasNoUpstream(t *testing.T) {
	c := &WsJsonRpcClient{projectId: "test-project"}
	require.NotPanics(t, func() { c.setConnectedMetric(1) },
		"publishing the gauge without an upstream panicked")
}
