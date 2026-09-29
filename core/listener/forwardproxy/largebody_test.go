package forwardproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// responseBodyProbe declares ReadsBody, which is what makes
// Pipeline.NeedsResponseBody true — the same shape as the three protocol
// parsers (inference-parser, mcp-parser, a2a-parser), each of which
// declares ReadsBody and nothing else. It records the length it was
// handed so a test can tell "buffered and parsed" from "streamed past".
type responseBodyProbe struct {
	sawResponseBodyLen int
	responseCalls      int
}

func (p *responseBodyProbe) Name() string { return "response-body-probe" }
func (p *responseBodyProbe) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{ReadsBody: true}
}
func (p *responseBodyProbe) OnRequest(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *responseBodyProbe) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	p.responseCalls++
	p.sawResponseBodyLen = len(pctx.ResponseBody)
	return pipeline.Action{Type: pipeline.Continue}
}

// serveOversizedBody writes maxBodySize+1 bytes of a non-SSE content type.
// When declareLength is false the handler leaves Content-Length unset, so
// Go serves it chunked and resp.ContentLength is -1 — the case a
// header-based short-circuit cannot see.
func serveOversizedBody(t *testing.T, declareLength bool) (*httptest.Server, int) {
	t.Helper()
	size := maxBodySize + 1
	payload := make([]byte, 32<<10)
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		if declareLength {
			w.Header().Set("Content-Length", strconv.Itoa(size))
		}
		w.WriteHeader(http.StatusOK)
		remaining := size
		for remaining > 0 {
			n := len(payload)
			if n > remaining {
				n = remaining
			}
			if _, err := w.Write(payload[:n]); err != nil {
				return
			}
			remaining -= n
		}
	}))
	return srv, size
}

func proxyGet(t *testing.T, srv *Server, url string) *http.Response {
	t.Helper()
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))},
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// TestForwardProxy_OversizedDeclaredLengthStreamsThrough is the regression
// test for the bug that made `claude update` fail behind the laptop proxy:
// a 226MB binary download over a bridged (non-passthrough) host was answered
// 502 {"error":"response body too large"}.
//
// There is no stream to preserve here and nothing was downgraded — the
// incremental path is gated on Content-Type: text/event-stream, so a
// non-SSE response has only ever had the buffered path available. Any
// ReadsBody plugin therefore turns every large download into a 502, even
// though the plugin could not have parsed application/octet-stream.
//
// Content-Length is present and accurate, so the size is knowable before a
// byte of body is read. The assertion is that the caller gets all the bytes.
func TestForwardProxy_OversizedDeclaredLengthStreamsThrough(t *testing.T) {
	backend, size := serveOversizedBody(t, true)
	defer backend.Close()

	probe := &responseBodyProbe{}
	p, err := pipeline.New([]pipeline.Plugin{probe})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{OutboundPipeline: pipeline.NewHolder(p), Client: http.DefaultClient}

	resp := proxyGet(t, srv, backend.URL+"/claude-code-releases/2.1.284/darwin-arm64/claude")

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (body %q), want 200", resp.StatusCode, body)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		t.Fatalf("reading proxied body: %v", err)
	}
	if n != int64(size) {
		t.Errorf("proxied %d bytes, want %d", n, size)
	}
	// The plugin must see an empty body rather than a truncated one: a
	// partial body is indistinguishable from a real short response and
	// would make a parser emit nonsense. Mirrors the SSE passthrough
	// contract, where a ReadsBody-only plugin also gets an empty body.
	if probe.sawResponseBodyLen != 0 {
		t.Errorf("plugin saw %d body bytes, want 0 (not buffered)", probe.sawResponseBodyLen)
	}
}

// TestForwardProxy_OversizedUnknownLengthStreamsThrough covers the other
// half: a chunked response declares no length, so the size only becomes
// known once the cap is already exceeded mid-read. Discovering it late must
// still not fail the request — the bytes already read belong to the caller.
func TestForwardProxy_OversizedUnknownLengthStreamsThrough(t *testing.T) {
	backend, size := serveOversizedBody(t, false)
	defer backend.Close()

	probe := &responseBodyProbe{}
	p, err := pipeline.New([]pipeline.Plugin{probe})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{OutboundPipeline: pipeline.NewHolder(p), Client: http.DefaultClient}

	resp := proxyGet(t, srv, backend.URL+"/chunked")

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (body %q), want 200", resp.StatusCode, body)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		t.Fatalf("reading proxied body: %v", err)
	}
	if n != int64(size) {
		t.Errorf("proxied %d bytes, want %d", n, size)
	}
}

// TestForwardProxy_UndersizedBodyStillBuffers guards the other direction:
// the fix must not stop buffering the bodies plugins exist to read. A
// normal-sized response still reaches the plugin whole.
func TestForwardProxy_UndersizedBodyStillBuffers(t *testing.T) {
	payload := `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}]}}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(payload))
	}))
	defer backend.Close()

	probe := &responseBodyProbe{}
	p, err := pipeline.New([]pipeline.Plugin{probe})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{OutboundPipeline: pipeline.NewHolder(p), Client: http.DefaultClient}

	resp := proxyGet(t, srv, backend.URL+"/mcp")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != payload {
		t.Errorf("proxied body = %q, want %q", body, payload)
	}
	if probe.sawResponseBodyLen != len(payload) {
		t.Errorf("plugin saw %d body bytes, want %d", probe.sawResponseBodyLen, len(payload))
	}
}
