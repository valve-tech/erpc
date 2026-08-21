package data

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// startDynamoConnector brings up one DynamoDB Local container and returns a
// ready connector. One container serves every subtest below, because container
// startup dominates the run time.
func startDynamoConnector(t *testing.T, table string) (context.Context, *DynamoDBConnector, string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	t.Cleanup(cancel)

	ddbC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "amazon/dynamodb-local",
			ExposedPorts: []string{"8000/tcp"},
			WaitingFor:   wait.ForListeningPort("8000/tcp").WithStartupTimeout(3 * time.Minute),
		},
		Started: true,
	})
	require.NoError(t, err, "failed to start the DynamoDB Local container")
	t.Cleanup(func() { _ = ddbC.Terminate(context.Background()) })

	host, err := ddbC.Host(ctx)
	require.NoError(t, err)
	port, err := ddbC.MappedPort(ctx, "8000")
	require.NoError(t, err)

	logger := zerolog.New(io.Discard)
	cfg := &common.DynamoDBConnectorConfig{
		Endpoint:          fmt.Sprintf("http://%s:%s", host, port.Port()),
		Region:            "us-west-2",
		Table:             table,
		PartitionKeyName:  "pk",
		RangeKeyName:      "rk",
		ReverseIndexName:  "rk-pk-index",
		TTLAttributeName:  "ttl",
		InitTimeout:       common.Duration(60 * time.Second),
		GetTimeout:        common.Duration(30 * time.Second),
		SetTimeout:        common.Duration(30 * time.Second),
		StatePollInterval: common.Duration(100 * time.Millisecond),
		LockRetryInterval: common.Duration(100 * time.Millisecond),
		Auth: &common.AwsAuthConfig{
			Mode:            "secret",
			AccessKeyID:     "fakeKey",
			SecretAccessKey: "fakeSecret",
		},
	}

	connector, err := NewDynamoDBConnector(ctx, &logger, "ddb-crud", cfg)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return connector.initializer.State() == util.StateReady
	}, 60*time.Second, 200*time.Millisecond, "the connector must reach the ready state")

	return ctx, connector, cfg.Endpoint
}

// newDynamoConnectorAt builds a second connector against an already-running
// DynamoDB Local, with a caller-chosen counter poll interval. A long interval
// isolates the initial-value send in WatchCounterInt64 from the poller.
func newDynamoConnectorAt(t *testing.T, ctx context.Context, endpoint, table, id string, poll time.Duration) *DynamoDBConnector {
	t.Helper()
	logger := zerolog.New(io.Discard)
	cfg := &common.DynamoDBConnectorConfig{
		Endpoint:          endpoint,
		Region:            "us-west-2",
		Table:             table,
		PartitionKeyName:  "pk",
		RangeKeyName:      "rk",
		ReverseIndexName:  "rk-pk-index",
		TTLAttributeName:  "ttl",
		InitTimeout:       common.Duration(60 * time.Second),
		GetTimeout:        common.Duration(30 * time.Second),
		SetTimeout:        common.Duration(30 * time.Second),
		StatePollInterval: common.Duration(poll),
		LockRetryInterval: common.Duration(100 * time.Millisecond),
		Auth: &common.AwsAuthConfig{
			Mode:            "secret",
			AccessKeyID:     "fakeKey",
			SecretAccessKey: "fakeSecret",
		},
	}
	connector, err := NewDynamoDBConnector(ctx, &logger, id, cfg)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return connector.initializer.State() == util.StateReady
	}, 60*time.Second, 200*time.Millisecond, "the connector must reach the ready state")
	return connector
}

// TestDynamoDBConnector_CRUD covers the connector methods that need a live
// DynamoDB: Id, Delete, List and the counter watch/publish pair.
func TestDynamoDBConnector_CRUD(t *testing.T) {
	ctx, connector, endpoint := startDynamoConnector(t, "ddb_crud_table")

	t.Run("Id reports the configured connector id", func(t *testing.T) {
		assert.Equal(t, "ddb-crud", connector.Id())
	})

	t.Run("Delete removes one item and leaves the rest", func(t *testing.T) {
		require.NoError(t, connector.Set(ctx, "del-pk", "keep", []byte("keep-me"), nil))
		require.NoError(t, connector.Set(ctx, "del-pk", "drop", []byte("drop-me"), nil))

		got, err := connector.Get(ctx, ConnectorMainIndex, "del-pk", "drop", nil)
		require.NoError(t, err)
		require.Equal(t, []byte("drop-me"), got, "the item must exist before the delete")

		require.NoError(t, connector.Delete(ctx, "del-pk", "drop"))

		_, err = connector.Get(ctx, ConnectorMainIndex, "del-pk", "drop", nil)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound))

		kept, err := connector.Get(ctx, ConnectorMainIndex, "del-pk", "keep", nil)
		require.NoError(t, err)
		assert.Equal(t, []byte("keep-me"), kept, "the sibling item must survive")

		assert.NoError(t, connector.Delete(ctx, "no-such-pk", "no-such-rk"),
			"deleting an absent item must not be an error")
	})

	// DynamoDB reads the main index with GetItem, which matches both keys
	// exactly. An API-key record is therefore only reachable at the one address
	// a reader can name from the API key alone.
	t.Run("an API-key record round trips at its canonical address", func(t *testing.T) {
		record := []byte(`{"userId":"alice","enabled":true,"rateLimitBudget":"gold"}`)
		require.NoError(t, connector.Set(ctx, "sk_live", ConnectorApiKeyRangeKey, record, nil))

		got, err := connector.Get(ctx, ConnectorMainIndex, "sk_live", ConnectorApiKeyRangeKey, nil)
		require.NoError(t, err, "the record must be readable at the address it was written to")
		assert.JSONEq(t, string(record), string(got))

		// The user id is payload, not address. A writer that put it in the key
		// would leave a record no reader could locate.
		_, err = connector.Get(ctx, ConnectorMainIndex, "sk_live", "alice", nil)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound))

		require.NoError(t, connector.Delete(ctx, "sk_live", ConnectorApiKeyRangeKey))
		_, err = connector.Get(ctx, ConnectorMainIndex, "sk_live", ConnectorApiKeyRangeKey, nil)
		require.Error(t, err, "a revoked key must be gone")
		assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound))
	})

	t.Run("List returns every stored pair with its value", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			require.NoError(t, connector.Set(ctx, fmt.Sprintf("list-pk-%d", i), "rk",
				[]byte(fmt.Sprintf("v%d", i)), nil))
		}

		results, _, err := connector.List(ctx, ConnectorMainIndex, 1000, "")
		require.NoError(t, err)

		got := map[string]string{}
		for _, kv := range results {
			require.NotEmpty(t, kv.PartitionKey)
			require.NotEmpty(t, kv.RangeKey)
			got[kv.PartitionKey+"|"+kv.RangeKey] = string(kv.Value)
		}
		for i := 0; i < 5; i++ {
			key := fmt.Sprintf("list-pk-%d|rk", i)
			require.Contains(t, got, key, "every stored pair must be listed")
			assert.Equal(t, fmt.Sprintf("v%d", i), got[key], "the listed value must match what was stored")
		}
	})

	t.Run("List paginates with an opaque token", func(t *testing.T) {
		for i := 0; i < 20; i++ {
			require.NoError(t, connector.Set(ctx, fmt.Sprintf("page-pk-%02d", i), "rk",
				[]byte(fmt.Sprintf("p%02d", i)), nil))
		}

		seen := map[string]string{}
		token := ""
		pages := 0
		for pages < 200 {
			results, next, err := connector.List(ctx, ConnectorMainIndex, 3, token)
			require.NoError(t, err)
			pages++
			for _, kv := range results {
				if strings.HasPrefix(kv.PartitionKey, "page-pk-") {
					seen[kv.PartitionKey] = string(kv.Value)
				}
			}
			token = next
			if token == "" {
				break
			}
		}

		require.Greater(t, pages, 1, "20 items scanned 3 at a time must take more than one page")
		require.Equal(t, "", token, "pagination must end with an empty token")
		require.Len(t, seen, 20, "every paged item must be visited")
		assert.Equal(t, "p00", seen["page-pk-00"], "the paged value must match what was stored")
		assert.Equal(t, "p19", seen["page-pk-19"])
	})

	t.Run("List rejects a malformed pagination token", func(t *testing.T) {
		_, _, err := connector.List(ctx, ConnectorMainIndex, 10, "!!!not-base64!!!")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid pagination token")

		_, _, err = connector.List(ctx, ConnectorMainIndex, 10,
			base64.StdEncoding.EncodeToString([]byte("not-json")))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid pagination token format")
	})

	t.Run("List skips expired items", func(t *testing.T) {
		// DynamoDB Local does not run the TTL reaper, so the connector filters
		// expired items itself. Write one whose ttl attribute is already in the
		// past — Set refuses a negative TTL, so put the item directly.
		_, err := connector.writeClient.PutItemWithContext(ctx, &dynamodb.PutItemInput{
			TableName: aws.String(connector.table),
			Item: map[string]*dynamodb.AttributeValue{
				connector.partitionKeyName: {S: aws.String("expired-pk")},
				connector.rangeKeyName:     {S: aws.String("rk")},
				"value":                    {B: []byte("gone")},
				connector.ttlAttributeName: {N: aws.String(fmt.Sprintf("%d", time.Now().Add(-time.Hour).Unix()))},
			},
		})
		require.NoError(t, err)
		require.NoError(t, connector.Set(ctx, "live-pk", "rk", []byte("here"), nil))

		results, _, listErr := connector.List(ctx, ConnectorMainIndex, 1000, "")
		require.NoError(t, listErr)

		keys := map[string]bool{}
		for _, kv := range results {
			keys[kv.PartitionKey] = true
		}
		assert.True(t, keys["live-pk"], "a live item must be listed")
		assert.False(t, keys["expired-pk"], "an item past its ttl must not be listed")
	})

	t.Run("counter watch delivers the stored value", func(t *testing.T) {
		const key = "ddb-counter-key"

		want := CounterInt64State{Value: 4242, UpdatedAt: time.Now().UnixMilli(), UpdatedBy: "pod-x"}
		payload, err := common.SonicCfg.Marshal(want)
		require.NoError(t, err)
		require.NoError(t, connector.Set(ctx, key, "value", payload, nil))

		// getSimpleValue is the read behind both the initial send and the poll.
		st, ok, err := connector.getSimpleValue(ctx, key)
		require.NoError(t, err)
		require.True(t, ok, "the stored counter must be readable")
		assert.Equal(t, int64(4242), st.Value)
		assert.Equal(t, "pod-x", st.UpdatedBy)

		// A connector whose poll interval is an hour cannot deliver anything
		// through the poller, so a value arriving here proves the initial send
		// in WatchCounterInt64 works on its own.
		slowCtx, cancelSlow := context.WithTimeout(ctx, 60*time.Second)
		defer cancelSlow()
		slow := newDynamoConnectorAt(t, slowCtx, endpoint, connector.table, "ddb-slow-poll", time.Hour)

		slowUpdates, slowCleanup, err := slow.WatchCounterInt64(slowCtx, key)
		require.NoError(t, err)
		select {
		case got := <-slowUpdates:
			assert.Equal(t, int64(4242), got.Value, "the watcher must be seeded with the stored value")
			assert.Equal(t, "pod-x", got.UpdatedBy, "the writer identity must be seeded too")
		case <-time.After(20 * time.Second):
			t.Fatal("the initial value was never sent to the watcher")
		}
		cancelSlow()
		time.Sleep(200 * time.Millisecond)
		slowCleanup()

		watchCtx, cancelWatch := context.WithTimeout(ctx, 60*time.Second)
		defer cancelWatch()

		updates, cleanup, err := connector.WatchCounterInt64(watchCtx, key)
		require.NoError(t, err)
		require.NotNil(t, updates)
		require.NotNil(t, cleanup)

		// Drain whatever the initial send or the first poll delivered.
		select {
		case <-updates:
		case <-time.After(20 * time.Second):
			t.Fatal("the watcher never received the stored value")
		}

		// A later write is picked up by the poller because its timestamp is newer.
		newer := CounterInt64State{Value: 5555, UpdatedAt: want.UpdatedAt + 1000, UpdatedBy: "pod-y"}
		newerPayload, err := common.SonicCfg.Marshal(newer)
		require.NoError(t, err)
		require.NoError(t, connector.Set(ctx, key, "value", newerPayload, nil))

		// The initial send and the first poll race, so the old value may be
		// buffered twice. Read until the newer value arrives, and refuse any
		// value that is neither the old one nor the new one.
		deadline := time.After(30 * time.Second)
		gotNewer := false
		for !gotNewer {
			select {
			case got := <-updates:
				if got.Value == want.Value {
					continue // a duplicate of the seeded value; keep reading
				}
				assert.Equal(t, int64(5555), got.Value, "the poller must deliver the newer value")
				assert.Equal(t, "pod-y", got.UpdatedBy)
				gotNewer = true
			case <-deadline:
				t.Fatal("the poller never delivered the newer value")
			}
		}

		// Cancel the watch context before cleanup so the polling goroutine has
		// stopped; cleanup closes the channel the poller writes to.
		cancelWatch()
		time.Sleep(500 * time.Millisecond)
		cleanup()
	})

	t.Run("getSimpleValue reports a missing or unparsable counter as absent", func(t *testing.T) {
		st, ok, err := connector.getSimpleValue(ctx, "counter-never-written")
		require.NoError(t, err, "a missing counter is not an error")
		assert.False(t, ok)
		assert.Equal(t, CounterInt64State{}, st)

		require.NoError(t, connector.Set(ctx, "counter-garbage", "value", []byte("{not json"), nil))
		st, ok, err = connector.getSimpleValue(ctx, "counter-garbage")
		require.NoError(t, err, "an unparsable counter is treated as missing, not as an error")
		assert.False(t, ok)
		assert.Equal(t, CounterInt64State{}, st)

		// A record whose timestamp is zero is also treated as absent, because
		// the poller uses the timestamp to order updates.
		zeroTs, err := common.SonicCfg.Marshal(CounterInt64State{Value: 7, UpdatedAt: 0})
		require.NoError(t, err)
		require.NoError(t, connector.Set(ctx, "counter-zero-ts", "value", zeroTs, nil))
		_, ok, err = connector.getSimpleValue(ctx, "counter-zero-ts")
		require.NoError(t, err)
		assert.False(t, ok, "a counter with no update timestamp must not be served")
	})

	t.Run("PublishCounterInt64 is a no-op for dynamodb", func(t *testing.T) {
		// DynamoDB has no push channel; the authoritative state is Set() and
		// readers poll. Publish must therefore succeed without writing.
		assert.NoError(t, connector.PublishCounterInt64(ctx, "publish-noop-key",
			CounterInt64State{Value: 1, UpdatedAt: time.Now().UnixMilli()}))

		_, ok, err := connector.getSimpleValue(ctx, "publish-noop-key")
		require.NoError(t, err)
		assert.False(t, ok, "Publish must not create a counter record on its own")
	})
}

// TestDynamoLock_IsNil pins the nil guard callers rely on before unlocking.
func TestDynamoLock_IsNil(t *testing.T) {
	t.Parallel()

	var absent *dynamoLock
	assert.True(t, absent.IsNil(), "a nil lock pointer must report itself as nil")
	assert.True(t, (&dynamoLock{lockKey: "k"}).IsNil(), "a lock with no connector must report itself as nil")
	assert.False(t, (&dynamoLock{connector: &DynamoDBConnector{}, lockKey: "k"}).IsNil(),
		"a lock holding a connector must not report itself as nil")
}

// TestDynamoDBConnector_NotInitializedRefusesEveryCall pins that a connector
// whose client never came up reports an error from every entry point instead
// of dereferencing a nil client.
func TestDynamoDBConnector_NotInitializedRefusesEveryCall(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	logger := zerolog.New(io.Discard)
	cfg := &common.DynamoDBConnectorConfig{
		Endpoint:         "http://127.0.0.1:9876", // nothing listens here
		Region:           "us-west-2",
		Table:            "never_created",
		PartitionKeyName: "pk",
		RangeKeyName:     "rk",
		ReverseIndexName: "rk-pk-index",
		TTLAttributeName: "ttl",
		InitTimeout:      common.Duration(500 * time.Millisecond),
		GetTimeout:       common.Duration(500 * time.Millisecond),
		SetTimeout:       common.Duration(500 * time.Millisecond),
		Auth: &common.AwsAuthConfig{
			Mode:            "secret",
			AccessKeyID:     "fakeKey",
			SecretAccessKey: "fakeSecret",
		},
	}

	connector, err := NewDynamoDBConnector(ctx, &logger, "ddb-not-ready", cfg)
	require.NoError(t, err)
	require.NotEqual(t, util.StateReady, connector.initializer.State())

	require.NotPanics(t, func() {
		assert.Error(t, connector.Delete(ctx, "pk", "rk"), "Delete must refuse when the endpoint is unreachable")

		_, _, listErr := connector.List(ctx, ConnectorMainIndex, 10, "")
		assert.Error(t, listErr, "List must refuse when the endpoint is unreachable")
	}, "an unconnected connector must report errors, not crash")
}

// TestDynamoDBConnector_WatchCounterNonPositivePollInterval PINS A LIVE
// DEFECT. It asserts today's crashing behaviour, not the desired behaviour.
//
// Defect: data/dynamodb.go:723 calls time.NewTicker(d.statePollInterval)
// without checking the interval. time.NewTicker panics on any interval that is
// not positive. common/defaults.go:1332 only substitutes a default when
// StatePollInterval is exactly 0, so a negative value in the operator's YAML
// survives config load and reaches the ticker unchecked.
//
// What an operator observes: writing `statePollInterval: -1s` under a DynamoDB
// shared-state connector makes eRPC panic — "non-positive interval for
// NewTicker" — the first time any shared counter is watched, which is during
// startup. There is no validation error and no log line pointing at the
// offending field.
//
// This test locks the behaviour in place. Do not "fix" the test — add a guard
// in WatchCounterInt64 or validation in SetDefaults.
func TestDynamoDBConnector_WatchCounterNonPositivePollInterval(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	logger := zerolog.New(io.Discard)
	cfg := &common.DynamoDBConnectorConfig{
		Endpoint:          "http://127.0.0.1:9876",
		Region:            "us-west-2",
		Table:             "never_created",
		PartitionKeyName:  "pk",
		RangeKeyName:      "rk",
		ReverseIndexName:  "rk-pk-index",
		TTLAttributeName:  "ttl",
		InitTimeout:       common.Duration(500 * time.Millisecond),
		GetTimeout:        common.Duration(500 * time.Millisecond),
		SetTimeout:        common.Duration(500 * time.Millisecond),
		StatePollInterval: common.Duration(-1 * time.Second), // survives SetDefaults
		Auth: &common.AwsAuthConfig{
			Mode:            "secret",
			AccessKeyID:     "fakeKey",
			SecretAccessKey: "fakeSecret",
		},
	}

	// SetDefaults leaves a negative interval untouched — it only fills in a
	// value that is exactly zero. Confirm that before relying on it.
	require.NoError(t, cfg.SetDefaults(""))
	require.Equal(t, common.Duration(-1*time.Second), cfg.StatePollInterval,
		"SetDefaults must be shown to leave a negative poll interval in place")

	connector, err := NewDynamoDBConnector(ctx, &logger, "ddb-bad-interval", cfg)
	require.NoError(t, err)

	assert.PanicsWithValue(t, "non-positive interval for NewTicker", func() {
		_, _, _ = connector.WatchCounterInt64(ctx, "any-counter")
	}, "DEFECT PINNED: a negative statePollInterval crashes the process instead of being rejected")
}
