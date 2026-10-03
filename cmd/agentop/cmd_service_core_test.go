package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
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

// removeService prints what removeServiceReport returns, exactly as it printed before
// the two were split. The calls under test go through failingUnload, so each one
// prints the unload line as well: once beside a stamp that will not go, which still
// returns true, and once with the unit already gone, which returns false.
func TestRemoveServiceStillReportsToStderr(t *testing.T) {
	loaded := fakeSupervisor(t)
	sc := newServiceScene(t)
	var out, errb bytes.Buffer
	if r := runServiceInstall(sc.p, 0, false, false, &out, &errb); r.exit != 0 {
		t.Fatalf("install: %+v %s", r, errb.String())
	}
	failingUnload(t, loaded)
	// The refused unload leaves the job loaded, so asking again gets the same error.
	unloadErr := unloadService(runtime.GOOS, sc.p)
	if unloadErr == nil {
		t.Fatal("fixture: the unload did not fail")
	}
	if err := os.Remove(sc.p.stampFile); err != nil {
		t.Fatalf("clearing the stamp: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(sc.p.stampFile, "keep"), 0o755); err != nil {
		t.Fatalf("putting a directory in the stamp's place: %v", err)
	}
	stampErr := os.Remove(sc.p.stampFile)
	if stampErr == nil {
		t.Fatal("fixture: the stamp's directory could be removed")
	}

	errb.Reset()
	if !removeService(sc.p, &errb) {
		t.Error("removeService reported failure with the unit gone")
	}
	if want := fmt.Sprintf("agentop: %v\nagentop: could not remove %s: %v\n", unloadErr, sc.p.stampFile, stampErr); errb.String() != want {
		t.Errorf("stderr =\n%q\nwant\n%q", errb.String(), want)
	}

	unitErr := os.Remove(sc.p.unitFile)
	if unitErr == nil {
		t.Fatal("fixture: the unit was still there to remove")
	}
	errb.Reset()
	if removeService(sc.p, &errb) {
		t.Error("removeService reported success with no unit to remove")
	}
	if want := fmt.Sprintf("agentop: %v\nagentop: removing %s: %v\n", unloadErr, sc.p.unitFile, unitErr); errb.String() != want {
		t.Errorf("stderr =\n%q\nwant\n%q", errb.String(), want)
	}
}

// uninstall says a job is still loaded when the supervisor refused to unload it, so
// removeServiceReport returns that failure apart from the unit's. The unit still
// goes, and a stamp that is already gone is not a failure either.
func TestRemoveServiceReportReturnsTheUnloadFailure(t *testing.T) {
	loaded := fakeSupervisor(t)
	sc := newServiceScene(t)
	var out, errb bytes.Buffer
	if r := runServiceInstall(sc.p, 0, false, false, &out, &errb); r.exit != 0 {
		t.Fatalf("install: %+v %s", r, errb.String())
	}
	failingUnload(t, loaded)
	if err := os.Remove(sc.p.stampFile); err != nil {
		t.Fatalf("clearing the stamp: %v", err)
	}

	unloadErr, unitErr, stampErr := removeServiceReport(sc.p)
	if unloadErr == nil || !strings.Contains(unloadErr.Error(), "unload refused") {
		t.Errorf("unloadErr = %v, want the supervisor's refusal", unloadErr)
	}
	if unitErr != nil {
		t.Errorf("unitErr = %v, want nil: the unit was there to remove", unitErr)
	}
	if stampErr != nil {
		t.Errorf("stampErr = %v, want nil: a stamp that does not exist is not a failure", stampErr)
	}
	if _, err := os.Stat(sc.p.unitFile); !os.IsNotExist(err) {
		t.Errorf("unit still on disk (stat: %v)", err)
	}
	if _, err := os.Stat(loaded); err != nil {
		t.Error("fixture: the fake job is not loaded, so the unload did not fail")
	}
}
