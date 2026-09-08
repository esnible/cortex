package session

import (
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// TestNoExpiryByDefault: ttl <= 0 means sessions are never dropped for being idle.
// Time-based expiry read as data loss — traffic vanished because someone stepped away,
// not because anything overflowed.
func TestNoExpiryByDefault(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Minute} {
		s := New(ttl, 500, 100)
		defer s.Close()
		s.Append("s1", pipeline.SessionEvent{})

		// Pretend a very long idle period by backdating the entry.
		s.mu.Lock()
		s.sessions["s1"].UpdatedAt = time.Now().Add(-72 * time.Hour)
		s.mu.Unlock()

		if v := s.View("s1"); v == nil {
			t.Errorf("ttl=%v: session gone after 72h idle; it must not expire on time", ttl)
		}
		if got := len(s.ListSessions()); got != 1 {
			t.Errorf("ttl=%v: List() = %d sessions, want 1", ttl, got)
		}
		s.Cleanup() // explicit sweep must also spare it
		if v := s.View("s1"); v == nil {
			t.Errorf("ttl=%v: Cleanup() deleted a session that cannot expire", ttl)
		}
	}
}

// TestExplicitTTLStillExpires: the capability is not gone, only the default.
func TestExplicitTTLStillExpires(t *testing.T) {
	s := New(30*time.Minute, 500, 100)
	defer s.Close()
	s.Append("s1", pipeline.SessionEvent{})

	s.mu.Lock()
	s.sessions["s1"].UpdatedAt = time.Now().Add(-31 * time.Minute)
	s.mu.Unlock()

	if v := s.View("s1"); v != nil {
		t.Error("an explicitly configured ttl no longer expires idle sessions")
	}
}

// TestNoReaperWhenNothingCanExpire: with ttl <= 0 the interval clamps to one second, so
// a reaper would wake every second forever to find nothing.
func TestNoReaperWhenNothingCanExpire(t *testing.T) {
	s := New(0, 500, 100)
	// Close() closes s.stop; a running backgroundCleanup would return on it. The point
	// here is simply that New with ttl=0 does not panic on a non-positive ticker and
	// that Close is safe whether or not the goroutine was started.
	s.Close()
	s.Close() // idempotent: must not panic on a double close
}

// TestSizeCapsStillBound: removing time expiry must not remove the memory bound.
func TestSizeCapsStillBound(t *testing.T) {
	s := New(0, 10, 3)
	defer s.Close()

	for i := 0; i < 25; i++ {
		s.Append("s1", pipeline.SessionEvent{})
	}
	if v := s.View("s1"); v != nil && len(v.Events) > 10 {
		t.Errorf("maxEvents ignored: %d events retained, cap is 10", len(v.Events))
	}
	for _, id := range []string{"s2", "s3", "s4", "s5"} {
		s.Append(id, pipeline.SessionEvent{})
	}
	if got := len(s.ListSessions()); got > 3 {
		t.Errorf("maxSessions ignored: %d sessions retained, cap is 3", got)
	}
}
