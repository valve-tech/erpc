package valvebilling

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every test in this file runs against a real redis-server, not miniredis.
//
// The rest of the package uses miniredis and is right to. This file is not:
// what it measures is exactly the semantics an emulator is most likely to get
// nearly right — whether RENAME is atomic against a concurrent INCRBY, what
// RENAME does to a TTL, what a missing source key returns, whether GETSET and
// the counter it clears can be interleaved. A near-miss in any of those would
// make a settle design look correct here and lose money in production.
// limits_test.go already records one divergence between the two engines.
//
// The measured table this file pins, one window of 1000 credits with a
// concurrent Capture of 250 landing during the settle. Postgres must end with
// 1250:
//
//	strategy                                     crash after      Postgres holds
//	read -> Postgres -> DECRBY V                 the Postgres write   2250
//	GETSET spend 0 -> Postgres                   the GETSET            250
//	RENAME -> Postgres -> DEL                    the RENAME           1250
//	RENAME -> Postgres -> DEL, append write      the Postgres write   2250
//	RENAME -> Postgres -> DEL, upsert on window  the Postgres write   1250
//
// The conclusion: a correct settle needs BOTH an atomic move to a NAMED
// staging key AND a durable write that is idempotent on that name. Either one
// alone fails at one of the two crash points.

const (
	settleWindowCredits     = 1000 // what the settle takes custody of
	settleConcurrentCredits = 250  // what Capture adds while the settle is in flight
	settleTrueTotal         = 1250 // what Postgres must hold when the dust settles
)

// newSettleRedis starts a redis-server for one test and returns a client.
//
// It listens on a unix socket in a temporary directory rather than a TCP port.
// Picking a free port means a race between the check and the bind, and a
// flaky billing test is a test people learn to re-run.
//
// A missing binary SKIPS rather than fails, which is the standard Go answer
// and also a real hole: a CI without redis-server runs none of this and the
// suite still reads green. See the report accompanying this file.
func newSettleRedis(t *testing.T) *redis.Client {
	t.Helper()

	bin := "/usr/local/bin/redis-server"
	if _, err := os.Stat(bin); err != nil {
		found, lookErr := exec.LookPath("redis-server")
		if lookErr != nil {
			t.Skipf("no redis-server available (%v); these tests measure real Redis semantics and "+
				"miniredis is not a substitute for them", err)
		}
		bin = found
	}

	dir, err := os.MkdirTemp("", "vbsettle")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "r.sock")

	cmd := exec.Command(bin,
		"--port", "0", // no TCP listener at all
		"--unixsocket", sock,
		"--dir", dir,
		"--save", "", // no RDB snapshots
		"--appendonly", "no",
	)
	// The server's own log is captured rather than passed through. A
	// redis-server banner per test drowns the package's output, and this file
	// starts one server per test on purpose.
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	require.NoError(t, cmd.Start(), "cannot start %s", bin)
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.Dial("unix", sock)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("redis-server did not open %s: %v\n%s", sock, err, log.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	rdb := redis.NewClient(&redis.Options{Network: "unix", Addr: sock})
	t.Cleanup(func() { _ = rdb.Close() })
	require.NoError(t, rdb.Ping(context.Background()).Err())
	return rdb
}

// settleLedger stands in for the durable store. It models both write shapes,
// because which one the writer uses is the difference between rows 4 and 5 of
// the table.
//
// idempotent = an upsert keyed on (account, window id): the same window
// written twice leaves one row. Otherwise every call appends a row of its own,
// which is the naive write and the one that double-bills.
type settleLedger struct {
	mu         sync.Mutex
	idempotent bool
	rows       map[string]*big.Int
	calls      int
	// fail, when set, decides whether a given write fails. It runs before the
	// row is recorded, so a failing write records nothing.
	fail func(accountID, windowID string) error
}

func newSettleLedger(idempotent bool) *settleLedger {
	return &settleLedger{idempotent: idempotent, rows: map[string]*big.Int{}}
}

func (l *settleLedger) write(_ context.Context, accountID, windowID string, amount *big.Int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.fail != nil {
		if err := l.fail(accountID, windowID); err != nil {
			return err
		}
	}
	rowKey := accountID + "|" + windowID
	if !l.idempotent {
		// A plain append: nothing collides, so a replayed window bills again.
		rowKey = fmt.Sprintf("%s|%s|append%d", accountID, windowID, l.calls)
	}
	l.rows[rowKey] = new(big.Int).Set(amount)
	return nil
}

func (l *settleLedger) writer() SettleWriter { return l.write }

func (l *settleLedger) total() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	sum := new(big.Int)
	for _, v := range l.rows {
		sum.Add(sum, v)
	}
	return sum.Int64()
}

func (l *settleLedger) writeCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

func (l *settleLedger) rowCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.rows)
}

// capture is the concurrent charge that lands during a settle. It goes through
// the real Capture so the test cannot drift from what production does.
func settleCapture(t *testing.T, rdb redis.Cmdable, accountID string, credits int64) {
	t.Helper()
	require.NoError(t, Capture(context.Background(), rdb, accountID, big.NewInt(credits)))
}

func settleSpendNow(t *testing.T, rdb redis.Cmdable, accountID string) int64 {
	t.Helper()
	v, err := rdb.Get(context.Background(), spendKey(accountID)).Int64()
	if err == redis.Nil {
		return 0
	}
	require.NoError(t, err)
	return v
}

// settleStageOnly performs the settle up to and including the RENAME, then stops.
//
// That is a crash between the RENAME and the durable write, expressed by not
// running the next step. It calls the same script Settle calls, so the test
// cannot pass against a rename this file does and Settle does not.
func settleStageOnly(t *testing.T, rdb redis.Cmdable, accountID string) (staging string, amount int64) {
	t.Helper()
	windowID, err := newWindowID()
	require.NoError(t, err)
	staging = stagingKey(spendKey(accountID), windowID)
	raw, err := settleScript.Run(context.Background(), rdb, []string{spendKey(accountID), staging}).Text()
	require.NoError(t, err)
	n, err := parseCounter(raw)
	require.NoError(t, err)
	return staging, n.Int64()
}

func scanStagingKeys(t *testing.T, rdb redis.Cmdable) []string {
	t.Helper()
	var (
		cursor uint64
		found  []string
		seen   = map[string]bool{}
	)
	for {
		keys, next, err := rdb.Scan(context.Background(), cursor, stagingScanPattern, scanBatch).Result()
		require.NoError(t, err)
		for _, k := range keys {
			if !seen[k] {
				seen[k] = true
				found = append(found, k)
			}
		}
		if next == 0 {
			return found
		}
		cursor = next
	}
}

// The five measured strategies, each crashed at the point the operator crashed
// it. This is the evidence for the design, restated as executable rows.
//
// Every row runs the SAME scenario: a window of 1000, a concurrent Capture of
// 250, a crash, then whatever recovery that strategy has. The only difference
// is the strategy. A row that does not end at 1250 is a strategy that must not
// ship, and the test says which failure it is.
func TestSettleStrategies_TheFiveMeasuredCrashOutcomes(t *testing.T) {
	ctx := context.Background()

	// Row 1. Read the counter, write it down, subtract what you read.
	// The concurrency is handled — DECRBY keeps the concurrent 250, exactly as
	// limits_test.go measured. The CRASH is not: nothing records that the
	// window was already written, so the retry writes it again.
	t.Run("read then Postgres then DECRBY: 2250, billed twice", func(t *testing.T) {
		rdb := newSettleRedis(t)
		ledger := newSettleLedger(false)
		require.NoError(t, rdb.Set(ctx, spendKey("acct_1"), "1000", 0).Err())

		// The settler reads the window.
		read, err := rdb.Get(ctx, spendKey("acct_1")).Int64()
		require.NoError(t, err)
		require.EqualValues(t, settleWindowCredits, read)

		settleCapture(t, rdb, "acct_1", settleConcurrentCredits)

		// It writes the window down...
		require.NoError(t, ledger.write(ctx, "acct_1", "window-a", big.NewInt(read)))
		// ...and dies here, before the DECRBY.

		// Recovery, such as it is: run the settle again. There is no staging
		// key and no window id, so all it can do is read the counter.
		read2, err := rdb.Get(ctx, spendKey("acct_1")).Int64()
		require.NoError(t, err)
		require.NoError(t, ledger.write(ctx, "acct_1", "window-b", big.NewInt(read2)))

		t.Logf("read/DECRBY, crash after the write: ledger holds %d, true total %d", ledger.total(), settleTrueTotal)
		assert.EqualValues(t, 2250, ledger.total(),
			"the read/DECRBY strategy stopped double-billing; re-measure before trusting it")
		assert.Greater(t, ledger.total(), int64(settleTrueTotal), "the customer is billed for credits they did not spend")
	})

	// Row 2. GETSET closes the read/write race in one operation. It also hands
	// the only copy of the amount to a process that is about to die.
	t.Run("GETSET spend 0 then Postgres: 250, the window is lost", func(t *testing.T) {
		rdb := newSettleRedis(t)
		ledger := newSettleLedger(true)
		require.NoError(t, rdb.Set(ctx, spendKey("acct_1"), "1000", 0).Err())

		// SET key 0 GET is GETSET under its current spelling — Redis 6.2
		// replaced the command and go-redis deprecated the old call. The two
		// read and clear identically, and using the current one proves the
		// hole below is a property of read-and-clear, not an artifact of a
		// legacy command.
		prev, err := rdb.SetArgs(ctx, spendKey("acct_1"), "0", redis.SetArgs{Get: true}).Result()
		require.NoError(t, err)
		taken, err := strconv.ParseInt(prev, 10, 64)
		require.NoError(t, err)
		require.EqualValues(t, settleWindowCredits, taken)
		// Dies here. `taken` was only ever in memory.

		settleCapture(t, rdb, "acct_1", settleConcurrentCredits)

		// Recovery: nothing names the lost window, so the settler can only
		// take what is in the counter now.
		next, err := rdb.Get(ctx, spendKey("acct_1")).Int64()
		require.NoError(t, err)
		require.NoError(t, ledger.write(ctx, "acct_1", "window-b", big.NewInt(next)))

		t.Logf("GETSET, crash after it: ledger holds %d, true total %d, LOST %d credits",
			ledger.total(), settleTrueTotal, settleTrueTotal-ledger.total())
		assert.EqualValues(t, 250, ledger.total(),
			"the GETSET crash hole closed; re-measure before preferring it to RENAME")
		assert.Less(t, ledger.total(), int64(settleTrueTotal),
			"this is the silent failure: the credits are gone and nothing goes red")
	})

	// Row 3. The move is atomic AND the amount survives under a name.
	t.Run("RENAME then Postgres then DEL, crash after the RENAME: 1250, exact", func(t *testing.T) {
		rdb := newSettleRedis(t)
		ledger := newSettleLedger(true)
		require.NoError(t, rdb.Set(ctx, spendKey("acct_1"), "1000", 0).Err())

		staging := spendKey("acct_1") + ":settling:window-a"
		require.NoError(t, rdb.Rename(ctx, spendKey("acct_1"), staging).Err())
		// Dies here, before the write.

		settleCapture(t, rdb, "acct_1", settleConcurrentCredits)

		// Recovery finds the orphan by name and finishes it.
		orphan, err := rdb.Get(ctx, staging).Int64()
		require.NoError(t, err)
		require.NoError(t, ledger.write(ctx, "acct_1", "window-a", big.NewInt(orphan)))
		require.NoError(t, rdb.Del(ctx, staging).Err())

		// And the next window settles the charge that landed during the gap.
		next, err := rdb.Get(ctx, spendKey("acct_1")).Int64()
		require.NoError(t, err)
		require.NoError(t, ledger.write(ctx, "acct_1", "window-b", big.NewInt(next)))

		t.Logf("RENAME, crash after it: ledger holds %d, true total %d", ledger.total(), settleTrueTotal)
		assert.EqualValues(t, settleTrueTotal, ledger.total())
	})

	// Row 4. The same rename, the later crash, and a writer that just appends.
	// The staging key fixed the first crash point and did nothing for this one.
	t.Run("RENAME with an append write, crash after the write: 2250, billed twice", func(t *testing.T) {
		rdb := newSettleRedis(t)
		ledger := newSettleLedger(false)
		require.NoError(t, rdb.Set(ctx, spendKey("acct_1"), "1000", 0).Err())

		staging := spendKey("acct_1") + ":settling:window-a"
		require.NoError(t, rdb.Rename(ctx, spendKey("acct_1"), staging).Err())
		settleCapture(t, rdb, "acct_1", settleConcurrentCredits)

		amount, err := rdb.Get(ctx, staging).Int64()
		require.NoError(t, err)
		require.NoError(t, ledger.write(ctx, "acct_1", "window-a", big.NewInt(amount)))
		// Dies here, before the DEL. The staging key still exists.

		// Recovery finds it again and, because the write appends, bills again.
		again, err := rdb.Get(ctx, staging).Int64()
		require.NoError(t, err)
		require.NoError(t, ledger.write(ctx, "acct_1", "window-a", big.NewInt(again)))
		require.NoError(t, rdb.Del(ctx, staging).Err())

		next, err := rdb.Get(ctx, spendKey("acct_1")).Int64()
		require.NoError(t, err)
		require.NoError(t, ledger.write(ctx, "acct_1", "window-b", big.NewInt(next)))

		t.Logf("RENAME + append write, crash after the write: ledger holds %d, true total %d",
			ledger.total(), settleTrueTotal)
		assert.EqualValues(t, 2250, ledger.total(),
			"an append writer stopped double-billing on replay; that would make the idempotency requirement moot")
		assert.Equal(t, 3, ledger.rowCount(), "the replay must land as a second row, not an overwrite")
	})

	// Row 5. The same again with an upsert keyed on (account, window id). The
	// replay overwrites its own row instead of adding one.
	t.Run("RENAME with an upsert on the window id, crash after the write: 1250, exact", func(t *testing.T) {
		rdb := newSettleRedis(t)
		ledger := newSettleLedger(true)
		require.NoError(t, rdb.Set(ctx, spendKey("acct_1"), "1000", 0).Err())

		staging := spendKey("acct_1") + ":settling:window-a"
		require.NoError(t, rdb.Rename(ctx, spendKey("acct_1"), staging).Err())
		settleCapture(t, rdb, "acct_1", settleConcurrentCredits)

		amount, err := rdb.Get(ctx, staging).Int64()
		require.NoError(t, err)
		require.NoError(t, ledger.write(ctx, "acct_1", "window-a", big.NewInt(amount)))
		// Dies here.

		again, err := rdb.Get(ctx, staging).Int64()
		require.NoError(t, err)
		require.NoError(t, ledger.write(ctx, "acct_1", "window-a", big.NewInt(again)))
		require.NoError(t, rdb.Del(ctx, staging).Err())

		next, err := rdb.Get(ctx, spendKey("acct_1")).Int64()
		require.NoError(t, err)
		require.NoError(t, ledger.write(ctx, "acct_1", "window-b", big.NewInt(next)))

		t.Logf("RENAME + upsert, crash after the write: ledger holds %d over %d rows, %d writer calls",
			ledger.total(), ledger.rowCount(), ledger.writeCount())
		assert.EqualValues(t, settleTrueTotal, ledger.total())
		assert.Equal(t, 3, ledger.writeCount(), "the replay must reach the writer")
		assert.Equal(t, 2, ledger.rowCount(), "and must land on the row it already wrote")
	})
}

// The two crash points of the shipped design, through the shipped API.
//
// This is the test that says the design works, as opposed to the table above
// which says why the alternatives do not.
func TestSettle_RecoversFromBothCrashPointsToTheExactTotal(t *testing.T) {
	ctx := context.Background()

	t.Run("crash between the RENAME and the write", func(t *testing.T) {
		rdb := newSettleRedis(t)
		ledger := newSettleLedger(true)
		require.NoError(t, rdb.Set(ctx, spendKey("acct_1"), "1000", 0).Err())

		staging, amount := settleStageOnly(t, rdb, "acct_1")
		require.EqualValues(t, settleWindowCredits, amount)
		require.Equal(t, 0, ledger.writeCount(), "nothing may be written yet")

		settleCapture(t, rdb, "acct_1", settleConcurrentCredits)

		done, err := Recover(ctx, rdb, ledger.writer())
		require.NoError(t, err)
		assert.Equal(t, 1, done)
		assert.False(t, settleKeyExists(t, rdb, staging), "a recovered window must not leave its staging key")

		settled, err := Settle(ctx, rdb, "acct_1", ledger.writer())
		require.NoError(t, err)
		assert.EqualValues(t, settleConcurrentCredits, settled.Int64())

		t.Logf("crash after RENAME: ledger holds %d over %d rows", ledger.total(), ledger.rowCount())
		assert.EqualValues(t, settleTrueTotal, ledger.total())
	})

	t.Run("crash between the write and the DEL", func(t *testing.T) {
		rdb := newSettleRedis(t)
		ledger := newSettleLedger(true)
		require.NoError(t, rdb.Set(ctx, spendKey("acct_1"), "1000", 0).Err())

		// Take custody, write it down, then die before the DEL.
		staging, amount := settleStageOnly(t, rdb, "acct_1")
		_, windowID, ok := parseStagingKey(staging)
		require.True(t, ok)
		require.NoError(t, ledger.write(ctx, "acct_1", windowID, big.NewInt(amount)))

		settleCapture(t, rdb, "acct_1", settleConcurrentCredits)

		// Recovery presents the SAME window id a second time. The idempotent
		// writer absorbs it; an append writer would bill 1000 twice here, and
		// row 4 above shows that it does.
		done, err := Recover(ctx, rdb, ledger.writer())
		require.NoError(t, err)
		assert.Equal(t, 1, done)

		settled, err := Settle(ctx, rdb, "acct_1", ledger.writer())
		require.NoError(t, err)
		assert.EqualValues(t, settleConcurrentCredits, settled.Int64())

		t.Logf("crash after the write: ledger holds %d over %d rows from %d writer calls",
			ledger.total(), ledger.rowCount(), ledger.writeCount())
		assert.EqualValues(t, settleTrueTotal, ledger.total())
		assert.Equal(t, 3, ledger.writeCount(), "the replay must reach the writer")
		assert.Equal(t, 2, ledger.rowCount(), "and must not create a row")
	})
}

// A capture landing during the rename window goes into the NEXT window rather
// than being lost, and the window it lands in settles it.
//
// There is no window to land in, which is the point: the RENAME is one
// operation, so a capture either precedes it and is settled, or follows it and
// starts the next counter. This drives real captures from several goroutines
// across a settle and checks that the two halves add up to every credit.
func TestSettle_ACaptureDuringTheRenameLandsInTheNextWindow(t *testing.T) {
	ctx := context.Background()
	rdb := newSettleRedis(t)
	ledger := newSettleLedger(true)

	const writers = 8
	const perWriter = 200
	const each = 3 // credits per capture

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < perWriter; j++ {
				if err := Capture(ctx, rdb, "acct_1", big.NewInt(each)); err != nil {
					t.Errorf("capture: %v", err)
					return
				}
			}
		}()
	}

	settleErr := make(chan error, 1)
	settled := make(chan int64, 1)
	go func() {
		<-start
		time.Sleep(2 * time.Millisecond) // land the rename in the middle of the traffic
		amount, err := Settle(ctx, rdb, "acct_1", ledger.writer())
		if err != nil {
			settleErr <- err
			return
		}
		settleErr <- nil
		settled <- amount.Int64()
	}()

	close(start)
	wg.Wait()
	require.NoError(t, <-settleErr)
	first := <-settled

	// Whatever arrived after the rename is the next window. Settle it.
	second, err := Settle(ctx, rdb, "acct_1", ledger.writer())
	require.NoError(t, err)

	const want = writers * perWriter * each
	t.Logf("captured %d credits across %d goroutines: first window %d, second window %d",
		want, writers, first, second.Int64())
	assert.EqualValues(t, want, ledger.total(),
		"a capture was lost or duplicated across the rename")
	assert.EqualValues(t, 0, settleSpendNow(t, rdb, "acct_1"), "the counter must be empty after the second settle")
}

// An empty window settles as a no-op. RENAME on a missing key is an ERROR in
// Redis, and an account that served no traffic is not an error — it is most
// accounts, most windows.
//
// This is the case the script's EXISTS guard exists for. Without it every idle
// account produces a settle failure, and a settler that logs a failure per idle
// account per window teaches its operator to ignore the log.
func TestSettle_AnEmptyWindowIsANoOpNotAnError(t *testing.T) {
	ctx := context.Background()
	rdb := newSettleRedis(t)
	ledger := newSettleLedger(true)

	// No keys at all.
	amount, err := Settle(ctx, rdb, "acct_never_used", ledger.writer())
	require.NoError(t, err, "an idle account must not be a settle failure")
	assert.EqualValues(t, 0, amount.Int64())
	assert.Equal(t, 0, ledger.writeCount(), "an empty window must not reach the writer")
	assert.Empty(t, scanStagingKeys(t, rdb), "an empty window must not leave a staging key")

	// And the same account immediately after a successful settle, which is the
	// state every settled account is left in.
	settleCapture(t, rdb, "acct_1", 42)
	first, err := Settle(ctx, rdb, "acct_1", ledger.writer())
	require.NoError(t, err)
	require.EqualValues(t, 42, first.Int64())

	again, err := Settle(ctx, rdb, "acct_1", ledger.writer())
	require.NoError(t, err)
	assert.EqualValues(t, 0, again.Int64())
	assert.Equal(t, 1, ledger.writeCount(), "the second settle of an idle account wrote a row")

	// Bare RENAME is what this replaces. Recorded so the reason for the script
	// is visible rather than asserted.
	err = rdb.Rename(ctx, spendKey("acct_never_used"), "anything").Err()
	require.Error(t, err)
	t.Logf("bare RENAME on a missing key: %v", err)
}

// Two settlers racing on the same account. Only one may win the RENAME, and
// the loser must not write anything.
//
// The RENAME is what serialises them, so nothing else has to: no lock, no
// leader election, no per-account coordination. That is the property being
// pinned, and it is the reason a second settler can be started safely.
func TestSettle_OnlyOneOfTwoRacingSettlersWinsTheRename(t *testing.T) {
	ctx := context.Background()

	t.Run("many settlers, one window", func(t *testing.T) {
		rdb := newSettleRedis(t)
		ledger := newSettleLedger(true)
		require.NoError(t, rdb.Set(ctx, spendKey("acct_1"), "1000", 0).Err())

		const settlers = 16
		results := make([]int64, settlers)
		errs := make([]error, settlers)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < settlers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				amount, err := Settle(ctx, rdb, "acct_1", ledger.writer())
				errs[i] = err
				if err == nil {
					results[i] = amount.Int64()
				}
			}(i)
		}
		close(start)
		wg.Wait()

		winners := 0
		for i := range results {
			require.NoError(t, errs[i], "settler %d", i)
			if results[i] != 0 {
				winners++
				assert.EqualValues(t, settleWindowCredits, results[i], "settler %d took a partial window", i)
			}
		}
		t.Logf("%d settlers raced: %d took the window, ledger holds %d over %d rows from %d writer calls",
			settlers, winners, ledger.total(), ledger.rowCount(), ledger.writeCount())

		assert.Equal(t, 1, winners, "more than one settler took the same window")
		assert.Equal(t, 1, ledger.writeCount(), "a losing settler wrote to the ledger")
		assert.EqualValues(t, settleWindowCredits, ledger.total())
		assert.Empty(t, scanStagingKeys(t, rdb), "a racing settler left a staging key behind")
	})

	// The same property stated without concurrency, so a failure says which
	// half broke.
	t.Run("the second settle of an already-taken window takes nothing", func(t *testing.T) {
		rdb := newSettleRedis(t)
		ledger := newSettleLedger(true)
		require.NoError(t, rdb.Set(ctx, spendKey("acct_1"), "1000", 0).Err())

		staging, amount := settleStageOnly(t, rdb, "acct_1")
		require.EqualValues(t, settleWindowCredits, amount)

		second, err := Settle(ctx, rdb, "acct_1", ledger.writer())
		require.NoError(t, err)
		assert.EqualValues(t, 0, second.Int64())
		assert.Equal(t, 0, ledger.writeCount())
		assert.True(t, settleKeyExists(t, rdb, staging), "the loser must not disturb the winner's staging key")
	})

	// Two recoveries racing on the SAME orphan. They both write, and the
	// idempotency is what makes that safe — the same property the crash needs.
	t.Run("two recoveries of one orphan write one row", func(t *testing.T) {
		rdb := newSettleRedis(t)
		ledger := newSettleLedger(true)
		require.NoError(t, rdb.Set(ctx, spendKey("acct_1"), "1000", 0).Err())
		_, _ = settleStageOnly(t, rdb, "acct_1")

		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := Recover(ctx, rdb, ledger.writer()); err != nil {
					t.Errorf("recover: %v", err)
				}
			}()
		}
		wg.Wait()

		t.Logf("4 concurrent recoveries: %d writer calls, %d rows, total %d",
			ledger.writeCount(), ledger.rowCount(), ledger.total())
		assert.Equal(t, 1, ledger.rowCount(), "concurrent recovery must collapse onto one row")
		assert.EqualValues(t, settleWindowCredits, ledger.total())
		assert.Empty(t, scanStagingKeys(t, rdb))
	})
}

// A writer that fails must leave the staging key in place, and the amount must
// still be there for the next recovery.
//
// This is the difference between a settle failure and a data loss. The amount
// is already out of the spend counter by the time the writer runs, so a settle
// that cleaned up after a failed write would delete the only copy.
func TestSettle_AWriterErrorLeavesTheStagingKeyAndLosesNothing(t *testing.T) {
	ctx := context.Background()
	rdb := newSettleRedis(t)

	failing := newSettleLedger(true)
	failing.fail = func(string, string) error { return fmt.Errorf("postgres is down") }
	require.NoError(t, rdb.Set(ctx, spendKey("acct_1"), "1000", 0).Err())

	_, err := Settle(ctx, rdb, "acct_1", failing.writer())
	require.Error(t, err)
	t.Logf("settle with a failing writer: %v", err)
	assert.ErrorContains(t, err, "postgres is down")
	assert.ErrorContains(t, err, "left in place", "the error must say the amount is safe")
	assert.EqualValues(t, 0, failing.total(), "a failed write must record nothing")

	staged := scanStagingKeys(t, rdb)
	require.Len(t, staged, 1, "the failed settle must leave exactly one staging key")
	held, err := rdb.Get(ctx, staged[0]).Int64()
	require.NoError(t, err)
	assert.EqualValues(t, settleWindowCredits, held, "the amount must survive the failed write intact")

	// The staging key must not expire out from under us. RENAME carries the
	// source key's TTL across, so a TTL on spend would silently delete money
	// this settle has already taken custody of. PERSIST is what stops that.
	ttl, err := rdb.TTL(ctx, staged[0]).Result()
	require.NoError(t, err)
	assert.Equal(t, time.Duration(-1), ttl, "the staging key must have no TTL")

	// Traffic keeps arriving against the account while the write is broken.
	settleCapture(t, rdb, "acct_1", settleConcurrentCredits)

	// A later recovery, with a working writer, loses nothing.
	working := newSettleLedger(true)
	done, err := Recover(ctx, rdb, working.writer())
	require.NoError(t, err)
	assert.Equal(t, 1, done)

	settled, err := Settle(ctx, rdb, "acct_1", working.writer())
	require.NoError(t, err)
	assert.EqualValues(t, settleConcurrentCredits, settled.Int64())

	t.Logf("after the writer recovered: ledger holds %d, true total %d", working.total(), settleTrueTotal)
	assert.EqualValues(t, settleTrueTotal, working.total())
	assert.Empty(t, scanStagingKeys(t, rdb))
}

// A TTL on the spend counter must not become a TTL on the money in flight.
//
// Nothing sets one today — authorize.lua expires the rate buckets and leaves
// spend alone — so this pins the PERSIST, not authorize.lua. If the PERSIST
// goes, this test says what breaks: an expiring staging key deletes an amount
// the ledger has not seen, with nothing going red.
func TestSettle_AStagedWindowNeverInheritsATTL(t *testing.T) {
	ctx := context.Background()
	rdb := newSettleRedis(t)

	require.NoError(t, rdb.Set(ctx, spendKey("acct_1"), "1000", time.Hour).Err())
	staging, amount := settleStageOnly(t, rdb, "acct_1")
	require.EqualValues(t, settleWindowCredits, amount)

	ttl, err := rdb.TTL(ctx, staging).Result()
	require.NoError(t, err)
	t.Logf("spend had a 1h TTL; the staged window's TTL is %v", ttl)
	assert.Equal(t, time.Duration(-1), ttl,
		"the staged amount inherited a TTL and will delete itself before the ledger sees it")
}

// INCRBY after the rename starts a fresh counter at the captured amount, not
// at the old total.
//
// This is the property that makes the next window a window rather than a
// running total. If RENAME left the source key behind at its old value — or if
// the settle copied instead of moving — the next window would re-bill
// everything already settled.
func TestSettle_IncrByAfterTheRenameStartsAFreshCounter(t *testing.T) {
	ctx := context.Background()
	rdb := newSettleRedis(t)
	ledger := newSettleLedger(true)

	require.NoError(t, rdb.Set(ctx, spendKey("acct_1"), "1000", 0).Err())
	_, amount := settleStageOnly(t, rdb, "acct_1")
	require.EqualValues(t, settleWindowCredits, amount)

	assert.False(t, settleKeyExists(t, rdb, spendKey("acct_1")),
		"the rename must MOVE the counter; a copy leaves the next window carrying the old total")

	settleCapture(t, rdb, "acct_1", settleConcurrentCredits)
	assert.EqualValues(t, settleConcurrentCredits, settleSpendNow(t, rdb, "acct_1"),
		"the next window started at the old total instead of at the captured amount")

	// And that is what the next settle reports.
	done, err := Recover(ctx, rdb, ledger.writer())
	require.NoError(t, err)
	require.Equal(t, 1, done)
	next, err := Settle(ctx, rdb, "acct_1", ledger.writer())
	require.NoError(t, err)
	assert.EqualValues(t, settleConcurrentCredits, next.Int64())
	assert.EqualValues(t, settleTrueTotal, ledger.total())
}

// Recovery enumerates orphans with SCAN, never KEYS, and finds every account.
//
// KEYS walks the keyspace inside one command and blocks the server, and this
// Redis serves authorize.lua on the request path — the settler's housekeeping
// would show up as customer latency. The command counters make the choice
// observable instead of a claim in a comment.
func TestRecover_SweepsEveryAccountWithScanAndNotKeys(t *testing.T) {
	ctx := context.Background()
	rdb := newSettleRedis(t)
	ledger := newSettleLedger(true)

	accounts := []string{"acct_1", "acct_2", "acct_3", "acct_with:colon", "acct_4"}
	for i, a := range accounts {
		require.NoError(t, rdb.Set(ctx, spendKey(a), fmt.Sprint((i+1)*100), 0).Err())
		_, _ = settleStageOnly(t, rdb, a)
	}
	// Noise the sweep must not touch: live counters, ceilings, rate buckets.
	require.NoError(t, rdb.Set(ctx, spendKey("acct_9"), "77", 0).Err())
	require.NoError(t, rdb.Set(ctx, ceilingKey("acct_9"), "5000", 0).Err())
	require.NoError(t, rdb.Set(ctx, "valve:rate:s:aaaa:1700000000", "3", 0).Err())

	require.NoError(t, rdb.ConfigResetStat(ctx).Err())
	done, err := Recover(ctx, rdb, ledger.writer())
	require.NoError(t, err)
	stats := rdb.Info(ctx, "commandstats").Val()

	assert.Equal(t, len(accounts), done)
	assert.EqualValues(t, 100+200+300+400+500, ledger.total())
	assert.Equal(t, len(accounts), ledger.rowCount(), "each account must land on its own row")
	assert.Empty(t, scanStagingKeys(t, rdb), "recovery must delete every staging key it wrote")

	assert.NotContains(t, stats, "cmdstat_keys:", "recovery used KEYS, which blocks the server")
	assert.Contains(t, stats, "cmdstat_scan:", "recovery did not use SCAN")
	t.Logf("recovery command mix: %s", strings.TrimSpace(settleCommandNames(stats)))

	// The noise is untouched.
	assert.EqualValues(t, 77, settleSpendNow(t, rdb, "acct_9"))
	assert.True(t, settleKeyExists(t, rdb, ceilingKey("acct_9")))
	assert.True(t, settleKeyExists(t, rdb, "valve:rate:s:aaaa:1700000000"))
}

// One failing account must not stop the sweep. The settler runs for the whole
// fleet, and a single account whose write keeps failing would otherwise hold
// up everybody else's money until somebody noticed.
func TestRecover_ContinuesPastAFailingAccountAndKeepsItsAmount(t *testing.T) {
	ctx := context.Background()
	rdb := newSettleRedis(t)

	ledger := newSettleLedger(true)
	ledger.fail = func(accountID, _ string) error {
		if accountID == "acct_bad" {
			return fmt.Errorf("this account's write is broken")
		}
		return nil
	}

	for _, a := range []string{"acct_1", "acct_bad", "acct_2"} {
		require.NoError(t, rdb.Set(ctx, spendKey(a), "100", 0).Err())
		_, _ = settleStageOnly(t, rdb, a)
	}

	done, err := Recover(ctx, rdb, ledger.writer())
	require.Error(t, err, "a failing account must be reported")
	t.Logf("sweep with one broken account: %d done, error: %v", done, err)
	assert.ErrorContains(t, err, "acct_bad")
	assert.Equal(t, 2, done, "the healthy accounts must still settle")
	assert.EqualValues(t, 200, ledger.total())

	staged := scanStagingKeys(t, rdb)
	require.Len(t, staged, 1, "only the failing account may be left staged")
	account, _, ok := parseStagingKey(staged[0])
	require.True(t, ok)
	assert.Equal(t, "acct_bad", account)
	held, err := rdb.Get(ctx, staged[0]).Int64()
	require.NoError(t, err)
	assert.EqualValues(t, 100, held, "the failing account's amount must survive the sweep")
}

// The staging key name is the only thing a crash leaves behind, so recovery
// has to read both the account and the window back out of it. This pins the
// round trip against the shape keys.go builds — a rename there fails here
// rather than silently making the sweep find nothing.
func TestStagingKey_RoundTripsThroughTheNameKeysGoBuilds(t *testing.T) {
	require.Equal(t, "valve:credits:acct_1:spend", spendKey("acct_1"),
		"keys.go changed the spend key shape; stagingScanPattern and parseStagingKey follow it by hand")

	for _, account := range []string{
		"acct_1",
		"acct:with:colons",
		"acct:settling:looks-like-the-infix",
		"a",
		"UPPER-and-lower_123",
	} {
		windowID, err := newWindowID()
		require.NoError(t, err)
		key := stagingKey(spendKey(account), windowID)

		gotAccount, gotWindow, ok := parseStagingKey(key)
		require.True(t, ok, "did not parse %s", key)
		assert.Equal(t, account, gotAccount)
		assert.Equal(t, windowID, gotWindow)
	}

	// Names that are not staging keys must not parse, so a sweep never
	// mistakes a live counter for money in flight.
	for _, key := range []string{
		spendKey("acct_1"),
		ceilingKey("acct_1"),
		"valve:credits:acct_1:spend:settling:", // no window id
		"valve:credits::spend:settling:abc",    // no account
		"something:else:settling:abc",
		"valve:credits:acct_1:ceiling:settling:abc", // the ceiling is not settled this way
	} {
		_, _, ok := parseStagingKey(key)
		assert.False(t, ok, "%s parsed as a staging key", key)
	}
}

// Window ids must differ between windows, or the idempotent write merges two
// windows into one row and loses the smaller one. This is the half of the id
// contract that generation is responsible for; the other half — being stable
// across a retry — is the key name's job, and the recovery tests above prove
// it.
func TestSettle_EveryWindowGetsItsOwnID(t *testing.T) {
	ctx := context.Background()
	rdb := newSettleRedis(t)
	ledger := newSettleLedger(true)

	const windows = 25
	for i := 0; i < windows; i++ {
		settleCapture(t, rdb, "acct_1", 10)
		amount, err := Settle(ctx, rdb, "acct_1", ledger.writer())
		require.NoError(t, err)
		require.EqualValues(t, 10, amount.Int64())
	}

	assert.Equal(t, windows, ledger.rowCount(),
		"two windows shared a window id and the upsert merged them")
	assert.EqualValues(t, windows*10, ledger.total())
}

// Settle must refuse the inputs that would clear a counter with nothing
// recording it.
func TestSettle_RefusesAnEmptyAccountOrAMissingWriter(t *testing.T) {
	ctx := context.Background()
	rdb := newSettleRedis(t)

	_, err := Settle(ctx, rdb, "", newSettleLedger(true).writer())
	assert.ErrorContains(t, err, "empty accountId")

	require.NoError(t, rdb.Set(ctx, spendKey("acct_1"), "1000", 0).Err())
	_, err = Settle(ctx, rdb, "acct_1", nil)
	assert.ErrorContains(t, err, "no writer")
	assert.EqualValues(t, settleWindowCredits, settleSpendNow(t, rdb, "acct_1"),
		"the counter must not move when the settle is refused")

	_, err = Recover(ctx, rdb, nil)
	assert.ErrorContains(t, err, "no writer")
}

// A staged value that is not an integer is refused, and the key is left alone.
// Coercing it would bill a number nobody chose; deleting it would destroy the
// evidence of whatever wrote it.
func TestSettle_RefusesAStagedValueItCannotRead(t *testing.T) {
	ctx := context.Background()
	rdb := newSettleRedis(t)
	ledger := newSettleLedger(true)

	bad := stagingKey(spendKey("acct_1"), "deadbeef")
	require.NoError(t, rdb.Set(ctx, bad, "not-a-number", 0).Err())

	done, err := Recover(ctx, rdb, ledger.writer())
	require.Error(t, err)
	t.Logf("recovery over an unreadable staged value: %v", err)
	assert.Equal(t, 0, done)
	assert.Equal(t, 0, ledger.writeCount())
	assert.True(t, settleKeyExists(t, rdb, bad), "an unreadable staged value must be left for a human")
}

func settleKeyExists(t *testing.T, rdb redis.Cmdable, key string) bool {
	t.Helper()
	n, err := rdb.Exists(context.Background(), key).Result()
	require.NoError(t, err)
	return n == 1
}

// settleCommandNames trims INFO commandstats down to the command names, so a log
// line is readable.
func settleCommandNames(info string) string {
	var names []string
	for _, line := range strings.Split(info, "\n") {
		if name, _, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && strings.HasPrefix(name, "cmdstat_") {
			names = append(names, strings.TrimPrefix(name, "cmdstat_"))
		}
	}
	return strings.Join(names, " ")
}
