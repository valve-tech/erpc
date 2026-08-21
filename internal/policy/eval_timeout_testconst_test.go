package policy_test

import (
	"time"

	"github.com/erpc/erpc/common"
)

// testEvalTimeout is deliberately far larger than any eval in this tree needs.
//
// These tests exercise the policy stdlib and the engine's ranking, NOT the eval
// deadline. Small per-test deadlines (10ms, 50ms, 100ms) were a bet on how busy
// the machine is. Under `make test-fast`, which runs six shards at once, the
// sobek primer alone can cost more than 50ms per fresh runtime under -race. The
// first eval is then dropped and GetOrdered returns an EMPTY list — which reads
// as "the policy ranked nothing" rather than "the eval never finished". That is
// bug 168, and bugs 10 and 23 are the same shape in other packages.
//
// It matters more since bug 46 was fixed. The timeout now wins: a late result is
// discarded rather than quietly overwriting the deadline, so an eval that
// overruns no longer sneaks its answer in.
//
// The initial eval is synchronous inside RegisterNetwork, so a deadline that
// cannot plausibly fire makes these tests deterministic rather than merely less
// flaky. A test that means to exercise the deadline sets its own — see
// TestSlot_ASlowEvalReportsTheTimeoutInsteadOfItsLateResult.
//
// 10s, not more: EvalInterval defaults to 15s when unset (common/defaults.go),
// and Validate refuses an evalTimeout that is not below the interval. A test
// that drives a real ticker sets a smaller interval, so it keeps its own small
// timeout — the constant would be invalid there.
const testEvalTimeout = common.Duration(10 * time.Second)
