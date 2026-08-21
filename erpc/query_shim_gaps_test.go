package erpc

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// A range query walks blocks one at a time, and an upstream does not always
// have every one of them: a pruned node answers `null`, and eRPC turns a
// too-old block into a missing-data error. Both mean "not here", and the shim
// has to tell them apart from a real failure. Getting that wrong is expensive
// in one direction and silent in the other — an absent block reported as an
// error fails the whole page, and a real failure read as an absent block
// deletes rows from the answer without saying so.

// blockAnswers builds an executor whose eth_getBlockByNumber answers come from
// a table keyed by block number. A number with no entry is a test-writing
// mistake and fails loudly.
func blockAnswers(t *testing.T, answers map[uint64]func() ([]byte, error)) *EvmQueryExecutor {
	t.Helper()
	nop := zerolog.Nop()
	return &EvmQueryExecutor{
		logger: &nop,
		forwardSubrequestFn: func(_ context.Context, method string, params []interface{}) ([]byte, error) {
			require.Equal(t, "eth_getBlockByNumber", method)
			require.NotEmpty(t, params)
			ref, ok := params[0].(string)
			require.True(t, ok)
			var num uint64
			_, err := fmt.Sscanf(ref, "0x%x", &num)
			require.NoError(t, err)
			answer, ok := answers[num]
			require.True(t, ok, "the shim asked for block %#x, which the fixture does not describe", num)
			return answer()
		},
	}
}

func blockResult(t *testing.T, number uint64) func() ([]byte, error) {
	return func() ([]byte, error) {
		return mustMarshalJSON(t, makeProtoBlockResult(number, []interface{}{})), nil
	}
}

// TestFetchBlockViaForward_ReadsAnAbsentBlockAsAbsentRatherThanAsAFailure
// covers the two ways an upstream says it does not have a block. Both must
// yield no header and no error, so the walk continues.
func TestFetchBlockViaForward_ReadsAnAbsentBlockAsAbsentRatherThanAsAFailure(t *testing.T) {
	for name, answer := range map[string]func() ([]byte, error){
		"a null result": func() ([]byte, error) {
			return []byte("null"), nil
		},
		"a missing-data error": func() ([]byte, error) {
			return nil, common.NewErrEndpointMissingData(errors.New("pruned"), nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			qe := blockAnswers(t, map[uint64]func() ([]byte, error){7: answer})

			header, txs, err := qe.fetchBlockViaForward(context.Background(), 7, false)
			require.NoError(t, err, "an absent block is not a failed request")
			assert.Nil(t, header)
			assert.Nil(t, txs)
		})
	}
}

// TestFetchBlockViaForward_ReportsAFailureItCannotReadAsAbsence is the other
// half. A transport failure and an unreadable body are not absence, and
// swallowing either would drop blocks out of a page with nothing said.
//
// The three failures happen at three different steps, and each subtest names
// the step in the message rather than only asserting that some error came back.
// Decoding and conversion both fail on a body that is not a block, so a test
// that accepted any error would pass with either step deleted.
func TestFetchBlockViaForward_ReportsAFailureItCannotReadAsAbsence(t *testing.T) {
	for name, tc := range map[string]struct {
		answer  func() ([]byte, error)
		want    string
		wantNot string
	}{
		"the subrequest failed": {
			answer:  func() ([]byte, error) { return nil, errors.New("upstream refused") },
			want:    "upstream refused",
			wantNot: "JsonRpcBlock",
		},
		"the body does not decode into a block": {
			answer:  func() ([]byte, error) { return []byte(`"0x1"`), nil },
			want:    "JsonRpcBlock",
			wantNot: "strconv",
		},
		"the decoded block does not convert": {
			answer:  func() ([]byte, error) { return []byte(`{"number":"0x1"}`), nil },
			want:    "strconv",
			wantNot: "JsonRpcBlock",
		},
	} {
		t.Run(name, func(t *testing.T) {
			qe := blockAnswers(t, map[uint64]func() ([]byte, error){7: tc.answer})

			header, _, err := qe.fetchBlockViaForward(context.Background(), 7, false)
			require.Error(t, err)
			assert.Nil(t, header)
			assert.Contains(t, err.Error(), tc.want)
			assert.NotContains(t, err.Error(), tc.wantNot,
				"the error must name the step that failed, not a later one")
		})
	}
}

// TestShimQueryBlocks_LeavesAGapWhereTheUpstreamHasNoBlock is the page-level
// consequence. Block 2 is absent, so the page carries 1 and 3 and the walk runs
// to the end of the range rather than stopping at the gap.
func TestShimQueryBlocks_LeavesAGapWhereTheUpstreamHasNoBlock(t *testing.T) {
	qe := blockAnswers(t, map[uint64]func() ([]byte, error){
		1: blockResult(t, 1),
		2: func() ([]byte, error) { return []byte("null"), nil },
		3: blockResult(t, 3),
	})

	var page *evm.QueryBlocksResponse
	err := qe.shimQueryBlocks(context.Background(), &evm.QueryBlocksRequest{}, 1, 3, func(msg proto.Message) error {
		page = msg.(*evm.QueryBlocksResponse)
		return nil
	})

	require.NoError(t, err)
	require.NotNil(t, page)
	require.Len(t, page.Blocks, 2)
	assert.Equal(t, uint64(1), page.Blocks[0].Number)
	assert.Equal(t, uint64(3), page.Blocks[1].Number,
		"the walk must continue past the absent block, not stop on it")
	assert.Nil(t, page.CursorBlock, "the range was walked to its end, so there is no next page")
}

// TestShimQueryBlocks_StopsAtTheLimitAndNamesTheBlockToResumeFrom pins the
// ascending page boundary. The cursor carries the hashes a caller needs to spot
// a reorg between pages, so a cursor built from the wrong block is worse than
// no cursor at all.
func TestShimQueryBlocks_StopsAtTheLimitAndNamesTheBlockToResumeFrom(t *testing.T) {
	qe := blockAnswers(t, map[uint64]func() ([]byte, error){
		1: blockResult(t, 1),
		2: blockResult(t, 2),
	})

	var page *evm.QueryBlocksResponse
	err := qe.shimQueryBlocks(context.Background(), &evm.QueryBlocksRequest{
		Limit: uint32Ptr(2),
	}, 1, 5, func(msg proto.Message) error {
		page = msg.(*evm.QueryBlocksResponse)
		return nil
	})

	require.NoError(t, err)
	require.NotNil(t, page)
	require.Len(t, page.Blocks, 2)
	require.NotNil(t, page.CursorBlock, "blocks 3..5 are unread")
	assert.Equal(t, uint64(2), page.CursorBlock.Number)
	assert.Equal(t, page.Blocks[1].Hash, page.CursorBlock.Hash,
		"the cursor must name the block it stopped on, hash included")
}

// TestShimQueryBlocks_WalksADescendingRangeFromTheTop proves the order the
// client asked for is the order the upstream is walked in. A descending page
// that was assembled ascending would answer with the wrong end of the range.
func TestShimQueryBlocks_WalksADescendingRangeFromTheTop(t *testing.T) {
	qe := blockAnswers(t, map[uint64]func() ([]byte, error){
		4: blockResult(t, 4),
		5: blockResult(t, 5),
	})

	var page *evm.QueryBlocksResponse
	err := qe.shimQueryBlocks(context.Background(), &evm.QueryBlocksRequest{
		Limit: uint32Ptr(2),
		Order: evm.SortOrder_DESC.Enum(),
	}, 1, 5, func(msg proto.Message) error {
		page = msg.(*evm.QueryBlocksResponse)
		return nil
	})

	require.NoError(t, err)
	require.NotNil(t, page)
	require.Len(t, page.Blocks, 2)
	assert.Equal(t, uint64(5), page.Blocks[0].Number, "a descending page starts at the top of the range")
	assert.Equal(t, uint64(4), page.Blocks[1].Number)
	require.NotNil(t, page.CursorBlock, "blocks 1..3 are unread")
	assert.Equal(t, uint64(4), page.CursorBlock.Number)
}
