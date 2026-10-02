package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/rossoctl/cortex/cmd/agentop/checklist"
)

// setupOptions are agentop setup's flags — the interface install.sh hands off
// through, frozen here so PR 4's script needs none that this binary lacks.
type setupOptions struct {
	from           string
	claudeCode     bool
	yes            bool
	noService      bool
	installOnly    bool
	noModifyPath   bool
	restart        bool
	handoffBytes   int64
	handoffSeconds int
}

// fromInstaller reports whether install.sh invoked setup: only it sets the
// hidden handoff flags.
func (o setupOptions) fromInstaller() bool { return o.handoffBytes > 0 || o.handoffSeconds > 0 }

// setupEnv is one run's machine and what the plans found out about it.
type setupEnv struct {
	home, goos, shell, pathEnv string
	binDir, cortexDir, fromDir string
	opts                       setupOptions

	// Established while planning; later plans and the ending read them.
	freshInstall      bool     // no agentop in binDir before this run
	installedVersion  string   // what binDir/agentop reported before this run
	binaryChanges     []string // file names the binaries step replaces
	binOnPath         bool     // binDir is on this shell's PATH
	binOnPathNewTerms bool     // binDir is, or will be, on PATH in new terminals
	pathProfile       string   // the profile the PATH step edits
	configFresh       bool     // no config.yaml before this run
	configWillChange  bool     // the config step migrates an existing config
	configPinsPending bool     // the migration adds listener pins, which a running proxy takes only on a restart
	unsupervised      bool     // the proxy runs without launchd/systemd
	priorService      bool     // a supervised Cortex was serving before this run

	// Set while applying.
	restorePrior func() error // brings back the Cortex that served before, after a rollback
	priorDesc    string
	onSuccess    []func() // run once every step has applied

	configPinsChanged bool // the config step added listener pins, so the service must restart
	startedBackground bool // the service step started a background proxy, so the ending says how to stop it
}

func (e *setupEnv) configPath() string  { return filepath.Join(e.cortexDir, "config.yaml") }
func (e *setupEnv) previousDir() string { return filepath.Join(e.cortexDir, "previous") }

// tilde shows a path under HOME as ~/…, as every line setup prints does.
func (e *setupEnv) tilde(p string) string {
	rel, err := filepath.Rel(e.home, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return filepath.Join("~", rel)
}

// tildeText is s with each path under HOME shown as ~/…, for the lines setup
// passes on from elsewhere: a log, or service install's own messages. HOME counts
// only as a whole path, so /Users/al does not shorten /Users/alice.
func (e *setupEnv) tildeText(s string) string {
	if e.home == "" || e.home == "/" {
		return s
	}
	var b strings.Builder
	for {
		i := strings.Index(s, e.home)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		j := i + len(e.home)
		whole := (i == 0 || !isPathByte(s[i-1])) && (j == len(s) || s[j] == '/' || !isPathByte(s[j]))
		b.WriteString(s[:i])
		if whole {
			b.WriteString("~")
		} else {
			b.WriteString(e.home)
		}
		s = s[j:]
	}
}

func isPathByte(c byte) bool {
	return c == '/' || c == '.' || c == '_' || c == '-' ||
		'0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

// stepPlan is what one step would do, worked out before anything changes.
type stepPlan struct {
	label   string   // the checklist label
	verb    string   // consent row: install, replace, add, write, update, run, route, remove
	what    string   //
	where   string   //
	done    bool     // nothing to change: a dim "·" line, or "!" when advice is set
	doneMsg string   //
	advice  *problem // a non-fatal note shown in place of the "·" line
	hidden  bool     // nothing to change and nothing worth a line
}

// problem is why a step cannot go ahead, or what the user should do, with the remedy.
type problem struct {
	label  string
	reason string
	fix    []string
}

// undo reverses one applied step; fn is nil when there is nothing to reverse.
type undo struct {
	label  string
	fn     func() error
	manual string // what the user can run or do by hand if fn fails
}

// step is one unit of agentop setup. plan must not change anything on disk. apply
// returns the undo for whatever it changed — also when it fails part way, so the
// runner can reverse the part that happened.
type step interface {
	name() string
	plan(env *setupEnv) (stepPlan, *problem)
	apply(env *setupEnv, act *checklist.Running) (detail string, u undo, err error)
}

type plannedStep struct {
	s step
	p stepPlan
}

// planSteps plans every step, carrying on past a problem so the user sees all of
// them at once.
func planSteps(env *setupEnv, steps []step) ([]plannedStep, []*problem) {
	var out []plannedStep
	var probs []*problem
	for _, s := range steps {
		p, prob := s.plan(env)
		if p.label == "" {
			p.label = s.name()
		}
		if prob != nil {
			if prob.label == "" {
				prob.label = p.label
			}
			probs = append(probs, prob)
			continue
		}
		out = append(out, plannedStep{s, p})
	}
	return out, probs
}

func allDone(ps []plannedStep) bool {
	for _, p := range ps {
		if !p.p.done {
			return false
		}
	}
	return true
}

func consentItems(ps []plannedStep) []checklist.Item {
	var items []checklist.Item
	for _, p := range ps {
		if !p.p.done {
			items = append(items, checklist.Item{Verb: p.p.verb, What: p.p.what, Where: p.p.where})
		}
	}
	return items
}

// applySteps applies each planned change in order. After a failure, or a signal
// between steps, it undoes what it applied in reverse and brings back the Cortex
// that was serving before. It reports whether every planned change applied.
func applySteps(env *setupEnv, ui *checklist.UI, ps []plannedStep, interrupted <-chan os.Signal) bool {
	var undos []undo
	for _, p := range ps {
		switch {
		case p.p.hidden:
			continue
		case p.p.done && p.p.advice != nil:
			ui.Advise(p.p.label, p.p.advice.reason, p.p.advice.fix...)
			continue
		case p.p.done:
			ui.Already(p.p.label, p.p.doneMsg)
			continue
		}
		act := ui.Start(p.p.label)
		detail, u, err := p.s.apply(env, act)
		if u.fn != nil {
			undos = append(undos, u)
		}
		if err != nil {
			var se stepError
			if errors.As(err, &se) {
				act.Fail(se.reason, se.detail...)
			} else {
				reason, rest := errorLines(err)
				act.Fail(reason, rest...)
			}
			rollback(env, ui, undos)
			return false
		}
		act.Done(detail)
		select {
		case <-interrupted:
			ui.Blank()
			ui.Plain("Interrupted.")
			rollback(env, ui, undos)
			return false
		default:
		}
	}
	return true
}

// errorLines splits an error into its first line, the reason, and the lines after
// it, so a joined error prints as one marked line with its detail indented below.
func errorLines(err error) (string, []string) {
	lines := strings.Split(strings.TrimRight(err.Error(), "\n"), "\n")
	var rest []string
	for _, l := range lines[1:] {
		if l != "" {
			rest = append(rest, l)
		}
	}
	return lines[0], rest
}

// rollback runs the undos newest first, then restarts the Cortex that was serving
// before — after the undos, because the binaries step's undo runs last and the old
// service needs its binary back first. Each undo that works gets a dim line; each
// that fails is listed with its error and, where known, the manual fix.
func rollback(env *setupEnv, ui *checklist.UI, undos []undo) {
	type failure struct {
		label, manual string
		err           error
	}
	var failed []failure
	for i := len(undos) - 1; i >= 0; i-- {
		if err := undos[i].fn(); err != nil {
			failed = append(failed, failure{undos[i].label, undos[i].manual, err})
		} else {
			ui.Note("undone", undos[i].label)
		}
	}
	if env.restorePrior != nil {
		if err := env.restorePrior(); err != nil {
			failed = append(failed, failure{"the previous Cortex", "agentop service restart", err})
		} else {
			ui.Note("reverted", env.priorDesc)
		}
	}
	if len(failed) == 0 {
		if len(undos) == 0 && env.restorePrior == nil {
			// Nothing had been applied that needed reversing.
			ui.Note("rolled back", "nothing had been changed")
		} else {
			ui.Note("rolled back", "the changes above are undone")
		}
		return
	}
	for _, f := range failed {
		reason, rest := errorLines(f.err)
		ui.Note("could not roll back", f.label+": "+reason)
		// Faint adds two spaces: these sit at Fail's detail indent of six.
		for _, l := range rest {
			ui.Faint("    " + l)
		}
		if f.manual != "" {
			ui.Faint("    do it yourself: " + f.manual)
		}
	}
}
