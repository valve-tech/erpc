package data

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// startPostgresContainer brings up one postgres container and returns its
// "host:port". One container serves every subtest below, because container
// startup dominates the run time.
func startPostgresContainer(t *testing.T, ctx context.Context) string {
	t.Helper()
	pgC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "postgres:15-alpine",
			Env:          map[string]string{"POSTGRES_PASSWORD": "password"},
			ExposedPorts: []string{"5432/tcp"},
			WaitingFor: wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(3 * time.Minute),
		},
		Started: true,
	})
	require.NoError(t, err, "failed to start the postgres container")
	t.Cleanup(func() { _ = pgC.Terminate(context.Background()) })

	host, err := pgC.Host(ctx)
	require.NoError(t, err)
	port, err := pgC.MappedPort(ctx, "5432")
	require.NoError(t, err)
	return fmt.Sprintf("%s:%s", host, port.Port())
}

// connectorBehindProxy builds a postgres connector that reaches the database
// only through the supplied break proxy. Everything the connector dials — the
// main pool, the listener pool, every reconnect — goes through it.
func connectorBehindProxy(t *testing.T, ctx context.Context, proxy *breakProxy, table string) *PostgreSQLConnector {
	t.Helper()
	logger := zerolog.New(io.Discard)
	cfg := &common.PostgreSQLConnectorConfig{
		Table:         table,
		ConnectionUri: fmt.Sprintf("postgres://postgres:password@%s/postgres?sslmode=disable", proxy.Addr()),
		InitTimeout:   common.Duration(20 * time.Second),
		GetTimeout:    common.Duration(5 * time.Second),
		SetTimeout:    common.Duration(5 * time.Second),
		MinConns:      1,
		MaxConns:      3,
	}
	c, err := NewPostgreSQLConnector(ctx, &logger, "pg-"+table, cfg)
	require.NoError(t, err)
	require.Equal(t, util.StateReady, c.initializer.State(), "the connector must come up through the proxy")
	return c
}

// TestPostgreSQLConnector_Reconnect drives the connector's failure-recovery
// loops with a proxy that severs live connections on command. Every wait is
// bounded, so a mutation that stalls a loop fails the test instead of wedging
// the run.
func TestPostgreSQLConnector_Reconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	dbAddr := startPostgresContainer(t, ctx)

	// The main pool must come back after the database connection dies and the
	// database becomes reachable again. Until it does, every cache read and
	// every auth lookup on this connector fails.
	t.Run("the main pool recovers after the connection is severed", func(t *testing.T) {
		proxy := newBreakProxy(t, dbAddr)
		c := connectorBehindProxy(t, ctx, proxy, "recover_main")

		require.NoError(t, c.Set(ctx, "pk1", "rk1", []byte("v1"), nil))
		got, err := c.Get(ctx, ConnectorMainIndex, "pk1", "rk1", nil)
		require.NoError(t, err)
		require.Equal(t, "v1", string(got))

		// The database goes away: live connections die and new ones are turned
		// away, so the pool cannot quietly re-dial its way out.
		require.Positive(t, proxy.Break(), "the connector must have had a live connection to sever")
		proxy.Refuse(true)

		waitFor(t, 30*time.Second, 200*time.Millisecond, "reads to start failing", func() bool {
			_, gerr := c.Get(ctx, ConnectorMainIndex, "pk1", "rk1", nil)
			return gerr != nil
		})

		// The database comes back.
		proxy.Refuse(false)
		acceptedWhileDown := proxy.Accepted()

		waitFor(t, 90*time.Second, 500*time.Millisecond, "reads to recover", func() bool {
			v, gerr := c.Get(ctx, ConnectorMainIndex, "pk1", "rk1", nil)
			return gerr == nil && string(v) == "v1"
		})
		assert.Greater(t, proxy.Accepted(), acceptedWhileDown,
			"recovery must come from a new connection, not from a socket that never broke")

		// Writes must recover too, not just reads.
		require.NoError(t, c.Set(ctx, "pk2", "rk2", []byte("v2"), nil))
	})

	// A severed connection must not be mistaken for a permanent fault. The
	// connector marks its bootstrap task failed and the initializer re-runs
	// connectTask, which is the only thing that installs a fresh pool.
	t.Run("a severed connection re-runs the bootstrap task", func(t *testing.T) {
		proxy := newBreakProxy(t, dbAddr)
		c := connectorBehindProxy(t, ctx, proxy, "rerun_task")

		require.NoError(t, c.Set(ctx, "pk", "rk", []byte("v"), nil))
		proxy.Break()
		proxy.Refuse(true)

		waitFor(t, 60*time.Second, 200*time.Millisecond, "the connector to leave the ready state", func() bool {
			for i := 0; i < 3; i++ {
				_, _ = c.Get(ctx, ConnectorMainIndex, "pk", "rk", nil)
			}
			return c.initializer.State() != util.StateReady
		})

		proxy.Refuse(false)
		waitFor(t, 120*time.Second, 500*time.Millisecond, "the connector to become ready again", func() bool {
			return c.initializer.State() == util.StateReady
		})

		got, err := c.Get(ctx, ConnectorMainIndex, "pk", "rk", nil)
		require.NoError(t, err)
		assert.Equal(t, "v", string(got))
	})

	// A watcher must survive the database going away. The LISTEN connection is
	// rebuilt by the listener loop, and the next published value has to reach
	// the same channel the caller is already holding.
	t.Run("a watcher survives a severed connection", func(t *testing.T) {
		proxy := newBreakProxy(t, dbAddr)
		c := connectorBehindProxy(t, ctx, proxy, "watch_survive")

		watchCtx, watchCancel := context.WithCancel(ctx)
		defer watchCancel()

		updates, cleanup, err := c.WatchCounterInt64(watchCtx, "survive")
		require.NoError(t, err)
		defer cleanup()

		// Prove the path works before breaking anything.
		require.NoError(t, c.PublishCounterInt64(ctx, "survive", CounterInt64State{Value: 1, UpdatedAt: 1}))
		requireCounterValue(t, updates, 1, 30*time.Second)

		proxy.Break()

		// Publishing has to work again first — the publish goes through the
		// main pool, which is recovering at the same time as the listener.
		waitFor(t, 90*time.Second, 500*time.Millisecond, "publishing to recover", func() bool {
			return c.PublishCounterInt64(ctx, "survive", CounterInt64State{Value: 2, UpdatedAt: 2}) == nil
		})

		// The listener reconnects on its own, so keep republishing until one
		// lands. A publish that arrives while the LISTEN socket is down is
		// dropped by postgres: pg_notify has no replay.
		deadline := time.Now().Add(90 * time.Second)
		delivered := false
		for time.Now().Before(deadline) && !delivered {
			_ = c.PublishCounterInt64(ctx, "survive", CounterInt64State{Value: 2, UpdatedAt: 2})
			select {
			case st := <-updates:
				if st.Value == 2 {
					delivered = true
				}
			case <-time.After(500 * time.Millisecond):
			}
		}
		assert.True(t, delivered, "a watcher must still receive updates after the connection was severed")
	})

	// A watch started while the database refuses connections must give up when
	// the caller's context ends. The listener connect loop retries forever by
	// design, so the context is the only thing that stops it; without that
	// check the caller blocks for the life of the process.
	t.Run("a watch gives up when its context ends while the database refuses", func(t *testing.T) {
		proxy := newBreakProxy(t, dbAddr)
		c := connectorBehindProxy(t, ctx, proxy, "watch_giveup")

		proxy.Break()
		proxy.Refuse(true)

		watchCtx, watchCancel := context.WithTimeout(ctx, 2*time.Second)
		defer watchCancel()

		done := make(chan error, 1)
		go func() {
			_, _, werr := c.WatchCounterInt64(watchCtx, "giveup")
			done <- werr
		}()

		select {
		case werr := <-done:
			require.Error(t, werr, "a watch that cannot reach the database must not report success")
			assert.Contains(t, werr.Error(), "failed to create listener")
		case <-time.After(60 * time.Second):
			t.Fatal("WatchCounterInt64 never returned after its context ended")
		}
	})
}

// TestPostgreSQLConnector_ListenerPoolIsExhaustedByWatchedKeys pins a leak the
// break proxy made visible.
//
// getOrCreateListener (data/postgresql.go:872) acquires one connection from
// the listener pool per watched key and never releases it. The listener is
// cached in p.listeners for the life of the connector, and WatchCounterInt64's
// cleanup removes only the watcher — never the listener, its goroutine, or its
// connection. So watching maxConns distinct keys exhausts the listener pool
// permanently, even after every watcher has gone.
//
// An operator sees shared-state watches stop working once the connector has
// seen maxConns distinct counter keys, with no error at startup.
//
// This test asserts today's behaviour. A fix makes it fail.
func TestPostgreSQLConnector_ListenerPoolIsExhaustedByWatchedKeys(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dbAddr := startPostgresContainer(t, ctx)
	proxy := newBreakProxy(t, dbAddr)
	c := connectorBehindProxy(t, ctx, proxy, "listener_pool")
	// connectorBehindProxy sets MaxConns to 3, so three keys fill the pool.
	const maxConns = 3

	for i := 0; i < maxConns; i++ {
		_, cleanup, werr := c.WatchCounterInt64(ctx, fmt.Sprintf("exhaust%d", i))
		require.NoError(t, werr, "watch %d must succeed while the listener pool has room", i)
		cleanup() // stop watching immediately; the connection is still held
	}

	// No watcher remains, yet the pool has no connection left to give.
	watchCtx, watchCancel := context.WithTimeout(ctx, 3*time.Second)
	defer watchCancel()

	done := make(chan error, 1)
	go func() {
		_, _, werr := c.WatchCounterInt64(watchCtx, "exhaust-one-too-many")
		done <- werr
	}()

	select {
	case werr := <-done:
		require.Error(t, werr,
			"one watched key past maxConns must fail today: the listener pool is exhausted by "+
				"connections no watcher is using. If this passes because the leak was fixed, "+
				"report it and delete this test")
		assert.Contains(t, werr.Error(), "failed to create listener")
	case <-time.After(90 * time.Second):
		t.Fatal("WatchCounterInt64 never returned after its context ended")
	}
}

// TestPostgreSQLConnector_WatchCleanupLeaksItsFallbackPoller pins upstream bug
// log entry 26 on the PostgreSQL side.
//
// WatchCounterInt64 (data/postgresql.go:665-685) spawns a fallback poller that
// exits only on ctx.Done(). The cleanup function calls ticker.Stop() and
// closes the updates channel, but Stop neither stops that goroutine nor drains
// a tick already delivered. A caller that stops a watch without cancelling its
// context therefore leaks one goroutine per watch — and the leaked goroutine
// can later send on the closed channel, which panics the process.
//
// This test asserts today's behaviour. Fixing the leak makes it fail, which is
// the point: the fix must arrive with the entry, not silently.
func TestPostgreSQLConnector_WatchCleanupLeaksItsFallbackPoller(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dbAddr := startPostgresContainer(t, ctx)
	proxy := newBreakProxy(t, dbAddr)
	c := connectorBehindProxy(t, ctx, proxy, "watch_leak")

	const key = "leaky"
	const watches = 25

	// Warm the listener and its pool first, so the baseline below counts only
	// the per-watch goroutines.
	_, warmCleanup, err := c.WatchCounterInt64(ctx, key)
	require.NoError(t, err)
	warmCleanup()
	time.Sleep(500 * time.Millisecond)

	// Count the poller goroutines by their own stack frame rather than by
	// runtime.NumGoroutine(). A process-wide count also moves when an
	// unrelated goroutine (a pool health checker, a container client) starts
	// or stops between the two samples, which made this test flake at 24 of 25.
	base := countGoroutinesIn(t, pollerFrame)

	cleanups := make([]func(), 0, watches)
	for i := 0; i < watches; i++ {
		_, cl, werr := c.WatchCounterInt64(ctx, key)
		require.NoError(t, werr)
		cleanups = append(cleanups, cl)
	}
	for _, cl := range cleanups {
		cl()
	}

	// Give any goroutine that intends to exit a generous chance to do so.
	time.Sleep(2 * time.Second)
	after := countGoroutinesIn(t, pollerFrame)

	assert.Equal(t, watches, after-base,
		"upstream bug log entry 26: every stopped watch still leaks its fallback poller; "+
			"if this fails because the leak was fixed, update the entry and delete this test")
}

// pollerFrame is the stack frame of the fallback poller WatchCounterInt64
// spawns. Matching on it counts exactly those goroutines and nothing else.
const pollerFrame = "data.(*PostgreSQLConnector).WatchCounterInt64.func1"

// countGoroutinesIn reports how many live goroutines have frame on their stack.
func countGoroutinesIn(t *testing.T, frame string) int {
	t.Helper()
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, len(buf)*2)
	}
	return strings.Count(string(buf), frame)
}

// requireCounterValue waits for one state carrying want on the channel.
func requireCounterValue(t *testing.T, ch <-chan CounterInt64State, want int64, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case st := <-ch:
			if st.Value == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for counter value %d", want)
		}
	}
}
