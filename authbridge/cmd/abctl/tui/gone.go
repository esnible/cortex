package tui

import (
	"fmt"
	"sort"

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
	if events, ok := m.events[session.DefaultSessionID]; ok && !serverIDs[session.DefaultSessionID] {
		if newID := soleNewSession(serverIDs, m.sessions); newID != "" {
			m.migrateSession(session.DefaultSessionID, newID, events)
		}
	}

	for id := range m.events {
		if serverIDs[id] {
			// Live again — drop any stale tombstone.
			delete(m.gone, id)
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

// soleNewSession returns the id the store rekeyed to, or "" when that cannot be
// established unambiguously. A rekey shows up as exactly one id that the server
// now lists and abctl has never seen before; if several appeared, guessing which
// inherited "default" would be worse than leaving the events where they are
// (they stay viewable under the old id either way, just marked gone).
func soleNewSession(serverIDs map[string]bool, prev []session.SessionSummary) string {
	seen := make(map[string]bool, len(prev))
	for _, s := range prev {
		seen[s.ID] = true
	}
	found := ""
	for id := range serverIDs {
		if seen[id] {
			continue
		}
		if found != "" {
			return "" // ambiguous
		}
		found = id
	}
	return found
}

// migrateSession moves cached events from oldID to newID, mirroring the store's
// Rekey. It will not clobber an existing cache under newID: the store's own
// Rekey is a no-op when newID already exists, so the events there are already
// the authoritative copy.
func (m *model) migrateSession(oldID, newID string, events []pipeline.SessionEvent) {
	if _, exists := m.events[newID]; !exists {
		for i := range events {
			events[i].SessionID = newID
		}
		m.events[newID] = events
	}
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
		msg = "proxy restarted — no longer live. Events below are abctl's copy; the server's is gone."
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
