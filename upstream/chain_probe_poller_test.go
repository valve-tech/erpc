package upstream

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chainProbePoller writes exactly two signals: the tip, through
// health.Tracker.SetLatestBlockNumber, and the cordon flag the selection
// policy's removeCordoned reads. Both are routing decisions — a stuck cordon
// hides a healthy node, and a lifted one sends traffic to a node that is behind.

// scriptedFamily is a common.ChainFamily whose probe answer a test sets between
// calls, and which reports each call on a channel.
type scriptedFamily struct {
	mu     sync.Mutex
	probe  common.ChainProbe
	calls  int
	probed chan struct{}
}

func newScriptedFamily(p common.ChainProbe) *scriptedFamily {
	return &scriptedFamily{probe: p, probed: make(chan struct{}, 64)}
}

func (f *scriptedFamily) Family() common.NetworkArchitecture { return fakeFamilyName }
func (f *scriptedFamily) Transport() common.ChainTransport   { return common.TransportJsonRpc }
func (f *scriptedFamily) ValidateNetworkId(body string) bool { return body != "" }
func (f *scriptedFamily) Classify(common.ClassifyInput) common.RotateVerdict {
	return common.VerdictServe
}

func (f *scriptedFamily) Probe(ctx context.Context, _ common.ProbeCaller) common.ChainProbe {
	f.mu.Lock()
	p := f.probe
	f.calls++
	f.mu.Unlock()
	// A probe must run under a deadline, or a slow node accumulates in-flight
	// requests until the pool is empty.
	if _, ok := ctx.Deadline(); !ok {
		p.Detail = "PROBE RAN WITHOUT A DEADLINE"
	}
	select {
	case f.probed <- struct{}{}:
	default:
	}
	return p
}

func (f *scriptedFamily) set(p common.ChainProbe) {
	f.mu.Lock()
	f.probe = p
	f.mu.Unlock()
}

func (f *scriptedFamily) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newProbePoller wires an upstream to a scripted family without going through
// bootstrap, so each test drives poll() itself instead of waiting on a ticker.
func newProbePoller(t *testing.T, p common.ChainProbe) (*Upstream, *scriptedFamily, *chainProbePoller) {
	t.Helper()
	fam := newScriptedFamily(p)
	u := newTestUpstream(t, t.Context(), &common.UpstreamConfig{
		Id: "u1", Type: common.UpstreamType(fakeFamilyName),
		Endpoint: "http://node.localhost:1234", Chain: "mainnet",
		ChainProbeInterval: common.Duration(time.Hour),
	})
	u.networkId.Store("fakechain:mainnet")
	poller := u.chainProbePoller(fam, nopProbeCaller{})
	t.Cleanup(func() { chainProbePollers.Delete(u) })
	return u, fam, poller
}

func tipOf(t *testing.T, u *Upstream) int64 {
	t.Helper()
	return u.metricsTracker.
		GetUpstreamMethodMetrics(u, "*", common.DataFinalityStateAll).
		BlockHeadLag.Load()
}

func TestChainProbePoller_PollCordonsANodeThatStopsServing(t *testing.T) {
	u, fam, poller := newProbePoller(t, common.ChainProbe{
		Liveness: common.ChainSyncing, Tip: 700000, Detail: "initial block download",
	})

	poller.poll()

	require.Equal(t, 1, fam.callCount())
	reason, cordoned := u.CordonedReason("*")
	require.True(t, cordoned, "a node that is not serving must be taken out of rotation")
	assert.Contains(t, reason, "initial block download",
		"the cordon reason must carry the probe detail, or an operator cannot tell why")
	assert.NotContains(t, reason, "WITHOUT A DEADLINE", "poll must bound the probe call")
}

func TestChainProbePoller_PublishesTheTipEvenWhileTheNodeIsBehind(t *testing.T) {
	// The tracker derives lag from the highest tip in the network. Withholding
	// a syncing node's height makes it read as "furthest behind" instead of
	// "catching up", and the operator loses the number they watch.
	_, _, ahead := newProbePoller(t, common.ChainProbe{Liveness: common.ChainHealthy, Tip: 812345})
	ahead.poll()

	behindUps, _, behind := newProbePoller(t, common.ChainProbe{
		Liveness: common.ChainSyncing, Tip: 812300, Detail: "catching up",
	})
	behindUps.metricsTracker = ahead.upstream.metricsTracker
	behind.upstream.metricsTracker = ahead.upstream.metricsTracker
	behind.poll()

	assert.EqualValues(t, 45, tipOf(t, behindUps),
		"a syncing node's tip never reached the tracker, so its lag reads as unknown")
}

// NOTE: "a failed probe (Tip 0) must not be published as a height" is NOT
// tested here. health.Tracker.SetLatestBlockNumber already refuses every
// non-positive sample (see TestSetLatestBlockNumber_NonPositiveSamplesAreRejected
// in health/), so the `probe.Tip > 0` guard in apply() cannot change the
// outcome. A test here would pass with the guard deleted, which makes it worth
// nothing.

func TestChainProbePoller_LiftsItsOwnCordonWhenTheNodeCatchesUp(t *testing.T) {
	u, fam, poller := newProbePoller(t, common.ChainProbe{
		Liveness: common.ChainSyncing, Tip: 100, Detail: "behind",
	})

	poller.poll()
	_, cordoned := u.CordonedReason("*")
	require.True(t, cordoned)

	// The cordon is edge-triggered, and this is what that means in practice:
	// once the poller has cordoned, an operator who deliberately puts the node
	// back into rotation is not overruled on the next tick. A level-triggered
	// poller would re-cordon immediately and the operator could never win.
	u.Uncordon("*", "operator: serving stale reads on purpose")
	poller.poll()
	poller.poll()
	_, cordoned = u.CordonedReason("*")
	require.False(t, cordoned,
		"the poller re-cordoned a node the operator had returned to rotation")

	fam.set(common.ChainProbe{Liveness: common.ChainHealthy, Tip: 200, Detail: "height 200"})
	poller.poll()

	_, cordoned = u.CordonedReason("*")
	assert.False(t, cordoned, "a recovered node stayed cordoned and will never take traffic again")

	// And a healthy node that was never cordoned stays untouched.
	poller.poll()
	_, cordoned = u.CordonedReason("*")
	assert.False(t, cordoned)
}

func TestChainProbePoller_DoesNotLiftAnOperatorsCordon(t *testing.T) {
	// Edge-triggering exists for exactly this: an operator who cordons a node
	// for maintenance must not have a health probe put it back in rotation.
	u, _, poller := newProbePoller(t, common.ChainProbe{
		Liveness: common.ChainHealthy, Tip: 900, Detail: "height 900",
	})

	u.Cordon("*", "operator: draining for maintenance")
	poller.poll()

	reason, cordoned := u.CordonedReason("*")
	require.True(t, cordoned, "the probe lifted a cordon it did not place")
	assert.Equal(t, "operator: draining for maintenance", reason)
}

func TestChainProbePoller_OneUpstreamKeepsOnePoller(t *testing.T) {
	// Bootstrap is retried against the SAME Upstream. A second poller would be
	// a second ticker goroutine that only app shutdown can stop.
	u, fam, first := newProbePoller(t, common.ChainProbe{Liveness: common.ChainHealthy, Tip: 1})

	second := u.chainProbePoller(fam, nopProbeCaller{})
	assert.Same(t, first, second, "a bootstrap retry created a second poller for one upstream")
}

func TestChainProbePoller_DefaultsTheIntervalWhenTheUpstreamNamesNone(t *testing.T) {
	fam := newScriptedFamily(common.ChainProbe{Liveness: common.ChainHealthy, Tip: 1})
	u := newTestUpstream(t, t.Context(), &common.UpstreamConfig{
		Id: "u1", Type: common.UpstreamType(fakeFamilyName),
		Endpoint: "http://node.localhost:1234", Chain: "mainnet",
	})
	t.Cleanup(func() { chainProbePollers.Delete(u) })

	// A zero interval would make time.NewTicker panic and take the process
	// down at bootstrap.
	assert.Equal(t, defaultChainProbeInterval, u.chainProbePoller(fam, nopProbeCaller{}).interval)
}

func TestChainProbePoller_LoopProbesOnTheTickerAndStopsWithTheApp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fam := newScriptedFamily(common.ChainProbe{
		Liveness: common.ChainSyncing, Tip: 5, Detail: "behind",
	})
	u := newTestUpstream(t, ctx, &common.UpstreamConfig{
		Id: "u1", Type: common.UpstreamType(fakeFamilyName),
		Endpoint: "http://node.localhost:1234", Chain: "mainnet",
		ChainProbeInterval: common.Duration(10 * time.Millisecond),
	})
	u.networkId.Store("fakechain:mainnet")
	poller := u.chainProbePoller(fam, nopProbeCaller{})
	t.Cleanup(func() { chainProbePollers.Delete(u) })

	poller.start()
	// start() is guarded; extra calls must not add a second ticker goroutine.
	poller.start()

	select {
	case <-fam.probed:
	case <-time.After(5 * time.Second):
		t.Fatal("the poller never probed; a stalled node would never be cordoned")
	}

	// The loop's only exit is the app context. A poller that outlived it would
	// keep hitting a node after shutdown.
	cancel()
	require.Eventually(t, func() bool {
		_, still := chainProbePollers.Load(u)
		return !still
	}, 5*time.Second, 10*time.Millisecond,
		"the poller goroutine did not stop when the app context was cancelled")
}
