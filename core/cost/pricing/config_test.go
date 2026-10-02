package pricing

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func mustYAML(t *testing.T, src string) *Config {
	t.Helper()
	var c Config
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	return &c
}

func TestBuild_NilConfigIsBundledOnly(t *testing.T) {
	// An operator who writes no pricing: section still gets the shipped table —
	// the explicit decision that internal usage works with no manual setup.
	tab, err := Build(nil)
	if err != nil {
		t.Fatalf("Build(nil): %v", err)
	}
	if _, p := tab.Resolve("api.anthropic.com", "claude-opus-5", 0); p != ProvBundled {
		t.Errorf("provenance = %s, want bundled", p)
	}
}

func TestBuild_BundledCanBeDisabled(t *testing.T) {
	tab, err := Build(mustYAML(t, "bundled: false\n"))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, p := tab.Resolve("api.anthropic.com", "claude-opus-5", 0); p != ProvNone {
		t.Errorf("provenance = %s, want none with bundled disabled", p)
	}
}

func TestBuild_ConfiguredOutranksBundled(t *testing.T) {
	tab, err := Build(mustYAML(t, `
endpoints:
  - hosts: [gw.internal]
    models:
      "*claude-opus-*":
        input_cost_per_million: 3.80
        cache_write_cost_per_million: 4.75
        cache_read_cost_per_million: 0.38
        output_cost_per_million: 19.00
`))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	r, p := tab.Resolve("gw.internal:4000", "claude-opus-5", 0)
	if p != ProvConfigured {
		t.Errorf("provenance = %s, want configured", p)
	}
	if got := inputPerMillion(r); got != 3.80 {
		t.Errorf("input rate = %v, want 3.80 (the gateway's, not vendor list)", got)
	}
	// Traffic to the vendor endpoint still gets vendor list from bundled.
	if _, p := tab.Resolve("api.anthropic.com", "claude-opus-5", 0); p != ProvBundled {
		t.Errorf("other endpoint provenance = %s, want bundled", p)
	}
}

func TestBuild_TwoGatewaysPricedDifferently(t *testing.T) {
	// The requirement the endpoint dimension exists for: one laptop, several
	// endpoints, each with its own pricing.
	tab, err := Build(mustYAML(t, `
endpoints:
  - hosts: [gw-a.internal]
    models:
      "*": {input_cost_per_million: 3.80}
  - hosts: [gw-b.internal]
    models:
      "*": {input_cost_per_million: 7.60}
`))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for host, want := range map[string]float64{"gw-a.internal": 3.80, "gw-b.internal": 7.60} {
		if got := inputPerMillion(first(tab.Resolve(host, "some-model", 0))); got != want {
			t.Errorf("%s input rate = %v, want %v", host, got, want)
		}
	}
}

func TestBuild_PerTokenUnitAccepted(t *testing.T) {
	// LiteLLM's own map is per-token, so rates get copied straight out of it.
	tab, err := Build(mustYAML(t, `
bundled: false
endpoints:
  - hosts: ["*"]
    models:
      "*": {input_cost_per_token: 0.0000038}
`))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := inputPerMillion(first(tab.Resolve("h", "m", 0))); got != 3.80 {
		t.Errorf("input rate = %v, want 3.80", got)
	}
}

func TestBuild_BothUnitsForOneTierIsAnError(t *testing.T) {
	// Rejected rather than resolved by precedence: the units differ by 10^6, so
	// silently picking a winner either overstates a figure a millionfold or buries
	// it below rounding, and the readout gives no way to tell which was honoured.
	_, err := Build(mustYAML(t, `
endpoints:
  - hosts: ["*"]
    models:
      "claude-opus-5":
        input_cost_per_million: 5.00
        input_cost_per_token: 0.000005
`))
	if err == nil {
		t.Fatal("Build accepted both units for one tier")
	}
	for _, want := range []string{"input", "claude-opus-5", "set one"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestBuild_RejectsNegativeAndNonFiniteRates(t *testing.T) {
	for name, src := range map[string]string{
		"negative": `
endpoints:
  - hosts: ["*"]
    models:
      "m": {input_cost_per_million: -1}
`,
		"nan": `
endpoints:
  - hosts: ["*"]
    models:
      "m": {input_cost_per_million: .nan}
`,
		"inf": `
endpoints:
  - hosts: ["*"]
    models:
      "m": {input_cost_per_million: .inf}
`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Build(mustYAML(t, src)); err == nil {
				t.Fatalf("Build accepted a %s rate", name)
			}
		})
	}
}

func TestBuild_ContextThresholds(t *testing.T) {
	tab, err := Build(mustYAML(t, `
bundled: false
endpoints:
  - hosts: ["*"]
    models:
      "*":
        input_cost_per_million: 3.00
        above:
          - prompt_tokens: 200000
            input_cost_per_million: 6.00
`))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := inputPerMillion(first(tab.Resolve("h", "m", 100_000))); got != 3.00 {
		t.Errorf("below threshold = %v, want 3.00", got)
	}
	if got := inputPerMillion(first(tab.Resolve("h", "m", 300_000))); got != 6.00 {
		t.Errorf("above threshold = %v, want 6.00", got)
	}
}

func TestBuild_ThresholdNeedsAPositivePromptTokens(t *testing.T) {
	_, err := Build(mustYAML(t, `
endpoints:
  - hosts: ["*"]
    models:
      "m":
        input_cost_per_million: 3.00
        above:
          - input_cost_per_million: 6.00
`))
	if err == nil {
		t.Fatal("Build accepted a threshold with no prompt_tokens")
	}
	if !strings.Contains(err.Error(), "prompt_tokens") {
		t.Errorf("error %q does not mention prompt_tokens", err)
	}
}

func TestBuild_RejectsAModelThatPricesNothing(t *testing.T) {
	// An empty model block matches traffic and then resolves it as unpriced, which
	// is indistinguishable from having no entry and hides the typo.
	_, err := Build(mustYAML(t, `
endpoints:
  - hosts: ["*"]
    models:
      "claude-opus-5": {}
`))
	if err == nil {
		t.Fatal("Build accepted a model with no rates")
	}
	if !strings.Contains(err.Error(), "claude-opus-5") {
		t.Errorf("error %q does not name the offending model", err)
	}
}

func TestBuild_RejectsAnEndpointWithNoModels(t *testing.T) {
	_, err := Build(mustYAML(t, `
endpoints:
  - hosts: [gw.internal]
`))
	if err == nil {
		t.Fatal("Build accepted an endpoint with no models")
	}
	if !strings.Contains(err.Error(), "gw.internal") {
		t.Errorf("error %q does not name the endpoint", err)
	}
}

func TestBuild_ErrorNamesTheEndpointAndModel(t *testing.T) {
	// An operator editing YAML needs to be told which row is wrong.
	_, err := Build(mustYAML(t, `
endpoints:
  - hosts: [gw.internal]
    models:
      "claude-[": {input_cost_per_million: 1}
`))
	if err == nil {
		t.Fatal("Build accepted a malformed model glob")
	}
	if !strings.Contains(err.Error(), "claude-[") {
		t.Errorf("error %q does not name the offending pattern", err)
	}
}

// The 1.32x overstatement on a discounted gateway is silent: an overstated figure looks
// exactly like an accurate one. A boot warning is the only thing that makes it visible.
func TestWarnIfUnpinned(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *Config
		warn bool
	}{
		{"no pricing section at all", nil, true},
		{"bundled on, nothing pinned", &Config{}, true},
		{"an endpoint pinned", &Config{Endpoints: []EndpointConfig{{Hosts: []string{"gw"}}}}, false},
		{"bundled disabled", &Config{Bundled: boolPtr(false)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf strings.Builder
			log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
			tc.cfg.WarnIfUnpinned(log)
			got := buf.String()
			if tc.warn && got == "" {
				t.Error("no warning for a deployment pricing everything from vendor list")
			}
			if !tc.warn && got != "" {
				t.Errorf("warned unnecessarily: %s", got)
			}
			if tc.warn {
				// The direction of the error and the remedy both have to be in it, or an
				// operator cannot act on it. Each surface is pinned together with the
				// CAPABILITY claimed for it: a bare "agentop observe" also passes with the
				// two halves swapped, and swapped the hint is false in both halves.
				for _, want := range []string{
					"VENDOR LIST", "OVERSTATED", "pricing.endpoints",
					"agentop pricing --host <gateway> shows the rates",
					"agentop observe annotates the cost total",
				} {
					if !strings.Contains(got, want) {
						t.Errorf("warning omits %q: %s", want, got)
					}
				}
				// Naming the right surfaces does not exclude the wrong one. The bug this
				// pins was the hint sending an operator to `agentop cost`, which prints no
				// provenance annotation (provenanceNote lives only in cmd/agentop/tui), and
				// re-adding it leaves every pin above satisfied — so the ban is the only
				// half that can fail for the original defect, and the pins above are the
				// only half that can fail for its deletion.
				if strings.Contains(got, "agentop cost") {
					t.Errorf("warning sends the operator to agentop cost, which carries no provenance: %s", got)
				}
			}
		})
	}
}

// unit: names the currency an endpoint's rates are denominated in.
//
// ABSENT MEANS USD, which is what every existing config says and what every row already on disk
// means. That default is not a convenience: it is the same rule the ledger's own schema forces,
// where a field added today decodes as its zero value for the whole retained history.
func TestConfig_UnitDefaultsToUSDAndIsCarriedOnTheEntry(t *testing.T) {
	for _, tc := range []struct {
		name string
		unit string
		want string
	}{
		{"absent means USD", "", CurrencyUSD},
		{"explicit USD stays USD", "USD", CurrencyUSD},
		{"a gateway that bills in credits", "credits", "credits"},
		// Compared case-insensitively but CASE-PRESERVED, so an operator's spelling is what
		// they see back in `agentop pricing` rather than a normalised one they never typed.
		{"case is preserved", "Bobcoins", "Bobcoins"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Endpoints: []EndpointConfig{{
				Hosts: []string{"gw.example"},
				Unit:  tc.unit,
				Models: map[string]ModelConfig{
					"m": {TierRates: TierRates{InputCostPerMillion: 2}},
				},
			}}}
			entries, err := cfg.entries()
			if err != nil {
				t.Fatalf("entries: %v", err)
			}
			if len(entries) != 1 {
				t.Fatalf("got %d entries, want 1", len(entries))
			}
			if entries[0].Currency != tc.want {
				t.Errorf("Currency = %q, want %q", entries[0].Currency, tc.want)
			}
		})
	}
}

// A unit that is not an identifier is refused at startup, naming the endpoint.
//
// These strings reach a durable ledger row, a terminal and a JSON document, so the same
// reasoning that caps and sanitises a model name applies: refuse at load, where an operator is
// looking at the error, rather than write something unreadable into a file retained for a month.
// maxUnitLen is 16, and four things outside this package restate that number or derive from it.
//
// A LITERAL, DELIBERATELY. The boundary cases below derive their fixtures from maxUnitLen, which
// makes them test the BEHAVIOUR at the bound and leaves them green when the bound moves — verified:
// 16 -> 24 passes every package. So they cannot be the pin, and a test that derives its fixture
// from the constant it means to hold never can be.
//
// What the number is load-bearing FOR, neither of which the compiler connects:
//   - docs/pricing.md tells operators a unit is "at most 16 bytes";
//   - cmd/agentop's TestRunCost_TheWidestLegalUnitIsNeverTruncated uses a 16-byte fixture as its
//     worst case, and cannot import an unexported constant to check it;
//   - cmd_cost.go's headline-geometry comment writes "up to maxUnitLen (16)";
//   - and DERIVES "~25 columns" from it, so that figure moves too.
//
// The last two are the ones the first draft of this comment missed, and one of them sits three
// lines above where that draft wrote the count.
//
// Changing the bound is fine; changing it silently is not. This is the tripwire that makes it a
// decision, and its failure message is the checklist.
func TestConfig_MaxUnitLenIsSixteen(t *testing.T) {
	if maxUnitLen != 16 {
		t.Errorf("maxUnitLen = %d, not 16. That is allowed, but five things have to move with it: "+
			"docs/pricing.md's \"at most 16 bytes\", cmd/agentop's widest-unit fixture, cmd_cost.go's "+
			"\"up to maxUnitLen (16)\" and the \"~25 columns\" it derives, and this test.",
			maxUnitLen)
	}
}

// A unit of exactly maxUnitLen bytes is ACCEPTED.
//
// The other half of the bound's BEHAVIOUR, and the half that keeps the refusal below from being
// satisfiable by a validator that refuses everything. Derived from maxUnitLen on purpose: this pair
// asserts the boundary is where the constant says, whatever the constant says.
func TestConfig_AUnitOfExactlyTheBoundIsAccepted(t *testing.T) {
	unit := strings.Repeat("c", maxUnitLen)
	got, err := normaliseUnit(unit, "pricing.endpoints[0]")
	if err != nil {
		t.Fatalf("a unit of exactly maxUnitLen (%d) was refused: %v", maxUnitLen, err)
	}
	if got != unit {
		t.Errorf("normaliseUnit(%q) = %q; a non-default unit keeps the operator's spelling", unit, got)
	}
}

func TestConfig_RejectsAnUnusableUnit(t *testing.T) {
	for _, tc := range []struct{ name, unit string }{
		{"whitespace only", "   "},
		{"embedded space", "bob coins"},
		{"control character", "cre\x1bdits"},
		{"absurdly long", strings.Repeat("c", 40)},
		// THE BOUNDARY, not just a value far past it. "absurdly long" above is refused by any
		// bound at all, so it pins nothing: raising maxUnitLen from 16 to 24 left it green, and
		// left a cmd/agentop fixture that restates 16 green too — that fixture cannot import an
		// unexported constant, so this is the only place the number can be held. One over the
		// bound must be refused; exactly the bound is accepted by the sibling test below.
		{"one byte past maxUnitLen", strings.Repeat("c", maxUnitLen+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Endpoints: []EndpointConfig{{
				Hosts: []string{"gw.example"},
				Unit:  tc.unit,
				Models: map[string]ModelConfig{
					"m": {TierRates: TierRates{InputCostPerMillion: 2}},
				},
			}}}
			_, err := cfg.entries()
			if err == nil {
				t.Fatalf("unit %q was accepted; it must be refused at load", tc.unit)
			}
			// The endpoint has to be named: a deployment may configure several, and "bad unit"
			// with no location is a message an operator cannot act on.
			if !strings.Contains(err.Error(), "gw.example") {
				t.Errorf("error does not name the endpoint: %v", err)
			}
		})
	}
}

// Two endpoints may use different units; one endpoint may not use two.
//
// The unit sits on the ENDPOINT because that is the level a gateway's billing is decided at, and
// this is what makes "never sum across units" expressible at all: a rate resolved for a request
// carries the unit of the endpoint it was resolved on, so the arithmetic never has to guess.
func TestConfig_UnitIsPerEndpointNotGlobal(t *testing.T) {
	cfg := &Config{Endpoints: []EndpointConfig{
		{
			Hosts:  []string{"api.anthropic.com"},
			Models: map[string]ModelConfig{"claude": {TierRates: TierRates{InputCostPerMillion: 3}}},
		},
		{
			Hosts:  []string{"api.us-east.bob.ibm.com"},
			Unit:   "credits",
			Models: map[string]ModelConfig{"premium-ide": {TierRates: TierRates{InputCostPerMillion: 2}}},
		},
	}}

	entries, err := cfg.entries()
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	got := map[string]string{}
	for _, e := range entries {
		got[e.Host] = e.Currency
	}
	if got["api.anthropic.com"] != CurrencyUSD {
		t.Errorf("anthropic entry = %q, want %q", got["api.anthropic.com"], CurrencyUSD)
	}
	if got["api.us-east.bob.ibm.com"] != "credits" {
		t.Errorf("bob entry = %q, want credits", got["api.us-east.bob.ibm.com"])
	}
}

// CurrencyFor answers with the unit of the endpoint, which is also the unit of the row that priced.
//
// Two blocks can match one host — a `hosts: ["*"]` catch-all beside a specific gateway — and the
// more specific block decides the gateway's unit. The catch-all keeps its own unit for every other
// endpoint, so one credits gateway does not recolour the deployment.
func TestTable_CurrencyForFollowsTheRowThatPriced(t *testing.T) {
	tbl, err := Build(&Config{
		Bundled: boolPtr(false),
		Endpoints: []EndpointConfig{
			{
				// A catch-all in dollars.
				Models: map[string]ModelConfig{"*": {TierRates: TierRates{InputCostPerMillion: 3}}},
			},
			{
				// A more specific gateway billing in credits.
				Hosts: []string{"api.us-east.bob.ibm.com"},
				Unit:  "credits",
				Models: map[string]ModelConfig{
					"premium-ide": {TierRates: TierRates{InputCostPerMillion: 2}},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if got := tbl.CurrencyFor("api.us-east.bob.ibm.com", "premium-ide"); got != "credits" {
		t.Errorf("Bob's endpoint resolved %q, want credits", got)
	}
	// The catch-all keeps USD, so one credits endpoint does not recolour the deployment.
	if got := tbl.CurrencyFor("api.anthropic.com", "claude-opus-5"); got != CurrencyUSD {
		t.Errorf("the catch-all resolved %q, want %s", got, CurrencyUSD)
	}
}

// An unpriced pair, and a nil table, answer USD rather than empty.
//
// USD IS THE ONLY SAFE ANSWER FOR "I DON'T KNOW". The caller is about to label a figure, and an
// empty unit would travel to a durable row where empty already means USD — so returning "" would
// be the same answer written less legibly. A nil Table is the Kubernetes deployment, where
// pricing is not wired at all: it must report the default rather than panic on the response path,
// the same reason Resolve answers ProvNone there.
func TestTable_CurrencyForDefaultsToUSD(t *testing.T) {
	var nilTable *Table
	if got := nilTable.CurrencyFor("anywhere", "anything"); got != CurrencyUSD {
		t.Errorf("nil table resolved %q, want %s", got, CurrencyUSD)
	}
	tbl, err := Build(&Config{Bundled: boolPtr(false), Endpoints: []EndpointConfig{{
		Hosts:  []string{"known.example"},
		Models: map[string]ModelConfig{"m": {TierRates: TierRates{InputCostPerMillion: 1}}},
	}}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := tbl.CurrencyFor("unknown.example", "whatever"); got != CurrencyUSD {
		t.Errorf("an unmatched endpoint resolved %q, want %s", got, CurrencyUSD)
	}
}

// boolPtr names the inline `func() *bool {...}()` this file would otherwise repeat at each use.
func boolPtr(b bool) *bool { return &b }

// The unit reaches the wire from the PRODUCER side, under the field name a client decodes.
//
// cmd/agentop's pricing tests feed hand-written JSON, so they pin the CLIENT's struct tag and say
// nothing about what this package emits — renaming json:"unit" on either side alone went
// undetected, and dropping the field from either producer left every test green. This asserts the
// bytes Describe() and EffectiveFor() actually serialise, which is the only place the two sides
// meet.
//
// BOTH PRODUCERS, because they are separate call sites with separate struct tags: Describe() is
// the raw table (`agentop pricing`) and EffectiveFor() is the resolved one (`agentop pricing --host`).
// Only the second was reachable from any existing assertion.
func TestDescribe_UnitIsSerialisedForBothProducers(t *testing.T) {
	cfg := &Config{Endpoints: []EndpointConfig{
		{
			Hosts:  []string{"api.anthropic.com"},
			Models: map[string]ModelConfig{"claude": {TierRates: TierRates{InputCostPerMillion: 3}}},
		},
		{
			Hosts: []string{"gw.bob"},
			Unit:  "credits",
			Models: map[string]ModelConfig{
				"premium-ide": {TierRates: TierRates{InputCostPerMillion: 2}},
			},
		},
	}}
	tbl, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Describe(): the raw table. The credits row must name its unit and the USD row must not.
	raw, err := json.Marshal(tbl.Describe())
	if err != nil {
		t.Fatalf("marshal Describe: %v", err)
	}
	if !strings.Contains(string(raw), `"unit":"credits"`) {
		t.Errorf(`Describe() emitted no "unit":"credits"; a client cannot label the figure:\n%s`, raw)
	}
	if strings.Contains(string(raw), `"unit":"USD"`) {
		t.Errorf(`Describe() spelled the default out; every existing document changes:\n%s`, raw)
	}

	// EffectiveFor(): the resolved view, per host, and the one agentop --host renders.
	eff, err := json.Marshal(tbl.EffectiveFor("gw.bob"))
	if err != nil {
		t.Fatalf("marshal EffectiveFor: %v", err)
	}
	if !strings.Contains(string(eff), `"unit":"credits"`) {
		t.Errorf(`EffectiveFor("gw.bob") emitted no "unit":"credits":\n%s`, eff)
	}
	usd, err := json.Marshal(tbl.EffectiveFor("api.anthropic.com"))
	if err != nil {
		t.Fatalf("marshal EffectiveFor: %v", err)
	}
	if strings.Contains(string(usd), `"unit"`) {
		t.Errorf(`EffectiveFor("api.anthropic.com") named a unit for a dollars endpoint:\n%s`, usd)
	}
}

// Every spelling of the default resolves to CurrencyUSD, so no consumer's `== CurrencyUSD` drifts.
//
// The charset check accepts "usd" by design — it is letters — and five consumers test the result
// against CurrencyUSD to decide whether a figure may be labelled "$". Four compared
// case-sensitively, so `unit: usd` read as a NON-default unit: `agentop pricing` printed "per Mtok"
// over a table of dollars and the ledger wrote a currency field on every row of a deployment that
// had only ever billed dollars. Canonicalised at this one entrance instead of at each comparison.
//
// NON-DEFAULT UNITS ARE STILL CASE-PRESERVED, which is the other half of the property and the
// direction that would break an operator's own spelling: only USD has a canonical form here.
func TestConfig_EverySpellingOfTheDefaultCanonicalises(t *testing.T) {
	for _, tc := range []struct{ unit, want string }{
		{"USD", CurrencyUSD},
		{"usd", CurrencyUSD},
		{"Usd", CurrencyUSD},
		{"uSd", CurrencyUSD},
		// Preserved, because this package has no canonical spelling to offer for it.
		{"credits", "credits"},
		{"Bobcoins", "Bobcoins"},
		{"CREDITS", "CREDITS"},
	} {
		t.Run(tc.unit, func(t *testing.T) {
			got, err := normaliseUnit(tc.unit, "pricing.endpoints[0]")
			if err != nil {
				t.Fatalf("normaliseUnit(%q): %v", tc.unit, err)
			}
			if got != tc.want {
				t.Errorf("normaliseUnit(%q) = %q, want %q", tc.unit, got, tc.want)
			}
		})
	}
}

// currencyOrDefault folds too, for a unit that never passed through a config file.
//
// Endpoint is EXPORTED and core is consumed outside this repo, so a caller can build one with
// Currency "usd" having read no YAML. normaliseUnit cannot see that path; this funnel can, and it
// is the one every consumer's value arrives through.
func TestCurrencyFor_FoldsADefaultSpellingSetProgrammatically(t *testing.T) {
	tbl, err := NewTable([]Entry{{
		Host: "gw", Model: "*", Currency: "usd", Prov: ProvConfigured,
		Rates: Rates{
			Base: [numTiers]float64{TierInput: 2e-6},
			Set:  [numTiers]bool{TierInput: true},
		},
	}})
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	if got := tbl.CurrencyFor("gw", "m"); got != CurrencyUSD {
		t.Errorf("CurrencyFor = %q, want %s: a programmatic \"usd\" must not read as a foreign unit",
			got, CurrencyUSD)
	}
}

// A response whose model could not be read still resolves its gateway's unit.
//
// bestRow needs the host AND the model to match, so it answers nil for a model-less response and
// CurrencyFor used to return USD. The only figure such a response can carry is one the gateway
// reported, which settle publishes without consulting this table, and which is denominated in the
// gateway's unit — so USD was a mislabel on the one path that has no rate to check it against.
//
// BOTH DIRECTIONS. The model-less answer must match the pair-matched one, which is the invariant
// CurrencyFor's own comment rests on, so the specific and the catch-all row are both asserted.
//
// The "*" block comes FIRST and covers gw.bob too, so only specificity picks gw.bob's own row.
func TestCurrencyFor_AModellessResponseResolvesTheEndpointsUnit(t *testing.T) {
	tbl, err := Build(&Config{Endpoints: []EndpointConfig{
		{
			Hosts:  []string{"*"},
			Models: map[string]ModelConfig{"claude-opus-5": {TierRates: TierRates{InputCostPerMillion: 5}}},
		},
		{
			Hosts: []string{"gw.bob"}, Unit: "credits",
			Models: map[string]ModelConfig{"premium-ide": {TierRates: TierRates{InputCostPerMillion: 2}}},
		},
		{
			Hosts:  []string{"api.anthropic.com"},
			Models: map[string]ModelConfig{"claude-opus-5": {TierRates: TierRates{InputCostPerMillion: 5}}},
		},
	}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if got := tbl.CurrencyFor("gw.bob", ""); got != "credits" {
		t.Errorf("CurrencyFor(gw.bob, \"\") = %q, want credits; a gateway-reported charge with no "+
			"model would be written with no unit and folded into the dollar total", got)
	}
	// The pair-matched answer is unchanged, which is what keeps a figure from being priced at one
	// row's rate and labelled with another's.
	if got := tbl.CurrencyFor("gw.bob", "premium-ide"); got != "credits" {
		t.Errorf("CurrencyFor(gw.bob, premium-ide) = %q, want credits", got)
	}
	// And a dollars endpoint is untouched in both forms — the direction that would break every
	// deployment configured today.
	for _, model := range []string{"claude-opus-5", ""} {
		if got := tbl.CurrencyFor("api.anthropic.com", model); got != CurrencyUSD {
			t.Errorf("CurrencyFor(api.anthropic.com, %q) = %q, want %s", model, got, CurrencyUSD)
		}
	}
}

// A dollar catch-all does not price a gateway that bills in credits.
//
// `models: {"*"}` matches every model on every host, so it used to price gw.bob's traffic in
// dollars — and a charge the gateway reported itself, in credits, was then labelled with the
// catch-all's USD. Only rates in the endpoint's own unit may price it, so the catch-all is not a
// candidate there and keeps pricing every other endpoint.
func TestCurrencyFor_ADollarCatchAllDoesNotPriceACreditsGateway(t *testing.T) {
	tbl, err := Build(&Config{Endpoints: []EndpointConfig{
		{
			Hosts:  []string{"*"},
			Models: map[string]ModelConfig{"*": {TierRates: TierRates{InputCostPerMillion: 1}}},
		},
		{
			Hosts: []string{"gw.bob"}, Unit: "credits",
			Models: map[string]ModelConfig{"premium-ide": {TierRates: TierRates{InputCostPerMillion: 2}}},
		},
	}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, prov := tbl.Resolve("gw.bob", "", 0); prov != ProvNone {
		t.Errorf("gw.bob's model-less response resolved %v, want %v: the only row that matches is "+
			"the dollar catch-all", prov, ProvNone)
	}
	if got := tbl.CurrencyFor("gw.bob", ""); got != "credits" {
		t.Errorf("CurrencyFor(gw.bob, \"\") = %q, want credits", got)
	}
	// The catch-all still prices, in dollars, everywhere else.
	if _, prov := tbl.Resolve("api.anthropic.com", "claude-opus-5", 0); prov != ProvConfigured {
		t.Errorf("the catch-all no longer prices api.anthropic.com (prov %v)", prov)
	}
	if got := tbl.CurrencyFor("api.anthropic.com", "claude-opus-5"); got != CurrencyUSD {
		t.Errorf("CurrencyFor(api.anthropic.com) = %q, want %s", got, CurrencyUSD)
	}
}

// The gateway's unit covers models its block does not name, and bundled dollar rates do not
// price them there.
//
// The bundled rows match every host. Through a credits gateway they would price a model in dollars
// that the gateway bills in credits — a figure in neither — and label it USD.
func TestTable_AGatewaysUnitCoversModelsItsBlockDoesNotName(t *testing.T) {
	tbl, err := Build(&Config{Endpoints: []EndpointConfig{{
		Hosts: []string{"gw.bob"}, Unit: "credits",
		Models: map[string]ModelConfig{"premium-ide": {TierRates: TierRates{InputCostPerMillion: 2}}},
	}}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	const bundledModel = "claude-4-opus-20250514"
	if _, prov := tbl.Resolve("api.anthropic.com", bundledModel, 0); prov != ProvBundled {
		t.Fatalf("%s resolved %v on api.anthropic.com, want %v; the fixture pins nothing", bundledModel, prov, ProvBundled)
	}
	if _, prov := tbl.Resolve("gw.bob", bundledModel, 0); prov != ProvNone {
		t.Errorf("gw.bob/%s resolved %v, want %v: a dollar row priced a credits gateway", bundledModel, prov, ProvNone)
	}
	// The gateway's own rows still price it.
	if _, prov := tbl.Resolve("gw.bob", "premium-ide", 0); prov != ProvConfigured {
		t.Errorf("gw.bob/premium-ide resolved %v, want %v", prov, ProvConfigured)
	}
	if got := tbl.CurrencyFor("gw.bob", bundledModel); got != "credits" {
		t.Errorf("CurrencyFor(gw.bob, %s) = %q, want credits", bundledModel, got)
	}
	if got := tbl.CurrencyFor("api.anthropic.com", bundledModel); got != CurrencyUSD {
		t.Errorf("CurrencyFor(api.anthropic.com, %s) = %q, want %s", bundledModel, got, CurrencyUSD)
	}
}

// No model name decides an endpoint's unit, even where two blocks in different units tie on host.
//
// *.bob.ibm.com and api.*.ibm.com are both globs of 13 bytes and both cover api.bob.ibm.com. The
// unit lookup used to fall through specificity.beats to the MODEL fields there, so the USD block's
// model name picked the unit: "gpt-4o" (shorter than "premium-ide") left the host in credits,
// "gpt-4o-mini-2024-07-18" (longer) moved it to USD and unpriced premium-ide. The tie now breaks on
// the host pattern alone, and "*" sorts first.
func TestCurrencyFor_NoModelNameDecidesAnEndpointsUnit(t *testing.T) {
	for _, usdModel := range []string{"gpt-4o", "gpt-4o-mini-2024-07-18"} {
		t.Run(usdModel, func(t *testing.T) {
			tbl, err := Build(&Config{Endpoints: []EndpointConfig{
				{
					Hosts: []string{"*.bob.ibm.com"}, Unit: "credits",
					Models: map[string]ModelConfig{"premium-ide": {TierRates: TierRates{InputCostPerMillion: 2}}},
				},
				{
					Hosts:  []string{"api.*.ibm.com"},
					Models: map[string]ModelConfig{usdModel: {TierRates: TierRates{InputCostPerMillion: 1}}},
				},
			}})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if got := tbl.CurrencyFor("api.bob.ibm.com", ""); got != "credits" {
				t.Errorf("CurrencyFor(api.bob.ibm.com) = %q, want credits", got)
			}
			if _, prov := tbl.Resolve("api.bob.ibm.com", "premium-ide", 0); prov != ProvConfigured {
				t.Errorf("api.bob.ibm.com/premium-ide resolved %v, want %v", prov, ProvConfigured)
			}
			if _, prov := tbl.Resolve("api.bob.ibm.com", usdModel, 0); prov != ProvNone {
				t.Errorf("api.bob.ibm.com/%s resolved %v, want %v: a dollar row priced a credits endpoint",
					usdModel, prov, ProvNone)
			}
			// The dollar block still prices the hosts only it covers.
			if got := tbl.CurrencyFor("api.us.ibm.com", ""); got != CurrencyUSD {
				t.Errorf("CurrencyFor(api.us.ibm.com) = %q, want %s", got, CurrencyUSD)
			}
			if _, prov := tbl.Resolve("api.us.ibm.com", usdModel, 0); prov != ProvConfigured {
				t.Errorf("api.us.ibm.com/%s resolved %v, want %v", usdModel, prov, ProvConfigured)
			}
		})
	}
}

// The most specific block covering a host decides its unit, and a block with no unit decides USD.
//
// NOT A STARTUP ERROR, deliberately: the two blocks name different host patterns, so neither
// claims the other's host, and pinning one gateway of a fleet at dollars is a thing an operator
// may mean. What it costs is that the glob's models are unpriced on that one host, which
// unpricedBy names.
func TestCurrencyFor_TheMostSpecificBlockDecidesAHostsUnit(t *testing.T) {
	tbl, err := Build(&Config{Endpoints: []EndpointConfig{
		{
			Hosts: []string{"*.bob.ibm.com"}, Unit: "credits",
			Models: map[string]ModelConfig{"premium-ide": {TierRates: TierRates{InputCostPerMillion: 2}}},
		},
		{
			Hosts:  []string{"api.us-east.bob.ibm.com"},
			Models: map[string]ModelConfig{"gpt-4o": {TierRates: TierRates{InputCostPerMillion: 1}}},
		},
	}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := tbl.CurrencyFor("api.us-east.bob.ibm.com", ""); got != CurrencyUSD {
		t.Errorf("CurrencyFor(api.us-east.bob.ibm.com) = %q, want %s", got, CurrencyUSD)
	}
	if _, prov := tbl.Resolve("api.us-east.bob.ibm.com", "premium-ide", 0); prov != ProvNone {
		t.Errorf("api.us-east.bob.ibm.com/premium-ide resolved %v, want %v", prov, ProvNone)
	}
	if got := tbl.CurrencyFor("api.eu.bob.ibm.com", ""); got != "credits" {
		t.Errorf("CurrencyFor(api.eu.bob.ibm.com) = %q, want credits", got)
	}
}

// A unit on a block with only a multiplier is refused: the block creates no rates to carry it.
func TestConfig_AUnitOnAMultiplierOnlyBlockIsRefused(t *testing.T) {
	f := 0.76
	_, err := Build(&Config{Endpoints: []EndpointConfig{{
		Hosts: []string{"gw.bob"}, Unit: "credits", Multiplier: &f,
	}}})
	if err == nil || !strings.Contains(err.Error(), "needs a models block") {
		t.Fatalf("Build = %v, want a refusal naming the models block", err)
	}
	// Any spelling of the default is not a unit claim, so it stays accepted.
	for _, unit := range []string{"", "usd"} {
		if _, err := Build(&Config{Endpoints: []EndpointConfig{{
			Hosts: []string{"gw.bob"}, Unit: unit, Multiplier: &f,
		}}}); err != nil {
			t.Errorf("unit %q on a multiplier-only block: %v, want accepted", unit, err)
		}
	}
}

// A unit on a block that covers every endpoint is refused: it would put every endpoint no more
// specific block names in that unit.
//
// WHY IT IS AN ERROR AND NOT A FOOTGUN. Only rows in an endpoint's unit may price its traffic, so
// a catch-all credits block leaves the bundled dollar table pricing nothing — loud, since every
// pair lands in unpricedBy — and labels every charge an endpoint reports itself as credits, which
// nothing reports. A unit is a gateway's, so its hosts can be named.
//
// A CATCH-ALL ANYWHERE IN THE LIST COUNTS: naming one gateway beside "*" still covers the rest.
func TestConfig_AUnitOnACatchAllBlockIsRefused(t *testing.T) {
	block := func(hosts []string, unit string) EndpointConfig {
		return EndpointConfig{Hosts: hosts, Unit: unit,
			Models: map[string]ModelConfig{"premium-ide": {TierRates: TierRates{InputCostPerMillion: 2}}}}
	}
	for _, tc := range []struct {
		name  string
		hosts []string
	}{
		{"no hosts", nil},
		{"star", []string{"*"}},
		{"star beside a named gateway", []string{"gw.bob", "*"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Build(&Config{Endpoints: []EndpointConfig{block(tc.hosts, "credits")}})
			if err == nil || !strings.Contains(err.Error(), "needs hosts") {
				t.Fatalf("Build = %v, want a refusal asking for hosts", err)
			}
			if !strings.Contains(err.Error(), "pricing.endpoints[0]") {
				t.Errorf("the refusal does not name the block: %v", err)
			}
		})
	}
	// Any spelling of the default is not a unit claim, and a named gateway is what a unit is for.
	for _, tc := range []struct {
		hosts []string
		unit  string
	}{
		{nil, ""},
		{nil, "usd"},
		{[]string{"*"}, "USD"},
		{[]string{"gw.bob"}, "credits"},
	} {
		if _, err := Build(&Config{Endpoints: []EndpointConfig{block(tc.hosts, tc.unit)}}); err != nil {
			t.Errorf("hosts %v unit %q: %v, want accepted", tc.hosts, tc.unit, err)
		}
	}
}

// Two blocks naming one host must agree on its unit, or its unit depends on which of their rows
// happens to rank first.
//
// NO CATCH-ALL CASE: a block for every endpoint may not carry a unit at all, so two spellings of
// it always agree. See TestConfig_AUnitOnACatchAllBlockIsRefused.
func TestConfig_BlocksForOneHostMustAgreeOnTheUnit(t *testing.T) {
	block := func(hosts []string, unit, model string) EndpointConfig {
		return EndpointConfig{Hosts: hosts, Unit: unit,
			Models: map[string]ModelConfig{model: {TierRates: TierRates{InputCostPerMillion: 1}}}}
	}
	for _, tc := range []struct {
		name   string
		blocks []EndpointConfig
		ok     bool
	}{
		{"one host, two units", []EndpointConfig{
			block([]string{"gw.bob"}, "credits", "a"), block([]string{"GW.bob"}, "", "b")}, false},
		{"one unit spelled two ways", []EndpointConfig{
			block([]string{"gw.bob"}, "credits", "a"), block([]string{"gw.bob"}, "Credits", "b")}, true},
		{"different hosts, different units", []EndpointConfig{
			block([]string{"gw.bob"}, "credits", "a"), block([]string{"api.anthropic.com"}, "", "b")}, true},
	} {
		tbl, err := Build(&Config{Endpoints: tc.blocks})
		if (err == nil) != tc.ok || (err != nil && !strings.Contains(err.Error(), "one gateway bills in one unit")) {
			t.Errorf("%s: Build = %v, want ok=%v", tc.name, err, tc.ok)
		}
		if err != nil {
			continue
		}
		// An accepted config prices every block's model on its own host, whichever spelling of the
		// unit that host's best row carries.
		for _, b := range tc.blocks {
			for m := range b.Models {
				if _, prov := tbl.Resolve(b.Hosts[0], m, 0); prov == ProvNone {
					t.Errorf("%s: %s/%s is unpriced", tc.name, b.Hosts[0], m)
				}
			}
		}
	}
}
