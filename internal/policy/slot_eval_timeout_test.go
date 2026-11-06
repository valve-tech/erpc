package policy_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// These tests pin upstream #1284: sobek cannot interrupt a running eval, so
// evalTimeout cannot cut one short. An eval past the timeout is reported
// (a Warn log and selection_eval_errors_total{kind="timeout"}) and its
// result still replaces the older cache. Every eval below busy-waits in JS,
// because that is what a real overrunning policy does.

// slowEval returns an eval source that busy-waits for `busy` and then
// returns every upstream unchanged.
func slowEval(busy time.Duration) string {
	return fmt.Sprintf(
		"(ups, _ctx) => { const t = Date.now(); while (Date.now() - t < %d) {} return ups; }",
		busy.Milliseconds(),
	)
}

// frozenPolicy builds a policy config whose ticker never starts, so each
// test drives every tick itself and no background eval can move the
// verdict under an assertion.
func frozenPolicy(evalFunc string, timeout time.Duration) *common.SelectionPolicyConfig {
	return &common.SelectionPolicyConfig{
		DisableTickerForTest: true,
		EvalTimeout:          common.Duration(timeout),
		EvalFunc:             evalFunc,
	}
}

// TestSlot_EvalTimeout_IsReportedNotFailed: an eval 15x over its timeout
// still yields a decision with no error, and the overrun is logged.
func TestSlot_EvalTimeout_IsReportedNotFailed(t *testing.T) {
	f := newEngineFixture(t)
	f.register("evm:1", frozenPolicy(slowEval(300*time.Millisecond), 20*time.Millisecond),
		f.upstream("rpc1"), f.upstream("rpc2"))

	require.Empty(t, f.lastDecision("evm:1", "*").Error, "a late eval is applied, not failed")
	require.Contains(t, f.Logs.String(), "selection policy eval exceeded evalTimeout")
}

// TestSlot_EvalTimeout_PublishesTheLateOrder: a tick that overruns still
// replaces the previous cache with its result. The eval is fast on its first
// call and slow on every call after, so one slot shows both.
func TestSlot_EvalTimeout_PublishesTheLateOrder(t *testing.T) {
	f := newEngineFixture(t)
	evalFunc := `(ups, _ctx) => {
		globalThis.__tick = (globalThis.__tick || 0) + 1;
		if (globalThis.__tick === 1) { return ups.filter((u) => u.id === 'rpc1'); }
		const t = Date.now();
		while (Date.now() - t < 900) {}
		return ups;
	}`
	f.register("evm:1", frozenPolicy(evalFunc, 400*time.Millisecond),
		f.upstream("rpc1"), f.upstream("rpc2"))
	require.Equal(t, []string{"rpc1"}, f.orderIDs("evm:1", "*"),
		"first tick is fast and publishes its verdict")

	f.tick("evm:1", "*")

	require.Empty(t, f.lastDecision("evm:1", "*").Error)
	require.Equal(t, []string{"rpc1", "rpc2"}, f.orderIDs("evm:1", "*"),
		"an overrunning tick publishes its late result")
}

// TestSlot_EvalTimeout_CountsAndLogs pins the operator surfaces: each
// overrunning tick increments selection_eval_errors_total{kind="timeout"}
// once and writes the overrun warning.
func TestSlot_EvalTimeout_CountsAndLogs(t *testing.T) {
	f := newEngineFixture(t)
	counter := telemetry.MetricSelectionEvalErrorsTotal.WithLabelValues("p1", "evm:1", "*", "timeout")
	before := promUtil.ToFloat64(counter)

	f.register("evm:1", frozenPolicy(slowEval(300*time.Millisecond), 20*time.Millisecond),
		f.upstream("rpc1"))

	require.Equal(t, 1.0, promUtil.ToFloat64(counter)-before,
		`one overrunning tick must increment selection_eval_errors_total{kind="timeout"} exactly once`)
	require.Contains(t, f.Logs.String(), "selection policy eval exceeded evalTimeout")

	f.tick("evm:1", "*")
	require.Equal(t, 2.0, promUtil.ToFloat64(counter)-before,
		"a later overrunning tick counts once more")
}

// TestSlot_EvalTimeout_FirstTickPublishesItsLateResult pins the cold start:
// the first eval runs inside RegisterNetwork, where a busy host can miss a
// short deadline. Its late result is the policy's real answer and is
// published, so the network never sits on raw registration order.
func TestSlot_EvalTimeout_FirstTickPublishesItsLateResult(t *testing.T) {
	f := newEngineFixture(t)
	evalFunc := `(ups, _ctx) => {
		const t = Date.now();
		while (Date.now() - t < 300) {}
		return ups.filter((u) => u.id === 'rpc2');
	}`
	f.register("evm:1", frozenPolicy(evalFunc, 20*time.Millisecond),
		f.upstream("rpc1"), f.upstream("rpc2"))

	require.Equal(t, []string{"rpc2"}, f.orderIDs("evm:1", "*"),
		"the late first result is published; the slot must not stay empty")
	require.Empty(t, f.lastDecision("evm:1", "*").Error)
}

// TestSlot_EvalTimeout_FirstTickLateThrowPublishesNothing: only a valid
// result is published. A first eval that overruns and then throws leaves the
// slot empty, and the decision carries the throw.
func TestSlot_EvalTimeout_FirstTickLateThrowPublishesNothing(t *testing.T) {
	f := newEngineFixture(t)
	evalFunc := `(ups, _ctx) => {
		const t = Date.now();
		while (Date.now() - t < 300) {}
		throw new Error('boom');
	}`
	f.register("evm:1", frozenPolicy(evalFunc, 20*time.Millisecond),
		f.upstream("rpc1"), f.upstream("rpc2"))

	require.Empty(t, f.orderIDs("evm:1", "*"), "a late throw publishes no ordering")
	require.Contains(t, f.lastDecision("evm:1", "*").Error, "boom")
}

// TestSlot_EvalWithinTimeout_PublishesItsResult is the control. An eval
// that finishes inside its timeout reports no error and its order reaches
// the cache — the fix must not turn every tick into a timeout.
func TestSlot_EvalWithinTimeout_PublishesItsResult(t *testing.T) {
	f := newEngineFixture(t)
	f.register("evm:1", frozenPolicy("(ups, _ctx) => ups", 5*time.Second),
		f.upstream("rpc1"), f.upstream("rpc2"))

	require.Empty(t, f.lastDecision("evm:1", "*").Error)
	require.Equal(t, []string{"rpc1", "rpc2"}, f.orderIDs("evm:1", "*"))
}

// TestSlot_UserThrowAboutATimeout_IsNotAPolicyTimeout pins the error
// classification. `emitMetrics` reads a rendered string, so it must match
// this package's own timeout text and not the bare words "timed out" —
// otherwise a user eval that throws about ITS timeout inflates the
// policy-timeout counter and hides the real signal.
func TestSlot_UserThrowAboutATimeout_IsNotAPolicyTimeout(t *testing.T) {
	f := newEngineFixture(t)
	timeoutKind := telemetry.MetricSelectionEvalErrorsTotal.WithLabelValues("p1", "evm:1", "*", "timeout")
	throwKind := telemetry.MetricSelectionEvalErrorsTotal.WithLabelValues("p1", "evm:1", "*", "throw")
	beforeTimeout := promUtil.ToFloat64(timeoutKind)
	beforeThrow := promUtil.ToFloat64(throwKind)

	f.register("evm:1", frozenPolicy(
		`(ups, _ctx) => { throw new Error('upstream health probe timed out'); }`,
		5*time.Second), f.upstream("rpc1"))

	require.Contains(t, f.lastDecision("evm:1", "*").Error, "timed out")
	require.Equal(t, 1.0, promUtil.ToFloat64(throwKind)-beforeThrow,
		"a user throw is a throw, whatever words it uses")
	require.Equal(t, 0.0, promUtil.ToFloat64(timeoutKind)-beforeTimeout,
		"a user throw must not increment the policy-timeout counter")
}

// TestSlot_EvalTimeout_HasNoRaceOnTheOutcome drives many overrunning ticks
// so the race detector gets a wide window on the eval and its reporting.
func TestSlot_EvalTimeout_HasNoRaceOnTheOutcome(t *testing.T) {
	f := newEngineFixture(t)
	f.register("evm:1", frozenPolicy(slowEval(30*time.Millisecond), time.Millisecond),
		f.upstream("rpc1"))

	for i := 0; i < 10; i++ {
		f.tick("evm:1", "*")
	}
	require.Empty(t, f.lastDecision("evm:1", "*").Error)
	require.Equal(t, []string{"rpc1"}, f.orderIDs("evm:1", "*"))
}
