package search

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"skein-api/internal/authz"
)

// candidateLimit is the per-CTE top-K used inside vec_candidates and
// fts_candidates. RRF math (`1/(60+rank)`) means score contributions
// beyond rank 50 are <0.012, so a tighter cutoff than 50 would not
// materially change the merged ordering.  Going wider (200, 500) only
// inflates the FULL OUTER JOIN cost.
const candidateLimit = 50

// rrfK is the smoothing constant in the RRF score formula. The 60 default
// from the original RRF paper is industry standard; bigger K flattens
// score differences (good for noisy retrievers), smaller K rewards
// top-of-list more aggressively. We stay with 60.
const rrfK = 60

// resultCachePoolLimit is the maximum number of items we materialise per
// search.  Cursor pagination slices into this slice; values up to 200
// give a UI room to "load more" up to four pages without re-running the
// query.
const resultCachePoolLimit = 200

// idPrefixPattern matches anything that looks like a typed-out skein
// entity ID. We accept project id_prefixes (`demo`), counter IDs
// (`demo-42`), hash IDs (`demo-a3b7c2`), and hierarchical children
// (`demo-42.1.3`).  Length floor 2 chars to avoid catching every typed
// letter as the user types.
var idPrefixPattern = regexp.MustCompile(`^[a-z0-9_-]+(\.\d+)*$`)

// bareNumberPattern matches a query that is purely digits (e.g. `15`,
// `77`). Such a query can't prefix-match a skein id (ids always start
// with a letter id_prefix), so we route it through the numeric-tail
// branch instead: match issues whose hierarchical id ends in `-N`
// (top-level, sk-15) or `.N` (child, sk-2.15). Digits-only is also the
// safety gate that lets us interpolate the value into a PG regex with a
// bind parameter without risking metacharacter injection.
var bareNumberPattern = regexp.MustCompile(`^\d+$`)

// Store is the database gateway for the search package. Like every other
// store in skein-api it holds an immutable pool reference and is safe to
// share across goroutines.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a Store backed by the given pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// SearchInput is the per-kind query input. We pre-build it once in the
// service layer and pass it to each entity-specific function so the
// per-kind code stays focused on its SQL.
type SearchInput struct {
	Query     string              // raw user query (passed through plainto_tsquery on PG side)
	Embedding pgvector.HalfVector // zero value when vec unavailable; check Vec is nil
	Vec       []float32           // raw vector (nil = FTS-only fallback)
	Project   *uuid.UUID
	// Space narrows every kind to the projects of one space (sk-674.18
	// T031, the page filter `space=`); nil — every space the caller sees.
	Space    *uuid.UUID
	CallerID uuid.UUID
	Filters  Filters
	Limit    int // top-K within this kind; service caps overall result-set
}

// visibleSet binds the caller's visible set (authz, the access relation) for
// one store call — the per-request memo of the context when the caller is
// the request's principal.
func (s *Store) visibleSet(ctx context.Context, callerID uuid.UUID) ([]uuid.UUID, error) {
	_, visible, err := authz.ScopeFor(ctx, callerID).Bind(ctx, s.pool)
	return visible, err
}

// visibilityFingerprint is a stable digest of the caller's visible set —
// its size and an FNV-1a hash of the sorted project ids — and of the
// deletion generation, for the result cache key (Service.Search). Two
// requests share a cached pool only when they see exactly the same
// projects and no account was deleted in between.
//
// The deletion generation (sk-674.18 T041, SC-011; migration 000084): an
// account deletion rewrites its email and name out of the text a cached
// pool may hold in its snippets, and «after the answer the email is in no
// response» cannot wait for the TTL — nor be told to the other replicas.
// The anonymization raises the one-row counter in its own transaction, so
// every deletion retires every cached pool, on every replica, from the next
// request on, and a rolled back deletion changes nothing. Read by primary
// key: a constant cost per search.
func (s *Store) visibilityFingerprint(ctx context.Context, callerID uuid.UUID) (string, error) {
	visible, err := s.visibleSet(ctx, callerID)
	if err != nil {
		return "", err
	}
	var generation int64
	if err := s.pool.QueryRow(ctx, `SELECT generation FROM account_deletion_generation WHERE id = 1`).Scan(&generation); err != nil {
		return "", fmt.Errorf("reading the deletion generation: %w", err)
	}
	ids := slices.Clone(visible)
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	h := fnv.New64a()
	for _, id := range ids {
		_, _ = h.Write(id[:]) // hash.Hash.Write never returns an error
	}
	return fmt.Sprintf("%d:%016x:%d", len(ids), h.Sum64(), generation), nil
}

// SearchIssues runs the per-kind RRF query against the issues table.
//
// SQL strategy:
//
//  1. allowed scope subquery (admin or member-of clause)
//  2. vec_candidates CTE — top 50 by cosine similarity (skipped when
//     Embedding is empty)
//  3. fts_candidates CTE — top 50 by ts_rank sum (russian + english)
//  4. rrf_scores CTE — FULL OUTER JOIN of the two on id, summed RRF
//  5. final SELECT joins back to issues for the row content + project
//     name + ts_headline snippet
//
// The whole thing runs inside a short read-only transaction with
// `SET LOCAL hnsw.ef_search = 100` so the vector probe widens the
// candidate graph (default 40 misses some near-neighbours under the
// halfvec dimension reduction).
func (s *Store) SearchIssues(ctx context.Context, in SearchInput) ([]Item, error) {
	args := make([]any, 0, 8)
	addArg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	qRef := addArg(in.Query)
	limit := in.Limit
	if limit <= 0 {
		limit = candidateLimit
	}
	limitRef := addArg(limit)

	// vec_candidates is omitted entirely in the FTS-only fallback. The
	// rrf_scores CTE handles a missing vec column via FULL OUTER JOIN +
	// COALESCE, but it's clearer to drop the CTE altogether.
	var vecRef string
	if in.Vec != nil {
		vecRef = addArg(in.Embedding)
	}

	// Permission filter: the access relation for the caller, applied inside
	// the candidate set before ranking and LIMIT.
	visible, vErr := s.visibleSet(ctx, in.CallerID)
	if vErr != nil {
		return nil, vErr
	}
	permFrag := authz.VisibleSetExpr("i.project_id", addArg(visible))

	// Optional project narrowing.
	projectFrag := ""
	if in.Project != nil {
		projectFrag = fmt.Sprintf(" AND i.project_id = %s", addArg(*in.Project))
	}
	if in.Space != nil {
		projectFrag += fmt.Sprintf(" AND i.project_id IN (SELECT sp_p.id FROM projects sp_p WHERE sp_p.space_id = %s)", addArg(*in.Space))
	}

	// Issue-only filters (only applied when caller scoped to KindIssue).
	filterFrag := ""
	if len(in.Filters.Statuses) > 0 {
		filterFrag += fmt.Sprintf(" AND i.status = ANY(%s)", addArg(in.Filters.Statuses))
	}
	if len(in.Filters.Priorities) > 0 {
		filterFrag += fmt.Sprintf(" AND i.priority = ANY(%s)", addArg(in.Filters.Priorities))
	}
	if len(in.Filters.Types) > 0 {
		filterFrag += fmt.Sprintf(" AND i.type = ANY(%s)", addArg(in.Filters.Types))
	}
	if len(in.Filters.Labels) > 0 {
		filterFrag += fmt.Sprintf(
			` AND EXISTS (SELECT 1 FROM issue_labels il WHERE il.issue_id = i.id AND il.label = ANY(%s))`,
			addArg(in.Filters.Labels))
	}
	if in.Filters.Source != nil {
		filterFrag += fmt.Sprintf(" AND i.source = %s", addArg(*in.Filters.Source))
	}

	whereCommon := fmt.Sprintf("%s%s%s", permFrag, projectFrag, filterFrag)

	var query string
	if in.Vec != nil {
		query = fmt.Sprintf(`
WITH vec_candidates AS (
    SELECT i.id,
           ROW_NUMBER() OVER (ORDER BY i.embedding <=> %s::halfvec) AS vec_rank
      FROM issues i
     WHERE i.embedding IS NOT NULL AND %s
     ORDER BY i.embedding <=> %s::halfvec
     LIMIT %d
),
fts_candidates AS (
    SELECT i.id,
           ROW_NUMBER() OVER (
               ORDER BY ts_rank(i.tsv_ru, plainto_tsquery('russian',  %s)) +
                        ts_rank(i.tsv_en, plainto_tsquery('english', %s)) DESC
           ) AS fts_rank
      FROM issues i
     WHERE (i.tsv_ru @@ plainto_tsquery('russian',  %s)
         OR i.tsv_en @@ plainto_tsquery('english', %s))
       AND %s
     LIMIT %d
),
rrf_scores AS (
    SELECT COALESCE(v.id, f.id) AS id,
           SUM(COALESCE(1.0/(%d + v.vec_rank), 0) +
               COALESCE(1.0/(%d + f.fts_rank), 0)) AS score
      FROM vec_candidates v
      FULL OUTER JOIN fts_candidates f ON v.id = f.id
     GROUP BY COALESCE(v.id, f.id)
)
SELECT i.id, i.project_id, p.name AS project_name,
       i.title, i.status, i.priority, i.type, i.pinned, i.assignee,
       i.updated_at,
       rs.score,
       ts_headline('russian',
           coalesce(i.title,'') || E'\n' || coalesce(i.description,''),
           plainto_tsquery('russian', %s),
           'StartSel=<b>,StopSel=</b>,MaxFragments=2,MaxWords=20,MinWords=8') AS snippet_ru,
       ts_headline('english',
           coalesce(i.title,'') || E'\n' || coalesce(i.description,''),
           plainto_tsquery('english', %s),
           'StartSel=<b>,StopSel=</b>,MaxFragments=2,MaxWords=20,MinWords=8') AS snippet_en
  FROM rrf_scores rs
  JOIN issues   i ON i.id = rs.id
  JOIN projects p ON p.id = i.project_id
 ORDER BY rs.score DESC
 LIMIT %s`,
			vecRef, whereCommon, vecRef, candidateLimit,
			qRef, qRef, qRef, qRef, whereCommon, candidateLimit,
			rrfK, rrfK,
			qRef, qRef, limitRef)
	} else {
		// FTS-only fallback. Single CTE, ranked directly.
		query = fmt.Sprintf(`
WITH fts_candidates AS (
    SELECT i.id,
           ts_rank(i.tsv_ru, plainto_tsquery('russian',  %s)) +
           ts_rank(i.tsv_en, plainto_tsquery('english', %s)) AS rrf_score
      FROM issues i
     WHERE (i.tsv_ru @@ plainto_tsquery('russian',  %s)
         OR i.tsv_en @@ plainto_tsquery('english', %s))
       AND %s
     ORDER BY rrf_score DESC
     LIMIT %d
)
SELECT i.id, i.project_id, p.name,
       i.title, i.status, i.priority, i.type, i.pinned, i.assignee,
       i.updated_at,
       f.rrf_score AS score,
       ts_headline('russian',
           coalesce(i.title,'') || E'\n' || coalesce(i.description,''),
           plainto_tsquery('russian', %s),
           'StartSel=<b>,StopSel=</b>,MaxFragments=2,MaxWords=20,MinWords=8') AS snippet_ru,
       ts_headline('english',
           coalesce(i.title,'') || E'\n' || coalesce(i.description,''),
           plainto_tsquery('english', %s),
           'StartSel=<b>,StopSel=</b>,MaxFragments=2,MaxWords=20,MinWords=8') AS snippet_en
  FROM fts_candidates f
  JOIN issues   i ON i.id = f.id
  JOIN projects p ON p.id = i.project_id
 ORDER BY f.rrf_score DESC
 LIMIT %s`,
			qRef, qRef, qRef, qRef, whereCommon, candidateLimit,
			qRef, qRef, limitRef)
	}

	return s.runItemsQuery(ctx, query, args, KindIssue, in.Vec != nil)
}

// SearchComments runs the per-kind RRF query against the comments table.
// Comments don't carry project_id directly, so we JOIN issues for both
// the permission filter and the project name in the result. The CTEs use
// the JOIN inside themselves so the candidate cap (LIMIT 50) is applied
// after the permission check, not before.
func (s *Store) SearchComments(ctx context.Context, in SearchInput) ([]Item, error) {
	args := make([]any, 0, 6)
	addArg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	qRef := addArg(in.Query)
	limit := in.Limit
	if limit <= 0 {
		limit = candidateLimit
	}
	limitRef := addArg(limit)

	var vecRef string
	if in.Vec != nil {
		vecRef = addArg(in.Embedding)
	}

	visible, vErr := s.visibleSet(ctx, in.CallerID)
	if vErr != nil {
		return nil, vErr
	}
	permFrag := authz.VisibleExpr("i.project_id", "false", addArg(visible))

	projectFrag := ""
	if in.Project != nil {
		projectFrag = fmt.Sprintf(" AND i.project_id = %s", addArg(*in.Project))
	}
	if in.Space != nil {
		projectFrag += fmt.Sprintf(" AND i.project_id IN (SELECT sp_p.id FROM projects sp_p WHERE sp_p.space_id = %s)", addArg(*in.Space))
	}

	// Comment-only filter: restrict by comments.author_kind. Folded into
	// whereCommon so it lands inside BOTH candidate CTEs (vec + fts) before
	// their LIMIT — RRF must rank over the already-filtered pool, not trim a
	// mixed pool and filter afterwards. The same placeholder is reused in
	// both CTEs because whereCommon is interpolated into each. Empty slice =
	// no restriction. This never reaches SearchIssues, so issue results are
	// unaffected.
	commentFilterFrag := ""
	if len(in.Filters.CommentAuthorKinds) > 0 {
		commentFilterFrag = fmt.Sprintf(" AND c.author_kind = ANY(%s)", addArg(in.Filters.CommentAuthorKinds))
	}

	whereCommon := fmt.Sprintf("%s%s%s", permFrag, projectFrag, commentFilterFrag)

	var query string
	if in.Vec != nil {
		query = fmt.Sprintf(`
WITH vec_candidates AS (
    SELECT c.id,
           ROW_NUMBER() OVER (ORDER BY c.embedding <=> %s::halfvec) AS vec_rank
      FROM comments c
      JOIN issues   i ON i.id = c.issue_id
     WHERE c.embedding IS NOT NULL AND %s
     ORDER BY c.embedding <=> %s::halfvec
     LIMIT %d
),
fts_candidates AS (
    SELECT c.id,
           ROW_NUMBER() OVER (
               ORDER BY ts_rank(c.tsv_ru, plainto_tsquery('russian',  %s)) +
                        ts_rank(c.tsv_en, plainto_tsquery('english', %s)) DESC
           ) AS fts_rank
      FROM comments c
      JOIN issues   i ON i.id = c.issue_id
     WHERE (c.tsv_ru @@ plainto_tsquery('russian',  %s)
         OR c.tsv_en @@ plainto_tsquery('english', %s))
       AND %s
     LIMIT %d
),
rrf_scores AS (
    SELECT COALESCE(v.id, f.id) AS id,
           SUM(COALESCE(1.0/(%d + v.vec_rank), 0) +
               COALESCE(1.0/(%d + f.fts_rank), 0)) AS score
      FROM vec_candidates v
      FULL OUTER JOIN fts_candidates f ON v.id = f.id
     GROUP BY COALESCE(v.id, f.id)
)
SELECT c.id::text, i.project_id, p.name,
       c.issue_id, i.title, u.username, c.updated_at,
       rs.score,
       ts_headline('russian', coalesce(c.body,''),
           plainto_tsquery('russian', %s),
           'StartSel=<b>,StopSel=</b>,MaxFragments=2,MaxWords=20,MinWords=8') AS snippet_ru,
       ts_headline('english', coalesce(c.body,''),
           plainto_tsquery('english', %s),
           'StartSel=<b>,StopSel=</b>,MaxFragments=2,MaxWords=20,MinWords=8') AS snippet_en
  FROM rrf_scores rs
  JOIN comments c ON c.id = rs.id::uuid
  JOIN issues   i ON i.id = c.issue_id
  JOIN projects p ON p.id = i.project_id
  LEFT JOIN users u ON u.id = c.author_user_id
 ORDER BY rs.score DESC
 LIMIT %s`,
			vecRef, whereCommon, vecRef, candidateLimit,
			qRef, qRef, qRef, qRef, whereCommon, candidateLimit,
			rrfK, rrfK,
			qRef, qRef, limitRef)
	} else {
		query = fmt.Sprintf(`
WITH fts_candidates AS (
    SELECT c.id,
           ts_rank(c.tsv_ru, plainto_tsquery('russian',  %s)) +
           ts_rank(c.tsv_en, plainto_tsquery('english', %s)) AS rrf_score
      FROM comments c
      JOIN issues   i ON i.id = c.issue_id
     WHERE (c.tsv_ru @@ plainto_tsquery('russian',  %s)
         OR c.tsv_en @@ plainto_tsquery('english', %s))
       AND %s
     ORDER BY rrf_score DESC
     LIMIT %d
)
SELECT c.id::text, i.project_id, p.name,
       c.issue_id, i.title, u.username, c.updated_at,
       f.rrf_score AS score,
       ts_headline('russian', coalesce(c.body,''),
           plainto_tsquery('russian', %s),
           'StartSel=<b>,StopSel=</b>,MaxFragments=2,MaxWords=20,MinWords=8') AS snippet_ru,
       ts_headline('english', coalesce(c.body,''),
           plainto_tsquery('english', %s),
           'StartSel=<b>,StopSel=</b>,MaxFragments=2,MaxWords=20,MinWords=8') AS snippet_en
  FROM fts_candidates f
  JOIN comments c ON c.id = f.id
  JOIN issues   i ON i.id = c.issue_id
  JOIN projects p ON p.id = i.project_id
  LEFT JOIN users u ON u.id = c.author_user_id
 ORDER BY f.rrf_score DESC
 LIMIT %s`,
			qRef, qRef, qRef, qRef, whereCommon, candidateLimit,
			qRef, qRef, limitRef)
	}

	return s.runCommentsQuery(ctx, query, args, in.Vec != nil)
}

// SearchProjects: same RRF shape, simpler joins.  The "title" displayed
// in the result is the project name. project_name and project_id are
// populated for symmetry with issue/component results — the project IS
// the row.
func (s *Store) SearchProjects(ctx context.Context, in SearchInput) ([]Item, error) {
	args := make([]any, 0, 6)
	addArg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	qRef := addArg(in.Query)
	limit := in.Limit
	if limit <= 0 {
		limit = candidateLimit
	}
	limitRef := addArg(limit)

	var vecRef string
	if in.Vec != nil {
		vecRef = addArg(in.Embedding)
	}

	visible, vErr := s.visibleSet(ctx, in.CallerID)
	if vErr != nil {
		return nil, vErr
	}
	permFrag := authz.VisibleExpr("p.id", "false", addArg(visible))
	projectFrag := ""
	if in.Project != nil {
		projectFrag = fmt.Sprintf(" AND p.id = %s", addArg(*in.Project))
	}
	if in.Space != nil {
		projectFrag += fmt.Sprintf(" AND p.id IN (SELECT sp_p.id FROM projects sp_p WHERE sp_p.space_id = %s)", addArg(*in.Space))
	}
	whereCommon := fmt.Sprintf("%s%s", permFrag, projectFrag)

	var query string
	if in.Vec != nil {
		query = fmt.Sprintf(`
WITH vec_candidates AS (
    SELECT p.id,
           ROW_NUMBER() OVER (ORDER BY p.embedding <=> %s::halfvec) AS vec_rank
      FROM projects p
     WHERE p.embedding IS NOT NULL AND %s
     ORDER BY p.embedding <=> %s::halfvec
     LIMIT %d
),
fts_candidates AS (
    SELECT p.id,
           ROW_NUMBER() OVER (
               ORDER BY ts_rank(p.tsv_ru, plainto_tsquery('russian',  %s)) +
                        ts_rank(p.tsv_en, plainto_tsquery('english', %s)) DESC
           ) AS fts_rank
      FROM projects p
     WHERE (p.tsv_ru @@ plainto_tsquery('russian',  %s)
         OR p.tsv_en @@ plainto_tsquery('english', %s))
       AND %s
     LIMIT %d
),
rrf_scores AS (
    SELECT COALESCE(v.id, f.id) AS id,
           SUM(COALESCE(1.0/(%d + v.vec_rank), 0) +
               COALESCE(1.0/(%d + f.fts_rank), 0)) AS score
      FROM vec_candidates v
      FULL OUTER JOIN fts_candidates f ON v.id = f.id
     GROUP BY COALESCE(v.id, f.id)
)
SELECT p.id::text, p.name, (p.archived_at IS NOT NULL) AS archived,
       p.updated_at, rs.score,
       ts_headline('russian',
           coalesce(p.name,'') || E'\n' || coalesce(p.description,''),
           plainto_tsquery('russian', %s),
           'StartSel=<b>,StopSel=</b>,MaxFragments=2,MaxWords=20,MinWords=8') AS snippet_ru,
       ts_headline('english',
           coalesce(p.name,'') || E'\n' || coalesce(p.description,''),
           plainto_tsquery('english', %s),
           'StartSel=<b>,StopSel=</b>,MaxFragments=2,MaxWords=20,MinWords=8') AS snippet_en
  FROM rrf_scores rs
  JOIN projects p ON p.id = rs.id::uuid
 ORDER BY rs.score DESC
 LIMIT %s`,
			vecRef, whereCommon, vecRef, candidateLimit,
			qRef, qRef, qRef, qRef, whereCommon, candidateLimit,
			rrfK, rrfK, qRef, qRef, limitRef)
	} else {
		query = fmt.Sprintf(`
WITH fts_candidates AS (
    SELECT p.id,
           ts_rank(p.tsv_ru, plainto_tsquery('russian',  %s)) +
           ts_rank(p.tsv_en, plainto_tsquery('english', %s)) AS rrf_score
      FROM projects p
     WHERE (p.tsv_ru @@ plainto_tsquery('russian',  %s)
         OR p.tsv_en @@ plainto_tsquery('english', %s))
       AND %s
     ORDER BY rrf_score DESC
     LIMIT %d
)
SELECT p.id::text, p.name, (p.archived_at IS NOT NULL) AS archived,
       p.updated_at, f.rrf_score AS score,
       ts_headline('russian',
           coalesce(p.name,'') || E'\n' || coalesce(p.description,''),
           plainto_tsquery('russian', %s),
           'StartSel=<b>,StopSel=</b>,MaxFragments=2,MaxWords=20,MinWords=8') AS snippet_ru,
       ts_headline('english',
           coalesce(p.name,'') || E'\n' || coalesce(p.description,''),
           plainto_tsquery('english', %s),
           'StartSel=<b>,StopSel=</b>,MaxFragments=2,MaxWords=20,MinWords=8') AS snippet_en
  FROM fts_candidates f
  JOIN projects p ON p.id = f.id
 ORDER BY f.rrf_score DESC
 LIMIT %s`,
			qRef, qRef, qRef, qRef, whereCommon, candidateLimit,
			qRef, qRef, limitRef)
	}

	return s.runProjectsQuery(ctx, query, args, in.Vec != nil)
}

// SearchComponents: project-scoped, joins projects for the project name.
// Same query shape as SearchProjects with one extra column.
func (s *Store) SearchComponents(ctx context.Context, in SearchInput) ([]Item, error) {
	args := make([]any, 0, 6)
	addArg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	qRef := addArg(in.Query)
	limit := in.Limit
	if limit <= 0 {
		limit = candidateLimit
	}
	limitRef := addArg(limit)

	var vecRef string
	if in.Vec != nil {
		vecRef = addArg(in.Embedding)
	}

	visible, vErr := s.visibleSet(ctx, in.CallerID)
	if vErr != nil {
		return nil, vErr
	}
	permFrag := authz.VisibleExpr("c.project_id", "false", addArg(visible))
	projectFrag := ""
	if in.Project != nil {
		projectFrag = fmt.Sprintf(" AND c.project_id = %s", addArg(*in.Project))
	}
	if in.Space != nil {
		projectFrag += fmt.Sprintf(" AND c.project_id IN (SELECT sp_p.id FROM projects sp_p WHERE sp_p.space_id = %s)", addArg(*in.Space))
	}
	whereCommon := fmt.Sprintf("%s%s", permFrag, projectFrag)

	var query string
	if in.Vec != nil {
		query = fmt.Sprintf(`
WITH vec_candidates AS (
    SELECT c.id,
           ROW_NUMBER() OVER (ORDER BY c.embedding <=> %s::halfvec) AS vec_rank
      FROM components c
     WHERE c.embedding IS NOT NULL AND %s
     ORDER BY c.embedding <=> %s::halfvec
     LIMIT %d
),
fts_candidates AS (
    SELECT c.id,
           ROW_NUMBER() OVER (
               ORDER BY ts_rank(c.tsv_ru, plainto_tsquery('russian',  %s)) +
                        ts_rank(c.tsv_en, plainto_tsquery('english', %s)) DESC
           ) AS fts_rank
      FROM components c
     WHERE (c.tsv_ru @@ plainto_tsquery('russian',  %s)
         OR c.tsv_en @@ plainto_tsquery('english', %s))
       AND %s
     LIMIT %d
),
rrf_scores AS (
    SELECT COALESCE(v.id, f.id) AS id,
           SUM(COALESCE(1.0/(%d + v.vec_rank), 0) +
               COALESCE(1.0/(%d + f.fts_rank), 0)) AS score
      FROM vec_candidates v
      FULL OUTER JOIN fts_candidates f ON v.id = f.id
     GROUP BY COALESCE(v.id, f.id)
)
SELECT c.id::text, c.project_id, p.name AS project_name, c.name,
       (c.archived_at IS NOT NULL) AS component_archived,
       c.updated_at, rs.score,
       ts_headline('russian',
           coalesce(c.name,'') || E'\n' || coalesce(c.description,''),
           plainto_tsquery('russian', %s),
           'StartSel=<b>,StopSel=</b>,MaxFragments=2,MaxWords=20,MinWords=8') AS snippet_ru,
       ts_headline('english',
           coalesce(c.name,'') || E'\n' || coalesce(c.description,''),
           plainto_tsquery('english', %s),
           'StartSel=<b>,StopSel=</b>,MaxFragments=2,MaxWords=20,MinWords=8') AS snippet_en
  FROM rrf_scores rs
  JOIN components c ON c.id = rs.id::uuid
  JOIN projects   p ON p.id = c.project_id
 ORDER BY rs.score DESC
 LIMIT %s`,
			vecRef, whereCommon, vecRef, candidateLimit,
			qRef, qRef, qRef, qRef, whereCommon, candidateLimit,
			rrfK, rrfK, qRef, qRef, limitRef)
	} else {
		query = fmt.Sprintf(`
WITH fts_candidates AS (
    SELECT c.id,
           ts_rank(c.tsv_ru, plainto_tsquery('russian',  %s)) +
           ts_rank(c.tsv_en, plainto_tsquery('english', %s)) AS rrf_score
      FROM components c
     WHERE (c.tsv_ru @@ plainto_tsquery('russian',  %s)
         OR c.tsv_en @@ plainto_tsquery('english', %s))
       AND %s
     ORDER BY rrf_score DESC
     LIMIT %d
)
SELECT c.id::text, c.project_id, p.name, c.name,
       (c.archived_at IS NOT NULL) AS component_archived,
       c.updated_at, f.rrf_score AS score,
       ts_headline('russian',
           coalesce(c.name,'') || E'\n' || coalesce(c.description,''),
           plainto_tsquery('russian', %s),
           'StartSel=<b>,StopSel=</b>,MaxFragments=2,MaxWords=20,MinWords=8') AS snippet_ru,
       ts_headline('english',
           coalesce(c.name,'') || E'\n' || coalesce(c.description,''),
           plainto_tsquery('english', %s),
           'StartSel=<b>,StopSel=</b>,MaxFragments=2,MaxWords=20,MinWords=8') AS snippet_en
  FROM fts_candidates f
  JOIN components c ON c.id = f.id
  JOIN projects   p ON p.id = c.project_id
 ORDER BY f.rrf_score DESC
 LIMIT %s`,
			qRef, qRef, qRef, qRef, whereCommon, candidateLimit,
			qRef, qRef, limitRef)
	}

	return s.runComponentsQuery(ctx, query, args, in.Vec != nil)
}

// runItemsQuery executes a query that returns issue rows, scanning each
// row into an Item. The boolean useTx is true for hybrid queries (vec is
// not nil) and triggers the SET LOCAL hnsw.ef_search wrapper. FTS-only
// queries skip the transaction — they don't need the HNSW knob.
func (s *Store) runItemsQuery(
	ctx context.Context,
	query string,
	args []any,
	kind Kind,
	useTx bool,
) ([]Item, error) {
	rows, cleanup, err := s.executeQuery(ctx, query, args, useTx)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	out := make([]Item, 0, candidateLimit)
	for rows.Next() {
		var (
			id          string
			projectID   uuid.UUID
			projectName string
			title       string
			status      string
			priority    int
			issueType   string
			pinned      bool
			assignee    *string // nullable in schema (issues.assignee)
			updatedAt   time.Time
			score       float64
			snipRu      string
			snipEn      string
		)
		if err := rows.Scan(&id, &projectID, &projectName, &title,
			&status, &priority, &issueType, &pinned, &assignee,
			&updatedAt, &score, &snipRu, &snipEn); err != nil {
			return nil, fmt.Errorf("scan issue search row: %w", err)
		}

		pid := projectID.String()
		pname := projectName
		st := status
		pr := priority
		ty := issueType
		pn := pinned
		out = append(out, Item{
			Kind:        kind,
			ID:          id,
			ProjectID:   &pid,
			ProjectName: &pname,
			Title:       title,
			Snippet:     pickSnippet(snipRu, snipEn),
			Score:       score,
			UpdatedAt:   updatedAt,
			Status:      &st,
			Priority:    &pr,
			Type:        &ty,
			Pinned:      &pn,
			Assignee:    assignee, // already *string, nil when DB NULL
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iter issue search: %w", err)
	}
	return out, nil
}

func (s *Store) runCommentsQuery(
	ctx context.Context,
	query string,
	args []any,
	useTx bool,
) ([]Item, error) {
	rows, cleanup, err := s.executeQuery(ctx, query, args, useTx)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	out := make([]Item, 0, candidateLimit)
	for rows.Next() {
		var (
			id             string
			projectID      uuid.UUID
			projectName    string
			issueID        string
			issueTitle     string
			authorUsername *string // nullable: LEFT JOIN users — author_user_id may be NULL for system/agent comments
			updatedAt      time.Time
			score          float64
			snipRu         string
			snipEn         string
		)
		if err := rows.Scan(&id, &projectID, &projectName, &issueID, &issueTitle,
			&authorUsername, &updatedAt, &score, &snipRu, &snipEn); err != nil {
			return nil, fmt.Errorf("scan comment search row: %w", err)
		}
		pid := projectID.String()
		pname := projectName
		iss := issueID
		out = append(out, Item{
			Kind:           KindComment,
			ID:             id,
			ProjectID:      &pid,
			ProjectName:    &pname,
			ParentIssueID:  &iss,
			AuthorUsername: authorUsername, // already *string, nil when DB NULL
			Title:          fmt.Sprintf("Comment on %s — %s", issueID, issueTitle),
			Snippet:        pickSnippet(snipRu, snipEn),
			Score:          score,
			UpdatedAt:      updatedAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iter comment search: %w", err)
	}
	return out, nil
}

func (s *Store) runProjectsQuery(
	ctx context.Context,
	query string,
	args []any,
	useTx bool,
) ([]Item, error) {
	rows, cleanup, err := s.executeQuery(ctx, query, args, useTx)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	out := make([]Item, 0, candidateLimit)
	for rows.Next() {
		var (
			id        string
			name      string
			archived  bool
			updatedAt time.Time
			score     float64
			snipRu    string
			snipEn    string
		)
		if err := rows.Scan(&id, &name, &archived, &updatedAt, &score, &snipRu, &snipEn); err != nil {
			return nil, fmt.Errorf("scan project search row: %w", err)
		}
		pid := id
		pname := name
		ar := archived
		out = append(out, Item{
			Kind:        KindProject,
			ID:          id,
			ProjectID:   &pid,
			ProjectName: &pname,
			Title:       name,
			Snippet:     pickSnippet(snipRu, snipEn),
			Score:       score,
			UpdatedAt:   updatedAt,
			Archived:    &ar,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iter project search: %w", err)
	}
	return out, nil
}

func (s *Store) runComponentsQuery(
	ctx context.Context,
	query string,
	args []any,
	useTx bool,
) ([]Item, error) {
	rows, cleanup, err := s.executeQuery(ctx, query, args, useTx)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	out := make([]Item, 0, candidateLimit)
	for rows.Next() {
		var (
			id                string
			projectID         uuid.UUID
			projectName       string
			name              string
			componentArchived bool
			updatedAt         time.Time
			score             float64
			snipRu            string
			snipEn            string
		)
		if err := rows.Scan(&id, &projectID, &projectName, &name,
			&componentArchived, &updatedAt, &score, &snipRu, &snipEn); err != nil {
			return nil, fmt.Errorf("scan component search row: %w", err)
		}
		pid := projectID.String()
		pname := projectName
		ca := componentArchived
		out = append(out, Item{
			Kind:              KindComponent,
			ID:                id,
			ProjectID:         &pid,
			ProjectName:       &pname,
			Title:             name,
			Snippet:           pickSnippet(snipRu, snipEn),
			Score:             score,
			UpdatedAt:         updatedAt,
			ComponentArchived: &ca,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iter component search: %w", err)
	}
	return out, nil
}

// executeQuery is the shared transaction/no-tx scaffold. When useTx is
// true it opens a short read-only transaction with `SET LOCAL
// hnsw.ef_search = 100`, runs the query, and returns rows + a cleanup
// closure that commits the tx after rows.Close(). When useTx is false
// (FTS-only) it runs the query against the pool directly.
func (s *Store) executeQuery(
	ctx context.Context,
	query string,
	args []any,
	useTx bool,
) (pgx.Rows, func(), error) {
	if !useTx {
		rows, err := s.pool.Query(ctx, query, args...)
		if err != nil {
			return nil, nil, fmt.Errorf("execute search query: %w", err)
		}
		return rows, func() { rows.Close() }, nil
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, nil, fmt.Errorf("begin search tx: %w", err)
	}

	if _, err := tx.Exec(ctx, "SET LOCAL hnsw.ef_search = 100"); err != nil {
		_ = tx.Rollback(ctx)
		return nil, nil, fmt.Errorf("set hnsw.ef_search: %w", err)
	}

	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, nil, fmt.Errorf("execute search query: %w", err)
	}

	cleanup := func() {
		rows.Close()
		// Commit on a read-only tx is a cheap operation; rollback would
		// also work, but committing is the logically-correct close.
		_ = tx.Commit(ctx)
	}
	return rows, cleanup, nil
}

// pickSnippet returns the better of the two ts_headline outputs. We
// prefer the one that actually contains a `<b>` highlight tag; if
// neither does, fall back to the russian snippet (it's the first column,
// so it's stable).
func pickSnippet(ru, en string) string {
	if strings.Contains(en, "<b>") && !strings.Contains(ru, "<b>") {
		return en
	}
	if strings.Contains(ru, "<b>") {
		return ru
	}
	if ru != "" {
		return ru
	}
	return en
}

// LookupIDPrefix is the fast-path for queries that look like a typed-out
// entity ID. Returns up to 5 items (issues + projects). The caller decides
// whether to flag exactMatch based on whether exactly one item came back.
//
// We do NOT include comments/components here — components are looked up
// by name elsewhere, comments don't have human-typeable IDs.
func (s *Store) LookupIDPrefix(
	ctx context.Context,
	rawQuery string,
	callerID uuid.UUID,
	project *uuid.UUID,
	// space — the page filter space= (sk-674.18 T031): only issues and
	// projects of that space; uuid.Nil (an unknown short id) matches
	// nothing. nil — every space.
	space *uuid.UUID,
) ([]Item, error) {
	q := strings.ToLower(strings.TrimSpace(rawQuery))
	if !idPrefixPattern.MatchString(q) || len(q) < 2 {
		// One exception to the len<2 floor: a bare single-digit number
		// (e.g. `5`) is a legitimate numeric-tail query. We accept it
		// here and route to the numeric branch below.
		if !bareNumberPattern.MatchString(q) {
			return nil, nil
		}
	}

	if bareNumberPattern.MatchString(q) {
		return s.lookupByNumber(ctx, q, callerID, project, space)
	}

	args := make([]any, 0, 4)
	addArg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	// Issues match: lower(id) = q (exact) OR lower(id) LIKE q || '%' (prefix).
	likeRef := addArg(q + "%")
	exactRef := addArg(q)

	visible, vErr := s.visibleSet(ctx, callerID)
	if vErr != nil {
		return nil, vErr
	}
	issuesPerm := authz.VisibleExpr("i.project_id", "false", addArg(visible))
	issuesProjFrag := ""
	if project != nil {
		issuesProjFrag = fmt.Sprintf(" AND i.project_id = %s", addArg(*project))
	}
	if space != nil {
		issuesProjFrag += fmt.Sprintf(" AND p.space_id = %s", addArg(*space))
	}

	projectsPerm := authz.VisibleExpr("p.id", "false", addArg(visible))
	projectsProjFrag := ""
	if project != nil {
		projectsProjFrag = fmt.Sprintf(" AND p.id = %s", addArg(*project))
	}
	if space != nil {
		projectsProjFrag += fmt.Sprintf(" AND p.space_id = %s", addArg(*space))
	}

	// Each branch is wrapped in a subquery so the per-branch ORDER BY +
	// LIMIT 5 actually applies before the UNION ALL collates results.
	// A bare ORDER BY/LIMIT after UNION applies to the merged set, which
	// would silently drop one branch when the other dominated.
	//
	// The two SELECTs share a fixed column list (issue columns + project
	// `archived`); each branch fills the columns it doesn't carry with
	// typed NULLs so the UNION stays well-typed.  `exact` is 1 when the
	// caller typed the full id-code verbatim — that's tierCode in Go.
	query := fmt.Sprintf(`
SELECT * FROM (
    SELECT 'issue' AS kind, i.id, i.project_id::text AS pid, p.name AS pname,
           i.title, i.status, i.priority::int AS priority,
           i.type, i.pinned, i.assignee,
           NULL::boolean AS archived,
           i.updated_at,
           (CASE WHEN lower(i.id) = %s THEN 1 ELSE 0 END) AS exact
      FROM issues i
      JOIN projects p ON p.id = i.project_id
     WHERE (lower(i.id) = %s OR lower(i.id) LIKE %s)
       AND %s%s
     ORDER BY exact DESC, length(i.id), i.id
     LIMIT 5
) AS issues_hit
UNION ALL
SELECT * FROM (
    SELECT 'project' AS kind, p.id::text, p.id::text, p.name,
           p.name AS title, NULL::text AS status, NULL::int AS priority,
           NULL::text AS type, NULL::boolean AS pinned, NULL::text AS assignee,
           (p.archived_at IS NOT NULL) AS archived,
           p.updated_at,
           (CASE WHEN lower(p.id_prefix) = %s THEN 1 ELSE 0 END) AS exact
      FROM projects p
     WHERE (lower(p.id_prefix) = %s OR lower(p.id_prefix) LIKE %s)
       AND %s%s
     ORDER BY exact DESC, length(p.id_prefix), p.id_prefix
     LIMIT 5
) AS projects_hit`,
		exactRef, exactRef, likeRef, issuesPerm, issuesProjFrag,
		exactRef, exactRef, likeRef, projectsPerm, projectsProjFrag,
	)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("id prefix lookup: %w", err)
	}
	defer rows.Close()

	out := make([]Item, 0, 10)
	for rows.Next() {
		var (
			kind      string
			id        string
			pid       string
			pname     string
			title     string
			status    *string
			priority  *int
			issueType *string
			pinned    *bool
			assignee  *string
			archived  *bool
			updatedAt time.Time
			exact     int
		)
		if err := rows.Scan(&kind, &id, &pid, &pname, &title,
			&status, &priority, &issueType, &pinned, &assignee,
			&archived, &updatedAt, &exact); err != nil {
			return nil, fmt.Errorf("scan id prefix row: %w", err)
		}
		tier := tierFuzzy
		if exact == 1 {
			tier = tierCode
		}
		item := Item{
			Kind:        Kind(kind),
			ID:          id,
			ProjectID:   &pid,
			ProjectName: &pname,
			Title:       title,
			Snippet:     "", // ID lookups don't have snippets
			ExactMatch:  exact == 1,
			matchTier:   tier,
			UpdatedAt:   updatedAt,
		}
		// Issue-only fields are NULL on the project branch and vice
		// versa; pointer assignment keeps the JSON shape clean via
		// omitempty.
		if status != nil {
			item.Status = status
		}
		if priority != nil {
			item.Priority = priority
		}
		if issueType != nil {
			item.Type = issueType
		}
		if pinned != nil {
			item.Pinned = pinned
		}
		if assignee != nil {
			item.Assignee = assignee
		}
		if archived != nil {
			item.Archived = archived
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// LookupIDsBatch resolves a list of EXACT issue ids in a single query.
// Unlike LookupIDPrefix this does NOT do prefix matching and does NOT
// consult projects/components — it is issues-only, exact-id-only. It
// backs POST /api/v1/search/lookup-id-batch, the "paste a blob → extract
// task codes → render the tasks" surface.
//
// Matching is `lower(i.id) = ANY($1::text[])`; the caller is expected to
// have already deduped + lowercased the ids (the handler does). The same
// visibility predicate as LookupIDPrefix applies — the access relation.
// Ids that are missing OR not visible are simply absent from the result;
// order is not guaranteed (the client matches by id).
//
// The column projection mirrors the issue branch of LookupIDPrefix exactly
// so the returned Item shape is identical to /search and /search/lookup-id.
// matchTier is set to tierCode and ExactMatch to true for every row — by
// construction these are all verbatim id hits.
func (s *Store) LookupIDsBatch(
	ctx context.Context,
	ids []string,
	callerID uuid.UUID,
) ([]Item, error) {
	if len(ids) == 0 {
		return []Item{}, nil
	}

	args := make([]any, 0, 2)
	addArg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	idsRef := addArg(ids)

	// Visibility predicate identical to LookupIDPrefix's issues branch.
	visible, vErr := s.visibleSet(ctx, callerID)
	if vErr != nil {
		return nil, vErr
	}
	issuesPerm := authz.VisibleExpr("i.project_id", "false", addArg(visible))

	query := fmt.Sprintf(`
SELECT 'issue' AS kind, i.id, i.project_id::text AS pid, p.name AS pname,
       i.title, i.status, i.priority::int AS priority,
       i.type, i.pinned, i.assignee,
       NULL::boolean AS archived,
       i.updated_at
  FROM issues i
  JOIN projects p ON p.id = i.project_id
 WHERE lower(i.id) = ANY(%s::text[])
   AND %s`,
		idsRef, issuesPerm,
	)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("id batch lookup: %w", err)
	}
	defer rows.Close()

	out := make([]Item, 0, len(ids))
	for rows.Next() {
		var (
			kind      string
			id        string
			pid       string
			pname     string
			title     string
			status    *string
			priority  *int
			issueType *string
			pinned    *bool
			assignee  *string
			archived  *bool
			updatedAt time.Time
		)
		if err := rows.Scan(&kind, &id, &pid, &pname, &title,
			&status, &priority, &issueType, &pinned, &assignee,
			&archived, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan id batch row: %w", err)
		}
		item := Item{
			Kind:        Kind(kind),
			ID:          id,
			ProjectID:   &pid,
			ProjectName: &pname,
			Title:       title,
			Snippet:     "", // ID lookups don't have snippets
			ExactMatch:  true,
			matchTier:   tierCode,
			UpdatedAt:   updatedAt,
		}
		if status != nil {
			item.Status = status
		}
		if priority != nil {
			item.Priority = priority
		}
		if issueType != nil {
			item.Type = issueType
		}
		if pinned != nil {
			item.Pinned = pinned
		}
		if assignee != nil {
			item.Assignee = assignee
		}
		if archived != nil {
			item.Archived = archived
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// lookupByNumber is the numeric-tail fast-path: the caller typed a bare
// number (e.g. `15`), so we match issues whose hierarchical id ends in
// `-15` (top-level, sk-15) OR `.15` (child, sk-2.15). BOTH variants are
// returned. The match is anchored on end-of-id so `15` matches `sk-15`
// but not `sk-150`, and matches `sk-2.15` but not `sk-2.150`.
//
// `n` is digits-only (validated by bareNumberPattern at the call site),
// but we still pass it as a bind parameter and never interpolate it into
// the SQL string — the regex is assembled in PG from '[-.]' || $n || '$'
// with $n bound, so there is no path for a metacharacter to reach the
// regex engine.
//
// Projects are intentionally NOT consulted here: a project id_prefix is
// a token (`demo`), never a number tail, so a bare number can only ever
// address an issue. Scope respects the optional project filter AND
// the access relation, identically to LookupIDPrefix — nobody sees another
// project's `*-15` they cannot open.
func (s *Store) lookupByNumber(
	ctx context.Context,
	n string,
	callerID uuid.UUID,
	project *uuid.UUID,
	space *uuid.UUID,
) ([]Item, error) {
	args := make([]any, 0, 4)
	addArg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	// Anchored numeric-tail regex: id ends in `-<n>` or `.<n>`. The
	// number is bound; PG concatenates the regex literal at plan time.
	//
	// COST NOTE: the `lower(id) ~ regex` predicate is not index-eligible
	// (unlike the sibling LookupIDPrefix `LIKE q || '%'` left-anchored
	// path), so this branch seq-scans the issues the caller can see. It is
	// bounded by the visibility predicate + LIMIT 5 and fires only on a
	// pure-digit query (rare relative to text search, debounced upstream),
	// so it's acceptable at team-scale row counts. If the issues table
	// grows large, revisit with a generated trailing-segment column +
	// btree index matched as `id_tail = $n::int`.
	nRef := addArg(n)

	visible, vErr := s.visibleSet(ctx, callerID)
	if vErr != nil {
		return nil, vErr
	}
	perm := authz.VisibleExpr("i.project_id", "false", addArg(visible))
	projFrag := ""
	if project != nil {
		projFrag = fmt.Sprintf(" AND i.project_id = %s", addArg(*project))
	}
	if space != nil {
		projFrag += fmt.Sprintf(" AND p.space_id = %s", addArg(*space))
	}

	query := fmt.Sprintf(`
SELECT i.id, i.project_id::text AS pid, p.name AS pname,
       i.title, i.status, i.priority::int AS priority,
       i.type, i.pinned, i.assignee, i.updated_at
  FROM issues i
  JOIN projects p ON p.id = i.project_id
 WHERE lower(i.id) ~ ('[-.]' || %s || '$')
   AND %s%s
 ORDER BY length(i.id), i.id
 LIMIT 5`, nRef, perm, projFrag)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("id number lookup: %w", err)
	}
	defer rows.Close()

	out := make([]Item, 0, 5)
	for rows.Next() {
		var (
			id        string
			pid       string
			pname     string
			title     string
			status    string
			priority  int
			issueType string
			pinned    bool
			assignee  *string
			updatedAt time.Time
		)
		if err := rows.Scan(&id, &pid, &pname, &title, &status, &priority,
			&issueType, &pinned, &assignee, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan id number row: %w", err)
		}
		st := status
		pr := priority
		ty := issueType
		pn := pinned
		out = append(out, Item{
			Kind:        KindIssue,
			ID:          id,
			ProjectID:   &pid,
			ProjectName: &pname,
			Title:       title,
			Snippet:     "", // ID lookups don't have snippets
			// A bare-number hit is an exact id match (the user pinpointed a
			// specific issue number), so it carries exactMatch:true — but it
			// ranks BELOW a full id-code match via tierNumber < tierCode.
			ExactMatch: true,
			matchTier:  tierNumber,
			UpdatedAt:  updatedAt,
			Status:     &st,
			Priority:   &pr,
			Type:       &ty,
			Pinned:     &pn,
			Assignee:   assignee,
		})
	}
	return out, rows.Err()
}

// LooksLikeID is a Go-side gate so we don't bother the DB on every search
// query. Mirror of the SQL regex but cheaper. A bare number (e.g. `15`)
// also qualifies — it routes to the numeric-tail branch in LookupIDPrefix.
func LooksLikeID(q string) bool {
	q = strings.ToLower(strings.TrimSpace(q))
	if len(q) == 0 || len(q) > 200 {
		return false
	}
	if bareNumberPattern.MatchString(q) {
		return true
	}
	if len(q) < 2 {
		return false
	}
	return idPrefixPattern.MatchString(q)
}

// CountPending counts rows with embedding IS NULL for the given table.
// Used by the worker on every tick to publish the gauge.
func (s *Store) CountPending(ctx context.Context, table string) (int64, error) {
	q := fmt.Sprintf(`SELECT count(*) FROM %s WHERE embedding IS NULL`, table)
	var n int64
	err := s.pool.QueryRow(ctx, q).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count pending embeddings on %s: %w", table, err)
	}
	return n, nil
}

// CountPendingAll returns embedding-null counts for the four searchable
// tables in one round-trip (same gauge values as four CountPending calls).
// Keys match entitySpec.table: issues, comments, projects, components.
func (s *Store) CountPendingAll(ctx context.Context) (map[string]int64, error) {
	var issues, comments, projects, components int64
	err := s.pool.QueryRow(ctx, `
SELECT
  (SELECT count(*) FROM issues WHERE embedding IS NULL),
  (SELECT count(*) FROM comments WHERE embedding IS NULL),
  (SELECT count(*) FROM projects WHERE embedding IS NULL),
  (SELECT count(*) FROM components WHERE embedding IS NULL)`).Scan(
		&issues, &comments, &projects, &components)
	if err != nil {
		return nil, fmt.Errorf("count pending embeddings (all): %w", err)
	}
	return map[string]int64{
		"issues":     issues,
		"comments":   comments,
		"projects":   projects,
		"components": components,
	}, nil
}

// PendingRow is one entry returned by FetchPendingBatch.  The Text is
// already truncated to MaxEmbedRunes; Truncated tells the caller whether
// to bump the truncated_total counter.
type PendingRow struct {
	ID        string
	Text      string
	Truncated bool
}

// FetchPendingBatch reads up to limit rows that need embeddings from the
// given table. The composeText callback decides what columns to feed into
// the embedder for the row.  The returned rows are SELECT FOR NO KEY UPDATE
// SKIP LOCKED — multi-replica safe (see the lock-mode note below).
//
// Caller is expected to chain WriteEmbeddings inside the same tx; both
// FetchPendingBatch and WriteEmbeddings take a *pgx.Tx so the locks
// survive the embed round-trip.
//
// The query columns are passed via columns/idCol so we can reuse one
// function for issues, comments, projects, components.
func (s *Store) FetchPendingBatch(
	ctx context.Context,
	tx pgx.Tx,
	table string,
	idCol string,
	columns []string,
	composeText func(cols []any) (string, bool),
	limit int,
) ([]PendingRow, error) {
	if tx == nil {
		return nil, errors.New("FetchPendingBatch: tx is nil")
	}
	if limit <= 0 {
		limit = 32
	}
	// Lock mode (sk-465): FOR NO KEY UPDATE, not FOR UPDATE.
	//
	// The lock is held across the whole embed round-trip (processOne, up to
	// EmbedTimeout — 60s by default), so whatever it conflicts with waits for
	// the embedder. FOR UPDATE also conflicts with FOR KEY SHARE, the lock a
	// foreign-key check takes on the referenced row; every insert pointing at
	// a pending row therefore stalled behind the GPU box. Observed shape: a
	// skein_create with an inline dependency on a freshly edited issue sat in
	// INSERT INTO dependencies until the proxy cut it at 30s and the create
	// rolled back.
	//
	// NO KEY UPDATE is the exact strength the worker needs: WriteEmbeddings
	// sets only `embedding`, which is in no unique index (only the HNSW index
	// and the partial "pending" index cover it), and the only trigger on
	// these four tables (issues_stamp_assignee_set_at) writes
	// assignee_set_at, which is not a key either and fires only on an
	// assignee change the worker never makes. It is also the lock Postgres
	// itself takes for that UPDATE, and it still conflicts with itself, so
	// SKIP LOCKED keeps two replicas off the same rows.
	//
	// No longer waits for the embedder — inserts that reference a row in the
	// batch through a foreign key:
	//   - issues: dependencies (from_id, to_id), comments, issues.parent_id
	//     (a new child), child_counters, issue_components, issue_labels,
	//     issue_commits, issue_watchers, attachments, reminders;
	//   - comments: attachments.comment_id;
	//   - projects: issues.project_id (every create in the project),
	//     components, project_members, project_counters, project_aliases,
	//     project_watchers, monitor_credentials;
	//   - components: issue_components.
	//
	// BOUNDARY — still waits: an UPDATE or DELETE of a row that is in the
	// batch (skein_update / close / claim of that very issue, editing that
	// comment, …) takes FOR NO KEY UPDATE or stronger on it and conflicts
	// with this lock. Removing that wait means not holding row locks across
	// the embed call at all — a different design, not a lock-mode change.
	//
	// Pinned by TestWorkerBatchLock_DoesNotBlockReferencingInserts.
	q := fmt.Sprintf(`
SELECT %s, %s
  FROM %s
 WHERE embedding IS NULL
 ORDER BY %s
 LIMIT %d
 FOR NO KEY UPDATE SKIP LOCKED`,
		idCol, strings.Join(columns, ", "), table, idCol, limit)

	rows, err := tx.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("fetch pending %s: %w", table, err)
	}
	defer rows.Close()

	out := make([]PendingRow, 0, limit)
	for rows.Next() {
		// Allocate fresh per-row destinations.  Each column scans into
		// its own *string; we then deref into the readable []any that
		// composeText consumes.  Allocating per row avoids the classic
		// "every entry points to the same loop variable" trap.
		var id string
		colDest := make([]*string, len(columns))
		scanTargets := make([]any, len(columns)+1)
		scanTargets[0] = &id
		for i := range columns {
			colDest[i] = new(string)
			scanTargets[i+1] = colDest[i]
		}
		if err := rows.Scan(scanTargets...); err != nil {
			return nil, fmt.Errorf("scan pending %s: %w", table, err)
		}
		readable := make([]any, len(columns))
		for i := range colDest {
			readable[i] = *colDest[i]
		}
		text, truncated := composeText(readable)
		out = append(out, PendingRow{ID: id, Text: text, Truncated: truncated})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iter pending %s: %w", table, err)
	}
	return out, nil
}

// WriteEmbeddings updates each (id, embedding) inside the active tx.
// Uses pgx.Batch to pipeline the N updates over a single network round-trip;
// at EMBEDDING_BATCH_SIZE=32 this trades 32 sync round-trips for one
// batched send (≈5–10× faster), and scales linearly when the batch size
// is bumped to drain a backfill faster.
func (s *Store) WriteEmbeddings(
	ctx context.Context,
	tx pgx.Tx,
	table, idCol string,
	rows []PendingRow,
	vecs [][]float32,
) error {
	if tx == nil {
		return errors.New("WriteEmbeddings: tx is nil")
	}
	if len(rows) != len(vecs) {
		return errors.New("WriteEmbeddings: rows/vecs length mismatch")
	}
	if len(rows) == 0 {
		return nil
	}

	q := fmt.Sprintf(`UPDATE %s SET embedding = $1 WHERE %s = $2`, table, idCol)
	batch := &pgx.Batch{}
	for i, r := range rows {
		batch.Queue(q, pgvector.NewHalfVector(vecs[i]), r.ID)
	}
	br := tx.SendBatch(ctx, batch)
	defer br.Close()
	for _, r := range rows {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("update embedding %s.%s=%s: %w", table, idCol, r.ID, err)
		}
	}
	return nil
}

// spaceIDByShort resolves a space short id; uuid.Nil when no space has it
// (the filter then matches nothing — a space the caller does not see is
// filtered out by visibility anyway).
func (s *Store) spaceIDByShort(ctx context.Context, short string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT id FROM spaces WHERE short_id = $1`, short).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, nil
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("resolving space %q: %w", short, err)
	}
	return id, nil
}

// fillSpaceIDs sets SpaceID on every item that carries a project, in one
// statement (sk-674.18 T031).
func (s *Store) fillSpaceIDs(ctx context.Context, items []Item) error {
	ids := make([]string, 0, len(items))
	for _, it := range items {
		if it.ProjectID != nil {
			ids = append(ids, *it.ProjectID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := s.pool.Query(ctx, `
SELECT p.id::text, sp.short_id FROM projects p JOIN spaces sp ON sp.id = p.space_id
 WHERE p.id::text = ANY($1::text[])`, ids)
	if err != nil {
		return fmt.Errorf("reading the spaces of the hits: %w", err)
	}
	defer rows.Close()
	short := map[string]string{}
	for rows.Next() {
		var pid, sid string
		if err := rows.Scan(&pid, &sid); err != nil {
			return fmt.Errorf("scanning the spaces of the hits: %w", err)
		}
		short[pid] = sid
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range items {
		if items[i].ProjectID != nil {
			items[i].SpaceID = short[*items[i].ProjectID]
		}
	}
	return nil
}
