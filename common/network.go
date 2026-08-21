package common

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

type NetworkArchitecture string

const (
	ArchitectureEvm NetworkArchitecture = "evm"
	ArchitectureSvm NetworkArchitecture = "svm"
)

type Network interface {
	Id() string
	Label() string
	ProjectId() string
	Architecture() NetworkArchitecture
	Config() *NetworkConfig
	Logger() *zerolog.Logger
	GetMethodMetrics(method string) TrackedMetrics
	Forward(ctx context.Context, nq *NormalizedRequest) (*NormalizedResponse, error)
	GetFinality(ctx context.Context, req *NormalizedRequest, resp *NormalizedResponse) DataFinalityState
}

// EvmNetwork is the EVM-specific view of a Network. Callers that need
// block-number accessors or leader-upstream selection should go through the
// EvmHighestLatestBlockNumber / EvmHighestFinalizedBlockNumber / EvmLeaderUpstream
// helpers below, which type-assert and degrade to zero-value on mismatch.
type EvmNetwork interface {
	Network
	EvmHighestLatestBlockNumber(ctx context.Context) int64
	EvmHighestFinalizedBlockNumber(ctx context.Context) int64
	EvmLeaderUpstream(ctx context.Context) Upstream
}

// SvmNetwork is the SVM-specific view of a Network. Production Network
// implementations satisfy this automatically when the underlying network is
// SVM; EVM networks correctly fail the assertion.
type SvmNetwork interface {
	Network
	SvmHighestLatestSlot(ctx context.Context) int64
	SvmHighestFinalizedSlot(ctx context.Context) int64
	SvmHighestIndexedSlot(ctx context.Context) int64
	SvmEnforceBlockAvailability() bool
}

// EvmHighestLatestBlockNumber returns the highest observed "latest" block
// across EVM upstreams of n, or 0 if n is not an EVM network or no upstream
// has reported a block yet. Use in place of a direct method call so callers
// don't need to type-assert inline.
func EvmHighestLatestBlockNumber(n Network, ctx context.Context) int64 {
	if e, ok := n.(EvmNetwork); ok {
		return e.EvmHighestLatestBlockNumber(ctx)
	}
	return 0
}

// EvmHighestFinalizedBlockNumber mirrors EvmHighestLatestBlockNumber for the
// finalized tip.
func EvmHighestFinalizedBlockNumber(n Network, ctx context.Context) int64 {
	if e, ok := n.(EvmNetwork); ok {
		return e.EvmHighestFinalizedBlockNumber(ctx)
	}
	return 0
}

// EvmLeaderUpstream returns the currently-elected leader EVM upstream for n,
// or nil if n is not EVM-shaped or no leader has been elected.
func EvmLeaderUpstream(n Network, ctx context.Context) Upstream {
	if e, ok := n.(EvmNetwork); ok {
		return e.EvmLeaderUpstream(ctx)
	}
	return nil
}

// IsValidArchitecture reports whether eRPC serves this architecture. It is the
// URL gate: /<project>/<architecture>/<chain> is rejected here before any
// network is looked up.
//
// The answer comes from the chain-family registry (chain_family.go), not from
// a list of names. Every architecture eRPC serves registers a family in its
// package init(), so adding a chain does not touch this function — and a chain
// that is registered can never be probeable and unroutable at the same time.
func IsValidArchitecture(architecture string) bool {
	_, ok := LookupChainFamily(NetworkArchitecture(architecture))
	return ok
}

// IsValidNetwork reports whether a config may name this network.
//
// Two questions live here, and they are not the same question:
//
//  1. IS THE ID WELL FORMED? That belongs to the chain family, so this asks
//     the registry through util.IsValidNetworkId. A family registers its own
//     rule at init, so adding a chain needs no edit here — the same property
//     IsValidArchitecture above already has. This function used to match
//     "evm:" and then "svm:" and then return false, which meant a config
//     naming btc:mainnet under providers[].onlyNetworks failed to load even
//     though every other gate in the request path accepted it.
//
//  2. DOES CONFIG ACCEPT IT? That is policy, and it stays here. EVM is the
//     one family with a rule of this kind: a chain id must be positive.
//     util.IsEvmNetworkIdBody deliberately accepts a negative integer (see its
//     comment) because it answers question 1 only. Delegating outright would
//     therefore start accepting "evm:0" and "evm:-1" in config, so the check
//     stays, right next to the other thing config decides.
func IsValidNetwork(network string) bool {
	if !util.IsValidNetworkId(network) {
		return false
	}
	if body, ok := strings.CutPrefix(network, "evm:"); ok {
		chainId, err := strconv.ParseInt(body, 10, 64)
		return err == nil && chainId > 0
	}
	return true
}

type QuantileTracker interface {
	Add(value float64)
	GetQuantile(qtile float64) time.Duration
	Reset()
}

type TrackedMetrics interface {
	ErrorRate() float64
	GetResponseQuantiles() QuantileTracker
}
