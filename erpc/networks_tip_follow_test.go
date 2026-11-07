package erpc

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// When the network's corroborated head moves past the tip voters' polled
// heads, the served tip follows at once instead of after a poll debounce, and
// only as many voters poll as the quorum needs (bug log 200).
func TestNetwork_ServedTipFollowsTheNetworkHeadWithQuorumVotersOnly(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Background polling off (1h): only the tip follower can move the heads.
	network := setupRealPollLagNetwork(t, ctx, []realPollFixture{
		{id: "rpc-a", latest: 1000},
		{id: "rpc-b", latest: 1000},
		{id: "rpc-c", latest: 1000},
	})
	require.Equal(t, int64(1000), network.EvmHighestLatestBlockNumber(ctx))
	network.startTipFollow()

	// Every node now has block 1001; the pollers still hold 1000.
	util.ResetGock()
	setupGockObserver(t)
	for _, id := range []string{"rpc-a", "rpc-b", "rpc-c"} {
		mockPollerHostWithLatest(upstreamHostFromID(id), 1001, 991)
	}
	// Two reporters see 1001 first (as the ws siblings do), so the network's
	// corroborated head advances to 1001.
	tr := network.metricsTracker
	tr.SetLatestBlockNumber(upstreamByID(t, network, "rpc-a"), 1001, 0x6702a8f1)
	tr.SetLatestBlockNumber(upstreamByID(t, network, "rpc-b"), 1001, 0x6702a8f1)

	require.Eventually(t, func() bool { return network.EvmHighestLatestBlockNumber(ctx) == 1001 },
		2*time.Second, 10*time.Millisecond, "the served tip must follow the network head without waiting for a poll")

	time.Sleep(200 * time.Millisecond)
	moved := 0
	for _, id := range []string{"rpc-a", "rpc-b", "rpc-c"} {
		if upstreamByID(t, network, id).(common.EvmUpstream).EvmStatePoller().LatestBlock() == 1001 {
			moved++
		}
	}
	assert.Equal(t, 2, moved, "only the two voters the default quorum needs are polled")
}
