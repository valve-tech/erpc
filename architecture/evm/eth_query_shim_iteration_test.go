package evm

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests drive the per-block iteration bodies of the query shims — the
// loops that decide which block advances the cursor, which block ends the page,
// and which failure stops the whole query. Every assertion names the cursor or
// the requests that went on the wire, because a shim that lost a block returns
// the same shape as one that did not.

// queryShimFn is the signature every shim shares.
type queryShimFn func(context.Context, common.Network, interface{}, string, *common.EvmQueryShimConfig, *QueryRequest) (*QueryResponse, error)

// blockRangeShims are the shims that walk a block range through fetchBlockRange.
var blockRangeShims = []struct {
	name string
	shim queryShimFn
}{
	{"Blocks", shimQueryBlocks},
	{"Transactions", shimQueryTransactions},
	{"Traces", shimQueryTraces},
	{"Transfers", shimQueryTransfers},
}

func rangeQuery(method string, from, to uint64, limit uint64) *QueryRequest {
	return &QueryRequest{
		Method:    method,
		FromBlock: from,
		ToBlock:   to,
		Order:     "asc",
		Limit:     limit,
		Fields: &QueryFieldSelection{
			Blocks:       []string{"number", "hash"},
			Transactions: []string{"hash"},
			Traces:       true,
			Transfers:    true,
		},
	}
}

func TestShimQueries_ABlockFetchFailureFailsTheWholeQuery(t *testing.T) {
	for _, tc := range blockRangeShims {
		t.Run(tc.name, func(t *testing.T) {
			network := newRouterBackedQueryNetwork(t, func(_ context.Context, _ *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
				require.Equal(t, "eth_getBlockByNumber", jrq.Method)
				return nil, errors.New("dial tcp: connection refused")
			})

			resp, err := tc.shim(shimTestCtx(t), network, "parent", "", nil, rangeQuery("eth_query"+tc.name, 1, 3, 10))

			// Discriminating: a missing block is NOT an empty answer. Returning a
			// page here would tell the caller the range holds no data.
			require.Error(t, err)
			assert.Contains(t, err.Error(), "connection refused")
			assert.Nil(t, resp)
		})
	}
}

func TestShimQueries_ABlockBodyThatIsNotAnObjectFailsTheWholeQuery(t *testing.T) {
	for _, tc := range blockRangeShims {
		t.Run(tc.name, func(t *testing.T) {
			network := newRouterBackedQueryNetwork(t, func(_ context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
				require.Equal(t, "eth_getBlockByNumber", jrq.Method)
				// A node (or a proxy) that answers with a bare string where a
				// block object belongs.
				return jsonResultResponse(t, req, "block 1"), nil
			})

			_, err := tc.shim(shimTestCtx(t), network, "parent", "", nil, rangeQuery("eth_query"+tc.name, 1, 1, 10))

			require.Error(t, err, "a block body the decoder cannot read must not be skipped silently")
		})
	}
}

// --- shimQueryTransactions ---

func TestShimQueryTransactions_ABlockWithNoMatchingTransactionsStillAdvancesTheCursor(t *testing.T) {
	// Block 1 carries two matching transactions, block 2 carries none, block 3
	// carries two more. With a limit of 3, block 3 overflows the page — so the
	// cursor must name block 2, the last block the shim actually scanned.
	blocks := map[uint64][]interface{}{
		1: {
			makeTransactionResult("0xa1", 1, 0, "0x01", "0x02", "0x"),
			makeTransactionResult("0xa2", 1, 1, "0x01", "0x02", "0x"),
		},
		2: {},
		3: {
			makeTransactionResult("0xc1", 3, 0, "0x01", "0x02", "0x"),
			makeTransactionResult("0xc2", 3, 1, "0x01", "0x02", "0x"),
		},
	}
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		require.Equal(t, "eth_getBlockByNumber", jrq.Method)
		blockRef, _ := jrq.Params[0].(string)
		n, err := common.HexToUint64(blockRef)
		require.NoError(t, err)
		return jsonResultResponse(t, req, makeBlockResult(n, blocks[n])), nil
	})

	resp, err := shimQueryTransactions(shimTestCtx(t), network, "parent", "", nil, rangeQuery("eth_queryTransactions", 1, 3, 3))

	require.NoError(t, err)
	require.Len(t, resp.Transactions, 2, "block 3 would overflow the page, so it must wait for the next one")
	// Discriminating: the cursor must be block 2, not block 1. A shim that
	// skipped an empty block WITHOUT recording it would resume from block 1 and
	// re-scan a block it already knows is empty on every page.
	require.NotNil(t, resp.CursorBlock)
	assert.Equal(t, uint64(2), resp.CursorBlock.Number)
}

func TestShimQueryTransactions_ATransactionEntryThatIsNotAnObjectIsSkipped(t *testing.T) {
	// A node answering a fullTx request with bare hashes for some entries —
	// the shape a mixed cache or a lying proxy produces.
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		require.Equal(t, "eth_getBlockByNumber", jrq.Method)
		return jsonResultResponse(t, req, makeBlockResult(1, []interface{}{
			"0xdeadbeef",
			makeTransactionResult("0xa1", 1, 1, "0x01", "0x02", "0x"),
		})), nil
	})

	resp, err := shimQueryTransactions(shimTestCtx(t), network, "parent", "", nil, rangeQuery("eth_queryTransactions", 1, 1, 10))

	require.NoError(t, err, "one unusable entry must not fail the block")
	// Discriminating: exactly the object entry survives. Counting two would mean
	// the bare hash was projected into a transaction record with no fields.
	require.Len(t, resp.Transactions, 1)
	assert.Equal(t, "0xa1", resp.Transactions[0]["hash"])
}

// --- shimQueryLogs ---

func TestShimQueryLogs_ALogsFailureFailsTheQuery(t *testing.T) {
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, _ *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		require.Equal(t, "eth_getLogs", jrq.Method)
		return nil, errors.New("dial tcp: connection refused")
	})

	resp, err := shimQueryLogs(shimTestCtx(t), network, "parent", "", nil, rangeQuery("eth_queryLogs", 1, 3, 10))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
	assert.Nil(t, resp, "a failed log fetch must not be answered with an empty page")
}

func TestShimQueryLogs_ABlockHydrationFailureFailsTheQuery(t *testing.T) {
	log1 := makeLogResult(1, 0, 0, "0xaaa", "0x00000000000000000000000000000000000000aa")
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		switch jrq.Method {
		case "eth_getLogs":
			return jsonResultResponse(t, req, []interface{}{log1}), nil
		case "eth_getBlockByNumber":
			return nil, errors.New("dial tcp: connection refused")
		default:
			return nil, fmt.Errorf("unexpected method %s", jrq.Method)
		}
	})

	_, err := shimQueryLogs(shimTestCtx(t), network, "parent", "", nil, rangeQuery("eth_queryLogs", 1, 1, 10))

	// Discriminating: the LOGS came back fine. Only the block the cursor is
	// built from failed, and a page with no usable cursor must not be served.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
}

func TestShimQueryLogs_ALogWithNoTransactionHashIsNotHydrated(t *testing.T) {
	withHash := makeLogResult(1, 0, 0, "0xaaa", "0x00000000000000000000000000000000000000aa")
	noHash := makeLogResult(1, 1, 0, "", "0x00000000000000000000000000000000000000aa")
	var txLookups []string
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		switch jrq.Method {
		case "eth_getLogs":
			return jsonResultResponse(t, req, []interface{}{withHash, noHash}), nil
		case "eth_getBlockByNumber":
			return jsonResultResponse(t, req, makeBlockResult(1, nil)), nil
		case "eth_getTransactionByHash":
			hash, _ := jrq.Params[0].(string)
			txLookups = append(txLookups, hash)
			return jsonResultResponse(t, req, makeTransactionResult(hash, 1, 0, "0x01", "0x02", "0x")), nil
		default:
			return nil, fmt.Errorf("unexpected method %s", jrq.Method)
		}
	})

	resp, err := shimQueryLogs(shimTestCtx(t), network, "parent", "", nil, rangeQuery("eth_queryLogs", 1, 1, 10))

	require.NoError(t, err)
	require.Len(t, resp.Logs, 2, "a log with no transaction hash is still a log")
	// Discriminating: exactly ONE lookup went on the wire. Without the guard the
	// shim would ask the node for the empty hash, and every node answers that
	// with an error or a null the caller then has to explain.
	assert.Equal(t, []string{"0xaaa"}, txLookups)
}

// --- shimQueryTraces ---

// traceOnlyNetwork answers eth_getBlockByNumber from blocks and trace_block
// from traces, both keyed by block number.
func traceOnlyNetwork(
	t *testing.T,
	blocks map[uint64][]interface{},
	traces map[uint64][]interface{},
	onTx func(hash string) (*common.NormalizedResponse, error),
) *queryTestNetwork {
	t.Helper()
	return newRouterBackedQueryNetwork(t, func(_ context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		switch jrq.Method {
		case "eth_getBlockByNumber":
			blockRef, _ := jrq.Params[0].(string)
			n, err := common.HexToUint64(blockRef)
			require.NoError(t, err)
			return jsonResultResponse(t, req, makeBlockResult(n, blocks[n])), nil
		case "trace_block":
			blockRef, _ := jrq.Params[0].(string)
			n, err := common.HexToUint64(blockRef)
			require.NoError(t, err)
			return jsonResultResponse(t, req, traces[n]), nil
		case "eth_getTransactionByHash":
			hash, _ := jrq.Params[0].(string)
			if onTx != nil {
				return onTx(hash)
			}
			return jsonResultResponse(t, req, makeTransactionResult(hash, 1, 0, "0x01", "0x02", "0x")), nil
		default:
			return nil, fmt.Errorf("unexpected method %s", jrq.Method)
		}
	})
}

// parityTrace renders one parity-style trace_block entry.
func parityTrace(blockNumber uint64, txHash string, txIndex uint64) map[string]interface{} {
	return map[string]interface{}{
		"type": "call",
		"action": map[string]interface{}{
			"callType": "call",
			"from":     "0x0000000000000000000000000000000000000001",
			"to":       "0x0000000000000000000000000000000000000002",
			"value":    "0x1",
			"gas":      "0x5208",
			"input":    "0x",
		},
		"result":              map[string]interface{}{"gasUsed": "0x5208", "output": "0x"},
		"subtraces":           0,
		"traceAddress":        []interface{}{},
		"transactionHash":     txHash,
		"transactionPosition": txIndex,
		"blockNumber":         blockNumber,
		"blockHash":           fmt.Sprintf("0x%064x", blockNumber),
	}
}

func TestShimQueryTraces_ABlockWithNoTracesStillAdvancesTheCursor(t *testing.T) {
	// Same page shape as the transaction test: block 2 holds no traces, and the
	// cursor must still move onto it before block 3 overflows the page.
	traces := map[uint64][]interface{}{
		1: {parityTrace(1, "0xa1", 0), parityTrace(1, "0xa2", 1)},
		2: {},
		3: {parityTrace(3, "0xc1", 0), parityTrace(3, "0xc2", 1)},
	}
	network := traceOnlyNetwork(t, map[uint64][]interface{}{}, traces, nil)

	req := rangeQuery("eth_queryTraces", 1, 3, 3)
	req.Fields.Transactions = nil
	resp, err := shimQueryTraces(shimTestCtx(t), network, "parent", "", nil, req)

	require.NoError(t, err)
	require.Len(t, resp.Traces, 2, "block 3 would overflow the page")
	// Discriminating: block 2 was scanned and found empty, so the next page must
	// resume from block 2. Reporting block 1 would re-scan an empty block.
	require.NotNil(t, resp.CursorBlock)
	assert.Equal(t, uint64(2), resp.CursorBlock.Number)
}

func TestShimQueryTraces_ATraceWithNoTransactionHashIsNotHydrated(t *testing.T) {
	// A block-reward trace carries no transaction. So does a frame whose hash
	// the node renders as the empty "0x".
	traces := map[uint64][]interface{}{
		1: {parityTrace(1, "0xa1", 0), parityTrace(1, "", 1), parityTrace(1, "0x", 2)},
	}
	var txLookups []string
	network := traceOnlyNetwork(t, map[uint64][]interface{}{}, traces,
		func(hash string) (*common.NormalizedResponse, error) {
			txLookups = append(txLookups, hash)
			return nil, common.NewErrEndpointMissingData(errors.New("no such transaction"), nil)
		})

	resp, err := shimQueryTraces(shimTestCtx(t), network, "parent", "", nil, rangeQuery("eth_queryTraces", 1, 1, 10))

	require.NoError(t, err)
	require.Len(t, resp.Traces, 3, "a trace with no transaction is still a trace")
	// Discriminating: exactly one lookup. Both empty forms must be skipped, and
	// "0x" is the one a plain `txHash == ""` guard would let through.
	assert.Equal(t, []string{"0xa1"}, txLookups)
}

func TestShimQueryTraces_ATransactionHydrationFailureFailsTheQuery(t *testing.T) {
	traces := map[uint64][]interface{}{1: {parityTrace(1, "0xa1", 0)}}
	network := traceOnlyNetwork(t, map[uint64][]interface{}{}, traces,
		func(string) (*common.NormalizedResponse, error) {
			return nil, errors.New("dial tcp: connection refused")
		})

	_, err := shimQueryTraces(shimTestCtx(t), network, "parent", "", nil, rangeQuery("eth_queryTraces", 1, 1, 10))

	// Discriminating: the traces themselves arrived. A half-hydrated page must
	// not pass as complete.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
}

// --- fetchBlockByNumber / fetchTransactionByHash ---

func TestFetchBlockByNumber_ANullBlockReadsAsAbsent(t *testing.T) {
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		require.Equal(t, "eth_getBlockByNumber", jrq.Method)
		return jsonResultResponse(t, req, nil), nil
	})

	block, err := fetchBlockByNumber(shimTestCtx(t), network, "parent", "", 7, false)

	// A block a node has not produced yet is absent, not a failure. (The
	// explicit "null" short-circuit above blockMapFromRaw is redundant — the
	// decoder turns "null" into a nil map with no error either way — so this
	// pins the CONTRACT, not that one branch.)
	require.NoError(t, err)
	assert.Nil(t, block)
}

func TestFetchTransactionByHash_AnUnreadableBodyIsAnError(t *testing.T) {
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		require.Equal(t, "eth_getTransactionByHash", jrq.Method)
		return jsonResultResponse(t, req, []interface{}{"an array where an object belongs"}), nil
	})

	tx, err := fetchTransactionByHash(shimTestCtx(t), network, "parent", "", "0xaaa")

	require.Error(t, err, "a body that is not a transaction object must not read as an absent transaction")
	assert.Nil(t, tx)
}

// --- fetchTracesForBlock ---

func TestFetchTracesForBlock_ABlockNumberThatIsNotHexStillNamesTheBlockOnTheWire(t *testing.T) {
	// Some decoders hand the shim a block whose "number" is a JSON number
	// rather than a hex string. The trace request must still carry the block.
	block := map[string]interface{}{
		"number":    float64(9),
		"hash":      fmt.Sprintf("0x%064x", 9),
		"timestamp": "0x64",
	}
	var traceParams []string
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		require.Equal(t, "trace_block", jrq.Method)
		p, _ := jrq.Params[0].(string)
		traceParams = append(traceParams, p)
		return jsonResultResponse(t, req, []interface{}{}), nil
	})

	_, err := fetchTracesForBlock(shimTestCtx(t), network, "parent", "", block)

	require.NoError(t, err)
	// Discriminating: an empty block reference would make the node answer for
	// the WRONG block (most treat it as an error, some as "latest"), and the
	// traces would be attributed to block 9 regardless.
	assert.Equal(t, []string{"0x9"}, traceParams)
}

func TestFetchTracesForBlock_ADebugFallbackMayAnswerWithOneFramePerTransaction(t *testing.T) {
	// The geth batch shape: one entry per transaction, each wrapping its call
	// frame under "result". The shim must stamp each frame with the transaction
	// at the SAME index in the block.
	batch := []interface{}{
		map[string]interface{}{
			"txHash": "0xaaaa",
			"result": map[string]interface{}{
				"type": "CALL", "from": "0x0000000000000000000000000000000000000001",
				"to": "0x0000000000000000000000000000000000000002",
				"value": "0x1", "gas": "0x5208", "gasUsed": "0x5208", "input": "0x",
			},
		},
		map[string]interface{}{
			"txHash": "0xbbbb",
			"result": map[string]interface{}{
				"type": "CALL", "from": "0x0000000000000000000000000000000000000003",
				"to": "0x0000000000000000000000000000000000000004",
				"value": "0x2", "gas": "0x5208", "gasUsed": "0x5208", "input": "0x",
			},
		},
	}
	block := makeBlockResult(3, []interface{}{
		makeTransactionResult("0xaaaa", 3, 0, "0x01", "0x02", "0x"),
		makeTransactionResult("0xbbbb", 3, 1, "0x03", "0x04", "0x"),
	})
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		if jrq.Method == "trace_block" {
			return nil, common.NewErrEndpointUnsupported(errors.New("method not found"))
		}
		return jsonResultResponse(t, req, batch), nil
	})

	traces, err := fetchTracesForBlock(shimTestCtx(t), network, "parent", "", block)

	require.NoError(t, err)
	require.Len(t, traces, 2, "a batch of two frames must produce two traces, not one and not zero")
	// Discriminating: the transaction context comes from the BLOCK, by index —
	// the batch entries' own "txHash" key is not what the shim reads. A shim
	// that stamped every frame from index 0 would report both traces against
	// transaction 0, and the caller would join them to the wrong transaction.
	assert.Equal(t, "0xaaaa", traces[0]["transactionHash"])
	assert.Equal(t, "0xbbbb", traces[1]["transactionHash"])
	assert.Equal(t, "0x0", traces[0]["transactionIndex"])
	assert.Equal(t, "0x1", traces[1]["transactionIndex"])
	assert.Equal(t, "0x3", traces[0]["blockNumber"])
}

func TestFetchTracesForBlock_AParityFrameWithAnUnreadableAddressIsAnError(t *testing.T) {
	bad := parityTrace(3, "0xaaaa", 0)
	bad["action"].(map[string]interface{})["from"] = "0xnothexatall"
	network := newRouterBackedQueryNetwork(t, func(_ context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		require.Equal(t, "trace_block", jrq.Method)
		return jsonResultResponse(t, req, []interface{}{bad}), nil
	})

	traces, err := fetchTracesForBlock(shimTestCtx(t), network, "parent", "", aBlock(3))

	// Discriminating: the array PARSED fine — only one frame inside it is
	// undecodable. Dropping that frame would report a shorter trace list as a
	// complete one, and a caller reconciling against the block would see a
	// transaction with no traces at all.
	require.Error(t, err)
	assert.Nil(t, traces)
}

func TestFetchTracesForBlock_ADebugBatchFrameWithAnUnreadableAddressIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name string
		body interface{}
	}{
		{"Batch", []interface{}{
			map[string]interface{}{"result": map[string]interface{}{
				"type": "CALL", "from": "0xnothexatall", "gas": "0x1", "gasUsed": "0x1", "input": "0x",
			}},
		}},
		{"SingleFrame", map[string]interface{}{
			"type": "CALL", "from": "0xnothexatall", "gas": "0x1", "gasUsed": "0x1", "input": "0x",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			network := newRouterBackedQueryNetwork(t, func(_ context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
				if jrq.Method == "trace_block" {
					return nil, common.NewErrEndpointUnsupported(errors.New("method not found"))
				}
				return jsonResultResponse(t, req, tc.body), nil
			})

			traces, err := fetchTracesForBlock(shimTestCtx(t), network, "parent", "", aBlock(3))

			require.Error(t, err, "an undecodable call frame must not be dropped from the block's traces")
			assert.Nil(t, traces)
		})
	}
}

// --- protoTraceFromJSON ---

func TestProtoTraceFromJSON_AFieldOfTheWrongJsonTypeIsAnError(t *testing.T) {
	// Every numeric field arrives as a hex STRING on the wire. A map carrying a
	// bare JSON number instead cannot be decoded, and must not silently become
	// a zero.
	_, err := protoTraceFromJSON(map[string]interface{}{
		"traceType": "call",
		"callType":  "call",
		"from":      "0x0000000000000000000000000000000000000001",
		"subtraces": 3,
	})

	require.Error(t, err, "a numeric field where a hex string belongs must be reported")
}
