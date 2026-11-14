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
// u is authoritative for: u's config lists method in
// evm.emptyResultAuthoritativeMethods, and the result is present and not
// JSON null. An empty value such as [] is the answer. null, or no result,
// is how a node says it has not got the block yet (reth answers trace_block
// with null), so it stays missing data and fails over as before.
//
// For a method in the network's evm.emptyResultHeadCheckMethods (block-range
// queries by default), u must also have the requested block: see
// upstreamHasRequestedBlock. Other methods, trace_block included, skip that
// check.
//
// req is the request resp answers; nil means resp.Request().
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
	if !EmptyResultHeadCheckApplies(req, method) {
		return true
	}
	return upstreamHasRequestedBlock(u, req)
}

// AcceptedEmptyIsFinal reports whether an empty result that the network's
// emptyResultAccept list accepts, from upstream u, may stand as the final
// answer to req. Only methods in evm.emptyResultHeadCheckMethods are
// checked; every other method keeps today's behaviour and returns true.
// For a checked method, a node at the tip can clamp the range to its head
// and answer [] for blocks it has not imported yet, so the answer is final
// only when the range end is at or below u's known head. A request with no
// resolved block number (a blockHash filter, say) keeps today's behaviour.
// Non-EVM upstreams are not affected.
func AcceptedEmptyIsFinal(u Upstream, method string, req *NormalizedRequest) bool {
	if !EmptyResultHeadCheckApplies(req, method) {
		return true
	}
	if _, ok := u.(EvmUpstream); !ok {
		return true
	}
	if emptyCheckBlock(req) <= 0 {
		return true
	}
	return upstreamHasRequestedBlock(u, req)
}

// emptyCheckBlock is the block the head check compares against: the range
// end RefreshHeadForEmptyResult resolved, else req.EvmBlockNumber (toBlock
// once normalization turned it into a number). 0 means unknown.
func emptyCheckBlock(req *NormalizedRequest) int64 {
	if req == nil {
		return 0
	}
	if end := req.EmptyResultRangeEnd(); end > 0 {
		return end
	}
	if bn, ok := req.EvmBlockNumber().(int64); ok && bn > 0 {
		return bn
	}
	return 0
}

// resolveRangeEnd returns the last block a range filter (params[0] of
// eth_getLogs or trace_filter) covers, or 0 when it cannot tell. A literal
// toBlock is used as is. An omitted toBlock, or one left as latest, safe or
// pending, runs to the chain head, so it resolves to the request's served
// latest head; finalized resolves to the served finalized head. ctx must be
// the request's context, so the served head is the request's own lane. A
// blockHash filter names no number and resolves to 0.
func resolveRangeEnd(ctx context.Context, req *NormalizedRequest) int64 {
	jrq, err := req.JsonRpcRequest(ctx)
	if err != nil || jrq == nil {
		return 0
	}
	jrq.RLock()
	var filter map[string]interface{}
	if len(jrq.Params) > 0 {
		filter, _ = jrq.Params[0].(map[string]interface{})
	}
	var to interface{}
	present, byHash := false, false
	if filter != nil {
		_, byHash = filter["blockHash"]
		to, present = filter["toBlock"]
	}
	jrq.RUnlock()
	if filter == nil || byHash {
		return 0
	}
	tag, _ := to.(string)
	if present && strings.HasPrefix(tag, "0x") {
		n, err := HexToInt64(tag)
		if err != nil {
			return 0
		}
		return n
	}
	nw := req.Network()
	if nw == nil {
		return 0
	}
	switch {
	case !present || tag == "" || tag == "latest" || tag == "safe" || tag == "pending":
		return EvmHighestLatestBlockNumber(nw, ctx)
	case tag == "finalized":
		return EvmHighestFinalizedBlockNumber(nw, ctx)
	}
	return 0
}

// EmptyResultHeadCheckApplies reports whether method is in the network's
// evm.emptyResultHeadCheckMethods list (DefaultEmptyResultHeadCheckMethods
// when unset). An empty list turns the check off.
func EmptyResultHeadCheckApplies(req *NormalizedRequest, method string) bool {
	if req == nil || method == "" {
		return false
	}
	list := defaultEmptyResultHeadCheckMethods
	if nw := req.Network(); nw != nil {
		if cfg := nw.Config(); cfg != nil && cfg.Evm != nil && cfg.Evm.EmptyResultHeadCheckMethods != nil {
			list = cfg.Evm.EmptyResultHeadCheckMethods
		}
	}
	for _, m := range list {
		if strings.EqualFold(m, method) {
			return true
		}
	}
	return false
}

// RefreshHeadForEmptyResult runs before the gates judge an empty answer. When
// the head check applies to method and u's known head is below the range end
// (or unknown), it asks u's state poller for a fresh head once, through
// PollLatestBlockNumberAtLeast. The poller's known head lags the node's real
// head by up to a poll interval, and without the refresh a node that has the
// block would see its true [] treated as not final (erpc/erpc#934 is the same
// race on the pre-forward side). The poller decides whether a poll is due
// and polls at most once per new block. Errors are ignored: the gates then
// read the head as it stands.
func RefreshHeadForEmptyResult(ctx context.Context, u Upstream, method string, req *NormalizedRequest, resp *NormalizedResponse) {
	if !EmptyResultHeadCheckApplies(req, method) {
		return
	}
	if resp == nil || resp.IsObjectNull() || !resp.IsResultEmptyish(ctx) {
		return
	}
	eu, ok := u.(EvmUpstream)
	if !ok {
		return
	}
	if end := resolveRangeEnd(ctx, req); end > 0 {
		req.emptyResultRangeEnd.Store(end)
	}
	bn := emptyCheckBlock(req)
	if bn <= 0 {
		return
	}
	sp := eu.EvmStatePoller()
	if sp == nil || sp.IsObjectNull() || sp.LatestBlock() >= bn {
		return
	}
	_, _ = sp.PollLatestBlockNumberAtLeast(ctx, bn)
}

// upstreamHasRequestedBlock reports whether the block req targets is at or
// below u's known head. The block is emptyCheckBlock(req): the range end,
// with an omitted or tag toBlock resolved to the served head. The head is u's own state-poller head: the
// latest block, or the finalized block when the network sets
// evm.emptyResultConfidence to finalizedBlock. A configured
// blockAvailability upper bound (for example latestBlockMinus) lowers it
// further. It returns false when either the block or the head is unknown.
func upstreamHasRequestedBlock(u Upstream, req *NormalizedRequest) bool {
	if req == nil {
		return false
	}
	bn := emptyCheckBlock(req)
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
