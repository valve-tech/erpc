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

// An upstream that is not up yet must not fail startup. The constructor hands
// the dial to the background reconnect loop, and the loop must close connReady
// once it finally connects — that channel is what readLoop parks on. If the
// loop connects but never closes connReady, readLoop stays parked forever and
// the client is connected yet delivers nothing.
func TestWsReconnect_FirstSuccessAfterAFailedStartupReleasesReadLoop(t *testing.T) {
	srv := newFlakyWsServer(t, 1) // the constructor's own dial is refused
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newWsClientWithCtx(t, ctx, srv.wsURL(t))

	select {
	case <-c.connReady:
	case <-time.After(5 * time.Second):
		t.Fatal("the background reconnect connected but never released readLoop")
	}
	require.True(t, c.IsConnected())
	require.GreaterOrEqual(t, srv.attempts.Load(), int64(2),
		"the client never retried after the refused first dial")
}

// A reconnect must fire the registered reconnect callbacks. Subscribers use
// them to re-subscribe; a reconnect that skips them leaves a client connected
// to a socket carrying none of its subscriptions.
func TestWsReconnect_FiresTheReconnectCallbacks(t *testing.T) {
	srv := newFlakyWsServer(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newWsClientWithCtx(t, ctx, srv.wsURL(t))
	select {
	case <-c.connReady:
	case <-time.After(5 * time.Second):
		t.Fatal("initial reconnect never completed")
	}

	fired := make(chan struct{}, 1)
	c.SetOnReconnect("sub-1", func() {
		select {
		case fired <- struct{}{}:
		default:
		}
	})

	c.reconnect()

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("reconnect did not fire the registered callback; subscriptions are never restored")
	}
}

// A cancelled app context must stop the reconnect loop. The loop is otherwise
// infinite: it retries until it connects. eRPC cancels this context on process
// shutdown, so a loop that ignores it keeps re-dialling a dead upstream while
// the process is trying to exit, and the exit blocks behind it.
//
// The loop checks the context in two places — at the top of each attempt and
// inside the backoff wait. Either one alone stops it, so this test asserts the
// joint property; only removing BOTH checks makes it hang.
func TestWsReconnect_CancelledContextStopsTheLoop(t *testing.T) {
	srv := newFlakyWsServer(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	c := newWsClientWithCtx(t, ctx, srv.wsURL(t))

	select {
	case <-c.connReady:
	case <-time.After(5 * time.Second):
		t.Fatal("initial connection never completed")
	}
	cancel()
	// Let the shutdown goroutine finish before counting dials.
	time.Sleep(50 * time.Millisecond)
	before := srv.attempts.Load()

	done := make(chan struct{})
	go func() { c.reconnect(); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reconnect kept running after the app context was cancelled")
	}
	require.Equal(t, before, srv.attempts.Load(),
		"reconnect dialled the upstream after the app context was cancelled")
	require.False(t, c.IsConnected(),
		"reconnect re-armed the connection after the app context was cancelled")
}

// A context cancelled WHILE the loop waits out its backoff must break the wait
// immediately. Without the appCtx branch in that select, shutdown stalls for a
// full backoff step — up to 30 seconds once the ladder has climbed.
func TestWsReconnect_CancelDuringBackoffReturnsWithoutWaitingItOut(t *testing.T) {
	srv := newFlakyWsServer(t, 0)
	u := srv.wsURL(t)
	ctx, cancel := context.WithCancel(context.Background())
	c := newWsClientWithCtx(t, ctx, u)

	select {
	case <-c.connReady:
	case <-time.After(5 * time.Second):
		t.Fatal("initial connection never completed")
	}

	// Take the server away so every dial from here on fails and the loop
	// enters its backoff wait.
	srv.close()

	done := make(chan struct{})
	go func() { c.reconnect(); close(done) }()

	// Let the first dial fail and the loop settle into the backoff wait. The
	// first step is one second, so cancelling now lands well inside it and
	// leaves ~800ms of wait that must NOT be served.
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reconnect waited out its backoff instead of honouring the cancelled context")
	}
	require.Less(t, time.Since(start), 300*time.Millisecond,
		"reconnect returned only after the backoff elapsed, not on cancellation")
}

// A reconnect that lands while the wake pulse is still unread must not block.
// connWake has capacity 1 and readLoop only drains it when parked, so a second
// reconnect finding it full is normal — and a blocking send there would wedge
// the client's only reconnect path forever.
func TestWsReconnect_DoesNotBlockWhenTheWakePulseIsAlreadyPending(t *testing.T) {
	srv := newFakeWsServer(t)
	c := newTestWsClient(t, srv.wsURL(t))

	// Fill the wake channel so reconnect's non-blocking send has nowhere to go.
	select {
	case c.connWake <- struct{}{}:
	default:
		t.Fatal("connWake was already full before the test filled it")
	}

	done := make(chan struct{})
	go func() { c.reconnect(); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reconnect blocked on a full wake channel; the client can never reconnect again")
	}
	// Nothing else is asserted here on purpose. The client's own readLoop is
	// concurrently tearing down the connection this reconnect displaced, so its
	// connected flag is genuinely racy at this instant. Returning at all is the
	// property under test.
}

// drainPending is how every in-flight caller learns the socket died. A caller
// left waiting gets no answer until its own timeout — and for a caller with no
// deadline, never. The loop must reach EVERY pending caller, not just the
// first, and must not block on one whose channel nobody is reading.
func TestDrainPending_EveryWaitingCallerIsWokenWithTheError(t *testing.T) {
	srv := newFakeWsServer(t)
	c := newTestWsClient(t, srv.wsURL(t))

	readable := make(chan *wsPendingResult, 1)
	abandoned := make(chan *wsPendingResult) // unbuffered, nobody receiving
	second := make(chan *wsPendingResult, 1)

	c.pendingMu.Lock()
	c.pending["1"] = readable
	c.pending["2"] = abandoned
	c.pending["3"] = second
	c.pendingMu.Unlock()

	cause := common.NewErrEndpointTransportFailure(c.Url, context.Canceled)

	done := make(chan struct{})
	go func() { c.drainPending(cause); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drainPending blocked on an abandoned caller; the whole client wedges")
	}

	for name, ch := range map[string]chan *wsPendingResult{"first": readable, "third": second} {
		select {
		case got := <-ch:
			require.Error(t, got.err, "the %s caller was woken without an error", name)
			require.True(t, common.HasErrorCode(got.err, common.ErrCodeEndpointTransportFailure),
				"the %s caller got %v, not the transport failure that caused the drain", name, got.err)
		default:
			t.Fatalf("the %s waiting caller was never woken", name)
		}
	}

	// The map must be emptied, or the next drain wakes callers that already
	// went home and a retry files its entry behind a stale one.
	c.pendingMu.RLock()
	left := len(c.pending)
	c.pendingMu.RUnlock()
	require.Zero(t, left, "drainPending left %d stale entries behind", left)
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
	c.pending["42"] = waiting
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
