package usage

import (
	"fmt"
	"sort"
	"strings"
)

// BucketScope says whether ScopeToAgent narrows the per-bucket series as well as the
// window-level figures.
//
// AN EXPLICIT PARAMETER RATHER THAN A DEFAULT, because the two callers want different things
// and the cheaper one is not the safe one. abctl's usage pane renders a CHART from Buckets, so
// leaving them whole-window would draw every agent's traffic under a title naming one — silent,
// and wrong in the direction a reader cannot detect. `abctl cost` reads only window totals on
// its scoped path, so it asks for no narrowing and pays for none.
//
// NEITHER VALUE IS A SAFE DEFAULT, which is why there is no zero-argument form. Defaulting to
// KeepBuckets hands a chart the whole window; defaulting to NarrowBuckets silently drops the
// latency a caller may be about to render, and drops Series from a response a caller may be
// about to break down. The caller knows which it reads; this package does not.
type BucketScope int

const (
	// KeepBuckets narrows only the window-level figures. Buckets and their Series come
	// through untouched.
	KeepBuckets BucketScope = iota
	// NarrowBuckets additionally rewrites every bucket's counts to the scoped agent's share
	// of it and drops Series. See ScopeToAgent for what this cannot carry across.
	NarrowBuckets
)

// ScopeToAgent narrows a group=agent snapshot to one agent's figures.
//
// IT REWRITES Totals AND HANDS BACK A SNAPSHOT, rather than rendering the agent itself, so
// callers apply their existing writers to it with no second implementation and no chance of the
// two drifting: `abctl cost`'s negative-total refusal, coverage-gap disclosure and
// incomplete-read admission each read one agent's numbers, and the TUI's chart reads the same
// narrowing. Which fields do NOT survive, and why, is stated at each narrowing below. A COPY,
// never the caller's snapshot mutated in place — the TUI holds one fetched snapshot and
// re-derives a scoped view from it whenever the scope changes.
//
// The fold is FoldSeriesAcrossWindow, the same one abctl's AGENTS pane uses, so the figure
// printed by the CLI and the row shown in the pane cannot disagree.
//
// AN UNKNOWN AGENT IS AN ERROR THAT NAMES THE KNOWN ONES. The labels are User-Agents, so they
// are neither short nor guessable — "bob" is the obvious thing to try and is not what Bob
// sends. The set is already in hand, so withholding it would be a choice.
func ScopeToAgent(snap *Snapshot, agent string, buckets BucketScope) (*Snapshot, error) {
	series := FoldSeriesAcrossWindow(snap.Buckets)
	counts, ok := series[agent]
	if !ok {
		known := make([]string, 0, len(series))
		for label := range series {
			known = append(known, label)
		}
		// Sorted so the same window reports the same order every run; a set printed in map
		// order is a set a reader cannot diff against yesterday's.
		sort.Strings(known)
		if len(known) == 0 {
			return nil, fmt.Errorf("no agent traffic in the %s window, so --agent %q matches nothing",
				snap.Window, agent)
		}
		return nil, fmt.Errorf("no agent %q in the %s window; seen: %s",
			agent, snap.Window, strings.Join(known, ", "))
	}
	scoped := *snap
	scoped.Totals = counts
	// EVERY WHOLE-WINDOW STATEMENT ABOUT WHERE THE TOTALS CAME FROM GOES WITH Totals, or it is
	// printed beside one agent's figure while describing all of them. Replacing only Totals left
	// `--agent <an agent nothing priced>` printing $0.00 — the window was priced, just not this
	// agent's traffic — for exactly the agent the AGENTS pane prints "—" for, which breaks both
	// writeCostSummary's "cost unavailable rather than $0.00" rule and this function's own claim
	// that the figure there and the row in the pane cannot disagree.
	//
	// Priced is RE-DERIVED with the producers' own rule rather than one invented here: both
	// snapshot.go and sessionapi set it to Totals.PricedRequests > 0, so the narrowed snapshot is
	// the one they would have emitted had this agent's traffic been the whole window.
	scoped.Priced = counts.PricedRequests > 0
	// The three by-model maps are DROPPED, not narrowed, because nothing here can narrow them: a
	// bucket's series is keyed by agent and carries no per-model breakdown, so the only available
	// readings are the window's maps — which describe other agents' traffic — or none. They are
	// omitempty on the wire, and `abctl cost`'s costIncompleteReasonLines already treats an
	// absent map as nothing to say, which is its common case for a ledger-backed window anyway.
	scoped.PricedBy = nil
	scoped.UnpricedBy = nil
	scoped.IncompleteBy = nil
	// Degraded and DaysOutsideRetention STAY, and the asymmetry is the point: they describe the
	// READ and the retention configuration, which are the same facts whichever agent is scoped
	// to. Dropping them would hide a short sum behind a narrower question.
	//
	// SeriesOvershootMicros and SeriesAvoidedOvershootMicros stay too, and they are the two the
	// "every" above has to account for rather than pass over. Both are defect reports about a
	// breakdown — the series summed to MORE than the total — so they belong with Degraded rather
	// than with the provenance maps. A correct producer never sends either on this path:
	// residualOf leaves them nil unless the series overshoots, which cannot happen where the
	// figures reconcile. Where one does arrive it is upstream's bug, and forwarding it says so;
	// narrowing it to an agent would be inventing a per-agent overshoot nothing computed.

	// Currencies IS CARRIED OVER, AND IT NO LONGER DESCRIBES Totals. Said out loud because it is
	// the one field on this struct that the narrowing above invalidates, and the honest options are
	// worse than keeping it.
	//
	// The field means "every unit the rows behind Totals were denominated in", and after this copy
	// Totals is one agent while the list is the whole window. Narrowing it is not available:
	// deciding which units THIS agent's traffic carries needs a cross-tabulation of agent against
	// currency, and a folded per-agent Counts has already summed that axis away. Dropping it is
	// worse than leaving it — this agent's own traffic may well be the mixed part, and an absent
	// list reads as "single unit", so the surface would print a confident figure that is exactly
	// the credits-plus-dollars sum this whole change exists to refuse.
	//
	// SO THE OVER-REFUSAL IS DELIBERATE, and it is the safe direction: a per-agent figure is
	// withheld in a mixed window even when that agent billed in one unit. `abctl cost`'s
	// writeCostSummary says which of the two it is rather than letting the reader assume, because
	// "no figure for this agent" and "no figure for this window" have different fixes.

	if buckets == NarrowBuckets {
		scoped.Buckets = narrowBucketsToAgent(snap.Buckets, agent)
	}
	return &scoped, nil
}

// narrowBucketsToAgent rewrites each bucket to one agent's share of it.
//
// A NEW SLICE, because Bucket holds a map and the caller's snapshot must survive intact —
// see ScopeToAgent's copy rule. Assigning through scoped.Buckets[i] would write into the
// array the caller still owns.
//
// EVERY BUCKET SURVIVES, including the ones this agent sent nothing in, which become zero
// buckets at their original timestamps. The chart reads Buckets positionally against a time
// axis — Snapshot.Buckets' own doc says the zeroed entries are what let a client tell an idle
// minute from one that fell off the ring — so dropping an agent's idle buckets would compress
// its history and move every bar left of where it happened.
//
// LATENCY IS ZEROED RATHER THAN CARRIED, and it is the one reading this narrowing cannot
// produce: Series is map[string]Counts and Counts holds no latency, so LatMeanMs, LatStdDevMs
// and LatSamples describe every agent that shared the bucket. Keeping them would draw one
// agent's chart out of another's response times. A caller that offers a latency view has to say
// it is unavailable under a scope; there is no per-agent latency on the wire to offer instead.
func narrowBucketsToAgent(buckets []Bucket, agent string) []Bucket {
	out := make([]Bucket, 0, len(buckets))
	for _, b := range buckets {
		// The zero Counts when absent, which is the idle-bucket case above.
		out = append(out, Bucket{At: b.At, Counts: b.Series[agent]})
	}
	return out
}
