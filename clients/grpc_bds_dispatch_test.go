package clients

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/blockchain-data-standards/manifesto/svm"
	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// SendRequest's method switch is the ONLY place a JSON-RPC method name becomes
// a BDS RPC. Every handler test in this package calls its handler directly, so
// the switch itself was never executed: an arm wired to the wrong handler, or
// dropped entirely, produced a plausible-looking response and no test noticed.
//
// The tests below drive SendRequest end to end against a server that answers
// the whole BDS surface and records which gRPC method it served. Asserting on
// that name — not just on "no error" — is what makes a swapped arm fail.

// ───────────────────────── remaining evm.RPCQueryService methods ─────────────

func (s *happyRPCServer) GetBlockByHash(ctx context.Context, _ *evm.GetBlockByHashRequest) (*evm.GetBlockResponse, error) {
	s.record("GetBlockByHash")
	s.recordMetadata(ctx)
	return &evm.GetBlockResponse{
		Block: &evm.BlockHeader{Number: s.blockNumber, Hash: []byte{0xab, 0xcd}},
	}, nil
}

func (s *happyRPCServer) GetLogs(ctx context.Context, _ *evm.GetLogsRequest) (*evm.GetLogsResponse, error) {
	s.record("GetLogs")
	s.recordMetadata(ctx)
	return &evm.GetLogsResponse{
		Logs: []*evm.Log{{
			Address:     []byte{0x01, 0x02},
			BlockNumber: s.blockNumber,
			LogIndex:    7,
		}},
	}, nil
}

func (s *happyRPCServer) GetTransactionByHash(ctx context.Context, req *evm.GetTransactionByHashRequest) (*evm.GetTransactionByHashResponse, error) {
	s.record("GetTransactionByHash")
	s.recordMetadata(ctx)
	s.mu.Lock()
	s.lastChainIdParam = req.ChainId
	s.mu.Unlock()
	return &evm.GetTransactionByHashResponse{
		Transaction: &evm.Transaction{Hash: []byte{0x11}, Nonce: 3},
	}, nil
}

// chainIdParamSeen returns the chainId assertion carried by the last
// GetTransactionByHash the server served (nil when the client sent none).
func (s *happyRPCServer) chainIdParamSeen() *uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastChainIdParam
}

func (s *happyRPCServer) GetTransactionReceipt(ctx context.Context, _ *evm.GetTransactionReceiptRequest) (*evm.GetTransactionReceiptResponse, error) {
	s.record("GetTransactionReceipt")
	s.recordMetadata(ctx)
	return &evm.GetTransactionReceiptResponse{
		Receipt: &evm.Receipt{TransactionHash: []byte{0x11}, BlockNumber: s.blockNumber},
	}, nil
}

func (s *happyRPCServer) GetBlockReceipts(ctx context.Context, _ *evm.GetBlockReceiptsRequest) (*evm.GetBlockReceiptsResponse, error) {
	s.record("GetBlockReceipts")
	s.recordMetadata(ctx)
	return &evm.GetBlockReceiptsResponse{
		Receipts: []*evm.Receipt{{TransactionHash: []byte{0x22}, BlockNumber: s.blockNumber}},
	}, nil
}

// ───────────────────────── evm.QueryService (streaming) ─────────────────────

// happyQueryServer answers every streaming query with a single page carrying
// one row of its own kind, and logs the method into the shared recorder.
type happyQueryServer struct {
	evm.UnimplementedQueryServiceServer
	seen *happyRPCServer
}

func (s *happyQueryServer) QueryBlocks(_ *evm.QueryBlocksRequest, stream grpc.ServerStreamingServer[evm.QueryBlocksResponse]) error {
	s.seen.record("QueryBlocks")
	return stream.Send(&evm.QueryBlocksResponse{
		Blocks:      []*evm.BlockHeader{{Number: 1}},
		FromBlock:   &evm.CursorBlock{Number: 1},
		CursorBlock: &evm.CursorBlock{Number: 1},
	})
}

func (s *happyQueryServer) QueryTransactions(_ *evm.QueryTransactionsRequest, stream grpc.ServerStreamingServer[evm.QueryTransactionsResponse]) error {
	s.seen.record("QueryTransactions")
	return stream.Send(&evm.QueryTransactionsResponse{
		Transactions: []*evm.Transaction{{Nonce: 1}},
		CursorBlock:  &evm.CursorBlock{Number: 1},
	})
}

func (s *happyQueryServer) QueryLogs(_ *evm.QueryLogsRequest, stream grpc.ServerStreamingServer[evm.QueryLogsResponse]) error {
	s.seen.record("QueryLogs")
	return stream.Send(&evm.QueryLogsResponse{
		Logs:        []*evm.Log{{LogIndex: 1}},
		CursorBlock: &evm.CursorBlock{Number: 1},
	})
}

func (s *happyQueryServer) QueryTraces(_ *evm.QueryTracesRequest, stream grpc.ServerStreamingServer[evm.QueryTracesResponse]) error {
	s.seen.record("QueryTraces")
	return stream.Send(&evm.QueryTracesResponse{
		Traces:      []*evm.Trace{{BlockNumber: 1}},
		CursorBlock: &evm.CursorBlock{Number: 1},
	})
}

func (s *happyQueryServer) QueryTransfers(_ *evm.QueryTransfersRequest, stream grpc.ServerStreamingServer[evm.QueryTransfersResponse]) error {
	s.seen.record("QueryTransfers")
	return stream.Send(&evm.QueryTransfersResponse{
		Transfers:   []*evm.NativeTransfer{{BlockNumber: 1}},
		CursorBlock: &evm.CursorBlock{Number: 1},
	})
}

// ───────────────────────── svm.RPCQueryService ──────────────────────────────

type happySvmServer struct {
	svm.UnimplementedRPCQueryServiceServer
	seen *happyRPCServer
}

func (s *happySvmServer) GetBlock(_ context.Context, req *svm.GetBlockRequest) (*svm.GetBlockResponse, error) {
	s.seen.record("SvmGetBlock")
	return &svm.GetBlockResponse{
		SlotStatus: svm.SlotStatus_SLOT_PRESENT,
		Block:      &svm.ConfirmedBlock{Slot: req.Slot, ParentSlot: req.Slot - 1},
	}, nil
}

// ───────────────────────── the dispatch table itself ────────────────────────

// TestSendRequest_DispatchRoutesEveryMethodToItsRPC walks every arm of the
// SendRequest switch. Each case asserts the EXACT gRPC method that reached the
// server, so an arm pointing at a neighbouring handler fails here even though
// the response would still parse.
func TestSendRequest_DispatchRoutesEveryMethodToItsRPC(t *testing.T) {
	cases := []struct {
		jsonRpcMethod string
		params        string
		wantGrpcCall  string
	}{
		{"eth_getBlockByNumber", `["0x64",false]`, "GetBlockByNumber"},
		{"eth_getBlockByHash", `["0x` + hash32 + `",false]`, "GetBlockByHash"},
		{"eth_getLogs", `[{"fromBlock":"0x1","toBlock":"0x2"}]`, "GetLogs"},
		{"eth_getTransactionByHash", `["0x` + hash32 + `"]`, "GetTransactionByHash"},
		{"eth_getTransactionReceipt", `["0x` + hash32 + `"]`, "GetTransactionReceipt"},
		{"eth_getBlockReceipts", `["0x64"]`, "GetBlockReceipts"},
		{"eth_chainId", `[]`, "ChainId"},
		{"eth_queryBlocks", `[{"fromBlock":"0x1","toBlock":"0x2"}]`, "QueryBlocks"},
		{"eth_queryTransactions", `[{"fromBlock":"0x1","toBlock":"0x2"}]`, "QueryTransactions"},
		{"eth_queryLogs", `[{"fromBlock":"0x1","toBlock":"0x2"}]`, "QueryLogs"},
		{"eth_queryTraces", `[{"fromBlock":"0x1","toBlock":"0x2"}]`, "QueryTraces"},
		{"eth_queryTransfers", `[{"fromBlock":"0x1","toBlock":"0x2"}]`, "QueryTransfers"},
		{"getBlock", `[42]`, "SvmGetBlock"},
	}
	require.Len(t, cases, 13, "one case per arm of the SendRequest switch")

	for _, tc := range cases {
		t.Run(tc.jsonRpcMethod, func(t *testing.T) {
			addr, server, stop := startHappyServer(t, 1, 0x64)
			defer stop()

			client := newTestClient(t, addr)
			body := `{"jsonrpc":"2.0","id":1,"method":"` + tc.jsonRpcMethod + `","params":` + tc.params + `}`
			resp, err := client.SendRequest(context.Background(), common.NewNormalizedRequest([]byte(body)))
			require.NoError(t, err)
			require.NotNil(t, resp)

			require.Equal(t, []string{tc.wantGrpcCall}, server.methodsSeen(),
				"%s must reach exactly one BDS RPC, and it must be %s", tc.jsonRpcMethod, tc.wantGrpcCall)

			jrr, err := resp.JsonRpcResponse()
			require.NoError(t, err)
			require.NotEmpty(t, jrr.GetResultString(),
				"%s must carry a result back to the caller", tc.jsonRpcMethod)
		})
	}
}

// hash32 is a 32-byte hex body (no 0x), reused for block and transaction
// hashes. The client parses hashes strictly, so a short string would fail in
// the handler before the dispatch arm proved anything.
const hash32 = "1111111111111111111111111111111111111111111111111111111111111111"

// TestSendRequest_GetBlockByNumberWithHashParam_UsesGetBlockByHash pins the one
// arm that is NOT a straight method→RPC mapping: eth_getBlockByNumber given a
// block-hash object must call GetBlockByHash, not GetBlockByNumber. Serving it
// as a number would ask the archive for block 0.
func TestSendRequest_GetBlockByNumberWithHashParam_UsesGetBlockByHash(t *testing.T) {
	addr, server, stop := startHappyServer(t, 1, 0x64)
	defer stop()

	client := newTestClient(t, addr)
	body := `{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":[{"blockHash":"0x` + hash32 + `"},false]}`
	resp, err := client.SendRequest(context.Background(), common.NewNormalizedRequest([]byte(body)))
	require.NoError(t, err)
	require.NotNil(t, resp)

	require.Equal(t, []string{"GetBlockByHash"}, server.methodsSeen(),
		"a blockHash param must route to GetBlockByHash even under eth_getBlockByNumber")
}

// TestSendRequest_ChainIdAssertionReachesTheWire proves the per-request chain
// assertion is stamped on the wire, not merely stored on the client. A
// chain-aware server rejects a request that lands on the wrong backend only if
// the field is actually populated, so dropping it silently disables the guard.
func TestSendRequest_ChainIdAssertionReachesTheWire(t *testing.T) {
	addr, server, stop := startHappyServer(t, 137, 0x64)
	defer stop()

	client := newTestClient(t, addr)
	body := `{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionByHash","params":["0x` + hash32 + `"]}`

	// Unarmed: the client knows no chain, so it must assert nothing rather
	// than guess a value the server would then validate against.
	_, err := client.SendRequest(context.Background(), common.NewNormalizedRequest([]byte(body)))
	require.NoError(t, err)
	require.Nil(t, server.chainIdParamSeen(),
		"an unarmed client must not invent a chainId assertion")

	// Armed after construction — the path the gRPC cache connector uses.
	client.SetExpectedChainId(137)
	_, err = client.SendRequest(context.Background(), common.NewNormalizedRequest([]byte(body)))
	require.NoError(t, err)
	got := server.chainIdParamSeen()
	require.NotNil(t, got, "an armed client must carry the chainId assertion")
	require.Equal(t, uint64(137), *got)
}

// TestSendRequest_QueryEnvelopeReachesCaller checks the streaming arms produce
// the query envelope rather than a bare result. The dispatch test above proves
// routing; this proves the aggregated page survives the trip back.
func TestSendRequest_QueryEnvelopeReachesCaller(t *testing.T) {
	addr, _, stop := startHappyServer(t, 1, 0x64)
	defer stop()

	client := newTestClient(t, addr)
	body := `{"jsonrpc":"2.0","id":1,"method":"eth_queryLogs","params":[{"fromBlock":"0x1","toBlock":"0x2"}]}`
	resp, err := client.SendRequest(context.Background(), common.NewNormalizedRequest([]byte(body)))
	require.NoError(t, err)

	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)

	var env struct {
		Data struct {
			Logs []map[string]interface{} `json:"logs"`
		} `json:"data"`
		CursorBlock map[string]interface{} `json:"cursorBlock"`
	}
	require.NoError(t, json.Unmarshal([]byte(jrr.GetResultString()), &env))
	require.Len(t, env.Data.Logs, 1, "the single streamed page must reach the caller")
	require.NotNil(t, env.CursorBlock, "the cursor must reach the caller for pagination to work")
}
