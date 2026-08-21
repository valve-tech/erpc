package erpc

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// The brief's acceptance test: several bitcoind upstreams at different heights
// behind ONE eRPC, a JSON-RPC request served, the most caught-up node chosen and
// a syncing node excluded.
//
// Nothing here is mocked above the wire. The three nodes are httptest servers
// speaking bitcoind's real envelope (JSON-RPC 1.0, an explicit `error: null`,
// HTTP 500 carrying an RPC error body); everything above them — config load and
// validation, upstream bootstrap, the health tracker, the default selection
// policy and the request path — is the production code a running eRPC uses.

// fakeBitcoindNode is a stand-in bitcoind for the full request path. It answers
// the probe (`getblockchaininfo`) from its own height AND a user call
// (`getbestblockhash`) with a value that names the node, so the served response
// proves WHICH upstream answered rather than only that something did.
//
// Its height and sync flag are atomics because a test changes them while the
// poller is reading them.
type fakeBitcoindNode struct {
	*httptest.Server

	id      string
	height  atomic.Int64
	headers atomic.Int64
	ibd     atomic.Bool

	mu      sync.Mutex
	methods []string
	// forwards counts user calls — everything except the probe — which is how
	// an excluded upstream proves it took no traffic.
	forwards atomic.Int64
}

func newFakeBitcoindNode(t *testing.T, id string, height, headers int64, ibd bool) *fakeBitcoindNode {
	t.Helper()
	n := &fakeBitcoindNode{id: id}
	n.height.Store(height)
	n.headers.Store(headers)
	n.ibd.Store(ibd)

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

		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)

		switch req.Method {
		case "getblockchaininfo":
			_ = enc.Encode(map[string]interface{}{
				"result": map[string]interface{}{
					"chain":                "main",
					"blocks":               n.height.Load(),
					"headers":              n.headers.Load(),
					"verificationprogress": 0.999999,
					"initialblockdownload": n.ibd.Load(),
				},
				"error": nil,
				"id":    req.Id,
			})
		case "getbestblockhash":
			n.forwards.Add(1)
			_ = enc.Encode(map[string]interface{}{
				"result": "besthash-from-" + n.id,
				"error":  nil,
				"id":     req.Id,
			})
		default:
			n.forwards.Add(1)
			// bitcoind's real failure shape: HTTP 500 carrying a JSON-RPC error.
			w.WriteHeader(http.StatusInternalServerError)
			_ = enc.Encode(map[string]interface{}{
				"result": nil,
				"error":  map[string]interface{}{"code": -32601, "message": "Method not found"},
				"id":     req.Id,
			})
		}
	}))
	t.Cleanup(n.Server.Close)
	return n
}

func (n *fakeBitcoindNode) probeCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	count := 0
	for _, m := range n.methods {
		if m == "getblockchaininfo" {
			count++
		}
	}
	return count
}

func btcUpstreamConfig(id, endpoint string, probeInterval time.Duration) *common.UpstreamConfig {
	return &common.UpstreamConfig{
		Id:                 id,
		Type:               common.UpstreamType("btc"),
		Endpoint:           endpoint,
		Chain:              "mainnet",
		ChainProbeInterval: common.Duration(probeInterval),
		JsonRpc:            &common.JsonRpcUpstreamConfig{SupportsBatch: &common.FALSE},
		// `probe: off` opts out of the default policy's probeExcluded step,
		// which mirrors sampled real traffic to EXCLUDED upstreams in the
		// background so they can earn their way back in. That mirroring is
		// sampled and asynchronous, so with it on "the excluded node received no
		// call" is a race rather than an invariant. It is also orthogonal to the
		// decision under test: the ordered set the request path reads is built
		// before any probe traffic exists.
		Routing: &common.UpstreamRoutingConfig{Probe: common.ProbeModeOff},
	}
}

// startBtcErpc builds a real eRPC over the given nodes and returns its
// btc:mainnet network.
func startBtcErpc(t *testing.T, ctx context.Context, probeInterval time.Duration, nodes ...*fakeBitcoindNode) *Network {
	t.Helper()
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	ups := make([]*common.UpstreamConfig, 0, len(nodes))
	for _, n := range nodes {
		ups = append(ups, btcUpstreamConfig(n.id, n.URL, probeInterval))
	}

	cfg := &common.Config{
		Projects: []*common.ProjectConfig{
			{
				Id:        "test",
				Networks:  []*common.NetworkConfig{{Architecture: "btc", Chain: "mainnet"}},
				Upstreams: ups,
			},
		},
	}
	if err := cfg.SetDefaults(nil); err != nil {
		t.Fatalf("config SetDefaults: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a btc project config was rejected by validation: %v", err)
	}

	lg := log.Logger.Level(zerolog.WarnLevel)
	ssr, err := data.NewSharedStateRegistry(ctx, &lg, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "100MB"},
		},
	})
	if err != nil {
		t.Fatalf("NewSharedStateRegistry: %v", err)
	}

	instance, err := NewERPC(ctx, &lg, ssr, nil, nil, cfg)
	if err != nil {
		t.Fatalf("NewERPC: %v", err)
	}
	instance.Bootstrap(ctx)

	nw, err := instance.GetNetwork(ctx, "test", "btc:mainnet")
	if err != nil {
		t.Fatalf("GetNetwork(btc:mainnet): %v", err)
	}
	if err := nw.upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, "btc:mainnet"); err != nil {
		t.Fatalf("PrepareUpstreamsForNetwork: %v — no btc upstream reached the pool", err)
	}
	return nw
}

// waitForPolicySet ticks the selection policy until the upstreams it offers the
// request path are exactly `want`, or fails naming what it settled on.
//
// A SET, not an order: which of two eligible upstreams ranks first is decided by
// measured latency between two local servers, which is noise. Membership is the
// question here — who may take traffic at all.
func waitForPolicySet(t *testing.T, nw *Network, want ...string) {
	t.Helper()
	sort.Strings(want)
	deadline := time.Now().Add(15 * time.Second)
	var got []string
	for time.Now().Before(deadline) {
		policy.TickForTest(nw.policyEngine, "btc:mainnet", "*")
		got = got[:0]
		for _, u := range nw.policyEngine.GetOrdered("btc:mainnet", "*", "*") {
			got = append(got, u.Id())
		}
		sort.Strings(got)
		if strings.Join(got, ",") == strings.Join(want, ",") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("selection policy settled on %v, want %v", got, want)
}

func TestBtc_ServesARequestFromTheMostCaughtUpNode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// tip is the caught-up node. lagging is healthy but 50 blocks behind, which
	// the default policy's blockNumberLagAbove(16) excludes. syncing is only ONE
	// block behind — lag cannot exclude it — so it proves the sync verdict
	// itself takes an upstream out of rotation.
	tip := newFakeBitcoindNode(t, "btc-tip", 900000, 900000, false)
	lagging := newFakeBitcoindNode(t, "btc-lagging", 899950, 899950, false)
	syncing := newFakeBitcoindNode(t, "btc-syncing", 899999, 900000, true)

	nw := startBtcErpc(t, ctx, time.Second, tip, lagging, syncing)

	// Only the caught-up node may take traffic.
	waitForPolicySet(t, nw, "btc-tip")

	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"1.0","id":1,"method":"getbestblockhash","params":[]}`))
	req.SetNetwork(nw)
	resp, err := nw.Forward(ctx, req)
	if err != nil {
		t.Fatalf("forwarding a bitcoind call through eRPC: %v", err)
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		t.Fatalf("JsonRpcResponse: %v", err)
	}
	if got := jrr.GetResultString(); got != `"besthash-from-btc-tip"` {
		t.Fatalf("served result = %s, want the caught-up node's answer", got)
	}

	if tip.forwards.Load() == 0 {
		t.Fatal("the caught-up node saw no user call, so nothing was really forwarded")
	}
	if n := syncing.forwards.Load(); n != 0 {
		t.Fatalf("the syncing node served %d user calls; it must be excluded", n)
	}
	if n := lagging.forwards.Load(); n != 0 {
		t.Fatalf("the lagging node served %d user calls; it must be excluded", n)
	}

	// Every node must still be probed, including the excluded ones — that is how
	// they get back in, and how an operator watches them catch up.
	for _, n := range []*fakeBitcoindNode{tip, lagging, syncing} {
		if n.probeCount() == 0 {
			t.Fatalf("node %s was never probed", n.id)
		}
	}
}

func TestBtc_ARecoveredNodeReturnsToRotation(t *testing.T) {
	// The other half of exclusion. A node that finishes its initial block
	// download must come back WITHOUT an operator touching anything, which only
	// happens if the probe keeps running after bootstrap.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tip := newFakeBitcoindNode(t, "btc-tip", 900000, 900000, false)
	recovering := newFakeBitcoindNode(t, "btc-recovering", 899999, 900000, true)

	nw := startBtcErpc(t, ctx, 200*time.Millisecond, tip, recovering)
	waitForPolicySet(t, nw, "btc-tip")

	probesBefore := recovering.probeCount()
	recovering.ibd.Store(false)
	recovering.height.Store(900000)

	waitForPolicySet(t, nw, "btc-tip", "btc-recovering")

	if recovering.probeCount() <= probesBefore {
		t.Fatal("the recovered node came back without being re-probed; " +
			"the poller is not running and something else lifted the cordon")
	}
}
