package valvebilling

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tierEnv is one valid setting of the five variables. The numbers are the ones
// the monorepo's meter.ts uses; they live here in a test rather than in the
// loader, because a default in the loader is the same number written down in
// two repositories.
var tierEnv = map[string]string{
	EnvSlowThresholdUSD:  "5",
	EnvFullCreditsPerSec: "5000",
	EnvSlowCreditsPerSec: "500",
	EnvFullRateRPS:       "1000",
	EnvSlowRateRPS:       "100",
	EnvCreditsPerUSD:     "1000000000",
}

func setTierEnv(t *testing.T) {
	t.Helper()
	for name, value := range tierEnv {
		t.Setenv(name, value)
	}
}

// unsetEnv really removes a variable. t.Setenv cannot, but it registers the
// restore, so setting and then removing leaves the environment clean.
func unsetEnv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "restored-by-cleanup")
	require.NoError(t, os.Unsetenv(name))
}

// The five names are the TypeScript relay's, and each one lands on the field
// the script reads it as. A swapped pair here would rate-limit the wrong tier
// with nothing going red.
func TestLoadTierLimits_ReadsTheMonorepoNames(t *testing.T) {
	setTierEnv(t)

	got, err := LoadTierLimitsFromEnv()
	require.NoError(t, err)

	assert.Equal(t, int64(5_000_000_000), got.SlowThreshold,
		"the threshold is dollars in, credits out, at the configured peg of 10^9")
	assert.Equal(t, int64(5000), got.FullCPS)
	assert.Equal(t, int64(500), got.SlowCPS)
	assert.Equal(t, int64(1000), got.FullRPS)
	assert.Equal(t, int64(100), got.SlowRPS)
}

// The four per-key quotas are NOT deployment-wide. The monorepo reads them
// from the key record and passes them per request, so this loader must leave
// them alone rather than invent one value for every key on the relay.
func TestLoadTierLimits_LeavesThePerKeyQuotasToTheCaller(t *testing.T) {
	setTierEnv(t)

	got, err := LoadTierLimitsFromEnv()
	require.NoError(t, err)

	assert.Zero(t, got.DayLimit, "requests per day belongs to the key record")
	assert.Zero(t, got.CUSecondLimit, "compute units per second belongs to the key record")
	assert.Zero(t, got.CUDayLimit, "compute units per day belongs to the key record")
	assert.Zero(t, got.KeyRPS, "the per-key request rate belongs to the key record")
}

// The hazard this loader exists to prevent.
//
// authorize.lua switches a gate OFF at zero or below. For the two credits
// buckets that removes the only bound on overdraft, because balance
// sufficiency is read and never reserved. So every way of arriving at zero has
// to be refused, and the four ways are not the same accident:
//
//   - unset — the deployment never supplied the value;
//   - an empty string — a template expanded to nothing, and os.Getenv reports
//     it exactly as it reports an unset variable;
//   - a literal "0" — somebody meant "no limit" and wrote "no gate";
//   - a negative number — a typo that a `< 0` check would let through.
func TestLoadTierLimits_RefusesEveryRouteToADisabledGate(t *testing.T) {
	const unset = "\x00unset\x00"

	for _, name := range []string{
		EnvSlowThresholdUSD,
		EnvFullCreditsPerSec,
		EnvSlowCreditsPerSec,
		EnvFullRateRPS,
		EnvSlowRateRPS,
	} {
		for _, tc := range []struct{ label, value, want, why string }{
			{
				label: "unset", value: unset, want: "is unset",
				why: "an unset variable must fail at boot, not become a zero",
			},
			{
				label: "empty string", value: "", want: "is empty",
				why: "an empty string is not a zero and not a default; os.Getenv reports it as \"\" either way",
			},
			{
				label: "literal zero", value: "0", want: "greater than zero",
				why: "zero is what the script reads as \"no gate\"; this is the monorepo's live trap",
			},
			{
				label: "negative", value: "-1", want: "greater than zero",
				why: "a negative limit also skips the gate, so a `< 0` check would not be enough",
			},
		} {
			t.Run(name+"/"+tc.label, func(t *testing.T) {
				setTierEnv(t)
				if tc.value == unset {
					unsetEnv(t, name)
				} else {
					t.Setenv(name, tc.value)
				}

				_, err := LoadTierLimitsFromEnv()
				require.Error(t, err, tc.why)
				assert.ErrorContains(t, err, name, "the error must name the variable an operator has to fix")
				assert.ErrorContains(t, err, tc.want, tc.why)
			})
		}
	}
}

// Whitespace is the same accident as an empty string. A value read from a file
// arrives with a newline on it, and a variable holding one space is empty.
func TestLoadTierLimits_TrimsAValueAndRefusesABlankOne(t *testing.T) {
	t.Run("a trailing newline is not an error", func(t *testing.T) {
		setTierEnv(t)
		t.Setenv(EnvFullCreditsPerSec, " 5000\n")
		got, err := LoadTierLimitsFromEnv()
		require.NoError(t, err)
		assert.Equal(t, int64(5000), got.FullCPS)
	})

	t.Run("whitespace alone is empty", func(t *testing.T) {
		setTierEnv(t)
		t.Setenv(EnvFullCreditsPerSec, "   ")
		_, err := LoadTierLimitsFromEnv()
		require.Error(t, err)
		assert.ErrorContains(t, err, "is empty")
	})
}

// A threshold small enough to round away is a zero threshold. The rounding is
// where a positive input can still reach the value the script treats as "no
// slow tier", so the check has to run on the credits, not on the dollars.
func TestLoadTierLimits_RefusesAThresholdThatRoundsToZero(t *testing.T) {
	setTierEnv(t)
	t.Setenv(EnvSlowThresholdUSD, "0.0000000001")
	_, err := LoadTierLimitsFromEnv()
	require.Error(t, err, "0.1 of a credit rounds to zero, which puts every account on the FULL tier")
	assert.ErrorContains(t, err, "greater than zero")

	// One credit is the smallest threshold that survives.
	t.Setenv(EnvSlowThresholdUSD, "0.000000001")
	got, err := LoadTierLimitsFromEnv()
	require.NoError(t, err)
	assert.Equal(t, int64(1), got.SlowThreshold)
}

// ParseFloat accepts "NaN" and "Inf", and every comparison against NaN is
// false, so a range check alone would pass NaN straight through to Redis.
// TypeScript's parseFloat has the same hole and meter.ts guards it.
func TestLoadTierLimits_RefusesAValueThatIsNotAFiniteNumber(t *testing.T) {
	for _, value := range []string{"NaN", "nan", "Inf", "+Inf", "-Inf", "five"} {
		t.Run("threshold/"+value, func(t *testing.T) {
			setTierEnv(t)
			t.Setenv(EnvSlowThresholdUSD, value)
			_, err := LoadTierLimitsFromEnv()
			require.Error(t, err, "%q must not reach the script", value)
			assert.ErrorContains(t, err, EnvSlowThresholdUSD)
		})
	}

	// The four gate limits are whole numbers of credits and of requests. A
	// fraction or an exponent is a value somebody meant differently.
	for _, value := range []string{"5000.5", "1e3", "many"} {
		t.Run("cps/"+value, func(t *testing.T) {
			setTierEnv(t)
			t.Setenv(EnvFullCreditsPerSec, value)
			_, err := LoadTierLimitsFromEnv()
			require.Error(t, err, "%q must not reach the script", value)
			assert.ErrorContains(t, err, "whole number")
		})
	}
}

// The peg is required configuration rather than a Go constant, and this is
// why. It is already a hard-coded literal in two places in the monorepo that
// DISAGREE by a factor of a thousand — meter.ts says 10^9, the web
// calculator's pricing.ts says 10^6 — so the landing page over-quotes by that
// much. A third literal here would not stop them disagreeing; it would add a
// third place to go stale.
//
// Required configuration turns that into a deployment error instead.
func TestLoadTierLimits_RefusesAnAbsentOrUselessPeg(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"unset", ""},
		{"empty", " "},
		{"zero", "0"},
		{"negative", "-1000000000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setTierEnv(t)
			if tc.name == "unset" {
				unsetEnv(t, EnvCreditsPerUSD)
			} else {
				t.Setenv(EnvCreditsPerUSD, tc.value)
			}
			_, err := LoadTierLimitsFromEnv()
			require.Error(t, err, "a %s peg must be refused, not guessed", tc.name)
			assert.Contains(t, err.Error(), EnvCreditsPerUSD)
		})
	}
}

// The peg actually drives the conversion. If it were ignored — the failure a
// leftover constant would cause — the threshold would come out the same at
// every peg, and this catches that.
func TestLoadTierLimits_ThePegConvertsTheThreshold(t *testing.T) {
	for _, tc := range []struct {
		peg  string
		want int64
	}{
		{"1000000000", 5_000_000_000}, // the ledger's 10^9
		{"1000000", 5_000_000},        // the web calculator's 10^6
	} {
		t.Run(tc.peg, func(t *testing.T) {
			setTierEnv(t)
			t.Setenv(EnvCreditsPerUSD, tc.peg)
			got, err := LoadTierLimitsFromEnv()
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.SlowThreshold,
				"$5 at a peg of %s credits per dollar", tc.peg)
		})
	}
}
