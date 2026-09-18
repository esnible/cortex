package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/rossoctl/cortex/authbridge/cmd/abctl/tui"
)

// sessionMetaRel is where the harvested metadata lands, relative to the user's home.
//
// A sibling of abctl-config.yaml and claude-code-state.json, in the same ~/.cortex
// tree rather than a new dotfile: it is abctl's own bookkeeping about an agent, not
// something the agent or the proxy reads. Follows userCfgRel / cortexCfgRel /
// stateRel so there is one tree to find and one to delete.
const sessionMetaRel = ".cortex/session-metadata.json"

// claudeConfigDirEnv is the variable Claude Code itself honours for relocating its
// config tree. Read here so a user who has moved it is not told there are no
// sessions; nothing else in abctl consults it today, which is why the two commands
// that read ~/.claude resolve it differently — see the follow-up in the PR.
const claudeConfigDirEnv = "CLAUDE_CONFIG_DIR"

const experimentalUsage = `abctl experimental — unstable helpers, no compatibility promise

Usage:
  abctl experimental read-claude-sessions [--dir PATH]

Actions:
  read-claude-sessions   harvest session titles from Claude Code's own transcripts
                         into ~/.cortex/session-metadata.json. Run
                         "abctl experimental read-claude-sessions --help" for the detail.

Everything under this verb may change or disappear in any release, including the
shape of any file it writes. The namespace exists so a half-built idea can ship and
be used without the rest of abctl having to promise it forever — if something here
proves itself, it graduates to its own subcommand and this spelling goes away.

Exit status: 0 done, 1 something went wrong, 2 a usage error.
`

const readClaudeSessionsUsage = `abctl experimental read-claude-sessions — name Cortex's sessions from Claude Code's transcripts

Usage:
  abctl experimental read-claude-sessions [--dir PATH]

Cortex buckets traffic by session id, and a session id is a UUID. Claude Code knows
more about the same session: it writes a transcript per session under its config
directory, carrying a model-generated title and the directory the session ran in.
This reads those transcripts and writes what it finds to
~/.cortex/session-metadata.json, keyed by the same UUID Cortex uses — so a later
reader can put a name next to a row.

Each entry carries a title, the agent type ("Claude Code"), the config directory it
came from, and the transcript it was read from. The title is the transcript's
model-generated one when it has one, and otherwise the working directory: most
sessions never get a title, so most entries name a directory.

The config directory is ` + claudeConfigDirEnv + ` when set, and ~/.claude otherwise.
Only transcripts one level down (projects/<project>/<id>.jsonl) are read: deeper
files are subagent transcripts, whose names are not session ids.

The file is rewritten whole on every run — this reports what Claude Code holds now,
not a history. Nothing in Cortex reads it yet.

Flags:
  --dir PATH   config directory to read instead of ` + claudeConfigDirEnv + ` / ~/.claude

Exit status: 0 done, 1 the directory could not be read or the file could not be
written, 2 a usage error.
`

// runExperimental dispatches on the action name. Returns the process exit code.
func runExperimental(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, experimentalUsage)
		return 2
	}
	action := args[0]
	// Same shape as `abctl configure` and `abctl service`: an explicit --help is a
	// request for the list, so it must not be read as the name of an action that does
	// not exist. Explicit help goes to stdout at exit 0, which is what makes it
	// pipeable; a missing or wrong action stays an error on stderr at exit 2.
	switch action {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, experimentalUsage)
		return 0
	}

	switch action {
	case "read-claude-sessions":
		return runReadClaudeSessions(args[1:], stdout, stderr)
	default:
		// The named list answers a typo; the usage block after it answers "what else
		// can this do", which is what someone who guessed wrong most likely wanted.
		fmt.Fprintf(stderr, "abctl: unknown experimental action %q (read-claude-sessions)\n", action)
		fmt.Fprint(stderr, experimentalUsage)
		return 2
	}
}

// runReadClaudeSessions harvests Claude Code's transcripts into
// ~/.cortex/session-metadata.json. Returns the process exit code.
func runReadClaudeSessions(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("experimental read-claude-sessions", flag.ContinueOnError)
	fs.SetOutput(stderr)
	// Silenced, and printed from the Parse result instead: Parse calls Usage for a bad
	// flag as well as for -h, and only its return value says which happened. Scanning
	// argv for "--help" cannot tell them apart, because a flag may consume it as a
	// VALUE — `--dir --help` would then put the whole usage on stdout while the error
	// went to stderr, splitting one failure across both streams.
	fs.Usage = func() {}
	dir := fs.String("dir", "", "Claude Code config directory")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, readClaudeSessionsUsage)
			return 0
		}
		fmt.Fprint(stderr, readClaudeSessionsUsage)
		return 2
	}
	// Positional arguments are a mistake, not something to ignore: this command takes
	// none, so `read-claude-sessions ~/.claude` most likely means the user meant --dir
	// and silently reading the default instead is a worse answer than saying so.
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "abctl: unexpected argument %q (did you mean --dir %s?)\n", fs.Arg(0), fs.Arg(0))
		return 2
	}

	configDir := *dir
	if configDir == "" {
		var err error
		if configDir, err = defaultClaudeConfigDir(); err != nil {
			fmt.Fprintf(stderr, "abctl: %v\n", err)
			return 1
		}
	}

	meta, err := readClaudeSessions(configDir)
	if err != nil {
		fmt.Fprintf(stderr, "abctl: reading %s: %v\n", configDir, err)
		return 1
	}

	path, err := sessionMetadataPath()
	if err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}
	if err := saveSessionMetadata(path, meta); err != nil {
		fmt.Fprintf(stderr, "abctl: writing %s: %v\n", path, err)
		return 1
	}

	// Reported rather than silent: the count is the only way to notice that a wrong
	// --dir found nothing, and zero is not an error — a machine that has never run
	// Claude Code legitimately has no transcripts.
	fmt.Fprintf(stdout, "Wrote %d session(s) to %s\n", len(meta), path)
	if len(meta) == 0 {
		fmt.Fprintf(stdout, "No transcripts under %s/projects — is that the right config directory?\n", configDir)
	}
	return 0
}

// defaultClaudeConfigDir resolves Claude Code's config directory: CLAUDE_CONFIG_DIR
// when set, else ~/.claude.
func defaultClaudeConfigDir() (string, error) {
	if d := os.Getenv(claudeConfigDirEnv); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine your home directory: %w", err)
	}
	if home == "" {
		// A separate branch, not folded into the one above: %w on a nil error renders
		// as "%!w(<nil>)", which would make this path unreadable to whoever hits it.
		// Same shape as userConfigPath.
		return "", errors.New("cannot determine your home directory: it is empty")
	}
	return filepath.Join(home, ".claude"), nil
}

// sessionMetadataPath returns ~/.cortex/session-metadata.json.
func sessionMetadataPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine your home directory: %w", err)
	}
	if home == "" {
		return "", errors.New("cannot determine your home directory: it is empty")
	}
	return filepath.Join(home, sessionMetaRel), nil
}

// readClaudeSessions harvests every session transcript under configDir/projects.
//
// One level down only — projects/<project>/<id>.jsonl. Deliberately not
// filepath.WalkDir, which the rest of this module uses: deeper files are subagent
// transcripts (<id>/subagents/agent-*.jsonl) whose basenames are agent ids, not
// session ids, so walking would key 92 non-sessions into the map on the machine this
// was written against.
//
// A missing projects directory is not an error: a machine that has never run Claude
// Code has none, and an empty result already says so.
func readClaudeSessions(configDir string) (map[string]tui.SessionMetadata, error) {
	projects := filepath.Join(configDir, "projects")
	entries, err := os.ReadDir(projects)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]tui.SessionMetadata{}, nil
		}
		return nil, err
	}

	out := make(map[string]tui.SessionMetadata)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		projDir := filepath.Join(projects, e.Name())
		files, err := os.ReadDir(projDir)
		if err != nil {
			// One unreadable project directory must not lose the other hundred.
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(projDir, f.Name())
			title := titleFromTranscript(path)
			out[strings.TrimSuffix(f.Name(), ".jsonl")] = tui.SessionMetadata{
				Title:          title,
				AgentType:      "Claude Code",
				AgentConfigDir: configDir,
				LogFile:        path,
			}
		}
	}
	return out, nil
}

// titleFromTranscript reads one transcript and returns the best name for it.
//
// The model-generated title when the transcript carries one, otherwise the working
// directory it ran in, otherwise "". Both are LAST-wins: a session can be titled more
// than once and can change directory, and the newest claim is the current one.
//
// Returns "" rather than an error for an unreadable or malformed transcript. A
// missing title costs a label; refusing the whole harvest over one bad file would
// cost every other name. That is the opposite of toolscan's choice on the same files,
// and deliberately so: there, under-reporting makes it propose removing MORE tools,
// so it must fail loudly.
func titleFromTranscript(path string) string {
	f, err := os.Open(path) //nolint:gosec // operator-supplied transcript dir
	if err != nil {
		return ""
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// Transcript lines routinely exceed the default 64KB — the largest single line
	// measured locally was 1.36MB — and a Scanner past its limit stops silently, which
	// would drop the title of exactly the longest sessions. Same buffer as
	// toolscan.scanFile.
	sc.Buffer(make([]byte, 0, 256*1024), 16*1024*1024)

	var title, cwd string
	for sc.Scan() {
		line := sc.Bytes()
		// Hot path: most lines are conversation turns carrying neither field. A
		// substring test over the raw bytes is far cheaper than parsing them, and
		// bytes.Contains avoids the copy that strings.Contains(string(line), …) makes.
		if !bytes.Contains(line, []byte(`"aiTitle"`)) && !bytes.Contains(line, []byte(`"cwd"`)) {
			continue
		}
		var e transcriptMeta
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		if e.Cwd != "" {
			cwd = e.Cwd
		}
		// Guarded on Type as well as on the value: "aiTitle" appearing on some other
		// line kind is not a title claim.
		if e.Type == "ai-title" && e.AiTitle != "" {
			title = e.AiTitle
		}
	}
	// sc.Err() deliberately unchecked: a truncated read yields whatever was found
	// before it, which is a better answer than none, and the fallbacks already cover
	// finding nothing. The buffer above is what makes that rare.
	if title != "" {
		return title
	}
	return cwd
}

// transcriptMeta is the minimum shape needed from a transcript line. Decoding only
// these fields keeps the parse cheap on multi-megabyte lines.
type transcriptMeta struct {
	Type    string `json:"type"`
	AiTitle string `json:"aiTitle"`
	Cwd     string `json:"cwd"`
}

// saveSessionMetadata writes the map atomically, creating ~/.cortex if needed.
//
// Same mechanics as saveUserConfig, for the same reasons: CreateTemp rather than a
// fixed path+".tmp" (crash debris at a predictable name wedges every later write, and
// CreateTemp's unpredictable name passes O_EXCL itself), Rename rather than truncate
// (a half-written file would be read back as malformed), dir 0700 and file 0600.
//
// SECURITY, considered rather than skipped: this does NOT use tui's checkYankDir
// Lstat sweep, though saveUserConfig's comment names "a filesystem path" as the
// trigger for it and two fields here are paths. The sweep exists because yanked
// events carry content only abctl saw — identity subjects, raw completions. These
// paths name the reader's own ~/.claude tree, which they can already list, and the
// titles come from their own transcripts. A redirected write leaks nothing they lack.
// What would change that: harvesting an agent whose config lives somewhere the user
// cannot read, or adding any field carrying transcript CONTENT rather than a path.
func saveSessionMetadata(path string, meta map[string]tui.SessionMetadata) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')

	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// err, not a fresh `err :=`: a scoped one would be discarded and Close and Rename
	// would then see success, renaming a truncated file over the good one. The bug
	// saveUserConfig's comment records having made.
	_, err = writeAll(f, body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// CreateTemp is 0600 already; chmod is belt-and-braces against a umask surprise.
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
