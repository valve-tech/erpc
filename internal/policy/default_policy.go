package policy

import (
	_ "embed"
	"fmt"
	"strings"
	"sync"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// failoverCtxKey is the JS name of the eval-context field that carries
// `failover.onDefaultsExhausted` into the policy program.
const failoverCtxKey = "failoverOnDefaultsExhausted"

// warnIfFailoverIgnored tells the operator when `failover.onDefaultsExhausted`
// cannot take effect on the HTTP path. The bundled default policy reads
// `ctx.failoverOnDefaultsExhausted`; a hand-written evalFunc that never
// mentions it silently ignores the key, and a config surface that
// silently does nothing is worse than one that does not exist.
func warnIfFailoverIgnored(logger *zerolog.Logger, networkID string, cfg *common.SelectionPolicyConfig) {
	if logger == nil || cfg == nil || !cfg.FailoverOnDefaultsExhausted {
		return
	}
	if strings.Contains(cfg.EvalFunc, failoverCtxKey) {
		return
	}
	logger.Warn().
		Str("networkId", networkID).
		Msgf("failover.onDefaultsExhausted is set, but this network's custom selectionPolicy.evalFunc never reads ctx.%s. "+
			"HTTP requests will not escalate to the tier:fallback upstreams. WebSocket subscribes are not affected", failoverCtxKey)
}

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
// needs. The deeper fix is for the engine to stop editing a config it does
// not own; that is logged as a separate finding.
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
