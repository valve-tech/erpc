package health

import (
	"math"
	"testing"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

// The quantile tracker feeds the latency term of the selection score. A single
// poisoned sample would move every upstream's score at once, so the tracker has
// to drop the bad value and keep serving the good ones.

func TestQuantileTracker_ADropoutSampleDoesNotPoisonTheWindow(t *testing.T) {
	// A duration that arrives as NaN or Inf (a clock jump, an unset start
	// time) must be rejected at the input. If it entered the sketch, every
	// later quantile read would come back as garbage.
	q := NewQuantileTracker(&log.Logger)

	q.Add(math.NaN())
	q.Add(math.Inf(1))
	q.Add(math.Inf(-1))
	require.Equal(t, time.Duration(0), q.GetQuantile(0.9),
		"a tracker that only saw invalid samples has no data, so it must read 0")

	q.Add(1.0)
	p90 := q.GetQuantile(0.9)
	require.Greater(t, p90, 900*time.Millisecond)
	require.Less(t, p90, 1100*time.Millisecond,
		"the one real sample must survive the invalid ones next to it")
}

func TestQuantileSeconds_ReturnsZeroForANilSketch(t *testing.T) {
	// NewQuantileTracker and Reset both discard the sketch-construction
	// error, so a nil bucket is reachable in principle. The read path answers
	// 0 for it instead of dereferencing nil in the request path.
	require.Equal(t, 0.0, quantileSeconds(&log.Logger, nil, 0.9))
}

func TestMergedSnapshot_SkipsANilBucketInsteadOfPanicking(t *testing.T) {
	// Same story one level up: the merge walks every ring slot, and one nil
	// slot must not take down the read.
	q := NewQuantileTracker(&log.Logger)
	q.Add(1.0)
	q.buckets[0] = nil

	require.NotPanics(t, func() {
		p90 := q.GetQuantile(0.9)
		require.Greater(t, p90, 900*time.Millisecond,
			"the surviving buckets must still contribute their samples")
	})
}
