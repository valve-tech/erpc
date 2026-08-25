package valvebilling

import (
	"encoding/json"
	"math"
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the block-span tariff.
//
// The span is the second pricing dimension, and it is the one an attacker can
// choose. Every test below therefore feeds ExtractSpan something a real client
// sends, something a careless client sends, or something a hostile client
// sends, and asks the same two questions: does this bill the right amount, and
// can it be made to bill nothing.
//
// Two tests are characterization tests. They record what the code DOES, not
// what it should do, and each one says so in its name and its comment.

// testHeads is the chain state most cases resolve against. Latest and
// finalized differ so a test that reads the wrong one fails.
var testHeads = Heads{Latest: 1_000_000, Finalized: 999_000}

// The tariff used through most of the file: 1,000 blocks buy one unit, and a
// unit costs 5 credits. Both numbers are arbitrary — the rounding rule is not.
var testTariff = RangePrice{BlocksPerUnit: 1000, CreditsPerUnit: 5}

// ---------------------------------------------------------------------------
// ExtractSpan
// ---------------------------------------------------------------------------

// The shapes clients actually put on the wire, and the shapes they put on it
// by accident. Each case asserts the WHOLE Span, so a case that means to test
// resolution cannot pass by landing on the right block numbers with the wrong
// Found flag.
func TestExtractSpan_ReadsTheShapesClientsSend(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params string
		heads  Heads
		want   Span
		why    string
	}{
		{
			name:   "the real eth_getLogs filter object",
			params: `[{"fromBlock":"0x1","toBlock":"0x2","address":"0xdead","topics":[]}]`,
			heads:  testHeads,
			want:   Span{Found: true, Resolved: true, From: 1, To: 2},
			why:    "this is the shape every logs client sends; nothing else matters if this fails",
		},
		{
			name:   "one block, both ends equal",
			params: `[{"fromBlock":"0x64","toBlock":"0x64"}]`,
			heads:  testHeads,
			want:   Span{Found: true, Resolved: true, From: 100, To: 100},
			why:    "the inclusive count makes this one block, not zero; see the Blocks test",
		},
		{
			name:   "the filter object is the SECOND element",
			params: `["0xdeadbeef",{"fromBlock":"0x1","toBlock":"0xa"}]`,
			heads:  testHeads,
			want:   Span{Found: true, Resolved: true, From: 1, To: 10},
			why:    "trace_filter and the filter-id methods put a string first; the walk must not stop there",
		},
		{
			name:   "an earlier object carries no range",
			params: `[{"to":"0xdead","data":"0x00"},{"fromBlock":"0x1","toBlock":"0x3"}]`,
			heads:  testHeads,
			want:   Span{Found: true, Resolved: true, From: 1, To: 3},
			why:    "the first object that carries a range wins, not the first object",
		},
		{
			name:   "params carry no range at all",
			params: `["0x1",false]`,
			heads:  testHeads,
			want:   Span{},
			why:    "the common case: eth_getBlockByNumber and everything like it pays the flat price only",
		},
		{
			name:   "params sent by name as one object",
			params: `{"fromBlock":"0x1","toBlock":"0x2"}`,
			heads:  testHeads,
			want:   Span{Found: true, Resolved: true, From: 1, To: 2},
			why:    "by-name params are legal JSON-RPC 2.0; reading only the array made this shape a free range",
		},
		{
			name:   "params sent by name with no range",
			params: `{"address":"0xdead","topics":[]}`,
			heads:  testHeads,
			want:   Span{},
			why:    "the by-name fallback must find a range, not invent one; no range here is still no range",
		},
		{
			name:   "params sent by name with a tag that cannot resolve",
			params: `{"fromBlock":"earliest","toBlock":"pending"}`,
			heads:  testHeads,
			want:   Span{Found: true},
			why:    "the by-name form reaches the same resolver, so it reaches the same refusal",
		},
		{
			name:   "params are a bare JSON string",
			params: `"0x1"`,
			heads:  testHeads,
			want:   Span{},
			why:    "the fallback wraps the whole value; a string is not an object and must not error or be found",
		},
		{
			name:   "params are a bare JSON number",
			params: `12345`,
			heads:  testHeads,
			want:   Span{},
			why:    "the same, for the other scalar a careless client sends",
		},
		{
			name:   "params are JSON null",
			params: `null`,
			heads:  testHeads,
			want:   Span{},
			why:    "null unmarshals into a nil slice and then a nil map; neither carries a range and neither panics",
		},
		{
			name:   "params are an empty object",
			params: `{}`,
			heads:  testHeads,
			want:   Span{},
			why:    "an object with no keys is the by-name form of no arguments",
		},
		{
			name:   "a by-name object nesting the filter under a key",
			params: `{"filter":{"fromBlock":"0x1","toBlock":"0x2"}}`,
			heads:  testHeads,
			want:   Span{},
			why:    "the walk reads top-level keys only and does not recurse; a nested filter is priced flat, which is a limit, not a rule",
		},
		{
			name:   "params are an empty array",
			params: `[]`,
			heads:  testHeads,
			want:   Span{},
			why:    "eth_blockNumber and friends",
		},
		{
			name:   "params are not JSON",
			params: `not json at all`,
			heads:  testHeads,
			want:   Span{},
			why:    "an unparseable body is upstream's problem to reject, not this one's to guess at",
		},
		{
			name:   "params carry a JSON null element",
			params: `[null,{"fromBlock":"0x1","toBlock":"0x2"}]`,
			heads:  testHeads,
			want:   Span{Found: true, Resolved: true, From: 1, To: 2},
			why:    "a null unmarshals into a nil map; reading a key from it must not panic",
		},
		{
			name:   "eth_getLogs by blockHash carries no fromBlock or toBlock",
			params: `[{"blockHash":"0xabc","topics":[]}]`,
			heads:  testHeads,
			want:   Span{},
			why:    "a blockHash query is one block by construction, so the flat price is the whole price",
		},
		{
			name:   "only fromBlock, with a head",
			params: `[{"fromBlock":"0x1"}]`,
			heads:  testHeads,
			want:   Span{Found: true, Resolved: true, From: 1, To: 1_000_000},
			why:    "toBlock absent means latest in the filter object, and this is the expensive open-ended query",
		},
		{
			name:   "only fromBlock, no head",
			params: `[{"fromBlock":"0x1"}]`,
			heads:  Heads{},
			want:   Span{Found: true},
			why:    "without a head there is no latest, so the range is found but not resolved",
		},
		{
			name:   "only toBlock, with a head",
			params: `[{"toBlock":"0x5"}]`,
			heads:  testHeads,
			want:   Span{Found: true, Resolved: true, From: 5, To: 1_000_000},
			why:    "fromBlock absent also means latest, and the swap then makes the span the whole chain",
		},
		{
			name:   "an explicit null end",
			params: `[{"fromBlock":"0x1","toBlock":null}]`,
			heads:  testHeads,
			want:   Span{Found: true, Resolved: true, From: 1, To: 1_000_000},
			why:    "web3.js sends an explicit null where the spec means absent",
		},
		{
			name:   "an explicit null end with no head",
			params: `[{"fromBlock":"0x1","toBlock":null}]`,
			heads:  Heads{},
			want:   Span{Found: true},
			why:    "null is latest, and latest needs a head",
		},
		{
			name:   "the ends arrive as JSON numbers",
			params: `[{"fromBlock":100,"toBlock":200}]`,
			heads:  testHeads,
			want:   Span{Found: true, Resolved: true, From: 100, To: 200},
			why:    "some clients send decimal numbers rather than hex strings; both are on the wire",
		},
		{
			name:   "a negative JSON number",
			params: `[{"fromBlock":-1,"toBlock":200}]`,
			heads:  testHeads,
			want:   Span{Found: true},
			why:    "a negative height is not a block; resolving it would make the span longer than the chain",
		},
		{
			name:   "a JSON number too large for an int64",
			params: `[{"fromBlock":0,"toBlock":99999999999999999999}]`,
			heads:  testHeads,
			want:   Span{Found: true},
			why:    "the decimal path must refuse what the hex path refuses",
		},
		{
			name:   "a fractional JSON number",
			params: `[{"fromBlock":1.5,"toBlock":200}]`,
			heads:  testHeads,
			want:   Span{Found: true},
			why:    "half a block does not exist",
		},
		{
			name:   "a JSON boolean end",
			params: `[{"fromBlock":true,"toBlock":"0x2"}]`,
			heads:  testHeads,
			want:   Span{Found: true},
			why:    "a boolean is neither a number nor a string, and it must not read as block 1",
		},
		{
			name:   "a non-hex string",
			params: `[{"fromBlock":"banana","toBlock":"0x2"}]`,
			heads:  testHeads,
			want:   Span{Found: true},
			why:    "an unknown word is not a tag and not a number",
		},
		{
			name:   "a decimal string without the 0x prefix",
			params: `[{"fromBlock":"100","toBlock":"200"}]`,
			heads:  testHeads,
			want:   Span{Found: true},
			why:    "the wire format is hex; a bare decimal string is refused rather than read as hex or as decimal",
		},
		{
			name:   "the 0x prefix with no digits",
			params: `[{"fromBlock":"0x","toBlock":"0x2"}]`,
			heads:  testHeads,
			want:   Span{Found: true},
			why:    "an empty hex body is not a zero",
		},
		{
			name:   "a hex block above 2^63",
			params: `[{"fromBlock":"0x1","toBlock":"0xffffffffffffffff"}]`,
			heads:  testHeads,
			want:   Span{Found: true},
			why:    "a uint64 that does not fit an int64 must be refused, not truncated to -1 and then swapped into a range",
		},
		{
			name:   "the first hex block an int64 cannot hold",
			params: `[{"fromBlock":"0x1","toBlock":"0x8000000000000000"}]`,
			heads:  testHeads,
			want:   Span{Found: true},
			why:    "2^63 exactly; the boundary the ceiling has to sit on",
		},
		{
			name:   "the largest hex block an int64 holds",
			params: `[{"fromBlock":"0x0","toBlock":"0x7fffffffffffffff"}]`,
			heads:  testHeads,
			want:   Span{Found: true, Resolved: true, From: 0, To: math.MaxInt64},
			why:    "2^63-1 is representable, so it resolves and Credits prices it; the hostile probe gets a real number",
		},
		{
			name:   "uppercase hex digits and an uppercase prefix",
			params: `[{"fromBlock":"0X10","toBlock":"0xFF"}]`,
			heads:  testHeads,
			want:   Span{Found: true, Resolved: true, From: 16, To: 255},
			why:    "hex is case-insensitive on both the prefix and the digits",
		},
		{
			name:   "whitespace around a hex end",
			params: `[{"fromBlock":" 0x5 ","toBlock":"0xa"}]`,
			heads:  testHeads,
			want:   Span{Found: true, Resolved: true, From: 5, To: 10},
			why:    "a client that pads its numbers still names a real range",
		},
		{
			name:   "the ends arrive reversed",
			params: `[{"fromBlock":"0x10","toBlock":"0x1"}]`,
			heads:  testHeads,
			want:   Span{Found: true, Resolved: true, From: 1, To: 16},
			why:    "eRPC's own blockSpan swaps rather than going negative; a negative span would bill nothing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractSpan([]byte(tc.params), tc.heads)
			assert.Equal(t, tc.want, got, tc.why)
		})
	}
}

// The five tags, and the two eRPC refuses to translate.
//
// "safe" and "pending" are unresolved on purpose: json_rpc.go:55-80 passes
// both to the upstream untouched because eRPC tracks neither. This module
// declines the same guess. Charging either as "latest" would overbill by an
// amount nobody can check.
func TestExtractSpan_ResolvesOnlyTheTagsERPCResolves(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tag   string
		heads Heads
		want  Span
		why   string
	}{
		{
			name: "earliest is zero and needs no head",
			tag:  "earliest", heads: Heads{},
			want: Span{Found: true, Resolved: true, From: 0, To: 100},
			why:  "genesis is a constant, so no chain state is required to price it",
		},
		{
			name: "latest reads the head",
			tag:  "latest", heads: testHeads,
			want: Span{Found: true, Resolved: true, From: 100, To: 1_000_000},
			why:  "the ends swap, so latest lands on To",
		},
		{
			name: "latest with no head is unresolved",
			tag:  "latest", heads: Heads{},
			want: Span{Found: true},
			why:  "a zero head means the poller has not answered yet; eRPC treats zero as unknown too",
		},
		{
			name: "finalized reads the finalized head",
			tag:  "finalized", heads: testHeads,
			want: Span{Found: true, Resolved: true, From: 100, To: 999_000},
			why:  "finalized must not read the latest head; the two differ here so a swap fails",
		},
		{
			name: "finalized with only a latest head is unresolved",
			tag:  "finalized", heads: Heads{Latest: 1_000_000},
			want: Span{Found: true},
			why:  "a chain with no finalized state cannot have finalized priced from latest",
		},
		{
			name: "safe is never resolved",
			tag:  "safe", heads: testHeads,
			want: Span{Found: true},
			why:  "safe sits at an unknown point between finalized and latest; eRPC will not guess and neither does this",
		},
		{
			name: "pending is never resolved",
			tag:  "pending", heads: testHeads,
			want: Span{Found: true},
			why:  "pending is a mempool view nothing here can see",
		},
		{
			name: "an unknown tag is unresolved",
			tag:  "whenever", heads: testHeads,
			want: Span{Found: true},
			why:  "a word this does not know must not fall through to a number",
		},
		{
			name: "tags are case-insensitive",
			tag:  "LATEST", heads: testHeads,
			want: Span{Found: true, Resolved: true, From: 100, To: 1_000_000},
			why:  "clients send Latest and LATEST; the tag is a wire word, not a hash key",
		},
		{
			name: "a padded tag still resolves",
			tag:  "  finalized  ", heads: testHeads,
			want: Span{Found: true, Resolved: true, From: 100, To: 999_000},
			why:  "whitespace around a tag is a client quirk, not a different tag",
		},
		{
			name: "a padded refused tag stays refused",
			tag:  "  SAFE  ", heads: testHeads,
			want: Span{Found: true},
			why:  "trimming and folding must not turn a refused tag into a resolved one",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params, err := json.Marshal([]any{map[string]any{"fromBlock": "0x64", "toBlock": tc.tag}})
			require.NoError(t, err)
			got := ExtractSpan(params, tc.heads)
			assert.Equal(t, tc.want, got, tc.why)
		})
	}
}

// earliest to latest is the whole chain, and it is the query that costs the
// most. It must resolve, because refusing it is how the largest request on the
// relay becomes the cheapest.
func TestExtractSpan_PricesTheWholeChainQuery(t *testing.T) {
	got := ExtractSpan([]byte(`[{"fromBlock":"earliest","toBlock":"latest"}]`), testHeads)
	require.Equal(t, Span{Found: true, Resolved: true, From: 0, To: 1_000_000}, got)
	assert.Equal(t, int64(1_000_001), got.Blocks())
}

// A range that is absent and a range that cannot be resolved are different
// answers, and the caller must be able to tell them apart. Both bill zero
// range credits today, so a struct that conflated them would hide the second
// case entirely — and the second case is the one a client can force.
func TestExtractSpan_SeparatesNoRangeFromAnUnresolvableRange(t *testing.T) {
	absent := ExtractSpan([]byte(`["0x1",false]`), testHeads)
	unresolvable := ExtractSpan([]byte(`[{"fromBlock":"0x1","toBlock":"pending"}]`), testHeads)

	assert.False(t, absent.Found, "no range at all")
	assert.True(t, unresolvable.Found, "a range is named; only its end is unknown")
	assert.False(t, unresolvable.Resolved)
	assert.NotEqual(t, absent, unresolvable,
		"a caller that wants to refuse an unpriceable range must be able to see it")

	// The two states now bill differently, so conflating them would give the
	// whole chain away at the flat price.
	_, absentErr := testTariff.Credits(absent)
	_, unresolvableErr := testTariff.Credits(unresolvable)
	assert.NoError(t, absentErr, "no range is the common case and it is free")
	assert.Error(t, unresolvableErr, "a named range that cannot be priced is refused")
}

// ---------------------------------------------------------------------------
// Span.Blocks
// ---------------------------------------------------------------------------

// The count is inclusive of both ends, matching eRPC's own blockSpan at
// architecture/evm/eth_query_helpers.go:548. An exclusive count makes the
// single-block query zero blocks, and a zero-block query is free.
func TestSpanBlocks_CountsBothEnds(t *testing.T) {
	for _, tc := range []struct {
		name string
		span Span
		want int64
	}{
		{"one block", Span{Resolved: true, From: 100, To: 100}, 1},
		{"two blocks", Span{Resolved: true, From: 100, To: 101}, 2},
		{"genesis alone", Span{Resolved: true, From: 0, To: 0}, 1},
		{"a thousand blocks", Span{Resolved: true, From: 1, To: 1000}, 1000},
		{"an unresolved span counts nothing", Span{Found: true}, 0},
		{"the zero span counts nothing", Span{}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.span.Blocks(), "blocks between %d and %d", tc.span.From, tc.span.To)
		})
	}
}

// CHARACTERIZATION, not a requirement. This records a defect.
//
// Blocks computes To - From + 1, and exactly one valid span does not fit: 0 to
// MaxInt64 is 2^63 blocks, one more than an int64 can hold, so the addition
// wraps to MinInt64. Every other span in range is safe, because From is never
// negative and the sum only overflows when To - From is already MaxInt64.
//
// Credits no longer reads Blocks, so this wrap does not reach a bill. It stays
// here because Blocks is exported: a caller that logs it, sums it or compares
// it against a span limit gets a large negative number with nothing going red.
// The fix is a signature change, which the owner should choose.
func TestSpanBlocks_WrapsOnTheOneSpanAnInt64CannotCount(t *testing.T) {
	widest := Span{Resolved: true, From: 0, To: math.MaxInt64}

	got := widest.Blocks()

	assert.Equal(t, int64(math.MinInt64), got,
		"this is the wrap, pinned so a change to Blocks is a deliberate change")
	assert.Negative(t, got, "a block count is never negative; this one is")
}

// ---------------------------------------------------------------------------
// RangePrice.Credits
// ---------------------------------------------------------------------------

// A partial unit rounds UP, and the whole tariff depends on it.
func TestRangeCredits_RoundsAPartialUnitUp(t *testing.T) {
	for _, tc := range []struct {
		name   string
		blocks int64
		want   int64
		why    string
	}{
		{"a single block", 1, 5, "the common case, and the case truncation makes free"},
		{"one block short of a unit", 999, 5, "still one unit"},
		{"exactly one unit", 1000, 5, "an exact multiple must not buy a second unit"},
		{"one block past a unit", 1001, 10, "the partial second unit rounds up"},
		{"exactly two units", 2000, 10, "the exact multiple again, one unit higher"},
		{"one block past two units", 2001, 15, ""},
		{"five million blocks", 5_000_000, 25_000, "the archive query the flat method price undercharges"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			span := Span{Resolved: true, From: 1, To: tc.blocks}
			require.Equal(t, tc.blocks, span.Blocks(), "the fixture must name the block count it says it does")

			got, err := testTariff.Credits(span)

			require.NoError(t, err)
			assert.Equal(t, tc.want, got, tc.why)
		})
	}
}

// The counterfactual, stated as arithmetic rather than as a claim.
//
// Truncating integer division prices every span under BlocksPerUnit at zero.
// At 1,000 blocks to the unit that is every single-block eth_getLogs on every
// chain, which is most of the traffic the tariff exists to price. This checks
// that truncation really would be free and that the shipped code is not.
func TestRangeCredits_TruncationWouldMakeASingleBlockRequestFree(t *testing.T) {
	single := Span{Resolved: true, From: 18_000_000, To: 18_000_000}
	require.Equal(t, int64(1), single.Blocks())

	truncated := single.Blocks() / testTariff.BlocksPerUnit * testTariff.CreditsPerUnit
	require.Zero(t, truncated,
		"truncation prices a one-block query at nothing; this is the bug the round-up rule prevents")

	got, err := testTariff.Credits(single)

	require.NoError(t, err)
	assert.Equal(t, testTariff.CreditsPerUnit, got,
		"the shipped code charges one whole unit for the partial unit")
	assert.Positive(t, got, "a request that does work must not be free")
}

// exactCredits is the answer the tariff would give with unbounded integers. It
// is the same arithmetic — round the span up to a whole unit, then multiply —
// done in big.Int, and it reports whether the answer fits an int64.
//
// It exists because a wrap does not look like a wrap. A saturated or wrapped
// charge is still a number, and a test that asserts "some number came back"
// passes on both the right one and a negative one.
func exactCredits(p RangePrice, s Span) (*big.Int, bool) {
	blocks := new(big.Int).Sub(big.NewInt(s.To), big.NewInt(s.From))
	blocks.Add(blocks, big.NewInt(1))

	units, rem := new(big.Int).QuoRem(blocks, big.NewInt(p.BlocksPerUnit), new(big.Int))
	if rem.Sign() != 0 {
		units.Add(units, big.NewInt(1))
	}
	total := units.Mul(units, big.NewInt(p.CreditsPerUnit))
	return total, total.IsInt64()
}

// A dishonest span must produce the true charge or an error, never a third
// number.
//
// The rule is exact: where the charge fits an int64 the caller gets it, and
// where it does not the caller gets an error and zero. Refusing a charge that
// fits would be its own defect — a span of 2^63 blocks at 5 credits per 1,000
// is 46 quadrillion credits, which is a real number the credit gate then
// refuses on its own merits.
func TestRangeCredits_MatchesUnboundedArithmeticOrRefusesToAnswer(t *testing.T) {
	const maxInt64 = int64(math.MaxInt64)

	for _, tc := range []struct {
		name   string
		tariff RangePrice
		span   Span
		why    string
	}{
		{
			name:   "the widest span an int64 can name",
			tariff: testTariff,
			span:   Span{Resolved: true, From: 0, To: maxInt64},
			why:    "toBlock 0x7fffffffffffffff against fromBlock 0x0, the probe a hostile client sends",
		},
		{
			name:   "the widest span at one credit per block",
			tariff: RangePrice{BlocksPerUnit: 1, CreditsPerUnit: 1},
			span:   Span{Resolved: true, From: 0, To: maxInt64},
			why:    "2^63 units of one credit does not fit; this one must be refused",
		},
		{
			name:   "the widest span at a large unit price",
			tariff: RangePrice{BlocksPerUnit: 1000, CreditsPerUnit: 1_000_000},
			span:   Span{Resolved: true, From: 0, To: maxInt64},
			why:    "the units fit an int64 on their own and the product does not; the guard is the only thing between",
		},
		{
			name:   "one block short of the widest span",
			tariff: testTariff,
			span:   Span{Resolved: true, From: 1, To: maxInt64},
			why:    "the neighbour of the wrapping case must still price exactly",
		},
		{
			name:   "the largest span that fits at three credits a block",
			tariff: RangePrice{BlocksPerUnit: 1, CreditsPerUnit: 3},
			span:   Span{Resolved: true, From: 1, To: maxInt64 / 3},
			why:    "units is exactly MaxInt64/CreditsPerUnit, the last value the guard admits",
		},
		{
			name:   "one block past the largest span that fits",
			tariff: RangePrice{BlocksPerUnit: 1, CreditsPerUnit: 3},
			span:   Span{Resolved: true, From: 1, To: maxInt64/3 + 1},
			why:    "the first value the guard rejects; MaxInt64/3 truncates, so this boundary is where an off-by-one hides",
		},
		{
			name:   "a span whose units fit but whose charge does not",
			tariff: RangePrice{BlocksPerUnit: 2, CreditsPerUnit: maxInt64 / 4},
			span:   Span{Resolved: true, From: 0, To: 9},
			why:    "five units at a quarter of MaxInt64 each",
		},
		{
			name:   "an ordinary archive query",
			tariff: testTariff,
			span:   Span{Resolved: true, From: 1, To: 5_000_000},
			why:    "the oracle must agree with the ordinary case too, or it proves nothing about the extreme ones",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, fits := exactCredits(tc.tariff, tc.span)

			got, err := tc.tariff.Credits(tc.span)

			if fits {
				require.NoError(t, err, "%s: the exact charge is %s, which fits an int64", tc.why, want)
				assert.Equal(t, want.Int64(), got, tc.why)
				return
			}
			require.Error(t, err, "%s: the exact charge is %s, which does not fit an int64", tc.why, want)
			assert.Zero(t, got, "a refused charge returns nothing, not a saturated amount")
			assert.Contains(t, err.Error(), "overflow",
				"the error must say why the request was refused")
		})
	}
}

// No span at any tariff may produce a negative charge. A negative charge does
// not just misbill: it CREDITS the account, because the credit gate subtracts
// the cost from the balance.
func TestRangeCredits_NeverReturnsANegativeCharge(t *testing.T) {
	tariffs := []RangePrice{
		{BlocksPerUnit: 1, CreditsPerUnit: 1},
		{BlocksPerUnit: 1000, CreditsPerUnit: 5},
		{BlocksPerUnit: 1, CreditsPerUnit: math.MaxInt64},
		{BlocksPerUnit: math.MaxInt64, CreditsPerUnit: math.MaxInt64},
	}
	spans := []Span{
		{Resolved: true, From: 0, To: 0},
		{Resolved: true, From: 0, To: math.MaxInt64},
		{Resolved: true, From: 1, To: math.MaxInt64},
		{Resolved: true, From: math.MaxInt64 - 1, To: math.MaxInt64},
		{Resolved: true, From: math.MaxInt64, To: math.MaxInt64},
	}

	for _, p := range tariffs {
		for _, s := range spans {
			got, err := p.Credits(s)
			if err != nil {
				assert.Zero(t, got, "an error must carry no charge")
				continue
			}
			assert.GreaterOrEqual(t, got, int64(0),
				"span %d..%d at %d credits per %d blocks billed a negative amount",
				s.From, s.To, p.CreditsPerUnit, p.BlocksPerUnit)
		}
	}
}

// An inverted span is not an error and not a negative charge. ExtractSpan
// orders the ends before Credits ever sees them, so this only defends a Span a
// caller built by hand.
func TestRangeCredits_ChargesNothingForASpanBuiltBackwards(t *testing.T) {
	got, err := testTariff.Credits(Span{Resolved: true, From: 100, To: 1})

	require.NoError(t, err)
	assert.Zero(t, got, "the ordered path is ExtractSpan's job; this one just refuses to invent a charge")
}

// The tariff is off by default, and off means zero — including for the spans
// that would otherwise cost the most.
func TestRangeCredits_ChargesNothingWhileTheTariffIsOff(t *testing.T) {
	widest := Span{Resolved: true, From: 0, To: math.MaxInt64}

	for _, tc := range []struct {
		name   string
		tariff RangePrice
		why    string
	}{
		{"the zero tariff", RangePrice{}, "the zero value is today's behaviour exactly"},
		{"blocks without credits", RangePrice{BlocksPerUnit: 1000}, "half a tariff prices nothing"},
		{"credits without blocks", RangePrice{CreditsPerUnit: 5}, "a zero divisor must not reach the division"},
		{"a negative unit size", RangePrice{BlocksPerUnit: -1000, CreditsPerUnit: 5}, "a negative divisor would flip the sign of the charge"},
		{"a negative unit price", RangePrice{BlocksPerUnit: 1000, CreditsPerUnit: -5}, "a negative price would credit the account"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.False(t, tc.tariff.Enabled(), tc.why)

			got, err := tc.tariff.Credits(widest)

			require.NoError(t, err)
			assert.Zero(t, got, tc.why)
		})
	}
}

// A range this cannot price is refused, not given away.
//
// This was a characterization test. Credits used to return (0, nil) here, and
// that was the defect: eRPC forwards "safe" and "pending" to the upstream
// untouched, so a client asked for the whole chain with toBlock "safe", got
// the data, and paid the flat method price only. One word bought the tariff
// off.
//
// Credits now returns an error, which moves the fee decision to the caller and
// makes it visible. The caller may refuse the request or bill it flat and
// count it; both are defensible. Silence was not.
func TestRangeCredits_RefusesToPriceARangeItCannotResolve(t *testing.T) {
	span := ExtractSpan([]byte(`[{"fromBlock":"earliest","toBlock":"safe"}]`), testHeads)
	require.True(t, span.Found, "the client did name a range")
	require.False(t, span.Resolved)

	got, err := testTariff.Credits(span)

	require.Error(t, err, "the whole chain through a tag eRPC forwards verbatim must not be free")
	assert.Zero(t, got, "a refused range carries no charge")
	assert.Contains(t, err.Error(), "cannot resolve",
		"the error must say what the caller has to decide about")
}

// Every input that lands on Resolved:false is refused. The list is the whole
// set of ways a client can reach that state, and each one is a whole-chain
// query if the upstream answers it.
func TestRangeCredits_RefusesEveryRangeThatFailsToResolve(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params string
		heads  Heads
		why    string
	}{
		{
			name: "safe", params: `[{"fromBlock":"0x0","toBlock":"safe"}]`, heads: testHeads,
			why: "eRPC forwards safe untouched, so the upstream answers it in full",
		},
		{
			name: "pending", params: `[{"fromBlock":"0x0","toBlock":"pending"}]`, heads: testHeads,
			why: "the same for pending",
		},
		{
			name: "latest with no head", params: `[{"fromBlock":"0x0","toBlock":"latest"}]`, heads: Heads{},
			why: "a cold poller must not make the whole chain free",
		},
		{
			name: "finalized with no head", params: `[{"fromBlock":"0x0","toBlock":"finalized"}]`, heads: Heads{},
			why: "the same for finalized",
		},
		{
			name: "an absent end with no head", params: `[{"fromBlock":"0x0"}]`, heads: Heads{},
			why: "the defaulted end reaches the same resolver as a written one",
		},
		{
			name: "an explicit null end with no head", params: `[{"fromBlock":"0x0","toBlock":null}]`, heads: Heads{},
			why: "so does null",
		},
		{
			name: "a decimal string end", params: `[{"fromBlock":"0","toBlock":"100"}]`, heads: testHeads,
			why: "a parse failure must not be cheaper than a well-formed range",
		},
		{
			name: "the 0x prefix with no digits", params: `[{"fromBlock":"0x","toBlock":"0x2"}]`, heads: testHeads,
			why: "an empty hex body is the shortest way to write an unpriceable range",
		},
		{
			name: "a fractional number end", params: `[{"fromBlock":0,"toBlock":1.5}]`, heads: testHeads,
			why: "half a block does not resolve and must not bill nothing",
		},
		{
			name: "a boolean end", params: `[{"fromBlock":true,"toBlock":"0x2"}]`, heads: testHeads,
			why: "a boolean is neither a number nor a tag",
		},
		{
			name: "a hex block above 2^63", params: `[{"fromBlock":"0x0","toBlock":"0xffffffffffffffff"}]`, heads: testHeads,
			why: "the probe that would saturate; refusing it beats billing zero for it",
		},
		{
			name: "a negative JSON number", params: `[{"fromBlock":-1,"toBlock":100}]`, heads: testHeads,
			why: "a negative height is the other way to write nonsense",
		},
		{
			name: "an unknown tag", params: `[{"fromBlock":"0x0","toBlock":"whenever"}]`, heads: testHeads,
			why: "a word this does not know is a range it cannot price",
		},
		{
			name: "by name, with a tag that cannot resolve", params: `{"fromBlock":"0x0","toBlock":"safe"}`, heads: testHeads,
			why: "the by-name form must not be the way around the refusal",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			span := ExtractSpan([]byte(tc.params), tc.heads)
			require.True(t, span.Found, "the fixture must name a range, or it exercises the wrong path")
			require.False(t, span.Resolved, "the fixture must fail to resolve, or it exercises the wrong path")

			got, err := testTariff.Credits(span)

			require.Error(t, err, tc.why)
			assert.Zero(t, got, "a refused range carries no charge")
			assert.Contains(t, err.Error(), "cannot resolve", tc.why)
		})
	}
}

// No range at all is still free, and that is most of the traffic. The refusal
// above must not reach eth_blockNumber.
func TestRangeCredits_ChargesNothingWhenTheRequestNamesNoRange(t *testing.T) {
	for _, params := range []string{
		`["0x1",false]`,
		`[]`,
		`{}`,
		`null`,
		`"a bare string"`,
		`not json at all`,
		`[{"blockHash":"0xabc","topics":[]}]`,
	} {
		t.Run(params, func(t *testing.T) {
			span := ExtractSpan([]byte(params), testHeads)
			require.False(t, span.Found, "this fixture names no range")

			got, err := testTariff.Credits(span)

			require.NoError(t, err, "the flat method price is the whole charge for a request with no range")
			assert.Zero(t, got)
		})
	}
}

// Credits reads Resolved before Found.
//
// The two fields can express a combination ExtractSpan never builds. A Span
// built by hand with Resolved:true and Found:false is a real span, and reading
// Found first priced it at nothing. This pins the order.
func TestRangeCredits_PricesAResolvedSpanEvenWhenFoundIsUnset(t *testing.T) {
	got, err := testTariff.Credits(Span{Resolved: true, From: 1, To: 1000})

	require.NoError(t, err)
	assert.Equal(t, int64(5), got, "resolved is the stronger statement, so it decides first")
}

// The tariff being off wins over the refusal. A deployment that never
// configured a range charge must not start refusing requests because a client
// wrote "pending".
func TestRangeCredits_RefusesNothingWhileTheTariffIsOff(t *testing.T) {
	unresolvable := ExtractSpan([]byte(`[{"fromBlock":"0x0","toBlock":"pending"}]`), testHeads)
	require.True(t, unresolvable.Found)
	require.False(t, unresolvable.Resolved)

	got, err := RangePrice{}.Credits(unresolvable)

	require.NoError(t, err, "off means off; the zero tariff is stock behaviour and refuses nothing")
	assert.Zero(t, got)
}

// CHARACTERIZATION, and an operational warning rather than a rule.
//
// Heads{} is what a relay holds until the head poller answers, and every
// defaulted or tag-ended range is unresolvable in that window. Credits refuses
// all of them, so on a cold start the caller decides the fate of every
// eth_getLogs that omits toBlock. That is the correct refusal — the span
// really is unknown — but it is a refusal that arrives in a burst at boot, and
// whoever wires this in has to choose the fallback deliberately.
func TestRangeCredits_RefusesTheDefaultedRangeUntilTheHeadPollerAnswers(t *testing.T) {
	params := []byte(`[{"fromBlock":"0x1"}]`)

	cold := ExtractSpan(params, Heads{})
	_, err := testTariff.Credits(cold)
	require.Error(t, err, "no head means no latest, and no latest means no span")

	warm := ExtractSpan(params, testHeads)
	got, err := testTariff.Credits(warm)
	require.NoError(t, err, "the same request prices normally once the poller has answered")
	assert.Equal(t, int64(5000), got, "1 to 1,000,000 is 1,000 units")
}

// ---------------------------------------------------------------------------
// The corpus cross-check
// ---------------------------------------------------------------------------

// CHARACTERIZATION. This pins the mispricing the file exists to fix; it does
// not endorse it.
//
// The corpus is the live pricing table. eth_getLogs is 18 credits on every
// chain in it, and no row or case in the corpus carries a block, a range or a
// span field at all — there is nowhere for a span to be priced. So a one-block
// query and a five-million-block query resolve to the same 18 credits, which
// is the same price for six orders of magnitude more work.
//
// The second half shows the tariff separating them. When the range charge is
// wired into the request path, the first assertion below is the one that has
// to change.
func TestRangeCredits_PinsTheFlatGetLogsPriceTheCorpusCarries(t *testing.T) {
	c := loadCorpus(t)
	tbl := tableFrom(t, c)

	require.Equal(t, int64(18), c.MethodCU["eth_getLogs"],
		"the corpus prices eth_getLogs flat at 18 credits")

	raw, err := os.ReadFile(corpusFile)
	require.NoError(t, err)
	for _, field := range corpusFieldNames(t, raw) {
		for _, forbidden := range []string{"block", "range", "span"} {
			assert.NotContains(t, strings.ToLower(field), forbidden,
				"the corpus grew a %q dimension in field %q; this test is stale and the pricing path needs re-reading",
				forbidden, field)
		}
	}

	oneBlock := ExtractSpan([]byte(`[{"fromBlock":"0x1140000","toBlock":"0x1140000"}]`), testHeads)
	archive := ExtractSpan([]byte(`[{"fromBlock":"0x0","toBlock":"0x4c4b40"}]`), testHeads)
	require.Equal(t, int64(1), oneBlock.Blocks())
	require.Equal(t, int64(5_000_001), archive.Blocks())

	// The defect. The method table cannot see the span, so both cost 18.
	cheap := tbl.Resolve(1, "eth_getLogs", ZeroAddress)
	dear := tbl.Resolve(1, "eth_getLogs", ZeroAddress)
	assert.Equal(t, "18", cheap.AmountWei.String())
	assert.Equal(t, cheap.AmountWei.String(), dear.AmountWei.String(),
		"five million blocks bills what one block bills; this is the mispricing, pinned")

	// The fix, once the range charge is added on top.
	cheapRange, err := testTariff.Credits(oneBlock)
	require.NoError(t, err)
	dearRange, err := testTariff.Credits(archive)
	require.NoError(t, err)

	assert.Equal(t, int64(5), cheapRange, "one block buys one unit")
	assert.Equal(t, int64(25_005), dearRange, "5,000,001 blocks buy 5,001 units")
	assert.NotEqual(t, cheapRange, dearRange, "the span dimension is what separates them")
}

// corpusFieldNames collects every field name the corpus uses in its rows and
// its cases. It reads the raw JSON rather than the typed structs, because the
// typed structs would silently drop a field the generator added.
func corpusFieldNames(t *testing.T, raw []byte) []string {
	t.Helper()
	var top struct {
		Rows  []map[string]json.RawMessage `json:"rows"`
		Cases []map[string]json.RawMessage `json:"cases"`
	}
	require.NoError(t, json.Unmarshal(raw, &top))

	seen := map[string]bool{}
	var names []string
	for _, group := range [][]map[string]json.RawMessage{top.Rows, top.Cases} {
		for _, item := range group {
			for name := range item {
				if !seen[name] {
					seen[name] = true
					names = append(names, name)
				}
			}
		}
	}
	require.NotEmpty(t, names)
	return names
}

// ---------------------------------------------------------------------------
// LoadRangePriceFromEnv
// ---------------------------------------------------------------------------

// Both variables absent is a real deployment choice — it is stock behaviour —
// so it is the one way of arriving at a zero tariff that is not an error.
func TestLoadRangePriceFromEnv_TreatsBothAbsentAsTheFeatureBeingOff(t *testing.T) {
	unsetEnv(t, EnvBlocksPerUnit)
	unsetEnv(t, EnvCreditsPerUnit)

	got, err := LoadRangePriceFromEnv()

	require.NoError(t, err, "an unset range tariff is today's behaviour, not a misconfiguration")
	assert.Equal(t, RangePrice{}, got)
	assert.False(t, got.Enabled())
}

func TestLoadRangePriceFromEnv_ReadsAValidTariff(t *testing.T) {
	t.Setenv(EnvBlocksPerUnit, "1000")
	t.Setenv(EnvCreditsPerUnit, "5")

	got, err := LoadRangePriceFromEnv()

	require.NoError(t, err)
	assert.Equal(t, RangePrice{BlocksPerUnit: 1000, CreditsPerUnit: 5}, got)
	assert.True(t, got.Enabled())
}

// Padding is a deployment quirk, not a different number. This pins that the
// loader trims, so a value from a templated YAML file still loads.
func TestLoadRangePriceFromEnv_TrimsPaddingAroundAValue(t *testing.T) {
	t.Setenv(EnvBlocksPerUnit, "  1000\n")
	t.Setenv(EnvCreditsPerUnit, "\t5 ")

	got, err := LoadRangePriceFromEnv()

	require.NoError(t, err)
	assert.Equal(t, RangePrice{BlocksPerUnit: 1000, CreditsPerUnit: 5}, got)
}

// Every other route to a zero tariff is an error.
//
// Half a tariff is the dangerous one: the half that is missing reads as zero,
// Enabled goes false, and the deployment believes it is charging for spans
// while it charges for none. The rest mirror the tier-limit loader — an empty
// string is not a zero, and a zero is not an off switch.
func TestLoadRangePriceFromEnv_RefusesEveryOtherRouteToAZeroTariff(t *testing.T) {
	const unset = "\x00unset\x00"

	for _, tc := range []struct {
		name    string
		blocks  string
		credits string
		want    string
		why     string
	}{
		{
			name: "blocks alone", blocks: "1000", credits: unset, want: "must be set together",
			why: "naming one leaves the other at zero and the charge silently off",
		},
		{
			name: "credits alone", blocks: unset, credits: "5", want: "must be set together",
			why: "the same hazard from the other side",
		},
		{
			name: "blocks is zero", blocks: "0", credits: "5", want: "greater than zero",
			why: "zero blocks per unit would divide by zero if it ever reached the arithmetic",
		},
		{
			name: "credits is zero", blocks: "1000", credits: "0", want: "greater than zero",
			why: "a zero unit price is the feature off, and off is spelled by unsetting both",
		},
		{
			name: "blocks is negative", blocks: "-1000", credits: "5", want: "greater than zero",
			why: "a negative divisor flips the sign of the charge",
		},
		{
			name: "credits is negative", blocks: "1000", credits: "-5", want: "greater than zero",
			why: "a negative price credits the account instead of billing it",
		},
		{
			name: "blocks is empty", blocks: "", credits: "5", want: "is empty",
			why: "a template that expanded to nothing must not read as a default",
		},
		{
			name: "credits is empty", blocks: "1000", credits: "", want: "is empty",
			why: "the same from the other side",
		},
		{
			name: "both are empty", blocks: "", credits: "", want: "is empty",
			why: "two empty strings are two mistakes, not the off switch",
		},
		{
			name: "blocks is whitespace", blocks: "   ", credits: "5", want: "is empty",
			why: "whitespace is how an empty value survives a shell",
		},
		{
			name: "credits is not a number", blocks: "1000", credits: "five", want: "not a whole number",
			why: "a word must fail at boot, not at the first request",
		},
		{
			name: "blocks is a float", blocks: "1000.0", credits: "5", want: "not a whole number",
			why: "blocks are whole; a float here is a units mistake somewhere upstream",
		},
		{
			name: "blocks is in exponent form", blocks: "1e3", credits: "5", want: "not a whole number",
			why: "1e3 is a thousand to a human and a parse error to strconv; refusing it is the honest answer",
		},
		{
			name: "credits has a trailing unit", blocks: "1000", credits: "5credits", want: "not a whole number",
			why: "a partially numeric value must not read as its numeric prefix",
		},
		{
			name: "blocks overflows an int64", blocks: "99999999999999999999", credits: "5", want: "not a whole number",
			why: "ParseInt refuses it rather than saturating at MaxInt64",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setRangeEnv(t, EnvBlocksPerUnit, tc.blocks, unset)
			setRangeEnv(t, EnvCreditsPerUnit, tc.credits, unset)

			got, err := LoadRangePriceFromEnv()

			require.Error(t, err, tc.why)
			assert.Contains(t, err.Error(), tc.want, tc.why)
			assert.Equal(t, RangePrice{}, got, "a failed load returns no tariff")
		})
	}
}

// setRangeEnv sets one variable, or really removes it when the case asks for
// an unset variable.
func setRangeEnv(t *testing.T, name, value, unsetSentinel string) {
	t.Helper()
	if value == unsetSentinel {
		unsetEnv(t, name)
		return
	}
	t.Setenv(name, value)
}
