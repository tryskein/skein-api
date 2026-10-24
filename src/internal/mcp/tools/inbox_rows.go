package tools

// inbox_rows.go — the MCP projection of an inbox row (sk-282 T019, spec
// US-7).
//
// Two cuts, both on values rather than on keys, and both only here — the
// REST inbox feeds the web UI and INV-2 freezes its values, so neither may
// be made in the notifications store.
//
// # 1. The old/new values
//
// A status change carries «open» and «in_progress»; a description edit
// carries the whole description, twice. The second case is what makes a
// single inbox row larger than the issue it is about.
//
// TWO limits apply, and they answer different questions.
//
// The first is the per-value ceiling, InboxValuePreviewMaxRunes — not a
// new number, it is the preview limit the system already applies on the
// monitor-poll path (R-8). It answers "how much of one value is worth
// carrying at all».
//
// The second is the ROW budget, and it is what makes SC-005 true rather
// than lucky. The ceiling alone bounds the pair at 2 × 1 024 runes, which
// is already over SC-005's "a row under 1 500 characters" before a single
// byte of the service part is counted — the criterion held only while
// the fixture happened to be narrow (a 25-character title and an empty
// oldValue). A 100-character title, or the non-empty oldValue that almost
// every real `issue.updated` on a description carries, put the row at
// 1 544 and 2 493 characters respectively. So the pair is cut a second
// time, against what is LEFT of the budget once the service part and
// the title are on the wire — the same shift SC-002 applies for the title,
// made here in the implementation instead of in the text of the criterion.
// The result holds on any input rather than on the measured one.
//
// The INV-1 marker is set when either cut actually dropped something. A
// status change is therefore untouched and unmarked.
//
// # 2. The summary
//
// The summary is rendered as «<actor> commented on <id> (<title>)», and
// both the id and the title are ALREADY on the row structurally, under
// entity.id and entity.title. An agent parsing the row pays for them
// twice. The entity reference is replaced with the neutral placeholder the
// summary renderer itself uses when it has no reference, so the sentence
// stays grammatical and the structured fields remain the single source of
// the identifiers.
//
// The actor is left alone: US-7 names the identifier and the title, and
// removing the actor as well would be a wider cut than the spec asked for.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"skein-api/internal/notifications"

	"skein-api/internal/spaces"
)

// inboxValuesTruncatedKey is the INV-1 marker for the value cut.
//
// Deliberately NOT the DTO's existing `truncated` key: that one belongs to
// the monitor-poll comment preview, and reusing it would make one name
// mean two different cuts on two different paths.
const inboxValuesTruncatedKey = "valuesTruncated"

// inboxValueKeys are the two row keys the value cut applies to, in the
// order the row is walked. Named once so the ceiling pass, the budget pass
// and the marker cannot disagree about what «the pair» is.
var inboxValueKeys = []string{"oldValue", "newValue"}

// sc005RowBudgetRunes is SC-005 as a number: "a 5-kilobyte comment
// notification yields an inbox row UNDER 1 500 characters."
//
// Exclusive — 1 500 itself fails — so everything below aims at
// sc005RowBudgetRunes-1. The unit is runes on the row's own JSON encoding,
// which is what the criterion counts and what the acceptance test
// measures.
const sc005RowBudgetRunes = 1500

// inboxEntityPlaceholder is what an entity reference becomes once removed
// from the summary. It is the same wording notifications.FormatSummary
// substitutes when it is given no reference at all, so the projected
// sentence is one the renderer could itself have produced.
const inboxEntityPlaceholder = "this issue"

// inboxResponse is the MCP wire shape of an inbox page.
type inboxResponse struct {
	Items      []map[string]any `json:"items"`
	NextCursor *string          `json:"nextCursor,omitempty"`
	HasMore    bool             `json:"hasMore"`
	// Invitations are the pending space invitations to the caller, on the
	// first page of the unread and all scopes (sk-674.18 T029, US-5) —
	// never the token or the link; accepting is the person's act in the
	// web UI (skein_accept refuses an invitation id). Absent when none.
	Invitations []spaces.PendingInvitation `json:"invitations,omitempty"`
}

// entityRefOf rebuilds the reference string the store embedded into the
// summary. It mirrors the store's own construction: the id alone, or the
// id followed by the title in parentheses.
func entityRefOf(n notifications.NotificationDTO) string {
	if n.Entity.Title != "" {
		return n.Entity.ID + " (" + n.Entity.Title + ")"
	}
	return n.Entity.ID
}

// dedupeSummary removes the entity reference from a summary.
//
// If the reference is not found the summary is returned unchanged: some
// event types render without one, and rewriting a sentence we do not
// recognise would be guessing.
func dedupeSummary(summary string, n notifications.NotificationDTO) string {
	ref := entityRefOf(n)
	if ref == "" || !strings.Contains(summary, ref) {
		return summary
	}
	return strings.ReplaceAll(summary, ref, inboxEntityPlaceholder)
}

// truncateInboxValue cuts one old/new value.
//
// Only STRING values are cut. A status, a priority or a boolean is already
// short, and a number rendered through the truncation rule would come back
// as a string — changing the type of a field on the wire, which is a
// bigger break than the bytes it would save.
func truncateInboxValue(v any) (any, bool) {
	s, isString := v.(string)
	if !isString {
		return v, false
	}
	preview, truncated := TruncateRunes(s, InboxValuePreviewMaxRunes)
	return preview, truncated
}

// projectInboxRow projects one notification.
func projectInboxRow(n notifications.NotificationDTO) (map[string]any, error) {
	b, err := json.Marshal(n)
	if err != nil {
		return nil, fmt.Errorf("marshal notification: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("unmarshal notification: %w", err)
	}

	if summary, ok := m["summary"].(string); ok {
		m["summary"] = dedupeSummary(summary, n)
	}

	cut := false
	for _, key := range inboxValueKeys {
		raw, present := m[key]
		if !present {
			continue
		}
		projected, truncated := truncateInboxValue(raw)
		m[key] = projected
		cut = cut || truncated
	}
	if cut {
		m[inboxValuesTruncatedKey] = true
	}

	cut, err = fitInboxRowToBudget(m, cut)
	if err != nil {
		return nil, err
	}
	if cut {
		m[inboxValuesTruncatedKey] = true
	} else {
		delete(m, inboxValuesTruncatedKey)
	}
	return m, nil
}

// fitInboxRowToBudget cuts the old/new pair until the whole row fits the
// SC-005 budget, and reports whether anything was dropped by either pass.
//
// The order — measure, then derive the pair's share from what is left — is
// the point. The share is not a second constant: it is the budget minus
// what the row costs with the values emptied, so the service part and
// the title are paid for first and the pair gets the remainder. That is
// the same shift SC-002 states for the title, applied here rather than
// written into the criterion.
//
// The loop re-measures because a rune is not a character on the wire: a
// quote, a backslash or a control character encodes as two or six, so a
// share computed on plain runes can still overflow. Each pass subtracts
// the measured overflow from the share, so it converges — every dropped
// rune removes at least one encoded rune.
//
// # The edge this cannot fix, stated rather than hidden
//
// The title is not truncatable — the spec forbids it (US-2/SC-002: the title
// is not subject to truncation, and titles are allowed up to 1 000 characters). So
// a row whose service part plus title already exceeds the budget cannot
// be brought under it by cutting the pair: the values go to empty, the
// marker says so, and the row is still over. That outcome is honest — the
// alternative is a truncated title, which is a bigger lie than a long
// row — and it is the only input on which SC-005 does not hold.
func fitInboxRowToBudget(m map[string]any, cut bool) (bool, error) {
	const limit = sc005RowBudgetRunes - 1

	size, err := encodedRunes(m)
	if err != nil {
		return cut, err
	}
	if size <= limit {
		return cut, nil
	}

	keys := make([]string, 0, len(inboxValueKeys))
	values := make(map[string]string, len(inboxValueKeys))
	for _, key := range inboxValueKeys {
		s, isString := m[key].(string)
		if !isString {
			continue
		}
		keys = append(keys, key)
		values[key] = s
	}
	if len(keys) == 0 {
		// Nothing on this row may be cut — see the edge above.
		return cut, nil
	}

	// The marker costs bytes of its own, so it is on the row while the
	// overhead is measured; if the budget pass turns out to drop nothing
	// after all, the caller removes it again (INV-1: the marker is set if
	// and only if data was actually dropped).
	m[inboxValuesTruncatedKey] = true
	for _, key := range keys {
		m[key] = ""
	}
	overhead, err := encodedRunes(m)
	if err != nil {
		return cut, err
	}

	budget := limit - overhead
	for budget > 0 {
		cut = applyPairBudget(m, keys, values, budget) || cut
		size, err := encodedRunes(m)
		if err != nil {
			return cut, err
		}
		over := size - limit
		if over <= 0 {
			return cut, nil
		}
		budget -= over
	}

	// Budget exhausted: the pair is gone and the row is still too long.
	for _, key := range keys {
		m[key] = ""
		if values[key] != "" {
			cut = true
		}
	}
	return cut, nil
}

// applyPairBudget writes the budgeted prefixes of the pair onto the row
// and reports whether any of them lost anything.
func applyPairBudget(m map[string]any, keys []string, values map[string]string, budget int) bool {
	cut := false
	for key, share := range splitPairBudget(keys, values, budget) {
		prefix, truncated := TruncateRunes(values[key], share)
		m[key] = prefix
		cut = cut || truncated
	}
	return cut
}

// splitPairBudget shares `budget` runes between the pair.
//
// Shortest first: a value that fits inside its equal share takes only what
// it needs and releases the rest to the other one. An even split would let
// a four-rune `oldValue` («open») reserve half the row and cut `newValue`
// twice as hard as necessary.
func splitPairBudget(keys []string, values map[string]string, budget int) map[string]int {
	order := append([]string(nil), keys...)
	sort.SliceStable(order, func(i, j int) bool {
		return utf8.RuneCountInString(values[order[i]]) < utf8.RuneCountInString(values[order[j]])
	})

	out := make(map[string]int, len(order))
	remaining, left := budget, len(order)
	for _, key := range order {
		share := remaining / left
		if n := utf8.RuneCountInString(values[key]); n < share {
			share = n
		}
		out[key] = share
		remaining -= share
		left--
	}
	return out
}

// encodedRunes is the row's size in the unit SC-005 counts: runes of its
// own JSON encoding. Measured on the real encoding rather than on the
// values, because escaping is what makes the two differ.
func encodedRunes(m map[string]any) (int, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return 0, fmt.Errorf("measure inbox row: %w", err)
	}
	return utf8.RuneCount(b), nil
}

// projectInboxRows projects a page, preserving order.
func projectInboxRows(items []notifications.NotificationDTO) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(items))
	for i := range items {
		row, err := projectInboxRow(items[i])
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}
