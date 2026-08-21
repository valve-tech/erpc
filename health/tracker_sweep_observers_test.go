package health

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

// A client that sends random JSON-RPC method names mints a new Prometheus
// label-set per request. Prometheus never forgets a series on its own, so the
// sweep has to delete the label-sets as well as the in-memory cache entries.
// If it only cleared the cache, /metrics would keep re-emitting the attack's
// series forever and the scrape would grow without bound.

// seriesMatching counts the time series of one collector whose label values
// contain `token`. It goes through a private registry so the count is scoped
// to this test, not to whatever the rest of the package left in the global one.
func seriesMatching(t *testing.T, c prometheus.Collector, token string) int {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	require.NoError(t, reg.Register(c))

	families, err := reg.Gather()
	require.NoError(t, err)

	n := 0
	for _, fam := range families {
		for _, m := range fam.GetMetric() {
			for _, lp := range m.GetLabel() {
				if strings.Contains(lp.GetValue(), token) {
					n++
					break
				}
			}
		}
	}
	return n
}

// countCacheEntries reports how many label-sets a hot-path cache holds.
func countCacheEntries(m *sync.Map) int {
	n := 0
	m.Range(func(any, any) bool {
		n++
		return true
	})
	return n
}

func TestSweepIdleObservers_ReleasesTheDurationLabelSetOfAnIdleEntry(t *testing.T) {
	const token = "sweep-obs-duration"
	tracker := NewTracker(&log.Logger, token, time.Minute)
	ups := common.NewFakeUpstream("up1")

	tracker.RecordUpstreamDuration(ups, "eth_flood1", 10*time.Millisecond, true, "none", common.DataFinalityStateAll, "user-a")

	require.Equal(t, 1, countCacheEntries(&tracker.urdObsCache),
		"the observer cache must hold the entry the request just created")
	require.Equal(t, 1, seriesMatching(t, telemetry.MetricUpstreamRequestDuration, token))

	// A cutoff in the past leaves a freshly-used entry alone.
	tracker.sweepIdleObservers(time.Now().Add(-time.Hour).UnixMilli())
	require.Equal(t, 1, countCacheEntries(&tracker.urdObsCache),
		"an entry used a moment ago is not idle")

	// A cutoff in the future makes every entry idle.
	tracker.sweepIdleObservers(time.Now().Add(time.Hour).UnixMilli())
	require.Equal(t, 0, countCacheEntries(&tracker.urdObsCache))
	require.Equal(t, 0, seriesMatching(t, telemetry.MetricUpstreamRequestDuration, token),
		"the sweep cleared the cache but left the Prometheus series behind")
}

func TestSweepIdleObservers_ReleasesTheRateLimitLabelSetOfAnIdleEntry(t *testing.T) {
	const token = "sweep-obs-ratelimit"
	tracker := NewTracker(&log.Logger, token, time.Minute)
	ups := common.NewFakeUpstream("up1")

	tracker.RecordUpstreamRemoteRateLimited(context.Background(), ups, "eth_flood1", nil)

	require.Equal(t, 1, countCacheEntries(&tracker.remoteRateLimitedCounterCache))
	require.Equal(t, 1, seriesMatching(t, telemetry.MetricRateLimitsTotal, token))

	tracker.sweepIdleObservers(time.Now().Add(-time.Hour).UnixMilli())
	require.Equal(t, 1, countCacheEntries(&tracker.remoteRateLimitedCounterCache))

	tracker.sweepIdleObservers(time.Now().Add(time.Hour).UnixMilli())
	require.Equal(t, 0, countCacheEntries(&tracker.remoteRateLimitedCounterCache))
	require.Equal(t, 0, seriesMatching(t, telemetry.MetricRateLimitsTotal, token),
		"the sweep cleared the cache but left the Prometheus series behind")
}

func TestSweepIdleObservers_KeepsAnEntryThatIsStillInUse(t *testing.T) {
	// The sweep runs on a coarse cadence over every cached label-set. It must
	// evict per-entry, not per-pass: deleting a live upstream's series would
	// blank the dashboards for a node that is serving traffic right now.
	const token = "sweep-obs-mixed"
	tracker := NewTracker(&log.Logger, token, time.Minute)
	ups := common.NewFakeUpstream("up1")

	tracker.RecordUpstreamDuration(ups, "eth_stale", 10*time.Millisecond, true, "none", common.DataFinalityStateAll, "user-a")
	staleCutoff := time.Now().UnixMilli() + 1

	// Wait past the cutoff, then use the second entry only.
	time.Sleep(5 * time.Millisecond)
	tracker.RecordUpstreamDuration(ups, "eth_live", 10*time.Millisecond, true, "none", common.DataFinalityStateAll, "user-a")

	tracker.sweepIdleObservers(staleCutoff)

	require.Equal(t, 1, countCacheEntries(&tracker.urdObsCache),
		"exactly the idle entry must go")
	require.Equal(t, 1, seriesMatching(t, telemetry.MetricUpstreamRequestDuration, token))

	tracker.urdObsCache.Range(func(k, _ any) bool {
		require.Equal(t, "eth_live", k.(urdoKey).category,
			"the sweep evicted the live entry and kept the stale one")
		return true
	})
}

func TestSweepIdle_RunsTheObserverSweepOnTheRotationCadence(t *testing.T) {
	// sweepIdleObservers is only reachable through sweepIdle, which the
	// rotation loop drives. A wiring break here disables the whole defence
	// silently — the caches just keep growing.
	tracker := NewTracker(&log.Logger, "sweep-wiring", time.Minute)
	tracker.SetIdleEvictionAfter(time.Nanosecond)
	ups := common.NewFakeUpstream("up1")

	tracker.RecordUpstreamDuration(ups, "eth_flood1", 10*time.Millisecond, true, "none", common.DataFinalityStateAll, "user-a")
	require.Equal(t, 1, countCacheEntries(&tracker.urdObsCache))

	// One millisecond puts the entry past a 1ns idle threshold.
	time.Sleep(2 * time.Millisecond)
	tracker.sweepIdle()

	require.Equal(t, 0, countCacheEntries(&tracker.urdObsCache),
		"sweepIdle must reach the observer caches, not only the metric maps")
}
