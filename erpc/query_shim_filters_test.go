package erpc

import (
	"testing"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

// The shim answers eth_query* calls against upstreams that have no query
// service, by fetching whole blocks and filtering them here in Go. So these
// predicates ARE the query result. A predicate that returns false too often
// drops rows the caller asked for, and the caller cannot tell: the response is
// a well-formed, shorter list. That is the worst shape of data loss in this
// codebase, because an indexer downstream will happily persist the gap.

func addr(b byte) []byte { return []byte{b, b, b, b, b, b, b, b, b, b, b, b, b, b, b, b, b, b, b, b} }

// TestMatchTransactionFilter_KeepsARowOnlyWhenEveryClauseMatches pins the AND
// across from, to and selector. An OR here would return transactions the caller
// did not ask for; a wrong clause would drop ones they did.
func TestMatchTransactionFilter_KeepsARowOnlyWhenEveryClauseMatches(t *testing.T) {
	tx := &evm.Transaction{
		From:  addr(0xaa),
		To:    addr(0xbb),
		Input: []byte{0xde, 0xad, 0xbe, 0xef, 0x01, 0x02},
	}

	// No filter at all means no filtering: an empty request must return the
	// whole range rather than nothing.
	require.True(t, matchTransactionFilter(tx, nil))
	require.True(t, matchTransactionFilter(nil, &evm.TransactionFilter{From: [][]byte{addr(0xaa)}}))
	require.True(t, matchTransactionFilter(tx, &evm.TransactionFilter{}))

	require.True(t, matchTransactionFilter(tx, &evm.TransactionFilter{From: [][]byte{addr(0xaa)}}))
	require.False(t, matchTransactionFilter(tx, &evm.TransactionFilter{From: [][]byte{addr(0xcc)}}))

	// A list of candidates is an OR within one clause: any sender in the list
	// matches, which is how a caller watches several wallets at once.
	require.True(t, matchTransactionFilter(tx, &evm.TransactionFilter{
		From: [][]byte{addr(0xcc), addr(0xaa)},
	}))

	require.True(t, matchTransactionFilter(tx, &evm.TransactionFilter{To: [][]byte{addr(0xbb)}}))
	require.False(t, matchTransactionFilter(tx, &evm.TransactionFilter{To: [][]byte{addr(0xcc)}}))

	require.True(t, matchTransactionFilter(tx, &evm.TransactionFilter{
		Selector: [][]byte{{0xde, 0xad, 0xbe, 0xef}},
	}))
	require.False(t, matchTransactionFilter(tx, &evm.TransactionFilter{
		Selector: [][]byte{{0x00, 0x00, 0x00, 0x00}},
	}))

	// Every clause has to hold at once. A matching sender with the wrong
	// recipient is not a match.
	require.False(t, matchTransactionFilter(tx, &evm.TransactionFilter{
		From: [][]byte{addr(0xaa)},
		To:   [][]byte{addr(0xcc)},
	}))
	require.True(t, matchTransactionFilter(tx, &evm.TransactionFilter{
		From:     [][]byte{addr(0xaa)},
		To:       [][]byte{addr(0xbb)},
		Selector: [][]byte{{0xde, 0xad, 0xbe, 0xef}},
	}))

	// A plain value transfer carries no calldata, so a selector filter must
	// exclude it rather than read past the end of a short input.
	plainTransfer := &evm.Transaction{From: addr(0xaa), To: addr(0xbb), Input: []byte{0xde, 0xad}}
	require.False(t, matchTransactionFilter(plainTransfer, &evm.TransactionFilter{
		Selector: [][]byte{{0xde, 0xad, 0xbe, 0xef}},
	}))
	require.True(t, matchTransactionFilter(plainTransfer, &evm.TransactionFilter{
		From: [][]byte{addr(0xaa)},
	}))
}

// TestMatchTraceFilter_AddsTheTopLevelClauseToTheSameRules covers the trace
// predicate. Its extra clause separates a transaction's own call from the
// internal calls it made, which is how a caller asks for one without the other.
func TestMatchTraceFilter_AddsTheTopLevelClauseToTheSameRules(t *testing.T) {
	topLevel := &evm.Trace{
		From:  addr(0xaa),
		To:    addr(0xbb),
		Input: []byte{0xde, 0xad, 0xbe, 0xef},
	}
	internal := &evm.Trace{
		From:         addr(0xaa),
		To:           addr(0xbb),
		Input:        []byte{0xde, 0xad, 0xbe, 0xef},
		TraceAddress: []uint32{0},
	}

	require.True(t, matchTraceFilter(topLevel, nil))
	require.True(t, matchTraceFilter(nil, &evm.TraceFilter{From: [][]byte{addr(0xaa)}}))

	require.True(t, matchTraceFilter(topLevel, &evm.TraceFilter{From: [][]byte{addr(0xaa)}}))
	require.False(t, matchTraceFilter(topLevel, &evm.TraceFilter{From: [][]byte{addr(0xcc)}}))
	require.True(t, matchTraceFilter(topLevel, &evm.TraceFilter{To: [][]byte{addr(0xbb)}}))
	require.False(t, matchTraceFilter(topLevel, &evm.TraceFilter{To: [][]byte{addr(0xcc)}}))

	require.True(t, matchTraceFilter(topLevel, &evm.TraceFilter{
		Selector: [][]byte{{0xde, 0xad, 0xbe, 0xef}},
	}))
	require.False(t, matchTraceFilter(topLevel, &evm.TraceFilter{
		Selector: [][]byte{{0x11, 0x22, 0x33, 0x44}},
	}))
	require.False(t, matchTraceFilter(
		&evm.Trace{From: addr(0xaa), Input: []byte{0xde}},
		&evm.TraceFilter{Selector: [][]byte{{0xde, 0xad, 0xbe, 0xef}}}))

	require.True(t, matchTraceFilter(topLevel, &evm.TraceFilter{IsTopLevel: util.BoolPtr(true)}))
	require.False(t, matchTraceFilter(internal, &evm.TraceFilter{IsTopLevel: util.BoolPtr(true)}),
		"a trace with a trace address is an internal call and must be excluded")

	// isTopLevel:false is not "only internal calls" — it switches the clause
	// off, so both kinds come back.
	require.True(t, matchTraceFilter(internal, &evm.TraceFilter{IsTopLevel: util.BoolPtr(false)}))
	require.True(t, matchTraceFilter(topLevel, &evm.TraceFilter{IsTopLevel: util.BoolPtr(false)}))
}

// TestMatchTransferFilter_SelectsNativeValueMovements covers the transfer
// predicate, which has from, to and top-level clauses but no selector — a
// native value movement carries no calldata to match on.
func TestMatchTransferFilter_SelectsNativeValueMovements(t *testing.T) {
	topLevel := &evm.NativeTransfer{From: addr(0xaa), To: addr(0xbb)}
	internal := &evm.NativeTransfer{From: addr(0xaa), To: addr(0xbb), TraceAddress: []uint32{0, 1}}

	require.True(t, matchTransferFilter(topLevel, nil))
	require.True(t, matchTransferFilter(nil, &evm.TransferFilter{From: [][]byte{addr(0xaa)}}))
	require.True(t, matchTransferFilter(topLevel, &evm.TransferFilter{}))

	require.True(t, matchTransferFilter(topLevel, &evm.TransferFilter{From: [][]byte{addr(0xaa)}}))
	require.False(t, matchTransferFilter(topLevel, &evm.TransferFilter{From: [][]byte{addr(0xcc)}}))
	require.True(t, matchTransferFilter(topLevel, &evm.TransferFilter{To: [][]byte{addr(0xbb)}}))
	require.False(t, matchTransferFilter(topLevel, &evm.TransferFilter{To: [][]byte{addr(0xcc)}}))

	require.False(t, matchTransferFilter(topLevel, &evm.TransferFilter{
		From: [][]byte{addr(0xaa)},
		To:   [][]byte{addr(0xcc)},
	}), "both endpoints must match, not either one")

	require.True(t, matchTransferFilter(topLevel, &evm.TransferFilter{IsTopLevel: util.BoolPtr(true)}))
	require.False(t, matchTransferFilter(internal, &evm.TransferFilter{IsTopLevel: util.BoolPtr(true)}),
		"a transfer made from inside a contract call is not top level")
	require.True(t, matchTransferFilter(internal, &evm.TransferFilter{IsTopLevel: util.BoolPtr(false)}))
}

// TestBytesMatchAny_ComparesContentRatherThanIdentity guards the one thing that
// silently breaks address matching in Go: comparing slices by identity. Every
// address the shim tests arrives from a fresh decode, so it is never the same
// backing array as the filter's copy.
func TestBytesMatchAny_ComparesContentRatherThanIdentity(t *testing.T) {
	target := addr(0xaa)
	sameContent := addr(0xaa)
	require.NotSame(t, &target[0], &sameContent[0])

	require.True(t, bytesMatchAny(target, [][]byte{sameContent}))
	require.True(t, bytesMatchAny(target, [][]byte{addr(0xbb), addr(0xcc), sameContent}))
	require.False(t, bytesMatchAny(target, [][]byte{addr(0xbb)}))
	require.False(t, bytesMatchAny(target, nil), "an empty candidate list matches nothing")

	// A contract creation has no recipient. Nil must not match a real address,
	// or every creation would be returned for every `to` filter.
	require.False(t, bytesMatchAny(nil, [][]byte{addr(0xaa)}))
	require.True(t, bytesMatchAny(nil, [][]byte{nil}))

	// A shorter address is not a match for a longer one that starts the same
	// way — otherwise a truncated filter value would sweep in unrelated rows.
	require.False(t, bytesMatchAny([]byte{0xaa, 0xbb}, [][]byte{{0xaa}}))
}

// TestBytesPrefixMatchAny_IsAnEqualityTestDespiteItsName records a naming trap.
// The function compares the whole value, so it only behaves like a prefix match
// because its one caller slices the input to exactly four bytes first
// (query_shim.go:549 and :567). A future caller that passes a 3-byte selector
// expecting a prefix match would get no rows and no error.
func TestBytesPrefixMatchAny_IsAnEqualityTestDespiteItsName(t *testing.T) {
	selector := []byte{0xde, 0xad, 0xbe, 0xef}

	require.True(t, bytesPrefixMatchAny(selector, [][]byte{{0xde, 0xad, 0xbe, 0xef}}))
	require.True(t, bytesPrefixMatchAny(selector, [][]byte{{0x11}, {0xde, 0xad, 0xbe, 0xef}}))
	require.False(t, bytesPrefixMatchAny(selector, [][]byte{{0x11, 0x22, 0x33, 0x44}}))
	require.False(t, bytesPrefixMatchAny(selector, nil))

	require.False(t, bytesPrefixMatchAny(selector, [][]byte{{0xde, 0xad}}),
		"read the note above: a real prefix match would return true here")
}

// TestReverseLogs_FlipsTheOrderInPlace covers the descending-order path. A
// caller that asks for newest-first and gets oldest-first pages from the wrong
// end of the range and misses rows entirely.
func TestReverseLogs_FlipsTheOrderInPlace(t *testing.T) {
	logs := []*evm.Log{
		{BlockNumber: 1}, {BlockNumber: 2}, {BlockNumber: 3},
	}
	reverseLogs(logs)
	require.Equal(t, []uint64{3, 2, 1}, []uint64{
		logs[0].BlockNumber, logs[1].BlockNumber, logs[2].BlockNumber,
	})

	even := []*evm.Log{{BlockNumber: 1}, {BlockNumber: 2}}
	reverseLogs(even)
	require.Equal(t, uint64(2), even[0].BlockNumber)
	require.Equal(t, uint64(1), even[1].BlockNumber)

	// The degenerate sizes must not panic — an empty range is a normal answer.
	reverseLogs(nil)
	single := []*evm.Log{{BlockNumber: 9}}
	reverseLogs(single)
	require.Equal(t, uint64(9), single[0].BlockNumber)
}

// TestBlockIterator_WalksEveryBlockInTheRangeExactlyOnce is the shim's range
// walk. A range it ends one block early leaves the caller a hole at the edge of
// every query, which is the hardest kind of gap to notice.
func TestBlockIterator_WalksEveryBlockInTheRangeExactlyOnce(t *testing.T) {
	// The walk is capped. A descending iterator that steps below block 0 wraps
	// to the top of the uint64 range and never terminates, so without a cap the
	// symptom would be a hung test rather than a failed one — and a hang in this
	// package reads as an unrelated timeout.
	const maxSteps = 64
	collect := func(from, to uint64, order evm.SortOrder) []uint64 {
		t.Helper()
		it := newBlockIterator(from, to, order)
		out := []uint64{}
		for it.Next() {
			out = append(out, it.Value())
			require.Less(t, len(out), maxSteps,
				"the iterator walked past %d blocks over the range %d..%d; "+
					"it is not terminating, most likely by wrapping below block 0",
				maxSteps, from, to)
		}
		return out
	}

	require.Equal(t, []uint64{10, 11, 12}, collect(10, 12, evm.SortOrder_ASC),
		"both ends of the range are inclusive")
	require.Equal(t, []uint64{12, 11, 10}, collect(10, 12, evm.SortOrder_DESC))

	require.Equal(t, []uint64{7}, collect(7, 7, evm.SortOrder_ASC),
		"a single-block range still yields that block")
	require.Equal(t, []uint64{7}, collect(7, 7, evm.SortOrder_DESC))

	require.Empty(t, collect(12, 10, evm.SortOrder_ASC),
		"an inverted range yields nothing rather than looping")

	// Descending down to genesis must stop at block 0 instead of wrapping
	// round to the top of the uint64 range.
	require.Equal(t, []uint64{2, 1, 0}, collect(0, 2, evm.SortOrder_DESC))
}

// TestBlockIterator_HasMoreReportsWhetherTheWalkIsFinished covers the flag the
// shim uses to decide whether to hand the caller a continuation cursor. A
// wrong answer either truncates the query or loops it forever.
func TestBlockIterator_HasMoreReportsWhetherTheWalkIsFinished(t *testing.T) {
	it := newBlockIterator(10, 12, evm.SortOrder_ASC)
	require.True(t, it.Next())
	require.True(t, it.HasMore(), "at block 10 of 10..12 there is more to walk")
	require.True(t, it.Next())
	require.True(t, it.HasMore())
	require.True(t, it.Next())
	require.False(t, it.HasMore(), "the last block of the range is not 'more'")
	require.False(t, it.Next())

	desc := newBlockIterator(10, 12, evm.SortOrder_DESC)
	require.True(t, desc.Next())
	require.True(t, desc.HasMore())
	require.True(t, desc.Next())
	require.True(t, desc.Next())
	require.False(t, desc.HasMore())
	require.False(t, desc.Next())
}

// TestCursorFromBlock_CarriesTheHashesAReorgCheckNeeds covers the continuation
// cursor. Number alone is not enough: a caller resuming after a reorg needs the
// hash and parent hash to notice that the chain moved under them.
func TestCursorFromBlock_CarriesTheHashesAReorgCheckNeeds(t *testing.T) {
	require.Nil(t, cursorFromBlock(nil))

	cursor := cursorFromBlock(&evm.BlockHeader{
		Number:     42,
		Hash:       []byte{0x01},
		ParentHash: []byte{0x02},
	})
	require.Equal(t, uint64(42), cursor.Number)
	require.Equal(t, []byte{0x01}, cursor.Hash)
	require.Equal(t, []byte{0x02}, cursor.ParentHash)

	// The number-only form is what the log paginator emits mid-block-range,
	// where no single header defines the resume point.
	require.Equal(t, uint64(7), cursorFromNumber(7).Number)
	require.Nil(t, cursorFromNumber(7).Hash)
}

// TestHeaderAccessors_TolerateAMissingHeader covers the two nil guards. The
// shim reads these from a block it may have failed to fetch, so a nil header
// must yield nothing rather than panic in the middle of a served query.
func TestHeaderAccessors_TolerateAMissingHeader(t *testing.T) {
	require.Nil(t, headerHash(nil))
	require.Nil(t, headerTimestamp(nil))

	header := &evm.BlockHeader{Hash: []byte{0xab}, Timestamp: 1700000000}
	require.Equal(t, []byte{0xab}, headerHash(header))
	require.NotNil(t, headerTimestamp(header))
	require.Equal(t, uint64(1700000000), *headerTimestamp(header))
}
