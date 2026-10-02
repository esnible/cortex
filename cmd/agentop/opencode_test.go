package main

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/peerproc"
)

// fakeOpenCode is what the three seams answer for one test, and a record of how they
// were called.
type fakeOpenCode struct {
	status    string // what `opencode service status` prints
	statusErr error
	pid       int32 // the listener's pid, unless listener is set
	listenErr error
	listener  func(netip.AddrPort) (int32, error) // overrides pid and listenErr
	env       []string
	envErr    error

	bins     []string // the binary each run was given
	runs     []string // each run's arguments, space-joined
	lookups  []netip.AddrPort
	environs []int32
}

// stubOpenCode points every seam at f for one test, and gives it a HOME of its own, so
// nothing a test does can run the real opencode or read the real user's files.
func stubOpenCode(t *testing.T, f *fakeOpenCode) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	run, listener, environ := openCodeRun, openCodeListener, openCodeEnviron
	t.Cleanup(func() { openCodeRun, openCodeListener, openCodeEnviron = run, listener, environ })
	openCodeRun = func(bin string, args ...string) (string, error) {
		f.bins = append(f.bins, bin)
		f.runs = append(f.runs, strings.Join(args, " "))
		if got := strings.Join(args, " "); got != "service status" {
			t.Errorf("ran opencode %s; the probe should only ask for the service status", got)
			return "", errors.New("unexpected command")
		}
		return f.status, f.statusErr
	}
	openCodeListener = func(addr netip.AddrPort) (int32, error) {
		f.lookups = append(f.lookups, addr)
		if f.listener != nil {
			return f.listener(addr)
		}
		return f.pid, f.listenErr
	}
	openCodeEnviron = func(pid int32) ([]string, error) {
		f.environs = append(f.environs, pid)
		return f.env, f.envErr
	}
}

// fakeOpenCodeBin puts an executable named opencode where findOpenCode's fallback
// looks, ~/.opencode/bin, and empties PATH, so findOpenCode succeeds without finding
// the real one. Nothing runs it: openCodeRun is stubbed.
func fakeOpenCodeBin(t *testing.T) string {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	dir := filepath.Join(os.Getenv("HOME"), ".opencode", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "opencode")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 99\n"), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	return bin
}

func TestProbeOpenCodeService_NotRunning(t *testing.T) {
	f := &fakeOpenCode{status: "stopped"}
	stubOpenCode(t, f)
	svc, err := probeOpenCodeService("/fake/opencode")
	if err != nil {
		t.Fatal(err)
	}
	if svc != (openCodeService{}) {
		t.Errorf("svc = %+v, want the zero value", svc)
	}
	if len(f.lookups) != 0 || len(f.environs) != 0 {
		t.Errorf("a stopped service was looked up: lookups %v, environs %v", f.lookups, f.environs)
	}
	if !slices.Equal(f.bins, []string{"/fake/opencode"}) {
		t.Errorf("ran %v, want the binary it was given", f.bins)
	}
}

func TestProbeOpenCodeService_Running(t *testing.T) {
	f := &fakeOpenCode{
		status: "http://127.0.0.1:49374",
		pid:    4242,
		env:    []string{"PATH=/usr/bin", "HTTPS_PROXY=http://127.0.0.1:47600", "https_proxy=http://other:1"},
	}
	stubOpenCode(t, f)
	svc, err := probeOpenCodeService("/fake/opencode")
	if err != nil {
		t.Fatal(err)
	}
	want := openCodeService{Running: true, Port: 49374, PID: 4242, Proxy: "http://127.0.0.1:47600"}
	if svc != want {
		t.Errorf("svc = %+v, want %+v", svc, want)
	}
	if want := []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:49374")}; !slices.Equal(f.lookups, want) {
		t.Errorf("lookups = %v, want %v", f.lookups, want)
	}
	if !slices.Equal(f.environs, []int32{4242}) {
		t.Errorf("environs = %v, want the listener's pid", f.environs)
	}
}

func TestProbeOpenCodeService_LowercaseProxyOnly(t *testing.T) {
	f := &fakeOpenCode{
		status: "http://127.0.0.1:49374",
		pid:    4242,
		env:    []string{"https_proxy=http://localhost:47600"},
	}
	stubOpenCode(t, f)
	svc, err := probeOpenCodeService("/fake/opencode")
	if err != nil {
		t.Fatal(err)
	}
	if svc.Proxy != "http://localhost:47600" {
		t.Errorf("Proxy = %q, want the lowercase https_proxy", svc.Proxy)
	}
}

func TestProbeOpenCodeService_NoProxyInAReadableEnvironment(t *testing.T) {
	f := &fakeOpenCode{status: "http://127.0.0.1:49374", pid: 4242, env: []string{"PATH=/usr/bin"}}
	stubOpenCode(t, f)
	svc, err := probeOpenCodeService("/fake/opencode")
	if err != nil {
		t.Fatal(err)
	}
	if svc.Proxy != "" || svc.EnvErr != nil {
		t.Errorf("svc = %+v, want no proxy and no error: the environment was read and lacks one", svc)
	}
}

// IPv6 only when IPv4 is not found: a listener bound to [::1] alone does not take
// connections to 127.0.0.1.
func TestProbeOpenCodeService_FallsBackToIPv6Loopback(t *testing.T) {
	f := &fakeOpenCode{
		status: "http://127.0.0.1:49374",
		listener: func(addr netip.AddrPort) (int32, error) {
			if addr.Addr().Is4() {
				return 0, peerproc.ErrNotFound
			}
			return 4343, nil
		},
		env: []string{"HTTPS_PROXY=http://[::1]:47600"},
	}
	stubOpenCode(t, f)
	svc, err := probeOpenCodeService("/fake/opencode")
	if err != nil {
		t.Fatal(err)
	}
	if svc.PID != 4343 || svc.EnvErr != nil {
		t.Errorf("svc = %+v, want pid 4343 from [::1]", svc)
	}
	want := []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:49374"), netip.MustParseAddrPort("[::1]:49374")}
	if !slices.Equal(f.lookups, want) {
		t.Errorf("lookups = %v, want %v", f.lookups, want)
	}
}

// Running but uninspectable is not an error: the service is there, and the caller
// says it could not check it. PID stays 0, so nothing reads as a process it named.
func TestProbeOpenCodeService_LookupFails(t *testing.T) {
	f := &fakeOpenCode{status: "http://127.0.0.1:49374", listenErr: peerproc.ErrNotFound}
	stubOpenCode(t, f)
	svc, err := probeOpenCodeService("/fake/opencode")
	if err != nil {
		t.Fatal(err)
	}
	if !svc.Running || svc.Port != 49374 || svc.PID != 0 || svc.EnvErr == nil {
		t.Errorf("svc = %+v, want running on 49374 with EnvErr and no pid", svc)
	}
	if !errors.Is(svc.EnvErr, peerproc.ErrNotFound) {
		t.Errorf("EnvErr = %v, want it to wrap the lookup's error", svc.EnvErr)
	}
	if len(f.environs) != 0 {
		t.Errorf("read an environment with no pid: %v", f.environs)
	}
}

func TestProbeOpenCodeService_EnvironFails(t *testing.T) {
	f := &fakeOpenCode{
		status: "http://127.0.0.1:49374",
		pid:    4242,
		envErr: fmt.Errorf("%w: pid 4242's environment is withheld", peerproc.ErrNotFound),
	}
	stubOpenCode(t, f)
	svc, err := probeOpenCodeService("/fake/opencode")
	if err != nil {
		t.Fatal(err)
	}
	if !svc.Running || svc.PID != 4242 || svc.Proxy != "" || svc.EnvErr == nil {
		t.Errorf("svc = %+v, want running, pid 4242, EnvErr set", svc)
	}
	if !errors.Is(svc.EnvErr, peerproc.ErrNotFound) {
		t.Errorf("EnvErr = %v, want it to wrap Environ's error", svc.EnvErr)
	}
}

// peerproc.Environ documents an empty result as unknown, not absent: a process that
// overwrote its argument memory shows nothing. Reading that as "no HTTPS_PROXY" would
// warn about a service that may well be using Cortex.
func TestProbeOpenCodeService_EmptyEnvironmentIsUnknown(t *testing.T) {
	f := &fakeOpenCode{status: "http://127.0.0.1:49374", pid: 4242, env: []string{}}
	stubOpenCode(t, f)
	svc, err := probeOpenCodeService("/fake/opencode")
	if err != nil {
		t.Fatal(err)
	}
	if svc.EnvErr == nil || svc.EnvErr.Error() != "its environment could not be read" {
		t.Errorf("EnvErr = %v, want %q", svc.EnvErr, "its environment could not be read")
	}
}

func TestProbeOpenCodeService_BadStatusOutput(t *testing.T) {
	for _, out := range []string{"http://127.0.0.1", "http://127.0.0.1:port", "http://127.0.0.1:0", "something else"} {
		t.Run(out, func(t *testing.T) {
			f := &fakeOpenCode{status: out}
			stubOpenCode(t, f)
			svc, err := probeOpenCodeService("/fake/opencode")
			if err == nil {
				t.Fatalf("svc = %+v, want an error", svc)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%q", out)) {
				t.Errorf("error %q does not name the output %q", err, out)
			}
			if len(f.lookups) != 0 {
				t.Errorf("looked up %v with no port", f.lookups)
			}
		})
	}
}

func TestProbeOpenCodeService_StatusFails(t *testing.T) {
	f := &fakeOpenCode{statusErr: errors.New("opencode service status: exit status 1")}
	stubOpenCode(t, f)
	if _, err := probeOpenCodeService("/fake/opencode"); err == nil {
		t.Fatal("want the CLI's error")
	}
}

func TestSameProxy(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"http://localhost:47600", "http://127.0.0.1:47600", true},
		{"127.0.0.1:47600", "http://[::1]:47600", true},
		{"http://LOCALHOST:47600", "http://localhost:47600", true},
		{"http://proxy.corp:3128", "http://PROXY.corp:3128", true},
		{"http://127.0.0.1:47600", "http://127.0.0.1:47601", false},
		{"http://proxy.corp:47600", "http://127.0.0.1:47600", false},
		{"", "http://127.0.0.1:47600", false},
		{"http://127.0.0.1:47600", "", false},
		{"", "", false},
	} {
		if got := sameProxy(tc.a, tc.b); got != tc.want {
			t.Errorf("sameProxy(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestFindOpenCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec bits")
	}
	t.Run("on PATH", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		fakeOpenCodeBin(t) // a fallback that must lose to PATH
		dir := t.TempDir()
		onPath := filepath.Join(dir, "opencode")
		if err := os.WriteFile(onPath, []byte("#!/bin/sh\nexit 99\n"), 0o755); err != nil { //nolint:gosec // test fixture
			t.Fatal(err)
		}
		t.Setenv("PATH", dir)
		got, err := findOpenCode()
		if err != nil || got != onPath {
			t.Errorf("findOpenCode() = %q, %v; want %q", got, err, onPath)
		}
	})
	t.Run("installer's directory", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		want := fakeOpenCodeBin(t)
		got, err := findOpenCode()
		if err != nil || got != want {
			t.Errorf("findOpenCode() = %q, %v; want %q", got, err, want)
		}
	})
	t.Run("not executable", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		bin := fakeOpenCodeBin(t)
		if err := os.Chmod(bin, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := findOpenCode()
		if err == nil || err.Error() != "opencode not found on PATH or in ~/.opencode/bin" {
			t.Errorf("err = %v, want the not-found error", err)
		}
	})
}

// The real runner, given /bin/sh rather than opencode: it trims stdout, and a failure
// carries the first line of stderr, which is where a CLI says what went wrong.
func TestOpenCodeRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	t.Setenv("HOME", t.TempDir())
	out, err := openCodeRun("/bin/sh", "-c", `printf '  http://127.0.0.1:49374 \n\n'`)
	if err != nil || out != "http://127.0.0.1:49374" {
		t.Errorf("openCodeRun = %q, %v; want the trimmed URL", out, err)
	}
	_, err = openCodeRun("/bin/sh", "-c", "echo 'Error: no service' >&2; echo second >&2; exit 3")
	if err == nil {
		t.Fatal("a non-zero exit returned no error")
	}
	// A suffix check: the script's own text, in the command named at the front, has
	// the second line in it too.
	if msg := err.Error(); !strings.HasSuffix(msg, ": exit status 3: Error: no service") {
		t.Errorf("err = %q, want it to end with the exit status and stderr's first line", msg)
	}
}

const openCodeWarning = "agentop: warning: OpenCode's background service (pid 4242) is not using Cortex.\n" +
	"  It sends all of OpenCode's traffic and keeps the environment it started with,\n" +
	"  so this session bypasses Cortex. To route it through Cortex for good:\n" +
	"    agentop configure opencode enable\n" +
	"    opencode service restart   # ends every OpenCode session using the service\n"

func TestWarnOpenCodeService(t *testing.T) {
	inject := map[string]string{"HTTPS_PROXY": "http://127.0.0.1:47600"}
	running := func(env []string, envErr error) *fakeOpenCode {
		return &fakeOpenCode{status: "http://127.0.0.1:49374", pid: 4242, env: env, envErr: envErr}
	}
	for _, tc := range []struct {
		name      string
		argv      []string
		fake      *fakeOpenCode
		noBinary  bool
		want      string
		wantProbe bool
	}{
		{name: "not opencode", argv: []string{"curl", "https://example.com"}, fake: running(nil, nil)},
		{name: "opencode not installed", argv: []string{"opencode"}, fake: running(nil, nil), noBinary: true},
		{name: "status fails", argv: []string{"opencode"}, fake: &fakeOpenCode{statusErr: errors.New("boom")}, wantProbe: true},
		{name: "stopped", argv: []string{"opencode"}, fake: &fakeOpenCode{status: "stopped"}, wantProbe: true},
		{
			name: "uses Cortex", argv: []string{"opencode"},
			fake: running([]string{"HTTPS_PROXY=http://localhost:47600"}, nil), wantProbe: true,
		},
		{
			name: "proxy unset", argv: []string{"opencode", "run", "hi"},
			fake: running([]string{"PATH=/usr/bin", "HOME=/Users/someone"}, nil), want: openCodeWarning, wantProbe: true,
		},
		{
			name: "another proxy", argv: []string{"/opt/homebrew/bin/opencode"},
			fake: running([]string{"HTTPS_PROXY=http://proxy.corp:3128"}, nil), want: openCodeWarning, wantProbe: true,
		},
		{
			name: "environment withheld", argv: []string{"opencode"},
			fake: running(nil, fmt.Errorf("%w: pid 4242's environment is withheld", peerproc.ErrNotFound)),
			want: "agentop: note: could not check OpenCode's background service " +
				"(reading the environment of pid 4242: peerproc: not found: pid 4242's environment is withheld).\n" +
				"  If it was started without Cortex, OpenCode's traffic bypasses Cortex.\n",
			wantProbe: true,
		},
		{
			name: "environment empty", argv: []string{"opencode"}, fake: running([]string{}, nil),
			want: "agentop: note: could not check OpenCode's background service (its environment could not be read).\n" +
				"  If it was started without Cortex, OpenCode's traffic bypasses Cortex.\n",
			wantProbe: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubOpenCode(t, tc.fake)
			if tc.noBinary {
				t.Setenv("PATH", t.TempDir())
			} else {
				fakeOpenCodeBin(t)
			}
			var stderr bytes.Buffer
			warnOpenCodeService(tc.argv, inject, &stderr)
			if got := stderr.String(); got != tc.want {
				t.Errorf("stderr =\n%s\nwant\n%s", got, tc.want)
			}
			if probed := len(tc.fake.runs) > 0; probed != tc.wantProbe {
				t.Errorf("probed = %v, want %v (runs %v)", probed, tc.wantProbe, tc.fake.runs)
			}
		})
	}
}

// Wired into runExec: the warning comes before the child, and the child still runs
// and decides the exit status. The opencode here is a script on PATH, not OpenCode.
func TestRunExec_WarnsAboutOpenCodeServiceAndStillRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	cfgPath, _ := execCfg(t)
	f := &fakeOpenCode{status: "http://127.0.0.1:49374", pid: 4242, env: []string{"PATH=/usr/bin"}}
	stubOpenCode(t, f)
	dir := t.TempDir()
	bin := filepath.Join(dir, "opencode")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho ran\nexit 7\n"), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var stdout, stderr bytes.Buffer
	code := runExec([]string{"--cortex-stats-url", execStats(t, cfgPath), "--", "opencode"}, &stdout, &stderr)
	if code != 7 {
		t.Errorf("exit %d, want the child's 7; stderr: %s", code, stderr.String())
	}
	if stdout.String() != "ran\n" {
		t.Errorf("stdout = %q, want the child's output", stdout.String())
	}
	if !strings.Contains(stderr.String(), openCodeWarning) {
		t.Errorf("stderr = %q, want the warning", stderr.String())
	}
	if !slices.Equal(f.bins, []string{bin}) {
		t.Errorf("probed %v, want the opencode on PATH", f.bins)
	}
}
