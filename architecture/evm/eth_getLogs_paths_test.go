package evm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover the eth_getLogs guards, the splitter and the merged-writer
// paths that the existing suite leaves dark: the malformed-filter fallthroughs,
// the split fan-out over a network that really answers, and the writer's
// behaviour when the sink fails mid-stream.

// --- resolveBlockTagForGetLogs / getLogsConcreteRangeSize ---

func TestResolveBlockTagForGetLogs_RefusesWhatItCannotRead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"EmptyValue", ""},
		{"HexWithNonHexDigits", "0xzz"},
		{"UnresolvableTag", "pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hex, num := resolveBlockTagForGetLogs(context.Background(), nil, tc.value)
			assert.Empty(t, hex)
			assert.Equal(t, int64(0), num, "an unreadable bound must not resolve to block zero silently")
		})
	}
}

func TestGetLogsConcreteRangeSize_RefusesMalformedFilters(t *testing.T) {
	ctx := context.Background()

	t.Run("NilRequest", func(t *testing.T) {
		_, ok := getLogsConcreteRangeSize(ctx, nil)
		assert.False(t, ok)
	})

	for _, tc := range []struct {
		name   string
		params []interface{}
	}{
		{"NoParams", []interface{}{}},
		{"FilterIsNotAnObject", []interface{}{"0x1"}},
		{"FromBlockIsNotHex", []interface{}{map[string]interface{}{"fromBlock": "0xzz", "toBlock": "0x5"}}},
		{"ToBlockIsNotHex", []interface{}{map[string]interface{}{"fromBlock": "0x1", "toBlock": "0xzz"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jrq := common.NewJsonRpcRequest("eth_getLogs", tc.params)
			size, ok := getLogsConcreteRangeSize(ctx, jrq)
			assert.False(t, ok, "a range that cannot be read must not be attributed a size")
			assert.Equal(t, float64(0), size)
		})
	}
}

// --- networkPreForward_eth_getLogs: the fallthroughs ---

func TestNetworkPreForward_PassesThroughWhatItCannotValidate(t *testing.T) {
	ctx := context.Background()

	t.Run("NilRequest", func(t *testing.T) {
		handled, resp, err := networkPreForward_eth_getLogs(ctx, new(mockNetwork), nil, nil)
		require.NoError(t, err)
		assert.False(t, handled)
		assert.Nil(t, resp)
	})

	t.Run("NilNetwork", func(t *testing.T) {
		r := createTestRequest(map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x5"})
		handled, _, err := networkPreForward_eth_getLogs(ctx, nil, nil, r)
		require.NoError(t, err)
		assert.False(t, handled)
	})

	t.Run("ASubRequestIsNeverSplitAgain", func(t *testing.T) {
		// Re-entrancy guard: a sub-request already carries a parent id, and a
		// second split would fan out combinatorially.
		n := new(mockNetwork)
		r := createTestRequest(map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x5"})
		r.SetParentRequestId("parent-1")

		handled, _, err := networkPreForward_eth_getLogs(ctx, n, nil, r)
		require.NoError(t, err)
		assert.False(t, handled)
		// Discriminating: the network config is never even consulted.
		n.AssertNotCalled(t, "Config")
	})

	t.Run("NoParams", func(t *testing.T) {
		r := common.NewNormalizedRequestFromJsonRpcRequest(common.NewJsonRpcRequest("eth_getLogs", []interface{}{}))
		handled, _, err := networkPreForward_eth_getLogs(ctx, new(mockNetwork), nil, r)
		require.NoError(t, err)
		assert.False(t, handled)
	})

	t.Run("FilterIsNotAnObject", func(t *testing.T) {
		r := common.NewNormalizedRequestFromJsonRpcRequest(common.NewJsonRpcRequest("eth_getLogs", []interface{}{"0x1"}))
		handled, _, err := networkPreForward_eth_getLogs(ctx, new(mockNetwork), nil, r)
		require.NoError(t, err)
		assert.False(t, handled)
	})

	t.Run("NonEvmNetworkConfig", func(t *testing.T) {
		n := new(mockNetwork)
		n.On("Config").Return(&common.NetworkConfig{})
		r := createTestRequest(map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x5"})

		handled, _, err := networkPreForward_eth_getLogs(ctx, n, nil, r)
		require.NoError(t, err)
		assert.False(t, handled, "without EVM network config there is no limit to enforce")
	})
}

// splitCapturingNetwork answers every eth_getLogs sub-request with one log
// tagged by its range, and records the directives each sub-request carried.
type splitCapturingNetwork struct {
	*forwardingNetwork
	mu             sync.Mutex
	skipCacheReads []string
	ranges         []string
}

func newSplitCapturingNetwork(t *testing.T, cfg *common.EvmNetworkConfig) *splitCapturingNetwork {
	t.Helper()
	n := &splitCapturingNetwork{forwardingNetwork: newForwardingNetwork(123)}
	n.cfg = &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: cfg}
	n.on("eth_getLogs", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		jrq, err := req.JsonRpcRequest()
		require.NoError(t, err)
		filter, ok := jrq.Params[0].(map[string]interface{})
		require.True(t, ok)
		n.mu.Lock()
		n.ranges = append(n.ranges, fmt.Sprintf("%v-%v", filter["fromBlock"], filter["toBlock"]))
		if d := req.Directives(); d != nil {
			n.skipCacheReads = append(n.skipCacheReads, d.SkipCacheRead)
		}
		n.mu.Unlock()
		return jsonResult(req, fmt.Sprintf(`[{"blockNumber":%q}]`, filter["fromBlock"]))
	})
	return n
}

func (n *splitCapturingNetwork) observed() ([]string, []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.ranges...), append([]string(nil), n.skipCacheReads...)
}

func upstreamWithSplitThreshold(threshold int64) *forwardingUpstream {
	u := newForwardingUpstream(123)
	u.cfg.Evm.GetLogsAutoSplittingRangeThreshold = threshold
	return u
}

func TestNetworkPreForward_SplitsIntoContiguousRangesAndCarriesTheDirective(t *testing.T) {
	n := newSplitCapturingNetwork(t, &common.EvmNetworkConfig{ChainId: 123, GetLogsSplitConcurrency: 1})
	r := createTestRequest(map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x6"})
	r.SetDirectives(&common.RequestDirectives{SkipCacheRead: "redis-*"})

	handled, resp, err := networkPreForward_eth_getLogs(
		context.Background(), n, []common.Upstream{upstreamWithSplitThreshold(2)}, r)

	require.NoError(t, err)
	require.True(t, handled, "a range past the threshold must be answered by the split, not forwarded whole")
	require.NotNil(t, resp)

	ranges, skips := n.observed()
	// Discriminating: the sub-ranges must tile [1,6] exactly, with no gap and no
	// overlap. Asserting the count alone would pass for wrong boundaries.
	assert.Equal(t, []string{"0x1-0x2", "0x3-0x4", "0x5-0x6"}, ranges)
	assert.Equal(t, []string{"redis-*", "redis-*", "redis-*"}, skips,
		"the caller's skip-cache-read directive must reach every sub-request")
}

func TestNetworkPreForward_AFailingSubRequestFailsTheWholeSplit(t *testing.T) {
	n := &splitCapturingNetwork{forwardingNetwork: newForwardingNetwork(123)}
	n.cfg = &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	n.on("eth_getLogs", func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return nil, errors.New("upstream exhausted")
	})
	r := createTestRequest(map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x6"})

	handled, resp, err := networkPreForward_eth_getLogs(
		context.Background(), n, []common.Upstream{upstreamWithSplitThreshold(2)}, r)

	require.Error(t, err, "a partial answer must never be presented as the whole range")
	assert.True(t, handled)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "upstream exhausted")
}

func TestNetworkPreForward_UpstreamsWithoutAThresholdAreIgnored(t *testing.T) {
	n := newSplitCapturingNetwork(t, &common.EvmNetworkConfig{ChainId: 123, GetLogsSplitConcurrency: 1})
	withThreshold := upstreamWithSplitThreshold(2)
	noEvmConfig := newForwardingUpstream(123)
	noEvmConfig.cfg.Evm = nil
	r := createTestRequest(map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x4"})

	handled, _, err := networkPreForward_eth_getLogs(
		context.Background(), n, []common.Upstream{nil, noEvmConfig, withThreshold}, r)

	require.NoError(t, err)
	require.True(t, handled)
	ranges, _ := n.observed()
	// Discriminating: a nil entry or a non-EVM upstream must neither panic nor
	// drag the effective threshold to zero (which would disable splitting).
	assert.Equal(t, []string{"0x1-0x2", "0x3-0x4"}, ranges)
}

// --- upstreamPreForward_eth_getLogs ---

func getLogsIntegrityNetwork() *mockNetwork {
	n := new(mockNetwork)
	n.On("Id").Return("evm:123").Maybe()
	n.On("Config").Return(&common.NetworkConfig{
		Evm: &common.EvmNetworkConfig{
			Integrity: &common.EvmIntegrityConfig{EnforceGetLogsBlockRange: util.BoolPtr(true)},
		},
	}).Maybe()
	return n
}

func TestUpstreamPreForward_ANonEvmUpstreamIsPassedThrough(t *testing.T) {
	n := &testNetwork{cfg: &common.NetworkConfig{
		Evm: &common.EvmNetworkConfig{
			Integrity: &common.EvmIntegrityConfig{EnforceGetLogsBlockRange: util.BoolPtr(true)},
		},
	}}
	u := &fakePollerUpstream{cfg: &common.UpstreamConfig{Id: "non-evm"}, logger: zerolog.Nop()}
	r := createTestRequest(map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x5"})

	handled, resp, err := upstreamPreForward_eth_getLogs(context.Background(), n, u, r)
	require.NoError(t, err)
	assert.False(t, handled, "a non-EVM upstream has no block bounds to check against")
	assert.Nil(t, resp)
}

func TestUpstreamPreForward_RangeCheckIsOffWhenIntegrityIsDisabled(t *testing.T) {
	n := new(mockNetwork)
	n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{}})
	u := newForwardingUpstream(123)
	// fromBlock > toBlock: an inverted range the enabled hook would reject.
	r := createTestRequest(map[string]interface{}{"fromBlock": "0x5", "toBlock": "0x1"})

	handled, _, err := upstreamPreForward_eth_getLogs(context.Background(), n, u, r)
	require.NoError(t, err, "with the check disabled the upstream decides, not eRPC")
	assert.False(t, handled)
}

func TestUpstreamPreForward_RejectsAnInvertedRange(t *testing.T) {
	n := getLogsIntegrityNetwork()
	u := newForwardingUpstream(123)
	r := createTestRequest(map[string]interface{}{"fromBlock": "0x5", "toBlock": "0x1"})

	handled, resp, err := upstreamPreForward_eth_getLogs(context.Background(), n, u, r)
	require.Error(t, err)
	assert.True(t, handled)
	assert.Nil(t, resp)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeInvalidRequest))
	assert.Contains(t, err.Error(), "5", "the offending bounds must appear in the message")
}

func TestUpstreamPreForward_PassesThroughAMalformedFilter(t *testing.T) {
	n := getLogsIntegrityNetwork()
	u := newForwardingUpstream(123)

	for _, tc := range []struct {
		name   string
		params []interface{}
	}{
		{"NoParams", []interface{}{}},
		{"FilterIsNotAnObject", []interface{}{"0x1"}},
		// fromBlock resolves and toBlock does not. Discriminating: a hook that
		// skipped the "unresolved" guard would compare 5 against 0 and reject the
		// request as inverted, so only the guard explains the pass-through.
		{"OneUnresolvableBound", []interface{}{map[string]interface{}{"fromBlock": "0x5", "toBlock": "pending"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := common.NewNormalizedRequestFromJsonRpcRequest(common.NewJsonRpcRequest("eth_getLogs", tc.params))
			handled, _, err := upstreamPreForward_eth_getLogs(context.Background(), n, u, r)
			require.NoError(t, err)
			assert.False(t, handled)
		})
	}
}

// --- networkPostForward_eth_getLogs ---

func splitOnErrorNetworkConfig() *common.NetworkConfig {
	return &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId:                 123,
			GetLogsSplitOnError:     util.BoolPtr(true),
			GetLogsSplitConcurrency: 1,
		},
	}
}

func TestNetworkPostForward_NeverSplitsASubRequest(t *testing.T) {
	n := newSplitCapturingNetwork(t, splitOnErrorNetworkConfig().Evm)
	r := createTestRequest(map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x8"})
	r.SetParentRequestId("parent-1")
	tooLarge := common.NewErrEndpointRequestTooLarge(errors.New("too many logs"), common.EvmBlockRangeTooLarge)

	resp, err := networkPostForward_eth_getLogs(context.Background(), n, r, nil, tooLarge)

	require.Error(t, err, "the parent's own split already owns this range")
	assert.Nil(t, resp)
	ranges, _ := n.observed()
	assert.Empty(t, ranges, "a sub-request must never fan out again")
}

func TestNetworkPostForward_SplitsOnANormalizedLargeRangeCode(t *testing.T) {
	n := newSplitCapturingNetwork(t, splitOnErrorNetworkConfig().Evm)
	r := createTestRequest(map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x2"})
	r.SetDirectives(&common.RequestDirectives{SkipCacheRead: "*"})
	// Not an ErrEndpointRequestTooLarge: a raw vendor error that only the
	// NORMALIZED code marks as an over-large range.
	jre := common.NewErrJsonRpcExceptionInternal(0, common.JsonRpcErrorEvmLargeRange, "query returned more than 10000 results", nil, nil)

	resp, err := networkPostForward_eth_getLogs(context.Background(), n, r, nil, jre)

	require.NoError(t, err)
	require.NotNil(t, resp)
	ranges, skips := n.observed()
	assert.Equal(t, []string{"0x1-0x1", "0x2-0x2"}, ranges)
	assert.Equal(t, []string{"*", "*"}, skips)
}

func TestNetworkPostForward_AnUnsplittableRequestKeepsTheOriginalError(t *testing.T) {
	n := newSplitCapturingNetwork(t, splitOnErrorNetworkConfig().Evm)
	// One block, one address, one topic: nothing left to divide.
	r := createTestRequest(map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x1", "address": "0xabc"})
	tooLarge := common.NewErrEndpointRequestTooLarge(errors.New("too many logs"), common.EvmBlockRangeTooLarge)

	resp, err := networkPostForward_eth_getLogs(context.Background(), n, r, nil, tooLarge)

	require.Error(t, err)
	assert.Same(t, tooLarge, err, "an unsplittable request must surface the upstream's own error")
	assert.Nil(t, resp)
	ranges, _ := n.observed()
	assert.Empty(t, ranges)
}

func TestNetworkPostForward_AFailingSubRequestKeepsTheOriginalError(t *testing.T) {
	n := &splitCapturingNetwork{forwardingNetwork: newForwardingNetwork(123)}
	n.cfg = splitOnErrorNetworkConfig()
	n.on("eth_getLogs", func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return nil, errors.New("still too large")
	})
	r := createTestRequest(map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x4"})
	tooLarge := common.NewErrEndpointRequestTooLarge(errors.New("too many logs"), common.EvmBlockRangeTooLarge)

	resp, err := networkPostForward_eth_getLogs(context.Background(), n, r, nil, tooLarge)

	require.Error(t, err)
	assert.Same(t, tooLarge, err, "a failed split must not replace the cause with its own error")
	assert.Nil(t, resp)
}

// --- splitEthGetLogsRequest / extractBlockRange ---

func TestSplitEthGetLogsRequest_RefusesAMalformedRequest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params []interface{}
	}{
		{"NoParams", []interface{}{}},
		{"FilterIsNotAnObject", []interface{}{"0x1"}},
		{"FromBlockMissing", []interface{}{map[string]interface{}{"toBlock": "0x5"}}},
		{"FromBlockIsATag", []interface{}{map[string]interface{}{"fromBlock": "latest", "toBlock": "0x5"}}},
		{"ToBlockMissing", []interface{}{map[string]interface{}{"fromBlock": "0x1"}}},
		{"ToBlockIsATag", []interface{}{map[string]interface{}{"fromBlock": "0x1", "toBlock": "latest"}}},
		{"FromBlockOverflowsInt64", []interface{}{map[string]interface{}{"fromBlock": "0xffffffffffffffffff", "toBlock": "0x5"}}},
		{"ToBlockOverflowsInt64", []interface{}{map[string]interface{}{"fromBlock": "0x1", "toBlock": "0xffffffffffffffffff"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := common.NewNormalizedRequestFromJsonRpcRequest(common.NewJsonRpcRequest("eth_getLogs", tc.params))
			subs, err := splitEthGetLogsRequest(r)
			require.Error(t, err)
			assert.Empty(t, subs)
		})
	}
}

func TestSplitEthGetLogsRequest_RecordsTheSplitDimensionAgainstItsNetwork(t *testing.T) {
	// A request whose network is known drives the forced-split counter; the
	// split itself must be identical either way.
	for _, tc := range []struct {
		name   string
		filter map[string]interface{}
		want   []ethGetLogsSubRequest
	}{
		{
			"ByBlockRange",
			map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x4"},
			[]ethGetLogsSubRequest{{fromBlock: 1, toBlock: 2}, {fromBlock: 3, toBlock: 4}},
		},
		{
			"ByAddressWhenTheRangeIsOneBlock",
			map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x1", "address": []interface{}{"0xa", "0xb"}},
			[]ethGetLogsSubRequest{
				{fromBlock: 1, toBlock: 1, address: []interface{}{"0xa"}},
				{fromBlock: 1, toBlock: 1, address: []interface{}{"0xb"}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := createTestRequest(tc.filter)
			r.SetNetwork(&testNetwork{})
			subs, err := splitEthGetLogsRequest(r)
			require.NoError(t, err)
			require.Len(t, subs, 2)
			assert.Equal(t, tc.want[0].fromBlock, subs[0].fromBlock)
			assert.Equal(t, tc.want[0].toBlock, subs[0].toBlock)
			assert.Equal(t, tc.want[1].fromBlock, subs[1].fromBlock)
			assert.Equal(t, tc.want[1].toBlock, subs[1].toBlock)
			if tc.want[0].address != nil {
				assert.Equal(t, tc.want[0].address, subs[0].address)
				assert.Equal(t, tc.want[1].address, subs[1].address)
			}
		})
	}
}

func TestSplitEthGetLogsRequest_SplitsTopicZeroWithoutDisturbingLaterPositions(t *testing.T) {
	r := createTestRequest(map[string]interface{}{
		"fromBlock": "0x1",
		"toBlock":   "0x1",
		"topics":    []interface{}{[]interface{}{"0xa", "0xb"}, "0xkeep"},
	})
	r.SetNetwork(&testNetwork{})

	subs, err := splitEthGetLogsRequest(r)
	require.NoError(t, err)
	require.Len(t, subs, 2)

	left := subs[0].topics.([]interface{})
	right := subs[1].topics.([]interface{})
	assert.Equal(t, []interface{}{"0xa"}, left[0])
	assert.Equal(t, []interface{}{"0xb"}, right[0])
	// Discriminating: topic1 is shared state. A splitter that reused one slice
	// would show the same backing array in both halves.
	assert.Equal(t, "0xkeep", left[1])
	assert.Equal(t, "0xkeep", right[1])
}

// --- GetLogsMultiResponseWriter ---

// shortWriter accepts okBytes bytes and then fails, so each write site in the
// merged writer can be reached in turn.
type shortWriter struct {
	okBytes int
	written int
}

func (w *shortWriter) Write(p []byte) (int, error) {
	if w.written+len(p) > w.okBytes {
		n := w.okBytes - w.written
		if n < 0 {
			n = 0
		}
		w.written += n
		return n, errors.New("sink closed")
	}
	w.written += len(p)
	return len(p), nil
}

func logsResponse(t *testing.T, raw string) *common.JsonRpcResponse {
	t.Helper()
	jrr, err := common.NewJsonRpcResponseFromBytes(nil, []byte(raw), nil)
	require.NoError(t, err)
	return jrr
}

func TestGetLogsMultiResponseWriter_ASinkFailureIsReportedAtEveryWriteSite(t *testing.T) {
	writer := NewGetLogsMultiResponseWriter([]*common.JsonRpcResponse{
		logsResponse(t, `[{"logIndex":"0x1"}]`),
		logsResponse(t, `[{"logIndex":"0x2"}]`),
	})

	// A full write, to learn how many bytes the whole document needs.
	var full bytes.Buffer
	total, err := writer.WriteTo(&full, false)
	require.NoError(t, err)
	require.Greater(t, total, int64(4))

	for _, okBytes := range []int{0, 1, int(total) - 2, int(total) - 1} {
		t.Run(fmt.Sprintf("FailsAfter%dBytes", okBytes), func(t *testing.T) {
			w := &shortWriter{okBytes: okBytes}
			n, err := writer.WriteTo(w, false)
			require.Error(t, err, "a truncated write must be reported, never silently dropped")
			assert.LessOrEqual(t, n, total)
		})
	}
}

func TestGetLogsMultiResponseWriter_TrimSidesOmitsTheOuterBrackets(t *testing.T) {
	writer := NewGetLogsMultiResponseWriter([]*common.JsonRpcResponse{
		logsResponse(t, `[{"logIndex":"0x1"}]`),
	})

	var trimmed bytes.Buffer
	_, err := writer.WriteTo(&trimmed, true)
	require.NoError(t, err)
	assert.False(t, strings.HasPrefix(trimmed.String(), "["))
	assert.False(t, strings.HasSuffix(trimmed.String(), "]"))
	assert.Contains(t, trimmed.String(), "0x1")
}

func TestGetLogsMultiResponseWriter_EmptinessIgnoresNilEntries(t *testing.T) {
	assert.True(t, NewGetLogsMultiResponseWriter(nil).IsResultEmptyish(),
		"no sub-responses at all is empty")
	assert.True(t, NewGetLogsMultiResponseWriter([]*common.JsonRpcResponse{nil, nil}).IsResultEmptyish(),
		"a nil entry carries no logs")
	assert.False(t, NewGetLogsMultiResponseWriter([]*common.JsonRpcResponse{
		nil, logsResponse(t, `[{"logIndex":"0x1"}]`),
	}).IsResultEmptyish(),
		"one populated entry beside a nil one is not empty")
}
