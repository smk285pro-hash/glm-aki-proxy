package upstream

import (
	"context"
	"testing"
	"time"
)

func TestWarpRotator_CooldownAndState(t *testing.T) {
	rotator := &WarpRotator{
		cliPath:  "fake-warp-cli",
		enabled:  true,
		cooldown: 50 * time.Millisecond,
	}

	if !rotator.Enabled() {
		t.Fatalf("expected rotator to be enabled")
	}

	rotator.SetEnabled(false)
	if rotator.Enabled() {
		t.Fatalf("expected rotator to be disabled after SetEnabled(false)")
	}

	rotator.SetEnabled(true)
	rotator.lastRotated = time.Now()

	// Should reject rotation during cooldown
	rotated := rotator.RotateIP(context.Background(), nil)
	if rotated {
		t.Fatalf("expected RotateIP to return false during cooldown")
	}

	// Advance past cooldown
	time.Sleep(60 * time.Millisecond)
	// Even though it advances past cooldown, fake-warp-cli will fail to run disconnect, returning false safely without panic
	_ = rotator.RotateIP(context.Background(), nil)
}
