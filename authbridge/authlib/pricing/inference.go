package pricing

import "github.com/rossoctl/cortex/authbridge/authlib/pipeline"

// UsageFromInference reads the per-tier token split that the inference parsers
// publish onto pctx.Extensions.Inference.
//
// This is the one conversion between the parsers' vocabulary and this package's,
// and it lives here so both consumers share it rather than each writing their own
// — the D3 duplication this consolidation exists to remove.
//
// The split counters are preferred over the legacy PromptTokens / CompletionTokens
// aggregates, never the reverse: parsercommon.TokenUsage.Fill derives PromptTokens
// FROM the split, so reading the aggregate would fold the cache tiers into
// uncached input and price cache reads at up to 10x their real rate on
// cache-heavy traffic (which is what Claude Code produces).
//
// The aggregates are the fallback for a provider that reports only totals. Without
// it such traffic has a zero split, and Cost treats zero usage as UNPRICED — so
// the requests would drop out of the dollar total silently instead of being priced
// approximately. Attributing the aggregate to uncached input is the same fallback
// toolprune's OnFinish already makes (plugins/toolprune/plugin.go:705-709): it
// over-prices a cache-heavy request, but a provider that hides its cache split has
// given us no way to do better, and being visibly approximate beats being
// invisibly absent.
//
// The two fallbacks are independent because a provider can report a prompt split
// and only a legacy completion total.
func UsageFromInference(inf *pipeline.InferenceExtension) Usage {
	if inf == nil {
		return Usage{}
	}
	u := Usage{
		Input:      inf.InputTokens,
		CacheWrite: inf.CacheWriteTokens,
		CacheRead:  inf.CacheReadTokens,
		Output:     inf.OutputTokens,
	}
	if u.PromptTotal() == 0 && inf.PromptTokens > 0 {
		u.Input = inf.PromptTokens
	}
	if u.Output == 0 && inf.CompletionTokens > 0 {
		u.Output = inf.CompletionTokens
	}
	return u
}
