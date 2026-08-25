package valvebilling

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/redis/go-redis/v9"
)

// Settling a window: move the counter under a new name, write it down, then
// delete the name.
//
// Redis accumulates, Postgres resolves, the counter goes back to zero. That is
// the model valve/billing-module.md settles on, and this file is the half of
// it that runs in the relay. The whole difficulty is that the process can die
// between any two steps, and the two obvious ways to write this both lose or
// duplicate money at a crash point.
//
// # What was measured, and why this shape
//
// One window of 1000 credits, one concurrent Capture of 250 landing during the
// settle. Postgres must end up holding 1250. Every row below was run against a
// real redis-server 7.2.4; settle_test.go reproduces all five.
//
//	strategy                                     crash after      Postgres holds
//	read -> Postgres -> DECRBY V                 the Postgres write   2250  billed twice
//	GETSET spend 0 -> Postgres                   the GETSET            250  window lost
//	RENAME -> Postgres -> DEL                    the RENAME           1250  exact
//	RENAME -> Postgres -> DEL, append write      the Postgres write   2250  billed twice
//	RENAME -> Postgres -> DEL, upsert on window  the Postgres write   1250  exact
//
// Two independent properties are needed and NEITHER is sufficient alone:
//
//  1. An atomic move to a NAMED staging key. GETSET also closes the read/write
//     race, but it hands the amount to a variable in a process that is about to
//     die. RENAME hands it to Redis under a name a later process can find.
//  2. A durable write that is idempotent on that name. Without it the first
//     crash point is fixed and the second one — crash after Postgres, before
//     DEL — bills the window again on recovery.
//
// The crash hole is the worse of the two failures, which is why RENAME beats
// GETSET even though GETSET is one command shorter. A double bill is visible:
// the customer complains and the row is there to find. A lost window is
// silent — the credits simply never arrive and nothing goes red.
//
// # The idempotency is the caller's half of the contract
//
// This package cannot enforce it. It hands the writer an account, a window id
// and an amount; whether the write is an upsert keyed on (account, window id)
// or a plain append is decided in the writer, and a plain append is row 4 of
// the table. SettleWriter's doc states the requirement. It is the one thing a
// reviewer of a new writer has to check.
//
// # What this does NOT settle
//
// The spend counter only. billing-module.md also requires the CEILING to be
// windowed, and that is a different operation: the ceiling is a lease, so a
// settle re-grants it with a fresh amount rather than taking it away. Renaming
// a ceiling out from under a live account would refuse every request until the
// re-grant landed. Do not reach for this primitive for that job.

// stagingInfix separates a counter key from the window id staged under it.
//
// The staging key helper lives here rather than in keys.go, which is where the
// other key names live. That is not a preference: this change may not touch
// keys.go. The names in that file address state the api service also writes,
// so they are a shared contract; this one is private to the relay's settle
// path and nothing outside this package constructs or reads it. If it ever
// becomes shared, move it there.
const stagingInfix = ":settling:"

// stagingScanPattern finds every staging key in the keyspace, for any account.
//
// It matches the shape spendKey builds, plus the infix and an id. keys.go owns
// that shape and this file cannot import a parser from it, so
// TestStagingKey_RoundTripsThroughTheNameKeysGoBuilds pins the two together —
// a rename in keys.go fails a test instead of silently making recovery find
// nothing.
const stagingScanPattern = "valve:credits:*:spend" + stagingInfix + "*"

// stagingKey names one staged window.
func stagingKey(counterKey, windowID string) string {
	return counterKey + stagingInfix + windowID
}

// parseStagingKey reads the account and window back out of a staging key.
//
// The name is the ONLY thing a crash leaves behind, so it has to carry both.
// Storing the account id alongside the amount was the alternative, and it
// fails: the value is what INCRBY wrote, a bare integer, and a second key
// holding the metadata could not be written atomically with the RENAME.
//
// It splits on the LAST occurrence of the infix, so an account id that
// contains ":settling:" still parses. The window ids this file generates are
// hex and contain no colon.
func parseStagingKey(key string) (accountID, windowID string, ok bool) {
	i := strings.LastIndex(key, stagingInfix)
	if i < 0 {
		return "", "", false
	}
	counter, windowID := key[:i], key[i+len(stagingInfix):]
	if windowID == "" {
		return "", "", false
	}
	const prefix, suffix = "valve:credits:", ":spend"
	if !strings.HasPrefix(counter, prefix) || !strings.HasSuffix(counter, suffix) {
		return "", "", false
	}
	accountID = counter[len(prefix) : len(counter)-len(suffix)]
	if accountID == "" {
		return "", "", false
	}
	return accountID, windowID, true
}

// newWindowID mints an id for a window that is about to be staged.
//
// # Why not a clock
//
// The id has two jobs: be identical on every retry of the SAME window, and be
// different for the next one. The first job is not done by generating the id
// at all — it is done by the id living in the Redis KEY NAME. RENAME publishes
// the name in the same instant it takes custody of the amount, so every later
// attempt reads the id back off the key rather than recomputing it. Nothing
// has to be reproducible.
//
// That leaves distinctness, and a clock supplies it badly. Two processes
// settling the same account inside the same tick mint the same id for two
// different amounts, and an upsert keyed on (account, window id) then merges
// two windows into one row — it loses money using the exact mechanism that was
// supposed to protect it. Skew and NTP steps make it worse, and a workflow
// cannot call time.Now() at all. A Redis INCR would work, but it adds a shared
// key, a round trip and an ordering claim nothing reads.
//
// 128 random bits commit to none of that: no clock, no coordination, no
// ordering. A collision needs two draws out of 2^128.
//
// A crypto/rand failure is returned, not worked around. A fallback to a weaker
// source here would be a fallback to the failure mode above.
func newWindowID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("valvebilling: cannot mint a settle window id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// settleLua takes custody of a window: it moves the counter to the staging key
// and returns the amount, or nil when there is nothing to settle.
//
// It is a script rather than a bare RENAME for one reason. RENAME on a missing
// key answers with an error reply, and an empty window is NOT an error — an
// account that served no traffic this window is the ordinary case. Telling the
// two apart from Go means matching the text of a vendor error string, which is
// a commitment this repo's razor rejects outright: the message is not part of
// any protocol we validate, and a changed message would turn every empty
// window into a settle failure. EXISTS answers the same question with a
// number.
//
// PERSIST is here because RENAME carries the source key's TTL across. Nothing
// sets a TTL on the spend counter today — authorize.lua expires the rate
// buckets and leaves spend alone — but this file cannot enforce that, and a
// staging key that expired would delete money we had already taken custody of
// and never written down. PERSIST removes the dependency instead of asserting
// it. It is a no-op on a key with no TTL.
const settleLua = `if redis.call('EXISTS', KEYS[1]) == 0 then
  return false
end
redis.call('RENAME', KEYS[1], KEYS[2])
redis.call('PERSIST', KEYS[2])
return redis.call('GET', KEYS[2])`

var settleScript = redis.NewScript(settleLua)

// SettleWriter records one settled window durably.
//
// # It MUST be idempotent on (accountID, windowID)
//
// This is the load-bearing sentence in the file. The settle can crash after
// the writer returns and before the staging key is deleted, and recovery then
// hands the writer the same account, the same window id and the same amount a
// second time. A writer that appends a row bills that window twice — measured,
// row 4 of the table at the top of this file. A writer that upserts on
// (account, window id) ends on the exact total — row 5.
//
// The same property covers two settlers recovering the same orphan at once,
// which needs no extra locking because of it.
//
// # Why a func and not an interface
//
// Nothing here needs a named type or a struct to implement. The signature is
// the whole contract: an account, a window id, an amount, an error. This
// package must not learn what is on the other side of it — Postgres today, and
// this file would be wrong to know that.
//
// # The amount
//
// A *big.Int, matching Cost and Capture. It is whatever the counter held, read
// back verbatim: this file does not interpret it, cap it or round it. It is
// non-negative in practice because Capture refuses a negative charge, but
// nothing here depends on that.
type SettleWriter func(ctx context.Context, accountID, windowID string, amount *big.Int) error

// Settle closes the current spend window for one account.
//
// It returns the amount handed to the writer. An account with no counter — no
// traffic since the last settle — settles as zero with no writer call and no
// error. That is the common case, not an exception.
//
// On any error the staging key is LEFT IN PLACE and the amount stays in Redis
// under its window id. Nothing is lost by a failure here; the next Recover
// picks it up. That is why the writer's error is wrapped rather than swallowed
// and why the caller must not delete anything itself.
//
// A capture that lands between the RENAME and the next INCRBY is not lost
// either. The counter key is gone, so INCRBY recreates it at the captured
// amount and that becomes the next window. Nothing in the gap is dropped,
// because there is no gap: the RENAME is one operation.
//
// Callers should run Recover once before a settle pass. Settle deliberately
// does not sweep for orphans itself — a SCAN costs a walk of the keyspace, and
// paying that per account instead of once per pass is the same work multiplied
// by the number of accounts.
func Settle(ctx context.Context, rdb redis.Cmdable, accountID string, write SettleWriter) (*big.Int, error) {
	if accountID == "" {
		return nil, errors.New("valvebilling: settle called with an empty accountId")
	}
	if write == nil {
		return nil, fmt.Errorf("valvebilling: settle of account %q called with no writer; "+
			"the counter would be cleared with nothing recording it", accountID)
	}
	windowID, err := newWindowID()
	if err != nil {
		return nil, err
	}

	counter := spendKey(accountID)
	staging := stagingKey(counter, windowID)

	raw, err := settleScript.Run(ctx, rdb, []string{counter, staging}).Text()
	if errors.Is(err, redis.Nil) {
		// No counter. Nothing to settle, and that is not a failure.
		return big.NewInt(0), nil
	}
	if err != nil {
		return nil, fmt.Errorf("valvebilling: cannot stage the spend window for account %q: %w", accountID, err)
	}

	amount, err := parseCounter(raw)
	if err != nil {
		// The amount is safe under its own name; refuse rather than guess.
		return nil, fmt.Errorf("valvebilling: account %q window %q staged at %s: %w", accountID, windowID, staging, err)
	}
	if err := finishWindow(ctx, rdb, staging, accountID, windowID, amount, write); err != nil {
		return nil, err
	}
	return amount, nil
}

// Recover finishes every window a crash left staged, across all accounts.
//
// Run it before a settle pass. Money sitting in a staging key is money the
// ledger has not seen, and a settler that only ever started new windows would
// defer the oldest one for as long as the writer kept failing.
//
// It returns how many windows it wrote. One account's failure does not stop
// the sweep: the errors are joined and returned together, so a single broken
// account cannot hold up every other account's money.
//
// # Why SCAN and not KEYS
//
// KEYS walks the whole keyspace inside one command and blocks the server for
// the duration. This Redis also serves the authorize script on the request
// path, so a KEYS against a large keyspace stalls live traffic — the settler's
// housekeeping would show up as latency on customer requests.
//
// SCAN does the same walk in bounded slices with a cursor. MATCH filters after
// the fetch, so the total work is the same; what changes is that no single
// command holds the server. Its weak guarantees are all this needs: a key
// present for the whole scan is returned at least once, and a key may be
// returned more than once. A duplicate is harmless here — the second pass
// finds the key already deleted and skips it, and even if it did not, the
// writer's idempotency absorbs it.
//
// # Why not a set of in-flight keys
//
// A Redis SET of staging keys would make this an O(members) lookup instead of
// a keyspace walk. It is rejected because the SADD cannot be atomic with the
// RENAME: a crash in between leaves a staging key that the set does not list,
// and recovery then walks past money it cannot see. That is the silent loss
// this whole file exists to prevent. The key names are what the RENAME made
// durable, so the key names are what recovery must read.
func Recover(ctx context.Context, rdb redis.Cmdable, write SettleWriter) (int, error) {
	if write == nil {
		return 0, errors.New("valvebilling: recover called with no writer")
	}

	var (
		cursor uint64
		done   int
		errs   []error
	)
	for {
		keys, next, err := rdb.Scan(ctx, cursor, stagingScanPattern, scanBatch).Result()
		if err != nil {
			errs = append(errs, fmt.Errorf("valvebilling: scanning for staged windows: %w", err))
			break
		}
		for _, key := range keys {
			ok, err := recoverOne(ctx, rdb, key, write)
			if err != nil {
				errs = append(errs, err)
			}
			if ok {
				done++
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	return done, errors.Join(errs...)
}

// scanBatch is the COUNT hint per SCAN call. It is a hint, not a limit: Redis
// may return more or fewer. It trades round trips against how long one command
// occupies the server, and nothing measured picked this number — it is the
// usual middle of the range and there is no evidence here for anything finer.
const scanBatch = 256

// recoverOne finishes one staged window. It reports whether it wrote one.
func recoverOne(ctx context.Context, rdb redis.Cmdable, key string, write SettleWriter) (bool, error) {
	accountID, windowID, ok := parseStagingKey(key)
	if !ok {
		// Matched the pattern but is not a name this file builds. Report it
		// and leave it alone: deleting a key we cannot explain is how a
		// recovery pass turns into a data loss.
		return false, fmt.Errorf("valvebilling: %s matches the staging pattern but is not a staging key; leaving it", key)
	}

	raw, err := rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		// Gone since the scan: another settler finished it, or SCAN returned
		// the same key twice. Both are ordinary.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("valvebilling: reading staged window %s: %w", key, err)
	}
	amount, err := parseCounter(raw)
	if err != nil {
		return false, fmt.Errorf("valvebilling: staged window %s: %w", key, err)
	}
	if err := finishWindow(ctx, rdb, key, accountID, windowID, amount, write); err != nil {
		return false, err
	}
	return true, nil
}

// finishWindow writes a staged amount down, then drops the staging key.
//
// The order is the whole point. DEL runs ONLY after the writer returns nil, so
// a writer that fails leaves the amount in Redis under a name the next Recover
// will find. A DEL before or beside the write is the lost-window failure with
// extra steps.
func finishWindow(ctx context.Context, rdb redis.Cmdable, staging, accountID, windowID string, amount *big.Int, write SettleWriter) error {
	if err := write(ctx, accountID, windowID, amount); err != nil {
		return fmt.Errorf(
			"valvebilling: account %q window %q (%s credits) was not written durably; %s is left in place "+
				"for the next recovery: %w", accountID, windowID, amount, staging, err)
	}
	if err := rdb.Del(ctx, staging).Err(); err != nil {
		// The money is recorded. The staging key surviving means the next
		// recovery presents the same window id again, which an idempotent
		// writer absorbs — this is exactly the crash point the idempotency
		// requirement is for.
		return fmt.Errorf(
			"valvebilling: account %q window %q was written but %s survived; recovery will present it again: %w",
			accountID, windowID, staging, err)
	}
	return nil
}

// parseCounter reads a counter's stored value.
//
// It is whatever INCRBY wrote, which is a base-10 integer. A value that is not
// one is refused rather than coerced: a coerced amount bills a number nobody
// chose, and the caller's contract is to leave the key alone on an error.
func parseCounter(raw string) (*big.Int, error) {
	n, ok := new(big.Int).SetString(raw, 10)
	if !ok {
		return nil, fmt.Errorf("value %q is not a base-10 integer; refusing to settle it", raw)
	}
	return n, nil
}
