package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/peerproc"
)

// OpenCode does not send its traffic from the process the user runs. The first client
// spawns a background service, `opencode serve --service`, one per user on a fixed
// port, and every later client reuses it. The service outlives the client that started
// it and keeps the environment it started with, so `agentop exec -- opencode` changes
// nothing for a service that was already running without Cortex. What follows finds
// that service and reads its environment, so agentop can say so instead of letting the
// session bypass Cortex in silence.

// openCodeTimeout bounds one opencode CLI call. It runs in front of the user's command,
// so a CLI that hangs must not hang agentop with it.
const openCodeTimeout = 10 * time.Second

// openCodeRun runs the opencode CLI and returns its stdout with surrounding whitespace
// trimmed. A package variable so tests replace it; nothing in a test may run the real binary.
var openCodeRun = func(bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), openCodeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // bin is the opencode CLI, found or named by the caller
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// Bounds how long Wait waits for the output pipes once the CLI has exited or been
	// killed: a process it started could otherwise hold them open indefinitely.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		what := filepath.Base(bin) + " " + strings.Join(args, " ")
		if ctx.Err() != nil {
			return "", fmt.Errorf("%s: no answer in %s", what, openCodeTimeout)
		}
		// Only stderr's first line, so the error stays one line.
		if line, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n"); line != "" {
			return "", fmt.Errorf("%s: %w: %s", what, err, strings.TrimSpace(line))
		}
		return "", fmt.Errorf("%s: %w", what, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// findOpenCode is the opencode binary: on PATH, else ~/.opencode/bin/opencode, where
// OpenCode's installer puts it.
func findOpenCode() (string, error) {
	if p, err := exec.LookPath("opencode"); err == nil {
		return p, nil
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		p := filepath.Join(home, ".opencode", "bin", "opencode")
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p, nil
		}
	}
	return "", errors.New("opencode not found on PATH or in ~/.opencode/bin")
}

// openCodeService is what agentop can learn about OpenCode's running background service.
type openCodeService struct {
	Running bool   // `opencode service status` named a URL rather than "stopped"
	Port    int    // its port, when Running
	PID     int32  // its process, 0 when the lookup failed
	Proxy   string // HTTPS_PROXY (else https_proxy) in its environment; "" when unset
	EnvErr  error  // why PID or Proxy could not be read; nil when both were
}

// openCodeListener and openCodeEnviron are the probe's process lookups, package variables
// so tests replace them.
var openCodeListener = func(addr netip.AddrPort) (int32, error) {
	r, err := peerproc.New()
	if err != nil {
		return 0, err
	}
	p, err := r.ListenerOwner(addr)
	if err != nil {
		return 0, err
	}
	return p.PID, nil
}

var openCodeEnviron = peerproc.Environ

// probeOpenCodeService asks the CLI whether the service runs and on which port, then
// names its process and reads its environment.
//
// Only the CLI's answer can fail the probe. A service that is running but cannot be
// inspected comes back with EnvErr set and a nil error, because it is still running and
// the caller should say it could not check rather than nothing.
func probeOpenCodeService(bin string) (openCodeService, error) {
	out, err := openCodeRun(bin, "service", "status")
	if err != nil {
		return openCodeService{}, err
	}
	if out == "stopped" {
		return openCodeService{}, nil
	}
	port, err := openCodeStatusPort(out)
	if err != nil {
		return openCodeService{}, err
	}
	svc := openCodeService{Running: true, Port: port}

	// 127.0.0.1 first, and [::1] only when nothing is found there: ListenerOwner
	// already counts a wildcard or dual-stack listener for an IPv4 address, so what
	// the second lookup adds is a service bound to [::1] alone.
	pid, err := openCodeListener(netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), uint16(port)))
	if errors.Is(err, peerproc.ErrNotFound) {
		pid, err = openCodeListener(netip.AddrPortFrom(netip.IPv6Loopback(), uint16(port)))
	}
	if err != nil {
		svc.EnvErr = fmt.Errorf("finding the process listening on port %d: %w", port, err)
		return svc, nil
	}
	svc.PID = pid

	env, err := openCodeEnviron(pid)
	switch {
	case err != nil:
		svc.EnvErr = fmt.Errorf("reading the environment of pid %d: %w", pid, err)
	case len(env) == 0:
		// Unknown, not absent: peerproc.Environ shows nothing for a process that wrote
		// over its own argument memory. Reading that as "no proxy" would warn about a
		// service that may well be using Cortex.
		svc.EnvErr = errors.New("its environment could not be read")
	default:
		svc.Proxy = environProxy(env)
	}
	return svc, nil
}

// openCodeStatusPort is the port in the URL `opencode service status` prints for a
// running service, such as http://127.0.0.1:49374.
func openCodeStatusPort(out string) (int, error) {
	u, err := url.Parse(out)
	if err == nil {
		if port, perr := strconv.ParseUint(u.Port(), 10, 16); perr == nil && port != 0 {
			return int(port), nil
		}
	}
	return 0, fmt.Errorf("opencode service status printed %q, which is neither \"stopped\" nor a URL with a port", out)
}

// environProxy is the value of the first of HTTPS_PROXY and https_proxy present in env,
// a list of "NAME=value" strings, or "" when neither is.
func environProxy(env []string) string {
	for _, name := range []string{"HTTPS_PROXY", "https_proxy"} {
		for _, kv := range env {
			if k, v, ok := strings.Cut(kv, "="); ok && k == name {
				return v
			}
		}
	}
	return ""
}

// sameProxy reports whether two proxy URLs name the same listener, treating localhost,
// 127.0.0.1 and ::1 as one host.
//
// A value with no scheme is read as http://, the scheme Cortex's proxy speaks. Two empty
// values are not the same proxy: an unset variable names no listener at all.
func sameProxy(a, b string) bool {
	ua, ub := parseProxyURL(a), parseProxyURL(b)
	if ua == nil || ub == nil {
		return false
	}
	return ua.Port() == ub.Port() && bobSameLoopback(ua.Hostname(), ub.Hostname())
}

// parseProxyURL is s as a URL, or nil when it is empty or names no host.
func parseProxyURL(s string) *url.URL {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	// Without this, url.Parse reads the host of "127.0.0.1:47600" as a scheme.
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" {
		return nil
	}
	return u
}

// warnOpenCodeService tells the user, before `agentop exec -- opencode` starts the
// child, when OpenCode's background service is running without Cortex's proxy.
//
// It only ever writes to stderr. The child runs whatever it finds, and nothing here
// can fail the command: an opencode agentop cannot find, or a status it cannot read,
// just means no warning. A service that is not running needs none either, because the
// child starts it with the environment exec gives it.
func warnOpenCodeService(cmdArgs []string, inject map[string]string, stderr io.Writer) {
	if len(cmdArgs) == 0 || filepath.Base(cmdArgs[0]) != "opencode" {
		return
	}
	bin, err := findOpenCode()
	if err != nil {
		return
	}
	svc, err := probeOpenCodeService(bin)
	if err != nil || !svc.Running {
		return
	}
	if svc.EnvErr != nil {
		fmt.Fprintf(stderr, "agentop: note: could not check OpenCode's background service (%v).\n"+
			"  If it was started without Cortex, OpenCode's traffic bypasses Cortex.\n", svc.EnvErr)
		return
	}
	if !sameProxy(svc.Proxy, inject[envProxy]) {
		fmt.Fprintf(stderr, "agentop: warning: OpenCode's background service (pid %d) is not using Cortex.\n"+
			"  It sends all of OpenCode's traffic and keeps the environment it started with,\n"+
			"  so this session bypasses Cortex. To route it through Cortex for good:\n"+
			"    agentop configure opencode enable\n"+
			"    opencode service restart   # ends every OpenCode session using the service\n", svc.PID)
	}
}
