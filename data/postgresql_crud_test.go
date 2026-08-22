package data

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
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

// startPostgresConnector brings up one postgres container and returns a ready
// connector against a table named after the caller. One container serves all
// the subtests below, because container startup dominates the run time.
func startPostgresConnector(t *testing.T, table string) (context.Context, *PostgreSQLConnector) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	t.Cleanup(cancel)

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

	logger := zerolog.New(io.Discard)
	cfg := &common.PostgreSQLConnectorConfig{
		Table:         table,
		ConnectionUri: fmt.Sprintf("postgres://postgres:password@%s:%s/postgres?sslmode=disable", host, port.Port()),
		InitTimeout:   common.Duration(60 * time.Second),
		GetTimeout:    common.Duration(30 * time.Second),
		SetTimeout:    common.Duration(30 * time.Second),
		MinConns:      1,
		MaxConns:      5,
	}

	connector, err := NewPostgreSQLConnector(ctx, &logger, "pg-crud", cfg)
	require.NoError(t, err)
	require.Equal(t, util.StateReady, connector.initializer.State(), "the connector must be ready")

	return ctx, connector
}

// offsetToken builds the pagination token shape List parses: base64 of
// {"offset":N}. Tests use it to reach a later page directly, because List
// itself never hands one out (see the pinned defect below).
func offsetToken(t *testing.T, offset int) string {
	t.Helper()
	raw, err := json.Marshal(map[string]int{"offset": offset})
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(raw)
}

// TestSanitizeChannelName pins the mapping from an arbitrary counter key to a
// legal postgres identifier. A key that produced an illegal identifier would
// make every LISTEN and pg_notify for that counter fail at the database.
func TestSanitizeChannelName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"already legal", "counter_latest", "counter_latest"},
		{"colons become underscores", "counter_latestBlock:evm:123", "counter_latestBlock_evm_123"},
		{"dashes and dots become underscores", "a-b.c", "a_b_c"},
		{"leading digit gets an underscore prefix", "1counter", "_1counter"},
		{"leading illegal char becomes a legal underscore", ":counter", "_counter"},
		{"empty stays empty", "", ""},
		{"unicode is replaced", "cnt€r", "cnt_r"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, sanitizeChannelName(tc.in))
		})
	}

	t.Run("long keys are truncated to the postgres identifier limit", func(t *testing.T) {
		got := sanitizeChannelName(strings.Repeat("k", 100))
		assert.Len(t, got, 63, "postgres refuses identifiers longer than 63 bytes")
		assert.Equal(t, strings.Repeat("k", 63), got)
	})

	t.Run("every produced name is a legal identifier", func(t *testing.T) {
		for _, in := range []string{"counter_latestBlock:evm:1", "9-lead", "@@@", strings.Repeat("x:", 60)} {
			got := sanitizeChannelName(in)
			require.NotEmpty(t, got)
			first := got[0]
			assert.True(t, first == '_' || (first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z'),
				"identifier %q must start with a letter or underscore", got)
			for i := 0; i < len(got); i++ {
				c := got[i]
				legal := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
				assert.True(t, legal, "identifier %q contains the illegal byte %q", got, string(c))
			}
		}
	})
}

// TestPostgreSQLConnector_CRUD covers the connector methods that need a live
// database: Id, Delete, List, wildcard Get, counter watch/publish and the
// expiry sweep. They share one container.
func TestPostgreSQLConnector_CRUD(t *testing.T) {
	ctx, connector := startPostgresConnector(t, "pg_crud_table")

	t.Run("Id reports the configured connector id", func(t *testing.T) {
		assert.Equal(t, "pg-crud", connector.Id())
	})

	t.Run("Delete removes one row and leaves the rest", func(t *testing.T) {
		require.NoError(t, connector.Set(ctx, "del-pk", "keep", []byte("keep-me"), nil))
		require.NoError(t, connector.Set(ctx, "del-pk", "drop", []byte("drop-me"), nil))

		got, err := connector.Get(ctx, ConnectorMainIndex, "del-pk", "drop", nil)
		require.NoError(t, err)
		require.Equal(t, []byte("drop-me"), got, "the row must exist before the delete")

		require.NoError(t, connector.Delete(ctx, "del-pk", "drop"))

		_, err = connector.Get(ctx, ConnectorMainIndex, "del-pk", "drop", nil)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound))

		kept, err := connector.Get(ctx, ConnectorMainIndex, "del-pk", "keep", nil)
		require.NoError(t, err)
		assert.Equal(t, []byte("keep-me"), kept, "the sibling row must survive")

		assert.NoError(t, connector.Delete(ctx, "no-such-pk", "no-such-rk"),
			"deleting an absent row must not be an error")
	})

	t.Run("Get with a wildcard range key resolves to the stored row", func(t *testing.T) {
		require.NoError(t, connector.Set(ctx, "wild-pk", "user-42", []byte(`{"userId":"user-42"}`), nil))

		got, err := connector.Get(ctx, ConnectorMainIndex, "wild-pk", "*", nil)
		require.NoError(t, err, "postgres expands '*' into a SQL LIKE match")
		assert.JSONEq(t, `{"userId":"user-42"}`, string(got))

		_, err = connector.Get(ctx, ConnectorMainIndex, "wild-pk-absent", "*", nil)
		require.Error(t, err, "a wildcard over a partition key with no rows must miss")
		assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound))
	})

	// PostgreSQL is the only store where API keys worked before the record
	// moved to a fixed address, so it is the only store with anything to
	// migrate. It expands "*" into a LIKE match, which is why a read at the new
	// address still finds a row an older eRPC left under the user id. That is
	// what lets an existing deployment upgrade without touching its data.
	t.Run("an API-key read reaches a record written under either layout", func(t *testing.T) {
		legacy := []byte(`{"userId":"alice","enabled":true}`)
		require.NoError(t, connector.Set(ctx, "sk_legacy", "alice", legacy, nil))

		got, err := connector.Get(ctx, ConnectorMainIndex, "sk_legacy", ConnectorApiKeyRangeKey, nil)
		require.NoError(t, err, "a record left at the user id must still resolve after the upgrade")
		assert.JSONEq(t, string(legacy), string(got))

		current := []byte(`{"userId":"bob","enabled":true}`)
		require.NoError(t, connector.Set(ctx, "sk_current", ConnectorApiKeyRangeKey, current, nil))

		got, err = connector.Get(ctx, ConnectorMainIndex, "sk_current", ConnectorApiKeyRangeKey, nil)
		require.NoError(t, err, "a record written at the fixed address must resolve")
		assert.JSONEq(t, string(current), string(got))

		// Revoking has to clear both addresses. Leaving the legacy row behind
		// would let the next read resolve it and keep the key working.
		require.NoError(t, connector.Delete(ctx, "sk_legacy", ConnectorApiKeyRangeKey))
		require.NoError(t, connector.Delete(ctx, "sk_legacy", "alice"))
		_, err = connector.Get(ctx, ConnectorMainIndex, "sk_legacy", ConnectorApiKeyRangeKey, nil)
		require.Error(t, err, "a revoked key must leave no row behind")
		assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound))
	})

	t.Run("Get with a wildcard partition key uses the reverse index", func(t *testing.T) {
		require.NoError(t, connector.Set(ctx, "evm:7:0x1f", "eth_getBlockByNumber", []byte("blk"), nil))

		got, err := connector.Get(ctx, ConnectorReverseIndex, "evm:7:*", "eth_getBlockByNumber", nil)
		require.NoError(t, err)
		assert.Equal(t, []byte("blk"), got)

		_, err = connector.Get(ctx, ConnectorReverseIndex, "evm:8:*", "eth_getBlockByNumber", nil)
		require.Error(t, err, "a wildcard over an unwritten network must miss")
		assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound))
	})

	t.Run("List returns one page of rows honouring the limit", func(t *testing.T) {
		const total = 40
		for i := 0; i < total; i++ {
			require.NoError(t, connector.Set(ctx, fmt.Sprintf("list-pk-%02d", i), "rk",
				[]byte(fmt.Sprintf("v%02d", i)), nil))
		}

		results, _, err := connector.List(ctx, ConnectorMainIndex, 7, "")
		require.NoError(t, err)
		require.Len(t, results, 7, "a page must contain exactly the requested number of rows")
		for _, kv := range results {
			assert.NotEmpty(t, kv.PartitionKey)
			assert.NotEmpty(t, kv.RangeKey)
			assert.NotEmpty(t, kv.Value, "each listed row must carry its stored value")
		}

		// Rows are ordered by (partition_key, range_key), so an explicit offset
		// token reaches a later page and returns different rows.
		second, _, err := connector.List(ctx, ConnectorMainIndex, 7, offsetToken(t, 7))
		require.NoError(t, err)
		require.Len(t, second, 7)
		assert.NotEqual(t, results[0].PartitionKey, second[0].PartitionKey,
			"an explicit offset must reach a different page")

		all, _, err := connector.List(ctx, ConnectorMainIndex, 1000, "")
		require.NoError(t, err)
		listed := 0
		for _, kv := range all {
			if strings.HasPrefix(kv.PartitionKey, "list-pk-") {
				listed++
			}
		}
		assert.Equal(t, total, listed, "a large enough limit must return every row")
	})

	// List pagination — the token is the ONLY way a caller learns there is
	// more to read. It used to always come back empty: the query asked for
	// limit+1 rows so the scan loop could detect a next page, the loop's
	// break consumed that extra row without recording it, and the probe
	// below the loop then called rows.Next() a second time — which needs a
	// limit+2-th row the query never fetched. So erpc_listApiKeys on a
	// PostgreSQL store returned one page and reported "no more pages", and
	// a caller looping until the token is empty stopped there believing it
	// had seen everything.
	//
	// Each sub-test below measures one page against the whole table, so a
	// token that stops arriving is caught, and so is one that never stops.
	t.Run("List hands out a token whenever a page is full", func(t *testing.T) {
		all, _, err := connector.List(ctx, ConnectorMainIndex, 1000, "")
		require.NoError(t, err)
		total := len(all)
		require.Greater(t, total, 5, "test setup: the table must hold more rows than the page limit below")

		for _, limit := range []int{1, 2, 5} {
			results, next, err := connector.List(ctx, ConnectorMainIndex, limit, "")
			require.NoError(t, err)
			require.Len(t, results, limit, "the page must be full, so a next page certainly exists")
			assert.NotEmpty(t, next,
				"%d of %d rows returned, so List must hand out a token for the rest", limit, total)
		}
	})

	t.Run("following the token walks every row exactly once", func(t *testing.T) {
		all, _, err := connector.List(ctx, ConnectorMainIndex, 1000, "")
		require.NoError(t, err)
		total := len(all)
		require.Greater(t, total, 5, "test setup: the table must hold more rows than the page size")

		// A caller's real loop: read a page, follow the token, stop when it
		// is empty. The page size does not divide the row count evenly, so
		// the last page is partial — that is the case that must end the walk.
		const pageSize = 3
		seen := make(map[string]int, total)
		token := ""
		pages := 0
		for {
			results, next, err := connector.List(ctx, ConnectorMainIndex, pageSize, token)
			require.NoError(t, err)
			pages++
			require.LessOrEqual(t, pages, total+2, "the walk does not terminate — the token keeps naming a next page")
			for _, kv := range results {
				seen[kv.PartitionKey+"/"+kv.RangeKey]++
			}
			if next == "" {
				break
			}
			require.NotEqual(t, token, next, "the token must advance, or the caller reads the same page forever")
			token = next
		}

		assert.Len(t, seen, total, "the walk must reach every row the unpaged read returned")
		for key, times := range seen {
			assert.Equal(t, 1, times, "row %s was handed out %d times", key, times)
		}
		assert.Greater(t, pages, 1, "test setup: %d rows at %d per page must take more than one page", total, pageSize)
	})

	t.Run("the last page reports that it is the last", func(t *testing.T) {
		all, _, err := connector.List(ctx, ConnectorMainIndex, 1000, "")
		require.NoError(t, err)
		total := len(all)

		// Ask for exactly the whole table. There is no limit+1-th row, so
		// there is no next page, and a token here would send the caller
		// round again for nothing.
		results, next, err := connector.List(ctx, ConnectorMainIndex, total, "")
		require.NoError(t, err)
		require.Len(t, results, total)
		assert.Empty(t, next, "a page that exhausted the table must not name a next one")

		// One row short of the table IS a full page with more behind it.
		results, next, err = connector.List(ctx, ConnectorMainIndex, total-1, "")
		require.NoError(t, err)
		require.Len(t, results, total-1)
		assert.NotEmpty(t, next, "one row is still unread, so the token must say so")
	})

	t.Run("a non-positive limit ends the walk instead of repeating it", func(t *testing.T) {
		// A limit of 0 asks for no rows. A token here would name the offset
		// the caller just asked for, so a caller looping until the token is
		// empty would never stop. Reading nothing forever is worse than
		// reading nothing once.
		results, next, err := connector.List(ctx, ConnectorMainIndex, 0, "")
		require.NoError(t, err)
		assert.Empty(t, results)
		assert.Empty(t, next, "an empty page must not hand out a token")
	})

	t.Run("List rejects a malformed pagination token", func(t *testing.T) {
		_, _, err := connector.List(ctx, ConnectorMainIndex, 10, "!!!not-base64!!!")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid pagination token")

		_, _, err = connector.List(ctx, ConnectorMainIndex, 10, "bm90LWpzb24=") // base64("not-json")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid pagination token format")
	})

	t.Run("List hides expired rows and cleanupExpired removes them", func(t *testing.T) {
		ttl := 1 * time.Second
		require.NoError(t, connector.Set(ctx, "ttl-pk", "soon", []byte("short-lived"), &ttl))

		got, err := connector.Get(ctx, ConnectorMainIndex, "ttl-pk", "soon", nil)
		require.NoError(t, err)
		require.Equal(t, []byte("short-lived"), got, "the row must be live before it expires")

		require.Eventually(t, func() bool {
			results, _, err := connector.List(ctx, ConnectorMainIndex, 1000, "")
			if err != nil {
				return false
			}
			for _, kv := range results {
				if kv.PartitionKey == "ttl-pk" {
					return false
				}
			}
			return true
		}, 20*time.Second, 200*time.Millisecond, "an expired row must drop out of List")

		// The expired row is hidden from reads but still occupies a row until
		// the sweep runs. Count it directly so the assertion below can tell
		// "hidden" apart from "deleted".
		rowCount := func() int {
			var n int
			require.NoError(t, connector.conn.QueryRow(ctx,
				fmt.Sprintf("SELECT count(*) FROM %s WHERE partition_key = $1", connector.table),
				"ttl-pk").Scan(&n))
			return n
		}
		require.Equal(t, 1, rowCount(), "the expired row must still occupy storage before the sweep")

		require.NoError(t, connector.cleanupExpired(ctx), "the sweep must succeed")

		assert.Equal(t, 0, rowCount(), "the sweep must physically delete the expired row")

		// A live row on the same table must survive the sweep.
		require.NoError(t, connector.Set(ctx, "ttl-live", "rk", []byte("no-ttl"), nil))
		require.NoError(t, connector.cleanupExpired(ctx))
		live, err := connector.Get(ctx, ConnectorMainIndex, "ttl-live", "rk", nil)
		require.NoError(t, err)
		assert.Equal(t, []byte("no-ttl"), live, "a row with no expiry must survive the sweep")
	})

	t.Run("counter watch receives the initial value and later publishes", func(t *testing.T) {
		const key = "counter-key-evm-123"

		initial := CounterInt64State{Value: 100, UpdatedAt: time.Now().UnixMilli(), UpdatedBy: "pod-init"}
		payload, err := common.SonicCfg.Marshal(initial)
		require.NoError(t, err)
		require.NoError(t, connector.Set(ctx, key, "value", payload, nil))

		watchCtx, cancelWatch := context.WithTimeout(ctx, 60*time.Second)
		defer cancelWatch()

		updates, cleanup, err := connector.WatchCounterInt64(watchCtx, key)
		require.NoError(t, err)
		require.NotNil(t, updates)
		require.NotNil(t, cleanup)

		select {
		case got := <-updates:
			assert.Equal(t, int64(100), got.Value, "the watcher must be seeded with the stored value")
			assert.Equal(t, "pod-init", got.UpdatedBy, "the writer identity must be seeded too")
		case <-time.After(30 * time.Second):
			t.Fatal("the watcher never received the stored initial value")
		}

		// A publish on the same key must reach the LISTEN connection.
		published := CounterInt64State{Value: 200, UpdatedAt: time.Now().UnixMilli() + 1, UpdatedBy: "pod-b"}
		deadline := time.After(30 * time.Second)
		var got CounterInt64State
		for {
			require.NoError(t, connector.PublishCounterInt64(ctx, key, published))
			select {
			case got = <-updates:
			case <-time.After(200 * time.Millisecond):
				continue
			case <-deadline:
				t.Fatal("the published counter never reached the watcher")
			}
			break
		}
		assert.Equal(t, int64(200), got.Value, "the published value must reach the watcher")
		assert.Equal(t, "pod-b", got.UpdatedBy, "the publishing pod's identity must reach the watcher")

		cleanup()
	})

	t.Run("counter watch on an unknown key yields no initial value", func(t *testing.T) {
		watchCtx, cancelWatch := context.WithTimeout(ctx, 60*time.Second)
		defer cancelWatch()

		updates, cleanup, err := connector.WatchCounterInt64(watchCtx, "counter-never-written")
		require.NoError(t, err)
		defer cleanup()

		select {
		case got := <-updates:
			t.Fatalf("an unwritten counter must not seed a value, got %+v", got)
		case <-time.After(2 * time.Second):
		}
	})
}

// TestPostgreSQLConnector_NotReadyRefusesEveryWrite pins the not-ready
// sentinel. A connector whose pool never came up must report
// ErrConnectorNotReady from every entry point, so the auth strategy can
// classify it as "db not ready" and fail open instead of blaming the network.
func TestPostgreSQLConnector_NotReadyRefusesEveryWrite(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	logger := zerolog.New(io.Discard)
	cfg := &common.PostgreSQLConnectorConfig{
		Table:         "never_created",
		ConnectionUri: "postgres://user:pass@127.0.0.1:9876/nodb?sslmode=disable",
		InitTimeout:   common.Duration(500 * time.Millisecond),
		GetTimeout:    common.Duration(500 * time.Millisecond),
		SetTimeout:    common.Duration(500 * time.Millisecond),
		MinConns:      1,
		MaxConns:      2,
	}

	connector, err := NewPostgreSQLConnector(ctx, &logger, "pg-not-ready", cfg)
	require.NoError(t, err)
	require.NotEqual(t, util.StateReady, connector.initializer.State())

	assert.ErrorIs(t, connector.Delete(ctx, "pk", "rk"), ErrConnectorNotReady)

	_, _, listErr := connector.List(ctx, ConnectorMainIndex, 10, "")
	assert.ErrorIs(t, listErr, ErrConnectorNotReady)

	assert.ErrorIs(t, connector.PublishCounterInt64(ctx, "k", CounterInt64State{Value: 1}), ErrConnectorNotReady)
}
