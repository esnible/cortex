package pipeline

// PromptTokens is the request's own billed token count: what the provider counted for
// everything we sent. It lives on the response because the provider is the only party that
// tokenizes, but it is a request-side quantity — which is what lets a request row show a
// total at all.
//
// OUTPUT IS DELIBERATELY EXCLUDED, which the prompt-context rule measured on the same
// sessions: output is 0.003%-2.2% of the prompt and 0.2% on conversations near the context
// limit, so folding it in would change no rendered pixel while making the figure mean
// something else.
//
// The PromptTokens fallback cannot currently fire, and is kept only to mirror
// savedTokensAndCost: parsercommon.TokenUsage.Fill is the sole production writer of these
// fields and sets PromptTokens to Input+CacheRead+CacheWrite — the same sum computed here —
// so when the split is zero the aggregate is zero too. It costs nothing and would start
// earning its keep if a parser ever published the aggregate directly.
func PromptTokens(resp *InferenceExtension) int {
	if resp == nil {
		return 0
	}
	if n := resp.InputTokens + resp.CacheReadTokens + resp.CacheWriteTokens; n > 0 {
		return n
	}
	return resp.PromptTokens
}
