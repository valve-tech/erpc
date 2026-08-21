package thirdparty

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RemoteDataCache is shared by every vendor that discovers chains from a
// remote API. A fault here reaches all of them at once, so these tests drive
// the cache directly rather than through any single vendor.

const cacheTestWait = 5 * time.Second

// publishAndWait runs one successful refresh for key and blocks until the
// snapshot carries it. Every wait is bounded.
func publishAndWait[T any](t *testing.T, c *RemoteDataCache[T], key string, val T) {
	t.Helper()
	logger := zerolog.Nop()
	c.TriggerAsyncRefresh(&logger, key, func(context.Context) (T, error) { return val, nil })
	require.Eventually(t, func() bool { return c.Has(key) }, cacheTestWait, 5*time.Millisecond,
		"async refresh never published key %q", key)
}

// inflightCount reads the in-flight tracker. TriggerAsyncRefresh fills it
// synchronously before it starts the goroutine, so a caller can check it
// straight after the trigger without waiting on the goroutine.
func inflightCount[T any](c *RemoteDataCache[T]) int {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	return len(c.inflight)
}

func waitInflightDrained[T any](t *testing.T, c *RemoteDataCache[T]) {
	t.Helper()
	require.Eventually(t, func() bool { return inflightCount(c) == 0 }, cacheTestWait, 5*time.Millisecond,
		"refresh never released its in-flight slot")
}

func TestRemoteDataCache_Lookup_ColdCacheReportsNoValueAndNotFresh(t *testing.T) {
	c := NewRemoteDataCache[map[int64]string]("test")

	val, fresh := c.Lookup("k", time.Hour)

	assert.Nil(t, val, "a cold cache must not hand back a value")
	assert.False(t, fresh, "a cold cache must not claim freshness")
}

func TestRemoteDataCache_Lookup_UnknownKeyIsNotFreshEvenWithAPopulatedSnapshot(t *testing.T) {
	c := NewRemoteDataCache[int]("test")
	publishAndWait(t, c, "a", 11)

	val, fresh := c.Lookup("b", time.Hour)

	assert.Equal(t, 0, val, "an unknown key must not borrow another key's value")
	assert.False(t, fresh, "an unknown key must never read as fresh")
}

func TestRemoteDataCache_Lookup_KeepsTheValueButDropsFreshnessPastTheRecheckInterval(t *testing.T) {
	c := NewRemoteDataCache[int]("test")
	publishAndWait(t, c, "a", 11)

	freshVal, fresh := c.Lookup("a", time.Hour)
	require.Equal(t, 11, freshVal)
	require.True(t, fresh, "a value published moments ago is fresh under a one-hour interval")

	// A zero recheck interval makes every stored value stale by definition.
	staleVal, stale := c.Lookup("a", 0)
	assert.Equal(t, 11, staleVal, "a stale entry must still return its value so callers can serve it")
	assert.False(t, stale, "a zero recheck interval must mark the entry stale")
}

func TestRemoteDataCache_Has_ReportsKeyPresenceNotValueUsability(t *testing.T) {
	c := NewRemoteDataCache[[]string]("test")
	assert.False(t, c.Has("a"), "a cold cache holds no key")

	// A vendor fetcher that finds nothing returns a nil slice, and that nil
	// is a successful result. The cache stores it and Has reports it.
	publishAndWait(t, c, "a", nil)

	assert.True(t, c.Has("a"), "a published nil value still occupies the key")
	val, fresh := c.Lookup("a", time.Hour)
	assert.Nil(t, val)
	assert.True(t, fresh, "a nil value published moments ago is fresh")
	// Every vendor branches on `value == nil` to mean "cold start", so a
	// successful empty fetch is indistinguishable from an unpopulated cache
	// at the vendor layer. See the report for the operator-visible effect.
}

func TestRemoteDataCache_TriggerAsyncRefresh_PublishesExactlyWhatTheFetcherReturned(t *testing.T) {
	c := NewRemoteDataCache[map[int64]string]("test")
	publishAndWait(t, c, "a", map[int64]string{7: "seven"})

	val, fresh := c.Lookup("a", time.Hour)
	require.True(t, fresh)
	assert.Equal(t, map[int64]string{7: "seven"}, val)
}

func TestRemoteDataCache_TriggerAsyncRefresh_RunsOneFetchPerKeyAtATime(t *testing.T) {
	c := NewRemoteDataCache[int]("test")
	logger := zerolog.Nop()

	var calls atomic.Int32
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	fetcher := func(context.Context) (int, error) {
		calls.Add(1)
		started <- struct{}{}
		<-release
		return 11, nil
	}

	c.TriggerAsyncRefresh(&logger, "a", fetcher)
	select {
	case <-started:
	case <-time.After(cacheTestWait):
		t.Fatal("the first refresh never started")
	}

	// The first fetch is parked inside the fetcher, so the slot is held for
	// certain while these five extra triggers arrive.
	for i := 0; i < 5; i++ {
		c.TriggerAsyncRefresh(&logger, "a", fetcher)
	}
	assert.Equal(t, int32(1), calls.Load(), "a busy key must not start a second fetch")

	close(release)
	require.Eventually(t, func() bool { return c.Has("a") }, cacheTestWait, 5*time.Millisecond)
	assert.Equal(t, int32(1), calls.Load(), "six triggers for one key must collapse to one fetch")
}

func TestRemoteDataCache_TriggerAsyncRefresh_ABusyKeyDoesNotStallAnotherKey(t *testing.T) {
	c := NewRemoteDataCache[int]("test")
	logger := zerolog.Nop()

	startedA := make(chan struct{}, 1)
	releaseA := make(chan struct{})
	c.TriggerAsyncRefresh(&logger, "a", func(context.Context) (int, error) {
		startedA <- struct{}{}
		<-releaseA
		return 1, nil
	})
	select {
	case <-startedA:
	case <-time.After(cacheTestWait):
		t.Fatal("the refresh for key a never started")
	}
	defer close(releaseA)

	// Key b must publish while key a is still parked in its fetcher.
	c.TriggerAsyncRefresh(&logger, "b", func(context.Context) (int, error) { return 2, nil })
	require.Eventually(t, func() bool { return c.Has("b") }, cacheTestWait, 5*time.Millisecond,
		"a refresh on one key must not block a refresh on another")

	assert.False(t, c.Has("a"), "key a is still fetching, so it must not have published yet")
}

func TestRemoteDataCache_ARefreshFailureKeepsThePreviousValue(t *testing.T) {
	c := NewRemoteDataCache[int]("test")
	publishAndWait(t, c, "a", 11)
	logger := zerolog.Nop()

	c.TriggerAsyncRefresh(&logger, "a", func(context.Context) (int, error) {
		return 0, errors.New("vendor API is down")
	})
	waitInflightDrained(t, c)

	val, fresh := c.Lookup("a", time.Hour)
	assert.Equal(t, 11, val, "a failed refresh must not overwrite the good value")
	assert.True(t, fresh, "a failed refresh must not reset the fetch time either")
}

func TestRemoteDataCache_ARefreshFailureReleasesTheKeyForALaterRefresh(t *testing.T) {
	c := NewRemoteDataCache[int]("test")
	logger := zerolog.Nop()

	c.TriggerAsyncRefresh(&logger, "a", func(context.Context) (int, error) {
		return 0, errors.New("vendor API is down")
	})
	waitInflightDrained(t, c)
	require.False(t, c.Has("a"), "the failed refresh published nothing")

	// If the failure leaked the in-flight slot, this second refresh is a no-op
	// and the key stays cold for the life of the process.
	publishAndWait(t, c, "a", 11)
	val, fresh := c.Lookup("a", time.Hour)
	assert.Equal(t, 11, val)
	assert.True(t, fresh)
}

func TestRemoteDataCache_APanickingFetcherLeavesTheKeyRefreshable(t *testing.T) {
	c := NewRemoteDataCache[int]("test")
	logger := zerolog.Nop()

	// An unrecovered panic in the refresh goroutine kills the whole process,
	// so reaching the assertions at all proves the recover works.
	c.TriggerAsyncRefresh(&logger, "a", func(context.Context) (int, error) {
		panic("vendor fetcher blew up")
	})
	waitInflightDrained(t, c)
	require.False(t, c.Has("a"), "the panicking refresh published nothing")

	publishAndWait(t, c, "a", 11)
	val, _ := c.Lookup("a", time.Hour)
	assert.Equal(t, 11, val, "a key must stay refreshable after its fetcher panics")
}

func TestRemoteDataCache_PublishingOneKeyPreservesTheValueAndFetchTimeOfEveryOtherKey(t *testing.T) {
	c := NewRemoteDataCache[int]("test")
	publishAndWait(t, c, "a", 11)
	publishAndWait(t, c, "b", 22)

	valA, freshA := c.Lookup("a", time.Hour)
	assert.Equal(t, 11, valA, "publishing key b must carry key a's value forward")
	assert.True(t, freshA, "publishing key b must carry key a's fetch time forward")

	valB, freshB := c.Lookup("b", time.Hour)
	assert.Equal(t, 22, valB)
	assert.True(t, freshB)
}

func TestRemoteDataCache_EnsureFresh_ColdStartIsUnusableAndStartsARefresh(t *testing.T) {
	c := NewRemoteDataCache[int]("test")
	logger := zerolog.Nop()

	release := make(chan struct{})
	defer close(release)
	val, usable := c.EnsureFresh(&logger, "a", time.Hour, func(context.Context) (int, error) {
		<-release
		return 11, nil
	})

	assert.Equal(t, 0, val, "a cold key must hand back the zero value")
	assert.False(t, usable, "a cold key must not be reported usable")
	assert.Equal(t, 1, inflightCount(c), "a cold key must start a refresh")
}

func TestRemoteDataCache_EnsureFresh_AFreshValueSkipsTheRefresh(t *testing.T) {
	c := NewRemoteDataCache[int]("test")
	publishAndWait(t, c, "a", 11)
	waitInflightDrained(t, c)
	logger := zerolog.Nop()

	release := make(chan struct{})
	defer close(release)
	val, usable := c.EnsureFresh(&logger, "a", time.Hour, func(context.Context) (int, error) {
		<-release
		return 99, nil
	})

	assert.Equal(t, 11, val)
	assert.True(t, usable)
	// TriggerAsyncRefresh claims the slot before it spawns the goroutine, so
	// an empty tracker here proves no fetch was started.
	assert.Equal(t, 0, inflightCount(c), "a fresh value must not hit the network")
}

func TestRemoteDataCache_EnsureFresh_AStaleValueStaysUsableAndStartsARefresh(t *testing.T) {
	c := NewRemoteDataCache[int]("test")
	publishAndWait(t, c, "a", 11)
	waitInflightDrained(t, c)
	logger := zerolog.Nop()

	release := make(chan struct{})
	defer close(release)
	// A zero recheck interval makes the stored value stale.
	val, usable := c.EnsureFresh(&logger, "a", 0, func(context.Context) (int, error) {
		<-release
		return 99, nil
	})

	assert.Equal(t, 11, val, "a stale value must still be served")
	assert.True(t, usable, "a stale value must still be reported usable")
	assert.Equal(t, 1, inflightCount(c), "a stale value must start a refresh")
}

func TestRemoteDataCache_EnsureFresh_APublishedNilValueReadsAsUsable(t *testing.T) {
	c := NewRemoteDataCache[[]string]("test")
	publishAndWait(t, c, "a", nil)
	waitInflightDrained(t, c)
	logger := zerolog.Nop()

	val, usable := c.EnsureFresh(&logger, "a", time.Hour, func(context.Context) ([]string, error) {
		return []string{"x"}, nil
	})

	assert.Nil(t, val)
	assert.True(t, usable, "EnsureFresh keys on presence, so a published nil counts as usable")
	// Note the divergence: every vendor open-codes Lookup plus a `== nil`
	// check instead, and therefore reads this same state as a cold start.
}
