package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// These tests cover the cache's remaining SET decisions and the block-reference
// failures on both paths. They reuse the fixture in
// json_rpc_cache_fixture_test.go, which builds the cache through its real
// constructor over a real connector.

// unresolvableBlockRequest names a block the reference extractor cannot read,
// so ResolveCacheBlockRef fails on both the read and the write path.
func unresolvableBlockRequest() *common.NormalizedRequest {
	return common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0xnothex",false]}`))
}

func TestCacheSet_AnUnreadableRequestBodyIsReported(t *testing.T) {
	f := newCacheFixture(t, cacheConfig(
		cacheConns(memoryConnector("mem")),
		cachePolicies(cachePolicyCfg("mem", "*")),
	))
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":`))

	err := f.cache.Set(context.Background(), req, cacheResponse(t, blockRequest(t, "0x1"), `{"number":"0x1"}`))

	require.Error(t, err, "a body the cache cannot parse must not be written under a guessed key")
}

func TestCacheSet_AnUnresolvableBlockReferenceIsReported(t *testing.T) {
	f := newCacheFixture(t, cacheConfig(
		cacheConns(memoryConnector("mem")),
		cachePolicies(cachePolicyCfg("mem", "*")),
	))
	req := unresolvableBlockRequest()

	err := f.cache.Set(context.Background(), req, cacheResponse(t, req, `{"number":"0x1"}`))

	// Discriminating: a block reference the cache cannot resolve is NOT the
	// same as one it resolves to the empty string. The empty string is a quiet
	// "do not cache this method"; an unreadable one means the key would be
	// wrong, and a wrong key serves this answer for another block.
	require.Error(t, err)
}

func TestCacheGet_AnUnresolvableBlockReferenceIsAMissNotAnError(t *testing.T) {
	f := newCacheFixture(t, cacheConfig(
		cacheConns(memoryConnector("mem")),
		cachePolicies(cachePolicyCfg("mem", "*")),
	))

	resp, err := f.cache.Get(context.Background(), unresolvableBlockRequest())

	// Discriminating: the READ path must fall through to the upstream, which
	// can still answer (and reject) the request properly. Surfacing the error
	// here would turn a cache-key problem into a failed user request.
	require.NoError(t, err)
	assert.Nil(t, resp)
}

func TestCacheSet_ANilResponseTakesItsFinalityFromTheRequest(t *testing.T) {
	conn := data.NewMockConnector("m")
	conn.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil).Maybe()
	c := pathsCache(t, mockPolicy(t, conn, common.CacheEmptyBehaviorIgnore))

	err := c.Set(context.Background(), blockRequest(t, "0x1"), nil)

	// A Set with no response must neither fail nor store anything. (Which
	// finality it picks is NOT observable here: every path that would act on
	// the choice needs a non-nil response body — see the report's note on
	// json_rpc_cache.go:818.)
	require.NoError(t, err)
	conn.AssertNotCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// --- construction ---

func TestNewEvmJsonRpcCache_AnUnparsableItemSizeFailsConstruction(t *testing.T) {
	bad := "512 kilobits"
	policy := cachePolicyCfg("mem", "*")
	policy.MaxItemSize = &bad

	_, err := newCacheFixtureErr(t, cacheConfig(cacheConns(memoryConnector("mem")), cachePolicies(policy)))

	// Discriminating: an unreadable size limit must stop startup. Treating it
	// as "no limit" would silently let oversized payloads into a store the
	// operator sized on purpose.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "maxItemSize")
}

// --- shouldCacheResponse: the not-yet-produced block ---

func TestShouldCacheResponse_AnEmptyAnswerForAFutureBlockIsNeverStored(t *testing.T) {
	network := &queryTestNetwork{
		cfg:    &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 1}},
		latest: 1000,
	}
	conn := data.NewMockConnector("m")

	for _, tc := range []struct {
		name  string
		block int64
		empty common.CacheEmptyBehavior
		want  bool
	}{
		// A block above the head does not exist yet. Its empty answer becomes
		// wrong the moment the chain produces the block, so no policy may keep
		// it — not even one that exists to store empties.
		{"AboveTheHeadUnderAllow", 1001, common.CacheEmptyBehaviorAllow, false},
		{"AboveTheHeadUnderOnly", 1001, common.CacheEmptyBehaviorOnly, false},
		// Discriminating: at or below the head the SAME empty answer is a real
		// answer, and the policy decides as usual.
		{"AtTheHeadUnderAllow", 1000, common.CacheEmptyBehaviorAllow, true},
		{"AtTheHeadUnderOnly", 1000, common.CacheEmptyBehaviorOnly, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := common.NewNormalizedRequest([]byte(
				`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x3e9",false]}`))
			req.SetNetwork(network)
			req.SetEvmBlockNumber(tc.block)
			resp := cacheResponse(t, req, `null`)
			jrr, err := resp.JsonRpcResponse()
			require.NoError(t, err)

			got, err := shouldCacheResponse(context.Background(), zerolog.Nop(), resp, jrr,
				mockPolicy(t, conn, tc.empty), common.DataFinalityStateUnknown)

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
