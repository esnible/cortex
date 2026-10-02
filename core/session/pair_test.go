package session

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

func pairEvent(requestID string, tunnel bool) pipeline.SessionEvent {
	return pipeline.SessionEvent{
		At: time.Now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
		RequestID: requestID, Tunnel: tunnel,
	}
}

// AppendPair exists for a reader that pairs an event with the one after it — agentop
// folds a bridged tunnel's open into the next event in the session — so no concurrent
// append may ever land between the two.
func TestAppendPair_NothingLandsBetweenThePair(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	const n = 200
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			id := fmt.Sprint("pair-", i)
			s.AppendPair("sess-1", pairEvent(id, true), pairEvent(id, false))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			s.Append("sess-1", pairEvent(fmt.Sprint("other-", i), false))
		}
	}()
	wg.Wait()

	evs := s.View("sess-1").Events
	if len(evs) != 3*n {
		t.Fatalf("%d events, want %d", len(evs), 3*n)
	}
	opens := 0
	for i, e := range evs {
		if !e.Tunnel {
			continue
		}
		opens++
		if i+1 >= len(evs) || evs[i+1].Tunnel || evs[i+1].RequestID != e.RequestID {
			t.Fatalf("event %d (%s) is not directly followed by its pair", i, e.RequestID)
		}
		if evs[i+1].Seq != e.Seq+1 {
			t.Errorf("pair %s has seqs %d and %d, want consecutive", e.RequestID, e.Seq, evs[i+1].Seq)
		}
	}
	if opens != n {
		t.Errorf("%d first-of-pair events, want %d", opens, n)
	}
}

// The returned bucket is the one both events went into, so a later trailing event —
// a tunnel's close — lands beside them, and an adopted pending id is followed exactly
// as Append follows it.
func TestAppendPair_ReturnsTheBucketBothWentInto(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	pending := PendingSessionID("claude-code")
	s.Append(pending, pairEvent("probe", false))
	s.Claim("sess-1", "claude-code") // adopts the pending bucket into sess-1

	b := s.AppendPair(pending, pairEvent("p", true), pairEvent("p", false))
	if !s.AppendTrailing(b, trailingEvent()) {
		t.Fatal("AppendTrailing refused the bucket AppendPair returned")
	}
	v := s.View("sess-1")
	if v == nil || len(v.Events) != 4 {
		t.Fatalf("sess-1 = %+v, want the adopted probe, the pair and the trailing event", v)
	}
	if !v.Events[1].Tunnel || v.Events[2].Tunnel {
		t.Errorf("events 1 and 2 are not the pair in order: %+v", v.Events[1:3])
	}
	if s.View(pending) != nil {
		t.Error("AppendPair re-created the adopted pending bucket")
	}
	if got := s.ActiveSession(); got != "sess-1" {
		t.Errorf("ActiveSession() = %q, want sess-1: a pair is that session speaking", got)
	}
}
