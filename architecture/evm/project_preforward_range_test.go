package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// The project pre-forward hooks for eth_getLogs and trace_filter do one job:
// they observe the block-range size an operator asked for, before the cache and
// before upstream selection. Nothing downstream reads their verdict — they
// always pass the request through. So the behaviour worth pinning is the
// histogram: it records the resolved range exactly once, and it records nothing
// when the range cannot be read.

// histSamples reads the sample count and sum of every series of `lh` whose
// `project` label equals `project`. Each test uses its own project id, so the
// series starts empty and the numbers below are absolute.
func histSamples(t *testing.T, lh *telemetry.LabeledHistogram, project string) (uint64, float64) {
	t.Helper()
	ch := make(chan prometheus.Metric, 64)
	go func() {
		lh.Collect(ch)
		close(ch)
	}()
	var count uint64
	var sum float64
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		for _, l := range pb.GetLabel() {
			if l.GetName() == "project" && l.GetValue() == project {
				count += pb.GetHistogram().GetSampleCount()
				sum += pb.GetHistogram().GetSampleSum()
				break
			}
		}
	}
	return count, sum
}

// rangeNetwork builds a network that reports `project` as its project id and
// `head` as its highest known latest block. A head of 0 leaves "latest"
// unresolvable, which is what a cold state poller looks like.
func rangeNetwork(project string, head int64) *mockNetwork {
	n := new(mockNetwork)
	n.On("ProjectId").Return(project).Maybe()
	n.On("Id").Return("evm:123").Maybe()
	n.On("EvmHighestLatestBlockNumber", mock.Anything).Return(head).Maybe()
	n.On("EvmHighestFinalizedBlockNumber", mock.Anything).Return(head).Maybe()
	return n
}

func TestProjectPreForward_EthGetLogs_RecordsTheRangeItCanRead(t *testing.T) {
	ctx := context.Background()

	t.Run("ConcreteHexBounds", func(t *testing.T) {
		const project = "range-getlogs-hex"
		n := rangeNetwork(project, 0)
		nq := common.NewNormalizedRequest([]byte(
			`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x10","toBlock":"0x14"}]}`))

		handled, resp, err := HandleProjectPreForward(ctx, n, nq)
		require.NoError(t, err)
		assert.False(t, handled, "the range hook never answers the request itself")
		assert.Nil(t, resp)

		count, sum := histSamples(t, telemetry.MetricNetworkEvmGetLogsRangeRequested, project)
		assert.Equal(t, uint64(1), count, "one request must produce one observation")
		assert.Equal(t, float64(5), sum, "0x10..0x14 inclusive is 5 blocks")
	})

	t.Run("LatestTagResolvesAgainstTheKnownHead", func(t *testing.T) {
		const project = "range-getlogs-latest"
		n := rangeNetwork(project, 120)
		nq := common.NewNormalizedRequest([]byte(
			`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x64","toBlock":"latest"}]}`))

		handled, _, err := HandleProjectPreForward(ctx, n, nq)
		require.NoError(t, err)
		assert.False(t, handled)

		count, sum := histSamples(t, telemetry.MetricNetworkEvmGetLogsRangeRequested, project)
		assert.Equal(t, uint64(1), count)
		assert.Equal(t, float64(21), sum, "block 100 to head 120 inclusive is 21 blocks")
	})
}

func TestProjectPreForward_EthGetLogs_RecordsNothingItCannotRead(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		project string
		params  string
	}{
		{"NoParams", "range-getlogs-noparams", `[]`},
		{"FilterIsNotAnObject", "range-getlogs-notobject", `["0x1"]`},
		// EIP-234 makes blockHash exclusive with the range bounds. A client
		// that sends both anyway must not be counted as asking for a range.
		{"BlockHashFilter", "range-getlogs-blockhash", `[{"blockHash":"0xabc","fromBlock":"0x1","toBlock":"0xa"}]`},
		{"UnresolvableTag", "range-getlogs-pending", `[{"fromBlock":"pending","toBlock":"latest"}]`},
		{"ToBlockBelowFromBlock", "range-getlogs-inverted", `[{"fromBlock":"0x14","toBlock":"0x10"}]`},
		{"MissingBounds", "range-getlogs-nobounds", `[{"address":"0xdead"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := rangeNetwork(tc.project, 0)
			nq := common.NewNormalizedRequest([]byte(
				`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":` + tc.params + `}`))

			handled, resp, err := HandleProjectPreForward(ctx, n, nq)
			require.NoError(t, err)
			assert.False(t, handled)
			assert.Nil(t, resp)

			count, _ := histSamples(t, telemetry.MetricNetworkEvmGetLogsRangeRequested, tc.project)
			assert.Equal(t, uint64(0), count,
				"a range the hook cannot read must not become a histogram sample")
		})
	}

	t.Run("NilNetwork", func(t *testing.T) {
		nq := common.NewNormalizedRequest([]byte(
			`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x1","toBlock":"0x2"}]}`))
		handled, resp, err := projectPreForward_eth_getLogs(ctx, nil, nq)
		require.NoError(t, err)
		assert.False(t, handled)
		assert.Nil(t, resp)
	})

	t.Run("NilRequest", func(t *testing.T) {
		handled, resp, err := projectPreForward_eth_getLogs(ctx, rangeNetwork("range-getlogs-nilreq", 0), nil)
		require.NoError(t, err)
		assert.False(t, handled)
		assert.Nil(t, resp)
	})
}

func TestProjectPreForward_TraceFilter_RecordsTheRangeItCanRead(t *testing.T) {
	ctx := context.Background()

	for _, method := range []string{"trace_filter", "arbtrace_filter"} {
		t.Run(method, func(t *testing.T) {
			project := "range-" + method
			n := rangeNetwork(project, 0)
			nq := common.NewNormalizedRequest([]byte(
				`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":[{"fromBlock":"0x1","toBlock":"0xa"}]}`))

			handled, resp, err := HandleProjectPreForward(ctx, n, nq)
			require.NoError(t, err)
			assert.False(t, handled)
			assert.Nil(t, resp)

			count, sum := histSamples(t, telemetry.MetricNetworkEvmTraceFilterRangeRequested, project)
			assert.Equal(t, uint64(1), count)
			assert.Equal(t, float64(10), sum, "0x1..0xa inclusive is 10 blocks")
		})
	}
}

func TestProjectPreForward_TraceFilter_RecordsNothingItCannotRead(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		project string
		params  string
	}{
		{"NoParams", "range-tracefilter-noparams", `[]`},
		{"FilterIsNotAnObject", "range-tracefilter-notobject", `["0x1"]`},
		{"UnresolvableTag", "range-tracefilter-safe", `[{"fromBlock":"safe","toBlock":"latest"}]`},
		{"ToBlockBelowFromBlock", "range-tracefilter-inverted", `[{"fromBlock":"0xa","toBlock":"0x1"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := rangeNetwork(tc.project, 0)
			nq := common.NewNormalizedRequest([]byte(
				`{"jsonrpc":"2.0","id":1,"method":"trace_filter","params":` + tc.params + `}`))

			handled, resp, err := HandleProjectPreForward(ctx, n, nq)
			require.NoError(t, err)
			assert.False(t, handled)
			assert.Nil(t, resp)

			count, _ := histSamples(t, telemetry.MetricNetworkEvmTraceFilterRangeRequested, tc.project)
			assert.Equal(t, uint64(0), count)
		})
	}

	// The hook is also reachable directly. It re-checks the method itself, so a
	// caller that dispatches it for the wrong method gets a clean pass-through
	// instead of a trace_filter sample carrying an eth_getLogs range.
	t.Run("WrongMethod", func(t *testing.T) {
		const project = "range-tracefilter-wrongmethod"
		n := rangeNetwork(project, 0)
		nq := common.NewNormalizedRequest([]byte(
			`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x1","toBlock":"0xa"}]}`))

		handled, _, err := projectPreForward_trace_filter(ctx, n, nq)
		require.NoError(t, err)
		assert.False(t, handled)

		count, _ := histSamples(t, telemetry.MetricNetworkEvmTraceFilterRangeRequested, project)
		assert.Equal(t, uint64(0), count, "an eth_getLogs range must not land on the trace_filter histogram")
	})

	t.Run("NilNetwork", func(t *testing.T) {
		nq := common.NewNormalizedRequest([]byte(
			`{"jsonrpc":"2.0","id":1,"method":"trace_filter","params":[{"fromBlock":"0x1","toBlock":"0x2"}]}`))
		handled, resp, err := projectPreForward_trace_filter(ctx, nil, nq)
		require.NoError(t, err)
		assert.False(t, handled)
		assert.Nil(t, resp)
	})

	t.Run("NilRequest", func(t *testing.T) {
		handled, resp, err := projectPreForward_trace_filter(ctx, rangeNetwork("range-tf-nilreq", 0), nil)
		require.NoError(t, err)
		assert.False(t, handled)
		assert.Nil(t, resp)
	})
}
