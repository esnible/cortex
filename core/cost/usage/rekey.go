package usage

// Rekeyed moves oldID's figures to newID: the per-session ring, and oldID's row in every
// bucket's session breakdown. It implements session.Rekeyer, which session.Store.Adopt calls
// when a coding agent's pending bucket is renamed to the session its first header named.
//
// Without it the store would list the session under its new id while /v1/usage kept
// answering for the old one — the "listed but zeroed" shape sessionRing exists to prevent.
// A newID that already has a ring keeps it; the store never adopts into a session that holds
// events, so that is a defensive no-op rather than a merge.
func (a *Aggregator) Rekeyed(oldID, newID string) {
	if oldID == newID {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if ring, ok := a.sessions[oldID]; ok {
		if _, taken := a.sessions[newID]; !taken {
			a.sessions[newID] = ring
			delete(a.sessions, oldID)
			relabelSession(ring.buckets, oldID, newID)
		}
	}
	relabelSession(a.all, oldID, newID)
}

func relabelSession(ring []bucket, oldID, newID string) {
	from, to := ringLabel(oldID), ringLabel(newID)
	for i := range ring {
		c, ok := ring[i].bySession[from]
		if !ok {
			continue
		}
		delete(ring[i].bySession, from)
		addLabel(&ring[i].bySession, to, c)
	}
}
