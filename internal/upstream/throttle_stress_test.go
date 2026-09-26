package upstream

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

// TestThrottlePacingConcurrentBursts stress tests that concurrent requests
// are strictly serialized and separated by at least ANTHROPIC_ZAI_MIN_MS.
func TestThrottlePacingConcurrentBursts(t *testing.T) {
	// Use 50ms for fast testing
	os.Setenv("ANTHROPIC_ZAI_MIN_MS", "50")
	defer os.Unsetenv("ANTHROPIC_ZAI_MIN_MS")
	ResetThrottle()

	const numGoroutines = 5
	var wg sync.WaitGroup
	startSignal := make(chan struct{})
	timestamps := make([]time.Time, numGoroutines)
	var mu sync.Mutex

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startSignal
			ctx := context.Background()
			if err := Throttle(ctx); err != nil {
				t.Errorf("Throttle unexpected error: %v", err)
				return
			}
			now := time.Now()
			mu.Lock()
			timestamps = append(timestamps, now)
			mu.Unlock()
		}()
	}

	// Release all goroutines at the exact same instant
	close(startSignal)
	wg.Wait()

	// Filter non-zero timestamps and sort
	var nonZero []time.Time
	for _, ts := range timestamps {
		if !ts.IsZero() {
			nonZero = append(nonZero, ts)
		}
	}
	if len(nonZero) != numGoroutines {
		t.Fatalf("expected %d timestamps, got %d", numGoroutines, len(nonZero))
	}

	// Sort timestamps chronologically
	for i := 0; i < len(nonZero)-1; i++ {
		for j := i + 1; j < len(nonZero); j++ {
			if nonZero[j].Before(nonZero[i]) {
				nonZero[i], nonZero[j] = nonZero[j], nonZero[i]
			}
		}
	}

	// Verify pacing between consecutive completions
	minExpectedGap := 40 * time.Millisecond // allowing 10ms timer jitter
	for i := 1; i < len(nonZero); i++ {
		gap := nonZero[i].Sub(nonZero[i-1])
		if gap < minExpectedGap {
			t.Errorf("burst violation between #%d and #%d: gap was %v, expected >= %v",
				i-1, i, gap, minExpectedGap)
		}
	}
}

// TestThrottleContextCancellationWhileLocked tests whether a cancelled context
// immediately aborts when another goroutine is currently holding the throttle gate.
func TestThrottleContextCancellationWhileLocked(t *testing.T) {
	// Set 500ms pacing gap
	os.Setenv("ANTHROPIC_ZAI_MIN_MS", "500")
	defer os.Unsetenv("ANTHROPIC_ZAI_MIN_MS")
	ResetThrottle()

	// 1. Prime the throttle so lastRequestAt is set
	if err := Throttle(context.Background()); err != nil {
		t.Fatalf("prime throttle failed: %v", err)
	}

	// 2. Goroutine A starts Throttle with a 500ms wait
	started := make(chan struct{})
	go func() {
		close(started)
		_ = Throttle(context.Background())
	}()

	<-started
	// Give Goroutine A 20ms to acquire throttleMu and enter select/timer
	time.Sleep(20 * time.Millisecond)

	// 3. Goroutine B attempts Throttle with an ALREADY CANCELLED context
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel() // Cancelled immediately

	startB := time.Now()
	errB := Throttle(cancelCtx)
	elapsedB := time.Since(startB)

	if !errors.Is(errB, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", errB)
	}

	// Goroutine B should abort immediately (< 50ms) because its context is already cancelled.
	// If it hangs for ~450ms+ waiting for Goroutine A to release throttleMu, that is a blocking flaw.
	t.Logf("Goroutine B (already cancelled) took %v to return from Throttle", elapsedB)
	if elapsedB > 100*time.Millisecond {
		t.Errorf("FAIL: Throttle with cancelled context blocked for %v (expected < 100ms) because throttleMu was held by another goroutine", elapsedB)
	}
}
