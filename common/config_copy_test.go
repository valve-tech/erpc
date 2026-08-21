package common

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The Copy() family on UpstreamConfig exists for one reason: upstream/registry.go
// copies an upstream config before it bootstraps the upstream, so that vendor
// detection and per-attempt mutation cannot reach the config the operator loaded
// or the sibling copies made from it. A field that Copy() leaves aliased breaks
// that guarantee silently.
//
// These tests walk the whole config tree by reflection instead of listing
// fields, so a new field added to any of these structs is checked the day it
// lands rather than the day someone remembers to extend a fixture.

func TestUpstreamConfigCopy_ReproducesEveryValue(t *testing.T) {
	seed := 0
	original := &UpstreamConfig{}
	fillForCopy(t, reflect.ValueOf(original).Elem(), &seed, 0)

	copied := original.Copy()

	require.NotSame(t, original, copied, "Copy must return a new struct")
	require.Equal(t, original, copied, "the copy must carry every value of the original")
}

// TestUpstreamConfigCopy_SharesNoMemoryWithTheOriginal admits no exceptions.
// It used to carry an allowlist of seven known-aliased paths (bug 115); those
// fields are deep-copied now, so any path this walk reports is a real gap —
// including one in a field added tomorrow.
func TestUpstreamConfigCopy_SharesNoMemoryWithTheOriginal(t *testing.T) {
	seed := 0
	original := &UpstreamConfig{}
	fillForCopy(t, reflect.ValueOf(original).Elem(), &seed, 0)

	copied := original.Copy()

	shared := findSharedRefs(reflect.ValueOf(original).Elem(), reflect.ValueOf(copied).Elem(), "UpstreamConfig", nil)
	require.Empty(t, shared,
		"UpstreamConfig.Copy left these fields aliased with the original:\n  %s",
		strings.Join(shared, "\n  "))
}

func TestUpstreamConfigCopy_NilReceiverReturnsNil(t *testing.T) {
	var nilCfg *UpstreamConfig
	require.Nil(t, nilCfg.Copy())

	var nilEvm *EvmUpstreamConfig
	require.Nil(t, nilEvm.Copy())

	var nilFailsafe *FailsafeConfig
	require.Nil(t, nilFailsafe.Copy())

	var nilConsensus *ConsensusPolicyConfig
	require.Nil(t, nilConsensus.Copy())

	var nilJsonRpc *JsonRpcUpstreamConfig
	require.Nil(t, nilJsonRpc.Copy())
}

// TestJsonRpcUpstreamConfigCopy_HeadersAreIndependent pins the field that
// carries credentials. A vendor that writes an Authorization header onto one
// copy must not write it onto every sibling copy and the operator's own config.
func TestJsonRpcUpstreamConfigCopy_HeadersAreIndependent(t *testing.T) {
	original := &JsonRpcUpstreamConfig{
		Headers: map[string]string{"X-Api-Key": "original"},
	}

	copied := original.Copy()
	copied.Headers["Authorization"] = "Bearer added-by-vendor"
	copied.Headers["X-Api-Key"] = "overwritten"

	require.Equal(t, map[string]string{"X-Api-Key": "original"}, original.Headers,
		"writing a header on the copy must not reach the original config")
	require.Equal(t, "Bearer added-by-vendor", copied.Headers["Authorization"])
}

// TestUpstreamConfigCopy_FormerlyAliasedFieldsAreIndependent says in plain
// writes what the reflection walk says by inspection. Each of these seven
// fields was aliased between an upstream config and its copy (bug 115), so a
// mutation through the copy is the direct proof that the alias is gone.
//
// upstream/registry.go copies a config exactly so that concurrent bootstrap
// attempts cannot race. A shared map turns that race into a Go fatal
// "concurrent map writes", which no recover catches.
//
// One subtest per field, so a field that becomes aliased again names itself
// instead of hiding behind whichever assertion happens to run first.
func TestUpstreamConfigCopy_FormerlyAliasedFieldsAreIndependent(t *testing.T) {
	rate := 0.25
	original := &UpstreamConfig{
		Tags:        []string{"tier:main"},
		CreditUnits: map[string]int64{"eth_call": 10},
		Svm:         &SvmUpstreamConfig{Chain: "solana", Cluster: "mainnet-beta"},
		Shadow: &ShadowUpstreamConfig{
			Enabled:      true,
			SampleRate:   &rate,
			IgnoreFields: map[string][]string{"eth_getLogs": {"blockHash"}},
		},
		Routing: &UpstreamRoutingConfig{
			Probe: ProbeModeOn,
			ScoreMultipliers: []*ScoreMultiplierConfig{
				{Network: "evm:1", Finality: []DataFinalityState{DataFinalityStateFinalized}},
			},
		},
		Failsafe: []*FailsafeConfig{{
			Retry: &RetryPolicyConfig{
				EmptyResultAccept: []string{"eth_getLogs"},
				EmptyResultIgnore: []string{"eth_getBalance"},
			},
		}},
	}

	copied := original.Copy()

	t.Run("Tags", func(t *testing.T) {
		// Write into the existing element first: `append` on a full slice
		// reallocates and would hide the sharing.
		copied.Tags[0] = "tier:mutated"
		copied.Tags = append(copied.Tags, "tier:fallback")
		require.Equal(t, []string{"tier:main"}, original.Tags)
	})

	t.Run("CreditUnits", func(t *testing.T) {
		copied.CreditUnits["eth_call"] = 999
		copied.CreditUnits["eth_getLogs"] = 5
		require.Equal(t, map[string]int64{"eth_call": 10}, original.CreditUnits)
	})

	t.Run("Svm", func(t *testing.T) {
		copied.Svm.Cluster = "devnet"
		require.Equal(t, "mainnet-beta", original.Svm.Cluster)
	})

	t.Run("Shadow", func(t *testing.T) {
		copied.Shadow.Enabled = false
		*copied.Shadow.SampleRate = 0.9
		copied.Shadow.IgnoreFields["eth_getLogs"][0] = "mutated"
		copied.Shadow.IgnoreFields["eth_call"] = []string{"added"}
		require.True(t, original.Shadow.Enabled)
		require.Equal(t, 0.25, *original.Shadow.SampleRate)
		require.Equal(t, []string{"blockHash"}, original.Shadow.IgnoreFields["eth_getLogs"])
		require.NotContains(t, original.Shadow.IgnoreFields, "eth_call")
	})

	t.Run("Routing", func(t *testing.T) {
		copied.Routing.Probe = ProbeModeOff
		copied.Routing.ScoreMultipliers[0].Network = "evm:137"
		copied.Routing.ScoreMultipliers[0].Finality[0] = DataFinalityStateUnfinalized
		require.Equal(t, ProbeModeOn, original.Routing.Probe)
		require.Equal(t, "evm:1", original.Routing.ScoreMultipliers[0].Network)
		require.Equal(t, DataFinalityStateFinalized, original.Routing.ScoreMultipliers[0].Finality[0])
	})

	t.Run("Retry.EmptyResultAccept", func(t *testing.T) {
		copied.Failsafe[0].Retry.EmptyResultAccept[0] = "mutated"
		require.Equal(t, []string{"eth_getLogs"}, original.Failsafe[0].Retry.EmptyResultAccept)
	})

	t.Run("Retry.EmptyResultIgnore", func(t *testing.T) {
		copied.Failsafe[0].Retry.EmptyResultIgnore[0] = "mutated"
		require.Equal(t, []string{"eth_getBalance"}, original.Failsafe[0].Retry.EmptyResultIgnore)
	})
}

// TestConsensusPolicyConfigCopy_NestedMapsAreIndependent pins the two
// map[string][]string fields, where a shallow copy would leave the inner
// slices shared even if the outer map were cloned.
func TestConsensusPolicyConfigCopy_NestedMapsAreIndependent(t *testing.T) {
	original := &ConsensusPolicyConfig{
		IgnoreFields:          map[string][]string{"eth_getLogs": {"blockHash"}},
		PreferHighestValueFor: map[string][]string{"eth_getBalance": {"result"}},
		RequiredParticipants:  []*ConsensusRequiredParticipant{{Tag: "tier:main", MinParticipants: 2}},
	}

	copied := original.Copy()
	copied.IgnoreFields["eth_getLogs"][0] = "mutated"
	copied.IgnoreFields["eth_call"] = []string{"added"}
	copied.PreferHighestValueFor["eth_getBalance"][0] = "mutated"
	copied.RequiredParticipants[0].MinParticipants = 99

	require.Equal(t, []string{"blockHash"}, original.IgnoreFields["eth_getLogs"])
	require.NotContains(t, original.IgnoreFields, "eth_call")
	require.Equal(t, []string{"result"}, original.PreferHighestValueFor["eth_getBalance"])
	require.Equal(t, 2, original.RequiredParticipants[0].MinParticipants)
}

// TestConsensusPolicyConfigCopy_SkipsNilRequiredParticipant proves the nil
// guard inside the RequiredParticipants loop: a nil entry must survive as a
// nil entry rather than panic or shift the other entries.
func TestConsensusPolicyConfigCopy_SkipsNilRequiredParticipant(t *testing.T) {
	original := &ConsensusPolicyConfig{
		RequiredParticipants: []*ConsensusRequiredParticipant{
			nil,
			{Tag: "tier:main", MinParticipants: 3},
		},
	}

	copied := original.Copy()

	require.Len(t, copied.RequiredParticipants, 2)
	require.Nil(t, copied.RequiredParticipants[0])
	require.Equal(t, "tier:main", copied.RequiredParticipants[1].Tag)
	require.NotSame(t, original.RequiredParticipants[1], copied.RequiredParticipants[1])
}

// fillForCopy gives every field of v a distinctive non-zero value. Pointers get
// allocated, slices get two elements and maps get two entries, so that a
// shallow copy of any reference field is detectable.
func fillForCopy(t *testing.T, v reflect.Value, seed *int, depth int) {
	t.Helper()
	if depth > 12 || !v.CanSet() {
		return
	}
	*seed++
	n := *seed

	switch v.Kind() {
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(n))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(n))
	case reflect.Float32, reflect.Float64:
		// Keep it inside (0,1] so fields validated as a quantile stay sane.
		v.SetFloat(1.0 / float64(n+1))
	case reflect.String:
		v.SetString("v" + itoa(n))
	case reflect.Ptr:
		v.Set(reflect.New(v.Type().Elem()))
		fillForCopy(t, v.Elem(), seed, depth+1)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 2, 2)
		for i := 0; i < 2; i++ {
			fillForCopy(t, s.Index(i), seed, depth+1)
		}
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		for i := 0; i < 2; i++ {
			k := reflect.New(v.Type().Key()).Elem()
			fillForCopy(t, k, seed, depth+1)
			val := reflect.New(v.Type().Elem()).Elem()
			fillForCopy(t, val, seed, depth+1)
			m.SetMapIndex(k, val)
		}
		v.Set(m)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).PkgPath != "" {
				continue // unexported
			}
			fillForCopy(t, v.Field(i), seed, depth+1)
		}
	default:
		// Interfaces, channels and funcs have no meaningful filler; the config
		// tree does not use them, and leaving them nil keeps the walk total.
	}
}

// findSharedRefs walks orig and cp in parallel and returns the dotted path of
// every slice backing array, map, or pointer-to-container the two still share.
//
// A shared pointer to a SCALAR (*bool, *int64) is not reported. Sharing hurts
// when a writer can change the shared thing in place — append to a slice, set a
// map key, assign a struct field — and every writer of a *bool in this
// codebase replaces the pointer rather than writing through it, so a shared
// *bool cannot carry a mutation from one config to another.
func findSharedRefs(orig, cp reflect.Value, path string, out []string) []string {
	if orig.Kind() != cp.Kind() {
		return out
	}
	switch orig.Kind() {
	case reflect.Ptr:
		if orig.IsNil() || cp.IsNil() {
			return out
		}
		if orig.Pointer() == cp.Pointer() {
			if hasMutableInterior(orig.Type().Elem()) {
				return append(out, path)
			}
			return out
		}
		return findSharedRefs(orig.Elem(), cp.Elem(), path, out)
	case reflect.Slice:
		if orig.IsNil() || cp.IsNil() || orig.Len() == 0 {
			return out
		}
		if orig.Pointer() == cp.Pointer() {
			return append(out, path)
		}
		for i := 0; i < orig.Len() && i < cp.Len(); i++ {
			out = findSharedRefs(orig.Index(i), cp.Index(i), path+"["+itoa(i)+"]", out)
		}
		return out
	case reflect.Map:
		if orig.IsNil() || cp.IsNil() || orig.Len() == 0 {
			return out
		}
		if orig.Pointer() == cp.Pointer() {
			return append(out, path)
		}
		for _, k := range orig.MapKeys() {
			cv := cp.MapIndex(k)
			if !cv.IsValid() {
				continue
			}
			out = findSharedRefs(orig.MapIndex(k), cv, path+"["+k.String()+"]", out)
		}
		return out
	case reflect.Struct:
		for i := 0; i < orig.NumField(); i++ {
			f := orig.Type().Field(i)
			if f.PkgPath != "" {
				continue
			}
			out = findSharedRefs(orig.Field(i), cp.Field(i), path+"."+f.Name, out)
		}
		return out
	default:
		return out
	}
}

// hasMutableInterior reports whether a writer can change t in place, which is
// what makes sharing it dangerous.
func hasMutableInterior(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Struct, reflect.Slice, reflect.Map, reflect.Array:
		return true
	default:
		return false
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
