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

// These tests pin bug 46/144: the eval timeout used to race the eval's own
// result and then always lose it, so `ErrEvalTimeout` never reached a
// caller. Every eval below busy-waits in JS, because that is what a real
// overrunning policy does — sobek cannot be interrupted mid-call, so the
// slot has to wait for the eval either way.

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

// TestSlot_EvalTimeout_ReachesTheDecision is the core pin. The eval
// overruns its timeout by 15x, so the slot MUST record the timeout on the
// decision. Before the fix the late result overwrote the timeout verdict
// and the decision carried no error at all.
func TestSlot_EvalTimeout_ReachesTheDecision(t *testing.T) {
	f := newEngineFixture(t)
	f.register("evm:1", frozenPolicy(slowEval(300*time.Millisecond), 20*time.Millisecond),
		f.upstream("rpc1"), f.upstream("rpc2"))

	d := f.lastDecision("evm:1", "*")
	require.Contains(t, d.Error, "selection policy eval timed out",
		"a slow eval must report ErrEvalTimeout on its decision")
}

// TestSlot_EvalTimeout_KeepsThePreviousOrder pins the verdict half: a tick
// that times out retains the previous cache instead of publishing the late
// result. The eval is fast on its first call and slow on every call after,
// so one slot produces both outcomes.
func TestSlot_EvalTimeout_KeepsThePreviousOrder(t *testing.T) {
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

	require.Contains(t, f.lastDecision("evm:1", "*").Error, "selection policy eval timed out")
	require.Equal(t, []string{"rpc1"}, f.orderIDs("evm:1", "*"),
		"a timed-out tick keeps the previous order; it must not publish the late result")
}

// TestSlot_EvalTimeout_CountsAndLogs pins the operator surfaces. A
// timed-out tick increments `selection_eval_errors_total{kind="timeout"}`
// and writes the eval-failed warning. Both read the decision error, which
// the fix is what makes non-empty.
func TestSlot_EvalTimeout_CountsAndLogs(t *testing.T) {
	f := newEngineFixture(t)
	counter := telemetry.MetricSelectionEvalErrorsTotal.WithLabelValues("p1", "evm:1", "*", "timeout")
	before := promUtil.ToFloat64(counter)

	f.register("evm:1", frozenPolicy(slowEval(300*time.Millisecond), 20*time.Millisecond),
		f.upstream("rpc1"))

	require.Equal(t, 1.0, promUtil.ToFloat64(counter)-before,
		`one timed-out tick must increment selection_eval_errors_total{kind="timeout"} exactly once`)
	require.Contains(t, f.Logs.String(), "selection policy eval failed; retaining previous cache")
	require.Contains(t, f.Logs.String(), "selection policy eval timed out")
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

// TestSlot_EvalTimeout_HasNoRaceOnTheOutcome drives many timed-out ticks
// so the race detector gets a wide window on the eval goroutine and the
// timeout branch. Before the fix both wrote the same variable.
func TestSlot_EvalTimeout_HasNoRaceOnTheOutcome(t *testing.T) {
	f := newEngineFixture(t)
	f.register("evm:1", frozenPolicy(slowEval(30*time.Millisecond), time.Millisecond),
		f.upstream("rpc1"))

	for i := 0; i < 10; i++ {
		f.tick("evm:1", "*")
	}
	require.Contains(t, f.lastDecision("evm:1", "*").Error, "selection policy eval timed out")
}
