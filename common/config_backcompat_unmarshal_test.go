package common

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// These tests cover the wire-format surface an operator's EXISTING file hits on
// upgrade: the custom UnmarshalYAML / UnmarshalJSON methods in config.go.
//
// The YAML side already has coverage elsewhere. What is untested is the JSON
// side — the admin API and the TypeScript config loader both hand eRPC JSON,
// and the JSON decoders re-implement the legacy folding by hand rather than
// sharing the YAML code. A divergence there means the same config text behaves
// differently depending on which door it came through.

// ---------------------------------------------------------------------------
// Legacy flat timeout / hedge, over JSON
// ---------------------------------------------------------------------------

func TestTimeoutPolicyConfig_UnmarshalJSON_FoldsTheLegacyFlatFormIntoDuration(t *testing.T) {
	// `duration + quantile + minDuration + maxDuration` as four sibling keys is
	// the pre-AdaptiveDuration spelling. The runtime reads only Duration, so a
	// sibling that fails to fold is an adaptive timeout that silently runs as a
	// fixed one — no quantile, no floor, no ceiling.
	var c TimeoutPolicyConfig
	require.NoError(t, c.UnmarshalJSON([]byte(
		`{"duration":"5s","quantile":0.99,"minDuration":"200ms","maxDuration":"10s"}`)))

	require.NotNil(t, c.Duration)
	require.Equal(t, Duration(5*time.Second), c.Duration.Base)
	require.Equal(t, 0.99, c.Duration.Quantile)
	require.Equal(t, Duration(200*time.Millisecond), c.Duration.Min)
	require.Equal(t, Duration(10*time.Second), c.Duration.Max)
}

func TestTimeoutPolicyConfig_UnmarshalJSON_ReadsANumericDurationAsMilliseconds(t *testing.T) {
	// A JSON producer that emits numbers instead of duration strings is the
	// normal case for a generated config. Reading 200 as 200 nanoseconds would
	// make every timeout fire instantly.
	var c TimeoutPolicyConfig
	require.NoError(t, c.UnmarshalJSON([]byte(`{"quantile":0.5,"minDuration":200,"maxDuration":3000}`)))

	require.NotNil(t, c.Duration)
	require.Equal(t, Duration(200*time.Millisecond), c.Duration.Min)
	require.Equal(t, Duration(3*time.Second), c.Duration.Max)
}

func TestTimeoutPolicyConfig_UnmarshalJSON_NamesTheFieldThatFailedToParse(t *testing.T) {
	// A typo'd duration has to name its own key. "invalid duration" alone leaves
	// the operator grepping a config with a dozen duration fields in it.
	var c TimeoutPolicyConfig
	err := c.UnmarshalJSON([]byte(`{"minDuration":"5 seconds"}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "timeout.minDuration")

	err = c.UnmarshalJSON([]byte(`{"maxDuration":"later"}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "timeout.maxDuration")
}

func TestTimeoutPolicyConfig_UnmarshalJSON_TreatsNullAsNoPolicyAtAll(t *testing.T) {
	// `"timeout": null` is how a JSON producer says "not configured". Decoding
	// it into a zero-valued Duration would install a 0s timeout, which cancels
	// every request the moment it starts.
	c := TimeoutPolicyConfig{Duration: &AdaptiveDuration{Base: Duration(time.Second)}}
	require.NoError(t, c.UnmarshalJSON([]byte(`null`)))
	require.Equal(t, Duration(time.Second), c.Duration.Base, "null must not touch the receiver")

	require.NoError(t, c.UnmarshalJSON(nil))
	require.Equal(t, Duration(time.Second), c.Duration.Base)
}

func TestHedgePolicyConfig_UnmarshalJSON_FoldsTheLegacyFlatFormIntoDelay(t *testing.T) {
	// Same legacy shape as timeout, different key names. maxCount travels beside
	// the folded delay and must survive: a hedge policy with the delay right and
	// maxCount zero sends no hedges at all.
	var c HedgePolicyConfig
	require.NoError(t, c.UnmarshalJSON([]byte(
		`{"delay":"100ms","maxCount":3,"quantile":0.95,"minDelay":"50ms","maxDelay":"2s"}`)))

	require.Equal(t, 3, c.MaxCount)
	require.NotNil(t, c.Delay)
	require.Equal(t, Duration(100*time.Millisecond), c.Delay.Base)
	require.Equal(t, 0.95, c.Delay.Quantile)
	require.Equal(t, Duration(50*time.Millisecond), c.Delay.Min)
	require.Equal(t, Duration(2*time.Second), c.Delay.Max)
}

func TestHedgePolicyConfig_UnmarshalJSON_BuildsADelayWhenOnlySiblingsArePresent(t *testing.T) {
	// The purely-adaptive legacy form has no `delay` key at all. The folding has
	// to allocate the Delay object; without it the siblings land nowhere and the
	// policy hedges with no delay, doubling load on every request.
	var c HedgePolicyConfig
	require.NoError(t, c.UnmarshalJSON([]byte(`{"maxCount":2,"quantile":0.9}`)))

	require.NotNil(t, c.Delay)
	require.Equal(t, 0.9, c.Delay.Quantile)
	require.Equal(t, Duration(0), c.Delay.Base)
}

func TestHedgePolicyConfig_UnmarshalJSON_LeavesAnExplicitDelayObjectAlone(t *testing.T) {
	// The current spelling states quantile/min/max inside `delay`. A legacy
	// sibling must not overwrite what the object already says.
	var c HedgePolicyConfig
	require.NoError(t, c.UnmarshalJSON([]byte(
		`{"delay":{"base":"100ms","quantile":0.5,"min":"10ms"},"quantile":0.99,"minDelay":"999ms"}`)))

	require.Equal(t, 0.5, c.Delay.Quantile, "the object's own quantile must win")
	require.Equal(t, Duration(10*time.Millisecond), c.Delay.Min, "the object's own min must win")
}

func TestHedgePolicyConfig_UnmarshalJSON_NamesTheFieldThatFailedToParse(t *testing.T) {
	var c HedgePolicyConfig
	err := c.UnmarshalJSON([]byte(`{"minDelay":"soon"}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "hedge.minDelay")

	err = c.UnmarshalJSON([]byte(`{"maxDelay":"never"}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "hedge.maxDelay")
}

func TestHedgePolicyConfig_UnmarshalJSON_TreatsNullAsNoPolicyAtAll(t *testing.T) {
	c := HedgePolicyConfig{MaxCount: 4}
	require.NoError(t, c.UnmarshalJSON([]byte(`null`)))
	require.Equal(t, 4, c.MaxCount, "null must not touch the receiver")
}

// ---------------------------------------------------------------------------
// skipCacheRead is a pattern, not a boolean
// ---------------------------------------------------------------------------

func TestDirectiveDefaultsConfig_Unmarshal_NormalizesSkipCacheReadToAString(t *testing.T) {
	// skipCacheRead grew from a boolean into a connector-id glob. Both spellings
	// still parse, and both must arrive as a STRING because that is what the
	// cache layer matches against. A raw `true` reaching the matcher as a bool
	// matches no connector, so `skipCacheRead: true` would silently read the
	// cache anyway.
	t.Run("yaml boolean", func(t *testing.T) {
		var d DirectiveDefaultsConfig
		require.NoError(t, yaml.Unmarshal([]byte(`skipCacheRead: true`), &d))
		require.Equal(t, "true", d.SkipCacheRead)
	})
	t.Run("yaml glob", func(t *testing.T) {
		var d DirectiveDefaultsConfig
		require.NoError(t, yaml.Unmarshal([]byte(`skipCacheRead: "redis-*"`), &d))
		require.Equal(t, "redis-*", d.SkipCacheRead)
	})
	t.Run("json boolean", func(t *testing.T) {
		var d DirectiveDefaultsConfig
		require.NoError(t, d.UnmarshalJSON([]byte(`{"skipCacheRead":true}`)))
		require.Equal(t, "true", d.SkipCacheRead)
	})
	t.Run("json glob", func(t *testing.T) {
		var d DirectiveDefaultsConfig
		require.NoError(t, d.UnmarshalJSON([]byte(`{"skipCacheRead":"redis-*"}`)))
		require.Equal(t, "redis-*", d.SkipCacheRead)
	})
	t.Run("absent stays nil", func(t *testing.T) {
		// An absent key must stay nil, not become the string "<nil>", so a
		// project-level default can still fill it later.
		var d DirectiveDefaultsConfig
		require.NoError(t, yaml.Unmarshal([]byte(`retryEmpty: true`), &d))
		require.Nil(t, d.SkipCacheRead)

		var j DirectiveDefaultsConfig
		require.NoError(t, j.UnmarshalJSON([]byte(`{"retryEmpty":true}`)))
		require.Nil(t, j.SkipCacheRead)
	})
}

func TestDirectiveDefaultsConfig_Unmarshal_CarriesTheDeprecatedIntegrityFlags(t *testing.T) {
	// The `validate*` flags are read by migrateLegacyIntegrityChecks at startup.
	// If the decoder dropped them the migration would find nothing and the
	// operator's checks would quietly stop running — see the migration tests in
	// defaults_whole_config_test.go, which cannot fire without this decode.
	var d DirectiveDefaultsConfig
	require.NoError(t, yaml.Unmarshal([]byte(`
validateLogsBloomMatch: true
enforceLogIndexStrictIncrements: false
receiptsCountAtLeast: 3
`), &d))
	require.NotNil(t, d.DeprecatedValidateLogsBloomMatch)
	require.True(t, *d.DeprecatedValidateLogsBloomMatch)
	require.NotNil(t, d.DeprecatedEnforceLogIndexStrictIncrements)
	require.False(t, *d.DeprecatedEnforceLogIndexStrictIncrements)
	require.NotNil(t, d.DeprecatedReceiptsCountAtLeast)
	require.Equal(t, int64(3), *d.DeprecatedReceiptsCountAtLeast)
}

// ---------------------------------------------------------------------------
// Legacy `group:` / `cohort:` upstream keys
// ---------------------------------------------------------------------------

func TestUpstreamConfig_UnmarshalYAML_TurnsTheLegacyGroupAndCohortKeysIntoTags(t *testing.T) {
	// `group:` and `cohort:` predate `tags:`. Routing and failover read tags
	// only, so an upgraded config whose group never became `tier:<group>` puts
	// its fallback nodes into the default tier and sends them live traffic.
	var u UpstreamConfig
	require.NoError(t, yaml.Unmarshal([]byte(`
id: legacy
endpoint: http://a.example
group: fallback
cohort: eu
`), &u))
	require.ElementsMatch(t, []string{"tier:fallback", "cohort:eu"}, u.Tags)
}

func TestUpstreamConfig_UnmarshalYAML_DoesNotDuplicateATagTheOperatorAlreadyWrote(t *testing.T) {
	// A config mid-migration carries both spellings. Appending a second
	// `tier:fallback` would double-count the upstream anywhere tags are counted
	// rather than matched.
	var u UpstreamConfig
	require.NoError(t, yaml.Unmarshal([]byte(`
id: both
endpoint: http://a.example
tags:
  - tier:fallback
  - cohort:eu
group: fallback
cohort: eu
`), &u))
	require.Equal(t, []string{"tier:fallback", "cohort:eu"}, u.Tags)
}

func TestUpstreamConfig_UnmarshalYAML_MergesTheLegacyKeysOnTheLegacyFailsafePathToo(t *testing.T) {
	// The single-object `failsafe:` shape sends the decode down a second,
	// hand-written path. `group:` has to be honoured there as well, or the exact
	// oldest configs — the ones with BOTH legacy spellings — lose their tier.
	var u UpstreamConfig
	require.NoError(t, yaml.Unmarshal([]byte(`
id: oldest
endpoint: http://a.example
group: fallback
routing:
  scoreMultipliers:
    - network: "*"
      method: "*"
      overall: 2
failsafe:
  retry:
    maxAttempts: 3
`), &u))
	require.Equal(t, []string{"tier:fallback"}, u.Tags)
	require.Len(t, u.Failsafe, 1, "the object form becomes a one-entry list")
	require.Equal(t, "*", u.Failsafe[0].MatchMethod)
	require.NotNil(t, u.Routing, "routing must survive the legacy path")
}

func TestUpstreamConfig_UnmarshalYAML_ReportsAnUnknownKeyInsteadOfSilentlyDroppingIt(t *testing.T) {
	// A strict decoder is the only thing that catches a typo'd key. The legacy
	// fallback must not swallow the error, otherwise `endpiont:` parses clean
	// and the upstream comes up with no endpoint at all.
	//
	// Two guards produce this error — the unknown-field check before the legacy
	// attempt, and `return originalErr` after it. The legacy struct is a subset
	// of the strict one, so an unknown key always fails both, and deleting
	// either guard alone changes nothing. This asserts the OUTCOME.
	var u UpstreamConfig
	dec := yaml.NewDecoder(strings.NewReader(`
id: typo
endpiont: http://a.example
`))
	dec.KnownFields(true)
	err := dec.Decode(&u)
	require.Error(t, err)
	require.Contains(t, err.Error(), "endpiont")
}

// The legacy single-object `failsafe:` must not cost the operator any other
// key. This replaces TestUpstreamConfig_UnmarshalYAML_LegacyFailsafeObjectDropsNewerKeys,
// which pinned the defect.
//
// UpstreamConfig.UnmarshalYAML used to fall back to a hand-listed `oldShadow`
// struct whenever the canonical decode failed, which a legacy object-form
// `failsafe:` always caused. The list never grew with the struct. It dropped
// rateLimitCountMode and creditUnits when the defect was recorded, and by the
// time it was fixed it had drifted further and dropped chain and
// chainProbeInterval too. An operator writing credit-based accounting got flat
// per-request counting, no warning, and a budget draining at the wrong rate.
//
// The assertion is a PROPERTY, not a field list: the two forms must agree on
// everything except failsafe itself. Naming fields here would rot the same way
// oldShadow did. It cannot rot now — FailsafeConfigList takes both shapes, so
// there is only one decode and no parallel struct left to drift.
func TestUpstreamConfig_UnmarshalYAML_TheLegacyFailsafeObjectKeepsEveryOtherKey(t *testing.T) {
	// Every key the drifting shadow dropped, plus the ones it did copy.
	const body = `
id: u1
endpoint: http://a.example
rateLimitCountMode: credit
creditUnits:
  eth_call: 5
chain: mainnet
chainProbeInterval: 30s
ignoreMethods:
  - eth_coinbase
`
	var listForm UpstreamConfig
	require.NoError(t, yaml.Unmarshal([]byte(body+`
failsafe:
  - matchMethod: "*"
    retry:
      maxAttempts: 3
`), &listForm))

	var objectForm UpstreamConfig
	require.NoError(t, yaml.Unmarshal([]byte(body+`
failsafe:
  retry:
    maxAttempts: 3
`), &objectForm))

	require.Len(t, objectForm.Failsafe, 1, "the legacy failsafe itself still arrives")
	require.Equal(t, "*", objectForm.Failsafe[0].MatchMethod,
		"a single policy carried no matchMethod and applied to everything")

	// Compare everything else. The failsafe field is the one the two forms are
	// allowed to write differently, so it is the one thing excluded.
	listForm.Failsafe, objectForm.Failsafe = nil, nil
	require.Equal(t, listForm, objectForm,
		"the legacy failsafe shape must cost the operator no other key")
}

// ---------------------------------------------------------------------------
// Legacy `failsafe:` object at the network and networkDefaults levels
// ---------------------------------------------------------------------------

func TestNetworkDefaults_UnmarshalYAML_KeepsEveryOtherKeyBesideALegacyFailsafe(t *testing.T) {
	// NetworkDefaults decodes into the receiver itself, so the failed strict
	// pass leaves the non-failsafe keys populated and the legacy pass only
	// overwrites what it knows. That is what stops an upgraded config from
	// losing multiplexing or failover — assert it, because the sibling
	// UpstreamConfig path decodes into a separate struct and does lose them.
	var nd NetworkDefaults
	require.NoError(t, yaml.Unmarshal([]byte(`
rateLimitBudget: shared
multiplexing: false
failover:
  onDefaultsExhausted: true
failsafe:
  retry:
    maxAttempts: 3
`), &nd))

	require.Len(t, nd.Failsafe, 1)
	require.Equal(t, "*", nd.Failsafe[0].MatchMethod, "the object form gains the wildcard matcher")
	require.Equal(t, 3, nd.Failsafe[0].Retry.MaxAttempts)
	require.Equal(t, "shared", nd.RateLimitBudget)
	require.NotNil(t, nd.Multiplexing)
	require.False(t, *nd.Multiplexing)
	require.True(t, nd.Failover.Enabled())
}

func TestNetworkDefaults_UnmarshalYAML_ReportsAnUnknownKeyInsteadOfSilentlyDroppingIt(t *testing.T) {
	var nd NetworkDefaults
	dec := yaml.NewDecoder(strings.NewReader(`
rateLimitBidget: shared
`))
	dec.KnownFields(true)
	err := dec.Decode(&nd)
	require.Error(t, err)
	require.Contains(t, err.Error(), "rateLimitBidget")
}

func TestNetworkConfig_UnmarshalYAML_KeepsChainAndMultiplexingBesideALegacyFailsafe(t *testing.T) {
	// Same shape as NetworkDefaults. `chain:` is the network's identity for
	// every non-EVM family, so losing it would leave the network with no id at
	// all and no upstream would ever match it.
	var n NetworkConfig
	require.NoError(t, yaml.Unmarshal([]byte(`
architecture: evm
chain: mainnet
multiplexing: false
evm:
  chainId: 1
failsafe:
  retry:
    maxAttempts: 3
`), &n))

	require.Len(t, n.Failsafe, 1)
	require.Equal(t, "*", n.Failsafe[0].MatchMethod)
	require.Equal(t, "mainnet", n.Chain)
	require.NotNil(t, n.Multiplexing)
	require.False(t, *n.Multiplexing)
	require.Equal(t, int64(1), n.Evm.ChainId)
}

// ---------------------------------------------------------------------------
// Small accessors that decide routing
// ---------------------------------------------------------------------------

func TestNetworkConfig_MultiplexingEnabled_DefaultsOnAndHonoursAnExplicitFalse(t *testing.T) {
	// Multiplexing collapses concurrent identical requests into one upstream
	// call. It is on unless the operator says otherwise; reading a nil pointer
	// as "off" would multiply upstream load on every deployment that never
	// mentioned the key.
	var nilCfg *NetworkConfig
	require.True(t, nilCfg.MultiplexingEnabled())
	require.True(t, (&NetworkConfig{}).MultiplexingEnabled())

	off := false
	require.False(t, (&NetworkConfig{Multiplexing: &off}).MultiplexingEnabled())
	on := true
	require.True(t, (&NetworkConfig{Multiplexing: &on}).MultiplexingEnabled())
}

func TestNetworkConfig_NetworkId_RefusesToMintAnIdItCannotJustify(t *testing.T) {
	// The network id is the cache-key prefix and the upstream match key. An id
	// invented for a config eRPC does not understand would route requests to a
	// network no upstream serves, and poison the cache under that name.
	require.Equal(t, "", (&NetworkConfig{}).NetworkId(), "no architecture, no id")
	require.Equal(t, "", (&NetworkConfig{Architecture: ArchitectureEvm}).NetworkId(),
		"evm with no evm block has no chain id to name")
	require.Equal(t, "", (&NetworkConfig{Architecture: ArchitectureSvm}).NetworkId(),
		"svm with no svm block has no cluster to name")
	require.Equal(t, "", (&NetworkConfig{Architecture: ArchitectureSvm, Svm: &SvmNetworkConfig{}}).NetworkId(),
		"an svm block with no cluster still has nothing to name")

	require.Equal(t, "evm:1",
		(&NetworkConfig{Architecture: ArchitectureEvm, Evm: &EvmNetworkConfig{ChainId: 1}}).NetworkId())
	require.Equal(t, "svm:mainnet-beta",
		(&NetworkConfig{Architecture: ArchitectureSvm, Svm: &SvmNetworkConfig{Cluster: "mainnet-beta"}}).NetworkId())
}

func TestNetworkConfig_NetworkId_AsksTheChainFamilyAboutEveryOtherArchitecture(t *testing.T) {
	// Architectures beyond evm/svm name themselves in `chain:`, and only the
	// registered family knows which names are real. An unregistered
	// architecture is a typo; minting `btcc:mainnet` from it would create a
	// network that looks configured and answers nothing.
	registerForTest(t, &fakeFamily{
		name:      "idfam",
		transport: TransportJsonRpc,
		validId:   func(body string) bool { return body == "mainnet" },
	})

	require.Equal(t, "idfam:mainnet",
		(&NetworkConfig{Architecture: "idfam", Chain: "mainnet"}).NetworkId())
	require.Equal(t, "",
		(&NetworkConfig{Architecture: "idfam", Chain: "notachain"}).NetworkId(),
		"the family rejects the body, so there is no id")
	require.Equal(t, "",
		(&NetworkConfig{Architecture: "unregistered", Chain: "mainnet"}).NetworkId(),
		"an unregistered architecture must not mint an id")
}

func TestConfig_GetProjectConfig_FindsByIdAndReportsAMiss(t *testing.T) {
	// Every inbound request resolves its project by id. Returning the wrong
	// project would serve one tenant's upstreams to another.
	c := &Config{Projects: []*ProjectConfig{{Id: "alpha"}, {Id: "beta"}}}
	require.Equal(t, "beta", c.GetProjectConfig("beta").Id)
	require.Equal(t, "alpha", c.GetProjectConfig("alpha").Id)
	require.Nil(t, c.GetProjectConfig("gamma"))
	require.Nil(t, (&Config{}).GetProjectConfig("alpha"))
}

func TestConfig_HasRateLimiterBudget_MatchesOnIdOnly(t *testing.T) {
	// Validation refuses a config that references a budget nobody declared. A
	// false positive here would let eRPC boot with an upstream pointing at a
	// budget that does not exist, so its rate limit would never apply.
	c := &Config{RateLimiters: &RateLimiterConfig{Budgets: []*RateLimitBudgetConfig{{Id: "shared"}}}}
	require.True(t, c.HasRateLimiterBudget("shared"))
	require.False(t, c.HasRateLimiterBudget("other"))
	require.False(t, (&Config{}).HasRateLimiterBudget("shared"))
	require.False(t, (&Config{RateLimiters: &RateLimiterConfig{}}).HasRateLimiterBudget("shared"))
}

// ---------------------------------------------------------------------------
// Per-vendor credit-unit overrides
// ---------------------------------------------------------------------------

func TestVendorSettings_CreditUnits_NormalizesEveryNumericShapeYamlAndJsonProduce(t *testing.T) {
	// `providers[].settings.creditUnits` is what makes credit-mode rate limiting
	// charge the right amount per method. YAML gives ints, sonic gives float64,
	// and a hand-built config gives int64 — all three have to normalize, or the
	// method they describe falls back to the vendor default and the budget
	// drains at the wrong rate.
	s := VendorSettings{"creditUnits": map[string]interface{}{
		"eth_call":    10,
		"eth_getLogs": int64(75),
		"trace_block": float64(120),
		"*":           4,
	}}
	require.Equal(t, map[string]int64{
		"eth_call":    10,
		"eth_getLogs": 75,
		"trace_block": 120,
		"*":           4,
	}, s.CreditUnits())
}

func TestVendorSettings_CreditUnits_ReturnsNilWhenThereIsNothingUsable(t *testing.T) {
	// Nil means "the vendor's own table governs". An empty non-nil map would
	// read as "this vendor charges nothing for anything", which switches credit
	// accounting off for that provider without saying so.
	require.Nil(t, VendorSettings{}.CreditUnits())
	require.Nil(t, VendorSettings{"creditUnits": "not-a-map"}.CreditUnits())
	require.Nil(t, VendorSettings{"creditUnits": map[string]interface{}{}}.CreditUnits())
	require.Nil(t, VendorSettings{"creditUnits": map[string]interface{}{"eth_call": "ten"}}.CreditUnits(),
		"a non-numeric value is not a credit count, and nothing else is usable")
}

func TestVendorSettings_CreditUnits_KeepsTheUsableEntriesBesideAnUnusableOne(t *testing.T) {
	// One bad entry must not discard the table. The operator loses the method
	// they typo'd, not every method they got right.
	s := VendorSettings{"creditUnits": map[string]interface{}{
		"eth_call":    10,
		"eth_getLogs": "seventy-five",
	}}
	require.Equal(t, map[string]int64{"eth_call": 10}, s.CreditUnits())
}
