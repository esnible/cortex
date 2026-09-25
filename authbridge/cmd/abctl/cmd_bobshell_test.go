package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every enable/disable call here passes yes=true, so the confirmation prompt is never
// reached — the same convention as the claudeCode tests, and for the same reason: a
// test process has no controlling terminal for confirm() to open. The prompt's own
// answer parsing is covered by TestConfirmFrom against a string reader.

func TestBobShellHelp_PrintsUsageOnStdout(t *testing.T) {
	// An answer must be pipeable, so --help goes to stdout and exits 0. A subcommand
	// that printed its own help to stderr would break `abctl configure bobshell
	// --help | less`.
	for _, arg := range []string{"-h", "--help", "help"} {
		t.Run(arg, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := runBobShell([]string{arg}, &out, &errb); code != 0 {
				t.Errorf("exit = %d, want 0", code)
			}
			if !strings.Contains(out.String(), "abctl configure bobshell") {
				t.Errorf("stdout is not the usage text:\n%s", out.String())
			}
			if errb.Len() != 0 {
				t.Errorf("stderr not empty: %q", errb.String())
			}
		})
	}
}

// The usage text must show the block it is describing, since "what will this put in
// my shell startup file" is the question a reader of this help most needs answered
// before they run it.
func TestBobShellHelp_ShowsTheBlock(t *testing.T) {
	var out, errb bytes.Buffer
	runBobShell([]string{"--help"}, &out, &errb)
	got := out.String()
	for _, want := range []string{bobShellMarkerStart, "bob() {", bobShellMarkerEnv} {
		if !strings.Contains(got, want) {
			t.Errorf("usage omits %q:\n%s", want, got)
		}
	}
}

func TestBobShellUsageErrors(t *testing.T) {
	t.Run("no action", func(t *testing.T) {
		var out, errb bytes.Buffer
		if code := runBobShell(nil, &out, &errb); code != 2 {
			t.Errorf("exit = %d, want 2", code)
		}
		if errb.Len() == 0 {
			t.Error("stderr empty; a usage error must say something")
		}
	})

	t.Run("unknown action", func(t *testing.T) {
		var out, errb bytes.Buffer
		if code := runBobShell([]string{"frobnicate"}, &out, &errb); code != 2 {
			t.Errorf("exit = %d, want 2", code)
		}
		got := errb.String()
		// Quoted, so an action carrying a stray shell character stays legible.
		if !strings.Contains(got, `"frobnicate"`) {
			t.Errorf("stderr does not quote the input: %q", got)
		}
		// Naming the valid set is the difference between a refusal and a dead end.
		for _, verb := range []string{"enable", "disable", "status"} {
			if !strings.Contains(got, verb) {
				t.Errorf("stderr omits %q: %q", verb, got)
			}
		}
	})

	// status changes nothing and never prompts, so neither flag means anything there.
	// Accepting them silently would imply an effect that does not exist.
	t.Run("status rejects enable-only flags", func(t *testing.T) {
		for _, arg := range []string{"--yes", "--rc"} {
			var out, errb bytes.Buffer
			if code := runBobShell([]string{"status", arg}, &out, &errb); code != 2 {
				t.Errorf("status %s: exit = %d, want 2", arg, code)
			}
		}
	})
}

func TestBobShellRCPath(t *testing.T) {
	// zsh needs no file to exist: interactive zsh always reads .zshrc and creates it
	// if absent, so the answer is unconditional.
	t.Run("zsh is unconditional", func(t *testing.T) {
		home := t.TempDir()
		got, err := bobShellRCPath("/bin/zsh", home, "darwin")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := filepath.Join(home, ".zshrc"); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	// bash reads a different file depending on platform and login-ness, so an
	// existing file is preferred over creating one the shell may never read. The
	// order matters: .bash_profile wins over .bashrc when both exist.
	t.Run("bash prefers an existing file, in order", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			present []string
			want    string
		}{
			{"all three", []string{".bash_profile", ".bashrc", ".profile"}, ".bash_profile"},
			{"bashrc and profile", []string{".bashrc", ".profile"}, ".bashrc"},
			{"profile only", []string{".profile"}, ".profile"},
			{"bash_profile only", []string{".bash_profile"}, ".bash_profile"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				home := t.TempDir()
				for _, f := range tc.present {
					if err := os.WriteFile(filepath.Join(home, f), nil, 0o644); err != nil {
						t.Fatal(err)
					}
				}
				got, err := bobShellRCPath("/bin/bash", home, "linux")
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if want := filepath.Join(home, tc.want); got != want {
					t.Errorf("got %q, want %q", got, want)
				}
			})
		}
	})

	// With nothing present the platform decides, because that is what bash itself
	// does: macOS terminals start login shells and read .bash_profile.
	t.Run("bash with no file falls back per platform", func(t *testing.T) {
		for goos, want := range map[string]string{"darwin": ".bash_profile", "linux": ".bashrc"} {
			home := t.TempDir()
			got, err := bobShellRCPath("/bin/bash", home, goos)
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", goos, err)
			}
			if w := filepath.Join(home, want); got != w {
				t.Errorf("%s: got %q, want %q", goos, got, w)
			}
		}
	})

	// A shell we do not know is refused, not guessed at: editing the wrong file
	// leaves the user with a function that never appears and a file they did not
	// expect to change. The error must name --rc, or the refusal is a dead end.
	t.Run("unknown shell and unset SHELL are refused, naming --rc", func(t *testing.T) {
		for _, shell := range []string{"", "/usr/bin/fish", "/bin/ksh"} {
			_, err := bobShellRCPath(shell, t.TempDir(), "linux")
			if err == nil {
				t.Errorf("shell %q: expected an error", shell)
				continue
			}
			if !strings.Contains(err.Error(), "--rc") {
				t.Errorf("shell %q: error does not mention --rc: %v", shell, err)
			}
		}
	})
}

// enable is the only verb that writes, so its edits are checked byte-for-byte: an rc
// file is hand-accreted over years and holds things nothing else has a copy of.
func TestBobShellEnable(t *testing.T) {
	t.Run("creates a missing file", func(t *testing.T) {
		rc := filepath.Join(t.TempDir(), ".zshrc")
		var out, errb bytes.Buffer
		if code := bobShellEnable(rc, mustTarget(t, rc), true, &out, &errb); code != 0 {
			t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb.String())
		}
		if got := readFile(t, rc); got != bobShellBlock {
			t.Errorf("content = %q, want exactly the block", got)
		}
	})

	t.Run("appends without disturbing what was there", func(t *testing.T) {
		rc := filepath.Join(t.TempDir(), ".zshrc")
		orig := "export FOO=1\nalias ll='ls -l'\n"
		writeFile(t, rc, orig)
		var out, errb bytes.Buffer
		if code := bobShellEnable(rc, mustTarget(t, rc), true, &out, &errb); code != 0 {
			t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb.String())
		}
		if want := orig + bobShellBlock; readFile(t, rc) != want {
			t.Errorf("content =\n%q\nwant\n%q", readFile(t, rc), want)
		}
	})

	// Without the inserted newline the marker would fuse onto the user's last line,
	// which both corrupts that line and makes the block unmatchable by disable.
	t.Run("adds a newline to a file that lacks one", func(t *testing.T) {
		rc := filepath.Join(t.TempDir(), ".zshrc")
		writeFile(t, rc, "export FOO=1")
		var out, errb bytes.Buffer
		if code := bobShellEnable(rc, mustTarget(t, rc), true, &out, &errb); code != 0 {
			t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb.String())
		}
		got := readFile(t, rc)
		if want := "export FOO=1\n" + bobShellBlock; got != want {
			t.Errorf("content =\n%q\nwant\n%q", got, want)
		}
		if !strings.Contains(got, "\n"+bobShellMarkerStart) {
			t.Error("the marker does not start its own line")
		}
	})

	// Running it twice must not append twice, or disable's exactly-once rule would be
	// broken by the very command that wrote the file.
	t.Run("is idempotent", func(t *testing.T) {
		rc := filepath.Join(t.TempDir(), ".zshrc")
		writeFile(t, rc, "export FOO=1\n")
		var out, errb bytes.Buffer
		bobShellEnable(rc, mustTarget(t, rc), true, &out, &errb)
		first := readFile(t, rc)

		out.Reset()
		if code := bobShellEnable(rc, mustTarget(t, rc), true, &out, &errb); code != 0 {
			t.Fatalf("second enable: exit = %d, want 0", code)
		}
		if got := readFile(t, rc); got != first {
			t.Errorf("second enable changed the file:\n%q", got)
		}
		if !strings.Contains(out.String(), "Already enabled") {
			t.Errorf("second enable does not say it was already enabled: %q", out.String())
		}
		if n := strings.Count(readFile(t, rc), bobShellMarkerStart); n != 1 {
			t.Errorf("marker appears %d times, want 1", n)
		}
	})

	// The backup is the user's undo. Overwriting it on a later run would replace the
	// only pristine copy with one this command had already edited.
	t.Run("backs up once, and the backup stays pristine", func(t *testing.T) {
		rc := filepath.Join(t.TempDir(), ".zshrc")
		orig := "export FOO=1\n"
		writeFile(t, rc, orig)
		var out, errb bytes.Buffer
		bobShellEnable(rc, mustTarget(t, rc), true, &out, &errb)
		if got := readFile(t, rc+".bak"); got != orig {
			t.Errorf("backup = %q, want the original %q", got, orig)
		}
		// A disable-then-enable cycle must not let the backup drift to a version we
		// wrote.
		bobShellDisable(rc, mustTarget(t, rc), true, &out, &errb)
		bobShellEnable(rc, mustTarget(t, rc), true, &out, &errb)
		if got := readFile(t, rc+".bak"); got != orig {
			t.Errorf("backup drifted to %q, want the original %q", got, orig)
		}
	})

	// An rc file is normally world-readable; silently tightening it to 0600 is a
	// change the user did not ask for and might not notice.
	t.Run("preserves the existing mode", func(t *testing.T) {
		rc := filepath.Join(t.TempDir(), ".zshrc")
		writeFile(t, rc, "export FOO=1\n")
		if err := os.Chmod(rc, 0o640); err != nil {
			t.Fatal(err)
		}
		var out, errb bytes.Buffer
		bobShellEnable(rc, mustTarget(t, rc), true, &out, &errb)
		fi, err := os.Stat(rc)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o640 {
			t.Errorf("mode = %o, want 640", got)
		}
	})

	// enable's whole effect is deferred to the next shell, so a run that did not say
	// so would leave the user thinking it had failed.
	t.Run("tells the user to start a new shell", func(t *testing.T) {
		rc := filepath.Join(t.TempDir(), ".zshrc")
		var out, errb bytes.Buffer
		bobShellEnable(rc, mustTarget(t, rc), true, &out, &errb)
		got := out.String()
		if !strings.Contains(got, "source") || !strings.Contains(got, "new terminal") {
			t.Errorf("output does not tell the user how to pick up the change: %q", got)
		}
	})

	// A declined prompt must leave the file untouched. yes=false with no terminal
	// declines, which is the non-interactive path CI takes.
	t.Run("declining writes nothing", func(t *testing.T) {
		rc := filepath.Join(t.TempDir(), ".zshrc")
		orig := "export FOO=1\n"
		writeFile(t, rc, orig)
		var out, errb bytes.Buffer
		if code := bobShellEnable(rc, mustTarget(t, rc), false, &out, &errb); code != exitDeclined {
			t.Errorf("exit = %d, want %d", code, exitDeclined)
		}
		if got := readFile(t, rc); got != orig {
			t.Errorf("file changed despite declining: %q", got)
		}
	})
}

func TestBobShellDisable(t *testing.T) {
	// The round trip is the property that matters most: whatever enable did, disable
	// must undo exactly, leaving no trailing blank line or lost newline behind.
	t.Run("round trip is byte-identical", func(t *testing.T) {
		for _, orig := range []string{
			"export FOO=1\n",
			"export FOO=1\nalias ll='ls -l'\n",
			"export FOO=1",       // no trailing newline
			"",                   // empty file
			"# just a comment\n", // nothing but a comment
		} {
			rc := filepath.Join(t.TempDir(), ".zshrc")
			writeFile(t, rc, orig)
			var out, errb bytes.Buffer
			if code := bobShellEnable(rc, mustTarget(t, rc), true, &out, &errb); code != 0 {
				t.Fatalf("enable: exit = %d (stderr: %s)", code, errb.String())
			}
			if code := bobShellDisable(rc, mustTarget(t, rc), true, &out, &errb); code != 0 {
				t.Fatalf("disable: exit = %d (stderr: %s)", code, errb.String())
			}
			// enable normalises a missing trailing newline, and disable cannot know
			// that was not the user's own text — so that one case round-trips to the
			// normalised form. Every other input must come back untouched.
			want := orig
			if want != "" && !strings.HasSuffix(want, "\n") {
				want += "\n"
			}
			if got := readFile(t, rc); got != want {
				t.Errorf("round trip of %q gave %q", orig, got)
			}
		}
	})

	t.Run("absent block changes nothing", func(t *testing.T) {
		rc := filepath.Join(t.TempDir(), ".zshrc")
		orig := "export FOO=1\n"
		writeFile(t, rc, orig)
		var out, errb bytes.Buffer
		if code := bobShellDisable(rc, mustTarget(t, rc), true, &out, &errb); code != 0 {
			t.Errorf("exit = %d, want 0", code)
		}
		if got := readFile(t, rc); got != orig {
			t.Errorf("file changed: %q", got)
		}
		if !strings.Contains(out.String(), "Not enabled") {
			t.Errorf("output does not say it was not enabled: %q", out.String())
		}
	})

	// A missing file is not an error: "remove the thing" when there is no file to
	// remove it from has already achieved what was asked.
	t.Run("missing file is not an error", func(t *testing.T) {
		rc := filepath.Join(t.TempDir(), "nonexistent")
		var out, errb bytes.Buffer
		if code := bobShellDisable(rc, mustTarget(t, rc), true, &out, &errb); code != 0 {
			t.Errorf("exit = %d, want 0 (stderr: %s)", code, errb.String())
		}
		if fileExists(rc) {
			t.Error("disable created the file")
		}
	})

	// Two copies means a hand-edit or a merge we do not understand. Removing "the"
	// block is then ambiguous, so it removes neither and says why.
	t.Run("two copies changes nothing and says how many", func(t *testing.T) {
		rc := filepath.Join(t.TempDir(), ".zshrc")
		orig := "export FOO=1\n" + bobShellBlock + "export BAR=2\n" + bobShellBlock
		writeFile(t, rc, orig)
		var out, errb bytes.Buffer
		if code := bobShellDisable(rc, mustTarget(t, rc), true, &out, &errb); code != 0 {
			t.Errorf("exit = %d, want 0", code)
		}
		if got := readFile(t, rc); got != orig {
			t.Errorf("file changed: %q", got)
		}
		if got := out.String(); !strings.Contains(got, "2 copies") {
			t.Errorf("output does not report the count: %q", got)
		}
	})

	// An altered block is the user's text now, not ours. This is the case the exact
	// match exists for: a near-miss must not be silently "fixed" by deleting it.
	t.Run("altered block is left alone and reported", func(t *testing.T) {
		rc := filepath.Join(t.TempDir(), ".zshrc")
		altered := strings.Replace(bobShellBlock, "return 127", "return 1", 1)
		if altered == bobShellBlock {
			t.Fatal("test did not actually alter the block")
		}
		orig := "export FOO=1\n" + altered
		writeFile(t, rc, orig)
		var out, errb bytes.Buffer
		if code := bobShellDisable(rc, mustTarget(t, rc), true, &out, &errb); code != 0 {
			t.Errorf("exit = %d, want 0", code)
		}
		if got := readFile(t, rc); got != orig {
			t.Errorf("file changed: %q", got)
		}
		// Whitespace-collapsed before matching: the message wraps across lines, and a
		// substring test against the raw output pins the wrap position rather than the
		// wording. Rewrapping the sentence is not a regression; losing it is.
		if got := strings.Join(strings.Fields(out.String()), " "); !strings.Contains(got, "does not match this version") {
			t.Errorf("output does not explain the near-miss: %q", got)
		}
	})

	// A user's own bob(), written before they ever ran this, carries no marker. It
	// must be neither claimed as ours nor removed.
	t.Run("a user's own bob function is untouched", func(t *testing.T) {
		rc := filepath.Join(t.TempDir(), ".zshrc")
		orig := "bob() {\n  echo my own bob\n}\nalias bob='something else'\n"
		writeFile(t, rc, orig)
		var out, errb bytes.Buffer
		if code := bobShellDisable(rc, mustTarget(t, rc), true, &out, &errb); code != 0 {
			t.Errorf("exit = %d, want 0", code)
		}
		if got := readFile(t, rc); got != orig {
			t.Errorf("file changed: %q", got)
		}
	})

	t.Run("declining writes nothing", func(t *testing.T) {
		rc := filepath.Join(t.TempDir(), ".zshrc")
		var out, errb bytes.Buffer
		bobShellEnable(rc, mustTarget(t, rc), true, &out, &errb)
		before := readFile(t, rc)
		if code := bobShellDisable(rc, mustTarget(t, rc), false, &out, &errb); code != exitDeclined {
			t.Errorf("exit = %d, want %d", code, exitDeclined)
		}
		if got := readFile(t, rc); got != before {
			t.Errorf("file changed despite declining: %q", got)
		}
	})
}

// Symlinks are the case the spec singles out. One hop is the dotfiles-repo setup and
// must be followed, because writing the link itself would REPLACE it with a regular
// file — the repo's tracked copy would never get the block and every later dotfile
// edit would silently stop reaching the shell.
func TestBobShellSymlinks(t *testing.T) {
	t.Run("one hop writes through to the target", func(t *testing.T) {
		dir := t.TempDir()
		real := filepath.Join(dir, "dotfiles", "zshrc")
		if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, real, "export FOO=1\n")
		link := filepath.Join(dir, ".zshrc")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}

		var out, errb bytes.Buffer
		if code := bobShellEnable(link, mustTarget(t, link), true, &out, &errb); code != 0 {
			t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb.String())
		}
		if !strings.Contains(readFile(t, real), bobShellMarkerStart) {
			t.Error("the target file did not receive the block")
		}
		// The link must survive as a link: an atomic rename onto the link path is
		// exactly how it would have been destroyed.
		fi, err := os.Lstat(link)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Error("the symlink was replaced by a regular file")
		}
		// Say so, since the file edited is not the file named.
		if !strings.Contains(out.String(), "symlink") {
			t.Errorf("output does not mention the symlink: %q", out.String())
		}
	})

	t.Run("relative link targets resolve against the link's directory", func(t *testing.T) {
		dir := t.TempDir()
		real := filepath.Join(dir, "zshrc.real")
		writeFile(t, real, "export FOO=1\n")
		link := filepath.Join(dir, ".zshrc")
		if err := os.Symlink("zshrc.real", link); err != nil {
			t.Fatal(err)
		}
		got, hops := rcTarget(link)
		if hops != 1 {
			t.Errorf("hops = %d, want 1", hops)
		}
		if got != real {
			t.Errorf("target = %q, want %q", got, real)
		}
	})

	// The accept/refuse decision at every depth, which the 2-hop case below does not
	// cover on its own: it asserts that a 2-hop chain is refused, but not that a 1-hop
	// chain is still followed or that deeper chains stay refused. 0 and 1 hops must
	// resolve all the way to the real file; 2 and deeper must report >= 2 so callers
	// decline. rcTarget's own loop bound is deliberately not pinned — it stops as soon
	// as it knows the answer is "more than one", so raising it changes no outcome.
	t.Run("hop counting pins the threshold", func(t *testing.T) {
		for chain := 0; chain <= 4; chain++ {
			dir := t.TempDir()
			real := filepath.Join(dir, "real")
			writeFile(t, real, "export FOO=1\n")
			cur := real
			for i := 0; i < chain; i++ {
				next := filepath.Join(dir, "link"+string(rune('a'+i)))
				if err := os.Symlink(cur, next); err != nil {
					t.Fatal(err)
				}
				cur = next
			}
			got, hops := rcTarget(cur)
			switch chain {
			case 0, 1:
				if hops != chain {
					t.Errorf("chain of %d: hops = %d, want %d", chain, hops, chain)
				}
				// Followed all the way, so the file written is the real one.
				if got != real {
					t.Errorf("chain of %d: target = %q, want %q", chain, got, real)
				}
			default:
				// Capped: the loop only has to tell "one" from "more than one", so it
				// stops at 2 rather than measuring the depth.
				if hops < 2 {
					t.Errorf("chain of %d: hops = %d, want >= 2 so callers decline", chain, hops)
				}
			}
		}
	})

	// Two or more hops is stow/chezmoi territory, where guessing which file in the
	// chain the user means is how a working setup gets silently broken.
	t.Run("two hops edits nothing and prints the block", func(t *testing.T) {
		dir := t.TempDir()
		real := filepath.Join(dir, "real")
		writeFile(t, real, "export FOO=1\n")
		mid := filepath.Join(dir, "mid")
		if err := os.Symlink(real, mid); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, ".zshrc")
		if err := os.Symlink(mid, link); err != nil {
			t.Fatal(err)
		}

		for _, verb := range []string{"enable", "disable"} {
			var out, errb bytes.Buffer
			// Driven through runBobShell rather than the verb: the depth refusal is
			// checked there, above both verbs, so that is the only place it happens.
			code := runBobShell([]string{verb, "--rc", link, "--yes"}, &out, &errb)
			// Printing guidance is a success: it did what it could safely do.
			if code != 0 {
				t.Errorf("%s: exit = %d, want 0", verb, code)
			}
			if got := readFile(t, real); got != "export FOO=1\n" {
				t.Errorf("%s: edited the file through a 2-hop chain: %q", verb, got)
			}
			got := out.String()
			if !strings.Contains(got, "Not editing") {
				t.Errorf("%s: output does not say it declined: %q", verb, got)
			}
			// Declining without showing the block would leave the user stuck.
			if !strings.Contains(got, bobShellMarkerStart) {
				t.Errorf("%s: output does not include the block to add by hand: %q", verb, got)
			}
		}
	})
}

// status reads the environment and nothing else — that is what keeps this command
// small, and it is what the spec asks for.
func TestBobShellStatus(t *testing.T) {
	t.Run("set means enabled", func(t *testing.T) {
		t.Setenv(bobShellMarkerEnv, "1")
		var out bytes.Buffer
		if code := bobShellStatus(&out); code != 0 {
			t.Errorf("exit = %d, want 0", code)
		}
		if got := out.String(); !strings.HasPrefix(got, "enabled") {
			t.Errorf("output does not lead with enabled: %q", got)
		}
	})

	t.Run("unset means not enabled, with a way forward", func(t *testing.T) {
		t.Setenv(bobShellMarkerEnv, "")
		var out bytes.Buffer
		if code := bobShellStatus(&out); code != 0 {
			t.Errorf("exit = %d, want 0", code)
		}
		got := out.String()
		if !strings.HasPrefix(got, "not enabled") {
			t.Errorf("output does not lead with not enabled: %q", got)
		}
		if !strings.Contains(got, "enable") {
			t.Errorf("output does not name the command to run: %q", got)
		}
	})

	// The consequence of reading the environment: enable's own shell has not re-read
	// the rc file, so status there says "not enabled". Surprising enough that the
	// output has to explain it, or it reads as a bug.
	t.Run("explains why a shell that predates enable says not enabled", func(t *testing.T) {
		t.Setenv(bobShellMarkerEnv, "")
		var out bytes.Buffer
		bobShellStatus(&out)
		if !strings.Contains(out.String(), "before enable ran") {
			t.Errorf("output does not explain the stale-shell case: %q", out.String())
		}
	})

	// status must never read the rc file, per spec. A file holding the block while
	// the variable is unset is exactly the case that would expose a peek at it.
	t.Run("ignores the rc file entirely", func(t *testing.T) {
		rc := filepath.Join(t.TempDir(), ".zshrc")
		writeFile(t, rc, bobShellBlock)
		t.Setenv(bobShellMarkerEnv, "")
		var out bytes.Buffer
		bobShellStatus(&out)
		if !strings.HasPrefix(out.String(), "not enabled") {
			t.Errorf("status consulted the rc file: %q", out.String())
		}
	})
}

// The block is what gets written into a shell startup file, so a malformed one is a
// broken login shell. These assertions are about the shape the shells require.
func TestBobShellBlock_ShapeIsSafe(t *testing.T) {
	// Marker-fenced, so disable can match it exactly without parsing shell.
	if !strings.HasPrefix(bobShellBlock, bobShellMarkerStart+"\n") {
		t.Error("block does not begin with its start marker on its own line")
	}
	if !strings.HasSuffix(bobShellBlock, bobShellMarkerEnd+"\n") {
		t.Error("block does not end with its end marker and a newline")
	}
	// A trailing newline keeps a later append on its own line.
	if !strings.HasSuffix(bobShellBlock, "\n") {
		t.Error("block does not end with a newline")
	}
	// The recursion guard: the function must reach the binary by resolved path.
	// `\bob` or a bare `bob` here would re-enter the function — the exact bug an
	// earlier draft of this feature carried.
	if strings.Contains(bobShellBlock, `\bob`) {
		t.Error(`block uses \bob, which suppresses alias expansion only and recurses inside a function`)
	}
	if !strings.Contains(bobShellBlock, "whence -p bob") || !strings.Contains(bobShellBlock, "type -P bob") {
		t.Error("block does not resolve bob to a path with both the zsh and bash spellings")
	}
	if !strings.Contains(bobShellBlock, `abctl exec -- "$p"`) {
		t.Error("block does not invoke the resolved path, so it may recurse")
	}
	// Arguments: an alias forwarded them for free, a function must say "$@".
	if !strings.Contains(bobShellBlock, `"$@"`) {
		t.Error(`block does not forward "$@", so user arguments are dropped`)
	}
	// Both lookups silenced, or the other shell's unknown-flag error reaches stderr
	// on every single prompt.
	if strings.Count(bobShellBlock, "2>/dev/null") < 2 {
		t.Error("block does not silence both the zsh and bash path lookups")
	}
	// status can only work if the rc file exports the marker.
	if !strings.Contains(bobShellBlock, "export "+bobShellMarkerEnv+"=1") {
		t.Error("block does not export the marker status reads")
	}
	// The scratch variable must be scoped to the function. Without `local` the
	// assignment is global in both bash and zsh, so every `bob` call silently
	// overwrote whatever the user had in `p` — confirmed by running
	// `p=MINE; bob; echo $p` against the installed block in both shells.
	if !strings.Contains(bobShellBlock, "local p") {
		t.Error("block does not declare p local, so calling bob clobbers the user's p")
	}
	// Ordering matters: `local p` must come before the assignment, or it scopes
	// nothing. Asserted by index rather than by presence, because a block with both
	// lines in the wrong order passes every other check here.
	if i, j := strings.Index(bobShellBlock, "local p"), strings.Index(bobShellBlock, "p=$("); i < 0 || j < 0 || i > j {
		t.Errorf("`local p` (at %d) does not precede the assignment (at %d)", i, j)
	}
}

// readFile is shared with the other tests in this package
// (cmd_config_migrate_test.go:210).

// writeFile seeds an rc file. 0644 because that is what a real shell startup file
// is, and the mode is one of the things enable must preserve.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// The defects a review of this command's first version found, each asserted by the
// fixture that exposed it. Grouped because they share a theme: every one of them was
// a path where the command reported success, or reported the wrong thing, while doing
// something other than what its own help text promises.
func TestBobShellReviewFindings(t *testing.T) {
	// A .bak that is not a regular file used to be indistinguishable from "backup
	// already taken": os.Stat succeeds on a directory, so os.IsNotExist is false, the
	// backup branch was skipped, and the edit proceeded to exit 0 with no backup and
	// no mention of one. Backup-once is the stated reason this command is safe to run
	// on a hand-accreted file, so it must fail rather than quietly not happen.
	t.Run("backup path blocked by a directory refuses the edit", func(t *testing.T) {
		dir := t.TempDir()
		rc := filepath.Join(dir, ".zshrc")
		orig := "export FOO=1\n"
		writeFile(t, rc, orig)
		if err := os.Mkdir(rc+".bak", 0o755); err != nil {
			t.Fatal(err)
		}

		var out, errb bytes.Buffer
		if code := bobShellEnable(rc, mustTarget(t, rc), true, &out, &errb); code == 0 {
			t.Errorf("exit = 0, want non-zero: enable succeeded with no backup")
		}
		// The whole point: the original is untouched, so the user can retry.
		if got := readFile(t, rc); got != orig {
			t.Errorf("file was edited without a backup:\n%s", got)
		}
		if !strings.Contains(errb.String(), ".bak") {
			t.Errorf("stderr does not name the backup path: %q", errb.String())
		}
	})

	// A regular .bak is the backup-once rule working, and must still be left alone.
	// Paired with the case above so a fix for one cannot silently break the other.
	t.Run("existing regular backup is preserved", func(t *testing.T) {
		dir := t.TempDir()
		rc := filepath.Join(dir, ".zshrc")
		writeFile(t, rc, "export FOO=1\n")
		writeFile(t, rc+".bak", "PRISTINE\n")

		var out, errb bytes.Buffer
		if code := bobShellEnable(rc, mustTarget(t, rc), true, &out, &errb); code != 0 {
			t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb.String())
		}
		if got := readFile(t, rc+".bak"); got != "PRISTINE\n" {
			t.Errorf("backup was overwritten: %q", got)
		}
	})

	// A missing parent directory used to reach the write, which creates a sibling
	// .tmp — so the user saw a confident "Add to ...", the whole block, and then
	// `open /nonexistent/deeper/rc.tmp: no such file or directory` at exit 1: an
	// errno naming a path they never typed. It is a usage error (2), caught before
	// anything is printed.
	t.Run("missing parent directory is a usage error", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "nonexistent", "deeper", ".zshrc")
		for _, action := range []string{"enable", "disable"} {
			t.Run(action, func(t *testing.T) {
				var out, errb bytes.Buffer
				if code := runBobShell([]string{action, "--yes", "--rc", missing}, &out, &errb); code != 2 {
					t.Errorf("exit = %d, want 2", code)
				}
				// Nothing on stdout: the block and the "Add to ..." line must not be
				// printed before a refusal.
				if out.Len() != 0 {
					t.Errorf("stdout not empty before a refusal: %q", out.String())
				}
				// The errno spelling is the bug's signature, asserted directly.
				if strings.Contains(errb.String(), ".tmp") {
					t.Errorf("stderr names a .tmp path the user never gave: %q", errb.String())
				}
				if !strings.Contains(errb.String(), "--rc") {
					t.Errorf("stderr does not point at the flag to fix: %q", errb.String())
				}
			})
		}
	})

	// `--rc ""` is what `--rc "$RC_FILE"` expands to when RC_FILE is unset, and it
	// used to fall through to the $SHELL default — writing the user's real startup
	// file when they had named a different one. `*rcPath == ""` cannot tell that from
	// "flag absent"; fs.Visit can.
	t.Run("empty --rc is refused, not defaulted", func(t *testing.T) {
		// A home directory that would be written if the fallback fired, so the
		// assertion is about behaviour and not only about the message.
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("SHELL", "/bin/zsh")

		for _, action := range []string{"enable", "disable"} {
			t.Run(action, func(t *testing.T) {
				var out, errb bytes.Buffer
				if code := runBobShell([]string{action, "--yes", "--rc", ""}, &out, &errb); code != 2 {
					t.Errorf("exit = %d, want 2", code)
				}
				if fileExists(filepath.Join(home, ".zshrc")) {
					t.Error("fell back to the $SHELL default and wrote a file the caller never named")
				}
				if !strings.Contains(errb.String(), "--rc") {
					t.Errorf("stderr does not name the flag: %q", errb.String())
				}
			})
		}
	})

	// Omitting --rc entirely must still reach the default. The guard above keys off
	// fs.Visit, and a guard that also caught the absent case would break the command
	// for everyone who uses it normally.
	t.Run("absent --rc still uses the default", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("SHELL", "/bin/zsh")

		var out, errb bytes.Buffer
		if code := runBobShell([]string{"enable", "--yes"}, &out, &errb); code != 0 {
			t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb.String())
		}
		if !fileExists(filepath.Join(home, ".zshrc")) {
			t.Error("did not write the $SHELL default when --rc was omitted")
		}
	})

	// disable used to headline "Not enabled" over a file holding our markers — while
	// the function was live, CORTEX_BOBSHELL was exported, and `status` in a new
	// shell would say enabled. Two commands, opposite answers, same file.
	t.Run("disable does not claim not-enabled over a hand-edited block", func(t *testing.T) {
		dir := t.TempDir()
		rc := filepath.Join(dir, ".zshrc")
		writeFile(t, rc, "export FOO=1\n")
		var enOut, enErr bytes.Buffer
		if code := bobShellEnable(rc, mustTarget(t, rc), true, &enOut, &enErr); code != 0 {
			t.Fatalf("enable exit = %d (stderr: %s)", code, enErr.String())
		}
		// Hand-edit inside the fence: markers ours, block no longer verbatim.
		edited := strings.Replace(readFile(t, rc), "bob: not found in PATH", "bob missing", 1)
		writeFile(t, rc, edited)

		var out, errb bytes.Buffer
		if code := bobShellDisable(rc, mustTarget(t, rc), true, &out, &errb); code != 0 {
			t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb.String())
		}
		got := out.String()
		// The contradiction, asserted directly: this file's `bob` may well be routed.
		if strings.Contains(got, "Not enabled") {
			t.Errorf("headline still claims the file is not enabled:\n%s", got)
		}
		if !strings.Contains(got, "Not changing "+rc) {
			t.Errorf("output does not say the file was left alone:\n%s", got)
		}
		if !strings.Contains(got, "does not") {
			t.Errorf("output does not explain the version mismatch:\n%s", got)
		}
		// Left alone means left alone.
		if readFile(t, rc) != edited {
			t.Error("the hand-edited block was modified")
		}
	})

	// The plain case must keep the plain message: a file with nothing of ours in it
	// really is "not enabled", and the fix above must not have widened to cover it.
	t.Run("a file with no markers still says not enabled", func(t *testing.T) {
		rc := filepath.Join(t.TempDir(), ".zshrc")
		writeFile(t, rc, "export FOO=1\n")
		var out, errb bytes.Buffer
		if code := bobShellDisable(rc, mustTarget(t, rc), true, &out, &errb); code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		if !strings.Contains(out.String(), "Not enabled") {
			t.Errorf("output does not say it was not enabled: %q", out.String())
		}
	})
}

// The resolved write target for a path, the way runBobShell resolves it before
// calling either verb. Tests that drive a verb directly need the same answer;
// hops is checked here so a fixture that accidentally builds a deep symlink chain
// fails as a broken fixture rather than as the behaviour under test.
func mustTarget(t *testing.T, path string) string {
	t.Helper()
	target, hops := rcTarget(path)
	if hops >= 2 {
		t.Fatalf("fixture path %s is %d symlink hops deep; the verbs are never reached with one", path, hops)
	}
	return target
}

// Round-3 review findings. Both are cases where round 2's fix was in the right
// spirit and the wrong place, or where a guard that now carries weight had nothing
// holding it down.
func TestBobShellReviewFindings3(t *testing.T) {
	// The missing-parent guard from round 2 stat'd the path as TYPED while the write
	// went to the path as RESOLVED, so the defect it was added for still reproduced
	// through the one case the symlink support exists to serve: an rc file that is a
	// symlink whose target's directory is gone. The link's own directory exists, so
	// the guard passed; the write then failed with `open <target>.tmp: no such file
	// or directory` at exit 1, after printing the whole block — an errno naming a
	// .tmp path the user never typed, at the wrong exit code, too late to be useful.
	t.Run("dangling symlink target's missing parent is caught before anything prints", func(t *testing.T) {
		for _, verb := range []string{"enable", "disable"} {
			t.Run(verb, func(t *testing.T) {
				dir := t.TempDir()
				// The link's parent exists; the target's parent does not. That gap is
				// the whole finding.
				missing := filepath.Join(dir, "gone", "deeper", "rc")
				link := filepath.Join(dir, ".zshrc")
				if err := os.Symlink(missing, link); err != nil {
					t.Fatal(err)
				}

				var out, errb bytes.Buffer
				code := runBobShell([]string{verb, "--rc", link, "--yes"}, &out, &errb)
				if code != 2 {
					t.Errorf("exit = %d, want 2 (usage error, not an operational failure)", code)
				}
				// The directory named must be the one that is actually missing — the
				// resolved target's parent. Naming the link's parent would be a lie:
				// it exists.
				if want := filepath.Dir(missing); !strings.Contains(errb.String(), want) {
					t.Errorf("stderr does not name the missing directory %q: %q", want, errb.String())
				}
				// The signature of the bug, asserted directly so a regression is named
				// rather than merely detected.
				if strings.Contains(errb.String(), ".tmp") {
					t.Errorf("stderr leaks the temp path the user never typed: %q", errb.String())
				}
				// Refused before committing to anything. An earlier version printed
				// "Add to ..." and the twelve-line block first, then failed.
				if out.Len() != 0 {
					t.Errorf("printed %d bytes before refusing:\n%s", out.Len(), out.String())
				}
			})
		}
	})

	// enable's mismatched-marker guard had no test, and round 2 made it load-bearing:
	// the answer to "what if the rc file is not a valid script" became "status in a
	// new shell is the check", which only holds if a second enable cannot quietly
	// stack another block on top of a hand-edited one. Without the guard, enable
	// appends: two `bob()` definitions with the later one winning, and two marker
	// pairs — which also puts disable into its >1 refusal, so the file can no longer
	// be cleaned up by this command at all.
	//
	// The disable-side twin is tested twice. This is the enable side.
	t.Run("enable refuses rather than stacking a second block on an altered one", func(t *testing.T) {
		dir := t.TempDir()
		rc := filepath.Join(dir, "rc")
		writeFile(t, rc, "export FOO=1\n")

		var out, errb bytes.Buffer
		if code := bobShellEnable(rc, mustTarget(t, rc), true, &out, &errb); code != 0 {
			t.Fatalf("first enable: exit = %d, want 0", code)
		}
		// Hand-edit inside the block, the way someone tweaking the message would. The
		// markers survive; the body no longer matches bobShellBlock byte-for-byte.
		altered := strings.Replace(readFile(t, rc), "bob: not found in PATH", "bob missing", 1)
		if altered == readFile(t, rc) {
			t.Fatal("fixture did not alter the block")
		}
		writeFile(t, rc, altered)

		out.Reset()
		errb.Reset()
		if code := bobShellEnable(rc, mustTarget(t, rc), true, &out, &errb); code != 0 {
			t.Errorf("second enable: exit = %d, want 0 (declining is a success)", code)
		}
		if got := readFile(t, rc); got != altered {
			t.Errorf("second enable changed the file:\n%s", got)
		}
		// The counts the mutant produces, asserted as counts so the failure says what
		// went wrong rather than dumping two files to diff by eye.
		if n := strings.Count(readFile(t, rc), "\nbob() {"); n != 1 {
			t.Errorf("file holds %d bob() definitions, want 1", n)
		}
		if n := strings.Count(readFile(t, rc), bobShellMarkerStart); n != 1 {
			t.Errorf("file holds %d marker starts, want 1", n)
		}
		// Whitespace-collapsed: the message wraps, and pinning the wrap position is
		// not the point.
		if got := strings.Join(strings.Fields(out.String()), " "); !strings.Contains(got, "does not match this version") {
			t.Errorf("output does not explain the refusal: %q", out.String())
		}
		// Declining must leave the user a way forward, not a dead end.
		if !strings.Contains(out.String(), "remove it first") {
			t.Errorf("output does not say what to do: %q", out.String())
		}
	})
}
