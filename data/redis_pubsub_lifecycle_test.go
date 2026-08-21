package data

import (
	"context"
	"io"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The pubsub manager has to survive a Redis that is not there yet: the
// connector builds it during start-up, and Subscribe is called from the
// request path. None of the paths below need a Redis server, because the
// point is what happens without one.

// TestRedisPubSubManager_StartsWithoutAClientAndKeepsRetrying proves that a
// manager built before its Redis client exists neither fails construction nor
// claims to be running.
//
// The connector creates the manager during initialization, and a failed start
// there must not abort the connector. It must leave `running` false, so the
// next Subscribe tries again rather than parking a caller on a subscription
// that will never deliver.
func TestRedisPubSubManager_StartsWithoutAClientAndKeepsRetrying(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lg := zerolog.New(io.Discard)
	// A connector with no client at all — exactly the window between the
	// connector being constructed and its first successful connect.
	m := NewRedisPubSubManager(ctx, &lg, &RedisConnector{})
	require.NotNil(t, m, "a failed start must not stop the manager from existing")
	assert.False(t, m.running.Load(), "a failed start must not leave the manager marked running")

	ch, cleanup, err := m.Subscribe("some-key")
	require.Error(t, err, "a subscription that cannot be served must say so")
	assert.Nil(t, ch)
	assert.Nil(t, cleanup)

	// Once it believes it is running, start is a no-op rather than a second
	// set of message and polling goroutines.
	m.running.Store(true)
	require.NoError(t, m.start(), "start must be idempotent while the manager runs")
	m.running.Store(false)
}

// TestRedisPubSubManager_MessageLoopReportsAMissingSubscription proves the
// message loop returns instead of reading from a nil subscription.
//
// messageLoop treats the returned error as "the connection is gone" and enters
// its reconnect cycle. Any other outcome here — a panic, or a silent loop —
// takes down every counter watcher on the process.
func TestRedisPubSubManager_MessageLoopReportsAMissingSubscription(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lg := zerolog.New(io.Discard)
	m := &RedisPubSubManager{logger: &lg, appCtx: ctx, connector: &RedisConnector{}}

	err := m.runMessageLoop()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pubsub not initialized")
}

// TestRedisPubSubManager_RemoveSubscriber covers the bookkeeping that decides
// whether a key still has watchers.
//
// The subscriber list is copy-on-write, so removal rebuilds it. Removing the
// wrong entry silently unsubscribes a live watcher; failing to delete the last
// entry keeps the key in the polling set forever, and pollAllKeys then fetches
// a counter nobody reads on every tick.
func TestRedisPubSubManager_RemoveSubscriber(t *testing.T) {
	t.Parallel()

	lg := zerolog.New(io.Discard)
	m := &RedisPubSubManager{logger: &lg, appCtx: context.Background()}

	// Removing from a key that has no subscribers is a no-op, not a panic.
	m.removeSubscriber("absent", newTestSubscriberChannel())

	first, second := newTestSubscriberChannel(), newTestSubscriberChannel()
	m.addSubscriber("k", first)
	m.addSubscriber("k", second)

	m.removeSubscriber("k", first)
	value, ok := m.subscribers.Load("k")
	require.True(t, ok, "one watcher remains, so the key must stay subscribed")
	remaining := value.([]*subscriberChannel)
	require.Len(t, remaining, 1)
	assert.Same(t, second, remaining[0], "the surviving watcher must be the one not removed")

	m.removeSubscriber("k", second)
	_, ok = m.subscribers.Load("k")
	assert.False(t, ok, "the last watcher leaving must drop the key from the polling set")
}
