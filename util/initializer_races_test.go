package util

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The two branches below exist only for the window between a task's state
// change and the publication of its done channel. Both are real: nothing in
// the Initializer holds a lock across that window, and MarkTaskAsFailed writes
// task state without taking tasksMu at all.

// Bug 70 pin. attemptRemainingTasks swaps a task to Running and only THEN
// stores the channel that signals the attempt's end. A Wait that landed between
// the two found a running task with no channel, and the branch it took was a
// bare `continue`: no context check, no sleep, no yield. Wait burned a whole
// core and could not be cancelled until another goroutine published the channel
// or moved the task to a terminal state.
//
// (Earlier comments here cited "bug 60". The entry is 70.)
//
// Both no-progress paths now use the same ctx-aware sleep, so a cancelled
// context ends the wait. The setup is deterministic, not a race: Running with
// no done channel is the exact window, and it cannot resolve itself.
func TestBootstrapTask_WaitInTheNoChannelWindowHonoursACancelledContext(t *testing.T) {
	t.Parallel()

	task := NewBootstrapTask("windowed", func(context.Context) error { return nil })
	// Running, with no done channel yet — the exact window.
	task.state.Store(int32(TaskRunning))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before Wait even starts

	waited := make(chan error, 1)
	go func() { waited <- task.Wait(ctx) }()

	select {
	case err := <-waited:
		require.ErrorIs(t, err, context.Canceled,
			"bug 70: the no-channel branch must honour a cancelled context")
	case <-time.After(5 * time.Second):
		t.Fatal("bug 70: Wait ignored a cancelled context in the no-channel window")
	}
}

// TestInitializer_ALostStateRaceStillReleasesTheStartBarrier proves that a
// task whose state changes under attemptRemainingTasks does not strand the
// caller.
//
// attemptRemainingTasks counts every task into one WaitGroup and then waits for
// all of them to start. When the compare-and-swap to Running loses to a
// concurrent MarkTaskAsFailed, no goroutine is launched for that task, so the
// skip path owns the wg.Done(). Miss it and wg.Wait() never returns — and
// because tasksMu is held across the whole function, every later ExecuteTasks
// and Stop on that Initializer blocks too. That is the same failure shape as
// the shutdown-race wedge already logged as bug 56.
//
// The race is driven, not hoped for: one goroutine rewrites the task's state
// continuously while the scheduler walks many registrations of it.
func TestInitializer_ALostStateRaceStillReleasesTheStartBarrier(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lg := zerolog.New(io.Discard)
	i := NewInitializer(ctx, &lg, &InitializerConfig{
		TaskTimeout:   10 * time.Second,
		AutoRetry:     false,
		RetryFactor:   1.5,
		RetryMinDelay: time.Millisecond,
		RetryMaxDelay: time.Millisecond,
	})

	var runs sync.WaitGroup
	task := NewBootstrapTask("racy", func(context.Context) error { return nil })
	// One task under many keys: the scheduler visits it once per key, so a
	// single writer racing those visits gets many chances to land inside the
	// load-then-swap window.
	const registrations = 2000
	for n := 0; n < registrations; n++ {
		i.tasks.Store(fmt.Sprintf("racy-%d", n), task)
	}

	stop := make(chan struct{})
	const writers = 4
	runs.Add(writers)
	for w := 0; w < writers; w++ {
		go func() {
			defer runs.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				task.state.Store(int32(TaskFailed))
				task.state.Store(int32(TaskPending))
			}
		}()
	}

	scheduled := make(chan struct{})
	go func() {
		defer close(scheduled)
		i.attemptRemainingTasks(false)
	}()

	select {
	case <-scheduled:
	case <-time.After(30 * time.Second):
		close(stop)
		runs.Wait()
		t.Fatal("attemptRemainingTasks never released its start barrier; tasksMu is now held forever")
	}
	close(stop)
	runs.Wait()

	// The Initializer must still be usable: tasksMu was released, so a second
	// scheduling pass returns too.
	second := make(chan struct{})
	go func() {
		defer close(second)
		i.attemptRemainingTasks(false)
	}()
	select {
	case <-second:
	case <-time.After(30 * time.Second):
		t.Fatal("the Initializer is wedged: a later scheduling pass cannot take tasksMu")
	}

	assert.NotZero(t, task.attempts.Load(), "at least one visit must have started the task")
}
