package erpc

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// The health check is what a load balancer or an orchestrator polls to decide
// whether this process may take traffic. Every test in this file drives one
// failure the operator cares about: the process is draining, no project is
// loaded, the requested network does not exist, or a node answers for the wrong
// chain. A health check that says "OK" through any of these is worse than no
// health check, because it removes the signal the operator would have acted on.

// probeLogger keeps the handler's chatter out of the test output.
func probeLogger() zerolog.Logger {
	return zerolog.New(io.Discard)
}

// chainIdProbeUpstream builds a single live upstream pointed at a fake node.
// checkEvmChainId takes upstreams directly, so a test can put one node on the
// wrong chain without building a whole project around it.
func chainIdProbeUpstream(t *testing.T, ctx context.Context, id, endpoint string, chainId int64) *upstream.Upstream {
	t.Helper()
	lg := probeLogger()
	clReg := clients.NewClientRegistry(&lg, "test", nil, upstream.NewCompositeJsonRpcErrorExtractor())
	rlr, err := upstream.NewRateLimitersRegistry(ctx, &common.RateLimiterConfig{}, &lg)
	require.NoError(t, err)
	mt := health.NewTracker(&lg, "test", 10*time.Second)
	ups, err := upstream.NewUpstream(ctx, "test", &common.UpstreamConfig{
		Id:       id,
		Type:     common.UpstreamTypeEvm,
		Endpoint: endpoint,
		Evm:      &common.EvmUpstreamConfig{ChainId: chainId},
	}, clReg, rlr, thirdparty.NewVendorsRegistry(), &lg, mt, nil)
	require.NoError(t, err)
	return ups
}

// detailsFor seeds the per-upstream detail map the way both callers of
// checkEvmChainId do, then hands it back for assertions.
func detailsFor(ups ...*upstream.Upstream) map[string]map[string]any {
	d := make(map[string]map[string]any, len(ups))
	for _, u := range ups {
		d[u.Id()] = map[string]any{"network": u.NetworkId()}
	}
	return d
}

// TestHealthCheck_ChainIdMismatchNamesBothChainIds is the whole reason the
// chain-identity probe exists. A node answering for another chain returns real,
// well-formed, wrong data, and the only way the operator learns which node and
// which chain is from this message.
func TestHealthCheck_ChainIdMismatchNamesBothChainIds(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	node := evmNode(t)
	node.chainIdHex = "0x1" // mainnet, while the config claims chain 123
	ups := chainIdProbeUpstream(t, ctx, "wrong-chain", node.URL, 123)

	details := detailsFor(ups)
	results := checkEvmChainId(ctx, []*upstream.Upstream{ups}, details, common.EvalEvmAllChainId)

	require.False(t, results.healthy, "a node on another chain must fail the health check")
	require.Equal(t, "ERROR", results.status)
	require.Contains(t, results.message, "chain id verification failed for 1 / 1 upstreams")

	d := details["wrong-chain"]
	require.Equal(t, "ERROR", d["status"])
	require.Equal(t, int64(123), d["expectedChainId"], "the operator must see what the config asked for")
	require.Equal(t, int64(1), d["actualChainId"], "the operator must see what the node answered")
	require.Equal(t, "chain id mismatch: expected 123, got 1", d["message"])
}

// TestHealthCheck_MatchingChainIdPasses is the other half of the same branch.
// Without it a probe that failed every upstream would still pass the test above.
func TestHealthCheck_MatchingChainIdPasses(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	node := evmNode(t) // answers 0x7b == 123
	ups := chainIdProbeUpstream(t, ctx, "right-chain", node.URL, 123)

	details := detailsFor(ups)
	results := checkEvmChainId(ctx, []*upstream.Upstream{ups}, details, common.EvalEvmAllChainId)

	require.True(t, results.healthy)
	require.Equal(t, "OK", results.status)
	require.Equal(t, "all 1 / 1 upstreams verified", results.message)
	require.Equal(t, "OK", details["right-chain"]["status"])
	require.Equal(t, int64(123), details["right-chain"]["actualChainId"])
}

// TestHealthCheck_AnyChainIdKeepsServingWhenOneNodeIsWrong pins the difference
// between the two chain-id strategies. With "any", one good node keeps the
// project in rotation and the message still names how many failed, so the
// operator can act without an outage. With "all", the same fleet is unhealthy.
func TestHealthCheck_AnyChainIdKeepsServingWhenOneNodeIsWrong(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	good := evmNode(t)
	bad := evmNode(t)
	bad.chainIdHex = "0x1"

	upGood := chainIdProbeUpstream(t, ctx, "good", good.URL, 123)
	upBad := chainIdProbeUpstream(t, ctx, "bad", bad.URL, 123)
	list := []*upstream.Upstream{upGood, upBad}

	anyRes := checkEvmChainId(ctx, list, detailsFor(list...), common.EvalEvmAnyChainId)
	require.True(t, anyRes.healthy, "one healthy node must keep the 'any' strategy serving")
	require.Equal(t, "OK", anyRes.status)
	require.Equal(t, "1 / 2 upstreams passed (1 failed)", anyRes.message,
		"the message must still count the failure, or the operator never learns about it")

	allRes := checkEvmChainId(ctx, list, detailsFor(list...), common.EvalEvmAllChainId)
	require.False(t, allRes.healthy, "the 'all' strategy must fail on the same fleet")
	require.Contains(t, allRes.message, "chain id verification failed for 1 / 2 upstreams")
}

// TestHealthCheck_NoEvmUpstreamsToVerify covers the case where the probe has
// nothing to probe — an operator who points an EVM chain-id strategy at a
// non-EVM fleet. Reporting OK here would claim a verification that never ran.
func TestHealthCheck_NoEvmUpstreamsToVerify(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	results := checkEvmChainId(ctx, []*upstream.Upstream{}, map[string]map[string]any{}, common.EvalEvmAllChainId)

	require.False(t, results.healthy)
	require.Equal(t, "ERROR", results.status)
	require.Equal(t, "no upstreams available for chain ID verification", results.message)
}

// healthProbeServer wires an HttpServer over a real, bootstrapped eRPC. The
// health check reads state that only network bootstrap produces — the prepared
// upstream list and the initializer status — so a hand-built registry would
// answer differently from production.
func healthProbeServer(t *testing.T, ctx context.Context, projectCfg *common.ProjectConfig, mode common.HealthCheckMode) *HttpServer {
	t.Helper()
	lg := probeLogger()

	cfg := &common.Config{
		Projects:     []*common.ProjectConfig{projectCfg},
		RateLimiters: &common.RateLimiterConfig{},
	}
	require.NoError(t, cfg.SetDefaults(nil))

	ssr, err := data.NewSharedStateRegistry(ctx, &lg, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "100MB"},
		},
	})
	require.NoError(t, err)

	instance, err := NewERPC(ctx, &lg, ssr, nil, nil, cfg)
	require.NoError(t, err)
	instance.Bootstrap(ctx)

	// Bootstrap prepares each configured network's upstreams asynchronously.
	// Wait for that to land so the probe reads a settled fleet.
	if len(projectCfg.Networks) > 0 {
		nw, err := instance.GetNetwork(ctx, projectCfg.Id, projectCfg.Networks[0].NetworkId())
		require.NoError(t, err)
		require.NoError(t, nw.upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, projectCfg.Networks[0].NetworkId()))
	}

	return &HttpServer{
		logger:         &lg,
		erpc:           instance,
		serverCfg:      &common.ServerConfig{IncludeErrorDetails: &common.TRUE},
		healthCheckCfg: &common.HealthCheckConfig{Mode: mode},
		draining:       &atomic.Bool{},
	}
}

// staticHealthProbeServer wires an HttpServer over a hand-built project. It
// exists for the provider-only case, where a real bootstrap would reach out to
// the provider's endpoint repository over the network.
func staticHealthProbeServer(t *testing.T, ctx context.Context, projectCfg *common.ProjectConfig, mode common.HealthCheckMode) *HttpServer {
	t.Helper()
	lg := probeLogger()
	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&lg, vr, nil, nil)
	require.NoError(t, err)
	ssr, err := data.NewSharedStateRegistry(ctx, &lg, &common.SharedStateConfig{
		ClusterKey: "test",
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
	})
	require.NoError(t, err)
	mtk := health.NewTracker(&lg, "test", time.Second)

	pp := &PreparedProject{
		Config:            projectCfg,
		upstreamsRegistry: upstream.NewUpstreamsRegistry(ctx, &lg, "", projectCfg.Upstreams, ssr, nil, vr, pr, nil, mtk, nil),
	}
	_ = pp.upstreamsRegistry.BootstrapAndWait(ctx)
	pp.networksRegistry = NewNetworksRegistry(pp, ctx, pp.upstreamsRegistry, mtk, nil, nil, nil, nil, &lg)

	return &HttpServer{
		logger: &lg,
		erpc: &ERPC{
			projectsRegistry: &ProjectsRegistry{
				preparedProjects: map[string]*PreparedProject{projectCfg.Id: pp},
			},
		},
		serverCfg:      &common.ServerConfig{IncludeErrorDetails: &common.TRUE},
		healthCheckCfg: &common.HealthCheckConfig{Mode: mode},
		draining:       &atomic.Bool{},
	}
}

// probeHealth calls the handler the way the HTTP router does and returns the
// status code and the body the caller would read.
func probeHealth(t *testing.T, ctx context.Context, s *HttpServer, projectId, architecture, chainId, rawQuery string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	startTime := time.Now()
	encoder := common.SonicCfg.NewEncoder(w)
	s.handleHealthCheck(
		ctx, w,
		&http.Request{Method: "GET", URL: &url.URL{Path: "/healthcheck", RawQuery: rawQuery}},
		&startTime,
		projectId, architecture, chainId,
		encoder,
		func(ctx context.Context, statusCode int, body error) {
			w.WriteHeader(statusCode)
			_ = encoder.Encode(map[string]string{"error": body.Error()})
		},
	)
	resp := w.Result()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

// TestHealthCheck_DrainingRefusesTraffic pins the shutdown contract. Once the
// process starts draining it must fail the check immediately, so the load
// balancer removes it before the in-flight requests end.
func TestHealthCheck_DrainingRefusesTraffic(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	node := evmNode(t)
	s := healthProbeServer(t, ctx, &common.ProjectConfig{
		Id:        "prod",
		Networks:  []*common.NetworkConfig{{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}},
		Upstreams: []*common.UpstreamConfig{evmUpstream("node-a", node.URL, 123)},
	}, common.HealthCheckModeSimple)

	code, body := probeHealth(t, ctx, s, "prod", "", "", "")
	require.Equal(t, http.StatusOK, code, "the fixture must be healthy before draining")
	require.Contains(t, body, "OK")

	s.draining.Store(true)
	code, body = probeHealth(t, ctx, s, "prod", "", "", "")
	require.Equal(t, http.StatusServiceUnavailable, code,
		"a draining process must fail the check, not keep taking traffic")
	require.Contains(t, body, "shutting down")
}

// TestHealthCheck_ReportsWhenNoProjectIsLoaded covers a config that parsed but
// bootstrapped nothing. The process is listening, so a TCP check passes; only
// this handler can tell the orchestrator the instance cannot serve.
func TestHealthCheck_ReportsWhenNoProjectIsLoaded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	lg := probeLogger()
	s := &HttpServer{
		logger: &lg,
		erpc: &ERPC{
			projectsRegistry: &ProjectsRegistry{preparedProjects: map[string]*PreparedProject{}},
		},
		serverCfg:      &common.ServerConfig{IncludeErrorDetails: &common.TRUE},
		healthCheckCfg: &common.HealthCheckConfig{Mode: common.HealthCheckModeSimple},
		draining:       &atomic.Bool{},
	}

	code, body := probeHealth(t, ctx, s, "", "", "", "")
	require.Equal(t, http.StatusServiceUnavailable, code,
		"an instance with no project must not answer 200; a load balancer reads the status code")
	require.Contains(t, body, "no projects found")
}

// TestHealthCheck_ReportsWhenErpcNeverInitialized covers the earliest failure:
// the server is up but eRPC itself never came together. The handler must say so
// instead of panicking on a nil registry.
func TestHealthCheck_ReportsWhenErpcNeverInitialized(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	lg := probeLogger()
	s := &HttpServer{
		logger:         &lg,
		erpc:           nil,
		serverCfg:      &common.ServerConfig{IncludeErrorDetails: &common.TRUE},
		healthCheckCfg: &common.HealthCheckConfig{Mode: common.HealthCheckModeSimple},
		draining:       &atomic.Bool{},
	}

	code, body := probeHealth(t, ctx, s, "prod", "", "", "")
	require.Equal(t, http.StatusServiceUnavailable, code,
		"an uninitialized instance must not answer 200; a load balancer reads the status code")
	require.Contains(t, body, "eRPC is not initialized")
}

// TestHealthCheck_NamesTheNetworkThatDoesNotExist covers the per-network probe
// path an orchestrator uses for one chain. Asking about a chain this project
// never configured must name that chain, or the operator checks the node when
// the mistake is in the URL.
func TestHealthCheck_NamesTheNetworkThatDoesNotExist(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	node := evmNode(t)
	s := healthProbeServer(t, ctx, &common.ProjectConfig{
		Id:        "prod",
		Networks:  []*common.NetworkConfig{{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}},
		Upstreams: []*common.UpstreamConfig{evmUpstream("node-a", node.URL, 123)},
	}, common.HealthCheckModeSimple)

	code, body := probeHealth(t, ctx, s, "prod", "evm", "999", "")
	require.Equal(t, http.StatusBadGateway, code,
		"a network the project does not serve must not report healthy")
	require.Contains(t, body, "network evm:999 not found")
}

// TestHealthCheck_ReportsAnUnparsableChainIdAsNotFound covers the path where the
// URL carries something that is not a number at all. The handler skips the
// upstream filter and the project ends up with no matching network, which is the
// answer the operator needs: this URL serves nothing.
func TestHealthCheck_ReportsAnUnparsableChainIdAsNotFound(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	node := evmNode(t)
	s := healthProbeServer(t, ctx, &common.ProjectConfig{
		Id:        "prod",
		Networks:  []*common.NetworkConfig{{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}},
		Upstreams: []*common.UpstreamConfig{evmUpstream("node-a", node.URL, 123)},
	}, common.HealthCheckModeSimple)

	code, body := probeHealth(t, ctx, s, "prod", "evm", "not-a-number", "")
	require.Equal(t, http.StatusBadGateway, code,
		"a chain id that is not a number must not report healthy")
	require.Contains(t, body, "network evm:not-a-number not found")
}

// TestHealthCheck_ProviderOnlyProjectIsHealthyBeforeItsFirstRequest pins the
// deliberate exception. A project configured only with providers has no
// upstreams until the first request creates them, and reporting ERROR there
// would keep a correct deployment out of rotation forever.
func TestHealthCheck_ProviderOnlyProjectIsHealthyBeforeItsFirstRequest(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s := staticHealthProbeServer(t, ctx, &common.ProjectConfig{
		Id: "prod",
		Providers: []*common.ProviderConfig{
			{Id: "repo", Vendor: "repository", Settings: common.VendorSettings{
				"repositoryUrl": "https://evm-public-endpoints.erpc.cloud",
			}},
		},
	}, common.HealthCheckModeVerbose)

	code, body := probeHealth(t, ctx, s, "prod", "", "", "")
	require.Equal(t, http.StatusOK, code,
		"a provider-only project must stay in rotation until its first request")
	require.Contains(t, body, "send first actual request to initialize the upstreams")
}

// verboseHealthReport is the shape an operator's tooling reads out of verbose
// mode: a per-project block with a per-upstream map inside it.
type verboseHealthReport struct {
	Status  string `json:"status"`
	Details map[string]struct {
		Status    string `json:"status"`
		Message   string `json:"message"`
		Upstreams map[string]struct {
			ExpectedChainId int64  `json:"expectedChainId"`
			ActualChainId   int64  `json:"actualChainId"`
			Status          string `json:"status"`
			Message         string `json:"message"`
		} `json:"upstreams"`
		Networks map[string]struct {
			Status string `json:"status"`
		} `json:"networks"`
	} `json:"details"`
}

// TestHealthCheck_VerboseModeReportsTheChainIdPerUpstream pins what the operator
// reads back from the chain-identity strategy across a whole project. The verdict
// alone is not enough: the report has to carry the expected and the actual chain
// id per upstream, because that pair is what tells the operator which node to
// repoint.
func TestHealthCheck_VerboseModeReportsTheChainIdPerUpstream(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	nodeA, nodeB := evmNode(t), evmNode(t)
	s := healthProbeServer(t, ctx, &common.ProjectConfig{
		Id:       "prod",
		Networks: []*common.NetworkConfig{{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}},
		Upstreams: []*common.UpstreamConfig{
			evmUpstream("node-a", nodeA.URL, 123),
			evmUpstream("node-b", nodeB.URL, 123),
		},
	}, common.HealthCheckModeVerbose)

	code, body := probeHealth(t, ctx, s, "prod", "", "", "eval=all:evm:eth_chainId")
	require.Equal(t, http.StatusOK, code)

	var out verboseHealthReport
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	require.Equal(t, "OK", out.Status)

	prod, ok := out.Details["prod"]
	require.True(t, ok, "the report must name the project")
	require.Equal(t, "all 2 / 2 upstreams verified", prod.Message)

	for _, id := range []string{"node-a", "node-b"} {
		ups, ok := prod.Upstreams[id]
		require.True(t, ok, "the report must name upstream %s", id)
		require.Equal(t, int64(123), ups.ExpectedChainId, "upstream %s", id)
		require.Equal(t, int64(123), ups.ActualChainId,
			"upstream %s must report what the node answered, not the configured value repeated", id)
		require.Equal(t, "OK", ups.Status, "upstream %s", id)
	}

	require.Equal(t, "OK", prod.Networks["evm:123"].Status,
		"the per-network block is what a multi-chain operator reads first")
}

// TestHealthCheck_NetworkScopedProbeReportsOnlyThatNetwork covers the per-network
// URL an orchestrator points at one chain. The chain-id detail has to reach the
// report through the network-scoped path too, not only the project-wide one.
func TestHealthCheck_NetworkScopedProbeReportsOnlyThatNetwork(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	node := evmNode(t)
	s := healthProbeServer(t, ctx, &common.ProjectConfig{
		Id:        "prod",
		Networks:  []*common.NetworkConfig{{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}},
		Upstreams: []*common.UpstreamConfig{evmUpstream("node-a", node.URL, 123)},
	}, common.HealthCheckModeVerbose)

	code, body := probeHealth(t, ctx, s, "prod", "evm", "123", "eval=all:evm:eth_chainId")
	require.Equal(t, http.StatusOK, code)

	var out verboseHealthReport
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	prod, ok := out.Details["prod"]
	require.True(t, ok)
	require.Equal(t, "all 1 / 1 upstreams verified", prod.Message)

	ups, ok := prod.Upstreams["node-a"]
	require.True(t, ok, "the network-scoped report must still name the upstream")
	require.Equal(t, int64(123), ups.ExpectedChainId)
	require.Equal(t, int64(123), ups.ActualChainId)
	require.Equal(t, "OK", ups.Status)
}
