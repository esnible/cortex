package session

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rossoctl/cortex/core/pipeline"
)

// userEvent builds an inference event whose user messages carry the given contents.
func userEvent(contents ...string) pipeline.SessionEvent {
	msgs := make([]pipeline.InferenceMessage, 0, len(contents))
	for _, c := range contents {
		msgs = append(msgs, pipeline.InferenceMessage{Role: "user", Content: c})
	}
	return pipeline.SessionEvent{Inference: &pipeline.InferenceExtension{Messages: msgs}}
}

func renameMsg(args string) string {
	return renamePrefix + "<command-args>" + args + "</command-args>"
}

func TestSessionTitle_EmptySlice(t *testing.T) {
	if got := sessionTitle(nil); got != emptySessionTitle {
		t.Errorf("nil slice = %q, want %q", got, emptySessionTitle)
	}
	if got := sessionTitle([]pipeline.SessionEvent{}); got != emptySessionTitle {
		t.Errorf("empty slice = %q, want %q", got, emptySessionTitle)
	}
}

// An event with no user text is not the same as no events: "" (absent on the wire), not
// "(empty session)", which would claim the session had nothing in it at all.
func TestSessionTitle_NoMatch(t *testing.T) {
	events := []pipeline.SessionEvent{
		{Inference: &pipeline.InferenceExtension{Messages: []pipeline.InferenceMessage{
			{Role: "assistant", Content: "sure, let me look"},
			{Role: "tool", Content: "exit 0"},
		}}},
		{Host: "api.example.com"}, // Inference == nil
	}
	if got := sessionTitle(events); got != "" {
		t.Errorf("no user text = %q, want %q", got, "")
	}
}

// THE CORE PRECEDENCE CASE. A /rename outranks a user message that arrived after it, so the
// reverse walk must not stop at the first candidate it finds.
func TestSessionTitle_RenameWinsOverLaterInference(t *testing.T) {
	events := []pipeline.SessionEvent{
		userEvent(renameMsg("Fix the parser")),
		userEvent("and now do something else entirely"),
	}
	if got := sessionTitle(events); got != "Fix the parser" {
		t.Errorf("got %q, want %q — a later user message outranked a /rename", got, "Fix the parser")
	}
}

func TestSessionTitle_RenameLastWins(t *testing.T) {
	events := []pipeline.SessionEvent{
		userEvent(renameMsg("first name")),
		userEvent(renameMsg("second name")),
	}
	if got := sessionTitle(events); got != "second name" {
		t.Errorf("got %q, want %q", got, "second name")
	}
	// Two renames inside ONE event: the forward inner loop must also take the later.
	one := []pipeline.SessionEvent{userEvent(renameMsg("early"), renameMsg("late"))}
	if got := sessionTitle(one); got != "late" {
		t.Errorf("same-event renames: got %q, want %q", got, "late")
	}
}

func TestSessionTitle_UserQuery(t *testing.T) {
	events := []pipeline.SessionEvent{
		userEvent("preamble <user_query>find the bug</user_query> trailer"),
	}
	if got := sessionTitle(events); got != "find the bug" {
		t.Errorf("got %q, want %q", got, "find the bug")
	}
	// Outranks a plain user message that came later...
	events = append(events, userEvent("some follow-up"))
	if got := sessionTitle(events); got != "find the bug" {
		t.Errorf("a later plain message outranked <user_query>: got %q", got)
	}
	// ...but loses to a /rename, wherever it sits.
	events = append(events, userEvent(renameMsg("explicit")))
	if got := sessionTitle(events); got != "explicit" {
		t.Errorf("<user_query> outranked a /rename: got %q", got)
	}
}

func TestSessionTitle_LastUserMessage(t *testing.T) {
	// Last user message within one event.
	one := []pipeline.SessionEvent{userEvent("first ask", "second ask")}
	if got := sessionTitle(one); got != "second ask" {
		t.Errorf("within one event: got %q, want %q", got, "second ask")
	}
	// A later event beats an earlier one at the same rank.
	two := []pipeline.SessionEvent{userEvent("old ask"), userEvent("new ask")}
	if got := sessionTitle(two); got != "new ask" {
		t.Errorf("across events: got %q, want %q", got, "new ask")
	}
	// Non-user roles are never candidates, even when they are last.
	mixed := []pipeline.SessionEvent{{Inference: &pipeline.InferenceExtension{
		Messages: []pipeline.InferenceMessage{
			{Role: "user", Content: "the real ask"},
			{Role: "assistant", Content: "my answer"},
		},
	}}}
	if got := sessionTitle(mixed); got != "the real ask" {
		t.Errorf("an assistant message was chosen: got %q", got)
	}
}

// The trap the literal spec walks into. A user-role message carrying a tool result or an
// image flattens to Content == "" (pipeline.InferenceMessage.ContentBytes documents it,
// TestInferenceParser_AnthropicMessages_RequestContentBytes pins it) — and it is the LAST
// message of every agentic turn after the first tool call. Taking it verbatim would leave
// the majority of real sessions unnamed.
func TestSessionTitle_EmptyUserContentSkipped(t *testing.T) {
	events := []pipeline.SessionEvent{{Inference: &pipeline.InferenceExtension{
		Messages: []pipeline.InferenceMessage{
			{Role: "user", Content: "read /etc/hosts"},
			{Role: "assistant", Content: ""},
			// A tool_result block array: billed for, but no text.
			{Role: "user", Content: "", ContentBytes: 4096},
		},
	}}}
	if got := sessionTitle(events); got != "read /etc/hosts" {
		t.Errorf("got %q, want %q — an empty tool-result message was chosen", got, "read /etc/hosts")
	}
}

func TestSessionTitle_Sanitizes(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"newline and tab fold", "a\nb\tc", "a b c"},
		{"leading whitespace dropped", "  lead", "lead"},
		{"trailing whitespace dropped", "trail  ", "trail"},
		{"bidi mark folds", "a‎b", "a b"},
		{"nbsp folds", "a b", "a b"},
		{"ideographic space folds", "a　b", "a b"},
		{"nul folds", "a\x00b", "a b"},
		{"a run collapses to one space", "a \n\t b", "a b"},
		{"crlf folds to one", "a\r\nb", "a b"},
		{"zero width folds", "a​b", "a b"},
		{"whitespace only", " \n\t ", ""},
		{"combining mark survives", "café", "café"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sessionTitle([]pipeline.SessionEvent{userEvent(tc.in)})
			if got != tc.want {
				t.Errorf("sanitize(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.ContainsAny(got, "\n\r\t") {
				t.Errorf("result %q is not single-line", got)
			}
		})
	}
}

// A missing closing tag yields "" from between(), not the rest of the string — so the
// candidate falls through to the next rank rather than swallowing the whole message under a
// tag name it never closed.
func TestSessionTitle_UnclosedTag(t *testing.T) {
	in := "<user_query>no close"
	if got := sessionTitle([]pipeline.SessionEvent{userEvent(in)}); got != in {
		t.Errorf("got %q, want the raw string %q", got, in)
	}
	// A rename whose args never close yields NOTHING, not the raw envelope. The asymmetry
	// with <user_query> above is deliberate: that tag can appear in ordinary prose, so the
	// whole message is a reasonable title, whereas a /rename envelope is machinery and
	// "<command-name>/rename</command-name><command-args>no close" names nothing. This
	// assertion originally expected the raw envelope, which is what the fall-through in
	// titleFrom produced — the fall-through was the bug.
	open := renamePrefix + "<command-args>no close"
	if got := sessionTitle([]pipeline.SessionEvent{userEvent(open)}); got != "" {
		t.Errorf("got %q, want %q — a raw /rename envelope leaked as a title", got, "")
	}
	// And a real title behind such an envelope still wins.
	events := []pipeline.SessionEvent{userEvent("the genuine ask"), userEvent(open)}
	if got := sessionTitle(events); got != "the genuine ask" {
		t.Errorf("got %q, want %q", got, "the genuine ask")
	}
}

// An argument-less /rename names nothing. If it claimed rank 0 with "", it would end the
// walk and lose a real title sitting behind it.
func TestSessionTitle_EmptyCommandArgs(t *testing.T) {
	events := []pipeline.SessionEvent{
		userEvent("a genuine earlier ask"),
		userEvent(renamePrefix + "<command-args></command-args>"),
	}
	if got := sessionTitle(events); got != "a genuine earlier ask" {
		t.Errorf("got %q, want %q — an empty /rename won", got, "a genuine earlier ask")
	}
}

func TestSessionTitle_ClipsToMaxTitleLen(t *testing.T) {
	// A ~190KB message, the size of the largest measured event.
	t.Run("long ascii", func(t *testing.T) {
		got := sessionTitle([]pipeline.SessionEvent{userEvent(strings.Repeat("x", 190_000))})
		if n := utf8.RuneCountInString(got); n != maxTitleLen {
			t.Errorf("clipped to %d runes, want %d", n, maxTitleLen)
		}
	})

	// RUNES, not bytes: 200 CJK runes is 600 bytes, and a byte cut would split one.
	t.Run("multibyte stays valid utf8", func(t *testing.T) {
		got := sessionTitle([]pipeline.SessionEvent{userEvent(strings.Repeat("日", 200))})
		if n := utf8.RuneCountInString(got); n != maxTitleLen {
			t.Errorf("clipped to %d runes, want %d", n, maxTitleLen)
		}
		if !utf8.ValidString(got) {
			t.Errorf("clip produced invalid UTF-8: %q", got)
		}
	})

	t.Run("exactly at the cap is unchanged", func(t *testing.T) {
		in := strings.Repeat("y", maxTitleLen)
		if got := sessionTitle([]pipeline.SessionEvent{userEvent(in)}); got != in {
			t.Errorf("a title exactly at the cap was altered: %d runes", utf8.RuneCountInString(got))
		}
	})

	// The cut can land just after a folded space, which is why clipTitle trims again.
	t.Run("no trailing space survives the cut", func(t *testing.T) {
		in := strings.Repeat("ab ", 200) // a space lands at rune index 80
		got := sessionTitle([]pipeline.SessionEvent{userEvent(in)})
		if strings.HasSuffix(got, " ") {
			t.Errorf("clip left a trailing space: %q", got)
		}
		if n := utf8.RuneCountInString(got); n > maxTitleLen {
			t.Errorf("clipped to %d runes, want <= %d", n, maxTitleLen)
		}
	})
}

// The cap is what keeps an unbounded user message off every /v1/sessions response.
func TestSessionTitle_NeverExceedsCap(t *testing.T) {
	for _, in := range []string{
		strings.Repeat("x", 190_000),
		renameMsg(strings.Repeat("r", 5000)),
		"<user_query>" + strings.Repeat("q", 5000) + "</user_query>",
		strings.Repeat("日本", 5000),
	} {
		if n := utf8.RuneCountInString(sessionTitle([]pipeline.SessionEvent{userEvent(in)})); n > maxTitleLen {
			t.Errorf("a title reached %d runes, over the %d cap", n, maxTitleLen)
		}
	}
}

func TestListSessions_Title(t *testing.T) {
	s := New(5*time.Minute, 100, 0)
	s.Append("named", userEvent(renameMsg("Ship the title field")))
	s.Append("named", pipeline.SessionEvent{MCP: &pipeline.MCPExtension{Method: "tools/call"}})
	s.Append("unnamed", pipeline.SessionEvent{Inference: &pipeline.InferenceExtension{
		Messages: []pipeline.InferenceMessage{{Role: "assistant", Content: "no user text here"}},
	}})

	byID := map[string]string{}
	for _, sum := range s.ListSessions() {
		byID[sum.ID] = sum.Title
	}
	if byID["named"] != "Ship the title field" {
		t.Errorf("named title = %q, want %q", byID["named"], "Ship the title field")
	}
	if byID["unnamed"] != "" {
		t.Errorf("unnamed title = %q, want empty", byID["unnamed"])
	}
}

func TestSessionSummary_TitleOmittedWhenEmpty(t *testing.T) {
	b, err := json.Marshal(SessionSummary{ID: "s"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Case-insensitive for the reason TestSessionSummary_PromptContextOmittedWhenNil gives:
	// an untagged field marshals under its Go name ("Title"), so a case-sensitive check for
	// "title" would pass whether or not the tag exists and would pin nothing.
	if strings.Contains(strings.ToLower(string(b)), "title") {
		t.Errorf("an empty title serialized as %s, want the field absent", b)
	}

	b, err = json.Marshal(SessionSummary{ID: "s", Title: "a name"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back SessionSummary
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Title != "a name" {
		t.Errorf("round-tripped to %q", back.Title)
	}
}
