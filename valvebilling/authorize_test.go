package valvebilling

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"math/big"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The single most important test in this file.
//
// go-redis and ioredis both key EVALSHA on the SHA1 of the script body, so an
// identical copy means this process and the TypeScript relay share ONE cached
// script inside Redis. A drifted copy does not fail loudly: it loads a second
// script that runs against the same counters and can decide differently. This
// pins the copy to the digest the monorepo published.
//
// If this fails, do not update the constant. Re-copy authorize.lua from the
// monorepo verbatim and find out what changed there.
func TestAuthorizeScript_MatchesTheMonorepoDigest(t *testing.T) {
	sum := sha1.Sum([]byte(authorizeLua))
	assert.Equal(t, AuthorizeScriptSHA1, hex.EncodeToString(sum[:]),
		"authorize.lua has drifted from the monorepo copy; re-copy it rather than editing this constant")

	// Guard against the copy being emptied or truncated to something that
	// still hashes to a constant somebody updated.
	assert.Greater(t, len(authorizeLua), 3000, "the embedded script looks truncated")
	assert.Contains(t, authorizeLua, "no_credits")
	assert.Contains(t, authorizeLua, "cps_throttle")
}

func newTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

func baseInput() AuthorizeInput {
	return AuthorizeInput{
		AccountID: "acct_1",
		// 32 hex characters, the shape HashAPIKey emits, made of one repeated
		// character on purpose. The obvious synthetic value — the hex alphabet
		// written twice — scores entropy 4.0 and secret scanners read it as a
		// credential. A repeated character carries the same shape at near-zero
		// entropy, so the fixture is unmistakably a fixture to a tool as well
		// as to a reader.
		KeyID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		// A fixed instant, so the second and day buckets are deterministic.
		Now:    time.Unix(1_700_000_000, 0).UTC(),
		Cost:   big.NewInt(100),
		CUCost: 5,
		Limits: Limits{
			DayLimit: 0, CUSecondLimit: 0, CUDayLimit: 0,
			SlowThreshold: 0,
			FullCPS:       0, SlowCPS: 0,
			FullRPS: 0, SlowRPS: 0, KeyRPS: 0,
		},
	}
}

// A funded account passes every gate. This also proves the ten keys and
// twelve arguments are in the order the script expects — a misordered ARGV
// would not error, it would compare the wrong numbers.
func TestAuthorize_AllowsAFundedAccount(t *testing.T) {
	mr, rdb := newTestRedis(t)
	require.NoError(t, mr.Set(ceilingKey("acct_1"), "1000"))

	v, err := Authorize(context.Background(), rdb, baseInput())
	require.NoError(t, err)
	assert.True(t, v.OK(), "got code %q", v.Code)
	assert.Equal(t, TierFull, v.Tier)
}

// The balance is ceiling + pending - spend. Dropping `pending` is the exact
// mutation that survived the monorepo's entire 740-test suite: it would give
// every freshly topped-up account an instant no_credits, with nothing red.
func TestAuthorize_CountsPendingTowardsTheBalance(t *testing.T) {
	mr, rdb := newTestRedis(t)
	// Nothing settled yet; the whole balance is a pending top-up.
	require.NoError(t, mr.Set(ceilingKey("acct_1"), "0"))
	require.NoError(t, mr.Set(pendingKey("acct_1"), "1000"))

	v, err := Authorize(context.Background(), rdb, baseInput())
	require.NoError(t, err)
	assert.True(t, v.OK(),
		"a freshly topped-up account was refused with %q; pending is not counting toward the balance", v.Code)
}

func TestAuthorize_RefusesAnAccountWithoutCredits(t *testing.T) {
	mr, rdb := newTestRedis(t)
	require.NoError(t, mr.Set(ceilingKey("acct_1"), "10"))

	in := baseInput()
	in.Cost = big.NewInt(100)

	v, err := Authorize(context.Background(), rdb, in)
	require.NoError(t, err)
	assert.False(t, v.OK())
	assert.Equal(t, "no_credits", v.Code)
	assert.Equal(t, TierNone, v.Tier)
}

// Authorize decides; capture charges. If authorize moved spend, a failed
// upstream would have cost the customer money with no refund path.
func TestAuthorize_NeverMovesSpend(t *testing.T) {
	mr, rdb := newTestRedis(t)
	require.NoError(t, mr.Set(ceilingKey("acct_1"), "1000"))

	v, err := Authorize(context.Background(), rdb, baseInput())
	require.NoError(t, err)
	require.True(t, v.OK())

	assert.False(t, mr.Exists(spendKey("acct_1")),
		"authorize moved spend; the authorize/capture split is what makes a failed upstream free")
}

// A rejection must leave every bucket untouched. The script evaluates all
// gates before it commits anything, precisely so a late rejection does not
// burn day quota, CU quota and the CPS bucket the way the old sequence did.
func TestAuthorize_ARejectionBurnsNothing(t *testing.T) {
	mr, rdb := newTestRedis(t)
	require.NoError(t, mr.Set(closingKey("acct_1"), "1"))
	require.NoError(t, mr.Set(ceilingKey("acct_1"), "1000000"))

	v, err := Authorize(context.Background(), rdb, baseInput())
	require.NoError(t, err)
	require.Equal(t, "closing", v.Code)

	for _, k := range []string{
		spendKey("acct_1"),
		cpsBucketKey("acct_1"),
		"valve:rate:d:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:19675",
		"valve:rate:s:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:1700000000",
	} {
		assert.False(t, mr.Exists(k), "a rejected request moved %s", k)
	}
}

// The Mode-1 lock is the first gate. A per-request payment in flight must stop
// the credits path from spending the same account underneath it.
func TestAuthorize_RefusesWhileAPerRequestLockIsHeld(t *testing.T) {
	mr, rdb := newTestRedis(t)
	require.NoError(t, mr.Set(ceilingKey("acct_1"), "1000000"))
	require.NoError(t, mr.Set(perRequestLockKey("acct_1"), "1"))

	v, err := Authorize(context.Background(), rdb, baseInput())
	require.NoError(t, err)
	assert.Equal(t, "per_request_lock", v.Code)
}

// Costs are carried as decimal strings, and above 2^53 the SHARED script — not
// this package — stops comparing them exactly.
//
// authorize.lua does tonumber(ARGV[1]), and Lua numbers are IEEE-754 doubles,
// so integers above 2^53 round to the nearest representable value. Measured
// against the embedded script: below 2^53 a difference of 1 is caught; at 2^53
// a difference of 1 is invisible and 2 is caught; at 2^54 the granularity is 4.
// That is the double ULP.
//
// This is NOT a Go defect and must not be "fixed" here. The TypeScript relay
// passes String(bigint) to the same script and gets the same rounding, and the
// brief requires byte-identical outcomes. Diverging — by pre-comparing in Go,
// say — would make the two implementations disagree, which is the failure this
// module exists to avoid. The limit is recorded so nobody re-discovers it as a
// mystery, and it is reported upstream as a property of the shared script.
//
// What this test DOES pin is that Go hands over the full decimal. A Go-side
// float would lose precision far earlier, and this would catch that.
func TestAuthorize_CarriesTheFullDecimalAndSharesTheScriptsPrecisionLimit(t *testing.T) {
	run := func(t *testing.T, ceiling, cost string) string {
		t.Helper()
		mr, rdb := newTestRedis(t)
		require.NoError(t, mr.Set(ceilingKey("acct_1"), ceiling))
		in := baseInput()
		var ok bool
		in.Cost, ok = new(big.Int).SetString(cost, 10)
		require.True(t, ok)
		v, err := Authorize(context.Background(), rdb, in)
		require.NoError(t, err)
		return v.Code
	}

	// Well below 2^53: exact. A float64 on the Go side would already be fine
	// here, so this alone proves little — it is the control.
	assert.Equal(t, "no_credits", run(t, "1000000000000000", "1000000000000001"),
		"a one-unit shortfall below 2^53 must be caught")

	// At 2^53 the script cannot see a difference of 1. Asserted so a change in
	// Redis or in the script that makes it exact shows up as a failure to read
	// rather than a silent behaviour change.
	assert.Equal(t, "ok", run(t, "9007199254740992", "9007199254740993"),
		"the shared script's precision limit moved; confirm the TypeScript relay moved with it")

	// ...but a difference of 2 at that magnitude is still caught, which is what
	// shows the value reached Redis as a full decimal rather than as something
	// Go had already rounded.
	assert.Equal(t, "no_credits", run(t, "9007199254740992", "9007199254740994"),
		"a two-unit shortfall at 2^53 was missed; the cost is being rounded before it reaches Redis")
}

// A Redis failure is not a verdict. Returning a rejection here would make an
// unreachable Redis indistinguishable from an out-of-credit customer.
func TestAuthorize_ReportsARedisFailureAsAnError(t *testing.T) {
	mr, rdb := newTestRedis(t)
	mr.Close()

	v, err := Authorize(context.Background(), rdb, baseInput())
	require.Error(t, err)
	assert.False(t, v.OK())
	assert.Empty(t, v.Code, "a transport failure must not be dressed up as a rejection code")
}

func TestAuthorize_RejectsIncompleteInput(t *testing.T) {
	_, rdb := newTestRedis(t)
	ctx := context.Background()

	in := baseInput()
	in.Cost = nil
	_, err := Authorize(ctx, rdb, in)
	assert.ErrorContains(t, err, "cost is nil")

	in = baseInput()
	in.AccountID = ""
	_, err = Authorize(ctx, rdb, in)
	assert.ErrorContains(t, err, "required")
}

// Why the credits-per-second limit may never be zero.
//
// Authorize reads the balance and never reserves it, and it never moves spend
// — capture does that after the upstream answers. So every request in flight
// on one account sees the same balance and passes the credit gate. The
// credits-per-second bucket is the only thing that stops them, and
// authorize.lua skips that bucket entirely when the limit is zero.
//
// This is the trap valvebilling.LoadTierLimitsFromEnv refuses to reproduce:
// FULL_CREDITS_PER_SEC=0 parses as a real zero in the TypeScript relay, which
// reads as "throttling off" and means "overdraft protection off".
func TestAuthorize_TheCreditsPerSecondBucketIsTheOnlyOverdraftBound(t *testing.T) {
	// A balance of exactly one request. Every further request overdraws.
	const balance = "100"

	t.Run("a zero limit leaves the overdraft unbounded", func(t *testing.T) {
		mr, rdb := newTestRedis(t)
		require.NoError(t, mr.Set(ceilingKey("acct_1"), balance))

		in := baseInput()
		in.Cost = big.NewInt(100)
		in.Limits.FullCPS = 0

		for i := 1; i <= 10; i++ {
			v, err := Authorize(context.Background(), rdb, in)
			require.NoError(t, err)
			require.True(t, v.OK(),
				"request %d was refused with %q; if the script ever bounds a zero limit, "+
					"read LoadTierLimitsFromEnv again before relaxing it", i, v.Code)
		}
	})

	t.Run("a positive limit bounds it", func(t *testing.T) {
		mr, rdb := newTestRedis(t)
		require.NoError(t, mr.Set(ceilingKey("acct_1"), balance))

		in := baseInput()
		in.Cost = big.NewInt(100)
		in.Limits.FullCPS = 250

		for i := 1; i <= 2; i++ {
			v, err := Authorize(context.Background(), rdb, in)
			require.NoError(t, err)
			require.True(t, v.OK(), "request %d was refused with %q", i, v.Code)
		}

		v, err := Authorize(context.Background(), rdb, in)
		require.NoError(t, err)
		assert.False(t, v.OK())
		assert.Equal(t, "cps_throttle", v.Code,
			"the third request overdraws past the per-second budget and must be stopped")
	})
}
