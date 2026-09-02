package pipeline

import "testing"

// TestUpstreamErrorKind: a bare "backend_error / 400" gives an operator nothing
// to act on. The provider's own classification does — and it must be the
// classification only, never the human message, which quotes request content
// into an unauthenticated store.
func TestUpstreamErrorKind(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "anthropic error type",
			body: `{"type":"error","error":{"type":"invalid_request_error","message":"tools.3: unexpected"}}`,
			want: "invalid_request_error",
		},
		{
			name: "openai style falls back to code",
			body: `{"error":{"message":"bad","code":"context_length_exceeded"}}`,
			want: "context_length_exceeded",
		},
		{"type preferred over code", `{"error":{"type":"rate_limit_error","code":"429"}}`, "rate_limit_error"},
		{"no error object", `{"ok":true}`, ""},
		{"malformed json", `{"error":{"type":`, ""},
		{"empty body", ``, ""},
		{"not json at all", `<html>502 Bad Gateway</html>`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := upstreamErrorKind([]byte(tc.body)); got != tc.want {
				t.Errorf("upstreamErrorKind() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestUpstreamErrorKind_NeverLeaksTheMessage is the privacy assertion: the
// provider's prose can quote the request, so it must never reach the event.
func TestUpstreamErrorKind_NeverLeaksTheMessage(t *testing.T) {
	secret := "sk-live-abcdef123456"
	body := `{"error":{"type":"authentication_error","message":"invalid key ` + secret + `"}}`
	got := upstreamErrorKind([]byte(body))
	if got != "authentication_error" {
		t.Fatalf("got %q, want the type", got)
	}
	if got == secret || len(got) > 64 {
		t.Errorf("message content leaked into the event: %q", got)
	}
}

// TestDeriveError_PopulatesKindFrom4xxBody wires it to the event an operator
// actually reads.
func TestDeriveError_PopulatesKindFrom4xxBody(t *testing.T) {
	pctx := &Context{
		StatusCode:   400,
		ResponseBody: []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"x"}}`),
	}
	e := DeriveError(pctx)
	if e == nil {
		t.Fatal("expected an error event for a 400")
	}
	if e.Kind != "backend_error" || e.Code != "400" {
		t.Errorf("kind/code = %q/%q", e.Kind, e.Code)
	}
	if e.Message != "invalid_request_error" {
		t.Errorf("Message = %q, want the provider's error type", e.Message)
	}
	// A 4xx with no parseable body must still produce the event, just without
	// a classification — never an error swallowed for lack of a body.
	bare := DeriveError(&Context{StatusCode: 503})
	if bare == nil || bare.Code != "503" || bare.Message != "" {
		t.Errorf("bare 5xx = %+v, want backend_error/503 with empty message", bare)
	}
}
