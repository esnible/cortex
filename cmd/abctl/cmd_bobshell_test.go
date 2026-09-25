package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHome points os.UserHomeDir at a scratch directory. USERPROFILE as well as
// HOME, matching userconfig_e2e_test.go — os.UserHomeDir reads whichever the
// platform uses, and a test that sets only one passes on the author's machine.
func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

// The whole of the file-selection policy, pinned as a table. No file need exist:
// this maps a shell name to a path and nothing more.
//
// The two error rows are the load-bearing ones. An unrecognised shell must NOT
// fall back to a guess — writing a block into .bashrc for a fish user puts it in
// a file nothing reads, and the user has no reason to look there.
func TestBobShellRCPath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		shell   string
		want    string // relative to home; "" means expect an error
		wantErr string // substring the error must carry
	}{
		{name: "zsh", shell: "/bin/zsh", want: ".zshrc"},
		{name: "bash", shell: "/bin/bash", want: ".bashrc"},
		// The basename is what decides, so a Homebrew or nix path works.
		{name: "bash from a prefix", shell: "/usr/local/bin/bash", want: ".bashrc"},
		{name: "zsh from a prefix", shell: "/opt/homebrew/bin/zsh", want: ".zshrc"},
		{name: "another shell", shell: "/usr/bin/fish", wantErr: "/usr/bin/fish"},
		{name: "unset", shell: "", wantErr: "$SHELL is not set"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := "/home/someone"
			got, err := bobShellRCPath(tc.shell, home)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("path = %q, want an error", got)
				}
				// The message must name what it could not handle, or the user
				// cannot tell which of their settings to change.
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not mention %q", err, tc.wantErr)
				}
				// An error must not also hand back a path a caller might use.
				if got != "" {
					t.Errorf("error case returned a path anyway: %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if want := filepath.Join(home, tc.want); got != want {
				t.Errorf("path = %q, want %q", got, want)
			}
		})
	}
}

// enable appends. The user's existing content must survive byte-for-byte as a
// prefix — an rc file holds things whose order matters, and rewriting or
// reordering any of it is not what "append a block" means.
func TestBobShellEnableAppends(t *testing.T) {
	home := fakeHome(t)
	t.Setenv("SHELL", "/bin/zsh")
	rc := filepath.Join(home, ".zshrc")
	original := "export EDITOR=vim\nalias ll='ls -l'\n"
	if err := os.WriteFile(rc, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := runBobShell([]string{"enable"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, errb.String())
	}
	if errb.Len() != 0 {
		t.Errorf("stderr not empty: %q", errb.String())
	}

	got, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), original) {
		t.Errorf("existing content not preserved as a prefix:\n%s", got)
	}
	if !strings.Contains(string(got), bobShellBlock) {
		t.Errorf("block not written:\n%s", got)
	}
	// Without this the user enables it, sees nothing happen in the current
	// shell, and concludes it is broken.
	if !strings.Contains(out.String(), "source") {
		t.Errorf("stdout does not tell the user to source the file:\n%s", out.String())
	}
	// The path is the only way a user finds out WHERE it went, which matters most
	// in the macOS-bash case where the file may not be the one their terminal
	// reads.
	if !strings.Contains(out.String(), rc) {
		t.Errorf("stdout does not name the file it wrote:\n%s", out.String())
	}
}

// A file with no trailing newline is the case that silently corrupts: the block's
// first line fuses onto the user's last line, which both breaks that line and
// makes the block unmatchable, so disable could never remove it again.
func TestBobShellEnableAddsMissingNewline(t *testing.T) {
	home := fakeHome(t)
	t.Setenv("SHELL", "/bin/bash")
	rc := filepath.Join(home, ".bashrc")
	if err := os.WriteFile(rc, []byte("export EDITOR=vim"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := runBobShell([]string{"enable"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, errb.String())
	}
	got, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), "export EDITOR=vim\n"+bobShellMarkerStart) {
		t.Errorf("block did not start on its own line:\n%q", got)
	}
	// The property that fusing would have destroyed.
	if !strings.Contains(string(got), bobShellBlock) {
		t.Errorf("block not matchable after the newline fix:\n%q", got)
	}
}

// enable on a file that does not exist creates it. This is the common case on a
// fresh machine, and the one where a missing-file error would be wrong.
func TestBobShellEnableCreatesTheFile(t *testing.T) {
	home := fakeHome(t)
	t.Setenv("SHELL", "/bin/zsh")
	rc := filepath.Join(home, ".zshrc")

	var out, errb bytes.Buffer
	if code := runBobShell([]string{"enable"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, errb.String())
	}
	got, err := os.ReadFile(rc)
	if err != nil {
		t.Fatalf("file not created: %v", err)
	}
	if string(got) != bobShellBlock {
		t.Errorf("content = %q, want just the block", got)
	}
	st, err := os.Stat(rc)
	if err != nil {
		t.Fatal(err)
	}
	// 0644, not 0600: an rc file the user may share across machines should not be
	// created more restrictively than the convention.
	if perm := st.Mode().Perm(); perm != rcFileMode {
		t.Errorf("mode = %v, want %v", perm, rcFileMode)
	}
}

// enable twice must not append twice. Two definitions of the same function is a
// file where the last one silently wins, and disable would then refuse (>1 copy),
// leaving the user unable to undo what enable did.
func TestBobShellEnableIsIdempotent(t *testing.T) {
	home := fakeHome(t)
	t.Setenv("SHELL", "/bin/zsh")
	rc := filepath.Join(home, ".zshrc")

	var out1, errb bytes.Buffer
	if code := runBobShell([]string{"enable"}, &out1, &errb); code != 0 {
		t.Fatalf("first enable: exit = %d; stderr: %s", code, errb.String())
	}
	first, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}

	var out2 bytes.Buffer
	errb.Reset()
	if code := runBobShell([]string{"enable"}, &out2, &errb); code != 0 {
		t.Fatalf("second enable: exit = %d; stderr: %s", code, errb.String())
	}
	second, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}

	if n := strings.Count(string(second), bobShellBlock); n != 1 {
		t.Errorf("block appears %d times, want 1:\n%s", n, second)
	}
	// Byte-identical, not merely "still one copy": a second run must not rewrite
	// the file at all.
	if !bytes.Equal(first, second) {
		t.Errorf("second enable changed the file:\n%q\nvs\n%q", first, second)
	}
	if !strings.Contains(out2.String(), "Already enabled") {
		t.Errorf("second run did not say it was already enabled:\n%s", out2.String())
	}
}

// The load-bearing test: enable then disable must restore the file byte-for-byte.
//
// Asserted as byte equality rather than "the markers are gone", because that is
// the actual promise — disable removes exactly what enable added and nothing
// else. It is also what makes drift between the two unmergeable: any change to
// what enable writes that disable does not match fails here.
func TestBobShellRoundTripIsByteIdentical(t *testing.T) {
	for _, original := range []string{
		"",
		"export EDITOR=vim\n",
		"# leading comment\n\nexport PATH=$PATH:/opt/bin\nalias g=git\n",
	} {
		t.Run("len"+string(rune('0'+len(original)%10)), func(t *testing.T) {
			home := fakeHome(t)
			t.Setenv("SHELL", "/bin/zsh")
			rc := filepath.Join(home, ".zshrc")
			if original != "" {
				if err := os.WriteFile(rc, []byte(original), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			var out, errb bytes.Buffer
			if code := runBobShell([]string{"enable"}, &out, &errb); code != 0 {
				t.Fatalf("enable: exit = %d; stderr: %s", code, errb.String())
			}
			out.Reset()
			errb.Reset()
			if code := runBobShell([]string{"disable"}, &out, &errb); code != 0 {
				t.Fatalf("disable: exit = %d; stderr: %s", code, errb.String())
			}

			got, err := os.ReadFile(rc)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != original {
				t.Errorf("round trip changed the file:\ngot  %q\nwant %q", got, original)
			}
		})
	}
}

func TestBobShellDisable(t *testing.T) {
	for _, tc := range []struct {
		name      string
		content   string // "" means do not create the file
		wantSays  string
		wantFinal string // "" means "expect the block gone"; "=" means unchanged
	}{
		{
			name:      "no file",
			wantSays:  "Not enabled",
			wantFinal: "=",
		},
		{
			name:      "file without the block",
			content:   "export EDITOR=vim\n",
			wantSays:  "Not enabled",
			wantFinal: "=",
		},
		{
			name:      "exactly one copy",
			content:   "export EDITOR=vim\n" + bobShellBlock,
			wantSays:  "Disabled",
			wantFinal: "export EDITOR=vim\n",
		},
		{
			// Removing one of two is a silent half-measure; removing both assumes
			// the extra is ours. Neither is right, so refuse and say so.
			name:      "two copies",
			content:   bobShellBlock + bobShellBlock,
			wantSays:  "2 copies",
			wantFinal: "=",
		},
		{
			// Our markers, someone else's content. Deleting it is not "remove
			// exactly what enable added".
			name:      "hand-edited between the markers",
			content:   bobShellMarkerStart + "\nbob() { echo my own version; }\n" + bobShellMarkerEnd + "\n",
			wantSays:  "edited since it was written",
			wantFinal: "=",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := fakeHome(t)
			t.Setenv("SHELL", "/bin/zsh")
			rc := filepath.Join(home, ".zshrc")
			if tc.content != "" {
				if err := os.WriteFile(rc, []byte(tc.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			var out, errb bytes.Buffer
			// Every one of these is exit 0: a refusal that explains itself is a
			// successful outcome for a command whose job is "or do nothing".
			if code := runBobShell([]string{"disable"}, &out, &errb); code != 0 {
				t.Fatalf("exit = %d, want 0; stderr: %s", code, errb.String())
			}
			if errb.Len() != 0 {
				t.Errorf("stderr not empty: %q", errb.String())
			}
			if !strings.Contains(out.String(), tc.wantSays) {
				t.Errorf("stdout missing %q:\n%s", tc.wantSays, out.String())
			}

			want := tc.wantFinal
			if want == "=" {
				want = tc.content
			}
			got, err := os.ReadFile(rc)
			if err != nil && tc.content != "" {
				t.Fatal(err)
			}
			if string(got) != want {
				t.Errorf("file = %q, want %q", got, want)
			}
		})
	}
}

// status reads the environment and nothing else, and succeeds either way — "not
// enabled" is a report, not a failure, matching claudeCodeStatus.
func TestBobShellStatus(t *testing.T) {
	for _, tc := range []struct {
		name     string
		set      bool
		value    string
		wantSays string
	}{
		{name: "enabled", set: true, value: "1", wantSays: "enabled in this shell"},
		{name: "not enabled", wantSays: "not enabled in this shell"},
		// Any value counts as set. The block writes 1, but a user who exported it
		// by hand should not get a different answer for an equivalent setting.
		{name: "some other value", set: true, value: "yes", wantSays: "enabled in this shell"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(bobShellEnvVar, tc.value)
			} else {
				// t.Setenv restores on cleanup, so this cannot leak either way.
				t.Setenv(bobShellEnvVar, "")
				os.Unsetenv(bobShellEnvVar)
			}
			var out, errb bytes.Buffer
			if code := runBobShell([]string{"status"}, &out, &errb); code != 0 {
				t.Errorf("exit = %d, want 0", code)
			}
			if !strings.Contains(out.String(), tc.wantSays) {
				t.Errorf("stdout missing %q:\n%s", tc.wantSays, out.String())
			}
			// A status a script cannot pipe is half a status.
			if errb.Len() != 0 {
				t.Errorf("stderr not empty: %q", errb.String())
			}
		})
	}
}

// status must not need a home directory, an rc file, or a recognisable $SHELL:
// it answers from the environment, so it has to work where enable would refuse.
func TestBobShellStatusNeedsNothingButTheEnvironment(t *testing.T) {
	fakeHome(t)
	t.Setenv("SHELL", "/usr/bin/fish")
	t.Setenv(bobShellEnvVar, "1")

	var out, errb bytes.Buffer
	if code := runBobShell([]string{"status"}, &out, &errb); code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "enabled in this shell") {
		t.Errorf("status did not answer under an unrecognised shell:\n%s", out.String())
	}
}

// rcTarget's own contract, tested directly because runBobShell can only observe
// it through outcomes. The hop count is the value the caller branches on, and
// EvalSymlinks cannot produce it — which is why this is hand-rolled.
func TestRCTarget(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	one := filepath.Join(dir, "one")
	if err := os.Symlink(plain, one); err != nil {
		t.Fatal(err)
	}
	two := filepath.Join(dir, "two")
	if err := os.Symlink(one, two); err != nil {
		t.Fatal(err)
	}
	// A relative link must resolve against the link's own directory, not the
	// process working directory.
	rel := filepath.Join(dir, "rel")
	if err := os.Symlink("plain", rel); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		path     string
		wantHops int
		wantErr  bool
	}{
		{name: "regular file", path: plain, wantHops: 0},
		{name: "one hop", path: one, wantHops: 1},
		// Counting stops once past the limit; the caller only needs "too deep".
		{name: "two hops", path: two, wantHops: 2},
		{name: "relative link", path: rel, wantHops: 1},
		// Absent is normal — enable creates the file — so it reports 0 hops and
		// an error the caller distinguishes with errors.Is(err, os.ErrNotExist).
		{name: "absent", path: filepath.Join(dir, "missing"), wantHops: 0, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, hops, err := rcTarget(tc.path)
			if hops != tc.wantHops {
				t.Errorf("hops = %d, want %d", hops, tc.wantHops)
			}
			if tc.wantErr != (err != nil) {
				t.Errorf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}

	t.Run("absent is os.ErrNotExist", func(t *testing.T) {
		// The distinction enable depends on: a missing file is a normal state, a
		// dangling link is not, and both arrive as an error from Lstat.
		_, _, err := rcTarget(filepath.Join(dir, "missing"))
		if !os.IsNotExist(err) {
			t.Errorf("err = %v, want a not-exist error", err)
		}
	})
}

// The block's shape, asserted against the specific mistakes a previous attempt
// made. Each of these was a real defect: dialect-specific builtins to resolve the
// binary past the function (dash has neither "whence" nor "type -P", and printed
// prose that the shell then executed), and a variable that leaked into the user's
// interactive shell. None is needed — "abctl exec" execve's into a process that
// never reads this file, so the function cannot recurse.
//
// A test rather than a review note, so reintroducing any of it fails a build.
func TestBobShellBlockShape(t *testing.T) {
	for _, banned := range []string{
		"whence",  // zsh-only
		"type -P", // bash-only
		"command -v",
		"local ", // leaked a variable into the user's shell
		"$(",     // command substitution: nothing here needs to run at source time
		"`",      // the other substitution spelling
		"if ",    // no conditionals: the block must be the same on every machine
	} {
		if strings.Contains(bobShellBlock, banned) {
			t.Errorf("block contains %q — see this test's comment for why not:\n%s", banned, bobShellBlock)
		}
	}

	// The properties disable depends on.
	if !strings.HasPrefix(bobShellBlock, bobShellMarkerStart) {
		t.Errorf("block does not start with the start marker:\n%s", bobShellBlock)
	}
	if !strings.HasSuffix(bobShellBlock, bobShellMarkerEnd+"\n") {
		t.Errorf("block does not end with the end marker and a newline:\n%q", bobShellBlock)
	}
	// Removing the block must not leave a dangling blank region or join two
	// unrelated lines, which a block not ending in a newline would do.
	if !strings.HasSuffix(bobShellBlock, "\n") {
		t.Errorf("block does not end with a newline: %q", bobShellBlock)
	}
	// What the feature actually is. Asserted so a refactor cannot quietly change
	// which command the function runs.
	if !strings.Contains(bobShellBlock, `abctl exec -- bob "$@"`) {
		t.Errorf("block does not run abctl exec -- bob \"$@\":\n%s", bobShellBlock)
	}
	if !strings.Contains(bobShellBlock, "export "+bobShellEnvVar+"=1") {
		t.Errorf("block does not export %s, which is what status reads:\n%s", bobShellEnvVar, bobShellBlock)
	}
}

// An unrecognised shell must write NOTHING and print the block. Writing a guess
// puts the block in a file the user's shell never reads, and they have no reason
// to look there to find out why "bob" does not work.
func TestBobShellUnknownShell(t *testing.T) {
	home := fakeHome(t)
	t.Setenv("SHELL", "/usr/bin/fish")

	for _, action := range []string{"enable", "disable"} {
		t.Run(action, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := runBobShell([]string{action}, &out, &errb); code != 0 {
				t.Fatalf("exit = %d, want 0; stderr: %s", code, errb.String())
			}
			// Nothing at all in the home directory: not .bashrc, not .zshrc, and
			// no leftover temp file from a write that was attempted and undone.
			entries, err := os.ReadDir(home)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				names := make([]string, 0, len(entries))
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Errorf("created files under an unrecognised shell: %v", names)
			}
			if !strings.Contains(out.String(), bobShellBlock) {
				t.Errorf("stdout does not carry the block to paste:\n%s", out.String())
			}
			// Name the shell, so the user can see which setting produced this.
			if !strings.Contains(out.String(), "/usr/bin/fish") {
				t.Errorf("stdout does not name $SHELL:\n%s", out.String())
			}
		})
	}
}

// Usage errors, and the stdout/stderr split they turn on: explicit help is a
// successful answer (stdout, 0); a missing or misspelled action is an error
// (stderr, 2). Same shape as TestClaudeCodeHelp_PrintsUsageOnStdout, and for the
// same reason — `--help` read as an action name sends someone looking for the
// command list to the one branch that refuses to print it.
func TestBobShellHelpAndUsageErrors(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		t.Run("help "+arg, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := runBobShell([]string{arg}, &out, &errb); code != 0 {
				t.Errorf("exit = %d, want 0", code)
			}
			if !strings.Contains(out.String(), "abctl configure bobshell —") {
				t.Errorf("usage not on stdout:\n%s", out.String())
			}
			if strings.Contains(out.String()+errb.String(), "unknown bobshell action") {
				t.Errorf("help was read as an action name:\n%s%s", out.String(), errb.String())
			}
			// Explicit help is a successful answer, so it must be pipeable.
			if errb.Len() != 0 {
				t.Errorf("stderr not empty: %q", errb.String())
			}
		})
	}

	t.Run("no action", func(t *testing.T) {
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

	t.Run("misspelled action", func(t *testing.T) {
		fakeHome(t)
		t.Setenv("SHELL", "/bin/zsh")
		var out, errb bytes.Buffer
		// A genuine typo must still be an error — the help case above must not
		// have widened into swallowing every unrecognised action.
		if code := runBobShell([]string{"enabel"}, &out, &errb); code != 2 {
			t.Errorf("exit = %d, want 2", code)
		}
		// Quoted, so an action carrying a stray shell character is legible.
		if !strings.Contains(errb.String(), `"enabel"`) {
			t.Errorf("stderr does not quote the input: %q", errb.String())
		}
		// Naming the valid set is the difference between a refusal and a dead end.
		if !strings.Contains(errb.String(), "enable, disable, status") {
			t.Errorf("stderr does not name the valid actions: %q", errb.String())
		}
	})

	// A bad verb must be refused whatever the environment looks like.
	//
	// The subtest above pins SHELL=/bin/zsh, and that is exactly why the bug this
	// covers survived: three steps between the help switch and the old
	// action check answer successfully on their own — an unrecognised $SHELL, a
	// dangling rc symlink, and a chain past the hop limit each print the block and
	// return 0. With the check at the bottom, `bobshell enabel` under fish printed
	// the block and exited 0, reporting success for a verb that does not exist.
	//
	// Each row below reaches a different one of those early returns, so a future
	// early return that forgets to validate first fails here rather than shipping.
	t.Run("misspelled action beats every early return", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			setup func(t *testing.T, home string)
			shell string
		}{
			{name: "unrecognised shell", shell: "/usr/bin/fish"},
			{name: "shell unset", shell: ""},
			{
				name:  "dangling rc symlink",
				shell: "/bin/zsh",
				setup: func(t *testing.T, home string) {
					if err := os.Symlink(filepath.Join(home, "nowhere"), filepath.Join(home, ".zshrc")); err != nil {
						t.Fatal(err)
					}
				},
			},
			{
				name:  "rc symlink past the hop limit",
				shell: "/bin/zsh",
				setup: func(t *testing.T, home string) {
					dir := t.TempDir()
					real := filepath.Join(dir, "zshrc")
					if err := os.WriteFile(real, []byte("export EDITOR=vim\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					mid := filepath.Join(dir, "middle")
					if err := os.Symlink(real, mid); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(mid, filepath.Join(home, ".zshrc")); err != nil {
						t.Fatal(err)
					}
				},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				home := fakeHome(t)
				t.Setenv("SHELL", tc.shell)
				if tc.setup != nil {
					tc.setup(t, home)
				}

				var out, errb bytes.Buffer
				if code := runBobShell([]string{"enabel"}, &out, &errb); code != 2 {
					t.Errorf("exit = %d, want 2", code)
				}
				if !strings.Contains(errb.String(), `"enabel"`) {
					t.Errorf("stderr does not name the bad action: %q", errb.String())
				}
				// The signature of the bug: advice printed for a verb that does not
				// exist. A refusal must not look like a successful answer.
				if strings.Contains(out.String(), bobShellBlock) {
					t.Errorf("printed the block for an unknown action:\n%s", out.String())
				}
			})
		}
	})
}
