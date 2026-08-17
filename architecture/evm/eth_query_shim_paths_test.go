package evm

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	bdsevm "github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover the eth_query shim paths the existing suite leaves dark:
// the empty-range short circuits, the eth_getLogs filter the shim BUILDS from a
// QueryFilter, the sub-request fetchers' missing-data and null answers, the
// trace fallback's remaining shapes, and the trace/transaction context helpers.
//
// They reuse newRouterBackedQueryNetwork (eth_query_test.go), which answers
// every sub-request from one router closure and so lets a test assert on the
// REQUEST the shim sent — the discriminating property whenever two code paths
// return the same response.

// shimTestCtx bounds every shim call. A shim that fans out over a block range
// must not be able to hang the suite.
func shimTestCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// refusingQueryNetwork fails the test the moment any sub-request reaches it.
func refusingQueryNetwork(t *testing.T) *queryTestNetwork {
	t.Helper()
	return newRouterBackedQueryNetwork(t, func(_ context.Context, _ *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		t.Fatalf("the shim forwarded %q for a range that holds no blocks", jrq.Method)
		return nil, nil
	})
}

// --- empty ranges ---

func TestShimQueries_AnEmptyRangeIsAnsweredWithoutTouchingTheNetwork(t *testing.T) {
	type shim func(context.Context, common.Network, interface{}, string, *common.EvmQueryShimConfig, *QueryRequest) (*QueryResponse, error)

	for _, tc := range []struct {
		name  string
		shim  shim
		order string
		from  uint64
		to    uint64
	}{
		// Ascending: fromBlock past toBlock. Descending: the mirror image.
		{"Blocks", shimQueryBlocks, "asc", 9, 4},
		{"Transactions", shimQueryTransactions, "asc", 9, 4},
		{"Logs", shimQueryLogs, "asc", 9, 4},
		{"Traces", shimQueryTraces, "asc", 9, 4},
		{"Transfers", shimQueryTransfers, "asc", 9, 4},
		{"BlocksDescending", shimQueryBlocks, "desc", 4, 9},
		{"LogsDescending", shimQueryLogs, "desc", 4, 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := tc.shim(shimTestCtx(t), refusingQueryNetwork(t), "parent", "", nil, &QueryRequest{
				Method:    "eth_query" + tc.name,
				FromBlock: tc.from,
				ToBlock:   tc.to,
				Order:     tc.order,
				Limit:     10,
				Fields:    &QueryFieldSelection{Blocks: []string{"number"}},
			})
			require.NoError(t, err)
			require.NotNil(t, resp)
			// Discriminating: the bounds must be echoed back exactly, and the
			// cursor must stay nil. A shim that simply returned an empty struct
			// would lose the bounds the caller has to page against.
			require.NotNil(t, resp.FromBlock)
			require.NotNil(t, resp.ToBlock)
			assert.Equal(t, tc.from, resp.FromBlock.Number)
			assert.Equal(t, tc.to, resp.ToBlock.Number)
			assert.Nil(t, resp.CursorBlock)
			assert.Empty(t, resp.Blocks)
			assert.Empty(t, resp.Transactions)
			assert.Empty(t, resp.Logs)
			assert.Empty(t, resp.Traces)
			assert.Empty(t, resp.Transfers)
		})
	}
}

// --- shimQueryLogs: the eth_getLogs filter the shim builds ---

// capturedLogsFilter runs shimQueryLogs against a network that answers with no
// logs, and returns the eth_getLogs filter the shim put on the wire.
func capturedLogsFilter(t *testing.T, filter *QueryFilter) map[string]interface{} {
	t.Helper()
	var captured map[string]interface{}
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		require.Equal(t, "eth_getLogs", jrq.Method)
		f, ok := jrq.Params[0].(map[string]interface{})
		require.True(t, ok)
		captured = f
		return jsonResultResponse(t, req, []interface{}{}), nil
	})

	_, err := shimQueryLogs(shimTestCtx(t), network, "parent", "", nil, &QueryRequest{
		Method:    "eth_queryLogs",
		FromBlock: 1,
		ToBlock:   3,
		Order:     "asc",
		Limit:     10,
		Filter:    filter,
		Fields:    &QueryFieldSelection{Logs: []string{"logIndex"}},
	})
	require.NoError(t, err)
	require.NotNil(t, captured, "the shim never forwarded an eth_getLogs request")
	return captured
}

func TestShimQueryLogs_OneAddressGoesOnTheWireAsAScalar(t *testing.T) {
	got := capturedLogsFilter(t, &QueryFilter{
		LogAddresses: [][]byte{{0xaa, 0xbb}},
	})
	// Discriminating: a single address must NOT be wrapped in an array. Nodes
	// accept both, but the scalar form is what the shim commits to, and only
	// the scalar tells the one-address branch apart from the many-address one.
	assert.Equal(t, "0xaabb", got["address"])
}

func TestShimQueryLogs_ManyAddressesGoOnTheWireAsAList(t *testing.T) {
	got := capturedLogsFilter(t, &QueryFilter{
		LogAddresses: [][]byte{{0xaa}, {0xbb}, {0xcc}},
	})
	assert.Equal(t, []string{"0xaa", "0xbb", "0xcc"}, got["address"])
}

func TestShimQueryLogs_NoAddressesLeaveTheFilterKeyOut(t *testing.T) {
	got := capturedLogsFilter(t, &QueryFilter{})
	// Discriminating: an empty list must not become `"address": []`, which every
	// node reads as "match nothing".
	assert.NotContains(t, got, "address")
	assert.NotContains(t, got, "topics")
	assert.Equal(t, "0x1", got["fromBlock"])
	assert.Equal(t, "0x3", got["toBlock"])
}

func TestShimQueryLogs_EachTopicGroupSizeGetsItsOwnWireShape(t *testing.T) {
	got := capturedLogsFilter(t, &QueryFilter{
		Topics: [][]topicValue{
			{},                                   // a wildcard position
			{topicValue{0x11}},                   // exactly one value
			{topicValue{0x22}, topicValue{0x33}}, // an OR list
		},
	})
	topics, ok := got["topics"].([]interface{})
	require.True(t, ok)
	require.Len(t, topics, 3)
	// Discriminating: the three group sizes must produce three DIFFERENT wire
	// shapes — null, a scalar, and a list. Collapsing any two changes what the
	// node matches.
	assert.Nil(t, topics[0])
	assert.Equal(t, "0x11", topics[1])
	assert.Equal(t, []string{"0x22", "0x33"}, topics[2])
}

func TestShimQueryLogs_APageBoundaryStopsAtABlockEdge(t *testing.T) {
	// Two logs in block 1, two in block 2, and a limit of 3. A page must never
	// carry half a block, so the shim serves block 1 only and reports a cursor.
	logs := []interface{}{
		makeLogResult(1, 0, 0, "0xaaa", "0x00000000000000000000000000000000000000aa"),
		makeLogResult(1, 1, 0, "0xaaa", "0x00000000000000000000000000000000000000aa"),
		makeLogResult(2, 0, 0, "0xbbb", "0x00000000000000000000000000000000000000bb"),
		makeLogResult(2, 1, 0, "0xbbb", "0x00000000000000000000000000000000000000bb"),
	}
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		switch jrq.Method {
		case "eth_getLogs":
			return jsonResultResponse(t, req, logs), nil
		case "eth_getBlockByNumber":
			blockRef, _ := jrq.Params[0].(string)
			n, err := common.HexToUint64(blockRef)
			require.NoError(t, err)
			return jsonResultResponse(t, req, makeBlockResult(n, nil)), nil
		default:
			return nil, fmt.Errorf("unexpected method %s", jrq.Method)
		}
	})

	resp, err := shimQueryLogs(shimTestCtx(t), network, "parent", "", nil, &QueryRequest{
		Method:    "eth_queryLogs",
		FromBlock: 1,
		ToBlock:   2,
		Order:     "asc",
		Limit:     3,
		Fields:    &QueryFieldSelection{Logs: []string{"blockNumber", "logIndex"}},
	})
	require.NoError(t, err)
	require.Len(t, resp.Logs, 2, "block 2 would overflow the page, so it must be left for the next one")
	assert.Equal(t, "0x1", resp.Logs[0]["blockNumber"])
	// Discriminating: the cursor must name block 1. A nil cursor would tell the
	// caller the range is exhausted and silently lose block 2.
	require.NotNil(t, resp.CursorBlock)
	assert.Equal(t, uint64(1), resp.CursorBlock.Number)
}

func TestShimQueryLogs_AnUnreadableLogsBodyIsAnError(t *testing.T) {
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, req *common.NormalizedRequest, _ *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		// An object where the shim expects an array of logs.
		return jsonResultResponse(t, req, map[string]interface{}{"unexpected": true}), nil
	})

	_, err := shimQueryLogs(shimTestCtx(t), network, "parent", "", nil, &QueryRequest{
		Method: "eth_queryLogs", FromBlock: 1, ToBlock: 1, Order: "asc", Limit: 10,
		Fields: &QueryFieldSelection{Logs: []string{"logIndex"}},
	})
	require.Error(t, err, "a body that is not a log array must not be served as zero logs")
}

func TestShimQueryLogs_ATransactionHydrationFailureFailsTheQuery(t *testing.T) {
	log1 := makeLogResult(1, 0, 0, "0xaaa", "0x00000000000000000000000000000000000000aa")
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		switch jrq.Method {
		case "eth_getLogs":
			return jsonResultResponse(t, req, []interface{}{log1}), nil
		case "eth_getBlockByNumber":
			return jsonResultResponse(t, req, makeBlockResult(1, nil)), nil
		case "eth_getTransactionByHash":
			return nil, errors.New("upstreams exhausted")
		default:
			return nil, fmt.Errorf("unexpected method %s", jrq.Method)
		}
	})

	_, err := shimQueryLogs(shimTestCtx(t), network, "parent", "", nil, &QueryRequest{
		Method: "eth_queryLogs", FromBlock: 1, ToBlock: 1, Order: "asc", Limit: 10,
		Fields: &QueryFieldSelection{Logs: []string{"logIndex"}, Transactions: []string{"hash"}},
	})
	// Discriminating: the logs themselves were fetched fine. Only the parent
	// hydration failed, and a half-hydrated page must not pass as complete.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "upstreams exhausted")
}

// --- fetchBlockByNumber / fetchTransactionByHash ---

func TestFetchBlockByNumber_MissingDataReadsAsAbsentNotAsAFailure(t *testing.T) {
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, _ *common.NormalizedRequest, _ *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		return nil, common.NewErrEndpointMissingData(errors.New("block outside available range"), nil)
	})

	block, err := fetchBlockByNumber(shimTestCtx(t), network, "parent", "", 7, false)
	require.NoError(t, err, "an upstream without the block is not a query failure")
	assert.Nil(t, block)
}

func TestFetchBlockByNumber_AnyOtherFailureReachesTheCaller(t *testing.T) {
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, _ *common.NormalizedRequest, _ *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		return nil, errors.New("dial tcp: connection refused")
	})

	_, err := fetchBlockByNumber(shimTestCtx(t), network, "parent", "", 7, false)
	require.Error(t, err, "a transport failure must not be laundered into 'block absent'")
	assert.Contains(t, err.Error(), "connection refused")
}

func TestFetchBlockByNumber_ARealBlockIsDecodedAndTheRequestCarriesTheFullTxFlag(t *testing.T) {
	var fullTx interface{}
	var blockRef interface{}
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		blockRef, fullTx = jrq.Params[0], jrq.Params[1]
		return jsonResultResponse(t, req, makeBlockResult(7, nil)), nil
	})

	block, err := fetchBlockByNumber(shimTestCtx(t), network, "parent", "", 7, true)
	require.NoError(t, err)
	require.NotNil(t, block)
	assert.Equal(t, "0x7", block["number"])
	// Discriminating: the shim must ask for the block it was given, in hex, and
	// pass the full-transaction flag through. A hard-coded false would return
	// the same block map with no transactions.
	assert.Equal(t, "0x7", blockRef)
	assert.Equal(t, true, fullTx)
}

func TestFetchTransactionByHash_MissingDataAndNullBothReadAsAbsent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply func(t *testing.T, req *common.NormalizedRequest) (*common.NormalizedResponse, error)
	}{
		{"MissingData", func(_ *testing.T, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return nil, common.NewErrEndpointMissingData(errors.New("tx outside available range"), nil)
		}},
		{"NullResult", func(t *testing.T, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return jsonResultResponse(t, req, nil), nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			network := newRouterBackedQueryNetwork(t, func(_ context.Context, req *common.NormalizedRequest, _ *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
				return tc.reply(t, req)
			})
			tx, err := fetchTransactionByHash(shimTestCtx(t), network, "parent", "", "0xdead")
			require.NoError(t, err)
			assert.Nil(t, tx)
		})
	}
}

func TestFetchTransactionByHash_AnyOtherFailureReachesTheCaller(t *testing.T) {
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, _ *common.NormalizedRequest, _ *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		return nil, errors.New("dial tcp: connection refused")
	})

	_, err := fetchTransactionByHash(shimTestCtx(t), network, "parent", "", "0xdead")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
}

func TestFetchTransactionByHash_ARealTransactionIsDecoded(t *testing.T) {
	tx := makeTransactionResult("0xaaa", 1, 0, "0x01", "0x02", "0x12345678")
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		assert.Equal(t, "0xaaa", jrq.Params[0], "the shim must ask for the hash it was given")
		return jsonResultResponse(t, req, tx), nil
	})

	got, err := fetchTransactionByHash(shimTestCtx(t), network, "parent", "", "0xaaa")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "0xaaa", got["hash"])
}

// --- fetchTracesForBlock ---

func aBlock(number uint64) map[string]interface{} {
	return makeBlockResult(number, nil)
}

func TestFetchTracesForBlock_ANullTraceBlockAnswerIsNoTracesNotAnError(t *testing.T) {
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		require.Equal(t, "trace_block", jrq.Method)
		return jsonResultResponse(t, req, nil), nil
	})

	traces, err := fetchTracesForBlock(shimTestCtx(t), network, "parent", "", aBlock(3))
	require.NoError(t, err, "a node that has no traces for a block is not a failure")
	// Discriminating: a null answer must short-circuit to a nil slice. Letting
	// it fall through to the decoder yields an allocated empty slice, which
	// reads to a caller as "the node answered with zero traces".
	assert.Nil(t, traces)
}

func TestFetchTracesForBlock_AnUnreadableTraceBlockBodyIsAnError(t *testing.T) {
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, req *common.NormalizedRequest, _ *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		return jsonResultResponse(t, req, map[string]interface{}{"not": "an array"}), nil
	})

	_, err := fetchTracesForBlock(shimTestCtx(t), network, "parent", "", aBlock(3))
	require.Error(t, err, "a trace_block body that is not an array must not read as zero traces")
}

func TestFetchTracesForBlock_ATraceBlockFailureThatIsNotUnsupportedStopsThere(t *testing.T) {
	var methods []string
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, _ *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		methods = append(methods, jrq.Method)
		return nil, errors.New("dial tcp: connection refused")
	})

	_, err := fetchTracesForBlock(shimTestCtx(t), network, "parent", "", aBlock(3))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
	// Discriminating: a transport failure says nothing about trace support, so
	// the shim must NOT burn a second request on the debug fallback.
	assert.Equal(t, []string{"trace_block"}, methods)
}

func TestFetchTracesForBlock_ADebugFallbackFailureThatIsNotUnsupportedReachesTheCaller(t *testing.T) {
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, _ *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		if jrq.Method == "trace_block" {
			return nil, common.NewErrEndpointUnsupported(errors.New("the method trace_block does not exist"))
		}
		return nil, errors.New("dial tcp: connection refused")
	})

	_, err := fetchTracesForBlock(shimTestCtx(t), network, "parent", "", aBlock(3))
	require.Error(t, err)
	// Discriminating: only an UNSUPPORTED debug answer means "this node cannot
	// trace". A transport failure must keep its own cause.
	assert.Contains(t, err.Error(), "connection refused")
	assert.NotContains(t, err.Error(), "requires trace_block or debug_traceBlockByNumber")
}

func TestFetchTracesForBlock_ANullDebugFallbackAnswerIsNoTraces(t *testing.T) {
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		if jrq.Method == "trace_block" {
			return nil, common.NewErrEndpointUnsupported(errors.New("method not found"))
		}
		return jsonResultResponse(t, req, nil), nil
	})

	traces, err := fetchTracesForBlock(shimTestCtx(t), network, "parent", "", aBlock(3))
	require.NoError(t, err)
	// Discriminating: same as the trace_block case — nil, not an empty slice.
	assert.Nil(t, traces)
}

func TestFetchTracesForBlock_ADebugFallbackMayAnswerWithASingleFrame(t *testing.T) {
	// Some nodes answer debug_traceBlockByNumber with ONE call frame instead of
	// an array of them. The shim must read both shapes.
	single := map[string]interface{}{
		"type":    "CALL",
		"from":    "0x0000000000000000000000000000000000000001",
		"to":      "0x0000000000000000000000000000000000000002",
		"value":   "0x1",
		"gas":     "0x5208",
		"gasUsed": "0x5208",
		"input":   "0x",
	}
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		if jrq.Method == "trace_block" {
			return nil, common.NewErrEndpointUnsupported(errors.New("method not found"))
		}
		return jsonResultResponse(t, req, single), nil
	})

	traces, err := fetchTracesForBlock(shimTestCtx(t), network, "parent", "", aBlock(3))
	require.NoError(t, err)
	require.Len(t, traces, 1, "a single frame must produce one trace, not zero")
	assert.Equal(t, "0x3", traces[0]["blockNumber"])
}

func TestFetchTracesForBlock_ADebugFallbackBodyThatIsNeitherShapeIsAnError(t *testing.T) {
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		if jrq.Method == "trace_block" {
			return nil, common.NewErrEndpointUnsupported(errors.New("method not found"))
		}
		return jsonResultResponse(t, req, "a bare string"), nil
	})

	_, err := fetchTracesForBlock(shimTestCtx(t), network, "parent", "", aBlock(3))
	require.Error(t, err, "a body that is neither a frame nor a list of frames must not read as zero traces")
}

// --- isUnsupportedTraceMethod ---

// silentlyCodedError carries an eRPC error code that HasErrorCode still finds,
// while its own message says nothing about method support.
type silentlyCodedError struct {
	inner *common.BaseError
}

func (e silentlyCodedError) Error() string   { return "endpoint declined the call" }
func (e silentlyCodedError) Unwrap() []error { return []error{e.inner} }

func TestIsUnsupportedTraceMethod_ReadsBothTheTypedCodeAndThePlainMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"NilError", nil, false},
		{"TypedUnsupported", common.NewErrEndpointUnsupported(errors.New("nope")), true},
		// Discriminating: the code alone must decide. Every eRPC error that
		// CARRIES the unsupported code also renders that code's name, whose
		// lowercase form contains "unsupported" — so the plain-message check
		// would catch it by accident and the code check would look dead. This
		// double keeps the code and drops it from the message.
		{"TypedCodeWithASilentMessage", silentlyCodedError{
			inner: &common.BaseError{
				Code:    common.ErrCodeEndpointUnsupported,
				Message: "unavailable",
			},
		}, true},
		// Vendors that do not set a code still say so in the message, in any case.
		{"MessageSaysMethodNotFound", errors.New("the method trace_block does not exist/is not available: Method not found"), true},
		{"MessageSaysUnsupported", errors.New("UNSUPPORTED method"), true},
		{"AnUnrelatedFailure", errors.New("dial tcp: connection refused"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isUnsupportedTraceMethod(tc.err))
		})
	}
}

// --- protoTraceFromJSON ---

func TestProtoTraceFromJSON_MapsEveryTraceAndCallTypeItNames(t *testing.T) {
	for _, tc := range []struct {
		traceType string
		callType  string
		wantTrace bdsevm.TraceType
		wantCall  bdsevm.TraceCallType
	}{
		{"call", "call", bdsevm.TraceType_TRACE_CALL, bdsevm.TraceCallType_TRACE_CALL_CALL},
		{"create", "staticcall", bdsevm.TraceType_TRACE_CREATE, bdsevm.TraceCallType_TRACE_CALL_STATICCALL},
		{"selfdestruct", "delegatecall", bdsevm.TraceType_TRACE_SELFDESTRUCT, bdsevm.TraceCallType_TRACE_CALL_DELEGATECALL},
		{"reward", "callcode", bdsevm.TraceType_TRACE_REWARD, bdsevm.TraceCallType_TRACE_CALL_CALLCODE},
		// The names arrive in whatever case a vendor chose, and an unknown name
		// must fall back rather than fail the query.
		{"CREATE", "STATICCALL", bdsevm.TraceType_TRACE_CREATE, bdsevm.TraceCallType_TRACE_CALL_STATICCALL},
		{"somethingNew", "somethingNew", bdsevm.TraceType_TRACE_CALL, bdsevm.TraceCallType_TRACE_CALL_CALL},
	} {
		t.Run(tc.traceType+"/"+tc.callType, func(t *testing.T) {
			got, err := protoTraceFromJSON(map[string]interface{}{
				"traceType":        tc.traceType,
				"callType":         tc.callType,
				"from":             "0x0000000000000000000000000000000000000001",
				"to":               "0x0000000000000000000000000000000000000002",
				"value":            "0x1",
				"input":            "0xabcd",
				"output":           "0xef",
				"gas":              "0x5208",
				"gasUsed":          "0x5207",
				"subtraces":        "0x2",
				"traceAddress":     []interface{}{"0x0", "0x1"},
				"transactionHash":  "0x00000000000000000000000000000000000000000000000000000000000000aa",
				"transactionIndex": "0x3",
				"blockNumber":      "0x9",
				"blockHash":        "0x00000000000000000000000000000000000000000000000000000000000000bb",
				"blockTimestamp":   "0x64",
			})
			require.NoError(t, err)
			assert.Equal(t, tc.wantTrace, got.TraceType)
			assert.Equal(t, tc.wantCall, got.CallType)
			// Discriminating: the rest of the frame must survive the mapping.
			// Asserting the enums alone would pass for a decoder that dropped
			// every other field.
			assert.Equal(t, uint64(9), got.BlockNumber)
			assert.Equal(t, uint32(3), got.TransactionIndex)
			assert.Equal(t, uint32(2), got.Subtraces)
			assert.Equal(t, []uint32{0, 1}, got.TraceAddress)
			require.NotNil(t, got.BlockTimestamp)
			assert.Equal(t, uint64(100), *got.BlockTimestamp)
		})
	}
}

func TestProtoTraceFromJSON_AnAbsentToAddressStaysAbsent(t *testing.T) {
	got, err := protoTraceFromJSON(map[string]interface{}{
		"traceType": "create",
		"from":      "0x0000000000000000000000000000000000000001",
		"value":     "0x0",
	})
	require.NoError(t, err)
	// Discriminating: a contract creation has no callee. An empty byte slice
	// would render as the zero address, which is a different account.
	assert.Nil(t, got.To)
	assert.Nil(t, got.BlockTimestamp)
}

// --- injectTransactionContext / propagateTransactionContext ---

func TestInjectTransactionContext_StampsTheFrameAndEveryNestedCall(t *testing.T) {
	frame := map[string]interface{}{
		"type": "CALL",
		"calls": []interface{}{
			map[string]interface{}{"type": "STATICCALL"},
			"not a frame", // a malformed child must be skipped, not panic
			map[string]interface{}{
				"type":  "DELEGATECALL",
				"calls": []interface{}{map[string]interface{}{"type": "CALL"}},
			},
		},
	}
	txs := []interface{}{
		map[string]interface{}{"hash": "0xaaa", "transactionIndex": "0x0"},
		map[string]interface{}{"hash": "0xbbb", "transactionIndex": "0x1"},
	}

	injectTransactionContext(frame, txs, 1)

	assert.Equal(t, "0xbbb", frame["transactionHash"])
	assert.Equal(t, "0x1", frame["transactionIndex"])
	children := frame["calls"].([]interface{})
	// Discriminating: the recursion must reach a GRANDCHILD. Stamping only the
	// top frame would leave nested calls unattributable to a transaction.
	first := children[0].(map[string]interface{})
	assert.Equal(t, "0xbbb", first["transactionHash"])
	third := children[2].(map[string]interface{})
	grandchild := third["calls"].([]interface{})[0].(map[string]interface{})
	assert.Equal(t, "0xbbb", grandchild["transactionHash"])
	assert.Equal(t, "0x1", grandchild["transactionIndex"])
}

func TestInjectTransactionContext_StampsTheResultSubObjectWhenThereIsOne(t *testing.T) {
	// debug_traceBlockByNumber wraps each frame as {"txHash":..,"result":{..}}.
	// The context belongs on the inner frame, which is what the decoder reads.
	frame := map[string]interface{}{
		"result": map[string]interface{}{"type": "CALL"},
	}
	txs := []interface{}{map[string]interface{}{"hash": "0xaaa", "transactionIndex": "0x0"}}

	injectTransactionContext(frame, txs, 0)

	inner := frame["result"].(map[string]interface{})
	assert.Equal(t, "0xaaa", inner["transactionHash"])
	// Discriminating: the wrapper must stay clean. Stamping both would put the
	// context where the decoder does not look and hide a wiring mistake.
	assert.NotContains(t, frame, "transactionHash")
}

func TestInjectTransactionContext_LeavesTheFrameAloneWhenThereIsNoTransaction(t *testing.T) {
	txs := []interface{}{map[string]interface{}{"hash": "0xaaa"}}

	for _, tc := range []struct {
		name  string
		index int
	}{
		{"IndexBelowZero", -1},
		{"IndexPastTheEnd", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := map[string]interface{}{"type": "CALL"}
			injectTransactionContext(frame, txs, tc.index)
			assert.NotContains(t, frame, "transactionHash",
				"an index with no transaction behind it must not stamp anything")
		})
	}

	t.Run("EntryIsNotAFrame", func(t *testing.T) {
		frame := map[string]interface{}{"type": "CALL"}
		injectTransactionContext(frame, []interface{}{"not a transaction"}, 0)
		assert.NotContains(t, frame, "transactionHash")
	})

	t.Run("NilFrame", func(t *testing.T) {
		injectTransactionContext(nil, txs, 0) // must not panic
	})
}

func TestPropagateTransactionContext_KeepsWhatItWasNotGiven(t *testing.T) {
	frame := map[string]interface{}{
		"transactionHash":  "0xkeep",
		"transactionIndex": "0xkeep",
	}

	propagateTransactionContext(frame, "", nil)

	// Discriminating: an empty hash and a nil index mean "unknown", not
	// "overwrite with empty". Clearing them would erase context a caller had
	// already resolved.
	assert.Equal(t, "0xkeep", frame["transactionHash"])
	assert.Equal(t, "0xkeep", frame["transactionIndex"])
}
