package erpc

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The BDS wire declares three fields the gRPC surface used to accept and then
// drop: GetLogsRequest.blockHash, and chainId/chainGenesisHash on every
// request. A dropped field is worse than a missing one, because the client
// gets a success and never learns the setting did nothing. These tests pin the
// three answers: blockHash is honoured, chainId is honoured, and a
// chainGenesisHash the server cannot honour is refused by name.

// ---------------------------------------------------------------------------
// GetLogsRequest.blockHash
// ---------------------------------------------------------------------------

func TestGrpcGetLogs_SendsTheBlockHashFilter(t *testing.T) {
	h := newGrpcHarness(t, nil)
	h.node.reply("eth_getLogs", `[]`)

	hash := grpcBlockHash(0x64)
	ctx, cancel := h.callCtx(nil)
	defer cancel()
	_, err := h.rpc.GetLogs(ctx, &evm.GetLogsRequest{BlockHash: grpcHexBytes(t, hash)})
	require.NoError(t, err)

	filter := h.node.lastParams("eth_getLogs")[0].(map[string]interface{})
	assert.Equal(t, hash, filter["blockHash"],
		"a blockHash filter must reach the node, not leave an empty filter the node answers for its own default range")
	assert.NotContains(t, filter, "fromBlock")
	assert.NotContains(t, filter, "toBlock")
}

func TestGrpcGetLogs_RejectsABlockHashTogetherWithARange(t *testing.T) {
	h := newGrpcHarness(t, nil)
	h.node.reply("eth_getLogs", `[]`)

	from, to := uint64(0x10), uint64(0x20)
	ctx, cancel := h.callCtx(nil)
	defer cancel()
	_, err := h.rpc.GetLogs(ctx, &evm.GetLogsRequest{
		BlockHash: grpcHexBytes(t, grpcBlockHash(0x64)),
		FromBlock: &from,
		ToBlock:   &to,
	})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Contains(t, status.Convert(err).Message(), "blockHash")
	assert.Zero(t, h.node.callCount("eth_getLogs"),
		"the server must refuse the contradiction instead of picking one half of it")
}

// ---------------------------------------------------------------------------
// chainId and chainGenesisHash, on every service
// ---------------------------------------------------------------------------

// grpcPinnedCaller makes one call with the chain pinned in the request body.
// The check lives on the shared request path, so this table proves it runs for
// a unary RPC, a query stream and the block stream alike.
type grpcPinnedCaller struct {
	name string
	// hasGenesisField reports whether this request message declares
	// chainGenesisHash. QueryService requests declare chainId only.
	hasGenesisField bool
	// tipSubscription marks a call that answers only when a new head arrives.
	// An accepted one blocks instead of returning, so the accept test reads the
	// deadline as the proof that the server took the call.
	tipSubscription bool
	call            func(ctx context.Context, h *grpcHarness, chainId *uint64, genesis []byte) error
}

func grpcPinnedCallers() []grpcPinnedCaller {
	return []grpcPinnedCaller{
		{"GetLogs", true, false, func(ctx context.Context, h *grpcHarness, chainId *uint64, genesis []byte) error {
			_, err := h.rpc.GetLogs(ctx, &evm.GetLogsRequest{ChainId: chainId, ChainGenesisHash: genesis})
			return err
		}},
		{"GetBlockByNumber", true, false, func(ctx context.Context, h *grpcHarness, chainId *uint64, genesis []byte) error {
			_, err := h.rpc.GetBlockByNumber(ctx, &evm.GetBlockByNumberRequest{
				BlockNumber: "0x64", ChainId: chainId, ChainGenesisHash: genesis,
			})
			return err
		}},
		{"QueryBlocks", false, false, func(ctx context.Context, h *grpcHarness, chainId *uint64, genesis []byte) error {
			from, to := "0x1", "0x2"
			s, err := h.query.QueryBlocks(ctx, &evm.QueryBlocksRequest{
				FromBlock: &from, ToBlock: &to, ChainId: chainId,
			})
			if err != nil {
				return err
			}
			_, err = s.Recv()
			return err
		}},
		{"StreamBlocks", true, true, func(ctx context.Context, h *grpcHarness, chainId *uint64, genesis []byte) error {
			s, err := h.stream.StreamBlocks(ctx, &evm.StreamBlocksRequest{
				ChainId: chainId, ChainGenesisHash: genesis,
			})
			if err != nil {
				return err
			}
			_, err = s.Recv()
			return err
		}},
	}
}

func TestGrpcChainPin_RejectsAChainIdTheMetadataContradicts(t *testing.T) {
	h := newGrpcHarness(t, nil)
	h.node.reply("eth_getLogs", `[]`)
	h.node.replyBlockByNumber()

	other := uint64(999)
	for _, c := range grpcPinnedCallers() {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := h.callCtx(nil) // the metadata pins chain 123
			defer cancel()
			err := c.call(ctx, h, &other, nil)
			require.Error(t, err)
			assert.Equal(t, codes.InvalidArgument, status.Code(err))
			msg := status.Convert(err).Message()
			assert.Contains(t, msg, "999", "the message must name the chain the request asked for")
			assert.Contains(t, msg, fmt.Sprint(grpcTestChainID),
				"the message must name the chain the metadata asked for")
		})
	}
}

func TestGrpcChainPin_TakesTheChainFromTheRequestWhenTheMetadataOmitsIt(t *testing.T) {
	h := newGrpcHarness(t, nil)
	h.node.reply("eth_getLogs", `[]`)
	h.node.replyBlockByNumber()

	pinned := uint64(grpcTestChainID)
	for _, c := range grpcPinnedCallers() {
		t.Run(c.name, func(t *testing.T) {
			outer, cancel := h.callCtx(map[string]string{"x-erpc-chain-id": ""})
			defer cancel()
			ctx, bound := context.WithTimeout(outer, 3*time.Second)
			defer bound()

			err := c.call(ctx, h, &pinned, nil)
			if c.tipSubscription {
				assert.Equal(t, codes.DeadlineExceeded, status.Code(err),
					"the server took the call and held the subscription open; only a new head was missing")
				return
			}
			require.NoError(t, err,
				"a chainId in the request must be able to name the chain on its own")
		})
	}
}

func TestGrpcChainPin_AcceptsAChainIdThatAgreesWithTheMetadata(t *testing.T) {
	h := newGrpcHarness(t, nil)
	h.node.reply("eth_getLogs", `[]`)

	same := uint64(grpcTestChainID)
	ctx, cancel := h.callCtx(nil)
	defer cancel()
	_, err := h.rpc.GetLogs(ctx, &evm.GetLogsRequest{ChainId: &same})
	require.NoError(t, err, "an agreeing chainId must not read as a contradiction")
}

// eRPC picks a network by chain id alone. It holds no genesis hash for an EVM
// network, so it cannot honour chainGenesisHash. It says so by name instead of
// serving whatever the chain id resolves to.
func TestGrpcChainPin_RefusesAChainGenesisHashItCannotHonour(t *testing.T) {
	h := newGrpcHarness(t, nil)
	h.node.reply("eth_getLogs", `[]`)
	h.node.replyBlockByNumber()

	genesis := grpcHexBytes(t, grpcBlockHash(0))
	for _, c := range grpcPinnedCallers() {
		if !c.hasGenesisField {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := h.callCtx(nil)
			defer cancel()
			err := c.call(ctx, h, nil, genesis)
			require.Error(t, err)
			assert.Equal(t, codes.Unimplemented, status.Code(err))
			assert.Contains(t, status.Convert(err).Message(), "chainGenesisHash",
				"the refusal must name the field the client set")
		})
	}
}
