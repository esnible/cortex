package pricing

import "testing"

// OpenCode Zen's free models cost nothing, and a call to one is a priced zero, not unpriced
// traffic: unpriced counts against coverage and asks the operator for a rate that does
// not exist. Config cannot say $0 — a zero rate there leaves the tier unset — so the rate
// has to ship.
func TestBuild_ZenFreeModelsArePricedAtZero(t *testing.T) {
	tab, err := Build(nil)
	if err != nil {
		t.Fatalf("Build(nil): %v", err)
	}
	u := Usage{Input: 1200, CacheWrite: 50, CacheRead: 300, Output: 80}
	for _, model := range []string{"longcat-2.5-preview-free", "MiMo-V2.6-Flash-Free", "big-pickle"} {
		r, p := tab.Resolve("opencode.ai:443", model, u.PromptTotal())
		if p != ProvBundled {
			t.Errorf("%s: provenance = %s, want bundled", model, p)
			continue
		}
		if micros, ok := Cost(r, u); !ok || micros != 0 {
			t.Errorf("%s: Cost = %d, %v; want a priced zero", model, micros, ok)
		}
	}
}

// Only Zen's free models are shipped at zero. A paid Zen model, and a "-free" model on any
// other endpoint, stay unpriced until someone states a rate.
func TestBuild_ZenFreeRatesAreScopedToZensFreeModels(t *testing.T) {
	tab, err := Build(nil)
	if err != nil {
		t.Fatalf("Build(nil): %v", err)
	}
	for _, tc := range []struct{ host, model string }{
		{"opencode.ai", "glm-5.1"},
		{"openrouter.ai", "qwen3-coder-free"},
		{"gw.internal", "big-pickle"},
	} {
		if _, p := tab.Resolve(tc.host, tc.model, 0); p != ProvNone {
			t.Errorf("%s on %s: provenance = %s, want none", tc.model, tc.host, p)
		}
	}
}

// The free rates ship with the bundled table and go when it is turned off, like the
// shipped gateway discounts.
func TestBuild_ZenFreeRatesGoWithTheBundledTable(t *testing.T) {
	tab, err := Build(mustYAML(t, "bundled: false\n"))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, p := tab.Resolve("opencode.ai", "big-pickle", 0); p != ProvNone {
		t.Errorf("provenance = %s, want none with bundled disabled", p)
	}
}

// free is what lets the views tell a priced zero from an unset tier, whose rate fields
// both arrive as 0.
func TestRates_FreeIsEverySetTierAtZero(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    Rates
		want bool
	}{
		{"nothing set", Rates{}, false},
		{"every tier set at zero", Rates{Set: [numTiers]bool{true, true, true, true}}, true},
		// Rendered "free" in all four columns, so an unset tier must not pass.
		{"one tier set at zero, the rest unset", Rates{Set: [numTiers]bool{TierInput: true}}, false},
		{"one set tier has a rate", Rates{
			Set:  [numTiers]bool{TierInput: true, TierOutput: true},
			Base: [numTiers]float64{TierOutput: 0.000015},
		}, false},
		// A long-context override is a rate for some prompts, so the row is not free.
		{"has a threshold", Rates{
			Set:        [numTiers]bool{TierInput: true},
			Thresholds: []ContextThreshold{{AbovePromptTokens: 200_000}},
		}, false},
	} {
		if got := tc.r.free(); got != tc.want {
			t.Errorf("%s: free() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A free row's rate fields are omitted like an unset tier's, so the raw table has to say
// it is free or `agentop pricing` renders it as a coverage gap.
func TestDescribe_ZenFreeRowsSayFree(t *testing.T) {
	d := mustBuild(t, nil).Describe()
	free := map[string]bool{}
	for _, r := range d.Rows {
		if r.Free {
			free[r.Host+" "+r.Model] = true
		}
		if r.Model == "claude-opus-5" && r.Free {
			t.Errorf("claude-opus-5 row is marked free: %+v", r)
		}
	}
	for _, want := range []string{"opencode.ai *-free", "opencode.ai big-pickle"} {
		if !free[want] {
			t.Errorf("no free row %q among %v", want, free)
		}
	}
	if len(free) != 2 {
		t.Errorf("free rows = %v, want only Zen's two", free)
	}
}

// The free rows are Zen's. On Zen they resolve priced at zero; on any other endpoint they
// are not listed, because listing them there as UNPRICED reads as a gap in that endpoint's
// rates. A configured row for another host is still listed, as before.
func TestEffectiveFor_ZenFreeRowsStayOnZen(t *testing.T) {
	tab := mustBuild(t, mustYAML(t, `
endpoints:
  - hosts: [gw.internal]
    models:
      "only-on-gw": {input_cost_per_million: 1.00}
`))
	models := func(host string) map[string]EffectiveRates {
		out := map[string]EffectiveRates{}
		for _, e := range tab.EffectiveFor(host).Models {
			out[e.Model] = e
		}
		return out
	}

	zen := models("opencode.ai:443")
	for _, m := range []string{"*-free", "big-pickle"} {
		e, ok := zen[m]
		if !ok {
			t.Errorf("opencode.ai: %s not listed", m)
			continue
		}
		if !e.Free || e.Unpriced || e.Provenance != "bundled" {
			t.Errorf("opencode.ai: %s = %+v, want free, priced, bundled", m, e)
		}
	}
	if e := zen["claude-opus-5"]; e.Free {
		t.Errorf("opencode.ai: claude-opus-5 = %+v, want not free", e)
	}

	anthropic := models("api.anthropic.com")
	for _, m := range []string{"*-free", "big-pickle"} {
		if e, ok := anthropic[m]; ok {
			t.Errorf("api.anthropic.com lists Zen's %s: %+v", m, e)
		}
	}
	if e, ok := anthropic["only-on-gw"]; !ok || !e.Unpriced {
		t.Errorf("api.anthropic.com: configured only-on-gw = %+v (listed %v), want listed and unpriced", e, ok)
	}
}

// A configured rate outranks a shipped one whatever its pattern, so an operator's
// catch-all for opencode.ai replaces the zero. The docs tell operators to name Zen's paid
// models for that reason; this pins the behavior they describe.
func TestBuild_ConfiguredCatchAllOutranksTheShippedZero(t *testing.T) {
	tab := mustBuild(t, mustYAML(t, `
endpoints:
  - hosts: [opencode.ai]
    models:
      "*": {input_cost_per_million: 1.00, output_cost_per_million: 2.00}
`))
	r, p := tab.Resolve("opencode.ai", "big-pickle", 0)
	if p != ProvConfigured {
		t.Fatalf("provenance = %s, want configured", p)
	}
	if micros, ok := Cost(r, Usage{Input: 1_000_000}); !ok || micros != 1_000_000 {
		t.Errorf("Cost = %d, %v; want the configured $1.00", micros, ok)
	}
}
