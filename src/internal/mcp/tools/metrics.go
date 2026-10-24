package tools

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"skein-api/internal/auth"
)

// Prometheus metrics for the MCP tool surface. Names follow the
// `skein_<area>_<measure>` convention used elsewhere in skein-api, and
// both counters are package-level singletons registered with promauto in
// the default registerer — the /metrics endpoint mounted in cmd/api/main.go
// picks them up automatically.
//
// Why they exist (sk-256): the surface — 67 tools when these counters
// were added, 41 today — is being trimmed, consolidated and selectively
// promoted to _meta["anthropic/alwaysLoad"].
// Until now those calls were made on impression. These two counters are
// the evidence:
//
//   - never called over a window → removal / consolidation candidate
//     (`rate(skein_mcp_tool_calls_total[7d]) == 0`, or simply an absent
//     series — the Vec creates a child only on first call, so a tool no
//     one ever invoked has no series at all);
//   - called in nearly every session → alwaysLoad candidate. Prometheus
//     cannot count distinct sessions, but it does not need to: the server
//     `instructions` make skein_whoami a mandatory once-per-session call,
//     so the skein_whoami series is a session counter and
//     `calls{tool="X"} / calls{tool="skein_whoami"}` is calls-per-session
//     for X;
//   - failing more than its neighbours → bad schema or bad description,
//     read off skein_mcp_tool_errors_total split by code (below).
//
// Deliberately NOT labelled by caller (user id, api-key id, Mcp-Session-Id
// or username). Those are the textbook unbounded-cardinality sources —
// session ids in particular rotate per MCP connection, so the label would
// grow without limit — and the distinction actually wanted (the agent's
// role in a multi-agent setup) is not represented in the auth context at all,
// so no bounded label could express it. Per-principal attribution already
// exists in Postgres: every mutation writes events.actor_session_id, which
// is joinable and queryable without paying for it in every scrape.
var (
	// toolCallsTotal counts one increment per dispatched MCP tool call.
	//
	// @metric description Total MCP tool invocations, one increment per tools/call dispatch, labelled by tool name, the action within a consolidated tool, and coarse outcome. Answers which registered tools are never called over a window (missing series or zero rate — consolidation and removal candidates) and which are called in nearly every session (alwaysLoad candidates: divide by the skein_whoami series, which the server instructions make a mandatory once-per-session call). Counts calls, not work items — a batch tool that updated twenty issues increments by one.
	// @metric calculation Incremented in the deferred observation inside instrumentTool (and instrumentActionTool for consolidated tools), the wrapper internal/mcp/tools.addTool / addActionTool puts around every registered handler, so it fires exactly once per call including the panic path. outcome is derived from the handler return in classifyToolCall, which requires CallToolResult.IsError and the JSON error envelope to agree: nil error, IsError false and no envelope is "ok"; IsError true plus an envelope carrying any business code is "business_error"; a non-nil error return, a nil result, or IsError true plus the internal_error envelope code is "internal_error"; a result where only one of the two error signals is present is "contract_violation"; a handler that panicked is "panic" (the panic is re-raised untouched, never recovered).
	// @label tool MCP tool name exactly as registered, for example "skein_create". Bounded by the registered surface (41 tools today).
	// @label action Sub-action of a consolidated tool, taken from that tool's action enum, for example "upload_url" on skein_attachments. Empty string for the tools that are not consolidated, so sum by (tool) reads exactly as it did before the label existed. Bounded: the value is matched against the tool's action allowlist and anything outside it — a typo, a stale name, a missing parameter — collapses to "__invalid__" rather than minting a series from caller-controlled text.
	// @label outcome Coarse call outcome. Values: "ok", "business_error", "internal_error", "contract_violation", "panic"
	toolCallsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "skein_mcp_tool_calls_total",
			Help: "Total MCP tool invocations by tool name, action and outcome",
		},
		[]string{"tool", "action", "outcome"},
	)

	// toolErrorsTotal breaks the non-ok calls down by error code.
	//
	// @metric description MCP tool calls that were not a clean success, broken down by the stable error code the tool returned. The code is what makes an error rate actionable: a tool with a high validation_error share has a schema or a description agents mis-read and is a rewrite candidate, issue_not_found concentrates where agents guess identifiers, while version_conflict is write contention and says nothing about the tool description. Divide by skein_mcp_tool_calls_total for the same tool to get its error ratio.
	// @metric calculation Incremented in the same deferred observation as skein_mcp_tool_calls_total, once per call, only when the outcome is not "ok". code is the top-level error field of the JSON envelope produced by mcp.ToolError, passed through the closed knownErrCodes allowlist mirroring the ErrCode constants in internal/mcp/tools/errors.go; a code outside the allowlist collapses to "other", a handler that returned a transport-level error becomes "transport_error", a nil result becomes "nil_result", and a panicking handler becomes "panic". A result carrying only one of the two error signals is recorded as "envelope_without_flag" (JSON envelope present, CallToolResult.IsError false) or "flag_without_envelope" (IsError true, no parsable envelope); both are defects in this package rather than caller errors. That allowlist is what keeps the label bounded.
	// @label tool MCP tool name exactly as registered, for example "skein_create". Bounded by the registered surface (41 tools today).
	// @label action Sub-action of a consolidated tool, taken from that tool's action enum, for example "delete" on skein_attachments. Empty string for tools that are not consolidated; "__invalid__" for anything outside the tool's action allowlist, which is what keeps the label bounded — the raw string an agent sent is in the WARN line the dispatcher writes, not in the label.
	// @label code Stable business error code from the closed ErrCode set in internal/mcp/tools/errors.go, for example "validation_error", "version_conflict", "forbidden", "issue_not_found". Plus the synthetic values "transport_error", "nil_result", "panic", "envelope_without_flag", "flag_without_envelope" and "other" for anything off the allowlist.
	toolErrorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "skein_mcp_tool_errors_total",
			Help: "Failed MCP tool invocations by tool name, action and error code",
		},
		[]string{"tool", "action", "code"},
	)
)

// Outcome values for the `outcome` label of skein_mcp_tool_calls_total.
//
// The split between business_error and internal_error is the point of the
// metric. Skein returns business failures as a JSON envelope in the tool
// result (mcp.ToolError), NOT as a transport error, so a naive counter
// records them as successes and the "which tool has a bad schema?"
// question becomes unanswerable.
const (
	outcomeOK            = "ok"
	outcomeBusinessError = "business_error"
	outcomeInternalError = "internal_error"
	outcomePanic         = "panic"
	// outcomeContractViolation — the two halves of a tool result disagree:
	// CallToolResult.IsError and the JSON error envelope. Deliberately its
	// own bucket rather than being folded into internal_error or ok:
	//
	//   - it is not "ok": we cannot tell whether the call actually failed,
	//     and silently scoring it a success is how the classifier would rot
	//     unnoticed (exactly the failure mode this bucket exists to expose);
	//   - it is not "internal_error" either: internal_error is what an alert
	//     on "skein is broken in production" fires on, and a payload-shape
	//     defect is not that. Folding it in would poison that signal;
	//   - it is not "business_error": counting an envelope the tool never
	//     flagged as an error as a business failure is the false positive
	//     this whole classification change is about.
	//
	// Which half disagreed is in the `code` label of the errors counter —
	// see codeEnvelopeWithoutFlag / codeFlagWithoutEnvelope. Any series here
	// is a bug in this package, not in the caller's usage.
	outcomeContractViolation = "contract_violation"
)

// Synthetic values for the `code` label of skein_mcp_tool_errors_total.
// They cover the failures that have no ErrCode because they never
// produced an envelope.
const (
	// codeTransportError — the handler returned a non-nil error instead of
	// a result. In this package that means the result could not even be
	// marshalled (mcp.ToolJSON); it is never a business outcome.
	codeTransportError = "transport_error"
	// codeNilResult — handler returned (nil, nil). Contract violation.
	codeNilResult = "nil_result"
	// codePanic — the handler panicked. Recorded, then the panic continues
	// to propagate; instrumentTool never calls recover().
	codePanic = "panic"
	// codeOther — an error result whose envelope code is outside the
	// closed ErrCode set. The bucket exists so an unexpected string can
	// never mint a new label value and inflate cardinality.
	codeOther = "other"
	// codeEnvelopeWithoutFlag — the result carries a top-level string
	// `error` but IsError is false. Either a success payload happens to
	// have a top-level `error` string field (then the envelope probe is
	// lying and the classifier needs narrowing), or a tool built an error
	// envelope without mcp.ToolError and lost the flag. Both are ours to
	// fix; neither may be recorded as a business_error.
	codeEnvelopeWithoutFlag = "envelope_without_flag"
	// codeFlagWithoutEnvelope — IsError is true but no parsable envelope.
	// Every failure in this package goes through mcp.ToolError, which
	// always writes both, so this means a raw NewToolResultError slipped
	// in and agents are getting an error they cannot branch on.
	codeFlagWithoutEnvelope = "flag_without_envelope"
)

// knownErrCodes is the closed allowlist the `code` label is filtered
// through. It mirrors the ErrCode* constants of this package 1:1 — the
// constants are referenced, not their string values, so a renamed code
// cannot drift here silently, and TestKnownErrCodes_CoversPackage fails
// the build if a new ErrCode constant is added without an entry.
//
// That guard scans every non-test file in the package, not just errors.go:
// declaring an ErrCode constant next to the tool that emits it is what let
// preview_unavailable reach review as code="other" (sk-256 review), and a
// guard that trusts the convention it is supposed to enforce is not a guard.
// errors.go remains the one place codes are declared — the scan is the
// backstop for when someone forgets that.
var knownErrCodes = map[string]struct{}{
	ErrCodeProjectNotFound:         {},
	ErrCodeIssueNotFound:           {},
	ErrCodeVersionConflict:         {},
	ErrCodeValidationError:         {},
	ErrCodeForbidden:               {},
	ErrCodeUnauthenticated:         {},
	ErrCodeInternalError:           {},
	ErrCodeDependencyCycle:         {},
	ErrCodeDependencyAlreadyExists: {},
	ErrCodeParentClosed:            {},
	ErrCodeParentNotFound:          {},
	ErrCodeParentWrongProject:      {},
	ErrCodeIDTaken:                 {},
	ErrCodeComponentsWrongProject:  {},
	ErrCodePinnedNoForce:           {},
	ErrCodeAmbiguousProject:        {},

	ErrCodeAlreadyClaimed: {},
	ErrCodeLockedByOther:  {},
	ErrCodeNotLockHolder:  {},

	ErrCodeCannotClaimClosed:  {},
	ErrCodeCannotAcceptClosed: {},

	ErrCodeNotAdmin:         {},
	ErrCodeUserNotFound:     {},
	ErrCodeEmailTaken:       {},
	ErrCodeUsernameTaken:    {},
	ErrCodeMemberNotFound:   {},
	ErrCodeMemberViaSpace:   {},
	ErrCodeConcurrentChange: {},

	ErrCodeSpaceNotFound:               {},
	ErrCodePersonalSpace:               {},
	ErrCodeExternalUser:                {},
	ErrCodeAlreadyMember:               {},
	ErrCodeNoSeats:                     {},
	ErrCodeNotSpaceMember:              {},
	ErrCodeAssigneeNotAllowed:          {},
	ErrCodeForceUnlockForbidden:        {},
	ErrCodeInvitationAgentForbidden:    {},
	ErrCodeSpaceRequired:               {},
	ErrCodeSpaceChangeForbidden:        {},
	ErrCodeExternalCannotCreateProject: {},
	ErrCodePrefixTaken:                 {},
	ErrCodePrefixInvalid:               {},
	ErrCodeProjectNameTaken:            {},
	ErrCodeProjectQuota:                {},
	ErrCodeStorageQuota:                {},
	ErrCodeAdminRoleDisabled:           {},
	ErrCodeLastAdminCannotDemote:       {},

	ErrCodeAttachmentNotFound:      {},
	ErrCodeAttachmentsUnavailable:  {},
	ErrCodeAttachmentNotReady:      {},
	ErrCodeAttachmentObjectMissing: {},
	ErrCodeAttachmentLimit:         {},
	ErrCodeAttachmentMIMEBlocked:   {},
	ErrCodeAttachmentTooLarge:      {},

	ErrCodeRepoNotFound:  {},
	ErrCodeRepoAmbiguous: {},

	ErrCodeParentAlreadySet:              {},
	ErrCodeSerializationRetry:            {},
	ErrCodeRequestCancelled:              {},
	ErrCodeBatchDeadlineExceeded:         {},
	ErrCodeDependencyTargetNotVisibleYet: {},
	ErrCodeReparentSelf:                  {},
	ErrCodeReparentCrossProject:          {},
	ErrCodeDependencyNotFound:            {},
	ErrCodeCloseOutcomeLinkRequired:      {},

	ErrCodeReminderNotFound:        {},
	ErrCodeReminderTargetAmbiguous: {},

	ErrCodeUnknownThemeRole: {},

	ErrCodeCannotPromoteLockedToStanding: {},
	ErrCodeCannotWorkStandingDirectly:    {},
	ErrCodeCannotDeferStanding:           {},
	ErrCodeNestedStandingForbidden:       {},
	ErrCodeBlockerTargetStanding:         {},
	ErrCodeCannotClaimStanding:           {},
	ErrCodeStandingHasOpenChildren:       {},

	ErrCodeAPIKeyRequired:  {},
	ErrCodeCommentNotFound: {},

	ErrCodeLockRequiredSessionID: {},
	ErrCodePreviewUnavailable:    {},

	ErrCodeReviewForbidden: {},

	ErrCodeMonitorNotFound: {},
}

// instrumentTool wraps a tool handler so every dispatch lands in
// skein_mcp_tool_calls_total (and, when it failed, in
// skein_mcp_tool_errors_total). It is applied once, in addTool, which is
// the single registration funnel for the non-consolidated tools in this
// package — there is no per-tool instrumentation to forget.
//
// The `action` label is the empty string here, and that is a statement
// rather than a placeholder: it marks a tool that is not folded, so
// sum by (tool) and "which tool was never called" keep reading exactly
// as they did before consolidation added the label.
//
// Two invariants the wrapper must not break:
//
//   - it returns the handler's result and error untouched;
//   - it never swallows a panic. The observation runs in a defer without
//     recover(), so a panicking handler is recorded as outcome="panic"
//     and the panic then keeps propagating exactly as before. The
//     pessimistic default (panic) is overwritten only by a normal
//     return, which is what makes the panic path observable at all.
func instrumentTool(name string, h server.ToolHandlerFunc) server.ToolHandlerFunc {
	return observeToolCall(name, func(mcpgo.CallToolRequest) string { return "" }, h)
}

// instrumentActionTool is instrumentTool for a consolidated tool: same
// counters, but each call is attributed to the action it selected.
//
// The label comes from actionSet.label, never from the raw argument —
// see actionInvalid. Cardinality stays bounded by the registered surface,
// but it is not unchanged by the fold and this comment used to claim it
// was: the 41 tools produce 33 (tool, "") pairs for the unfolded ones, 39
// (tool, action) pairs across the eight action tables, and 8 more
// (tool, "__invalid__") buckets — 80 pairs, against the 67 the pre-fold
// surface had. The 39 exceeds the 34 tools that were folded because three
// discriminators that used to live INSIDE a tool became actions of their
// own (skein_filter_watch's `op`, skein_project_watch's `unwatch`,
// skein_mark_read's presence-based one-of). Multiplied by the five
// `outcome` values that is 400 potential series where the old surface had
// 335 — bounded and small either way, which is the property that matters;
// exact parity was never true.
func instrumentActionTool(name string, actions actionSet, h server.ToolHandlerFunc) server.ToolHandlerFunc {
	return observeToolCall(name, actions.label, h)
}

// observeToolCall is the shared body of both wrappers. The action label
// is resolved BEFORE the handler runs so a panicking handler is still
// attributed to its action rather than to a blank one.
func observeToolCall(name string, labelFor func(mcpgo.CallToolRequest) string, h server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		action := labelFor(req)
		outcome, code := outcomePanic, codePanic
		defer func() {
			toolCallsTotal.WithLabelValues(name, action, outcome).Inc()
			if outcome != outcomeOK {
				toolErrorsTotal.WithLabelValues(name, action, code).Inc()
			}
		}()

		start := time.Now()
		res, err := h(ctx, req)
		elapsed := time.Since(start)
		outcome, code = classifyToolCall(res, err)
		// sk-303.38: skein_mcp_tool_errors_total counts a failure but
		// never says why — a week of 533 failures left no trail an
		// operator could grep. This is the single funnel every tool
		// dispatch passes through (addTool / addActionTool both route
		// here), so one WARN line here covers the whole surface instead
		// of one copy-pasted into every handler.
		if outcome != outcomeOK {
			logToolFailure(ctx, name, action, outcome, code, res, elapsed)
		}
		return res, err
	}
}

// maxLoggedToolFailureReason bounds the `reason` attribute of the
// "mcp tool failed" line. Business-error messages in this package are
// built from static templates plus closed-set values (error codes, enum
// names, field names) — see the sk-303.38 review — but 700+ call sites
// is too many to prove that invariant forever, and a message CAN still
// carry a caller-controlled fragment (e.g. an echoed commit hash). This
// is the same defence-in-depth sk-275's `snippet` applies to prose
// fields: cap the length rather than trust every call site to stay
// short. It is not a redaction — the message already reached the
// calling agent in the tool response — only a bound on how much of it
// this package repeats into Loki.
const maxLoggedToolFailureReason = 200

// logToolFailure emits one WARN line per non-ok tool dispatch: the tool,
// the code the counter already has, and — the thing the counter cannot
// carry — the actual message that went back to the caller, plus who
// called and how long it took. sk-303.38: without this, a `validation_error`
// bucket of several hundred failures a week is one number with six or
// more distinct causes hiding inside it and no way to tell them apart.
//
// WARN, not ERROR: a business_error or contract_violation is not "skein
// is broken" — see outcomeContractViolation's own reasoning — it is the
// class of thing an operator needs to grep for without a log-level flip
// in production, same rationale as logMCPRequest's failure line (sk-303.10).
//
// None of the attribute keys below may contain
// password/token/secret/key/credential/authorization/apikey/api_key:
// logging.DualHandler blanks any attribute whose KEY contains one of
// those, so a key named too close to the list would reach Loki as `***`
// and the line would be silently worthless.
// TestLogToolFailure_KeysSurviveTheSanitizer pins that against the real
// handler, mirroring TestToolCallSyntaxDiagnostics_KeysSurviveTheSanitizer
// (sk-275) — the exact mistake this comment warns about has already
// shipped once, in sk-275, and once in skein-web.
func logToolFailure(ctx context.Context, tool, action, outcome, code string, res *mcpgo.CallToolResult, elapsed time.Duration) {
	var reason string
	if res != nil {
		reason = toolErrorMessage(res)
	}
	if n := utf8.RuneCountInString(reason); n > maxLoggedToolFailureReason {
		reason = string([]rune(reason)[:maxLoggedToolFailureReason]) + "…"
	}

	attrs := []any{
		"tool", tool,
		"outcome", outcome,
		"code", code,
		"reason", reason,
		"latencyMs", elapsed.Milliseconds(),
	}
	if action != "" {
		attrs = append(attrs, "action", action)
	}
	if userID, ok := auth.UserIDFrom(ctx); ok {
		attrs = append(attrs, "userId", userID.String())
	}
	if sessionID := auth.SessionIDFrom(ctx); sessionID != "" {
		attrs = append(attrs, "sessionId", sessionID)
	}
	slog.WarnContext(ctx, "mcp tool failed", attrs...)
}

// classifyToolCall maps a handler return onto (outcome, code).
//
// A failed tool call in this package has two halves, and mcp.ToolError
// always writes both: CallToolResult.IsError (the MCP-level flag) and the
// JSON envelope `{"error":"<code>", ...}` (the contract agents parse, and
// the only place the code lives). The classifier requires both to agree
// rather than trusting either alone, because each alone has a failure mode:
//
//   - envelope alone (what this function did before sk-256's review): any
//     future tool whose *successful* payload has a top-level string `error`
//     field is silently scored business_error. Nothing in the codebase
//     enforces "success never carries top-level `error`" — the batch tools
//     already put an `error` there, saved only by it being an object rather
//     than a string. That is a shape coincidence, not a contract;
//   - IsError alone: it carries no code, so the errors counter — whose
//     entire purpose is the per-code split — would have nothing to record.
//
// So: agreement classifies, disagreement is its own outcome
// (outcomeContractViolation) with the direction of the disagreement in the
// code label. Disagreement is never business_error and never ok — see the
// reasoning on outcomeContractViolation.
//
// code is "" exactly when outcome is "ok" — callers must not record it.
func classifyToolCall(res *mcpgo.CallToolResult, err error) (outcome, code string) {
	switch {
	case err != nil:
		// A handler error is a transport-level failure: mcp-go turns it
		// into a JSON-RPC error, not into a tool result. Business
		// failures never travel this way.
		return outcomeInternalError, codeTransportError
	case res == nil:
		return outcomeInternalError, codeNilResult
	}

	envelope := toolErrorCode(res)
	switch {
	case !res.IsError && envelope == "":
		return outcomeOK, ""
	case !res.IsError:
		return outcomeContractViolation, codeEnvelopeWithoutFlag
	case envelope == "":
		return outcomeContractViolation, codeFlagWithoutEnvelope
	}

	code = codeOther
	if _, known := knownErrCodes[envelope]; known {
		code = envelope
	}
	if envelope == ErrCodeInternalError {
		return outcomeInternalError, code
	}
	return outcomeBusinessError, code
}

// toolErrorCode returns the top-level `error` code of the JSON envelope
// in a tool result, or "" when the result is not an error envelope.
//
// The probe is intentionally strict — object at the top level, `error`
// present and a JSON string — so a successful payload that merely
// contains a nested `error` object (skein_dep_add_many and
// skein_close_many both have per-row `error` fields) is not mistaken for
// a failed call.
func toolErrorCode(res *mcpgo.CallToolResult) string {
	for _, content := range res.Content {
		text, ok := content.(mcpgo.TextContent)
		if !ok {
			continue
		}
		body := strings.TrimSpace(text.Text)
		if !strings.HasPrefix(body, "{") {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &fields); err != nil {
			continue
		}
		raw, present := fields["error"]
		if !present {
			continue
		}
		var code string
		if err := json.Unmarshal(raw, &code); err != nil {
			continue
		}
		if code != "" {
			return code
		}
	}
	return ""
}

// toolErrorMessage returns the top-level `message` string of the JSON
// envelope in a tool result, or "" when absent or unparsable. Mirrors
// toolErrorCode's extraction exactly, one field over — see that comment
// for why the probe stays this strict.
//
// The other structured fields of the envelope (currentVersion, childIds,
// the repo list added by sk-303.36, …) are deliberately NOT extracted
// here: `message` is the one field this package promises to log in full
// (bounded — see maxLoggedToolFailureReason), everything else stays
// inside the response the caller already has.
func toolErrorMessage(res *mcpgo.CallToolResult) string {
	for _, content := range res.Content {
		text, ok := content.(mcpgo.TextContent)
		if !ok {
			continue
		}
		body := strings.TrimSpace(text.Text)
		if !strings.HasPrefix(body, "{") {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &fields); err != nil {
			continue
		}
		raw, present := fields["message"]
		if !present {
			continue
		}
		var message string
		if err := json.Unmarshal(raw, &message); err != nil {
			continue
		}
		if message != "" {
			return message
		}
	}
	return ""
}
