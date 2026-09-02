package plugins

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/config"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// unconstrainedByDesign lists registered plugins that legitimately
// declare no Directions. Test stubs only — a real plugin that forgets
// the field should fail TestRegisteredPluginsDeclareDirections rather
// than be quietly excused, so keep this list short and justified.
var unconstrainedByDesign = map[string]string{
	"jwt-validation-stub": "plugintesting stub; asserts capability plumbing, not direction",
	"token-exchange-stub": "plugintesting stub; asserts capability plumbing, not direction",
}

// Every real registered plugin declares which chain(s) it belongs in.
// Without this, a newly-added plugin silently reports "unconstrained"
// and config generators lose the placement metadata this field exists
// to provide.
func TestRegisteredPluginsDeclareDirections(t *testing.T) {
	cat := Catalog()
	if len(cat) == 0 {
		t.Skip("no plugins linked into this test binary")
	}
	for _, e := range cat {
		if why, ok := unconstrainedByDesign[e.Name]; ok {
			if len(e.Capabilities.Directions) > 0 {
				t.Errorf("%s is on the unconstrained allowlist (%s) but declares %v; "+
					"remove it from the allowlist", e.Name, why, e.Capabilities.Directions)
			}
			continue
		}
		if len(e.Capabilities.Directions) == 0 {
			t.Errorf("plugin %q declares no Directions; add them to its Capabilities() "+
				"(see docs/plugin-catalog.md for the intended chain), or add it to "+
				"unconstrainedByDesign with a reason", e.Name)
		}
	}
}

// Catalog() hands out clones. Mutating a returned entry's Directions must
// not taint the memoized snapshot every later caller (and every
// /v1/plugins response) reads. cloneCatalog enumerates capability fields
// by hand, so a new slice field is easy to forget there — this is the
// regression guard for exactly that.
func TestCatalogCloneIsolatesDirections(t *testing.T) {
	first := Catalog()
	idx := -1
	for i, e := range first {
		if len(e.Capabilities.Directions) > 0 {
			idx = i
			break
		}
	}
	if idx < 0 {
		// NOT a skip. Every real plugin declares Directions (see
		// TestRegisteredPluginsDeclareDirections), so "nobody has any"
		// means the field was dropped somewhere between Capabilities()
		// and here — cloneCatalog being the likeliest culprit, since it
		// copies capability fields by hand. Skipping would let exactly
		// the bug this test exists to catch report a green run.
		if len(first) == 0 {
			t.Skip("no plugins linked into this test binary")
		}
		t.Fatal("no catalog entry carries Directions; the field is being dropped " +
			"between Capabilities() and Catalog() (check cloneCatalog)")
	}
	name := first[idx].Name
	orig := append([]pipeline.Direction(nil), first[idx].Capabilities.Directions...)

	// Scribble on the caller's copy.
	first[idx].Capabilities.Directions[0] = pipeline.Direction(99)

	second := Catalog()
	for _, e := range second {
		if e.Name != name {
			continue
		}
		if len(e.Capabilities.Directions) != len(orig) {
			t.Fatalf("%s: length changed after mutation: got %v, want %v",
				name, e.Capabilities.Directions, orig)
		}
		for i := range orig {
			if e.Capabilities.Directions[i] != orig[i] {
				t.Errorf("%s: cached catalog was tainted by caller mutation: got %v, want %v",
					name, e.Capabilities.Directions, orig)
			}
		}
		return
	}
	t.Fatalf("plugin %q vanished from the catalog", name)
}

// warnBuffer returns a logger writing to a buffer plus the buffer.
func warnBuffer() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})), &buf
}

func cfgWith(inbound, outbound []string) *config.Config {
	toEntries := func(names []string) []config.PluginEntry {
		out := make([]config.PluginEntry, len(names))
		for i, n := range names {
			out[i] = config.PluginEntry{Name: n}
		}
		return out
	}
	c := &config.Config{}
	c.Pipeline.Inbound.Plugins = toEntries(inbound)
	c.Pipeline.Outbound.Plugins = toEntries(outbound)
	return c
}

// A plugin placed in a chain it doesn't declare gets a WARN naming both
// the plugin and the mismatch — but the process is expected to continue.
func TestWarnPluginDirectionsFlagsMisplacement(t *testing.T) {
	if _, ok := factoryFor("jwt-validation"); !ok {
		t.Skip("jwt-validation not linked into this test binary")
	}
	logger, buf := warnBuffer()
	// jwt-validation declares inbound; putting it outbound is the mistake.
	WarnPluginDirections(cfgWith(nil, []string{"jwt-validation"}), logger)

	out := buf.String()
	if !strings.Contains(out, "jwt-validation") {
		t.Errorf("warning should name the plugin:\n%s", out)
	}
	if !strings.Contains(out, "outbound") {
		t.Errorf("warning should name the configured direction:\n%s", out)
	}
	if !strings.Contains(out, "inbound") {
		t.Errorf("warning should report the declared direction:\n%s", out)
	}
}

// The correct placement, a both-chain plugin, and an unknown name must
// all stay silent. The unknown-name case matters because Build already
// reports it with a better error listing every registered plugin.
func TestWarnPluginDirectionsStaysSilent(t *testing.T) {
	cases := []struct {
		name            string
		needs           string
		inbound, outbnd []string
	}{
		{"correct placement", "jwt-validation", []string{"jwt-validation"}, nil},
		{"both-chain plugin inbound", "opa", []string{"opa"}, nil},
		{"both-chain plugin outbound", "opa", nil, []string{"opa"}},
		{"unknown plugin name", "", nil, []string{"no-such-plugin"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.needs != "" {
				if _, ok := factoryFor(c.needs); !ok {
					t.Skipf("%s not linked into this test binary", c.needs)
				}
			}
			logger, buf := warnBuffer()
			WarnPluginDirections(cfgWith(c.inbound, c.outbnd), logger)
			if out := buf.String(); out != "" {
				t.Errorf("expected no warning, got:\n%s", out)
			}
		})
	}
}

// The warning is advisory: a misplaced plugin must still build into a
// working pipeline. If this ever fails, the check has become fatal and
// the feature's core promise is broken.
func TestWarnPluginDirectionsIsAdvisory(t *testing.T) {
	if _, ok := factoryFor("mcp-parser"); !ok {
		t.Skip("mcp-parser not linked into this test binary")
	}
	logger, buf := warnBuffer()
	// mcp-parser declares outbound; build it inbound anyway.
	entries := []config.PluginEntry{{Name: "mcp-parser"}}
	WarnPluginDirections(cfgWith([]string{"mcp-parser"}, nil), logger)
	if buf.String() == "" {
		t.Fatal("setup: expected a warning for the misplaced plugin")
	}
	p, err := Build(entries)
	if err != nil {
		t.Fatalf("Build must succeed despite the direction warning: %v", err)
	}
	if p == nil {
		t.Fatal("Build returned a nil pipeline")
	}
}

// A nil config must not panic — main() calls this right after Validate,
// but defensive since the sibling WarnEmptyPipelines is nil-tolerant too.
func TestWarnPluginDirectionsNilConfig(t *testing.T) {
	logger, buf := warnBuffer()
	WarnPluginDirections(nil, logger)
	if out := buf.String(); out != "" {
		t.Errorf("nil config should warn nothing, got:\n%s", out)
	}
}
