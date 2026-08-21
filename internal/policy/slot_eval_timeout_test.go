package policy_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// Bug 46 pin (and bug 144, which is the same defect found again).
//
// tickOnce runs the eval on its own goroutine. It used to share two plain
// variables with that goroutine, and on the timeout branch it wrote evalErr and
// THEN waited for the goroutine — so the goroutine's write always landed second
// and always overwrote ErrEvalTimeout.
//
// The consequences were: the race detector reported the pair, and the timeout
// could not be observed at all. A slow-but-successful eval was published as if
// it had arrived on time. `selection_eval_errors_total{kind="timeout"}` was a
// counter that could never increment, so an operator whose policy outgrew its
// evalTimeout saw no error, no log and no metric.
//
// This test is deterministic — it does not depend on machine load. The eval
// busy-waits well past the deadline, so the timeout always fires first and the
// result always arrives late. What it asserts is that the LATE result loses.
func TestSlot_ASlowEvalReportsTheTimeoutInsteadOfItsLateResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	tracker := health.NewTracker(&logger, "p1", time.Minute)

	// The eval succeeds, but only after 400ms. The deadline is 50ms.
	cfg := &common.SelectionPolicyConfig{
		EvalInterval: 0,
		EvalTimeout:  common.Duration(50 * time.Millisecond),
		EvalFunc:     "(ups, _ctx) => { const t = Date.now(); while (Date.now() - t < 400) {} return ups }",
	}
	require.NoError(t, cfg.SetDefaults())

	engine := policy.NewEngine(ctx, &logger, "p1", tracker, nil, nil)
	defer engine.Stop()
	ups := []common.Upstream{&fakeUpstream{id: "rpc1", tier: "main"}}
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	logs := buf.String()
	require.Contains(t, logs, "selection policy eval timed out",
		"bug 46: the timeout must reach the Decision, not be overwritten by the late result")
	require.Contains(t, logs, "retaining previous cache",
		"bug 46: a result that misses the deadline must not be published as if it met it")
}
