package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// wire is the exact JSON the plugin publishes under "tool-prune".
const wire = `{"toolsRemoved":["NotebookEdit","WebSearch"],"bytesRemoved":28568,
  "bodyBytesAfter":90635,"model":"claude-opus-5","rateInput":3.8e-06,
  "rateCacheWrite":4.75e-06,"rateCacheRead":3.8e-07,"rateSource":"default"}`

func reqEvent(t *testing.T, raw string) *pipeline.SessionEvent {
	t.Helper()
	return &pipeline.SessionEvent{
		Phase:   pipeline.SessionRequest,
		Plugins: map[string]json.RawMessage{"tool-prune": json.RawMessage(raw)},
	}
}

// TestDecodePruneSaving guards the tags against drift with the plugin's struct.
// A silent decode failure would show a plain token total and look like the plugin
// having saved nothing.
func TestDecodePruneSaving(t *testing.T) {
	ps, ok := decodePruneSaving(reqEvent(t, wire))
	if !ok {
		t.Fatal("failed to decode the published event")
	}
	if ps.BytesRemoved != 28568 || ps.BodyBytesAfter != 90635 {
		t.Errorf("byte fields = %d/%d", ps.BytesRemoved, ps.BodyBytesAfter)
	}
	if ps.RateCacheWrite != 4.75e-06 || ps.RateCacheRead != 3.8e-07 {
		t.Errorf("rates did not decode: %+v", ps)
	}
	if ps.RateSource != "default" {
		t.Errorf("RateSource = %q", ps.RateSource)
	}
	// Absent, malformed, and zero-valued all decline rather than render zeros.
	for _, bad := range []*pipeline.SessionEvent{
		nil,
		{Phase: pipeline.SessionRequest},
		reqEvent(t, `{"bytesRemoved":0,"bodyBytesAfter":100}`),
		reqEvent(t, `{"bytesRemoved":5,"bodyBytesAfter":0}`),
		reqEvent(t, `not json`),
	} {
		if _, ok := decodePruneSaving(bad); ok {
			t.Errorf("should not decode: %+v", bad)
		}
	}
}

// TestSavedTokensAndCost_TierDecidesTheValue is the point of doing this per
// request. The same saved bytes are worth over 12x more on a cache miss than on
// a cache hit, because providers charge ~1.25x input for a write and ~0.1x for a
// read. An aggregate that averages the two describes neither turn.
func TestSavedTokensAndCost_TierDecidesTheValue(t *testing.T) {
	ps, _ := decodePruneSaving(reqEvent(t, wire))

	miss := &pipeline.InferenceExtension{InputTokens: 8881, CacheWriteTokens: 24701}
	hit := &pipeline.InferenceExtension{InputTokens: 26, CacheReadTokens: 24701, CacheWriteTokens: 8907}

	tMiss, usdMiss, ok := savedTokensAndCost(ps, miss)
	if !ok || tMiss <= 0 || usdMiss <= 0 {
		t.Fatalf("miss: tokens=%v usd=%v ok=%v", tMiss, usdMiss, ok)
	}
	_, usdHit, ok := savedTokensAndCost(ps, hit)
	if !ok || usdHit <= 0 {
		t.Fatalf("hit: usd=%v ok=%v", usdHit, ok)
	}
	if r := usdMiss / usdHit; r < 11 || r > 14 {
		t.Errorf("miss/hit cost ratio = %.2f, want ~12.5 — the tier must pick the rate", r)
	}

	// A provider that reports only an aggregate still works.
	agg := &pipeline.InferenceExtension{PromptTokens: 33582}
	if _, _, ok := savedTokensAndCost(ps, agg); !ok {
		t.Error("should fall back to PromptTokens when the split is absent")
	}
	// No usage at all declines rather than dividing by zero.
	if _, _, ok := savedTokensAndCost(ps, &pipeline.InferenceExtension{}); ok {
		t.Error("no usage should not produce a figure")
	}
	if _, _, ok := savedTokensAndCost(ps, nil); ok {
		t.Error("nil usage should not produce a figure")
	}
}

// TestFormatTokensWithSaving: a request row carries its prompt total with the
// saving in parentheses. The total is exact-with-commas (a measured count) and
// the saving compact (a derived estimate), so the two are not read as equally
// precise.
func TestFormatTokensWithSaving(t *testing.T) {
	got := formatTokensWithSaving(681300, 10577.5, false)
	for _, want := range []string{"681,300", "(−10.6k)"} {
		if !strings.Contains(got, want) {
			t.Errorf("cell %q missing %q", got, want)
		}
	}
	// Nothing saved: a bare total, no empty parentheses.
	if got := formatTokensWithSaving(681300, 0, false); got != "681,300" {
		t.Errorf("no-saving cell = %q, want %q", got, "681,300")
	}
	// No total: blank, so rows without a paired response stay empty rather than
	// rendering a lone "0".
	if got := formatTokensWithSaving(0, 10577.5, false); got != "" {
		t.Errorf("no-total cell = %q, want empty", got)
	}
}

// TestFormatUSDWithSaving: both halves at fixed 4dp. formatUSD's variable
// precision would render the total at 3dp and the saving at 4dp, misaligning the
// decimal point in a single column and implying different accuracy.
func TestFormatUSDWithSaving(t *testing.T) {
	got := formatUSDWithSaving(0.2546, 0.0037, false)
	if got != "$0.2546(−$0.0037)" {
		t.Errorf("cell = %q, want %q", got, "$0.2546(−$0.0037)")
	}
	if got := formatUSDWithSaving(0.2546, 0, false); got != "$0.2546" {
		t.Errorf("no-saving cell = %q, want %q", got, "$0.2546")
	}
	if got := formatUSDWithSaving(0, 0.0037, false); got != "" {
		t.Errorf("no-total cell = %q, want empty", got)
	}
}

// TestSavingSignProjected: an observe-mode figure must be visually distinct from
// a realized one, or an operator adds up money that was still spent.
func TestSavingSignProjected(t *testing.T) {
	real := formatTokensWithSaving(681300, 10577.5, false)
	proj := formatTokensWithSaving(681300, 10577.5, true)
	if real == proj {
		t.Fatalf("projected renders identically to realized: %q", real)
	}
	if !strings.Contains(proj, "(~") {
		t.Errorf("projected cell %q should mark the saving with ~", proj)
	}
	if strings.Contains(proj, "−") {
		t.Errorf("projected = %q, must not claim bytes were removed", proj)
	}
	realUSD := formatUSDWithSaving(0.2546, 0.0037, false)
	projUSD := formatUSDWithSaving(0.2546, 0.0037, true)
	if realUSD == projUSD {
		t.Fatalf("projected cost renders identically to realized: %q", realUSD)
	}
	if strings.Contains(projUSD, "−") {
		t.Errorf("projected cost = %q, must not claim money was not spent", projUSD)
	}
}

// TestUnpricedModelShowsNoCost: RateSource "none" means no rate was resolvable,
// so the COST cell must stay blank rather than invent a $0.00 that reads as a
// free prompt. The tokens cell is unaffected — the count is still real.
func TestUnpricedModelShowsNoCost(t *testing.T) {
	resp := &pipeline.InferenceExtension{InputTokens: 100, CacheReadTokens: 500}
	if _, ok := promptCost(pruneSaving{RateSource: "none", RateInput: 1e-6}, resp); ok {
		t.Error("an unpriced model produced a cost figure")
	}
	// A resolved rate does price.
	if _, ok := promptCost(pruneSaving{RateSource: "default", RateInput: 1e-6, RateCacheRead: 1e-7}, resp); !ok {
		t.Error("a priced model produced no cost figure")
	}
}

func TestFormatCompactAndUSD(t *testing.T) {
	for in, want := range map[float64]string{0: "0", 950: "950", 10577.5: "10.6k", 2_400_000: "2.4M"} {
		if got := formatCompact(in); got != want {
			t.Errorf("formatCompact(%v) = %q, want %q", in, got, want)
		}
	}
	// Sub-cent savings must not all round to 0.00.
	for in, want := range map[float64]string{1.5: "1.50", 0.05: "0.050", 0.0004: "0.0004"} {
		if got := formatUSD(in); got != want {
			t.Errorf("formatUSD(%v) = %q, want %q", in, got, want)
		}
	}
}

// TestComputeEventPairs_DuplicateResponseStaysUnpaired: a second response sharing
// a RequestID — a retry, or a streamed reply recorded twice — finds the request
// already paired. Falling through to the adjacency heuristic would have it walk
// back and claim an unrelated earlier request, reintroducing exactly the
// mis-attribution the id was added to end.
func TestComputeEventPairs_DuplicateResponseStaysUnpaired(t *testing.T) {
	ev := func(phase pipeline.SessionPhase, id string, code int) *pipeline.SessionEvent {
		return &pipeline.SessionEvent{
			Direction: pipeline.Outbound, Phase: phase,
			Host: "h", RequestID: id, StatusCode: code,
		}
	}
	rows := []eventRow{
		{event: ev(pipeline.SessionRequest, "aaa", 0)},    // 0
		{event: ev(pipeline.SessionRequest, "bbb", 0)},    // 1
		{event: ev(pipeline.SessionResponse, "bbb", 200)}, // 2 pairs with 1
		{event: ev(pipeline.SessionResponse, "bbb", 500)}, // 3 duplicate for bbb
	}
	_, partner := computeEventPairs(rows)

	if partner[1] != 2 {
		t.Errorf("bbb should pair 1↔2, got %v", partner)
	}
	if j, ok := partner[3]; ok {
		t.Errorf("duplicate response paired with row %d; it must stay unpaired", j)
	}
	if j, ok := partner[0]; ok {
		t.Errorf("request aaa was claimed by the duplicate (row %d) — the bug this guards", j)
	}
}

// TestRequestCells_RequireAnIDMatch: pricing against a heuristically matched
// response could take its cache tier from a different request, and the tiers are
// ~12.5x apart — a wrong figure presented as a measurement. Both request-side
// cells must enforce it, since both read the paired response.
func TestRequestCells_RequireAnIDMatch(t *testing.T) {
	req := reqEvent(t, wire)
	req.RequestID = "aaa"
	resp := &pipeline.SessionEvent{
		Phase: pipeline.SessionResponse, RequestID: "zzz", // different exchange
		Inference: &pipeline.InferenceExtension{CacheWriteTokens: 24701, TotalTokens: 33582},
	}
	rows := []eventRow{{event: req}, {event: resp}}
	partner := map[int]int{0: 1, 1: 0}
	m := &model{}
	if got := m.tokensCell(rows, partner, 0, req); got != "" {
		t.Errorf("tokens priced against a mismatched response: %q", got)
	}
	if got := m.costCell(rows, partner, 0, req); got != "" {
		t.Errorf("cost priced against a mismatched response: %q", got)
	}
	// Matching ids do price.
	resp.RequestID = "aaa"
	if got := m.tokensCell(rows, partner, 0, req); got == "" {
		t.Error("an id-matched pair should show tokens")
	}
	if got := m.costCell(rows, partner, 0, req); got == "" {
		t.Error("an id-matched pair should show a cost")
	}
}

// TestPruneSavingProjectedDecodes guards the wire tag.
func TestPruneSavingProjectedDecodes(t *testing.T) {
	ps, ok := decodePruneSaving(reqEvent(t, `{"bytesRemoved":100,"bodyBytesAfter":1000,"projected":true}`))
	if !ok {
		t.Fatal("decode failed")
	}
	if !ps.Projected {
		t.Error("projected did not decode")
	}
}
