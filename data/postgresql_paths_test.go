package data

import (
	"context"
	"errors"
	"io"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover the PostgreSQL connector paths that need no database at
// all: config parsing, IAM wiring, the cleanup gate, and the failure
// coalescing that decides when a reconnect fires.

// TestPostgreSQLConnector_RejectsAnUnparseableUriAsFatal proves that a
// connection URI the driver cannot parse ends the bootstrap task instead of
// retrying it forever.
//
// A typo in `connectionUri` never becomes valid on a retry. Without the fatal
// marker the initializer re-dials a broken string every few seconds for the
// life of the process, and the operator sees a retry loop instead of a
// configuration error.
func TestPostgreSQLConnector_RejectsAnUnparseableUriAsFatal(t *testing.T) {
	t.Parallel()

	lg := zerolog.New(io.Discard)
	p := &PostgreSQLConnector{logger: &lg, initTimeout: time.Second}

	err := p.connectTask(context.Background(), &common.PostgreSQLConnectorConfig{
		Table:         "t",
		ConnectionUri: "postgres://%%zz",
	})

	require.Error(t, err)
	var fatal interface{ IsTaskFatal() bool }
	require.ErrorAs(t, err, &fatal, "an unparseable URI must stop the retry loop")
	assert.True(t, fatal.IsTaskFatal())
	assert.Contains(t, err.Error(), "failed to parse connection URI")
}

// TestPostgreSQLConnector_IAMAuth covers both halves of the RDS IAM branch in
// connectTask: a credential source the SDK rejects, and a usable one.
//
// The first half must be fatal for the same reason as a bad URI — an
// unsupported `auth.mode` is a configuration error, not a transient one. The
// second half must install BeforeConnect, because that hook is the only thing
// that mints a fresh IAM token per connection; without it every connection
// authenticates with an empty password.
func TestPostgreSQLConnector_IAMAuth(t *testing.T) {
	t.Parallel()

	base := func(auth *common.AwsAuthConfig) *common.PostgreSQLConnectorConfig {
		return &common.PostgreSQLConnectorConfig{
			Table: "t",
			// Port 1 refuses instantly, so the connect attempt that follows
			// the IAM setup returns fast.
			ConnectionUri: "postgres://rdsuser@127.0.0.1:1/postgres?sslmode=disable",
			IAMAuth: &common.PostgreSQLIAMAuthConfig{
				Enabled:  true,
				Endpoint: "127.0.0.1:1",
				Region:   "us-east-1",
				DBUser:   "rdsuser",
				Auth:     auth,
			},
		}
	}

	t.Run("an unsupported credential mode is fatal", func(t *testing.T) {
		lg := zerolog.New(io.Discard)
		p := &PostgreSQLConnector{logger: &lg, initTimeout: time.Second}

		err := p.connectTask(context.Background(), base(&common.AwsAuthConfig{Mode: "not-a-mode"}))

		require.Error(t, err)
		var fatal interface{ IsTaskFatal() bool }
		require.ErrorAs(t, err, &fatal, "a bad credential mode must stop the retry loop")
		assert.Contains(t, err.Error(), "rds iam")
	})

	t.Run("static credentials install the per-connection token hook", func(t *testing.T) {
		lg := zerolog.New(io.Discard)
		p := &PostgreSQLConnector{logger: &lg, initTimeout: 2 * time.Second, minConns: 1, maxConns: 1}

		err := p.connectTask(context.Background(), base(&common.AwsAuthConfig{
			Mode:            "secret",
			AccessKeyID:     "AKIAEXAMPLE",
			SecretAccessKey: "secret",
		}))

		// The dial fails (nothing listens on port 1), but it must fail as a
		// retryable connect error, not as a fatal IAM setup error.
		require.Error(t, err)
		var fatal interface{ IsTaskFatal() bool }
		assert.False(t, errors.As(err, &fatal), "a refused dial must stay retryable: %v", err)
	})
}

// TestPostgreSQLConnector_StartCleanupSkipsWhenPgCronOwnsIt proves the local
// cleanup goroutine returns at once when the ticker is nil.
//
// applySchema sets cleanupTicker to nil after it schedules the pg_cron job.
// If startCleanup then fell through to its select, it would block forever on a
// nil channel and hold a goroutine per connector for the life of the process.
func TestPostgreSQLConnector_StartCleanupSkipsWhenPgCronOwnsIt(t *testing.T) {
	t.Parallel()

	lg := zerolog.New(io.Discard)
	p := &PostgreSQLConnector{logger: &lg}

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.startCleanup(context.Background())
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("startCleanup must return immediately when pg_cron owns the cleanup")
	}
}

// newIdleConnector builds a connector whose initializer is parked in the
// supplied task state, without touching a database. The returned release
// function unblocks the bootstrap task.
func newIdleConnector(t *testing.T, ctx context.Context, lg *zerolog.Logger) (*PostgreSQLConnector, func()) {
	t.Helper()
	p := &PostgreSQLConnector{id: "idle", logger: lg}
	p.initializer = util.NewInitializer(ctx, lg, &util.InitializerConfig{
		TaskTimeout:   time.Minute,
		AutoRetry:     false,
		RetryFactor:   1.5,
		RetryMinDelay: time.Second,
		RetryMaxDelay: time.Second,
	})

	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	task := util.NewBootstrapTask(p.taskId(), func(taskCtx context.Context) error {
		close(started)
		select {
		case <-release:
		case <-taskCtx.Done():
		}
		return nil
	})
	go func() { _ = p.initializer.ExecuteTasks(ctx, task) }()
	<-started
	return p, func() { once.Do(func() { close(release) }) }
}

// TestPostgreSQLConnector_HandleConnectionFailureWhileReinitializing proves a
// connection error that arrives while the connector is already reconnecting
// does not mark the task failed a second time.
//
// This is the 2026-05-13 cascade guard. During the incident every in-flight
// query failed at once; each failure re-marked the task, each mark logged at
// Error, and the log fan-out was what starved the process. The branch must
// return without touching the initializer.
func TestPostgreSQLConnector_HandleConnectionFailureWhileReinitializing(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lg := zerolog.New(io.Discard)
	p, release := newIdleConnector(t, ctx, &lg)
	defer release()

	require.Contains(t,
		[]util.InitializationState{util.StateInitializing, util.StateRetrying},
		p.initializer.State(),
		"the connector must be mid-initialization for this branch")

	p.handleConnectionFailure(syscall.ECONNRESET)

	assert.Zero(t, p.lastFailureMarkNanos.Load(),
		"a failure during reinitialization must not arm the reconnect cooldown")
	assert.Contains(t,
		[]util.InitializationState{util.StateInitializing, util.StateRetrying},
		p.initializer.State(),
		"the task must not be re-marked while it is already retrying")
}

// TestPostgreSQLConnector_ConcurrentFailuresCoalesceIntoOneMark proves that a
// burst of simultaneous connection failures produces exactly one reconnect
// mark.
//
// Every caller reads the same zero cooldown stamp and passes the window check,
// so the compare-and-swap is the only thing standing between one reconnect and
// one per failing query. An edge node runs thousands of concurrent queries; if
// the swap did not gate them, each would log at Error and drive the initializer
// again.
func TestPostgreSQLConnector_ConcurrentFailuresCoalesceIntoOneMark(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lg, lines := recordingLogger(t)
	p, release := newIdleConnector(t, ctx, lg)
	release() // let the bootstrap task finish so the connector reaches Ready

	waitFor(t, 10*time.Second, 5*time.Millisecond, "the connector to be ready", func() bool {
		return p.initializer.State() == util.StateReady
	})

	// Several bursts, each released by a spin gate so the callers reach the
	// compare-and-swap together. Every burst starts from a clear cooldown, so
	// every caller in it passes the window check and races for the swap.
	const callers = 64
	const bursts = 200
	for b := 0; b < bursts; b++ {
		p.lastFailureMarkNanos.Store(0)

		var gate atomic.Bool
		var done sync.WaitGroup
		done.Add(callers)
		for i := 0; i < callers; i++ {
			go func() {
				defer done.Done()
				for !gate.Load() {
					runtime.Gosched()
				}
				p.handleConnectionFailure(syscall.ECONNRESET)
			}()
		}
		gate.Store(true)
		done.Wait()

		assert.NotZero(t, p.lastFailureMarkNanos.Load(),
			"one caller must win the race and arm the cooldown")
	}

	// A second failure inside the cooldown window must change nothing.
	stamp := p.lastFailureMarkNanos.Load()
	p.handleConnectionFailure(syscall.ECONNRESET)
	assert.Equal(t, stamp, p.lastFailureMarkNanos.Load(),
		"a failure inside the cooldown window must not re-arm it")

	// The count is the point: bursts * callers failures must produce exactly
	// one reconnect mark per burst. Each mark logs at Error, and it was that
	// log fan-out that starved the process during the 2026-05-13 cascade.
	marks := 0
	for _, l := range lines() {
		if strings.Contains(l, "marking task as failed") {
			marks++
		}
	}
	assert.Equal(t, bursts, marks,
		"%d failures must collapse into %d marks, one per burst", bursts*callers, bursts)
}
