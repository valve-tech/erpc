package policy_test

import (
	"testing"
	"time"

	"github.com/erpc/erpc/internal/policy"
	"github.com/stretchr/testify/require"
)

// `stickyPrimary` is the anti-flap step. It holds the primary across
// ticks unless a challenger is decisively better AND the cooldown has
// expired. Both guards matter: during an incident the score gap is huge,
// so hysteresis alone would let a degrading primary flap every tick.
//
// These tests advance the eval clock with `AdvanceEvalNowForTest` rather
// than sleeping, so the 30-second cooldown costs no wall-clock time and
// no timing luck.

// stickyFixture registers two clean upstreams and returns them. The
// register tick elects "aaa" — both score 1, and sortByScore breaks the
// tie alphabetically.
func stickyFixture(t *testing.T) (*engineFixture, *fixtureUpstream, *fixtureUpstream) {
	t.Helper()
	f := newEngineFixture(t)
	a := f.upstream("aaa")
	b := f.upstream("bbb")
	f.register("evm:1", defaultPolicyConfig(), a, b)
	require.Equal(t, "aaa", f.orderIDs("evm:1", "*")[0], "aaa wins the cold-start tiebreak")
	return f, a, b
}

// TestEngine_StickyPrimary_HoldsTheIncumbentInsideTheCooldown — the
// incumbent has just been elected. Even a challenger that is eight times
// better must wait: switching immediately is how a fleet flaps.
func TestEngine_StickyPrimary_HoldsTheIncumbentInsideTheCooldown(t *testing.T) {
	f, a, _ := stickyFixture(t)

	// aaa degrades to a 500 ms p70; bbb stays clean. Well under the
	// 3 s latency-exclusion floor, so this is a ranking change only.
	f.seed(a, seedSpec{requests: 30, latencyMs: 500})
	f.tick("evm:1", "*")

	require.Equal(t, "aaa", f.orderIDs("evm:1", "*")[0],
		"the cooldown has not elapsed, so the incumbent holds")
	require.True(t, f.lastDecision("evm:1", "*").Diff.StickyHeld,
		"the hold must be reported so the sticky-hold metric fires")
	require.False(t, f.lastDecision("evm:1", "*").Diff.PrimaryChanged)
}

// TestEngine_StickyPrimary_SwitchesOnceTheCooldownElapses — the hold is a
// delay, not a lock. Past the cooldown a decisively better challenger
// must take over, or a degraded primary would serve forever.
func TestEngine_StickyPrimary_SwitchesOnceTheCooldownElapses(t *testing.T) {
	f, a, _ := stickyFixture(t)
	f.seed(a, seedSpec{requests: 30, latencyMs: 500})

	f.tick("evm:1", "*")
	require.Equal(t, "aaa", f.orderIDs("evm:1", "*")[0], "held inside the cooldown")

	// Move the eval clock 60 s past the last switch — twice the 30 s
	// minSwitchInterval the default policy asks for.
	policy.AdvanceEvalNowForTest(f.Engine, "evm:1", "*", 60*time.Second)
	f.tick("evm:1", "*")

	require.Equal(t, "bbb", f.orderIDs("evm:1", "*")[0],
		"past the cooldown the better upstream takes the primary")
	require.True(t, f.lastDecision("evm:1", "*").Diff.PrimaryChanged)
	require.False(t, f.lastDecision("evm:1", "*").Diff.StickyHeld,
		"a switch is not a hold")
}

// TestEngine_StickyPrimary_HoldsAMarginallyBetterChallenger — hysteresis
// is the second guard. Past the cooldown, a challenger that is only a
// little better must still lose: swapping the primary for a few percent
// costs more in connection churn than it wins in latency.
func TestEngine_StickyPrimary_HoldsAMarginallyBetterChallenger(t *testing.T) {
	f, a, _ := stickyFixture(t)

	// aaa at a 10 ms p70 scores 1/(1+0.15) ≈ 0.87 against bbb's 1.0 —
	// about 15% better, comfortably inside the 30% hysteresis margin.
	f.seed(a, seedSpec{requests: 30, latencyMs: 10})
	policy.AdvanceEvalNowForTest(f.Engine, "evm:1", "*", 60*time.Second)
	f.tick("evm:1", "*")

	require.Equal(t, "bbb", f.lastDecision("evm:1", "*").Output.Order[1],
		"bbb scores higher, so sticky is what moved it to second")
	require.Equal(t, "aaa", f.orderIDs("evm:1", "*")[0],
		"a marginal challenger does not clear the hysteresis margin")
	require.True(t, f.lastDecision("evm:1", "*").Diff.StickyHeld)
}

// TestEngine_StickyPrimary_ReleasesThePrimaryItCanNoLongerSee — when the
// incumbent is excluded outright, holding it would mean routing to a
// broken upstream. The step must stand aside.
func TestEngine_StickyPrimary_ReleasesThePrimaryItCanNoLongerSee(t *testing.T) {
	f, a, b := stickyFixture(t)

	f.seed(a, seedSpec{failed: 20}) // error rate 1.0 — excluded outright
	f.seed(b, seedSpec{requests: 20, latencyMs: 10})
	f.tick("evm:1", "*")

	require.Equal(t, []string{"bbb"}, f.orderIDs("evm:1", "*"),
		"the excluded incumbent cannot be held")
	require.Equal(t, []string{"aaa"}, f.excludedIDs("evm:1", "*"))
	require.False(t, f.lastDecision("evm:1", "*").Diff.StickyHeld,
		"standing aside is not a hold")
}
