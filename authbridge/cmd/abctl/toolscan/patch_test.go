package toolscan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleConfig = `mode: proxy-sidecar
pipeline:
  outbound:
    plugins:
      # Parses the inference request so downstream plugins see a manifest.
      - name: inference-parser
      - name: tool-prune
        on_error: observe        # measure only; switch to enforce when trusted
        config:
          remove: []
      - name: token-exchange
        config:
          keycloak_url: http://keycloak:8080
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "demo.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestPatchConfig_TouchesOnlyTheRemoveLine: the operator's comments and
// hand-tuned entries must survive. This is why the patch is line-based rather
// than a YAML round-trip.
func TestPatchConfig_TouchesOnlyTheRemoveLine(t *testing.T) {
	p := writeConfig(t, sampleConfig)
	changed, err := PatchConfig(p, []string{"NotebookEdit", "WebSearch"})
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected the file to change")
	}
	out, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "          remove: [NotebookEdit, WebSearch]") {
		t.Errorf("remove line not patched (indentation must be preserved):\n%s", got)
	}
	for _, keep := range []string{
		"# Parses the inference request so downstream plugins see a manifest.",
		"on_error: observe        # measure only; switch to enforce when trusted",
		"keycloak_url: http://keycloak:8080",
		"mode: proxy-sidecar",
		"- name: token-exchange",
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("patch disturbed unrelated content, missing %q:\n%s", keep, got)
		}
	}
	// Exactly one line differs.
	var diffs int
	origLines := strings.Split(sampleConfig, "\n")
	newLines := strings.Split(got, "\n")
	if len(origLines) != len(newLines) {
		t.Fatalf("line count changed: %d -> %d", len(origLines), len(newLines))
	}
	for i := range origLines {
		if origLines[i] != newLines[i] {
			diffs++
		}
	}
	if diffs != 1 {
		t.Errorf("%d lines changed, want exactly 1", diffs)
	}
}

// TestPatchConfig_Idempotent: install-demo.sh may run the scan on every
// invocation, so re-writing the same candidates must not report a change or
// rewrite the file.
func TestPatchConfig_Idempotent(t *testing.T) {
	p := writeConfig(t, sampleConfig)
	if _, err := PatchConfig(p, []string{"NotebookEdit"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := PatchConfig(p, []string{"NotebookEdit"})
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("second identical patch reported a change; must be idempotent")
	}
	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("file rewritten despite no change")
	}
}

func TestPatchConfig_EmptyCandidatesWritesEmptyList(t *testing.T) {
	p := writeConfig(t, strings.Replace(sampleConfig, "remove: []", "remove: [NotebookEdit]", 1))
	changed, err := PatchConfig(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected a change back to an empty list")
	}
	out, _ := os.ReadFile(p)
	if !strings.Contains(string(out), "remove: []") {
		t.Errorf("want an empty list:\n%s", out)
	}
}

// TestPatchConfig_ErrorsWhenPluginAbsent: silently doing nothing would leave the
// operator believing the list was written.
func TestPatchConfig_ErrorsWhenPluginAbsent(t *testing.T) {
	p := writeConfig(t, "pipeline:\n  outbound:\n    plugins:\n      - name: token-exchange\n")
	_, err := PatchConfig(p, []string{"NotebookEdit"})
	if err == nil {
		t.Fatal("expected an error when the tool-prune entry is missing")
	}
	if !strings.Contains(err.Error(), "tool-prune") {
		t.Errorf("error should name the missing entry: %v", err)
	}
}

func TestPatchConfig_ErrorsWhenRemoveKeyAbsent(t *testing.T) {
	p := writeConfig(t, "pipeline:\n  outbound:\n    plugins:\n      - name: tool-prune\n        on_error: observe\n      - name: token-exchange\n")
	_, err := PatchConfig(p, []string{"NotebookEdit"})
	if err == nil {
		t.Fatal("expected an error when remove: is missing")
	}
	if !strings.Contains(err.Error(), "remove:") {
		t.Errorf("error should name the missing key: %v", err)
	}
}

// TestPatchConfig_DoesNotEscapeTheEntry: a remove: key belonging to a different
// plugin further down the file must not be hijacked.
func TestPatchConfig_DoesNotEscapeTheEntry(t *testing.T) {
	cfg := `pipeline:
  outbound:
    plugins:
      - name: tool-prune
        on_error: observe
        config:
          remove: []
      - name: other-plugin
        config:
          remove: [SomethingElse]
`
	p := writeConfig(t, cfg)
	if _, err := PatchConfig(p, []string{"NotebookEdit"}); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(p)
	got := string(out)
	if !strings.Contains(got, "remove: [SomethingElse]") {
		t.Errorf("another plugin's remove list was modified:\n%s", got)
	}
	if !strings.Contains(got, "remove: [NotebookEdit]") {
		t.Errorf("tool-prune's list was not patched:\n%s", got)
	}
}
