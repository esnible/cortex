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
