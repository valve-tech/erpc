package evm

import (
	"context"
	"fmt"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover entry 132 in valve/upstream-bug-log.md: parseUint64Value
// reports an unreadable quantity, and a set of call sites throw the report away
// and use the zero. A zero block number is not a missing value — it names the
// genesis block, so the client reads a wrong answer rather than an error.

// --- sortLogs and the paging loop ---

// TestShimQueryLogs_ALogWithNoBlockNumberIsAnErrorNotBlockZero pins both halves
// at once: the sort keys on blockNumber, and the paging loop groups on it. With
// the error discarded, an unreadable log sorts to the head of an ascending page
// and can push a cursor of 0, which sends the client back to genesis.
func TestShimQueryLogs_ALogWithNoBlockNumberIsAnErrorNotBlockZero(t *testing.T) {
	good := makeLogResult(7, 0, 0, "0xaaa", "0x00000000000000000000000000000000000000aa")
	broken := makeLogResult(8, 1, 0, "0xbbb", "0x00000000000000000000000000000000000000bb")
	delete(broken, "blockNumber")

	network := newRouterBackedQueryNetwork(t, func(_ context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		switch jrq.Method {
		case "eth_getLogs":
			return jsonResultResponse(t, req, []interface{}{good, broken}), nil
		case "eth_getBlockByNumber":
			blockRef, _ := jrq.Params[0].(string)
			n, err := common.HexToUint64(blockRef)
			require.NoError(t, err)
			return jsonResultResponse(t, req, makeBlockResult(n, nil)), nil
		default:
			return nil, fmt.Errorf("unexpected method %s", jrq.Method)
		}
	})

	_, err := shimQueryLogs(shimTestCtx(t), network, "parent", "", nil, &QueryRequest{
		Method: "eth_queryLogs", FromBlock: 7, ToBlock: 8, Order: "asc", Limit: 10,
		Fields: &QueryFieldSelection{Logs: []string{"blockNumber", "logIndex"}},
	})
	require.Error(t, err, "a log whose blockNumber eRPC cannot read must not be sorted or paged as block 0")
	assert.Contains(t, err.Error(), "blockNumber")
}

// TestShimQueryLogs_ALogWithNoLogIndexIsAnErrorNotIndexZero covers the second
// sort key. Within one block the order is the log index, so an unreadable index
// silently reorders the page.
func TestShimQueryLogs_ALogWithNoLogIndexIsAnErrorNotIndexZero(t *testing.T) {
	good := makeLogResult(7, 1, 0, "0xaaa", "0x00000000000000000000000000000000000000aa")
	broken := makeLogResult(7, 2, 0, "0xbbb", "0x00000000000000000000000000000000000000bb")
	broken["logIndex"] = "not a quantity"

	network := newRouterBackedQueryNetwork(t, func(_ context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		switch jrq.Method {
		case "eth_getLogs":
			return jsonResultResponse(t, req, []interface{}{good, broken}), nil
		case "eth_getBlockByNumber":
			return jsonResultResponse(t, req, makeBlockResult(7, nil)), nil
		default:
			return nil, fmt.Errorf("unexpected method %s", jrq.Method)
		}
	})

	_, err := shimQueryLogs(shimTestCtx(t), network, "parent", "", nil, &QueryRequest{
		Method: "eth_queryLogs", FromBlock: 7, ToBlock: 7, Order: "asc", Limit: 10,
		Fields: &QueryFieldSelection{Logs: []string{"blockNumber", "logIndex"}},
	})
	require.Error(t, err, "a log whose logIndex eRPC cannot read must not sort as index 0")
	assert.Contains(t, err.Error(), "logIndex")
}

// TestShimQueryLogs_AWellFormedPageStillSorts is the counterweight. The change
// above must reject only what it cannot read; a normal out-of-order page must
// still come back sorted.
func TestShimQueryLogs_AWellFormedPageStillSorts(t *testing.T) {
	logs := []interface{}{
		makeLogResult(9, 1, 0, "0xccc", "0x00000000000000000000000000000000000000cc"),
		makeLogResult(7, 3, 0, "0xaaa", "0x00000000000000000000000000000000000000aa"),
		makeLogResult(7, 1, 0, "0xbbb", "0x00000000000000000000000000000000000000bb"),
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
		Method: "eth_queryLogs", FromBlock: 7, ToBlock: 9, Order: "asc", Limit: 10,
		Fields: &QueryFieldSelection{Logs: []string{"blockNumber", "logIndex"}},
	})
	require.NoError(t, err)
	require.Len(t, resp.Logs, 3)
	assert.Equal(t, []string{"0x7", "0x7", "0x9"}, []string{
		resp.Logs[0]["blockNumber"].(string),
		resp.Logs[1]["blockNumber"].(string),
		resp.Logs[2]["blockNumber"].(string),
	})
	assert.Equal(t, "0x1", resp.Logs[0]["logIndex"], "within a block the log index orders the page")
	assert.Equal(t, "0x3", resp.Logs[1]["logIndex"])
}

// --- fetchTracesForBlock ---

// TestFetchTracesForBlock_ABlockWithNoNumberIsAnError covers the third site. A
// block whose number eRPC cannot read falls back to trace_block("0x0") and
// stamps every trace with block 0 — traces of the wrong block, served as this
// block's.
func TestFetchTracesForBlock_ABlockWithNoNumberIsAnError(t *testing.T) {
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, _ *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		t.Fatalf("the shim must not trace a block it cannot name; it sent %q", jrq.Method)
		return nil, nil
	})

	block := makeBlockResult(3, nil)
	delete(block, "number")

	_, err := fetchTracesForBlock(shimTestCtx(t), network, "parent", "", block)
	require.Error(t, err, "a block with no readable number must not be traced as block 0")
	assert.Contains(t, err.Error(), "number")
}

// TestFetchTracesForBlock_AnUnreadableTimestampIsAnError covers the guarded
// site next to it. `err == nil` there drops a present-but-broken timestamp to
// nil, which reads downstream as "this chain does not report timestamps".
func TestFetchTracesForBlock_AnUnreadableTimestampIsAnError(t *testing.T) {
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, _ *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		t.Fatalf("the shim must reject the block before tracing it; it sent %q", jrq.Method)
		return nil, nil
	})

	block := makeBlockResult(3, nil)
	block["timestamp"] = "not a quantity"

	_, err := fetchTracesForBlock(shimTestCtx(t), network, "parent", "", block)
	require.Error(t, err, "a timestamp that is present but unreadable must not be dropped to absent")
	assert.Contains(t, err.Error(), "timestamp")
}

// --- protoTraceFromJSON ---

// TestProtoTraceFromJSON_APresentButUnreadableQuantityIsAnError pins the rule
// the trace decoder needs: an ABSENT field keeps the proto's zero, because the
// proto has no way to say "absent" and Parity really does omit gas and
// transactionIndex on reward traces. A field that is PRESENT and unreadable is
// a different event, and eRPC must not answer it with a number it invented.
func TestProtoTraceFromJSON_APresentButUnreadableQuantityIsAnError(t *testing.T) {
	for _, field := range []string{"gas", "gasUsed", "subtraces", "transactionIndex", "blockNumber", "blockTimestamp"} {
		t.Run(field, func(t *testing.T) {
			trace := map[string]interface{}{
				"traceType": "call",
				"callType":  "call",
				"from":      "0x0000000000000000000000000000000000000001",
				"value":     "0x0",
			}
			trace[field] = "not a quantity"

			_, err := protoTraceFromJSON(trace)
			require.Error(t, err, "%s is present and unreadable, so it must not decode as 0", field)
			assert.Contains(t, err.Error(), field)
		})
	}
}

// TestProtoTraceFromJSON_AnUnreadableTraceAddressEntryIsAnError covers the
// element loop. Every entry of traceAddress is present by construction, so an
// unreadable one is always garbage, and a silent 0 moves the trace to a
// different position in the call tree.
func TestProtoTraceFromJSON_AnUnreadableTraceAddressEntryIsAnError(t *testing.T) {
	_, err := protoTraceFromJSON(map[string]interface{}{
		"traceType":    "call",
		"callType":     "call",
		"from":         "0x0000000000000000000000000000000000000001",
		"traceAddress": []interface{}{"0x0", "not a quantity"},
	})
	require.Error(t, err, "an unreadable traceAddress entry must not decode as position 0")
	assert.Contains(t, err.Error(), "traceAddress")
}

// TestProtoTraceFromJSON_AnAbsentQuantityKeepsTheProtoZero is the counterweight
// to the two tests above, and it is the reason the rule is "present but
// unreadable", not "unreadable". Parity omits gas and transactionIndex on a
// reward trace; rejecting those would make eRPC refuse valid chain data.
func TestProtoTraceFromJSON_AnAbsentQuantityKeepsTheProtoZero(t *testing.T) {
	got, err := protoTraceFromJSON(map[string]interface{}{
		"traceType": "reward",
		"from":      "0x0000000000000000000000000000000000000001",
		"value":     "0x1",
	})
	require.NoError(t, err, "a reward trace omits gas and transactionIndex, and is still valid")
	assert.Equal(t, uint64(0), got.Gas)
	assert.Equal(t, uint32(0), got.TransactionIndex)
	assert.Nil(t, got.BlockTimestamp, "an absent timestamp stays absent, not zero")
}
