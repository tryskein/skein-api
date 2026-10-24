package issues

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"

	"skein-api/internal/assignee"
	"skein-api/internal/authz"
)

// QueryEmbedder is the minimal embedder interface for the optional
// semantic rerank on the cross-project /issues search path. Mirrors the
// shape used by `internal/projects` so a single *search.Embedder
// instance satisfies both via structural typing — kept here so the
// `issues` package doesn't have to import `projects` or `search`.
type QueryEmbedder interface {
	EmbedSingle(ctx context.Context, text string) ([]float32, error)
	Available() bool
}

// listAllSemanticThreshold mirrors the projects-side cosine-similarity
// floor: rows below this don't participate in the semantic merge.
const listAllSemanticThreshold = 0.6

// listAllSemanticTopK caps the number of semantic candidates the rerank
// pulls when the FTS layer came back thin.
const listAllSemanticTopK = 20

// listAllFTSThinThreshold is the FTS-result count below which we trigger
// the optional semantic rerank. If FTS yielded ≥ N hits we skip the
// embedder round-trip.
const listAllFTSThinThreshold = 20

// issueIDLooseRe matches strings that *look* like full issue ids of the
// form `prefix-N[.N…]` (e.g. `demo-2`, `demo-2.5`). Used to boost exact-
// id matches to the top of the FTS result set when the user types an
// issue id into the search box.
var issueIDLooseRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*-\d+(\.\d+)*$`)

// ListAllIssuesArgs is the filter set for the cross-project list path
// (GET /api/v1/issues). All filters mirror the project-scoped
// ListIssuesArgs semantics where they overlap; new knobs (Q for FTS,
// SortBy, Caller / Role for visibility) are scoped to this surface.
//
// Visibility predicate: the access relation (authz) for Scope — an issue
// is listed only if its project is in the scope's visible set.
type ListAllIssuesArgs struct {
	// Space narrows the list to one space's projects (its short id); ""
	// — every space (sk-674.18 T032).
	Space string
	// Caller is the resolved caller's user id. Required — drives the
	// visibility predicate, the iAssigned column, and the "assignee=me"
	// / "createdBy=me" resolution.
	Caller uuid.UUID
	// CallerUsername is the caller's canonical username (lowercased at
	// the handler boundary). Empty is allowed — the assignee=me resolve
	// path goes through the user-id (ua.id) JOIN, not the username.
	//
	// Where it is still consulted (the sk-34 Delegated chain) it GATES the
	// predicate; the handle comparison itself runs through
	// assignee.MatchesCaller on the caller's id (sk-523).
	CallerUsername string
	// Scope is the caller's horizon (authz.FromContext): the list holds
	// only issues of projects the access relation grants it. Required —
	// the zero Scope sees nothing.
	Scope authz.Scope

	// Q is the search box value — searched against the issue's stored
	// tsv_ru / tsv_en generated columns (title + description +
	// acceptance_criteria + design + notes; see migration 000006).
	// When Q looks like a full issue id (`prefix-N[.N…]`) we additionally
	// boost the row whose id matches it exactly so typing `demo-2.5`
	// jumps the right row to the top.
	Q string

	// Status is a single status filter ("open" | "in_progress" | ...).
	// Empty means "no filter" — same default behaviour as ListByUser
	// (caller sees every status including closed; the UI passes a
	// concrete status when it wants the open-only Inbox/board scope).
	Status string
	// Type is a single type filter ("bug" | "feature" | ...). Empty =
	// no filter.
	Type string
	// OriginGroup maps the wire-level origin filter to a set of
	// issues.source values, identical semantics to ListIssuesArgs.
	// Pointer so nil = no filter.
	OriginGroup *string

	// AssigneeUserID resolves "assignee=me" without forcing the caller
	// to know the assignee's username. Mutually exclusive with Assignee.
	AssigneeUserID *uuid.UUID
	// Assignee is the free-form username equality filter (resolves to
	// `i.assignee = $1`). Mutually exclusive with AssigneeUserID.
	Assignee *string
	// CreatedBy filters on issues.created_by. Powers the cross-project
	// Outbox surface ("tasks I originated"); the handler restricts
	// wire values to "me" → caller.UserID.
	CreatedBy *uuid.UUID
	// CreatedByExclude is the symmetric inverse of CreatedBy. Powers the
	// "On me" cross-project lens: assignee=me AND created_by != me.
	CreatedByExclude *uuid.UUID
	// AcceptedAtNull, when non-nil and true, restricts the result to
	// `accepted_at IS NULL`.
	AcceptedAtNull *bool
	// AcceptedAtNotNull is the symmetric inverse — restricts to
	// `accepted_at IS NOT NULL`. Powers the "On me" lens prefix.
	AcceptedAtNotNull *bool

	// IsBlocked filters by the derived blocked-ness flag (sk-31). Nil
	// = no filter; same EXISTS predicate as the SELECT-list column.
	IsBlocked *bool

	Cursor *string
	Limit  int
	// SortBy is the row order — "updated" (default) | "created". Search
	// (Q != "") always orders by FTS score first, with the SortBy column
	// as tiebreaker so search results stay stable across pages.
	SortBy string

	// Embedder is the optional cosine-similarity embedder for the
	// semantic rerank layer. nil falls back to FTS-only. Available()
	// short-circuits when the embedder is up but Ollama is down.
	Embedder QueryEmbedder
}

// ListAllIssues runs the cross-project issues query. Visibility is the
// caller's project_members horizon (admin: all). Pagination is keyset
// on (updated_at DESC, id DESC) by default, or (score DESC, id DESC)
// when Q is non-empty.
//
// Two-phase when Q != "":
//
//  1. FTS layer over tsv_ru / tsv_en with ts_rank_cd scoring + alias-
//     style boost for exact id match. Cursor pagination uses the (score,
//     id) pair packed through the existing time+id cursor codec.
//  2. Optional semantic rerank — same trigger as the projects path
//     (first page only, FTS thin, embedder available). Failure degrades
//     to FTS-only.
//
// Total is exact: SELECT COUNT(*) with the same WHERE clause as the row
// query (sans cursor). Cheap on typical visibility horizons (5-50
// projects, a few thousand issues) — revisit when datasets push past
// ~1M visible rows.
func (s *Store) ListAllIssues(ctx context.Context, args ListAllIssuesArgs) (*ListResult, error) {
	if args.Limit < 1 {
		args.Limit = ListIssuesDefaultLimit
	}
	if args.Limit > ListIssuesMaxLimit {
		args.Limit = ListIssuesMaxLimit
	}

	q := strings.TrimSpace(args.Q)
	if q != "" {
		return s.listAllIssuesWithSearch(ctx, args, q)
	}
	return s.listAllIssuesPlain(ctx, args)
}

// listAllIssuesPlain is the no-query path: keyset-paginated by
// (sort column DESC, id DESC).
func (s *Store) listAllIssuesPlain(ctx context.Context, args ListAllIssuesArgs) (*ListResult, error) {
	h, hErr := s.horizon(ctx)
	if hErr != nil {
		return nil, hErr
	}
	scopeU, scopeV, shErr := args.Scope.Bind(ctx, s.pool)
	if shErr != nil {
		return nil, shErr
	}
	sh := readerHorizon{unrestricted: scopeU, visible: scopeV}
	sortColumn := "updated_at"
	if args.SortBy == "created" {
		sortColumn = "created_at"
	}

	whereClauses, whereParams := buildAllIssuesWhere(h, sh, args)
	rowParams := append([]any(nil), whereParams...)
	addRowParam := func(v any) string {
		rowParams = append(rowParams, v)
		return fmt.Sprintf("$%d", len(rowParams))
	}

	// Snapshot the where-without-cursor clauses for the COUNT query
	// before adding the cursor clause (count is unfiltered by cursor).
	countClauses := append([]string(nil), whereClauses...)
	countParams := append([]any(nil), whereParams...)

	if args.Cursor != nil && *args.Cursor != "" {
		t, idStr, err := DecodeCursor(*args.Cursor)
		if err != nil {
			return nil, fmt.Errorf("decoding cursor: %w", err)
		}
		whereClauses = append(whereClauses, fmt.Sprintf(
			"(i.%s, i.id) < (%s, %s)",
			sortColumn, addRowParam(t), addRowParam(idStr),
		))
	}

	limitPh := addRowParam(args.Limit + 1)

	callerPh := addRowParam(args.Caller)
	iAssignedExpr := fmt.Sprintf(
		"(i.created_by = %s AND ua.id IS NOT NULL AND ua.id <> %s)",
		callerPh, callerPh)

	// sk-291 — epic progress is scoped to the caller's horizon.
	epicUnrestricted, epicCaller := h.unrestricted, h.visible
	epicProgressCols := epicProgressProjection(addRowParam(epicUnrestricted), addRowParam(epicCaller), addRowParam(h.spaces))
	parentCol := parentIDForReader(h, addRowParam)

	// Admin caller with zero other filters produces an empty clauses
	// list; collapse to a constant-true predicate so the SQL stays
	// syntactically valid (`WHERE TRUE` rather than `WHERE  ORDER BY`).
	whereSQL := "TRUE"
	if len(whereClauses) > 0 {
		whereSQL = strings.Join(whereClauses, " AND ")
	}

	sql := fmt.Sprintf(`
SELECT i.id, i.project_id, `+parentCol+`, i.version, i.title,
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
 LIMIT %s`, iAssignedExpr, readerBlockedBinder(h, addRowParam)(), epicProgressCols,
		whereSQL, sortColumn, limitPh)

	items, err := s.queryIssueSummaries(ctx, sql, rowParams, args.Limit+1)
	if err != nil {
		return nil, err
	}

	hasMore := false
	if len(items) > args.Limit {
		hasMore = true
		items = items[:args.Limit]
	}

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

	total, err := s.countAllIssues(ctx, countClauses, countParams)
	if err != nil {
		return nil, err
	}

	return &ListResult{Items: items, Total: total, NextCursor: nextCursor, HasMore: hasMore}, nil
}

// listAllIssuesWithSearch is the FTS+semantic path. Score expression
// matches the projects/list_search.go shape — ts_rank_cd over both
// language tsvectors plus an exact-id boost when Q looks like a full
// issue id.
func (s *Store) listAllIssuesWithSearch(ctx context.Context, args ListAllIssuesArgs, q string) (*ListResult, error) {
	h, hErr := s.horizon(ctx)
	if hErr != nil {
		return nil, hErr
	}
	scopeU, scopeV, shErr := args.Scope.Bind(ctx, s.pool)
	if shErr != nil {
		return nil, shErr
	}
	sh := readerHorizon{unrestricted: scopeU, visible: scopeV}
	whereClauses, whereParams := buildAllIssuesWhere(h, sh, args)

	addWhereParam := func(v any) string {
		whereParams = append(whereParams, v)
		return fmt.Sprintf("$%d", len(whereParams))
	}

	// Match clause: row participates if either tsvector matches OR the
	// id is an exact match for the typed-out issue id. qLowerPh only
	// gets registered when exactIDMatch is true — an unreferenced
	// parameter would trigger "could not determine data type of
	// parameter" on the planner side.
	qPh := addWhereParam(q)
	qLower := strings.ToLower(q)
	exactIDMatch := issueIDLooseRe.MatchString(qLower)
	var qLowerPh string
	if exactIDMatch {
		qLowerPh = addWhereParam(qLower)
	}

	matchClauses := []string{
		fmt.Sprintf("i.tsv_ru @@ websearch_to_tsquery('russian', %s)", qPh),
		fmt.Sprintf("i.tsv_en @@ websearch_to_tsquery('english', %s)", qPh),
	}
	if exactIDMatch {
		matchClauses = append(matchClauses, fmt.Sprintf("LOWER(i.id) = %s", qLowerPh))
	}
	matchClause := "(" + strings.Join(matchClauses, " OR ") + ")"
	whereClauses = append(whereClauses, matchClause)

	// Snapshot for COUNT before the cursor clause is appended.
	countClauses := append([]string(nil), whereClauses...)
	countParams := append([]any(nil), whereParams...)

	// Score expression mirrors the projects list_search.go shape: sum
	// of both language ts_rank_cd values plus a fixed boost for an
	// exact id match. ts_rank_cd cannot range across a non-FTS row
	// (when Q doesn't match either tsvector but i.id == q does), so
	// the COALESCE keeps the score finite in that branch.
	scoreParts := []string{
		fmt.Sprintf("COALESCE(ts_rank_cd(i.tsv_ru, websearch_to_tsquery('russian', %s)), 0)", qPh),
		fmt.Sprintf("COALESCE(ts_rank_cd(i.tsv_en, websearch_to_tsquery('english', %s)), 0)", qPh),
	}
	if exactIDMatch {
		scoreParts = append(scoreParts, fmt.Sprintf(
			"CASE WHEN LOWER(i.id) = %s THEN 1.0 ELSE 0 END", qLowerPh,
		))
	}
	scoreExpr := "(" + strings.Join(scoreParts, " + ") + ")"

	rowParams := append([]any(nil), whereParams...)
	addRowParam := func(v any) string {
		rowParams = append(rowParams, v)
		return fmt.Sprintf("$%d", len(rowParams))
	}

	// Cursor pagination on (score, id). We pack the score into a
	// synthetic time.Time so EncodeCursor / DecodeCursor keep working
	// without a schema change — identical trick to projects/list_search.go.
	var cursorClause string
	if args.Cursor != nil && *args.Cursor != "" {
		t, idStr, err := DecodeCursor(*args.Cursor)
		if err != nil {
			return nil, fmt.Errorf("decoding cursor: %w", err)
		}
		score := scoreFromCursorTime(t)
		scorePh := addRowParam(score)
		idPh := addRowParam(idStr)
		cursorClause = fmt.Sprintf(" AND (%[1]s < %[2]s OR (%[1]s = %[2]s AND i.id < %[3]s))",
			scoreExpr, scorePh, idPh,
		)
	}

	limitPh := addRowParam(args.Limit + 1)

	callerPh := addRowParam(args.Caller)
	iAssignedExpr := fmt.Sprintf(
		"(i.created_by = %s AND ua.id IS NOT NULL AND ua.id <> %s)",
		callerPh, callerPh)

	// sk-291 — epic progress is scoped to the caller's horizon.
	epicUnrestricted, epicCaller := h.unrestricted, h.visible
	epicProgressCols := epicProgressProjection(addRowParam(epicUnrestricted), addRowParam(epicCaller), addRowParam(h.spaces))
	parentCol := parentIDForReader(h, addRowParam)

	sql := fmt.Sprintf(`
SELECT i.id, i.project_id, `+parentCol+`, i.version, i.title,
       i.type, i.priority, i.status, i.pinned, i.assignee,
       i.created_by, uc.username, ua.id, `+issueSummaryPersonCols+`,
       i.created_at, i.updated_at, i.row_seq,
       `+rowComponentsLabelsCols+`,
       i.accepted_at, i.close_outcome,
       %[1]s AS i_assigned,
       %[6]s AS is_blocked,
       %[7]s,
       %[2]s AS score
  FROM issues i
  LEFT JOIN users uc ON uc.id = i.created_by
  `+assignee.LeftJoin("ua", "i")+`
 WHERE %[3]s%[4]s
 ORDER BY score DESC, i.id DESC
 LIMIT %[5]s`,
		iAssignedExpr, scoreExpr,
		strings.Join(whereClauses, " AND "),
		cursorClause, limitPh, readerBlockedBinder(h, addRowParam)(),
		epicProgressCols)

	type scoredRow struct {
		item  IssueSummary
		score float64
	}
	rows, err := s.pool.Query(ctx, sql, rowParams...)
	if err != nil {
		return nil, fmt.Errorf("listing all issues with search: %w", err)
	}
	defer rows.Close()
	scored := make([]scoredRow, 0, args.Limit+1)
	for rows.Next() {
		var score float64
		item, err := scanIssueSummaryRow(rows, &score)
		if err != nil {
			return nil, fmt.Errorf("scanning all-issues row: %w", err)
		}
		scored = append(scored, scoredRow{item: item, score: score})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating list-all search rows: %w", err)
	}

	// Optional semantic rerank — only on first page (no cursor) AND
	// when layer 1 returned thin results AND embedder is available.
	if args.Cursor == nil && len(scored) < listAllFTSThinThreshold &&
		args.Embedder != nil && args.Embedder.Available() {
		// Strip the FTS match clause from the where (last entry) for
		// the semantic search — the whole point is to find rows the
		// FTS layer missed. The trailing params we registered for the
		// match clause (qPh always; qLowerPh only when exactIDMatch)
		// also have to come off so the parameter indices in the
		// remaining clauses still align.
		baseClauses := whereClauses[:len(whereClauses)-1]
		matchParamCount := 1
		if exactIDMatch {
			matchParamCount = 2
		}
		baseParams := whereParams[:len(whereParams)-matchParamCount]
		semantic, semErr := s.semanticRerankAllIssues(ctx, args, q, baseClauses, baseParams)
		if semErr != nil {
			slog.WarnContext(ctx, "all-issues semantic rerank failed; FTS-only result returned",
				"queryLen", len(q), "error", semErr.Error())
		} else {
			existing := make(map[string]bool, len(scored))
			for _, r := range scored {
				existing[r.item.ID] = true
			}
			for _, sit := range semantic {
				if existing[sit.item.ID] {
					continue
				}
				scored = append(scored, sit)
			}
		}
	}

	hasMore := false
	if len(scored) > args.Limit {
		hasMore = true
		scored = scored[:args.Limit]
	}

	items := make([]IssueSummary, len(scored))
	for i, r := range scored {
		items[i] = r.item
	}

	var nextCursor *string
	if hasMore && len(scored) > 0 {
		last := scored[len(scored)-1]
		c := EncodeCursor(scoreToCursorTime(last.score), last.item.ID)
		nextCursor = &c
	}

	total, err := s.countAllIssues(ctx, countClauses, countParams)
	if err != nil {
		return nil, err
	}

	return &ListResult{Items: items, Total: total, NextCursor: nextCursor, HasMore: hasMore}, nil
}

// semanticRerankAllIssues embeds the query and runs an HNSW cosine
// scan against issues.embedding, filtered by the same visibility +
// status/type/origin clauses as the FTS layer. The match clause is
// dropped — semantic search exists to find rows the FTS layer missed.
//
// Returns rows scored by (1 - cosine_distance) above
// listAllSemanticThreshold.
func (s *Store) semanticRerankAllIssues(
	ctx context.Context,
	args ListAllIssuesArgs,
	q string,
	baseClauses []string,
	baseParams []any,
) ([]struct {
	item  IssueSummary
	score float64
}, error) {
	h, hErr := s.horizon(ctx)
	if hErr != nil {
		return nil, hErr
	}
	embedCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	vec, err := args.Embedder.EmbedSingle(embedCtx, q)
	if err != nil {
		return nil, err
	}
	if len(vec) == 0 {
		return nil, errors.New("empty embedding vector")
	}

	rowParams := append([]any(nil), baseParams...)
	addRowParam := func(v any) string {
		rowParams = append(rowParams, v)
		return fmt.Sprintf("$%d", len(rowParams))
	}

	embedPh := addRowParam(pgvector.NewHalfVector(vec))
	thresholdPh := addRowParam(listAllSemanticThreshold)
	limitPh := addRowParam(listAllSemanticTopK)

	callerPh := addRowParam(args.Caller)
	iAssignedExpr := fmt.Sprintf(
		"(i.created_by = %s AND ua.id IS NOT NULL AND ua.id <> %s)",
		callerPh, callerPh)

	// sk-291 — epic progress is scoped to the caller's horizon.
	epicUnrestricted, epicCaller := h.unrestricted, h.visible
	epicProgressCols := epicProgressProjection(addRowParam(epicUnrestricted), addRowParam(epicCaller), addRowParam(h.spaces))
	parentCol := parentIDForReader(h, addRowParam)

	where := ""
	if len(baseClauses) > 0 {
		where = "WHERE " + strings.Join(baseClauses, " AND ") + " AND i.embedding IS NOT NULL"
	} else {
		where = "WHERE i.embedding IS NOT NULL"
	}

	sql := fmt.Sprintf(`
WITH semantic_hits AS (
    SELECT i.id, (1 - (i.embedding <=> %[1]s::halfvec))::float AS similarity
      FROM issues i
      %[5]s
     ORDER BY i.embedding <=> %[1]s::halfvec
     LIMIT %[3]s
)
SELECT i.id, i.project_id, `+parentCol+`, i.version, i.title,
       i.type, i.priority, i.status, i.pinned, i.assignee,
       i.created_by, uc.username, ua.id, `+issueSummaryPersonCols+`,
       i.created_at, i.updated_at, i.row_seq,
       `+rowComponentsLabelsCols+`,
       i.accepted_at, i.close_outcome,
       %[4]s AS i_assigned,
       %[6]s AS is_blocked,
       %[7]s,
       sh.similarity AS score
  FROM semantic_hits sh
  JOIN issues i ON i.id = sh.id
  LEFT JOIN users uc ON uc.id = i.created_by
  `+assignee.LeftJoin("ua", "i")+`
 WHERE sh.similarity >= %[2]s
 ORDER BY sh.similarity DESC`, embedPh, thresholdPh, limitPh, iAssignedExpr, where, readerBlockedBinder(h, addRowParam)(),
		epicProgressCols)

	rows, err := s.pool.Query(ctx, sql, rowParams...)
	if err != nil {
		return nil, fmt.Errorf("semantic rerank all issues: %w", err)
	}
	defer rows.Close()
	out := make([]struct {
		item  IssueSummary
		score float64
	}, 0, listAllSemanticTopK)
	for rows.Next() {
		var score float64
		item, err := scanIssueSummaryRow(rows, &score)
		if err != nil {
			return nil, fmt.Errorf("scanning semantic rerank row: %w", err)
		}
		out = append(out, struct {
			item  IssueSummary
			score float64
		}{item: item, score: score})
	}
	return out, rows.Err()
}

// buildAllIssuesWhere assembles the shared visibility + filter clauses
// for both the plain and search paths. Returns the WHERE-clause list and
// the parameter slice (positional, $1.. as accumulated). Cursor and
// match clauses are appended by callers.
// buildAllIssuesWhere — visibility is args.Scope's (sh, bound by the
// caller); the isBlocked filter reads the reader's horizon (h).
func buildAllIssuesWhere(h, sh readerHorizon, args ListAllIssuesArgs) ([]string, []any) {
	var (
		clauses []string
		params  []any
	)
	addParam := func(v any) string {
		params = append(params, v)
		return fmt.Sprintf("$%d", len(params))
	}

	clauses = append(clauses, authz.ProjectVisibleExpr("i", addParam(sh.unrestricted), addParam(sh.visible)))
	// sk-674.18 T032: the page filter `space=` — the issues of one space's
	// projects, the space taken from the issue's PROJECT (not its id
	// prefix); an unknown short id matches nothing.
	if args.Space != "" {
		clauses = append(clauses, fmt.Sprintf(
			"i.project_id IN (SELECT pj_s.id FROM projects pj_s JOIN spaces sp_s ON sp_s.id = pj_s.space_id WHERE sp_s.short_id = %s)",
			addParam(args.Space)))
	}

	if args.Status != "" {
		clauses = append(clauses, fmt.Sprintf("i.status = %s", addParam(args.Status)))
	}
	if args.Type != "" {
		clauses = append(clauses, fmt.Sprintf("i.type = %s", addParam(args.Type)))
	}
	if args.OriginGroup != nil {
		switch *args.OriginGroup {
		case OriginHumanDriven:
			clauses = append(clauses, fmt.Sprintf(
				"i.source = ANY(%s)",
				addParam([]string{SourceHuman, SourceAgentDirected})))
		case OriginAgentAutonomous:
			clauses = append(clauses, fmt.Sprintf(
				"i.source = %s",
				addParam(SourceAgentAutonomous)))
		case OriginAll:
			// no filter
		}
	}
	if args.AssigneeUserID != nil {
		clauses = append(clauses, assignee.MatchesCaller("i", addParam(*args.AssigneeUserID)))
	} else if args.Assignee != nil {
		clauses = append(clauses, fmt.Sprintf("LOWER(i.assignee) = LOWER(%s)", addParam(*args.Assignee)))
	}
	if args.CreatedBy != nil {
		// sk-34 — Delegated lens extension. See the matching branch in
		// store.go::ListIssues for the rationale + predicate. The
		// cross-project list mirrors the project-scoped path one-for-
		// one: when CallerUsername is populated, the OR-chain widens
		// to "created_by = me OR me ∈ previous_assignees AND ...";
		// otherwise we degrade to the strict pre-sk-34 predicate.
		callerPh := addParam(*args.CreatedBy)
		if args.CallerUsername != "" {
			// Trailing `assignee != me` applies to BOTH branches —
			// Delegated is the "tasks not currently on my plate"
			// lens (sk-34 T6).
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
		clauses = append(clauses, fmt.Sprintf("i.created_by <> %s", addParam(*args.CreatedByExclude)))
	}
	if args.AcceptedAtNull != nil && *args.AcceptedAtNull {
		clauses = append(clauses, "i.accepted_at IS NULL")
	}
	if args.AcceptedAtNotNull != nil && *args.AcceptedAtNotNull {
		clauses = append(clauses, "i.accepted_at IS NOT NULL")
	}
	if args.IsBlocked != nil {
		// sk-423: the reader's blocked-ness, the same fragment the row
		// projections render (each query binds its own copy of the horizon).
		blocked := readerBlockedBinder(h, addParam)()
		if *args.IsBlocked {
			clauses = append(clauses, blocked)
		} else {
			clauses = append(clauses, "NOT "+blocked)
		}
	}

	return clauses, params
}

// rowScanner is the single method scanIssueSummaryRow needs from
// pgx.Rows — declared consumer-side so the helper stays testable without
// a live pool.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanIssueSummaryRow scans one row of the shared all-issues projection
// (the column list is identical across the plain, FTS-search and
// semantic-rerank paths) and applies the nullable→field mapping every
// caller repeated verbatim. Keeping the 24-column order in ONE place is
// the point: a projection change that misses a scan site fails at
// runtime, not at compile time.
//
// sk-489 T005 widened it from 21 to 23: `close_outcome` on the row and the
// fourth progress leg `epic_no_work`. Three query builders share this
// scanner, so the count is stated once and the three of them cannot drift.
// (Recounted after review: the doc block said "22 at 20 -> 22", while
// the `dest` literal carried 21 elements before the change and 23 after. The delta was right, both
// absolute numbers were one too low, and the skew was inherited.)
//
// score is nil for the plain path (no score column) and non-nil for the
// search / rerank paths, which append a trailing score to the same
// projection; when non-nil it is filled from that column. The error is
// returned unwrapped so each caller keeps its own context message.
//
// sk-622.21 T026 widened it from 23 to 24: `row_seq` (the row freshness
// order, K16) right after `updated_at`. The column is NOT NULL since
// migration 000064, so it scans straight into IssueSummary.RowSeq.
//
// sk-622.21 R26 item 2 widened it from 24 to 26: `component_ids` and `labels`
// (rowComponentsLabelsCols) right after `row_seq`, so a row's labels and
// components come from the same statement — the same snapshot — as its
// rowSeq.
func scanIssueSummaryRow(rows rowScanner, score *float64) (IssueSummary, error) {
	var (
		item              IssueSummary
		parentID          *string
		assignee          *string
		createdByUsername *string
		assigneeUserID    *uuid.UUID
		acceptedAt        *time.Time
		closeOutcome      *string
		iAssigned         bool
		isBlocked         bool
		epicTotal         *int
		epicClosed        *int
		epicUnavailable   *int
		epicNoWork        *int
	)
	dest := []any{
		&item.ID, &item.ProjectID, &parentID, &item.Version, &item.Title,
		&item.Type, &item.Priority, &item.Status, &item.Pinned, &assignee,
		&item.CreatedBy, &createdByUsername, &assigneeUserID, &item.AssigneeStatus, &item.CreatedByStatus, &item.SpaceID,
		&item.CreatedAt, &item.UpdatedAt, &item.RowSeq,
		&item.ComponentIDs, &item.Labels,
		&acceptedAt, &closeOutcome,
		&iAssigned,
		&isBlocked,
		&epicTotal, &epicClosed, &epicUnavailable, &epicNoWork,
	}
	if score != nil {
		dest = append(dest, score)
	}
	if err := rows.Scan(dest...); err != nil {
		return IssueSummary{}, err
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
	return item, nil
}

// queryIssueSummaries runs the SQL projected by listAllIssuesPlain (no
// score column) and returns IssueSummary rows. Same scan shape as the
// other list paths. capacity is the expected max row count (typically Limit+1).
func (s *Store) queryIssueSummaries(ctx context.Context, sql string, params []any, capacity int) ([]IssueSummary, error) {
	rows, err := s.pool.Query(ctx, sql, params...)
	if err != nil {
		return nil, fmt.Errorf("listing all issues: %w", err)
	}
	defer rows.Close()
	items := make([]IssueSummary, 0, capacity)
	for rows.Next() {
		item, err := scanIssueSummaryRow(rows, nil)
		if err != nil {
			return nil, fmt.Errorf("scanning all-issues row: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// countAllIssues runs `SELECT count(*) FROM issues i ...` against the
// same WHERE clauses as the row query (sans cursor).
//
// No clause names the users join (the assignee=me filter is
// assignee.MatchesCaller on the issue row), so the count joins nothing: the
// row query's assignee join brings at most one user per issue (package
// assignee) and so never changes the number of issues. Joining it here was
// the cost (sk-674.18 EXPLAIN gate, sk-712): the admin's unfiltered
// COUNT became markedly slower on the baseline corpus.
func (s *Store) countAllIssues(ctx context.Context, clauses []string, params []any) (int, error) {
	where := ""
	if len(clauses) > 0 {
		where = "WHERE " + strings.Join(clauses, " AND ")
	}
	sql := "SELECT COUNT(*) FROM issues i " + where
	var n int
	if err := s.pool.QueryRow(ctx, sql, params...).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting all issues: %w", err)
	}
	return n, nil
}

// scoreToCursorTime / scoreFromCursorTime pack a float64 search score
// into a time.Time so the existing EncodeCursor / DecodeCursor pair
// keeps working without a schema change. Mirrors the helper in
// internal/projects/list_search.go.
func scoreToCursorTime(score float64) time.Time {
	if score < 0 {
		score = 0
	}
	const maxScore = 1e6
	if score > maxScore {
		score = maxScore
	}
	return time.Unix(0, int64(score*1e9)).UTC()
}

func scoreFromCursorTime(t time.Time) float64 {
	return float64(t.UnixNano()) / 1e9
}
