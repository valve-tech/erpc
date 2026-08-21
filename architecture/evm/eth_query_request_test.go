package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// parseQueryRequest turns one JSON object into the bounds a page is scanned
// with. Two things there decide correctness rather than convenience: the cursor
// arithmetic, which must step exactly one block past the last page in whichever
// direction the caller asked for, and the caps, which stop one request from
// scanning the whole chain.

// queryShimNetwork reports the given tips so block tags resolve.
func queryShimNetwork(latest, finalized int64) *mockNetwork {
	n := new(mockNetwork)
	n.On("Id").Return("evm:1").Maybe()
	n.On("ProjectId").Return("query-shim").Maybe()
	n.On("EvmHighestLatestBlockNumber", mock.Anything).Return(latest).Maybe()
	n.On("EvmHighestFinalizedBlockNumber", mock.Anything).Return(finalized).Maybe()
	return n
}

// queryRequest builds an eth_queryLogs request whose single param is the given
// JSON object.
func queryRequest(params string) *common.NormalizedRequest {
	return common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_queryLogs","params":[` + params + `]}`))
}

func TestParseQueryRequest_StepsPastTheCursorInTheRequestedDirection(t *testing.T) {
	ctx := context.Background()
	n := queryShimNetwork(1000, 900)

	t.Run("AscendingResumesOneBlockAbove", func(t *testing.T) {
		qr, err := parseQueryRequest(ctx, n, nil, queryRequest(
			`{"fromBlock":"0x1","toBlock":"0x64","cursor":{"number":"0x32"}}`))
		require.NoError(t, err)
		assert.Equal(t, uint64(51), qr.FromBlock,
			"the next ascending page starts one block past the cursor, or block 50 is served twice")
		assert.Equal(t, uint64(100), qr.ToBlock)
	})

	t.Run("DescendingResumesOneBlockBelow", func(t *testing.T) {
		qr, err := parseQueryRequest(ctx, n, nil, queryRequest(
			`{"fromBlock":"0x1","toBlock":"0x64","order":"desc","cursor":{"number":"0x32"}}`))
		require.NoError(t, err)
		assert.Equal(t, "desc", qr.Order)
		assert.Equal(t, uint64(49), qr.FromBlock,
			"the next descending page starts one block below the cursor")
	})

	t.Run("DescendingRefusesACursorAtBlockZero", func(t *testing.T) {
		// Stepping below block zero would wrap the unsigned counter to the top
		// of the range and scan the whole chain backwards.
		_, err := parseQueryRequest(ctx, n, nil, queryRequest(
			`{"fromBlock":"0x1","toBlock":"0x64","order":"desc","cursor":{"number":0}}`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "greater than zero")
		assert.True(t, common.HasErrorCode(err, common.ErrCodeInvalidRequest))
	})

	t.Run("CursorBlockIsAcceptedAsAnAlias", func(t *testing.T) {
		qr, err := parseQueryRequest(ctx, n, nil, queryRequest(
			`{"fromBlock":"0x1","toBlock":"0x64","cursorBlock":{"number":"0x32"}}`))
		require.NoError(t, err)
		assert.Equal(t, uint64(51), qr.FromBlock)
	})
}

func TestParseQueryRequest_OrdersTheBoundsToMatchTheDirection(t *testing.T) {
	ctx := context.Background()
	n := queryShimNetwork(1000, 900)

	t.Run("AscendingPutsTheLowBlockFirst", func(t *testing.T) {
		qr, err := parseQueryRequest(ctx, n, nil, queryRequest(
			`{"fromBlock":"0x64","toBlock":"0x1"}`))
		require.NoError(t, err)
		assert.Equal(t, uint64(1), qr.FromBlock, "reversed bounds must be swapped, not scanned empty")
		assert.Equal(t, uint64(100), qr.ToBlock)
	})

	t.Run("DescendingPutsTheHighBlockFirst", func(t *testing.T) {
		qr, err := parseQueryRequest(ctx, n, nil, queryRequest(
			`{"fromBlock":"0x1","toBlock":"0x64","order":"DESC"}`))
		require.NoError(t, err)
		assert.Equal(t, "desc", qr.Order, "the order value is compared case-insensitively")
		assert.Equal(t, uint64(100), qr.FromBlock)
		assert.Equal(t, uint64(1), qr.ToBlock)
	})

	t.Run("AnUnknownOrderIsRefused", func(t *testing.T) {
		_, err := parseQueryRequest(ctx, n, nil, queryRequest(
			`{"fromBlock":"0x1","toBlock":"0x64","order":"sideways"}`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid order")
	})
}

func TestParseQueryRequest_HoldsTheCapsThatBoundOneRequest(t *testing.T) {
	ctx := context.Background()
	n := queryShimNetwork(1000, 900)
	cfg := &common.EvmQueryShimConfig{MaxBlockRange: 10, MaxLimit: 5, DefaultLimit: 2}

	t.Run("DefaultLimitAppliesWhenNoneIsGiven", func(t *testing.T) {
		qr, err := parseQueryRequest(ctx, n, cfg, queryRequest(`{"fromBlock":"0x1","toBlock":"0x5"}`))
		require.NoError(t, err)
		assert.Equal(t, uint64(2), qr.Limit)
	})

	t.Run("ZeroLimitFallsBackToTheDefault", func(t *testing.T) {
		qr, err := parseQueryRequest(ctx, n, cfg, queryRequest(`{"fromBlock":"0x1","toBlock":"0x5","limit":0}`))
		require.NoError(t, err)
		assert.Equal(t, uint64(2), qr.Limit, "a zero limit means unset, not an empty page")
	})

	t.Run("LimitAboveTheCapIsRefused", func(t *testing.T) {
		_, err := parseQueryRequest(ctx, n, cfg, queryRequest(`{"fromBlock":"0x1","toBlock":"0x5","limit":50}`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "max limit")
	})

	t.Run("UnreadableLimitIsRefused", func(t *testing.T) {
		_, err := parseQueryRequest(ctx, n, cfg, queryRequest(`{"fromBlock":"0x1","toBlock":"0x5","limit":"banana"}`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid limit")
	})

	t.Run("BlockRangeAboveTheCapIsRefused", func(t *testing.T) {
		_, err := parseQueryRequest(ctx, n, cfg, queryRequest(`{"fromBlock":"0x1","toBlock":"0x3e8"}`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "max block range")
	})

	t.Run("ASingleBlockIsAlwaysInRange", func(t *testing.T) {
		qr, err := parseQueryRequest(ctx, n, cfg, queryRequest(`{"fromBlock":"0x5","toBlock":"0x5"}`))
		require.NoError(t, err, "one block can never exceed a range cap")
		assert.Equal(t, uint64(5), qr.FromBlock)
		assert.Equal(t, uint64(5), qr.ToBlock)
	})
}

func TestParseQueryRequest_RefusesWhatItCannotRead(t *testing.T) {
	ctx := context.Background()
	n := queryShimNetwork(1000, 900)

	t.Run("NoParams", func(t *testing.T) {
		nq := common.NewNormalizedRequest([]byte(
			`{"jsonrpc":"2.0","id":1,"method":"eth_queryLogs","params":[]}`))
		_, err := parseQueryRequest(ctx, n, nil, nq)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "query params are required")
	})

	t.Run("ParamIsNotAnObject", func(t *testing.T) {
		_, err := parseQueryRequest(ctx, n, nil, queryRequest(`"0x1"`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be an object")
	})

	t.Run("UnreadableCursor", func(t *testing.T) {
		_, err := parseQueryRequest(ctx, n, nil, queryRequest(
			`{"fromBlock":"0x1","toBlock":"0x5","cursor":"0x1"}`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid cursor block")
	})

	t.Run("UnsupportedBlockTag", func(t *testing.T) {
		_, err := parseQueryRequest(ctx, n, nil, queryRequest(
			`{"fromBlock":"pending","toBlock":"latest"}`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "pending block tag is not supported")
	})
}
