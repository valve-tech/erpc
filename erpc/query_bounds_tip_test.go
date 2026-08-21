package erpc

import (
	"context"
	"testing"

	bdsevm "github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// query_bounds_test.go covers the references a query can resolve with no chain
// state: "earliest", a hex number, and the two rejections. The other three —
// "latest", "finalized" and "safe" — read the network's served tip, so they
// need a network that has one. Those three decide how far a BDS range query
// walks. If "latest" answered 0, the client would get an empty page instead of
// the tip of the chain and nothing would report an error.
//
// The fixture seeds the heads directly, so the numbers below are the ones a
// live network would report, not values the test invented afterwards.

// tipExecutor builds a query executor over a network whose heads are known.
func tipExecutor(t *testing.T, ctx context.Context, latest, finalized int64) *EvmQueryExecutor {
	t.Helper()
	ntw, _ := setupQueryTestNetworkWithHeads(t, ctx, defaultQueryNetworkConfig(), latest, finalized)
	return NewEvmQueryExecutor(ntw, &log.Logger)
}

// TestResolveBlockTag_ReadsTheHeadsTheNetworkServes pins the three tags to the
// two heads. "latest" and "finalized" are distinct numbers here, so a
// resolution that read the wrong head would fail rather than coincide.
func TestResolveBlockTag_ReadsTheHeadsTheNetworkServes(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	qe := tipExecutor(t, ctx, 1000, 990)

	for _, tc := range []struct {
		ref  string
		want uint64
		why  string
	}{
		{"", 1000, "an omitted reference means the latest head"},
		{"latest", 1000, "latest is the served latest head"},
		{"finalized", 990, "finalized is the served finalized head, not the latest one"},
		{"safe", 990, "safe prefers the finalized head while the chain has one"},
	} {
		t.Run(tc.ref, func(t *testing.T) {
			got, err := qe.resolveBlockTag(ctx, tc.ref, false)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got, tc.why)
		})
	}
}

// TestResolveBlockTag_FallsBackToTheLatestHeadForSafeWithoutAFinalizedOne is
// the branch a chain that never serves `finalized` takes on every query. eRPC
// must answer with the latest head; answering 0 would collapse the range to
// genesis and return an empty page for a range the client can see is populated.
func TestResolveBlockTag_FallsBackToTheLatestHeadForSafeWithoutAFinalizedOne(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	qe := tipExecutor(t, ctx, 1000, 0)

	finalized, err := qe.resolveBlockTag(ctx, "finalized", true)
	require.NoError(t, err)
	require.Zero(t, finalized,
		"the fixture must leave the finalized head unknown, or the fallback below is not the branch under test")

	safe, err := qe.resolveBlockTag(ctx, "safe", true)
	require.NoError(t, err)
	assert.Equal(t, uint64(1000), safe,
		"safe must fall back to the latest head, not to block 0")
}

// TestResolveQueryBounds_TurnsTheTagPairIntoTheServedRange is the caller's view
// of the two resolutions above. "earliest" to "latest" is the widest range a
// client can ask for, and it has to come back as the whole chain.
func TestResolveQueryBounds_TurnsTheTagPairIntoTheServedRange(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	qe := tipExecutor(t, ctx, 1000, 990)

	from, to, err := qe.resolveQueryBounds(ctx, "earliest", "latest", bdsevm.SortOrder_ASC, nil)
	require.NoError(t, err)
	assert.Equal(t, uint64(0), from)
	assert.Equal(t, uint64(1000), to, "the upper bound is the served latest head")

	from, to, err = qe.resolveQueryBounds(ctx, "finalized", "latest", bdsevm.SortOrder_ASC, nil)
	require.NoError(t, err)
	assert.Equal(t, uint64(990), from)
	assert.Equal(t, uint64(1000), to)
}
