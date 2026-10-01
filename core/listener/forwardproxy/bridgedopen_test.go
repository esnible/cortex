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
