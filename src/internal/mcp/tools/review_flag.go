package tools

// review_flag.go — sk-303.14, task T007, contract C1.
//
// The "review required" flag on the agent mutation path: parsing the
// skein_update argument and mapping store refusals to tool codes.
//
// There is no viewed mark here and there will not be: no agent tool for
// the mark is introduced. The mark is a human action after reading the
// result; an agent tool would set "viewed" without a viewing, and the
// unreviewed count would stop meaning the thing it was created for.

import (
	"context"
	"errors"
	"log/slog"
	"slices"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/issues"
	skmcp "skein-api/internal/mcp"
)

// reviewUpdateEnvelopeArgs — skein_update arguments that change NOTHING
// on the issue: address, write condition and response shape.
//
// The list enumerates the "harmless" keys on purpose, not the "mutating"
// ones, and that direction is the only safe one. A list of MUTATING keys
// would stay silent on an omission: an argument added to skein_update
// tomorrow and forgotten here would travel with the flag, and the mixed
// request would apply halfway — exactly the outcome the refusal exists
// to forbid. A list of harmless keys errs the other way: a new argument
// is treated as mutating by default, and a request mixed with it is
// refused. A false refusal is visible immediately and is fixed with one
// line; a silent leak is never seen.
//
// `reason` is counted as MUTATING on purpose even though by itself it
// writes nothing: it is only meaningful on a status change, and a
// request "flip the flag and also explain a transition that is not
// happening" is mixed in intent.
var reviewUpdateEnvelopeArgs = []string{"id", "expectedVersion", "force", "verbose"}

// reviewFlagArg parses the `reviewRequired` argument of a skein_update
// call.
//
// Returns:
//   - (nil, nil)      — flag not passed; the call takes the ordinary path;
//   - (&v, nil)       — flag passed ALONE; the call takes its own path;
//   - (nil, refusal)  — flag passed together with something else, or not a boolean.
//
// A mixed-request refusal is a form code (validation_error), not a
// business code: it is a constraint on the request shape, not a domain
// rule. And it is raised BEFORE any write: "refused, not applied
// halfway" means that by the time of the refusal nothing has been
// applied, including the flag itself.
func reviewFlagArg(args map[string]any) (*bool, *mcpgo.CallToolResult) {
	raw, ok := args["reviewRequired"]
	if !ok {
		return nil, nil
	}
	flag, isBool := raw.(bool)
	if !isBool {
		return nil, skmcp.ToolError(ErrCodeValidationError,
			"reviewRequired must be a boolean",
			map[string]any{"got": raw})
	}

	mixedWith := make([]string, 0, len(args))
	for key := range args {
		if key == "reviewRequired" || slices.Contains(reviewUpdateEnvelopeArgs, key) {
			continue
		}
		mixedWith = append(mixedWith, key)
	}
	if len(mixedWith) > 0 {
		slices.Sort(mixedWith)
		return nil, skmcp.ToolError(ErrCodeValidationError,
			"reviewRequired does not travel the ordinary update path and cannot be "+
				"combined with other changes — send it in a call of its own. Nothing "+
				"was written, including the flag",
			map[string]any{"mixedWith": mixedWith})
	}
	return &flag, nil
}

// rejectReviewFlagInBatch refuses a batch-update row that carries the
// review flag.
//
// # Why refuse rather than support
//
// The flag lives outside the ordinary patch path: it does not move the
// version and does not stamp updated_at (INV-2). The skein_update_many
// batch response is built the opposite way — per row it returns
// {id, ok, version}, and `version` in it IS the proof that the write
// applied. A flag sent through the batch would return an unchanged
// version under `ok:true`: to the reader that is indistinguishable from
// "nothing happened", i.e. the acknowledgement would stop acknowledging
// anything.
//
// Second: the three mechanics keys are the author's private bookkeeping,
// and every new surface they travel through is a new place they can be
// shown to the wrong person. The single-issue path pays for that by
// narrowing the acknowledgement (matrix row 120); standing up a second
// such surface for a batch patch that exists for bulk mechanical
// changes is a price with no buyer.
//
// # Why a refusal, not a silent ignore
//
// Before this fix the key in a batch row was not read at all: the row
// returned `ok:true` having written nothing. A lost write under a green
// acknowledgement is the worst kind of refusal: the caller is sure the
// flag is off, and it is still on. A refusal is visible immediately and
// is fixed with one skein_update call.
//
// # One rule for both cases
//
// ANY row with the key is refused — both a lone flag and one mixed with
// ordinary fields. There is no reason to split them: the answer is the
// same ("not here"), and two rules instead of one would be a seam along
// which the mixed case could drift from the lone one, as it already
// drifted between the single and batch paths.
//
// Called from buildUpdateOneInput — the only parser of a batch row,
// before any write.
func rejectReviewFlagInBatch(args map[string]any) *mcpgo.CallToolResult {
	if _, ok := args["reviewRequired"]; !ok {
		return nil
	}
	return skmcp.ToolError(ErrCodeValidationError,
		"reviewRequired is not accepted by skein_update_many — the review flag "+
			"travels its own path (it moves neither version nor updated_at) and a "+
			"batch row could not report it. Send it with skein_update, in a call of "+
			"its own. Nothing in this row was written",
		map[string]any{"field": "reviewRequired"})
}

// mapReviewFlagError maps issues.Store.SetReviewRequired refusals to
// tool codes.
//
// The "not the author" refusal is one code for every non-author,
// administrator included (INV-4). A separate code for an administrator
// would be an oracle: it would report that the mechanics apply to this
// issue, and that the missing privilege was specifically authorship.
func mapReviewFlagError(ctx context.Context, err error, userID uuid.UUID, id string) *mcpgo.CallToolResult {
	switch {
	case errors.Is(err, issues.ErrNotFound):
		return skmcp.ToolError(ErrCodeIssueNotFound,
			"issue does not exist or is not visible", map[string]any{"id": id})
	case errors.Is(err, issues.ErrReviewNotAuthor):
		return skmcp.ToolError(ErrCodeReviewForbidden,
			"only the issue author may change the review flag", map[string]any{"id": id})
	}
	var verr *issues.ErrVersionConflict
	if errors.As(err, &verr) {
		return skmcp.ToolError(ErrCodeVersionConflict,
			"expectedVersion does not match the live version",
			map[string]any{"currentVersion": verr.CurrentVersion})
	}
	slog.ErrorContext(ctx, "mcp tool error",
		"tool", "skein_update",
		"reason", "review flag update failed",
		"userId", userID.String(), "issueId", id, "error", err.Error())
	return skmcp.ToolError(ErrCodeInternalError, "failed to update the review flag", nil)
}

// createReviewRequiredArg parses the optional `reviewRequired` argument
// of a skein_create call.
//
// Returns:
//   - (nil, nil)     — not passed; the author's personal default applies;
//   - (&v, nil)      — passed; the given value applies;
//   - (nil, refusal) — passed as a non-boolean.
//
// A separate function, not req.GetBool("reviewRequired", true): a
// default in the parser would mean that a call which does not mention
// the flag EXPLICITLY passes the built-in default. Then the author's
// personal default would never apply — and US-3 requires that silence
// applies exactly that default.
//
// Unlike skein_update there is no mixing ban here and there cannot be:
// create writes every field at once, in one row and one transaction.
// The ban there guards something else — that the flag does not move
// version or updated_at (INV-2); a row being born has nothing to move.
func createReviewRequiredArg(args map[string]any) (*bool, *mcpgo.CallToolResult) {
	raw, ok := args["reviewRequired"]
	if !ok {
		return nil, nil
	}
	flag, isBool := raw.(bool)
	if !isBool {
		return nil, skmcp.ToolError(ErrCodeValidationError,
			"reviewRequired must be a boolean",
			map[string]any{"got": raw})
	}
	return &flag, nil
}
