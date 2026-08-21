package erpc

import (
	"context"
	"testing"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Every BDS query begins by turning two block references and an optional cursor
// into a numeric range. That range decides which blocks the shim walks, so a
// mistake here is not visible in the answer: the client gets a well-formed page
// that describes a range nobody asked for, or re-reads a block it already had.
//
// The cases below use hex references only, so no upstream tip is consulted and
// the arithmetic is the only thing under test.

func newBoundsExecutor() *EvmQueryExecutor {
	nop := zerolog.Nop()
	return &EvmQueryExecutor{logger: &nop}
}

// TestResolveBlockTag_ReadsTheReferencesItCanAnswerWithoutAnUpstream pins the
// two references that need no chain state. "earliest" is block 0 by definition,
// and a hex number is itself.
func TestResolveBlockTag_ReadsTheReferencesItCanAnswerWithoutAnUpstream(t *testing.T) {
	qe := newBoundsExecutor()

	for _, tc := range []struct {
		ref  string
		want uint64
	}{
		{"earliest", 0},
		{"0x0", 0},
		{"0x1", 1},
		{"0x1f4", 500},
		{"0xffffffff", 4294967295},
	} {
		t.Run(tc.ref, func(t *testing.T) {
			got, err := qe.resolveBlockTag(context.Background(), tc.ref, false)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestResolveBlockTag_RefusesAReferenceItCannotServe covers the two rejections.
// "pending" is a real Ethereum tag that a range query cannot honour — the
// pending block has no fixed contents — and anything else is a typo the client
// must be told about rather than have silently read as block 0.
func TestResolveBlockTag_RefusesAReferenceItCannotServe(t *testing.T) {
	qe := newBoundsExecutor()

	t.Run("pending", func(t *testing.T) {
		_, err := qe.resolveBlockTag(context.Background(), "pending", false)
		require.Error(t, err)
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
		assert.Contains(t, status.Convert(err).Message(), "pending")
	})

	for _, ref := range []string{"0xzz", "latest-1", "12345", "0x"} {
		t.Run(ref, func(t *testing.T) {
			_, err := qe.resolveBlockTag(context.Background(), ref, false)
			require.Error(t, err, "an unreadable reference must not resolve to a block")
			assert.Equal(t, codes.InvalidArgument, status.Code(err))
			assert.Contains(t, status.Convert(err).Message(), ref,
				"the client has to learn which reference eRPC could not read")
		})
	}
}

// TestResolveQueryBounds_RefusesAnInvertedRange stops a from>to range before
// any cursor arithmetic runs. Without this the iterator would yield nothing and
// the client would get an empty page instead of being told the range is wrong.
func TestResolveQueryBounds_RefusesAnInvertedRange(t *testing.T) {
	qe := newBoundsExecutor()

	_, _, err := qe.resolveQueryBounds(context.Background(), "0x5", "0x1", evm.SortOrder_ASC, nil)
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Contains(t, status.Convert(err).Message(), "fromBlock")
}

// TestResolveQueryBounds_PassesAPlainRangeThrough is the control: with no
// cursor the bounds are exactly what the client asked for.
func TestResolveQueryBounds_PassesAPlainRangeThrough(t *testing.T) {
	qe := newBoundsExecutor()

	from, to, err := qe.resolveQueryBounds(context.Background(), "0x1", "0xa", evm.SortOrder_ASC, nil)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), from)
	assert.Equal(t, uint64(10), to)
}

// TestResolveQueryBounds_StartsAfterAnAscendingCursor proves the next page
// begins one block past the last one delivered. Off by one in either direction
// is silent: +0 re-sends a whole block, +2 drops one.
func TestResolveQueryBounds_StartsAfterAnAscendingCursor(t *testing.T) {
	qe := newBoundsExecutor()

	from, to, err := qe.resolveQueryBounds(
		context.Background(), "0x1", "0xa", evm.SortOrder_ASC, &evm.CursorBlock{Number: 4},
	)
	require.NoError(t, err)
	assert.Equal(t, uint64(5), from, "block 4 was already delivered")
	assert.Equal(t, uint64(10), to, "the upper bound is untouched by an ascending cursor")
}

// TestResolveQueryBounds_StopsBeforeADescendingCursor is the mirror image: a
// descending walk moves the ceiling down, not the floor.
func TestResolveQueryBounds_StopsBeforeADescendingCursor(t *testing.T) {
	qe := newBoundsExecutor()

	from, to, err := qe.resolveQueryBounds(
		context.Background(), "0x1", "0xa", evm.SortOrder_DESC, &evm.CursorBlock{Number: 4},
	)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), from, "the lower bound is untouched by a descending cursor")
	assert.Equal(t, uint64(3), to, "block 4 was already delivered")
}

// TestResolveQueryBounds_RefusesADescendingCursorAtGenesis guards the
// subtraction below it. A DESC cursor of 0 means the walk already reached
// genesis; decrementing would wrap to the maximum block and the shim would then
// walk the entire chain.
func TestResolveQueryBounds_RefusesADescendingCursorAtGenesis(t *testing.T) {
	qe := newBoundsExecutor()

	_, _, err := qe.resolveQueryBounds(
		context.Background(), "0x0", "0xa", evm.SortOrder_DESC, &evm.CursorBlock{Number: 0},
	)
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Contains(t, status.Convert(err).Message(), "DESC cursor")
}

// TestResolveQueryBounds_ReportsAnExhaustedRangeWithoutAnError covers the one
// case where from>to is a correct answer rather than a bad request: the cursor
// consumed the range. The shims depend on it — shimQueryLogs answers such a
// range with an empty page and never touches an upstream — so turning it into
// an error would make the last page of every walk fail.
func TestResolveQueryBounds_ReportsAnExhaustedRangeWithoutAnError(t *testing.T) {
	qe := newBoundsExecutor()

	from, to, err := qe.resolveQueryBounds(
		context.Background(), "0x1", "0x2", evm.SortOrder_ASC, &evm.CursorBlock{Number: 2},
	)
	require.NoError(t, err, "an exhausted walk is a completed walk, not a bad request")
	assert.Equal(t, uint64(3), from)
	assert.Equal(t, uint64(2), to)
	assert.Greater(t, from, to, "the caller reads the inversion as an empty page")
}
