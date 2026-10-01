package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// planClaudeCodeEnable is what `agentop setup` calls before it asks anything, so the
// property that matters is that it touches nothing — not the settings, not a backup,
// not the state record.
func TestPlanClaudeCodeEnable_TouchesNothing(t *testing.T) {
	settings, cfg := fixture(t, settingsWithSecret)
	before, _ := os.ReadFile(settings)
	pl, err := planClaudeCodeEnable(settings, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.changes) != len(managedKeys) {
		t.Errorf("changes = %d, want %d (none of the keys are set yet)", len(pl.changes), len(managedKeys))
	}
	if after, _ := os.ReadFile(settings); !bytes.Equal(before, after) {
		t.Error("planning changed the settings file")
	}
	if _, err := os.Stat(settings + ".bak"); err == nil {
		t.Error("planning wrote a backup")
	}
}

func TestPlanClaudeCodeEnable_NoChangesOnceEnabled(t *testing.T) {
	settings, cfg := fixture(t, settingsWithSecret)
	var out, errb bytes.Buffer
	if code := claudeCodeEnable2(settings, cfg, "", true, &out, &errb); code != 0 {
		t.Fatalf("enable failed: %s", errb.String())
	}
	pl, err := planClaudeCodeEnable(settings, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.changes) != 0 {
		t.Errorf("changes after enable = %q, want none", pl.changes)
	}
}

func TestPlanClaudeCodeEnable_RefusesAForeignValue(t *testing.T) {
	settings, cfg := fixture(t, `{"env":{"HTTPS_PROXY":"http://corp:3128"}}`)
	_, err := planClaudeCodeEnable(settings, cfg)
	if err == nil || !strings.Contains(err.Error(), "Refusing to overwrite") {
		t.Errorf("err = %v, want the overwrite refusal", err)
	}
}

func TestApplyClaudeCodeEnable_WritesThePlanAndRecordsPrior(t *testing.T) {
	settings, cfg := fixture(t, `{"env":{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC":"1"}}`)
	state := filepath.Join(t.TempDir(), "state.json")
	pl, err := planClaudeCodeEnable(settings, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var errb bytes.Buffer
	if err := applyClaudeCodeEnable(pl, state, &errb); err != nil {
		t.Fatalf("apply: %v (%s)", err, errb.String())
	}
	env := readEnv(t, settings)
	for k, v := range pl.want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	st, err := readState(state)
	if err != nil {
		t.Fatal(err)
	}
	if p := st.Prior[envNoTelem]; p == nil || *p != "1" {
		t.Errorf("prior %s not recorded as the user's \"1\": %v", envNoTelem, p)
	}
	if p, ok := st.Prior[envProxy]; !ok || p != nil {
		t.Errorf("prior %s should be recorded as absent", envProxy)
	}
}
