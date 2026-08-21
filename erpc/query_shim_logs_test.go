package erpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// eth_queryLogs has no upstream to serve it, so eRPC shims it onto eth_getLogs.
// The translation is where the risk sits: a topic that lands in the wrong slot,
// or an address list flattened the wrong way, returns a full page of logs that
// simply are not the ones the caller asked for. The caller cannot tell.

// logShimExecutor builds a query executor whose subrequests a test answers.
// forwarded records every call so a test can assert on the exact JSON eRPC put
// on the wire, which is the only thing the upstream ever sees.
type logShimExecutor struct {
	qe        *EvmQueryExecutor
	forwarded []forwardedCall
}

type forwardedCall struct {
	method string
	params []interface{}
}

func newLogShimExecutor(answer func(method string, params []interface{}) ([]byte, error)) *logShimExecutor {
	nop := zerolog.Nop()
	h := &logShimExecutor{}
	h.qe = &EvmQueryExecutor{
		logger: &nop,
		forwardSubrequestFn: func(_ context.Context, method string, params []interface{}) ([]byte, error) {
			h.forwarded = append(h.forwarded, forwardedCall{method: method, params: params})
			return answer(method, params)
		},
	}
	return h
}

// only returns the single call made with the given method, failing if there is
// not exactly one.
func (h *logShimExecutor) only(t *testing.T, method string) forwardedCall {
	t.Helper()
	var found []forwardedCall
	for _, c := range h.forwarded {
		if c.method == method {
			found = append(found, c)
		}
	}
	require.Len(t, found, 1, "expected exactly one %s subrequest, got %d", method, len(found))
	return found[0]
}

// rawLog is one entry of an eth_getLogs result, in the shape an upstream sends.
func rawLog(blockNumber, logIndex uint64, address string, topics ...string) map[string]interface{} {
	if topics == nil {
		topics = []string{}
	}
	return map[string]interface{}{
		"address":          address,
		"blockHash":        fmt.Sprintf("0x%064x", blockNumber),
		"blockNumber":      fmt.Sprintf("0x%x", blockNumber),
		"data":             "0x",
		"logIndex":         fmt.Sprintf("0x%x", logIndex),
		"topics":           topics,
		"transactionHash":  fmt.Sprintf("0x%064x", blockNumber*1000+logIndex),
		"transactionIndex": "0x0",
	}
}

// hexBytes turns a 0x string into the raw bytes a proto filter carries.
func hexBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := evm.HexToBytes(s)
	require.NoError(t, err)
	return b
}

const (
	addrA  = "0x00000000000000000000000000000000000000aa"
	addrB  = "0x00000000000000000000000000000000000000bb"
	topic0 = "0x0000000000000000000000000000000000000000000000000000000000000001"
	topic1 = "0x0000000000000000000000000000000000000000000000000000000000000002"
	topic2 = "0x0000000000000000000000000000000000000000000000000000000000000003"
)

// TestFetchLogsViaForward_TranslatesAFilterIntoTheFilterAnUpstreamUnderstands
// is the load-bearing test for the shim. eth_getLogs gives one meaning to a
// scalar address and another to an array, and topics are positional with null
// standing for "any". Every case here changes which logs come back.
func TestFetchLogsViaForward_TranslatesAFilterIntoTheFilterAnUpstreamUnderstands(t *testing.T) {
	cases := []struct {
		name   string
		filter *evm.LogFilter
		want   string
	}{
		{
			name:   "no filter asks only for a block range",
			filter: nil,
			want:   `{"fromBlock":"0x10","toBlock":"0x20"}`,
		},
		{
			name:   "an empty filter asks only for a block range",
			filter: &evm.LogFilter{},
			want:   `{"fromBlock":"0x10","toBlock":"0x20"}`,
		},
		{
			name: "one address goes as a scalar",
			filter: &evm.LogFilter{
				Address: [][]byte{hexBytes(t, addrA)},
			},
			want: `{"address":"` + addrA + `","fromBlock":"0x10","toBlock":"0x20"}`,
		},
		{
			name: "several addresses go as an array",
			filter: &evm.LogFilter{
				Address: [][]byte{hexBytes(t, addrA), hexBytes(t, addrB)},
			},
			want: `{"address":["` + addrA + `","` + addrB + `"],"fromBlock":"0x10","toBlock":"0x20"}`,
		},
		{
			name: "one value in a topic slot goes as a scalar",
			filter: &evm.LogFilter{
				Topics: []*evm.TopicFilter{{Values: [][]byte{hexBytes(t, topic0)}}},
			},
			want: `{"fromBlock":"0x10","toBlock":"0x20","topics":["` + topic0 + `"]}`,
		},
		{
			name: "several values in a topic slot go as an array",
			filter: &evm.LogFilter{
				Topics: []*evm.TopicFilter{{Values: [][]byte{hexBytes(t, topic0), hexBytes(t, topic1)}}},
			},
			want: `{"fromBlock":"0x10","toBlock":"0x20","topics":[["` + topic0 + `","` + topic1 + `"]]}`,
		},
		{
			name: "an empty slot becomes null so the later slots keep their positions",
			filter: &evm.LogFilter{
				Topics: []*evm.TopicFilter{
					{Values: [][]byte{hexBytes(t, topic0)}},
					{},
					{Values: [][]byte{hexBytes(t, topic2)}},
				},
			},
			want: `{"fromBlock":"0x10","toBlock":"0x20","topics":["` + topic0 + `",null,"` + topic2 + `"]}`,
		},
		{
			name: "a nil slot becomes null the same way",
			filter: &evm.LogFilter{
				Topics: []*evm.TopicFilter{nil, {Values: [][]byte{hexBytes(t, topic1)}}},
			},
			want: `{"fromBlock":"0x10","toBlock":"0x20","topics":[null,"` + topic1 + `"]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newLogShimExecutor(func(string, []interface{}) ([]byte, error) {
				return []byte(`[]`), nil
			})

			logs, err := h.qe.fetchLogsViaForward(context.Background(), 0x10, 0x20, tc.filter)
			require.NoError(t, err)
			require.Empty(t, logs)

			call := h.only(t, "eth_getLogs")
			require.Len(t, call.params, 1, "eth_getLogs takes exactly one filter object")
			encoded, err := json.Marshal(call.params[0])
			require.NoError(t, err)
			require.JSONEq(t, tc.want, string(encoded))
		})
	}
}

// TestFetchLogsViaForward_ConvertsEachEntryAndStopsOnOneItCannotRead covers the
// decode half. A log eRPC cannot parse must fail the query: silently dropping it
// would return a short page that looks complete.
func TestFetchLogsViaForward_ConvertsEachEntryAndStopsOnOneItCannotRead(t *testing.T) {
	t.Run("well-formed logs convert in order", func(t *testing.T) {
		h := newLogShimExecutor(func(string, []interface{}) ([]byte, error) {
			return mustMarshalJSON(t, []map[string]interface{}{
				rawLog(16, 0, addrA, topic0),
				rawLog(16, 1, addrB, topic0, topic1),
			}), nil
		})

		logs, err := h.qe.fetchLogsViaForward(context.Background(), 0x10, 0x10, nil)
		require.NoError(t, err)
		require.Len(t, logs, 2)
		require.Equal(t, uint64(16), logs[0].BlockNumber)
		require.Equal(t, uint32(0), logs[0].LogIndex)
		require.Equal(t, hexBytes(t, addrA), logs[0].Address)
		require.Equal(t, uint32(1), logs[1].LogIndex)
		require.Len(t, logs[1].Topics, 2, "a dropped topic changes what the log means")
	})

	t.Run("an unreadable entry fails the whole fetch", func(t *testing.T) {
		bad := rawLog(16, 0, addrA)
		bad["blockNumber"] = "not-a-number"
		h := newLogShimExecutor(func(string, []interface{}) ([]byte, error) {
			return mustMarshalJSON(t, []map[string]interface{}{
				rawLog(16, 0, addrA), bad,
			}), nil
		})

		_, err := h.qe.fetchLogsViaForward(context.Background(), 0x10, 0x10, nil)
		require.Error(t, err, "a short page that looks complete is worse than an error")
	})

	t.Run("a result that is not an array fails the fetch", func(t *testing.T) {
		h := newLogShimExecutor(func(string, []interface{}) ([]byte, error) {
			return []byte(`{"unexpected":true}`), nil
		})

		_, err := h.qe.fetchLogsViaForward(context.Background(), 0x10, 0x10, nil)
		require.Error(t, err)
	})

	t.Run("an upstream failure reaches the caller unchanged", func(t *testing.T) {
		boom := errors.New("upstream refused")
		h := newLogShimExecutor(func(string, []interface{}) ([]byte, error) {
			return nil, boom
		})

		_, err := h.qe.fetchLogsViaForward(context.Background(), 0x10, 0x10, nil)
		require.ErrorIs(t, err, boom)
	})
}

// TestShimQueryLogs_AnswersAnInvertedRangeWithoutAskingAnUpstream covers the
// degenerate range. Forwarding it would ask an upstream for a range it cannot
// serve, and some vendors bill for that.
func TestShimQueryLogs_AnswersAnInvertedRangeWithoutAskingAnUpstream(t *testing.T) {
	h := newLogShimExecutor(func(method string, _ []interface{}) ([]byte, error) {
		t.Fatalf("an inverted range must not reach an upstream, but %s did", method)
		return nil, nil
	})

	var page *evm.QueryLogsResponse
	err := h.qe.shimQueryLogs(context.Background(), &evm.QueryLogsRequest{}, 20, 10,
		func(msg proto.Message) error {
			page = msg.(*evm.QueryLogsResponse)
			return nil
		})

	require.NoError(t, err)
	require.NotNil(t, page)
	require.Empty(t, page.Logs)
	require.NotNil(t, page.FromBlock)
	require.Equal(t, uint64(20), page.FromBlock.Number)
	require.Equal(t, uint64(10), page.ToBlock.Number)
	require.Nil(t, page.CursorBlock)
	require.Empty(t, h.forwarded)
}

// TestShimQueryLogs_OrdersAndPaginatesByWholeBlocks pins the two decisions a
// caller pages on. A block split across two pages would make the caller either
// re-read logs it already has or skip them, depending on how it uses the cursor.
func TestShimQueryLogs_OrdersAndPaginatesByWholeBlocks(t *testing.T) {
	answer := func(string, []interface{}) ([]byte, error) {
		return mustMarshalJSON(t, []map[string]interface{}{
			rawLog(10, 0, addrA),
			rawLog(10, 1, addrA),
			rawLog(11, 0, addrA),
			rawLog(11, 1, addrA),
		}), nil
	}

	t.Run("ascending keeps the upstream order", func(t *testing.T) {
		h := newLogShimExecutor(answer)
		var page *evm.QueryLogsResponse
		require.NoError(t, h.qe.shimQueryLogs(context.Background(),
			&evm.QueryLogsRequest{Limit: uint32Ptr(3)}, 10, 11,
			func(msg proto.Message) error { page = msg.(*evm.QueryLogsResponse); return nil }))

		require.Len(t, page.Logs, 2, "block 11 does not fit, so it waits for the next page")
		require.Equal(t, uint64(10), page.Logs[0].BlockNumber)
		require.Equal(t, uint64(10), page.Logs[1].BlockNumber)
		require.NotNil(t, page.CursorBlock)
		require.Equal(t, uint64(10), page.CursorBlock.Number,
			"the cursor names the last WHOLE block returned")
	})

	t.Run("descending reverses before it pages", func(t *testing.T) {
		h := newLogShimExecutor(answer)
		var page *evm.QueryLogsResponse
		require.NoError(t, h.qe.shimQueryLogs(context.Background(),
			&evm.QueryLogsRequest{Limit: uint32Ptr(3), Order: evm.SortOrder_DESC.Enum()}, 10, 11,
			func(msg proto.Message) error { page = msg.(*evm.QueryLogsResponse); return nil }))

		require.Len(t, page.Logs, 2)
		require.Equal(t, uint64(11), page.Logs[0].BlockNumber,
			"a DESC query that returns ascending logs pages backwards forever")
		require.Equal(t, uint32(1), page.Logs[0].LogIndex)
		require.Equal(t, uint64(11), page.CursorBlock.Number)
	})

	t.Run("a page that holds everything carries no cursor", func(t *testing.T) {
		h := newLogShimExecutor(answer)
		var page *evm.QueryLogsResponse
		require.NoError(t, h.qe.shimQueryLogs(context.Background(),
			&evm.QueryLogsRequest{Limit: uint32Ptr(10)}, 10, 11,
			func(msg proto.Message) error { page = msg.(*evm.QueryLogsResponse); return nil }))

		require.Len(t, page.Logs, 4)
		require.Nil(t, page.CursorBlock, "a cursor here would make the caller ask again forever")
	})
}

// TestShimQueryLogs_FetchesAParentBlockOncePerBlock covers the join onto block
// and transaction data. The fetch is one upstream call per block, so repeating
// it per log would turn a 200-log page into 200 upstream calls.
func TestShimQueryLogs_FetchesAParentBlockOncePerBlock(t *testing.T) {
	blockCalls := 0
	h := newLogShimExecutor(func(method string, params []interface{}) ([]byte, error) {
		switch method {
		case "eth_getLogs":
			return mustMarshalJSON(t, []map[string]interface{}{
				rawLog(1, 0, addrA),
				rawLog(1, 1, addrA),
				rawLog(2, 0, addrB),
			}), nil
		case "eth_getBlockByNumber":
			blockCalls++
			ref := params[0].(string)
			num := uint64(1)
			if ref == "0x2" {
				num = 2
			}
			return mustMarshalJSON(t, makeProtoBlockResult(num, []interface{}{
				makeProtoTransactionResult(num*1000, num, 0),
			})), nil
		}
		return nil, fmt.Errorf("unexpected method %s", method)
	})

	var page *evm.QueryLogsResponse
	require.NoError(t, h.qe.shimQueryLogs(context.Background(), &evm.QueryLogsRequest{
		BlockFields:       &evm.BlockFieldSelection{},
		TransactionFields: &evm.TransactionFieldSelection{},
	}, 1, 2, func(msg proto.Message) error {
		page = msg.(*evm.QueryLogsResponse)
		return nil
	}))

	require.Len(t, page.Logs, 3)
	require.Equal(t, 2, blockCalls,
		"three logs over two blocks must cost two block fetches, not three")
	require.Len(t, page.Blocks, 2, "each block must appear once")

	numbers := map[uint64]bool{}
	for _, b := range page.Blocks {
		require.False(t, numbers[b.Number], "block %d was returned twice", b.Number)
		numbers[b.Number] = true
	}
	require.True(t, numbers[1] && numbers[2])
}

// TestShimQueryLogs_SkipsTheParentFetchWhenNoParentFieldsWereAskedFor is the
// other half of that rule: a caller who wants only logs must not pay for a
// block fetch per block.
func TestShimQueryLogs_SkipsTheParentFetchWhenNoParentFieldsWereAskedFor(t *testing.T) {
	h := newLogShimExecutor(func(method string, _ []interface{}) ([]byte, error) {
		if method != "eth_getLogs" {
			t.Fatalf("no parent fields were requested, yet eRPC called %s", method)
		}
		return mustMarshalJSON(t, []map[string]interface{}{
			rawLog(1, 0, addrA),
			rawLog(2, 0, addrB),
		}), nil
	})

	var page *evm.QueryLogsResponse
	require.NoError(t, h.qe.shimQueryLogs(context.Background(), &evm.QueryLogsRequest{}, 1, 2,
		func(msg proto.Message) error { page = msg.(*evm.QueryLogsResponse); return nil }))

	require.Len(t, page.Logs, 2)
	require.Empty(t, page.Blocks)
	require.Empty(t, page.Transactions)
	require.Len(t, h.forwarded, 1)
}

// TestShimQueryLogs_ReportsAFailureRatherThanAPartialPage covers both failure
// exits. A caller that receives a page has no way to know it is incomplete, so
// neither failure may be swallowed.
func TestShimQueryLogs_ReportsAFailureRatherThanAPartialPage(t *testing.T) {
	t.Run("the log fetch fails", func(t *testing.T) {
		boom := errors.New("upstream refused")
		h := newLogShimExecutor(func(string, []interface{}) ([]byte, error) { return nil, boom })

		called := false
		err := h.qe.shimQueryLogs(context.Background(), &evm.QueryLogsRequest{}, 1, 2,
			func(proto.Message) error { called = true; return nil })

		require.ErrorIs(t, err, boom)
		require.False(t, called, "a failed query must not deliver a page at all")
	})

	t.Run("the parent block fetch fails", func(t *testing.T) {
		boom := errors.New("block gone")
		h := newLogShimExecutor(func(method string, _ []interface{}) ([]byte, error) {
			if method == "eth_getLogs" {
				return mustMarshalJSON(t, []map[string]interface{}{rawLog(1, 0, addrA)}), nil
			}
			return nil, boom
		})

		called := false
		err := h.qe.shimQueryLogs(context.Background(), &evm.QueryLogsRequest{
			BlockFields: &evm.BlockFieldSelection{},
		}, 1, 2, func(proto.Message) error { called = true; return nil })

		require.ErrorIs(t, err, boom)
		require.False(t, called)
	})

	t.Run("the page delivery itself fails", func(t *testing.T) {
		boom := errors.New("stream closed")
		h := newLogShimExecutor(func(string, []interface{}) ([]byte, error) {
			return mustMarshalJSON(t, []map[string]interface{}{rawLog(1, 0, addrA)}), nil
		})

		err := h.qe.shimQueryLogs(context.Background(), &evm.QueryLogsRequest{}, 1, 2,
			func(proto.Message) error { return boom })
		require.ErrorIs(t, err, boom,
			"the gRPC stream failing must not look like a completed query")
	})
}
