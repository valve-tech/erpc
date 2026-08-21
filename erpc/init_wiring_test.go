package erpc

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	bdsevm "github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// Init is the only function that reads the whole config and turns it into a
// running process. Everything below it has its own tests; Init itself decides
// which of those parts exist. A branch it skips is a feature the operator wrote
// into the config and never got: a cache that is configured but not wired means
// every request pays an upstream call, and a gRPC transport that is enabled but
// never started means a listener nobody can connect to. Neither shows up as an
// error — the process starts, serves HTTP, and reports healthy.
//
// The tests below run the real Init, then assert the effect from outside the
// process: an upstream that is called once for two identical requests, and a
// gRPC port that answers.

// freeTcpPort returns a loopback port that is free right now. Two agents on one
// machine cannot collide on a fixed port, and Init needs the number up front
// because it binds the address itself.
func freeTcpPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

// initTestProject is the one-upstream project both tests below serve.
func initTestProject() *common.ProjectConfig {
	return &common.ProjectConfig{
		Id: "main",
		Networks: []*common.NetworkConfig{
			{
				Architecture: common.ArchitectureEvm,
				Evm:          &common.EvmNetworkConfig{ChainId: 123},
			},
		},
		Upstreams: []*common.UpstreamConfig{
			{
				Id:       "rpc1",
				Type:     common.UpstreamTypeEvm,
				Endpoint: "http://rpc1.localhost",
				Evm:      &common.EvmUpstreamConfig{ChainId: 123},
			},
		},
	}
}

// runInit starts Init on its own goroutine and returns a stop function that
// cancels the context and reports how long Init took to return.
func runInit(t *testing.T, cfg *common.Config) (stop func() (time.Duration, error)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Init(ctx, cfg, log.Logger) }()
	return func() (time.Duration, error) {
		start := time.Now()
		cancel()
		select {
		case err := <-done:
			return time.Since(start), err
		case <-time.After(30 * time.Second):
			t.Fatal("Init did not return within 30s of its context being cancelled")
			return 0, nil
		}
	}
}

// postJsonRpc sends one JSON-RPC request to a running eRPC and returns the body.
// It retries while the listener is still coming up, because Init starts the HTTP
// server on a goroutine and returns no readiness signal.
func postJsonRpc(t *testing.T, url, body string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		req, err := http.NewRequest("POST", url, strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err == nil {
			defer resp.Body.Close()
			out, rerr := io.ReadAll(resp.Body)
			require.NoError(t, rerr)
			return string(out)
		}
		if time.Now().After(deadline) {
			t.Fatalf("eRPC never accepted a request on %s: %v", url, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestInit_ServesTheSecondCallFromTheCacheItsConfigNames proves Init wires the
// configured EVM cache into the eRPC it builds. The upstream answers the block
// once. If the cache were built and then dropped, the second request would go
// upstream, find no mock, and come back an error — so the assertion cannot pass
// on an unwired cache.
//
// The shared-state registry in the same config block is exercised on the way
// through; this test makes no claim about it beyond the process starting.
func TestInit_ServesTheSecondCallFromTheCacheItsConfigNames(t *testing.T) {
	mainMutex.Lock()
	defer mainMutex.Unlock()

	util.ResetGock()
	defer util.ResetGock()
	gock.EnableNetworking()
	defer gock.DisableNetworking()
	gock.NetworkingFilter(func(req *http.Request) bool {
		return strings.Split(req.URL.Host, ":")[0] == "localhost"
	})
	util.SetupMocksForEvmStatePoller()
	// Times(1): the block leaves eRPC for the upstream exactly once.
	defer util.AssertNoPendingMocks(t, 0)

	// 0x386053 sits far below the poller's finalized tip 0x11117777, so the
	// finalized cache policy applies to it.
	const blockResult = `{"number":"0x386053","hash":"0xfeed"}`
	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			b := util.SafeReadBody(r)
			return strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, "0x386053")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":` + blockResult + `}`))

	host := "localhost"
	port := freeTcpPort(t)
	cfg := &common.Config{
		LogLevel: "ERROR",
		Server: &common.ServerConfig{
			HttpHostV4: &host,
			ListenV4:   util.BoolPtr(true),
			HttpPortV4: &port,
			MaxTimeout: common.Duration(10 * time.Second).Ptr(),
		},
		Database: &common.DatabaseConfig{
			EvmJsonRpcCache: &common.CacheConfig{
				Connectors: []*common.ConnectorConfig{
					{
						Id:     "mem",
						Driver: common.DriverMemory,
						Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
					},
				},
				Policies: []*common.CachePolicyConfig{
					{
						Network:   "*",
						Method:    "*",
						Finality:  common.DataFinalityStateFinalized,
						Connector: "mem",
						TTL:       common.FixedDuration(5 * time.Minute),
					},
				},
			},
			SharedState: &common.SharedStateConfig{
				Connector: &common.ConnectorConfig{
					Driver: common.DriverMemory,
					Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
				},
			},
		},
		Projects:     []*common.ProjectConfig{initTestProject()},
		RateLimiters: &common.RateLimiterConfig{},
	}
	require.NoError(t, cfg.SetDefaults(nil))
	cfg.Server.ListenV4 = util.BoolPtr(true)
	cfg.Server.HttpPortV4 = &port

	stop := runInit(t, cfg)

	url := fmt.Sprintf("http://localhost:%d/main/evm/123", port)
	const body = `{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x386053",false]}`

	first := postJsonRpc(t, url, body)
	require.Contains(t, first, `"0x386053"`, "the first call must be served by the upstream")

	second := postJsonRpc(t, url, body)
	assert.Contains(t, second, `"0x386053"`,
		"the second call must be served from the cache Init wired, not from an upstream that has no reply left")
	assert.NotContains(t, second, `"error"`)

	_, err := stop()
	require.NoError(t, err)
}

// TestInit_StartsTheGrpcTransportAndHonoursWaitAfterShutdown covers the two
// branches an operator turns on for a fleet rollout. grpcEnabled on its own
// port has to produce a listener that answers; waitAfterShutdown has to delay
// the return so a load balancer can drain the node before the process leaves.
// Both are silent when they break: the process starts either way, and the only
// symptom of a missing drain window is dropped requests during a deploy.
func TestInit_StartsTheGrpcTransportAndHonoursWaitAfterShutdown(t *testing.T) {
	mainMutex.Lock()
	defer mainMutex.Unlock()

	util.ResetGock()
	defer util.ResetGock()
	gock.EnableNetworking()
	defer gock.DisableNetworking()
	gock.NetworkingFilter(func(req *http.Request) bool {
		host := strings.Split(req.URL.Host, ":")[0]
		return host == "localhost" || host == "127.0.0.1"
	})
	util.SetupMocksForEvmStatePoller()

	host := "localhost"
	grpcHost := "127.0.0.1"
	httpPort := freeTcpPort(t)
	grpcPort := freeTcpPort(t)
	const drain = 300 * time.Millisecond

	cfg := &common.Config{
		LogLevel: "ERROR",
		Server: &common.ServerConfig{
			HttpHostV4:        &host,
			ListenV4:          util.BoolPtr(true),
			HttpPortV4:        &httpPort,
			GrpcEnabled:       util.BoolPtr(true),
			GrpcHostV4:        &grpcHost,
			GrpcPortV4:        &grpcPort,
			MaxTimeout:        common.Duration(10 * time.Second).Ptr(),
			WaitAfterShutdown: common.Duration(drain).Ptr(),
		},
		Projects:     []*common.ProjectConfig{initTestProject()},
		RateLimiters: &common.RateLimiterConfig{},
	}
	require.NoError(t, cfg.SetDefaults(nil))
	cfg.Server.ListenV4 = util.BoolPtr(true)
	cfg.Server.HttpPortV4 = &httpPort
	cfg.Server.GrpcPortV4 = &grpcPort
	cfg.Server.WaitAfterShutdown = common.Duration(drain).Ptr()
	require.False(t, grpcSharesHttpV4(cfg.Server),
		"the gRPC listener must be its own, or Init takes the shared-port branch instead")

	stop := runInit(t, cfg)

	conn, err := grpc.NewClient(
		fmt.Sprintf("127.0.0.1:%d", grpcPort),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	defer conn.Close()

	rpc := bdsevm.NewRPCQueryServiceClient(conn)
	var chainResp *bdsevm.ChainIdResponse
	require.Eventually(t, func() bool {
		callCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		callCtx = metadata.NewOutgoingContext(callCtx, metadata.New(map[string]string{
			"x-erpc-project":  "main",
			"x-erpc-chain-id": "123",
		}))
		chainResp, err = rpc.ChainId(callCtx, &bdsevm.ChainIdRequest{})
		return err == nil
	}, 20*time.Second, 100*time.Millisecond,
		"the gRPC listener Init started never answered: %v", err)
	assert.Equal(t, uint64(123), chainResp.ChainId,
		"the gRPC transport must be wired to the eRPC instance Init built")

	took, err := stop()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, took, drain,
		"Init must hold the process open for waitAfterShutdown so the node can drain")
}
