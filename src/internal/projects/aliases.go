package projects

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/authz"
	"skein-api/internal/signals"
)

// ErrAliasTaken is the typed error for "alias already exists for some
// project". OwnerProjectID and OwnerProjectName are populated when the
// owning project is visible to the caller; otherwise (collision against a
// hidden project) only the alias itself is surfaced.
type ErrAliasTaken struct {
	Alias            string
	OwnerProjectID   *uuid.UUID
	OwnerProjectName *string
}

func (e *ErrAliasTaken) Error() string {
	return fmt.Sprintf("alias %q is already taken", e.Alias)
}

// AddAlias inserts a new alias mapping. The collision branch translates to
// ErrAliasTaken with optional owner context (only when the owner project
// is visible to the caller — vision §10 "do not leak existence").
func (s *Store) AddAlias(ctx context.Context, projectID, addedBy uuid.UUID, alias string, actor Actor) (*AliasDTO, error) {
	var dto AliasDTO

	err := signals.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
INSERT INTO project_aliases (alias, project_id, added_by, added_by_kind, created_at)
VALUES ($1, $2, $3, 'human', now())
RETURNING alias, project_id, added_by, added_by_kind, created_at`,
			alias, projectID, addedBy)
		if err := row.Scan(&dto.Alias, &dto.ProjectID, &dto.AddedBy, &dto.AddedByKind, &dto.CreatedAt); err != nil {
			return err
		}
		_, err := WriteProjectEvent(ctx, tx, EventProjectAliasAdded, projectID, actor, nil, dto)
		return err
	})
	if err != nil {
		if name, ok := isUniqueViolation(err); ok && name != "" {
			// Collision — go fetch owner if visible to the caller.
			conflict := &ErrAliasTaken{Alias: alias}
			s.populateAliasOwner(ctx, alias, addedBy, conflict)
			return nil, conflict
		}
		return nil, err
	}
	return &dto, nil
}

// populateAliasOwner does a best-effort lookup of the project that already
// owns the alias. Only fills in the conflict struct when the caller can
// already see that project — the access relation decides (authz).
func (s *Store) populateAliasOwner(ctx context.Context, alias string, callerID uuid.UUID, conflict *ErrAliasTaken) {
	_, visible, bindErr := authz.ScopeFor(ctx, callerID).Bind(ctx, s.pool)
	if bindErr != nil {
		return
	}
	var ownerID uuid.UUID
	var ownerName string
	err := s.pool.QueryRow(ctx, `
SELECT pa.project_id, p.name
  FROM project_aliases pa
  JOIN projects p ON p.id = pa.project_id
 WHERE pa.alias = $1
   AND `+authz.VisibleExpr("pa.project_id", "false", "$2"),
		alias, visible).Scan(&ownerID, &ownerName)
	if err != nil {
		// Not visible (or the alias was already deleted in a race) — leave
		// owner fields nil.
		return
	}
	conflict.OwnerProjectID = &ownerID
	conflict.OwnerProjectName = &ownerName
}

// RemoveAlias deletes the alias row. ErrNotFound when the alias doesn't
// exist or doesn't belong to this project (we deliberately don't
// distinguish — a malicious caller wouldn't get more info than "not in
// your project").
func (s *Store) RemoveAlias(ctx context.Context, projectID uuid.UUID, alias string, actor Actor) error {
	return signals.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		var dto AliasDTO
		row := tx.QueryRow(ctx, `
DELETE FROM project_aliases
 WHERE alias = $1 AND project_id = $2
RETURNING alias, project_id, added_by, added_by_kind, created_at`, alias, projectID)
		err := row.Scan(&dto.Alias, &dto.ProjectID, &dto.AddedBy, &dto.AddedByKind, &dto.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		_, err = WriteProjectEvent(ctx, tx, EventProjectAliasRemoved, projectID, actor, dto, nil)
		return err
	})
}

// ListAliases returns every alias bound to the given project, ordered
// alphabetically.
func (s *Store) ListAliases(ctx context.Context, projectID uuid.UUID) ([]AliasDTO, error) {
	rows, err := s.pool.Query(ctx, `
SELECT alias, project_id, added_by, added_by_kind, created_at
  FROM project_aliases
 WHERE project_id = $1
 ORDER BY alias`, projectID)
	if err != nil {
		return nil, fmt.Errorf("listing aliases: %w", err)
	}
	defer rows.Close()

	out := make([]AliasDTO, 0)
	for rows.Next() {
		var d AliasDTO
		if err := rows.Scan(&d.Alias, &d.ProjectID, &d.AddedBy, &d.AddedByKind, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning alias row: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
