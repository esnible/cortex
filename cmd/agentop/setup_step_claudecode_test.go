package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func claudeEnv(t *testing.T, settings string) (*setupEnv, string) {
	t.Helper()
	env := configEnv(t)
	path := filepath.Join(env.home, settingsRel)
	if settings != "" {
		writeExe(t, path, settings)
	}
	return env, path
}

func TestClaudeCodeStepFreshPreflight(t *testing.T) {
	env, _ := claudeEnv(t, `{"env":{"HTTPS_PROXY":"http://corp:3128"}}`)
	env.configFresh = true
	_, prob := claudeCodeStep{}.plan(env)
	if prob == nil || !strings.Contains(prob.reason, `HTTPS_PROXY is already set to "http://corp:3128"`) {
		t.Errorf("problem = %+v", prob)
	}
	env2, _ := claudeEnv(t, `{"env":{"HTTPS_PROXY":"http://127.0.0.1:47699"}}`)
	env2.configFresh = true
	if p, prob := (claudeCodeStep{}).plan(env2); prob != nil || p.verb != "route" {
		t.Errorf("a stale Cortex value refused on a fresh install: %+v %v", p, prob)
	}
}

func TestClaudeCodeStepAppliesAgainstTheConfigNowOnDiskAndUndoes(t *testing.T) {
	env, path := claudeEnv(t, `{"model":"opus"}`)
	env.configFresh = true
	if _, prob := (claudeCodeStep{}).plan(env); prob != nil {
		t.Fatal(prob)
	}
	if _, _, err := (configStep{}).apply(env, nil); err != nil { // what the config step does first
		t.Fatal(err)
	}
	_, u, err := claudeCodeStep{}.apply(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	if readEnv(t, path)[envProxy] != "http://127.0.0.1:47600" {
		t.Error("Claude Code was not routed")
	}
	if err := u.fn(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != `{"model":"opus"}` {
		t.Errorf("undo left %q", b)
	}
	for _, f := range []string{path + ".bak", filepath.Join(env.home, stateRel)} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("undo left %s", f)
		}
	}
}

func TestClaudeCodeStepDoneWhenRouted(t *testing.T) {
	env, _ := claudeEnv(t, "")
	env.configFresh = true
	if _, _, err := (configStep{}).apply(env, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (claudeCodeStep{}).apply(env, nil); err != nil {
		t.Fatal(err)
	}
	env.configFresh = false // a later run: the config is on disk now
	if p, prob := (claudeCodeStep{}).plan(env); prob != nil || !p.done {
		t.Errorf("a routed Claude Code planned a change: %+v %v", p, prob)
	}
}

func TestClaudeCodeStepUndoKeepsASymlinkedSettingsFile(t *testing.T) {
	env, path := claudeEnv(t, "")
	real := filepath.Join(env.home, "dotfiles", "claude.json")
	writeExe(t, real, `{}`)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, path); err != nil {
		t.Fatal(err)
	}
	env.configFresh = true
	if _, _, err := (configStep{}).apply(env, nil); err != nil {
		t.Fatal(err)
	}
	_, u, err := claudeCodeStep{}.apply(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.fn(); err != nil {
		t.Fatal(err)
	}
	if l, err := os.Readlink(path); err != nil || l != real {
		t.Errorf("settings.json is no longer the link: %q %v", l, err)
	}
}

// A fresh install's plan cannot tell that settings.json already holds the values
// the built-in config yields, as after a reinstall with ~/.cortex removed. Apply's
// re-plan finds nothing to change, so the file keeps its bytes and gets no .bak.
func TestClaudeCodeStepApplyLeavesAnAlreadyRoutedFileAlone(t *testing.T) {
	env, path := claudeEnv(t, "")
	env.configFresh = true
	if _, _, err := (configStep{}).apply(env, nil); err != nil {
		t.Fatal(err)
	}
	want, err := wanted(env.configPath())
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(map[string]any{"env": want})
	if err != nil {
		t.Fatal(err)
	}
	writeExe(t, path, string(b))
	detail, _, err := claudeCodeStep{}.apply(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(b) || detail != "Claude Code already routed" {
		t.Errorf("apply with nothing to change rewrote the file: %q (detail %q)", got, detail)
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Error("apply with nothing to change wrote a .bak")
	}
}

// With a config on disk the plan is planClaudeCodeEnable's, and apply plans again
// rather than writing that plan's copy of the file, so an edit made to
// settings.json after the plan, as Claude Code makes to it itself, survives.
func TestClaudeCodeStepApplyKeepsAnEditMadeAfterThePlan(t *testing.T) {
	env, path := claudeEnv(t, `{"model":"opus"}`)
	env.configFresh = true
	if _, _, err := (configStep{}).apply(env, nil); err != nil {
		t.Fatal(err)
	}
	env.configFresh = false // a later run: the config is on disk now
	if p, prob := (claudeCodeStep{}).plan(env); prob != nil || p.verb != "route" {
		t.Fatalf("plan = %+v %v", p, prob)
	}
	writeExe(t, path, `{"model":"opus","theme":"dark"}`)
	if _, _, err := (claudeCodeStep{}).apply(env, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil || doc["theme"] != "dark" {
		t.Errorf("apply lost the edit made after the plan: %s", b)
	}
}
