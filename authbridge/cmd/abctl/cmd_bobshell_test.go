package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
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
	// A LITERAL, not bobShellAliasLine(testAbctl): every other assertion in this file
	// compares against that function, so a change to the rendered line — dropping the
	// backslash, say — moves both sides together and nothing notices. This spells the
	// expected line out independently, which is what makes the backslash covered.
	//
	// The backslash is the load-bearing character: without it `bob` inside the alias
	// body re-expands to the alias and it calls itself.
	if want := `alias bob='` + testAbctl + ` exec -- \bob'`; !strings.Contains(first, want) {
		t.Errorf("alias not written as %q:\n%s", want, first)
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
//
// A table over the shapes a real rc file's tail takes, because the invariant is
// unconditional identity and the shape of the last line is what used to break it:
// while enable appended a separator blank and disable reclaimed one, a file that
// already ended blank had the user's own blank line eaten on disable. The single
// fixture ending in a non-blank line could not reach that.
func TestBobShellEnable_PreservesSurroundingContent(t *testing.T) {
	// wantBack differs from original only where writeRC's documented exception
	// applies: a file with no trailing newline gains one, because an unterminated end
	// marker is unfindable by disable and swallows the next append.
	for _, tc := range []struct{ name, original, wantBack string }{
		{"ends with content", "# my rc\nexport EDITOR=vim\n\nalias ll='ls -l'\n", "# my rc\nexport EDITOR=vim\n\nalias ll='ls -l'\n"},
		{"ends with a blank line", "# mine\n\n", "# mine\n\n"},
		{"exactly one newline", "\n", "\n"},
		{"no trailing newline", "# mine", "# mine\n"},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := filepath.Join(t.TempDir(), ".zshrc")
			if tc.original != "" {
				if err := os.WriteFile(rc, []byte(tc.original), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			var out, errb bytes.Buffer
			if code := bobShellEnable(rc, testAbctl, true, &out, &errb); code != 0 {
				t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb.String())
			}
			got := readFile(t, rc)
			if !strings.HasPrefix(got, tc.original) {
				t.Errorf("original content did not survive at the head:\n%s", got)
			}
			if !strings.Contains(got, bobShellAliasLine(testAbctl)) {
				t.Errorf("the alias is missing:\n%s", got)
			}

			// And a full round trip returns the file exactly as it was found.
			out.Reset()
			if code := bobShellDisable(rc, true, &out, &errb); code != 0 {
				t.Fatalf("disable: exit = %d, want 0", code)
			}
			if back := readFile(t, rc); back != tc.wantBack {
				t.Errorf("enable/disable round trip changed the file:\nwant %q\ngot  %q", tc.wantBack, back)
			}
		})
	}
}

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

	// A second write: disable. Asserted HERE, before the third write, because this is
	// the only moment an unconditional backup would be visibly wrong — the file now
	// holds the block, so a clobbered backup would hold it too. Deferring the check
	// to after a re-enable made it unfailable: the round trip restores the original
	// byte-for-byte, so an unconditional copy would write the original content back
	// and the assertion would pass over the bug it was named for.
	if code := bobShellDisable(rc, true, &out, &errb); code != 0 {
		t.Fatalf("disable: exit = %d, want 0", code)
	}
	if got := readFile(t, rc+".bak"); got != original {
		t.Fatalf("the second write overwrote the backup: got %q, want %q", got, original)
	}

	// And still intact after a third.
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
		if got := out.String(); !strings.HasPrefix(got, "not enabled") {
			t.Errorf("stdout = %q, want it to start with %q", got, "not enabled")
		}
	})

	// The alias names the RUNNING binary, so this reaches the success branch. With a
	// literal path it never could: status compares the alias against abctlPath(), which
	// under `go test` is the test binary, so every run took the "different abctl" branch
	// and the happy path was asserted by nothing.
	t.Run("after enable", func(t *testing.T) {
		self, err := abctlPath()
		if err != nil {
			t.Fatalf("abctlPath: %v", err)
		}
		rc := filepath.Join(t.TempDir(), ".zshrc")
		var out, errb bytes.Buffer
		if code := bobShellEnable(rc, self, true, &out, &errb); code != 0 {
			t.Fatalf("enable: exit = %d", code)
		}
		out.Reset()
		if code := bobShellStatus(rc, &out); code != 0 {
			t.Errorf("exit = %d, want 0", code)
		}
		// Anchored: "enabled in <rc>" is a substring of "not enabled in <rc>", so
		// Contains alone cannot tell the two verdicts apart and passed for both.
		if got := lastLine(out.String()); got != "enabled in "+rc {
			t.Errorf("last line = %q, want %q\nfull:\n%s", got, "enabled in "+rc, out.String())
		}
		if strings.Contains(out.String(), "different abctl") {
			t.Errorf("status reported a path mismatch against its own binary:\n%s", out.String())
		}
	})

	// The mismatch branch, which carries the actionable "Re-run" advice: an alias left
	// behind by an abctl that has since moved or been reinstalled elsewhere.
	t.Run("alias names a different abctl", func(t *testing.T) {
		rc := filepath.Join(t.TempDir(), ".zshrc")
		var out, errb bytes.Buffer
		if code := bobShellEnable(rc, "/somewhere/else/abctl", true, &out, &errb); code != 0 {
			t.Fatalf("enable: exit = %d", code)
		}
		out.Reset()
		if code := bobShellStatus(rc, &out); code != 0 {
			t.Errorf("exit = %d, want 0", code)
		}
		if !strings.Contains(out.String(), "different abctl") {
			t.Errorf("status did not flag the stale path:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "Re-run") {
			t.Errorf("status did not say how to fix it:\n%s", out.String())
		}
	})

	// A truncated alias must NOT read as enabled. This is what the equality check in
	// bobShellStatus buys over a Contains against the whole rendered block: the broken
	// line is a substring of the correct one.
	t.Run("truncated alias is not enabled", func(t *testing.T) {
		self, err := abctlPath()
		if err != nil {
			t.Fatalf("abctlPath: %v", err)
		}
		rc := filepath.Join(t.TempDir(), ".zshrc")
		truncated := strings.TrimSuffix(bobShellAliasLine(self), "b'")
		body := bobShellMarkerStart + "\n" + truncated + "\n" + bobShellMarkerEnd + "\n"
		if err := os.WriteFile(rc, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if code := bobShellStatus(rc, &out); code != 0 {
			t.Errorf("exit = %d, want 0", code)
		}
		if lastLine(out.String()) == "enabled in "+rc {
			t.Errorf("a truncated alias reported as fully enabled:\n%s", out.String())
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

// Declining writes nothing and exits with the declined code.
//
// confirmFn is swapped rather than relying on confirm's own behaviour: confirm opens
// /dev/tty, which resolves to the developer's terminal whenever `go test` runs from
// an interactive shell — so the version of this test that called through blocked on
// a read there, and passed only where no tty exists. Driving the seam asserts the
// decline path itself, identically in a terminal and in CI.
func TestBobShellEnable_DeclineWritesNothing(t *testing.T) {
	restore := confirmFn
	t.Cleanup(func() { confirmFn = restore })
	asked := false
	confirmFn = func(io.Writer) bool { asked = true; return false }

	rc := filepath.Join(t.TempDir(), ".zshrc")
	original := "# mine\n"
	if err := os.WriteFile(rc, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := bobShellEnable(rc, testAbctl, false, &out, &errb); code != exitDeclined {
		t.Errorf("exit = %d, want %d", code, exitDeclined)
	}
	if !asked {
		t.Error("enable did not ask for confirmation")
	}
	if !strings.Contains(out.String(), "Not changed.") {
		t.Errorf("stdout: %s", out.String())
	}
	if got := readFile(t, rc); got != original {
		t.Errorf("file was written despite declining: %q", got)
	}
}

// And --yes must not ask at all: the flag is the consent.
func TestBobShellEnable_YesDoesNotPrompt(t *testing.T) {
	restore := confirmFn
	t.Cleanup(func() { confirmFn = restore })
	confirmFn = func(io.Writer) bool {
		t.Error("enable prompted despite --yes")
		return false
	}

	rc := filepath.Join(t.TempDir(), ".zshrc")
	var out, errb bytes.Buffer
	if code := bobShellEnable(rc, testAbctl, true, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb.String())
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

// lastLine is the final non-empty line, for anchoring an assertion on a verdict that
// is a substring of its own negation ("enabled in X" inside "not enabled in X").
func lastLine(s string) string {
	fields := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return fields[len(fields)-1]
}

// A path containing a single quote would end the alias body's quoting mid-word and
// leave a malformed line in the user's rc file — a failure they would see only as a
// broken shell, not as an error. Deleting this guard left the suite green.
func TestValidateAliasPath(t *testing.T) {
	if err := validateAliasPath("/opt/abctl/abctl"); err != nil {
		t.Errorf("ordinary path rejected: %v", err)
	}
	if err := validateAliasPath("/home/o'brien/bin/abctl"); err == nil {
		t.Error("a path containing a single quote was accepted")
	} else if !strings.Contains(err.Error(), "single quote") {
		t.Errorf("error does not name the problem: %v", err)
	}
}

// TestBobShellEnable_TerminatesTheEndMarker pins the newline writeRC adds to a file
// that had none. Two things break without it, and neither is cosmetic: the next line
// appended to the rc file fuses onto the end marker and is commented out by it, and
// disable can no longer match the marker — so it reports success, exits 0, and
// leaves the live alias in the file.
func TestBobShellEnable_TerminatesTheEndMarker(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".zshrc")
	if err := os.WriteFile(rc, []byte("# my rc\nMY_OWN_SETTING=kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := bobShellEnable(rc, testAbctl, true, &out, &errb); code != 0 {
		t.Fatalf("enable: exit = %d, want 0 (stderr: %s)", code, errb.String())
	}
	if got := readFile(t, rc); !strings.HasSuffix(got, bobShellMarkerEnd+"\n") {
		t.Fatalf("the end marker is not a complete line:\n%q", got)
	}

	// What that costs when it is missing: a later append lands on the marker's line.
	f, err := os.OpenFile(rc, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("export APPENDED_LATER=1\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	for _, want := range []string{bobShellMarkerEnd + "\n", "\nexport APPENDED_LATER=1\n"} {
		if got := readFile(t, rc); !strings.Contains(got, want) {
			t.Errorf("appended line fused onto the marker; want %q in:\n%s", want, got)
		}
	}

	// And disable still finds the block, rather than exiting 0 with the alias left in.
	out.Reset()
	if code := bobShellDisable(rc, true, &out, &errb); code != 0 {
		t.Fatalf("disable: exit = %d, want 0", code)
	}
	if got := readFile(t, rc); strings.Contains(got, "alias bob") {
		t.Errorf("disable exited 0 but left the alias behind:\n%s", got)
	}
}

// TestBobShellEnable_WritesThroughASymlink pins that a symlinked rc file — a link
// into a dotfiles repo, which is how rc files usually look — is followed rather than
// replaced. os.Rename over the link turns it into a regular file: the alias lands
// somewhere the repo does not track, the repo's copy never gets it, and later
// dotfile edits stop reaching the shell.
func TestBobShellEnable_WritesThroughASymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "zshrc")
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("# real rc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ".zshrc")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	var out, errb bytes.Buffer
	if code := bobShellEnable(link, testAbctl, true, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb.String())
	}

	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced by a regular file")
	}
	if got := readFile(t, real); !strings.Contains(got, bobShellAliasLine(testAbctl)) {
		t.Errorf("the alias did not reach the link target:\n%s", got)
	}
	// The backup belongs beside the real file too, not beside the link.
	if _, err := os.Stat(real + ".bak"); err != nil {
		t.Errorf("no backup beside the link target: %v", err)
	}
}

// TestBobShellEnable_NamesTheBackupItActuallyWrites pins the MUST-FIX that the
// symlink fix itself introduced: writeRC follows the link, so the backup lands beside
// the target, and the message named the link. Being told where the only pristine copy
// of a hand-accreted rc file lives and finding nothing there is the whole cost.
func TestBobShellEnable_NamesTheBackupItActuallyWrites(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "zshrc")
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("# real rc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ".zshrc")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	var out, errb bytes.Buffer
	if code := bobShellEnable(link, testAbctl, true, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb.String())
	}

	// Whatever path the message names as the backup must be a file that exists.
	named := ""
	for _, line := range strings.Split(out.String(), "\n") {
		if _, after, ok := strings.Cut(line, "a copy is kept as "); ok {
			named = strings.TrimSpace(after)
		}
	}
	if named == "" {
		t.Fatalf("no backup path in the output:\n%s", out.String())
	}
	if _, err := os.Stat(named); err != nil {
		t.Errorf("the message names a backup that does not exist (%s): %v", named, err)
	}
	if strings.Contains(out.String(), "is a link to") == false {
		t.Error("the output does not mention that the rc file is a link")
	}
}

// TestBobShellEnable_PlainFileMessageIsNotResolved is the other half: EvalSymlinks
// resolves PARENT directories too, so on macOS a plain /tmp/rc comes back
// /private/tmp/rc. Reporting that as "a link" — or naming the backup by it — is true
// and unhelpful, so the notice is gated on the rc file itself being a link.
func TestBobShellEnable_PlainFileMessageIsNotResolved(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".zshrc")
	if err := os.WriteFile(rc, []byte("# mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := bobShellEnable(rc, testAbctl, true, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.Contains(out.String(), "is a link to") {
		t.Errorf("a plain file was announced as a link:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "a copy is kept as "+rc+".bak") {
		t.Errorf("the backup is not named by the path the user gave:\n%s", out.String())
	}
}

// TestBobShellEnable_DanglingSymlinkIsFollowed covers the state a dotfiles repo is in
// before it is cloned, or mid stow/chezmoi setup. EvalSymlinks fails on a broken link,
// so without a Readlink fallback the path stays the link and os.Rename replaces it
// with a regular file — the exact clobber the symlink fix exists to prevent, reached
// by a different route.
func TestBobShellEnable_DanglingSymlinkIsFollowed(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "dotfiles"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ".zshrc")
	if err := os.Symlink(filepath.Join("dotfiles", "zshrc"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	var out, errb bytes.Buffer
	if code := bobShellEnable(link, testAbctl, true, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb.String())
	}
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("a dangling symlink was replaced by a regular file")
	}
	target := filepath.Join(dir, "dotfiles", "zshrc")
	if got := readFile(t, target); !strings.Contains(got, bobShellAliasLine(testAbctl)) {
		t.Errorf("the alias did not reach the dangling link's target:\n%s", got)
	}
}

// TestBobShellEnable_CreatesWorldReadable — the comment about not silently tightening
// an rc file's mode applies to the file this command creates, too. Every shell's own
// rc file is world-readable; 0600 was an unexplained departure.
func TestBobShellEnable_CreatesWorldReadable(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".zshrc")
	var out, errb bytes.Buffer
	if code := bobShellEnable(rc, testAbctl, true, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	fi, err := os.Stat(rc)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("a from-scratch rc file has mode %o, want 644", got)
	}
}

// TestBobShellBlock_SurvivesADamagedEndMarker pins the worse half of a hand-damaged
// fence. With the end marker mangled the block went unfound, so status reported "not
// enabled" while the shell had a LIVE alias, and disable removed the comment, exited
// 0, and left the alias in place.
func TestBobShellBlock_SurvivesADamagedEndMarker(t *testing.T) {
	for _, tc := range []struct{ name, damage string }{
		{"mangled", "# <<< cortex abctl MANGLED"},
		{"deleted", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := filepath.Join(t.TempDir(), ".zshrc")
			if err := os.WriteFile(rc, []byte("# mine\nexport AFTER=1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			// The running binary, not testAbctl: status appends a "different abctl"
			// hint after its verdict otherwise, and that hint is then the last line.
			self, err := abctlPath()
			if err != nil {
				t.Fatalf("abctlPath: %v", err)
			}
			var out, errb bytes.Buffer
			if code := bobShellEnable(rc, self, true, &out, &errb); code != 0 {
				t.Fatalf("enable: exit = %d, want 0", code)
			}
			// Damage the far side of the fence, the way a hand-edit would.
			lines := strings.Split(strings.TrimSuffix(readFile(t, rc), "\n"), "\n")
			kept := make([]string, 0, len(lines))
			for _, l := range lines {
				if strings.HasPrefix(l, bobShellMarkerEnd) {
					if tc.damage != "" {
						kept = append(kept, tc.damage)
					}
					continue
				}
				kept = append(kept, l)
			}
			if err := os.WriteFile(rc, []byte(strings.Join(kept, "\n")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			// status must not claim "not enabled" while the alias is live.
			out.Reset()
			if code := bobShellStatus(rc, &out); code != 0 {
				t.Fatalf("status: exit = %d, want 0", code)
			}
			if got := lastLine(out.String()); !strings.HasPrefix(got, "enabled in ") {
				t.Errorf("status = %q, want it to report the live alias as enabled", got)
			}

			// And disable must actually remove it.
			out.Reset()
			if code := bobShellDisable(rc, true, &out, &errb); code != 0 {
				t.Fatalf("disable: exit = %d, want 0", code)
			}
			got := readFile(t, rc)
			if strings.Contains(got, "alias bob") {
				t.Errorf("disable exited 0 but left the live alias:\n%s", got)
			}
			// A marker we WROTE must be gone. A hand-MANGLED one is no longer a line
			// this command emits, so ownership by exact match leaves it — inert text,
			// and the alternative is the prefix matching that let a recovered block
			// claim a user's own alias (round-4 MUST FIX 2). Round 4's suggestion 4
			// asked about exactly this residue; the answer then was "already removed",
			// which was true only as a side effect of that unsafe prefix.
			if strings.Contains(got, bobShellMarkerStart) || strings.Contains(got, bobShellMarkerEnd) {
				t.Errorf("disable left one of our own markers behind:\n%s", got)
			}
			if tc.damage == "" && strings.Contains(got, "cortex abctl") {
				t.Errorf("disable left a marker behind:\n%s", got)
			}
			if !strings.Contains(got, "export AFTER=1") {
				t.Errorf("disable ate the user's line below the block:\n%s", got)
			}
		})
	}
}

// TestBobShellDisable_LeavesAUserOwnedAliasAlone is the constraint that makes the
// damaged-fence fix safe, and it is the one an over-broad fix breaks: an `alias bob`
// the user wrote themselves, outside any marker, is neither reported as ours nor
// removed as ours. A first attempt at the fix above scanned the whole file for
// `alias bob=` and deleted exactly this line.
func TestBobShellDisable_LeavesAUserOwnedAliasAlone(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".zshrc")
	original := "# mine\nalias bob='/usr/local/bin/bob --flag'\n"
	if err := os.WriteFile(rc, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := bobShellStatus(rc, &out); code != 0 {
		t.Fatalf("status: exit = %d, want 0", code)
	}
	if got := lastLine(out.String()); !strings.HasPrefix(got, "not enabled in ") {
		t.Errorf("status = %q, want not-enabled for an alias we did not write", got)
	}
	out.Reset()
	if code := bobShellDisable(rc, true, &out, &errb); code != 0 {
		t.Fatalf("disable: exit = %d, want 0", code)
	}
	if got := readFile(t, rc); got != original {
		t.Errorf("disable touched a user-owned alias:\nwant %q\ngot  %q", original, got)
	}
}

// TestBobShellUsageErrorsExitTwo — --help documents 2 as the usage code, and a --rc
// that cannot be an rc file is a usage error. It used to surface the raw errno at
// exit 1 ("read /x/adir: is a directory"), contradicting the documented code and
// reading like an internal failure rather than a correctable argument. --yes on
// status is a usage error for a different reason: status never prompts, so accepting
// the flag would accept one that does nothing.
func TestBobShellUsageErrorsExitTwo(t *testing.T) {
	dir := t.TempDir()
	adir := filepath.Join(dir, "adir")
	if err := os.MkdirAll(adir, 0o755); err != nil {
		t.Fatal(err)
	}
	rc := filepath.Join(dir, ".zshrc")
	if err := os.WriteFile(rc, []byte("# mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"rc is a directory", []string{"enable", "--rc", adir, "--yes"}, "is a directory"},
		{"disable rc is a directory", []string{"disable", "--rc", adir, "--yes"}, "is a directory"},
		{"status rc is a directory", []string{"status", "--rc", adir}, "is a directory"},
		{"yes is not a status flag", []string{"status", "--rc", rc, "--yes"}, "not defined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := runBobShell(tc.args, &out, &errb); code != 2 {
				t.Errorf("exit = %d, want 2 (stderr: %s)", code, errb.String())
			}
			if !strings.Contains(errb.String(), tc.want) {
				t.Errorf("stderr = %q, want it to mention %q", errb.String(), tc.want)
			}
		})
	}
}

// TestWriteRC_CleansUpItsTempFileOnFailure — after the symlink resolve the .tmp sits
// inside the user's dotfiles repo, where an untracked file left by a failed write is
// something they may well commit by accident. A directory at the destination makes
// the rename fail without making the write fail.
func TestWriteRC_CleansUpItsTempFileOnFailure(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, ".zshrc")
	// A non-empty directory where the file should go: os.WriteFile of the .tmp
	// succeeds, os.Rename onto it does not.
	if err := os.MkdirAll(filepath.Join(dest, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeRC(dest, []string{"# x"}, true); err == nil {
		t.Fatal("writeRC succeeded onto a non-empty directory, want an error")
	}
	if _, err := os.Stat(dest + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("the temp file outlived the failed write: %v", err)
	}
}

// TestBobShellEnable_FollowsASymlinkChain covers head -> mid -> tail with tail absent,
// which is what stow and chezmoi produce mid-setup — a link to a link. A single
// Readlink hop stopped at mid, so the write landed there: mid lost its symlink bit to
// a regular file and the real target was never created. Asserting on mid's mode is the
// point; asserting only that tail exists would pass for a version that clobbers mid
// and then also happens to write tail.
func TestBobShellEnable_FollowsASymlinkChain(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "chain"), 0o755); err != nil {
		t.Fatal(err)
	}
	mid := filepath.Join(dir, "chain", "mid")
	head := filepath.Join(dir, "head")
	if err := os.Symlink("tail", mid); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join("chain", "mid"), head); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	var out, errb bytes.Buffer
	if code := bobShellEnable(head, testAbctl, true, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb.String())
	}
	for _, link := range []string{head, mid} {
		fi, err := os.Lstat(link)
		if err != nil {
			t.Fatalf("%s: %v", link, err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s: an intermediate symlink was replaced by a regular file", link)
		}
	}
	tail := filepath.Join(dir, "chain", "tail")
	if got := readFile(t, tail); !strings.Contains(got, bobShellAliasLine(testAbctl)) {
		t.Errorf("the alias did not reach the end of the chain:\n%s", got)
	}
	// The message must name the file actually written, not the first hop.
	if !strings.Contains(out.String(), tail) {
		t.Errorf("stdout never names the real target %s:\n%s", tail, out.String())
	}
}

// TestBobShellEnable_RefusesASymlinkCycle — a cycle has no resolution, so following it
// is not an option and neither is guessing. Every hop is a live symlink; handing any of
// them to the writer would clobber it. Refusing leaves both intact.
func TestBobShellEnable_RefusesASymlinkCycle(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.Symlink(b, a); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	var out, errb bytes.Buffer
	if code := bobShellEnable(a, testAbctl, true, &out, &errb); code == 0 {
		t.Errorf("exit = 0, want non-zero on a symlink cycle (stdout: %s)", out.String())
	}
	for _, link := range []string{a, b} {
		fi, err := os.Lstat(link)
		if err != nil {
			t.Fatalf("%s: %v", link, err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s: a symlink in a cycle was replaced by a regular file", link)
		}
	}
}

// TestBobShellBlock_KeepsAUserAliasUnderADamagedMarker is the regression test the
// earlier one could not be. TestBobShellDisable_LeavesAUserOwnedAliasAlone has no START
// marker in its fixture, so the lone-START recovery branch never runs and the bug lived
// there untouched: the walk matched the bare prefix "alias bob=", claimed the user's own
// alias on the adjacent line, and disable deleted it.
func TestBobShellBlock_KeepsAUserAliasUnderADamagedMarker(t *testing.T) {
	theirs := "alias bob='/usr/local/bin/bob --fast'"
	for _, tc := range []struct{ name, damaged string }{
		{"end marker deleted", ""},
		{"end marker mangled", "# <<< cortex abctl MANGLED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := []string{"# mine", bobShellMarkerStart, bobShellAliasLine(testAbctl)}
			if tc.damaged != "" {
				lines = append(lines, tc.damaged)
			}
			lines = append(lines, theirs)

			if !bobShellOwnsAnything(lines) {
				t.Fatal("nothing recognized as ours; the recovery path did not run")
			}
			// The OWNED SET, not findBobShellBlock's span. The span is the hull of the
			// owned lines and is deliberately wider when a hand-edit splits the block —
			// it exists to quote the damaged region back to the user, and no writer keys
			// off it. Ownership is what decides what gets deleted, so ownership is what
			// this asserts.
			for _, i := range ownedLines(lines) {
				if strings.TrimSpace(lines[i]) == theirs {
					t.Fatalf("claimed the user's own alias as ours:\n%v", lines)
				}
			}
			updated, removed := removeBobShellBlock(lines)
			if !removed {
				t.Fatal("removeBobShellBlock found nothing to remove")
			}
			if !slices.Contains(updated, theirs) {
				t.Errorf("disable deleted the user's own alias:\n%v", updated)
			}
			if slices.Contains(updated, bobShellAliasLine(testAbctl)) {
				t.Error("our own alias line survived removal")
			}
		})
	}
}

// TestBobShellEnable_DoesNotPromiseABackupItWillNotWrite — writeRC backs up only what it
// could read, so on a file that does not exist there is no copy to keep. Promising one
// in the consent prompt offers a rollback artifact that will not be there, and the
// from-scratch path is the COMMON first run: bobShellRCPath returns ~/.zshrc for zsh
// whether or not the file exists.
func TestBobShellEnable_DoesNotPromiseABackupItWillNotWrite(t *testing.T) {
	t.Run("file does not exist", func(t *testing.T) {
		rc := filepath.Join(t.TempDir(), ".zshrc")
		var out, errb bytes.Buffer
		if code := bobShellEnable(rc, testAbctl, true, &out, &errb); code != 0 {
			t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb.String())
		}
		if strings.Contains(out.String(), ".bak") {
			t.Errorf("promised a .bak for a file that did not exist:\n%s", out.String())
		}
		if _, err := os.Stat(rc + ".bak"); err == nil {
			t.Error("a .bak was written for a file that did not exist")
		}
	})

	t.Run("file exists", func(t *testing.T) {
		rc := filepath.Join(t.TempDir(), ".zshrc")
		if err := os.WriteFile(rc, []byte("# mine\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		var out, errb bytes.Buffer
		if code := bobShellEnable(rc, testAbctl, true, &out, &errb); code != 0 {
			t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb.String())
		}
		// The promise and the artifact have to agree in this direction too, or the
		// test above would pass for a version that never mentions a backup at all.
		if !strings.Contains(out.String(), rc+".bak") {
			t.Errorf("stdout does not name the backup it wrote:\n%s", out.String())
		}
		if _, err := os.Stat(rc + ".bak"); err != nil {
			t.Errorf("promised %s.bak but: %v", rc, err)
		}
	})
}

// TestWriteRC_DoesNotConjureParentDirectories — MkdirAll built a whole tree for a
// dangling link whose target lived in a not-yet-cloned dotfiles repo, at a location no
// message named. An rc file's directory existing is the normal case.
func TestWriteRC_DoesNotConjureParentDirectories(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "never", "asked", "for")
	err := writeRC(filepath.Join(missing, "zshrc"), []string{"# x"}, true)
	if err == nil {
		t.Fatal("writeRC = nil, want an error for a missing parent directory")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error does not name the missing directory %s: %v", missing, err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "never")); serr == nil {
		t.Error("writeRC created a directory tree the user did not ask for")
	}
}

// TestIsOurAliasLine pins the provenance test directly, since it is the thing standing
// between a recovered block and a user's own alias.
func TestIsOurAliasLine(t *testing.T) {
	for _, tc := range []struct {
		line string
		want bool
	}{
		{bobShellAliasLine(testAbctl), true},
		{bobShellAliasLine("/usr/local/bin/abctl"), true},
		{"  " + bobShellAliasLine(testAbctl) + "  ", true},
		{"alias bob='/usr/local/bin/bob --fast'", false},
		{"alias bob='bob'", false},
		{"alias bob=", false},
		{"alias bobcat='/opt/abctl/abctl exec -- \\bob'", false},
		{"alias bob='/opt/abctl/abctl exec -- \\bob' # mine", false},
		{"", false},
	} {
		if got := isOurAliasLine(tc.line); got != tc.want {
			t.Errorf("isOurAliasLine(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

// TestBobShellEnable_WritesTheFileItNamed drives the TOCTOU window the round-4 review
// names: enable used to call resolveRC once for the message and again for the write,
// straddling the prompt. Repointing the link from inside the confirm callback is exactly
// the interleaving that made the two disagree — the message named one file and the write
// landed on another. Resolving once makes them the same value, so they cannot.
func TestBobShellEnable_WritesTheFileItNamed(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	for _, f := range []string{first, second} {
		if err := os.WriteFile(f, []byte("# "+filepath.Base(f)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, ".zshrc")
	if err := os.Symlink(first, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	restore := confirmFn
	t.Cleanup(func() { confirmFn = restore })
	confirmFn = func(io.Writer) bool {
		// The user is reading the prompt; stow/chezmoi/a git checkout moves the link.
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(second, link); err != nil {
			t.Fatal(err)
		}
		return true
	}

	var out, errb bytes.Buffer
	if code := bobShellEnable(link, testAbctl, false, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb.String())
	}
	// Whichever file the message named must be the one that changed, and the other must
	// be untouched. Asserting "first" specifically is the stronger claim: it is what
	// resolving before the prompt commits to.
	if !strings.Contains(out.String(), first) {
		t.Fatalf("stdout does not name %s:\n%s", first, out.String())
	}
	if got := readFile(t, first); !strings.Contains(got, bobShellAliasLine(testAbctl)) {
		t.Errorf("the named file %s was not the one written:\n%s", first, got)
	}
	if got := readFile(t, second); strings.Contains(got, bobShellAliasLine(testAbctl)) {
		t.Errorf("the write landed on %s, which no message mentioned:\n%s", second, got)
	}
}

// theirAlias is a user's own bob alias — never ours, whatever it sits next to.
const theirAlias = "alias bob='/usr/local/bin/bob --fast'"

// TestBobShellOwnership_HoldsAcrossDamagedFiles is the test the three previous rounds
// of this file needed and did not have. Each round, a reviewer hand-edited the rc file
// into a shape the recovery code had not anticipated — a blank line inside a damaged
// block, a lone END marker, a second START — and each time the result was the same
// failure: enable left TWO live aliases, or disable left one behind while reporting
// success. The bug was never really the particular shape; it was that ownership was
// expressed as a contiguous span, and a damaged file's owned lines are a set.
//
// So this asserts the invariant directly, over every shape found so far plus the
// healthy one, rather than testing the shapes one at a time:
//
//   - after enable: exactly one line the shell would take as our alias;
//   - after disable: none, and no marker of ours;
//   - always: a user's own `alias bob=...` is still there, byte for byte.
func TestBobShellOwnership_HoldsAcrossDamagedFiles(t *testing.T) {
	ours := bobShellAliasLine(testAbctl)
	for _, tc := range []struct {
		name  string
		lines []string
	}{
		{"healthy block", []string{"# a", bobShellMarkerStart, ours, bobShellMarkerEnd, "# b"}},
		{"no block at all", []string{"# a", "export X=1"}},
		{"blank line inside a damaged block", []string{bobShellMarkerStart, "", ours, "# tail"}},
		{"comment inside a damaged block", []string{bobShellMarkerStart, "# added by me", ours, "# tail"}},
		{"lone end marker", []string{"# mine", bobShellMarkerEnd}},
		{"lone end marker with our alias above", []string{"# mine", ours, bobShellMarkerEnd}},
		{"two start markers no end", []string{bobShellMarkerStart, ours, bobShellMarkerStart, ours}},
		{"gutted block", []string{bobShellMarkerStart, bobShellMarkerEnd}},
		{"end before start", []string{bobShellMarkerEnd, "# x", bobShellMarkerStart, ours}},
		{"their alias under a damaged marker", []string{bobShellMarkerStart, ours, theirAlias}},
		{"their alias alone", []string{theirAlias}},
		{"their alias inside a healthy block", []string{bobShellMarkerStart, ours, bobShellMarkerEnd, theirAlias}},
		// Found by TestBobShellOwnership_HoldsOverGeneratedDamage, not by hand: a user
		// who pastes their own alias BETWEEN the markers. Being fenced does not make a
		// line ours, so enable must not replace it and disable must not remove it.
		{"their alias inside the fence", []string{bobShellMarkerStart, theirAlias, bobShellMarkerEnd}},
		{"their alias fenced beside ours", []string{bobShellMarkerStart, theirAlias, ours, bobShellMarkerEnd}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			theirs := slices.Contains(tc.lines, theirAlias)

			enabled := replaceBobShellBlock(tc.lines, bobShellBlock(testAbctl))
			if n := countOurAliases(enabled); n != 1 {
				t.Errorf("after enable: %d of our alias lines, want exactly 1:\n%v", n, enabled)
			}
			if theirs && !slices.Contains(enabled, theirAlias) {
				t.Errorf("enable removed the user's own alias:\n%v", enabled)
			}

			// Disable from the ENABLED state, which is the sequence a user performs.
			disabled, found := removeBobShellBlock(enabled)
			if !found {
				t.Fatal("disable found nothing to remove after enable")
			}
			if n := countOurAliases(disabled); n != 0 {
				t.Errorf("after disable: %d of our alias lines left live:\n%v", n, disabled)
			}
			for _, l := range disabled {
				if t2 := strings.TrimSpace(l); t2 == bobShellMarkerStart || t2 == bobShellMarkerEnd {
					t.Errorf("disable left one of our markers behind:\n%v", disabled)
				}
			}
			if theirs && !slices.Contains(disabled, theirAlias) {
				t.Errorf("disable removed the user's own alias:\n%v", disabled)
			}

			// Disabling straight from the original must be just as complete: a user who
			// never ran enable on the damaged file still gets every alias of ours gone.
			fromOriginal, _ := removeBobShellBlock(tc.lines)
			if n := countOurAliases(fromOriginal); n != 0 {
				t.Errorf("disable on the original left %d of our aliases live:\n%v", n, fromOriginal)
			}
			if theirs && !slices.Contains(fromOriginal, theirAlias) {
				t.Errorf("disable on the original removed the user's own alias:\n%v", fromOriginal)
			}
		})
	}
}

// countOurAliases counts lines the shell would take as an alias WE wrote. Independent
// of ownedLines on purpose: a counter built from the thing under test could not catch
// a line that ownership fails to claim, which is the whole family of bugs here.
func countOurAliases(lines []string) int {
	n := 0
	for _, l := range lines {
		if isOurAliasLine(strings.TrimSpace(l)) {
			n++
		}
	}
	return n
}

// TestBobShellOwnership_HoldsOverGeneratedDamage generalizes the table above. Three
// review rounds each produced a new hand-edit that broke a span-based implementation,
// so rather than wait for a fourth, this enumerates every arrangement of the pieces a
// damaged rc file is built from and asserts the same invariant over all of them.
func TestBobShellOwnership_HoldsOverGeneratedDamage(t *testing.T) {
	ours := bobShellAliasLine(testAbctl)
	pieces := []string{bobShellMarkerStart, bobShellMarkerEnd, ours, theirAlias, "", "# mine"}

	var rec func(prefix []string, depth int)
	checked := 0
	rec = func(prefix []string, depth int) {
		if depth == 0 {
			checked++
			lines := slices.Clone(prefix)
			theirs := strings.Count(strings.Join(lines, "\n"), theirAlias)

			enabled := replaceBobShellBlock(lines, bobShellBlock(testAbctl))
			if n := countOurAliases(enabled); n != 1 {
				t.Fatalf("after enable: %d of our aliases, want 1\n  in:  %q\n  out: %q", n, lines, enabled)
			}
			if got := strings.Count(strings.Join(enabled, "\n"), theirAlias); got != theirs {
				t.Fatalf("enable changed the count of the user's own alias %d -> %d\n  in:  %q\n  out: %q",
					theirs, got, lines, enabled)
			}
			disabled, _ := removeBobShellBlock(enabled)
			if n := countOurAliases(disabled); n != 0 {
				t.Fatalf("after disable: %d of our aliases left\n  in:  %q\n  out: %q", n, lines, disabled)
			}
			if got := strings.Count(strings.Join(disabled, "\n"), theirAlias); got != theirs {
				t.Fatalf("disable changed the count of the user's own alias %d -> %d\n  in:  %q\n  out: %q",
					theirs, got, lines, disabled)
			}
			return
		}
		for _, p := range pieces {
			rec(append(prefix, p), depth-1)
		}
	}
	// Up to 4 lines: 6^1+6^2+6^3+6^4 = 1554 files, which covers every ordering of a
	// marker pair with two lines between them — the shape all three MUST FIX items of
	// round 5 live in — and runs in well under a second.
	for depth := 1; depth <= 4; depth++ {
		rec(nil, depth)
	}
	t.Logf("checked %d generated files", checked)
}

// TestBobShellStatus_ReportsEveryAlias — a damaged file can hold more than one of our
// aliases, and reporting only the first describes a file the user does not have. The
// shell takes the last, so the count is what matters, not which one is shown.
func TestBobShellStatus_ReportsEveryAlias(t *testing.T) {
	rc := filepath.Join(t.TempDir(), "rc")
	ours := bobShellAliasLine(testAbctl)
	content := strings.Join([]string{bobShellMarkerStart, ours, bobShellMarkerStart, ours, ""}, "\n")
	if err := os.WriteFile(rc, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if code := bobShellStatus(rc, &out); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "2 alias lines") {
		t.Errorf("status does not report that there are two aliases:\n%s", out.String())
	}
	// And enable must collapse them, which is what status tells the user to do.
	var eout, errb bytes.Buffer
	if code := bobShellEnable(rc, testAbctl, true, &eout, &errb); code != 0 {
		t.Fatalf("enable: exit = %d, want 0 (stderr: %s)", code, errb.String())
	}
	if n := countOurAliases(strings.Split(readFile(t, rc), "\n")); n != 1 {
		t.Errorf("enable left %d aliases, want 1:\n%s", n, readFile(t, rc))
	}
}
