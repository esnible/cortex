package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Bob Shell has no settings file, so its persistence surface is the user's own
// shell startup file: `abctl configure bobshell` writes an alias, so the plain
// `bob` the user types routes through Cortex in every future interactive shell.
// That is what `configure claude-code enable` achieves for `claude`, by a
// different route.
//
// The markers delimit the block this command owns. Everything outside them is the
// user's, and a rewrite must leave it byte-identical — an rc file is hand-written
// over years and holds things nothing else has a copy of. One exception, in
// writeRC: a file that did not end in a newline gains one, because an unterminated
// end marker swallows whatever is appended next and makes the block unfindable by
// disable. Adding the missing byte is the lesser harm, and disable still restores
// everything else exactly. They also make `status`
// and `disable` exact: both look for this block rather than pattern-matching a
// line that happens to mention bob, so a user's own `alias bob=...` written before
// they ever ran this is neither reported as ours nor removed as ours.
const (
	bobShellMarkerStart = "# >>> cortex abctl (bobshell) >>>"
	bobShellMarkerEnd   = "# <<< cortex abctl (bobshell) <<<"
)

const bobShellUsage = `abctl configure bobshell — route Bob Shell through Cortex from any new shell

Usage:
  abctl configure bobshell enable  [--yes] [--rc PATH]
  abctl configure bobshell disable [--yes] [--rc PATH]
  abctl configure bobshell status  [--rc PATH]

enable adds three lines to your shell startup file — a marker comment, the alias,
and a closing marker:

  ` + bobShellMarkerStart + `
  alias bob='/path/to/abctl exec -- \bob'
  ` + bobShellMarkerEnd + `

Afterwards, plain "bob" in a new interactive shell goes through Cortex. Nothing
else in the file changes, and the first run copies the original to <file>.bak and
never overwrites that copy.

The backslash in \bob is load-bearing: it suppresses alias expansion on that one
word, so the alias runs the real bob from your PATH instead of calling itself.

Bob Shell is a command, so unlike Claude Code it has no settings file to write —
which is why this configures the shell instead. The trade-off is scope: an alias
reaches interactive shells, not scripts or other programs that run bob directly.
For those, "abctl exec -- bob" is still the answer and needs no configuration.

Which file: from $SHELL. zsh gets ~/.zshrc; bash gets whichever of
~/.bash_profile, ~/.bashrc or ~/.profile already exists, since which one bash
reads depends on the platform and on whether the shell is a login shell. Any
other shell is refused rather than guessed at — pass --rc to name the file
yourself.

An alias applies to shells started after the edit, so enable tells you to source
the file for the one you are in, and disable tells you to unalias — removing the
line cannot reach a shell that has already read it.

Cortex need not be running for any of this: the alias resolves the proxy address
when you run bob, not now.

Exit status: 0 applied or already correct, 2 a usage error, 3 declined (or no
terminal to ask on), 1 something went wrong.

Flags:
  --yes           do not prompt for confirmation (enable and disable; status
                  changes nothing and never prompts)
  --rc PATH       shell startup file to edit (default: from $SHELL)
`

func runBobShell(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, bobShellUsage)
		return 2
	}
	action := args[0]
	// Answered before the flag set is built, for the reason spelled out in
	// runClaudeCode: --help asks for the whole command's usage, and a flag set named
	// "bobshell --help" would print only the flags of an action that does not exist.
	// Explicit help is a successful answer, so stdout and 0; a bad action is an
	// error, so stderr and 2.
	switch action {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, bobShellUsage)
		return 0
	}

	fs := flag.NewFlagSet("bobshell "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	yes := fs.Bool("yes", false, "do not prompt for confirmation")
	rcPath := fs.String("rc", "", "shell startup file to edit")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	if *rcPath == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			fmt.Fprintf(stderr, "abctl: cannot determine your home directory: %v\n", err)
			return 1
		}
		p, rerr := bobShellRCPath(os.Getenv("SHELL"), home, runtime.GOOS)
		if rerr != nil {
			fmt.Fprintf(stderr, "abctl: %v\n", rerr)
			return 1
		}
		*rcPath = p
	}

	switch action {
	case "enable":
		self, err := abctlPath()
		if err != nil {
			fmt.Fprintf(stderr, "abctl: %v\n", err)
			return 1
		}
		return bobShellEnable(*rcPath, self, *yes, stdout, stderr)
	case "disable":
		return bobShellDisable(*rcPath, *yes, stdout, stderr)
	case "status":
		return bobShellStatus(*rcPath, stdout)
	default:
		fmt.Fprintf(stderr, "abctl: unknown bobshell action %q (enable, disable, status)\n", action)
		fmt.Fprint(stderr, bobShellUsage)
		return 2
	}
}

// confirmFn is confirm, indirected so tests can drive the decline path.
//
// The direct call could not be tested: confirm opens /dev/tty, which resolves to
// the developer's terminal whenever `go test` runs from an interactive shell, so a
// test reaching it blocks on a read — and passed only in a sandbox with no tty,
// where the open fails and it declines by itself. A test that passes for the
// absence of a terminal is asserting the environment, not the code.
var confirmFn = confirm

// bobShellRCPath picks the startup file to edit, given $SHELL, the home directory and
// the platform. Ported from install.sh's offer_path_setup, which faces the same
// question for its PATH line; the reasoning there applies unchanged.
//
// Takes its inputs as parameters rather than reading the environment so the table
// of cases is testable without mutating process state.
func bobShellRCPath(shell, home, goos string) (string, error) {
	switch filepath.Base(shell) {
	case "zsh":
		// Unconditional: interactive zsh always reads .zshrc, creating it if need be.
		// (.zshenv and .zprofile are the wrong files for an alias — the first is read
		// by non-interactive shells too, the second only by login shells.)
		return filepath.Join(home, ".zshrc"), nil
	case "bash":
		// bash reads .bash_profile for login shells on macOS and .bashrc elsewhere,
		// and .profile when neither exists — so prefer whichever file is already
		// there over creating one the shell may never read.
		for _, c := range []string{".bash_profile", ".bashrc", ".profile"} {
			if p := filepath.Join(home, c); fileExists(p) {
				return p, nil
			}
		}
		if goos == "darwin" {
			return filepath.Join(home, ".bash_profile"), nil
		}
		return filepath.Join(home, ".bashrc"), nil
	}
	// An unknown shell is refused, not guessed: we do not know which file it reads,
	// and editing the wrong one leaves the user with an alias that never appears and
	// a modified file they did not expect.
	if shell == "" {
		return "", fmt.Errorf("$SHELL is not set, so there is no way to tell which startup file your shell reads; pass --rc PATH to name it")
	}
	return "", fmt.Errorf("unsupported shell %q: only zsh and bash startup files are known; pass --rc PATH to name yours", shell)
}

// abctlPath returns the absolute path of the running abctl, for the alias body.
//
// An absolute path, not the bare word: the alias outlives this process, and a
// later PATH change must not silently repoint it at a different abctl. Same
// reasoning as resolveServicePaths, minus its sibling search — this binary IS
// the one wanted, so os.Executable is the first and best answer rather than a
// hint about where a companion lives.
func abctlPath() (string, error) {
	self, err := os.Executable()
	if err != nil {
		// A platform where os.Executable fails; PATH is all that is left. This can
		// resolve a DIFFERENT abctl than the one running, which is the outcome the
		// comment above says must not happen — accepted only because the alternative
		// on such a platform is refusing to work at all, and os.Executable failing
		// is not reachable on the platforms this ships to.
		found, lerr := exec.LookPath("abctl")
		if lerr != nil {
			return "", fmt.Errorf("cannot determine the path of this abctl (%v) and it is not on PATH (%v)", err, lerr)
		}
		self = found
	}
	abs, err := filepath.Abs(self)
	if err != nil {
		return "", fmt.Errorf("cannot make %s absolute: %w", self, err)
	}
	if err := validateAliasPath(abs); err != nil {
		return "", err
	}
	return abs, nil
}

// validateAliasPath rejects a path that cannot be written into the alias body.
//
// Split from abctlPath so it is testable without a binary at an exotic path: the
// failure it prevents is a malformed line in the user's rc file rather than an
// error they would see.
//
// The body is single-quoted, so a path containing a single quote would end the
// quoting mid-word. The shell's escape for it (close-quote, backslash-quote,
// reopen) is possible, but the result is a path no one should have and a line no
// one can read; refuse and let --rc-style manual setup handle the exotic case.
func validateAliasPath(abs string) error {
	if strings.Contains(abs, "'") {
		return fmt.Errorf("the path of this abctl contains a single quote (%s), which cannot be written into a shell alias; move or symlink it somewhere without one", abs)
	}
	return nil
}

// bobShellAliasLine renders the alias itself — the one line of the block that does
// the work, split out so status can compare against it by equality instead of asking
// whether the whole rendered block contains what it found.
func bobShellAliasLine(abctlPath string) string {
	return "alias bob='" + abctlPath + " exec -- \\bob'"
}

// bobShellBlock renders the managed block, newline-terminated.
//
// The alias body is single-quoted so nothing in it is expanded when the rc file is
// read — in particular the backslash before bob, which is what stops the alias
// recursing into itself.
func bobShellBlock(abctlPath string) string {
	return bobShellMarkerStart + "\n" +
		bobShellAliasLine(abctlPath) + "\n" +
		bobShellMarkerEnd + "\n"
}

// findBobShellBlock locates the managed block in lines, returning the half-open range
// [start, end) that covers it, markers included.
//
// An unterminated start marker — someone deleted the end marker by hand — reports
// just the start line, so a rewrite replaces it rather than silently appending a
// second block below the first.
func findBobShellBlock(lines []string) (start, end int, found bool) {
	start = -1
	for i, l := range lines {
		switch strings.TrimSpace(l) {
		case bobShellMarkerStart:
			if start == -1 {
				start = i
			}
		case bobShellMarkerEnd:
			if start != -1 {
				return start, i + 1, true
			}
		}
	}
	if start != -1 {
		return start, start + 1, true
	}
	return -1, -1, false
}

// bobShellAliasIn returns the alias line inside the managed block, or "" if there is no
// block. Used by status to report a block that names a different abctl.
func bobShellAliasIn(lines []string) string {
	start, end, found := findBobShellBlock(lines)
	if !found {
		return ""
	}
	for _, l := range lines[start:end] {
		if t := strings.TrimSpace(l); strings.HasPrefix(t, "alias bob=") {
			return t
		}
	}
	return ""
}

// replaceBobShellBlock puts block where the managed block is, or appends it.
//
// Appending adds no separator line. A cosmetic blank ahead of the block would have
// to be removed again on disable to keep the round trip byte-identical, and "remove
// the blank before the block" cannot distinguish the one enable added from one the
// user already had — so a file ending blank lost the user's line. Adding nothing
// makes the round trip an unconditional identity, which is the invariant this file
// states at the top; the block simply sits against the preceding line.
func replaceBobShellBlock(lines []string, block string) []string {
	blockLines := strings.Split(strings.TrimSuffix(block, "\n"), "\n")
	if start, end, found := findBobShellBlock(lines); found {
		out := make([]string, 0, len(lines)-(end-start)+len(blockLines))
		out = append(out, lines[:start]...)
		out = append(out, blockLines...)
		out = append(out, lines[end:]...)
		return out
	}
	out := append([]string(nil), lines...)
	return append(out, blockLines...)
}

// removeBobShellBlock drops the managed block, reporting whether there was one.
//
// Exactly the block, and nothing adjacent to it: enable appends no separator, so
// there is none to reclaim, and a blank line before the block is the user's.
func removeBobShellBlock(lines []string) ([]string, bool) {
	start, end, found := findBobShellBlock(lines)
	if !found {
		return lines, false
	}
	out := make([]string, 0, len(lines)-(end-start))
	out = append(out, lines[:start]...)
	return append(out, lines[end:]...), true
}

// readRC reads the file into lines, plus whether it ended with a newline so a
// rewrite can preserve that. A missing file reads as empty: enable may create it.
func readRC(path string) (lines []string, trailingNewline bool, err error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		if os.IsNotExist(err) {
			return nil, true, nil
		}
		return nil, false, err
	}
	if len(b) == 0 {
		return nil, true, nil
	}
	s := string(b)
	trailingNewline = strings.HasSuffix(s, "\n")
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n"), trailingNewline, nil
}

// writeRC backs the file up once, then replaces it atomically.
//
// The same discipline as writeSettings, and the backup-once rule matters more
// here: an rc file is accreted by hand over years, and overwriting the backup on a
// second run would replace the only pristine copy with one this command had
// already edited. (install.sh's cp does overwrite, which is the bug this avoids.)
// The file's existing mode is preserved — an rc file is commonly 0644 and silently
// tightening it to 0600 is a change the user did not ask for.
func writeRC(path string, lines []string, trailingNewline bool) error {
	body := strings.Join(lines, "\n")
	// len(lines), not body != "": a file holding exactly one newline is one empty
	// line, and joins to the same "" an empty file does. Testing the body suppressed
	// the newline for both, so disable wrote that file as 0 bytes and ate the user's
	// only byte — the round trip this file's header promises is byte-identical.
	if len(lines) > 0 && (trailingNewline || strings.HasSuffix(body, bobShellMarkerEnd)) {
		// A file that did not end with a newline gets one anyway when our block is
		// the new tail, because the end marker MUST be a complete line. Left
		// unterminated, the next thing appended to the rc file fuses onto it and is
		// swallowed by the comment — a `#`-prefixed line is what the marker is —
		// and `disable` can no longer match the marker, so it reports success,
		// exits 0, and leaves the live alias in place. That is a worse outcome than
		// adding the byte the file was missing, and it is not a round-trip
		// violation: disable removes our lines and restores the original tail.
		body += "\n"
	}
	// Resolve a symlink and write through to its target. An rc file is very often a
	// link into a dotfiles repo, and os.Rename over the link REPLACES it with a
	// regular file: the alias lands in a file the repo does not track, the repo's
	// own copy never gets it, and every later dotfile edit silently stops reaching
	// the shell. Backing up beside the link rather than the target has the same
	// shape of wrongness, so resolve before the backup so both land on the real
	// file. (writeSettings does not do this because settings.json is rarely
	// symlinked; rc files are symlinked far more often.)
	if resolved, rerr := filepath.EvalSymlinks(path); rerr == nil {
		path = resolved
	}
	mode := os.FileMode(0o600)
	if cur, rerr := os.ReadFile(path); rerr == nil { //nolint:gosec // operator-supplied path
		if fi, serr := os.Stat(path); serr == nil {
			mode = fi.Mode().Perm()
		}
		bak := path + ".bak"
		if _, serr := os.Stat(bak); os.IsNotExist(serr) {
			if werr := os.WriteFile(bak, cur, mode); werr != nil {
				return fmt.Errorf("writing backup %s: %w", bak, werr)
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func bobShellEnable(rcPath, abctlPath string, yes bool, stdout, stderr io.Writer) int {
	lines, trailingNewline, err := readRC(rcPath)
	if err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}
	block := bobShellBlock(abctlPath)
	updated := replaceBobShellBlock(lines, block)
	if strings.Join(updated, "\n") == strings.Join(lines, "\n") {
		// Already exactly right: say so and write nothing. Rewriting an unchanged
		// file would churn its mtime and, worse, is where a backup-overwrite bug
		// would do its damage.
		fmt.Fprintf(stdout, "Already enabled: %s aliases bob through Cortex.\n", rcPath)
		return 0
	}

	fmt.Fprintf(stdout, "Adds to %s:\n%s\n", rcPath, indentBlock(block))
	fmt.Fprintf(stdout, "Nothing else in the file changes; a copy is kept as %s.bak\n\n", rcPath)
	if !yes && !confirmFn(stdout) {
		fmt.Fprintln(stdout, "Not changed.")
		return exitDeclined
	}

	if err := writeRC(rcPath, updated, trailingNewline); err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}
	// An alias is read at shell startup, so the edit reaches new shells only. Saying
	// so, and naming the command for this one, is the difference between "it worked"
	// and "it did nothing".
	fmt.Fprintf(stdout, "Enabled — new shells route `bob` through Cortex.\n")
	fmt.Fprintf(stdout, "For this one:  source %s\n", rcPath)
	return 0
}

func bobShellDisable(rcPath string, yes bool, stdout, stderr io.Writer) int {
	lines, trailingNewline, err := readRC(rcPath)
	if err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}
	updated, found := removeBobShellBlock(lines)
	if !found {
		fmt.Fprintf(stdout, "Nothing to do: no Cortex bob alias in %s.\n", rcPath)
		return 0
	}
	fmt.Fprintf(stdout, "Removes from %s:\n%s\n", rcPath, indentBlock(bobShellAliasIn(lines)+"\n"))
	if !yes && !confirmFn(stdout) {
		fmt.Fprintln(stdout, "Not changed.")
		return exitDeclined
	}
	if err := writeRC(rcPath, updated, trailingNewline); err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}
	// Removing the line cannot reach a shell that already read it: this one still
	// has the alias until told otherwise.
	fmt.Fprintf(stdout, "Disabled. New shells run `bob` directly.\n")
	fmt.Fprintln(stdout, "For this one:  unalias bob")
	return 0
}

// bobShellStatus reports whether the alias is installed. Always exit 0, stdout only:
// "not enabled" is an answer, not a failure, and an unreadable file is one more
// way of not being enabled.
func bobShellStatus(rcPath string, stdout io.Writer) int {
	lines, _, err := readRC(rcPath)
	if err != nil {
		fmt.Fprintf(stdout, "not enabled (%v)\n", err)
		return 0
	}
	alias := bobShellAliasIn(lines)
	if alias == "" {
		if _, _, found := findBobShellBlock(lines); found {
			// Markers but no alias: a hand-edit gutted the block. Enable repairs it.
			fmt.Fprintf(stdout, "not enabled in %s (the Cortex block is there but has no alias line)\n", rcPath)
			return 0
		}
		fmt.Fprintf(stdout, "not enabled in %s\n", rcPath)
		return 0
	}
	fmt.Fprintf(stdout, "  %s\n", alias)
	// A block naming an abctl that is no longer where it was — reinstalled
	// elsewhere, or moved — aliases bob to a path that may not exist. Enable
	// rewrites it, and saying so here is cheaper than debugging it from the
	// "command not found" the alias itself would produce.
	if self, serr := abctlPath(); serr == nil {
		// Equality, not Contains: an alias line truncated by a hand-edit is a
		// substring of the correct one, and would have reported as matching while
		// the shell saw a broken alias.
		if alias != bobShellAliasLine(self) {
			fmt.Fprintf(stdout, "enabled in %s, but the alias names a different abctl than this one (%s)\n", rcPath, self)
			fmt.Fprintln(stdout, "  Re-run `abctl configure bobshell enable` to point it here.")
			return 0
		}
	}
	fmt.Fprintf(stdout, "enabled in %s\n", rcPath)
	return 0
}

// indentBlock indents each line by two spaces, for quoting file content inside a
// message. Matches how claudeCodeEnable2 presents the keys it is about to add.
func indentBlock(block string) string {
	lines := strings.Split(strings.TrimSuffix(block, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n") + "\n"
}
