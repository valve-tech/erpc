package clients

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The reconnect backoff ladder decides how fast a client comes back after an
// upstream drops, and — more importantly — how hard it hammers an upstream
// that stays down. Climbing to the real 30-second ceiling costs 31 seconds of
// waiting, so the ladder was never exercised: nothing showed that the wait
// grows, and nothing showed that it saturates. wsReconnectMin/Max/Factor now
// live in the same var block as the liveness windows and are snapshotted into
// per-client fields at construction, which lets these tests compress time
// without racing the client's own goroutines.

// compressReconnectLadder shrinks the backoff ladder for one test. The values
// are read only by NewWsJsonRpcClient, on the caller's goroutine, so a client
// built before this returns keeps the ladder it was born with.
func compressReconnectLadder(t *testing.T, min, max time.Duration, factor float64) {
	t.Helper()
	oldMin, oldMax, oldFactor := wsReconnectMin, wsReconnectMax, wsReconnectFactor
	wsReconnectMin, wsReconnectMax, wsReconnectFactor = min, max, factor
	t.Cleanup(func() {
		wsReconnectMin, wsReconnectMax, wsReconnectFactor = oldMin, oldMax, oldFactor
	})
}

// A client must carry its OWN copy of the ladder. reconnect() runs on a
// background goroutine, so reading the package vars there would race any later
// write — the same reason the liveness windows are snapshotted.
func TestWsClient_SnapshotsTheBackoffLadderAtConstruction(t *testing.T) {
	compressReconnectLadder(t, 7*time.Millisecond, 21*time.Millisecond, 3.0)

	srv := newFakeWsServer(t)
	c := newTestWsClient(t, srv.wsURL(t))

	require.Equal(t, 7*time.Millisecond, c.reconnectMin)
	require.Equal(t, 21*time.Millisecond, c.reconnectMax)
	require.Equal(t, 3.0, c.reconnectFactor)

	// A later change to the package vars must not reach a live client.
	wsReconnectMin = time.Hour
	require.Equal(t, 7*time.Millisecond, c.reconnectMin,
		"the client re-read the package var instead of its snapshot")
}

// The ladder must GROW and then SATURATE. Growth is what stops a down upstream
// from being hammered once a second forever; saturation is what stops the wait
// from running away — without the ceiling the fifth retry of a long outage
// waits over a minute, and a recovered upstream sits idle for that whole time.
//
// The two bounds below pin both halves at once. With min=10ms, factor=10 and
// max=50ms the four waits before success are 10+50+50+50 = 160ms. A ladder
// that never multiplies waits 4x10 = 40ms and misses the lower bound; a ladder
// with no ceiling waits 10+100+1000+10000 = over 11 seconds and misses the
// upper one.
func TestWsReconnect_BackoffClimbsThenSaturatesAtTheCeiling(t *testing.T) {
	compressReconnectLadder(t, 10*time.Millisecond, 50*time.Millisecond, 10.0)

	// The constructor's own dial is refused too, so the loop makes four failed
	// attempts (and four backoff waits) before the fifth succeeds.
	srv := newFlakyWsServer(t, 5)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := time.Now()
	c := newWsClientWithCtx(t, ctx, srv.wsURL(t))
	select {
	case <-c.connReady:
	case <-time.After(20 * time.Second):
		t.Fatal("the client never reconnected; the backoff ladder never saturated")
	}
	elapsed := time.Since(start)

	require.GreaterOrEqual(t, elapsed, 100*time.Millisecond,
		"four retries finished in %v — faster than a ladder that multiplies its wait can manage", elapsed)
	require.Less(t, elapsed, 5*time.Second,
		"four retries took %v — the wait is not being clamped at the ceiling", elapsed)
	require.GreaterOrEqual(t, srv.attempts.Load(), int64(5),
		"the client stopped retrying before the server started accepting")
}

// The first retry must wait wsReconnectMin, not zero. A client that retries
// instantly turns one dropped upstream into a dial storm.
func TestWsReconnect_FirstRetryWaitsTheMinimum(t *testing.T) {
	compressReconnectLadder(t, 300*time.Millisecond, 10*time.Second, 2.0)

	srv := newFlakyWsServer(t, 2) // constructor dial + one loop attempt refused
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := time.Now()
	c := newWsClientWithCtx(t, ctx, srv.wsURL(t))
	select {
	case <-c.connReady:
	case <-time.After(20 * time.Second):
		t.Fatal("the client never reconnected")
	}
	require.GreaterOrEqual(t, time.Since(start), 300*time.Millisecond,
		"the single retry took less than the configured minimum backoff")
}
