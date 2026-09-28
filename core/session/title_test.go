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

// The harness attaches a <system-reminder> block to the user turn it belongs to, so a rank-2
// candidate that takes the message verbatim titles the session with the reminder and never
// reaches the prompt. Live payload shape: the reminder leads, the real ask follows.
func TestSessionTitle_StripsReminder(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{
			"leading reminder, prompt after",
			"<system-reminder>Codebase and user instructions are shown below.</system-reminder>test connection",
			"test connection",
		},
		{
			// 67 of 68 real cases have prose on BOTH sides, which is why the block is spliced
			// out rather than everything before it being discarded.
			"prose on both sides",
			"my question\n<system-reminder>noise</system-reminder>\nand the follow-up",
			"my question and the follow-up",
		},
		{
			"repeated blocks",
			"<system-reminder>a</system-reminder>mid<system-reminder>b</system-reminder>tail",
			"midtail",
		},
		{
			"adjacent blocks",
			"<system-reminder>a</system-reminder><system-reminder>b</system-reminder>the ask",
			"the ask",
		},
		{
			"trailing reminder",
			"the real ask<system-reminder>appended context</system-reminder>",
			"the real ask",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sessionTitle([]pipeline.SessionEvent{userEvent(tc.in)})
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// A message that is nothing but a reminder names nothing. It must fall through to a real
// title behind it rather than answering "" — the same mechanism
// TestSessionTitle_EmptyUserContentSkipped pins for a tool-result message.
func TestSessionTitle_ReminderOnlyFallsThrough(t *testing.T) {
	events := []pipeline.SessionEvent{
		userEvent("the genuine ask"),
		userEvent("<system-reminder>just context, no prompt</system-reminder>"),
	}
	if got := sessionTitle(events); got != "the genuine ask" {
		t.Errorf("got %q, want %q — a reminder-only message won", got, "the genuine ask")
	}
}

// THE SAME FALL-THROUGH, WITHIN ONE EVENT. titleCandidate picks one message cheaply and only
// then settles its rank, so a pick it has to reject must not take the event down with it —
// an earlier message in the very same Messages slice can still title it. Every other
// fall-through case in this file spans two events, where the outer walk covers the mistake;
// these do not, and a two-pass titleCandidate answered "" for all three.
func TestSessionTitle_DemotedPickFallsBackWithinEvent(t *testing.T) {
	for _, tc := range []struct {
		name string
		msgs []string
		want string
	}{
		{
			"empty rename args after a real ask",
			[]string{"a genuine ask", renamePrefix + "<command-args></command-args>"},
			"a genuine ask",
		},
		{
			"reminder-only message after a real ask",
			[]string{"another genuine ask", "<system-reminder>ctx</system-reminder>"},
			"another genuine ask",
		},
		{
			// Two demotions deep: both trailing messages name nothing.
			"two rejects in a row",
			[]string{"the real one", "<system-reminder>a</system-reminder>", renamePrefix + "<command-args></command-args>"},
			"the real one",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sessionTitle([]pipeline.SessionEvent{userEvent(tc.msgs...)})
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Mirrors TestSessionTitle_UnclosedTag's stated asymmetry: a tag that can appear in ordinary
// prose must not truncate the title when it closes nothing.
func TestSessionTitle_UnterminatedReminder(t *testing.T) {
	in := "what does <system-reminder> mean in this code"
	if got := sessionTitle([]pipeline.SessionEvent{userEvent(in)}); got != in {
		t.Errorf("got %q, want the raw string %q", got, in)
	}
}

// Why the strip runs before EVERY rank arm and not only the rank-2 one. Both nestings are
// wrong when it runs later: a reminder inside the query brackets rides along into the title,
// and a <user_query> inside a reminder — a reminder quoting an earlier turn is enough — gets
// mistaken for the real ask.
func TestSessionTitle_ReminderNestedWithUserQuery(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{
			"reminder outside the query",
			"<user_query>find the bug</user_query>\n<system-reminder>noise</system-reminder>",
			"find the bug",
		},
		{
			"reminder inside the query",
			"<user_query>find <system-reminder>noise</system-reminder>the bug</user_query>",
			"find the bug",
		},
		{
			"query inside the reminder",
			"<system-reminder>ctx <user_query>decoy</user_query></system-reminder>real ask",
			"real ask",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionTitle([]pipeline.SessionEvent{userEvent(tc.in)}); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// A /rename tests a PREFIX, so a reminder in front of one hides it entirely unless the strip
// has already run. The strip's placement at the top of titleFrom is what makes this work.
func TestSessionTitle_ReminderBeforeRename(t *testing.T) {
	in := "<system-reminder>noise</system-reminder>" + renameMsg("Fix the parser")
	events := []pipeline.SessionEvent{
		userEvent(in),
		userEvent("a later plain message that must not outrank it"),
	}
	if got := sessionTitle(events); got != "Fix the parser" {
		t.Errorf("got %q, want %q", got, "Fix the parser")
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

// A candidate that SANITIZES to empty names nothing, and accepting one used to lock its rank and
// discard the real title behind it. The raw-content tests inside titleFrom cannot catch this:
// whitespace and control runes are non-empty until sanitizeTitle folds them away.
//
// All three rank arms, because the two better ones are worse: a whitespace-only /rename claimed
// rank 0, which also BREAKS the walk, so nothing behind it was even examined.
//
// EACH CASE RUNS TWICE, in two events and in one, and the two are not the same test. The
// separate-event form is satisfied by sessionTitle's own fold-then-skip; the same-event form is
// not, and passed nowhere until titleCandidate grew its own blank test. titleCandidate collapses
// an event to ONE answer, so a blank winner there does not fall through — it discards the whole
// event, and userEvent("the real ask", "<user_query>   </user_query>") titled the session "".
func TestSessionTitle_BlankAfterSanitizeFallsThrough(t *testing.T) {
	for _, tc := range []struct{ name, blank string }{
		{"spaces", "   "},
		{"tab", "\t"},
		{"newline", "\n"},
		{"nul", "\x00"},
		{"zero width", "​"},
		{"line separator", " "},
		{"rename with blank args", renameMsg("  ")},
		{"user_query with blank body", "<user_query> </user_query>"},
		{"rename with control args", renameMsg("\x00")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []pipeline.SessionEvent{userEvent("the real ask"), userEvent(tc.blank)}
			if got := sessionTitle(events); got != "the real ask" {
				t.Errorf("got %q, want %q — a blank-after-sanitize candidate won", got, "the real ask")
			}
		})
		t.Run(tc.name+" same event", func(t *testing.T) {
			events := []pipeline.SessionEvent{userEvent("the real ask", tc.blank)}
			if got := sessionTitle(events); got != "the real ask" {
				t.Errorf("got %q, want %q — a blank candidate hid a title in its own event", got, "the real ask")
			}
		})
	}
}

// A session whose ONLY candidate sanitizes to blank is unnamed — "" and not the blank string,
// so omitempty drops the field rather than shipping a whitespace title.
func TestSessionTitle_OnlyBlankCandidateIsUnnamed(t *testing.T) {
	for _, in := range []string{"   ", "\t\n", renameMsg(" "), "<user_query>​</user_query>"} {
		if got := sessionTitle([]pipeline.SessionEvent{userEvent(in)}); got != "" {
			t.Errorf("sessionTitle(%q) = %q, want %q", in, got, "")
		}
	}
}

// quickRank is an upper bound in ONE direction only, and the code used to claim both. An
// unterminated <user_query> guesses rank 1 and settles at rank 2, so a rank recorded from the
// guess would outrank a genuine <user_query> elsewhere in the session.
func TestSessionTitle_UnterminatedUserQueryDoesNotOutrank(t *testing.T) {
	// The prose mentioning the tag comes LAST, so an unsettled rank-1 guess would win.
	events := []pipeline.SessionEvent{
		userEvent("<user_query>the genuine query</user_query>"),
		userEvent("what does <user_query> mean in this code"),
	}
	if got := sessionTitle(events); got != "the genuine query" {
		t.Errorf("got %q, want %q — an unterminated <user_query> was recorded as rank 1", got, "the genuine query")
	}
}

// THE SAME THING WITHIN ONE EVENT, which is where the two-pass pick lost it: titleFrom demoted
// the pick to a non-empty rank-2 title, and the retry loop only re-picked on EMPTY, so a
// better-ranked message sitting in front of it was discarded.
func TestSessionTitle_DemotedNonEmptyPickKeepsBetterRank(t *testing.T) {
	msgs := []string{"<user_query>the real ask</user_query>", "what does <user_query> mean here"}
	if got := sessionTitle([]pipeline.SessionEvent{userEvent(msgs...)}); got != "the real ask" {
		t.Errorf("got %q, want %q — a demoted pick discarded a better-ranked message", got, "the real ask")
	}
}

// A DEMOTED GUESS AND A DEFERRED ONE AT THE SAME RANK: the later message wins, per title.go's
// last-match rule. Neither test above reaches this, and the reason is worth stating because it is
// how the bug survived a review: both put a GENUINE <user_query> at index 0, which settles at rank 1
// and outranks everything, so the deferred-candidate loop never runs at all. Plain prose at index 0
// is what forces the tie — index 0 defers at rank 2, index 1 guesses rank 1 and titleFrom demotes it
// to 2, and the two are then equal-ranked with the deferred one EARLIER.
//
// titleCandidate used to return the deferred candidate unconditionally here, on a stated invariant
// that a deferred index is always the later of the two. It is not: the reverse scan meets the newest
// rank-2 GUESS first, but a rank-0/1 guess can demote to rank 2 from anywhere, including after it.
func TestSessionTitle_DeferredLosesToLaterDemotedAtEqualRank(t *testing.T) {
	for _, tc := range []struct{ name, early, late string }{
		// An unterminated <user_query> is the shape that reaches this: quickRank sees the opening
		// tag and guesses rank 1, titleFrom finds no closing tag and settles at rank 2.
		{"unterminated user_query", "early plain prose", "what does <user_query> mean here"},
		{"tag named mid-sentence", "plain earlier", "later prose mentioning <user_query> tag"},
		// NOT a case here: a /rename envelope behind prose. quickRank guesses rank 2 for it (the
		// test below pins that), so both messages defer and the deferred loop's own newest-first
		// walk picks the later one — it passes with or without the index comparison, which is the
		// kind of case that hid this bug in the first place.
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sessionTitle([]pipeline.SessionEvent{userEvent(tc.early, tc.late)})
			if got != tc.late {
				t.Errorf("got %q, want %q — an earlier deferred candidate beat a later demoted one at equal rank", got, tc.late)
			}
		})
	}
}

// bestIdx IS A -1 SENTINEL and it participates in a comparison, so the nothing-settled path needs
// its own pin: when the eager loop accepts nothing, bestRank is rankNone and every real rank must
// still beat it rather than tripping the "equally ranked and earlier" break against index -1.
func TestSessionTitle_DeferredOnlyWithNothingSettled(t *testing.T) {
	for _, tc := range []struct {
		name string
		msgs []string
		want string
	}{
		{"single prose", []string{"just prose"}, "just prose"},
		{"two prose, later wins", []string{"older prose", "newer prose"}, "newer prose"},
		{"blank then prose", []string{"   ", "real prose"}, "real prose"},
		{"prose then blank", []string{"real prose", "   "}, "real prose"},
		{"all blank names nothing", []string{"   ", "\t"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionTitle([]pipeline.SessionEvent{userEvent(tc.msgs...)}); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// The mirror image: a GENUINELY better-ranked demoted-loop neighbour still wins over a later
// deferred one, so the index comparison above did not turn into "later always wins".
func TestSessionTitle_LaterDeferredLosesToBetterRank(t *testing.T) {
	for _, tc := range []struct{ name, first, second, want string }{
		{"user_query then prose", "<user_query>the query</user_query>", "plain prose after it", "the query"},
		{"rename then prose", renameMsg("named"), "plain prose after it", "named"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sessionTitle([]pipeline.SessionEvent{userEvent(tc.first, tc.second)})
			if got != tc.want {
				t.Errorf("got %q, want %q — a later rank-2 message beat a better rank", got, tc.want)
			}
		})
	}
}

// quickRank DELIBERATELY does not detect a /rename envelope sitting behind prose — only behind a
// reminder — and that blind spot is only safe while titleFrom tests the prefix too. Pinned so the
// two functions cannot silently desynchronize: if quickRank ever starts calling such a message
// rank 0 while titleFrom still refuses it, or vice versa, one of these fails.
func TestSessionTitle_RenameEnvelopeBehindProseIsRank2(t *testing.T) {
	content := "please run " + renameMsg("a name") + " for me"
	if got := quickRank(content); got != rankUserMsg {
		t.Errorf("quickRank = %d, want %d (rankUserMsg)", got, rankUserMsg)
	}
	if got, _ := titleFrom(content); got != rankUserMsg {
		t.Errorf("titleFrom rank = %d, want %d (rankUserMsg) — desynchronized from quickRank", got, rankUserMsg)
	}
	// And it must therefore lose to a real /rename anywhere in the session, not win at rank 0.
	events := []pipeline.SessionEvent{userEvent(renameMsg("the real name")), userEvent(content)}
	if got := sessionTitle(events); got != "the real name" {
		t.Errorf("got %q, want %q — a prose-embedded envelope claimed rank 0", got, "the real name")
	}
}

// THE SCAN STOPS AT titleScanEvents, which is what bounds ListSessions' lock hold. Pinned in both
// directions: a title inside the window is found, one older than it is not — the cap is a real
// behaviour change and not just a performance note, so it needs a test that fails if someone
// removes it OR quietly widens it to "whole session".
func TestSessionTitle_ScanCeiling(t *testing.T) {
	// A /rename is rank 0 and would otherwise beat everything and break the walk, so it is the
	// strongest possible candidate to place out of reach.
	build := func(titleAt int, total int) []pipeline.SessionEvent {
		evs := make([]pipeline.SessionEvent, 0, total)
		for i := 0; i < total; i++ {
			if i == titleAt {
				evs = append(evs, userEvent(renameMsg("the name")))
				continue
			}
			evs = append(evs, userEvent("filler prose"))
		}
		return evs
	}
	total := titleScanEvents + 20

	// Newest event inside the window: found.
	if got := sessionTitle(build(total-1, total)); got != "the name" {
		t.Errorf("newest event: got %q, want %q", got, "the name")
	}
	// Oldest event still inside the window: found.
	if got := sessionTitle(build(total-titleScanEvents, total)); got != "the name" {
		t.Errorf("oldest in-window event: got %q, want %q", got, "the name")
	}
	// One event past the window: not reached, so the newest filler names it instead.
	if got := sessionTitle(build(total-titleScanEvents-1, total)); got != "filler prose" {
		t.Errorf("first out-of-window event: got %q, want %q — the ceiling is not being applied", got, "filler prose")
	}
	// A session shorter than the window is walked entirely.
	if got := sessionTitle(build(0, titleScanEvents)); got != "the name" {
		t.Errorf("short session: got %q, want %q", got, "the name")
	}
}

// A <transcript> ENVELOPE NAMES NOTHING. Observed live: a session titled `\", \"` — the fold
// reducing a wall of quoted JSONL to its punctuation. Discarded, so the walk reaches a real title
// behind it.
func TestSessionTitle_TranscriptEnvelopeDiscarded(t *testing.T) {
	// The shape from the live report, and a fuller one.
	for _, tc := range []struct{ name, in string }{
		{"reported shape", `<transcript> \", \" </transcript>`},
		{"jsonl body", `<transcript> {"type":"user","message":{"role":"user","content":"hi"}} </transcript>`},
		{"unterminated", `<transcript> {"type":"user"`},
		{"reminder in front", "<system-reminder>ctx</system-reminder>" + `<transcript> \", \" </transcript>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Alone, it names nothing at all.
			if got := sessionTitle([]pipeline.SessionEvent{userEvent(tc.in)}); got != "" {
				t.Errorf("alone: got %q, want %q", got, "")
			}
			// Behind a real ask, in the same event and in an earlier one.
			if got := sessionTitle([]pipeline.SessionEvent{userEvent("the real ask", tc.in)}); got != "the real ask" {
				t.Errorf("same event: got %q, want %q", got, "the real ask")
			}
			two := []pipeline.SessionEvent{userEvent("the real ask"), userEvent(tc.in)}
			if got := sessionTitle(two); got != "the real ask" {
				t.Errorf("earlier event: got %q, want %q", got, "the real ask")
			}
		})
	}
}

// THE ANCHOR IS THE RULE. Of the user messages in local transcripts that mention this tag, the only
// one is a bug report QUOTING it — which is a perfectly good title — so an unanchored match would
// discard exactly the message a reader wants. Pinned because "discard anything containing
// <transcript>" is the obvious next simplification and it is wrong.
func TestSessionTitle_ProseMentioningTranscriptSurvives(t *testing.T) {
	for _, in := range []string{
		"what does <transcript> mean here",
		"the title comes from a content that begins with <transcript>",
		"why is <transcript> being discarded",
	} {
		if got := sessionTitle([]pipeline.SessionEvent{userEvent(in)}); got != in {
			t.Errorf("sessionTitle(%q) = %q, want it kept verbatim", in, got)
		}
	}
}

// LAST MATCH IN EVENT ORDER WINS (title.go's stated rule), even when a reminder hides the later
// match's /rename prefix. quickRank used HasPrefix, so the reminder-prefixed rename guessed rank
// 2 and the earlier bare rename took it.
func TestSessionTitle_LaterReminderPrefixedRenameWins(t *testing.T) {
	msgs := []string{renameMsg("early"), "<system-reminder>n</system-reminder>" + renameMsg("late")}
	if got := sessionTitle([]pipeline.SessionEvent{userEvent(msgs...)}); got != "late" {
		t.Errorf("got %q, want %q — a later reminder-prefixed /rename lost to an earlier one", got, "late")
	}
	// Across events too, where the outer walk's reverse order should already favour the later.
	two := []pipeline.SessionEvent{
		userEvent(renameMsg("early")),
		userEvent("<system-reminder>n</system-reminder>" + renameMsg("late")),
	}
	if got := sessionTitle(two); got != "late" {
		t.Errorf("across events: got %q, want %q", got, "late")
	}
}

// Nested reminder blocks must be matched by DEPTH. Pairing each open with the next close leaked
// the outer closing tag into the title and dropped the text between the two open tags.
func TestSessionTitle_NestedReminders(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{
			"nested block leaves no stray tag",
			"prose <system-reminder>a<system-reminder>b</system-reminder> outer-tail</system-reminder> real ask",
			"prose real ask",
		},
		{
			"nested block spanning to the end",
			"<system-reminder>a<system-reminder>b</system-reminder>c</system-reminder>the ask",
			"the ask",
		},
		{
			// Unbalanced: two opens, one close. The remainder is kept verbatim, matching the
			// unterminated policy TestSessionTitle_UnterminatedReminder pins.
			"unbalanced opens keep the remainder",
			"<system-reminder>a<system-reminder>b</system-reminder>c",
			"<system-reminder>a<system-reminder>b</system-reminder>c",
		},
		{
			"three deep",
			"head <system-reminder>1<system-reminder>2<system-reminder>3</system-reminder></system-reminder></system-reminder> tail",
			"head tail",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionTitle([]pipeline.SessionEvent{userEvent(tc.in)}); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
	// No stray closing tag may survive in any title this function produces.
	got := sessionTitle([]pipeline.SessionEvent{userEvent(
		"prose <system-reminder>a<system-reminder>b</system-reminder> t</system-reminder> ask")})
	if strings.Contains(got, reminderClose) || strings.Contains(got, reminderOpen) {
		t.Errorf("title leaked a reminder tag: %q", got)
	}
}
