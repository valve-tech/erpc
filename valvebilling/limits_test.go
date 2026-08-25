package valvebilling

import (
	"context"
	"math/big"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Characterization tests for the arithmetic limits of the credit rail.
//
// These tests describe what the SHARED authorize.lua and Redis actually do at
// the edges of their number ranges. Several of them assert behaviour that
// looks wrong. That is on purpose: the script is byte-identical to the
// TypeScript relay's copy and both callers must decide alike, so a limit
// recorded here is a limit to design around, never a bug to patch in Go. Each
// test says which of the two it is.
//
// The peg throughout is 10^9 credits per USD, so one credit is $0.000000001.
//
// Every boundary below was also run against a real redis-server 7.2.4 on
// 2026-08-24 and agreed with miniredis, with one exception that is called out
// where it bites: a cost above int64. See TestOverflow_ATokenWeiCostNeverBills.

// creditVerdict runs one authorization with only the credit gate armed:
// baseInput leaves every rate limit at zero, so no other gate can answer.
// Values are decimal strings because the interesting ones exceed a float and
// some exceed an int64.
func creditVerdict(t *testing.T, ceiling, pending, spend, cost string) string {
	t.Helper()
	mr, rdb := newTestRedis(t)
	require.NoError(t, mr.Set(ceilingKey("acct_1"), ceiling))
	require.NoError(t, mr.Set(pendingKey("acct_1"), pending))
	require.NoError(t, mr.Set(spendKey("acct_1"), spend))

	in := baseInput()
	in.Cost = mustInt(t, cost)
	v, err := Authorize(context.Background(), rdb, in)
	require.NoError(t, err)
	return v.Code
}

// exactVerdict is the answer the script would give if Lua had integers. It is
// the same comparison — effective - cost < 0 — done in big.Int.
func exactVerdict(t *testing.T, ceiling, pending, spend, cost string) string {
	t.Helper()
	eff := new(big.Int).Add(mustInt(t, ceiling), mustInt(t, pending))
	eff.Sub(eff, mustInt(t, spend))
	if eff.Sub(eff, mustInt(t, cost)).Sign() < 0 {
		return "no_credits"
	}
	return "ok"
}

func mustInt(t *testing.T, s string) *big.Int {
	t.Helper()
	n, ok := new(big.Int).SetString(s, 10)
	require.True(t, ok, "not a decimal integer: %s", s)
	return n
}

func dec(n int64) string { return new(big.Int).SetInt64(n).String() }

// A balance one credit short refuses a one-credit charge at EVERY magnitude,
// including magnitudes where a double cannot count in ones.
//
// This is the surprise, and it is worth stating before the tests that do show
// error. Rounding a decimal string to a double is MONOTONE. When ceiling and
// spend carry the same value they round to the same double, so their
// difference is exactly zero however large they are, and a positive cost is
// refused. The precision limit needs the two operands to round in opposite
// directions; equal operands never do.
//
// This pins a REQUIREMENT, not a limit: an exhausted account must be refused,
// and it is, all the way to 2^62 credits (about $4.6 billion).
func TestPrecision_AnExhaustedBalanceRefusesAOneCreditChargeAtEveryMagnitude(t *testing.T) {
	for k := 40; k <= 62; k++ {
		m := dec(int64(1) << uint(k))
		got := creditVerdict(t, m, "0", m, "1")
		t.Logf("2^%d ceiling=%s spend=%s cost=1 -> %s", k, m, m, got)
		assert.Equal(t, "no_credits", got,
			"an exhausted account was allowed to spend at 2^%d", k)
	}
}

// A balance of exactly one credit vanishes at 2^54, and the customer is
// refused.
//
// ceiling = 2^k, spend = 2^k - 1, so one credit is left and a one-credit
// charge must pass. At and above 2^54 the double nearest 2^k - 1 is 2^k
// itself, the computed balance is zero, and the charge is refused.
//
// 2^54 = 18014398509481984 credits = $18,014,398.51. That is the real ceiling
// on exact single-credit accounting, and the error at it runs in the
// customer's disfavour — we refuse work they had paid for, we do not give
// work away.
//
// This test pins a LIMIT, not a requirement. Do not "fix" it here. The
// TypeScript relay runs the same script and must keep answering the same way.
func TestPrecision_AOneCreditBalanceDisappearsAtTwoToThe54(t *testing.T) {
	firstWrong := 0
	for k := 40; k <= 62; k++ {
		m := int64(1) << uint(k)
		got := creditVerdict(t, dec(m), "0", dec(m-1), "1")
		want := exactVerdict(t, dec(m), "0", dec(m-1), "1")
		t.Logf("2^%d ceiling=%d spend=%d cost=1 -> script %s, exact %s", k, m, m-1, got, want)
		if got != want && firstWrong == 0 {
			firstWrong = k
		}
	}
	assert.Equal(t, 54, firstWrong,
		"the magnitude at which a single credit stops being visible moved")

	// Stated as literals as well, so the boundary is greppable rather than
	// only computed.
	assert.Equal(t, "ok", creditVerdict(t, "9007199254740992", "0", "9007199254740991", "1"),
		"at 2^53 the last credit must still spend")
	assert.Equal(t, "no_credits", creditVerdict(t, "18014398509481984", "0", "18014398509481983", "1"),
		"at 2^54 the last credit is invisible; that is the recorded limit")
}

// With a pending top-up in the sum, a one-credit charge on an exhausted
// account IS wrongly allowed, from 2^53 up.
//
// The script computes (ceiling + pending) - spend. The intermediate sum is
// rounded before the subtraction, so ceiling and spend no longer round alike
// and the two errors can point in opposite directions. The values below are
// searched, not invented: each is the first triple at that magnitude where the
// account owes exactly its whole balance and the script still says yes.
//
// 2^53 = 9007199254740992 credits = $9,007,199.25. Above that balance an
// account can overdraw, and pending — an unsettled top-up — is what opens the
// door. The amount is one credit here, $0.000000001.
//
// This test pins a LIMIT. It is the honest boundary of the current design and
// the number the drain proposal has to beat.
func TestPrecision_APendingTopUpHidesAOneCreditChargeFromTwoToThe53(t *testing.T) {
	// ceiling, pending, spend. spend == ceiling+pending in every row, so the
	// true balance is zero and a cost of 1 must be refused.
	rows := []struct {
		magnitude               int
		ceiling, pending, spend string
	}{
		{53, "9007199254740995", "2", "9007199254740997"},
		{54, "18014398509481979", "2", "18014398509481981"},
		{57, "144115188075855817", "8", "144115188075855825"},
		{62, "4611686018427386113", "256", "4611686018427386369"},
	}
	for _, r := range rows {
		require.Equal(t, "no_credits", exactVerdict(t, r.ceiling, r.pending, r.spend, "1"),
			"row %d is not an exhausted account; the fixture is wrong", r.magnitude)
		got := creditVerdict(t, r.ceiling, r.pending, r.spend, "1")
		t.Logf("2^%d ceiling=%s pending=%s spend=%s cost=1 -> script %s, exact no_credits",
			r.magnitude, r.ceiling, r.pending, r.spend, got)
		assert.Equal(t, "ok", got,
			"the pending-top-up rounding gap closed at 2^%d; confirm the TypeScript relay changed with it",
			r.magnitude)
	}

	// Below 2^53 the same shape is exact, which is what makes 2^53 the
	// boundary rather than an arbitrary sample.
	assert.Equal(t, "no_credits", creditVerdict(t, "4503599627370499", "2", "4503599627370501", "1"),
		"at 2^52 every credit is still countable")
}

// The operator's question: does draining the counters fix the precision
// problem?
//
// Answer, measured: draining SPEND alone does not. The rounding error scales
// with the LARGER operand, and that is ceiling. What draining does is keep the
// account far from zero, where the error cannot change a verdict — and that
// only holds while the ceiling itself is not being drawn down.
//
// The three shapes below are at the live magnitude, ~10^17 credits ($100M),
// using the real largest live ceiling.
//
// This test pins a LIMIT and the shape of its fix. Nothing here is a defect to
// patch in Go.
func TestDrain_PrecisionFollowsTheCeilingNotTheSpend(t *testing.T) {
	const liveCeiling = "99999680453646021" // ~$99,999,680.45

	t.Run("large ceiling and a drained spend: verdict right, arithmetic not", func(t *testing.T) {
		// A drained account: one hour of unsettled spend against a lifetime
		// ceiling. The verdict is right because the balance is nowhere near
		// zero, not because the subtraction is exact.
		assert.Equal(t, "ok", creditVerdict(t, liveCeiling, "0", "500", "1"))

		// The same read, shown to be inexact: the ceiling alone loses 5
		// credits on the way into Lua. A balance report built from this
		// number is wrong by that much, silently.
		c := mustInt(t, liveCeiling)
		f, _ := new(big.Float).SetInt(c).Float64()
		drift := new(big.Int).Sub(big.NewInt(int64(f)), c)
		t.Logf("ceiling %s reads into Lua as %.0f, drift %+d credits", liveCeiling, f, drift)
		assert.Equal(t, int64(-5), drift.Int64(),
			"the live ceiling's rounding drift changed; re-measure the overspend bound below")
	})

	t.Run("spend near ceiling: the verdict is wrong by up to 3 credits", func(t *testing.T) {
		// The same account near exhaustion. The ceiling rounds DOWN by 5 and a
		// spend can round DOWN by up to 8, so the script can see up to 3
		// credits that do not exist. Three credits is $0.000000003 — the whole
		// overdraft an account at this ceiling can reach through rounding.
		const spend = "99999680453645896" // ceiling - 125
		require.Equal(t, "no_credits", exactVerdict(t, liveCeiling, "0", spend, "128"),
			"the fixture must be a genuine 3-credit overdraw")
		got := creditVerdict(t, liveCeiling, "0", spend, "128")
		t.Logf("near exhaustion: ceiling=%s spend=%s cost=128 -> script %s, exact no_credits",
			liveCeiling, spend, got)
		assert.Equal(t, "ok", got,
			"the overspend window at the live ceiling closed; re-measure it before relying on that")

		// Four credits is past the window, so the error is bounded, not open.
		assert.Equal(t, "no_credits", creditVerdict(t, liveCeiling, "0", spend, "129"),
			"the overspend window is wider than the measured 3 credits")
	})

	t.Run("drained ceiling and drained spend: exact", func(t *testing.T) {
		// The model the operator proposes, done fully: Redis holds the
		// REMAINING balance as the ceiling and the unsettled delta as the
		// spend. Both operands are small, so every credit is countable.
		for _, cost := range []string{"1", "2", "50"} {
			assert.Equal(t, exactVerdict(t, "1000", "0", "1000", cost),
				creditVerdict(t, "1000", "0", "1000", cost),
				"exhausted at a drained magnitude, cost %s", cost)
			assert.Equal(t, exactVerdict(t, "1000", "0", "999", cost),
				creditVerdict(t, "1000", "0", "999", cost),
				"one credit left at a drained magnitude, cost %s", cost)
		}
		// And the pending shape that breaks at 2^53 is exact here too.
		assert.Equal(t, "no_credits", creditVerdict(t, "998", "2", "1000", "1"))
	})
}

// What the STORE does at its own limit. INCRBY is int64, and Redis refuses to
// cross it rather than wrapping.
//
// This pins a REQUIREMENT. A wrapping counter would hand an exhausted account
// an unlimited balance, so the error is the behaviour to keep.
func TestStore_IncrByRefusesToCrossInt64MaxAndCaptureSurfacesIt(t *testing.T) {
	const int64Max = "9223372036854775807" // $9,223,372,036.85

	t.Run("INCRBY at the limit errors and leaves the counter alone", func(t *testing.T) {
		mr, rdb := newTestRedis(t)
		require.NoError(t, mr.Set(spendKey("acct_1"), int64Max))

		err := Capture(context.Background(), rdb, "acct_1", big.NewInt(1))
		require.Error(t, err)
		t.Logf("capture at int64 max: %v", err)
		assert.ErrorContains(t, err, "capture failed for account")
		assert.ErrorContains(t, err, "overflow")

		v, err := mr.Get(spendKey("acct_1"))
		require.NoError(t, err)
		assert.Equal(t, int64Max, v, "the counter must not wrap")
	})

	t.Run("a cost past int64 is refused before Redis sees it", func(t *testing.T) {
		mr, rdb := newTestRedis(t)
		cost := new(big.Int).Lsh(big.NewInt(1), 63) // int64 max + 1

		err := Capture(context.Background(), rdb, "acct_2", cost)
		require.Error(t, err)
		t.Logf("capture past int64: %v", err)
		assert.ErrorContains(t, err, "exceeds INCRBY's range")
		assert.False(t, mr.Exists(spendKey("acct_2")),
			"the guard must refuse before it writes anything")
	})

	// And what the Lua sees when a counter sits near int64 max: at that
	// magnitude a double counts in steps of 1024, so a shortfall of up to 511
	// credits is invisible and the charge passes. 512 is caught.
	//
	// This part pins a LIMIT. It is unreachable in practice — no account holds
	// $9.2 billion of credits — and it is recorded so the int64 ceiling and the
	// double ceiling are not confused for each other. The store stays exact all
	// the way to 9223372036854775807; the COMPARISON stops being exact at 2^53,
	// three orders of magnitude earlier.
	t.Run("a counter near int64 max rounds in steps of 1024 inside Lua", func(t *testing.T) {
		for _, tc := range []struct {
			short int64
			want  string
		}{
			{511, "ok"},         // invisible: both operands round to 2^63
			{512, "no_credits"}, // one credit more, and the shortfall reappears
		} {
			ceiling := new(big.Int).Sub(mustInt(t, int64Max), big.NewInt(tc.short)).String()
			require.Equal(t, "no_credits", exactVerdict(t, ceiling, "0", "0", int64Max),
				"the fixture must be a genuine shortfall")
			got := creditVerdict(t, ceiling, "0", "0", int64Max)
			t.Logf("ceiling=%s (int64max-%d) cost=%s -> script %s, exact no_credits",
				ceiling, tc.short, int64Max, got)
			assert.Equal(t, tc.want, got,
				"the granularity near int64 max moved off 1024 credits")
		}
	})
}

// A token-wei cost never bills. The two Lua engines disagree about HOW it
// fails, and that difference is the point of this test.
//
// 10^24 wei is an ordinary ERC-20 amount. Real Redis parses a decimal string
// with strtod, so tonumber gives 1e+24 and the credit gate answers no_credits.
// Measured directly against redis-server 7.2.4 on 2026-08-24: the script
// returns {no_credits, NONE} for this exact input.
//
// miniredis runs gopher-lua, whose tonumber parses an integer-shaped string
// with ParseInt only and returns NIL when it overflows int64. The script then
// hits `effective - cost` with a nil operand and fails to compile-time-checked
// arithmetic, so Authorize returns an ERROR rather than a verdict.
//
// What holds on BOTH engines is the assertion that matters: the request does
// not proceed. What differs is the caller's exposure — a fail-open caller
// treats the error as a Redis outage and could let the request through. So
// this pins a REQUIREMENT (never bill wei on the credits rail) and records a
// HARNESS LIMIT (miniredis cannot show the real refusal path above int64).
// Do not weaken cost.go's strict decode on the strength of either behaviour;
// that decode is the guard that keeps this input from ever being built.
func TestOverflow_ATokenWeiCostNeverBills(t *testing.T) {
	const wei = "1000000000000000000000000" // 10^24, one token at 18 decimals
	const int64Max = "9223372036854775807"

	mr, rdb := newTestRedis(t)
	require.NoError(t, mr.Set(ceilingKey("acct_1"), int64Max))
	in := baseInput()
	in.Cost = mustInt(t, wei)

	v, err := Authorize(context.Background(), rdb, in)
	assert.False(t, v.OK(), "a wei-sized cost must never be authorized")
	require.Error(t, err, "miniredis's tonumber returns nil above int64; real Redis returns no_credits")
	t.Logf("miniredis: cost=%s -> %v", wei, err)
	assert.ErrorContains(t, err, "authorize script failed")

	// Capture refuses it too, so neither half of the authorize/capture split
	// can be tricked into moving that much.
	err = Capture(context.Background(), rdb, "acct_1", mustInt(t, wei))
	assert.ErrorContains(t, err, "exceeds INCRBY's range")

	// One credit below int64 max is the largest cost the shared script can
	// still read as a number on both engines.
	assert.Equal(t, "no_credits", creditVerdict(t, "1000", "0", "0", int64Max),
		"a cost at int64 max must read as a number and lose to a small balance")
}

// The drain implementation detail: SET 0 loses concurrent charges, DECRBY does
// not.
//
// A drain reads spend, settles that amount downstream, then clears what it
// settled. Between the read and the write, Capture keeps running — the relay
// does not stop for the drain. SET 0 throws away whatever arrived in that
// window; DECRBY of the exact amount read leaves it in place.
//
// This pins a REQUIREMENT for code that does not exist yet. The number below
// is the money at stake per lost window.
func TestDrain_SetZeroDiscardsAConcurrentChargeAndDecrByKeepsIt(t *testing.T) {
	const settled = 1000 // what the drain read and settled
	const concurrent = 250

	t.Run("SET 0 discards it", func(t *testing.T) {
		mr, rdb := newTestRedis(t)
		require.NoError(t, mr.Set(spendKey("acct_1"), dec(settled)))

		// The drain reads 1000 and starts settling.
		read, err := rdb.Get(context.Background(), spendKey("acct_1")).Int64()
		require.NoError(t, err)
		require.EqualValues(t, settled, read)

		// A request completes and captures while the drain is in flight.
		require.NoError(t, Capture(context.Background(), rdb, "acct_1", big.NewInt(concurrent)))

		// The drain finishes and clears the counter.
		require.NoError(t, rdb.Set(context.Background(), spendKey("acct_1"), "0", 0).Err())

		after, err := rdb.Get(context.Background(), spendKey("acct_1")).Int64()
		require.NoError(t, err)
		assert.EqualValues(t, 0, after)
		t.Logf("SET 0 drain: settled %d, captured %d concurrently, counter now %d, LOST %d credits ($%.9f)",
			settled, concurrent, after, concurrent, float64(concurrent)/1e9)
	})

	t.Run("DECRBY of the amount read keeps it", func(t *testing.T) {
		mr, rdb := newTestRedis(t)
		require.NoError(t, mr.Set(spendKey("acct_1"), dec(settled)))

		read, err := rdb.Get(context.Background(), spendKey("acct_1")).Int64()
		require.NoError(t, err)

		require.NoError(t, Capture(context.Background(), rdb, "acct_1", big.NewInt(concurrent)))

		require.NoError(t, rdb.DecrBy(context.Background(), spendKey("acct_1"), read).Err())

		after, err := rdb.Get(context.Background(), spendKey("acct_1")).Int64()
		require.NoError(t, err)
		assert.EqualValues(t, concurrent, after,
			"DECRBY must leave exactly the charges that arrived during the drain")
		t.Logf("DECRBY %d drain: settled %d, captured %d concurrently, counter now %d, lost 0 credits",
			read, settled, concurrent, after)
	})
}

// The boundaries around zero: an overdrawn account, a zero cost, and an
// account with no credit keys at all.
//
// Two of these pin REQUIREMENTS and one pins a LIMIT; the comments say which.
func TestLimit_ZeroCostAnOverdraftAndAnAccountWithNoKeys(t *testing.T) {
	// REQUIREMENT. An overdrawn account is refused, and the script does not
	// care how far under it is.
	t.Run("an overdrawn account is refused", func(t *testing.T) {
		assert.Equal(t, "no_credits", creditVerdict(t, "1000", "0", "1500", "1"))
		assert.Equal(t, "no_credits", creditVerdict(t, "1000", "0", "1500", "0"))
	})

	// LIMIT, and a deliberate one. A zero-cost request still runs the credit
	// gate, so an overdrawn account cannot even make free calls. That is the
	// row above. With credit to spare, a zero cost passes and moves no
	// counter — the script's `cost > 0` guard keeps the shared per-second
	// bucket still, so a free method cannot throttle the other keys on the
	// account.
	t.Run("a zero cost passes on a funded account and moves nothing", func(t *testing.T) {
		mr, rdb := newTestRedis(t)
		require.NoError(t, mr.Set(ceilingKey("acct_1"), "1000"))

		in := baseInput()
		in.Cost = big.NewInt(0)
		in.Limits.FullCPS = 100

		v, err := Authorize(context.Background(), rdb, in)
		require.NoError(t, err)
		assert.Equal(t, CodeOK, v.Code)
		assert.False(t, mr.Exists(cpsBucketKey("acct_1")),
			"a zero-cost call moved the shared per-second bucket")
	})

	// REQUIREMENT. A brand-new account has no ceiling key. tonumber(false) is
	// nil, the script's `or 0` catches it, and the balance is zero: the
	// account is refused rather than treated as unlimited.
	t.Run("an account with no keys has a zero balance", func(t *testing.T) {
		_, rdb := newTestRedis(t)

		in := baseInput()
		v, err := Authorize(context.Background(), rdb, in)
		require.NoError(t, err)
		assert.Equal(t, "no_credits", v.Code,
			"a missing ceiling key must read as no credit, never as no limit")

		in.Cost = big.NewInt(0)
		v, err = Authorize(context.Background(), rdb, in)
		require.NoError(t, err)
		assert.Equal(t, CodeOK, v.Code,
			"a zero cost needs no credit, so a new account is not blocked by the gate")
	})
}
