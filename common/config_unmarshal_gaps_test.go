package common

import (
	"bytes"
	"testing"
	"time"

	pb "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// decodeYAMLStrict decodes into target the way LoadConfig does, so a test can
// pose a schema question about one struct instead of a whole config.
func decodeYAMLStrict(doc string, target any) error {
	dec := yaml.NewDecoder(bytes.NewReader([]byte(doc)))
	dec.KnownFields(true)
	return dec.Decode(target)
}

// ---------------------------------------------------------------------------
// RateLimitPeriod.Unit
// ---------------------------------------------------------------------------

// Unit is the wire mapping onto the Envoy ratelimit protobuf. A wrong entry
// here silently changes the window a shared rate limiter enforces: the operator
// writes "minute" and the limiter counts by the hour.
func TestRateLimitPeriod_Unit_MapsEveryPeriodToItsEnvoyUnit(t *testing.T) {
	for _, tc := range []struct {
		period RateLimitPeriod
		want   pb.RateLimitResponse_RateLimit_Unit
	}{
		{RateLimitPeriodSecond, pb.RateLimitResponse_RateLimit_SECOND},
		{RateLimitPeriodMinute, pb.RateLimitResponse_RateLimit_MINUTE},
		{RateLimitPeriodHour, pb.RateLimitResponse_RateLimit_HOUR},
		{RateLimitPeriodDay, pb.RateLimitResponse_RateLimit_DAY},
		{RateLimitPeriodWeek, pb.RateLimitResponse_RateLimit_WEEK},
		{RateLimitPeriodMonth, pb.RateLimitResponse_RateLimit_MONTH},
		{RateLimitPeriodYear, pb.RateLimitResponse_RateLimit_YEAR},
	} {
		t.Run(tc.period.String(), func(t *testing.T) {
			assert.Equal(t, tc.want, tc.period.Unit())
		})
	}
}

// A period outside the enum must map to UNKNOWN rather than to whichever unit
// happens to sit at that offset. The unknown case is the one an upgrade or a
// hand-written integer can produce.
func TestRateLimitPeriod_Unit_UnknownPeriodMapsToUnknown(t *testing.T) {
	for _, p := range []RateLimitPeriod{-1, 7, 99} {
		assert.Equal(t, pb.RateLimitResponse_RateLimit_UNKNOWN, p.Unit(),
			"period %d is not in the enum", int(p))
		assert.Equal(t, "unknown", p.String())
	}
}

// ---------------------------------------------------------------------------
// FailoverConfig.Enabled
// ---------------------------------------------------------------------------

// Enabled decides whether traffic may reach the fallback tier. It is read on
// configs that never set the key, so the nil receiver must answer false rather
// than panic.
func TestFailoverConfig_Enabled(t *testing.T) {
	var nilCfg *FailoverConfig
	assert.False(t, nilCfg.Enabled(), "an absent failover block enables nothing")

	assert.False(t, (&FailoverConfig{}).Enabled(), "an empty block leaves the key unset")

	off := false
	assert.False(t, (&FailoverConfig{OnDefaultsExhausted: &off}).Enabled())

	on := true
	assert.True(t, (&FailoverConfig{OnDefaultsExhausted: &on}).Enabled())
}

// ---------------------------------------------------------------------------
// RateLimitRuleConfig.MarshalZerologObject
// ---------------------------------------------------------------------------

// The rule is logged whenever a budget is built or a limit is hit. Every field
// the operator wrote must appear.
//
// waitTimeMs is WRONG today: WaitTime is a common.Duration, which counts
// nanoseconds, and config.go:3105 prints it with %d under a key that says
// milliseconds. A 1s waitTime logs as 1000000000 "ms". This test PINS that
// number; a fix should change it to 1000 and update this assertion.
func TestRateLimitRuleConfig_MarshalZerologObject(t *testing.T) {
	prevLevel := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.TraceLevel)
	t.Cleanup(func() { zerolog.SetGlobalLevel(prevLevel) })

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	rule := &RateLimitRuleConfig{
		Method:   "eth_call",
		MaxCount: 500,
		Period:   RateLimitPeriodMinute,
		WaitTime: Duration(time.Second),
	}
	logger.Info().EmbedObject(rule).Msg("budget rule")

	line := buf.String()
	assert.Contains(t, line, `"method":"eth_call"`)
	assert.Contains(t, line, `"maxCount":500`)
	assert.Contains(t, line, `"period":"minute"`)
	assert.Contains(t, line, `"waitTimeMs":"1000000000"`,
		"today the key says milliseconds and the value is nanoseconds")
}

// ---------------------------------------------------------------------------
// Legacy back-compat unmarshalling
// ---------------------------------------------------------------------------

// NetworkDefaults tries the current schema, then the old one. For a key that
// both shapes declare, the operator gets the real type mismatch back.
func TestNetworkDefaults_UnmarshalYAML_BothShapesFailReturnsTheRealError(t *testing.T) {
	var nd NetworkDefaults
	err := decodeYAMLStrict("rateLimitBudget: [a, b]\n", &nd)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot unmarshal !!seq into string",
		"the operator must see the type mismatch they actually wrote")
}

// For a key that only the CURRENT schema declares, the operator now gets the
// real type mismatch. There is no second decode to mask it: the legacy single
// `failsafe:` object was the only reason NetworkDefaults kept a hand-listed
// shadow struct, and FailsafeConfigList takes that shape itself.
//
// This replaces TestNetworkDefaults_UnmarshalYAML_CurrentOnlyKeyReportsTheLegacyShadow,
// which pinned the defect: an operator who mistyped a valid key was told the
// key did not exist, and was shown `common.oldNetworkDefaults`, an unexported
// type declared inside a function body that appears in no documentation and in
// no config they can edit.
func TestNetworkDefaults_UnmarshalYAML_ReportsTheRealTypeMismatchForACurrentOnlyKey(t *testing.T) {
	for _, tc := range []struct{ name, doc, wantMismatch string }{
		{"multiplexing", "multiplexing: yes-please\n", "cannot unmarshal !!str `yes-please` into bool"},
		{"failover", "failover: 42\n", "cannot unmarshal !!int `42` into common.FailoverConfig"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var nd NetworkDefaults
			err := decodeYAMLStrict(tc.doc, &nd)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMismatch,
				"the operator must see the type mismatch they actually wrote")
			assert.NotContains(t, err.Error(), "oldNetworkDefaults",
				"no internal shadow type may appear in an operator-facing error")
			assert.NotContains(t, err.Error(), "not found in type",
				"the key exists; saying otherwise sends the operator hunting for a typo")
		})
	}
}

// NetworkConfig carried the same two-shape decoder, and loses it the same way.
func TestNetworkConfig_UnmarshalYAML_ReportsTheRealTypeMismatchForACurrentOnlyKey(t *testing.T) {
	var nc NetworkConfig
	err := decodeYAMLStrict("architecture: evm\nmultiplexing: yes-please\n", &nc)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot unmarshal !!str `yes-please` into bool")
	assert.NotContains(t, err.Error(), "oldNetworkConfig")
}

// ---------------------------------------------------------------------------
// Failsafe policy decoders
//
// The legacy-sibling folding and the per-key error messages already have tests
// in config_backcompat_unmarshal_test.go. What is left here is the plain
// decode failure: the payload is not a policy at all.
// ---------------------------------------------------------------------------

// A malformed policy must fail the decode. Falling through would leave the
// policy at its zero value, which means no timeout and no hedge at all.
func TestFailsafePolicyDecoders_RejectAMalformedDocument(t *testing.T) {
	t.Run("timeout yaml", func(t *testing.T) {
		var c TimeoutPolicyConfig
		require.Error(t, decodeYAMLStrict("quantile: not-a-number\n", &c))
	})

	t.Run("timeout json", func(t *testing.T) {
		var c TimeoutPolicyConfig
		require.Error(t, c.UnmarshalJSON([]byte(`{"quantile":`)))
	})

	t.Run("hedge yaml", func(t *testing.T) {
		var c HedgePolicyConfig
		require.Error(t, decodeYAMLStrict("maxCount: not-a-number\n", &c))
	})

	t.Run("hedge json", func(t *testing.T) {
		var c HedgePolicyConfig
		require.Error(t, c.UnmarshalJSON([]byte(`{"maxCount":`)))
	})
}

// ---------------------------------------------------------------------------
// DirectiveDefaultsConfig
//
// The bool/string normalisation has a test in
// config_backcompat_unmarshal_test.go. These are the two cases it leaves open.
// ---------------------------------------------------------------------------

// An absent skipCacheRead must stay absent. Normalising a nil interface with
// fmt would turn it into the literal string "<nil>", which then reads as an
// explicit directive at request time.
func TestDirectiveDefaultsConfig_AbsentSkipCacheReadStaysAbsent(t *testing.T) {
	var d DirectiveDefaultsConfig
	require.NoError(t, d.UnmarshalJSON([]byte(`{}`)))
	assert.Nil(t, d.SkipCacheRead)

	var y DirectiveDefaultsConfig
	require.NoError(t, decodeYAMLStrict("retryEmpty: true\n", &y))
	assert.Nil(t, y.SkipCacheRead)
}

// A malformed directive block must fail rather than silently apply nothing.
func TestDirectiveDefaultsConfig_RejectsAMalformedDocument(t *testing.T) {
	var d DirectiveDefaultsConfig
	require.Error(t, d.UnmarshalJSON([]byte(`{"skipCacheRead":`)))
	require.Error(t, decodeYAMLStrict("noSuchDirective: 1\n", &d))
}

// ---------------------------------------------------------------------------
// SelectionPolicyConfig legacy capture
// ---------------------------------------------------------------------------

// The legacy selectionPolicy keys are captured for the translator hook. When
// none of them appear the stash must stay nil, otherwise the hook would rewrite
// a policy the operator wrote in the current schema.
func TestSelectionPolicyConfig_UnmarshalYAML_LegacyStash(t *testing.T) {
	t.Run("legacy keys are stashed", func(t *testing.T) {
		var s SelectionPolicyConfig
		require.NoError(t, decodeYAMLStrict("resampleCount: 4\nresampleExcluded: true\n", &s))
		require.NotNil(t, s.LegacySelectionPolicy)
		assert.Equal(t, 4, s.LegacySelectionPolicy.ResampleCount)
		assert.True(t, s.LegacySelectionPolicy.ResampleExcluded)
	})

	t.Run("a current-schema policy stashes nothing", func(t *testing.T) {
		var s SelectionPolicyConfig
		require.NoError(t, decodeYAMLStrict("evalInterval: 30s\n", &s))
		assert.Nil(t, s.LegacySelectionPolicy, "nothing legacy was written, so nothing may be stashed")
	})

	t.Run("a malformed document is rejected", func(t *testing.T) {
		var s SelectionPolicyConfig
		require.Error(t, decodeYAMLStrict("resampleCount: not-a-number\n", &s))
	})
}
