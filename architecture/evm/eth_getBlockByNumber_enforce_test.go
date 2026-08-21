package evm

import (
	"context"
	"errors"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// enforceHighestBlock exists because an upstream can answer "latest" with a
// stale head. The hook re-forwards the request pinned to the tip the network
// already knows about, then keeps whichever answer carries the higher block.
// Both halves can go wrong quietly: a re-forward aimed at the wrong upstream
// loops forever, and a pick that prefers the stale side undoes the whole fix.

// blockRespFrom builds a response that looks like it came from `upstreamId`.
func blockRespFrom(t *testing.T, req *common.NormalizedRequest, raw string, up common.Upstream) *common.NormalizedResponse {
	t.Helper()
	jrr, err := common.NewJsonRpcResponseFromBytes(nil, []byte(raw), nil)
	require.NoError(t, err)
	r := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	if up != nil {
		r = r.SetUpstream(up)
	}
	return r
}

func latestReq(t *testing.T, tag string, enforce bool) *common.NormalizedRequest {
	t.Helper()
	r := common.NewNormalizedRequestFromJsonRpcRequest(
		common.NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{tag, false}))
	r.SetDirectives(&common.RequestDirectives{EnforceHighestBlock: enforce})
	return r
}

func TestBuildGetBlockByNumberRequest(t *testing.T) {
	t.Run("AcceptsTheKnownTags", func(t *testing.T) {
		for _, tag := range []string{"latest", "finalized", "safe", "pending", "earliest"} {
			jrq, err := BuildGetBlockByNumberRequest(tag, true)
			require.NoError(t, err, tag)
			assert.Equal(t, []interface{}{tag, true}, jrq.Params)
		}
	})

	t.Run("AcceptsAHexString", func(t *testing.T) {
		jrq, err := BuildGetBlockByNumberRequest("0x1b4", false)
		require.NoError(t, err)
		assert.Equal(t, []interface{}{"0x1b4", false}, jrq.Params)
	})

	t.Run("NormalizesNumbersToHex", func(t *testing.T) {
		// The tip arrives as an int64 from the state poller. Sending it as a
		// decimal would make every node reject the re-forward.
		for _, v := range []interface{}{int(436), int64(436)} {
			jrq, err := BuildGetBlockByNumberRequest(v, false)
			require.NoError(t, err, "%T", v)
			assert.Equal(t, "0x1b4", jrq.Params[0], "%T must render as hex", v)
		}
	})

	// float64 is the type every JSON-decoded number arrives as, so it has to
	// render as hex like the integer types do. common.NormalizeHex has no
	// float64 case, so the builder converts before calling it.
	t.Run("NormalizesAJsonDecodedNumberToHex", func(t *testing.T) {
		jrq, err := BuildGetBlockByNumberRequest(float64(436), false)
		require.NoError(t, err)
		assert.Equal(t, "0x1b4", jrq.Params[0])
	})

	t.Run("RejectsAFloatThatIsNotAWholeBlockNumber", func(t *testing.T) {
		// A fraction, a negative height and a value past float64's exact
		// integer range are all block references we cannot honour. Guessing a
		// nearby integer would send the node a different block than the caller
		// asked for.
		for _, v := range []float64{436.5, -1, 1 << 54} {
			_, err := BuildGetBlockByNumberRequest(v, false)
			require.Error(t, err, "%v", v)
			assert.Contains(t, err.Error(), "invalid block number or tag")
		}
	})

	t.Run("RejectsAnUnknownTag", func(t *testing.T) {
		// An unknown bare word must not reach the node as a block reference.
		_, err := BuildGetBlockByNumberRequest("newest", false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "newest")
	})

	t.Run("RejectsANonBlockType", func(t *testing.T) {
		_, err := BuildGetBlockByNumberRequest(map[string]string{"block": "1"}, false)
		require.Error(t, err)
	})
}

func TestPickHighestBlock(t *testing.T) {
	req := latestReq(t, "latest", false)

	t.Run("KeepsTheHigherBlock", func(t *testing.T) {
		x := blockRespFrom(t, req, `{"number":"0x64"}`, nil)
		y := blockRespFrom(t, req, `{"number":"0x63"}`, nil)

		got, err := pickHighestBlock(context.Background(), x, y, nil)
		require.NoError(t, err)
		assert.Same(t, x, got, "block 100 must beat block 99")
	})

	t.Run("KeepsTheOriginalWhenTheReForwardIsLower", func(t *testing.T) {
		// A corrupted tip can send the re-forward at a block that does not
		// exist yet, or a different upstream can answer with something older.
		x := blockRespFrom(t, req, `{"number":"0x63"}`, nil)
		y := blockRespFrom(t, req, `{"number":"0x64"}`, nil)

		got, err := pickHighestBlock(context.Background(), x, y, nil)
		require.NoError(t, err)
		assert.Same(t, y, got)
	})

	t.Run("EqualBlocksKeepTheOriginal", func(t *testing.T) {
		x := blockRespFrom(t, req, `{"number":"0x64"}`, nil)
		y := blockRespFrom(t, req, `{"number":"0x64"}`, nil)

		got, err := pickHighestBlock(context.Background(), x, y, nil)
		require.NoError(t, err)
		assert.Same(t, y, got, "a tie must not churn the response the caller already has")
	})

	t.Run("NullReForwardFallsBackToTheOriginal", func(t *testing.T) {
		y := blockRespFrom(t, req, `{"number":"0x64"}`, nil)

		got, err := pickHighestBlock(context.Background(), nil, y, errors.New("re-forward failed"))
		require.NoError(t, err, "a failed re-forward must not lose the answer we already had")
		assert.Same(t, y, got)
	})

	t.Run("EmptyishReForwardFallsBackToTheOriginal", func(t *testing.T) {
		x := blockRespFrom(t, req, `null`, nil)
		y := blockRespFrom(t, req, `{"number":"0x64"}`, nil)

		got, err := pickHighestBlock(context.Background(), x, y, nil)
		require.NoError(t, err)
		assert.Same(t, y, got)
	})

	t.Run("EmptyishOriginalYieldsToTheReForward", func(t *testing.T) {
		x := blockRespFrom(t, req, `{"number":"0x64"}`, nil)
		y := blockRespFrom(t, req, `null`, nil)

		got, err := pickHighestBlock(context.Background(), x, y, nil)
		require.NoError(t, err)
		assert.Same(t, x, got)
	})

	t.Run("BothEmptyWithAnErrorSurfacesTheError", func(t *testing.T) {
		x := blockRespFrom(t, req, `null`, nil)
		y := blockRespFrom(t, req, `null`, nil)
		boom := errors.New("re-forward failed")

		got, err := pickHighestBlock(context.Background(), x, y, boom)
		assert.Same(t, boom, err, "with nothing to keep, the caller must see why")
		assert.Nil(t, got)
	})

	t.Run("BothEmptyWithoutAnErrorKeepsTheOriginal", func(t *testing.T) {
		// No error means the re-forward genuinely answered null. There is
		// nothing to report, so the original null flows on to the null-block
		// enforcement further down the hook.
		x := blockRespFrom(t, req, `null`, nil)
		y := blockRespFrom(t, req, `null`, nil)

		got, err := pickHighestBlock(context.Background(), x, y, nil)
		require.NoError(t, err)
		assert.Same(t, y, got)
	})

	t.Run("UnparsableBlockNumberOnEitherSideYieldsToTheOther", func(t *testing.T) {
		good := blockRespFrom(t, req, `{"number":"0x64"}`, nil)
		bad := blockRespFrom(t, req, `{"number":"not-a-number"}`, nil)
		got, err := pickHighestBlock(context.Background(), bad, good, nil)
		require.NoError(t, err)
		assert.Same(t, good, got)

		good2 := blockRespFrom(t, req, `{"number":"0x64"}`, nil)
		bad2 := blockRespFrom(t, req, `{"number":"not-a-number"}`, nil)
		got, err = pickHighestBlock(context.Background(), good2, bad2, nil)
		require.NoError(t, err)
		assert.Same(t, good2, got)
	})

	t.Run("MissingNumberFieldYieldsToTheOther", func(t *testing.T) {
		good := blockRespFrom(t, req, `{"number":"0x64"}`, nil)
		noNumber := blockRespFrom(t, req, `{"hash":"0xabc"}`, nil)

		got, err := pickHighestBlock(context.Background(), noNumber, good, nil)
		require.NoError(t, err)
		assert.Same(t, good, got)
	})
}

func TestEnforceHighestBlock(t *testing.T) {
	ctx := context.Background()

	t.Run("PassesTheErrorThroughUntouched", func(t *testing.T) {
		n := newForwardingNetwork(1)
		n.highestLatest = 1000
		boom := errors.New("upstream failed")

		_, err := enforceHighestBlock(ctx, n, latestReq(t, "latest", true), nil, boom)

		assert.Same(t, boom, err)
		assert.Empty(t, n.allCalls(), "a failed forward must not trigger a re-forward")
	})

	t.Run("DirectiveOffSkipsEnforcement", func(t *testing.T) {
		n := newForwardingNetwork(1)
		n.highestLatest = 1000
		rq := latestReq(t, "latest", false)
		rs := blockRespFrom(t, rq, `{"number":"0x1"}`, nil)

		got, err := enforceHighestBlock(ctx, n, rq, rs, nil)

		require.NoError(t, err)
		assert.Same(t, rs, got, "a stale head is only corrected when the caller asks")
		assert.Empty(t, n.allCalls())
	})

	t.Run("NonTagParamSkipsEnforcement", func(t *testing.T) {
		// A pinned block number is what it is; there is no "higher" answer.
		n := newForwardingNetwork(1)
		n.highestLatest = 1000
		rq := latestReq(t, "0x5", true)
		rs := blockRespFrom(t, rq, `{"number":"0x5"}`, nil)

		got, err := enforceHighestBlock(ctx, n, rq, rs, nil)

		require.NoError(t, err)
		assert.Same(t, rs, got)
		assert.Empty(t, n.allCalls())
	})

	t.Run("UpToDateResponseIsNotReForwarded", func(t *testing.T) {
		n := newForwardingNetwork(1)
		n.highestLatest = 100
		rq := latestReq(t, "latest", true)
		rs := blockRespFrom(t, rq, `{"number":"0x64"}`, nil)

		got, err := enforceHighestBlock(ctx, n, rq, rs, nil)

		require.NoError(t, err)
		assert.Same(t, rs, got)
		assert.Empty(t, n.allCalls(), "an answer already at the tip needs no correction")
	})

	t.Run("StaleLatestIsReForwardedAtTheKnownTipAwayFromTheStaleUpstream", func(t *testing.T) {
		up := newForwardingUpstream(1)
		n := newForwardingNetwork(1)
		n.highestLatest = 200
		var seen *common.RequestDirectives
		n.on("eth_getBlockByNumber", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			seen = req.Directives()
			return jsonResult(req, `{"number":"0xc8"}`)
		})

		rq := latestReq(t, "latest", true)
		rs := blockRespFrom(t, rq, `{"number":"0x64"}`, up) // node is 100 blocks behind

		got, err := enforceHighestBlock(ctx, n, rq, rs, nil)

		require.NoError(t, err)
		jrr, err := got.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), "0xc8", "the caller must get the tip block")

		// Discriminating: the correction only works if the re-forward asks for
		// the KNOWN TIP, skips the cache, and steers away from the upstream
		// that just answered stale. A re-forward that repeats "latest" at the
		// same upstream would loop and still return block 100.
		require.Len(t, n.methodCalls("eth_getBlockByNumber"), 1)
		assert.Contains(t, n.methodCalls("eth_getBlockByNumber")[0], `"0xc8"`,
			"the re-forward must pin the known tip, not repeat the tag")
		require.NotNil(t, seen)
		assert.Equal(t, "true", seen.SkipCacheRead, "a cached stale block would defeat the correction")
		assert.Equal(t, "!fwd-ups", seen.UseUpstream, "the stale upstream must be excluded")
	})

	t.Run("ReForwardCarriesTheIncludeTransactionsFlagAndTheRequestId", func(t *testing.T) {
		up := newForwardingUpstream(1)
		n := newForwardingNetwork(1)
		n.highestLatest = 200
		n.on("eth_getBlockByNumber", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return jsonResult(req, `{"number":"0xc8"}`)
		})

		jrq := common.NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{"latest", true})
		require.NoError(t, jrq.SetID(77))
		rq := common.NewNormalizedRequestFromJsonRpcRequest(jrq)
		rq.SetDirectives(&common.RequestDirectives{EnforceHighestBlock: true})
		rs := blockRespFrom(t, rq, `{"number":"0x64"}`, up)

		_, err := enforceHighestBlock(ctx, n, rq, rs, nil)
		require.NoError(t, err)

		body := n.methodCalls("eth_getBlockByNumber")[0]
		assert.Contains(t, body, "true", "includeTransactions must survive the re-forward")
		assert.Contains(t, body, `"id":77`, "the re-forward must keep the caller's id")
	})

	t.Run("StaleFinalizedIsReForwardedAtTheFinalizedTip", func(t *testing.T) {
		up := newForwardingUpstream(1)
		n := newForwardingNetwork(1)
		n.highestLatest = 9_999 // must NOT be used for the finalized tag
		n.highestFinalized = 300
		n.on("eth_getBlockByNumber", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return jsonResult(req, `{"number":"0x12c"}`)
		})

		rq := latestReq(t, "finalized", true)
		rs := blockRespFrom(t, rq, `{"number":"0x64"}`, up)

		got, err := enforceHighestBlock(ctx, n, rq, rs, nil)

		require.NoError(t, err)
		jrr, err := got.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), "0x12c")
		// Discriminating: the finalized branch must read the FINALIZED tip.
		// Using the latest tip would re-forward at 9999 and hand back an
		// unfinalized block under a finalized tag.
		body := n.methodCalls("eth_getBlockByNumber")[0]
		assert.Contains(t, body, `"0x12c"`)
		assert.NotContains(t, body, `"0x270f"`)
	})

	t.Run("ReForwardFailureKeepsTheStaleAnswer", func(t *testing.T) {
		// Better a slightly stale block than an error, since the caller did get
		// a valid answer from a real node.
		up := newForwardingUpstream(1)
		n := newForwardingNetwork(1)
		n.highestLatest = 200
		n.on("eth_getBlockByNumber", func(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return nil, errors.New("no upstream has block 200 yet")
		})

		rq := latestReq(t, "latest", true)
		rs := blockRespFrom(t, rq, `{"number":"0x64"}`, up)

		got, err := enforceHighestBlock(ctx, n, rq, rs, nil)

		require.NoError(t, err)
		assert.Same(t, rs, got)
	})

	t.Run("JsonRpcErrorResponseStillAllowsTheSameUpstream", func(t *testing.T) {
		// respBlockNumber == 0 means the response was an error, not a stale
		// block. The current upstream may have hit a blip, so the re-forward
		// must NOT exclude it.
		up := newForwardingUpstream(1)
		n := newForwardingNetwork(1)
		n.highestLatest = 200
		n.on("eth_getBlockByNumber", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return jsonResult(req, `{"number":"0xc8"}`)
		})

		var seen *common.RequestDirectives
		n.on("eth_getBlockByNumber", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			seen = req.Directives()
			return jsonResult(req, `{"number":"0xc8"}`)
		})

		rq := latestReq(t, "latest", true)
		rs := blockRespFrom(t, rq, `{"number":"0x0"}`, up)

		_, err := enforceHighestBlock(ctx, n, rq, rs, nil)
		require.NoError(t, err)

		require.Len(t, n.methodCalls("eth_getBlockByNumber"), 1)
		require.NotNil(t, seen)
		assert.Empty(t, seen.UseUpstream,
			"an error response is not proof the upstream is behind, so it must stay eligible")
	})
}
