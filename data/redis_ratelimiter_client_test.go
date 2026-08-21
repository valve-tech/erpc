package data

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	envoyratelimitredis "github.com/envoyproxy/ratelimit/src/redis"
	"github.com/mediocregopher/radix/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newIAMRateLimitClientOn builds the IAM rate-limit client over a radix pool
// pointed at a local miniredis. It skips NewIAMRateLimitClient's AWS token
// minting so the client's own command semantics can be exercised offline.
func newIAMRateLimitClientOn(t *testing.T) (*iamRateLimitClient, *miniredis.Miniredis) {
	t.Helper()

	m, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(m.Close)

	pool, err := radix.NewPool("tcp", m.Addr(), 2,
		radix.PoolPipelineWindow(5*time.Millisecond, 32))
	require.NoError(t, err)

	c := &iamRateLimitClient{pool: pool}
	t.Cleanup(func() { _ = c.Close() })
	return c, m
}

// TestIAMRateLimitClient_DoCmd pins that a command reaches Redis and its reply
// reaches the caller. The envoy rate limiter reads its counters through this
// path, so a lost reply reads as "no requests seen" and the limit never trips.
func TestIAMRateLimitClient_DoCmd(t *testing.T) {
	t.Parallel()
	c, m := newIAMRateLimitClientOn(t)

	var count int64
	require.NoError(t, c.DoCmd(&count, "INCRBY", "budget:key", 3))
	assert.Equal(t, int64(3), count, "the INCRBY reply must reach the caller")
	assert.Equal(t, "3", mustGet(t, m, "budget:key"), "the counter must be stored in redis")

	require.NoError(t, c.DoCmd(&count, "INCRBY", "budget:key", 4))
	assert.Equal(t, int64(7), count, "the counter must accumulate across commands")
}

func mustGet(t *testing.T, m *miniredis.Miniredis, key string) string {
	t.Helper()
	v, err := m.Get(key)
	require.NoError(t, err)
	return v
}

// TestIAMRateLimitClient_PipeAppendAndPipeDo pins the pipeline path the envoy
// rate limiter uses for its INCRBY+EXPIRE pair: every appended command must run
// and its reply must land in the caller's receiver.
func TestIAMRateLimitClient_PipeAppendAndPipeDo(t *testing.T) {
	t.Parallel()
	c, m := newIAMRateLimitClientOn(t)

	var incr int64
	var expireOK int64

	var pipeline envoyratelimitredis.Pipeline
	pipeline = c.PipeAppend(pipeline, &incr, "INCRBY", "budget:pipe", 5)
	pipeline = c.PipeAppend(pipeline, &expireOK, "EXPIRE", "budget:pipe", 60)
	require.Len(t, pipeline, 2, "PipeAppend must accumulate commands, not replace them")

	require.NoError(t, c.PipeDo(pipeline))

	assert.Equal(t, int64(5), incr, "the INCRBY reply must be delivered")
	assert.Equal(t, int64(1), expireOK, "the EXPIRE reply must be delivered")
	assert.Equal(t, "5", mustGet(t, m, "budget:pipe"), "the counter must be stored")
	assert.Greater(t, m.TTL("budget:pipe"), time.Duration(0), "the expiry must be applied")
}

// TestIAMRateLimitClient_PipeDoStopsAtFirstError pins the documented
// early-return: the envoy library builds one pipeline per key, so aborting on
// the first failure never abandons another key's commands.
func TestIAMRateLimitClient_PipeDoStopsAtFirstError(t *testing.T) {
	t.Parallel()
	c, m := newIAMRateLimitClientOn(t)

	// A string key cannot take a list push, so the second command fails.
	require.NoError(t, c.DoCmd(nil, "SET", "wrongtype", "not-a-list"))

	var pushed int64
	var never int64
	var pipeline envoyratelimitredis.Pipeline
	pipeline = c.PipeAppend(pipeline, &pushed, "LPUSH", "wrongtype", "x")
	pipeline = c.PipeAppend(pipeline, &never, "INCRBY", "after-the-error", 1)

	err := c.PipeDo(pipeline)
	require.Error(t, err, "the WRONGTYPE failure must surface")

	assert.Equal(t, int64(0), never, "the command after the failure must not have run")
	assert.False(t, m.Exists("after-the-error"),
		"no key may be written after the pipeline aborts")
}

// TestIAMRateLimitClient_StaticContract pins the two constants the envoy
// library reads. ImplicitPipeliningEnabled must stay true, because the pool is
// built with PoolPipelineWindow and the library sizes its admission channel
// from that assumption.
func TestIAMRateLimitClient_StaticContract(t *testing.T) {
	t.Parallel()
	c, _ := newIAMRateLimitClientOn(t)

	assert.True(t, c.ImplicitPipeliningEnabled(),
		"the radix pool pipelines implicitly, so the library must be told so")
	assert.Equal(t, 0, c.NumActiveConns(),
		"radix/v3 exposes no active-connection counter; the client reports zero")
}

// TestIAMRateLimitClient_CloseReleasesThePool pins that Close shuts the pool
// down: a command issued afterwards must fail rather than silently succeed
// against a half-closed pool.
func TestIAMRateLimitClient_CloseReleasesThePool(t *testing.T) {
	t.Parallel()
	c, _ := newIAMRateLimitClientOn(t)

	var n int64
	require.NoError(t, c.DoCmd(&n, "INCRBY", "before-close", 1),
		"the pool must work before Close")
	require.Equal(t, int64(1), n)

	require.NoError(t, c.Close())

	assert.Error(t, c.DoCmd(&n, "INCRBY", "after-close", 1),
		"a command on a closed pool must fail")
}

// TestIAMRateLimitClient_ImplementsEnvoyInterface pins the interface contract.
// The connector hands this type to the envoy rate limiter in place of its own
// client; a drift in the interface must fail at compile time here.
func TestIAMRateLimitClient_ImplementsEnvoyInterface(t *testing.T) {
	t.Parallel()
	var _ envoyratelimitredis.Client = (*iamRateLimitClient)(nil)
}
