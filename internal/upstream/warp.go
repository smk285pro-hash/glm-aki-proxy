package upstream

import (
	"context"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"glm-aki-proxy/internal/session"
)

// WarpRotator manages automated Cloudflare WARP IP rotation when upstream WAF blocks are hit.
type WarpRotator struct {
	mu          sync.Mutex
	lastRotated time.Time
	cliPath     string
	enabled     bool
	cooldown    time.Duration
}

// NewWarpRotator creates and initializes a WarpRotator instance.
func NewWarpRotator() *WarpRotator {
	cli, err := exec.LookPath("warp-cli")
	hasCLI := err == nil

	enabled := hasCLI
	if env := strings.ToLower(strings.TrimSpace(os.Getenv("WARP_AUTO_ROTATE"))); env != "" {
		enabled = (env == "true" || env == "1" || env == "yes")
	}

	return &WarpRotator{
		cliPath:  cli,
		enabled:  enabled,
		cooldown: 15 * time.Second,
	}
}

// DefaultWarpRotator is the singleton rotator used across the upstream package.
var DefaultWarpRotator = NewWarpRotator()

// Enabled returns true if warp-cli is present and auto-rotation is active.
func (r *WarpRotator) Enabled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.enabled && r.cliPath != ""
}

// SetEnabled allows toggling the rotator dynamically.
func (r *WarpRotator) SetEnabled(enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enabled = enabled
}

// RotateIP triggers a quick WARP disconnect -> connect cycle to acquire a fresh egress IP.
// It enforces a 15-second cooldown to prevent excessive reconnection thrashing.
func (r *WarpRotator) RotateIP(ctx context.Context, pool *session.Pool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.enabled || r.cliPath == "" {
		return false
	}

	if time.Since(r.lastRotated) < r.cooldown {
		return false
	}

	log.Printf("[warp] WAF 405 block detected: rotating Cloudflare WARP IP (disconnect -> connect)...")
	r.lastRotated = time.Now()

	// 1. Disconnect
	discCmd := exec.CommandContext(ctx, r.cliPath, "disconnect")
	_ = discCmd.Run()

	select {
	case <-ctx.Done():
		return false
	case <-time.After(1 * time.Second):
	}

	// 2. Connect
	connCmd := exec.CommandContext(ctx, r.cliPath, "connect")
	if err := connCmd.Run(); err != nil {
		log.Printf("[warp] warp-cli connect error: %v", err)
		return false
	}

	select {
	case <-ctx.Done():
		return false
	case <-time.After(2 * time.Second):
	}

	// 3. Reset TLS connection pool to drop old sockets bound to the previous network interface
	if _, err := ResetDirectClient(); err != nil {
		log.Printf("[warp] warning resetting direct TLS client: %v", err)
	}

	// 4. Refresh anti-bot cookies via homepage scrape under the new IP
	if pool != nil {
		pool.Refresh()
	}

	log.Printf("[warp] Cloudflare WARP IP rotation completed successfully")
	return true
}
