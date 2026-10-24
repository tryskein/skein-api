package projects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/assignee"
	"skein-api/internal/authz"
	"skein-api/internal/eventbus"
	"skein-api/internal/search"
	"skein-api/internal/signals"
	"skein-api/internal/status"
)

// CreateComponentArgs is the input shape for CreateComponent.
type CreateComponentArgs struct {
	Name        string
	Description string
	Paths       []ComponentPath
}

// CreateComponent inserts a new components row. Caller must have already
// validated each path's repo_id against the project's git_repos[] (use
// ListRepos). Returns ErrConflict on UNIQUE(project_id, name) collisions.
func (s *Store) CreateComponent(
	ctx context.Context,
	projectID uuid.UUID,
	createdBy uuid.UUID,
	args CreateComponentArgs,
	actor Actor,
) (*ComponentDTO, error) {
	// sk-28 store-level backstop. MCP wraps these at the boundary; REST
	// funnels raw struct fields straight here.
	if err := ValidateProse("name", args.Name); err != nil {
		return nil, err
	}
	if err := ValidateProse("description", args.Description); err != nil {
		return nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("generating component id: %w", err)
	}

	pathsBytes, err := marshalPaths(args.Paths)
	if err != nil {
		return nil, err
	}

	var dto ComponentDTO
	err = signals.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
INSERT INTO components (id, project_id, name, description, paths, archived_at, created_by, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5::jsonb, NULL, $6, now(), now())
RETURNING id, project_id, name, description, paths, archived_at, created_by, created_at, updated_at`,
			id, projectID, args.Name, args.Description, string(pathsBytes), createdBy)
		var pathsJSON []byte
		var archivedAt *time.Time
		if err := row.Scan(&dto.ID, &dto.ProjectID, &dto.Name, &dto.Description,
			&pathsJSON, &archivedAt, &dto.CreatedBy, &dto.CreatedAt, &dto.UpdatedAt); err != nil {
			return err
		}
		dto.ArchivedAt = archivedAt
		dto.Paths = unmarshalPaths(pathsJSON)
		_, werr := WriteComponentEvent(ctx, tx, EventComponentCreated, dto.ID, projectID, actor, nil, dto)
		return werr
	})
	if err != nil {
		if name, ok := isUniqueViolation(err); ok {
			return nil, &ErrConflict{Constraint: name}
		}
		return nil, err
	}
	return &dto, nil
}

// UpdateComponentArgs is the partial-update input. Nil fields are skipped.
// Paths uses a pointer to a slice so we can distinguish "set to empty"
// from "leave alone".
type UpdateComponentArgs struct {
	Name        *string
	Description *string
	Paths       *[]ComponentPath
}

// UpdateComponent updates the row, returns ErrNotFound on missing component
// or wrong project. Caller validates path repo_ids beforehand.
func (s *Store) UpdateComponent(
	ctx context.Context,
	projectID, componentID uuid.UUID,
	args UpdateComponentArgs,
	actor Actor,
) (*ComponentDTO, error) {
	// sk-28 store-level backstop. Pointer-typed → only validate when supplied.
	if args.Name != nil {
		if err := ValidateProse("name", *args.Name); err != nil {
			return nil, err
		}
	}
	if args.Description != nil {
		if err := ValidateProse("description", *args.Description); err != nil {
			return nil, err
		}
	}
	var dto ComponentDTO

	err := signals.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		old, err := s.fetchComponentInTx(ctx, tx, projectID, componentID)
		if err != nil {
			return err
		}

		var pathsArg any
		if args.Paths != nil {
			b, err := marshalPaths(*args.Paths)
			if err != nil {
				return err
			}
			pathsArg = string(b)
		}

		row := tx.QueryRow(ctx, `
UPDATE components
   SET name        = COALESCE($3, name),
       description = COALESCE($4, description),
       paths       = COALESCE($5::jsonb, paths),
       updated_at  = now()
 WHERE id = $1 AND project_id = $2
RETURNING id, project_id, name, description, paths, archived_at, created_by, created_at, updated_at`,
			componentID, projectID, args.Name, args.Description, pathsArg)
		var pathsJSON []byte
		var archivedAt *time.Time
		if err := row.Scan(&dto.ID, &dto.ProjectID, &dto.Name, &dto.Description,
			&pathsJSON, &archivedAt, &dto.CreatedBy, &dto.CreatedAt, &dto.UpdatedAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		dto.ArchivedAt = archivedAt
		dto.Paths = unmarshalPaths(pathsJSON)

		// R1.5 search: reset embedding when name/description change.
		// Path-list changes don't feed BuildComponentEmbedText.
		if (args.Name != nil && *args.Name != old.Name) ||
			(args.Description != nil && *args.Description != old.Description) {
			if err := search.ResetEmbedding(ctx, tx, "components", "id", componentID); err != nil {
				return err
			}
		}

		// R3.2b hybrid event write: parent component.updated (full diff)
		// + N component.field_changed children, all on the same tx.
		changes := eventbus.DiffRegistry(ComponentPatchFields, old, &args)
		if _, _, err := eventbus.WriteHybridUpdateEvents(
			ctx, tx,
			EventComponentUpdated, EventComponentFieldChanged,
			"component", componentID.String(), &projectID,
			eventbusActor(actor),
			changes,
			"",
		); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		if name, ok := isUniqueViolation(err); ok {
			return nil, &ErrConflict{Constraint: name}
		}
		return nil, err
	}
	return &dto, nil
}

// ArchiveComponent sets archived_at = now(); idempotency mirrors the
// project archive path (ErrAlreadyArchived).
func (s *Store) ArchiveComponent(ctx context.Context, projectID, componentID uuid.UUID, actor Actor) error {
	return signals.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		old, err := s.fetchComponentInTx(ctx, tx, projectID, componentID)
		if err != nil {
			return err
		}
		if old.ArchivedAt != nil {
			return ErrAlreadyArchived
		}
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `
UPDATE components SET archived_at = $3, updated_at = $3 WHERE id = $1 AND project_id = $2`,
			componentID, projectID, now); err != nil {
			return err
		}
		new := *old
		new.ArchivedAt = &now
		new.UpdatedAt = now
		_, werr := WriteComponentEvent(ctx, tx, EventComponentArchived, componentID, projectID, actor, old, new)
		return werr
	})
}

func (s *Store) UnarchiveComponent(ctx context.Context, projectID, componentID uuid.UUID, actor Actor) error {
	return signals.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		old, err := s.fetchComponentInTx(ctx, tx, projectID, componentID)
		if err != nil {
			return err
		}
		if old.ArchivedAt == nil {
			return ErrNotArchived
		}
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `
UPDATE components SET archived_at = NULL, updated_at = $3 WHERE id = $1 AND project_id = $2`,
			componentID, projectID, now); err != nil {
			return err
		}
		new := *old
		new.ArchivedAt = nil
		new.UpdatedAt = now
		_, werr := WriteComponentEvent(ctx, tx, EventComponentUnarchive, componentID, projectID, actor, old, new)
		return werr
	})
}

// GetComponent fetches a single component scoped to its project. Returns
// ErrNotFound when the (project, component) pair doesn't exist.
func (s *Store) GetComponent(ctx context.Context, projectID, componentID uuid.UUID) (*ComponentDTO, error) {
	var dto ComponentDTO
	var pathsJSON []byte
	var archivedAt *time.Time
	row := s.pool.QueryRow(ctx, `
SELECT id, project_id, name, description, paths, archived_at, created_by, created_at, updated_at
  FROM components
 WHERE id = $1 AND project_id = $2`, componentID, projectID)
	err := row.Scan(&dto.ID, &dto.ProjectID, &dto.Name, &dto.Description,
		&pathsJSON, &archivedAt, &dto.CreatedBy, &dto.CreatedAt, &dto.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	dto.ArchivedAt = archivedAt
	dto.Paths = unmarshalPaths(pathsJSON)
	return &dto, nil
}

// ListComponentsArgs is the extended argument set for ListComponents.
// Lens="on-me" / "delegated" narrows the result to components with at
// least one matching issue under the active lens (HAVING filter),
// mirroring ListProjects. Empty Lens returns every component the
// caller can see (the historical behaviour).
type ListComponentsArgs struct {
	ProjectID uuid.UUID
	Archived  string
	CallerID  uuid.UUID
	// sk-524 — the name is no longer here: the gate asks "is the caller
	// allowed?" by CallerID. The full argument is at ListProjectsArgs.
	Lens string
}

// ListComponents returns components for a project. archived='false' (default)
// hides archived; 'true' shows only archived; 'all' shows everything.
//
// Each row carries open/in_progress/blocked issue counts aggregated via
// LEFT JOIN issue_components → issues. GROUP BY components.id keeps one
// row per component; the LEFT JOIN ensures components with zero linked
// issues still surface (with all counts at 0).
//
// Caller-scoped (My-assigned-mode / outbox) MyOutbox* counts narrow to
// "issues the caller created AND delegated to someone else"
// (created_by = caller AND assignee IS NOT NULL AND
// LOWER(assignee) <> LOWER(callerUsername)). Pass uuid.Nil + ""
// to leave them at zero — the filter predicates never match a real
// row in that case, mirroring the GetProjectWithStats fallback.
func (s *Store) ListComponents(ctx context.Context, projectID uuid.UUID, archived string, callerID uuid.UUID, callerUsername string) ([]ComponentDTO, error) {
	return s.ListComponentsWithLens(ctx, ListComponentsArgs{
		ProjectID: projectID,
		Archived:  archived,
		CallerID:  callerID,
	})
}

// ListComponentsWithLens is the lens-aware variant. Existing callers
// that don't care about lens narrowing keep using ListComponents
// (above) which delegates here with an empty Lens.
func (s *Store) ListComponentsWithLens(ctx context.Context, args ListComponentsArgs) ([]ComponentDTO, error) {
	clause := "AND c.archived_at IS NULL"
	switch args.Archived {
	case "true":
		clause = "AND c.archived_at IS NOT NULL"
	case "all":
		clause = ""
	}

	// Lens HAVING (sk-30): drop components whose lens-scoped counts
	// are all zero. callerID is $2 in
	// the prepared statement below; buildLensHaving renders those
	// placeholders into the HAVING expression.
	//
	// sk-423: every «blocked» leg and its open / in_progress complement
	// count only blockers inside the caller's horizon — $3 / $4, bound from
	// the request context (authz.ReaderBlockedExpr), the same rule as
	// the project row.
	hU, hC := "$3", "$4"
	having := buildLensHaving(args.Lens, "$2", hU, hC)
	hUnrestricted, hCaller, bindErr := authz.FromContext(ctx).Bind(ctx, s.pool)
	if bindErr != nil {
		return nil, bindErr
	}

	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT c.id, c.project_id, c.name, c.description, c.paths, c.archived_at,
       c.created_by, c.created_at, c.updated_at,
       -- sk-31: open/in_progress exclude derived-blocked so pills are disjoint.
       COUNT(i.id) FILTER (
           WHERE i.status = 'open'
             AND NOT `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS open_count,
       COUNT(i.id) FILTER (
           WHERE i.status = 'in_progress'
             AND NOT `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS in_progress_count,
       COUNT(i.id) FILTER (
           WHERE `+status.ActiveWork("i")+`
             AND `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS blocked_count,
       -- sk-118.8: standing is its own aggregate (decision #4). Unscoped,
       -- no readiness gate.
       COUNT(i.id) FILTER (WHERE i.status = 'standing') AS standing_count,
       COUNT(i.id) FILTER (
           WHERE i.status = 'open'
             AND i.created_by = $2
             AND i.assignee IS NOT NULL
             AND NOT `+assignee.MatchesCaller("i", "$2")+`
             AND NOT `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS my_outbox_open_count,
       COUNT(i.id) FILTER (
           WHERE i.status = 'in_progress'
             AND i.created_by = $2
             AND i.assignee IS NOT NULL
             AND NOT `+assignee.MatchesCaller("i", "$2")+`
             AND NOT `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS my_outbox_in_progress_count,
       COUNT(i.id) FILTER (
           WHERE `+status.ActiveWork("i")+`
             AND i.created_by = $2
             AND i.assignee IS NOT NULL
             AND NOT `+assignee.MatchesCaller("i", "$2")+`
             AND `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS my_outbox_blocked_count,
       -- sk-118.8: outbox-scoped standing pill — same Delegated predicate
       -- as my_outbox_open_count, narrowed to status='standing'.
       COUNT(i.id) FILTER (
           WHERE i.status = 'standing'
             AND i.created_by = $2
             AND i.assignee IS NOT NULL
             AND NOT `+assignee.MatchesCaller("i", "$2")+`
       ) AS my_outbox_standing_count,
       COUNT(i.id) FILTER (
           WHERE i.status = 'open'
             AND i.created_by <> $2
             AND i.accepted_at IS NOT NULL
             AND `+assignee.MatchesCaller("i", "$2")+`
             AND NOT `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS on_me_open_count,
       COUNT(i.id) FILTER (
           WHERE i.status = 'in_progress'
             AND i.created_by <> $2
             AND i.accepted_at IS NOT NULL
             AND `+assignee.MatchesCaller("i", "$2")+`
             AND NOT `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS on_me_in_progress_count,
       COUNT(i.id) FILTER (
           WHERE `+status.ActiveWork("i")+`
             AND i.created_by <> $2
             AND i.accepted_at IS NOT NULL
             AND `+assignee.MatchesCaller("i", "$2")+`
             AND `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS on_me_blocked_count
  FROM components c
  LEFT JOIN issue_components ic ON ic.component_id = c.id
  LEFT JOIN issues i            ON i.id = ic.issue_id
 WHERE c.project_id = $1 %s
 GROUP BY c.id
%s
 ORDER BY c.name`, clause, having), args.ProjectID, args.CallerID,
		hUnrestricted, hCaller)
	if err != nil {
		return nil, fmt.Errorf("listing components: %w", err)
	}
	defer rows.Close()

	out := make([]ComponentDTO, 0)
	for rows.Next() {
		var d ComponentDTO
		var pathsJSON []byte
		var archivedAt *time.Time
		if err := rows.Scan(&d.ID, &d.ProjectID, &d.Name, &d.Description,
			&pathsJSON, &archivedAt, &d.CreatedBy, &d.CreatedAt, &d.UpdatedAt,
			&d.OpenCount, &d.InProgressCount, &d.BlockedCount, &d.StandingCount,
			&d.MyOutboxOpenCount, &d.MyOutboxInProgressCount, &d.MyOutboxBlockedCount,
			&d.MyOutboxStandingCount,
			&d.OnMeOpenCount, &d.OnMeInProgressCount, &d.OnMeBlockedCount); err != nil {
			return nil, fmt.Errorf("scanning component row: %w", err)
		}
		d.ArchivedAt = archivedAt
		d.Paths = unmarshalPaths(pathsJSON)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) fetchComponentInTx(ctx context.Context, tx pgx.Tx, projectID, componentID uuid.UUID) (*ComponentDTO, error) {
	var dto ComponentDTO
	var pathsJSON []byte
	var archivedAt *time.Time
	row := tx.QueryRow(ctx, `
SELECT id, project_id, name, description, paths, archived_at, created_by, created_at, updated_at
  FROM components
 WHERE id = $1 AND project_id = $2`, componentID, projectID)
	err := row.Scan(&dto.ID, &dto.ProjectID, &dto.Name, &dto.Description,
		&pathsJSON, &archivedAt, &dto.CreatedBy, &dto.CreatedAt, &dto.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	dto.ArchivedAt = archivedAt
	dto.Paths = unmarshalPaths(pathsJSON)
	return &dto, nil
}

// lowerOrNil returns a lowercased trimmed copy of `s`, or nil when the
// trimmed result is empty. Used by ListComponents to bind the caller
// username as either a real text value or SQL NULL — empty strings
// would otherwise compare equal to themselves and break the
// MyOutbox* FILTER predicates.
func lowerOrNil(s string) any {
	if u := strings.ToLower(strings.TrimSpace(s)); u != "" {
		return u
	}
	return nil
}

func marshalPaths(paths []ComponentPath) ([]byte, error) {
	if paths == nil {
		paths = []ComponentPath{}
	}
	b, err := json.Marshal(paths)
	if err != nil {
		return nil, fmt.Errorf("marshalling paths: %w", err)
	}
	return b, nil
}

func unmarshalPaths(raw []byte) []ComponentPath {
	out := []ComponentPath{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

// CountArchivedComponents returns how many of the project's components are
// archived (archived_at IS NOT NULL). It exists for the MCP list answer,
// which reports the count beside `items` so a caller can tell "this project
// has no components at all" from "it has components, they are all archived"
// without a second call with archivedFilter="all".
//
// Deliberately not lens-aware and not filter-aware: the question it answers
// is about the project's component set, not about the current listing.
func (s *Store) CountArchivedComponents(ctx context.Context, projectID uuid.UUID) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
SELECT COUNT(*) FROM components WHERE project_id = $1 AND archived_at IS NOT NULL`,
		projectID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting archived components: %w", err)
	}
	return n, nil
}
