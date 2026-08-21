package data

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// One postgres container serves every subtest in this file. Each subtest works
// in its own table or its own database, so they cannot disturb one another.

// recordingLogger returns a trace-level logger and a function that reports
// every message it has written. Several paths below distinguish themselves
// only by which line they log, so the log is the evidence that the intended
// branch ran and not its neighbour.
//
// Other tests in this package call util.ConfigureTestLogger, which disables
// logging globally, so the global level is raised for the duration of the
// test and put back afterwards.
func recordingLogger(t *testing.T) (*zerolog.Logger, func() []string) {
	t.Helper()
	previous := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.TraceLevel)
	t.Cleanup(func() { zerolog.SetGlobalLevel(previous) })

	rec := &lineRecorder{}
	lg := zerolog.New(rec).Level(zerolog.TraceLevel)
	return &lg, rec.lines
}

type lineRecorder struct {
	mu  sync.Mutex
	buf []string
}

func (r *lineRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	r.buf = append(r.buf, string(p))
	r.mu.Unlock()
	return len(p), nil
}

func (r *lineRecorder) lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.buf))
	copy(out, r.buf)
	return out
}

func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}

// adminPool opens a direct pool to the database, bypassing the connector, so a
// test can shape the schema the connector will meet.
func adminPool(t *testing.T, ctx context.Context, uri string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.Connect(ctx, uri)
	require.NoError(t, err, "the test could not reach the database directly")
	t.Cleanup(pool.Close)
	return pool
}

// newConnector builds a connector against the given URI and waits for it to
// come up.
func newConnector(t *testing.T, ctx context.Context, lg *zerolog.Logger, id, uri, table string, skipSchema bool) *PostgreSQLConnector {
	t.Helper()
	c, err := NewPostgreSQLConnector(ctx, lg, id, &common.PostgreSQLConnectorConfig{
		Table:           table,
		ConnectionUri:   uri,
		InitTimeout:     common.Duration(20 * time.Second),
		GetTimeout:      common.Duration(5 * time.Second),
		SetTimeout:      common.Duration(5 * time.Second),
		MinConns:        1,
		MaxConns:        10,
		SkipSchemaSetup: skipSchema,
	})
	require.NoError(t, err)
	require.Equal(t, util.StateReady, c.initializer.State(), "the connector must come up")
	return c
}

func postgresURI(addr, db string) string {
	return fmt.Sprintf("postgres://postgres:password@%s/%s?sslmode=disable", addr, db)
}

// TestPostgreSQLConnector_DatabasePaths exercises the connector's data paths
// against a real database: the detailed-tracing attributes an operator reads in
// a span, every error path a missing table produces, and the counter helpers.
func TestPostgreSQLConnector_DatabasePaths(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	addr := startPostgresContainer(t, ctx)
	uri := postgresURI(addr, "postgres")

	// A round trip through every entry point, with detailed tracing on so the
	// attribute-building branches run too.
	//
	// The assertions are on the data, not on the span: `tracer` is unexported
	// and set once through common.InitializeTracing, so no test outside the
	// common package can install a recording tracer. Removing an attribute
	// therefore goes undetected here; breaking a read or a write does not.
	t.Run("every entry point round-trips with detailed tracing on", func(t *testing.T) {
		// Only the detail flag is raised. No tracer is installed in this
		// binary, so the spans stay no-ops; the flag alone is what decides
		// whether the connector builds the attributes at all.
		prevDetailed := common.IsTracingDetailed
		common.IsTracingDetailed = true
		t.Cleanup(func() { common.IsTracingDetailed = prevDetailed })

		lg := zerolog.New(io.Discard)
		c := newConnector(t, ctx, &lg, "pg-traced", uri, "traced_items", false)

		// A value over the connector's own 1 KiB logging threshold, so the
		// large-value branch runs too.
		big := make([]byte, 2048)
		for i := range big {
			big[i] = 'x'
		}
		require.NoError(t, c.Set(ctx, "pk-a", "rk-1", big, nil))
		require.NoError(t, c.Set(ctx, "pk-a", "rk-2", []byte("small"), nil))

		got, err := c.Get(ctx, ConnectorMainIndex, "pk-a", "rk-2", nil)
		require.NoError(t, err)
		require.Equal(t, "small", string(got))

		// Wildcard reads go through getWithWildcard, which has its own span.
		wild, err := c.Get(ctx, ConnectorMainIndex, "pk-a", "rk-*", nil)
		require.NoError(t, err)
		require.NotEmpty(t, wild)

		lock, err := c.Lock(ctx, "traced-lock", time.Second)
		require.NoError(t, err)
		require.NoError(t, lock.Unlock(ctx))

		items, _, err := c.List(ctx, ConnectorMainIndex, 10, "")
		require.NoError(t, err)
		require.Len(t, items, 2)

		require.NoError(t, c.PublishCounterInt64(ctx, "traced-counter", CounterInt64State{
			Value: 7, UpdatedAt: time.Now().UnixMilli(), UpdatedBy: "test",
		}))

		require.NoError(t, c.Delete(ctx, "pk-a", "rk-1"))
	})

	// Every write and read path reports the database's own failure instead of
	// swallowing it. A dropped table stands in for any schema-level fault: the
	// error must reach the caller, and it must NOT be mistaken for a broken
	// socket, because that would tear the pool down and reconnect.
	t.Run("a missing table surfaces on every path", func(t *testing.T) {
		lg := zerolog.New(io.Discard)
		c := newConnector(t, ctx, &lg, "pg-missing", uri, "vanishing_items", false)

		admin := adminPool(t, ctx, uri)
		_, err := admin.Exec(ctx, "DROP TABLE vanishing_items")
		require.NoError(t, err)

		require.Error(t, c.Set(ctx, "pk", "rk", []byte("v"), nil))
		require.Error(t, c.Delete(ctx, "pk", "rk"))
		require.Error(t, c.cleanupExpired(ctx))

		_, _, listErr := c.List(ctx, ConnectorMainIndex, 5, "")
		require.Error(t, listErr)

		_, wildErr := c.Get(ctx, ConnectorMainIndex, "pk", "rk-*", nil)
		require.Error(t, wildErr)
		assert.False(t, common.HasErrorCode(wildErr, common.ErrCodeRecordNotFound),
			"a missing table is a failure, not an empty cache")

		// A counter read must pass the failure up. Reporting "no value" would
		// let a watcher seed itself with a zero it never observed.
		_, ok, counterErr := c.getCurrentValue(ctx, "pk")
		require.Error(t, counterErr)
		assert.False(t, ok)

		// The connector must stay up: a schema error is not a transport error.
		assert.Equal(t, util.StateReady, c.initializer.State(),
			"a missing table must not trigger a reconnect")
	})

	// getCurrentValue seeds a watcher with the counter's present value. A
	// stored value that is not a counter must read as "nothing there" rather
	// than as an error, or a single bad row would break every watcher on the
	// key.
	t.Run("a counter read tolerates a value that is not a counter", func(t *testing.T) {
		lg := zerolog.New(io.Discard)
		c := newConnector(t, ctx, &lg, "pg-counter", uri, "counter_items", false)

		good := CounterInt64State{Value: 42, UpdatedAt: time.Now().UnixMilli(), UpdatedBy: "test"}
		payload, err := common.SonicCfg.Marshal(good)
		require.NoError(t, err)
		require.NoError(t, c.Set(ctx, "good-key", "value", payload, nil))

		st, ok, err := c.getCurrentValue(ctx, "good-key")
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, int64(42), st.Value)

		require.NoError(t, c.Set(ctx, "junk-key", "value", []byte("not json"), nil))
		_, ok, err = c.getCurrentValue(ctx, "junk-key")
		require.NoError(t, err, "an unparseable row must not fail the read")
		assert.False(t, ok, "an unparseable row must read as absent")
	})

	// pg_notify rejects a payload over 8000 bytes. The publish must return that
	// failure so the caller knows the update never reached the other nodes.
	t.Run("an oversized counter update reports the publish failure", func(t *testing.T) {
		lg := zerolog.New(io.Discard)
		c := newConnector(t, ctx, &lg, "pg-publish", uri, "publish_items", false)

		err := c.PublishCounterInt64(ctx, "huge", CounterInt64State{
			Value:     1,
			UpdatedAt: time.Now().UnixMilli(),
			UpdatedBy: strings.Repeat("n", 9000),
		})
		require.Error(t, err, "a payload postgres cannot carry must not report success")
	})

	// The local cleanup routine is what keeps expired rows from accumulating
	// when pg_cron is absent. It must delete on every tick, and a failed delete
	// must be reported and must not stop the routine — a cleanup loop that
	// exits on the first error leaves the table growing without a signal.
	t.Run("the cleanup routine deletes expired rows and survives a failure", func(t *testing.T) {
		lg, lines := recordingLogger(t)
		// The real connector only creates the table; its own 5-minute cleanup
		// goroutine is left alone. The routine under test runs on a separate
		// connector so the ticker can be short without racing the first one.
		_ = newConnector(t, ctx, lg, "pg-cleanup-schema", uri, "cleanup_items", false)
		admin := adminPool(t, ctx, uri)

		_, err := admin.Exec(ctx, `INSERT INTO cleanup_items (partition_key, range_key, value, expires_at)
			VALUES ('old-1', 'rk', 'v', NOW() - INTERVAL '1 hour'),
			       ('old-2', 'rk', 'v', NOW() - INTERVAL '1 hour')`)
		require.NoError(t, err)

		fast := &PostgreSQLConnector{
			logger:        lg,
			table:         "cleanup_items",
			conn:          admin,
			cleanupTicker: time.NewTicker(10 * time.Millisecond),
		}
		defer fast.cleanupTicker.Stop()
		cleanupCtx, stopCleanup := context.WithCancel(ctx)
		defer stopCleanup()
		go fast.startCleanup(cleanupCtx)

		waitFor(t, 30*time.Second, 20*time.Millisecond, "the expired rows to be deleted", func() bool {
			var n int
			if err := admin.QueryRow(ctx, "SELECT count(*) FROM cleanup_items").Scan(&n); err != nil {
				return false
			}
			return n == 0
		})

		_, err = admin.Exec(ctx, "DROP TABLE cleanup_items")
		require.NoError(t, err)

		waitFor(t, 30*time.Second, 20*time.Millisecond, "the failed cleanup to be reported", func() bool {
			return containsLine(lines(), "failed to cleanup expired items")
		})
	})

	// List asks for one row more than the caller wants so it can tell whether a
	// next page exists. Pin what it actually returns.
	t.Run("List truncates at the limit and never offers a next page", func(t *testing.T) {
		lg := zerolog.New(io.Discard)
		c := newConnector(t, ctx, &lg, "pg-list", uri, "paged_items", false)

		for i := 0; i < 7; i++ {
			require.NoError(t, c.Set(ctx, fmt.Sprintf("pk-%02d", i), "rk", []byte("v"), nil))
		}

		items, token, err := c.List(ctx, ConnectorMainIndex, 3, "")
		require.NoError(t, err)
		assert.Len(t, items, 3, "the caller's limit is respected")
		// Bug 61: the extra probe row was already consumed by the scan loop, so
		// the has-more check always fails and the caller is told the listing is
		// complete when four rows remain.
		assert.Empty(t, token, "bug 61: no next-page token is ever produced")

		// A supplied token still works, which is how a caller would page if a
		// token were ever handed out.
		second, _, err := c.List(ctx, ConnectorMainIndex, 3, encodeOffsetToken(t, 3))
		require.NoError(t, err)
		require.Len(t, second, 3)
		assert.NotEqual(t, items[0].PartitionKey, second[0].PartitionKey,
			"the offset token must move the window")
	})
}

// TestPostgreSQLConnector_SchemaAndListenerPaths covers the connector's
// start-up DDL and its LISTEN plumbing. Both need a database in a shape the
// happy path never produces, so each subtest builds its own.
func TestPostgreSQLConnector_SchemaAndListenerPaths(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	addr := startPostgresContainer(t, ctx)
	admin := adminPool(t, ctx, postgresURI(addr, "postgres"))

	// makeDB creates a database whose search path puts `public` ahead of
	// `pg_catalog`. Objects created in `public` then shadow the catalog, which
	// is how these tests present a pg_cron installation to a stock postgres
	// image without the extension. The connector's own query text is
	// unchanged; only what it resolves to differs.
	makeDB := func(t *testing.T, name string) *pgxpool.Pool {
		t.Helper()
		_, err := admin.Exec(ctx, "CREATE DATABASE "+name)
		require.NoError(t, err)
		_, err = admin.Exec(ctx, "ALTER DATABASE "+name+" SET search_path TO public, pg_catalog")
		require.NoError(t, err)
		return adminPool(t, ctx, postgresURI(addr, name))
	}

	// With pg_cron present, the database itself deletes expired rows, so the
	// connector must stand down its local cleanup ticker. Two connectors both
	// deleting is wasted write load on the exact table the cache hammers.
	t.Run("pg_cron takes over the expiry cleanup", func(t *testing.T) {
		db := makeDB(t, "pgcron_ok")
		_, err := db.Exec(ctx, `CREATE TABLE public.pg_extension (extname text)`)
		require.NoError(t, err)
		_, err = db.Exec(ctx, `INSERT INTO public.pg_extension VALUES ('pg_cron')`)
		require.NoError(t, err)
		_, err = db.Exec(ctx, `CREATE SCHEMA cron`)
		require.NoError(t, err)
		_, err = db.Exec(ctx, `CREATE FUNCTION cron.schedule(text, text) RETURNS bigint
			AS 'SELECT 1::bigint' LANGUAGE sql`)
		require.NoError(t, err)

		lg := zerolog.New(io.Discard)
		c := newConnector(t, ctx, &lg, "pg-cron-ok", postgresURI(addr, "pgcron_ok"), "cron_items", false)
		assert.Nil(t, c.cleanupTicker,
			"with the cron job scheduled the connector must not also delete rows itself")
	})

	// When scheduling the cron job fails, the connector must keep its own
	// ticker. Standing down on a job that was never created would leave expired
	// rows in the table forever.
	t.Run("a failed cron schedule keeps the local cleanup", func(t *testing.T) {
		db := makeDB(t, "pgcron_fail")
		_, err := db.Exec(ctx, `CREATE TABLE public.pg_extension (extname text)`)
		require.NoError(t, err)
		_, err = db.Exec(ctx, `INSERT INTO public.pg_extension VALUES ('pg_cron')`)
		require.NoError(t, err)
		// No cron schema: the schedule call fails.

		lg, lines := recordingLogger(t)
		c := newConnector(t, ctx, lg, "pg-cron-fail", postgresURI(addr, "pgcron_fail"), "cron_items", false)
		require.NotNil(t, c.cleanupTicker,
			"a cron job that was never created must not disarm the local cleanup")
		c.cleanupTicker.Stop()
		assert.True(t, containsLine(lines(), "failed to create pg_cron cleanup job"),
			"the operator must be told the cron job did not happen")
	})

	// A pg_extension the connector cannot query must not stop start-up. The
	// cache works fine without pg_cron; refusing to connect over a failed
	// capability probe would take the whole connector down.
	t.Run("an unqueryable extension catalog does not stop start-up", func(t *testing.T) {
		db := makeDB(t, "pgcron_probe")
		// A pg_extension without the column the probe reads.
		_, err := db.Exec(ctx, `CREATE TABLE public.pg_extension (other text)`)
		require.NoError(t, err)

		lg, lines := recordingLogger(t)
		c := newConnector(t, ctx, lg, "pg-cron-probe", postgresURI(addr, "pgcron_probe"), "cron_items", false)
		require.NotNil(t, c.cleanupTicker, "the local cleanup stays armed when pg_cron is unknown")
		c.cleanupTicker.Stop()
		assert.True(t, containsLine(lines(), "failed to check for pg_cron extension"))

		// Start-up completed: the table is there and usable.
		require.NoError(t, c.Set(ctx, "pk", "rk", []byte("v"), nil))
	})

	// A role that may not take advisory locks must get an error, not a lock it
	// does not hold. Shared-state coordination is built on this lock; a silent
	// success here would let two nodes believe they each own the same key.
	t.Run("a role denied the advisory lock cannot take one", func(t *testing.T) {
		db := makeDB(t, "lockdown")
		_, err := admin.Exec(ctx, `DROP ROLE IF EXISTS lockless`)
		require.NoError(t, err)
		_, err = admin.Exec(ctx, `CREATE ROLE lockless LOGIN PASSWORD 'lockless'`)
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = admin.Exec(context.Background(), `DROP ROLE IF EXISTS lockless`) })
		_, err = admin.Exec(ctx, `GRANT CONNECT ON DATABASE lockdown TO lockless`)
		require.NoError(t, err)
		// Database-scoped: only this database's catalog entry changes.
		_, err = db.Exec(ctx, `REVOKE EXECUTE ON FUNCTION pg_catalog.pg_try_advisory_xact_lock(bigint) FROM PUBLIC`)
		require.NoError(t, err)

		lg := zerolog.New(io.Discard)
		uri := fmt.Sprintf("postgres://lockless:lockless@%s/lockdown?sslmode=disable", addr)
		// SkipSchemaSetup: the role owns nothing and must not try to run DDL.
		c := newConnector(t, ctx, &lg, "pg-lockless", uri, "lock_items", true)

		lock, err := c.Lock(ctx, "some-key", time.Second)
		require.Error(t, err, "a denied advisory lock must not read as acquired")
		assert.Nil(t, lock)
		assert.Contains(t, err.Error(), "failed to acquire advisory lock")
	})

	// Two watchers starting at once both find no listener pool and both build
	// one. Exactly one may be installed; the loser must close the pool it
	// built, or its connections sit open against the database until the
	// process ends.
	t.Run("only one listener pool survives a concurrent build", func(t *testing.T) {
		lg := zerolog.New(io.Discard)
		// pool_min_conns makes each listener pool open real connections on
		// construction, so both builders are genuinely in flight at once.
		uri := postgresURI(addr, "postgres") + "&pool_min_conns=8"
		c := newConnector(t, ctx, &lg, "pg-listener-race", uri, "listener_race_items", false)
		require.Nil(t, c.listenerPool, "the listener pool is built lazily")

		const builders = 2
		var start sync.WaitGroup
		var done sync.WaitGroup
		start.Add(1)
		done.Add(builders)
		conns := make([]*pgxpool.Conn, builders)
		errs := make([]error, builders)
		for n := 0; n < builders; n++ {
			go func(n int) {
				defer done.Done()
				start.Wait()
				conns[n], errs[n] = c.connectListener(ctx, fmt.Sprintf("counter_race_%d", n))
			}(n)
		}
		start.Done()
		done.Wait()

		for n := range errs {
			require.NoError(t, errs[n])
			require.NotNil(t, conns[n])
			conns[n].Release()
		}
		require.NotNil(t, c.listenerPool, "one pool must be installed")

		// The surviving pool is the only one still holding connections. Anything
		// the loser built was closed, so the total stays within one pool's cap.
		var open int
		require.NoError(t, admin.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE datname = 'postgres' AND application_name <> 'psql'`,
		).Scan(&open))
		t.Logf("open connections after the race: %d", open)
		// One listener pool holds 8; a second, orphaned one would push this past 16.
		assert.LessOrEqual(t, open, 14, "an orphaned listener pool would leave its connections open")
	})

	// A listener that acquires a connection the database has already dropped
	// must retry rather than report a subscription it does not have. A watcher
	// built on a dead LISTEN never receives a notification and silently falls
	// back to 30-second polling.
	t.Run("a LISTEN on a dead connection is retried", func(t *testing.T) {
		proxy := newBreakProxy(t, addr)
		lg, lines := recordingLogger(t)
		c := newConnector(t, ctx, lg,
			"pg-listen-retry",
			fmt.Sprintf("postgres://postgres:password@%s/postgres?sslmode=disable", proxy.Addr()),
			"listen_retry_items", false)

		first, err := c.connectListener(ctx, "counter_alpha")
		require.NoError(t, err)
		// Hand the connection back so the pool holds it idle. pgxpool only
		// re-checks a connection that has been idle for over a second, so the
		// next acquire returns this one without noticing it is dead.
		first.Release()
		require.Positive(t, proxy.Break(), "the listener pool must have had a live connection")

		second, err := c.connectListener(ctx, "counter_beta")
		require.NoError(t, err, "the listener must recover, not give up")
		require.NotNil(t, second)
		second.Release()

		assert.True(t, containsLine(lines(), "failed to listen to postgres channel, will retry"),
			"the retry must come from the LISTEN itself, not from the acquire above it")
	})
}

// encodeOffsetToken builds the pagination token List would emit for an offset.
func encodeOffsetToken(t *testing.T, offset int) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(`{"offset":%d}`, offset)))
}
