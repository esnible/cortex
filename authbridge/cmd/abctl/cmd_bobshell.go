package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	// --yes only where there is a prompt to skip. status writes nothing and never
	// asks, so registering it there would accept a flag that does nothing — and the
	// help text already scopes it to enable and disable, which this makes true rather
	// than aspirational. An unknown flag is flag's own usage error, so it exits 2.
	yes := new(bool)
	if action != "status" {
		yes = fs.Bool("yes", false, "do not prompt for confirmation")
	}
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

	// A --rc that cannot be an rc file is a usage error, so it exits 2 like every
	// other one — the code --help documents. It used to surface the raw errno at exit
	// 1 ("read /x/adir: is a directory"), which both contradicted the documented code
	// and read like an internal failure rather than a correctable argument.
	if fi, serr := os.Stat(*rcPath); serr == nil && fi.IsDir() {
		fmt.Fprintf(stderr, "abctl: --rc %s is a directory; give the path of a shell startup file\n", *rcPath)
		return 2
	}

	// Resolve this binary's own path before dispatching, for ALL THREE verbs.
	//
	// Only enable used to need it, and that asymmetry was the bug: disable and status
	// recognised an alias by the binary's NAME containing "abctl", so a binary called
	// anything else (a rename, a symlink, `/tmp/ab8`) did not recognise the block enable
	// had just written — a second enable duplicated it and disable left it live. The
	// running path is what makes the three verbs agree, so all three get it.
	//
	// A failure here is fatal only for enable, which must write the path into the file.
	// disable and status still work without it, falling back to the name test, so they
	// carry on rather than refusing to run over a file they can very likely still read.
	self, selfErr := abctlPath()
	bobShellSelfPath = self

	switch action {
	case "enable":
		if selfErr != nil {
			fmt.Fprintf(stderr, "abctl: %v\n", selfErr)
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

// unquoteShellWord returns the literal value of a shell word, honouring quotes
// wherever they sit in it, and whether it parsed cleanly.
//
// This exists because the question "is this line a live alias to our proxy" is a
// question about what the SHELL sees, and text matching answers a different one.
// Round 7 found three inputs where the two diverge, all of them live aliases the
// tool reported as absent: a trailing comment (`alias bob='...' # note`), split
// quoting (`alias bob=/b/abctl' exec -- \bob'`), and the double-quoted form. To
// bash all three define the identical alias; the predicate they defeated compared
// text with strings.Trim, which only strips quotes at the ends of the string.
//
// Unquoting first collapses every spelling of one alias onto one value, so the
// predicate compares meanings instead of spellings and the spellings stop mattering.
// Parsing rather than executing is the point: sourcing a user's rc to ask the shell
// directly would run arbitrary code, which a status command must never do.
func unquoteShellWord(s string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); {
		switch c := s[i]; c {
		case '\'':
			// Single quotes: everything literal until the next one.
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return "", false
			}
			b.WriteString(s[i+1 : i+1+j])
			i += j + 2
		case '"':
			i++
			for i < len(s) && s[i] != '"' {
				// In double quotes a backslash escapes only these four; before
				// anything else it is itself literal — which is what keeps the
				// \bob in `alias bob="... -- \bob"` intact.
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`", s[i+1]) >= 0 {
					b.WriteByte(s[i+1])
					i += 2
					continue
				}
				b.WriteByte(s[i])
				i++
			}
			if i >= len(s) {
				return "", false
			}
			i++
		case ' ', '\t':
			// Unquoted whitespace ends the word, which is what makes a trailing comment
			// stop hiding the alias in front of it: in `alias bob='...' # note` the word
			// is over at the space, and the comment never enters the value.
			//
			// A `#` is deliberately NOT a terminator here. It looked like one — a
			// comment starts a comment — but both bash and zsh were asked directly, and
			// `alias bob=/bin/echo#c` defines the value `/bin/echo#c`: a `#` only opens a
			// comment where a word could START, not mid-word. Treating it as a
			// terminator truncated a value the shell keeps whole. Unobservable for this
			// predicate either way, since a value containing `#` is not our alias in
			// either reading — but this function's only job is to model what the shell
			// does, so a clause that models something else does not belong in it.
			return b.String(), true
		case '\\':
			if i+1 >= len(s) {
				return "", false
			}
			b.WriteByte(s[i+1])
			i += 2
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), true
}

// aliasRoutesThroughAbctl reports whether line defines `bob` as an alias that runs
// some abctl's `exec -- bob` — this feature's mechanism, however it is spelled.
//
// It replaces the two predicates rounds 5 and 6 left behind (an exact-reconstruction
// one for file scope, a looser one for inside a fence). Keeping two was what let a
// live alias be invisible in one scope and claimed in the other, which is the same
// defect the reviewer reported from both directions in rounds 6 and 7. One question
// deserves one answer, and unquoting makes a single answer possible: every spelling
// of the same alias now reduces to the same value before anything is compared.
//
// It stays narrow in the way round 5 established. The discriminator is what the
// alias INVOKES, not how it is written: `alias bob='/usr/local/bin/bob --fast'` is
// the user's own alias to the real binary and is never ours, no matter where in the
// file it sits, while `/b/abctl exec -- \bob` is this mechanism and nothing else.
// bobShellSelfPath is the absolute path of the running binary, as the alias would
// spell it. Set once by runBobShell; empty in a context that never resolved it, which
// only costs the self-path arm of aliasRoutesThroughAbctl.
var bobShellSelfPath string

func aliasRoutesThroughAbctl(line string) bool {
	for _, off := range scanLineStarts(line) {
		if commandIsOurAlias(line[off:]) {
			return true
		}
	}
	return false
}

// scanLineStarts is scanLine's offsets for a single standalone line, for the callers
// that have a line and no file context. Callers walking a whole file use
// commandStarts, which carries quote and heredoc state across lines.
func scanLineStarts(line string) []int {
	starts, _ := scanLine(line, 0)
	return starts
}

// commandIsOurAlias reports whether the command beginning at the start of s is an
// `alias` builtin that defines `bob` as some abctl's `exec -- bob` — this feature's
// mechanism, however it is spelled.
//
// Round 8's MF3: ownership used to pivot on `strings.HasPrefix(t, "alias bob=")`,
// which is text matching in the one place round 7 claimed to have removed it. Five
// spellings each defeated it while being live per bash, and each ended with TWO live
// aliases after enable: a tab between the words, `\alias bob=` (the backslash only
// suppresses alias expansion, the builtin still runs), `x=1; alias bob=…` after a
// separator, `$'…'` ANSI-C quoting, and the `alias \` + newline + `bob=…` continuation.
//
// The fix is to tokenise the command rather than to enumerate its spellings. Each
// word is unquoted first, so `\alias`, `alias`, and `'alias'` are one word; the
// operand is split on the first `=` after unquoting, so a tab or any other whitespace
// between words is just a word boundary; and the caller supplies the offset, so a
// command after `;` is reached at all.
func commandIsOurAlias(s string) bool {
	words, ok := splitShellWords(s)
	if !ok || len(words) < 2 || words[0] != "alias" {
		return false
	}
	// `alias -- bob=…` is accepted by both shells and defines exactly the same alias,
	// so the separator is skipped rather than matched as part of a prefix. Other flags
	// (`alias -p`) print rather than define, and fall out below for lack of an operand
	// that names bob.
	rest := words[1:]
	for len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	// One `alias` command can define several aliases; only ours has to match.
	for _, w := range rest {
		name, value, found := strings.Cut(w, "=")
		if !found || name != "bob" {
			continue
		}
		if aliasValueRoutesThroughAbctl(value) {
			return true
		}
	}
	return false
}

// aliasValueRoutesThroughAbctl reports whether an alias VALUE, already unquoted, runs
// some abctl's `exec -- bob`.
//
// It stays narrow in the way round 5 established. The discriminator is what the alias
// INVOKES, not how it is written: `alias bob='/usr/local/bin/bob --fast'` is the
// user's own alias to the real binary and is never ours, no matter where in the file
// it sits, while `/b/abctl exec -- \bob` is this mechanism and nothing else.
func aliasValueRoutesThroughAbctl(value string) bool {
	f := strings.Fields(value)
	// <abctl> exec [flags...] -- bob
	//
	// The arity floor is a bounds guard for f[1] and f[len(f)-2], and nothing more:
	// mutating 4 down to 2 changes no answer, because `exec` followed by the
	// `-- bob` tail already implies four fields. What it does prevent is a panic on
	// `alias bob=/b/abctl` — one field, no subcommand — which is why the test table
	// carries a one-field row. Lowering it to 1 panics; the constant itself is
	// unobservable above 2, so no test pins 4 specifically and none should pretend to.
	if len(f) < 4 || f[1] != "exec" {
		return false
	}
	last := f[len(f)-1]
	if f[len(f)-2] != "--" || (last != "bob" && last != `\bob`) {
		return false
	}
	// The first field must be an abctl: either one by name, or the exact path this
	// process is running as.
	//
	// Name alone was wrong, and it took a real round trip to see it. `Contains(Base,
	// "abctl")` accepts `abctl-dev` but not `/tmp/ab8`, so a binary whose filename
	// does not happen to contain "abctl" did not recognise the alias IT HAD JUST
	// WRITTEN: a second enable appended a duplicate, and disable then left a live
	// alias behind. A user who renames the binary, symlinks it as `~/bin/cortex`, or
	// runs a versioned download hits exactly that. Identical inputs differing only in
	// the binary's filename must not differ in behaviour.
	//
	// Matching the running binary's own path fixes the symmetry without loosening the
	// check: whatever this binary is called, it always recognises its own work. The
	// name test stays for blocks written by a DIFFERENT abctl than the one running now
	// — a moved or reinstalled binary, which status reports and enable rewrites.
	//
	// bobShellSelfPath is a package-level value rather than a parameter threaded
	// through ownedLines, bobShellAliasesIn and the two rewriters. It is genuinely
	// one value per process, and passing it down six call sites would add exactly the
	// plumbing this file is already too long for. Tests set it directly.
	if strings.Contains(filepath.Base(f[0]), "abctl") {
		return true
	}
	return bobShellSelfPath != "" && f[0] == bobShellSelfPath
}

// splitShellWords splits a command into unquoted words, stopping at the first
// operator that ends it (`;`, `&`, `|`, a redirection, or a comment). ok is false for
// input the shell itself would not accept as a complete word — an unterminated quote
// or a trailing backslash — which is the same "do not guess" rule unquoteShellWord
// established: a line we cannot read is a line we do not claim.
//
// `$'…'` (ANSI-C quoting) is handled here rather than in unquoteShellWord because it
// is a word FORM, not a quote that can appear mid-word the way `'` and `"` can. Only
// the escapes that can plausibly appear in a path or in our own alias body are
// decoded; anything else keeps its literal characters, which is conservative in the
// safe direction — an undecoded escape makes a value NOT match ours.
func splitShellWords(s string) (words []string, ok bool) {
	i := 0
	for {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i >= len(s) {
			return words, true
		}
		switch s[i] {
		case ';', '&', '|', '\n', '#', '<', '>', ')', '}':
			return words, true
		}
		w, next, wok := shellWordAt(s, i)
		if !wok {
			return words, false
		}
		words = append(words, w)
		i = next
	}
}

// shellWordAt reads one word starting at i, returning its literal value and the
// offset just past it.
func shellWordAt(s string, i int) (word string, next int, ok bool) {
	var b strings.Builder
	for i < len(s) {
		switch c := s[i]; c {
		case ' ', '\t', ';', '&', '|', '\n', '<', '>':
			return b.String(), i, true
		case '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return "", i, false
			}
			b.WriteString(s[i+1 : i+1+j])
			i += j + 2
		case '"':
			i++
			for i < len(s) && s[i] != '"' {
				// In double quotes a backslash escapes only these four; before anything
				// else it is itself literal — which is what keeps the \bob in
				// `alias bob="… -- \bob"` intact.
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`", s[i+1]) >= 0 {
					b.WriteByte(s[i+1])
					i += 2
					continue
				}
				b.WriteByte(s[i])
				i++
			}
			if i >= len(s) {
				return "", i, false
			}
			i++
		case '$':
			if i+1 < len(s) && s[i+1] == '\'' {
				v, n, vok := ansiCQuoted(s, i+2)
				if !vok {
					return "", i, false
				}
				b.WriteString(v)
				i = n
				continue
			}
			b.WriteByte(c)
			i++
		case '\\':
			if i+1 >= len(s) {
				return "", i, false
			}
			b.WriteByte(s[i+1])
			i += 2
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), i, true
}

// ansiCQuoted reads a `$'…'` body starting at i (just past the quote) and returns its
// decoded value and the offset past the closing quote.
func ansiCQuoted(s string, i int) (string, int, bool) {
	var b strings.Builder
	for i < len(s) {
		switch c := s[i]; c {
		case '\'':
			return b.String(), i + 1, true
		case '\\':
			if i+1 >= len(s) {
				return "", i, false
			}
			switch e := s[i+1]; e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '\\', '\'', '"':
				b.WriteByte(e)
			default:
				// Not a decoded escape: keep both bytes, as an undecoded escape can only
				// make the value fail to match ours.
				b.WriteByte('\\')
				b.WriteByte(e)
			}
			i += 2
		default:
			b.WriteByte(c)
			i++
		}
	}
	return "", i, false
}

// scanLine walks one line of shell text, tracking what is quoted, and reports the
// offsets at which a fresh command could begin plus the quote state left open at the
// end of the line.
//
// Three of round 8's five findings were the same mistake in three places: deciding
// what a line means by looking at its text from the left edge. `heredocBody` modelled
// exactly one construct that holds data (the heredoc) and called every other line a
// command, so a quoted multi-line string was edited surgically (MF1) and a quoted
// `<<EOF` was read as an opener that suppressed the rest of the file (MF2). Ownership
// separately pivoted on `strings.HasPrefix(t, "alias bob=")`, so `alias<TAB>bob=`,
// `x=1; alias bob=`, and a backslash-escaped `\alias` were all invisible while being
// live (MF3).
//
// One scanner answers all three, because all three want the same two facts: which
// bytes of this line are quoted, and where a command starts. A command starts at the
// beginning of a line and after an unquoted `;`, `&`, `|`, `(`, or a `then`/`do`-style
// keyword — the last of which this does NOT model, and does not need to: a command
// found anywhere on the line is enough for ownership, and the keyword case only ever
// adds candidate offsets we already cover by scanning the whole line.
//
// quote is the state carried INTO the line: 0 for none, '\” or '"' inside an
// unterminated string of that kind. A continued line has no command starts at all,
// which is precisely what MF1 needed — the alias inside `MSG="…\nalias bob=…\n…"` is
// data, exactly as heredoc body is.
func scanLine(line string, quote byte) (starts []int, endQuote byte) {
	q := quote
	atStart := q == 0 // a command can begin here
	// atWord is the weaker condition: a new WORD can begin here. It differs from
	// atStart after the first word of a command (`echo hi` — `hi` begins a word but not
	// a command), and that difference is exactly what `#` needs. Keying the comment on
	// atStart claimed the contents of a TRAILING comment as a command:
	// `echo hi # note; alias bob=x` reported a start at the `alias`, because the `;`
	// inside the comment set atStart back to true. bash defines no alias there, so that
	// is over-detection — and by MF2's own lesson over-detection is not the harmless
	// direction: disable would have deleted a line the user wrote as a comment.
	atWord := atStart
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case q == '\'':
			if c == '\'' {
				q = 0
			}
			continue
		case q == '"':
			// Inside double quotes a backslash escapes the next byte, so `\"` does not
			// close the string. Everything else stays data.
			if c == '\\' && i+1 < len(line) {
				i++
				continue
			}
			if c == '"' {
				q = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			// A quote can BEGIN a command word, not just appear inside one: bash and zsh
			// both run `'alias' bob=x` and `"alias" bob=x`, defining the alias. This case
			// used to set atStart=false without recording anything, so quoting the keyword
			// hid the command — a sixth spelling of round 8's MF3, past the five the
			// review enumerated, and found by testing beyond them rather than by reading.
			// shellWordAt already unquotes the word, so recording the start is the whole fix.
			if atStart {
				starts = append(starts, i)
				atStart = false
			}
			atWord = false
			q = c
		case '\\':
			// An unquoted backslash escapes the next byte and does not end the word. It
			// also does not stop a command from starting: bash runs `\alias bob=x`, the
			// backslash only suppressing alias expansion of the word itself. So a start
			// already recorded at this offset stands, and the escaped byte is consumed.
			if atStart {
				starts = append(starts, i)
				atStart = false
			}
			atWord = false
			i++
		case ' ', '\t':
			// Whitespace before the first word of a command does not consume the start,
			// and it does begin a new word.
			atWord = true
		case ';', '&', '|', '(', ')', '{', '}', '\n':
			atStart = true
			atWord = true
		case '#':
			if atWord {
				// A `#` that BEGINS a word comments out the rest of the line — including
				// any separator in it, which is why this must return rather than merely
				// clear atStart. Mid-word `#` is an ordinary byte in both shells
				// (`x=a#b`), so only a word-initial one matters.
				return starts, q
			}
			atStart = false
		default:
			if atStart {
				starts = append(starts, i)
				atStart = false
			}
			atWord = false
		}
	}
	return starts, q
}

// heredocDelim returns the delimiter word opened by the last heredoc operator on
// line, and whether it was the tab-stripping `<<-` form. "" means the line opens no
// heredoc. quote is the quote state carried into the line; an operator inside quotes
// is text, not an operator.
//
// RE2 has no lookahead, so the scan is manual: that is how `<<<` (a herestring,
// which has no body) is told apart from `<<`. Our own end marker contains `<<<`,
// and an earlier regex read it as opening a heredoc named `cortex` — which marked
// the rest of a perfectly ordinary rc file as heredoc body and would have made
// disable a no-op on real blocks. Only executing it showed that; reasoning about
// the pattern did not.
//
// Quote-awareness is round 8's MF2. `echo '<<EOF'` was read as an opener, so every
// following line was classified as body forever: a live alias below it was invisible
// to status, disable reported nothing to do, and enable then wrote a SECOND live
// alias which status still could not see. That refuted this function's own former
// claim that over-detection is "the recoverable error" — a suppressed observation
// does not stay contained, it propagates into the next write. Neither direction is
// recoverable, which is why the fix is to be correct about quoting rather than to
// pick a safer bias.
func heredocDelim(line string, quote byte) (delim string, dashed bool) {
	q := quote
	for i := 0; i+1 < len(line); i++ {
		c := line[i]
		switch {
		case q == '\'':
			if c == '\'' {
				q = 0
			}
			continue
		case q == '"':
			if c == '\\' && i+1 < len(line) {
				i++
				continue
			}
			if c == '"' {
				q = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			q = c
			continue
		}
		if c == '\\' {
			i++
			continue
		}
		if c != '<' || line[i+1] != '<' {
			continue
		}
		if i+2 < len(line) && line[i+2] == '<' {
			i += 2 // herestring, not a heredoc
			continue
		}
		rest := line[i+2:]
		m := heredocDelimRe.FindStringSubmatch(rest)
		if m == nil {
			continue
		}
		for _, g := range m[1:] {
			if g != "" {
				delim = g
				break
			}
		}
		dashed = strings.HasPrefix(rest, "-")
	}
	return delim, dashed
}

var heredocDelimRe = regexp.MustCompile(`^-?[ \t]*(?:'([^']*)'|"([^"]*)"|\\?([A-Za-z_][A-Za-z0-9_]*))`)

// commandStarts reports, per line, the offsets at which a command begins — the only
// places this file will look for an alias of ours. A line with no starts holds no
// command: it is heredoc body, or the continuation of a quoted string, or a comment.
//
// Round 7 called the equivalent function heredocBody and had it model one construct.
// The rename is the finding: what the callers actually need is "is there a command
// here, and where", and both data-carrying constructs — heredoc bodies and multi-line
// quoted strings — answer it the same way, with no starts at all. Tracking them in one
// loop is the fix round 8 asked for, and it is also less code than two mechanisms.
//
// What this is still not: a shell parser. It does not model `case` patterns, arithmetic
// contexts, or `$( )` nesting. It models quoting and heredocs, which are what put
// non-command text in an rc file, and it errs toward finding a command start (so a
// construct it does not know cannot hide a live alias from status).
func commandStarts(lines []string) []lineCommands {
	out := make([]lineCommands, len(lines))
	var quote byte
	delim, dashed := "", false
	// A line ending in an unescaped backslash continues into the next: the two are ONE
	// command, and `alias \` + newline + `bob=…` is a live alias whose `alias` keyword
	// and `bob=` operand sit on different physical lines — round 8's MF3, fifth
	// spelling, live per bash and invisible to a per-line scan.
	//
	// The joined text is scanned, and the result is attributed to the line where the
	// command BEGAN. That line is the one a rewrite can meaningfully act on, and it is
	// the one the user reads as "the alias line".
	carry, carryAt := "", -1
	for i, l := range lines {
		if delim != "" {
			t := l
			if dashed {
				t = strings.TrimLeft(t, "\t")
			}
			if strings.TrimRight(t, " \t") == delim {
				delim, dashed = "", false // the terminator is a boundary, not body
			}
			continue // heredoc body and its terminator hold no command of ours
		}
		text, at := l, i
		if carry != "" {
			text, at = carry+l, carryAt
		}
		if endsWithContinuation(l) {
			carry, carryAt = strings.TrimSuffix(text, "\\"), at
			continue
		}
		carry, carryAt = "", -1
		starts, end := scanLine(text, quote)
		out[at].through = i // the last physical line of this command
		// Appending rather than assigning: a continued command's starts land on the
		// line it began at, which may already carry starts of its own when an earlier
		// command on that same line ended with `;`.
		out[at].text = text
		out[at].starts = append(out[at].starts, starts...)
		// A heredoc opened by a line that is itself a continuation of a quoted string
		// is not an opener; heredocDelim gets the same incoming state for that reason.
		// No `end == 0` guard here: heredocDelim tracks quoting itself and is handed the
		// carried state, so on a line that ends mid-quote it already sees any `<<` as
		// quoted and reports nothing. A guard was written here first and mutation testing
		// showed it unreachable — no file shape distinguishes its presence — so it is
		// gone rather than pinned by a test asserting nothing.
		delim, dashed = heredocDelim(text, quote)
		quote = end
		// A line ending inside an unterminated quote continues into the next, so the
		// next line's text is data. This is MF1: `MSG="<START>\nalias bob=…\n<END>"`
		// had its middle line surgically deleted — bash defined nothing, status claimed
		// enabled, disable reported success and took the file 122→88 bytes. The same
		// shape with single quotes, with `python3 -c "…"` (58→24, collapsing a script to
		// two quote characters), and with a DOC='…' holding the user's secrets.
	}
	return out
}

// lineCommands is what commandStarts knows about one physical line: the command text
// attributed to it (its own, or the whole backslash-joined command it begins) and the
// offsets within that text at which a command starts. No starts means no command —
// heredoc body, a quoted-string continuation, a comment, or a bare continuation line.
type lineCommands struct {
	text   string
	starts []int
	// through is the last physical line this command occupies. It equals the line's own
	// index except for a backslash-continued command, whose ownership must cover every
	// line it spans: marking only the line that began it left the continuation — which
	// is where the `bob=` text actually sits — in the file after disable, so the alias
	// stayed live. Found by executing MF3's fifth spelling rather than by reading.
	through int
}

// holdsOurAlias reports whether any command attributed to this line is our alias.
func (lc lineCommands) holdsOurAlias() bool {
	for _, off := range lc.starts {
		if commandIsOurAlias(lc.text[off:]) {
			return true
		}
	}
	return false
}

// endsWithContinuation reports whether the line ends in a backslash that escapes the
// newline, joining it to the next. An even run of trailing backslashes escapes itself
// and does not continue.
func endsWithContinuation(l string) bool {
	n := 0
	for n < len(l) && l[len(l)-1-n] == '\\' {
		n++
	}
	return n%2 == 1
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

// ownedLines returns the indices of the lines this command may rewrite or delete.
//
// It returns a SET, not a span. A span was tried and failed twice: a file whose
// owned lines are non-contiguous has a hull covering lines that are not ours, and
// keying a write off the hull deletes the user's content between our lines.
// What we own is a SET, not a span: a hand-edit can leave our lines
// non-contiguous, and every writer iterates the set for that reason.
//
// A line is ours when both of these hold:
//
//   - It is a COMMAND, not heredoc body. A heredoc hands its lines to another
//     program; they are data. Round 7 found disable reaching into a Python string
//     literal to excise an alias line, reporting success over a file where bash
//     confirmed nothing was ever defined, and collapsing a `cat <<'EOF'` to its
//     opener plus terminator. Pairing could not prevent it, because the alias test
//     applied at file scope and the heredoc was inside the fence's reach either way.
//
//   - It is either one of our markers, or an alias that routes bob through an abctl
//     (aliasRoutesThroughAbctl), anywhere in the file, fenced or not.
//
// Note what is NOT here: position. Being inside a fence does not make a line ours,
// and being outside one does not make it safe. Both halves have been learned the
// hard way. Round 5: a user's own `alias bob='/usr/local/bin/bob --fast'` pasted
// between the markers was silently replaced, because the fence was read as a licence
// to claim its contents — it is a bookkeeping device written into someone else's
// file, not that. Round 7: the double-quoted form of OUR alias was invisible once
// its markers were hand-deleted, so status said not enabled, disable said nothing to
// do, and enable then appended a second live alias to a file that already had one.
// Provenance is the only test, and it is per line.
//
// That one rule replaced the two-predicate, two-pass, pair-matching arrangement
// rounds 5 and 6 built up. The reason it can is aliasRoutesThroughAbctl: unquoting
// before comparing collapses every spelling of an alias onto one value, so the
// strict-inside / loose-outside split those rounds needed — and the contradiction
// between them the reviewer reported from both directions — stops existing.
//
// The one thing position still buys is the INSERTION POINT: see
// replaceBobShellBlock, which puts the fresh block where the first owned line was.
//
// The invariants the callers need: after enable the file defines exactly one live
// bob alias, and after disable exactly zero.
func ownedLines(lines []string) []int { return ownedLinesFor(lines, false) }

// ownedLinesFor is ownedLines with the marker rule made explicit.
//
// installing is true only for enable, which is about to put a live alias into this
// file. That changes what a marker means here, and the asymmetry is load-bearing
// rather than a convenience:
//
//   - disable and status must not claim a marker in a file with no live alias of
//     ours, because a file that merely DOCUMENTS the block looks exactly like that
//     and deleting from it is unrecoverable (round 6 / MF3).
//   - enable, installing the alias itself, owns the markers regardless: an orphan
//     START left by a hand-edit is part of the block it is replacing, not prose. Left
//     unclaimed it made enable non-idempotent — the first call appended past the
//     orphan and the second absorbed it, so two identical calls produced two
//     different files.
func ownedLinesFor(lines []string, installing bool) []int {
	cmds := commandStarts(lines)
	live := make([]bool, len(lines))
	marker := make([]bool, len(lines))
	for i, l := range lines {
		t := strings.TrimSpace(l)
		// A marker is recognised whether or not the line holds a command: the markers
		// are comments, so commandStarts reports no command on them. What it does still
		// gate is a marker inside a heredoc or a quoted string, which is data.
		if t == bobShellMarkerStart || t == bobShellMarkerEnd {
			// Unconditionally a marker: t is the TRIMMED line and has to equal a marker
			// constant exactly to get here, and both constants begin with '#', so this line
			// is a comment by construction. An inertness test was written here
			// (`len(starts) == 0 || isCommentLine(l)`) and mutation testing showed it
			// always true — no file shape distinguishes it — so it is gone rather than
			// pinned by a test asserting nothing. A line with a marker plus anything else
			// on it never reaches this branch at all; it falls through to the alias check.
			marker[i] = true
			continue
		}
		if cmds[i].holdsOurAlias() {
			for j := i; j <= cmds[i].through && j < len(lines); j++ {
				live[j] = true
			}
		}
	}
	// A marker belongs to us only when it is part of a RUN of our own lines that
	// contains a live alias — not merely when the file has one somewhere.
	//
	// Round 8's MF5: the gate used to be a file-wide `any`, so a user's pasted --help
	// transcript was claimed as soon as a real alias existed anywhere else in the file.
	// Executed, that is exactly what happened: disable deleted all three pasted lines
	// along with the real alias, and enable de-indented the paste into a canonical
	// block, destroying the note. Round 6 protected the paste by requiring a live alias
	// between a matched PAIR; round 7 dropped the pairing as "unnecessary" and that
	// reintroduced the bug from the other side.
	//
	// Adjacency is what actually separates the two cases, and it is weaker than pairing
	// in the way that matters: a hand-deleted marker leaves its survivor still adjacent
	// to the live alias, so the orphan is still claimed (the round-7 concern), while a
	// paste separated from the real alias by even one unrelated line is not (the round-6
	// concern). Both hold at once.
	owned := make([]bool, len(lines))
	for i := range lines {
		if !live[i] {
			continue
		}
		owned[i] = true
		for j := i - 1; j >= 0 && marker[j]; j-- {
			owned[j] = true
		}
		for j := i + 1; j < len(lines) && marker[j]; j++ {
			owned[j] = true
		}
	}
	if installing {
		// enable is putting a live alias into this file, so a marker from a hand-edited
		// block is part of what it is replacing rather than prose to preserve. Without
		// this, the first enable appended past an orphan START and the second absorbed
		// it: two identical calls, two different files.
		//
		// The adjacency rule above cannot see an orphan that has no live alias next to
		// it, which is precisely the non-idempotent case, so enable claims markers
		// file-wide. It is safe here for the reason the general case is not: enable
		// rewrites the block rather than deleting it, so a claimed prose marker is
		// reformatted, not lost — and a file where enable is being run is one the user
		// has just asked us to manage.
		for i := range lines {
			if marker[i] {
				owned[i] = true
			}
		}
	}

	out := make([]int, 0, 4)
	for i := range lines {
		if owned[i] {
			out = append(out, i)
		}
	}
	return out
}

// isCommentLine reports whether the line's first non-blank character is `#`, making
// the whole line a comment. Used only to recognise our own markers, which are
// comments and therefore carry no command start of their own.
func isCommentLine(l string) bool {
	t := strings.TrimLeft(l, " \t")
	return strings.HasPrefix(t, "#")
}

// bobShellAliasesIn returns every alias line this command owns, in file order.
//
// Plural because a damaged file can hold more than one, and status has to be able to
// say so rather than reporting the first and implying it is the only one. The filter
// is aliasRoutesThroughAbctl, not the `alias bob=` prefix the single-valued version
// used: that prefix would report a user's own alias as though it were ours, the
// round-4 defect in a different function. Iterating ownedLines rather than the raw
// file also means heredoc bodies are skipped here, so status no longer reports an
// alias that exists only as data inside someone's embedded script.
func bobShellAliasesIn(lines []string) []string {
	// The scanner is asked, not re-derived. This function used to re-test each owned
	// line with `aliasRoutesThroughAbctl(strings.TrimSpace(lines[i]))`, which is a
	// second mechanism answering the same question — the two-predicates defect rounds 6
	// and 7 each fixed in one place and left in another. It disagreed with ownedLines on
	// every continued command: enable and disable handled `alias \` + newline + `bob=…`
	// correctly while status called the same file "not enabled".
	cmds := commandStarts(lines)
	owned := map[int]bool{}
	for _, i := range ownedLines(lines) {
		owned[i] = true
	}
	var out []string
	for i := range lines {
		if owned[i] && cmds[i].holdsOurAlias() {
			// The joined text for a continued command, so status shows the whole alias
			// rather than the fragment on its first line.
			out = append(out, strings.TrimSpace(cmds[i].text))
		}
	}
	return out
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
	// The installing view: see ownedLinesFor. enable is putting a live alias here, so
	// a stray marker from a hand-edited block is ours to absorb rather than prose to
	// preserve — which is what makes two identical enables produce identical files.
	owned := ownedLinesFor(lines, true)
	if len(owned) == 0 {
		out := append([]string(nil), lines...)
		return append(out, blockLines...)
	}
	// Every owned line goes, and the fresh block lands where the FIRST one was. A
	// span-based rewrite substituted the block for its hull, which on a
	// non-contiguous file both swallowed the user's lines inside the hull and left
	// our lines outside it — the second of those being a live alias that survived
	// enable, so the file ended up with two.
	drop := make(map[int]bool, len(owned))
	for _, i := range owned {
		drop[i] = true
	}
	// The block inherits the indentation of the line it replaces.
	//
	// Round 8's MF5 reported enable "de-indenting" an indented block, destroying a
	// user's note. Executing it showed the claim needs splitting: an indented
	// `alias bob=…` is LIVE — both bash and zsh define it, indentation does not make it
	// inert — so claiming and rewriting it is what a tool that promises exactly one live
	// alias must do, and leaving it would leave two. (A genuinely inert copy is
	// protected, and that was verified separately: a paste inside a heredoc, a paste
	// commented out, and a markers-only documentation file all survive disable intact
	// while a real alias elsewhere is removed.)
	//
	// What was a real defect is the reformatting. Rewriting an indented block flush-left
	// changed lines the user had aligned, for no reason the user asked for, so the
	// indentation of the first owned line is carried onto the replacement.
	indent := ""
	if first := lines[owned[0]]; true {
		indent = first[:len(first)-len(strings.TrimLeft(first, " \t"))]
	}
	out := make([]string, 0, len(lines)-len(owned)+len(blockLines))
	for i, l := range lines {
		if i == owned[0] {
			for _, b := range blockLines {
				out = append(out, indent+b)
			}
		}
		if !drop[i] {
			out = append(out, l)
		}
	}
	return out
}

// removeBobShellBlock drops the managed block, reporting whether there was one.
//
// Exactly the block, and nothing adjacent to it: enable appends no separator, so
// there is none to reclaim, and a blank line before the block is the user's.
func removeBobShellBlock(lines []string) ([]string, bool) {
	owned := ownedLines(lines)
	if len(owned) == 0 {
		return lines, false
	}
	// Every owned line, not the span between the first and the last: the span holds
	// the user's lines too when a hand-edit split the block, and removing it took
	// them with it. Dropping the set leaves everything that is not ours exactly where
	// it was, which is the round trip this file's header promises, and leaves no alias
	// of ours behind, which is what disable means.
	drop := make(map[int]bool, len(owned))
	for _, i := range owned {
		drop[i] = true
	}
	// An owned line inside a compound command cannot simply vanish.
	//
	// Round 8's MF4. The guarded-rc idiom is `if [ -n "$PS1" ]; then` / `alias bob=…`
	// / `fi`, and our alias is the construct's only statement. Deleting the line left
	// `then` immediately followed by `fi` — a file bash refuses to parse, while disable
	// printed "Disabled." and exited 0. Every later line in the rc silently stops
	// running, and it surfaces at the NEXT login with nothing tying it to the abctl
	// command that caused it. Function bodies (`myfn() {` / `}`) and loops (`for` /
	// `done`) break identically; all three were executed and all three produced a file
	// `bash -n` rejects.
	//
	// The asymmetry is what makes it specifically a disable bug: enable appends the
	// block at file scope and stays valid, so enable→disable over such a file is a
	// corruption ROUND TRIP rather than a pre-existing hazard.
	//
	// Substituting `:` — the shell's no-op, which is a complete command — keeps the
	// construct syntactically whole while leaving nothing of ours behind. It is only
	// done where deletion would actually orphan something, so the common case (a block
	// at file scope) still round-trips byte-identically, which the header promises.
	keepAsNoop := noopSubstitutions(lines, drop)
	out := make([]string, 0, len(lines)-len(owned))
	for i, l := range lines {
		if !drop[i] {
			out = append(out, l)
			continue
		}
		if indent, ok := keepAsNoop[i]; ok {
			out = append(out, indent+":")
		}
	}
	return out, true
}

// noopSubstitutions reports which dropped lines must leave a `:` behind, and with what
// indentation, so that removing them cannot leave a compound command with an empty
// body.
//
// It looks for the enclosing construct rather than parsing the file: walking outward
// from the dropped run, if the nearest non-dropped line above OPENS a compound command
// and the nearest below CLOSES one, then the run is that command's entire body and
// something has to stay. Anything less specific would sprinkle `:` into files that do
// not need it and break the byte-identical round trip.
func noopSubstitutions(lines []string, drop map[int]bool) map[int]string {
	out := map[int]string{}
	for i := range lines {
		if !drop[i] || out[i] != "" {
			continue
		}
		// The contiguous run of dropped lines containing i.
		lo, hi := i, i
		for lo-1 >= 0 && drop[lo-1] {
			lo--
		}
		for hi+1 < len(lines) && drop[hi+1] {
			hi++
		}
		above, below := "", ""
		for j := lo - 1; j >= 0; j-- {
			if t := strings.TrimSpace(lines[j]); t != "" && !isCommentLine(lines[j]) {
				above = t
				break
			}
		}
		for j := hi + 1; j < len(lines); j++ {
			if t := strings.TrimSpace(lines[j]); t != "" && !isCommentLine(lines[j]) {
				below = t
				break
			}
		}
		if opensCompound(above) && closesCompound(below) {
			l := lines[lo]
			out[lo] = l[:len(l)-len(strings.TrimLeft(l, " \t"))]
		}
	}
	return out
}

// opensCompound reports whether a line leaves a compound command expecting a body:
// a `then`/`do`/`else` at its end, or an opening brace or paren.
func opensCompound(t string) bool {
	t = strings.TrimRight(t, " \t")
	for _, kw := range []string{"then", "do", "else", "in", "{", "(", "&&", "||"} {
		if t == kw || strings.HasSuffix(t, " "+kw) || strings.HasSuffix(t, ";"+kw) {
			return true
		}
	}
	return false
}

// closesCompound reports whether a line closes a compound command, so that a body
// removed just above it would leave nothing between the two.
func closesCompound(t string) bool {
	t = strings.TrimSpace(t)
	for _, kw := range []string{"fi", "done", "esac", "}", ")", "elif", "else", ";;"} {
		if t == kw || strings.HasPrefix(t, kw+" ") || strings.HasPrefix(t, kw+";") {
			return true
		}
	}
	return false
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

// resolveRC follows a symlinked rc file to the file that must actually be written.
//
// An rc file is very often a link into a dotfiles repo, and os.Rename over the link
// REPLACES it with a regular file: the alias lands in a file the repo does not
// track, the repo's own copy never gets it, and every later dotfile edit silently
// stops reaching the shell. Backing up beside the link rather than the target is the
// same shape of wrongness, so callers resolve once and use the result for both the
// backup and the write — and for what they tell the user, which is why this is a
// function rather than two lines inside writeRC. (writeSettings does not do this
// because settings.json is rarely symlinked; rc files are symlinked far more often.)
//
// EvalSymlinks fails on a DANGLING link, which is a common real state rather than a
// corrupt one: a dotfiles repo not yet cloned, or stow/chezmoi mid-setup. Falling
// back to the link's own target reaches the same file the shell would, and creates
// it — whereas leaving the path as the link walks straight into the clobber this
// whole function exists to prevent. A relative target resolves against the link's
// directory, the way the kernel reads it.
// Callers that report the indirection to the user should gate on rcIsSymlink rather
// than on resolveRC(p) != p: EvalSymlinks also resolves PARENT directories, so on
// macOS a plain /tmp/rc comes back /private/tmp/rc and announcing that as "a link"
// would be noise about a file the user did not link.
func rcIsSymlink(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

func resolveRC(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	// The fallback LOOPS, because a link to a link is exactly what stow and chezmoi
	// produce mid-setup — the state named above as this function's motivation. A
	// single Readlink stopped at the first hop, so on head -> mid -> (absent) tail the
	// write landed on `mid`, which lost its symlink bit to a regular file while the
	// real target was never created. The cap is what a kernel does for the same
	// reason: a link cycle has no resolution, and the honest answer is to stop rather
	// than spin. Exhausting the cap returns the ORIGINAL path rather than whichever
	// hop we stopped on: a cycle's every hop is a live symlink, and handing one of
	// them back would clobber it — the precise harm this function exists to prevent.
	// Returning the original leaves it to readRC, whose ELOOP is a better report than
	// anything guessed here.
	const maxHops = 32
	orig := path
	for i := 0; i < maxHops; i++ {
		target, err := os.Readlink(path)
		if err != nil {
			// Not a symlink, or unreadable: this is the end of the chain and the file
			// the shell would open. Resolving zero hops returns the original path,
			// which is what a plain file should get.
			return path
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		path = filepath.Clean(target)
	}
	return orig
}

// writeRC backs the file up once, then replaces it atomically.
//
// The same discipline as writeSettings, and the backup-once rule matters more
// here: an rc file is accreted by hand over years, and overwriting the backup on a
// second run would replace the only pristine copy with one this command had
// already edited. (install.sh's cp does overwrite, which is the bug this avoids.)
// The file's existing mode is preserved — an rc file is commonly 0644 and silently
// tightening it to 0600 is a change the user did not ask for.
// path must already be resolved by the caller via resolveRC.
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
	// No resolveRC here: the caller resolves once and passes the result. Resolving
	// again would reopen the window suggestion 1 of the round-4 review names — the
	// message said which file it was about to write, then this function asked the
	// filesystem a second time and could get a different answer if the link moved in
	// between. Now the path the user was shown and the path written are the same
	// value, which is a property no test has to defend.
	// 0644 for a file we are creating, not 0600: the comment above about not
	// tightening an existing rc file's mode applies just as much to the one we make,
	// and every shell's own rc file is world-readable. An existing file's mode wins
	// over this default.
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
	// Only the immediate parent, and only if it is already there. MkdirAll would
	// happily build a whole tree for a dangling link — `ln -s ~/dotfiles/zshrc
	// ~/.zshrc` with the repo not yet cloned silently created three directories at a
	// location the "Adds to ~/.zshrc" message never mentions. An rc file's directory
	// existing is the normal case; conjuring one is a side effect nobody consented to,
	// and the error names the directory so the fix is obvious.
	if dir := filepath.Dir(path); dir != "" {
		if _, err := os.Stat(dir); err != nil {
			return fmt.Errorf("directory %s does not exist: create it first, or point --rc somewhere else", dir)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		// Remove it: after the resolve this sits inside the user's dotfiles repo,
		// where an untracked .tmp is something they may well commit by accident.
		os.Remove(tmp)
		return err
	}
	return nil
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
	// Resolved ONCE, here, and handed to writeRC — so the file this message describes
	// and the file that gets written cannot be two different files. They could before:
	// each called resolveRC separately, straddling the prompt, the mode read, the
	// backup and the rename.
	target := resolveRC(rcPath)
	// The name to print is the path the USER typed, unless the rc file is itself a
	// link — then the backup really does land beside the target, and naming the link
	// would send someone looking for the only pristine copy of a hand-accreted file to
	// a path that does not exist. Gating on rcIsSymlink rather than on target != rcPath
	// is deliberate: resolveRC also resolves parent directories, so a plain /tmp/rc
	// comes back /private/tmp/rc and announcing that would be noise about a file the
	// user did not link.
	written := rcPath
	if rcIsSymlink(rcPath) {
		written = target
		fmt.Fprintf(stdout, "%s is a link to %s, which is what gets written.\n", rcPath, written)
	}
	// One Stat, shared: whether the file exists decides both halves of this sentence.
	// Unconditionally promising a .bak was a consent prompt offering a rollback
	// artifact that would not exist — on the FIRST run of the common case, since
	// bobShellRCPath hands back ~/.zshrc whether or not it is there, and on a dangling
	// link too. writeRC only backs up what it could read, so there is nothing to keep
	// a copy of, and nothing else in the file to leave alone either.
	if _, serr := os.Stat(target); serr == nil {
		fmt.Fprintf(stdout, "Nothing else in the file changes; a copy is kept as %s.bak\n\n", written)
	} else {
		fmt.Fprintf(stdout, "%s does not exist yet; it will be created with just this block.\n\n", written)
	}
	if !yes && !confirmFn(stdout) {
		fmt.Fprintln(stdout, "Not changed.")
		return exitDeclined
	}

	if err := writeRC(target, updated, trailingNewline); err != nil {
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
	// Exactly the lines the write removes — the OWNED SET, not its
	// hull. This is a consent prompt for a destructive edit, so the one property it
	// must have is that it describes the edit. The hull does not: on a file whose
	// owned lines are non-contiguous it spans the user's lines between ours, so the
	// prompt listed `export SECRET_TOKEN=...` and `source ~/.work_secrets` as being
	// removed while the write left them untouched. Ten lines shown, two removed. Both
	// ways of misreading that prompt are bad — declining a safe operation, or
	// believing secrets were just destroyed — and neither is recoverable by reading
	// the code.
	//
	// (Printing the removed lines rather than bobShellAliasesIn is still right, for
	// the earlier reason: on a marker-damaged block that helper finds no alias and
	// printed an empty body while the write removed real lines.)
	var removed []string
	for _, i := range ownedLines(lines) {
		removed = append(removed, lines[i])
	}
	fmt.Fprintf(stdout, "Removes from %s:\n%s\n", rcPath, indentBlock(strings.Join(removed, "\n")+"\n"))
	// Resolved once and passed to writeRC, for the reason enable states: the path
	// described and the path written are then the same value.
	target := resolveRC(rcPath)
	if rcIsSymlink(rcPath) {
		fmt.Fprintf(stdout, "%s is a link to %s, which is what gets written.\n", rcPath, target)
	}
	if !yes && !confirmFn(stdout) {
		fmt.Fprintln(stdout, "Not changed.")
		return exitDeclined
	}
	if err := writeRC(target, updated, trailingNewline); err != nil {
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
	aliases := bobShellAliasesIn(lines)
	if len(aliases) == 0 {
		// No "the block is there but has no alias line" case, because markers alone are
		// never owned: ownedLinesFor claims a marker only when it is adjacent to a live
		// alias of ours, so there is no state where we hold markers and no alias.
		//
		// This comment used to describe the round-6 pair scan, which round 7 deleted —
		// it survived the deletion and then asserted the opposite of shipped behaviour,
		// which round 8 flagged. What makes the claim true now is adjacency, not pairing.
		// Reporting a marker-only file as enabled would mean claiming a user's `--help`
		// transcript as our block.
		fmt.Fprintf(stdout, "not enabled in %s\n", rcPath)
		return 0
	}
	for _, a := range aliases {
		fmt.Fprintf(stdout, "  %s\n", a)
	}
	// More than one is a state a hand-edit can reach, and reporting only the first
	// would describe a file the user does not have. Whichever the shell takes, enable
	// collapses them to one.
	if len(aliases) > 1 {
		fmt.Fprintf(stdout, "enabled in %s, but with %d alias lines — the shell uses the last one.\n", rcPath, len(aliases))
		fmt.Fprintln(stdout, "  Re-run `abctl configure bobshell enable` to collapse them to one.")
		return 0
	}
	alias := aliases[0]
	// A block naming an abctl that is no longer where it was — reinstalled
	// elsewhere, or moved — aliases bob to a path that may not exist. Enable
	// rewrites it, and saying so here is cheaper than debugging it from the
	// "command not found" the alias itself would produce.
	if self := bobShellSelfPath; self != "" {
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
