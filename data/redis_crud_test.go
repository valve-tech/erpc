package data

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// newReadyRedisConnector starts a miniredis server and returns a connector
// that has reached the ready state. Both are torn down with the test.
func newReadyRedisConnector(t *testing.T, id string) (context.Context, *RedisConnector, *miniredis.Miniredis) {
	t.Helper()

	m, err := miniredis.Run()
	require.NoError(t, err, "failed to start miniredis")
	t.Cleanup(m.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	logger := zerolog.New(io.Discard)
	cfg := &common.RedisConnectorConfig{
		Addr:         m.Addr(),
		DB:           0,
		ConnPoolSize: 5,
		InitTimeout:  common.Duration(5 * time.Second),
		GetTimeout:   common.Duration(5 * time.Second),
		SetTimeout:   common.Duration(5 * time.Second),
	}
	require.NoError(t, cfg.SetDefaults())

	connector, err := NewRedisConnector(ctx, &logger, id, cfg)
	require.NoError(t, err)
	require.Equal(t, util.StateReady, connector.initializer.State(), "connector must be ready")

	return ctx, connector, m
}

// TestRedisConnector_Id pins that the connector reports the id it was built
// with — it is the label operators see in connector logs and metrics.
func TestRedisConnector_Id(t *testing.T) {
	_, connector, _ := newReadyRedisConnector(t, "redis-id-under-test")
	assert.Equal(t, "redis-id-under-test", connector.Id())
}

// TestRedisConnector_Delete pins the delete path: the value must be gone
// afterwards, and deleting a key that is not there must not be an error
// (callers treat delete as idempotent).
func TestRedisConnector_Delete(t *testing.T) {
	ctx, connector, m := newReadyRedisConnector(t, "redis-delete")

	require.NoError(t, connector.Set(ctx, "pk1", "rk1", []byte("keep-me"), nil))
	require.NoError(t, connector.Set(ctx, "pk2", "rk2", []byte("delete-me"), nil))

	// Both are present before the delete — otherwise the assertion below would
	// pass against an empty store.
	got, err := connector.Get(ctx, ConnectorMainIndex, "pk2", "rk2", nil)
	require.NoError(t, err)
	require.Equal(t, []byte("delete-me"), got)

	require.NoError(t, connector.Delete(ctx, "pk2", "rk2"))

	_, err = connector.Get(ctx, ConnectorMainIndex, "pk2", "rk2", nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound))
	assert.False(t, m.Exists("pk2:rk2"), "the redis key must be gone")

	// The untouched key survives.
	kept, err := connector.Get(ctx, ConnectorMainIndex, "pk1", "rk1", nil)
	require.NoError(t, err)
	assert.Equal(t, []byte("keep-me"), kept)

	// Deleting an absent key is a no-op, not an error.
	assert.NoError(t, connector.Delete(ctx, "pk-absent", "rk-absent"))
}

// TestRedisConnector_DeleteRemovesReverseIndex pins the reverse-index cleanup.
// A stale reverse-index entry would keep resolving a wildcard lookup to a
// partition key whose value no longer exists.
func TestRedisConnector_DeleteRemovesReverseIndex(t *testing.T) {
	ctx, connector, m := newReadyRedisConnector(t, "redis-delete-rvi")

	require.NoError(t, connector.Set(ctx, "evm:123:0x7b", "eth_getBlockByNumber", []byte("block-body"), nil))

	reverseKey := fmt.Sprintf("%s#evm:123:*#eth_getBlockByNumber", redisReverseIndexPrefix)
	require.True(t, m.Exists(reverseKey), "the reverse index entry must exist before the delete")

	// The wildcard Get resolves through that reverse index today.
	resolved, err := connector.Get(ctx, ConnectorReverseIndex, "evm:123:*", "eth_getBlockByNumber", nil)
	require.NoError(t, err)
	require.Equal(t, []byte("block-body"), resolved)

	require.NoError(t, connector.Delete(ctx, "evm:123:0x7b", "eth_getBlockByNumber"))

	assert.False(t, m.Exists(reverseKey), "the reverse index entry must be deleted with its record")
	assert.False(t, m.Exists("evm:123:0x7b:eth_getBlockByNumber"), "the record itself must be deleted")

	_, err = connector.Get(ctx, ConnectorReverseIndex, "evm:123:*", "eth_getBlockByNumber", nil)
	require.Error(t, err, "the wildcard lookup must miss once the reverse index is gone")
	assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound))
}

// TestRedisConnector_DeleteLeavesUnrelatedReverseIndexAlone pins the guard on
// the reverse-index cleanup: a partition key that is not architecture-prefixed
// has no reverse-index companion, and the delete must not go looking for one.
func TestRedisConnector_DeleteLeavesUnrelatedReverseIndexAlone(t *testing.T) {
	ctx, connector, m := newReadyRedisConnector(t, "redis-delete-plain")

	require.NoError(t, connector.Set(ctx, "evm:123:0x7b", "m", []byte("with-index"), nil))
	require.NoError(t, connector.Set(ctx, "plainkey", "m", []byte("no-index"), nil))

	evmReverse := fmt.Sprintf("%s#evm:123:*#m", redisReverseIndexPrefix)
	require.True(t, m.Exists(evmReverse))

	require.NoError(t, connector.Delete(ctx, "plainkey", "m"))

	assert.False(t, m.Exists("plainkey:m"), "the plain record must be deleted")
	assert.True(t, m.Exists(evmReverse), "another record's reverse index must be untouched")
}

// TestRedisConnector_List pins the main-index listing: every stored pair comes
// back with its partition key, range key and value split correctly.
func TestRedisConnector_List(t *testing.T) {
	ctx, connector, _ := newReadyRedisConnector(t, "redis-list")

	want := map[string][]byte{
		"alpha:one":   []byte("v-alpha"),
		"beta:two":    []byte("v-beta"),
		"gamma:three": []byte("v-gamma"),
	}
	for key, value := range want {
		parts := strings.SplitN(key, ":", 2)
		require.NoError(t, connector.Set(ctx, parts[0], parts[1], value, nil))
	}

	results, _, err := connector.List(ctx, ConnectorMainIndex, 100, "")
	require.NoError(t, err)
	require.Len(t, results, len(want), "every stored pair must be listed")

	for _, kv := range results {
		key := kv.PartitionKey + ":" + kv.RangeKey
		expected, ok := want[key]
		require.True(t, ok, "unexpected key in listing: %s", key)
		assert.Equal(t, expected, kv.Value, "the listed value must match what was stored under %s", key)
	}
}

// TestRedisConnector_ListReverseIndex pins that the reverse-index listing
// returns the reverse entries — partition key and range key parsed out of the
// "rvi#pk#rk" shape — and not the main records.
func TestRedisConnector_ListReverseIndex(t *testing.T) {
	ctx, connector, _ := newReadyRedisConnector(t, "redis-list-rvi")

	require.NoError(t, connector.Set(ctx, "evm:1:0x10", "eth_getBlockByNumber", []byte("b1"), nil))
	require.NoError(t, connector.Set(ctx, "evm:2:0x20", "eth_getBlockByNumber", []byte("b2"), nil))
	require.NoError(t, connector.Set(ctx, "plainkey", "m", []byte("no-index"), nil))

	results, _, err := connector.List(ctx, ConnectorReverseIndex, 100, "")
	require.NoError(t, err)
	require.Len(t, results, 2, "only the two reverse index entries must be listed")

	got := map[string]string{}
	for _, kv := range results {
		got[kv.PartitionKey+"|"+kv.RangeKey] = string(kv.Value)
	}
	assert.Equal(t, "evm:1:0x10", got["evm:1:*|eth_getBlockByNumber"],
		"the reverse entry must point at the concrete partition key")
	assert.Equal(t, "evm:2:0x20", got["evm:2:*|eth_getBlockByNumber"])
}

// TestRedisConnector_ListPaginatesOnRealRedis pins the SCAN cursor round trip.
// It needs a real Redis: miniredis answers every SCAN in one shot with cursor
// 0, so the cursor path is invisible there.
//
// Paging with a small limit must visit every key exactly once, carry the
// cursor across calls, and finish with an empty token.
func TestRedisConnector_ListPaginatesOnRealRedis(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	redisC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "redis:7-alpine",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForListeningPort("6379/tcp"),
		},
		Started: true,
	})
	require.NoError(t, err, "failed to start the redis container")
	t.Cleanup(func() { _ = redisC.Terminate(context.Background()) })

	host, err := redisC.Host(ctx)
	require.NoError(t, err)
	port, err := redisC.MappedPort(ctx, "6379")
	require.NoError(t, err)

	logger := zerolog.New(io.Discard)
	cfg := &common.RedisConnectorConfig{
		Addr:         fmt.Sprintf("%s:%s", host, port.Port()),
		ConnPoolSize: 5,
		InitTimeout:  common.Duration(30 * time.Second),
		GetTimeout:   common.Duration(30 * time.Second),
		SetTimeout:   common.Duration(30 * time.Second),
	}
	require.NoError(t, cfg.SetDefaults())

	connector, err := NewRedisConnector(ctx, &logger, "redis-list-page", cfg)
	require.NoError(t, err)
	require.Equal(t, util.StateReady, connector.initializer.State())

	const total = 500
	for i := 0; i < total; i++ {
		require.NoError(t, connector.Set(ctx, fmt.Sprintf("pk%03d", i), "rk", []byte(fmt.Sprintf("v%03d", i)), nil))
	}

	seen := map[string]string{}
	token := ""
	pages := 0
	for pages < 1000 {
		results, next, err := connector.List(ctx, ConnectorMainIndex, 10, token)
		require.NoError(t, err)
		pages++
		for _, kv := range results {
			seen[kv.PartitionKey] = string(kv.Value)
		}
		token = next
		if token == "" {
			break
		}
	}

	require.Greater(t, pages, 1, "a 500-key store scanned 10 at a time must take more than one page")
	require.Equal(t, "", token, "pagination must terminate with an empty token")
	require.Len(t, seen, total, "every key must be visited across all pages")
	assert.Equal(t, "v000", seen["pk000"], "the paged value must match what was stored")
	assert.Equal(t, "v499", seen["pk499"])
}

// TestRedisConnector_ApiKeyAddressRoundTripsOnRealRedis pins the storage half
// of API-key management against a real Redis.
//
// Redis compares keys literally. It cannot expand a wildcard on the main index
// and it cannot enumerate the range keys under one partition key, so an
// API-key record is only reachable if it sits at ConnectorApiKeyRangeKey — the
// one address a reader can name from the API key alone. This walks the three
// operations the admin RPCs perform: issue, list, revoke.
func TestRedisConnector_ApiKeyAddressRoundTripsOnRealRedis(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	redisC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "redis:7-alpine",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForListeningPort("6379/tcp"),
		},
		Started: true,
	})
	require.NoError(t, err, "failed to start the redis container")
	t.Cleanup(func() { _ = redisC.Terminate(context.Background()) })

	host, err := redisC.Host(ctx)
	require.NoError(t, err)
	port, err := redisC.MappedPort(ctx, "6379")
	require.NoError(t, err)

	logger := zerolog.New(io.Discard)
	cfg := &common.RedisConnectorConfig{
		Addr:         fmt.Sprintf("%s:%s", host, port.Port()),
		ConnPoolSize: 5,
		InitTimeout:  common.Duration(30 * time.Second),
		GetTimeout:   common.Duration(30 * time.Second),
		SetTimeout:   common.Duration(30 * time.Second),
	}
	require.NoError(t, cfg.SetDefaults())

	connector, err := NewRedisConnector(ctx, &logger, "redis-api-keys", cfg)
	require.NoError(t, err)
	require.Equal(t, util.StateReady, connector.initializer.State())

	record := []byte(`{"userId":"alice","enabled":true,"rateLimitBudget":"gold"}`)
	require.NoError(t, connector.Set(ctx, "sk_live", ConnectorApiKeyRangeKey, record, nil))

	got, err := connector.Get(ctx, ConnectorMainIndex, "sk_live", ConnectorApiKeyRangeKey, nil)
	require.NoError(t, err, "the record must be readable at the address it was written to")
	assert.JSONEq(t, string(record), string(got))

	// The user id is payload. Redis cannot resolve an address it was not given,
	// which is why the writer must not put the user id in the key.
	_, err = connector.Get(ctx, ConnectorMainIndex, "sk_live", "alice", nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound))

	// Listing has to carry the record body, because that body is the only place
	// the user id is written down.
	results, _, err := connector.List(ctx, ConnectorMainIndex, 100, "")
	require.NoError(t, err)
	var listed *KeyValuePair
	for i := range results {
		if results[i].PartitionKey == "sk_live" {
			listed = &results[i]
		}
	}
	require.NotNil(t, listed, "an issued key must appear in the listing")
	assert.Equal(t, ConnectorApiKeyRangeKey, listed.RangeKey)
	assert.JSONEq(t, string(record), string(listed.Value),
		"the listed record must carry the user id, since the range key names nobody")

	require.NoError(t, connector.Delete(ctx, "sk_live", ConnectorApiKeyRangeKey))
	_, err = connector.Get(ctx, ConnectorMainIndex, "sk_live", ConnectorApiKeyRangeKey, nil)
	require.Error(t, err, "a revoked key must be gone")
	assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound))
}

// TestRedisConnector_ListRejectsBadPaginationToken pins the token validation.
// A token from another driver must be refused, not silently treated as page 0.
func TestRedisConnector_ListRejectsBadPaginationToken(t *testing.T) {
	ctx, connector, _ := newReadyRedisConnector(t, "redis-list-badtoken")

	_, _, err := connector.List(ctx, ConnectorMainIndex, 10, "not-a-cursor")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid pagination token")
}

// TestRedisConnector_CounterPubSub pins the shared-counter transport: a value
// published on one connector reaches a watcher, with its value, timestamp and
// writer identity intact.
func TestRedisConnector_CounterPubSub(t *testing.T) {
	ctx, connector, _ := newReadyRedisConnector(t, "redis-counter")

	updates, cleanup, err := connector.WatchCounterInt64(ctx, "latestBlock:evm:123")
	require.NoError(t, err)
	require.NotNil(t, updates)
	require.NotNil(t, cleanup)
	defer cleanup()

	want := CounterInt64State{Value: 987654, UpdatedAt: time.Now().UnixMilli(), UpdatedBy: "pod-a"}

	// The subscription is established asynchronously, so republish until the
	// watcher sees it rather than sleeping on a guess.
	deadline := time.After(20 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	var got CounterInt64State
	for {
		require.NoError(t, connector.PublishCounterInt64(ctx, "latestBlock:evm:123", want))
		select {
		case got = <-updates:
		case <-ticker.C:
			continue
		case <-deadline:
			t.Fatal("the published counter never reached the watcher")
		}
		break
	}

	assert.Equal(t, want.Value, got.Value, "the published counter value must reach the watcher")
	assert.Equal(t, want.UpdatedAt, got.UpdatedAt, "the update timestamp must survive the round trip")
	assert.Equal(t, "pod-a", got.UpdatedBy, "the writer identity must survive the round trip")
}

// TestRedisConnector_WatchCounterInt64RequiresReady pins that a connector that
// never connected refuses to hand out a watch channel instead of returning a
// channel that silently never fires.
func TestRedisConnector_WatchCounterInt64RequiresReady(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	logger := zerolog.New(io.Discard)
	cfg := &common.RedisConnectorConfig{
		Addr:        "127.0.0.1:1", // nothing listens here
		InitTimeout: common.Duration(200 * time.Millisecond),
		GetTimeout:  common.Duration(200 * time.Millisecond),
		SetTimeout:  common.Duration(200 * time.Millisecond),
	}
	require.NoError(t, cfg.SetDefaults())

	connector, err := NewRedisConnector(ctx, &logger, "redis-not-ready", cfg)
	require.NoError(t, err)
	require.NotEqual(t, util.StateReady, connector.initializer.State())

	updates, cleanup, err := connector.WatchCounterInt64(ctx, "some-counter")
	require.Error(t, err, "an unconnected connector must refuse the watch")
	assert.Nil(t, updates)
	assert.Nil(t, cleanup)

	err = connector.PublishCounterInt64(ctx, "some-counter", CounterInt64State{Value: 1})
	require.Error(t, err, "an unconnected connector must refuse the publish")

	_, _, err = connector.List(ctx, ConnectorMainIndex, 10, "")
	require.Error(t, err, "an unconnected connector must refuse the list")

	err = connector.Delete(ctx, "pk", "rk")
	require.Error(t, err, "an unconnected connector must refuse the delete")
}

// TestRedisConnector_MarkConnectionAsLost pins which errors flip the connector
// back into reconnecting. Getting this wrong either wedges a healthy pool on a
// business-level miss, or leaves a dead pool in the ready state.
func TestRedisConnector_MarkConnectionAsLost(t *testing.T) {
	_, connector, _ := newReadyRedisConnector(t, "redis-mark-lost")

	notLost := []struct {
		name string
		err  error
	}{
		{"nil error", nil},
		{"redis.Nil is a cache miss", redis.Nil},
		{"failed transaction", redis.TxFailedErr},
		{"context canceled", context.Canceled},
		{"deadline exceeded", context.DeadlineExceeded},
		{"record not found", common.NewErrRecordNotFound("pk", "rk", RedisDriverName)},
		{"application-level error", errors.New("WRONGTYPE Operation against a key")},
	}
	for _, tc := range notLost {
		t.Run("keeps ready on "+tc.name, func(t *testing.T) {
			connector.markConnectionAsLostIfNecessary(tc.err)
			assert.Equal(t, util.StateReady, connector.initializer.State(),
				"%s must not tear down a healthy connection", tc.name)
		})
	}

	// A real transport failure does flip the state.
	connector.markConnectionAsLostIfNecessary(errors.New("dial tcp 127.0.0.1:6379: connect: connection refused"))
	require.Eventually(t, func() bool {
		return connector.initializer.State() != util.StateReady
	}, 10*time.Second, 10*time.Millisecond, "a connection refusal must mark the connector for reconnection")
}

// TestRedisLock_IsNil pins the nil guard callers rely on before unlocking.
func TestRedisLock_IsNil(t *testing.T) {
	ctx, connector, _ := newReadyRedisConnector(t, "redis-lock-isnil")

	var absent *redisLock
	assert.True(t, absent.IsNil(), "a nil lock pointer must report itself as nil")
	assert.True(t, (&redisLock{}).IsNil(), "a lock with no mutex must report itself as nil")

	lock, err := connector.Lock(ctx, "lock-key", 5*time.Second)
	require.NoError(t, err)
	assert.False(t, lock.IsNil(), "an acquired lock must not report itself as nil")
	require.NoError(t, lock.Unlock(ctx))
}
