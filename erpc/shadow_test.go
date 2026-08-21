package erpc

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/upstream"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// Shadow traffic is how an operator qualifies a new provider before sending it
// real users: mirror live calls at it, compare its answers with production, and
// read the verdict off two counters. Everything downstream of those counters is
// a human decision to cut over, so the counters have to be right. A mismatch
// counted as identical says "this provider agrees with your fleet" about a
// provider that does not.
//
// These tests drive executeShadowRequests directly. The comparison is the part
// with judgement in it; the project-level plumbing that calls it is one clone
// and a `go`.

// shadowNode is a stand-in provider for the mirror. It records the methods it
// was asked for, so a test can prove a request really left eRPC rather than
// only that a counter moved.
type shadowNode struct {
	*httptest.Server

	// blockNumber is what eth_blockNumber returns.
	blockNumber string
	// extraField, when set, is added to the eth_getBlockByNumber result. It
	// gives a test one differing field to put on an ignore list.
	extraField string

	mu      sync.Mutex
	methods []string
}

func newShadowNode(t *testing.T, blockNumber string) *shadowNode {
	t.Helper()
	n := &shadowNode{blockNumber: blockNumber}
	n.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Id     interface{} `json:"id"`
			Method string      `json:"method"`
		}
		_ = json.Unmarshal(body, &req)

		n.mu.Lock()
		n.methods = append(n.methods, req.Method)
		n.mu.Unlock()

		result := interface{}(nil)
		switch req.Method {
		case "eth_chainId":
			result = "0x7b"
		case "eth_syncing":
			result = false
		case "eth_blockNumber":
			result = n.blockNumber
		case "eth_getBlockByNumber":
			block := map[string]interface{}{"number": "0x64", "hash": "0xabc"}
			if n.extraField != "" {
				block["clientVersion"] = n.extraField
			}
			result = block
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0", "id": req.Id, "result": result,
		})
	}))
	t.Cleanup(n.Server.Close)
	return n
}

func (n *shadowNode) sawMethod(method string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, m := range n.methods {
		if m == method {
			return true
		}
	}
	return false
}

// startShadowErpc builds an eRPC whose network has one ordinary upstream and
// one shadow upstream, and returns the project, the network and the shadow
// upstream list the mirror runs over.
func startShadowErpc(t *testing.T, ctx context.Context, shadowCfg *common.ShadowUpstreamConfig, node *shadowNode) (*PreparedProject, *Network, []*upstream.Upstream) {
	t.Helper()

	primary := evmNode(t)
	cfg := &common.Config{
		RateLimiters: &common.RateLimiterConfig{},
		Projects: []*common.ProjectConfig{{
			Id:       "prod",
			Networks: []*common.NetworkConfig{{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}},
			Upstreams: []*common.UpstreamConfig{
				evmUpstream("primary", primary.URL, 123),
				func() *common.UpstreamConfig {
					u := evmUpstream("candidate", node.URL, 123)
					u.Shadow = shadowCfg
					return u
				}(),
			},
		}},
	}
	require.NoError(t, cfg.SetDefaults(nil))

	lg := zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.ErrorLevel)
	ssr, err := data.NewSharedStateRegistry(ctx, &lg, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "100MB"},
		},
	})
	require.NoError(t, err)

	instance, err := NewERPC(ctx, &lg, ssr, nil, nil, cfg)
	require.NoError(t, err)
	instance.Bootstrap(ctx)

	network, err := instance.GetNetwork(ctx, "prod", "evm:123")
	require.NoError(t, err)
	require.NoError(t, network.upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, "evm:123"))

	project, err := instance.GetProject("prod")
	require.NoError(t, err)

	shadows := network.ShadowUpstreams()
	require.Len(t, shadows, 1,
		"the candidate must be registered as a shadow upstream, not as a routable one")
	require.Equal(t, "candidate", shadows[0].Id())

	return project, network, shadows
}

// servedResponse builds the production answer the mirror compares against.
func servedResponse(t *testing.T, network *Network, method, result string) *common.NormalizedResponse {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":[]}`))
	req.SetNetwork(network)

	jrr, err := common.NewJsonRpcResponseFromBytes([]byte(`1`), []byte(result), nil)
	require.NoError(t, err)
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
}

func shadowIdentical(t *testing.T, ups *upstream.Upstream, network *Network, method string) float64 {
	t.Helper()
	return promUtil.ToFloat64(telemetry.MetricShadowResponseIdenticalTotal.WithLabelValues(
		"prod", ups.VendorName(), network.Label(), ups.Id(), method))
}

// shadowMismatch totals the mismatch counter for one candidate and method
// across its remaining label dimensions. Those three labels describe the
// difference (how final the answer was, whether it was empty, whether it was
// larger) and are useful to an operator triaging the mismatch — but the claim
// under test is that a mismatch was counted at all, so summing keeps the test
// from failing over a label value it did not predict.
func shadowMismatch(t *testing.T, ups *upstream.Upstream, network *Network, method string) float64 {
	t.Helper()
	total := 0.0
	for _, finality := range []string{"finalized", "unfinalized", "realtime", "unknown"} {
		for _, empty := range []string{"true", "false"} {
			for _, larger := range []string{"true", "false"} {
				total += promUtil.ToFloat64(telemetry.MetricShadowResponseMismatchTotal.WithLabelValues(
					"prod", ups.VendorName(), network.Label(), ups.Id(), method, finality, empty, larger))
			}
		}
	}
	return total
}

// TestShadow_CountsAnAgreeingCandidateAsIdentical is the "safe to cut over"
// signal. The candidate must really be called — a counter that moves without a
// request would qualify a provider nobody ever asked anything.
func TestShadow_CountsAnAgreeingCandidateAsIdentical(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	node := newShadowNode(t, "0x64")
	project, network, shadows := startShadowErpc(t, ctx,
		&common.ShadowUpstreamConfig{Enabled: true}, node)

	before := shadowIdentical(t, shadows[0], network, "eth_blockNumber")

	project.executeShadowRequests(ctx, network, shadows,
		servedResponse(t, network, "eth_blockNumber", `"0x64"`))

	require.Eventually(t, func() bool {
		return node.sawMethod("eth_blockNumber")
	}, 20*time.Second, 10*time.Millisecond,
		"the candidate never received the mirrored call")

	require.Eventually(t, func() bool {
		return shadowIdentical(t, shadows[0], network, "eth_blockNumber") > before
	}, 20*time.Second, 10*time.Millisecond,
		"the candidate agreed with production but was not counted as identical")

	require.Zero(t, shadowMismatch(t, shadows[0], network, "eth_blockNumber"),
		"an agreeing candidate must not also be counted as a mismatch")
}

// TestShadow_CountsADisagreeingCandidateAsMismatch is the other verdict, and the
// one an operator acts on. Silently counting it as identical is how a provider
// that returns a stale head gets promoted to production.
func TestShadow_CountsADisagreeingCandidateAsMismatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The candidate is 5 blocks behind what production served.
	node := newShadowNode(t, "0x5f")
	project, network, shadows := startShadowErpc(t, ctx,
		&common.ShadowUpstreamConfig{Enabled: true}, node)

	identicalBefore := shadowIdentical(t, shadows[0], network, "eth_blockNumber")

	project.executeShadowRequests(ctx, network, shadows,
		servedResponse(t, network, "eth_blockNumber", `"0x64"`))

	require.Eventually(t, func() bool {
		return shadowMismatch(t, shadows[0], network, "eth_blockNumber") > 0
	}, 20*time.Second, 10*time.Millisecond,
		"a candidate returning a different head was not counted as a mismatch")

	require.Equal(t, identicalBefore, shadowIdentical(t, shadows[0], network, "eth_blockNumber"),
		"a disagreeing candidate must never be counted as identical")
}

// TestShadow_SkipsTheCandidateEntirelyAtSampleRateZero covers the sampling
// throttle. Mirroring doubles a provider's bill, so an operator who dials the
// rate down must actually see less traffic, not just fewer log lines.
func TestShadow_SkipsTheCandidateEntirelyAtSampleRateZero(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	node := newShadowNode(t, "0x64")
	rate := 0.0
	project, network, shadows := startShadowErpc(t, ctx,
		&common.ShadowUpstreamConfig{Enabled: true, SampleRate: &rate}, node)

	// The bootstrap probes already hit the node, so the baseline is whatever it
	// has seen so far; what must not happen is a mirrored user call on top.
	require.False(t, node.sawMethod("eth_blockNumber"))
	before := shadowIdentical(t, shadows[0], network, "eth_blockNumber")

	for i := 0; i < 20; i++ {
		project.executeShadowRequests(ctx, network, shadows,
			servedResponse(t, network, "eth_blockNumber", `"0x64"`))
	}

	// A mirrored call runs in its own goroutine, so "nothing arrived" has to
	// hold over a window rather than at one instant — asserting immediately
	// would pass simply because the request had not landed yet. Two seconds is
	// far beyond the sub-millisecond round trip to a local test server.
	require.Never(t, func() bool {
		return node.sawMethod("eth_blockNumber")
	}, 2*time.Second, 10*time.Millisecond,
		"sampleRate 0 must send the candidate nothing")

	require.Equal(t, before, shadowIdentical(t, shadows[0], network, "eth_blockNumber"),
		"a call that was never mirrored must not be counted as a match")
}

// TestShadow_IgnoresTheFieldsAnOperatorListed covers ignoreFields. Providers
// legitimately differ on bookkeeping fields, and without this the comparison
// reports every single response as a mismatch — which trains the operator to
// ignore the metric entirely.
func TestShadow_IgnoresTheFieldsAnOperatorListed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	node := newShadowNode(t, "0x64")
	node.extraField = "candidate-client/v2"

	project, network, shadows := startShadowErpc(t, ctx, &common.ShadowUpstreamConfig{
		Enabled:      true,
		IgnoreFields: map[string][]string{"eth_getBlockByNumber": {"clientVersion"}},
	}, node)

	before := shadowIdentical(t, shadows[0], network, "eth_getBlockByNumber")

	project.executeShadowRequests(ctx, network, shadows,
		servedResponse(t, network, "eth_getBlockByNumber", `{"number":"0x64","hash":"0xabc"}`))

	require.Eventually(t, func() bool {
		return shadowIdentical(t, shadows[0], network, "eth_getBlockByNumber") > before
	}, 20*time.Second, 10*time.Millisecond,
		"the only difference was on the ignore list, so this must count as identical")
}

// TestShadow_FlagsAnIgnoredFieldThatWasNotListed is the control for the test
// above. Without it, an ignoreFields implementation that ignored everything
// would also pass.
func TestShadow_FlagsAnIgnoredFieldThatWasNotListed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	node := newShadowNode(t, "0x64")
	node.extraField = "candidate-client/v2"

	project, network, shadows := startShadowErpc(t, ctx, &common.ShadowUpstreamConfig{
		Enabled:      true,
		IgnoreFields: map[string][]string{"eth_getBlockByNumber": {"someOtherField"}},
	}, node)

	project.executeShadowRequests(ctx, network, shadows,
		servedResponse(t, network, "eth_getBlockByNumber", `{"number":"0x64","hash":"0xabc"}`))

	require.Eventually(t, func() bool {
		return shadowMismatch(t, shadows[0], network, "eth_getBlockByNumber") > 0
	}, 20*time.Second, 10*time.Millisecond,
		"an unlisted extra field is a real difference and must be reported")
}

// TestShadow_DoesNothingWithoutAResponseOrACandidate pins the two early exits.
// The mirror runs on every served request, so a nil response or an empty
// candidate list must be a cheap no-op rather than a panic in a goroutine that
// takes the process down.
func TestShadow_DoesNothingWithoutAResponseOrACandidate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	node := newShadowNode(t, "0x64")
	project, network, shadows := startShadowErpc(t, ctx,
		&common.ShadowUpstreamConfig{Enabled: true}, node)

	// The mirror wraps itself in a recover() and counts what it catches, so a
	// missing guard shows up as a recovered panic rather than as a crash. That
	// counter is the assertion: it distinguishes "returned early" from "blew up
	// and was swallowed", which look identical from the outside.
	panicsBefore := promUtil.CollectAndCount(telemetry.MetricUnexpectedPanicTotal)

	project.executeShadowRequests(ctx, network, shadows, nil)
	project.executeShadowRequests(ctx, network, nil,
		servedResponse(t, network, "eth_blockNumber", `"0x64"`))

	require.False(t, node.sawMethod("eth_blockNumber"),
		"neither early exit may reach the candidate")
	require.Equal(t, panicsBefore, promUtil.CollectAndCount(telemetry.MetricUnexpectedPanicTotal),
		"the mirror panicked and recovered instead of returning early; "+
			"on a busy gateway that is a recovered panic per served request")
}
