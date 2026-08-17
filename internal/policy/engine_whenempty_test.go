package policy_test

import (
	"testing"
	"time"

	"github.com/erpc/erpc/internal/policy"
	"github.com/stretchr/testify/require"
)

// `whenEmpty(() => upstreams)` is the default policy's outage safety net.
// When every health rule drops every upstream, failing closed would turn
// a degraded fleet into a total outage. The chain restores the raw set
// instead and lets retry, hedge and consensus do what they can.

// TestEngine_WhenEveryUpstreamFailsTheHealthRules_TheRawSetComesBack —
// serving a degraded upstream beats serving nothing.
func TestEngine_WhenEveryUpstreamFailsTheHealthRules_TheRawSetComesBack(t *testing.T) {
	f := newEngineFixture(t)
	a := f.upstream("aaa")
	b := f.upstream("bbb")
	f.register("evm:1", defaultPolicyConfig(), a, b)

	// Error rate 1.0 on both, over the samplesAbove(10) gate.
	f.seed(a, seedSpec{failed: 20})
	f.seed(b, seedSpec{failed: 20})
	f.tick("evm:1", "*")

	require.Equal(t, []string{"aaa", "bbb"}, f.orderIDs("evm:1", "*"),
		"a total outage must restore every upstream, not fail closed")
	require.Empty(t, f.excludedIDs("evm:1", "*"),
		"the restored set excludes nobody")
	require.Empty(t, f.lastDecision("evm:1", "*").Output.Excluded)
}

// TestEngine_WhenEmptyRunsAfterTheTierSplitSoTheFallbackTierSurvives —
// the ordering of `whenEmpty` against `preferTag` is load-bearing. Run
// `whenEmpty` first and it restores every upstream; `preferTag` then sees
// healthy primaries again and discards the whole fallback tier — at the
// exact moment the fallback tier is the only thing left to try.
//
// Running it after the tier split keeps the fallback tier in the list,
// ranked last by `demoteTag`.
func TestEngine_WhenEmptyRunsAfterTheTierSplitSoTheFallbackTierSurvives(t *testing.T) {
	f := newEngineFixture(t)
	primaryA := f.upstream("aaa")
	primaryB := f.upstream("bbb")
	fallback := f.upstream("ccc", "tier:fallback")
	f.register("evm:1", defaultPolicyConfig(), primaryA, primaryB, fallback)

	// Every tier fails the health rules at once.
	f.seed(primaryA, seedSpec{failed: 20})
	f.seed(primaryB, seedSpec{failed: 20})
	f.seed(fallback, seedSpec{failed: 20})
	f.tick("evm:1", "*")

	order := f.orderIDs("evm:1", "*")
	require.Contains(t, order, "ccc",
		"the fallback tier must survive a total outage")
	require.Equal(t, []string{"aaa", "bbb", "ccc"}, order,
		"the fallback tier stays, ranked behind every primary")
}

// TestEngine_HealthyPrimaryStillHidesTheFallbackTier — the safety net
// must not leak. With the fallback escape off, one healthy primary keeps
// the fallback tier out of the tick entirely; that is what makes a
// fallback upstream a fallback rather than a cheaper peer.
func TestEngine_HealthyPrimaryStillHidesTheFallbackTier(t *testing.T) {
	f := newEngineFixture(t)
	primary := f.upstream("aaa")
	fallback := f.upstream("ccc", "tier:fallback")
	f.register("evm:1", defaultPolicyConfig(), primary, fallback)

	f.seed(primary, seedSpec{requests: 20, latencyMs: 10})
	f.seed(fallback, seedSpec{requests: 20, latencyMs: 10})
	f.tick("evm:1", "*")

	require.Equal(t, []string{"aaa"}, f.orderIDs("evm:1", "*"),
		"a healthy primary removes the fallback tier from the tick")
}

// TestEngine_FailoverEscapeKeepsTheFallbackTierRankedLast — with
// `failover.onDefaultsExhausted` on, `preferTag` keeps the loser tier in
// the list so a request that exhausts the primaries escalates inside the
// same request. `demoteTag` must still rank it behind every primary.
func TestEngine_FailoverEscapeKeepsTheFallbackTierRankedLast(t *testing.T) {
	f := newEngineFixture(t)
	cfg := defaultPolicyConfig()
	cfg.FailoverOnDefaultsExhausted = true
	slowPrimary := f.upstream("aaa")
	fastFallback := f.upstream("ccc", "tier:fallback")
	f.register("evm:1", cfg, slowPrimary, fastFallback)

	// The fallback is the faster of the two, so score alone would put it
	// first. Tier order has to win.
	f.seed(slowPrimary, seedSpec{requests: 30, latencyMs: 500})
	f.seed(fastFallback, seedSpec{requests: 30, latencyMs: 10})
	// Push the eval clock past the sticky cooldown. Without this the
	// incumbent primary is held anyway, and the test would pass even if
	// `demoteTag` did nothing at all.
	policy.AdvanceEvalNowForTest(f.Engine, "evm:1", "*", 60*time.Second)
	f.tick("evm:1", "*")

	require.Equal(t, []string{"aaa", "ccc"}, f.orderIDs("evm:1", "*"),
		"the escape keeps the fallback reachable, ranked behind the primary")
}

// TestEngine_TierOrderHasTheLastWordAfterStickyHoistsAFallback — the
// chain calls `demoteTag('tier:fallback')` twice, and the second call is
// the one that survives `stickyPrimary`.
//
// The engine boots while every primary is broken, so the only survivor is
// the fallback and the shared primary register latches onto it. When the
// primaries heal, `stickyPrimary` hoists that fallback back to the head
// of the list — a fallback serving every request while healthy primaries
// sit behind it. The trailing `demoteTag` puts the tiers back in order.
func TestEngine_TierOrderHasTheLastWordAfterStickyHoistsAFallback(t *testing.T) {
	f := newEngineFixture(t)
	primaryA := f.upstreamOn("evm:1", "aaa")
	primaryB := f.upstreamOn("evm:1", "bbb")
	fallback := f.upstreamOn("evm:1", "ccc", "tier:fallback")

	// Both primaries are already broken when the engine boots.
	f.seed(primaryA, seedSpec{failed: 20})
	f.seed(primaryB, seedSpec{failed: 20})
	f.seed(fallback, seedSpec{requests: 20, latencyMs: 10})

	cfg := defaultPolicyConfig()
	cfg.FailoverOnDefaultsExhausted = true
	f.register("evm:1", cfg, primaryA, primaryB, fallback)
	require.Equal(t, []string{"ccc"}, f.orderIDs("evm:1", "*"),
		"only the fallback survives the boot tick, so it becomes the primary")

	// The primaries recover: plenty of fresh successes, though still
	// slower than the fallback.
	f.seed(primaryA, seedSpec{requests: 400, latencyMs: 200})
	f.seed(primaryB, seedSpec{requests: 400, latencyMs: 200})
	f.tick("evm:1", "*")

	require.True(t, f.lastDecision("evm:1", "*").Diff.StickyHeld,
		"sticky must be actively hoisting the fallback — that is the case being corrected")
	require.Equal(t, []string{"aaa", "bbb", "ccc"}, f.orderIDs("evm:1", "*"),
		"tier order has the last word: the fallback ranks behind both primaries")
}
