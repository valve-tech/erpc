package valvebilling

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Characterization tests for how far an account can OVERDRAW.
//
// authorize.lua reads balance sufficiency and never reserves it. `spend` does
// not move until Capture runs, after the upstream answers, so every request in
// flight compares itself against the same `effective = ceiling + pending -
// spend`. N concurrent requests therefore all pass the credit gate on the same
// credits. The only thing that stops them is the per-ACCOUNT credits-per-second
// bucket at valve:credits:<accountId>:cps.
//
// Everything below is MEASURED against a real redis-server 7.2.4 with real
// goroutines. miniredis is not used in this file: the bucket's bound is a
// property of a 2-second TTL and of Redis's single-threaded script execution,
// and valve/billing-module.md already records one place where the two engines
// disagree. A bound measured against a fake is not a bound.
//
// Every number here pins a LIMIT, not a requirement. The script is
// byte-identical to the TypeScript relay's copy and both callers must decide
// alike, so nothing in this file is a defect to patch in Go. Where a
// measurement contradicts valve/periodic-enforcement.md, the comment says so
// and the measurement wins.
//
// # Runtime
//
// Measured at about 40 seconds with -race, because three of these tests must
// watch a real 2-second TTL elapse and one of them spans twelve seconds twice.
// `go test -short` skips those and leaves about 5 seconds of deterministic
// work. Nothing here sleeps for a fixed duration: the wall-clock tests poll
// Redis for the state they are waiting on, and the two that must catch a
// window boundary start over when they miss it.

// The deployment defaults, from valve/periodic-enforcement.md's table of the
// relay's code fallbacks (packages/relay/src/meter.ts). They are stated here
// as test fixtures, not as this package's policy — config.go deliberately
// refuses to default them.
const (
	relayFullCPS       int64 = 5000
	relaySlowCPS       int64 = 500
	relaySlowRPS       int64 = 100
	relaySlowThreshold int64 = 5_000_000_000 // $5 at the 10^9 peg
	relayDefaultCU     int64 = 6             // DEFAULT_CU, the common read cost
)

// FULL_RATE_RPS (1,000) gets no fixture here on purpose. At the default
// threshold an account is always on the SLOW tier by the time it can overdraw,
// so the FULL request-rate cap applies to none of the measurements below.

// cpsBucketTTL is the lifetime authorize.lua arms on the cps bucket.
const cpsBucketTTL = 2 * time.Second

const overdraftRedisBinary = "/usr/local/bin/redis-server"

// newOverdraftRedis starts a private redis-server and returns a client for it.
//
// It listens on a unix socket inside a temporary directory rather than on a
// TCP port, so several of these can run at once — in this file, in a sibling
// test file, or in another `go test` process — without racing for a port
// number. Persistence is off: the server holds one account's counters for a
// few seconds and then dies.
func newOverdraftRedis(t *testing.T, poolSize int) *redis.Client {
	t.Helper()
	if _, err := os.Stat(overdraftRedisBinary); err != nil {
		t.Skipf("no redis-server at %s: %v", overdraftRedisBinary, err)
	}
	dir, err := os.MkdirTemp("", "vbod")
	require.NoError(t, err)
	sock := filepath.Join(dir, "r.sock")
	cmd := exec.Command(overdraftRedisBinary,
		"--port", "0",
		"--unixsocket", sock,
		"--save", "",
		"--appendonly", "no",
		"--dir", dir,
	)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		_ = os.RemoveAll(dir)
	})

	rdb := redis.NewClient(&redis.Options{
		Network:  "unix",
		Addr:     sock,
		PoolSize: poolSize,
	})
	t.Cleanup(func() { _ = rdb.Close() })

	deadline := time.Now().Add(15 * time.Second)
	for {
		if err := rdb.Ping(context.Background()).Err(); err == nil {
			return rdb
		}
		if time.Now().After(deadline) {
			t.Fatalf("redis-server at %s did not become ready", sock)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// overdraftInput is one request on an account that is at the edge of its
// balance. Every rate gate is off, so only the credit gate and the cps bucket
// can answer; the tests that want a rate gate arm it themselves.
func overdraftInput(cost int64) AuthorizeInput {
	in := baseInput()
	in.Cost = big.NewInt(cost)
	in.CUCost = 0
	in.Limits = Limits{
		SlowThreshold: relaySlowThreshold,
		FullCPS:       relayFullCPS,
		SlowCPS:       relaySlowCPS,
	}
	return in
}

// seedOverdraftBalance gives the account a ceiling and nothing else. pending and spend
// stay absent, which the script reads as zero, so the effective balance is
// exactly what is passed here and stays there for the whole test.
func seedOverdraftBalance(t *testing.T, rdb *redis.Client, credits int64) {
	t.Helper()
	require.NoError(t, rdb.Set(context.Background(), ceilingKey("acct_1"), dec(credits), 0).Err())
}

// overdraftBurst is what one saturation run measured.
type overdraftBurst struct {
	approved int64
	rejected int64
	credits  int64 // approved credits, the number the account actually spent
	bucket   int64 // the cps bucket's value when the burst ended
	codes    map[string]int64
	elapsed  time.Duration
}

// overdraft is how far past its balance the account got.
func (b overdraftBurst) overdraft(balance int64) int64 { return b.credits - balance }

func (b overdraftBurst) codeSummary() string {
	keys := make([]string, 0, len(b.codes))
	for k := range b.codes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := ""
	for i, k := range keys {
		if i > 0 {
			out += " "
		}
		out += fmt.Sprintf("%s=%d", k, b.codes[k])
	}
	return out
}

// saturate fires workers*perWorker authorizations concurrently and reports
// what got through.
//
// keyIDs lets a caller spread the requests over several API keys on the SAME
// account, which is how the per-key rate gate is told apart from the
// per-account credit bucket. Passing nil uses the input's own key.
func overdraftSaturate(t *testing.T, rdb *redis.Client, in AuthorizeInput, workers, perWorker int, keyIDs []string) overdraftBurst {
	t.Helper()
	ctx := context.Background()
	var approved, rejected int64
	var firstErr atomic.Value
	var mu sync.Mutex
	codes := map[string]int64{}

	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			req := in
			if len(keyIDs) > 0 {
				req.KeyID = keyIDs[w%len(keyIDs)]
			}
			for i := 0; i < perWorker; i++ {
				v, err := Authorize(ctx, rdb, req)
				if err != nil {
					firstErr.CompareAndSwap(nil, err)
					return
				}
				if v.OK() {
					atomic.AddInt64(&approved, 1)
				} else {
					atomic.AddInt64(&rejected, 1)
				}
				mu.Lock()
				codes[v.Code]++
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(start)
	if err, ok := firstErr.Load().(error); ok && err != nil {
		require.NoError(t, err, "authorize failed during the burst")
	}

	bucket, err := rdb.Get(ctx, cpsBucketKey("acct_1")).Int64()
	if err == redis.Nil {
		bucket = 0
	} else {
		require.NoError(t, err)
	}
	cost := in.Cost.Int64()
	return overdraftBurst{
		approved: approved,
		rejected: rejected,
		credits:  approved * cost,
		bucket:   bucket,
		codes:    codes,
		elapsed:  elapsed,
	}
}

// requireOneOverdraftWindow fails the test unless the burst stayed inside a
// single 2-second bucket lifetime.
//
// The check is exact rather than a timing heuristic: the bucket is INCRBY'd by
// the cost of every approval and is never reset except by expiry, so a bucket
// value equal to the credits approved proves no window rolled over underneath
// the burst. A rolled window would leave the bucket holding less.
func requireOneOverdraftWindow(t *testing.T, b overdraftBurst) {
	t.Helper()
	require.Equal(t, b.credits, b.bucket,
		"the burst straddled a bucket expiry (took %v); the single-window numbers below are not valid",
		b.elapsed)
}

// requireCreditGateSilent fails unless every rejection came from the cps
// bucket. It is the guard that keeps these tests honest: if the credit gate
// started refusing, the measured ceiling would be a balance, not the bound
// under test.
func requireCreditGateSilent(t *testing.T, rdb *redis.Client, b overdraftBurst) {
	t.Helper()
	assert.Zero(t, b.codes["no_credits"],
		"the credit gate refused a request; these tests measure the cps bound, not the balance")
	exists, err := rdb.Exists(context.Background(), spendKey("acct_1")).Result()
	require.NoError(t, err)
	assert.EqualValues(t, 0, exists,
		"Authorize moved the spend counter; balance sufficiency is supposed to be read, never reserved")
}

// A funded-to-the-penny account is overdrawn by a whole window of
// credits-per-second, and the credit gate never once says no.
//
// This is the claim in one test. The account holds exactly one request's worth
// of credit. Thousands of concurrent requests all read the same balance, all
// pass, and the only thing that eventually stops them is the cps bucket.
//
// LIMIT. The shared script decides this and the relay must keep answering the
// same way.
func TestOverdraft_ABalanceIsReadAndNeverReserved(t *testing.T) {
	rdb := newOverdraftRedis(t, 64)
	seedOverdraftBalance(t, rdb, relayDefaultCU)

	in := overdraftInput(relayDefaultCU)
	b := overdraftSaturate(t, rdb, in, 64, 40, nil)

	requireOneOverdraftWindow(t, b)
	requireCreditGateSilent(t, rdb, b)

	// floor(500/6) = 83 approvals, 498 credits, against a balance of 6.
	assert.EqualValues(t, 83, b.approved)
	assert.EqualValues(t, 498, b.credits)
	assert.EqualValues(t, 492, b.overdraft(relayDefaultCU))
	t.Logf("balance=%d cost=%d SlowCPS=%d: approved=%d rejected=%d credits=%d OVERDRAFT=%d credits ($%.9f) in %v [%s]",
		relayDefaultCU, relayDefaultCU, relaySlowCPS, b.approved, b.rejected, b.credits,
		b.overdraft(relayDefaultCU), float64(b.overdraft(relayDefaultCU))/1e9, b.elapsed, b.codeSummary())
}

// The per-window ceiling, swept over the credits-per-second limit and the
// request cost.
//
// The measured law is exact and has no term for concurrency, request rate, or
// balance:
//
//	credits approved per 2-second window  =  floor(cpsLimit / cost) × cost
//
// The floor is not a rounding detail. The gate is `cpsCount + cost > cpsLimit`,
// so the last approval that fits must fit WHOLE. A cost that does not divide
// the limit strands up to cost-1 credits of allowance in every window, which
// is why the round numbers in the documents (3,000; 30,000) are never quite
// what a real cost reaches.
//
// LIMIT.
func TestOverdraft_TheWindowCeilingIsFloorOfCpsOverCost(t *testing.T) {
	rdb := newOverdraftRedis(t, 64)
	ctx := context.Background()

	type row struct{ cps, cost int64 }
	rows := []row{
		{100, 1}, {100, 6}, {100, 50},
		{relaySlowCPS, 1}, {relaySlowCPS, relayDefaultCU}, {relaySlowCPS, 50},
		{relayFullCPS, 1}, {relayFullCPS, relayDefaultCU}, {relayFullCPS, 50},
	}
	t.Logf("%8s %6s %10s %10s %12s %12s", "cps", "cost", "approved", "credits", "predicted", "overdraft")
	for _, r := range rows {
		require.NoError(t, rdb.FlushAll(ctx).Err())
		seedOverdraftBalance(t, rdb, r.cost) // exactly one request's worth of credit

		in := overdraftInput(r.cost)
		in.Limits.SlowCPS = r.cps
		// Just enough requests to fill the window and then be refused a few
		// hundred times over. Offering many multiples of the allowance would
		// push the burst past the 2-second expiry under -race, and then the
		// run would measure two windows instead of one.
		perWorker := (int(r.cps/r.cost)+512)/64 + 1
		b := overdraftSaturate(t, rdb, in, 64, perWorker, nil)

		requireOneOverdraftWindow(t, b)
		requireCreditGateSilent(t, rdb, b)

		predicted := (r.cps / r.cost) * r.cost
		assert.Equal(t, predicted, b.credits,
			"cps=%d cost=%d: the per-window ceiling moved off floor(cps/cost)*cost", r.cps, r.cost)
		t.Logf("%8d %6d %10d %10d %12d %12d", r.cps, r.cost, b.approved, b.credits, predicted,
			b.overdraft(r.cost))
	}
}

// Doubling the client's send rate does not move the overdraft.
//
// This is the distinction valve/periodic-enforcement.md rests its pivot on: a
// credits-per-TIME bucket does not care how fast a client sends, whereas a
// cached-projection bound is TTL × request-rate × cost and does. Measured, the
// first half holds exactly. One goroutine and 512 goroutines get the identical
// number of credits out of the same window; the fast client just collects its
// rejections sooner.
//
// LIMIT, and the load-bearing one for the periodic-state proposal.
func TestOverdraft_TheSendRateDoesNotMoveTheBound(t *testing.T) {
	rdb := newOverdraftRedis(t, 512)
	ctx := context.Background()

	const cost = relayDefaultCU
	want := (relaySlowCPS / cost) * cost
	t.Logf("%10s %10s %10s %10s %12s", "workers", "requests", "approved", "credits", "elapsed")
	for _, workers := range []int{1, 8, 64, 256, 512} {
		require.NoError(t, rdb.FlushAll(ctx).Err())
		seedOverdraftBalance(t, rdb, cost)

		perWorker := 2560/workers + 4
		b := overdraftSaturate(t, rdb, overdraftInput(cost), workers, perWorker, nil)

		requireOneOverdraftWindow(t, b)
		requireCreditGateSilent(t, rdb, b)
		assert.Equal(t, want, b.credits,
			"the overdraft changed with the send rate at %d workers", workers)
		t.Logf("%10d %10d %10d %10d %12v", workers, workers*perWorker, b.approved, b.credits, b.elapsed)
	}
	t.Logf("credits approved is %d at every send rate: the bound is per unit TIME, not per request", want)
}

// The mutation control for the whole claim: turn the cps bucket off and the
// overdraft has no ceiling at all.
//
// config.go refuses a zero FULL_CREDITS_PER_SEC or SLOW_CREDITS_PER_SEC, and
// this is the trap it is refusing. At zero the script's `cpsLimit > 0` guard
// skips the bucket entirely, the credit gate is the only gate left, and the
// credit gate cannot refuse anybody because nothing has moved `spend` yet.
// Approvals then scale one-for-one with requests offered, with no bound in
// sight, and NOTHING goes red — every one of them returns ok.
//
// Restoring the limit restores the bound in the same process, against the same
// Redis, in the same second.
//
// LIMIT, and the reason config.go's refusal is a safety property rather than a
// quota.
func TestOverdraft_TheCpsBucketIsTheOnlyThingBoundingIt(t *testing.T) {
	if testing.Short() {
		t.Skip("fires 47,000 authorizations against a real server; ~5s")
	}
	rdb := newOverdraftRedis(t, 64)
	ctx := context.Background()
	const cost = relayDefaultCU

	t.Run("SlowCPS=0 leaves the overdraft unbounded", func(t *testing.T) {
		var last int64
		for _, offered := range []int{2048, 8192, 32768} {
			require.NoError(t, rdb.FlushAll(ctx).Err())
			seedOverdraftBalance(t, rdb, cost)

			in := overdraftInput(cost)
			in.Limits.SlowCPS = 0
			b := overdraftSaturate(t, rdb, in, 64, offered/64, nil)

			requireCreditGateSilent(t, rdb, b)
			assert.EqualValues(t, offered, b.approved, "a request was refused with the bucket switched off")
			assert.EqualValues(t, 0, b.bucket, "the bucket must not move when the gate is skipped")
			assert.Greater(t, b.overdraft(cost), last, "the overdraft did not grow with the offered load")
			last = b.overdraft(cost)
			t.Logf("SlowCPS=0, %6d requests offered: approved=%d OVERDRAFT=%d credits ($%.9f) [%s]",
				offered, b.approved, b.overdraft(cost), float64(b.overdraft(cost))/1e9, b.codeSummary())
		}
		t.Logf("no ceiling appeared: the overdraft is exactly the offered load times the cost")
	})

	t.Run("restoring SlowCPS restores the bound", func(t *testing.T) {
		require.NoError(t, rdb.FlushAll(ctx).Err())
		seedOverdraftBalance(t, rdb, cost)

		in := overdraftInput(cost)
		in.Limits.SlowCPS = relaySlowCPS
		// Fewer requests than the run above, because with the bucket armed the
		// burst has to finish inside one 2-second window to be readable.
		const offered = 4096
		b := overdraftSaturate(t, rdb, in, 64, offered/64, nil)

		requireOneOverdraftWindow(t, b)
		requireCreditGateSilent(t, rdb, b)
		assert.EqualValues(t, 498, b.credits,
			"the bound did not come back when the limit did")
		t.Logf("SlowCPS=%d, %d requests offered: approved=%d OVERDRAFT=%d credits [%s]",
			relaySlowCPS, offered, b.approved, b.overdraft(cost), b.codeSummary())
	})
}

// The per-second REQUEST gate is per API key; the credits bucket is per
// account. Only the second one bounds an account's overdraft.
//
// An account with several keys multiplies its own request-rate allowance —
// each key gets a fresh valve:rate:s:<keyId>:<second> counter — but every key
// charges the one valve:credits:<accountId>:cps bucket. So fanning out over
// keys walks straight past the RPS gate and lands on the credits bucket, which
// does not move.
//
// This is the second half of the mutation control: it rules out the RPS gate
// as the thing doing the bounding.
//
// The numbers are deterministic without a clock, because AuthorizeInput.Now is
// a parameter: the per-second key names are pinned by the fixture instant, so
// only the cps bucket depends on real time.
//
// LIMIT.
func TestOverdraft_TheRequestRateGateIsPerKeyAndDoesNotBoundTheAccount(t *testing.T) {
	rdb := newOverdraftRedis(t, 64)
	ctx := context.Background()
	const cost = 1

	t.Logf("%6s %14s %10s %10s %s", "keys", "rps allowance", "credits", "bucket", "codes")
	for _, keys := range []int{1, 2, 5, 10} {
		require.NoError(t, rdb.FlushAll(ctx).Err())
		seedOverdraftBalance(t, rdb, cost)

		keyIDs := make([]string, keys)
		for i := range keyIDs {
			keyIDs[i] = overdraftKeyID(i)
		}

		in := overdraftInput(cost)
		in.Limits.SlowRPS = relaySlowRPS // 100 requests per key per second
		b := overdraftSaturate(t, rdb, in, 20*keys, 60, keyIDs)

		requireOneOverdraftWindow(t, b)
		requireCreditGateSilent(t, rdb, b)

		allowance := int64(keys) * relaySlowRPS
		want := allowance
		if want > relaySlowCPS {
			want = relaySlowCPS
		}
		assert.Equal(t, want, b.credits,
			"%d keys: expected min(per-key RPS × keys, SlowCPS) credits", keys)
		t.Logf("%6d %14d %10d %10d %s", keys, allowance, b.credits, b.bucket, b.codeSummary())
	}
	t.Logf("the per-key gate scales with the key count; the account's credits bucket caps at %d and does not",
		relaySlowCPS)
}

// overdraftKeyID builds the nth distinct hashed-key fixture: 32 characters of
// one repeated letter. That is the shape HashAPIKey emits, at near-zero
// entropy so no secret scanner reads it as a credential. Same reasoning as
// baseInput's KeyID in authorize_test.go.
func overdraftKeyID(n int) string {
	out := make([]byte, 32)
	for i := range out {
		out[i] = byte('a' + n)
	}
	return string(out)
}

// A request costing more than the tier's credits-per-second limit can NEVER be
// authorized, even on an empty bucket and a rich account.
//
// The gate is `cpsCount + cost > cpsLimit` with no exemption for the first
// request of a window, so cost > cpsLimit is a permanent refusal, not a
// throttle that clears. At SLOW_CREDITS_PER_SEC=500 that makes any single
// request over 500 credits impossible while the account is in the SLOW tier —
// a JSON-RPC batch of eleven 50-credit methods, for instance. The account is
// not out of money; it is on the wrong side of a rate bucket, and the code it
// gets back says cps_throttle.
//
// LIMIT, and one neither document records.
func TestOverdraft_ACostAboveTheTierLimitIsRefusedForever(t *testing.T) {
	rdb := newOverdraftRedis(t, 8)
	ctx := context.Background()
	seedOverdraftBalance(t, rdb, 1_000_000_000) // $1.00, far more than any of these costs

	for _, cost := range []int64{relaySlowCPS - 1, relaySlowCPS, relaySlowCPS + 1, 550, 5000} {
		require.NoError(t, rdb.Del(ctx, cpsBucketKey("acct_1")).Err())
		v, err := Authorize(ctx, rdb, overdraftInput(cost))
		require.NoError(t, err)
		want := CodeOK
		if cost > relaySlowCPS {
			want = "cps_throttle"
		}
		assert.Equal(t, want, v.Code, "cost=%d on an empty bucket", cost)
		t.Logf("cost=%4d SlowCPS=%d empty bucket -> %s/%s", cost, relaySlowCPS, v.Code, v.Tier)
	}
}

// The 2-second TTL: armed once, never extended, and the window tumbles.
//
// The EXPIRE carries NX, so only the charge that CREATES the key sets its
// lifetime. Later charges in the same window inherit the remaining time. That
// makes the bucket a TUMBLING window anchored at the first charge, not a
// sliding one — which is what lets a client collect a whole fresh allowance
// the instant the key expires.
//
// The deterministic half of this test observes the TTL falling. The half that
// crosses the boundary needs real time; it polls for the key's disappearance
// rather than sleeping a fixed duration, and it is guarded by -short.
//
// LIMIT.
func TestOverdraft_TheTwoSecondWindowTumblesAndNeverSlides(t *testing.T) {
	rdb := newOverdraftRedis(t, 8)
	ctx := context.Background()
	seedOverdraftBalance(t, rdb, relayDefaultCU)
	in := overdraftInput(relayDefaultCU)

	t.Run("the TTL is armed once and decays", func(t *testing.T) {
		var prev time.Duration
		for i := 0; i < 4; i++ {
			v, err := Authorize(ctx, rdb, in)
			require.NoError(t, err)
			require.True(t, v.OK())
			pttl, err := rdb.PTTL(ctx, cpsBucketKey("acct_1")).Result()
			require.NoError(t, err)
			bucket, err := rdb.Get(ctx, cpsBucketKey("acct_1")).Int64()
			require.NoError(t, err)

			assert.LessOrEqual(t, pttl, cpsBucketTTL, "the TTL exceeded the 2 seconds the script arms")
			if i > 0 {
				assert.Less(t, pttl, prev,
					"charge %d refreshed the TTL; EXPIRE NX is supposed to prevent that", i)
			}
			prev = pttl
			t.Logf("charge %d: bucket=%d pttl=%v", i, bucket, pttl)
			// Enough real time to make the decay measurable, far less than the
			// window.
			time.Sleep(20 * time.Millisecond)
		}
	})

	// Deleting the key is exactly what expiry does to it, so this pins the
	// "a fresh window grants a fresh allowance" half without a clock.
	t.Run("a fresh bucket grants a fresh allowance", func(t *testing.T) {
		require.NoError(t, rdb.FlushAll(ctx).Err())
		seedOverdraftBalance(t, rdb, relayDefaultCU)
		first := overdraftSaturate(t, rdb, in, 32, 40, nil)
		requireOneOverdraftWindow(t, first)
		require.NoError(t, rdb.Del(ctx, cpsBucketKey("acct_1")).Err())
		second := overdraftSaturate(t, rdb, in, 32, 40, nil)
		requireOneOverdraftWindow(t, second)

		assert.Equal(t, first.credits, second.credits)
		t.Logf("window 1 gave %d credits, a fresh bucket gave another %d; two windows = %d credits",
			first.credits, second.credits, first.credits+second.credits)
	})

	t.Run("a real expiry does the same, and the bucket is gone at 2s", func(t *testing.T) {
		if testing.Short() {
			t.Skip("waits out a real 2-second TTL")
		}
		require.NoError(t, rdb.FlushAll(ctx).Err())
		seedOverdraftBalance(t, rdb, relayDefaultCU)

		v, err := Authorize(ctx, rdb, in)
		require.NoError(t, err)
		require.True(t, v.OK())
		created := time.Now()

		giveUp := created.Add(5 * time.Second)
		for {
			n, err := rdb.Exists(ctx, cpsBucketKey("acct_1")).Result()
			require.NoError(t, err)
			if n == 0 {
				break
			}
			require.True(t, time.Now().Before(giveUp),
				"the bucket outlived its 2-second TTL by seconds; the script's EXPIRE did not take")
			time.Sleep(2 * time.Millisecond)
		}
		lived := time.Since(created)
		assert.InDelta(t, cpsBucketTTL.Seconds(), lived.Seconds(), 0.25,
			"the bucket's measured lifetime is not the 2 seconds the script arms")

		after := overdraftSaturate(t, rdb, in, 32, 40, nil)
		requireOneOverdraftWindow(t, after)
		assert.EqualValues(t, 498, after.credits,
			"the window after a real expiry did not grant a full fresh allowance")
		t.Logf("bucket lived %v, then a fresh window granted %d credits", lived, after.credits)
	})
}

// A client that paces itself to the window edge collects two windows' worth in
// a fraction of a second.
//
// This is the tumbling window's burst behaviour, and it is the reason the
// "6 windows in 12 seconds" arithmetic in valve/periodic-enforcement.md is a
// floor rather than a bound. The client opens a window with one cheap charge,
// idles until the TTL is nearly spent, drains the rest of that window, waits
// for the key to vanish, and drains a whole new one. Both drains land inside a
// couple of hundred milliseconds.
//
// The test does not sleep a fixed duration: it polls PTTL for the edge and
// polls EXISTS for the expiry. If it misses the edge — the key expired while
// it was still lining up — it starts over, up to three times, rather than
// asserting on a run that did not test what it meant to.
//
// LIMIT.
func TestOverdraft_PacingToTheWindowEdgeDoublesTheBurst(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out a real 2-second TTL")
	}
	rdb := newOverdraftRedis(t, 8)
	ctx := context.Background()
	in := overdraftInput(relayDefaultCU)

	drain := func() int64 {
		var n int64
		for {
			v, err := Authorize(ctx, rdb, in)
			require.NoError(t, err)
			if !v.OK() {
				return n * relayDefaultCU
			}
			n++
		}
	}

	for attempt := 1; attempt <= 3; attempt++ {
		require.NoError(t, rdb.FlushAll(ctx).Err())
		seedOverdraftBalance(t, rdb, relayDefaultCU)

		// Open the window with one charge and let it nearly run out.
		v, err := Authorize(ctx, rdb, in)
		require.NoError(t, err)
		require.True(t, v.OK())

		missed := false
		for {
			pttl, err := rdb.PTTL(ctx, cpsBucketKey("acct_1")).Result()
			require.NoError(t, err)
			if pttl <= 0 {
				missed = true // the edge went past while we were lining up
				break
			}
			if pttl < 150*time.Millisecond {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		if missed {
			t.Logf("attempt %d missed the window edge; retrying", attempt)
			continue
		}

		start := time.Now()
		tail := drain() + relayDefaultCU // the rest of window 1, plus the charge that opened it
		for {
			n, err := rdb.Exists(ctx, cpsBucketKey("acct_1")).Result()
			require.NoError(t, err)
			if n == 0 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		fresh := drain()
		span := time.Since(start)

		assert.EqualValues(t, 498, tail, "window 1 did not yield its full allowance")
		assert.EqualValues(t, 498, fresh, "the window after the edge did not yield a full allowance")
		assert.Less(t, span, time.Second,
			"the two windows did not land inside a fraction of a second, so this did not test the edge")
		t.Logf("edge burst: %d + %d = %d credits in %v — two windows' allowance in well under one window",
			tail, fresh, tail+fresh, span)
		return
	}
	t.Skip("could not line up the window edge in three attempts; timing, not a behaviour change")
}

// The number: what an account can actually overdraw at the deployment
// defaults, measured over a real 12-second in-flight span.
//
// valve/periodic-enforcement.md argues the reachable bound is 3,000 credits
// and that the 30,000 figure is stated for the wrong tier. The measurements
// below agree with the ARGUMENT and disagree with both NUMBERS:
//
//   - Aligned, a 12-second span covers six windows and yields 2,988 credits,
//     not 3,000. The shortfall is the floor: floor(500/6)×6 = 498 per window,
//     because a 6-credit read cannot use the last 2 credits of a 500-credit
//     allowance. 3,000 is only reachable at a cost of 1, 2, 4, 5 or 10.
//   - Unaligned, the same 12-second span touches SEVEN windows and yields
//     3,486. The window is anchored at the first charge, not at a clock
//     boundary, so a span that starts mid-window gets the tail of that window
//     plus six more. 3,486 credits is 16% above the stated bound.
//
// So the honest figure at the defaults is 3,486 credits — $0.0000035 — and
// 3,000 is neither the aligned nor the worst-case answer.
//
// LIMIT.
func TestOverdraft_TheReachableBoundOverATwelveSecondInFlightSpan(t *testing.T) {
	if testing.Short() {
		t.Skip("saturates real Redis for 12 seconds twice; ~28s")
	}
	const inFlight = 12 * time.Second
	const perWindow = (relaySlowCPS / relayDefaultCU) * relayDefaultCU // 498

	measure := func(t *testing.T, rdb *redis.Client, in AuthorizeInput, preOpen bool) int64 {
		ctx := context.Background()
		require.NoError(t, rdb.FlushAll(ctx).Err())
		seedOverdraftBalance(t, rdb, relayDefaultCU)

		var approved int64
		if preOpen {
			// Open the window early and start the span near its edge, which is
			// what an arbitrary arrival time looks like.
			v, err := Authorize(ctx, rdb, in)
			require.NoError(t, err)
			require.True(t, v.OK())
			approved = 1
			for {
				pttl, err := rdb.PTTL(ctx, cpsBucketKey("acct_1")).Result()
				require.NoError(t, err)
				if pttl <= 0 || pttl < 150*time.Millisecond {
					break
				}
				time.Sleep(2 * time.Millisecond)
			}
		}

		var firstErr atomic.Value
		var wg sync.WaitGroup
		deadline := time.Now().Add(inFlight)
		for w := 0; w < 32; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for time.Now().Before(deadline) {
					v, err := Authorize(ctx, rdb, in)
					if err != nil {
						firstErr.CompareAndSwap(nil, err)
						return
					}
					if v.OK() {
						atomic.AddInt64(&approved, 1)
					}
				}
			}()
		}
		wg.Wait()
		if err, ok := firstErr.Load().(error); ok && err != nil {
			require.NoError(t, err)
		}
		return approved * relayDefaultCU
	}

	rdb := newOverdraftRedis(t, 32)
	in := overdraftInput(relayDefaultCU)

	t.Run("aligned: six windows", func(t *testing.T) {
		credits := measure(t, rdb, in, false)
		windows := float64(credits) / float64(perWindow)
		t.Logf("12s span starting at the window: %d credits = %.2f windows of %d; OVERDRAFT %d credits ($%.9f)",
			credits, windows, perWindow, credits-relayDefaultCU, float64(credits-relayDefaultCU)/1e9)
		assert.EqualValues(t, 6*perWindow, credits,
			"an aligned 12-second span no longer yields exactly six windows")
		assert.NotEqualValues(t, 3000, credits,
			"the document's 3,000 became reachable; re-check the floor(cps/cost) arithmetic")
	})

	t.Run("unaligned: seven windows", func(t *testing.T) {
		credits := measure(t, rdb, in, true)
		windows := float64(credits) / float64(perWindow)
		t.Logf("12s span starting at a window edge: %d credits = %.2f windows of %d; OVERDRAFT %d credits ($%.9f)",
			credits, windows, perWindow, credits-relayDefaultCU, float64(credits-relayDefaultCU)/1e9)
		assert.Greater(t, credits, int64(6*perWindow),
			"the unaligned span did not reach past six windows; the phase effect may have been missed")
		assert.EqualValues(t, 7*perWindow, credits,
			"the worst-phase 12-second span moved off seven windows")
		assert.Greater(t, credits, int64(3000),
			"the reachable bound is meant to exceed the document's stated 3,000")
	})
}

// Which tier an overdrawing account is in, and what that does to the bound.
//
// valve/periodic-enforcement.md argues an account can only overdraft when its
// effective balance is near zero, which is ~10^9 times below the $5 threshold,
// so it is always in SLOW and the FULL branch cannot overdraft at all. At the
// DEFAULT threshold that is measured to be true.
//
// It is a property of the configured threshold, not of the script. The tier
// test is `effective < thresh`, so an account holding just over the threshold
// is FULL — and if the threshold is small enough to sit under a window's FULL
// allowance, the FULL branch overdrafts too. config.go requires the threshold
// to be positive but sets no floor, so SLOW_MODE_THRESHOLD_USD=0.000001 is
// accepted and puts a 1,000-credit account on the FULL tier with a 4,998-credit
// window.
//
// LIMIT, and a correction: "the FULL branch cannot overdraft" is true of
// today's numbers, not of the design.
func TestOverdraft_TheTierIsAConfiguredNumberNotAnInvariant(t *testing.T) {
	rdb := newOverdraftRedis(t, 64)
	ctx := context.Background()
	const balance = 600 // $0.0000006

	t.Logf("%18s %8s %10s %12s", "threshold", "tier", "credits", "overdraft")
	for _, thresh := range []int64{relaySlowThreshold, 1000, balance} {
		require.NoError(t, rdb.FlushAll(ctx).Err())
		seedOverdraftBalance(t, rdb, balance)

		in := overdraftInput(relayDefaultCU)
		in.Limits.SlowThreshold = thresh
		b := overdraftSaturate(t, rdb, in, 64, 100, nil)

		requireOneOverdraftWindow(t, b)
		requireCreditGateSilent(t, rdb, b)

		wantCPS := relaySlowCPS
		wantTier := "SLOW"
		if balance >= thresh {
			wantCPS = relayFullCPS
			wantTier = "FULL"
		}
		want := (wantCPS / relayDefaultCU) * relayDefaultCU
		assert.Equal(t, want, b.credits, "threshold=%d", thresh)
		t.Logf("%18d %8s %10d %12d", thresh, wantTier, b.credits, b.overdraft(balance))
	}
	t.Logf("at the default $5 threshold an overdrawing account is always SLOW, so its window is %d credits, "+
		"not %d; a threshold under one FULL window puts it on the FULL branch instead",
		(relaySlowCPS/relayDefaultCU)*relayDefaultCU, (relayFullCPS/relayDefaultCU)*relayDefaultCU)
}
