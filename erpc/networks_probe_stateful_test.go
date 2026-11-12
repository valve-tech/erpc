package erpc

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNetworkProbe_NeverMirrorsAFilterPollToAnotherUpstream walks the fleet
// incident of 2026-11-11 through a real Network. Network.Forward publishes the
// request to the probe bus BEFORE shouldHandleMethod applies the stateful-method
// gate, so a filter poll reaches the prober whether or not the network serves
// it. The prober then mirrors it to each excluded upstream, and the mirror
// skips the X-ERPC-Use-Upstream check.
//
// On reth and geth a filter poll moves the filter's cursor, so a mirror to the
// node that holds the filter takes events from the client. A mirror to any
// other node is an error that counts against that node.
func TestNetworkProbe_NeverMirrorsAFilterPollToAnotherUpstream(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	setupGockObserver(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		poll = "eth_getFilterChanges"
		read = "eth_getBlockByNumber"
	)
	holderHost, otherHost := upstreamHostFromID("holder"), upstreamHostFromID("other")
	network := setupSelectionPolicyNetworkWithEval(t, ctx, []*common.UpstreamConfig{
		{Type: common.UpstreamTypeEvm, Id: "holder", Endpoint: holderHost, Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Type: common.UpstreamTypeEvm, Id: "other", Endpoint: otherHost, Evm: &common.EvmUpstreamConfig{ChainId: 123}},
	}, `(upstreams, ctx) => upstreams
		.blockMethods({ match: ['*'], when: all(samplesAbove(20), latencyAbove(5000, 99)) })
		.probeExcluded({ sampleRate: 1, minSamples: 0 })`)

	holder, other := upstreamByID(t, network, "holder"), upstreamByID(t, network, "other")
	for _, m := range []string{poll, read} {
		seedDegraded(network.metricsTracker, holder, seedSpec{method: m, successful: 100, successAvgMs: 10})
		seedDegraded(network.metricsTracker, other, seedSpec{method: m, successful: 100, successAvgMs: 9000})
	}
	policy.TickForTest(network.policyEngine, network.networkId, "*")
	for _, m := range []string{poll, read} {
		require.Equal(t, []string{"other"}, idsFromUpstreams(network.policyEngine.GetExcluded(network.networkId, m, "*")),
			"other must be a probe candidate for %s, or this test proves nothing", m)
	}

	mockClean(t, holderHost, poll, "0x")
	mockClean(t, otherHost, poll, "0x")
	mockClean(t, holderHost, read, "0x1")
	mockClean(t, otherHost, read, "0x1")

	waitForHits := func(host string, want int) int {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && gockHits(host) < want {
			time.Sleep(10 * time.Millisecond)
		}
		return gockHits(host)
	}
	forward := func(method, params, pin string) {
		t.Helper()
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":` + params + `}`))
		if pin != "" {
			req.SetDirectives(&common.RequestDirectives{UseUpstream: pin})
		}
		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.Equal(t, "holder", resp.Upstream().Id())
	}

	// Control: an unpinned stateless read IS mirrored to the excluded upstream.
	// Without this, a broken prober would pass every assertion below.
	resetGockHits()
	forward(read, `["0x10",false]`, "")
	require.GreaterOrEqual(t, waitForHits(otherHost, 1), 1,
		"an unpinned stateless read must still be mirrored to the excluded upstream")

	// The client's filter lives on holder, so the client pins its polls there.
	resetGockHits()
	forward(poll, `["0xabc"]`, "holder")
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, 0, gockHits(otherHost),
		"a filter poll must never be mirrored to an upstream that does not hold the filter")
	assert.Equal(t, 1, gockHits(holderHost),
		"holder must see the client's poll exactly once: a mirrored poll would take its events")

	// A pinned stateless read: the pin still binds the mirror.
	resetGockHits()
	forward(read, `["0x10",false]`, "holder")
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, 0, gockHits(otherHost),
		"a request pinned to holder must never be mirrored to other")
}
