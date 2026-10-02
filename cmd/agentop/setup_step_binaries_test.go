package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestBinariesFreshInstallAndUndo(t *testing.T) {
	env := newTestSetupEnv(t)
	env.fromDir = stageDir(t, "#!/bin/sh\necho cortex v9.9.9\n")
	p, prob := binariesStep{}.plan(env)
	if prob != nil || p.done || p.verb != "install" || !env.freshInstall {
		t.Fatalf("plan = %+v %v fresh=%v", p, prob, env.freshInstall)
	}
	_, u, err := binariesStep{}.apply(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"agentop", "cortex"} {
		got, _ := os.ReadFile(filepath.Join(env.binDir, n))
		want, _ := os.ReadFile(filepath.Join(env.fromDir, n))
		if !bytes.Equal(got, want) || !isExecutable(filepath.Join(env.binDir, n)) {
			t.Errorf("%s not installed as staged", n)
		}
	}
	if !strings.Contains(u.manual, "~/.cortex/previous/ back into ~/.local/bin") {
		t.Errorf("undo manual = %q, want the previous/ copy-back", u.manual)
	}
	if err := u.fn(); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"agentop", "cortex"} {
		if _, err := os.Stat(filepath.Join(env.binDir, n)); err == nil {
			t.Errorf("undo left %s", n)
		}
	}
}

func TestBinariesUpgradeKeepsThePreviousUntilUndoneOrDone(t *testing.T) {
	env := newTestSetupEnv(t)
	writeExe(t, filepath.Join(env.binDir, "agentop"), "#!/bin/sh\necho agentop v1.0.0\n")
	writeExe(t, filepath.Join(env.binDir, "cortex"), "#!/bin/sh\necho cortex v1.0.0\n")
	env.fromDir = stageDir(t, "#!/bin/sh\necho cortex v9.9.9\n")
	p, prob := binariesStep{}.plan(env)
	if prob != nil || p.verb != "replace" || env.freshInstall || env.installedVersion != "v1.0.0" {
		t.Fatalf("plan = %+v %v installed=%q", p, prob, env.installedVersion)
	}
	_, u, err := binariesStep{}.apply(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(env.previousDir(), "cortex")); !bytes.Contains(b, []byte("v1.0.0")) {
		t.Error("the previous cortex was not kept")
	}
	if err := u.fn(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(env.binDir, "cortex")); !bytes.Contains(b, []byte("v1.0.0")) {
		t.Error("undo did not bring the previous cortex back")
	}
	if _, err := os.Stat(filepath.Join(env.previousDir(), "cortex")); err == nil {
		t.Error("undo left its copy in previous/")
	}

	// Done: once every step has applied, onSuccess drops previous/.
	env.onSuccess = nil
	if _, _, err := (binariesStep{}).apply(env, nil); err != nil {
		t.Fatal(err)
	}
	for _, f := range env.onSuccess {
		f()
	}
	if _, err := os.Stat(env.previousDir()); err == nil {
		t.Error("onSuccess left previous/")
	}
}

func TestBinariesUnchangedIsDone(t *testing.T) {
	env := newTestSetupEnv(t)
	env.fromDir = stageDir(t, "#!/bin/sh\necho cortex v9.9.9\n")
	for _, n := range []string{"agentop", "cortex"} {
		if err := copyFile(filepath.Join(env.fromDir, n), filepath.Join(env.binDir, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if p, prob := (binariesStep{}).plan(env); prob != nil || !p.done {
		t.Errorf("identical binaries planned a change: %+v %v", p, prob)
	}
}

func TestBinariesRepairModeNeedsAnInstall(t *testing.T) {
	env := newTestSetupEnv(t)
	if _, prob := (binariesStep{}).plan(env); prob == nil {
		t.Error("repair mode with nothing installed raised no problem")
	}
	writeExe(t, filepath.Join(env.binDir, "agentop"), "#!/bin/sh\necho agentop v1\n")
	writeExe(t, filepath.Join(env.binDir, "cortex"), "#!/bin/sh\n")
	if p, prob := (binariesStep{}).plan(env); prob != nil || !p.done {
		t.Errorf("repair mode with an install = %+v %v, want done", p, prob)
	}
}

func TestCleanupRemovesOnlyOurStaleBinariesAndCanUndo(t *testing.T) {
	env := newTestSetupEnv(t)
	writeExe(t, filepath.Join(env.binDir, "abctl"), "#!/bin/sh\n# github.com/rossoctl/cortex/cmd/abctl\n")
	writeExe(t, filepath.Join(env.binDir, "authbridge-proxy"), "#!/bin/sh\n# github.com/rossoctl/cortex/authbridge/cmd/authbridge-proxy\n")
	if p, _ := (cleanupStep{afterService: false}).plan(env); p.what != "pre-rename abctl" {
		t.Errorf("before the service step, plan = %q, want abctl only", p.what)
	}
	p, _ := cleanupStep{afterService: true}.plan(env)
	if p.hidden || p.what != "pre-rename abctl, authbridge-proxy" {
		t.Fatalf("plan = %+v", p)
	}
	_, u, err := cleanupStep{afterService: true}.apply(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(env.binDir, "abctl")); err == nil {
		t.Error("our abctl was not removed")
	}
	if _, err := os.Stat(filepath.Join(env.previousDir(), "abctl")); err != nil {
		t.Error("abctl was not moved into previous/")
	}
	if !strings.Contains(u.manual, "~/.cortex/previous/ back into ~/.local/bin") {
		t.Errorf("undo manual = %q, want the previous/ copy-back", u.manual)
	}
	if err := u.fn(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(env.binDir, "abctl")); err != nil {
		t.Error("undo did not restore abctl")
	}
	if _, err := os.Stat(filepath.Join(env.previousDir(), "abctl")); err == nil {
		t.Error("undo left its copy of abctl in previous/")
	}
}

func TestCleanupKeepsABinaryAUnitStillRuns(t *testing.T) {
	for _, unit := range []string{
		filepath.Join("Library", "LaunchAgents", launchdLabel+".plist"),
		filepath.Join(".config", "systemd", "user", systemdUnit),
	} {
		env := newTestSetupEnv(t)
		writeExe(t, filepath.Join(env.binDir, "authbridge-proxy"), "#!/bin/sh\n# github.com/rossoctl/cortex/cmd/authbridge-proxy\n")
		writeExe(t, filepath.Join(env.home, unit), "<string>"+filepath.Join(env.binDir, "authbridge-proxy")+"</string>")
		detail, _, err := cleanupStep{afterService: true}.apply(env, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(env.binDir, "authbridge-proxy")); err != nil {
			t.Errorf("%s: a binary the unit still runs was removed", unit)
		}
		if detail != "kept authbridge-proxy: the service still runs it" {
			t.Errorf("%s: detail = %q", unit, detail)
		}
	}
}

// A live pidfile process that ps cannot name may still be a pre-rename proxy, so
// authbridge-proxy stays; abctl is not subject to that guard.
func TestCleanupKeepsAuthbridgeProxyWhileAnUnnamedProcessHoldsThePidfile(t *testing.T) {
	env := newTestSetupEnv(t)
	writeExe(t, filepath.Join(env.binDir, "abctl"), "#!/bin/sh\n# github.com/rossoctl/cortex/cmd/abctl\n")
	writeExe(t, filepath.Join(env.binDir, "authbridge-proxy"), "#!/bin/sh\n# github.com/rossoctl/cortex/cmd/authbridge-proxy\n")
	if err := os.MkdirAll(env.cortexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pid := []byte(strconv.Itoa(os.Getpid()) + "\n")
	if err := os.WriteFile(filepath.Join(env.cortexDir, "proxy.pid"), pid, 0o600); err != nil {
		t.Fatal(err)
	}
	installStub(t, "ps", "#!/bin/sh\nexit 1\n")
	detail, _, err := cleanupStep{afterService: true}.apply(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(env.binDir, "authbridge-proxy")); err != nil {
		t.Error("authbridge-proxy was removed while an unnamed process held the pidfile")
	}
	if detail != "removed abctl; kept authbridge-proxy: the service still runs it" {
		t.Errorf("detail = %q", detail)
	}
}

func TestCleanupIgnoresSomeoneElsesAbctl(t *testing.T) {
	env := newTestSetupEnv(t)
	writeExe(t, filepath.Join(env.binDir, "abctl"), "#!/bin/sh\necho abctl v1.0.0\n")
	if p, _ := (cleanupStep{afterService: true}).plan(env); !p.hidden {
		t.Errorf("a foreign abctl was planned for removal: %+v", p)
	}
}

// remove_stale matches cmd/<old>, not just our module: a file named abctl that
// carries another of our commands' paths is not a pre-rename abctl.
func TestCleanupWantsTheOldCommandsOwnPath(t *testing.T) {
	env := newTestSetupEnv(t)
	writeExe(t, filepath.Join(env.binDir, "abctl"), "#!/bin/sh\n# github.com/rossoctl/cortex/cmd/agentop\n")
	if p, _ := (cleanupStep{afterService: true}).plan(env); !p.hidden {
		t.Errorf("an abctl without cmd/abctl was planned for removal: %+v", p)
	}
}
