package session

import (
	"strings"
	"unicode"

	"github.com/rossoctl/cortex/core/capabilities"
	"github.com/rossoctl/cortex/core/pipeline"
)

const emptySessionTitle = "(empty session)"

// maxTitleLen caps a title, in RUNES. A user message is unbounded — the largest measured
// event carries ~190KB — and this is served on every /v1/sessions response, whose size is
// already the constraint that `?before=` exists for.
//
// NOT A DISPLAY-COLUMN BUDGET: 80 runes of CJK occupy 160 columns, so a client still has to
// truncate by width. Runes, not bytes, so a multi-byte title is not cut mid-character.
//
// Deliberately the same 80 as core/observe/claude.MaxTitleLen, and deliberately NOT that
// constant: that one is documented as the cap on a HARVESTED title and is exported so
// cmd/abctl's tests can hold a contract with the harvester. Importing it here would make the
// session store depend on the transcript harvester for a number, and would couple two caps
// that answer to different consumers. If they ever need to differ, they can.
const maxTitleLen = 80

// Title sources, best first. Within one rank the last match in event order wins.
const (
	rankRename    = 0
	rankUserQuery = 1
	rankUserMsg   = 2
	rankNone      = 3
)

const renamePrefix = "<command-name>/rename</command-name>"

// sessionTitle names a session from its own events: a /rename envelope if one is present,
// else a <user_query> block, else the last user message carrying text.
func sessionTitle(events []pipeline.SessionEvent) string {
	if len(events) == 0 {
		return emptySessionTitle
	}
	best, bestRank := "", rankNone
	for i := len(events) - 1; i >= 0; i-- {
		// Reverse, so the FIRST hit at a rank is the LAST one in event order — hence
		// `>=` rather than `>`: an earlier event must not displace it at equal rank.
		r, t := titleCandidate(&events[i])
		if t == "" || r >= bestRank {
			continue
		}
		best, bestRank = t, r
		if bestRank == rankRename {
			break // nothing can outrank it
		}
	}
	if bestRank == rankNone {
		return ""
	}
	return sanitizeTitle(best)
}

// titleCandidate returns the best-ranked title this event's user messages offer.
//
// Two passes, because titleFrom is O(len(content)) and this is called for every event: the
// first pass finds WHICH message wins using quickRank, which is O(1) on the rank-2 majority,
// and only the winner goes through titleFrom. A 300-turn session walks ~45k messages and
// titles from one of them.
func titleCandidate(e *pipeline.SessionEvent) (int, string) {
	if e.Inference == nil {
		return rankNone, ""
	}
	// rejected holds the messages titleFrom has already refused, so each retry considers one
	// fewer and the loop terminates. Allocated only on the first rejection, which is the rare
	// path: nil-safe reads make the common case a map that never exists.
	var rejected map[int]bool
	for {
		rank, pick := rankNone, -1
		for i := range e.Inference.Messages {
			if e.Inference.Messages[i].Role != capabilities.RoleUser || rejected[i] {
				continue
			}
			// Forward within one event, so `<=` — a later message at equal rank wins. The
			// opposite of the reverse outer loop's `>=`; both mean "last in event order".
			if r := quickRank(e.Inference.Messages[i].Content); r <= rank {
				rank, pick = r, i
			}
		}
		if pick < 0 {
			return rankNone, ""
		}
		// quickRank over-promises: a message it called rank 2 can turn out to be a /rename
		// behind a reminder, and one it called rank 0 or 2 can turn out to name nothing at all
		// (empty /rename args, or nothing but reminders). titleFrom settles it.
		if r, t := titleFrom(e.Inference.Messages[pick].Content); t != "" {
			return r, t
		}
		// A DEMOTION MUST NOT LOSE THE EVENT. An earlier message here may still title it —
		// TestSessionTitle_EmptyCommandArgs and the reminder-only case both put the rejected
		// message LAST, so returning now would answer "" with a real title sitting in front of
		// it. Retry without the message just judged.
		if rejected == nil {
			rejected = make(map[int]bool, 1)
		}
		rejected[pick] = true
	}
}

// quickRank guesses a message's rank without stripping reminders, cheaply enough to run on
// every message of every event. It is an UPPER BOUND on quality: the rank titleFrom settles on
// can only be better (a /rename or <user_query> hiding behind a reminder) or rankNone (the
// message was nothing but reminders). Never worse — stripping cannot introduce prose.
//
// So a caller may use it to choose a candidate, but must take the rank from titleFrom.
func quickRank(content string) int {
	if content == "" {
		return rankNone
	}
	if strings.HasPrefix(content, renamePrefix) {
		return rankRename
	}
	if strings.Contains(content, "<user_query>") {
		return rankUserQuery
	}
	return rankUserMsg
}

// titleFrom ranks one user message and returns the title it offers.
//
// <system-reminder> blocks are excised first, before every rank arm rather than only the
// rank-2 one. All three arms need it and two are wrong without it: a reminder nested INSIDE a
// <user_query> would ride along into the title, and a <user_query> nested inside a REMINDER —
// one quoting an earlier turn is enough — would be mistaken for the real ask. The /rename arm
// tests a PREFIX, so a reminder in front of an envelope hides it entirely.
//
// THE STRIP IS NOT FREE, which is why titleCandidate calls this on ONE message per event
// rather than on all of them. Excising a tag that can sit anywhere means scanning the whole
// message and building a new string; calling it per message cost +70% on
// BenchmarkListSessions_Title/user-text, which walks ~45k messages of ~500 bytes. quickRank
// picks the candidate instead, and this settles its rank.
func titleFrom(content string) (int, string) {
	content = stripReminders(content)
	if strings.HasPrefix(content, renamePrefix) {
		// A /rename envelope is machinery, not prose, so it yields its ARGUMENT OR NOTHING
		// and never falls through to a lower rank. Falling through was the first attempt and
		// it was worse than the bug it avoided: the generic rank-2 arm below then took the
		// whole literal "<command-name>/rename</command-name><command-args></command-args>"
		// as the title. Returning rankNone lets the walk reach a real title behind it, which
		// is the thing an argument-less /rename must not defeat.
		if t := between(content, "<command-args>", "</command-args>"); t != "" {
			return rankRename, t
		}
		return rankNone, ""
	}
	if t := between(content, "<user_query>", "</user_query>"); t != "" {
		return rankUserQuery, t
	}
	// A user-role message whose payload was a tool result or an image flattens to "" (see
	// pipeline.InferenceMessage.ContentBytes) — it is the last message of every agentic turn
	// and must not be chosen as the title. A message that was nothing BUT reminders reaches
	// here as "" too, and falls through by the same route: rankNone, so the walk finds a real
	// title behind it.
	if content != "" {
		return rankUserMsg, content
	}
	return rankNone, ""
}

const (
	reminderOpen  = "<system-reminder>"
	reminderClose = "</system-reminder>"
)

// stripReminders excises every <system-reminder>…</system-reminder> span and returns what
// surrounds them. The harness injects these blocks into the user turn it is attached to, so
// without this the block itself becomes the title and the real prompt — which sits after it —
// is never reached.
//
// SPLICES rather than keeping only the tail: of the user turns in local transcripts that carry
// a reminder alongside real prose, 67 of 68 have text on BOTH sides of the block. The seam is
// left as-is; sanitizeTitle folds "before\n" + "\nafter" to one space later.
//
// Called once at the top of titleFrom, so every rank arm sees content with the blocks already
// gone — see the comment there for why that matters to all three and not just rank 2.
//
// An UNTERMINATED open tag is left alone, matching the policy between() fixes for <user_query>:
// the tag name can appear in ordinary prose (someone discussing this very code), so the whole
// message stays a reasonable title rather than being cut at a tag that closes nothing.
//
// Matched literally in lowercase, which is the only form the harness emits into an inference
// payload. core/observe/claude canonicalises tag names because a capitalised variant reached
// its filter from a transcript; that is a different producer, and importing its machinery here
// would be the wrong dependency direction for a string scan.
//
// Runs on RAW content, before sanitizeTitle — folding U+0085 to a space would turn
// "<\u0085system-reminder>" into a non-tag and hide the block from this scan.
func stripReminders(s string) string {
	// The common case is no reminder at all, and this runs on every message of every event: a
	// session whose best rank is 2 can never terminate the reverse walk early, since rank 2 is
	// always beatable. One Index over the message beats building a string for each.
	if !strings.Contains(s, reminderOpen) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for {
		i := strings.Index(s, reminderOpen)
		if i < 0 {
			break
		}
		rest := s[i+len(reminderOpen):]
		j := strings.Index(rest, reminderClose)
		if j < 0 {
			break // unterminated: keep the remainder verbatim, tag and all
		}
		b.WriteString(s[:i])
		s = rest[j+len(reminderClose):]
	}
	b.WriteString(s)
	return b.String()
}

// between returns the text bracketed by open and closing, or "" if either is absent.
//
// Hand-parsed rather than a regexp: matching a tag pair wants a backreference, which RE2
// lacks. A local copy of core/observe/claude's unexported helper — importing a transcript
// harvester into the session store for a string helper is the wrong dependency direction.
// Unlike the original this one does not TrimSpace; sanitizeTitle trims at the end.
func between(s, open, closing string) string {
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, closing)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// sanitizeTitle folds a title to a single line: every whitespace or control rune becomes at
// most one U+0020, and the result is clipped to maxTitleLen runes.
//
// Applied ONCE, to the winner — not per candidate, which would be O(content) on every hit.
// Rank never depends on whitespace, so comparing raw strings is safe. And applied to the
// EXTRACTED title, never the haystack: folding U+0085 to a space turns "<\u0085command-name>"
// into "< command-name>", which is no longer a tag.
func sanitizeTitle(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := true // drops leading whitespace
	for _, r := range s {
		// TWO PREDICATES. pipeline.IsControlRune covers C0/C1/DEL plus the BIDI and
		// zero-width runes that reorder or hide their surroundings; unicode.IsSpace covers
		// \n\r\t and the exotic spaces (U+00A0, U+3000, the U+2000 block), which are not
		// controls. Both FOLD rather than drop, so "one\ntwo" does not become "onetwo".
		if unicode.IsSpace(r) || pipeline.IsControlRune(r) {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	return clipTitle(strings.TrimRight(b.String(), " "))
}

// clipTitle caps s at maxTitleLen runes.
//
// A PLAIN RUNE CUT, which can sever a combining mark from its base character — "e" plus a
// combining acute cut between the two leaves a dangling accent on whatever now precedes it.
// core/observe/claude's clipTitle is exempt because its scrubRunes drops every combining
// mark, modifier and regional indicator first; this sanitizer keeps them, so "café" survives
// as "café" and the cut is the price. Measuring in grapheme clusters instead needs a
// segmentation library core/ does not have, and the cut lands at rune 80 of a title that is
// almost never that long.
//
// TrimRight runs again here because the cut can land just after a folded space.
func clipTitle(s string) string {
	r := []rune(s)
	if len(r) <= maxTitleLen {
		return s
	}
	return strings.TrimRight(string(r[:maxTitleLen]), " ")
}
