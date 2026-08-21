package btc

import (
	"context"
	"net/http"

	"github.com/erpc/erpc/common"
)

func init() {
	common.RegisterArchitecture(Architecture, &ArchitectureHandler{})
}

// ArchitectureHandler is btc's entry in upstream's pipeline registry
// (common/architecture.go). It exists so a btc network can be BUILT: the
// networks registry refuses to prepare a network whose architecture has no
// handler, and the pipeline dispatches its five hooks through it.
//
// Every hook is a pass-through, and that is the finding, not an omission.
// eRPC's pipeline is already chain-agnostic: ranking, rotation, hedging,
// caching keys and breaker state all run on inputs no chain owns. EVM's hooks
// exist for hex block references and EVM-specific error shapes; SVM's exist for
// commitment injection and slot tracking. Bitcoin needs neither. The chain's
// real judgement — its probe, its tip and its rotation rule — lives in
// family.go, where it can be unit-tested without a pipeline.
//
// Add a hook here only when a REAL bitcoind behaviour forces it. An empty hook
// that guesses is worse than no hook: it runs on every request.
type ArchitectureHandler struct{}

func (h *ArchitectureHandler) HandleProjectPreForward(ctx context.Context, network common.Network, req *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	return false, nil, nil
}

func (h *ArchitectureHandler) HandleNetworkPreForward(ctx context.Context, network common.Network, upstreams []common.Upstream, req *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	return false, nil, nil
}

func (h *ArchitectureHandler) HandleNetworkPostForward(ctx context.Context, network common.Network, req *common.NormalizedRequest, resp *common.NormalizedResponse, err error) (*common.NormalizedResponse, error) {
	return resp, err
}

func (h *ArchitectureHandler) HandleUpstreamPreForward(ctx context.Context, network common.Network, upstream common.Upstream, req *common.NormalizedRequest, skipCacheRead bool) (bool, *common.NormalizedResponse, error) {
	return false, nil, nil
}

func (h *ArchitectureHandler) HandleUpstreamPostForward(ctx context.Context, network common.Network, upstream common.Upstream, req *common.NormalizedRequest, resp *common.NormalizedResponse, err error, skipCacheRead bool) (*common.NormalizedResponse, error) {
	return resp, err
}

// NewJsonRpcErrorExtractor returns an extractor that claims nothing.
//
// The extractors are composed across ALL registered architectures
// (upstream/composite_error_extractor.go) and the first non-nil answer wins, so
// an extractor that guessed at bitcoind's error codes would also be offered
// every EVM and SVM error. Claiming nothing leaves eRPC's generic JSON-RPC
// error handling in charge, which is the honest state until bitcoind's codes
// have been mapped against real nodes.
func (h *ArchitectureHandler) NewJsonRpcErrorExtractor() common.JsonRpcErrorExtractor {
	return &jsonRpcErrorExtractor{}
}

type jsonRpcErrorExtractor struct{}

func (e *jsonRpcErrorExtractor) Extract(_ *http.Response, _ *common.NormalizedResponse, _ *common.JsonRpcResponse, upstream common.Upstream) error {
	return nil
}
