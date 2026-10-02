package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func ok200(w http.ResponseWriter, _ *http.Request) {}

func TestServiceStepFreshInstallAndUndo(t *testing.T) {
	loaded := fakeSupervisor(t)
	env := serviceEnv(t, ok200)
	p, prob := serviceStep{}.plan(env)
	if prob != nil || p.done || !strings.Contains(p.where, supervisorName(env.goos)) || env.priorService {
		t.Fatalf("plan = %+v %v prior=%v", p, prob, env.priorService)
	}
	detail, u, err := serviceStep{}.apply(env, nil)
	if err != nil || !strings.HasSuffix(detail, "healthy on 127.0.0.1:47600") {
		t.Fatalf("apply = %q %v", detail, err)
	}
	if _, err := os.Stat(loaded); err != nil {
		t.Fatal("the fake job is not loaded")
	}
	if u.manual != "agentop service uninstall" {
		t.Errorf("fresh undo manual = %q", u.manual)
	}
	if err := u.fn(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(loaded); err == nil {
		t.Error("undo left the job loaded")
	}
	sp, _ := resolveServicePaths(env.configPath(), "", filepath.Join(env.binDir, "cortex"))
	for _, f := range []string{sp.unitFile, sp.stampFile} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("undo left %s", f)
		}
	}
}

func TestServiceStepIsDoneWhenCurrent(t *testing.T) {
	fakeSupervisor(t)
	env := serviceEnv(t, ok200)
	if _, prob := (serviceStep{}).plan(env); prob != nil {
		t.Fatal(prob)
	}
	if _, _, err := (serviceStep{}).apply(env, nil); err != nil {
		t.Fatal(err)
	}
	again := *env
	again.configFresh, again.binaryChanges = false, nil
	if p, prob := (serviceStep{}).plan(&again); prob != nil || !p.done || !again.priorService {
		t.Errorf("a current service planned a change: %+v %v", p, prob)
	}
}

func TestServiceStepUpgradeUndoRestoresTheUnitAndThePrior(t *testing.T) {
	loaded := fakeSupervisor(t)
	env := serviceEnv(t, ok200)
	if _, prob := (serviceStep{}).plan(env); prob != nil {
		t.Fatal(prob)
	}
	if _, _, err := (serviceStep{}).apply(env, nil); err != nil {
		t.Fatal(err)
	}
	sp, _ := resolveServicePaths(env.configPath(), "", filepath.Join(env.binDir, "cortex"))
	// An older agentop's unit, so the upgrade's rewrite changes its bytes: without
	// this the rewrite is byte-identical and an undo that skipped the unit would pass.
	unitBefore, _ := os.ReadFile(sp.unitFile)
	unitBefore = append(unitBefore, "\n# written by an older agentop\n"...)
	if err := os.WriteFile(sp.unitFile, unitBefore, 0o644); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	stampBefore, _ := os.ReadFile(sp.stampFile)

	up := *env
	up.configFresh, up.binaryChanges = false, []string{"cortex"}
	writeExe(t, filepath.Join(env.binDir, "cortex"), cortexStub("x")+"# v2\n")
	if p, prob := (serviceStep{}).plan(&up); prob != nil || p.done || !up.priorService {
		t.Fatalf("upgrade plan = %+v %v prior=%v", p, prob, up.priorService)
	}
	_, u, err := serviceStep{}.apply(&up, nil)
	if err != nil {
		t.Fatal(err)
	}
	if u.manual != "agentop service install --restart" {
		t.Errorf("upgrade undo manual = %q", u.manual)
	}
	if err := u.fn(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(sp.unitFile); !bytes.Equal(b, unitBefore) {
		t.Error("undo did not restore the unit's bytes")
	}
	if b, _ := os.ReadFile(sp.stampFile); !bytes.Equal(b, stampBefore) {
		t.Error("undo did not restore the stamp")
	}
	if up.restorePrior == nil {
		t.Fatal("an upgrade over a serving Cortex set no restorePrior")
	}
	// The job is still loaded from the upgrade: unload it, as a failed later step's
	// rollback would find it, so only restorePrior can bring it back.
	if err := os.Remove(loaded); err != nil {
		t.Fatal(err)
	}
	if err := up.restorePrior(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(loaded); err != nil {
		t.Error("restorePrior did not load the job")
	}
}

// stopProcessOnCleanup kills *pid, if it is still alive when the test ends:
// startUnsupervised's own goroutine reaps it, so this waits for it to go.
func stopProcessOnCleanup(t *testing.T, pid *int) {
	t.Helper()
	t.Cleanup(func() {
		if !alive(*pid) {
			return
		}
		_ = syscall.Kill(*pid, syscall.SIGKILL)
		for i := 0; i < 50 && alive(*pid); i++ {
			time.Sleep(100 * time.Millisecond)
		}
	})
}

func TestServiceStepRunsUnsupervisedInASandbox(t *testing.T) {
	fakeNoSupervisor(t)
	env := serviceEnv(t, ok200)
	p, prob := serviceStep{}.plan(env)
	if prob != nil || !env.unsupervised || !strings.HasPrefix(p.where, "unsupervised") {
		t.Fatalf("plan = %+v %v", p, prob)
	}
	var pid int
	stopProcessOnCleanup(t, &pid)
	_, u, err := serviceStep{}.apply(env, nil)
	pid = readPIDFile(filepath.Join(env.cortexDir, "proxy.pid"))
	if err != nil {
		t.Fatal(err)
	}
	if !alive(pid) {
		t.Fatal("no background proxy")
	}
	if u.manual != "kill $(cat ~/.cortex/proxy.pid)" {
		t.Errorf("unsupervised undo manual = %q", u.manual)
	}
	if err := u.fn(); err != nil {
		t.Fatal(err)
	}
	if alive(pid) {
		t.Error("undo left the background proxy running")
		return
	}
	pid = 0 // gone: the cleanup must not signal whatever reuses the number
}

func TestServiceStepPreflight(t *testing.T) {
	t.Run("a busy port on a fresh install", func(t *testing.T) {
		fakeSupervisor(t)
		env := newTestSetupEnv(t)
		env.configFresh = true
		saved := portInUse
		portInUse = func(p string) bool { return p == "47601" }
		t.Cleanup(func() { portInUse = saved })
		_, prob := serviceStep{}.plan(env)
		if prob == nil || prob.reason != "port 47601 is already in use by something else" {
			t.Errorf("problem = %+v", prob)
		}
	})
	t.Run("a foreign holder of 47600", func(t *testing.T) {
		fakeSupervisor(t)
		env := serviceEnv(t, ok200)
		strangerOn(t, "47600")
		_, prob := serviceStep{}.plan(env)
		if prob == nil || !strings.Contains(prob.reason, "/usr/bin/python3 (pid 31337), not this Cortex") {
			t.Errorf("problem = %+v", prob)
		}
	})
	// The port checked is the one the config names, so moving it, as the fresh
	// install's busy-port advice says to, is not refused on the old one.
	t.Run("a stranger on 47600 after the config moved to 47700", func(t *testing.T) {
		fakeSupervisor(t)
		env := serviceEnv(t, ok200)
		moveForwardPort(t, env, "127.0.0.1:47700")
		strangerOn(t, "47600")
		if _, prob := (serviceStep{}).plan(env); prob != nil {
			t.Errorf("a stranger on the old port refused the moved one: %+v", prob)
		}
	})
	t.Run("a stranger on the moved port", func(t *testing.T) {
		fakeSupervisor(t)
		env := serviceEnv(t, ok200)
		moveForwardPort(t, env, "127.0.0.1:47700")
		strangerOn(t, "47700")
		_, prob := serviceStep{}.plan(env)
		if prob == nil || !strings.HasPrefix(prob.reason, "127.0.0.1:47700 is held by /usr/bin/python3 (pid 31337)") {
			t.Errorf("moved-port problem = %+v", prob)
		}
	})
	// The holder is ours when proxy.pid names it, or when it runs the installed
	// cortex: each case pins one of the two paths the step hands ourProxy.
	for _, tc := range []struct {
		name    string
		pidFile bool
	}{{"our proxy by its pidfile", true}, {"our proxy by its executable", false}} {
		t.Run(tc.name, func(t *testing.T) {
			fakeSupervisor(t)
			env := serviceEnv(t, ok200)
			installStub(t, "lsof", `#!/bin/sh
case "$1" in
  -nP) case "$2" in -iTCP@127.0.0.1:47600) printf 'p31337\n' ;; esac ;;
esac
exit 0
`)
			exe := filepath.Join(env.binDir, "cortex")
			if tc.pidFile {
				exe = "/usr/bin/python3"
				if err := os.WriteFile(filepath.Join(env.cortexDir, "proxy.pid"), []byte("31337\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			fakeProc(t, 31337, exe)
			if _, prob := (serviceStep{}).plan(env); prob != nil {
				t.Errorf("our own proxy read as foreign: %+v", prob)
			}
		})
	}
}

// strangerOn makes lsof name pid 31337, /usr/bin/python3, as the listener on
// 127.0.0.1:port.
func strangerOn(t *testing.T, port string) {
	t.Helper()
	installStub(t, "lsof", `#!/bin/sh
case "$1" in
  -nP) case "$2" in -iTCP@127.0.0.1:`+port+`) printf 'p31337\n' ;; esac ;;
  -p) printf 'p31337\nftxt\nn/usr/bin/python3\n' ;;
esac
exit 0
`)
	fakeProc(t, 31337, "")
}

// moveForwardPort rewrites the config's forward_proxy_addr to addr.
func moveForwardPort(t *testing.T, env *setupEnv, addr string) {
	t.Helper()
	b, err := os.ReadFile(env.configPath())
	if err != nil {
		t.Fatal(err)
	}
	moved := strings.Replace(string(b), "forward_proxy_addr: 127.0.0.1:47600", "forward_proxy_addr: "+addr, 1)
	if moved == string(b) {
		t.Fatal("the config names no forward_proxy_addr to move")
	}
	if err := os.WriteFile(env.configPath(), []byte(moved), 0o600); err != nil {
		t.Fatal(err)
	}
}

// logSupervisorCalls wraps fakeSupervisor's launchctl and systemctl, so each call
// is appended to the returned log before the fake answers it. It refuses to wrap
// anything but the fakes, which name loaded: the real launchctl acts on the real
// io.rossoctl.cortex.
func logSupervisorCalls(t *testing.T, loaded string) string {
	t.Helper()
	calls := filepath.Join(t.TempDir(), "calls.log")
	for _, tool := range []string{"launchctl", "systemctl"} {
		fake, err := exec.LookPath(tool)
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(fake); !strings.Contains(string(b), loaded) { //nolint:gosec
			t.Fatalf("%s on PATH is not fakeSupervisor's", fake)
		}
		installStub(t, tool, "#!/bin/sh\necho '"+tool+" '\"$*\" >> '"+calls+"'\nexec '"+fake+"' \"$@\"\n")
	}
	return calls
}

// With the service current, a config step that adds listener pins makes the
// service step restart it: the config step's migration leaves runServiceInstall's
// own with nothing to do. Bob's rate alone does not, as the proxy reloads pricing.
func TestServiceStepRestartsForNewPinsButNotForBobsRate(t *testing.T) {
	for _, tc := range []struct {
		name string
		pins bool
	}{{"new listener pins", true}, {"Bob's rate only", false}} {
		t.Run(tc.name, func(t *testing.T) {
			loaded := fakeSupervisor(t)
			env := serviceEnv(t, ok200)
			unpinned, err := os.ReadFile(env.configPath())
			if err != nil {
				t.Fatal(err)
			}
			if _, prob := (serviceStep{}).plan(env); prob != nil {
				t.Fatal(prob)
			}
			if _, _, err := (serviceStep{}).apply(env, nil); err != nil {
				t.Fatal(err)
			}
			// The service is current; its config goes back to needing the migrations.
			if err := os.WriteFile(env.configPath(), unpinned, 0o600); err != nil {
				t.Fatal(err)
			}
			if !tc.pins {
				if _, err := migrateConfig(env.configPath(), io.Discard); err != nil {
					t.Fatal(err)
				}
			}
			next := *env
			if _, prob := (configStep{}).plan(&next); prob != nil || !next.configWillChange || next.configPinsPending != tc.pins {
				t.Fatalf("config plan: %v willChange=%v pinsPending=%v", prob, next.configWillChange, next.configPinsPending)
			}
			if _, _, err := (configStep{}).apply(&next, nil); err != nil || next.configPinsChanged != tc.pins {
				t.Fatalf("config apply: %v pinsChanged=%v", err, next.configPinsChanged)
			}
			calls := logSupervisorCalls(t, loaded)
			p, prob := serviceStep{}.plan(&next)
			if prob != nil || p.done != !tc.pins {
				t.Fatalf("service plan done=%v, want %v (%v)", p.done, !tc.pins, prob)
			}
			if !tc.pins {
				return
			}
			detail, _, err := serviceStep{}.apply(&next, nil)
			if err != nil || strings.HasSuffix(detail, "already running") {
				t.Errorf("new pins did not restart the service: %q %v", detail, err)
			}
			log, _ := os.ReadFile(calls)
			if !strings.Contains(string(log), "launchctl bootstrap") && !strings.Contains(string(log), "systemctl --user restart") {
				t.Errorf("the supervisor saw no start: %q", log)
			}
		})
	}
}

// Unsupervised, beside a Cortex of ours that proxy.pid does not record: a
// supervisor runs it, so setup must not start a second copy on its ports.
func TestServiceStepWillNotStartASecondCortexBesideASupervisedOne(t *testing.T) {
	ourHolder := func(t *testing.T, env *setupEnv) {
		t.Helper()
		installStub(t, "lsof", `#!/bin/sh
case "$1" in
  -nP) case "$2" in -iTCP@127.0.0.1:47600) printf 'p31337\n' ;; esac ;;
esac
exit 0
`)
		fakeProc(t, 31337, filepath.Join(env.binDir, "cortex"))
	}
	sup := func(env *setupEnv) string { return strings.Fields(supervisorName(env.goos))[0] }

	t.Run("nothing changes", func(t *testing.T) {
		fakeNoSupervisor(t)
		env := serviceEnv(t, ok200)
		ourHolder(t, env)
		p, prob := serviceStep{}.plan(env)
		want := "Cortex is already running under " + sup(env) + "; not starting a second copy"
		if prob != nil || !p.done || p.advice == nil || p.advice.reason != want {
			t.Errorf("plan = %+v advice=%+v %v", p, p.advice, prob)
		}
	})
	t.Run("an upgrade in a sandbox", func(t *testing.T) {
		fakeNoSupervisor(t)
		env := serviceEnv(t, ok200)
		ourHolder(t, env)
		env.binaryChanges = []string{"cortex"}
		_, prob := serviceStep{}.plan(env)
		want := "a supervised Cortex is running here, and this environment cannot manage " + sup(env)
		if prob == nil || prob.reason != want || len(prob.fix) != 2 {
			t.Errorf("sandbox problem = %+v", prob)
		}
	})
	t.Run("an upgrade with --no-service", func(t *testing.T) {
		fakeSupervisor(t)
		env := serviceEnv(t, ok200)
		ourHolder(t, env)
		env.opts.noService, env.binaryChanges = true, []string{"cortex"}
		_, prob := serviceStep{}.plan(env)
		if prob == nil || prob.reason != "a supervised Cortex is running here, and --no-service would start a second copy" {
			t.Errorf("--no-service problem = %+v", prob)
		}
	})
	// The background proxy is cortex --supervise, and its child holds the port: the
	// pidfile names the holder's parent, which is still ours, unsupervised.
	t.Run("our background proxy's child", func(t *testing.T) {
		fakeNoSupervisor(t)
		env := serviceEnv(t, ok200)
		child := exec.Command("/bin/sleep", "30")
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
		installStub(t, "lsof", `#!/bin/sh
case "$1" in
  -nP) case "$2" in -iTCP@127.0.0.1:47600) printf 'p`+strconv.Itoa(child.Process.Pid)+`\n' ;; esac ;;
esac
exit 0
`)
		fakeProc(t, child.Process.Pid, filepath.Join(env.binDir, "cortex"))
		if err := os.WriteFile(filepath.Join(env.cortexDir, "proxy.pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		env.binaryChanges = []string{"cortex"}
		p, prob := serviceStep{}.plan(env)
		if prob != nil || p.done || p.advice != nil {
			t.Errorf("our own background proxy read as supervised: %+v advice=%+v %v", p, p.advice, prob)
		}
	})
}

// serviceLoaded asks the supervisor, not the unit file: the service step's undo
// stops the job again only when it was not loaded before.
func TestServiceLoadedFollowsTheSupervisor(t *testing.T) {
	loaded := fakeSupervisor(t)
	if serviceLoaded(runtimeGOOS()) {
		t.Error("a job the supervisor does not have read as loaded")
	}
	if err := os.WriteFile(loaded, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !serviceLoaded(runtimeGOOS()) {
		t.Error("a loaded job read as not loaded")
	}
}

// A failed start shows what that start wrote to the log, not an older run's
// lines: from where the log ended before it, the last logTailLines, the first
// labelled proxy.log in the checklist's label column.
func TestLogTailIsWhatTheStartWrote(t *testing.T) {
	env := newTestSetupEnv(t)
	log := filepath.Join(env.home, "proxy.log")
	if got := logTail(env, log, markLog(log)); got != nil {
		t.Errorf("no log: %q", got)
	}
	if err := os.WriteFile(log, []byte("old run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := markLog(log)
	if got := logTail(env, log, m); got != nil {
		t.Errorf("nothing written since the mark: %q", got)
	}
	f, err := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("one\n\ntwo " + env.home + "/x\nthree\nfour\nfive\nsix\n")
	_ = f.Close()
	want := []string{"proxy.log    two ~/x", "             three", "             four", "             five", "             six"}
	if got := logTail(env, log, m); !slices.Equal(got, want) {
		t.Errorf("tail = %q, want %q", got, want)
	}
	// Rotated: runServiceInstall renames a long log away, so the start writes a new one.
	if err := os.Rename(log, log+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log, []byte("fresh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := logTail(env, log, m); !slices.Equal(got, []string{"proxy.log    fresh"}) {
		t.Errorf("after a rotation, tail = %q", got)
	}
}

// An unsupervised proxy that exits at once shows its own log lines too.
func TestServiceStepUnsupervisedFailureShowsTheLog(t *testing.T) {
	fakeNoSupervisor(t)
	env := serviceEnv(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	if _, prob := (serviceStep{}).plan(env); prob != nil || !env.unsupervised {
		t.Fatalf("plan: %v unsupervised=%v", prob, env.unsupervised)
	}
	log := filepath.Join(env.cortexDir, "proxy.log")
	if err := os.WriteFile(log, []byte("an older run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeExe(t, filepath.Join(env.binDir, "cortex"), "#!/bin/sh\necho \"cortex: open $HOME/.cortex/ca/ca.pem: permission denied\" >&2\nexit 1\n")
	_, _, err := serviceStep{}.apply(env, nil)
	var se stepError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v", err)
	}
	want := []string{"see ~/.cortex/proxy.log", "proxy.log    cortex: open ~/.cortex/ca/ca.pem: permission denied"}
	if se.reason != "the proxy exited immediately" || !slices.Equal(se.detail, want) {
		t.Errorf("failure = %q %q, want detail %q", se.reason, se.detail, want)
	}
}
