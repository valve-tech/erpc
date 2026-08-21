package evm

import (
	"context"
	"errors"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// resolveReceipts fetches a block's receipts BY HASH and treats the answer as
// canonical evidence. A node can answer a by-hash request with another fork's
// receipts, and accepting that both corrupts the corroboration and poisons the
// by-hash cache for as long as the entry lives. So the anchor check is the point
// of the function, and these tests read the cache as well as the return value.

const receiptsBlockHash = "0xabc123"

// receiptsView builds a chainView backed by a network whose Forward returns the
// given JSON-RPC result bytes, or the given error.
func receiptsView(t *testing.T, result string, forwardErr error) (*chainView, *mockNetwork) {
	t.Helper()
	n := new(mockNetwork)
	n.On("Id").Return("evm:1").Maybe()
	n.On("ProjectId").Return("receipts-anchor").Maybe()
	if forwardErr != nil {
		n.On("Forward", mock.Anything, mock.Anything).Return(nil, forwardErr).Maybe()
	} else {
		n.On("Forward", mock.Anything, mock.Anything).Return(
			common.NewNormalizedResponse().WithJsonRpcResponse(
				common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(result), nil),
			), nil,
		).Maybe()
	}
	return newChainView(n, 8, "", "grp", nil), n
}

// cachedReceiptCount reports how many receipts the view holds for a hash.
func cachedReceiptCount(c *chainView, blockHash string) int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.receipts[blockHash])
}

func TestChainView_ResolveReceipts_AcceptsAndCachesAnAnchoredAnswer(t *testing.T) {
	c, _ := receiptsView(t, `[
		{"blockHash":"0xabc123","blockNumber":"0x64","transactionHash":"0xt1"},
		{"blockHash":"0xABC123","blockNumber":"0x64","transactionHash":"0xt2"}
	]`, nil)

	got, ok := c.resolveReceipts(context.Background(), receiptsBlockHash)
	require.True(t, ok, "receipts that all claim the requested block are canonical evidence")
	require.Len(t, got, 2)
	assert.Equal(t, "0xt1", got[0].TransactionHash)
	assert.Equal(t, 2, cachedReceiptCount(c, receiptsBlockHash),
		"an accepted answer must land in the by-hash cache")
}

func TestChainView_ResolveReceipts_RefusesAnotherForksReceipts(t *testing.T) {
	// The node answers a by-hash request with receipts from a different block.
	c, _ := receiptsView(t, `[
		{"blockHash":"0xdeadbeef","blockNumber":"0x64","transactionHash":"0xt1"}
	]`, nil)

	got, ok := c.resolveReceipts(context.Background(), receiptsBlockHash)
	assert.False(t, ok, "receipts from another block are not evidence about this one")
	assert.Nil(t, got)
	assert.Equal(t, 0, cachedReceiptCount(c, receiptsBlockHash),
		"a mismatched answer must never reach the by-hash cache")
}

func TestChainView_ResolveReceipts_RefusesAPartiallyAnchoredAnswer(t *testing.T) {
	// One receipt in the batch belongs to a different block. The whole batch
	// fails: a partially honest answer still proves nothing about membership.
	c, _ := receiptsView(t, `[
		{"blockHash":"0xabc123","blockNumber":"0x64","transactionHash":"0xt1"},
		{"blockHash":"0xdeadbeef","blockNumber":"0x64","transactionHash":"0xt2"}
	]`, nil)

	got, ok := c.resolveReceipts(context.Background(), receiptsBlockHash)
	assert.False(t, ok)
	assert.Nil(t, got)
	assert.Equal(t, 0, cachedReceiptCount(c, receiptsBlockHash))
}

func TestChainView_ResolveReceipts_RefusesReceiptsWithNoBlockHash(t *testing.T) {
	// A receipt that carries no blockHash cannot prove membership either.
	c, _ := receiptsView(t, `[{"blockNumber":"0x64","transactionHash":"0xt1"}]`, nil)

	got, ok := c.resolveReceipts(context.Background(), receiptsBlockHash)
	assert.False(t, ok)
	assert.Nil(t, got)
}

func TestChainView_ResolveReceipts_RefusesAndDoesNotCacheAnEmptyAnswer(t *testing.T) {
	// A tip-lagged node answers []. Caching that would blind corroboration for
	// this block permanently, because the cache entry is keyed by an immutable
	// hash and would never be refreshed.
	c, _ := receiptsView(t, `[]`, nil)

	got, ok := c.resolveReceipts(context.Background(), receiptsBlockHash)
	assert.False(t, ok, "an empty answer is unavailability, not proof of an empty block")
	assert.Nil(t, got)
	assert.Equal(t, 0, cachedReceiptCount(c, receiptsBlockHash))
}

func TestChainView_ResolveReceipts_RefusesWhatItCannotFetch(t *testing.T) {
	t.Run("ForwardFails", func(t *testing.T) {
		c, _ := receiptsView(t, "", errors.New("upstream refused"))
		got, ok := c.resolveReceipts(context.Background(), receiptsBlockHash)
		assert.False(t, ok)
		assert.Nil(t, got)
	})

	t.Run("ResultIsNotAReceiptList", func(t *testing.T) {
		c, _ := receiptsView(t, `{"not":"a list"}`, nil)
		got, ok := c.resolveReceipts(context.Background(), receiptsBlockHash)
		assert.False(t, ok)
		assert.Nil(t, got)
	})

	t.Run("NoNetwork", func(t *testing.T) {
		c := newChainView(nil, 8, "", "grp", nil)
		got, ok := c.resolveReceipts(context.Background(), receiptsBlockHash)
		assert.False(t, ok, "a view with no network cannot fetch anything")
		assert.Nil(t, got)
	})
}
