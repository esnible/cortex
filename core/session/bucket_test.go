package session

import (
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

func trailingEvent() pipeline.SessionEvent {
	return pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Outbound, Phase: pipeline.SessionResponse, Tunnel: true}
}

// Adopt renames the pending bucket the way Rekey renames default, so a trailing event
// follows it into the claimed session rather than re-creating the pending bucket.
func TestAppendTrailing_FollowsAnAdoption(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	pending := PendingSessionID("claude-code")
	b := s.AppendBucket(pending, pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest})
	s.Claim("sess-1", "claude-code")

	if !s.AppendTrailing(b, trailingEvent()) {
		t.Fatal("AppendTrailing refused a bucket whose session was adopted, not removed")
	}
	if v := s.View("sess-1"); v == nil || len(v.Events) != 2 {
		t.Errorf("sess-1 = %+v, want the adopted open and the trailing event", v)
	}
	if v := s.View(pending); v != nil {
		t.Errorf("the pending bucket was re-created holding %d event(s)", len(v.Events))
	}
}

// An expired session is gone to every reader before cleanup gets to it, so a trailing
// event must not land in it either.
func TestAppendTrailing_RefusesAnExpiredSession(t *testing.T) {
	s := New(20*time.Millisecond, 0, 0)
	defer s.Close()
	b := s.AppendBucket("sess-1", pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest})
	time.Sleep(50 * time.Millisecond)

	if s.AppendTrailing(b, trailingEvent()) {
		t.Error("AppendTrailing appended to a session past its ttl")
	}
}

func TestAppendTrailing_NilBucketRecordsNothing(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	if s.AppendTrailing(nil, trailingEvent()) {
		t.Error("AppendTrailing reported success for a nil bucket")
	}
	if n := len(s.ListSessions()); n != 0 {
		t.Errorf("a nil bucket created %d session(s)", n)
	}
}
