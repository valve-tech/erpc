package valvebilling

import (
	"context"
	"math/big"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Definition-of-done item 1: the flag switches the module OFF COMPLETELY.
//
// "Off" here is the absence of the object, not a branch inside it. New returns
// a nil *Module and a nil error, so there is no Redis connection, no goroutine,
// no timer and no state — nothing that could still act on a request.
func TestModule_TheFlagLeavesNothingRunning(t *testing.T) {
	t.Setenv(EnvEnabled, "")
	// Deliberately point the other variables at nonsense. A disabled module
	// must not read them, so a deployment that never wanted billing cannot be
	// broken by a wrong or missing value.
	t.Setenv(EnvRedisURL, "redis://this-host-does-not-exist:1")
	t.Setenv(EnvPepper, "too-short")

	cfg, err := LoadConfigFromEnv()
	require.NoError(t, err, "a cleared flag must not validate the rest of the configuration")
	require.False(t, cfg.Enabled)

	m, err := New(context.Background(), cfg, nil)
	require.NoError(t, err, "a disabled module is the success case, not an error")
	require.Nil(t, m, "a disabled module must be nil, so there is nothing left to run")

	assert.False(t, m.Enabled(), "Enabled must be safe on a nil receiver")
	assert.NoError(t, m.Close(), "Close must be safe on a nil receiver")
}

// The flag being absent and the flag being false must behave identically. A
// deployment that has never heard of this module is the common case.
func TestModule_AnAbsentFlagIsTheSameAsAFalseOne(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"absent", ""},
		{"false", "false"},
		{"zero", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.value == "" {
				os_unset(t, EnvEnabled)
			} else {
				t.Setenv(EnvEnabled, tc.value)
			}
			cfg, err := LoadConfigFromEnv()
			require.NoError(t, err)
			assert.False(t, cfg.Enabled)
		})
	}
}

// A disabled module that IS used anyway must say so rather than answer. Both
// neutral answers are silent failures: "allowed" bills nobody while looking
// healthy, "denied" breaks a deployment that never wanted billing.
func TestModule_ADisabledModuleRefusesToAnswer(t *testing.T) {
	var m *Module
	ctx := context.Background()

	_, err := m.HashKey("vk_x")
	assert.ErrorContains(t, err, "disabled")

	_, err = m.ResolveCost(1, "eth_call", ZeroAddress)
	assert.ErrorContains(t, err, "disabled")

	_, err = m.Authorize(ctx, AuthorizeInput{})
	assert.ErrorContains(t, err, "disabled")

	err = m.Capture(ctx, "acct_1", big.NewInt(1))
	assert.ErrorContains(t, err, "disabled")

	assert.Nil(t, m.Prices())
}

// Enabling without the values it needs must fail at BOOT, not at the first
// request. A module that started and then could not bill would serve every
// request free and look healthy.
func TestModule_EnablingWithoutItsConfigurationFailsLoudly(t *testing.T) {
	t.Run("no redis url", func(t *testing.T) {
		t.Setenv(EnvEnabled, "true")
		t.Setenv(EnvRedisURL, "")
		t.Setenv(EnvPepper, "0123456789012345678901234567890123456789")
		_, err := LoadConfigFromEnv()
		require.Error(t, err)
		assert.ErrorContains(t, err, EnvRedisURL)
		assert.ErrorContains(t, err, "refusing to guess")
	})

	t.Run("short pepper", func(t *testing.T) {
		t.Setenv(EnvEnabled, "true")
		t.Setenv(EnvRedisURL, "redis://127.0.0.1:6379")
		t.Setenv(EnvPepper, "short")
		_, err := LoadConfigFromEnv()
		require.Error(t, err)
		assert.ErrorContains(t, err, EnvPepper)
		assert.NotContains(t, err.Error(), "short",
			"the error must not echo the pepper, not even a short one")
	})

	t.Run("unparseable flag is not off", func(t *testing.T) {
		t.Setenv(EnvEnabled, "yes-please")
		_, err := LoadConfigFromEnv()
		require.Error(t, err,
			"an unparseable flag means somebody meant to enable this; defaulting to off would hide it")
	})
}

// A module that is enabled but cannot reach Redis must not start.
func TestModule_EnabledWithoutRedisDoesNotStart(t *testing.T) {
	cfg := Config{
		Enabled:  true,
		RedisURL: "redis://127.0.0.1:1",
		Pepper:   "0123456789012345678901234567890123456789",
	}
	m, err := New(context.Background(), cfg, NewPriceTable(nil, 6))
	require.Error(t, err)
	assert.Nil(t, m)
	assert.ErrorContains(t, err, "Redis")
}

func TestModule_EnabledWithoutAPriceTableDoesNotStart(t *testing.T) {
	cfg := Config{Enabled: true, RedisURL: "redis://127.0.0.1:6379", Pepper: "0123456789012345678901234567890123456789"}
	_, err := New(context.Background(), cfg, nil)
	assert.ErrorContains(t, err, "price table")
}

func os_unset(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	// t.Setenv cannot unset, and LoadConfigFromEnv treats "" as absent, which
	// is the behaviour under test.
}
