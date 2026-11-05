package failsafe

import (
	"context"
	"errors"
	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

// ComputeBackoff decides how long a retry waits. Two operator-visible things
// depend on it: an upstream that is failing must not be re-hammered at line
// rate, and a retry storm must not stall a request for minutes because the
// exponential factor ran away. These tests pin both ends.

func TestComputeBackoff_NilConfigMeansNoDelay(t *testing.T) {
	// A scope with no retry policy must not sleep at all. If this returned a
	// non-zero default, every un-configured retry path would silently add
	// latency to production traffic.
	require.Equal(t, time.Duration(0), ComputeBackoff(nil, 0))
	require.Equal(t, time.Duration(0), ComputeBackoff(nil, 7))
}

func TestComputeBackoff_ZeroDelayMeansImmediateRetry(t *testing.T) {
	// delay: 0 is the config way of saying "retry immediately". Neither the
	// backoff factor nor the jitter may resurrect a delay the operator
	// switched off — jitter is set here precisely so that skipping the
	// zero-delay short-circuit would produce a visible non-zero wait.
	cfg := &common.RetryPolicyConfig{
		Delay:         common.Duration(0),
		BackoffFactor: 4,
		Jitter:        common.Duration(50 * time.Millisecond),
	}
	for i := 0; i < 50; i++ {
		require.Equal(t, time.Duration(0), ComputeBackoff(cfg, 3))
	}
}

func TestComputeBackoff_FirstRetryUsesTheFlatDelay(t *testing.T) {
	// Attempt 0 is the FIRST retry and must not be scaled. Off-by-one here
	// would make every first retry wait a full backoff step.
	cfg := &common.RetryPolicyConfig{
		Delay:         common.Duration(100 * time.Millisecond),
		BackoffFactor: 2,
	}
	require.Equal(t, 100*time.Millisecond, ComputeBackoff(cfg, 0))
}

func TestComputeBackoff_GrowsExponentiallyPerAttempt(t *testing.T) {
	// The factor compounds once per attempt. A failing upstream gets
	// progressively more breathing room instead of a fixed drum beat.
	cfg := &common.RetryPolicyConfig{
		Delay:         common.Duration(100 * time.Millisecond),
		BackoffFactor: 2,
	}
	require.Equal(t, 200*time.Millisecond, ComputeBackoff(cfg, 1))
	require.Equal(t, 400*time.Millisecond, ComputeBackoff(cfg, 2))
	require.Equal(t, 800*time.Millisecond, ComputeBackoff(cfg, 3))
}

func TestComputeBackoff_MaxDelayCapsRunawayGrowth(t *testing.T) {
	// Without the cap, attempt 10 at factor 2 waits ~102s and the client
	// times out long before the retry fires. The cap is what keeps a deep
	// retry chain bounded.
	cfg := &common.RetryPolicyConfig{
		Delay:           common.Duration(100 * time.Millisecond),
		BackoffFactor:   2,
		BackoffMaxDelay: common.Duration(500 * time.Millisecond),
	}
	require.Equal(t, 500*time.Millisecond, ComputeBackoff(cfg, 10),
		"backoffMaxDelay must clamp the exponential result")
	require.Equal(t, 200*time.Millisecond, ComputeBackoff(cfg, 1),
		"a delay under the cap must pass through untouched")
}

func TestComputeBackoff_JitterStaysInsideItsWindow(t *testing.T) {
	// Jitter de-synchronises retries across concurrent requests so they do
	// not all land on the upstream in the same millisecond. It is ADDITIVE
	// and bounded: [base, base+jitter). A jitter that could subtract would
	// let a retry fire before the operator's configured delay.
	cfg := &common.RetryPolicyConfig{
		Delay:  common.Duration(100 * time.Millisecond),
		Jitter: common.Duration(50 * time.Millisecond),
	}
	sawSpread := false
	for i := 0; i < 200; i++ {
		d := ComputeBackoff(cfg, 0)
		require.GreaterOrEqual(t, d, 100*time.Millisecond, "jitter must never shorten the base delay")
		require.Less(t, d, 150*time.Millisecond, "jitter must stay inside the configured window")
		if d != 100*time.Millisecond {
			sawSpread = true
		}
	}
	require.True(t, sawSpread, "jitter must actually vary the delay, otherwise retries stay synchronised")
}

func TestComputeBackoff_JitterAppliesOnTopOfTheCap(t *testing.T) {
	// The cap clamps the exponential term, then jitter is added. An operator
	// reading backoffMaxDelay as an absolute ceiling would be surprised —
	// this test states the real contract so it cannot drift silently.
	cfg := &common.RetryPolicyConfig{
		Delay:           common.Duration(100 * time.Millisecond),
		BackoffFactor:   2,
		BackoffMaxDelay: common.Duration(200 * time.Millisecond),
		Jitter:          common.Duration(50 * time.Millisecond),
	}
	for i := 0; i < 50; i++ {
		d := ComputeBackoff(cfg, 9)
		require.GreaterOrEqual(t, d, 200*time.Millisecond)
		require.Less(t, d, 250*time.Millisecond)
	}
}

func TestSleepCtx_SleepsForTheRequestedDuration(t *testing.T) {
	start := time.Now()
	require.NoError(t, SleepCtx(context.Background(), 40*time.Millisecond))
	require.GreaterOrEqual(t, time.Since(start), 35*time.Millisecond,
		"SleepCtx must actually wait, otherwise backoff is a no-op")
}

func TestSleepCtx_CancellationSurfacesTheOriginalCause(t *testing.T) {
	// The recurring failure mode in this codebase is a layer swallowing a
	// cancellation and returning its own tidy error. A caller that cannot see
	// context.Canceled will keep retrying a request the client already gave
	// up on. errors.Is proves the cause survives the trip.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := SleepCtx(ctx, 10*time.Second)
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled), "got %v, want a context.Canceled cause", err)
	require.Less(t, time.Since(start), 5*time.Second, "SleepCtx must abandon the timer on cancellation")
}

func TestSleepCtx_DeadlineSurfacesAsDeadlineExceeded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := SleepCtx(ctx, 10*time.Second)
	require.True(t, errors.Is(err, context.DeadlineExceeded), "got %v, want context.DeadlineExceeded", err)
}

func TestSleepCtx_ZeroDurationReturnsEvenOnADeadContext(t *testing.T) {
	// A zero backoff means "retry now". Checking the context first would turn
	// an already-cancelled parent into a spurious error on a path that was
	// never going to block.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, SleepCtx(ctx, 0))
	require.NoError(t, SleepCtx(ctx, -1*time.Second))
}

func TestComputeBackoff(t *testing.T) {
	ms := func(n int) common.Duration { return common.Duration(time.Duration(n) * time.Millisecond) }
	cases := []struct {
		name    string
		cfg     *common.RetryPolicyConfig
		attempt int
		want    time.Duration
	}{
		{"nil config", nil, 3, 0},
		{"zero delay", &common.RetryPolicyConfig{BackoffFactor: 2}, 3, 0},
		{"negative delay", &common.RetryPolicyConfig{Delay: ms(-10)}, 0, 0},
		{"fixed delay without factor", &common.RetryPolicyConfig{Delay: ms(100)}, 5, 100 * time.Millisecond},
		{"first retry ignores factor", &common.RetryPolicyConfig{Delay: ms(100), BackoffFactor: 2}, 0, 100 * time.Millisecond},
		{"exponential growth", &common.RetryPolicyConfig{Delay: ms(100), BackoffFactor: 2}, 3, 800 * time.Millisecond},
		{"fractional factor", &common.RetryPolicyConfig{Delay: ms(100), BackoffFactor: 1.5}, 2, 225 * time.Millisecond},
		{"capped at max delay", &common.RetryPolicyConfig{Delay: ms(100), BackoffFactor: 2, BackoffMaxDelay: ms(300)}, 3, 300 * time.Millisecond},
		{"below cap untouched", &common.RetryPolicyConfig{Delay: ms(100), BackoffFactor: 2, BackoffMaxDelay: ms(300)}, 1, 200 * time.Millisecond},
		{"cap applies to fixed delay", &common.RetryPolicyConfig{Delay: ms(500), BackoffMaxDelay: ms(50)}, 0, 50 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ComputeBackoff(tc.cfg, tc.attempt))
		})
	}
}

func TestComputeBackoff_JitterIsAdditiveAndBounded(t *testing.T) {
	cfg := &common.RetryPolicyConfig{
		Delay:           common.Duration(100 * time.Millisecond),
		BackoffFactor:   2,
		BackoffMaxDelay: common.Duration(150 * time.Millisecond),
		Jitter:          common.Duration(20 * time.Millisecond),
	}
	seen := map[time.Duration]bool{}
	for range 200 {
		d := ComputeBackoff(cfg, 2)
		// Jitter is added after the cap, in [0, Jitter).
		require.GreaterOrEqual(t, d, 150*time.Millisecond)
		require.Less(t, d, 170*time.Millisecond)
		seen[d] = true
	}
	assert.Greater(t, len(seen), 1, "jitter must vary the delay")
}

func TestSleepCtx(t *testing.T) {
	t.Run("non-positive duration returns immediately even when canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		assert.NoError(t, SleepCtx(ctx, 0))
		assert.NoError(t, SleepCtx(ctx, -time.Second))
	})

	t.Run("elapses", func(t *testing.T) {
		start := time.Now()
		require.NoError(t, SleepCtx(context.Background(), 5*time.Millisecond))
		assert.GreaterOrEqual(t, time.Since(start), 5*time.Millisecond)
	})

	t.Run("canceled context interrupts", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		assert.ErrorIs(t, SleepCtx(ctx, time.Hour), context.Canceled)
	})

	t.Run("deadline interrupts", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		defer cancel()
		assert.ErrorIs(t, SleepCtx(ctx, time.Hour), context.DeadlineExceeded)
	})
}
