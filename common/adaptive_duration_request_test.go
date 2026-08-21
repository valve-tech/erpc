package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ResolveForRequest is how a quantile-driven timeout or hedge delay reaches a
// live request. Every step it takes can be missing at runtime — the request has
// no network yet, the body did not parse, the method has no latency history —
// and each of those must fall back to the static part of the spec rather than
// producing a zero cap or a panic.

func TestAdaptiveDurationResolveForRequest(t *testing.T) {
	quantileSpec := &AdaptiveDuration{
		Base:     Duration(10 * time.Millisecond),
		Quantile: 0.99,
		Min:      Duration(50 * time.Millisecond),
		Max:      Duration(2 * time.Second),
	}
	staticSpec := &AdaptiveDuration{Base: Duration(250 * time.Millisecond)}

	t.Run("a zero spec resolves to no cap", func(t *testing.T) {
		var nilSpec *AdaptiveDuration
		require.Equal(t, time.Duration(0), nilSpec.ResolveForRequest(timeoutTestRequest(t, nil)))
		require.Equal(t, time.Duration(0), (&AdaptiveDuration{}).ResolveForRequest(timeoutTestRequest(t, nil)))
	})

	t.Run("a nil request resolves to no cap", func(t *testing.T) {
		require.Equal(t, time.Duration(0), quantileSpec.ResolveForRequest(nil))
	})

	t.Run("a static spec ignores the request entirely", func(t *testing.T) {
		ntw := &timeoutTestNetwork{metrics: &fixedMetrics{q: &fixedQuantiles{d: 900 * time.Millisecond}}}
		require.Equal(t, 250*time.Millisecond, staticSpec.ResolveForRequest(timeoutTestRequest(t, ntw)))
	})

	t.Run("a request with no network falls back to the static part", func(t *testing.T) {
		// Base 10ms + the Min floor 50ms, because there is no quantile data.
		require.Equal(t, 60*time.Millisecond, quantileSpec.ResolveForRequest(timeoutTestRequest(t, nil)))
	})

	t.Run("a network with no metrics for the method falls back to the static part", func(t *testing.T) {
		ntw := &timeoutTestNetwork{metrics: nil}
		require.Equal(t, 60*time.Millisecond, quantileSpec.ResolveForRequest(timeoutTestRequest(t, ntw)))
	})

	t.Run("a network with cold metrics falls back to the Min floor", func(t *testing.T) {
		ntw := &timeoutTestNetwork{metrics: &fixedMetrics{q: &fixedQuantiles{d: 0}}}
		require.Equal(t, 60*time.Millisecond, quantileSpec.ResolveForRequest(timeoutTestRequest(t, ntw)))
	})

	t.Run("a warm quantile drives the result", func(t *testing.T) {
		ntw := &timeoutTestNetwork{metrics: &fixedMetrics{q: &fixedQuantiles{d: 400 * time.Millisecond}}}
		require.Equal(t, 410*time.Millisecond, quantileSpec.ResolveForRequest(timeoutTestRequest(t, ntw)))
	})

	t.Run("the Max ceiling clamps a slow quantile", func(t *testing.T) {
		ntw := &timeoutTestNetwork{metrics: &fixedMetrics{q: &fixedQuantiles{d: 30 * time.Second}}}
		require.Equal(t, 2*time.Second, quantileSpec.ResolveForRequest(timeoutTestRequest(t, ntw)))
	})

	t.Run("an unparseable body falls back to the static part", func(t *testing.T) {
		// The method name is the metrics key. Without one there is nothing to
		// look up, so the spec must resolve without quantile data instead of
		// keying the whole network's latency under an empty method.
		ntw := &timeoutTestNetwork{metrics: &fixedMetrics{q: &fixedQuantiles{d: 900 * time.Millisecond}}}
		req := NewNormalizedRequest([]byte(`not json at all`))
		req.SetNetwork(ntw)

		method, err := req.Method()
		require.Error(t, err)
		require.Equal(t, "", method)

		require.Equal(t, 60*time.Millisecond, quantileSpec.ResolveForRequest(req))
	})
}
