package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The path written into the alias body. A fixed value rather than the real
// os.Executable(), so the assertions are about the block and not about where the
// test binary happens to live.
const testAbctl = "/opt/abctl/abctl"

// Which startup file each shell gets. The bash arm is the interesting one: which
// file bash reads depends on the platform AND on whether the shell is a login
// shell, so it prefers a file that already exists over creating one the shell may
// never read.
func TestBobShellRCPath(t *testing.T) {
	t.Run("zsh is unconditional", func(t *testing.T) {
		home := t.TempDir()
		got, err := bobShellRCPath("/bin/zsh", home, "darwin")
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(home, ".zshrc"); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	// Precedence among the three candidates, and the fact that an existing file wins
	// over the platform default.
	for _, tc := range []struct {
		name    string
		present []string
		goos    string
		want    string
	}{
		{"prefers .bash_profile", []string{".bash_profile", ".bashrc", ".profile"}, "linux", ".bash_profile"},
		{"then .bashrc", []string{".bashrc", ".profile"}, "darwin", ".bashrc"},
		{"then .profile", []string{".profile"}, "darwin", ".profile"},
		{"none present, darwin creates .bash_profile", nil, "darwin", ".bash_profile"},
		{"none present, linux creates .bashrc", nil, "linux", ".bashrc"},
	} {
		t.Run("bash: "+tc.name, func(t *testing.T) {
			home := t.TempDir()
			for _, f := range tc.present {
				if err := os.WriteFile(filepath.Join(home, f), []byte("# x\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := bobShellRCPath("/bin/bash", home, tc.goos)
			if err != nil {
				t.Fatal(err)
			}
			if want := filepath.Join(home, tc.want); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}

	// An unknown shell is refused rather than guessed at, and the error must name the
	// way through — a refusal with no exit is just a dead end.
	for _, tc := range []struct{ name, shell string }{
		{"unknown shell", "/usr/bin/fish"},
		{"unset SHELL", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := bobShellRCPath(tc.shell, t.TempDir(), "linux")
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			if !strings.Contains(err.Error(), "--rc") {
				t.Errorf("error does not name --rc: %v", err)
			}
		})
	}
}

// enable on a file that does not exist yet, then again: the second run must report
// already-enabled and leave the file BYTE-identical. Comparing bytes rather than
// "contains the alias" is what catches a rewrite that re-appends the block or
// reorders the file.
func TestBobShellEnable_CreatesThenIdempotent(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".zshrc")

	var out, errb bytes.Buffer
	if code := bobShellEnable(rc, testAbctl, true, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb.String())
	}
	first := readFile(t, rc)
	if !strings.Contains(first, "alias bob='"+testAbctl+" exec -- \\bob'") {
		t.Errorf("alias not written:\n%s", first)
	}
	// The backslash is load-bearing: without it the alias calls itself.
	if !strings.Contains(first, `\bob`) {
		t.Errorf("alias body lost the backslash before bob:\n%s", first)
	}
	if !strings.Contains(out.String(), "source "+rc) {
		t.Errorf("did not say how to apply it to this shell:\n%s", out.String())
	}

	out.Reset()
	errb.Reset()
	if code := bobShellEnable(rc, testAbctl, true, &out, &errb); code != 0 {
		t.Fatalf("second enable: exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "Already enabled") {
		t.Errorf("second enable did not report already-enabled:\n%s", out.String())
	}
	if again := readFile(t, rc); again != first {
		t.Errorf("second enable changed the file:\nbefore:\n%s\nafter:\n%s", first, again)
	}
}

// Everything outside the markers is the user's, and must survive verbatim. This is
// the whole reason the block is marker-delimited rather than found by pattern.
func TestBobShellEnable_PreservesSurroundingContent(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".zshrc")
	original := "# my rc\nexport EDITOR=vim\n\nalias ll='ls -l'\n"
	if err := os.WriteFile(rc, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := bobShellEnable(rc, testAbctl, true, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb.String())
	}
	got := readFile(t, rc)
	if !strings.HasPrefix(got, original) {
		t.Errorf("original content did not survive at the head:\n%s", got)
	}

	// And a full round trip returns the file exactly as it was found — no accreted
	// blank line per cycle.
	out.Reset()
	if code := bobShellDisable(rc, true, &out, &errb); code != 0 {
		t.Fatalf("disable: exit = %d, want 0", code)
	}
	if back := readFile(t, rc); back != original {
		t.Errorf("enable/disable round trip changed the file:\nwant %q\ngot  %q", original, back)
	}
}

// The file's mode is the user's choice; an rc file is commonly 0644 and silently
// tightening it to 0600 is a change nobody asked for.
func TestBobShellEnable_PreservesMode(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".zshrc")
	if err := os.WriteFile(rc, []byte("# my rc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := bobShellEnable(rc, testAbctl, true, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	fi, err := os.Stat(rc)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("mode = %o, want 644", got)
	}
}

// The backup is written ONCE and never overwritten. An rc file is accreted by hand
// over years, so replacing the pristine copy with one this command already edited
// loses the only version the user actually wrote. Same rule, and same reason, as
// writeSettings — and install.sh's unconditional cp is the bug being avoided.
func TestBobShellEnable_BackupWrittenOnce(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".zshrc")
	original := "# pristine\n"
	if err := os.WriteFile(rc, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := bobShellEnable(rc, testAbctl, true, &out, &errb); code != 0 {
		t.Fatalf("enable: exit = %d, want 0", code)
	}
	if got := readFile(t, rc+".bak"); got != original {
		t.Fatalf("backup = %q, want %q", got, original)
	}

	// A second write: disable, then enable again. The backup must still hold the
	// pre-Cortex content, not an intermediate state of our own making.
	if code := bobShellDisable(rc, true, &out, &errb); code != 0 {
		t.Fatalf("disable: exit = %d, want 0", code)
	}
	if code := bobShellEnable(rc, testAbctl, true, &out, &errb); code != 0 {
		t.Fatalf("re-enable: exit = %d, want 0", code)
	}
	if got := readFile(t, rc+".bak"); got != original {
		t.Errorf("backup was overwritten: got %q, want the original %q", got, original)
	}
}

func TestBobShellDisable_NoBlockIsNoOp(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".zshrc")
	original := "# mine\nalias ll='ls -l'\n"
	if err := os.WriteFile(rc, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := bobShellDisable(rc, true, &out, &errb); code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "Nothing to do") {
		t.Errorf("stdout: %s", out.String())
	}
	if got := readFile(t, rc); got != original {
		t.Errorf("file changed: %q", got)
	}
	// No block means nothing was written, so there is nothing to back up either.
	if _, err := os.Stat(rc + ".bak"); err == nil {
		t.Error("a backup was made for a no-op disable")
	}
}

// A user's own `alias bob=...`, written before they ever ran this command, is not
// ours: neither reported as enabled nor removed by disable.
func TestBobShell_LeavesForeignAliasAlone(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".zshrc")
	original := "alias bob='/usr/local/bin/bob --fast'\n"
	if err := os.WriteFile(rc, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := bobShellStatus(rc, &out); code != 0 {
		t.Errorf("status exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "not enabled") {
		t.Errorf("a foreign alias was reported as ours:\n%s", out.String())
	}

	out.Reset()
	if code := bobShellDisable(rc, true, &out, &errb); code != 0 {
		t.Errorf("disable exit = %d, want 0", code)
	}
	if got := readFile(t, rc); got != original {
		t.Errorf("disable removed an alias it did not write: %q", got)
	}
}

func TestBobShellStatus(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		var out bytes.Buffer
		if code := bobShellStatus(filepath.Join(t.TempDir(), "nope"), &out); code != 0 {
			t.Errorf("exit = %d, want 0", code)
		}
		if !strings.Contains(out.String(), "not enabled") {
			t.Errorf("stdout: %s", out.String())
		}
	})

	t.Run("after enable", func(t *testing.T) {
		rc := filepath.Join(t.TempDir(), ".zshrc")
		var out, errb bytes.Buffer
		if code := bobShellEnable(rc, testAbctl, true, &out, &errb); code != 0 {
			t.Fatalf("enable: exit = %d", code)
		}
		out.Reset()
		if code := bobShellStatus(rc, &out); code != 0 {
			t.Errorf("exit = %d, want 0", code)
		}
		if !strings.Contains(out.String(), "enabled in "+rc) {
			t.Errorf("stdout: %s", out.String())
		}
	})

	// Markers with the alias line deleted by hand: not enabled, and the message says
	// why rather than reporting a bare "not enabled" that contradicts the file.
	t.Run("gutted block", func(t *testing.T) {
		rc := filepath.Join(t.TempDir(), ".zshrc")
		body := bobShellMarkerStart + "\n" + bobShellMarkerEnd + "\n"
		if err := os.WriteFile(rc, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if code := bobShellStatus(rc, &out); code != 0 {
			t.Errorf("exit = %d, want 0", code)
		}
		if !strings.Contains(out.String(), "not enabled") || !strings.Contains(out.String(), "no alias line") {
			t.Errorf("stdout: %s", out.String())
		}
	})
}

// An unterminated block — the end marker deleted by hand — must be replaced, not
// appended below, or every enable adds another one.
func TestBobShellEnable_RepairsUnterminatedBlock(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".zshrc")
	if err := os.WriteFile(rc, []byte("# mine\n"+bobShellMarkerStart+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := bobShellEnable(rc, testAbctl, true, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb.String())
	}
	got := readFile(t, rc)
	if n := strings.Count(got, bobShellMarkerStart); n != 1 {
		t.Errorf("start marker appears %d times, want 1:\n%s", n, got)
	}
	if n := strings.Count(got, "alias bob="); n != 1 {
		t.Errorf("alias appears %d times, want 1:\n%s", n, got)
	}
}

// Declining is a normal outcome, distinct from a failure: exit 3, nothing written.
// The prompt is reached only when --yes is absent, and a test process has no
// controlling terminal for confirm to open, so it declines on its own.
func TestBobShellEnable_DeclineWritesNothing(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".zshrc")
	original := "# mine\n"
	if err := os.WriteFile(rc, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := bobShellEnable(rc, testAbctl, false, &out, &errb); code != exitDeclined {
		t.Errorf("exit = %d, want %d", code, exitDeclined)
	}
	if !strings.Contains(out.String(), "Not changed.") {
		t.Errorf("stdout: %s", out.String())
	}
	if got := readFile(t, rc); got != original {
		t.Errorf("file was written despite declining: %q", got)
	}
}

// Same three spellings TestClaudeCodeHelp_PrintsUsageOnStdout pins, and for the
// same reason: -h is what a habit produces, `help` is what someone copies from
// another tool, and an explicit help must be pipeable.
func TestBobShellHelp_PrintsUsageOnStdout(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		t.Run(arg, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := runBobShell([]string{arg}, &out, &errb); code != 0 {
				t.Errorf("exit = %d, want 0", code)
			}
			if !strings.Contains(out.String(), "abctl configure bobshell —") {
				t.Errorf("usage not on stdout:\n%s", out.String())
			}
			if strings.Contains(out.String()+errb.String(), "unknown bobshell action") {
				t.Errorf("help was read as an action name:\n%s\n%s", out.String(), errb.String())
			}
			if errb.Len() != 0 {
				t.Errorf("stderr not empty: %q", errb.String())
			}
		})
	}
}

func TestBobShellUsageErrors(t *testing.T) {
	t.Run("no args", func(t *testing.T) {
		var out, errb bytes.Buffer
		if code := runBobShell(nil, &out, &errb); code != 2 {
			t.Errorf("exit = %d, want 2", code)
		}
		if !strings.Contains(errb.String(), "abctl configure bobshell —") {
			t.Errorf("usage not on stderr:\n%s", errb.String())
		}
		if out.Len() != 0 {
			t.Errorf("stdout not empty: %q", out.String())
		}
	})

	t.Run("unknown action", func(t *testing.T) {
		var out, errb bytes.Buffer
		if code := runBobShell([]string{"enabel", "--rc", filepath.Join(t.TempDir(), "rc")}, &out, &errb); code != 2 {
			t.Errorf("exit = %d, want 2", code)
		}
		if !strings.Contains(errb.String(), "unknown bobshell action") {
			t.Errorf("stderr: %q", errb.String())
		}
	})
}

// `configure bobshell` must be a spelling of the same logic, not a
// reimplementation. Asserted as equality against the direct call rather than
// against a literal, so it keeps passing when the wording changes and fails only
// if the two actually diverge — the same shape as
// TestConfigure_ClaudeCodeReachesTheSameLogic.
//
// status is the action to test: it neither prompts nor writes.
func TestConfigure_BobShellReachesTheSameLogic(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".zshrc")
	if err := os.WriteFile(rc, []byte("# mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var viaConfigureOut, viaConfigureErr bytes.Buffer
	configureCode := runConfigure([]string{"bobshell", "status", "--rc", rc}, &viaConfigureOut, &viaConfigureErr)

	var directOut, directErr bytes.Buffer
	directCode := runBobShell([]string{"status", "--rc", rc}, &directOut, &directErr)

	if configureCode != directCode {
		t.Errorf("exit codes differ: configure = %d, bob = %d", configureCode, directCode)
	}
	if viaConfigureOut.String() != directOut.String() {
		t.Errorf("stdout differs:\nconfigure:\n%s\nbob:\n%s", viaConfigureOut.String(), directOut.String())
	}
	if viaConfigureErr.Len() != 0 || directErr.Len() != 0 {
		t.Errorf("stderr not empty: configure %q, bob %q", viaConfigureErr.String(), directErr.String())
	}
}

// The rc file is only reached through --rc here, but enable without it derives the
// path from $SHELL — and must not fall back to editing some default when the shell
// is one it does not know.
func TestBobShell_UnknownShellWithoutRCIsRefused(t *testing.T) {
	t.Setenv("SHELL", "/usr/bin/fish")
	var out, errb bytes.Buffer
	if code := runBobShell([]string{"enable", "--yes"}, &out, &errb); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "--rc") {
		t.Errorf("stderr does not name the way through: %q", errb.String())
	}
}
