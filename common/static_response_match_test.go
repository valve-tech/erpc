package common

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A static response replaces a real upstream call. Matching too loosely serves
// a canned answer to a request that should have gone to a node; matching too
// tightly silently disables the entry an operator configured. Both failures are
// invisible in the logs, so the matcher is pinned here.

func TestFindStaticResponseMatch_MethodMustMatchExactly(t *testing.T) {
	entry := &StaticResponseConfig{
		Method:   "eth_chainId",
		Response: &StaticResponseBodyConfig{Result: "0x1"},
	}
	entries := []*StaticResponseConfig{nil, entry}

	require.Same(t, entry, FindStaticResponseMatch(entries, "eth_chainId", nil))

	// Case and prefix must not match: eRPC would answer a different method.
	require.Nil(t, FindStaticResponseMatch(entries, "eth_chainID", nil))
	require.Nil(t, FindStaticResponseMatch(entries, "eth_chain", nil))
	require.Nil(t, FindStaticResponseMatch(entries, "", nil))
	require.Nil(t, FindStaticResponseMatch(nil, "eth_chainId", nil))
}

// The first matching entry wins. An operator overriding a broad entry puts the
// narrow one first, so order has to be honoured.
func TestFindStaticResponseMatch_FirstMatchWins(t *testing.T) {
	first := &StaticResponseConfig{
		Method:   "eth_getBlockByNumber",
		Params:   []interface{}{"0x0", false},
		Response: &StaticResponseBodyConfig{Result: "genesis"},
	}
	second := &StaticResponseConfig{
		Method:   "eth_getBlockByNumber",
		Params:   []interface{}{"0x0", false},
		Response: &StaticResponseBodyConfig{Result: "shadowed"},
	}

	got := FindStaticResponseMatch([]*StaticResponseConfig{first, second}, "eth_getBlockByNumber",
		[]interface{}{"0x0", false})
	require.Same(t, first, got)
}

// Params arrive as JSON (numbers become float64) while the config arrives as
// YAML (numbers stay int). Comparing the concrete types would make every
// numeric entry dead config.
func TestFindStaticResponseMatch_NumbersCompareByValueNotType(t *testing.T) {
	entry := &StaticResponseConfig{
		Method:   "eth_getBlockByNumber",
		Params:   []interface{}{0, false}, // YAML shape
		Response: &StaticResponseBodyConfig{Result: "genesis"},
	}
	entries := []*StaticResponseConfig{entry}

	// JSON shape of the same request.
	require.Same(t, entry, FindStaticResponseMatch(entries, "eth_getBlockByNumber",
		[]interface{}{float64(0), false}))

	// Every numeric width must agree with the same value.
	for _, v := range []interface{}{
		int8(0), int16(0), int32(0), int64(0),
		uint(0), uint8(0), uint16(0), uint32(0), uint64(0), float32(0),
	} {
		require.Same(t, entry, FindStaticResponseMatch(entries, "eth_getBlockByNumber",
			[]interface{}{v, false}), "numeric type %T must compare by value", v)
	}

	// A different number must NOT match, or the entry would answer every block.
	require.Nil(t, FindStaticResponseMatch(entries, "eth_getBlockByNumber",
		[]interface{}{float64(1), false}))

	// A number must not match a string that looks like one.
	require.Nil(t, FindStaticResponseMatch(entries, "eth_getBlockByNumber",
		[]interface{}{"0", false}))
}

func TestFindStaticResponseMatch_ParamCountMustAgree(t *testing.T) {
	entry := &StaticResponseConfig{
		Method:   "eth_call",
		Params:   []interface{}{"a", "b"},
		Response: &StaticResponseBodyConfig{Result: "0x"},
	}
	entries := []*StaticResponseConfig{entry}

	require.Same(t, entry, FindStaticResponseMatch(entries, "eth_call", []interface{}{"a", "b"}))
	require.Nil(t, FindStaticResponseMatch(entries, "eth_call", []interface{}{"a"}))
	require.Nil(t, FindStaticResponseMatch(entries, "eth_call", []interface{}{"a", "b", "c"}))

	// An entry with no params only matches a request with no params: otherwise
	// it would shadow every call to that method.
	bare := &StaticResponseConfig{Method: "eth_call", Response: &StaticResponseBodyConfig{Result: "0x"}}
	require.Same(t, bare, FindStaticResponseMatch([]*StaticResponseConfig{bare}, "eth_call", nil))
	require.Same(t, bare, FindStaticResponseMatch([]*StaticResponseConfig{bare}, "eth_call", []interface{}{}))
	require.Nil(t, FindStaticResponseMatch([]*StaticResponseConfig{bare}, "eth_call", []interface{}{"a"}))
}

// Object params (the eth_call transaction object, an eth_getLogs filter) come
// out of YAML and JSON with different key order. Comparing by order would make
// them never match.
func TestFindStaticResponseMatch_ObjectsCompareByKey(t *testing.T) {
	entry := &StaticResponseConfig{
		Method: "eth_call",
		Params: []interface{}{
			map[string]interface{}{"to": "0xabc", "data": "0x01"},
			"latest",
		},
		Response: &StaticResponseBodyConfig{Result: "0x"},
	}
	entries := []*StaticResponseConfig{entry}

	require.Same(t, entry, FindStaticResponseMatch(entries, "eth_call", []interface{}{
		map[string]interface{}{"data": "0x01", "to": "0xabc"},
		"latest",
	}))

	// A missing key, an extra key, or a changed value must all miss.
	require.Nil(t, FindStaticResponseMatch(entries, "eth_call", []interface{}{
		map[string]interface{}{"to": "0xabc"}, "latest",
	}))
	require.Nil(t, FindStaticResponseMatch(entries, "eth_call", []interface{}{
		map[string]interface{}{"to": "0xabc", "data": "0x01", "gas": "0x1"}, "latest",
	}))
	require.Nil(t, FindStaticResponseMatch(entries, "eth_call", []interface{}{
		map[string]interface{}{"to": "0xdef", "data": "0x01"}, "latest",
	}))
	// A map must not match a non-map.
	require.Nil(t, FindStaticResponseMatch(entries, "eth_call", []interface{}{"0xabc", "latest"}))
}

func TestFindStaticResponseMatch_NestedSlicesCompareElementwise(t *testing.T) {
	entry := &StaticResponseConfig{
		Method: "eth_getLogs",
		Params: []interface{}{
			map[string]interface{}{
				"topics": []interface{}{"0xaa", []interface{}{"0xbb", "0xcc"}},
			},
		},
		Response: &StaticResponseBodyConfig{Result: []interface{}{}},
	}
	entries := []*StaticResponseConfig{entry}

	require.Same(t, entry, FindStaticResponseMatch(entries, "eth_getLogs", []interface{}{
		map[string]interface{}{
			"topics": []interface{}{"0xaa", []interface{}{"0xbb", "0xcc"}},
		},
	}))

	// Order inside a slice IS significant — topic position selects a different
	// log set on chain.
	require.Nil(t, FindStaticResponseMatch(entries, "eth_getLogs", []interface{}{
		map[string]interface{}{
			"topics": []interface{}{"0xaa", []interface{}{"0xcc", "0xbb"}},
		},
	}))
	require.Nil(t, FindStaticResponseMatch(entries, "eth_getLogs", []interface{}{
		map[string]interface{}{
			"topics": []interface{}{"0xaa", []interface{}{"0xbb"}},
		},
	}))
	// A slice must not match a non-slice.
	require.Nil(t, FindStaticResponseMatch(entries, "eth_getLogs", []interface{}{
		map[string]interface{}{"topics": "0xaa"},
	}))
}

// A null param is a real JSON-RPC value. It must equal only another null, or a
// request asking for nothing would match one asking for something.
func TestFindStaticResponseMatch_NullMatchesOnlyNull(t *testing.T) {
	entry := &StaticResponseConfig{
		Method:   "eth_call",
		Params:   []interface{}{nil},
		Response: &StaticResponseBodyConfig{Result: "0x"},
	}
	entries := []*StaticResponseConfig{entry}

	require.Same(t, entry, FindStaticResponseMatch(entries, "eth_call", []interface{}{nil}))
	require.Nil(t, FindStaticResponseMatch(entries, "eth_call", []interface{}{"latest"}))
	require.Nil(t, FindStaticResponseMatch(entries, "eth_call", []interface{}{0}))

	// And the reverse: a non-null entry must not match a null request param.
	other := &StaticResponseConfig{
		Method:   "eth_call",
		Params:   []interface{}{"latest"},
		Response: &StaticResponseBodyConfig{Result: "0x"},
	}
	require.Nil(t, FindStaticResponseMatch([]*StaticResponseConfig{other}, "eth_call", []interface{}{nil}))
}

func TestFindStaticResponseMatch_BooleansAndStringsAreTypeSafe(t *testing.T) {
	entry := &StaticResponseConfig{
		Method:   "eth_getBlockByNumber",
		Params:   []interface{}{"latest", true},
		Response: &StaticResponseBodyConfig{Result: "0x"},
	}
	entries := []*StaticResponseConfig{entry}

	require.Same(t, entry, FindStaticResponseMatch(entries, "eth_getBlockByNumber",
		[]interface{}{"latest", true}))

	// true must not match "true" or 1 — a client sending either means something
	// different to the node.
	require.Nil(t, FindStaticResponseMatch(entries, "eth_getBlockByNumber",
		[]interface{}{"latest", "true"}))
	require.Nil(t, FindStaticResponseMatch(entries, "eth_getBlockByNumber",
		[]interface{}{"latest", false}))
	require.Nil(t, FindStaticResponseMatch(entries, "eth_getBlockByNumber",
		[]interface{}{true, true}))
}
