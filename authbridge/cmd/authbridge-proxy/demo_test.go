package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/config"
)

// writeDemoConfig must produce a config file inside caDir that loads, presets,
// and validates cleanly and describes a forward-only TLS-bridge observe
// pipeline pointed at that dir — otherwise --demo would fail at boot instead of
// giving users a working, hot-reloadable local demo.
func TestDemoConfig_WriteLoadsAndValidates(t *testing.T) {
	caDir := t.TempDir()

	p, err := writeDemoConfig(caDir)
	if err != nil {
		t.Fatalf("writeDemoConfig: %v", err)
	}
	if filepath.Dir(p) != caDir {
		t.Errorf("config written to %q, want inside %q", p, caDir)
	}

	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	config.ApplyPreset(cfg)
	if err := config.Validate(cfg); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if cfg.Mode != config.ModeProxySidecar {
		t.Errorf("Mode = %q, want %q", cfg.Mode, config.ModeProxySidecar)
	}

	roles := cfg.Listener.ActiveRoles()
	if !roles[config.RoleForward] || roles[config.RoleReverse] {
		t.Errorf("expected forward-only roles, got %v", roles)
	}

	// The listeners the demo uses must bind loopback on the uncommon ports the
	// installer probes/prints, never a wildcard that would expose an open forward
	// proxy, the stats endpoint, or the unauthenticated session API (decrypted
	// bodies + injected tokens) to the LAN. The transparent listener isn't started
	// under --demo (main.go gates it), so it's not asserted here.
	if got := cfg.Listener.ForwardProxyAddr; got != "127.0.0.1:47600" {
		t.Errorf("ForwardProxyAddr = %q, want loopback 127.0.0.1:47600", got)
	}
	if got := cfg.Listener.SessionAPIAddr; got != "127.0.0.1:47601" {
		t.Errorf("SessionAPIAddr = %q, want loopback 127.0.0.1:47601", got)
	}
	if got := cfg.Stats.StatsAddress; got != "127.0.0.1:47602" {
		t.Errorf("Stats.StatsAddress = %q, want loopback 127.0.0.1:47602", got)
	}

	if cfg.TLSBridge == nil {
		t.Fatalf("expected tls_bridge config, got nil")
	}
	if cfg.TLSBridge.Mode != "enabled" || !cfg.TLSBridge.GenerateCA {
		t.Errorf("expected tls_bridge enabled with generate_ca, got %+v", cfg.TLSBridge)
	}
	if cfg.TLSBridge.CADir != caDir {
		t.Errorf("CADir = %q, want %q", cfg.TLSBridge.CADir, caDir)
	}

	// Assert the exact parser set and order, not just the count — a swapped or
	// renamed plugin would otherwise pass silently.
	gotPlugins := make([]string, len(cfg.Pipeline.Outbound.Plugins))
	for i, p := range cfg.Pipeline.Outbound.Plugins {
		gotPlugins[i] = p.Name
	}
	// tool-prune must come last: it is the request-body mutator, and the
	// pipeline refuses to build a chain where a body reader follows it.
	wantPlugins := []string{"inference-parser", "mcp-parser", "a2a-parser", "tool-prune"}
	if !slices.Equal(gotPlugins, wantPlugins) {
		t.Errorf("outbound plugins = %v, want %v", gotPlugins, wantPlugins)
	}

	// tool-prune ships inert, and that is a property worth pinning: the demo
	// must never silently start rewriting a user's traffic. Two independent
	// guards — an empty remove list (nothing to do) and observe policy
	// (measure only) — so a future edit has to defeat both to enable it.
	var tp *config.PluginEntry
	for i := range cfg.Pipeline.Outbound.Plugins {
		if cfg.Pipeline.Outbound.Plugins[i].Name == "tool-prune" {
			tp = &cfg.Pipeline.Outbound.Plugins[i]
		}
	}
	if tp == nil {
		t.Fatal("tool-prune entry not found")
	}
	if tp.OnError != "observe" {
		t.Errorf("tool-prune on_error = %q, want observe so the demo only measures", tp.OnError)
	}
	if !strings.Contains(string(tp.Config), "\"remove\":[]") &&
		!strings.Contains(string(tp.Config), "\"remove\": []") {
		t.Errorf("tool-prune must ship with an empty remove list, got %s", tp.Config)
	}
}
