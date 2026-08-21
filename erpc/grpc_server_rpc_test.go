package erpc

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// grpcHexBytes decodes a 0x-prefixed hex string into the bytes a BDS request
// carries on the wire.
func grpcHexBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s[2:])
	require.NoError(t, err)
	return b
}

// grpcTwoProjectHarness gives the harness a second project on a second chain
// with its own node, so a test can prove a call lands where the metadata says.
func grpcTwoProjectHarness(t *testing.T) (*grpcHarness, *grpcTestNode) {
	t.Helper()
	other := newGrpcTestNodeOnChain(t, 456)
	h := newGrpcHarness(t, func(c *common.Config) {
		up := grpcTestUpstream("node2", other.URL)
		up.Evm.ChainId = 456
		c.Projects = append(c.Projects, &common.ProjectConfig{
			Id:        "other",
			Upstreams: []*common.UpstreamConfig{up},
			Networks: []*common.NetworkConfig{
				{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 456}},
			},
		})
	})
	return h, other
}

// ---------------------------------------------------------------------------
// Routing: which project and which network a call lands on
// ---------------------------------------------------------------------------

func TestGrpcChainId_ServesTheNetworksChainId(t *testing.T) {
	h := newGrpcHarness(t, nil)
	ctx, cancel := h.callCtx(nil)
	defer cancel()

	resp, err := h.rpc.ChainId(ctx, &evm.ChainIdRequest{})
	require.NoError(t, err)
	assert.Equal(t, uint64(grpcTestChainID), resp.ChainId)
}

// TestGrpcRouting_SendsACallToTheProjectAndNetworkNamedInTheMetadata is the
// routing claim: the same client connection reaches two different projects, and
// the answer proves which one served it.
func TestGrpcRouting_SendsACallToTheProjectAndNetworkNamedInTheMetadata(t *testing.T) {
	h, other := grpcTwoProjectHarness(t)

	mainCtx, cancel1 := h.callCtx(nil)
	defer cancel1()
	mainResp, err := h.rpc.ChainId(mainCtx, &evm.ChainIdRequest{})
	require.NoError(t, err)
	assert.Equal(t, uint64(123), mainResp.ChainId)

	otherCtx, cancel2 := h.callCtx(map[string]string{
		"x-erpc-project":  "other",
		"x-erpc-chain-id": "456",
	})
	defer cancel2()
	otherResp, err := h.rpc.ChainId(otherCtx, &evm.ChainIdRequest{})
	require.NoError(t, err)
	assert.Equal(t, uint64(456), otherResp.ChainId)

	// And the upstream traffic follows the same header.
	other.reply("eth_getBlockByNumber", grpcBlockJSON(0x64, nil))
	blockCtx, cancel3 := h.callCtx(map[string]string{
		"x-erpc-project":  "other",
		"x-erpc-chain-id": "456",
	})
	defer cancel3()
	_, err = h.rpc.GetBlockByNumber(blockCtx, &evm.GetBlockByNumberRequest{BlockNumber: "0x64"})
	require.NoError(t, err)

	assert.Contains(t, other.firstParams("eth_getBlockByNumber"), "0x64",
		"the second project's node must have been asked for the block")
	assert.NotContains(t, h.node.firstParams("eth_getBlockByNumber"), "0x64",
		"the first project's node must not see another project's traffic")
}

func TestGrpcRouting_RejectsAnUnknownProject(t *testing.T) {
	h := newGrpcHarness(t, nil)
	ctx, cancel := h.callCtx(map[string]string{"x-erpc-project": "nope"})
	defer cancel()

	_, err := h.rpc.ChainId(ctx, &evm.ChainIdRequest{})
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
	assert.Contains(t, status.Convert(err).Message(), "nope")
}

func TestGrpcRouting_RejectsAnUnknownNetwork(t *testing.T) {
	h := newGrpcHarness(t, nil)
	ctx, cancel := h.callCtx(map[string]string{"x-erpc-chain-id": "999"})
	defer cancel()

	_, err := h.rpc.ChainId(ctx, &evm.ChainIdRequest{})
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
	assert.Contains(t, status.Convert(err).Message(), "999")
}

// ---------------------------------------------------------------------------
// Metadata validation
// ---------------------------------------------------------------------------

func TestGrpcMetadata_RejectsACallWithoutAProject(t *testing.T) {
	h := newGrpcHarness(t, nil)
	ctx, cancel := h.callCtx(map[string]string{"x-erpc-project": ""})
	defer cancel()

	_, err := h.rpc.ChainId(ctx, &evm.ChainIdRequest{})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Equal(t, "x-erpc-project metadata required", status.Convert(err).Message())

	// Control: the very same call succeeds once the header is there, so the
	// rejection is about the header and not about something else being broken.
	okCtx, okCancel := h.callCtx(nil)
	defer okCancel()
	_, err = h.rpc.ChainId(okCtx, &evm.ChainIdRequest{})
	require.NoError(t, err)
}

func TestGrpcMetadata_RejectsACallWithoutAChainId(t *testing.T) {
	h := newGrpcHarness(t, nil)
	ctx, cancel := h.callCtx(map[string]string{"x-erpc-chain-id": ""})
	defer cancel()

	_, err := h.rpc.ChainId(ctx, &evm.ChainIdRequest{})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Equal(t, "x-erpc-chain-id metadata required", status.Convert(err).Message())

	// Control: with the header back, the chain id is really read and used.
	okCtx, okCancel := h.callCtx(nil)
	defer okCancel()
	resp, err := h.rpc.ChainId(okCtx, &evm.ChainIdRequest{})
	require.NoError(t, err)
	assert.Equal(t, uint64(grpcTestChainID), resp.ChainId)
}

// TestGrpcMetadata_RejectsUnreadableCredentials covers the one auth-extraction
// failure a client can actually cause: basic auth that is not valid base64.
func TestGrpcMetadata_RejectsUnreadableCredentials(t *testing.T) {
	h := newGrpcHarness(t, nil)
	ctx, cancel := h.callCtx(map[string]string{"authorization": "Basic !!!not-base64!!!"})
	defer cancel()

	_, err := h.rpc.ChainId(ctx, &evm.ChainIdRequest{})
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
	assert.Contains(t, status.Convert(err).Message(), "base64")
}

func TestGrpcMetadata_AcceptsWellFormedCredentials(t *testing.T) {
	h := newGrpcHarness(t, nil)
	creds := base64.StdEncoding.EncodeToString([]byte("user:secret"))
	ctx, cancel := h.callCtx(map[string]string{"authorization": "Basic " + creds})
	defer cancel()

	resp, err := h.rpc.ChainId(ctx, &evm.ChainIdRequest{})
	require.NoError(t, err, "a project with no auth strategy must accept a readable credential")
	assert.Equal(t, uint64(grpcTestChainID), resp.ChainId)
}

// ---------------------------------------------------------------------------
// GetBlockByNumber / GetBlockByHash
// ---------------------------------------------------------------------------

func TestGrpcGetBlockByNumber_SendsTheNumberAndFlagAndServesTheHeader(t *testing.T) {
	h := newGrpcHarness(t, nil)
	h.node.reply("eth_getBlockByNumber", grpcBlockJSON(0x64, nil))

	ctx, cancel := h.callCtx(nil)
	defer cancel()
	resp, err := h.rpc.GetBlockByNumber(ctx, &evm.GetBlockByNumberRequest{
		BlockNumber:         "0x64",
		IncludeTransactions: true,
	})
	require.NoError(t, err)
	require.NotNil(t, resp.Block)
	assert.Equal(t, uint64(0x64), resp.Block.Number)
	assert.Equal(t, grpcHexBytes(t, grpcBlockHash(0x64)), resp.Block.Hash)

	params := h.node.lastParams("eth_getBlockByNumber")
	require.Len(t, params, 2)
	assert.Equal(t, "0x64", params[0], "the block number must reach the node verbatim")
	assert.Equal(t, true, params[1], "includeTransactions must reach the node")
}

func TestGrpcGetBlockByNumber_ServesTransactionHashesAndFullTransactions(t *testing.T) {
	h := newGrpcHarness(t, nil)
	txHash := "0x" + fmt.Sprintf("%064x", 0xbeef)
	fullTx := fmt.Sprintf(`{"hash":%q,"nonce":"0x1","blockNumber":"0x64","transactionIndex":"0x0",
		"from":"0x1111111111111111111111111111111111111111",
		"to":"0x2222222222222222222222222222222222222222",
		"value":"0x0","gas":"0x5208","gasPrice":"0x7","input":"0x","type":"0x0"}`, txHash)
	h.node.reply("eth_getBlockByNumber", grpcBlockJSON(0x64, []string{fullTx}))

	ctx, cancel := h.callCtx(nil)
	defer cancel()
	resp, err := h.rpc.GetBlockByNumber(ctx, &evm.GetBlockByNumberRequest{
		BlockNumber:         "0x64",
		IncludeTransactions: true,
	})
	require.NoError(t, err)
	require.Len(t, resp.Transactions, 1)
	assert.Equal(t, grpcHexBytes(t, txHash), resp.Transactions[0])
	require.Len(t, resp.FullTransactions, 1)
	assert.Equal(t, grpcHexBytes(t, txHash), resp.FullTransactions[0].Hash)
}

func TestGrpcGetBlockByNumber_MapsAnUndecodableBlockToInternal(t *testing.T) {
	h := newGrpcHarness(t, nil)
	h.node.reply("eth_getBlockByNumber", `"not-a-block-object"`)

	ctx, cancel := h.callCtx(nil)
	defer cancel()
	_, err := h.rpc.GetBlockByNumber(ctx, &evm.GetBlockByNumberRequest{BlockNumber: "0x64"})
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
}

func TestGrpcGetBlockByHash_SendsTheHashAsHexAndServesTheHeader(t *testing.T) {
	h := newGrpcHarness(t, nil)
	h.node.reply("eth_getBlockByHash", grpcBlockJSON(0x64, nil))

	ctx, cancel := h.callCtx(nil)
	defer cancel()
	hash := grpcHexBytes(t, grpcBlockHash(0x64))
	resp, err := h.rpc.GetBlockByHash(ctx, &evm.GetBlockByHashRequest{
		BlockHash:           hash,
		IncludeTransactions: false,
	})
	require.NoError(t, err)
	require.NotNil(t, resp.Block)
	assert.Equal(t, uint64(0x64), resp.Block.Number)

	params := h.node.lastParams("eth_getBlockByHash")
	require.Len(t, params, 2)
	assert.Equal(t, grpcBlockHash(0x64), params[0],
		"the 32 request bytes must become the 0x hash the node expects")
	assert.Equal(t, false, params[1])
}

func TestGrpcGetBlockByHash_MapsAnUndecodableBlockToInternal(t *testing.T) {
	h := newGrpcHarness(t, nil)
	h.node.reply("eth_getBlockByHash", `"not-a-block-object"`)

	ctx, cancel := h.callCtx(nil)
	defer cancel()
	_, err := h.rpc.GetBlockByHash(ctx, &evm.GetBlockByHashRequest{
		BlockHash: grpcHexBytes(t, grpcBlockHash(0x64)),
	})
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
}

// ---------------------------------------------------------------------------
// GetLogs — the filter translation
// ---------------------------------------------------------------------------

// TestGrpcGetLogs_TranslatesTheWholeFilterIntoJsonRpc is the densest decision in
// the file: five shapes of the BDS filter must become the exact eth_getLogs
// object a node understands.
func TestGrpcGetLogs_TranslatesTheWholeFilterIntoJsonRpc(t *testing.T) {
	h := newGrpcHarness(t, nil)
	h.node.reply("eth_getLogs", `[]`)

	addrA := "0x1111111111111111111111111111111111111111"
	addrB := "0x2222222222222222222222222222222222222222"
	topic0 := "0x" + fmt.Sprintf("%064x", 0xaa)
	topic2a := "0x" + fmt.Sprintf("%064x", 0xbb)
	topic2b := "0x" + fmt.Sprintf("%064x", 0xcc)

	from, to := uint64(0x10), uint64(0x20)
	ctx, cancel := h.callCtx(nil)
	defer cancel()
	_, err := h.rpc.GetLogs(ctx, &evm.GetLogsRequest{
		FromBlock: &from,
		ToBlock:   &to,
		Addresses: [][]byte{grpcHexBytes(t, addrA), grpcHexBytes(t, addrB)},
		Topics: []*evm.TopicFilter{
			{Values: [][]byte{grpcHexBytes(t, topic0)}},
			{}, // a wildcard position
			{Values: [][]byte{grpcHexBytes(t, topic2a), grpcHexBytes(t, topic2b)}},
		},
	})
	require.NoError(t, err)

	params := h.node.lastParams("eth_getLogs")
	require.Len(t, params, 1)
	filter, ok := params[0].(map[string]interface{})
	require.True(t, ok)

	assert.Equal(t, "0x10", filter["fromBlock"], "block bounds go out as hex quantities")
	assert.Equal(t, "0x20", filter["toBlock"])
	assert.Equal(t, []interface{}{addrA, addrB}, filter["address"],
		"two addresses go out as a list")
	assert.Equal(t, []interface{}{
		topic0,
		nil,
		[]interface{}{topic2a, topic2b},
	}, filter["topics"],
		"a one-value position collapses to a scalar, an empty one becomes null, a multi-value one stays a list")
}

func TestGrpcGetLogs_SendsASingleAddressAsAScalar(t *testing.T) {
	h := newGrpcHarness(t, nil)
	h.node.reply("eth_getLogs", `[]`)

	addr := "0x3333333333333333333333333333333333333333"
	ctx, cancel := h.callCtx(nil)
	defer cancel()
	_, err := h.rpc.GetLogs(ctx, &evm.GetLogsRequest{Addresses: [][]byte{grpcHexBytes(t, addr)}})
	require.NoError(t, err)

	filter := h.node.lastParams("eth_getLogs")[0].(map[string]interface{})
	assert.Equal(t, addr, filter["address"], "one address goes out as a scalar, not a list")
	assert.NotContains(t, filter, "fromBlock", "an unset bound must not be invented")
	assert.NotContains(t, filter, "toBlock")
	assert.NotContains(t, filter, "topics")
}

func TestGrpcGetLogs_ServesDecodedLogs(t *testing.T) {
	h := newGrpcHarness(t, nil)
	addr := "0x1111111111111111111111111111111111111111"
	topic := "0x" + fmt.Sprintf("%064x", 0xaa)
	txHash := "0x" + fmt.Sprintf("%064x", 0xdd)
	h.node.reply("eth_getLogs", fmt.Sprintf(`[{
		"address":%q,"topics":[%q],"data":"0xdeadbeef",
		"blockNumber":"0x11","blockHash":%q,
		"transactionHash":%q,"transactionIndex":"0x2","logIndex":"0x3"
	}]`, addr, topic, grpcBlockHash(0x11), txHash))

	from, to := uint64(0x10), uint64(0x20)
	ctx, cancel := h.callCtx(nil)
	defer cancel()
	resp, err := h.rpc.GetLogs(ctx, &evm.GetLogsRequest{FromBlock: &from, ToBlock: &to})
	require.NoError(t, err)
	require.Len(t, resp.Logs, 1)

	got := resp.Logs[0]
	assert.Equal(t, grpcHexBytes(t, addr), got.Address)
	require.Len(t, got.Topics, 1)
	assert.Equal(t, grpcHexBytes(t, topic), got.Topics[0])
	assert.Equal(t, []byte{0xde, 0xad, 0xbe, 0xef}, got.Data)
	assert.Equal(t, uint64(0x11), got.BlockNumber)
	assert.Equal(t, grpcHexBytes(t, txHash), got.TransactionHash)
	assert.Equal(t, uint32(2), got.TransactionIndex)
	assert.Equal(t, uint32(3), got.LogIndex)
}

func TestGrpcGetLogs_MapsAnUndecodableLogToInternal(t *testing.T) {
	h := newGrpcHarness(t, nil)
	h.node.reply("eth_getLogs", `[{"address":"0xnothex","topics":[],"data":"0x","blockNumber":"0x11","logIndex":"0x0","transactionIndex":"0x0"}]`)

	from, to := uint64(0x10), uint64(0x20)
	ctx, cancel := h.callCtx(nil)
	defer cancel()
	_, err := h.rpc.GetLogs(ctx, &evm.GetLogsRequest{FromBlock: &from, ToBlock: &to})
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
}

// ---------------------------------------------------------------------------
// Transactions and receipts
// ---------------------------------------------------------------------------

func TestGrpcGetTransactionByHash_SendsTheHashAndServesTheTransaction(t *testing.T) {
	h := newGrpcHarness(t, nil)
	txHash := "0x" + fmt.Sprintf("%064x", 0xbeef)
	h.node.reply("eth_getTransactionByHash", fmt.Sprintf(`{
		"hash":%q,"nonce":"0x1","blockNumber":"0x64","blockHash":%q,"transactionIndex":"0x0",
		"from":"0x1111111111111111111111111111111111111111",
		"to":"0x2222222222222222222222222222222222222222",
		"value":"0x0","gas":"0x5208","gasPrice":"0x7","input":"0x","type":"0x0"}`,
		txHash, grpcBlockHash(0x64)))

	ctx, cancel := h.callCtx(nil)
	defer cancel()
	resp, err := h.rpc.GetTransactionByHash(ctx, &evm.GetTransactionByHashRequest{
		TransactionHash: grpcHexBytes(t, txHash),
	})
	require.NoError(t, err)
	require.NotNil(t, resp.Transaction)
	assert.Equal(t, grpcHexBytes(t, txHash), resp.Transaction.Hash)
	assert.Equal(t, txHash, h.node.lastParams("eth_getTransactionByHash")[0])
}

func TestGrpcGetTransactionByHash_ServesAnEmptyResponseForAnUnknownHash(t *testing.T) {
	h := newGrpcHarness(t, nil)
	h.node.reply("eth_getTransactionByHash", `null`)

	ctx, cancel := h.callCtx(nil)
	defer cancel()
	resp, err := h.rpc.GetTransactionByHash(ctx, &evm.GetTransactionByHashRequest{
		TransactionHash: grpcHexBytes(t, "0x"+fmt.Sprintf("%064x", 0xdead)),
	})
	require.NoError(t, err, "an unknown transaction is an empty answer, not a failure")
	assert.Nil(t, resp.Transaction)
}

func TestGrpcGetTransactionReceipt_ServesTheReceipt(t *testing.T) {
	h := newGrpcHarness(t, nil)
	txHash := "0x" + fmt.Sprintf("%064x", 0xbeef)
	h.node.reply("eth_getTransactionReceipt", grpcReceiptJSON(txHash, 0x64, 0))

	ctx, cancel := h.callCtx(nil)
	defer cancel()
	resp, err := h.rpc.GetTransactionReceipt(ctx, &evm.GetTransactionReceiptRequest{
		TransactionHash: grpcHexBytes(t, txHash),
	})
	require.NoError(t, err)
	require.NotNil(t, resp.Receipt)
	assert.Equal(t, grpcHexBytes(t, txHash), resp.Receipt.TransactionHash)
	assert.Equal(t, uint64(0x64), resp.Receipt.BlockNumber)
	assert.Equal(t, txHash, h.node.lastParams("eth_getTransactionReceipt")[0])
}

func TestGrpcGetTransactionReceipt_ServesAnEmptyResponseForAnUnknownHash(t *testing.T) {
	h := newGrpcHarness(t, nil)
	h.node.reply("eth_getTransactionReceipt", `null`)

	ctx, cancel := h.callCtx(nil)
	defer cancel()
	resp, err := h.rpc.GetTransactionReceipt(ctx, &evm.GetTransactionReceiptRequest{
		TransactionHash: grpcHexBytes(t, "0x"+fmt.Sprintf("%064x", 0xdead)),
	})
	require.NoError(t, err)
	assert.Nil(t, resp.Receipt)
}

func TestGrpcGetBlockReceipts_RejectsARequestNamingNoBlock(t *testing.T) {
	h := newGrpcHarness(t, nil)
	ctx, cancel := h.callCtx(nil)
	defer cancel()

	_, err := h.rpc.GetBlockReceipts(ctx, &evm.GetBlockReceiptsRequest{})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Equal(t, "blockNumber or blockHash required", status.Convert(err).Message())
	assert.Zero(t, h.node.callCount("eth_getBlockReceipts"),
		"a request naming no block must not reach an upstream")
}

func TestGrpcGetBlockReceipts_ServesReceiptsForABlockNumber(t *testing.T) {
	h := newGrpcHarness(t, nil)
	txHash := "0x" + fmt.Sprintf("%064x", 0xbeef)
	h.node.reply("eth_getBlockReceipts", "["+grpcReceiptJSON(txHash, 0x64, 0)+"]")

	number := "0x64"
	ctx, cancel := h.callCtx(nil)
	defer cancel()
	resp, err := h.rpc.GetBlockReceipts(ctx, &evm.GetBlockReceiptsRequest{BlockNumber: &number})
	require.NoError(t, err)
	require.Len(t, resp.Receipts, 1)
	assert.Equal(t, grpcHexBytes(t, txHash), resp.Receipts[0].TransactionHash)
	assert.Equal(t, "0x64", h.node.lastParams("eth_getBlockReceipts")[0])
}

// TestGrpcGetBlockReceipts_RejectsABlockNumberTogetherWithABlockHash replaces
// a test that pinned the old precedence: the handler took the hash and dropped
// the number without a word. BDS declares the two mutually exclusive, so a
// client that sets both gets told, not answered for a block it did not name.
func TestGrpcGetBlockReceipts_RejectsABlockNumberTogetherWithABlockHash(t *testing.T) {
	h := newGrpcHarness(t, nil)
	txHash := "0x" + fmt.Sprintf("%064x", 0xbeef)
	h.node.reply("eth_getBlockReceipts", "["+grpcReceiptJSON(txHash, 0x64, 0)+"]")

	number := "0x999"
	ctx, cancel := h.callCtx(nil)
	defer cancel()
	_, err := h.rpc.GetBlockReceipts(ctx, &evm.GetBlockReceiptsRequest{
		BlockNumber: &number,
		BlockHash:   grpcHexBytes(t, grpcBlockHash(0x64)),
	})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Zero(t, h.node.callCount("eth_getBlockReceipts"),
		"the server must refuse the contradiction instead of silently picking one side")
}

// grpcReceiptJSON builds a receipt complete enough for JsonRpcReceipt.ToProto.
func grpcReceiptJSON(txHash string, blockNumber uint64, index int) string {
	return fmt.Sprintf(`{
		"transactionHash":%q,"transactionIndex":"0x%x",
		"blockHash":%q,"blockNumber":"0x%x",
		"from":"0x1111111111111111111111111111111111111111",
		"to":"0x2222222222222222222222222222222222222222",
		"cumulativeGasUsed":"0x5208","gasUsed":"0x5208","effectiveGasPrice":"0x7",
		"contractAddress":null,"logs":[],"logsBloom":"0x%0512x","status":"0x1","type":"0x0"
	}`, txHash, index, grpcBlockHash(blockNumber), blockNumber, 0)
}

// ---------------------------------------------------------------------------
// Upstream failures → gRPC status codes, end to end
// ---------------------------------------------------------------------------

// TestGrpcErrors_MapAnUpstreamFailureOntoTheRightStatusCode drives the real
// request path so the mapping is proved against the errors eRPC actually
// produces, not hand-built ones.
func TestGrpcErrors_MapAnUpstreamFailureOntoTheRightStatusCode(t *testing.T) {
	cases := []struct {
		name       string
		httpStatus int
		rpcCode    int
		rpcMessage string
		want       codes.Code
	}{
		{"method not supported", 200, -32601, "the method eth_getBlockByHash does not exist", codes.Unimplemented},
		{"unauthorized", 401, -32000, "unauthorized", codes.Unauthenticated},
		{"rate limited", 429, -32000, "too many requests", codes.ResourceExhausted},
		{"unclassified server error", 500, -32603, "internal error", codes.Internal},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newGrpcHarness(t, nil)
			h.node.replyError("eth_getBlockByHash", tc.httpStatus, tc.rpcCode, tc.rpcMessage)

			ctx, cancel := h.callCtx(nil)
			defer cancel()
			_, err := h.rpc.GetBlockByHash(ctx, &evm.GetBlockByHashRequest{
				BlockHash: grpcHexBytes(t, grpcBlockHash(0x64)),
			})
			require.Error(t, err)
			assert.Equal(t, tc.want, status.Code(err),
				"status message was: %s", status.Convert(err).Message())
		})
	}
}
