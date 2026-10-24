package dependencies

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/authz"
	"skein-api/internal/idorder"
	"skein-api/internal/notifications"
	"skein-api/internal/projects"
	"skein-api/internal/signals"
	"skein-api/internal/status"
)

// pgUniqueViolation is the SQLSTATE class for a unique-constraint hit,
// duplicated locally so this package doesn't pull projects/issues just
// for one constant.
const pgUniqueViolation = "23505"

// pgForeignKeyViolation surfaces when from_id or to_id reference an
// issue that doesn't exist (FK ON DELETE CASCADE doesn't help on insert).
const pgForeignKeyViolation = "23503"

// pgSerializationFailure is SQLSTATE 40001 — a SERIALIZABLE transaction
// lost to a concurrent writer. The INSERT made no durable change; the
// caller may retry verbatim. Mapped to ErrSerializationRetry.
const pgSerializationFailure = "40001"

// depToIDFKConstraint is Postgres's auto-generated name for the to_id
// foreign key on the dependencies table (column-level REFERENCES →
// "<table>_<column>_fkey"). We match on it to tell a missing TARGET
// (retryable — the row may be mid-commit in a concurrent tx) apart from a
// missing FROM (caller used a wrong path).
const depToIDFKConstraint = "dependencies_to_id_fkey"

// Store is the dependencies-domain database gateway. Goroutine-safe — the
// pool itself owns connections.
//
// embedder is optional: when nil (or not Available) CheckDuplicates falls
// back to its trigram-only path. Injection via the QueryEmbedder
// interface keeps this package free of any import on `search`, so the
// dep graph stays acyclic.
type Store struct {
	pool     *pgxpool.Pool
	embedder QueryEmbedder
}

// NewStore returns a Store backed by the given pool with no embedder
// (trigram-only duplicate detection). Use NewStoreWithEmbedder when an
// embedder is available — main.go does the wiring.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// NewStoreWithEmbedder returns a Store with the optional embedder wired
// for layer-2 vector duplicate detection. Pass nil to disable layer-2.
func NewStoreWithEmbedder(pool *pgxpool.Pool, embedder QueryEmbedder) *Store {
	return &Store{pool: pool, embedder: embedder}
}

// ----------------- Sentinel errors -----------------

// ErrNotFound is returned by Delete when no row matches the
// (from, to, type) tuple.
var ErrNotFound = errors.New("dependency not found")

// ErrDependencyExists is returned when an INSERT collides with the
// composite PK. Mapped to 409 Conflict by the handler so clients can
// distinguish "already there" from a validation error.
var ErrDependencyExists = errors.New("dependency already exists")

// ErrDependencyTargetNotVisible is returned when caller can see the
// FROM-side project but not the TO-side. Every surface answers it exactly
// as ErrTargetNotFound (INV-3): REST 422 `target_not_found`, MCP
// `issue_not_found`.
var ErrDependencyTargetNotVisible = errors.New("dependency target not visible to caller")

// ErrTargetNotFound — TO-side issue id doesn't exist at all.
var ErrTargetNotFound = errors.New("dependency target issue not found")

// ErrFromNotFound — FROM-side issue id doesn't exist (path was wrong).
var ErrFromNotFound = errors.New("dependency from-issue not found")

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation
}

func isFKViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgForeignKeyViolation
}

// classifyInsertError maps a raw error from the dependency INSERT into a
// stable typed error. Mapping (first match wins):
//
//   - 23505 unique_violation            → ErrDependencyExists (the edge
//     already exists; idempotent re-add).
//   - 40001 serialization_failure       → ErrSerializationRetry (lost a
//     SERIALIZABLE race; the write made no durable change — safe to retry).
//   - 23503 foreign_key_violation on the to_id FK → ErrDependencyTargetNotReady
//     (target row not visible in this snapshot — e.g. created in a
//     still-uncommitted concurrent tx; retry shortly).
//   - 23503 on any other FK (from_id)   → ErrFromNotFound (path was wrong).
//   - anything else                     → nil (caller wraps as internal_error).
//
// Returns nil when err is not one of the recognised classes so the caller
// can fall through to its generic %w wrap.
func classifyInsertError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return nil
	}
	switch pgErr.Code {
	case pgUniqueViolation:
		return ErrDependencyExists
	case pgSerializationFailure:
		return ErrSerializationRetry
	case pgForeignKeyViolation:
		if pgErr.ConstraintName == depToIDFKConstraint {
			return ErrDependencyTargetNotReady
		}
		return ErrFromNotFound
	default:
		return nil
	}
}

// ----------------- Internal lookups -----------------

// issueLookup is the row shape we read for visibility / cycle decisions
// on a dep insert. Just enough columns to make the right calls without
// hauling the full IssueDetail.
type issueLookup struct {
	ID        string
	ProjectID uuid.UUID
	Title     string
	Status    string
	Type      string
	Priority  int
}

// fetchIssue reads a minimal issue row inside the supplied tx. Used by
// Create to resolve from/to before deciding on visibility & cycles.
func fetchIssue(ctx context.Context, tx pgx.Tx, id string) (*issueLookup, error) {
	var r issueLookup
	err := tx.QueryRow(ctx, `
SELECT id, project_id, title, status, type, priority
  FROM issues
 WHERE id = $1`, id).Scan(&r.ID, &r.ProjectID, &r.Title, &r.Status, &r.Type, &r.Priority)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("loading issue %s: %w", id, err)
	}
	return &r, nil
}

// ----------------- Create -----------------

// CallerAccess is the slim access info Store.Create needs to decide
// whether the caller can see the TO-side project. We don't take
// projects.Store here; we just take the user identity and a callerRole
// label and use projects.EnsureProjectVisible directly inside the tx.
type CallerAccess struct {
	UserID uuid.UUID
	Role   string
}

// Create inserts one dependency row in a SERIALIZABLE transaction. Steps:
//
//  1. Validate type + (optional) gate.
//  2. Resolve FROM and TO issues (fail fast on missing).
//  3. Space check (INV-24, sk-674.18 T033): a TO-side issue of another
//     space is refused as ErrTargetNotFound — the nonexistent answer.
//     Then visibility check on the TO-side project — caller must already be
//     able to see the FROM project (handler enforced via path), so we
//     verify the OTHER side here. If the TO-side project is invisible,
//     return ErrDependencyTargetNotVisible (422).
//  4. Cycle detection for blocks/waits-for types.
//  5. INSERT and write event in the same tx (atomicity); a parent-child
//     edge also touches the child's row (touchChildRow, R26 item 3).
//
// The store NEVER mutates the target row's status/version on supersedes
// or duplicates. Auto-close is an explicit, separate user action.
func (s *Store) Create(
	ctx context.Context,
	fromID string,
	input CreateDepInput,
	caller CallerAccess,
	actor Actor,
) (*DependencyDTO, error) {
	var dto DependencyDTO
	err := signals.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		d, ierr := CreateInTx(ctx, tx, fromID, input, caller, actor)
		if ierr != nil {
			return ierr
		}
		dto = *d
		return nil
	})
	if err != nil {
		// A SERIALIZABLE abort (SQLSTATE 40001) can surface at ANY statement
		// of this transaction or at COMMIT, not only at the dependency
		// INSERT that classifyInsertError sees: the cycle walk, the event
		// insert (WriteDependencyEvent) and the commit all take it, and
		// each wraps it with %w. Whichever statement it came from, Postgres
		// rolled the whole transaction back — nothing was written, and the
		// call is safe to replay verbatim. So it is classified once, here,
		// for the transaction as a whole (sk-647.2; Delete and Reparent do
		// the same). Before this a 40001 from the event insert reached
		// callers as internal_error.
		if isSerializationFailure(err) {
			return nil, ErrSerializationRetry
		}
		return nil, err
	}
	return &dto, nil
}

// CreateInTx is the tx-bound variant of Create. Identical semantics, but
// runs inside the caller-supplied pgx.Tx instead of opening its own
// SERIALIZABLE transaction. Used by issues.CreateIssue when inline
// `dependencies[]` are supplied on skein_create — the new issue and all
// its edges must commit (or roll back) together.
//
// Callers MUST open a SERIALIZABLE transaction themselves; this helper
// does not enforce isolation level (cycle detection assumes a stable
// snapshot of the dependency graph).
func CreateInTx(
	ctx context.Context,
	tx pgx.Tx,
	fromID string,
	input CreateDepInput,
	caller CallerAccess,
	actor Actor,
) (*DependencyDTO, error) {
	depType, err := ValidateType(input.Type)
	if err != nil {
		return nil, err
	}
	gate, err := ValidateGate(depType, input.Gate)
	if err != nil {
		return nil, err
	}

	if fromID == input.ToID {
		return nil, newValidationError("dep_self_reference",
			"a dependency cannot point at itself")
	}

	metadata := map[string]any{}
	if gate != "" {
		metadata["gate"] = gate
	}
	metadataBytes, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("marshalling metadata: %w", err)
	}

	// Resolve both endpoints. fetchIssue returns ErrNotFound when the
	// id doesn't exist; we surface a more specific error per side.
	from, err := fetchIssue(ctx, tx, fromID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrFromNotFound
		}
		return nil, err
	}
	to, err := fetchIssue(ctx, tx, input.ToID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrTargetNotFound
		}
		return nil, err
	}

	// Cross-project visibility: caller must be able to see the
	// project on the OTHER side. Same-project case is short-circuited
	// (caller already had the FROM project visible to reach the
	// handler).
	if from.ProjectID != to.ProjectID {
		// INV-24 (sk-674.18 T033): a link never leaves the space of the
		// issue. Checked BEFORE visibility, so a target of another space
		// answers exactly as a nonexistent one — never «not visible».
		same, err := projects.SameSpace(ctx, tx, from.ProjectID, to.ProjectID)
		if err != nil {
			return nil, err
		}
		if !same {
			return nil, ErrTargetNotFound
		}
		if _, err := projects.EnsureProjectVisible(ctx, tx,
			to.ProjectID, caller.UserID); err != nil {
			if errors.Is(err, projects.ErrProjectNotVisible) {
				return nil, ErrDependencyTargetNotVisible
			}
			return nil, fmt.Errorf("checking target visibility: %w", err)
		}
	}

	// sk-118 (F2f): blocker-target standing ban. A `blocks`/`waits-for` edge
	// gates the FROM (blocked) issue's readiness while the TO (blocker) issue is
	// not closed. A standing TO-side never closes — it is a perpetual obligation —
	// so such an edge would gate FROM forever. Reject creating it at the source.
	// (The GatesReadiness predicate independently excludes standing so an
	// ALREADY-existing such edge — e.g. a blocker promoted to standing after the
	// edge was made — stops gating; this is the create-time defence in depth.)
	// Other edge types (related / discovered-from / supersedes / duplicates /
	// parent-child) may legitimately target a standing issue and are NOT banned.
	// `to` was already fetched above with its status.
	if (depType == DepBlocks || depType == DepWaitsFor) && to.Status == statusStanding {
		return nil, ErrBlockerTargetStanding
	}

	// Cycle detection — only for types that participate in a meaningful
	// directed graph. related / discovered-from / supersedes /
	// duplicates do not, by spec. parent-child does — sk-35 restored
	// the dedicated walk over parent-child edges so a child can't
	// become an ancestor of itself.
	switch depType {
	case DepBlocks, DepWaitsFor:
		if err := CheckBlocksCycle(ctx, tx, fromID, input.ToID); err != nil {
			return nil, err
		}
	case DepParentChild:
		// sk-118 (F2e): nested-standing ban. A `parent-child` edge has
		// from = child, to = parent; creating this edge would put the child
		// directly under the parent. Reject when BOTH ends are standing —
		// nesting a standing task under a standing task reintroduces the
		// §11.4 mutual-retire deadlock. (from/to were already fetched above
		// with their status.) Checked before the cycle / single-parent guards
		// so the structural invariant short-circuits.
		if from.Status == statusStanding && to.Status == statusStanding {
			return nil, ErrNestedStandingForbidden
		}
		if err := CheckParentChildCycle(ctx, tx, fromID, input.ToID); err != nil {
			return nil, err
		}
		// Single-parent invariant: a child may have at most ONE
		// parent-child edge. If from already points at a DIFFERENT
		// parent, reject with ErrParentAlreadySet — re-parenting must go
		// through Reparent (which atomically replaces the edge). Adding
		// the SAME edge again falls through to the INSERT and trips the
		// composite-PK unique violation → ErrDependencyExists, preserving
		// the prior idempotency contract. This guard is what stops the
		// "two parent edges, earliest keeps winning" bug at the source —
		// it covers dep_add, dep_add_many, and create-inline-deps, which
		// all funnel through this function.
		var existingParent string
		err := tx.QueryRow(ctx, `
SELECT to_id FROM dependencies
 WHERE from_id = $1 AND type = 'parent-child' AND to_id <> $2
 LIMIT 1`, fromID, input.ToID).Scan(&existingParent)
		if err == nil {
			return nil, ErrParentAlreadySet
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("checking existing parent for %s: %w", fromID, err)
		}
	}

	// INSERT — composite PK collision → ErrDependencyExists (409).
	// FK violation → target/from missing (we already pre-checked, but
	// concurrent issue delete could race; surface as 422 target_not_found).
	var dto DependencyDTO
	row := tx.QueryRow(ctx, `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, $3, $4::jsonb, 1, $5, now())
RETURNING from_id, to_id, type, metadata, version, created_by, created_at`,
		fromID, input.ToID, depType, metadataBytes, caller.UserID)

	var metaBytes []byte
	var createdAt time.Time
	if err := row.Scan(&dto.FromID, &dto.ToID, &dto.Type, &metaBytes,
		&dto.Version, &dto.CreatedBy, &createdAt); err != nil {
		// Classify transient / known DB failures into stable typed errors:
		// unique → already-exists, 40001 → serialization-retry, 23503 on
		// the to_id FK → target-not-ready (both retryable). Anything else
		// stays internal_error via the %w wrap.
		if classified := classifyInsertError(err); classified != nil {
			return nil, classified
		}
		return nil, fmt.Errorf("inserting dependency: %w", err)
	}
	dto.CreatedAt = createdAt
	dto.Metadata = map[string]any{}
	if len(metaBytes) > 0 {
		_ = json.Unmarshal(metaBytes, &dto.Metadata)
	}

	if depType == DepParentChild {
		if err := touchChildRow(ctx, tx, fromID); err != nil {
			return nil, err
		}
	}

	// Event payload mirrors the wire DTO so consumers replay state
	// directly from the timeline.
	payload := map[string]any{
		"fromId":    dto.FromID,
		"toId":      dto.ToID,
		"type":      dto.Type,
		"metadata":  dto.Metadata,
		"createdBy": dto.CreatedBy,
	}
	if _, werr := WriteDependencyEvent(ctx, tx, EventDependencyCreated,
		dto.FromID, dto.ToID, dto.Type, payload, from.ProjectID, actor); werr != nil {
		return nil, werr
	}
	return &dto, nil
}

// ----------------- Delete -----------------

// Delete removes one dependency row identified by the composite PK.
// Returns ErrNotFound when the row didn't exist (idempotency tradeoff:
// we surface 404 instead of silent success so the UI can flag a stale
// dep list).
//
// Permission check is the handler's job: it verifies the caller can see
// the FROM project, same level required for INSERT.
//
// sk-540.8: the last edge that carries a close outcome is refused with
// *ErrCloseOutcomeLinkRequired — see ensureNotLastOutcomeLink. The check
// and the DELETE share this SERIALIZABLE transaction, so a concurrent close
// or a concurrent removal of the sibling edge cannot slip between them: one
// of the two transactions is aborted and surfaces as ErrSerializationRetry.
func (s *Store) Delete(
	ctx context.Context,
	fromID, toID, depType string,
	actor Actor,
) error {
	if _, err := ValidateType(depType); err != nil {
		return err
	}

	err := signals.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		// Snapshot the row so the event payload carries the old state.
		var (
			metaBytes []byte
			version   int
			createdBy uuid.UUID
			createdAt time.Time
			projectID uuid.UUID
		)
		err := tx.QueryRow(ctx, `
SELECT d.metadata, d.version, d.created_by, d.created_at, i.project_id
  FROM dependencies d
  JOIN issues i ON i.id = d.from_id
 WHERE d.from_id = $1 AND d.to_id = $2 AND d.type = $3`,
			fromID, toID, depType).Scan(&metaBytes, &version, &createdBy, &createdAt, &projectID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("loading dependency for delete: %w", err)
		}

		if err := ensureNotLastOutcomeLink(ctx, tx, fromID, toID, depType); err != nil {
			return err
		}
		if deleteGuardSeam != nil {
			deleteGuardSeam(ctx, fromID, toID, depType)
		}

		ct, err := tx.Exec(ctx, `
DELETE FROM dependencies WHERE from_id = $1 AND to_id = $2 AND type = $3`,
			fromID, toID, depType)
		if err != nil {
			return fmt.Errorf("deleting dependency: %w", err)
		}
		if ct.RowsAffected() == 0 {
			return ErrNotFound
		}
		if depType == DepParentChild {
			if err := touchChildRow(ctx, tx, fromID); err != nil {
				return err
			}
		}

		metadata := map[string]any{}
		if len(metaBytes) > 0 {
			_ = json.Unmarshal(metaBytes, &metadata)
		}
		oldData := map[string]any{
			"fromId":    fromID,
			"toId":      toID,
			"type":      depType,
			"metadata":  metadata,
			"version":   version,
			"createdBy": createdBy,
			"createdAt": createdAt,
		}
		_, werr := WriteDependencyEvent(ctx, tx, EventDependencyDeleted,
			fromID, toID, depType, oldData, projectID, actor)
		return werr
	})
	if isSerializationFailure(err) {
		return ErrSerializationRetry
	}
	return err
}

// touchChildRow writes the child's issues row in the transaction that adds
// or removes its parent-child edge (sk-622.21, decision R26 item 3). The child's REST row and its phase-2 event row carry
// `parentIdCurrent`, derived from this edge, and the client substitutes an
// event row only when its rowSeq is greater than the shown one — so a
// committed parent change must move the child's rowSeq, and rowSeq moves
// only on a write of the issues row (trigger issues_stamp_row_seq, K16).
// Reparent already writes the row; dep_add / dep_remove of a parent-child
// edge (REST, MCP skein_dep_add / skein_dep_add_many / skein_dep_remove,
// create-inline deps — all through CreateInTx / Delete) now do too.
//
// What is written: updated_at only — the same touch the label paths use
// (issues/store.go#AddLabelNoLoad, #RemoveLabelNoLoad). `version` is NOT
// bumped: adding or removing an edge has never been a versioned edit of the
// issue (dep_add takes no expectedVersion), and bumping it would turn a
// concurrent PATCH of the child into a version_conflict for a change the
// patcher never saw. updated_at moves, as on Reparent and the label paths:
// the row a reader sees did change.
//
// Lock order: the edge write first (its FK takes KEY SHARE on both issues
// rows), then this UPDATE (FOR NO KEY UPDATE on the child, compatible with
// the KEY SHARE this transaction already holds) — the order Reparent uses.
// No retry is added: Create and Delete stay single-attempt SERIALIZABLE, and
// a 40001 or 40P01 here surfaces as ErrSerializationRetry (nothing written, safe to
// retry), like the INSERT / DELETE beside it.
func touchChildRow(ctx context.Context, tx pgx.Tx, childID string) error {
	if _, err := tx.Exec(ctx, `UPDATE issues SET updated_at = now() WHERE id = $1`, childID); err != nil {
		// 40P01 too: the touch takes a row lock on the child while the
		// edge write holds others, and a deadlock victim is rolled back
		// whole — as safe to replay as a 40001.
		var pgErr *pgconn.PgError
		if isSerializationFailure(err) || (errors.As(err, &pgErr) && pgErr.Code == pgDeadlockDetected) {
			return ErrSerializationRetry
		}
		return fmt.Errorf("touching child %s after a parent-child edge change: %w", childID, err)
	}
	return nil
}

// deleteGuardSeam sits between the outcome-link guard and the DELETE in
// Delete. In production it is nil and costs one pointer comparison.
//
// Why it exists: the guard is only as good as the SERIALIZABLE transaction
// around it, and the interleavings that matter — a close committing while
// the delete sits between its check and its write, two deletes of the last
// two edges each checking before either writes — do not open on natural
// timing. Without a seam the concurrency tests would pass whether or not
// the guard and the DELETE shared a transaction. Same reasoning and same
// discipline as issues/close_outcome.go#closeLinkRaceSeam: assigned only
// by tests (export_test.go), always restored, never called from
// production code.
var deleteGuardSeam func(ctx context.Context, fromID, toID, depType string)

// Close outcomes that are defined by an edge. Duplicated from the issues
// package (issues.CloseOutcomeDuplicate / CloseOutcomeSuperseded) because
// issues imports this package, not the other way round; the strings are
// the values of the issues.close_outcome CHECK and cannot drift silently —
// the integration tests close issues through the issues writer and then
// delete through this store.
const (
	closeOutcomeDuplicate  = "duplicate"
	closeOutcomeSuperseded = "superseded"
)

// ensureNotLastOutcomeLink refuses to remove the edge that is the only
// remaining justification of a close outcome (sk-540.8).
//
// Two outcomes are defined by an edge, and the edge points in opposite
// directions for them (issues/close_outcome.go#closeLinkEdge):
//
//   - "duplicate": the CLOSED issue is the FROM side of a `duplicates` edge;
//   - "superseded": the CLOSED issue is the TO side of a `supersedes` edge
//     (the replacement supersedes it).
//
// When the closed side of the edge being removed carries the matching
// outcome and no OTHER edge of the same type and direction remains, the
// removal is refused: it would leave a "duplicate of nothing" — an issue
// dropped out of its epic's progress with no trace of where the work went,
// the exact state sk-540.2 forbids at close time. Changing the outcome or
// reopening the issue first makes the removal legal again.
//
// Every other type, every non-closed issue and every other outcome passes
// through untouched. Both reads run inside the caller's SERIALIZABLE
// transaction: the issue row read conflicts with a concurrent close, the
// sibling-edge count conflicts with a concurrent delete of the sibling.
//
// The refusal holds for every caller, but what it SAYS depends on the
// caller's horizon (sk-423: the reader learns nothing about issues it cannot
// see). Delete only requires the FROM project to be visible, and for
// `supersedes` the closed issue is the TO side, which may sit in a project
// the caller cannot see. The same read therefore classifies the closed
// issue with authz.ProjectVisibleExpr — the rule the blockers list
// applies — and an invisible one yields a refusal without its id and
// outcome.
func ensureNotLastOutcomeLink(ctx context.Context, tx pgx.Tx, fromID, toID, depType string) error {
	var closedSide, outcome string
	switch depType {
	case DepDuplicates:
		closedSide, outcome = fromID, closeOutcomeDuplicate
	case DepSupersedes:
		closedSide, outcome = toID, closeOutcomeSuperseded
	default:
		return nil
	}

	// close_outcome alone answers "closed with this outcome": the schema
	// holds it non-NULL exactly when the issue is closed
	// (issues_close_outcome_consistent_chk, migration 000050).
	var (
		current *string
		visible bool
	)
	unrestricted, callerID, bindErr := authz.FromContext(ctx).Bind(ctx, tx)
	if bindErr != nil {
		return bindErr
	}
	err := tx.QueryRow(ctx,
		`SELECT i.close_outcome, `+authz.ProjectVisibleExpr("i", "$2", "$3")+`
   FROM issues i WHERE i.id = $1`,
		closedSide, unrestricted, callerID,
	).Scan(&current, &visible)
	if errors.Is(err, pgx.ErrNoRows) {
		// The edge row was found above, and both ends are FK-bound, so the
		// issue cannot be missing inside this snapshot. Nothing to guard.
		return nil
	}
	if err != nil {
		return fmt.Errorf("loading close outcome of %s: %w", closedSide, err)
	}
	if current == nil || *current != outcome {
		return nil
	}

	// Sibling edges of the same type and direction, the one being removed
	// excluded. For "duplicate" the closed issue is the FROM side, so the
	// siblings are its other outgoing edges; for "superseded" it is the TO
	// side, so the siblings are its other incoming edges.
	var others int
	if depType == DepDuplicates {
		err = tx.QueryRow(ctx, `
SELECT count(*) FROM dependencies
 WHERE from_id = $1 AND type = $2 AND to_id <> $3`,
			fromID, depType, toID).Scan(&others)
	} else {
		err = tx.QueryRow(ctx, `
SELECT count(*) FROM dependencies
 WHERE to_id = $1 AND type = $2 AND from_id <> $3`,
			toID, depType, fromID).Scan(&others)
	}
	if err != nil {
		return fmt.Errorf("counting %s edges of %s: %w", depType, closedSide, err)
	}
	if others > 0 {
		return nil
	}
	if !visible {
		return &ErrCloseOutcomeLinkRequired{Type: depType}
	}
	return &ErrCloseOutcomeLinkRequired{IssueID: closedSide, CloseOutcome: outcome, Type: depType}
}

// ----------------- Reparent -----------------

// ReparentResult is the post-state returned by Store.Reparent. NewEdge is
// the freshly-inserted parent-child edge (same shape Create returns).
// OldParentIDs is every parent the child had BEFORE the move (usually one,
// but ≥2 when the call self-healed a double-parented issue). The wiring
// pass uses these to build a rich response / re-fetch the IssueDetail
// without re-querying the dep graph.
type ReparentResult struct {
	NewEdge      DependencyDTO `json:"newEdge"`
	OldParentIDs []string      `json:"oldParentIds"`
}

// reparentRow is the minimal issue shape Reparent reads under its tx to
// run the optimistic-concurrency predicate. Just enough columns: project
// (same-project check) + version (optimistic check). Kept private to this
// file. Reparent is NOT lock-gated — it moves a graph edge, like
// skein_dep_add / skein_dep_remove, which are also lock-free; the lock
// guards field edits (update/close), not structural links.
type reparentRow struct {
	ProjectID uuid.UUID
	Version   int
	Status    string
}

// Reparent atomically moves issue `id` under a new parent. The whole
// operation runs in ONE SERIALIZABLE transaction so the child never
// transiently has zero or two parents on disk:
//
//  1. Load id (version, project_id). version mismatch →
//     ErrVersionConflict{CurrentVersion}. Not lock-gated (see reparentRow).
//  2. Validate newParentID: id != newParentID (ErrReparentSelf), the
//     parent exists and lies in the issue's space (ErrTargetNotFound —
//     INV-24, as for a nonexistent one), is visible to the caller
//     (ErrDependencyTargetNotVisible), and is in the SAME project
//     (ErrReparentCrossProject).
//  3. Cycle check via CheckParentChildCycle: newParentID must not already
//     be a descendant of id (*ErrDepCycle).
//  4. DELETE every existing parent-child outgoing edge of id (self-heals a
//     double-parented child to exactly one edge), capturing the removed
//     rows for per-edge deletion events.
//  5. INSERT the single id → newParentID parent-child edge.
//  6. Bump the issue: version = version + 1, updated_at = now().
//  7. Emit, in the same tx: one dependency.deleted per removed edge + one
//     dependency.created for the new edge + one issue.reparented (issue-
//     scoped, records old parent(s) + new parent).
//
// Idempotency note: re-running Reparent with the SAME newParentID still
// performs a real DELETE+INSERT (the edge is removed and re-added) and
// bumps the version — it is NOT a no-op. Callers expecting idempotency
// should compare parentIdCurrent first. This matches the explicit-move
// semantics the tool surface wants (every move is an auditable event).
func (s *Store) Reparent(
	ctx context.Context,
	id, newParentID string,
	expectedVersion int,
	caller CallerAccess,
	actor Actor,
) (*ReparentResult, error) {
	if expectedVersion < 1 {
		return nil, newValidationError("missing_version", "expected version is required")
	}
	// Reject self-parenting before opening a tx — cheap, stable error.
	if id == newParentID {
		return nil, ErrReparentSelf
	}

	var result ReparentResult
	err := s.withReparentRetry(ctx, id, func(tx pgx.Tx) error {
		// Step 1: load the child row for the optimistic + lock guards.
		var child reparentRow
		err := tx.QueryRow(ctx, `
SELECT project_id, version, status FROM issues WHERE id = $1`, id).
			Scan(&child.ProjectID, &child.Version, &child.Status)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrFromNotFound
		}
		if err != nil {
			return fmt.Errorf("loading issue %s for reparent: %w", id, err)
		}

		// Optimistic-concurrency predicate. Reparent is NOT lock-gated —
		// it's a graph-edge move like dep_add/dep_remove; concurrent field
		// edits are caught by the version check, not a lock.
		if child.Version != expectedVersion {
			return &ErrVersionConflict{CurrentVersion: child.Version}
		}

		// Step 2: resolve + validate the new parent.
		parent, err := fetchIssue(ctx, tx, newParentID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return ErrTargetNotFound
			}
			return err
		}
		// Same-project requirement (parent-child is intra-project). Check
		// visibility too: even an admin moving cross-project is rejected by
		// the same-project rule, but we surface a cleaner error when the
		// caller can't see the parent's project at all.
		if parent.ProjectID != child.ProjectID {
			// INV-24 (sk-674.18 T033): a parent of another space answers
			// as a nonexistent one, not as «cross-project».
			same, serr := projects.SameSpace(ctx, tx, parent.ProjectID, child.ProjectID)
			if serr != nil {
				return serr
			}
			if !same {
				return ErrTargetNotFound
			}
			if _, verr := projects.EnsureProjectVisible(ctx, tx,
				parent.ProjectID, caller.UserID); verr != nil {
				if errors.Is(verr, projects.ErrProjectNotVisible) {
					return ErrDependencyTargetNotVisible
				}
				return fmt.Errorf("checking new parent visibility: %w", verr)
			}
			return ErrReparentCrossProject
		}

		// sk-118 (F2e): nested-standing ban on the reparent path. Moving the
		// child under a new parent must not place a standing task directly
		// under a standing task. Reject when BOTH the child and the new parent
		// are standing — same §11.4 mutual-retire deadlock the create-edge guard
		// closes. Checked before the cycle / DELETE so no edge churn happens on
		// a rejected move.
		if child.Status == statusStanding && parent.Status == statusStanding {
			return ErrNestedStandingForbidden
		}

		// Step 3: cycle check — newParentID must not be a descendant of id.
		if err := CheckParentChildCycle(ctx, tx, id, newParentID); err != nil {
			return err
		}

		// Step 4: remove ALL existing parent-child outgoing edges of id,
		// returning the rows so we can emit one deletion event each. This
		// collapses a double-parented child (the bug we're closing) to a
		// single edge.
		rows, err := tx.Query(ctx, `
DELETE FROM dependencies
 WHERE from_id = $1 AND type = 'parent-child'
RETURNING to_id, metadata, version, created_by, created_at`, id)
		if err != nil {
			return fmt.Errorf("removing existing parent edges of %s: %w", id, err)
		}
		type removedEdge struct {
			toID      string
			metadata  map[string]any
			version   int
			createdBy uuid.UUID
			createdAt time.Time
		}
		var removed []removedEdge
		for rows.Next() {
			var (
				e         removedEdge
				metaBytes []byte
			)
			if scanErr := rows.Scan(&e.toID, &metaBytes, &e.version, &e.createdBy, &e.createdAt); scanErr != nil {
				rows.Close()
				return fmt.Errorf("scanning removed parent edge: %w", scanErr)
			}
			e.metadata = map[string]any{}
			if len(metaBytes) > 0 {
				_ = json.Unmarshal(metaBytes, &e.metadata)
			}
			removed = append(removed, e)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterating removed parent edges: %w", err)
		}

		oldParentIDs := make([]string, 0, len(removed))
		for _, e := range removed {
			oldParentIDs = append(oldParentIDs, e.toID)
		}

		// Step 5: insert the single new parent-child edge.
		var (
			dto       DependencyDTO
			metaBytes []byte
			createdAt time.Time
		)
		insErr := tx.QueryRow(ctx, `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'parent-child', '{}'::jsonb, 1, $3, now())
RETURNING from_id, to_id, type, metadata, version, created_by, created_at`,
			id, newParentID, caller.UserID).
			Scan(&dto.FromID, &dto.ToID, &dto.Type, &metaBytes,
				&dto.Version, &dto.CreatedBy, &createdAt)
		if insErr != nil {
			if classified := classifyInsertError(insErr); classified != nil {
				return classified
			}
			return fmt.Errorf("inserting new parent edge: %w", insErr)
		}
		dto.CreatedAt = createdAt
		dto.Metadata = map[string]any{}
		if len(metaBytes) > 0 {
			_ = json.Unmarshal(metaBytes, &dto.Metadata)
		}

		// Step 6: bump the issue row (version + updated_at), mirroring the
		// issues store version-bump pattern.
		ct, err := tx.Exec(ctx, `
UPDATE issues SET version = version + 1, updated_at = now() WHERE id = $1`, id)
		if err != nil {
			return fmt.Errorf("bumping issue %s version: %w", id, err)
		}
		if ct.RowsAffected() == 0 {
			// The row vanished between the load and the bump (concurrent
			// delete). Surface as version_conflict so the caller re-reads.
			return &ErrVersionConflict{CurrentVersion: child.Version}
		}

		// Step 7: events. One dependency.deleted per removed edge…
		for _, e := range removed {
			oldData := map[string]any{
				"fromId":    id,
				"toId":      e.toID,
				"type":      DepParentChild,
				"metadata":  e.metadata,
				"version":   e.version,
				"createdBy": e.createdBy,
				"createdAt": e.createdAt,
			}
			if _, werr := WriteDependencyEvent(ctx, tx, EventDependencyDeleted,
				id, e.toID, DepParentChild, oldData, child.ProjectID, actor); werr != nil {
				return werr
			}
		}
		// …one dependency.created for the new edge…
		newData := map[string]any{
			"fromId":    dto.FromID,
			"toId":      dto.ToID,
			"type":      dto.Type,
			"metadata":  dto.Metadata,
			"createdBy": dto.CreatedBy,
		}
		if _, werr := WriteDependencyEvent(ctx, tx, EventDependencyCreated,
			dto.FromID, dto.ToID, dto.Type, newData, child.ProjectID, actor); werr != nil {
			return werr
		}
		// …and one issue.reparented summary event (issue-scoped).
		reparentEventID, werr := WriteReparentEvent(ctx, tx, id, child.ProjectID,
			oldParentIDs, newParentID, actor)
		if werr != nil {
			return werr
		}

		// Fan out the issue.reparented event to the issue's watchers +
		// project watchers, in the SAME tx, so the move shows up in the
		// notification bell. The dependencies package does not auto-fan-out
		// its dependency.* edge events (those are audit-only here); the
		// issue-scoped reparent summary IS notifiable — the registry maps it
		// onto the "Move in the tree" reason — so we call FanOut explicitly,
		// mirroring how the issues store fans out status_changed. FanOut is
		// gated by the registry itself: a type the registry calls audit-only
		// is a no-op there, not an error here.
		sessionID, actorUserID, actorType := normaliseActor(actor)
		_ = sessionID // actor_session_id is recorded on the event row, not by FanOut.
		var fanoutUserID uuid.UUID
		if id, ok := actorUserID.(uuid.UUID); ok {
			fanoutUserID = id
		}
		if _, ferr := notifications.FanOut(ctx, tx, notifications.FanOutInput{
			EventID:     reparentEventID,
			EventType:   EventIssueReparented,
			EntityType:  "issue",
			IssueID:     id,
			ProjectID:   child.ProjectID,
			ActorUserID: fanoutUserID,
			ActorType:   actorType,
		}); ferr != nil {
			return fmt.Errorf("fanning out %s for %s: %w", EventIssueReparented, id, ferr)
		}

		result = ReparentResult{NewEdge: dto, OldParentIDs: oldParentIDs}
		return nil
	})
	if err != nil {
		// A whole-tx SERIALIZABLE abort (SQLSTATE 40001) rolled everything
		// back and made no durable change — it is safe to retry verbatim,
		// and the issue's version did NOT move, so version_conflict is the
		// wrong signal. (A best-effort re-read could also fail and yield
		// CurrentVersion=0, which then trips the caller's expectedVersion>=1
		// guard and turns a retryable abort into a hard missing_version.)
		// Surface the dedicated retryable error instead.
		if isSerializationFailure(err) {
			return nil, ErrSerializationRetry
		}
		return nil, err
	}
	return &result, nil
}

// reparentMaxAttempts is Reparent's own replay budget (sk-622.21, R17, K2):
// the move fans issue.reparented out to subscribers inside the same
// SERIALIZABLE transaction, and the subtree fan-out now walks the canonical
// ancestor chain (internal/hierarchy), so an abort against a concurrent
// subscription or move is an expected outcome of ordinary concurrency. The
// contention is incidental — one child row, its parent edges and the
// watchers of its chain — so 3 attempts, like the issues package's
// claimMaxAttempts; a separate constant so it can move on its own evidence.
const reparentMaxAttempts = 3

// pgDeadlockDetected is SQLSTATE 40P01. Together with 40001 it is the class-40
// abort that rolls a transaction back whole and is safe to replay.
const pgDeadlockDetected = "40P01"

// withReparentRetry runs fn in a SERIALIZABLE transaction and replays it on a
// class-40 abort (40001 / 40P01) up to reparentMaxAttempts times, with a
// small jittered backoff. fn re-reads everything per attempt. Returns the
// last error when the budget runs out; Reparent keeps its classification of
// it (40001 → ErrSerializationRetry).
func (s *Store) withReparentRetry(ctx context.Context, id string, fn func(tx pgx.Tx) error) error {
	var err error
	for attempt := range reparentMaxAttempts {
		err = signals.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, fn)
		if err == nil {
			return nil
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || (pgErr.Code != pgSerializationFailure && pgErr.Code != pgDeadlockDetected) {
			return err
		}
		if attempt == reparentMaxAttempts-1 {
			break
		}
		slog.WarnContext(ctx, "reparent hit a retryable abort; retrying",
			"issueId", id, "attempt", attempt+1, "maxAttempts", reparentMaxAttempts,
			"sqlstate", pgErr.Code)
		wait := 25*time.Millisecond*time.Duration(attempt+1) +
			time.Duration(rand.Int64N(int64(25*time.Millisecond)))
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	return err
}

// isSerializationFailure reports whether err is Postgres SQLSTATE 40001
// (serialization_failure). Mirrors the issues package helper of the same
// name — duplicated to keep dependencies free of an issues import.
func isSerializationFailure(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgSerializationFailure
}

// ----------------- List -----------------

// List returns the outgoing and incoming edges for issueID, with embedded
// issue summaries on the OTHER side of each edge. Permission redaction:
// edges remain visible (the user knows there IS a link) but when the
// caller cannot see the other side's project the edge is `redacted=true`,
// carries no issue payload, and its other end is NOT the real issue id but
// a synthetic token "hidden-1", "hidden-2", … numbered in the order of the
// response (sk-674.18 T009; INV-28 (4) allows only the fact of the link).
// The token names nothing: an operation addressed by it answers as for a
// missing issue.
//
// scope is the caller's horizon (authz); visibility is decided in the
// query, per row, by the access relation.
func (s *Store) List(
	ctx context.Context,
	issueID string,
	scope authz.Scope,
) (*DependencyList, error) {
	out := &DependencyList{
		Outgoing: make([]DependencyEdge, 0, 8),
		Incoming: make([]DependencyEdge, 0, 8),
	}

	// Single round-trip: outgoing + incoming with a direction flag.
	// direction 0 = outgoing (from_id = issue; issue payload = TO-side),
	// direction 1 = incoming (to_id = issue; issue payload = FROM-side).
	// Order within each direction: type, then the other end's id under the
	// id-order rule of sk-421.2 US-6 — sortListEdges below, not an ORDER BY
	// (sk-426; the database's string order put `x.10` before `x.2`).
	unrestricted, caller, bindErr := scope.Bind(ctx, s.pool)
	if bindErr != nil {
		return nil, bindErr
	}
	rows, err := s.pool.Query(ctx, `
SELECT direction, from_id, to_id, type, metadata, version, created_at,
       issue_id, project_id, title, status, issue_type, priority,
       `+authz.ProjectVisibleExpr("edges", "$2", "$3")+` AS visible
  FROM (
    SELECT 0 AS direction,
           d.from_id, d.to_id, d.type, d.metadata, d.version, d.created_at,
           i.id AS issue_id, i.project_id, i.title, i.status, i.type AS issue_type, i.priority
      FROM dependencies d
      JOIN issues i ON i.id = d.to_id
     WHERE d.from_id = $1
    UNION ALL
    SELECT 1 AS direction,
           d.from_id, d.to_id, d.type, d.metadata, d.version, d.created_at,
           i.id AS issue_id, i.project_id, i.title, i.status, i.type AS issue_type, i.priority
      FROM dependencies d
      JOIN issues i ON i.id = d.from_id
     WHERE d.to_id = $1
  ) edges`, issueID, unrestricted, caller)
	if err != nil {
		return nil, fmt.Errorf("listing deps: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		direction, edge, err := scanListEdge(rows)
		if err != nil {
			return nil, err
		}
		if direction == 0 {
			out.Outgoing = append(out.Outgoing, edge)
		} else {
			out.Incoming = append(out.Incoming, edge)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating deps: %w", err)
	}
	sortListEdges(out.Outgoing, func(e DependencyEdge) string { return e.ToID })
	sortListEdges(out.Incoming, func(e DependencyEdge) string { return e.FromID })

	// Replace the real id of every hidden other end, after the sort, so the
	// tokens follow the response order. The same hidden issue linked twice
	// gets one token.
	tokens := map[string]string{}
	token := func(id string) string {
		if t, ok := tokens[id]; ok {
			return t
		}
		t := fmt.Sprintf("hidden-%d", len(tokens)+1)
		tokens[id] = t
		return t
	}
	for i := range out.Outgoing {
		if out.Outgoing[i].Redacted {
			out.Outgoing[i].ToID = token(out.Outgoing[i].ToID)
		}
	}
	for i := range out.Incoming {
		if out.Incoming[i].Redacted {
			out.Incoming[i].FromID = token(out.Incoming[i].FromID)
		}
	}
	return out, nil
}

// sortListEdges orders one direction of the dependency section: by edge type
// (bytewise — the type vocabulary is ASCII, so this is the order the database
// gave), then by the other end's id under the id-order rule of sk-421.2 US-6
// (idorder.Compare), so `x.2` precedes `x.10` (sk-426). Sorted in Go after the
// fetch, not in SQL: see package idorder. The pair is unique within a
// direction — (from_id, to_id, type) is the table's key — so the order is
// total and does not depend on the fetch order.
func sortListEdges(edges []DependencyEdge, otherEnd func(DependencyEdge) string) {
	slices.SortFunc(edges, func(a, b DependencyEdge) int {
		if c := strings.Compare(a.Type, b.Type); c != 0 {
			return c
		}
		return idorder.Compare(otherEnd(a), otherEnd(b))
	})
}

// scanListEdge reads one row from the List UNION ALL query (direction
// flag first, visibility last) and applies redaction. The joined issue is
// the OTHER side of the edge.
func scanListEdge(rows pgx.Rows) (int, DependencyEdge, error) {
	var (
		direction int
		edge      DependencyEdge
		metaBytes []byte
		ref       IssueRef
		visible   bool
	)
	if err := rows.Scan(
		&direction,
		&edge.FromID, &edge.ToID, &edge.Type, &metaBytes, &edge.Version, &edge.CreatedAt,
		&ref.ID, &ref.ProjectID, &ref.Title, &ref.Status, &ref.Type, &ref.Priority,
		&visible,
	); err != nil {
		return 0, DependencyEdge{}, fmt.Errorf("scanning dep row: %w", err)
	}
	edge.Metadata = map[string]any{}
	if len(metaBytes) > 0 {
		_ = json.Unmarshal(metaBytes, &edge.Metadata)
	}
	if visible {
		edge.Issue = &ref
		return direction, edge, nil
	}
	edge.Redacted = true
	return direction, edge, nil
}

// ----------------- Ready -----------------

// IssueSummary is a slim issue shape used as the Ready response item.
// It deliberately mirrors the public fields of issues.IssueSummary
// (without component/label aggregates which require N+1 joins) — kept
// in this package to avoid an issues<-dependencies import cycle.
//
// Field order/names match issues.IssueSummary so the wire payload looks
// identical to /issues list responses. componentIds / labels are always
// `[]` (not aggregated here for the same N+1 reason); R2.x can layer
// this if the UI needs labels on the Ready board.
type IssueSummary struct {
	ID        string    `json:"id"`
	ProjectID uuid.UUID `json:"projectId"`
	// sk-35: derived from the parent-child dep graph (first edge by
	// created_at); wire field renamed to parentIdCurrent so clients
	// don't assume it's a stored column.
	ParentID     *string     `json:"parentIdCurrent,omitempty"`
	Version      int         `json:"version"`
	Title        string      `json:"title"`
	Type         string      `json:"type"`
	Priority     int         `json:"priority"`
	Status       string      `json:"status"`
	Pinned       bool        `json:"pinned"`
	Assignee     *string     `json:"assignee,omitempty"`
	ComponentIDs []uuid.UUID `json:"componentIds"`
	Labels       []string    `json:"labels"`
	UpdatedAt    time.Time   `json:"updatedAt"`
	CreatedAt    time.Time   `json:"createdAt"`
}

// ListReady runs the Ready algorithm (vision §6 / db-schema §4.3) for one
// project. An issue is "ready" iff:
//
//   - status = 'open'
//   - no outgoing blocks/waits-for edge points at an open blocker
//
// `waits-for` is treated as a structural blocker here too — the gate-aware
// evaluation (some-or-all-children) is R2.4 work; for R1.4 we pessimistically
// gate on "target not closed" which matches user expectation for the simple
// case (no spawner-children yet).
//
// This IS the published contract now (sk-483): the agent guide and the gate
// parameter text (mcp/tools depGateParamDescription) say
// the gate is stored but not evaluated. TestListReady_WaitsForGateIsNotEvaluated
// pins it; evaluating the gate means changing those descriptions in the same
// commit.
//
// Filters are AND-ed: assignee exact match, priority upper bound (≤ N),
// type exact match. Limit is clamped to [1, 100] with default 50.
func (s *Store) ListReady(
	ctx context.Context,
	projectID uuid.UUID,
	filters ReadyFilters,
	limit int,
) ([]IssueSummary, error) {
	if limit < 1 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}

	// Build the WHERE clause in a way that keeps the parameters strictly
	// positional. We always pass projectID in $1; filter parameters get
	// appended as we go.
	args := []any{projectID}
	addArg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	clauses := []string{
		"i.project_id = $1",
		"i.status = 'open'",
		// Subquery: ANY outgoing blocks/waits-for edge whose target is
		// not closed disqualifies this issue. Use ANY-array for the type
		// filter so the partial idx_deps_blockers index can be combined
		// with the broader idx_deps_from_type when waits-for joins.
		`NOT EXISTS (
            SELECT 1 FROM dependencies d
              JOIN issues b ON b.id = d.to_id
             WHERE d.from_id = i.id
               AND d.type = ANY(ARRAY['blocks','waits-for'])
               AND ` + status.GatesReadiness("b") + `
        )`,
	}

	if filters.Assignee != nil {
		clauses = append(clauses, fmt.Sprintf("i.assignee = %s", addArg(*filters.Assignee)))
	}
	if filters.Priority != nil {
		clauses = append(clauses, fmt.Sprintf("i.priority <= %s", addArg(*filters.Priority)))
	}
	if filters.Type != nil {
		clauses = append(clauses, fmt.Sprintf("i.type = %s", addArg(*filters.Type)))
	}
	if filters.ComponentID != nil {
		// Component filter — push the issue_components join into SQL so
		// callers don't have to over-fetch and post-filter.
		clauses = append(clauses, fmt.Sprintf(`EXISTS (
            SELECT 1 FROM issue_components ic
             WHERE ic.issue_id = i.id AND ic.component_id = %s
        )`, addArg(*filters.ComponentID)))
	}

	limitArg := addArg(limit)
	// sk-35: parent_id column is frozen; derive parent from the
	// canonical parent-child dep edge (first by created_at). The reader is
	// told it only when the parent's project is in the reader's horizon
	// (sk-674.18 T009): an issue the reader cannot see is not named.
	parentUnrestricted, parentCaller, bindErr := authz.FromContext(ctx).Bind(ctx, s.pool)
	if bindErr != nil {
		return nil, bindErr
	}
	parentU, parentC := addArg(parentUnrestricted), addArg(parentCaller)
	sql := fmt.Sprintf(`
SELECT i.id, i.project_id,
       (SELECT CASE WHEN `+authz.ProjectVisibleExpr("par", parentU, parentC)+` THEN d.to_id END
          FROM dependencies d
          JOIN issues par ON par.id = d.to_id
          WHERE d.from_id = i.id AND d.type = 'parent-child'
          ORDER BY d.created_at LIMIT 1) AS parent_id_current,
       i.version, i.title,
       i.type, i.priority, i.status, i.pinned, i.assignee,
       i.created_at, i.updated_at
  FROM issues i
 WHERE %s
 ORDER BY i.priority ASC, i.updated_at DESC
 LIMIT %s`, joinAnd(clauses), limitArg)

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("running ready query: %w", err)
	}
	defer rows.Close()

	out := make([]IssueSummary, 0, limit)
	for rows.Next() {
		var s IssueSummary
		var parentID *string
		var assignee *string
		if err := rows.Scan(
			&s.ID, &s.ProjectID, &parentID, &s.Version, &s.Title,
			&s.Type, &s.Priority, &s.Status, &s.Pinned, &assignee,
			&s.CreatedAt, &s.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning ready row: %w", err)
		}
		s.ParentID = parentID
		s.Assignee = assignee
		s.ComponentIDs = []uuid.UUID{}
		s.Labels = []string{}
		out = append(out, s)
	}
	return out, rows.Err()
}

// joinAnd is a tiny helper instead of pulling strings.Join indirectly.
// Keeps the SQL template readable above.
func joinAnd(parts []string) string {
	switch len(parts) {
	case 0:
		return "TRUE"
	case 1:
		return parts[0]
	default:
		out := parts[0]
		for _, p := range parts[1:] {
			out += " AND " + p
		}
		return out
	}
}

// (sk-33) WriteParentChildDep was removed as the auto-mirror from
// issues.parent_id into the dependencies graph. sk-35 restored
// parent-child as the canonical hierarchy mechanism: it's now a
// regular dep edge written directly in issues.createIssueInTx
// (Step 3a) and from skein_dep_add. The issues.parent_id column is
// frozen.
