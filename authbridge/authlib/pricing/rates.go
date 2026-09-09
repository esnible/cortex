// Package pricing owns model rates and the arithmetic that turns tokens into
// dollars. It is the single source of truth for both: before it, rates lived in
// two plugins' configs and dollars were computed in five places, which is how a
// stale "understates by 4x" comment survived a gateway repricing.
//
// The package holds no I/O and no provider knowledge. A rate table is built once
// at startup and swapped atomically; resolution is a pure function of
// (endpoint, model, prompt size).
package pricing

// Tier names which kind of token is being priced.
//
// Four tiers, because that is what every provider on a Claude path bills
// separately and prompt-cache tiers differ by more than 12x: a cache write is
// 1.25x input and a cache read 0.1x, so one flat rate misprices cache-heavy
// traffic (Claude Code) by up to ~10x.
//
// Deliberately absent: service-tier variants (flex / priority / ultrafast) and
// the 1-hour-TTL cache-write premium. Both are real fields in LiteLLM's
// ModelInfoBase, neither is on a path we run, and an unpopulated tier is an
// untested tier. Adding one later is a field plus a threshold row, not a reshape.
type Tier int

const (
	TierInput Tier = iota
	TierCacheWrite
	TierCacheRead
	TierOutput
)

// numTiers sizes the per-tier arrays. Arrays rather than named fields so the
// arithmetic is a loop over every tier: an enumerated form is where a forgotten
// tier hides, and a forgotten tier silently prices at zero.
const numTiers = 4

// Usage is one request's token count, split by tier.
//
// This mirrors parsercommon.TokenUsage, which is what the inference parsers
// already publish — but parsercommon sits under authlib/plugins/internal/, so
// Go's internal rule puts it out of reach here. Reasoning is omitted (a subset of
// Output, already counted) and so is ReportedTotal (an aggregate no tier prices).
type Usage struct {
	Input      int // uncached prompt tokens
	CacheWrite int // prompt tokens written to cache
	CacheRead  int // prompt tokens served from cache
	Output     int // generated completion tokens
}

// PromptTotal is the prompt-side total, which is what a context threshold is
// measured against. Output is excluded: a long-context premium is priced on how
// much prompt was sent, not on what came back.
func (u Usage) PromptTotal() int { return u.Input + u.CacheWrite + u.CacheRead }

// tokens indexes the counts by Tier so Cost can loop over every tier.
func (u Usage) tokens() [numTiers]int {
	return [numTiers]int{
		TierInput:      u.Input,
		TierCacheWrite: u.CacheWrite,
		TierCacheRead:  u.CacheRead,
		TierOutput:     u.Output,
	}
}

// ContextThreshold is a long-context rate override: above AbovePromptTokens the
// tiers it Sets replace the base ones.
//
// Tiers it does not set keep their base rate rather than becoming unset. A table
// that prices only long-context input would otherwise unset output above the
// threshold, and Cost's per-tier invariant would flip the whole request to
// unpriced — turning a partial price list into a coverage hole.
type ContextThreshold struct {
	AbovePromptTokens int
	Rate              [numTiers]float64
	Set               [numTiers]bool
}

// Rates is one (endpoint, model) pair's rates, in USD per token.
//
// Set exists because a zero rate and an absent rate are different answers. Zero
// means free; absent means unknown, and Cost refuses to price a request whose
// tokens land on an absent tier (see Cost).
type Rates struct {
	Base       [numTiers]float64
	Set        [numTiers]bool
	Thresholds []ContextThreshold
}

// At flattens Rates for a prompt of promptTotal tokens: the highest threshold
// strictly exceeded, overlaid on Base, with Thresholds cleared.
//
// Strictly exceeded, not met — "$X above 200k tokens" means the premium starts at
// token 200,001, so a request of exactly 200k pays base.
//
// The scan takes the maximum exceeded threshold rather than the first, so At does
// not depend on slice order. Order-dependence here would be a footgun with no
// visible symptom: a config or test that happened to list thresholds ascending
// would price long context at the lower premium and look plausible.
//
// Idempotent: the result carries no thresholds, so flattening it again returns it
// unchanged. Resolve hands out flattened rates and Cost flattens again.
func (r Rates) At(promptTotal int) Rates {
	out := Rates{Base: r.Base, Set: r.Set}
	best := -1
	var pick *ContextThreshold
	for i := range r.Thresholds {
		t := &r.Thresholds[i]
		if promptTotal > t.AbovePromptTokens && t.AbovePromptTokens > best {
			best, pick = t.AbovePromptTokens, t
		}
	}
	if pick == nil {
		return out
	}
	for i := range out.Base {
		if pick.Set[i] {
			out.Base[i], out.Set[i] = pick.Rate[i], true
		}
	}
	return out
}

// For returns the rate for one tier and whether a rate is actually available.
//
// The bool is the per-tier invariant in accessor form, and it is the whole reason
// this is not a plain field read: Set is per tier, so a model priced only for cache
// reads must report "no rate" for a cache write rather than the zero value. Cost
// enforces the same rule across a whole request; consumers that publish individual
// rates (tool-prune's per-request event) use this.
//
// Call it on rates from Resolve or At, which are already flattened for the
// request's prompt size. On unflattened rates it reads the BASE tier and ignores
// thresholds — correct for "what is this model's list rate", wrong for pricing a
// specific request.
func (r Rates) For(t Tier) (float64, bool) {
	if int(t) < 0 || int(t) >= numTiers {
		return 0, false
	}
	return r.Base[t], r.Set[t]
}

// any reports whether these rates price anything at all, at any prompt size.
// NewTable uses it to reject a row that prices nothing: such a row would match
// traffic and then resolve it as unpriced, which is indistinguishable from having
// no row and hides the typo that produced it.
func (r Rates) any() bool {
	for _, s := range r.Set {
		if s {
			return true
		}
	}
	for _, t := range r.Thresholds {
		for _, s := range t.Set {
			if s {
				return true
			}
		}
	}
	return false
}
