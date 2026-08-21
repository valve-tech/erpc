package common

import "testing"

// The rotation rule these functions implement is the fork's own fix: an
// emptyish result for an accepted method is the FINAL answer, not a "this
// upstream is missing data" signal. Getting the resolution wrong is measured
// in real traffic — 299,997 emptyish eth_call responses previously drove
// ~1.75M redundant upstream calls on evm:369, all returning the same zero.

func TestEmptyResultAccept_DefaultListIsAFreshCopyEachCall(t *testing.T) {
	// Both the retry layer and the rotation layer call this. If they shared one
	// backing array, a caller that appended to its list would silently change
	// the other layer's policy for the rest of the process.
	first := DefaultEmptyResultAccept()
	if len(first) == 0 {
		t.Fatal("the default accept list must not be empty")
	}
	first[0] = "MUTATED"

	second := DefaultEmptyResultAccept()
	if second[0] == "MUTATED" {
		t.Fatal("DefaultEmptyResultAccept must return a fresh copy, not the shared slice")
	}
	if !IsEmptyResultAccepted(nil, second[0]) {
		t.Fatalf("the pristine default must still resolve: %q", second[0])
	}
}

func TestEmptyResultAccept_NilPolicyEntriesAreSkipped(t *testing.T) {
	// A YAML list can carry a nil entry (an empty "- " item). Skipping it must
	// let resolution continue to the next policy instead of panicking or
	// silently returning the built-in default.
	cfg := &NetworkConfig{
		Failsafe: []*FailsafeConfig{
			nil,
			{MatchMethod: "*", Retry: &RetryPolicyConfig{EmptyResultAccept: []string{"eth_chainId"}}},
		},
	}
	if !IsEmptyResultAccepted(cfg, "eth_chainId") {
		t.Fatal("resolution must skip the nil entry and use the next policy")
	}
	if IsEmptyResultAccepted(cfg, "eth_call") {
		t.Fatal("the reached policy's list must win over the built-in default")
	}
}

func TestEmptyResultAccept_FirstMatchingPolicyWinsEvenIfALaterOneAlsoMatches(t *testing.T) {
	// networkExecutor instances are built one per failsafe entry and matched in
	// declaration order. If this resolved differently, the retry layer and the
	// rotation layer would disagree about the same method — which is exactly
	// the bug the shared resolver was written to remove.
	cfg := &NetworkConfig{
		Failsafe: []*FailsafeConfig{
			{MatchMethod: "eth_*", Retry: &RetryPolicyConfig{EmptyResultAccept: []string{"eth_getLogs"}}},
			{MatchMethod: "*", Retry: &RetryPolicyConfig{EmptyResultAccept: []string{"eth_call", "trace_filter"}}},
		},
	}
	if !IsEmptyResultAccepted(cfg, "eth_getLogs") {
		t.Error("eth_getLogs must resolve through the FIRST matching policy")
	}
	if IsEmptyResultAccepted(cfg, "eth_call") {
		t.Error("eth_call matches eth_* first, whose list omits it — the wildcard must not be consulted")
	}
	if !IsEmptyResultAccepted(cfg, "trace_filter") {
		t.Error("a method that misses eth_* must fall through to the wildcard policy")
	}
}

func TestEmptyResultAccept_APolicyWithNoRetryBlockUsesTheDefaultList(t *testing.T) {
	// A failsafe entry that configures only a timeout still MATCHES. Its
	// missing retry block must resolve to the built-in default rather than to
	// an empty list, which would send every emptyish eth_call round the fleet.
	cfg := &NetworkConfig{
		Failsafe: []*FailsafeConfig{
			{MatchMethod: "*"},
		},
	}
	if !IsEmptyResultAccepted(cfg, "eth_call") {
		t.Fatal("a matching policy without a retry block must fall back to the default list")
	}
	if IsEmptyResultAccepted(cfg, "eth_getTransactionReceipt") {
		t.Fatal("the default list must still exclude point lookups")
	}
}

func TestEmptyResultAccept_AnExplicitlyEmptyListDisablesAcceptance(t *testing.T) {
	// `emptyResultAccept: []` is how an operator says "always rotate". It must
	// be distinguishable from "unset", which means "use the defaults".
	explicit := &NetworkConfig{
		Failsafe: []*FailsafeConfig{
			{MatchMethod: "*", Retry: &RetryPolicyConfig{EmptyResultAccept: []string{}}},
		},
	}
	if IsEmptyResultAccepted(explicit, "eth_call") {
		t.Fatal("an explicitly empty list must accept nothing")
	}

	unset := &NetworkConfig{
		Failsafe: []*FailsafeConfig{
			{MatchMethod: "*", Retry: &RetryPolicyConfig{MaxAttempts: 3}},
		},
	}
	if !IsEmptyResultAccepted(unset, "eth_call") {
		t.Fatal("an unset list must fall back to the defaults")
	}
}

func TestEmptyResultAccept_NoPolicyMatchesFallsBackToTheDefaults(t *testing.T) {
	// A config whose policies all target other methods must leave the method
	// under the built-in rule, not under an empty list.
	cfg := &NetworkConfig{
		Failsafe: []*FailsafeConfig{
			{MatchMethod: "trace_*", Retry: &RetryPolicyConfig{EmptyResultAccept: []string{}}},
		},
	}
	if !IsEmptyResultAccepted(cfg, "eth_call") {
		t.Fatal("an unmatched method must use the default accept list")
	}
	if IsEmptyResultAccepted(cfg, "trace_filter") {
		t.Fatal("a matched method must use its policy's empty list")
	}
}

func TestEmptyResultAccept_UnparseablePatternDoesNotSwallowTheMethod(t *testing.T) {
	// A malformed matchMethod must not act as a catch-all. Treating a pattern
	// error as a match would apply the wrong policy to every method on the
	// network, silently.
	// "(eth_call" fails to parse: the matcher grammar wants a closing paren.
	if _, err := WildcardMatch("(eth_call", "eth_call"); err == nil {
		t.Fatal("fixture is stale: this pattern must be a parse error for the test to mean anything")
	}
	cfg := &NetworkConfig{
		Failsafe: []*FailsafeConfig{
			{MatchMethod: "(eth_call", Retry: &RetryPolicyConfig{EmptyResultAccept: []string{}}},
			{MatchMethod: "*", Retry: &RetryPolicyConfig{EmptyResultAccept: []string{"eth_call"}}},
		},
	}
	if !IsEmptyResultAccepted(cfg, "eth_call") {
		t.Fatal("a broken pattern must be skipped so the next policy still applies")
	}
}
