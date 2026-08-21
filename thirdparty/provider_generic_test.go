package thirdparty

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// These tests pin the VENDOR-AGNOSTIC half of the provider path: the part
// that runs identically for a vendor nobody has written yet. Vendors are an
// open-ended set, so this generic fallthrough is the primary path — an
// unknown vendor still has to get a correct upstream ID, a correct chain
// ID, its overrides applied and its endpoint env-expanded.

// stubVendor is a vendor with no behaviour of its own. It records what the
// generic path handed it and echoes back whatever configs the test asks
// for, so a failure here is always a defect in provider.go.
type stubVendor struct {
	name string

	supports    bool
	supportsErr error

	gotBase     *common.UpstreamConfig
	gotSettings common.VendorSettings
	generate    func(base *common.UpstreamConfig) []*common.UpstreamConfig
	generateErr error
}

func (v *stubVendor) Name() string                               { return v.name }
func (v *stubVendor) OwnsUpstream(_ *common.UpstreamConfig) bool { return false }
func (v *stubVendor) SupportsNetwork(_ context.Context, _ *zerolog.Logger, _ common.VendorSettings, _ string) (bool, error) {
	return v.supports, v.supportsErr
}
func (v *stubVendor) GenerateConfigs(_ context.Context, _ *zerolog.Logger, base *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	v.gotBase = base
	v.gotSettings = settings
	if v.generateErr != nil {
		return nil, v.generateErr
	}
	if v.generate != nil {
		return v.generate(base), nil
	}
	return []*common.UpstreamConfig{base}, nil
}
func (v *stubVendor) GetVendorSpecificErrorIfAny(_ *common.NormalizedRequest, _ *http.Response, _ interface{}, _ map[string]interface{}) error {
	return nil
}

var _ common.Vendor = (*stubVendor)(nil)

func testLogger() *zerolog.Logger {
	l := zerolog.Nop()
	return &l
}

func TestApplyUpstreamIDTemplate_SubstitutesEveryPlaceholder(t *testing.T) {
	// The generated ID is the upstream's identity in metrics, logs and the
	// registry. An unsubstituted placeholder produces two upstreams with
	// the literal ID "<VENDOR>-<NETWORK>" that then collide.
	got := applyUpstreamIDTemplate("<VENDOR>-<PROVIDER>-<NETWORK>-<EVM_CHAIN_ID>", "alchemy", "prov1", "evm:42161")
	require.Equal(t, "alchemy-prov1-evm:42161-42161", got)
}

func TestApplyUpstreamIDTemplate_SubstitutesEveryOccurrence(t *testing.T) {
	got := applyUpstreamIDTemplate("<VENDOR>/<VENDOR>", "v", "p", "evm:1")
	require.Equal(t, "v/v", got, "a repeated placeholder must be replaced everywhere, not once")
}

func TestApplyUpstreamIDTemplate_NonEvmNetworkGetsNAForChainId(t *testing.T) {
	// A non-EVM network has no chain ID. Leaving the placeholder in place
	// would put a literal "<EVM_CHAIN_ID>" into a Prometheus label.
	got := applyUpstreamIDTemplate("<VENDOR>-<EVM_CHAIN_ID>", "v", "p", "svm:mainnet")
	require.Equal(t, "v-N/A", got)
	require.NotContains(t, got, "<EVM_CHAIN_ID>")
}

func TestApplyUpstreamIDTemplate_TemplateWithoutPlaceholdersIsVerbatim(t *testing.T) {
	require.Equal(t, "fixed-id", applyUpstreamIDTemplate("fixed-id", "v", "p", "evm:1"))
	require.Equal(t, "", applyUpstreamIDTemplate("", "v", "p", "evm:1"))
}

func TestProvider_BuildBaseUpstreamConfig_DerivesTypeAndChainIdForEvm(t *testing.T) {
	p := NewProvider(testLogger(), &common.ProviderConfig{
		Id: "prov1", Vendor: "stub", UpstreamIdTemplate: "<PROVIDER>-<EVM_CHAIN_ID>",
	}, &stubVendor{name: "stub"}, nil)

	cfg, err := p.buildBaseUpstreamConfig("evm:137")
	require.NoError(t, err)
	require.Equal(t, common.UpstreamTypeEvm, cfg.Type)
	require.NotNil(t, cfg.Evm)
	require.Equal(t, int64(137), cfg.Evm.ChainId,
		"a wrong chain ID routes traffic for one chain at another chain's nodes")
	require.Equal(t, "prov1-137", cfg.Id)
	require.Equal(t, "stub", cfg.VendorName)
}

func TestProvider_BuildBaseUpstreamConfig_RejectsAMalformedEvmChainId(t *testing.T) {
	// "evm:mainnet" is not a chain ID. Accepting it would boot an upstream
	// with chainId 0, which then matches nothing and silently serves errors.
	p := NewProvider(testLogger(), &common.ProviderConfig{Id: "prov1", Vendor: "stub"}, &stubVendor{name: "stub"}, nil)
	_, err := p.buildBaseUpstreamConfig("evm:mainnet")
	require.Error(t, err)
}

func TestProvider_BuildBaseUpstreamConfig_DoesNotDeriveAChainIdForANonEvmNetwork(t *testing.T) {
	// The generic path must not read a chain ID out of a network ID it
	// does not own. A parsed-anyway value would route btc traffic at an
	// EVM chain's nodes.
	p := NewProvider(testLogger(), &common.ProviderConfig{
		Id: "prov1", Vendor: "stub", UpstreamIdTemplate: "<PROVIDER>-<NETWORK>",
	}, &stubVendor{name: "stub"}, nil)

	cfg, err := p.buildBaseUpstreamConfig("btc:mainnet")
	require.NoError(t, err)
	require.Equal(t, "prov1-btc:mainnet", cfg.Id)
	if cfg.Evm != nil {
		require.Equal(t, int64(0), cfg.Evm.ChainId,
			"no chain ID may be invented for a network outside the evm family")
	}
}

func TestProvider_BuildBaseUpstreamConfig_TypeDefaultingDivergesBetweenItsTwoPaths(t *testing.T) {
	// Observed behaviour, pinned so a change is deliberate. For a non-EVM
	// network the fresh path runs UpstreamConfig.SetDefaults, which forces
	// Type=evm; the override path copies the override verbatim and leaves
	// Type empty. One provider therefore types its btc upstreams two
	// different ways depending on whether an override happened to match.
	p := NewProvider(testLogger(), &common.ProviderConfig{
		Id: "prov1", Vendor: "stub", UpstreamIdTemplate: "<PROVIDER>-<NETWORK>",
		Overrides: map[string]*common.UpstreamConfig{
			"btc:testnet": {Endpoint: "https://btc-testnet.example.com"},
		},
	}, &stubVendor{name: "stub"}, nil)

	fresh, err := p.buildBaseUpstreamConfig("btc:mainnet")
	require.NoError(t, err)
	require.Equal(t, common.UpstreamTypeEvm, fresh.Type,
		"fresh path: SetDefaults forces the evm type even for a btc network")

	overridden, err := p.buildBaseUpstreamConfig("btc:testnet")
	require.NoError(t, err)
	require.Equal(t, common.UpstreamType(""), overridden.Type,
		"override path: the copy skips SetDefaults, so the type stays empty")
}

func TestProvider_BuildBaseUpstreamConfig_AppliesTheWildcardOverride(t *testing.T) {
	// Overrides are how an operator pins per-chain settings. A missed
	// wildcard silently drops that whole block of config.
	p := NewProvider(testLogger(), &common.ProviderConfig{
		Id: "prov1", Vendor: "stub", UpstreamIdTemplate: "<PROVIDER>-<EVM_CHAIN_ID>",
		Overrides: map[string]*common.UpstreamConfig{
			"evm:42*": {Endpoint: "https://override.example.com", RateLimitBudget: "tight"},
		},
	}, &stubVendor{name: "stub"}, nil)

	matched, err := p.buildBaseUpstreamConfig("evm:42161")
	require.NoError(t, err)
	require.Equal(t, "https://override.example.com", matched.Endpoint)
	require.Equal(t, "tight", matched.RateLimitBudget)
	require.Equal(t, "prov1-42161", matched.Id,
		"the template still owns the ID — the override must not keep its own")

	unmatched, err := p.buildBaseUpstreamConfig("evm:1")
	require.NoError(t, err)
	require.Empty(t, unmatched.Endpoint, "a non-matching override must not leak into another chain")
	require.Empty(t, unmatched.RateLimitBudget)
}

func TestProvider_BuildBaseUpstreamConfig_DoesNotMutateTheOverride(t *testing.T) {
	// Overrides are shared across every network the provider serves. If
	// the builder wrote through to the map entry, the first chain built
	// would poison every later chain with its own ID and chain ID.
	override := &common.UpstreamConfig{Endpoint: "https://shared.example.com"}
	p := NewProvider(testLogger(), &common.ProviderConfig{
		Id: "prov1", Vendor: "stub", UpstreamIdTemplate: "<PROVIDER>-<EVM_CHAIN_ID>",
		Overrides: map[string]*common.UpstreamConfig{"evm:*": override},
	}, &stubVendor{name: "stub"}, nil)

	first, err := p.buildBaseUpstreamConfig("evm:1")
	require.NoError(t, err)
	second, err := p.buildBaseUpstreamConfig("evm:10")
	require.NoError(t, err)

	require.Equal(t, "prov1-1", first.Id)
	require.Equal(t, "prov1-10", second.Id)
	require.Equal(t, int64(1), first.Evm.ChainId,
		"building the second chain must not rewrite the first one's chain ID")
	require.Empty(t, override.Id, "the override in the config map must stay untouched")
	require.Nil(t, override.Evm)
}

func TestProvider_GenerateUpstreamConfigs_ExpandsEnvVarsInTheEndpoint(t *testing.T) {
	// Operators keep keys in the environment. An unexpanded ${VAR} dials a
	// literal dollar-sign host and the upstream never connects.
	t.Setenv("ERPC_TEST_PROVIDER_KEY", "s3cr3t")
	vendor := &stubVendor{name: "stub", generate: func(base *common.UpstreamConfig) []*common.UpstreamConfig {
		c := base.Copy()
		c.Endpoint = "https://rpc.example.com/${ERPC_TEST_PROVIDER_KEY}"
		return []*common.UpstreamConfig{c}
	}}
	p := NewProvider(testLogger(), &common.ProviderConfig{Id: "prov1", Vendor: "stub"}, vendor, nil)

	cfgs, err := p.GenerateUpstreamConfigs(context.Background(), testLogger(), "evm:1")
	require.NoError(t, err)
	require.Len(t, cfgs, 1)
	require.Equal(t, "https://rpc.example.com/s3cr3t", cfgs[0].Endpoint)
}

func TestProvider_GenerateUpstreamConfigs_ExpandsEveryGeneratedUpstream(t *testing.T) {
	// A vendor may fan one network out to several endpoints. Expanding only
	// the first would leave the rest dialling a literal ${VAR}.
	t.Setenv("ERPC_TEST_PROVIDER_KEY", "k")
	vendor := &stubVendor{name: "stub", generate: func(base *common.UpstreamConfig) []*common.UpstreamConfig {
		a, b := base.Copy(), base.Copy()
		a.Endpoint = "https://a.example.com/${ERPC_TEST_PROVIDER_KEY}"
		b.Endpoint = "https://b.example.com/${ERPC_TEST_PROVIDER_KEY}"
		return []*common.UpstreamConfig{a, b}
	}}
	p := NewProvider(testLogger(), &common.ProviderConfig{Id: "prov1", Vendor: "stub"}, vendor, nil)

	cfgs, err := p.GenerateUpstreamConfigs(context.Background(), testLogger(), "evm:1")
	require.NoError(t, err)
	require.Len(t, cfgs, 2)
	require.Equal(t, "https://a.example.com/k", cfgs[0].Endpoint)
	require.Equal(t, "https://b.example.com/k", cfgs[1].Endpoint)
}

func TestProvider_GenerateUpstreamConfigs_CopiesCreditUnitsOntoEveryUpstream(t *testing.T) {
	// The credit-unit override is how an operator prices a vendor's calls.
	// If it stops at the provider, the X-ERPC-Credits header under-reports
	// and the operator's cost accounting is wrong.
	vendor := &stubVendor{name: "stub", generate: func(base *common.UpstreamConfig) []*common.UpstreamConfig {
		a, b := base.Copy(), base.Copy()
		b.CreditUnits = map[string]int64{"eth_call": 99} // already priced by the vendor
		return []*common.UpstreamConfig{a, b}
	}}
	p := NewProvider(testLogger(), &common.ProviderConfig{
		Id: "prov1", Vendor: "stub",
		Settings: common.VendorSettings{"creditUnits": map[string]interface{}{"eth_getLogs": 75}},
	}, vendor, nil)

	cfgs, err := p.GenerateUpstreamConfigs(context.Background(), testLogger(), "evm:1")
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"eth_getLogs": 75}, cfgs[0].CreditUnits)
	require.Equal(t, map[string]int64{"eth_call": 99}, cfgs[1].CreditUnits,
		"a vendor that already priced an upstream keeps its own table")
}

func TestProvider_GenerateUpstreamConfigs_PropagatesTheVendorError(t *testing.T) {
	// A vendor that cannot resolve its endpoints must fail the boot loudly
	// rather than register zero upstreams and look healthy.
	boom := errors.New("vendor endpoint discovery failed")
	p := NewProvider(testLogger(), &common.ProviderConfig{Id: "prov1", Vendor: "stub"},
		&stubVendor{name: "stub", generateErr: boom}, nil)

	_, err := p.GenerateUpstreamConfigs(context.Background(), testLogger(), "evm:1")
	require.ErrorIs(t, err, boom)
}

func TestProvider_GenerateUpstreamConfigs_HandsTheVendorItsOwnSettings(t *testing.T) {
	vendor := &stubVendor{name: "stub"}
	settings := common.VendorSettings{"apiKey": "abc"}
	p := NewProvider(testLogger(), &common.ProviderConfig{Id: "prov1", Vendor: "stub", Settings: settings}, vendor, nil)

	_, err := p.GenerateUpstreamConfigs(context.Background(), testLogger(), "evm:1")
	require.NoError(t, err)
	require.Equal(t, settings, vendor.gotSettings)
	require.NotNil(t, vendor.gotBase)
	require.Equal(t, int64(1), vendor.gotBase.Evm.ChainId,
		"the vendor must receive a base config that already knows its chain")
}

func TestProvider_SupportsNetwork_IgnoreListWinsOverTheVendor(t *testing.T) {
	// ignoreNetworks is the operator's kill switch. A vendor that claims a
	// network must not be able to override it.
	vendor := &stubVendor{name: "stub", supports: true}
	p := NewProvider(testLogger(), &common.ProviderConfig{
		Id: "prov1", Vendor: "stub", IgnoreNetworks: []string{"evm:1"},
	}, vendor, nil)

	ok, err := p.SupportsNetwork(context.Background(), "evm:1")
	require.NoError(t, err)
	require.False(t, ok, "an explicitly ignored network must stay off")

	ok, err = p.SupportsNetwork(context.Background(), "evm:10")
	require.NoError(t, err)
	require.True(t, ok, "networks outside the ignore list still ask the vendor")
}

func TestProvider_SupportsNetwork_OnlyListIsExclusive(t *testing.T) {
	// onlyNetworks is an allow-list. Anything outside it must be refused
	// WITHOUT asking the vendor — otherwise a chatty vendor turns the
	// allow-list into a suggestion and issues discovery calls for chains
	// the operator deliberately excluded.
	vendor := &stubVendor{name: "stub", supports: true}
	p := NewProvider(testLogger(), &common.ProviderConfig{
		Id: "prov1", Vendor: "stub", OnlyNetworks: []string{"evm:1", "evm:10"},
	}, vendor, nil)

	for _, n := range []string{"evm:1", "evm:10"} {
		ok, err := p.SupportsNetwork(context.Background(), n)
		require.NoError(t, err)
		require.True(t, ok, "%s is on the allow-list", n)
	}
	ok, err := p.SupportsNetwork(context.Background(), "evm:137")
	require.NoError(t, err)
	require.False(t, ok, "a network outside onlyNetworks must be refused")
}

func TestProvider_SupportsNetwork_IgnoreBeatsOnly(t *testing.T) {
	// Both lists set, same network in each. The safe reading is that the
	// deny list wins; the other reading turns a kill switch into a no-op.
	p := NewProvider(testLogger(), &common.ProviderConfig{
		Id: "prov1", Vendor: "stub",
		OnlyNetworks:   []string{"evm:1"},
		IgnoreNetworks: []string{"evm:1"},
	}, &stubVendor{name: "stub", supports: true}, nil)

	ok, err := p.SupportsNetwork(context.Background(), "evm:1")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestProvider_SupportsNetwork_FallsThroughToTheVendorWithNoLists(t *testing.T) {
	// The unconfigured case is the common one: whatever the vendor says,
	// including its errors, reaches the caller unchanged.
	yes := NewProvider(testLogger(), &common.ProviderConfig{Id: "p", Vendor: "stub"}, &stubVendor{supports: true}, nil)
	ok, err := yes.SupportsNetwork(context.Background(), "evm:1")
	require.NoError(t, err)
	require.True(t, ok)

	no := NewProvider(testLogger(), &common.ProviderConfig{Id: "p", Vendor: "stub"}, &stubVendor{supports: false}, nil)
	ok, err = no.SupportsNetwork(context.Background(), "evm:1")
	require.NoError(t, err)
	require.False(t, ok)

	boom := errors.New("discovery timed out")
	bad := NewProvider(testLogger(), &common.ProviderConfig{Id: "p", Vendor: "stub"}, &stubVendor{supportsErr: boom}, nil)
	_, err = bad.SupportsNetwork(context.Background(), "evm:1")
	require.ErrorIs(t, err, boom)
}

func TestProvider_Id_EchoesTheConfiguredId(t *testing.T) {
	p := NewProvider(testLogger(), &common.ProviderConfig{Id: "prov-7", Vendor: "stub"}, &stubVendor{}, nil)
	require.Equal(t, "prov-7", p.Id())
}
