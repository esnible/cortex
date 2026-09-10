package pricing

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The tests here assert that phase 7 — per-endpoint discovery via GET /model/info —
// can be added without reopening anything in this package. They are cheap, and they
// fail loudly if a later change quietly closes one of the seams the deferred work
// depends on.

// ProvDiscovered has to rank strictly between bundled and configured: a fetched rate
// should beat the shipped table but lose to an operator's explicit override.
func TestPhase7_DiscoveredRanksBetweenBundledAndConfigured(t *testing.T) {
	if !(ProvBundled < ProvDiscovered && ProvDiscovered < ProvConfigured) {
		t.Fatalf("ordering broken: bundled=%d discovered=%d configured=%d",
			ProvBundled, ProvDiscovered, ProvConfigured)
	}
	rate := func(perM float64, prov Provenance) Entry {
		var r Rates
		r.Base[TierInput], r.Set[TierInput] = perM/tokensPerMillion, true
		return Entry{Host: "gw.internal", Model: "claude-opus-5", Rates: r, Prov: prov}
	}
	for _, tc := range []struct {
		name    string
		entries []Entry
		want    float64
		prov    Provenance
	}{
		{"discovered beats bundled", []Entry{rate(5, ProvBundled), rate(3, ProvDiscovered)}, 3, ProvDiscovered},
		{"configured beats discovered", []Entry{rate(3, ProvDiscovered), rate(1, ProvConfigured)}, 1, ProvConfigured},
		{"all three", []Entry{rate(5, ProvBundled), rate(3, ProvDiscovered), rate(1, ProvConfigured)}, 1, ProvConfigured},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tab, err := NewTable(tc.entries)
			if err != nil {
				t.Fatal(err)
			}
			r, p := tab.Resolve("gw.internal", "claude-opus-5", 0)
			v, _ := r.For(TierInput)
			if v*tokensPerMillion != tc.want || p != tc.prov {
				t.Errorf("got %.2f/Mtok (%s), want %.2f (%s)", v*tokensPerMillion, p, tc.want, tc.prov)
			}
		})
	}
}

// A discovery client must be able to install a refreshed table without any consumer
// re-registering. That is the whole reason Registry holds an atomic pointer.
func TestPhase7_DiscoveryCanSwapWithoutTouchingConsumers(t *testing.T) {
	mk := func(perM float64, prov Provenance) *Table {
		var r Rates
		r.Base[TierInput], r.Set[TierInput] = perM/tokensPerMillion, true
		tab, err := NewTable([]Entry{{Host: "*", Model: "*", Rates: r, Prov: prov}})
		if err != nil {
			t.Fatal(err)
		}
		return tab
	}
	reg := NewRegistry(mk(5, ProvBundled))
	// Captured before the refresh, exactly as a plugin and the usage aggregator are.
	var consumer Resolver = reg

	reg.Swap(mk(3, ProvDiscovered)) // what a refresh loop would do

	r, p := consumer.Resolve("gw.internal", "claude-opus-5", 0)
	v, _ := r.For(TierInput)
	if v*tokensPerMillion != 3 || p != ProvDiscovered {
		t.Errorf("consumer saw %.2f/Mtok (%s) after a refresh, want 3.00 (discovered)", v*tokensPerMillion, p)
	}
}

// Phase 7 adds config (endpoint credential, refresh interval). The strict decoder
// derives its key set by reflection, so a new field must be accepted with no change
// to strict.go — otherwise adding discovery config would start rejecting itself.
func TestPhase7_StrictDecoderPicksUpNewFieldsAutomatically(t *testing.T) {
	keys := yamlKeys(reflect.TypeOf(Config{}))
	for _, want := range []string{"bundled", "endpoints"} {
		if _, ok := keys[want]; !ok {
			t.Errorf("yamlKeys missed the declared field %q, so reflection is not driving it", want)
		}
	}
	// An endpoint block's keys likewise come from the struct, not a hand-written list.
	epKeys := yamlKeys(reflect.TypeOf(EndpointConfig{}))
	for _, want := range []string{"host", "models"} {
		if _, ok := epKeys[want]; !ok {
			t.Errorf("yamlKeys missed EndpointConfig field %q", want)
		}
	}
	// And an undeclared key is still refused, which is what makes the above matter.
	var c Config
	err := yaml.Unmarshal([]byte("refresh: 5m\n"), &c)
	if err == nil {
		t.Fatal("an undeclared key was accepted; strictness is not in effect")
	}
	if !strings.Contains(err.Error(), "refresh") {
		t.Errorf("error %q does not name the undeclared key", err)
	}
}

// Cost must not trust a rate, because discovery's rates arrive from a remote gateway
// and never pass through config validation.
func TestPhase7_CostGuardsRemoteRates(t *testing.T) {
	var r Rates
	r.Base[TierInput], r.Set[TierInput] = -1, true
	if _, ok := Cost(r, Usage{Input: 1000}); ok {
		t.Error("Cost accepted a negative rate; a hostile /model/info response would emit negative money")
	}
}
