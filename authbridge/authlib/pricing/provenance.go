package pricing

// Provenance says where a rate came from, so every reported figure carries its
// own credibility instead of looking equally authoritative either way.
//
// Ordered by precedence: a higher value wins a resolution. That is the whole
// reason these are ranked rather than named — see Table.Resolve.
type Provenance int

const (
	// ProvNone means no rate was found. The request is unpriced, which is not the
	// same as free.
	ProvNone Provenance = iota
	// ProvBundled is the price table shipped in the binary. A starting point that
	// needs no configuration, not a fact about the operator's account.
	ProvBundled
	// ProvDiscovered is a rate fetched from the gateway's own /model/info. Phase 7
	// produces these; nothing in this PR does.
	ProvDiscovered
	// ProvConfigured is an explicit pricing.endpoints[].models entry. An override
	// is an override, so it outranks a fetched value.
	ProvConfigured
	// ProvAuthoritative is a real observed cost for one request — LiteLLM's
	// X-Litellm-Response-Cost or its -Original variant. Not a rate but a settled
	// figure, so it never appears in a rate table and bypasses Cost entirely.
	// NewTable rejects it.
	ProvAuthoritative
)

func (p Provenance) String() string {
	switch p {
	case ProvBundled:
		return "bundled"
	case ProvDiscovered:
		return "discovered"
	case ProvConfigured:
		return "configured"
	case ProvAuthoritative:
		return "authoritative"
	default:
		return "none"
	}
}

// Resolver hands out rates for a request. An interface so a plugin can be tested
// against a fixed table, and so discovery (phase 7) can wrap a Table rather than
// reach inside it.
//
// promptTotal is the request's prompt-side token count: the returned Rates are
// already flattened through Rates.At for that size, so a caller cannot forget to
// apply a long-context premium.
type Resolver interface {
	Resolve(endpoint, model string, promptTotal int) (Rates, Provenance)
}
