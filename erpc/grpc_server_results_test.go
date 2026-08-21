package erpc

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// This file covers what each unary handler does with a result it cannot use:
// an upstream failure, a JSON-RPC error, a body of the wrong shape, and a body
// that decodes but cannot become a proto message. Those four are the difference
// between a client seeing a clear status code and a client seeing a panic.

// grpcUnaryCase is one RPCQueryService method plus the JSON-RPC method it
// forwards and the params it forwards them with. The params matter because a
// static response only matches a request with the same params.
type grpcUnaryCase struct {
	name       string
	rpcMethod  string
	rpcParams  []interface{}
	call       func(ctx context.Context, c evm.RPCQueryServiceClient) error
	skipStatic bool
}

var grpcTestTxHash = "0x" + fmt.Sprintf("%064x", 0xbeef)

func grpcUnaryCases(t *testing.T) []grpcUnaryCase {
	t.Helper()
	blockHash := grpcBlockHash(0x64)
	// A block above the chain head. eRPC rewrites a null or failed answer for a
	// block BELOW the head into "missing data"; above the head it hands the
	// answer back untouched, which is what lets these tables see the handler's
	// own decision instead of eRPC's.
	number := "0x99999"
	from, to := uint64(0x10), uint64(0x12)

	return []grpcUnaryCase{
		{
			// eth_chainId is answered from network config before any upstream
			// or static response is consulted, so it takes the static path out.
			name: "ChainId", rpcMethod: "eth_chainId", rpcParams: []interface{}{}, skipStatic: true,
			call: func(ctx context.Context, c evm.RPCQueryServiceClient) error {
				_, err := c.ChainId(ctx, &evm.ChainIdRequest{})
				return err
			},
		},
		{
			name: "GetBlockByNumber", rpcMethod: "eth_getBlockByNumber", rpcParams: []interface{}{number, false},
			call: func(ctx context.Context, c evm.RPCQueryServiceClient) error {
				_, err := c.GetBlockByNumber(ctx, &evm.GetBlockByNumberRequest{BlockNumber: number})
				return err
			},
		},
		{
			name: "GetBlockByHash", rpcMethod: "eth_getBlockByHash", rpcParams: []interface{}{blockHash, false},
			call: func(ctx context.Context, c evm.RPCQueryServiceClient) error {
				_, err := c.GetBlockByHash(ctx, &evm.GetBlockByHashRequest{BlockHash: grpcHexBytes(t, blockHash)})
				return err
			},
		},
		{
			name: "GetLogs", rpcMethod: "eth_getLogs",
			rpcParams: []interface{}{map[string]interface{}{"fromBlock": "0x10", "toBlock": "0x12"}},
			call: func(ctx context.Context, c evm.RPCQueryServiceClient) error {
				_, err := c.GetLogs(ctx, &evm.GetLogsRequest{FromBlock: &from, ToBlock: &to})
				return err
			},
		},
		{
			name: "GetTransactionByHash", rpcMethod: "eth_getTransactionByHash", rpcParams: []interface{}{grpcTestTxHash},
			call: func(ctx context.Context, c evm.RPCQueryServiceClient) error {
				_, err := c.GetTransactionByHash(ctx, &evm.GetTransactionByHashRequest{
					TransactionHash: grpcHexBytes(t, grpcTestTxHash),
				})
				return err
			},
		},
		{
			name: "GetTransactionReceipt", rpcMethod: "eth_getTransactionReceipt", rpcParams: []interface{}{grpcTestTxHash},
			call: func(ctx context.Context, c evm.RPCQueryServiceClient) error {
				_, err := c.GetTransactionReceipt(ctx, &evm.GetTransactionReceiptRequest{
					TransactionHash: grpcHexBytes(t, grpcTestTxHash),
				})
				return err
			},
		},
		{
			name: "GetBlockReceipts", rpcMethod: "eth_getBlockReceipts", rpcParams: []interface{}{number},
			call: func(ctx context.Context, c evm.RPCQueryServiceClient) error {
				_, err := c.GetBlockReceipts(ctx, &evm.GetBlockReceiptsRequest{BlockNumber: &number})
				return err
			},
		},
	}
}

// TestGrpcUnary_EveryHandlerValidatesItsMetadataFirst proves no handler starts
// work on a request that names no project.
func TestGrpcUnary_EveryHandlerValidatesItsMetadataFirst(t *testing.T) {
	h := newGrpcHarness(t, nil)
	for _, tc := range grpcUnaryCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := h.callCtx(map[string]string{"x-erpc-project": ""})
			defer cancel()
			err := tc.call(ctx, h.rpc)
			require.Error(t, err)
			assert.Equal(t, codes.InvalidArgument, status.Code(err))
			assert.Equal(t, "x-erpc-project metadata required", status.Convert(err).Message())
			if len(tc.rpcParams) > 0 {
				// The state poller calls some of these methods on its own, so
				// the proof is that the HANDLER's own parameter never went out.
				assert.NotContains(t, h.node.firstParams(tc.rpcMethod), fmt.Sprint(tc.rpcParams[0]),
					"a request with no project must not reach an upstream")
			}
		})
	}
}

// TestGrpcUnary_EveryHandlerMapsAnUpstreamFailure proves each handler passes the
// processor's error through the status mapper instead of swallowing it.
func TestGrpcUnary_EveryHandlerMapsAnUpstreamFailure(t *testing.T) {
	for _, tc := range grpcUnaryCases(t) {
		if tc.skipStatic {
			continue // eth_chainId never reaches an upstream
		}
		t.Run(tc.name, func(t *testing.T) {
			h := newGrpcHarness(t, nil)
			h.node.replyError(tc.rpcMethod, 200, -32601, "the method "+tc.rpcMethod+" is not available")

			ctx, cancel := h.callCtx(nil)
			defer cancel()
			err := tc.call(ctx, h.rpc)
			require.Error(t, err)
			assert.Equal(t, codes.Unimplemented, status.Code(err),
				"an unsupported upstream method must reach the client as Unimplemented; got %s",
				status.Convert(err).Message())
		})
	}
}

// TestGrpcUnary_EveryHandlerReportsAJsonRpcError covers the branch after a
// SUCCESSFUL forward: the response carries a JSON-RPC error object, and the
// handler must turn that into a status rather than decode an empty result.
//
// A network static response is the deterministic way to produce that shape —
// it is served without contacting an upstream, so nothing can reclassify it.
func TestGrpcUnary_EveryHandlerReportsAJsonRpcError(t *testing.T) {
	for _, tc := range grpcUnaryCases(t) {
		if tc.skipStatic {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			h := newGrpcHarness(t, func(c *common.Config) {
				c.Projects[0].Networks[0].StaticResponses = []*common.StaticResponseConfig{{
					Method: tc.rpcMethod,
					Params: tc.rpcParams,
					Response: &common.StaticResponseBodyConfig{
						// -32602 is a client-side fault, so eRPC does not retry
						// it against an upstream and the answer the handler sees
						// is exactly the one configured here.
						Error: &common.StaticResponseErrorConfig{Code: -32602, Message: "static gremlins"},
					},
				}}
			})

			ctx, cancel := h.callCtx(nil)
			defer cancel()
			err := tc.call(ctx, h.rpc)
			require.Error(t, err)
			assert.Contains(t, status.Convert(err).Message(), "static gremlins",
				"the JSON-RPC error text must reach the client")
		})
	}
}

// TestGrpcUnary_EveryHandlerRejectsAResultOfTheWrongShape is the anti-panic
// test: a node that answers the right method with the wrong JSON must produce a
// status code, never a crash.
func TestGrpcUnary_EveryHandlerRejectsAResultOfTheWrongShape(t *testing.T) {
	for _, tc := range grpcUnaryCases(t) {
		if tc.skipStatic {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			h := newGrpcHarness(t, func(c *common.Config) {
				c.Projects[0].Networks[0].StaticResponses = []*common.StaticResponseConfig{{
					Method:   tc.rpcMethod,
					Params:   tc.rpcParams,
					Response: &common.StaticResponseBodyConfig{Result: "a result of entirely the wrong shape"},
				}}
			})

			ctx, cancel := h.callCtx(nil)
			defer cancel()
			err := tc.call(ctx, h.rpc)
			require.Error(t, err)
			assert.Equal(t, codes.Internal, status.Code(err))

			// The server must still be alive and answering.
			liveCtx, liveCancel := h.callCtx(nil)
			defer liveCancel()
			resp, liveErr := h.rpc.ChainId(liveCtx, &evm.ChainIdRequest{})
			require.NoError(t, liveErr, "a malformed result must not take the server down")
			assert.Equal(t, uint64(grpcTestChainID), resp.ChainId)
		})
	}
}

// TestGrpcUnary_RejectsABodyThatCannotBecomeAProtoMessage covers the last decode
// step: the JSON parses into eRPC's own struct but a hex field inside it is not
// a number, so the conversion to the wire message fails.
func TestGrpcUnary_RejectsABodyThatCannotBecomeAProtoMessage(t *testing.T) {
	number := "0x64"
	badBlock := map[string]interface{}{
		"number": "0x64", "hash": grpcBlockHash(0x64), "parentHash": grpcBlockHash(0x63),
		"gasLimit": "0xzzzz", "gasUsed": "0x0", "timestamp": "0x1", "size": "0x1",
	}
	badReceipt := map[string]interface{}{
		"transactionHash": grpcTestTxHash, "transactionIndex": "0x0",
		"blockHash": grpcBlockHash(0x64), "blockNumber": "0xzzzz",
		"gasUsed": "0x1", "cumulativeGasUsed": "0x1", "logs": []interface{}{},
	}
	badLog := map[string]interface{}{
		"address": "0xnothexatall", "topics": []interface{}{}, "data": "0x",
		"blockNumber": "0x10", "logIndex": "0x0", "transactionIndex": "0x0",
	}

	cases := []struct {
		name   string
		method string
		params []interface{}
		result interface{}
		call   func(ctx context.Context, c evm.RPCQueryServiceClient) error
	}{
		{
			"GetBlockByNumber", "eth_getBlockByNumber", []interface{}{number, false}, badBlock,
			func(ctx context.Context, c evm.RPCQueryServiceClient) error {
				_, err := c.GetBlockByNumber(ctx, &evm.GetBlockByNumberRequest{BlockNumber: number})
				return err
			},
		},
		{
			"GetBlockByHash", "eth_getBlockByHash", []interface{}{grpcBlockHash(0x64), false}, badBlock,
			func(ctx context.Context, c evm.RPCQueryServiceClient) error {
				_, err := c.GetBlockByHash(ctx, &evm.GetBlockByHashRequest{BlockHash: grpcHexBytes(t, grpcBlockHash(0x64))})
				return err
			},
		},
		{
			"GetTransactionReceipt", "eth_getTransactionReceipt", []interface{}{grpcTestTxHash}, badReceipt,
			func(ctx context.Context, c evm.RPCQueryServiceClient) error {
				_, err := c.GetTransactionReceipt(ctx, &evm.GetTransactionReceiptRequest{
					TransactionHash: grpcHexBytes(t, grpcTestTxHash),
				})
				return err
			},
		},
		{
			"GetBlockReceipts", "eth_getBlockReceipts", []interface{}{number}, []interface{}{badReceipt},
			func(ctx context.Context, c evm.RPCQueryServiceClient) error {
				_, err := c.GetBlockReceipts(ctx, &evm.GetBlockReceiptsRequest{BlockNumber: &number})
				return err
			},
		},
		{
			"GetTransactionByHash", "eth_getTransactionByHash", []interface{}{grpcTestTxHash},
			map[string]interface{}{
				"hash": grpcTestTxHash, "nonce": "0xzzzz", "blockNumber": "0x64",
				"from": "0x1111111111111111111111111111111111111111", "input": "0x", "gas": "0x1",
			},
			func(ctx context.Context, c evm.RPCQueryServiceClient) error {
				_, err := c.GetTransactionByHash(ctx, &evm.GetTransactionByHashRequest{
					TransactionHash: grpcHexBytes(t, grpcTestTxHash),
				})
				return err
			},
		},
		{
			"GetLogs", "eth_getLogs",
			[]interface{}{map[string]interface{}{"fromBlock": "0x10", "toBlock": "0x12"}},
			[]interface{}{badLog},
			func(ctx context.Context, c evm.RPCQueryServiceClient) error {
				from, to := uint64(0x10), uint64(0x12)
				_, err := c.GetLogs(ctx, &evm.GetLogsRequest{FromBlock: &from, ToBlock: &to})
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newGrpcHarness(t, func(c *common.Config) {
				c.Projects[0].Networks[0].StaticResponses = []*common.StaticResponseConfig{{
					Method:   tc.method,
					Params:   tc.params,
					Response: &common.StaticResponseBodyConfig{Result: tc.result},
				}}
			})
			ctx, cancel := h.callCtx(nil)
			defer cancel()
			err := tc.call(ctx, h.rpc)
			require.Error(t, err)
			assert.Equal(t, codes.Internal, status.Code(err))
		})
	}
}

// TestGrpcUnary_ServesAnEmptyAnswerForABlockThatDoesNotExistYet covers the
// explicit null branch. A block above the chain head is not missing data — it
// has not been produced — so the client gets an empty answer, not an error.
func TestGrpcUnary_ServesAnEmptyAnswerForABlockThatDoesNotExistYet(t *testing.T) {
	h := newGrpcHarness(t, nil)
	h.node.reply("eth_getBlockByNumber", `null`)

	ctx, cancel := h.callCtx(nil)
	defer cancel()
	// 0x99999 is far above the node's head of 0x3e8.
	resp, err := h.rpc.GetBlockByNumber(ctx, &evm.GetBlockByNumberRequest{BlockNumber: "0x99999"})
	require.NoError(t, err, "a block that is not mined yet is an empty answer, not a failure")
	assert.Nil(t, resp.Block)
	assert.Empty(t, resp.Transactions)
}

// TestGrpcUnary_ReportsNotFoundForAMissingHistoricalBlock is the other side of
// the same coin: below the head, a null block means the upstream does not have
// the data, and the client must be able to tell the two apart.
func TestGrpcUnary_ReportsNotFoundForAMissingHistoricalBlock(t *testing.T) {
	h := newGrpcHarness(t, nil)
	h.node.reply("eth_getBlockByNumber", `null`)

	ctx, cancel := h.callCtx(nil)
	defer cancel()
	_, err := h.rpc.GetBlockByNumber(ctx, &evm.GetBlockByNumberRequest{BlockNumber: "0x64"})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
}

// TestGrpcUnary_ServesAnEmptyAnswerForAnUnknownBlockHash covers the same null
// branch on the hash handler, where any unknown hash is simply absent.
func TestGrpcUnary_ServesAnEmptyAnswerForAnUnknownBlockHash(t *testing.T) {
	blockHash := grpcBlockHash(0x64)
	h := grpcHarnessWithStaticResult(t, "eth_getBlockByHash", []interface{}{blockHash, false}, nil)

	ctx, cancel := h.callCtx(nil)
	defer cancel()
	resp, err := h.rpc.GetBlockByHash(ctx, &evm.GetBlockByHashRequest{BlockHash: grpcHexBytes(t, blockHash)})
	require.NoError(t, err)
	assert.Nil(t, resp.Block)
}

// grpcHarnessWithStaticResult builds a harness whose network answers one method
// from config, so the test controls the exact result bytes the handler decodes.
func grpcHarnessWithStaticResult(t *testing.T, method string, params []interface{}, result interface{}) *grpcHarness {
	t.Helper()
	return newGrpcHarness(t, func(c *common.Config) {
		c.Projects[0].Networks[0].StaticResponses = []*common.StaticResponseConfig{{
			Method:   method,
			Params:   params,
			Response: &common.StaticResponseBodyConfig{Result: result},
		}}
	})
}

// ---------------------------------------------------------------------------
// ChainId's own decoding, reached by turning off the config short-circuit
// ---------------------------------------------------------------------------

// TestGrpcChainId_RejectsAChainIdTheUpstreamCannotExpress reaches ChainId's
// decoding by setting skipCacheRead, which makes eRPC ask the upstream instead
// of answering eth_chainId from network config.
func TestGrpcChainId_RejectsAChainIdTheUpstreamCannotExpress(t *testing.T) {
	cases := []struct {
		name   string
		result string
	}{
		{"not a string", `1234`},
		{"not a hex number", `"0xzzzz"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newGrpcHarness(t, func(c *common.Config) {
				c.Projects[0].Networks[0].DirectiveDefaults = &common.DirectiveDefaultsConfig{
					SkipCacheRead: true,
				}
			})
			h.node.reply("eth_chainId", tc.result)

			ctx, cancel := h.callCtx(nil)
			defer cancel()
			_, err := h.rpc.ChainId(ctx, &evm.ChainIdRequest{})
			require.Error(t, err)
			assert.Equal(t, codes.Internal, status.Code(err))
		})
	}
}

// TestGrpcChainId_ReportsAJsonRpcErrorFromTheChainIdCall covers ChainId's own
// "the answer carried an error" branch, again reached by skipping the config
// short-circuit.
func TestGrpcChainId_ReportsAJsonRpcErrorFromTheChainIdCall(t *testing.T) {
	h := newGrpcHarness(t, func(c *common.Config) {
		c.Projects[0].Networks[0].DirectiveDefaults = &common.DirectiveDefaultsConfig{SkipCacheRead: true}
		c.Projects[0].Networks[0].StaticResponses = []*common.StaticResponseConfig{{
			Method: "eth_chainId",
			Params: []interface{}{},
			Response: &common.StaticResponseBodyConfig{
				Error: &common.StaticResponseErrorConfig{Code: -32602, Message: "static gremlins"},
			},
		}}
	})

	ctx, cancel := h.callCtx(nil)
	defer cancel()
	_, err := h.rpc.ChainId(ctx, &evm.ChainIdRequest{})
	require.Error(t, err)
	assert.Contains(t, status.Convert(err).Message(), "static gremlins")
}

// TestGrpcChainId_AsksTheUpstreamWhenTheCacheReadIsSkipped is the control for
// the pair above: with the short-circuit off the answer really comes from the
// node, which is what makes the decoding tests meaningful.
func TestGrpcChainId_AsksTheUpstreamWhenTheCacheReadIsSkipped(t *testing.T) {
	h := newGrpcHarness(t, func(c *common.Config) {
		c.Projects[0].Networks[0].DirectiveDefaults = &common.DirectiveDefaultsConfig{
			SkipCacheRead: true,
		}
	})
	before := h.node.callCount("eth_chainId")

	ctx, cancel := h.callCtx(nil)
	defer cancel()
	resp, err := h.rpc.ChainId(ctx, &evm.ChainIdRequest{})
	require.NoError(t, err)
	assert.Equal(t, uint64(grpcTestChainID), resp.ChainId)
	assert.Greater(t, h.node.callCount("eth_chainId"), before,
		"with the cache read skipped the chain id must come from the upstream")
}

// ---------------------------------------------------------------------------
// Client IP
// ---------------------------------------------------------------------------

// TestGrpcClientIP_SkipsABlankTrustedHeaderName guards the loop against a
// configuration that leaves a blank entry: it must move on to the next header,
// not treat the blank as a match.
func TestGrpcClientIP_SkipsABlankTrustedHeaderName(t *testing.T) {
	gs := &GrpcServer{
		trustedForwarderIPs: map[string]struct{}{"127.0.0.1": {}},
		trustedIPHeaders:    []string{"", "x-real-ip"},
	}
	md := metadata.New(map[string]string{"x-real-ip": "203.0.113.9"})
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9000},
	})
	assert.Equal(t, "203.0.113.9", gs.grpcClientIP(ctx, md))
}

// TestGrpcClientIP_KeepsAnUntrustedPeersOwnAddress is the spoofing guard. An
// untrusted caller may claim any address in a forwarded header; eRPC must
// attribute the request to the socket it really came from, or rate limits and
// audit trails can be evaded by anyone who sets one header.
func TestGrpcClientIP_KeepsAnUntrustedPeersOwnAddress(t *testing.T) {
	gs := &GrpcServer{
		trustedForwarderIPs: map[string]struct{}{"10.0.0.1": {}},
		trustedIPHeaders:    []string{"x-forwarded-for"},
	}
	md := metadata.New(map[string]string{"x-forwarded-for": "203.0.113.9"})
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("198.51.100.25"), Port: 9000},
	})
	assert.Equal(t, "198.51.100.25", gs.grpcClientIP(ctx, md),
		"a peer that is not a trusted forwarder must not be able to name its own client IP")
}

// TestGrpcClientIP_FallsBackToThePeerWhenNoHeaderCarriesAUsableIp keeps the
// trusted-forwarder path honest: a trusted peer sending an unusable header
// still gets attributed to its own address.
func TestGrpcClientIP_FallsBackToThePeerWhenNoHeaderCarriesAUsableIp(t *testing.T) {
	gs := &GrpcServer{
		trustedForwarderIPs: map[string]struct{}{"127.0.0.1": {}},
		trustedIPHeaders:    []string{"x-forwarded-for"},
	}
	md := metadata.New(map[string]string{"x-forwarded-for": "not-an-ip"})
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9000},
	})
	assert.Equal(t, "127.0.0.1", gs.grpcClientIP(ctx, md))
}

// TestGrpcClientIP_IsEmptyWithoutAPeer covers the in-process case where no peer
// is attached at all.
func TestGrpcClientIP_IsEmptyWithoutAPeer(t *testing.T) {
	gs := &GrpcServer{}
	assert.Equal(t, "", gs.grpcClientIP(context.Background(), metadata.New(nil)))
}
