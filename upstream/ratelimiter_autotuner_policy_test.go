package upstream

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// The auto-tuner moves an upstream's rate-limit budget up when the upstream is
// answering cleanly and down when it starts rejecting. Getting it wrong is
// expensive in both directions: too eager an increase means the provider
// throttles us (and we pay for the 429s), too eager a decrease means we throw
// away paid capacity and queue client requests behind a limit that no longer
// reflects reality.

// tunerWindow is the adjustment period used by these tests. It must be > 0:
// with a zero period the tuner re-evaluates on every single record, so its
// counters reset before they ever reach the 10-sample minimum and the budget
// never moves at all.
const tunerWindow = 40 * time.Millisecond

func newAutoTuneBudget(t *testing.T, id string, rules []*common.RateLimitRuleConfig) *RateLimiterBudget {
	t.Helper()
	logger := zerolog.Nop()
	reg, err := NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{
		Store:   &common.RateLimitStoreConfig{Driver: "memory"},
		Budgets: []*common.RateLimitBudgetConfig{{Id: id, Rules: rules}},
	}, &logger)
	require.NoError(t, err)
	budget, err := reg.GetBudget(id)
	require.NoError(t, err)
	return budget
}

func newAutoTuner(t *testing.T, budget *RateLimiterBudget, minB, maxB int) *RateLimitAutoTuner {
	t.Helper()
	logger := zerolog.Nop()
	return NewRateLimitAutoTuner(&logger, budget, tunerWindow,
		0.1 /*errorRateThreshold*/, 2.0 /*increaseFactor*/, 0.5 /*decreaseFactor*/, minB, maxB)
}

// primeWindow issues the tuner's very FIRST record for a method. The tuner has
// no window open at that point, so it opens one and clears its counters —
// meaning this record is not a sample. Tests call it so the sample arithmetic
// below is exact rather than off by one.
func primeWindow(tuner *RateLimitAutoTuner, method string) {
	tuner.RecordSuccess(method)
}

// closeWindow waits out the adjustment period and records one final SUCCESS.
// That last record is what makes the tuner evaluate the window it has been
// filling, so it also counts as the window's last sample.
func closeWindow(tuner *RateLimitAutoTuner, method string) {
	time.Sleep(tunerWindow + 20*time.Millisecond)
	tuner.RecordSuccess(method)
}

func maxCountOf(t *testing.T, b *RateLimiterBudget, method string) uint32 {
	t.Helper()
	rules, err := b.GetRulesByMethod(method)
	require.NoError(t, err)
	require.Len(t, rules, 1)
	return rules[0].Config.MaxCount
}

func TestAutoTuner_CleanWindowRaisesTheBudget(t *testing.T) {
	// An upstream that answered ten requests without one error has spare
	// capacity. Not raising the limit leaves paid throughput unused.
	budget := newAutoTuneBudget(t, "tune-up", []*common.RateLimitRuleConfig{
		{Method: "*", MaxCount: 100, Period: common.RateLimitPeriodSecond},
	})
	tuner := newAutoTuner(t, budget, 10, 1000)

	primeWindow(tuner, "eth_getBalance")
	for i := 0; i < 9; i++ {
		tuner.RecordSuccess("eth_getBalance")
	}
	closeWindow(tuner, "eth_getBalance") // 10 samples, 0 errors

	require.Equal(t, uint32(200), maxCountOf(t, budget, "eth_getBalance"),
		"a clean window must scale the budget by the increase factor")
}

func TestAutoTuner_ErrorRateAboveThresholdLowersTheBudget(t *testing.T) {
	// Sustained rejections mean we are asking for more than the provider will
	// give. Holding the budget steady just keeps generating 429s.
	budget := newAutoTuneBudget(t, "tune-down", []*common.RateLimitRuleConfig{
		{Method: "*", MaxCount: 100, Period: common.RateLimitPeriodSecond},
	})
	tuner := newAutoTuner(t, budget, 10, 1000)

	primeWindow(tuner, "eth_getBalance")
	for i := 0; i < 8; i++ {
		tuner.RecordError("eth_getBalance")
	}
	tuner.RecordSuccess("eth_getBalance")
	closeWindow(tuner, "eth_getBalance") // 10 samples, 8 errors

	require.Equal(t, uint32(50), maxCountOf(t, budget, "eth_getBalance"),
		"a window above the error threshold must scale the budget down")
}

func TestAutoTuner_ASmallNonZeroErrorRateLeavesTheBudgetAlone(t *testing.T) {
	// Between "perfect" and "too many errors" the tuner must do nothing.
	// Raising the budget on a window that already produced errors would chase
	// the limit straight into the provider's throttle.
	budget := newAutoTuneBudget(t, "tune-hold", []*common.RateLimitRuleConfig{
		{Method: "*", MaxCount: 100, Period: common.RateLimitPeriodSecond},
	})
	tuner := newAutoTuner(t, budget, 10, 1000)

	primeWindow(tuner, "eth_getBalance")
	tuner.RecordError("eth_getBalance")
	for i := 0; i < 18; i++ {
		tuner.RecordSuccess("eth_getBalance")
	}
	closeWindow(tuner, "eth_getBalance") // 20 samples, 1 error = 5%

	require.Equal(t, uint32(100), maxCountOf(t, budget, "eth_getBalance"),
		"a small but non-zero error rate is neither a raise nor a cut signal")
}

func TestAutoTuner_TooFewSamplesNeverMovesTheBudget(t *testing.T) {
	// Nine requests are not evidence. Adjusting on them would make the budget
	// swing wildly on a quiet method.
	budget := newAutoTuneBudget(t, "tune-tiny", []*common.RateLimitRuleConfig{
		{Method: "*", MaxCount: 100, Period: common.RateLimitPeriodSecond},
	})
	tuner := newAutoTuner(t, budget, 10, 1000)

	primeWindow(tuner, "eth_getBalance")
	for i := 0; i < 8; i++ {
		tuner.RecordSuccess("eth_getBalance")
	}
	closeWindow(tuner, "eth_getBalance") // 9 samples

	require.Equal(t, uint32(100), maxCountOf(t, budget, "eth_getBalance"),
		"fewer than 10 samples in the window must not move the budget")
}

func TestAutoTuner_SecondWindowInsideThePeriodDoesNotAdjustAgain(t *testing.T) {
	// Without the cool-down the budget would be recomputed on every request,
	// doubling within a single second.
	budget := newAutoTuneBudget(t, "tune-period", []*common.RateLimitRuleConfig{
		{Method: "*", MaxCount: 100, Period: common.RateLimitPeriodSecond},
	})
	tuner := newAutoTuner(t, budget, 10, 1000)

	primeWindow(tuner, "eth_getBalance")
	for i := 0; i < 9; i++ {
		tuner.RecordSuccess("eth_getBalance")
	}
	closeWindow(tuner, "eth_getBalance")
	require.Equal(t, uint32(200), maxCountOf(t, budget, "eth_getBalance"), "the first window adjusts")

	// Twenty more clean samples, all landing inside the fresh cool-down.
	for i := 0; i < 20; i++ {
		tuner.RecordSuccess("eth_getBalance")
	}
	require.Equal(t, uint32(200), maxCountOf(t, budget, "eth_getBalance"),
		"records inside the adjustment period must not trigger a second adjustment")
}

func TestAutoTuner_ClampsTheIncreaseToTheConfiguredCeiling(t *testing.T) {
	// maxBudget is the operator's contract with their provider. Blowing
	// through it gets the account throttled.
	budget := newAutoTuneBudget(t, "tune-ceiling", []*common.RateLimitRuleConfig{
		{Method: "*", MaxCount: 100, Period: common.RateLimitPeriodSecond},
	})
	tuner := newAutoTuner(t, budget, 10, 150)

	primeWindow(tuner, "eth_getBalance")
	for i := 0; i < 9; i++ {
		tuner.RecordSuccess("eth_getBalance")
	}
	closeWindow(tuner, "eth_getBalance")

	require.Equal(t, uint32(150), maxCountOf(t, budget, "eth_getBalance"),
		"the doubled budget must be clamped to maxBudget, not applied in full")
}

func TestAutoTuner_ClampsTheDecreaseToTheConfiguredFloor(t *testing.T) {
	// minBudget stops a bad minute from starving traffic the provider is
	// still happy to serve.
	budget := newAutoTuneBudget(t, "tune-floor", []*common.RateLimitRuleConfig{
		{Method: "*", MaxCount: 100, Period: common.RateLimitPeriodSecond},
	})
	tuner := newAutoTuner(t, budget, 80, 1000)

	primeWindow(tuner, "eth_getBalance")
	for i := 0; i < 9; i++ {
		tuner.RecordError("eth_getBalance")
	}
	closeWindow(tuner, "eth_getBalance") // 10 samples, 9 errors

	require.Equal(t, uint32(80), maxCountOf(t, budget, "eth_getBalance"),
		"the halved budget must be clamped to minBudget")
}

func TestAutoTuner_OnlyAdjustsTheRulesThatMatchTheMethod(t *testing.T) {
	// Budgets carry per-method rules. A clean window on eth_getBalance must
	// not raise the limit of an unrelated, possibly struggling method.
	budget := newAutoTuneBudget(t, "tune-scoped", []*common.RateLimitRuleConfig{
		{Method: "eth_getBalance", MaxCount: 100, Period: common.RateLimitPeriodSecond},
		{Method: "eth_getLogs", MaxCount: 20, Period: common.RateLimitPeriodSecond},
	})
	tuner := newAutoTuner(t, budget, 1, 10000)

	primeWindow(tuner, "eth_getBalance")
	for i := 0; i < 9; i++ {
		tuner.RecordSuccess("eth_getBalance")
	}
	closeWindow(tuner, "eth_getBalance")

	require.Equal(t, uint32(200), maxCountOf(t, budget, "eth_getBalance"))
	require.Equal(t, uint32(20), maxCountOf(t, budget, "eth_getLogs"),
		"an unrelated method's budget must stay where the operator put it")
}

func TestAdjustBudgetByFactor_SaturatesInsteadOfWrappingTheUint32(t *testing.T) {
	// A float64 above math.MaxUint32 wraps to an arbitrary SMALL number when
	// cast, so an aggressive increase factor would silently collapse the
	// budget to near zero — the exact opposite of what the tuner asked for.
	budget := newAutoTuneBudget(t, "tune-overflow", []*common.RateLimitRuleConfig{
		{Method: "*", MaxCount: 4000000000, Period: common.RateLimitPeriodSecond},
	})
	rules, err := budget.GetRulesByMethod("*")
	require.NoError(t, err)
	require.Len(t, rules, 1)

	prev, next, changed := budget.AdjustBudgetByFactor(rules[0], 1000, 0, 0)
	require.True(t, changed)
	require.Equal(t, uint32(4000000000), prev)
	require.Equal(t, uint32(4294967295), next, "the product must saturate at MaxUint32, never wrap")
}

func TestAdjustBudgetByFactor_NoChangeReportsUnchanged(t *testing.T) {
	// The caller only logs and re-publishes the metric when something moved.
	// A false "changed" would spam the log on every quiet window.
	budget := newAutoTuneBudget(t, "tune-nochange", []*common.RateLimitRuleConfig{
		{Method: "*", MaxCount: 100, Period: common.RateLimitPeriodSecond},
	})
	rules, err := budget.GetRulesByMethod("*")
	require.NoError(t, err)

	prev, next, changed := budget.AdjustBudgetByFactor(rules[0], 1.0, 0, 0)
	require.False(t, changed)
	require.Equal(t, prev, next)

	_, _, changed = budget.AdjustBudgetByFactor(nil, 2.0, 0, 0)
	require.False(t, changed, "a nil rule must be a no-op, not a panic")
}

func TestAdjustBudget_SetsTheNewMaxCountAndIgnoresNoOps(t *testing.T) {
	budget := newAutoTuneBudget(t, "tune-set", []*common.RateLimitRuleConfig{
		{Method: "*", MaxCount: 100, Period: common.RateLimitPeriodSecond},
	})
	rules, err := budget.GetRulesByMethod("*")
	require.NoError(t, err)

	require.NoError(t, budget.AdjustBudget(rules[0], 250))
	require.Equal(t, uint32(250), maxCountOf(t, budget, "eth_getBalance"))

	require.NoError(t, budget.AdjustBudget(rules[0], 250), "re-setting the same value is a no-op")
	require.NoError(t, budget.AdjustBudget(nil, 10), "a nil rule must not panic")
}
