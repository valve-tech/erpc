package failsafe

import (
	"context"
	"errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// releaseLog records every result handed to release.
type releaseLog struct {
	mu   sync.Mutex
	vals []string
}

func (l *releaseLog) release(r *string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.vals = append(l.vals, *r)
}

func (l *releaseLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.vals...)
}

func strp(s string) *string { return &s }

func keepNoErr(r *string, err error) bool { return err == nil && r != nil }

func TestRunHedged_MaxHedgesBoundsAttemptsAndAllFailuresReturnLast(t *testing.T) {
	var calls atomic.Int32
	var fired []int
	var firedMu sync.Mutex
	inner := func(ctx context.Context) (*string, error) {
		n := calls.Add(1)
		return nil, errors.New("fail-" + string(rune('0'+n)))
	}
	_, err := RunHedged[*string](
		context.Background(),
		2,
		func(idx int) time.Duration { return 0 },
		inner,
		keepNoErr,
		nil,
		HedgeHooks{OnFire: func(idx int, _ time.Duration) {
			firedMu.Lock()
			fired = append(fired, idx)
			firedMu.Unlock()
		}},
	)
	require.Error(t, err)
	assert.Regexp(t, `^fail-[123]$`, err.Error())
	assert.Equal(t, int32(3), calls.Load(), "primary + 2 hedges, never more")
	firedMu.Lock()
	assert.Equal(t, []int{1, 2}, fired, "OnFire reports hedges only, not the primary")
	firedMu.Unlock()
}

func TestRunHedged_NegativeMaxHedgesRunsPrimaryOnly(t *testing.T) {
	var calls atomic.Int32
	r, err := RunHedged[*string](
		context.Background(), -3, nil,
		func(ctx context.Context) (*string, error) {
			calls.Add(1)
			return nil, errors.New("primary failed")
		},
		keepNoErr, nil, HedgeHooks{},
	)
	assert.Nil(t, r)
	assert.EqualError(t, err, "primary failed")
	assert.Equal(t, int32(1), calls.Load())
}

func TestRunHedged_NilKeepAcceptsFirstResult(t *testing.T) {
	r, err := RunHedged[*string](
		context.Background(), 0, nil,
		func(ctx context.Context) (*string, error) { return strp("partial"), errors.New("soft error") },
		nil, nil, HedgeHooks{},
	)
	require.NotNil(t, r)
	assert.Equal(t, "partial", *r)
	assert.EqualError(t, err, "soft error", "nil keep returns the first result with its error")
}

func TestRunHedged_HedgeRecoversAfterFastPrimaryFailure(t *testing.T) {
	var calls atomic.Int32
	hedgeGo := make(chan struct{})
	inner := func(ctx context.Context) (*string, error) {
		if calls.Add(1) == 1 {
			close(hedgeGo)
			return nil, errors.New("primary transient")
		}
		return strp("hedge"), nil
	}
	r, err := RunHedged[*string](
		context.Background(), 1,
		func(idx int) time.Duration {
			<-hedgeGo // arm the hedge timer only once the primary is running
			return 5 * time.Millisecond
		},
		inner, keepNoErr, nil, HedgeHooks{},
	)
	require.NoError(t, err)
	assert.Equal(t, "hedge", *r, "a scheduled hedge still fires after the primary failed")
}

func TestRunHedged_NegativeDelayFiresHedgeImmediately(t *testing.T) {
	var calls atomic.Int32
	primaryRelease := make(chan struct{})
	defer close(primaryRelease)
	inner := func(ctx context.Context) (*string, error) {
		if calls.Add(1) == 1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-primaryRelease:
				return strp("primary"), nil
			}
		}
		return strp("hedge"), nil
	}
	r, err := RunHedged[*string](
		context.Background(), 1,
		func(idx int) time.Duration { return -time.Second },
		inner, keepNoErr, nil, HedgeHooks{},
	)
	require.NoError(t, err)
	assert.Equal(t, "hedge", *r)
}

func TestRunHedged_LosersAreReleasedAndCanceled(t *testing.T) {
	var calls atomic.Int32
	losersCanceled := make(chan struct{}, 2)
	winnerGo := make(chan struct{})
	started := make(chan struct{}, 3)
	inner := func(ctx context.Context) (*string, error) {
		n := calls.Add(1)
		started <- struct{}{}
		if n == 3 {
			<-winnerGo
			return strp("winner"), nil
		}
		<-ctx.Done()
		losersCanceled <- struct{}{}
		return strp("loser"), nil // non-nil: must be released
	}
	var rel releaseLog
	go func() {
		for range 3 {
			<-started
		}
		close(winnerGo)
	}()
	r, err := RunHedged[*string](
		context.Background(), 2,
		func(idx int) time.Duration { return 0 },
		inner, keepNoErr, rel.release, HedgeHooks{},
	)
	require.NoError(t, err)
	assert.Equal(t, "winner", *r)
	for range 2 {
		select {
		case <-losersCanceled:
		case <-time.After(5 * time.Second):
			t.Fatal("losers must observe sibling cancellation")
		}
	}
	// RunHedged drains every in-flight attempt before returning.
	assert.ElementsMatch(t, []string{"loser", "loser"}, rel.snapshot(), "winner is never released")
}

func TestRunHedged_NotKeptResultsAreNotReleasedWhenReturned(t *testing.T) {
	var rel releaseLog
	r, err := RunHedged[*string](
		context.Background(), 0, nil,
		func(ctx context.Context) (*string, error) { return strp("rejected"), errors.New("bad") },
		keepNoErr, rel.release, HedgeHooks{},
	)
	require.EqualError(t, err, "bad")
	assert.Equal(t, "rejected", *r, "caller owns the returned non-kept result")
	assert.Empty(t, rel.snapshot())
}

func TestRunHedged_ParentCancelReleasesInFlightResults(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, 2)
	inner := func(c context.Context) (*string, error) {
		started <- struct{}{}
		<-c.Done()
		return strp("abandoned"), nil
	}
	go func() {
		<-started
		<-started
		cancel()
	}()
	var rel releaseLog
	r, err := RunHedged[*string](
		ctx, 1,
		func(idx int) time.Duration { return 0 },
		inner, keepNoErr, rel.release, HedgeHooks{},
	)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, r)
	assert.Equal(t, []string{"abandoned", "abandoned"}, rel.snapshot())
}

func TestRunHedged_ParentCancelSkipsZeroResults(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	go func() {
		<-started
		cancel()
	}()
	var rel releaseLog
	_, err := RunHedged[*string](
		ctx, 0, nil,
		func(c context.Context) (*string, error) {
			close(started)
			<-c.Done()
			return nil, c.Err()
		},
		keepNoErr, rel.release, HedgeHooks{},
	)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, rel.snapshot(), "nil results are never passed to release")
}

func TestRunHedged_PanicInAttemptBecomesError(t *testing.T) {
	var calls atomic.Int32
	r, err := RunHedged[*string](
		context.Background(), 1,
		func(idx int) time.Duration { return 0 },
		func(ctx context.Context) (*string, error) {
			if calls.Add(1) == 1 {
				panic("attempt exploded")
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
		func(r *string, err error) bool { return err != nil },
		nil, HedgeHooks{},
	)
	assert.Nil(t, r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hedge attempt panic: attempt exploded")
}
