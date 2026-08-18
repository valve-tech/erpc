package data

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// listenerTestConnector builds a connector against a real postgres whose
// listener pool holds at most maxConns connections.
func listenerTestConnector(t *testing.T, ctx context.Context, dbAddr string, table string, maxConns int32) *PostgreSQLConnector {
	t.Helper()

	logger := zerolog.New(io.Discard)
	connector, err := NewPostgreSQLConnector(ctx, &logger, "pg-"+table, &common.PostgreSQLConnectorConfig{
		Table:         table,
		ConnectionUri: fmt.Sprintf("postgres://postgres:password@%s/postgres?sslmode=disable", dbAddr),
		InitTimeout:   common.Duration(30 * time.Second),
		GetTimeout:    common.Duration(5 * time.Second),
		SetTimeout:    common.Duration(5 * time.Second),
		MinConns:      1,
		MaxConns:      maxConns,
	})
	require.NoError(t, err)
	require.Equal(t, util.StateReady, connector.initializer.State(), "the connector must come up")
	return connector
}

// acquiredListenerConns reports how many connections the listener pool has
// handed out and not got back.
func acquiredListenerConns(p *PostgreSQLConnector) int32 {
	p.connMu.RLock()
	pool := p.listenerPool
	p.connMu.RUnlock()
	if pool == nil {
		return 0
	}
	return pool.Stat().AcquiredConns()
}

// WatchCounterInt64 took one connection out of the listener pool per watched
// key and never gave it back: the cleanup closed the caller's channel and left
// the listener, its goroutine and its connection in place. The pool holds
// maxConns connections, so a process that watched maxConns distinct keys could
// never watch another one — Acquire blocked and the next watch failed with
// "context deadline exceeded".
//
// eRPC watches one counter per tracked value per network, so a fleet with more
// networks than maxConns hit this during normal startup.
func TestPostgreSQLConnector_WatchCounterInt64_ReleasesTheListenerConnection(t *testing.T) {
	if testing.Short() {
		t.Skip("container test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const maxConns = 3
	connector := listenerTestConnector(t, ctx, startPostgresContainer(t, ctx), "listener_pool_release", maxConns)

	// Fill the listener pool: one watch per key, up to the pool's limit.
	cleanups := make([]func(), 0, maxConns)
	for i := 0; i < maxConns; i++ {
		key := fmt.Sprintf("counter-key-%d", i)
		watchCtx, watchCancel := context.WithTimeout(ctx, 30*time.Second)
		defer watchCancel()

		_, cleanup, err := connector.WatchCounterInt64(watchCtx, key)
		require.NoError(t, err, "watch %d must succeed while the pool has room", i)
		cleanups = append(cleanups, cleanup)
	}
	assert.Equal(t, int32(maxConns), acquiredListenerConns(connector),
		"each watched key takes one connection out of the listener pool")

	// Every caller goes away.
	for _, cleanup := range cleanups {
		cleanup()
	}

	require.Eventually(t, func() bool {
		return acquiredListenerConns(connector) == 0
	}, 10*time.Second, 50*time.Millisecond,
		"the cleanup must give every listener connection back to the pool")

	// The pool is empty again, so a later watch must still work.
	watchCtx, watchCancel := context.WithTimeout(ctx, 20*time.Second)
	defer watchCancel()
	updates, cleanup, err := connector.WatchCounterInt64(watchCtx, "counter-key-after-cleanup")
	require.NoError(t, err, "a watch after the cleanup must not wait for a free connection")
	require.NotNil(t, updates)
	cleanup()
}

// A listener is shared by every watcher of the same key, so it must outlive
// any single watcher. The first watcher leaving must not take the notification
// stream away from the second one.
func TestPostgreSQLConnector_WatchCounterInt64_AListenerSurvivesItsFirstWatcher(t *testing.T) {
	if testing.Short() {
		t.Skip("container test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	connector := listenerTestConnector(t, ctx, startPostgresContainer(t, ctx), "listener_shared", 5)
	const key = "shared-counter-key"

	firstCtx, firstCancel := context.WithCancel(ctx)
	_, firstCleanup, err := connector.WatchCounterInt64(firstCtx, key)
	require.NoError(t, err)

	secondUpdates, secondCleanup, err := connector.WatchCounterInt64(ctx, key)
	require.NoError(t, err)
	defer secondCleanup()

	// Drain the initial value the watch sends, if any.
	select {
	case <-secondUpdates:
	case <-time.After(2 * time.Second):
	}

	// The first watcher goes away, context and all.
	firstCleanup()
	firstCancel()

	// The second watcher must still receive published updates.
	published := CounterInt64State{Value: 42, UpdatedAt: time.Now().UnixMilli()}
	require.Eventually(t, func() bool {
		if err := connector.PublishCounterInt64(ctx, key, published); err != nil {
			return false
		}
		select {
		case st := <-secondUpdates:
			return st.Value == 42
		case <-time.After(500 * time.Millisecond):
			return false
		}
	}, 20*time.Second, 250*time.Millisecond,
		"the surviving watcher must still see notifications")
}
