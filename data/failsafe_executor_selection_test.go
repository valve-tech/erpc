package data

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// finalityNetwork is a stand-in Network that reports one fixed finality. The
// real finality calculation needs an upstream state poller; the executor
// selection under test only reads the answer, so a fixed one is enough.
type finalityNetwork struct {
	finality common.DataFinalityState
}

var _ common.Network = (*finalityNetwork)(nil)

func (n *finalityNetwork) Id() string                            { return "evm:1" }
func (n *finalityNetwork) Label() string                         { return "evm:1" }
func (n *finalityNetwork) ProjectId() string                     { return "test_project" }
func (n *finalityNetwork) Architecture() common.NetworkArchitecture {
	return common.ArchitectureEvm
}
func (n *finalityNetwork) Config() *common.NetworkConfig { return nil }
func (n *finalityNetwork) Logger() *zerolog.Logger {
	lg := zerolog.Nop()
	return &lg
}
func (n *finalityNetwork) GetMethodMetrics(method string) common.TrackedMetrics { return nil }
func (n *finalityNetwork) Forward(ctx context.Context, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return nil, errors.New("not used in this test")
}
func (n *finalityNetwork) GetFinality(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) common.DataFinalityState {
	return n.finality
}

func requestWithMethodAndFinality(method string, finality common.DataFinalityState) context.Context {
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"` + method + `","params":[],"id":1}`))
	req.SetNetwork(&finalityNetwork{finality: finality})
	return context.WithValue(context.Background(), common.RequestContextKey, req)
}

func newSelectableExecutor(t *testing.T, method string, finalities []common.DataFinalityState) *cacheExecutor {
	t.Helper()
	logger := zerolog.New(io.Discard)
	e, err := NewCacheExecutor(&common.CacheFailsafeConfig{
		MatchMethod:   method,
		MatchFinality: finalities,
	}, &logger)
	require.NoError(t, err)
	return e
}

// TestPickCacheExecutor_FourTierPriority pins the documented order:
// method+finality, then method, then finality, then default. An operator sets a
// short timeout for realtime reads and a long one for finalized reads; if the
// order slipped, the wrong timeout would apply and either cut off a slow cold
// store or hold a hot request open.
func TestPickCacheExecutor_FourTierPriority(t *testing.T) {
	methodAndFinality := newSelectableExecutor(t, "eth_getLogs", []common.DataFinalityState{common.DataFinalityStateRealtime})
	methodOnly := newSelectableExecutor(t, "eth_getLogs", nil)
	finalityOnly := newSelectableExecutor(t, "*", []common.DataFinalityState{common.DataFinalityStateRealtime})
	fallback := newSelectableExecutor(t, "*", nil)

	// Deliberately listed weakest-first so a picker that simply returned the
	// first entry would fail every case below.
	all := []*cacheExecutor{fallback, finalityOnly, methodOnly, methodAndFinality}

	t.Run("method and finality both match: the most specific wins", func(t *testing.T) {
		got := pickCacheExecutor(all, requestWithMethodAndFinality("eth_getLogs", common.DataFinalityStateRealtime))
		assert.Same(t, methodAndFinality, got)
	})

	t.Run("only the method matches: the method-only executor wins", func(t *testing.T) {
		got := pickCacheExecutor(all, requestWithMethodAndFinality("eth_getLogs", common.DataFinalityStateFinalized))
		assert.Same(t, methodOnly, got)
	})

	t.Run("only the finality matches: the finality-only executor wins", func(t *testing.T) {
		got := pickCacheExecutor(all, requestWithMethodAndFinality("eth_call", common.DataFinalityStateRealtime))
		assert.Same(t, finalityOnly, got)
	})

	t.Run("nothing matches: the default executor wins", func(t *testing.T) {
		got := pickCacheExecutor(all, requestWithMethodAndFinality("eth_call", common.DataFinalityStateFinalized))
		assert.Same(t, fallback, got)
	})

	t.Run("a method glob still counts as a method match", func(t *testing.T) {
		globbed := newSelectableExecutor(t, "eth_get*", nil)
		got := pickCacheExecutor([]*cacheExecutor{fallback, globbed}, requestWithMethodAndFinality("eth_getBalance", common.DataFinalityStateFinalized))
		assert.Same(t, globbed, got)
	})

	t.Run("a finality list matches any member", func(t *testing.T) {
		multi := newSelectableExecutor(t, "*", []common.DataFinalityState{
			common.DataFinalityStateUnfinalized,
			common.DataFinalityStateRealtime,
		})
		for _, f := range []common.DataFinalityState{common.DataFinalityStateUnfinalized, common.DataFinalityStateRealtime} {
			got := pickCacheExecutor([]*cacheExecutor{fallback, multi}, requestWithMethodAndFinality("eth_call", f))
			assert.Same(t, multi, got, "finality %s must match a list that contains it", f)
		}
		got := pickCacheExecutor([]*cacheExecutor{fallback, multi}, requestWithMethodAndFinality("eth_call", common.DataFinalityStateFinalized))
		assert.Same(t, fallback, got, "a finality outside the list must fall through")
	})

	t.Run("no request on the context still resolves to the default", func(t *testing.T) {
		// Background prefetch and admin paths run without a request. They must
		// still get an executor, not a nil that skips every policy.
		got := pickCacheExecutor(all, context.Background())
		assert.Same(t, fallback, got)
	})

	t.Run("no default configured yields nil so the caller passes through", func(t *testing.T) {
		got := pickCacheExecutor([]*cacheExecutor{methodOnly}, requestWithMethodAndFinality("eth_call", common.DataFinalityStateFinalized))
		assert.Nil(t, got)
	})
}

// TestBuildCacheExecutors_AlwaysAppendsADefault proves an operator config that
// names only specific methods still leaves a catch-all. Without it, every
// unmatched cache call would bypass the failsafe wrapper entirely.
func TestBuildCacheExecutors_AlwaysAppendsADefault(t *testing.T) {
	logger := zerolog.New(io.Discard)

	executors, err := buildCacheExecutors(&logger, "memory-1", []*common.FailsafeConfig{
		{MatchMethod: "eth_getLogs", Retry: &common.RetryPolicyConfig{MaxAttempts: 2}},
		nil, // a nil entry in the list must be skipped, not panic
	})
	require.NoError(t, err)
	require.Len(t, executors, 2, "the specific executor plus the appended default")
	assert.Equal(t, "eth_getLogs", executors[0].MatchMethod())
	assert.Equal(t, "*", executors[1].MatchMethod())
	assert.Empty(t, executors[1].MatchFinality())

	t.Run("an unsupported policy fails the whole build", func(t *testing.T) {
		_, err := buildCacheExecutors(&logger, "memory-1", []*common.FailsafeConfig{
			{Consensus: &common.ConsensusPolicyConfig{}},
		})
		require.Error(t, err, "consensus has no meaning on a cache connector and must be refused")
	})
}

// TestNewCacheExecutor_DefaultsTheMethodToAWildcard keeps an unnamed executor
// in the catch-all tier rather than in the "matches the empty method" tier,
// which nothing would ever hit.
func TestNewCacheExecutor_DefaultsTheMethodToAWildcard(t *testing.T) {
	logger := zerolog.New(io.Discard)

	e, err := NewCacheExecutor(&common.CacheFailsafeConfig{}, &logger)
	require.NoError(t, err)
	assert.Equal(t, "*", e.MatchMethod())

	e, err = NewCacheExecutor(nil, &logger)
	require.NoError(t, err)
	assert.Equal(t, "*", e.MatchMethod())
	assert.Empty(t, e.MatchFinality())

	e, err = NewCacheExecutor(&common.CacheFailsafeConfig{
		MatchMethod:   "eth_getLogs",
		MatchFinality: []common.DataFinalityState{common.DataFinalityStateRealtime},
	}, &logger)
	require.NoError(t, err)
	assert.Equal(t, "eth_getLogs", e.MatchMethod())
	assert.Equal(t, []common.DataFinalityState{common.DataFinalityStateRealtime}, e.MatchFinality())
}

// headAwareConnector is a connector that also reports the head it serves.
type headAwareConnector struct {
	Connector
	ts   int64
	ok   bool
	seen string
}

func (h *headAwareConnector) CacheLatestBlockTimestamp(networkId string) (int64, bool) {
	h.seen = networkId
	return h.ts, h.ok
}

// TestFailsafeConnector_PassesThroughTheHeadReport proves the realtime age
// guard keeps working through the failsafe wrapper. If the wrapper stopped
// forwarding, the guard would see "head unknown" and serve stale realtime data
// with no way for an operator to notice.
func TestFailsafeConnector_PassesThroughTheHeadReport(t *testing.T) {
	logger := zerolog.New(io.Discard)

	t.Run("a head-aware connector's answer reaches the caller", func(t *testing.T) {
		inner := &headAwareConnector{Connector: NewMockConnector("memory-1"), ts: 1717171717, ok: true}
		fc, err := NewFailsafeConnector(&logger, inner, nil, nil)
		require.NoError(t, err)

		ts, ok := fc.CacheLatestBlockTimestamp("evm:1")
		assert.True(t, ok)
		assert.Equal(t, int64(1717171717), ts)
		assert.Equal(t, "evm:1", inner.seen, "the network id must reach the wrapped connector unchanged")
	})

	t.Run("a connector that is not head-aware reports unknown", func(t *testing.T) {
		fc, err := NewFailsafeConnector(&logger, NewMockConnector("memory-1"), nil, nil)
		require.NoError(t, err)

		ts, ok := fc.CacheLatestBlockTimestamp("evm:1")
		assert.False(t, ok)
		assert.Equal(t, int64(0), ts)
	})
}

// TestFailsafeConnector_DelegatesSharedStateOperations proves the lock and
// counter paths reach the real connector. A broken delegation here is silent
// and corrupting: two erpc instances would each believe they hold the lock.
func TestFailsafeConnector_DelegatesSharedStateOperations(t *testing.T) {
	logger := zerolog.New(io.Discard)
	mem, err := NewMemoryConnector(context.Background(), &logger, "memory-1", &common.MemoryConnectorConfig{
		MaxItems: 100, MaxTotalSize: "1mb",
	})
	require.NoError(t, err)

	fc, err := NewFailsafeConnector(&logger, mem, nil, nil)
	require.NoError(t, err)

	assert.Equal(t, "memory-1", fc.Id(), "telemetry labels the connector by this id")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t.Run("the first lock is granted and the second is refused", func(t *testing.T) {
		lock, err := fc.Lock(ctx, "counter/evm:1/latest", 2*time.Second)
		require.NoError(t, err)
		require.NotNil(t, lock)
		require.False(t, lock.IsNil(), "a granted lock must not be the nil sentinel")

		_, err = fc.Lock(ctx, "counter/evm:1/latest", 2*time.Second)
		require.Error(t, err, "a second holder must be refused or the counter can be double-written")

		require.NoError(t, lock.Unlock(ctx))
	})

	t.Run("List forwards its arguments unchanged", func(t *testing.T) {
		// The memory connector refuses List; the point is that the refusal
		// comes from the wrapped connector rather than from the wrapper.
		_, _, err := fc.List(ctx, ConnectorMainIndex, 10, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "MemoryConnector")
	})
}

// TestFailsafeConnector_ForwardsCounterCallsVerbatim proves the wrapper hands
// the shared-counter calls to the real connector with the key and state
// untouched. A wrapper that swallowed a publish would stall cross-instance
// block-tip sync silently: every instance would keep its own stale tip and
// nothing would log an error.
func TestFailsafeConnector_ForwardsCounterCallsVerbatim(t *testing.T) {
	logger := zerolog.New(io.Discard)
	inner := &recordingCounterConnector{
		Connector: NewMockConnector("redis-1"),
		watchCh:   make(chan CounterInt64State, 1),
	}

	fc, err := NewFailsafeConnector(&logger, inner, nil, nil)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	want := CounterInt64State{Value: 21_000_000, UpdatedAt: 1717171717000, UpdatedBy: "pod-a"}
	require.NoError(t, fc.PublishCounterInt64(ctx, "counter/evm:1/latest", want))
	assert.Equal(t, "counter/evm:1/latest", inner.publishedKey)
	assert.Equal(t, want, inner.publishedState,
		"the exact state must reach the connector, not a zeroed copy")

	ch, unsubscribe, err := fc.WatchCounterInt64(ctx, "counter/evm:1/latest")
	require.NoError(t, err)
	require.NotNil(t, ch)
	require.NotNil(t, unsubscribe)
	require.Equal(t, "counter/evm:1/latest", inner.watchedKey,
		"the watch must reach the connector, or nothing ever publishes to this channel")

	// The channel the caller receives must be the connector's own, or the
	// registry would wait forever on a channel nobody writes to. The send is
	// buffered, so a wrapper that substituted its own channel fails here at
	// once rather than blocking.
	inner.watchCh <- want
	select {
	case got := <-ch:
		assert.Equal(t, want, got)
	case <-time.After(2 * time.Second):
		t.Fatal("the wrapper handed back a channel the connector does not write to")
	}
}

// recordingCounterConnector captures the shared-counter calls it receives.
type recordingCounterConnector struct {
	Connector
	publishedKey   string
	publishedState CounterInt64State
	watchedKey     string
	watchCh        chan CounterInt64State
}

func (r *recordingCounterConnector) PublishCounterInt64(ctx context.Context, key string, value CounterInt64State) error {
	r.publishedKey = key
	r.publishedState = value
	return nil
}

func (r *recordingCounterConnector) WatchCounterInt64(ctx context.Context, key string) (<-chan CounterInt64State, func(), error) {
	r.watchedKey = key
	return r.watchCh, func() {}, nil
}
