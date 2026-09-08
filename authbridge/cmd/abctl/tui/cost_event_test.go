package tui

import (
	"encoding/json"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// costWire is the exact JSON litellm-budget-track publishes under
// "litellm-budget-track".
const costWire = `{"cost_usd":0.2824,"source":"gateway-header",
  "daily_total_usd":1.4207,"daily_max_usd":5}`

// bigPromptWire is a tool-prune event sized for the cache-heavy agent turn this
// split exists for: a ~2.4MB body (≈3.5 bytes/token against a 681k-token prompt)
// with a 34KB tool block removed. The package-level `wire` fixture describes a
// 90KB body, whose byte ratio against a 681k prompt would imply a 31% saving —
// arithmetically valid but not a shape that occurs.
const bigPromptWire = `{"bytesRemoved":34645,"bodyBytesAfter":2384550,
  "model":"claude-opus-5","rateInput":3.8e-06,"rateCacheWrite":4.75e-06,
  "rateCacheRead":3.8e-07,"rateSource":"default"}`

// agentTurn is the usage a long-running agent reports: almost entirely cache
// reads, a small uncached delta, and a modest completion. Sums to TotalTokens so
// the split invariant is checkable.
func agentTurn() *pipeline.InferenceExtension {
	return &pipeline.InferenceExtension{
		InputTokens: 1_300, CacheReadTokens: 680_000,
		OutputTokens: 1_850, TotalTokens: 683_150,
	}
}

func respEvent(raw string, inf *pipeline.InferenceExtension) *pipeline.SessionEvent {
	e := &pipeline.SessionEvent{Phase: pipeline.SessionResponse, Inference: inf}
	if raw != "" {
		e.Plugins = map[string]json.RawMessage{"litellm-budget-track": json.RawMessage(raw)}
	}
	return e
}

// TestDecodeCostEvent guards the tags against drift with the plugin's struct. A
// silent decode failure would leave the COST column blank, which is
// indistinguishable from an unpriced call — so the tags are the only thing
// standing between "free" and "not reported".
func TestDecodeCostEvent(t *testing.T) {
	ce, ok := decodeCostEvent(respEvent(costWire, nil))
	if !ok {
		t.Fatal("failed to decode the published event")
	}
	if ce.CostUSD != 0.2824 {
		t.Errorf("CostUSD = %v, want 0.2824", ce.CostUSD)
	}
	if ce.Source != "gateway-header" {
		t.Errorf("Source = %q, want gateway-header", ce.Source)
	}
	if ce.DailyTotalUSD != 1.4207 || ce.DailyMaxUSD != 5 {
		t.Errorf("daily fields did not decode: %+v", ce)
	}
	// Absent, malformed, and unpriced all decline rather than render $0.0000,
	// which would read as a call that cost nothing.
	for _, bad := range []*pipeline.SessionEvent{
		nil,
		{Phase: pipeline.SessionResponse},
		respEvent(`{"cost_usd":0,"source":"gateway-header"}`, nil),
		respEvent(`not json`, nil),
	} {
		if _, ok := decodeCostEvent(bad); ok {
			t.Errorf("should not decode: %+v", bad)
		}
	}
}

// TestExchangeCellsSumToTotal is the invariant behind splitting the column: the
// request row's prompt count plus the response row's generated count equal the
// billed total. Before the split the response row showed TotalTokens, so the
// prompt was counted once on each row and the generated tokens were invisible.
func TestExchangeCellsSumToTotal(t *testing.T) {
	inf := agentTurn()
	req := reqEvent(t, bigPromptWire)
	req.RequestID = "aaa"
	resp := respEvent(costWire, inf)
	resp.RequestID = "aaa"
	rows := []eventRow{{event: req}, {event: resp}}
	partner := map[int]int{0: 1, 1: 0}
	m := &model{}

	// 1,300 + 680,000 = 681,300 prompt tokens, with the saving in parens:
	// 34,645 bytes × 681,300 tokens / 2,384,550 bytes ≈ 9.9k tokens.
	gotReq := m.tokensCell(rows, partner, 0, req)
	if gotReq != "681,300(−9.9k)" {
		t.Errorf("request TOKENS = %q, want %q", gotReq, "681,300(−9.9k)")
	}
	gotResp := m.tokensCell(rows, partner, 1, resp)
	if gotResp != "1,850" {
		t.Errorf("response TOKENS = %q, want %q", gotResp, "1,850")
	}
	if want := promptTokens(inf) + inf.OutputTokens; want != inf.TotalTokens {
		t.Errorf("the two rows sum to %d but the provider billed %d", want, inf.TotalTokens)
	}
}

// TestCostCellPhases: the request row is a modelled prompt figure, the response
// row the authoritative exchange total. The response figure INCLUDES the request
// figure — it is not a generated-token cost, because no plugin publishes an
// output rate.
func TestCostCellPhases(t *testing.T) {
	inf := agentTurn()
	req := reqEvent(t, bigPromptWire)
	req.RequestID = "aaa"
	resp := respEvent(costWire, inf)
	resp.RequestID = "aaa"
	rows := []eventRow{{event: req}, {event: resp}}
	partner := map[int]int{0: 1, 1: 0}
	m := &model{}

	// Prompt: 1,300 × 3.8e-6 + 680,000 × 3.8e-7 = 0.00494 + 0.2584 = 0.26334.
	// Saving: 9.9k tokens at the cache-read rate = 0.0038.
	if got := m.costCell(rows, partner, 0, req); got != "$0.2633(−$0.0038)" {
		t.Errorf("request COST = %q, want %q", got, "$0.2633(−$0.0038)")
	}
	if got := m.costCell(rows, partner, 1, resp); got != "$0.2824" {
		t.Errorf("response COST = %q, want %q", got, "$0.2824")
	}
	// Without the budget-track event there is no authoritative total, so the
	// response cost is blank rather than modelled.
	if got := m.costCell(rows, partner, 1, respEvent("", inf)); got != "" {
		t.Errorf("unpriced response COST = %q, want empty", got)
	}
}

// TestPromptCostIsTierWeighted: pricing the whole prompt at the uncached input
// rate overstates a cache-heavy turn by close to an order of magnitude, which is
// the common shape for a long-running agent. The weighted figure must be well
// below the flat one.
func TestPromptCostIsTierWeighted(t *testing.T) {
	ps := pruneSaving{RateInput: 3.8e-6, RateCacheRead: 3.8e-7, RateCacheWrite: 4.75e-6, RateSource: "default"}
	inf := &pipeline.InferenceExtension{InputTokens: 1_000, CacheReadTokens: 99_000}
	got, ok := promptCost(ps, inf)
	if !ok {
		t.Fatal("no cost figure")
	}
	flat := float64(promptTokens(inf)) * ps.RateInput
	if got >= flat {
		t.Errorf("weighted %v should be below flat %v", got, flat)
	}
	want := 1_000*3.8e-6 + 99_000*3.8e-7
	if got < want-1e-12 || got > want+1e-12 {
		t.Errorf("promptCost = %v, want %v", got, want)
	}
	// nil usage yields no figure rather than a zero that reads as free.
	if _, ok := promptCost(ps, nil); ok {
		t.Error("nil usage produced a cost figure")
	}
}

// TestGeneratedTokensCellFallsBackToCompletion: providers that report only the
// aggregate still get a generated count, or the response row would go blank on
// exactly the dialects that motivated the split.
func TestGeneratedTokensCellFallsBackToCompletion(t *testing.T) {
	if got := generatedTokensCell(respEvent("", &pipeline.InferenceExtension{CompletionTokens: 1_850})); got != "1,850" {
		t.Errorf("cell = %q, want 1,850", got)
	}
	// No usage at all: blank, not "0".
	for _, inf := range []*pipeline.InferenceExtension{nil, {}} {
		if got := generatedTokensCell(respEvent("", inf)); got != "" {
			t.Errorf("cell = %q, want empty", got)
		}
	}
}

// TestPromptTokensPrefersSplitOverAggregate: the split counters are the
// authoritative shape; PromptTokens is a fallback for providers that expose only
// it. Preferring the aggregate would double-count on providers that send both.
func TestPromptTokensPrefersSplitOverAggregate(t *testing.T) {
	both := &pipeline.InferenceExtension{InputTokens: 100, CacheReadTokens: 400, PromptTokens: 500}
	if got := promptTokens(both); got != 500 {
		t.Errorf("promptTokens = %d, want 500", got)
	}
	only := &pipeline.InferenceExtension{PromptTokens: 500}
	if got := promptTokens(only); got != 500 {
		t.Errorf("aggregate-only promptTokens = %d, want 500", got)
	}
	if got := promptTokens(nil); got != 0 {
		t.Errorf("nil promptTokens = %d, want 0", got)
	}
}
