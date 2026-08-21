package common

import (
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// These enums decide caching behaviour. An unrecognised value that parsed to
// the zero value would silently change what eRPC caches and for how long, so
// every one of them must reject what it does not know.

func TestDataFinalityState_StringIsStableAndNamesTheInvalidValue(t *testing.T) {
	require.Equal(t, "finalized", DataFinalityStateFinalized.String())
	require.Equal(t, "unfinalized", DataFinalityStateUnfinalized.String())
	require.Equal(t, "realtime", DataFinalityStateRealtime.String())
	require.Equal(t, "unknown", DataFinalityStateUnknown.String())

	// An out-of-range value must print the number, so a corrupted enum shows up
	// in a log instead of masquerading as "finalized".
	require.Equal(t, "invalid(9)", DataFinalityState(9).String())
	require.Equal(t, "invalid(-1)", DataFinalityStateAll.String())
}

func TestDataFinalityState_MarshalsAsAName(t *testing.T) {
	y, err := DataFinalityStateRealtime.MarshalYAML()
	require.NoError(t, err)
	require.Equal(t, "realtime", y)

	j, err := DataFinalityStateRealtime.MarshalJSON()
	require.NoError(t, err)
	require.JSONEq(t, `"realtime"`, string(j))
}

func TestDataFinalityState_UnmarshalYAMLAcceptsNamesAndOrdinals(t *testing.T) {
	cases := map[string]DataFinalityState{
		"finalized":   DataFinalityStateFinalized,
		"FINALIZED":   DataFinalityStateFinalized,
		"\"0\"":       DataFinalityStateFinalized,
		"unfinalized": DataFinalityStateUnfinalized,
		"\"1\"":       DataFinalityStateUnfinalized,
		"realtime":    DataFinalityStateRealtime,
		"\"2\"":       DataFinalityStateRealtime,
		"unknown":     DataFinalityStateUnknown,
		"\"3\"":       DataFinalityStateUnknown,
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			var wrapper struct {
				Finality DataFinalityState `yaml:"finality"`
			}
			require.NoError(t, yaml.Unmarshal([]byte("finality: "+in), &wrapper))
			require.Equal(t, want, wrapper.Finality)
		})
	}

	// A typo must fail the config load. Falling back to the zero value would
	// cache unfinalized data as if it were final.
	var wrapper struct {
		Finality DataFinalityState `yaml:"finality"`
	}
	err := yaml.Unmarshal([]byte("finality: finalised"), &wrapper)
	require.ErrorContains(t, err, "invalid data finality state: finalised")
}

func TestCacheEmptyBehavior_NamesAndParsing(t *testing.T) {
	require.Equal(t, "ignore", CacheEmptyBehaviorIgnore.String())
	require.Equal(t, "allow", CacheEmptyBehaviorAllow.String())
	require.Equal(t, "only", CacheEmptyBehaviorOnly.String())
	require.Equal(t, "invalid(7)", CacheEmptyBehavior(7).String())

	y, err := CacheEmptyBehaviorOnly.MarshalYAML()
	require.NoError(t, err)
	require.Equal(t, "only", y)

	cases := map[string]CacheEmptyBehavior{
		"ignore": CacheEmptyBehaviorIgnore,
		"\"0\"":  CacheEmptyBehaviorIgnore,
		"ALLOW":  CacheEmptyBehaviorAllow,
		"\"1\"":  CacheEmptyBehaviorAllow,
		"only":   CacheEmptyBehaviorOnly,
		"\"2\"":  CacheEmptyBehaviorOnly,
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			var wrapper struct {
				Empty CacheEmptyBehavior `yaml:"empty"`
			}
			require.NoError(t, yaml.Unmarshal([]byte("empty: "+in), &wrapper))
			require.Equal(t, want, wrapper.Empty)
		})
	}

	var wrapper struct {
		Empty CacheEmptyBehavior `yaml:"empty"`
	}
	err = yaml.Unmarshal([]byte("empty: sometimes"), &wrapper)
	require.ErrorContains(t, err, "invalid cache empty behavior: sometimes")
}

// appliesTo decides whether a policy is read from, written to, or both. An
// empty value means "both", so it must round-trip as "both" rather than as an
// empty string a downstream comparison would never match.
func TestCachePolicyAppliesTo_EmptyMeansBoth(t *testing.T) {
	var unset CachePolicyAppliesTo
	require.Equal(t, "both", unset.String())

	y, err := unset.MarshalYAML()
	require.NoError(t, err)
	require.Equal(t, "both", y)

	j, err := unset.MarshalJSON()
	require.NoError(t, err)
	require.JSONEq(t, `"both"`, string(j))

	require.Equal(t, "get", CachePolicyAppliesToGet.String())
}

func TestCachePolicyAppliesTo_UnmarshalsFromYAMLAndJSON(t *testing.T) {
	cases := map[string]CachePolicyAppliesTo{
		`""`:    CachePolicyAppliesToBoth,
		"both":  CachePolicyAppliesToBoth,
		"BOTH":  CachePolicyAppliesToBoth,
		"get":   CachePolicyAppliesToGet,
		"  set": CachePolicyAppliesToSet,
	}

	for in, want := range cases {
		t.Run("yaml "+in, func(t *testing.T) {
			var wrapper struct {
				AppliesTo CachePolicyAppliesTo `yaml:"appliesTo"`
			}
			require.NoError(t, yaml.Unmarshal([]byte("appliesTo: "+in), &wrapper))
			require.Equal(t, want, wrapper.AppliesTo)
		})
	}

	for in, want := range map[string]CachePolicyAppliesTo{
		`""`:      CachePolicyAppliesToBoth,
		`"both"`:  CachePolicyAppliesToBoth,
		`"GET"`:   CachePolicyAppliesToGet,
		`" set "`: CachePolicyAppliesToSet,
	} {
		t.Run("json "+in, func(t *testing.T) {
			var got CachePolicyAppliesTo
			require.NoError(t, got.UnmarshalJSON([]byte(in)))
			require.Equal(t, want, got)
		})
	}

	t.Run("unknown value is refused on both paths", func(t *testing.T) {
		var wrapper struct {
			AppliesTo CachePolicyAppliesTo `yaml:"appliesTo"`
		}
		require.ErrorContains(t,
			yaml.Unmarshal([]byte("appliesTo: read"), &wrapper),
			"invalid cache policy appliesTo: read")

		var got CachePolicyAppliesTo
		require.ErrorContains(t, got.UnmarshalJSON([]byte(`"read"`)), "invalid cache policy appliesTo: read")
	})
}
