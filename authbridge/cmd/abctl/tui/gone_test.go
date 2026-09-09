package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
)

// The regression this whole file exists for.
//
// Nearly every deployment of the single-agent shape names its session "default"
// (session.DefaultSessionID). Restart the proxy and /v1/sessions returns an empty
// list — not an error — until traffic arrives. The old reconcile read that as
// "the server does not know these sessions" and deleted every cached event 2s
// later, then threw the user back to the sessions pane. Since the store is
// in-memory and per-pod, abctl's copy was the only one, so the events the user
// was reading were gone for good, without them doing anything.
func TestEmptySessionList_KeepsEventsAndPane(t *testing.T) {
	m := newTestGoneModel(t, session.DefaultSessionID)

	m.Update(sessionsLoadedMsg{})

	if got := len(m.events[session.DefaultSessionID]); got != 3 {
		t.Errorf("events were deleted on an empty server list: got %d, want 3", got)
	}
	if m.pane != paneEvents {
		t.Errorf("pane changed under the user: got %v, want paneEvents", m.pane)
	}
	if m.selectedSess != session.DefaultSessionID {
		t.Errorf("selection cleared: got %q", m.selectedSess)
	}
	if m.gone[session.DefaultSessionID] != goneRestart {
		t.Errorf("an empty list should read as a restart, got %v", m.gone[session.DefaultSessionID])
	}
}

// A session missing while OTHERS are still listed is an eviction, not a restart —
// different cause, different advice in the banner, same retention.
func TestMissingWhileOthersLive_MarksEvicted(t *testing.T) {
	m := newTestGoneModel(t, "old")

	m.Update(sessionsLoadedMsg{{ID: "fresh", UpdatedAt: time.Now()}})

	if len(m.events["old"]) != 3 {
		t.Error("evicted session's cached events were deleted")
	}
	if m.gone["old"] != goneEvicted {
		t.Errorf("want goneEvicted, got %v", m.gone["old"])
	}
}

// The notice must name the real mechanism. Time-based expiry is OFF by default
// (session.ttl defaults to never), so "expired" would be actively misleading —
// it is what the original issue guessed, and it sent the diagnosis the wrong way.
func TestGoneBanner_DoesNotClaimExpiry(t *testing.T) {
	for _, r := range []goneReason{goneRestart, goneEvicted} {
		b := goneBanner(r, 200)
		if strings.Contains(strings.ToLower(b), "expir") {
			t.Errorf("banner blames expiry: %q", b)
		}
		if !strings.Contains(b, "⚠") {
			t.Errorf("banner is not marked as a warning: %q", b)
		}
	}
}

// The banner reserves a row in rebuildEventsTable, so it must always render one.
// At width 0 (before the first WindowSizeMsg) trunc would otherwise return "",
// reserving a row that draws nothing and pushing the last event off-screen.
func TestGoneBanner_NeverEmpty(t *testing.T) {
	for _, w := range []int{0, -1, 10, 200} {
		if goneBanner(goneRestart, w) == "" {
			t.Errorf("empty banner at width %d, but a row was reserved for it", w)
		}
	}
}

// A tombstoned session still holds events, so it must stay reachable in the
// sessions table — retaining events the user cannot navigate to would be a
// half-fix.
func TestGoneSessionStaysInSessionsTable(t *testing.T) {
	m := newTestGoneModel(t, "vanished")
	m.sessionsTbl = newSessionsTable()

	m.Update(sessionsLoadedMsg{})
	m.rebuildSessionsTable()

	var found bool
	for _, r := range m.sessionsTbl.Rows() {
		if r[0] == "vanished" {
			found = true
			if !strings.Contains(r[2], "3") {
				t.Errorf("tombstone row lost its event count: %v", r)
			}
		}
	}
	if !found {
		t.Error("tombstoned session disappeared from the sessions table")
	}
}

// Traffic resuming under the same id proves the session is live again.
func TestReappearingSession_ClearsTombstone(t *testing.T) {
	m := newTestGoneModel(t, "s")
	m.Update(sessionsLoadedMsg{})
	if _, ok := m.gone["s"]; !ok {
		t.Fatal("precondition: session should be tombstoned")
	}

	m.Update(sessionsLoadedMsg{{ID: "s", UpdatedAt: time.Now()}})

	if _, ok := m.gone["s"]; ok {
		t.Error("tombstone survived the session coming back")
	}
}

// Cleanup is deferred to the moment the user picks a DIFFERENT session — the one
// reliable signal that the old events stopped being what they were looking at.
// The session being opened is kept even when gone; that is the point.
func TestForgetGoneExcept_KeepsTheOpenedSession(t *testing.T) {
	m := newTestGoneModel(t, "a")
	m.events["b"] = make([]pipeline.SessionEvent, 2)
	m.gone = map[string]goneReason{"a": goneRestart, "b": goneRestart}

	m.forgetGoneExcept("a")

	if len(m.events["a"]) != 3 {
		t.Error("the session the user opened was dropped")
	}
	if _, ok := m.events["b"]; ok {
		t.Error("an unrelated gone session was not cleaned up on selection")
	}
}

// A rekey is the one case where cached events legitimately MOVE: the store
// renames the bootstrap "default" bucket to the server-assigned contextId. The
// events are the same events, so they follow the id — and so does the selection.
func TestRekey_MigratesEventsAndSelection(t *testing.T) {
	m := newTestGoneModel(t, session.DefaultSessionID)
	m.sessions = []session.SessionSummary{{ID: session.DefaultSessionID}}

	m.Update(sessionsLoadedMsg{{ID: "ctx-42", UpdatedAt: time.Now()}})

	if len(m.events["ctx-42"]) != 3 {
		t.Errorf("events did not follow the rekey: %d under the new id", len(m.events["ctx-42"]))
	}
	if _, ok := m.events[session.DefaultSessionID]; ok {
		t.Error("the old bucket was left behind as a duplicate")
	}
	if m.selectedSess != "ctx-42" {
		t.Errorf("selection did not follow the rekey: %q", m.selectedSess)
	}
	if _, ok := m.gone["ctx-42"]; ok {
		t.Error("a rekeyed session should be live, not tombstoned")
	}
	for _, e := range m.events["ctx-42"] {
		if e.SessionID != "ctx-42" {
			t.Errorf("migrated event kept the stale SessionID %q", e.SessionID)
		}
	}
}

// Two unseen ids at once cannot identify which inherited "default". Guessing
// would be worse than holding still: the events stay viewable under the old id
// either way, just marked gone.
func TestAmbiguousRekey_DoesNotGuess(t *testing.T) {
	m := newTestGoneModel(t, session.DefaultSessionID)

	m.Update(sessionsLoadedMsg{
		{ID: "ctx-1", UpdatedAt: time.Now()},
		{ID: "ctx-2", UpdatedAt: time.Now()},
	})

	if len(m.events[session.DefaultSessionID]) != 3 {
		t.Error("events were moved or dropped on an ambiguous rekey")
	}
	if m.gone[session.DefaultSessionID] != goneEvicted {
		t.Error("the unmoved bucket should be tombstoned, not silently live")
	}
}

// newTestGoneModel builds a model focused on sessionID with 3 cached events.
func newTestGoneModel(t *testing.T, sessionID string) *model {
	t.Helper()
	events := make([]pipeline.SessionEvent, 3)
	for i := range events {
		events[i] = pipeline.SessionEvent{
			At:        time.Now(),
			Direction: pipeline.Outbound,
			Phase:     pipeline.SessionRequest,
			Host:      "api.example.com",
			SessionID: sessionID,
		}
	}
	m := &model{
		pane: paneEvents, selectedSess: sessionID, bodyHeight: 12, width: 200,
		events:       map[string][]pipeline.SessionEvent{sessionID: events},
		eventColumns: defaultColumnSelection(),
	}
	m.eventsTbl = newEventsTable()
	m.sessionsTbl = newSessionsTable()
	m.rebuildEventsTable()
	return m
}
