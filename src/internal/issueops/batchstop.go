package issueops

import (
	"context"
	"errors"
)

// ContextStop classifies cancellation/deadline errors using the batch tools'
// existing codes. It does not establish whether a commit landed. An atomic
// version-checked close may be resent with the old expectedVersion: a landed
// close refuses that replay. force=true requires a re-read first.
//
// Single-tool update also uses this classifier. Its cancellation message
// requires a re-read because the change may have committed as a whole
// before the stop was seen. Batch update uses updateRowContextStop for its
// row message.
func ContextStop(parent, loop context.Context, err error) (code, message string, ok bool) {
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return "", "", false
	}
	code, _, stop := BatchStopReason(parent, loop)
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

// BatchStopReason says whether the loop must stop before running another
// row, and with which code the rows that did not run are marked.
//
// The two cases are reported apart on purpose. Until now both arrived as
// internal_error with "request cancelled", which is also what a genuine
// store failure returns — so a caller could not tell "the server gave up
// on schedule and is telling you where it stopped" from "something broke
// while writing this row". They call for different responses: the first is
// retried for the remaining rows, the second is not retried blindly.
func BatchStopReason(parent, loop context.Context) (code, message string, stop bool) {
	if parent.Err() != nil {
		// The caller is gone (client disconnect, proxy teardown). Marking
		// the rows still matters — the result is written into the log line
		// and the audit trail even when nobody reads the response.
		return ErrCodeRequestCancelled,
			"the caller went away before this row ran; rows reported ok above were applied", true
	}
	if loop.Err() != nil {
		return ErrCodeBatchDeadlineExceeded,
			"the batch reached its server-side deadline before this row ran; rows reported ok above were applied and are not rolled back", true
	}
	return "", "", false
}
