package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "demo.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const minimalShell = `
total: 10s
grid: {cols: 20, rows: 4}
acts:
  - name: a
    kind: shell
    runtime: 10s
    steps:
      - cmd: "ls"
        out: ["one"]
`

func TestLoad_Minimal(t *testing.T) {
	s, err := Load(write(t, minimalShell))
	if err != nil {
		t.Fatal(err)
	}
	if s.Total != 10*time.Second {
		t.Errorf("total = %s", s.Total)
	}
	if s.Acts[0].Runtime != 10*time.Second {
		t.Errorf("runtime = %s", s.Acts[0].Runtime)
	}
}

// TestLoad_RuntimesMustSumToTotal matters because the emitter lays every state on
// one absolute timeline of exactly Total: a mismatch would silently shift every
// keyframe rather than fail.
func TestLoad_RuntimesMustSumToTotal(t *testing.T) {
	_, err := Load(write(t, strings.Replace(minimalShell, "total: 10s", "total: 12s", 1)))
	if err == nil || !strings.Contains(err.Error(), "sum") {
		t.Fatalf("want a sum error, got %v", err)
	}
}

func TestLoad_RejectsUnknownKind(t *testing.T) {
	_, err := Load(write(t, strings.Replace(minimalShell, "kind: shell", "kind: bogus", 1)))
	if err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Fatalf("want an unknown-kind error, got %v", err)
	}
}

func TestLoad_RejectsUnparsableDuration(t *testing.T) {
	_, err := Load(write(t, strings.Replace(minimalShell, "runtime: 10s", "runtime: soon", 1)))
	if err == nil || !strings.Contains(err.Error(), "runtime") {
		t.Fatalf("want a runtime parse error, got %v", err)
	}
}

func TestLoad_ShellActNeedsSteps(t *testing.T) {
	_, err := Load(write(t, `
total: 5s
grid: {cols: 10, rows: 2}
acts: [{name: a, kind: shell, runtime: 5s}]
`))
	if err == nil || !strings.Contains(err.Error(), "needs steps") {
		t.Fatalf("want a missing-steps error, got %v", err)
	}
}

func TestLoad_TUIActNeedsFixtureSessions(t *testing.T) {
	_, err := Load(write(t, `
total: 5s
grid: {cols: 10, rows: 2}
acts:
  - name: a
    kind: tui
    runtime: 5s
    beats: [{label: x, keys: [], hold: 5s}]
`))
	if err == nil || !strings.Contains(err.Error(), "fixture sessions") {
		t.Fatalf("want a missing-fixture error, got %v", err)
	}
}

// TestShellStates_SlowLongerThanOutIsRejected: extra delays would silently do
// nothing, which is the kind of script bug that is invisible in the output.
func TestShellStates_SlowLongerThanOutIsRejected(t *testing.T) {
	act := Act{Name: "a", Kind: "shell", Runtime: 10 * time.Second, Steps: []Step{
		{Cmd: "ls", Out: []string{"one"}, Slow: []time.Duration{time.Second, time.Second}},
	}}
	if _, err := ShellStates(act, 0, Grid{Cols: 20, Rows: 4}); err == nil {
		t.Error("want an error when slow has more entries than out")
	}
}

func TestShellStates_TypingCarriesATypeReveal(t *testing.T) {
	act := Act{Name: "a", Kind: "shell", Runtime: 10 * time.Second, Steps: []Step{
		{Cmd: "abctl", Out: []string{"hi"}},
	}}
	states, err := ShellStates(act, 0, Grid{Cols: 20, Rows: 4})
	if err != nil {
		t.Fatal(err)
	}
	if got := states[0].Rows[0].Reveal.Kind; got != "type" {
		t.Errorf("command row reveal = %q, want type", got)
	}
	if want := 5 * typeRate; states[0].Rows[0].Reveal.Dur != want {
		t.Errorf("typing duration = %s, want %s", states[0].Rows[0].Reveal.Dur, want)
	}
	if got := states[0].Rows[1].Reveal.Kind; got != "fill" {
		t.Errorf("output row reveal = %q, want fill", got)
	}
}

// TestShellStates_SlowDelaysOnlyItsOwnLine is the AX trick this design borrows:
// one long pause makes an authored sequence read as work being done.
func TestShellStates_SlowDelaysOnlyItsOwnLine(t *testing.T) {
	act := Act{Name: "a", Kind: "shell", Runtime: 20 * time.Second, Steps: []Step{
		{Cmd: "go", Out: []string{"first", "second", "third"},
			Slow: []time.Duration{100 * time.Millisecond, 2500 * time.Millisecond}},
	}}
	states, err := ShellStates(act, 0, Grid{Cols: 20, Rows: 8})
	if err != nil {
		t.Fatal(err)
	}
	rows := states[0].Rows
	gapToSecond := rows[2].Reveal.At - rows[1].Reveal.At
	gapToThird := rows[3].Reveal.At - rows[2].Reveal.At
	if gapToSecond != 2500*time.Millisecond {
		t.Errorf("second line delayed by %s, want 2.5s", gapToSecond)
	}
	if gapToThird != outputRate {
		t.Errorf("third line delayed by %s, want the default %s", gapToThird, outputRate)
	}
}

func TestShellStates_RejectsContentLongerThanTheAct(t *testing.T) {
	act := Act{Name: "a", Kind: "shell", Runtime: time.Second, Steps: []Step{
		{Cmd: "a-very-long-command-that-cannot-be-typed-in-one-second", Out: []string{"x"}},
	}}
	if _, err := ShellStates(act, 0, Grid{Cols: 80, Rows: 8}); err == nil {
		t.Error("want an error when the content does not fit the act's runtime")
	}
}

func TestShellStates_RejectsMoreRowsThanTheGrid(t *testing.T) {
	act := Act{Name: "a", Kind: "shell", Runtime: 30 * time.Second, Steps: []Step{
		{Cmd: "x", Out: []string{"1", "2", "3", "4", "5"}},
	}}
	if _, err := ShellStates(act, 0, Grid{Cols: 20, Rows: 3}); err == nil {
		t.Error("want an error when the script overflows the grid")
	}
}

func TestWindowStates_WindowsDoNotOverlap(t *testing.T) {
	act := Act{Name: "w", Kind: "windows", Runtime: 6 * time.Second, Windows: []WindowSpec{
		{Title: "a", Lines: []string{"$ claude", "working"}},
		{Title: "b", Lines: []string{"$ claude", "working"}},
	}}
	states, err := WindowStates(act, 0, Grid{Cols: 40, Rows: 20})
	if err != nil {
		t.Fatal(err)
	}
	w := states[0].Windows
	if len(w) != 2 {
		t.Fatalf("want 2 windows, got %d", len(w))
	}
	if w[0].TopRow+w[0].Height > w[1].TopRow {
		t.Errorf("windows overlap: %+v then %+v", w[0], w[1])
	}
}
