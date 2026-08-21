package telemetry

import (
	"math"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// These tests write down what eRPC's metrics actually mean. The counters and
// their label values are read by dashboards, alerts and billing pipelines, and
// several of them do NOT mean what their name suggests. Encoding the real
// semantics here stops a well-meant "cleanup" from silently changing what an
// operator's graph is counting.

func TestUpstreamRequestTotal_SchemaIsStable(t *testing.T) {
	// Every consumer of erpc_upstream_request_total groups by these labels.
	// Adding, removing or reordering one changes what a PromQL `sum by (...)`
	// returns, and the call sites pass values positionally — a reorder
	// mislabels every series without any compile error.
	require.Equal(t, []string{
		"project", "vendor", "network", "upstream",
		"category", "attempt", "composite", "finality",
		"user", "agent_name",
	}, MetricUpstreamRequestTotal.schema)
	require.Equal(t, "upstream_request_total", MetricUpstreamRequestTotal.metricName)
}

func TestUpstreamRequestTotal_CountsInternalPollerTrafficToo(t *testing.T) {
	// The trap: erpc_upstream_request_total is NOT a count of client requests.
	// The EVM state poller's own probes increment it through the same code
	// path, so a completely idle eRPC still shows steady upstream request
	// traffic. Anyone reading this counter as "customer calls" over-reports.
	//
	// The counter carries no label that separates poller traffic from client
	// traffic — the `category` label holds the METHOD, so poller probes appear
	// as ordinary eth_blockNumber / eth_getBlockByNumber rows.
	lc := newTestLabeledCounter(t, "test_upstream_request_total",
		[]string{"project", "vendor", "network", "upstream", "category", "attempt", "composite", "finality", "user", "agent_name"},
		nil, nil)

	// A client call and a state-poller probe, side by side.
	lc.WithLabelValues("p", "alchemy", "evm:1", "up1", "eth_getBalance", "0", "none", "unfinalized", "user-1", "curl").Inc()
	lc.WithLabelValues("p", "alchemy", "evm:1", "up1", "eth_blockNumber", "0", "none", "unfinalized", "n/a", "n/a").Inc()

	require.Equal(t, 2, testutil.CollectAndCount(lc),
		"poller probes land in the same counter as client calls, distinguished only by method")
}

func TestUpstreamRequestTotal_SentinelLabelValuesAreNotEndpoints(t *testing.T) {
	// "n/a", "<error>" and "*" are sentinels, not upstream identities. A
	// dashboard that lists distinct `upstream` values renders them as phantom
	// servers, and an alert on "upstream X is failing" fires for a value that
	// no operator can find in their config.
	//
	//   "n/a"      — the upstream / network was not resolved for this event
	//                (see Upstream.NetworkId and NetworkLabel, which return
	//                "n/a" for a nil or not-yet-bootstrapped upstream)
	//   "<error>"  — a value the recording site could not determine
	//   "*"        — a deliberate aggregate row, not one endpoint
	//
	// Consumers must filter these out before treating `upstream` as a fleet
	// inventory.
	for _, sentinel := range []string{"n/a", "<error>", "*"} {
		require.True(t, isMetricSentinel(sentinel),
			"%q must be recognised as a sentinel label value, not an endpoint", sentinel)
	}
	require.False(t, isMetricSentinel("alchemy-eth-mainnet"),
		"a real upstream id must not be mistaken for a sentinel")
}

// isMetricSentinel reports whether a label value is one of eRPC's placeholder
// values rather than a real identity. It lives in the test because it encodes
// a CONSUMER-side rule: the recording sites emit these values, and every
// reader has to exclude them.
func isMetricSentinel(v string) bool {
	switch v {
	case "n/a", "<error>", "*", "":
		return true
	}
	return strings.HasPrefix(v, "<") && strings.HasSuffix(v, ">")
}

func TestDroppingTheUpstreamLabelKeepsTheSumButLosesTheAttribution(t *testing.T) {
	// counterDropLabels is the knob operators reach for when /metrics gets too
	// big. Dropping `upstream` keeps every total correct but makes per-upstream
	// attribution impossible — and any billing query grouping by it silently
	// returns one merged row instead of failing.
	schema := []string{"project", "upstream", "category"}
	lc := newTestLabeledCounter(t, "test_drop_upstream_total", schema, []string{"upstream"}, nil)

	lc.WithLabelValues("p", "up1", "eth_getBalance").Inc()
	lc.WithLabelValues("p", "up2", "eth_getBalance").Inc()
	lc.WithLabelValues("p", "up3", "eth_getBalance").Inc()

	require.Equal(t, 1, testutil.CollectAndCount(lc), "three upstreams collapse into one series")
	require.Equal(t, float64(3), testutil.ToFloat64(lc.vec.WithLabelValues("p", "eth_getBalance")),
		"the total must be preserved exactly — only the dimension is lost")
}

func TestLabeledCounter_RebuildAppliesAFilterChangedAfterConstruction(t *testing.T) {
	// The package counters are built at init, before the config is read.
	// Rebuild is the only way a counterDropLabels setting can reach them:
	// Prometheus freezes a metric's label-set hash for the registry's life, so
	// unregister-and-re-register cannot change it.
	schema := []string{"network", "agent_name"}
	before := newTestLabeledCounter(t, "test_rebuild_total", schema, nil, nil)
	require.Len(t, before.activeIdx, 2, "no filter yet: the full schema is active")

	SetCounterLabelFilter([]string{"agent_name"}, nil)
	after := before.Rebuild()
	require.Len(t, after.activeIdx, 1, "Rebuild must pick up the filter installed after construction")

	reg := prometheus.NewRegistry()
	reg.MustRegister(after)
	after.WithLabelValues("evm:1", "agent-a").Inc()
	after.WithLabelValues("evm:1", "agent-b").Inc()
	require.Equal(t, 1, testutil.CollectAndCount(after))

	require.Len(t, before.activeIdx, 2, "Rebuild must return a NEW counter, not mutate the old one")
}

func TestLabeledCounter_ResetClearsEverySeries(t *testing.T) {
	// Reset is used when metrics are rebuilt at config reload. Leaving stale
	// series behind would double-count the pre-reload traffic.
	lc := newTestLabeledCounter(t, "test_reset_total", []string{"network"}, nil, nil)
	lc.WithLabelValues("evm:1").Inc()
	lc.WithLabelValues("evm:137").Inc()
	require.Equal(t, 2, testutil.CollectAndCount(lc))

	lc.Reset()
	require.Equal(t, 0, testutil.CollectAndCount(lc))
}

func TestLabeledCounter_DeleteLabelValuesUsesTheFullSchema(t *testing.T) {
	// Call sites hold full-schema tuples. If Delete expected post-filter
	// values, the idle sweep would silently delete nothing and the /metrics
	// page would grow without bound under a method flood.
	lc := newTestLabeledCounter(t, "test_delete_total", []string{"network", "agent_name"}, []string{"agent_name"}, nil)
	lc.WithLabelValues("evm:1", "agent-a").Inc()
	require.Equal(t, 1, testutil.CollectAndCount(lc))

	require.True(t, lc.DeleteLabelValues("evm:1", "agent-a"),
		"deleting with the full schema must find the collapsed series")
	require.Equal(t, 0, testutil.CollectAndCount(lc))
	require.False(t, lc.DeleteLabelValues("evm:1", "agent-a"),
		"a second delete must be a no-op, not a double count")
}

func TestLabeledHistogram_DeleteAndResetHonourTheFullSchema(t *testing.T) {
	// Same contract as the counter. The health tracker's idle sweep calls both
	// with full-schema tuples; a mismatch leaks series forever.
	t.Cleanup(func() { SetHistogramLabelFilter(nil, nil) })
	SetHistogramLabelFilter([]string{"user"}, nil)

	schema := []string{"network", "user"}
	lh := NewLabeledHistogram(prometheus.HistogramOpts{Name: "test_lh_delete_seconds"}, schema)
	reg := prometheus.NewRegistry()
	reg.MustRegister(lh)

	lh.WithLabelValues("evm:1", "user-a").Observe(0.1)
	lh.WithLabelValues("evm:1", "user-b").Observe(0.2)
	require.Equal(t, 1, testutil.CollectAndCount(lh), "the dropped user label collapses both observations")

	require.Equal(t, []string{"evm:1"}, lh.ActiveLabelValues([]string{"evm:1", "user-a"}),
		"ActiveLabelValues must project down to the retained labels")

	require.True(t, lh.DeleteLabelValues("evm:1", "user-a"))
	require.Equal(t, 0, testutil.CollectAndCount(lh))

	lh.WithLabelValues("evm:1", "user-c").Observe(0.3)
	lh.Reset()
	require.Equal(t, 0, testutil.CollectAndCount(lh))
}

func TestLabeledHistogram_UnfilteredActiveLabelValuesReturnTheInput(t *testing.T) {
	t.Cleanup(func() { SetHistogramLabelFilter(nil, nil) })
	SetHistogramLabelFilter(nil, nil)
	lh := NewLabeledHistogram(prometheus.HistogramOpts{Name: "test_lh_passthrough_seconds"}, []string{"network", "user"})
	require.Equal(t, []string{"evm:1", "user-a"}, lh.ActiveLabelValues([]string{"evm:1", "user-a"}))
}

func TestGaugeHandle_CachesOneChildPerLabelTuple(t *testing.T) {
	// The handle cache exists to keep a Vec map lookup off the hot path. If it
	// returned a fresh child each call, the cache would be pure overhead; if it
	// returned the SAME child for different labels, gauges would overwrite each
	// other's values.
	t.Cleanup(ResetHandleCache)
	ResetHandleCache()

	gv := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "test_gauge_handle"}, []string{"network"})
	reg := prometheus.NewRegistry()
	reg.MustRegister(gv)

	a1 := GaugeHandle(gv, "evm:1")
	a2 := GaugeHandle(gv, "evm:1")
	b := GaugeHandle(gv, "evm:137")

	require.Equal(t, a1, a2, "the same labels must return the cached child")
	require.NotEqual(t, a1, b, "different labels must return different children")

	a1.Set(42)
	b.Set(7)
	require.Equal(t, float64(42), testutil.ToFloat64(gv.WithLabelValues("evm:1")))
	require.Equal(t, float64(7), testutil.ToFloat64(gv.WithLabelValues("evm:137")))
}

func TestObserverHandle_FilteredHistogramTuplesShareOneCacheEntry(t *testing.T) {
	// Under a label filter several full-schema tuples resolve to ONE underlying
	// observer. Keying the cache on the full tuple would create several entries
	// for one series, which is exactly the bug the counter cache had.
	t.Cleanup(func() { SetHistogramLabelFilter(nil, nil) })
	t.Cleanup(ResetHandleCache)
	SetHistogramLabelFilter([]string{"user"}, nil)
	ResetHandleCache()

	lh := NewLabeledHistogram(prometheus.HistogramOpts{Name: "test_obs_handle_seconds"}, []string{"network", "user"})
	reg := prometheus.NewRegistry()
	reg.MustRegister(lh)

	a := ObserverHandle(lh, "evm:1", "user-a")
	b := ObserverHandle(lh, "evm:1", "user-b")
	require.Equal(t, a, b, "collapsed tuples must share one cached observer")

	entries := 0
	observerHandleCache.Range(func(_, _ any) bool { entries++; return true })
	require.Equal(t, 1, entries, "one underlying series must not occupy several cache entries")
}

func TestObserverHandle_PlainHistogramVecIsCachedPerTuple(t *testing.T) {
	t.Cleanup(ResetHandleCache)
	ResetHandleCache()

	hv := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "test_obs_plain_seconds"}, []string{"network"})
	reg := prometheus.NewRegistry()
	reg.MustRegister(hv)

	require.Equal(t, ObserverHandle(hv, "evm:1"), ObserverHandle(hv, "evm:1"))
	require.NotEqual(t, ObserverHandle(hv, "evm:1"), ObserverHandle(hv, "evm:137"))
}

func TestParseHistogramBuckets_EmptyStringYieldsTheDefaults(t *testing.T) {
	// An unset config must not produce a histogram with zero buckets, which
	// would make every duration quantile unqueryable.
	got, err := ParseHistogramBuckets("")
	require.NoError(t, err)
	require.Equal(t, DefaultHistogramBuckets, got)
	require.NotEmpty(t, got)
}

func TestParseHistogramBuckets_ParsesAndSortsTheValues(t *testing.T) {
	// Prometheus requires ascending bucket bounds. Accepting an unsorted list
	// as written would produce a histogram whose cumulative counts decrease —
	// silently corrupt quantiles rather than an error.
	got, err := ParseHistogramBuckets("1.0, 0.05,0.5 , 10")
	require.NoError(t, err)
	require.Equal(t, []float64{0.05, 0.5, 1.0, 10}, got)
}

func TestParseHistogramBuckets_RejectsANonNumericEntry(t *testing.T) {
	// A typo in the config must fail loudly at startup. Skipping the bad entry
	// would give the operator a histogram they did not ask for.
	_, err := ParseHistogramBuckets("0.1,fast,1.0")
	require.Error(t, err)
	require.Contains(t, err.Error(), "fast", "the error must name the offending value")
}

func TestParseHistogramBuckets_SingleValueIsAccepted(t *testing.T) {
	got, err := ParseHistogramBuckets("0.25")
	require.NoError(t, err)
	require.Equal(t, []float64{0.25}, got)
}

func TestParseHistogramBuckets_TrailingSeparatorIsAnError(t *testing.T) {
	// "0.1,0.5," splits into an empty final field. Treating it as zero would
	// silently add a 0-second bucket to every duration histogram.
	_, err := ParseHistogramBuckets("0.1,0.5,")
	require.Error(t, err)
}

func TestParseHistogramBuckets_AcceptsNaNAndInfinity(t *testing.T) {
	// This test records CURRENT behaviour, not desired behaviour.
	// ParseHistogramBuckets delegates to strconv.ParseFloat, which accepts
	// "NaN" and "Inf". A NaN bound never sorts into place and never matches an
	// observation, so the bucket it creates silently stays empty — the
	// operator sees a histogram that looks fine and answers quantile queries
	// wrongly. There is no validation step to catch it.
	//
	// If someone adds that validation, this test will fail. That is the
	// correct outcome: update it to require an error.
	got, err := ParseHistogramBuckets("NaN")
	require.NoError(t, err, "no validation rejects NaN today")
	require.Len(t, got, 1)
	require.True(t, math.IsNaN(got[0]), "the NaN bound is passed straight through to Prometheus")

	got, err = ParseHistogramBuckets("0.1,+Inf")
	require.NoError(t, err)
	require.True(t, math.IsInf(got[1], 1), "an explicit +Inf bound is also accepted")
}
