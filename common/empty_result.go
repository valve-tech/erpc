package common

import (
	"context"
	"strings"
)

// ResolveEmptyResultAccept returns the method list for which an emptyish
// result is the canonical final answer rather than a "data is missing, retry
// elsewhere" signal. A nil retry policy, or one that leaves EmptyResultAccept
// unset, falls back to the built-in default.
//
// Both the network-retry layer (erpc/network_executor.go) and the
// upstream-rotation layer (common/request.go) resolve the list through this
// function. They previously did not: DefaultEmptyResultAccept already listed
// eth_call, but only the retry layer read it, so an eth_call returning a
// 32-byte zero word (a zero balanceOf, a zero allowance, a false bool) was
// still treated as missing data by the rotation layer and re-sent to every
// other upstream. Measured on evm:369: 299,997 emptyish responses drove
// ~1.75M redundant upstream calls, and the final answer was the same zero.
func ResolveEmptyResultAccept(retry *RetryPolicyConfig) []string {
	if retry != nil && retry.EmptyResultAccept != nil {
		return retry.EmptyResultAccept
	}
	return DefaultEmptyResultAccept()
}

// IsEmptyResultAccepted reports whether an emptyish result for `method` is a
// final answer under `cfg`. It walks the network's failsafe policies in
// declaration order, picks the first whose MatchMethod matches, and consults
// that policy's list. A nil config, or no matching policy, uses the default.
func IsEmptyResultAccepted(cfg *NetworkConfig, method string) bool {
	if method == "" {
		return false
	}
	for _, m := range emptyResultAcceptFor(cfg, method) {
		if m == method {
			return true
		}
	}
	return false
}

// emptyResultAcceptFor picks the effective list for `method`. Mirrors how
// networkExecutor instances are built — one per failsafe entry, matched by
// MatchMethod — so both layers resolve the same policy for a given method.
func emptyResultAcceptFor(cfg *NetworkConfig, method string) []string {
	if cfg == nil {
		return DefaultEmptyResultAccept()
	}
	for _, fs := range cfg.Failsafe {
		if fs == nil {
			continue
		}
		pattern := fs.MatchMethod
		if pattern == "" {
			pattern = "*"
		}
		matched, err := WildcardMatch(pattern, method)
		if err != nil || !matched {
			continue
		}
		return ResolveEmptyResultAccept(fs.Retry)
	}
	return DefaultEmptyResultAccept()
}

// UpstreamEmptyIsAuthoritative reports whether resp is an empty answer that
// u is authoritative for. Three things must hold:
//   - u's config lists method in evm.emptyResultAuthoritativeMethods;
//   - the result is present and not JSON null. null, or no result, is how a
//     node says it has not got the block yet (reth answers trace_block with
//     null), so it stays missing data and fails over as before;
//   - u has the requested block (see upstreamHasRequestedBlock). A node at
//     the tip can answer [] or 0x0 for a block it has not imported yet, so an
//     empty answer above its head still fails over.
//
// req is the request resp answers; nil means resp.Request(). Every layer
// that decides whether an empty answer is final asks this function: the
// network loop (which returns a final answer before the fallback escape
// can run), the retry policy, the upstream rotation and the
// markEmptyAsErrorMethods hook.
func UpstreamEmptyIsAuthoritative(u Upstream, method string, req *NormalizedRequest, resp *NormalizedResponse) bool {
	if u == nil || resp == nil {
		return false
	}
	if req == nil {
		req = resp.Request()
	}
	cfg := u.Config()
	if cfg == nil || cfg.Evm == nil {
		return false
	}
	listed := false
	for _, m := range cfg.Evm.EmptyResultAuthoritativeMethods {
		if strings.EqualFold(m, method) {
			listed = true
			break
		}
	}
	if !listed {
		return false
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil || jrr == nil {
		return false
	}
	result := strings.TrimSpace(jrr.GetResultString())
	if result == "" || result == "null" {
		return false
	}
	return upstreamHasRequestedBlock(u, req)
}

// AcceptedEmptyIsFinal reports whether an empty result that the network's
// emptyResultAccept list accepts, from upstream u, may stand as the final
// answer to req. On EVM it applies the same head check as
// UpstreamEmptyIsAuthoritative: a node at the tip can answer [] or 0x for a
// block it has not imported yet, so an empty answer for a block above u's
// known head, or with u's head unknown, is not final and the request moves
// on. When the request has no resolved block number (a pending tag, a block
// hash, a method with no block) the check cannot run and the answer stays
// final, as before. Non-EVM upstreams are not affected.
func AcceptedEmptyIsFinal(u Upstream, req *NormalizedRequest) bool {
	if _, ok := u.(EvmUpstream); !ok || req == nil {
		return true
	}
	if requestedBlockNumber(req) <= 0 {
		return true
	}
	return upstreamHasRequestedBlock(u, req)
}

// requestedBlockNumber returns the block req targets, or 0 when it cannot be
// resolved. A literal number, or a tag that normalization already turned
// into one, comes from req.EvmBlockNumber (toBlock for eth_getLogs). Some
// methods, eth_call and the value reads among them, keep their latest or
// finalized tag on the wire. Those tags resolve here to the network's
// served latest or finalized head, the block the client expects the answer
// for. safe, pending, earliest and block hashes do not resolve.
func requestedBlockNumber(req *NormalizedRequest) int64 {
	if bn, ok := req.EvmBlockNumber().(int64); ok && bn > 0 {
		return bn
	}
	nw := req.Network()
	if nw == nil {
		return 0
	}
	switch req.EvmBlockRef() {
	case "latest":
		return EvmHighestLatestBlockNumber(nw, context.Background())
	case "finalized":
		return EvmHighestFinalizedBlockNumber(nw, context.Background())
	}
	return 0
}

// upstreamHasRequestedBlock reports whether the block req targets is at or
// below u's known head. The block is requestedBlockNumber(req): toBlock for
// eth_getLogs, the number a latest or finalized tag resolves to, or the
// literal number. The head is u's own state-poller head: the latest block, or
// the finalized block when the network sets evm.emptyResultConfidence to
// finalizedBlock. A configured blockAvailability upper bound (for example
// latestBlockMinus) lowers it further. It returns false when either the block
// or the head is unknown, so the empty answer keeps failing over.
func upstreamHasRequestedBlock(u Upstream, req *NormalizedRequest) bool {
	if req == nil {
		return false
	}
	bn := requestedBlockNumber(req)
	if bn <= 0 {
		return false
	}
	eu, ok := u.(EvmUpstream)
	if !ok {
		return false
	}
	sp := eu.EvmStatePoller()
	if sp == nil || sp.IsObjectNull() {
		return false
	}
	head := sp.LatestBlock()
	if nw := req.Network(); nw != nil {
		if cfg := nw.Config(); cfg != nil && cfg.Evm != nil &&
			cfg.Evm.EmptyResultConfidence == AvailbilityConfidenceFinalized {
			head = sp.FinalizedBlock()
		}
	}
	if _, upper := eu.EvmBlockAvailabilityBounds(); upper < head {
		head = upper
	}
	return head > 0 && bn <= head
}
