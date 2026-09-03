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
	parse := func(t *testing.T, text string) (AvailbilityConfidence, error) {
		t.Helper()
		var cfg struct {
			Confidence AvailbilityConfidence `yaml:"confidence"`
		}
		err := yaml.Unmarshal([]byte("confidence: \""+text+"\""), &cfg)
		return cfg.Confidence, err
	}

	// Every value the printer can name, the parser accepts. The loop is
	// driven off the values themselves on purpose: a hand-written parse
	// table is exactly how the parser and the printer drifted apart, and
	// `stateProven` marshalled out and failed to come back.
	t.Run("EveryValueRoundTrips", func(t *testing.T) {
		for _, want := range availbilityConfidences {
			name := want.String()
			t.Run(name, func(t *testing.T) {
				printed, err := want.MarshalYAML()
				require.NoError(t, err)
				require.Equal(t, name, printed)

				got, err := parse(t, name)
				require.NoError(t, err, "the parser must accept what the printer emits")
				require.Equal(t, want, got)
			})
		}
	})

	t.Run("SpellingIsCaseInsensitive", func(t *testing.T) {
		for text, want := range map[string]AvailbilityConfidence{
			"blockhead":      AvailbilityConfidenceBlockHead,
			"BLOCKHEAD":      AvailbilityConfidenceBlockHead,
			"finalizedblock": AvailbilityConfidenceFinalized,
		} {
			got, err := parse(t, text)
			require.NoError(t, err, text)
			require.Equal(t, want, got, text)
		}
	})

	// The numeric form has always been accepted, and a dropped quote in
	// YAML reads as one.
	t.Run("TheNumberIsAcceptedToo", func(t *testing.T) {
		for text, want := range map[string]AvailbilityConfidence{
			"1": AvailbilityConfidenceBlockHead,
			"2": AvailbilityConfidenceFinalized,
		} {
			got, err := parse(t, text)
			require.NoError(t, err, text)
			require.Equal(t, want, got, text)
		}
	})

	t.Run("AnUnknownSpellingIsRejectedAndTheSetIsNamed", func(t *testing.T) {
		_, err := parse(t, "latest")
		require.Error(t, err)
		require.Contains(t, err.Error(), `invalid availability confidence "latest"`)
		for _, known := range availbilityConfidences {
			require.Contains(t, err.Error(), known.String(),
				"the error must name what the operator may write instead")
		}
	})

	t.Run("ANonStringNodeIsRejected", func(t *testing.T) {
		var cfg struct {
			Confidence AvailbilityConfidence `yaml:"confidence"`
		}
		require.Error(t, yaml.Unmarshal([]byte("confidence:\n  a: b\n"), &cfg))
	})

	// A value added to the const block but left out of the list would print
	// a name the parser rejects — the drift this whole test exists to stop.
	// The values run 1..N, so the next integer must have no name.
	t.Run("TheListCoversEveryNamedValue", func(t *testing.T) {
		next := AvailbilityConfidence(len(availbilityConfidences) + 1)
		require.Contains(t, next.String(), "unknown(",
			"a named value is missing from availbilityConfidences")
	})
}

func TestEvmSyncingStateString(t *testing.T) {
	require.Equal(t, "syncing", EvmSyncingStateSyncing.String())
	require.Equal(t, "not_syncing", EvmSyncingStateNotSyncing.String())
	require.Equal(t, "unknown(0)", EvmSyncingStateUnknown.String())
	require.Equal(t, "unknown(7)", EvmSyncingState(7).String())
}
