package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Bob Shell has no settings file, so its persistence surface is the user's own
// shell startup file: `abctl configure bobshell enable` defines a `bob` shell
// function, so the plain `bob` the user types routes through Cortex in every
// future interactive shell. That is what `configure claude-code enable` achieves
// for `claude`, by a different route.
//
// A FUNCTION, not an alias, and the choice is load-bearing twice over:
//
//   - An alias is invisible to a child process, so nothing written as an alias can
//     ever be observed by abctl. A function can export a variable, which is what
//     makes `status` possible at all.
//   - A function can reach the real binary without re-entering itself. An alias
//     needs `\bob` for that, and `\bob` DOES NOT WORK inside a function: the
//     backslash suppresses alias expansion only, so it finds the function again and
//     recurses. Resolving `bob` to an absolute path first cannot recurse, because
//     an absolute path never matches a function name.
//
// The markers delimit the block this command owns. Everything outside them is the
// user's, and disable must leave it byte-identical — an rc file is hand-written over
// years and holds things nothing else has a copy of. They are also what makes
// disable exact: it matches this block verbatim rather than pattern-matching a line
// that happens to mention bob, so a user's own `bob()` written before they ever ran
// this is neither claimed as ours nor removed as ours.
const (
	bobShellMarkerStart = "# >>> cortex abctl (bobshell) >>>"
	bobShellMarkerEnd   = "# <<< cortex abctl (bobshell) <<<"
)

// bobShellMarker is the environment variable enable exports and status reads.
//
// status reads THIS and nothing else — not the rc file, not a spawned shell. That
// is what keeps this command small: recognising our own text inside a shell startup
// file is a shell-parsing problem (quoting, heredocs, compound commands, comments),
// and reading one variable is not a problem at all.
//
// What it proves is that the rc file RAN, not that the function is intact. A user
// who later defines their own `bob`, or who deletes the function body and leaves the
// export line, still reads as enabled. Accepted: the alternative is parsing the file,
// which is the complexity this design exists to avoid.
const bobShellMarkerEnv = "CORTEX_BOBSHELL"

// bobShellBlock is exactly what enable appends and disable removes.
//
// One fixed string, no interpolation, which is what makes disable's "exactly once,
// exactly as written" rule a string comparison rather than a parser. `abctl` is the
// bare word rather than this binary's absolute path, deliberately: an absolute path
// would make the block vary per machine, and every verb would then have to resolve
// and compare paths — which is most of what made an earlier attempt at this command
// unreviewable.
//
// Inside the function:
//   - `whence -p` (zsh) and `type -P` (bash) both skip functions and aliases and
//     return only the PATH binary. Each is silenced so the other shell's failure to
//     recognise the flag is invisible.
//   - The emptiness guard turns "bob is not installed" into a plain 127 instead of
//     `abctl exec -- "" ...`, which would be an obscure failure from abctl instead.
//   - "$@" forwards the user's arguments. An alias got this for free; a function has
//     to say so, and omitting it would silently drop every argument.
const bobShellBlock = bobShellMarkerStart + `
bob() {
  p=$(whence -p bob 2>/dev/null || type -P bob 2>/dev/null)
  if [ -z "$p" ]; then
    echo "bob: not found in PATH" >&2
    return 127
  fi
  abctl exec -- "$p" "$@"
}
export ` + bobShellMarkerEnv + `=1
` + bobShellMarkerEnd + "\n"

var bobShellUsage = `abctl configure bobshell — route Bob Shell through Cortex from any new shell

Usage:
  abctl configure bobshell enable  [--yes] [--rc PATH]
  abctl configure bobshell disable [--yes] [--rc PATH]
  abctl configure bobshell status

enable appends this block to your shell startup file:

` + bobShellIndentedBlock() + `
Afterwards, plain "bob" in a NEW interactive shell goes through Cortex. Nothing else
in the file changes, and the first run copies the original to <file>.bak and never
overwrites that copy.

Bob Shell is a command, so unlike Claude Code it has no settings file to write —
which is why this configures the shell instead. The trade-off is scope: a function
reaches interactive shells, not scripts or other programs that run bob directly. For
those, "abctl exec -- bob" is still the answer and needs no configuration.

A function rather than an alias, for two reasons. An alias is invisible to any child
process, so status could never observe one. And inside a function "\bob" does not
reach the real binary — the backslash suppresses alias expansion only, so it finds
the function again and recurses; resolving bob to an absolute path first cannot.

status reads the ` + bobShellMarkerEnv + ` variable from its own environment, not the
startup file. So it answers "is bob routed through Cortex in THIS shell", which is
the question worth asking — and it reports "not enabled" in the shell you ran enable
in, until you source the file or open a new terminal.

Which file: from $SHELL. zsh gets ~/.zshrc; bash gets whichever of ~/.bash_profile,
~/.bashrc or ~/.profile already exists, since which one bash reads depends on the
platform and on whether the shell is a login shell. Any other shell is refused
rather than guessed at — pass --rc to name the file yourself.

A startup file reached through more than one symlink is not edited: the block is
printed for you to add yourself. One link, the usual dotfiles-repo case, is followed
and the file it points at is the one written.

Cortex need not be running for any of this: the function resolves the proxy address
when you run bob, not now.

Exit status: 0 applied or already correct, 2 a usage error, 3 declined (or no
terminal to ask on), 1 something went wrong.

Flags:
  --yes           do not prompt for confirmation (enable and disable; status
                  changes nothing and never prompts)
  --rc PATH       shell startup file to edit (default: from $SHELL)
`

// bobShellIndentedBlock renders the block indented, for quoting inside the help
// text. Matches how claudeCodeEnable2 presents the keys it is about to add.
func bobShellIndentedBlock() string {
	lines := strings.Split(strings.TrimSuffix(bobShellBlock, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n") + "\n"
}

// runBobShell dispatches on the action. Returns the process exit code.
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
	// --yes and --rc only where there is something to skip or to edit. status reads
	// one environment variable: it touches no file and never asks, so registering
	// either there would accept a flag that does nothing.
	yes := new(bool)
	rcPath := new(string)
	if action != "status" {
		yes = fs.Bool("yes", false, "do not prompt for confirmation")
		rcPath = fs.String("rc", "", "shell startup file to edit")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	switch action {
	case "status":
		return bobShellStatus(stdout)
	case "enable", "disable":
		// Resolved here rather than in each verb so both agree on which file they are
		// talking about, and so the error text is written once.
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
		// A --rc that cannot be an rc file is a usage error, so it exits 2 like every
		// other one rather than surfacing a raw errno at 1 and reading like an
		// internal failure.
		if fi, serr := os.Stat(*rcPath); serr == nil && fi.IsDir() {
			fmt.Fprintf(stderr, "abctl: --rc %s is a directory; give the path of a shell startup file\n", *rcPath)
			return 2
		}
		if action == "enable" {
			return bobShellEnable(*rcPath, *yes, stdout, stderr)
		}
		return bobShellDisable(*rcPath, *yes, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "abctl: unknown bobshell action %q (enable, disable, status)\n", action)
		fmt.Fprint(stderr, bobShellUsage)
		return 2
	}
}

// bobShellRCPath picks the startup file to edit, given $SHELL, the home directory and
// the platform. Ported from install.sh's offer_path_setup, which faces the same
// question for its PATH line; the reasoning there applies unchanged.
//
// Takes its inputs as parameters rather than reading the environment so the table of
// cases is testable without mutating process state.
func bobShellRCPath(shell, home, goos string) (string, error) {
	switch filepath.Base(shell) {
	case "zsh":
		// Unconditional: interactive zsh always reads .zshrc, creating it if need be.
		// (.zshenv and .zprofile are the wrong files — the first is read by
		// non-interactive shells too, the second only by login shells.)
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
	// and editing the wrong one leaves the user with a function that never appears
	// and a modified file they did not expect.
	if shell == "" {
		return "", fmt.Errorf("$SHELL is not set, so there is no way to tell which startup file your shell reads; pass --rc PATH to name it")
	}
	return "", fmt.Errorf("unsupported shell %q: only zsh and bash startup files are known; pass --rc PATH to name yours", shell)
}

// rcTarget reports the file to write and how many symlinks were followed to find it.
//
// Zero hops is a plain file. One hop is the common dotfiles-repo case, and following
// it is the correct thing: writing the link itself would REPLACE it with a regular
// file, so the repo's tracked copy would never receive the block and every later
// dotfile edit would silently stop reaching the shell.
//
// Two or more hops is stow/chezmoi territory, where guessing which file in the chain
// the user means is how a working setup gets silently broken. Callers refuse to edit
// and print the block instead. The loop therefore stops at two — it only has to tell
// "one" from "more than one", not measure the depth.
func rcTarget(path string) (target string, hops int) {
	for hops = 0; hops < 2; hops++ {
		fi, err := os.Lstat(path)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			return path, hops
		}
		dest, err := os.Readlink(path)
		if err != nil {
			return path, hops
		}
		if !filepath.IsAbs(dest) {
			// A relative target resolves against the link's directory, the way the
			// kernel reads it.
			dest = filepath.Join(filepath.Dir(path), dest)
		}
		path = filepath.Clean(dest)
	}
	return path, hops
}

// bobShellAdviseManual prints the block for the user to add by hand, for the cases
// this command declines to edit. Changes nothing, so it is never an error.
func bobShellAdviseManual(why, path string, stdout io.Writer) int {
	fmt.Fprintf(stdout, "Not editing %s: %s\n", path, why)
	fmt.Fprintln(stdout, "Add these lines to your shell startup file yourself:")
	fmt.Fprint(stdout, "\n"+bobShellIndentedBlock())
	return 0
}

func bobShellEnable(rcPath string, yes bool, stdout, stderr io.Writer) int {
	target, hops := rcTarget(rcPath)
	if hops >= 2 {
		return bobShellAdviseManual("it is reached through more than one symlink, so which file to write is ambiguous", rcPath, stdout)
	}

	content, err := readFileAllowMissing(target)
	if err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}
	// Idempotent, and by the same exact-match rule disable uses: if our block is
	// already there verbatim, there is nothing to do and that is a success.
	if strings.Contains(content, bobShellBlock) {
		fmt.Fprintf(stdout, "Already enabled in %s\n", target)
		fmt.Fprintf(stdout, "Open a new terminal, or run `source %s`, if `bob` is not routed yet.\n", target)
		return 0
	}
	// A `bob` defined by something other than us. Reported rather than worked around:
	// appending after it would leave two definitions with the later one winning, and
	// which that is depends on where in the file we landed. Naming it lets the user
	// decide, and changes nothing meanwhile.
	if strings.Contains(content, bobShellMarkerStart) {
		return bobShellAdviseManual("it already holds a cortex bobshell block that does not match this version; remove it first", target, stdout)
	}

	if target != rcPath {
		fmt.Fprintf(stdout, "%s is a symlink to %s, which is the file that will be written.\n", rcPath, target)
	}
	fmt.Fprintf(stdout, "Add to %s:\n\n%s\n", target, bobShellIndentedBlock())
	if !yes && !confirm(stdout) {
		return exitDeclined
	}

	// A file that did not end in a newline gets one, so our marker starts its own
	// line. Without it the block fuses onto the user's last line, which both breaks
	// that line and makes the block unmatchable by disable.
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if err := writeRCFile(target, content+bobShellBlock); err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Enabled in %s\n", target)
	fmt.Fprintf(stdout, "Run `source %s`, or open a new terminal, for `bob` to route through Cortex.\n", target)
	return 0
}

func bobShellDisable(rcPath string, yes bool, stdout, stderr io.Writer) int {
	target, hops := rcTarget(rcPath)
	if hops >= 2 {
		return bobShellAdviseManual("it is reached through more than one symlink, so which file to edit is ambiguous", rcPath, stdout)
	}

	content, err := readFileAllowMissing(target)
	if err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}
	// Exactly once, exactly as written — the rule the whole design turns on. Anything
	// else is reported and left alone: this command removes text it is certain it
	// wrote, and nothing else.
	switch n := strings.Count(content, bobShellBlock); {
	case n == 0:
		fmt.Fprintf(stdout, "Not enabled in %s\n", target)
		// A marker with no matching block means a hand-edit, which is the one case
		// where "not enabled" alone would mislead: there is something of ours in
		// there, and we are deliberately not touching it.
		if strings.Contains(content, bobShellMarkerStart) {
			fmt.Fprintln(stdout, "  A cortex bobshell block is present but does not match this version, so it")
			fmt.Fprintln(stdout, "  was left alone. Remove it by hand if you no longer want it.")
		}
		return 0
	case n > 1:
		fmt.Fprintf(stdout, "Not changing %s: it holds %d copies of the cortex bobshell block.\n", target, n)
		fmt.Fprintln(stdout, "Remove the extras by hand, leaving one, and run this again.")
		return 0
	}

	if target != rcPath {
		fmt.Fprintf(stdout, "%s is a symlink to %s, which is the file that will be edited.\n", rcPath, target)
	}
	fmt.Fprintf(stdout, "Remove from %s:\n\n%s\n", target, bobShellIndentedBlock())
	if !yes && !confirm(stdout) {
		return exitDeclined
	}

	if err := writeRCFile(target, strings.Replace(content, bobShellBlock, "", 1)); err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Disabled in %s\n", target)
	// Removing the definition cannot reach a shell that has already read it.
	fmt.Fprintln(stdout, "Run `unset -f bob` in this shell, or open a new terminal, to stop routing `bob`.")
	return 0
}

// bobShellStatus reports whether THIS process's environment carries the marker.
//
// Deliberately blind to the rc file: see bobShellMarkerEnv. The consequence worth
// stating in the output is that enable's effect is not visible until a shell has
// read the file, so "not enabled" right after a successful enable is expected.
func bobShellStatus(stdout io.Writer) int {
	if os.Getenv(bobShellMarkerEnv) != "" {
		fmt.Fprintln(stdout, "enabled")
		fmt.Fprintf(stdout, "  %s is set in this shell, so `bob` routes through Cortex here.\n", bobShellMarkerEnv)
		return 0
	}
	fmt.Fprintln(stdout, "not enabled")
	fmt.Fprintf(stdout, "  %s is not set in this shell.\n", bobShellMarkerEnv)
	fmt.Fprintln(stdout, "  Run `abctl configure bobshell enable`, then open a new terminal.")
	fmt.Fprintln(stdout, "  A shell that read the startup file before enable ran will not have it.")
	return 0
}

// readFileAllowMissing reads path, treating a missing file as empty.
//
// A startup file that does not exist yet is the ordinary case for a fresh account,
// and enable creates it. Only a file that exists and cannot be read is an error.
func readFileAllowMissing(path string) (string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(b), nil
}

// writeRCFile backs the file up once, then replaces it atomically.
//
// The same discipline as writeSettings, and the backup-once rule matters more here:
// an rc file is accreted by hand over years, and overwriting the backup on a second
// run would replace the only pristine copy with one this command had already edited.
// The existing mode is preserved — an rc file is commonly 0644, and silently
// tightening it to 0600 is a change the user did not ask for.
//
// path must already be resolved by the caller via rcTarget.
func writeRCFile(path, content string) error {
	// 0644 for a file we create, not 0600: every shell's own rc file is
	// world-readable, and an existing file's mode wins over this default anyway.
	mode := os.FileMode(0o644)
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
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		// Remove it: after following a symlink this sits inside the user's dotfiles
		// repo, where an untracked .tmp is something they may well commit by accident.
		os.Remove(tmp)
		return err
	}
	return nil
}
