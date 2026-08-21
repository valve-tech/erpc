package policy_test

import (
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/internal/policy"
	"github.com/stretchr/testify/require"
)

// `erpc dump` prints the config an operator is about to run. A network
// whose selectionPolicy reads `nil`, or reads as the identity function,
// tells the operator nothing about what will actually route their
// traffic. ResolveEffectiveSelectionPolicies fills the real source in.

func dumpConfigWith(policies ...*common.SelectionPolicyConfig) *common.Config {
	networks := make([]*common.NetworkConfig, 0, len(policies))
	for _, p := range policies {
		networks = append(networks, &common.NetworkConfig{
			Architecture:    common.ArchitectureEvm,
			Evm:             &common.EvmNetworkConfig{ChainId: 1},
			SelectionPolicy: p,
		})
	}
	return &common.Config{
		Projects: []*common.ProjectConfig{{Id: "main", Networks: networks}},
	}
}

// TestResolveEffectiveSelectionPolicies_FillsInTheDefaultForANilPolicy —
// a network with no selectionPolicy block still runs the rich default at
// runtime. The dump has to say so.
func TestResolveEffectiveSelectionPolicies_FillsInTheDefaultForANilPolicy(t *testing.T) {
	cfg := dumpConfigWith(nil)

	policy.ResolveEffectiveSelectionPolicies(cfg)

	got := cfg.Projects[0].Networks[0].SelectionPolicy
	require.NotNil(t, got, "the dump must not show a nil policy")
	require.Equal(t, policy.DefaultPolicySource(), got.EvalFunc,
		"the dump must show the policy the engine will actually run")
	require.Contains(t, got.EvalFunc, "stickyPrimary")
}

// TestResolveEffectiveSelectionPolicies_UpgradesTheIdentityPlaceholder —
// an empty `selectionPolicy:` block leaves the identity placeholder
// behind. The engine upgrades it at register time; the dump must show
// the same upgrade rather than "(upstreams, ctx) => upstreams".
func TestResolveEffectiveSelectionPolicies_UpgradesTheIdentityPlaceholder(t *testing.T) {
	cfg := dumpConfigWith(&common.SelectionPolicyConfig{
		EvalFunc: common.DefaultSelectionPolicySource,
	})

	policy.ResolveEffectiveSelectionPolicies(cfg)

	got := cfg.Projects[0].Networks[0].SelectionPolicy
	require.Equal(t, policy.DefaultPolicySource(), got.EvalFunc)
	require.Equal(t, policy.DefaultPolicySource(), got.EvalFuncOriginal,
		"the original must be upgraded too, or the two fields disagree")
}

// TestResolveEffectiveSelectionPolicies_LeavesAnOperatorPolicyAlone — the
// resolver is for display only. Rewriting a policy the operator wrote
// would make the dump lie about their config.
func TestResolveEffectiveSelectionPolicies_LeavesAnOperatorPolicyAlone(t *testing.T) {
	const custom = "(ups, ctx) => ups.byTag('tier:premium')"
	cfg := dumpConfigWith(&common.SelectionPolicyConfig{EvalFunc: custom})

	policy.ResolveEffectiveSelectionPolicies(cfg)

	require.Equal(t, custom, cfg.Projects[0].Networks[0].SelectionPolicy.EvalFunc)
}

// TestResolveEffectiveSelectionPolicies_KeepsAnUnresolvableTSSentinel — a
// TS-loaded config carries a `__ts_fn__:<id>` sentinel whose source lives
// in the user script. With no script to evaluate the sentinel must stay,
// so the operator at least sees the id instead of a wrong policy.
func TestResolveEffectiveSelectionPolicies_KeepsAnUnresolvableTSSentinel(t *testing.T) {
	const sentinel = "__ts_fn__:fn-42"
	cfg := dumpConfigWith(&common.SelectionPolicyConfig{EvalFunc: sentinel})
	require.True(t, common.IsTSFunctionSentinel(sentinel), "sanity: this is a sentinel")

	policy.ResolveEffectiveSelectionPolicies(cfg)

	require.Equal(t, sentinel, cfg.Projects[0].Networks[0].SelectionPolicy.EvalFunc,
		"an unresolvable sentinel must survive rather than be replaced")
}

// TestResolveEffectiveSelectionPolicies_SkipsNilProjectsAndNetworks — the
// dump path runs on whatever the loader produced. A hole in the slice
// must not take the command down.
func TestResolveEffectiveSelectionPolicies_SkipsNilProjectsAndNetworks(t *testing.T) {
	cfg := &common.Config{
		Projects: []*common.ProjectConfig{
			nil,
			{Id: "main", Networks: []*common.NetworkConfig{nil}},
		},
	}

	require.NotPanics(t, func() { policy.ResolveEffectiveSelectionPolicies(cfg) })
	require.NotPanics(t, func() { policy.ResolveEffectiveSelectionPolicies(nil) })
}

// TestDefaultPolicySource_IsTheEmbeddedChain — every test above compares
// against this string, so it is worth stating what it is.
func TestDefaultPolicySource_IsTheEmbeddedChain(t *testing.T) {
	src := policy.DefaultPolicySource()
	require.Equal(t, src, strings.TrimSpace(src), "the source is trimmed")
	for _, step := range []string{"removeCordoned", "excludeIf", "preferTag", "whenEmpty", "sortByScore", "stickyPrimary", "probeExcluded"} {
		require.Contains(t, src, step)
	}
}
