package issues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/assignee"
	"skein-api/internal/authz"
	"skein-api/internal/dependencies"
	"skein-api/internal/eventbus"
	"skein-api/internal/idorder"
	"skein-api/internal/mentions"
	"skein-api/internal/notifications"
	"skein-api/internal/person"
	"skein-api/internal/projects"
	"skein-api/internal/redact"
	"skein-api/internal/review"
	"skein-api/internal/search"
	"skein-api/internal/signals"
	"skein-api/internal/spaces"
	"skein-api/internal/status"
)

// commitHashRegex validates the shape of a commit-on-close hash. We
// accept lowercase hex 7..64 chars (covers both abbreviated SHA-1
// `1234567` and full SHA-256). Validated up front in Close() so an
// invalid hash short-circuits before the SERIALIZABLE tx opens.
var commitHashRegex = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// MaxCommitsPerClose caps how many commit refs a single close call may
// attach. Matches the cap documented in the skein_close tool description
// and enforced upstream at the MCP boundary; the store re-checks so the
// limit is a hard contract — not a polite suggestion in the JSON Schema.
//
// Exported so the MCP tool layer can pin its own constant to this value
// (see internal/mcp/tools.maxCommitsPerCloseCall) — drift between the
// two layers would silently break the contract on one surface.
const MaxCommitsPerClose = 10

// pgUniqueViolation is the SQLSTATE class for a unique-constraint hit
// (mirrors projects.pgUniqueViolation — duplicated to avoid a cross-package
// dep solely for one constant).
const pgUniqueViolation = "23505"

// pgSerializationFailure is SQLSTATE 40001, raised when a SERIALIZABLE
// transaction loses to a concurrent writer. We map it to
// ErrVersionConflict (with currentVersion=0 since we never read the
// winner's row) — semantically, "another writer modified this issue
// while you were trying to". The agent retries with the fresh version.
const pgSerializationFailure = "40001"

// pgDeadlockDetected is SQLSTATE 40P01, raised when Postgres breaks a
// deadlock by aborting one of the participants. Like 40001 it rolls the
// loser's transaction back whole, leaving no partial write behind.
const pgDeadlockDetected = "40P01"

// isSerializationFailure reports whether the error is Postgres SQLSTATE
// 40001 (serialization_failure) — that code and no other.
//
// sk-202: the 40P01 omission is deliberate, and this predicate must not be
// widened to include it. Every caller answers a hit by re-reading the live
// version and returning &ErrVersionConflict{CurrentVersion: cv}, i.e. it
// tells the agent "someone else modified this issue — re-read and retry on
// the bumped version". That is true for 40001, where a concurrent writer
// really did commit. It is false for a deadlock: nothing about the row
// changed, so the agent would be handed a version_conflict quoting the
// same version it already held, and a compliant retry would walk straight
// back into the deadlock.
//
// Callers that replay the transaction themselves instead of blaming the
// version want isRetryableTxError — see its doc for the split.
func isSerializationFailure(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgSerializationFailure
}

// isRetryableTxError reports whether err is a Postgres transaction abort
// that is safe to replay verbatim: 40001 (serialization_failure) or 40P01
// (deadlock_detected). Both abort the transaction whole, so a body that
// re-reads its inputs on every attempt sees fresh state on the next one.
// Matches the code set of attachments.isSerializationFailure and
// lockreap.isSerializationFailure.
//
// sk-202: this exists alongside isSerializationFailure above rather than
// replacing it because the two answer different questions.
//
//   - isSerializationFailure: "should this become ErrVersionConflict?"
//     → 40001 only. Used by Patch / UpdateStatus / ReplaceComponents /
//     AddLabel / RemoveLabel / Close.
//   - isRetryableTxError: "should this transaction be replayed?"
//     → 40001 and 40P01. Used by withSerializableRetry (claim, create)
//     and Claim's terminal mapping to ErrSerializationRetry.
//
// Teaching the single shared predicate about 40P01 would have silently
// re-pointed all of the first group at a wrong answer, turning every
// deadlock into a version_conflict with an unchanged version.
func isRetryableTxError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == pgSerializationFailure || pgErr.Code == pgDeadlockDetected
}

// Store is the issues-domain database gateway. Like projects.Store it
// wraps a *pgxpool.Pool and exposes one method per business operation.
// Methods are goroutine-safe because the pool itself owns connections.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a Store backed by the given pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// ----------------- Sentinel errors -----------------

// ErrNotFound is returned by Get*/Update* when no row matches the (project,
// id) pair. Handlers map this to 404 (not 422) — caller asked for a
// specific row and the row doesn't exist.
var ErrNotFound = errors.New("issue not found")

// ErrVersionConflict surfaces optimistic-concurrency loss: a concurrent
// writer bumped version after the caller read but before they wrote.
// CurrentVersion lets the handler echo the live value so the client can
// rebase its edit against it without an extra GET.
type ErrVersionConflict struct{ CurrentVersion int }

func (e *ErrVersionConflict) Error() string {
	return fmt.Sprintf("version conflict — current version is %d", e.CurrentVersion)
}

// ErrParentNotEligible signals the parent issue is missing, archived
// (closed), or otherwise unsuitable for accepting a child. Code is the
// machine-readable reason ("parent_not_found" | "parent_closed").
type ErrParentNotEligible struct{ Code, Message string }

func (e *ErrParentNotEligible) Error() string { return e.Message }

// ErrIDTaken is returned in custom mode when the explicit id collides.
// Distinct from generic ErrConflict so handlers can map to a stable
// "id_taken" code without inspecting constraint names.
var ErrIDTaken = errors.New("id is already taken")

// ErrPinnedNoForce is returned when a status transition would close a
// pinned issue without ?force=true.
var ErrPinnedNoForce = errors.New("pinned issue cannot be closed without force=true")

// ErrAlreadyClaimed is returned by Claim when the issue is already
// locked by another session (or by the same user but a different
// session — R2.3 strict mode). Re-claiming from the SAME session is
// idempotent and does NOT surface this error.
//
// sk-202: this sentinel means "somebody else holds this issue", and
// nothing else. It must only be returned after actually reading a row
// that says so — never as a stand-in for a transient database failure.
// Claim returns it wrapped in an *AlreadyClaimedError carrying the row
// state that justified the rejection; match with errors.Is, inspect with
// errors.As.
var ErrAlreadyClaimed = errors.New("issue is already claimed by another session")

// ErrSerializationRetry is returned by Claim when its SERIALIZABLE
// transaction exhausted its retries against a retryable abort (SQLSTATE
// 40001 serialization_failure or 40P01 deadlock_detected — see
// isRetryableTxError). The transaction rolled back, so the issue is
// untouched and the call is safe to repeat verbatim. Mirrors
// dependencies.ErrSerializationRetry; maps to the "serialization_retry"
// tool code.
//
// sk-202: before this existed, Claim reported a 40001 as ErrAlreadyClaimed
// on the assumption that the only possible concurrent writer was another
// claim. Postgres raises 40001 for any concurrent write to the row after
// the snapshot (and, under SSI, for read/write dependency cycles that need
// not touch the row at all), so that assumption produced a false
// "already claimed by another session" on issues nobody held.
var ErrSerializationRetry = errors.New("claim lost a serialization race — safe to retry")

// AlreadyClaimedError carries the observed row state that made Claim
// reject, so the MCP layer can log WHY without re-reading the row (and
// racing with whoever changed it). Wraps ErrAlreadyClaimed: existing
// errors.Is(err, ErrAlreadyClaimed) call sites keep working unchanged.
//
// Branch identifies which predicate rejected — one of:
//
//   - "locked_by_other"     — locked_by is set to a different session
//   - "assignee_vs_override" — explicit assignee arg names a DIFFERENT
//     USER than the row's assignee (admin claim-on-behalf)
//   - "assignee_vs_actor"   — the row's assignee is somebody other than
//     the caller
//
// Both assignee branches compare user IDENTITY, not handle strings: the
// column stores a free-form handle in any of three documented forms
// (username / e-mail / user UUID) and all three are resolved before the
// comparison (sk-294). A handle that resolves to no user at all is a
// free-form label, and for those the compare stays textual.
//
// Status / LockedBy / Assignee are the values read inside the claim
// transaction. Empty string means SQL NULL.
type AlreadyClaimedError struct {
	Branch   string
	Status   string
	LockedBy string
	Assignee string
}

// ClaimManyError identifies the batch member whose predicate rejected.
// The enclosing transaction is rolled back before this escapes the store.
type ClaimManyError struct {
	ID  string
	Err error
}

func (e *ClaimManyError) Error() string { return fmt.Sprintf("claim %s: %v", e.ID, e.Err) }
func (e *ClaimManyError) Unwrap() error { return e.Err }

func (e *AlreadyClaimedError) Error() string {
	return fmt.Sprintf("%s (branch=%s status=%s lockedBy=%q assignee=%q)",
		ErrAlreadyClaimed.Error(), e.Branch, e.Status, e.LockedBy, e.Assignee)
}

// Unwrap keeps errors.Is(err, ErrAlreadyClaimed) true.
func (e *AlreadyClaimedError) Unwrap() error { return ErrAlreadyClaimed }

// ErrLockedByOther is returned by Patch / UpdateStatus / ReplaceComponents
// / AddLabel / RemoveLabel / Close when the issue's locked_by is set to a
// session id that does not match the caller's. The handler maps this to
// the locked_by_other tool error code. Force=true bypasses this check.
type ErrLockedByOther struct {
	LockedBy string
}

func (e *ErrLockedByOther) Error() string {
	return fmt.Sprintf("issue is locked by another session (%s)", e.LockedBy)
}

// ErrNotLockHolder is returned by Unclaim when the caller's session does
// not hold the lock and Force=false. Idempotent unclaim of an already-
// unlocked issue does NOT surface this error — see Unclaim's contract.
var ErrNotLockHolder = errors.New("caller session does not hold the lock")

// ErrCannotClaimClosed is returned by Claim when the issue is in
// status='closed'. Claim is a "start work" verb; reopening a closed
// issue is an explicit decision and must go through
// skein_update(status=open) first. (sk-18)
var ErrCannotClaimClosed = errors.New("cannot claim a closed issue — reopen it first")

// ErrAssigneeNotAllowed — the assignee was chosen by id (a user UUID) and
// that id names nobody who may work in the issue's project now: a missing
// account and a person without the access are one answer (INV-30,
// sk-674.18 T028; E-32: the person left while the form was open). Nothing
// is written; the previous assignment stays. Handlers answer 422
// spaces.CodeAssigneeNotAllowed.
var ErrAssigneeNotAllowed = errors.New("the assignee chosen by id may not work in this project")

// checkAssigneeChoice refuses a choice by id that INV-30 does not allow:
// a handle in user-id form must resolve, by the delivery rule, to a user who
// may work in project. A username or an e-mail is free text — stored as
// typed, resolved (or not) on read — and is never refused here (E-33, E-37).
// Runs inside the write's transaction, after the space lock the write holds
// (K-7), so a person who left meanwhile is seen as gone.
func checkAssigneeChoice(ctx context.Context, tx pgx.Tx, handle string, project uuid.UUID) error {
	if _, err := uuid.Parse(strings.TrimSpace(handle)); err != nil {
		return nil
	}
	id, err := notifications.ResolveAssigneeUserID(ctx, tx, handle, project)
	if err != nil {
		return err
	}
	if id == uuid.Nil {
		return ErrAssigneeNotAllowed
	}
	return nil
}

// ErrForbidden is returned by Accept when the caller is not the issue's
// current assignee: only the assignee accepts; the project owner's rights
// and role=admin do not let anyone accept for another (US-6, sk-741) —
// otherwise non-assignees could pre-accept tasks for others. The handler
// maps this to the stable `forbidden` error code.
var ErrForbidden = errors.New("caller is not the assignee")

// ErrCannotAcceptClosed is returned by Accept when the issue is in
// status='closed'. Accept is a "yes, I see this task and I'm taking it"
// verb — a closed issue is already done, so there's nothing to accept.
// Symmetric with ErrCannotClaimClosed: both reject the "start work" /
// "ack work" verbs on a terminal-state row. To accept anyway (e.g.
// because the closure was wrong), reopen via skein_update(status=open)
// first. (sk-25)
var ErrCannotAcceptClosed = errors.New("cannot accept a closed issue — reopen it first")

// ErrComponentsWrongProject is returned when caller-supplied componentIds
// belong to a different project than the issue. Names is the offending
// component IDs (string form) for the response body.
type ErrComponentsWrongProject struct{ ComponentIDs []string }

func (e *ErrComponentsWrongProject) Error() string {
	return fmt.Sprintf("%d component(s) do not belong to this project", len(e.ComponentIDs))
}

// Standing transition guards (sk-118 F2c). These are *ValidationError
// values so they flow through IsValidationError on both the MCP and REST
// boundaries; their Code fields are the stable, actionable identifiers the
// frontend / agents branch on. The MCP boundary additionally promotes these
// codes to the top-level envelope `error` (see mapUpdateError /
// classifyUpdateError) so callers don't have to dig into detail.code.
//
//   - cannot_promote_locked_to_standing — an in_progress (claimed/locked)
//     issue cannot be promoted to standing; the lock holder must unclaim
//     first. Standing tasks are never claimed/worked directly.
//   - cannot_work_standing_directly — a standing issue cannot transition to
//     in_progress. You don't work a standing task; you spawn a child
//     (parent.N) and work that.
//   - nested_standing_forbidden — a transition INTO standing was rejected
//     because the issue already has a standing parent OR a standing child
//     (in either representation: issues.parent_id OR a parent-child dep
//     edge). Nesting standing tasks would reintroduce the mutual-retire
//     deadlock (§11.4) — flatten the hierarchy first.
//   - cannot_claim_standing — Claim (skein_claim) was invoked on a standing
//     issue. Standing tasks are never worked directly; spawn a child
//     (skein_create parentId=<this>) and claim that instead. Mirrors how
//     ErrCannotClaimClosed rejects the terminal closed state, but standing
//     is a deliberate-perpetual state, not a terminal one (sk-118 F2d).
var (
	ErrCannotPromoteLockedToStanding = newValidationError(
		"cannot_promote_locked_to_standing",
		"cannot promote a claimed (in_progress) issue to standing — unclaim first")
	ErrCannotWorkStandingDirectly = newValidationError(
		"cannot_work_standing_directly",
		"a standing task is not worked directly — spawn a child (parent.N) and work that instead")
	// ErrCannotDeferStanding rejects standing → deferred. A standing task is a
	// perpetual obligation, not a unit of work that can be parked — deferring it
	// is nonsensical (§11, integration-tests-architecture §6 invariant 9).
	// Demote to `open` first (skein_update status=open), then defer if desired.
	// Sibling of ErrCannotWorkStandingDirectly (which rejects standing →
	// in_progress) — distinct message because the directive differs (demote,
	// not spawn-a-child). Surfaced as a top-level MCP envelope code via
	// standingTransitionCode, identical to the other F2c guards.
	ErrCannotDeferStanding = newValidationError(
		"cannot_defer_standing",
		"a standing task cannot be deferred — demote it to open first (skein_update status=open), then defer")
	ErrNestedStandingForbidden = newValidationError(
		"nested_standing_forbidden",
		"cannot make this issue standing: it already has a standing parent or a standing child; "+
			"standing tasks must not be nested")
	// ErrCannotClaimStanding rejects skein_claim on a standing issue (sk-118
	// F2d). It is a *ValidationError carrying the stable `cannot_claim_standing`
	// code so it flows through IsValidationError on both the MCP and REST
	// boundaries; mapClaimError promotes the code to the top-level envelope
	// `error` (not the generic validation_error) so agents branch on the
	// specific cause and follow the directive.
	ErrCannotClaimStanding = newValidationError(
		"cannot_claim_standing",
		"a standing task is not claimed/worked directly — spawn a child subtask "+
			"(skein_create parentId=<this>) and claim that instead")
)

// ErrStandingHasOpenChildren is returned by Close (sk-118 F2e) when a
// retire-close is attempted on a `standing` issue that still has at least
// one NON-CLOSED child — in EITHER hierarchy representation (the canonical
// `parent-child` dep edge OR the legacy issues.parent_id column). "Non-closed"
// covers open, in_progress AND deferred (status <> 'closed'), so a deferred
// child legitimately blocks the retire (resolve or close it first — that is
// the intended guarantee, §11.4). This guard fires ONLY for standing issues;
// normal epics with open children stay closeable (§11.9), and it is NOT
// bypassed by force=true — the open-children block is a structural invariant
// (mirrors the F2c nested-standing matrix stance), not a permission gate.
//
// It carries the offending child ids and embeds a *ValidationError (code
// "standing_has_open_children") via Unwrap so it flows through
// IsValidationError on both the MCP and REST boundaries; the MCP close mapper
// promotes the code to the top-level envelope `error` (with the ids in
// `childIds`) so agents branch on the specific cause and retire/close the
// children first.
type ErrStandingHasOpenChildren struct {
	ChildIDs []string
	ve       *ValidationError
}

func (e *ErrStandingHasOpenChildren) Error() string { return e.ve.Error() }

// Unwrap exposes the embedded *ValidationError so errors.As (and therefore
// IsValidationError) resolves the stable code/message on both boundaries.
func (e *ErrStandingHasOpenChildren) Unwrap() error { return e.ve }

func newStandingHasOpenChildren(childIDs []string) *ErrStandingHasOpenChildren {
	return &ErrStandingHasOpenChildren{
		ChildIDs: childIDs,
		ve: &ValidationError{
			Code: "standing_has_open_children",
			Message: "cannot retire (close) a standing task while it still has non-closed children — " +
				"close or retire the children first",
		},
	}
}

// ErrInlineDepFailed wraps an error returned by dependencies.CreateInTx
// during an atomic skein_create with inline Dependencies. Carries the
// failing index + (toID, type) so the handler / MCP tool can surface a
// precise "dependencies[i]: <reason>" error. Err is the raw underlying
// dependencies error (ErrTargetNotFound, ErrDependencyTargetNotVisible,
// *ErrDepCycle, *ValidationError, ErrDependencyExists, …) — the caller
// uses errors.Is / errors.As to classify and map to MCP / HTTP codes.
type ErrInlineDepFailed struct {
	Index int
	ToID  string
	Type  string
	Err   error
}

func (e *ErrInlineDepFailed) Error() string {
	return fmt.Sprintf("dependencies[%d] (toId=%s type=%s): %v", e.Index, e.ToID, e.Type, e.Err)
}

// Unwrap exposes the inner dependencies error so handlers can
// errors.Is / errors.As against the typed dependencies sentinels
// without manually peeling the wrapper.
func (e *ErrInlineDepFailed) Unwrap() error { return e.Err }

// isUniqueViolation extracts the constraint name on a SQLSTATE 23505 error.
func isUniqueViolation(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		return pgErr.ConstraintName, true
	}
	return "", false
}

// ----------------- Issue create -----------------

// CreateIssue inserts the new row, claims its ID per the project's
// default_id_mode (or hierarchical when ParentID is set), seeds the
// issue_components / issue_labels junctions, and writes an issue.created
// event — all under one SERIALIZABLE transaction so partial states (issue
// without component refs, counter advanced without a row) cannot leak.
//
// Returns the freshly-loaded IssueDetail (counts re-queried after commit)
// plus a typed error for the handler to map.
func (s *Store) CreateIssue(
	ctx context.Context,
	projectID uuid.UUID,
	idPrefix string,
	defaultIDMode string,
	input CreateIssueInput,
	creatorID uuid.UUID,
	actor Actor,
) (*IssueDetail, error) {
	// sk-28 store-level backstop. MCP handlers already validate at the
	// tool boundary; REST handlers funnel raw struct fields straight here,
	// so the same guard runs again for them. Cheap on every call.
	if err := validateCreateInputProse(input); err != nil {
		return nil, err
	}
	source := input.Source
	if source == "" {
		source = SourceHuman
	}

	var (
		newID          string
		inheritedComps []uuid.UUID
		committed      []CommittedState
	)
	// sk-305: replay the transaction on a class-40 abort instead of handing
	// the caller an internal_error. Child-id allocation (nextChildID) writes
	// the per-parent child_counters row, so two agents creating children
	// under the SAME parent serialise on it and Postgres aborts one with
	// 40001 — a routine outcome meaning "run me again", not a failed write.
	// Observed in production at a few percent of creates under load
	// before this retry existed.
	//
	// The closure only assigns to newID / inheritedComps, which a replay
	// overwrites — no side effects escape the transaction.
	err := s.withSerializableRetry(ctx, "createIssue", createMaxAttempts, func(tx pgx.Tx) error {
		// sk-565 (P3): per-attempt creator projection, built afresh on
		// every replay so an aborted attempt's reads never leak forward.
		att := newCreateAttempt(creatorID, &input)
		id, inh, _, ierr := s.createIssueInTx(ctx, tx, att, projectID, idPrefix, defaultIDMode, input, source, creatorID, actor, nil, false)
		if ierr != nil {
			return ierr
		}
		// sk-607: the post-state, read inside the transaction, so the
		// create can still be acknowledged if the read-back after the
		// commit fails.
		st, serr := committedStatesInTx(ctx, tx, id)
		if serr != nil {
			return serr
		}
		newID = id
		inheritedComps = inh
		committed = st
		return nil
	})
	if err != nil {
		// sk-350.3: same tail gap sk-303.17 closed for AddComment /
		// UpdateComment / DeleteComment / CreateEpicWithChildren, left open
		// here on purpose at the time (this IS the sk-305 site the whole
		// retry was built for). Exhausting createMaxAttempts against a
		// genuine class-40 abort used to hand the caller the raw
		// *pgconn.PgError straight through — past every named branch in
		// mapCreateError / mapIssueErr, into their internal_error default.
		// Degrade it to the SAME ErrSerializationRetry sentinel Claim and
		// the sk-303.17 sites already use for "no durable change, safe to
		// retry verbatim" — NOT ErrVersionConflict: CreateIssue is an
		// INSERT, so there is no row yet whose version a re-read could
		// quote, and a caller told "version stale, re-read" would re-read
		// an issue that was never created.
		return nil, s.mapSerializationExhausted(ctx, err, "createIssue", createMaxAttempts)
	}
	afterCommit(ctx, "create", newID)
	// sk-607: the issue exists from here on. The read-back runs detached
	// from the caller's context; if even that fails, the caller gets the
	// committed state as *ErrReadBackFailed, never a plain error that
	// reads as "nothing was created".
	details, err := s.readBack(ctx, "create", committed)
	if err != nil {
		var rb *ErrReadBackFailed
		if errors.As(err, &rb) && len(inheritedComps) > 0 {
			rb.InheritedComponentIDs = inheritedComps
		}
		return nil, err
	}
	dto := details[0]
	// Surface the "echo what happened" channel only when inheritance
	// actually fired — keep the wire shape clean on every other create.
	if len(inheritedComps) > 0 {
		dto.InheritedComponentIDs = inheritedComps
	}
	return dto, nil
}

// createIssueInTx is the tx-bound body of CreateIssue. Extracted so
// CreateEpicWithChildren (sk-32 slice 4) can drive multiple inserts
// inside a single SERIALIZABLE tx without each child opening its own
// transaction. The signature mirrors CreateIssue minus the (ctx, tx)
// surface change and accepts `source` already-resolved (callers map
// from `agent_directed` / `agent_autonomous` / `human` before invoking).
//
// Returns the chosen ID on success. On failure the caller must propagate
// the error so the outer BeginTxFunc rolls back.
func (s *Store) createIssueInTx(
	ctx context.Context,
	tx pgx.Tx,
	att *createAttempt,
	projectID uuid.UUID,
	idPrefix string,
	defaultIDMode string,
	input CreateIssueInput,
	source string,
	creatorID uuid.UUID,
	actor Actor,
	parentVerdict *ancestorVerdict,
	needChildVerdict bool,
) (string, []uuid.UUID, ancestorVerdict, error) {
	var (
		newID                 string
		inheritedComponentIDs []uuid.UUID
		childVerdict          ancestorVerdict
	)
	err := func(tx pgx.Tx) error {
		// K-7 (sk-674.18 T018): an assignee is set under the shared lock
		// of the project's space, first, so it serialises with a user
		// leaving the space (INV-12, E-2).
		if input.Assignee != nil && *input.Assignee != "" {
			if err := spaces.ShareProjectSpace(ctx, tx, projectID); err != nil {
				return err
			}
		}
		// Step 1: choose ID. Hierarchical when parent set; else fall through
		// the project's default mode. Custom mode requires an explicit ID.
		var (
			chosenID    string
			err         error
			parentRowMu *parentRow // populated by the parentID case below; used by the component-inheritance step.
		)
		switch {
		case input.ParentID != nil && *input.ParentID != "":
			// sk-35 — parent's project is no longer required to match
			// currentProj. The child inherits the parent's id prefix
			// (so its ID is parent.N) but its `project_id` column stays
			// at `projectID` (current project). Cross-project hierarchy
			// is allowed; cosmetic discord between prefix and project
			// handle is accepted.
			parent, perr := s.fetchParentCrossProject(ctx, tx, *input.ParentID)
			if perr != nil {
				return perr
			}
			parentRowMu = parent
			// Visibility — caller must be able to see the parent's
			// project (otherwise we'd leak existence). When the parent
			// is in a different project from the new issue, run the
			// dedicated visibility check; same-project is implicit
			// (caller already had to see currentProj to reach here).
			if parent.ProjectID != projectID {
				if input.DependenciesCaller == nil {
					return fmt.Errorf("DependenciesCaller is required when creating a cross-project child")
				}
				// INV-24 (sk-674.18 T033): cross-project, never cross-space —
				// a parent of another space answers as a nonexistent one,
				// before visibility.
				same, serr := projects.SameSpace(ctx, tx, parent.ProjectID, projectID)
				if serr != nil {
					return serr
				}
				if !same {
					return &ErrParentNotEligible{Code: "parent_not_found", Message: "parent issue does not exist"}
				}
				if _, verr := projects.EnsureProjectVisible(ctx, tx,
					parent.ProjectID, input.DependenciesCaller.UserID); verr != nil {
					if errors.Is(verr, projects.ErrProjectNotVisible) {
						// INV-3 (sk-674.18 T033): the same answer as a
						// nonexistent parent.
						return &ErrParentNotEligible{Code: "parent_not_found", Message: "parent issue does not exist"}
					}
					return fmt.Errorf("checking parent visibility: %w", verr)
				}
				// sk-565 (P5): the same answer the default-assignee walk
				// would ask for; not asked twice in one attempt.
				att.markVisible(parent.ProjectID, true)
			}
			// Closed parents reject children — once an epic is closed, it's
			// frozen. (R1.3 spec: parent_archived → 422.)
			if parent.Status == StatusClosed {
				return &ErrParentNotEligible{
					Code:    "parent_closed",
					Message: "parent issue is closed; cannot add children",
				}
			}
			// sk-118 (F2i / R7): nested-standing ban on the CREATE path.
			// CreateIssue writes the parent-child edge via a DIRECT tx.Exec
			// below (NOT dependencies.CreateInTx), so the F2e/F2c nested guards
			// do NOT cover skein_create(parentId=<standing>, standing:true).
			// A standing child under a standing parent reintroduces the §11.4
			// mutual-retire deadlock — reject before any insert. (A NON-standing
			// child under a standing parent is allowed; only standing-under-
			// standing is banned.) This is the only nested-standing shape
			// reachable via parentId at create time: the new issue is brand-new,
			// so its only possible standing relation is to this parent.
			if input.Standing && parent.Status == StatusStanding {
				return ErrNestedStandingForbidden
			}
			chosenID, err = nextChildID(ctx, tx, *input.ParentID)
			if err != nil {
				return fmt.Errorf("claiming child id: %w", err)
			}

		case defaultIDMode == "custom":
			if strings.TrimSpace(input.ID) == "" {
				return newValidationError("id_required",
					"project is in custom id mode but no id was provided")
			}
			canonical, _, verr := ValidateCustomID(input.ID, idPrefix)
			if verr != nil {
				return verr
			}
			chosenID = canonical

		case defaultIDMode == "hash":
			chosenID, err = nextHashID(ctx, tx, idPrefix)
			if err != nil {
				return err
			}

		default: // "counter" and any unknown mode falls back to counter
			chosenID, err = nextCounterID(ctx, tx, projectID, idPrefix)
			if err != nil {
				return err
			}
		}

		// Step 1b: inherit componentIds from parent when the caller
		// omitted them (ComponentIDsExplicit=false). Mirrors the
		// dominant industry pattern (Jira / Linear / YouTrack / Asana):
		// new subtasks default to the parent's component set so they
		// appear under the same component-scoped UI tree node. The
		// caller can always override post-create via skein_update.
		//
		// Rules (see sk- componentIds-inheritance task body):
		//   - Explicit empty array (`componentIds: []`) is honoured as
		//     "no component" — only the omitted (nil + !Explicit) path
		//     triggers inheritance.
		//   - No parentId → nothing to inherit from.
		//   - Cross-project parent (parent.ProjectID != projectID) is
		//     skipped: component UUIDs only resolve inside the child's
		//     own project; partial intersect would be a footgun.
		//   - Parent with no components attached → silent no-op, no
		//     warning surfaced.
		//
		// inheritedComponentIDs is the outward signal used by the
		// CreateIssue wrapper to populate IssueDetail.InheritedComponentIDs
		// — empty/nil means inheritance did not fire on this call.
		if !input.ComponentIDsExplicit && len(input.ComponentIDs) == 0 &&
			parentRowMu != nil && parentRowMu.ProjectID == projectID {
			inh, ierr := s.componentsForTx(ctx, tx, parentRowMu.ID)
			if ierr != nil {
				return fmt.Errorf("loading parent components for inheritance: %w", ierr)
			}
			if len(inh) > 0 {
				input.ComponentIDs = inh
				inheritedComponentIDs = inh
			}
		}

		// Step 2: validate component IDs belong to the same project. We
		// run this BEFORE the issue insert so we can short-circuit with a
		// clean 422 instead of rolling back a half-built row.
		if len(input.ComponentIDs) > 0 {
			if err := assertComponentsInProject(ctx, tx, projectID, input.ComponentIDs); err != nil {
				return err
			}
		}

		// Step 2b (sk-565, US-1, US-6, P2, P6): the default assignee.
		//
		// Decided BEFORE the insert, from the declared parent, so the row,
		// issue.created, the acceptance stamp, fan-out and the response all
		// carry one value. It does not fire when: P1 is off (REST, INV-4);
		// an explicit non-empty assignee was given (INV-1); the issue is
		// standing (INV-3); the creator's setting is off (US-2); or the
		// nearest assigned ancestor is someone else's (US-6, INV-11). The
		// actual assignee goes into this call's local copy of the input.
		//
		// The chain verdict is computed when the default needs it, or when
		// the caller will hand it to children created in this attempt (P6:
		// an epic's children); a verdict passed in by the caller replaces
		// the walk — the epic of this call is the children's parent.
		explicitAssignee := input.Assignee != nil && *input.Assignee != ""
		substituted := false
		var chain ancestorVerdict
		chainKnown := false
		resolveChain := func() error {
			if chainKnown {
				return nil
			}
			if parentVerdict != nil {
				chain = *parentVerdict
			} else if pid := declaredParentID(&input); pid != "" {
				v, werr := chainVerdict(ctx, tx, att, projectID, pid, input.DependenciesCaller, creatorID)
				if werr != nil {
					return werr
				}
				chain = v
			}
			chainKnown = true
			return nil
		}
		if input.AssignCreatorByDefault && !explicitAssignee && !input.Standing {
			username, on, aerr := att.creator.AssignDefault(ctx, tx)
			if aerr != nil {
				return aerr
			}
			if on {
				if err := resolveChain(); err != nil {
					return err
				}
				if chain.allowsDefault() {
					input.Assignee = &username
					substituted = true
				}
			}
		}

		// Step 3: insert issues row.
		//
		// sk-35: `issues.parent_id` is FROZEN — we never write it from
		// the main code path. The new canonical representation of
		// hierarchy is a `parent-child` dep edge (inserted in Step 3a
		// below). The column stays NULL on every new row and the
		// readers below derive the parent from the dependencies graph.
		// `parentArg` is left as `any(nil)` so the placeholder binds to
		// SQL NULL.
		var parentArg any

		var sourceContextArg, assigneeArg, externalRefArg any
		if input.SourceContext != nil {
			sourceContextArg = *input.SourceContext
		}
		if input.Assignee != nil {
			assigneeArg = *input.Assignee
		}
		if input.ExternalRef != nil {
			externalRefArg = *input.ExternalRef
		}

		var estimatedArg any
		if input.EstimatedMinutes != nil {
			estimatedArg = *input.EstimatedMinutes
		}
		var dueArg, deferArg any
		if input.DueAt != nil {
			dueArg = input.DueAt.UTC()
		}
		if input.DeferUntil != nil {
			deferArg = input.DeferUntil.UTC()
		}

		// Resolve the assignee (case-insensitively, across all three handle
		// forms the column accepts — username, e-mail, user UUID)
		// once up-front. We need the resolved user id for two purposes:
		// (1) auto-accept-on-self-assign (Phase 1 ui-redesign — see
		// accepted_at / accepted_by columns below) and (2) auto-watcher
		// in Step 7 below. ResolveAssigneeUserID returns uuid.Nil when the
		// assignee string doesn't map to any user.
		var assigneeUserID uuid.UUID
		switch {
		case substituted:
			// sk-565: the default assigns the creator by username (D6);
			// the identity is known, no lookup needed.
			assigneeUserID = creatorID
		case input.Assignee != nil && *input.Assignee != "":
			if err := checkAssigneeChoice(ctx, tx, *input.Assignee, projectID); err != nil {
				return err
			}
			rid, rerr := notifications.ResolveAssigneeUserID(ctx, tx, *input.Assignee, projectID)
			if rerr != nil {
				return rerr
			}
			assigneeUserID = rid
		}

		// sk-565 (P6): the verdict this issue hands to children created in
		// the same attempt. An issue that got an assignee — any way — is
		// their nearest assigned ancestor; one left unassigned passes its
		// own chain's verdict through.
		if needChildVerdict {
			if input.Assignee != nil && *input.Assignee != "" {
				if assigneeUserID != uuid.Nil && assigneeUserID == creatorID {
					childVerdict = verdictOwner
				} else {
					childVerdict = verdictForeign
				}
			} else {
				if err := resolveChain(); err != nil {
					return err
				}
				childVerdict = chain
			}
		}

		// Auto-accept-on-self-assign: if the resolved assignee equals the
		// creator, seed accepted_at = now() / accepted_by = creatorID at
		// INSERT time.
		//
		// sk-303.13 / T021: this stamp gets an `issue.accepted` event too
		// (step 6b below). On this path there is no "empty → filled"
		// transition on a LIVE row — the row is born stamped — so the event
		// is written together with the creation rather than after it
		// (US-11). It is journal-only: an issue you filed on yourself
		// notifies nobody.
		//
		// sk-118 (F2i): a standing task is a perpetual obligation, never
		// claimed/worked in place — so it is NOT auto-accepted even when the
		// creator self-assigns. Auto-accept only fires on the default (open)
		// create path. (started_at / lock are never written by this INSERT on
		// ANY create path — standing simply keeps accepted_at NULL too.)
		var acceptedAtArg, acceptedByArg any
		var acceptedAtOnCreate time.Time
		if !input.Standing && assigneeUserID == creatorID && assigneeUserID != uuid.Nil {
			acceptedAtOnCreate = time.Now().UTC()
			acceptedAtArg = acceptedAtOnCreate
			acceptedByArg = creatorID
		}
		// One decision, shared by every path that touches the stamp. A
		// fresh row never had one, so hadStamp is false by construction.
		acceptance := ClassifyAcceptance(false, acceptedAtArg != nil, false)

		// sk-118 (F2i): parameterise the inserted status. Default stays
		// 'open' (byte-identical to the legacy path); input.Standing flips it
		// to 'standing'. Bound from a validated package constant, never a
		// caller-supplied string.
		insertStatus := StatusOpen
		if input.Standing {
			insertStatus = StatusStanding
		}

		// sk-303.14 / T009: the "review required" flag is set EXPLICITLY
		// on every create path. This is the only issue-insert point in
		// the product (CreateEpicWithChildren calls it for the epic
		// and for each child), so "every create path yields one
		// result" (US-3) is guaranteed by construction, not by
		// checking a list of paths.
		//
		// The column's schema DEFAULT (false) stays as a safety net
		// for writers that do not know about the column, and is NOT
		// a working value: it expresses a different requirement —
		// "issues created before the release do not require review"
		// (SC-015). Relying on it here would pass the historical
		// default off as the author's personal default.
		reviewRequired, err := resolveReviewRequiredOnCreate(ctx, tx, att.creator, input.ReviewRequired)
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
INSERT INTO issues (
    id, project_id, parent_id, version,
    title, description, type, priority, status, pinned,
    source, source_context, assignee,
    design, acceptance_criteria, notes,
    estimated_minutes, due_at, defer_until,
    external_ref, created_by, created_at, updated_at, metadata,
    accepted_at, accepted_by, review_required
) VALUES (
    $1, $2, $3, 1,
    $4, $5, $6, $7, $21, false,
    $8, $9, $10,
    $11, $12, $13,
    $14, $15, $16,
    $17, $18, now(), now(), '{}'::jsonb,
    $19, $20, $22
)`,
			chosenID, projectID, parentArg,
			input.Title, input.Description, input.Type, input.Priority,
			source, sourceContextArg, assigneeArg,
			input.Design, input.AcceptanceCriteria, input.Notes,
			estimatedArg, dueArg, deferArg,
			externalRefArg, creatorID,
			acceptedAtArg, acceptedByArg, insertStatus, reviewRequired,
		)
		if err != nil {
			if name, ok := isUniqueViolation(err); ok && name == "issues_pkey" {
				// Custom mode collision OR hash mode race. Both surface as 422
				// id_taken to the caller.
				return ErrIDTaken
			}
			return fmt.Errorf("inserting issue: %w", err)
		}

		// Step 3a (sk-35): write the canonical parent-child dep edge.
		// chosenID is the FROM side (child), input.ParentID is the TO
		// side (parent). Direct tx.Exec rather than
		// dependencies.CreateInTx because the latter runs validation /
		// visibility / cycle checks we've already done above (and a
		// brand-new child can't be in a cycle).
		if input.ParentID != nil && *input.ParentID != "" {
			// NOTE: this edge does NOT emit a dependency.created event by
			// design. Consumers that rebuild parent state must read
			// issues.created.parentId from the snapshot in events.new_data,
			// OR query the dependencies table directly.
			if _, err := tx.Exec(ctx, `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'parent-child', '{}'::jsonb, 1, $3, now())`,
				chosenID, *input.ParentID, creatorID,
			); err != nil {
				return fmt.Errorf("inserting parent-child dep edge: %w", err)
			}
		}

		// Step 4: components junction.
		for _, cid := range input.ComponentIDs {
			if _, err := tx.Exec(ctx, `
INSERT INTO issue_components (issue_id, component_id) VALUES ($1, $2)
ON CONFLICT DO NOTHING`, chosenID, cid); err != nil {
				return fmt.Errorf("linking component %s: %w", cid, err)
			}
		}

		// Step 5: labels.
		for _, label := range input.Labels {
			if _, err := tx.Exec(ctx, `
INSERT INTO issue_labels (issue_id, label) VALUES ($1, $2)
ON CONFLICT DO NOTHING`, chosenID, label); err != nil {
				return fmt.Errorf("linking label %q: %w", label, err)
			}
		}

		// Step 6: event. New issue → old=nil, new=summary snapshot. We
		// snapshot the input + chosenID (cheaper than re-querying for the
		// event payload, and the audit log doesn't need DB-trip-truth).
		snap := map[string]any{
			"id":           chosenID,
			"projectId":    projectID,
			"parentId":     input.ParentID,
			"title":        input.Title,
			"type":         input.Type,
			"priority":     input.Priority,
			"status":       insertStatus, // sk-118 F2i: standing creates land as 'standing' in the audit snapshot, not 'open'
			"source":       source,
			"assignee":     input.Assignee,
			"componentIds": input.ComponentIDs,
			"labels":       input.Labels,
		}
		eventID, err := WriteIssueEvent(ctx, tx, EventIssueCreated, chosenID, projectID, actor, "", nil, snap, "")
		if err != nil {
			return err
		}

		// Step 6b (sk-303.13 / T021): the acceptance stamp seeded above
		// gets its own audit event, so no path stamps acceptance silently
		// (INV-5). No fan-out: ClassifyAcceptance says Deliver=false for
		// every automatic acceptance, and calling fan-out for an event
		// that has no reason by construction would only add a query and a
		// failure mode to the create path.
		if acceptance.WriteEvent {
			if _, err := WriteIssueEvent(ctx, tx, EventIssueAccepted, chosenID, projectID,
				actor, "accepted_at",
				map[string]any{"acceptedBy": nil, "acceptedAt": nil},
				map[string]any{"acceptedBy": creatorID, "acceptedAt": acceptedAtOnCreate},
				acceptanceReasonAuto); err != nil {
				return err
			}
		}

		// Step 7: auto-watchers. If the assignee resolved to a real user
		// (resolution happened up-front so we can decide accepted_at)
		// they're auto-watched, whoever created the issue. The CREATOR's
		// subscription is decided in Step 11, after the inline
		// dependencies: whether the issue is a child depends on the final
		// hierarchy, and an inline parent-child edge makes it one just as
		// ParentID does (sk-551.10 review). An explicit unwatch is honoured
		// by AddIssueWatcherScoped: an opt-out of the pair (user, issue), or
		// an old-version tombstone, makes the automatic add a no-op
		// (sk-622.21, US-1, K3).
		//
		// sk-565 (D7, P7, INV-7): an assignee put there by the creator's
		// «assign to me» setting is NOT auto-watched here. At creation the
		// creator's subscriptions stay exactly what the author rule of Step
		// 11 gives an unassigned issue; the acceptance stamp above and the
		// fan-out below still treat the assignee as a self-assignment. Only
		// this moment is special: later paths (reopen, reassignment) do not
		// tell a substituted assignee from an explicit one (US-9).
		if assigneeUserID != uuid.Nil && !substituted {
			if err := notifications.AddIssueWatcher(ctx, tx, chosenID, assigneeUserID, notifications.ReasonAssignee); err != nil {
				return err
			}
		}

		// Step 8: fan-out. issue.created notifies project_watchers (the
		// issue itself has no watchers other than creator+assignee, who
		// are excluded as the actor / via UNIQUE). FanOut is a no-op for
		// non-notifiable events, so this stays cheap when the type list
		// changes.
		if _, err := notifications.FanOut(ctx, tx, notifications.FanOutInput{
			EventID:    eventID,
			EventType:  EventIssueCreated,
			EntityType: "issue",
			IssueID:    chosenID,
			ProjectID:  projectID,
			// Role path: an issue created ALREADY assigned installs an
			// assignee, and for that person the event is "Assigned issue",
			// not "New issue" — the more specific reason wins. A
			// self-assign resolves to the actor and is excluded there.
			CreatorUserID:     creatorID,
			AssigneeUserID:    assigneeUserID,
			NewAssigneeUserID: assigneeUserID,
			ActorUserID:       actor.UserID,
			ActorType:         actor.Role,
		}); err != nil {
			return err
		}

		// Step 9: R4.3 — scan description / notes / acceptance_criteria
		// / design for @username mentions, resolve against project
		// membership, auto-watch, emit issue.mention events, and write
		// directed notifications. Inline in the SERIALIZABLE tx (plan
		// §F-13) so a mention failure rolls back the issue create.
		if err := HandleIssueMentions(ctx, tx, chosenID, projectID, actor,
			IssueMentionTexts{
				Description:        input.Description,
				Notes:              input.Notes,
				AcceptanceCriteria: input.AcceptanceCriteria,
				Design:             input.Design,
			},
			nil, /* CREATE path — no oldTexts to diff against */
		); err != nil {
			return err
		}

		// Step 10: inline dependencies. Each edge is created via
		// dependencies.CreateInTx inside the SAME SERIALIZABLE tx so the
		// whole skein_create call is atomic — any failed edge rolls back
		// the issue insert. Cycle / visibility / unique / FK errors are
		// wrapped in ErrInlineDepFailed with the failing index so the
		// handler can surface "dependencies[i] ...".
		if len(input.Dependencies) > 0 {
			if input.DependenciesCaller == nil {
				return fmt.Errorf("DependenciesCaller is required when Dependencies is non-empty")
			}
			depActor := dependencies.Actor{
				UserID:    actor.UserID,
				Username:  actor.Username,
				Role:      actor.Role,
				SessionID: actor.SessionID,
			}
			depCaller := dependencies.CallerAccess{
				UserID: input.DependenciesCaller.UserID,
				Role:   input.DependenciesCaller.Role,
			}
			for i, dep := range input.Dependencies {
				_, derr := dependencies.CreateInTx(ctx, tx, chosenID, dependencies.CreateDepInput{
					ToID: dep.ToID,
					Type: dep.Type,
					Gate: dep.Gate,
				}, depCaller, depActor)
				if derr != nil {
					return &ErrInlineDepFailed{Index: i, ToID: dep.ToID, Type: dep.Type, Err: derr}
				}
			}
		}

		// Step 11: the creator's auto-subscription (sk-551.10). The
		// creator watches their own new issue UNLESS one of their agents
		// filed it below the top-level errand or on its own initiative —
		// creatorAutoWatches holds the rule. "Below" is read from the
		// canonical hierarchy as this transaction left it (ParentID's edge
		// from Step 3a and any inline parent-child edge from Step 10), the
		// same reading the reopen path uses.
		hasParent, err := issueHasParentInTx(ctx, tx, chosenID)
		if err != nil {
			return err
		}
		if creatorAutoWatches(source, hasParent) {
			if err := notifications.AddIssueWatcher(ctx, tx, chosenID, creatorID, notifications.ReasonCreator); err != nil {
				return err
			}
		}

		// sk-622.21 T015 (US-11): personal signal "Issue created" —
		// published after the enclosing transaction commits.
		signalCreated(tx, creatorID, assigneeUserID)

		newID = chosenID
		return nil
	}(tx)
	if err != nil {
		return "", nil, verdictNoAssignedAncestor, err
	}
	return newID, inheritedComponentIDs, childVerdict, nil
}

// CreateEpicWithChildren executes one SERIALIZABLE tx that inserts an
// epic followed by N children, all sharing the same id-generation
// strategy and event fan-out as a regular CreateIssue. Used by
// skein_epic_create (sk-32 slice 4) to make epic decomposition atomic
// — any child failure (validation, race, FK) rolls back the entire
// batch, including the epic itself. Returns the freshly-loaded
// IssueDetail for the epic and each child in input order.
//
// Caller is responsible for:
//   - resolving `source` (agent_directed / agent_autonomous / human)
//     against the agent/human kind before invocation;
//   - clamping len(childTemplates) at the tool layer (50 max per spec);
//   - supplying DependenciesCaller alongside a CROSS-PROJECT
//     epicInput.ParentID, which createIssueInTx needs for the parent's
//     visibility check (children need none — their parent is the epic
//     inserted in this very transaction, in this very project).
//
// epicInput.ParentID is OPTIONAL and honoured: nil leaves the epic at the
// project root (the original behaviour, and still what every caller that
// omits the field gets), a set value hangs the epic under that issue with
// a parent-child dep edge and a `<parent>.N` id. It used to be forced to
// nil here, which is why a caller wanting the node anywhere else had to
// follow up with a separate Reparent.
//
// Each child INHERITS the epic's project; the caller sets ParentID on
// the child input so Step 3a writes a parent-child dep edge to the
// freshly-allocated epic id. The legacy issues.parent_id column stays
// NULL (frozen per sk-35). Children inherit `source` from the epic so
// the audit trail is uniform across the decomposition.
func (s *Store) CreateEpicWithChildren(
	ctx context.Context,
	projectID uuid.UUID,
	idPrefix string,
	defaultIDMode string,
	epicInput CreateIssueInput,
	childTemplates []CreateIssueInput,
	source string,
	creatorID uuid.UUID,
	actor Actor,
) (*IssueDetail, []*IssueDetail, error) {
	if source == "" {
		source = SourceHuman
	}
	// Force epic semantics — a non-epic type here would be a programming
	// error in the caller. We refuse rather than silently coerce so the
	// tool layer surfaces an internal_error if it regresses.
	if epicInput.Type != TypeEpic {
		return nil, nil, fmt.Errorf("CreateEpicWithChildren: epicInput.Type must be %q, got %q",
			TypeEpic, epicInput.Type)
	}
	// epicInput.ParentID is HONOURED, not cleared. It used to be forced to
	// nil here, which is why skein_epic_create could only land a node at
	// the project root and every caller that wanted it elsewhere had to
	// follow up with a separate Reparent — a second call that could be
	// lost between the two, leaving a populated node invisible to every
	// parentId-scoped listing. The parent is validated by createIssueInTx
	// on the same path CreateIssue uses (existence, not-closed,
	// visibility, nested-standing), so an ineligible parent aborts the
	// transaction before the epic exists.

	// sk-303.17: replay on a class-40 abort — the same defect shape
	// sk-305 fixed for CreateIssue, and the same rationale for a larger-
	// than-claim budget: a ROOT epic (the caller passed no ParentID)
	// claims its id off project_counters via nextCounterID, the SAME row
	// every OTHER top-level create in this project (including plain
	// CreateIssue) advances. That is structural contention, not
	// incidental — and it is exactly what a caller sidesteps by passing
	// ParentID: a child id is allocated from the parent's own subtree, so
	// concurrent epics under DIFFERENT parents no longer meet on that row — measured at 7/8 losses with ZERO retries on an 8-way
	// concurrent CreateEpicWithChildren in the same project
	// (TestCreateEpicWithChildren_ConcurrentEpics_NoSerializationError).
	// The closure only assigns to captured locals (epicID, childIDs —
	// childIDs is re-`make`'d fresh on every attempt), so a replay has no
	// side effects to double.
	var (
		epicID    string
		childIDs  []string
		committed []CommittedState
	)
	// sk-565 (P3): every input of the call, for the per-attempt creator
	// projection — one read of the creator's row for the epic and all
	// children together.
	callInputs := make([]*CreateIssueInput, 0, len(childTemplates)+1)
	callInputs = append(callInputs, &epicInput)
	childrenNeedVerdict := false
	for i := range childTemplates {
		callInputs = append(callInputs, &childTemplates[i])
		if childTemplates[i].AssignCreatorByDefault {
			childrenNeedVerdict = true
		}
	}
	err := s.withSerializableRetry(ctx, "createEpicWithChildren", epicCreateMaxAttempts, func(tx pgx.Tx) error {
		att := newCreateAttempt(creatorID, callInputs...)
		// sk-565 (P6): the epic hands its verdict to the children of THIS
		// attempt — computed from what the epic actually got, never
		// re-read from its column (an empty column does not say why).
		id, _, epicVerdict, ierr := s.createIssueInTx(ctx, tx, att, projectID, idPrefix, defaultIDMode,
			epicInput, source, creatorID, actor, nil, childrenNeedVerdict)
		if ierr != nil {
			return ierr
		}
		epicID = id

		childIDs = make([]string, 0, len(childTemplates))
		for i := range childTemplates {
			child := childTemplates[i]
			// Children always live under the epic. Inherit source so the
			// audit trail is uniform across the decomposition; the tool
			// layer can override per-child by setting Source explicitly.
			if child.Source == "" {
				child.Source = source
			}
			child.ParentID = &epicID
			// Children inherit the epic's componentIds via Step 1b of
			// createIssueInTx (componentsForTx reads back the just-
			// inserted epic row inside the same SERIALIZABLE tx). The
			// returned inheritedComponentIDs signal is discarded here
			// — it only matters for the per-call
			// IssueDetail.InheritedComponentIDs echo, which the epic-
			// create response shape does not surface.
			cid, _, _, cerr := s.createIssueInTx(ctx, tx, att, projectID, idPrefix, defaultIDMode,
				child, child.Source, creatorID, actor, &epicVerdict, false)
			if cerr != nil {
				return &ErrEpicChildFailed{Index: i, Err: cerr}
			}
			childIDs = append(childIDs, cid)
		}
		// sk-607: epic and children as this transaction leaves them, in
		// one query — the acknowledgement's source if the read-back after
		// the commit fails.
		st, serr := committedStatesInTx(ctx, tx, append([]string{epicID}, childIDs...)...)
		if serr != nil {
			return serr
		}
		committed = st
		return nil
	})
	if err != nil {
		// sk-303.17 review: exhausting epicCreateMaxAttempts against a
		// genuine class-40 abort used to leak a raw pgError, same as
		// CreateIssue's own sk-305 gap (not touched here — see sk-354).
		// A wrapped &ErrEpicChildFailed{} still resolves through:
		// isRetryableTxError unwraps it via errors.As to reach the
		// pgconn.PgError underneath. Neither the epic nor any child was
		// ever committed (INSERT, no existing row to blame a version on),
		// so ErrVersionConflict would be as dishonest here as it is for
		// AddComment — ErrSerializationRetry says what actually happened.
		return nil, nil, s.mapSerializationExhausted(ctx, err, "createEpicWithChildren", epicCreateMaxAttempts,
			"projectId", projectID.String())
	}

	afterCommit(ctx, "epic_create", epicID)
	// sk-607: everything is committed. Read back detached from the
	// caller's context, and hand back the committed states if even that
	// fails — a committed tree is never reported as a failed create. In the
	// production incident the context ended because skein-web's proxy gave
	// up and closed the connection; that caller gets no answer either way,
	// and what stops its retry duplicating the tree is the proxy's window
	// (sk-608) and the transaction finishing inside it (sk-609), not this.
	details, err := s.readBack(ctx, "epic_create", committed)
	if err != nil {
		return nil, nil, err
	}
	return details[0], details[1:], nil
}

// ErrEpicChildFailed wraps an error from one of the per-child inserts
// inside CreateEpicWithChildren. Carries the failing template index so
// the tool layer can surface `children[i] ...` to the agent.
type ErrEpicChildFailed struct {
	Index int
	Err   error
}

func (e *ErrEpicChildFailed) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("epic child %d failed", e.Index)
	}
	return fmt.Sprintf("epic child %d: %v", e.Index, e.Err)
}

func (e *ErrEpicChildFailed) Unwrap() error { return e.Err }

// parentRow is the minimal projection of an issue used by parent-resolution
// paths (sk-35: cross-project allowed via fetchParentCrossProject).
type parentRow struct {
	ID        string
	ProjectID uuid.UUID
	Status    string
}

// fetchParentCrossProject is the sk-35 variant of fetchParent that
// allows the parent to belong to a different project from the new
// child. The child still inherits the parent's id prefix (so the new
// id is parent.N) but its `project_id` column points to the caller's
// current project. Visibility is enforced by the caller via
// projects.EnsureProjectVisible — this helper only reads the row.
func (s *Store) fetchParentCrossProject(ctx context.Context, tx pgx.Tx, parentID string) (*parentRow, error) {
	var p parentRow
	err := tx.QueryRow(ctx, `SELECT id, project_id, status FROM issues WHERE id = $1`, parentID).
		Scan(&p.ID, &p.ProjectID, &p.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, &ErrParentNotEligible{Code: "parent_not_found", Message: "parent issue does not exist"}
	}
	if err != nil {
		return nil, fmt.Errorf("loading parent: %w", err)
	}
	return &p, nil
}

// assertComponentsInProject verifies that every supplied component_id is
// in the issue's project. We do one COUNT round-trip rather than N×SELECT
// because component lists are typically tiny (1..3) but the validation
// must be 100% correct.
func assertComponentsInProject(ctx context.Context, tx pgx.Tx, projectID uuid.UUID, componentIDs []uuid.UUID) error {
	if len(componentIDs) == 0 {
		return nil
	}

	// Fetch valid IDs and diff against the input set.
	rows, err := tx.Query(ctx, `
SELECT id FROM components
 WHERE project_id = $1 AND id = ANY($2)`, projectID, componentIDs)
	if err != nil {
		return fmt.Errorf("checking component project: %w", err)
	}
	defer rows.Close()

	valid := make(map[uuid.UUID]struct{}, len(componentIDs))
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return err
		}
		valid[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	bad := make([]string, 0)
	for _, cid := range componentIDs {
		if _, ok := valid[cid]; !ok {
			bad = append(bad, cid.String())
		}
	}
	if len(bad) > 0 {
		return &ErrComponentsWrongProject{ComponentIDs: bad}
	}
	return nil
}

// ----------------- Get / List -----------------

// issueRowSelect is the SELECT clause shared by GetIssue and the cursor
// list. Keeping it as a single string avoids drift between the two paths.
//
// R1.6 polish (F258): we LEFT JOIN users twice — once on i.created_by to
// surface the creator's username, once on a case-insensitive
// LOWER(i.assignee) = LOWER(u.username) match to expose the assignee's
// uuid when the assignee string actually maps to a real user. Both
// joins are LEFT — older rows or deleted users surface as NULL, the
// scan path treats them as optional fields.
//
// Phase 1 ui-redesign: a third LEFT JOIN on i.accepted_by surfaces the
// accepter's username so IssueDetail.AcceptedByUsername is populated in
// one round-trip.
var issueRowSelect = `
SELECT i.id, i.project_id, ` + parentIDCurrentExpr + `, i.version, i.title, i.description,
       i.type, i.priority, i.status, i.pinned, i.source, i.source_context,
       i.assignee, i.locked_by, i.locked_at, i.design, i.acceptance_criteria,
       i.notes, i.estimated_minutes, i.due_at, i.defer_until,
       i.started_at, i.closed_at, i.closed_reason, i.close_outcome, i.external_ref,
       i.created_by, i.created_at, i.updated_at, i.row_seq,
       ` + rowComponentsLabelsCols + `,
       i.metadata,
       uc.username AS created_by_username,
       ua.id       AS assignee_user_id,
       ` + person.AssigneeStatusExpr("i") + ` AS assignee_status,
       ` + person.StatusInProjectExpr("uc", "i.project_id") + ` AS created_by_status,
       ` + issueSpaceCol + ` AS space_short,
       i.accepted_at, i.accepted_by,
       uacc.username AS accepted_by_username,
       -- sk-421.1 T001 (decision 1 of the plan, INV-11): the "blocked" flag
       -- is deliberately NOT in this projection. On the detail it is derived in
       -- hydrateIssueDetail from the already assembled blockers list, so that the flag
       -- and the list come from one read of the graph, not from two.
       i.previous_assignees, i.dismissed_by,
       (
         SELECT COALESCE(array_agg(COALESCE(u.username, '') ORDER BY arr.ord), '{}')
           FROM unnest(i.previous_assignees) WITH ORDINALITY AS arr(uid, ord)
           LEFT JOIN users u ON u.id = arr.uid
       ) AS previous_assignee_usernames,
       (
         SELECT COALESCE(array_agg(COALESCE(u.username, '') ORDER BY arr.ord), '{}')
           FROM unnest(i.dismissed_by) WITH ORDINALITY AS arr(uid, ord)
           LEFT JOIN users u ON u.id = arr.uid
       ) AS dismissed_by_usernames,
       -- sk-303.14 C1: the author's private bookkeeping. Always read,
       -- but lands on the card only through applyReviewVisibility —
       -- see review_visibility.go. Scanned into unexported fields
       -- the serialiser cannot see.
       i.review_required, i.reviewed_at, i.reviewed_by
`

// rowComponentsLabelsCols projects the component ids and the labels of row
// `i` as two arrays, aggregated in the SAME statement as `i.row_seq`
// (sk-622.21, decision R26 item 2). Every REST read that hands out a row's
// rowSeq — the card (issueRowSelect), the project list, the cross-project
// list and its search paths, the /me lists and the tree rows — takes the
// labels and components from here, so the answer's labels and components
// are exactly the state its rowSeq names. A separate statement after the
// row read would let a label commit land in between: the answer would then
// show labels newer than its rowSeq, and an earlier event row with a
// greater rowSeq would pass the client's strict-greater rule and roll those
// labels back (US-15).
//
// Order and empty shape match the neutral event row (neutral_row.go#
// neutralRowsSQL): component id / label ascending, '{}' when none. The
// correlated subqueries cost an index seek on the issue_labels /
// issue_components primary key per OUTPUT row and replace the two batched
// side queries a page used to send.
const rowComponentsLabelsCols = `COALESCE((SELECT array_agg(ic.component_id ORDER BY ic.component_id)
                   FROM issue_components ic WHERE ic.issue_id = i.id), '{}') AS component_ids,
       COALESCE((SELECT array_agg(il.label ORDER BY il.label)
                   FROM issue_labels il WHERE il.issue_id = i.id), '{}') AS labels`

// normalizeComponentsLabels keeps the wire shape `[]` (never null) for a
// row whose aggregated arrays came back empty.
func normalizeComponentsLabels(components *[]uuid.UUID, labels *[]string) {
	if *components == nil {
		*components = []uuid.UUID{}
	}
	if *labels == nil {
		*labels = []string{}
	}
}

// parentIDCurrentExpr is the canonical sk-35 fragment that derives an
// issue's parent from the dependencies graph. Picks the first
// parent-child edge by created_at to pin tree views on a single
// representative parent when an issue carries multiple parent edges
// (allowed — no UNIQUE on type='parent-child'). Returns NULL when the
// issue has no parent.
//
// Why a correlated subquery rather than a LEFT JOIN: callers that
// project this fragment also project i.* alongside it; adding another
// JOIN per issue would force GROUP BY i.id on every list query. The
// subquery returns at most one row by virtue of LIMIT 1, costs an
// index seek on (from_id, type) per row, and keeps the surrounding
// queries free of grouping.
const parentIDCurrentExpr = `(
    SELECT d.to_id FROM dependencies d
     WHERE d.from_id = i.id AND d.type = 'parent-child'
     ORDER BY d.created_at LIMIT 1
)`

// parentIDForReaderExpr is parentIDCurrentExpr as a reader is told it: the
// same canonical parent (first parent-child edge by created_at), or NULL
// when that parent lives in a project outside the reader's horizon — the
// id of an issue the reader cannot see is not on the wire (sk-674.18 T009;
// INV-28 (4) allows only counts). A hidden canonical parent is not
// replaced by a later visible one: the canonical parent is a fact about
// the issue, and the reader is told only that it is not theirs to see.
// The placeholders bind authz.Scope.Args.
func parentIDForReaderExpr(unrestrictedPlaceholder, callerPlaceholder string) string {
	return `(
    SELECT CASE WHEN ` + authz.ProjectVisibleExpr("par", unrestrictedPlaceholder, callerPlaceholder) + `
                THEN d.to_id END
      FROM dependencies d
      JOIN issues par ON par.id = d.to_id
     WHERE d.from_id = i.id AND d.type = 'parent-child'
     ORDER BY d.created_at LIMIT 1
)`
}

// readerHorizon is the reader's visibility, bound once for the statements
// one store call builds: the unrestricted flag and the visible set of the
// access relation (authz.Scope.Bind).
type readerHorizon struct {
	unrestricted bool
	visible      []uuid.UUID
	// spaces is the reader's own non-personal spaces (Scope.BindSpaces):
	// hidden issues count in unavailable* numbers only there (INV-28 (4)).
	spaces []uuid.UUID
}

// horizon binds the request context's scope (authz.FromContext) — one read
// of the relation, and of the reader's own spaces, per request.
func (s *Store) horizon(ctx context.Context) (readerHorizon, error) {
	scope := authz.FromContext(ctx)
	unrestricted, visible, err := scope.Bind(ctx, s.pool)
	if err != nil {
		return readerHorizon{}, err
	}
	spaces, err := scope.BindSpaces(ctx, s.pool)
	return readerHorizon{unrestricted: unrestricted, visible: visible, spaces: spaces}, err
}

// parentIDForReader binds the reader's horizon through addParam and returns
// parentIDForReaderExpr.
func parentIDForReader(h readerHorizon, addParam func(any) string) string {
	return parentIDForReaderExpr(addParam(h.unrestricted), addParam(h.visible))
}

// isBlockedExprFor renders the SELECT-time predicate that derives an issue's
// blocked-ness from the dependency graph (sk-31) FOR THE READER (sk-423):
// true when at least one outgoing blocks / waits-for edge of the row i has a
// target that gates readiness (status.GatesReadiness — a closed or standing
// target never blocks) AND lives in a project inside the reader's horizon.
// The two placeholders bind authz.Scope.Bind of the request context.
//
// Scope: list, search and tree ROWS project and filter on this expression
// (bind it with readerBlockedBinder). The issue DETAIL does not — there
// IsBlocked is derived from the assembled blockers list (blockersFor, see
// hydrateIssueDetail), so the flag and the list on one card cannot come from
// two reads of the graph. Both use readerBlockerPredicate, so the value is
// the same.
//
// Direction mirrors dependencies/types.go semantics (from = blocked, to =
// blocker). Readiness (skein_ready, dependencies.Store.ListReady) uses the
// same edge types and status predicate WITHOUT the horizon leg: readiness
// counts every blocker, the flag only the ones the reader may be told about.
func isBlockedExprFor(unrestrictedPlaceholder, callerPlaceholder string) string {
	return authz.ReaderBlockedExpr("i", unrestrictedPlaceholder, callerPlaceholder)
}

// readerBlockedBinder returns a renderer of isBlockedExprFor that binds the
// reader's horizon (readerHorizon) through addParam on its FIRST call and
// returns the same fragment on every later one.
//
// Lazy on purpose: list paths snapshot their parameters for the COUNT query
// before adding the projection's. A filter clause renders the fragment
// before the snapshot, so the count query carries its two bind values; a
// query without the filter renders it only for the projection, after the
// snapshot, so the count query does not carry two bind values it never
// references (PostgreSQL refuses a parameter whose type it cannot infer).
func readerBlockedBinder(h readerHorizon, addParam func(any) string) func() string {
	var expr string
	return func() string {
		if expr == "" {
			expr = isBlockedExprFor(addParam(h.unrestricted), addParam(h.visible))
		}
		return expr
	}
}

// readerBlockerPredicate is THE rule of which edge makes its target a
// blocker of its source for the reader (sk-421.2.8 T003, INV-5, R-4;
// sk-423): an outgoing blocks / waits-for edge whose target gates readiness
// and lives inside the reader's horizon — authz.ReaderBlockerExpr. dep
// is the dependencies alias, blocker the alias of the issues row on the
// edge's to_id side; the placeholders bind authz.Scope.Args.
//
// One fragment for every surface that answers "who blocks this issue" — the
// detail's list (blockersFor), the rows' flag (isBlockedExprFor), the tree
// node's list (treeBlockersFor) and the epic rollup's blockedChildren — so
// that the set of blockers cannot drift between them. The project and
// component counters use the same visibility fragment. Readiness does not.
func readerBlockerPredicate(dep, blocker, unrestrictedPlaceholder, callerPlaceholder string) string {
	return authz.ReaderBlockerExpr(dep, blocker, unrestrictedPlaceholder, callerPlaceholder)
}

// hiddenBlockingLinkPredicate is the companion of readerBlockerPredicate
// (sk-423): an outgoing blocks / waits-for edge whose target lives OUTSIDE
// the reader's horizon, in one of the reader's own non-personal spaces
// (INV-28 (4)), whatever that target's status. Such an edge never
// counts towards «blocked»; the detail and the tree node publish how many
// there are (UnavailableBlockingLinks) — the fact of the link, which is
// status-independent by construction.
func hiddenBlockingLinkPredicate(dep, blocker, unrestrictedPlaceholder, visiblePlaceholder, spacesPlaceholder string) string {
	return authz.HiddenBlockingLinkExpr(dep, blocker, unrestrictedPlaceholder, visiblePlaceholder, spacesPlaceholder)
}

// sortBlockerEntries is the order of blocker entries (sk-421.2.8 T003, R-4):
// by the blocker's id under the id-order rule of sk-421.2 US-6
// (idorder.Compare), so `x.2` precedes `x.10` (sk-426; it was the database's
// string order before). The detail (blockersFor), the tree node
// (treeBlockersFor) and the skein_close open-child warning
// (EpicOpenChildrenFor) all order their whole list by it — since sk-423 every
// entry is visible, so the tree's former split «redacted after visible» has
// nothing left to move. Changing the order here changes every surface at
// once; do not add an ORDER BY to those queries — it would be a second rule.
//
// Sorted in Go after the fetch, not in SQL: see package idorder. Stable, so
// two entries for one blocker (a blocks and a waits-for edge to the same
// issue) — identical on the wire — stay adjacent.
func sortBlockerEntries[E any](entries []E, id func(E) string) {
	slices.SortStableFunc(entries, func(a, b E) int { return idorder.Compare(id(a), id(b)) })
}

// epicSummaryTotalExpr / epicSummaryClosedExpr are the per-row
// projections that surface IssueSummary.EpicProgress on list paths.
// CASE WHEN gates the correlated subquery so non-epic rows pay zero
// cost (the planner short-circuits CASE when the THEN branch wouldn't
// be reached).
//
// sk-35: children sourced from the dependencies graph (parent-child
// edges where to_id = epic.id). The issues.parent_id column is frozen
// — no new rows write it, so reading it would silently drop children
// created post-sk-35.
//
// Scan target on the caller side is *int (sql.NullInt64-style nullability
// via pgx) so the NULL → nil → omitempty chain stays clean.
// F2h (sk-118.9, R11): the Total leg JOINs issues and excludes
// `standing` children so a perpetual standing obligation does not
// inflate the inline (closed/total) progress chip on list/tree rows.
// The closed leg below is untouched — standing is never closed, so it
// already drops out of Closed naturally; only the denominator needed
// fixing. A standing child must be INVISIBLE to epic rollup (§11.5).
// sk-291: the three legs are parameterised by the caller's horizon because
// parent-child is allowed to cross projects (sk-35), so an epic's children
// can live where the caller has no access. Counting those in the status
// legs published the status distribution of tasks the caller cannot open —
// the exact thing the dependency list refuses to do next door, where an
// invisible edge keeps the link but blanks the card and sets redacted.
//
// The split follows sk-282's counters rather than inventing a second
// answer: an invisible child is NOT dropped (a vanishing cross-project tail
// reads as "the group is finished" when it is not) and NOT bucketed by
// status. It lands in its own `unavailable` leg.
//
// TOTAL keeps counting invisible children, which is what makes the pair
// honest: closed can then never equal total while an invisible child
// exists, so the progress chip reads "cannot say" instead of "done". It is
// also why the standing exclusion applies only to VISIBLE children —
// excluding invisible standing ones would let a reader recover their status
// by subtracting unavailable from total.
//
// sk-674.18 T017, INV-28 (4): «invisible» here is HIDDEN-COUNTABLE (hid) —
// hidden AND in one of the reader's own non-personal spaces. A hidden child
// in another's space or another's "Personal" is not counted in any leg.
func epicSummaryTotalExpr(vis, hid string) string {
	return `CASE WHEN i.type = 'epic' THEN (
    SELECT COUNT(*) FROM dependencies d
       JOIN issues c ON c.id = d.from_id
     WHERE d.to_id = i.id AND d.type = 'parent-child'
       AND ((` + vis + ` AND c.status <> 'standing') OR ` + hid + `)
) ELSE NULL END`
}

func epicSummaryClosedExpr(vis string) string {
	return `CASE WHEN i.type = 'epic' THEN (
    SELECT COUNT(*) FROM dependencies d
       JOIN issues c ON c.id = d.from_id
     WHERE d.to_id = i.id AND d.type = 'parent-child'
       AND ` + vis + ` AND c.status = 'closed'
) ELSE NULL END`
}

func epicSummaryUnavailableExpr(hid string) string {
	return `CASE WHEN i.type = 'epic' THEN (
    SELECT COUNT(*) FROM dependencies d
       JOIN issues c ON c.id = d.from_id
     WHERE d.to_id = i.id AND d.type = 'parent-child'
       AND ` + hid + `
) ELSE NULL END`
}

// epicSummaryNoWorkExpr is the fourth leg (sk-489, T005): how many of the
// row's VISIBLE children are closed WITHOUT work — closed with an outcome
// other than "done". It is the leg every progress number on every row
// surface is built from: Done = Closed - NoWork, Counted = Total - NoWork
// (INV-5, "denominator = total - visible dropped-out ones").
//
// Spelled as the CLOSED leg plus one predicate, deliberately: NoWork is a
// subset of Closed, so the two must agree on «visible» and on «closed» by
// construction rather than by two independent spellings that a later edit
// could drift apart. An invisible child is therefore outside this leg at
// ANY outcome, which is exactly INV-4 — its outcome is card content and
// cannot be recovered from a number (R-6).
//
// ⚠ THE SAME RULE IS WRITTEN A SECOND TIME IN Go — `close_outcome.go#IsNoWorkOutcome`,
// as a WHITELIST (three values) against the blacklist here ("anything but `done`"). That
// half does not take part in the count: the numbers are counted by this leg and its twins, and
// the Go predicate is not called by production code at all — it is the normative definition of the rule
// and a reference point. Today the wordings coincide, because there are exactly
// four values; a fifth will diverge: SQL will count it as dropped out, Go will not. The divergence is
// observable — the expectation of the per-read-site table
// (`close_outcome_progress_test.go#wantRowProgress`) is computed by the Go half, and
// the fact comes from here. Add a value — fix BOTH halves.
//
// NULL is read as "NOT without work" (`<> 'done'` on NULL gives NULL, and FILTER
// does not take the row) — the same answer as `IsNoWorkOutcome("")` in Go. Under
// INV-1 a closed row without an outcome is impossible (CHECK and trigger 000050), but
// if one appeared, both halves would answer about it identically rather than diverge
// on an impossible input.
func epicSummaryNoWorkExpr(vis string) string {
	return `CASE WHEN i.type = 'epic' THEN (
    SELECT COUNT(*) FROM dependencies d
       JOIN issues c ON c.id = d.from_id
     WHERE d.to_id = i.id AND d.type = 'parent-child'
       AND ` + vis + ` AND c.status = 'closed' AND c.close_outcome <> 'done'
) ELSE NULL END`
}

// epicProgressProjection renders the four epic-progress columns every
// list/tree SELECT carries, bound to the caller's horizon.
//
// One renderer for six call sites on purpose: the visible/invisible split
// is a policy, and a policy spliced by hand into six queries is a policy
// with six chances to be spelled differently. (Six: ListIssues, the three
// list_all builders, ListByUser, summariesForIDs. The four Scan sites that
// read them go through epicProgressFrom below.) The alias is "c" — every
// caller joins the child row as `JOIN issues c ON c.id = d.from_id`.
//
// sk-489 T005 — the fourth column (`epic_no_work`) is what makes the
// compiler the guard for INV-5: this renderer and epicProgressFrom below
// are the only way a list row gets its progress, and a call site that did
// not learn the new leg does not compile (the scan list is one shorter than
// the projection). That is why the leg was added HERE and not beside each
// query.
//
// Bind the first two placeholders from authz.Scope.Bind in this order, the
// third from authz.Scope.BindSpaces (INV-28 (4)).
func epicProgressProjection(unrestrictedPlaceholder, visiblePlaceholder, spacesPlaceholder string) string {
	vis := authz.ProjectVisibleExpr("c", unrestrictedPlaceholder, visiblePlaceholder)
	hid := authz.HiddenCountableExpr("c.project_id", unrestrictedPlaceholder, visiblePlaceholder, spacesPlaceholder)
	return epicSummaryTotalExpr(vis, hid) + ` AS epic_total,
       ` + epicSummaryClosedExpr(vis) + ` AS epic_closed,
       ` + epicSummaryUnavailableExpr(hid) + ` AS epic_unavailable,
       ` + epicSummaryNoWorkExpr(vis) + ` AS epic_no_work`
}

// epicProgressFrom assembles the scanned quadruple. All four legs are NULL
// together (the CASE returns NULL for non-epic rows), so a nil total means
// "not an epic" rather than "an epic with no children".
//
// sk-489 T005 — Done and Counted are ARITHMETIC over the scanned legs, not
// legs of their own, and that is the point: one subtraction in one place
// cannot disagree with itself, whereas a fifth and sixth SQL leg would have
// to re-spell «visible» and «closed» twice more. Closed keeps its old
// meaning (closed by status, INV-3); Done is the part of it that was
// actually done, and Counted is Total minus the visible children that
// dropped out. Invisible children stay in Total and therefore in Counted —
// «cannot say» is not «dropped out» (INV-4).
func epicProgressFrom(total, closed, unavailable, noWork *int) *EpicProgress {
	if total == nil || closed == nil {
		return nil
	}
	p := &EpicProgress{Closed: *closed, Total: *total}
	if unavailable != nil {
		p.Unavailable = *unavailable
	}
	if noWork != nil {
		p.NoWork = *noWork
	}
	p.Done = p.Closed - p.NoWork
	p.Counted = p.Total - p.NoWork
	return p
}

// epicChildrenSQL is the canonical CTE fragment that materialises an
// epic's hierarchical children (parent-child edges where to_id =
// $epic.id). Reused by IssueDetail.epic rollup and skein_close
// open-children warning. Returns (id, status) per row.
//
// $1 is the epic's id. Caller embeds the CTE around it with a SELECT
// against `children` — see Store.epicRollupFor for the canonical usage.
// Returns (id, status, project_id, close_outcome) per row; sk-489 added the
// last one so the rollup can count progress by OUTCOME in the same single
// round-trip rather than joining `issues` a second time.
//
// sk-35: the children set is derived from the dependencies graph
// (canonical) — the legacy issues.parent_id column is frozen and would
// miss every child created post-restore. Pre-sk-33 used the same
// graph; sk-33 switched to parent_id; sk-35 reverts to the graph.
const epicChildrenSQL = `
WITH children AS (
    SELECT c.id, c.status, c.project_id, c.close_outcome FROM dependencies d
       JOIN issues c ON c.id = d.from_id
     WHERE d.to_id = $1 AND d.type = 'parent-child'
)`

// issueSummaryPersonCols are the person marks of a summary row — the
// assignee's and the author's, in the issue's space (sk-674.18 T027) —
// placed right after `ua.id` in every summary projection. The assignee's
// follow INV-30's display grounds (person.AssigneeStatusExpr, T028), not
// the delivery join `ua`, which never holds a person who left.
var issueSummaryPersonCols = person.AssigneeStatusExpr("i") + ", " +
	person.StatusInProjectExpr("uc", "i.project_id") + ", " + issueSpaceCol

// issueSpaceCol is the short id of the space of the issue's PROJECT
// (sk-674.18 T032) — never derived from the id prefix: a subtask may carry
// its parent's prefix from another project.
const issueSpaceCol = `(SELECT sp_i.short_id FROM projects pj_i JOIN spaces sp_i ON sp_i.id = pj_i.space_id
   WHERE pj_i.id = i.project_id)`

// issueRowJoins is the FROM/JOIN tail used by GetIssue. ListIssues uses
// the lighter projection in its own query but still needs the same joins
// to populate IssueSummary's createdByUsername / assigneeUserId.
//
// It is a var rather than a const because the assignee leg is built by
// assignee.LeftJoin: the three handle forms live in one place (sk-523) and a
// function call is not a constant expression. Nothing mutates it.
var issueRowJoins = `
  FROM issues i
  LEFT JOIN users uc   ON uc.id   = i.created_by
  ` + assignee.LeftJoin("ua", "i") + `
  LEFT JOIN users uacc ON uacc.id = i.accepted_by
`

// GetIssueVersion reads just the optimistic-concurrency version.
//
// One query, where GetIssue is five on a non-epic: the issue row (with its
// components and labels aggregated in, R26 item 2) plus the hydration of
// comment count, commit refs, blockers and mentions (an epic pays a sixth
// for its rollup). The batch update path needs the version and nothing
// else — every other field it would load is dropped on the floor, and on a
// hundred-row call that is four hundred round-trips of pure waiting.
//
// Returns ErrNotFound for an unknown id, same as GetIssue, so callers keep
// one not-found branch.
func (s *Store) GetIssueVersion(ctx context.Context, id string) (int, error) {
	var version int
	err := s.pool.QueryRow(ctx, `SELECT version FROM issues WHERE id = $1`, id).Scan(&version)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("reading version of issue %s: %w", id, err)
	}
	return version, nil
}

// GetIssueProjectID reads just the owning project.
//
// Same reasoning as GetIssueVersion: the per-row visibility check in a
// batch needs the project and nothing else, and paying seven round-trips
// to learn one uuid is the bulk of what the batch loop spends. Returns
// ErrNotFound for an unknown id.
func (s *Store) GetIssueProjectID(ctx context.Context, id string) (uuid.UUID, error) {
	var projectID uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT project_id FROM issues WHERE id = $1`, id).Scan(&projectID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, ErrNotFound
		}
		return uuid.Nil, fmt.Errorf("reading project of issue %s: %w", id, err)
	}
	return projectID, nil
}

// GetIssue returns the full IssueDetail for one row, with components,
// labels, and the comments-count aggregate.
func (s *Store) GetIssue(ctx context.Context, id string) (*IssueDetail, error) {
	row := s.pool.QueryRow(ctx, issueRowSelect+issueRowJoins+`
 WHERE i.id = $1`, id)
	dto, err := scanIssueDetail(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("getting issue: %w", err)
	}
	if err := s.hydrateIssueDetail(ctx, dto); err != nil {
		return nil, err
	}
	return dto, nil
}

// IssueBelongsToProject reports whether `issueID` exists AND lives in
// `projectID`. It returns nil when the pair matches and ErrNotFound
// otherwise — deliberately collapsing "no such issue" and "issue exists but
// in a different project" into ONE indistinguishable outcome.
//
// # Why this exists (sk-194.6.50 / sk-194.6.51)
//
// Issues are addressed as /projects/{projectID}/issues/{issueID}. The
// project segment is what every handler runs its ACL against
// (projects.EnsureProjectVisible), but the issue id is globally unique and
// carries its OWN project. Nothing in the path forces the two to agree, so
// a caller who can see project A could name an issue of project B and have
// the mutation applied to B — the project ACL passed, and it was never the
// right project's ACL. That is Broken Object Level Authorization: the
// object-level check (does THIS object belong to the parent named in the
// URL) was missing even though the parent-level check passed.
//
// # Why the scoped predicate, and not "load the issue and compare"
//
// This is the shape OWASP's IDOR Prevention Cheat Sheet prescribes —
// "when looking up objects based on primary keys, use datasets that users
// have access to" — and the Multi-Tenant Security Cheat Sheet's composite
// key (tenant + resource) rule. Beyond doctrine, it cannot be got wrong at
// the call site: there is no project id in the return value for a caller to
// forget to compare, and no issue payload that could be logged or leaked on
// the failure path. It is also cheap — a primary-key lookup returning a
// constant, against the full IssueDetail hydration GetIssue performs (three
// user joins plus components, labels and the comment-count aggregate) —
// which matters now that it runs on every issue-scoped request.
//
// # Why a separate read is not a TOCTOU hole here
//
// The general objection to check-then-act (CWE-367) is that the row you
// authorized may not be the row you write. It does not apply to this
// predicate: issues.project_id is write-once. It is set by the INSERT in
// Store.Create and no statement anywhere in the codebase or in the
// migrations ever updates it — Skein has no in-place project move at all
// (a move is copy + supersedes + close, by design, because the hierarchical
// id is referenced from git messages and chat logs we do not control). The
// pair therefore cannot change under the caller between this read and the
// write. Should an in-place move ever be introduced, this comment stops
// being true and the predicate has to move into the mutating statements
// themselves (WHERE id = $1 AND project_id = $2, rowsAffected == 0 => 404).
func (s *Store) IssueBelongsToProject(ctx context.Context, issueID string, projectID uuid.UUID) error {
	var one int
	err := s.pool.QueryRow(ctx, `
SELECT 1 FROM issues WHERE id = $1 AND project_id = $2`, issueID, projectID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("checking issue project membership: %w", err)
	}
	return nil
}

// UnclosedWorkIDsInProject returns, in caller order and without repeats, the
// ids from `ids` that belong to project `projectID` and are neither closed
// nor standing — the rows an archive without an explicit outcome may not
// touch (sk-540.4). Ids of other projects and unknown ids are simply absent
// from the answer: the query is scoped to the authorised project, so the
// caller learns nothing about issues it may not see (same scoping as
// IssueBelongsToProject).
func (s *Store) UnclosedWorkIDsInProject(ctx context.Context, projectID uuid.UUID, ids []string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
SELECT id FROM issues
 WHERE project_id = $1 AND id = ANY($2) AND status NOT IN ($3, $4)`,
		projectID, ids, StatusClosed, StatusStanding)
	if err != nil {
		return nil, fmt.Errorf("reading unclosed issues of the batch: %w", err)
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("scanning unclosed issues of the batch: %w", err)
	}
	if len(found) == 0 {
		return nil, nil
	}
	isUnclosed := make(map[string]bool, len(found))
	for _, id := range found {
		isUnclosed[id] = true
	}
	out := make([]string, 0, len(found))
	for _, id := range ids {
		if isUnclosed[id] {
			out = append(out, id)
			delete(isUnclosed, id)
		}
	}
	return out, nil
}

// getIssuesByIDs loads the primary IssueDetail rows in one query and returns
// them in caller order. Detail hydration remains identical to GetIssue, so
// the batch and single claim wire contracts cannot drift.
func (s *Store) getIssuesByIDs(ctx context.Context, ids []string) ([]*IssueDetail, error) {
	rows, err := s.pool.Query(ctx, issueRowSelect+issueRowJoins+`
 WHERE i.id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("getting issues batch: %w", err)
	}
	defer rows.Close()

	byID := make(map[string]*IssueDetail, len(ids))
	for rows.Next() {
		dto, err := scanIssueDetail(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning issue batch: %w", err)
		}
		byID[dto.ID] = dto
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading issue batch: %w", err)
	}

	result := make([]*IssueDetail, 0, len(ids))
	for _, id := range ids {
		dto, ok := byID[id]
		if !ok {
			return nil, ErrNotFound
		}
		if err := s.hydrateIssueDetail(ctx, dto); err != nil {
			return nil, err
		}
		result = append(result, dto)
	}
	return result, nil
}

func scanIssueDetail(row rowScanner) (*IssueDetail, error) {
	var dto IssueDetail
	var (
		parentID           *string
		sourceContext      *string
		assignee           *string
		lockedBy           *string
		lockedAt           *time.Time
		estimated          *int
		dueAt              *time.Time
		deferUntil         *time.Time
		startedAt          *time.Time
		closedAt           *time.Time
		closedReason       *string
		closeOutcome       *string
		externalRef        *string
		metadataBytes      []byte
		createdByUsername  *string
		assigneeUserID     *uuid.UUID
		acceptedAt         *time.Time
		acceptedBy         *uuid.UUID
		acceptedByUsername *string
		reviewRequired     bool
		reviewedAt         *time.Time
		reviewedBy         *uuid.UUID
	)

	err := row.Scan(
		&dto.ID, &dto.ProjectID, &parentID, &dto.Version, &dto.Title, &dto.Description,
		&dto.Type, &dto.Priority, &dto.Status, &dto.Pinned, &dto.Source, &sourceContext,
		&assignee, &lockedBy, &lockedAt, &dto.Design, &dto.AcceptanceCriteria,
		&dto.Notes, &estimated, &dueAt, &deferUntil,
		&startedAt, &closedAt, &closedReason, &closeOutcome, &externalRef,
		&dto.CreatedBy, &dto.CreatedAt, &dto.UpdatedAt, &dto.RowSeq,
		&dto.ComponentIDs, &dto.Labels, &metadataBytes,
		&createdByUsername, &assigneeUserID,
		&dto.AssigneeStatus, &dto.CreatedByStatus, &dto.SpaceID,
		&acceptedAt, &acceptedBy, &acceptedByUsername,
		&dto.PreviousAssignees, &dto.DismissedBy,
		&dto.PreviousAssigneeUsernames, &dto.DismissedByUsernames,
		&reviewRequired, &reviewedAt, &reviewedBy,
	)
	if err != nil {
		return nil, err
	}

	normalizeComponentsLabels(&dto.ComponentIDs, &dto.Labels)
	dto.ParentID = parentID
	dto.SourceContext = sourceContext
	dto.Assignee = assignee
	dto.LockedBy = lockedBy
	dto.LockedAt = lockedAt
	dto.EstimatedMinutes = estimated
	dto.DueAt = dueAt
	dto.DeferUntil = deferUntil
	dto.StartedAt = startedAt
	dto.ClosedAt = closedAt
	dto.ClosedReason = closedReason
	// INV-1 repeated on the wire, not computed here: an unclosed issue has
	// NULL in the column (CHECK + trigger 000050), so the closeOutcome key
	// will not be on the card — without a single branch on the Go side.
	dto.CloseOutcome = closeOutcome
	dto.ExternalRef = externalRef
	dto.CreatedByUsername = createdByUsername
	dto.AssigneeUserID = assigneeUserID
	dto.AcceptedAt = acceptedAt
	dto.AcceptedBy = acceptedBy
	dto.AcceptedByUsername = acceptedByUsername
	// sk-303.14 C1: values go into unexported raw fields. They land
	// on the card only from applyReviewVisibility and only for the
	// issue author — exported ReviewRequired / ReviewedAt /
	// ReviewedBy are NOT touched here, so a card that missed the
	// gate is private, not leaked.
	dto.rawReviewRequired = reviewRequired
	dto.rawReviewedAt = reviewedAt
	dto.rawReviewedBy = reviewedBy
	dto.Metadata = map[string]any{}
	if len(metadataBytes) > 0 {
		_ = json.Unmarshal(metadataBytes, &dto.Metadata)
	}

	return &dto, nil
}

func (s *Store) hydrateIssueDetail(ctx context.Context, dto *IssueDetail) error {
	var err error
	// The canonical parent is on the card only when the reader sees it
	// (sk-674.18 T009, the rule of parentIDForReaderExpr): the row statement
	// is shared by every read of the card, so the reader's check is here,
	// on the one assembly path.
	if dto.ParentID != nil {
		visible, err := s.visibleIssueIDs(ctx, []string{*dto.ParentID}, authz.FromContext(ctx))
		if err != nil {
			return err
		}
		if _, ok := visible[*dto.ParentID]; !ok {
			dto.ParentID = nil
		}
	}
	// Components and labels are NOT read here: they come from the row
	// statement itself (issueRowSelect → rowComponentsLabelsCols), the same
	// snapshot as the card's rowSeq (R26 item 2). Side queries below hydrate
	// what rowSeq does not order: comment count, commits, blockers, lock
	// holder, epic rollup, mentions.
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM comments WHERE issue_id = $1`, dto.ID).
		Scan(&dto.CommentsCount); err != nil {
		return fmt.Errorf("counting comments: %w", err)
	}
	dto.CommitRefs, err = s.commitRefsFor(ctx, dto.ID, dto.ProjectID)
	if err != nil {
		return err
	}

	// sk-31: hydrate the blockers list to back the IsBlocked flag.
	// Always returned as a non-nil array; empty when not blocked.
	//
	// sk-421.1 T001: IsBlocked is derived from THIS list rather than from a
	// separate EXISTS in the row projection (INV-11 by construction: flag and
	// list come from one read of the graph).
	//
	// sk-423: the list holds only blockers inside the reader's horizon; an
	// edge to an issue outside it is counted into UnavailableBlockingLinks,
	// whatever that issue's status, by the same read (blockersFor).
	//
	// GUARD — INV-10 / INV-11 as sk-423 left them: the list is exactly the
	// set of blockers the flag counts, and the flag is the list's
	// non-emptiness. Who is counted is decided in one place —
	// readerBlockerPredicate — for the detail, the rows, the tree and the
	// epic rollup at once. Do not reintroduce an independent predicate for
	// the flag, and do not filter the list after this line.
	//
	// sk-421.2.8 (INV-5, FOR-PLAN m-7): this guard covers a SECOND surface —
	// the blockers list of an issue-tree node (treeBlockersFor in tree.go,
	// whose INV-2 guard repeats this one for the tree). Membership and the
	// flag change only in readerBlockerPredicate, the order only in
	// sortBlockerEntries — for the detail and the tree at once (sk-426). Do
	// not give the tree a predicate or an order of its own.
	dto.Blockers, dto.UnavailableBlockingLinks, err = s.blockersFor(ctx, dto.ID)
	if err != nil {
		return err
	}
	dto.IsBlocked = len(dto.Blockers) > 0

	// sk-421.1 T002 (US-15, INV-7): who performed the claim that holds the
	// lock right now. Only a locked issue pays the lookup; see
	// lockHolderUsernameFor for the matching rule and ADR-1.
	if dto.LockedBy != nil && *dto.LockedBy != "" && dto.LockedAt != nil {
		dto.LockedByUsername, err = s.lockHolderUsernameFor(ctx, dto.ID, *dto.LockedBy, *dto.LockedAt)
		if err != nil {
			return err
		}
	}

	// sk-32 slice 1: epic rollup. Only fired for epic-typed issues so
	// non-epic GetIssue calls pay zero cost. The nine counters share a
	// single CTE round-trip; EligibleForClose is derived in Go from
	// (Closed == Total AND Total > 0) so an empty epic never reads as
	// "ready to close".
	if dto.Type == TypeEpic {
		rollup, err := s.epicRollupFor(ctx, dto.ID)
		if err != nil {
			return err
		}
		dto.Epic = rollup
	}

	// R4.3 — hydrate @username mentions across the four scanned text
	// fields. Single SQL via HydrateMany (vs four). The frontend uses
	// this to substitute <UserMention> for resolved usernames in the
	// rendered body; unresolved `@foo` stays plain text.
	resolved, err := mentions.HydrateMany(ctx, s.pool, dto.ProjectID, dto.ID,
		dto.Description, dto.Notes, dto.AcceptanceCriteria, dto.Design)
	if err != nil {
		return fmt.Errorf("hydrating mentions: %w", err)
	}
	dto.Mentions = toMentionRefs(resolved)

	// sk-303.14 T006 (contract C1, INV-2): the only call of the
	// private review-fields gate. It sits here, not in the read
	// handler, because hydrateIssueDetail is the only point through
	// which a card leaves on ANY return: read, create, patch, claim,
	// close, accept, labels. Details and the fail-closed rationale
	// are in review_visibility.go.
	dto.applyReviewVisibility(ctx)

	return nil
}

// toMentionRefs converts the package-level mentions.ResolvedUser shape
// into the wire MentionRef shape. Always returns non-nil (`[]`) so JS
// clients don't have to special-case `null`.
func toMentionRefs(in []mentions.ResolvedUser) []MentionRef {
	out := make([]MentionRef, 0, len(in))
	for _, r := range in {
		out = append(out, MentionRef{Username: r.Username, UserID: r.UserID, Status: r.Status})
	}
	return out
}

// componentsForTx reads an issue's component ids inside tx — used by
// createIssueInTx's parent-component inheritance step so the read
// happens inside the same SERIALIZABLE transaction as the new issue
// insert. Going through s.pool would dodge the tx's isolation
// guarantees (a concurrent reassign of the parent's components would
// be invisible to s.pool but visible to tx); the trade-off is one
// extra method on the Store surface.
func (s *Store) componentsForTx(ctx context.Context, tx pgx.Tx, issueID string) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `
SELECT component_id FROM issue_components WHERE issue_id = $1 ORDER BY component_id`, issueID)
	if err != nil {
		return nil, fmt.Errorf("loading components: %w", err)
	}
	defer rows.Close()
	out := make([]uuid.UUID, 0)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// commitRefsFor loads issue_commits for the given issue and joins each
// row to projects.git_repos[] (LATERAL jsonb_array_elements) to surface
// the repo URL alongside the hash. linked_by maps to a username via
// LEFT JOIN users — system actors land with NULL.
//
// Sort: committed_at DESC NULLS LAST, linked_at DESC. Newer commits at
// the top; commits without a committed_at fall back to the order in
// which they were linked.
//
// projectID is supplied by the caller (we already loaded it for the
// IssueDetail) so we don't need a second projects lookup.
func (s *Store) commitRefsFor(ctx context.Context, issueID string, projectID uuid.UUID) ([]CommitRef, error) {
	rows, err := s.pool.Query(ctx, `
SELECT ic.hash,
       ic.repo_id,
       COALESCE(repo->>'url', '')           AS repo_url,
       ic.url,
       ic.message,
       ic.author,
       ic.committed_at,
       ic.linked_at,
       u.username                           AS linked_by_username,
       ic.linked_by_kind
  FROM issue_commits ic
  LEFT JOIN LATERAL (
       SELECT r AS repo
         FROM projects p,
              jsonb_array_elements(p.git_repos) AS r
        WHERE p.id = $2
          AND (r->>'id')::uuid = ic.repo_id
        LIMIT 1
  ) jr ON TRUE
  LEFT JOIN users u ON u.id = ic.linked_by
 WHERE ic.issue_id = $1
 ORDER BY ic.committed_at DESC NULLS LAST, ic.linked_at DESC`, issueID, projectID)
	if err != nil {
		return nil, fmt.Errorf("loading commit refs: %w", err)
	}
	defer rows.Close()

	out := make([]CommitRef, 0)
	for rows.Next() {
		var (
			ref         CommitRef
			repoURL     string
			urlPtr      *string
			messagePtr  *string
			authorPtr   *string
			committedAt *time.Time
			linkedBy    *string
		)
		if err := rows.Scan(
			&ref.Hash, &ref.RepoID, &repoURL,
			&urlPtr, &messagePtr, &authorPtr,
			&committedAt, &ref.LinkedAt,
			&linkedBy, &ref.LinkedByKind,
		); err != nil {
			return nil, fmt.Errorf("scanning commit ref: %w", err)
		}
		ref.RepoURL = repoURL
		ref.URL = urlPtr
		ref.Message = messagePtr
		ref.Author = authorPtr
		ref.CommittedAt = committedAt
		ref.LinkedBy = linkedBy
		out = append(out, ref)
	}
	return out, rows.Err()
}

// blockersFor returns, from ONE read of the issue's outgoing edges, the two
// halves of what the detail says about its blockers (sk-423):
//
//   - the slim list of issues currently blocking it FOR THE READER — gating
//     targets (status.GatesReadiness: neither closed nor standing) of
//     outgoing blocks / waits-for edges whose project is inside the reader's
//     horizon (readerBlockerPredicate). Every entry is the visible form
//     {id, title, status}. Always non-nil (empty when none);
//     sortBlockerEntries orders it by the id-order rule (sk-426);
//   - the number of outgoing blocks / waits-for edges to issues OUTSIDE the
//     horizon (hiddenBlockingLinkPredicate), WHATEVER their status.
//
// The detail's IsBlocked is the list's non-emptiness (hydrateIssueDetail).
//
// The horizon is the READER's, taken from the context by
// authz.FromContext — the same mechanism, and so the same horizon, as
// the epic rollup on this very card (epicRollupFor), including the admin
// ceiling for external collaborators already inside auth.Principal.IsAdmin.
// An undeclared context sees nothing: every edge is a hidden link and
// nothing blocks (fail-closed, as the rollup does).
//
// Why a hidden edge is a COUNT and not an entry: an entry that exists only
// while the hidden target gates would say «that invisible issue is not
// closed» (sk-421.1 kept it, redacted, and that was the defect sk-423
// fixes). The count includes closed and standing targets, so it states only
// that the link exists — what the dependency list on the same card already
// states for every edge.
//
// For hidden rows the query projects NULLs, so a hidden blocker's id, title
// and status never even reach this process. Cost is bounded by this one
// issue (NF-1): one index-driven scan over its outgoing edges plus, per
// target, a primary-key probe on project_members.
func (s *Store) blockersFor(ctx context.Context, issueID string) ([]BlockerRef, int, error) {
	scope := authz.FromContext(ctx)
	unrestricted, callerID, hErr := scope.Bind(ctx, s.pool)
	if hErr != nil {
		return nil, 0, hErr
	}
	ownSpaces, sErr := scope.BindSpaces(ctx, s.pool)
	if sErr != nil {
		return nil, 0, sErr
	}
	vis := authz.ProjectVisibleExpr("b", "$2", "$3")
	rows, err := s.pool.Query(ctx, `
SELECT CASE WHEN `+vis+` THEN b.id     END AS id,
       CASE WHEN `+vis+` THEN b.title  END AS title,
       CASE WHEN `+vis+` THEN b.status END AS status
  FROM dependencies d
  JOIN issues b ON b.id = d.to_id
 WHERE d.from_id = $1
   AND ((`+readerBlockerPredicate("d", "b", "$2", "$3")+`)
     OR (`+hiddenBlockingLinkPredicate("d", "b", "$2", "$3", "$4")+`))`,
		issueID, unrestricted, callerID, ownSpaces)
	if err != nil {
		return nil, 0, fmt.Errorf("loading blockers: %w", err)
	}
	defer rows.Close()
	out := make([]BlockerRef, 0)
	hidden := 0
	for rows.Next() {
		var id, title, st *string
		if err := rows.Scan(&id, &title, &st); err != nil {
			return nil, 0, fmt.Errorf("scanning blocker: %w", err)
		}
		// b.id and b.status are NOT NULL, so NULLs can only come from the
		// CASE: the edge's target is outside the reader's horizon.
		if id == nil || st == nil {
			hidden++
			continue
		}
		b := BlockerRef{ID: *id, Status: *st}
		if title != nil {
			b.Title = *title
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterating blockers: %w", err)
	}
	sortBlockerEntries(out, func(b BlockerRef) string { return b.ID })
	return out, hidden, nil
}

// lockHolderUsernameFor returns the username of the user who PERFORMED the
// claim that holds the issue's lock right now — sk-421.1 T002 (US-15,
// INV-7, DRAFT ADR-1) — or nil when no such record exists.
//
// The authoritative record is the issue.claimed event. The claim that holds
// the lock is the one whose event carries, in new_data.lockedBy, the session
// now in issues.locked_by, AND was written at the moment now in
// issues.locked_at. The latest such event wins (ORDER BY id DESC): a claim
// the server re-executes as new for the same session writes a newer event
// with its own moment, and only that one matches locked_at. The name is the
// event's actor user — whoever performed the claim (an admin claiming on
// behalf of someone else is the performer, not the assignee).
//
// What is deliberately NOT used:
//
//   - events.actor_session_id — that is the session of whoever ACTED, and a
//     later comment or edit sent with the holder's session carries it too;
//     matching on it would let any such request rename the holder. The
//     value the claim wrote into the lock is new_data.lockedBy.
//   - the MCP session registry (agents) — filled before the session is
//     checked, best-effort, and ambiguous when several users present one
//     session id (rejected in the spec, "Rejected alternatives").
//   - the assignee — not the performer.
//
// lockedBy / lockedAt are the values scanned into THIS response's row, not a
// fresh read, so the name always describes the lock the same card shows. A
// request that does not change the lock leaves both values — and therefore
// the name — as they were; releasing the lock by any path (unclaim, close,
// the stale-lock reaper, a forced release) clears locked_by and the field
// disappears with it.
//
// OBLIGATION (ADR-1, "Costlier later"): matching on the moment is sound only
// because the lock and its event are written in ONE transaction, where
// `locked_at = now()` and the event's `occurred_at DEFAULT now()` are the
// same transaction timestamp. Today claimInTx is the single writer of both,
// serving the single and the batch claim path alike. A new path that sets a
// lock MUST write the same issue.claimed event (new_data.lockedBy = the
// session) in the same transaction, or the name silently disappears for
// locks it sets. If the event was purged, the field is simply absent — the
// card does not fail.
//
// Cost is bounded by this one issue (NF-1): idx_events_entity
// (entity_type, entity_id, id) narrows the scan to its own events.
func (s *Store) lockHolderUsernameFor(ctx context.Context, issueID, lockedBy string, lockedAt time.Time) (*string, error) {
	var username string
	err := s.pool.QueryRow(ctx, `
SELECT u.username
  FROM events e
  JOIN users u ON u.id = e.actor_user_id
 WHERE e.entity_type = 'issue'
   AND e.entity_id = $1
   AND e.type = $2
   AND e.new_data->>'lockedBy' = $3
   AND e.occurred_at = $4
 ORDER BY e.id DESC
 LIMIT 1`, issueID, EventIssueClaimed, lockedBy, lockedAt).Scan(&username)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resolving lock holder of %s: %w", issueID, err)
	}
	return &username, nil
}

// epicRollupFor computes the sk-32 EpicRollup for one epic-typed
// issue. Single round-trip — the nine counters share the same CTE
// (epicChildrenSQL) so the children set is materialised exactly once.
// Nine since sk-489 (T006): total, open, in_progress, closed, deferred,
// standing, unavailable, no_work, blocked. The docblock said «five» from
// sk-32 until the A/3 review counted them.
//
// BlockedChildren applies sk-31's derived-blocked predicate (EXISTS
// outgoing blocks/waits-for edge to an open target) to each child that
// is itself open or in progress. sk-421.2 (R-11, owner answer N-1a)
// narrowed the leg from "not closed, not standing" to exactly those two
// statuses: a DEFERRED child with an active blocker no longer counts.
// BlockedChildren is therefore a SUBSET of OpenChildren +
// InProgressChildren — a breakdown of "not done", not a standalone flag.
// sk-423: the blocker leg is readerBlockerPredicate — a child counts as
// blocked only by a blocker inside the reader's horizon, the same rule as
// the child's own isBlocked on its card and rows. A child blocked only from
// outside the horizon stays in its open / in_progress bucket: counting it
// here would say whether that invisible blocker is closed.
// DeferredChildren is its own bucket, so the epic card's identity holds:
// closed + (open + in_progress) + deferred + unavailable = total.
//
// EligibleForClose is derived in Go: (Total > 0 AND Closed == Total).
// Empty epics never become eligible.
//
// sk-489 (T006) brings the statement to NINE `COUNT(*) FILTER` legs by adding
// NoWorkChildren — the visible children closed with an outcome other than
// "done", eighth in the SELECT order — and derives DoneChildren and
// CountedChildren from it in Go. Two readings of the same children set now
// live side by side and must not be confused:
//
//   - ClosedChildren / OpenChildren / … stay the breakdown BY STATUS and keep
//     their meaning byte for byte (INV-3). EligibleForClose and the open-child
//     warning read that one, and only that one (INV-6): a task closed without
//     work is closed, and there is nothing left for it to hold the epic open
//     for.
//   - DoneChildren / CountedChildren are the PROGRESS reading (INV-5): the
//     ring fills to Done of Counted, where Counted is Total minus the visible
//     children that dropped out. NoWorkChildren is the tail printed beside it.
//
// The identity that holds on screen (US-5, "the on-screen identity of the ring"):
// Done + Open + InProgress + Deferred + Unavailable = Counted, and
// Counted + NoWork = Total. Both follow from the legs being one partition of
// one children set — which is why the new leg is spelled as the CLOSED leg
// plus one predicate, over the same `vis`, in the same statement.
//
// The invisible children keep their sk-291 treatment unchanged: they are in
// NO outcome leg at ANY outcome, so NoWork cannot disclose one, and they stay
// inside Counted — «cannot say» is not «dropped out» (INV-4, R-6).
//
// F2h (sk-118.9, §11.5): `standing` children are INVISIBLE to the
// rollup. They are excluded from Total, OpenChildren, InProgressChildren
// and BlockedChildren, and surfaced ONLY in the dedicated StandingChildren
// bucket. Because Total drops them, an epic whose only non-closed children
// are standing has Closed == Total and becomes EligibleForClose — the
// standing obligation never strands the epic's close. The BlockedChildren
// FILTER's leg is narrowed since sk-421.2 to `status IN
// ('open','in_progress')`, which admits neither standing nor deferred
// children — so a standing child WITH an active blocker still cannot
// inflate BlockedChildren (the F2g warning), and a deferred one
// no longer can either.
//
// sk-291: children the caller cannot see are counted in UnavailableChildren
// and in NO status bucket. Parent-child may cross projects (sk-35), so an
// epic can hold children in a project the caller is not a member of, and
// the pre-fix rollup published their status distribution — while the
// dependency list right next door deliberately keeps the edge and blanks
// the card. Status is card content.
//
// The invisible rows keep their place in Total, and their standing-ness is
// NOT consulted: excluding invisible standing children would let a reader
// recover the status of an invisible task by subtracting Unavailable from
// Total. As a consequence EligibleForClose is false whenever anything is
// unavailable — "all visible children are closed" is not "the epic is
// done", and answering "cannot say" is the whole point of the bucket.
//
// sk-674.18 T017, INV-28 (4): «invisible» counts only hidden children in
// the reader's own non-personal spaces (hid); a hidden child elsewhere is in
// no number — not in Total, not in UnavailableChildren.
func (s *Store) epicRollupFor(ctx context.Context, epicID string) (*EpicRollup, error) {
	h, hErr := s.horizon(ctx)
	if hErr != nil {
		return nil, hErr
	}
	unrestricted, callerID := h.unrestricted, h.visible
	vis := authz.ProjectVisibleExpr("children", "$2", "$3")
	hid := authz.HiddenCountableExpr("children.project_id", "$2", "$3", "$4")

	var rollup EpicRollup
	err := s.pool.QueryRow(ctx, epicChildrenSQL+`
SELECT
    COUNT(*) FILTER (WHERE (`+vis+` AND status <> 'standing') OR `+hid+`) AS total,
    COUNT(*) FILTER (WHERE `+vis+` AND status = 'open')         AS open_count,
    COUNT(*) FILTER (WHERE `+vis+` AND status = 'in_progress')  AS in_progress_count,
    COUNT(*) FILTER (WHERE `+vis+` AND status = 'closed')       AS closed_count,
    COUNT(*) FILTER (WHERE `+vis+` AND status = 'deferred')     AS deferred_count,
    COUNT(*) FILTER (WHERE `+vis+` AND status = 'standing')     AS standing_count,
    COUNT(*) FILTER (WHERE `+hid+`)                             AS unavailable_count,
    COUNT(*) FILTER (
        WHERE `+vis+` AND status = 'closed'
          AND close_outcome <> 'done'
    )                                                           AS no_work_count,
    COUNT(*) FILTER (
        WHERE `+vis+`
          AND status IN ('open', 'in_progress')
          AND EXISTS (
              SELECT 1 FROM dependencies d
                JOIN issues b ON b.id = d.to_id
               WHERE d.from_id = children.id
                 AND `+readerBlockerPredicate("d", "b", "$2", "$3")+`
          )
    ) AS blocked_count
  FROM children`, epicID, unrestricted, callerID, h.spaces).Scan(
		&rollup.TotalChildren,
		&rollup.OpenChildren,
		&rollup.InProgressChildren,
		&rollup.ClosedChildren,
		&rollup.DeferredChildren,
		&rollup.StandingChildren,
		&rollup.UnavailableChildren,
		&rollup.NoWorkChildren,
		&rollup.BlockedChildren,
	)
	if err != nil {
		return nil, fmt.Errorf("epic rollup for %s: %w", epicID, err)
	}
	// sk-489 — progress by OUTCOME on top of the by-status breakdown. The same
	// arithmetic as in the rows (epicProgressFrom): one subtraction in Go
	// instead of two extra SQL legs, which would have to repeat
	// "visible" and "closed" again.
	rollup.DoneChildren = rollup.ClosedChildren - rollup.NoWorkChildren
	rollup.CountedChildren = rollup.TotalChildren - rollup.NoWorkChildren
	// INV-6: the "can close the epic" flag does NOT depend on the outcome and stays
	// on the by-status breakdown. A closed-without-work issue is closed — the work
	// on it is over, and there is no reason for it to hold the epic open.
	rollup.EligibleForClose = rollup.TotalChildren > 0 &&
		rollup.ClosedChildren == rollup.TotalChildren
	return &rollup, nil
}

// EpicOpenChildrenFor returns the slim list of currently-open
// children (status <> 'closed') under an epic, used by Slice 5 to
// surface the skein_close warning when an agent closes an epic that
// still has work attached. Children come from the dependencies graph
// (parent-child edges to_id = epic.id). sk-35 restored this as the
// canonical mechanism; the issues.parent_id column is frozen. Returns
// BlockerRef shape for symmetry with blockersFor — UI renders both
// lists identically, and both are ordered by sortBlockerEntries (sk-426).
//
// Exported because the MCP close tool computes the snapshot before
// calling Store.Close (the warning is a tool-layer affordance, not a
// store contract — Close itself stays non-blocking by design).
//
// sk-291: children outside the caller's horizon are omitted. This list is
// the same leak as the rollup only worse — it carries titles, not just a
// status distribution — and a list has nowhere to put an "unavailable"
// entry without inventing a placeholder row that the warning text would
// then have to explain. The rollup on the same card carries
// UnavailableChildren, so the "there is something you cannot see" signal is
// not lost; it is just delivered by the surface built to carry it.
func (s *Store) EpicOpenChildrenFor(ctx context.Context, epicID string) ([]BlockerRef, error) {
	h := authz.FromContext(ctx)
	unrestricted, callerID, hErr := h.Bind(ctx, s.pool)
	if hErr != nil {
		return nil, hErr
	}
	rows, err := s.pool.Query(ctx, epicChildrenSQL+`
SELECT c.id, c.title, c.status
  FROM children
  JOIN issues c ON c.id = children.id
 WHERE `+status.ActiveWork("children")+`
   AND `+authz.ProjectVisibleExpr("children", "$2", "$3"), epicID, unrestricted, callerID)
	if err != nil {
		return nil, fmt.Errorf("epic open children for %s: %w", epicID, err)
	}
	defer rows.Close()
	out := make([]BlockerRef, 0)
	for rows.Next() {
		var b BlockerRef
		if err := rows.Scan(&b.ID, &b.Title, &b.Status); err != nil {
			return nil, fmt.Errorf("scanning epic open child: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating epic open children for %s: %w", epicID, err)
	}
	// sk-426: the id-order rule, the same order as the blockers list the UI
	// renders this one like.
	sortBlockerEntries(out, func(b BlockerRef) string { return b.ID })
	return out, nil
}

// ListIssuesArgs is the filter set for the paginated list endpoint.
type ListIssuesArgs struct {
	ProjectID  uuid.UUID
	Statuses   []string // multi-select; empty = no filter
	Assignee   *string
	Priorities []int    // multi-select; empty = no filter
	Types      []string // multi-select; empty = no filter
	Labels     []string // multi-select; matched as ANY
	ParentID   *string
	Pinned     *bool
	Text       *string
	// Source narrows by issues.source. Pointer so nil = "no filter".
	// Validated to be one of {human, agent_directed, agent_autonomous}
	// at the handler boundary (Phase 1 ui-redesign split the legacy
	// "agent" value into directed/autonomous); the store assumes a sane
	// value and inlines it directly into the WHERE clause via a
	// parameter.
	Source *string
	// ComponentID restricts the result to issues that have this
	// component pinned via the issue_components junction. Pointer so nil
	// = no filter. Pushed into the SQL as an EXISTS clause so callers
	// don't need to over-fetch and post-filter.
	ComponentID *uuid.UUID
	CursorTime  *time.Time
	CursorID    *string
	Limit       int
	SortBy      string // "updated" (default) | "created"

	// AssigneeUserID resolves "assignee=me" without forcing the caller to
	// know the assignee's username. When non-nil, ListIssues filters via
	// the LEFT JOIN to users (LOWER(username) match) instead of the
	// free-form Assignee text equality. Mutually exclusive with Assignee
	// — handlers pick one or the other.
	AssigneeUserID *uuid.UUID

	// CreatedBy filters on issues.created_by (the user UUID stamped at
	// CreateIssue). Powers the Outbox / "tasks I created" view; pointer
	// so nil = no filter. Phase 2 ui-redesign — B5.
	CreatedBy *uuid.UUID

	// CreatedByExclude is the symmetric inverse of CreatedBy — restricts
	// to issues whose created_by IS DIFFERENT FROM the supplied user id.
	// Powers the "On me" lens (assignee=me AND created_by != me — tasks
	// assigned to the caller by SOMEONE ELSE). Mutually independent from
	// CreatedBy; both can be set in the same call though no current
	// surface combines them.
	CreatedByExclude *uuid.UUID

	// AcceptedAtNull, when non-nil and true, restricts the result to
	// issues with `accepted_at IS NULL` — the Inbox surface (assignee
	// hasn't acknowledged yet). Pointer so nil = no filter; false is
	// rejected at the handler boundary (only `null` is meaningful for
	// the Inbox use case). Phase 2 ui-redesign — B5.
	AcceptedAtNull *bool

	// AcceptedAtNotNull is the symmetric inverse of AcceptedAtNull —
	// when non-nil and true, restricts to `accepted_at IS NOT NULL`. The
	// "On me" lens needs this prefix: tasks the caller has already
	// accepted and is actively working. Inbox shows the unaccepted
	// prefix; On me shows the accepted prefix.
	AcceptedAtNotNull *bool

	// OriginGroups maps the wire-level multi-select origin filter to a set
	// of issues.source values via OriginGroupsToSources. Validated at the
	// handler boundary to a subset of {human-driven, agent-autonomous,
	// all}. Empty = no filter; a group set containing "all" also means no
	// filter (OriginGroupsToSources' noFilter return). Phase 2 ui-redesign
	// — B5; multi-select — sk-273.
	OriginGroups []string

	// Caller is the resolved caller's user id, used to compute the
	// IssueSummary.IAssigned flag (true when caller created the issue
	// and assigned it to someone else). nil = unauthenticated context;
	// the flag is left false. Phase 2 ui-redesign — B5.
	Caller *uuid.UUID

	// IsBlocked filters by the derived blocked-ness flag (sk-31). Nil
	// = no filter. Non-nil/true restricts to issues whose dep graph
	// EXISTS predicate is true (open blockers exist); non-nil/false
	// restricts to issues with no open blockers. The same EXISTS
	// subquery from the SELECT-list is reused as a WHERE predicate so
	// the two reads can never disagree.
	IsBlocked *bool

	// CallerUsername is the caller's `users.username` (canonical case).
	// Pairs with Caller (the uuid) to drive the sk-34 Delegated chain
	// predicate when `CreatedBy == Caller` is supplied — see the
	// extended OR-chain in ListIssues. Optional in every other context.
	// It GATES that chain rather than feeding it: the chain's
	// "assigned to somebody other than me" leg resolves the handle
	// through assignee.MatchesCaller (all three forms — username,
	// e-mail, user UUID), pivoting on the caller's id (sk-523).
	CallerUsername string

	// Unreviewed — the unreviewed filter (sk-303.14, contract C3). This
	// is the PROJECT path of the Delegated surface: the project issue
	// list filtered by creator (`?createdBy=me&unreviewed=true`). The
	// second path — cross-project — lives in ListByUser.
	//
	// The predicate is NOT written here: it is taken whole from
	// review.UnreviewedPredicate, the single definition of the set
	// (INV-3). The filter is expressed as a SEPARATE query parameter, not
	// as a new value in a closed status set: "unreviewed" is not an issue
	// status, it is a relation of the issue to the caller, and it has no
	// place in the status enumeration (FOR-PLAN 5).
	//
	// This path does NOT apply a "without closed" default (empty Statuses
	// here means "any status"), so there is nothing to lift — the lift is
	// recorded in ListByUser, on the path that has the default.
	//
	// The visibility horizon sits inside the fragment (the access
	// relation, authz); the fragment does not need the caller's name — it
	// resolves it in the query itself.
	Unreviewed bool
}

// ListIssuesDefaultLimit and ListIssuesMaxLimit are the page-size default
// and ceiling BOTH issue-list paths (ListIssues and ListAllIssues) apply
// when the caller does not name one, or names one above the ceiling.
//
// They are exported and named — rather than inline literals — because
// INV-5 ("the schema does not lie") is checked by a guard that compares what a tool's
// schema DECLARES against what the code APPLIES. That comparison needs one
// reachable name per applied value; two literals in two functions cannot be
// compared to anything, which is how the current skein_list divergence
// (schema says 50/100, code applies 500/500) went unnoticed.
const (
	ListIssuesDefaultLimit = 500
	ListIssuesMaxLimit     = 500
)

// ListIssues runs a keyset-paginated SELECT honouring the filters above.
// Sort is `updated_at DESC, id DESC` by default; cursor encodes the same
// pair so deletes on prior pages don't shift remaining results.
//
// Total is populated with an exact COUNT(*) over the same WHERE clause
// (sans cursor) so the Tasks column header can render an authoritative
// "Tasks · N" counter without paging through every match. Cheap on
// typical project sizes (a few thousand issues per project) — same
// pattern used by ListAllIssues.
func (s *Store) ListIssues(ctx context.Context, args ListIssuesArgs) (*ListResult, error) {
	h, hErr := s.horizon(ctx)
	if hErr != nil {
		return nil, hErr
	}
	if args.Limit < 1 {
		args.Limit = ListIssuesDefaultLimit
	}
	if args.Limit > ListIssuesMaxLimit {
		args.Limit = ListIssuesMaxLimit
	}

	sortColumn := "updated_at"
	if args.SortBy == "created" {
		sortColumn = "created_at"
	}

	var (
		clauses []string
		params  []any
	)
	addParam := func(v any) string {
		params = append(params, v)
		return fmt.Sprintf("$%d", len(params))
	}
	// sk-423: the blocked flag is the reader's; bound on first use.
	blockedExpr := readerBlockedBinder(h, addParam)

	clauses = append(clauses, fmt.Sprintf("i.project_id = %s", addParam(args.ProjectID)))

	if len(args.Statuses) > 0 {
		clauses = append(clauses, fmt.Sprintf("i.status = ANY(%s)", addParam(args.Statuses)))
	}
	if args.AssigneeUserID != nil {
		// "assignee=me" — the issues delivered to that user
		// (assignee.MatchesCaller: the same person LeftJoin brings in as
		// ua), not the free-form i.assignee text. Note: this excludes
		// issues whose assignee text doesn't resolve to a real user
		// (legacy / typoed usernames) which is the right behaviour for
		// the Inbox.
		clauses = append(clauses, assignee.MatchesCaller("i", addParam(*args.AssigneeUserID)))
	} else if args.Assignee != nil {
		clauses = append(clauses, fmt.Sprintf("i.assignee = %s", addParam(*args.Assignee)))
	}
	if len(args.Priorities) > 0 {
		clauses = append(clauses, fmt.Sprintf("i.priority = ANY(%s)", addParam(args.Priorities)))
	}
	if len(args.Types) > 0 {
		clauses = append(clauses, fmt.Sprintf("i.type = ANY(%s)", addParam(args.Types)))
	}
	if args.ParentID != nil {
		// sk-35: parent_id column is frozen, derive from dep graph.
		clauses = append(clauses, fmt.Sprintf(`EXISTS (
    SELECT 1 FROM dependencies d
     WHERE d.from_id = i.id AND d.to_id = %s AND d.type = 'parent-child'
)`, addParam(*args.ParentID)))
	}
	if args.Pinned != nil {
		clauses = append(clauses, fmt.Sprintf("i.pinned = %s", addParam(*args.Pinned)))
	}
	if args.Source != nil {
		clauses = append(clauses, fmt.Sprintf("i.source = %s", addParam(*args.Source)))
	}
	if args.CreatedBy != nil {
		// sk-34 — Delegated lens extension. The pre-sk-34 predicate was
		// strict "i.created_by = $caller". After sk-34 the Delegated
		// lens widens to "I'm somewhere in this issue's chain of
		// responsibility":
		//
		//   created_by = me
		//   OR (me ∈ previous_assignees
		//       AND me ∉ dismissed_by
		//       AND LOWER(assignee) <> me_username)
		//
		// CreatedBy is the only wire path that flips this lens on (the
		// handler maps `?createdBy=me` to CreatedBy = caller.UserID), so
		// re-using the same arg is the single source for the new
		// semantics. CallerUsername gates the assignee-mismatch clause:
		// when the caller is unauthenticated or has no username (rare —
		// only the cross-project list with anonymous role can hit this)
		// we degrade to the strict predicate so the lens never widens
		// without a username to compare against.
		callerPh := addParam(*args.CreatedBy)
		if args.CallerUsername != "" {
			// Both branches of the OR-chain share the trailing
			// `assignee != me` predicate — Delegated is the "tasks
			// not currently on my plate" lens. A creator who got
			// the task reassigned back to themselves drops out of
			// Delegated (sk-34 T6).
			//
			// Unassigned issues (i.assignee IS NULL) are NOT in
			// Delegated: a task with no assignee isn't actually
			// delegated to anyone — it's just unowned. Treating it
			// as Delegated put a creator's own un-assigned tasks
			// into their Outbox (demo-5 surfaced this). The strict
			// `assignee IS NOT NULL AND LOWER(assignee) <> me`
			// matches the user-visible semantics of "delegated to
			// someone else who is currently holding it".
			clauses = append(clauses, fmt.Sprintf(
				"(i.assignee IS NOT NULL AND NOT %[2]s "+
					"AND (i.created_by = %[1]s "+
					"OR (%[1]s = ANY(i.previous_assignees) "+
					"AND NOT %[1]s = ANY(i.dismissed_by))))",
				callerPh, assignee.MatchesCaller("i", callerPh)))
		} else {
			clauses = append(clauses, fmt.Sprintf("i.created_by = %s", callerPh))
		}
	}
	if args.CreatedByExclude != nil {
		// "On me" predicate: tasks I did NOT originate. Pairs with
		// AssigneeUserID=caller to surface delegations addressed to me.
		clauses = append(clauses, fmt.Sprintf("i.created_by <> %s", addParam(*args.CreatedByExclude)))
	}
	if args.Unreviewed {
		// sk-303.14 T012 — the unreviewed filter, project path.
		// The single definition of the set; this path does not invent
		// its own predicate (INV-3). The fragment's strictly-author
		// breadth automatically narrows the widened Delegated lens here
		// too (`createdBy=me` above lifts the previous-assignee chain —
		// intersection with the fragment leaves only what the caller
		// created, INV-13 / SC-018).
		var caller uuid.UUID
		if args.Caller != nil {
			caller = *args.Caller
		}
		clauses = append(clauses, review.UnreviewedPredicate(addParam, review.UnreviewedArgs{
			Alias:    "i",
			CallerID: caller,
		}))
	}
	if args.AcceptedAtNull != nil && *args.AcceptedAtNull {
		// Inbox predicate: assignee hasn't acknowledged yet.
		clauses = append(clauses, "i.accepted_at IS NULL")
	}
	if args.AcceptedAtNotNull != nil && *args.AcceptedAtNotNull {
		// "On me" predicate: assignee has accepted, is actively working.
		clauses = append(clauses, "i.accepted_at IS NOT NULL")
	}
	if sources, noFilter := OriginGroupsToSources(args.OriginGroups); !noFilter {
		clauses = append(clauses, fmt.Sprintf("i.source = ANY(%s)", addParam(sources)))
	}
	if args.Text != nil {
		// Pre-FTS path (R1.5 will replace with tsvector). Keep ILIKE on
		// title only — description ILIKE on a million rows would be brutal.
		clauses = append(clauses, fmt.Sprintf("i.title ILIKE %s", addParam("%"+*args.Text+"%")))
	}
	if len(args.Labels) > 0 {
		// EXISTS subquery — we want issues that have AT LEAST ONE of the
		// requested labels. For all-of semantics we'd need a HAVING COUNT.
		clauses = append(clauses, fmt.Sprintf(`EXISTS (
    SELECT 1 FROM issue_labels il
     WHERE il.issue_id = i.id AND il.label = ANY(%s)
)`, addParam(args.Labels)))
	}
	if args.ComponentID != nil {
		// Component filter — push the issue_components join into SQL so
		// callers don't have to over-fetch and post-filter (which
		// silently truncates when many issues at the front of the order
		// don't match the component).
		clauses = append(clauses, fmt.Sprintf(`EXISTS (
    SELECT 1 FROM issue_components ic
     WHERE ic.issue_id = i.id AND ic.component_id = %s
)`, addParam(*args.ComponentID)))
	}
	if args.IsBlocked != nil {
		// sk-31: filter by derived blocked-ness. Same EXISTS expression
		// (and the same bound horizon, sk-423) as the SELECT-list column so
		// the two reads always agree.
		if *args.IsBlocked {
			clauses = append(clauses, blockedExpr())
		} else {
			clauses = append(clauses, "NOT "+blockedExpr())
		}
	}
	// Snapshot the where-without-cursor clauses for the COUNT query
	// before adding the cursor clause (count is unfiltered by cursor).
	countClauses := append([]string(nil), clauses...)
	countParams := append([]any(nil), params...)

	if args.CursorTime != nil && args.CursorID != nil {
		// Tuple comparison gives the right keyset semantics: the next page
		// starts strictly after (cursor.time, cursor.id) in the (DESC, DESC)
		// ordering — same as ORDER BY direction.
		clauses = append(clauses, fmt.Sprintf("(i.%s, i.id) < (%s, %s)",
			sortColumn, addParam(*args.CursorTime), addParam(*args.CursorID)))
	}

	limitPh := addParam(args.Limit + 1)

	// iAssigned column — true when the caller created the issue and
	// assigned it to someone else (resolved assignee user id != caller).
	// When Caller is nil (anonymous context) we emit a constant FALSE so
	// the result shape stays uniform.
	iAssignedExpr := "FALSE"
	if args.Caller != nil {
		callerPh := addParam(*args.Caller)
		iAssignedExpr = fmt.Sprintf(
			"(i.created_by = %s AND ua.id IS NOT NULL AND ua.id <> %s)",
			callerPh, callerPh)
	}

	// sk-291: the epic-progress columns are scoped to the caller's
	// horizon. Bound here, after the WHERE params, so the placeholder
	// numbers stay stable no matter which filters fired.
	epicVisUnrestricted, epicVisCaller := h.unrestricted, h.visible
	epicProgressCols := epicProgressProjection(addParam(epicVisUnrestricted), addParam(epicVisCaller), addParam(h.spaces))
	parentCol := parentIDForReader(h, addParam)

	// Same LEFT JOINs as GetIssue (see issueRowJoins) so each summary
	// row carries the resolved creator/assignee usernames without a
	// second query. Both joins are LEFT so missing users stay NULL.
	//
	// Phase 1 ui-redesign: i.accepted_at flows through to
	// IssueSummary.AcceptedAt so the Inbox/list view can render the
	// "needs acceptance" chip without round-tripping for the full detail.
	// Phase 2 ui-redesign (B5): an extra computed column `i_assigned`
	// surfaces the "tasks I delegated" highlight without forcing the UI
	// to re-derive it client-side.
	sql := fmt.Sprintf(`
SELECT i.id, i.project_id, %s, i.version, i.title,
       i.type, i.priority, i.status, i.pinned, i.assignee,
       i.created_by, uc.username, ua.id, `+issueSummaryPersonCols+`,
       i.created_at, i.updated_at, i.row_seq,
       `+rowComponentsLabelsCols+`,
       i.accepted_at, i.close_outcome,
       %s AS i_assigned,
       %s AS is_blocked,
       %s
  FROM issues i
  LEFT JOIN users uc ON uc.id = i.created_by
  `+assignee.LeftJoin("ua", "i")+`
 WHERE %s
 ORDER BY i.%s DESC, i.id DESC
 LIMIT %s`, parentCol, iAssignedExpr, blockedExpr(),
		epicProgressCols,
		strings.Join(clauses, " AND "), sortColumn, limitPh)

	rows, err := s.pool.Query(ctx, sql, params...)
	if err != nil {
		return nil, fmt.Errorf("listing issues: %w", err)
	}
	defer rows.Close()

	items := make([]IssueSummary, 0, args.Limit)
	for rows.Next() {
		var item IssueSummary
		var parentID *string
		var assignee *string
		var createdByUsername *string
		var assigneeUserID *uuid.UUID
		var acceptedAt *time.Time
		var closeOutcome *string
		var iAssigned bool
		var isBlocked bool
		var epicTotal, epicClosed, epicUnavailable, epicNoWork *int
		if err := rows.Scan(
			&item.ID, &item.ProjectID, &parentID, &item.Version, &item.Title,
			&item.Type, &item.Priority, &item.Status, &item.Pinned, &assignee,
			&item.CreatedBy, &createdByUsername, &assigneeUserID, &item.AssigneeStatus, &item.CreatedByStatus, &item.SpaceID,
			&item.CreatedAt, &item.UpdatedAt, &item.RowSeq,
			&item.ComponentIDs, &item.Labels,
			&acceptedAt, &closeOutcome,
			&iAssigned,
			&isBlocked,
			&epicTotal, &epicClosed, &epicUnavailable, &epicNoWork,
		); err != nil {
			return nil, fmt.Errorf("scanning issue row: %w", err)
		}
		item.ParentID = parentID
		item.Assignee = assignee
		item.CreatedByUsername = createdByUsername
		item.AssigneeUserID = assigneeUserID
		item.AcceptedAt = acceptedAt
		item.CloseOutcome = closeOutcome
		item.IAssigned = iAssigned
		item.IsBlocked = isBlocked
		item.EpicProgress = epicProgressFrom(epicTotal, epicClosed, epicUnavailable, epicNoWork)
		normalizeComponentsLabels(&item.ComponentIDs, &item.Labels)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	hasMore := false
	if len(items) > args.Limit {
		hasMore = true
		items = items[:args.Limit]
	}

	// Components and labels arrived with the rows (rowComponentsLabelsCols):
	// one statement for the page, the same snapshot as each row's rowSeq.

	var nextCursor *string
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		t := last.UpdatedAt
		if sortColumn == "created_at" {
			t = last.CreatedAt
		}
		c := EncodeCursor(t, last.ID)
		nextCursor = &c
	}

	total, err := s.countListIssues(ctx, countClauses, countParams)
	if err != nil {
		return nil, err
	}

	return &ListResult{Items: items, Total: total, NextCursor: nextCursor, HasMore: hasMore}, nil
}

// countListIssues runs `SELECT COUNT(*) FROM issues i ...` against the
// same WHERE clauses as the row query (sans cursor). Used by ListIssues and
// ListByUser to populate ListResult.Total — mirrors countAllIssues in
// list_all.go: no clause names the users join, so the count joins nothing
// (sk-712).
func (s *Store) countListIssues(ctx context.Context, clauses []string, params []any) (int, error) {
	where := ""
	if len(clauses) > 0 {
		where = "WHERE " + strings.Join(clauses, " AND ")
	}
	sql := "SELECT COUNT(*) FROM issues i " + where
	var n int
	if err := s.pool.QueryRow(ctx, sql, params...).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting issues: %w", err)
	}
	return n, nil
}

// oldAssigneeAdmittedToChain reports whether the person the old handle
// names (oldUserID) may join the delegation chain of issue id when the
// assignee changes: he may work in the project now (the delivery
// resolution, notifications.ResolveAssigneeUserID) or he already has a
// historical relation to the issue (authz.PersonLinkedToIssueExpr). Read
// before the issue row changes, inside the same transaction.
func oldAssigneeAdmittedToChain(ctx context.Context, tx pgx.Tx, id, oldHandle string, oldUserID, projectID uuid.UUID) (bool, error) {
	eligible, err := notifications.ResolveAssigneeUserID(ctx, tx, oldHandle, projectID)
	if err != nil {
		return false, err
	}
	if eligible == oldUserID {
		return true, nil
	}
	var linked bool
	if err := tx.QueryRow(ctx, `SELECT `+authz.PersonLinkedToIssueExpr("$1::uuid", "i")+` FROM issues i WHERE i.id = $2`,
		oldUserID, id).Scan(&linked); err != nil {
		return false, fmt.Errorf("reading the old assignee's relation to the issue: %w", err)
	}
	return linked, nil
}

// ----------------- Patch -----------------

// PatchIssue applies whatever fields are non-nil on input, with optimistic
// concurrency: the UPDATE only matches if version = ExpectedVersion. A
// row-count of 0 after a successful row lookup means the version moved
// underneath us — we re-read and return ErrVersionConflict.
//
// R2.3: when input.SessionID is non-empty AND the issue's locked_by is set
// to a different session, the patch is blocked with ErrLockedByOther. The
// caller can pass input.Force=true (admin only at the tool layer) to skip
// this check. When force=true we annotate the issue.updated event payload
// with reason="force-overridden" so audit logs can surface override usage.
//
// sk-489: PatchIssue is the thin wrapper — every existing call site keeps
// its two-value signature. The close-target link state (US-2) rides on
// PatchIssueWithResult below, for the same reason it does on
// CloseWithResult: only the MCP acknowledgements need it.
func (s *Store) PatchIssue(
	ctx context.Context,
	id string,
	input PatchIssueInput,
	actor Actor,
) (*IssueDetail, error) {
	dto, _, err := s.PatchIssueWithResult(ctx, id, input, actor)
	return dto, err
}

// PatchIssueWithResult is PatchIssue plus the close-target link state
// (sk-489, US-2, US-3).
//
// An outcome edit is PRECISELY a field edit, not a close: the status, close date
// and close reason stay as they were (INV-8), the lock and version rule is
// the same as for any edit. Exactly three things set it apart from an ordinary field, and all
// three follow from the outcome being not text but the state of a result:
// its own issue.outcome_changed event instead of a line in the hybrid
// diff, a reset of the review mark when leaving "done" by a non-author, and
// the link by target.
func (s *Store) PatchIssueWithResult(
	ctx context.Context,
	id string,
	input PatchIssueInput,
	actor Actor,
) (*IssueDetail, CloseLinkState, error) {
	state, linkState, err := s.PatchIssueCommitted(ctx, id, input, actor)
	if err != nil {
		return nil, CloseLinkNone, err
	}
	// sk-607: committed — read back detached from the caller's context; a
	// failed read-back is *ErrReadBackFailed carrying the committed state.
	details, err := s.readBack(ctx, "patch", []CommittedState{state})
	if err != nil {
		return nil, linkState, err
	}
	return details[0], linkState, nil
}

// PatchIssueCommitted is the field-patch writer without the read-back of
// the card (sk-607): it returns the post-state its transaction read and the
// close-target link state. The skein_update chain needs only the version
// between its steps, and a card read there was a read that could fail after
// the patch had committed and stop the chain.
func (s *Store) PatchIssueCommitted(
	ctx context.Context,
	id string,
	input PatchIssueInput,
	actor Actor,
) (CommittedState, CloseLinkState, error) {
	linkState := CloseLinkNone
	var committed CommittedState
	if input.ExpectedVersion < 1 {
		return CommittedState{}, linkState, &ValidationError{Code: "missing_version", Message: "expected version is required"}
	}
	// sk-28 store-level backstop, same rationale as CreateIssue.
	if err := validatePatchInputProse(input); err != nil {
		return CommittedState{}, linkState, err
	}
	if strings.TrimSpace(input.CloseTarget) != "" && input.DependenciesCaller == nil {
		return CommittedState{}, linkState, fmt.Errorf("DependenciesCaller is required when CloseTarget is set")
	}

	var projectID uuid.UUID

	err := s.withSerializableRetry(ctx, "patchIssue", patchIssueMaxAttempts, withCommittedState(ctx, id, &committed, func(tx pgx.Tx) error {
		// A replayed attempt starts from scratch (sk-622.21, R17): the link
		// state of a rolled-back attempt must not leak into this one.
		linkState = CloseLinkNone
		// K-7 (sk-674.18 T018): a change of assignee is made under the
		// shared lock of the issue's space, taken before the issue row.
		if input.Assignee != nil {
			if err := spaces.ShareIssueSpace(ctx, tx, id); err != nil {
				return err
			}
		}
		// Read current row to compare versions and capture old snapshot
		// for the event payload.
		old, err := s.fetchIssueInTx(ctx, tx, id)
		if err != nil {
			return err
		}
		// sk-622.21 T015: "Assignee change" names the roles before the
		// write too (the old assignee among them).
		var rolesBefore signalRoles
		if input.Assignee != nil {
			if rolesBefore, err = readSignalRoles(ctx, tx, id); err != nil {
				return err
			}
		}
		// Lock predicate runs BEFORE the version check so a locked issue
		// surfaces ErrLockedByOther rather than a confusing version
		// mismatch when the holder bumped version mid-flight.
		if err := lockGuard(old, input.SessionID, actor.UserID, input.Force); err != nil {
			return err
		}
		if !input.Force && old.Version != input.ExpectedVersion {
			return &ErrVersionConflict{CurrentVersion: old.Version}
		}
		projectID = old.ProjectID

		// sk-489 (T004): an outcome edit of a closed issue. The same helper
		// as in both close writers — all checks of the target and the value
		// sound here verbatim the same as there.
		touchesOutcome := input.CloseOutcome != nil || strings.TrimSpace(input.CloseTarget) != ""
		var decision CloseOutcomeDecision
		if touchesOutcome {
			if old.Status != StatusClosed {
				// An unclosed issue has no outcome (INV-1), so there is
				// nothing to edit. The rejection comes before any write: the version does not grow.
				//
				// The request is correct by FORM here — it is forbidden by
				// the issue's current state — so the code is a business one and the
				// surface must return 422 (the list is in
				// handler/issues.go#issueValidationIsBusinessError). The rejection
				// of a transition "outcome with a status != closed" carries a DIFFERENT code
				// (close_outcome_not_a_close) and a different class, 400: there
				// the very combination of request fields is impossible.
				return newValidationError("close_outcome_requires_closed",
					"closeOutcome can only be edited on a closed issue")
			}
			previousOutcome := ""
			if old.CloseOutcome != nil {
				previousOutcome = *old.CloseOutcome
			}
			var derr error
			decision, derr = DecideCloseOutcome(CloseOutcomeRequest{
				IssueID:         id,
				Requested:       input.CloseOutcome,
				Target:          input.CloseTarget,
				AlreadyClosed:   true,
				PreviousOutcome: previousOutcome,
			})
			if derr != nil {
				return derr
			}
			// sk-540.2 exception (a): before any write.
			if err := ensureCloseLinkExists(ctx, tx, id, decision); err != nil {
				return err
			}
		}
		// Branch (2) of the review-mark reset predicate (SC-009) — shared with the close
		// writers. Branch (1) is impossible here: an edit does not change the status.
		resetReviewOnOutcome := touchesOutcome &&
			OutcomeChangeResetsReview(decision.Previous, decision.Outcome,
				actor.UserID, old.CreatedBy)
		// The outcome is written only when it is edited; the COALESCE below leaves the
		// column untouched on nil. The close date and reason are not
		// mentioned in the UPDATE at all — INV-8 is held by the absence of a write,
		// not by undoing one.
		var outcomeArg any
		if touchesOutcome {
			outcomeArg = decision.Outcome
		}

		// Phase 1 ui-redesign: detect assignee change so we can reset /
		// re-seed acceptance state in the same UPDATE.
		//
		// "assigneeChanged" mirrors the IssuePatchFields diff semantics:
		// the patch must explicitly set Assignee (non-nil pointer) AND
		// the value must differ from the existing column. A PATCH that
		// sets assignee to its current value writes nothing to
		// acceptance state. We pre-compute resolved acceptance here
		// rather than parsing the change-list post-UPDATE because the
		// new accepted_at/by need to land in the same SQL statement.
		assigneeChanged := false
		var newAcceptedAtArg, newAcceptedByArg any
		var patchAcceptedAt time.Time
		var newAssigneeUserID uuid.UUID
		// sk-34: captured pre-UPDATE so the post-UPDATE array mutation can
		// append the OLD assignee to previous_assignees and prune the NEW
		// assignee from both arrays when they come back. Scoped to the
		// "if input.Assignee != nil" branch where it's resolved.
		var oldAssigneeUserIDForChain uuid.UUID
		// newAssigneeIdentityForChain — the person the new handle names,
		// whatever his access (sk-674.18 T028): the chain prunes him from
		// previous_assignees and dismissed_by by identity.
		var newAssigneeIdentityForChain uuid.UUID
		if input.Assignee != nil {
			oldNil := old.Assignee == nil
			oldVal := ""
			if !oldNil {
				oldVal = *old.Assignee
			}
			newVal := *input.Assignee
			if newVal != "" {
				if err := checkAssigneeChoice(ctx, tx, newVal, projectID); err != nil {
					return err
				}
			}

			// Resolve both sides to a users.id and gate "changed" on UUID
			// inequality. ResolveAssigneeUserID is case-insensitive on
			// users.username (LOWER=LOWER), so a case-only edit like
			// "Admin" → "admin" lands the same UUID on both sides and
			// surfaces here as unchanged — leaving accepted_at /
			// accepted_by intact and emitting no audit row from the
			// change-list. Raw string compare (the pre-fix behaviour)
			// would have flipped assigneeChanged=true and spuriously
			// reset acceptance.
			//
			// uuid.Nil on either side means the string didn't resolve to
			// any user (free-form label like "the docs team", or empty
			// for unassign). In that case we fall back to a
			// case-insensitive string compare so two different
			// unresolvable labels still register as a change while
			// case-only edits of the same label do not.
			var oldUserID uuid.UUID
			if !oldNil && oldVal != "" {
				rid, rerr := notifications.ResolveAssigneeIdentity(ctx, tx, oldVal)
				if rerr != nil {
					return rerr
				}
				oldUserID = rid
			}
			// Who joins previous_assignees: the old
			// assignee only if he could legitimately hold the issue — he may
			// work in the project at this change — or already has a
			// historical relation to it (authz.PersonLinkedToIssueExpr, the
			// display ground of INV-30). Free text naming anybody else is
			// text (E-33, E-37); appending its identity would turn it into
			// a person on the card. A participant who loses his access is
			// moved here by the revocation itself (INV-12, authz/revoke.go).
			if oldUserID != uuid.Nil {
				admitted, aerr := oldAssigneeAdmittedToChain(ctx, tx, id, oldVal, oldUserID, projectID)
				if aerr != nil {
					return aerr
				}
				if admitted {
					oldAssigneeUserIDForChain = oldUserID
				}
			}
			// Identity on both sides (sk-674.18 T028): whether the assignee
			// changed and who joins previous_assignees are questions about
			// the person, not about delivering to him.
			var newIdentity uuid.UUID
			if newVal != "" {
				rid, rerr := notifications.ResolveAssigneeIdentity(ctx, tx, newVal)
				if rerr != nil {
					return rerr
				}
				newIdentity = rid
				newAssigneeIdentityForChain = rid
				// Delivery (watch, notification, acceptance) only to a person
				// who may work in the project now (INV-30).
				did, derr := notifications.ResolveAssigneeUserID(ctx, tx, newVal, projectID)
				if derr != nil {
					return derr
				}
				newAssigneeUserID = did
			}

			switch {
			case newIdentity != uuid.Nil && oldUserID != uuid.Nil:
				assigneeChanged = newIdentity != oldUserID
			case newIdentity == uuid.Nil && oldUserID == uuid.Nil:
				// Both sides are unresolvable (or both empty). Compare
				// the raw strings case-insensitively so "" ≠ "the docs
				// team" is a change, but "alice" ≡ "Alice" is not.
				assigneeChanged = !strings.EqualFold(oldVal, newVal)
			default:
				// One resolves, the other does not (or transitions
				// to/from empty) — always a change.
				assigneeChanged = true
			}

			if assigneeChanged && newVal != "" {
				if newAssigneeUserID == actor.UserID && newAssigneeUserID != uuid.Nil {
					// Self-assign on patch → auto-accept (Q-4 = a,
					// symmetric with create path).
					patchAcceptedAt = time.Now().UTC()
					newAcceptedAtArg = patchAcceptedAt
					newAcceptedByArg = actor.UserID
				}
			}
			// If assigneeChanged && newVal == "" — unassign (Q-3 = a):
			// leave both args nil so the CASE expressions write NULL.
		}

		// SQL predicate parametrised: $force OR (locked_by IS NULL OR
		// locked_by = $session) AND $force OR version = $expected. The Go-
		// level lockGuard above has already validated, but we also encode
		// the predicate in SQL so a concurrent UPDATE racing between the
		// SELECT and our UPDATE cannot slip past unnoticed.
		//
		// sk-303.13 / T021: one decision for every path that touches the
		// stamp. Note what it deliberately does NOT fire on. A patch that
		// re-points the assignee at somebody else CLEARS the stamp — the
		// reverse transition — and INV-5 is one-directional: the
		// historical acceptance event stays, no new event is written
		// (US-11). And a self-assign onto a row that was ALREADY accepted
		// re-stamps it without an empty→filled transition, so it writes
		// nothing either.
		patchAcceptance := ClassifyAcceptance(old.AcceptedAt != nil,
			assigneeChanged && newAcceptedAtArg != nil, false)

		// accepted_at / accepted_by are CASE-gated on $assigneeChanged
		// so rows whose assignee didn't change keep their prior
		// acceptance state untouched (Q-6 = b — acceptance does NOT get
		// its own audit row of the "issue.updated" kind; the acceptance
		// event above is a different thing).
		ct, err := tx.Exec(ctx, `
UPDATE issues
   SET title               = COALESCE($2, title),
       description         = COALESCE($3, description),
       type                = COALESCE($4, type),
       priority            = COALESCE($5, priority),
       assignee            = COALESCE($6, assignee),
       design              = COALESCE($7, design),
       acceptance_criteria = COALESCE($8, acceptance_criteria),
       notes               = COALESCE($9, notes),
       estimated_minutes   = COALESCE($10, estimated_minutes),
       due_at              = COALESCE($11, due_at),
       defer_until         = COALESCE($12, defer_until),
       external_ref        = COALESCE($13, external_ref),
       source_context      = COALESCE($14, source_context),
       accepted_at         = CASE WHEN $18::boolean THEN $19 ELSE accepted_at END,
       accepted_by         = CASE WHEN $18::boolean THEN $20 ELSE accepted_by END,
       close_outcome       = COALESCE($22::text, close_outcome),
       reviewed_at         = CASE WHEN $23::boolean THEN NULL ELSE reviewed_at END,
       reviewed_by         = CASE WHEN $23::boolean THEN NULL ELSE reviewed_by END,
       version             = version + 1,
       updated_at          = now()
 WHERE id = $1
   AND ($16::boolean OR version = $15)
   AND ($16::boolean OR locked_by IS NULL
        OR (locked_by = $17::text AND locked_by_user_id = $21::uuid))`,
			id,
			input.Title, input.Description, input.Type, input.Priority,
			input.Assignee, input.Design, input.AcceptanceCriteria,
			input.Notes, input.EstimatedMinutes, input.DueAt, input.DeferUntil,
			input.ExternalRef, input.SourceContext,
			input.ExpectedVersion,
			input.Force, input.SessionID,
			assigneeChanged, newAcceptedAtArg, newAcceptedByArg,
			actor.UserID, outcomeArg, resetReviewOnOutcome,
		)
		if err != nil {
			return fmt.Errorf("updating issue: %w", err)
		}
		if ct.RowsAffected() == 0 {
			// fetchIssueInTx already verified version matched, so this
			// branch is essentially defensive (multi-tenant trigger races).
			return &ErrVersionConflict{CurrentVersion: old.Version}
		}

		// sk-34 — delegation-chain bookkeeping on assignee change.
		//
		// Three array mutations run in one statement so the tx stays
		// atomic with the main UPDATE above:
		//
		//   1. Append the OLD assignee uuid to previous_assignees iff
		//      it isn't already present (the array is set-semantic).
		//      uuid.Nil is skipped — happens when the prior assignee
		//      didn't resolve to a real user.
		//
		//   2. Remove the NEW assignee uuid from dismissed_by — if
		//      they explicitly dropped earlier and the task came back
		//      to them, the sticky dismissed_by entry is cleared so the
		//      Delegated lens predicate doesn't continue hiding it
		//      (the bell-silence side-effect — the deleted subscription
		//      and its opt-out — stays decoupled: re-watching is a
		//      separate user action via skein_watchers(action="watch")).
		//
		//   3. Remove the NEW assignee uuid from previous_assignees —
		//      the array tracks PAST assignees only; the current
		//      assignee is always implicit and must not double up.
		//
		// All three are conditional via WHERE NOT $X = ANY(...) so
		// the statement is idempotent and won't redundantly bump the
		// physical TOAST page when arrays don't actually change.
		if assigneeChanged {
			if _, err := tx.Exec(ctx, `
UPDATE issues
   SET previous_assignees = CASE
           WHEN $2::uuid <> '00000000-0000-0000-0000-000000000000'::uuid
                AND NOT $2::uuid = ANY(previous_assignees)
               THEN array_append(previous_assignees, $2::uuid)
           ELSE previous_assignees
       END,
       dismissed_by = CASE
           WHEN $3::uuid <> '00000000-0000-0000-0000-000000000000'::uuid
               THEN array_remove(dismissed_by, $3::uuid)
           ELSE dismissed_by
       END
 WHERE id = $1`,
				id,
				oldAssigneeUserIDForChain,
				newAssigneeIdentityForChain,
			); err != nil {
				return fmt.Errorf("updating delegation chain (append old): %w", err)
			}
			// Second statement: prune the NEW assignee from
			// previous_assignees if present. Split out so the WHERE
			// guard on each mutation stays readable (array_remove is
			// always a no-op when the value isn't present, but the
			// CASE-guard above is cleaner with one mutation per
			// branch).
			if newAssigneeIdentityForChain != uuid.Nil {
				if _, err := tx.Exec(ctx, `
UPDATE issues
   SET previous_assignees = array_remove(previous_assignees, $2::uuid)
 WHERE id = $1
   AND $2::uuid = ANY(previous_assignees)`,
					id,
					newAssigneeIdentityForChain,
				); err != nil {
					return fmt.Errorf("updating delegation chain (prune new): %w", err)
				}
			}
		}

		// R3.2b hybrid event write: one issue.updated parent (full diff in
		// payload.changes) plus N issue.field_changed children (one per
		// changed field). The eventbus helper writes all rows on the same
		// tx so the parent + children + the UPDATE land atomically.
		// Force=true callers see reason="force-overridden" on every row so
		// audit dashboards can flag admin overrides without inspecting
		// old/new.
		eventReason := ""
		if input.Force {
			eventReason = "force-overridden"
		}
		changes := eventbus.DiffRegistry(IssuePatchFields, old, &input)

		// Auto-watch on assignee change must run BEFORE FanOut so the new
		// assignee appears in the recipient set of the parent event
		// (typical UX: "you've been assigned demo-42"). Pre-R3.2b this
		// lived inside the per-event branch; with the hybrid layout we
		// pull it up to the change-list scan. We reuse the resolved
		// newAssigneeUserID computed pre-UPDATE for the acceptance
		// reset so the patch path resolves the assignee at most once.
		assigneeAutoWatched := false
		for _, fc := range changes {
			if fc.Field != "assignee" {
				continue
			}
			newAssignee, _ := fc.NewValue.(string)
			if newAssignee == "" {
				break
			}
			if newAssigneeUserID != uuid.Nil {
				if err := notifications.AddIssueWatcher(ctx, tx, id, newAssigneeUserID, notifications.ReasonAssignee); err != nil {
					return err
				}
				assigneeAutoWatched = true
			}
			break
		}

		// sk-303.13 / T021: a self-assign patch that moved the acceptance
		// stamp from empty to filled writes its own audit event, so no path
		// stamps acceptance silently (INV-5). Journal-only: the same patch
		// already notifies through issue.updated / "Assigned issue", and
		// a second notification for one user action is what INV-6 forbids.
		// Written BEFORE the update events so the journal reads in the order
		// the writes happened.
		if patchAcceptance.WriteEvent {
			if _, err := WriteIssueEvent(ctx, tx, EventIssueAccepted, id, projectID,
				actor, "accepted_at",
				map[string]any{"acceptedBy": nil, "acceptedAt": nil},
				map[string]any{"acceptedBy": actor.UserID, "acceptedAt": patchAcceptedAt},
				acceptanceReasonAuto); err != nil {
				return err
			}
		}

		if len(changes) > 0 {
			parentEventID, parentPayload, err := eventbus.WriteHybridUpdateEvents(
				ctx, tx,
				EventIssueUpdated, EventIssueFieldChanged,
				"issue", id, &projectID,
				eventbusActor(actor),
				changes,
				eventReason,
			)
			if err != nil {
				return err
			}
			// FanOut consumes the parent's payload (changes[]) to decide
			// whether issue.updated is notifiable. WriteHybridUpdateEvents
			// returns the exact bytes that were written into events.payload,
			// so subscribers and the gate see the same shape.
			patchCreator, patchAssignee, err := roleContextOf(ctx, tx, old)
			if err != nil {
				return err
			}
			if newAssigneeUserID != uuid.Nil {
				// The patch itself installed the assignee, so the post-state
				// is what the audiences must read.
				patchAssignee = newAssigneeUserID
			}
			if _, err := notifications.FanOut(ctx, tx, notifications.FanOutInput{
				EventID:           parentEventID,
				EventType:         EventIssueUpdated,
				EntityType:        "issue",
				IssueID:           id,
				ProjectID:         projectID,
				CreatorUserID:     patchCreator,
				AssigneeUserID:    patchAssignee,
				NewAssigneeUserID: newAssigneeUserID,
				ActorUserID:       actor.UserID,
				ActorType:         actor.Role,
				Payload:           parentPayload,
			}); err != nil {
				return err
			}

		}

		// sk-622.21, decisions R23 / R23′ (US-2 acceptance: on a closed issue
		// no user with «unwatch on close» on keeps an automatic subscription
		// reason). An assignment on an ALREADY closed issue goes the way of
		// the close itself: the new assignee is subscribed above, the
		// assignment is fanned out — so they get its feed row with the
		// Watching card, the last notification the automatic subscription
		// brings — and then, in this same transaction, the automatic reasons
		// of everybody with the setting on are stripped
		// (StripAutoWatchersOnClose: an automatic-only row is deleted, a manual
		// one keeps only `manual`). At commit the invariant holds; with the
		// setting off the subscription stays, as before. A later reopen
		// subscribes the assignee again (reAddAutoWatchersOnReopen).
		if assigneeAutoWatched && old.Status == StatusClosed {
			if _, err := notifications.StripAutoWatchersOnClose(ctx, tx, id); err != nil {
				return err
			}
		}

		// sk-489 (US-3): an outcome change is its own event and its own fan-out, by the same
		// helper as in the close writers. Passing the same
		// outcome again (X = X) is NOT a change: no event, no notification —
		// only the link, if a target is passed.
		if decision.Changed {
			if err := writeOutcomeChangedInTx(ctx, tx, id, old, actor, decision, eventReason); err != nil {
				return err
			}
		}
		if target := strings.TrimSpace(input.CloseTarget); target != "" {
			state, lerr := linkCloseTargetInTx(ctx, tx, id, target,
				decision.Outcome, *input.DependenciesCaller, actor)
			if lerr != nil {
				if errors.Is(lerr, errCloseLinkRaceLost) {
					return &ErrVersionConflict{CurrentVersion: old.Version}
				}
				return lerr
			}
			linkState = state
		}

		// R4.3 — re-scan the four text fields and notify only on
		// newly-introduced @username mentions. PatchIssue is the only
		// path where text fields can grow new mentions; the diff against
		// the old values (captured pre-UPDATE in `old`) ensures we
		// don't re-fire on every PATCH that touches an unrelated field.
		// Plan §F-9.
		//
		// The new text per field is "input.X if non-nil else old.X" —
		// PatchIssueInput is partial, COALESCE in the UPDATE statement
		// preserved unset fields. Resolve runs against the post-write
		// state so the membership snapshot is consistent with the
		// notifications we emit.
		if patchTouchesMentionFields(input) {
			oldTexts := IssueMentionTexts{
				Description:        old.Description,
				Notes:              old.Notes,
				AcceptanceCriteria: old.AcceptanceCriteria,
				Design:             old.Design,
			}
			newTexts := IssueMentionTexts{
				Description:        coalesceString(input.Description, old.Description),
				Notes:              coalesceString(input.Notes, old.Notes),
				AcceptanceCriteria: coalesceString(input.AcceptanceCriteria, old.AcceptanceCriteria),
				Design:             coalesceString(input.Design, old.Design),
			}
			if err := HandleIssueMentions(ctx, tx, id, projectID, actor, newTexts, &oldTexts); err != nil {
				return err
			}
		}

		// R1.5 search: any change to a text field that contributes to
		// the embedding source invalidates the row's vector. Resetting
		// embedding=NULL inside the same tx is the only way to keep the
		// invariant "row.embedding ⇔ Build*EmbedText(row.cols)". The
		// backfill worker picks the row up on its next tick (typically
		// 5..10 seconds).
		if embeddingDirtyFromPatch(old, input) {
			if err := search.ResetEmbedding(ctx, tx, "issues", "id", id); err != nil {
				return err
			}
		}
		// sk-622.21 T015 (US-11): personal signal addressees of the patch.
		return signalPatch(ctx, tx, id, changes, rolesBefore, decision.Changed)
	}))
	if err != nil {
		// SERIALIZABLE serialization failure → re-read the live version
		// and surface it as ErrVersionConflict so callers can retry on
		// the bumped value rather than seeing an opaque internal error.
		return CommittedState{}, CloseLinkNone, s.degradeSerializationAbort(ctx, err, "patchIssue", id, &input.ExpectedVersion)
	}
	afterCommit(ctx, "patch", id)
	return committed, linkState, nil
}

// currentVersion best-effort reads the live version for a follow-up
// ErrVersionConflict payload. Errors are swallowed — the caller will get
// a 0 currentVersion which still beats no info.
func (s *Store) currentVersion(ctx context.Context, id string) (int, error) {
	var v int
	err := s.pool.QueryRow(ctx, `SELECT version FROM issues WHERE id = $1`, id).Scan(&v)
	return v, err
}

// degradeSerializationAbort is the shared fallback that turns a class-40
// abort of a SERIALIZABLE write path into version_conflict. Since sk-622.21
// (R17) it is reached in two ways: on the paths still without a retry
// wrapper (ReplaceComponents, AttachCommits, DetachCommits, ResumeDelegated)
// a single 40001 lands here; on PatchIssue, UpdateStatus, Close, AddLabel,
// RemoveLabel, Accept and DismissDelegated, which replay through
// withSerializableRetry, only an EXHAUSTED retry budget does — so for them
// versionConflictOriginTotal counts budget exhaustion, not a lone abort (see
// its doc comment in metrics.go). Behaviour is UNCHANGED from before
// sk-354: any error that is not a class-40 abort passes through verbatim;
// a class-40 abort still degrades to the exact same
// &ErrVersionConflict{CurrentVersion: cv} every caller of these ten
// already received. sk-354's only addition is classifying and counting
// that degradation via versionConflictOriginTotal — consolidated here,
// once, instead of duplicated at each of the ten call sites, so the
// classification is unit-tested in one place (classifyVersionConflictOrigin)
// rather than trusted to be copied correctly ten times.
//
// expectedVersion is the caller-declared version to compare the freshly
// re-read live version against, or nil when the operation's input has no
// such field at all (ReplaceComponents / AddLabel / RemoveLabel /
// AttachCommits / DismissDelegated / ResumeDelegated never took one;
// Accept's is optional and a nil here also covers a caller who omitted
// it) — see classifyVersionConflictOrigin.
//
// The live-version read reuses currentVersion, which best-effort swallows
// its own read error and returns 0 — the exact same value this function
// already handed callers as CurrentVersion before sk-354 existed, so a
// failed read does not introduce a new failure mode, only (rarely) an
// inaccurate origin label on top of an already-approximate currentVersion.
func (s *Store) degradeSerializationAbort(ctx context.Context, err error, op, id string, expectedVersion *int) error {
	if !isSerializationFailure(err) {
		return err
	}
	cv, _ := s.currentVersion(ctx, id)
	origin := classifyVersionConflictOrigin(cv, expectedVersion)
	versionConflictOriginTotal.WithLabelValues(op, origin).Inc()
	return &ErrVersionConflict{CurrentVersion: cv}
}

// patchTouchesMentionFields reports whether the PATCH input modifies any
// of the four text fields scanned for @username mentions
// (description, notes, acceptance_criteria, design). Fast-path guard:
// when no mention-bearing field is touched we skip the parse +
// resolve work entirely.
//
// Note: we check input != nil (the caller provided the field), not
// "value differs from old". A PATCH with description set to its
// existing value would re-run the parse/resolve, but Diff() against
// old text returns the empty set so no notifications fire — slightly
// wasteful in CPU, but keeps the contract simple.
func patchTouchesMentionFields(input PatchIssueInput) bool {
	return input.Description != nil ||
		input.Notes != nil ||
		input.AcceptanceCriteria != nil ||
		input.Design != nil
}

// coalesceString picks the patch value when non-nil (caller is updating
// the field) or falls back to the old DB value (field unchanged in
// this PATCH). Equivalent to SQL `COALESCE($newOrNull, oldValue)`.
func coalesceString(patched *string, old string) string {
	if patched == nil {
		return old
	}
	return *patched
}

// embeddingDirtyFromPatch reports whether the patch input changes any
// text field that contributes to BuildIssueEmbedText. Currently only
// title / description / acceptance_criteria are folded into the embed
// input (design + notes are FTS-only — see search/text.go), so we don't
// reset on those.  Add fields here in lockstep with text.go.
func embeddingDirtyFromPatch(old *issueRow, input PatchIssueInput) bool {
	if input.Title != nil && *input.Title != old.Title {
		return true
	}
	if input.Description != nil && *input.Description != old.Description {
		return true
	}
	if input.AcceptanceCriteria != nil && *input.AcceptanceCriteria != old.AcceptanceCriteria {
		return true
	}
	return false
}

// ----------------- Status / Pin -----------------

// UpdateStatus applies a status transition with required If-Match version
// match. Auto-fills started_at on transition → in_progress and closed_at /
// closed_reason on transition → closed.
//
// Pinned protection: closing a pinned issue requires force=true (vision
// F09/F118). Force=true ALSO bypasses the lock check (R2.3 — admin
// override). For status='closed' the new tool layer (skein_close) routes
// through Close() directly, but legacy callers using
// skein_update(status=closed) and the REST endpoint still pass through
// here — UpdateStatus emits issue.status_changed and (when transitioning
// to closed) clears the lock + writes closed_reason.
//
// R2.3 semantics: when newStatus=='closed' the lock IS auto-cleared and
// an issue.unclaimed event is written if the issue was previously
// locked. Other transitions (open, in_progress, blocked, deferred) keep
// the lock as-is — the holder explicitly drops it via Unclaim.
func (s *Store) UpdateStatus(
	ctx context.Context,
	id string,
	input UpdateStatusInput,
	actor Actor,
) (*IssueDetail, error) {
	state, err := s.UpdateStatusCommitted(ctx, id, input, actor)
	if err != nil {
		return nil, err
	}
	// sk-607: committed — read back detached from the caller's context; a
	// failed read-back is *ErrReadBackFailed carrying the committed state.
	details, err := s.readBack(ctx, "status", []CommittedState{state})
	if err != nil {
		return nil, err
	}
	return details[0], nil
}

// UpdateStatusCommitted is the status writer without the read-back of the
// card (sk-607) — see PatchIssueCommitted for why the update chain wants it.
func (s *Store) UpdateStatusCommitted(
	ctx context.Context,
	id string,
	input UpdateStatusInput,
	actor Actor,
) (CommittedState, error) {
	var committed CommittedState
	if input.ExpectedVersion < 1 {
		return CommittedState{}, &ValidationError{Code: "missing_version", Message: "expected version is required"}
	}
	if input.Reason != nil {
		if err := ValidateProse("reason", *input.Reason); err != nil {
			return CommittedState{}, err
		}
	}
	newStatus, err := ValidateStatus(input.Status)
	if err != nil {
		return CommittedState{}, err
	}
	// sk-489 (US-1): the outcome is a property of the CLOSE. Passed together with a
	// status other than "closed", it is rejected by a check: it cannot be
	// silently swallowed (the caller is sure the outcome was applied), and there is
	// nothing to write it to, an unclosed issue has no outcome (INV-1).
	//
	// This rejection has its OWN code, different from close_outcome_requires_closed,
	// which an outcome edit on an unclosed issue returns
	// (Store.PatchIssueWithResult). The rejections differ in meaning, and what
	// makes them different is not taste but the organisation's error classification
	// (the project guidelines, "HTTP codes for business errors"): here the request
	// itself carries an impossible combination — "move to in_progress and here is
	// a close outcome" — that is a FORM error, 400; there the request
	// is correct, and it is forbidden by the issue's current STATE (not yet
	// closed), that is a business rejection, 422. One code for both cases would mean
	// one HTTP status for both, while the REST surface (T007) must
	// answer differently: 400 on a transition and 422 on an edit. Hence the rule
	// for the reader: this code is NOT added to handler/issues.go#issueValidationIsBusinessError,
	// while close_outcome_requires_closed is.
	if newStatus != StatusClosed &&
		(input.CloseOutcome != nil || strings.TrimSpace(input.CloseTarget) != "") {
		return CommittedState{}, newValidationError("close_outcome_not_a_close",
			"closeOutcome and closeTarget are only accepted when the new status is "+StatusClosed)
	}
	if strings.TrimSpace(input.CloseTarget) != "" && input.DependenciesCaller == nil {
		return CommittedState{}, fmt.Errorf("DependenciesCaller is required when CloseTarget is set")
	}

	err = s.withSerializableRetry(ctx, "updateStatus", updateStatusMaxAttempts, withCommittedState(ctx, id, &committed, func(tx pgx.Tx) error {
		old, err := s.fetchIssueInTx(ctx, tx, id)
		if err != nil {
			return err
		}
		// Lock predicate first — same ordering as PatchIssue.
		if err := lockGuard(old, input.SessionID, actor.UserID, input.Force); err != nil {
			return err
		}
		if !input.Force && old.Version != input.ExpectedVersion {
			return &ErrVersionConflict{CurrentVersion: old.Version}
		}

		// Pinned protection on close.
		if newStatus == StatusClosed && old.Pinned && !input.Force {
			return ErrPinnedNoForce
		}

		// sk-118 (F2c): standing transition matrix. UpdateStatus is the
		// sole promote/demote path for the `standing` status (Claim/Close
		// have their own dedicated guards; create-as-standing goes through
		// CreateIssue). The matrix enforced here:
		//
		//   TO standing (promote / reopen-as-standing):
		//     open      → standing   ✓
		//     deferred  → standing   ✓
		//     closed    → standing   ✓  (reopen; fromClosed clears
		//                                closed_at + closed_reason below)
		//     in_progress → standing ✗  (a claimed issue is locked — the
		//                                holder must unclaim first; even an
		//                                unlocked in_progress is rejected so
		//                                the directive is uniform)
		//     standing  → standing   ✓  (idempotent self-promote; harmless)
		//
		//   FROM standing (demote — issue becomes claimable again):
		//     standing → open        ✓  (the sole escape hatch / demote)
		//     standing → in_progress ✗  (you don't WORK a standing task;
		//                                spawn a child and work that)
		//     standing → deferred    ✗  (a perpetual obligation can't be
		//                                parked; demote to open first)
		//     standing → closed      ✓  (retire-close — but routed through
		//                                Close(), never here; UpdateStatus
		//                                rejects nothing on this leg)
		//
		// Force does NOT bypass this matrix: the rejected legs are not
		// permission checks, they are structural invariants (a locked issue
		// genuinely still holds a lock; standing genuinely isn't worked
		// directly). Admins who must override unclaim/close first.
		if newStatus == StatusStanding && old.Status == StatusInProgress {
			// Promote of an in_progress issue. Reject regardless of whether
			// the lock is currently held — the directive is always "unclaim
			// first". (A locked issue would also trip lockGuard above unless
			// the caller holds the lock; this guard catches the held-lock
			// and the lock-already-dropped-but-still-in_progress cases.)
			return ErrCannotPromoteLockedToStanding
		}
		if old.Status == StatusStanding && newStatus == StatusInProgress {
			// Demote-to-work is not a thing: standing tasks are acted on via
			// spawned children, never claimed/worked in place.
			return ErrCannotWorkStandingDirectly
		}
		if old.Status == StatusStanding && newStatus == StatusDeferred {
			// A standing task is a perpetual obligation, not a parkable unit of
			// work — deferring it is nonsensical (§11 / §6 invariant 9). The
			// only demote is standing → open; defer from there if desired.
			return ErrCannotDeferStanding
		}

		// R10 (CRITICAL): nested-standing guard on the PROMOTE path. On ANY
		// transition INTO standing, reject if the issue already participates
		// in a standing parent/child relationship — in EITHER representation
		// (the frozen issues.parent_id column OR a parent-child dep edge).
		// UpdateStatus is otherwise hierarchy-blind, so without this a
		// skein_update(status=standing) could promote a child under a
		// standing parent (or a parent that already has a standing child),
		// reintroducing the §11.4 mutual-retire deadlock that the
		// create/reparent/dep-add guards close on their own paths.
		if newStatus == StatusStanding && old.Status != StatusStanding {
			hasStandingRel, err := standingRelationExists(ctx, tx, id)
			if err != nil {
				return err
			}
			if hasStandingRel {
				return ErrNestedStandingForbidden
			}
		}

		// sk-118 (F2e): standing retire-block on the SHARED close path.
		// UpdateStatus is the close path used by the REST/MCP bulk handler
		// (BulkHandler.runOne routes action:"close"/"archive" through
		// UpdateStatus(StatusClosed), NOT Store.Close), so the F2e retire
		// guard must live here as well — otherwise bulk close/archive could
		// retire a standing task while non-closed children remain, defeating
		// the structural invariant. We run the SAME check Close() uses:
		// standingOpenChildIDs covers BOTH hierarchy representations (the
		// canonical parent-child dep edge AND the legacy issues.parent_id
		// column) via status.ActiveWork. An empty standing (no children, or
		// all children closed) retires fine. Like the nested-standing matrix
		// above and Close()'s own guard, this is a structural invariant — NOT
		// bypassed by force. Non-standing closes are untouched.
		if old.Status == StatusStanding && newStatus == StatusClosed {
			childIDs, cerr := standingOpenChildIDs(ctx, tx, id)
			if cerr != nil {
				return cerr
			}
			if len(childIDs) > 0 {
				return newStandingHasOpenChildren(childIDs)
			}
		}

		// Compute the auto-managed timestamp side-effects per the spec:
		//
		//  - started_at: set on first transition into in_progress; we don't
		//    overwrite if it's already present (a re-open → in_progress
		//    keeps the original start time, which matches "started" intent).
		//  - closed_at: required when status='closed' (CHECK constraint),
		//    cleared on any other status.
		//  - closed_reason: stored when caller provided one on a close
		//    transition; cleared when leaving closed.
		startedNow := old.StartedAt == nil && newStatus == StatusInProgress
		toClosed := newStatus == StatusClosed
		fromClosed := old.Status == StatusClosed && newStatus != StatusClosed

		// sk-303.14 / T009: resetting the review mark is a property of
		// the TRANSITION into closed, not of the status value.
		// `toClosed` will not do: it is also true on closing an already-
		// closed issue, where the spec requires the opposite (SC-023,
		// edge-case table). The second closing path — Store.Close —
		// computes the same predicate by the same rule; both must match,
		// otherwise "every close path yields one semantics" stops being
		// true.
		//
		// The reverse transition (`fromClosed`, a reopen) does NOT
		// touch the mark on purpose: on a reopened and not-yet-closed
		// issue the previous mark is kept until the next close. Such an
		// issue is not in the unreviewed set — the set counts only
		// closed results — and T028 hides it from the card.
		resetReview := toClosed && old.Status != StatusClosed

		// sk-489 (T003): the same outcome helper as in the second close
		// writer — Store.Close. The one input present here and not there is
		// Archive (sk-540.4): archiving goes through this writer
		// (handler/bulk.go#runOne). Deciding it HERE, on the row read inside
		// the transaction, is what refuses a row reopened after the batch
		// preflight instead of archiving it without an outcome.
		decision := CloseOutcomeDecision{}
		if toClosed {
			previousOutcome := ""
			if old.CloseOutcome != nil {
				previousOutcome = *old.CloseOutcome
			}
			var derr error
			decision, derr = DecideCloseOutcome(CloseOutcomeRequest{
				IssueID:         id,
				Requested:       input.CloseOutcome,
				Target:          input.CloseTarget,
				AlreadyClosed:   old.Status == StatusClosed,
				PreviousOutcome: previousOutcome,
				Archive:         input.Archive,
				Standing:        old.Status == StatusStanding,
			})
			if derr != nil {
				return derr
			}
			// sk-540.2 exception (a): before any write.
			if err := ensureCloseLinkExists(ctx, tx, id, decision); err != nil {
				return err
			}
			// Branch (2) of the review-mark reset predicate — see Store.Close.
			resetReview = resetReview ||
				OutcomeChangeResetsReview(decision.Previous, decision.Outcome,
					actor.UserID, old.CreatedBy)
		}
		// Leaving "closed" erases the outcome with an explicit NULL (US-4). Trigger
		// 000050 would do it by itself, but the writer must assert
		// the invariant, not rely on the schema's safety net.
		var outcomeArg any
		if toClosed {
			outcomeArg = decision.Outcome
		}

		var reasonArg any
		if toClosed && input.Reason != nil {
			reasonArg = *input.Reason
		}

		// On close transition we auto-clear locked_by/locked_at so the
		// issue is not "stuck" locked after the holder finishes work.
		// Other transitions keep the lock — agents must explicitly drop
		// it via Unclaim.
		ct, err := tx.Exec(ctx, `
UPDATE issues
   SET status        = $2,
       started_at    = CASE WHEN $3::boolean THEN now() ELSE started_at END,
       closed_at     = CASE WHEN $4::boolean THEN now()
                            WHEN $5::boolean THEN NULL
                            ELSE closed_at END,
       closed_reason = CASE WHEN $4::boolean THEN $6
                            WHEN $5::boolean THEN NULL
                            ELSE closed_reason END,
       close_outcome = CASE WHEN $4::boolean THEN $12::text
                            WHEN $5::boolean THEN NULL
                            ELSE close_outcome END,
       locked_by     = CASE WHEN $4::boolean THEN NULL ELSE locked_by END,
       locked_at     = CASE WHEN $4::boolean THEN NULL ELSE locked_at END,
       locked_by_user_id = CASE WHEN $4::boolean THEN NULL ELSE locked_by_user_id END,
       reviewed_at   = CASE WHEN $10::boolean THEN NULL ELSE reviewed_at END,
       reviewed_by   = CASE WHEN $10::boolean THEN NULL ELSE reviewed_by END,
       version       = version + 1,
       updated_at    = now()
 WHERE id = $1
   AND ($8::boolean OR version = $7)
   AND ($8::boolean OR locked_by IS NULL
        OR (locked_by = $9::text AND locked_by_user_id = $11::uuid))`,
			id, newStatus,
			startedNow, toClosed, fromClosed, reasonArg, input.ExpectedVersion,
			input.Force, input.SessionID, resetReview,
			actor.UserID, outcomeArg,
		)
		if err != nil {
			return fmt.Errorf("updating status: %w", err)
		}
		if ct.RowsAffected() == 0 {
			return &ErrVersionConflict{CurrentVersion: old.Version}
		}

		reasonStr := ""
		if input.Reason != nil {
			reasonStr = *input.Reason
		}
		// Force=true callers see reason="force-overridden" appended to the
		// status_changed reason so the audit row records the override even
		// when the caller passed an explicit reason.
		statusEventReason := buildForceReason(reasonStr, input.Force)
		// sk-489: on a closing transition both close events carry the outcome
		// in the payload; on all other transitions the payload is empty, as it was.
		var statusPayload map[string]any
		var statusPayloadJSON []byte
		if toClosed {
			statusPayload = closeOutcomePayload(decision)
			statusPayloadJSON, err = json.Marshal(statusPayload)
			if err != nil {
				return fmt.Errorf("marshalling close payload for %s: %w", id, err)
			}
		}
		eventID, err := WriteIssueEventWithPayload(ctx, tx, EventIssueStatusChanged, id, old.ProjectID,
			actor, "status", old.Status, newStatus, statusEventReason, statusPayload)
		if err != nil {
			return err
		}
		// sk-622.21 (US-2, US-6 "reopened"): a reopen gives the
		// automatic subscriptions back BEFORE the fan-out below, so the
		// people who come back receive the reopen notification as
		// subscribers, with its Watching card. The opt-out check rides on
		// the subscription primitive (AddIssueWatcherScoped): a person who
		// explicitly unwatched this issue is not re-subscribed.
		if fromClosed {
			if err := reAddAutoWatchersOnReopen(ctx, tx, old); err != nil {
				return err
			}
		}
		statusCreator, statusAssignee, err := roleContextOf(ctx, tx, old)
		if err != nil {
			return err
		}
		if _, err := notifications.FanOut(ctx, tx, notifications.FanOutInput{
			EventID:    eventID,
			EventType:  EventIssueStatusChanged,
			EntityType: "issue",
			IssueID:    id,
			ProjectID:  old.ProjectID,
			Payload:    statusPayloadJSON,
			// "Status of a delegated issue": the feed reaches every
			// subscriber, the push reaches the creator and only when the task
			// is genuinely delegated — hence both ids.
			CreatorUserID:  statusCreator,
			AssigneeUserID: statusAssignee,
			ActorUserID:    actor.UserID,
			ActorType:      actor.Role,
		}); err != nil {
			return err
		}

		// R2.3: closing an issue auto-clears its lock. Emit issue.closed
		// (so subscribers can filter the close-event channel without
		// pattern-matching status_changed) AND issue.unclaimed (when the
		// issue was previously locked) for symmetry with skein_unclaim.
		// sk-551.10, item 5 / sk-622.21 US-2 — the personal «unwatch on
		// close» setting (on by default): closing strips the automatic
		// subscriptions of the people who have it on, AFTER the fan-out
		// above, so they still learn of the close and its notification
		// keeps its Watching card (the card becomes historical). No
		// opt-out is written and no card is cleared. Reopening gives the
		// subscriptions back — before the fan-out, see above.
		if toClosed {
			if _, err := notifications.StripAutoWatchersOnClose(ctx, tx, id); err != nil {
				return err
			}
		}

		if toClosed {
			closedReason := buildForceReason(reasonStr, input.Force)
			if _, err := WriteIssueEventWithPayload(ctx, tx, EventIssueClosed, id, old.ProjectID,
				actor, "status", old.Status, newStatus, closedReason, statusPayload); err != nil {
				return err
			}
			// A repeated close that changed the outcome (US-3) — the same event
			// and fan-out helper as in Store.Close.
			if decision.Changed {
				if err := writeOutcomeChangedInTx(ctx, tx, id, old, actor, decision, closedReason); err != nil {
					return err
				}
			}
			if old.LockedBy != nil && *old.LockedBy != "" {
				unclaimReason := "auto-unclaimed-on-close"
				if input.Force {
					unclaimReason = "force-overridden"
				}
				if _, err := WriteIssueEvent(ctx, tx, EventIssueUnclaimed, id, old.ProjectID,
					actor, "lockedBy",
					map[string]any{"lockedBy": *old.LockedBy, "lockedAt": old.LockedAt},
					map[string]any{"lockedBy": nil, "lockedAt": nil},
					unclaimReason); err != nil {
					return err
				}
				// sk-674.18 T034: a forced close releases another holder's claim.
				if forceReleasesAnother(old, actor, input.Force) {
					if err := writeForceReleased(ctx, tx, id, old, actor, unclaimReason); err != nil {
						return err
					}
				}
			}

			// The link by target — in the same transaction (R-5), by the same helper
			// as in Store.Close. This writer does not hand the link flag outward:
			// confirmations with it (US-2) are built by MCP, and MCP
			// closes through Store.CloseWithResult — the REST transition response
			// is the issue card anyway.
			//
			// sk-540.4: an archive of an already closed or standing row does
			// not take the batch's target (decision.TargetDropped).
			if target := strings.TrimSpace(input.CloseTarget); target != "" && !decision.TargetDropped {
				if _, lerr := linkCloseTargetInTx(ctx, tx, id, target,
					decision.Outcome, *input.DependenciesCaller, actor); lerr != nil {
					if errors.Is(lerr, errCloseLinkRaceLost) {
						return &ErrVersionConflict{CurrentVersion: old.Version}
					}
					return lerr
				}
			}
		}
		// sk-622.21 T015 (US-11): "Status change / close / reopen /
		// outcome". A status transition never changes the roles, so the
		// post-write read is both "before" and "after".
		return signalStatusChanged(ctx, tx, id, signalRoles{})
	}))
	if err != nil {
		return CommittedState{}, s.degradeSerializationAbort(ctx, err, "updateStatus", id, &input.ExpectedVersion)
	}
	afterCommit(ctx, "status", id)
	return committed, nil
}

// PinIssue sets pinned=true. Optimistic concurrency on version. Idempotent
// at the wire level — pinning an already-pinned issue still bumps version
// and emits an event so the audit log stays linear.
func (s *Store) Pin(ctx context.Context, id string, expectedVersion int, actor Actor) (*IssueDetail, error) {
	return s.setPinned(ctx, id, true, expectedVersion, actor)
}

func (s *Store) Unpin(ctx context.Context, id string, expectedVersion int, actor Actor) (*IssueDetail, error) {
	return s.setPinned(ctx, id, false, expectedVersion, actor)
}

// pinMaxAttempts bounds how many times setPinned replays its transaction
// against a retryable abort (sk-303.17).
//
// The contention here is the same SHAPE as Claim's, not CreateIssue's:
// setPinned reads-then-updates a SINGLE issue row (fetchIssueInTx →
// UPDATE ... WHERE id = $1 AND version = $3), and the realistic
// competitor is one other writer touching that same row concurrently
// (a label/comment/claim landing between the SELECT and the UPDATE) —
// not a structural fan-out through a shared counter row that N siblings
// must all advance. That is exactly claimMaxAttempts's contention
// profile, so it gets the same budget — matched because the mechanism
// is the same, not copied on the assumption that neighbours share a
// number. TestSetPinned_SerializationFailure_Retries pins a single
// parked competitor and is green at this budget.
const pinMaxAttempts = claimMaxAttempts

func (s *Store) setPinned(ctx context.Context, id string, pinned bool, expectedVersion int, actor Actor) (*IssueDetail, error) {
	// Pin / Unpin bypass the lock guard intentionally — pin state is an
	// admin/ops control surface, not part of the agent's normal write flow.
	// This is a deliberate scope decision about lock gating (Q4), not an
	// oversight.
	if expectedVersion < 1 {
		return nil, &ValidationError{Code: "missing_version", Message: "expected version is required"}
	}
	// sk-303.17: replay on a class-40 abort instead of leaking a raw
	// pgError to the caller as internal_error. The closure only assigns
	// to captured locals (old, ct) that a replay overwrites in full — no
	// side effects escape the transaction (see withSerializableRetry's
	// doc comment for the safety argument).
	err := s.withSerializableRetry(ctx, "setPinned", pinMaxAttempts, func(tx pgx.Tx) error {
		old, err := s.fetchIssueInTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if old.Version != expectedVersion {
			return &ErrVersionConflict{CurrentVersion: old.Version}
		}
		ct, err := tx.Exec(ctx, `
UPDATE issues SET pinned = $2, version = version + 1, updated_at = now()
 WHERE id = $1 AND version = $3`, id, pinned, expectedVersion)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return &ErrVersionConflict{CurrentVersion: old.Version}
		}
		evt := EventIssuePinned
		if !pinned {
			evt = EventIssueUnpinned
		}
		_, err = WriteIssueEvent(ctx, tx, evt, id, old.ProjectID, actor,
			"pinned", old.Pinned, pinned, "")
		if err != nil {
			return err
		}
		// sk-622.21 T015 (US-11): "Row field change".
		return signalRowFields(ctx, tx, id)
	})
	if err != nil {
		// Exhausted the retry budget on a genuine abort — degrade to the
		// same business error every other write path in this file uses
		// (ErrVersionConflict with the live version), not a raw
		// internal_error. Mirrors PatchIssue / ReplaceComponents / etc.
		if isSerializationFailure(err) {
			cv, _ := s.currentVersion(ctx, id)
			return nil, &ErrVersionConflict{CurrentVersion: cv}
		}
		return nil, err
	}
	return s.GetIssue(ctx, id)
}

// ----------------- Components junction -----------------

// ReplaceComponents atomically rewrites the issue_components rows to match
// the supplied set. Validates components are in the same project before
// touching the junction. Writes one components_changed event with the
// before/after diff so subscribers don't have to subtract two events.
//
// R2.3: opts honours the lock predicate — when opts.SessionID is set and
// the issue is locked by a different session, returns ErrLockedByOther
// unless opts.Force=true. Passing an empty LockOpts preserves the legacy
// R2.2 behaviour.
func (s *Store) ReplaceComponentsNoLoad(
	ctx context.Context,
	id string,
	componentIDs []uuid.UUID,
	actor Actor,
	opts LockOpts,
) (CommittedState, error) {
	// sk-607: the post-state rides out of the transaction — the update chain
	// takes the version a component write moved from here, not from a read
	// after the commit.
	var committed CommittedState
	err := signals.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, withCommittedState(ctx, id, &committed, func(tx pgx.Tx) error {
		old, err := s.fetchIssueInTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := lockGuard(old, opts.SessionID, actor.UserID, opts.Force); err != nil {
			return err
		}

		if len(componentIDs) > 0 {
			if err := assertComponentsInProject(ctx, tx, old.ProjectID, componentIDs); err != nil {
				return err
			}
		}

		// Read existing for the event payload.
		oldRows, err := tx.Query(ctx, `SELECT component_id FROM issue_components WHERE issue_id = $1`, id)
		if err != nil {
			return fmt.Errorf("loading existing components: %w", err)
		}
		oldComps := []uuid.UUID{}
		for oldRows.Next() {
			var c uuid.UUID
			if err := oldRows.Scan(&c); err != nil {
				oldRows.Close()
				return err
			}
			oldComps = append(oldComps, c)
		}
		oldRows.Close()
		if err := oldRows.Err(); err != nil {
			return err
		}

		// Delete-all + insert-new. The junction has no event row tying it to
		// version on issues, so we must bump issues.version too — otherwise
		// listing endpoints don't see the change in updated_at order.
		if _, err := tx.Exec(ctx, `DELETE FROM issue_components WHERE issue_id = $1`, id); err != nil {
			return fmt.Errorf("clearing components: %w", err)
		}
		for _, cid := range componentIDs {
			if _, err := tx.Exec(ctx, `
INSERT INTO issue_components (issue_id, component_id) VALUES ($1, $2)
ON CONFLICT DO NOTHING`, id, cid); err != nil {
				return fmt.Errorf("inserting component %s: %w", cid, err)
			}
		}

		// SQL-level lock-aware predicate as belt-and-suspenders against a
		// concurrent claim landing between the SELECT and our UPDATE.
		if _, err := tx.Exec(ctx, `
UPDATE issues SET version = version + 1, updated_at = now()
 WHERE id = $1
   AND ($2::boolean OR locked_by IS NULL
        OR (locked_by = $3::text AND locked_by_user_id = $4::uuid))`,
			id, opts.Force, opts.SessionID, actor.UserID); err != nil {
			return fmt.Errorf("bumping issue version: %w", err)
		}

		eventReason := ""
		if opts.Force {
			eventReason = "force-overridden"
		}
		_, err = WriteIssueEvent(ctx, tx, EventComponentsChanged, id, old.ProjectID, actor,
			"componentIds", oldComps, componentIDs, eventReason)
		if err != nil {
			return err
		}
		// sk-622.21 T015 (US-11): "Row field change".
		return signalRowFields(ctx, tx, id)
	}))
	if err != nil {
		return CommittedState{}, s.degradeSerializationAbort(ctx, err, "replaceComponents", id, nil)
	}
	return committed, nil
}

// ReplaceComponents applies the change and returns the post-state
// IssueDetail. The REST handler echoes that detail; the MCP update chain
// does not, and calls ReplaceComponentsNoLoad instead — the reload is seven
// database round-trips (the issue row plus six hydration queries) and was
// being discarded.
func (s *Store) ReplaceComponents(
	ctx context.Context,
	id string,
	componentIDs []uuid.UUID,
	actor Actor,
	opts LockOpts,
) (*IssueDetail, error) {
	if _, err := s.ReplaceComponentsNoLoad(ctx, id, componentIDs, actor, opts); err != nil {
		return nil, err
	}
	return s.GetIssue(ctx, id)
}

// ----------------- Labels -----------------

// AddLabelNoLoad inserts (issue_id, label) idempotently and loads nothing
// back. Writes a label_added event ONLY when an INSERT actually happened —
// otherwise the audit log fills with no-op rows. Note what this means for a
// replay: the insert is a no-op, no event is written, and the issue's
// version does NOT move, so resending an applied labels-only row cannot be
// refused by an expectedVersion check.
//
// R2.3: opts honours the lock predicate — see ReplaceComponentsNoLoad.
func (s *Store) AddLabelNoLoad(ctx context.Context, id, label string, actor Actor, opts LockOpts) (CommittedState, error) {
	if _, err := ValidateLabel(label); err != nil {
		return CommittedState{}, err
	}
	// sk-607: the post-state rides out of the transaction (see
	// ReplaceComponentsNoLoad); a label write moves no version, but a
	// labels-only update still needs one to acknowledge.
	var committed CommittedState
	err := s.withSerializableRetry(ctx, "addLabel", addLabelMaxAttempts, withCommittedState(ctx, id, &committed, func(tx pgx.Tx) error {
		old, err := s.fetchIssueInTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := lockGuard(old, opts.SessionID, actor.UserID, opts.Force); err != nil {
			return err
		}
		ct, err := tx.Exec(ctx, `
INSERT INTO issue_labels (issue_id, label) VALUES ($1, $2)
ON CONFLICT DO NOTHING`, id, label)
		if err != nil {
			return fmt.Errorf("inserting label: %w", err)
		}
		if ct.RowsAffected() == 0 {
			return nil // already present — no event, no version bump
		}
		if _, err := tx.Exec(ctx, `
UPDATE issues SET updated_at = now() WHERE id = $1`, id); err != nil {
			return err
		}
		eventReason := ""
		if opts.Force {
			eventReason = "force-overridden"
		}
		_, err = WriteIssueEvent(ctx, tx, EventLabelAdded, id, old.ProjectID, actor,
			"label", nil, label, eventReason)
		if err != nil {
			return err
		}
		// sk-622.21 T015 (US-11): "Row field change".
		return signalRowFields(ctx, tx, id)
	}))
	if err != nil {
		return CommittedState{}, s.degradeSerializationAbort(ctx, err, "addLabel", id, nil)
	}
	return committed, nil
}

// AddLabel applies the change and returns the post-state IssueDetail. See
// ReplaceComponents for why the MCP update chain uses AddLabelNoLoad.
func (s *Store) AddLabel(ctx context.Context, id, label string, actor Actor, opts LockOpts) (*IssueDetail, error) {
	if _, err := s.AddLabelNoLoad(ctx, id, label, actor, opts); err != nil {
		return nil, err
	}
	return s.GetIssue(ctx, id)
}

// RemoveLabelNoLoad deletes (issue_id, label) and loads nothing back.
// Returns ErrNotFound when the row didn't exist; the handler maps that to
// 404, and a replay of an applied row lands there too — the label is
// already gone.
//
// R2.3: opts honours the lock predicate — see ReplaceComponentsNoLoad.
func (s *Store) RemoveLabelNoLoad(ctx context.Context, id, label string, actor Actor, opts LockOpts) (CommittedState, error) {
	if _, err := ValidateLabel(label); err != nil {
		return CommittedState{}, err
	}
	// sk-607: the post-state rides out of the transaction (see
	// ReplaceComponentsNoLoad); a label write moves no version, but a
	// labels-only update still needs one to acknowledge.
	var committed CommittedState
	err := s.withSerializableRetry(ctx, "removeLabel", removeLabelMaxAttempts, withCommittedState(ctx, id, &committed, func(tx pgx.Tx) error {
		old, err := s.fetchIssueInTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := lockGuard(old, opts.SessionID, actor.UserID, opts.Force); err != nil {
			return err
		}
		ct, err := tx.Exec(ctx, `
DELETE FROM issue_labels WHERE issue_id = $1 AND label = $2`, id, label)
		if err != nil {
			return fmt.Errorf("removing label: %w", err)
		}
		if ct.RowsAffected() == 0 {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, `
UPDATE issues SET updated_at = now() WHERE id = $1`, id); err != nil {
			return err
		}
		eventReason := ""
		if opts.Force {
			eventReason = "force-overridden"
		}
		_, err = WriteIssueEvent(ctx, tx, EventLabelRemoved, id, old.ProjectID, actor,
			"label", label, nil, eventReason)
		if err != nil {
			return err
		}
		// sk-622.21 T015 (US-11): "Row field change".
		return signalRowFields(ctx, tx, id)
	}))
	if err != nil {
		return CommittedState{}, s.degradeSerializationAbort(ctx, err, "removeLabel", id, nil)
	}
	return committed, nil
}

// RemoveLabel applies the change and returns the post-state IssueDetail.
// See ReplaceComponents for why the MCP update chain uses
// RemoveLabelNoLoad.
func (s *Store) RemoveLabel(ctx context.Context, id, label string, actor Actor, opts LockOpts) (*IssueDetail, error) {
	if _, err := s.RemoveLabelNoLoad(ctx, id, label, actor, opts); err != nil {
		return nil, err
	}
	return s.GetIssue(ctx, id)
}

// ListLabelsInProject returns DISTINCT labels used across the project's
// issues, with usage count. Drives the UI suggestion popover. We deliberately
// do NOT cache — the query is fast (label index) and the frontend needs the
// latest set, not a stale snapshot.
func (s *Store) ListLabelsInProject(ctx context.Context, projectID uuid.UUID) ([]LabelStat, error) {
	rows, err := s.pool.Query(ctx, `
SELECT il.label, count(*) AS n
  FROM issue_labels il
  JOIN issues i ON i.id = il.issue_id
 WHERE i.project_id = $1
 GROUP BY il.label
 ORDER BY n DESC, il.label ASC
 LIMIT 500`, projectID)
	if err != nil {
		return nil, fmt.Errorf("listing project labels: %w", err)
	}
	defer rows.Close()
	out := make([]LabelStat, 0)
	for rows.Next() {
		var s LabelStat
		if err := rows.Scan(&s.Label, &s.Count); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ----------------- Comments -----------------

// CommentAuthorTypeHuman / CommentAuthorTypeAgent are the legal values for
// the comments.author_type column (mirrors the DB CHECK). REST callers
// always pass Human; the MCP agent path uses Agent. We deliberately omit
// "system" — the only system-authored comments today are migration-time
// backfills, which write SQL directly.
const (
	CommentAuthorTypeHuman = "human"
	CommentAuthorTypeAgent = "agent"
)

// CommentAuthorKindHuman / CommentAuthorKindAgentDirected /
// CommentAuthorKindAgentAutonomous are the legal values for the
// comments.author_kind column (sk-16). Mirrors issues.source's three-way
// split — distinguishes the *intent* behind a comment from the
// *transport* (author_type). Cookie-session writes → Human. Agent writes
// → one of AgentDirected (the user told the agent to comment) or
// AgentAutonomous (handoff note / progress / side-quest). The choice is
// caller-supplied via the `kind` parameter on AddComment and validated
// against this enum + the DB CHECK.
const (
	CommentAuthorKindHuman           = "human"
	CommentAuthorKindAgentDirected   = "agent_directed"
	CommentAuthorKindAgentAutonomous = "agent_autonomous"
)

// AddComment inserts a comment row + writes comment.created event. The
// authorType parameter is the wire value for comments.author_type — pass
// CommentAuthorTypeHuman from REST handlers and CommentAuthorTypeAgent
// from the MCP agent path. Any other value is rejected at the DB CHECK.
//
// authorKind (sk-16) is the *intent* axis — Human / AgentDirected /
// AgentAutonomous. Cookie-session callers always pass Human; agent
// callers pass AgentDirected or AgentAutonomous based on the MCP/REST
// `kind` parameter. Validation parallels authorType.
func (s *Store) AddComment(
	ctx context.Context,
	issueID, body string,
	authorUserID uuid.UUID,
	authorName string,
	authorType string,
	authorKind string,
	actor Actor,
) (*CommentDTO, error) {
	body = strings.TrimSpace(body)
	if err := ValidateProse("body", body); err != nil {
		return nil, err
	}
	if body == "" {
		return nil, &ValidationError{Code: "invalid_body", Message: "comment body is required"}
	}
	if len(body) > 100000 {
		return nil, &ValidationError{Code: "invalid_body", Message: "comment body too long (>100k chars)"}
	}
	switch authorType {
	case CommentAuthorTypeHuman, CommentAuthorTypeAgent:
		// ok
	default:
		return nil, &ValidationError{
			Code:    "invalid_author_type",
			Message: "authorType must be 'human' or 'agent'",
		}
	}
	switch authorKind {
	case CommentAuthorKindHuman, CommentAuthorKindAgentDirected, CommentAuthorKindAgentAutonomous:
		// ok
	default:
		return nil, &ValidationError{
			Code:    "invalid_author_kind",
			Message: "authorKind must be 'human', 'agent_directed', or 'agent_autonomous'",
		}
	}

	commentID, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("generating comment id: %w", err)
	}

	// sk-303.17: replay on a class-40 abort. commentID is minted once
	// above and reused across attempts — safe because a retryable abort
	// rolls the WHOLE transaction back (nothing committed), so replaying
	// the INSERT with the same id is not a duplicate. dto is fully
	// re-populated by row.Scan on every attempt (every field, including
	// dto.Mentions at the end of the closure), so a replay leaves no
	// stale partial state from the losing attempt.
	//
	// Contention is real, not theoretical: N concurrent comments on one
	// issue that all @mention the SAME user contend on that user's
	// notification/mention bookkeeping — TestAddComment_ConcurrentMentions_
	// NoSerializationError reproduces 3-4 raw 40001s out of 40 concurrent
	// commenters with plain BeginTxFunc (measured, not assumed — a same-
	// issue run with DISTINCT mention targets produces zero). Before this
	// fix that abort had no isSerializationFailure fallback at all (unlike
	// PatchIssue / ReplaceComponents / etc.) and travelled to the caller as
	// a raw pgError, which the MCP/REST layers map to internal_error — the
	// exact sk-305 defect shape.
	var dto CommentDTO
	err = s.withSerializableRetry(ctx, "addComment", commentMentionMaxAttempts, func(tx pgx.Tx) error {
		// Verify issue exists + capture project_id for event payload.
		var projectID uuid.UUID
		err := tx.QueryRow(ctx, `SELECT project_id FROM issues WHERE id = $1`, issueID).Scan(&projectID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("loading issue for comment: %w", err)
		}

		row := tx.QueryRow(ctx, `
INSERT INTO comments (id, issue_id, author, author_user_id, author_type, author_kind, body, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, now(), now())
RETURNING id, issue_id, author, author_user_id, author_type, author_kind, body, created_at, updated_at, version, edited_at, deleted_at`,
			commentID, issueID, authorName, authorUserID, authorType, authorKind, body)
		var authorUUID *uuid.UUID
		if err := row.Scan(&dto.ID, &dto.IssueID, &dto.Author, &authorUUID,
			&dto.AuthorType, &dto.AuthorKind, &dto.Body, &dto.CreatedAt, &dto.UpdatedAt,
			&dto.Version, &dto.EditedAt, &dto.DeletedAt); err != nil {
			return fmt.Errorf("inserting comment: %w", err)
		}
		dto.AuthorUserID = authorUUID

		eventID, err := WriteCommentEvent(ctx, tx, EventCommentCreated, commentID, issueID, projectID, actor,
			nil, map[string]any{
				"id":      dto.ID,
				"issueId": dto.IssueID,
				"author":  dto.Author,
				"body":    dto.Body,
			}, "")
		if err != nil {
			return err
		}

		// NO auto-watch for the commenter (US-8 / R-7). The subscription
		// belongs to the OWNER OF THE API KEY, and comments are written by
		// their agents dozens of times a session — so "watch what you touched"
		// quietly subscribed a person to everything their agent walked past.
		// Auto-subscription now exists only for creating and for being
		// assigned; an explicit watch (the UI button, the agent tool) is
		// unaffected, and an explicit unwatch (its opt-out) keeps its force.
		//
		// This is deliberately not a setting: "nobody will find the
		// setting — the trap stays".

		// Fan-out the comment.created event to issue and project
		// watchers. Pass issueID (not the comment uuid) so the issue
		// watcher arm of FanOut joins correctly.
		if _, err := notifications.FanOut(ctx, tx, notifications.FanOutInput{
			EventID:     eventID,
			EventType:   EventCommentCreated,
			EntityType:  "comment",
			IssueID:     issueID,
			ProjectID:   projectID,
			ActorUserID: actor.UserID,
			ActorType:   actor.Role,
		}); err != nil {
			return err
		}

		// R4.3 — parse @username mentions in the comment body, resolve
		// against project membership (admin bypass), auto-watch each
		// resolved user with reason='mention', and write directed
		// notification rows. Inline in the same SERIALIZABLE tx (plan
		// §F-13) so a mention failure rolls back the comment write —
		// atomicity is load-bearing for the watcher graph invariant.
		//
		// We capture the resolved set into the returned dto.Mentions
		// using the same tx (no re-resolve after commit). The frontend
		// substitutes <UserMention> for @usernames in this list.
		// The notification path: Resolve is the recipient filter
		// (Hydrate is the display rule and would include deleted
		// accounts — sk-674.18 T027).
		resolved, err := mentions.Resolve(ctx, tx, projectID, mentions.Parse(body))
		if err != nil {
			return err
		}
		// Self-mentions are excluded from notifications/watchers but
		// remain in the wire-DTO mentions list — the frontend still
		// renders @self as a clickable mention; we just don't ping
		// the actor about their own write.
		if err := emitCommentMentions(ctx, tx, commentID, issueID, projectID, actor, resolved); err != nil {
			return err
		}
		dto.Mentions = toMentionRefs(resolved)
		return nil
	})
	if err != nil {
		// sk-303.17 review: exhausting commentMentionMaxAttempts against a
		// genuine class-40 abort used to leak straight out as a raw
		// pgError — the exact sk-305 defect shape the retry was added to
		// close, just pushed to the tail of the budget instead of removed.
		// mapSerializationExhausted degrades it to ErrSerializationRetry
		// (nothing was written — safe to retry verbatim); any other error
		// passes through untouched.
		return nil, s.mapSerializationExhausted(ctx, err, "addComment", commentMentionMaxAttempts,
			"issueId", issueID)
	}
	return &dto, nil
}

// ErrCommentNotFound is returned by UpdateComment / DeleteComment when the
// comment id does not exist. Distinct from ErrNotFound (the issue sentinel)
// so handlers can map it to a comment-scoped 404 without ambiguity.
var ErrCommentNotFound = errors.New("comment not found")

// CommentMeta is the slim projection the REST handlers / MCP tools need to
// authorize an edit/delete BEFORE mutating: which issue (and project) the
// comment belongs to, and who authored it. AuthorUserID is nil for legacy
// system-seeded rows (author_user_id is nullable); author-only checks treat
// nil as "no human author" → never matches the caller.
type CommentMeta struct {
	CommentID    uuid.UUID
	IssueID      string
	ProjectID    uuid.UUID
	AuthorUserID *uuid.UUID
	DeletedAt    *time.Time
}

// GetCommentMeta loads the authorization-relevant fields for one comment.
// Returns ErrCommentNotFound when the id doesn't exist. Pure read — the
// actual mutation re-loads FOR UPDATE inside its own SERIALIZABLE tx, so a
// TOCTOU between this check and the write is harmless (the write re-checks
// version + deleted_at; authorship is immutable).
func (s *Store) GetCommentMeta(ctx context.Context, commentID uuid.UUID) (*CommentMeta, error) {
	var m CommentMeta
	m.CommentID = commentID
	err := s.pool.QueryRow(ctx, `
SELECT c.issue_id, i.project_id, c.author_user_id, c.deleted_at
  FROM comments c
  JOIN issues i ON i.id = c.issue_id
 WHERE c.id = $1`, commentID).Scan(&m.IssueID, &m.ProjectID, &m.AuthorUserID, &m.DeletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrCommentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("loading comment meta: %w", err)
	}
	return &m, nil
}

// GetComment loads one comment as a full CommentDTO with mentions hydrated
// like ListComments (sk-242.1 US-4, INV-4/6). Soft-deleted rows return as
// tombstones: body "", mentions []. Missing id → ErrCommentNotFound.
//
// projectID is returned alongside the DTO so MCP visibility gates (and
// future callers) can check project membership without a second round-trip
// (GetCommentMeta would re-join the same tables).
func (s *Store) GetComment(ctx context.Context, commentID uuid.UUID) (*CommentDTO, uuid.UUID, error) {
	var (
		dto       CommentDTO
		projectID uuid.UUID
		authorUID *uuid.UUID
	)
	err := s.pool.QueryRow(ctx, `
SELECT c.id, c.issue_id, c.author, c.author_user_id, c.author_type, c.author_kind,
       c.body, c.created_at, c.updated_at, c.version, c.edited_at, c.deleted_at,
       i.project_id, `+person.StatusInProjectExpr("ua_c", "i.project_id")+`
  FROM comments c
  JOIN issues i ON i.id = c.issue_id
  LEFT JOIN users ua_c ON ua_c.id = c.author_user_id
 WHERE c.id = $1`, commentID).Scan(
		&dto.ID, &dto.IssueID, &dto.Author, &authorUID, &dto.AuthorType, &dto.AuthorKind,
		&dto.Body, &dto.CreatedAt, &dto.UpdatedAt, &dto.Version, &dto.EditedAt, &dto.DeletedAt,
		&projectID, &dto.AuthorStatus,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, uuid.Nil, ErrCommentNotFound
	}
	if err != nil {
		return nil, uuid.Nil, fmt.Errorf("loading comment: %w", err)
	}
	dto.AuthorUserID = authorUID

	// Tombstone parity with ListComments: blank body so deleted text never
	// leaves the server; mentions forced to empty (hydration keys off Body).
	if dto.DeletedAt != nil {
		dto.Body = ""
		dto.Mentions = []MentionRef{}
		return &dto, projectID, nil
	}

	resolved, err := mentions.Hydrate(ctx, s.pool, projectID, dto.IssueID, dto.Body)
	if err != nil {
		return nil, uuid.Nil, fmt.Errorf("hydrating comment mentions: %w", err)
	}
	// Always non-nil array (live empty mentions → []), matching ListComments
	// and US-4 / matrix #30.
	dto.Mentions = toMentionRefs(resolved)
	return &dto, projectID, nil
}

// UpdateComment edits an existing comment's body (sk-57). Author-only — the
// caller-authorization (caller user id == comment.author_user_id) is enforced
// by the handler / MCP tool BEFORE calling this; the store path trusts that
// gate and only enforces the concurrency + lifecycle invariants:
//
//   - The comment must exist (ErrCommentNotFound otherwise).
//   - A soft-deleted comment cannot be edited — returns a ValidationError
//     with code "comment_deleted" which the handler maps to 422.
//   - expectedVersion must match the live version, else ErrVersionConflict
//     carrying the current version (mirrors issue update/close).
//
// On success: body is replaced, version bumped, edited_at + updated_at set
// to now(). Authorship columns (author / author_user_id / author_type /
// author_kind) are NEVER rewritten — the editor is recorded solely via the
// event actor.
//
// Emits EventCommentEdited (old_data={"body":<old>},
// new_data={"body":<new>,"id":...,"issueId":...}). @mentions newly
// introduced in the edited body (present in new, absent in old) run the
// same comment.mention emit + auto-watch path AddComment uses; mentions
// removed by the edit are NOT un-notified.
func (s *Store) UpdateComment(
	ctx context.Context,
	commentID uuid.UUID,
	newBody string,
	expectedVersion int,
	actor Actor,
) (*CommentDTO, error) {
	newBody = strings.TrimSpace(newBody)
	if err := ValidateProse("body", newBody); err != nil {
		return nil, err
	}
	if newBody == "" {
		return nil, &ValidationError{Code: "invalid_body", Message: "comment body is required"}
	}
	if len(newBody) > 100000 {
		return nil, &ValidationError{Code: "invalid_body", Message: "comment body too long (>100k chars)"}
	}

	// sk-303.17: replay on a class-40 abort. Unlike PatchIssue etc., this
	// path had NO isSerializationFailure fallback at all — a raw pgError
	// went straight to mapCommentWriteError's default branch
	// (ErrCodeInternalError). The closure only assigns to dto, fully
	// re-populated by row.Scan every attempt, so a replay leaves no stale
	// partial state.
	var dto CommentDTO
	err := s.withSerializableRetry(ctx, "updateComment", commentRowMaxAttempts, func(tx pgx.Tx) error {
		// Load + lock the current row. We need issue_id (for project +
		// mention scope), the old body (event old_data + mention diff),
		// the live version (optimistic concurrency), and deleted_at
		// (lifecycle gate).
		var (
			issueID   string
			oldBody   string
			liveVer   int
			deletedAt *time.Time
		)
		err := tx.QueryRow(ctx, `
SELECT issue_id, body, version, deleted_at
  FROM comments
 WHERE id = $1
   FOR UPDATE`, commentID).Scan(&issueID, &oldBody, &liveVer, &deletedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCommentNotFound
		}
		if err != nil {
			return fmt.Errorf("loading comment for update: %w", err)
		}
		if deletedAt != nil {
			return &ValidationError{Code: "comment_deleted", Message: "cannot edit a deleted comment"}
		}
		if liveVer != expectedVersion {
			return &ErrVersionConflict{CurrentVersion: liveVer}
		}

		// Resolve project_id for event payload + mention scope.
		var projectID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT project_id FROM issues WHERE id = $1`, issueID).Scan(&projectID); err != nil {
			return fmt.Errorf("loading issue for comment update: %w", err)
		}

		row := tx.QueryRow(ctx, `
UPDATE comments
   SET body = $2, version = version + 1, edited_at = now(), updated_at = now()
 WHERE id = $1
RETURNING id, issue_id, author, author_user_id, author_type, author_kind, body, created_at, updated_at, version, edited_at, deleted_at`,
			commentID, newBody)
		var authorUUID *uuid.UUID
		if err := row.Scan(&dto.ID, &dto.IssueID, &dto.Author, &authorUUID,
			&dto.AuthorType, &dto.AuthorKind, &dto.Body, &dto.CreatedAt, &dto.UpdatedAt,
			&dto.Version, &dto.EditedAt, &dto.DeletedAt); err != nil {
			return fmt.Errorf("updating comment: %w", err)
		}
		dto.AuthorUserID = authorUUID

		if _, err := WriteCommentEvent(ctx, tx, EventCommentEdited, commentID, issueID, projectID, actor,
			map[string]any{"body": oldBody},
			map[string]any{
				"id":      dto.ID,
				"issueId": dto.IssueID,
				"body":    dto.Body,
			}, ""); err != nil {
			return err
		}

		// @mention diff: only usernames newly introduced by the edit
		// (present in new body, absent in old) get the comment.mention
		// event and its notification — no subscription is created for
		// them (US-8 / INV-8). Removed mentions are left alone (no
		// un-notify). We resolve the delta set against project
		// membership, then reuse emitCommentMentions for the per-user
		// emit (it drops a human author's self-mention, but not an
		// agent's mention of its owner).
		newCands := mentions.Parse(newBody)
		oldCands := mentions.Parse(oldBody)
		deltaCands := mentions.Diff(newCands, oldCands)
		if len(deltaCands) > 0 {
			delta, err := mentions.Resolve(ctx, tx, projectID, deltaCands)
			if err != nil {
				return err
			}
			if err := emitCommentMentions(ctx, tx, commentID, issueID, projectID, actor, delta); err != nil {
				return err
			}
		}

		// Hydrate the full mention set for the returned DTO (matches the
		// AddComment contract — dto.Mentions reflects the *current* body,
		// not just the delta). Self-mentions stay in the list (the
		// frontend renders @self as a clickable mention).
		resolvedAll, err := mentions.Resolve(ctx, tx, projectID, newCands)
		if err != nil {
			return err
		}
		dto.Mentions = toMentionRefs(resolvedAll)
		return nil
	})
	if err != nil {
		// sk-303.17 review: this path had NO fallback at all — a raw
		// pgError on the exhausted attempt went straight to
		// mapCommentWriteError's default ErrCodeInternalError branch. The
		// comment's version genuinely did not move (the whole tx rolled
		// back on every attempt), so mapping to issues.ErrVersionConflict
		// would tell the caller a lie identical to what a compliant
		// re-read-and-retry would just repeat; ErrSerializationRetry says
		// what actually happened instead.
		return nil, s.mapSerializationExhausted(ctx, err, "updateComment", commentRowMaxAttempts,
			"commentId", commentID.String())
	}
	return &dto, nil
}

// DeleteComment soft-deletes a comment (sk-57). Author-or-owner
// authorization is enforced by the handler / MCP tool BEFORE this call;
// the store path only flips the tombstone flag and emits the event.
//
// Idempotent: a comment that is already soft-deleted is a no-op success —
// no second event is written. A missing comment returns ErrCommentNotFound.
//
// The body text stays in the row (audit trail); ListComments blanks it on
// read so deleted text never reaches clients. Emits EventCommentDeleted
// (old_data={"body":<body>}, reason=<reason>).
func (s *Store) DeleteComment(
	ctx context.Context,
	commentID uuid.UUID,
	reason string,
	actor Actor,
) error {
	// sk-303.17: same FOR UPDATE row-lock shape as UpdateComment (same
	// comments row, same predicate-then-write pattern) — see
	// commentRowMaxAttempts. No captured locals escape the closure (this
	// path returns only an error), so a replay has nothing to reset.
	err := s.withSerializableRetry(ctx, "deleteComment", commentRowMaxAttempts, func(tx pgx.Tx) error {
		var (
			issueID   string
			body      string
			deletedAt *time.Time
		)
		err := tx.QueryRow(ctx, `
SELECT issue_id, body, deleted_at
  FROM comments
 WHERE id = $1
   FOR UPDATE`, commentID).Scan(&issueID, &body, &deletedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCommentNotFound
		}
		if err != nil {
			return fmt.Errorf("loading comment for delete: %w", err)
		}
		// Idempotent — already deleted: no-op success, no second event.
		if deletedAt != nil {
			return nil
		}

		var projectID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT project_id FROM issues WHERE id = $1`, issueID).Scan(&projectID); err != nil {
			return fmt.Errorf("loading issue for comment delete: %w", err)
		}

		if _, err := tx.Exec(ctx, `
UPDATE comments SET deleted_at = now(), updated_at = now() WHERE id = $1`, commentID); err != nil {
			return fmt.Errorf("soft-deleting comment: %w", err)
		}

		if _, err := WriteCommentEvent(ctx, tx, EventCommentDeleted, commentID, issueID, projectID,
			actor, map[string]any{"body": body}, nil, reason); err != nil {
			return err
		}
		return nil
	})
	// sk-303.17 review: same fallback as UpdateComment — an exhausted
	// budget used to surface as a raw pgError (→ MCP/REST internal_error)
	// with no fallback at all. mapSerializationExhausted is a no-op on
	// nil/non-retryable err, so this is safe on the success path too.
	return s.mapSerializationExhausted(ctx, err, "deleteComment", commentRowMaxAttempts,
		"commentId", commentID.String())
}

// EventDTO is the wire shape returned by ListEvents — a slim subset of
// the events row tailored to the MCP tool surface. We expose a stable
// struct here (instead of a map[string]any blob) so the JSON contract
// for skein_show events stays additive: new columns added to the events
// table don't surface to the agent until we add them here intentionally.
type EventDTO struct {
	ID             int64     `json:"id"`
	OccurredAt     time.Time `json:"occurredAt"`
	Type           string    `json:"type"`
	Actor          string    `json:"actor"`
	ActorSessionID *string   `json:"actorSessionId"`
	ActorType      string    `json:"actorType"`
	Field          *string   `json:"field,omitempty"`
	Reason         *string   `json:"reason,omitempty"`
}

// ListEvents returns the most recent N events for one issue, newest
// first. R2.2a uses this to populate the optional `events` block of
// skein_show. We deliberately keep the projection narrow — old_data /
// new_data / payload can be large and the agent path consumes through
// MAX_MCP_OUTPUT_TOKENS, so heavy fields stay out of the slim view.
//
// limit is clamped to [1, 50] so a runaway caller can't blow the result
// budget — events are a debugging aid here, not a primary surface.
func (s *Store) ListEvents(ctx context.Context, issueID string, limit int) ([]EventDTO, error) {
	if limit < 1 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
SELECT id, occurred_at, type, actor, actor_session_id, actor_type, field, reason
  FROM events
 WHERE entity_type = 'issue' AND entity_id = $1
 ORDER BY id DESC
 LIMIT $2`, issueID, limit)
	if err != nil {
		return nil, fmt.Errorf("listing events: %w", err)
	}
	defer rows.Close()
	out := make([]EventDTO, 0, limit)
	for rows.Next() {
		var e EventDTO
		var field, reason *string
		if err := rows.Scan(&e.ID, &e.OccurredAt, &e.Type, &e.Actor, &e.ActorSessionID, &e.ActorType, &field, &reason); err != nil {
			return nil, fmt.Errorf("scanning event row: %w", err)
		}
		e.Field = field
		e.Reason = reason
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecentEventsForUser returns the most recent N issue-related events the
// caller is involved in — either as actor (they did the thing) or as
// the assignee/creator of the affected issue. Used by skein_prime to
// build the "what just happened in my world" block.
//
// Visibility rule: events are only returned for issues whose project is
// visible to scope — the access relation (authz), in the query.
//
// projectID (optional) further narrows to a single project when set;
// nil = all visible.
//
// limit is clamped to [1, 50] — events are a small priming hint, not a
// primary surface.
func (s *Store) RecentEventsForUser(
	ctx context.Context,
	userID uuid.UUID,
	scope authz.Scope,
	projectID *uuid.UUID,
	limit int,
) ([]UserEventDTO, error) {
	if limit < 1 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	// Build the WHERE clause incrementally. We always filter on
	// entity_type='issue' (the table also stores comment/project events
	// but the per-user feed only surfaces issue-scope activity to keep
	// the response cohesive with the rest of skein_prime).
	args := []any{userID}
	addArg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	// Caller is involved in two ways:
	//  - actor (they performed the action, including assignment),
	//  - the issue is currently assigned to them (resolved via the same
	//    LOWER(username) JOIN GetIssue/ListIssues use) or they created it.
	// We rely on a JOIN to issues + a LEFT JOIN to users to pull title
	// and check the assignee-by-username condition cheaply.
	clauses := []string{
		"e.entity_type = 'issue'",
		"(e.actor_user_id = $1 OR ua.id = $1 OR i.created_by = $1)",
	}

	scopeUnrestricted, scopeCaller, hErr := scope.Bind(ctx, s.pool)
	if hErr != nil {
		return nil, hErr
	}
	clauses = append(clauses, authz.ProjectVisibleExpr("e", addArg(scopeUnrestricted), addArg(scopeCaller)))

	if projectID != nil {
		clauses = append(clauses, fmt.Sprintf("e.project_id = %s", addArg(*projectID)))
	}

	limitArg := addArg(limit)
	sql := fmt.Sprintf(`
SELECT e.occurred_at, e.type, e.entity_id, COALESCE(i.title, ''), e.actor
  FROM events e
  JOIN issues i ON i.id = e.entity_id
  `+assignee.LeftJoin("ua", "i")+`
 WHERE %s
 ORDER BY e.id DESC
 LIMIT %s`, strings.Join(clauses, " AND "), limitArg)

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("listing user events: %w", err)
	}
	defer rows.Close()

	out := make([]UserEventDTO, 0, limit)
	for rows.Next() {
		var ev UserEventDTO
		if err := rows.Scan(&ev.OccurredAt, &ev.Type, &ev.IssueID, &ev.Title, &ev.Actor); err != nil {
			return nil, fmt.Errorf("scanning user event row: %w", err)
		}
		ev.OccurredAt = ev.OccurredAt.UTC()
		out = append(out, ev)
	}
	return out, rows.Err()
}

// CommentOriginFilter — sk-16. Optional narrowing on the
// comments.author_kind column for ListComments. Mirrors the
// `origin=human-driven|agent-autonomous|all` query token used by
// skein_list and the project counters: "human-driven" folds together
// human + agent_directed (intent: a human is behind the comment),
// "agent-autonomous" surfaces only the self-spawned agent notes,
// "all" (the zero value) is no filter.
type CommentOriginFilter string

const (
	CommentOriginAll             CommentOriginFilter = ""
	CommentOriginHumanDriven     CommentOriginFilter = "human-driven"
	CommentOriginAgentAutonomous CommentOriginFilter = "agent-autonomous"
)

// ListComments returns comments for an issue with optional since-filter
// and last-N selection (sk-242.1 US-5/6, FR-004/008, INV-7/8).
//
// Order on the returned slice is always chronological ASC with
// (created_at, id) tiebreak. Soft-deleted rows stay in the population as
// tombstones (body blanked, mentions emptied) so clients keep slot order.
//
// opts.Since — exclusive lower bound on created_at (strict >). Applied
// before total and before limit. Zero/nil = no time filter.
//
// opts.Limit:
//   - ≤0 — return the full filtered population (REST zero-opts path)
//   - N>0 — last N of the filtered set (DESC pick, outer ASC reorder)
//
// total is the count of the filtered population AFTER since/origin and
// BEFORE limit — so MCP can set commentsTruncated = returned < total.
//
// origin (sk-16) narrows by author_kind in SQL. Unknown values error.
func (s *Store) ListComments(ctx context.Context, issueID string, origin CommentOriginFilter, opts ListCommentsOpts) ([]CommentDTO, int, error) {
	// We need the issue's project_id to resolve @username mentions
	// against project_members visibility. Cheaper to grab it once than
	// to re-query per comment.
	var projectID uuid.UUID
	if err := s.pool.QueryRow(ctx,
		`SELECT project_id FROM issues WHERE id = $1`, issueID).Scan(&projectID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, 0, ErrNotFound
		}
		return nil, 0, fmt.Errorf("loading issue for comment list: %w", err)
	}

	whereExtra := ""
	switch origin {
	case CommentOriginAll:
		// No extra filter. CommentOriginAll == "" so this also matches
		// the unset / zero-value case.
	case CommentOriginHumanDriven:
		// human + agent_directed — the "a human is behind this" lens.
		whereExtra = " AND author_kind IN ('human', 'agent_directed')"
	case CommentOriginAgentAutonomous:
		whereExtra = " AND author_kind = 'agent_autonomous'"
	default:
		// sk-16 review fix #3 — fail fast on unknown filters instead of
		// silently returning the unfiltered list. The REST handler
		// validates at the boundary, so this default only catches API
		// misuses (future tools, scripts) where a hard error is what
		// we want.
		return nil, 0, fmt.Errorf("listing comments: unknown origin filter: %q", origin)
	}

	// Build bind args: $1 = issueID, optional $2 = since.
	args := []any{issueID}
	sinceClause := ""
	if opts.Since != nil {
		args = append(args, opts.Since.UTC())
		sinceClause = fmt.Sprintf(" AND created_at > $%d", len(args))
	}
	where := "issue_id = $1" + whereExtra + sinceClause

	// total AFTER since/origin filter, BEFORE limit (FR-004 / US-5).
	var total int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM comments WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting comments: %w", err)
	}

	// sk-57 — soft-deleted rows are NOT filtered out; they stay in the
	// thread as tombstone slots so the client can render "comment
	// deleted" in place. We blank the body below so deleted text never
	// reaches clients (and excludes it from mention hydration).
	//
	// author_status — the author's person marks in the issue's space
	// (sk-674.18 T027), the project bound as the last parameter.
	args = append(args, projectID)
	authorStatusCol := "(SELECT " + person.StatusInProjectExpr("u_ca", fmt.Sprintf("$%d::uuid", len(args))) +
		" FROM users u_ca WHERE u_ca.id = comments.author_user_id) AS author_status"
	selectCols := `id, issue_id, author, author_user_id, author_type, author_kind, body, created_at, updated_at, version, edited_at, deleted_at, author_status`
	innerCols := `id, issue_id, author, author_user_id, author_type, author_kind, body, created_at, updated_at, version, edited_at, deleted_at, ` + authorStatusCol
	var (
		rows pgx.Rows
		err  error
	)
	if opts.Limit > 0 {
		// last-N of filtered population: DESC pick, outer ASC + id tiebreak.
		limitArg := append(args, opts.Limit)
		limitPlaceholder := fmt.Sprintf("$%d", len(limitArg))
		rows, err = s.pool.Query(ctx, `
SELECT `+selectCols+` FROM (
  SELECT `+innerCols+`
    FROM comments
   WHERE `+where+`
   ORDER BY created_at DESC, id DESC
   LIMIT `+limitPlaceholder+`
) sub
 ORDER BY created_at ASC, id ASC`, limitArg...)
	} else {
		// Limit ≤0 → full filtered list (REST zero-opts / commentsLimit:0).
		rows, err = s.pool.Query(ctx, `
SELECT `+innerCols+`
  FROM comments
 WHERE `+where+`
 ORDER BY created_at ASC, id ASC`, args...)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("listing comments: %w", err)
	}
	defer rows.Close()
	out := make([]CommentDTO, 0)
	for rows.Next() {
		var c CommentDTO
		var authorUUID *uuid.UUID
		if err := rows.Scan(&c.ID, &c.IssueID, &c.Author, &authorUUID,
			&c.AuthorType, &c.AuthorKind, &c.Body, &c.CreatedAt, &c.UpdatedAt,
			&c.Version, &c.EditedAt, &c.DeletedAt, &c.AuthorStatus); err != nil {
			return nil, 0, fmt.Errorf("scanning comment row: %w", err)
		}
		c.AuthorUserID = authorUUID
		// Tombstone: blank the body so deleted text never leaves the
		// server. Mentions for this row are forced empty below (the
		// hydration step keys off Body, which is now "").
		if c.DeletedAt != nil {
			c.Body = ""
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterating comment rows: %w", err)
	}

	// R4.3 — hydrate per-comment mentions in one DB round-trip. We
	// gather every candidate username across all bodies, resolve the
	// union once (Resolve is single-SQL), and reuse the per-comment
	// Parse() output that HydrateManyWithParses already produced — no
	// second walk of every body. This is O(N) parses + 1 SQL.
	if len(out) > 0 {
		texts := make([]string, len(out))
		for i, c := range out {
			texts[i] = c.Body
		}
		resolved, parses, err := mentions.HydrateManyWithParses(ctx, s.pool, projectID, issueID, texts...)
		if err != nil {
			return nil, 0, fmt.Errorf("hydrating comment mentions: %w", err)
		}
		// Build username → userID lookup so we can scope the global
		// resolved set down to each comment's actual mentions. Parse()
		// already deduplicates by username so no per-comment `seen`
		// map is needed.
		byUsername := make(map[string]uuid.UUID, len(resolved))
		for _, r := range resolved {
			byUsername[r.Username] = r.UserID
		}
		for i := range out {
			perBody := parses[i]
			refs := make([]MentionRef, 0, len(perBody))
			for _, u := range perBody {
				uid, ok := byUsername[u]
				if !ok {
					continue
				}
				refs = append(refs, MentionRef{Username: u, UserID: uid})
			}
			out[i].Mentions = refs
		}
	}
	return out, total, nil
}

// ----------------- R2.3 — claim / unclaim / close -----------------

// Claim atomically takes the lock on an issue for the caller's MCP
// session. Predicate (success conditions, ANY one):
//
//   - assignee IS NULL (issue is free)
//   - assignee = caller's optional override (admin "claim on behalf"
//     path) AND locked_by IS NULL
//   - locked_by = caller's session id (idempotent re-claim — no-op,
//     no version bump, no event)
//
// Re-claim by the SAME session is idempotent: returns the issue
// unchanged. Different session of the same user, or any other locked-
// by-someone-else case, returns ErrAlreadyClaimed — wrapped in an
// *AlreadyClaimedError that carries the branch and the row state
// (status / locked_by / assignee) the rejection was based on.
//
// sk-202: ErrAlreadyClaimed is returned ONLY after reading a row that
// actually shows someone else holding the issue. Transient database
// conflicts are not laundered through it: the transaction is replayed on
// SQLSTATE 40001 (see withSerializableRetry) and, if every attempt loses, the
// caller gets ErrSerializationRetry — "no durable change, retry verbatim" —
// instead of a false accusation that another session owns the issue.
//
// Successful first-time claim:
//   - sets locked_by, locked_at
//   - sets assignee when input.OptionalAssignee is non-nil OR when
//     assignee is currently NULL (defaults to actor.Username so the
//     downstream "who is working on this" view stays populated)
//   - bumps version + updated_at
//   - emits issue.claimed event
//
// SessionID="" is a programmer error — Mcp-Session-Id is required for
// any claim flow. Callers should pre-validate; the store guards as a
// belt-and-suspenders measure and returns a ValidationError.
//
// sk-18: Claim is atomic over (lock, status). open/deferred → in_progress
// in the same transaction as taking the lock; on first transition into
// in_progress, started_at is stamped (matches UpdateStatus semantics).
// in_progress is preserved. closed is rejected with ErrCannotClaimClosed
// — reopen via skein_update(status=open) first. standing is rejected with
// ErrCannotClaimStanding (sk-118 F2d) — standing tasks are not worked
// directly; spawn a child subtask (parent.N) and claim that instead.
//
// Event emission:
//   - issue.claimed — always written on a real claim (and on the
//     transitional re-claim that finishes a pre-sk-18 lock-without-status).
//   - issue.status_changed — written whenever the status actually
//     transitioned (open|deferred → in_progress). Mirrors the
//     UpdateStatus contract so subscribers filtering on status_changed
//     don't need to also pattern-match issue.claimed.
func (s *Store) Claim(ctx context.Context, input ClaimInput) (*IssueDetail, error) {
	if input.SessionID == "" {
		return nil, &ValidationError{
			Code:    "missing_session_id",
			Message: "session id is required to claim an issue",
		}
	}

	err := s.withSerializableRetry(ctx, "claim", claimMaxAttempts, func(tx pgx.Tx) error {
		return s.claimInTx(ctx, tx, input)
	})
	if err != nil {
		return nil, s.mapClaimTxError(ctx, err, input.SessionID, input.ID)
	}
	return s.GetIssue(ctx, input.ID)
}

// ClaimMany atomically claims every requested issue. A predicate failure,
// event failure, or notification failure rolls back the complete batch.
func (s *Store) ClaimMany(ctx context.Context, input ClaimManyInput) ([]*IssueDetail, error) {
	if input.SessionID == "" {
		return nil, &ValidationError{Code: "missing_session_id", Message: "session id is required to claim issues"}
	}
	if len(input.IDs) == 0 {
		return nil, &ValidationError{Code: "empty_claim_batch", Message: "at least one issue id is required"}
	}
	seen := make(map[string]struct{}, len(input.IDs))
	for _, id := range input.IDs {
		if _, duplicate := seen[id]; duplicate {
			return nil, &ValidationError{Code: "duplicate_claim_id", Message: "claim batch issue ids must be unique"}
		}
		seen[id] = struct{}{}
	}
	ordered := append([]string(nil), input.IDs...)
	sort.Strings(ordered)
	err := s.withSerializableRetry(ctx, "claim", claimMaxAttempts, func(tx pgx.Tx) error {
		for _, id := range ordered {
			if err := s.claimInTx(ctx, tx, ClaimInput{ID: id, SessionID: input.SessionID, OptionalAssignee: input.OptionalAssignee, Actor: input.Actor}); err != nil {
				return &ClaimManyError{ID: id, Err: err}
			}
		}
		return nil
	})
	if err != nil {
		return nil, s.mapClaimTxError(ctx, err, input.SessionID, strings.Join(input.IDs, ","))
	}
	return s.getIssuesByIDs(ctx, input.IDs)
}

// claimInTx is the single writer of a non-null issues.locked_by, for the
// one-issue and the batch claim alike.
//
// Obligation (sk-421.1 DRAFT ADR-1): setting locked_by/locked_at here and
// writing the issue.claimed event (new_data.lockedBy = the session, no
// explicit occurred_at, so it equals locked_at = now() of this transaction)
// MUST stay in the same transaction. The detail's lockedByUsername is
// matched against exactly that event (see lockHolderUsernameFor); any new
// path that sets a lock without writing it silently drops the holder name.
func (s *Store) claimInTx(ctx context.Context, tx pgx.Tx, input ClaimInput) error {
	// K-7 (sk-674.18 T018): a claim may make the caller the assignee, so it
	// takes the shared lock of the issue's space before the issue row.
	if err := spaces.ShareIssueSpace(ctx, tx, input.ID); err != nil {
		return err
	}
	old, err := s.fetchIssueInTx(ctx, tx, input.ID)
	if err != nil {
		return err
	}

	// sk-18: closed is terminal. Reopen first.
	if old.Status == StatusClosed {
		return ErrCannotClaimClosed
	}

	// sk-118 (F2d): standing is never claimed/worked directly. A standing
	// task is a perpetual obligation; you act on it by spawning a child
	// subtask (parent.N) and claiming THAT. Reject before any assignee /
	// lock / status mutation so the row is left wholly untouched (no
	// status flip, no locked_by, no started_at, no version bump, no
	// issue.claimed event) — mirrors the ErrCannotClaimClosed guard above.
	if old.Status == StatusStanding {
		return ErrCannotClaimStanding
	}

	// Re-claim by the SAME HOLDER (same owning user AND same session) with
	// status already in_progress → true idempotent no-op (no version bump, no
	// event). If the lock is held by us but status is still open/deferred
	// (pre-sk-18 row), fall through to finish the transition.
	//
	// sk-424: the owner is checked here too. Without it, presenting another
	// user's session string made a takeover look like the holder's own
	// harmless re-claim.
	if old.LockedBy != nil && *old.LockedBy == input.SessionID &&
		lockOwnedBy(old, input.Actor.UserID) && old.Status == StatusInProgress {
		return nil
	}

	// Held by somebody else → fail. "Somebody else" is a different session OR
	// the same session string presented by a different user (sk-424).
	if old.LockedBy != nil && *old.LockedBy != "" &&
		(*old.LockedBy != input.SessionID || !lockOwnedBy(old, input.Actor.UserID)) {
		return newAlreadyClaimed("locked_by_other", old)
	}

	// sk-622.21 T015: «Claim / unclaim» names the assignee before and after.
	rolesBefore, err := readSignalRoles(ctx, tx, input.ID)
	if err != nil {
		return err
	}

	// Assignee predicate (locked_by is NULL or held by us):
	//   - assignee IS NULL → free, claim succeeds
	//   - assignee is the same USER as the override (admin
	//     claim-on-behalf) → succeeds
	//   - assignee is the same USER as the caller (R2.2 issue with
	//     assignee=me) → succeeds
	//   - else → already_claimed (someone else is the assignee)
	//
	// sk-294: both comparisons are on USER IDENTITY, not on the handle
	// string. `assignee` is a free-form handle column and the write
	// surface documents three legal forms for it — "username, email, or
	// user UUID — stored verbatim, resolved on read". A raw string
	// compare made two of those three unusable: a task assigned by UUID
	// could not be claimed by the very person it was assigned to, and the
	// rejection blamed `already_claimed` — "somebody else holds this" —
	// for what was really "the handle is written in another form". A
	// surface that accepts three forms on write and understands one on
	// read manufactures tasks that look ordinary and cannot be taken.
	newAssignee := old.Assignee
	switch {
	case input.OptionalAssignee != nil:
		a := *input.OptionalAssignee
		if err := checkAssigneeChoice(ctx, tx, a, old.ProjectID); err != nil {
			return err
		}
		if old.Assignee != nil && *old.Assignee != "" {
			same, err := sameAssigneeHandleUser(ctx, tx, *old.Assignee, a)
			if err != nil {
				return err
			}
			if !same {
				return newAlreadyClaimed("assignee_vs_override", old)
			}
		}
		newAssignee = &a
	case old.Assignee == nil || *old.Assignee == "":
		a := input.Actor.Username
		newAssignee = &a
	default:
		// assignee is set; only allow if it denotes the caller.
		same, err := assigneeHandleIsActor(ctx, tx, *old.Assignee, input.Actor)
		if err != nil {
			return err
		}
		if !same {
			return newAlreadyClaimed("assignee_vs_actor", old)
		}
	}

	var assigneeArg any
	if newAssignee != nil && *newAssignee != "" {
		assigneeArg = *newAssignee
	}

	// sk-26: auto-accept on self-assign-via-claim. Symmetric with
	// the CreateIssue (store.go:436-446) and UpdateIssue
	// (store.go:1892-1899) auto-accept paths — the invariant is
	// "any self-assign (create / patch / claim) ⟹ accepted_at =
	// now()" because claiming a task is semantically the act of
	// taking it. Pre-sk-26 the free-assignee branch (assignee=NULL
	// → claim by caller) set assignee=caller without stamping
	// accepted_at, so a task created without assignee and then
	// claimed surfaced as "assignee=me + acceptedAt=null" forever
	// (the sk-25 button-on-closed scenario).
	//
	// Fires when newAssignee resolves to input.Actor.UserID AND
	// the row's pre-claim accepted_at was NULL. Resolves via
	// ResolveAssigneeUserID (case-insensitive on users.username) —
	// uuid.Nil result means the new assignee is a free-form label
	// that doesn't map to any user, so we can't claim it's
	// "self-assign" and skip the stamp.
	//
	// Skipped on the admin-claim-on-behalf-of-someone-else path
	// because newAssignee != caller in that case → delegation, not
	// self-assign. Skipped on the already-accepted case because
	// re-stamping would erase the original acceptance timestamp.
	// sk-303.13 / T021: no longer silent. The stamp moving from empty to
	// filled writes an `issue.accepted` event here exactly as it does on
	// the explicit path (INV-5) — but journal-only. A claim already
	// notifies the creator through issue.status_changed; adding
	// "assignee accepted" on top would be a second notification for one
	// user action, which INV-6 forbids.
	var newAcceptedAtArg, newAcceptedByArg any
	var acceptedAtOnClaim time.Time
	acceptanceChanged := false
	if old.AcceptedAt == nil && newAssignee != nil && *newAssignee != "" {
		rid, rerr := notifications.ResolveAssigneeUserID(ctx, tx, *newAssignee, old.ProjectID)
		if rerr != nil {
			return rerr
		}
		if rid != uuid.Nil && rid == input.Actor.UserID {
			acceptedAtOnClaim = time.Now().UTC()
			newAcceptedAtArg = acceptedAtOnClaim
			newAcceptedByArg = input.Actor.UserID
			acceptanceChanged = true
		}
	}
	claimAcceptance := ClassifyAcceptance(old.AcceptedAt != nil, acceptanceChanged, false)

	// sk-18: status transitions atomically with the lock take.
	statusChanged := old.Status != StatusInProgress
	startedNow := old.StartedAt == nil && statusChanged
	ct, err := tx.Exec(ctx, `
UPDATE issues
   SET status     = 'in_progress',
       started_at  = CASE WHEN $4::boolean THEN now() ELSE started_at END,
       assignee    = COALESCE($2, assignee),
       locked_by   = $3,
       locked_at   = now(),
       locked_by_user_id = $8,
       accepted_at = CASE WHEN $5::boolean THEN $6 ELSE accepted_at END,
       accepted_by = CASE WHEN $5::boolean THEN $7 ELSE accepted_by END,
       version     = version + 1,
       updated_at  = now()
 WHERE id = $1
   AND status <> 'closed'
   AND (locked_by IS NULL OR (locked_by = $3 AND locked_by_user_id = $8))`,
		input.ID, assigneeArg, input.SessionID, startedNow,
		acceptanceChanged, newAcceptedAtArg, newAcceptedByArg, input.Actor.UserID)
	if err != nil {
		return fmt.Errorf("claiming issue: %w", err)
	}
	if ct.RowsAffected() == 0 {
		// The UPDATE matched nothing even though the SELECT above, in
		// this same snapshot, saw a row that satisfies the WHERE. It
		// cannot be a lost claim race: under SERIALIZABLE a concurrent
		// writer that changed the row makes this statement raise 40001
		// rather than silently skip it (handled by the retry in the
		// caller). So report it as an internal inconsistency instead of
		// blaming an imaginary other session (sk-202).
		//
		// The one input shape that could reach here without a genuine
		// inconsistency is locked_by = '' — the Go predicate above skips
		// it (*old.LockedBy != "" is false) while this statement's
		// `locked_by IS NULL OR locked_by = $3` does not match it either.
		// That shape is structurally impossible, so it is asserted, not
		// defended against:
		//
		//   - This UPDATE is the only writer of a non-NULL locked_by in
		//     the whole codebase. Every other write sets it to NULL
		//     (Patch's unlock CASE, Unclaim, Close, lockreap's reaper);
		//     no INSERT INTO issues names the column, so a fresh row is
		//     NULL by default.
		//   - Its $3 is input.SessionID, and Claim rejects SessionID ==
		//     "" with a ValidationError before opening the transaction.
		//     That guard landed in the same commit that introduced Claim
		//     (6e9efde), so no release could ever have written ''.
		//   - Migration 000036 pins the invariant in the schema
		//     (issues_locked_by_nonempty), so a future second writer
		//     cannot reintroduce it silently.
		return fmt.Errorf("claim UPDATE matched no rows for %s (snapshot saw status=%s lockedBy=%v)",
			input.ID, old.Status, old.LockedBy)
	}

	// Audit event. old_data carries the pre-claim assignee; new_data
	// carries the post-claim assignee + lockedBy + lockedAt placeholder.
	oldAssigneeStr := ""
	if old.Assignee != nil {
		oldAssigneeStr = *old.Assignee
	}
	newAssigneeStr := ""
	if newAssignee != nil {
		newAssigneeStr = *newAssignee
	}
	if _, err := WriteIssueEvent(ctx, tx, EventIssueClaimed, input.ID, old.ProjectID,
		input.Actor, "assignee",
		map[string]any{"assignee": oldAssigneeStr, "lockedBy": nil},
		map[string]any{"assignee": newAssigneeStr, "lockedBy": input.SessionID},
		""); err != nil {
		return err
	}

	// sk-303.13 / T021: the acceptance stamp this claim just set gets its
	// own audit event. Journal-only — see claimAcceptance above.
	if claimAcceptance.WriteEvent {
		if _, err := WriteIssueEvent(ctx, tx, EventIssueAccepted, input.ID, old.ProjectID,
			input.Actor, "accepted_at",
			map[string]any{"acceptedBy": nil, "acceptedAt": nil},
			map[string]any{"acceptedBy": input.Actor.UserID, "acceptedAt": acceptedAtOnClaim},
			acceptanceReasonAuto); err != nil {
			return err
		}
	}

	// sk-18: emit issue.status_changed for the actual status flip.
	// reason="claim" lets audit consumers attribute the transition to
	// the claim path (vs an explicit skein_update).
	if statusChanged {
		eventID, err := WriteIssueEvent(ctx, tx, EventIssueStatusChanged, input.ID, old.ProjectID,
			input.Actor, "status", old.Status, StatusInProgress, "claim")
		if err != nil {
			return err
		}
		statusCreator, statusAssignee, err := roleContextOf(ctx, tx, old)
		if err != nil {
			return err
		}
		if _, err := notifications.FanOut(ctx, tx, notifications.FanOutInput{
			EventID:        eventID,
			EventType:      EventIssueStatusChanged,
			EntityType:     "issue",
			IssueID:        input.ID,
			ProjectID:      old.ProjectID,
			CreatorUserID:  statusCreator,
			AssigneeUserID: statusAssignee,
			ActorUserID:    input.Actor.UserID,
			ActorType:      input.Actor.Role,
		}); err != nil {
			return err
		}
	}
	// sk-622.21 T015 (US-11): personal signal «Claim»; a claim that flips
	// the status is also "Status change".
	if err := signalClaim(ctx, tx, input.ID, rolesBefore); err != nil {
		return err
	}
	if statusChanged {
		return signalStatusChanged(ctx, tx, input.ID, rolesBefore)
	}
	return nil
}

func (s *Store) mapClaimTxError(ctx context.Context, err error, sessionID, issueID string) error {
	// sk-202: a transaction abort that survived every retry is a transient
	// database outcome, not evidence that somebody else holds the issue.
	if isRetryableTxError(err) {
		slog.WarnContext(ctx, "claim exhausted serialization retries",
			"issueId", issueID,
			"sessionId", sessionID,
			"attempts", claimMaxAttempts,
			"error", err.Error())
		return ErrSerializationRetry
	}
	return err
}

// mapSerializationExhausted classifies the error withSerializableRetry
// returns after its FINAL attempt still failed (sk-303.17 review fix).
// A retryable abort that survived the whole budget (SQLSTATE 40001 or
// 40P01 — see isRetryableTxError) means Postgres rolled the transaction
// back WHOLE on every attempt: nothing durable changed, so unlike
// PatchIssue / setPinned / etc. — where the abort is degraded to
// &ErrVersionConflict{CurrentVersion: cv} because the row genuinely has a
// version a re-read can quote — the callers this helper serves have no
// truthful "current version" to blame it on. AddComment and
// CreateEpicWithChildren are INSERTs: nothing was ever written, so there
// is no row to re-read a version from. UpdateComment/DeleteComment DO
// touch an existing comments row, but mapping their exhaustion to
// issues.ErrVersionConflict would still be a lie: that sentinel is
// defined (and every caller assumes) version conflict against an ISSUE,
// not a comment, and the row's version genuinely did not change — a
// compliant caller who re-reads and retries on "the same version it
// already had" walks straight back into nothing new.
//
// The honest terminal is ErrSerializationRetry — the SAME sentinel
// Claim's mapClaimTxError returns for the identical situation: "no
// durable change, safe to retry verbatim". Reusing it here (rather than
// minting a second sentinel with the same meaning) keeps exactly one
// code — `serialization_retry` — for "your write raced and lost, nothing
// happened, try again" across the whole store.
//
// Any other error — a business error one of the attempts returned
// directly (ErrNotFound, ErrCommentNotFound, an explicit
// ErrVersionConflict from a real expectedVersion mismatch, ...) — passes
// through UNCHANGED: only exhaustion of the retryable-abort budget gets
// remapped. err == nil passes through as nil, so callers can wrap their
// withSerializableRetry result in this helper unconditionally instead of
// gating the call on err != nil first.
//
// op names the operation in the log line (mirrors withSerializableRetry's
// own "op" parameter, so the two log lines for one operation — the
// per-attempt WARN and this exhaustion WARN — share a grep key);
// logFields are extra slog key/value pairs identifying the target (e.g.
// "commentId", "issueId") the way mapClaimTxError's "issueId"/"sessionId"
// do for Claim.
func (s *Store) mapSerializationExhausted(ctx context.Context, err error, op string, attempts int, logFields ...any) error {
	if !isRetryableTxError(err) {
		return err
	}
	fields := append([]any{"op", op, "attempts", attempts, "error", err.Error()}, logFields...)
	slog.WarnContext(ctx, op+" exhausted serialization retries", fields...)
	return ErrSerializationRetry
}

// claimMaxAttempts bounds how many times Claim replays its transaction
// against a retryable abort. Matches attachments.withSerializableRetry —
// three attempts absorb the realistic contention surface on a single issue
// row (a concurrent label/comment touch, the lock reaper's sweep, an
// embedding backfill) without inflating tail latency for the caller.
const claimMaxAttempts = 3

// createMaxAttempts is deliberately larger than claimMaxAttempts because
// the contention it absorbs is structural, not incidental.
//
// Claim contends by accident: two sessions happen to touch the same issue
// row. Create contends by construction — every child of a parent must
// advance the SAME child_counters row (nextChildID), so an agent fanning an
// epic out into N siblings produces an N-way pile-up on one row by design,
// and the row can only be advanced one committed transaction at a time. A
// budget of 3 covers a three-way tie; it does not cover a fan-out, and
// under-sizing it here does not degrade gracefully — the loser gets
// internal_error, which is the sk-305 defect itself.
//
// Six is the measured floor, not a guess: with 3 attempts an 8-way
// concurrent fan-out lost one create outright
// (TestCreateIssue_ConcurrentChildren_NoSerializationError, 7/8), and the
// same test is green at 6. Worst-case added latency stays bounded — the
// backoff is 25ms x attempt plus up to 25ms of jitter, so five waits total
// under ~0.5s before the caller gets a fast, honest error instead of an
// unbounded retry chain.
const createMaxAttempts = 6

// commentMentionMaxAttempts bounds AddComment's replay budget (sk-303.17).
//
// AddComment's contention is neither Claim's accidental single-row
// collision nor CreateIssue's counter-row fan-out — it is per-recipient:
// N concurrent comments on ONE issue that all @mention the SAME user
// contend on that user's mention/notification bookkeeping
// (emitCommentMentions / notifications.WriteMentionNotification), while
// concurrent comments on the same issue with DISTINCT (or no) mention
// targets showed zero serialization failures across repeated runs. This
// was measured, not assumed: a same-issue, same-mention-target run of 40
// concurrent AddComment calls against plain BeginTxFunc produced 3-4 raw
// SQLSTATE 40001s per run (3 runs), consistent with a real but modest
// pile-up on one recipient's rows — not a guaranteed N-way tie the way
// nextChildID is.
//
// 3 is the measured floor for this shape:
// TestAddComment_ConcurrentMentions_NoSerializationError (40 concurrent
// commenters, one shared @mention target) is green at 3 across repeated
// runs; it is NOT copied from claimMaxAttempts — the two just happen to
// land on the same number because both contend through a small handful
// of concurrent touchers, not a large fan-out.
const commentMentionMaxAttempts = 3

// commentRowMaxAttempts bounds UpdateComment / DeleteComment's replay
// budget (sk-303.17). Both do `SELECT ... FOR UPDATE` on the SAME
// comments row before writing it — Claim's exact contention shape (one
// row, one accidental concurrent toucher), not a structural fan-out.
// TestUpdateComment_SerializationFailure_Retries and
// TestDeleteComment_SerializationFailure_Retries each park a single
// concurrent writer on the target comment row (the sk-202 repro
// technique) and are green at this budget; matched to claimMaxAttempts
// because the mechanism is identical, not copied on assumption.
const commentRowMaxAttempts = claimMaxAttempts

// epicCreateMaxAttempts bounds CreateEpicWithChildren's replay budget
// (sk-303.17). Structural contention like createMaxAttempts, not
// incidental like claimMaxAttempts: a ROOT epic (the caller passed no
// ParentID) claims its id from project_counters — the SAME per-project row EVERY top-level create in
// the project serialises through, CreateIssue included. An 8-way
// concurrent CreateEpicWithChildren in one project measured 7/8 losses
// with zero retries (worse than CreateIssue's own 8-way child fan-out,
// which lost 1/8 at 3 attempts) — plausibly because this transaction
// does 1+len(children) inserts instead of CreateIssue's one, holding the
// SERIALIZABLE snapshot open longer and widening the window a concurrent
// top-level create can land in.
//
// See TestCreateEpicWithChildren_ConcurrentEpics_NoSerializationError.
const epicCreateMaxAttempts = 10

// R17 budgets (sk-622.21, K2). These paths write subscriptions, opt-outs,
// Watching cards or fan-out rows inside SERIALIZABLE transactions that, until
// then, ran once and degraded any class-40 abort to version_conflict. The
// SSI fan-out (T012) and the subscription writers (T010/T011) make such aborts
// an expected outcome of ordinary concurrency, not a user-visible conflict, so
// each path replays through withSerializableRetry with ITS OWN budget (sk-354:
// not one shared number). All of them contend the way Claim does — a
// handful of incidental touchers of one issue row and of its watchers'
// rows, not a structural fan-out — so each starts at 3; they are separate
// constants so that a measurement on one path can move its budget alone.
// On exhaustion each path keeps its previous classification: PatchIssue,
// UpdateStatus, Close, Accept and the label paths degrade to version_conflict
// (degradeSerializationAbort, counted by versionConflictOriginTotal),
// DismissDelegated likewise, Unclaim answers ErrNotLockHolder.
//
// sk-303.73 moved PatchIssue, UpdateStatus and Close to 10, by measurement.
// Their contention is NOT only the issue row: SSI also aborts them against
// CreateIssue transactions in OTHER projects that share no issue with them
// (read/write dependencies through the fan-out and subscription tables), so
// one user creating issues from a few sessions while editing or closing in
// another — an agent fanning out, a person with several tabs — lands the
// edit in a stream of creates, not a three-way tie. Measured with
// TestWritePaths_ConcurrentCreatesElsewhere_NoFalseVersionConflict (8
// sessions creating 4 issues each in their own projects, 4 sessions writing
// 3 issues each in another): at 3 attempts every one of the three paths
// answered false version_conflicts (the caller's version untouched); 6
// attempts let updateStatus exhaust under a longer burst (12 creates per
// session); a successful write took up to 7 attempts, and 10 passed both
// shapes. The cost is bounded by the backoff: nine waits of 25ms × attempt
// plus up to 25ms of jitter, at most ~1.4s before the caller gets the
// degraded answer instead of a false conflict at ~0.1s.
const (
	patchIssueMaxAttempts       = 10
	updateStatusMaxAttempts     = 10
	closeMaxAttempts            = 10
	addLabelMaxAttempts         = 3
	removeLabelMaxAttempts      = 3
	acceptMaxAttempts           = 3
	unclaimMaxAttempts          = 3
	dismissDelegatedMaxAttempts = 3
)

// withSerializableRetry runs fn inside a SERIALIZABLE transaction, replaying
// it on a retryable abort — SQLSTATE 40001 (serialization_failure) or 40P01
// (deadlock_detected), see isRetryableTxError — with a small jittered
// backoff. Returns the last error when the attempts run out. `op` names the
// operation in the retry log lines and nowhere else.
//
// This is the issues-domain sibling of attachments.withSerializableRetry.
// It was the claim path's private helper until sk-305; nothing about its
// mechanics changed when it was generalised, because none of them were
// claim-specific — the replay argument below is a property of a rolled-back
// SERIALIZABLE transaction, not of what the transaction did.
//
// Replay is safe because a class-40 abort rolls the transaction back WHOLE:
// no partial state survives it, and fn re-reads everything it needs on each
// attempt. For the claim path that re-read is fetchIssueInTx, and there the
// property is load-bearing: a retry therefore observes whatever the winning writer committed
// and re-evaluates the claim predicates against it. That property is what
// makes retrying strictly more correct than the pre-sk-202 shortcut of
// mapping 40001 to ErrAlreadyClaimed — if a competing claim really did land
// first, the retry re-reads the row, sees the other session's locked_by, and
// returns a truthful already_claimed from the lock predicate. If the
// conflict came from anything else (a concurrent updated_at touch, the lock
// reaper, an SSI read/write dependency cycle involving rows this claim never
// read), the retry simply succeeds instead of lying to the caller. The same
// argument covers a deadlock (40P01): Postgres aborted the whole
// transaction, so a replay re-reads and re-decides from scratch.
// A caller whose fn has side effects OUTSIDE the transaction (an external
// call, a write to shared state) must not use this helper: a replay would
// double them. Assigning to captured locals is fine — a retry overwrites
// them before the successful return.
// Each attempt opens its transaction through signals.BeginTxFunc, so the
// personal-signal addressees fn names with signals.Add are collected per
// attempt and published only after the attempt that COMMITs (sk-622.21
// T014, INV-12): an aborted attempt's addressees are dropped with it.
// `attempts` is the TOTAL number of tries, not the number of extra ones.
// It is a parameter rather than a constant because the two call sites face
// different contention shapes: claim contends incidentally on one issue row
// (claimMaxAttempts), while create contends BY DESIGN on the per-parent
// child_counters row that every sibling must serialise through
// (createMaxAttempts).
func (s *Store) withSerializableRetry(ctx context.Context, op string, attempts int, fn func(tx pgx.Tx) error) error {
	var err error
	for attempt := range attempts {
		err = signals.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, fn)
		if err == nil {
			return nil
		}
		if !isRetryableTxError(err) {
			return err
		}
		if attempt == attempts-1 {
			break
		}
		// Retries are invisible to the caller, so log them — an operation
		// that silently costs three round-trips is worth seeing before it
		// turns into a latency report (sk-202).
		if pgErr := (*pgconn.PgError)(nil); errors.As(err, &pgErr) {
			slog.WarnContext(ctx, "serializable transaction hit a retryable abort; retrying",
				"op", op,
				"attempt", attempt+1,
				"maxAttempts", attempts,
				"sqlstate", pgErr.Code,
				"detail", pgErr.Message)
		}
		// Jittered backoff: 0..25ms × (attempt+1). Small enough to stay
		// invisible in claim latency, spread enough to break lockstep
		// retries between two contending sessions.
		base := 25 * time.Millisecond * time.Duration(attempt+1)
		jitter := time.Duration(rand.Int64N(int64(25 * time.Millisecond)))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(base + jitter):
		}
	}
	return err
}

// assigneeHandleIsActor reports whether an issue's assignee HANDLE denotes
// the calling actor (sk-294).
//
// The handle is resolved through notifications.ResolveAssigneeUserID, which
// understands all three documented forms (username / e-mail / user UUID),
// and the result is compared against the AUTHENTICATED caller's users.id —
// not against a second resolution of their username. One round-trip, and
// the answer cannot drift from who the request actually is.
//
// An unresolvable handle is not an identity. Free-form labels ("the docs
// team") are a legitimate value of this column, and for them the pre-sk-294
// string rule is kept verbatim so their behaviour does not change: the
// label is not the caller, and the claim is refused. The compare is
// case-insensitive to match the resolver (LOWER=LOWER on users.username)
// and PatchIssue's assigneeChanged gate; in reachable states it is
// equivalent to the old exact compare, because a string that equals a
// username case-insensitively would have resolved and never reached here.
func assigneeHandleIsActor(ctx context.Context, tx pgx.Tx, handle string, actor Actor) (bool, error) {
	uid, err := notifications.ResolveAssigneeIdentity(ctx, tx, handle)
	if err != nil {
		return false, err
	}
	if uid != uuid.Nil {
		return uid == actor.UserID, nil
	}
	return strings.EqualFold(handle, actor.Username), nil
}

// sameAssigneeHandleUser reports whether two assignee HANDLES denote the
// same user — the admin claim-on-behalf case, where the row already carries
// one form of a handle and the caller supplies another.
//
// The three-way rule is the same one PatchIssue's assigneeChanged gate
// applies (store.go, "Resolve both sides to a users.id"), and it is
// deliberately identical so "is this the same assignee?" has ONE answer in
// this package: both sides resolve → compare ids; neither resolves →
// compare the raw strings case-insensitively (two different labels are
// different, one label in two cases is not); exactly one resolves → a real
// user and a label are never the same person.
func sameAssigneeHandleUser(ctx context.Context, tx pgx.Tx, a, b string) (bool, error) {
	ida, err := notifications.ResolveAssigneeIdentity(ctx, tx, a)
	if err != nil {
		return false, err
	}
	idb, err := notifications.ResolveAssigneeIdentity(ctx, tx, b)
	if err != nil {
		return false, err
	}
	switch {
	case ida != uuid.Nil && idb != uuid.Nil:
		return ida == idb, nil
	case ida == uuid.Nil && idb == uuid.Nil:
		return strings.EqualFold(a, b), nil
	default:
		return false, nil
	}
}

// newAlreadyClaimed builds the typed rejection carrying the row state that
// justified it, so the caller can log WHY without re-reading the row.
func newAlreadyClaimed(branch string, old *issueRow) *AlreadyClaimedError {
	e := &AlreadyClaimedError{Branch: branch, Status: old.Status}
	if old.LockedBy != nil {
		e.LockedBy = *old.LockedBy
	}
	if old.Assignee != nil {
		e.Assignee = *old.Assignee
	}
	return e
}

// Unclaim atomically releases the lock on an issue AND (sk-19) transitions
// status to NextStatus in the same transaction.
//
// NextStatus is one of:
//
//   - "in_progress" — handoff: lock released, status stays in_progress so
//     a teammate / another agent can claim and continue. Pair with a
//     comment describing what's done and what's left.
//   - "open" — give up: lock released, status reverts to open so the
//     issue is visible again in skein_ready and someone else can pick it
//     up fresh.
//   - "deferred" — punt: lock released, status moves to deferred (the
//     caller may set defer_until via a follow-up skein_update; this tool
//     does not accept deferUntil to keep its surface focused).
//
// "closed" is rejected (use skein_close instead, which carries reason +
// commits[]). Reason is required (non-empty after trim) — lands on both
// the issue.unclaimed and (when status actually changed) the
// issue.status_changed event.
//
// Predicate:
//
//   - locked_by IS NULL → idempotent no-op. Status is NOT mutated either:
//     "unclaim" on a free issue is a contract no-op, not a sneaky
//     status-update. If the caller wants to flip a status without a
//     lock, they should use skein_update directly.
//   - locked_by = caller's session → success.
//   - locked_by = some other session AND force=false → ErrNotLockHolder.
//   - locked_by = some other session AND force=true → success
//     (event reason="<input.Reason> (force-overridden)").
//
// Note: SessionID="" with locked_by set and force=false yields
// ErrNotLockHolder — a request without a session id cannot be a lock
// holder by definition.
func (s *Store) Unclaim(ctx context.Context, input UnclaimInput) (*IssueDetail, error) {
	// sk-19: validate the new required inputs up front so failures
	// short-circuit before the SERIALIZABLE tx opens.
	if err := ValidateProse("reason", input.Reason); err != nil {
		return nil, err
	}
	newStatus, err := ValidateStatus(input.NextStatus)
	if err != nil {
		return nil, err
	}
	if newStatus == StatusClosed {
		return nil, &ValidationError{
			Code:    "next_status_closed",
			Message: "nextStatus=closed is rejected — use skein_close to close an issue",
		}
	}
	// sk-118 (F2c): unclaim only releases the lock into the work lifecycle
	// {in_progress, open, deferred}. `standing` lives outside that lifecycle
	// — you reach it only via skein_update(status=standing) or
	// create-as-standing, never by dropping a lock. Adding `standing` to the
	// shared ValidationStatus set (F2a) made Store.Unclaim accept it for
	// direct store callers (the MCP enum already excludes it); this guard
	// closes that gap explicitly, mirroring the closed rejection above.
	if newStatus == StatusStanding {
		return nil, &ValidationError{
			Code:    "next_status_standing",
			Message: "nextStatus=standing is rejected — promote via skein_update(status=standing), not unclaim",
		}
	}
	reason := strings.TrimSpace(input.Reason)
	if reason == "" {
		return nil, &ValidationError{
			Code:    "missing_reason",
			Message: "reason is required for skein_unclaim (non-empty after trim)",
		}
	}

	err = s.withSerializableRetry(ctx, "unclaim", unclaimMaxAttempts, func(tx pgx.Tx) error {
		old, err := s.fetchIssueInTx(ctx, tx, input.ID)
		if err != nil {
			return err
		}

		// Already unlocked → idempotent no-op (status untouched too).
		if old.LockedBy == nil || *old.LockedBy == "" {
			return nil
		}

		// Held by somebody else and not forcing → fail. "Somebody else" is a
		// different session OR the same session string presented by a
		// different user — the owner is the authority (sk-424).
		if !input.Force && (*old.LockedBy != input.SessionID ||
			!lockOwnedBy(old, input.Actor.UserID)) {
			return ErrNotLockHolder
		}

		// Either we hold the lock, or force=true. Drop it AND apply the
		// caller's chosen status transition atomically.
		ct, err := tx.Exec(ctx, `
UPDATE issues
   SET status     = $4,
       locked_by  = NULL,
       locked_at  = NULL,
       locked_by_user_id = NULL,
       version    = version + 1,
       updated_at = now()
 WHERE id = $1
   AND ($2::boolean
        OR (locked_by = $3::text AND locked_by_user_id = $5::uuid))`,
			input.ID, input.Force, input.SessionID, newStatus, input.Actor.UserID)
		if err != nil {
			return fmt.Errorf("unclaiming issue: %w", err)
		}
		if ct.RowsAffected() == 0 {
			// Race: someone unclaimed (or re-claimed by a different
			// session) between our SELECT and our UPDATE.
			return ErrNotLockHolder
		}

		unclaimReason := buildForceReason(reason, input.Force)
		if _, err := WriteIssueEvent(ctx, tx, EventIssueUnclaimed, input.ID, old.ProjectID,
			input.Actor, "lockedBy",
			map[string]any{"lockedBy": *old.LockedBy, "lockedAt": old.LockedAt},
			map[string]any{"lockedBy": nil, "lockedAt": nil},
			unclaimReason); err != nil {
			return err
		}
		// sk-674.18 T034: a forced unclaim releases another holder's claim.
		if forceReleasesAnother(old, input.Actor, input.Force) {
			if err := writeForceReleased(ctx, tx, input.ID, old, input.Actor, unclaimReason); err != nil {
				return err
			}
		}

		// sk-19: emit status_changed only when the status actually
		// transitioned. Pair with FanOut so watchers / mentioned users
		// are notified the same way as for skein_update(status=...).
		if old.Status != newStatus {
			eventID, err := WriteIssueEvent(ctx, tx, EventIssueStatusChanged, input.ID, old.ProjectID,
				input.Actor, "status", old.Status, newStatus, unclaimReason)
			if err != nil {
				return err
			}
			statusCreator, statusAssignee, err := roleContextOf(ctx, tx, old)
			if err != nil {
				return err
			}
			if _, err := notifications.FanOut(ctx, tx, notifications.FanOutInput{
				EventID:        eventID,
				EventType:      EventIssueStatusChanged,
				EntityType:     "issue",
				IssueID:        input.ID,
				ProjectID:      old.ProjectID,
				CreatorUserID:  statusCreator,
				AssigneeUserID: statusAssignee,
				ActorUserID:    input.Actor.UserID,
				ActorType:      input.Actor.Role,
			}); err != nil {
				return err
			}
			// sk-622.21 T015 (US-11): an unclaim that moves the status is
			// also "Status change" — its addressees cover the claim row's.
			return signalStatusChanged(ctx, tx, input.ID, signalRoles{})
		}
		// sk-622.21 T015 (US-11): personal signal «Unclaim».
		return signalClaim(ctx, tx, input.ID, signalRoles{})
	})
	if err != nil {
		if isSerializationFailure(err) {
			return nil, ErrNotLockHolder
		}
		return nil, err
	}
	return s.GetIssue(ctx, input.ID)
}

// Close atomically transitions an issue to status='closed', clears the
// lock, and writes closed_at + closed_reason. Predicate:
//
//   - pinned issue → ErrPinnedNoForce unless input.Force=true
//   - locked_by != caller's session AND force=false → ErrLockedByOther
//   - version != input.ExpectedVersion AND force=false → ErrVersionConflict
//
// Successful close emits two events (the second only when the issue was
// previously locked):
//
//   - issue.closed (always)
//   - issue.unclaimed (when old.locked_by != NULL — for symmetry with
//     skein_unclaim so subscribers can filter on the unclaim event-type
//     uniformly)
//
// Reason is allowed to be empty for back-compat with the legacy
// skein_update(status=closed) path; skein_close enforces non-empty at
// the tool layer.
//
// sk-489: Close is the thin wrapper — every existing call site keeps its
// two-value signature. The target-link flag (US-2) is only needed by
// the MCP acknowledgements, so it rides on CloseWithResult below rather
// than forcing an out-parameter into CloseInput.
func (s *Store) Close(ctx context.Context, input CloseInput) (*IssueDetail, error) {
	dto, _, err := s.CloseWithResult(ctx, input)
	return dto, err
}

// CloseWithResult is Close plus the close-target link state (sk-489, US-2):
// CloseLinkCreated when this call created the "duplicate" / "superseded" edge,
// CloseLinkExisted when the edge was already there (the close still
// succeeds), CloseLinkNone when no target was passed.
//
// The link is created inside the SAME SERIALIZABLE transaction as the close
// (R-5): there is no state where the issue is closed as a duplicate with a
// target given and the edge is missing, and none where the edge exists but
// the close did not happen.
func (s *Store) CloseWithResult(ctx context.Context, input CloseInput) (*IssueDetail, CloseLinkState, error) {
	state, linkState, err := s.CloseCommitted(ctx, input)
	if err != nil {
		return nil, CloseLinkNone, err
	}
	// sk-607: the close is committed. The read-back runs detached from the
	// caller's context; if even that fails, the caller gets the committed
	// state as *ErrReadBackFailed together with the link state — the close
	// and its edge happened, and an error that reads as "nothing was
	// written" would make the caller re-close and overwrite closed_reason.
	details, err := s.readBack(ctx, "close", []CommittedState{state})
	if err != nil {
		return nil, linkState, err
	}
	return details[0], linkState, nil
}

// CloseCommitted is the close writer itself: CloseWithResult without the
// read-back of the card (sk-607). It returns the post-state the committing
// transaction read — version, status, outcome, the close and lock stamps —
// and the close-target link state.
//
// For a caller that needs nothing more: skein_close_many answers each row
// with {id, ok, version, closeOutcome, closeLink}, and loading the whole
// card per row only to drop it cost a hydration per row and was the read
// production lost to the batch's own deadline.
func (s *Store) CloseCommitted(ctx context.Context, input CloseInput) (CommittedState, CloseLinkState, error) {
	linkState := CloseLinkNone
	var committed CommittedState
	// expectedVersion is required even with force=true: a SRE running a
	// force-override should still have observed current state. There's no
	// scenario where "I want to force-close but I haven't bothered to look
	// at the issue".
	if input.ExpectedVersion < 1 {
		return CommittedState{}, linkState, &ValidationError{Code: "missing_version", Message: "expected version is required"}
	}
	if err := ValidateProse("reason", input.Reason); err != nil {
		return CommittedState{}, linkState, err
	}

	// Validate commit refs up front — bad hash format short-circuits
	// before we open a SERIALIZABLE tx (cheaper) and gives the caller a
	// stable validation_error rather than a deeper-layer surprise.
	if len(input.Commits) > MaxCommitsPerClose {
		return CommittedState{}, linkState, &ValidationError{
			Code:    "too_many_commits",
			Message: fmt.Sprintf("at most %d commits per close call (got %d)", MaxCommitsPerClose, len(input.Commits)),
		}
	}
	for i, c := range input.Commits {
		if !commitHashRegex.MatchString(c.Hash) {
			return CommittedState{}, linkState, &ValidationError{
				Code:    "invalid_commit_hash",
				Message: fmt.Sprintf("commits[%d].hash must be lowercase hex of length 7..64 (got %q)", i, c.Hash),
			}
		}
	}
	if strings.TrimSpace(input.CloseTarget) != "" && input.DependenciesCaller == nil {
		return CommittedState{}, linkState, fmt.Errorf("DependenciesCaller is required when CloseTarget is set")
	}

	err := s.withSerializableRetry(ctx, "close", closeMaxAttempts, func(tx pgx.Tx) error {
		// A replayed attempt starts from scratch (sk-622.21, R17).
		linkState = CloseLinkNone
		old, err := s.fetchIssueInTx(ctx, tx, input.ID)
		if err != nil {
			return err
		}

		// sk-118 (F2e): retire-block. Closing a `standing` issue is a RETIRE,
		// and a standing task's children ARE its reason to exist — retiring
		// with an in-flight child would silently strand real work. Reject the
		// retire while ANY non-closed child remains, in EITHER hierarchy
		// representation (the canonical `parent-child` dep edge OR the legacy
		// issues.parent_id column). "Non-closed" = status <> 'closed', so
		// open / in_progress / deferred children ALL block (a deferred child is
		// real, unresolved work — §11.4). This guard fires ONLY for standing
		// issues: a normal epic with open children stays closeable (§11.9, the
		// open-children list there is a tool-layer affordance, not a store
		// contract). It is NOT bypassed by force — the block is a structural
		// invariant (mirrors the F2c nested-standing matrix stance), so it runs
		// before the pinned / lock / version predicates.
		if old.Status == StatusStanding {
			childIDs, cerr := standingOpenChildIDs(ctx, tx, input.ID)
			if cerr != nil {
				return cerr
			}
			if len(childIDs) > 0 {
				return newStandingHasOpenChildren(childIDs)
			}
		}

		// Pinned check first — same ordering as REST: pinned → lock → version.
		if old.Pinned && !input.Force {
			return ErrPinnedNoForce
		}
		// Lock predicate.
		if err := lockGuard(old, input.SessionID, input.Actor.UserID, input.Force); err != nil {
			return err
		}
		// Version predicate.
		if !input.Force && old.Version != input.ExpectedVersion {
			return &ErrVersionConflict{CurrentVersion: old.Version}
		}

		var reasonArg any
		if input.Reason != "" {
			reasonArg = input.Reason
		}

		// sk-303.14 / T009: the review mark is reset on the TRANSITION
		// into closed, not on every closing call. The predicate is the
		// old status, and it is what splits the two cases the spec
		// requires distinguishing: an issue explicitly reopened and
		// closed again returns to the unreviewed set (SC-023), while a
		// technical re-close of an ALREADY closed issue (rewriting the
		// reason, appending commits) does not touch the mark — otherwise
		// an author who marked the result would see it as unreviewed
		// again because of someone else's close-reason edit.
		resetReview := old.Status != StatusClosed

		// sk-489 (T003): the outcome is decided by ONE helper for both close
		// writers — DecideCloseOutcome. All target checks are caught here too:
		// a value outside the four, a target without an explicit outcome, a target with
		// "done" / "won't do", a target that is the issue itself. The decision
		// is taken AFTER the lock and version predicates: a close that lost
		// the version race must answer with a version conflict, not with a parse
		// of its own input.
		previousOutcome := ""
		if old.CloseOutcome != nil {
			previousOutcome = *old.CloseOutcome
		}
		decision, derr := DecideCloseOutcome(CloseOutcomeRequest{
			IssueID:         input.ID,
			Requested:       input.CloseOutcome,
			Target:          input.CloseTarget,
			AlreadyClosed:   old.Status == StatusClosed,
			PreviousOutcome: previousOutcome,
			// Archive is not set here and CloseInput has no such
			// field: archiving goes through UpdateStatus (US-11).
			Standing:            old.Status == StatusStanding,
			RequireOnTransition: input.RequireOutcomeOnTransition,
		})
		if derr != nil {
			return derr
		}
		// sk-540.2 exception (a): before any write.
		if err := ensureCloseLinkExists(ctx, tx, input.ID, decision); err != nil {
			return err
		}
		// Branch (2) of the review-mark reset predicate (US-3): an outcome change on an already
		// closed issue. Branch (1) — the resetReview line above — does not
		// change.
		resetReview = resetReview ||
			OutcomeChangeResetsReview(decision.Previous, decision.Outcome,
				input.Actor.UserID, old.CreatedBy)

		ct, err := tx.Exec(ctx, `
UPDATE issues
   SET status        = 'closed',
       closed_at     = now(),
       closed_reason = $2,
       close_outcome = $8::text,
       locked_by     = NULL,
       locked_at     = NULL,
       locked_by_user_id = NULL,
       reviewed_at   = CASE WHEN $6::boolean THEN NULL ELSE reviewed_at END,
       reviewed_by   = CASE WHEN $6::boolean THEN NULL ELSE reviewed_by END,
       version       = version + 1,
       updated_at    = now()
 WHERE id = $1
   AND ($4::boolean OR version = $3)
   AND ($4::boolean OR locked_by IS NULL
        OR (locked_by = $5::text AND locked_by_user_id = $7::uuid))
   AND ($4::boolean OR pinned = false)`,
			input.ID, reasonArg, input.ExpectedVersion, input.Force, input.SessionID,
			resetReview, input.Actor.UserID, decision.Outcome)
		if err != nil {
			return fmt.Errorf("closing issue: %w", err)
		}
		if ct.RowsAffected() == 0 {
			// Fallthrough returns version_conflict; the rare race could be pinned or lock change but this is dominated by SERIALIZABLE 40001 mapping.
			return &ErrVersionConflict{CurrentVersion: old.Version}
		}

		eventReason := buildForceReason(input.Reason, input.Force)
		// sk-489: both close events carry the outcome in the payload. new_data of
		// status_changed stays a JSON status string — it is read by
		// outbox/push.go#delegatedStatusHead, including on an old pod in
		// the rollout window.
		closePayload := closeOutcomePayload(decision)
		closePayloadJSON, err := json.Marshal(closePayload)
		if err != nil {
			return fmt.Errorf("marshalling close payload for %s: %w", input.ID, err)
		}
		// status_changed for back-compat with subscribers that filter the
		// generic event-type. Notification fan-out goes through
		// status_changed (issue.closed isn't in the notifiable set).
		eventID, err := WriteIssueEventWithPayload(ctx, tx, EventIssueStatusChanged, input.ID, old.ProjectID,
			input.Actor, "status", old.Status, StatusClosed, eventReason, closePayload)
		if err != nil {
			return err
		}
		statusCreator, statusAssignee, err := roleContextOf(ctx, tx, old)
		if err != nil {
			return err
		}
		if _, err := notifications.FanOut(ctx, tx, notifications.FanOutInput{
			EventID:        eventID,
			EventType:      EventIssueStatusChanged,
			EntityType:     "issue",
			IssueID:        input.ID,
			ProjectID:      old.ProjectID,
			CreatorUserID:  statusCreator,
			AssigneeUserID: statusAssignee,
			ActorUserID:    input.Actor.UserID,
			ActorType:      input.Actor.Role,
			Payload:        closePayloadJSON,
		}); err != nil {
			return err
		}

		// sk-551.10, item 5 — strip the automatic subscriptions of people
		// with «unwatch on close» on; after the fan-out, so they still get
		// the closing notification.
		if _, err := notifications.StripAutoWatchersOnClose(ctx, tx, input.ID); err != nil {
			return err
		}

		// Dedicated issue.closed event.
		if _, err := WriteIssueEventWithPayload(ctx, tx, EventIssueClosed, input.ID, old.ProjectID,
			input.Actor, "status", old.Status, StatusClosed, eventReason, closePayload); err != nil {
			return err
		}

		// A repeated close that changed the outcome is also an outcome change
		// (US-3): its own event and its own fan-out, shared with the other writer and
		// with the edit.
		if decision.Changed {
			if err := writeOutcomeChangedInTx(ctx, tx, input.ID, old, input.Actor, decision, eventReason); err != nil {
				return err
			}
		}

		// issue.unclaimed when the issue was previously locked.
		if old.LockedBy != nil && *old.LockedBy != "" {
			unclaimReason := "auto-unclaimed-on-close"
			if input.Force {
				unclaimReason = "force-overridden"
			}
			if _, err := WriteIssueEvent(ctx, tx, EventIssueUnclaimed, input.ID, old.ProjectID,
				input.Actor, "lockedBy",
				map[string]any{"lockedBy": *old.LockedBy, "lockedAt": old.LockedAt},
				map[string]any{"lockedBy": nil, "lockedAt": nil},
				unclaimReason); err != nil {
				return err
			}
			// sk-674.18 T034: a forced close releases another holder's claim.
			if forceReleasesAnother(old, input.Actor, input.Force) {
				if err := writeForceReleased(ctx, tx, input.ID, old, input.Actor, unclaimReason); err != nil {
					return err
				}
			}
		}

		// Commit-on-close: resolve each {repoUrl?, repoId?} into a
		// uuid against the project's git_repos[], then INSERT into
		// issue_commits inside the same tx. Re-close with the same
		// (issue_id, repo_id, hash) is idempotent (ON CONFLICT DO
		// NOTHING) — the agent can re-run skein_close after a
		// transient failure without duplicating rows.
		if len(input.Commits) > 0 {
			if err := linkCommitsInTx(ctx, tx, input.ID, old.ProjectID, input.Actor, input.Commits); err != nil {
				return err
			}
		}

		// The link by target — in the same transaction (R-5). It goes last:
		// dependencies.CreateInTx reads both sides of the edge, and by that
		// moment the issue being closed is already in its final state.
		if target := strings.TrimSpace(input.CloseTarget); target != "" {
			state, lerr := linkCloseTargetInTx(ctx, tx, input.ID, target,
				decision.Outcome, *input.DependenciesCaller, input.Actor)
			if lerr != nil {
				if errors.Is(lerr, errCloseLinkRaceLost) {
					return &ErrVersionConflict{CurrentVersion: old.Version}
				}
				return lerr
			}
			linkState = state
		}
		// sk-607: the post-state, read last so it reflects every write of
		// this transaction whatever is added above it later.
		st, serr := committedStatesInTx(ctx, tx, input.ID)
		if serr != nil {
			return serr
		}
		committed = st[0]
		// sk-622.21 T015 (US-11): personal signal "close".
		return signalStatusChanged(ctx, tx, input.ID, signalRoles{})
	})
	if err != nil {
		// A race for the edge rolls back the whole transaction, so linkState
		// set inside a failed attempt must not go outward.
		return CommittedState{}, CloseLinkNone, s.degradeSerializationAbort(ctx, err, "close", input.ID, &input.ExpectedVersion)
	}
	afterCommit(ctx, "close", input.ID)
	return committed, linkState, nil
}

// AttachCommits links one or more commit refs to an existing issue
// WITHOUT touching status, closed_reason, closed_at, version, or
// locked_by. This is the retroactive counterpart to skein_close's
// `commits[]`: when an agent forgot to pass `commits[]` on the
// original close (or wants to add a hash that landed later), it
// calls AttachCommits instead of re-running Close — re-close would
// overwrite the original closed_reason with the new reason string
// (sk-35).
//
// Predicate (force=false):
//
//   - locked_by != caller's session AND locked_by IS NOT NULL → ErrLockedByOther
//
// Pinned issues are NOT gated: attach is a metadata-add, semantically
// safer than the destructive operations the pin flag protects against
// (close / reopen / re-prioritise). Version is NOT checked: attach
// doesn't read-then-write any field that could race, and forcing the
// agent to chase a version conflict for a pure-add operation would
// make agents reach for force=true reflexively. The DB unique
// (issue_id, repo_id, hash) + ON CONFLICT DO NOTHING is the actual
// idempotency anchor.
//
// Validation (up-front, before tx opens):
//
//   - len(Commits) in [1, MaxCommitsPerClose] → ValidationError
//   - every hash matches commitHashRegex → ValidationError
//
// Success:
//
//   - INSERTs into issue_commits via linkCommitsInTx (resolves repos,
//     dedups on (issue_id, repo_id, hash))
//   - writes ONE issue.commits_attached event with new_data =
//     {hashes: [<distinct hashes in input order>]}
//   - returns the full IssueDetail post-state (CommitRefs reflects the
//     merge of previously-attached + newly-attached, sorted by
//     linked_at as the existing reader expects)
//
// No notification fan-out: attach is an audit-only signal. Watchers
// who care about commits already follow issue.closed (where the
// commits typically land first) — broadcasting a second-class
// "we forgot a hash and added it later" wakeup would be noise.
func (s *Store) AttachCommits(ctx context.Context, input AttachCommitsInput) (*IssueDetail, error) {
	if len(input.Commits) == 0 {
		return nil, &ValidationError{
			Code:    "no_commits",
			Message: "commits must be non-empty",
		}
	}
	if len(input.Commits) > MaxCommitsPerClose {
		return nil, &ValidationError{
			Code:    "too_many_commits",
			Message: fmt.Sprintf("at most %d commits per attach call (got %d)", MaxCommitsPerClose, len(input.Commits)),
		}
	}
	for i, c := range input.Commits {
		if !commitHashRegex.MatchString(c.Hash) {
			return nil, &ValidationError{
				Code:    "invalid_commit_hash",
				Message: fmt.Sprintf("commits[%d].hash must be lowercase hex of length 7..64 (got %q)", i, c.Hash),
			}
		}
	}

	// Capture distinct hashes (in input order) for the event payload.
	// We dedup here rather than reading back from the DB so the event
	// reflects the agent's request shape; the actual row inserts are
	// deduped by ON CONFLICT in linkCommitsInTx.
	seen := make(map[string]struct{}, len(input.Commits))
	hashes := make([]string, 0, len(input.Commits))
	for _, c := range input.Commits {
		if _, ok := seen[c.Hash]; ok {
			continue
		}
		seen[c.Hash] = struct{}{}
		hashes = append(hashes, c.Hash)
	}

	err := signals.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		old, err := s.fetchIssueInTx(ctx, tx, input.ID)
		if err != nil {
			return err
		}
		if err := lockGuard(old, input.SessionID, input.Actor.UserID, input.Force); err != nil {
			return err
		}

		if err := linkCommitsInTx(ctx, tx, input.ID, old.ProjectID, input.Actor, input.Commits); err != nil {
			return err
		}

		if _, err := WriteIssueEvent(ctx, tx, EventIssueCommitsAttached, input.ID, old.ProjectID,
			input.Actor, "commits", nil, map[string]any{"hashes": hashes}, ""); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, s.degradeSerializationAbort(ctx, err, "attachCommits", input.ID, nil)
	}
	return s.GetIssue(ctx, input.ID)
}

// DetachCommits removes commit links from an issue WITHOUT touching
// status, closed_reason, closed_at, version or locked_by — the mirror
// image of AttachCommits, and the answer to sk-394.
//
// # Why removal has to exist at all
//
// A hash is accepted on its FORM alone: lowercase hex, 7..64 chars.
// Skein has no clone of the repository and cannot ask whether the
// object exists, so an abbreviation padded out by hand — this is a real
// case, not a hypothesis — satisfies the contract and is written. Until
// this method existed there was no way back: no MCP tool, no REST
// route, nothing but hand-editing issue_commits in the database. A
// commit ref is the evidentiary part of the tracker, and a wrong one
// could only be found by walking to git; permanence turned a single
// mistyped argument into a permanent lie.
//
// # Predicate and shape
//
// Lock guard, force and version handling are AttachCommits's, verbatim:
//
//   - locked_by != caller's session AND locked_by IS NOT NULL → ErrLockedByOther
//   - version is NOT checked — the row this touches is not issues
//   - pinned issues are NOT gated
//
// Matching is by hash, not by (repo, hash); DetachCommitsInput explains
// why the asymmetry with attach is deliberate rather than an omission.
//
// Validation (up-front, before the tx opens):
//
//   - len(Hashes) in [1, MaxCommitsPerClose] → ValidationError
//   - every hash matches commitHashRegex → ValidationError
//
// The hash shape is validated on the way OUT too, and that is not
// symmetry for its own sake: an unvalidated detach would accept the
// garbage an attach rejects and answer `notFound`, which reads as «no
// such link on this issue» — indistinguishable from a correctly-spelled
// hash that simply is not linked. The caller would learn nothing about
// their own typo.
//
// Success:
//
//   - DELETEs the matching issue_commits rows, RETURNING (repo_id, hash)
//   - writes ONE issue.commits_detached event with old_data =
//     {hashes: [<hashes actually removed, in input order>],
//     refs: [{repoId, hash} of every row actually removed]} — and NO
//     event when nothing was removed (see EventIssueCommitsDetached for
//     why this differs from attach)
//   - answers with the post-state plus the per-hash verdict: what left
//     (Detached) and what was never there (NotFound)
//
// No notification fan-out, for AttachCommits's reason: commit links are
// an audit-only signal.
func (s *Store) DetachCommits(ctx context.Context, input DetachCommitsInput) (*DetachCommitsResult, error) {
	if len(input.Hashes) == 0 {
		return nil, &ValidationError{
			Code:    "no_commits",
			Message: "hashes must be non-empty",
		}
	}
	if len(input.Hashes) > MaxCommitsPerClose {
		return nil, &ValidationError{
			Code:    "too_many_commits",
			Message: fmt.Sprintf("at most %d hashes per detach call (got %d)", MaxCommitsPerClose, len(input.Hashes)),
		}
	}
	for i, h := range input.Hashes {
		if !commitHashRegex.MatchString(h) {
			return nil, &ValidationError{
				Code:    "invalid_commit_hash",
				Message: fmt.Sprintf("hashes[%d] must be lowercase hex of length 7..64 (got %q)", i, h),
			}
		}
	}

	// Distinct hashes in input order: the verdict below is reported per
	// hash the caller named, and naming one twice must not make it look
	// like two rows left.
	seen := make(map[string]struct{}, len(input.Hashes))
	wanted := make([]string, 0, len(input.Hashes))
	for _, h := range input.Hashes {
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		wanted = append(wanted, h)
	}

	var removed []DetachedCommitRef
	err := signals.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		old, err := s.fetchIssueInTx(ctx, tx, input.ID)
		if err != nil {
			return err
		}
		if err := lockGuard(old, input.SessionID, input.Actor.UserID, input.Force); err != nil {
			return err
		}

		// Re-read on retry: a SERIALIZABLE abort re-runs this closure, and
		// a stale slice from the first attempt would be reported as this
		// attempt's removals.
		removed = removed[:0]
		rows, err := tx.Query(ctx,
			`DELETE FROM issue_commits
              WHERE issue_id = $1 AND hash = ANY($2)
          RETURNING repo_id, hash`,
			input.ID, wanted)
		if err != nil {
			return fmt.Errorf("deleting issue_commits: %w", err)
		}
		for rows.Next() {
			var ref DetachedCommitRef
			if err := rows.Scan(&ref.RepoID, &ref.Hash); err != nil {
				rows.Close()
				return fmt.Errorf("scanning detached issue_commits: %w", err)
			}
			removed = append(removed, ref)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("reading detached issue_commits: %w", err)
		}

		if len(removed) == 0 {
			return nil
		}

		if _, err := WriteIssueEvent(ctx, tx, EventIssueCommitsDetached, input.ID, old.ProjectID,
			input.Actor, "commits", map[string]any{
				"hashes": detachedHashesInOrder(wanted, removed),
				"refs":   detachedRefsInOrder(wanted, removed),
			}, nil, ""); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, s.degradeSerializationAbort(ctx, err, "detachCommits", input.ID, nil)
	}

	detail, err := s.GetIssue(ctx, input.ID)
	if err != nil {
		return nil, err
	}

	gone := make(map[string]struct{}, len(removed))
	for _, r := range removed {
		gone[r.Hash] = struct{}{}
	}
	notFound := make([]string, 0, len(wanted))
	for _, h := range wanted {
		if _, ok := gone[h]; !ok {
			notFound = append(notFound, h)
		}
	}

	return &DetachCommitsResult{
		Issue:    detail,
		Detached: removed,
		NotFound: notFound,
	}, nil
}

// detachedHashesInOrder projects the removed rows onto the caller's
// input order, one entry per hash however many repos it was filed
// under. The event answers «which commits stopped being linked», so a
// hash that lived in two repos is one answer, not two.
func detachedHashesInOrder(wanted []string, removed []DetachedCommitRef) []string {
	gone := make(map[string]struct{}, len(removed))
	for _, r := range removed {
		gone[r.Hash] = struct{}{}
	}
	out := make([]string, 0, len(gone))
	for _, h := range wanted {
		if _, ok := gone[h]; ok {
			out = append(out, h)
		}
	}
	return out
}

// detachedRefRecord is one entry of the `refs` key of the
// issue.commits_detached payload: a removed issue_commits row.
type detachedRefRecord struct {
	RepoID string `json:"repoId"`
	Hash   string `json:"hash"`
}

// detachedRefsInOrder lists every removed row as a (repoId, hash) pair
// (sk-540.9). Where `hashes` answers «which commits stopped being linked»
// with one entry per hash, `refs` answers «which links were removed»: a
// hash filed under two repos is two entries here. Detach matches by hash
// alone, so without the repo the audit trail of a multi-repo issue could
// not say what was actually taken away.
//
// Order is deterministic so two readers of one event agree: the caller's
// input order of hashes, then repo id within a hash (RETURNING order is
// not defined).
func detachedRefsInOrder(wanted []string, removed []DetachedCommitRef) []detachedRefRecord {
	rank := make(map[string]int, len(wanted))
	for i, h := range wanted {
		rank[h] = i
	}
	out := make([]detachedRefRecord, 0, len(removed))
	for _, r := range removed {
		out = append(out, detachedRefRecord{RepoID: r.RepoID.String(), Hash: r.Hash})
	}
	slices.SortFunc(out, func(a, b detachedRefRecord) int {
		if d := rank[a.Hash] - rank[b.Hash]; d != 0 {
			return d
		}
		return strings.Compare(a.RepoID, b.RepoID)
	})
	return out
}

// Accept marks the issue as acknowledged by the current assignee.
// Phase 2 of the ui-redesign acceptance flow — gives the creator an
// explicit signal that the assignee has seen the task and is taking it
// (vs the auto-accept on self-assign and reset-on-reassign mechanics
// from Phase 1).
//
// Permission: caller MUST be the issue's current assignee. Only the
// assignee accepts; the project owner's rights and role=admin do not let
// anyone accept for another (US-6, sk-741) — otherwise non-assignees could
// pre-accept tasks they were never assigned to. ErrForbidden is the
// sentinel for the deny path.
//
// Idempotency: if the issue is already accepted (accepted_at IS NOT
// NULL) the method short-circuits and returns the current detail
// without writing anything (no version bump, no event row, no fan-
// out). This is a deliberate silent no-op — the same agent calling
// skein_accept twice on the same issue does not create duplicate
// events or churn the inbox.
//
// Optimistic concurrency: AcceptInput.ExpectedVersion is optional
// (nil skips the check). Non-nil = enforced inside the SERIALIZABLE
// tx; mismatch returns ErrVersionConflict with the live version
// (mirrors Close's contract).
//
// Side-effects on success:
//   - SET accepted_at = now(), accepted_by = caller's user_id,
//     version + 1, updated_at = now()
//   - WriteIssueEvent("issue.accepted") with field="accepted_at",
//     payload encoding the new acceptedBy/acceptedAt pair
//   - FanOut over notifiableLifecycle (creator + watchers see the
//     event in their inbox)
//
// Errors:
//   - ErrNotFound      — issue id doesn't exist
//   - ErrForbidden     — caller is not the assignee (owner rights and
//     role=admin do not count)
//   - ErrVersionConflict — only when ExpectedVersion was non-nil and
//     the live version differs
func (s *Store) Accept(ctx context.Context, input AcceptInput) (*IssueDetail, error) {
	err := s.withSerializableRetry(ctx, "accept", acceptMaxAttempts, func(tx pgx.Tx) error {
		old, err := s.fetchIssueInTx(ctx, tx, input.ID)
		if err != nil {
			return err
		}

		// sk-25: closed is terminal. Accept on a done issue is
		// meaningless — the work it would acknowledge has already
		// concluded. Reopen via skein_update(status=open) first if the
		// closure itself was wrong. Symmetric with Claim's
		// ErrCannotClaimClosed guard. This check runs BEFORE the
		// permission gate so a non-assignee asking to accept a closed
		// row gets the precise reason ("can't, it's closed") instead
		// of a misleading 'forbidden'.
		if old.Status == StatusClosed {
			return ErrCannotAcceptClosed
		}

		// Permission gate: the caller's users.id must resolve from the
		// issue's assignee column — only the assignee accepts. (The
		// instance admin's override is gone, sk-674.18 T017: he accepts
		// only his own assignments, as the project owner does, US-6.) The
		// assignee is stored as a free-form string so we go through
		// notifications.ResolveAssigneeUserID — single round-trip,
		// case-insensitive on users.username, returns uuid.Nil on no
		// match (which we treat as "not the assignee" → forbidden).
		if old.Assignee == nil || *old.Assignee == "" {
			return ErrForbidden
		}
		assigneeUserID, err := notifications.ResolveAssigneeUserID(ctx, tx, *old.Assignee, old.ProjectID)
		if err != nil {
			return err
		}
		if assigneeUserID == uuid.Nil || assigneeUserID != input.Actor.UserID {
			return ErrForbidden
		}

		// Optimistic concurrency — only when caller supplied a value.
		// nil = "I don't care which version". This matches the spec
		// contract; existing single-state-flip operations (Close,
		// Pin) require ExpectedVersion always, but Accept is the
		// first method where omitting it is a legitimate use case
		// (REST UI flows where the user clicks "Accept" without
		// having read the latest version).
		if input.ExpectedVersion != nil && old.Version != *input.ExpectedVersion {
			return &ErrVersionConflict{CurrentVersion: old.Version}
		}

		// Idempotent path: already accepted → silent no-op. We do NOT
		// re-stamp accepted_at; the original timestamp is the truth.
		// Returning nil here lets the post-tx tail re-fetch and
		// return the fresh DTO consistently.
		// fetchIssueInTx already loaded AcceptedAt into old — no second SELECT.
		//
		// sk-303.13 / T021: the same decision the automatic paths make,
		// with explicitTool=true. Having one classifier answer for all four
		// paths is the point — "no transition, no event" and "explicit
		// acceptance is the only one that delivers" are then one rule with
		// four call sites instead of four hand-matched copies.
		acceptance := ClassifyAcceptance(old.AcceptedAt != nil, true, true)
		if !acceptance.WriteEvent {
			return nil
		}

		now := time.Now().UTC()
		ct, err := tx.Exec(ctx, `
UPDATE issues
   SET accepted_at = $2,
       accepted_by = $3,
       version     = version + 1,
       updated_at  = now()
 WHERE id = $1
   AND accepted_at IS NULL`,
			input.ID, now, input.Actor.UserID,
		)
		if err != nil {
			return fmt.Errorf("accepting issue: %w", err)
		}
		if ct.RowsAffected() == 0 {
			// Race: another writer accepted between our SELECT and
			// our UPDATE. Treat as idempotent — the desired post-
			// state ("accepted") holds.
			return nil
		}

		eventID, err := WriteIssueEvent(ctx, tx, EventIssueAccepted, input.ID, old.ProjectID,
			input.Actor, "accepted_at",
			map[string]any{"acceptedBy": nil, "acceptedAt": nil},
			map[string]any{"acceptedBy": input.Actor.UserID, "acceptedAt": now},
			"")
		if err != nil {
			return err
		}

		if !acceptance.Deliver {
			// Unreachable on this path (explicitTool=true always delivers)
			// and asserted rather than defended: if the classifier ever
			// says otherwise, the explicit acceptance must stop notifying
			// too, not keep an ad-hoc exception here.
			return nil
		}

		acceptCreator, acceptAssignee, err := roleContextOf(ctx, tx, old)
		if err != nil {
			return err
		}
		if _, err := notifications.FanOut(ctx, tx, notifications.FanOutInput{
			EventID:    eventID,
			EventType:  EventIssueAccepted,
			EntityType: "issue",
			IssueID:    input.ID,
			ProjectID:  old.ProjectID,
			// "Issue accepted by assignee" pushes the CREATOR — the person
			// who filed it is the one waiting for the acknowledgement.
			CreatorUserID:  acceptCreator,
			AssigneeUserID: acceptAssignee,
			// This is the EXPLICIT acceptance tool. Acceptance stamped
			// automatically (claim, self-assign, create-on-self) is
			// journal-only and carries no reason — see EventFacts.
			ExplicitAccept: true,
			ActorUserID:    input.Actor.UserID,
			ActorType:      input.Actor.Role,
		}); err != nil {
			return err
		}

		// sk-622.21 T015 (US-11): personal signal "Issue accepted".
		roles, err := readSignalRoles(ctx, tx, input.ID)
		if err != nil {
			return err
		}
		signalAccepted(tx, roles)
		return nil
	})
	if err != nil {
		return nil, s.degradeSerializationAbort(ctx, err, "accept", input.ID, input.ExpectedVersion)
	}
	// Re-fetch via GetIssue so the caller gets the same hydrated shape
	// regardless of which path inside the tx ran (fresh-accept,
	// already-accepted, or race-loser).
	return s.GetIssue(ctx, input.ID)
}

// DelegationActionInput is the request shape for Store.DismissDelegated
// and Store.ResumeDelegated. The actor's user_id is the row appended to
// (or removed from) issues.dismissed_by; Actor carries the audit context
// for the event row.
type DelegationActionInput struct {
	ID    string
	Actor Actor
}

// DismissDelegated adds the caller's user_id to issues.dismissed_by and
// emits an issue.delegation_dropped event. Idempotent — calling on an
// issue where the caller is already in dismissed_by returns the current
// IssueDetail without writing.
//
// Side effects:
//
//   - The caller's subscription is stopped in the same tx through the
//     explicit-unwatch primitive (notifications.UnwatchExplicitTx,
//     sk-622.21): the subscription row is deleted, an opt-out is recorded
//     when there was one, and the issue's Watching card is cleared on the
//     caller's notifications. The user can call
//     skein_watchers(action="watch") later to resume bell notifications
//     independently of resuming the Delegated lens (see ResumeDelegated
//     below — the symmetric inverse does NOT re-watch).
//
//   - Visibility check is the CALLER'S responsibility (handler / tool
//     layer); this method assumes the issue is visible.
//
// sk-34.
func (s *Store) DismissDelegated(ctx context.Context, input DelegationActionInput) (*IssueDetail, error) {
	if input.Actor.UserID == uuid.Nil {
		return nil, &ValidationError{Code: "missing_actor", Message: "actor user id is required"}
	}
	err := s.withSerializableRetry(ctx, "dismissDelegated", dismissDelegatedMaxAttempts, func(tx pgx.Tx) error {
		old, err := s.fetchIssueInTx(ctx, tx, input.ID)
		if err != nil {
			return err
		}

		// sk-34 — chain-membership precondition. Only the creator OR a
		// past assignee may dismiss. Random project members must not be
		// able to scribble onto dismissed_by / record opt-outs on
		// arbitrary issues. We read created_by + previous_assignees
		// inside the tx so the check sees the same row as the writes
		// below.
		var createdBy uuid.UUID
		var previousAssignees []uuid.UUID
		if err := tx.QueryRow(ctx,
			`SELECT created_by, previous_assignees FROM issues WHERE id = $1`,
			input.ID,
		).Scan(&createdBy, &previousAssignees); err != nil {
			return fmt.Errorf("reading chain membership: %w", err)
		}
		inChain := createdBy == input.Actor.UserID
		if !inChain {
			for _, u := range previousAssignees {
				if u == input.Actor.UserID {
					inChain = true
					break
				}
			}
		}
		if !inChain {
			return &ValidationError{
				Code:    "not_in_chain",
				Message: "you must be the creator or have been a previous assignee to dismiss this issue",
			}
		}

		// Idempotent: already dismissed → skip the array-append + the
		// event-emission, but still run the explicit unwatch below. A user
		// may have dismissed earlier and then re-watched by hand
		// (skein_watchers(action="watch"), which also deletes the opt-out);
		// a second dismiss must stop that subscription again.
		var already bool
		if err := tx.QueryRow(ctx,
			`SELECT $2::uuid = ANY(dismissed_by) FROM issues WHERE id = $1`,
			input.ID, input.Actor.UserID,
		).Scan(&already); err != nil {
			return fmt.Errorf("reading dismissed_by: %w", err)
		}

		// sk-622.21 (US-1, US-6): declining a delegation is an explicit
		// "stop watching" — the same primitive as the single unwatch.
		// With a subscription (manual included): it is deleted and the
		// opt-out is recorded, so the automatic paths do not re-subscribe
		// the caller; with or without one, the Watching card of this issue
		// is cleared on all of the caller's notifications. Same SERIALIZABLE
		// transaction as the drop, under this path's retry (K2, R17).
		unwatched, err := notifications.UnwatchExplicitTx(ctx, tx, input.ID, input.Actor.UserID)
		if err != nil {
			return err
		}
		watchingChanged := unwatched.HadSubscription || unwatched.CardsCleared > 0

		if already {
			// No transition — no array-mutation, no event, no version bump.
			// sk-622.21 T015: only the Watching of the caller may have
			// changed (a re-watched subscription stopped again).
			if watchingChanged {
				signals.Add(tx, input.Actor.UserID, signals.AreaWatching)
			}
			return nil
		}

		if _, err := tx.Exec(ctx, `
UPDATE issues
   SET dismissed_by = array_append(dismissed_by, $2::uuid)
 WHERE id = $1
   AND NOT $2::uuid = ANY(dismissed_by)`,
			input.ID, input.Actor.UserID,
		); err != nil {
			return fmt.Errorf("appending dismissed_by: %w", err)
		}

		// Audit-only event — no FanOut. The caller is opting OUT of
		// notifications on this issue; broadcasting the drop would
		// undo the bell-silence we just installed.
		if _, err := WriteIssueEvent(ctx, tx, EventIssueDelegationDropped, input.ID, old.ProjectID,
			input.Actor, "", nil, nil, ""); err != nil {
			return err
		}
		// sk-622.21 T015 (US-11): personal signal "Delegation refused".
		signalDelegation(tx, input.Actor.UserID, watchingChanged)
		return nil
	})
	if err != nil {
		return nil, s.degradeSerializationAbort(ctx, err, "dismissDelegated", input.ID, nil)
	}
	return s.GetIssue(ctx, input.ID)
}

// ResumeDelegated removes the caller's user_id from issues.dismissed_by
// and emits an issue.delegation_resumed event. Symmetric inverse of
// DismissDelegated.
//
// Does NOT re-add the watcher row — the subscription DismissDelegated
// stopped (and its opt-out) stays in effect until the user explicitly calls
// skein_watchers(action="watch") (REST: POST /issues/{id}/watch). This
// decoupling matches
// the design: "I want to see this issue in Delegated again" and "I
// want bell notifications" are two independent user intents.
//
// Idempotent — calling on an issue where the caller is not in
// dismissed_by returns the current IssueDetail without writing.
//
// sk-34.
func (s *Store) ResumeDelegated(ctx context.Context, input DelegationActionInput) (*IssueDetail, error) {
	if input.Actor.UserID == uuid.Nil {
		return nil, &ValidationError{Code: "missing_actor", Message: "actor user id is required"}
	}
	err := signals.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		old, err := s.fetchIssueInTx(ctx, tx, input.ID)
		if err != nil {
			return err
		}

		var present bool
		if err := tx.QueryRow(ctx,
			`SELECT $2::uuid = ANY(dismissed_by) FROM issues WHERE id = $1`,
			input.ID, input.Actor.UserID,
		).Scan(&present); err != nil {
			return fmt.Errorf("reading dismissed_by: %w", err)
		}
		if !present {
			return nil
		}

		if _, err := tx.Exec(ctx, `
UPDATE issues
   SET dismissed_by = array_remove(dismissed_by, $2::uuid)
 WHERE id = $1`,
			input.ID, input.Actor.UserID,
		); err != nil {
			return fmt.Errorf("removing from dismissed_by: %w", err)
		}

		if _, err := WriteIssueEvent(ctx, tx, EventIssueDelegationResumed, input.ID, old.ProjectID,
			input.Actor, "", nil, nil, ""); err != nil {
			return err
		}
		// sk-622.21 T015 (US-11): personal signal "Delegation
		// resumed" — the subscription is not restored, so no watching.
		signalDelegation(tx, input.Actor.UserID, false)
		return nil
	})
	if err != nil {
		return nil, s.degradeSerializationAbort(ctx, err, "resumeDelegated", input.ID, nil)
	}
	return s.GetIssue(ctx, input.ID)
}

// linkCommitsInTx resolves each CloseCommitInput against the project's
// git_repos[] and INSERTs an issue_commits row per resolved (repoID,
// hash) pair. Runs entirely inside the supplied SERIALIZABLE tx so
// resolve+insert is atomic with the status flip.
//
// The project's git_repos[] is loaded once at the top — the slice is
// invariant for the duration of one Close, so resolving N commits
// against it requires zero additional SELECTs. INSERTs are then
// collapsed into one batched VALUES (...), (...), ... ON CONFLICT DO
// NOTHING statement, dropping per-commit round-trips inside the
// SERIALIZABLE tx.
//
// Errors:
//   - projects.ErrRepoNotFound when repoID/repoURL doesn't match
//   - projects.ErrRepoAmbiguous when neither is set and the project
//     has zero or multiple repos.
//
// Both surface to the caller as-is; the tool layer maps them to
// validation_error envelopes (see mapCloseError).
func linkCommitsInTx(
	ctx context.Context,
	tx pgx.Tx,
	issueID string,
	projectID uuid.UUID,
	actor Actor,
	commits []CloseCommitInput,
) error {
	if len(commits) == 0 {
		return nil
	}

	// Load git_repos[] once — invariant for the rest of this tx.
	repos, err := projects.LoadProjectRepos(ctx, tx, projectID)
	if err != nil {
		return fmt.Errorf("loading project repos: %w", err)
	}

	// linked_by / linked_by_kind are uniform across the batch — same
	// actor, same close call. Compute once and reuse for every row.
	var linkedByArg any
	if actor.UserID != uuid.Nil && actor.Role != "system" {
		linkedByArg = actor.UserID
	}
	linkedByKind := actor.Role
	if linkedByKind == "" {
		linkedByKind = "system"
	}

	// Build one batched INSERT with N value-tuples. Nine columns per
	// row; placeholders run $1, $2, ... in row-major order.
	const colsPerRow = 9
	args := make([]any, 0, len(commits)*colsPerRow)
	var sb strings.Builder
	// Header ~160B + ~80B per row of placeholders/punctuation (max 10 rows).
	sb.Grow(256 + len(commits)*80)
	sb.WriteString(`INSERT INTO issue_commits
    (issue_id, repo_id, hash, url, message, author, committed_at, linked_by, linked_by_kind)
VALUES `)
	for i, c := range commits {
		repoID, err := projects.ResolveRepoFromList(repos, c.RepoURL, c.RepoID)
		if err != nil {
			return fmt.Errorf("commits[%d]: %w", i, err)
		}

		var urlArg, msgArg, authorArg, committedAtArg any
		if c.URL != nil {
			urlArg = *c.URL
		}
		if c.Message != nil {
			msgArg = *c.Message
		}
		if c.Author != nil {
			authorArg = *c.Author
		}
		if c.CommittedAt != nil {
			committedAtArg = c.CommittedAt.UTC()
		}

		if i > 0 {
			sb.WriteString(", ")
		}
		base := i * colsPerRow
		fmt.Fprintf(&sb, "($%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d)",
			base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8, base+9)

		args = append(args,
			issueID, repoID, c.Hash,
			urlArg, msgArg, authorArg, committedAtArg,
			linkedByArg, linkedByKind,
		)
	}
	sb.WriteString(`
ON CONFLICT (issue_id, repo_id, hash) DO NOTHING`)

	if _, err := tx.Exec(ctx, sb.String(), args...); err != nil {
		return fmt.Errorf("inserting issue_commits: %w", err)
	}
	return nil
}

// ----------------- Internal helpers -----------------

// issueRow is the in-transaction snapshot used by Patch/UpdateStatus/Pin
// to compare versions and build event diffs. Compact subset of IssueDetail
// — only the fields we read for the version-CAS path.
//
// R2.3 added LockedBy / LockedAt to the projection so the lock-aware
// predicate can run inside the same SERIALIZABLE transaction without a
// second round-trip.
type issueRow struct {
	ID          string
	ProjectID   uuid.UUID
	Version     int
	Title       string
	Description string
	Type        string
	Priority    int
	Status      string
	Pinned      bool
	Assignee    *string
	LockedBy    *string
	// LockedByUserID is the OWNER of the lock — the user whose api-key
	// performed the claim (sk-424). It is the basis of every "may the caller
	// dispose of this lock" decision; LockedBy (the MCP session) is the second
	// half of the pair and only separates sibling agent instances of that same
	// user. nil while LockedBy is set means "owner unknown" — see lockGuard.
	LockedByUserID     *uuid.UUID
	LockedAt           *time.Time
	StartedAt          *time.Time
	ClosedAt           *time.Time
	AcceptedAt         *time.Time
	Design             string
	AcceptanceCriteria string
	Notes              string
	EstimatedMinutes   *int
	DueAt              *time.Time
	DeferUntil         *time.Time
	ExternalRef        *string
	SourceContext      *string
	// CloseOutcome — the close outcome (sk-489). Read on every in-tx
	// fetch, because three decisions of the close writers depend on it:
	// the default of a repeated close (INV-10 — the outcome does not change), the fact
	// of an outcome change (the issue.outcome_changed event and the
	// previousCloseOutcome key in the payload) and branch (2) of the review mark
	// reset (leaving "done"). nil <=> the issue is not closed (INV-1).
	CloseOutcome *string
	// CreatedBy is the author of the issue (the requester). It is read on every
	// in-tx fetch because the fan-out's ROLE path needs it: "Issue accepted
	// by the assignee" and "Status of a delegated issue" push the creator, and
	// the delegating-creator audience additionally has to know whether that
	// person is also the assignee.
	CreatedBy uuid.UUID
}

func (s *Store) fetchIssueInTx(ctx context.Context, tx pgx.Tx, id string) (*issueRow, error) {
	var r issueRow
	err := tx.QueryRow(ctx, `
SELECT id, project_id, version, title, description, type, priority, status,
       pinned, assignee, locked_by, locked_by_user_id, locked_at, started_at, closed_at, accepted_at,
       design, acceptance_criteria, notes,
       estimated_minutes, due_at, defer_until, external_ref, source_context,
       created_by, close_outcome
  FROM issues WHERE id = $1`, id).Scan(
		&r.ID, &r.ProjectID, &r.Version, &r.Title, &r.Description,
		&r.Type, &r.Priority, &r.Status, &r.Pinned, &r.Assignee,
		&r.LockedBy, &r.LockedByUserID, &r.LockedAt, &r.StartedAt, &r.ClosedAt, &r.AcceptedAt,
		&r.Design, &r.AcceptanceCriteria, &r.Notes,
		&r.EstimatedMinutes, &r.DueAt, &r.DeferUntil, &r.ExternalRef, &r.SourceContext,
		&r.CreatedBy, &r.CloseOutcome,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("loading issue in tx: %w", err)
	}
	return &r, nil
}

// roleContextOf resolves the fan-out's ROLE-path context from an issue row:
// who filed the task (the creator) and who holds it now.
//
// The role path is the half of the recipient set the subscription pools cannot
// answer: "who the event is personally about". It is deliberately resolved from the
// issue row rather than guessed from the actor — a person may be the creator
// without watching anything, and the delegating-creator push audience is
// defined by exactly this pair ("the creator is neither the assignee nor
// the author of this action").
//
// The assignee is stored as a free-form handle, so it costs one resolution
// query per fan-out — one, not one per recipient.
func roleContextOf(ctx context.Context, tx pgx.Tx, row *issueRow) (creator, assignee uuid.UUID, err error) {
	if row == nil {
		return uuid.Nil, uuid.Nil, nil
	}
	creator = row.CreatedBy
	if row.Assignee != nil && *row.Assignee != "" {
		assignee, err = notifications.ResolveAssigneeUserID(ctx, tx, *row.Assignee, row.ProjectID)
		if err != nil {
			return uuid.Nil, uuid.Nil, fmt.Errorf("resolving assignee for fan-out role context: %w", err)
		}
	}
	return creator, assignee, nil
}

// standingRelationExists reports whether the issue `id` participates in a
// standing parent/child relationship — i.e. it has at least one standing
// PARENT or at least one standing CHILD. It checks BOTH representations of
// the hierarchy so the nested-standing ban (sk-118 R10) cannot be bypassed:
//
//   - the canonical `parent-child` dependency edge (from_id = child,
//     to_id = parent), which CreateIssue / skein_reparent maintain; and
//   - the legacy, frozen `issues.parent_id` column (still populated on
//     pre-sk-35 rows).
//
// A row counts only when the OTHER end is currently status='standing'. The
// four arms below cover {standing parent, standing child} × {edge, column}.
// Used by UpdateStatus on the promote-into-standing path; the create /
// reparent / dep-add paths have their own equivalent guards.
func standingRelationExists(ctx context.Context, tx pgx.Tx, id string) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, `
SELECT EXISTS (
    -- standing PARENT via parent-child dep edge (id is the child / from side)
    SELECT 1 FROM dependencies d
      JOIN issues p ON p.id = d.to_id
     WHERE d.from_id = $1 AND d.type = 'parent-child' AND p.status = 'standing'
    UNION ALL
    -- standing CHILD via parent-child dep edge (id is the parent / to side)
    SELECT 1 FROM dependencies d
      JOIN issues c ON c.id = d.from_id
     WHERE d.to_id = $1 AND d.type = 'parent-child' AND c.status = 'standing'
    UNION ALL
    -- standing PARENT via the legacy issues.parent_id column
    SELECT 1 FROM issues self
      JOIN issues p ON p.id = self.parent_id
     WHERE self.id = $1 AND p.status = 'standing'
    UNION ALL
    -- standing CHILD via the legacy issues.parent_id column
    SELECT 1 FROM issues c
     WHERE c.parent_id = $1 AND c.status = 'standing'
)`, id).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("checking nested-standing relation for %s: %w", id, err)
	}
	return exists, nil
}

// standingOpenChildIDs returns the ids of every NON-CLOSED child of `id`,
// covering BOTH representations of the hierarchy so the retire-block (sk-118
// F2e) cannot be bypassed:
//
//   - the canonical `parent-child` dependency edge (from_id = child,
//     to_id = parent = id); and
//   - the legacy, frozen issues.parent_id column (still populated on
//     pre-sk-35 rows).
//
// "Non-closed" is the centralized status.ActiveWork predicate (status <>
// 'closed'), so open / in_progress / deferred children all count — a deferred
// child is unresolved work and must block the retire (§11.4). A child reachable
// via BOTH representations is de-duplicated by the UNION. The result is empty
// when the standing task has no children OR all its children are closed (an
// empty retire and an all-children-closed retire both succeed).
//
// Used by Close() on the standing retire path; ordered for a stable error
// payload.
func standingOpenChildIDs(ctx context.Context, tx pgx.Tx, id string) ([]string, error) {
	rows, err := tx.Query(ctx, `
SELECT child_id FROM (
    -- non-closed child via the canonical parent-child dep edge
    SELECT c.id AS child_id
      FROM dependencies d
      JOIN issues c ON c.id = d.from_id
     WHERE d.to_id = $1 AND d.type = 'parent-child' AND `+status.ActiveWork("c")+`
    UNION
    -- non-closed child via the legacy issues.parent_id column
    SELECT c.id AS child_id
      FROM issues c
     WHERE c.parent_id = $1 AND `+status.ActiveWork("c")+`
) AS children
ORDER BY child_id`, id)
	if err != nil {
		return nil, fmt.Errorf("listing non-closed children of standing %s: %w", id, err)
	}
	defer rows.Close()

	var childIDs []string
	for rows.Next() {
		var childID string
		if scanErr := rows.Scan(&childID); scanErr != nil {
			return nil, fmt.Errorf("scanning non-closed child of %s: %w", id, scanErr)
		}
		childIDs = append(childIDs, childID)
	}
	if rows.Err() != nil {
		return nil, fmt.Errorf("iterating non-closed children of %s: %w", id, rows.Err())
	}
	return childIDs, nil
}

// lockOwnedBy reports whether the lock on row is owned by userID — the
// identity half of the holder pair (sk-424).
//
// A lock whose owner is not recorded (LockedByUserID nil, or the zero UUID)
// is owned by NOBODY and this returns false for every caller. That is the
// fail-closed direction and it is deliberate: the alternative — treating an
// unknown owner as "whoever presents the session string" — is exactly the
// defect sk-424 closes. Such a row cannot occur through the product (claimInTx
// writes both halves in one statement, and migration 000049 seeded the locks
// that predate it from the issue.claimed journal); if one appears anyway, an
// admin releases it with force, which is the documented escape hatch.
func lockOwnedBy(row *issueRow, userID uuid.UUID) bool {
	if row.LockedByUserID == nil || *row.LockedByUserID == uuid.Nil {
		return false
	}
	return userID != uuid.Nil && *row.LockedByUserID == userID
}

// lockGuard checks the in-tx issueRow against the caller's lock predicate.
// Returns nil when the operation may proceed:
//   - force=true → always pass (admin-only at the tool layer)
//   - locked_by IS NULL → pass (no one holds the lock)
//   - the caller is the holder → pass
//   - else → ErrLockedByOther{LockedBy: ...}
//
// # What "the caller is the holder" means (sk-424)
//
// BOTH halves must match: the lock's OWNER (locked_by_user_id) must be the
// calling user, and the lock's SESSION (locked_by) must be the caller's
// Mcp-Session-Id. The owner is the authority; the session only separates
// sibling agent instances of one user.
//
// The user half is the fix. Before sk-424 this function compared the session
// string alone, and that string is not a credential: the credential in this
// product is the api-key, which resolves to a user. The session id identifies
// an agent INSTANCE, rotates on every reconnect, is handed to every reader of
// the issue (`lockedBy` on the card, `actorSessionId` in the event feed) and
// belongs to no one — `agents` is keyed on the PAIR (user_id, session_id),
// with no uniqueness on the session alone, and its row is written before any
// check as a best-effort convenience. So presenting somebody else's session
// string satisfied a session-only comparison, and releasing, closing or
// editing another user's claimed issue followed from that.
//
// The session half is KEPT, not replaced, so the documented per-agent
// granularity survives: two sibling agents of the same user still do not step
// on each other's claims. Widening the basis to the user alone would have
// handed every sibling instance authority over the others' locks.
//
// SessionID="" with a non-null locked_by is treated as "different session"
// and yields ErrLockedByOther — REST callers carry no session id, so any
// locked issue requires force=true on the legacy path, exactly as before.
// R2.2 callers that never set locks see the IS NULL branch and pass through
// unchanged.
func lockGuard(row *issueRow, sessionID string, userID uuid.UUID, force bool) error {
	if force {
		return nil
	}
	if row.LockedBy == nil || *row.LockedBy == "" {
		return nil
	}
	if sessionID != "" && *row.LockedBy == sessionID && lockOwnedBy(row, userID) {
		return nil
	}
	return &ErrLockedByOther{LockedBy: *row.LockedBy}
}

// ----------------- R3.4 — Activity feed -----------------

// DefaultActivityEventTypes is the closed set of lifecycle types that the
// project / me / issue activity feed surfaces by default. Audit-only
// field-changed rows (issue.field_changed.*) are intentionally excluded so the
// feed reads as a third-person timeline of meaningful events, not a raw event
// stream.
//
// ⚠ THIS LIST IS THE WHOLE FEED, and nothing narrows it further downstream.
// The frontend sends no type filter at all (skein-frontend
// src/api/activity.ts#listIssueActivity passes only limit and cursor), so an
// event type missing here is invisible in the issue history no matter how
// correctly it was written. A previous version of this comment claimed the
// list «mirrors notifications.IsNotifiableEventType»; that function no longer
// exists anywhere in the tree, and the claim had quietly become a description
// of nothing — the list is maintained by hand and must be extended by hand.
//
// The list is exported so handlers + tests can reuse it; passing nil
// to ListProjectActivity / ListUserActivity also defaults to this set.
var DefaultActivityEventTypes = []string{
	"issue.created",
	"issue.accepted",
	"issue.updated",
	"issue.status_changed",
	// sk-489 / US-3: "the issue history has an outcome-change event". The event is
	// written by both close writers and by the outcome patch; without this
	// entry the feed would silently omit it and the acceptance criterion would
	// fail with every other half of the feature in place.
	"issue.outcome_changed",
	"issue.archived",
	"issue.closed",
	"issue.claimed",
	"issue.unclaimed",
	// sk-43 — re-parent is a structural lifecycle change (issue-scoped,
	// entity_type='issue'); it belongs in the timeline alongside
	// status_changed / closed so the activity feed shows the move.
	"issue.reparented",
	"comment.created",
	"dependency.created",
	"dependency.deleted",
}

// activityListSQL is the shared SELECT used by ListProjectActivity and
// ListUserActivity. The {{WHERE}} placeholder is filled with caller-
// supplied predicates (project visibility / cursor / type filter).
//
// LEFT JOINs follow the entity_type discriminator on events:
//   - 'issue' rows pull title from issues
//   - 'comment' rows go through comments → parent issue title
//   - 'project' rows pull title from projects.name
//   - 'dep' / 'component' rows have no canonical "title" — left blank
//
// projects is always joined so we can hand the client an id_prefix for
// route-building (issue URLs require both project uuid and prefix).
//
// Ordering is keyset DESC on the bigserial id — events.idx_events_project
// (project_id, id) covers the common case (per-project feed); the
// /me/activity cross-project query falls back to a sorted scan over the
// same index by leveraging the project_id IN (...) shape.
var activityListSQL = `
SELECT e.id, e.occurred_at, e.type, e.actor, e.actor_user_id, e.actor_type,
       ` + person.StatusInProjectExpr("uact", "e.project_id") + ` AS actor_status,
       e.entity_type, e.entity_id, e.project_id, e.field, e.payload,
       e.old_data, e.new_data,
       i.title       AS issue_title,
       c.issue_id    AS comment_issue_id,
       ci.title      AS comment_issue_title,
       prj.name      AS project_name,
       prj.id_prefix AS project_id_prefix,
       cprj.id_prefix AS comment_project_prefix,
       psp.short_id   AS space_short
  FROM events e
  LEFT JOIN issues i  ON e.entity_type = 'issue'   AND i.id = e.entity_id
  -- The cast to uuid only fires when entity_type='comment' because the
  -- CASE returns NULL for non-comment rows and = NULL never matches.
  -- Without the CASE, Postgres pre-evaluates entity_id::uuid for every
  -- row and trips on issue ids like "demo-42" before the type guard
  -- can short-circuit.
  LEFT JOIN comments c ON c.id = CASE WHEN e.entity_type = 'comment'
                                       THEN e.entity_id::uuid
                                       ELSE NULL END
  LEFT JOIN issues ci ON ci.id = c.issue_id
  LEFT JOIN projects prj  ON prj.id  = e.project_id
  LEFT JOIN projects cprj ON cprj.id = ci.project_id
  LEFT JOIN users uact    ON uact.id = e.actor_user_id
  LEFT JOIN spaces psp    ON psp.id = prj.space_id
 WHERE %s
 ORDER BY e.id DESC
 LIMIT %s
`

// decodeJSONValue unmarshals a jsonb column (events.old_data /
// new_data) into a free-form Go value for the ActivityEntry wire shape.
// Returns nil for a SQL NULL column (nil/empty bytes), an explicit JSON
// null, or unparseable bytes — all three render as "no value" (∅) on the
// frontend, and the omitempty tag drops the field from the response.
func decodeJSONValue(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		// A malformed audit value must not break the whole feed — drop
		// just this field and let the row render with its summary.
		return nil
	}
	return v
}

// decodeActivityChanges lifts the per-field diff out of an issue.updated
// event's payload (events.payload.changes[]). Returns nil for any other
// event type (whose payload has no "changes" key), an empty payload, or
// unparseable bytes — the omitempty tag then drops the field. The shape
// matches eventbus.WriteHybridUpdateEvents: {"changes":[{field, oldValue,
// newValue}, ...]}.
func decodeActivityChanges(payload []byte) []ActivityChange {
	if len(payload) == 0 {
		return nil
	}
	var p struct {
		Changes []ActivityChange `json:"changes"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil
	}
	return p.Changes
}

// scanActivityRows reads activityListSQL into ActivityEntry slice.
// fetchedLimit is the caller's "limit + 1" sentinel value used to
// detect whether another page exists; the returned items slice is
// trimmed to limit and nextCursor is non-nil iff there is more.
func scanActivityRows(rows pgx.Rows, limit int) ([]ActivityEntry, *int64, error) {
	out := make([]ActivityEntry, 0, limit)
	for rows.Next() {
		var (
			id                                int64
			occurredAt                        time.Time
			eventType, actorName              string
			actorUserID                       *uuid.UUID
			actorType                         string
			entityType, entityID              string
			projectID                         *uuid.UUID
			field                             *string
			payload                           []byte
			oldData, newData                  []byte
			issueTitle                        *string
			commentIssueID, commentIssueTitle *string
			projectName, projectIDPrefix      *string
			commentProjectPrefix              *string
			actorStatus                       *person.Status
			spaceShort                        *string
		)
		if err := rows.Scan(
			&id, &occurredAt, &eventType, &actorName, &actorUserID, &actorType, &actorStatus,
			&entityType, &entityID, &projectID, &field, &payload,
			&oldData, &newData,
			&issueTitle,
			&commentIssueID, &commentIssueTitle,
			&projectName, &projectIDPrefix,
			&commentProjectPrefix, &spaceShort,
		); err != nil {
			return nil, nil, fmt.Errorf("scanning activity row: %w", err)
		}
		var entry ActivityEntry
		entry.ID = id
		entry.OccurredAt = occurredAt.UTC()
		entry.Type = eventType
		entry.Actor = ActivityActor{
			UserID:   actorUserID,
			Username: actorName,
			Type:     actorType,
		}
		if actorStatus != nil {
			entry.Actor.Status = *actorStatus
		}
		if spaceShort != nil {
			entry.SpaceID = *spaceShort
		}
		entry.Field = field
		// sk-73 — surface the event's own audit data so the Activity UI
		// renders meaningful change detail instead of guessing from the
		// issue's current state. old_data / new_data carry the scalar
		// diff (status strings, reparent parent ids, dep edge descriptor);
		// payload.changes carries the per-field diff for issue.updated.
		entry.OldValue = decodeJSONValue(oldData)
		entry.NewValue = decodeJSONValue(newData)
		entry.Changes = decodeActivityChanges(payload)
		// project_id is NOT NULL for every activity row (the WHERE
		// clauses in ListProjectActivity / ListUserActivity /
		// ListIssueActivity all require it), so projectID is non-nil
		// here. Keep it as a pointer so a Hard-redacted row can drop it
		// from the wire entirely; the visibility comparison downstream
		// dereferences it before any redaction runs.
		entry.ProjectID = projectID

		// Build the entity reference. Comment events expose the parent
		// issue id+title (mirrors notifications inbox UX) so a click in
		// the activity row lands on the issue, not on a transient
		// comment id.
		switch entityType {
		case "issue":
			entry.Entity = ActivityEntity{
				Type:      "issue",
				ID:        entityID,
				ProjectID: entry.ProjectID,
			}
			if issueTitle != nil {
				entry.Entity.Title = *issueTitle
			}
			if projectIDPrefix != nil {
				entry.Entity.ProjectIDPrefix = *projectIDPrefix
			}
		case "comment":
			entry.Entity = ActivityEntity{
				Type:      "comment",
				ProjectID: entry.ProjectID,
			}
			if commentIssueID != nil {
				entry.Entity.ID = *commentIssueID
			} else {
				entry.Entity.ID = entityID
			}
			if commentIssueTitle != nil {
				entry.Entity.Title = *commentIssueTitle
			}
			if commentProjectPrefix != nil {
				entry.Entity.ProjectIDPrefix = *commentProjectPrefix
			} else if projectIDPrefix != nil {
				entry.Entity.ProjectIDPrefix = *projectIDPrefix
			}
		case "project":
			entry.Entity = ActivityEntity{
				Type:      "project",
				ID:        entityID,
				ProjectID: entry.ProjectID,
			}
			if projectName != nil {
				entry.Entity.Title = *projectName
			}
			if projectIDPrefix != nil {
				entry.Entity.ProjectIDPrefix = *projectIDPrefix
			}
		default:
			entry.Entity = ActivityEntity{
				Type:      entityType,
				ID:        entityID,
				ProjectID: entry.ProjectID,
			}
			if projectIDPrefix != nil {
				entry.Entity.ProjectIDPrefix = *projectIDPrefix
			}
		}

		entityRef := entry.Entity.ID
		if entry.Entity.Title != "" && entry.Entity.ID != "" {
			entityRef = entry.Entity.ID + " (" + entry.Entity.Title + ")"
		}
		entry.Summary = notifications.FormatSummary(entry.Type, actorName, entityRef, payload)

		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	var nextCursor *int64
	if len(out) > limit {
		// We over-fetched by one to detect more pages. Drop the spare
		// and emit a cursor pointing AT the last visible row's id —
		// keyset paging is `WHERE id < $cursor`, so the next page
		// resumes strictly older than this row.
		out = out[:limit]
		last := out[len(out)-1].ID
		nextCursor = &last
	}
	return out, nextCursor, nil
}

// clampActivityLimit enforces [1, 100] with a default of 50. Mirrors
// notifications.ListInbox semantics so paginated UIs that re-use both
// endpoints don't have to special-case differing bounds.
func clampActivityLimit(limit int) int {
	if limit < 1 {
		return 50
	}
	if limit > 100 {
		return 100
	}
	return limit
}

// ListProjectActivity returns the project's activity feed, newest first,
// with keyset pagination on events.id. Filtered to the notifiable
// lifecycle set (DefaultActivityEventTypes) when typeFilter is empty.
//
// Visibility is enforced at the handler layer (EnsureProjectVisible);
// the store assumes the caller has already authorised the projectID.
func (s *Store) ListProjectActivity(
	ctx context.Context,
	projectID uuid.UUID,
	typeFilter []string,
	cursor *int64,
	limit int,
) ([]ActivityEntry, *int64, error) {
	limit = clampActivityLimit(limit)
	types := typeFilter
	if len(types) == 0 {
		types = DefaultActivityEventTypes
	}

	args := []any{projectID, types}
	clauses := []string{
		"e.project_id = $1",
		"e.type = ANY($2)",
	}
	if cursor != nil {
		args = append(args, *cursor)
		clauses = append(clauses, fmt.Sprintf("e.id < $%d", len(args)))
	}
	args = append(args, limit+1)
	limitParam := fmt.Sprintf("$%d", len(args))

	sql := fmt.Sprintf(activityListSQL, strings.Join(clauses, " AND "), limitParam)

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("listing project activity: %w", err)
	}
	defer rows.Close()

	return scanActivityRows(rows, limit)
}

// ListUserActivity returns activity across projects, newest first.
//
// Scope and redaction are TWO INDEPENDENT axes (do not collapse them):
//
//   - reach selects the SQL row set. reach == nil (the /me/activity
//     contract) CUTS rows to the caller's visible set: admin
//     (isAdmin=true) sees every project that has activity; a non-admin is
//     restricted to `visible`, and an empty set short-circuits to (empty,
//     nil, nil) — a no-op SQL round trip avoided, matching the "0 rows, no
//     cursor" semantics handlers expect. A non-nil reach (the HOT ZONE
//     /dashboard/feed contract, sk-674.18 T030) returns events of the
//     projects of the caller's reach — the visible ones and those of his
//     own non-personal spaces (INV-28) — and of no other project.
//
//   - level selects how invisible rows are redacted. For a non-admin
//     caller, every returned row whose project is not in `visible` is
//     passed through redactActivityEntry at the given level:
//     redact.Soft strips the human-readable Title/Summary/diff but keeps
//     the identity (id, project prefix, projectId); redact.Hard strips
//     those identifiers too. redact.None leaves rows untouched (used for
//     the scoped /me/activity feed, where no invisible row can appear).
//     Admins are never redacted.
//
// The two axes are passed explicitly by every caller — there is no
// zero-value default. redactActivityEntry itself is fail-closed: any
// level other than Soft strips the most.
func (s *Store) ListUserActivity(
	ctx context.Context,
	userID uuid.UUID,
	visible map[uuid.UUID]struct{},
	isAdmin bool,
	typeFilter []string,
	cursor *int64,
	limit int,
	// reach — the caller's HOT ZONE reach (sk-674.18 T030): rows of these
	// projects, those outside visible redacted at level. nil — the visible
	// projects only (the /me/activity contract).
	reach map[uuid.UUID]struct{},
	level redact.Level,
) ([]ActivityEntry, *int64, error) {
	_ = userID // currently used only for logging upstream; kept on the
	// signature to mirror RecentEventsForUser and to leave room for
	// future "actor or assignee or watcher" personalisation without
	// another surface change.

	limit = clampActivityLimit(limit)
	types := typeFilter
	if len(types) == 0 {
		types = DefaultActivityEventTypes
	}

	args := []any{types}
	clauses := []string{
		"e.project_id IS NOT NULL",
		"e.type = ANY($1)",
	}

	if !isAdmin {
		set := visible
		if reach != nil {
			set = reach
		}
		if len(set) == 0 {
			return []ActivityEntry{}, nil, nil
		}
		ids := make([]uuid.UUID, 0, len(set))
		for pid := range set {
			ids = append(ids, pid)
		}
		args = append(args, ids)
		clauses = append(clauses, fmt.Sprintf("e.project_id = ANY($%d)", len(args)))
	}

	if cursor != nil {
		args = append(args, *cursor)
		clauses = append(clauses, fmt.Sprintf("e.id < $%d", len(args)))
	}
	args = append(args, limit+1)
	limitParam := fmt.Sprintf("$%d", len(args))

	sql := fmt.Sprintf(activityListSQL, strings.Join(clauses, " AND "), limitParam)

	queryArgs := args
	if !isAdmin {
		// The project array of the reach or the visible set: planned with
		// it on every call, never as a cached generic plan blind to its
		// size (the same trap as dashboard.reachPlanned; sk-674.18
		// EXPLAIN gate, dashboard-feed).
		queryArgs = append([]any{pgx.QueryExecModeDescribeExec}, args...)
	}
	rows, err := s.pool.Query(ctx, sql, queryArgs...)
	if err != nil {
		return nil, nil, fmt.Errorf("listing user activity: %w", err)
	}
	defer rows.Close()

	items, next, err := scanActivityRows(rows, limit)
	if err != nil {
		return nil, nil, err
	}
	// Redaction axis: a non-admin caller viewing a global (war-room) feed
	// has every invisible-project row stripped at `level`. redact.None
	// (the scoped /me/activity path) skips the loop entirely — there are
	// no invisible rows to strip anyway. Admins are never redacted.
	if level != redact.None && !isAdmin {
		for i := range items {
			pid := items[i].ProjectID
			visibleRow := pid != nil
			if visibleRow {
				_, visibleRow = visible[*pid]
			}
			if !visibleRow {
				// Decide visibility on the non-nil project id FIRST,
				// then redact (which may nil the pointer) — never the
				// other way round.
				redactActivityEntry(&items[i], level)
			}
		}
	}
	return items, next, nil
}

// redactActivityEntry strips the content of an activity row whose project
// the caller cannot see (war-room cross-project view).
//
// The base strip (content: title, summary, per-field diff) runs for every
// level. Then a fail-closed switch decides the identity strip: only
// redact.Soft is spared — it keeps the id, project prefix and both
// projectId fields. redact.Hard, and ANY other/unknown level (fail
// closed: a forgotten or future level must over-redact, never leak),
// additionally drops every stable project/task identifier:
//
//   - Entity.ID — for issue rows the issue id; for comment rows the
//     PARENT issue id; for dep rows the composite "fromID::toID::depType"
//     (two ids) — a single blank covers all three.
//   - Entity.ProjectIDPrefix, Entity.ProjectID, and the top-level
//     ProjectID (both projectId fields go absent from the wire).
//   - project.* rows carry the project NAME in Title, already blanked by
//     the base content strip above.
func redactActivityEntry(e *ActivityEntry, level redact.Level) {
	// Base content strip — every redaction level.
	e.Entity.Title = ""
	e.Summary = ""
	e.Field = nil
	e.OldValue = nil
	e.NewValue = nil
	e.Changes = nil
	e.Redacted = true

	switch level {
	case redact.Soft:
		// Soft keeps identity: id, project prefix, projectId survive.
		return
	default:
		// Hard + fail-closed: strip every stable project/task identifier.
		e.Entity.ID = ""
		e.Entity.ProjectIDPrefix = ""
		e.Entity.ProjectID = nil
		e.ProjectID = nil
		e.SpaceID = ""
	}
}

// ListIssueActivity returns the per-issue activity feed, newest first,
// with keyset pagination on events.id. Filtered to the same notifiable
// lifecycle set as the project feed (DefaultActivityEventTypes) when
// typeFilter is empty.
//
// Scope: an event belongs to this issue when ANY of —
//   - entity_type='issue' AND entity_id = issueID
//   - entity_type='comment' AND comment.issue_id = issueID (the LEFT JOIN
//     in activityListSQL already exposes c.issue_id; we just predicate
//     on it)
//   - entity_type='dep' AND the encoded entity_id (fromID::toID::depType
//     per dependencies.dependencyEntityID) matches issueID on either
//     side. Issue IDs cannot contain ':' (validate.go customSuffixRegex),
//     so the '::' separator is unambiguous.
//
// project_id is added as a defence-in-depth predicate — events.project_id
// is denormalised on the row and the existing
// idx_events_project (project_id, id) covers the keyset scan, so the
// per-issue query plan reuses it. Visibility is enforced at the handler
// layer; the store assumes the caller has already authorised projectID.
func (s *Store) ListIssueActivity(
	ctx context.Context,
	projectID uuid.UUID,
	issueID string,
	typeFilter []string,
	cursor *int64,
	limit int,
) ([]ActivityEntry, *int64, error) {
	limit = clampActivityLimit(limit)
	types := typeFilter
	if len(types) == 0 {
		types = DefaultActivityEventTypes
	}

	// Patterns for dep entity_id matching:
	//   "<issueID>::%"      → fromID side
	//   "%::<issueID>::%"   → toID side (depType is the trailing segment;
	//                         issueID never appears in depType because
	//                         depType is one of blocks/relates_to/etc.)
	depFromPattern := issueID + "::%"
	depToPattern := "%::" + issueID + "::%"

	args := []any{projectID, types, issueID, depFromPattern, depToPattern}
	clauses := []string{
		"e.project_id = $1",
		"e.type = ANY($2)",
		// Three-way OR over entity scopes: direct issue events, comment
		// events whose parent issue is ours (c.issue_id from the JOIN),
		// or dep events whose encoded entity_id mentions issueID.
		"(" +
			"(e.entity_type = 'issue' AND e.entity_id = $3) OR " +
			"(e.entity_type = 'comment' AND c.issue_id = $3) OR " +
			"(e.entity_type = 'dep' AND (e.entity_id LIKE $4 OR e.entity_id LIKE $5))" +
			")",
	}
	if cursor != nil {
		args = append(args, *cursor)
		clauses = append(clauses, fmt.Sprintf("e.id < $%d", len(args)))
	}
	args = append(args, limit+1)
	limitParam := fmt.Sprintf("$%d", len(args))

	sql := fmt.Sprintf(activityListSQL, strings.Join(clauses, " AND "), limitParam)

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("listing issue activity: %w", err)
	}
	defer rows.Close()

	return scanActivityRows(rows, limit)
}
