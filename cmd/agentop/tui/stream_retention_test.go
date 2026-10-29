package tui

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/pipeline"
)

// TestStreamRetentionOffScreen: streamed events for sessions the user is not
// watching must not be retained (#1542). handleStreamEvent used to append
// every /v1/events arrival to m.events[sessionID] for EVERY session — never
// interned, never released — so an idle agentop grew at the wire rate
// (~18 MB/min measured live; 1.5 GB after 13h). This is the issue's fixture:
// 3 background sessions x 50 turns, each turn re-sending the whole
// conversation, into a model sitting on the Sessions pane.
func TestStreamRetentionOffScreen(t *testing.T) {
	m := newRetentionModel(t, "on-screen", 0)
	m.pane = paneSessions
	hp := func() uint64 {
		// Enough GCs to settle: a bare double-GC left suite garbage above the
		// 0.6 MB distinct-content bound once in a full-suite run.
		for i := 0; i < 5; i++ {
			runtime.GC()
		}
		var s runtime.MemStats
		runtime.ReadMemStats(&s)
		return s.HeapAlloc
	}
	base := hp()
	for n := 1; n <= 50; n++ {
		for k := 0; k < 3; k++ {
			msgs := make([]pipeline.InferenceMessage, n)
			for i := range msgs {
				msgs[i] = pipeline.InferenceMessage{Role: "user", Content: strings.Clone(fmt.Sprintf("s%d-m%d-", k, i) + strings.Repeat("x", 4096))}
			}
			m.handleStreamEvent(apiclient.StreamEvent{Event: &pipeline.SessionEvent{
				At: time.Now(), SessionID: fmt.Sprintf("bg-%d", k), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
				Inference: &pipeline.InferenceExtension{Messages: msgs},
			}})
		}
	}
	retained := hp() - base
	distinct := uint64(3 * 50 * 4110)
	t.Logf("heap +%.1f MB retained; distinct content %.1f MB", float64(retained)/1e6, float64(distinct)/1e6)
	// Before the fix this held 18.9 MB — 31x the distinct content. After it,
	// nothing is retained for sessions nobody is watching. The bound is a
	// fixed 2 MB rather than the distinct content (0.6 MB): the deterministic
	// assertion is held-events == 0 below, this one guards the heap, and a
	// full-suite run leaves more than 0.6 MB of uncollected garbage after
	// even five GCs. The bug's signature is 18.9 MB — an order above.
	if retained > 2<<20 {
		t.Errorf("off-screen streamed events retained %.1f MB; only the watched session may retain events (bug signature was 18.9 MB)",
			float64(retained)/1e6)
	}
	_ = distinct
	for _, id := range []string{"bg-0", "bg-1", "bg-2"} {
		if len(m.events[id]) != 0 {
			t.Errorf("session %s (never opened) holds %d streamed events", id, len(m.events[id]))
		}
	}
	// The summary the handler owes every session regardless of retention.
	if len(m.sessions) != 4 {
		t.Errorf("sessions = %d, want 4 (on-screen stub + 3 streamed)", len(m.sessions))
	}
}

// The same events for the WATCHED session are still retained — the
// pane-agnostic selectedSess, so walking events → sessions → events does not
// punch a gap in the live tail.
func TestStreamRetentionWatchedSessionStillAppends(t *testing.T) {
	m := newRetentionModel(t, "watched", 0)
	m.pane = paneSessions // not even the events pane — selectedSess is what counts
	for i := 0; i < 10; i++ {
		m.handleStreamEvent(apiclient.StreamEvent{Event: &pipeline.SessionEvent{
			At: time.Now(), SessionID: "watched", Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
		}})
	}
	if len(m.events["watched"]) != 10 {
		t.Errorf("watched session holds %d events, want 10", len(m.events["watched"]))
	}
}

// The timeline's session is watched too, even though openTimeline does not
// set selectedSess.
func TestStreamRetentionTimelineSessionStillAppends(t *testing.T) {
	m := newRetentionModel(t, "other", 0)
	m.pane = paneTimeline
	m.timeline.session = "tl-sess"
	for i := 0; i < 5; i++ {
		m.handleStreamEvent(apiclient.StreamEvent{Event: &pipeline.SessionEvent{
			At: time.Now(), SessionID: "tl-sess", Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
		}})
	}
	if len(m.events["tl-sess"]) != 5 {
		t.Errorf("timeline session holds %d events, want 5", len(m.events["tl-sess"]))
	}
}
