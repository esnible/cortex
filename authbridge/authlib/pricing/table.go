package pricing

import (
	"fmt"
	"strings"

	"github.com/gobwas/glob"
)

// Entry is one row of a rate table: rates for the models matching Model on the
// endpoints matching Host.
//
// Host "" and "*" both mean any endpoint — how the bundled slice is scoped, since
// a shipped table cannot know an operator's gateway names. Model is a glob
// matched case-insensitively.
type Entry struct {
	Host  string
	Model string
	Rates Rates
	Prov  Provenance
}

// Table is an immutable resolved rate table. Build one with NewTable; never
// mutate one that is live, because a Registry hands the same pointer to every
// concurrent reader.
type Table struct{ rows []row }

type row struct {
	host  string // lower-cased; "" or "*" means any
	model modelMatcher
	spec  specificity
	rates Rates
	prov  Provenance
}

// modelMatcher is one compiled model pattern.
//
// Compiled with NO separator passed to glob.Compile, so "*" spans both "-" and
// "/" — "*claude-opus-*" has to match "aws/claude-opus-4-1-20250805". That is
// toolprune/pricing.go:65-67's rule, and it differs from the "."-delimited host
// globs elsewhere in authlib, which is why the two dimensions do not share a
// matcher.
type modelMatcher struct {
	pattern string
	g       glob.Glob
	// gPrefixed matches the same pattern behind a provider prefix, so a literal
	// key also matches "anthropic/<key>" and "aws/<key>".
	//
	// This is load-bearing, not a convenience. A metacharacter-free key compiles to
	// a literal matcher, so "claude-opus-4-1" did not match
	// "anthropic/claude-opus-4-1"; that name fell through to the family glob, which
	// carries the NEWEST member's rates. The result was the exact error the
	// generated table exists to fix — opus-4-1 priced at opus-5's $5/Mtok instead
	// of $15 — for every gateway that echoes a provider-prefixed model name, and
	// long-context thresholds were lost the same way.
	gPrefixed glob.Glob
	exact     bool // no metacharacters: names exactly one model
}

// globMeta are the characters that make a pattern a glob rather than a literal.
const globMeta = `*?[]{}!\`

func compileModel(pattern string) (modelMatcher, error) {
	lower := strings.ToLower(pattern)
	g, err := glob.Compile(lower)
	if err != nil {
		return modelMatcher{}, fmt.Errorf("pricing: model pattern %q: %w", pattern, err)
	}
	// Only literal keys get the prefix-tolerant form. A pattern that already
	// contains metacharacters is the operator's own business, and silently
	// widening it would make "claude-*" match "vertex_ai/claude-*" against their
	// intent.
	exact := !strings.ContainsAny(lower, globMeta)
	m := modelMatcher{pattern: lower, g: g, exact: exact}
	if exact {
		gp, err := glob.Compile("*/" + lower)
		if err != nil {
			return modelMatcher{}, fmt.Errorf("pricing: model pattern %q: %w", pattern, err)
		}
		m.gPrefixed = gp
	}
	return m, nil
}

// match lower-cases the subject because gateways vary in how they echo model
// names, and a case mismatch would silently unprice the traffic rather than fail
// visibly (toolprune/plugin.go:224-226).
func (m modelMatcher) match(model string) bool {
	lower := strings.ToLower(model)
	if m.g.Match(lower) {
		return true
	}
	return m.gPrefixed != nil && m.gPrefixed.Match(lower)
}

// specificity ranks how tightly a row names its target, for tie-breaks WITHIN one
// provenance level.
//
// The endpoint axis is compared before the model axis. Rates differ more between
// a discounted gateway and vendor list than between two models on one endpoint,
// so a deployment-specific host row must not be shadowed by a broader model glob.
// Within each axis: exact beats glob, then longer beats shorter — toolprune's rule
// (plugin.go:190-209, pricing.go:80-85). The pattern string is the final tie-break
// so two equally specific rows resolve the same way across restarts instead of
// whichever map iteration reached first.
type specificity struct {
	namedHost bool
	// exactHost is true when the host pattern is a literal, so the host it names
	// beats a glob that merely covers it.
	//
	// Its absence was a live defect: at equal pattern length the final tie-break
	// decided, and "*" (0x2A) sorts before any letter, so "*.internal" beat
	// "a.internal" for traffic to a.internal — in either slice order. An operator
	// pinning one discounted gateway beside a broader "*.internal" block silently
	// got the broad rate.
	exactHost bool
	hostLen   int

	exactModel bool
	modelLen   int
	pattern    string
}

func (s specificity) beats(o specificity) bool {
	switch {
	case s.namedHost != o.namedHost:
		return s.namedHost
	case s.exactHost != o.exactHost:
		return s.exactHost
	case s.hostLen != o.hostLen:
		return s.hostLen > o.hostLen
	case s.exactModel != o.exactModel:
		return s.exactModel
	case s.modelLen != o.modelLen:
		return s.modelLen > o.modelLen
	default:
		return s.pattern < o.pattern
	}
}

// NewTable compiles entries into a table, rejecting rows that cannot mean
// anything useful.
func NewTable(entries []Entry) (*Table, error) {
	t := &Table{rows: make([]row, 0, len(entries))}
	for _, e := range entries {
		switch e.Prov {
		case ProvAuthoritative:
			return nil, fmt.Errorf("pricing: entry %q/%q claims authoritative provenance, which is a settled per-request figure and not a table rate", e.Host, e.Model)
		case ProvNone:
			return nil, fmt.Errorf("pricing: entry %q/%q has no provenance", e.Host, e.Model)
		}
		if !e.Rates.any() {
			return nil, fmt.Errorf("pricing: entry %q/%q sets no rate for any tier", e.Host, e.Model)
		}
		m, err := compileModel(e.Model)
		if err != nil {
			return nil, err
		}
		host := strings.ToLower(e.Host)
		if !anyHost(host) {
			if err := validHostPattern(host); err != nil {
				return nil, fmt.Errorf("pricing: host pattern %q: %w", e.Host, err)
			}
		}
		t.rows = append(t.rows, row{
			host:  host,
			model: m,
			rates: e.Rates,
			prov:  e.Prov,
			spec: specificity{
				namedHost: !anyHost(host),
				exactHost: !anyHost(host) && !strings.ContainsAny(host, globMeta),
				// Zero for a catch-all, so "*" and "" rank identically — the docs
				// promise they mean the same thing, but len("*") is 1 and len("") is
				// 0, and hostLen is compared before the model axis, so a
				// {"*", "*"} row used to beat a {"", "claude-opus-5"} row: a
				// catch-all shadowing an exact model.
				hostLen:    hostRankLen(host),
				exactModel: m.exact,
				modelLen:   len(m.pattern),
				pattern:    host + "\x00" + m.pattern,
			},
		})
	}
	return t, nil
}

// Resolve returns the rates for one (endpoint, model) pair and where they came
// from, already flattened for a prompt of promptTotal tokens.
//
// Provenance decides first, specificity only within a level — see the
// specificity type for why the endpoint axis outranks the model axis. A nil Table
// resolves to ProvNone rather than panicking: a binary built without pricing
// wiring must report traffic as unpriced, not crash on the response path.
func (t *Table) Resolve(endpoint, model string, promptTotal int) (Rates, Provenance) {
	if t == nil {
		return Rates{}, ProvNone
	}
	var best *row
	for i := range t.rows {
		r := &t.rows[i]
		if !matchHost(r.host, endpoint) || !r.model.match(model) {
			continue
		}
		if best == nil || r.prov > best.prov || (r.prov == best.prov && r.spec.beats(best.spec)) {
			best = r
		}
	}
	if best == nil {
		return Rates{}, ProvNone
	}
	return best.rates.At(promptTotal), best.prov
}

var _ Resolver = (*Table)(nil)
