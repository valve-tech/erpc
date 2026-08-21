package evm

import (
	"context"

	"github.com/erpc/erpc/common"
)

// NormalizeRequest implements common.RequestNormalizer for EVM.
//
// It carries the work Network.prepareRequest used to do inside a
// `case common.ArchitectureEvm:` branch. Moving it behind the handler is what
// lets that function stop naming architectures: the pipeline parses the body
// and asks the registered handler whether it wants to rewrite it, and only EVM
// says yes.
//
// The method lives in its own file rather than in handler.go so that merging
// upstream's changes to the handler never collides with the fork's seam.
func (h *EvmArchitectureHandler) NormalizeRequest(ctx context.Context, req *common.NormalizedRequest) error {
	jrq, err := req.JsonRpcRequest(ctx)
	if err != nil {
		// The caller has already parsed the body, so this is the memoized
		// result and cannot realistically fail here. Return it rather than
		// normalizing a nil request.
		return err
	}
	NormalizeHttpJsonRpc(ctx, req, jrq)
	return nil
}
