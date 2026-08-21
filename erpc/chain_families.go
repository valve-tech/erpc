package erpc

// Chain-family registration.
//
// A family reaches the shared registry through its package init(), so a family
// nobody imports is dead code that its own unit tests still pass. This file is
// the single place the fork lists them, kept separate from erpc.go and init.go
// so an upstream change to either does not conflict with adding a chain.
//
// evm and svm are NOT listed here: upstream imports those packages directly
// for their concrete types (evm.EvmJsonRpcCache, svm.SvmJsonRpcCache), so they
// are already linked in.
//
// SCOPE — the four gates that used to enumerate architectures by hand are now
// registry lookups: IsValidArchitecture (common/network.go), IsValidNetworkId
// (util/ids.go, through util/network_id_shape.go), Network.prepareRequest
// (erpc/networks.go) and the client factory (clients/registry.go). So is
// NetworkConfig.NetworkId (common/config.go) and the lazy-network path in
// networks_registry.go. A registered family is validated, routed, given a
// client and prepared without any architecture name appearing in a switch.
//
// WHAT IS STILL NOT REGISTRY-DRIVEN — a btc UPSTREAM does not bootstrap.
// Upstream.detectFeatures (upstream/upstream.go) recognises only evm and svm
// and rejects everything else, so a btc upstream never reaches the pool and no
// btc request is ever forwarded. common.IsValidNetwork (common/validation.go)
// likewise still knows only evm and svm, so a btc network cannot be named in a
// provider's onlyNetworks/ignoreNetworks list. Those are the next gates.
import (
	_ "github.com/erpc/erpc/architecture/btc"
)
