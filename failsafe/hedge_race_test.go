package failsafe

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// These tests cover what RunHedged does with results the caller REJECTS.
// The rejected-result path is where eRPC's "empty result from a lagging
// upstream" behaviour lives: the caller's keep predicate says no, the race
// must continue, and every rejected response must be released so its buffer
// returns to the pool instead of leaking.

// countingRelease records how many results the hedger handed back.
type countingRelease struct {
	mu       sync.Mutex
	released []string
}

func (c *countingRelease) release(s string) {
	c.mu.Lock()
	c.released = append(c.released, s)
	c.mu.Unlock()
}

func (c *countingRelease) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.released))
	copy(out, c.released)
	return out
}

func TestRunHedged_RejectedPrimaryLetsTheHedgeStillWin(t *testing.T) {
	// The primary answers quickly but with something the caller will not
	// accept (an emptyish result from a lagging upstream). Returning that
	// answer would hand the client bad data even though a good upstream was
	// one hedge away.
	var calls atomic.Int32
	inner := func(ctx context.Context) (string, error) {
		if calls.Add(1) == 1 {
			return "empty", nil
		}
		return "good", nil
	}
	keep := func(r string, err error) bool { return err == nil && r == "good" }
	rel := &countingRelease{}

	got, err := RunHedged[string](
		context.Background(), 1,
		func(idx int) time.Duration { return 10 * time.Millisecond },
		inner, keep, rel.release, HedgeHooks{},
	)
	require.NoError(t, err)
	require.Equal(t, "good", got, "the accepted hedge result must win over the rejected primary")
	require.Equal(t, int32(2), calls.Load(), "the hedge must fire after the primary was rejected")
}

func TestRunHedged_AllRejectedReturnsTheLastAttemptAndItsError(t *testing.T) {
	// Every attempt failed the keep predicate. The caller must get the last
	// real (result, error) pair, not a synthesised nil — otherwise the layer
	// above cannot tell WHY the request failed.
	sentinel := errors.New("upstream said no")
	var calls atomic.Int32
	inner := func(ctx context.Context) (string, error) {
		calls.Add(1)
		return "rejected", sentinel
	}
	keep := func(r string, err error) bool { return false }

	got, err := RunHedged[string](
		context.Background(), 2,
		func(idx int) time.Duration { return 5 * time.Millisecond },
		inner, keep, nil, HedgeHooks{},
	)
	require.True(t, errors.Is(err, sentinel), "got %v, want the original cause to survive", err)
	require.Equal(t, "rejected", got)
	require.Equal(t, int32(3), calls.Load(), "all hedges must be tried before giving up")
}

func TestRunHedged_LateArrivalsAreReleasedNotLeaked(t *testing.T) {
	// A hedge that returns AFTER the winner was picked still holds a response
	// buffer. eRPC pools those buffers, so a missed release is a slow memory
	// leak that only shows up under sustained hedged traffic.
	rel := &countingRelease{}
	var calls atomic.Int32

	// The primary parks until the winner cancels it, then still returns a
	// non-zero result — the shape of a client that was mid-flight when the
	// hedge won. Waiting on ctx (not a test-only channel) matters: RunHedged
	// does not return until every in-flight attempt has reported, so gating
	// the loser on anything else would deadlock the test.
	inner := func(ctx context.Context) (string, error) {
		if calls.Add(1) == 1 {
			<-ctx.Done()
			return "loser", nil
		}
		return "winner", nil
	}
	keep := func(r string, err error) bool { return r == "winner" }

	got, err := RunHedged[string](
		context.Background(), 1,
		func(idx int) time.Duration { return 10 * time.Millisecond },
		inner, keep, rel.release, HedgeHooks{},
	)
	require.NoError(t, err)
	require.Equal(t, "winner", got)

	require.Contains(t, rel.snapshot(), "loser",
		"the late-arriving loser's result must be released back to the pool")
	require.NotContains(t, rel.snapshot(), "winner",
		"the winner belongs to the caller and must never be released by the hedger")
}

func TestRunHedged_OnFireHookReportsOnlyExtraAttempts(t *testing.T) {
	// The hook drives the hedge counter operators watch. Counting the primary
	// as a hedge would make every single request look hedged and hide a real
	// hedge-rate regression.
	var fires atomic.Int32
	var firstIdx atomic.Int32
	firstIdx.Store(-1)

	inner := func(ctx context.Context) (string, error) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(60 * time.Millisecond):
			return "slow", nil
		}
	}
	keep := func(r string, err error) bool { return err == nil && r != "" }

	_, err := RunHedged[string](
		context.Background(), 1,
		func(idx int) time.Duration { return 10 * time.Millisecond },
		inner, keep, nil,
		HedgeHooks{OnFire: func(fireIdx int, d time.Duration) {
			fires.Add(1)
			firstIdx.CompareAndSwap(-1, int32(fireIdx))
		}},
	)
	require.NoError(t, err)
	require.Equal(t, int32(1), fires.Load(), "only the extra attempt may be reported as a hedge")
	require.Equal(t, int32(1), firstIdx.Load(), "the primary is index 0 and is never reported")
}

func TestRunHedged_ZeroHedgesRunsThePrimaryAlone(t *testing.T) {
	// maxHedges: 0 must mean "no hedging", not "hedge once". Firing an extra
	// attempt here would double the upstream load of every operator who left
	// hedging switched off.
	var calls atomic.Int32
	inner := func(ctx context.Context) (string, error) {
		calls.Add(1)
		time.Sleep(30 * time.Millisecond)
		return "only", nil
	}
	got, err := RunHedged[string](
		context.Background(), 0,
		func(idx int) time.Duration { return time.Millisecond },
		inner, func(r string, err error) bool { return true }, nil, HedgeHooks{},
	)
	require.NoError(t, err)
	require.Equal(t, "only", got)
	require.Equal(t, int32(1), calls.Load())
}

func TestRunHedged_NegativeMaxHedgesIsClampedToZero(t *testing.T) {
	// A negative value from a bad config must not make `maxHedges+1` a
	// zero-capacity channel, which would block every goroutine's send.
	var calls atomic.Int32
	inner := func(ctx context.Context) (string, error) {
		calls.Add(1)
		return "only", nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		got, err := RunHedged[string](
			context.Background(), -5,
			func(idx int) time.Duration { return time.Millisecond },
			inner, func(r string, err error) bool { return true }, nil, HedgeHooks{},
		)
		require.NoError(t, err)
		require.Equal(t, "only", got)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunHedged deadlocked on a negative maxHedges")
	}
	require.Equal(t, int32(1), calls.Load())
}

func TestRunHedged_NilDelayFnFiresEveryHedgeImmediately(t *testing.T) {
	// A nil delay function means zero delay. Treating nil as "never fire"
	// would silently disable hedging for any caller that forgot the spec.
	var calls atomic.Int32
	inner := func(ctx context.Context) (string, error) {
		if calls.Add(1) < 3 {
			<-ctx.Done()
			return "", ctx.Err()
		}
		return "third", nil
	}
	got, err := RunHedged[string](
		context.Background(), 2, nil,
		inner, func(r string, err error) bool { return err == nil }, nil, HedgeHooks{},
	)
	require.NoError(t, err)
	require.Equal(t, "third", got)
	require.Equal(t, int32(3), calls.Load())
}

func TestRunHedged_WinnerCancelsItsSiblings(t *testing.T) {
	// Once a winner is chosen the remaining attempts must be cancelled, so a
	// hedged request does not keep burning upstream capacity (and rate-limit
	// budget) on work whose answer will be thrown away.
	siblingSawCancel := make(chan struct{}, 4)
	var calls atomic.Int32
	inner := func(ctx context.Context) (string, error) {
		if calls.Add(1) == 1 {
			// Primary hangs until it is cancelled.
			<-ctx.Done()
			siblingSawCancel <- struct{}{}
			return "", ctx.Err()
		}
		return "hedge", nil
	}
	got, err := RunHedged[string](
		context.Background(), 1,
		func(idx int) time.Duration { return 10 * time.Millisecond },
		inner, func(r string, err error) bool { return err == nil && r != "" }, nil, HedgeHooks{},
	)
	require.NoError(t, err)
	require.Equal(t, "hedge", got)
	select {
	case <-siblingSawCancel:
	case <-time.After(3 * time.Second):
		t.Fatal("the losing sibling was never cancelled: hedged work keeps running after a winner")
	}
}
