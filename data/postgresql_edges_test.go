package data

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPostgreSQLConnector_ListenerWaitsForTheMainPool covers the branch a
// watcher hits when it starts before the connector has ever connected.
//
// connectListener derives the listener pool from the main pool's connection
// string, so with no main pool there is nothing to copy. It waits and retries
// forever by design; the caller's context is the ONLY thing that ends the
// wait. Without that check a shared-state watch started during a cold start
// against an unreachable database parks its caller for the life of the process.
//
// No container: the point is what happens when there is no database at all.
func TestPostgreSQLConnector_ListenerWaitsForTheMainPool(t *testing.T) {
	lg := zerolog.New(io.Discard)
	p := &PostgreSQLConnector{logger: &lg, maxConns: 2}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	conn, err := p.connectListener(ctx, "counter_never")
	elapsed := time.Since(start)

	require.Error(t, err, "with no pool to copy, the listener must not claim success")
	assert.Nil(t, conn)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, elapsed, 30*time.Second, "the wait must end with the context, not run on")
}

// startPostgresRaw brings up a postgres container and returns its connection
// URI, so a test can shape the database BEFORE the connector ever sees it.
func startPostgresRaw(t *testing.T, ctx context.Context) string {
	t.Helper()
	addr := startPostgresContainer(t, ctx)
	return fmt.Sprintf("postgres://postgres:password@%s/postgres?sslmode=disable", addr)
}

// TestPostgreSQLConnector_MigratesALegacyTextValueColumn covers the in-place
// upgrade path for a database created by an older erpc, whose `value` column
// is TEXT rather than BYTEA.
//
// This is a one-way migration of live operator data. If it silently drops or
// corrupts rows, an operator loses their cache and their API keys with it, and
// the only signal is an INFO log line.
func TestPostgreSQLConnector_MigratesALegacyTextValueColumn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	uri := startPostgresRaw(t, ctx)
	const table = "legacy_text_table"

	admin, err := pgxpool.Connect(ctx, uri)
	require.NoError(t, err)
	defer admin.Close()

	// A table exactly as an older erpc left it: value is TEXT and there is no
	// expires_at column yet.
	_, err = admin.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s (
			partition_key TEXT,
			range_key TEXT,
			value TEXT,
			PRIMARY KEY (partition_key, range_key)
		)`, table))
	require.NoError(t, err)
	_, err = admin.Exec(ctx,
		fmt.Sprintf(`INSERT INTO %s (partition_key, range_key, value) VALUES ($1,$2,$3)`, table),
		"pk-old", "rk-old", "legacy-value")
	require.NoError(t, err)

	logger := zerolog.New(io.Discard)
	c, err := NewPostgreSQLConnector(ctx, &logger, "pg-legacy", &common.PostgreSQLConnectorConfig{
		Table:         table,
		ConnectionUri: uri,
		InitTimeout:   common.Duration(60 * time.Second),
		GetTimeout:    common.Duration(30 * time.Second),
		SetTimeout:    common.Duration(30 * time.Second),
		MinConns:      1,
		MaxConns:      3,
	})
	require.NoError(t, err)
	require.Equal(t, util.StateReady, c.initializer.State(),
		"a legacy table must not stop the connector coming up: %v", c.initializer.Errors())

	var dataType string
	require.NoError(t, admin.QueryRow(ctx, `
		SELECT data_type FROM information_schema.columns
		WHERE table_name = $1 AND column_name = 'value'`, table).Scan(&dataType))
	assert.Equal(t, "bytea", dataType, "the value column must end up as BYTEA")

	var hasExpiresAt bool
	require.NoError(t, admin.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = $1 AND column_name = 'expires_at'
		)`, table).Scan(&hasExpiresAt))
	assert.True(t, hasExpiresAt, "the TTL column must be added to a legacy table")

	got, err := c.Get(ctx, ConnectorMainIndex, "pk-old", "rk-old", nil)
	require.NoError(t, err, "the pre-existing row must survive the migration")
	assert.Equal(t, "legacy-value", string(got))

	// The migrated table must still work for new writes.
	require.NoError(t, c.Set(ctx, "pk-new", "rk-new", []byte("new-value"), nil))
	got, err = c.Get(ctx, ConnectorMainIndex, "pk-new", "rk-new", nil)
	require.NoError(t, err)
	assert.Equal(t, "new-value", string(got))
}

// TestPostgreSQLConnector_UnusableTableNameFailsAtStartup: the table name comes
// straight from operator config and is interpolated into the DDL unquoted, so a
// name that is not a bare identifier produces a syntax error. The connector must
// stay out of the ready state rather than accept traffic it cannot serve.
func TestPostgreSQLConnector_UnusableTableNameFailsAtStartup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	uri := startPostgresRaw(t, ctx)
	logger := zerolog.New(io.Discard)

	c, err := NewPostgreSQLConnector(ctx, &logger, "pg-badtable", &common.PostgreSQLConnectorConfig{
		Table:         "erpc cache", // a space makes this not a bare identifier
		ConnectionUri: uri,
		InitTimeout:   common.Duration(10 * time.Second),
		GetTimeout:    common.Duration(5 * time.Second),
		SetTimeout:    common.Duration(5 * time.Second),
		MinConns:      1,
		MaxConns:      2,
	})
	require.NoError(t, err, "the constructor reports the failure through the initializer, not here")
	require.NotNil(t, c)

	assert.NotEqual(t, util.StateReady, c.initializer.State(),
		"a table the connector cannot create must not leave it looking healthy")
	require.Error(t, c.initializer.Errors())

	_, err = c.Get(ctx, ConnectorMainIndex, "pk", "rk", nil)
	assert.Error(t, err, "a connector that never applied its schema must refuse reads")
}

// TestPostgreSQLConnector_LockRefusedBeforeTheFirstConnect: Lock is the first
// thing a shared-state writer calls. Before any pool exists it must refuse with
// the typed not-ready error, so callers can tell "still starting up" from a
// transport failure and do not re-trigger the reconnect cascade.
//
// No container: the point is what happens with no database behind the connector.
func TestPostgreSQLConnector_LockRefusedBeforeTheFirstConnect(t *testing.T) {
	lg := zerolog.New(io.Discard)
	p := &PostgreSQLConnector{logger: &lg}

	lock, err := p.Lock(context.Background(), "any-key", 5*time.Second)
	require.Error(t, err, "a connector with no pool must never report a held lock")
	assert.Nil(t, lock)
	assert.ErrorIs(t, err, ErrConnectorNotReady)
}

// TestPostgreSQLConnector_LockAndUnlockFailurePaths covers what a caller sees
// when the database is unreachable or the lock is used twice.
//
// The distributed lock guards shared-state writes across erpc instances.
// A Lock that reports success without holding anything, or an Unlock that
// reports success without committing, lets two instances both believe they own
// the same key.
func TestPostgreSQLConnector_LockAndUnlockFailurePaths(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	dbAddr := startPostgresContainer(t, ctx)

	t.Run("unlocking twice reports no active transaction", func(t *testing.T) {
		proxy := newBreakProxy(t, dbAddr)
		c := connectorBehindProxy(t, ctx, proxy, "lock_twice")

		lock, err := c.Lock(ctx, "double-unlock", 5*time.Second)
		require.NoError(t, err)
		require.NoError(t, lock.Unlock(ctx))

		err = lock.Unlock(ctx)
		require.Error(t, err, "a released lock must not report a second successful release")
		assert.Contains(t, err.Error(), "no active transaction")
	})

	t.Run("locking against an unreachable database fails", func(t *testing.T) {
		proxy := newBreakProxy(t, dbAddr)
		c := connectorBehindProxy(t, ctx, proxy, "lock_down")

		// Prove the path works first, so the failure below is the database
		// going away and not a broken fixture.
		lock, err := c.Lock(ctx, "warmup", 5*time.Second)
		require.NoError(t, err)
		require.NoError(t, lock.Unlock(ctx))

		proxy.Break()
		proxy.Refuse(true)

		waitFor(t, 60*time.Second, 200*time.Millisecond, "locking to start failing", func() bool {
			l, lerr := c.Lock(ctx, "while-down", 5*time.Second)
			if lerr == nil {
				_ = l.Unlock(ctx)
				return false
			}
			return true
		})

		_, err = c.Lock(ctx, "while-down", 5*time.Second)
		require.Error(t, err, "a lock nobody can hold must never be reported as held")
	})

	t.Run("unlocking after the connection dies reports the commit failure", func(t *testing.T) {
		proxy := newBreakProxy(t, dbAddr)
		c := connectorBehindProxy(t, ctx, proxy, "unlock_broken")

		lock, err := c.Lock(ctx, "commit-fails", 5*time.Second)
		require.NoError(t, err)

		// The database goes away while the lock is held. The commit that
		// would release the advisory lock can no longer reach it.
		require.Positive(t, proxy.Break())
		proxy.Refuse(true)

		err = lock.Unlock(ctx)
		require.Error(t, err, "a commit that never reached the database must not report success")
		assert.Contains(t, err.Error(), "failed to commit transaction")
	})
}
