package forwardproxy

import (
	"bytes"
	"encoding/pem"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
	"github.com/rossoctl/cortex/authbridge/authlib/tlsbridge"
)

// bridgeForRejectTest builds a Server whose upstream verification SUCCEEDS (so
// bridgeServe reaches the forge step) against a throwaway TLS origin.
func bridgeForRejectTest(t *testing.T) (*Server, *session.Store, string) {
	t.Helper()

	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(origin.Close)
	originCAPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: origin.Certificate().Raw})

	src, err := tlsbridge.NewEphemeralSource()
	if err != nil {
		t.Fatalf("NewEphemeralSource: %v", err)
	}
	up, err := tlsbridge.NewUpstreamClient(originCAPEM)
	if err != nil {
		t.Fatalf("NewUpstreamClient: %v", err)
	}
	store := session.New(5*time.Minute, 100, 0)
	t.Cleanup(store.Close)

	s := &Server{
		Sessions: store,
		TLSBridge: &tlsbridge.Engine{
			Term:     tlsbridge.NewTerminator(tlsbridge.NewMinter(src, tlsbridge.MinterOpts{})),
			Skip:     tlsbridge.NewSkipSet(),
			Upstream: up,
			CAPEM:    src.CACertPEM(),
		},
	}
	return s, store, strings.TrimPrefix(origin.URL, "https://")
}

// nonTLSClient returns the proxy-side conn of a pair whose peer sends bytes that
// are not a TLS handshake, so Terminate fails exactly as it does for a client
// that refuses the forged leaf.
func nonTLSClient(t *testing.T) net.Conn {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	done := make(chan net.Conn, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			done <- nil
			return
		}
		done <- c
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	// Not a ClientHello, and then gone — the shape of a rejected handshake.
	_, _ = client.Write([]byte("nope"))
	_ = client.Close()

	srv := <-done
	if srv == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// TestClientRejectedCA_WarnsEvenAfterOtherTrafficBridged is the regression test
// for the bug this change exists to fix.
//
// The guidance ("does not trust the bridge CA") used to sit behind
// bridgedRequests == 0, which treats CA trust as a property of the deployment.
// It is a property of each client. On a machine running several agents they
// disagree — one predates the CA, the rest do not — so the counter was non-zero
// and the one message that explains the failure never printed. Diagnosing it
// then took the proxy log, the CA's NotBefore and a process listing.
func TestClientRejectedCA_WarnsEvenAfterOtherTrafficBridged(t *testing.T) {
	s, store, authority := bridgeForRejectTest(t)

	// The condition that used to suppress everything: something has bridged.
	s.bridgedRequests.Store(7)

	var logbuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	client := nonTLSClient(t)
	pctx := &pipeline.Context{Direction: pipeline.Outbound, Host: authority}
	rec := func(reason string) { s.recordTunnelOpened(pctx, reason) }

	if handled := s.bridgeServe(client, authority, hostOnly(authority), rec); !handled {
		t.Fatal("bridgeServe returned false; the connection is dead post-forge and must be reported handled")
	}

	got := logbuf.String()
	if !strings.Contains(got, pipeline.TunnelClientRejectedCA) {
		t.Errorf("warning did not name the reason %q despite bridgedRequests=7:\n%s",
			pipeline.TunnelClientRejectedCA, got)
	}
	// The client address is the discriminator — the host cannot tell two clients
	// apart because they all dial the same host.
	if !strings.Contains(got, "client=") {
		t.Errorf("warning did not name the client, so the offender is unattributable:\n%s", got)
	}
	if !strings.Contains(got, "ca_not_before=") {
		t.Errorf("warning did not state the CA cutoff, which is what makes it actionable:\n%s", got)
	}
	if !strings.Contains(got, "lsof") {
		t.Errorf("warning did not say how to map the port to a process:\n%s", got)
	}

	// And the timeline carries it, so this is visible without reading a log file.
	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 1 {
		t.Fatalf("want exactly 1 tunnel event, got %+v", v)
	}
	if ev := v.Events[0]; !ev.Tunnel || ev.TunnelReason != pipeline.TunnelClientRejectedCA {
		t.Errorf("event = {Tunnel:%v Reason:%q}, want {true %q}",
			ev.Tunnel, ev.TunnelReason, pipeline.TunnelClientRejectedCA)
	}
}

// TestClientRejectedCA_SkipsHostAfterwards pins the existing self-healing
// behaviour: the failure is remembered so the client's retry tunnels instead of
// dying again. Unchanged by this work, asserted because the reason vocabulary
// now distinguishes that later tunnel (skip-cached) from this one.
func TestClientRejectedCA_SkipsHostAfterwards(t *testing.T) {
	s, _, authority := bridgeForRejectTest(t)
	host := hostOnly(authority)
	if s.TLSBridge.Skip.Contains(host) {
		t.Fatal("host skipped before any failure")
	}
	s.bridgeServe(nonTLSClient(t), authority, host, func(string) {})
	if !s.TLSBridge.Skip.Contains(host) {
		t.Error("host not skipped after a rejected forge; the client's retry would fail again")
	}
}
