package policy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The prober calls Upstream.Forward with byPassMethodExclusion=true, so the
// operator's own `ignoreMethods` never runs for probe traffic. The flag exists
// to re-admit an upstream that eRPC wrongly tagged as non-supporting; it must
// not also cancel the operator's configuration.
//
// Measured on the fleet over 24 hours: 512,334 probes a day went to an upstream
// whose config forbids the method — 82% of all probe traffic.
func TestProber_SkipsAMethodTheUpstreamIgnores(t *testing.T) {
	deps := &stubEngine{}
	// This upstream's own config ignores txpool_*; it serves eth_* normally.
	dead := &stubProbeUpstream{
		id: "public",
		handleMethod: func(method string) (bool, error) {
			return method != "txpool_content", nil
		},
	}
	deps.setExcluded("evm:1", []common.Upstream{dead})

	p, _ := newTestProber(t, deps, &ProbeConfig{
		SampleRate:    1.0,
		MaxConcurrent: 4,
		Timeout:       5 * time.Second,
	})
	defer p.Stop()

	skipped := telemetry.MetricSelectionProbeSkipped.WithLabelValues("evm:1", "method_ignored")
	before := testutil.ToFloat64(skipped)

	p.Publish(makeProbeReq(t, "txpool_content"))
	time.Sleep(150 * time.Millisecond)

	assert.Equal(t, int64(0), dead.calls.Load(),
		"a method the upstream's own config ignores must never be probed")
	assert.Equal(t, before+1, testutil.ToFloat64(skipped),
		"the skip must be counted as method_ignored, so the loss of probe volume is visible")

	// Re-admission must still work for every method the upstream can serve.
	// A gate that stops all probing strands the upstream in the excluded set.
	p.Publish(makeProbeReq(t, "eth_getBalance"))
	assert.Equal(t, int64(1), waitForCalls(dead, 1, 1*time.Second),
		"a method the upstream can serve must still be probed")
}

// The prober hands the CALLER's own request object to the probed upstream, and
// Upstream.Forward writes its answer into that object
// (upstream/upstream.go:795 SetLastValidResponse). When the caller's own
// attempts then return empty, erpc/networks.go:2381 serves the stored response
// and adopts its upstream. A probe's answer therefore reaches a real caller.
//
// SetLastValidResponse prefers a non-empty body over an empty one, so the probe
// wins the slot precisely when our own upstreams answered empty.
func TestProber_DoesNotTouchTheCallersRequest(t *testing.T) {
	deps := &stubEngine{}
	// This upstream answers, and writes its answer into whatever request it
	// is handed — exactly as the real Upstream.Forward does.
	dead := &stubProbeUpstream{id: "public", writeLVR: true}
	deps.setExcluded("evm:1", []common.Upstream{dead})

	p, _ := newTestProber(t, deps, &ProbeConfig{
		SampleRate:    1.0,
		MaxConcurrent: 4,
		Timeout:       5 * time.Second,
	})
	defer p.Stop()

	caller := makeProbeReq(t, "eth_getBalance")
	p.Publish(caller)
	require.Equal(t, int64(1), waitForCalls(dead, 1, 1*time.Second),
		"the probe must fire, or this test proves nothing")

	// Let the mirror goroutine finish its write after Forward returns.
	time.Sleep(100 * time.Millisecond)

	assert.Nil(t, caller.LastValidResponse(),
		"a probe's response must never enter the caller's last-valid-response slot")
	assert.Nil(t, caller.LastUpstream(),
		"a probe must never set the caller's last upstream")

	seen := dead.seenReq.Load()
	require.NotNil(t, seen, "the stub must have recorded the request it received")
	assert.NotSame(t, caller, seen,
		"the probe must carry its own request, not the caller's object")

	// The probe still has to be the same call, or it measures nothing.
	seenMethod, err := seen.Method()
	require.NoError(t, err)
	assert.Equal(t, "eth_getBalance", seenMethod,
		"the probe must ask the same method the caller asked")
}

// A request reaches the probe bus in one of two shapes: parsed from an HTTP
// body, or built directly from a JsonRpcRequest (body nil, because
// JsonRpcRequest() drops the raw body after a successful parse). The copy must
// work for both, and must share no mutable state in either.
func TestProbeRequestFrom_SharesNothingMutable(t *testing.T) {
	ctx := context.Background()

	t.Run("a body-backed request", func(t *testing.T) {
		src := makeProbeReq(t, "eth_getBalance")
		src.SetDirectives(&common.RequestDirectives{RetryEmpty: true})

		preq, err := probeRequestFrom(ctx, src)
		require.NoError(t, err)
		assert.NotSame(t, src, preq)

		method, err := preq.Method()
		require.NoError(t, err)
		assert.Equal(t, "eth_getBalance", method)

		// Directives carry over by value, so probe behaviour is unchanged,
		// but the probe cannot edit the caller's copy.
		require.NotNil(t, preq.Directives())
		assert.True(t, preq.Directives().RetryEmpty,
			"the probe keeps the caller's directive values")
		assert.NotSame(t, src.Directives(), preq.Directives(),
			"the probe must hold its own directives")
	})

	t.Run("a request whose raw body is gone", func(t *testing.T) {
		// The EVM layer interpolates block tags into params IN PLACE, so a
		// shared params slice is a live race, not just a stale read.
		src := common.NewNormalizedRequestFromJsonRpcRequest(&common.JsonRpcRequest{
			JSONRPC: "2.0",
			ID:      1,
			Method:  "eth_getLogs",
			Params:  []interface{}{map[string]interface{}{"fromBlock": "latest"}},
		})
		require.Nil(t, src.Body(), "this case exists because the body is nil")

		preq, err := probeRequestFrom(ctx, src)
		require.NoError(t, err)

		pjrq, err := preq.JsonRpcRequest(ctx)
		require.NoError(t, err)
		sjrq, err := src.JsonRpcRequest(ctx)
		require.NoError(t, err)
		assert.NotSame(t, sjrq, pjrq, "the probe must hold its own json-rpc request")

		// Interpolate on the probe's copy, as the EVM layer would.
		pjrq.Params[0].(map[string]interface{})["fromBlock"] = "0xdeadbeef"
		assert.Equal(t, "latest", sjrq.Params[0].(map[string]interface{})["fromBlock"],
			"a probe must never rewrite the params the caller is still using")
	})
}

// A method verdict that ERRORS is not a verdict. The gate stops a probe only
// when the upstream's config says "no" for certain; anything else keeps
// re-admission working, which is the whole point of probing.
func TestProber_ProbesWhenTheMethodVerdictErrors(t *testing.T) {
	deps := &stubEngine{}
	dead := &stubProbeUpstream{
		id: "public",
		handleMethod: func(method string) (bool, error) {
			return false, errors.New("bad wildcard in ignoreMethods")
		},
	}
	deps.setExcluded("evm:1", []common.Upstream{dead})

	p, _ := newTestProber(t, deps, &ProbeConfig{
		SampleRate:    1.0,
		MaxConcurrent: 4,
		Timeout:       5 * time.Second,
	})
	defer p.Stop()

	p.Publish(makeProbeReq(t, "eth_getBalance"))
	assert.Equal(t, int64(1), waitForCalls(dead, 1, 1*time.Second),
		"an unresolvable method verdict must not silently stop re-admission")
}
