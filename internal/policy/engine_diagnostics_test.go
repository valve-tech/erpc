package policy_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// This file covers the engine's diagnostic surface — the accessors an
// operator reads during an incident: GetScores, RecentDecisions,
// GetExcluded, LastEvalAt and LastSwitchAt. Each needs a ticked engine,
// which is what `engine_fixture_test.go` provides.

// TestEngine_GetScores_ReportsTheRankingTheOrderWasBuiltFrom — an operator
// asking "why is this upstream second?" reads GetScores. The map must be
// the same numbers the order was sorted on, not a second opinion.
func TestEngine_GetScores_ReportsTheRankingTheOrderWasBuiltFrom(t *testing.T) {
	f := newEngineFixture(t)
	fast := f.upstream("aaa-fast")
	slow := f.upstream("bbb-slow")
	f.register("evm:1", defaultPolicyConfig(), fast, slow)

	f.seed(fast, seedSpec{requests: 30, latencyMs: 10})
	f.seed(slow, seedSpec{requests: 30, latencyMs: 800})
	f.tick("evm:1", "*")

	scores := f.Engine.GetScores("evm:1", "*", "*")
	require.Len(t, scores, 2, "both upstreams survived, so both must be scored")
	require.Greater(t, scores["aaa-fast"], scores["bbb-slow"],
		"the faster upstream must score higher")
	require.Equal(t, []string{"aaa-fast", "bbb-slow"}, f.orderIDs("evm:1", "*"),
		"the order must agree with the scores")
}

// TestEngine_GetScores_HandsOutACopy — the returned map must not alias the
// slot's own. An admin endpoint that walks it while a tick runs would
// otherwise race the engine, and a caller that edits it would rewrite the
// engine's ranking.
func TestEngine_GetScores_HandsOutACopy(t *testing.T) {
	f := newEngineFixture(t)
	a := f.upstream("aaa")
	f.register("evm:1", defaultPolicyConfig(), a)
	f.tick("evm:1", "*")

	first := f.Engine.GetScores("evm:1", "*", "*")
	require.Contains(t, first, "aaa")
	first["aaa"] = -999

	second := f.Engine.GetScores("evm:1", "*", "*")
	require.NotEqual(t, -999.0, second["aaa"],
		"the caller must not be able to edit the engine's scores")
}

// TestEngine_GetScores_IsNilWithoutAScoringStep — a policy that never
// calls sortByScore has no ranking to report. Reporting zeroes would read
// as "every upstream scores 0", which is a different and wrong claim.
func TestEngine_GetScores_IsNilWithoutAScoringStep(t *testing.T) {
	f := newEngineFixture(t)
	cfg := defaultPolicyConfig()
	cfg.EvalFunc = "(ups, _ctx) => ups"
	a := f.upstream("aaa")
	b := f.upstream("bbb")
	f.register("evm:1", cfg, a, b)
	f.tick("evm:1", "*")

	require.Len(t, f.orderIDs("evm:1", "*"), 2, "the pass-through policy keeps both")
	require.Nil(t, f.Engine.GetScores("evm:1", "*", "*"),
		"no scoring step ran, so there is no score to report")
}

// TestEngine_RecentDecisions_ReplaysTicksOldestFirst — the replay is how
// an operator answers "what changed at 14:03?". Oldest-first is the
// documented order; reading it backwards inverts every cause and effect.
func TestEngine_RecentDecisions_ReplaysTicksOldestFirst(t *testing.T) {
	f := newEngineFixture(t)
	a := f.upstream("aaa")
	b := f.upstream("bbb")
	f.register("evm:1", defaultPolicyConfig(), a, b)

	// Tick 1 (from register) sees both healthy. Break bbb, then tick again.
	f.seed(b, seedSpec{failed: 20})
	f.tick("evm:1", "*")

	all := f.Engine.RecentDecisions("evm:1", "*", "*", 0)
	require.Len(t, all, 2, "register ticks once, the test ticks once more")
	require.True(t, all[0].TickAt.Before(all[1].TickAt) || all[0].TickAt.Equal(all[1].TickAt),
		"entry 0 must be the older tick")
	require.Empty(t, all[0].Output.Excluded, "the first tick saw a clean fleet")
	require.Len(t, all[1].Output.Excluded, 1, "the second tick saw bbb break")
	require.Equal(t, "bbb", all[1].Output.Excluded[0].ID)
	require.Equal(t, uint64(0), all[0].State.TickCount)
	require.Equal(t, uint64(1), all[1].State.TickCount)
}

// TestEngine_RecentDecisions_LimitReturnsTheNewest — a caller asking for
// the last two ticks wants the last two, not the first two.
func TestEngine_RecentDecisions_LimitReturnsTheNewest(t *testing.T) {
	f := newEngineFixture(t)
	a := f.upstream("aaa")
	f.register("evm:1", defaultPolicyConfig(), a)
	for i := 0; i < 4; i++ {
		f.tick("evm:1", "*")
	}

	all := f.Engine.RecentDecisions("evm:1", "*", "*", 0)
	require.Len(t, all, 5, "one register tick plus four explicit ticks")

	last2 := f.Engine.RecentDecisions("evm:1", "*", "*", 2)
	require.Len(t, last2, 2)
	require.Equal(t, all[3].ID, last2[0].ID, "limit must keep the newest, oldest-first")
	require.Equal(t, all[4].ID, last2[1].ID)
}

// TestEngine_RecentDecisions_RingKeepsTheNewestSixtyFour — the ring is
// what stops an idle slot growing without bound. It must drop the oldest,
// never the newest: a replay that loses the tick you are investigating is
// worse than no replay.
func TestEngine_RecentDecisions_RingKeepsTheNewestSixtyFour(t *testing.T) {
	f := newEngineFixture(t)
	a := f.upstream("aaa")
	f.register("evm:1", defaultPolicyConfig(), a)

	// 1 register tick + 79 = 80 total, comfortably past the 64-entry ring.
	for i := 0; i < 79; i++ {
		f.tick("evm:1", "*")
	}

	all := f.Engine.RecentDecisions("evm:1", "*", "*", 0)
	require.Len(t, all, 64, "the ring caps retention at 64 ticks")
	require.Equal(t, uint64(79), all[63].State.TickCount, "the newest tick must survive")
	require.Equal(t, uint64(16), all[0].State.TickCount, "ticks 0..15 must have aged out")
}

// TestEngine_RecentDecisions_RecordsAFailedTick — when the policy throws,
// the engine keeps serving the previous order. The operator still has to
// be able to see that the tick failed, and why.
func TestEngine_RecentDecisions_RecordsAFailedTick(t *testing.T) {
	f := newEngineFixture(t)
	cfg := defaultPolicyConfig()
	// Succeeds on the register tick, throws on every later one.
	cfg.EvalFunc = "(ups, ctx) => { if (ctx.tickCount > 0) { throw new Error('boom'); } return ups; }"
	a := f.upstream("aaa")
	b := f.upstream("bbb")
	f.register("evm:1", cfg, a, b)
	require.Equal(t, []string{"aaa", "bbb"}, f.orderIDs("evm:1", "*"))

	f.tick("evm:1", "*")

	last := f.lastDecision("evm:1", "*")
	require.Contains(t, last.Error, "boom", "the failed tick must record the throw")
	require.Empty(t, last.Output.Order, "a failed tick produces no order")
	require.Equal(t, []string{"aaa", "bbb"}, f.orderIDs("evm:1", "*"),
		"the engine must keep serving the last good order")
}

// TestEngine_GetExcluded_NamesTheBrokenUpstreamAndTheRuleThatDroppedIt —
// GetExcluded is what the prober reads and what an operator checks first.
// It must name the upstream, and the decision must name the predicate.
func TestEngine_GetExcluded_NamesTheBrokenUpstreamAndTheRuleThatDroppedIt(t *testing.T) {
	f := newEngineFixture(t)
	good := f.upstream("aaa")
	bad := f.upstream("bbb")
	f.register("evm:1", defaultPolicyConfig(), good, bad)

	f.seed(good, seedSpec{requests: 20, latencyMs: 10})
	f.seed(bad, seedSpec{failed: 20})
	f.tick("evm:1", "*")

	require.Equal(t, []string{"aaa"}, f.orderIDs("evm:1", "*"))
	require.Equal(t, []string{"bbb"}, f.excludedIDs("evm:1", "*"))

	last := f.lastDecision("evm:1", "*")
	require.Len(t, last.Output.Excluded, 1)
	ex := last.Output.Excluded[0]
	require.Equal(t, "bbb", ex.ID)
	require.Equal(t, "excludeIf", ex.Step, "the exclude step must be attributed")
	require.Contains(t, ex.LeafReasons, "error_rate_above",
		"the leaf reason must name the predicate that tripped")
	require.True(t, ex.ProbeEligible, "an error-rate exclusion reverses via fresh traffic")
}

// TestEngine_GetExcluded_OmitsAnUpstreamProbingCannotHelp — a cordoned
// upstream is excluded, but shadow traffic will never un-cordon it. The
// prober's candidate list must leave it out; the decision must still show
// it, marked ineligible. Otherwise the prober burns quota on an upstream
// no gate is watching.
func TestEngine_GetExcluded_OmitsAnUpstreamProbingCannotHelp(t *testing.T) {
	f := newEngineFixture(t)
	good := f.upstream("aaa")
	cordoned := f.upstream("bbb")
	f.register("evm:1", defaultPolicyConfig(), good, cordoned)

	f.seed(good, seedSpec{requests: 20, latencyMs: 10})
	f.seed(cordoned, seedSpec{requests: 20, latencyMs: 10})
	f.Tracker.Cordon(cordoned, "*", "consensus sit-out")
	f.tick("evm:1", "*")

	require.Equal(t, []string{"aaa"}, f.orderIDs("evm:1", "*"))
	require.Empty(t, f.excludedIDs("evm:1", "*"),
		"a cordoned upstream is not a probe candidate")

	last := f.lastDecision("evm:1", "*")
	require.Len(t, last.Output.Excluded, 1, "the decision must still report it")
	require.Equal(t, "bbb", last.Output.Excluded[0].ID)
	require.Equal(t, "removeCordoned", last.Output.Excluded[0].Step)
	require.False(t, last.Output.Excluded[0].ProbeEligible)
}

// TestEngine_GetExcluded_FallsBackToTheWildcardBeforeTheNarrowSlotTicks —
// a method-scoped slot is created by the first request for that method
// and has no verdict until its first tick. Until then GetExcluded must
// answer from the network wildcard slot rather than claim "nothing is
// excluded".
func TestEngine_GetExcluded_FallsBackToTheWildcardBeforeTheNarrowSlotTicks(t *testing.T) {
	f := newEngineFixture(t)
	good := f.upstream("aaa")
	bad := f.upstream("bbb")
	f.register("evm:1", defaultPolicyConfig(perMethod), good, bad)

	f.seed(good, seedSpec{requests: 20, latencyMs: 10})
	f.seed(bad, seedSpec{failed: 20})
	f.tick("evm:1", "*") // only the wildcard slot has ticked

	// This call lazy-creates the eth_call slot; the frozen ticker means it
	// never ticks, so its own caches stay empty.
	require.Equal(t, []string{"aaa"}, f.orderIDs("evm:1", "eth_call"),
		"the order falls back to the wildcard slot")
	require.Equal(t, []string{"bbb"}, f.excludedIDs("evm:1", "eth_call"),
		"the excluded set falls back to the wildcard slot too")
}

// TestEngine_LastEvalAt_TracksTheMostRecentTick — "when did the policy
// last run?" is the first question when routing looks stale.
func TestEngine_LastEvalAt_TracksTheMostRecentTick(t *testing.T) {
	f := newEngineFixture(t)
	a := f.upstream("aaa")
	f.register("evm:1", defaultPolicyConfig(), a)

	require.True(t, f.Engine.LastEvalAt("evm:2", "*", "*").IsZero(),
		"an unregistered network has never evaluated")

	first := f.Engine.LastEvalAt("evm:1", "*", "*")
	require.False(t, first.IsZero(), "register runs one synchronous tick")

	time.Sleep(2 * time.Millisecond)
	f.tick("evm:1", "*")
	require.True(t, f.Engine.LastEvalAt("evm:1", "*", "*").After(first),
		"a later tick must move the timestamp forward")
}

// TestEngine_LastSwitchAt_MovesOnlyWhenThePrimaryChanges — the sticky
// cooldown is measured from this timestamp. If a tick that held the
// primary also reset it, the cooldown would never expire.
func TestEngine_LastSwitchAt_MovesOnlyWhenThePrimaryChanges(t *testing.T) {
	f := newEngineFixture(t)
	a := f.upstream("aaa")
	b := f.upstream("bbb")
	f.register("evm:1", defaultPolicyConfig(), a, b)
	require.Equal(t, "aaa", f.orderIDs("evm:1", "*")[0])

	afterFirst := f.Engine.LastSwitchAt("evm:1", "*", "*")
	require.False(t, afterFirst.IsZero(), "electing the first primary is a switch")

	time.Sleep(2 * time.Millisecond)
	f.tick("evm:1", "*")
	require.Equal(t, afterFirst, f.Engine.LastSwitchAt("evm:1", "*", "*"),
		"a tick that keeps the same primary must not move the timestamp")

	// Break aaa outright so it leaves the survivor set; bbb takes over.
	f.seed(a, seedSpec{failed: 20})
	f.seed(b, seedSpec{requests: 20, latencyMs: 10})
	f.tick("evm:1", "*")
	require.Equal(t, "bbb", f.orderIDs("evm:1", "*")[0])
	require.True(t, f.Engine.LastSwitchAt("evm:1", "*", "*").After(afterFirst),
		"a real primary change must move the timestamp")
}

// TestEngine_SetStepLogEnabled_AddsTheChainTrailToTheDecision — the step
// trail is how the simulator and DEBUG logs explain a verdict step by
// step. It is off by default because it allocates per step.
func TestEngine_SetStepLogEnabled_AddsTheChainTrailToTheDecision(t *testing.T) {
	f := newEngineFixture(t)
	good := f.upstream("aaa")
	bad := f.upstream("bbb")
	f.register("evm:1", defaultPolicyConfig(), good, bad)
	f.seed(good, seedSpec{requests: 20, latencyMs: 10})
	f.seed(bad, seedSpec{failed: 20})

	require.False(t, f.Engine.IsStepLogEnabled(), "off by default")
	f.tick("evm:1", "*")
	require.Empty(t, f.lastDecision("evm:1", "*").Output.StepLog,
		"no trail while the toggle is off")

	f.Engine.SetStepLogEnabled(true)
	require.True(t, f.Engine.IsStepLogEnabled())
	f.tick("evm:1", "*")

	steps := f.lastDecision("evm:1", "*").Output.StepLog
	require.NotEmpty(t, steps, "the toggle must produce a trail")
	names := make([]string, 0, len(steps))
	for _, s := range steps {
		names = append(names, s.Step)
	}
	require.Contains(t, names, "removeCordoned")
	require.Contains(t, names, "excludeIf")
	require.Contains(t, names, "sortByScore")
	require.Contains(t, names, "stickyPrimary")
	require.Contains(t, f.Logs.String(), "policy step",
		"the trail must also reach the DEBUG log")
}

// TestEngine_SetPaused_FreezesTheTicker — the simulator's pause button
// must stop the engine re-evaluating, so the verdict an operator is
// reading stays the verdict that was made.
func TestEngine_SetPaused_FreezesTheTicker(t *testing.T) {
	const interval = 150 * time.Millisecond
	f := newEngineFixture(t)
	a := f.upstream("aaa")
	f.register("evm:1", tickingPolicyConfig(interval), a)

	require.False(t, f.Engine.IsPaused())
	baseline := f.Engine.LastEvalAt("evm:1", "*", "*")
	require.True(t, waitUntil(t, 5*time.Second, func() bool {
		return f.Engine.LastEvalAt("evm:1", "*", "*").After(baseline)
	}), "the ticker must run before the test pauses it")

	f.Engine.SetPaused(true)
	require.True(t, f.Engine.IsPaused())
	// Let any in-flight tick land, then take the reading to compare.
	time.Sleep(3 * interval)
	frozen := f.Engine.LastEvalAt("evm:1", "*", "*")
	time.Sleep(5 * interval)
	require.Equal(t, frozen, f.Engine.LastEvalAt("evm:1", "*", "*"),
		"a paused engine must not evaluate again")

	f.Engine.SetPaused(false)
	require.True(t, waitUntil(t, 5*time.Second, func() bool {
		return f.Engine.LastEvalAt("evm:1", "*", "*").After(frozen)
	}), "unpausing must resume the ticker")
}

// waitUntil polls `cond` until it holds or `timeout` elapses. Every wait
// in this package is bounded — a test that waits on a background ticker
// without a bound turns a regression into a hung suite.
func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}
