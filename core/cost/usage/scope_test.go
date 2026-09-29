package usage

import (
	"testing"
	"time"
)

// scopeFixture is a two-bucket group=agent snapshot: one agent in both buckets, one in only
// the second. The second agent's absence from the first bucket is the case that separates
// "narrow the buckets" from "sum them" — a fold cannot tell an idle bucket from a missing one,
// and the chart has to keep the idle bucket to keep its time axis.
func scopeFixture() *Snapshot {
	base := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	return &Snapshot{
		Window:        "today",
		BucketSeconds: 60,
		Group:         GroupAgent,
		Buckets: []Bucket{
			{
				At:          base,
				Counts:      Counts{Requests: 10, Tokens: 1000, PricedRequests: 10, PriceableRequests: 10, CostMicros: 3_000},
				LatMeanMs:   42,
				LatStdDevMs: 7,
				LatSamples:  10,
				Series: map[string]Counts{
					"claude-code/2.1.270": {Requests: 10, Tokens: 1000, PricedRequests: 10, PriceableRequests: 10, CostMicros: 3_000},
				},
			},
			{
				At:          base.Add(time.Minute),
				Counts:      Counts{Requests: 12, Tokens: 1200, PricedRequests: 4, PriceableRequests: 12, CostMicros: 1_000},
				LatMeanMs:   55,
				LatStdDevMs: 9,
				LatSamples:  12,
				Series: map[string]Counts{
					"claude-code/2.1.270": {Requests: 4, Tokens: 400, PricedRequests: 4, PriceableRequests: 4, CostMicros: 1_000},
					"bob-shell/2.0.5":     {Requests: 8, Tokens: 800, PriceableRequests: 8},
				},
			},
		},
		Totals: Counts{Requests: 22, Tokens: 2200, PricedRequests: 14, PriceableRequests: 22, CostMicros: 4_000},
		Priced: true,
	}
}

// The window-level figures become the named agent's, folded across every bucket.
func TestScopeToAgent_NarrowsTotalsToOneAgent(t *testing.T) {
	got, err := ScopeToAgent(scopeFixture(), "claude-code/2.1.270", KeepBuckets)
	if err != nil {
		t.Fatalf("ScopeToAgent: %v", err)
	}
	// 10 + 4 across the two buckets, not the window's 22.
	if got.Totals.Requests != 14 {
		t.Errorf("Totals.Requests = %d, want 14 (this agent's, not the window's 22)", got.Totals.Requests)
	}
	if got.Totals.CostMicros != 4_000 {
		t.Errorf("Totals.CostMicros = %d, want 4000", got.Totals.CostMicros)
	}
}

// Priced is RE-DERIVED, so an agent nothing priced reports cost-unavailable rather than $0.00.
//
// This is the assertion that a narrowing which replaced only Totals would fail: the WINDOW was
// priced — the other agent's traffic carried cost — so an inherited Priced:true would have the
// caller print $0.00 for exactly the agent the AGENTS pane prints "—" for.
func TestScopeToAgent_RederivesPricedForAnUnpricedAgent(t *testing.T) {
	got, err := ScopeToAgent(scopeFixture(), "bob-shell/2.0.5", KeepBuckets)
	if err != nil {
		t.Fatalf("ScopeToAgent: %v", err)
	}
	if got.Priced {
		t.Error("Priced = true for an agent whose traffic nothing priced; the caller will print $0.00 instead of \"—\"")
	}
	if got.Totals.CostMicros != 0 {
		t.Errorf("Totals.CostMicros = %d, want 0", got.Totals.CostMicros)
	}
}

// The three by-model maps are dropped, because nothing here can narrow them: a bucket's series
// is keyed by agent and carries no per-model breakdown, so the only available readings are the
// window's maps — which describe other agents' traffic — or none.
func TestScopeToAgent_DropsTheProvenanceMapsAndKeepsTheReadFacts(t *testing.T) {
	snap := scopeFixture()
	snap.PricedBy = map[string]int64{"claude-sonnet-5": 14}
	snap.UnpricedBy = map[string]int64{"some-model": 8}
	snap.IncompleteBy = map[string]int64{"claude-sonnet-5": 2}
	snap.DaysOutsideRetention = 3
	got, err := ScopeToAgent(snap, "claude-code/2.1.270", KeepBuckets)
	if err != nil {
		t.Fatalf("ScopeToAgent: %v", err)
	}
	if got.PricedBy != nil || got.UnpricedBy != nil || got.IncompleteBy != nil {
		t.Errorf("a by-model map survived the narrowing: priced=%v unpriced=%v incomplete=%v",
			got.PricedBy, got.UnpricedBy, got.IncompleteBy)
	}
	// DaysOutsideRetention describes the READ, which is the same fact whichever agent is
	// scoped to. Dropping it would hide a short sum behind a narrower question.
	if got.DaysOutsideRetention != 3 {
		t.Errorf("DaysOutsideRetention = %d, want 3 — it describes the read, not the agent",
			got.DaysOutsideRetention)
	}
}

// KeepBuckets is the mode `abctl cost` asks for: it prints window totals and reads no buckets,
// so it wants no per-bucket work done. The buckets and their Series come through untouched.
func TestScopeToAgent_KeepBucketsLeavesTheSeriesIntact(t *testing.T) {
	got, err := ScopeToAgent(scopeFixture(), "claude-code/2.1.270", KeepBuckets)
	if err != nil {
		t.Fatalf("ScopeToAgent: %v", err)
	}
	if len(got.Buckets) != 2 {
		t.Fatalf("len(Buckets) = %d, want 2", len(got.Buckets))
	}
	if got.Buckets[0].Requests != 10 || got.Buckets[1].Requests != 12 {
		t.Errorf("bucket counts = %d, %d; want the window's 10, 12 left alone",
			got.Buckets[0].Requests, got.Buckets[1].Requests)
	}
	if len(got.Buckets[1].Series) != 2 {
		t.Errorf("bucket 1 kept %d series entries, want both", len(got.Buckets[1].Series))
	}
}

// NarrowBuckets is what a CHART needs: every bucket's counts become this agent's, so a series
// rendered from Buckets describes the scoped agent rather than the whole window.
func TestScopeToAgent_NarrowBucketsRewritesEachBucket(t *testing.T) {
	got, err := ScopeToAgent(scopeFixture(), "claude-code/2.1.270", NarrowBuckets)
	if err != nil {
		t.Fatalf("ScopeToAgent: %v", err)
	}
	if got.Buckets[0].Requests != 10 {
		t.Errorf("bucket 0 Requests = %d, want this agent's 10", got.Buckets[0].Requests)
	}
	// The window's second bucket held 12 requests; this agent's share of it is 4.
	if got.Buckets[1].Requests != 4 {
		t.Errorf("bucket 1 Requests = %d, want this agent's 4 (the window's is 12)",
			got.Buckets[1].Requests)
	}
	// Series is dropped rather than kept: it describes a grouping the narrowed bucket has
	// already been reduced to one member of, and a renderer that found it there would stack
	// every agent back on top of the scoped one.
	for i, b := range got.Buckets {
		if b.Series != nil {
			t.Errorf("bucket %d kept its Series after narrowing: %v", i, b.Series)
		}
	}
}

// A bucket the agent is absent from survives as a ZERO bucket rather than being dropped.
//
// The chart reads Buckets positionally against a time axis — Snapshot.Buckets' own doc says
// zeroed entries are what let a client tell an idle minute from one that fell off the ring — so
// dropping the agent's idle buckets would compress its history and move every bar left.
func TestScopeToAgent_NarrowBucketsKeepsIdleBucketsAsZero(t *testing.T) {
	got, err := ScopeToAgent(scopeFixture(), "bob-shell/2.0.5", NarrowBuckets)
	if err != nil {
		t.Fatalf("ScopeToAgent: %v", err)
	}
	if len(got.Buckets) != 2 {
		t.Fatalf("len(Buckets) = %d, want 2 — an idle bucket must not be dropped", len(got.Buckets))
	}
	if got.Buckets[0].Requests != 0 {
		t.Errorf("bucket 0 Requests = %d, want 0: this agent sent nothing in it", got.Buckets[0].Requests)
	}
	if !got.Buckets[0].At.Equal(scopeFixture().Buckets[0].At) {
		t.Errorf("bucket 0 At = %v, want the original timestamp", got.Buckets[0].At)
	}
	if got.Buckets[1].Requests != 8 {
		t.Errorf("bucket 1 Requests = %d, want 8", got.Buckets[1].Requests)
	}
}

// Latency is ZEROED by NarrowBuckets, because it cannot be narrowed: Series is
// map[string]Counts and Counts carries no latency, so a bucket's LatMeanMs / LatStdDevMs /
// LatSamples describe every agent that shared the bucket. Keeping them would render one
// agent's chart out of another's response times; the caller must say latency is unavailable
// under a scope instead.
func TestScopeToAgent_NarrowBucketsZeroesLatencyItCannotAttribute(t *testing.T) {
	got, err := ScopeToAgent(scopeFixture(), "claude-code/2.1.270", NarrowBuckets)
	if err != nil {
		t.Fatalf("ScopeToAgent: %v", err)
	}
	for i, b := range got.Buckets {
		if b.LatMeanMs != 0 || b.LatStdDevMs != 0 || b.LatSamples != 0 {
			t.Errorf("bucket %d kept unattributable latency: mean=%v stddev=%v samples=%d",
				i, b.LatMeanMs, b.LatStdDevMs, b.LatSamples)
		}
	}
}

// An unknown agent is an error that NAMES the known ones, sorted.
//
// The labels are User-Agents, so they are neither short nor guessable — "bob" is the obvious
// thing to try and is not what Bob sends. Sorted because a set printed in map order is a set a
// reader cannot diff against yesterday's.
func TestScopeToAgent_UnknownAgentNamesTheKnownOnesInOrder(t *testing.T) {
	_, err := ScopeToAgent(scopeFixture(), "bob", KeepBuckets)
	if err == nil {
		t.Fatal("ScopeToAgent accepted an agent that is not in the window")
	}
	want := "no agent \"bob\" in the today window; seen: bob-shell/2.0.5, claude-code/2.1.270"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

// A window with no agent traffic at all is a DIFFERENT sentence: there is no set to print, and
// "seen: " followed by nothing reads as a truncated message rather than an empty window.
func TestScopeToAgent_EmptyWindowSaysSoRatherThanListingNothing(t *testing.T) {
	_, err := ScopeToAgent(&Snapshot{Window: "today", Group: GroupAgent}, "bob", KeepBuckets)
	if err == nil {
		t.Fatal("ScopeToAgent accepted an agent against an empty window")
	}
	want := "no agent traffic in the today window, so --agent \"bob\" matches nothing"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

// The caller's snapshot is never mutated: both modes hand back a copy.
//
// NarrowBuckets is the mode that makes this load-bearing: it rewrites per-bucket counts, so
// narrowing in place would leave the caller holding one agent's figures with no way back to the
// window's own.
func TestScopeToAgent_DoesNotMutateTheCallersSnapshot(t *testing.T) {
	snap := scopeFixture()
	if _, err := ScopeToAgent(snap, "claude-code/2.1.270", NarrowBuckets); err != nil {
		t.Fatalf("ScopeToAgent: %v", err)
	}
	if snap.Totals.Requests != 22 {
		t.Errorf("caller's Totals.Requests = %d, want the window's 22", snap.Totals.Requests)
	}
	if snap.Buckets[1].Requests != 12 {
		t.Errorf("caller's bucket 1 Requests = %d, want the window's 12", snap.Buckets[1].Requests)
	}
	if len(snap.Buckets[1].Series) != 2 {
		t.Errorf("caller's bucket 1 lost its Series: %v", snap.Buckets[1].Series)
	}
	if snap.Group != GroupAgent {
		t.Errorf("caller's Group = %v, want it untouched", snap.Group)
	}
}
