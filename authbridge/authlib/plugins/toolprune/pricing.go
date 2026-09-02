package toolprune

// defaultPricing holds per-token rates for the models seen on the rossoctl
// LiteLLM gateway, measured from its own x-litellm-response-cost headers:
// send two non-streaming requests of differing prompt length and difference
// them, rate = Δcost / Δinput_tokens; the cache tiers were obtained the same
// way with a cache_control block sent twice.
//
// These exist so `$ saved` works with no configuration, which is the difference
// between a number an operator sees and one they never get around to enabling.
// They are a starting point, not a fact about your account:
//
//   - Rates are gateway-specific. This gateway bills well below Anthropic list;
//     a deployment talking straight to the vendor pays more, so these would
//     understate its saving.
//   - Rates change. Nothing here refreshes them.
//
// A figure derived from these is therefore labelled as coming from default
// rates wherever it is reported, so it cannot be mistaken for one measured on
// the operator's own account. Any `pricing` entry in config overrides the
// matching model outright.
//
// Keys are lower-case; lookup folds the observed model name the same way.
var defaultPricing = map[string]modelRates{
	// input 1.00x / cache write 1.25x / cache read 0.10x
	"claude-opus-5": {
		InputCostPerToken:      0.0000038,
		CacheWriteCostPerToken: 0.00000475,
		CacheReadCostPerToken:  0.00000038,
	},
	"aws/claude-opus-5": {
		InputCostPerToken:      0.0000038,
		CacheWriteCostPerToken: 0.00000475,
		CacheReadCostPerToken:  0.00000038,
	},
	"aws/claude-sonnet-5": {
		InputCostPerToken:      0.00000152,
		CacheWriteCostPerToken: 0.0000019,
		CacheReadCostPerToken:  0.000000152,
	},
	"aws/claude-haiku-4-5": {
		InputCostPerToken:      0.00000076,
		CacheWriteCostPerToken: 0.00000095,
		CacheReadCostPerToken:  0.000000076,
	},
	"claude-haiku-4-5-20251001": {
		InputCostPerToken:      0.00000076,
		CacheWriteCostPerToken: 0.00000095,
		CacheReadCostPerToken:  0.000000076,
	},
}

func (s rateSource) String() string {
	switch s {
	case rateConfigured:
		return "configured"
	case rateDefault:
		return "default"
	default:
		return "none"
	}
}
