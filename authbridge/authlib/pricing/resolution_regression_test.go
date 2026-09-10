package pricing

import "testing"

// The bundled table's whole justification is per-version accuracy, and these assert
// the RATE, not merely that something matched. The original bundled test fed
// provider-prefixed names and checked only provenance, so it passed while returning
// the wrong price — the failure mode this file exists to prevent.
func TestResolution_ProviderPrefixedNamesKeepTheirVersionRate(t *testing.T) {
	tab, err := NewTable(Bundled())
	if err != nil {
		t.Fatal(err)
	}
	rate := func(model string) float64 {
		r, p := tab.Resolve("api.anthropic.com", model, 0)
		if p == ProvNone {
			t.Fatalf("%s resolved unpriced", model)
		}
		v, ok := r.For(TierInput)
		if !ok {
			t.Fatalf("%s has no input rate", model)
		}
		return v * tokensPerMillion
	}

	// A gateway echoing "anthropic/claude-opus-4-1" must not be charged opus-5's
	// rate. This was 5.00 instead of 15.00 — the exact 3x error the generated table
	// replaced the family globs to fix.
	bare := rate("claude-opus-4-1")
	for _, prefixed := range []string{
		"anthropic/claude-opus-4-1",
		"aws/claude-opus-4-1",
		"bedrock/claude-opus-4-1",
	} {
		if got := rate(prefixed); got != bare {
			t.Errorf("%s = %.2f/Mtok, want %.2f (its own version's rate, not a family fallback)", prefixed, got, bare)
		}
	}

	// Version-FIRST names, which the old family glob could not match at all.
	if got := rate("bedrock/claude-3-opus-20240229"); got != rate("claude-3-opus-20240229") {
		t.Errorf("prefixed version-first name = %.2f/Mtok, want %.2f", got, rate("claude-3-opus-20240229"))
	}
}

func TestResolution_ThresholdSurvivesAProviderPrefix(t *testing.T) {
	tab, err := NewTable(Bundled())
	if err != nil {
		t.Fatal(err)
	}
	at := func(model string, prompt int) float64 {
		r, _ := tab.Resolve("api.anthropic.com", model, prompt)
		v, _ := r.For(TierInput)
		return v * tokensPerMillion
	}
	// claude-sonnet-4-5 doubles its input rate above 200k upstream. A prefixed name
	// used to fall to the family glob, which has no threshold, so a long-context
	// request was priced at the base rate.
	bare, prefixed := at("claude-sonnet-4-5", 300_000), at("anthropic/claude-sonnet-4-5", 300_000)
	if prefixed != bare {
		t.Errorf("prefixed @300k = %.2f/Mtok, want %.2f (threshold lost through the prefix)", prefixed, bare)
	}
	if base := at("claude-sonnet-4-5", 100_000); bare <= base {
		t.Fatalf("fixture is not exercising a threshold: @300k %.2f vs @100k %.2f", bare, base)
	}
}

func TestResolution_ExactHostBeatsAGlobThatCoversIt(t *testing.T) {
	mk := func(host string, perM float64) Entry {
		var r Rates
		r.Base[TierInput], r.Set[TierInput] = perM/tokensPerMillion, true
		return Entry{Host: host, Model: "*", Rates: r, Prov: ProvConfigured}
	}
	// Both orders: the bug reproduced either way, because it came from the
	// lexicographic tie-break ("*" sorts before any letter), not from slice order.
	for name, entries := range map[string][]Entry{
		"exact first": {mk("a.internal", 1.00), mk("*.internal", 99.00)},
		"glob first":  {mk("*.internal", 99.00), mk("a.internal", 1.00)},
	} {
		t.Run(name, func(t *testing.T) {
			tab, err := NewTable(entries)
			if err != nil {
				t.Fatal(err)
			}
			r, _ := tab.Resolve("a.internal", "m", 0)
			v, _ := r.For(TierInput)
			if got := v * tokensPerMillion; got != 1.00 {
				t.Errorf("rate = %.2f/Mtok, want 1.00 — an operator pinning one gateway beside a broader glob got the broad rate", got)
			}
			// And the glob still covers everything it should.
			r, _ = tab.Resolve("b.internal", "m", 0)
			v, _ = r.For(TierInput)
			if got := v * tokensPerMillion; got != 99.00 {
				t.Errorf("unpinned host rate = %.2f/Mtok, want 99.00", got)
			}
		})
	}
}

func TestResolution_CatchAllHostSpellingsRankEqually(t *testing.T) {
	// plugin-catalog.md documents "" and "*" as identical. They must therefore lose
	// to a more specific MODEL the same way, which len("*")==1 vs len("")==0 broke.
	var catchAll, exact Rates
	catchAll.Base[TierInput], catchAll.Set[TierInput] = 42.0/tokensPerMillion, true
	exact.Base[TierInput], exact.Set[TierInput] = 7.0/tokensPerMillion, true

	for _, star := range []string{"*", ""} {
		tab, err := NewTable([]Entry{
			{Host: star, Model: "*", Rates: catchAll, Prov: ProvConfigured},
			{Host: "", Model: "claude-opus-5", Rates: exact, Prov: ProvConfigured},
		})
		if err != nil {
			t.Fatal(err)
		}
		r, _ := tab.Resolve("h", "claude-opus-5", 0)
		v, _ := r.For(TierInput)
		if got := v * tokensPerMillion; got != 7.00 {
			t.Errorf("host %q: rate = %.2f/Mtok, want 7.00 — a catch-all shadowed an exact model", star, got)
		}
	}
}
