package erpc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// This file is the in-process gRPC harness the erpc package was missing: a real
// GrpcServer on a loopback listener, a real grpc-go client, and a real JSON-RPC
// node underneath, so a test drives the whole path a BDS consumer drives.
//
// How to reuse it:
//
//	h := newGrpcHarness(t, nil)                        // server + client + node
//	h.node.reply("eth_getLogs", `[]`)                  // program the node
//	ctx, cancel := h.callCtx(nil)                      // bounded ctx + metadata
//	defer cancel()
//	resp, err := h.rpc.GetLogs(ctx, &evm.GetLogsRequest{})
//
// Pass a tune func to change the server or project config before the server is
// built (message-size limits, reflection off, a second project, TLS). Every
// listener, connection and goroutine is closed by t.Cleanup, and every call
// context carries a deadline, so a hung stream fails the test instead of the
// suite.

const (
	grpcTestChainID   = int64(123)
	grpcTestProjectID = "main"
	// grpcCallTimeout bounds every harness RPC. A gRPC client that parks on an
	// idle connection never cancels the server's request context, so the
	// deadline has to come from the client side.
	grpcCallTimeout = 20 * time.Second
)

// ---------------------------------------------------------------------------
// Fake JSON-RPC node
// ---------------------------------------------------------------------------

// grpcNodeReply is one programmed answer. Exactly one of result and rpcErr is
// used; status overrides the HTTP status code when non-zero.
type grpcNodeReply struct {
	result json.RawMessage
	rpcErr json.RawMessage
	status int
}

// grpcTestNode is a fake EVM JSON-RPC node. It always answers the state
// poller's probes so the upstream comes up healthy, and answers everything else
// from a table a test programs. An unprogrammed method gets a real
// "method not found", which is what a node does and what eRPC must classify.
type grpcTestNode struct {
	*httptest.Server

	mu      sync.Mutex
	log     []grpcNodeCall
	replies map[string]grpcNodeReply

	chainID        uint64
	latestBlock    uint64
	finalizedBlock uint64
	// dynamicBlocks makes the node answer eth_getBlockByNumber for ANY hex
	// number with a block of that number, which is what a range query needs.
	dynamicBlocks bool
}

// grpcNodeCall is one JSON-RPC body the node received.
type grpcNodeCall struct {
	method string
	params []interface{}
}

func newGrpcTestNode(t *testing.T) *grpcTestNode {
	return newGrpcTestNodeOnChain(t, uint64(grpcTestChainID))
}

// newGrpcTestNodeOnChain builds a node that answers eth_chainId for the given
// chain. eRPC cordons an upstream whose chain disagrees with its config, so a
// second network needs a node that really answers for it.
func newGrpcTestNodeOnChain(t *testing.T, chainID uint64) *grpcTestNode {
	t.Helper()
	n := &grpcTestNode{
		replies:        map[string]grpcNodeReply{},
		chainID:        chainID,
		latestBlock:    0x3e8,
		finalizedBlock: 0x3e0,
	}
	n.Server = httptest.NewServer(http.HandlerFunc(n.serve))
	t.Cleanup(n.Server.Close)
	return n
}

func (n *grpcTestNode) serve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Id     interface{}   `json:"id"`
		Method string        `json:"method"`
		Params []interface{} `json:"params"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	reply := n.route(req.Method, req.Params)

	w.Header().Set("Content-Type", "application/json")
	if reply.status != 0 && reply.status != http.StatusOK {
		w.WriteHeader(reply.status)
	}
	envelope := map[string]interface{}{"jsonrpc": "2.0", "id": req.Id}
	if reply.rpcErr != nil {
		envelope["error"] = reply.rpcErr
	} else {
		envelope["result"] = reply.result
	}
	_ = json.NewEncoder(w).Encode(envelope)
}

// route decides one answer. The poller's own reads of "latest" and "finalized"
// are answered from the node's own height whatever a test programmed, so
// programming eth_getBlockByNumber cannot accidentally break bootstrap.
func (n *grpcTestNode) route(method string, params []interface{}) grpcNodeReply {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.log = append(n.log, grpcNodeCall{method: method, params: params})

	if method == "eth_getBlockByNumber" && len(params) > 0 {
		tag, _ := params[0].(string)
		switch tag {
		case "latest":
			return grpcNodeReply{result: json.RawMessage(grpcBlockJSON(n.latestBlock, nil))}
		case "finalized":
			return grpcNodeReply{result: json.RawMessage(grpcBlockJSON(n.finalizedBlock, nil))}
		}
		if n.dynamicBlocks {
			var num uint64
			if _, err := fmt.Sscanf(tag, "0x%x", &num); err == nil {
				return grpcNodeReply{result: json.RawMessage(grpcBlockJSON(num, nil))}
			}
		}
	}

	if r, ok := n.replies[method]; ok {
		return r
	}

	switch method {
	case "eth_chainId":
		return grpcNodeReply{result: json.RawMessage(fmt.Sprintf(`"0x%x"`, n.chainID))}
	case "eth_syncing":
		return grpcNodeReply{result: json.RawMessage(`false`)}
	case "eth_blockNumber":
		return grpcNodeReply{result: json.RawMessage(fmt.Sprintf(`"0x%x"`, n.latestBlock))}
	}
	return grpcNodeReply{rpcErr: json.RawMessage(`{"code":-32601,"message":"the method ` + method + ` does not exist/is not available"}`)}
}

// replyBlockByNumber makes the node serve a block for every number asked for.
func (n *grpcTestNode) replyBlockByNumber() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.dynamicBlocks = true
}

// bumpHead moves the node's chain head on by one block.
func (n *grpcTestNode) bumpHead() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.latestBlock++
}

// latestHead reports the node's current head.
func (n *grpcTestNode) latestHead() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.latestBlock
}

// reply programs the raw JSON the node returns as the JSON-RPC result.
func (n *grpcTestNode) reply(method, result string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.replies[method] = grpcNodeReply{result: json.RawMessage(result)}
}

// replyError programs a JSON-RPC error object, optionally with an HTTP status.
func (n *grpcTestNode) replyError(method string, httpStatus, code int, message string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.replies[method] = grpcNodeReply{
		rpcErr: json.RawMessage(fmt.Sprintf(`{"code":%d,"message":%q}`, code, message)),
		status: httpStatus,
	}
}

// callCount reports how many times the node saw a method.
func (n *grpcTestNode) callCount(method string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	count := 0
	for _, c := range n.log {
		if c.method == method {
			count++
		}
	}
	return count
}

// lastParams returns the params of the last call to a method, or nil.
func (n *grpcTestNode) lastParams(method string) []interface{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	for i := len(n.log) - 1; i >= 0; i-- {
		if n.log[i].method == method {
			return n.log[i].params
		}
	}
	return nil
}

// firstParams lists the first parameter of every call to a method, rendered as
// a string. It is how a test proves WHICH block or hash an upstream was asked
// for, separately from the poller's own "latest"/"finalized" reads.
func (n *grpcTestNode) firstParams(method string) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, c := range n.log {
		if c.method != method || len(c.params) == 0 {
			continue
		}
		out = append(out, fmt.Sprint(c.params[0]))
	}
	return out
}

// grpcBlockJSON builds a block object complete enough for
// evm.JsonRpcBlock.ToProto to succeed.
func grpcBlockJSON(number uint64, transactions []string) string {
	txs := "[]"
	if len(transactions) > 0 {
		txs = "["
		for i, tx := range transactions {
			if i > 0 {
				txs += ","
			}
			txs += tx
		}
		txs += "]"
	}
	zero32 := fmt.Sprintf("0x%064x", 0)
	return fmt.Sprintf(`{
		"number":%q,
		"hash":%q,
		"parentHash":%q,
		"sha3Uncles":"0x1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347",
		"miner":"0x0000000000000000000000000000000000000000",
		"stateRoot":%q,
		"transactionsRoot":%q,
		"receiptsRoot":%q,
		"logsBloom":"0x%0512x",
		"difficulty":"0x0",
		"gasLimit":"0x5208",
		"gasUsed":"0x0",
		"timestamp":"0x1",
		"extraData":"0x",
		"mixHash":%q,
		"nonce":"0x0000000000000000",
		"baseFeePerGas":"0x7",
		"size":"0x1",
		"transactions":%s
	}`,
		fmt.Sprintf("0x%x", number),
		grpcBlockHash(number),
		grpcBlockHash(number-1),
		zero32, zero32, zero32,
		0,
		zero32,
		txs,
	)
}

// grpcBlockHash is the deterministic hash the fake node gives a block number.
func grpcBlockHash(number uint64) string {
	return fmt.Sprintf("0x%064x", number+0x1000)
}

// ---------------------------------------------------------------------------
// Server and client
// ---------------------------------------------------------------------------

// grpcTestUpstream points one EVM upstream at the fake node. Batching is off so
// one gRPC call maps to one JSON-RPC body the node can assert on.
func grpcTestUpstream(id, endpoint string) *common.UpstreamConfig {
	return &common.UpstreamConfig{
		Id:       id,
		Type:     common.UpstreamTypeEvm,
		Endpoint: endpoint,
		JsonRpc:  &common.JsonRpcUpstreamConfig{SupportsBatch: &common.FALSE},
		Evm:      &common.EvmUpstreamConfig{ChainId: grpcTestChainID},
	}
}

// grpcTestConfig builds a defaulted single-project config over the fake node.
// tune runs BEFORE SetDefaults, so a test can add projects, networks or servers
// and still get them defaulted; SetDefaults only fills what tune left nil.
func grpcTestConfig(t *testing.T, node *grpcTestNode, tune func(*common.Config)) *common.Config {
	t.Helper()
	host := "127.0.0.1"
	port := 0
	cfg := &common.Config{
		LogLevel: "ERROR",
		Server: &common.ServerConfig{
			HttpHostV4:  &host,
			ListenV4:    util.BoolPtr(true),
			HttpPortV4:  &port,
			GrpcEnabled: util.BoolPtr(true),
		},
		Projects: []*common.ProjectConfig{
			{
				Id:        grpcTestProjectID,
				Upstreams: []*common.UpstreamConfig{grpcTestUpstream("node1", node.URL)},
				Networks: []*common.NetworkConfig{
					{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: grpcTestChainID}},
				},
			},
		},
	}
	if tune != nil {
		tune(cfg)
	}
	require.NoError(t, cfg.SetDefaults(nil))
	return cfg
}

// startGrpcErpc boots a real ERPC over the config and prepares its networks.
func startGrpcErpc(t *testing.T, ctx context.Context, cfg *common.Config) *ERPC {
	t.Helper()
	lg := zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.ErrorLevel)
	instance, err := NewERPC(ctx, &lg, nil, nil, nil, cfg)
	require.NoError(t, err)
	instance.Bootstrap(ctx)

	for _, pcfg := range cfg.Projects {
		for _, ncfg := range pcfg.Networks {
			if ncfg.Evm == nil {
				continue
			}
			nwID := fmt.Sprintf("evm:%d", ncfg.Evm.ChainId)
			nw, err := instance.GetNetwork(ctx, pcfg.Id, nwID)
			require.NoError(t, err)
			require.NoError(t, nw.upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, nwID))
		}
	}
	return instance
}

// startGrpcServer builds a GrpcServer and serves it on an ephemeral loopback
// port, returning the server and its address. Port 0 keeps parallel agents on
// this machine from colliding. Cleanup stops the server and joins its
// goroutine, so a leaked listener fails this test instead of the suite.
func startGrpcServer(t *testing.T, ctx context.Context, srvCfg *common.ServerConfig, instance *ERPC) (*GrpcServer, string) {
	t.Helper()
	lg := zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.ErrorLevel)
	gs, err := NewGrpcServer(ctx, &lg, srvCfg, instance)
	require.NoError(t, err)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var closeOnce sync.Once
	closeListener := func() { closeOnce.Do(func() { _ = lis.Close() }) }

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = gs.server.Serve(lis)
	}()
	t.Cleanup(func() {
		gs.server.Stop()
		closeListener()
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			t.Error("the gRPC server did not stop within 10s")
		}
	})
	return gs, lis.Addr().String()
}

// dialGrpc opens a client connection. Extra options come last so a caller can
// replace the insecure credentials with TLS.
func dialGrpc(t *testing.T, addr string, opts ...grpc.DialOption) *grpc.ClientConn {
	t.Helper()
	all := append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, opts...)
	conn, err := grpc.NewClient(addr, all...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// grpcHarness is one server, one client and one node, wired together.
type grpcHarness struct {
	t    *testing.T
	ctx  context.Context
	cfg  *common.Config
	node *grpcTestNode
	erpc *ERPC
	gs   *GrpcServer
	addr string
	conn *grpc.ClientConn

	rpc    evm.RPCQueryServiceClient
	query  evm.QueryServiceClient
	stream evm.StreamServiceClient
}

func newGrpcHarness(t *testing.T, tune func(*common.Config)) *grpcHarness {
	t.Helper()
	// Other tests in this package install gock on the default transport. Clear
	// it so the harness reaches its own loopback node whatever ran before.
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	node := newGrpcTestNode(t)
	cfg := grpcTestConfig(t, node, tune)
	instance := startGrpcErpc(t, ctx, cfg)
	gs, addr := startGrpcServer(t, ctx, cfg.Server, instance)
	conn := dialGrpc(t, addr)

	return &grpcHarness{
		t:      t,
		ctx:    ctx,
		cfg:    cfg,
		node:   node,
		erpc:   instance,
		gs:     gs,
		addr:   addr,
		conn:   conn,
		rpc:    evm.NewRPCQueryServiceClient(conn),
		query:  evm.NewQueryServiceClient(conn),
		stream: evm.NewStreamServiceClient(conn),
	}
}

// callCtx returns a deadline-bounded outgoing context carrying the routing
// metadata. Entries in overrides replace the defaults; an empty value removes
// the key so a test can prove what a missing header does.
func (h *grpcHarness) callCtx(overrides map[string]string) (context.Context, context.CancelFunc) {
	md := map[string]string{
		"x-erpc-project":  grpcTestProjectID,
		"x-erpc-chain-id": fmt.Sprintf("%d", grpcTestChainID),
	}
	for k, v := range overrides {
		if v == "" {
			delete(md, k)
			continue
		}
		md[k] = v
	}
	ctx, cancel := context.WithTimeout(h.ctx, grpcCallTimeout)
	return metadata.NewOutgoingContext(ctx, metadata.New(md)), cancel
}

// ---------------------------------------------------------------------------
// TLS material
// ---------------------------------------------------------------------------

// grpcTestCert writes a self-signed certificate for 127.0.0.1 into a temp dir
// and returns the cert and key paths plus the PEM the client must trust.
func grpcTestCert(t *testing.T) (certFile, keyFile string, certPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)

	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	require.NoError(t, os.WriteFile(certFile, certPEM, 0o600))
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
	return certFile, keyFile, certPEM
}
