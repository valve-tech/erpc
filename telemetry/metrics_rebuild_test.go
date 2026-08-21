package telemetry

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RebuildFilteredCounters is the step erpc.Init runs to (a) apply the operator's
// counterDropLabels and (b) expose the package counters on /metrics at all.
// Package counters are built UNREGISTERED at init, so if this call panics or is
// skipped the process either dies at startup or serves a /metrics with no
// counters on it.
//
// Every test here restores the package's global state — the counter vars, the
// default registerer and the filter — because the whole package shares them.

// rebuiltCounters lists every package var RebuildFilteredCounters reassigns.
// Kept next to the test that swaps them so a counter added to the rebuild list
// without being added here shows up as an unrestored global.
func rebuiltCounters() []**LabeledCounter {
	return []**LabeledCounter{
		&MetricUnexpectedPanicTotal,
		&MetricUpstreamRequestTotal,
		&MetricUpstreamErrorTotal,
		&MetricUpstreamSkippedTotal,
		&MetricUpstreamMissingDataErrorTotal,
		&MetricUpstreamEmptyResponseTotal,
		&MetricNetworkEvmGetLogsSplitSuccess,
		&MetricNetworkEvmGetLogsSplitFailure,
		&MetricNetworkEvmGetLogsForcedSplits,
		&MetricNetworkEvmTraceFilterSplitSuccess,
		&MetricNetworkEvmTraceFilterSplitFailure,
		&MetricNetworkEvmTraceFilterForcedSplits,
		&MetricUpstreamWrongEmptyResponseTotal,
		&MetricNetworkRequestsReceived,
		&MetricNetworkMultiplexedRequests,
		&MetricNetworkHedgedRequestTotal,
		&MetricNetworkHedgeDiscardsTotal,
		&MetricNetworkFailedRequests,
		&MetricNetworkSuccessfulRequests,
		&MetricRateLimitsTotal,
		&MetricRateLimiterBudgetDecisionTotal,
		&MetricRateLimiterFailopenTotal,
		&MetricCacheSetErrorTotal,
		&MetricCacheGetErrorTotal,
		&MetricShadowResponseErrorTotal,
		&MetricAuthFailedTotal,
		&MetricConsensusTotal,
		&MetricConsensusMisbehaviorDetected,
		&MetricConsensusUpstreamPunished,
		&MetricConsensusShortCircuit,
		&MetricConsensusWaitCapped,
		&MetricConsensusErrors,
		&MetricConsensusUpstreamErrors,
		&MetricConsensusPanics,
		&MetricConsensusCancellations,
		&MetricNetworkEvmBlockRangeRequested,
	}
}

// isolateCounterGlobals points the default registerer at a scratch registry and
// restores every counter var, the registerer and the filter afterwards.
func isolateCounterGlobals(t *testing.T, drop []string, overrides map[string][]string) *prometheus.Registry {
	t.Helper()

	vars := rebuiltCounters()
	saved := make([]*LabeledCounter, len(vars))
	for i, v := range vars {
		saved[i] = *v
	}
	savedRegisterer := prometheus.DefaultRegisterer

	t.Cleanup(func() {
		for i, v := range vars {
			*v = saved[i]
		}
		prometheus.DefaultRegisterer = savedRegisterer
		SetCounterLabelFilter(nil, nil)
		ResetHandleCache()
	})

	reg := prometheus.NewRegistry()
	prometheus.DefaultRegisterer = reg
	SetCounterLabelFilter(drop, overrides)
	ResetHandleCache()
	return reg
}

func gatheredNames(t *testing.T, reg *prometheus.Registry) map[string]bool {
	t.Helper()
	families, err := reg.Gather()
	require.NoError(t, err)
	out := make(map[string]bool, len(families))
	for _, f := range families {
		out[f.GetName()] = true
	}
	return out
}

func TestRebuildFilteredCounters_MakesThePackageCountersScrapeable(t *testing.T) {
	reg := isolateCounterGlobals(t, nil, nil)

	// Before the rebuild the init-time counters exist but are registered
	// nowhere, so /metrics carries none of them.
	require.Empty(t, gatheredNames(t, reg), "sanity: the scratch registry starts empty")

	RebuildFilteredCounters()

	// Only a registered counter that has been incremented shows up in Gather,
	// so touch one of each shape.
	MetricUpstreamRequestTotal.WithLabelValues(
		"p", "vendor", "evm:1", "up1", "eth_call", "0", "none", "unfinalized", "user-1", "curl").Inc()
	MetricNetworkRequestsReceived.WithLabelValues(
		"p", "evm:1", "eth_call", "unfinalized", "user-1", "curl").Inc()

	names := gatheredNames(t, reg)
	assert.True(t, names["erpc_upstream_request_total"],
		"the rebuild did not register the upstream counter; /metrics would show none of it")
	assert.True(t, names["erpc_network_request_received_total"],
		"the rebuild must register every package counter, got %v", names)
}

func TestRebuildFilteredCounters_AppliesTheOperatorsDropList(t *testing.T) {
	// Dropping a label collapses the series that differed only in it. This is
	// the whole reason the rebuild exists: Prometheus freezes a metric's label
	// set for the life of a registry, so the filter can only be applied here.
	isolateCounterGlobals(t, []string{"user"}, nil)
	RebuildFilteredCounters()

	require.NotContains(t, activeLabelsOf(MetricNetworkFailedRequests), "user",
		"the drop list did not reach the rebuilt counter")

	inc := func(user string) {
		MetricNetworkFailedRequests.WithLabelValues(
			"p", "evm:1", "eth_call", "0", "ErrX", "critical", "unfinalized", user, "curl").Inc()
	}
	inc("user-1")
	inc("user-2")
	inc("user-3")

	assert.Equal(t, 1, testutil.CollectAndCount(MetricNetworkFailedRequests),
		"three users must collapse into one series once `user` is dropped")
}

func TestRebuildFilteredCounters_KeepsALabelAnOverrideNames(t *testing.T) {
	// A fleet-wide drop must still spare the one counter a billing or
	// attribution pipeline groups by.
	isolateCounterGlobals(t, []string{"user"},
		map[string][]string{"network_failed_request_total": {"user"}})
	RebuildFilteredCounters()

	assert.Contains(t, activeLabelsOf(MetricNetworkFailedRequests), "user",
		"the per-metric override did not survive the rebuild")
	assert.NotContains(t, activeLabelsOf(MetricUpstreamRequestTotal), "user",
		"a metric the override does not name must still drop the label")
}

func TestRebuildFilteredCounters_SecondCallWithTheSameFilterIsIdempotent(t *testing.T) {
	// registerOrReuseCounter is what makes this safe. Without it the second
	// Register returns AlreadyRegisteredError and the rebuild panics — which in
	// production is a hot reload taking the process down.
	isolateCounterGlobals(t, []string{"user"}, nil)

	RebuildFilteredCounters()
	require.NotPanics(t, RebuildFilteredCounters,
		"a repeated rebuild under the same filter must reuse the registered counters")
}

// activeLabelsOf reports the labels a LabeledCounter actually forwards to its
// underlying vec after the filter has been applied.
func activeLabelsOf(lc *LabeledCounter) []string {
	out := make([]string, 0, len(lc.activeIdx))
	for _, i := range lc.activeIdx {
		out = append(out, lc.schema[i])
	}
	return out
}

func TestNewLabeledCounter_ReusesTheAlreadyRegisteredCollector(t *testing.T) {
	// The same guard from a caller's point of view: declaring the same counter
	// twice returns the live one instead of panicking, so a second Init or a
	// re-run test does not kill the process.
	savedRegisterer := prometheus.DefaultRegisterer
	t.Cleanup(func() { prometheus.DefaultRegisterer = savedRegisterer })
	prometheus.DefaultRegisterer = prometheus.NewRegistry()

	opts := prometheus.CounterOpts{Name: "test_reuse_total", Help: "h"}
	schema := []string{"network", "user"}

	first := NewLabeledCounter(opts, schema)
	require.NotNil(t, first)
	first.WithLabelValues("evm:1", "u").Inc()

	second := NewLabeledCounter(opts, schema)
	assert.Same(t, first, second,
		"a repeat declaration must hand back the registered counter, not a fresh empty one")
	assert.Equal(t, float64(1), testutil.ToFloat64(second.vec.WithLabelValues("evm:1", "u")),
		"the reused counter must carry the counts already recorded")
}

func TestRegisterOrReplaceHistogram_ReplacesTheOldInstance(t *testing.T) {
	// SetHistogramBuckets calls this on every bucket change, including on a
	// hot reload. Unregistering the old instance first is what stops the second
	// call from panicking on a duplicate registration.
	savedRegisterer := prometheus.DefaultRegisterer
	t.Cleanup(func() { prometheus.DefaultRegisterer = savedRegisterer })
	reg := prometheus.NewRegistry()
	prometheus.DefaultRegisterer = reg

	schema := []string{"network"}
	opts := prometheus.HistogramOpts{Name: "test_replace_seconds", Help: "h", Buckets: []float64{1, 2}}

	first := RegisterOrReplaceHistogram(nil, opts, schema)
	require.NotNil(t, first)
	first.WithLabelValues("evm:1").Observe(1.5)

	opts.Buckets = []float64{0.1, 0.5, 1}
	var second *LabeledHistogram
	require.NotPanics(t, func() { second = RegisterOrReplaceHistogram(first, opts, schema) },
		"re-declaring a histogram must unregister the old one first")
	require.NotSame(t, first, second)

	// The registry must hold exactly the replacement, with the new buckets.
	second.WithLabelValues("evm:1").Observe(0.2)
	families, err := reg.Gather()
	require.NoError(t, err)
	require.Len(t, families, 1, "the old histogram was left registered alongside the new one")
	require.Len(t, families[0].GetMetric(), 1)
	assert.Len(t, families[0].GetMetric()[0].GetHistogram().GetBucket(), 3,
		"the replacement must expose the NEW bucket boundaries")
	assert.EqualValues(t, 1, families[0].GetMetric()[0].GetHistogram().GetSampleCount(),
		"the replacement starts empty; the old instance's samples are gone by design")
}
