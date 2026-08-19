package stdlib_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	"github.com/stretchr/testify/require"
)

// `minSwitchInterval` is the only duration an operator hands the std-lib, and
// the Go side parses it. Every spelling ends up as one number of milliseconds,
// and that number decides one thing: whether a degraded primary keeps the
// traffic for another cooldown window, or the challenger takes it over on the
// very next tick. These tests drive the whole engine and read the routing
// outcome, because the parse result is only interesting through the switch.

// stickyCooldownFixture registers two upstreams under an eval that sorts by
// score and then applies stickyPrimary with the caller-supplied
// `minSwitchInterval` literal. It returns the engine after the first tick has
// elected `aaa`, plus the tracker so the caller can degrade `aaa`.
func stickyCooldownFixture(t *testing.T, minSwitchIntervalJS string) (*policy.Engine, *health.Tracker, []common.Upstream, func()) {
	t.Helper()
	eval := fmt.Sprintf(`(upstreams, ctx) => upstreams
		.sortByScore(PREFER_FASTEST)
		.stickyPrimary({ hysteresis: 0.10, minSwitchInterval: %s })`, minSwitchIntervalJS)

	engine, _, tracker, cancel := newTestEngine(t, eval)
	ups := mkUps("aaa", "bbb")
	cfg := &common.SelectionPolicyConfig{
		EvalInterval: common.Duration(0),
		EvalTimeout:  common.Duration(500 * time.Millisecond),
		EvalFunc:     eval,
	}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	// `aaa` is clean and fast, `bbb` is clean but slow. The first tick elects
	// `aaa` and seeds the sticky register with that moment.
	for i := 0; i < 100; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[0], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
		tracker.RecordUpstreamRequest(ups[1], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[1], "*", 200*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}
	policy.TickForTest(engine, "evm:1", "*")
	require.Equal(t, "aaa", engine.GetOrdered("evm:1", "*", "*")[0].Id(),
		"the fast upstream must win the first tick, or the cooldown test has no incumbent")

	return engine, tracker, ups, func() {
		engine.Stop()
		cancel()
	}
}

// degradeIncumbent flips the health picture: `aaa` slows to two seconds while
// `bbb` speeds up to five milliseconds. That leaves `bbb` roughly 28 times the
// score of `aaa` — far past the 10% hysteresis margin, so only the cooldown can
// still hold the traffic on `aaa`.
func degradeIncumbent(tracker *health.Tracker, ups []common.Upstream) {
	for i := 0; i < 300; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[0], "*", 2000*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
		tracker.RecordUpstreamRequest(ups[1], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[1], "*", 5*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}
}

// TestStickyPrimary_MinSwitchInterval_DecidesTheHandover walks every spelling an
// operator can write. A spelling the Go parser understands buys the incumbent a
// cooldown; a spelling it does not understand becomes 0 ms, and the challenger
// takes the traffic on the next tick.
func TestStickyPrimary_MinSwitchInterval_DecidesTheHandover(t *testing.T) {
	for _, tc := range []struct {
		name    string
		js      string
		primary string
		why     string
	}{
		{
			name: "DurationString", js: `'30s'`, primary: "aaa",
			why: "30s parses to 30000 ms, so the cooldown holds the degraded incumbent",
		},
		{
			name: "MillisecondString", js: `'30000ms'`, primary: "aaa",
			why: "the ms suffix parses the same as the seconds form",
		},
		{
			name: "WholeNumber", js: `30000`, primary: "aaa",
			why: "a bare number is already milliseconds and must hold just like '30s'",
		},
		{
			name: "FractionalNumber", js: `30000.5`, primary: "aaa",
			why: "a fractional millisecond count truncates, it does not degrade to no cooldown",
		},
		{
			name: "EmptyString", js: `''`, primary: "bbb",
			why: "an empty duration is no cooldown, so the challenger takes over at once",
		},
		{
			name: "UnparseableString", js: `'banana'`, primary: "bbb",
			why: "a typo becomes 0 ms — the cooldown silently disappears",
		},
		{
			name: "WrongType", js: `true`, primary: "bbb",
			why: "a value that is neither a string nor a number becomes 0 ms",
		},
		{
			name: "AbsentKnobReadThroughDurationMs", js: `durationMs(ctx.noSuchKnob)`, primary: "bbb",
			why: "reading a knob the context does not carry yields 0 ms rather than throwing",
		},
		{
			name: "ZeroMilliseconds", js: `0`, primary: "bbb",
			why: "an explicit zero cooldown lets every tick reconsider the primary",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, tracker, ups, stop := stickyCooldownFixture(t, tc.js)
			defer stop()

			degradeIncumbent(tracker, ups)
			policy.TickForTest(engine, "evm:1", "*")

			got := engine.GetOrdered("evm:1", "*", "*")
			require.NotEmpty(t, got)
			require.Equal(t, tc.primary, got[0].Id(), tc.why)
		})
	}
}

// TestStickyPrimary_MinSwitchInterval_HoldIsADelayNotALock pins the other half
// of the contract: a parsed cooldown delays the handover, it does not cancel it.
// Once the eval clock passes the window the challenger takes the traffic.
func TestStickyPrimary_MinSwitchInterval_HoldIsADelayNotALock(t *testing.T) {
	engine, tracker, ups, stop := stickyCooldownFixture(t, `'30s'`)
	defer stop()

	degradeIncumbent(tracker, ups)
	policy.TickForTest(engine, "evm:1", "*")
	require.Equal(t, "aaa", engine.GetOrdered("evm:1", "*", "*")[0].Id(),
		"inside the window the degraded incumbent keeps the traffic")

	policy.AdvanceEvalNowForTest(engine, "evm:1", "*", 60*time.Second)
	policy.TickForTest(engine, "evm:1", "*")
	require.Equal(t, "bbb", engine.GetOrdered("evm:1", "*", "*")[0].Id(),
		"past the window the healthy challenger must take over")
}
