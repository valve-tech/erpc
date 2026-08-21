package clients

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/blockchain-data-standards/manifesto/evm"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The BDS query surface has five streaming handlers. Three of them —
// transactions, traces and transfers — had no test at all, so nothing showed
// that they aggregate their pages, stamp the chainId assertion, or refuse a
// truncated stream. They are near-identical in shape to the two that ARE
// tested, which is exactly why an aggregation bug in one of them would go
// unnoticed: the reviewer reads the covered twin and assumes the rest.
//
// Each handler aggregates a DIFFERENT set of repeated fields, so the tests
// below check every one of them: dropping any single append still returns a
// plausible-looking envelope.

// --- fake stream clients for the three untested handlers ---

type fakeTxQueryClient struct {
	evm.QueryServiceClient
	got       *evm.QueryTransactionsRequest
	pages     []*evm.QueryTransactionsResponse
	openErr   error
	streamErr error
}

func (f *fakeTxQueryClient) QueryTransactions(_ context.Context, in *evm.QueryTransactionsRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryTransactionsResponse], error) {
	f.got = in
	if f.openErr != nil {
		return nil, f.openErr
	}
	return &fakeQueryStream[evm.QueryTransactionsResponse]{pages: f.pages, err: f.streamErr}, nil
}

type fakeTraceQueryClient struct {
	evm.QueryServiceClient
	got       *evm.QueryTracesRequest
	pages     []*evm.QueryTracesResponse
	openErr   error
	streamErr error
}

func (f *fakeTraceQueryClient) QueryTraces(_ context.Context, in *evm.QueryTracesRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryTracesResponse], error) {
	f.got = in
	if f.openErr != nil {
		return nil, f.openErr
	}
	return &fakeQueryStream[evm.QueryTracesResponse]{pages: f.pages, err: f.streamErr}, nil
}

type fakeTransferQueryClient struct {
	evm.QueryServiceClient
	got       *evm.QueryTransfersRequest
	pages     []*evm.QueryTransfersResponse
	openErr   error
	streamErr error
}

func (f *fakeTransferQueryClient) QueryTransfers(_ context.Context, in *evm.QueryTransfersRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[evm.QueryTransfersResponse], error) {
	f.got = in
	if f.openErr != nil {
		return nil, f.openErr
	}
	return &fakeQueryStream[evm.QueryTransfersResponse]{pages: f.pages, err: f.streamErr}, nil
}

// queryEnvelope is the shape every eth_query* handler produces. The per-kind
// slices are read individually so a handler that aggregates one field and
// drops another is caught.
type queryEnvelope struct {
	Data struct {
		Blocks       []map[string]interface{} `json:"blocks"`
		Transactions []map[string]interface{} `json:"transactions"`
		Traces       []map[string]interface{} `json:"traces"`
		Transfers    []map[string]interface{} `json:"transfers"`
	} `json:"data"`
	FromBlock   map[string]interface{} `json:"fromBlock"`
	ToBlock     map[string]interface{} `json:"toBlock"`
	CursorBlock map[string]interface{} `json:"cursorBlock"`
}

func decodeQueryEnvelope(t *testing.T, raw string) queryEnvelope {
	t.Helper()
	var out queryEnvelope
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("result is not a query envelope: %v\n%s", err, raw)
	}
	return out
}

// --- eth_queryTransactions ---

// Every page's transactions AND its blocks must survive aggregation. A caller
// backfilling a range gets a silently short answer otherwise, and records a gap
// it will never revisit.
func TestQueryTransactions_AggregatesEveryPageOfBothSlices(t *testing.T) {
	c := bdsHandlerClient(t, 1)
	fake := &fakeTxQueryClient{pages: []*evm.QueryTransactionsResponse{
		{
			Transactions: []*evm.Transaction{{Nonce: 1}, {Nonce: 2}},
			Blocks:       []*evm.BlockHeader{{Number: 1}},
			FromBlock:    &evm.CursorBlock{Number: 1},
			CursorBlock:  &evm.CursorBlock{Number: 1},
		},
		{
			Transactions: []*evm.Transaction{{Nonce: 3}},
			Blocks:       []*evm.BlockHeader{{Number: 2}},
			CursorBlock:  &evm.CursorBlock{Number: 2},
		},
	}}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_queryTransactions","params":[{"fromBlock":"0x1","toBlock":"0x2"}]}`)

	nr, err := c.handleQueryTransactions(context.Background(), &bdsConn{queryClient: fake}, req, jrReq)
	if err != nil {
		t.Fatalf("handleQueryTransactions: %v", err)
	}
	env := decodeQueryEnvelope(t, resultOf(t, nr))
	if len(env.Data.Transactions) != 3 {
		t.Fatalf("aggregated %d transactions, want 3 — a later page was dropped", len(env.Data.Transactions))
	}
	if len(env.Data.Blocks) != 2 {
		t.Fatalf("aggregated %d blocks, want 2 — the block slice is not aggregated", len(env.Data.Blocks))
	}
	if got := env.CursorBlock["number"]; got != "0x2" {
		t.Fatalf("cursorBlock.number = %v, want 0x2 (the last page's); a resumed caller re-reads the range", got)
	}
	if got := env.FromBlock["number"]; got != "0x1" {
		t.Fatalf("fromBlock.number = %v, want 0x1 (the first page's)", got)
	}
}

// The chainId assertion must reach this stream too. Without it a cross-wired
// BDS endpoint answers with another chain's transactions and nothing notices.
func TestQueryTransactions_StampsTheChainIdAssertion(t *testing.T) {
	c := bdsHandlerClient(t, 8453)
	fake := &fakeTxQueryClient{}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_queryTransactions","params":[{"fromBlock":"0x1","toBlock":"0x2"}]}`)

	if _, err := c.handleQueryTransactions(context.Background(), &bdsConn{queryClient: fake}, req, jrReq); err != nil {
		t.Fatalf("handleQueryTransactions: %v", err)
	}
	if fake.got == nil || fake.got.ChainId == nil || *fake.got.ChainId != 8453 {
		t.Fatalf("chainId assertion = %v, want 8453", fake.got.GetChainId())
	}
}

// A stream that dies halfway must not be served as a complete answer.
func TestQueryTransactions_MidStreamFailureIsNotServedAsAPartialResult(t *testing.T) {
	c := bdsHandlerClient(t, 1)
	fake := &fakeTxQueryClient{
		pages:     []*evm.QueryTransactionsResponse{{Transactions: []*evm.Transaction{{Nonce: 1}}}},
		streamErr: status.Error(codes.Unavailable, "backend went away"),
	}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_queryTransactions","params":[{"fromBlock":"0x1","toBlock":"0x2"}]}`)

	nr, err := c.handleQueryTransactions(context.Background(), &bdsConn{queryClient: fake}, req, jrReq)
	if err == nil {
		t.Fatalf("a truncated stream was served as a result: %s", resultOf(t, nr))
	}
	if !strings.Contains(err.Error(), "backend went away") {
		t.Fatalf("error %v lost the stream failure reason", err)
	}
}

// Malformed params must be refused before any RPC, or the server scans a
// default range nobody asked for.
func TestQueryTransactions_RefusesMalformedParams(t *testing.T) {
	c := bdsHandlerClient(t, 1)
	fake := &fakeTxQueryClient{}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_queryTransactions","params":["not-an-object"]}`)

	if _, err := c.handleQueryTransactions(context.Background(), &bdsConn{queryClient: fake}, req, jrReq); err == nil {
		t.Fatal("a bare string was accepted as an eth_queryTransactions filter")
	}
	if fake.got != nil {
		t.Fatal("malformed params still opened a stream")
	}
}

// --- eth_queryTraces ---

// Traces pages carry three repeated slices. All three must aggregate: a trace
// whose transaction or block was dropped cannot be attributed to anything.
func TestQueryTraces_AggregatesEveryPageOfAllThreeSlices(t *testing.T) {
	c := bdsHandlerClient(t, 1)
	fake := &fakeTraceQueryClient{pages: []*evm.QueryTracesResponse{
		{
			Traces:       []*evm.Trace{{}, {}},
			Transactions: []*evm.Transaction{{Nonce: 1}},
			Blocks:       []*evm.BlockHeader{{Number: 1}},
			FromBlock:    &evm.CursorBlock{Number: 1},
			CursorBlock:  &evm.CursorBlock{Number: 1},
		},
		{
			Traces:       []*evm.Trace{{}},
			Transactions: []*evm.Transaction{{Nonce: 2}},
			Blocks:       []*evm.BlockHeader{{Number: 2}},
			CursorBlock:  &evm.CursorBlock{Number: 2},
		},
	}}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_queryTraces","params":[{"fromBlock":"0x1","toBlock":"0x2"}]}`)

	nr, err := c.handleQueryTraces(context.Background(), &bdsConn{queryClient: fake}, req, jrReq)
	if err != nil {
		t.Fatalf("handleQueryTraces: %v", err)
	}
	env := decodeQueryEnvelope(t, resultOf(t, nr))
	if len(env.Data.Traces) != 3 {
		t.Fatalf("aggregated %d traces, want 3", len(env.Data.Traces))
	}
	if len(env.Data.Transactions) != 2 {
		t.Fatalf("aggregated %d transactions, want 2 — the transaction slice is not aggregated", len(env.Data.Transactions))
	}
	if len(env.Data.Blocks) != 2 {
		t.Fatalf("aggregated %d blocks, want 2 — the block slice is not aggregated", len(env.Data.Blocks))
	}
	if got := env.CursorBlock["number"]; got != "0x2" {
		t.Fatalf("cursorBlock.number = %v, want 0x2 (the last page's)", got)
	}
}

func TestQueryTraces_StampsTheChainIdAssertion(t *testing.T) {
	c := bdsHandlerClient(t, 42161)
	fake := &fakeTraceQueryClient{}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_queryTraces","params":[{"fromBlock":"0x1","toBlock":"0x2"}]}`)

	if _, err := c.handleQueryTraces(context.Background(), &bdsConn{queryClient: fake}, req, jrReq); err != nil {
		t.Fatalf("handleQueryTraces: %v", err)
	}
	if fake.got == nil || fake.got.ChainId == nil || *fake.got.ChainId != 42161 {
		t.Fatalf("chainId assertion = %v, want 42161", fake.got.GetChainId())
	}
}

// A stream that cannot even be opened must surface the server's reason, not a
// bare "no traces".
func TestQueryTraces_StreamOpenFailureSurfaces(t *testing.T) {
	c := bdsHandlerClient(t, 1)
	fake := &fakeTraceQueryClient{openErr: status.Error(codes.PermissionDenied, "no access to traces")}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_queryTraces","params":[{"fromBlock":"0x1","toBlock":"0x2"}]}`)

	_, err := c.handleQueryTraces(context.Background(), &bdsConn{queryClient: fake}, req, jrReq)
	if err == nil {
		t.Fatal("a stream that never opened produced no error")
	}
	if !strings.Contains(err.Error(), "no access to traces") {
		t.Fatalf("error %v lost the server's reason", err)
	}
}

func TestQueryTraces_RefusesMalformedParams(t *testing.T) {
	c := bdsHandlerClient(t, 1)
	fake := &fakeTraceQueryClient{}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_queryTraces","params":["not-an-object"]}`)

	if _, err := c.handleQueryTraces(context.Background(), &bdsConn{queryClient: fake}, req, jrReq); err == nil {
		t.Fatal("a bare string was accepted as an eth_queryTraces filter")
	}
	if fake.got != nil {
		t.Fatal("malformed params still opened a stream")
	}
}

// --- eth_queryTransfers ---

func TestQueryTransfers_AggregatesEveryPageOfAllThreeSlices(t *testing.T) {
	c := bdsHandlerClient(t, 1)
	fake := &fakeTransferQueryClient{pages: []*evm.QueryTransfersResponse{
		{
			Transfers:    []*evm.NativeTransfer{{Value: "0x1"}, {Value: "0x2"}},
			Transactions: []*evm.Transaction{{Nonce: 1}},
			Blocks:       []*evm.BlockHeader{{Number: 1}},
			FromBlock:    &evm.CursorBlock{Number: 1},
			CursorBlock:  &evm.CursorBlock{Number: 1},
		},
		{
			Transfers:    []*evm.NativeTransfer{{Value: "0x3"}},
			Transactions: []*evm.Transaction{{Nonce: 2}},
			Blocks:       []*evm.BlockHeader{{Number: 2}},
			CursorBlock:  &evm.CursorBlock{Number: 2},
		},
	}}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_queryTransfers","params":[{"fromBlock":"0x1","toBlock":"0x2"}]}`)

	nr, err := c.handleQueryTransfers(context.Background(), &bdsConn{queryClient: fake}, req, jrReq)
	if err != nil {
		t.Fatalf("handleQueryTransfers: %v", err)
	}
	env := decodeQueryEnvelope(t, resultOf(t, nr))
	if len(env.Data.Transfers) != 3 {
		t.Fatalf("aggregated %d transfers, want 3", len(env.Data.Transfers))
	}
	if len(env.Data.Transactions) != 2 {
		t.Fatalf("aggregated %d transactions, want 2 — the transaction slice is not aggregated", len(env.Data.Transactions))
	}
	if len(env.Data.Blocks) != 2 {
		t.Fatalf("aggregated %d blocks, want 2 — the block slice is not aggregated", len(env.Data.Blocks))
	}
	if got := env.CursorBlock["number"]; got != "0x2" {
		t.Fatalf("cursorBlock.number = %v, want 0x2 (the last page's)", got)
	}
}

func TestQueryTransfers_StampsTheChainIdAssertion(t *testing.T) {
	c := bdsHandlerClient(t, 10)
	fake := &fakeTransferQueryClient{}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_queryTransfers","params":[{"fromBlock":"0x1","toBlock":"0x2"}]}`)

	if _, err := c.handleQueryTransfers(context.Background(), &bdsConn{queryClient: fake}, req, jrReq); err != nil {
		t.Fatalf("handleQueryTransfers: %v", err)
	}
	if fake.got == nil || fake.got.ChainId == nil || *fake.got.ChainId != 10 {
		t.Fatalf("chainId assertion = %v, want 10", fake.got.GetChainId())
	}
}

func TestQueryTransfers_MidStreamFailureIsNotServedAsAPartialResult(t *testing.T) {
	c := bdsHandlerClient(t, 1)
	fake := &fakeTransferQueryClient{
		pages:     []*evm.QueryTransfersResponse{{Transfers: []*evm.NativeTransfer{{Value: "0x1"}}}},
		streamErr: status.Error(codes.Unavailable, "backend went away"),
	}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_queryTransfers","params":[{"fromBlock":"0x1","toBlock":"0x2"}]}`)

	nr, err := c.handleQueryTransfers(context.Background(), &bdsConn{queryClient: fake}, req, jrReq)
	if err == nil {
		t.Fatalf("a truncated stream was served as a result: %s", resultOf(t, nr))
	}
	if !strings.Contains(err.Error(), "backend went away") {
		t.Fatalf("error %v lost the stream failure reason", err)
	}
}

func TestQueryTransfers_RefusesMalformedParams(t *testing.T) {
	c := bdsHandlerClient(t, 1)
	fake := &fakeTransferQueryClient{}
	req, jrReq := jsonRpcFor(t, `{"jsonrpc":"2.0","id":1,"method":"eth_queryTransfers","params":["not-an-object"]}`)

	if _, err := c.handleQueryTransfers(context.Background(), &bdsConn{queryClient: fake}, req, jrReq); err == nil {
		t.Fatal("a bare string was accepted as an eth_queryTransfers filter")
	}
	if fake.got != nil {
		t.Fatal("malformed params still opened a stream")
	}
}
