package issues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/dependencies"
	"skein-api/internal/notifications"
	"skein-api/internal/projects"
)

// close_outcome.go — the close outcome of an issue (sk-489, T001).
//
// The outcome is a separate enumerated property of a CLOSED issue that answers
// the question "how did it end", which the free-text close reason does not
// answer in a machine-readable way. Progress is computed from it: only
// issues closed with the outcome "done" count as done, the other closed ones
// drop out of the denominator (INV-5) and are shown as a separate "no work" leg.
//
// ONLY the names and their validation live here. Resolving the outcome on a
// transition and on a repeated close, checking the target and creating the
// "duplicate" / "superseded" link are separate work (T003), and it lands in this
// same file; the split across files is chosen so that "which values exist" and
// "how a value is chosen" read side by side.
//
// INV-1 (an outcome exists if and only if the issue is closed) is held at the
// STORAGE level — the CHECK and the normalising trigger of migration 000050 —
// not by these functions: the schema must hold the invariant even when an old
// api version that knows nothing about the outcome works on it (US-10).

// The four outcome values are a closed set (INV-2). There are no others: the server
// rejects anything else on input, and a client that receives an unknown value
// prints it as is and counts it neither as done nor as dropped out.
//
// The strings are the same as those in the issues.close_outcome column and sent
// on the wire in the `closeOutcome` key; there is no separate enum type for the same
// reason as with the statuses above: the value travels through JSON, SQL and
// MCP arguments, and a wrapper around it would cost a conversion at each of
// these points, adding nothing to the validation.
const (
	// CloseOutcomeDone — "done": the work is complete. The default of a TRANSITION to
	// "closed" (US-1); archiving an unclosed non-standing issue has no
	// default since sk-540.4. The only value that progress
	// counts as done. Since sk-540.1 the default applies to REST only:
	// MCP close transitions must name the outcome explicitly.
	CloseOutcomeDone = "done"
	// CloseOutcomeWontDo — "won't do": the work will not happen. Never a
	// default: until sk-540.4 archiving applied it silently, now archiving
	// an unclosed issue requires the outcome to be named.
	CloseOutcomeWontDo = "wont_do"
	// CloseOutcomeDuplicate — "duplicate": the same work is already described by another
	// issue. When closing with a target it creates a `duplicates` link from the
	// closed issue to the target (US-2).
	CloseOutcomeDuplicate = "duplicate"
	// CloseOutcomeSuperseded — "superseded": the issue was replaced by another. When
	// closing with a target it creates a `supersedes` link from the TARGET to the closed one
	// — the direction is the reverse of `duplicates`, and this is not a typo: the target
	// replaces the closed issue (US-2).
	CloseOutcomeSuperseded = "superseded"
)

// validCloseOutcomes — the closed set for validation, a mirror of the CHECK on column
// issues.close_outcome. A set, not a slice, for the sake of a one-line branch
// of the validator — after the pattern of validStatuses above.
var validCloseOutcomes = map[string]struct{}{
	CloseOutcomeDone:       {},
	CloseOutcomeWontDo:     {},
	CloseOutcomeDuplicate:  {},
	CloseOutcomeSuperseded: {},
}

// ValidCloseOutcomes returns the closed set of close outcomes, sorted.
//
// Exported for the same reason as ValidStatuses: a consumer that
// must list ALL the values (an MCP argument description, a refusal hint,
// a guard test) needs one source instead of a hand-written copy.
// A copy diverges silently — exactly how the outbox push headers went out without
// `standing`.
func ValidCloseOutcomes() []string {
	out := make([]string, 0, len(validCloseOutcomes))
	for o := range validCloseOutcomes {
		out = append(out, o)
	}
	sort.Strings(out)
	return out
}

// ValidateCloseOutcome enforces the four-value closed set (INV-2).
//
// Returns the canonical (already lowercase) outcome or a *ValidationError with code
// "invalid_close_outcome". An empty string is a REFUSAL, not "outcome not passed":
// "not passed" is expressed by the caller having no value (a nil pointer
// in the input), and the validator is not called for that case at all. Telling "empty"
// from "not passed" inside the validator would mean allowing an empty string
// as input and losing the only place where it is caught.
//
// The case is NOT folded: "Done" is a refusal. The value is stored in a column under
// a CHECK and travels on the wire as is, so normalising the case
// here would make the validator the only place that knows "Done" and
// "done" are one; everyone who compares the string themselves (SQL, frontend, agent)
// does not know it.
func ValidateCloseOutcome(s string) (string, error) {
	s = strings.TrimSpace(s)
	if _, ok := validCloseOutcomes[s]; !ok {
		return "", newValidationError("invalid_close_outcome",
			"closeOutcome must be one of "+strings.Join(ValidCloseOutcomes(), ", "))
	}
	return s, nil
}

// IsNoWorkOutcome reports whether an outcome means "closed without work" —
// that is, closed with an outcome other than "done".
//
// This is exactly the progress denominator predicate (INV-5): visible children for
// which it is true drop out of the denominator and are counted as a separate leg
// "no work". It is true for three of the four values and false for
// `done`.
//
// An UNKNOWN value gives false — and this is a deliberate choice, not a forgotten
// branch. The predicate answers "has it dropped out", and for a string that cannot
// be here the honest answer is "I do not claim it dropped out": true would exclude
// the issue from the denominator, that is, silently change the progress numbers by
// a value the server does not let in on input. The server computes the numbers
// (INV-2), and an unfamiliar outcome has no right to move them.
//
// An empty string (no outcome — the issue is not closed) by the same rule gives
// false: an unclosed issue has not dropped out, it is simply not closed yet.
//
// ⚠ THIS PREDICATE COMPUTES NO PROGRESS NUMBER — NOT ONE. All six (Done,
// NoWork, Counted on a row and DoneChildren, NoWorkChildren, CountedChildren in
// the summary) are computed by SQL — the `noWork` leg of the progress queries
// (`issues/store.go#epicSummaryNoWorkExpr`, `#epicRollupFor`,
// `issues/tree.go#branchProgressFor`). The function has no production-code
// callers today at all; it exists as the NORMATIVE definition of the rule in Go
// and as a cross-check point for future consumers (the nearest candidate is T008,
// the warning "closed without an assignee").
//
// Because of that INV-5 has two records of one rule: here the set is given as a WHITE
// LIST (three values), in SQL as a BLACK one ("everything that is not `done`"). Today
// the wordings coincide because there are exactly four values; when a fifth is
// added they will diverge — Go will stop counting it as dropped out, SQL will continue.
// This no longer happens silently: the expectation "how many children dropped out" in
// `close_outcome_progress_test.go#wantRowProgress` is computed by THIS predicate,
// and the fact comes from the SQL half, so a divergence turns the table red in all
// six places of the query (added after the review of wave sk-489/A/3 — before it nothing
// held the link between the halves except this paragraph). Adding a value
// to the set — fix BOTH halves.
func IsNoWorkOutcome(outcome string) bool {
	switch outcome {
	case CloseOutcomeWontDo, CloseOutcomeDuplicate, CloseOutcomeSuperseded:
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// Outcome resolution (sk-489, T003) — ONE helper for the two close writers
// ---------------------------------------------------------------------------
//
// Two writers can close an issue: Store.Close (the agent path
// skein_close, skein_close_many, skein_update with status "closed") and
// Store.UpdateStatus (a REST transition and a REST batch, archiving included). Merging
// them into one is a large blast radius and not this task, so "all close paths
// give one semantics" is held not by a shared writer but by a shared helper:
// both call DecideCloseOutcome and both call linkCloseTargetInTx. A rule
// written here once physically cannot diverge between the paths.

// CloseLinkState — the flag of the link by target (US-2): what the close did with
// the "duplicate" / "superseded" link.
//
// The values go on the wire in MCP confirmations (the `closeLink` key),
// so they are strings, not a bool: it matters to the agent to tell "I created the link"
// from "the link already existed", and both cases are a successful close, not a refusal.
type CloseLinkState string

const (
	// CloseLinkNone — no target was passed, there is no link in this operation.
	// The empty string is chosen deliberately: in the confirmation it is dropped by
	// omitempty, and the `closeLink` key appears only where there was a target.
	CloseLinkNone CloseLinkState = ""
	// CloseLinkCreated — the link was created by this operation.
	CloseLinkCreated CloseLinkState = "created"
	// CloseLinkExisted — a link of the needed type and direction already existed;
	// a second one is not created, the close goes through (US-2).
	CloseLinkExisted CloseLinkState = "existed"
)

// CloseOutcomeRequest — the input of outcome resolution: what the caller passed and
// the state in which the writer found the issue.
//
// A struct, not an argument list, because there are seven fields and five of them
// are bool or string: a positional call on two writers would silently diverge
// in argument order.
type CloseOutcomeRequest struct {
	// IssueID — the issue being closed. Needed for exactly one check:
	// the target cannot be the issue itself.
	IssueID string
	// Requested — the EXPLICITLY passed outcome; nil means "the caller said
	// not a word about the outcome". A pointer, not a string, exactly for this
	// distinction: "did not pass" and "passed an empty string" are different intentions,
	// and the second must be a refusal (ValidateCloseOutcome), whereas
	// the first turns on the default.
	Requested *string
	// Target — the target of the "duplicate" / "superseded" link; empty string = no target.
	Target string
	// AlreadyClosed — the issue is already closed, i.e. this is a REPEATED close,
	// not a transition. The default depends on it: for a transition it is "done"
	// (or "won't do" on archiving), for a repeat there is no default
	// at all — the outcome stays as it was (INV-10).
	AlreadyClosed bool
	// PreviousOutcome — the outcome before the operation; empty when the issue was not
	// closed. By INV-1 non-empty exactly when AlreadyClosed.
	PreviousOutcome string
	// Archive marks a close by ARCHIVING (the REST bulk `action=archive`,
	// sk-540.4). Only UpdateStatusInput carries it: Store.Close has no
	// archive path at all — a recorded «no such link», not a forgotten
	// field. Under it the row decides what the request means:
	//   - an unclosed, non-standing row needs an explicit outcome — without
	//     one it is refused with close_outcome_required (there is no silent
	//     wont_do any more);
	//   - an already closed or standing row does NOT take the requested
	//     outcome and target: the closed one keeps its outcome, the standing
	//     one gets done, and no edge is created (TargetDropped).
	Archive bool
	// Standing — the issue is standing. The archive default does not apply to it:
	// a standing issue is an obligation, not work, is not counted in progress,
	// and closing it without an explicit outcome gives "done" (US-1).
	//
	// sk-540.1: the «standing without an outcome → done» default above now
	// holds for REST only. On the MCP surfaces (RequireOnTransition) a
	// standing → closed retire is a transition like any other and needs an
	// explicit outcome.
	Standing bool
	// RequireOnTransition — the caller's surface demands an explicit outcome
	// on a TRANSITION into closed (sk-540.1, CloseInput.RequireOutcomeOnTransition).
	// A transition without Requested is then refused with
	// close_outcome_required instead of defaulting to done. A re-close
	// (AlreadyClosed) is unaffected and keeps the previous outcome.
	RequireOnTransition bool
	// RowStateUnknown marks a FORM-ONLY call made before the row is read —
	// the MCP argument parser and the REST bulk preflight (sk-540.2). Such a
	// caller cannot tell a transition from a re-close, so the rule that
	// depends on it — duplicate/superseded needs closeTarget unless the issue
	// is already closed (see DecideCloseOutcome) — is skipped here and
	// enforced by the writer, which knows the row. AlreadyClosed and
	// PreviousOutcome are meaningless when it is set.
	RowStateUnknown bool
}

// CloseOutcomeDecision — the resolved outcome and everything that follows from it for
// events, notifications and the acknowledgement mark.
type CloseOutcomeDecision struct {
	// Outcome — the outcome the writer must write to the column. Never
	// empty: a closed issue without an outcome violates INV-1.
	Outcome string
	// Previous — the outcome before the operation ("" — the issue was not closed).
	Previous string
	// Changed — a repeated close CHANGED the outcome. Everything else is derived from exactly
	// this: the issue.outcome_changed event, the
	// previousCloseOutcome key in the payload of close events and branch (2) of the
	// acknowledgement-mark reset predicate.
	//
	// A transition to "closed" does NOT set Changed, even when the outcome differs from
	// "done": an unclosed issue had no outcome, there was nothing to change, and
	// the notification about it is an ordinary status notification (R-7).
	Changed bool
	// TargetDropped — an archive (CloseOutcomeRequest.Archive) of an already
	// closed or standing row: the requested outcome and closeTarget were not
	// applied to it, so the writer must not create the target edge either.
	TargetDropped bool
	// NeedsExistingLink — the resolved outcome is duplicate/superseded, no
	// closeTarget was given, and the call CHANGES the outcome of an already
	// closed issue (sk-540.2, exception (a)). That is allowed only when the
	// issue already carries the edge of the matching type and direction; the
	// decider cannot see edges, so the writer checks it in its own
	// transaction with ensureCloseLinkExists before writing anything.
	NeedsExistingLink bool
}

// DecideCloseOutcome resolves the outcome of a closing operation and checks the target.
//
// The default rule (R-4, INV-10, US-11):
//
//   - an explicit outcome is accepted on any input — on a transition and on a repeat;
//   - a TRANSITION without an outcome → "done";
//   - sk-540.4: an ARCHIVE (Archive) of an unclosed non-standing row without
//     an outcome is refused with close_outcome_required; an archive of an
//     already closed or standing row ignores the requested outcome and
//     target (TargetDropped) — closed keeps its outcome, standing gets done;
//   - sk-540.1: with RequireOnTransition (the MCP surfaces) a transition
//     without an outcome is REFUSED with close_outcome_required instead —
//     standing included; the defaults above remain for REST only;
//   - a REPEATED close without an outcome does NOT change the outcome (INV-10). This is
//     the reason why the default is a property of the transition, not of the call:
//     otherwise appending commits or rewriting the reason would erase
//     the set outcome.
//
// The target checks (US-2) are all here, because both surfaces and
// both writers must answer them identically:
//
//   - a target without an EXPLICIT outcome — a refusal. An input that said nothing
//     about the outcome has no right to create a link by default: on a transition the default
//     would give "done" (a target is forbidden with it), on archiving — "won't
//     do" (also forbidden), and on a repeat of an already-duplicate — the previous
//     outcome allowed by the white list, and the link would be created from a call
//     that did not ask for it;
//   - a target with an outcome other than "duplicate" / "superseded" — a refusal;
//   - a target equal to the issue itself — a refusal;
//   - sk-540.2: duplicate / superseded WITHOUT a target — refused on a
//     transition (close_target_required); on an already closed issue it
//     passes when the outcome does not change (exception (b)) and, when it
//     does, only with an existing edge of the matching type and direction
//     (exception (a), NeedsExistingLink — verified by the writer). Skipped
//     for form-only calls (RowStateUnknown).
//
// The state of the target is NOT checked: open, closed with any outcome,
// the parent of the closing issue, a mutual duplicate — all are allowed exactly as
// for a manual link of the same type (US-2, "edges"). The existence and visibility
// of the target are not here: that is a database access, and it lives in
// EnsureCloseTargetVisible.
func DecideCloseOutcome(in CloseOutcomeRequest) (CloseOutcomeDecision, error) {
	var requested string
	if in.Requested != nil {
		v, err := ValidateCloseOutcome(*in.Requested)
		if err != nil {
			return CloseOutcomeDecision{}, err
		}
		requested = v
	}

	// sk-540.4: archiving decides per ROW, so a form-only call skips it. The
	// form checks below (value, target without an explicit outcome) still
	// run on every row: they are properties of the request.
	archiveDropsRequest := in.Archive && !in.RowStateUnknown && (in.AlreadyClosed || in.Standing)

	if strings.TrimSpace(in.Target) != "" {
		target := strings.TrimSpace(in.Target)
		if in.Requested == nil {
			return CloseOutcomeDecision{}, newValidationError("close_target_requires_outcome",
				"closeTarget requires an explicit closeOutcome of "+
					CloseOutcomeDuplicate+" or "+CloseOutcomeSuperseded)
		}
		if requested != CloseOutcomeDuplicate && requested != CloseOutcomeSuperseded {
			return CloseOutcomeDecision{}, newValidationError("close_target_not_allowed",
				"closeTarget is only allowed with closeOutcome "+
					CloseOutcomeDuplicate+" or "+CloseOutcomeSuperseded)
		}
		if target == in.IssueID && !archiveDropsRequest {
			return CloseOutcomeDecision{}, newValidationError("close_target_self",
				"closeTarget cannot be the issue being closed")
		}
	}

	if in.Requested == nil && in.RequireOnTransition && !in.AlreadyClosed {
		return CloseOutcomeDecision{}, CloseOutcomeRequiredError()
	}
	if in.Archive && !in.RowStateUnknown && !archiveDropsRequest && in.Requested == nil {
		return CloseOutcomeDecision{}, ArchiveOutcomeRequiredError()
	}
	if archiveDropsRequest {
		// Owner decision 2026-09-29: the batch-wide outcome and target are
		// meant for the unclosed rows; an already closed row keeps its
		// outcome (INV-10) and a standing one is retired as done (US-1).
		in.Requested, in.Target = nil, ""
	}

	decision := CloseOutcomeDecision{Previous: in.PreviousOutcome}
	switch {
	case in.Requested != nil:
		decision.Outcome = requested
	case in.AlreadyClosed && in.PreviousOutcome != "":
		// INV-10: a repeated close without an outcome does not change the outcome. The
		// non-empty condition is insurance against a row that violates INV-1 (it cannot
		// exist: CHECK and trigger 000050); had it got here, it would have received an
		// empty outcome and tripped the CHECK on write, which is worse than an honest
		// "done".
		decision.Outcome = in.PreviousOutcome
	default:
		decision.Outcome = CloseOutcomeDone
	}
	decision.Changed = in.AlreadyClosed && decision.Outcome != in.PreviousOutcome
	decision.TargetDropped = archiveDropsRequest

	// sk-540.2: duplicate/superseded point somewhere, so they need a target.
	// Two exceptions, both about issues that are already closed:
	//   (a) the outcome of a closed issue CHANGES to duplicate/superseded
	//       and the matching edge already exists — the writer verifies the
	//       edge (NeedsExistingLink), otherwise old issues could not be
	//       corrected;
	//   (b) a re-close that keeps the outcome (explicitly or by omitting it)
	//       — including a historical issue closed before the rule existed
	//       and carrying no edge at all.
	// A transition into closed has no exception: the target is required.
	if outcomeNeedsTarget(decision.Outcome) && strings.TrimSpace(in.Target) == "" && !in.RowStateUnknown {
		switch {
		case !in.AlreadyClosed:
			return CloseOutcomeDecision{}, CloseTargetRequiredError(decision.Outcome)
		case decision.Changed:
			decision.NeedsExistingLink = true
		}
	}
	return decision, nil
}

// outcomeNeedsTarget reports whether an outcome names another issue — the
// original of a duplicate or the replacement of a superseded issue.
func outcomeNeedsTarget(outcome string) bool {
	return outcome == CloseOutcomeDuplicate || outcome == CloseOutcomeSuperseded
}

// CloseTargetRequiredError is the refusal of a duplicate/superseded outcome
// that names no closeTarget where the exceptions do not apply (sk-540.2).
// Code close_target_required; the request is well-formed and the issue's
// current state (not closed yet, or no matching edge) forbids it, so REST
// answers 422.
func CloseTargetRequiredError(outcome string) error {
	return newValidationError("close_target_required",
		"closeOutcome "+outcome+" requires closeTarget — the issue this one "+
			"duplicates or that replaced it; without a target it is accepted only "+
			"when re-closing with the same outcome, or when changing the outcome "+
			"of an already closed issue that already has the matching "+
			dependencies.DepDuplicates+" / "+dependencies.DepSupersedes+" edge")
}

// ensureCloseLinkExists verifies exception (a) of the target rule
// (sk-540.2): an already closed issue whose outcome changes to
// duplicate/superseded without a target must already carry an edge of the
// matching type and direction — `duplicates` FROM the issue, `supersedes`
// TO the issue (closeLinkEdge). The other end is not constrained: any
// original or replacement will do.
//
// A no-op unless decision.NeedsExistingLink. Writers call it inside their
// transaction, before the first write, so the refusal leaves nothing
// behind and a concurrent edge removal cannot slip between check and
// write under SERIALIZABLE.
func ensureCloseLinkExists(ctx context.Context, db CloseTargetDB, issueID string, decision CloseOutcomeDecision) error {
	if !decision.NeedsExistingLink {
		return nil
	}
	query := `SELECT EXISTS(SELECT 1 FROM dependencies WHERE from_id = $1 AND type = $2)`
	depType := dependencies.DepDuplicates
	if decision.Outcome == CloseOutcomeSuperseded {
		query = `SELECT EXISTS(SELECT 1 FROM dependencies WHERE to_id = $1 AND type = $2)`
		depType = dependencies.DepSupersedes
	}
	var exists bool
	if err := db.QueryRow(ctx, query, issueID, depType).Scan(&exists); err != nil {
		return fmt.Errorf("checking %s edge of %s: %w", depType, issueID, err)
	}
	if !exists {
		return CloseTargetRequiredError(decision.Outcome)
	}
	return nil
}

// CloseOutcomeRequiredError is the refusal of a transition into closed that
// names no outcome on a surface where the outcome is mandatory (sk-540.1).
//
// The message lists the closed set: an agent reads it and retries with a
// valid value, and it teaches the rule the schema cannot express — the
// field is optional for a re-close, so it is not marked required there.
func CloseOutcomeRequiredError() error {
	return newValidationError("close_outcome_required",
		"closeOutcome is required when transitioning to closed: pass one of "+
			strings.Join(ValidCloseOutcomes(), ", ")+
			"; repeating a close without it preserves the previous outcome")
}

// ArchiveOutcomeRequiredError is the refusal of archiving an issue that is
// not closed (and not standing) without an explicit outcome (sk-540.4). Same
// code as CloseOutcomeRequiredError — one mistake, one code — with a message
// that names the archive-specific exceptions. The REST batch raises it for
// the whole request in its preflight, and UpdateStatus raises it per row
// when the row was reopened after that preflight.
func ArchiveOutcomeRequiredError() error {
	return newValidationError("close_outcome_required",
		"closeOutcome is required when archiving an issue that is not closed: pass one of "+
			strings.Join(ValidCloseOutcomes(), ", ")+
			"; already closed and standing issues are archived without one")
}

// PrecheckClose is the pre-check of the update surfaces: skein_update /
// skein_update_many with status=closed write the other fields in their own
// transaction BEFORE the close, so a refusal that only CloseWithResult
// raised would arrive after those fields committed. Called before the first
// write, it runs the state-dependent close rules while nothing has been
// written yet:
//
//   - sk-540.1: a transition into closed without an outcome is refused
//     (CloseOutcomeRequiredError); a re-close may omit it;
//   - sk-540.2: duplicate/superseded without closeTarget is refused unless
//     the issue is already closed and either keeps its outcome or already
//     carries the matching edge (CloseTargetRequiredError).
//
// When neither rule can fire — an explicit outcome that needs no target, or
// a target given — it returns without a query. Both checks are repeated
// inside the CloseWithResult transaction (CloseInput.RequireOutcomeOnTransition
// and the writer's own ensureCloseLinkExists): with force=true the version
// predicate is bypassed, so the issue may change between this read and the
// close, and only the in-transaction check sees that.
//
// Returns ErrNotFound for an unknown id.
func (s *Store) PrecheckClose(ctx context.Context, id string, requested *string, target string) error {
	target = strings.TrimSpace(target)
	if requested != nil && (target != "" || !outcomeNeedsTarget(strings.TrimSpace(*requested))) {
		return nil
	}
	var status string
	var previous *string
	err := s.pool.QueryRow(ctx, `SELECT status, close_outcome FROM issues WHERE id = $1`, id).
		Scan(&status, &previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("reading close state of issue %s: %w", id, err)
	}
	previousOutcome := ""
	if previous != nil {
		previousOutcome = *previous
	}
	decision, err := DecideCloseOutcome(CloseOutcomeRequest{
		IssueID:             id,
		Requested:           requested,
		Target:              target,
		AlreadyClosed:       status == StatusClosed,
		PreviousOutcome:     previousOutcome,
		Standing:            status == StatusStanding,
		RequireOnTransition: true,
	})
	if err != nil {
		return err
	}
	return ensureCloseLinkExists(ctx, s.pool, id, decision)
}

// OutcomeChangeResetsReview — branch (2) of the acknowledgement-mark
// reset predicate (US-3, INV-8, SC-009).
//
// Branch (1) — a reset on a TRANSITION to "closed" — lives in the writers themselves
// and reads there `old.Status != StatusClosed`, with no condition on the actor: the
// new result the requester by definition has not seen. Branch (2) — a change of outcome on
// an already closed issue — does have an actor condition: we reset only when the
// outcome goes away FROM "done" (the requester marked the result and there is no
// result any more) AND it is changed NOT by the author (the author knows about their own change).
//
// A change BETWEEN dropped-out outcomes (wont_do → duplicate) does not touch the mark: there
// is nothing to void — there was no done result.
//
// The predicate is shared by the three places where the outcome changes: a repeated close on both
// writers and an edit of a closed issue (T004).
func OutcomeChangeResetsReview(previous, next string, actorUserID, authorUserID uuid.UUID) bool {
	return previous == CloseOutcomeDone && next != previous && actorUserID != authorUserID
}

// CloseTargetDB — the minimal reading surface needed by the target check:
// one QueryRow. Both *pgxpool.Pool and pgx.Tx satisfy it, so
// the check is called both inside the writer's transaction and before the REST batch loop on
// the pool (US-11: the batch is rejected whole BEFORE execution when the target is not
// visible). The model is the unexported projects/access.go#dbq; here the
// interface is exported because the caller lives in another package.
type CloseTargetDB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// EnsureCloseTargetVisible checks that the target exists and is visible to
// the reader, and returns its project.
//
// The order — existence, space, then visibility — is the same as in the
// manual link paths (dependencies/store.go#CreateInTx and #Reparent).
// There are two errors (dependencies.ErrTargetNotFound / #ErrDependencyTargetNotVisible),
// but since sk-674.18 T033 (INV-3) both surfaces answer them identically:
// REST target_not_found, MCP issue_not_found.
//
// Visibility is checked for ANY link direction and WITHOUT a short-circuit
// on a same-named project — unlike the manual paths, where the
// same-named project is short-circuited. Two reasons. First: for "superseded"
// the FROM side of the link is the target, and CreateInTx checks only the TO side, so
// the target would remain unchecked altogether. Second: this helper is called
// also from the REST batch BEFORE the loop, where the project of the closing issue is not yet
// at hand. The extra access is one per call with a target, and it always passes
// for its own project: the closing reader is a member of it.
//
// INV-24 (sk-674.18 T033): the target must lie in the space of the project
// ownerProject — the project of the closing issue. A target of another space
// answers dependencies.ErrTargetNotFound, like a nonexistent one, and this is
// checked BEFORE visibility.
func EnsureCloseTargetVisible(
	ctx context.Context,
	db CloseTargetDB,
	targetID string,
	ownerProject uuid.UUID,
	caller DependenciesCaller,
) (uuid.UUID, error) {
	var projectID uuid.UUID
	err := db.QueryRow(ctx, `SELECT project_id FROM issues WHERE id = $1`, targetID).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, dependencies.ErrTargetNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("loading close target %s: %w", targetID, err)
	}
	same, err := projects.SameSpace(ctx, db, ownerProject, projectID)
	if err != nil {
		return uuid.Nil, err
	}
	if !same {
		return uuid.Nil, dependencies.ErrTargetNotFound
	}
	if _, err := projects.EnsureProjectVisible(ctx, db, projectID, caller.UserID); err != nil {
		if errors.Is(err, projects.ErrProjectNotVisible) {
			return uuid.Nil, dependencies.ErrDependencyTargetNotVisible
		}
		return uuid.Nil, fmt.Errorf("checking close target visibility: %w", err)
	}
	return projectID, nil
}

// closeLinkEdge lays the pair (issue, target) out as an edge of the needed type and
// direction.
//
// The directions are OPPOSITE, and this is not a typo: "duplicate" — the closing issue
// duplicates the target (from = closing, type duplicates), "superseded" — the target
// replaces the closing issue (from = TARGET, type supersedes). The edge must be
// indistinguishable from one created by hand (INV-9), and a manual `supersedes` reads
// "from replaces to".
func closeLinkEdge(issueID, targetID, outcome string) (fromID, toID, depType string) {
	if outcome == CloseOutcomeSuperseded {
		return targetID, issueID, dependencies.DepSupersedes
	}
	return issueID, targetID, dependencies.DepDuplicates
}

// errCloseLinkRaceLost — the edge insert was intercepted by a competitor.
//
// It does not go outside: the writers turn it into a version conflict. The reason
// is this. A uniqueness violation aborts the whole Postgres transaction,
// so "the link already existed" is caught BEFORE the insert by a separate SELECT EXISTS, and
// only the case where between that SELECT and the insert somebody else
// created the edge gets here. The close transaction is no longer usable at that moment:
// everything rolls back — both the close and the edge. The honest answer to the caller is
// "the state went out from under you, re-read and repeat", i.e. exactly a version
// conflict; the repeat will see the edge and answer `existed` (R-5: there is no closed
// duplicate without a link). ErrDependencyExists cannot be handed out — the close
// is not "already existing" — and internal_error would be a lie about the cause.
var errCloseLinkRaceLost = errors.New("close target link lost a race with a concurrent writer")

// closeLinkRaceSeam — the seam between "the edge is not there yet" and the edge insert. In
// production it is nil, and then the check standing on it costs exactly zero: one
// pointer comparison with nil per close-with-target path.
//
// Why the seam exists. Turning errCloseLinkRaceLost into a version conflict is
// the only thing holding the three outcomes forbidden by row 10 of the matrix
// (internal_error and ErrDependencyExists out of the close, a closed duplicate
// without a link). It is reachable ONLY if a competitor inserts the edge in the window between
// the SELECT EXISTS above and the insert below — and this window does not open
// on natural timing: a manual link always gets in earlier than the close reaches
// the linking (before it — a read, an UPDATE, three events and a fan-out). Measured
// independently by two critics of the wave: the branch "the close lost the race" in
// TestCloseTarget_RaceWithManualDependency was never executed, and a mutant
// with the translation removed gave a green run. That is, without the seam the guard
// can neither be checked nor kept from silently disappearing.
//
// It must not be called from production code, assigned only by a test, and
// only with restoring the previous value (the package's tests run
// sequentially). The hook gets exactly what a competitor needs:
// the side, direction and type of the edge this transaction is about to insert.
var closeLinkRaceSeam func(ctx context.Context, fromID, toID, depType string)

// linkCloseTargetInTx creates the link by target in the close or edit
// transaction (R-5: closing with a target and creating the link — one operation).
//
// Steps: existence and visibility of the target → SELECT EXISTS of an edge of the needed type
// and direction → dependencies.CreateInTx. The edge existence check is
// a separate query, not by the insert error, precisely because an insert
// error aborts the transaction (see errCloseLinkRaceLost).
//
// The edge is created by THE SAME call as a manual link, so the
// dependency.created event, metadata and cycle checks are the same (INV-9).
func linkCloseTargetInTx(
	ctx context.Context,
	tx pgx.Tx,
	issueID, targetID, outcome string,
	caller DependenciesCaller,
	actor Actor,
) (CloseLinkState, error) {
	var ownerProject uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT project_id FROM issues WHERE id = $1`, issueID).Scan(&ownerProject); err != nil {
		return CloseLinkNone, fmt.Errorf("loading the project of %s: %w", issueID, err)
	}
	if _, err := EnsureCloseTargetVisible(ctx, tx, targetID, ownerProject, caller); err != nil {
		return CloseLinkNone, err
	}
	fromID, toID, depType := closeLinkEdge(issueID, targetID, outcome)

	var exists bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS(
  SELECT 1 FROM dependencies WHERE from_id = $1 AND to_id = $2 AND type = $3
)`, fromID, toID, depType).Scan(&exists); err != nil {
		return CloseLinkNone, fmt.Errorf("checking %s edge %s -> %s: %w", depType, fromID, toID, err)
	}
	if exists {
		return CloseLinkExisted, nil
	}
	// The lost-race seam (nil in prod) — see closeLinkRaceSeam.
	if closeLinkRaceSeam != nil {
		closeLinkRaceSeam(ctx, fromID, toID, depType)
	}

	if _, err := dependencies.CreateInTx(ctx, tx, fromID,
		dependencies.CreateDepInput{ToID: toID, Type: depType},
		dependencies.CallerAccess{UserID: caller.UserID, Role: caller.Role},
		dependencies.Actor{
			UserID:    actor.UserID,
			Username:  actor.Username,
			Role:      actor.Role,
			SessionID: actor.SessionID,
		}); err != nil {
		if errors.Is(err, dependencies.ErrDependencyExists) ||
			errors.Is(err, dependencies.ErrSerializationRetry) {
			return CloseLinkNone, errCloseLinkRaceLost
		}
		return CloseLinkNone, err
	}
	return CloseLinkCreated, nil
}

// closeOutcomePayload assembles the payload of the close and outcome-change events.
//
// One rule for all three events (issue.status_changed, issue.closed,
// issue.outcome_changed): the closeOutcome key is always present and carries the
// RESOLVED outcome — the one that went into the column, not the one that came on input
// (otherwise archiving without an outcome would hand the subscriber emptiness instead of
// "wont_do"); the previousCloseOutcome key appears ONLY on a repeated
// close that changed the outcome. The fact of the change is read from it too: it is derived
// exactly from the presence of the key, and there is no second flag in the payload (T010).
//
// The outcome goes into the payload, not into new_data, on purpose: new_data of
// issue.status_changed is the JSON string of the status, read by
// outbox/push.go#delegatedStatusHead, including on old pods in the
// rollout window. The payload is additive and does not touch the shape of existing events.
func closeOutcomePayload(d CloseOutcomeDecision) map[string]any {
	payload := map[string]any{"closeOutcome": d.Outcome}
	if d.Changed {
		payload["previousCloseOutcome"] = d.Previous
	}
	return payload
}

// writeOutcomeChangedInTx writes the outcome-change event and its fan-out.
//
// It is called from THREE places that change the outcome: a repeated close on both
// writers (Store.Close, Store.UpdateStatus) and an edit of a closed issue (T004,
// Store.PatchIssue). The helper is shared here precisely because the law "one
// semantics" concerns events too: if these three places diverged even by a field or
// a reason, an outcome change by a batch would write something other than an outcome change
// by an agent, and half of the checks would be green.
//
// The event itself is an ordinary public issue.outcome_changed: field
// closeOutcome, old / new — outcome strings (which the history prints), payload
// — {closeOutcome, previousCloseOutcome}. The fan-out under R-7 is told apart
// by the classifier on the pair "event × recipient" (T010): the author gets exactly one
// notification of the new reason, subscribers — a status line from the
// close event.
//
// ⚠ THE WINDOW THAT WAS CLOSED BY T010, AND WHAT KEEPS IT CLOSED. Before T010
// the reason (migration 000051) did not exist, there was no branch for the type in
// notifications/reasons.go#Classify, and it was not entered into auditOnlyEventTypes — the
// classifier reached `default` and answered NOT "reason not
// named for the recipient" (OutcomeNoReasonForRecipient), but OutcomeUnmapped:
// "the type claims a delivery tract that the registry cannot name". On this
// outcome notifications/fanout.go#FanOut increments
// notifications/metrics.go#fanoutUnmappedEventsTotal and writes an
// ERROR-level record, and the metric annotation calls any non-zero value a defect to
// fix, not traffic to observe. The price of the window was measured on wave
// A/2 by two critics independently: 35 and 39 ERROR lines per run.
//
// T010 closed the window in one commit — the Classify branch, the registry entry and
// migration 000051 — and the closing is held not by this paragraph but by a test:
// notifications/outcome_fanout_test.go#TestOutcomeFanOut_NoUnmappedSignal
// goes red with both halves (the counter and the journal) exactly when the window is
// reopened, on all three write paths at once. The reverse half is
// #TestOutcomeFanOut_UnmappedSignalStillWorks: the signal has not unlearned to
// fire at all.
func writeOutcomeChangedInTx(
	ctx context.Context,
	tx pgx.Tx,
	issueID string,
	old *issueRow,
	actor Actor,
	decision CloseOutcomeDecision,
	reason string,
) error {
	payload := closeOutcomePayload(decision)
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshalling outcome-changed payload for %s: %w", issueID, err)
	}
	eventID, err := WriteIssueEventWithPayload(ctx, tx, EventIssueOutcomeChanged,
		issueID, old.ProjectID, actor, "closeOutcome",
		decision.Previous, decision.Outcome, reason, payload)
	if err != nil {
		return err
	}
	creator, assignee, err := roleContextOf(ctx, tx, old)
	if err != nil {
		return err
	}
	if _, err := notifications.FanOut(ctx, tx, notifications.FanOutInput{
		EventID:        eventID,
		EventType:      EventIssueOutcomeChanged,
		EntityType:     "issue",
		IssueID:        issueID,
		ProjectID:      old.ProjectID,
		CreatorUserID:  creator,
		AssigneeUserID: assignee,
		ActorUserID:    actor.UserID,
		ActorType:      actor.Role,
		Payload:        payloadJSON,
	}); err != nil {
		return err
	}
	return nil
}
