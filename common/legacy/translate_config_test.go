package legacy_test

import (
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/common/legacy"
	"github.com/stretchr/testify/require"
)

// evmNetwork returns a minimal EVM network config the translator can
// write a synthesized selectionPolicy onto.
func evmNetwork(chainID int64) *common.NetworkConfig {
	return &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: chainID},
	}
}

// The legacy `scoreLatencyQuantile` is a 0..1 float. sortByScore takes a
// bucket label instead, so the translator snaps the float to the nearest
// bucket. An operator who writes 0.71 must get the next bucket UP (p90),
// never a silently faster one: snapping down would loosen the latency
// target the operator asked for.
func TestTranslate_ScoreLatencyQuantile_SnapsToBucket(t *testing.T) {
	cases := []struct {
		quantile float64
		label    string
	}{
		{0.10, "p50"},
		{0.50, "p50"},
		{0.51, "p70"},
		{0.70, "p70"},
		{0.71, "p90"},
		{0.90, "p90"},
		{0.91, "p95"},
		{0.95, "p95"},
		{0.99, "p99"},
		{1.0, "p99"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			legacyUps := []legacy.WidenedUpstream{legacy.WidenedUpstreamForTest(nil, tc.quantile)}
			nwCfgs := []*common.NetworkConfig{evmNetwork(1)}
			_, err := legacy.Translate(
				legacy.WidenedProject{}, legacyUps, []*common.UpstreamConfig{{Id: "a"}},
				[]legacy.WidenedNetwork{{}}, nwCfgs,
			)
			require.NoError(t, err)
			require.NotNil(t, nwCfgs[0].SelectionPolicy)
			require.Contains(t, nwCfgs[0].SelectionPolicy.EvalFunc,
				"{ latencyQuantile: '"+tc.label+"' }",
				"quantile %v must snap to bucket %s", tc.quantile, tc.label)
		})
	}
}

// A negative scoreLatencyQuantile is not a value sortByScore can use.
// The translator drops it: the emitted call stays the plain
// `sortByScore(PREFER_FASTEST)` and no bucket is invented.
func TestTranslate_NegativeScoreLatencyQuantile_EmitsNoOpt(t *testing.T) {
	legacyUps := []legacy.WidenedUpstream{legacy.WidenedUpstreamForTest(nil, -0.5)}
	nwCfgs := []*common.NetworkConfig{evmNetwork(1)}
	_, err := legacy.Translate(
		legacy.WidenedProject{RoutingStrategy: "score-based"}, legacyUps,
		[]*common.UpstreamConfig{{Id: "a"}}, []legacy.WidenedNetwork{{}}, nwCfgs,
	)
	require.NoError(t, err)
	require.NotNil(t, nwCfgs[0].SelectionPolicy)
	require.Contains(t, nwCfgs[0].SelectionPolicy.EvalFunc, "sortByScore(PREFER_FASTEST)",
		"a negative quantile must leave sortByScore un-optioned")
	require.NotContains(t, nwCfgs[0].SelectionPolicy.EvalFunc, "latencyQuantile",
		"a negative quantile must not emit a latencyQuantile opt")
}

// The first upstream that sets scoreLatencyQuantile wins. One eval calls
// sortByScore once, so per-upstream divergence cannot be expressed. An
// upstream with no routing block must not reset the choice.
func TestTranslate_ScoreLatencyQuantile_FirstUpstreamWins(t *testing.T) {
	legacyUps := []legacy.WidenedUpstream{
		{},
		legacy.WidenedUpstreamForTest(nil, 0.50),
		legacy.WidenedUpstreamForTest(nil, 0.99),
	}
	nwCfgs := []*common.NetworkConfig{evmNetwork(1)}
	_, err := legacy.Translate(
		legacy.WidenedProject{}, legacyUps,
		[]*common.UpstreamConfig{{Id: "a"}, {Id: "b"}, {Id: "c"}},
		[]legacy.WidenedNetwork{{}}, nwCfgs,
	)
	require.NoError(t, err)
	require.Contains(t, nwCfgs[0].SelectionPolicy.EvalFunc, "{ latencyQuantile: 'p50' }",
		"the first upstream that sets a quantile decides the sortByScore opt")
	require.NotContains(t, nwCfgs[0].SelectionPolicy.EvalFunc, "p99",
		"a later upstream must not override the first quantile")
}

// TranslateFromConfig runs on every config load. A nil config and a nil
// project entry are both reachable from a hand-built config, so neither
// may panic the loader.
func TestTranslateFromConfig_NilInputs(t *testing.T) {
	warns, err := legacy.TranslateFromConfig(nil)
	require.NoError(t, err)
	require.Empty(t, warns)

	cfg := &common.Config{Projects: []*common.ProjectConfig{nil, {Id: "p1"}}}
	warns, err = legacy.TranslateFromConfig(cfg)
	require.NoError(t, err)
	require.Empty(t, warns, "a project with no legacy fields emits no warning")
}

// Every warning the operator reads names the project it came from. A
// multi-project config otherwise gives no way to tell which project
// holds the deprecated key.
func TestTranslateFromConfig_WarningsNameTheProject(t *testing.T) {
	cfg := &common.Config{Projects: []*common.ProjectConfig{
		{
			Id:            "alpha",
			LegacyProject: &common.LegacyProjectFields{RoutingStrategy: "round-robin"},
			Networks:      []*common.NetworkConfig{evmNetwork(1)},
		},
		{
			Id:            "beta",
			LegacyProject: &common.LegacyProjectFields{ScoreMetricsMode: "detailed"},
			Networks:      []*common.NetworkConfig{evmNetwork(10)},
		},
	}}
	warns, err := legacy.TranslateFromConfig(cfg)
	require.NoError(t, err)
	require.Len(t, warns, 2)

	var alpha, beta string
	for _, w := range warns {
		if strings.Contains(w, `project "alpha"`) {
			alpha = w
		}
		if strings.Contains(w, `project "beta"`) {
			beta = w
		}
	}
	require.Contains(t, alpha, "routingStrategy", "alpha's warning must name its own deprecated key")
	require.Contains(t, beta, "scoreMetricsMode", "beta's warning must name its own deprecated key")
}

// An operator who writes the modern `evalScope: network-method-finality`
// alongside a legacy `evalFunction` must keep the finality scope. The
// translator reads evalScope back as "per method" and must not downgrade
// it to the coarser network-method.
func TestTranslateFromConfig_KeepsModernEvalScope(t *testing.T) {
	nw := evmNetwork(1)
	nw.SelectionPolicy = &common.SelectionPolicyConfig{
		EvalScope: common.EvalScopeNetworkMethodFinality,
		LegacySelectionPolicy: &common.LegacySelectionPolicyFields{
			EvalFunction: "(upstreams, method) => upstreams",
		},
	}
	cfg := &common.Config{Projects: []*common.ProjectConfig{{
		Id:       "p1",
		Networks: []*common.NetworkConfig{nw},
	}}}

	_, err := legacy.TranslateFromConfig(cfg)
	require.NoError(t, err)
	require.Equal(t, common.EvalScopeNetworkMethodFinality, nw.SelectionPolicy.EvalScope,
		"the translator must not downgrade a modern evalScope")
	require.Contains(t, nw.SelectionPolicy.EvalFunc, "const __legacyFn =",
		"the legacy evalFunction is still wrapped into the modern eval")
}

// The deprecated `evalPerMethod: true` bool promotes to the modern
// evalScope enum on the LoadConfig path, and the legacy stash is cleared
// so nothing downstream reads it again.
func TestTranslateFromConfig_EvalPerMethodPromotesToScope(t *testing.T) {
	perMethod := true
	nw := evmNetwork(1)
	nw.SelectionPolicy = &common.SelectionPolicyConfig{
		EvalPerMethod: &perMethod,
		LegacySelectionPolicy: &common.LegacySelectionPolicyFields{
			EvalFunction: "(upstreams, method) => upstreams",
		},
	}
	cfg := &common.Config{Projects: []*common.ProjectConfig{{
		Id:       "p1",
		Networks: []*common.NetworkConfig{nw},
	}}}

	_, err := legacy.TranslateFromConfig(cfg)
	require.NoError(t, err)
	require.Equal(t, common.EvalScopeNetworkMethod, nw.SelectionPolicy.EvalScope,
		"legacy evalPerMethod must promote to evalScope=network-method")
	require.Nil(t, nw.SelectionPolicy.LegacySelectionPolicy,
		"the legacy stash must be cleared after translation")
}

// A network-level legacy key is reported, not rewritten in silence. The
// translator changes selection behaviour when it wraps
// `selectionPolicy.evalFunction` or `resampleExcluded`, and the operator
// learns which key caused it.
//
// The sub-tests measure the reported cases against the quiet one, so the
// test fails if the translator starts warning about everything as readily
// as it fails if it goes back to warning about nothing.
func TestTranslateFromConfig_NetworkLegacyFieldsAreReported(t *testing.T) {
	// translate runs one network through the whole LoadConfig path and
	// hands back the warnings and the network it rewrote.
	translate := func(t *testing.T, sp *common.SelectionPolicyConfig) ([]string, *common.NetworkConfig) {
		t.Helper()
		nw := evmNetwork(1)
		nw.SelectionPolicy = sp
		cfg := &common.Config{Projects: []*common.ProjectConfig{{
			Id:       "p1",
			Networks: []*common.NetworkConfig{nw},
		}}}
		warns, err := legacy.TranslateFromConfig(cfg)
		require.NoError(t, err)
		return warns, nw
	}

	joined := func(warns []string) string { return strings.Join(warns, "\n") }

	t.Run("EvalFunctionAndResampleExcludedBothReport", func(t *testing.T) {
		warns, nw := translate(t, &common.SelectionPolicyConfig{
			LegacySelectionPolicy: &common.LegacySelectionPolicyFields{
				EvalFunction:     "(upstreams, method) => upstreams",
				ResampleExcluded: true,
				ResampleInterval: common.Duration(5 * time.Minute),
			},
		})
		require.NotEmpty(t, nw.SelectionPolicy.EvalFunc, "the translation itself still happens")
		require.Contains(t, joined(warns), "selectionPolicy.evalFunction",
			"the rewrite must name the key that caused it")
		require.Contains(t, joined(warns), "selectionPolicy.resampleExcluded",
			"the rewrite must name the key that caused it")
	})

	// One key, one warning. The other key must stay quiet, or the message
	// tells the operator nothing about which one they wrote.
	t.Run("ResampleExcludedAloneDoesNotBlameEvalFunction", func(t *testing.T) {
		warns, _ := translate(t, &common.SelectionPolicyConfig{
			LegacySelectionPolicy: &common.LegacySelectionPolicyFields{
				ResampleExcluded: true,
				ResampleInterval: common.Duration(5 * time.Minute),
			},
		})
		require.Contains(t, joined(warns), "selectionPolicy.resampleExcluded")
		require.NotContains(t, joined(warns), "selectionPolicy.evalFunction")
	})

	// A modern eval beside a legacy one is the worse case: the legacy
	// function is discarded, not translated. The operator reads code that
	// decides nothing, so the message must say "ignored", not "wrapped".
	t.Run("ADiscardedEvalFunctionSaysItIsIgnored", func(t *testing.T) {
		warns, nw := translate(t, &common.SelectionPolicyConfig{
			EvalFunc: "(upstreams, ctx) => upstreams",
			LegacySelectionPolicy: &common.LegacySelectionPolicyFields{
				EvalFunction: "(upstreams, method) => upstreams.slice(0, 1)",
			},
		})
		require.Equal(t, "(upstreams, ctx) => upstreams", nw.SelectionPolicy.EvalFunc,
			"the modern eval still wins")
		require.Contains(t, joined(warns), "is ignored",
			"a discarded legacy function must not be reported as a translation")
	})

	// One line per key, however many networks share the config block.
	t.Run("ThreeNetworksProduceOneLinePerKey", func(t *testing.T) {
		var nws []*common.NetworkConfig
		for _, chainID := range []int64{1, 10, 137} {
			nw := evmNetwork(chainID)
			nw.SelectionPolicy = &common.SelectionPolicyConfig{
				LegacySelectionPolicy: &common.LegacySelectionPolicyFields{
					EvalFunction: "(upstreams, method) => upstreams",
				},
			}
			nws = append(nws, nw)
		}
		cfg := &common.Config{Projects: []*common.ProjectConfig{{Id: "p1", Networks: nws}}}
		warns, err := legacy.TranslateFromConfig(cfg)
		require.NoError(t, err)
		require.Equal(t, 1, strings.Count(joined(warns), "selectionPolicy.evalFunction"),
			"three networks, one config key, one line")
	})

	// Nothing legacy, nothing said.
	t.Run("AModernOnlyNetworkSaysNothing", func(t *testing.T) {
		warns, _ := translate(t, &common.SelectionPolicyConfig{
			EvalFunc: "(upstreams, ctx) => upstreams",
		})
		require.Empty(t, warns, "a modern config must produce no deprecation line")
	})
}
