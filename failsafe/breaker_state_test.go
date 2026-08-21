package failsafe

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// These tests pin the breaker's decision rules. Every one of them maps to an
// operator-visible outcome: a breaker that opens too eagerly takes a healthy
// upstream out of rotation, and one that never opens keeps routing traffic
// into a dead endpoint.

func newBreaker(t *testing.T, cfg *common.CircuitBreakerPolicyConfig) *Breaker {
	t.Helper()
	lg := zerolog.Nop()
	return NewBreaker(cfg, &lg)
}

func TestBreaker_NilConfigMeansNoBreakerAndAlwaysPermits(t *testing.T) {
	// An upstream with no circuitBreaker block must never be taken out of
	// rotation. NewBreaker returns nil, and every nil-receiver method has to
	// behave as "no policy" rather than panic on the request hot path.
	b := newBreaker(t, nil)
	require.Nil(t, b)
	require.True(t, b.TryAcquirePermit(), "a nil breaker must permit every call")
	require.Equal(t, StateClosed, b.State(), "a nil breaker reads as closed")
	b.Record(OutcomeFailure) // must not panic
	f, s, e := b.Metrics()
	require.Equal(t, [3]uint64{0, 0, 0}, [3]uint64{f, s, e})
}

func TestBreaker_StateNamesAreStable(t *testing.T) {
	// These strings land in logs and in the breaker-transition metric label.
	// Renaming one silently breaks every dashboard and alert that matches it.
	require.Equal(t, "closed", StateClosed.String())
	require.Equal(t, "open", StateOpen.String())
	require.Equal(t, "half_open", StateHalfOpen.String())
	require.Equal(t, "unknown", State(99).String())
}

func TestErrCircuitOpen_MatchesWithErrorsIs(t *testing.T) {
	// Call sites wrap this sentinel in their own error type. If the sentinel
	// stopped comparing equal, the wrapping layer could no longer tell
	// "breaker refused a permit" from a real upstream failure.
	wrapped := fmt.Errorf("upstream call rejected: %w", ErrCircuitOpen)
	require.True(t, errors.Is(wrapped, ErrCircuitOpen))
	require.Equal(t, "circuit breaker is open", ErrCircuitOpen.Error())
}

func TestBreaker_StaysClosedUntilTheWindowIsFull(t *testing.T) {
	// failureThresholdCapacity is the sample window. Opening before the
	// window fills would let two unlucky failures at process start cordon an
	// upstream that has never been given a fair sample.
	b := newBreaker(t, &common.CircuitBreakerPolicyConfig{
		FailureThresholdCount:    2,
		FailureThresholdCapacity: 5,
		SuccessThresholdCount:    1,
		SuccessThresholdCapacity: 1,
		HalfOpenAfter:            common.Duration(time.Minute),
	})

	b.Record(OutcomeFailure)
	b.Record(OutcomeFailure)
	require.Equal(t, StateClosed, b.State(),
		"2 failures out of 2 samples must not open a breaker whose window is 5")

	b.Record(OutcomeSuccess)
	b.Record(OutcomeSuccess)
	require.Equal(t, StateClosed, b.State())

	// Fifth sample completes the window: 2 failures out of 5 hits the count.
	b.Record(OutcomeSuccess)
	require.Equal(t, StateOpen, b.State(),
		"the breaker must open once the full window carries failureThresholdCount failures")
}

func TestBreaker_HealthyWindowKeepsTheBreakerClosed(t *testing.T) {
	// The complement of the test above: a full window that stays under the
	// failure count must leave the upstream in rotation.
	b := newBreaker(t, &common.CircuitBreakerPolicyConfig{
		FailureThresholdCount:    3,
		FailureThresholdCapacity: 5,
		SuccessThresholdCount:    1,
		SuccessThresholdCapacity: 1,
		HalfOpenAfter:            common.Duration(time.Minute),
	})
	for i := 0; i < 20; i++ {
		b.Record(OutcomeSuccess)
		b.Record(OutcomeSuccess)
		b.Record(OutcomeFailure)
	}
	require.Equal(t, StateClosed, b.State(),
		"a steady 33% failure rate under a 3-of-5 threshold must not open the breaker")
}

func TestBreaker_OldFailuresAgeOutOfTheWindow(t *testing.T) {
	// The window is a ring buffer, not a lifetime tally. Failures that have
	// scrolled out must stop counting — otherwise an upstream that had a bad
	// minute hours ago opens on its next single failure.
	b := newBreaker(t, &common.CircuitBreakerPolicyConfig{
		FailureThresholdCount:    3,
		FailureThresholdCapacity: 3,
		SuccessThresholdCount:    1,
		SuccessThresholdCapacity: 1,
		HalfOpenAfter:            common.Duration(time.Minute),
	})

	b.Record(OutcomeFailure)
	b.Record(OutcomeFailure)
	// Three successes push both failures out of the 3-slot window.
	b.Record(OutcomeSuccess)
	b.Record(OutcomeSuccess)
	b.Record(OutcomeSuccess)
	require.Equal(t, StateClosed, b.State())

	// Two more failures: with the old pair evicted the window now holds
	// success+failure+failure, which is 2 of 3 — still below the threshold.
	b.Record(OutcomeFailure)
	b.Record(OutcomeFailure)
	require.Equal(t, StateClosed, b.State(),
		"evicted failures must not be counted again by the ring buffer")

	// The third consecutive failure fills the window with failures.
	b.Record(OutcomeFailure)
	require.Equal(t, StateOpen, b.State())
}

func TestBreaker_OpenRefusesPermitsUntilTheHalfOpenDelayElapses(t *testing.T) {
	// This is the whole point of the open state: stop sending traffic to a
	// broken upstream for halfOpenAfter, then probe it once.
	b := newBreaker(t, &common.CircuitBreakerPolicyConfig{
		FailureThresholdCount:    1,
		FailureThresholdCapacity: 1,
		SuccessThresholdCount:    1,
		SuccessThresholdCapacity: 1,
		HalfOpenAfter:            common.Duration(60 * time.Millisecond),
	})
	require.True(t, b.TryAcquirePermit())
	b.Record(OutcomeFailure)
	require.Equal(t, StateOpen, b.State())

	require.False(t, b.TryAcquirePermit(), "an open breaker must refuse traffic before halfOpenAfter")
	require.Equal(t, StateOpen, b.State(), "a refused permit must not move the breaker out of open")

	time.Sleep(80 * time.Millisecond)
	require.True(t, b.TryAcquirePermit(), "after halfOpenAfter the breaker must grant a trial permit")
	require.Equal(t, StateHalfOpen, b.State())
}

func TestBreaker_ZeroHalfOpenAfterProbesImmediately(t *testing.T) {
	// halfOpenAfter unset means "no cool-down". Treating the zero value as an
	// infinite wait would wedge the breaker open forever on a default config.
	b := newBreaker(t, &common.CircuitBreakerPolicyConfig{
		FailureThresholdCount:    1,
		FailureThresholdCapacity: 1,
		SuccessThresholdCount:    1,
		SuccessThresholdCapacity: 1,
	})
	require.True(t, b.TryAcquirePermit())
	b.Record(OutcomeFailure)
	require.Equal(t, StateOpen, b.State())
	require.True(t, b.TryAcquirePermit(), "a zero halfOpenAfter must permit an immediate trial")
	require.Equal(t, StateHalfOpen, b.State())
}

func TestBreaker_HalfOpenCapsConcurrentTrialPermits(t *testing.T) {
	// The half-open state exists to probe a recovering upstream with a few
	// requests, not to reopen the floodgates. successThresholdCapacity is
	// that cap; losing it would dump full production load onto an upstream
	// that has answered exactly zero requests since it broke.
	b := newBreaker(t, &common.CircuitBreakerPolicyConfig{
		FailureThresholdCount:    1,
		FailureThresholdCapacity: 1,
		SuccessThresholdCount:    2,
		SuccessThresholdCapacity: 2,
		HalfOpenAfter:            common.Duration(time.Millisecond),
	})
	require.True(t, b.TryAcquirePermit())
	b.Record(OutcomeFailure)
	time.Sleep(5 * time.Millisecond)

	require.True(t, b.TryAcquirePermit(), "first trial permit granted")
	require.Equal(t, StateHalfOpen, b.State())
	require.True(t, b.TryAcquirePermit(), "second trial permit granted (capacity 2)")
	require.False(t, b.TryAcquirePermit(), "third concurrent trial must be refused")
}

func TestBreaker_HalfOpenSuccessesCloseTheBreakerAndResetTheWindow(t *testing.T) {
	// Recovery path. After the breaker closes, the pre-open failures must be
	// gone from the sample window — otherwise the very next failure re-opens
	// the breaker and the upstream flaps in and out of rotation. The window is
	// cleared at the moment the breaker OPENS (checkOpenLocked), so that is
	// the line this assertion actually holds down.
	b := newBreaker(t, &common.CircuitBreakerPolicyConfig{
		FailureThresholdCount:    2,
		FailureThresholdCapacity: 2,
		SuccessThresholdCount:    2,
		SuccessThresholdCapacity: 2,
		HalfOpenAfter:            common.Duration(time.Millisecond),
	})
	b.Record(OutcomeFailure)
	b.Record(OutcomeFailure)
	require.Equal(t, StateOpen, b.State())

	time.Sleep(5 * time.Millisecond)
	require.True(t, b.TryAcquirePermit())
	b.Record(OutcomeSuccess)
	require.Equal(t, StateHalfOpen, b.State(), "one success is not the configured threshold of two")
	require.True(t, b.TryAcquirePermit())
	b.Record(OutcomeSuccess)
	require.Equal(t, StateClosed, b.State())

	// A single fresh failure must not re-open: the window restarted empty.
	b.Record(OutcomeFailure)
	require.Equal(t, StateClosed, b.State(),
		"closing must reset the failure window, otherwise the upstream flaps")
}

func TestBreaker_HalfOpenFailureReopensImmediately(t *testing.T) {
	// A failed trial means the upstream is still broken. Waiting for the full
	// trial capacity before re-opening would push more doomed requests at it.
	b := newBreaker(t, &common.CircuitBreakerPolicyConfig{
		FailureThresholdCount:    1,
		FailureThresholdCapacity: 1,
		SuccessThresholdCount:    3,
		SuccessThresholdCapacity: 3,
		HalfOpenAfter:            common.Duration(30 * time.Millisecond),
	})
	b.Record(OutcomeFailure)
	require.Equal(t, StateOpen, b.State())
	time.Sleep(40 * time.Millisecond)

	require.True(t, b.TryAcquirePermit())
	b.Record(OutcomeFailure)
	require.Equal(t, StateOpen, b.State(),
		"a single failed trial must re-open the breaker without waiting for the rest of the trial budget")
	require.False(t, b.TryAcquirePermit(), "and the cool-down must restart from the re-open")
}

func TestBreaker_OnTransitionReportsEveryStateChangeOnce(t *testing.T) {
	// The transition hook is how the breaker's state reaches metrics and
	// traces. A missing or duplicated edge makes the "upstream cordoned"
	// dashboard disagree with what the router is actually doing.
	type edge struct{ from, to, reason string }
	var mu sync.Mutex
	var seen []edge
	done := make(chan struct{}, 8)

	b := newBreaker(t, &common.CircuitBreakerPolicyConfig{
		FailureThresholdCount:    1,
		FailureThresholdCapacity: 1,
		SuccessThresholdCount:    1,
		SuccessThresholdCapacity: 1,
		HalfOpenAfter:            common.Duration(time.Millisecond),
	})
	b.OnTransition = func(from, to State, reason string) {
		mu.Lock()
		seen = append(seen, edge{from.String(), to.String(), reason})
		mu.Unlock()
		done <- struct{}{}
	}

	b.Record(OutcomeFailure) // closed -> open
	time.Sleep(5 * time.Millisecond)
	require.True(t, b.TryAcquirePermit()) // open -> half_open
	b.Record(OutcomeSuccess)              // half_open -> closed

	// The hook runs on its own goroutine so it cannot recurse into the
	// breaker; wait for all three rather than sleeping and hoping.
	for i := 0; i < 3; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of 3 transition callbacks fired", i)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	// Each hook call runs on its own goroutine, so the ORDER the callbacks
	// arrive in is not guaranteed — only the set of edges is. A consumer that
	// rebuilds the breaker's state by replaying these callbacks in arrival
	// order can therefore read the wrong final state; consumers must treat
	// each callback as an independent event.
	require.ElementsMatch(t, []edge{
		{"closed", "open", "failure_threshold"},
		{"open", "half_open", "half_open_delay_elapsed"},
		{"half_open", "closed", "half_open_success_threshold"},
	}, seen)
}

func TestBreaker_LateFailuresWhileOpenDoNotExtendTheCoolDown(t *testing.T) {
	// Requests that were already in flight when the breaker opened keep
	// reporting failures after it opened. Those late reports must not restart
	// the halfOpenAfter clock: on a busy upstream a steady trickle of them
	// would push the recovery probe out forever and the breaker would never
	// re-test the endpoint. They must also not re-emit the closed->open edge,
	// which would make one outage look like a flapping upstream.
	var mu sync.Mutex
	var fired int
	// The hook runs on its own goroutine, so wait on a channel rather than
	// reading a counter that may not have been written yet.
	edges := make(chan struct{}, 8)
	b := newBreaker(t, &common.CircuitBreakerPolicyConfig{
		FailureThresholdCount:    1,
		FailureThresholdCapacity: 1,
		SuccessThresholdCount:    1,
		SuccessThresholdCapacity: 1,
		HalfOpenAfter:            common.Duration(60 * time.Millisecond),
	})
	b.OnTransition = func(from, to State, reason string) {
		mu.Lock()
		fired++
		mu.Unlock()
		edges <- struct{}{}
	}
	b.Record(OutcomeFailure)
	require.Equal(t, StateOpen, b.State())

	// Keep reporting late failures across the whole cool-down window.
	deadline := time.Now().Add(90 * time.Millisecond)
	for time.Now().Before(deadline) {
		b.Record(OutcomeFailure)
		time.Sleep(5 * time.Millisecond)
	}

	require.True(t, b.TryAcquirePermit(),
		"the cool-down must expire on schedule regardless of late failure reports")
	require.Equal(t, StateHalfOpen, b.State())

	for i := 0; i < 2; i++ {
		select {
		case <-edges:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of 2 transition callbacks fired", i)
		}
	}
	// Drain briefly to catch any surplus edge the breaker should not emit.
	select {
	case <-edges:
		t.Fatal("a third transition edge fired: late failures re-opened an already-open breaker")
	case <-time.After(100 * time.Millisecond):
	}
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 2, fired, "exactly two edges: closed->open and open->half_open")
}

func TestBreaker_MetricsCountRealOutcomesAndSkipIgnored(t *testing.T) {
	// Metrics() feeds the operator's view of an upstream's health. Counting
	// ignorable events (cancellations, hedges) as executions would make a
	// healthy upstream look like it is failing a share of its traffic.
	b := newBreaker(t, &common.CircuitBreakerPolicyConfig{
		FailureThresholdCount:    10,
		FailureThresholdCapacity: 10,
		SuccessThresholdCount:    1,
		SuccessThresholdCapacity: 1,
		HalfOpenAfter:            common.Duration(time.Minute),
	})
	b.Record(OutcomeSuccess)
	b.Record(OutcomeSuccess)
	b.Record(OutcomeFailure)
	for i := 0; i < 5; i++ {
		b.Record(OutcomeIgnore)
	}

	failures, successes, executions := b.Metrics()
	require.Equal(t, uint64(1), failures)
	require.Equal(t, uint64(2), successes)
	require.Equal(t, uint64(3), executions, "ignored outcomes must not count as executions")
}

func TestBreaker_CapacityFallsBackToCountWhenUnset(t *testing.T) {
	// Many configs set only failureThresholdCount. If capacity defaulted to
	// zero, checkOpenLocked would bail out and the breaker would never open —
	// a silently disabled safety net.
	b := newBreaker(t, &common.CircuitBreakerPolicyConfig{
		FailureThresholdCount: 3,
		SuccessThresholdCount: 1,
		HalfOpenAfter:         common.Duration(time.Minute),
	})
	require.Equal(t, 3, len(b.results), "the ring buffer sizes itself from the count when capacity is unset")
	b.Record(OutcomeFailure)
	b.Record(OutcomeFailure)
	require.Equal(t, StateClosed, b.State())
	b.Record(OutcomeFailure)
	require.Equal(t, StateOpen, b.State(), "count-only config must still be able to open the breaker")
}

func TestBreaker_RecordWhileOpenStillCountsTowardsMetrics(t *testing.T) {
	// A caller that bypassed TryAcquirePermit (an in-flight request that
	// started before the breaker opened) still reports its outcome. Losing it
	// would understate the true request volume against a broken upstream.
	b := newBreaker(t, &common.CircuitBreakerPolicyConfig{
		FailureThresholdCount:    1,
		FailureThresholdCapacity: 1,
		SuccessThresholdCount:    1,
		SuccessThresholdCapacity: 1,
		HalfOpenAfter:            common.Duration(time.Hour),
	})
	b.Record(OutcomeFailure)
	require.Equal(t, StateOpen, b.State())

	b.Record(OutcomeSuccess)
	b.Record(OutcomeFailure)
	failures, successes, executions := b.Metrics()
	require.Equal(t, uint64(2), failures)
	require.Equal(t, uint64(1), successes)
	require.Equal(t, uint64(3), executions)
	require.Equal(t, StateOpen, b.State(),
		"a late success recorded while open must not close the breaker behind the cool-down's back")
}
