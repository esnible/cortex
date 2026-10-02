package pricing

// zenHost is OpenCode Zen's endpoint, opencode.ai/zen.
const zenHost = "opencode.ai"

// bundledFreeRates are models whose calls cost nothing, shipped at a rate of zero so a
// call to one reads as a priced zero rather than as unpriced traffic.
//
// HAND-MAINTAINED, deliberately not generated, like bundledMultipliers: bundled.go is
// derived from LiteLLM's price map, which carries none of these, and `make pricing-table`
// must never be able to clobber them. Shipped rather than left to config because config
// cannot say zero — a zero rate there leaves its tier unset, so the call stays unpriced.
//
// SCOPED to the endpoint that gives the model away. "-free" is OpenCode Zen's naming for
// its free models (opencode.ai/docs/zen lists them, and every one but big-pickle carries
// the suffix), and only Zen's: another endpoint's "-free" model is its own business, so
// it stays unpriced until someone states a rate. A Zen model that stops being free keeps
// its old id only if Zen keeps the suffix, which is the convention this relies on; a
// configured rate for it outranks this row either way.
func bundledFreeRates() []Entry {
	free := Rates{Set: [numTiers]bool{TierInput: true, TierCacheWrite: true, TierCacheRead: true, TierOutput: true}}
	return []Entry{
		{Host: zenHost, Model: "*-free", Prov: ProvBundled, Rates: free},
		// Zen's one free model without the suffix: "a stealth model that's free on
		// OpenCode for a limited time". Drop it when Zen does.
		{Host: zenHost, Model: "big-pickle", Prov: ProvBundled, Rates: free},
	}
}
