# Bridged CONNECT Rows Follow Their First Request — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A bridged tunnel's open row is recorded together with the tunnel's first decrypted request — in that request's session, directly before it — instead of under `ActiveSession()` at handshake time.

**Architecture:** The session store gains `AppendPair`, which appends two events under one lock so nothing can land between them. The forward proxy's `tunnelLog` stops recording a bridged tunnel's open eagerly: it defers it, and the first decrypted request that records a row (its request row or its denial) records the open with it through `AppendPair`. A tunnel whose handlers all finish without recording anything gets today's open plus a close, once `ServeConn` has returned and no admitted handler is still running.

**Tech Stack:** Go 1.26.5; packages `core/session` and `core/listener/forwardproxy`.

**Spec:** `docs/superpowers/specs/2026-10-01-per-process-session-attribution-design.md` — section "Bridged CONNECT rows (PR 1)". This plan is PR 1 of four.

## Global Constraints

- Work only in `.worktrees/attribution-design` (branch `design/per-process-attribution`); never in the top-level checkout.
- Every commit: `git commit -s` (DCO), and end the message with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. Never `Co-Authored-By`.
- Run Go tests from `core/` (the `core` CI job uses the workspace; no `GOWORK=off` needed there).
- Before the final push: `gofmt -l core/session core/listener/forwardproxy` prints nothing, and `go vet ./...` in `core/` passes.
- The open row of a bridged tunnel carries **no** `TunnelReason` — that empty reason is what tells agentop to fold it into the decrypted request.
- The deferred open row takes **the first recorded row's `At`**, never a later time: agentop's pager reads an older page whose last event is later than the newer page's first as "session restarted".
- Opaque tunnels and the transparent listener keep today's behaviour. The transparent listener records its open before calling `bridgeServe`, so deferring is a no-op there.
- No change to `tunnelSessionID`, `resolvePluginSessionID`, or any session-resolution rule. Only *when* and *where* the bridged open is appended changes.

---

### Task 1: `Store.AppendPair`

**Files:**
- Modify: `core/session/store.go` (`append`, currently lines 373–592)
- Create: `core/session/pair_test.go`

**Interfaces:**
- Produces: `func (s *Store) AppendPair(sessionID string, first, second pipeline.SessionEvent) *Bucket` — appends `first` then `second` to the named session under one lock acquisition and returns the bucket both went into (usable with `AppendTrailing`). Same semantics as two `Append` calls otherwise: follows an adopted pending id, creates the session if needed, bumps `UpdatedAt` and `activeID`.

- [ ] **Step 1: Write the failing tests**

Create `core/session/pair_test.go`:

```go
package session

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

func pairEvent(requestID string, tunnel bool) pipeline.SessionEvent {
	return pipeline.SessionEvent{
		At: time.Now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
		RequestID: requestID, Tunnel: tunnel,
	}
}

// AppendPair exists for a reader that pairs an event with the one after it — agentop
// folds a bridged tunnel's open into the next event in the session — so no concurrent
// append may ever land between the two.
func TestAppendPair_NothingLandsBetweenThePair(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	const n = 200
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			id := fmt.Sprint("pair-", i)
			s.AppendPair("sess-1", pairEvent(id, true), pairEvent(id, false))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			s.Append("sess-1", pairEvent(fmt.Sprint("other-", i), false))
		}
	}()
	wg.Wait()

	evs := s.View("sess-1").Events
	if len(evs) != 3*n {
		t.Fatalf("%d events, want %d", len(evs), 3*n)
	}
	opens := 0
	for i, e := range evs {
		if !e.Tunnel {
			continue
		}
		opens++
		if i+1 >= len(evs) || evs[i+1].Tunnel || evs[i+1].RequestID != e.RequestID {
			t.Fatalf("event %d (%s) is not directly followed by its pair", i, e.RequestID)
		}
		if evs[i+1].Seq != e.Seq+1 {
			t.Errorf("pair %s has seqs %d and %d, want consecutive", e.RequestID, e.Seq, evs[i+1].Seq)
		}
	}
	if opens != n {
		t.Errorf("%d first-of-pair events, want %d", opens, n)
	}
}

// The returned bucket is the one both events went into, so a later trailing event —
// a tunnel's close — lands beside them, and an adopted pending id is followed exactly
// as Append follows it.
func TestAppendPair_ReturnsTheBucketBothWentInto(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	pending := PendingSessionID("claude-code")
	s.Append(pending, pairEvent("probe", false))
	s.Claim("sess-1", "claude-code") // adopts the pending bucket into sess-1

	b := s.AppendPair(pending, pairEvent("p", true), pairEvent("p", false))
	if !s.AppendTrailing(b, trailingEvent()) {
		t.Fatal("AppendTrailing refused the bucket AppendPair returned")
	}
	v := s.View("sess-1")
	if v == nil || len(v.Events) != 4 {
		t.Fatalf("sess-1 = %+v, want the adopted probe, the pair and the trailing event", v)
	}
	if !v.Events[1].Tunnel || v.Events[2].Tunnel {
		t.Errorf("events 1 and 2 are not the pair in order: %+v", v.Events[1:3])
	}
	if s.View(pending) != nil {
		t.Error("AppendPair re-created the adopted pending bucket")
	}
	if got := s.ActiveSession(); got != "sess-1" {
		t.Errorf("ActiveSession() = %q, want sess-1: a pair is that session speaking", got)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `cd core && go test ./session -run TestAppendPair -v`
Expected: build failure, `s.AppendPair undefined (type *Store has no field or method AppendPair)`.

- [ ] **Step 3: Split `append` into its pre-lock and locked halves, and add `AppendPair`**

In `core/session/store.go`:

1. Add, directly above `func (s *Store) append(`:

```go
// appendPrep is what an append computes from the event alone, before taking the lock.
// See prepareAppend for why each part is hoisted out of the critical section.
type appendPrep struct {
	money     eventMoney
	titleRank int
	titleText string
	agentName string
}
```

2. Replace the body of `append` — from `if len(sessionID) > MaxSessionIDLen {` down to and including `defer s.mu.Unlock()` — so that `append` reads exactly:

```go
func (s *Store) append(sessionID string, b *Bucket, event pipeline.SessionEvent) *entry {
	prep := prepareAppend(&event)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(sessionID, b, event, prep)
}
```

3. Add `prepareAppend` directly below `append`. Move the two existing comment blocks — the one beginning `// BEFORE THE LOCK. This is a json.Unmarshal` and the one beginning `// BEFORE THE LOCK FOR THE SAME REASON` — into it **verbatim**, above the lines they explain:

```go
// prepareAppend computes everything an append needs from the event alone. It runs
// before the store's lock is taken.
func prepareAppend(event *pipeline.SessionEvent) appendPrep {
	// <the "BEFORE THE LOCK. This is a json.Unmarshal ..." block, verbatim>
	money := moneyOf(event)

	// <the "BEFORE THE LOCK FOR THE SAME REASON ..." block, verbatim>
	titleRank, titleText := titleCandidate(event)
	return appendPrep{money: money, titleRank: titleRank, titleText: titleText, agentName: event.Client.AffinityName()}
}
```

4. Add `appendLocked` directly below `prepareAppend`. Its body is the old remainder of `append` — everything from `now := s.clock()` through the final `return sess` — **unchanged**, preceded by the session-id cap and one line that restores the four locals the body already uses (`p` is taken: the trim plan below is named `p`):

```go
// appendLocked is append's critical section. s.mu must be held; prep is what
// prepareAppend computed from event before the lock was taken.
func (s *Store) appendLocked(sessionID string, b *Bucket, event pipeline.SessionEvent, prep appendPrep) *entry {
	if len(sessionID) > MaxSessionIDLen {
		sessionID = sessionID[:MaxSessionIDLen]
	}
	money, titleRank, titleText, agentName := prep.money, prep.titleRank, prep.titleText, prep.agentName

	now := s.clock()
	// ... the rest of the old append body, unchanged, through `return sess`
}
```

5. Add `AppendPair` directly below `AppendTrailing`:

```go
// AppendPair appends first and then second to the named session under one lock
// acquisition, so no concurrent append can land between them, and returns the bucket
// both went into. agentop folds a bridged tunnel's open row into the event directly
// after it in the session, and two separate appends could be split by any traffic in
// between.
func (s *Store) AppendPair(sessionID string, first, second pipeline.SessionEvent) *Bucket {
	p1, p2 := prepareAppend(&first), prepareAppend(&second)
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.appendLocked(sessionID, nil, first, p1)
	// e.ID, not sessionID: the first append has already capped the id and followed any
	// adoption, and the second must land in the same entry.
	s.appendLocked(e.ID, nil, second, p2)
	return &Bucket{e: e}
}
```

- [ ] **Step 4: Run the new tests and the whole package**

Run: `cd core && go test ./session -run TestAppendPair -v && go test ./session`
Expected: both `TestAppendPair_*` PASS, then `ok  github.com/rossoctl/cortex/core/session`.

Also run the race detector on the concurrency test: `cd core && go test -race ./session -run TestAppendPair_NothingLandsBetweenThePair`
Expected: PASS, no `DATA RACE`.

- [ ] **Step 5: Commit**

```bash
gofmt -l core/session   # must print nothing
git add core/session/store.go core/session/pair_test.go
git commit -s -F - <<'EOF'
feat: Add Store.AppendPair for events a reader pairs by adjacency

agentop folds a bridged tunnel's open row into the event directly after
it in the session. AppendPair appends two events under one lock so no
concurrent traffic can split them. append is split into its pre-lock
preparation and its critical section so both callers share one body.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
EOF
```

---

### Task 2: Defer a bridged tunnel's open row to its first recorded request

**Files:**
- Modify: `core/listener/forwardproxy/server.go` — `tunnelLog` (struct ~line 1935, methods to ~line 2020), `markBridged` (~1811), `bridgeServe` (~660–770), `bridgedHandler` (~772), `handleRequest` (~263), `serveOutbound` (~275, its reject call ~367 and its `s.Sessions.Append(sid, ev)` ~412), `recordOutboundReject` (~1304)
- Modify: `core/listener/forwardproxy/transparent.go` — `recordTunnelOpened` (~210)
- Modify: `core/listener/forwardproxy/tunnelclose_test.go` — `bridgingProxy` (~503), the two `tl.closeUnserved()` calls (~589, ~614)
- Create: `core/listener/forwardproxy/bridgedopen_test.go`

**Interfaces:**
- Consumes: `(*session.Store).AppendPair(sessionID string, first, second pipeline.SessionEvent) *session.Bucket` from Task 1.
- Produces (package-internal, used by the tests in this task):
  - `func (t *tunnelLog) deferOpen()`
  - `func (t *tunnelLog) recordWith(sid string, ev pipeline.SessionEvent) bool`
  - `func (t *tunnelLog) admit() bool` (existing; now counts in-flight handlers)
  - `func (t *tunnelLog) release()`
  - `func (t *tunnelLog) finish()` (replaces `closeUnserved`)
  - `func (s *Server) appendOutbound(tl *tunnelLog, sid string, ev pipeline.SessionEvent)`
  - `func (s *Server) tunnelOpenEvent(pctx *pipeline.Context, reason pipeline.TunnelReason) pipeline.SessionEvent`
  - `func (s *Server) recordOutboundRejectIn(tl *tunnelLog, pctx *pipeline.Context, action pipeline.Action, sid string)`
  - `serveOutbound(w http.ResponseWriter, r *http.Request, tl *tunnelLog)`: the `isBridge bool` parameter becomes `tl`; `nil` means plaintext.

- [ ] **Step 1: Write the failing tests**

First, in `core/listener/forwardproxy/tunnelclose_test.go`, make the bridging fixture configurable. Replace `bridgingProxy` (the whole function) with:

```go
// bridgingProxy is a proxy whose TLS bridge terminates CONNECTs to a trusted TLS origin.
func bridgingProxy(t *testing.T, store *session.Store) (proxyAddr, target string, bridgeCA []byte, done <-chan struct{}) {
	t.Helper()
	return bridgingProxyWith(t, store, nil)
}

// bridgingProxyWith is bridgingProxy with configure applied to the server before it
// serves anything.
func bridgingProxyWith(t *testing.T, store *session.Store, configure func(*Server)) (proxyAddr, target string, bridgeCA []byte, done <-chan struct{}) {
	t.Helper()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(origin.Close)
	originCA := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: origin.Certificate().Raw})
	u, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatalf("parse origin URL: %v", err)
	}
	engine := bridgeEngine(t, portOf(u.Host), originCA)
	s := connectServer(t, store, engine)
	if configure != nil {
		configure(s)
	}
	proxyAddr, done = connectProxy(t, s)
	return proxyAddr, u.Host, engine.CAPEM, done
}
```

In the same file, change both `tl.closeUnserved()` calls to `tl.finish()` (in `TestBridgedHandler_RequestAfterTheCloseRecordsNothing` and `TestTunnelLog_AdmittedRequestSuppressesTheClose`); their comments stay true.

Then create `core/listener/forwardproxy/bridgedopen_test.go`:

```go
package forwardproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/plugintesting"
	"github.com/rossoctl/cortex/core/session"
)

// Tests for #1187's first part: a bridged tunnel's open row is recorded with the
// tunnel's first decrypted request that records a row — in that row's session and
// directly before it — rather than under ActiveSession() at handshake time.

func decryptedRequest(path string) pipeline.SessionEvent {
	return pipeline.SessionEvent{
		At: time.Now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
		Host: "example.com", HTTPMethod: http.MethodGet, HTTPPath: path,
	}
}

// deferredTunnel is a bridged tunnel's log as bridgeServe leaves it after a
// successful handshake.
func deferredTunnel(s *Server) *tunnelLog {
	tl := s.newTunnelLog(&pipeline.Context{Direction: pipeline.Outbound, Method: http.MethodConnect, Host: "example.com:443"}, false)
	tl.deferOpen()
	return tl
}

// openThen asserts sid holds exactly one tunnel open, directly followed by another
// event for the same host and stamped no later than it, and returns the two.
func openThen(t *testing.T, store *session.Store, sid string) (open, next pipeline.SessionEvent) {
	t.Helper()
	v := store.View(sid)
	if v == nil {
		t.Fatalf("session %q holds nothing", sid)
	}
	at := -1
	for i, e := range v.Events {
		if e.Tunnel && e.Phase == pipeline.SessionRequest {
			if at >= 0 {
				t.Fatalf("session %q holds more than one tunnel open", sid)
			}
			at = i
		}
	}
	if at < 0 || at+1 >= len(v.Events) {
		t.Fatalf("session %q: no tunnel open followed by an event (%d events)", sid, len(v.Events))
	}
	open, next = v.Events[at], v.Events[at+1]
	if next.Tunnel || hostOnly(next.Host) != hostOnly(open.Host) {
		t.Fatalf("event after the open = %+v, want the decrypted request to %s", next, open.Host)
	}
	if open.TunnelReason != "" {
		t.Errorf("bridged open carries reason %q; an empty reason is what makes agentop fold it", open.TunnelReason)
	}
	if open.At.After(next.At) {
		t.Errorf("open stamped %s, after the next row's %s; agentop's pager reads that as a restarted session", open.At, next.At)
	}
	return open, next
}

func TestTunnelLog_FirstRecordedRowCarriesTheOpen(t *testing.T) {
	store := session.New(0, 0, 0)
	defer store.Close()
	store.Append("other", decryptedRequest("/unrelated")) // what ActiveSession() answers
	s := &Server{Sessions: store}
	tl := deferredTunnel(s)

	if !tl.admit() {
		t.Fatal("a request on a live tunnel was refused")
	}
	s.appendOutbound(tl, "sess-1", decryptedRequest("/first"))
	s.appendOutbound(tl, "sess-1", decryptedRequest("/second"))
	tl.release()
	tl.finish()

	if _, next := openThen(t, store, "sess-1"); next.HTTPPath != "/first" {
		t.Errorf("open precedes %q, want the first recorded request", next.HTTPPath)
	}
	if v := store.View("sess-1"); len(v.Events) != 3 {
		t.Errorf("sess-1 holds %d events, want the open and both requests", len(v.Events))
	}
	if opens, closes := tunnelRows(store, "other"); len(opens)+len(closes) != 0 {
		t.Error("a tunnel row landed in the session that was merely active")
	}
	if _, closes := tunnelRows(store, "sess-1"); len(closes) != 0 {
		t.Errorf("a tunnel its requests answered recorded %d close(s)", len(closes))
	}
}

// On h2 several decrypted requests share one tunnel and run concurrently. Exactly one
// of them takes the open, and nothing records between the two.
func TestTunnelLog_ConcurrentRequestsShareOneOpen(t *testing.T) {
	store := session.New(0, 0, 0)
	defer store.Close()
	s := &Server{Sessions: store}
	tl := deferredTunnel(s)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if !tl.admit() {
				t.Error("a request on a live tunnel was refused")
				return
			}
			defer tl.release()
			s.appendOutbound(tl, "sess-1", decryptedRequest(fmt.Sprint("/", i)))
		}(i)
	}
	wg.Wait()
	tl.finish()

	openThen(t, store, "sess-1")
	if v := store.View("sess-1"); len(v.Events) != 9 {
		t.Errorf("sess-1 holds %d events, want one open and eight requests", len(v.Events))
	}
}

// A request admitted but never recorded — its body failed to read, say — answers
// nothing, so the tunnel still owes its open and a close.
func TestTunnelLog_UnrecordedRequestStillSettlesTheTunnel(t *testing.T) {
	store := session.New(0, 0, 0)
	defer store.Close()
	s := &Server{Sessions: store}
	tl := deferredTunnel(s)

	if !tl.admit() {
		t.Fatal("a request on a live tunnel was refused")
	}
	tl.release()
	tl.finish()

	if _, closed := onePair(t, store, session.DefaultSessionID); closed.StatusCode != http.StatusOK {
		t.Errorf("close status %d, want 200", closed.StatusCode)
	}
}

// On h2 ServeConn returns without waiting for handlers. The tunnel is settled by the
// last one to finish, so its row cannot land after a close saying it carried nothing.
func TestTunnelLog_HandlerOutlivingServeConnSettlesOnRelease(t *testing.T) {
	store := session.New(0, 0, 0)
	defer store.Close()
	s := &Server{Sessions: store}
	tl := deferredTunnel(s)

	if !tl.admit() {
		t.Fatal("a request on a live tunnel was refused")
	}
	tl.finish() // ServeConn returned; the handler is still running
	if n := len(store.ListSessions()); n != 0 {
		t.Fatalf("%d session(s) recorded while a handler could still record", n)
	}
	tl.release()

	onePair(t, store, session.DefaultSessionID)
}

// bridgedRoundTrip sends req over an established bridged connection and drains the
// response.
func bridgedRoundTrip(t *testing.T, tc *tls.Conn, req *http.Request) *http.Response {
	t.Helper()
	go func() { _ = req.Write(tc) }()
	resp, err := http.ReadResponse(bufio.NewReader(tc), req)
	if err != nil {
		t.Fatalf("bridged request: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp
}

// End to end: the CONNECT names no session, the decrypted request does, and the open
// follows the request rather than the session active when the CONNECT arrived.
func TestHandleConnect_BridgedOpenJoinsItsRequestsSession(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	store.Append("other", decryptedRequest("/unrelated")) // ActiveSession() at CONNECT time
	proxyAddr, target, ca, done := bridgingProxyWith(t, store, func(s *Server) {
		s.SessionIDHeaders = []string{session.ClaudeCodeSessionHeader}
	})

	raw, br, _ := sendConnect(t, proxyAddr, target)
	tc := bridgedTLS(t, raw, br, target, ca)
	req, _ := http.NewRequest(http.MethodGet, "https://"+hostOnly(target)+"/x", nil)
	req.Header.Set(session.ClaudeCodeSessionHeader, "sess-1")
	if resp := bridgedRoundTrip(t, tc, req); resp.StatusCode != http.StatusOK {
		t.Fatalf("bridged request status %d, want 200", resp.StatusCode)
	}
	_ = tc.Close()
	waitDone(t, done)

	if _, next := openThen(t, store, "sess-1"); next.HTTPPath != "/x" || next.Phase != pipeline.SessionRequest {
		t.Errorf("open precedes %s %q, want the request to /x", next.Phase, next.HTTPPath)
	}
	if opens, _ := tunnelRows(store, "other"); len(opens) != 0 {
		t.Error("the open landed in the session that was active at CONNECT time")
	}
}

// denyDecrypted lets CONNECTs through and denies every decrypted request, so a bridged
// tunnel's first recorded row is a denial.
type denyDecrypted struct{}

func (denyDecrypted) Name() string                              { return "deny-decrypted" }
func (denyDecrypted) Capabilities() pipeline.PluginCapabilities { return pipeline.PluginCapabilities{} }
func (denyDecrypted) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if pctx.Method == http.MethodConnect {
		return pipeline.Action{Type: pipeline.Continue}
	}
	return pctx.DenyAndRecord("denied_for_test", "test.denied", "denied for the test")
}
func (denyDecrypted) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// A denial is a recorded row too, so it takes the open the same way.
func TestHandleConnect_BridgedOpenJoinsARejectedRequest(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	proxyAddr, target, ca, done := bridgingProxyWith(t, store, func(s *Server) {
		s.SessionIDHeaders = []string{session.ClaudeCodeSessionHeader}
		p, err := plugintesting.BuildPipeline([]pipeline.Plugin{denyDecrypted{}})
		if err != nil {
			t.Fatalf("BuildPipeline: %v", err)
		}
		s.OutboundPipeline = pipeline.NewHolder(p)
	})

	raw, br, _ := sendConnect(t, proxyAddr, target)
	tc := bridgedTLS(t, raw, br, target, ca)
	req, _ := http.NewRequest(http.MethodGet, "https://"+hostOnly(target)+"/x", nil)
	req.Header.Set(session.ClaudeCodeSessionHeader, "sess-1")
	if resp := bridgedRoundTrip(t, tc, req); resp.StatusCode < 400 {
		t.Fatalf("denied request got status %d", resp.StatusCode)
	}
	_ = tc.Close()
	waitDone(t, done)

	if _, next := openThen(t, store, "sess-1"); next.Phase != pipeline.SessionDenied {
		t.Errorf("open precedes a %s row, want the denial", next.Phase)
	}
	if v := store.View(session.DefaultSessionID); v != nil {
		t.Errorf("default holds %d event(s); the open belongs with the denial", len(v.Events))
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `cd core && go test ./listener/forwardproxy -run 'TestTunnelLog_|TestHandleConnect_Bridged|TestBridgedHandler_' -v`
Expected: build failure naming `tl.deferOpen undefined`, `s.appendOutbound undefined`, `tl.release undefined` and `tl.finish undefined`.

- [ ] **Step 3: Split the open row's construction out of `recordTunnelOpened`**

In `core/listener/forwardproxy/transparent.go`, replace everything in `recordTunnelOpened` from `plugins := pipeline.SnapshotPlugins(pctx.Extensions.Custom)` to the end of the function with:

```go
	// Always record the tunnel-open so passthrough/non-bridged tunnels (no
	// plugin activity) are still visible. For a TLS-bridged call agentop folds
	// this CONNECT event into the decrypted inner-request row.
	return s.Sessions.AppendBucket(sid, s.tunnelOpenEvent(pctx, reason))
}

// tunnelOpenEvent is a tunnel's open row, for recordTunnelOpened and for a bridged
// tunnel's deferred open (tunnelLog.recordWith).
func (s *Server) tunnelOpenEvent(pctx *pipeline.Context, reason pipeline.TunnelReason) pipeline.SessionEvent {
	return pipeline.SessionEvent{
		At:          time.Now(),
		Direction:   pipeline.Outbound,
		Phase:       pipeline.SessionRequest,
		RequestID:   pctx.RequestID(),
		Invocations: pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseRequest),
		Plugins:     pipeline.SnapshotPlugins(pctx.Extensions.Custom),
		Identity:    pipeline.SnapshotIdentity(pctx),
		Host:        pctx.Host,
		// <the existing "Method is CONNECT — real on a proxied CONNECT ..." comment, verbatim>
		HTTPMethod: pctx.Method,
		HTTPPath:   pctx.Path,
		// <the existing "Explicit opaque-tunnel marker ..." comment, verbatim>
		Tunnel: true,
		// <the existing "Why the bytes stayed opaque ..." comment, verbatim>
		TunnelReason: reason,
		Client:       pctx.ClientInfo(),
	}
}
```

Delete the old local `plugins` variable and the old `ev` literal; their fields are now the literal above.

- [ ] **Step 4: Rework `tunnelLog` in `server.go`**

Replace the `served` field and its comment in `type tunnelLog struct` with:

```go
	// deferred marks a bridged tunnel whose open row is not recorded yet. It waits for the
	// first decrypted request that records a row, so it can land in that row's session
	// directly before it; see recordWith. settleLocked records it on today's rule when no
	// request ever does.
	deferred bool
	// answered is set once a decrypted request has recorded a row. That row answers the
	// tunnel, and agentop folds the open into it, so no close is recorded.
	answered bool
	// inflight counts admitted handlers still running; done is set once ServeConn has
	// returned. The tunnel is settled only when both say nothing more can record.
	inflight int
	done     bool
```

Replace `admit` and `closeUnserved` (keep `admit`'s existing doc paragraph and append one sentence to it) with:

```go
// admit counts a decrypted request the bridged tunnel is about to serve, and refuses it
// once the close has been recorded. On h2 that can happen: ServeConn returns without
// waiting for a request's handler to start, and by then the connection is closed and the
// request's context cancelled. Serving it would record a request beside a close that says
// the tunnel carried none. Every admit is matched by a release when the handler returns.
func (t *tunnelLog) admit() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return false
	}
	t.inflight++
	return true
}

// release ends an admitted handler, settling the tunnel if it was the last one running
// after ServeConn returned.
func (t *tunnelLog) release() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.inflight--
	t.settleLocked()
}

// finish is called once ServeConn has returned. It settles the tunnel unless an admitted
// handler is still running; on h2 ServeConn does not wait for them, and the last release
// settles it instead.
func (t *tunnelLog) finish() {
	if t.skipped {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.done = true
	t.settleLocked()
}

// settleLocked records what a bridged tunnel still owes once nothing more can record
// under it: its open, if no request took it, and a close. A tunnel some request answered
// owes nothing.
func (t *tunnelLog) settleLocked() {
	if !t.done || t.inflight > 0 || t.answered {
		return
	}
	if t.deferred {
		t.deferred, t.opened = false, true
		t.bucket = t.s.recordTunnelOpened(t.pctx, t.reason)
	}
	t.closeLocked(http.StatusOK, nil, 0, 0)
}

// deferOpen marks t as a bridged tunnel whose open row waits for recordWith. A no-op once
// the open is recorded: the transparent listener records its own before bridging.
func (t *tunnelLog) deferOpen() {
	if t.skipped {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.opened {
		t.deferred = true
	}
}

// recordWith records ev under sid together with the tunnel's deferred open, and reports
// whether it did. The open goes in directly before ev, in one store call, because agentop
// folds a tunnel row only into the event that follows it. It takes ev's timestamp: a row
// stamped later than the one after it reads as out of order to agentop's pager. When
// the open is not deferred it records nothing and reports false, leaving ev to the
// caller.
//
// t.mu is held across the append so that a second request multiplexed onto the same
// tunnel cannot record between this check and the pair.
func (t *tunnelLog) recordWith(sid string, ev pipeline.SessionEvent) bool {
	if t.skipped || t.s.Sessions == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.answered = true
	if !t.deferred {
		return false
	}
	t.deferred, t.opened = false, true
	open := t.s.tunnelOpenEvent(t.pctx, "")
	open.At = ev.At
	t.bucket = t.s.Sessions.AppendPair(sid, open, ev)
	return true
}
```

Replace `markBridged` and its comment with:

```go
// markBridged records a successful bridge: the open row is deferred to the first
// decrypted request that records a row, and carries no reason, which is the signal
// agentop uses to fold the CONNECT row into that request. Named because
// `tl.deferOpen()` at the call site does not say why.
func markBridged(tl *tunnelLog) { tl.deferOpen() }
```

- [ ] **Step 5: Wire the bridge, the handler and both recording sites**

In `bridgeServe`, replace the comment above `tl.closeUnserved()` and the call itself with:

```go
	// ServeConn returns once the connection has closed. A bridged tunnel's decrypted
	// requests answer it and agentop folds the open into the first of them, so a close row
	// there would render as an orphan response. With no recorded request at all there is
	// nothing to fold into, so finish records the open and a close — now, or when the last
	// handler still running returns.
	tl.finish()
```

Replace `bridgedHandler` with:

```go
// bridgedHandler serves one bridged tunnel's decrypted requests through the pipeline.
func (s *Server) bridgedHandler(authority string, tl *tunnelLog) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !tl.admit() {
			return
		}
		defer tl.release()
		r.URL.Scheme = "https"
		r.URL.Host = authority // host:port — preserves non-443 origins
		s.serveOutbound(w, r, tl)
	})
}
```

In `handleRequest`, change `s.serveOutbound(w, r, false)` to `s.serveOutbound(w, r, nil)`.

Change `serveOutbound`'s signature and its first lines. Replace the doc comment's sentence `isBridge=true marks requests produced by TLS bridging:` with `tl is the bridged tunnel the request was decrypted from, nil for a plaintext request. A non-nil tl marks requests produced by TLS bridging:`, then:

```go
func (s *Server) serveOutbound(w http.ResponseWriter, r *http.Request, tl *tunnelLog) {
	isBridge := tl != nil
	if isBridge {
		s.bridgedRequests.Add(1)
	}
```

(The later `if isBridge && s.TLSBridge != nil {` is unchanged.)

In `serveOutbound`'s reject branch, change
`s.recordOutboundReject(pctx, action, s.recordingSessionID(sessionID, r.Header))`
to
`s.recordOutboundRejectIn(tl, pctx, action, s.recordingSessionID(sessionID, r.Header))`.

In `serveOutbound`'s recording block, change the final `s.Sessions.Append(sid, ev)` to `s.appendOutbound(tl, sid, ev)`.

Replace `recordOutboundReject`'s signature line and its last line so it delegates. Its doc comment stays; the function becomes:

```go
func (s *Server) recordOutboundReject(pctx *pipeline.Context, action pipeline.Action, sid string) {
	s.recordOutboundRejectIn(nil, pctx, action, sid)
}

// recordOutboundRejectIn is recordOutboundReject for a request decrypted from tl, whose
// deferred open — when the denial is the tunnel's first recorded row — goes in with it.
// tl is nil for a plaintext request or a CONNECT.
func (s *Server) recordOutboundRejectIn(tl *tunnelLog, pctx *pipeline.Context, action pipeline.Action, sid string) {
	if s.Sessions == nil || pctx.Extensions.Invocations == nil {
		return
	}
	// ... the existing status/code/message derivation and `ev := pipeline.SessionEvent{...}`, unchanged
	s.appendOutbound(tl, sid, ev)
}
```

Add `appendOutbound` directly below `recordingSessionID`:

```go
// appendOutbound records ev under sid. For a request decrypted from a bridged tunnel tl is
// that tunnel, and the tunnel's first recorded row also records its open; see
// tunnelLog.recordWith.
func (s *Server) appendOutbound(tl *tunnelLog, sid string, ev pipeline.SessionEvent) {
	if tl != nil && tl.recordWith(sid, ev) {
		return
	}
	s.Sessions.Append(sid, ev)
}
```

- [ ] **Step 6: Run the new tests**

Run: `cd core && go test ./listener/forwardproxy -run 'TestTunnelLog_|TestHandleConnect_Bridged|TestBridgedHandler_' -v`
Expected: all PASS, including the pre-existing `TestHandleConnect_BridgedTunnelWithRequestsRecordsNoClose`, `TestHandleConnect_BridgedTunnelWithNoRequestRecordsItsClose`, `TestBridgedHandler_RequestAfterTheCloseRecordsNothing` and `TestTunnelLog_AdmittedRequestSuppressesTheClose`.

- [ ] **Step 7: Run the package with the race detector**

Run: `cd core && go test -race ./listener/forwardproxy`
Expected: `ok`, no `DATA RACE`. If a pre-existing test asserts a bridged open's position before any request was sent, read it before changing it: the open now appears with the first recorded request, or with the close when there is none. Update its expectation only if it encodes the old eager timing rather than an attribution rule.

- [ ] **Step 8: Commit**

```bash
gofmt -l core/listener/forwardproxy   # must print nothing
git add core/listener/forwardproxy
git commit -s -F - <<'EOF'
fix: Record a bridged CONNECT row with its first decrypted request

A bridged tunnel's open row was recorded at handshake time, before any
decrypted request existed, so it could only go under ActiveSession(). On
a laptop where OpenCode's self-polling keeps default active, Claude
Code's ete-litellm CONNECT rows landed there while their requests landed
in the Claude session.

The open is now deferred: the tunnel's first decrypted request that
records a row records the open with it, in that row's session and
directly before it, through Store.AppendPair. It takes that row's
timestamp so agentop's pager never sees time run backwards. A tunnel
whose handlers all finish without recording settles with today's open
and a close, once ServeConn has returned and the last admitted handler
is done.

Part of #1187.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
EOF
```

---

### Task 3: Docs, full verification, and acceptance against a live agent

**Files:**
- Modify: `docs/laptop-service.md:85-89` (the client-affinity paragraph)
- Modify: `CLAUDE.md` (Event schema, the `tunnel, tunnelReason, bytesUp, bytesDown` bullet)
- Modify: `docs/superpowers/specs/2026-10-01-per-process-session-attribution-design.md` (the "Timestamped at recording" bullet)

**Interfaces:**
- Consumes: the behaviour from Tasks 1–2.
- Produces: nothing code-facing.

- [ ] **Step 1: Update the docs**

In `docs/laptop-service.md`, replace `(a tunnel's own row keeps today's attribution)` with:

```
(a bridged tunnel's own row goes wherever its first decrypted request goes, directly before
it; an opaque tunnel's row keeps today's attribution)
```

In `CLAUDE.md`, in the Event-schema bullet that begins `` - `tunnel`, `tunnelReason`, `bytesUp`, `bytesDown` ``, replace the sentence `A bridged tunnel records no close, because its open folds into the first decrypted request, which carries its own response — unless it carried no request at all.` with:

```
A bridged tunnel's open is recorded with its first decrypted request — in that request's session, directly before it, stamped with its time — and records no close, because the request carries its own response; a bridged tunnel that recorded no request gets its open and a close when it ends.
```

In the spec, replace the bullet beginning `- **Timestamped at recording, not at CONNECT.**` with:

```markdown
- **Stamped with its first row's time, not the CONNECT's.** The open row takes the `At`
  of the row it is appended with. Keeping the CONNECT's own time would put a row with an
  earlier `At` after rows with later ones, and agentop's pager treats an older page whose
  last event is later than the newer page's first as "session restarted" — a false flash
  at any page boundary that fell between them. Stamping it at its own append would be a
  few microseconds *later* than the request after it, the same fault in the other
  direction. The cost is the 50–150 ms between CONNECT and first request.
```

- [ ] **Step 2: Full verification**

```bash
cd core && go vet ./... && go test ./... && cd ..
gofmt -l core/session core/listener/forwardproxy     # must print nothing
(cd cmd/agentop && GOWORK=off go test ./...)          # agentop folds these rows; it must still pass
```

Expected: `go vet` silent, every package `ok`. `cmd/agentop`'s `cmd_exec` test can fail in a shell that exports `SSL_CERT_FILE` (a known, unrelated issue); rerun it as `env -u SSL_CERT_FILE GOWORK=off go test ./...` before reading anything into it.

- [ ] **Step 3: Acceptance against a live agent, on a private proxy (does not touch `:47600`)**

```bash
TAGS=$(go -C scripts/profile-tags run . full)
mkdir -p /tmp/pr1 && go build -tags "$TAGS" -o /tmp/pr1/cortex ./cmd/cortex
sed -e 's/127.0.0.1:4760\([0-4]\)/127.0.0.1:4770\1/' -e 's/generate_ca: true/generate_ca: false/' \
    ~/.cortex/config.yaml > /tmp/pr1/config.yaml
printf 'cost_ledger:\n  enabled: false\n' >> /tmp/pr1/config.yaml
/tmp/pr1/cortex --config /tmp/pr1/config.yaml > /tmp/pr1/proxy.log 2>&1 &
sleep 3
claude -p --settings '{"env":{"HTTPS_PROXY":"http://127.0.0.1:47700"}}' "Reply with exactly one word: pong"
# One line per tunnel open: its session, its host, and the event right after it.
for s in $(curl -s http://127.0.0.1:47701/v1/sessions | jq -r '.sessions[].id'); do
  curl -s "http://127.0.0.1:47701/v1/sessions/$s?limit=2000" | jq -r --arg s "$s" '
    .events as $e | range(0; $e|length) as $i
    | select($e[$i].tunnel and $e[$i].phase == "request")
    | "\($s)  \($e[$i].host)  ->  \($e[$i+1].httpMethod // "-") \($e[$i+1].host // "-")\($e[$i+1].httpPath // "")"'
done
```

Expected: every line for a bridged host (`ete-litellm…:443`, `mcp.ete-server…:443`, `api.anthropic.com:443`) names the Claude session — the `X-Claude-Code-Session-Id` UUID, never `default` — and its arrow points at a decrypted request to the same host. Then look at it the way a user will: `agentop observe --endpoint http://127.0.0.1:47701`, open the Claude session, and check the bridged CONNECTs have folded into their requests (no standalone `tunnel` rows for those hosts). Stop the private proxy afterwards: `pkill -f '/tmp/pr1/cortex --config'`.

- [ ] **Step 4: Commit**

```bash
git add docs/laptop-service.md CLAUDE.md docs/superpowers/specs/2026-10-01-per-process-session-attribution-design.md
git commit -s -F - <<'EOF'
docs: Say where a bridged CONNECT row now lands

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
EOF
```

- [ ] **Step 5: Open the PR**

Title: `Fix: Record a bridged CONNECT row with its first decrypted request`. Body: the problem (with the `ete-litellm` rows in `default`), the change, how it was verified (tests plus the Step 3 acceptance output), and that it is PR 1 of 4 under #1187 with the spec path. End the body with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. Ask before pushing.
