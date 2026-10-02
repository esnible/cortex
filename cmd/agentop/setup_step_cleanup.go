package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/rossoctl/cortex/cmd/agentop/checklist"
)

// staleBinaries are the pre-rename names, newest name beside each.
var staleBinaries = []struct{ old, now string }{{"abctl", "agentop"}, {"authbridge-proxy", "cortex"}}

// cleanupStep is install.sh's remove_stale. afterService admits authbridge-proxy,
// which install.sh removes only once the service has moved to cortex.
type cleanupStep struct{ afterService bool }

func (cleanupStep) name() string { return "cleaned up" }

func (c cleanupStep) candidates(env *setupEnv) []string {
	var out []string
	for _, sb := range staleBinaries {
		if sb.old == "authbridge-proxy" && !c.afterService {
			continue
		}
		if isOurStaleBinary(filepath.Join(env.binDir, sb.old), sb.old) {
			out = append(out, sb.old)
		}
	}
	return out
}

func (c cleanupStep) plan(env *setupEnv) (stepPlan, *problem) {
	p := stepPlan{label: "cleaned up"}
	names := c.candidates(env)
	if len(names) == 0 {
		p.done, p.hidden = true, true
		return p, nil
	}
	p.verb, p.what, p.where = "remove", "pre-rename "+strings.Join(names, ", "), env.tilde(env.binDir)
	return p, nil
}

func (c cleanupStep) apply(env *setupEnv, _ *checklist.Running) (string, undo, error) {
	var restores []func() error
	u := undo{label: "pre-rename binaries", fn: func() error {
		var errs []error
		for i := len(restores) - 1; i >= 0; i-- {
			errs = append(errs, restores[i]())
		}
		return errors.Join(errs...)
	}, manual: "copy the files in " + env.tilde(env.previousDir()) + "/ back into " + env.tilde(env.binDir)}
	var removed, kept []string
	for _, old := range c.candidates(env) {
		path := filepath.Join(env.binDir, old)
		if unitStillNames(env, old) ||
			(old == "authbridge-proxy" && pidfileProcessUnnamed(filepath.Join(env.cortexDir, "proxy.pid"))) {
			kept = append(kept, old)
			continue
		}
		r, err := moveToPrevious(env, path)
		if r != nil {
			restores = append(restores, r)
		}
		if err != nil {
			return "", u, err
		}
		removed = append(removed, old)
	}
	env.onSuccess = append(env.onSuccess, func() { _ = os.RemoveAll(env.previousDir()) })
	var parts []string
	if len(removed) > 0 {
		parts = append(parts, "removed "+strings.Join(removed, ", "))
	}
	if len(kept) > 0 {
		parts = append(parts, "kept "+strings.Join(kept, ", ")+": the service still runs it")
	}
	return strings.Join(parts, "; "), u, nil
}

// moveToPrevious moves path into previous/ — a copy, then a remove, so it works
// when the two are on different filesystems — and returns the func that copies it
// back and removes the copy. The func is nil when nothing was moved.
func moveToPrevious(env *setupEnv, path string) (func() error, error) {
	prev := filepath.Join(env.previousDir(), filepath.Base(path))
	if err := os.MkdirAll(env.previousDir(), 0o700); err != nil {
		return nil, err
	}
	if err := copyFile(path, prev, 0o755); err != nil {
		return nil, err
	}
	restore := func() error {
		if err := copyFile(prev, path, 0o755); err != nil {
			return err
		}
		return removeIfExists(prev)
	}
	return restore, removeIfExists(path)
}

// isOurStaleBinary is remove_stale's ownership rule: the file carries our module
// path. It is never executed — a stranger's abctl may do anything when run.
func isOurStaleBinary(path, old string) bool {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	b, err := os.ReadFile(path) //nolint:gosec // a file in our own bin dir
	if err != nil {
		return false
	}
	re := regexp.MustCompile(`github\.com/rossoctl/cortex/(authbridge/)?cmd/` + regexp.QuoteMeta(old))
	return re.Match(b)
}

// unitStillNames is remove_stale's unit guard: either unit file, whatever the OS,
// containing /<old>.
func unitStillNames(env *setupEnv, old string) bool {
	for _, unit := range []string{
		filepath.Join(env.home, "Library", "LaunchAgents", launchdLabel+".plist"),
		filepath.Join(env.home, ".config", "systemd", "user", systemdUnit),
	} {
		if b, err := os.ReadFile(unit); err == nil && bytes.Contains(b, []byte("/"+old)) { //nolint:gosec
			return true
		}
	}
	return false
}
