package forwardproxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// TestClientAffinity_InClusterResolutionIsUnchanged is a characterization test: it pins
// the in-cluster answers with the knob OFF and asserts the SAME answers with it ON.
//
// In-cluster, an A2A agent's outbound calls carry no coding-agent header and no
// recognised User-Agent, and ActiveSession() is what correlates them with the inbound
// turn that caused them — IBAC and sparc depend on it. ClientAffinity must not reach
// that case at all: every row here is traffic from no known coding agent.
func TestClientAffinity_InClusterResolutionIsUnchanged(t *testing.T) {
	cases := []struct {
		name       string
		inbound    bool   // seed an inbound A2A turn under conv-A
		ua         string // "" keeps Go's default User-Agent
		header     string // X-Claude-Code-Session-Id, "" for none
		nilHeaders bool   // the transparent path: no request to read
		wantPlugin string
		wantRecord string
	}{
		{name: "A2A turn, Go client", inbound: true, wantPlugin: "conv-A", wantRecord: "conv-A"},
		{name: "A2A turn, agent library UA", inbound: true, ua: "python-httpx/0.27.0", wantPlugin: "conv-A", wantRecord: "conv-A"},
		{name: "A2A turn, transparent", inbound: true, nilHeaders: true, wantPlugin: "conv-A", wantRecord: "conv-A"},
		{name: "nothing active", wantPlugin: "", wantRecord: session.DefaultSessionID},
		{name: "header wins over the A2A turn", inbound: true, header: "hdr-1", wantPlugin: "hdr-1", wantRecord: "hdr-1"},
	}
	for _, affinity := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				store := session.New(5*time.Minute, 100, 0)
				defer store.Close()
				if tc.inbound {
					store.Append("conv-A", pipeline.SessionEvent{
						At:        time.Now(),
						Direction: pipeline.Inbound,
						Phase:     pipeline.SessionRequest,
						A2A:       &pipeline.A2AExtension{Method: "message/send", SessionID: "conv-A"},
					})
				}
				s := &Server{
					Sessions:         store,
					SessionIDHeaders: []string{session.ClaudeCodeSessionHeader},
					ClientAffinity:   affinity,
				}
				var h http.Header
				if !tc.nilHeaders {
					h = http.Header{}
					if tc.ua != "" {
						h.Set("User-Agent", tc.ua)
					}
					if tc.header != "" {
						h.Set(session.ClaudeCodeSessionHeader, tc.header)
					}
				}
				if got := s.resolvePluginSessionID(h); got != tc.wantPlugin {
					t.Errorf("affinity=%v: plugin identity = %q, want %q", affinity, got, tc.wantPlugin)
				}
				if got := s.resolveOutboundSessionID(h); got != tc.wantRecord {
					t.Errorf("affinity=%v: recording bucket = %q, want %q", affinity, got, tc.wantRecord)
				}
			})
		}
	}
}

// TestClientAffinity_InClusterPluginIdentityThroughTheHandler drives the same A2A case
// through the real handler with the knob ON, so a resolver that agreed in isolation but
// was bypassed on the request path would still fail here.
func TestClientAffinity_InClusterPluginIdentityThroughTheHandler(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	store.Append("conv-A", pipeline.SessionEvent{
		At:        time.Now(),
		Direction: pipeline.Inbound,
		Phase:     pipeline.SessionRequest,
		A2A:       &pipeline.A2AExtension{Method: "message/send", SessionID: "conv-A"},
	})
	probe, client, backendURL := newAffinityProxy(t, store)

	get(t, client, backendURL, "")

	if len(probe.sawID) != 1 || probe.sawID[0] != "conv-A" {
		t.Fatalf("plugin saw %v, want [conv-A] (A2A correlation must survive client_affinity)", probe.sawID)
	}
	if v := store.View("conv-A"); v == nil || len(v.Events) != 3 {
		n := -1
		if v != nil {
			n = len(v.Events)
		}
		t.Fatalf("conv-A holds %d events, want 3 (inbound turn, outbound request, its response)", n)
	}
}

// newAffinityProxy is newProbedProxy with ClientAffinity on.
func newAffinityProxy(t *testing.T, store *session.Store) (*identityProbePlugin, *http.Client, string) {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)
	probe := &identityProbePlugin{}
	p, err := pipeline.New([]pipeline.Plugin{probe})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		Client:           http.DefaultClient,
		SessionIDHeaders: []string{session.ClaudeCodeSessionHeader, session.BobSessionHeader},
		ClientAffinity:   true,
	}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)
	return probe, &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))}}, backend.URL
}
