package sessionapi

import (
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins"
)

// PluginsCatalog adapts plugins.Catalog() into the wire-shaped
// CatalogEntry slice WithCatalog expects. Both binaries (authbridge-
// proxy, -envoy) plug it in identically; centralizing the conversion
// here keeps the field list one-place.
//
// The singular Direction is left empty: it means "the chain this
// configured instance sits in", which is a property of an instance, not
// of a type. abctl renders it only for the active pipeline, where the
// answer is positional.
//
// The plural Directions IS populated: it is the type-level declaration
// of which chains a plugin supports (PluginCapabilities.Directions),
// which is exactly what a config generator needs to place a plugin.
// Empty means the plugin makes no claim.
//
// Fields is populated for plugins that implement
// pipeline.SchemaProvider (most config-bearing plugins). Plugins
// without configs emit a nil Fields slice and the wire format
// elides it via `omitempty`.
func PluginsCatalog() []CatalogEntry {
	src := plugins.Catalog()
	out := make([]CatalogEntry, len(src))
	for i, e := range src {
		n := e.Capabilities.Normalize()
		out[i] = CatalogEntry{
			Name:        e.Name,
			Directions:  directionStrings(n.Directions),
			ReadsBody:   n.ReadsBody,
			Requires:    n.Requires,
			RequiresAny: n.RequiresAny,
			Description: n.Description,
			Fields:      convertFieldSchemas(e.Fields),
		}
	}
	return out
}

// convertFieldSchemas maps the framework-level FieldSchema slice to
// the wire-level FieldSchemaEntry slice. Recurses into nested struct
// schemas. Returns nil for empty input so the JSON marshaller can
// elide the field via `omitempty`.
func convertFieldSchemas(in []pipeline.FieldSchema) []FieldSchemaEntry {
	if len(in) == 0 {
		return nil
	}
	out := make([]FieldSchemaEntry, len(in))
	for i, f := range in {
		out[i] = FieldSchemaEntry{
			Name:        f.Name,
			Type:        f.Type,
			Required:    f.Required,
			Description: f.Description,
			Default:     f.Default,
			Enum:        append([]string(nil), f.Enum...),
			Fields:      convertFieldSchemas(f.Fields),
		}
	}
	return out
}

// directionStrings renders a Direction slice as its wire form.
//
// The wire type is []string rather than []pipeline.Direction on purpose:
// Direction.UnmarshalJSON decodes any unrecognized string to Inbound
// without erroring (a deliberate forward-compatibility choice for the
// single-valued field), which on a slice would silently turn a future
// third direction into a false "inbound" claim. Strings keep the wire
// honest and match the existing Requires/RequiresAny precedent.
//
// Returns nil for empty input so the field elides via omitempty and
// "unconstrained" stays absent rather than an empty array.
func directionStrings(ds []pipeline.Direction) []string {
	if len(ds) == 0 {
		return nil
	}
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.String()
	}
	return out
}
