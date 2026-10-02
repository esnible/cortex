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
	u := Usage{Input: 1200, CacheRead: 300, Output: 80}
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
