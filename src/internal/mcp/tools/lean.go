package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/issues"
	skmcp "skein-api/internal/mcp"
)

// leanComment projects a CommentDTO onto the wire shape the comment
// write-tools return: the full DTO minus `body`, plus a
// `bodyOmitted:true` marker so the caller can tell "elided" from
// "empty" (sk-248).
//
// Same rationale as the issue acknowledgement (ack.go), one step
// further: on a comment write the caller has just supplied the body
// verbatim, so echoing it puts the identical text in the agent's context
// twice — once as the tool_use argument, once as the tool_result — and
// both copies are re-sent on every later turn of the session.
//
// Measured on a real multi-agent run: 172
// skein_comment calls across two sessions echoed ~499k characters
// (~166k tokens), roughly 14% of the coordinating agent's context. Comments
// are a coordinating agent's most frequent write by far — they are its only
// channel to the other agents — so this is the single largest echo in the
// tool surface.
//
// Unlike the issue write-tools there is no verbose escape hatch. A flag
// defaults to off (saving nothing) or gets turned on (saving nothing for
// whoever turned it on); and unlike issue prose, a comment body has no
// post-write use — the caller wrote it. Everything else is KEPT: id
// (needed for a follow-up skein_comment_update / skein_comment_delete),
// version (the expectedVersion cycle), issueId, author*, timestamps,
// mentions. The body remains readable via skein_comment_get and
// skein_show.
func leanComment(dto *issues.CommentDTO) (map[string]any, error) {
	if dto == nil {
		return nil, nil
	}
	b, err := json.Marshal(dto)
	if err != nil {
		return nil, fmt.Errorf("marshal comment dto: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("unmarshal comment dto: %w", err)
	}
	delete(m, "body")
	m["bodyOmitted"] = true
	return m, nil
}

// writeCommentResult builds the complete tool result for a comment
// write-tool. Mirrors writeIssueAck: centralises the projection and
// the defensive "projection failed" branch, which can only trigger if
// CommentDTO stops being JSON-marshalable. It is surfaced as
// internal_error rather than silently falling back to the full DTO,
// because that fallback would quietly undo the change.
func writeCommentResult(
	ctx context.Context,
	toolName string,
	userID, commentID string,
	dto *issues.CommentDTO,
) (*mcpgo.CallToolResult, error) {
	payload, err := leanComment(dto)
	if err != nil {
		slog.ErrorContext(ctx, "mcp tool error",
			"tool", toolName,
			"reason", "comment projection failed",
			"userId", userID, "commentId", commentID, "error", err.Error())
		return skmcp.ToolError(ErrCodeInternalError, "failed to encode result", nil), nil
	}
	return skmcp.ToolJSON(commentResponse{Comment: payload})
}

// issueWithoutDescription projects an IssueDetail onto the wire shape
// skein_show returns when the caller passed includeDescription=false:
// every field exactly as-is, except `description`, which is set to an
// explicit JSON null (sk-244.1).
//
// The projection goes through JSON rather than through a parallel struct
// for the reason issuerow.go states at length: a parallel struct would
// silently drop every field later added to IssueDetail, and that absence
// would be indistinguishable from a deliberate omission. Marshalling the real type and rewriting one
// known key means new fields flow through automatically and the diff
// between full and elided is always exactly that one key.
//
// The remaining reason is a hard constraint rather than a preference:
// IssueDetail.Description cannot become a *string. The struct is shared
// with the REST handlers, so a pointer would turn every REST response's
// `"description": ""` into `"description": null` and break the frontend.
// Elision therefore has to happen at the MCP wire edge, not in the type.
//
// The key is REWRITTEN to nil, not deleted, so "not requested" (null)
// stays distinguishable from "empty" (""). That matches the convention
// skein_show already uses for includeComments / includeEvents, where the
// off state renders as null rather than an absent key. `bodyOmitted` is
// deliberately NOT set: that marker now belongs to the COMMENT write-tools
// alone (leanComment), where it means "the body was elided, not empty".
// The issue write-tools stopped emitting it when sk-282 replaced their
// card with an acknowledgement, which carries no prose to mark. Here
// `design`, `acceptanceCriteria` and `notes` are returned untouched, so
// there is nothing to mark either.
func issueWithoutDescription(d *issues.IssueDetail) (map[string]any, error) {
	if d == nil {
		return nil, nil
	}
	b, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("marshal issue detail: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("unmarshal issue detail: %w", err)
	}
	m["description"] = nil
	return m, nil
}

// ackAnswerNote is the one sentence every write-tool of the US-5 class
// puts in its OPENING paragraph (sk-282 T026, US-10).
//
// US-10's finding is that a model optimises for completeness unless the
// cheap path is the first thing it reads — so a tool whose answer changed
// shape has to say so before the reader stops, not in a parameter blurb
// halfway down the schema. These eleven descriptions all opened by
// describing the ACTION and never said what comes back; an agent reading
// one of them still expected an issue card.
//
// Deliberately short: it is inlined into eleven schemas, so every
// character is paid eleven times in tools/list. The parameter blurb
// (ackVerboseDescription) carries the field-level detail.
const ackAnswerNote = " ANSWERS WITH AN ACKNOWLEDGEMENT by default — the id, " +
	"the applied state and the new version, no issue card and no prose — so " +
	"the expectedVersion cycle continues without a re-read; pass `verbose` " +
	"for the full IssueDetail."

// withAckAnswerNote appends ackAnswerNote to the END of a description's
// first paragraph, leaving the rest of the text untouched.
//
// Placement is a rule rather than a per-tool decision because the tools
// disagree about what their first paragraph is for: skein_create opens
// with a required warning about prose dependency refs, skein_claim with
// the id/ids split, skein_accept with a single sentence and no paragraph
// break at all. Appending to the first paragraph puts the answer shape in
// the reader's first screen in every one of those layouts without
// reordering anyone's text.
func withAckAnswerNote(description string) string {
	head, tail, found := strings.Cut(description, "\n\n")
	if !found {
		return description + ackAnswerNote
	}
	return head + ackAnswerNote + "\n\n" + tail
}

// ackVerboseDescription is the shared parameter blurb for the `verbose`
// flag on the write-tools that answer with an ACKNOWLEDGEMENT (sk-282,
// US-5) rather than with an issue card.
//
// It replaced a sibling constant that described the sk-233 lean card. The
// two coexisted for the length of wave G-A4 — the class converts one tool
// per task, and a tool still answering with the lean card had to keep
// describing it, because a schema advertising a contract its tool does
// not implement is exactly what INV-5 forbids, temporarily or not. With
// the last tool converted the old wording had no user left and went.
//
// Economy rule: this string is inlined into every converted tool's
// schema, so every character is paid eleven times in the tools/list
// payload. It says what comes back and how to reach the rest, and leaves
// the reasoning to the agent guide.
const ackVerboseDescription = "Return the full IssueDetail, prose " +
	"included. Default false = acknowledgement: `id`, `ok`, `version` " +
	"plus the post-state stamps that apply (`status`, `assignee`, lock / " +
	"claim / close stamps) — no issue card and no free text. The version " +
	"is always there, so the expectedVersion cycle needs no extra read. " +
	"Details: skein_get_instructions."

// writeIssueAck builds the complete tool result for a write-tool whose
// success payload is an ACKNOWLEDGEMENT rather than an issue card
// (sk-282 T007 / US-5).
//
// It began as the sibling of writeIssueResult — the sk-233 helper that
// returned the issue card minus its prose — with the same signature so
// that G-A4 could convert the class one call site at a time instead of
// rewriting each. That conversion is finished: every write-tool of the
// US-5 class now answers here, including the four that used to build
// their own response (create, the close warning branch, the claim batch,
// epic_create), and writeIssueResult was removed with its last caller.
//
// It is deliberately the ONLY way an issue post-state reaches the wire
// from a write-tool (D-6). Leaving the old helper in place would have
// left a second, card-shaped serialisation point sitting ready for the
// next tool to pick up — which is the partial application FP-2 and FP-9
// describe, arriving a release later.
func writeIssueAck(
	ctx context.Context,
	toolName string,
	userID, issueID string,
	d *issues.IssueDetail,
	verbose bool,
) (*mcpgo.CallToolResult, error) {
	return writeIssueAckWith(ctx, toolName, userID, issueID, d, verbose, nil)
}

// writeIssueAckWith is writeIssueAck plus a slot for keys that belong at
// the TOP LEVEL of the response, beside the acknowledgement's own.
//
// The slot exists because three of D-6's four named bypasses need it. The
// close warning branch (close.go) is the concrete one: it does not call
// the common helper at all, it builds the response map itself and injects
// `warning` and `openChildList` flat — an agent parses them at the top
// level in both lean and verbose mode, and wrapping them would be a wire
// change. Without a slot here, G-A4 could bring every OTHER write-tool to
// the acknowledgement and would have to leave that branch on its own
// serialisation path — "the class applied only in part", which is exactly the
// outcome FP-2 and FP-9 name as a failure.
//
// The default form does not move by a byte: with no extra keys the
// payload is serialised exactly as writeIssueAck serialises it, struct and
// all, not routed through a map. The map path exists only for a caller
// that asked for it.
//
// A key that collides with one the acknowledgement already emits is an
// ERROR, not an overwrite: silently replacing `version` or `status` would
// hand the caller a post-state the server never wrote, and that is the one
// thing an acknowledgement exists to be trusted about.
func writeIssueAckWith(
	ctx context.Context,
	toolName string,
	userID, issueID string,
	d *issues.IssueDetail,
	verbose bool,
	extra map[string]any,
) (*mcpgo.CallToolResult, error) {
	return writeAck(ctx, toolName, userID, issueID, d, verbose, extra, ackFromIssue)
}

// writeIssueCloseAck is writeIssueAckWith for the three surfaces whose
// acknowledgement carries the close outcome and the close-target link
// state — skein_close, skein_close_many and skein_update (sk-489, US-9).
//
// Same shape of exception as writeIssueAcceptanceAck, and here for the
// same reason the delegation pair needed one: `link` is not a field of
// the issue but the writer's answer about THIS call, so it cannot be read
// off the post-state and has to be threaded from the call site. The
// `extra` slot is kept because skein_close's warning branch needs it —
// this entry point replaces writeIssueAckWith there, and dropping the
// slot would push that branch back off the common serialisation point
// (D-6).
func writeIssueCloseAck(
	ctx context.Context,
	toolName string,
	userID, issueID string,
	d *issues.IssueDetail,
	verbose bool,
	extra map[string]any,
	link issues.CloseLinkState,
) (*mcpgo.CallToolResult, error) {
	return writeAck(ctx, toolName, userID, issueID, d, verbose, extra,
		ackFromClosedIssue(link))
}

// writeIssueAcceptanceAck is writeIssueAck for the one surface whose
// acknowledgement carries the acceptance stamp — skein_accept.
//
// It exists as its own entry point rather than as a flag on the shared
// one so that the exception is visible where it applies (accept.go) and
// nowhere else: a boolean threaded through eleven call sites would be
// `false` at ten of them and would invite the twelfth to guess. See
// ackFromAcceptedIssue for why the stamp is not on the common row.
func writeIssueAcceptanceAck(
	ctx context.Context,
	toolName string,
	userID, issueID string,
	d *issues.IssueDetail,
	verbose bool,
) (*mcpgo.CallToolResult, error) {
	return writeAck(ctx, toolName, userID, issueID, d, verbose, nil, ackFromAcceptedIssue)
}

// writeIssueDelegationAck is writeIssueAck for skein_drop_delegated and
// skein_resume_delegated — the acknowledgement that carries the caller's
// own post-state in dismissed_by.
//
// Same shape of exception as writeIssueAcceptanceAck, and here for the
// same reason: the caller's id is needed to build the row, and threading
// it through the shared entry point would put a parameter on eleven call
// sites that only two of them can answer. See ackFromDelegatedIssue for
// why these two tools cannot acknowledge with the common row.
func writeIssueDelegationAck(
	ctx context.Context,
	toolName string,
	userID uuid.UUID,
	issueID string,
	d *issues.IssueDetail,
	verbose bool,
) (*mcpgo.CallToolResult, error) {
	return writeAck(ctx, toolName, userID.String(), issueID, d, verbose, nil,
		ackFromDelegatedIssue(userID))
}

// writeAck is the single body behind the acknowledgement writers.
func writeAck(
	ctx context.Context,
	toolName string,
	userID, issueID string,
	d *issues.IssueDetail,
	verbose bool,
	extra map[string]any,
	build func(*issues.IssueDetail) (ackRow, error),
) (*mcpgo.CallToolResult, error) {
	payload, err := ackPayloadWith(d, verbose, build)
	if err == nil && len(extra) > 0 {
		payload, err = withTopLevelKeys(payload, extra)
	}
	if err != nil {
		slog.ErrorContext(ctx, "mcp tool error",
			"tool", toolName,
			"reason", "issue acknowledgement projection failed",
			"userId", userID, "issueId", issueID, "error", err.Error())
		return skmcp.ToolError(ErrCodeInternalError, "failed to encode result", nil), nil
	}
	return skmcp.ToolJSON(payload)
}

// writeIssueBatchAck builds the complete tool result for a GROUP
// write-tool whose success payload is a set of acknowledgements
// (sk-282 G-A4).
//
// Same role as writeIssueAck one cardinality up, including the defensive
// error branch: a group tool that cannot project its own post-state must
// surface that as a tool error, not as a confirmation. It is a real
// branch here rather than a formality — ackBatchPayload refuses a nil
// slot, and a missing post-write load is exactly how a group site can go
// wrong.
func writeIssueBatchAck(
	ctx context.Context,
	toolName string,
	userID string,
	details []*issues.IssueDetail,
	verbose bool,
) (*mcpgo.CallToolResult, error) {
	payload, err := ackBatchPayload(details, verbose)
	if err != nil {
		slog.ErrorContext(ctx, "mcp tool error",
			"tool", toolName,
			"reason", "group acknowledgement projection failed",
			"userId", userID, "issueCount", len(details), "error", err.Error())
		return skmcp.ToolError(ErrCodeInternalError, "failed to encode result", nil), nil
	}
	return skmcp.ToolJSON(payload)
}

// withTopLevelKeys re-projects a payload as a JSON object and adds keys to
// it, refusing to overwrite one that is already there.
//
// It goes through the marshalled form rather than a wrapper struct: the
// payload is one of two different types (the acknowledgement or the full
// IssueDetail, depending on `verbose`), and a wrapper would have to be
// written twice and would nest the extras differently in each mode.
func withTopLevelKeys(payload any, extra map[string]any) (map[string]any, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal acknowledgement: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("unmarshal acknowledgement: %w", err)
	}
	if m == nil {
		m = make(map[string]any, len(extra))
	}
	for k, v := range extra {
		if _, taken := m[k]; taken {
			return nil, fmt.Errorf("extra response key %q collides with a key the "+
				"acknowledgement already carries", k)
		}
		m[k] = v
	}
	return m, nil
}
