package tui

import (
	"fmt"
	"sort"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
)

// goneReason says why a session left the server's list. It drives the banner
// text, so the user learns which of several unrelated mechanisms cost them
// their live feed instead of guessing.
type goneReason int

const (
	// goneEvicted: the session is absent while the server still lists others.
	// Almost always the store's oldest-first eviction once max_sessions is
	// exceeded (authlib/session/store.go evictOldestLocked), or a TTL sweep in
	// the unusual case where session.ttl was set explicitly.
	goneEvicted goneReason = iota
	// goneRestart: the server listed nothing at all. The store is in-memory and
	// per-pod, so a restarted proxy comes back with zero sessions and stays that
	// way until traffic arrives.
	//
	// An empty list is strong but not conclusive evidence of a restart. Eviction
	// cannot produce one (it fires only when the count EXCEEDS max_sessions, so
	// something always survives), which leaves one other route: an explicit
	// session.ttl whose sweep aged out every session at once. That needs a
	// non-default config, so the wording below leads with the restart and admits
	// the alternative rather than asserting one cause. Distinguishing them for
	// real needs a boot/instance id from the server — the count is not on the
	// wire today. See the follow-up issue.
	goneRestart
)

// reconcileGone folds the server's session list into the local cache.
//
// The rule: the list is authoritative about what is LIVE, never about what is
// VIEWABLE. Events already fetched stay in m.events, and the user stays in
// whatever pane they were reading. Three reasons this is not the coin-flip it
// looks like:
//
//   - Deleting is irreversible. The session store is in-memory and per-pod, so
//     once abctl frees its copy the events exist nowhere. What deleting buys is
//     memory already bounded by maxEventsPerSession times the number of sessions
//     actually visited — an unmeasured saving traded against unrecoverable data.
//   - The user is the only party who knows when the data stopped being
//     interesting. Cleanup therefore happens on selecting a DIFFERENT session
//     (see forgetGoneExcept), where it is harmless because nothing on screen
//     depends on it.
//   - Absence is a poor signal in the first place. The single-agent shape
//     documented at session.DefaultSessionID puts nearly everything in one
//     "default" bucket, so a restart empties the list and the old code deleted
//     the only bucket the user had, 2s later, having also thrown them back to
//     the sessions pane.
//
// A session that reappears (traffic resumed, or the same id after a restart)
// is un-marked, and its cached events continue to accumulate.
func (m *model) reconcileGone(summaries []session.SessionSummary) {
	serverIDs := make(map[string]bool, len(summaries))
	for _, s := range summaries {
		serverIDs[s.ID] = true
	}

	// A rekey is the one case where cached events should MOVE rather than be
	// dropped: the store renames the bootstrap "default" bucket to the
	// server-assigned contextId once the backend response reveals it, and the
	// events under the old key are the same events. Migrate them so the history
	// stays attached to the surviving id, and follow the user's selection over.
	//
	// ORDERING DEPENDENCY: rekeyedTo compares against m.sessions, which must still
	// hold the PREVIOUS list. The caller therefore has to invoke this before
	// assigning m.sessions = msg (see the sessionsLoadedMsg case in app.go). Swap
	// those two lines and the previous "default" summary is gone, rekeyedTo returns
	// "", and rekeys silently stop migrating — the events survive under the old id,
	// so the bug shows up only as a stale duplicate bucket rather than as a failure.
	//
	// The discriminator is CreatedAt, not the shape of the list. Earlier revisions
	// of this code guessed from "default vanished and one unfamiliar id appeared",
	// narrowed with list-length and prior-membership checks. Those heuristics fail
	// in the ordinary steady state at max_sessions: Append evicts when the count
	// EXCEEDS the cap and evictOldestLocked removes exactly one entry, so session
	// #101 arriving means one id vanishes and one appears with the length
	// unchanged. If the evicted one was a stale "default" (it is spared only while
	// it is activeID), every heuristic passed and default's history was filed under
	// an unrelated session — the precise outcome the old comment claimed to
	// prevent. See TestEvictionAtCapacity_DoesNotMisMigrate.
	if events, ok := m.events[session.DefaultSessionID]; ok && !serverIDs[session.DefaultSessionID] {
		if newID := rekeyedTo(summaries, m.sessions); newID != "" {
			m.migrateSession(session.DefaultSessionID, newID, events)
		}
	}

	for id, cached := range m.events {
		if serverIDs[id] {
			// Live again — drop any stale tombstone.
			delete(m.gone, id)
			continue
		}
		if len(cached) == 0 {
			// A key with no events. snapshotLoadedMsg assigns m.events[id]
			// unconditionally, so drilling into a session that has nothing yet
			// creates the key with an empty slice. Tombstoning it would render
			// "id — 0 — gone": a row advertising retained events that do not exist,
			// held until the user selects something else. The tombstone is justified
			// by abctl holding the only copy; with no copy there is nothing to hold.
			continue
		}
		if _, already := m.gone[id]; already {
			continue
		}
		if m.gone == nil {
			m.gone = make(map[string]goneReason)
		}
		// An empty list is a restart (or a not-yet-populated store); an absence
		// alongside other live sessions is an eviction.
		if len(summaries) == 0 {
			m.gone[id] = goneRestart
		} else {
			m.gone[id] = goneEvicted
		}
	}
}

// rekeyedTo returns the id that "default" was renamed to, or "" when no rename
// can be proven. It is a proof, not a guess.
//
// Store.Rekey renames in place on the same *entry, so a rekeyed session carries
// default's ORIGINAL CreatedAt; a session the store creates fresh gets
// CreatedAt: now. Both values ride the wire as SessionSummary.CreatedAt
// ("createdAt") and apiclient.ListSessions decodes them, so the timestamps are
// available on both the previous list and the incoming one. Equal CreatedAt on an
// id the client has never seen therefore identifies a rename and nothing else —
// an evicted default plus an unrelated new session cannot fake it, because the
// new session's CreatedAt is its own.
//
// Ambiguity still yields "": if several unseen ids share default's CreatedAt,
// guessing which inherited the history would be worse than declining, and
// declining is cheap (the events stay viewable under the old id, tombstoned).
func rekeyedTo(summaries, prev []session.SessionSummary) string {
	var created time.Time
	for _, s := range prev {
		if s.ID == session.DefaultSessionID {
			created = s.CreatedAt
			break
		}
	}
	// No previous default summary — nothing to match against. Note this is also
	// what a caller that already overwrote m.sessions looks like.
	if created.IsZero() {
		return ""
	}

	seen := make(map[string]bool, len(prev))
	for _, s := range prev {
		seen[s.ID] = true
	}

	found := ""
	for _, s := range summaries {
		if seen[s.ID] {
			continue
		}
		// .Equal, never ==: these came back through JSON, so the wall clocks match
		// but the monotonic readings and *Location pointers need not. == compares
		// the struct fields and returns false for the same instant — which would
		// silently disable migration altogether rather than fail loudly.
		if !s.CreatedAt.Equal(created) {
			continue
		}
		if found != "" {
			return "" // ambiguous
		}
		found = s.ID
	}
	return found
}

// migrateSession moves cached events from oldID to newID, mirroring the store's
// Rekey. It never clobbers an existing cache under newID, and never deletes
// oldID's events unless they were successfully moved — see the early return.
func (m *model) migrateSession(oldID, newID string, events []pipeline.SessionEvent) {
	if _, exists := m.events[newID]; exists {
		// Already holding a cache under newID. Do NOT drop oldID's events: this is
		// the one place a delete could still lose the only copy of a history, which
		// is exactly what this file exists to prevent. Leaving them alone lets
		// reconcileGone's loop tombstone oldID on its own, so they stay viewable.
		// (The server's own Rekey is a no-op in this situation, but abctl's cache is
		// not the server's store — that distinction is the whole point here.)
		return
	}
	for i := range events {
		events[i].SessionID = newID
	}
	m.events[newID] = events
	delete(m.events, oldID)
	delete(m.gone, oldID)
	if m.selectedSess == oldID {
		m.selectedSess = newID
	}
}

// forgetGoneExcept drops cached events for tombstoned sessions other than keep.
// Called when the user selects a session: at that moment any OTHER gone session
// has demonstrably stopped being what they are looking at, which is the only
// reliable signal available for when the data stopped mattering. The session
// being opened is retained even when gone — that is precisely the case this
// whole change exists to preserve.
//
// SOLE RELEASE POINT: enter on the sessions pane (keys.go) is the only caller, so
// a user who never drills into another session after a restart keeps all N
// tombstoned sessions for the process lifetime. That is the one path where
// retention is unbounded in TIME; it stays bounded in SIZE by
// maxEventsPerSession per session, and a whole pod switch clears the map outright
// (backToPodsPane), so there is no need to release there too. Deliberate: a timer
// or a count-based sweep would be another mechanism deleting events out from
// under someone who stepped away, which is the exact failure this file exists to
// remove. If a bound is ever wanted, cap the NUMBER of tombstones and evict the
// least-recently-viewed — never the one on screen.
func (m *model) forgetGoneExcept(keep string) {
	for id := range m.gone {
		if id == keep {
			continue
		}
		delete(m.events, id)
		delete(m.gone, id)
	}
}

// goneBanner renders the notice shown above a tombstoned session's events. It
// names the mechanism rather than saying "expired": time-based expiry is off by
// default (session.ttl defaults to never), so "expired" would be wrong as well
// as unhelpful.
func goneBanner(reason goneReason, width int) string {
	// Before the first WindowSizeMsg the width is 0 and trunc would return an
	// empty string, while rebuildEventsTable has already reserved a row for the
	// banner. Fall back to a sane width so the reservation and the render agree.
	if width <= 0 {
		width = 80
	}
	var msg string
	switch reason {
	case goneRestart:
		msg = "server has no sessions (proxy restarted, or all aged out) — events below are abctl's copy."
	default:
		msg = "session no longer on server (evicted) — events below are abctl's copy."
	}
	return styleWarn.Render(trunc(fmt.Sprintf("⚠ %s", msg), width))
}

// goneBannerHeight is the rendered height of goneBanner: one line, since trunc
// keeps it to a single row.
const goneBannerHeight = 1

// goneIDs lists tombstoned sessions in a stable order. Sorted rather than map
// order so the sessions table does not reshuffle under the cursor on every 2s
// refresh.
func (m *model) goneIDs() []string {
	out := make([]string, 0, len(m.gone))
	for id := range m.gone {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
