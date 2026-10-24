package projects

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"

	"skein-api/internal/assignee"
	"skein-api/internal/authz"
	"skein-api/internal/status"
)

// listProjectsWithSearch is the FTS+semantic path of ListProjects.
//
// Layer 1 (FTS) — runs against a single SELECT that scores each row by:
//
//   - ts_rank_cd over websearch_to_tsquery('russian', q) + the english
//     equivalent — covers natural-language stemmed matches in both
//     languages,
//   - +0.5 boost for an exact idPrefix match,
//   - +0.3 boost for idPrefix prefix match,
//   - +0.2 boost for a name ILIKE substring match,
//   - +0.4 boost for an alias match via project_aliases.
//
// Rows with zero score are filtered out via HAVING. The single query
// keeps it fast — no UNION fan-out.
//
// Layer 2 (semantic) — when the embedder is available AND layer 1
// returned fewer than searchFTSThinThreshold candidate rows, we embed
// the query and pull the top searchSemanticTopK rows whose cosine
// similarity to the query vector exceeds searchSemanticThreshold.
// Embedder failure / unavailability degrades cleanly to FTS-only —
// the layer-1 result is returned unchanged.
//
// Pagination uses a `(score, id) < (cursor_score, cursor_id)` cursor
// for the FTS path. Since semantic rerank only kicks in on the first
// page (FTS thin) and only adds new rows on top of the FTS hits, the
// downstream pages stay purely on FTS rank — predictable for the user.
//
// The total count uses the same WHERE+match clause as layer 1 (which
// also covers any semantic-only rows that the user might page into via
// the cursor — they wouldn't be in the FTS set so they don't inflate
// the count, but the row-level OR list catches them).
func (s *Store) listProjectsWithSearch(ctx context.Context, args ListProjectsArgs, q string) (*ListResult, error) {
	qLower := strings.ToLower(q)

	// Build the shared WHERE clause: permission + archived + match.
	// Match expression is the OR of (FTS hit, idPrefix variants, name
	// substring, alias join) — same predicate the COUNT(*) query
	// re-uses so layer-1 count and layer-1 result set are guaranteed
	// to align.
	var (
		whereClauses []string
		whereParams  []any
	)
	addWhereParam := func(v any) string {
		whereParams = append(whereParams, v)
		return fmt.Sprintf("$%d", len(whereParams))
	}

	// The access relation for the caller (authz); MineOnly — also an
	// explicit membership (see listProjectsAlphabetic).
	_, visible, bindErr := authz.ScopeFor(ctx, args.CallerID).Bind(ctx, s.pool)
	if bindErr != nil {
		return nil, bindErr
	}
	whereClauses = append(whereClauses, authz.VisibleExpr("p.id", addWhereParam(false), addWhereParam(visible)))
	if args.MineOnly {
		whereClauses = append(whereClauses, authz.MemberExpr("p.id", addWhereParam(args.CallerID)))
	}

	switch args.Archived {
	case "true":
		whereClauses = append(whereClauses, "p.archived_at IS NOT NULL")
	case "all":
		// no filter
	default:
		whereClauses = append(whereClauses, "p.archived_at IS NULL")
	}
	if args.Space != "" {
		whereClauses = append(whereClauses, spaceFilterExpr(addWhereParam(args.Space)))
	}

	// Match clause: row participates if ANY of these are true. The
	// match + score SQL is factored into buildProjectMatchClause /
	// buildProjectScoreExpr so the resolver path (resolve.go) shares the
	// exact same relevance definition — single source of truth.
	//
	// baseParamCount marks the boundary before the match-clause params:
	// the semantic rerank below drops the match clause from its SQL, so
	// its params have to come off too. A parameter that is sent but never
	// referenced makes Postgres fail type inference with 42P18
	// ("could not determine data type of parameter $N"). The match params
	// are the trailing entries, so truncating at this mark keeps the
	// placeholder numbering of the surviving clauses intact.
	baseParamCount := len(whereParams)
	qPh := addWhereParam(q)
	qLowerPh := addWhereParam(qLower)
	qLikePh := addWhereParam("%" + qLower + "%")
	qPrefixPh := addWhereParam(qLower + "%")
	matchClause := buildProjectMatchClause(qPh, qLowerPh, qLikePh, qPrefixPh)
	whereClauses = append(whereClauses, matchClause)

	where := "WHERE " + strings.Join(whereClauses, " AND ")

	// Build the row query. Add cursor + outbox-counts params on top of
	// the count params (count uses just the WHERE).
	rowParams := append([]any(nil), whereParams...)
	addRowParam := func(v any) string {
		rowParams = append(rowParams, v)
		return fmt.Sprintf("$%d", len(rowParams))
	}

	// sk-291 — the epics-eligible counter is scoped to the caller's horizon,
	// in lockstep with issues.EpicRollup.EligibleForClose.
	epicUnrestricted, epicCaller, epicErr := authz.FromContext(ctx).Bind(ctx, s.pool)
	if epicErr != nil {
		return nil, epicErr
	}
	// sk-423: every «blocked» leg — and its open / in_progress complement,
	// so the pills stay disjoint and sum up — counts only blockers inside
	// the caller's horizon (authz.ReaderBlockedExpr), the same horizon
	// the epics-eligible counter uses. A blocker the caller cannot see must
	// not move a count: that would tell whether it is closed.
	hU, hC := addRowParam(epicUnrestricted), addRowParam(epicCaller)

	// Score expression mirrors the match clause — same q references,
	// boosted by the strongest matching path so exact idPrefix lands
	// at the top of the list. Shared with the resolver (resolve.go).
	scoreExpr := buildProjectScoreExpr(qPh, qLowerPh, qLikePh, qPrefixPh)

	// Cursor — the K-20 order (ListCursor): a later space, or the same
	// space and a lower (score, id). The score is a derived column, so the
	// relevance half is spelled out instead of a row comparison.
	var cursorClause string
	if c := args.Cursor; c != nil {
		spaceKey := fmt.Sprintf("(%s, lower(sp.name), sp.short_id)", spaceRankExpr("sp"))
		cursorKey := fmt.Sprintf("(%s::int, %s::text, %s::text)",
			addRowParam(c.Rank), addRowParam(c.SpaceName), addRowParam(c.SpaceID))
		scorePh, idPh := addRowParam(c.Score), addRowParam(c.ID)
		cursorClause = fmt.Sprintf(
			" AND (%[1]s > %[2]s OR (%[1]s = %[2]s AND (%[3]s < %[4]s OR (%[3]s = %[4]s AND p.id < %[5]s))))",
			spaceKey, cursorKey, scoreExpr, scorePh, idPh)
	}

	limitPh := addRowParam(args.Limit + 1)

	callerIDPh := addRowParam(args.CallerID)

	sql := fmt.Sprintf(`
SELECT p.id, p.name, p.id_prefix, p.default_id_mode, p.description,
       p.archived_at, p.created_at, p.updated_at,
       -- sk-31: open/in_progress exclude derived-blocked so pills are disjoint.
       COUNT(*) FILTER (
           WHERE i.status = 'open'
             AND NOT `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS open_count,
       COUNT(*) FILTER (
           WHERE i.status = 'in_progress'
             AND NOT `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS in_progress_count,
       COUNT(*) FILTER (
           WHERE `+status.ActiveWork("i")+`
             AND `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS blocked_count,
       -- sk-118.8: standing is its own aggregate (decision #4). Unscoped,
       -- no readiness gate (standing never participates in blocked-derivation).
       COUNT(*) FILTER (WHERE i.status = 'standing') AS standing_count,
       -- sk-34: the my_outbox_* counters honor the extended Delegated
       -- predicate (creator-or-chain-member, with sticky opt-out via
       -- dismissed_by). Matches store.go::ListIssues + list_by_user.go
       -- so the project-row counter and the cross-project list always
       -- agree on what's "in my Delegated".
       COUNT(*) FILTER (
           WHERE i.status = 'open'
             AND (
               (i.created_by = %[2]s
                 AND i.assignee IS NOT NULL AND NOT `+assignee.MatchesCaller("i", "%[2]s")+`)
               OR (%[2]s = ANY(i.previous_assignees)
                 AND NOT %[2]s = ANY(i.dismissed_by)
                 AND i.assignee IS NOT NULL AND NOT `+assignee.MatchesCaller("i", "%[2]s")+`)
             )
             AND NOT `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS my_outbox_open_count,
       COUNT(*) FILTER (
           WHERE i.status = 'in_progress'
             AND (
               (i.created_by = %[2]s
                 AND i.assignee IS NOT NULL AND NOT `+assignee.MatchesCaller("i", "%[2]s")+`)
               OR (%[2]s = ANY(i.previous_assignees)
                 AND NOT %[2]s = ANY(i.dismissed_by)
                 AND i.assignee IS NOT NULL AND NOT `+assignee.MatchesCaller("i", "%[2]s")+`)
             )
             AND NOT `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS my_outbox_in_progress_count,
       COUNT(*) FILTER (
           WHERE `+status.ActiveWork("i")+`
             AND (
               (i.created_by = %[2]s
                 AND i.assignee IS NOT NULL AND NOT `+assignee.MatchesCaller("i", "%[2]s")+`)
               OR (%[2]s = ANY(i.previous_assignees)
                 AND NOT %[2]s = ANY(i.dismissed_by)
                 AND i.assignee IS NOT NULL AND NOT `+assignee.MatchesCaller("i", "%[2]s")+`)
             )
             AND `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS my_outbox_blocked_count,
       -- sk-118.8: outbox-scoped standing pill — same Delegated predicate
       -- as my_outbox_open_count, narrowed to status='standing'.
       COUNT(*) FILTER (
           WHERE i.status = 'standing'
             AND (
               (i.created_by = %[2]s
                 AND i.assignee IS NOT NULL AND NOT `+assignee.MatchesCaller("i", "%[2]s")+`)
               OR (%[2]s = ANY(i.previous_assignees)
                 AND NOT %[2]s = ANY(i.dismissed_by)
                 AND i.assignee IS NOT NULL AND NOT `+assignee.MatchesCaller("i", "%[2]s")+`)
             )
       ) AS my_outbox_standing_count,
       COUNT(*) FILTER (
           WHERE i.status = 'open'
             AND i.created_by <> %[2]s
             AND i.accepted_at IS NOT NULL
             AND `+assignee.MatchesCaller("i", "%[2]s")+`
             AND NOT `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS on_me_open_count,
       COUNT(*) FILTER (
           WHERE i.status = 'in_progress'
             AND i.created_by <> %[2]s
             AND i.accepted_at IS NOT NULL
             AND `+assignee.MatchesCaller("i", "%[2]s")+`
             AND NOT `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS on_me_in_progress_count,
       COUNT(*) FILTER (
           WHERE `+status.ActiveWork("i")+`
             AND i.created_by <> %[2]s
             AND i.accepted_at IS NOT NULL
             AND `+assignee.MatchesCaller("i", "%[2]s")+`
             AND `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS on_me_blocked_count,
       %[7]s,
       `+activeComponentsCountSQL+`,
       %[4]s AS score,
       sp.short_id, p.visibility, `+spaceRankExpr("sp")+`, lower(sp.name), sp.short_id, `+spaceOrdinalExpr("sp")+`
  FROM projects p
  JOIN spaces sp ON sp.id = p.space_id
  LEFT JOIN issues i ON i.project_id = p.id
  %[1]s%[5]s
 GROUP BY p.id, sp.id
%[6]s
 ORDER BY `+spaceRankExpr("sp")+`, lower(sp.name), sp.short_id, score DESC, p.id DESC
 LIMIT %[3]s`, where, callerIDPh, limitPh, scoreExpr, cursorClause,
		buildLensHaving(args.Lens, callerIDPh, hU, hC),
		epicsEligibleCountExpr(addRowParam(epicUnrestricted), addRowParam(epicCaller)))

	scored := make([]projectScoredRow, 0, args.Limit+1)
	if err := s.readJITOff(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, rowParams...)
		if err != nil {
			return fmt.Errorf("listing projects with search: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var r projectScoredRow
			var archivedAt *time.Time
			if err := rows.Scan(
				&r.item.ID, &r.item.Name, &r.item.IDPrefix, &r.item.DefaultIDMode, &r.item.Description,
				&archivedAt, &r.item.CreatedAt, &r.item.UpdatedAt,
				&r.item.OpenCount, &r.item.InProgressCount, &r.item.BlockedCount, &r.item.StandingCount,
				&r.item.MyOutboxOpenCount, &r.item.MyOutboxInProgressCount, &r.item.MyOutboxBlockedCount,
				&r.item.MyOutboxStandingCount,
				&r.item.OnMeOpenCount, &r.item.OnMeInProgressCount, &r.item.OnMeBlockedCount,
				&r.item.EpicsEligibleCount,
				&r.item.ComponentsCount,
				&r.score,
				&r.item.SpaceID, &r.item.Visibility, &r.key.Rank, &r.key.SpaceName, &r.key.SpaceID, &r.spaceOrd,
			); err != nil {
				return fmt.Errorf("scanning project search row: %w", err)
			}
			r.item.ArchivedAt = archivedAt
			scored = append(scored, r)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterating project search rows: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	// Optional semantic rerank — only on first page (no cursor) AND
	// when layer 1 returned thin results AND embedder is available.
	if args.Cursor == nil &&
		len(scored) < searchFTSThinThreshold &&
		args.Embedder != nil && args.Embedder.Available() {
		semantic, err := s.semanticRerank(ctx, args, q, whereClauses[:len(whereClauses)-1], whereParams[:baseParamCount])
		if err != nil {
			// Degrade quietly — semantic is best-effort. Log + carry on.
			slog.WarnContext(ctx, "project search semantic rerank failed; FTS-only result returned",
				"queryLen", len(q), "error", err.Error())
		} else {
			// Merge: any semantic row not already in `scored` joins with
			// score 0, so inside its space it sorts after every keyword
			// hit (they all score > 0) — keyword matches are more
			// trustworthy than nearest-neighbour rerank for a project
			// name index. The space order (K-20) is kept by the ordinal
			// the database computed, so the merge and the next page's
			// cursor agree with the database's collation.
			existing := make(map[uuid.UUID]bool, len(scored))
			for _, r := range scored {
				existing[r.item.ID] = true
			}
			for _, sit := range semantic {
				if existing[sit.item.ID] {
					continue
				}
				sit.score = 0
				scored = append(scored, sit)
			}
			sort.SliceStable(scored, func(i, j int) bool {
				a, b := scored[i], scored[j]
				if a.spaceOrd != b.spaceOrd {
					return a.spaceOrd < b.spaceOrd
				}
				if a.score != b.score {
					return a.score > b.score
				}
				return bytes.Compare(b.item.ID[:], a.item.ID[:]) < 0
			})
		}
	}

	hasMore := false
	if len(scored) > args.Limit {
		hasMore = true
		scored = scored[:args.Limit]
	}

	items := make([]ProjectListItem, len(scored))
	for i, r := range scored {
		items[i] = r.item
	}

	var nextCursor *string
	if hasMore && len(scored) > 0 {
		last := scored[len(scored)-1]
		key := last.key
		key.Score, key.ID = last.score, last.item.ID
		c := EncodeListCursor(key)
		nextCursor = &c
	}

	total, err := s.countProjectsWithLens(ctx, where, whereParams, args.Lens, args.CallerID)
	if err != nil {
		return nil, err
	}

	return &ListResult{Items: items, Total: total, NextCursor: nextCursor, HasMore: hasMore}, nil
}

// projectScoredRow is the internal row+score pair used by the search-
// aware list path. Kept package-private so the merge logic in
// listProjectsWithSearch can pass the slice down by value.
type projectScoredRow struct {
	item  ProjectListItem
	score float64
	// key is the row's space part of the K-20 order (ListCursor); spaceOrd
	// the space's position in that order, computed by the database.
	key      ListCursor
	spaceOrd int
}

// spaceOrdinalExpr is the position of the space of alias in the K-20 order
// among all spaces — an integer the Go merge of the search path compares
// instead of re-collating space names itself.
func spaceOrdinalExpr(alias string) string {
	return `(SELECT count(*) FROM spaces o
          WHERE (` + spaceRankExpr("o") + `, lower(o.name), o.short_id) < (` + spaceRankExpr(alias) + `, lower(` + alias + `.name), ` + alias + `.short_id))::int`
}

// semanticRerank embeds the query and runs an HNSW cosine-similarity
// scan against the projects.embedding column, filtered by the same
// permission + archived clauses as the FTS layer. The match clause is
// dropped here — semantic search exists exactly to find rows the FTS
// layer missed, so re-imposing the keyword predicate would be
// counter-productive.
//
// Returns rows with score = (1 - cosine_distance) so higher = better,
// truncated to those above searchSemanticThreshold.
func (s *Store) semanticRerank(ctx context.Context, args ListProjectsArgs, q string, baseClauses []string, baseParams []any) ([]projectScoredRow, error) {

	embedCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	vec, err := args.Embedder.EmbedSingle(embedCtx, q)
	if err != nil {
		return nil, err
	}
	if len(vec) == 0 {
		return nil, errors.New("empty embedding vector")
	}

	// Clone base params; add embedding + threshold + caller-scoping params.
	rowParams := append([]any(nil), baseParams...)
	addRowParam := func(v any) string {
		rowParams = append(rowParams, v)
		return fmt.Sprintf("$%d", len(rowParams))
	}

	// sk-291 — the epics-eligible counter is scoped to the caller's horizon,
	// in lockstep with issues.EpicRollup.EligibleForClose.
	epicUnrestricted, epicCaller, epicErr := authz.FromContext(ctx).Bind(ctx, s.pool)
	if epicErr != nil {
		return nil, epicErr
	}
	// sk-423: every «blocked» leg — and its open / in_progress complement,
	// so the pills stay disjoint and sum up — counts only blockers inside
	// the caller's horizon (authz.ReaderBlockedExpr), the same horizon
	// the epics-eligible counter uses. A blocker the caller cannot see must
	// not move a count: that would tell whether it is closed.
	hU, hC := addRowParam(epicUnrestricted), addRowParam(epicCaller)

	embedPh := addRowParam(pgvector.NewHalfVector(vec))
	thresholdPh := addRowParam(searchSemanticThreshold)
	limitPh := addRowParam(searchSemanticTopK)
	callerIDPh := addRowParam(args.CallerID)

	where := ""
	if len(baseClauses) > 0 {
		where = "WHERE " + strings.Join(baseClauses, " AND ") + " AND p.embedding IS NOT NULL"
	} else {
		where = "WHERE p.embedding IS NOT NULL"
	}

	// Two-step: candidate-ids by HNSW, then a regular GROUP BY for the
	// row + outbox counts. Doing the cosine compare directly in a
	// GROUP BY query forces Postgres to either include p.embedding in
	// GROUP BY (halfvec doesn't have an equality class) or aggregate
	// it, neither of which plays nice with the hnsw index. The CTE
	// pulls top-K ids first, then we join back for the projection.
	sql := fmt.Sprintf(`
WITH semantic_hits AS (
    SELECT p.id, (1 - (p.embedding <=> %[1]s::halfvec))::float AS similarity
      FROM projects p
      %[5]s
     ORDER BY p.embedding <=> %[1]s::halfvec
     LIMIT %[3]s
)
SELECT p.id, p.name, p.id_prefix, p.default_id_mode, p.description,
       p.archived_at, p.created_at, p.updated_at,
       -- sk-31: open/in_progress exclude derived-blocked so pills are disjoint.
       COUNT(*) FILTER (
           WHERE i.status = 'open'
             AND NOT `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS open_count,
       COUNT(*) FILTER (
           WHERE i.status = 'in_progress'
             AND NOT `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS in_progress_count,
       COUNT(*) FILTER (
           WHERE `+status.ActiveWork("i")+`
             AND `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS blocked_count,
       -- sk-118.8: standing is its own aggregate (decision #4). Unscoped,
       -- no readiness gate.
       COUNT(*) FILTER (WHERE i.status = 'standing') AS standing_count,
       -- sk-34: my_outbox_* counters honor the extended Delegated
       -- predicate. See the matching block in the FTS path above for
       -- the rationale + structure.
       COUNT(*) FILTER (
           WHERE i.status = 'open'
             AND (
               (i.created_by = %[4]s
                 AND i.assignee IS NOT NULL AND NOT `+assignee.MatchesCaller("i", "%[4]s")+`)
               OR (%[4]s = ANY(i.previous_assignees)
                 AND NOT %[4]s = ANY(i.dismissed_by)
                 AND i.assignee IS NOT NULL AND NOT `+assignee.MatchesCaller("i", "%[4]s")+`)
             )
             AND NOT `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS my_outbox_open_count,
       COUNT(*) FILTER (
           WHERE i.status = 'in_progress'
             AND (
               (i.created_by = %[4]s
                 AND i.assignee IS NOT NULL AND NOT `+assignee.MatchesCaller("i", "%[4]s")+`)
               OR (%[4]s = ANY(i.previous_assignees)
                 AND NOT %[4]s = ANY(i.dismissed_by)
                 AND i.assignee IS NOT NULL AND NOT `+assignee.MatchesCaller("i", "%[4]s")+`)
             )
             AND NOT `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS my_outbox_in_progress_count,
       COUNT(*) FILTER (
           WHERE `+status.ActiveWork("i")+`
             AND (
               (i.created_by = %[4]s
                 AND i.assignee IS NOT NULL AND NOT `+assignee.MatchesCaller("i", "%[4]s")+`)
               OR (%[4]s = ANY(i.previous_assignees)
                 AND NOT %[4]s = ANY(i.dismissed_by)
                 AND i.assignee IS NOT NULL AND NOT `+assignee.MatchesCaller("i", "%[4]s")+`)
             )
             AND `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS my_outbox_blocked_count,
       -- sk-118.8: outbox-scoped standing pill — same Delegated predicate
       -- as my_outbox_open_count, narrowed to status='standing'.
       COUNT(*) FILTER (
           WHERE i.status = 'standing'
             AND (
               (i.created_by = %[4]s
                 AND i.assignee IS NOT NULL AND NOT `+assignee.MatchesCaller("i", "%[4]s")+`)
               OR (%[4]s = ANY(i.previous_assignees)
                 AND NOT %[4]s = ANY(i.dismissed_by)
                 AND i.assignee IS NOT NULL AND NOT `+assignee.MatchesCaller("i", "%[4]s")+`)
             )
       ) AS my_outbox_standing_count,
       COUNT(*) FILTER (
           WHERE i.status = 'open'
             AND i.created_by <> %[4]s
             AND i.accepted_at IS NOT NULL
             AND `+assignee.MatchesCaller("i", "%[4]s")+`
             AND NOT `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS on_me_open_count,
       COUNT(*) FILTER (
           WHERE i.status = 'in_progress'
             AND i.created_by <> %[4]s
             AND i.accepted_at IS NOT NULL
             AND `+assignee.MatchesCaller("i", "%[4]s")+`
             AND NOT `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS on_me_in_progress_count,
       COUNT(*) FILTER (
           WHERE `+status.ActiveWork("i")+`
             AND i.created_by <> %[4]s
             AND i.accepted_at IS NOT NULL
             AND `+assignee.MatchesCaller("i", "%[4]s")+`
             AND `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS on_me_blocked_count,
       %[6]s,
       `+activeComponentsCountSQL+`,
       sh.similarity AS similarity,
       sp.short_id, p.visibility, `+spaceRankExpr("sp")+`, lower(sp.name), sp.short_id, `+spaceOrdinalExpr("sp")+`
  FROM semantic_hits sh
  JOIN projects p ON p.id = sh.id
  JOIN spaces sp ON sp.id = p.space_id
  LEFT JOIN issues i ON i.project_id = p.id
 WHERE sh.similarity >= %[2]s
 GROUP BY p.id, p.name, p.id_prefix, p.default_id_mode, p.description,
          p.archived_at, p.created_at, p.updated_at, sh.similarity, sp.id
 ORDER BY sh.similarity DESC`, embedPh, thresholdPh, limitPh, callerIDPh, where,
		epicsEligibleCountExpr(addRowParam(epicUnrestricted), addRowParam(epicCaller)))

	out := make([]projectScoredRow, 0, searchSemanticTopK)
	if err := s.readJITOff(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, rowParams...)
		if err != nil {
			return fmt.Errorf("semantic rerank query: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var r projectScoredRow
			var archivedAt *time.Time
			if err := rows.Scan(
				&r.item.ID, &r.item.Name, &r.item.IDPrefix, &r.item.DefaultIDMode, &r.item.Description,
				&archivedAt, &r.item.CreatedAt, &r.item.UpdatedAt,
				&r.item.OpenCount, &r.item.InProgressCount, &r.item.BlockedCount, &r.item.StandingCount,
				&r.item.MyOutboxOpenCount, &r.item.MyOutboxInProgressCount, &r.item.MyOutboxBlockedCount,
				&r.item.MyOutboxStandingCount,
				&r.item.OnMeOpenCount, &r.item.OnMeInProgressCount, &r.item.OnMeBlockedCount,
				&r.item.EpicsEligibleCount,
				&r.item.ComponentsCount,
				&r.score,
				&r.item.SpaceID, &r.item.Visibility, &r.key.Rank, &r.key.SpaceName, &r.key.SpaceID, &r.spaceOrd,
			); err != nil {
				return fmt.Errorf("scanning semantic rerank row: %w", err)
			}
			r.item.ArchivedAt = archivedAt
			out = append(out, r)
		}
		return rows.Err()
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// buildProjectMatchClause returns the boolean SQL fragment that decides
// whether a projects row `p` participates in a lexical search for the
// query. A row matches when ANY of these hold:
//
//   - FTS hit on tsv_ru / tsv_en via websearch_to_tsquery,
//   - exact idPrefix match,
//   - idPrefix prefix match (q%),
//   - name substring match (%q%),
//   - exact alias match in project_aliases.
//
// The four arguments are pre-bound placeholder tokens ("$3" etc.):
//
//	qPh      → the raw query (for websearch_to_tsquery)
//	qLowerPh → lower(query)            (exact idPrefix / alias compare)
//	qLikePh  → "%" + lower(query) + "%" (name substring)
//	qPrefixPh→ lower(query) + "%"      (idPrefix prefix)
//
// This is the single source of truth for project lexical matching —
// both listProjectsWithSearch (list path) and the resolver
// (resolve.go) splice the exact same predicate so the two surfaces can
// never drift apart.
func buildProjectMatchClause(qPh, qLowerPh, qLikePh, qPrefixPh string) string {
	return fmt.Sprintf(
		"(p.tsv_ru @@ websearch_to_tsquery('russian', %[1]s)"+
			" OR p.tsv_en @@ websearch_to_tsquery('english', %[1]s)"+
			" OR LOWER(p.id_prefix) = %[2]s"+
			" OR LOWER(p.id_prefix) LIKE %[4]s"+
			" OR LOWER(p.name) LIKE %[3]s"+
			" OR EXISTS (SELECT 1 FROM project_aliases pa"+
			" WHERE pa.project_id = p.id AND LOWER(pa.alias) = %[2]s))",
		qPh, qLowerPh, qLikePh, qPrefixPh,
	)
}

// buildProjectScoreExpr returns the numeric SQL fragment that scores a
// projects row `p` against the query. It mirrors buildProjectMatchClause
// exactly (same placeholder contract) so a row that matches always
// scores > 0 and a row that scores > 0 always matches. The score is
// ts_rank_cd (ru + en) plus the strongest-path boosts:
//
//	exact idPrefix   +0.5
//	idPrefix prefix  +0.3
//	name substring   +0.2
//	exact alias      +0.4
//
// Shared between the list path and the resolver — single definition.
func buildProjectScoreExpr(qPh, qLowerPh, qLikePh, qPrefixPh string) string {
	return fmt.Sprintf(
		"(COALESCE(ts_rank_cd(p.tsv_ru, websearch_to_tsquery('russian', %[1]s)), 0)"+
			" + COALESCE(ts_rank_cd(p.tsv_en, websearch_to_tsquery('english', %[1]s)), 0)"+
			" + CASE WHEN LOWER(p.id_prefix) = %[2]s THEN 0.5 ELSE 0 END"+
			" + CASE WHEN LOWER(p.id_prefix) LIKE %[4]s THEN 0.3 ELSE 0 END"+
			" + CASE WHEN LOWER(p.name) LIKE %[3]s THEN 0.2 ELSE 0 END"+
			" + CASE WHEN EXISTS (SELECT 1 FROM project_aliases pa"+
			" WHERE pa.project_id = p.id AND LOWER(pa.alias) = %[2]s) THEN 0.4 ELSE 0 END)",
		qPh, qLowerPh, qLikePh, qPrefixPh,
	)
}
