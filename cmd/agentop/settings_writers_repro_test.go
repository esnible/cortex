package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteState_TornWriteBlocksEveryLaterEnable reproduces #1565's deadliness
// as a sequence, on a record built the way writeState builds it:
//
//  1. enable writes the record with os.WriteFile — truncate, then write.
//  2. agentop is killed mid-write: the file now holds a prefix of the JSON.
//  3. readState fails on it, and writeState REFUSES to overwrite — so every
//     later enable only warns, forever, and the user's Prior settings are
//     stuck in a file nothing can read.
//
// The fix is writeFileAtomic (CreateTemp + fsync + rename): a kill mid-write
// leaves the PREVIOUS record intact, so step 2 cannot produce this state.
// This test pins the loop so the fix's test can pin its absence.
func TestWriteState_TornWriteBlocksEveryLaterEnable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-code-state.json")

	// Step 1: a successful enable.
	prior := "https://before.example"
	st := managedState{
		Settings: "/settings.json",
		Prior:    map[string]*string{"ANTHROPIC_BASE_URL": &prior},
		Written:  map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:47600"},
	}
	if err := writeState(path, st); err != nil {
		t.Fatalf("first writeState: %v", err)
	}

	// Step 2: the kill mid-write. Simulated by truncating the file to a
	// prefix — exactly what os.WriteFile leaves between O_TRUNC and the
	// write completing.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b[:len(b)/2], 0o600); err != nil {
		t.Fatal(err)
	}

	// Step 3: every later enable refuses, and the Prior is unrecoverable.
	if _, err := readState(path); err == nil {
		t.Fatal("readState accepted a torn record")
	}
	err = writeState(path, st)
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("writeState after a torn write: %v, want the refusal that blocks every later enable", err)
	}
}

// TestWriteSettings_SymlinkReplaced reproduces #1565's silent dotfiles break:
// writeSettings writes path+".tmp" and renames it over path, and rename over a
// symlink replaces the LINK with a regular file — so a ~/.claude/settings.json
// managed as a symlink into a dotfiles repo stops tracking, and the repo's copy
// silently goes stale while agentop believes it configured the user's settings.
func TestWriteSettings_SymlinkReplaced(t *testing.T) {
	dir := t.TempDir()
	dotfiles := filepath.Join(dir, "dotfiles")
	if err := os.MkdirAll(dotfiles, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dotfiles, "settings.json")
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if err := writeSettings(link, map[string]any{"env": map[string]any{"ANTHROPIC_BASE_URL": "http://127.0.0.1:47600"}}); err != nil {
		t.Fatalf("writeSettings over a symlink: %v", err)
	}

	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("writeSettings replaced the symlink with a regular file — the dotfiles repo's copy silently goes stale")
	}
	// And the target must carry the write, or the link is hollow.
	tb, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(tb), "47600") {
		t.Error("the write did not reach the symlink's target")
	}
}

// TestWriteFileAtomic_PlantedTmpSymlink: a fixed "path+.tmp" name was an
// open redirect — os.WriteFile follows a symlink planted at that name, so
// anything able to create files in the directory could steer the write
// (userconfig.go guards its own copy against exactly this). CreateTemp names
// the temp file uniquely, so the plant sits untouched and the write lands
// where it was aimed.
func TestWriteFileAtomic_PlantedTmpSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(real, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(dir, "stolen")
	if err := os.WriteFile(victim, []byte("untouched\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plant := real + ".tmp"
	if err := os.Symlink(victim, plant); err != nil {
		t.Fatal(err)
	}

	if err := writeFileAtomic(real, []byte("{\"routed\":true}\n"), 0o600); err != nil {
		t.Fatalf("writeFileAtomic past a planted tmp symlink: %v", err)
	}

	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "untouched\n" {
		t.Errorf("the planted tmp symlink redirected the write: victim now %q", got)
	}
	// The plant itself is not our problem to clean, but it must not have been
	// followed. Its target is intact above; the real file carries the write.
	rgot, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rgot), "routed") {
		t.Errorf("the write did not land at the real path: %q", rgot)
	}
	_ = plant
}
