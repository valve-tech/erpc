package common

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
