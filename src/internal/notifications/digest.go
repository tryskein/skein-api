package notifications

// digest.go — sk-303.14, task T015.
//
// The digest-event payload contract and the "row reason → reminder category"
// mapping.
//
// # Why this lives here, next to the registry
//
// The digest event types (EventTypeUnacceptedDigest / EventTypeUnreviewedDigest)
// are declared in reasons.go for the reason recorded there: the classifier is
// the one place that must know EVERY type, and the producers (the reminder
// worker, T017–T018) come later and read the constants instead of re-typing
// string literals. The payload shape obeys exactly the same reason: the
// reminder worker WRITES it, the delivery tract (internal/outbox) READS it,
// and those are two different packages, neither of which should depend on the
// other. The shared vocabulary is here.
//
// # Why the payload carries only identifiers
//
// The category is NOT put into it, even though there is room. The category is
// already derived from the notification row's reason (the registry above:
// EventTypeUnacceptedDigest → ReasonKeyUnacceptedReminder →
// CategoryUnaccepted), and a second copy of the same fact in a neighbouring
// column is exactly how two sources of truth drift: the row is edited, the
// payload is forgotten, and the tract starts re-checking unaccepted issues
// with the unreviewed predicate. The reader takes the category from the
// reason — the same place the unsubscribe check takes it.
//
// Issue titles are not put in the payload either. A title taken at selection
// time is already wrong by send time, and INV-14 requires a re-check at
// delivery: the tract re-reads both the identifier and the title, on the
// category predicate, and an inaccessible issue is not disclosed by either.

import (
	"encoding/json"
	"fmt"

	"skein-api/internal/review"
)

// DigestPayload is the digest-event payload (events.payload).
//
// The identifiers are a SNAPSHOT AT SELECTION TIME, not a promise of the
// notification's contents: the tract must re-check every element at delivery
// and may drop any of them or all of them. The count shown to the person is
// computed AFTER the re-check, so the length of this array is not a number in
// the notification text.
type DigestPayload struct {
	// IssueIDs — hierarchical identifiers of the issues that fell into the
	// selection. Order does not matter: the tract imposes its own
	// deterministic order so that "the same set of issues yields the same
	// text" holds independently of the order the selection returned them.
	IssueIDs []string `json:"issueIds"`
}

// MarshalDigestPayload serialises the payload for the events.payload column.
func MarshalDigestPayload(p DigestPayload) ([]byte, error) {
	if p.IssueIDs == nil {
		p.IssueIDs = []string{}
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("marshalling digest payload: %w", err)
	}
	return b, nil
}

// ParseDigestPayload reads a digest-event payload.
//
// Empty input (SQL NULL in a column that is NOT NULL DEFAULT '{}' by schema)
// and a payload without the key yield an empty list WITHOUT error: that is
// not a format breakage, it is a digest with nothing to show, and the reader
// must tell "could not read" from "read it, it is empty" — the first is
// answered with a retry, the second by finishing the row.
func ParseDigestPayload(raw []byte) (DigestPayload, error) {
	if len(raw) == 0 {
		return DigestPayload{IssueIDs: []string{}}, nil
	}
	var p DigestPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return DigestPayload{}, fmt.Errorf("parsing digest payload: %w", err)
	}
	if p.IssueIDs == nil {
		p.IssueIDs = []string{}
	}
	return p, nil
}

// DigestCategoryForReason translates a notification row's reason into a
// reminder category — and thereby answers "is this a digest at all?".
//
// The second return is false for EVERY reason except the two: the tract
// branches on it, so "unknown reason" here means "ordinary single-issue
// delivery", not "error". A new kind of digest MUST appear in this table —
// otherwise it silently takes the single-issue path and is finalised without
// a send, because the user event does not resolve to any issue.
//
// Categories are NOT merged (R-4): each has its own reason, its own
// unsubscribe and its own transition route, so the table is exactly two rows,
// not one shared "this is a reminder" flag.
func DigestCategoryForReason(key ReasonKey) (review.Category, bool) {
	switch key {
	case ReasonKeyUnacceptedReminder:
		return review.CategoryUnaccepted, true
	case ReasonKeyReviewReminder:
		return review.CategoryUnreviewed, true
	default:
		return "", false
	}
}

// DigestReasonForCategory is the reverse translation, for the producer: the
// reminder worker knows the category and must stamp the row with the reason
// the tract will later recognise it by.
func DigestReasonForCategory(cat review.Category) (ReasonKey, bool) {
	switch cat {
	case review.CategoryUnaccepted:
		return ReasonKeyUnacceptedReminder, true
	case review.CategoryUnreviewed:
		return ReasonKeyReviewReminder, true
	default:
		return "", false
	}
}

// DigestEventTypeForCategory is the event type the producer writes into the
// audit for this category. The third translation of the same pair, and all
// three must agree: event type → reason (the classifier registry), reason →
// category (above), category → event type (here).
// Returns a PRIVATE-WRITER PASS (PrivateEventType), not a string: a digest
// event row is written in the user form and must not go onto the bus (repair
// A/8), and the only way to obtain a value of this type is registration in
// the private-type registry. A string type would let a private form be
// written past the registry, which is exactly how this pair of types once
// leaked to every administrator.
func DigestEventTypeForCategory(cat review.Category) (PrivateEventType, bool) {
	switch cat {
	case review.CategoryUnaccepted:
		return PrivateUnacceptedDigest, true
	case review.CategoryUnreviewed:
		return PrivateUnreviewedDigest, true
	default:
		return PrivateEventType{}, false
	}
}
