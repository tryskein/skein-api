package tools

import (
	"context"
	"errors"
	"log/slog"

	"skein-api/internal/issues"
)

// sk-607 — a write that committed is a write that happened.
//
// The create, epic-create and close writers read the post-state back after
// the commit. That read now runs detached from the request's context, and
// when even it fails the store answers *issues.ErrReadBackFailed carrying
// the state the committing transaction read. The helpers below turn that
// into what the caller is owed: the acknowledgement of the write, not
// internal_error — which a retrying agent reads as "nothing happened" and
// answers by creating the epic again or re-closing over the reason.

// asReadBackFailed reports whether err is a committed write whose read-back
// failed.
func asReadBackFailed(err error) (*issues.ErrReadBackFailed, bool) {
	var rb *issues.ErrReadBackFailed
	if errors.As(err, &rb) {
		return rb, true
	}
	return nil, false
}

// ackSourceFromCommitted turns a committed state into the input of the
// acknowledgement builders (ack.go#ackFromIssue, #ackFromClosedIssue). It
// fills exactly the fields those builders read — id, version, status,
// assignee, the lock / claim / close stamps and the close outcome — so the
// acknowledgement it yields is the one the full card would have yielded.
//
// It is NOT a card: every hydrated field is empty. It must only ever feed
// an acknowledgement, never be served under `verbose`, which promises the
// full IssueDetail. The callers reach it on the non-verbose branch only.
func ackSourceFromCommitted(c issues.CommittedState) *issues.IssueDetail {
	return &issues.IssueDetail{
		ID:           c.ID,
		Version:      c.Version,
		Status:       c.Status,
		Assignee:     c.Assignee,
		LockedBy:     c.LockedBy,
		LockedAt:     c.LockedAt,
		StartedAt:    c.StartedAt,
		ClosedAt:     c.ClosedAt,
		CloseOutcome: c.CloseOutcome,
	}
}

// logReadBackFallback records that a write was acknowledged from its
// committed state. Warn, not error: the write succeeded and the caller got
// a correct acknowledgement; what failed is a read.
func logReadBackFallback(ctx context.Context, tool, userID string, rb *issues.ErrReadBackFailed) {
	slog.WarnContext(ctx, "mcp tool warning",
		"tool", tool,
		"reason", "read-back after commit failed; acknowledged from the committed state",
		"userId", userID, "issueId", rb.State.ID, "version", rb.State.Version,
		"error", rb.Cause.Error())
}

// cardUnavailableKey is the top-level response key that marks a `verbose`
// answer whose card could not be read back after the write committed
// (sk-607). The answer is then the acknowledgement — the write applied, at
// that version — and the key says the full card the caller asked for is
// not in it. It is a success, not an error: an error tells a retrying agent
// that nothing happened. The key is absent on every other answer.
const cardUnavailableKey = "cardUnavailable"

// cardUnavailableNote is appended to the `verbose` parameter description of
// the four tools that can answer with cardUnavailableKey.
const cardUnavailableNote = " If the write applied but its card could not be read back, " +
	"the answer is the acknowledgement plus `cardUnavailable: true` — read the card " +
	"with skein_show; do not repeat the write."

// contextStop classifies cancellation/deadline errors using the batch tools'
// existing codes. It does not establish whether a commit landed. An atomic
// version-checked close may be resent with the old expectedVersion: a landed
// close refuses that replay. force=true requires a re-read first.
//
// Single-tool update also uses this classifier. Its cancellation message
// requires a re-read because earlier sub-transactions may have committed;
// resend only missing changes. Batch update uses updateRowContextStop for
// its partial-step message.
func contextStop(parent, loop context.Context, err error) (code, message string, ok bool) {
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return "", "", false
	}
	code, _, stop := batchStopReason(parent, loop)
	if !stop {
		return "", "", false
	}
	if code == ErrCodeBatchDeadlineExceeded {
		return code, "the batch reached its server-side deadline while this row was in progress; " +
			"it did not report a commit — re-read it, or resend it with the same expectedVersion", true
	}
	return code, "the request ended while this write was in progress; it did not report a commit — " +
		"re-read before repeating it", true
}
