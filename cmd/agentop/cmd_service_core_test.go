package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runServiceInstall's result is what lets `agentop setup` say "started", "already
// running" or "failed" without parsing text, so each outcome must be distinguishable.
func TestRunServiceInstall_ReportsWhatHappened(t *testing.T) {
	fakeSupervisor(t)
	sc := newServiceScene(t)
	var out, errb bytes.Buffer

	first := runServiceInstall(sc.p, 0, false, false, &out, &errb)
	if first != (installResult{exit: 0, healthy: true, replaced: true}) {
		t.Errorf("fresh install = %+v, want started and healthy\n%s%s", first, out.String(), errb.String())
	}
	second := runServiceInstall(sc.p, 0, false, false, &out, &errb)
	if second != (installResult{exit: 0, alreadyCurrent: true}) {
		t.Errorf("re-run = %+v, want already current", second)
	}
	forced := runServiceInstall(sc.p, 0, true, false, &out, &errb)
	if forced.alreadyCurrent || !forced.healthy {
		t.Errorf("--restart = %+v, want a real restart that comes back healthy", forced)
	}
}

// With no health URL there is nothing to probe, so the install succeeds without
// claiming health: the result `agentop setup` reports as "started", not "started and
// healthy". Without parsing stdout, only the result tells the two apart; `.exit`
// is 0 for both.
func TestRunServiceInstall_UnprobedInstallIsNotHealthy(t *testing.T) {
	fakeSupervisor(t)
	sc := newServiceScene(t)
	// The scene's config predates the loopback pins, and a migration that changes the
	// config re-reads the health URL from it, which would restore the probe. Bring the
	// config up to date first so that the empty health URL survives into the install.
	if _, err := migrateConfig(sc.p.configFile, io.Discard); err != nil {
		t.Fatalf("bringing the scene's config up to date: %v", err)
	}
	if changed, err := migrateConfig(sc.p.configFile, io.Discard); err != nil || changed {
		t.Fatalf("config still needs migrating (changed=%v, err=%v), so the install would re-read the health URL", changed, err)
	}
	sc.p.healthURL = ""
	var out, errb bytes.Buffer

	if got := runServiceInstall(sc.p, 0, false, false, &out, &errb); got != (installResult{replaced: true}) {
		t.Errorf("install with no health URL = %+v, want installResult{replaced: true} (exit 0, not healthy, not already current)\n%s%s",
			got, out.String(), errb.String())
	}
	if _, err := os.Stat(sc.p.unitFile); err != nil {
		t.Errorf("unit not written: %v", err)
	}
}

func TestRunServiceInstall_FailureCarriesTheCommandsExitCode(t *testing.T) {
	fakeNoSupervisor(t)
	sc := newServiceScene(t)
	var out, errb bytes.Buffer
	if got := runServiceInstall(sc.p, 0, false, false, &out, &errb); got.exit != exitNoSupervisor {
		t.Errorf("exit = %d, want %d", got.exit, exitNoSupervisor)
	}
}

func TestRemoveService_ReportsWhetherTheUnitIsGone(t *testing.T) {
	fakeSupervisor(t)
	sc := newServiceScene(t)
	var out, errb bytes.Buffer
	if r := runServiceInstall(sc.p, 0, false, false, &out, &errb); r.exit != 0 {
		t.Fatalf("install: %+v %s", r, errb.String())
	}
	if _, err := os.Stat(sc.p.stampFile); err != nil {
		t.Fatalf("install wrote no launch stamp, so its removal cannot be checked: %v", err)
	}
	if !removeService(sc.p, &errb) {
		t.Fatalf("removeService reported failure: %s", errb.String())
	}
	if _, err := os.Stat(sc.p.unitFile); err == nil {
		t.Error("unit still on disk")
	}
	if _, err := os.Stat(sc.p.stampFile); !os.IsNotExist(err) {
		t.Errorf("launch stamp still on disk (stat: %v)", err)
	}
	errb.Reset()
	if removeService(sc.p, &errb) {
		t.Error("removing an absent unit reported success")
	}
	if !strings.Contains(errb.String(), "removing "+sc.p.unitFile) {
		t.Errorf("the failure was not reported: %q", errb.String())
	}
}

// A stamp that will not go is reported but does not fail the removal. The unit is what
// makes the service look installed, and the unit is gone.
func TestRemoveService_AStampThatWillNotGoIsReportedNotFatal(t *testing.T) {
	fakeSupervisor(t)
	sc := newServiceScene(t)
	var out, errb bytes.Buffer
	if r := runServiceInstall(sc.p, 0, false, false, &out, &errb); r.exit != 0 {
		t.Fatalf("install: %+v %s", r, errb.String())
	}
	// A non-empty directory where the stamp was: os.Remove fails on it with an error
	// other than "does not exist", which is the branch under test.
	if err := os.Remove(sc.p.stampFile); err != nil {
		t.Fatalf("clearing the stamp: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(sc.p.stampFile, "keep"), 0o755); err != nil {
		t.Fatalf("putting a directory in the stamp's place: %v", err)
	}

	errb.Reset()
	if !removeService(sc.p, &errb) {
		t.Fatalf("removeService reported failure over a stamp it could not remove: %s", errb.String())
	}
	if _, err := os.Stat(sc.p.unitFile); !os.IsNotExist(err) {
		t.Errorf("unit still on disk (stat: %v)", err)
	}
	if want := "could not remove " + sc.p.stampFile; !strings.Contains(errb.String(), want) {
		t.Errorf("stderr = %q, want it to contain %q", errb.String(), want)
	}
}
