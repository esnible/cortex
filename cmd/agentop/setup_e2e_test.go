package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/checklist"
)

type setupScene struct {
	home, stage, loaded string
}

// newSetupScene is a machine for runSetup: a temp HOME on zsh, the fake
// supervisor, ports that read free, a stub cortex whose health endpoint healthz
// serves, and a terminal that answers yes.
func newSetupScene(t *testing.T, healthz http.HandlerFunc) setupScene {
	t.Helper()
	plainOutput(t)
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
		"  cortex v9.9.9 ready.\n", // the staged agentop's version
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
	if code != 0 || !strings.Contains(out, "  cortex v9.9.9 is installed and healthy.\n") || strings.Contains(out, "✓") {
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
	if strings.Contains(out, "agentop service uninstall") {
		t.Errorf("the undo names service uninstall, which leaves a background proxy running:\n%s", out)
	}
}

// donePlan is a step with nothing to do; prior says the service plan's health
// check answered.
type donePlan struct {
	label  string
	advice *problem
	prior  bool
}

func (d donePlan) name() string { return d.label }

func (d donePlan) plan(env *setupEnv) (stepPlan, *problem) {
	env.priorService = env.priorService || d.prior
	return stepPlan{label: d.label, done: true, doneMsg: "current", advice: d.advice}, nil
}

func (d donePlan) apply(*setupEnv, *checklist.Running) (string, undo, error) {
	return "", undo{}, errors.New("a done step was applied")
}

// With nothing to change, setup says healthy only when a health check answered,
// and still shows a step's advice.
func TestSetupWithNothingToChangeSaysOnlyWhatItChecked(t *testing.T) {
	path := &problem{reason: "~/.local/bin is not on PATH", fix: []string{"exec zsh"}}
	for _, c := range []struct {
		name  string
		steps []step
		want  string
	}{
		{"health answered", []step{donePlan{label: "started", prior: true}}, "  cortex " + version + " is installed and healthy.\n"},
		{"found running, not asked", []step{donePlan{label: "started"}}, "  cortex " + version + " is installed and running.\n"},
		{"advice", []step{donePlan{label: "PATH", advice: path}, donePlan{label: "started", prior: true}},
			"  ! PATH         ~/.local/bin is not on PATH\n      fix: exec zsh\n\n  cortex " + version + " is installed and healthy.\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			sc := newSetupScene(t, ok200)
			saved := setupStepsHook
			setupStepsHook = func([]step) []step { return c.steps }
			t.Cleanup(func() { setupStepsHook = saved })
			if code, out := sc.run(t, "--yes"); code != 0 || !strings.HasSuffix(out, c.want) {
				t.Errorf("exit %d, want the ending %q:\n%s", code, c.want, out)
			}
		})
	}
}

// Without a PATH edit the commands are spelled out from ~, and a re-run keeps the
// PATH advice rather than hiding it behind its one line.
func TestSetupWithoutAPathEditSpellsOutTheCommand(t *testing.T) {
	sc := newSetupScene(t, ok200)
	code, out := sc.run(t, "--from", sc.stage, "--yes", "--no-modify-path")
	if code != 0 || !strings.Contains(out, "\n    ~/.local/bin/agentop exec -- <cmd>        # send an agent through Cortex\n") {
		t.Errorf("fresh install exit %d, want the command from ~:\n%s", code, out)
	}
	code, out = sc.run(t, "--from", sc.stage, "--yes", "--no-modify-path")
	for _, want := range []string{"  ! PATH         ~/.local/bin is not on PATH — not editing dotfiles\n",
		"      fix: export PATH=", "  cortex v9.9.9 is installed and healthy.\n"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("re-run exit %d, output lacks %q:\n%s", code, want, out)
		}
	}
}

// An install-only run ends on the command left to run, setup, and on where it
// can be run from.
func TestSetupInstallOnlyEndsOnSetup(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"PATH added for new terminals", nil, "  Installed. Open a new terminal, then start Cortex with:  agentop setup\n"},
		{"--no-modify-path", []string{"--no-modify-path"}, "  Installed. Start Cortex with:  ~/.local/bin/agentop setup\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			sc := newSetupScene(t, ok200)
			code, out := sc.run(t, append([]string{"--from", sc.stage, "--yes", "--install-only"}, c.args...)...)
			if code != 0 || !strings.HasSuffix(out, c.want) {
				t.Errorf("exit %d, want the ending %q:\n%s", code, c.want, out)
			}
		})
	}
}

// An upgrade keeps the old binaries in previous/ until every step has applied;
// the success hooks then remove it. Its lines name the staged version, not the
// version of the agentop running setup.
func TestSetupUpgradeRemovesPrevious(t *testing.T) {
	sc := newSetupScene(t, ok200)
	if code, out := sc.run(t, "--from", sc.stage, "--yes"); code != 0 {
		t.Fatalf("first run: %d\n%s", code, out)
	}
	writeExe(t, filepath.Join(sc.stage, "agentop"), "#!/bin/sh\necho agentop v9.9.10\n")
	code, out := sc.run(t, "--from", sc.stage, "--yes")
	if code != 0 {
		t.Fatalf("upgrade exit %d:\n%s", code, out)
	}
	for _, want := range []string{"   v9.9.9 → v9.9.10 · ", "  ✓ installed    agentop, cortex v9.9.9 → v9.9.10\n", "  cortex v9.9.10 ready.\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("the upgrade's output lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(sc.home, ".cortex", "previous")); err == nil {
		t.Error("a successful upgrade left previous/ behind")
	}
}

// The header and the binaries plan both want the installed agentop's version, and
// the header the staged one's: each binary runs once.
func TestSetupAsksEachAgentopItsVersionOnce(t *testing.T) {
	sc := newSetupScene(t, ok200)
	if code, out := sc.run(t, "--from", sc.stage, "--yes"); code != 0 {
		t.Fatalf("first run: %d\n%s", code, out)
	}
	calls := filepath.Join(t.TempDir(), "calls")
	counting := func(name, v string) string {
		return "#!/bin/sh\necho " + name + " >> '" + calls + "'\necho agentop " + v + "\n"
	}
	writeExe(t, filepath.Join(sc.home, ".local", "bin", "agentop"), counting("installed", "v9.9.9"))
	writeExe(t, filepath.Join(sc.stage, "agentop"), counting("staged", "v9.9.10"))
	code, out := sc.run(t, "--from", sc.stage, "--yes")
	if b, _ := os.ReadFile(calls); code != 0 || string(b) != "installed\nstaged\n" {
		t.Errorf("exit %d; the agentops ran %q, want each once:\n%s", code, b, out)
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

// fakeSignals makes setupSignals hand setup the returned channel, for the test to
// send on, and reports whether setup has asked for it yet.
func fakeSignals(t *testing.T) (chan<- os.Signal, *atomic.Bool) {
	t.Helper()
	c := make(chan os.Signal, 1)
	var subscribed atomic.Bool
	saved := setupSignals
	setupSignals = func() (<-chan os.Signal, func()) {
		subscribed.Store(true)
		return c, func() {}
	}
	t.Cleanup(func() { setupSignals = saved })
	return c, &subscribed
}

// planHook runs fn as the step it wraps plans.
type planHook struct {
	step
	fn func()
}

func (h planHook) plan(env *setupEnv) (stepPlan, *problem) {
	h.fn()
	return h.step.plan(env)
}

// A Ctrl-C or SIGTERM before apply changes nothing: setup catches it from the
// start rather than dying of it, so it deletes the installer's staging dir, which
// exec put past the script's EXIT trap, and exits 3, as declining does.
func TestSetupASignalBeforeApplyChangesNothing(t *testing.T) {
	declined := func(t *testing.T, sc setupScene, before map[string]string, code int, out string) {
		t.Helper()
		if code != exitDeclined || !strings.Contains(out, "  Not changed.\n") || strings.Contains(out, "✓ installed") {
			t.Errorf("exit %d, want %d with nothing applied:\n%s", code, exitDeclined, out)
		}
		if _, err := os.Stat(sc.stage); err == nil {
			t.Error("the staging dir survived a signal before apply")
		}
		sameFiles(t, before, homeFiles(t, sc.home))
	}
	t.Run("while planning", func(t *testing.T) {
		sc := newSetupScene(t, ok200)
		sigs, subscribed := fakeSignals(t)
		saved := setupStepsHook
		setupStepsHook = func(s []step) []step {
			out := append([]step(nil), s...)
			out[0] = planHook{out[0], func() {
				if !subscribed.Load() {
					t.Error("setup plans before it catches signals: one now would kill it and leave the stage")
				}
				sigs <- os.Interrupt
			}}
			return out
		}
		t.Cleanup(func() { setupStepsHook = saved })
		before := homeFiles(t, sc.home)
		code, out := sc.run(t, "--from", sc.stage, "--yes", "--handoff-bytes=2048")
		declined(t, sc, before, code, out)
	})
	t.Run("at the prompt", func(t *testing.T) {
		sc := newSetupScene(t, ok200)
		sigs, subscribed := fakeSignals(t)
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		saved := setupConfirm
		setupConfirm = func(w io.Writer) bool {
			_, _ = fmt.Fprint(w, "  Continue? [Y/n] ")
			if !subscribed.Load() {
				t.Error("setup asks before it catches signals: a Ctrl-C now would kill it and leave the stage")
			}
			sigs <- os.Interrupt
			// A yes after the Ctrl-C, too late: setup has stopped waiting by then.
			select {
			case <-release:
			case <-time.After(2 * time.Second):
			}
			return true
		}
		t.Cleanup(func() { setupConfirm = saved })
		before := homeFiles(t, sc.home)
		code, out := sc.run(t, "--from", sc.stage, "--handoff-bytes=2048")
		declined(t, sc, before, code, out)
		if !strings.Contains(out, "  Continue? [Y/n] \n  Not changed.\n") {
			t.Errorf("Not changed. is not on a line of its own after the prompt:\n%s", out)
		}
	})
}
