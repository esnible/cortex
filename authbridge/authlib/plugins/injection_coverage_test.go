package plugins

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEveryBinaryInjectsPricing guards the gap that shipped in this PR's first
// round: only cmd/authbridge-proxy was wired, so authbridge-envoy and
// authbridge-cpex injected no resolver at all.
//
// That was a zero-config REGRESSION rather than a missing feature. tool-prune is
// linked into those binaries by default and used to carry its own rate table, so
// `$ saved` worked with no configuration everywhere. Worse, config.Validate builds
// the pricing table and discards it, so an operator's `pricing:` block validated
// cleanly in envoy mode and was then never applied — the failure reports as
// "nothing is priced", which is indistinguishable from having configured nothing.
//
// A source-level assertion because the alternative is a live pipeline per binary,
// and the thing being checked is precisely that a call site was not forgotten.
func TestEveryBinaryInjectsPricing(t *testing.T) {
	root := filepath.Join("..", "..", "cmd")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Skipf("cmd/ not reachable from this module: %v", err)
	}

	// Binaries that build plugin pipelines. abctl is a client and has none.
	buildsPipelines := regexp.MustCompile(`plugins\.Build(WithDeps|WithSPIFFE)?\(`)

	for _, e := range entries {
		if !e.IsDir() || e.Name() == "abctl" {
			continue
		}
		dir := filepath.Join(root, e.Name())
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(files) == 0 {
			continue
		}
		var src strings.Builder
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			src.Write(b)
		}
		body := src.String()
		if !buildsPipelines.MatchString(body) {
			continue // not a pipeline-building binary
		}

		t.Run(e.Name(), func(t *testing.T) {
			if strings.Contains(body, "plugins.BuildWithSPIFFE(") {
				t.Errorf("%s still calls BuildWithSPIFFE, which injects no pricing resolver; "+
					"use BuildWithDeps with Deps{SPIFFE: ..., Pricing: ...}", e.Name())
			}
			if !strings.Contains(body, "pricing.NewRegistry(") {
				t.Errorf("%s builds pipelines but never constructs a pricing.Registry, so every "+
					"request it serves is unpriced and any `pricing:` config is silently ignored", e.Name())
			}
			if !strings.Contains(body, "Pricing: pricingRegistry") {
				t.Errorf("%s constructs a registry but does not pass it in Deps", e.Name())
			}
			if !strings.Contains(body, "pricingRegistry.Swap(") {
				t.Errorf("%s never swaps the table on reload, so rates would be frozen at boot", e.Name())
			}
		})
	}
}
