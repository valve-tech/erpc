package policy

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// On reth and geth, eth_getFilterChanges is not a read. It returns the changes
// since the last poll and moves the filter's cursor. A mirrored poll to the node
// that holds the filter takes the events, and the client's next real poll does
// not get them. A mirrored poll to any other node is an error that the tracker
// counts against that node.
//
// Seen on the fleet 2026-11-11: 42 eth_getFilterChanges probes to an external
// upstream on chain 369, and 13 to the node that held the filter on chain 1.
//
// The subscription methods and the writes in a custom namespace (bug-log entry
// 189, msgboard_addMessage) have the same problem: the prober cannot know what
// they do. A method that the network's method table does not classify as a
// stateless read is never mirrored.
func TestProber_NeverMirrorsAMethodThatIsNotAClassifiedRead(t *testing.T) {
	for _, method := range []string{
		"eth_getFilterChanges",
		"eth_getFilterLogs",
		"eth_newFilter",
		"eth_newBlockFilter",
		"eth_newPendingTransactionFilter",
		"eth_uninstallFilter",
		"eth_subscribe",
		"eth_unsubscribe",
		// A non-canonical casing must not slip past the gate.
		"ETH_GETFILTERCHANGES",
		// A write in a custom namespace (entry 189).
		"msgboard_addMessage",
		// A method nobody classified.
		"vendor_somethingNew",
	} {
		t.Run(method, func(t *testing.T) {
			deps := &stubEngine{}
			holder := &stubProbeUpstream{id: "holder"}
			deps.setExcluded("evm:1", []common.Upstream{holder})

			p, _ := newTestProber(t, deps, &ProbeConfig{
				SampleRate:    1.0,
				MaxConcurrent: 4,
				Timeout:       5 * time.Second,
			})
			defer p.Stop()

			skipped := telemetry.MetricSelectionProbeSkipped.WithLabelValues("evm:1", "not_probe_eligible")
			before := testutil.ToFloat64(skipped)

			p.Publish(makeProbeReq(t, method))
			time.Sleep(150 * time.Millisecond)

			assert.Equal(t, int64(0), holder.calls.Load(),
				"%s must never be mirrored to another upstream", method)
			assert.Equal(t, before+1, testutil.ToFloat64(skipped),
				"the skip must be counted as not_probe_eligible, so the lost probe volume is visible")
		})
	}
}

// The gate must not stop re-admission. A stateless read in the method table
// is still mirrored, or an excluded upstream never gets samples to heal.
func TestProber_StillMirrorsClassifiedReads(t *testing.T) {
	for _, method := range []string{
		"eth_blockNumber",
		"eth_getBlockByNumber",
		"eth_getBalance",
		"eth_call",
		"eth_getLogs",
		"eth_chainId",
		"eth_getTransactionReceipt",
	} {
		t.Run(method, func(t *testing.T) {
			deps := &stubEngine{}
			dead := &stubProbeUpstream{id: "dead"}
			deps.setExcluded("evm:1", []common.Upstream{dead})

			p, _ := newTestProber(t, deps, &ProbeConfig{
				SampleRate:    1.0,
				MaxConcurrent: 4,
				Timeout:       5 * time.Second,
			})
			defer p.Stop()

			p.Publish(makeProbeReq(t, method))
			assert.Equal(t, int64(1), waitForCalls(dead, 1, 1*time.Second),
				"%s is a stateless read and must still be mirrored", method)
		})
	}
}

// mirror() calls Forward with byPassMethodExclusion=true, and that flag also
// skips the X-ERPC-Use-Upstream check in Upstream.shouldSkip. A client that
// pins a request to one upstream has said which upstreams may see it. The
// prober must not send it to any other.
func TestProber_HonoursTheUpstreamPin(t *testing.T) {
	deps := &stubEngine{}
	holder := &stubProbeUpstream{id: "holder"}
	other := &stubProbeUpstream{id: "other"}
	deps.setExcluded("evm:1", []common.Upstream{other, holder})

	p, _ := newTestProber(t, deps, &ProbeConfig{
		SampleRate:    1.0,
		MaxConcurrent: 4,
		Timeout:       5 * time.Second,
	})
	defer p.Stop()

	skipped := telemetry.MetricSelectionProbeSkipped.WithLabelValues("evm:1", "pinned_elsewhere")
	before := testutil.ToFloat64(skipped)

	req := makeProbeReq(t, "eth_getBalance")
	req.SetDirectives(&common.RequestDirectives{UseUpstream: "holder"})
	p.Publish(req)

	require.Equal(t, int64(1), waitForCalls(holder, 1, 1*time.Second),
		"the pinned upstream is still a probe target, or this test proves nothing")
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int64(0), other.calls.Load(),
		"a request pinned to holder must never be mirrored to other")
	assert.Equal(t, before+1, testutil.ToFloat64(skipped),
		"the skip must be counted as pinned_elsewhere")
}

// The method table is the network's own, so an operator decides what is a
// read. A method the operator adds is mirrored; a default the operator drops
// (methods.definitions without preserveDefaultMethods) is not.
func TestProber_UsesTheNetworksMethodTable(t *testing.T) {
	methods := &common.MethodsConfig{
		Definitions: map[string]*common.CacheMethodConfig{
			"custom_getThing": {},
		},
	}
	require.NoError(t, methods.SetDefaults())
	network := &stubProbeNetwork{cfg: &common.NetworkConfig{Methods: methods}}

	for method, want := range map[string]int64{
		"custom_getThing":      1,
		"eth_getBalance":       0,
		"eth_getFilterChanges": 0,
	} {
		t.Run(method, func(t *testing.T) {
			deps := &stubEngine{}
			dead := &stubProbeUpstream{id: "dead"}
			deps.setExcluded("evm:1", []common.Upstream{dead})

			p, _ := newTestProber(t, deps, &ProbeConfig{
				SampleRate:    1.0,
				MaxConcurrent: 4,
				Timeout:       5 * time.Second,
			})
			defer p.Stop()

			req := makeProbeReq(t, method)
			req.SetNetwork(network)
			p.Publish(req)
			if want > 0 {
				assert.Equal(t, want, waitForCalls(dead, want, 1*time.Second))
				return
			}
			time.Sleep(150 * time.Millisecond)
			assert.Equal(t, int64(0), dead.calls.Load())
		})
	}
}

// stubProbeNetwork carries only a network config, which is all the prober reads.
type stubProbeNetwork struct {
	cfg *common.NetworkConfig
}

func (s *stubProbeNetwork) Id() string                                    { return "evm:1" }
func (s *stubProbeNetwork) Label() string                                 { return "evm:1" }
func (s *stubProbeNetwork) ProjectId() string                             { return "p1" }
func (s *stubProbeNetwork) Architecture() common.NetworkArchitecture      { return common.ArchitectureEvm }
func (s *stubProbeNetwork) Config() *common.NetworkConfig                 { return s.cfg }
func (s *stubProbeNetwork) Logger() *zerolog.Logger                       { l := zerolog.Nop(); return &l }
func (s *stubProbeNetwork) GetMethodMetrics(string) common.TrackedMetrics { return nil }
func (s *stubProbeNetwork) Forward(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return nil, nil
}
func (s *stubProbeNetwork) GetFinality(context.Context, *common.NormalizedRequest, *common.NormalizedResponse) common.DataFinalityState {
	return common.DataFinalityStateUnknown
}
