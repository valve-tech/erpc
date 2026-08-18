package erpc

import (
	"testing"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/stretchr/testify/require"
)

func TestProjectBlockFieldsPreservesIdentity(t *testing.T) {
	block := &evm.BlockHeader{
		Number:     10,
		Hash:       []byte{0xaa},
		ParentHash: []byte{0xbb},
		Timestamp:  123,
		GasLimit:   100,
		GasUsed:    50,
	}

	ProjectBlockFields(block, &evm.BlockFieldSelection{Timestamp: true})

	require.Equal(t, uint64(10), block.Number)
	require.Equal(t, []byte{0xaa}, block.Hash)
	require.Equal(t, uint64(123), block.Timestamp)
	require.Nil(t, block.ParentHash)
	require.Zero(t, block.GasLimit)
	require.Zero(t, block.GasUsed)
}

func TestProjectBlockForResponseClonesBeforeProjection(t *testing.T) {
	original := &evm.BlockHeader{
		Number:     42,
		Hash:       []byte{0xaa},
		ParentHash: []byte{0xbb},
		Timestamp:  123,
	}

	projected := projectBlockForResponse(original, &evm.BlockFieldSelection{Hash: true})
	cursor := cursorFromBlock(original)

	require.NotSame(t, original, projected)
	require.Equal(t, []byte{0xbb}, cursor.ParentHash)
	require.Equal(t, []byte{0xbb}, original.ParentHash)
	require.Nil(t, projected.ParentHash)
	require.Equal(t, []byte{0xaa}, projected.Hash)
}

func TestProjectLogForResponseClonesBeforeProjection(t *testing.T) {
	original := &evm.Log{
		BlockNumber:     42,
		TransactionHash: []byte{0xaa},
		LogIndex:        3,
	}

	projected := projectLogForResponse(original, &evm.LogFieldSelection{LogIndex: true})

	require.NotSame(t, original, projected)
	require.Equal(t, uint64(42), original.BlockNumber)
	require.Equal(t, []byte{0xaa}, original.TransactionHash)
	require.Zero(t, projected.BlockNumber)
	require.Nil(t, projected.TransactionHash)
	require.Equal(t, uint32(3), projected.LogIndex)
}

func TestProjectTransactionForResponseClonesBeforeProjection(t *testing.T) {
	original := &evm.Transaction{
		Hash:             []byte{0xaa},
		From:             []byte{0x01},
		TransactionIndex: func() *uint32 { v := uint32(7); return &v }(),
	}

	projected := projectTransactionForResponse(original, &evm.TransactionFieldSelection{From: true})

	require.NotSame(t, original, projected)
	require.Equal(t, []byte{0xaa}, original.Hash)
	require.Equal(t, uint32(7), *original.TransactionIndex)
	require.Equal(t, []byte{0x01}, projected.From)
	require.Nil(t, projected.TransactionIndex)
}

func TestProjectTraceForResponseClonesBeforeProjection(t *testing.T) {
	original := &evm.Trace{
		CallType:        evm.TraceCallType_TRACE_CALL_DELEGATECALL,
		TransactionHash: []byte{0xaa},
		TraceAddress:    []uint32{1, 2},
	}

	projected := projectTraceForResponse(original, &evm.TraceFieldSelection{TransactionHash: true})

	require.NotSame(t, original, projected)
	require.Equal(t, evm.TraceCallType_TRACE_CALL_DELEGATECALL, original.CallType)
	require.Equal(t, []byte{0xaa}, original.TransactionHash)
	require.Equal(t, []uint32{1, 2}, original.TraceAddress)
	require.Equal(t, []byte{0xaa}, projected.TransactionHash)
	require.Equal(t, evm.TraceCallType_TRACE_CALL_CALL, projected.CallType)
	require.Nil(t, projected.TraceAddress)
}

func TestProjectTraceFields(t *testing.T) {
	trace := &evm.Trace{
		From:            []byte{0x1},
		To:              []byte{0x2},
		Value:           "0x10",
		TransactionHash: []byte{0x3},
		BlockHash:       []byte{0x4},
		GasUsed:         21,
	}

	ProjectTraceFields(trace, &evm.TraceFieldSelection{From: true, Value: true})

	require.Equal(t, []byte{0x1}, trace.From)
	require.Equal(t, "0x10", trace.Value)
	require.Nil(t, trace.To)
	require.Nil(t, trace.TransactionHash)
	require.Nil(t, trace.BlockHash)
	require.Zero(t, trace.GasUsed)
}

// A field selection is a promise about what the answer contains. Keeping a
// field the client did not ask for leaks data across a projection boundary;
// dropping one it did ask for makes the row look empty rather than filtered.
// ProjectTransferFields decides nine fields one at a time, so a single test
// with one field selected would leave eight decisions unmade.

// fullTransfer is a native transfer with every projectable field populated, so
// a field the projection fails to clear is visible.
func fullTransfer() *evm.NativeTransfer {
	ts := uint64(1700000000)
	return &evm.NativeTransfer{
		From:             []byte{0x1},
		To:               []byte{0x2},
		Value:            "0x10",
		TransactionHash:  []byte{0x3},
		TransactionIndex: 7,
		BlockNumber:      42,
		BlockHash:        []byte{0x4},
		TraceAddress:     []uint32{0, 1},
		BlockTimestamp:   &ts,
	}
}

func TestProjectTransferFields_ClearsEveryFieldTheSelectionOmits(t *testing.T) {
	transfer := fullTransfer()

	ProjectTransferFields(transfer, &evm.TransferFieldSelection{From: true, Value: true})

	require.Equal(t, []byte{0x1}, transfer.From)
	require.Equal(t, "0x10", transfer.Value)
	require.Nil(t, transfer.To)
	require.Nil(t, transfer.TransactionHash)
	require.Zero(t, transfer.TransactionIndex)
	require.Zero(t, transfer.BlockNumber)
	require.Nil(t, transfer.BlockHash)
	require.Nil(t, transfer.TraceAddress)
	require.Nil(t, transfer.BlockTimestamp)
}

func TestProjectTransferFields_KeepsEveryFieldTheSelectionNames(t *testing.T) {
	transfer := fullTransfer()
	want := fullTransfer()

	ProjectTransferFields(transfer, &evm.TransferFieldSelection{
		From: true, To: true, Value: true, TransactionHash: true,
		TransactionIndex: true, BlockNumber: true, BlockHash: true,
		TraceAddress: true, BlockTimestamp: true,
	})

	require.Equal(t, want.From, transfer.From)
	require.Equal(t, want.To, transfer.To)
	require.Equal(t, want.Value, transfer.Value)
	require.Equal(t, want.TransactionHash, transfer.TransactionHash)
	require.Equal(t, want.TransactionIndex, transfer.TransactionIndex)
	require.Equal(t, want.BlockNumber, transfer.BlockNumber)
	require.Equal(t, want.BlockHash, transfer.BlockHash)
	require.Equal(t, want.TraceAddress, transfer.TraceAddress)
	require.Equal(t, *want.BlockTimestamp, *transfer.BlockTimestamp)
}

// TestProjectTransferFields_LeavesTheRowAloneWithoutASelection pins the
// no-selection case. eth_queryTransfers sends no selection when the client
// asked for none, and the shim calls the projection anyway — so "no selection"
// has to mean "every field", not "no field".
func TestProjectTransferFields_LeavesTheRowAloneWithoutASelection(t *testing.T) {
	transfer := fullTransfer()

	ProjectTransferFields(transfer, nil)

	require.Equal(t, []byte{0x1}, transfer.From)
	require.Equal(t, "0x10", transfer.Value)
	require.Equal(t, uint64(42), transfer.BlockNumber)
	require.Equal(t, []uint32{0, 1}, transfer.TraceAddress)

	require.NotPanics(t, func() {
		ProjectTransferFields(nil, &evm.TransferFieldSelection{From: true})
	}, "a nil row is skipped rather than dereferenced")
}
