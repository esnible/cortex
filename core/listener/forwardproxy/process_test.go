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
//
// Bob speaks last, so ActiveSession() is task-1 and two agents are active: without the
// process lookup git's tunnel would land in task-1, and the stranger's would not land in
// default.
func TestProcessAttribution_AnOpaqueTunnelJoinsItsProcessSession(t *testing.T) {
	// Off, the CONNECT pin is the process answer alone, and only appendTunnelOpen reading
	// it under processesOn keeps git's tunnel out of ActiveSession()'s task-1.
	for _, tc := range []struct {
		name      string
		configure func(*Server)
	}{
		{name: "client affinity on"},
		{name: "client affinity off", configure: func(s *Server) { s.ClientAffinity = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			procs := newFakeProcs(
				fproc(100, 50, "/bin/claude"), fproc(200, 100, "/bin/bash"), fproc(310, 200, "/usr/bin/git"),
				fproc(800, 60, "/usr/bin/node"), fproc(700, 60, "/usr/bin/python3"),
			)
			store := session.New(0, 0, 0)
			defer store.Close()
			proxyURL, backendURL, _ := newProcessProxy(t, store, procs, tc.configure)
			sendAs(t, procs.clientFor(proxyURL, 100), backendURL+"/v1/messages", procClaudeUA, session.ClaudeCodeSessionHeader, "s1")
			sendAs(t, procs.clientFor(proxyURL, 800), backendURL+"/inference", "bob-shell/2.0.5", session.BobSessionHeader, "task-1")

			raw, br, resp := connectAs(t, procs, proxyURL, pingPongOrigin(t), 310)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("CONNECT status %d", resp.StatusCode)
			}
			pingPong(t, raw, br)
			eventually(t, func() bool { _, closes := tunnelRows(store, "s1"); return len(closes) == 1 }, "the git tunnel's close row in s1")
			onePair(t, store, "s1")

			// A process of no agent while agents are active: its tunnel goes to default.
			raw, br, resp = connectAs(t, procs, proxyURL, pingPongOrigin(t), 700)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("CONNECT status %d", resp.StatusCode)
			}
			pingPong(t, raw, br)
			eventually(t, func() bool {
				_, closes := tunnelRows(store, session.DefaultSessionID)
				return len(closes) == 1
			}, "the stranger's tunnel close row in default")
			onePair(t, store, session.DefaultSessionID)
			if v := store.View(session.DefaultSessionID); len(v.Events) != 2 {
				t.Errorf("default holds %d event(s), want only the stranger's tunnel pair", len(v.Events))
			}
			onePair(t, store, "s1")
			if opens, closes := tunnelRows(store, "task-1"); len(opens)+len(closes) != 0 {
				t.Errorf("task-1 holds %d tunnel row(s); neither tunnel is Bob's", len(opens)+len(closes))
			}
		})
	}
}

// ConnContext runs on the accept loop, so it must not look the client up: a connection
// that sends nothing costs no lookup.
func TestProcessAttribution_AConnectionWithNoRequestIsNotLookedUp(t *testing.T) {
	procs := newFakeProcs(fproc(100, 50, "/bin/claude"))
	store := session.New(0, 0, 0)
	defer store.Close()
	proxyURL, _, _ := newProcessProxy(t, store, procs, nil)
	raw, err := net.Dial("tcp", strings.TrimPrefix(proxyURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	procs.own(raw, 100)
	time.Sleep(50 * time.Millisecond) // let the server accept it and run ConnContext
	_ = raw.Close()
	if n := procs.lookupCount(); n != 0 {
		t.Errorf("%d lookups for a connection that sent no request, want 0", n)
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
