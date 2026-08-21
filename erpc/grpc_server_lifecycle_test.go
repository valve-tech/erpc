package erpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"
)

// grpcMinimalServerConfig is the smallest defaulted server config NewGrpcServer
// accepts. tune runs before SetDefaults so it can set any field the defaults
// would otherwise fill.
func grpcMinimalServerConfig(t *testing.T, tune func(*common.ServerConfig)) *common.ServerConfig {
	t.Helper()
	host := "127.0.0.1"
	port := 0
	scfg := &common.ServerConfig{
		HttpHostV4:  &host,
		ListenV4:    util.BoolPtr(true),
		HttpPortV4:  &port,
		GrpcEnabled: util.BoolPtr(true),
	}
	if tune != nil {
		tune(scfg)
	}
	cfg := &common.Config{Server: scfg}
	require.NoError(t, cfg.SetDefaults(nil))
	return cfg.Server
}

func grpcTestLogger(t *testing.T) zerolog.Logger {
	return zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.ErrorLevel)
}

// ---------------------------------------------------------------------------
// grpcSharesHttpV4 — the decision that puts gRPC on the HTTP port
// ---------------------------------------------------------------------------

func TestGrpcSharesHttpV4_OnlyWhenBothListenersNameTheSameAddress(t *testing.T) {
	host := func(s string) *string { return &s }
	port := func(p int) *int { return &p }

	cases := []struct {
		name string
		cfg  *common.ServerConfig
		want bool
	}{
		{"nil config", nil, false},
		{"grpc not configured", &common.ServerConfig{}, false},
		{"grpc disabled", &common.ServerConfig{GrpcEnabled: util.BoolPtr(false)}, false},
		{
			"ipv4 listener off",
			&common.ServerConfig{
				GrpcEnabled: util.BoolPtr(true), ListenV4: util.BoolPtr(false),
				HttpHostV4: host("127.0.0.1"), HttpPortV4: port(4000),
				GrpcHostV4: host("127.0.0.1"), GrpcPortV4: port(4000),
			},
			false,
		},
		{
			"grpc port unset",
			&common.ServerConfig{
				GrpcEnabled: util.BoolPtr(true), ListenV4: util.BoolPtr(true),
				HttpHostV4: host("127.0.0.1"), HttpPortV4: port(4000),
				GrpcHostV4: host("127.0.0.1"),
			},
			false,
		},
		{
			"different port",
			&common.ServerConfig{
				GrpcEnabled: util.BoolPtr(true), ListenV4: util.BoolPtr(true),
				HttpHostV4: host("127.0.0.1"), HttpPortV4: port(4000),
				GrpcHostV4: host("127.0.0.1"), GrpcPortV4: port(4001),
			},
			false,
		},
		{
			"different host",
			&common.ServerConfig{
				GrpcEnabled: util.BoolPtr(true), ListenV4: util.BoolPtr(true),
				HttpHostV4: host("127.0.0.1"), HttpPortV4: port(4000),
				GrpcHostV4: host("0.0.0.0"), GrpcPortV4: port(4000),
			},
			false,
		},
		{
			"same host and port",
			&common.ServerConfig{
				GrpcEnabled: util.BoolPtr(true), ListenV4: util.BoolPtr(true),
				HttpHostV4: host("127.0.0.1"), HttpPortV4: port(4000),
				GrpcHostV4: host("127.0.0.1"), GrpcPortV4: port(4000),
			},
			true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, grpcSharesHttpV4(tc.cfg))
		})
	}
}

// ---------------------------------------------------------------------------
// NewGrpcServer — trusted forwarders, reflection, TLS
// ---------------------------------------------------------------------------

// TestGrpcNewServer_KeepsOnlyParsableTrustedForwarders is the security-relevant
// half of the config: a typo in one entry must not silently promote every peer
// to trusted, and must not drop the entries that ARE valid.
func TestGrpcNewServer_KeepsOnlyParsableTrustedForwarders(t *testing.T) {
	scfg := grpcMinimalServerConfig(t, func(c *common.ServerConfig) {
		c.TrustedIPForwarders = []string{
			"  ", // blank, skipped
			"10.0.0.0/8",
			"not-a-cidr/8", // invalid CIDR, warned and dropped
			"203.0.113.7",
			"999.999.999.999", // invalid IP, warned and dropped
		}
		c.TrustedIPHeaders = []string{"", "  X-Forwarded-For  "}
	})
	lg := grpcTestLogger(t)

	gs, err := NewGrpcServer(context.Background(), &lg, scfg, nil)
	require.NoError(t, err)

	require.Len(t, gs.trustedForwarderNets, 1, "only the parsable CIDR survives")
	assert.Equal(t, "10.0.0.0/8", gs.trustedForwarderNets[0].String())
	assert.Equal(t, map[string]struct{}{"203.0.113.7": {}}, gs.trustedForwarderIPs)
	assert.Equal(t, []string{"x-forwarded-for"}, gs.trustedIPHeaders,
		"headers are trimmed and lower-cased so gRPC metadata keys match")

	assert.True(t, gs.isTrustedForwarder(net.ParseIP("10.1.2.3")), "inside the CIDR")
	assert.True(t, gs.isTrustedForwarder(net.ParseIP("203.0.113.7")), "the literal IP")
	assert.False(t, gs.isTrustedForwarder(net.ParseIP("203.0.113.8")), "a neighbour is not trusted")
	assert.False(t, gs.isTrustedForwarder(net.ParseIP("999.999.999.999")), "the rejected entry is not trusted")
	assert.False(t, gs.isTrustedForwarder(nil))
}

// TestGrpcReflection_IsServedByDefault and its off twin decide whether grpcurl
// and Postman can discover the BDS services without the .proto files.
func TestGrpcReflection_IsServedByDefault(t *testing.T) {
	h := newGrpcHarness(t, nil)
	require.Contains(t, h.gs.server.GetServiceInfo(), "grpc.reflection.v1.ServerReflection")

	ctx, cancel := context.WithTimeout(h.ctx, grpcCallTimeout)
	defer cancel()
	services := grpcListServices(t, ctx, h.conn)
	assert.Contains(t, services, "bds.evm.RPCQueryService")
	assert.Contains(t, services, "bds.evm.QueryService")
	assert.Contains(t, services, "bds.evm.StreamService")
}

func TestGrpcReflection_CanBeTurnedOff(t *testing.T) {
	h := newGrpcHarness(t, func(c *common.Config) {
		c.Server.GrpcReflection = util.BoolPtr(false)
	})
	require.NotContains(t, h.gs.server.GetServiceInfo(), "grpc.reflection.v1.ServerReflection")

	ctx, cancel := context.WithTimeout(h.ctx, grpcCallTimeout)
	defer cancel()
	stream, err := reflectionpb.NewServerReflectionClient(h.conn).ServerReflectionInfo(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&reflectionpb.ServerReflectionRequest{
		MessageRequest: &reflectionpb.ServerReflectionRequest_ListServices{ListServices: ""},
	}))
	_, err = stream.Recv()
	require.Error(t, err)
	assert.Equal(t, codes.Unimplemented, status.Code(err),
		"with reflection off the service must be absent, not empty")

	// The BDS services themselves must still be served.
	callCtx, callCancel := h.callCtx(nil)
	defer callCancel()
	resp, err := h.rpc.ChainId(callCtx, &evm.ChainIdRequest{})
	require.NoError(t, err)
	assert.Equal(t, uint64(grpcTestChainID), resp.ChainId)
}

// grpcListServices asks the reflection service what it serves.
func grpcListServices(t *testing.T, ctx context.Context, conn *grpc.ClientConn) []string {
	t.Helper()
	stream, err := reflectionpb.NewServerReflectionClient(conn).ServerReflectionInfo(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&reflectionpb.ServerReflectionRequest{
		MessageRequest: &reflectionpb.ServerReflectionRequest_ListServices{ListServices: ""},
	}))
	resp, err := stream.Recv()
	require.NoError(t, err)
	require.NoError(t, stream.CloseSend())

	list := resp.GetListServicesResponse()
	require.NotNil(t, list)
	names := make([]string, 0, len(list.Service))
	for _, s := range list.Service {
		names = append(names, s.Name)
	}
	return names
}

// TestGrpcTLS_ServesTheBdsServicesOverTls proves the credentials are really
// installed: a plaintext client must fail against the same listener.
func TestGrpcTLS_ServesTheBdsServicesOverTls(t *testing.T) {
	certFile, keyFile, certPEM := grpcTestCert(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	node := newGrpcTestNode(t)
	cfg := grpcTestConfig(t, node, func(c *common.Config) {
		c.Server.TLS = &common.TLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile}
	})
	instance := startGrpcErpc(t, ctx, cfg)
	_, addr := startGrpcServer(t, ctx, cfg.Server, instance)

	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(certPEM))
	conn := dialGrpc(t, addr, grpc.WithTransportCredentials(
		credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: "127.0.0.1"}),
	))

	callCtx, callCancel := context.WithTimeout(ctx, grpcCallTimeout)
	defer callCancel()
	callCtx = metadata.NewOutgoingContext(callCtx, metadata.New(map[string]string{
		"x-erpc-project":  grpcTestProjectID,
		"x-erpc-chain-id": fmt.Sprintf("%d", grpcTestChainID),
	}))
	resp, err := evm.NewRPCQueryServiceClient(conn).ChainId(callCtx, &evm.ChainIdRequest{})
	require.NoError(t, err)
	assert.Equal(t, uint64(grpcTestChainID), resp.ChainId)

	// The same address must refuse a plaintext client, which is what proves the
	// certificate is enforced rather than merely loaded.
	plain := dialGrpc(t, addr)
	plainCtx, plainCancel := context.WithTimeout(ctx, 5*time.Second)
	defer plainCancel()
	plainCtx = metadata.NewOutgoingContext(plainCtx, metadata.New(map[string]string{
		"x-erpc-project":  grpcTestProjectID,
		"x-erpc-chain-id": fmt.Sprintf("%d", grpcTestChainID),
	}))
	_, err = evm.NewRPCQueryServiceClient(plain).ChainId(plainCtx, &evm.ChainIdRequest{})
	require.Error(t, err, "a plaintext client must not reach a TLS listener")
}

func TestGrpcTLS_RefusesToStartWithAnUnreadableCertificate(t *testing.T) {
	scfg := grpcMinimalServerConfig(t, func(c *common.ServerConfig) {
		c.TLS = &common.TLSConfig{
			Enabled:  true,
			CertFile: "/nonexistent/erpc-grpc-cert.pem",
			KeyFile:  "/nonexistent/erpc-grpc-key.pem",
		}
	})
	lg := grpcTestLogger(t)

	gs, err := NewGrpcServer(context.Background(), &lg, scfg, nil)
	require.Error(t, err, "a missing certificate must stop the server, not start it in plaintext")
	assert.Nil(t, gs)
	assert.Contains(t, err.Error(), "no such file or directory")
}

// ---------------------------------------------------------------------------
// Start — binding and shutdown
// ---------------------------------------------------------------------------

// TestGrpcStart_StopsWhenTheAppContextIsCancelled covers the shutdown contract:
// Start blocks while serving and returns cleanly once the app context is done.
func TestGrpcStart_StopsWhenTheAppContextIsCancelled(t *testing.T) {
	port := grpcFreePort(t)
	scfg := grpcMinimalServerConfig(t, func(c *common.ServerConfig) {
		c.GrpcHostV4 = util.StringPtr("127.0.0.1")
		c.GrpcPortV4 = &port
	})
	lg := grpcTestLogger(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gs, err := NewGrpcServer(ctx, &lg, scfg, nil)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- gs.Start(&lg) }()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	require.Eventually(t, func() bool {
		c, derr := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if derr != nil {
			return false
		}
		_ = c.Close()
		return true
	}, 10*time.Second, 20*time.Millisecond, "Start never began accepting on %s", addr)

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err, "a graceful stop must not look like a serving failure")
	case <-time.After(10 * time.Second):
		gs.server.Stop()
		t.Fatal("Start did not return after the app context was cancelled")
	}
}

// TestGrpcStart_ReportsAPortItCannotBind is the operator-visible half: eRPC must
// name the address it failed on rather than exit silently.
func TestGrpcStart_ReportsAPortItCannotBind(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer occupied.Close()
	port := occupied.Addr().(*net.TCPAddr).Port

	scfg := grpcMinimalServerConfig(t, func(c *common.ServerConfig) {
		c.GrpcHostV4 = util.StringPtr("127.0.0.1")
		c.GrpcPortV4 = &port
	})
	lg := grpcTestLogger(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gs, err := NewGrpcServer(ctx, &lg, scfg, nil)
	require.NoError(t, err)
	defer gs.server.Stop()

	err = gs.Start(&lg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gRPC: failed to listen on")
	assert.Contains(t, err.Error(), fmt.Sprintf("127.0.0.1:%d", port),
		"the message must name the address so an operator can fix the conflict")
}

// grpcFreePort reserves and releases an ephemeral port so Start can bind it.
func grpcFreePort(t *testing.T) int {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := lis.Addr().(*net.TCPAddr).Port
	require.NoError(t, lis.Close())
	return port
}

// ---------------------------------------------------------------------------
// Message-size limits
// ---------------------------------------------------------------------------

func TestGrpcMessageSize_RejectsARequestOverTheReceiveLimit(t *testing.T) {
	h := newGrpcHarness(t, func(c *common.Config) {
		c.Server.GrpcMaxRecvMsgSize = util.IntPtr(64)
	})
	ctx, cancel := h.callCtx(nil)
	defer cancel()

	_, err := h.rpc.GetBlockByNumber(ctx, &evm.GetBlockByNumberRequest{
		BlockNumber: "0x" + strings.Repeat("a", 4096),
	})
	require.Error(t, err)
	assert.Equal(t, codes.ResourceExhausted, status.Code(err))
	assert.Contains(t, status.Convert(err).Message(), "larger than max")
	assert.Zero(t, h.node.callCount("eth_getBlockByNumber_user"),
		"an oversized request must be refused before any upstream is contacted")
}

func TestGrpcMessageSize_RefusesToSendAResponseOverTheSendLimit(t *testing.T) {
	h := newGrpcHarness(t, func(c *common.Config) {
		c.Server.GrpcMaxSendMsgSize = util.IntPtr(8)
	})
	h.node.reply("eth_getBlockByNumber", grpcBlockJSON(0x64, nil))

	ctx, cancel := h.callCtx(nil)
	defer cancel()
	_, err := h.rpc.GetBlockByNumber(ctx, &evm.GetBlockByNumberRequest{BlockNumber: "0x64"})
	require.Error(t, err)
	assert.Equal(t, codes.ResourceExhausted, status.Code(err))
	assert.Contains(t, status.Convert(err).Message(), "larger than max")
}

// ---------------------------------------------------------------------------
// Error taxonomy → gRPC status codes
// ---------------------------------------------------------------------------

func TestGrpcMapToGRPCStatus_MapsEachErrorClassToItsCode(t *testing.T) {
	gs := &GrpcServer{}
	assert.NoError(t, gs.mapToGRPCStatus(nil))

	cases := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"unsupported method", common.NewErrEndpointUnsupported(errors.New("no such method")), codes.Unimplemented},
		{"unauthorized endpoint", common.NewErrEndpointUnauthorized(errors.New("bad key")), codes.Unauthenticated},
		{"endpoint timeout", common.NewErrEndpointRequestTimeout(time.Second, errors.New("slow")), codes.DeadlineExceeded},
		{"capacity exceeded", common.NewErrEndpointCapacityExceeded(errors.New("429")), codes.ResourceExhausted},
		{"request too large", common.NewErrEndpointRequestTooLarge(errors.New("range"), common.EvmBlockRangeTooLarge), codes.ResourceExhausted},
		{"missing data", common.NewErrEndpointMissingData(errors.New("pruned"), nil), codes.NotFound},
		{"client-side exception", common.NewErrEndpointClientSideException(errors.New("reverted")), codes.InvalidArgument},
		{"anything else", errors.New("boom"), codes.Internal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mapped := gs.mapToGRPCStatus(tc.err)
			require.Error(t, mapped)
			st := status.Convert(mapped)
			assert.Equal(t, tc.want, st.Code())
			assert.Contains(t, st.Message(), tc.err.Error(),
				"the original error text must survive so an operator can triage it")
		})
	}
}

// TestGrpcMapToGRPCStatus_LooksThroughEuRpcsOwnWrappers covers the shape a real
// request path produces: the endpoint error is nested inside eRPC's own error
// types by the time a handler sees it, and the code must still be found.
func TestGrpcMapToGRPCStatus_LooksThroughEuRpcsOwnWrappers(t *testing.T) {
	gs := &GrpcServer{}
	nested := common.NewErrUpstreamRequest(
		common.NewErrEndpointUnsupported(errors.New("no such method")),
		nil, "evm:123", "eth_getLogs", time.Second, 1, 0, 0,
	)
	assert.Equal(t, codes.Unimplemented, status.Code(gs.mapToGRPCStatus(nested)))
}

// ---------------------------------------------------------------------------
// Panic recovery
// ---------------------------------------------------------------------------

func TestGrpcPanicRecovery_TurnsAUnaryPanicIntoInternal(t *testing.T) {
	lg := grpcTestLogger(t)
	gs := &GrpcServer{logger: &lg}

	resp, err := gs.panicRecoveryUnary()(
		context.Background(), &evm.ChainIdRequest{},
		&grpc.UnaryServerInfo{FullMethod: "/evm.RPCQueryService/ChainId"},
		func(ctx context.Context, req interface{}) (interface{}, error) {
			panic("handler exploded")
		},
	)
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Equal(t, codes.Internal, status.Code(err))
	assert.Equal(t, "internal server error", status.Convert(err).Message(),
		"the panic value must not leak to the client")
}

func TestGrpcPanicRecovery_PassesAUnaryResultThroughUntouched(t *testing.T) {
	lg := grpcTestLogger(t)
	gs := &GrpcServer{logger: &lg}

	want := &evm.ChainIdResponse{ChainId: 7}
	resp, err := gs.panicRecoveryUnary()(
		context.Background(), &evm.ChainIdRequest{},
		&grpc.UnaryServerInfo{FullMethod: "/evm.RPCQueryService/ChainId"},
		func(ctx context.Context, req interface{}) (interface{}, error) { return want, nil },
	)
	require.NoError(t, err)
	assert.Same(t, want, resp)
}

func TestGrpcPanicRecovery_TurnsAStreamPanicIntoInternal(t *testing.T) {
	lg := grpcTestLogger(t)
	gs := &GrpcServer{logger: &lg}

	err := gs.panicRecoveryStream()(
		nil, nil,
		&grpc.StreamServerInfo{FullMethod: "/evm.QueryService/QueryBlocks"},
		func(srv interface{}, ss grpc.ServerStream) error { panic("stream exploded") },
	)
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
	assert.Equal(t, "internal server error", status.Convert(err).Message())
}

func TestGrpcPanicRecovery_PassesAStreamErrorThroughUntouched(t *testing.T) {
	lg := grpcTestLogger(t)
	gs := &GrpcServer{logger: &lg}

	want := status.Error(codes.NotFound, "no such block")
	err := gs.panicRecoveryStream()(
		nil, nil,
		&grpc.StreamServerInfo{FullMethod: "/evm.QueryService/QueryBlocks"},
		func(srv interface{}, ss grpc.ServerStream) error { return want },
	)
	require.Equal(t, want, err, "a real stream error must not be rewritten as Internal")
}

// ---------------------------------------------------------------------------
// extractRequestInput — what the handlers read off the wire
// ---------------------------------------------------------------------------

func TestGrpcExtractRequestInput_RejectsACallWithNoMetadata(t *testing.T) {
	gs := &GrpcServer{}
	_, err := gs.extractRequestInput(context.Background(), "eth_chainId", &evm.ChainIdRequest{})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Equal(t, "missing metadata", status.Convert(err).Message())
}

func TestGrpcExtractRequestInput_CarriesTheClientIdentityAndDefaultsTheArchitecture(t *testing.T) {
	gs := &GrpcServer{
		trustedForwarderIPs: map[string]struct{}{"127.0.0.1": {}},
		trustedIPHeaders:    []string{"x-forwarded-for"},
	}
	md := metadata.New(map[string]string{
		"x-erpc-project":  "main",
		"x-erpc-chain-id": "123",
		"user-agent":      "bds-client/1.2",
		"x-erpc-user-id":  "tenant-42",
		"x-forwarded-for": "203.0.113.9",
	})
	ctx := peer.NewContext(metadata.NewIncomingContext(context.Background(), md), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5000},
	})

	input, err := gs.extractRequestInput(ctx, "eth_chainId", &evm.ChainIdRequest{})
	require.NoError(t, err)
	assert.Equal(t, "main", input.ProjectId)
	assert.Equal(t, "123", input.ChainId)
	assert.Equal(t, "evm", input.Architecture, "an absent architecture falls back to evm")
	assert.Equal(t, "bds-client/1.2", input.UserAgent)
	assert.Equal(t, "tenant-42", input.TrustedUserId)
	assert.Equal(t, "203.0.113.9", input.ClientIP)
	require.NotNil(t, input.AuthPayload)
	assert.Equal(t, "eth_chainId", input.AuthPayload.Method)
}

func TestGrpcExtractRequestInput_HonoursAnExplicitArchitecture(t *testing.T) {
	gs := &GrpcServer{}
	md := metadata.New(map[string]string{
		"x-erpc-project":      "main",
		"x-erpc-chain-id":     "mainnet",
		"x-erpc-architecture": "btc",
	})
	input, err := gs.extractRequestInput(metadata.NewIncomingContext(context.Background(), md), "eth_chainId", &evm.ChainIdRequest{})
	require.NoError(t, err)
	assert.Equal(t, "btc", input.Architecture)
}
