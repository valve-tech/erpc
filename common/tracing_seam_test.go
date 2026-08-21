package common

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// TracingDetailed / SetTracingDetailed
//
// These cover the seam that lets a test switch detailed tracing on while the
// code under test reads the flag from other goroutines. A plain bool cannot do
// that: the race detector fails the run before the assertion is reached.
// ---------------------------------------------------------------------------

// SetTracingDetailed must report the value it replaced, so a caller can put the
// process back the way it found it.
func TestSetTracingDetailed_ReturnsPreviousValue(t *testing.T) {
	saveTracingGlobals(t)

	SetTracingDetailed(false)

	assert.False(t, SetTracingDetailed(true), "the first swap reports the previous false")
	assert.True(t, TracingDetailed(), "the swap must publish the new value")

	assert.True(t, SetTracingDetailed(true), "an idempotent swap still reports the previous value")
	assert.True(t, SetTracingDetailed(false), "turning the flag off reports the previous true")
	assert.False(t, TracingDetailed(), "the flag must be off after the last swap")
}

// A goroutine that starts after the write must see the new value. This is the
// clients-side case: the test flips the flag, then the pooled interceptor runs.
func TestSetTracingDetailed_LaterReaderObservesTheFlip(t *testing.T) {
	saveTracingGlobals(t)

	const readers = 8

	for _, want := range []bool{true, false} {
		SetTracingDetailed(want)

		start := make(chan struct{})
		seen := make([]bool, readers)

		var wg sync.WaitGroup
		for i := 0; i < readers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				seen[i] = TracingDetailed()
			}(i)
		}

		// Closing start happens after the write, so every reader below must
		// observe it. No sleep and no retry loop are involved.
		close(start)
		wg.Wait()

		for i, got := range seen {
			assert.Equal(t, want, got, "reader %d must observe the value written before it started", i)
		}
	}
}

// Many readers and one writer must not trip the race detector, and the value
// left behind must be the last one written. Every goroutine runs a fixed number
// of iterations, so the test cannot hang.
func TestSetTracingDetailed_ConcurrentReadersAreRaceFree(t *testing.T) {
	saveTracingGlobals(t)

	SetTracingDetailed(false)

	const readers = 8
	const iterations = 2000

	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < iterations; j++ {
				_ = TracingDetailed()
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for j := 0; j < iterations; j++ {
			SetTracingDetailed(j%2 == 0)
		}
		SetTracingDetailed(true)
	}()

	close(start)
	wg.Wait()

	assert.True(t, TracingDetailed(), "the last write must be the value that survives")
}

// StartDetailSpan is the flag's main consumer inside common. It must follow the
// seam, otherwise a test that flips the flag changes nothing.
func TestStartDetailSpan_FollowsSetTracingDetailed(t *testing.T) {
	h := newTracingHarness(t, false)

	_, span := StartDetailSpan(context.Background(), "Detail.Off")
	span.End()
	assert.Empty(t, h.ended(), "with the flag off no detail span may be recorded")

	SetTracingDetailed(true)

	_, span = StartDetailSpan(context.Background(), "Detail.On")
	span.End()
	require.NotNil(t, h.endedNamed("Detail.On"), "flipping the seam must let detail spans through")
}

// The harness and InitializeTracing must publish the same value to both views,
// so a package that still reads the deprecated variable and a package that
// reads the accessor never disagree about a startup configuration.
func TestTracingDetailed_HarnessKeepsBothViewsInSync(t *testing.T) {
	for _, detailed := range []bool{true, false} {
		t.Run(map[bool]string{true: "detailed on", false: "detailed off"}[detailed], func(t *testing.T) {
			newTracingHarness(t, detailed)

			assert.Equal(t, detailed, IsTracingDetailed, "the deprecated variable must carry the configured value")
			assert.Equal(t, detailed, TracingDetailed(), "the accessor must carry the same value")
		})
	}
}
