package tui

import (
	"encoding/json"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// costEvent is the litellm-budget-track per-response event as published under
// "litellm-budget-track". Mirrors the plugin's shape; a decode test guards the
// tags.
//
// Unlike tool-prune's event, this one carries a finished dollar figure rather
// than rates: the plugin reads the authoritative post-discount cost out of
// LiteLLM's response header, so there is nothing left for a consumer to compute.
type costEvent struct {
	CostUSD float64 `json:"cost_usd"`
	// Source is "gateway-header" (authoritative) or "usage-fallback" (priced
	// from token counters, used for streamed responses whose header reports 0).
	Source        string  `json:"source"`
	DailyTotalUSD float64 `json:"daily_total_usd"`
	DailyMaxUSD   float64 `json:"daily_max_usd"`
}

// decodeCostEvent pulls the litellm-budget-track event off a response event, if
// present. Absent whenever the plugin is not in the pipeline, or the response
// was not priced (a cache hit / error charges nothing and emits nothing) — so a
// false return is the normal case, not an error.
func decodeCostEvent(e *pipeline.SessionEvent) (costEvent, bool) {
	if e == nil || len(e.Plugins) == 0 {
		return costEvent{}, false
	}
	raw, ok := e.Plugins["litellm-budget-track"]
	if !ok {
		return costEvent{}, false
	}
	var ce costEvent
	if err := json.Unmarshal(raw, &ce); err != nil || ce.CostUSD <= 0 {
		return costEvent{}, false
	}
	return ce, true
}

// promptCost models what the prompt of one request cost, from the response's
// per-tier token counts and the rates tool-prune published on the request.
//
// Tier-weighted rather than a flat prompt × single-rate: providers bill a cache
// read at ~0.1x the uncached input rate and a cache write at ~1.25x, so a
// cache-heavy turn (the common case for a long-running agent) is overstated by
// close to an order of magnitude by flat pricing. The three rates needed are
// exactly the three tool-prune already carries.
//
// This is a model, not a measurement — the authoritative figure only exists for
// the exchange as a whole, on the response, and only when litellm-budget-track
// is running. Rates resolved as "none" yield ok=false rather than a $0.00 that
// would read as a free prompt.
func promptCost(ps pruneSaving, resp *pipeline.InferenceExtension) (usd float64, ok bool) {
	if resp == nil || ps.RateSource == "none" {
		return 0, false
	}
	usd = float64(resp.InputTokens)*ps.RateInput +
		float64(resp.CacheReadTokens)*ps.RateCacheRead +
		float64(resp.CacheWriteTokens)*ps.RateCacheWrite
	if usd <= 0 {
		return 0, false
	}
	return usd, true
}

// promptTokens is the request's own billed token count: what the provider
// counted for everything we sent. It lives on the response because the provider
// is the only party that tokenizes, but it is a request-side quantity — which is
// what lets a request row show a total at all.
//
// Falls back to the reported aggregate when a provider exposes only PromptTokens
// without the split, mirroring savedTokensAndCost.
func promptTokens(resp *pipeline.InferenceExtension) int {
	if resp == nil {
		return 0
	}
	if n := resp.InputTokens + resp.CacheReadTokens + resp.CacheWriteTokens; n > 0 {
		return n
	}
	return resp.PromptTokens
}

// savingSign distinguishes a realized saving from a projected one. A projected
// saving (on_error: observe, where bytes were measured but not removed) uses "~"
// instead of "−": the money was still spent, and rendering it identically would
// invite an operator to subtract it twice.
func savingSign(projected bool) string {
	if projected {
		return "~"
	}
	return "−"
}

// formatTokensWithSaving renders "681,300(−9.9k)" — the total this row is
// responsible for, with what was kept off it in parentheses. A bare total when
// there was no saving.
//
// The total is exact-with-commas and the saving compact, matching how each is
// already rendered today: the total is a measured count worth reading precisely,
// the saving a derived estimate where trailing digits would be false precision.
func formatTokensWithSaving(total int, saved float64, projected bool) string {
	if total <= 0 {
		return ""
	}
	cell := formatCount(total)
	if saved <= 0 {
		return cell
	}
	return cell + "(" + savingSign(projected) + formatCompact(saved) + ")"
}

// formatUSDWithSaving is formatTokensWithSaving for money: "$0.2546(−$0.0037)".
//
// Both halves are rendered at a fixed 4 decimal places rather than through
// formatUSD's variable precision. Stacked in one column, "$0.255" above
// "−$0.0037" misaligns the decimal point and reads as though the two figures
// were measured to different accuracy.
func formatUSDWithSaving(total float64, saved float64, projected bool) string {
	if total <= 0 {
		return ""
	}
	cell := "$" + formatUSD4(total)
	if saved <= 0 {
		return cell
	}
	return cell + "(" + savingSign(projected) + "$" + formatUSD4(saved) + ")"
}
