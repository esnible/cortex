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
func titleCandidate(e *pipeline.SessionEvent) (int, string) {
	if e.Inference == nil {
		return rankNone, ""
	}
	rank, title := rankNone, ""
	for i := range e.Inference.Messages {
		if e.Inference.Messages[i].Role != capabilities.RoleUser {
			continue
		}
		// Forward within one event, so `<=` — a later message at equal rank wins. The
		// opposite of the reverse outer loop's `>=`; both mean "last in event order".
		if r, t := titleFrom(e.Inference.Messages[i].Content); t != "" && r <= rank {
			rank, title = r, t
		}
	}
	return rank, title
}

func titleFrom(content string) (int, string) {
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
	// and must not be chosen as the title.
	if content != "" {
		return rankUserMsg, content
	}
	return rankNone, ""
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
