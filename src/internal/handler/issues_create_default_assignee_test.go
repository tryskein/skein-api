package handler

// issues_create_default_assignee_test.go — sk-565, T010, test matrix row 34
// (INV-4, SC-004). Reference — spec sk-565: "REST create does not read the
// setting and behaves as before the change"; "REST create and
// standing create give the same result for the assignee as before the
// change". The setting is ON (explicitly and as the factory value of a
// missing row): only a default that fires without P1 — e.g. on an empty
// Assignee alone — would assign here.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/testsupport"
)

func readAssignAndAcceptance(t *testing.T, pool *pgxpool.Pool, id string) (*string, *time.Time, *uuid.UUID) {
	t.Helper()
	var (
		assignee   *string
		acceptedAt *time.Time
		acceptedBy *uuid.UUID
	)
	if err := pool.QueryRow(context.Background(),
		`SELECT assignee, accepted_at, accepted_by FROM issues WHERE id = $1`, id).
		Scan(&assignee, &acceptedAt, &acceptedBy); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return assignee, acceptedAt, acceptedBy
}

// TestRESTCreate_DefaultAssigneeNeverFires — row 34.
func TestRESTCreate_DefaultAssigneeNeverFires(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	h := newXPIssuesHandler(t, pool)

	for _, tc := range []struct {
		name    string
		prefRow bool
	}{
		{"setting on in a prefs row", true},
		{"no prefs row (factory on)", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			author, authorName := testsupport.SeedUser(t, pool, "user")
			pid, _ := testsupport.SeedProject(t, pool, author, "dacrest")
			if tc.prefRow {
				if _, err := pool.Exec(context.Background(), `
INSERT INTO user_notification_prefs (user_id, assign_agent_issues_to_me, updated_at)
VALUES ($1, true, now())`, author); err != nil {
					t.Fatalf("seed prefs row: %v", err)
				}
			}

			id := createViaREST(t, h, author, pid, map[string]any{
				"title": "from the web form", "type": "task", "priority": 2,
			})
			assignee, acceptedAt, acceptedBy := readAssignAndAcceptance(t, pool, id)
			if assignee != nil && *assignee != "" {
				t.Errorf("REST create without assignee: assignee = %q, want none (INV-4)", *assignee)
			}
			if acceptedAt != nil || acceptedBy != nil {
				t.Errorf("REST create without assignee: accepted_at = %v, accepted_by = %v, want both empty", acceptedAt, acceptedBy)
			}

			id = createViaREST(t, h, author, pid, map[string]any{
				"title": "explicit", "type": "task", "priority": 2, "assignee": authorName,
			})
			assignee, acceptedAt, acceptedBy = readAssignAndAcceptance(t, pool, id)
			if assignee == nil || *assignee != authorName {
				t.Errorf("REST create with explicit assignee %q: assignee = %v", authorName, assignee)
			}
			if acceptedAt == nil || acceptedBy == nil || *acceptedBy != author {
				t.Errorf("REST explicit self-assignment: accepted_at = %v, accepted_by = %v, want stamped by the author as before",
					acceptedAt, acceptedBy)
			}
		})
	}
}
