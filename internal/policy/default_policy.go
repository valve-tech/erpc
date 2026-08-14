package policy

import (
	_ "embed"
	"fmt"
	"strings"

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
func upgradeDefaultPolicy(cfg *common.SelectionPolicyConfig) error {
	if cfg == nil {
		return nil
	}
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
