package svm

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

// ChainFamily is SVM's implementation of the fork's pluggable chain pattern
// (common/chain_family.go).
//
// # WHY SVM HAS ONE AT ALL
//
// The pipeline gates — architecture validation, network-id validation, the
// client factory — used to enumerate "evm" and "svm" by hand. They now ask the
// family registry, and that only works if every served architecture is IN the
// registry: a registry with a hole still needs the switch it replaced, and the
// hole stays invisible until a request falls into it.
//
// This type is registration, not new behaviour. SvmStatePoller still owns
// liveness on the request path, and the network's own failsafe policy still
// owns rotation. See the notes on Probe and Classify.
type ChainFamily struct{}

func (f *ChainFamily) Family() common.NetworkArchitecture { return common.ArchitectureSvm }

func (f *ChainFamily) Transport() common.ChainTransport { return common.TransportJsonRpc }

// ValidateNetworkId accepts "<cluster>" (implicit solana, the back-compat
// form) and "<chain>:<cluster>" — the body of "svm:mainnet-beta" and
// "svm:fogo:mainnet".
//
// Delegates to util rather than restating the rule, so the registered path and
// util's own fallback cannot drift into disagreeing about which configs load.
func (f *ChainFamily) ValidateNetworkId(body string) bool {
	return util.IsSvmNetworkIdBody(body)
}

// SupportsEndpointScheme keeps SVM upstreams on http/https.
//
// This is the pre-registry restriction, carried by the family now that the
// client factory picks a client by scheme for every family alike. It is not
// incidental: nothing in the SVM path has been run against the WebSocket or
// gRPC clients, and building one from an `ws://` endpoint would produce an
// upstream that registers, ranks and takes traffic before anyone discovers it
// cannot serve. Lift this when an SVM WebSocket path is actually tested.
func (f *ChainFamily) SupportsEndpointScheme(scheme string) (bool, string) {
	switch scheme {
	case "http", "https":
		return true, ""
	}
	return false, "only http/https supported"
}

// MatchesConfiguredChain compares two SVM cluster names EXACTLY.
//
// Solana has no short form: a node and an operator both write "mainnet-beta",
// "devnet" or "testnet" in full, so there is nothing to reconcile and equality
// decides every case. Accepting anything looser would let "testnet" pass for a
// devnet pool on a resemblance the cluster names do not have.
//
// The real SVM identity check is the genesis hash, which Bootstrap already
// compares (upstream/upstream.go) — a name is a label an operator chose, while
// the genesis hash is the chain itself. This method exists so the
// chain-agnostic seam has an honest SVM answer, not to replace that check.
func (f *ChainFamily) MatchesConfiguredChain(configured, observed string) bool {
	return configured == observed
}

// Probe answers liveness and tip for a generic caller — a health endpoint, or
// an operator asking "is this node serving?".
//
// It is NOT on the request path: SvmStatePoller already tracks slots per
// upstream, applies the shred-insert lag rule and gates routing. This method
// exists so the chain-agnostic seam has an honest SVM answer, not to become a
// second source of truth.
func (f *ChainFamily) Probe(ctx context.Context, c common.ProbeCaller) common.ChainProbe {
	// getSlot first: it answers reachability AND the tip in one call, so a
	// dead node costs one round trip rather than two.
	raw, err := c.CallJsonRpc(ctx, "getSlot", nil)
	if err != nil {
		// Fail closed, and keep the cause: an operator needs to tell a refused
		// dial from a node answering badly.
		return common.ChainProbe{
			Liveness: common.ChainDown,
			Detail:   "getSlot failed",
			Err:      fmt.Errorf("svm probe: %w", err),
		}
	}
	var slot int64
	if err := json.Unmarshal(raw, &slot); err != nil {
		return common.ChainProbe{
			Liveness: common.ChainDown,
			Detail:   "getSlot returned an undecodable body",
			Err:      fmt.Errorf("svm probe decode: %w", err),
		}
	}
	if slot <= 0 {
		return common.ChainProbe{
			Liveness: common.ChainSyncing,
			Tip:      slot,
			Detail:   "slot 0: node has processed nothing yet",
		}
	}

	// getHealth is the caught-up check. A node that is behind answers it with
	// an error (-32005), which the caller surfaces as a transport error — but
	// getSlot has already proven the node is reachable, so a failure here means
	// "behind", not "gone". Reporting it as down would raise an outage for
	// recoverable lag; reporting it as healthy would serve stale reads.
	if _, err := c.CallJsonRpc(ctx, "getHealth", nil); err != nil {
		return common.ChainProbe{
			Liveness: common.ChainSyncing,
			Tip:      slot,
			Detail:   fmt.Sprintf("slot %d, getHealth refused: %v", slot, err),
		}
	}
	return common.ChainProbe{
		Liveness: common.ChainHealthy,
		Tip:      slot,
		Detail:   fmt.Sprintf("slot %d", slot),
	}
}

// Classify decides what a response means for upstream rotation.
//
// SVM's rule is driven by ERROR CODES, not by empty results. A Solana node
// that lacks a block or a transaction history says so (-32004, -32007, -32011
// — see error_normalizer.go), which eRPC normalizes to
// ErrCodeEndpointMissingData. An empty result, by contrast, is the real answer:
// a null account does not exist anywhere in the pool, so rotating on it re-asks
// every upstream for the same null. That asymmetry is the whole rule.
func (f *ChainFamily) Classify(in common.ClassifyInput) common.RotateVerdict {
	// The write guard comes first, ahead of every other verdict. A failing
	// sendTransaction may still propagate from the node that appeared to fail,
	// and requestAirdrop MINTS per call — re-sending either one duplicates an
	// effect that already happened, whatever the error says.
	if IsNonRetryableWriteMethod(in.Method) {
		if in.ErrCode == common.ErrCodeEndpointClientSideException {
			return common.VerdictClientError
		}
		return common.VerdictServe
	}
	// A malformed request fails identically on every upstream. Rotating it
	// multiplies one bad client call by the size of the pool.
	if in.ErrCode == common.ErrCodeEndpointClientSideException {
		return common.VerdictClientError
	}
	// This node cannot answer, but a peer with deeper history can.
	if in.ErrCode == common.ErrCodeEndpointMissingData {
		return common.VerdictRotate
	}
	return common.VerdictServe
}

func init() {
	// Registration failure is a programming error (duplicate name or a bad
	// transport), not a runtime condition — surface it immediately rather than
	// leaving an architecture that silently fails every registry gate.
	if err := common.RegisterChainFamily(&ChainFamily{}); err != nil {
		panic(fmt.Sprintf("svm: %v", err))
	}
}
