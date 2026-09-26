package pool

import (
	"encoding/base64"
	"fmt"
	"testing"
	"time"
)

func TestTokenTimestampParsing(t *testing.T) {
	nowMs := time.Now().UnixMilli()
	raw := fmt.Sprintf("SG_WEB#3795d28242a11619bc25f786f84e53d4-h-%d-d7158d8b98e24d14a829499fb38b9006#extra", nowMs)
	b64 := base64.StdEncoding.EncodeToString([]byte(raw))

	ts, ok := tokenTimestamp(b64)
	if !ok {
		t.Fatalf("expected tokenTimestamp to parse, got ok=false")
	}
	if diff := time.Since(ts); diff > 2*time.Second || diff < -2*time.Second {
		t.Fatalf("unexpected diff: %v", diff)
	}
}

func TestPoolAutoDiscardExpiredToken(t *testing.T) {
	// Create an expired token (10 hours ago) and a fresh token (now)
	oldMs := time.Now().Add(-10 * time.Hour).UnixMilli()
	oldRaw := fmt.Sprintf("SG_WEB#fingerprint1-h-%d-salt1#extra", oldMs)
	oldTok := base64.StdEncoding.EncodeToString([]byte(oldRaw))

	nowMs := time.Now().UnixMilli()
	freshRaw := fmt.Sprintf("SG_WEB#fingerprint2-h-%d-salt2#extra", nowMs)
	freshTok := base64.StdEncoding.EncodeToString([]byte(freshRaw))

	p := &Pool{
		path:   t.TempDir() + "/tokens.json",
		tokens: []string{oldTok, freshTok},
	}
	// Pool.Take() should discard the old token and return the fresh token
	tok, ok := p.Take()
	if !ok {
		t.Fatalf("expected Take to succeed, got ok=false")
	}
	if tok != freshTok {
		t.Fatalf("expected fresh token, got %q", tok)
	}
	if p.Count() != 0 {
		t.Fatalf("expected pool to be empty, got count %d", p.Count())
	}
}
