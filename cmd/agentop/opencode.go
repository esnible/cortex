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
	"slices"
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
	// agentop's own proxy and CA variables do not reach OpenCode's CLI. The CLI talks to its
	// service over loopback through any proxy it inherits, and a service it starts or restarts
	// inherits them as well, so after disable a restart from a shell with HTTPS_PROXY set would
	// leave the service on Cortex.
	cmd.Env = openCodeCLIEnv(os.Environ())
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

// openCodeCLIEnv is env, a list of "NAME=value" strings, without the nine variables
// enable manages (openCodeKeys).
func openCodeCLIEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); slices.Contains(openCodeKeys, k) {
			continue
		}
		out = append(out, kv)
	}
	return out
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
	Proxy   string // the proxy Bun reads from its environment (see environProxy); "" when none
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
	// the second lookup adds is a service bound to IPv6 alone.
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

// environProxy is the HTTPS proxy in env, a list of "NAME=value" strings, as OpenCode
// would use it: the first non-empty value of https_proxy, then HTTPS_PROXY, or "" when
// neither has one.
//
// That is Bun's order, and OpenCode runs on Bun: its env loader reads the lowercase name
// first and treats an empty value as unset. Reading the names the other way would name a
// proxy the service does not use whenever the two disagree.
func environProxy(env []string) string {
	for _, name := range []string{"https_proxy", "HTTPS_PROXY"} {
		for _, kv := range env {
			if k, v, ok := strings.Cut(kv, "="); ok && k == name {
				if v != "" {
					return v
				}
				break
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
	// Without this, url.Parse rejects "127.0.0.1:47600" and reads "localhost:47600" as
	// scheme "localhost".
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
// child starts it with exec's environment, under whatever service.json's env sets.
// `opencode service …` commands are skipped: the user is already managing the service,
// and the warning would be noise. `opencode serve` is skipped too: it is a foreground
// server that runs under exec's own environment. Only a command whose own name is
// opencode is recognised, so one run through a wrapper such as `env opencode` or an
// alias gets no check.
//
// The warning names the fix. When the service environment already holds every value
// exec sets (openCodeEnvHolds), only the running service predates it, and restarting
// it is enough; otherwise, including when that environment cannot be read, it names
// configure opencode enable.
func warnOpenCodeService(cmdArgs []string, inject map[string]string, stderr io.Writer) {
	if len(cmdArgs) == 0 || filepath.Base(cmdArgs[0]) != "opencode" {
		return
	}
	if len(cmdArgs) > 1 && (cmdArgs[1] == "service" || cmdArgs[1] == "serve") {
		return
	}
	// The binary the child will run, found as runChild finds it: a path is used as
	// given, a bare name is searched for on PATH. When that fails, findOpenCode tries
	// PATH and then the installer's directory.
	bin, err := exec.LookPath(cmdArgs[0])
	if err != nil {
		if bin, err = findOpenCode(); err != nil {
			return
		}
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
	if sameProxy(svc.Proxy, inject[envProxy]) {
		return
	}
	if openCodeEnvHolds(bin, inject) {
		fmt.Fprintf(stderr, "agentop: warning: OpenCode's background service (pid %d) is not using Cortex.\n"+
			"  It sends all of OpenCode's traffic and keeps the environment it started with. Its\n"+
			"  configuration already routes it through Cortex, so restarting it is enough:\n"+
			"    opencode service restart   # interrupts every OpenCode session using the service\n", svc.PID)
		return
	}
	fmt.Fprintf(stderr, "agentop: warning: OpenCode's background service (pid %d) is not using Cortex.\n"+
		"  It sends all of OpenCode's traffic and keeps the environment it started with,\n"+
		"  so this session bypasses Cortex. To route it through Cortex for good:\n"+
		"    agentop configure opencode enable   # restarts the service, interrupting every OpenCode session using it\n", svc.PID)
}

// openCodeEnvHolds reports whether OpenCode's service environment, as `opencode service
// get env` prints it, already holds every value in inject: a proxy variable a proxy on
// the same listener (sameProxy), any other the same text. False when that environment
// cannot be read.
func openCodeEnvHolds(bin string, inject map[string]string) bool {
	env, err := openCodeServiceEnv(bin)
	if err != nil {
		return false
	}
	for k, want := range inject {
		cur, ok := env[k]
		switch {
		case !ok:
			return false
		case openCodeCanonicalKey(k) == envProxy:
			if !sameProxy(cur, want) {
				return false
			}
		case cur != want:
			return false
		}
	}
	return true
}
