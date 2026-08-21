package thirdparty

import (
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

// The registries are the lookup layer every upstream passes through at
// boot. Two failure modes hurt an operator: a vendor that cannot be found
// by name (the provider is dropped), and a vendor claimed by the wrong
// owner (the upstream inherits another vendor's error normalisation).

// ownerVendor claims an upstream whose endpoint carries its own scheme.
// It stands in for any vendor, present or future — the registry contract
// must not depend on which one.
type ownerVendor struct {
	stubVendor
	scheme string
}

func (v *ownerVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	return strings.HasPrefix(ups.Endpoint, v.scheme+"://")
}

func TestVendorsRegistry_ListsEveryBuiltInVendorByName(t *testing.T) {
	// A vendor missing from the list is a provider that cannot boot. The
	// count is deliberately not asserted — adding a vendor is routine.
	r := NewVendorsRegistry()
	names := r.SupportedVendors()
	require.NotEmpty(t, names)

	seen := map[string]bool{}
	for _, n := range names {
		require.NotEmpty(t, n, "a vendor with an empty name can never be looked up")
		require.False(t, seen[n], "vendor name %q is registered twice — LookupByName would be ambiguous", n)
		seen[n] = true
		require.NotNil(t, r.LookupByName(n), "SupportedVendors listed %q but LookupByName cannot find it", n)
	}

	// Spot-check a few long-standing names so a rename cannot pass silently.
	for _, n := range []string{"alchemy", "drpc", "infura", "quicknode", "repository"} {
		require.True(t, seen[n], "built-in vendor %q disappeared from the registry", n)
	}
}

func TestVendorsRegistry_LookupByName_ReturnsNilForAnUnknownVendor(t *testing.T) {
	// nil is what NewProvidersRegistry turns into a readable boot error.
	// A non-nil placeholder would boot a provider that silently does nothing.
	r := NewVendorsRegistry()
	require.Nil(t, r.LookupByName("no-such-vendor"))
	require.Nil(t, r.LookupByName(""))
	require.Nil(t, r.LookupByName("Alchemy"), "lookup is case-sensitive; a near-miss must not resolve")
}

func TestVendorsRegistry_LookupByUpstream_PrefersTheExplicitVendorName(t *testing.T) {
	// When the operator names a vendor, that name wins outright. Falling
	// back to ownership probing would let a second vendor hijack the
	// upstream's error normalisation.
	r := &VendorsRegistry{}
	a := &ownerVendor{stubVendor: stubVendor{name: "vendor-a"}, scheme: "a"}
	b := &ownerVendor{stubVendor: stubVendor{name: "vendor-b"}, scheme: "b"}
	r.Register(a)
	r.Register(b)

	got := r.LookupByUpstream(&common.UpstreamConfig{VendorName: "vendor-b", Endpoint: "a://key"})
	require.Same(t, common.Vendor(b), got,
		"the explicit vendorName must win over the endpoint's owner")
}

func TestVendorsRegistry_LookupByUpstream_NamedButUnknownVendorReturnsNil(t *testing.T) {
	// The operator asked for a vendor that does not exist. Quietly
	// probing owners instead would attach a vendor they never chose.
	r := &VendorsRegistry{}
	r.Register(&ownerVendor{stubVendor: stubVendor{name: "vendor-a"}, scheme: "a"})

	got := r.LookupByUpstream(&common.UpstreamConfig{VendorName: "typo", Endpoint: "a://key"})
	require.Nil(t, got)
}

func TestVendorsRegistry_LookupByUpstream_FallsBackToOwnershipAndTakesTheFirstClaim(t *testing.T) {
	// The unnamed case is the primary path: an operator pastes an
	// endpoint and expects the right vendor to adopt it. First claim wins,
	// in registration order, so registration order is part of the contract.
	r := &VendorsRegistry{}
	first := &ownerVendor{stubVendor: stubVendor{name: "first"}, scheme: "shared"}
	second := &ownerVendor{stubVendor: stubVendor{name: "second"}, scheme: "shared"}
	r.Register(first)
	r.Register(second)

	got := r.LookupByUpstream(&common.UpstreamConfig{Endpoint: "shared://key"})
	require.Same(t, common.Vendor(first), got)
}

func TestVendorsRegistry_LookupByUpstream_UnclaimedUpstreamReturnsNil(t *testing.T) {
	// A plain https endpoint belongs to nobody. Returning a vendor here
	// would apply that vendor's error rules to a generic node.
	r := &VendorsRegistry{}
	r.Register(&ownerVendor{stubVendor: stubVendor{name: "vendor-a"}, scheme: "a"})

	require.Nil(t, r.LookupByUpstream(&common.UpstreamConfig{Endpoint: "https://node.example.com"}))
}

func TestVendorsRegistry_BuiltInVendorsDoNotClaimAPlainHttpsEndpoint(t *testing.T) {
	// The unknown-endpoint fallthrough is the case the design razor cares
	// about most: a self-hosted node must stay vendor-free rather than be
	// adopted by whichever built-in vendor has the loosest matcher.
	r := NewVendorsRegistry()
	for _, ep := range []string{
		"https://my-own-node.internal:8545",
		"http://127.0.0.1:8545",
		"ws://127.0.0.1:8546",
	} {
		v := r.LookupByUpstream(&common.UpstreamConfig{Endpoint: ep})
		require.Nil(t, v, "endpoint %q was claimed by vendor %v", ep, vendorName(v))
	}
}

func vendorName(v common.Vendor) string {
	if v == nil {
		return "<nil>"
	}
	return v.Name()
}

func TestProvidersRegistry_BuildsOneProviderPerConfigInOrder(t *testing.T) {
	r := &VendorsRegistry{}
	r.Register(&stubVendor{name: "stub"})

	pr, err := NewProvidersRegistry(testLogger(), r, []*common.ProviderConfig{
		{Id: "p1", Vendor: "stub"},
		{Id: "p2", Vendor: "stub"},
	}, nil)
	require.NoError(t, err)

	got := pr.GetAllProviders()
	require.Len(t, got, 2)
	require.Equal(t, "p1", got[0].Id())
	require.Equal(t, "p2", got[1].Id(),
		"provider order is config order — later providers are lower-priority")
}

func TestProvidersRegistry_UnknownVendorFailsBootWithAUsableMessage(t *testing.T) {
	// This error is what an operator sees when they typo a vendor name.
	// It must name the bad vendor, the provider it came from, and the
	// alternatives — otherwise they get a boot failure with nowhere to look.
	r := &VendorsRegistry{}
	r.Register(&stubVendor{name: "stub"})

	_, err := NewProvidersRegistry(testLogger(), r, []*common.ProviderConfig{
		{Id: "p1", Vendor: "stub"},
		{Id: "typo-provider", Vendor: "alchmey"},
	}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "alchmey", "the message must name the vendor that was not found")
	require.Contains(t, err.Error(), "typo-provider", "the message must name the offending provider")
	require.Contains(t, err.Error(), "stub", "the message must list the vendors that do exist")
}

func TestProvidersRegistry_EmptyConfigYieldsNoProviders(t *testing.T) {
	pr, err := NewProvidersRegistry(testLogger(), NewVendorsRegistry(), nil, nil)
	require.NoError(t, err)
	require.Empty(t, pr.GetAllProviders())
}

func TestProvidersRegistry_PassesUpstreamDefaultsThroughToTheProvider(t *testing.T) {
	// upstreamDefaults carry the operator's fleet-wide settings. A
	// provider that never sees them generates upstreams with stock
	// poll intervals instead of the configured ones, so a fleet tuned for
	// a fast chain silently reverts to the 30s default.
	r := &VendorsRegistry{}
	r.Register(&stubVendor{name: "stub"})
	defaults := &common.UpstreamConfig{
		Evm: &common.EvmUpstreamConfig{StatePollerInterval: common.Duration(3 * time.Second)},
	}

	pr, err := NewProvidersRegistry(testLogger(), r, []*common.ProviderConfig{{Id: "p1", Vendor: "stub"}}, defaults)
	require.NoError(t, err)

	cfg, err := pr.GetAllProviders()[0].buildBaseUpstreamConfig("evm:1")
	require.NoError(t, err)
	require.NotNil(t, cfg.Evm)
	require.Equal(t, common.Duration(3*time.Second), cfg.Evm.StatePollerInterval,
		"the fleet-wide poll interval must reach a provider-generated upstream")
}

func TestValidateChainsURL_AcceptsOnlyHttpAndHttps(t *testing.T) {
	// A remote chain list is fetched over the network at boot. Anything
	// that is not http(s) — a file path, a typo'd scheme — must fail as a
	// config error rather than as a confusing runtime fetch failure.
	for _, ok := range []string{
		"https://chains.example.com/list.json",
		"http://localhost:8080/list.json",
		"https://chains.example.com",
	} {
		require.NoError(t, validateChainsURL(ok), "url %q should be accepted", ok)
	}
	for _, bad := range []string{
		"",                      // empty
		"chains.example.com",    // no scheme
		"ftp://chains/list",     // wrong scheme
		"file:///etc/passwd",    // local file
		"https://",              // no host
		"://chains.example.com", // malformed
	} {
		require.Error(t, validateChainsURL(bad), "url %q should be rejected", bad)
	}
}

// Compile-time proof the stub still satisfies the interface the registry
// stores, so a Vendor interface change breaks this file loudly.
var _ common.Vendor = (*ownerVendor)(nil)
