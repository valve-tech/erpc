package clients

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// flakyWsServer refuses the first refuseFirst upgrade attempts with HTTP 500
// and accepts every attempt after that. That is what an upstream that is not
// up yet looks like to the client: the dial itself fails, so the constructor
// hands the connection off to the background reconnect loop.
type flakyWsServer struct {
	srv       *httptest.Server
	attempts  atomic.Int64
	closeOnce sync.Once
}

// newFlakyWsServer starts a server that rejects the first refuseFirst upgrades.
// Close is guarded by sync.Once: httptest.Server.Close blocks when called
// twice, and both t.Cleanup and an explicit close would otherwise hit it.
func newFlakyWsServer(t *testing.T, refuseFirst int64) *flakyWsServer {
	t.Helper()
	f := &flakyWsServer{}
	upgrader := websocket.Upgrader{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.attempts.Add(1) <= refuseFirst {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
	}))
	t.Cleanup(f.close)
	return f
}

func (f *flakyWsServer) close() { f.closeOnce.Do(f.srv.Close) }

func (f *flakyWsServer) wsURL(t *testing.T) *url.URL {
	t.Helper()
	u, err := url.Parse(f.srv.URL)
	require.NoError(t, err)
	u.Scheme = "ws"
	return u
}

// newWsClientWithCtx builds a client on a caller-owned context so a test can
// cancel it mid-reconnect.
func newWsClientWithCtx(t *testing.T, ctx context.Context, u *url.URL) *WsJsonRpcClient {
	t.Helper()
	logger := zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.WarnLevel)
	ci, err := NewWsJsonRpcClient(ctx, &logger, "test-project", common.NewFakeUpstream("test-ws-upstream"), u, nil, nil)
	require.NoError(t, err)
	c, ok := ci.(*WsJsonRpcClient)
	require.True(t, ok)
	return c
}

// waitConnected polls until c reports a live connection. The merged client
// has no connReady channel: readLoop owns the lifecycle and IsConnected is the
// observable signal.
func waitConnected(t *testing.T, c *WsJsonRpcClient, within time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if c.IsConnected() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

// An upstream that is not up yet must not fail startup. The constructor hands
// the dial to the background loop, and the loop must eventually connect and
// deliver. If it connected but never resumed reading, the client would be
// connected yet deliver nothing.
func TestWsReconnect_FirstSuccessAfterAFailedStartupReleasesReadLoop(t *testing.T) {
	srv := newFlakyWsServer(t, 1) // the constructor's own dial is refused
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newWsClientWithCtx(t, ctx, srv.wsURL(t))

	waitConnected(t, c, 5*time.Second, "the background reconnect never connected")
	require.GreaterOrEqual(t, srv.attempts.Load(), int64(2),
		"the client never retried after the refused first dial")
}

// A reconnect must fire the registered reconnect callbacks. Subscribers use
// them to re-subscribe; a reconnect that skips them leaves a client connected
// to a socket carrying none of its subscriptions.
func TestWsReconnect_FiresTheReconnectCallbacks(t *testing.T) {
	srv := newFlakyWsServer(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newWsClientWithCtx(t, ctx, srv.wsURL(t))
	waitConnected(t, c, 5*time.Second, "initial connection never completed")

	fired := make(chan struct{}, 1)
	c.SetOnReconnect("sub-1", func() {
		select {
		case fired <- struct{}{}:
		default:
		}
	})

	c.Reconnect(c.Epoch())

	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("reconnect did not fire the registered callback; subscriptions are never restored")
	}
}

// A cancelled app context must stop the reconnect loop. The loop is otherwise
// infinite: it retries until it connects. eRPC cancels this context on process
// shutdown, so a loop that ignores it keeps re-dialling a dead upstream while
// the process is trying to exit.
func TestWsReconnect_CancelledContextStopsTheLoop(t *testing.T) {
	srv := newFlakyWsServer(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	c := newWsClientWithCtx(t, ctx, srv.wsURL(t))

	waitConnected(t, c, 5*time.Second, "initial connection never completed")
	cancel()
	// Let the shutdown goroutine finish before counting dials.
	time.Sleep(50 * time.Millisecond)
	before := srv.attempts.Load()

	// Longer than the largest first backoff step (wsReconnectMin, jittered
	// down to half), so a loop that ignored the cancel would have re-dialled.
	time.Sleep(wsReconnectMin + 300*time.Millisecond)

	require.Equal(t, before, srv.attempts.Load(),
		"the client dialled the upstream after the app context was cancelled")
	require.False(t, c.IsConnected(),
		"the client re-armed the connection after the app context was cancelled")
}

// A context cancelled WHILE the loop waits out its backoff must stop it: no
// further dial may happen once the wait would have ended.
func TestWsReconnect_CancelDuringBackoffReturnsWithoutWaitingItOut(t *testing.T) {
	srv := newFlakyWsServer(t, 1<<40) // every dial is refused
	ctx, cancel := context.WithCancel(context.Background())
	_ = newWsClientWithCtx(t, ctx, srv.wsURL(t))

	// The constructor's dial fails; the loop is now inside its first backoff
	// wait (wsReconnectMin jittered to at least half of it).
	time.Sleep(100 * time.Millisecond)
	cancel()
	before := srv.attempts.Load()

	time.Sleep(2*wsReconnectMin + 300*time.Millisecond)
	require.Equal(t, before, srv.attempts.Load(),
		"the loop dialled again after its context was cancelled mid-backoff")
}

// teardownConn is how every in-flight caller learns the socket died. A caller
// left waiting gets no answer until its own timeout — and for a caller with no
// deadline, never. It must reach EVERY pending caller, and empty the map.
func TestDrainPending_EveryWaitingCallerIsWokenWithTheError(t *testing.T) {
	srv := newFakeWsServer(t)
	c := newTestWsClient(t, srv.wsURL(t))

	// SendRequest always gives its pending entry a 1-slot buffer, so the
	// teardown send never blocks on a caller that has gone away.
	chans := map[string]chan *wsPendingResult{
		"1": make(chan *wsPendingResult, 1),
		"2": make(chan *wsPendingResult, 1),
		"3": make(chan *wsPendingResult, 1),
	}
	c.pendingMu.Lock()
	for id, ch := range chans {
		c.pending[id] = &wsPending{ch: ch}
	}
	c.pendingMu.Unlock()

	cause := common.NewErrEndpointTransportFailure(c.Url, context.Canceled)

	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()
	done := make(chan struct{})
	go func() { c.teardownConn(conn, cause); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("teardownConn blocked; the whole client wedges")
	}

	for name, ch := range chans {
		select {
		case got := <-ch:
			require.Error(t, got.err, "caller %s was woken without an error", name)
			require.True(t, common.HasErrorCode(got.err, common.ErrCodeEndpointTransportFailure),
				"caller %s got %v, not the transport failure that caused the teardown", name, got.err)
		default:
			t.Fatalf("waiting caller %s was never woken", name)
		}
	}

	// The map must be emptied, or the next teardown wakes callers that
	// already went home and a retry files its entry behind a stale one.
	c.pendingMu.Lock()
	left := len(c.pending)
	c.pendingMu.Unlock()
	require.Zero(t, left, "teardownConn left %d stale entries behind", left)
}

// Shutdown must drain too. A client whose app context is cancelled while a
// caller waits has to hand that caller an error; otherwise the request hangs
// until its own deadline while the process is trying to exit.
func TestShutdown_WakesAWaitingCaller(t *testing.T) {
	srv := newFakeWsServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	c := newWsClientWithCtx(t, ctx, srv.wsURL(t))

	waiting := make(chan *wsPendingResult, 1)
	c.pendingMu.Lock()
	c.pending["42"] = &wsPending{ch: waiting}
	c.pendingMu.Unlock()

	cancel()

	select {
	case got := <-waiting:
		require.Error(t, got.err)
		require.True(t, common.HasErrorCode(got.err, common.ErrCodeEndpointRequestCanceled),
			"shutdown woke the caller with %v, not a cancellation", got.err)
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown left a caller waiting; the request hangs until its own deadline")
	}
	require.False(t, c.IsConnected())
}
