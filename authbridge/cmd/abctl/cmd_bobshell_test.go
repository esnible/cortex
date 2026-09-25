package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
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

// withSelfPath sets bobShellSelfPath for the duration of a test, the way runBobShell
// sets it for the duration of a process.
//
// The tests call bobShellEnable / bobShellDisable / bobShellStatus directly, so they do
// not go through the one place that resolves it. That is exactly the asymmetry the
// self-path recognition exists to remove — a verb that has not been told what binary it
// is falls back to matching "abctl" in the name — so a test asserting behaviour for a
// given binary has to say which binary it means.
func withSelfPath(t *testing.T, p string) {
	t.Helper()
	prev := bobShellSelfPath
	bobShellSelfPath = p
	t.Cleanup(func() { bobShellSelfPath = prev })
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
		withSelfPath(t, self)
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
		self, err := abctlPath()
		if err != nil {
			t.Fatalf("abctlPath: %v", err)
		}
		withSelfPath(t, self)
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
		// "not enabled" is the verdict; the "no alias line" elaboration is gone with
		// round 6. An empty marker pair is no longer something this command claims —
		// a pair fences nothing live, so there is no way to tell it from a user's note
		// pasted out of `--help`, which is the deletion MF3 reported. Status describes
		// a file it owns nothing in, and "not enabled" is the whole truth about it.
		if !strings.Contains(out.String(), "not enabled") {
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
	// One alias, and one MATCHED pair. The orphaned START is left where it was: an
	// unpaired marker is inert text this command no longer claims (round 6 / MF3 —
	// claiming bare markers deleted lines out of a user's documentation comment). So
	// the count of marker-shaped lines is 2 here, and that is deliberate.
	if n := strings.Count(got, "alias bob="); n != 1 {
		t.Errorf("alias appears %d times, want 1:\n%s", n, got)
	}
	if _, _, ok := ownedMarkerPair(strings.Split(got, "\n")); !ok {
		t.Errorf("no matched marker pair around the alias:\n%s", got)
	}
	// The property the test was written for — repeated enables must not pile up — is
	// what actually matters, and it still holds: the second enable finds the pair from
	// the first and replaces it in place.
	var out2, errb2 bytes.Buffer
	if code := bobShellEnable(rc, testAbctl, true, &out2, &errb2); code != 0 {
		t.Fatalf("second enable: exit = %d (stderr: %s)", code, errb2.String())
	}
	again := readFile(t, rc)
	if n := strings.Count(again, "alias bob="); n != 1 {
		t.Errorf("after a second enable the alias appears %d times, want 1:\n%s", n, again)
	}
	if again != got {
		t.Errorf("a second enable changed the file:\n%s\n--- vs ---\n%s", got, again)
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
			// No marker PAIR of ours may survive. A lone marker does, and here that is
			// the START whose partner this test just damaged — an unpaired marker is
			// inert text we no longer claim, because nothing distinguishes it from a
			// user's note (round 6 / MF3: claiming them deleted lines out of a
			// documentation comment). Round 4's suggestion 4 asked about this residue
			// and I answered "already removed"; that was true then only as a side
			// effect of prefix matching, and it could not survive fixing that.
			if _, _, ok := ownedMarkerPair(strings.Split(got, "\n")); ok {
				t.Errorf("disable left a marker pair of ours behind:\n%s", got)
			}
			// There is no undamaged row here — both rows destroy the END marker, so the
			// surviving START is expected in both. An earlier "if tc.damage == ''"
			// clause read as "the undamaged case" but selected the DELETED one, where a
			// residual marker is correct; it passed only while bare markers were
			// claimed. TestBobShellEnable_BackupWrittenOnce covers the clean round trip.
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

			if len(ownedLines(lines)) == 0 {
				t.Fatal("nothing recognized as ours; the recovery path did not run")
			}
			// Ownership is a SET, and it is what decides what gets deleted, so it is what
			// this asserts. A hand-edit can leave our lines non-contiguous, so there is
			// deliberately no span/hull helper for a writer to key off by mistake.
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
		// Round 7 MF2: this row asserted false — the trailing comment made the old
		// text-matching predicate miss a LIVE alias, and the test pinned that in
		// place. bash resolves it to exactly our alias, so it is ours.
		{"alias bob='/opt/abctl/abctl exec -- \\bob' # mine", true},
		// Round 7 MF2/MF3: spellings that reduce to the same alias. All were missed.
		{`alias bob=/b/abctl' exec -- \bob'`, true},
		{`alias bob="/b/abctl exec -- \bob"`, true},
		{`alias bob="/b/abctl exec --fast -- bob"`, true},
		// Still not ours: the real binary, and our shape around a foreign program.
		{`alias bob=/usr/bin/evil' exec -- \bob'`, false},
		{`alias bob='/b/abctl service status'`, false},
		{`# alias bob='/b/abctl exec -- \bob'`, false},
		// An abctl, our tail, but some other subcommand. `service status` above does not
		// reach the subcommand check — it is rejected on arity first — so without this
		// row, dropping the `exec` check entirely changed no test's answer.
		{`alias bob='/b/abctl frobnicate -- bob'`, false},
		// Our words without the `--` separator. `exec bob bob` passes every other
		// clause, so it is the only shape that pins the separator itself.
		{`alias bob='/b/abctl exec bob bob'`, false},
		// Our exact shape, proxying something that is not bob. Claiming it would mean
		// disable deleting a user's alias that deliberately routes another program
		// through Cortex — the severe direction, and nothing else in the table pins the
		// final word, so without this row the `bob` check could be dropped unnoticed.
		{`alias bob='/b/abctl exec -- other'`, false},
		// Backslash-escaped spaces, no quotes anywhere. bash resolves this to exactly
		// our alias value, so it is live and must be found. It is the MF2 class the
		// round-7 review named — a spelling that IS live without looking it — and the
		// unquoted-backslash branch is the only thing that reads it correctly.
		{`alias bob=/b/abctl\ exec\ --\ bob`, true},
		// The `alias -- bob=` spelling. Both bash and zsh accept it and define exactly
		// the same alias, so a live one written this way must be found — dropping the
		// alternative changed no test's answer until this row existed.
		{`alias -- bob='/b/abctl exec -- \bob'`, true},
		{`alias -- bob='/usr/local/bin/bob --fast'`, false},
		// One field: no subcommand to index. This row is the arity floor's whole job —
		// removing the floor panics here rather than returning an answer.
		{`alias bob=/b/abctl`, false},
		{"", false},
	} {
		if got := aliasRoutesThroughAbctl(tc.line); got != tc.want {
			t.Errorf("aliasRoutesThroughAbctl(%q) = %v, want %v", tc.line, got, tc.want)
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
			// No marker PAIR may survive — a pair fencing a live alias is a block we
			// wrote, and leaving one would let the next enable nest inside it.
			//
			// An UNPAIRED marker is deliberately left, which is round 6's third finding
			// and a change from what this assertion demanded before. Ownership by bare
			// marker text deleted lines 3 and 5 out of a user's documentation comment —
			// the `--help` output pasted in as a note, so byte-identical to ours — while
			// reporting "Disabled." Since nothing textual distinguishes those markers
			// from real ones, the only safe rule is that a lone marker is inert text.
			// The residue is cosmetic; the deletion was not.
			if _, _, paired := ownedMarkerPair(disabled); paired {
				t.Errorf("disable left a marker pair of ours behind:\n%v", disabled)
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

// ownedMarkerPair reports whether lines still hold a START/END pair fencing a live
// alias — a block this command would recognise as its own. Written out here rather
// than calling ownedLines so the assertion does not inherit the bug it is checking
// for, the same reason countOurAliases is independent.
func ownedMarkerPair(lines []string) (int, int, bool) {
	for i := range lines {
		if strings.TrimSpace(lines[i]) != bobShellMarkerStart {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			t := strings.TrimSpace(lines[j])
			if t == bobShellMarkerStart {
				break
			}
			if t == bobShellMarkerEnd {
				for k := i + 1; k < j; k++ {
					if kt := strings.TrimSpace(lines[k]); aliasRoutesThroughAbctl(kt) {
						return i, j, true
					}
				}
				break
			}
		}
	}
	return -1, -1, false
}

// countOurAliases counts lines the shell would take as an alias WE wrote. Independent
// of ownedLines on purpose: a counter built from the thing under test could not catch
// a line that ownership fails to claim, which is the whole family of bugs here.
func countOurAliases(lines []string) int {
	n := 0
	for _, l := range lines {
		if aliasRoutesThroughAbctl(strings.TrimSpace(l)) {
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

// TestBobShellDisable_PromptMatchesTheWrite is the round-6 MUST FIX 1 regression: the
// confirmation prompt must list exactly the lines the write removes.
//
// It printed the hull of the owned lines instead, which on a file whose owned lines are
// non-contiguous spans the user's own lines between ours. The reviewer's fixture is
// the one that shows why it is not cosmetic: the prompt claimed
// `export SECRET_TOKEN=...` and `source ~/.work_secrets` were being removed, and they
// were not. A consent prompt that overstates a destructive edit is as broken as one
// that understates it — a user either declines a safe operation or believes their
// secrets are gone.
func TestBobShellDisable_PromptMatchesTheWrite(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".zshrc")
	// The fixture needs TWO owned lines with the user's content BETWEEN them, or the
	// bug is unreachable: the hull of a single owned line is that same line, so a
	// fixture owning only the alias cannot tell the hull from the owned set, and the
	// mutation that reverts this fix passes it. Here a complete marker PAIR is owned
	// while the three lines it fences are not, so hull and owned set differ by exactly
	// the user's content — which is the reviewer's fixture and the real-world shape.
	body := strings.Join([]string{
		"# my rc",
		bobShellMarkerStart,
		"export SECRET_TOKEN=hunter2",
		"source ~/.work_secrets",
		"alias deploy='make deploy-prod'",
		bobShellAliasLine(testAbctl),
		bobShellMarkerEnd,
		"# tail",
	}, "\n") + "\n"
	if err := os.WriteFile(rc, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, rc)

	var out, errb bytes.Buffer
	if code := bobShellDisable(rc, true, &out, &errb); code != 0 {
		t.Fatalf("disable: exit = %d (stderr: %s)", code, errb.String())
	}
	prompt := out.String()

	// Every line the prompt claims to remove must actually be gone, and every line it
	// does not mention must still be there. Stated as two directions because only the
	// pair pins "the prompt describes the write" — one alone permits a prompt that
	// lists everything, or nothing.
	after := readFile(t, rc)
	for _, l := range []string{"export SECRET_TOKEN=hunter2", "source ~/.work_secrets", "alias deploy='make deploy-prod'"} {
		if strings.Contains(prompt, l) {
			t.Errorf("prompt claims to remove a line it does not touch: %q\n%s", l, prompt)
		}
		if !strings.Contains(after, l) {
			t.Errorf("the write removed %q, which the prompt did not mention", l)
		}
	}
	if !strings.Contains(prompt, bobShellAliasLine(testAbctl)) {
		t.Errorf("prompt does not mention the alias it removes:\n%s", prompt)
	}
	if strings.Contains(after, "exec -- ") {
		t.Errorf("disable left our alias live:\n%s", after)
	}
	if before == after {
		t.Error("disable changed nothing")
	}
}

// TestBobShellDisable_ClaimsOurMechanismInsideOurFence is round-6 MUST FIX 2: an alias
// inside a fence we wrote, spelled with different quoting (another abctl build, or a
// hand-edit), is still our mechanism and must go.
//
// Leaving it produced the worst outcome in this feature: disable printed the alias as
// removed, said "Disabled.", exited 0 — and a shell sourcing the file still routed bob
// through the proxy, with status reporting "not enabled". See looksLikeOurMechanism for
// why a fence's interior inverts the burden of proof without abandoning it.
func TestBobShellDisable_ClaimsOurMechanismInsideOurFence(t *testing.T) {
	for _, tc := range []struct{ name, alias string }{
		{"double quoted", `alias bob="` + testAbctl + ` exec -- \bob"`},
		{"extra flag", `alias bob='` + testAbctl + ` exec --profile x -- \bob'`},
		{"no backslash", `alias bob='` + testAbctl + ` exec -- bob'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := filepath.Join(t.TempDir(), ".zshrc")
			body := "# my rc\n" + bobShellMarkerStart + "\n" + tc.alias + "\n" + bobShellMarkerEnd + "\n"
			if err := os.WriteFile(rc, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			var out, errb bytes.Buffer
			if code := bobShellDisable(rc, true, &out, &errb); code != 0 {
				t.Fatalf("disable: exit = %d (stderr: %s)", code, errb.String())
			}
			got := readFile(t, rc)
			if strings.Contains(got, "alias bob") {
				t.Errorf("disable reported success but left a live alias:\n%s", got)
			}
			if !strings.Contains(got, "# my rc") {
				t.Errorf("disable ate the user's line:\n%s", got)
			}
			// And status must agree with the file, which is the half that made the bug
			// invisible: it said "not enabled" over a live alias.
			out.Reset()
			if code := bobShellStatus(rc, &out); code != 0 {
				t.Fatalf("status: exit = %d, want 0", code)
			}
			if !strings.Contains(out.String(), "not enabled") {
				t.Errorf("status = %q, want not enabled", out.String())
			}
		})
	}
}

// TestBobShellDisable_LeavesMarkersInUserDocumentation is round-6 MUST FIX 3: marker
// lines this command did not write must survive byte-for-byte.
//
// `--help` prints the block verbatim, so a user copying it into their rc as a note is
// the expected path to a file containing our exact marker text. Ownership by bare
// marker deleted two lines out of the middle of such a comment, silently, while
// reporting "Disabled." Nothing textual separates those markers from real ones — the
// user copied them — so the discriminator is whether the pair fences a LIVE alias.
func TestBobShellDisable_LeavesMarkersInUserDocumentation(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".zshrc")
	body := strings.Join([]string{
		"# my rc",
		"# Notes on the cortex alias. The block abctl writes looks like:",
		bobShellMarkerStart,
		`#   alias bob='...abctl exec -- \bob'`,
		bobShellMarkerEnd,
		"# ...which I keep here for reference.",
		"export FOO=1",
	}, "\n") + "\n"
	if err := os.WriteFile(rc, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := bobShellDisable(rc, true, &out, &errb); code != 0 {
		t.Fatalf("disable: exit = %d (stderr: %s)", code, errb.String())
	}
	if got := readFile(t, rc); got != body {
		t.Errorf("disable edited a file it owns nothing in:\n%s\n--- want ---\n%s", got, body)
	}
	// Status must agree that there is nothing here of ours.
	out.Reset()
	if code := bobShellStatus(rc, &out); code != 0 {
		t.Fatalf("status: exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "not enabled") {
		t.Errorf("status = %q, want not enabled", out.String())
	}
	// An empty pair is the same case: nothing live inside, so nothing to claim.
	rc2 := filepath.Join(t.TempDir(), ".zshrc")
	empty := bobShellMarkerStart + "\n" + bobShellMarkerEnd + "\n"
	if err := os.WriteFile(rc2, []byte(empty), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := bobShellDisable(rc2, true, &out, &errb); code != 0 {
		t.Fatalf("disable on empty pair: exit = %d", code)
	}
	if got := readFile(t, rc2); got != empty {
		t.Errorf("disable edited an empty marker pair:\n%s", got)
	}
}

// TestLooksLikeOurMechanism pins the predicate that separates round 5's constraint from
// round 6's. Both are `alias bob=` lines inside a matched pair; what decides ownership
// is whether the body invokes an abctl's `exec -- bob` (this feature) or the real bob
// binary (the user's own intent).
func TestLooksLikeOurMechanism(t *testing.T) {
	for _, tc := range []struct {
		line string
		want bool
	}{
		{`alias bob='/b/abctl exec -- \bob'`, true},
		{`alias bob="/b/abctl exec -- \bob"`, true},
		{`alias bob='/b/abctl exec --profile x -- \bob'`, true},
		{`alias bob='/b/abctl exec -- bob'`, true},
		// Round 5's constraint: the user's own alias to the real binary.
		{`alias bob='/usr/local/bin/bob --fast'`, false},
		{`alias bob='bob --fast'`, false},
		// An abctl, but not this mechanism.
		{`alias bob='/b/abctl service status'`, false},
		// Our mechanism's SHAPE around a binary that is not an abctl. Claiming this
		// would mean disable silently deleting an alias to someone else's program that
		// happens to take `exec -- bob` arguments, so the first field must be checked
		// and not just the tail.
		{`alias bob='/usr/bin/evil exec -- \bob'`, false},
		{`alias bob='/usr/bin/sudo exec -- bob'`, false},
		// Round 7 MF2: asserted false, and that was the bug — the trailing comment
		// hid a live alias from disable while bash ran it happily.
		{`alias bob='/b/abctl exec -- \bob' # note`, true},
		{`alias bob=/b/abctl' exec -- \bob'`, true},
		// Unrecognised shapes stay unclaimed — the safe direction.
		{`alias bobby='/b/abctl exec -- \bob'`, false},
		{`# alias bob='/b/abctl exec -- \bob'`, false},
		{"", false},
	} {
		if got := aliasRoutesThroughAbctl(tc.line); got != tc.want {
			t.Errorf("aliasRoutesThroughAbctl(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

// TestBobShellDisable_LeavesHeredocBodiesAlone pins round 7's first finding.
//
// A heredoc hands its lines to another program; they are data, not commands the
// shell runs. disable was editing them: on the python3 case it excised the alias
// line from the middle of a string literal, printed "Disabled.", exited 0 — over a
// file where `bash -c 'source ...; alias bob'` reports no alias at all — and left
// the script running with a silently different value. The cat case lost all three
// lines, collapsing the heredoc to its opener immediately followed by its
// terminator, which breaks the user's script outright.
//
// The fixtures are the reviewer's, executed rather than paraphrased. Each is checked
// byte-for-byte, because the failure was silent: nothing in the output said a
// heredoc had been touched.
func TestBobShellDisable_LeavesHeredocBodiesAlone(t *testing.T) {
	alias := bobShellAliasLine("/usr/local/bin/abctl")
	for _, tc := range []struct {
		name  string
		lines []string
	}{
		{"quoted cat heredoc", []string{
			"#!/bin/bash", "cat <<'EOF'",
			bobShellMarkerStart, alias, bobShellMarkerEnd,
			"EOF", "echo done",
		}},
		{"python string literal", []string{
			"# my rc", "python3 - <<'PY'", `doc = """`,
			bobShellMarkerStart, alias, bobShellMarkerEnd,
			`"""`, "print(len(doc))", "PY",
		}},
		{"unquoted heredoc", []string{
			"cat <<EOF", bobShellMarkerStart, alias, bobShellMarkerEnd, "EOF",
		}},
		{"tab-stripping heredoc", []string{
			"cat <<-'EOF'", bobShellMarkerStart, "\t" + alias, bobShellMarkerEnd, "\tEOF",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := filepath.Join(t.TempDir(), ".zshrc")
			body := strings.Join(tc.lines, "\n") + "\n"
			if err := os.WriteFile(rc, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			var out, errb bytes.Buffer
			if code := bobShellDisable(rc, true, &out, &errb); code != 0 {
				t.Fatalf("disable: exit = %d (stderr: %s)", code, errb.String())
			}
			if got := readFile(t, rc); got != body {
				t.Errorf("disable edited heredoc body:\n%s\n--- want ---\n%s", got, body)
			}
			// And status must not claim an alias that exists only as data.
			out.Reset()
			if code := bobShellStatus(rc, &out); code != 0 {
				t.Fatalf("status: exit = %d, want 0", code)
			}
			if !strings.Contains(out.String(), "not enabled") {
				t.Errorf("status = %q, want not enabled (the alias is heredoc data)", out.String())
			}
		})
	}
}

// TestBobShellDisable_RemovesEverySpellingOfOurAlias pins round 7's second and third
// findings, which are one defect reported from two directions.
//
// Each of these is a LIVE alias — bash resolves every one to the same thing our own
// canonical line resolves to — that the old text-matching predicate missed. disable
// printed "Nothing to do", exited 0, and left the shell routing bob through the
// proxy; status said not enabled. The third case is the sharpest, because it is the
// exact spelling round 6 added support for, with its markers hand-deleted: unowned at
// file scope, so enable then appended a SECOND live alias.
func TestBobShellDisable_RemovesEverySpellingOfOurAlias(t *testing.T) {
	for _, tc := range []struct{ name, line string }{
		{"trailing comment", `alias bob='/b/abctl exec -- \bob' # note`},
		{"split quoting", `alias bob=/b/abctl' exec -- \bob'`},
		{"double quoted, unfenced", `alias bob="/b/abctl exec -- \bob"`},
		{"extra exec flag", `alias bob='/b/abctl exec --fast -- bob'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := filepath.Join(t.TempDir(), ".zshrc")
			if err := os.WriteFile(rc, []byte("# rc\n"+tc.line+"\nexport X=1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			// status must see it.
			var out, errb bytes.Buffer
			if code := bobShellStatus(rc, &out); code != 0 {
				t.Fatalf("status: exit = %d", code)
			}
			if !strings.Contains(out.String(), "enabled in ") || strings.Contains(lastLine(out.String()), "not enabled") {
				t.Errorf("status = %q, want it to report this live alias", out.String())
			}
			// disable must remove it.
			out.Reset()
			if code := bobShellDisable(rc, true, &out, &errb); code != 0 {
				t.Fatalf("disable: exit = %d (stderr: %s)", code, errb.String())
			}
			got := readFile(t, rc)
			if strings.Contains(got, "alias bob") {
				t.Errorf("disable exited 0 but left the live alias:\n%s", got)
			}
			if !strings.Contains(got, "export X=1") || !strings.Contains(got, "# rc") {
				t.Errorf("disable took the user's lines too:\n%s", got)
			}
			// And enable must not stack a second alias on top of one already live.
			if err := os.WriteFile(rc, []byte("# rc\n"+tc.line+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			if code := bobShellEnable(rc, testAbctl, true, &out, &errb); code != 0 {
				t.Fatalf("enable: exit = %d (stderr: %s)", code, errb.String())
			}
			if n := countOurAliases(strings.Split(readFile(t, rc), "\n")); n != 1 {
				t.Errorf("after enable over a live alias, %d live aliases, want 1:\n%s", n, readFile(t, rc))
			}
		})
	}
}

// TestHeredocDelim pins the operator scan, including the two ways it was wrong before
// being executed against real input.
func TestHeredocDelim(t *testing.T) {
	for _, tc := range []struct {
		line   string
		delim  string
		dashed bool
	}{
		{"cat <<'EOF'", "EOF", false},
		{"cat <<EOF", "EOF", false},
		{`cat <<"EOF"`, "EOF", false},
		{"cat <<-'EOF'", "EOF", true},
		{"python3 - <<'PY'", "PY", false},
		// The last operator on the line is the one whose body comes next.
		{"cat a <<X b <<Y", "Y", false},
		// A herestring has no body. `<<<` was read as `<<` plus a delimiter, and
		// since our own END marker contains `<<<` that marked the rest of an
		// ordinary rc file as heredoc body — which would have made disable a no-op
		// on real blocks. Only running it showed this.
		{"cat <<<'word'", "", false},
		{bobShellMarkerEnd, "", false},
		{bobShellMarkerStart, "", false},
		// An earlier pattern consumed the delimiter's first byte, yielding "OF".
		{"cat <<ABC", "ABC", false},
		{"alias bob='/b/abctl exec -- \\bob'", "", false},
		{"", "", false},
	} {
		d, dash := heredocDelim(tc.line, 0)
		if d != tc.delim || dash != tc.dashed {
			t.Errorf("heredocDelim(%q) = (%q, %v), want (%q, %v)", tc.line, d, dash, tc.delim, tc.dashed)
		}
	}
}

// TestUnquoteShellWord pins the unquoter that makes one predicate possible.
//
// The point of the table is the block in the middle: five different spellings that
// must all reduce to the SAME value, because to the shell they are the same alias.
// Comparing text instead of meaning is what made rounds 5 and 6 need two predicates
// with opposite burdens of proof, and what let a live alias hide behind a comment.
// TestUnquoteShellWord_UnquotedBackslashEscapes pins the backslash outside quotes,
// where it escapes the very next character whatever it is — most usefully a space,
// which is how a word can contain one without any quotes at all.
//
// bash resolves `alias bob=/b/abctl\ exec\ --\ bob` to the same value as the quoted
// form, so this is a live alias that must be recognised. Mutating the branch to write
// the backslash through instead survived the suite until this existed.
func TestUnquoteShellWord_UnquotedBackslashEscapes(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{`/b/ab\ ctl`, `/b/ab ctl`},
		{`/b/abctl\ exec\ --\ bob`, `/b/abctl exec -- bob`},
		{`\#notacomment`, `#notacomment`},
	} {
		got, ok := unquoteShellWord(tc.in)
		if !ok || got != tc.want {
			t.Errorf("unquoteShellWord(%q) = %q, %v; want %q, true", tc.in, got, ok, tc.want)
		}
	}
}

// TestUnquoteShellWord_DoubleQuoteEscapes pins the four characters a backslash
// escapes inside double quotes, and the far more common case of one that escapes
// nothing.
//
// The escape branch survived mutation until this test existed, and the reason is worth
// keeping: the only double-quoted row in the predicate table was
// `alias bob="... -- \bob"`, where deleting the branch writes the backslash through
// literally and produces the same `\bob` the correct path produces. The row could not
// fail. What the branch actually governs is `\"`, `\\`, `\$` and backtick — asked of
// bash directly, which reports `"\bob"` → `\bob` but `"\\bob"` → `\bob` and
// `"a\"b"` → `a"b`.
func TestUnquoteShellWord_DoubleQuoteEscapes(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{`"/b/abctl exec -- \bob"`, `/b/abctl exec -- \bob`},  // \b escapes nothing: both kept
		{`"/b/abctl exec -- \\bob"`, `/b/abctl exec -- \bob`}, // \\ is one backslash
		{`"/b/ab\"ctl"`, `/b/ab"ctl`},                         // \" is a literal quote
		{`"/b/abctl -- \$bob"`, `/b/abctl -- $bob`},           // \$ suppresses expansion
	} {
		got, ok := unquoteShellWord(tc.in)
		if !ok || got != tc.want {
			t.Errorf("unquoteShellWord(%q) = %q, %v; want %q, true", tc.in, got, ok, tc.want)
		}
	}
}

// TestUnquoteShellWord_HashIsOnlyACommentBetweenWords pins the one place this
// unquoter deliberately differs from what "handle comments" would suggest.
//
// `#` opens a comment only where a word could begin. Mid-word it is an ordinary
// character: `bash` and `zsh` both define `alias bob=/bin/echo#c` with the value
// `/bin/echo#c`. An earlier draft terminated the word at any unquoted `#`, which is
// unobservable through the predicate — such a value is not our alias under either
// reading — and therefore exactly the kind of wrong that survives a test suite.
func TestUnquoteShellWord_HashIsOnlyACommentBetweenWords(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{`/bin/echo#c`, `/bin/echo#c`},       // abuts the word: part of the value
		{`/bin/echo' x'#c`, `/bin/echo x#c`}, // same, after a quoted section
		{`/bin/echo # c`, `/bin/echo`},       // space first: the comment is separate
		{`'#notacomment'`, `#notacomment`},   // quoted: always literal
	} {
		got, ok := unquoteShellWord(tc.in)
		if !ok || got != tc.want {
			t.Errorf("unquoteShellWord(%q) = %q, %v; want %q, true", tc.in, got, ok, tc.want)
		}
	}
}

func TestUnquoteShellWord(t *testing.T) {
	const want = `/b/abctl exec -- \bob`
	for _, in := range []string{
		`'/b/abctl exec -- \bob'`,
		`'/b/abctl exec -- \bob' # note`,
		`/b/abctl' exec -- \bob'`,
		`"/b/abctl exec -- \bob"`,
		`/b/abctl" exec -- "'\bob'`,
	} {
		got, ok := unquoteShellWord(in)
		if !ok || got != want {
			t.Errorf("unquoteShellWord(%q) = (%q, %v), want (%q, true)", in, got, ok, want)
		}
	}
	// Unterminated quoting is not something to guess at.
	for _, in := range []string{`'/b/abctl exec`, `"/b/abctl exec`, `abc\`} {
		if _, ok := unquoteShellWord(in); ok {
			t.Errorf("unquoteShellWord(%q) reported a clean parse, want not ok", in)
		}
	}
}

// TestBobShellEnable_IsIdempotentOverAnOrphanMarker pins a bug this round introduced
// and mutation-style re-running caught.
//
// Gating marker ownership on "is there a live alias in this file" fixed the
// documentation case but made enable non-idempotent: with an orphan START and no
// alias yet, the first enable saw no live alias, left the orphan unclaimed and
// appended past it; the second enable saw the alias it had just written, claimed the
// orphan and absorbed it. Two identical calls, two different files. See
// ownedLinesFor's `installing` parameter.
func TestBobShellEnable_IsIdempotentOverAnOrphanMarker(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".zshrc")
	if err := os.WriteFile(rc, []byte("# mine\n"+bobShellMarkerStart+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := bobShellEnable(rc, testAbctl, true, &out, &errb); code != 0 {
		t.Fatalf("first enable: exit = %d (stderr: %s)", code, errb.String())
	}
	first := readFile(t, rc)
	out.Reset()
	if code := bobShellEnable(rc, testAbctl, true, &out, &errb); code != 0 {
		t.Fatalf("second enable: exit = %d (stderr: %s)", code, errb.String())
	}
	if second := readFile(t, rc); second != first {
		t.Errorf("enable is not idempotent over an orphan marker:\n%s\n--- vs ---\n%s", first, second)
	}
	if n := countOurAliases(strings.Split(first, "\n")); n != 1 {
		t.Errorf("%d live aliases after enable, want 1:\n%s", n, first)
	}
}

// TestAliasRoutesThroughAbctl_RecognisesRenamedBinaries pins the other bug this round
// introduced.
//
// Requiring the path to END in "abctl" failed to recognise a block this command had
// genuinely written whenever the binary was named anything else — the test binary
// (abctl.test) is one, and so is a user's abctl-dev or a versioned download. Failing
// to recognise our own alias is the severe direction: disable reports success over a
// live proxy alias. Matching the basename still refuses a foreign program, which is
// the only thing the check is for.
func TestAliasRoutesThroughAbctl_RecognisesRenamedBinaries(t *testing.T) {
	for _, p := range []string{"/tmp/abctl.test", "/usr/local/bin/abctl-dev", "/opt/abctl.v2", "/x/my-abctl"} {
		if !aliasRoutesThroughAbctl(bobShellAliasLine(p)) {
			t.Errorf("aliasRoutesThroughAbctl did not recognise our own alias for %q", p)
		}
	}
	for _, p := range []string{"/usr/bin/evil", "/usr/bin/sudo", "/bin/sh"} {
		if aliasRoutesThroughAbctl(bobShellAliasLine(p)) {
			t.Errorf("aliasRoutesThroughAbctl claimed an alias to %q", p)
		}
	}
}

// TestBobShellDisable_RealBlockAfterAHeredoc is the other half of heredoc handling,
// and it exists because mutation testing found it missing.
//
// Mutating "the terminator closes the body" to a no-op SURVIVED the whole suite: with
// every line after a heredoc treated as data, disable silently does nothing on any rc
// file that opens a heredoc before our block — and no test noticed. Skipping heredoc
// bodies is only half a requirement; resuming afterwards is the other half, and a
// suite that only checks the skipping cannot tell correct tracking from tracking that
// never stops.
func TestBobShellDisable_RealBlockAfterAHeredoc(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".zshrc")
	body := strings.Join([]string{
		"# rc", "cat <<'EOF'", "just text", "EOF",
		bobShellMarkerStart, bobShellAliasLine("/x/abctl"), bobShellMarkerEnd,
	}, "\n") + "\n"
	if err := os.WriteFile(rc, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := bobShellStatus(rc, &out); code != 0 {
		t.Fatalf("status: exit = %d", code)
	}
	if strings.Contains(lastLine(out.String()), "not enabled") {
		t.Errorf("status = %q, want the block after the heredoc reported as enabled", out.String())
	}
	out.Reset()
	if code := bobShellDisable(rc, true, &out, &errb); code != 0 {
		t.Fatalf("disable: exit = %d (stderr: %s)", code, errb.String())
	}
	got := readFile(t, rc)
	if strings.Contains(got, "alias bob") {
		t.Errorf("disable left the block that follows a heredoc:\n%s", got)
	}
	// The heredoc itself is untouched.
	for _, want := range []string{"cat <<'EOF'", "just text", "EOF"} {
		if !strings.Contains(got, want) {
			t.Errorf("disable damaged the heredoc (missing %q):\n%s", want, got)
		}
	}
}

// TestBobShellDisable_RealBlockAfterATabbedHeredoc is the same requirement as
// TestBobShellDisable_RealBlockAfterAHeredoc for the `<<-` form, which strips leading
// tabs from its terminator.
//
// It is separate because it needs the block AFTER the heredoc to discriminate at all.
// Disabling the tab-strip was a surviving mutation against a fixture that did have a
// tab-indented terminator: without stripping, the heredoc never closes, so the whole
// rest of the file stays body — and a test that only asserts "nothing was changed"
// cannot tell that from correct handling, because in both cases nothing is changed.
// Putting something real past the terminator is what makes the two outcomes differ.
func TestBobShellDisable_RealBlockAfterATabbedHeredoc(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".zshrc")
	body := strings.Join([]string{
		"cat <<-'EOF'", "\tjust text", "\tEOF",
		bobShellMarkerStart, bobShellAliasLine("/x/abctl"), bobShellMarkerEnd,
	}, "\n") + "\n"
	if err := os.WriteFile(rc, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := bobShellStatus(rc, &out); code != 0 {
		t.Fatalf("status: exit = %d", code)
	}
	if strings.Contains(lastLine(out.String()), "not enabled") {
		t.Errorf("status = %q, want the block after the tabbed heredoc reported as enabled", out.String())
	}
	out.Reset()
	if code := bobShellDisable(rc, true, &out, &errb); code != 0 {
		t.Fatalf("disable: exit = %d (stderr: %s)", code, errb.String())
	}
	got := readFile(t, rc)
	if strings.Contains(got, "alias bob") {
		t.Errorf("disable left the block that follows a <<- heredoc:\n%s", got)
	}
	for _, want := range []string{"cat <<-'EOF'", "\tjust text", "\tEOF"} {
		if !strings.Contains(got, want) {
			t.Errorf("disable damaged the heredoc (missing %q):\n%s", want, got)
		}
	}
}

// TestBobShell_RoundTripUnderAnyBinaryName is the regression test for a bug a real
// round trip found and no unit test could: behaviour that depended on what the binary
// was CALLED.
//
// Ownership matched `abctl` in the basename, so a binary named anything else did not
// recognise the block it had itself just written. Built as /tmp/ab8, a second enable
// appended a duplicate alias and disable then left one live — while the identical run
// as /tmp/abctl-dev was correctly idempotent. Same inputs, same code, different
// filename, different outcome.
//
// The names below are the realistic ones: a versioned download, a symlink a user made,
// a scratch build. Each must enable once, stay byte-identical on a second enable, and
// round-trip back to the original file on disable.
func TestBobShell_RoundTripUnderAnyBinaryName(t *testing.T) {
	for _, self := range []string{
		"/tmp/ab8",             // the scratch build that found this
		"/home/u/bin/cortex",   // symlinked under another name entirely
		"/opt/abctl-v2.3/ctl",  // versioned install, binary not named abctl
		"/usr/local/bin/abctl", // the ordinary case, still has to work
	} {
		t.Run(filepath.Base(self), func(t *testing.T) {
			withSelfPath(t, self)
			rc := filepath.Join(t.TempDir(), ".zshrc")
			orig := "# my rc\nexport FOO=1\n"
			if err := os.WriteFile(rc, []byte(orig), 0o644); err != nil {
				t.Fatal(err)
			}
			var out, errb bytes.Buffer
			if code := bobShellEnable(rc, self, true, &out, &errb); code != 0 {
				t.Fatalf("enable: exit = %d (stderr: %s)", code, errb.String())
			}
			afterFirst := readFile(t, rc)
			if n := strings.Count(afterFirst, "alias bob"); n != 1 {
				t.Fatalf("first enable wrote %d alias lines, want 1:\n%s", n, afterFirst)
			}

			// Idempotence is where the name dependence showed: a binary that cannot see
			// its own alias appends another one.
			out.Reset()
			if code := bobShellEnable(rc, self, true, &out, &errb); code != 0 {
				t.Fatalf("second enable: exit = %d", code)
			}
			if got := readFile(t, rc); got != afterFirst {
				t.Errorf("second enable changed the file:\n--- first ---\n%s\n--- second ---\n%s", afterFirst, got)
			}

			// status must see it too, or it reports not-enabled over a live alias.
			out.Reset()
			if code := bobShellStatus(rc, &out); code != 0 {
				t.Errorf("status: exit = %d", code)
			}
			if strings.Contains(lastLine(out.String()), "not enabled") {
				t.Errorf("status = %q, want enabled for a block this binary wrote", out.String())
			}

			// And disable must remove it, not report success over it.
			out.Reset()
			if code := bobShellDisable(rc, true, &out, &errb); code != 0 {
				t.Fatalf("disable: exit = %d (stderr: %s)", code, errb.String())
			}
			if got := readFile(t, rc); got != orig {
				t.Errorf("disable did not round-trip to the original:\n%q\nwant:\n%q", got, orig)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Round 8: quoted data, quoted operators, command spellings, shell validity,
// and pasted transcripts.
// ---------------------------------------------------------------------------

// bashSyntaxOK reports whether bash accepts the file, skipping if bash is absent.
//
// Round 8's MF4 was a file that `bash -n` rejects while disable printed "Disabled."
// and exited 0. Round 7's execution had seen the sibling shape and asserted only that
// bytes changed, which is why the corruption went unnoticed: the assertion has to be
// about the shell's verdict, not about the diff.
func bashSyntaxOK(t *testing.T, path string) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	cmd := exec.Command(bash, "-n", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("bash -n rejected the file after the command ran: %v\n%s\n--- file ---\n%s",
			err, out, readFile(t, path))
	}
}

// TestCommandStarts_QuotedDataHoldsNoCommand is round 8's MF1 at the unit level.
//
// A line inside an unterminated quoted string is data, exactly as heredoc body is.
// Before this, heredocBody modelled only the heredoc, so `MSG="<START>\nalias bob=…\n
// <END>"` had its middle line surgically deleted: bash defined nothing, status claimed
// enabled, and disable reported success while taking the file 122→88 bytes.
func TestCommandStarts_QuotedDataHoldsNoCommand(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
		// dataLines are indexes that must hold no command at all.
		dataLines []int
	}{
		{
			name:      "double quoted",
			lines:     []string{`MSG="` + bobShellMarkerStart, bobShellAliasLine("/x/abctl"), bobShellMarkerEnd + `"`, "export T=1"},
			dataLines: []int{1, 2},
		},
		{
			name:      "single quoted",
			lines:     []string{`MSG='` + bobShellMarkerStart, bobShellAliasLine("/x/abctl"), bobShellMarkerEnd + `'`, "export T=1"},
			dataLines: []int{1, 2},
		},
		{
			name:      "python -c script",
			lines:     []string{`python3 -c "`, bobShellAliasLine("/x/abctl"), "print(1)", `"`},
			dataLines: []int{1, 2},
		},
		{
			name:      "doc string with secrets",
			lines:     []string{`DOC='line1 secret=hunter2`, bobShellAliasLine("/x/abctl"), `line3 token=abc123'`, "export T=1"},
			dataLines: []int{1, 2},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmds := commandStarts(tc.lines)
			for _, i := range tc.dataLines {
				if len(cmds[i].starts) != 0 {
					t.Errorf("line %d (%q) reported a command start %v, want none — it is quoted data",
						i, tc.lines[i], cmds[i].starts)
				}
				if cmds[i].holdsOurAlias() {
					t.Errorf("line %d (%q) claimed as our alias, but it is quoted data", i, tc.lines[i])
				}
			}
			// And the whole-file view agrees: nothing here is owned.
			if got := ownedLines(tc.lines); len(got) != 0 {
				t.Errorf("ownedLines = %v, want none — every candidate line is quoted data", got)
			}
		})
	}
}

// TestBobShell_QuotedDataSurvivesEveryVerb is MF1 end to end, over the real files.
func TestBobShell_QuotedDataSurvivesEveryVerb(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"double quoted", `MSG="` + bobShellMarkerStart + "\n" + bobShellAliasLine("/x/abctl") + "\n" + bobShellMarkerEnd + "\"\nexport T=1\n"},
		{"single quoted", `MSG='` + bobShellMarkerStart + "\n" + bobShellAliasLine("/x/abctl") + "\n" + bobShellMarkerEnd + "'\nexport T=1\n"},
		{"python -c", "python3 -c \"\n" + bobShellAliasLine("/x/abctl") + "\nprint(1)\n\"\n"},
		{"doc with secrets", "DOC='line1 secret=hunter2\n" + bobShellAliasLine("/x/abctl") + "\nline3 token=abc123'\nexport T=1\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withSelfPath(t, "/tmp/abctl")
			rc := filepath.Join(t.TempDir(), ".bashrc")
			if err := os.WriteFile(rc, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			var out, errb bytes.Buffer
			if code := bobShellStatus(rc, &out); code != 0 {
				t.Fatalf("status: exit = %d", code)
			}
			if !strings.Contains(out.String(), "not enabled") {
				t.Errorf("status = %q, want not-enabled: the alias exists only as quoted data", out.String())
			}
			out.Reset()
			if code := bobShellDisable(rc, true, &out, &errb); code != 0 {
				t.Fatalf("disable: exit = %d (stderr: %s)", code, errb.String())
			}
			if !strings.Contains(out.String(), "Nothing to do") {
				t.Errorf("disable said %q, want Nothing to do", out.String())
			}
			if got := readFile(t, rc); got != tc.body {
				t.Errorf("disable edited quoted data.\n got:\n%s\nwant:\n%s", got, tc.body)
			}
		})
	}
}

// TestCommandStarts_QuotedHeredocOperatorIsNotAnOpener is round 8's MF2.
//
// `echo '<<EOF'` was read as an opener, so every following line was classified as
// heredoc body forever: a live alias below it was invisible to status, disable
// reported nothing to do, and enable then wrote a SECOND live alias that status still
// could not see. That refuted the old comment claiming over-detection is "the
// recoverable error" — a suppressed observation propagates into the next write.
func TestCommandStarts_QuotedHeredocOperatorIsNotAnOpener(t *testing.T) {
	for _, tc := range []struct{ name, opener string }{
		{"single quoted", `echo '<<EOF'`},
		{"double quoted", `echo "<<EOF"`},
		{"escaped", `echo \<\<EOF`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := []string{tc.opener, bobShellAliasLine("/x/abctl"), "export T=1"}
			cmds := commandStarts(lines)
			if !cmds[1].holdsOurAlias() {
				t.Errorf("the alias below %q was not seen; a quoted operator was read as a heredoc opener", tc.opener)
			}
			if got := ownedLines(lines); len(got) != 1 || got[0] != 1 {
				t.Errorf("ownedLines = %v, want exactly [1]", got)
			}
		})
	}
	// The two findings compose, and this is the case that needs BOTH halves. A line
	// that begins inside a quote carried from the line above can contain `<<`: it is
	// data, but heredocDelim only knows that if commandStarts passes it the carried
	// quote state. Without it, that `<<` reads as a real opener and every line below —
	// including a live alias bash does define — is suppressed forever. Found by
	// mutation: dropping the quote argument to heredocDelim survived the MF1 and MF2
	// tables, because neither builds a quoted string that mentions a heredoc operator.
	// Two shapes, and only the second reaches heredocDelim's quote argument — worth
	// keeping both, because getting that distinction wrong is what made the first
	// mutation survive a table that looked like it covered this.
	//
	// (a) The quote is still open at end of line, so commandStarts never asks about
	// heredocs at all: the `end == 0` guard carries it.
	stillOpen := []string{
		`MSG="line one`,
		"this line is data and mentions <<EOF",
		`line three"`,
		bobShellAliasLine("/x/abctl"),
		"export T=1",
	}
	if got := ownedLines(stillOpen); len(got) != 1 || got[0] != 3 {
		t.Errorf("ownedLines = %v, want exactly [3]: the `<<EOF` inside a carried quote is data,\nso the live alias below it must still be seen", got)
	}
	// (b) The carried quote CLOSES on the same line, so heredocDelim does run — and it
	// only knows the `<<EOF` was quoted because commandStarts hands it the carried
	// state. Pass 0 there and this `<<EOF` reads as a real opener, swallowing every
	// line below including an alias bash does define.
	closesSameLine := []string{
		`MSG="line one`,
		`still data <<EOF and now the quote closes"`,
		bobShellAliasLine("/x/abctl"),
		"export T=1",
	}
	if got := ownedLines(closesSameLine); len(got) != 1 || got[0] != 2 {
		t.Errorf("ownedLines = %v, want exactly [2]: the `<<EOF` sits inside the carried quote,\nso it opens no heredoc and the live alias below it stands", got)
	}

	// A REAL heredoc still suppresses its body, so the fix did not simply stop
	// tracking heredocs.
	real := []string{"cat <<EOF", bobShellAliasLine("/x/abctl"), "EOF", "export T=1"}
	if got := ownedLines(real); len(got) != 0 {
		t.Errorf("ownedLines over a real heredoc = %v, want none", got)
	}
}

// TestBobShell_QuotedOperatorThenEnableLeavesOneAlias is MF2's consequence: the
// suppressed claim used to propagate into enable, which wrote a second live alias.
func TestBobShell_QuotedOperatorThenEnableLeavesOneAlias(t *testing.T) {
	withSelfPath(t, "/tmp/abctl")
	rc := filepath.Join(t.TempDir(), ".bashrc")
	body := "echo '<<EOF'\n" + bobShellAliasLine("/x/abctl") + "\nexport T=1\n"
	if err := os.WriteFile(rc, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := bobShellEnable(rc, "/tmp/abctl", true, &out, &errb); code != 0 {
		t.Fatalf("enable: exit = %d (stderr: %s)", code, errb.String())
	}
	if n := strings.Count(readFile(t, rc), "alias bob="); n != 1 {
		t.Errorf("after enable the file has %d alias lines, want 1:\n%s", n, readFile(t, rc))
	}
	// And status can see what enable just wrote — the self-check MF2 said was missing.
	out.Reset()
	if code := bobShellStatus(rc, &out); code != 0 {
		t.Fatalf("status: exit = %d", code)
	}
	if strings.Contains(out.String(), "not enabled") {
		t.Errorf("status cannot see the block enable just wrote: %q", out.String())
	}
	bashSyntaxOK(t, rc)
}

// TestCommandIsOurAlias_EverySpelling is round 8's MF3.
//
// Ownership used to pivot on strings.HasPrefix(t, "alias bob="), which is text
// matching in the one place round 7's commit title said it had been removed. Each of
// these is live per bash and was invisible, and each ended with TWO live aliases after
// enable.
func TestCommandIsOurAlias_EverySpelling(t *testing.T) {
	withSelfPath(t, "")
	for _, tc := range []struct {
		name string
		line string
		want bool
	}{
		{"plain", `alias bob='/b/abctl exec -- \bob'`, true},
		{"tab between words", "alias\tbob='/b/abctl exec -- \\bob'", true},
		{"backslash escaped keyword", `\alias bob='/b/abctl exec -- \bob'`, true},
		{"after a separator", `x=1; alias bob='/b/abctl exec -- \bob'`, true},
		{"ansi-c quoting", `alias bob=$'/b/abctl exec -- \\bob'`, true},
		{"quoted keyword", `'alias' bob='/b/abctl exec -- \bob'`, true},
		{"double separator", `alias -- bob='/b/abctl exec -- \bob'`, true},
		{"several aliases in one command", `alias x=1 bob='/b/abctl exec -- \bob'`, true},
		// Still narrow: the discriminator is what the alias INVOKES.
		{"user's own alias", `alias bob='/usr/local/bin/bob --fast'`, false},
		{"a different name", `alias bobby='/b/abctl exec -- \bob'`, false},
		{"proxying something else", `alias bob='/b/abctl exec -- other'`, false},
		{"not an alias command", `echo alias bob='/b/abctl exec -- \bob'`, false},
		{"commented out", `# alias bob='/b/abctl exec -- \bob'`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := aliasRoutesThroughAbctl(tc.line); got != tc.want {
				t.Errorf("aliasRoutesThroughAbctl(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

// TestBobShell_EverySpellingRoundTrips is MF3 end to end: each spelling must be seen
// by status, collapsed to ONE alias by enable, and fully removed by disable.
func TestBobShell_EverySpellingRoundTrips(t *testing.T) {
	for _, tc := range []struct{ name, alias string }{
		{"tab", "alias\tbob='/x/abctl exec -- \\bob'"},
		{"escaped keyword", `\alias bob='/x/abctl exec -- \bob'`},
		{"after separator", `x=1; alias bob='/x/abctl exec -- \bob'`},
		{"ansi-c", `alias bob=$'/x/abctl exec -- \\bob'`},
		{"quoted keyword", `'alias' bob='/x/abctl exec -- \bob'`},
		{"double quoted keyword", `"alias" bob='/x/abctl exec -- \bob'`},
		{"line continuation", "alias \\\n bob='/x/abctl exec -- \\bob'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withSelfPath(t, "/tmp/abctl")
			rc := filepath.Join(t.TempDir(), ".bashrc")
			body := "# rc\n" + tc.alias + "\nexport T=1\n"
			if err := os.WriteFile(rc, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			var out, errb bytes.Buffer
			if code := bobShellStatus(rc, &out); code != 0 {
				t.Fatalf("status: exit = %d", code)
			}
			if strings.Contains(out.String(), "not enabled") {
				t.Errorf("status = %q, want enabled: this spelling is live per bash", out.String())
			}
			out.Reset()
			if code := bobShellEnable(rc, "/tmp/abctl", true, &out, &errb); code != 0 {
				t.Fatalf("enable: exit = %d (stderr: %s)", code, errb.String())
			}
			if n := strings.Count(readFile(t, rc), "bob="); n != 1 {
				t.Errorf("after enable, %d bob= lines, want 1:\n%s", n, readFile(t, rc))
			}
			out.Reset()
			if code := bobShellDisable(rc, true, &out, &errb); code != 0 {
				t.Fatalf("disable: exit = %d (stderr: %s)", code, errb.String())
			}
			if got := readFile(t, rc); strings.Contains(got, "bob=") {
				t.Errorf("disable left an alias behind:\n%s", got)
			}
			bashSyntaxOK(t, rc)
		})
	}
}

// TestBobShellDisable_KeepsCompoundCommandsValid is round 8's MF4.
//
// The guarded-rc idiom puts our alias inside `if [ -n "$PS1" ]; then` / `fi`. Deleting
// the line left `then` immediately followed by `fi` — a file bash refuses to parse,
// while disable printed "Disabled." and exited 0. Every later line in the rc silently
// stops running, surfacing at the next login with nothing tying it to the abctl
// command. Function bodies and loops break identically.
func TestBobShellDisable_KeepsCompoundCommandsValid(t *testing.T) {
	alias := "  " + bobShellAliasLine("/x/abctl")
	for _, tc := range []struct {
		name string
		body string
		// noopNeeded is true when our alias is the construct's ONLY statement, so
		// something has to stay behind to keep it syntactically whole.
		noopNeeded bool
	}{
		{"if/then/fi", "# rc\nif [ -n \"$PS1\" ]; then\n" + alias + "\nfi\nexport T=1\n", true},
		{"function body", "# rc\nmyfn() {\n" + alias + "\n}\nexport T=1\n", true},
		{"for loop", "# rc\nfor i in 1 2; do\n" + alias + "\ndone\nexport T=1\n", true},
		{"while loop", "# rc\nwhile true; do\n" + alias + "\nbreak\ndone\nexport T=1\n", false},
		{"if with a sibling statement", "# rc\nif true; then\n  export INNER=1\n" + alias + "\nfi\nexport T=1\n", false},
		{"file scope needs no noop", "# rc\n" + bobShellAliasLine("/x/abctl") + "\nexport T=1\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withSelfPath(t, "/tmp/abctl")
			rc := filepath.Join(t.TempDir(), ".bashrc")
			if err := os.WriteFile(rc, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			bashSyntaxOK(t, rc) // the fixture itself must be valid, or the test proves nothing
			var out, errb bytes.Buffer
			if code := bobShellDisable(rc, true, &out, &errb); code != 0 {
				t.Fatalf("disable: exit = %d (stderr: %s)", code, errb.String())
			}
			got := readFile(t, rc)
			if strings.Contains(got, "alias bob") {
				t.Errorf("disable left the alias behind:\n%s", got)
			}
			// The whole point: the result is still a file the shell will read.
			bashSyntaxOK(t, rc)
			if gotNoop := strings.Contains(got, ":"); gotNoop != tc.noopNeeded {
				t.Errorf("no-op substitution present = %v, want %v — it must appear only where\ndeletion would empty a construct, or the byte-identical round trip breaks:\n%s",
					gotNoop, tc.noopNeeded, got)
			}
			// And the construct's own lines survive verbatim.
			for _, l := range strings.Split(tc.body, "\n") {
				if l == "" || strings.Contains(l, "alias bob") {
					continue
				}
				if !strings.Contains(got, strings.TrimSpace(l)) {
					t.Errorf("disable lost the user's line %q:\n%s", l, got)
				}
			}
		})
	}
}

// TestBobShell_EnableDisableIsNotACorruptionRoundTrip is MF4's asymmetry: enable
// appends at file scope and stays valid, so a file where enable then disable ran must
// come back byte-identical rather than syntactically broken.
func TestBobShell_EnableDisableIsNotACorruptionRoundTrip(t *testing.T) {
	withSelfPath(t, "/tmp/abctl")
	rc := filepath.Join(t.TempDir(), ".bashrc")
	body := "# rc\nif [ -n \"$PS1\" ]; then\n  export INNER=1\nfi\nexport T=1\n"
	if err := os.WriteFile(rc, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := bobShellEnable(rc, "/tmp/abctl", true, &out, &errb); code != 0 {
		t.Fatalf("enable: exit = %d (stderr: %s)", code, errb.String())
	}
	bashSyntaxOK(t, rc)
	out.Reset()
	if code := bobShellDisable(rc, true, &out, &errb); code != 0 {
		t.Fatalf("disable: exit = %d (stderr: %s)", code, errb.String())
	}
	bashSyntaxOK(t, rc)
	if got := readFile(t, rc); got != body {
		t.Errorf("enable then disable is not an identity.\n got:\n%s\nwant:\n%s", got, body)
	}
}

// TestBobShell_InertPastedTranscriptSurvives is round 8's MF5, and the finding needs
// splitting to state it correctly.
//
// An INDENTED `alias bob=…` is not inert: both bash and zsh define it, so a tool that
// promises exactly one live alias must claim and rewrite it — leaving it would leave
// two. What must survive is a copy the shell genuinely never runs: inside a heredoc,
// commented out, or markers with no alias between them at all. Those are the cases a
// `--help` paste actually produces when it is documentation rather than configuration.
func TestBobShell_InertPastedTranscriptSurvives(t *testing.T) {
	alias := bobShellAliasLine("/x/abctl")
	realAlias := bobShellAliasLine("/real/abctl")
	for _, tc := range []struct {
		name string
		body string
		// hasReal is true when a genuinely live alias sits elsewhere in the file, so
		// the file-wide `any` gate round 7 used would have claimed the paste.
		hasReal bool
	}{
		{
			name:    "paste inside a heredoc",
			body:    "cat <<'EOF'\n" + bobShellMarkerStart + "\n" + alias + "\n" + bobShellMarkerEnd + "\nEOF\n" + realAlias + "\nexport T=1\n",
			hasReal: true,
		},
		{
			name:    "paste commented out",
			body:    "# " + bobShellMarkerStart + "\n# " + alias + "\n# " + bobShellMarkerEnd + "\n" + realAlias + "\nexport T=1\n",
			hasReal: true,
		},
		{
			name: "markers with no alias between them",
			body: "# docs:\n" + bobShellMarkerStart + "\n" + bobShellMarkerEnd + "\nexport T=1\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withSelfPath(t, "/tmp/abctl")
			rc := filepath.Join(t.TempDir(), ".bashrc")
			if err := os.WriteFile(rc, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			var out, errb bytes.Buffer
			if code := bobShellDisable(rc, true, &out, &errb); code != 0 {
				t.Fatalf("disable: exit = %d (stderr: %s)", code, errb.String())
			}
			got := readFile(t, rc)
			// Every line of the paste is still there, verbatim.
			for _, l := range strings.Split(tc.body, "\n") {
				if l == "" || l == realAlias {
					continue
				}
				if !strings.Contains(got, l) {
					t.Errorf("disable destroyed an inert pasted line %q:\n%s", l, got)
				}
			}
			if tc.hasReal && strings.Contains(got, realAlias) {
				t.Errorf("disable left the genuinely live alias behind:\n%s", got)
			}
			if !tc.hasReal && !strings.Contains(out.String(), "Nothing to do") {
				t.Errorf("disable over a documentation-only file said %q, want Nothing to do", out.String())
			}
		})
	}
}

// TestBobShellEnable_PreservesIndentation is the part of MF5 that was a real defect:
// rewriting an indented block flush-left changed lines the user had aligned.
func TestBobShellEnable_PreservesIndentation(t *testing.T) {
	withSelfPath(t, "/tmp/abctl")
	rc := filepath.Join(t.TempDir(), ".bashrc")
	body := "# notes:\n    " + bobShellMarkerStart + "\n    " + bobShellAliasLine("/x/abctl") +
		"\n    " + bobShellMarkerEnd + "\nexport T=1\n"
	if err := os.WriteFile(rc, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := bobShellEnable(rc, "/tmp/abctl", true, &out, &errb); code != 0 {
		t.Fatalf("enable: exit = %d (stderr: %s)", code, errb.String())
	}
	got := readFile(t, rc)
	for _, want := range []string{
		"    " + bobShellMarkerStart,
		"    " + bobShellAliasLine("/tmp/abctl"),
		"    " + bobShellMarkerEnd,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("enable de-indented the block (missing %q):\n%s", want, got)
		}
	}
	if n := strings.Count(got, "alias bob="); n != 1 {
		t.Errorf("after enable, %d alias lines, want 1:\n%s", n, got)
	}
}

// TestScanLine_CommentTerminatesTheLine pins the `#` handling at the level where it is
// actually observable: the command starts themselves.
//
// The MF3 table's "commented out" row does NOT prove this. Mutation testing showed that
// removing scanLine's `#` case leaves that row passing, and so does removing
// splitShellWords' `#` terminator, and so does removing BOTH — because
// commandIsOurAlias independently requires words[0] == "alias", and a consumed `#`
// makes the first word `#` or `#alias`. Three mechanisms answer one question, which is
// the two-predicates shape every round of this review has found. The two scanner-level
// ones are kept because they are correct and cheap, but they are pinned here, where a
// change to them shows up, rather than left looking covered by an end-to-end row that
// would pass without them.
func TestScanLine_CommentTerminatesTheLine(t *testing.T) {
	for _, tc := range []struct {
		line string
		want []int
	}{
		{`# alias bob=x`, nil},
		{`   # alias bob=x`, nil},
		{`echo hi; # alias bob=x`, []int{0}},
		// Mid-word `#` is an ordinary byte in both shells, so the word continues and
		// nothing after it on that word is a new start. bash: `echo a#b; alias bob=…`
		// DOES define the alias, and `echo hi #note; alias bob=…` does not.
		{`x=a#b`, []int{0}},
		{`echo a#b; alias bob=x`, []int{0, 10}},
		// The cases that distinguish word-boundary from command-start. A separator INSIDE
		// a comment must not resurrect scanning: keying the comment on atStart reported a
		// start at the `alias` here, claiming a line bash leaves inert — over-detection,
		// which by MF2's lesson is not the safe direction, since disable would then delete
		// a line the user wrote as a comment. Every expectation below was checked against
		// bash directly.
		{`# note; alias bob=x`, nil},
		{`# a && alias bob=x`, nil},
		{`echo hi # note; alias bob=x`, []int{0}},
		{`echo hi #note; alias bob=x`, []int{0}},
		// And a real separator still starts a real command.
		{`echo hi; alias bob=x`, []int{0, 9}},
		// A `#` immediately after a separator, with no space, still opens a comment —
		// bash leaves all three of these inert — so a separator has to set the
		// word boundary as well as the command boundary.
		{`echo hi;# note; alias bob=x`, []int{0}},
		{`echo hi&&# note; alias bob=x`, []int{0}},
		{`(#note; alias bob=x`, nil},
	} {
		got, _ := scanLine(tc.line, 0)
		if !slices.Equal(got, tc.want) {
			t.Errorf("scanLine(%q) starts = %v, want %v", tc.line, got, tc.want)
		}
	}
}

// TestEndsWithContinuation pins the odd/even backslash rule: an even run escapes
// itself and does not join the next line.
func TestEndsWithContinuation(t *testing.T) {
	for _, tc := range []struct {
		line string
		want bool
	}{
		{`alias \`, true},
		{`alias \\`, false},
		{`alias \\\`, true},
		{`alias`, false},
		{``, false},
	} {
		if got := endsWithContinuation(tc.line); got != tc.want {
			t.Errorf("endsWithContinuation(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}
