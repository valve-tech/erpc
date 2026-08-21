package erpc

import (
	"context"
	"strings"
	"testing"

	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// How far `architecture: btc` gets, proved on the real gates rather than on
// fakes. Everything here runs in package erpc, which links evm, svm AND btc,
// so the answers are the ones a running eRPC would give.
//
// WHERE THE PROOF STOPS: no bitcoind is contacted. This covers config
// acceptance, network-id derivation, network construction, request preparation
// and client construction. It does NOT cover upstream bootstrap
// (Upstream.detectFeatures still recognises only evm and svm) or a forwarded
// response. See the report and erpc/chain_families.go.

func btcNetworkConfig() *common.NetworkConfig {
	return &common.NetworkConfig{Architecture: "btc", Chain: "mainnet"}
}

func TestBtc_ArchitectureIsValid(t *testing.T) {
	// The URL gate: /main/btc/mainnet is rejected here before anything else
	// runs.
	if !common.IsValidArchitecture("btc") {
		t.Fatal("architecture btc is not valid; every btc URL is refused at the door")
	}
	// The shipping architectures must keep answering — this is the regression
	// that would matter most.
	for _, arch := range []string{"evm", "svm"} {
		if !common.IsValidArchitecture(arch) {
			t.Fatalf("architecture %s stopped being valid", arch)
		}
	}
	// Negative control: the gate must not have become "everything is valid".
	if common.IsValidArchitecture("doge") {
		t.Fatal("an unregistered architecture validated")
	}
}

func TestBtc_NetworkIdIsValidAndDerivedFromConfig(t *testing.T) {
	if !util.IsValidNetworkId("btc:mainnet") {
		t.Fatal("util.IsValidNetworkId(btc:mainnet) = false; the networks registry " +
			"refuses the id before it reads any config")
	}
	if got := btcNetworkConfig().NetworkId(); got != "btc:mainnet" {
		t.Fatalf("NetworkId() = %q, want btc:mainnet; a network with no id is stored "+
			"under the empty string and never found again", got)
	}
	// Negative controls. An unregistered architecture must not mint an id, and
	// neither must a registered one with nothing to name.
	if got := (&common.NetworkConfig{Architecture: "doge", Chain: "mainnet"}).NetworkId(); got != "" {
		t.Fatalf("NetworkId() = %q for an unregistered architecture, want empty", got)
	}
	if got := (&common.NetworkConfig{Architecture: "btc"}).NetworkId(); got != "" {
		t.Fatalf("NetworkId() = %q with no chain named, want empty", got)
	}
	// evm and svm keep deriving their ids from their own config blocks.
	evmCfg := &common.NetworkConfig{Architecture: "evm", Evm: &common.EvmNetworkConfig{ChainId: 42161}}
	if got := evmCfg.NetworkId(); got != "evm:42161" {
		t.Fatalf("evm NetworkId() = %q, want evm:42161", got)
	}
	svmCfg := &common.NetworkConfig{Architecture: "svm", Svm: &common.SvmNetworkConfig{Cluster: "mainnet-beta"}}
	if got := svmCfg.NetworkId(); got != "svm:mainnet-beta" {
		t.Fatalf("svm NetworkId() = %q, want svm:mainnet-beta", got)
	}
}

func TestBtc_NetworkConfigPassesValidation(t *testing.T) {
	cfg := btcNetworkConfig()
	if err := cfg.Validate(&common.Config{}); err != nil {
		t.Fatalf("a btc network config was rejected by validation: %v", err)
	}
}

func TestBtc_NetworkBuildsWithItsArchitectureHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, err := NewNetwork(ctx, &log.Logger, "test", btcNetworkConfig(), nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewNetwork for a btc network: %v", err)
	}
	if network.Id() != "btc:mainnet" {
		t.Fatalf("network id = %q, want btc:mainnet", network.Id())
	}
	if network.Architecture() != common.NetworkArchitecture("btc") {
		t.Fatalf("architecture = %q, want btc", network.Architecture())
	}
	// Without a handler the network exists but every pipeline hook is skipped
	// and prepareRequest refuses the request as an unsupported architecture.
	if network.architectureHandler == nil {
		t.Fatal("btc network built with no architecture handler")
	}
}

func TestBtc_PrepareRequestAcceptsBitcoindCallsUntouched(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, err := NewNetwork(ctx, &log.Logger, "test", btcNetworkConfig(), nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}

	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"1.0","id":1,"method":"getblockchaininfo","params":[]}`))
	if err := network.prepareRequest(ctx, req); err != nil {
		t.Fatalf("prepareRequest for a bitcoind call: %v", err)
	}
	jrq, err := req.JsonRpcRequest(ctx)
	if err != nil {
		t.Fatalf("JsonRpcRequest after prepareRequest: %v", err)
	}
	if jrq.Method != "getblockchaininfo" {
		t.Fatalf("method = %q after preparation, want getblockchaininfo", jrq.Method)
	}

	// A bitcoind call whose params look EVM-ish must come out unchanged. EVM's
	// normalizer pads and lower-cases block references; running it on another
	// chain's params would rewrite a Bitcoin block hash into something no node
	// can answer.
	req = common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"1.0","id":1,"method":"getblock","params":["0x1273C18"]}`))
	if err := network.prepareRequest(ctx, req); err != nil {
		t.Fatalf("prepareRequest: %v", err)
	}
	jrq, err = req.JsonRpcRequest(ctx)
	if err != nil {
		t.Fatalf("JsonRpcRequest: %v", err)
	}
	jrq.RLock()
	got, _ := jrq.Params[0].(string)
	jrq.RUnlock()
	if got != "0x1273C18" {
		t.Fatalf("param = %q, want it untouched at 0x1273C18 — another chain's "+
			"normalizer rewrote a btc parameter", got)
	}
}

func TestBtc_PrepareRequestRejectsAMalformedBody(t *testing.T) {
	// Negative control for the test above: prepareRequest must still be the
	// parse gate, not a pass-through that lets a broken body reach an upstream.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, err := NewNetwork(ctx, &log.Logger, "test", btcNetworkConfig(), nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}
	if err := network.prepareRequest(ctx, common.NewNormalizedRequest([]byte(`{not json`))); err == nil {
		t.Fatal("prepareRequest accepted a body that is not JSON-RPC")
	}
}

func TestBtc_PrepareRequestRefusesAnUnregisteredArchitecture(t *testing.T) {
	// The other negative control: an architecture with no handler must be
	// refused with a message that names it, not silently forwarded.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, err := NewNetwork(ctx, &log.Logger, "test",
		&common.NetworkConfig{Architecture: "doge", Chain: "mainnet"}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}
	err = network.prepareRequest(ctx, common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"1.0","id":1,"method":"getblockcount","params":[]}`)))
	if err == nil {
		t.Fatal("prepareRequest accepted a request for an architecture eRPC does not serve")
	}
	if !strings.Contains(err.Error(), "unsupported architecture") {
		t.Fatalf("error = %v, want it to name the unsupported architecture", err)
	}
}

func TestEvm_PrepareRequestStillNormalizes(t *testing.T) {
	// The EVM regression control for the same refactor. prepareRequest no
	// longer names architectures; EVM's normalization now runs through the
	// handler's optional RequestNormalizer. If that wiring breaks, EVM
	// requests keep working just enough to look fine — the block reference
	// never gets extracted, so cache keys, block-availability checks and gRPC
	// routing all silently lose their block number.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, err := NewNetwork(ctx, &log.Logger, "test",
		&common.NetworkConfig{Architecture: "evm", Evm: &common.EvmNetworkConfig{ChainId: 123}},
		nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewNetwork: %v", err)
	}

	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1273c18",false]}`))
	req.SetNetwork(network)
	if err := network.prepareRequest(ctx, req); err != nil {
		t.Fatalf("prepareRequest: %v", err)
	}
	got := req.EvmBlockNumber()
	if got == nil {
		t.Fatal("prepareRequest did not extract the evm block number; the EVM " +
			"normalizer is no longer wired into the request path")
	}
	if n, _ := got.(int64); n != 0x1273c18 {
		t.Fatalf("evm block number = %v, want %d", got, 0x1273c18)
	}
}

func TestBtc_UpstreamGetsAnHttpJsonRpcClient(t *testing.T) {
	// The client factory, on the real btc family rather than a fake one.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lg := zerolog.Nop()
	pools, err := clients.NewProxyPoolRegistry(nil, &lg)
	if err != nil {
		t.Fatalf("NewProxyPoolRegistry: %v", err)
	}
	registry := clients.NewClientRegistry(&lg, "test", pools, nil)

	ups := common.NewFakeUpstream("btc-node-1")
	ups.Config().Type = common.UpstreamType("btc")
	// bitcoind's default RPC port, with the basic-auth credentials it expects.
	ups.Config().Endpoint = "http://user:pass@btc-node-1.localhost:8332"

	client, err := registry.CreateClient(ctx, ups)
	if err != nil {
		t.Fatalf("CreateClient for a btc upstream: %v", err)
	}
	if client.GetType() != clients.ClientTypeHttpJsonRpc {
		t.Fatalf("client type = %v, want %v", client.GetType(), clients.ClientTypeHttpJsonRpc)
	}
}

func TestBtc_ResolveNetworkConfigBuildsALazyNetwork(t *testing.T) {
	// The URL path for a network nobody declared: /main/btc/mainnet must
	// produce a usable config, the same way /main/evm/42161 does.
	nr := &NetworksRegistry{project: &PreparedProject{
		Config: &common.ProjectConfig{Id: "main"},
	}}

	cfg, err := nr.resolveNetworkConfig("btc:mainnet")
	if err != nil {
		t.Fatalf("resolveNetworkConfig(btc:mainnet): %v", err)
	}
	if cfg.NetworkId() != "btc:mainnet" {
		t.Fatalf("lazily-created config has id %q, want btc:mainnet", cfg.NetworkId())
	}
	// Negative controls: an unregistered architecture and a body the family
	// rejects must not mint a config.
	if _, err := nr.resolveNetworkConfig("doge:mainnet"); err == nil {
		t.Fatal("resolveNetworkConfig accepted an unregistered architecture")
	}
	if _, err := nr.resolveNetworkConfig("btc:"); err == nil {
		t.Fatal("resolveNetworkConfig accepted a btc id with no chain name")
	}
}
