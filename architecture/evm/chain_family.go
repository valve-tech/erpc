package evm

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

// ChainFamily is EVM's implementation of the fork's pluggable chain pattern
// (common/chain_family.go).
//
// # WHY EVM HAS ONE AT ALL
//
// The pipeline gates — architecture validation, network-id validation, the
// client factory — used to enumerate "evm" and "svm" by hand, so adding a
// chain meant editing every one of them. They now ask the family registry
// instead. That only works if EVERY served architecture is in the registry:
// a registry with a hole still needs the switch it was meant to replace, and
// the hole is invisible until the request that falls into it 404s.
//
// This type is registration, not new behaviour. Nothing here re-decides
// anything the EVM pipeline already decides: the request path keeps using
// EvmStatePoller for liveness and the network's own failsafe policy for
// rotation. See the notes on Probe and Classify.
type ChainFamily struct{}

func (f *ChainFamily) Family() common.NetworkArchitecture { return common.ArchitectureEvm }

func (f *ChainFamily) Transport() common.ChainTransport { return common.TransportJsonRpc }

// ValidateNetworkId accepts a decimal chain id — the body of "evm:1".
//
// Delegates to util rather than restating the rule. util keeps the same
// function as the fallback it uses when this package is not linked, so the
// registered path and the fallback cannot drift into disagreeing about which
// configs load.
func (f *ChainFamily) ValidateNetworkId(body string) bool {
	return util.IsEvmNetworkIdBody(body)
}

// Probe answers liveness and tip for a generic caller — a health endpoint, or
// an operator asking "is this node serving?".
//
// It is NOT on the request path. EvmStatePoller already polls eth_syncing and
// eth_blockNumber per upstream, feeds health.Tracker and gates routing; this
// method exists so the chain-agnostic seam has an honest EVM answer, not to
// become a second source of truth. If it ever moves onto the request path, the
// poller is what it must replace, not run beside.
//
// Two calls, which the ChainFamily contract names EVM as the one case for:
// eth_syncing does not carry the head when a node is caught up. A node that
// admits it is syncing is answered by the first call alone, so the second is
// only spent on nodes that claim to be current.
func (f *ChainFamily) Probe(ctx context.Context, c common.ProbeCaller) common.ChainProbe {
	raw, err := c.CallJsonRpc(ctx, "eth_syncing", nil)
	if err != nil {
		// Fail closed, and keep the cause: an operator needs to tell a refused
		// dial from a node answering badly.
		return common.ChainProbe{
			Liveness: common.ChainDown,
			Detail:   "eth_syncing failed",
			Err:      fmt.Errorf("evm probe: %w", err),
		}
	}

	syncing, current, highest, err := parseSyncing(raw)
	if err != nil {
		return common.ChainProbe{
			Liveness: common.ChainDown,
			Detail:   "eth_syncing returned an undecodable body",
			Err:      fmt.Errorf("evm probe decode: %w", err),
		}
	}
	if syncing {
		// Report the node's own current block, not zero: the tracker ranks on
		// this number, and a zero would read as "furthest behind" rather than
		// "behind by this much".
		return common.ChainProbe{
			Liveness: common.ChainSyncing,
			Tip:      current,
			Detail:   fmt.Sprintf("syncing, block %d of %d", current, highest),
		}
	}

	raw, err = c.CallJsonRpc(ctx, "eth_blockNumber", nil)
	if err != nil {
		return common.ChainProbe{
			Liveness: common.ChainDown,
			Detail:   "eth_blockNumber failed",
			Err:      fmt.Errorf("evm probe: %w", err),
		}
	}
	var hexHead string
	if err := json.Unmarshal(raw, &hexHead); err != nil {
		return common.ChainProbe{
			Liveness: common.ChainDown,
			Detail:   "eth_blockNumber returned an undecodable body",
			Err:      fmt.Errorf("evm probe decode: %w", err),
		}
	}
	head, err := common.HexToInt64(hexHead)
	if err != nil {
		return common.ChainProbe{
			Liveness: common.ChainDown,
			Detail:   "eth_blockNumber returned a non-hex height",
			Err:      fmt.Errorf("evm probe decode: %w", err),
		}
	}
	if head <= 0 {
		// A node that has imported nothing answers every query with "not
		// found" while claiming not to be syncing.
		return common.ChainProbe{
			Liveness: common.ChainSyncing,
			Tip:      head,
			Detail:   "height 0: node has imported no blocks",
		}
	}
	return common.ChainProbe{
		Liveness: common.ChainHealthy,
		Tip:      head,
		Detail:   fmt.Sprintf("height %d", head),
	}
}

// parseSyncing decodes eth_syncing, which answers `false` when caught up and
// an object while syncing — and plain `true` on a few clients that report no
// numbers at all.
func parseSyncing(raw []byte) (syncing bool, current, highest int64, err error) {
	var flag bool
	if err := json.Unmarshal(raw, &flag); err == nil {
		return flag, 0, 0, nil
	}
	var obj struct {
		CurrentBlock string `json:"currentBlock"`
		HighestBlock string `json:"highestBlock"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false, 0, 0, err
	}
	// A missing or malformed height inside a syncing object is not fatal — the
	// node has already said it is syncing, which is the part that gates traffic.
	current, _ = common.HexToInt64(obj.CurrentBlock)
	highest, _ = common.HexToInt64(obj.HighestBlock)
	return true, current, highest, nil
}

// Classify generalizes the fork's emptyResultAccept rule to the chain-agnostic
// seam.
//
// It answers from the DEFAULT method lists, because ClassifyInput carries no
// network config. The request path does not call it: it resolves the same
// question against the network's own failsafe policy, which an operator can
// override per network (common/empty_result.go). Reading the defaults here
// keeps the two consistent for every network that has not overridden them, and
// makes the difference explicit rather than inventing a third rule.
func (f *ChainFamily) Classify(in common.ClassifyInput) common.RotateVerdict {
	// A malformed request fails identically on every upstream. Rotating it
	// multiplies one bad client call by the size of the pool.
	if in.ErrCode == common.ErrCodeEndpointClientSideException {
		return common.VerdictClientError
	}
	if !in.IsEmpty {
		return common.VerdictServe
	}
	// An accepted empty result IS the answer — a zero balance, an empty log
	// range. This is the case that drove ~1.75M redundant calls on evm:369
	// when the rotation layer disagreed with the retry layer about it.
	if common.IsEmptyResultAccepted(nil, in.Method) {
		return common.VerdictServe
	}
	// Only the methods eRPC treats as missing-data rotate. Rotating on every
	// unlisted method would re-ask the pool for answers that are legitimately
	// empty — a null receipt for a pending transaction, for instance.
	for _, m := range common.DefaultMarkEmptyAsErrorMethods() {
		if m == in.Method {
			return common.VerdictRotate
		}
	}
	return common.VerdictServe
}

func init() {
	// Registration failure is a programming error (duplicate name or a bad
	// transport), not a runtime condition — surface it immediately rather than
	// leaving an architecture that silently fails every registry gate.
	if err := common.RegisterChainFamily(&ChainFamily{}); err != nil {
		panic(fmt.Sprintf("evm: %v", err))
	}
}
