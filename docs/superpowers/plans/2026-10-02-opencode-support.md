# OpenCode Support Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make OpenCode a supported agent. Cortex recognises its User-Agent and reads its session header. `agentop configure opencode` routes it through Cortex persistently. `agentop exec -- opencode` warns when OpenCode's background service is running without Cortex. A doc page says what all of this does.

**Architecture:**
- OpenCode runs as TUI/`run` clients plus one background service per user, and that service sends all of OpenCode's outbound traffic.
- Cortex recognises the shared User-Agent and the service's `X-Opencode-Session-Id`. PR 3's per-process attribution then files the service's traffic under its sessions and suppresses the TUI's loopback polling.
- Persistence goes through the service's own environment (`opencode service set env`), which the service prefers over what it inherited.
- The stale-service check finds the service's PID by its port with `peerproc.ListenerOwner`, then reads its environment with a new `peerproc.Environ`.

**Tech Stack:** Go 1.26.5, `golang.org/x/sys/unix` (already a direct dependency of core), the `opencode` CLI (2.0.21).

**Spec:** `docs/superpowers/specs/2026-10-01-per-process-session-attribution-design.md` — section "OpenCode support (PR 4)" and "Decisions recorded".

## Global Constraints

- Work only in `.worktrees/opencode`, branch `feat/opencode-support`. It is stacked on `feat/process-attribution` (PR #1230, head 149f523c). Never use the top-level checkout or another worktree.
- **Commits:**
  - Every commit uses `git commit -s`, and its message ends with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. Never add `Co-Authored-By`.
  - Stage by explicit path only: `.superpowers/` is un-ignored scratch and must never be committed.
- **Formatting and vet:**
  - `gofmt -l` on every package you touched prints nothing. Always give gofmt a path.
  - `go vet ./...` passes in `core/`.
  - `GOWORK=off go vet ./...` passes in `cmd/agentop/`.
  - `go mod tidy -diff` prints nothing in `core/`, `cmd/agentop/` and `cmd/cortex/`.
- **Never touch the running Cortex or the user's OpenCode:**
  - Never start, stop or restart the shared Cortex (`:47600`–`:47604`) or the user's OpenCode service (port 49374).
  - Never edit `~/.cortex/config.yaml` or the user's `~/.config/opencode/service.json`.
  - Tests must not run the real `opencode` binary or write under the real `$HOME`. Use the seams this plan defines, and `t.Setenv("HOME", t.TempDir())`.
- **Never restart OpenCode's service from agentop.** A restart cuts every OpenCode session using it, so agentop says so and names `opencode service restart` instead.
- **Run tests this way:**
  - core: from `core/` (workspace mode).
  - agentop: `cd cmd/agentop && env -u SSL_CERT_FILE GOWORK=off go test ./...`. The user's shell exports `SSL_CERT_FILE`, and one existing exec test fails with it set.
  - cortex: `cd cmd/cortex && GOWORK=off go test -tags "$(go -C ../../scripts/profile-tags run . full)" ./...`.

---

### Task 1: Recognise OpenCode's User-Agent and session header

**Files:**
- Modify: `core/pipeline/client.go` — `knownClients` gains `opencode`; new `productVersion`; `ParseUserAgent` and `trailingKnownClient` use it; the godoc sentence that says OpenCode is not yet detected
- Modify: `core/pipeline/client_test.go`
- Modify: `core/session/idheader.go` — `OpenCodeSessionHeader`
- Modify: `core/session/idheader_test.go`
- Modify: `core/config/config.go` — `SessionIDHeaders()` default; the `IDHeaders` field doc if it lists the defaults
- Modify: `core/config/config_test.go`

**Interfaces:**
- Produces:
  - `session.OpenCodeSessionHeader = "X-Opencode-Session-Id"`.
  - `pipeline.ParseUserAgent("opencode/latest/2.0.21/cli")` → `Name "opencode"`, `Version "2.0.21"`.
  - `IsKnownAgent("opencode") == true`.
  - `config.SessionConfig{}.SessionIDHeaders()` → `[X-Claude-Code-Session-Id, X-Task-Id, X-Opencode-Session-Id]`.

- [ ] **Step 1: Write the failing tests**

Append to `core/pipeline/client_test.go`:

```go
// OpenCode sends one User-Agent from its TUI, its background service and its inference
// client alike, with a release channel before the version. Captured from OpenCode 2.0.21.
func TestParseUserAgent_OpenCodeVersionIsTheFieldAfterItsChannel(t *testing.T) {
	for _, tc := range []struct{ ua, version string }{
		{"opencode/latest/2.0.21/cli", "2.0.21"},
		{"opencode/beta/2.1.0-beta.3/tui", "2.1.0-beta.3"},
		{"opencode/2.0.21", "2.0.21"},
	} {
		got := ParseUserAgent(tc.ua)
		if got == nil || got.Name != "opencode" || got.Version != tc.version {
			t.Errorf("ParseUserAgent(%q) = %+v, want opencode %s", tc.ua, got, tc.version)
		}
	}
}

// Every other agent's version is all of what follows its product token's slash.
func TestParseUserAgent_OnlyOpenCodeHasAChannelField(t *testing.T) {
	if got := ParseUserAgent("bob-shell/2.0.5/extra"); got.Version != "2.0.5/extra" {
		t.Errorf("bob-shell version = %q, want the whole rest", got.Version)
	}
}
```

In `TestIsKnownAgent`'s table, after `{"ibm-bob", true},`, add `{"opencode", true},`.

In `core/session/idheader_test.go`, find the test that checks `BobSessionHeader` is in canonical form, near line 199. Add the same check for `OpenCodeSessionHeader` right after it, with the same shape and `http.CanonicalHeaderKey`.

In `core/config/config_test.go`'s `TestSessionConfig_SessionIDHeaders`, change the first row's want to `[]string{session.ClaudeCodeSessionHeader, session.BobSessionHeader, session.OpenCodeSessionHeader}`.

- [ ] **Step 2: Run them to verify they fail**

Run: `cd core && go test ./pipeline ./session ./config -run 'OpenCode|IsKnownAgent|SessionIDHeaders|Canonical'`
Expected: build failure `undefined: session.OpenCodeSessionHeader`. Once the constant exists, the pipeline and config tests fail on the values.

- [ ] **Step 3: Implement**

`core/session/idheader.go`, after `BobSessionHeader`:

```go
// OpenCodeSessionHeader is the request header OpenCode sets on its inference requests to
// name the session they belong to: its "ses_…" id, the same one in the TUI's URLs. Sent by
// OpenCode's background service from inside the TLS the bridge terminates, which is what
// makes the service the session's claimer (see Store.ClaimProcess). OpenCode sends the same
// value as X-Session-Id too; that name is generic, so this one is read. Verified against
// OpenCode 2.0.21.
const OpenCodeSessionHeader = "X-Opencode-Session-Id"
```

`core/config/config.go`, in `SessionIDHeaders()`:

```go
		return []string{session.ClaudeCodeSessionHeader, session.BobSessionHeader, session.OpenCodeSessionHeader}
```

Also check the `IDHeaders` field's doc comment in the same file (`grep -n "IDHeaders" core/config/config.go`). If it names the default headers, add `X-Opencode-Session-Id` to that list.

`core/pipeline/client.go`:

1. In the `knownClients` godoc, replace `tool-prune analysis only works for it — and OpenCode, Codex and the rest arrive` with `tool-prune analysis only works for it — and Codex and the rest arrive` (OpenCode now has its own detection work, below).
2. In the `knownClients` map, after the `"bob": "ibm-bob",` entry, add:

```go
	// OpenCode's one User-Agent, sent by its TUI, its background service and its inference
	// client alike: "opencode/<channel>/<version>/<client>", e.g. "opencode/latest/2.0.21/cli"
	// (captured from 2.0.21). The version is the field after the channel, not the first one —
	// see productVersion. It arrives with its own detection work: its session header is
	// session.OpenCodeSessionHeader, and `agentop configure opencode` sets it up.
	"opencode": "opencode",
```

3. Add, directly above `func ParseUserAgent`:

```go
// productVersion is the version in rest, what follows an agent's product token and its
// slash. For every agent but OpenCode that is all of rest. OpenCode puts a release channel
// first — "opencode/latest/2.0.21/cli" — so its version is the second field, and taking all
// of rest would label each release with its channel and client name too.
func productVersion(name, rest string) string {
	if name != "opencode" {
		return rest
	}
	if fields := strings.Split(rest, "/"); len(fields) >= 2 {
		return fields[1]
	}
	return rest
}
```

4. In `ParseUserAgent`, in the first-rule branch, replace `c.Version = version` with `c.Version = productVersion(name, version)`. In `trailingKnownClient`, replace `return n, ver, true` with `return n, productVersion(n, ver), true`.

- [ ] **Step 4: Run the tests**

Run: `cd core && go test -race ./pipeline ./session ./config ./listener/forwardproxy ./cost/...`
Expected: `ok`. If an existing test pins the default `id_headers` list or the known-agent set somewhere else, update it to include OpenCode and say so in the report.

- [ ] **Step 5: Commit**

```bash
gofmt -l core/pipeline core/session core/config   # must print nothing
git add core/pipeline/client.go core/pipeline/client_test.go core/session/idheader.go core/session/idheader_test.go core/config/config.go core/config/config_test.go
git commit -s -F - <<'EOF'
feat: Recognise OpenCode's User-Agent and read its session header

OpenCode sends "opencode/<channel>/<version>/<client>" from its TUI,
its background service and its inference client; it now parses as
agent opencode, version the field after the channel. Its inference
requests carry X-Opencode-Session-Id, which joins the default
session.id_headers, so its rows land in its own sessions.

Part of #941.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
EOF
```

---

### Task 2: `peerproc.Environ` — the environment a process started with

**Files:**
- Modify: `core/peerproc/peerproc.go` — exported `Environ`
- Modify: `core/peerproc/darwin.go` — `environ`, `parseProcArgs2`
- Modify: `core/peerproc/linux.go` — `environ`, `splitNUL`
- Modify: `core/peerproc/other.go` — `environ` stub
- Modify: `core/peerproc/peerproc_test.go`, `core/peerproc/darwin_test.go`, `core/peerproc/linux_test.go`

**Interfaces:**
- Produces: `func Environ(pid int32) ([]string, error)`. It returns `"NAME=value"` strings, the environment the process was exec'd with. Another user's process returns an error. Platforms other than darwin and linux return `ErrUnsupported`.

- [ ] **Step 1: Write the failing tests**

Append to `core/peerproc/peerproc_test.go`. Add any imports it lacks (`io`, `os`, `os/exec`, `runtime`, `slices`):

```go
// Environ reads a child's environment back from the kernel. The child is this test binary
// re-run as TestEnvironHelperProcess, not a system binary, which macOS can refuse to show.
func TestEnviron_ReadsAChildsEnvironment(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("Environ is unsupported on " + runtime.GOOS)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestEnvironHelperProcess$")
	cmd.Env = append(os.Environ(), "PEERPROC_ENVIRON_HELPER=1", "PEERPROC_MARKER=a=b c")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close(); _ = cmd.Wait() }()

	env, err := Environ(int32(cmd.Process.Pid))
	if err != nil {
		t.Fatalf("Environ(child): %v", err)
	}
	if !slices.Contains(env, "PEERPROC_MARKER=a=b c") {
		t.Errorf("child's environment lacks the marker; got %d entries", len(env))
	}
}

// TestEnvironHelperProcess is the child TestEnviron_ReadsAChildsEnvironment reads: it waits
// for its stdin to close, so it is alive while the test reads it.
func TestEnvironHelperProcess(t *testing.T) {
	if os.Getenv("PEERPROC_ENVIRON_HELPER") != "1" {
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// Another user's process — pid 1 belongs to root — cannot be read.
func TestEnviron_RefusesAnotherUsersProcess(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("Environ is unsupported on " + runtime.GOOS)
	}
	if os.Geteuid() == 0 {
		t.Skip("root can read every process")
	}
	if _, err := Environ(1); err == nil {
		t.Error("Environ(1) succeeded for a non-root caller")
	}
}
```

Append to `core/peerproc/darwin_test.go`:

```go
func TestParseProcArgs2(t *testing.T) {
	var buf []byte
	buf = binary.LittleEndian.AppendUint32(buf, 2) // argc
	buf = append(buf, "/usr/local/bin/x\x00\x00\x00"...)
	buf = append(buf, "x\x00-v\x00"...)
	buf = append(buf, "A=1\x00B=two=2\x00"...)
	buf = append(buf, "\x00ptr_munge=\x00main_stack=\x00"...) // Apple's strings, after an empty one
	env, err := parseProcArgs2(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(env, "|"); got != "A=1|B=two=2" {
		t.Errorf("env = %q, want A=1|B=two=2", got)
	}
	for _, bad := range [][]byte{nil, {1, 0, 0}, binary.LittleEndian.AppendUint32(nil, 3)} {
		if _, err := parseProcArgs2(bad); err == nil {
			t.Errorf("parseProcArgs2(%v) accepted a truncated buffer", bad)
		}
	}
}
```

(Add `encoding/binary` and `strings` to darwin_test.go's imports if missing.)

Append to `core/peerproc/linux_test.go`:

```go
func TestSplitNUL(t *testing.T) {
	got := splitNUL([]byte("A=1\x00B=two=2\x00\x00"))
	if strings.Join(got, "|") != "A=1|B=two=2" {
		t.Errorf("splitNUL = %q", got)
	}
	if got := splitNUL(nil); len(got) != 0 {
		t.Errorf("splitNUL(nil) = %q", got)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `cd core && go test ./peerproc -run 'Environ|ProcArgs2|SplitNUL'`
Expected: build failure, `undefined: Environ` / `parseProcArgs2`.

- [ ] **Step 3: Implement**

`core/peerproc/peerproc.go`, after the `Resolver` interface:

```go
// Environ is the environment process pid was exec'd with, as "NAME=value" strings. It is
// the start-time environment, not any change the process has made to its own since. Only
// this user's processes can be read; another user's answers an error. ErrUnsupported on
// platforms with no implementation.
func Environ(pid int32) ([]string, error) { return environ(pid) }
```

`core/peerproc/darwin.go` (add `errors` to its imports):

```go
func environ(pid int32) ([]string, error) {
	buf, err := unix.SysctlRaw("kern.procargs2", int(pid))
	if err != nil {
		return nil, fmt.Errorf("peerproc: kern.procargs2 for pid %d: %w", pid, err)
	}
	return parseProcArgs2(buf)
}

// parseProcArgs2 reads the environment out of a kern.procargs2 buffer. The layout is argc
// as a 32-bit integer, the executable path, NUL padding, argc NUL-terminated arguments,
// then the NUL-terminated environment, which ends at an empty string. Apple's own strings
// follow that empty one and are not the environment.
func parseProcArgs2(buf []byte) ([]string, error) {
	if len(buf) < 4 {
		return nil, fmt.Errorf("peerproc: kern.procargs2 is %d bytes", len(buf))
	}
	argc := int(binary.LittleEndian.Uint32(buf[:4]))
	rest := buf[4:]
	i := bytes.IndexByte(rest, 0)
	if i < 0 {
		return nil, errors.New("peerproc: kern.procargs2 has no executable path")
	}
	rest = rest[i:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	for n := 0; n < argc; n++ {
		i := bytes.IndexByte(rest, 0)
		if i < 0 {
			return nil, errors.New("peerproc: kern.procargs2 ends inside its arguments")
		}
		rest = rest[i+1:]
	}
	var env []string
	for {
		i := bytes.IndexByte(rest, 0)
		if i <= 0 {
			return env, nil
		}
		env = append(env, string(rest[:i]))
		rest = rest[i+1:]
	}
}
```

`core/peerproc/linux.go` (add `bytes` to its imports if missing):

```go
func environ(pid int32) ([]string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(int(pid)) + "/environ")
	if err != nil {
		return nil, fmt.Errorf("peerproc: %w", err)
	}
	return splitNUL(b), nil
}

// splitNUL splits /proc/<pid>/environ's NUL-terminated strings, dropping empty ones.
func splitNUL(b []byte) []string {
	var out []string
	for _, f := range bytes.Split(b, []byte{0}) {
		if len(f) > 0 {
			out = append(out, string(f))
		}
	}
	return out
}
```

`core/peerproc/other.go`, append:

```go
func environ(int32) ([]string, error) { return nil, ErrUnsupported }
```

- [ ] **Step 4: Run the tests**

```bash
cd core && go test -race ./peerproc && GOOS=windows go vet ./peerproc && GOOS=freebsd go build ./peerproc
podman run --rm --init -v "$PWD/..":/src:ro -w /src/core -e GOFLAGS=-buildvcs=false -e GOWORK=off docker.io/library/golang:1.26 go test -race -run 'Environ|SplitNUL' ./peerproc/
```

Expected: every command succeeds. If `podman info` fails, record that and skip only the podman command.

- [ ] **Step 5: Commit**

```bash
gofmt -l core/peerproc   # must print nothing
git add core/peerproc/peerproc.go core/peerproc/darwin.go core/peerproc/linux.go core/peerproc/other.go core/peerproc/peerproc_test.go core/peerproc/darwin_test.go core/peerproc/linux_test.go
git commit -s -F - <<'EOF'
feat: Add peerproc.Environ, the environment a process started with

Reads kern.procargs2 on macOS and /proc/<pid>/environ on Linux, for
this user's processes only. agentop uses it to tell whether OpenCode's
background service, which sends all of OpenCode's traffic, was started
with Cortex's proxy.

Part of #941.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
EOF
```

---

### Task 3: Find OpenCode's service and warn under `agentop exec` when it bypasses Cortex

**Files:**
- Create: `cmd/agentop/opencode.go`
- Create: `cmd/agentop/opencode_test.go`
- Modify: `cmd/agentop/cmd_exec.go` — one call in `runExec`, before `runChild`
- Modify: `cmd/agentop/cmd_exec_test.go` — only if an existing test needs the new seam stubbed

**Interfaces:**
- Consumes: `peerproc.New()`, `(peerproc.Resolver).ListenerOwner(netip.AddrPort, ...int32)`, `peerproc.Environ(int32)` (Task 2).
- Produces, used by Task 4:

```go
// openCodeRun runs the opencode CLI and returns its stdout with surrounding whitespace
// trimmed. A package variable so tests replace it; nothing in a test may run the real binary.
var openCodeRun = func(bin string, args ...string) (string, error)

// findOpenCode is the opencode binary: on PATH, else ~/.opencode/bin/opencode, where
// OpenCode's installer puts it.
func findOpenCode() (string, error)

// openCodeService is what agentop can learn about OpenCode's running background service.
type openCodeService struct {
	Running bool   // `opencode service status` named a URL rather than "stopped"
	Port    int    // its port, when Running
	PID     int32  // its process, 0 when the lookup failed
	Proxy   string // HTTPS_PROXY (else https_proxy) in its environment; "" when unset
	EnvErr  error  // why PID or Proxy could not be read; nil when both were
}

// probeOpenCodeService asks the CLI whether the service runs and on which port, then
// names its process and reads its environment.
func probeOpenCodeService(bin string) (openCodeService, error)

// openCodeListener and openCodeEnviron are the probe's process lookups, package variables
// so tests replace them.
var openCodeListener = func(addr netip.AddrPort) (int32, error) // peerproc.New() then ListenerOwner
var openCodeEnviron = peerproc.Environ

// sameProxy reports whether two proxy URLs name the same listener, treating localhost,
// 127.0.0.1 and ::1 as one host.
func sameProxy(a, b string) bool
```

**Behaviour, exactly:**

1. `openCodeRun` runs `exec.CommandContext(ctx, bin, args...)` with a 10-second timeout. It returns `strings.TrimSpace(stdout)`. On a non-zero exit, the error message includes stderr's first line.
2. `findOpenCode`: `exec.LookPath("opencode")`. If that fails, try `filepath.Join(home, ".opencode", "bin", "opencode")` if it exists and is executable. Otherwise return the error `opencode not found on PATH or in ~/.opencode/bin`.
3. `probeOpenCodeService(bin)`:
   - Run `opencode service status`. Output `stopped` gives `Running: false` and a nil error.
   - Otherwise the output is a URL such as `http://127.0.0.1:49374`. Parse it with `net/url`. A port that doesn't parse returns an error naming the output.
   - Set `Running: true` and `Port`.
   - Look the listener up at `127.0.0.1:<port>`, falling back to `[::1]:<port>` if the first is not found. Then call `openCodeEnviron(PID)`.
   - `Proxy` is the value of the first of `HTTPS_PROXY` and `https_proxy` that is present.
   - A lookup or environ failure sets `EnvErr` and returns a nil error: the service is running, it just can't be inspected.
4. `sameProxy`:
   - Parse both values as URLs, with or without a scheme, and compare ports.
   - Hosts match when equal case-insensitively, or when both are among `localhost`, `127.0.0.1`, `::1`.
   - Two empty values are NOT the same proxy.
   - Before writing this, check `bobSameLoopback` in `cmd_bob.go` and reuse it if it already does exactly this.
5. **In `runExec`**, directly before `return runChild(cmdArgs, inject, stdout, stderr)`, add `warnOpenCodeService(cmdArgs, inject, stderr)`. It does all of the following:
   - It does nothing unless `filepath.Base(cmdArgs[0]) == "opencode"`.
   - It uses `findOpenCode()`; if that fails, it does nothing.
   - It probes. If the probe errors or the service is not running, it does nothing. A service the child starts inherits this environment, which is correct.
   - If `EnvErr != nil`, it prints exactly:
     `agentop: note: could not check OpenCode's background service (<EnvErr>).\n  If it was started without Cortex, OpenCode's traffic bypasses Cortex.\n`
   - If `!sameProxy(svc.Proxy, inject["HTTPS_PROXY"])`, it prints exactly:

```
agentop: warning: OpenCode's background service (pid <PID>) is not using Cortex.
  It sends all of OpenCode's traffic and keeps the environment it started with,
  so this session bypasses Cortex. To route it through Cortex for good:
    agentop configure opencode enable
    opencode service restart   # ends every OpenCode session using the service
```

   - It never blocks and never changes the exit status. The child runs either way.

**Tests** (`opencode_test.go`, with every seam replaced and `t.Setenv("HOME", t.TempDir())`):
- **probe, not running:** `service status` → `stopped` gives `Running false` and no lookup.
- **probe, running:** a URL output gives `Port` 49374, `PID` from the fake listener, `Proxy` from a fake environ containing `HTTPS_PROXY=http://127.0.0.1:47600`.
- **probe, lowercase proxy only:** a lowercase-only `https_proxy` is read.
- **probe, environ fails:** gives `EnvErr` set, `Running true`, and a nil error.
- **`sameProxy` table:**
  - `http://localhost:47600` and `http://127.0.0.1:47600` are the same.
  - `127.0.0.1:47600`, with no scheme, and `http://[::1]:47600` are the same.
  - port 47600 against 47601 is not.
  - an empty value against anything is not.
- **`warnOpenCodeService`:**
  - Nothing is printed for a command that is not opencode, and the probe is not called.
  - Nothing is printed when the service is stopped.
  - Nothing is printed when the service's proxy is Cortex's.
  - The warning text above is printed, with the pid, when its proxy is unset.
  - The note is printed when `EnvErr` is set.
  - Assert on the exact strings.

- [ ] Steps:
  1. Write the tests. Run them to see them fail: `cd cmd/agentop && env -u SSL_CERT_FILE GOWORK=off go test -run 'OpenCode|SameProxy' .`
  2. Implement.
  3. Run `env -u SSL_CERT_FILE GOWORK=off go test -race ./... && GOWORK=off go vet ./... && GOWORK=off go mod tidy -diff`. If the module now needs `core/peerproc`, that is fine: core is already a dependency.
  4. Commit (`cmd/agentop/opencode.go cmd/agentop/opencode_test.go cmd/agentop/cmd_exec.go`, plus `cmd/agentop/cmd_exec_test.go` if changed) with the message:

```
feat: Warn under agentop exec when OpenCode's service bypasses Cortex

OpenCode's background service sends all of its traffic and keeps the
environment of whichever client started it, so `agentop exec --
opencode` does nothing for a service already running. agentop now
finds the service by its port, reads its environment, and says so,
naming the fix; it never restarts the service.

Part of #941.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
```

---

### Task 4: `agentop configure opencode enable | disable | status`

**Files:**
- Create: `cmd/agentop/cmd_opencode.go`
- Create: `cmd/agentop/cmd_opencode_test.go`
- Modify: `cmd/agentop/cmd_configure.go` — the `opencode` dispatch arm, `configureUsage`, the unknown-agent list
- Modify: `cmd/agentop/README.md` — the configure command reference, if it lists agents

**Interfaces:**
- Consumes (Task 3): `openCodeRun`, `findOpenCode`, `probeOpenCodeService`, `sameProxy`.
- Consumes (existing, `cmd_claudecode.go`):
  - `wantedFromConfig(cortexCfgPath) (map[string]string, *config.Config, error)`
  - `bridgeEnabled`, `errBridgeDisabled`, `isCortexValue`
  - `readState` / `writeState` / `managedState`
  - `confirm`, `exitDeclined`
  - the `env*` constants, `bundleKeys`, `cortexCfgRel`
- Produces: `func runOpenCode(args []string, stdout, stderr io.Writer) int`.

**Behaviour, exactly:**

1. **Usage and flags.** `agentop configure opencode enable | disable | status`, with flags `--yes`, `--config PATH` (the Cortex config, default `~/.cortex/config.yaml`) and `--opencode BIN` (default `findOpenCode()`).
   - Write usage text in the style of `claudeCodeUsage`.
   - Exit codes follow claude-code: 0 for applied or already correct, 3 for declined, 1 for an error, 2 for a usage error.
2. **Managed keys,** in this order: `HTTPS_PROXY`, `HTTP_PROXY`, `https_proxy`, `http_proxy`, `NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`, `GIT_SSL_CAINFO`, `REQUESTS_CA_BUNDLE`, `CURL_CA_BUNDLE`. These are the same variables `agentop exec` sets.
   - The values come from `wantedFromConfig(--config)`: its `HTTPS_PROXY` URL goes in all four proxy keys, `NODE_EXTRA_CA_CERTS` gets `ca.crt`, and the four bundle keys get the bundle.
   - It does NOT set `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`.
   - It refuses, as claude-code does, when the bridge is disabled or there is no `ca_dir`.
3. **Reading and writing OpenCode's service env** goes only through the CLI:
   - `opencode service get env` returns a JSON object of strings, `{}` when empty. Anything else is an error naming the output.
   - `opencode service set env K V` and `opencode service unset env K` change one variable each.
4. **enable:**
   - **Refuse foreign values.** If a managed key is set to a value that is neither ours nor `isCortexValue(k, v)`, refuse with:
     `<KEY> is already set to "<v>" in OpenCode's service environment.\n  Refusing to overwrite a value you set. Remove it first: opencode service unset env <KEY>`
   - **Already enabled.** If nothing differs, print `Already enabled: OpenCode's service environment routes it through Cortex.` and go to step 7.
   - **Show the change and confirm.** Otherwise print:

```
Sets in OpenCode's background-service environment (opencode service set env):
  KEY=VALUE
  …
Nothing else changes; agentop configure opencode disable puts back what was there.
```

     Then confirm with `opencodeConfirm`, a package variable initialised to `confirm`. Declining prints `Not changed.` and exits 3.
   - **Record what was there.** Write the state file `~/.cortex/opencode-state.json` with `writeState`. Use `Settings: "opencode service env"` and `Prior` as each managed key's value before the change, nil when it was absent. It is written only on the first enable, as `writeState` already guarantees.
   - **Apply.** Call `opencode service set env K V` for each differing key, in managed-key order. Then print `Enabled — OpenCode's service uses Cortex from its next start.`
5. **disable:**
   - **Find what's set.** If no managed key is set, print `Nothing to do: none of the Cortex variables are set in OpenCode's service environment.` and exit 0.
   - **Confirm.** Print `This will remove from OpenCode's service environment: K1, K2, …`, then confirm.
   - **Restore each present key from the state file:**
     - a recorded nil prior means `unset env K`;
     - a recorded value means `set env K <prior>`;
     - no record, or an unreadable one, means `unset env K`. When the record is unreadable, warn first, worded as claude-code's `applyClaudeCodeDisable` does.
   - **Clean up and report.** Remove the state file. List restored keys the way claude-code does, then print `Disabled. OpenCode's service no longer routes through Cortex from its next start.` and go to step 7.
6. **status:**
   - Print each managed key as `  KEY=VALUE` or `  KEY (unset)`, sorted, then `enabled` or `not fully enabled (n of 9 set)`, the same shape as `claudeCodeStatus`.
   - Then print one service line, step 7's wording, or `OpenCode's background service is not running.`
7. **After enable, disable or status, report the running service:**
   - **Its proxy disagrees with the configured state.** That is: enabled, but `!sameProxy(svc.Proxy, want)`; or disabled, but `sameProxy(svc.Proxy, want)`. Print:
     `OpenCode's background service (pid <PID>) is running with its old environment.\n  It picks this up when it restarts: opencode service restart   # ends every OpenCode session using it`
   - **Its proxy agrees.** Print `OpenCode's background service (pid <PID>) is using Cortex.` when enabled, and nothing extra when disabled.
   - **It can't be inspected** (`EnvErr`). Print `Could not check OpenCode's running service (<err>); restart it to be sure: opencode service restart`.
   - Never restart it.
8. **`cmd_configure.go` changes:**
   - The `opencode` arm calls `runOpenCode(args[1:], stdout, stderr)`.
   - In `configureUsage`:
     - Replace the line `  agentop configure codex | opencode` with `  agentop configure opencode enable | disable | status [--yes] [--config PATH]`, followed by `  agentop configure codex`.
     - Replace the `opencode       not yet persistent — use "agentop exec -- opencode"` entry with:
       `  opencode       sets the proxy and CA variables in OpenCode's background-service\n                 environment, which sends all of OpenCode's traffic. Run\n                 "agentop configure opencode --help" for the detail.`
     - In the paragraph beginning `Three agents persist`, make it four agents and add a sentence: `OpenCode's background service keeps an environment of its own, so its configuration goes there, through the opencode CLI.` Change `Codex and OpenCode read the process environment and nothing else` to `Codex reads the process environment and nothing else`. Re-read the whole paragraph afterwards so it stays true.
   - The unknown-agent list stays `(claude-code, bob, bobshell, codex, opencode)`.

**Tests** (`cmd_opencode_test.go`):
- Use an in-memory fake `openCodeRun`: a `map[string]string` env, a running flag, a port, and a log of calls. Fake `openCodeListener` and `openCodeEnviron` too.
- Set `t.Setenv("HOME", t.TempDir())` and write a minimal Cortex config into that HOME, as `cmd_claudecode_test.go`'s tests do; read them to reuse their config fixture.
- Every test passes `--opencode /fake/opencode`.
- Cases:
  - **enable on an empty env:** all 9 keys are set with the right values, the state records 9 nil priors, and output includes the `Sets in …` block.
  - **enable is idempotent:** a second run prints `Already enabled` and makes no `set` calls.
  - **enable refuses a foreign value:** with `HTTPS_PROXY=http://corp:3128`, it exits 1 with the refusal text and makes no `set` calls.
  - **enable then disable:** given a user `NODE_EXTRA_CA_CERTS=/my/ca.pem` beforehand, disable unsets the 8 keys that were absent and sets `NODE_EXTRA_CA_CERTS` back to `/my/ca.pem`, then removes the state file.
  - **disable with nothing set:** prints `Nothing to do` and exits 0.
  - **declining:** without `--yes`, a stubbed `opencodeConfirm` that answers false gives exit 3, `Not changed.`, and no `set` calls.
  - **status:** shows `not fully enabled (0 of 9 set)` with nothing set, and `enabled` after enable.
  - **the restart note:**
    - enabled, with a running service whose proxy is empty, prints the `running with its old environment` note;
    - a service whose proxy is Cortex's prints `is using Cortex`;
    - a stopped service prints `is not running` under `status`.
  - **the bridge is off:** with the Cortex config's bridge disabled, enable refuses the way claude-code does.
  - **`runConfigure([]string{"opencode", "status", …})`** reaches `runOpenCode`, not the old coming-soon text.

- [ ] Steps:
  1. Write the tests and run them to fail.
  2. Implement.
  3. Run `cd cmd/agentop && env -u SSL_CERT_FILE GOWORK=off go test -race ./... && GOWORK=off go vet ./... && GOWORK=off go mod tidy -diff`.
  4. Commit (`cmd/agentop/cmd_opencode.go cmd/agentop/cmd_opencode_test.go cmd/agentop/cmd_configure.go`, plus `cmd/agentop/README.md` if changed):

```
feat: Add agentop configure opencode

OpenCode's background service sends all of OpenCode's traffic and
keeps an environment of its own, which it prefers over what it
inherited. enable sets the proxy and CA variables there through the
opencode CLI, recording what was there so disable can put it back;
it refuses to overwrite a value someone else set. Neither restarts
a running service: both say when one is running with its old
environment and name opencode service restart.

Part of #941.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
```

---

### Task 5: The OpenCode doc, the docs that name agents, and verification

**Files:**
- Create: `docs/agents/opencode.md`
- Modify: `README.md` — the `**Any agent works**` line
- Modify: `docs/laptop-service.md` — the `Agents other than Claude Code and Bob need to be named` bullet and any list of the default session headers
- Modify: `cmd/agentop/README.md` — if Task 4 did not already cover `configure opencode`

**`docs/agents/opencode.md`** has these sections, as #941 requires. Every claim in it must be something the code does or the controller verified; mark unverified things as unverified.

1. **Enable and revert:**
   - `agentop configure opencode enable`, `disable` and `status`.
   - What enable writes: the `env` block of OpenCode's `service.json`, via `opencode service set env`. That is `~/.config/opencode/service.json`, or `$XDG_CONFIG_HOME/opencode/service.json`.
   - The state file: `~/.cortex/opencode-state.json`.
   - How to undo it by hand: `opencode service unset env <NAME>` for each of the nine variables.
   - That a running service picks the change up only on `opencode service restart`, which ends every OpenCode session using it.
   - The alternative: `agentop exec -- opencode` works only when it starts the service, and warns otherwise.
2. **CA trust:**
   - Enable sets both `NODE_EXTRA_CA_CERTS` (`ca.crt`) and the bundle variables.
   - The symptom when trust is missing. Leave a placeholder line `<!-- CONTROLLER: fill from acceptance -->` for the exact error text. The controller fills it in during acceptance.
3. **What Cortex shows for it:**
   - Its own agent row (`opencode`).
   - Its sessions keyed by `X-Opencode-Session-Id` (`ses_…`).
   - Inference parsed as typed events for OpenCode Zen (`/zen/v1/chat/completions`) and OpenAI-compatible providers such as LiteLLM, with token counts and cost wherever those providers report usage.
   - Tool pruning is **unsupported**: agentop's tool inventory knows only Claude Code's built-in tools.
   - With process attribution (the laptop default), the service's header-less traffic files under its sessions, and the TUI's own polling of the service is not recorded once the service has named a session.
4. **Verified depth:** OpenCode 2.0.21 on macOS 26.6 (arm64), Cortex built from this branch. Linux, other OpenCode versions and providers other than OpenCode Zen were not tested live.
5. **Known issues:**
   - A service holding several sessions files its header-less traffic under the most recently active one, and before a new session's first request, under the previous one.
   - Requests before the service names its first session are recorded, in a per-process pending bucket that this session then adopts.
   - The service keeps the environment it started with (see above).
   - With #1223 merged, OpenCode's probes of local model servers that are not running (LM Studio on `:1234`, `:8000`) appear as `502 upstream_refused` rows in its session every probe cycle.
   - OpenCode also sends `X-Session-Id`, which Cortex does not read.

**README:** change `**Any agent works**, not only Claude Code: point it at` to `**Any agent works**, not only Claude Code ([OpenCode](./docs/agents/opencode.md) has its own \`agentop configure\` command): point it at`. Keep the rest of the line.

**laptop-service.md:** the bullet `Agents other than Claude Code and Bob need to be named` becomes `Agents other than Claude Code, Bob and OpenCode need to be named`. Wherever that file names the default header list, add `X-Opencode-Session-Id`.

- [ ] Steps:
  1. Write the docs.
  2. Run the full verification and paste every command's output into the report:

```bash
cd core && go vet ./... && go test -race ./... && go mod tidy -diff && cd ..
gofmt -l core/pipeline core/session core/config core/peerproc cmd/agentop
(cd cmd/agentop && GOWORK=off go vet ./... && env -u SSL_CERT_FILE GOWORK=off go test -race ./... && GOWORK=off go mod tidy -diff)
TAGS=$(go -C scripts/profile-tags run . full)
(cd cmd/cortex && GOWORK=off go vet -tags "$TAGS" ./... && GOWORK=off go test -tags "$TAGS" ./... && GOWORK=off go mod tidy -diff)
```

  3. Commit (`docs/agents/opencode.md README.md docs/laptop-service.md`, plus `cmd/agentop/README.md` if changed):

```
docs: Add the OpenCode agent page

How to route OpenCode through Cortex and undo it, what it needs to
trust the CA, what Cortex records for it, what was verified, and its
known issues. Linked from the README; laptop-service names OpenCode
among the agents whose sessions are grouped without configuration.

Part of #941.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
```

**Acceptance (controller, not a subagent).** It runs on a private proxy (ports 477xx), with a scratch `HOME` and an isolated `XDG_*` OpenCode on port 49399:
1. `configure opencode enable --yes --config <private>` writes the service env. A plain `opencode run`, with the client's proxy variables stripped, is recorded under an `opencode` agent row and a `ses_…` session.
2. A service started without the env makes `agentop exec -- opencode run …` print the warning. `status` prints the restart note.
3. `disable` restores a pre-set variable and unsets the rest.
4. Trust experiment: with only `NODE_EXTRA_CA_CERTS` set, then only `SSL_CERT_FILE`, record which one Bun-built OpenCode honours and the error text when neither is set. Fill in the doc's placeholder.
