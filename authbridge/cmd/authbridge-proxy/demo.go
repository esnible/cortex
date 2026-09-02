package main

import (
	"os"
	"path/filepath"
)

// demoCADirDefault is the default CA directory for --demo, relative to the
// current working directory — no absolute path is baked into the binary.
// Override with --ca-dir. The built-in config is written into this same
// directory (demo.yaml), next to the generated CA.
const demoCADirDefault = "cortex-ca"

// demoConfigYAML returns the built-in --demo config with caDir interpolated: a
// forward-only proxy with the TLS bridge on (auto-generated CA in caDir) and
// the LLM / MCP / A2A parsers, so an agent's egress is decrypted and parsed.
// Kept in sync with the root README.
//
// Every listener the demo uses is pinned to loopback on an uncommon port. This
// runs on a laptop, so (a) a wildcard bind would expose an open forward proxy,
// the stats endpoint, and the unauthenticated session API (which carries
// decrypted bodies and any injected tokens) to the LAN, and (b) the usual
// 8081/909x ports collide with common dev tools. The preset only fills empty
// addresses, so these explicit values win — keep them in sync with the ports
// the installer probes and prints (authbridge/install-demo.sh). The
// enforce-redirect transparent listener isn't used here (no iptables) and
// main.go skips starting it under --demo.
//
// The YAML body is flush-left on purpose — a raw string literal preserves
// leading whitespace, so indenting these lines in source would corrupt the YAML.
func demoConfigYAML(caDir string) string {
	return `# Built-in config for: authbridge-proxy --demo
# Forward-only proxy + TLS bridge (auto-generated CA) + LLM/MCP/A2A parsers.
# The running proxy watches this file — edit it to hot-reload.
mode: proxy-sidecar
listener:
  roles: [forward]
  forward_proxy_addr: 127.0.0.1:47600
  session_api_addr: 127.0.0.1:47601
stats:
  address: 127.0.0.1:47602
tls_bridge:
  mode: enabled
  ca_dir: "` + caDir + `"
  generate_ca: true
pipeline:
  outbound:
    plugins:
      - name: inference-parser
      - name: mcp-parser
      - name: a2a-parser
      # tool-prune drops unused tool definitions from the outbound manifest.
      # It ships inert: the remove list is empty, so it does nothing until you
      # fill it in, and on_error: observe means even then it only measures --
      # counting what it *would* remove while the bytes on the wire stay
      # untouched. Read the projection in abctl's plugin pane, then switch
      # on_error to enforce once the numbers look right. Fill the list with:
      #   abctl tools scan --write <this file>
      # Keep it last: it rewrites the request body, and body readers must
      # precede the mutator so they see the original bytes.
      - name: tool-prune
        on_error: observe
        config:
          remove: []
`
}

// writeDemoConfig writes the built-in --demo config next to the CA (in caDir)
// and returns its path, so --demo reuses the normal file-based load +
// hot-reload path — edits to the file are picked up live. caDir is
// caller-resolved (cwd-relative by default, or --ca-dir); no absolute path is
// baked into the binary. Overwrites any prior copy so the preset is canonical.
func writeDemoConfig(caDir string) (string, error) {
	if err := os.MkdirAll(caDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(caDir, "demo.yaml")
	if err := os.WriteFile(path, []byte(demoConfigYAML(caDir)), 0o644); err != nil {
		return "", err
	}
	return path, nil
}
