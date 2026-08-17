package evm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// These tests drive the cache's Get and Set decision paths: which policies a
// request selects, what the fan-out does with a miss / an error / a cancelled
// read, and what Set refuses to store. The fixture in
// json_rpc_cache_fixture_test.go builds the cache through its real constructor
// over a real connector; where a test needs a connector that FAILS, it swaps
// the policy set for one bound to a mock.

// --- helpers ---

// pathsCache builds a cache over a single mock connector with one policy.
// Using the real constructor first and then replacing the policies keeps the
// compression settings and the logger exactly as production builds them.
func pathsCache(t *testing.T, policies ...*data.CachePolicy) *EvmJsonRpcCache {
	t.Helper()
	f := newCacheFixture(t, cacheConfig(
		cacheConns(memoryConnector("mem")),
		cachePolicies(cachePolicyCfg("mem", "*")),
	))
	f.cache.SetPolicies(policies)
	return f.cache
}

// mockPolicy binds one policy to a mock connector, matching every network and
// method at unknown finality — the state a request with no network resolves to.
func mockPolicy(t *testing.T, conn data.Connector, empty common.CacheEmptyBehavior) *data.CachePolicy {
	t.Helper()
	ttl := time.Minute
	p, err := data.NewCachePolicy(&common.CachePolicyConfig{
		Connector: conn.Id(),
		Network:   "*",
		Method:    "*",
		Finality:  common.DataFinalityStateUnknown,
		Empty:     empty,
		TTL:       common.FixedDuration(ttl),
	}, conn)
	require.NoError(t, err)
	return p
}

func blockRequest(t *testing.T, block string) *common.NormalizedRequest {
	t.Helper()
	return common.NewNormalizedRequest([]byte(
		fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["%s",false]}`, block),
	))
}

// --- Get: request and policy selection ---

func TestCacheGet_UnparsableRequestBodyIsAnError(t *testing.T) {
	c := pathsCache(t, mockPolicy(t, data.NewMockConnector("m"), common.CacheEmptyBehaviorIgnore))

	_, err := c.Get(context.Background(), common.NewNormalizedRequest([]byte(`{"jsonrpc":`)))
	require.Error(t, err, "a body the cache cannot parse must not read as a plain miss")
}

func TestCacheGet_AnInvalidPolicyPatternIsReportedNotIgnored(t *testing.T) {
	conn := data.NewMockConnector("m")
	bad, err := data.NewCachePolicy(&common.CachePolicyConfig{
		Connector: "m",
		Network:   "(", // an unbalanced group: the matcher cannot compile it
		Method:    "*",
		Finality:  common.DataFinalityStateUnknown,
	}, conn)
	require.NoError(t, err)
	c := pathsCache(t, bad)

	_, err = c.Get(context.Background(), blockRequest(t, "0x1"))
	require.Error(t, err, "a policy pattern the matcher rejects must surface, not silently match nothing")
	// Discriminating: the connector must never be reached.
	conn.AssertNotCalled(t, "Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestCacheGet_SkipCacheReadDirectiveKeepsTheConnectorUntouched(t *testing.T) {
	conn := data.NewMockConnector("redis-main")
	conn.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return([]byte(`{"number":"0x1"}`), nil)
	c := pathsCache(t, mockPolicy(t, conn, common.CacheEmptyBehaviorIgnore))

	req := blockRequest(t, "0x1")
	req.SetDirectives(&common.RequestDirectives{SkipCacheRead: "redis-main"})

	resp, err := c.Get(context.Background(), req)
	require.NoError(t, err)
	// Discriminating: the connector HAS the value. Only a reader that honours
	// the directive returns nothing here.
	assert.Nil(t, resp, "a skip-cache-read directive must beat a stored value")
	conn.AssertNotCalled(t, "Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// --- Get: fan-out outcomes ---

func TestCacheGet_ConnectorFailureFallsThroughWithoutAnError(t *testing.T) {
	conn := data.NewMockConnector("redis-main")
	conn.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, errors.New("redis: connection refused"))
	c := pathsCache(t, mockPolicy(t, conn, common.CacheEmptyBehaviorIgnore))

	resp, err := c.Get(context.Background(), blockRequest(t, "0x1"))
	require.NoError(t, err, "a broken cache must not fail the user's request")
	assert.Nil(t, resp)
	conn.AssertNumberOfCalls(t, "Get", 1)
}

// lateConnector answers only after `gate` is closed, so a test can force the
// order in which the fan-out observes its peers.
type lateConnector struct {
	*data.MockConnector
	gate  chan struct{}
	value []byte
}

func (l *lateConnector) Get(ctx context.Context, _, _, _ string, _ interface{}) ([]byte, error) {
	select {
	case <-l.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// A grace window after the gate opens. The peer's error reaches the fan-out
	// inside it, so a fan-out that cancels its peers on an error cancels THIS
	// read before it can answer.
	select {
	case <-time.After(50 * time.Millisecond):
		return l.value, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestCacheGet_AnErroringPeerDoesNotAbortTheFanOut(t *testing.T) {
	// The broken connector opens the gate as it fails, so its error ALWAYS
	// reaches the fan-out before the healthy answer. Without that ordering the
	// healthy connector would win by luck, and the test could not tell a
	// fan-out that keeps racing apart from one that aborts on the first error.
	gate := make(chan struct{})
	healthy := &lateConnector{
		MockConnector: data.NewMockConnector("healthy"),
		gate:          gate,
		value:         []byte(`{"number":"0x1","hash":"0xfeed"}`),
	}
	broken := data.NewMockConnector("broken")
	broken.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) {
			select {
			case <-gate:
			default:
				close(gate)
			}
		}).
		Return(nil, errors.New("redis: connection refused"))
	c := pathsCache(t, mockPolicy(t, broken, common.CacheEmptyBehaviorIgnore),
		mockPolicy(t, healthy, common.CacheEmptyBehaviorIgnore))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := c.Get(ctx, blockRequest(t, "0x1"))
	require.NoError(t, err)
	// Discriminating: one connector errors first and the other holds the answer,
	// so only a fan-out that keeps racing after an error can serve it.
	require.NotNil(t, resp, "an erroring peer must not abort the fan-out")
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Contains(t, string(jrr.GetResultBytes()), "0xfeed")
}

func TestCacheGet_EmptyResultUnderIgnoreIsAMissEvenWhenStored(t *testing.T) {
	conn := data.NewMockConnector("m")
	conn.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return([]byte(`null`), nil)
	c := pathsCache(t, mockPolicy(t, conn, common.CacheEmptyBehaviorIgnore))

	resp, err := c.Get(context.Background(), blockRequest(t, "0x1"))
	require.NoError(t, err)
	assert.Nil(t, resp, "under empty=ignore a stored null is a miss, not a hit")
	conn.AssertNumberOfCalls(t, "Get", 1)
}

func TestCacheGet_EmptyResultUnderAllowIsServed(t *testing.T) {
	conn := data.NewMockConnector("m")
	conn.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return([]byte(`null`), nil)
	c := pathsCache(t, mockPolicy(t, conn, common.CacheEmptyBehaviorAllow))

	resp, err := c.Get(context.Background(), blockRequest(t, "0x1"))
	require.NoError(t, err)
	// Discriminating: the SAME stored bytes are a miss under ignore (above) and
	// a hit here. Only the policy's empty behaviour separates them.
	require.NotNil(t, resp, "under empty=allow a stored null is a legitimate hit")
	assert.True(t, resp.FromCache())
}

// blockingConnector holds its Get open until the caller's context ends. It
// models a hung cache backend, which is the only way to reach the fan-out's
// cancellation path.
type blockingConnector struct {
	*data.MockConnector
	entered chan struct{}
	once    sync.Once
}

func (b *blockingConnector) Get(ctx context.Context, _, _, _ string, _ interface{}) ([]byte, error) {
	b.once.Do(func() { close(b.entered) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestCacheGet_ACancelledReadIsNotRecordedAsAMiss(t *testing.T) {
	conn := &blockingConnector{MockConnector: data.NewMockConnector("hung"), entered: make(chan struct{})}
	c := pathsCache(t, mockPolicy(t, conn, common.CacheEmptyBehaviorIgnore))

	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		resp *common.NormalizedResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := c.Get(ctx, blockRequest(t, "0x1"))
		done <- result{resp, err}
	}()

	select {
	case <-conn.entered:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("the cache never reached the connector")
	}
	cancel()

	select {
	case r := <-done:
		require.NoError(t, r.err, "a cancelled read falls through, it does not fail the request")
		assert.Nil(t, r.resp)
	case <-time.After(2 * time.Second):
		t.Fatal("Get did not return after the caller cancelled")
	}
}

// --- doGet ---

func TestDoGet_AMethodWithNoBlockReferenceNeverReachesTheConnector(t *testing.T) {
	conn := data.NewMockConnector("m")
	conn.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return([]byte(`"0x1"`), nil)
	c := pathsCache(t, mockPolicy(t, conn, common.CacheEmptyBehaviorAllow))

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"zz_unknownMethod","params":[]}`))
	resp, err := c.Get(context.Background(), req)

	require.NoError(t, err)
	assert.Nil(t, resp)
	// Discriminating: the connector holds a value, so only the block-reference
	// guard can explain the miss.
	conn.AssertNotCalled(t, "Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestDoGet_ZeroBytesFromTheConnectorIsAMiss(t *testing.T) {
	conn := data.NewMockConnector("m")
	conn.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return([]byte{}, nil)
	c := pathsCache(t, mockPolicy(t, conn, common.CacheEmptyBehaviorAllow))

	resp, err := c.Get(context.Background(), blockRequest(t, "0x1"))
	require.NoError(t, err)
	assert.Nil(t, resp, "zero bytes is an absent key, not a cached empty value")
}

func TestDoGet_ACorruptCompressedValueIsAnError(t *testing.T) {
	// The zstd magic number with nothing valid behind it: the connector claims
	// a compressed value it cannot deliver.
	corrupt := append([]byte{0x28, 0xB5, 0x2F, 0xFD}, []byte("not a zstd frame")...)
	conn := data.NewMockConnector("m")
	conn.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(corrupt, nil)

	f := newCacheFixture(t, withCompression(cacheConfig(
		cacheConns(memoryConnector("mem")),
		cachePolicies(cachePolicyCfg("mem", "*")),
	), "fastest", 1))
	f.cache.SetPolicies([]*data.CachePolicy{mockPolicy(t, conn, common.CacheEmptyBehaviorAllow)})

	resp, err := f.cache.Get(context.Background(), blockRequest(t, "0x1"))
	require.NoError(t, err, "a corrupt entry is a cache failure, not a request failure")
	assert.Nil(t, resp, "a value that cannot be decompressed must never be served")
}

// --- Set ---

func TestCacheSet_NoMatchingPolicyStoresNothing(t *testing.T) {
	conn := data.NewMockConnector("m")
	narrow, err := data.NewCachePolicy(&common.CachePolicyConfig{
		Connector: "m",
		Network:   "*",
		Method:    "eth_getLogs", // the request below is eth_getBlockByNumber
		Finality:  common.DataFinalityStateUnknown,
	}, conn)
	require.NoError(t, err)
	c := pathsCache(t, narrow)

	req := blockRequest(t, "0x1")
	require.NoError(t, c.Set(context.Background(), req, cacheResponse(t, req, `{"number":"0x1"}`)))
	conn.AssertNotCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestCacheSet_AnInvalidPolicyPatternIsReported(t *testing.T) {
	conn := data.NewMockConnector("m")
	bad, err := data.NewCachePolicy(&common.CachePolicyConfig{
		Connector: "m",
		Network:   "(",
		Method:    "*",
		Finality:  common.DataFinalityStateUnknown,
	}, conn)
	require.NoError(t, err)
	c := pathsCache(t, bad)

	req := blockRequest(t, "0x1")
	err = c.Set(context.Background(), req, cacheResponse(t, req, `{"number":"0x1"}`))
	require.Error(t, err)
	conn.AssertNotCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestCacheSet_AResponseWithNoBlockReferenceIsNotStored(t *testing.T) {
	conn := data.NewMockConnector("m")
	c := pathsCache(t, mockPolicy(t, conn, common.CacheEmptyBehaviorAllow))

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"zz_unknownMethod","params":[]}`))
	require.NoError(t, c.Set(context.Background(), req, cacheResponse(t, req, `"0x1"`)))

	// Discriminating: the policy matches the method, so only the missing block
	// reference can explain the skipped write.
	conn.AssertNotCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestCacheSet_AConnectorFailureIsReturnedToTheCaller(t *testing.T) {
	conn := data.NewMockConnector("m")
	conn.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(errors.New("redis: write timeout"))
	c := pathsCache(t, mockPolicy(t, conn, common.CacheEmptyBehaviorAllow))

	req := blockRequest(t, "0x1")
	err := c.Set(context.Background(), req, cacheResponse(t, req, `{"number":"0x1"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "write timeout", "the cause must reach the caller unflattened")
}

func TestCacheSet_TwoConnectorFailuresAreReportedTogether(t *testing.T) {
	first := data.NewMockConnector("first")
	first.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(errors.New("first is down"))
	second := data.NewMockConnector("second")
	second.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(errors.New("second is down"))
	c := pathsCache(t, mockPolicy(t, first, common.CacheEmptyBehaviorAllow),
		mockPolicy(t, second, common.CacheEmptyBehaviorAllow))

	req := blockRequest(t, "0x1")
	err := c.Set(context.Background(), req, cacheResponse(t, req, `{"number":"0x1"}`))
	require.Error(t, err)
	// Discriminating: a report that keeps only the first failure would hide the
	// second connector entirely.
	assert.Contains(t, err.Error(), "first is down")
	assert.Contains(t, err.Error(), "second is down")
}

func TestCacheSet_AnOversizedResultIsSkippedNotFailed(t *testing.T) {
	conn := data.NewMockConnector("m")
	maxSize := "4"
	policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
		Connector:   "m",
		Network:     "*",
		Method:      "*",
		Finality:    common.DataFinalityStateUnknown,
		Empty:       common.CacheEmptyBehaviorAllow,
		MaxItemSize: &maxSize,
	}, conn)
	require.NoError(t, err)
	c := pathsCache(t, policy)

	req := blockRequest(t, "0x1")
	// Discriminating: the policy matches this request, and the response is
	// well-formed. Only the size guard can stop the write, and it must do so
	// without failing the caller.
	require.NoError(t, c.Set(context.Background(), req, cacheResponse(t, req, `{"number":"0x1"}`)))
	conn.AssertNotCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestCacheSet_AnUndecidablePolicyFailsTheWriteInsteadOfGuessing(t *testing.T) {
	conn := data.NewMockConnector("m")
	// An empty behaviour the writer does not recognise: it can neither store
	// nor drop the value on purpose, so it must say so.
	c := pathsCache(t, mockPolicy(t, conn, common.CacheEmptyBehavior(7)))

	req := blockRequest(t, "0x1")
	err := c.Set(context.Background(), req, cacheResponse(t, req, `{"number":"0x1"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown cache empty behavior")
	conn.AssertNotCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// --- shouldCacheResponse ---

func setTestPolicy(t *testing.T, empty common.CacheEmptyBehavior, minSize, maxSize *string) *data.CachePolicy {
	t.Helper()
	p, err := data.NewCachePolicy(&common.CachePolicyConfig{
		Connector:   "m",
		Network:     "*",
		Method:      "*",
		Finality:    common.DataFinalityStateUnknown,
		Empty:       empty,
		MinItemSize: minSize,
		MaxItemSize: maxSize,
	}, data.NewMockConnector("m"))
	require.NoError(t, err)
	return p
}

func TestShouldCacheResponse_AnErrorMemberIsNeverCached(t *testing.T) {
	req := blockRequest(t, "0x1")
	// A 200-OK carrying BOTH a populated result and an error member. Only this
	// shape tells "the writer honoured the error member" apart from "the result
	// was empty anyway".
	resp, err := jsonResultBesideError(req, `{"number":"0x1","hash":"0xfeed"}`, -32000, "reorg in progress")
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)

	ok, err := shouldCacheResponse(context.Background(), zerolog.Nop(), resp, jrr,
		setTestPolicy(t, common.CacheEmptyBehaviorAllow, nil, nil), common.DataFinalityStateUnknown)
	require.NoError(t, err)
	assert.False(t, ok, "an error member must veto the result beside it")
}

func TestShouldCacheResponse_SizeOutsideThePolicyLimitsIsRefused(t *testing.T) {
	req := blockRequest(t, "0x1")
	resp := cacheResponse(t, req, `{"number":"0x1"}`)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	maxSize := "4"

	ok, err := shouldCacheResponse(context.Background(), zerolog.Nop(), resp, jrr,
		setTestPolicy(t, common.CacheEmptyBehaviorAllow, nil, &maxSize), common.DataFinalityStateUnknown)
	require.NoError(t, err)
	assert.False(t, ok, "a body past maxItemSize must not be stored")
}

func TestShouldCacheResponse_EachEmptyBehaviourDecidesTheSameEmptyResultDifferently(t *testing.T) {
	req := blockRequest(t, "0x1")
	empty := cacheResponse(t, req, `null`)
	emptyJrr, err := empty.JsonRpcResponse()
	require.NoError(t, err)
	full := cacheResponse(t, req, `{"number":"0x1"}`)
	fullJrr, err := full.JsonRpcResponse()
	require.NoError(t, err)

	for _, tc := range []struct {
		behavior       common.CacheEmptyBehavior
		cacheEmpty     bool
		cachePopulated bool
		name           string
	}{
		{common.CacheEmptyBehaviorIgnore, false, true, "Ignore"},
		{common.CacheEmptyBehaviorAllow, true, true, "Allow"},
		{common.CacheEmptyBehaviorOnly, true, false, "Only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := setTestPolicy(t, tc.behavior, nil, nil)
			gotEmpty, err := shouldCacheResponse(context.Background(), zerolog.Nop(), empty, emptyJrr, policy, common.DataFinalityStateUnknown)
			require.NoError(t, err)
			assert.Equal(t, tc.cacheEmpty, gotEmpty, "empty result")

			gotFull, err := shouldCacheResponse(context.Background(), zerolog.Nop(), full, fullJrr, policy, common.DataFinalityStateUnknown)
			require.NoError(t, err)
			assert.Equal(t, tc.cachePopulated, gotFull, "populated result")
		})
	}
}

func TestShouldCacheResponse_AnUnknownEmptyBehaviourIsAnError(t *testing.T) {
	req := blockRequest(t, "0x1")
	resp := cacheResponse(t, req, `{"number":"0x1"}`)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)

	ok, err := shouldCacheResponse(context.Background(), zerolog.Nop(), resp, jrr,
		setTestPolicy(t, common.CacheEmptyBehavior(7), nil, nil), common.DataFinalityStateUnknown)
	require.Error(t, err, "an unrecognised empty behaviour must not silently store or drop the value")
	assert.False(t, ok)
}

func TestShouldCacheResponse_ARealtimeAnswerBehindTheTipIsNotStored(t *testing.T) {
	req := blockRequest(t, "latest")
	req.SetDirectives(&common.RequestDirectives{EnforceHighestBlock: true})
	req.SetNetwork(&testNetwork{highestLatest: 900, finalityState: common.DataFinalityStateRealtime})
	resp := cacheResponse(t, req, blockHeader(500)) // behind the tip
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)

	ok, err := shouldCacheResponse(context.Background(), zerolog.Nop(), resp, jrr,
		setTestPolicy(t, common.CacheEmptyBehaviorAllow, nil, nil), common.DataFinalityStateRealtime)
	require.NoError(t, err)
	assert.False(t, ok, "enforcement would never serve this value, so caching it can only poison later reads")
}

func TestShouldCacheResponse_ARealtimeAnswerAtTheTipIsStored(t *testing.T) {
	req := blockRequest(t, "latest")
	req.SetDirectives(&common.RequestDirectives{EnforceHighestBlock: true})
	req.SetNetwork(&testNetwork{highestLatest: 500, finalityState: common.DataFinalityStateRealtime})
	resp := cacheResponse(t, req, blockHeader(500))
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)

	ok, err := shouldCacheResponse(context.Background(), zerolog.Nop(), resp, jrr,
		setTestPolicy(t, common.CacheEmptyBehaviorAllow, nil, nil), common.DataFinalityStateRealtime)
	require.NoError(t, err)
	assert.True(t, ok, "the guard must only reject values BEHIND the tip")
}

// --- key generation ---

func TestGenerateKeys_AnEmptyBlockRefGetsItsOwnPartition(t *testing.T) {
	req := blockRequest(t, "0x1")

	withRef, key, err := generateKeysForJsonRpcRequest(req, "0x1")
	require.NoError(t, err)
	withoutRef, sameKey, err := generateKeysForJsonRpcRequest(req, "")
	require.NoError(t, err)

	assert.Equal(t, key, sameKey, "the request key does not depend on the block reference")
	assert.NotEqual(t, withRef, withoutRef)
	assert.Contains(t, withoutRef, ":nil", "an unresolved reference must not collide with a real block")
}
