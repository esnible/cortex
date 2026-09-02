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

func TestFormatSavedCell(t *testing.T) {
	got := formatSavedCell(33650, 10577.5, 0.05024, "default")
	for _, want := range []string{"33,650", "−10.6k", "$0.050"} {
		if !strings.Contains(got, want) {
			t.Errorf("cell %q missing %q", got, want)
		}
	}
	// No saving: the plain total, so unrelated rows are untouched.
	if got := formatSavedCell(33650, 0, 0, "default"); got != "33,650" {
		t.Errorf("no-saving cell = %q, want the bare total", got)
	}
	// Unpriced model: tokens shown, no dollar figure invented.
	got = formatSavedCell(33650, 10577.5, 0, "none")
	if strings.Contains(got, "$") {
		t.Errorf("cell %q shows a price for an unpriced model", got)
	}
	if !strings.Contains(got, "−10.6k") {
		t.Errorf("cell %q should still show the token saving", got)
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
