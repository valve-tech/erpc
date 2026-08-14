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
// SCOPE — registration is not yet service. A registered family answers
// "probe this upstream" and "should this response rotate", which is what the
// health/tip/failover layers need. It does NOT yet make `architecture: btc`
// routable end to end: IsValidArchitecture (common/network.go),
// IsValidNetworkId (util/ids.go), Network.prepareRequest (erpc/networks.go)
// and the client factory (clients/registry.go) still enumerate architectures
// by hand. Those four gates are the next step, and each one is where a
// registry lookup replaces a switch.
import (
	_ "github.com/erpc/erpc/architecture/btc"
)
