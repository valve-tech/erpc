package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A network-level `integrity` block layers over the project-level one. Each
// axis overrides independently, so an operator who sets one knob on one network
// keeps every other knob the project already decided. These tests cover the
// axes the existing merge test does not reach, and pin that merging leaves the
// project block untouched — the same block every other network merges from.

func fullIntegrityConfig() *IntegrityConfig {
	enabled := true
	return &IntegrityConfig{
		IntegritySettings: IntegritySettings{
			Level:           "corroborated",
			Checks:          map[string]*IntegrityCheckConfig{"blockHash": {Enabled: &enabled, Params: map[string]string{"depth": "8"}}},
			Budget:          &IntegrityBudgetConfig{MaxPerSecond: 10},
			InvalidBehavior: &IntegrityInvalidBehaviorConfig{Finalized: "reject"},
			ReorgWindow:     32,
			ObserveOnly:     &enabled,
			StateProbe:      &IntegrityStateProbeConfig{Enabled: &enabled, Interval: Duration(2 * time.Second)},
			Follow:          &IntegrityFollowConfig{Enabled: &enabled, Interval: Duration(time.Second), MaxBlocksPerTick: 16},
			MisbehaviorsDestination: &MisbehaviorsDestinationConfig{
				Type: MisbehaviorsDestinationTypeFile,
				Path: "/var/log/erpc",
			},
		},
		HeaderMode: "profiles",
		Profiles:   map[string]*IntegritySettings{"strict": {Level: "authoritative"}},
	}
}

func TestMergeIntegrityConfig_OverridesEveryAxisIndependently(t *testing.T) {
	base := fullIntegrityConfig()
	off := false

	over := &IntegrityConfig{
		IntegritySettings: IntegritySettings{
			ReorgWindow: 256,
			ObserveOnly: &off,
			StateProbe:  &IntegrityStateProbeConfig{Enabled: &off},
			Follow:      &IntegrityFollowConfig{Enabled: &off, MaxBlocksPerTick: 4},
			MisbehaviorsDestination: &MisbehaviorsDestinationConfig{
				Type: MisbehaviorsDestinationTypeS3,
				Path: "s3://bucket/prefix/",
			},
		},
	}

	merged := MergeIntegrityConfig(base, over)

	// Overridden axes take the network's value.
	require.Equal(t, 256, merged.ReorgWindow)
	require.False(t, *merged.ObserveOnly)
	require.False(t, merged.StateProbe.IsEnabled())
	require.False(t, merged.Follow.IsEnabled())
	require.Equal(t, 4, merged.Follow.MaxBlocksPerTick)
	require.Equal(t, MisbehaviorsDestinationTypeS3, merged.MisbehaviorsDestination.Type)

	// Untouched axes keep the project's value.
	require.Equal(t, "corroborated", merged.Level)
	require.Equal(t, "profiles", merged.HeaderMode)
	require.Equal(t, 10, merged.Budget.MaxPerSecond)
	require.Equal(t, "reject", merged.InvalidBehavior.Finalized)
	require.Contains(t, merged.Checks, "blockHash")
	require.Contains(t, merged.Profiles, "strict")
}

func TestMergeIntegrityConfig_LeavesTheProjectBlockUntouched(t *testing.T) {
	base := fullIntegrityConfig()
	off := false

	over := &IntegrityConfig{
		IntegritySettings: IntegritySettings{
			Level:       "authoritative",
			ReorgWindow: 256,
			ObserveOnly: &off,
			Budget:      &IntegrityBudgetConfig{MaxPerSecond: 99},
			Checks: map[string]*IntegrityCheckConfig{
				"blockHash": {Params: map[string]string{"depth": "64"}},
				"logsBloom": {},
			},
		},
		HeaderMode: "full",
		Profiles:   map[string]*IntegritySettings{"lax": {Level: "off"}},
	}

	merged := MergeIntegrityConfig(base, over)
	require.Len(t, merged.Checks, 2)
	require.Len(t, merged.Profiles, 2)

	// Mutating the merged result must not reach either input.
	merged.Checks["blockHash"].Params["depth"] = "999"
	merged.Profiles["strict"].Level = "off"
	merged.Budget.MaxPerSecond = 0

	require.Equal(t, "corroborated", base.Level)
	require.Equal(t, 32, base.ReorgWindow)
	require.True(t, *base.ObserveOnly)
	require.Equal(t, 10, base.Budget.MaxPerSecond)
	require.Len(t, base.Checks, 1, "the project's check map must not gain the network's checks")
	require.Equal(t, "8", base.Checks["blockHash"].Params["depth"])
	require.Len(t, base.Profiles, 1)
	require.Equal(t, "authoritative", base.Profiles["strict"].Level)
	require.Equal(t, "64", over.Checks["blockHash"].Params["depth"])
}

func TestMergeIntegrityConfig_CopiesTheSurvivingSideWhenTheOtherIsNil(t *testing.T) {
	base := fullIntegrityConfig()

	fromBase := MergeIntegrityConfig(base, nil)
	require.NotSame(t, base, fromBase)
	fromBase.Budget.MaxPerSecond = 0
	require.Equal(t, 10, base.Budget.MaxPerSecond, "merging with a nil override must still copy")

	fromOver := MergeIntegrityConfig(nil, base)
	require.NotSame(t, base, fromOver)
	fromOver.Checks["blockHash"].Params["depth"] = "0"
	require.Equal(t, "8", base.Checks["blockHash"].Params["depth"])

	require.Nil(t, MergeIntegrityConfig(nil, nil))
}

func TestIntegrityFollowAndStateProbe_CopyAndEnablement(t *testing.T) {
	t.Run("a nil config is disabled and copies to nil", func(t *testing.T) {
		var follow *IntegrityFollowConfig
		require.False(t, follow.IsEnabled())
		require.Nil(t, follow.Copy())

		var probe *IntegrityStateProbeConfig
		require.False(t, probe.IsEnabled())
		require.Nil(t, probe.Copy())
	})

	t.Run("a config with no explicit enabled flag is disabled", func(t *testing.T) {
		require.False(t, (&IntegrityFollowConfig{Interval: Duration(time.Second)}).IsEnabled())
		require.False(t, (&IntegrityStateProbeConfig{Interval: Duration(time.Second)}).IsEnabled())
	})

	t.Run("the copy carries its own enabled flag", func(t *testing.T) {
		on := true
		follow := &IntegrityFollowConfig{Enabled: &on, Interval: Duration(3 * time.Second), MaxBlocksPerTick: 7}
		followCopy := follow.Copy()
		*follow.Enabled = false

		require.True(t, followCopy.IsEnabled(), "turning the original off must not turn the copy off")
		require.Equal(t, Duration(3*time.Second), followCopy.Interval)
		require.Equal(t, 7, followCopy.MaxBlocksPerTick)

		on2 := true
		probe := &IntegrityStateProbeConfig{Enabled: &on2, Interval: Duration(5 * time.Second)}
		probeCopy := probe.Copy()
		*probe.Enabled = false

		require.True(t, probeCopy.IsEnabled())
		require.Equal(t, Duration(5*time.Second), probeCopy.Interval)
	})
}

func TestIntegrityConfigCopy_NilReceiversReturnNil(t *testing.T) {
	var cfg *IntegrityConfig
	require.Nil(t, cfg.Copy())

	var settings *IntegritySettings
	require.Nil(t, settings.Copy())

	var check *IntegrityCheckConfig
	require.Nil(t, check.Copy())

	var budget *IntegrityBudgetConfig
	require.Nil(t, budget.Copy())

	var invalid *IntegrityInvalidBehaviorConfig
	require.Nil(t, invalid.Copy())
}
