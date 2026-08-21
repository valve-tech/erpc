package common

import (
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/stretchr/testify/require"
)

// These two enums reach operators directly: their String() lands in metric
// labels and log lines, and AvailbilityConfidence also parses out of config
// YAML. A renamed label silently splits a metric series; a parser that accepts
// the wrong spelling silently changes routing.

func TestAvailbilityConfidenceString(t *testing.T) {
	require.Equal(t, "blockHead", AvailbilityConfidenceBlockHead.String())
	require.Equal(t, "finalizedBlock", AvailbilityConfidenceFinalized.String())
	require.Equal(t, "stateProven", AvailbilityConfidenceStateProven.String())
	require.Equal(t, "unknown(0)", AvailbilityConfidence(0).String(),
		"an unset confidence must name itself rather than borrow a real label")
	require.Equal(t, "unknown(9)", AvailbilityConfidence(9).String())
}

func TestAvailbilityConfidenceMarshal(t *testing.T) {
	y, err := AvailbilityConfidenceFinalized.MarshalYAML()
	require.NoError(t, err)
	require.Equal(t, "finalizedBlock", y)

	j, err := AvailbilityConfidenceBlockHead.MarshalJSON()
	require.NoError(t, err)
	require.JSONEq(t, `"blockHead"`, string(j))
}

func TestAvailbilityConfidenceUnmarshalYAML(t *testing.T) {
	accepted := map[string]AvailbilityConfidence{
		"blockHead":      AvailbilityConfidenceBlockHead,
		"blockhead":      AvailbilityConfidenceBlockHead,
		"BLOCKHEAD":      AvailbilityConfidenceBlockHead,
		"1":              AvailbilityConfidenceBlockHead,
		"finalizedBlock": AvailbilityConfidenceFinalized,
		"finalizedblock": AvailbilityConfidenceFinalized,
		"2":              AvailbilityConfidenceFinalized,
	}
	for text, want := range accepted {
		t.Run("accepts "+text, func(t *testing.T) {
			var cfg struct {
				Confidence AvailbilityConfidence `yaml:"confidence"`
			}
			require.NoError(t, yaml.Unmarshal([]byte("confidence: \""+text+"\""), &cfg))
			require.Equal(t, want, cfg.Confidence)
		})
	}

	t.Run("rejects an unknown spelling by name", func(t *testing.T) {
		var cfg struct {
			Confidence AvailbilityConfidence `yaml:"confidence"`
		}
		err := yaml.Unmarshal([]byte(`confidence: "latest"`), &cfg)
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid availability confidence: latest")
	})

	t.Run("rejects a non-string node", func(t *testing.T) {
		var cfg struct {
			Confidence AvailbilityConfidence `yaml:"confidence"`
		}
		require.Error(t, yaml.Unmarshal([]byte("confidence:\n  a: b\n"), &cfg))
	})

	t.Run("does not round-trip stateProven", func(t *testing.T) {
		// Recorded as bug 117: String/MarshalYAML emit "stateProven" but the
		// parser accepts only blockHead and finalizedBlock, so an operator
		// cannot feed a dumped config back in.
		var cfg struct {
			Confidence AvailbilityConfidence `yaml:"confidence"`
		}
		err := yaml.Unmarshal([]byte(`confidence: "stateProven"`), &cfg)
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid availability confidence: stateProven")
	})
}

func TestEvmSyncingStateString(t *testing.T) {
	require.Equal(t, "syncing", EvmSyncingStateSyncing.String())
	require.Equal(t, "not_syncing", EvmSyncingStateNotSyncing.String())
	require.Equal(t, "unknown(0)", EvmSyncingStateUnknown.String())
	require.Equal(t, "unknown(7)", EvmSyncingState(7).String())
}

// IsEvmStateQueryMethod names the methods the state-proven boundary applies to.
// It takes a LOWERCASED method name; feeding it the wire spelling must not
// silently drop a method out of the gate.
func TestIsEvmStateQueryMethod(t *testing.T) {
	stateReading := []string{
		"eth_call", "eth_getbalance", "eth_getcode", "eth_getstorageat",
		"eth_gettransactioncount", "eth_estimategas", "eth_getproof",
		"eth_simulatev1", "debug_tracecall",
	}
	for _, m := range stateReading {
		require.True(t, IsEvmStateQueryMethod(m), "%s reads the state trie", m)
	}

	chainData := []string{
		"eth_getblockbynumber", "eth_getlogs", "eth_gettransactionreceipt",
		"eth_blocknumber", "eth_chainid", "trace_call", "eth_createaccesslist",
	}
	for _, m := range chainData {
		require.False(t, IsEvmStateQueryMethod(m), "%s does not read the state trie", m)
	}

	require.False(t, IsEvmStateQueryMethod("eth_getBalance"),
		"the caller must lowercase the method before asking")
	require.False(t, IsEvmStateQueryMethod(""))
}
