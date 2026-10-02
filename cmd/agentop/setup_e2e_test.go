package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type setupScene struct {
	home, stage, loaded string
}

// newSetupScene is a machine for runSetup: a temp HOME on zsh, the fake
// supervisor, ports that read free, a stub cortex whose health endpoint healthz
// serves, and a terminal that answers yes.
func newSetupScene(t *testing.T, healthz http.HandlerFunc) setupScene {
	t.Helper()
	loaded := fakeSupervisor(t)
	freePorts(t)
	srv := httptest.NewServer(healthz)
	t.Cleanup(srv.Close)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")
	stubTerminal(t, true, "y\n")
	return setupScene{home: home, stage: stageDir(t, cortexStub(strings.TrimPrefix(srv.URL, "http://"))), loaded: loaded}
}

func stubTerminal(t *testing.T, has bool, answer string) {
	t.Helper()
	savedHas, savedConfirm := setupHasTerminal, setupConfirm
	setupHasTerminal = func() bool { return has }
	setupConfirm = func(w io.Writer) bool { return setupConfirmFrom(strings.NewReader(answer), w) }
	t.Cleanup(func() { setupHasTerminal, setupConfirm = savedHas, savedConfirm })
}

func (sc setupScene) run(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runSetup(args, &out, &errb)
	return code, out.String() + errb.String()
}

func runtimeGOOS() string { return runtime.GOOS }

// homeFiles is every regular file under home with its mode and content hash.
// proxy.log is left out: rollback keeps it on purpose, since it explains the
// failure.
func homeFiles(t *testing.T, home string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(home, p)
		if d.Type()&fs.ModeSymlink != 0 {
			target, _ := os.Readlink(p)
			files[rel] = "link → " + target
			return nil
		}
		if !d.Type().IsRegular() || rel == filepath.Join(".cortex", "proxy.log") {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		fi, _ := d.Info()
		files[rel] = fmt.Sprintf("%v %x", fi.Mode().Perm(), sha256.Sum256(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func sameFiles(t *testing.T, before, after map[string]string) {
	t.Helper()
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s changed: %s → %s", k, v, after[k])
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			t.Errorf("%s was left behind", k)
		}
	}
}

func TestSetupFreshInstallEndToEnd(t *testing.T) {
	sc := newSetupScene(t, ok200)
	code, out := sc.run(t, "--from", sc.stage, "--yes", "--claude-code")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{
		"  ✓ installed    agentop, cortex → ~/.local/bin\n",
		"  ✓ PATH         ~/.zshrc\n",
		"  ✓ config       ~/.cortex/config.yaml\n",
		"  ✓ started      " + supervisorName(runtimeGOOS()) + " · healthy on 127.0.0.1:47600\n",
		"  ✓ routed       Claude Code → Cortex\n",
		"  cortex " + version + " ready.\n",
		"    agentop        # watch your agent traffic live\n",
		"  Undo any time: agentop service uninstall · agentop configure claude-code disable\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(sc.home, ".cortex", "previous")); err == nil {
		t.Error("a successful run left previous/ behind")
	}
}

func TestSetupReRunIsOneLine(t *testing.T) {
	sc := newSetupScene(t, ok200)
	if code, out := sc.run(t, "--from", sc.stage, "--yes", "--claude-code"); code != 0 {
		t.Fatalf("first run: %d\n%s", code, out)
	}
	code, out := sc.run(t, "--from", sc.stage, "--yes", "--claude-code")
	if code != 0 || !strings.Contains(out, "  cortex "+version+" is installed and healthy.\n") || strings.Contains(out, "✓") {
		t.Errorf("re-run exit %d:\n%s", code, out)
	}
}

func TestSetupWithNoTerminalChangesNothing(t *testing.T) {
	sc := newSetupScene(t, ok200)
	stubTerminal(t, false, "")
	before := homeFiles(t, sc.home)
	code, out := sc.run(t, "--from", sc.stage, "--claude-code")
	if code != exitDeclined || !strings.Contains(out, "No terminal to ask on — re-run with --yes to apply.") ||
		!strings.Contains(out, "  This will:\n") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	sameFiles(t, before, homeFiles(t, sc.home))
}

func TestSetupDeclinedChangesNothing(t *testing.T) {
	sc := newSetupScene(t, ok200)
	stubTerminal(t, true, "n\n")
	before := homeFiles(t, sc.home)
	code, out := sc.run(t, "--from", sc.stage)
	if code != exitDeclined || !strings.Contains(out, "  Not changed.\n") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	sameFiles(t, before, homeFiles(t, sc.home))
}

func TestSetupProblemsChangeNothing(t *testing.T) {
	sc := newSetupScene(t, ok200)
	writeExe(t, filepath.Join(sc.home, settingsRel), `{"env":{"HTTPS_PROXY":"http://corp:3128"}}`)
	before := homeFiles(t, sc.home)
	code, out := sc.run(t, "--from", sc.stage, "--yes", "--claude-code")
	if code != 1 || !strings.Contains(out, "  ✗ routed") || !strings.Contains(out, "  Nothing was changed.\n") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	sameFiles(t, before, homeFiles(t, sc.home))
}

// A sandbox: no supervisor, so the service step starts the stub's --supervise in
// the background, and the ending says how to stop it.
func TestSetupUnsupervisedNamesTheStopCommand(t *testing.T) {
	sc := newSetupScene(t, ok200)
	fakeNoSupervisor(t)
	var pid int
	stopProcessOnCleanup(t, &pid)
	code, out := sc.run(t, "--from", sc.stage, "--yes")
	pid = readPIDFile(filepath.Join(sc.home, ".cortex", "proxy.pid"))
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{"  ✓ started      in the background · healthy\n", stopBackgroundLine} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// An upgrade keeps the old binaries in previous/ until every step has applied;
// the success hooks then remove it.
func TestSetupUpgradeRemovesPrevious(t *testing.T) {
	sc := newSetupScene(t, ok200)
	if code, out := sc.run(t, "--from", sc.stage, "--yes"); code != 0 {
		t.Fatalf("first run: %d\n%s", code, out)
	}
	writeExe(t, filepath.Join(sc.stage, "agentop"), "#!/bin/sh\necho agentop v9.9.10\n")
	code, out := sc.run(t, "--from", sc.stage, "--yes")
	if code != 0 || !strings.Contains(out, "  ✓ installed    agentop, cortex v9.9.9 → "+version+"\n") {
		t.Fatalf("upgrade exit %d:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(sc.home, ".cortex", "previous")); err == nil {
		t.Error("a successful upgrade left previous/ behind")
	}
}

// The handoff flags mark installer mode, but a --from dir outside the temp roots
// is never deleted: it may be a source tree's bin dir.
func TestSetupInstallerModeKeepsAFromDirOutsideTheTempRoots(t *testing.T) {
	sc := newSetupScene(t, ok200)
	saved := systemTempRoots
	systemTempRoots = func() []string { return nil } // leaves ~/.cortex/tmp, which sc.stage is not in
	t.Cleanup(func() { systemTempRoots = saved })
	if code, out := sc.run(t, "--from", sc.stage, "--yes", "--handoff-bytes=2048"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(sc.stage, "agentop")); err != nil {
		t.Errorf("installer mode deleted a --from dir outside the temp roots: %v", err)
	}
}
