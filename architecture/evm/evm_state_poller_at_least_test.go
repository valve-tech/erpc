package evm

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// headUpstream serves eth_getBlockByNumber("latest") from a head the test sets.
type headUpstream struct {
	fakePollerUpstream
	head  atomic.Int64
	delay time.Duration
}

func (h *headUpstream) Forward(ctx context.Context, nq *common.NormalizedRequest, _, _ bool) (*common.NormalizedResponse, error) {
	h.forwards.Add(1)
	// A node answers with the head it has when the request arrives; the
	// delay is the network round trip after that.
	head := h.head.Load()
	if h.delay > 0 {
		time.Sleep(h.delay)
	}
	block := map[string]interface{}{
		"number":    fmt.Sprintf("0x%x", head),
		"timestamp": fmt.Sprintf("0x%x", time.Now().Unix()),
		"hash":      fmt.Sprintf("0x%064x", head),
	}
	return common.NewNormalizedResponse().WithRequest(nq).WithJsonRpcResponse(common.MustNewJsonRpcResponse(1, block, nil)), nil
}

func newAtLeastPoller(t *testing.T, head int64, delay time.Duration) (*EvmStatePoller, *headUpstream, *health.Tracker, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logger := zerolog.Nop()
	up := &headUpstream{delay: delay}
	up.logger = logger
	up.cfg = &common.UpstreamConfig{
		Id:   "polled",
		Type: common.UpstreamTypeEvm,
		Evm: &common.EvmUpstreamConfig{
			ChainId:             123,
			StatePollerInterval: common.Duration(time.Hour),
			// A debounce far longer than the test: only a bypass can poll again.
			StatePollerDebounce: common.Duration(time.Hour),
		},
	}
	up.head.Store(head)
	ssr, err := data.NewSharedStateRegistry(ctx, &logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
	})
	require.NoError(t, err)
	tracker := health.NewTracker(&logger, "test", 2*time.Second)
	poller := NewEvmStatePoller("test", ctx, &logger, up, tracker, ssr)

	got, err := poller.PollLatestBlockNumber(ctx)
	require.NoError(t, err)
	require.Equal(t, head, got)
	return poller, up, tracker, ctx
}

// The tracker keys reporters by upstream, so the peers must be the same
// objects across calls.
var atLeastPeers = []*fakePollerUpstream{
	{logger: zerolog.Nop(), cfg: &common.UpstreamConfig{Id: "peer-a", Type: common.UpstreamTypeEvm}},
	{logger: zerolog.Nop(), cfg: &common.UpstreamConfig{Id: "peer-b", Type: common.UpstreamTypeEvm}},
}

// reportPeers sets the head of two other upstreams on the same network, so
// the tracker's corroborated network head (second-highest) is head.
func reportPeers(tracker *health.Tracker, head int64) {
	for _, p := range atLeastPeers {
		tracker.SetLatestBlockNumber(p, head, time.Now().Unix())
	}
}

// A request for a block the network already has, above this upstream's
// cached head, polls at once although the debounce has not passed. A
// read-triggered background poll that ran just before the block landed must
// not make the request wait a full block time (bug log 199).
func TestPollLatestBlockNumberAtLeast_PollsPastTheDebounceForABlockTheNetworkHas(t *testing.T) {
	poller, up, tracker, ctx := newAtLeastPoller(t, 100, 0)
	up.head.Store(101)
	// Read the count first: reportPeers can start the head-advance poll at once.
	before := up.forwards.Load()
	reportPeers(tracker, 101)

	got, err := poller.PollLatestBlockNumberAtLeast(ctx, 101)

	require.NoError(t, err)
	assert.Equal(t, int64(101), got)
	assert.Equal(t, before+1, up.forwards.Load(), "one poll past the debounce")
}

// A block no other upstream reports is not evidence that it exists, so a
// request for it keeps the debounce (an arbitrary future block cannot drive
// polls).
func TestPollLatestBlockNumberAtLeast_KeepsTheDebounceForABlockTheNetworkLacks(t *testing.T) {
	poller, up, tracker, ctx := newAtLeastPoller(t, 100, 0)
	up.head.Store(101)
	reportPeers(tracker, 100)
	before := up.forwards.Load()

	got, err := poller.PollLatestBlockNumberAtLeast(ctx, 101)

	require.NoError(t, err)
	assert.Equal(t, int64(100), got)
	assert.Equal(t, before, up.forwards.Load())
}

// An upstream that is really behind gets at most one extra poll per new
// block number, so the extra load is bounded by the chain's own rate.
func TestPollLatestBlockNumberAtLeast_PollsOncePerNewBlockForALaggingUpstream(t *testing.T) {
	poller, up, tracker, ctx := newAtLeastPoller(t, 100, 0)
	before := up.forwards.Load()
	reportPeers(tracker, 101)

	for range 5 {
		got, err := poller.PollLatestBlockNumberAtLeast(ctx, 101)
		require.NoError(t, err)
		assert.Equal(t, int64(100), got)
	}
	assert.Equal(t, before+1, up.forwards.Load(), "block 101: one poll")

	reportPeers(tracker, 102)
	for range 5 {
		_, _ = poller.PollLatestBlockNumberAtLeast(ctx, 102)
	}
	assert.Equal(t, before+2, up.forwards.Load(), "block 102: one more poll")
}

// Requests that arrive while the forced poll runs wait for it and share its
// answer. Without that they would fall back to the block-time retry wait.
func TestPollLatestBlockNumberAtLeast_ConcurrentRequestsShareOnePoll(t *testing.T) {
	poller, up, tracker, ctx := newAtLeastPoller(t, 100, 0)
	// Below the shared counter's 50 ms updateMaxWait, so the foreground waits
	// for the poll, but long enough that the 20 requests overlap it.
	up.delay = 20 * time.Millisecond
	up.head.Store(101)
	before := up.forwards.Load()
	reportPeers(tracker, 101)

	var wg sync.WaitGroup
	var served atomic.Int32
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := poller.PollLatestBlockNumberAtLeast(ctx, 101); err == nil && got == 101 {
				served.Add(1)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(20), served.Load(), "every request sees block 101")
	assert.Equal(t, before+1, up.forwards.Load(), "one poll for all of them")
}

// When the network's corroborated head moves past this upstream's cached head
// (the newHeads-fed ws siblings report a block first), the poller polls at once
// instead of waiting out its debounce. eth_blockNumber is answered from the
// served tip, which only the HTTP upstreams vote on under option (b), so
// without this the answer trails the chain by up to a debounce (bug log 200).
func TestEvmStatePoller_PollsWhenTheNetworkHeadPassesIt(t *testing.T) {
	poller, up, tracker, _ := newAtLeastPoller(t, 100, 0)
	up.head.Store(101)

	reportPeers(tracker, 101)

	require.Eventually(t, func() bool { return poller.LatestBlock() == 101 }, 2*time.Second, 10*time.Millisecond,
		"the poller must learn block 101 without waiting for its one-hour debounce")
}

// A network head that does not pass this upstream's head starts no poll.
func TestEvmStatePoller_NoPollWhenTheNetworkHeadIsNotAhead(t *testing.T) {
	poller, up, tracker, _ := newAtLeastPoller(t, 100, 0)
	before := up.forwards.Load()

	reportPeers(tracker, 100)

	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, before, up.forwards.Load())
	assert.Equal(t, int64(100), poller.LatestBlock())
}

// Two blocks close together (Sepolia, bug log 201): block 102 arrives while
// the forced poll for 101 is still running. The call for 102 joins that
// poll, which answers 101; it must then poll again for 102, not stop and
// leave 102 to the next debounced poll (8.4 s on a 12 s chain).
func TestPollLatestBlockNumberAtLeast_ABlockThatArrivesDuringAPollGetsItsOwnPoll(t *testing.T) {
	poller, up, tracker, ctx := newAtLeastPoller(t, 100, 0)
	up.delay = 20 * time.Millisecond
	up.head.Store(101)
	reportPeers(tracker, 101)

	first := make(chan int64, 1)
	go func() {
		got, _ := poller.PollLatestBlockNumberAtLeast(ctx, 101)
		first <- got
	}()
	// Block 102 lands while the poll for 101 is in flight.
	time.Sleep(5 * time.Millisecond)
	up.head.Store(102)
	reportPeers(tracker, 102)

	got, err := poller.PollLatestBlockNumberAtLeast(ctx, 102)
	require.NoError(t, err)
	assert.Equal(t, int64(102), got, "the request for 102 must poll again after joining the poll for 101")
	assert.Equal(t, int64(101), <-first)
}
