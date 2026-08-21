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
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// startRedisContainer brings up one redis container and returns its
// "host:port". Container startup dominates the run time, so one container
// serves every subtest below.
func startRedisContainer(t *testing.T, ctx context.Context) string {
	t.Helper()
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
	return fmt.Sprintf("%s:%s", host, port.Port())
}

// redisBehindProxy builds a redis connector that reaches redis only through
// the supplied break proxy — the main client and the pubsub connection alike.
func redisBehindProxy(t *testing.T, ctx context.Context, proxy *breakProxy, id string) *RedisConnector {
	t.Helper()
	logger := zerolog.New(io.Discard)
	cfg := &common.RedisConnectorConfig{
		Addr:         proxy.Addr(),
		ConnPoolSize: 5,
		InitTimeout:  common.Duration(20 * time.Second),
		GetTimeout:   common.Duration(5 * time.Second),
		SetTimeout:   common.Duration(5 * time.Second),
	}
	require.NoError(t, cfg.SetDefaults())

	c, err := NewRedisConnector(ctx, &logger, id, cfg)
	require.NoError(t, err)
	require.Equal(t, util.StateReady, c.initializer.State(), "the connector must come up through the proxy")
	return c
}

// storeCounter writes the counter value the pubsub manager's poll path reads.
// PublishCounterInt64 only broadcasts; the value itself lives in the store.
func storeCounter(t *testing.T, ctx context.Context, c *RedisConnector, key string, st CounterInt64State) {
	t.Helper()
	payload, err := common.SonicCfg.Marshal(st)
	require.NoError(t, err)
	require.NoError(t, c.Set(ctx, key, "value", payload, nil))
}

// TestRedisPubSubManager_Reconnect drives the pubsub manager's self-healing
// loops with a proxy that severs live connections on command. Every wait is
// bounded.
func TestRedisPubSubManager_Reconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	redisAddr := startRedisContainer(t, ctx)

	// A subscriber must keep receiving after the connection dies. The caller
	// keeps the same channel throughout. Note where the healing happens: the
	// go-redis PubSub re-dials underneath the manager, so the manager's own
	// reconnect loop is not what saves this case. The subtests below drive
	// that loop directly.
	t.Run("a subscriber survives a severed connection", func(t *testing.T) {
		proxy := newBreakProxy(t, redisAddr)
		c := redisBehindProxy(t, ctx, proxy, "redis-survive")

		updates, cleanup, err := c.WatchCounterInt64(ctx, "survive")
		require.NoError(t, err)
		defer cleanup()

		// Prove the path works before breaking anything.
		waitForCounterPublish(t, ctx, c, "survive", updates, CounterInt64State{Value: 1, UpdatedAt: 1}, 30*time.Second)

		require.Positive(t, proxy.Break(), "the manager must have had a live connection to sever")
		acceptedWhileDown := proxy.Accepted()

		// The manager reconnects on its own. Keep republishing until one
		// lands: redis pubsub has no replay, so a message sent while the
		// subscription is down is gone.
		waitForCounterPublish(t, ctx, c, "survive", updates, CounterInt64State{Value: 2, UpdatedAt: 2}, 90*time.Second)
		assert.Greater(t, proxy.Accepted(), acceptedWhileDown,
			"delivery must resume over a new connection, not one that never broke")
	})

	// A value written while the subscription is down is never published, so
	// pubsub cannot deliver it. The polling loop is the only thing that
	// recovers it. This drives a manager with a short poll interval, because
	// the production interval is five minutes — that is how long a subscriber
	// can hold a stale counter after a missed message.
	t.Run("the polling loop recovers a value pubsub never carried", func(t *testing.T) {
		proxy := newBreakProxy(t, redisAddr)
		c := redisBehindProxy(t, ctx, proxy, "redis-poll-recovery")

		lg := zerolog.New(io.Discard)
		m := &RedisPubSubManager{
			connector:    c,
			logger:       &lg,
			appCtx:       ctx,
			pollInterval: 300 * time.Millisecond,
		}
		require.NoError(t, m.start())
		t.Cleanup(m.stop)

		updates, cleanup, err := m.Subscribe("recovered")
		require.NoError(t, err)
		defer cleanup()

		// Write the value without publishing it. Only a poll can surface it.
		storeCounter(t, ctx, c, "recovered", CounterInt64State{Value: 77, UpdatedAt: 77})

		select {
		case got := <-updates:
			assert.Equal(t, int64(77), got.Value,
				"the polling loop must deliver a value that pubsub never carried")
		case <-time.After(60 * time.Second):
			t.Fatal("the polling loop never delivered the stored value")
		}
	})

	// The message loop's own self-healing path: when the subscription itself
	// ends, the loop must back off, resubscribe, and resume delivery on the
	// caller's existing channel.
	t.Run("the message loop resubscribes after its subscription ends", func(t *testing.T) {
		proxy := newBreakProxy(t, redisAddr)
		c := redisBehindProxy(t, ctx, proxy, "redis-msgloop")
		m := c.pubsubManager
		require.NotNil(t, m)

		updates, cleanup, err := m.Subscribe("healed")
		require.NoError(t, err)
		defer cleanup()
		waitForCounterPublish(t, ctx, c, "healed", updates, CounterInt64State{Value: 1, UpdatedAt: 1}, 30*time.Second)

		// End the subscription under the loop. runMessageLoop sees its channel
		// close and returns, which is the only entry into the reconnect path.
		m.mu.Lock()
		require.NotNil(t, m.pubsub)
		require.NoError(t, m.pubsub.Close())
		m.mu.Unlock()

		waitForCounterPublish(t, ctx, c, "healed", updates, CounterInt64State{Value: 2, UpdatedAt: 2}, 90*time.Second)
		assert.True(t, m.running.Load(), "the manager must still be running after it healed")
	})

	// The message loop must keep retrying while redis stays unreachable, and
	// recover once it comes back. A loop that gave up after one failure would
	// leave every subscriber on this pod permanently stale.
	t.Run("the message loop retries until redis returns", func(t *testing.T) {
		proxy := newBreakProxy(t, redisAddr)
		c := redisBehindProxy(t, ctx, proxy, "redis-msgloop-retry")
		m := c.pubsubManager
		require.NotNil(t, m)

		updates, cleanup, err := m.Subscribe("stubborn")
		require.NoError(t, err)
		defer cleanup()

		proxy.Refuse(true)
		m.mu.Lock()
		require.NotNil(t, m.pubsub)
		require.NoError(t, m.pubsub.Close())
		m.mu.Unlock()
		proxy.Break()

		// The loop's backoff starts at one second and doubles. Four seconds
		// covers at least two failed attempts. Throughout them the manager
		// must hold no subscription: installing one on an unreachable redis
		// would make the loop believe it healed and stop retrying.
		time.Sleep(4 * time.Second)
		m.mu.RLock()
		ps := m.pubsub
		m.mu.RUnlock()
		assert.Nil(t, ps, "the loop must not install a subscription while redis is unreachable")

		proxy.Refuse(false)
		waitFor(t, 120*time.Second, 200*time.Millisecond, "the loop to resubscribe", func() bool {
			m.mu.RLock()
			defer m.mu.RUnlock()
			return m.pubsub != nil
		})
		waitForCounterPublish(t, ctx, c, "stubborn", updates, CounterInt64State{Value: 9, UpdatedAt: 9}, 120*time.Second)
	})

	// reconnectPubSub must refuse to install a subscription on an unhealthy
	// client, and it must report why. Installing one anyway leaves a pubsub
	// that never delivers and a message loop that thinks it recovered.
	t.Run("reconnect refuses while redis is unreachable and succeeds once it returns", func(t *testing.T) {
		proxy := newBreakProxy(t, redisAddr)
		c := redisBehindProxy(t, ctx, proxy, "redis-reconnect-gate")
		m := c.pubsubManager
		require.NotNil(t, m)

		proxy.Break()
		proxy.Refuse(true)

		err := m.reconnectPubSub()
		require.Error(t, err, "a reconnect against an unreachable redis must fail")
		assert.Contains(t, err.Error(), "unhealthy")

		proxy.Refuse(false)
		waitFor(t, 60*time.Second, 500*time.Millisecond, "the reconnect to succeed", func() bool {
			return m.reconnectPubSub() == nil
		})
	})

	// Stopping the manager must signal every subscriber. A consumer that also
	// selects on the done channel has to observe the shutdown; without it the
	// consumer blocks on a channel nobody will ever write to again.
	t.Run("stopping the manager signals its subscribers", func(t *testing.T) {
		proxy := newBreakProxy(t, redisAddr)
		c := redisBehindProxy(t, ctx, proxy, "redis-stop")
		m := c.pubsubManager
		require.NotNil(t, m)

		_, cleanup, err := m.Subscribe("stopping")
		require.NoError(t, err)
		defer cleanup()

		subs, ok := m.subscribers.Load("stopping")
		require.True(t, ok)
		sc := subs.([]*subscriberChannel)[0]

		m.stop()

		select {
		case <-sc.done:
		case <-time.After(10 * time.Second):
			t.Fatal("stopping the manager did not signal its subscriber")
		}
		assert.False(t, m.running.Load(), "a stopped manager must not report itself running")

		// A second stop must be a no-op rather than a second close.
		m.stop()
	})

	// Subscribing after the manager stopped must restart it, not hand back a
	// channel nothing will ever write to.
	t.Run("subscribing after a stop restarts the manager", func(t *testing.T) {
		proxy := newBreakProxy(t, redisAddr)
		c := redisBehindProxy(t, ctx, proxy, "redis-restart")
		m := c.pubsubManager
		require.NotNil(t, m)

		m.stop()
		require.False(t, m.running.Load())

		updates, cleanup, err := m.Subscribe("restarted")
		require.NoError(t, err)
		defer cleanup()
		assert.True(t, m.running.Load(), "Subscribe must restart a stopped manager")

		waitForCounterPublish(t, ctx, c, "restarted", updates, CounterInt64State{Value: 5, UpdatedAt: 5}, 60*time.Second)
	})
}

// A manager whose application context is already done must refuse to
// subscribe. Handing back a channel would strand the caller.
func TestRedisPubSubManager_SubscribeRefusesAfterTheContextEnds(t *testing.T) {
	t.Parallel()
	appCtx, cancel := context.WithCancel(context.Background())
	cancel()

	lg := zerolog.New(io.Discard)
	m := &RedisPubSubManager{logger: &lg, appCtx: appCtx, pollInterval: time.Minute}

	ch, cleanup, err := m.Subscribe("k")

	require.Error(t, err)
	assert.Nil(t, ch)
	assert.Nil(t, cleanup)
	assert.Contains(t, err.Error(), "stopped")
}

// waitForCounterPublish republishes want until the subscriber receives it, or
// the deadline passes. Redis pubsub has no replay, so a single publish issued
// while the subscription is being rebuilt is legitimately lost.
func waitForCounterPublish(
	t *testing.T,
	ctx context.Context,
	c *RedisConnector,
	key string,
	updates <-chan CounterInt64State,
	want CounterInt64State,
	timeout time.Duration,
) {
	t.Helper()
	payload, merr := common.SonicCfg.Marshal(want)
	require.NoError(t, merr)

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// Both calls can fail while redis is unreachable. That is the state
		// under test, so retry rather than fail: the deadline is the oracle.
		_ = c.Set(ctx, key, "value", payload, nil)
		_ = c.PublishCounterInt64(ctx, key, want)
		select {
		case got := <-updates:
			if got.Value == want.Value {
				return
			}
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Fatalf("timed out after %s waiting for counter %q to reach value %d", timeout, key, want.Value)
}
