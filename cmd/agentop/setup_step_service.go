package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/checklist"
)

// serviceStep starts the proxy: under launchd or systemd through `agentop service
// install`'s runServiceInstall, or, where no supervisor can be used, as a
// background process in its own session.
type serviceStep struct{}

func (serviceStep) name() string { return "started" }

func (s serviceStep) plan(env *setupEnv) (stepPlan, *problem) {
	p := stepPlan{label: "started"}
	pidFile := filepath.Join(env.cortexDir, "proxy.pid")
	h, prob := s.preflightPorts(env)
	if prob != nil {
		return p, prob
	}
	env.unsupervised = env.opts.noService
	note := ""
	sup := strings.Fields(supervisorName(env.goos))[0]
	if !env.unsupervised && !setupSupervisorUsable(env.goos) {
		env.unsupervised = true
		note = " — " + sup + " can't be used here"
	}
	// Pins, not Bob's rate: a running proxy reloads pricing from the file itself.
	changing := len(env.binaryChanges) > 0 || env.configPinsPending || env.configFresh || env.opts.restart
	if env.unsupervised {
		// Ours, but not the background proxy proxy.pid records: a supervisor runs
		// this Cortex, and a second copy would only crash-loop on its ports.
		if h.known && supervisedHolder(pidFile, h.pid) {
			if !changing {
				p.done = true
				p.advice = &problem{reason: "Cortex is already running under " + sup + "; not starting a second copy"}
				return p, nil
			}
			if env.opts.noService {
				return p, &problem{reason: "a supervised Cortex is running here, and --no-service would start a second copy",
					fix: []string{"remove it first: agentop service uninstall", "or run agentop setup without --no-service"}}
			}
			return p, &problem{reason: "a supervised Cortex is running here, and this environment cannot manage " + sup,
				fix: []string{"run agentop setup from a normal terminal", "or stop it first: agentop service stop"}}
		}
		if _, running := proxyRunning(pidFile); running && !changing {
			p.done, p.doneMsg = true, "in the background (no supervisor)"
			return p, nil
		}
		p.verb, p.what = "run", "the cortex proxy"
		p.where = "unsupervised · won't restart after a reboot or a crash" + note
		return p, nil
	}
	if !env.configFresh {
		if sp, err := resolveServicePaths(env.configPath(), "", filepath.Join(env.binDir, "cortex")); err == nil {
			env.priorService = serviceInstalled(sp) && sp.healthURL != "" && waitHealthy(sp.healthURL, time.Second)
			// serviceIsCurrent only behind priorService: on a job that is not running it
			// polls the supervisor for its full readiness timeout.
			if env.priorService && !changing && serviceIsCurrent(sp) {
				p.done, p.doneMsg = true, supervisorName(env.goos)+" · healthy"
				return p, nil
			}
		}
	}
	p.verb, p.what, p.where = "run", "the cortex proxy", supervisorName(env.goos)+" · 127.0.0.1:47600"
	return p, nil
}

// forwardHold is the process holding the config's forward port, when lsof or ss
// can name it. Past preflightPorts, a known holder is ours.
type forwardHold struct {
	pid   int
	known bool
}

// preflightPorts is install.sh's port preflight for a fresh install, and its
// foreign-proxy check for an existing one — asked before consent here, rather than
// after a failed service install. The existing config's forward port is the one
// it names, so a user who moved the ports is checked on the new ones.
func (serviceStep) preflightPorts(env *setupEnv) (forwardHold, *problem) {
	if env.configFresh {
		var busy []string
		for _, port := range cortexPorts {
			if portInUse(port) {
				busy = append(busy, port)
			}
		}
		if len(busy) == 0 {
			return forwardHold{}, nil
		}
		reason := "port " + busy[0] + " is already in use by something else"
		if len(busy) > 1 {
			reason = "ports " + portList(busy) + " are already in use by something else"
		}
		return forwardHold{}, &problem{reason: reason,
			fix: []string{"free it, or change the ports in " + env.tilde(env.configPath()) + ", then re-run"}}
	}
	addr, port := forwardListener(env)
	pid, exe, ok := portHolder(port)
	if !ok {
		return forwardHold{}, nil
	}
	if ourProxy(env.binDir, filepath.Join(env.cortexDir, "proxy.pid"), pid, exe) {
		return forwardHold{pid: pid, known: true}, nil
	}
	fix := []string{fmt.Sprintf("kill %d", pid)}
	if env.goos == "darwin" {
		fix = append(fix, "or, if it is an older Cortex service: launchctl bootout gui/$(id -u)/"+launchdLabel)
	} else {
		fix = append(fix, "or, if it is an older Cortex service: systemctl --user disable --now "+systemdUnit)
	}
	return forwardHold{}, &problem{reason: fmt.Sprintf("%s is held by %s (pid %d), not this Cortex", addr, exe, pid), fix: fix}
}

// forwardListener is the existing config's forward proxy address and its port,
// or 127.0.0.1:47600 when the config does not give one that parses.
func forwardListener(env *setupEnv) (addr, port string) {
	sp, err := resolveServicePaths(env.configPath(), "", filepath.Join(env.binDir, "cortex"))
	if err == nil {
		if _, p, serr := net.SplitHostPort(sp.forwardAddr); serr == nil && p != "" {
			return sp.forwardAddr, p
		}
	}
	return "127.0.0.1:47600", "47600"
}

// supervisedHolder reports whether pid, our proxy on the forward port, runs under
// a supervisor rather than as the background proxy proxy.pid records. That proxy
// is cortex --supervise, whose child holds the port, so the pidfile names the
// holder or its parent. When ps cannot name the parent, a live pidfile proxy is
// taken to be it.
func supervisedHolder(pidFile string, pid int) bool {
	pf := readPIDFile(pidFile)
	if pf == pid {
		return false
	}
	if ppid, ok := parentPID(pid); ok {
		return ppid != pf
	}
	_, running := proxyRunning(pidFile)
	return !running
}

// serviceLoaded reports whether the supervisor has our job: in launchd's domain,
// running or not, or active or enabled under systemd. `agentop service stop`
// leaves neither; a crash-looping job is either. Unlike supervisorRunning it asks
// once, rather than waiting for the job to come up.
func serviceLoaded(goos string) bool {
	if goos == "darwin" {
		target := "gui/" + strconv.Itoa(os.Getuid()) + "/" + launchdLabel
		return exec.Command("launchctl", "print", target).Run() == nil
	}
	if exec.Command("systemctl", "--user", "is-active", "--quiet", systemdUnit).Run() == nil {
		return true
	}
	return exec.Command("systemctl", "--user", "is-enabled", "--quiet", systemdUnit).Run() == nil
}

// parentPID is ps's ppid for pid, and whether ps answered.
func parentPID(pid int) (int, bool) {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	return n, err == nil && n > 0
}

func (s serviceStep) apply(env *setupEnv, act *checklist.Running) (string, undo, error) {
	if env.unsupervised {
		return s.applyUnsupervised(env)
	}
	sp, err := resolveServicePaths(env.configPath(), "", filepath.Join(env.binDir, "cortex"))
	if err != nil {
		return "", undo{}, err
	}
	unitSnap, err := snapshotFile(sp.unitFile)
	if err != nil {
		return "", undo{}, err
	}
	stampSnap, err := snapshotFile(sp.stampFile)
	if err != nil {
		return "", undo{}, err
	}
	// A unit on disk whose job the supervisor does not have is what `agentop service
	// stop` leaves. The install below loads the job, so the undo stops it again; the
	// stop needs no binary, as the binaries undo runs after this one.
	stopped := unitSnap.existed && !env.priorService && !serviceLoaded(env.goos)
	u := undo{label: "the service", fn: func() error {
		if !unitSnap.existed {
			_ = unloadService(env.goos, sp) // best effort: a failed load may have left nothing
		}
		err := errors.Join(unitSnap.restore(), stampSnap.restore())
		if stopped {
			err = errors.Join(err, controlService(env.goos, "stop", sp, io.Discard))
		}
		return err
	}, manual: "agentop service uninstall"}
	switch {
	case stopped:
		u.manual = "agentop service stop"
	case unitSnap.existed:
		u.manual = "agentop service install --restart"
	}
	if env.priorService {
		env.priorDesc = "the previous Cortex is running again · healthy"
		env.restorePrior = func() error {
			if err := loadService(env.goos, sp, io.Discard); err != nil && !errors.Is(err, errLingerUnavailable) {
				return err
			}
			if !waitHealthy(sp.healthURL, serviceReadyTimeout) {
				return fmt.Errorf("it did not answer %s", sp.healthURL)
			}
			return nil
		}
	}
	if act != nil {
		act.Wait("the proxy to answer /healthz", serviceReadyTimeout)
	}
	var out, errb bytes.Buffer
	// The config step ran the pins migration, so runServiceInstall's own finds
	// nothing to do and would skip the restart that puts the new pins live.
	restart := env.opts.restart || env.configPinsChanged
	mark := markLog(sp.logFile)
	res := runServiceInstall(sp, adoptablePID(sp), restart, false, &out, &errb)
	if res.exit != 0 {
		// Read now, before the rollback brings the previous Cortex back to write
		// after them: these are the new version's lines.
		reason, detail := lastAgentopError(errb.String())
		for i, d := range detail {
			if d == "Last log lines:" { // runServiceInstall's own tail, of the whole log
				detail = detail[:i]
				break
			}
		}
		for i := range detail {
			detail[i] = env.tildeText(detail[i])
		}
		detail = append(detail, logTail(env, sp.logFile, mark)...)
		return "", u, stepError{reason: env.tildeText(reason), detail: detail}
	}
	detail := supervisorName(env.goos)
	switch {
	case res.healthy:
		detail += " · healthy on " + sp.forwardAddr
	case res.alreadyCurrent:
		detail += " · already running"
	default:
		detail += " · started"
	}
	return detail, u, nil
}

func (serviceStep) applyUnsupervised(env *setupEnv) (string, undo, error) {
	bin := filepath.Join(env.binDir, "cortex")
	pidFile := filepath.Join(env.cortexDir, "proxy.pid")
	health := resolveHealthURL(env.configPath())
	if old, running := proxyRunning(pidFile); running {
		if err := stopPID(old); err != nil {
			return "", undo{}, fmt.Errorf("could not stop the background proxy (pid %d): %w", old, err)
		}
		env.priorDesc = "the previous background Cortex is running again"
		env.restorePrior = func() error {
			_, _, err := startUnsupervised(bin, env.cortexDir, health)
			return err
		}
	}
	logPath := filepath.Join(env.cortexDir, "proxy.log")
	mark := markLog(logPath)
	pid, healthy, err := startUnsupervised(bin, env.cortexDir, health)
	var se stepError
	if errors.As(err, &se) {
		// The proxy exited: show what it wrote, as a supervised start's failure does.
		se.detail = append([]string{"see " + env.tilde(logPath)}, logTail(env, logPath, mark)...)
		err = se
	}
	u := undo{label: "the background proxy", fn: func() error {
		if alive(pid) {
			if err := stopPID(pid); err != nil {
				return err
			}
		}
		return removeIfExists(pidFile)
	}, manual: "kill $(cat " + env.tilde(pidFile) + ")"}
	if err != nil {
		return "", u, err
	}
	env.startedBackground = true
	if healthy {
		return "in the background · healthy", u, nil
	}
	return "in the background · not answering yet; see " + env.tilde(filepath.Join(env.cortexDir, "proxy.log")), u, nil
}

// logTailLines caps the proxy.log rows under a failed start.
const logTailLines = 5

// logMark is where the proxy log ended before a start, so that a failure shows
// what the start wrote rather than an older run's lines.
type logMark struct {
	fi   os.FileInfo // nil when there was no log
	size int64
}

func markLog(path string) logMark {
	fi, err := os.Stat(path)
	if err != nil {
		return logMark{}
	}
	return logMark{fi: fi, size: fi.Size()}
}

// logTail is the last logTailLines non-blank lines the log at path gained since
// m, as checklist detail rows: the first labelled proxy.log in the label column,
// HOME shown as ~. A log rotated or truncated since m is read whole. It is nil
// when the start wrote nothing or there is no log.
func logTail(env *setupEnv, path string, m logMark) []string {
	f, err := os.Open(path) //nolint:gosec // the service's own log
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err == nil && m.fi != nil && os.SameFile(fi, m.fi) && fi.Size() >= m.size {
		if _, err := f.Seek(m.size, io.SeekStart); err != nil {
			return nil
		}
	}
	var ring []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if l := strings.TrimRight(sc.Text(), " \t\r"); strings.TrimSpace(l) != "" {
			ring = append(ring, l)
			if len(ring) > logTailLines {
				ring = ring[1:]
			}
		}
	}
	var rows []string
	for i, l := range ring {
		label := "" // the checklist's label column is 12 wide
		if i == 0 {
			label = "proxy.log"
		}
		rows = append(rows, fmt.Sprintf("%-12s %s", label, env.tildeText(l)))
	}
	return rows
}

// lastAgentopError picks runServiceInstall's failure out of what it wrote to
// stderr: the last "agentop: " line is the reason, the lines after it the detail.
func lastAgentopError(s string) (string, []string) {
	lines := tailLines(s, 1<<10)
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(lines[i], "agentop: ") {
			detail := lines[i+1:]
			if len(detail) > 6 {
				detail = detail[:6]
			}
			return strings.TrimPrefix(lines[i], "agentop: "), detail
		}
	}
	if len(lines) > 0 {
		return lines[len(lines)-1], nil
	}
	return "the service did not start", nil
}
