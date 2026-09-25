package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProxyStampRoundTrip is the basic contract: what we record is what we read back.
func TestProxyStampRoundTrip(t *testing.T) {
	p := servicePathsFixture(t)
	want := binarySHA256(p.binary)
	if want == "" {
		t.Fatal("the fixture binary did not hash; the rest of this proves nothing")
	}
	writeProxyStamp(p)
	if got := readProxyStamp(p); got != want {
		t.Errorf("readProxyStamp = %q, want %q", got, want)
	}
	if !proxyBinaryUnchanged(p) {
		t.Error("a freshly recorded stamp does not match its own binary")
	}
}

// TestProxyStamp_LeavesTheUnitAlone is the fix for the second review finding.
//
// The hash used to live in the unit, so recording it rewrote the unit — and systemd
// flags a unit whose mtime moved as "changed on disk, run daemon-reload", which is
// exactly the message `abctl service` exists so nobody has to see. Launch state now
// lives beside proxy.pid instead, so the unit is untouched on every platform and no
// daemon-reload is owed.
func TestProxyStamp_LeavesTheUnitAlone(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			p := servicePathsFixture(t)
			unit := renderUnitFor(goos, p)
			if strings.Contains(unit, binarySHA256(p.binary)) {
				t.Error("the unit still embeds the proxy hash; recording it would churn the unit's mtime")
			}
			if err := os.WriteFile(p.unitFile, []byte(unit), 0o644); err != nil { //nolint:gosec // matches the install path's mode
				t.Fatal(err)
			}
			before, err := os.Stat(p.unitFile)
			if err != nil {
				t.Fatal(err)
			}

			writeProxyStamp(p)

			after, err := os.Stat(p.unitFile)
			if err != nil {
				t.Fatal(err)
			}
			if !after.ModTime().Equal(before.ModTime()) {
				t.Error("recording the stamp changed the unit's mtime; systemd would ask for a daemon-reload")
			}
			body, err := os.ReadFile(p.unitFile)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != unit {
				t.Error("the unit's content changed")
			}
			// The version stamp is unit content and stays in the unit.
			if got := unitWriterVersion(p.unitFile); got != version {
				t.Errorf("unitWriterVersion = %q, want %q", got, version)
			}
		})
	}
}

// TestProxyBinaryUnchanged_NoticesAReplacedBinary is the regression the stamp exists
// for. Path, config, version and supervisor state are all identical across a rebuild,
// so before the stamp every clause of serviceIsCurrent passed and install reported
// "Already current" while the old build kept serving.
func TestProxyBinaryUnchanged_NoticesAReplacedBinary(t *testing.T) {
	p := servicePathsFixture(t)
	writeProxyStamp(p)
	if !proxyBinaryUnchanged(p) {
		t.Fatal("baseline does not match")
	}
	// Rebuild: same path, different bytes.
	if err := os.WriteFile(p.binary, []byte("#!/bin/sh\nsleep 31\n"), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	if proxyBinaryUnchanged(p) {
		t.Error("a replaced binary still reads as unchanged; install would skip the restart")
	}
	// ...and recording it again — which is what start/restart does — makes an
	// installer re-run free once more. Without that, the manual `cp` + `restart` flow
	// left every later install restarting a service already serving the right code.
	writeProxyStamp(p)
	if !proxyBinaryUnchanged(p) {
		t.Error("re-recording after a launch did not restore the match")
	}
}

// TestProxyBinaryUnchanged_NoStampIsChanged pins the direction of the default. Reading
// an absent stamp as "unchanged" would make the first dev install after this upgrade
// silently skip its restart — the one run where the binary has certainly moved.
func TestProxyBinaryUnchanged_NoStampIsChanged(t *testing.T) {
	p := servicePathsFixture(t)
	if proxyBinaryUnchanged(p) {
		t.Error("a missing stamp file read as unchanged")
	}
}

// TestProxyBinaryUnchanged_MissingBinaryIsChanged guards the "" collision: a binary
// that cannot be read hashes to "", and so does an absent stamp. Comparing them as
// equal would call a vanished binary current.
func TestProxyBinaryUnchanged_MissingBinaryIsChanged(t *testing.T) {
	p := servicePathsFixture(t)
	writeProxyStamp(p)
	if err := os.Remove(p.binary); err != nil {
		t.Fatal(err)
	}
	if proxyBinaryUnchanged(p) {
		t.Error("a missing binary read as unchanged")
	}
}

// TestWriteProxyStamp_MissingBinaryKeepsTheOldStamp: hashing "" must never be written,
// or a vanished binary would leave a stamp that matches nothing readable.
func TestWriteProxyStamp_MissingBinaryKeepsTheOldStamp(t *testing.T) {
	p := servicePathsFixture(t)
	writeProxyStamp(p)
	want := readProxyStamp(p)
	if err := os.Remove(p.binary); err != nil {
		t.Fatal(err)
	}
	writeProxyStamp(p)
	if got := readProxyStamp(p); got != want {
		t.Errorf("stamp overwritten for an unhashable binary: %q, want %q", got, want)
	}
}

// TestResolveServicePaths_StampSitsBesideThePid keeps launch state together in
// ~/.cortex rather than drifting to a second location.
func TestResolveServicePaths_StampSitsBesideThePid(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("mode: proxy-sidecar\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := resolveServicePaths(cfg, filepath.Join(dir, "unit"), filepath.Join(dir, "proxy"))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "proxy.sha256"); p.stampFile != want {
		t.Errorf("stampFile = %q, want %q", p.stampFile, want)
	}
	if filepath.Dir(p.stampFile) != filepath.Dir(p.pidFile) {
		t.Errorf("stamp %q and pid %q are not in the same directory", p.stampFile, p.pidFile)
	}
}

// TestInstallCanSkip covers the guard --restart hangs off, including that the health
// probe stays lazy: evaluating it when the answer is already known would spend
// serviceIsCurrent's 2s budget on every run that had decided to restart anyway.
func TestInstallCanSkip(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		configChanged, restart  bool
		current                 bool
		wantSkip, wantProbeCall bool
	}{
		{name: "nothing changed", current: true, wantSkip: true, wantProbeCall: true},
		{name: "not current", current: false, wantSkip: false, wantProbeCall: true},
		{name: "--restart overrides current", restart: true, current: true},
		{name: "--restart with nothing current", restart: true},
		{name: "config changed", configChanged: true, current: true},
		{name: "config changed and --restart", configChanged: true, restart: true, current: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probed := false
			got := installCanSkip(tc.configChanged, tc.restart, func() bool {
				probed = true
				return tc.current
			})
			if got != tc.wantSkip {
				t.Errorf("installCanSkip = %v, want %v", got, tc.wantSkip)
			}
			if probed != tc.wantProbeCall {
				t.Errorf("health probe called = %v, want %v (a needless probe costs 2s)", probed, tc.wantProbeCall)
			}
		})
	}
}

// TestServiceUsageDocumentsRestart keeps the flag discoverable. A flag that only
// exists in a Makefile recipe is one nobody finds from `abctl service --help`.
func TestServiceUsageDocumentsRestart(t *testing.T) {
	if !strings.Contains(serviceUsage, "--restart") {
		t.Error("service usage does not mention --restart")
	}
}
