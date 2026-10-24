package tools

// depedge.go — the thin dependency edge of the MCP response (sk-282 T010,
// spec US-1a).
//
// # What was on the wire and what it was worth
//
// An edge carried both side ids, the type, an always-empty metadata map, a
// version, a creation timestamp, and a nested card of the other side
// including its project id. Measured: about 300 characters per edge, of
// which roughly 90 were useful. The rest is not «extra detail» — a version
// and a created-at on an EDGE answer no question an agent reading an issue
// has, and the empty map is pure syntax.
//
// # What stays, and the one line that decides it
//
// The obvious cut — «keep only the other side's card» — is wrong, and the
// spec says so in as many words: a redacted edge has NO card, so its side
// ids are the only thing identifying it. Cut them unconditionally and an
// unreachable edge becomes an anonymous object whose sole content is the
// flag saying it is unreachable, which is a direct INV-1 violation — the
// caller can no longer tell WHICH link it cannot see.
//
// So the side ids and the redaction flag survive unconditionally, and the
// card is what folds into the edge.
//
// # Why a projection and not a slimmer struct
//
// dependencies.DependencyEdge is serialised by the REST dependencies
// endpoint. INV-2 forbids changing a REST response's field set — including
// additively — so the narrowing happens here, at the MCP wire edge, and
// the shared type is untouched. The REST endpoint stays byte-for-byte what
// it was, which is checked by its own test rather than assumed.

import (
	"encoding/json"
	"fmt"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
)

// depEdge is the MCP projection of one dependency edge.
//
// Field order is the reading order an agent needs: which link this is,
// then who is on the other end.
type depEdge struct {
	// FromID / ToID are the edge's own identity. Kept unconditionally —
	// see the file comment: on a redacted edge they are the only
	// identification there is.
	FromID string `json:"fromId"`
	ToID   string `json:"toId"`
	// Type is the link's character: parent-child, blocks, related, …
	Type string `json:"type"`

	// Title / Status / IssueType / Priority describe the OTHER side,
	// flattened out of the nested card. Omitted entirely on a redacted
	// edge — the caller may not see them, and an empty string would claim
	// the other side has no title.
	Title     string `json:"title,omitempty"`
	Status    string `json:"status,omitempty"`
	IssueType string `json:"issueType,omitempty"`
	Priority  *int   `json:"priority,omitempty"`

	// Redacted marks an edge whose other side lives in a project the
	// caller is not a member of. Set only when true: a flag that is always
	// present carries no information and costs bytes on every edge, and
	// its absence already means «you can see this one» because the
	// descriptive fields are there.
	Redacted bool `json:"redacted,omitempty"`
}

// projectDepEdge is the pure projection. No context, no store, no
// serialisation — which is what lets SC-009 be a `unit` row instead of an
// integration one.
//
// The other side's project id is deliberately dropped: on a visible edge
// the caller already knows the project is one of theirs, and on a redacted
// edge it was never sent. It is the one field of the nested card that
// answers nothing.
func projectDepEdge(e dependencies.DependencyEdge) depEdge {
	out := depEdge{
		FromID:   e.FromID,
		ToID:     e.ToID,
		Type:     e.Type,
		Redacted: e.Redacted,
	}
	// e.Issue is nil exactly when the edge is redacted. Guarding on the
	// pointer rather than on the flag keeps the projection total: a future
	// caller that produced one without the other gets no card rather than
	// a nil dereference.
	if e.Issue != nil {
		out.Title = e.Issue.Title
		out.Status = e.Issue.Status
		out.IssueType = e.Issue.Type
		priority := e.Issue.Priority
		out.Priority = &priority
	}
	return out
}

// projectDepEdges projects a slice, preserving order.
//
// Always returns a non-nil slice so the wire renders `[]` rather than
// `null`: an empty direction means «no edges of the requested types this
// way», and null would read as «this direction was not computed».
func projectDepEdges(edges []dependencies.DependencyEdge) []depEdge {
	out := make([]depEdge, 0, len(edges))
	for _, e := range edges {
		out = append(out, projectDepEdge(e))
	}
	return out
}

// Child-counter keys on the issue card (sk-282 T012, spec US-3). The names
// are fixed by the spec, not chosen here: `unclosedChildrenCount` counts
// WIDER than the epic rollup's open-children field, and reusing that
// field's name for the wider meaning was rejected explicitly.
const (
	IssueCardChildrenCount         = "childrenCount"
	IssueCardUnclosedChildrenCount = "unclosedChildrenCount"
)

// withIssueChildCounts returns the issue card as a JSON map with the two
// US-3 counters added.
//
// It takes `any` because the card reaching this point is either the real
// *issues.IssueDetail or the map issueWithoutDescription already produced,
// and both have to end up with the same two keys.
//
// The counters live in the MCP projection and nowhere else: INV-2 forbids
// adding a key to a REST response even additively, so they must not become
// fields of the shared IssueDetail (D-2).
//
// Both keys are emitted even at zero. US-3 is explicit that a zero has to
// read as «counted, and there are none» — omitting it would make it
// indistinguishable from «not counted», which is the one thing a
// caller must not misread when deciding a batch of work is finished.
func withIssueChildCounts(card any, counts issues.ChildCounts) (map[string]any, error) {
	b, err := json.Marshal(card)
	if err != nil {
		return nil, fmt.Errorf("marshal issue card: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("unmarshal issue card: %w", err)
	}
	if m == nil {
		return nil, fmt.Errorf("issue card projected to a non-object")
	}
	m[IssueCardChildrenCount] = counts.Total
	m[IssueCardUnclosedChildrenCount] = counts.Unclosed
	return m, nil
}
