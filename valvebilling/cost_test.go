package valvebilling

import (
	"encoding/json"
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The corpus IS the contract. It is generated in the monorepo from the live
// pricing table plus a case per resolution path, and it is the acceptance test
// the brief names. Do not invent a second oracle here: an earlier design used
// relay.relay_request.weight and it holds exactly one distinct (chain, method)
// pair with a non-zero weight out of 155 seen, because nearly all traffic is
// credit-exempt.
const corpusFile = "testdata/cost-corpus.json"

type corpus struct {
	GeneratedAt    string       `json:"generatedAt"`
	SourceRowCount int          `json:"sourceRowCount"`
	ZeroAddress    string       `json:"zeroAddress"`
	DefaultCU      int64        `json:"defaultCu"`
	Rows           []PriceRow   `json:"rows"`
	Cases          []corpusCase `json:"cases"`
	// MethodCU is the tier-3 compute-unit table, shipped by the generator so
	// that no Go-side copy of it can drift from the TypeScript one.
	MethodCU map[string]int64 `json:"methodCu"`
}

type corpusCase struct {
	Why                       string `json:"why"`
	ChainID                   int64  `json:"chainId"`
	Method                    string `json:"method"`
	TokenAddress              string `json:"tokenAddress"`
	ExpectAmountWei           string `json:"expectAmountWei"`
	ExpectHoldLockUntilSettle bool   `json:"expectHoldLockUntilSettle"`
}

func loadCorpus(t *testing.T) corpus {
	t.Helper()
	raw, err := os.ReadFile(corpusFile)
	require.NoError(t, err, "the cost corpus is the acceptance test; without it nothing here is proven")
	var c corpus
	require.NoError(t, json.Unmarshal(raw, &c))

	require.NotNil(t, c.MethodCU,
		"the corpus must carry methodCu; a Go-side copy of that table is how the two languages drift")

	require.NotEmpty(t, c.Rows)
	require.NotEmpty(t, c.Cases)
	require.NotEmpty(t, c.MethodCU)
	return c
}

func tableFrom(t *testing.T, c corpus) *PriceTable {
	t.Helper()
	tbl := NewPriceTable(c.MethodCU, c.DefaultCU)
	require.NoError(t, tbl.Load(c.Rows))
	return tbl
}

// Definition-of-done item 2: every case, all three tiers.
func TestResolveCost_SatisfiesTheGoldenCorpus(t *testing.T) {
	c := loadCorpus(t)
	tbl := tableFrom(t, c)

	require.Equal(t, ZeroAddress, c.ZeroAddress,
		"this package and the corpus disagree on the zero address, so tier 2 cannot be compared")
	require.Equal(t, c.SourceRowCount, len(c.Rows),
		"the corpus says it captured %d rows but carries %d", c.SourceRowCount, len(c.Rows))

	for _, tc := range c.Cases {
		got := tbl.Resolve(tc.ChainID, tc.Method, tc.TokenAddress)

		want, ok := new(big.Int).SetString(tc.ExpectAmountWei, 10)
		require.True(t, ok, "case %s: expectAmountWei %q is not an integer", tc.Why, tc.ExpectAmountWei)

		assert.Zero(t, got.AmountWei.Cmp(want),
			"case %s (chain %d, %s, token %s): got %s want %s",
			tc.Why, tc.ChainID, tc.Method, tc.TokenAddress, got.AmountWei, want)
		assert.Equal(t, tc.ExpectHoldLockUntilSettle, got.HoldLockUntilSettle,
			"case %s: holdLockUntilSettle", tc.Why)
	}
}

// The corpus labels every case with the tier it means to exercise. Asserting
// the VALUE alone is not enough: a case meant for tier 2 that silently fell
// through to tier 3 could land on the same number by coincidence and pass.
// This checks the path, which is the part that actually breaks.
func TestResolveCost_TakesThePathEachCaseIntendsToExercise(t *testing.T) {
	c := loadCorpus(t)
	tbl := tableFrom(t, c)

	expected := map[string]CostSource{
		"tier1-exact":              SourceExactRow,
		"tier1-mixed-case-address": SourceExactRow,
		// The one that actually pins hazard 1. Every other mixed-case case
		// uses the zero address, which has no hex letters, so uppercasing it
		// is the identity function and the probe is byte-identical to the
		// stored row. Removing the token fold leaves all of those passing and
		// fails only this one.
		"tier1-mixed-case-address-with-hex-letters": SourceExactRow,
		"tier2-zero-address-fallback":               SourceZeroAddressRow,
		"tier3-method-cu":                           SourceMethodConstant,
		"tier3-default-cu":                          SourceDefaultConstant,
		"tier3-default-cu-huge":                     SourceExactRow,
	}

	seen := map[string]int{}
	for _, tc := range c.Cases {
		want, known := expected[tc.Why]
		require.True(t, known,
			"the corpus grew a case label %q this test does not know; classify it rather than ignoring it", tc.Why)
		got := tbl.Resolve(tc.ChainID, tc.Method, tc.TokenAddress)
		assert.Equal(t, want, got.Source,
			"case %s (chain %d, %s, token %s) resolved via %s", tc.Why, tc.ChainID, tc.Method, tc.TokenAddress, got.Source)
		seen[tc.Why]++
	}

	for label := range expected {
		assert.NotZero(t, seen[label], "no case exercised %s", label)
	}
	t.Logf("cases by path: %v", seen)
}

// Hazard 1, stated directly rather than only via the corpus. The TOKEN folds
// to lowercase; the METHOD and CHAIN do not. Folding the method too would
// change pricing with no error anywhere.
func TestResolveCost_FoldsTheTokenButNotTheMethod(t *testing.T) {
	// These two must differ in a hex LETTER, not merely in case somewhere.
	//
	// Uppercasing a string with no hex letters is the identity function, so an
	// address of digits alone probes nothing: the mixed-case lookup would be
	// byte-identical to the stored one and would pass with the fold removed
	// entirely. That is not hypothetical — the monorepo's golden corpus has
	// exactly that hole today, because every address in its 1105 cases is the
	// zero address. Removing strings.ToLower from cacheKey leaves all of them
	// passing and fails only this test.
	//
	// The guard below asserts the property rather than trusting these literals
	// to keep it, so an edit that swapped in a digit-only address fails here
	// instead of silently reopening the hole.
	const storedAddr = "0xabcdefabcdefabcdefabcdefabcdefabcdefabcd"
	const probeAddr = "0xAbCdEfAbCdEfAbCdEfAbCdEfAbCdEfAbCdEfAbCd"

	require.NotEqual(t, storedAddr, probeAddr, "the probe must differ from the stored address")
	require.Equal(t, storedAddr, strings.ToLower(probeAddr), "they must differ only in case")
	require.True(t, strings.ContainsAny(probeAddr, "ABCDEF"),
		"the probe has no upper-case hex letter, so folding it is a no-op and this test proves nothing")

	tbl := NewPriceTable(map[string]int64{}, 6)
	require.NoError(t, tbl.Load([]PriceRow{
		{ChainID: 1, Method: "eth_getLogs", TokenAddress: storedAddr, AmountWei: "42"},
	}))

	mixed := tbl.Resolve(1, "eth_getLogs", probeAddr)
	assert.Equal(t, SourceExactRow, mixed.Source, "an EIP-55 address must hit the row written in lowercase")
	assert.Equal(t, "42", mixed.AmountWei.String())

	// The method is case-sensitive. A different case is a DIFFERENT method and
	// must fall through, not silently reuse this price.
	wrongCase := tbl.Resolve(1, "eth_getlogs", "0xabcdefabcdefabcdefabcdefabcdefabcdefabcd")
	assert.Equal(t, SourceDefaultConstant, wrongCase.Source,
		"the method was folded to lowercase; pricing would change with no error")
}

// Hazard 2. A (chain, method) priced only at the zero address must resolve for
// ANY token — it is a distinct fallback tier, not a default row.
func TestResolveCost_ZeroAddressRowPricesEveryToken(t *testing.T) {
	tbl := NewPriceTable(map[string]int64{"eth_call": 12}, 6)
	require.NoError(t, tbl.Load([]PriceRow{
		{ChainID: 1, Method: "eth_call", TokenAddress: ZeroAddress, AmountWei: "7", HoldLockUntilSettle: true},
	}))

	got := tbl.Resolve(1, "eth_call", "0x1111111111111111111111111111111111111111")
	assert.Equal(t, SourceZeroAddressRow, got.Source)
	assert.Equal(t, "7", got.AmountWei.String(), "the zero-address row must win over the method constant")
	assert.True(t, got.HoldLockUntilSettle, "tier 2 carries the row's settle-mode flag")

	// And it must not shadow a more specific row.
	require.NoError(t, tbl.Load([]PriceRow{
		{ChainID: 1, Method: "eth_call", TokenAddress: ZeroAddress, AmountWei: "7"},
		{ChainID: 1, Method: "eth_call", TokenAddress: "0x1111111111111111111111111111111111111111", AmountWei: "9"},
	}))
	exact := tbl.Resolve(1, "eth_call", "0x1111111111111111111111111111111111111111")
	assert.Equal(t, SourceExactRow, exact.Source)
	assert.Equal(t, "9", exact.AmountWei.String())
}

// Hazard 3. A JSON number must be REFUSED, not rounded. No live row is
// anywhere near 2^53 today — the table maxes at 50 — but the column is
// Postgres numeric and a rounded read would be silent.
func TestPriceRow_RefusesANumericAmount(t *testing.T) {
	var row PriceRow
	err := json.Unmarshal([]byte(`{"chainId":1,"method":"eth_call","tokenAddress":"0x0","amountWei":100000000000000000000}`), &row)
	require.Error(t, err, "a JSON number must not be accepted; it has already lost precision")
	assert.Contains(t, err.Error(), "must be a JSON string")

	require.NoError(t, json.Unmarshal([]byte(`{"amountWei":"100000000000000000000"}`), &row))
	n, err := row.AmountWei.Big()
	require.NoError(t, err)
	assert.Equal(t, "100000000000000000000", n.String(), "a 21-digit amount must survive intact")
}

// Tier 3 can never opt a method into settle-mode. Only a real pricing row can.
func TestResolveCost_TierThreeNeverHoldsTheLock(t *testing.T) {
	tbl := NewPriceTable(map[string]int64{"eth_call": 12}, 6)
	for _, m := range []string{"eth_call", "valve_unknown_method"} {
		got := tbl.Resolve(999, m, ZeroAddress)
		assert.False(t, got.HoldLockUntilSettle, "%s: a constant-priced method must not hold the lock", m)
	}
}
