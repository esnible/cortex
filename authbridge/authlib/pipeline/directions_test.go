package pipeline

import (
	"reflect"
	"testing"
)

// A plugin that declares nothing is unconstrained: Supports must answer
// "no objection" for every direction, which is what keeps the field
// advisory and backward-compatible with out-of-tree plugins.
func TestSupportsUnconstrained(t *testing.T) {
	var caps PluginCapabilities
	for _, d := range []Direction{Inbound, Outbound} {
		if !caps.Supports(d) {
			t.Errorf("nil Directions should support %s", d)
		}
	}
	// An explicitly-empty slice behaves the same as nil.
	caps.Directions = []Direction{}
	for _, d := range []Direction{Inbound, Outbound} {
		if !caps.Supports(d) {
			t.Errorf("empty Directions should support %s", d)
		}
	}
}

func TestSupports(t *testing.T) {
	cases := []struct {
		name       string
		declared   []Direction
		wantIn     bool
		wantOutbnd bool
	}{
		{"inbound only", []Direction{Inbound}, true, false},
		{"outbound only", []Direction{Outbound}, false, true},
		{"both", []Direction{Inbound, Outbound}, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			caps := PluginCapabilities{Directions: c.declared}
			if got := caps.Supports(Inbound); got != c.wantIn {
				t.Errorf("Supports(Inbound) = %v, want %v", got, c.wantIn)
			}
			if got := caps.Supports(Outbound); got != c.wantOutbnd {
				t.Errorf("Supports(Outbound) = %v, want %v", got, c.wantOutbnd)
			}
		})
	}
}

// Normalize canonicalizes Directions so two literals describing the same
// plugin can't produce two different cached/wire representations.
func TestNormalizeDirections(t *testing.T) {
	cases := []struct {
		name string
		in   []Direction
		want []Direction
	}{
		{"nil stays nil", nil, nil},
		{"empty becomes nil", []Direction{}, nil},
		{"sorts", []Direction{Outbound, Inbound}, []Direction{Inbound, Outbound}},
		{"dedups", []Direction{Inbound, Inbound}, []Direction{Inbound}},
		{"dedups and sorts", []Direction{Outbound, Inbound, Outbound}, []Direction{Inbound, Outbound}},
		{"already canonical", []Direction{Inbound, Outbound}, []Direction{Inbound, Outbound}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := PluginCapabilities{Directions: c.in}.Normalize().Directions
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("Normalize().Directions = %v, want %v", got, c.want)
			}
		})
	}
}

// Normalize must not reorder the caller's slice: a plugin returning a
// package-level slice from Capabilities() would otherwise have it
// permuted underneath it by whoever normalized first.
func TestNormalizeDoesNotMutateInput(t *testing.T) {
	orig := []Direction{Outbound, Inbound}
	caps := PluginCapabilities{Directions: orig}
	_ = caps.Normalize()
	if orig[0] != Outbound || orig[1] != Inbound {
		t.Fatalf("Normalize mutated the input slice: %v", orig)
	}
}

// Normalize is idempotent — the catalog normalizes on read, so applying
// it twice must not change the answer.
func TestNormalizeDirectionsIdempotent(t *testing.T) {
	once := PluginCapabilities{Directions: []Direction{Outbound, Inbound, Inbound}}.Normalize()
	twice := once.Normalize()
	if !reflect.DeepEqual(once.Directions, twice.Directions) {
		t.Errorf("not idempotent: %v then %v", once.Directions, twice.Directions)
	}
}
