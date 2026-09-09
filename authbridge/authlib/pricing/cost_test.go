package pricing

import "testing"

// claudeOpus is a fully-priced entry in the published unit.
func claudeOpus() Rates {
	return Rates{
		Base: [numTiers]float64{
			TierInput:      3.80 / perMillion,
			TierCacheWrite: 4.75 / perMillion,
			TierCacheRead:  0.38 / perMillion,
			TierOutput:     19.00 / perMillion,
		},
		Set: [numTiers]bool{TierInput: true, TierCacheWrite: true, TierCacheRead: true, TierOutput: true},
	}
}

func TestCost_AllFourTiers(t *testing.T) {
	// 1000*3.80 + 2000*4.75 + 10000*0.38 + 500*19.00, per million
	//   = 3800 + 9500 + 3800 + 9500 micros
	u := Usage{Input: 1000, CacheWrite: 2000, CacheRead: 10_000, Output: 500}
	micros, ok := Cost(claudeOpus(), u)
	if !ok {
		t.Fatal("Cost reported unpriced for a fully-priced model")
	}
	if want := int64(26_600); micros != want {
		t.Errorf("Cost = %d micros, want %d", micros, want)
	}
}

// TestCost_PerTierInvariant is the regression this function exists to hold. A
// model priced only for cache reads must not price a cache-write request at zero.
func TestCost_PerTierInvariant(t *testing.T) {
	cacheReadOnly := Rates{
		Base: [numTiers]float64{TierCacheRead: 0.38 / perMillion},
		Set:  [numTiers]bool{TierCacheRead: true},
	}

	if _, ok := Cost(cacheReadOnly, Usage{CacheWrite: 5000}); ok {
		t.Error("a cache-write request priced against a cache-read-only model reported priced")
	}
	// The mirror: the tier it does price still prices.
	micros, ok := Cost(cacheReadOnly, Usage{CacheRead: 10_000})
	if !ok {
		t.Fatal("a cache-read request against a cache-read rate reported unpriced")
	}
	if want := int64(3_800); micros != want {
		t.Errorf("Cost = %d micros, want %d", micros, want)
	}
}

func TestCost_UnsetTierWithNoTokensStillPrices(t *testing.T) {
	// Absent output rate, zero output tokens. The invariant is about tiers that
	// CARRIED tokens; refusing here would unprice every request on a prompt-only
	// rate table (which is exactly what toolprune ships, having no output rate).
	noOutput := Rates{
		Base: [numTiers]float64{TierInput: 3.80 / perMillion},
		Set:  [numTiers]bool{TierInput: true},
	}
	micros, ok := Cost(noOutput, Usage{Input: 1000})
	if !ok {
		t.Fatal("reported unpriced when the only unset tier carried no tokens")
	}
	if want := int64(3_800); micros != want {
		t.Errorf("Cost = %d micros, want %d", micros, want)
	}
}

func TestCost_ZeroUsageIsUnpriced(t *testing.T) {
	// No tokens reported at all is unknown usage, not a free request. Calling it
	// "priced $0" would put it in the priced denominator and dilute coverage.
	if _, ok := Cost(claudeOpus(), Usage{}); ok {
		t.Error("empty usage reported priced")
	}
}

func TestCost_NegativeTokensRefused(t *testing.T) {
	// A negative count is a parser bug or a hostile body. Refuse rather than emit
	// a negative cost, which would corrode a running total that nothing re-derives.
	if _, ok := Cost(claudeOpus(), Usage{Input: -1, Output: 100}); ok {
		t.Error("negative token count reported priced")
	}
}

func TestCost_UnpricedModelIsUnpriced(t *testing.T) {
	if _, ok := Cost(Rates{}, Usage{Input: 1000}); ok {
		t.Error("empty Rates reported priced")
	}
}

func TestCost_AppliesContextThreshold(t *testing.T) {
	r := claudeOpus()
	r.Thresholds = []ContextThreshold{{
		AbovePromptTokens: 200_000,
		Rate:              [numTiers]float64{TierInput: 7.60 / perMillion},
		Set:               [numTiers]bool{TierInput: true},
	}}

	// 300k prompt tokens, all uncached: 300000 * 7.60/1e6 = 2.28 USD.
	micros, ok := Cost(r, Usage{Input: 300_000})
	if !ok {
		t.Fatal("long-context request reported unpriced")
	}
	if want := int64(2_280_000); micros != want {
		t.Errorf("Cost above threshold = %d micros, want %d", micros, want)
	}

	// And below it, the base rate: 100000 * 3.80/1e6 = 0.38 USD.
	micros, ok = Cost(r, Usage{Input: 100_000})
	if !ok {
		t.Fatal("short request reported unpriced")
	}
	if want := int64(380_000); micros != want {
		t.Errorf("Cost below threshold = %d micros, want %d", micros, want)
	}
}

func TestCost_ThresholdMeasuredOnPromptNotOutput(t *testing.T) {
	r := claudeOpus()
	r.Thresholds = []ContextThreshold{{
		AbovePromptTokens: 200_000,
		Rate:              [numTiers]float64{TierInput: 7.60 / perMillion},
		Set:               [numTiers]bool{TierInput: true},
	}}
	// 100k prompt with 500k output must NOT trip a prompt-size threshold.
	micros, ok := Cost(r, Usage{Input: 100_000, Output: 1})
	if !ok {
		t.Fatal("reported unpriced")
	}
	if want := int64(380_000 + 19); micros != want {
		t.Errorf("Cost = %d micros, want %d (threshold tripped on output)", micros, want)
	}
}
