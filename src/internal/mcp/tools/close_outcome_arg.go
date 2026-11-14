package tools

import "skein-api/internal/issueops"

// close_outcome_arg.go — parsing and checking of `closeOutcome` / `closeTarget`
// on the agent surfaces (sk-489, US-1, US-2, US-9).
//
// # Why a separate file and not in place
//
// There are FOUR surfaces that accept this pair, and none calls another:
//
//	skein_close        → close.go#handleClose
//	skein_close_many   → close_many.go#parseCloseManyItem
//	skein_update       → update.go#handleUpdate
//	skein_update_many  → update_run.go#buildUpdateOneInput
//
// The batch path does NOT go through the single one (checked by calls: zero),
// so "put the check into the single tool" means putting it on
// half of the surfaces. Therefore parsing and checks live HERE, and each of the
// four input builders calls the same helper.
//
// # What exactly the divergence costs — measured, not assumed
//
// The previous edition of this header promised that a refusal not intercepted before
// the store would fall into internal_error through the fall-through of
// close_errors.go#classifyCloseError. For the FOUR formal refusals (outcome
// not from the set; target without an explicit outcome; target with done; target with wont_do) this
// is WRONG: they are raised by issues.DecideCloseOutcome inside the transaction as
// *ValidationError, and classifyCloseError catches any ValidationError with its first
// branch — before the target branch and before the fall-through. The code comes out the same
// (review of wave A/4; the executor measured this by mutation and recorded it in 46f7597).
//
// The fall-through does threaten the OTHER two cells — "the target does not
// exist" and "the target is not visible" — but they are not checked here: this
// is a database access, and classifyCloseError renders them with an explicit branch
// regardless of where the parsing stands.
//
// Therefore the difference between "the check exists" and "the check is absent" has to be
// observed not from the code:
//
//	batch     — by the detail `details.id`, which only the input builder
//	            puts in (close_many.go#parseCloseManyItem);
//	single    — by ORDER: parsing stands before GetIssue and before the version
//	            predicates, so a formal refusal must beat
//	            version_conflict and issue_not_found.
//
// Both observations are recorded as assertions —
// close_outcome_mcp_test.go#TestCloseManyMCP_TargetRefusalsAreRowFailures and
// #TestCloseMCP_FormalRefusalPrecedesTheStore respectively.
//
// # One order for all four surfaces (sk-516)
//
// Parsing and the shape check stand BEFORE the store access in EACH of the
// four input builders, not only the closing ones. Before sk-516 that was
// untrue: in skein_update the formal block stood after GetIssue and the visibility check,
// and on the very same input — a wrong argument plus an issue
// that does not exist or is not visible — skein_close answered about the argument, and
// skein_update about the issue. Nobody chose between them, the divergence
// arose by itself, and the caller paid for it: an agent that got
// issue_not_found for ITS OWN wrong argument goes looking for the issue instead of
// fixing the call.
//
// The order of the second surface is recorded by its own assertion —
// update_close_outcome_test.go#TestUpdateMCP_FormalRefusalPrecedesTheStore
// and #TestUpdateManyMCP_FormalRefusalPrecedesTheStore: the batch half
// must be checked too, otherwise it is held only by the fact that nobody
// reordered the lines.
//
// # What is checked here and what is not
//
// Here — everything that needs no database: a value from the four, the target only with
// duplicate / superseded, the target only together with an EXPLICIT outcome, the target is not
// the issue itself. The rules are not rewritten: they are held by issues.DecideCloseOutcome —
// one resolver for both store writers and for all surfaces — and here
// it is only called earlier than the transaction opens.
//
// NOT here — the existence and visibility of the target: that is a database access, and it
// goes in the same transaction as the link (issues.EnsureCloseTargetVisible).
// Its errors are rendered by classifyCloseError.

// closeOutcomeParamDescription / closeTargetParamDescription — descriptions of
// the pair of parameters, shared by the four schemas.
//
// Shared not for economy but by the INV-5 rule already recorded at
// ackVerboseDescription: a schema promising a contract different from its neighbour's
// is an agent that learned different things about the same thing. The default rule for the outcome
// is one for all surfaces, and it must sound the same.
//
// There is economy too, and it is named there: the string gets into
// tools/list once per tool, so every character is paid
// four times. Since sk-540.6 the details live only in the guide's
// «Close outcome» section; the four tool descriptions keep one sentence.
const closeOutcomeParamDescription = "How the issue ENDED. Only done " +
	"counts toward progress; the other three are excluded from scope."

const closeTargetParamDescription = "The issue this one duplicates " +
	"(duplicate) or that replaced it (superseded); creates that edge in " +
	"the same transaction."

// closeOutcomeArgs — see issueops.CloseOutcomeArgs.
type closeOutcomeArgs = issueops.CloseOutcomeArgs

// parseCloseOutcomeArgs — see issueops.ParseCloseOutcomeArgs; the refusal
// as a batch row error.
func parseCloseOutcomeArgs(args map[string]any, issueID, field string) (closeOutcomeArgs, *batchErrorDTO) {
	o, f := issueops.ParseCloseOutcomeArgs(args, issueID, field)
	return o, batchErrorFrom(f)
}

// closeTargetDetails — see issueops.CloseTargetDetails.
func closeTargetDetails(target string) map[string]any { return issueops.CloseTargetDetails(target) }
