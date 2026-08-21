package evm

import (
	"bytes"
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover the eth_getLogs guards that only a request the parser
// cannot read, or a topic filter of a shape the suite never sent, can reach.

// unreadableGetLogsRequest carries a truncated JSON body, so every attempt to
// resolve it into a JsonRpcRequest fails.
func unreadableGetLogsRequest() *common.NormalizedRequest {
	return common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[`))
}

func TestNetworkPreForward_AnUnreadableBodyIsReportedNotForwarded(t *testing.T) {
	n := &testNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
	}}

	handled, resp, err := networkPreForward_eth_getLogs(
		context.Background(), n, nil, unreadableGetLogsRequest())

	require.Error(t, err, "a body the hook cannot parse must not be passed to an upstream unchecked")
	// Discriminating: handled=true is what stops the request. A hook that
	// returned (false, nil, err) would leave the caller free to forward the
	// same unparsable body, and every other fallthrough in this hook returns
	// handled=false with a nil error.
	assert.True(t, handled)
	assert.Nil(t, resp)
}

func TestUpstreamPreForward_AnUnreadableBodyIsReportedNotForwarded(t *testing.T) {
	n := &testNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId:   123,
			Integrity: &common.EvmIntegrityConfig{EnforceGetLogsBlockRange: util.BoolPtr(true)},
		},
	}}

	handled, resp, err := upstreamPreForward_eth_getLogs(
		context.Background(), n, newForwardingUpstream(123), unreadableGetLogsRequest())

	require.Error(t, err)
	assert.True(t, handled, "the range check must stop a request it cannot read")
	assert.Nil(t, resp)
}

func TestSplitEthGetLogsRequest_AnUnreadableBodyIsAnError(t *testing.T) {
	subs, err := splitEthGetLogsRequest(unreadableGetLogsRequest())

	require.Error(t, err, "a body that cannot be parsed cannot be split")
	// Discriminating: an empty slice WITHOUT an error is what the error-split
	// caller reads as "nothing to do, keep the original error" — the same
	// visible outcome. Only the error tells the two apart for any future
	// caller that acts on the split itself.
	assert.Empty(t, subs)
}

// --- topic counting ---

func TestNetworkPreForward_TheTopicLimitCountsTopicZeroOnly(t *testing.T) {
	for _, tc := range []struct {
		name      string
		topics    []interface{}
		maxTopics int64
		rejected  bool
	}{
		// A scalar topic0 counts as one topic, and one topic is WITHIN a limit
		// of one. This pins the boundary: the limit is a maximum, not an
		// exclusive bound. (The count itself is only ever compared against a
		// limit of one or more, so a count of one can never reject on its own.)
		{"AScalarTopicZeroIsOneTopic", []interface{}{"0xaaa"}, 1, false},
		// An OR list at topic0 counts its members.
		{"AnOrListAtTopicZeroCountsItsMembers", []interface{}{[]interface{}{"0xaaa", "0xbbb"}}, 1, true},
		// Later positions are AND-ed, not OR-ed, so they never add to the count.
		{"LaterPositionsDoNotCount", []interface{}{"0xaaa", []interface{}{"0xbbb", "0xccc", "0xddd"}}, 1, false},
		// A wildcard at topic0 is no topic at all.
		{"ANullTopicZeroIsNoTopic", []interface{}{nil}, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &testNetwork{cfg: &common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm:          &common.EvmNetworkConfig{ChainId: 123, GetLogsMaxAllowedTopics: tc.maxTopics},
			}}
			r := createTestRequest(map[string]interface{}{
				"fromBlock": "0x1",
				"toBlock":   "0x5",
				"topics":    tc.topics,
			})

			handled, _, err := networkPreForward_eth_getLogs(context.Background(), n, nil, r)

			if tc.rejected {
				require.Error(t, err)
				assert.True(t, handled)
				// Discriminating: the TOPIC limit must be the one that fired.
				// The range and address limits reject the same request with a
				// different code, and the caller maps each to its own response.
				assert.True(t,
					common.HasErrorCode(err, common.ErrCodeGetLogsExceededMaxAllowedTopics),
					"expected the topic limit to fire, got %v", err)
				return
			}
			require.NoError(t, err, "a filter within the topic limit must pass through")
			assert.False(t, handled)
		})
	}
}

// --- GetLogsMultiResponseWriter: the separator write ---

// oneShotFailingWriter rejects the single write that starts at failAt and
// accepts everything else. A sink that stays dead cannot discriminate here:
// the very next write would fail too, so the error would surface anyway. Only
// a sink that RECOVERS shows whether the separator's own failure was reported
// or swallowed.
type oneShotFailingWriter struct {
	buf     bytes.Buffer
	failAt  int
	written int
	failed  bool
}

func (w *oneShotFailingWriter) Write(p []byte) (int, error) {
	if !w.failed && w.written == w.failAt {
		w.failed = true
		return 0, errIOSinkRefused
	}
	n, _ := w.buf.Write(p)
	w.written += n
	return n, nil
}

var errIOSinkRefused = errSinkRefused{}

type errSinkRefused struct{}

func (errSinkRefused) Error() string { return "sink refused the write" }

func TestGetLogsMultiResponseWriter_ASinkThatFailsExactlyAtTheSeparatorIsReported(t *testing.T) {
	first := logsResponse(t, `[{"logIndex":"0x1"}]`)
	second := logsResponse(t, `[{"logIndex":"0x2"}]`)

	// The opening bracket plus the first entry's inner content. The very next
	// byte the writer emits is the comma separator.
	var firstOnly bytes.Buffer
	_, err := NewGetLogsMultiResponseWriter([]*common.JsonRpcResponse{first}).WriteTo(&firstOnly, true)
	require.NoError(t, err)
	upToSeparator := 1 + firstOnly.Len()

	w := &oneShotFailingWriter{failAt: upToSeparator}
	n, err := NewGetLogsMultiResponseWriter([]*common.JsonRpcResponse{first, second}).WriteTo(w, false)

	require.Error(t, err, "a separator the sink refused must be reported")
	assert.Equal(t, int64(upToSeparator), n, "the count must stop where the sink stopped")
	// Discriminating: the sink accepted every OTHER byte, so a writer that
	// swallowed the separator failure would return a nil error and a body with
	// the two log arrays glued together — invalid JSON the caller would ship.
	assert.NotContains(t, w.buf.String(), `"0x1"}{`)
}
