package handler

// issues_review_create_default_test.go — sk-303.14 T009, matrix row 40 on
// the CLIENT (REST) create path.
//
// The US-3 requirement — "every create path yields the same result" — is
// checked on each path separately, not inferred from the paths sharing
// one store: the client path has its own request-body parse, and that
// parse is exactly what could substitute a value instead of leaving the
// flag unset.
//
// The probe is a personal default of FALSE, different from factory:
// "the flag is on" after create is equally true both when the author's
// default was read and when nobody read it.
//
// The client path has NO explicit flag pass-through and this wave will
// not add one (task bounds): the request body does not carry the key, and
// the last assertion below checks that a key passed in spite of that is
// rejected by the shape.

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/review"
	"skein-api/internal/testsupport"
)

func setPersonalReviewDefaultREST(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, value bool) {
	t.Helper()
	if value == review.DefaultReviewRequired {
		t.Fatalf("the probe sets personal default %v, equal to factory — it would not tell "+
			"\"author default was read\" from \"a constant sits in the code\"", value)
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO user_notification_prefs (user_id, review_required_default, updated_at)
VALUES ($1, $2, now())
ON CONFLICT (user_id) DO UPDATE SET review_required_default = EXCLUDED.review_required_default`,
		userID, value); err != nil {
		t.Fatalf("set personal review default: %v", err)
	}
}

func readReviewRequiredREST(t *testing.T, pool *pgxpool.Pool, issueID string) bool {
	t.Helper()
	var required bool
	if err := pool.QueryRow(context.Background(),
		`SELECT review_required FROM issues WHERE id = $1`, issueID).Scan(&required); err != nil {
		t.Fatalf("read review_required: %v", err)
	}
	return required
}

// createViaREST calls POST /projects/{projectID}/issues and returns the
// id of the created issue.
func createViaREST(t *testing.T, h *IssuesHandler, author uuid.UUID, pid uuid.UUID, body map[string]any) string {
	t.Helper()
	req := xpRequest(t, http.MethodPost, "", body, author, "user", "projectID", pid.String())
	res := xpCallFull(t, h.Create, req)
	if res.Status != http.StatusCreated {
		t.Fatalf("POST /issues: %d %s", res.Status, res.Raw)
	}
	id, _ := res.Body["id"].(string)
	if id == "" {
		t.Fatalf("create response carries no id: %s", res.Raw)
	}
	return id
}

// TestRESTCreate_ReviewRequired_PersonalDefaultApplies — the client path
// applies the author's personal default.
func TestRESTCreate_ReviewRequired_PersonalDefaultApplies(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	author, _ := testsupport.SeedUser(t, pool, "user")
	pid, _ := testsupport.SeedProject(t, pool, author, "revcrest")
	h := newXPIssuesHandler(t, pool)

	setPersonalReviewDefaultREST(t, pool, author, false)

	id := createViaREST(t, h, author, pid, map[string]any{
		"title": "created by the form", "type": "task", "priority": 2,
	})
	if got := readReviewRequiredREST(t, pool, id); got != false {
		t.Errorf("review_required = %v, want the author's personal default (false) — "+
			"the client create path yields a different result from the others (US-3)", got)
	}
}

// TestRESTCreate_ReviewRequired_FactoryWhenNoPrefsRow — an author with no
// prefs row gets the built-in default.
func TestRESTCreate_ReviewRequired_FactoryWhenNoPrefsRow(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	author, _ := testsupport.SeedUser(t, pool, "user")
	pid, _ := testsupport.SeedProject(t, pool, author, "revcrest")
	h := newXPIssuesHandler(t, pool)

	var prefsRows int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM user_notification_prefs WHERE user_id = $1`, author).
		Scan(&prefsRows); err != nil {
		t.Fatalf("count prefs rows: %v", err)
	}
	if prefsRows != 0 {
		t.Fatalf("precondition: the author already has a prefs row (%d)", prefsRows)
	}

	id := createViaREST(t, h, author, pid, map[string]any{
		"title": "without prefs", "type": "task", "priority": 2,
	})
	if got := readReviewRequiredREST(t, pool, id); got != review.DefaultReviewRequired {
		t.Errorf("review_required = %v, want factory %v",
			got, review.DefaultReviewRequired)
	}
}

// TestRESTCreate_ReviewRequired_BodyKeyIsRejected — a flag key in the
// create body is REJECTED by the shape, not ignored.
//
// This is a wave boundary recorded as an assertion: explicit flag
// pass-through lives only on the agent path (Q3/A3); this wave has no
// client create form with that field. Body parsing forbids unknown keys,
// so the refusal arrives on the shape — and that is better than silent
// ignore: a client that tried to pass the flag learns the flag was not
// applied, instead of believing it was.
//
// The test reddens in both directions: if the field is ever added on the
// client path, there will be no refusal and the assertion will fall — the
// change will be a deliberate act, not a silent widening of the surface
// through which the author's private bookkeeping travels.
func TestRESTCreate_ReviewRequired_BodyKeyIsRejected(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	author, _ := testsupport.SeedUser(t, pool, "user")
	pid, _ := testsupport.SeedProject(t, pool, author, "revcrest")
	h := newXPIssuesHandler(t, pool)

	req := xpRequest(t, http.MethodPost, "", map[string]any{
		"title": "key in the body", "type": "task", "priority": 2,
		"reviewRequired": false,
	}, author, "user", "projectID", pid.String())
	res := xpCallFull(t, h.Create, req)

	if res.Status == http.StatusCreated {
		t.Fatalf("create with the reviewRequired key succeeded — explicit flag pass-through on "+
			"the client path is not in this wave, and it may appear only deliberately: %s",
			res.Raw)
	}
	if res.Status != http.StatusBadRequest {
		t.Errorf("status %d, want 400 (shape refusal on an unknown key): %s",
			res.Status, res.Raw)
	}
}
