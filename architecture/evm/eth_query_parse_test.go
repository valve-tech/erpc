package evm

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The query shim parses JSON a client wrote, so its unknown-input path is the
// path that matters. Every parser below has to decide the shapes it knows and
// refuse everything else out loud — a quantity that silently reads as 0, or a
// cursor hash that silently reads as empty, walks the caller past a block it
// never asked for.

func TestParseUint64Value_ReadsEveryQuantityShapeAJsonClientCanSend(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  interface{}
		want uint64
	}{
		{"Uint64", uint64(42), 42},
		{"Uint32", uint32(42), 42},
		{"Int", int(42), 42},
		{"Int64", int64(42), 42},
		// encoding/json hands every JSON number over as a float64.
		{"Float64", float64(42), 42},
		{"FloatWithFraction", float64(42.9), 42},
		{"HexStringLowerPrefix", "0x2a", 42},
		{"DecimalString", "42", 42},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseUint64Value(tc.raw)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestParseUint64Value_RefusesWhatItCannotRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		raw    interface{}
		reason string
	}{
		{"Nil", nil, "missing quantity"},
		{"NegativeInt", int(-1), "negative quantity"},
		{"NegativeInt64", int64(-1), "negative quantity"},
		{"NegativeFloat", float64(-1), "negative quantity"},
		{"EmptyString", "", "empty quantity"},
		{"GarbageString", "banana", "invalid syntax"},
		{"GarbageHexString", "0xzz", "expected integer"},
		{"Bool", true, "unsupported quantity type"},
		{"Object", map[string]interface{}{}, "unsupported quantity type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseUint64Value(tc.raw)
			require.Error(t, err, "an unreadable quantity must not pass as a number")
			assert.Equal(t, uint64(0), got)
			assert.Contains(t, err.Error(), tc.reason)
		})
	}
}

// parseUint64Value no longer tests for a "0X" prefix. It used to, and then
// handed the value to common.HexToUint64, which accepts only the lowercase
// "0x" — so the branch written for the uppercase form always ended in
// "invalid hex string", a message that hides which single character is wrong.
// Deleting the test lets the value fall through to the decimal parser and fail
// with a message that names the input. See entry 120 in
// valve/upstream-bug-log.md.
func TestParseUint64Value_ReportsAnUppercaseHexPrefixAgainstTheInput(t *testing.T) {
	got, err := parseUint64Value("0X2a")
	require.Error(t, err, "no converter here accepts an uppercase hex prefix")
	assert.Equal(t, uint64(0), got)
	assert.Contains(t, err.Error(), "0X2a",
		"the message must quote what the client sent, not name a hex format it never claimed")
	assert.NotContains(t, err.Error(), "invalid hex string",
		"a dead uppercase branch would still route this to the hex converter")
}

func TestNormalizeFieldSelection_CopiesAndFilters(t *testing.T) {
	t.Run("StringSliceIsCopiedNotAliased", func(t *testing.T) {
		src := []string{"number", "hash"}
		got := normalizeFieldSelection(src)
		require.Equal(t, src, got)
		got[0] = "mutated"
		assert.Equal(t, "number", src[0],
			"the caller's slice must survive whatever the shim does to the result")
	})

	t.Run("InterfaceSliceKeepsOnlyNonEmptyStrings", func(t *testing.T) {
		got := normalizeFieldSelection([]interface{}{"number", "", 7, nil, "hash"})
		assert.Equal(t, []string{"number", "hash"}, got,
			"a field list must drop the entries that are not usable field names")
	})

	for _, tc := range []struct {
		name string
		raw  interface{}
	}{
		{"Nil", nil},
		{"String", "number"},
		{"Bool", true},
	} {
		t.Run("Refuses"+tc.name, func(t *testing.T) {
			assert.Nil(t, normalizeFieldSelection(tc.raw),
				"a selection the shim cannot read must mean no selection, not an empty one")
		})
	}
}

func TestNormalizeFieldSelectionRaw_SeparatesAllFieldsFromNoFields(t *testing.T) {
	// `true` means every field; `false` means none. The two must not collapse,
	// because an empty list is what the shim sends when the caller opted out.
	assert.Equal(t, true, normalizeFieldSelectionRaw(true))
	assert.Equal(t, []string{}, normalizeFieldSelectionRaw(false))
	assert.Nil(t, normalizeFieldSelectionRaw(nil))
	assert.Equal(t, []string{"number"}, normalizeFieldSelectionRaw([]interface{}{"number", "", 3}))
	assert.Nil(t, normalizeFieldSelectionRaw(7), "an unknown shape selects nothing")
}

func TestParseQueryCursorBlock_ReadsTheWholeCursor(t *testing.T) {
	got, err := parseQueryCursorBlock(map[string]interface{}{
		"number":     "0x64",
		"hash":       "0xaabb",
		"parentHash": "0xccdd",
	})
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, uint64(100), got.Number)
	assert.Equal(t, []byte{0xaa, 0xbb}, got.Hash)
	assert.Equal(t, []byte{0xcc, 0xdd}, got.ParentHash)
}

func TestParseQueryCursorBlock_LeavesTheHashesOutWhenTheyAreAbsent(t *testing.T) {
	got, err := parseQueryCursorBlock(map[string]interface{}{"number": float64(7)})
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, uint64(7), got.Number)
	assert.Nil(t, got.Hash, "an absent hash must stay absent, not become an empty hash")
	assert.Nil(t, got.ParentHash)
}

func TestParseQueryCursorBlock_RefusesWhatItCannotRead(t *testing.T) {
	t.Run("NilMeansNoCursor", func(t *testing.T) {
		got, err := parseQueryCursorBlock(nil)
		require.NoError(t, err)
		assert.Nil(t, got, "no cursor is not an error — it is the first page")
	})

	for _, tc := range []struct {
		name   string
		raw    interface{}
		reason string
	}{
		{"NotAnObject", "0x64", "invalid cursor block"},
		{"MissingNumber", map[string]interface{}{"hash": "0xaa"}, "invalid cursor number"},
		{"NegativeNumber", map[string]interface{}{"number": float64(-1)}, "invalid cursor number"},
		{"BadHash", map[string]interface{}{"number": float64(1), "hash": "zz"}, "invalid cursor hash"},
		{"BadParentHash", map[string]interface{}{"number": float64(1), "parentHash": "zz"}, "invalid cursor parentHash"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseQueryCursorBlock(tc.raw)
			require.Error(t, err)
			assert.Nil(t, got)
			assert.Contains(t, err.Error(), tc.reason)
			assert.True(t, common.HasErrorCode(err, common.ErrCodeInvalidRequest),
				"a malformed cursor is the caller's fault, so it must classify as an invalid request")
		})
	}
}

func TestParseQueryFilter_ReadsTopicsOnlyForTheLogsMethod(t *testing.T) {
	raw := map[string]interface{}{
		"topics": []interface{}{
			"0xaa",
			[]interface{}{"0xbb", "0xcc"},
			nil,
		},
	}

	t.Run("LogsMethodReadsThem", func(t *testing.T) {
		got, err := parseQueryFilter("eth_queryLogs", raw)
		require.NoError(t, err)
		require.NotNil(t, got)
		require.Len(t, got.Topics, 3)
		assert.Equal(t, []topicValue{{0xaa}}, got.Topics[0], "a bare string is a one-value topic group")
		assert.Equal(t, []topicValue{{0xbb}, {0xcc}}, got.Topics[1], "an array is an OR group")
		assert.Empty(t, got.Topics[2], "a null topic matches anything, so its group stays empty")
	})

	t.Run("OtherMethodsIgnoreThem", func(t *testing.T) {
		got, err := parseQueryFilter("eth_queryTransactions", raw)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Nil(t, got.Topics, "topics belong to logs — no other method may inherit them")
	})
}

func TestParseQueryFilter_ReadsTheAddressAndFlagFields(t *testing.T) {
	got, err := parseQueryFilter("eth_queryTransactions", map[string]interface{}{
		"from":       "0xaa",
		"to":         []interface{}{"0xbb", "0xcc"},
		"selector":   "0xdd",
		"address":    "0xee",
		"isTopLevel": false,
	})
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, [][]byte{{0xaa}}, got.FromAddresses)
	assert.Equal(t, [][]byte{{0xbb}, {0xcc}}, got.ToAddresses)
	assert.Equal(t, [][]byte{{0xdd}}, got.Selectors)
	assert.Equal(t, [][]byte{{0xee}}, got.LogAddresses)
	require.NotNil(t, got.IsTopLevel, "an explicit false must reach the filter, not read as unset")
	assert.False(t, *got.IsTopLevel)
}

func TestParseQueryFilter_TreatsANonObjectAsNoFilter(t *testing.T) {
	for _, raw := range []interface{}{nil, "0xaa", []interface{}{"0xaa"}, 7} {
		got, err := parseQueryFilter("eth_queryLogs", raw)
		require.NoError(t, err)
		assert.Nil(t, got, "a filter the shim cannot read must mean no filter at all")
	}
}

func TestParseQueryFieldSelection_TreatsANonObjectAsAnEmptySelection(t *testing.T) {
	got, err := parseQueryFieldSelection("not an object")
	require.NoError(t, err)
	require.NotNil(t, got, "the caller always gets a selection struct back")
	assert.Nil(t, got.Blocks)
	assert.Nil(t, got.Logs)
}
