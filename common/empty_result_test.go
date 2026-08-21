package common

import "testing"

func TestResolveEmptyResultAccept_NilRetryFallsBackToDefault(t *testing.T) {
	got := ResolveEmptyResultAccept(nil)
	if len(got) != len(DefaultEmptyResultAccept()) {
		t.Fatalf("expected default list, got %v", got)
	}
}

func TestResolveEmptyResultAccept_UnsetFieldFallsBackToDefault(t *testing.T) {
	got := ResolveEmptyResultAccept(&RetryPolicyConfig{MaxAttempts: 3})
	if len(got) != len(DefaultEmptyResultAccept()) {
		t.Fatalf("expected default list, got %v", got)
	}
}

func TestResolveEmptyResultAccept_ConfiguredListWins(t *testing.T) {
	got := ResolveEmptyResultAccept(&RetryPolicyConfig{
		EmptyResultAccept: []string{"eth_chainId"},
	})
	if len(got) != 1 || got[0] != "eth_chainId" {
		t.Fatalf("expected configured list, got %v", got)
	}
}

func TestIsEmptyResultAccepted_DefaultsAcceptValueReads(t *testing.T) {
	// These are the methods where zero is a real value, not absence.
	for _, m := range []string{
		"eth_call",
		"eth_getBalance",
		"eth_getCode",
		"eth_getStorageAt",
		"eth_getTransactionCount",
		"eth_getLogs",
	} {
		if !IsEmptyResultAccepted(nil, m) {
			t.Errorf("expected %s to accept emptyish results by default", m)
		}
	}
}

func TestIsEmptyResultAccepted_DefaultsRejectPointLookups(t *testing.T) {
	// For these, empty means "not found yet, try another upstream".
	for _, m := range []string{
		"eth_getBlockByNumber",
		"eth_getTransactionByHash",
		"eth_getTransactionReceipt",
		"trace_block",
	} {
		if IsEmptyResultAccepted(nil, m) {
			t.Errorf("expected %s to still rotate on emptyish results", m)
		}
	}
}

func TestIsEmptyResultAccepted_EmptyMethodIsNotAccepted(t *testing.T) {
	if IsEmptyResultAccepted(nil, "") {
		t.Fatal("empty method name must not be accepted")
	}
}

func TestIsEmptyResultAccepted_HonoursMatchMethod(t *testing.T) {
	cfg := &NetworkConfig{
		Failsafe: []*FailsafeConfig{
			{
				MatchMethod: "eth_getLogs",
				Retry:       &RetryPolicyConfig{EmptyResultAccept: []string{}},
			},
			{
				MatchMethod: "*",
				Retry:       &RetryPolicyConfig{EmptyResultAccept: []string{"eth_call"}},
			},
		},
	}
	// eth_getLogs matches the first policy, which accepts nothing.
	if IsEmptyResultAccepted(cfg, "eth_getLogs") {
		t.Error("eth_getLogs should use the first matching policy's empty list")
	}
	// eth_call falls through to the wildcard policy.
	if !IsEmptyResultAccepted(cfg, "eth_call") {
		t.Error("eth_call should match the wildcard policy")
	}
	// eth_getBlockByNumber matches the wildcard policy, which omits it.
	if IsEmptyResultAccepted(cfg, "eth_getBlockByNumber") {
		t.Error("eth_getBlockByNumber is not in the wildcard policy's list")
	}
}

func TestIsEmptyResultAccepted_BlankMatchMethodTreatedAsWildcard(t *testing.T) {
	cfg := &NetworkConfig{
		Failsafe: []*FailsafeConfig{
			{Retry: &RetryPolicyConfig{EmptyResultAccept: []string{"eth_call"}}},
		},
	}
	if !IsEmptyResultAccepted(cfg, "eth_call") {
		t.Fatal("blank matchMethod must behave as \"*\"")
	}
}
