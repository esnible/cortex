package main

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rossoctl/cortex/cmd/agentop/checklist"
)

// claudeCodeStep routes Claude Code through Cortex by writing the managed keys
// into ~/.claude/settings.json, as `agentop configure claude-code enable` does.
type claudeCodeStep struct{}

func (claudeCodeStep) name() string { return "routed" }

func (claudeCodeStep) paths(env *setupEnv) (settings, state string) {
	return filepath.Join(env.home, settingsRel), filepath.Join(env.home, stateRel)
}

func (s claudeCodeStep) plan(env *setupEnv) (stepPlan, *problem) {
	p := stepPlan{label: "routed"}
	settings, _ := s.paths(env)
	if env.configFresh {
		// No config yet, so no planClaudeCodeEnable: the built-in config the config
		// step will write always yields Cortex-shaped values, which makes this the
		// same refusal planClaudeCodeEnable would give.
		doc, err := readSettings(settings)
		if err != nil {
			return p, &problem{reason: err.Error()}
		}
		vals := envStrings(doc)
		for _, k := range managedKeys {
			if cur, ok := vals[k]; ok && !isCortexValue(k, cur) {
				return p, &problem{
					reason: fmt.Sprintf("%s is already set to %q in %s", k, cur, env.tilde(settings)),
					fix:    []string{"remove it, or edit the file by hand, then re-run"},
				}
			}
		}
	} else {
		pl, err := planClaudeCodeEnable(settings, env.configPath())
		if err != nil {
			lines := strings.Split(err.Error(), "\n")
			return p, &problem{reason: lines[0], fix: trimAll(lines[1:])}
		}
		if len(pl.changes) == 0 {
			p.done, p.doneMsg = true, "Claude Code already routed"
			return p, nil
		}
	}
	p.verb, p.what, p.where = "route", "Claude Code via Cortex", env.tilde(settings)+" (.bak kept)"
	return p, nil
}

// apply snapshots settings.json, its .bak and the state file, so the undo puts
// back those bytes rather than running disable, which re-marshals the file.
func (s claudeCodeStep) apply(env *setupEnv, _ *checklist.Running) (string, undo, error) {
	settings, state := s.paths(env)
	var snaps []fileSnapshot
	for _, f := range []string{settings, settings + ".bak", state} {
		snap, err := snapshotFile(f)
		if err != nil {
			return "", undo{}, err
		}
		snaps = append(snaps, snap)
	}
	u := undo{label: env.tilde(settings), fn: func() error {
		var errs []error
		for _, snap := range snaps {
			errs = append(errs, snap.restore())
		}
		return errors.Join(errs...)
	}, manual: "agentop configure claude-code disable"}
	// Planned again now: the plan before consent may predate the config, and the
	// file may have changed while the steps before this one ran.
	pl, err := planClaudeCodeEnable(settings, env.configPath())
	if err != nil {
		lines := strings.Split(err.Error(), "\n")
		return "", u, stepError{reason: lines[0], detail: trimAll(lines[1:])}
	}
	if len(pl.changes) == 0 {
		return "Claude Code already routed", u, nil
	}
	var errb bytes.Buffer
	if err := applyClaudeCodeEnable(pl, state, &errb); err != nil {
		return "", u, err
	}
	return "Claude Code → Cortex", u, nil
}

// trimAll is lines with each trimmed and the empty ones dropped.
func trimAll(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
