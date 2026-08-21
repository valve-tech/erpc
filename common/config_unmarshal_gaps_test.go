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

// For a key that only the CURRENT schema declares, the operator gets the wrong
// message: the old-format attempt decodes the same node into the unexported
// `oldNetworkDefaults` shadow, and that attempt's unknown-field complaint is
// what escapes. The `return originalErr` at common/config.go:812 never reaches
// the operator.
//
// This test PINS the defect. An operator who mistypes a valid key is told the
// key does not exist, and is shown an internal type name they cannot find in
// any documentation.
func TestNetworkDefaults_UnmarshalYAML_CurrentOnlyKeyReportsTheLegacyShadow(t *testing.T) {
	for _, tc := range []struct{ name, doc, wantKey string }{
		{"multiplexing", "multiplexing: yes-please\n", "multiplexing"},
		{"failover", "failover: 42\n", "failover"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var nd NetworkDefaults
			err := decodeYAMLStrict(tc.doc, &nd)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "field "+tc.wantKey+" not found in type common.oldNetworkDefaults",
				"today the legacy shadow's complaint is what the operator sees")
			assert.NotContains(t, err.Error(), "cannot unmarshal",
				"the real type mismatch is lost")
		})
	}
}

// NetworkConfig carries the same two-shape decoder and the same defect.
func TestNetworkConfig_UnmarshalYAML_CurrentOnlyKeyReportsTheLegacyShadow(t *testing.T) {
	var nc NetworkConfig
	err := decodeYAMLStrict("architecture: evm\nmultiplexing: yes-please\n", &nc)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "field multiplexing not found in type common.oldNetworkConfig")
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
