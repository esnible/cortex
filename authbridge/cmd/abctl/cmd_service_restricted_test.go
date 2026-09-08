package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// fakeLaunchctl puts a launchctl on PATH that behaves like a restricted sandbox: it
// cannot answer, exactly as reported from a real one.
func fakeLaunchctl(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "launchctl"), []byte(body), 0o700); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestLabelGone_UnknownIsNotGone is the defect this fixes. launchctl print exits 113
// with "Could not find service" when a label is really absent, but 125 or a permission
// denial when it cannot see the domain. Treating the latter as "gone" walked into a
// bootstrap that failed with a bare EIO, reporting the one state we had ruled out.
func TestLabelGone_UnknownIsNotGone(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchctl only")
	}
	t.Run("really absent reads as gone", func(t *testing.T) {
		fakeLaunchctl(t, "#!/bin/sh\necho 'Could not find service \"x\" in domain for user gui: 501' >&2\nexit 113\n")
		gone, known := labelGone("gui/501/whatever")
		if !gone || !known {
			t.Errorf("gone=%v known=%v, want true/true", gone, known)
		}
	})
	t.Run("cannot query does NOT read as gone", func(t *testing.T) {
		fakeLaunchctl(t, "#!/bin/sh\necho 'Could not print domain: 125: Domain does not support specified action' >&2\nexit 125\n")
		gone, known := labelGone("gui/501/whatever")
		if gone {
			t.Error("a domain we cannot query was reported as gone — this is the bug")
		}
		if known {
			t.Error("claimed to know the state it could not query")
		}
	})
	t.Run("permission denied does NOT read as gone", func(t *testing.T) {
		fakeLaunchctl(t, "#!/bin/sh\necho 'Operation not permitted' >&2\nexit 1\n")
		if gone, _ := labelGone("gui/501/whatever"); gone {
			t.Error("a denial was reported as gone")
		}
	})
	t.Run("present reads as present", func(t *testing.T) {
		fakeLaunchctl(t, "#!/bin/sh\necho 'state = running'\nexit 0\n")
		gone, known := labelGone("gui/501/whatever")
		if gone || !known {
			t.Errorf("gone=%v known=%v, want false/true", gone, known)
		}
	})
}

// TestLaunchdUsable distinguishes "cannot talk to launchd" from "our job is not
// loaded" — the difference between an explainable failure and a bare EIO.
func TestLaunchdUsable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd only")
	}
	t.Run("a restricted domain is not usable, and says why", func(t *testing.T) {
		fakeLaunchctl(t, "#!/bin/sh\necho 'Could not print domain: 125: Domain does not support specified action' >&2\nexit 125\n")
		ok, why := launchdUsable()
		if ok {
			t.Error("reported usable in a restricted environment")
		}
		if !strings.Contains(why, "125") && !strings.Contains(why, "Domain") {
			t.Errorf("reason does not explain anything: %q", why)
		}
	})
	t.Run("a working domain is usable", func(t *testing.T) {
		fakeLaunchctl(t, "#!/bin/sh\necho 'com.apple.something'\nexit 0\n")
		if ok, why := launchdUsable(); !ok {
			t.Errorf("reported unusable against a working launchctl: %s", why)
		}
	})
}

// TestServiceInstall_RestrictedEnvironment: no unit on disk, a distinct exit code so
// install.sh can offer the unsupervised path, and a message naming the way forward.
func TestServiceInstall_RestrictedEnvironment(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd only")
	}
	fakeLaunchctl(t, "#!/bin/sh\necho 'Could not print domain: 125' >&2\nexit 125\n")
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, ".cortex")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(cfgDir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("mode: proxy-sidecar\nlistener:\n  roles: [forward]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(home, "unit.plist")
	p, err := resolveServicePaths(cfg, unit, filepath.Join(home, "proxy"))
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	code := serviceInstall(p, true, &out, &errOut)

	if code != exitNoSupervisor {
		t.Errorf("exit = %d, want %d so install.sh can offer the fallback", code, exitNoSupervisor)
	}
	if _, serr := os.Stat(unit); serr == nil {
		t.Error("a unit was written in an environment that cannot load it")
	}
	msg := errOut.String()
	for _, want := range []string{"cannot manage", "--local"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message is missing %q, so the user has no way forward:\n%s", want, msg)
		}
	}
}

// TestLoginHome reports the user-record home, which is what launchd scans — not $HOME.
func TestLoginHome(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("dscl only")
	}
	if _, err := exec.LookPath("dscl"); err != nil {
		t.Skip("no dscl")
	}
	// A bogus $HOME must not change the answer: that independence is the whole point.
	t.Setenv("HOME", "/tmp/definitely-not-my-home")
	got := loginHome()
	if got == "" {
		t.Skip("could not read the user record here")
	}
	if got == "/tmp/definitely-not-my-home" {
		t.Error("loginHome returned $HOME; it must come from the user record")
	}
	if !strings.HasPrefix(got, "/") {
		t.Errorf("loginHome = %q, want an absolute path", got)
	}
	_ = strconv.Itoa(os.Getuid())
}
