package session

import (
	"fmt"
	"testing"
)

func TestPoolRoundRobinAndCooldown(t *testing.T) {
	p := NewPool()
	s1 := &Session{userID: "user_1", userName: "Acc1", ready: true}
	s2 := &Session{userID: "user_2", userName: "Acc2", ready: true}
	s3 := &Session{userID: "user_3", userName: "Acc3", ready: true}
	p.sessions = []*Session{s1, s2, s3}

	if got := p.TotalCount(); got != 3 {
		t.Fatalf("want 3 accounts, got %d", got)
	}
	if got := p.HealthyCount(); got != 3 {
		t.Fatalf("want 3 healthy, got %d", got)
	}

	// 1. Round-robin picks s1, s2, s3, s1...
	p1 := p.Pick()
	if p1 != s1 {
		t.Fatalf("expected s1 first, got %v", p1.Name())
	}
	p2 := p.Pick()
	if p2 != s2 {
		t.Fatalf("expected s2 second, got %v", p2.Name())
	}
	p3 := p.Pick()
	if p3 != s3 {
		t.Fatalf("expected s3 third, got %v", p3.Name())
	}
	p4 := p.Pick()
	if p4 != s1 {
		t.Fatalf("expected s1 fourth, got %v", p4.Name())
	}

	// 2. Report capacity error on s2 -> enters cooldown
	p.ReportError(s2, fmt.Errorf("z.ai: Model is currently at capacity"))
	if !s2.InCooldown() {
		t.Fatalf("s2 should be in cooldown")
	}
	if got := p.HealthyCount(); got != 2 {
		t.Fatalf("want 2 healthy after s2 cooldown, got %d", got)
	}

	// 3. Picking should now skip s2 and cycle between s1 and s3
	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		picked := p.Pick()
		if picked == s2 {
			t.Fatalf("s2 is in cooldown, should not be picked while others are healthy")
		}
		seen[picked.Name()]++
	}
	if seen["Acc1"] == 0 || seen["Acc3"] == 0 {
		t.Fatalf("expected both Acc1 and Acc3 to be picked, got %+v", seen)
	}

	// 4. Test HasAlternative
	if !p.HasAlternative(s1) {
		t.Fatalf("s1 should have alternative (s3)")
	}

	// 5. If all accounts enter cooldown, pick the one exiting earliest
	p.ReportError(s1, fmt.Errorf("busy"))
	p.ReportError(s3, fmt.Errorf("busy"))
	if got := p.HealthyCount(); got != 0 {
		t.Fatalf("want 0 healthy, got %d", got)
	}
	// All in cooldown, but Pick must not panic and must return the earliest
	pickedFallback := p.Pick()
	if pickedFallback == nil {
		t.Fatalf("expected a fallback session, got nil")
	}
}

func TestPoolSingleAccountFallback(t *testing.T) {
	p := NewPool()
	s1 := &Session{userID: "user_single", userName: "Single", ready: true}
	p.sessions = []*Session{s1}

	if p.HasAlternative(s1) {
		t.Fatalf("single account should have no alternative")
	}
	if p.Pick() != s1 {
		t.Fatalf("expected s1")
	}
}

func TestPoolReportFatalErrorAndUserBlocked(t *testing.T) {
	p := NewPool()
	s := &Session{userID: "user_blocked", userName: "BlockedUser", ready: true}
	p.sessions = []*Session{s}

	// 1. Test ReportFatalError
	p.ReportFatalError(s, fmt.Errorf("account rejected: USER_BLOCKED"))
	if s.ready {
		t.Fatalf("session must not be ready after ReportFatalError")
	}
	if !s.InCooldown() {
		t.Fatalf("session must be in cooldown after ReportFatalError")
	}
	if p.HealthyCount() != 0 {
		t.Fatalf("healthy count must be 0")
	}

	// 2. Test MarkFatal
	s2 := &Session{userID: "user_fatal", userName: "FatalUser", ready: true}
	s2.MarkFatal()
	if s2.ready || !s2.InCooldown() {
		t.Fatalf("session must be not ready and in cooldown after MarkFatal")
	}

	// 3. Test ReportError with USER_BLOCKED string
	s3 := &Session{userID: "user_blocked_err", userName: "BlockedErr", ready: true}
	p.sessions = []*Session{s3}
	p.ReportError(s3, fmt.Errorf("upstream rejected: USER_BLOCKED"))
	if s3.ready || !s3.InCooldown() {
		t.Fatalf("session must be disabled after USER_BLOCKED error")
	}
}

func TestSharedDoerRegistration(t *testing.T) {
	prev := GetSharedDoer()
	defer RegisterSharedDoer(prev)

	RegisterSharedDoer(nil)
	if got := GetSharedDoer(); got != nil {
		t.Fatalf("expected nil doer after registering nil")
	}
}

