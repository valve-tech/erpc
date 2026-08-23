package erpc

import (
	"testing"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover entry 166 in valve/upstream-bug-log.md. BDS declares chain
// identity on responses too, and eRPC left all of it unset on every reply, so a
// client could not confirm which chain answered.
//
// The fields split in two, and the tests below hold both halves apart. eRPC
// knows the chain id — the request routed by it — so it fills that. eRPC holds
// no genesis hash for an EVM network, so those fields stay unset; that is the
// same reason extractRequestInput refuses a request that pins one.

// TestGrpcGetBlock_NamesTheChainThatAnswered pins the half eRPC can answer, on
// both block handlers. Reading it through the pointer, not GetChainId(), is
// deliberate: the accessor returns 0 for "chain 0" and for "not set" alike,
// which is the ambiguity the field exists to remove.
func TestGrpcGetBlock_NamesTheChainThatAnswered(t *testing.T) {
	h := newGrpcHarness(t, nil)
	h.node.replyBlockByNumber()
	h.node.reply("eth_getBlockByHash", grpcBlockJSON(0x64, nil))
	ctx, cancel := h.callCtx(nil)
	defer cancel()

	t.Run("GetBlockByNumber", func(t *testing.T) {
		resp, err := h.rpc.GetBlockByNumber(ctx, &evm.GetBlockByNumberRequest{BlockNumber: "0x64"})
		require.NoError(t, err)
		require.NotNil(t, resp.ChainId, "the handler routed by chain id, so it can always name it")
		assert.Equal(t, uint64(grpcTestChainID), *resp.ChainId)
	})

	t.Run("GetBlockByHash", func(t *testing.T) {
		blockHash := grpcBlockHash(0x64)
		resp, err := h.rpc.GetBlockByHash(ctx, &evm.GetBlockByHashRequest{
			BlockHash: grpcHexBytes(t, blockHash),
		})
		require.NoError(t, err)
		require.NotNil(t, resp.ChainId)
		assert.Equal(t, uint64(grpcTestChainID), *resp.ChainId)
	})
}

// TestGrpcGetBlock_NamesTheChainEvenWhenThereIsNoBlock covers the early return
// for a null result. No block is still an answer from a chain, and a client
// that reads the chain id off every reply must not find a hole in this one.
func TestGrpcGetBlock_NamesTheChainEvenWhenThereIsNoBlock(t *testing.T) {
	h := newGrpcHarness(t, nil)
	h.node.reply("eth_getBlockByNumber", `null`)

	ctx, cancel := h.callCtx(nil)
	defer cancel()
	// Far above the node's head, which is the branch that answers empty
	// rather than NotFound.
	resp, err := h.rpc.GetBlockByNumber(ctx, &evm.GetBlockByNumberRequest{BlockNumber: "0x99999"})
	require.NoError(t, err)
	require.Nil(t, resp.Block, "test setup: this must be the empty-answer branch")
	require.NotNil(t, resp.ChainId, "an empty answer still came from a chain")
	assert.Equal(t, uint64(grpcTestChainID), *resp.ChainId)
}

// TestGrpcResponses_LeaveEveryGenesisHashUnset is the counterweight, and it is
// the more important of the two. eRPC selects a network by chain id alone and
// holds no genesis hash, so answering one would be an invention. If this ever
// reddens, somebody has filled a field from a value eRPC does not have.
func TestGrpcResponses_LeaveEveryGenesisHashUnset(t *testing.T) {
	h := newGrpcHarness(t, nil)
	h.node.replyBlockByNumber()
	ctx, cancel := h.callCtx(nil)
	defer cancel()

	block, err := h.rpc.GetBlockByNumber(ctx, &evm.GetBlockByNumberRequest{BlockNumber: "0x64"})
	require.NoError(t, err)
	assert.Empty(t, block.ChainGenesisHash, "eRPC holds no genesis hash to report")

	chain, err := h.rpc.ChainId(ctx, &evm.ChainIdRequest{})
	require.NoError(t, err)
	assert.Equal(t, uint64(grpcTestChainID), chain.ChainId, "the chain id itself is answerable")
	assert.Empty(t, chain.GenesisHash, "and the genesis hash beside it is not")
}
