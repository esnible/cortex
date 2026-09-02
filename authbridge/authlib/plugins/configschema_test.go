package plugins

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// Every config-bearing plugin should publish field metadata, otherwise
// /v1/plugins reports it as a bare name and abctl's templates render it
// with no documented options.
//
// "Config-bearing" is detected structurally: a plugin implementing
// pipeline.Configurable takes a config block, so it should also
// implement SchemaProvider to describe it. Parsers with no config at all
// (a2a-parser, inference-parser) implement neither and are correctly
// exempt without needing an allowlist.
func TestConfigurablePluginsPublishSchema(t *testing.T) {
	cat := Catalog()
	if len(cat) == 0 {
		t.Skip("no plugins linked into this test binary")
	}
	for _, e := range cat {
		factory, ok := factoryFor(e.Name)
		if !ok {
			t.Errorf("catalog lists %q but no factory resolves it", e.Name)
			continue
		}
		inst := factory()
		if _, configurable := inst.(pipeline.Configurable); !configurable {
			// No config block; nothing to describe.
			if len(e.Fields) > 0 {
				t.Errorf("%s publishes %d schema fields but is not Configurable",
					e.Name, len(e.Fields))
			}
			continue
		}
		if _, hasSchema := inst.(pipeline.SchemaProvider); !hasSchema {
			t.Errorf("%s is Configurable but does not implement ConfigSchema(); "+
				"add `func (p *T) ConfigSchema() []pipeline.FieldSchema { "+
				"return pipeline.SchemaOf(tConfig{}) }`", e.Name)
			continue
		}
		if len(e.Fields) == 0 {
			t.Errorf("%s implements SchemaProvider but Catalog() reports no fields", e.Name)
		}
	}
}

// A schema field name that doesn't match a real json tag would send
// operators to a key the plugin's decoder rejects (these plugins decode
// with DisallowUnknownFields). Feed every advertised key back through
// Configure to prove the names are real.
//
// Uses factoryFor rather than Build: Build wraps plugins in
// configuredPlugin, which embeds only the narrow pipeline.Plugin
// interface, so neither Configurable nor SchemaProvider survives a type
// assertion through a built instance. Catalog() reads schemas off the raw
// factory instance, which is the path that actually ships.
func TestConfigSchemaFieldNamesDecode(t *testing.T) {
	for _, e := range Catalog() {
		factory, ok := factoryFor(e.Name)
		if !ok || len(e.Fields) == 0 {
			continue
		}
		cfg, ok := factory().(pipeline.Configurable)
		if !ok {
			continue
		}
		t.Run(e.Name, func(t *testing.T) {
			// Every advertised key, with a type-appropriate zero value.
			// A bogus name trips DisallowUnknownFields.
			obj := make(map[string]any, len(e.Fields))
			for _, f := range e.Fields {
				obj[f.Name] = zeroFor(f)
			}
			raw, err := json.Marshal(obj)
			if err != nil {
				t.Fatalf("marshal probe config: %v", err)
			}
			// Configure may legitimately reject on VALUE grounds (a
			// required field left empty, a cross-field rule). What must
			// not happen is an unknown-field rejection — that means the
			// schema advertises a key the struct doesn't have.
			if err := cfg.Configure(raw); err != nil {
				if strings.Contains(err.Error(), "unknown field") {
					t.Errorf("%s: schema advertises a field the decoder rejects: %v", e.Name, err)
				}
			}
		})
	}
}

// zeroFor returns a JSON-encodable zero value matching a field's declared
// schema type, so the probe config decodes without type errors.
func zeroFor(f pipeline.FieldSchema) any {
	switch f.Type {
	case "string":
		if len(f.Enum) > 0 {
			return f.Enum[0]
		}
		return ""
	case "int":
		return 0
	case "number":
		return 0.0
	case "bool":
		return false
	case "[]string":
		return []string{}
	case "object":
		inner := make(map[string]any, len(f.Fields))
		for _, sub := range f.Fields {
			inner[sub.Name] = zeroFor(sub)
		}
		return inner
	default:
		return nil
	}
}

// The plugins this change wired up report the field count and required
// set their config structs actually have, guarding against a SchemaOf
// pointed at the wrong type. Only asserts plugins present in this test
// binary; TestConfigurablePluginsPublishSchema covers the rest
// structurally.
func TestConfigSchemaShapes(t *testing.T) {
	want := map[string]struct {
		nFields  int
		required []string
	}{
		"mcp-parser":           {1, nil},
		"opa":                  {6, []string{"bundle_url"}},
		"session-budget":       {18, []string{"redis_url"}},
		"litellm-budget-track": {6, []string{"spend_file", "max_budget"}},
	}
	seen := 0
	for _, e := range Catalog() {
		w, tracked := want[e.Name]
		if !tracked {
			continue
		}
		seen++
		t.Run(e.Name, func(t *testing.T) {
			if len(e.Fields) != w.nFields {
				got := make([]string, len(e.Fields))
				for i, f := range e.Fields {
					got[i] = f.Name
				}
				t.Errorf("got %d fields %v, want %d", len(e.Fields), got, w.nFields)
			}
			var required []string
			for _, f := range e.Fields {
				if f.Required {
					required = append(required, f.Name)
				}
			}
			if !reflect.DeepEqual(required, w.required) {
				t.Errorf("required = %v, want %v", required, w.required)
			}
		})
	}
	if seen == 0 {
		t.Skip("none of the tracked plugins are linked into this test binary")
	}
}
