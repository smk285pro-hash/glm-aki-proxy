package upstream

import (
	"context"
	"math/rand/v2"
	"os"
	"strconv"
	"sync"
	"time"
)

var (
	throttleSem   = make(chan struct{}, 1)
	stateMu       sync.Mutex
	lastRequestAt time.Time
)

// DefaultMinGap is the default pacing interval between upstream requests (2800ms).
const DefaultMinGap = 2800 * time.Millisecond

// GetMinGap returns the pacing duration configured via ANTHROPIC_ZAI_MIN_MS.
func GetMinGap() time.Duration {
	if v := os.Getenv("ANTHROPIC_ZAI_MIN_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return DefaultMinGap
}

// Throttle enforces the pacing gate before sending requests to Z.AI.
// If the caller cancels context during wait or before acquisition, it returns immediately without blocking.
func Throttle(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case throttleSem <- struct{}{}:
	}
	defer func() { <-throttleSem }()

	gap := GetMinGap()
	// Add 100-400ms random jitter to break machine-like periodicity for anti-WAF bot detection
	jitter := time.Duration(100+rand.IntN(300)) * time.Millisecond

	stateMu.Lock()
	prev := lastRequestAt
	stateMu.Unlock()

	wait := (gap + jitter) - time.Since(prev)
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}

	stateMu.Lock()
	lastRequestAt = time.Now()
	stateMu.Unlock()
	return nil
}

// RecordRequestDone records completion time of an upstream request.
func RecordRequestDone() {
	stateMu.Lock()
	defer stateMu.Unlock()
	lastRequestAt = time.Now()
}

// ResetThrottle resets the throttle timestamp (used in unit tests).
func ResetThrottle() {
	stateMu.Lock()
	defer stateMu.Unlock()
	lastRequestAt = time.Time{}
}

