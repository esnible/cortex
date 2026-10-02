# Per-process session attribution

**Date:** 2026-10-01
**Status:** approved, implementing
**Issue:** #1187

## Goal

Every row the laptop proxy records lands in the session it belongs to — for Claude Code,
Bob and OpenCode, on macOS and Linux — and no agent's rows land in another agent's
session. In-cluster behaviour does not change.

## The problem, as it shows on a live store

On 2026-10-01 a laptop running Claude Code, Bob and OpenCode at once had 810 events in
`default`. About 780 were OpenCode's own; the rest belonged to other agents: 11
`ete-litellm` CONNECT rows from Claude Code, a Bob tunnel's open and close, two `node`
requests. In the other direction about 20 OpenCode rows sat inside Claude sessions.

The cause is one rule. A row that names no session — every CONNECT, every header-less
request from an agent the proxy does not recognise, every child process an agent spawns
— is filed under `ActiveSession()`, the session appended to most recently. Appends to
`default` count. OpenCode's TUI polls its own background service through the proxy, about
1.3 requests a second, so `default` was the most recent session almost all the time, and
whatever arrived next joined it. Across the whole store, 119 of 121 tunnel rows sat in the
session of the event appended just before them.

Client affinity (#1194, on by default since #1204) does not reach these rows: it keys on
User-Agent, and a CONNECT almost never carries one — none of Claude Code's or OpenCode's
do.

## What the experiments established

Measured on 2026-10-01 against main `39cb3dea`, with a private instrumented proxy on
`:477xx` that logged each request's client PID, ancestry and session-ish headers, and an
OpenCode isolated through its own `XDG_*` directories.

| Question | Answer |
|---|---|
| OpenCode's process model | A background service, `opencode serve --service`, on a **fixed port (49374)** — one per user. The first client spawns it; when that client exits it is reparented to PID 1, and every later client reuses it. |
| Who sends OpenCode's traffic | **The service, for every session**: inference, `webfetch`, the model catalog, and local-model probes every 30 s. `bash`-tool commands are its direct children. Clients only call the service's `/api/*` over loopback, through the proxy, because `agentop exec` sets no `NO_PROXY`. |
| OpenCode's session id | On inference: `x-opencode-session-id`, plus `X-Session-Id`, `x-session-affinity`, `x-opencode-session`, all carrying the same `ses_…` value. Nothing on any CONNECT, not even a User-Agent. `traceparent` is per-process — identical across sessions — so it cannot tell sessions apart. |
| Two concurrent OpenCode sessions | One service PID served both. Each tool's traffic fell between its own session's `finish_reason: tool_calls` response and that session's next request, so at each tool call the owning session was the process's most recently active one. |
| OpenCode's environment | The service keeps **its first client's environment**. Started without the proxy, it bypassed Cortex entirely, even for a later `agentop exec -- opencode`. |
| Claude Code | One process, one session at a time. `Bash` children (`curl` → `bash` → `claude`) descend from it; `WebFetch` and MCP run in-process, header-less. A nested `claude -p` sends header-less MCP calls before its own first header. |
| Lookup cost, macOS | Connection owner and listener owner each ~150–200 µs (`net.inet.tcp.pcblist_n`), pure Go, no root. The listener lookup tells OpenCode's service from Ollama on `11434`. |
| Lookup cost, Linux | From the earlier spike: ~3 ms scanning only candidate process trees, 23 ms for a full `/proc/*/fd` scan over 503 processes. Must run while the connection is live: a zombie's fd directory is unreadable. |
| Bob | Not measured: headless mode needs `BOB_API_KEY`. Which of `node bob` → `node bob` sends the pre-header calls is still unknown; the design covers either. |

## Design

### Resolution order

Each recorded row takes the first answer that applies:

1. **A session header** on the request (`id_headers`). Unchanged — except that the
   process sending it now also *claims* that session (step 3).
2. **A bridged tunnel's open row** takes the session of the tunnel's first decrypted
   request, and is appended immediately before it.
3. **Process attribution** (laptop only, see *Scope*): look the client PID up and walk its
   ancestry to the nearest process that has claimed a session.
   - **Found — the process itself or an ancestor:** that process's session. A process
     holding several live sessions (OpenCode's service) answers with its most recently
     active one. A descendant is bound once, on first sight, and keeps that binding for
     its lifetime.
   - **Nested agent:** a process that sends its own session header becomes the nearest
     claimer for itself and its descendants, so a `claude -p` started from a Claude
     session's shell does not inherit its parent's session.
   - **No claimer yet, but some process in the chain has sent a known coding agent's
     User-Agent:** a pending bucket for that agent's **root** — the topmost process of an
     unbroken run of that agent's processes —
     adopted by the first session any process under that root claims.
   - **No agent in the chain:** `default` while some agent has claimed a session within
     the last 5 minutes, else `ActiveSession()`. Your terminal's `curl` is no agent's.
     Today's affinity sends an unknown client to `default` only when two agents are
     active, because by User-Agent alone it cannot rule out one of them; a lookup can, so
     one active agent is enough.
4. **No lookup** — disabled, failed self-test, another user's process, another PID
   namespace — falls back to today's agent-name affinity.

### Bridged CONNECT rows (PR 1)

Today `bridgeServe` records the open row as soon as the forged handshake succeeds,
before any decrypted request exists, so it can only use `ActiveSession()`.

- **Deferred.** The open is held on the `tunnelLog` and recorded by the first decrypted
  request that records a row — its request row, or its rejection — under that row's
  session, whatever resolved it. PR 3 then improves the request's attribution and the
  open follows for free.
- **Adjacent.** The two rows go in through one new store call that appends both under a
  single lock acquisition. agentop folds a tunnel row only into the event that
  immediately follows it in the session (`buildEventRows`), so two separate appends could
  be split by any concurrent traffic. The `tunnelLog` lock is held across the claim and
  the append, so a second multiplexed request on the same tunnel cannot land between
  them.
- **Stamped with its first row's time, not the CONNECT's.** The open row takes the `At`
  of the row it is appended with. Keeping the CONNECT's own time would put a row with an
  earlier `At` after rows with later ones, and agentop's pager treats an older page whose
  last event is later than the newer page's first as "session restarted" — a false flash
  at any page boundary that fell between them. Stamping it at its own append would be a
  few microseconds *later* than the request after it, the same fault in the other
  direction. The cost is the 50–150 ms between CONNECT and first request.
- **Flushed if nothing records.** A tunnel whose admitted handlers all finish without
  recording — none admitted, a body that failed to read — records the open on today's
  rule and a close, once `ServeConn` has returned and no admitted handler is still
  running. This replaces `closeUnserved`'s `served == 0` test, which was right only
  while the open was recorded eagerly.
- **Built when the tunnel is marked bridged.** The open event is built then, on the
  CONNECT's goroutine, and only stamped and appended later. On h2 a handler can outlive
  `ServeConn` while `handleConnect` runs the CONNECT's finishers, which may write its
  context, so a handler must not read the CONNECT's context to build the row.
- **Out of scope:** opaque tunnels keep `tunnelSessionID`; the transparent listener's
  own open path is untouched, since nothing redirects into it on a laptop.
- **Applies everywhere, changes nothing in-cluster.** PR 1 is not gated on scope: a
  sidecar's decrypted requests carry no session header and resolve to `ActiveSession()`,
  the same answer the eagerly recorded open got, only ~100 ms later.

### `core/peerproc` (PR 2)

One package, three questions, per OS:

```go
type Proc struct {
	PID, PPID int32
	Start     time.Time // with PID, the process's identity: PIDs are reused
	Exe       string    // absolute executable path
}

type Resolver interface {
	// ConnOwner is the process holding the client end of an accepted connection.
	ConnOwner(client, server netip.AddrPort) (Proc, error)
	// ListenerOwner is the process listening on addr.
	ListenerOwner(addr netip.AddrPort) (Proc, error)
	// Ancestry is pid and its parents, nearest first, stopping at PID 1 or max.
	Ancestry(pid int32, max int) ([]Proc, error)
}

// New runs the self-test and returns ErrUnsupported where no implementation exists.
func New() (Resolver, error)
```

- **macOS:** `net.inet.tcp.pcblist_n`, walking kind-tagged records. `xinpcb_n` gives
  foreign and local port and the IPv4 local address; the following `xsocket_n` gives
  `so_last_pid`. A listener is an `xinpcb_n` with foreign port 0. `kinfo_proc` gives
  parent PID and µs start time; `kern.procargs2` gives the executable path. Record sizes
  are validated — 104/104 on macOS 26.6.2 — and any other size disables the resolver
  rather than guessing at a layout.
- **Linux:** `/proc/net/tcp` (and `tcp6`) for the socket inode — `st` `0A` for a listener —
  then a scan of `/proc/<pid>/fd` for `socket:[inode]`. Candidates first (callers pass a
  hint set: the processes already known to belong to agents and their descendants), a full
  scan only when that misses. `/proc/<pid>/stat` for parent and start time (clock ticks
  since boot, converted with `btime`); `readlink /proc/<pid>/exe` for the executable.
- **Self-test, at `New`:** dial a listener of our own and require the lookup to name our
  own PID. A failure logs one warning and returns an error; callers then never ask again.
- **IPv4 loopback is required, IPv6 best-effort.** `bind_loopback_only` binds
  `127.0.0.1`, so clients reach the proxy over IPv4.
- **CI:** a `macos-latest` job runs the package's tests. Linux runs in the existing matrix.

No caller in PR 2; nothing changes behaviour.

### Per-process attribution (PR 3)

**Where the lookup runs.** On the first request of each client connection, never on the
accept loop: `ConnContext` runs there and only attaches an empty, `sync.Once`-guarded
holder that the first request fills. Bridged requests run on an inner server that never
sees the outer connection's context; the holder reaches them through the `bridgedHandler`
closure, which already carries the tunnel. A CONNECT does the lookup itself, before
hijacking, while the client is certainly alive.

**What the store learns.** A request carrying a session header records the claim against
the client process `(PID, start)` as well as against the agent name `Claim` uses today.
Per process the store keeps the sessions it has claimed, each with when it was last
active; a descendant's binding; and, for pending buckets, the agent root. Nothing asks the
kernel whether a process exited: a process is keyed by PID and start time, so a reused PID
is a new process, and the table is pruned instead: once it reaches 4,096 entries,
processes not heard from for 24 hours are dropped, and if that frees nothing, the least
recently seen quarter goes, processes that never named a session first. A claim on a
session that has left the store is forgotten after a
minute. The existing `adopted` map — never pruned before — loses a session's records when
that session is evicted. A pending bucket is per agent process — `pending:<agent>@<pid>.<start>`
— so two windows of one agent starting at once do not share one; client affinity's agent-wide
`pending:<agent>` remains for clients the lookup cannot name.

**Multi-session processes** answer with their most recently active session. That was
correct at both tool calls in the concurrent-OpenCode experiment. The precise rule —
attribute to the one session whose tool window (from a `tool_calls`/`tool_use` response to
its next request) is open — is deferred until a misfile is seen; `FinishReason` and
`ToolCalls` are already parsed, so it needs no new data.

**Agent self-traffic is not recorded.** A plain-HTTP request whose destination is a
loopback port is forwarded the way `skip_hosts` traffic is — no pipeline, no row — when
the listener is an **agent's own process**, one that has named a session through its own
header, and runs the **same executable** as the client. That is OpenCode's TUI and `run`
client talking to their own service; it is also what kept `default` active. Requiring the
listener to be an agent's is what keeps a node-based agent's calls to a local node MCP
server visible: same executable, but an MCP server never names a session. Ollama, LM Studio
and every other local server stay visible for the same reason, and so does a request a
bridged tunnel decrypted, whose tunnel row waits for it. Each program-and-service pair is
announced once at INFO, and every skipped request at DEBUG; nothing is counted on
`/stats`.

**Scope.** `session.process_attribution: auto | on | off`, default `auto`, which means on
exactly when `listener.bind_loopback_only` is set — the laptop install. In-cluster
sidecars never look a process up, so `ActiveSession()`'s correlation of an A2A agent's
outbound calls with its inbound turn, which IBAC relies on, is untouched. Not
hot-reloadable, like the rest of `session.*`.

**Testability.** `Server` gains an injectable resolver, nil by default, which makes tests
independent of the OS: table-driven cases over a fake process table, plus the replay
approach used on session 13cdee89.

### OpenCode support (PR 4)

- `ParseUserAgent` learns `opencode/<channel>/<version>/<client>`, where the version is
  the third segment, and `knownClients` gains `opencode`. Today the User-Agent parses to
  no name at all, so OpenCode is "Other agent".
- `X-Opencode-Session-Id` joins the default `id_headers`, so OpenCode's inference rows
  land in OpenCode's own sessions, and its service claims them.
- `docs/agents/opencode.md`, linked from the README, as every supported agent has.
- **`agentop configure opencode enable | disable | status`** routes OpenCode through Cortex
  persistently. OpenCode's background service, not the process you run, sends all of
  OpenCode's traffic, and it keeps an environment of its own: `opencode service set env
  NAME VALUE` writes it to `service.json`'s `env`, and on start the service takes those
  values over what it inherited (verified on 2.0.21: a client started with a dead proxy
  spawned a service pointed at Cortex). Enable sets the proxy and CA variables `agentop
  exec` sets, through the `opencode` CLI, and records prior values in
  `~/.cortex/opencode-state.json` so disable can restore them; it refuses to overwrite a
  value someone else set. OpenCode's CLI stops a running service whenever its environment
  changes (verified on 2.0.21; it stops only the service its own config started), which
  ends every OpenCode session using it. So enable and disable say so before acting and
  ask, and the service starts with the new environment the next time OpenCode runs.
- **`agentop exec -- opencode` warns** when a service is already running without Cortex's
  proxy: it finds the service by its port (`opencode service status`), its PID with
  `peerproc.ListenerOwner`, and reads that process's environment with a new
  `peerproc.Environ` (macOS `kern.procargs2`, Linux `/proc/<pid>/environ`, same user only).
  `service.json` holds a password, a port and the `env` block, but no PID, which is why the
  original plan to read the PID from it could not work.

## Decisions recorded

- **Process lookups are laptop-only** (`auto`). Rejected: running them in-cluster behind
  a 5-minute rule; with `shareProcessNamespace` they succeed and would pull an A2A
  agent's calls away from the session that caused them.
- **Agent self-traffic is not recorded.** Rejected: filing it under the session named in
  its URL path, which keeps ~1.3 rows a second of an agent's own UI polling in the
  session people read.
- **The self-traffic listener must be an agent's process**, not only the same executable:
  an interpreter (node) makes executables equal across unrelated programs, so Bob's calls
  to a local node MCP server would vanish. **No `/stats` counter** for it: one INFO line
  per program and service, DEBUG per request. **cortex-cpex is not wired**: it needs cgo,
  and never runs on a laptop.
- **A multi-session process answers with its newest session**; tool windows wait for
  evidence.
- **A stale OpenCode service gets a warning, not a restart.**
- **configure opencode warns, then lets OpenCode's CLI stop the service.** Rejected: editing
  service.json directly to spare the running service, which bypasses the CLI's ownership
  of that file.
- **OpenCode is configured through its service's own environment**, not by `agentop exec`
  alone: the service outlives its clients and keeps the environment of whichever started
  it, so `agentop exec -- opencode` does nothing for a service already running.
- **The stale-service check reads the running process's environment**, not the config: a
  service started before `configure opencode enable` has the config but not the values.
- **The deferred open row takes its first row's time.** This reverses the earlier "keep
  the CONNECT's time", for the pager reason above.
- **`traceparent` is not a correlation key** — per-process in OpenCode.

## Known gaps after all four PRs

- Between `/clear` and the first request carrying the new session id, header-less
  traffic goes to the old session; work still running from before `/clear` keeps it.
- `claude --resume` into a session that already holds events: that process's pre-header
  calls stay in a pending row, since the store does not merge two histories.
- Processes outside an agent's tree that work for it — the Docker daemon, IDE language
  servers — go to `default`.
- Concurrent OpenCode sessions whose tool calls overlap can swap rows.
- Bob's process layout is unverified; pending-per-root covers either answer.

## PR sequence

| PR | Change | Size |
|---|---|---|
| 1 | Bridged CONNECT rows follow their first request | +500–700 |
| 2 | `core/peerproc` for macOS and Linux, self-test, macOS CI | +1,000–1,300 |
| 3 | Per-process attribution, self-traffic suppression, `session.process_attribution` | +1,100–1,600 |
| 4 | OpenCode support | +300–600 |

1 → 2 → 3 → 4. PR 4 depends on none of the others and could land right after PR 1, at
the cost of OpenCode's self-traffic landing in OpenCode's sessions instead of `default`
until PR 3. Each PR gets its own implementation plan in `docs/superpowers/plans/`,
written when it starts, so it reflects what the PRs before it actually changed.
