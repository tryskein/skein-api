package tools

import (
	"context"
	"time"
)

// batchLoopDeadline bounds the per-row loop of a batch tool
// (skein_update_many, skein_close_many, skein_dep_add_many).
//
// # Why a batch tool needs a deadline of its own
//
// These loops apply rows one at a time, each in its own transaction, and
// the response is the only place the caller learns WHICH rows landed.
// Without a deadline the loop runs until the caller's context dies — and
// by then there is nobody left to tell. The MCP proxy in skein-web caps how
// long it waits for this service's response headers: the setting
// SKEIN_MCP_UPSTREAM_HEADERS_TIMEOUT_SECONDS (SkeinApiOptions.
// McpUpstreamHeadersTimeout in McpProxyMiddleware; 120 s by default since
// sk-608, 30 s fixed before it). When that fires it tears down the
// connection to this service, the handler's context is cancelled mid-loop,
// and the rows already committed stay committed while the caller receives
// nothing at all — not a partial result, not an error, an aborted
// response. Observed five times in thirty days on skein_update_many (under
// the old 30 s cap): 181 rows requested, 68 written, 113 reported nowhere.
//
// Stopping on our own clock, safely inside that window, turns a silent
// partial write into a stated one: the loop halts, every row that did not
// run is marked batch_deadline_exceeded, and the response goes out while
// the caller is still listening.
//
// # Why 20 seconds
//
// The budget has to end before the proxy's window does, minus what
// elapses before this handler starts running — authentication, the
// agent-registry upsert and MCP transport setup all precede it and all hit
// the same database, and one measured incident spent 5.7 s there before
// the first row — and minus the response write. It was set against the
// old 30 s cap; against the 120 s default it leaves a wide margin, and it
// still holds for any proxy setting down to 30 s. It is not raised with the
// proxy's default on purpose: a batch that is slow because the database is
// slow should return what it has within a bound an agent waits out, not
// spend the larger window retrying into the same congestion. It is
// deliberately not a per-row timeout.
// It is a var, not a const, for one reason: the tests shorten it to prove
// the stop path without sleeping for twenty seconds. Production never
// assigns it.
var batchLoopDeadline = 20 * time.Second

// batchLoopContext derives the context the per-row work runs under. Callers
// keep using the parent context for logging, so a log line still gets
// written when the loop's own deadline has fired.
//
// A var for the same reason batchLoopDeadline is one: tests replace it
// (sk-647.2) with a context they end themselves, at the moment a row is
// observably parked inside its write — a deadline picked by the clock
// cannot say where in the row it lands. Production never assigns it.
var batchLoopContext = func(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, batchLoopDeadline)
}

// batchStopReason says whether the loop must stop before running another
// row, and with which code the rows that did not run are marked.
//
// The two cases are reported apart on purpose. Until now both arrived as
// internal_error with "request cancelled", which is also what a genuine
// store failure returns — so a caller could not tell "the server gave up
// on schedule and is telling you where it stopped" from "something broke
// while writing this row". They call for different responses: the first is
// retried for the remaining rows, the second is not retried blindly.
func batchStopReason(parent, loop context.Context) (code, message string, stop bool) {
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
