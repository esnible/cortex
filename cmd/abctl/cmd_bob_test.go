package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
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

	t.Run("ours is removed", func(t *testing.T) {
		settings, cfg := fixture(t, `{"editor.fontSize": 13, "http.proxy": "http://127.0.0.1:47600"}`)
		want, ca := bobWanted(cfg, t.TempDir())
		var out, errb bytes.Buffer
		if code := bobDisable(settings, want, ca, true, &out, &errb); code != 0 {
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
		settings, cfg := fixture(t, `{"http.proxy": "http://proxy.corp.example.com:3128"}`)
		want, ca := bobWanted(cfg, t.TempDir())
		before, err := os.ReadFile(settings)
		if err != nil {
			t.Fatal(err)
		}
		var out, errb bytes.Buffer
		if code := bobDisable(settings, want, ca, true, &out, &errb); code != 0 {
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
		settings, cfg := fixture(t, `{"editor.fontSize": 13}`)
		want, ca := bobWanted(cfg, t.TempDir())
		var out, errb bytes.Buffer
		if code := bobDisable(settings, want, ca, true, &out, &errb); code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		if !strings.Contains(out.String(), "Not enabled") {
			t.Errorf("output does not say there was nothing to do:\n%s", out.String())
		}
	})

	t.Run("a non-string value is reported, not deleted", func(t *testing.T) {
		settings, cfg := fixture(t, `{"http.proxy": false}`)
		want, ca := bobWanted(cfg, t.TempDir())
		before, err := os.ReadFile(settings)
		if err != nil {
			t.Fatal(err)
		}
		var out, errb bytes.Buffer
		if code := bobDisable(settings, want, ca, true, &out, &errb); code != 0 {
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
//
// This is the test that caught the regression in the first version of the fix for
// the exact-match report. Judging ownership by comparing against the config is
// right when there IS a config; making that the only rule meant a missing config
// answered "not a Cortex proxy" for Cortex's own default address, and disable
// then refused to remove it. That is precisely the case where the off switch
// matters most, so bobOwns answers bobUnknown here — a loopback http proxy with
// nothing to compare against — and disable proceeds while saying what it is going
// on.
//
// Both halves are asserted, because removing the value is only half the contract:
// the user is entitled to know the judgment was made by shape rather than by
// matching their config.
func TestBobDisable_WithUnreadableConfig(t *testing.T) {
	settings, cfg := fixture(t, `{"http.proxy": "http://127.0.0.1:47600"}`)
	if err := os.Remove(cfg); err != nil {
		t.Fatal(err)
	}
	want, ca := bobWanted(cfg, t.TempDir())
	// The point of the fixture: with the config gone there is nothing to compare
	// the value against, so ownership cannot be decided by matching.
	if want != "" {
		t.Fatalf("bobWanted invented a proxy address from a missing config: %q", want)
	}
	// The CA path is best-effort and must still produce something printable, or
	// the undo command disable prints would name nothing.
	if ca == "" {
		t.Fatal("bobWanted returned no CA path with the config gone")
	}
	if got := bobOwns("http://127.0.0.1:47600", want); got != bobUnknown {
		t.Errorf("bobOwns with no config = %v, want bobUnknown", got)
	}

	var out, errb bytes.Buffer
	if code := bobDisable(settings, want, ca, true, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if _, ok := bobDoc(t, settings)[bobProxyKey]; ok {
		t.Error("disable failed with the config missing, which is when it is most needed")
	}
	if !strings.Contains(out.String(), "shape alone") {
		t.Errorf("disable did not disclose that it judged by shape:\n%s", out.String())
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
			want, ca := bobWanted(cfg, t.TempDir())
			if code := bobStatus(settings, cfg, want, ca, &out); code != 0 {
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
	want, ca := bobWanted(cfg, t.TempDir())
	if code := bobStatus(settings, cfg, want, ca, &out); code != 0 {
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
		want, ca := bobWanted(cfg, t.TempDir())
		if code := bobDisable(settings, want, ca, false, &out, &errb); code != exitDeclined {
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
//
// This table replaces TestBobIsCortexProxy, whose predicate was a bool over a port
// PREFIX. Two things were wrong with it and one test cannot have caught both:
//
//   - "476" as a prefix is not the 476xx block it documented. It also accepted
//     http://127.0.0.1:476 and http://127.0.0.1:4769999, and it accepted 47600 on a
//     machine whose Cortex listens on 19999 — the reported bug.
//   - A bool has no room for "cannot tell". With the config gone there is nothing to
//     compare against, and answering false there broke disable for the uninstalled
//     case. bobUnknown is that third answer.
//
// So the cases are grouped by which of the two they pin, and every one names the
// wantProxy it is judged against — the parameter the old predicate did not have.
func TestBobOwns(t *testing.T) {
	const want = "http://127.0.0.1:19999" // deliberately NOT in the 476xx block

	for _, tc := range []struct {
		name, val, wantProxy string
		owns                 bobOwnership
	}{
		// Judged against a config: equality on host and port, nothing looser.
		{"exact match", "http://127.0.0.1:19999", want, bobOurs},
		{"surrounding space", "  http://127.0.0.1:19999  ", want, bobOurs},
		// wantedFromLoaded itself rewrites an empty/0.0.0.0/:: host, so the two
		// loopback spellings have to be one identity or enable and disable disagree.
		{"localhost for 127.0.0.1", "http://localhost:19999", want, bobOurs},
		{"127.0.0.1 for localhost", "http://127.0.0.1:19999", "http://localhost:19999", bobOurs},
		{"ipv6 loopback", "http://[::1]:19999", want, bobOurs},

		// The reported bug. Every one of these passed the old prefix test while the
		// configured port was 19999, so disable would have deleted them.
		{"the bug: 476 prefix", "http://127.0.0.1:47600", want, bobNotOurs},
		{"the bug: bare 476", "http://127.0.0.1:476", want, bobNotOurs},
		{"the bug: 4769999", "http://127.0.0.1:4769999", want, bobNotOurs},

		{"empty", "", want, bobNotOurs},
		{"corporate proxy", "http://proxy.corp.example.com:3128", want, bobNotOurs},
		{"loopback, wrong port", "http://127.0.0.1:3128", want, bobNotOurs},
		{"right port, not loopback", "http://192.168.1.5:19999", want, bobNotOurs},

		// A substring match says yes to both of these. That is why this parses.
		{"port in a query string", "http://corp.example.com/?next=127.0.0.1:19999", want, bobNotOurs},
		{"suffixed host", "http://localhost:19999.evil.example.com", want, bobNotOurs},

		// enable only ever writes http://, so anything else is not ours to remove.
		{"https", "https://127.0.0.1:19999", want, bobNotOurs},
		{"socks5", "socks5://127.0.0.1:19999", want, bobNotOurs},

		// No config to compare against: the third state. A loopback http proxy is
		// the shape abctl writes, so it is removable; anything else is not.
		{"no config, loopback", "http://127.0.0.1:47600", "", bobUnknown},
		{"no config, localhost", "http://localhost:1234", "", bobUnknown},
		{"no config, ipv6", "http://[::1]:47600", "", bobUnknown},
		{"no config, corporate", "http://proxy.corp.example.com:3128", "", bobNotOurs},
		{"no config, routable ip", "http://192.168.1.5:47600", "", bobNotOurs},
		{"no config, https", "https://127.0.0.1:47600", "", bobNotOurs},
		{"no config, empty value", "", "", bobNotOurs},

		// An unparseable wantProxy must degrade to the no-config answer, not to a
		// match: comparing against a host that failed to parse would make every
		// port-less value equal to it.
		{"unparseable config value", "http://127.0.0.1:47600", "::::", bobUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := bobOwns(tc.val, tc.wantProxy); got != tc.owns {
				t.Errorf("bobOwns(%q, %q) = %v, want %v", tc.val, tc.wantProxy, got, tc.owns)
			}
		})
	}
}

// The three states must be distinct values, or a switch over them collapses two
// cases into one and every table above still passes.
func TestBobOwnershipStatesAreDistinct(t *testing.T) {
	if bobNotOurs == bobOurs || bobOurs == bobUnknown || bobNotOurs == bobUnknown {
		t.Fatalf("two states share a value: notOurs=%d ours=%d unknown=%d",
			bobNotOurs, bobOurs, bobUnknown)
	}
	// bobNotOurs must be the zero value: a bobOwnership that was never assigned
	// has to mean "do not touch it", never "delete it".
	var zero bobOwnership
	if zero != bobNotOurs {
		t.Errorf("the zero bobOwnership is %d, not bobNotOurs — an unset value would authorise a delete", zero)
	}
}

// Report #4's named gap: no test round-tripped enable then disable on a port outside
// the 476xx block, which is exactly the pair the two halves of the old suite
// contradicted each other about. TestBobEnable_DerivesPortAndCAFromConfig asserted
// enable WRITES http://127.0.0.1:19999, TestBobIsCortexProxy asserted that shape is
// not ours, and nothing crossed between them — so a 50-test suite passed over a
// disable that could not undo its own enable.
//
// Written as a round trip rather than as two assertions about one port, because what
// broke was the relationship between the two commands and not either one alone.
func TestBob_EnableDisableRoundTripOnANon476xxPort(t *testing.T) {
	const port = "19999"

	settings, cfg := fixture(t, `{"editor.fontSize": 13}`)
	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	moved := strings.Replace(string(body), "127.0.0.1:47600", "127.0.0.1:"+port, 1)
	if moved == string(body) {
		t.Fatal("the fixture config no longer names 127.0.0.1:47600, so this test moved nothing")
	}
	if err := os.WriteFile(cfg, []byte(moved), 0o600); err != nil {
		t.Fatal(err)
	}

	var enOut, enErr bytes.Buffer
	if code := bobEnable(settings, cfg, true, &enOut, &enErr); code != 0 {
		t.Fatalf("enable exit %d: %s", code, enErr.String())
	}
	got, _ := bobDoc(t, settings)[bobProxyKey].(string)
	if want := "http://127.0.0.1:" + port; got != want {
		t.Fatalf("enable wrote %q, want %q", got, want)
	}

	// The half that was missing. Same config, so disable judges the value against
	// the address enable derived it from.
	want, ca := bobWanted(cfg, t.TempDir())
	var disOut, disErr bytes.Buffer
	if code := bobDisable(settings, want, ca, true, &disOut, &disErr); code != 0 {
		t.Fatalf("disable exit %d: %s", code, disErr.String())
	}
	if _, ok := bobDoc(t, settings)[bobProxyKey]; ok {
		t.Errorf("disable could not undo its own enable on port %s:\n%s", port, disOut.String())
	}
	// The failure mode being pinned was a silent refusal, not a crash: disable
	// exited 0 and said the value was not Cortex's. Assert the refusal wording is
	// absent, so a regression cannot pass by keeping the exit code.
	if strings.Contains(disOut.String(), "not a Cortex proxy") {
		t.Errorf("disable called its own enable's value foreign:\n%s", disOut.String())
	}
	// An unrelated key must survive both writes.
	if bobDoc(t, settings)["editor.fontSize"] == nil {
		t.Error("a sibling key did not survive the round trip")
	}
}

// bobSameLoopback is what makes localhost and 127.0.0.1 one identity, and it must do
// that WITHOUT resolving names: a DNS lookup in this path would make ownership depend
// on the resolver, and a machine whose "localhost" resolves elsewhere would get a
// different answer for the same settings file.
func TestBobSameLoopback(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{"127.0.0.1", "127.0.0.1", true},
		{"localhost", "127.0.0.1", true},
		{"127.0.0.1", "localhost", true},
		{"::1", "localhost", true},
		{"localhost", "::1", true},

		{"127.0.0.1", "192.168.1.5", false},
		{"localhost", "proxy.corp.example.com", false},
		// Not a loopback spelling abctl recognises, so it is only equal to itself.
		{"127.0.0.2", "127.0.0.1", false},
		{"127.0.0.2", "127.0.0.2", true},
		{"", "", true},
		{"", "127.0.0.1", false},
	} {
		if got := bobSameLoopback(tc.a, tc.b); got != tc.same {
			t.Errorf("bobSameLoopback(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.same)
		}
	}
}

// Liveness is a separate axis from ownership, and conflating them is a bug in both
// directions: a stopped Cortex must not make its own value foreign, and a stranger's
// proxy that happens to be up must not become ours.
func TestBobProxyIsListening(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	live := "http://" + ln.Addr().String()

	if !bobProxyIsListening(live) {
		t.Errorf("a listening socket reported as not listening: %s", live)
	}

	// Close it and ask again: the same URL, the opposite answer. A predicate that
	// ignored its argument would pass the first assertion alone.
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if bobProxyIsListening("http://" + addr) {
		t.Errorf("a closed socket reported as listening: %s", addr)
	}

	// Unparseable and empty values must answer false rather than panicking — this
	// runs on whatever is in the user's settings file.
	for _, bad := range []string{"", "::::", "not a url", "http://"} {
		if bobProxyIsListening(bad) {
			t.Errorf("bobProxyIsListening(%q) = true", bad)
		}
	}

	// Ownership must not move when liveness does. Nothing is listening on this
	// port now, and the value is still ours.
	if got := bobOwns("http://"+addr, "http://"+addr); got != bobOurs {
		t.Errorf("a stopped proxy changed ownership: got %v, want bobOurs", got)
	}
}

// The printed undo must actually undo the printed change. This is the reported bug:
// enable prints `add-trusted-cert -d -r trustRoot`, which writes trust settings to the
// ADMIN domain, and disable printed only `delete-certificate -t`, whose own usage text
// says it removes the certificate and "user trust settings" — a different domain. So a
// user who ran both was left with admin-domain trustRoot settings for a certificate
// that no longer existed, and no printed command had removed them.
//
// Asserted as a pairing rather than as a literal, so the two messages cannot drift
// apart again: whatever flag enable uses to WRITE trust, disable must name the
// command that removes it from the same domain.
func TestBobTrustNoteAndUndoCoverTheSameDomain(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skipf("the keychain commands are darwin-only; this GOOS prints distro guidance")
	}
	ca := "/tmp/ca.crt"
	trust, untrust := bobTrustNote(ca), bobUntrustNote(ca)

	// What enable writes.
	if !strings.Contains(trust, "add-trusted-cert -d") {
		t.Fatalf("enable no longer writes admin-domain trust, so this test is judging the wrong thing:\n%s", trust)
	}
	// The undo for exactly that, with -d repeated. Without the -d, remove-trusted-cert
	// works on the user domain, which the add never touched.
	if !strings.Contains(untrust, "remove-trusted-cert -d") {
		t.Errorf("the undo does not remove admin-domain trust settings:\n%s", untrust)
	}
	// And the certificate itself, from the keychain the add named. A delete that
	// defaults to the login keychain does not undo a System-keychain add.
	if !strings.Contains(untrust, "delete-certificate") {
		t.Errorf("the undo never deletes the certificate:\n%s", untrust)
	}
	if !strings.Contains(trust, bobSystemKeychain) || !strings.Contains(untrust, bobSystemKeychain) {
		t.Errorf("the two messages do not name the same keychain:\nadd:\n%s\nundo:\n%s", trust, untrust)
	}
	// The bug's exact signature: delete-certificate -t as the ONLY trust-removing
	// step. Pinned directly so a regression names itself.
	if strings.Contains(untrust, "delete-certificate") && !strings.Contains(untrust, "remove-trusted-cert") {
		t.Error("the undo is delete-certificate alone again, which leaves admin-domain trust behind")
	}
}

// The backup promise must match what writeSettings actually does. It writes <path>.bak
// from the file's current contents only when the file EXISTS and no .bak is there
// already — so a single unconditional "a copy is kept as <path>.bak" was false twice:
// once for a settings file abctl is creating, and once on a second run, where the
// existing .bak is deliberately not overwritten and therefore holds the file as first
// found rather than as it is now.
//
// Three branches, three different true statements. Each asserts its own wording AND
// the absence of the claim that would be wrong there, because a message that says
// everything says nothing.
func TestBobBackupNote_MatchesWhatWriteSettingsDoes(t *testing.T) {
	t.Run("no existing file: no backup is made", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.json")
		got := bobBackupNote(path)
		if !strings.Contains(got, "No backup is made") {
			t.Errorf("does not say a backup will not be made:\n%s", got)
		}
		// The false promise being fixed.
		if strings.Contains(got, "a copy is kept") {
			t.Errorf("promises a copy of a file that does not exist:\n%s", got)
		}
	})

	t.Run("file exists, no .bak yet: a copy is kept", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		got := bobBackupNote(path)
		if !strings.Contains(got, "a copy is kept as "+path+".bak") {
			t.Errorf("does not promise the backup it will make:\n%s", got)
		}
		if strings.Contains(got, "No backup is made") || strings.Contains(got, "already exists") {
			t.Errorf("claims a different branch's outcome:\n%s", got)
		}
	})

	t.Run("a .bak already exists: it is not overwritten", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".bak", []byte(`{"original": true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		got := bobBackupNote(path)
		if !strings.Contains(got, "already exists") {
			t.Errorf("does not say the existing backup is left alone:\n%s", got)
		}
		// The distinction that matters to someone deciding whether to trust the
		// .bak: it holds the file as FIRST found, not as it is now.
		if !strings.Contains(got, "as first found") {
			t.Errorf("does not say which state the existing backup holds:\n%s", got)
		}
	})

	// The note is a claim about writeSettings, so check it against writeSettings
	// rather than only against itself. A message and an implementation that drift
	// apart is the whole bug.
	t.Run("the claim matches the behaviour", func(t *testing.T) {
		for _, exists := range []bool{false, true} {
			path := filepath.Join(t.TempDir(), "settings.json")
			if exists {
				if err := os.WriteFile(path, []byte(`{"pre": 1}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			promised := strings.Contains(bobBackupNote(path), "a copy is kept")
			if err := writeSettings(path, map[string]any{"http.proxy": "http://127.0.0.1:47600"}); err != nil {
				t.Fatal(err)
			}
			_, statErr := os.Stat(path + ".bak")
			made := statErr == nil
			if promised != made {
				t.Errorf("file existed = %v: note promised a copy = %v, writeSettings made one = %v",
					exists, promised, made)
			}
		}
	})
}
