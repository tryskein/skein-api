package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/reminders"
)

// newRemHandler wires a RemindersHandler over the shared stores for tests.
func newRemHandler(t *testing.T, pool *pgxpool.Pool) *RemindersHandler {
	t.Helper()
	return NewRemindersHandler(pool, reminders.NewStore(pool), issues.NewStore(pool), projects.NewStore(pool))
}

// callCreateReminder POSTs to RemindersHandler.Create with the given
// project/issue path params, mirroring what the chi router injects.
func callCreateReminder(t *testing.T, h *RemindersHandler, projectID, issueID string, userID uuid.UUID, role string, remindAt time.Time) (*http.Response, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"remindAt": remindAt.UTC().Format(time.RFC3339)})
	req := httptest.NewRequest(http.MethodPost, "/reminders", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	ctx := req.Context()
	ctx = auth.WithUserID(ctx, userID)
	ctx = auth.WithUserRole(ctx, role)
	req = req.WithContext(ctx)
	req = withChiURLParams(req, projectID, issueID)

	rec := httptest.NewRecorder()
	h.Create(rec, req)
	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })
	var parsed map[string]any
	_ = json.NewDecoder(res.Body).Decode(&parsed)
	return res, parsed
}

// TestRestReminder_CreateRejectsWrongProjectPath is the regression guard for
// the project-scoped REST contract: an issue is reachable only through its
// OWN project's path. A reminder create that addresses a visible issue via a
// different (also-visible) project's path must 404 — never silently create the
// reminder under the wrong route project. Matches the issues/comments handlers
// (issue.ProjectID != pid → 404).
func TestRestReminder_CreateRejectsWrongProjectPath(t *testing.T) {
	pool := newIMTestPool(t)
	user, _ := seedIMUser(t, pool, "user")
	pidA, _ := seedIMProject(t, pool, user)       // caller is a member of A...
	pidB, prefixB := seedIMProject(t, pool, user) // ...and of B, where the issue lives
	issueID := prefixB + "-1"
	seedCmtIssue(t, pool, pidB, user, issueID, "human")
	h := newRemHandler(t, pool)
	remindAt := time.Now().Add(time.Hour)

	// Correct project path (B owns the issue) → 201 created.
	res, body := callCreateReminder(t, h, pidB.String(), issueID, user, "user", remindAt)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("correct project path: want 201, got %d (body=%v)", res.StatusCode, body)
	}

	// Wrong project path (issue lives in B, requested through A — which the
	// caller CAN see) → 404, not a silent cross-project create.
	//
	// The status alone proves nothing, and asserting only the status is the
	// tautology sk-194.6.50 was written to avoid: `IssuesHandler.Patch` used
	// to answer exactly this 404 *after* committing the write, so a handler
	// that created the reminder and then noticed the project mismatch would
	// pass a status-only test. Count the rows.
	before := countRemindersForIssue(t, pool, issueID)
	res, body = callCreateReminder(t, h, pidA.String(), issueID, user, "user", remindAt)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong project path: want 404, got %d (body=%v)", res.StatusCode, body)
	}
	if code, _ := body["error"].(string); code != "issue_not_found" {
		t.Errorf("wrong project path: error code = %q, want issue_not_found (body=%v)", code, body)
	}
	if after := countRemindersForIssue(t, pool, issueID); after != before {
		t.Fatalf("wrong project path answered 404 but WROTE a reminder: "+
			"rows for %s went %d → %d", issueID, before, after)
	}
}

// TestRestReminder_CreateForeignIdIndistinguishableFromNonexistent pins the
// anti-oracle property on this route too: a real issue in a project the
// caller cannot reach through this path must answer exactly as an id that
// was never created. A split (say 403 for the real one) would let a caller
// walk ids through their own project's URL and learn which are real.
func TestRestReminder_CreateForeignIdIndistinguishableFromNonexistent(t *testing.T) {
	pool := newIMTestPool(t)
	user, _ := seedIMUser(t, pool, "user")
	pidA, prefixA := seedIMProject(t, pool, user)
	pidB, prefixB := seedIMProject(t, pool, user)
	issueID := prefixB + "-1"
	seedCmtIssue(t, pool, pidB, user, issueID, "human")
	h := newRemHandler(t, pool)
	remindAt := time.Now().Add(time.Hour)

	realRes, realBody := callCreateReminder(t, h, pidA.String(), issueID, user, "user", remindAt)
	fakeRes, fakeBody := callCreateReminder(t, h, pidA.String(), prefixA+"-99999", user, "user", remindAt)

	if realRes.StatusCode != fakeRes.StatusCode {
		t.Fatalf("existence oracle: real-but-foreign id → %d, nonexistent id → %d",
			realRes.StatusCode, fakeRes.StatusCode)
	}
	if realRes.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for both, got %d (body=%v)", realRes.StatusCode, realBody)
	}
	if realBody["error"] != fakeBody["error"] {
		t.Fatalf("existence oracle: real-but-foreign id → %v, nonexistent id → %v",
			realBody["error"], fakeBody["error"])
	}
}

// countRemindersForIssue is the "was anything actually written" probe for
// the negative cases — asserting the HTTP status only would not notice a
// row created before the 404.
func countRemindersForIssue(t *testing.T, pool *pgxpool.Pool, issueID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM reminders WHERE issue_id = $1`, issueID).Scan(&n); err != nil {
		t.Fatalf("count reminders for %s: %v", issueID, err)
	}
	return n
}
