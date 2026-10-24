package projects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/signals"
)

// ErrRepoNotFound is returned when a repo_id is not present in the
// project's git_repos[].
var ErrRepoNotFound = errors.New("repo not found")

// ErrRepoAmbiguous is returned by ResolveRepoForCommit when no
// repoUrl/repoId is supplied AND the project does not have exactly one
// repo declared in git_repos[]. The caller must explicitly pick a
// repo via repoUrl or repoId in that case — we don't guess.
var ErrRepoAmbiguous = errors.New("project has zero or multiple repos; specify repoUrl or repoId")

// ErrRepoInUse is returned by RemoveRepo when at least one component still
// references the repo. The Components field carries human-readable names
// of the offending components so the handler can surface them to the user.
type ErrRepoInUse struct{ Components []string }

func (e *ErrRepoInUse) Error() string {
	return fmt.Sprintf("repo is referenced by %d component(s)", len(e.Components))
}

// AddRepoArgs is the input for AddRepo.
type AddRepoArgs struct {
	Name          string
	URL           string
	DefaultBranch string
}

// AddRepo appends a new repo to projects.git_repos[]. ID is server-generated
// (uuidv7). Returns the populated Repo so the caller can echo it back.
func (s *Store) AddRepo(ctx context.Context, projectID uuid.UUID, args AddRepoArgs, actor Actor) (*Repo, error) {
	repoID, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("generating repo id: %w", err)
	}
	repo := Repo{
		ID:            repoID,
		Name:          args.Name,
		URL:           args.URL,
		DefaultBranch: args.DefaultBranch,
	}
	repoBytes, err := json.Marshal(repo)
	if err != nil {
		return nil, fmt.Errorf("marshalling new repo: %w", err)
	}

	err = signals.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		// Append to git_repos jsonb array. jsonb_insert with -1 inserts at
		// the end; "create_if_missing => true" lets the array grow.
		ct, err := tx.Exec(ctx, `
UPDATE projects
   SET git_repos  = git_repos || $2::jsonb,
       updated_at = now()
 WHERE id = $1`, projectID, string(repoBytes))
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return ErrNotFound
		}
		_, werr := WriteProjectEvent(ctx, tx, EventProjectRepoAdded, projectID, actor, nil, repo)
		return werr
	})
	if err != nil {
		return nil, err
	}
	return &repo, nil
}

// UpdateRepoArgs is the input for UpdateRepo. Nil fields are skipped.
type UpdateRepoArgs struct {
	Name          *string
	URL           *string
	DefaultBranch *string
}

// UpdateRepo replaces the matching object inside projects.git_repos[]. Uses
// jsonb_set with a path index after locating the repo by id. Returns
// ErrRepoNotFound if no entry has the given id.
func (s *Store) UpdateRepo(ctx context.Context, projectID, repoID uuid.UUID, args UpdateRepoArgs, actor Actor) (*Repo, error) {
	var updated Repo

	err := signals.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		// Read current array, find target by id, mutate in Go, write back.
		// Manipulating jsonb in pure SQL with a path lookup by id is awkward
		// (we'd need a CTE with WITH ORDINALITY) and we already need the
		// original for the event payload anyway.
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT git_repos FROM projects WHERE id = $1`, projectID).Scan(&raw)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var repos []Repo
		if err := json.Unmarshal(raw, &repos); err != nil {
			return fmt.Errorf("decoding git_repos: %w", err)
		}

		idx := -1
		for i, r := range repos {
			if r.ID == repoID {
				idx = i
				break
			}
		}
		if idx == -1 {
			return ErrRepoNotFound
		}

		old := repos[idx]
		updated = old
		if args.Name != nil {
			updated.Name = *args.Name
		}
		if args.URL != nil {
			updated.URL = *args.URL
		}
		if args.DefaultBranch != nil {
			updated.DefaultBranch = *args.DefaultBranch
		}
		repos[idx] = updated

		newRaw, err := json.Marshal(repos)
		if err != nil {
			return fmt.Errorf("marshalling updated git_repos: %w", err)
		}
		if _, err := tx.Exec(ctx, `
UPDATE projects SET git_repos = $2::jsonb, updated_at = now() WHERE id = $1`,
			projectID, string(newRaw)); err != nil {
			return err
		}
		_, werr := WriteProjectEvent(ctx, tx, EventProjectRepoUpdated, projectID, actor, old, updated)
		return werr
	})
	if err != nil {
		return nil, err
	}
	return &updated, nil
}

// RemoveRepo deletes the matching git_repos[] entry, but only after
// verifying no component still references it. The component check runs in
// the same transaction for consistency.
func (s *Store) RemoveRepo(ctx context.Context, projectID, repoID uuid.UUID, actor Actor) error {
	return signals.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		// Components referencing this repo? jsonb_path_exists with @ ?
		// (boolean predicate) does the array scan in one round-trip.
		var refNames []string
		rows, err := tx.Query(ctx, `
SELECT name
  FROM components
 WHERE project_id = $1
   AND archived_at IS NULL
   AND EXISTS (
       SELECT 1 FROM jsonb_array_elements(paths) AS p
        WHERE (p->>'repoId') = $2::text
   )
 ORDER BY name`, projectID, repoID.String())
		if err != nil {
			return fmt.Errorf("checking component references: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return err
			}
			refNames = append(refNames, n)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(refNames) > 0 {
			return &ErrRepoInUse{Components: refNames}
		}

		// Read, mutate, write back — same pattern as UpdateRepo.
		var raw []byte
		err = tx.QueryRow(ctx, `SELECT git_repos FROM projects WHERE id = $1`, projectID).Scan(&raw)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var repos []Repo
		if err := json.Unmarshal(raw, &repos); err != nil {
			return fmt.Errorf("decoding git_repos: %w", err)
		}
		idx := -1
		for i, r := range repos {
			if r.ID == repoID {
				idx = i
				break
			}
		}
		if idx == -1 {
			return ErrRepoNotFound
		}
		removed := repos[idx]
		repos = append(repos[:idx], repos[idx+1:]...)
		newRaw, err := json.Marshal(repos)
		if err != nil {
			return fmt.Errorf("marshalling git_repos: %w", err)
		}
		if _, err := tx.Exec(ctx, `
UPDATE projects SET git_repos = $2::jsonb, updated_at = now() WHERE id = $1`,
			projectID, string(newRaw)); err != nil {
			return err
		}
		_, werr := WriteProjectEvent(ctx, tx, EventProjectRepoRemoved, projectID, actor, removed, nil)
		return werr
	})
}

// RepoQuerier is the minimal subset of pgxpool.Pool / pgx.Tx that
// ResolveRepoForCommit needs. Either type satisfies it, so callers
// can either pass the pool (read-only lookup outside a tx) or pass
// an open transaction (atomic resolve+insert during commit linking).
type RepoQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// LoadProjectRepos returns the parsed git_repos[] list for the given
// project, executing a single SELECT against the supplied querier
// (typically an open SERIALIZABLE pgx.Tx). Use this when a caller
// needs to resolve many commits against the same project without
// re-running ResolveRepoForCommit's SELECT once per commit.
//
// Returns ErrNotFound if the project itself is missing.
func LoadProjectRepos(
	ctx context.Context,
	q RepoQuerier,
	projectID uuid.UUID,
) ([]Repo, error) {
	var raw []byte
	err := q.QueryRow(ctx, `SELECT git_repos FROM projects WHERE id = $1`, projectID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("loading project git_repos: %w", err)
	}
	var repos []Repo
	if err := json.Unmarshal(raw, &repos); err != nil {
		return nil, fmt.Errorf("decoding git_repos: %w", err)
	}
	return repos, nil
}

// ResolveRepoFromList picks the right entry from a pre-loaded
// project.git_repos[] slice using the same rules as
// ResolveRepoForCommit, but without issuing any SQL. Intended for the
// inner loop in linkCommitsInTx where the slice has already been
// fetched once via LoadProjectRepos.
//
// Resolution rules:
//
//   - repoID non-nil  → must exist in repos; else ErrRepoNotFound.
//   - repoURL non-empty (and repoID nil) → exact match against repo.URL;
//     else ErrRepoNotFound.
//   - both empty → if exactly one repo, use it; else ErrRepoAmbiguous.
func ResolveRepoFromList(
	repos []Repo,
	repoURL string,
	repoID *uuid.UUID,
) (uuid.UUID, error) {
	// Explicit repoID wins.
	if repoID != nil {
		for _, r := range repos {
			if r.ID == *repoID {
				return r.ID, nil
			}
		}
		return uuid.Nil, ErrRepoNotFound
	}

	// repoURL fallback — exact-match (canonicalisation deferred per
	// resolveRepoURL's contract).
	if repoURL != "" {
		for _, r := range repos {
			if r.URL == repoURL {
				return r.ID, nil
			}
		}
		return uuid.Nil, ErrRepoNotFound
	}

	// Neither supplied — only safe when there's exactly one candidate.
	if len(repos) == 1 {
		return repos[0].ID, nil
	}
	return uuid.Nil, ErrRepoAmbiguous
}

// ResolveRepoForCommit picks the right git_repos[] entry for a commit
// link, given any combination of (repoID, repoURL). The resolution
// rules mirror the commit-on-close contract:
//
//   - repoID non-nil  → must exist in project.git_repos[]; else ErrRepoNotFound.
//   - repoURL non-empty (and repoID nil) → exact match against
//     project.git_repos[].url; else ErrRepoNotFound.
//   - both empty → if the project has EXACTLY ONE repo, use it;
//     else ErrRepoAmbiguous (caller must disambiguate).
//
// The function loads project.git_repos once and decides in Go to
// avoid two round-trips. Returns ErrNotFound when the project itself
// is missing — the caller should treat that as an internal/visibility
// problem (the project was already proven visible at the tool layer
// before we got here).
//
// Sugar wrapper around LoadProjectRepos + ResolveRepoFromList — for
// callers that resolve a single commit. Loop callers should call
// LoadProjectRepos once and ResolveRepoFromList per commit to avoid
// N redundant SELECTs.
func ResolveRepoForCommit(
	ctx context.Context,
	q RepoQuerier,
	projectID uuid.UUID,
	repoURL string,
	repoID *uuid.UUID,
) (uuid.UUID, error) {
	repos, err := LoadProjectRepos(ctx, q, projectID)
	if err != nil {
		return uuid.Nil, err
	}
	return ResolveRepoFromList(repos, repoURL, repoID)
}

// ListRepos returns the project's git_repos[] in one SELECT against the
// pool. Callers that must validate several repo ids against the same
// project (component path validation) load the list once and match in
// Go instead of issuing one identical SELECT per id.
//
// Returns ErrNotFound when the project row itself is missing.
func (s *Store) ListRepos(ctx context.Context, projectID uuid.UUID) ([]Repo, error) {
	return LoadProjectRepos(ctx, s.pool, projectID)
}
