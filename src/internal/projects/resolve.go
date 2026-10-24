package projects

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/authz"
	"skein-api/internal/spaces"
)

// Resolve resolves a string naming a project among the projects the caller
// may access (sk-674.18 INV-29, SC-024), the permission filter applied at
// every layer — an inaccessible project takes part in no form and never
// changes the answer.
//
// Exact paths first, in the order id → id_prefix → alias → name: the id,
// the prefix and the alias are unique on the installation, so a hit on one
// of them wins even if the string is also another project's name; the name
// is unique only inside a space — one accessible hit wins, two or more are
// the FINAL refusal «ambiguous» (Ambiguous, the candidates their projects)
// that no later form may bypass.
//
// Without an exact hit, the kept forms, as before: the repo URL (only when q
// looks like a URL), then lexical relevance (sk-60) — the SAME match/score
// logic the project-list search uses — with the confident auto-pick of a
// single candidate.
//
// Empty results are not 404 — return {match: nil, candidates: []}.
func (s *Store) Resolve(ctx context.Context, q string, callerID uuid.UUID) (*ResolveResult, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return &ResolveResult{Candidates: []ResolveMatch{}}, nil
	}
	qLower := strings.ToLower(q)

	if hits, err := s.resolveExactPaths(ctx, q, qLower, callerID); err != nil {
		return nil, err
	} else if len(hits) == 1 {
		return &ResolveResult{Match: &hits[0], Candidates: []ResolveMatch{}}, nil
	} else if len(hits) > 1 {
		return &ResolveResult{Candidates: hits, Ambiguous: true}, nil
	}

	// Repo URL exact (only when q looks URL-ish). Runs before the lexical
	// step so an exact repo URL still auto-matches.
	if IsURLLike(q) {
		if m, err := s.resolveRepoURL(ctx, q, callerID); err != nil {
			return nil, err
		} else if m != nil {
			return &ResolveResult{Match: m, Candidates: []ResolveMatch{}}, nil
		}
	}

	// Lexical relevance — reuse the project-list search match/score logic
	// (FTS + id_prefix exact/prefix + name substring + exact alias).
	candidates, err := s.resolveLexical(ctx, q, callerID)
	if err != nil {
		return nil, err
	}

	// Auto-match rule: exactly one candidate at high relevance score.
	if len(candidates) == 1 && candidates[0].Similarity >= resolveAutoMatchThreshold {
		m := candidates[0]
		return &ResolveResult{Match: &m, Candidates: []ResolveMatch{}}, nil
	}

	if candidates == nil {
		candidates = []ResolveMatch{}
	}
	return &ResolveResult{Match: nil, Candidates: candidates}, nil
}

// AmbiguousCandidates is the `candidates` of spaces.CodeAmbiguousProject
// for matches — the same body in REST and MCP (contract T013): enough to
// tell same-named projects apart and re-call with an exact handle.
func AmbiguousCandidates(matches []ResolveMatch) []spaces.AmbiguousProjectCandidate {
	out := make([]spaces.AmbiguousProjectCandidate, 0, len(matches))
	for _, m := range matches {
		out = append(out, spaces.AmbiguousProjectCandidate{
			IDPrefix: m.IDPrefix, Name: m.Name, SpaceID: m.SpaceID, SpaceName: m.SpaceName,
		})
	}
	return out
}

// resolveSpaceCols are the space columns of a resolve row of alias p.
const resolveSpaceCols = `(SELECT sp.short_id FROM spaces sp WHERE sp.id = p.space_id),
       (SELECT sp.name FROM spaces sp WHERE sp.id = p.space_id)`

// resolveExactPaths evaluates the exact paths in one round-trip and returns
// the hits of the first path that hits, in INV-29 order: id (1) →
// id_prefix (2) → alias (3) → name (4). The first three are unique, so
// they hit at most one project; the name may hit several accessible ones
// (different spaces) — all are returned, ordered by prefix.
func (s *Store) resolveExactPaths(ctx context.Context, q, qLower string, callerID uuid.UUID) ([]ResolveMatch, error) {
	_, visible, bindErr := authz.ScopeFor(ctx, callerID).Bind(ctx, s.pool)
	if bindErr != nil {
		return nil, bindErr
	}
	var asID *uuid.UUID
	if id, err := uuid.Parse(q); err == nil {
		asID = &id
	}
	// The access relation for the caller (authz): its visible set is $2.
	perm := authz.VisibleExpr("p.id", "false", "$2")
	rows, err := s.pool.Query(ctx, `
WITH exact AS (
  SELECT p.id, 'id'::text AS matched_via, 1 AS prio
    FROM projects p
   WHERE $3::uuid IS NOT NULL AND p.id = $3::uuid AND `+perm+`
  UNION ALL
  SELECT p.id, 'id_prefix', 2
    FROM projects p
   WHERE lower(p.id_prefix) = $1 AND `+perm+`
  UNION ALL
  SELECT p.id, 'alias', 3
    FROM projects p
    JOIN project_aliases a ON a.project_id = p.id
   WHERE a.alias = $1 AND `+perm+`
  UNION ALL
  SELECT p.id, 'name', 4
    FROM projects p
   WHERE lower(p.name) = $1 AND `+perm+`
), first AS (
  SELECT min(prio) AS prio FROM exact
)
SELECT p.id, p.name, p.id_prefix, e.matched_via, `+resolveSpaceCols+`
  FROM exact e
  JOIN first f ON f.prio = e.prio
  JOIN projects p ON p.id = e.id
 ORDER BY lower(p.id_prefix), p.id`, qLower, visible, asID)
	if err != nil {
		return nil, fmt.Errorf("resolving exact paths: %w", err)
	}
	defer rows.Close()
	var out []ResolveMatch
	for rows.Next() {
		var m ResolveMatch
		if err := rows.Scan(&m.ID, &m.Name, &m.IDPrefix, &m.MatchedVia, &m.SpaceID, &m.SpaceName); err != nil {
			return nil, fmt.Errorf("scanning exact resolve row: %w", err)
		}
		m.Similarity = 1.0
		out = append(out, m)
	}
	return out, rows.Err()
}

// resolveLexical gathers candidate projects using the SAME lexical
// match/score definition as the project-list search path
// (buildProjectMatchClause / buildProjectScoreExpr in list_search.go) —
// single source of truth, so the resolver and the list can never drift.
//
// A row participates when its FTS (tsv_ru/tsv_en via
// websearch_to_tsquery) OR id_prefix exact/prefix OR name substring OR
// exact alias matches; the score boosts the strongest path so an exact
// id_prefix lands ahead of a bare substring. Results are ordered by
// score desc and capped at resolveMaxCandidates.
//
// The admin-OR-membership permission predicate is applied as the first
// WHERE clause so invisible projects never leak into candidates — a hard
// security requirement preserved from the previous trigram step.
//
// matched_via is reported as "search" (the lexical-relevance path);
// exact paths above this step keep their precise via labels.
func (s *Store) resolveLexical(ctx context.Context, q string, callerID uuid.UUID) ([]ResolveMatch, error) {
	qLower := strings.ToLower(q)

	// Accumulate positional params in the same order the placeholder
	// tokens are minted — mirrors the addParam pattern in list_search.go.
	var params []any
	addParam := func(v any) string {
		params = append(params, v)
		return fmt.Sprintf("$%d", len(params))
	}

	_, visible, bindErr := authz.ScopeFor(ctx, callerID).Bind(ctx, s.pool)
	if bindErr != nil {
		return nil, bindErr
	}
	// Permission filter: the access relation for the caller (authz).
	permClause := authz.VisibleExpr("p.id", "false", addParam(visible))

	// Lexical match + score — shared with the list path.
	qPh := addParam(q)
	qLowerPh := addParam(qLower)
	qLikePh := addParam("%" + qLower + "%")
	qPrefixPh := addParam(qLower + "%")
	matchClause := buildProjectMatchClause(qPh, qLowerPh, qLikePh, qPrefixPh)
	scoreExpr := buildProjectScoreExpr(qPh, qLowerPh, qLikePh, qPrefixPh)

	limitPh := addParam(resolveMaxCandidates)

	sql := fmt.Sprintf(`
SELECT p.id, p.name, p.id_prefix, %[3]s AS score, `+resolveSpaceCols+`
  FROM projects p
 WHERE %[1]s
   AND %[2]s
 ORDER BY score DESC, p.id DESC
 LIMIT %[4]s`, permClause, matchClause, scoreExpr, limitPh)

	rows, err := s.pool.Query(ctx, sql, params...)
	if err != nil {
		return nil, fmt.Errorf("lexical resolve: %w", err)
	}
	defer rows.Close()

	out := make([]ResolveMatch, 0, resolveMaxCandidates)
	for rows.Next() {
		var m ResolveMatch
		var score float64
		if err := rows.Scan(&m.ID, &m.Name, &m.IDPrefix, &score, &m.SpaceID, &m.SpaceName); err != nil {
			return nil, fmt.Errorf("scanning lexical resolve row: %w", err)
		}
		m.Similarity = score
		m.MatchedVia = "search"
		out = append(out, m)
	}
	return out, rows.Err()
}

// resolveRepoURL scans projects.git_repos[] for an exact URL match,
// scoped to projects visible to caller.
//
// Limitation (until R5.2): comparison is literal string equality on
// git_repos[].url — different forms of the same remote (https vs ssh,
// trailing .git, user@host vs host:path, case) do NOT match. Callers
// should pass the URL exactly as stored on the project. Normalization
// is a separate R5.2 task, not silently patched here.
func (s *Store) resolveRepoURL(ctx context.Context, url string, callerID uuid.UUID) (*ResolveMatch, error) {
	_, visible, bindErr := authz.ScopeFor(ctx, callerID).Bind(ctx, s.pool)
	if bindErr != nil {
		return nil, bindErr
	}
	var m ResolveMatch
	row := s.pool.QueryRow(ctx, `
SELECT p.id, p.name, p.id_prefix, `+resolveSpaceCols+`
  FROM projects p
 WHERE jsonb_path_exists(p.git_repos,
        '$[*] ? (@.url == $u)',
        jsonb_build_object('u', $1::text))
   AND `+authz.VisibleExpr("p.id", "false", "$2")+`
 LIMIT 1`,
		url, visible)
	err := row.Scan(&m.ID, &m.Name, &m.IDPrefix, &m.SpaceID, &m.SpaceName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("repo url resolve: %w", err)
	}
	m.Similarity = 1.0
	m.MatchedVia = "repo"
	return &m, nil
}
