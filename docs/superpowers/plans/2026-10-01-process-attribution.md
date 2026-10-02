# Per-Process Session Attribution — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A request that carries no session header is filed under the session of the process that sent it — or of its nearest ancestor that named one — looked up per client connection with `core/peerproc`, on laptop installs; and an agent's traffic to its own loopback service is forwarded without being recorded.

**Architecture:** The session store gains a process table keyed by (PID, start time): `ClaimProcess` records which process named which session through its header; `SessionForProcess` walks a header-less request's process chain to the nearest claimer (multi-session processes answer with their newest session; a tool is bound at first sight), stops at an unclaimed agent process with a per-process pending bucket, and sends a process of no agent to `default` while some agent is active. The forward proxy looks the client's chain up once per connection (an `http.Server.ConnContext` slot filled on the first request; bridged requests reuse their CONNECT's), threads it through session resolution, and skips recording plain-HTTP requests to a loopback listener that is an agent's own process running the client's executable. `session.process_attribution: auto|on|off` gates it; `auto` = on exactly when `listener.bind_loopback_only`.

**Tech Stack:** Go 1.26.5; `core/session`, `core/listener/forwardproxy`, `core/config`, `core/bootstrap`, `cmd/cortex`; `core/peerproc` (from #1229, merged into this branch's base).

**Spec:** `docs/superpowers/specs/2026-10-01-per-process-session-attribution-design.md` — "Resolution order" step 3–4 and "Per-process attribution (PR 3)". This plan is PR 3 of four. Two approved refinements to the spec (Task 5 writes them into it): the self-traffic rule also requires the listener to be an agent's process (one that has named a session), so a node-based agent's calls to a local node server stay visible; and suppression is announced by log lines, not a `/stats` counter (`/stats` carries only auth counters).

## Global Constraints

- Work only in `.worktrees/procattr` (branch `feat/process-attribution`; base = `main` at `e5e71aee` with `feat/peerproc` merged in at `dbed2fa6`). Never the top-level checkout, never another worktree.
- Every commit: `git commit -s`, message ending `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`; never `Co-Authored-By`. Stage by explicit path only — `.superpowers/` is un-ignored scratch and must never be committed.
- `gofmt -l` on every package you touched prints nothing; `go vet ./...` in `core/` passes; `go mod tidy -diff` prints nothing in `core/` and `cmd/cortex/`.
- **In-cluster behaviour does not change:** with `process_attribution` off (the default anywhere `bind_loopback_only` is unset) or `Server.Processes == nil`, every resolution path is byte-for-byte today's.
- A lookup failure is never an error to the client: resolution falls back to client affinity, then `ActiveSession()`, exactly as today.
- Plugins never hear the default bucket as an identity: where resolution answers `session.DefaultSessionID`, `resolvePluginSessionID` returns `""` (the existing convention).
- `cmd/cortex-cpex` is NOT changed (needs cgo and a pinned library; never runs on a laptop).
- Run Go tests from `core/` (workspace); `cmd/cortex` with `GOWORK=off` and the `full` profile's tags.

---

### Task 1: The session store's process table

**Files:**
- Create: `core/session/process.go`
- Create: `core/session/process_test.go`
- Modify: `core/session/store.go` — `Store` struct fields; `cleanupLocked`; `evictOldestLocked`
- Modify: `core/session/affinity.go` — `SessionForClient`'s owner derivation

**Interfaces:**
- Produces (exported, used by Task 2):
  - `type Proc struct { PID int32; Start int64; Exe string }` (`Start` = Unix nanoseconds)
  - `func PendingProcessID(agent string, pid int32) string` → `"pending:<agent>@<pid>"`
  - `func (s *Store) ClaimProcess(sessionID, agent string, chain []Proc)`
  - `func (s *Store) SessionForProcess(chain []Proc, agent string) string` — a session id, `DefaultSessionID`, or `""` (fall back)
  - `func (s *Store) ProcessHints(limit int) []int32`
  - `func (s *Store) IsAgentProcess(p Proc) bool`

- [ ] **Step 1: Write the failing tests**

Create `core/session/process_test.go`:

```go
package session

import (
	"fmt"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// procTestClock is a store clock tests move by hand, so "most recently active" and the
// ambiguity window never depend on how fast the machine is.
type procTestClock struct{ t time.Time }

func (c *procTestClock) now() time.Time          { return c.t }
func (c *procTestClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newProcStore() (*Store, *procTestClock) {
	clk := &procTestClock{t: time.Unix(1_700_000_000, 0)}
	return New(0, 0, 0, WithClock(clk.now)), clk
}

// pchain is a process chain, nearest first. Each pid's start time is derived from it, so
// the same pid always names the same process unless a test says otherwise.
func pchain(pids ...int32) []Proc {
	out := make([]Proc, len(pids))
	for i, pid := range pids {
		out[i] = Proc{PID: pid, Start: int64(pid) * 1000, Exe: fmt.Sprintf("/bin/p%d", pid)}
	}
	return out
}

// recordIn appends one request to id a second after the last, as the listener does once
// it has resolved where the request goes.
func recordIn(s *Store, clk *procTestClock, id string) {
	clk.advance(time.Second)
	s.Append(id, pipeline.SessionEvent{At: clk.now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest})
}

// A tool an agent's shell runs belongs to the session that ran it, and keeps it when the
// agent moves on (/clear); a tool started after the move gets the new one.
func TestSessionForProcess_AToolJoinsTheSessionOfTheAgentThatRanIt(t *testing.T) {
	s, clk := newProcStore()
	defer s.Close()
	claude := pchain(100, 50)
	s.ClaimProcess("s1", "claude-code", claude)
	recordIn(s, clk, "s1")

	curl := pchain(300, 200, 100, 50) // curl ← bash ← claude ← login shell
	if got := s.SessionForProcess(curl, ""); got != "s1" {
		t.Fatalf("tool of the agent: %q, want s1", got)
	}
	s.ClaimProcess("s2", "claude-code", claude)
	recordIn(s, clk, "s2")
	if got := s.SessionForProcess(curl, ""); got != "s1" {
		t.Errorf("a tool still running moved to %q after the agent named s2; want it kept in s1", got)
	}
	if got := s.SessionForProcess(pchain(301, 200, 100, 50), ""); got != "s2" {
		t.Errorf("a tool started after the move: %q, want s2", got)
	}
}

// A nested agent — claude -p run from another session's shell — is an agent of its own:
// before its first header its calls wait in its own pending bucket, which its session
// adopts, and neither agent takes the other's session.
func TestSessionForProcess_ANestedAgentIsNotFiledUnderItsParent(t *testing.T) {
	s, clk := newProcStore()
	defer s.Close()
	outer := pchain(100, 50)
	s.ClaimProcess("outer", "claude-code", outer)
	recordIn(s, clk, "outer")
	nested := pchain(400, 200, 100, 50) // claude -p ← bash ← claude

	// Its first request is a CONNECT, which carries no User-Agent: it looks like any
	// tool of the outer session until a request names the agent.
	if got := s.SessionForProcess(nested, ""); got != "outer" {
		t.Fatalf("before it identifies: %q, want outer", got)
	}
	pending := s.SessionForProcess(nested, "claude-code")
	if want := PendingProcessID("claude-code", 400); pending != want {
		t.Fatalf("once it identifies: %q, want its own pending bucket %q", pending, want)
	}
	recordIn(s, clk, pending)
	s.ClaimProcess("inner", "claude-code", nested)
	if v := s.View("inner"); v == nil || len(v.Events) != 1 {
		t.Errorf("inner = %+v, want the adopted pre-header call", v)
	}
	recordIn(s, clk, "inner")
	if got := s.SessionForProcess(nested, "claude-code"); got != "inner" {
		t.Errorf("nested agent: %q, want inner", got)
	}
	if got := s.SessionForProcess(outer, "claude-code"); got != "outer" {
		t.Errorf("outer agent: %q, want outer", got)
	}
}

// One process serving several sessions — OpenCode's background service — answers with the
// one most recently active, and a tool it spawns keeps that answer.
func TestSessionForProcess_ASharedServiceAnswersWithItsNewestSession(t *testing.T) {
	s, clk := newProcStore()
	defer s.Close()
	svc := pchain(500)
	s.ClaimProcess("a", "", svc)
	recordIn(s, clk, "a")
	s.ClaimProcess("b", "", svc)
	recordIn(s, clk, "b")
	recordIn(s, clk, "a") // a spoke last

	if got := s.SessionForProcess(svc, ""); got != "a" {
		t.Fatalf("service: %q, want a, its most recently active session", got)
	}
	child := pchain(600, 500)
	if got := s.SessionForProcess(child, ""); got != "a" {
		t.Fatalf("its tool: %q, want a", got)
	}
	recordIn(s, clk, "b")
	if got := s.SessionForProcess(child, ""); got != "a" {
		t.Errorf("its tool moved to %q when the other session spoke; want it kept in a", got)
	}
}

// A process of no agent is no agent's: default while an agent is active, and "" — so
// ActiveSession() answers as before — once none has spoken for the ambiguity window.
func TestSessionForProcess_AProcessOfNoAgent(t *testing.T) {
	s, clk := newProcStore()
	defer s.Close()
	tool := pchain(700, 60)
	if got := s.SessionForProcess(tool, ""); got != "" {
		t.Fatalf("no agent yet: %q, want \"\"", got)
	}
	s.ClaimProcess("s1", "claude-code", pchain(100, 50))
	recordIn(s, clk, "s1")
	if got := s.SessionForProcess(tool, ""); got != DefaultSessionID {
		t.Errorf("an agent is active: %q, want default", got)
	}
	clk.advance(ambiguityWindow + time.Second)
	if got := s.SessionForProcess(pchain(701, 60), ""); got != "" {
		t.Errorf("no agent for longer than the window: %q, want \"\"", got)
	}
}

func TestSessionForProcess_AReusedPIDIsANewProcess(t *testing.T) {
	s, clk := newProcStore()
	defer s.Close()
	s.ClaimProcess("s1", "claude-code", []Proc{{PID: 100, Start: 1}})
	recordIn(s, clk, "s1")
	if got := s.SessionForProcess([]Proc{{PID: 100, Start: 2}}, ""); got == "s1" {
		t.Error("a new process with a reused pid inherited the old one's session")
	}
}

// A claim is made before the request that carried the header is recorded, so a request
// that arrives in between must not make the store forget it.
func TestSessionForProcess_AClaimSurvivesUntilItsSessionIsRecorded(t *testing.T) {
	s, clk := newProcStore()
	defer s.Close()
	s.ClaimProcess("s1", "claude-code", pchain(100))
	_ = s.SessionForProcess(pchain(300, 100), "")
	recordIn(s, clk, "s1")
	if got := s.SessionForProcess(pchain(301, 100), ""); got != "s1" {
		t.Errorf("after the session was recorded: %q, want s1", got)
	}
}

// Bob runs as `node bob` above `node bob`. Whichever of the two sends the calls before
// the first X-Task-Id, the first session adopts them, and both answer with it after.
func TestClaimProcess_ALauncherAndItsWorkerAreOneAgent(t *testing.T) {
	launcher, worker := pchain(820, 50), pchain(821, 820, 50)
	for name, preHeader := range map[string][]Proc{"launcher": launcher, "worker": worker} {
		t.Run(name, func(t *testing.T) {
			s, clk := newProcStore()
			defer s.Close()
			_ = s.SessionForProcess(launcher, "bob-shell") // the launcher identifies as Bob too
			pending := s.SessionForProcess(preHeader, "bob-shell")
			recordIn(s, clk, pending)
			s.ClaimProcess("task-1", "bob-shell", worker)
			if v := s.View("task-1"); v == nil || len(v.Events) != 1 {
				t.Fatalf("task-1 = %+v, want the adopted pre-header call from %s", v, pending)
			}
			recordIn(s, clk, "task-1")
			for who, c := range map[string][]Proc{"launcher": launcher, "worker": worker} {
				if got := s.SessionForProcess(c, "bob-shell"); got != "task-1" {
					t.Errorf("%s after the claim: %q, want task-1", who, got)
				}
			}
		})
	}
}

func TestSessionForProcess_TheProcessTableIsBounded(t *testing.T) {
	s, _ := newProcStore()
	defer s.Close()
	for pid := int32(2); pid < maxProcs+200; pid++ {
		_ = s.SessionForProcess(pchain(pid), "")
	}
	if n := len(s.procs); n > maxProcs {
		t.Errorf("%d processes held, want at most %d", n, maxProcs)
	}
}

func TestProcessHintsAndIsAgentProcess(t *testing.T) {
	s, clk := newProcStore()
	defer s.Close()
	s.ClaimProcess("s1", "", pchain(500))
	recordIn(s, clk, "s1")
	_ = s.SessionForProcess(pchain(600, 500), "")
	if !s.IsAgentProcess(pchain(500)[0]) || s.IsAgentProcess(pchain(600)[0]) {
		t.Error("IsAgentProcess: want true for the process that named a session, false for its tool")
	}
	if got := s.ProcessHints(8); len(got) != 1 || got[0] != 500 {
		t.Errorf("ProcessHints = %v, want [500]", got)
	}
}

// Per-process pending buckets belong to one agent for SessionForClient's "two agents are
// active" test, exactly like the agent-wide bucket.
func TestSessionForClient_PerProcessPendingBucketsCountAsOneAgent(t *testing.T) {
	s, clk := newProcStore()
	defer s.Close()
	recordIn(s, clk, PendingProcessID("claude-code", 1))
	recordIn(s, clk, PendingProcessID("claude-code", 2))
	if got := s.SessionForClient(""); got != "" {
		t.Errorf("two pending buckets of one agent: %q, want \"\" (one agent)", got)
	}
	recordIn(s, clk, PendingSessionID("bob-shell"))
	if got := s.SessionForClient(""); got != DefaultSessionID {
		t.Errorf("with a second agent: %q, want default", got)
	}
}

// The adoption records used to outlive both sessions they named, for the life of the
// process.
func TestEviction_ForgetsAdoptionRecords(t *testing.T) {
	s := New(0, 0, 2)
	defer s.Close()
	s.Append(PendingSessionID("claude-code"), pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest})
	s.Claim("s1", "claude-code") // adopts the pending bucket
	for _, id := range []string{"s2", "s3"} {
		s.Append(id, pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest})
	}
	if s.View("s1") != nil {
		t.Fatal("s1 was not evicted; the test needs it gone")
	}
	if n := len(s.adopted); n != 0 {
		t.Errorf("%d adoption record(s) left after their session was evicted", n)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `cd core && go test ./session -run 'Process|ProcessHints|PerProcessPending|ForgetsAdoption'`
Expected: build failure — `undefined: Proc`, `undefined: PendingProcessID`, `s.ClaimProcess undefined`, `s.procs undefined`, `undefined: maxProcs`.

- [ ] **Step 3: Write `core/session/process.go`**

```go
package session

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// Process attribution files a request that carries no session header under the session of
// the PROCESS that sent it, rather than by User-Agent (client affinity) or by timing
// (ActiveSession). A listener that can name a connection's process — core/peerproc, on a
// laptop install — passes the client's chain: the client first, then its parents.
// Everything here is inert until a listener calls ClaimProcess or SessionForProcess, which
// only one with process attribution on does.
//
// SessionForProcess walks the chain from the client up and, at each process it knows:
//
//  1. Named a session through its own session header (ClaimProcess): its most recently
//     active live session. A process that never named one — a tool an agent's shell ran —
//     is bound to that answer the first time it is seen, and keeps it for its life.
//  2. An agent's (its requests carry a known coding agent's User-Agent) that has named no
//     session yet: that agent's pending bucket, which the first session it names adopts.
//     Stopping here is what keeps a nested agent — a claude -p run from another session's
//     shell — out of its parent's session.
//  3. Bound earlier: that session.
//
// No agent anywhere in the chain is the default bucket while some process has named a
// session within ambiguityWindow, and "" otherwise, so ActiveSession() answers as before.

// Proc is one process in a request's chain.
type Proc struct {
	PID int32
	// Start is the process's start time in Unix nanoseconds. PIDs are reused; PID and
	// Start together name one process for its lifetime.
	Start int64
	// Exe is the process's executable path, "" when unknown.
	Exe string
}

type procKey struct {
	pid   int32
	start int64
}

func (p Proc) key() procKey { return procKey{pid: p.PID, start: p.Start} }

// procState is what the store knows about one process.
type procState struct {
	// claims are the sessions this process named through its own session header — or a
	// process of the same agent directly below it named (see agentRootIndexLocked) —
	// each with when it was named.
	claims map[string]time.Time
	// bound is the session a process that never named one was filed under the first time
	// it was seen; "" until then.
	bound string
	// agent is the coding agent this process's requests identify as by User-Agent.
	agent string
	// claimed is set the first time the process names a session and never cleared: it is
	// what makes a process an agent's own (IsAgentProcess) even after the sessions it
	// named have been evicted.
	claimed bool
	seen    time.Time
}

// maxProcs caps the process table and procIdleTTL is how long a process the store has not
// heard from is kept. A live agent re-names its session on every headered request, so only
// a process that has gone quiet — almost always one that has exited — ages out.
const (
	maxProcs    = 4096
	procIdleTTL = 24 * time.Hour
)

// claimGrace is how long a claim on a session that has recorded nothing is kept. A claim
// is made at hydration, before the request that carried the header is appended, so a
// concurrent request can see the claim before the session exists.
const claimGrace = time.Minute

// PendingProcessID is the pending bucket of an agent process that has named no session
// yet: the agent's pending bucket, per process, so two windows of one agent starting at
// once do not share one.
func PendingProcessID(agent string, pid int32) string {
	return PendingSessionID(agent) + "@" + strconv.Itoa(int(pid))
}

// pendingOwner is the agent a pending bucket belongs to — claude-code for both
// pending:claude-code and pending:claude-code@4242 — and false for any other id.
func pendingOwner(id string) (string, bool) {
	rest, ok := strings.CutPrefix(id, PendingPrefix)
	if !ok {
		return "", false
	}
	agent, _, _ := strings.Cut(rest, "@")
	return agent, true
}

// ClaimProcess records that chain[0] named sessionID through its own session header, as
// Claim does for an agent. The claim is also recorded on the chain's agent root, so an
// agent that runs as a launcher and a worker answers with one session from both, and the
// root's pending bucket — and chain[0]'s own, if different — is adopted into sessionID
// when sessionID holds nothing yet.
func (s *Store) ClaimProcess(sessionID, agent string, chain []Proc) {
	if sessionID == "" || len(chain) == 0 || strings.HasPrefix(sessionID, PendingPrefix) {
		return
	}
	if len(sessionID) > MaxSessionIDLen {
		sessionID = sessionID[:MaxSessionIDLen]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	s.lastProcClaim = now
	self := s.procLocked(chain[0], now)
	if agent != "" {
		self.agent, self.bound = agent, ""
	}
	root := s.agentRootIndexLocked(chain, self.agent)
	for _, i := range []int{0, root} {
		st := s.procLocked(chain[i], now)
		if st.claims == nil {
			st.claims = make(map[string]time.Time, 1)
		}
		st.claims[sessionID] = now
		st.claimed = true
	}
	if self.agent != "" {
		s.adoptLocked(PendingProcessID(self.agent, chain[root].PID), sessionID)
		if root != 0 {
			s.adoptLocked(PendingProcessID(self.agent, chain[0].PID), sessionID)
		}
	}
}

// SessionForProcess is where a request with no session header from chain[0] goes, in the
// order the comment at the top of this file gives, or "" to fall back to client affinity
// and then ActiveSession(). agent is the coding agent its User-Agent names, "" for none;
// it is remembered for the process, which is how an agent is told from the shell it runs.
func (s *Store) SessionForProcess(chain []Proc, agent string) string {
	if len(chain) == 0 {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	self := s.procLocked(chain[0], now)
	if agent != "" {
		// An agent's process is never a tool bound to another session. Its first request
		// may have been a CONNECT, which carries no User-Agent, and been bound before it
		// said whose it was.
		self.agent, self.bound = agent, ""
	}
	for i, p := range chain {
		st := self
		if i > 0 {
			if st = s.procs[p.key()]; st == nil {
				continue
			}
		}
		if sid := s.newestClaimLocked(st, now); sid != "" {
			if i > 0 && self.agent == "" {
				self.bound = sid
			}
			return sid
		}
		if st.agent != "" {
			root := i + s.agentRootIndexLocked(chain[i:], st.agent)
			return PendingProcessID(st.agent, chain[root].PID)
		}
		if st.bound != "" {
			if s.liveLocked(st.bound, now) {
				if i > 0 && self.agent == "" {
					self.bound = st.bound
				}
				return st.bound
			}
			st.bound = ""
		}
	}
	if !s.lastProcClaim.IsZero() && now.Sub(s.lastProcClaim) <= ambiguityWindow {
		return DefaultSessionID
	}
	return ""
}

// ProcessHints are the PIDs of processes that have named a session, most recently seen
// first and at most limit of them: where a lookup for a new connection looks first.
func (s *Store) ProcessHints(limit int) []int32 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	type seenProc struct {
		pid  int32
		seen time.Time
	}
	var claimers []seenProc
	for k, st := range s.procs {
		if st.claimed {
			claimers = append(claimers, seenProc{pid: k.pid, seen: st.seen})
		}
	}
	sort.Slice(claimers, func(i, j int) bool { return claimers[i].seen.After(claimers[j].seen) })
	if len(claimers) > limit {
		claimers = claimers[:limit]
	}
	out := make([]int32, len(claimers))
	for i, c := range claimers {
		out[i] = c.pid
	}
	return out
}

// IsAgentProcess reports whether p has ever named a session through its own header.
func (s *Store) IsAgentProcess(p Proc) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := s.procs[p.key()]
	return st != nil && st.claimed
}

// procLocked is p's state, created on first sight and stamped as seen now.
func (s *Store) procLocked(p Proc, now time.Time) *procState {
	if s.procs == nil {
		s.procs = make(map[procKey]*procState)
	}
	k := p.key()
	st, ok := s.procs[k]
	if !ok {
		if len(s.procs) >= maxProcs {
			s.pruneProcsLocked(now)
		}
		st = &procState{}
		s.procs[k] = st
	}
	st.seen = now
	return st
}

// pruneProcsLocked makes room in the process table: every process not heard from in
// procIdleTTL, then, if that freed nothing below the cap, the least recently seen quarter.
func (s *Store) pruneProcsLocked(now time.Time) {
	for k, st := range s.procs {
		if now.Sub(st.seen) > procIdleTTL {
			delete(s.procs, k)
		}
	}
	if len(s.procs) < maxProcs {
		return
	}
	type aged struct {
		k    procKey
		seen time.Time
	}
	all := make([]aged, 0, len(s.procs))
	for k, st := range s.procs {
		all = append(all, aged{k: k, seen: st.seen})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].seen.Before(all[j].seen) })
	for _, a := range all[:len(all)/4] {
		delete(s.procs, a.k)
	}
}

// agentRootIndexLocked is the index in chain of the topmost process of agent reached from
// chain[0] through processes of that same agent only. An agent that runs as a launcher
// and a worker (node bob above node bob) has one root; a claude -p started from a shell is
// its own, because the shell between it and the outer claude is no agent's.
func (s *Store) agentRootIndexLocked(chain []Proc, agent string) int {
	if agent == "" {
		return 0
	}
	i := 0
	for i+1 < len(chain) {
		st := s.procs[chain[i+1].key()]
		if st == nil || st.agent != agent {
			break
		}
		i++
	}
	return i
}

// newestClaimLocked is the most recently active live session st has named, "" when none
// is. A claim on a session that is gone is forgotten once claimGrace has passed.
func (s *Store) newestClaimLocked(st *procState, now time.Time) string {
	var best string
	var at time.Time
	for id, claimedAt := range st.claims {
		sess, ok := s.sessions[id]
		if !ok || s.isExpired(sess, now) {
			if now.Sub(claimedAt) > claimGrace {
				delete(st.claims, id)
			}
			continue
		}
		if best == "" || sess.UpdatedAt.After(at) {
			best, at = id, sess.UpdatedAt
		}
	}
	return best
}

func (s *Store) liveLocked(id string, now time.Time) bool {
	sess, ok := s.sessions[id]
	return ok && !s.isExpired(sess, now)
}

// forgetAdoptedLocked drops the adoption records that name id, as the pending bucket or
// as its adopter, once id has left the store. followAdoptedLocked already ignores a record
// whose adopter is gone; this stops them accumulating for the life of the process.
func (s *Store) forgetAdoptedLocked(id string) {
	for pending, to := range s.adopted {
		if pending == id || to == id {
			delete(s.adopted, pending)
		}
	}
}
```

- [ ] **Step 4: Wire it into the store**

In `core/session/store.go`, in `type Store struct`, directly after the `owners`/`adopted` fields and their comment, add:

```go
	// procs and lastProcClaim serve process attribution; see process.go. nil and zero
	// until a listener with it on first asks.
	procs         map[procKey]*procState
	lastProcClaim time.Time
```

In `cleanupLocked`, after `delete(s.owners, id)`, add `s.forgetAdoptedLocked(id)`. In `evictOldestLocked`, after `delete(s.owners, oldestID)`, add `s.forgetAdoptedLocked(oldestID)`.

In `core/session/affinity.go` `SessionForClient`, replace

```go
		owner := s.owners[id]
		if owner == "" {
			owner = strings.TrimPrefix(id, PendingPrefix)
			if owner == id {
				continue
			}
		}
```

with

```go
		owner := s.owners[id]
		if owner == "" {
			var ok bool
			if owner, ok = pendingOwner(id); !ok {
				continue
			}
		}
```

(If `strings` is then unused in `affinity.go`, the compiler says so; keep the import only if something else uses it.)

- [ ] **Step 5: Run the tests**

Run: `cd core && go test -race ./session`
Expected: `ok` — the new tests and every existing one.

- [ ] **Step 6: Commit**

```bash
gofmt -l core/session   # must print nothing
git add core/session/process.go core/session/process_test.go core/session/store.go core/session/affinity.go
git commit -s -F - <<'EOF'
feat: Track which process named which session in the session store

ClaimProcess records the process behind a headered request; for a
request with no header, SessionForProcess walks the client's process
chain to the nearest process that named a session, stops at an agent
process that has named none (a per-process pending bucket its first
session adopts), and sends a process of no agent to default while an
agent is active. Keyed by pid and start time, bounded, and inert until
a listener calls it. Adoption records are now forgotten with their
session.

Part of #1187.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
EOF
```

---

### Task 2: The forward proxy looks the client up and files by process

**Files:**
- Create: `core/listener/forwardproxy/process.go`
- Create: `core/listener/forwardproxy/process_test.go`
- Modify: `core/listener/forwardproxy/server.go` — `Server` (new field `Processes`), `bridgedHandler`, `serveOutbound`, `resolveOutboundSessionID`, `recordingSessionID`, `tunnelSessionID`, `resolvePluginSessionID`, `handleConnect`, `tunnelLog` (new field `conn`)
- Modify: `core/listener/forwardproxy/transparent.go` — the two resolver calls and `appendTunnelOpen`'s condition
- Modify (call sites only): `core/listener/forwardproxy/session_header_test.go`, `session_affinity_test.go`, `server_test.go`

**Interfaces:**
- Consumes (Task 1): `session.Proc`, `(*session.Store).ClaimProcess`, `SessionForProcess`, `ProcessHints`; (#1229) `peerproc.Resolver`, `peerproc.Proc`, `peerproc.ErrNotFound`.
- Produces (used by Tasks 3–4):
  - field `Server.Processes peerproc.Resolver`
  - `func (s *Server) ConnContext(ctx context.Context, c net.Conn) context.Context`
  - `func (s *Server) clientChain(r *http.Request) []session.Proc`, `func (s *Server) processesOn() bool`, `func connProcOf(ctx context.Context) *connProc`, `type connProc`, `const maxHints = 8`
  - resolver signatures with a trailing `chain []session.Proc`: `resolvePluginSessionID(h, chain)`, `resolveOutboundSessionID(h, chain)`, `recordingSessionID(resolved, h, chain)`, `tunnelSessionID(resolved, h, chain)`
  - test helpers in `process_test.go`: `fakeProcs` (`fproc`, `newFakeProcs`, `clientFor`, `own`, `listens`, `lookupCount`), `newProcessProxy`, `sendAs`, `connectAs`, `recordedPaths`, `eventually`

- [ ] **Step 1: Write the failing tests**

Create `core/listener/forwardproxy/process_test.go`:

```go
package forwardproxy

import (
	"bufio"
	"context"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/peerproc"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/plugintesting"
	"github.com/rossoctl/cortex/core/session"
)

// Tests for #1187's process attribution: a request with no session header is filed under
// the session of the process that sent it, looked up once per client connection.

const procClaudeUA = "claude-cli/2.1.286 (external, cli)"

// fakeProcs is a peerproc.Resolver over a made-up process table, so one test process can
// play several. A connection belongs to whichever pid the client that dialled it was made
// for (clientFor, connectAs); a listener to whichever pid listens registers.
type fakeProcs struct {
	mu        sync.Mutex
	procs     map[int32]peerproc.Proc
	byPort    map[uint16]int32
	listeners map[uint16]int32
	lookups   int
}

func fproc(pid, ppid int32, exe string) peerproc.Proc {
	return peerproc.Proc{PID: pid, PPID: ppid, Start: time.Unix(int64(pid), 0), Exe: exe}
}

func newFakeProcs(procs ...peerproc.Proc) *fakeProcs {
	f := &fakeProcs{procs: map[int32]peerproc.Proc{}, byPort: map[uint16]int32{}, listeners: map[uint16]int32{}}
	for _, p := range procs {
		f.procs[p.PID] = p
	}
	return f
}

func (f *fakeProcs) ConnOwner(client, _ netip.AddrPort, _ ...int32) (peerproc.Proc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	pid, ok := f.byPort[client.Port()]
	if !ok {
		return peerproc.Proc{}, peerproc.ErrNotFound
	}
	return f.procs[pid], nil
}

func (f *fakeProcs) ListenerOwner(addr netip.AddrPort, _ ...int32) (peerproc.Proc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pid, ok := f.listeners[addr.Port()]
	if !ok {
		return peerproc.Proc{}, peerproc.ErrNotFound
	}
	return f.procs[pid], nil
}

func (f *fakeProcs) Ancestry(pid int32, max int) ([]peerproc.Proc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []peerproc.Proc
	for pid > 1 && len(out) < max {
		p, ok := f.procs[pid]
		if !ok {
			break
		}
		out = append(out, p)
		pid = p.PPID
	}
	if len(out) == 0 {
		return nil, peerproc.ErrNotFound
	}
	return out, nil
}

// own registers c's local port as pid's.
func (f *fakeProcs) own(c net.Conn, pid int32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byPort[uint16(c.LocalAddr().(*net.TCPAddr).Port)] = pid
}

// listens registers the port of rawURL's host as pid's listener.
func (f *fakeProcs) listens(t *testing.T, rawURL string, pid int32) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listeners[uint16(portOf(u.Host))] = pid
}

func (f *fakeProcs) lookupCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookups
}

// clientFor is an HTTP client whose every connection to the proxy belongs to pid.
func (f *fakeProcs) clientFor(proxyURL string, pid int32) *http.Client {
	dialer := &net.Dialer{}
	return &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(mustParseURL(proxyURL)),
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := dialer.DialContext(ctx, network, addr)
			if err == nil {
				f.own(c, pid)
			}
			return c, err
		},
	}}
}

// newProcessProxy is a forward proxy with process attribution over procs, served by an
// http.Server carrying its ConnContext, as cortex wires it. configure, when non-nil, runs
// before it serves; backendHits counts what the backend received.
func newProcessProxy(t *testing.T, store *session.Store, procs *fakeProcs, configure func(*Server)) (proxyURL, backendURL string, backendHits *atomic.Int32) {
	t.Helper()
	backendHits = &atomic.Int32{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)
	p, err := plugintesting.BuildPipeline(nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		Client:           http.DefaultClient,
		SessionIDHeaders: []string{session.ClaudeCodeSessionHeader, session.BobSessionHeader},
		ClientAffinity:   true,
		Processes:        procs,
	}
	if configure != nil {
		configure(srv)
	}
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.Config.ConnContext = srv.ConnContext
	ts.Start()
	t.Cleanup(ts.Close)
	return ts.URL, backend.URL, backendHits
}

func sendAs(t *testing.T, c *http.Client, rawURL, ua, header, sid string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if header != "" {
		req.Header.Set(header, sid)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", rawURL, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// connectAs is sendConnect from pid: the connection's local port is registered to pid
// before the CONNECT is written.
func connectAs(t *testing.T, procs *fakeProcs, proxyURL, target string, pid int32) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	raw, err := net.Dial("tcp", strings.TrimPrefix(proxyURL, "http://"))
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	procs.own(raw, pid)
	_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(raw, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(raw)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	return raw, br, resp
}

// recordedPaths is the path of every request event recorded under id, in order.
func recordedPaths(store *session.Store, id string) []string {
	v := store.View(id)
	if v == nil {
		return nil
	}
	var out []string
	for _, e := range v.Events {
		if e.Phase == pipeline.SessionRequest && !e.Tunnel {
			out = append(out, e.HTTPPath)
		}
	}
	return out
}

func eventually(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The case this exists for. Client affinity alone files the tool's call by User-Agent —
// curl is no agent, so ActiveSession() — and files the unrelated script there too.
func TestProcessAttribution_AToolJoinsItsAgentsSessionAndAStrangerDoesNot(t *testing.T) {
	procs := newFakeProcs(
		fproc(100, 50, "/bin/claude"), fproc(200, 100, "/bin/bash"), fproc(300, 200, "/usr/bin/curl"),
		fproc(700, 60, "/usr/bin/python3"),
	)
	store := session.New(0, 0, 0)
	defer store.Close()
	proxyURL, backendURL, _ := newProcessProxy(t, store, procs, nil)

	sendAs(t, procs.clientFor(proxyURL, 100), backendURL+"/v1/messages", procClaudeUA, session.ClaudeCodeSessionHeader, "s1")
	sendAs(t, procs.clientFor(proxyURL, 300), backendURL+"/tool", "curl/8.7.1", "", "")
	sendAs(t, procs.clientFor(proxyURL, 700), backendURL+"/elsewhere", "python-requests/2.32", "", "")

	if got := strings.Join(recordedPaths(store, "s1"), ","); got != "/v1/messages,/tool" {
		t.Errorf("s1 = %s, want the agent's request and its tool's", got)
	}
	if got := strings.Join(recordedPaths(store, session.DefaultSessionID), ","); got != "/elsewhere" {
		t.Errorf("default = %s, want the request from a process of no agent", got)
	}
}

// A client the lookup cannot name falls back to client affinity, exactly as without it.
func TestProcessAttribution_FallsBackWhenTheClientCannotBeLookedUp(t *testing.T) {
	store := session.New(0, 0, 0)
	defer store.Close()
	proxyURL, backendURL, _ := newProcessProxy(t, store, newFakeProcs(), nil)
	plain := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxyURL))}}

	sendAs(t, plain, backendURL+"/admin/v1/profile", "bob-shell/2.0.5", "", "")
	if store.View(session.PendingSessionID("bob-shell")) == nil {
		t.Error("an unknown client's Bob call did not reach client affinity's pending:bob-shell")
	}
}

func TestProcessAttribution_LooksEachConnectionUpOnce(t *testing.T) {
	procs := newFakeProcs(fproc(100, 50, "/bin/claude"))
	store := session.New(0, 0, 0)
	defer store.Close()
	proxyURL, backendURL, _ := newProcessProxy(t, store, procs, nil)
	claude := procs.clientFor(proxyURL, 100)
	for i := 0; i < 3; i++ {
		sendAs(t, claude, backendURL+"/v1/messages", procClaudeUA, session.ClaudeCodeSessionHeader, "s1")
	}
	if n := procs.lookupCount(); n != 1 {
		t.Errorf("three requests on one connection took %d lookups, want 1", n)
	}
}

// An opaque tunnel — git to GitHub, which cannot be bridged — has no decrypted request to
// take a session from; its process names it.
func TestProcessAttribution_AnOpaqueTunnelJoinsItsProcessSession(t *testing.T) {
	procs := newFakeProcs(fproc(100, 50, "/bin/claude"), fproc(200, 100, "/bin/bash"), fproc(310, 200, "/usr/bin/git"))
	store := session.New(0, 0, 0)
	defer store.Close()
	proxyURL, backendURL, _ := newProcessProxy(t, store, procs, nil)
	sendAs(t, procs.clientFor(proxyURL, 100), backendURL+"/v1/messages", procClaudeUA, session.ClaudeCodeSessionHeader, "s1")

	raw, br, resp := connectAs(t, procs, proxyURL, pingPongOrigin(t), 310)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status %d", resp.StatusCode)
	}
	pingPong(t, raw, br)
	eventually(t, func() bool { _, closes := tunnelRows(store, "s1"); return len(closes) == 1 }, "the tunnel's close row in s1")
	onePair(t, store, "s1")
	if v := store.View(session.DefaultSessionID); v != nil {
		t.Errorf("default holds %d event(s); the tunnel belongs to s1", len(v.Events))
	}
}

// The requests a bridged CONNECT decrypts come from the CONNECT's client; they reuse its
// lookup rather than making their own, which the inner server could not anyway. Bob is
// active too, so a request with no process answer would land in default, not in s1.
func TestProcessAttribution_ABridgedTunnelsRequestsUseItsProcess(t *testing.T) {
	procs := newFakeProcs(
		fproc(100, 50, "/bin/claude"), fproc(200, 100, "/bin/bash"), fproc(300, 200, "/usr/bin/curl"),
		fproc(800, 60, "/usr/bin/node"),
	)
	store := session.New(0, 0, 0)
	defer store.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	t.Cleanup(origin.Close)
	originCA := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: origin.Certificate().Raw})
	u, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	engine := bridgeEngine(t, portOf(u.Host), originCA)
	proxyURL, backendURL, _ := newProcessProxy(t, store, procs, func(s *Server) { s.TLSBridge = engine })
	sendAs(t, procs.clientFor(proxyURL, 100), backendURL+"/v1/messages", procClaudeUA, session.ClaudeCodeSessionHeader, "s1")
	sendAs(t, procs.clientFor(proxyURL, 800), backendURL+"/inference", "bob-shell/2.0.5", session.BobSessionHeader, "task-1")

	before := procs.lookupCount()
	raw, br, _ := connectAs(t, procs, proxyURL, u.Host, 300)
	tc := bridgedTLS(t, raw, br, u.Host, engine.CAPEM)
	for _, path := range []string{"/a", "/b"} {
		req, _ := http.NewRequest(http.MethodGet, "https://"+hostOnly(u.Host)+path, nil)
		req.Header.Set("User-Agent", "curl/8.7.1")
		bridgedRoundTrip(t, tc, req)
	}
	_ = tc.Close()

	if n := procs.lookupCount() - before; n != 1 {
		t.Errorf("the CONNECT and its two requests took %d lookups, want 1", n)
	}
	eventually(t, func() bool { return len(recordedPaths(store, "s1")) == 3 }, "both decrypted requests in s1")
	if got := strings.Join(recordedPaths(store, "s1"), ","); got != "/v1/messages,/a,/b" {
		t.Errorf("s1 = %s, want the agent's request and the curl tunnel's two", got)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `cd core && go test ./listener/forwardproxy -run TestProcessAttribution`
Expected: build failure — `unknown field Processes in struct literal of type Server`, `srv.ConnContext undefined`.

- [ ] **Step 3: Write `core/listener/forwardproxy/process.go`**

```go
package forwardproxy

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sync"

	"github.com/rossoctl/cortex/core/peerproc"
	"github.com/rossoctl/cortex/core/session"
)

// maxChain is how many processes a client's chain holds — the client and its parents —
// enough to reach the agent from a tool its shell ran.
const maxChain = 16

// maxHints is how many processes known to have named a session a lookup looks at first.
// On Linux each costs a scan of that process's open files.
const maxHints = 8

type connProcKey struct{}

// connProc is one client connection's process chain. It is looked up on the connection's
// first request — while the client is certainly connected, since a process that has exited
// cannot be looked up — and reused by every later request on it, including those a bridged
// CONNECT decrypts.
type connProc struct {
	once  sync.Once
	chain []session.Proc
}

// ConnContext is the forward proxy's http.Server ConnContext. It gives each client
// connection an empty slot for its process chain and nothing else: it runs on the accept
// loop, so the lookup waits for the connection's first request.
func (s *Server) ConnContext(ctx context.Context, _ net.Conn) context.Context {
	if s.Processes == nil {
		return ctx
	}
	return context.WithValue(ctx, connProcKey{}, &connProc{})
}

func connProcOf(ctx context.Context) *connProc {
	cp, _ := ctx.Value(connProcKey{}).(*connProc)
	return cp
}

// processesOn reports whether header-less requests are filed by client process. Like
// affinityOn it needs header bucketing: a process's session is the one its header named.
func (s *Server) processesOn() bool {
	return s.Processes != nil && s.Sessions != nil && len(s.SessionIDHeaders) > 0
}

// clientChain is the process chain behind r's connection — the client first, then its
// parents — or nil when process attribution is off or the client could not be named, and
// then resolution is exactly as without it.
func (s *Server) clientChain(r *http.Request) []session.Proc {
	if !s.processesOn() {
		return nil
	}
	cp := connProcOf(r.Context())
	if cp == nil {
		return nil
	}
	cp.once.Do(func() { cp.chain = s.lookupChain(r) })
	return cp.chain
}

func (s *Server) lookupChain(r *http.Request) []session.Proc {
	client, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return nil
	}
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return nil
	}
	server, err := netip.ParseAddrPort(local.String())
	if err != nil {
		return nil
	}
	owner, err := s.Processes.ConnOwner(client, server, s.Sessions.ProcessHints(maxHints)...)
	if err != nil {
		slog.Debug("forward-proxy: client process not found, filing as without process attribution",
			"client", r.RemoteAddr, "error", err)
		return nil
	}
	procs, err := s.Processes.Ancestry(owner.PID, maxChain)
	if err != nil || len(procs) == 0 {
		procs = []peerproc.Proc{owner}
	}
	chain := make([]session.Proc, len(procs))
	for i, p := range procs {
		chain[i] = session.Proc{PID: p.PID, Start: p.Start.UnixNano(), Exe: p.Exe}
	}
	return chain
}
```

- [ ] **Step 4: Thread the chain through `server.go` and `transparent.go`**

In `core/listener/forwardproxy/server.go`:

1. Add `"github.com/rossoctl/cortex/core/peerproc"` to the imports, and to `type Server`, directly after the `ClientAffinity bool` field:

```go
	// Processes, when non-nil, names the process behind each client connection, and a
	// request with no session header is filed under that process's session — or its
	// nearest ancestor's — before client affinity is consulted; see
	// session.Store.SessionForProcess. Set only where every client is a process on this
	// host (session.process_attribution) and only from a resolver whose self-test passed
	// (peerproc.New). The http.Server must carry ConnContext. nil keeps today's
	// resolution byte for byte.
	Processes peerproc.Resolver
```

2. In `type tunnelLog struct`, directly after `skipped bool`, add:

```go
	// conn is the CONNECT's client process slot, handed to the requests a bridged tunnel
	// decrypts; nil off the CONNECT path.
	conn *connProc
```

3. In `bridgedHandler`, directly after `defer tl.release()`, add:

```go
		if tl.conn != nil {
			r = r.WithContext(context.WithValue(r.Context(), connProcKey{}, tl.conn))
		}
```

4. In `serveOutbound`, directly after the `if skipped { slog.Info("forward-proxy: skip_hosts match …) }` block, add:

```go
	// The client's process, looked up once per connection; nil when process attribution
	// is off or the lookup failed, and then resolution is exactly as without it.
	var chain []session.Proc
	if !skipped {
		chain = s.clientChain(r)
	}
```

and change its three resolver calls: `s.resolvePluginSessionID(r.Header)` → `s.resolvePluginSessionID(r.Header, chain)`; both `s.recordingSessionID(sessionID, r.Header)` → `s.recordingSessionID(sessionID, r.Header, chain)`.

5. Replace `resolveOutboundSessionID`, `recordingSessionID` and `tunnelSessionID`'s signatures and bodies (keep their doc comments, adding the sentence shown to `tunnelSessionID`'s):

```go
func (s *Server) resolveOutboundSessionID(clientHeaders http.Header, chain []session.Proc) string {
	if sid := s.resolvePluginSessionID(clientHeaders, chain); sid != "" {
		return sid
	}
	return session.DefaultSessionID
}

func (s *Server) recordingSessionID(resolved string, clientHeaders http.Header, chain []session.Proc) string {
	if resolved != "" {
		return resolved
	}
	return s.resolveOutboundSessionID(clientHeaders, chain)
}

// … existing tunnelSessionID comment, then:
// With the client's process known, an empty answer means a process of no agent, and the
// row goes to default like that process's requests.
func (s *Server) tunnelSessionID(resolved string, clientHeaders http.Header, chain []session.Proc) string {
	if s.ClientAffinity && resolved == "" && chain == nil && s.Sessions != nil {
		if sid := s.Sessions.ActiveSession(); sid != "" {
			return sid
		}
		return session.DefaultSessionID
	}
	return s.recordingSessionID(resolved, clientHeaders, chain)
}
```

6. Replace `resolvePluginSessionID`'s signature and body (keep its doc comment; append the paragraph shown):

```go
// With the client's process known (chain non-nil), a header claims the session for that
// process as well as for its agent, and a request with no header asks
// session.Store.SessionForProcess first; client affinity and ActiveSession() answer only
// what it leaves open.
func (s *Server) resolvePluginSessionID(clientHeaders http.Header, chain []session.Proc) string {
	affinity := s.affinityOn()
	procs := chain != nil && s.processesOn()
	agent := affinityClient(clientHeaders)
	if sid := session.IDFromHeaders(clientHeaders, s.SessionIDHeaders); sid != "" {
		if affinity {
			s.Sessions.Claim(sid, agent)
		}
		if procs {
			s.Sessions.ClaimProcess(sid, agent, chain)
		}
		return sid
	}
	if procs {
		// The default bucket is SessionForProcess's answer for a process of no agent while
		// an agent is active, and plugins hear it as "", as they do affinity's below.
		switch sid := s.Sessions.SessionForProcess(chain, agent); sid {
		case "":
		case session.DefaultSessionID:
			return ""
		default:
			return sid
		}
	}
	if affinity {
		// See session.Store.SessionForClient for the order. The default bucket is its
		// "ambiguous" answer, and plugins hear that as "" — the no-identity answer this
		// function exists to give them — while recording files it under default.
		switch sid := s.Sessions.SessionForClient(agent); sid {
		case "":
		case session.DefaultSessionID:
			return ""
		default:
			return sid
		}
	}
	if s.Sessions != nil {
		if sid := s.Sessions.ActiveSession(); sid != "" {
			return sid
		}
	}
	return ""
}
```

7. In `handleConnect`: directly before `if !skipped {` (the block containing the `defer … RunFinish`), add `var chain []session.Proc`; inside that block, before `if s.Sessions != nil {`, add `chain = s.clientChain(r)`; change `s.resolvePluginSessionID(r.Header)` → `s.resolvePluginSessionID(r.Header, chain)` and `s.tunnelSessionID(sessionID, r.Header)` → `s.tunnelSessionID(sessionID, r.Header, chain)`. Replace the pin

```go
	if s.ClientAffinity && !skipped && s.Sessions != nil && sessionID != "" {
		pctx.OutboundSessionID = sessionID
	}
```

with

```go
	if !skipped && s.Sessions != nil {
		switch {
		case chain != nil && s.processesOn():
			// The client's process is known, so the tunnel's own row goes where that
			// process's requests go — default included, for a process of no agent.
			pctx.OutboundSessionID = s.recordingSessionID(sessionID, r.Header, chain)
		case s.ClientAffinity && sessionID != "":
			pctx.OutboundSessionID = sessionID
		}
	}
```

and, directly after `tl := s.newTunnelLog(pctx, skipped)`, add `tl.conn = connProcOf(r.Context())`. Extend the comment above the pin by one sentence: `With process attribution the pin is the process's answer, which is never empty.`

In `core/listener/forwardproxy/transparent.go`: `s.resolvePluginSessionID(nil)` → `s.resolvePluginSessionID(nil, nil)`; `s.tunnelSessionID(sessionID, nil)` → `s.tunnelSessionID(sessionID, nil, nil)`; and in `appendTunnelOpen` change `if s.ClientAffinity {` to `if s.ClientAffinity || s.processesOn() {`, adding to the comment above it: `Process attribution pins it too, with the client process's answer.`

In `session_header_test.go`, `session_affinity_test.go` and `server_test.go`: append `, nil` as the last argument of every call to `resolvePluginSessionID`, `resolveOutboundSessionID`, `recordingSessionID` and `tunnelSessionID` (19 call sites; `grep -n` the four names to find them). Change nothing else in those files.

- [ ] **Step 5: Run the tests**

Run: `cd core && go test -race ./listener/forwardproxy ./session`
Expected: `ok` — the new tests and every existing one (attribution off is today's behaviour, so nothing existing changes).

- [ ] **Step 6: Commit**

```bash
gofmt -l core/listener/forwardproxy   # must print nothing
git add core/listener/forwardproxy/process.go core/listener/forwardproxy/process_test.go core/listener/forwardproxy/server.go core/listener/forwardproxy/transparent.go core/listener/forwardproxy/session_header_test.go core/listener/forwardproxy/session_affinity_test.go core/listener/forwardproxy/server_test.go
git commit -s -F - <<'EOF'
feat: File header-less forward-proxy traffic by the client's process

With Server.Processes set, the first request on each client connection
looks the client's process chain up (peerproc), and every request on
the connection — including those a bridged CONNECT decrypts — is filed
by it: a header claims the session for the process, and a request
without one asks the session store which session its process, or the
nearest ancestor that named one, belongs to. An opaque tunnel's rows
follow the same answer. Unset, resolution is unchanged.

Part of #1187.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
EOF
```

---

### Task 3: An agent's traffic to its own service is not recorded

**Files:**
- Modify: `core/listener/forwardproxy/process.go` — `connProc` gains a listener cache; new `selfTraffic`, `listenerBehind`, `loopbackDests`, `noteSelfTraffic`
- Modify: `core/listener/forwardproxy/server.go` — `Server` gains `selfTrafficSeen sync.Map`; `serveOutbound` calls `selfTraffic`
- Modify: `core/listener/forwardproxy/process_test.go` — three tests

**Interfaces:**
- Consumes (Task 1): `(*session.Store).IsAgentProcess`, `ProcessHints`; (Task 2): `connProc`, `connProcOf`, `clientChain`, `maxHints`, the `process_test.go` helpers.
- Produces: `func (s *Server) selfTraffic(r *http.Request, chain []session.Proc) bool`.

- [ ] **Step 1: Write the failing tests**

Append to `core/listener/forwardproxy/process_test.go`:

```go
const opencodeExe = "/Users/x/.opencode/bin/opencode"

// countPath is how many request events with path were recorded, in any session.
func countPath(store *session.Store, path string) int {
	n := 0
	for _, sum := range store.ListSessions() {
		for _, p := range recordedPaths(store, sum.ID) {
			if p == path {
				n++
			}
		}
	}
	return n
}

// OpenCode's TUI polls its own background service through the proxy. Once the service has
// named a session — so it is an agent's own process — and runs the TUI's executable, that
// polling is forwarded without a row.
func TestSelfTraffic_AnAgentPollingItsOwnServiceIsForwardedButNotRecorded(t *testing.T) {
	procs := newFakeProcs(fproc(500, 1, opencodeExe), fproc(510, 60, opencodeExe))
	store := session.New(0, 0, 0)
	defer store.Close()
	proxyURL, backendURL, hits := newProcessProxy(t, store, procs, func(s *Server) {
		s.SessionIDHeaders = append(s.SessionIDHeaders, "X-Opencode-Session-Id")
	})
	procs.listens(t, backendURL, 500) // the service listens where the TUI polls
	tui, svc := procs.clientFor(proxyURL, 510), procs.clientFor(proxyURL, 500)

	sendAs(t, tui, backendURL+"/api/info", "opencode/latest/2.0.21/cli", "", "")
	sendAs(t, svc, backendURL+"/zen/v1/chat/completions", "opencode/latest/2.0.21/cli", "X-Opencode-Session-Id", "ses_1")
	sendAs(t, tui, backendURL+"/api/info", "opencode/latest/2.0.21/cli", "", "")

	if got := hits.Load(); got != 3 {
		t.Errorf("the backend received %d requests, want all 3 forwarded", got)
	}
	if n := countPath(store, "/api/info"); n != 1 {
		t.Errorf("%d /api/info rows recorded; want only the one before the service named a session", n)
	}
	if got := strings.Join(recordedPaths(store, "ses_1"), ","); got != "/zen/v1/chat/completions" {
		t.Errorf("ses_1 = %s, want the service's inference request", got)
	}
}

// Bob runs under node, and so does the local MCP server it calls. Same executable — but the
// server never names a session, so it is no agent's and its traffic stays visible.
func TestSelfTraffic_ANodeAgentsCallsToALocalNodeServerAreRecorded(t *testing.T) {
	procs := newFakeProcs(fproc(900, 60, "/usr/bin/node"), fproc(910, 60, "/usr/bin/node"))
	store := session.New(0, 0, 0)
	defer store.Close()
	proxyURL, backendURL, _ := newProcessProxy(t, store, procs, nil)
	procs.listens(t, backendURL, 910)
	bob := procs.clientFor(proxyURL, 900)

	sendAs(t, bob, backendURL+"/inference", "bob-shell/2.0.5", session.BobSessionHeader, "task-1")
	sendAs(t, bob, backendURL+"/mcp", "bob-shell/2.0.5", "", "")
	if got := strings.Join(recordedPaths(store, "task-1"), ","); got != "/inference,/mcp" {
		t.Errorf("task-1 = %s, want the MCP call recorded beside the inference", got)
	}
}

// Another program calling an agent's service — curl, agentop — is not the agent talking to
// itself, and is recorded.
func TestSelfTraffic_AnotherProgramCallingTheServiceIsRecorded(t *testing.T) {
	procs := newFakeProcs(fproc(500, 1, opencodeExe), fproc(520, 60, "/usr/bin/curl"))
	store := session.New(0, 0, 0)
	defer store.Close()
	proxyURL, backendURL, _ := newProcessProxy(t, store, procs, func(s *Server) {
		s.SessionIDHeaders = append(s.SessionIDHeaders, "X-Opencode-Session-Id")
	})
	procs.listens(t, backendURL, 500)
	sendAs(t, procs.clientFor(proxyURL, 500), backendURL+"/zen/v1/chat/completions", "", "X-Opencode-Session-Id", "ses_1")
	sendAs(t, procs.clientFor(proxyURL, 520), backendURL+"/api/info", "curl/8.7.1", "", "")
	if n := countPath(store, "/api/info"); n != 1 {
		t.Errorf("curl's call to the service: %d rows, want 1", n)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `cd core && go test ./listener/forwardproxy -run TestSelfTraffic`
Expected: `TestSelfTraffic_AnAgentPollingItsOwnServiceIsForwardedButNotRecorded` FAILS (`2 /api/info rows recorded`); the other two pass already (they pin what must not change).

- [ ] **Step 3: Implement**

In `core/listener/forwardproxy/process.go`, add `"strconv"`, `"strings"` and `"time"` to the imports and give `connProc` a listener cache — the struct becomes:

```go
type connProc struct {
	once  sync.Once
	chain []session.Proc

	// mu guards listeners: which process listens behind each loopback destination this
	// connection's requests went to, for selfTraffic.
	mu        sync.Mutex
	listeners map[netip.AddrPort]listenerSeen
}

// listenerSeen is one cached ListenerOwner answer.
type listenerSeen struct {
	proc  peerproc.Proc
	found bool
	at    time.Time
}

// listenerTTL is how long a connection trusts a cached listener answer. Short, because a
// service can restart; whether the listener is an agent's is re-asked on every request,
// since a service becomes one only when it first names a session.
const listenerTTL = 30 * time.Second
```

and append:

```go
// selfTraffic reports whether r is an agent talking to its own service on this host: a
// plain-HTTP request to a loopback listener held by an agent's own process — one that has
// named a session through its own header, which an MCP or model server never does —
// running the same executable as the client. OpenCode's TUI polling its background service
// is the case it exists for: about 1.3 requests a second of the agent's own UI, sent
// nowhere. Comparing executables alone would hide a node-based agent's calls to any local
// node server, which is why the listener must be an agent's.
func (s *Server) selfTraffic(r *http.Request, chain []session.Proc) bool {
	if len(chain) == 0 || chain[0].Exe == "" {
		return false
	}
	cp := connProcOf(r.Context())
	if cp == nil {
		return false
	}
	for _, dest := range loopbackDests(r) {
		l, ok := s.listenerBehind(cp, dest)
		if !ok {
			continue
		}
		if l.Exe != chain[0].Exe || !s.Sessions.IsAgentProcess(session.Proc{PID: l.PID, Start: l.Start.UnixNano()}) {
			return false
		}
		s.noteSelfTraffic(chain[0].Exe, dest, l.PID)
		return true
	}
	return false
}

func (s *Server) listenerBehind(cp *connProc, dest netip.AddrPort) (peerproc.Proc, bool) {
	now := time.Now()
	cp.mu.Lock()
	seen, ok := cp.listeners[dest]
	cp.mu.Unlock()
	if ok && now.Sub(seen.at) < listenerTTL {
		return seen.proc, seen.found
	}
	p, err := s.Processes.ListenerOwner(dest, s.Sessions.ProcessHints(maxHints)...)
	seen = listenerSeen{proc: p, found: err == nil, at: now}
	cp.mu.Lock()
	if cp.listeners == nil {
		cp.listeners = make(map[netip.AddrPort]listenerSeen, 1)
	}
	cp.listeners[dest] = seen
	cp.mu.Unlock()
	return seen.proc, seen.found
}

// loopbackDests is where r goes when that is this host: its address when the URL names a
// loopback one, both loopbacks for "localhost" — the proxy has not dialled yet, so which
// one the name resolves to is not known — and nothing otherwise.
func loopbackDests(r *http.Request) []netip.AddrPort {
	host, port := r.URL.Hostname(), r.URL.Port()
	if port == "" {
		port = "80"
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return nil
	}
	if strings.EqualFold(host, "localhost") {
		return []netip.AddrPort{
			netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), uint16(n)),
			netip.AddrPortFrom(netip.IPv6Loopback(), uint16(n)),
		}
	}
	a, err := netip.ParseAddr(host)
	if err != nil || !a.IsLoopback() {
		return nil
	}
	return []netip.AddrPort{netip.AddrPortFrom(a.Unmap(), uint16(n))}
}

// noteSelfTraffic says once, at INFO, which program's traffic to which local service is
// not being recorded — a row that silently stops appearing is the thing nobody can debug —
// and every time at DEBUG.
func (s *Server) noteSelfTraffic(exe string, dest netip.AddrPort, pid int32) {
	if _, seen := s.selfTrafficSeen.LoadOrStore(exe+" "+dest.String(), struct{}{}); !seen {
		slog.Info("forward-proxy: not recording an agent's traffic to its own service on this host",
			"exe", exe, "service", dest.String(), "service_pid", pid)
	}
	slog.Debug("forward-proxy: agent self-traffic forwarded without recording", "exe", exe, "service", dest.String())
}
```

In `core/listener/forwardproxy/server.go`, add to `type Server`, after `bufferedFallbackOnce sync.Once`:

```go
	// selfTrafficSeen holds the (executable, service) pairs selfTraffic has announced.
	selfTrafficSeen sync.Map
```

and in `serveOutbound`, directly after the block Task 2 added (`chain = s.clientChain(r)`), add:

```go
	// An agent talking to its own service on this host is forwarded the way a skip_hosts
	// destination is: no pipeline, no row. See selfTraffic.
	if !skipped && !isBridge && s.selfTraffic(r, chain) {
		skipped = true
	}
```

- [ ] **Step 4: Run the tests**

Run: `cd core && go test -race ./listener/forwardproxy`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
gofmt -l core/listener/forwardproxy   # must print nothing
git add core/listener/forwardproxy/process.go core/listener/forwardproxy/process_test.go core/listener/forwardproxy/server.go
git commit -s -F - <<'EOF'
feat: Stop recording an agent's traffic to its own local service

A plain-HTTP request to a loopback listener is forwarded without a row
when the listener is an agent's own process — one that has named a
session — running the client's executable: OpenCode's TUI polling its
background service. Requiring the listener to be an agent's keeps a
node-based agent's calls to a local node MCP server visible. Announced
once per program and service at INFO.

Part of #1187.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
EOF
```

---

### Task 4: The setting, the server hook, and the wiring

**Files:**
- Modify: `core/config/config.go` — `SessionConfig.ProcessAttribution`, constants, `ProcessAttributionEnabled`
- Modify: `core/config/validate.go` — `validateSession`, called from `Validate`
- Create: `core/config/process_attribution_test.go`
- Modify: `core/bootstrap/bootstrap.go` — `ServerOption`, `WithConnContext`, `newHTTPServer`, `StartHTTPServer(..., opts ...ServerOption)`
- Modify: `core/bootstrap/bootstrap_test.go` — one test
- Modify: `cmd/cortex/main.go` — create the resolver, set `fpSrv.Processes`, pass `ConnContext`

**Interfaces:**
- Consumes (Task 2): `Server.Processes`, `(*Server).ConnContext`; (#1229) `peerproc.New`.
- Produces: `config.ProcessAttributionAuto/On/Off`, `(SessionConfig).ProcessAttributionEnabled(loopbackOnly bool) bool`, `bootstrap.ServerOption`, `bootstrap.WithConnContext`.

- [ ] **Step 1: Write the failing tests**

Create `core/config/process_attribution_test.go`:

```go
package config

import (
	"strings"
	"testing"
)

func TestProcessAttributionEnabled(t *testing.T) {
	for _, tc := range []struct {
		value        string
		loopbackOnly bool
		want         bool
	}{
		{"", true, true}, {"", false, false},
		{ProcessAttributionAuto, true, true}, {ProcessAttributionAuto, false, false},
		{ProcessAttributionOn, false, true}, {ProcessAttributionOff, true, false},
	} {
		got := SessionConfig{ProcessAttribution: tc.value}.ProcessAttributionEnabled(tc.loopbackOnly)
		if got != tc.want {
			t.Errorf("process_attribution %q, bind_loopback_only %v: %v, want %v", tc.value, tc.loopbackOnly, got, tc.want)
		}
	}
}

// A typo must not quietly mean auto: it would leave attribution on, or off, by accident.
func TestValidate_RejectsAnUnknownProcessAttribution(t *testing.T) {
	c := &Config{Mode: ModeProxySidecar, Listener: forwardOnlyListener(), Session: SessionConfig{ProcessAttribution: "maybe"}}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "process_attribution") {
		t.Errorf("Validate = %v, want an error naming session.process_attribution", err)
	}
	c.Session.ProcessAttribution = ProcessAttributionOn
	if err := c.Validate(); err != nil {
		t.Errorf("Validate with %q: %v", ProcessAttributionOn, err)
	}
}
```

Append to `core/bootstrap/bootstrap_test.go` (add `"context"`, `"net"`, `"net/http"` and `"time"` to its imports if missing):

```go
func TestNewHTTPServer_AppliesItsOptions(t *testing.T) {
	type key struct{}
	srv := newHTTPServer("127.0.0.1:0", http.NotFoundHandler(), []ServerOption{
		WithConnContext(func(ctx context.Context, _ net.Conn) context.Context { return context.WithValue(ctx, key{}, true) }),
	})
	if srv.ConnContext == nil {
		t.Fatal("WithConnContext did not set ConnContext")
	}
	if v := srv.ConnContext(context.Background(), nil).Value(key{}); v != true {
		t.Errorf("ConnContext is not the one given: value %v", v)
	}
	if srv.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %s, want the default kept", srv.ReadHeaderTimeout)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `cd core && go test ./config -run 'ProcessAttribution' ; go test ./bootstrap -run TestNewHTTPServer`
Expected: build failures — `undefined: ProcessAttributionAuto`, `unknown field ProcessAttribution`, `undefined: newHTTPServer`.

- [ ] **Step 3: Implement the setting**

In `core/config/config.go`, in `type SessionConfig struct`, after the `ClientAffinity` field, add:

```go
	// ProcessAttribution files a request that carries no session header under the session
	// of the process that sent it, or of its nearest ancestor that named one, looked up
	// from the kernel by core/peerproc — so a tool an agent's shell runs, its gh and git
	// tunnels and its WebFetch land in the agent's session, and a process of no agent in
	// none. "auto", the default (and what an empty value means), turns it on exactly when
	// listener.bind_loopback_only is set: only there is every client a process on this
	// host. "on" and "off" force it. Needs id_headers non-empty, like client_affinity. Not
	// hot-reloadable. See session.Store.SessionForProcess.
	ProcessAttribution string `yaml:"process_attribution" json:"process_attribution"`
```

and after `ClientAffinityEnabled`:

```go
// The values of session.process_attribution.
const (
	ProcessAttributionAuto = "auto"
	ProcessAttributionOn   = "on"
	ProcessAttributionOff  = "off"
)

// ProcessAttributionEnabled reports whether header-less requests are filed by the process
// that sent them, given whether every listener binds loopback only.
func (s SessionConfig) ProcessAttributionEnabled(loopbackOnly bool) bool {
	switch s.ProcessAttribution {
	case ProcessAttributionOn:
		return true
	case ProcessAttributionOff:
		return false
	default:
		return loopbackOnly
	}
}
```

In `core/config/validate.go`, in `Validate`, after the `validateListeners` check, add:

```go
	if err := validateSession(cfg); err != nil {
		return err
	}
```

and add:

```go
// validateSession refuses a session.process_attribution other than auto, on and off. Read
// as auto, a typo would leave attribution on or off by accident.
func validateSession(cfg *Config) error {
	switch cfg.Session.ProcessAttribution {
	case "", ProcessAttributionAuto, ProcessAttributionOn, ProcessAttributionOff:
		return nil
	default:
		return fmt.Errorf("session.process_attribution must be auto, on or off, got %q", cfg.Session.ProcessAttribution)
	}
}
```

- [ ] **Step 4: Implement the server hook**

In `core/bootstrap/bootstrap.go` (add `"context"` to its imports if missing), replace `StartHTTPServer`'s first statement — the `srv := &http.Server{…}` literal — with `srv := newHTTPServer(addr, handler, opts)`, change its signature to `func StartHTTPServer(name string, handler http.Handler, addr string, opts ...ServerOption) (*http.Server, error)`, and add above it:

```go
// ServerOption adjusts the http.Server StartHTTPServer builds, before it serves.
type ServerOption func(*http.Server)

// WithConnContext sets the server's ConnContext, which gives each connection state its
// handlers share — the forward proxy's slot for the client's process.
func WithConnContext(fn func(context.Context, net.Conn) context.Context) ServerOption {
	return func(s *http.Server) { s.ConnContext = fn }
}

func newHTTPServer(addr string, handler http.Handler, opts []ServerOption) *http.Server {
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	for _, o := range opts {
		o(srv)
	}
	return srv
}
```

- [ ] **Step 5: Wire it in `cmd/cortex`**

In `cmd/cortex/main.go`, add `"github.com/rossoctl/cortex/core/peerproc"` to the imports, and replace

```go
		fpSrv.ClientAffinity = cfg.Session.ClientAffinityEnabled()
		fpHTTP, herr := bootstrap.StartHTTPServer("forward-proxy", fpSrv.Handler(), cfg.Listener.ForwardProxyAddr)
```

with

```go
		fpSrv.ClientAffinity = cfg.Session.ClientAffinityEnabled()
		// Process attribution: header-less requests filed by the process that sent them.
		// On by default only for a loopback-bound install (session.process_attribution:
		// auto), and only behind a lookup whose self-test passed — a failure costs one
		// warning and leaves resolution as it was.
		if sessions != nil && cfg.Session.ProcessAttributionEnabled(cfg.Listener.BindLoopbackOnly) {
			if procs, perr := peerproc.New(); perr != nil {
				slog.Warn("process attribution off: this host's process lookup failed its self-test", "error", perr)
			} else {
				fpSrv.Processes = procs
				slog.Info("process attribution on: header-less requests are filed by the process that sent them")
			}
		}
		fpHTTP, herr := bootstrap.StartHTTPServer("forward-proxy", fpSrv.Handler(), cfg.Listener.ForwardProxyAddr,
			bootstrap.WithConnContext(fpSrv.ConnContext))
```

- [ ] **Step 6: Run the tests and build the binary**

```bash
cd core && go test ./config ./bootstrap && go vet ./... && cd ..
TAGS=$(go -C scripts/profile-tags run . full)
(cd cmd/cortex && GOWORK=off go build -tags "$TAGS" ./... && GOWORK=off go vet -tags "$TAGS" ./... && GOWORK=off go test -tags "$TAGS" ./... && GOWORK=off go mod tidy -diff)
```

Expected: every command succeeds; `go mod tidy -diff` prints nothing.

- [ ] **Step 7: Commit**

```bash
gofmt -l core/config core/bootstrap cmd/cortex   # must print nothing
git add core/config/config.go core/config/validate.go core/config/process_attribution_test.go core/bootstrap/bootstrap.go core/bootstrap/bootstrap_test.go cmd/cortex/main.go
git commit -s -F - <<'EOF'
feat: Add session.process_attribution and turn it on for laptop installs

auto (the default) turns process attribution on exactly when every
listener binds loopback only; on and off force it, and anything else
fails validation. cortex creates the process lookup at startup — one
warning and no attribution if its self-test fails — and gives the
forward proxy's http.Server the per-connection slot it needs, through a
new bootstrap.WithConnContext option.

Part of #1187.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
EOF
```

---

### Task 5: Docs, full verification, and acceptance against live agents

**Files:**
- Modify: `docs/superpowers/specs/2026-10-01-per-process-session-attribution-design.md`
- Modify: `docs/laptop-service.md` (the attribution bullet list around the client-affinity paragraph)

**Interfaces:** consumes the behaviour of Tasks 1–4.

- [ ] **Step 1: Update the spec**

In the spec's "Per-process attribution (PR 3)" section, replace the paragraph beginning `**Agent self-traffic is not recorded.** A plain-HTTP request whose destination is a` (through `executables and stay visible.`) with:

```markdown
**Agent self-traffic is not recorded.** A plain-HTTP request whose destination is a
loopback port is forwarded the way `skip_hosts` traffic is — no pipeline, no row — when
the listener is an **agent's own process**, one that has named a session through its own
header, and runs the **same executable** as the client. That is OpenCode's TUI and `run`
client talking to their own service; it is also what kept `default` active. Requiring the
listener to be an agent's is what keeps a node-based agent's calls to a local node MCP
server visible: same executable, but an MCP server never names a session. Ollama, LM Studio
and every other local server stay visible for the same reason. Each program-and-service
pair is announced once at INFO — `/stats` carries only authentication counters — and every
skipped request at DEBUG.
```

In the same section, in the paragraph beginning `**What the store learns.**`, append the sentence: `A pending bucket is per agent process — pending:<agent>@<pid> — so two windows of one agent starting at once do not share one; client affinity's agent-wide pending:<agent> remains for clients the lookup cannot name.` In the "Decisions recorded" list, add one bullet: `- **The self-traffic listener must be an agent's process**, not only the same executable — PR 2's review showed an interpreter (node) makes executables equal across unrelated programs. **cortex-cpex is not wired**: it needs cgo, and never runs on a laptop.`

- [ ] **Step 2: Update `docs/laptop-service.md`**

Directly after the bullet that begins `- **Header-less requests are attributed by agent, not by session.**` (and its following client-affinity paragraph, which ends `restart the proxy after changing it.`), insert a new bullet:

```markdown
- **On a laptop, header-less requests are attributed by process.** Cortex asks the
  kernel which process opened each connection, and files a request with no session
  header under the session of that process or of its nearest ancestor that named one —
  so `gh`, `git` and `curl` run by an agent's shell, its `WebFetch` and its MCP calls land
  in the agent's session, including opaque tunnels that cannot be decrypted, and a process
  of no agent (your own terminal's `curl`) lands in `default` while an agent is active. An
  agent talking to its own service on this machine — OpenCode's TUI and its background
  service — is forwarded without being recorded; the first time, Cortex logs which. It is
  `session.process_attribution`: `auto` (the default) means on for this loopback-only
  install and off in a cluster; `on` and `off` force it. Where the lookup is unavailable it
  logs one warning at startup and client affinity applies. Not hot-reloadable.
```

- [ ] **Step 3: Full verification**

```bash
cd core && go vet ./... && go test -race ./... && go mod tidy -diff && cd ..
gofmt -l core/session core/listener/forwardproxy core/config core/bootstrap core/peerproc cmd/cortex
TAGS=$(go -C scripts/profile-tags run . full)
(cd cmd/cortex && GOWORK=off go vet -tags "$TAGS" ./... && GOWORK=off go test -tags "$TAGS" ./... && GOWORK=off go mod tidy -diff)
(cd cmd/agentop && env -u SSL_CERT_FILE GOWORK=off go test ./...)
podman run --rm -v "$PWD":/src:ro -w /src/core -e GOFLAGS=-buildvcs=false -e GOWORK=off docker.io/library/golang:1.26 go test -race ./session/ ./listener/forwardproxy/ ./peerproc/
```

Expected: every command succeeds, `gofmt` and both `tidy -diff`s print nothing. Paste each command's actual output into the report.

- [ ] **Step 4: Acceptance on a private proxy (never touch `:47600`–`:47604` or `~/.cortex/config.yaml`)**

Build and start a private instance with attribution on (`bind_loopback_only: true` is in the copied config, so `auto` turns it on) and OpenCode's header named:

```bash
mkdir -p /tmp/pr3 && TAGS=$(go -C scripts/profile-tags run . full)
(cd cmd/cortex && GOWORK=off go build -tags "$TAGS" -o /tmp/pr3/cortex .)
sed -e 's/127.0.0.1:4760\([0-4]\)/127.0.0.1:4770\1/' -e 's/generate_ca: true/generate_ca: false/' ~/.cortex/config.yaml > /tmp/pr3/config.yaml
printf 'cost_ledger:\n  enabled: false\nsession:\n  id_headers: [X-Claude-Code-Session-Id, X-Task-Id, X-Opencode-Session-Id]\n' >> /tmp/pr3/config.yaml
/tmp/pr3/cortex --config /tmp/pr3/config.yaml > /tmp/pr3/proxy.log 2>&1 &
sleep 3; grep -E "process attribution" /tmp/pr3/proxy.log   # must say "on"
```

(a) **Claude Code with a tool, an opaque tunnel and a stranger:**

```bash
claude -p --settings '{"env":{"HTTPS_PROXY":"http://127.0.0.1:47700","HTTP_PROXY":"http://127.0.0.1:47700"}}' \
  --allowedTools "Bash(curl:*)" "Bash(git:*)" -- \
  "Run exactly these two bash commands and reply with both outputs: (1) curl -s -o /dev/null -w '%{http_code}' https://example.com  (2) git ls-remote https://github.com/rossoctl/cortex HEAD"
curl -s -o /dev/null -x http://127.0.0.1:47700 http://example.org/   # from this shell: no agent's
```

Expected, from `curl -s http://127.0.0.1:47701/v1/sessions` and each session's events: the Claude session (its `X-Claude-Code-Session-Id`) holds its inference rows, the bridged `example.com` request, **and the opaque `github.com:443` tunnel's open and close**; `default` holds the `example.org` request from this shell; nothing of Claude's is in `default`.

(b) **OpenCode, isolated, with a tool and its TUI's self-traffic.** Its own `XDG_*` directories give it its own background service, which must not collide with the user's on the fixed port 49374:

```bash
export PATH=$HOME/.opencode/bin:$PATH XDG_CONFIG_HOME=/tmp/pr3/oc/config XDG_DATA_HOME=/tmp/pr3/oc/data \
       XDG_STATE_HOME=/tmp/pr3/oc/state XDG_CACHE_HOME=/tmp/pr3/oc/cache
mkdir -p /tmp/pr3/oc/config /tmp/pr3/oc/data /tmp/pr3/oc/state /tmp/pr3/oc/cache /tmp/pr3/work && cd /tmp/pr3/work
opencode service set port 49399
agentop exec --cortex-stats-url http://127.0.0.1:47702 -- opencode run --auto \
  "Use the bash tool to run exactly: curl -s -o /dev/null -w '%{http_code}' https://example.com  -- then reply with only its output."
agentop exec --cortex-stats-url http://127.0.0.1:47702 -- opencode run "Reply with exactly one word: pong"
grep -E "not recording an agent's traffic" /tmp/pr3/proxy.log
```

Expected: OpenCode's session (`ses_…`) holds its inference rows and the `example.com` request its bash tool's `curl` made; after the service's first inference, its clients' `127.0.0.1:49399` `/api/*` calls are no longer recorded (the INFO line names the opencode executable and `127.0.0.1:49399`); its local-model probes (`11434`, `1234`, `8000`) are recorded. Stop the isolated service (`opencode service stop` under the same `XDG_*`) and the private proxy (`pkill -f '/tmp/pr3/cortex --config'`) afterwards, and remove `/tmp/pr3/oc`.

Paste the session listings and the relevant rows into the report.

- [ ] **Step 5: Commit**

```bash
git add docs/superpowers/specs/2026-10-01-per-process-session-attribution-design.md docs/laptop-service.md
git commit -s -F - <<'EOF'
docs: Describe per-process attribution and the self-traffic rule

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
EOF
```
