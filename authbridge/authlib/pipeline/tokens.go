package pipeline

// PromptTokens is what a response's prompt cost, in tokens: input plus both cache tiers.
//
// OUTPUT IS DELIBERATELY EXCLUDED. Measured across six live sessions it is 0.003%-2.2% of the
// prompt and 0.2% on conversations near the context limit — a fifth of one eighth-block at 1M,
// so including it would change no rendered pixel while making the figure mean something else.
//
// The three-way sum FIRST, PromptTokens second, because zero on the sum means "this provider
// reported the breakdown" and a provider that reports only a total sets the scalar instead.
// Taking the scalar first would silently discard the cache tiers, which on a cached
// conversation are ~99% of the prompt.
func PromptTokens(resp *InferenceExtension) int {
	if resp == nil {
		return 0
	}
	if n := resp.InputTokens + resp.CacheReadTokens + resp.CacheWriteTokens; n > 0 {
		return n
	}
	return resp.PromptTokens
}
