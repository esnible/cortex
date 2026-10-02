package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/rossoctl/cortex/cmd/agentop/checklist"
)

const setupUsage = `agentop setup — install or repair Cortex on this machine, after one question

Usage:
  agentop setup [--from DIR] [--claude-code] [--yes] [--no-service]
                [--install-only] [--no-modify-path] [--restart]

Plans every change first and changes nothing until you agree; a failure or a
Ctrl-C undoes what it did. Without --from it repairs what is installed.

  --from DIR         install the binaries in DIR (the installer's staging dir,
                     or ./bin for a source build)
  --claude-code      route Claude Code through Cortex
  --yes, -y          do not ask; needed when there is no terminal
  --no-service       run the proxy without launchd/systemd
  --install-only     install the binaries and PATH only
  --no-modify-path   never edit a shell profile
  --restart          restart the service even when nothing changed

Exit status: 0 done or already current, 1 failed (and rolled back), 2 usage,
3 declined, interrupted before any change, or no terminal to ask on.
`

// setupStepsHook lets tests inject a failure into the real step list.
var setupStepsHook = func(s []step) []step { return s }

// setupHasTerminal reports whether there is a terminal to ask on. A var for the
// reason claudeCodeConfirm gives.
var setupHasTerminal = func() bool {
	f, err := os.Open("/dev/tty")
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// setupConfirm asks setup's one question, on /dev/tty because stdin is the
// installer script. A var for the reason claudeCodeConfirm gives.
var setupConfirm = func(w io.Writer) bool {
	tty, err := os.Open("/dev/tty")
	if err != nil {
		return false
	}
	defer func() { _ = tty.Close() }()
	return setupConfirmFrom(tty, w)
}

// setupSignals is the channel Ctrl-C and SIGTERM arrive on, and the func that
// stops them arriving. A var so tests can send one without signalling themselves.
var setupSignals = func() (<-chan os.Signal, func()) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	return c, func() { signal.Stop(c) }
}

// setupConfirmFrom reads the answer. Enter means yes: the user typed the install
// command and the list of changes is right above the prompt. EOF is not consent.
func setupConfirmFrom(r io.Reader, w io.Writer) bool {
	fmt.Fprint(w, "  Continue? [Y/n] ")
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true
	}
	return false
}

func parseSetupFlags(args []string, stderr io.Writer) (setupOptions, error) {
	var o setupOptions
	fs := newFlagSet("setup", stderr)
	fs.Usage = func() { fmt.Fprint(stderr, setupUsage) }
	fs.StringVar(&o.from, "from", "", "")
	fs.BoolVar(&o.claudeCode, "claude-code", false, "")
	fs.BoolVar(&o.yes, "yes", false, "")
	fs.BoolVar(&o.yes, "y", false, "")
	fs.BoolVar(&o.noService, "no-service", false, "")
	fs.BoolVar(&o.installOnly, "install-only", false, "")
	fs.BoolVar(&o.noModifyPath, "no-modify-path", false, "")
	fs.BoolVar(&o.restart, "restart", false, "")
	fs.Int64Var(&o.handoffBytes, "handoff-bytes", 0, "")
	fs.IntVar(&o.handoffSeconds, "handoff-seconds", 0, "")
	_ = fs.Bool("local", false, "") // install.sh passes it; local is the only mode
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return o, nil
}

func newSetupEnv(opts setupOptions) (*setupEnv, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil, fmt.Errorf("cannot determine your home directory (is $HOME set?)")
	}
	env := &setupEnv{
		home: home, goos: runtime.GOOS, shell: os.Getenv("SHELL"), pathEnv: os.Getenv("PATH"),
		binDir: filepath.Join(home, ".local", "bin"), cortexDir: filepath.Join(home, ".cortex"), opts: opts,
	}
	if opts.from != "" {
		abs, err := filepath.Abs(opts.from)
		if err != nil {
			return nil, err
		}
		env.fromDir = abs
	}
	return env, nil
}

func buildSetupSteps(env *setupEnv) []step {
	steps := []step{binariesStep{}, pathStep{}}
	if env.opts.installOnly {
		return append(steps, cleanupStep{afterService: false})
	}
	steps = append(steps, configStep{}, serviceStep{})
	if env.opts.claudeCode {
		steps = append(steps, claudeCodeStep{})
	}
	return append(steps, cleanupStep{afterService: true})
}

func runSetup(args []string, stdout, stderr io.Writer) int {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			fmt.Fprint(stdout, setupUsage)
			return 0
		}
	}
	opts, err := parseSetupFlags(args, stderr)
	if err != nil {
		return 2 // flags that did not parse name no stage, so it stays
	}
	// Caught from here on, rather than killing setup: install.sh exec'd it, past its
	// own EXIT trap, so the staging dir goes on every way out, a signal's included.
	// Before apply nothing has changed, so a signal is a decline; during it, a
	// rollback after the current step.
	sigs, stopSignals := setupSignals()
	defer stopSignals()
	if stage := installerStage(opts); stage != "" {
		defer func() { _ = os.RemoveAll(stage) }()
	}
	env, err := newSetupEnv(opts)
	if err != nil {
		fmt.Fprintf(stderr, "agentop: %v\n", err)
		return 1
	}
	ui := checklist.New(stdout, isTerminal(stdout))
	defer ui.Close()
	started := time.Now()

	printSetupHeader(env, ui)
	if opts.handoffBytes > 0 {
		ui.Done("downloaded", downloadSize(opts.handoffBytes)+" · sha256 verified",
			time.Duration(opts.handoffSeconds)*time.Second)
	}
	ui.Blank()

	planned, problems := planSteps(env, setupStepsHook(buildSetupSteps(env)))
	select {
	case <-sigs:
		ui.Plain("Not changed.")
		return exitDeclined
	default:
	}
	if len(problems) > 0 {
		for _, p := range problems {
			fix := make([]string, 0, len(p.fix))
			for _, f := range p.fix {
				fix = append(fix, "fix: "+f)
			}
			ui.Fail(p.label, p.reason, fix...)
		}
		ui.Blank()
		ui.Plain("Nothing was changed.")
		return 1
	}
	if allDone(planned) {
		if opts.installOnly {
			ui.Plain(fmt.Sprintf("cortex %s is installed.", version))
		} else {
			ui.Plain(fmt.Sprintf("cortex %s is installed and healthy.", version))
		}
		return 0
	}
	if !opts.yes {
		ui.Consent(consentItems(planned))
		ui.Blank()
		if hint := undoHint(env); hint != "" {
			ui.Faint(hint)
			ui.Blank()
		}
		if !setupHasTerminal() {
			ui.Plain("No terminal to ask on — re-run with --yes to apply.")
			return exitDeclined
		}
		// Read aside, so a Ctrl-C at the prompt declines rather than waiting on the
		// terminal. The reader is left blocked: setup exits soon after.
		answer := make(chan bool, 1)
		go func() { answer <- setupConfirm(stdout) }()
		select {
		case yes := <-answer:
			if !yes {
				ui.Plain("Not changed.")
				return exitDeclined
			}
		case <-sigs:
			ui.Blank() // ends the prompt's line
			ui.Plain("Not changed.")
			return exitDeclined
		}
		ui.Blank()
	}

	if !applySteps(env, ui, planned, sigs) {
		return 1
	}
	for _, f := range env.onSuccess {
		f()
	}
	printSetupEnding(env, ui, time.Since(started))
	return 0
}

// downloadSize is the installer's byte count in decimal units, as the TUI's
// formatBytes shows one: a download under a megabyte in kB, so it never reads as
// "0.0 MB", and the tier promoted where the rounding carries, so none prints 1000.0.
func downloadSize(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d B", n)
	case n < 999_950:
		return fmt.Sprintf("%.1f kB", float64(n)/1e3)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/1e6)
}

func printSetupHeader(env *setupEnv, ui *checklist.UI) {
	title := "rosso cortex · setup"
	if env.opts.fromInstaller() {
		title = "rosso cortex · installer"
	}
	right := version + " · " + env.goos + "/" + runtime.GOARCH
	if prev := installedVersion(filepath.Join(env.binDir, "agentop")); env.fromDir != "" && prev != "" && prev != version {
		right = prev + " → " + right
	}
	if env.fromDir != "" && !env.opts.fromInstaller() {
		right += " · from " + env.tilde(env.fromDir)
	}
	ui.Header(title, right)
}

// undoHint names the commands that undo this run, as of PR 2; PR 3's
// `agentop uninstall` replaces it.
func undoHint(env *setupEnv) string {
	if env.opts.installOnly {
		return ""
	}
	hint := "Undo any time: agentop service uninstall"
	if env.opts.claudeCode {
		hint += " · agentop configure claude-code disable"
	}
	return hint
}

func printSetupEnding(env *setupEnv, ui *checklist.UI, took time.Duration) {
	agentop := "agentop"
	if !env.binOnPath && !env.binOnPathNewTerms {
		agentop = filepath.Join(env.binDir, "agentop")
	}
	if env.opts.installOnly {
		ui.Blank()
		ui.Plain("Installed. Start Cortex with:  " + agentop + " setup")
		return
	}
	ready := "cortex " + version + " ready."
	if ui.Animated() {
		ready = "cortex " + version + " ready in " + checklist.FormatDuration(took)
	}
	if !env.freshInstall {
		ui.Blank()
		ui.Plain(ready)
		return
	}
	if ui.Animated() {
		ui.Wordmark("ready in " + checklist.FormatDuration(took))
	} else {
		ui.Blank()
		ui.Plain(ready)
	}
	cmd, comment := agentop, "watch your agent traffic live"
	if !env.opts.claudeCode {
		cmd, comment = agentop+" exec -- <cmd>", "send an agent through Cortex"
	}
	ui.Next("Open a new terminal, then:", cmd, comment)
	ui.Blank()
	if env.startedBackground {
		ui.Faint("Cortex runs without a supervisor here; stop it with: kill $(cat ~/.cortex/proxy.pid)")
	}
	ui.Faint(undoHint(env))
}

// systemTempRoots are the temp dirs, besides ~/.cortex/tmp, that the installer
// stages into. A var because a test's HOME is itself under one of them.
var systemTempRoots = func() []string { return []string{os.TempDir(), "/tmp"} }

// installerStage is the dir setup deletes on its way out: --from, when the
// installer invoked setup and staged into it. Worked out from the flags alone, so
// a HOME that cannot be resolved still has the system temp roots to go by.
func installerStage(opts setupOptions) string {
	if !opts.fromInstaller() || opts.from == "" {
		return ""
	}
	dir, err := filepath.Abs(opts.from)
	if err != nil {
		return ""
	}
	home, _ := os.UserHomeDir()
	if !isStagingDir(dir, home) {
		return ""
	}
	return dir
}

// isStagingDir reports whether dir is one the installer staged into — strictly
// inside $TMPDIR, /tmp or ~/.cortex/tmp — and so one setup may delete. A source
// tree's ./bin never is.
func isStagingDir(dir, home string) bool {
	if dir == "" {
		return false
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	for _, root := range append(systemTempRoots(), filepath.Join(home, ".cortex", "tmp")) {
		r, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(r, real)
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(f.Fd())
}
