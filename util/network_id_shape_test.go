package util

import "testing"

// These tests pin the ID-SHAPE SEAM, not the evm/svm rules themselves — those
// keep their own table in ids_test.go. What matters here is that a family can
// teach util a shape util has never heard of, and that a family that says
// "no" is believed.

func registerShapeForTest(t *testing.T, family string, shape NetworkIdShape) {
	t.Helper()
	if err := RegisterNetworkIdShape(family, shape); err != nil {
		t.Fatalf("RegisterNetworkIdShape(%q): %v", family, err)
	}
	// The registry is process-global. Without this cleanup one test's family
	// leaks into the next and a later "unregistered" assertion passes for the
	// wrong reason.
	t.Cleanup(func() { UnregisterNetworkIdShape(family) })
}

func TestIsValidNetworkId_RegisteredFamilyIsAccepted(t *testing.T) {
	// The whole point of the seam: "btc:mainnet" is not a shape util knows,
	// and adding it must not need an edit to util.
	registerShapeForTest(t, "btc", func(body string) bool { return body == "mainnet" })

	if !IsValidNetworkId("btc:mainnet") {
		t.Fatal("a registered family's own network id was rejected; a new chain " +
			"cannot route at all if its id fails validation")
	}
	if IsValidNetworkId("btc:not-a-chain") {
		t.Fatal("the family said no and util accepted anyway; the family, not " +
			"util, owns which bodies are real")
	}
}

func TestIsValidNetworkId_UnregisteredFamilyIsRejected(t *testing.T) {
	// Negative control for the test above. Without it, an IsValidNetworkId
	// that returned true for every "x:y" string would pass it.
	if IsValidNetworkId("doge:mainnet") {
		t.Fatal("an unregistered family was accepted; every typo'd architecture " +
			"would then resolve to a network that has no upstreams")
	}
}

func TestIsValidNetworkId_BuiltinsSurviveWithoutRegistration(t *testing.T) {
	// util sits below common in the import graph, so a binary (or this very
	// test) can run with NO family registered. evm and svm must still validate
	// identically there — every existing config and cache key depends on it.
	for _, id := range []string{"evm:1", "evm:42161", "svm:mainnet-beta", "svm:fogo:mainnet"} {
		if !IsValidNetworkId(id) {
			t.Errorf("IsValidNetworkId(%q) = false with no family registered; "+
				"linking an architecture package must not decide whether evm/svm ids parse", id)
		}
	}
	for _, id := range []string{"evm:", "evm:abc", "svm:", "svm:a:b:c", "nocolon", ""} {
		if IsValidNetworkId(id) {
			t.Errorf("IsValidNetworkId(%q) = true, want false", id)
		}
	}
}

func TestRegisterNetworkIdShape_RejectsDuplicateAndEmpty(t *testing.T) {
	registerShapeForTest(t, "dupfam", func(string) bool { return true })

	if err := RegisterNetworkIdShape("dupfam", func(string) bool { return false }); err == nil {
		t.Error("a second registration for the same family succeeded; a silent " +
			"overwrite lets one chain decide another chain's ids")
	}
	if err := RegisterNetworkIdShape("", func(string) bool { return true }); err == nil {
		t.Error("registering an unnamed family succeeded; no id could ever reach it")
	}
	if err := RegisterNetworkIdShape("nilfam", nil); err == nil {
		t.Error("registering a nil shape succeeded; the first id to arrive would panic")
	}
}

func TestUnregisterNetworkIdShape_RestoresTheBuiltin(t *testing.T) {
	// A family may register the shape of an architecture util also has a
	// builtin for (evm and svm do exactly that, delegating to the same
	// functions). Removing it must fall back, not start rejecting.
	registerShapeForTest(t, "evm", func(string) bool { return false })
	if IsValidNetworkId("evm:1") {
		t.Fatal("the registered shape did not win over the builtin; a family " +
			"cannot own its ids if util overrules it")
	}
	UnregisterNetworkIdShape("evm")
	if !IsValidNetworkId("evm:1") {
		t.Fatal("evm:1 stayed rejected after the shape was removed; the builtin " +
			"fallback is what keeps util working on its own")
	}
}
