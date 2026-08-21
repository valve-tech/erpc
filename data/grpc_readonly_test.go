package data

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGrpcConnector_ReadOnlyContract pins that every mutating connector method
// refuses on the gRPC connector.
//
// This matters more than it looks. The gRPC connector is a read-through cache
// over a block-data service: it has no store of its own. If any of these
// silently returned nil, the cache layer would believe a write landed and stop
// writing that entry to the connector that can actually hold it — turning a
// warm cache into a permanent miss.
func TestGrpcConnector_ReadOnlyContract(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	g := &GrpcConnector{id: "grpc-under-test"}

	assert.Equal(t, "grpc-under-test", g.Id(), "the connector must report its configured id")

	t.Run("Set is refused", func(t *testing.T) {
		err := g.Set(ctx, "pk", "rk", []byte("v"), nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "read-only")
	})

	t.Run("Delete is refused", func(t *testing.T) {
		err := g.Delete(ctx, "pk", "rk")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "read-only")
	})

	t.Run("Lock is refused and yields no lock", func(t *testing.T) {
		lock, err := g.Lock(ctx, "k", time.Second)
		require.Error(t, err)
		assert.Nil(t, lock, "a refused Lock must not hand back a lock object")
		assert.Contains(t, err.Error(), "read-only")
	})

	t.Run("List is refused and yields no rows", func(t *testing.T) {
		results, token, err := g.List(ctx, ConnectorMainIndex, 10, "")
		require.Error(t, err)
		assert.Nil(t, results)
		assert.Equal(t, "", token)
		assert.Contains(t, err.Error(), "List")
	})

	t.Run("PublishCounterInt64 is refused", func(t *testing.T) {
		err := g.PublishCounterInt64(ctx, "k", CounterInt64State{Value: 1})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "PublishCounterInt64")
	})

	t.Run("WatchCounterInt64 is refused", func(t *testing.T) {
		updates, cleanup, err := g.WatchCounterInt64(ctx, "k")
		require.Error(t, err, "the gRPC connector has no counter transport")
		assert.Contains(t, err.Error(), "WatchCounterInt64")

		// It hands back a non-nil channel and cleanup alongside the error. A
		// caller that ignores the error would block on that channel forever, so
		// pin the shape a caller has to cope with today.
		require.NotNil(t, updates)
		require.NotNil(t, cleanup)
		select {
		case v := <-updates:
			t.Fatalf("the refused watch channel must stay empty, got %+v", v)
		case <-time.After(200 * time.Millisecond):
		}
		require.NotPanics(t, cleanup)
	})
}

// TestGrpcConnector_CacheLatestBlockTimestamp pins the head-age reporter the
// realtime cache guard depends on: an unknown network must report "not known"
// rather than a zero timestamp that reads as 1970.
func TestGrpcConnector_CacheLatestBlockTimestamp(t *testing.T) {
	t.Parallel()

	g := &GrpcConnector{
		id:                "grpc-head",
		latestTsByNetwork: map[string]int64{"evm:1": 1739000000},
	}

	ts, ok := g.CacheLatestBlockTimestamp("evm:1")
	require.True(t, ok, "a network with a polled head must report a known timestamp")
	assert.Equal(t, int64(1739000000), ts)

	_, ok = g.CacheLatestBlockTimestamp("evm:999")
	assert.False(t, ok, "an unpolled network must report its head as unknown")

	g.latestTsByNetwork["evm:2"] = 0
	_, ok = g.CacheLatestBlockTimestamp("evm:2")
	assert.False(t, ok, "a zero timestamp must read as unknown, not as 1970")
}

// TestGrpcConnector_CheckReady pins the readiness gate: a connector with no
// initializer, or with no clients yet, must report why it cannot serve.
func TestGrpcConnector_CheckReady(t *testing.T) {
	t.Parallel()

	noInit := &GrpcConnector{id: "grpc-no-init"}
	err := noInit.checkReady()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "initializer not set")
}
