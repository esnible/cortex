package cpex

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/rossoctl/cortex/core/config"
)

// TestDocumentedConfigsConfigure decodes every cpex plugin entry shown
// in this package's doc comment, its README and docs/cpex-plugin.md
// through the runtime loader's PluginEntry, then Configure. Issue #856:
// an example put apl:/pipelines: under config:, which Configure's strict
// decode rejects with `unknown field "apl"`.
func TestDocumentedConfigsConfigure(t *testing.T) {
	sources := []struct {
		name   string
		blocks []string
	}{
		{"plugin.go package doc", packageDocBlocks(t, "plugin.go")},
		{"README.md", markdownYAMLBlocks(t, "README.md")},
		{"docs/cpex-plugin.md", markdownYAMLBlocks(t, filepath.Join("..", "..", "..", "docs", "cpex-plugin.md"))},
	}
	for _, src := range sources {
		entries := 0
		for _, block := range src.blocks {
			for _, raw := range cpexEntryConfigs(t, src.name, block) {
				entries++
				p := setupFake(&FakeManager{})
				if err := p.Configure(withReadableConfigFile(t, raw)); err != nil {
					t.Errorf("%s: Configure: %v\nexample:\n%s", src.name, err, block)
				}
			}
		}
		if entries == 0 {
			t.Errorf("%s: no cpex plugin entry with a config block found", src.name)
		}
	}
}

// packageDocBlocks returns the indented code blocks of a Go file's
// package doc comment.
func packageDocBlocks(t *testing.T, file string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ParseComments|parser.PackageClauseOnly)
	if err != nil {
		t.Fatal(err)
	}
	if f.Doc == nil {
		t.Fatalf("%s has no package doc", file)
	}
	var blocks []string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			blocks = append(blocks, strings.Join(cur, "\n"))
			cur = nil
		}
	}
	for _, line := range strings.Split(f.Doc.Text(), "\n") {
		if strings.HasPrefix(line, "\t") {
			cur = append(cur, strings.TrimPrefix(line, "\t"))
			continue
		}
		flush()
	}
	flush()
	return blocks
}

// markdownYAMLBlocks returns the bodies of a markdown file's ```yaml fences.
func markdownYAMLBlocks(t *testing.T, file string) []string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var blocks []string
	rest := string(b)
	for {
		i := strings.Index(rest, "```yaml\n")
		if i < 0 {
			return blocks
		}
		rest = rest[i+len("```yaml\n"):]
		j := strings.Index(rest, "\n```")
		if j < 0 {
			t.Fatalf("%s: unterminated yaml fence", file)
		}
		blocks = append(blocks, rest[:j])
		rest = rest[j:]
	}
}

// cpexEntryConfigs returns the raw config of every `name: cpex` plugin
// entry in a YAML document, decoded the way the runtime loader does.
func cpexEntryConfigs(t *testing.T, src, doc string) []json.RawMessage {
	t.Helper()
	if !strings.Contains(doc, "name: cpex") {
		return nil
	}
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &root); err != nil {
		t.Fatalf("%s: example is not valid YAML: %v\n%s", src, err, doc)
	}
	var out []json.RawMessage
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value == "name" && n.Content[i+1].Value == "cpex" {
					var entry config.PluginEntry
					if err := n.Decode(&entry); err != nil {
						t.Fatalf("%s: plugin entry: %v\n%s", src, err, doc)
					}
					if entry.Config != nil {
						out = append(out, entry.Config)
					}
				}
			}
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(&root)
	return out
}

// withReadableConfigFile points a config_file at a file that exists, so
// Configure's read of it succeeds outside a pod.
func withReadableConfigFile(t *testing.T, raw json.RawMessage) json.RawMessage {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["config_file"]; !ok {
		return raw
	}
	path := filepath.Join(t.TempDir(), "cpex.yaml")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m["config_file"] = path
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
