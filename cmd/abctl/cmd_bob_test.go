package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// bobSettings is a plausible IBM Bob settings.json: flat dotted keys, which is the
// whole shape difference from Claude Code's nested "env" block. Deliberately long
// because TestBobEnable_PreservesEverythingElse is the test that matters most here —
// this is a file the user has configured by hand and we are editing it for them.
const bobSettings = `{
  "workbench.colorTheme": "Default Dark Modern",
  "editor.fontSize": 13,
  "editor.fontFamily": "Menlo, Monaco, monospace",
  "editor.tabSize": 4,
  "editor.formatOnSave": true,
  "editor.rulers": [80, 100],
  "files.autoSave": "onFocusChange",
  "files.trimTrailingWhitespace": true,
  "terminal.integrated.fontSize": 12,
  "git.autofetch": true,
  "telemetry.telemetryLevel": "off",
  "http.proxyAuthorization": "keep-me",
  "extensions.autoUpdate": false
}`

// noPrompt stubs the confirmation for the duration of one test.
//
// Necessary, not tidiness: `go test` inherits the terminal it was launched from, so an
// unstubbed bobConfirm opens /dev/tty and blocks the run waiting for a keystroke.
// Returns the count of prompts so a test can assert one was actually asked.
func noPrompt(t *testing.T, answer bool) *int {
	t.Helper()
	asked := 0
	saved := bobConfirm
	bobConfirm = func(path, what string, stdout io.Writer) bool {
		asked++
		return answer
	}
	t.Cleanup(func() { bobConfirm = saved })
	return &asked
}

// bobDoc reads the settings file back as a flat map.
func bobDoc(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, b)
	}
	return doc
}

// The basic claim: the flat key lands, with the value derived from the config.
func TestBobEnable_WritesHTTPProxy(t *testing.T) {
	settings, cfg := fixture(t, "{}")
	var out, errb bytes.Buffer
	if code := bobEnable(settings, cfg, true, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if got := bobDoc(t, settings)[bobProxyKey]; got != "http://127.0.0.1:47600" {
		t.Errorf("%s = %v, want the config's proxy", bobProxyKey, got)
	}
	// Top-level and flat, not nested under anything — the VS Code shape.
	if _, nested := bobDoc(t, settings)["http"]; nested {
		t.Error(`wrote a nested "http" object; VS Code reads the flat dotted key`)
	}
}

// The property that matters most: this is the user's own editor configuration.
func TestBobEnable_PreservesEverythingElse(t *testing.T) {
	settings, cfg := fixture(t, bobSettings)
	before := bobDoc(t, settings)

	var out, errb bytes.Buffer
	if code := bobEnable(settings, cfg, true, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}

	after := bobDoc(t, settings)
	for k, want := range before {
		got, ok := after[k]
		if !ok {
			t.Errorf("key %q was dropped", k)
			continue
		}
		// Compare through JSON so numbers and arrays compare by value.
		wj, _ := json.Marshal(want)
		gj, _ := json.Marshal(got)
		if string(wj) != string(gj) {
			t.Errorf("key %q changed: %s -> %s", k, wj, gj)
		}
	}
	// Exactly one key added, and it is ours. A neighbouring key that merely looks
	// related — http.proxyAuthorization — must be left alone, which the loop above
	// covers and this states.
	if len(after) != len(before)+1 {
		t.Errorf("key count %d -> %d, want exactly one added", len(before), len(after))
	}
	if after["http.proxyAuthorization"] != "keep-me" {
		t.Errorf("a neighbouring http.* key was touched: %v", after["http.proxyAuthorization"])
	}
	if _, err := os.Stat(settings + ".bak"); err != nil {
		t.Errorf("no backup written: %v", err)
	}
}

// Running enable twice must not prompt, rewrite, or fail the second time.
func TestBobEnable_IsIdempotent(t *testing.T) {
	settings, cfg := fixture(t, "{}")
	var out, errb bytes.Buffer
	if code := bobEnable(settings, cfg, true, &out, &errb); code != 0 {
		t.Fatalf("first run: exit %d: %s", code, errb.String())
	}
	first, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}

	asked := noPrompt(t, false) // would decline; must not be consulted at all
	out.Reset()
	errb.Reset()
	if code := bobEnable(settings, cfg, false, &out, &errb); code != 0 {
		t.Fatalf("second run: exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "Already enabled") {
		t.Errorf("second run does not report the existing state:\n%s", out.String())
	}
	if *asked != 0 {
		t.Errorf("prompted %d times for a no-op write", *asked)
	}
	second, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Error("the file was rewritten by a run that had nothing to change")
	}
}

// A corporate proxy is someone's working network configuration. Refuse, name it, and
// change nothing — do not "fix" it.
func TestBobEnable_RefusesAForeignProxy(t *testing.T) {
	settings, cfg := fixture(t, `{"http.proxy": "http://proxy.corp.example.com:3128"}`)
	before, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := bobEnable(settings, cfg, true, &out, &errb); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "proxy.corp.example.com:3128") {
		t.Errorf("the refusal does not name the value it refused to replace:\n%s", errb.String())
	}
	after, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("a refused enable still wrote to the file")
	}
	if _, err := os.Stat(settings + ".bak"); err == nil {
		t.Error("a refused enable left a .bak, so it opened the file for writing")
	}
}

// Hardcoding 47600 and ~/.cortex/ca would point Bob at nothing the moment someone
// edited their config. The IPv6 form is the one that broke a sibling command: a
// strings.Cut on ":" produced http://[:1]:47600 from [::1]:47600.
func TestBobEnable_DerivesPortAndCAFromConfig(t *testing.T) {
	for _, tc := range []struct {
		name, addr, wantProxy string
	}{
		{"moved port", "127.0.0.1:19999", "http://127.0.0.1:19999"},
		{"ipv6 loopback", "[::1]:47655", "http://[::1]:47655"},
		{"wildcard host becomes loopback", "0.0.0.0:47600", "http://localhost:47600"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings, cfg := fixture(t, "{}")
			body, err := os.ReadFile(cfg)
			if err != nil {
				t.Fatal(err)
			}
			moved := strings.Replace(string(body), `"127.0.0.1:47600"`, `"`+tc.addr+`"`, 1)
			if err := os.WriteFile(cfg, []byte(moved), 0o600); err != nil {
				t.Fatal(err)
			}

			var out, errb bytes.Buffer
			if code := bobEnable(settings, cfg, true, &out, &errb); code != 0 {
				t.Fatalf("exit %d: %s", code, errb.String())
			}
			if got := bobDoc(t, settings)[bobProxyKey]; got != tc.wantProxy {
				t.Errorf("%s = %v, want %q", bobProxyKey, got, tc.wantProxy)
			}
			// The CA in the printed trust command comes from ca_dir, so a moved
			// config must not produce a command naming the default location.
			if strings.Contains(out.String(), filepath.Join(".cortex", "ca")) {
				t.Errorf("output names the default CA dir, not the config's:\n%s", out.String())
			}
		})
	}
}

// mode: disabled means nothing terminates TLS, so a written proxy is a broken editor.
// Same gate as claude-code and exec.
func TestBobEnable_RefusesADisabledBridge(t *testing.T) {
	settings, cfg := fixture(t, "{}")
	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	off := strings.Replace(string(body), "mode: enabled", "mode: disabled", 1)
	if err := os.WriteFile(cfg, []byte(off), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := bobEnable(settings, cfg, true, &out, &errb); code != 1 {
		t.Fatalf("exit = %d, want 1: %s", code, errb.String())
	}
	if _, err := os.Stat(settings + ".bak"); err == nil {
		t.Error("a refused enable opened the file for writing")
	}
}

// VS Code permits comments in settings.json; encoding/json does not. Refuse and say
// why — do not rewrite the user's file to strip them.
func TestBobEnable_RejectsCommentedSettings(t *testing.T) {
	settings, cfg := fixture(t, "{\n  // my theme\n  \"editor.fontSize\": 13\n}")
	before, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := bobEnable(settings, cfg, true, &out, &errb); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "comment") {
		t.Errorf("the error does not name the likely cause:\n%s", errb.String())
	}
	after, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the commented file was rewritten")
	}
}

// Disable removes our value and leaves anyone else's alone. The asymmetry is the
// point: enable refuses a foreign value with exit 1, disable reports it with exit 0 —
// there is nothing wrong with a machine whose Bob uses a different proxy.
func TestBobDisable_RemovesOnlyOurs(t *testing.T) {
	ca := filepath.Join(t.TempDir(), "ca.crt")

	t.Run("ours is removed", func(t *testing.T) {
		settings, _ := fixture(t, `{"editor.fontSize": 13, "http.proxy": "http://127.0.0.1:47600"}`)
		var out, errb bytes.Buffer
		if code := bobDisable(settings, ca, true, &out, &errb); code != 0 {
			t.Fatalf("exit %d: %s", code, errb.String())
		}
		doc := bobDoc(t, settings)
		if _, ok := doc[bobProxyKey]; ok {
			t.Errorf("%s survived disable", bobProxyKey)
		}
		if doc["editor.fontSize"] == nil {
			t.Error("an unrelated key was dropped")
		}
	})

	t.Run("a foreign proxy is left in place", func(t *testing.T) {
		settings, _ := fixture(t, `{"http.proxy": "http://proxy.corp.example.com:3128"}`)
		before, err := os.ReadFile(settings)
		if err != nil {
			t.Fatal(err)
		}
		var out, errb bytes.Buffer
		if code := bobDisable(settings, ca, true, &out, &errb); code != 0 {
			t.Fatalf("exit = %d, want 0 — a foreign proxy is not an error", code)
		}
		after, err := os.ReadFile(settings)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Error("disable removed a value it did not write")
		}
	})

	t.Run("the key being absent is not an error", func(t *testing.T) {
		settings, _ := fixture(t, `{"editor.fontSize": 13}`)
		var out, errb bytes.Buffer
		if code := bobDisable(settings, ca, true, &out, &errb); code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		if !strings.Contains(out.String(), "Not enabled") {
			t.Errorf("output does not say there was nothing to do:\n%s", out.String())
		}
	})

	t.Run("a non-string value is reported, not deleted", func(t *testing.T) {
		settings, _ := fixture(t, `{"http.proxy": false}`)
		before, err := os.ReadFile(settings)
		if err != nil {
			t.Fatal(err)
		}
		var out, errb bytes.Buffer
		if code := bobDisable(settings, ca, true, &out, &errb); code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		after, err := os.ReadFile(settings)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Error("a value abctl cannot have written was deleted anyway")
		}
	})
}

// The off switch must not depend on the thing being switched off. A user who has
// uninstalled Cortex and wants Bob working again must still be able to run disable.
func TestBobDisable_WithUnreadableConfig(t *testing.T) {
	settings, cfg := fixture(t, `{"http.proxy": "http://127.0.0.1:47600"}`)
	if err := os.Remove(cfg); err != nil {
		t.Fatal(err)
	}
	// bobCAPath is the only consumer of the config in the disable path, and it is
	// best-effort — this exercises the fallback branch.
	ca := bobCAPath(cfg, t.TempDir())
	if ca == "" {
		t.Fatal("bobCAPath returned nothing with the config gone")
	}

	var out, errb bytes.Buffer
	if code := bobDisable(settings, ca, true, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if _, ok := bobDoc(t, settings)[bobProxyKey]; ok {
		t.Error("disable failed with the config missing, which is when it is most needed")
	}
}

// Status reports and never acts: three states, all exit 0, nothing written.
func TestBobStatus_ThreeStates(t *testing.T) {
	for _, tc := range []struct {
		name, settings, want string
	}{
		{"absent", `{"editor.fontSize": 13}`, "not enabled"},
		{"ours", `{"http.proxy": "http://127.0.0.1:47600"}`, "enabled in"},
		{"foreign", `{"http.proxy": "http://proxy.corp.example.com:3128"}`, "not enabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings, cfg := fixture(t, tc.settings)
			before, err := os.ReadFile(settings)
			if err != nil {
				t.Fatal(err)
			}

			var out bytes.Buffer
			if code := bobStatus(settings, cfg, bobCAPath(cfg, t.TempDir()), &out); code != 0 {
				t.Fatalf("exit = %d, want 0 — a report is not a verdict", code)
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("output does not contain %q:\n%s", tc.want, out.String())
			}
			after, err := os.ReadFile(settings)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Error("status wrote to the settings file")
			}
			if _, err := os.Stat(settings + ".bak"); err == nil {
				t.Error("status left a .bak")
			}
		})
	}
}

// Status must distinguish "pointing at Cortex" from "pointing at the port Cortex uses
// now" — reporting drift is useful, silently moving it is not.
func TestBobStatus_ReportsPortDrift(t *testing.T) {
	settings, cfg := fixture(t, `{"http.proxy": "http://127.0.0.1:47699"}`)
	var out bytes.Buffer
	if code := bobStatus(settings, cfg, bobCAPath(cfg, t.TempDir()), &out); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "47699") || !strings.Contains(out.String(), "47600") {
		t.Errorf("drift report names neither the stale value nor the current one:\n%s", out.String())
	}
}

// A declined prompt is the user saying no, and it must cost nothing.
func TestBob_DeclinedPromptWritesNothing(t *testing.T) {
	t.Run("enable", func(t *testing.T) {
		settings, cfg := fixture(t, `{"editor.fontSize": 13}`)
		before, err := os.ReadFile(settings)
		if err != nil {
			t.Fatal(err)
		}
		asked := noPrompt(t, false)

		var out, errb bytes.Buffer
		if code := bobEnable(settings, cfg, false, &out, &errb); code != exitDeclined {
			t.Fatalf("exit = %d, want %d", code, exitDeclined)
		}
		if *asked != 1 {
			t.Errorf("prompted %d times, want 1", *asked)
		}
		after, err := os.ReadFile(settings)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Error("a declined enable wrote to the file")
		}
	})

	t.Run("disable", func(t *testing.T) {
		settings, cfg := fixture(t, `{"http.proxy": "http://127.0.0.1:47600"}`)
		before, err := os.ReadFile(settings)
		if err != nil {
			t.Fatal(err)
		}
		noPrompt(t, false)

		var out, errb bytes.Buffer
		if code := bobDisable(settings, bobCAPath(cfg, t.TempDir()), false, &out, &errb); code != exitDeclined {
			t.Fatalf("exit = %d, want %d", code, exitDeclined)
		}
		after, err := os.ReadFile(settings)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Error("a declined disable wrote to the file")
		}
	})
}

// Help goes to stdout and exits 0; everything malformed goes to stderr and exits 2.
// The verb check has to come before any filesystem work, or a misspelled verb gets a
// successful-looking answer from a later step.
func TestBob_HelpAndUsageErrors(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		t.Run("help "+arg, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := runBob([]string{arg}, &out, &errb); code != 0 {
				t.Fatalf("exit = %d, want 0", code)
			}
			if !strings.Contains(out.String(), "Usage:") {
				t.Errorf("usage did not go to stdout:\n%s", out.String())
			}
			if errb.Len() != 0 {
				t.Errorf("help wrote to stderr: %s", errb.String())
			}
		})
	}

	t.Run("no arguments", func(t *testing.T) {
		var out, errb bytes.Buffer
		if code := runBob(nil, &out, &errb); code != 2 {
			t.Fatalf("exit = %d, want 2", code)
		}
		if !strings.Contains(errb.String(), "Usage:") {
			t.Errorf("usage did not go to stderr:\n%s", errb.String())
		}
	})

	t.Run("unknown verb", func(t *testing.T) {
		var out, errb bytes.Buffer
		if code := runBob([]string{"enabel"}, &out, &errb); code != 2 {
			t.Fatalf("exit = %d, want 2", code)
		}
		// Naming the valid set is the difference between a refusal and a dead end.
		for _, verb := range []string{"enable", "disable", "status"} {
			if !strings.Contains(errb.String(), verb) {
				t.Errorf("the error omits %q: %s", verb, errb.String())
			}
		}
	})

	t.Run("status does not take --yes", func(t *testing.T) {
		var out, errb bytes.Buffer
		if code := runBob([]string{"status", "--yes"}, &out, &errb); code != 2 {
			t.Errorf("exit = %d, want 2 — status writes nothing to confirm", code)
		}
	})
}

// A stray operand is usually a mistyped flag, and silently ignoring it means the run
// did something other than what was asked.
func TestBob_RejectsStrayArguments(t *testing.T) {
	for _, verb := range []string{"enable", "disable", "status"} {
		t.Run(verb, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := runBob([]string{verb, "extra"}, &out, &errb); code != 2 {
				t.Errorf("exit = %d, want 2", code)
			}
			if !strings.Contains(errb.String(), `"extra"`) {
				t.Errorf("the error does not quote the stray argument: %s", errb.String())
			}
		})
	}
}

// The printed command is the deliverable of the trust step, and it is copy-pasted by
// hand. An unquoted path with a space splits into two arguments: the settings write
// succeeds and the command under it silently does not, which is the worse of the two
// failures.
func TestBob_TrustMessageQuotesPaths(t *testing.T) {
	dir := t.TempDir()
	spaced := filepath.Join(dir, "Application Support", "ca.crt")
	for name, got := range map[string]string{
		"trust":   bobTrustNote(spaced),
		"untrust": bobUntrustNote(spaced),
		"verify":  bobVerifyNote(spaced),
	} {
		if got == "" {
			continue // non-darwin verify note
		}
		if strings.Contains(got, spaced) && !strings.Contains(got, shellQuote(spaced)) {
			t.Errorf("%s note contains the bare path, unquoted:\n%s", name, got)
		}
	}
}

// The request named bundle.crt; this implementation deliberately prints ca.crt.
//
// bundle.crt holds ~129 certificates and exists only for tools whose CA setting
// REPLACES the trust store. The macOS keychain is additive, so
// `add-trusted-cert -r trustRoot` on the bundle would install machine-wide explicit
// root trust for ~128 unrelated public CAs — far broader than asked, and one
// `delete-certificate -c authbridge-tls-bridge-ca` would not reverse it. This pins the
// deviation so it cannot be "corrected" back to the request's wording.
func TestBob_TrustMessageNamesCaCrtNotBundle(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the keychain commands are darwin-only")
	}
	ca := filepath.Join(t.TempDir(), "ca.crt")
	for name, got := range map[string]string{
		"trust":   bobTrustNote(ca),
		"untrust": bobUntrustNote(ca),
		"verify":  bobVerifyNote(ca),
	} {
		for _, line := range strings.Split(got, "\n") {
			if !strings.Contains(line, "security ") {
				continue // prose may mention bundle.crt to explain the choice
			}
			if strings.Contains(line, "bundle.crt") {
				t.Errorf("%s note runs security against the 129-cert bundle:\n%s", name, line)
			}
		}
	}
	// And the undo command has to name the keychain it added to, or a System-keychain
	// add is not undone by it.
	if !strings.Contains(bobUntrustNote(ca), bobSystemKeychain) {
		t.Errorf("the untrust command does not name the keychain:\n%s", bobUntrustNote(ca))
	}
}

// Guessing ~/.config/IBM Bob/... is the VS Code convention but unverified for this
// app, and writing a proxy setting into a file nothing reads is a silent no-op. Refuse
// and name the flag that works instead.
func TestBob_SettingsPathNonDarwin(t *testing.T) {
	got, err := bobSettingsPath("/home/someone")
	if runtime.GOOS == "darwin" {
		if err != nil {
			t.Fatalf("darwin should know the path: %v", err)
		}
		if !strings.Contains(got, filepath.Join("IBM Bob", "User", "settings.json")) {
			t.Errorf("path = %q", got)
		}
		return
	}
	if err == nil {
		t.Fatalf("guessed a path on %s: %q", runtime.GOOS, got)
	}
	if !strings.Contains(err.Error(), runtime.GOOS) {
		t.Errorf("the error does not name the platform it does not know: %v", err)
	}
}

// The ownership test decides what disable deletes, so its false positives cost more
// than claude-code's isCortexValue (a strings.Contains, which feeds a refusal there).
// These are the cases a substring match gets wrong.
func TestBobIsCortexProxy(t *testing.T) {
	for _, tc := range []struct {
		val  string
		want bool
	}{
		{"http://127.0.0.1:47600", true},
		{"http://localhost:47600", true},
		{"http://[::1]:47600", true},
		{"http://127.0.0.1:47601", true}, // the whole 476xx block is ours
		{"  http://127.0.0.1:47600  ", true},

		{"", false},
		{"http://proxy.corp.example.com:3128", false},
		{"http://127.0.0.1:3128", false},    // loopback, not our port
		{"http://192.168.1.5:47600", false}, // our port, not loopback

		// A substring match says yes to both of these. That is the reason this
		// predicate parses instead.
		{"http://corp.example.com/?next=127.0.0.1:47600", false},
		{"http://localhost:47600.evil.example.com", false},

		// enable only ever writes http://, so anything else is not ours to remove.
		{"https://127.0.0.1:47600", false},
		{"socks5://127.0.0.1:47600", false},
	} {
		if got := bobIsCortexProxy(tc.val); got != tc.want {
			t.Errorf("bobIsCortexProxy(%q) = %v, want %v", tc.val, got, tc.want)
		}
	}
}
