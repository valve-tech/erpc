package util

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// afterStopWriter records anything written to it once the test says Stop has
// returned. It stands in for whatever the caller tears down next — in a real
// process the writer behind the logger, in a test t.Log after tRunner has
// finished.
type afterStopWriter struct {
	mu        sync.Mutex
	stopped   bool
	afterStop []string
}

func (w *afterStopWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		w.afterStop = append(w.afterStop, string(p))
	}
	return len(p), nil
}

func (w *afterStopWriter) markStopped() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
}

func (w *afterStopWriter) writesAfterStop() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.afterStop...)
}

// A task goroutine that outlives Stop must not write to the caller's logger.
//
// Bug 109, second half. Stop waits one task-timeout for in-flight tasks and
// then returns anyway — it has to, because a task whose Fn ignores its context
// cannot be interrupted. That goroutine kept the logger it borrowed and went
// on emitting "initialization task failed" for a component the operator had
// already destroyed.
//
// The fix cannot end the goroutine. It stops the initializer from USING the
// caller's logger once Stop is over, which is the half the initializer owns.
//
// The task below ignores its context on purpose. That is not a contrived
// input: it is exactly the misbehaving task this entry is about, and a
// well-behaved one always finishes inside TaskTimeout and never reaches the
// branch.
func TestInitializer_AnAbandonedTaskStopsWritingToTheCallersLogger(t *testing.T) {
	writer := &afterStopWriter{}
	logger := zerolog.New(writer).Level(zerolog.TraceLevel)

	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	init := NewInitializer(appCtx, &logger, &InitializerConfig{
		TaskTimeout:   200 * time.Millisecond,
		AutoRetry:     false,
		RetryMinDelay: time.Hour,
		RetryMaxDelay: time.Hour,
	})

	// The task ignores tctx and outlives the whole stop sequence, so its
	// terminal log line is written well after Stop has returned.
	released := make(chan struct{})
	task := NewBootstrapTask("ignores-its-context", func(ctx context.Context) error {
		<-released
		return nil
	})

	// Register and launch the task WITHOUT ExecuteTasks. ExecuteTasks blocks
	// until its tasks end, and this test's whole point is a task that has not
	// ended — so calling it needs a detached goroutine, and that goroutine
	// then logs "initialization failed" through the caller's logger on its way
	// out. That write is legitimate: in production the caller of ExecuteTasks
	// is alive and blocked inside the call, so its own logger is still its
	// own. Only the TASK goroutine outlives its caller, and only it is under
	// test here.
	init.tasks.Store(task.Name, task)
	init.attemptRemainingTasks(false)
	require.Equal(t, TaskRunning, TaskState(task.state.Load()),
		"attemptRemainingTasks returns once the task has started")

	// Stop gives up after TaskTimeout+100ms and reports that it abandoned the
	// task — that is bug 109's first half, already fixed, and it is the
	// precondition for this one.
	err := init.Stop(nil)
	require.Error(t, err, "Stop must report that it abandoned a running task")
	require.Contains(t, err.Error(), "abandoned")

	// From here the caller is entitled to tear its logger down.
	writer.markStopped()

	// Now let the task finish. Its post-body log line runs here, and the task
	// publishes its terminal state only AFTER that line — so once the state
	// has moved, the write has already happened or it never will.
	close(released)
	require.Eventually(t, func() bool {
		return TaskState(task.state.Load()) != TaskRunning
	}, 5*time.Second, 10*time.Millisecond, "the abandoned task never reached a terminal state")

	require.Empty(t, writer.writesAfterStop(),
		"a task goroutine wrote to the caller's logger after Stop returned")
}
