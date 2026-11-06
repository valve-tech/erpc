package policy_test

import (
	"testing"

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
