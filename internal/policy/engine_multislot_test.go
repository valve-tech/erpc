package policy_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// One engine serves a whole project, so several networks and several
// methods share it. These tests hold each slot to its own verdict.

// TestEngine_TwoNetworksOnOneEngineDecideIndependently — a project runs
// one engine for every network it serves. An upstream breaking on one
// chain must not remove a same-named healthy upstream from another.
func TestEngine_TwoNetworksOnOneEngineDecideIndependently(t *testing.T) {
	f := newEngineFixture(t)

	oneGood := f.upstream("aaa")
	oneBad := f.upstream("bbb")
	f.register("evm:1", defaultPolicyConfig(), oneGood, oneBad)

	twoGood := f.upstream("ccc")
	twoAlsoGood := f.upstream("ddd")
	f.register("evm:2", defaultPolicyConfig(), twoGood, twoAlsoGood)

	f.seed(oneGood, seedSpec{requests: 20, latencyMs: 10})
	f.seed(oneBad, seedSpec{failed: 20})
	f.seed(twoGood, seedSpec{requests: 20, latencyMs: 10})
	f.seed(twoAlsoGood, seedSpec{requests: 20, latencyMs: 10})

	f.tick("evm:1", "*")
	f.tick("evm:2", "*")

	require.Equal(t, []string{"aaa"}, f.orderIDs("evm:1", "*"))
	require.Equal(t, []string{"bbb"}, f.excludedIDs("evm:1", "*"))

	require.Equal(t, []string{"ccc", "ddd"}, f.orderIDs("evm:2", "*"),
		"evm:2 is healthy and must keep both upstreams")
	require.Empty(t, f.excludedIDs("evm:2", "*"))

	require.NotContains(t, f.Engine.GetScores("evm:1", "*", "*"), "ccc",
		"evm:1 must not score an upstream it does not serve")
	require.NotContains(t, f.Engine.GetScores("evm:2", "*", "*"), "aaa")
}

// TestEngine_TwoMethodsOnOneNetworkExcludeIndependently — under
// evalScope=network-method each method gets its own slot. An upstream
// that fails `eth_call` must stay in rotation for `eth_getLogs`, and the
// converse. Sharing one exclusion set across methods would take a whole
// upstream out for one bad method.
func TestEngine_TwoMethodsOnOneNetworkExcludeIndependently(t *testing.T) {
	f := newEngineFixture(t)
	a := f.upstream("aaa")
	b := f.upstream("bbb")
	f.register("evm:1", defaultPolicyConfig(perMethod), a, b)

	// aaa is broken for eth_call only; bbb is broken for eth_getLogs only.
	f.seed(a, seedSpec{method: "eth_call", failed: 20})
	f.seed(b, seedSpec{method: "eth_call", requests: 20, latencyMs: 10})
	f.seed(a, seedSpec{method: "eth_getLogs", requests: 20, latencyMs: 10})
	f.seed(b, seedSpec{method: "eth_getLogs", failed: 20})

	// Reading an unseen method lazy-creates its slot; then tick it.
	_ = f.orderIDs("evm:1", "eth_call")
	_ = f.orderIDs("evm:1", "eth_getLogs")
	f.tick("evm:1", "eth_call")
	f.tick("evm:1", "eth_getLogs")

	require.Equal(t, []string{"bbb"}, f.orderIDs("evm:1", "eth_call"))
	require.Equal(t, []string{"aaa"}, f.excludedIDs("evm:1", "eth_call"))

	require.Equal(t, []string{"aaa"}, f.orderIDs("evm:1", "eth_getLogs"))
	require.Equal(t, []string{"bbb"}, f.excludedIDs("evm:1", "eth_getLogs"))
}

// TestEngine_UnregisterNetwork_StopsOnlyThatNetwork — reconfiguration
// drops one network at a time. The engine must forget it completely and
// leave every other network answering.
func TestEngine_UnregisterNetwork_StopsOnlyThatNetwork(t *testing.T) {
	f := newEngineFixture(t)
	f.register("evm:1", defaultPolicyConfig(), f.upstream("aaa"))
	f.register("evm:2", defaultPolicyConfig(), f.upstream("ccc"))
	require.Equal(t, []string{"aaa"}, f.orderIDs("evm:1", "*"))

	f.Engine.UnregisterNetwork("evm:1")

	require.Empty(t, f.orderIDs("evm:1", "*"), "the network is gone")
	require.Nil(t, f.Engine.RecentDecisions("evm:1", "*", "*", 0),
		"its decision history goes with it")
	require.Equal(t, []string{"ccc"}, f.orderIDs("evm:2", "*"),
		"the other network is untouched")
}

// TestEngine_PerFinalitySlotsRankIndependently — under
// evalScope=network-finality the engine keeps one slot per finality
// bucket, and the tracker starts writing per-finality metrics. An
// upstream that serves finalized reads badly must not be dropped from
// realtime reads, where its numbers are fine.
func TestEngine_PerFinalitySlotsRankIndependently(t *testing.T) {
	f := newEngineFixture(t)
	a := f.upstream("aaa")
	b := f.upstream("bbb")
	f.register("evm:1", defaultPolicyConfig(perFinality), a, b)
	require.True(t, f.Tracker.IsFinalityTracked(),
		"a finality-scoped network must switch the tracker to per-finality writes")

	f.seed(a, seedSpec{finality: "finalized", failed: 20})
	f.seed(b, seedSpec{finality: "finalized", requests: 20, latencyMs: 10})
	f.seed(a, seedSpec{finality: "realtime", requests: 20, latencyMs: 10})
	f.seed(b, seedSpec{finality: "realtime", failed: 20})

	// Reading an unseen finality lazy-creates its slot; then tick it.
	_ = f.orderIDsAt("evm:1", "*", "finalized")
	_ = f.orderIDsAt("evm:1", "*", "realtime")
	f.tickAt("evm:1", "*", "finalized")
	f.tickAt("evm:1", "*", "realtime")

	require.Equal(t, []string{"bbb"}, f.orderIDsAt("evm:1", "*", "finalized"))
	require.Equal(t, []string{"aaa"}, f.excludedIDsAt("evm:1", "*", "finalized"))

	require.Equal(t, []string{"aaa"}, f.orderIDsAt("evm:1", "*", "realtime"))
	require.Equal(t, []string{"bbb"}, f.excludedIDsAt("evm:1", "*", "realtime"))
}
