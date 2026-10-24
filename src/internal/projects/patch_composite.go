package projects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/eventbus"
	"skein-api/internal/search"
	"skein-api/internal/signals"
	"skein-api/internal/spaces"
)

// PatchProjectArgs is the input for PatchProject — a wide-scope project
// patch wrapping name/description/gitlabGroup/archived plus alias and
// gitRepos add/remove batches in a single SERIALIZABLE transaction.
//
// Only the operation actually requested is performed:
//   - nilable scalar (Name, Description, GitlabGroup, Archived) → applied when non-nil
//   - AliasesAdd / AliasesRemove → applied when non-empty
//   - GitReposAdd / GitReposRemove → applied when non-empty
//
// Caller is expected to validate/canonicalise inputs (alias casing, repo URL
// format, name length) before invoking this method — PatchProject only
// translates the request to SQL inside one tx.
type PatchProjectArgs struct {
	Name        *string
	Description *string
	GitlabGroup *string
	// Archived: true → archive (no-op if already archived), false →
	// unarchive (no-op if already active). nil → leave as-is.
	Archived *bool

	AliasesAdd    []string
	AliasesRemove []string

	GitReposAdd    []AddRepoArgs
	GitReposRemove []uuid.UUID

	// Visibility switches the project between spaces.VisibilitySpace and
	// spaces.VisibilityMembers in the same transaction (sk-674.18 T024,
	// spaces.SetVisibilityInTx, US-17). The caller validated the value and
	// the right (authz.ActionChangeVisibility).
	Visibility *string
}

// PatchProjectResult is what PatchProject returns: the updated project plus
// the set of effects that materialised (what was actually changed). The
// counts let the tool layer surface a concise summary in its response.
type PatchProjectResult struct {
	Project        *ProjectDTO
	AliasesAdded   []AliasDTO
	AliasesRemoved []string
	ReposAdded     []Repo
	ReposRemoved   []uuid.UUID
	ArchivedFlip   *bool // non-nil only when archive_at actually changed
	ScalarChanges  []eventbus.FieldChange
}

// PatchProject is the composite atomic patch entry point. Every mutation is
// performed inside a single SERIALIZABLE transaction — partial-write
// failures (e.g. one of the alias inserts collides) roll back the whole
// patch and leave the project untouched.
//
// Semantics:
//   - scalar fields (Name/Description/GitlabGroup) → COALESCE-style: nil
//     skipped, non-nil applied (empty string clears).
//   - Archived: nil skipped; *true archives if currently active (no-op if
//     already archived); *false unarchives if currently archived (no-op
//     if already active). The no-op branch is silent — no error, no event.
//   - AliasesAdd: each is INSERTed in order; first collision returns
//     *ErrAliasTaken and rolls back.
//   - AliasesRemove: each is DELETEd; missing rows return ErrNotFound
//     wrapped with the alias string and roll back.
//   - GitReposAdd: each is appended to git_repos[] with a server-generated
//     uuidv7 id and emits a project.repo_added event.
//   - GitReposRemove: each is removed by id; missing ids return
//     ErrRepoNotFound wrapped with the offending repo id and roll back.
//     The component-references guard from RemoveRepo applies here too:
//     a repo still referenced by a component returns *ErrRepoInUse.
//
// Returns the post-state ProjectDTO (with refreshed counts) plus the list
// of changes that actually materialised.
func (s *Store) PatchProject(
	ctx context.Context,
	id uuid.UUID,
	args PatchProjectArgs,
	callerID uuid.UUID,
	callerRole string,
	actor Actor,
) (*PatchProjectResult, error) {
	// sk-28 store-level backstop.
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
	var out *PatchProjectResult

	err := spaces.WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		// A fresh result per attempt: an attempt that
		// loses a serialization race is rolled back, and the answer names
		// only what the committed attempt materialised. Signals are already
		// per attempt (signals.BeginTxFunc).
		out = &PatchProjectResult{
			AliasesAdded:   []AliasDTO{},
			AliasesRemoved: []string{},
			ReposAdded:     []Repo{},
			ReposRemoved:   []uuid.UUID{},
		}
		// K-7: a visibility change takes the space's lock before any row of
		// the project — the order every membership operation keeps.
		if args.Visibility != nil {
			if err := spaces.LockProjectSpace(ctx, tx, id); err != nil {
				return err
			}
		}
		// Snapshot pre-patch project so we can compute scalar diffs and
		// detect archive/unarchive no-ops before we touch the row.
		oldDTO, err := s.fetchProjectInTx(ctx, tx, id)
		if err != nil {
			return err
		}

		// ------------- Scalars (name / description / gitlabGroup) -------------
		// Only run the UPDATE when at least one scalar arg is non-nil; this
		// avoids a redundant updated_at bump on alias/repo-only patches.
		hasScalar := args.Name != nil || args.Description != nil || args.GitlabGroup != nil
		if hasScalar {
			updArgs := UpdateProjectArgs{
				Name:        args.Name,
				Description: args.Description,
				GitlabGroup: args.GitlabGroup,
			}
			row := tx.QueryRow(ctx, `
UPDATE projects
   SET name         = COALESCE($2, name),
       description  = COALESCE($3, description),
       gitlab_group = COALESCE($4, gitlab_group),
       updated_at   = now()
 WHERE id = $1
RETURNING id`,
				id, args.Name, args.Description, args.GitlabGroup,
			)
			var dummy uuid.UUID
			if err := row.Scan(&dummy); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrNotFound
				}
				return err
			}

			// Recompute scalar diffs against the pre-patch snapshot. The
			// registry walk emits one FieldChange per actually-changed
			// field — same shape PatchIssue / UpdateProject use, fed into
			// WriteHybridUpdateEvents below.
			scalarChanges := eventbus.DiffRegistry(ProjectPatchFields, oldDTO, &updArgs)
			out.ScalarChanges = scalarChanges

			if len(scalarChanges) > 0 {
				if _, _, err := eventbus.WriteHybridUpdateEvents(
					ctx, tx,
					EventProjectUpdated, EventProjectFieldChanged,
					"project", id.String(), &id,
					eventbusActor(actor),
					scalarChanges,
					"",
				); err != nil {
					return err
				}
				// sk-622.21 T017 (US-11): "Project changed".
				if err := signalProjectMembers(ctx, tx, id, signals.AreaProjects); err != nil {
					return err
				}
			}

			// R1.5 search reset when name/description changed.
			if (args.Name != nil && *args.Name != oldDTO.Name) ||
				(args.Description != nil && *args.Description != oldDTO.Description) {
				if err := search.ResetEmbedding(ctx, tx, "projects", "id", id); err != nil {
					return err
				}
			}
		}

		// ------------- Archive flip -------------
		if args.Archived != nil {
			now := time.Now().UTC()
			switch {
			case *args.Archived && oldDTO.ArchivedAt == nil:
				// Archive an active project.
				if _, err := tx.Exec(ctx,
					`UPDATE projects SET archived_at = $2, updated_at = $2 WHERE id = $1`,
					id, now); err != nil {
					return err
				}
				newSnap := *oldDTO
				newSnap.ArchivedAt = &now
				newSnap.UpdatedAt = now
				if _, err := WriteProjectEvent(ctx, tx, EventProjectArchived, id, actor, oldDTO, newSnap); err != nil {
					return err
				}
				flip := true
				out.ArchivedFlip = &flip
			case !*args.Archived && oldDTO.ArchivedAt != nil:
				// Unarchive a previously archived project.
				if _, err := tx.Exec(ctx,
					`UPDATE projects SET archived_at = NULL, updated_at = $2 WHERE id = $1`,
					id, now); err != nil {
					return err
				}
				newSnap := *oldDTO
				newSnap.ArchivedAt = nil
				newSnap.UpdatedAt = now
				if _, err := WriteProjectEvent(ctx, tx, EventProjectUnarchive, id, actor, oldDTO, newSnap); err != nil {
					return err
				}
				flip := false
				out.ArchivedFlip = &flip
			default:
				// No-op: caller asked for the state we're already in.
			}
			// sk-622.21 T017 (US-11): "Project archived / unarchived".
			if out.ArchivedFlip != nil {
				if err := signalProjectMembers(ctx, tx, id, signals.AreaProjects, signals.AreaUnreviewed); err != nil {
					return err
				}
			}
		}

		// ------------- Aliases add -------------
		addedByKind := "agent"
		if actor.Role != "agent" {
			addedByKind = "human"
		}
		for _, alias := range args.AliasesAdd {
			row := tx.QueryRow(ctx, `
INSERT INTO project_aliases (alias, project_id, added_by, added_by_kind, created_at)
VALUES ($1, $2, $3, $4, now())
RETURNING alias, project_id, added_by, added_by_kind, created_at`,
				alias, id, callerID, addedByKind)
			var dto AliasDTO
			if err := row.Scan(&dto.Alias, &dto.ProjectID, &dto.AddedBy, &dto.AddedByKind, &dto.CreatedAt); err != nil {
				if name, ok := isUniqueViolation(err); ok && name != "" {
					return &ErrAliasTaken{Alias: alias}
				}
				return fmt.Errorf("inserting alias %q: %w", alias, err)
			}
			if _, err := WriteProjectEvent(ctx, tx, EventProjectAliasAdded, id, actor, nil, dto); err != nil {
				return err
			}
			out.AliasesAdded = append(out.AliasesAdded, dto)
		}

		// ------------- Aliases remove -------------
		for _, alias := range args.AliasesRemove {
			var dto AliasDTO
			row := tx.QueryRow(ctx, `
DELETE FROM project_aliases
 WHERE alias = $1 AND project_id = $2
RETURNING alias, project_id, added_by, added_by_kind, created_at`, alias, id)
			err := row.Scan(&dto.Alias, &dto.ProjectID, &dto.AddedBy, &dto.AddedByKind, &dto.CreatedAt)
			if errors.Is(err, pgx.ErrNoRows) {
				return &errAliasMissing{Alias: alias}
			}
			if err != nil {
				return fmt.Errorf("deleting alias %q: %w", alias, err)
			}
			if _, err := WriteProjectEvent(ctx, tx, EventProjectAliasRemoved, id, actor, dto, nil); err != nil {
				return err
			}
			out.AliasesRemoved = append(out.AliasesRemoved, alias)
		}

		// ------------- Repos add -------------
		// Each repo inflates git_repos[] with a server-generated id and
		// emits one project.repo_added event. Mirrors AddRepo's approach
		// but stays inside the outer tx instead of opening its own.
		for _, repoArgs := range args.GitReposAdd {
			repoID, err := uuid.NewV7()
			if err != nil {
				return fmt.Errorf("generating repo id: %w", err)
			}
			repo := Repo{
				ID:            repoID,
				Name:          repoArgs.Name,
				URL:           repoArgs.URL,
				DefaultBranch: repoArgs.DefaultBranch,
			}
			repoBytes, err := json.Marshal(repo)
			if err != nil {
				return fmt.Errorf("marshalling new repo: %w", err)
			}
			ct, err := tx.Exec(ctx, `
UPDATE projects
   SET git_repos  = git_repos || $2::jsonb,
       updated_at = now()
 WHERE id = $1`, id, string(repoBytes))
			if err != nil {
				return err
			}
			if ct.RowsAffected() == 0 {
				return ErrNotFound
			}
			if _, err := WriteProjectEvent(ctx, tx, EventProjectRepoAdded, id, actor, nil, repo); err != nil {
				return err
			}
			out.ReposAdded = append(out.ReposAdded, repo)
		}

		// ------------- Repos remove -------------
		// Read once → mutate in Go → write back. Mirrors RemoveRepo: refuses
		// to remove a repo still referenced by a non-archived component.
		if len(args.GitReposRemove) > 0 {
			var raw []byte
			if err := tx.QueryRow(ctx,
				`SELECT git_repos FROM projects WHERE id = $1`, id,
			).Scan(&raw); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrNotFound
				}
				return err
			}
			var repos []Repo
			if err := json.Unmarshal(raw, &repos); err != nil {
				return fmt.Errorf("decoding git_repos: %w", err)
			}

			// Component references for EVERY repo in the removal batch are
			// resolved in one query up front; the loop below then decides
			// per repo from the in-memory map. Same predicate and same
			// name ordering as the per-repo query it replaces, so the
			// first offending repo in caller order still determines which
			// error the client sees.
			refsByRepo, err := componentRefsForRepos(ctx, tx, id, args.GitReposRemove)
			if err != nil {
				return err
			}

			for _, repoID := range args.GitReposRemove {
				idx := -1
				for i, r := range repos {
					if r.ID == repoID {
						idx = i
						break
					}
				}
				if idx == -1 {
					return &errRepoMissing{RepoID: repoID}
				}

				// Refuse if any non-archived component still references this repo.
				if refNames := refsByRepo[repoID]; len(refNames) > 0 {
					return &ErrRepoInUse{Components: refNames}
				}

				removed := repos[idx]
				repos = append(repos[:idx], repos[idx+1:]...)
				if _, err := WriteProjectEvent(ctx, tx, EventProjectRepoRemoved, id, actor, removed, nil); err != nil {
					return err
				}
				out.ReposRemoved = append(out.ReposRemoved, repoID)
			}

			newRaw, err := json.Marshal(repos)
			if err != nil {
				return fmt.Errorf("marshalling git_repos: %w", err)
			}
			if _, err := tx.Exec(ctx, `
UPDATE projects SET git_repos = $2::jsonb, updated_at = now() WHERE id = $1`,
				id, string(newRaw)); err != nil {
				return err
			}
		}
		if args.Visibility != nil {
			return spaces.SetVisibilityInTx(ctx, tx, id, *args.Visibility, spacesActor(actor))
		}
		return nil
	})
	if err != nil {
		// Surface unique-violation that escaped (e.g. project name uniqueness)
		// as a typed conflict; the typed alias/repo errors above propagate
		// as-is.
		if name, ok := isUniqueViolation(err); ok {
			return nil, &ErrConflict{Constraint: name}
		}
		return nil, err
	}

	// Re-fetch with counts.
	full, err := s.GetProject(ctx, id)
	if err != nil {
		return nil, err
	}
	out.Project = full
	return out, nil
}

// errAliasMissing is returned by PatchProject when an alias listed in
// AliasesRemove does not exist on the project. Distinct from ErrNotFound
// so the tool layer can surface a precise validation_error per alias.
type errAliasMissing struct{ Alias string }

func (e *errAliasMissing) Error() string {
	return fmt.Sprintf("alias %q not found on project", e.Alias)
}

// IsAliasMissing unwraps errors from PatchProject to detect the alias-
// not-on-project condition raised by AliasesRemove. Returns the offending
// alias string when matched.
func IsAliasMissing(err error) (string, bool) {
	var e *errAliasMissing
	if errors.As(err, &e) {
		return e.Alias, true
	}
	return "", false
}

// errRepoMissing is returned by PatchProject when a repoId listed in
// GitReposRemove does not exist on the project.
type errRepoMissing struct{ RepoID uuid.UUID }

func (e *errRepoMissing) Error() string {
	return fmt.Sprintf("repo %s not found on project", e.RepoID)
}

// componentRefsForRepos resolves, in ONE query, which non-archived
// components of the project still reference each of the supplied repo
// ids. The result maps repo id → component names ordered by name; a repo
// with no references is simply absent from the map.
//
// The predicate is byte-for-byte the one RemoveRepo runs per repo
// (jsonb_array_elements over components.paths matched on repoId); the
// repo ids are joined in via unnest so a removal batch of N repos costs
// one round-trip instead of N. `r.repo_id::text` renders the canonical
// lowercase form, i.e. the same string uuid.UUID.String() produces, so
// the textual comparison against paths[].repoId is unchanged.
//
// Runs inside the caller's transaction — the guard and the git_repos[]
// rewrite must see the same snapshot.
func componentRefsForRepos(
	ctx context.Context,
	tx pgx.Tx,
	projectID uuid.UUID,
	repoIDs []uuid.UUID,
) (map[uuid.UUID][]string, error) {
	if len(repoIDs) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
SELECT r.repo_id, c.name
  FROM unnest($2::uuid[]) AS r(repo_id)
  JOIN components c
    ON c.project_id = $1
   AND c.archived_at IS NULL
   AND EXISTS (
       SELECT 1 FROM jsonb_array_elements(c.paths) AS p
        WHERE (p->>'repoId') = r.repo_id::text
   )
 ORDER BY r.repo_id, c.name`, projectID, repoIDs)
	if err != nil {
		return nil, fmt.Errorf("checking component references: %w", err)
	}
	defer rows.Close()

	out := make(map[uuid.UUID][]string, len(repoIDs))
	for rows.Next() {
		var repoID uuid.UUID
		var name string
		if err := rows.Scan(&repoID, &name); err != nil {
			return nil, err
		}
		out[repoID] = append(out[repoID], name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// IsRepoMissing unwraps errors from PatchProject to detect the repo-not-
// on-project condition raised by GitReposRemove. Returns the offending
// repo id when matched.
func IsRepoMissing(err error) (uuid.UUID, bool) {
	var e *errRepoMissing
	if errors.As(err, &e) {
		return e.RepoID, true
	}
	return uuid.Nil, false
}
