package legacy

import "fmt"

// Warning message templates. Each links the selection-policy reference so
// operators can find the full context.
//
// The link used to be https://docs.erpc.cloud/migration/selection-policy
// with a per-message anchor. No such page exists — docs/pages has no
// migration/ directory — and none of the anchors exist either, so every
// warning sent the operator to a 404. The anchors went with it: a link to
// a heading nobody wrote is a commitment to a page layout, not a
// destination.
const migrationDoc = "https://docs.erpc.cloud/config/projects/selection-policies"

func warnRoutingStrategy(strategy string) string {
	return fmt.Sprintf(
		"[deprecated config] routingStrategy=%q is deprecated; translated to selectionPolicy.eval. See %s",
		strategy, migrationDoc,
	)
}

func warnScoreMetricsMode(mode string) string {
	return fmt.Sprintf(
		"[deprecated config] scoreMetricsMode=%q is no longer used; the new erpc_selection_* metrics have fixed cardinality. See %s",
		mode, migrationDoc,
	)
}

// WarnLegacySelectionPolicy emits a warning when a network used the
// legacy selectionPolicy.evalFunction field.
func WarnLegacySelectionPolicy() string {
	return fmt.Sprintf(
		"[deprecated config] selectionPolicy.evalFunction is deprecated; wrapped into the new selectionPolicy.eval shape. Migrate manually for clarity (the new chainable stdlib is more expressive). See %s",
		migrationDoc,
	)
}

// WarnResampleExcluded emits a warning when a network used resampleExcluded.
func WarnResampleExcluded() string {
	return fmt.Sprintf(
		"[deprecated config] selectionPolicy.resampleExcluded is deprecated; translated to .probeExcluded() (deterministic time-based re-admission). See %s",
		migrationDoc,
	)
}

// warnDiscardedEvalFunction emits a warning when a network wrote BOTH the
// legacy `selectionPolicy.evalFunction` and the modern
// `selectionPolicy.eval`. The modern one wins and the legacy one never
// runs, so the operator is reading code that decides nothing.
func warnDiscardedEvalFunction() string {
	return fmt.Sprintf(
		"[deprecated config] selectionPolicy.evalFunction is ignored because selectionPolicy.eval is also set; the legacy function does not run. Delete it. See %s",
		migrationDoc,
	)
}

// warnInertField emits a deprecation warning for a project-level
// legacy field that has NO behavioral mapping in the new system. The
// translator must NOT synthesize an eval on account of these fields —
// they're warning-only so the canonical default policy stays in place.
func warnInertField(field, replacement string) string {
	return fmt.Sprintf(
		"[deprecated config] %s is no longer used (%s). Remove from your config; see %s",
		field, replacement, migrationDoc,
	)
}
