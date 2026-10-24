package tools

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
)

// sk-244.1 — skein_show includeDescription. The flag elides ONLY the
// description, and elides it to an explicit null so "not requested" stays
// distinguishable from "empty". Everything else on IssueDetail must
// survive the JSON-map projection untouched.

// setIssueProse fills the four prose fields on a seeded issue. seedIssue
// writes them as empty strings; these tests need real bodies to tell
// "elided" from "empty" and to prove the sibling fields are not cut.
func setIssueProse(t *testing.T, pool *pgxpool.Pool, issueID, description, design, acceptance, notes string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`UPDATE issues
		    SET description = $2, design = $3, acceptance_criteria = $4, notes = $5
		  WHERE id = $1`,
		issueID, description, design, acceptance, notes)
	if err != nil {
		t.Fatalf("set issue prose %s: %v", issueID, err)
	}
}

// showIssueMap pulls the `issue` object out of a successful skein_show
// response body.
func showIssueMap(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	raw, ok := body["issue"]
	if !ok {
		t.Fatalf("missing 'issue' key: %+v", body)
	}
	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("'issue' type %T, want object", raw)
	}
	return m
}

// seedIssueWithProse is the shared fixture: one project, one issue with
// all four prose fields populated, plus a wired-up handler and ctx.
func seedIssueWithProse(t *testing.T, prefixName, description string) (*pgxpool.Pool, string, func(map[string]any) map[string]any) {
	t.Helper()
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, prefixName)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "descflag issue", "open", 2)
	setIssueProse(t, pool, issueID, description,
		"design body", "acceptance body", "notes body")

	store := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	handler := handleShow(pool, store, depsStore)
	ctx := withCtx(t, userID, "admin")

	call := func(args map[string]any) map[string]any {
		return callShow(t, handler, ctx, args)
	}
	return pool, issueID, call
}

// TestShow_DescriptionDefaultIncluded — flag omitted keeps the pre-change
// behaviour: description present and equal to the issue body.
func TestShow_DescriptionDefaultIncluded(t *testing.T) {
	const body = "the long standing-task description"
	_, issueID, call := seedIssueWithProse(t, "shdesc", body)

	resp := call(map[string]any{"id": issueID})
	issue := showIssueMap(t, resp)

	if got := issue["description"]; got != body {
		t.Fatalf("description = %v (%T), want %q", got, got, body)
	}
	if _, ok := issue["bodyOmitted"]; ok {
		t.Errorf("bodyOmitted must not appear in skein_show responses: %v", issue["bodyOmitted"])
	}
}

// TestShow_DescriptionExplicitTrue — includeDescription:true is identical
// to omitting the flag.
func TestShow_DescriptionExplicitTrue(t *testing.T) {
	const body = "explicit true body"
	_, issueID, call := seedIssueWithProse(t, "shdesct", body)

	resp := call(map[string]any{"id": issueID, "includeDescription": true})
	issue := showIssueMap(t, resp)

	if got := issue["description"]; got != body {
		t.Fatalf("description = %v, want %q", got, body)
	}
}

// TestShow_DescriptionElidedToNull — includeDescription:false keeps the
// key PRESENT with a null value, and does not disturb any other field of
// IssueDetail (the whole risk of a JSON-map projection).
func TestShow_DescriptionElidedToNull(t *testing.T) {
	_, issueID, call := seedIssueWithProse(t, "shdescf", "should not be returned")

	resp := call(map[string]any{"id": issueID, "includeDescription": false})
	issue := showIssueMap(t, resp)

	raw, ok := issue["description"]
	if !ok {
		t.Fatalf("'description' key must be PRESENT (rendered null), not absent: %+v", issue)
	}
	if raw != nil {
		t.Fatalf("description = %v (%T), want null", raw, raw)
	}

	// The three sibling prose fields are explicitly out of scope for this
	// flag (sk-244.2 owns general field selection) — they must survive.
	for key, want := range map[string]string{
		"design":             "design body",
		"acceptanceCriteria": "acceptance body",
		"notes":              "notes body",
		"title":              "descflag issue",
		"status":             "open",
		"id":                 issueID,
		"type":               "task",
	} {
		if got := issue[key]; got != want {
			t.Errorf("issue[%q] = %v (%T), want %q", key, got, got, want)
		}
	}

	// version drives the expectedVersion cycle — the whole motive for the
	// flag is reading it cheaply, so losing it would defeat the feature.
	if got, ok := issue["version"].(float64); !ok || got != 1 {
		t.Errorf("version = %v (%T), want 1", issue["version"], issue["version"])
	}
	if got, ok := issue["priority"].(float64); !ok || got != 2 {
		t.Errorf("priority = %v (%T), want 2", issue["priority"], issue["priority"])
	}

	// Aggregates and scalars that the store always emits non-nil.
	for _, key := range []string{"projectId", "createdBy", "createdAt", "updatedAt", "source", "pinned", "metadata", "commentsCount"} {
		if _, ok := issue[key]; !ok {
			t.Errorf("field %q lost by the projection", key)
		}
	}
	for _, key := range []string{"labels", "componentIds", "commitRefs"} {
		v, ok := issue[key]
		if !ok {
			t.Errorf("field %q lost by the projection", key)
			continue
		}
		if _, isArr := v.([]any); !isArr {
			t.Errorf("field %q = %T, want array", key, v)
		}
	}

	// bodyOmitted belongs to the write-tools (sk-233) and means "all four
	// prose fields cut" — it must never leak into skein_show.
	if _, ok := issue["bodyOmitted"]; ok {
		t.Errorf("bodyOmitted must not appear when includeDescription=false: %v", issue["bodyOmitted"])
	}
}

// TestShow_EmptyDescriptionIsNotNull — "" and null are genuinely
// different signals: an issue with no description still returns "" when
// the description was requested.
func TestShow_EmptyDescriptionIsNotNull(t *testing.T) {
	_, issueID, call := seedIssueWithProse(t, "shdesce", "")

	included := showIssueMap(t, call(map[string]any{
		"id": issueID, "includeDescription": true,
	}))
	raw, ok := included["description"]
	if !ok {
		t.Fatalf("'description' key missing on includeDescription=true")
	}
	if raw == nil {
		t.Fatalf("empty description rendered as null — 'empty' must stay distinct from 'not requested'")
	}
	if raw != "" {
		t.Fatalf("description = %v, want empty string", raw)
	}

	elided := showIssueMap(t, call(map[string]any{
		"id": issueID, "includeDescription": false,
	}))
	if got := elided["description"]; got != nil {
		t.Fatalf("description = %v (%T), want null when not requested", got, got)
	}
}

// TestShow_DescriptionOffWithCommentsAndDepsOff — the minimal-read combo
// from the tool description: all three off in one call.
func TestShow_DescriptionOffWithCommentsAndDepsOff(t *testing.T) {
	_, issueID, call := seedIssueWithProse(t, "shdescm", "big body")

	resp := call(map[string]any{
		"id":                 issueID,
		"includeDescription": false,
		"includeComments":    false,
		"includeDeps":        false,
	})

	issue := showIssueMap(t, resp)
	if got, ok := issue["description"]; !ok || got != nil {
		t.Fatalf("description = %v (present=%v), want null", got, ok)
	}
	if got, ok := issue["version"].(float64); !ok || got != 1 {
		t.Errorf("version = %v, want 1", issue["version"])
	}

	// includeDeps=false → the WHOLE dependency block is gone: no edges and
	// — since sk-282 T009 — no counters either. This is the cheapest read
	// in a session (re-reading `version` before a write), and computing
	// counters for a caller that explicitly switched dependencies off
	// would be a regression on the most frequent call of a run.
	if _, ok := resp["deps"]; ok {
		t.Errorf("deps must be absent when includeDeps=false: %v", resp["deps"])
	}
	if _, ok := resp["depsCounts"]; ok {
		t.Errorf("depsCounts must be absent when includeDeps=false: %v — deps explicitly "+
			"off → neither edges nor counters", resp["depsCounts"])
	}
	// includeComments=false → comments null, meta keys absent.
	if v, ok := resp["comments"]; !ok || v != nil {
		t.Errorf("comments = %v (present=%v), want null", v, ok)
	}
	for _, key := range []string{"commentsTotal", "commentsReturned", "commentsTruncated"} {
		if _, ok := resp[key]; ok {
			t.Errorf("meta key %q must be absent when comments off", key)
		}
	}
}

// TestShow_DepsOffAlsoSuppressesCountsWithTypes is the other half of
// test-plan row 24: the explicit switch-off dominates a type list.
//
// Rule 1 of the US-1 binding is checked before the type list precisely so
// that this call answers with nothing rather than with a refusal — an
// explicit «no dependency block» is the strongest statement of intent the
// caller can make, and turning it into a second error path buys nothing.
func TestShow_DepsOffAlsoSuppressesCountsWithTypes(t *testing.T) {
	_, issueID, call := seedIssueWithProse(t, "shdepoff", "body")

	resp := call(map[string]any{
		"id":          issueID,
		"includeDeps": false,
		"depsTypes":   []any{"parent-child"},
	})

	for _, key := range []string{"deps", "depsCounts"} {
		if v, ok := resp[key]; ok {
			t.Errorf("%s present although includeDeps=false: %v — rule 1 dominates the type "+
				"list, and the answer is neither edges nor counters", key, v)
		}
	}
	if _, ok := resp["issue"]; !ok {
		t.Error("the call was turned into a refusal — an explicit switch-off must ANSWER, " +
			"not error")
	}
}
