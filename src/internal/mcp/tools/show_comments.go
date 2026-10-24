package tools

// show_comments.go — the thread index of skein_show (sk-282 T011, spec
// US-4).
//
// # Why "the whole thread" could not stay "the whole thread"
//
// Asking for the whole thread used to dump it: 48 comments, 64 672
// characters, more than any child graph in the same session. But this call
// is not an ordinary parameter — it is PRESCRIBED. The operational guide
// and the tool's own description tell an agent to re-read the thread in
// full before claiming, on first load, on resume and after a compaction,
// whenever the previous answer said the thread was truncated.
//
// That is why the change has to redefine the completeness flag in the same
// breath. If "whole thread" returned an index while the flag still meant
// "you did not get every body", an agent following the prescription would
// be told again that it is missing something, ask again, and get the index
// again — the cure looping on itself. So the flag's meaning is now exactly
// one thing:
//
//	incomplete ⇔ fewer entries were LISTED than the population has
//
// Under that rule the answer to "whole thread" on a thread below the cap
// is complete, and the loop opens.
//
// # Population, and why the cap applies to it rather than to the thread
//
// "Population" is the set "total" counts: the whole thread, or the time
// window when one is given. All five modes of US-4's table are defined
// against their own population, and the composite mode "window + whole
// thread" needs no row of its own — the window narrows the population and
// the last two rows then apply unchanged. The 200-entry cap therefore
// applies to the WINDOW in that mode, not to the thread behind it.
//
// # One marker, not two
//
// There is deliberately no separate "the index itself was cut" flag. Its
// job is done by the completeness flag, which now has exactly one meaning;
// adding a second marker beside a freshly-disambiguated one is how the
// ambiguity comes back. The per-ENTRY truncation flag is a different thing
// under a different rule (INV-1): it says this comment's text was cut at
// 80 runes, and it is set only when that really happened.

import (
	"time"

	"skein-api/internal/issues"
)

// ShowThreadIndexCap is the safety valve on the index: at most this many
// entries, the most recent OF THE POPULATION.
//
// Without it a long thread walks straight back into the declared result
// ceiling and the outlier merely changes its coefficient. 200 is
// symmetrical with the per-type edge cap.
//
// Past the cap the index does NOT claim to see the whole thread, and that
// is stated rather than implied: entries older than the two-hundredth are
// absent, their ids are absent with them, so they cannot be fetched by id.
// The only route to them is a bulk read of the last N with an N deep
// enough to reach — which is the INV-4 exit US-4 names.
const ShowThreadIndexCap = 200

// commentIndexEntry is one line of the thread index: enough to decide
// whether this comment is worth fetching, and the id to fetch it with.
type commentIndexEntry struct {
	// ID is what skein_comment_get takes — the index is only useful
	// because every entry can be turned back into a full comment.
	ID string `json:"id"`
	// Author and CreatedAt are what an agent scans to find the turn it
	// cares about.
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"createdAt"`
	// Preview is the leading fragment of the body, cut by the shared rule
	// (truncate.go) at ThreadIndexPreviewMaxRunes.
	Preview string `json:"preview"`
	// PreviewTruncated says the body is longer than the fragment. INV-1's
	// second marker rule: set only when the cut really happened, so a
	// comment shorter than the fragment carries nothing.
	PreviewTruncated bool `json:"previewTruncated,omitempty"`
	// DeletedAt makes a tombstone readable AS a tombstone. A deleted
	// comment keeps its slot in the thread and the store already blanks
	// its body, so without this the entry would look like a comment that
	// happens to be empty.
	DeletedAt *time.Time `json:"deletedAt,omitempty"`
}

// buildCommentIndex projects a comment page into index entries, preserving
// order.
//
// Pure: no store, no context — the completeness matrix is a `unit` row and
// an index built inside the handler could only be checked with a database
// up.
func buildCommentIndex(comments []issues.CommentDTO) []commentIndexEntry {
	out := make([]commentIndexEntry, 0, len(comments))
	for i := range comments {
		c := comments[i]
		preview, truncated := TruncateRunes(c.Body, ThreadIndexPreviewMaxRunes)
		out = append(out, commentIndexEntry{
			ID:               c.ID.String(),
			Author:           c.Author,
			CreatedAt:        c.CreatedAt,
			Preview:          preview,
			PreviewTruncated: truncated,
			DeletedAt:        c.DeletedAt,
		})
	}
	return out
}

// showThreadTailSize is how many comments come back in full alongside the
// index. Three — the same number the default read returns, so an agent
// that switched to the index does not lose the recent context it had.
const showThreadTailSize = 3

// threadTail returns the last showThreadTailSize comments of a page.
func threadTail(comments []issues.CommentDTO) []issues.CommentDTO {
	if len(comments) <= showThreadTailSize {
		out := make([]issues.CommentDTO, len(comments))
		copy(out, comments)
		return out
	}
	tail := comments[len(comments)-showThreadTailSize:]
	out := make([]issues.CommentDTO, len(tail))
	copy(out, tail)
	return out
}

// threadIncomplete is US-4's single rule, in one place so the five modes
// cannot drift apart: a listing is reported incomplete when fewer entries
// were listed than the population holds, OR when it is exactly as long as
// the valve that produced it (see capApplied below — at the cap the flag
// is set regardless of total).
//
// It is a function rather than an inline comparison because the whole
// point of the change is that this decision has ONE definition — the
// prescription in the guide keys on it, and a second interpretation
// anywhere is the loop coming back.
//
// # capApplied, and the one race that can make the rule lie
//
// `listed` and `total` do not come from one snapshot: ListComments takes
// the count and the page in two statements, and the spec deliberately
// promises no consistency between them. Almost every ordering of that
// race errs toward FALSE INCOMPLETENESS, which is safe — the caller
// re-reads and gets the same or more. Exactly one errs the other way: if
// the thread grew between the two statements AND the page hit the index
// cap, the result is listed == cap > total, and «fewer listed than exist»
// then declares a CUT listing complete. That is the one answer a caller
// cannot detect and cannot recover from.
//
// So a listing that is exactly as long as the valve that produced it is
// incomplete regardless of total. capApplied is that valve, and it is 0
// in every mode where none is in play: a caller's own commentsLimit is a
// REQUEST, not a valve — asking for the last 3 of a 3-comment thread is
// complete, and marking it incomplete would re-open the very US-8 loop
// this rule exists to close. The cost of the guard is a thread whose
// population is exactly the cap reporting itself incomplete; that is the
// safe direction, and the cap's own documentation already says a listing
// at the cap does not claim to see the whole thread.
func threadIncomplete(listed, total, capApplied int) bool {
	if capApplied > 0 && listed >= capApplied {
		return true
	}
	return listed < total
}
