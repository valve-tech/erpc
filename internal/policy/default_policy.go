package policy

import (
	_ "embed"
	"fmt"
	"strings"
	"sync"

	"github.com/erpc/erpc/common"
)

//go:embed default_policy.js
var defaultPolicyJS string

// DefaultPolicySource is the JS source for the policy applied when a user
// has not supplied `selectionPolicy.eval`. It is the canonical reference
// the admin endpoint `GET /admin/selection/default-policy` returns.
func DefaultPolicySource() string {
	return strings.TrimSpace(defaultPolicyJS)
}

// upgradeDefaultPolicy swaps the trivial identity placeholder
// (`common.DefaultSelectionPolicySource`) for the rich default-policy.js
// at engine-register time. common/ can't reach this package, so the
// upgrade must happen here.
// upgradeDefaultPolicyMu serialises the rewrite below.
//
// The config it edits is SHARED. Networks bootstrap concurrently, and every
// network that leaves selectionPolicy at the default reaches this function
// with the same *SelectionPolicyConfig, so two goroutines wrote EvalFunc,
// EvalFuncOriginal and CompiledProgram at once. `go test -race` reports it as
// a write/write race at networks.go:259, and a torn CompiledProgram would
// surface later as a selection policy that fails to evaluate.
//
// The early return below already makes the rewrite idempotent — the second
// caller sees the replaced EvalFunc and stops — so serialising is all this
// needs.
//
// Network.Bootstrap now hands RegisterNetwork a per-network COPY, so no two
// production registrations reach here with one pointer any more (entry 131).
// The mutex stays because RegisterNetwork is exported: it bounds what any
// other caller can do to a config it shares, which the copy alone cannot.
var upgradeDefaultPolicyMu sync.Mutex

func upgradeDefaultPolicy(cfg *common.SelectionPolicyConfig) error {
	if cfg == nil {
		return nil
	}
	upgradeDefaultPolicyMu.Lock()
	defer upgradeDefaultPolicyMu.Unlock()
	if strings.TrimSpace(cfg.EvalFunc) != strings.TrimSpace(common.DefaultSelectionPolicySource) {
		return nil
	}
	cfg.EvalFunc = DefaultPolicySource()
	cfg.EvalFuncOriginal = cfg.EvalFunc
	program, err := common.CompileProgram(cfg.EvalFunc)
	if err != nil {
		return fmt.Errorf("compile default selection policy: %w", err)
	}
	cfg.CompiledProgram = program
	return nil
}
