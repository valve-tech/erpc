package erpc

import (
	"context"
	"fmt"
	"testing"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// A shim page carries a cursor when the range is not exhausted. The cursor is
// the only thing that tells a client to ask again, so a page that drops it ends
// the client's walk early and the rows behind it are never fetched. Nothing in
// the response says data is missing: a short page with no cursor looks exactly
// like the last page of a complete answer.

// genesisTraceExecutor answers eth_getBlockByNumber and trace_block for every
// block in [0, last], one trace per block. It is the smallest fixture that puts
// block 0 at a page boundary.
func genesisTraceExecutor(t *testing.T, last uint64) *EvmQueryExecutor {
	t.Helper()
	nop := zerolog.Nop()
	return &EvmQueryExecutor{
		logger: &nop,
		forwardSubrequestFn: func(_ context.Context, method string, params []interface{}) ([]byte, error) {
			require.NotEmpty(t, params)
			blockRef, ok := params[0].(string)
			require.True(t, ok)
			var num uint64
			_, err := fmt.Sscanf(blockRef, "0x%x", &num)
			require.NoError(t, err, "the shim must ask for a hex block number")
			require.LessOrEqual(t, num, last)

			switch method {
			case "eth_getBlockByNumber":
				return mustMarshalJSON(t, makeProtoBlockResult(num, []interface{}{})), nil
			case "trace_block":
				return mustMarshalJSON(t, []map[string]interface{}{
					makeParityTraceResult(num, 0, 0),
				}), nil
			default:
				return nil, fmt.Errorf("unexpected method %s", method)
			}
		},
	}
}

// TestShimQueryTraces_CarriesACursorWhenItStopsOnABlockAboveGenesis is the
// positive control: the same shape one block higher does emit a cursor, so the
// test below isolates the block number rather than the pagination rule.
func TestShimQueryTraces_CarriesACursorWhenItStopsOnABlockAboveGenesis(t *testing.T) {
	qe := genesisTraceExecutor(t, 5)

	var page *evm.QueryTracesResponse
	err := qe.shimQueryTraces(context.Background(), &evm.QueryTracesRequest{
		Limit: uint32Ptr(1),
	}, 1, 5, func(msg proto.Message) error {
		page = msg.(*evm.QueryTracesResponse)
		return nil
	})

	require.NoError(t, err)
	require.NotNil(t, page)
	require.Len(t, page.Traces, 1)
	require.NotNil(t, page.CursorBlock, "blocks 2..5 are unread, so the client must be told to ask again")
	assert.Equal(t, uint64(1), page.CursorBlock.Number)
}

// TestShimQueryTraces_DropsTheCursorWhenItStopsOnBlockZero characterises a
// defect, it does not endorse it. shimQueryTraces guards the cursor with
// `lastIncluded > 0` (query_shim.go:246), but lastIncluded is a plain block
// number and 0 is a real block. A query that starts at "earliest" and fills its
// limit inside genesis therefore returns a full page with no cursor. The client
// reads that as "the range is exhausted" and stops, so every block after
// genesis is silently dropped from the answer.
//
// shimQueryTransactions does not have this problem: it guards on a
// *BlockHeader being non-nil, which separates "block 0" from "no block".
func TestShimQueryTraces_DropsTheCursorWhenItStopsOnBlockZero(t *testing.T) {
	qe := genesisTraceExecutor(t, 5)

	var page *evm.QueryTracesResponse
	err := qe.shimQueryTraces(context.Background(), &evm.QueryTracesRequest{
		Limit: uint32Ptr(1),
	}, 0, 5, func(msg proto.Message) error {
		page = msg.(*evm.QueryTracesResponse)
		return nil
	})

	require.NoError(t, err)
	require.NotNil(t, page)
	require.Len(t, page.Traces, 1, "the page is full, so blocks 1..5 are still unread")
	assert.Equal(t, uint64(0), page.Traces[0].BlockNumber)
	assert.Nil(t, page.CursorBlock,
		"defect: the page is truncated but names no cursor, so the client stops at genesis")
}

// TestShimQueryTransfers_InheritsTheSameLostCursor proves the defect is not
// confined to eth_queryTraces. Transfers are derived from the trace page, and
// the cursor is copied across untouched, so a transfer walk that begins at
// genesis ends there too.
func TestShimQueryTransfers_InheritsTheSameLostCursor(t *testing.T) {
	qe := genesisTraceExecutor(t, 5)

	var page *evm.QueryTransfersResponse
	err := qe.shimQueryTransfers(context.Background(), &evm.QueryTransfersRequest{
		Limit: uint32Ptr(1),
	}, 0, 5, func(msg proto.Message) error {
		page = msg.(*evm.QueryTransfersResponse)
		return nil
	})

	require.NoError(t, err)
	require.NotNil(t, page)
	require.Len(t, page.Transfers, 1, "the trace at genesis moves value, so it becomes a transfer")
	assert.Nil(t, page.CursorBlock,
		"defect: the truncated transfer page names no cursor either")
}

// TestShimQueryTransactions_KeepsTheCursorOnBlockZero is the contrast case. The
// transaction shim tracks the header it last included rather than its number,
// so genesis paginates correctly. It is here to show the fix shimQueryTraces
// needs is already written one function above it.
func TestShimQueryTransactions_KeepsTheCursorOnBlockZero(t *testing.T) {
	nop := zerolog.Nop()
	qe := &EvmQueryExecutor{
		logger: &nop,
		forwardSubrequestFn: func(_ context.Context, method string, params []interface{}) ([]byte, error) {
			require.Equal(t, "eth_getBlockByNumber", method)
			require.NotEmpty(t, params)
			blockRef, ok := params[0].(string)
			require.True(t, ok)
			var num uint64
			_, err := fmt.Sscanf(blockRef, "0x%x", &num)
			require.NoError(t, err)
			return mustMarshalJSON(t, makeProtoBlockResult(num, []interface{}{
				makeProtoTransactionResult(0x1000+num, num, 0),
			})), nil
		},
	}

	var page *evm.QueryTransactionsResponse
	err := qe.shimQueryTransactions(context.Background(), &evm.QueryTransactionsRequest{
		Limit: uint32Ptr(1),
	}, 0, 5, func(msg proto.Message) error {
		page = msg.(*evm.QueryTransactionsResponse)
		return nil
	})

	require.NoError(t, err)
	require.NotNil(t, page)
	require.Len(t, page.Transactions, 1)
	require.NotNil(t, page.CursorBlock, "genesis is a real block, so the walk must continue past it")
	assert.Equal(t, uint64(0), page.CursorBlock.Number)
}
