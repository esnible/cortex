package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"gopkg.in/yaml.v3"
)

// The built-in config prices IBM Bob, but only a config written after it started
// doing so: writeBuiltinConfig never rewrites an existing file, so every install
// from before keeps leaving each Bob request unpriced. This adds the same entry to
// those configs.
//
// A step of its own rather than another listener pin, because it differs from the
// pins in both ways service install cares about. The running proxy hot-reloads
// pricing, so adding it is no reason to restart; and a config without it exposes
// nothing, so failing to add it is no reason to refuse.

// bobHost is the gateway IBM Bob's IDE and shell send inference to.
const bobHost = "api.us-east.bob.ibm.com"

// bobProbeModel is the model name asked about when deciding whether Bob is already
// priced. Every production tier of Bob's IDE and shell reaches the gateway as
// premium-ide, and no bundled table names it, so it resolves only if the
// operator's own config prices it.
const bobProbeModel = "premium-ide"

// bobEndpointLines is the entry, unindented. KEEP IN STEP with the pricing block in
// builtinConfigYAML (cmd/cortex/local.go): a fresh install and a migrated one
// should price Bob identically. Both are pinned at 2 Bobcoins per Mtok by tests.
var bobEndpointLines = []string{
	"# Added by agentop: IBM Bob bills in Bobcoins at a flat rate no vendor list",
	"# carries, so without this entry every Bob request is unpriced. Edit the rate",
	"# here if IBM changes it; nothing else will.",
	`- hosts: ["` + bobHost + `"]`,
	"  unit: Bobcoins",
	"  models:",
	`    "*":                               # every Bob model, every tier: 2 per million tokens`,
	"      input_cost_per_million:       2.00",
	"      output_cost_per_million:      2.00",
	"      cache_read_cost_per_million:  2.00   # no cache discount on this gateway",
	"      cache_write_cost_per_million: 2.00",
}

// migrateBobPricing adds the Bob endpoint to a config that leaves Bob unpriced. It
// reports whether it changed the file.
func migrateBobPricing(path string, stdout io.Writer) (changed bool, err error) {
	raw, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return false, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return false, fmt.Errorf("%s does not parse (%w); not touching it", path, err)
	}
	// Asked of the pricing table the proxy builds, not of the file's text. Whatever
	// already prices Bob — an entry for its host, a glob, a catch-all rate — is the
	// operator's choice and stays; a catch-all multiplier prices nothing Bob serves,
	// so it does not count.
	if priced, perr := bobPriced(cfg); perr != nil || priced {
		return false, perr
	}

	updated, err := insertBobEndpoint(string(raw))
	if err != nil {
		return false, err
	}

	// The backup is this step's own, overwritten each time: it runs only when Bob is
	// unpriced, so it always holds the file as it was just before the entry went in.
	// The pin migration's backup keeps the file from before agentop first touched it,
	// which may be months older than this edit.
	bak := path + ".before-agentop-pricing"
	if werr := os.WriteFile(bak, raw, 0o600); werr != nil {
		return false, fmt.Errorf("writing %s: %w", bak, werr)
	}
	// A config that loads but still leaves Bob unpriced means the entry landed
	// somewhere other than pricing.endpoints. Refuse it rather than report a fix
	// that did not happen.
	if err := replaceConfig(path, updated, func(c *config.Config) error {
		if ok, berr := bobPriced(c); berr != nil || !ok {
			return fmt.Errorf("the added entry does not price %s", bobHost)
		}
		return nil
	}); err != nil {
		return false, err
	}

	fmt.Fprintf(stdout, "Updated %s (previous kept as %s):\n", path, bak)
	fmt.Fprintf(stdout, "  + pricing.endpoints: %s at 2 Bobcoins per million tokens   (was: unpriced)\n", bobHost)
	fmt.Fprintf(stdout, "  A running proxy reloads pricing from the file; this needs no restart.\n")
	return true, nil
}

// bobPriced reports whether cfg's pricing resolves a rate for Bob's traffic.
func bobPriced(cfg *config.Config) (bool, error) {
	tab, err := pricing.Build(cfg.Pricing)
	if err != nil {
		return false, err
	}
	_, prov := tab.Resolve(bobHost, bobProbeModel, 0)
	return prov != pricing.ProvNone, nil
}

// insertBobEndpoint adds the Bob entry to pricing.endpoints, creating either level
// when it is absent, at whatever indentation the file already uses.
//
// Positions come from the YAML parser rather than from scanning for a block's end:
// the entry goes directly under the key it belongs to, which needs only that key's
// line and its children's column. Where the block ends never matters.
func insertBobEndpoint(src string) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		return "", fmt.Errorf("reading the config's structure: %w", err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return "", fmt.Errorf("the config is not a mapping; not editing it")
	}
	root := doc.Content[0]
	lines := strings.Split(src, "\n")

	pk, pv := mappingEntry(root, "pricing")
	if pk == nil {
		// No pricing section: append one after the last line with anything on it, at
		// the document's own top-level indentation.
		indent := root.Column - 1
		at := len(lines)
		for at > 0 && strings.TrimSpace(lines[at-1]) == "" {
			at--
		}
		add := append([]string{pad(indent) + "pricing:", pad(indent+2) + "endpoints:"},
			indented(bobEndpointLines, indent+4)...)
		return insertLines(lines, at, add), nil
	}

	switch {
	case isEmptyValue(lines, pk, pv):
		return insertLines(lines, pk.Line, append([]string{pad(pk.Column+1) + "endpoints:"},
			indented(bobEndpointLines, pk.Column+3)...)), nil
	case pv.Kind != yaml.MappingNode:
		return "", fmt.Errorf("pricing: is not a mapping; add the %s entry by hand (see docs/pricing.md)", bobHost)
	case pv.Style&yaml.FlowStyle != 0:
		return "", fmt.Errorf("pricing: is written in flow style; rewrite it as an indented block, "+
			"or add the %s entry by hand (see docs/pricing.md)", bobHost)
	}

	ek, ev := mappingEntry(pv, "endpoints")
	switch {
	case ek == nil:
		// pv.Column is the column of pricing's first child key, so the new key lines
		// up with its siblings.
		return insertLines(lines, pk.Line, append([]string{pad(pv.Column-1) + "endpoints:"},
			indented(bobEndpointLines, pv.Column+1)...)), nil
	case isEmptyValue(lines, ek, ev):
		return insertLines(lines, ek.Line, indented(bobEndpointLines, ek.Column+1)), nil
	case ev.Kind != yaml.SequenceNode:
		return "", fmt.Errorf("pricing.endpoints is not a list; add the %s entry by hand (see docs/pricing.md)", bobHost)
	case ev.Style&yaml.FlowStyle != 0:
		return "", fmt.Errorf("pricing.endpoints is written in flow style; rewrite it as an indented list, "+
			"or add the %s entry by hand (see docs/pricing.md)", bobHost)
	}
	// A block sequence's column is its first dash, so the new item becomes the
	// first one, at the same column as the rest, compact or indented.
	return insertLines(lines, ek.Line, indented(bobEndpointLines, ev.Column-1)), nil
}

// mappingEntry returns the key and value nodes for key in m, or nils.
func mappingEntry(m *yaml.Node, key string) (k, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i], m.Content[i+1]
		}
	}
	return nil, nil
}

// isEmptyValue reports whether k has no value at all — `pricing:` with nothing
// after it but a comment — so children can go on the following lines. An explicit
// `~` or `null` is a value written on the key's own line, and indented children
// beneath it would not parse.
func isEmptyValue(lines []string, k, v *yaml.Node) bool {
	if v.Kind != yaml.ScalarNode || v.Tag != "!!null" {
		return false
	}
	line := lines[k.Line-1]
	rest := line[min(len(line), k.Column-1+len(k.Value)):]
	rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), ":"))
	return rest == "" || strings.HasPrefix(rest, "#")
}

// insertLines returns lines with add inserted before index at, joined back up.
func insertLines(lines []string, at int, add []string) string {
	out := make([]string, 0, len(lines)+len(add))
	out = append(out, lines[:at]...)
	out = append(out, add...)
	out = append(out, lines[at:]...)
	return strings.Join(out, "\n")
}

func indented(lines []string, n int) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = pad(n) + l
	}
	return out
}

func pad(n int) string { return strings.Repeat(" ", n) }
