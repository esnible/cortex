package tui

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
)

// pruneSaving is the tool-prune per-request event as published on the request
// event under "tool-prune". Mirrors the plugin's shape; a decode test guards the
// tags.
type pruneSaving struct {
	BytesRemoved   int     `json:"bytesRemoved"`
	BodyBytesAfter int     `json:"bodyBytesAfter"`
	RateInput      float64 `json:"rateInput"`
	RateCacheWrite float64 `json:"rateCacheWrite"`
	RateCacheRead  float64 `json:"rateCacheRead"`
	RateSource     string  `json:"rateSource"`
	// Projected marks observe mode: the saving was measured but the bytes were
	// not actually removed, so it must not render as money already not spent.
	Projected bool `json:"projected"`
}

// decodePruneSaving pulls the tool-prune event off a request event, if present.
func decodePruneSaving(e *pipeline.SessionEvent) (pruneSaving, bool) {
	if e == nil || len(e.Plugins) == 0 {
		return pruneSaving{}, false
	}
	raw, ok := e.Plugins["tool-prune"]
	if !ok {
		return pruneSaving{}, false
	}
	var ps pruneSaving
	if err := json.Unmarshal(raw, &ps); err != nil || ps.BytesRemoved <= 0 || ps.BodyBytesAfter <= 0 {
		return pruneSaving{}, false
	}
	return ps, true
}

// publishedRates rebuilds a pricing.Rates from the rates tool-prune put on its
// event, so the arithmetic below can go through pricing.Cost rather than being a
// second implementation of it.
//
// No output rate: tool-prune deliberately publishes none, because pruning only
// ever shrinks the prompt and attributing output cost to it would be false.
func (ps pruneSaving) publishedRates() pricing.Rates {
	var r pricing.Rates
	for tier, v := range map[pricing.Tier]float64{
		pricing.TierInput:      ps.RateInput,
		pricing.TierCacheWrite: ps.RateCacheWrite,
		pricing.TierCacheRead:  ps.RateCacheRead,
	} {
		if v > 0 {
			r.Base[tier], r.Set[tier] = v, true
		}
	}
	return r
}

// provenance names where this row's rates came from, for display. Empty when the
// producer published none, which is an older proxy rather than an error.
func (ps pruneSaving) provenance() string {
	if ps.RateSource == "" || ps.RateSource == "none" {
		return ""
	}
	return ps.RateSource
}

// savedTokensAndCost converts a request's byte saving into tokens and dollars,
// using the response's own usage.
//
// The two halves live on different events by necessity: the byte saving is known
// when the request is rewritten, and the tier it came out of — and the ratio to
// convert bytes to tokens — only from the response. So this is the last step of an
// arithmetic the plugin starts.
//
// The dollars themselves are computed by pricing.Cost, not here. This function
// supplies inputs and picks the tier via pricing.PromptTier; it holds no rate
// table and no multiplication of its own. Before the consolidation this file
// multiplied rates by tokens itself, which made it one of five places that turned
// tokens into dollars and the only one nothing else tested.
func savedTokensAndCost(ps pruneSaving, resp *pipeline.InferenceExtension) (tokens, usd float64, ok bool) {
	if resp == nil {
		return 0, 0, false
	}
	u := pricing.UsageFromInference(resp)
	prompt := u.PromptTotal()
	if prompt <= 0 {
		return 0, 0, false
	}
	// The plugin calibrates on the request it just sent: prompt tokens over the
	// post-prune body size, both measured on the same request so the two sides
	// agree.
	tokens = float64(ps.BytesRemoved) * float64(prompt) / float64(ps.BodyBytesAfter)
	if tokens <= 0 {
		return 0, 0, false
	}

	// The whole saving is attributed to the one tier it came out of, so the Usage
	// handed to Cost carries a count in that tier alone.
	saved := pricing.Usage{}
	switch pricing.PromptTier(u) {
	case pricing.TierCacheWrite:
		saved.CacheWrite = int(math.Round(tokens))
	case pricing.TierCacheRead:
		saved.CacheRead = int(math.Round(tokens))
	default:
		saved.Input = int(math.Round(tokens))
	}
	micros, priced := pricing.Cost(ps.publishedRates(), saved)
	if !priced {
		// The tier this request used has no rate. Report the token saving, which is
		// still known, and decline the dollars rather than showing a zero that
		// would read as "this saved nothing".
		return tokens, 0, true
	}
	return tokens, float64(micros) / 1e6, true
}

// formatCompact renders a token count tersely enough for a table cell: 10577
// becomes "10.6k". Exact below 1000, where the extra digits still fit.
func formatCompact(v float64) string {
	switch {
	case v >= 1_000_000:
		return fmt.Sprintf("%.1fM", v/1_000_000)
	case v >= 1_000:
		return fmt.Sprintf("%.1fk", v/1_000)
	default:
		return fmt.Sprintf("%.0f", v)
	}
}

// formatUSD keeps small amounts legible: a per-request saving is often fractions
// of a cent, where %.2f would round every row to "0.00".
func formatUSD(v float64) string {
	switch {
	case v >= 1:
		return fmt.Sprintf("%.2f", v)
	case v >= 0.01:
		return fmt.Sprintf("%.3f", v)
	default:
		return fmt.Sprintf("%.4f", v)
	}
}

// formatUSD4 is formatUSD at fixed precision, for the case where two amounts of
// different magnitude share one column and their decimal points must line up.
// Neither returns a "$" — the caller places it, since a saving needs it inside
// the parentheses.
func formatUSD4(v float64) string { return fmt.Sprintf("%.4f", v) }

// usdFloor is the smallest amount four decimal places can state. Anything
// positive below half of it rounds to "0.0000".
const usdFloor = 0.0001

// formatUSDCell renders a dollar amount for a table cell, with the "$" attached
// and a floor below which it says so rather than rounding to zero.
//
// The floor exists because %.4f renders anything under $0.00005 as "$0.0000",
// which reads as "this was free" — the exact reading decodeCostEvent (declining a
// cost of 0) and promptCost (declining an unpriced model rather than showing
// $0.00) both go out of their way to avoid. Reintroducing it at the formatting
// layer would undo both. Reachable on a small cache-read-only request: 100
// cache-read tokens at a typical rate is $0.000038.
func formatUSDCell(v float64) string {
	if v > 0 && v < usdFloor/2 {
		return "<$" + formatUSD4(usdFloor)
	}
	return "$" + formatUSD4(v)
}
