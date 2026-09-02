package main

import (
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/sessionapi"
)

// This binary links every default plugin, so it is the only place the
// FULL shipped catalog can be asserted. authlib's own tests see just the
// handful of plugins their side-effect imports pull in.
//
// Guards the two things a config generator reads off /v1/plugins:
// which chain each plugin belongs in, and its config field metadata.
func TestShippedCatalogPublishesDirections(t *testing.T) {
	cat := sessionapi.PluginsCatalog()
	if len(cat) == 0 {
		t.Fatal("catalog is empty; plugin registration is broken")
	}
	for _, e := range cat {
		if len(e.Directions) == 0 {
			t.Errorf("plugin %q publishes no directions; a config generator "+
				"cannot place it in a chain", e.Name)
			continue
		}
		for _, d := range e.Directions {
			if d != "inbound" && d != "outbound" {
				t.Errorf("plugin %q publishes unknown direction %q", e.Name, d)
			}
		}
		// The singular Direction is positional and must stay empty here:
		// the catalog describes types, not configured instances.
		if e.Direction != "" {
			t.Errorf("plugin %q: catalog should not set the positional Direction, got %q",
				e.Name, e.Direction)
		}
	}
}

// Field metadata for the plugins whose ConfigSchema this change added.
// Asserted here because litellm-budget-track is not linked into
// authlib's own test binaries.
//
// session-budget is deliberately absent: it is opt-IN
// (-tags include_plugin_sessionbudget, because it pulls in go-redis), so
// it is not part of the default build's catalog. Its schema is covered
// by authlib/plugins' TestConfigSchemaShapes when linked.
func TestShippedCatalogPublishesFieldSchemas(t *testing.T) {
	want := map[string]int{
		"mcp-parser":           1,
		"opa":                  6,
		"litellm-budget-track": 6,
	}
	got := map[string]int{}
	for _, e := range sessionapi.PluginsCatalog() {
		if _, tracked := want[e.Name]; tracked {
			got[e.Name] = len(e.Fields)
		}
	}
	for name, n := range want {
		have, present := got[name]
		if !present {
			t.Errorf("%s is not in the shipped catalog", name)
			continue
		}
		if have != n {
			t.Errorf("%s publishes %d schema fields, want %d", name, have, n)
		}
	}
}

// litellm-budget-track is the first plugin to expose float config, which
// is what motivated the "number" schema type. Without it these six
// fields would publish as "unknown" and render untyped in templates.
func TestShippedCatalogFloatFieldsAreNumbers(t *testing.T) {
	for _, e := range sessionapi.PluginsCatalog() {
		if e.Name != "litellm-budget-track" {
			continue
		}
		for _, f := range e.Fields {
			switch f.Name {
			case "spend_file":
				if f.Type != "string" {
					t.Errorf("%s: type = %q, want string", f.Name, f.Type)
				}
			default:
				if f.Type != "number" {
					t.Errorf("%s: type = %q, want number", f.Name, f.Type)
				}
			}
		}
		return
	}
	t.Skip("litellm-budget-track not linked")
}
