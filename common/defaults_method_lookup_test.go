package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Method resolution decides whether a request is cached, gated by block
// availability, and tag-translated. A client may send any casing it likes, so a
// miss here is not a cosmetic miss: the request silently skips the whole
// method-config path.

// ---------------------------------------------------------------------------
// lowerCacheMethodIndex
// ---------------------------------------------------------------------------

// An empty or nil table must produce no index at all. FindMethodConfig then
// falls back to a scan, which is the behaviour a programmatically built config
// depends on.
func TestLowerCacheMethodIndex_EmptyTableProducesNoIndex(t *testing.T) {
	assert.Nil(t, lowerCacheMethodIndex(nil))
	assert.Nil(t, lowerCacheMethodIndex(map[string]*CacheMethodConfig{}))
}

// A nil value is not a definition. Indexing it would hand a nil config to a
// caller that has already checked for one.
func TestLowerCacheMethodIndex_SkipsNilDefinitions(t *testing.T) {
	real := &CacheMethodConfig{Finalized: true}

	idx := lowerCacheMethodIndex(map[string]*CacheMethodConfig{
		"eth_getBalance": nil,
		"eth_chainId":    real,
	})

	require.Len(t, idx, 1, "only the real definition may be indexed")
	assert.Same(t, real, idx["eth_chainid"])
	_, present := idx["eth_getbalance"]
	assert.False(t, present, "a nil definition must not appear in the index")
}

// Two keys that differ only in case collapse to one entry, and the SMALLEST key
// wins. The rule has to be deterministic, because map iteration order is not:
// without it the same config would resolve differently between restarts.
func TestLowerCacheMethodIndex_DuplicateCasingsCollapseToTheSmallestKey(t *testing.T) {
	upper := &CacheMethodConfig{Finalized: true}
	lower := &CacheMethodConfig{Realtime: true}

	// Run it repeatedly: one pass could pick the winner by luck of iteration
	// order, a hundred passes could not.
	for i := 0; i < 100; i++ {
		idx := lowerCacheMethodIndex(map[string]*CacheMethodConfig{
			"ETH_call": upper, // "E" < "e", so this key wins
			"eth_call": lower,
		})

		require.Len(t, idx, 1)
		assert.Same(t, upper, idx["eth_call"], "the smallest key must win every time")
	}
}

// ---------------------------------------------------------------------------
// DefaultWithBlockMethodConfig
// ---------------------------------------------------------------------------

// The with-block table drives block-availability enforcement. A method in it
// must resolve on its canonical name and on any other casing a client sends.
func TestDefaultWithBlockMethodConfig(t *testing.T) {
	// Pick a name from the table itself rather than hard-coding one, so the
	// test keeps working when the table changes.
	var canonical string
	for name, cfg := range DefaultWithBlockCacheMethods {
		if cfg != nil {
			canonical = name
			break
		}
	}
	require.NotEmpty(t, canonical, "the with-block table must not be empty")

	t.Run("the canonical name resolves", func(t *testing.T) {
		assert.Same(t, DefaultWithBlockCacheMethods[canonical], DefaultWithBlockMethodConfig(canonical))
	})

	t.Run("an upper-case client spelling resolves to the same config", func(t *testing.T) {
		shouted := ""
		for _, r := range canonical {
			if r >= 'a' && r <= 'z' {
				r -= 32
			}
			shouted += string(r)
		}
		assert.Same(t, DefaultWithBlockCacheMethods[canonical], DefaultWithBlockMethodConfig(shouted),
			"casing must not decide whether block availability is enforced")
	})

	t.Run("a method outside the table resolves to nothing", func(t *testing.T) {
		assert.Nil(t, DefaultWithBlockMethodConfig("eth_notAMethodAnyoneImplements"))
	})
}

// ---------------------------------------------------------------------------
// MethodsConfig.FindMethodConfig
// ---------------------------------------------------------------------------

// With no definitions there is nothing to resolve, and the nil receiver is the
// common case: most networks never write a methods block.
func TestMethodsConfig_FindMethodConfig_NoDefinitionsResolvesToNothing(t *testing.T) {
	var nilCfg *MethodsConfig
	assert.Nil(t, nilCfg.FindMethodConfig("eth_call"))

	assert.Nil(t, (&MethodsConfig{}).FindMethodConfig("eth_call"))
	assert.Nil(t, (&MethodsConfig{Definitions: map[string]*CacheMethodConfig{}}).FindMethodConfig("eth_call"))
}

// A config built in code has no lowercase index, so resolution must fall back
// to a scan. Anything else would make method config depend on whether
// SetDefaults happened to run.
func TestMethodsConfig_FindMethodConfig_FallsBackToAScanWithoutAnIndex(t *testing.T) {
	want := &CacheMethodConfig{Finalized: true}
	m := &MethodsConfig{Definitions: map[string]*CacheMethodConfig{"eth_getLogs": want}}

	require.Nil(t, m.lowerIndex, "this fixture deliberately never ran SetDefaults")

	assert.Same(t, want, m.FindMethodConfig("eth_getLogs"), "the exact name resolves")
	assert.Same(t, want, m.FindMethodConfig("ETH_GETLOGS"), "the scan must be case-insensitive too")
	assert.Nil(t, m.FindMethodConfig("eth_call"))
}

// After SetDefaults the index exists and must give the same answers as the
// scan, for the canonical name and for any other casing.
func TestMethodsConfig_FindMethodConfig_UsesTheIndexAfterSetDefaults(t *testing.T) {
	cfg := mustSetDefaultsFromYAML(t, `
projects:
  - id: main
    upstreams:
      - id: u1
        endpoint: https://rpc.example.com
        evm:
          chainId: 1
    networks:
      - architecture: evm
        evm:
          chainId: 1
        methods:
          preserveDefaultMethods: true
          definitions:
            eth_getLogs:
              finalized: true
`, &DefaultOptions{})

	network := onlyProject(t, cfg).Networks[0]
	require.NotNil(t, network.Methods)
	require.NotNil(t, network.Methods.lowerIndex, "SetDefaults must build the lowercase index")

	byName := network.Methods.FindMethodConfig("eth_getLogs")
	require.NotNil(t, byName, "the operator's own definition must resolve")

	assert.Same(t, byName, network.Methods.FindMethodConfig("ETH_GETLOGS"),
		"a shouting client must get the same method config")
	assert.Nil(t, network.Methods.FindMethodConfig("eth_noSuchMethod"))
}
