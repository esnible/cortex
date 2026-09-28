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
		{"bundled disabled", &Config{Bundled: func() *bool { b := false; return &b }()}, false},
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
				// CAPABILITY claimed for it: a bare "abctl observe" also passes with the
				// two halves swapped, and swapped the hint is false in both halves.
				for _, want := range []string{
					"VENDOR LIST", "OVERSTATED", "pricing.endpoints",
					"abctl pricing --host <gateway> shows the rates",
					"abctl observe annotates the cost total",
				} {
					if !strings.Contains(got, want) {
						t.Errorf("warning omits %q: %s", want, got)
					}
				}
				// Naming the right surfaces does not exclude the wrong one. The bug this
				// pins was the hint sending an operator to `abctl cost`, which prints no
				// provenance annotation (provenanceNote lives only in cmd/abctl/tui), and
				// re-adding it leaves every pin above satisfied — so the ban is the only
				// half that can fail for the original defect, and the pins above are the
				// only half that can fail for its deletion.
				if strings.Contains(got, "abctl cost") {
					t.Errorf("warning sends the operator to abctl cost, which carries no provenance: %s", got)
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
		// they see back in `abctl pricing` rather than a normalised one they never typed.
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
func TestConfig_RejectsAnUnusableUnit(t *testing.T) {
	for _, tc := range []struct{ name, unit string }{
		{"whitespace only", "   "},
		{"embedded space", "bob coins"},
		{"control character", "cre\x1bdits"},
		{"absurdly long", strings.Repeat("c", 40)},
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

// CurrencyFor answers with the unit of the SAME row Resolve priced from.
//
// Not "the unit of the endpoint block", which is subtly different and would be wrong: two blocks
// can match one host — a `hosts: ["*"]` catch-all beside a specific gateway — and the rate that
// wins is the more specific row's. Taking the unit from anywhere else lets a figure be priced at
// one row's rate and labelled with another's, which is the one failure this whole field exists
// to prevent.
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

// boolPtr is the inline `func() *bool {...}()` this file used twice, named once.
func boolPtr(b bool) *bool { return &b }

// The unit reaches the wire from the PRODUCER side, under the field name a client decodes.
//
// cmd/abctl's pricing tests feed hand-written JSON, so they pin the CLIENT's struct tag and say
// nothing about what this package emits — renaming json:"unit" on either side alone went
// undetected, and dropping the field from either producer left every test green. This asserts the
// bytes Describe() and EffectiveFor() actually serialise, which is the only place the two sides
// meet.
//
// BOTH PRODUCERS, because they are separate call sites with separate struct tags: Describe() is
// the raw table (`abctl pricing`) and EffectiveFor() is the resolved one (`abctl pricing --host`).
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

	// EffectiveFor(): the resolved view, per host, and the one abctl --host renders.
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
// case-sensitively, so `unit: usd` read as a NON-default unit: `abctl pricing` printed "per Mtok"
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
