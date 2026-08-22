package util

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bytedance/sonic"
	"github.com/rs/zerolog"
)

type InitializationState int

const (
	StateUninitialized InitializationState = iota
	StateInitializing
	StatePartial
	StateRetrying
	StateReady
	StateFailed
	StateFatal
)

func (s InitializationState) String() string {
	return []string{"uninitialized", "initializing", "partial", "retrying", "ready", "failed", "fatal"}[s]
}

type TaskState int

const (
	TaskPending TaskState = iota
	TaskRunning
	TaskSucceeded
	TaskTimedOut
	TaskFailed
	TaskFatal
)

func (s TaskState) String() string {
	return []string{"pending", "running", "succeeded", "timedOut", "failed", "fatal"}[s]
}

type BootstrapTask struct {
	Name        string
	Fn          func(ctx context.Context) error // Must respect ctx.Done()
	state       atomic.Int32                    // TaskState
	lastErr     atomic.Value                    // error
	lastAttempt atomic.Value                    // time.Time
	ctxCancel   atomic.Value                    // context.CancelFunc
	doneVal     atomic.Value                    // chan struct{}
	attempts    atomic.Int32
}

func NewBootstrapTask(name string, fn func(ctx context.Context) error) *BootstrapTask {
	t := &BootstrapTask{
		Name: name,
		Fn:   fn,
	}
	return t
}

type TaskError struct {
	TaskName  string
	Err       error
	Timestamp time.Time
	Attempt   int
}

type wrappedError struct {
	err error
}

func (t *BootstrapTask) Error() *TaskError {
	wr, _ := t.lastErr.Load().(wrappedError)
	if wr.err == nil {
		return nil
	}
	// Comma-ok, not a bare type assertion. lastAttempt is empty until
	// beginAttempt runs, and an error can be recorded without one — Wait
	// substitutes a reason for a terminal failure that never recorded its own.
	// A bare assertion panics there; a zero Timestamp is the honest answer.
	ts, _ := t.lastAttempt.Load().(time.Time)
	return &TaskError{
		TaskName:  t.Name,
		Err:       wr.err,
		Timestamp: ts,
		Attempt:   int(t.attempts.Load()),
	}
}

// createNewDoneChannel re-creates the done channel for a fresh attempt.
// Must be called only after a successful CompareAndSwap to TaskRunning.
func (t *BootstrapTask) createNewDoneChannel() chan struct{} {
	newCh := make(chan struct{})
	t.doneVal.Store(newCh)
	return newCh
}

// Wait blocks until the task reaches a TERMINAL state, or ctx ends.
//
// "An attempt ended" is not "the task ended". With auto-retry on, a failed
// attempt is followed by another, and only the loop condition at the top of
// this function decides that the task is done. Two defects came from confusing
// the two:
//
//   - Bug 157. The old code returned as soon as the attempt's done channel
//     closed. It read the state on the next line to decide what to return, and
//     the auto-retry loop could claim a new attempt in between — so the state
//     read `TaskRunning`, the `== TaskFailed` test was false, and Wait returned
//     nil for a task that had failed every attempt and was failing again.
//     waitForTasks then saw a non-failed state, recorded nothing, and
//     ExecuteTasks reported success. A caller on the request path was told a
//     bootstrap finished cleanly when nothing had.
//   - Bug 157, second half. Even on the branch that DID see TaskFailed, the
//     empty-error substitution was stored in lastErr and then `return wr.err`
//     returned the original nil. That one was not a race: a failed task with no
//     recorded error reported success every time.
//
// Both disappear by looping instead of returning. The terminal check at the top
// re-reads lastErr, so the substituted error is what a caller gets.
//
//   - Bug 70. The no-channel branch used to `continue` with no context check
//     and no yield whenever the state was not Pending. attemptRemainingTasks
//     swaps a task to Running BEFORE it publishes the attempt's done channel, so
//     a Wait landing between the two spun a whole core and ignored cancellation.
//     Both no-progress paths now use the same ctx-aware sleep.
func (t *BootstrapTask) Wait(ctx context.Context) error {
	// The attempt whose end we already observed. The retry loop swaps the state
	// to Running before it publishes the next attempt's channel, so doneVal can
	// still hold the closed channel we just consumed. Selecting on it again
	// would return instantly and spin — bug 70's failure mode, reached by a
	// different route.
	var consumed chan struct{}

	for {
		state := TaskState(t.state.Load())
		switch state {
		case TaskSucceeded:
			// The initializer stores wrappedError{nil} before every attempt, so a
			// success cannot be carrying a previous attempt's error.
			return nil
		case TaskFailed, TaskTimedOut, TaskFatal:
			if wr, _ := t.lastErr.Load().(wrappedError); wr.err != nil {
				return wr.err
			}
			// A terminal FAILURE must never answer nil. It used to, whenever the
			// attempt recorded no reason — the cancellation path, for one, drops
			// its reason (see TestInitializer_CancelledTaskIsReportedWithoutItsReason).
			// waitForTasks then found task.Error() == nil, recorded nothing, and
			// ExecuteTasks reported success for a bootstrap that never happened.
			return errors.New("task failed without specific error")
		}

		ch, _ := t.doneVal.Load().(chan struct{})
		if ch == nil || ch == consumed {
			// No attempt to wait on yet, or the next one is not published. Sleep
			// rather than spin, and stay cancellable while doing it.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Millisecond):
				continue
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
			consumed = ch
			// Record a failure that carries no reason, so the terminal check at
			// the top has something to return. Do NOT return here: this attempt
			// ending says nothing about whether the TASK has ended.
			if TaskState(t.state.Load()) == TaskFailed {
				if wr, _ := t.lastErr.Load().(wrappedError); wr.err == nil {
					t.lastErr.Store(wrappedError{err: errors.New("task failed without specific error")})
				}
			}
		}
	}
}

// attempt is called just before a new attempt to run t.Fn.
func (t *BootstrapTask) beginAttempt() {
	t.attempts.Add(1)
	t.lastAttempt.Store(time.Now())
}

type InitializerConfig struct {
	TaskTimeout   time.Duration
	AutoRetry     bool
	RetryFactor   float64
	RetryMinDelay time.Duration
	RetryMaxDelay time.Duration
}

type Initializer struct {
	appCtx   context.Context
	logger   *zerolog.Logger
	attempts atomic.Int32
	tasks    sync.Map
	tasksMu  sync.Mutex

	autoRetryActive atomic.Bool
	cancelAutoRetry atomic.Value // context.CancelFunc
	autoRetryWg     sync.WaitGroup

	conf *InitializerConfig

	StateUpdates chan InitializationState
}

func NewInitializer(appCtx context.Context, logger *zerolog.Logger, conf *InitializerConfig) *Initializer {
	if conf == nil {
		conf = &InitializerConfig{
			TaskTimeout:   120 * time.Second,
			AutoRetry:     true,
			RetryFactor:   1.5,
			RetryMinDelay: 3 * time.Second,
			RetryMaxDelay: 130 * time.Second,
		}
	}
	return &Initializer{
		appCtx:          appCtx,
		logger:          logger,
		attempts:        atomic.Int32{},
		autoRetryActive: atomic.Bool{},
		conf:            conf,
	}
}

// Schedules tasks for execution (does not block).
// The caller is typically responsible for calling WaitForTasks after this returns.
func (i *Initializer) ExecuteTasks(ctx context.Context, tasks ...*BootstrapTask) error {
	if len(tasks) == 0 {
		return nil
	}

	i.tasksMu.Lock()
	tasksToWait := make([]*BootstrapTask, 0, len(tasks))
	for _, task := range tasks {
		actual, existed := i.tasks.LoadOrStore(task.Name, task)
		bts := actual.(*BootstrapTask)
		i.logger.Debug().Bool("existed", existed).Int32("state", bts.state.Load()).Str("task", task.Name).Msg("executing task")
		tasksToWait = append(tasksToWait, bts)
	}
	i.tasksMu.Unlock()

	i.ensureAutoRetryIfEnabled()
	i.attemptRemainingTasks(true)

	return i.waitForTasks(ctx, tasksToWait...)
}

func (i *Initializer) WaitForTasks(ctx context.Context) error {
	allTasks := []*BootstrapTask{}
	i.tasks.Range(func(key, value interface{}) bool {
		allTasks = append(allTasks, value.(*BootstrapTask))
		return true
	})
	return i.waitForTasks(ctx, allTasks...)
}

// Wait for a set of tasks to complete or ctx to expire.
func (i *Initializer) waitForTasks(ctx context.Context, tasks ...*BootstrapTask) error {
	var errs []error
	for _, task := range tasks {
		if err := task.Wait(ctx); err != nil {
			// Wait returns TWO kinds of error now. Since bug 157 it returns the
			// task's own error when the task reaches a terminal failed state —
			// it used to return nil there, which is how a failed bootstrap was
			// reported as success. Only a context error means "give up on the
			// remaining tasks"; a task failure falls through and is recorded
			// below, so the aggregate "N/M tasks failed" message survives.
			if ctx.Err() != nil {
				return err
			}
		}
		// If task is failed, record that error.
		//
		// Call Error() ONCE. It used to be called twice — a nil check and then a
		// dereference — and the initializer stores wrappedError{nil} before every
		// attempt, so a retry landing between the two turned the second call into
		// a nil deref. That is a SIGSEGV on the shutdown path. It stayed hidden
		// while Wait returned early for task errors and this line was rarely
		// reached; fixing bug 157 made it reachable and it crashed at once.
		if TaskState(task.state.Load()) == TaskFailed {
			if te := task.Error(); te != nil {
				errs = append(errs, te.Err)
			}
		}
	}
	if len(errs) > 0 {
		total := len(tasks)
		i.logger.Warn().Errs("tasks", errs).Msgf("initialization failed: %d/%d tasks failed", len(errs), total)
		// %w on a joined error, not %v on the slice. %v renders the causes into
		// text and the chain stops here, so errors.Is(err, context.Canceled)
		// answers false for an aggregate that plainly contains it. That did not
		// show before bug 157 was fixed, because a task error used to leave Wait
		// as a bare early return and never reached this line.
		return fmt.Errorf("initialization failed: %d/%d tasks failed: %w", len(errs), total, errors.Join(errs...))
	}
	return nil
}

// retryBackoff returns the minimum delay that must elapse after a task's last
// attempt before it may run again. It mirrors the auto-retry loop's schedule
// (RetryMinDelay growing by RetryFactor, capped at RetryMaxDelay) so the
// request path and the background loop pace re-attempts identically.
func (i *Initializer) retryBackoff(attempts int32) time.Duration {
	minDelay := i.conf.RetryMinDelay
	maxDelay := i.conf.RetryMaxDelay
	factor := i.conf.RetryFactor
	if minDelay <= 0 {
		minDelay = 3 * time.Second
	}
	if maxDelay <= 0 {
		maxDelay = 130 * time.Second
	}
	if factor < 1 {
		factor = 1.5
	}
	if attempts < 1 {
		return minDelay
	}
	d := float64(minDelay) * math.Pow(factor, float64(attempts-1))
	if d >= float64(maxDelay) {
		return maxDelay
	}
	return time.Duration(d)
}

// taskReadyForRetry reports whether a failed/timed-out task's backoff has
// elapsed since its last attempt. A task with no recorded attempt is always
// ready (it has effectively never run).
func (i *Initializer) taskReadyForRetry(t *BootstrapTask) bool {
	lastAttempt, ok := t.lastAttempt.Load().(time.Time)
	if !ok || lastAttempt.IsZero() {
		return true
	}
	return time.Since(lastAttempt) >= i.retryBackoff(t.attempts.Load())
}

// attemptRemainingTasks tries to run any tasks in Pending, Failed or TimedOut states again.
// This function must use appContext to avoid premature cancellation of tasks when caller context is cancelled.
// The correct way to enforce timeout is to pass appropriate context to "waitForTasks()" function.
// To enforce timeout of task execution set proper TaskTimeout in InitializerConfig.
// To cancel a running task, use MarkTaskAsFailed() function instead.
//
// respectBackoff gates re-attempts of already-failed/timed-out tasks behind
// their per-task retry backoff. The request path (ExecuteTasks) sets it true so
// a flood of requests for a not-yet-ready network cannot re-execute a failing
// task on every request. The auto-retry loop sets it false because it already
// paces itself, so it remains the authoritative driver of retry cadence.
func (i *Initializer) attemptRemainingTasks(respectBackoff bool) {
	i.tasksMu.Lock()
	defer i.tasksMu.Unlock()

	// wg counts LAUNCHED goroutines only: one Add next to each `go`, one Done
	// on that goroutine's only path. It used to count every task the walk
	// looked at, with a Done in each skip branch and one more inside the
	// goroutine — and the goroutine had an early return that skipped its Done.
	// The wait below then never ended, with i.tasksMu still held through the
	// deferred unlock, so every later caller on this Initializer was stranded
	// too. Keep the Add and the Done adjacent to the launch, and there is no
	// bookkeeping left to get wrong.
	wg := sync.WaitGroup{}
	i.tasks.Range(func(key, value interface{}) bool {
		t := value.(*BootstrapTask)
		state := TaskState(t.state.Load())
		if state != TaskPending && state != TaskFailed && state != TaskTimedOut {
			return true
		}

		// Gate re-attempts of already-failed/timed-out tasks behind their
		// retry backoff. ExecuteTasks (hence this function) runs on every
		// request for a not-yet-ready network, so without this gate a
		// permanently-failing task (e.g. a lazy-loaded network that resolves
		// to zero upstreams) is re-executed on every single request — burning
		// CPU and flooding logs. Pending tasks have never run, so they always
		// start immediately. The auto-retry loop passes respectBackoff=false
		// because it already paces its own cadence.
		if respectBackoff && (state == TaskFailed || state == TaskTimedOut) && !i.taskReadyForRetry(t) {
			return true
		}

		// Claim the attempt: [Pending|Failed|Timeout] -> Running.
		// #nosec G115 - We know TaskState is small enough that int->int32 won't overflow
		if !t.state.CompareAndSwap(int32(state), int32(TaskRunning)) {
			return true
		}
		t.beginAttempt()
		t.lastErr.Store(wrappedError{err: nil})

		// A fresh done channel signals this attempt's completion. The
		// CompareAndSwap above is what guarantees exactly one closer.
		doneCh := t.createNewDoneChannel()

		if err := i.appCtx.Err(); err != nil {
			// The app is shutting down. Fail the attempt right here instead of
			// launching a goroutine that can only report the same thing: a
			// task that never starts cannot fail to report that it started.
			//
			// This branch used to live inside the goroutine, and it returned
			// without calling Done. The wait below then never ended, and it
			// holds i.tasksMu through the deferred unlock. One Initializer
			// serves every network and upstream, and GetNetwork calls
			// ExecuteTasks on the request path, so a single task scheduled
			// after shutdown stranded every later caller and shutdown itself.
			t.lastErr.Store(wrappedError{err: err})
			t.state.Store(int32(TaskFailed))
			close(doneCh)
			i.logger.Warn().Str("task", t.Name).Err(err).Msg("initialization task context error")
			return true
		}

		wg.Add(1)
		go func(bt *BootstrapTask, doneCh chan struct{}) {
			defer close(doneCh)

			tctx, cancel := context.WithTimeout(i.appCtx, i.conf.TaskTimeout)
			bt.ctxCancel.Store(cancel)
			// The task has started. Nothing above this line can fail or return
			// early, so this Done always runs.
			wg.Done()

			// Each branch below publishes the terminal state LAST, after the
			// error and the log line it summarises. A watcher that sees a
			// terminal state has therefore seen every side effect of the
			// attempt, which is what Wait, State and Stop already assume. With
			// the state published first, a caller could observe "succeeded",
			// tear the component down, and only then have the task goroutine
			// write to the logger it borrowed.
			err := bt.Fn(tctx)
			if err == nil {
				// If the function returns nil but context says we're canceled, treat it as an error
				err = tctx.Err()
			}

			if err != nil {
				// Detect fatal control errors without importing the common package to avoid cycles
				var fatal interface{ IsTaskFatal() bool }
				if errors.As(err, &fatal) {
					// Fatal errors should stop retries
					// Unwrap underlying error if available
					underlying := err
					if uw, ok := err.(interface{ Unwrap() error }); ok && uw.Unwrap() != nil {
						underlying = uw.Unwrap()
					}
					bt.lastErr.Store(wrappedError{err: underlying})
					// Log the underlying fatal error
					i.logger.Error().Str("task", bt.Name).Err(underlying).Msg("initialization task fatal error")
					bt.state.Store(int32(TaskFatal))
					return
				}
				// If context is cancelled there will be a reason already set for it on lastErr
				if !errors.Is(err, context.Canceled) {
					if cause := context.Cause(tctx); cause != nil {
						err = cause
					}
					bt.lastErr.Store(wrappedError{err: err})
				} else {
					bt.lastErr.CompareAndSwap(nil, wrappedError{err: err})
				}
				i.logger.Warn().Str("task", bt.Name).Err(err).Msg("initialization task failed")
				bt.state.Store(int32(TaskFailed))
			} else {
				bt.lastErr.Store(wrappedError{err: nil})
				lastAttempt, _ := bt.lastAttempt.Load().(time.Time)
				i.logger.Info().Str("task", bt.Name).Dur("durationMs", time.Since(lastAttempt)).Msg("initialization task succeeded")
				bt.state.Store(int32(TaskSucceeded))
			}
		}(t, doneCh)
		return true
	})

	// Wait for tasks to "start" running. To wait for them to finish, use WaitForTasks()
	wg.Wait()
}

func (i *Initializer) State() InitializationState {
	var total, pending, running, succeeded, failed, fatal int
	i.tasks.Range(func(key, value interface{}) bool {
		t := value.(*BootstrapTask)
		state := TaskState(t.state.Load())
		switch state {
		case TaskPending:
			pending++
		case TaskRunning:
			running++
		case TaskSucceeded:
			succeeded++
		case TaskFailed:
			failed++
		case TaskFatal:
			fatal++
		}
		total++
		return true
	})
	i.logger.Trace().
		Int32("attempts", i.attempts.Load()).
		Int("total", total).
		Int("pending", pending).
		Int("running", running).
		Int("succeeded", succeeded).
		Int("failed", failed).
		Int("fatal", fatal).
		Msg("calculating initialization state")

	if total == succeeded {
		return StateReady
	}
	// If any fatal exists, prefer Fatal state
	if fatal > 0 {
		return StateFatal
	}
	// If all tasks are done (some are failed, none running or pending), it's a "Failed" state
	if failed > 0 && (pending+running+succeeded == 0) {
		return StateFailed
	}
	if failed > 0 && (pending+running == 0) {
		return StatePartial
	}
	// If we've tried multiple times but still have tasks not succeeded
	atp := i.attempts.Load()
	if atp > 1 && (pending > 0 || running > 0) {
		return StateRetrying
	}
	return StateInitializing
}

func (i *Initializer) Status() *InitializerStatus {
	state := i.State()
	return &InitializerStatus{
		State: state,
		Tasks: i.tasksStatus(),
	}
}

func (i *Initializer) Errors() error {
	var errs []error
	i.tasks.Range(func(key, value interface{}) bool {
		t := value.(*BootstrapTask)
		// Call Error() ONCE — the same defect fixed in waitForTasks above,
		// at its surviving twin. The initializer stores wrappedError{nil}
		// before every attempt, so a retry landing between the nil check and
		// the dereference makes the second call return nil and `.Err` panics.
		//
		// This one crashes a READ, not the shutdown path: Errors() is what a
		// caller polls while waiting for a connector to come up, so a retrying
		// task and a polling caller are the normal case, not a rare one.
		if te := t.Error(); te != nil {
			errs = append(errs, te.Err)
		}
		return true
	})
	return errors.Join(errs...)
}

func (i *Initializer) MarkTaskAsFailed(name string, err error) {
	i.logger.Error().Str("task", name).Err(err).Msg("marking task as failed")
	i.tasks.Range(func(key, value interface{}) bool {
		t := value.(*BootstrapTask)
		if t.Name == name {
			previousState := TaskState(t.state.Swap(int32(TaskFailed)))
			if previousState == TaskRunning {
				if ctxCancel, ok := t.ctxCancel.Load().(context.CancelFunc); ok && ctxCancel != nil {
					ctxCancel()
				}
			}
			t.lastErr.Store(wrappedError{err: err})
			return false
		}
		return true
	})

	i.ensureAutoRetryIfEnabled()
}

func (i *Initializer) Stop(destroyFn func() error) error {
	i.logger.Debug().Msg("stopping initializer")

	// Cancel the auto-retry loop and wait for it to exit BEFORE taking tasksMu.
	//
	// The loop calls attemptRemainingTasks, which takes tasksMu itself. Waiting
	// for the loop while holding that lock is a deadlock: the goroutine cannot
	// acquire the mutex, so it never returns, so autoRetryWg never drains, so
	// Stop never returns and never releases the mutex. Nothing breaks the
	// cycle — Stop has no timeout on this wait.
	//
	// `go test -race ./util/ -count=6` hit it: Stop sat at this Wait for 39
	// minutes holding tasksMu while the retry goroutine sat on the Lock. In
	// production the same cycle hangs shutdown forever, so an orchestrator has
	// to kill the process after its grace period.
	if cancel := i.cancelAutoRetry.Load(); cancel != nil {
		cancel.(context.CancelFunc)()
	}
	i.autoRetryWg.Wait()

	i.tasksMu.Lock()
	defer i.tasksMu.Unlock()

	// Now, wait for any tasks that might still be running to finish or fail.
	waitCtx, waitCancel := context.WithTimeout(i.appCtx, i.conf.TaskTimeout+100*time.Millisecond)
	defer waitCancel()

	// WaitForTasks will block until all tasks have ended (either succeeded or failed).
	if err := i.WaitForTasks(waitCtx); err != nil {
		i.logger.Warn().Err(err).Msg("failed waiting for tasks to finish within the stop sequence")
	}

	var err error
	if destroyFn != nil {
		err = destroyFn()
	}

	// Bug 109, first half. Stop used to log the timeout and return only
	// destroyFn's error, so a caller could not tell a clean stop from an
	// abandoned one. It now reports what happened and lets the caller decide,
	// rather than deciding for the caller that a timeout is survivable.
	//
	// The test is waitCtx, NOT the wait error. Since bug 157 was fixed
	// WaitForTasks also reports tasks that FAILED, and a failed task is not an
	// abandoned stop — the initializer stopped cleanly, the task simply did not
	// succeed. Only the deadline means goroutines are still running.
	if waitCtx.Err() != nil {
		return errors.Join(
			fmt.Errorf("stop abandoned tasks that were still running: %w", waitCtx.Err()),
			err,
		)
	}
	return err
}

type TaskStatus struct {
	Name        string
	State       TaskState
	Err         error
	LastAttempt time.Time
	Attempts    int
}

func (s *TaskStatus) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(map[string]interface{}{
		"name":        s.Name,
		"state":       s.State.String(),
		"err":         s.Err,
		"lastAttempt": s.LastAttempt,
		"attempts":    s.Attempts,
	})
}
func (i *Initializer) tasksStatus() []TaskStatus {
	var statuses []TaskStatus
	i.tasks.Range(func(key, value interface{}) bool {
		t := value.(*BootstrapTask)
		lastAttempt, _ := t.lastAttempt.Load().(time.Time)
		var errVal error
		if ev := t.lastErr.Load(); ev != nil {
			wr, _ := ev.(wrappedError)
			errVal = wr.err
		}
		statuses = append(statuses, TaskStatus{
			Name:        t.Name,
			State:       TaskState(t.state.Load()),
			Err:         errVal,
			LastAttempt: lastAttempt,
			Attempts:    int(t.attempts.Load()),
		})
		return true
	})
	return statuses
}

// RangeTaskStates calls fn(name, state) for each registered task. Return
// false from fn to stop iteration early.
//
// Allocation-free streaming alternative to `Status().Tasks` for callers
// that only need (name, state) and don't want to materialize the full
// `[]TaskStatus`. Pprof on prod showed `tasksStatus`'s growslice +
// per-task TaskStatus allocs at ~10% CPU during the bootstrap-wait
// window, where `summarizeNetworkTasks` was calling Status() every
// 200ms and immediately throwing away the Err / LastAttempt / Attempts
// fields it didn't need.
func (i *Initializer) RangeTaskStates(fn func(name string, state TaskState) bool) {
	i.tasks.Range(func(_, value any) bool {
		t := value.(*BootstrapTask)
		return fn(t.Name, TaskState(t.state.Load()))
	})
}

type InitializerStatus struct {
	State     InitializationState
	LastError error
	Tasks     []TaskStatus
}

func (s *InitializerStatus) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(map[string]interface{}{
		"state":     s.State.String(),
		"lastError": s.LastError,
		"tasks":     s.Tasks,
	})
}

// Start background auto-retry, if configured
func (i *Initializer) ensureAutoRetryIfEnabled() {
	if !i.conf.AutoRetry {
		return
	}
	if i.autoRetryActive.Load() {
		return
	}
	i.autoRetryActive.Store(true)

	rctx, cancel := context.WithCancel(i.appCtx)
	i.cancelAutoRetry.Store(cancel)

	// Add to wait group
	i.autoRetryWg.Add(1)
	go func() {
		defer i.autoRetryWg.Done()
		i.logger.Debug().Msg("initializer auto-retry loop started")
		i.autoRetryLoop(rctx)
		i.logger.Debug().Msg("initializer auto-retry loop finished")
	}()
}

// hasPendingWork reports whether any registered task is still in a non-terminal
// state (pending, running, failed, or timed-out) and could therefore benefit
// from another attempt. Only succeeded and fatal tasks are terminal.
//
// This is the auto-retry loop's stop condition. It deliberately does NOT key off
// State(): State() returns StateFatal as soon as ANY task is fatal, so keying
// the loop off it makes a single permanently-failing task (e.g. an upstream with
// a chainId mismatch) end auto-retry for every sibling task in the same
// initializer. Because one Initializer is shared across many independent
// resources (one bootstrap task per network/upstream), that stranded every
// not-yet-initialized resource until the process restarted.
func (i *Initializer) hasPendingWork() bool {
	pending := false
	i.tasks.Range(func(_, value interface{}) bool {
		switch TaskState(value.(*BootstrapTask).state.Load()) {
		case TaskPending, TaskRunning, TaskFailed, TaskTimedOut:
			pending = true
			return false // found one; stop iterating
		}
		return true
	})
	return pending
}

// Continually attempt tasks until every task is terminal (succeeded or fatal)
// or the context is canceled.
func (i *Initializer) autoRetryLoop(ctx context.Context) {
	if cancel := i.cancelAutoRetry.Load(); cancel != nil {
		defer cancel.(context.CancelFunc)()
	}
	// Nothing to retry once every task is terminal. A fatal task must not end
	// the loop on its own — recoverable siblings must keep retrying.
	if !i.hasPendingWork() {
		i.autoRetryActive.Store(false)
		return
	}

	delay := i.conf.RetryMinDelay
	// Wait for the first delay before doing the first retry
	<-time.After(delay)
	for {
		if ctx.Err() != nil {
			i.logger.Debug().Err(ctx.Err()).Msg("initialization auto-retry interrupted")
			i.autoRetryActive.Store(false)
			return
		}
		i.attempts.Add(1)
		i.attemptRemainingTasks(false)
		err := i.WaitForTasks(ctx)
		state := i.State()
		// Stop only once no task can benefit from another attempt (every task
		// succeeded or is fatal). Fatal tasks are skipped by
		// attemptRemainingTasks, so a permanently-failing task cannot wedge the
		// retries of its still-recoverable siblings.
		if !i.hasPendingWork() {
			i.autoRetryActive.Store(false)
			return
		}
		if err != nil {
			i.logger.Warn().Err(err).Str("state", state.String()).Msgf("initialization auto-retry failed, will retry in %v", delay)
		}

		select {
		case <-ctx.Done():
			i.logger.Debug().Err(ctx.Err()).Msg("initialization auto-retry cancelled")
			i.autoRetryActive.Store(false)
			return
		case <-time.After(delay):
		}

		delay = time.Duration(float64(delay) * i.conf.RetryFactor)
		if delay > i.conf.RetryMaxDelay {
			delay = i.conf.RetryMaxDelay
		} else if delay < i.conf.RetryMinDelay {
			delay = i.conf.RetryMinDelay
		}
	}
}
