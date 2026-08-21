package clients

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeRpcQueryClient stands in for a BDS server's unary RPC surface. It
// records the proto request each handler built and returns a canned answer.
//
// A fake rather than a live server on purpose: the JSON-RPC -> proto
// translation (block-hash routing, topic positions, the chainId assertion) and
// the proto -> JSON-RPC rendering (null vs [] vs an error) are only observable
// at this seam. A live server would re-test grpc-go instead.
type fakeRpcQueryClient struct {
	evm.RPCQueryServiceClient

	gotBlockByNumber *evm.GetBlockByNumberRequest
	gotBlockByHash   *evm.GetBlockByHashRequest
	gotLogs          *evm.GetLogsRequest
	gotTxByHash      *evm.GetTransactionByHashRequest
	gotReceipt       *evm.GetTransactionReceiptRequest
	gotBlockReceipts *evm.GetBlockReceiptsRequest

	blockResp        *evm.GetBlockResponse
	logsResp         *evm.GetLogsResponse
	txResp           *evm.GetTransactionByHashResponse
	receiptResp      *evm.GetTransactionReceiptResponse
	blockReceiptResp *evm.GetBlockReceiptsResponse
	chainIdResp      *evm.ChainIdResponse

	err error
}

func (f *fakeRpcQueryClient) GetBlockByNumber(_ context.Context, in *evm.GetBlockByNumberRequest, _ ...grpc.CallOption) (*evm.GetBlockResponse, error) {
	f.gotBlockByNumber = in
	if f.err != nil {
		return nil, f.err
	}
	return f.blockResp, nil
}

func (f *fakeRpcQueryClient) GetBlockByHash(_ context.Context, in *evm.GetBlockByHashRequest, _ ...grpc.CallOption) (*evm.GetBlockResponse, error) {
	f.gotBlockByHash = in
	if f.err != nil {
		return nil, f.err
	}
	return f.blockResp, nil
}

func (f *fakeRpcQueryClient) GetLogs(_ context.Context, in *evm.GetLogsRequest, _ ...grpc.CallOption) (*evm.GetLogsResponse, error) {
	f.gotLogs = in
	if f.err != nil {
		return nil, f.err
	}
	return f.logsResp, nil
}

func (f *fakeRpcQueryClient) GetTransactionByHash(_ context.Context, in *evm.GetTransactionByHashRequest, _ ...grpc.CallOption) (*evm.GetTransactionByHashResponse, error) {
	f.gotTxByHash = in
	if f.err != nil {
		return nil, f.err
	}
	return f.txResp, nil
}

func (f *fakeRpcQueryClient) GetTransactionReceipt(_ context.Context, in *evm.GetTransactionReceiptRequest, _ ...grpc.CallOption) (*evm.GetTransactionReceiptResponse, error) {
	f.gotReceipt = in
	if f.err != nil {
		return nil, f.err
	}
	return f.receiptResp, nil
}

func (f *fakeRpcQueryClient) GetBlockReceipts(_ context.Context, in *evm.GetBlockReceiptsRequest, _ ...grpc.CallOption) (*evm.GetBlockReceiptsResponse, error) {
	f.gotBlockReceipts = in
	if f.err != nil {
		return nil, f.err
	}
	return f.blockReceiptResp, nil
}

func (f *fakeRpcQueryClient) ChainId(_ context.Context, _ *evm.ChainIdRequest, _ ...grpc.CallOption) (*evm.ChainIdResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.chainIdResp, nil
}

// bdsHandlerClient builds a client with no pool: every test here drives a
// handler directly with an explicit *bdsConn, which is the only seam below
// SendRequest's pool Pick.
func bdsHandlerClient(t *testing.T, chainId uint64) *GenericGrpcBdsClient {
	t.Helper()
	lg := zerolog.Nop()
	c := &GenericGrpcBdsClient{
		Url:        &url.URL{Scheme: "grpc", Host: "bds.localhost:50051"},
		logger:     &lg,
		projectId:  "prj1",
		upstreamId: "bds1",
	}
	c.expectedChainId.Store(chainId)
	return c
}

// jsonRpcFor parses a raw JSON-RPC request the way the client does.
func jsonRpcFor(t *testing.T, body string) (*common.NormalizedRequest, *common.JsonRpcRequest) {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(body))
	jrReq, err := req.JsonRpcRequest()
	if err != nil {
		t.Fatalf("JsonRpcRequest: %v", err)
	}
	return req, jrReq
}

// resultOf returns the JSON-RPC result bytes a handler produced.
func resultOf(t *testing.T, nr *common.NormalizedResponse) string {
	t.Helper()
	if nr == nil {
		t.Fatal("handler returned no response")
	}
	jrr, err := nr.JsonRpcResponse()
	if err != nil {
		t.Fatalf("JsonRpcResponse: %v", err)
	}
	return string(jrr.GetResultBytes())
}

const (
	testBlockHash = "0x1111111111111111111111111111111111111111111111111111111111111111"
	testTxHash    = "0x2222222222222222222222222222222222222222222222222222222222222222"
	testAddress   = "0x3333333333333333333333333333333333333333"
	testTopic     = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
)

// A block the server does not have must come back as JSON-RPC null, not as an
// error. An error here would mark the upstream failed and cordon a healthy
// node for asking about a block it legitimately does not hold.
func TestGetBlockByHash_AbsentBlockRendersNullNotAnError(t *testing.T) {
	c := bdsHandlerClient(t, 0)
	fake := &fakeRpcQueryClient{blockResp: &evm.GetBlockResponse{}}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByHash","params":["`+testBlockHash+`",false]}`)

	nr, err := c.handleGetBlockByHash(context.Background(), &bdsConn{rpcClient: fake}, req, jrReq)
	if err != nil {
		t.Fatalf("absent block became an error: %v", err)
	}
	if got := resultOf(t, nr); got != "null" {
		t.Fatalf("result = %s, want null", got)
	}
}

// The per-request chainId assertion is what makes a cross-wired endpoint fail
// loudly instead of answering with another chain's blocks. If it stops being
// stamped, a stale DNS record serves wrong data silently.
func TestGetBlockByHash_StampsTheChainIdAssertion(t *testing.T) {
	c := bdsHandlerClient(t, 137)
	fake := &fakeRpcQueryClient{blockResp: &evm.GetBlockResponse{}}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByHash","params":["`+testBlockHash+`",true]}`)

	if _, err := c.handleGetBlockByHash(context.Background(), &bdsConn{rpcClient: fake}, req, jrReq); err != nil {
		t.Fatalf("handleGetBlockByHash: %v", err)
	}
	if fake.gotBlockByHash.ChainId == nil {
		t.Fatal("no chainId assertion on the wire; a cross-wired endpoint would answer silently")
	}
	if *fake.gotBlockByHash.ChainId != 137 {
		t.Fatalf("chainId = %d, want 137", *fake.gotBlockByHash.ChainId)
	}
	if !fake.gotBlockByHash.IncludeTransactions {
		t.Fatal("includeTransactions=true was dropped; the caller would get hashes instead of transactions")
	}
}

// An unconfigured chainId must send NO assertion rather than 0. A literal 0
// would be a claim about the chain, and a validating server would reject every
// request from an upstream that simply has not been told its chain yet.
func TestGetBlockByHash_UnknownChainIdSendsNoAssertion(t *testing.T) {
	c := bdsHandlerClient(t, 0)
	fake := &fakeRpcQueryClient{blockResp: &evm.GetBlockResponse{}}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByHash","params":["`+testBlockHash+`",false]}`)

	if _, err := c.handleGetBlockByHash(context.Background(), &bdsConn{rpcClient: fake}, req, jrReq); err != nil {
		t.Fatalf("handleGetBlockByHash: %v", err)
	}
	if fake.gotBlockByHash.ChainId != nil {
		t.Fatalf("chainId = %d sent while unconfigured; want no assertion", *fake.gotBlockByHash.ChainId)
	}
}

// eth_getBlockByNumber with a {"blockHash":...} object must reach GetBlockByHash.
// Routing it to GetBlockByNumber instead would send an empty block number and
// return the wrong block or nothing at all.
func TestGetBlockByNumber_BlockHashObjectRoutesToTheHashRpc(t *testing.T) {
	c := bdsHandlerClient(t, 1)
	fake := &fakeRpcQueryClient{blockResp: &evm.GetBlockResponse{}}
	req, jrReq := jsonRpcFor(t,
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":[{"blockHash":"`+testBlockHash+`"},false]}`)

	if _, err := c.handleGetBlockByNumber(context.Background(), &bdsConn{rpcClient: fake}, req, jrReq); err != nil {
		t.Fatalf("handleGetBlockByNumber: %v", err)
	}
	if fake.gotBlockByHash == nil {
		t.Fatal("a blockHash param did not reach GetBlockByHash")
	}
	if fake.gotBlockByNumber != nil {
		t.Fatal("a blockHash param also called GetBlockByNumber")
	}
}

// A missing second param is a client mistake. Defaulting it would silently
// change what the caller asked for, so the handler must refuse.
func TestGetBlockByNumber_RefusesTooFewParams(t *testing.T) {
	c := bdsHandlerClient(t, 1)
	fake := &fakeRpcQueryClient{blockResp: &evm.GetBlockResponse{}}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1"]}`)

	if _, err := c.handleGetBlockByNumber(context.Background(), &bdsConn{rpcClient: fake}, req, jrReq); err == nil {
		t.Fatal("a one-param eth_getBlockByNumber was accepted")
	}
	if fake.gotBlockByNumber != nil {
		t.Fatal("an invalid request still reached the server")
	}
}

// A non-boolean includeTransactions must be refused, not coerced. Coercing it
// to false would drop transaction bodies the caller asked for.
func TestGetBlockByNumber_RefusesNonBooleanIncludeTransactions(t *testing.T) {
	c := bdsHandlerClient(t, 1)
	// The fake is armed with a servable answer on purpose: if the guard ever
	// stops firing, the handler succeeds and the assertion below reports that
	// cleanly instead of tripping over a nil response.
	fake := &fakeRpcQueryClient{blockResp: &evm.GetBlockResponse{}}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1","true"]}`)

	if _, err := c.handleGetBlockByNumber(context.Background(), &bdsConn{rpcClient: fake}, req, jrReq); err == nil {
		t.Fatal(`includeTransactions "true" (a string) was accepted as a boolean`)
	}
}

// eth_getLogs with no matches must render [], never null. ethers and viem
// treat a null logs result as a malformed response and throw.
func TestGetLogs_NoMatchesRenderAnEmptyArrayNotNull(t *testing.T) {
	c := bdsHandlerClient(t, 1)
	fake := &fakeRpcQueryClient{logsResp: &evm.GetLogsResponse{}}
	req, jrReq := jsonRpcFor(t,
		`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x10","toBlock":"0x20"}]}`)

	nr, err := c.handleGetLogs(context.Background(), &bdsConn{rpcClient: fake}, req, jrReq)
	if err != nil {
		t.Fatalf("handleGetLogs: %v", err)
	}
	if got := resultOf(t, nr); got != "[]" {
		t.Fatalf("result = %s, want []", got)
	}
}

// A block TAG (latest/pending/earliest) has no numeric bound this path can
// send. Leaving the bound nil would ask the server for the whole chain, so the
// handler must refuse and let the request fall through to a live upstream.
func TestGetLogs_RefusesBlockTagsRatherThanQueryingTheWholeChain(t *testing.T) {
	for _, tag := range []string{"latest", "pending", "earliest"} {
		t.Run(tag, func(t *testing.T) {
			c := bdsHandlerClient(t, 1)
			fake := &fakeRpcQueryClient{logsResp: &evm.GetLogsResponse{}}
			req, jrReq := jsonRpcFor(t,
				`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x10","toBlock":"`+tag+`"}]}`)

			if _, err := c.handleGetLogs(context.Background(), &bdsConn{rpcClient: fake}, req, jrReq); err == nil {
				t.Fatalf("toBlock %q was accepted", tag)
			}
			if fake.gotLogs != nil {
				t.Fatalf("toBlock %q still reached the server as an unbounded query", tag)
			}
		})
	}
}

// A single address string and an address array must both reach the wire. If
// the string case were dropped, the query would widen to every contract and
// return logs the caller never asked for.
func TestGetLogs_AcceptsBothAddressShapes(t *testing.T) {
	cases := []struct {
		name  string
		param string
		want  int
	}{
		{"single string", `"address":"` + testAddress + `"`, 1},
		{"array", `"address":["` + testAddress + `","0x4444444444444444444444444444444444444444"]`, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := bdsHandlerClient(t, 1)
			fake := &fakeRpcQueryClient{logsResp: &evm.GetLogsResponse{}}
			req, jrReq := jsonRpcFor(t,
				`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x10","toBlock":"0x20",`+tc.param+`}]}`)

			if _, err := c.handleGetLogs(context.Background(), &bdsConn{rpcClient: fake}, req, jrReq); err != nil {
				t.Fatalf("handleGetLogs: %v", err)
			}
			if got := len(fake.gotLogs.Addresses); got != tc.want {
				t.Fatalf("addresses on the wire = %d, want %d", got, tc.want)
			}
		})
	}
}

// The block bounds must survive hex decoding unchanged. An off-by-one or a
// dropped bound silently returns the wrong window of logs, which an indexer
// records as missing history.
func TestGetLogs_CarriesTheRequestedBlockWindow(t *testing.T) {
	c := bdsHandlerClient(t, 1)
	fake := &fakeRpcQueryClient{logsResp: &evm.GetLogsResponse{}}
	req, jrReq := jsonRpcFor(t,
		`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x10","toBlock":"0x20","topics":["`+testTopic+`"]}]}`)

	if _, err := c.handleGetLogs(context.Background(), &bdsConn{rpcClient: fake}, req, jrReq); err != nil {
		t.Fatalf("handleGetLogs: %v", err)
	}
	if *fake.gotLogs.FromBlock != 0x10 || *fake.gotLogs.ToBlock != 0x20 {
		t.Fatalf("window = [%d,%d], want [16,32]", *fake.gotLogs.FromBlock, *fake.gotLogs.ToBlock)
	}
	if len(fake.gotLogs.Topics) != 1 || len(fake.gotLogs.Topics[0].Values) != 1 {
		t.Fatalf("topics = %v, want one filter with one value", fake.gotLogs.Topics)
	}
}

// A matching log must render as a JSON-RPC log object. Rendering it as an
// empty array would look like "no matches" to every caller.
func TestGetLogs_RendersAMatchingLog(t *testing.T) {
	c := bdsHandlerClient(t, 1)
	fake := &fakeRpcQueryClient{logsResp: &evm.GetLogsResponse{Logs: []*evm.Log{{
		Address:     make([]byte, 20),
		BlockNumber: 0x11,
		LogIndex:    0,
	}}}}
	req, jrReq := jsonRpcFor(t,
		`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x10","toBlock":"0x20"}]}`)

	nr, err := c.handleGetLogs(context.Background(), &bdsConn{rpcClient: fake}, req, jrReq)
	if err != nil {
		t.Fatalf("handleGetLogs: %v", err)
	}
	got := resultOf(t, nr)
	if got == "[]" || got == "null" {
		t.Fatalf("a matching log rendered as %s", got)
	}
	var decoded []map[string]interface{}
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("result is not a JSON array: %v (%s)", err, got)
	}
	if len(decoded) != 1 {
		t.Fatalf("rendered %d logs, want 1", len(decoded))
	}
}

// An unknown transaction is null, not an error — the same reason as the block
// case. A pending or pruned transaction must not cordon the upstream.
func TestGetTransactionByHash_UnknownTransactionRendersNull(t *testing.T) {
	c := bdsHandlerClient(t, 1)
	fake := &fakeRpcQueryClient{txResp: &evm.GetTransactionByHashResponse{}}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionByHash","params":["`+testTxHash+`"]}`)

	nr, err := c.handleGetTransactionByHash(context.Background(), &bdsConn{rpcClient: fake}, req, jrReq)
	if err != nil {
		t.Fatalf("unknown transaction became an error: %v", err)
	}
	if got := resultOf(t, nr); got != "null" {
		t.Fatalf("result = %s, want null", got)
	}
	if len(fake.gotTxByHash.TransactionHash) != 32 {
		t.Fatalf("tx hash on the wire is %d bytes, want 32", len(fake.gotTxByHash.TransactionHash))
	}
}

// A transaction the server DOES have must render as an object. Pinning this
// alongside the null case is what stops "always null" from passing both.
func TestGetTransactionByHash_PresentTransactionRendersAnObject(t *testing.T) {
	c := bdsHandlerClient(t, 1)
	fake := &fakeRpcQueryClient{txResp: &evm.GetTransactionByHashResponse{
		Transaction: &evm.Transaction{Hash: make([]byte, 32), Nonce: 7, Value: "0x0"},
	}}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionByHash","params":["`+testTxHash+`"]}`)

	nr, err := c.handleGetTransactionByHash(context.Background(), &bdsConn{rpcClient: fake}, req, jrReq)
	if err != nil {
		t.Fatalf("handleGetTransactionByHash: %v", err)
	}
	got := resultOf(t, nr)
	if !strings.HasPrefix(got, "{") {
		t.Fatalf("result = %s, want a transaction object", got)
	}
	if !strings.Contains(got, `"nonce":"0x7"`) {
		t.Fatalf("result %s did not carry the transaction's nonce", got)
	}
}

// A transaction hash that is not hex must be refused before the RPC. Sending
// garbage bytes would make the server answer for a different transaction.
func TestGetTransactionByHash_RefusesANonHexHash(t *testing.T) {
	c := bdsHandlerClient(t, 1)
	fake := &fakeRpcQueryClient{txResp: &evm.GetTransactionByHashResponse{}}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionByHash","params":["not-a-hash"]}`)

	if _, err := c.handleGetTransactionByHash(context.Background(), &bdsConn{rpcClient: fake}, req, jrReq); err == nil {
		t.Fatal("a non-hex transaction hash was accepted")
	}
	if fake.gotTxByHash != nil {
		t.Fatal("a non-hex hash still reached the server")
	}
}

// A receipt that does not exist yet (a pending transaction) is null. Returning
// an error would make every wallet polling for a receipt look like an outage.
func TestGetTransactionReceipt_PendingTransactionRendersNull(t *testing.T) {
	c := bdsHandlerClient(t, 1)
	fake := &fakeRpcQueryClient{receiptResp: &evm.GetTransactionReceiptResponse{}}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["`+testTxHash+`"]}`)

	nr, err := c.handleGetTransactionReceipt(context.Background(), &bdsConn{rpcClient: fake}, req, jrReq)
	if err != nil {
		t.Fatalf("pending receipt became an error: %v", err)
	}
	if got := resultOf(t, nr); got != "null" {
		t.Fatalf("result = %s, want null", got)
	}
}

// A present receipt must render as an object, and the chainId assertion must
// ride along.
func TestGetTransactionReceipt_PresentReceiptRendersAnObject(t *testing.T) {
	c := bdsHandlerClient(t, 8453)
	fake := &fakeRpcQueryClient{receiptResp: &evm.GetTransactionReceiptResponse{
		Receipt: &evm.Receipt{
			TransactionHash:   make([]byte, 32),
			BlockNumber:       9,
			BlockHash:         make([]byte, 32),
			EffectiveGasPrice: "0x1",
		},
	}}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["`+testTxHash+`"]}`)

	nr, err := c.handleGetTransactionReceipt(context.Background(), &bdsConn{rpcClient: fake}, req, jrReq)
	if err != nil {
		t.Fatalf("handleGetTransactionReceipt: %v", err)
	}
	if got := resultOf(t, nr); !strings.HasPrefix(got, "{") {
		t.Fatalf("result = %s, want a receipt object", got)
	}
	if fake.gotReceipt.ChainId == nil || *fake.gotReceipt.ChainId != 8453 {
		t.Fatalf("chainId assertion = %v, want 8453", fake.gotReceipt.ChainId)
	}
}

// eth_getBlockReceipts on a block with no transactions must render [], not
// null — same client-library reason as eth_getLogs.
func TestGetBlockReceipts_EmptyBlockRendersAnEmptyArray(t *testing.T) {
	c := bdsHandlerClient(t, 1)
	fake := &fakeRpcQueryClient{blockReceiptResp: &evm.GetBlockReceiptsResponse{}}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["0x2a"]}`)

	nr, err := c.handleGetBlockReceipts(context.Background(), &bdsConn{rpcClient: fake}, req, jrReq)
	if err != nil {
		t.Fatalf("handleGetBlockReceipts: %v", err)
	}
	if got := resultOf(t, nr); got != "[]" {
		t.Fatalf("result = %s, want []", got)
	}
	if fake.gotBlockReceipts.BlockNumber == nil || *fake.gotBlockReceipts.BlockNumber != "0x2a" {
		t.Fatalf("blockNumber on the wire = %v, want 0x2a", fake.gotBlockReceipts.BlockNumber)
	}
}

// A 32-byte hash param must select the hash field, not the number field. A
// server given both, or neither, answers for the wrong block.
func TestGetBlockReceipts_HashParamSelectsTheHashField(t *testing.T) {
	c := bdsHandlerClient(t, 1)
	fake := &fakeRpcQueryClient{blockReceiptResp: &evm.GetBlockReceiptsResponse{}}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["`+testBlockHash+`"]}`)

	if _, err := c.handleGetBlockReceipts(context.Background(), &bdsConn{rpcClient: fake}, req, jrReq); err != nil {
		t.Fatalf("handleGetBlockReceipts: %v", err)
	}
	if len(fake.gotBlockReceipts.BlockHash) != 32 {
		t.Fatalf("blockHash on the wire is %d bytes, want 32", len(fake.gotBlockReceipts.BlockHash))
	}
	if fake.gotBlockReceipts.BlockNumber != nil {
		t.Fatalf("blockNumber %q was also set; the server sees an ambiguous request", *fake.gotBlockReceipts.BlockNumber)
	}
}

// A server answering for another chain is a cross-wired endpoint. It must be a
// loud transport failure — propagating its chainId would let the state poller
// track another chain's heads.
func TestChainId_MismatchIsATransportFailureNotAnAnswer(t *testing.T) {
	c := bdsHandlerClient(t, 1)
	fake := &fakeRpcQueryClient{chainIdResp: &evm.ChainIdResponse{ChainId: 137}}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`)

	nr, err := c.handleChainId(context.Background(), &bdsConn{rpcClient: fake}, req, jrReq)
	if err == nil {
		t.Fatalf("a chainId mismatch was served as a result: %s", resultOf(t, nr))
	}
	if !common.HasErrorCode(err, common.ErrCodeEndpointTransportFailure) {
		t.Fatalf("error = %v, want a transport failure", err)
	}
	if !strings.Contains(err.Error(), "137") || !strings.Contains(err.Error(), "1") {
		t.Fatalf("error %v does not name both the detected and expected chain", err)
	}
}

// A matching chainId renders as the hex string JSON-RPC requires. A decimal
// here breaks every client that parses the value with parseInt(v, 16).
func TestChainId_MatchRendersHex(t *testing.T) {
	c := bdsHandlerClient(t, 137)
	fake := &fakeRpcQueryClient{chainIdResp: &evm.ChainIdResponse{ChainId: 137}}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`)

	nr, err := c.handleChainId(context.Background(), &bdsConn{rpcClient: fake}, req, jrReq)
	if err != nil {
		t.Fatalf("handleChainId: %v", err)
	}
	if got := resultOf(t, nr); got != `"0x89"` {
		t.Fatalf("result = %s, want \"0x89\"", got)
	}
}

// With no expected chainId configured the server's answer passes through. This
// is the bootstrap case: eRPC learns the chain by asking.
func TestChainId_UnconfiguredClientAcceptsTheServersAnswer(t *testing.T) {
	c := bdsHandlerClient(t, 0)
	fake := &fakeRpcQueryClient{chainIdResp: &evm.ChainIdResponse{ChainId: 42161}}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`)

	nr, err := c.handleChainId(context.Background(), &bdsConn{rpcClient: fake}, req, jrReq)
	if err != nil {
		t.Fatalf("handleChainId: %v", err)
	}
	if got := resultOf(t, nr); got != `"0xa4b1"` {
		t.Fatalf("result = %s, want \"0xa4b1\"", got)
	}
}

// normalizeGrpcError must find the gRPC status through the fmt.Errorf wrapping
// every handler applies. Without the unwrap walk, a NOT_FOUND or a
// RESOURCE_EXHAUSTED would be flattened into a generic transport failure and
// the retry/cordon logic would treat a rate limit like a dead node.
func TestNormalizeGrpcError_FindsTheStatusThroughHandlerWrapping(t *testing.T) {
	c := bdsHandlerClient(t, 1)
	fake := &fakeRpcQueryClient{err: status.Error(codes.ResourceExhausted, "quota exceeded")}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionByHash","params":["`+testTxHash+`"]}`)

	_, handlerErr := c.handleGetTransactionByHash(context.Background(), &bdsConn{rpcClient: fake}, req, jrReq)
	if handlerErr == nil {
		t.Fatal("a ResourceExhausted RPC produced no error")
	}
	normalized := c.normalizeGrpcError(handlerErr)
	if normalized == nil {
		t.Fatal("normalizeGrpcError dropped the error")
	}
	if common.HasErrorCode(normalized, common.ErrCodeEndpointTransportFailure) {
		t.Fatalf("a gRPC status was flattened to a transport failure: %v", normalized)
	}
	if !strings.Contains(normalized.Error(), "quota exceeded") {
		t.Fatalf("normalized error %v lost the server's message", normalized)
	}
}

// An error carrying no gRPC status is a genuine transport problem. It must be
// CLASSIFIED as one — that is what cordons the upstream — and the original
// cause must stay reachable so an operator can see what actually broke.
func TestNormalizeGrpcError_NonStatusErrorBecomesATransportFailureKeepingItsCause(t *testing.T) {
	c := bdsHandlerClient(t, 1)
	cause := errors.New("dial tcp: no route to host")

	got := c.normalizeGrpcError(cause)
	if got == nil {
		t.Fatal("normalizeGrpcError dropped a plain error")
	}
	if !common.HasErrorCode(got, common.ErrCodeEndpointTransportFailure) {
		t.Fatalf("error %v was not classified as a transport failure; the upstream stays in rotation", got)
	}
	if !errors.Is(got, cause) {
		t.Fatalf("error %v no longer unwraps to the original cause", got)
	}
}

func TestNormalizeGrpcError_NilStaysNil(t *testing.T) {
	if got := bdsHandlerClient(t, 1).normalizeGrpcError(nil); got != nil {
		t.Fatalf("normalizeGrpcError(nil) = %v, want nil", got)
	}
}

// fakeQueryStream replays canned pages then EOF, or fails at a chosen page.
type fakeQueryStream[T any] struct {
	grpc.ClientStream
	pages []*T
	i     int
	err   error
}

func (s *fakeQueryStream[T]) Recv() (*T, error) {
	if s.err != nil && s.i >= len(s.pages) {
		return nil, s.err
	}
	if s.i >= len(s.pages) {
		return nil, io.EOF
	}
	p := s.pages[s.i]
	s.i++
	return p, nil
}

// fakeQueryClient serves the streaming query surface.
type fakeQueryClient struct {
	evm.QueryServiceClient
	gotBlocks *evm.QueryBlocksRequest
	blocks    []*evm.QueryBlocksResponse
	logs      []*evm.QueryLogsResponse
	openErr   error
	streamErr error
}

func (f *fakeQueryClient) QueryBlocks(_ context.Context, in *evm.QueryBlocksRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryBlocksResponse], error) {
	f.gotBlocks = in
	if f.openErr != nil {
		return nil, f.openErr
	}
	return &fakeQueryStream[evm.QueryBlocksResponse]{pages: f.blocks, err: f.streamErr}, nil
}

func (f *fakeQueryClient) QueryLogs(_ context.Context, _ *evm.QueryLogsRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryLogsResponse], error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	return &fakeQueryStream[evm.QueryLogsResponse]{pages: f.logs, err: f.streamErr}, nil
}

// A paged stream must be aggregated into ONE result. Serving only the first
// page would silently truncate an indexer's backfill.
func TestQueryBlocks_AggregatesEveryPage(t *testing.T) {
	c := bdsHandlerClient(t, 1)
	fake := &fakeQueryClient{blocks: []*evm.QueryBlocksResponse{
		{Blocks: []*evm.BlockHeader{{Number: 1}, {Number: 2}}, FromBlock: &evm.CursorBlock{Number: 1}, CursorBlock: &evm.CursorBlock{Number: 2}},
		{Blocks: []*evm.BlockHeader{{Number: 3}}, CursorBlock: &evm.CursorBlock{Number: 3}},
	}}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_queryBlocks","params":[{"fromBlock":"0x1","toBlock":"0x3"}]}`)

	nr, err := c.handleQueryBlocks(context.Background(), &bdsConn{queryClient: fake}, req, jrReq)
	if err != nil {
		t.Fatalf("handleQueryBlocks: %v", err)
	}
	var decoded struct {
		Data struct {
			Blocks []map[string]interface{} `json:"blocks"`
		} `json:"data"`
		FromBlock   map[string]interface{} `json:"fromBlock"`
		CursorBlock map[string]interface{} `json:"cursorBlock"`
	}
	if err := json.Unmarshal([]byte(resultOf(t, nr)), &decoded); err != nil {
		t.Fatalf("result is not a query envelope: %v", err)
	}
	if len(decoded.Data.Blocks) != 3 {
		t.Fatalf("aggregated %d blocks, want 3 — later pages were dropped", len(decoded.Data.Blocks))
	}
	// The cursor must be the LAST page's, or a caller resuming from it re-reads
	// the range it already has.
	if decoded.CursorBlock == nil {
		t.Fatal("cursorBlock missing; a paged caller cannot resume")
	}
	if got := decoded.CursorBlock["number"]; got != "0x3" {
		t.Fatalf("cursorBlock.number = %v, want 0x3 (the last page's)", got)
	}
	if got := decoded.FromBlock["number"]; got != "0x1" {
		t.Fatalf("fromBlock.number = %v, want 0x1 (the first page's)", got)
	}
}

// The chainId assertion must also reach the streaming query path.
func TestQueryBlocks_StampsTheChainIdAssertion(t *testing.T) {
	c := bdsHandlerClient(t, 10)
	fake := &fakeQueryClient{}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_queryBlocks","params":[{"fromBlock":"0x1","toBlock":"0x3"}]}`)

	if _, err := c.handleQueryBlocks(context.Background(), &bdsConn{queryClient: fake}, req, jrReq); err != nil {
		t.Fatalf("handleQueryBlocks: %v", err)
	}
	if fake.gotBlocks.ChainId == nil || *fake.gotBlocks.ChainId != 10 {
		t.Fatalf("chainId assertion = %v, want 10", fake.gotBlocks.ChainId)
	}
}

// A stream that dies halfway must NOT return the partial pages as a complete
// answer. A truncated backfill that looks successful is the worst outcome
// here — the caller records a gap it will never revisit.
func TestQueryBlocks_MidStreamFailureIsNotServedAsAPartialResult(t *testing.T) {
	c := bdsHandlerClient(t, 1)
	fake := &fakeQueryClient{
		blocks:    []*evm.QueryBlocksResponse{{Blocks: []*evm.BlockHeader{{Number: 1}}}},
		streamErr: status.Error(codes.Unavailable, "backend went away"),
	}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_queryBlocks","params":[{"fromBlock":"0x1","toBlock":"0x3"}]}`)

	nr, err := c.handleQueryBlocks(context.Background(), &bdsConn{queryClient: fake}, req, jrReq)
	if err == nil {
		t.Fatalf("a truncated stream was served as a result: %s", resultOf(t, nr))
	}
	if !strings.Contains(err.Error(), "backend went away") {
		t.Fatalf("error %v lost the stream failure reason", err)
	}
}

// A stream that cannot even be opened must surface the open error.
func TestQueryLogs_StreamOpenFailureSurfaces(t *testing.T) {
	c := bdsHandlerClient(t, 1)
	fake := &fakeQueryClient{openErr: status.Error(codes.PermissionDenied, "no access to this dataset")}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_queryLogs","params":[{"fromBlock":"0x1","toBlock":"0x3"}]}`)

	_, err := c.handleQueryLogs(context.Background(), &bdsConn{queryClient: fake}, req, jrReq)
	if err == nil {
		t.Fatal("a stream that never opened produced no error")
	}
	if !strings.Contains(err.Error(), "no access to this dataset") {
		t.Fatalf("error %v lost the server's reason", err)
	}
}

// Malformed query params must be refused before any RPC. Sending them would
// make the server scan a default range nobody asked for.
func TestQueryBlocks_RefusesMalformedParams(t *testing.T) {
	c := bdsHandlerClient(t, 1)
	fake := &fakeQueryClient{}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_queryBlocks","params":["not-an-object"]}`)

	if _, err := c.handleQueryBlocks(context.Background(), &bdsConn{queryClient: fake}, req, jrReq); err == nil {
		t.Fatal("a bare string was accepted as an eth_queryBlocks filter")
	}
	if fake.gotBlocks != nil {
		t.Fatal("malformed params still opened a stream")
	}
}

// applyQueryRangeBounds is the shared paging rule for all five query handlers.
// From/To are first-wins (the opening page states them once) and Cursor is
// last-wins (each page advances it). Getting either backwards makes a resumed
// query re-read or skip a range.
func TestApplyQueryRangeBounds_FirstWinsForBoundsLastWinsForCursor(t *testing.T) {
	var from, to, cursor *evm.CursorBlock

	page1 := &evm.QueryBlocksResponse{
		FromBlock:   &evm.CursorBlock{Number: 100},
		ToBlock:     &evm.CursorBlock{Number: 200},
		CursorBlock: &evm.CursorBlock{Number: 150},
	}
	page2 := &evm.QueryBlocksResponse{
		FromBlock:   &evm.CursorBlock{Number: 999},
		ToBlock:     &evm.CursorBlock{Number: 999},
		CursorBlock: &evm.CursorBlock{Number: 180},
	}
	applyQueryRangeBounds(&from, &to, &cursor, page1)
	applyQueryRangeBounds(&from, &to, &cursor, page2)

	if from.Number != 100 {
		t.Fatalf("fromBlock = %d, want 100 (first page wins)", from.Number)
	}
	if to.Number != 200 {
		t.Fatalf("toBlock = %d, want 200 (first page wins)", to.Number)
	}
	if cursor.Number != 180 {
		t.Fatalf("cursorBlock = %d, want 180 (last page wins)", cursor.Number)
	}
}

// A page that omits the cursor must not erase the cursor an earlier page set.
// Erasing it would restart a resumable query from the beginning.
func TestApplyQueryRangeBounds_AbsentCursorDoesNotEraseTheKnownOne(t *testing.T) {
	var from, to, cursor *evm.CursorBlock
	applyQueryRangeBounds(&from, &to, &cursor, &evm.QueryBlocksResponse{CursorBlock: &evm.CursorBlock{Number: 7}})
	applyQueryRangeBounds(&from, &to, &cursor, &evm.QueryBlocksResponse{})

	if cursor == nil || cursor.Number != 7 {
		t.Fatalf("cursorBlock = %v, want 7 kept", cursor)
	}
}

// recvQueryStream must stop cleanly on EOF and hand every page to the callback.
func TestRecvQueryStream_DeliversEveryPageThenStopsOnEOF(t *testing.T) {
	s := &fakeQueryStream[evm.QueryBlocksResponse]{pages: []*evm.QueryBlocksResponse{{}, {}, {}}}
	seen := 0
	if err := recvQueryStream(s.Recv, func(*evm.QueryBlocksResponse) { seen++ }); err != nil {
		t.Fatalf("recvQueryStream: %v", err)
	}
	if seen != 3 {
		t.Fatalf("callback saw %d pages, want 3", seen)
	}
}

// A stream error must be returned, not swallowed as an early EOF.
func TestRecvQueryStream_ReturnsTheStreamError(t *testing.T) {
	want := errors.New("stream reset")
	s := &fakeQueryStream[evm.QueryBlocksResponse]{err: want}
	err := recvQueryStream(s.Recv, func(*evm.QueryBlocksResponse) {})
	if !errors.Is(err, want) {
		t.Fatalf("recvQueryStream error = %v, want the stream error", err)
	}
}

// A query call with no params at all must become an empty object, not a nil
// that the manifesto parser would reject.
func TestJsonRpcParamsFor_NoParamsBecomesAnEmptyObject(t *testing.T) {
	_, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_queryBlocks","params":[]}`)
	raw, err := jsonRpcParamsFor(jrReq)
	if err != nil {
		t.Fatalf("jsonRpcParamsFor: %v", err)
	}
	if string(raw) != "{}" {
		t.Fatalf("params = %s, want {}", raw)
	}
}

// SetHeaders must merge, not replace: the constructor applies config headers
// and the cache connector adds its own later. A replace would drop the auth
// key and every request would 401.
func TestSetHeaders_MergesRatherThanReplaces(t *testing.T) {
	c := &GenericGrpcBdsClient{headers: map[string]string{"authorization": "Bearer k"}}
	c.SetHeaders(map[string]string{"x-tenant": "acme"})

	if c.headers["authorization"] != "Bearer k" {
		t.Fatal("SetHeaders dropped an existing header; the auth key would not reach the wire")
	}
	if c.headers["x-tenant"] != "acme" {
		t.Fatal("SetHeaders did not add the new header")
	}
}

// The constructor calls SetHeaders before the client is fully built, and the
// cache connector calls it on a client it may have failed to create. A nil
// receiver must return quietly rather than take the process down.
//
// The `h == nil` half of the same guard is deliberately NOT asserted here:
// ranging over a nil map is already a no-op in Go, so no test can distinguish
// the guard from its absence.
func TestSetHeaders_NilReceiverDoesNotPanic(t *testing.T) {
	var c *GenericGrpcBdsClient
	c.SetHeaders(map[string]string{"authorization": "Bearer k"})
}

// SetExpectedChainId arms the assertion after construction — the cache
// connector learns the chain by probing. If the arm did not take effect,
// every later request would go out unasserted.
func TestSetExpectedChainId_ArmsTheAssertionAfterConstruction(t *testing.T) {
	c := bdsHandlerClient(t, 0)
	fake := &fakeRpcQueryClient{blockResp: &evm.GetBlockResponse{}}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByHash","params":["`+testBlockHash+`",false]}`)

	c.SetExpectedChainId(56)
	if _, err := c.handleGetBlockByHash(context.Background(), &bdsConn{rpcClient: fake}, req, jrReq); err != nil {
		t.Fatalf("handleGetBlockByHash: %v", err)
	}
	if fake.gotBlockByHash.ChainId == nil || *fake.gotBlockByHash.ChainId != 56 {
		t.Fatalf("chainId assertion = %v after arming, want 56", fake.gotBlockByHash.ChainId)
	}
}

func TestGetType_IsGrpcBds(t *testing.T) {
	if got := bdsHandlerClient(t, 1).GetType(); got != ClientTypeGrpcBds {
		t.Fatalf("GetType() = %v, want %v", got, ClientTypeGrpcBds)
	}
}
