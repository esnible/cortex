package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

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
			// Exact, not Contains: "13" and "30" contain "3" and would sail
			// through a substring check on a count that had drifted.
			if r[2] != "3" {
				t.Errorf("tombstone row lost its event count: got %q, want %q in %v", r[2], "3", r)
			}
		}
	}
	if !found {
		t.Error("tombstoned session disappeared from the sessions table")
	}
}

// The gap that let an ANSI-styled table cell through: every other test here
// asserts on Rows() DATA, which never exercises rendering, and CI has no TTY —
// lipgloss emits no escape sequences when its profile is Ascii, so a styled cell
// measures its visible width and truncation is a no-op.
//
// This forces a colour profile, renders the real View(), and asserts the label
// survives. bubbles v1.0.0 truncates each cell with runewidth.Truncate BEFORE
// styling (table.go renderRow); runewidth is not ANSI-aware, so a styled "gone"
// measures 11 against the ACTIVE column's width of 8 and renders as "gon…" with
// the closing reset stripped, bleeding colour into every later cell.
func TestGoneMarker_SurvivesRenderingUnderColor(t *testing.T) {
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(orig) })

	m := newTestGoneModel(t, "vanished")
	m.pane = paneSessions
	m.width, m.height = 200, 40
	m.Update(sessionsLoadedMsg{})
	m.rebuildSessionsTable()

	view := m.sessionsTbl.View()
	if !strings.Contains(view, "gone") {
		t.Errorf("the gone marker did not survive rendering; got truncated or mangled:\n%s", view)
	}
	if strings.Contains(view, "gon…") {
		t.Error("the gone marker was truncated mid-label — a styled cell is being " +
			"measured with its escape bytes counted against the column width")
	}
	// An odd number of SGR introducers without matching resets is how the colour
	// bleed manifests: Truncate drops the trailing \x1b[0m.
	if opens, resets := strings.Count(view, "\x1b["), strings.Count(view, "\x1b[0m"); opens > 0 && resets == 0 {
		t.Errorf("styled output has %d escape sequences and no resets — colour will bleed", opens)
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

// Guards the ordering dependency the rekey detection rests on: reconcileGone
// must run while m.sessions still holds the PREVIOUS list, because that is how it
// tells a rekeyed id from one it already knew.
//
// This drives Update (not reconcileGone directly), so reordering the two lines in
// the sessionsLoadedMsg case fails here. Without it a reorder would be silent: no
// id looks new, soleNewSession returns "", and the rekey just stops migrating —
// leaving a stale duplicate bucket rather than an error.
func TestRekeyDetection_RunsBeforeSessionsIsReplaced(t *testing.T) {
	m := newTestGoneModel(t, session.DefaultSessionID)
	// "known" is already in the previous list, so only "ctx-42" is new. Were
	// m.sessions replaced first, both would look known and nothing would migrate.
	m.sessions = []session.SessionSummary{
		{ID: session.DefaultSessionID}, {ID: "known"},
	}

	m.Update(sessionsLoadedMsg{
		{ID: "known", UpdatedAt: time.Now()},
		{ID: "ctx-42", UpdatedAt: time.Now()},
	})

	if len(m.events["ctx-42"]) != 3 {
		t.Fatalf("rekey did not migrate: reconcileGone likely ran after m.sessions " +
			"was replaced, so no id looked new")
	}
	if _, stale := m.events[session.DefaultSessionID]; stale {
		t.Error("old bucket left behind as a duplicate")
	}
}

// migrateSession must not delete the source when it declines to migrate. When
// newID is already cached the move is skipped, and an unconditional
// delete(m.events, oldID) would discard the default bucket with no migration and
// no tombstone — the one unrecoverable deletion path this file exists to remove.
func TestMigrateDeclined_DoesNotDropTheSource(t *testing.T) {
	m := newTestGoneModel(t, session.DefaultSessionID)
	// Already holding a cache under the target id.
	m.events["ctx-42"] = make([]pipeline.SessionEvent, 1)

	m.migrateSession(session.DefaultSessionID, "ctx-42", m.events[session.DefaultSessionID])

	if got := len(m.events[session.DefaultSessionID]); got != 3 {
		t.Errorf("source events dropped without being migrated: got %d, want 3", got)
	}
	if got := len(m.events["ctx-42"]); got != 1 {
		t.Errorf("existing target cache was clobbered: got %d, want 1", got)
	}
}

// And the declined case still leaves the source reachable: reconcileGone's loop
// tombstones it, so the events stay viewable under the old id.
func TestMigrateDeclined_TombstonesTheSource(t *testing.T) {
	m := newTestGoneModel(t, session.DefaultSessionID)
	m.events["ctx-42"] = make([]pipeline.SessionEvent, 1)
	m.sessions = []session.SessionSummary{{ID: session.DefaultSessionID}, {ID: "ctx-42"}}

	m.Update(sessionsLoadedMsg{{ID: "ctx-42", UpdatedAt: time.Now()}})

	if len(m.events[session.DefaultSessionID]) != 3 {
		t.Fatal("source events were dropped")
	}
	if _, ok := m.gone[session.DefaultSessionID]; !ok {
		t.Error("source was neither migrated nor tombstoned — retained but unreachable")
	}
}

// A cache key with no events must not be tombstoned. snapshotLoadedMsg assigns
// m.events[id] unconditionally, so drilling into an empty session creates the key;
// a tombstone there renders "id — 0 — gone", advertising events that do not exist.
func TestEmptyCacheKey_IsNotTombstoned(t *testing.T) {
	m := newTestGoneModel(t, "real")
	m.events["empty"] = nil

	m.Update(sessionsLoadedMsg{})

	if _, ok := m.gone["empty"]; ok {
		t.Error("a session with no cached events was tombstoned")
	}
	if _, ok := m.gone["real"]; !ok {
		t.Error("the session that does hold events was not tombstoned")
	}
	for _, r := range m.sessionsTbl.Rows() {
		if r[0] == "empty" {
			t.Errorf("empty session rendered a tombstone row: %v", r)
		}
	}
}

// An eviction of "default" plus an unrelated new session looks exactly like a
// rename to a naive check: default gone, one unfamiliar id present. Migrating
// then would file the events under a session they never belonged to. The list
// shrinking (2 previous, 2 now, but one of the previous was itself evicted) is
// the tell — here the list grows past what a rename could produce.
func TestEvictionPlusNewSession_IsNotTreatedAsRekey(t *testing.T) {
	m := newTestGoneModel(t, session.DefaultSessionID)
	// Previously: default only. Now: two unrelated sessions, default evicted.
	m.sessions = []session.SessionSummary{{ID: session.DefaultSessionID}}

	m.Update(sessionsLoadedMsg{
		{ID: "other", UpdatedAt: time.Now()},
		{ID: "unrelated", UpdatedAt: time.Now()},
	})

	if _, ok := m.events["other"]; ok {
		t.Error("events migrated to an unrelated session id")
	}
	if len(m.events[session.DefaultSessionID]) != 3 {
		t.Error("events left the default bucket without a real rename")
	}
	if _, ok := m.gone[session.DefaultSessionID]; !ok {
		t.Error("default should be tombstoned, not silently migrated")
	}
}

// forgetGoneExcept frees other tombstones, and the sessions table renders its
// tombstone rows from m.gone — so the table must be rebuilt at that call site.
//
// Without the rebuild the freed session keeps a row for up to one 2s refresh, and
// the row lies: it still shows the event count from before the cache was dropped.
// Backing out and selecting it then lands on an empty events pane AND flashes a
// 404, because the snapshot guard keys off m.gone[id] — which was just deleted for
// exactly that id, so it no longer looks gone.
func TestForgetGone_RebuildsSessionsTable(t *testing.T) {
	m := newTestGoneModel(t, "keep")
	m.events["freed"] = make([]pipeline.SessionEvent, 3)
	m.Update(sessionsLoadedMsg{})
	m.rebuildSessionsTable()
	if len(m.sessionsTbl.Rows()) != 2 {
		t.Fatalf("precondition: want 2 tombstone rows, got %d", len(m.sessionsTbl.Rows()))
	}

	// Drive the REAL handler, so removing the rebuild from keys.go fails here. A
	// test that called rebuildSessionsTable itself would pass either way.
	m.pane = paneSessions
	for i, r := range m.sessionsTbl.Rows() {
		if r[0] == "keep" {
			m.sessionsTbl.SetCursor(i)
		}
	}
	m.handleKey(keyRune('l')) // enter/right/l on the sessions pane
	if m.selectedSess != "keep" {
		t.Fatalf("precondition: handler did not open \"keep\" (got %q)", m.selectedSess)
	}

	for _, r := range m.sessionsTbl.Rows() {
		if r[0] == "freed" {
			t.Errorf("freed tombstone still has a row advertising %q events, "+
				"but its cache holds %d", r[2], len(m.events["freed"]))
		}
	}
	if _, ok := m.gone["keep"]; !ok {
		t.Error("the opened session lost its tombstone, so the snapshot guard " +
			"can no longer suppress a 404 fetch")
	}
	var keptRow bool
	for _, r := range m.sessionsTbl.Rows() {
		if r[0] == "keep" {
			keptRow = true
		}
	}
	if !keptRow {
		t.Error("the opened session lost its row; its retained events are unreachable")
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
