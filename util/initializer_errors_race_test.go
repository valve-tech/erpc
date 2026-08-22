package util

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Errors() must survive being read while a task retries.
//
// It used to call Error() twice — a nil check and then a dereference. The
// initializer stores wrappedError{nil} before every attempt, so a retry
// landing between the two makes the second call return nil and `.Err`
// panics. That is the same defect bug 170 fixed in waitForTasks; the fix
// landed on one of the two sites and left this one.
//
// This site is worse than the shutdown path 170 covers. Errors() is what a
// caller polls WHILE waiting for a connector to come up — see
// data/postgres_test.go, which calls it inside a require.Eventually — so a
// retrying task and a reading caller are the normal case, not a rare one.
// It crashed a full-suite run on 2026-08-22, in exactly that test.
func TestInitializer_ErrorsSurvivesAReadDuringARetry(t *testing.T) {
	// The window is one retry's width, so a single read almost never hits
	// it. Many readers against a 1µs retry loop close it in well under a
	// second.
	const readers = 8

	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	init := setupInitializer(t, appCtx, &InitializerConfig{
		TaskTimeout:   50 * time.Millisecond,
		AutoRetry:     true,
		RetryMinDelay: time.Microsecond,
		RetryMaxDelay: time.Microsecond,
	})

	// The task always fails, so the retry loop keeps re-entering the window
	// where lastErr holds wrappedError{nil}.
	_ = init.ExecuteTasks(appCtx, NewBootstrapTask("never-succeeds",
		func(ctx context.Context) error { return errors.New("keeps the retry loop busy") }))

	// Recover inside each reader. A nil deref here is a panic in a goroutine
	// that is not the test's own, which kills the whole test binary — every
	// other test in the package then reports nothing, and the crash reads as
	// an unrelated failure. Recovering turns it into an attributable one.
	var wg sync.WaitGroup
	deadline := time.Now().Add(2 * time.Second)
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Errors() panicked while a task was retrying: %v", r)
				}
			}()
			for time.Now().Before(deadline) {
				_ = init.Errors()
			}
		}()
	}
	wg.Wait()

	cancel()
	_ = init.Stop(nil)
}
