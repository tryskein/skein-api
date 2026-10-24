package handler

// notifications_inbox_rest_contract_test.go — sk-282 T019, test-plan row
// 74, and the code for the final consolidated REST regression (row 103).
//
// "INV-2: REST notification inbox — keys, types and PREVIOUS VALUES, in
// particular the comment body arrives UNTRUNCATED. The main T019 leak
// vector here is values, not the key set: a truncation applied at the
// wrong boundary will not touch any keys."
//
// That is the design of this file: the row's key set is pinned, but the
// assertion that matters is on the VALUE — a cut made in the notifications
// store instead of in the MCP projection would shorten what the web inbox
// renders without removing a single key, and a key-set guard would pass.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/testsupport"
)

// restNotificationKeys is the REST inbox row's always-present field set.
var restNotificationKeys = map[string]string{
	"id": "number", "type": "string", "createdAt": "string",
	"actor": "object", "entity": "object", "summary": "string",
}

// restNotificationOptionalKeys carry omitempty.
//
// oldValue / newValue are declared «any» because that is what they are:
// NotificationDTO types them `any` and they carry whatever the changed
// field held — a string for a description, a string for a status, a number
// for a priority, an object for a structured field. They were declared
// "string" here, which asserted a type the DTO does not promise: a
// priority change would have failed this guard for being correct.
var restNotificationOptionalKeys = map[string]string{
	"readAt": "string", "archivedAt": "string", "actorSessionId": "string",
	"commentId": "string", "bodyPreview": "string", "truncated": "bool",
	"field": "string", "oldValue": "any", "newValue": "any",
	"mentionContext": "string",

	// sk-674.18 T029 (spaces/contract.go: Inbox, Mentions, Watching rows) —
	// the short id of the space of the row's project.
	"spaceId": "string",

	// sk-489 T001 (US-3, the "Notifications" contract) — extending the key set as a
	// DELIBERATE ACT: an inbox row gets the close outcome of the issue
	// it is about.
	//
	// Optional not only by tag: the field is empty for the overwhelming
	// majority of rows (comments, mentions, assignments), and an inert
	// `closeOutcome:null` would change the inbox JSON on every one of them.
	"closeOutcome": "string",
}

// seedRESTNotification writes an event with field/old/new plus one unread
// notification, and returns the new value it wrote.
func seedRESTNotification(t *testing.T, pool *pgxpool.Pool, userID, projectID uuid.UUID, issueID, newValue string) {
	t.Helper()
	ctx := context.Background()
	oldRaw, _ := json.Marshal("")
	newRaw, _ := json.Marshal(newValue)

	var eventID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id,
                    actor, actor_user_id, actor_type, payload,
                    field, old_data, new_data)
VALUES (now(), 'issue.updated', 'issue', $1, $2, 'tester', $3, 'human', '{}'::jsonb,
        'description', $4::jsonb, $5::jsonb)
RETURNING id`, issueID, projectID, userID, oldRaw, newRaw).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO notifications (user_id, event_id, type, project_id, created_at, feed_channel)
VALUES ($1, $2, 'issue.updated', $3, now(), true)`, userID, eventID, projectID); err != nil {
		t.Fatalf("seed notification: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE id = $1`, eventID)
	})
}

func callRESTInbox(t *testing.T, pool *pgxpool.Pool, caller uuid.UUID) map[string]any {
	t.Helper()
	h := NewNotificationsHandler(pool)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/notifications", nil)
	ctx := auth.WithUserID(req.Context(), caller)
	ctx = auth.WithUserRole(ctx, "user")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	h.ListNotifications(rec, req)
	res := rec.Result()
	t.Cleanup(func() { _ = res.Body.Close() })
	if res.StatusCode != http.StatusOK {
		var problem map[string]any
		_ = json.NewDecoder(res.Body).Decode(&problem)
		t.Fatalf("GET /notifications = %d: %v", res.StatusCode, problem)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

// TestNotificationsREST_ValuesUntruncatedAndSummaryIntact is row 74.
func TestNotificationsREST_ValuesUntruncatedAndSummaryIntact(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "restnotif")
	const title = "a REST inbox fixture issue"
	issueID := testsupport.SeedIssue(t, pool, pid, owner, prefix+"-1", title, "open", 2)

	// Far longer than the MCP cut, so a leak is unmissable.
	long := strings.Repeat("y", 5*1024)
	seedRESTNotification(t, pool, owner, pid, issueID, long)

	body := callRESTInbox(t, pool, owner)
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items missing: %#v", body["items"])
	}
	if len(items) != 1 {
		t.Fatalf("REST inbox returned %d rows, want 1", len(items))
	}
	row := items[0].(map[string]any)

	// The leak vector: a value, not a key.
	newValue, _ := row["newValue"].(string)
	if newValue != long {
		t.Errorf("REST newValue is %d runes, want the whole %d — T019 cuts it in the MCP "+
			"projection ONLY. A cut applied at the wrong boundary shortens what the web inbox "+
			"renders and removes no key at all, which is why this is asserted on the value.",
			len([]rune(newValue)), len([]rune(long)))
	}
	if _, ok := row["valuesTruncated"]; ok {
		t.Error("REST row carries valuesTruncated — that marker belongs to the MCP projection, " +
			"and INV-2 forbids an additive key here")
	}

	// The summary must still name the issue: the de-duplication is an MCP
	// projection too, and the web inbox renders the sentence as-is.
	summary, _ := row["summary"].(string)
	if !strings.Contains(summary, issueID) {
		t.Errorf("REST summary no longer names the issue (%q) — T019 de-duplicates it in the "+
			"MCP projection only", summary)
	}
	if !strings.Contains(summary, title) {
		t.Errorf("REST summary no longer carries the issue title: %q", summary)
	}

	for key, value := range row {
		wantKind, known := restNotificationKeys[key]
		if !known {
			wantKind, known = restNotificationOptionalKeys[key]
		}
		if !known {
			t.Errorf("REST notification row carries an unclassified key %q = %#v", key, value)
			continue
		}
		assertJSONKind(t, "REST notification row", key, value, wantKind)
	}
	for key := range restNotificationKeys {
		if _, ok := row[key]; !ok {
			t.Errorf("REST notification row is MISSING %q — the set is exact, not an upper bound", key)
		}
	}
}

// ---------------------------------------------------------------------
// Row 103 — the final consolidated REST regression (SC-012 / INV-2).
//
// Its own matrix row says what it is NOT: "it is not a standalone proof
// of the contract and does not serve as a release gate" — a green sweep does
// not catch a changed type or a changed value of an existing field, and
// the per-surface rows above each carry an oracle for their own leak
// vector. This is the breadth check that sits on top of them: every REST
// surface sk-282 touched, called once, in one test, so a surface that
// stops responding at all cannot hide behind a passing neighbour.
//
// The RUN belongs to the final stage, not to this test file.
// ---------------------------------------------------------------------

// TestRESTRegression_AllTouchedSurfacesStillAnswer is row 103.
func TestRESTRegression_AllTouchedSurfacesStillAnswer(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, ownerName := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "restreg")
	issueID := testsupport.SeedIssue(t, pool, pid, owner, prefix+"-1", "regression fixture", "open", 2)
	testsupport.SetAssignee(t, pool, issueID, ownerName)
	child := testsupport.SeedIssue(t, pool, pid, owner, prefix+"-1.1", "child", "open", 2)
	seedRESTNotification(t, pool, owner, pid, issueID, "a value")

	// Each entry is a surface sk-282 changed on the MCP side and must not
	// have changed here. The check is deliberately shallow — presence and
	// shape — because depth is the per-surface rows' job.
	surfaces := []struct {
		name  string
		call  func(t *testing.T) map[string]any
		items string // key holding the collection, empty when the body IS one
	}{
		{
			name:  "issue card",
			call:  func(t *testing.T) map[string]any { return callIssueGet(t, pool, owner, "user", pid, issueID) },
			items: "",
		},
		{
			name:  "project issue list",
			call:  func(t *testing.T) map[string]any { return callRESTIssueList(t, pool, owner, "user", pid, "") },
			items: "items",
		},
		{
			name:  "project list",
			call:  func(t *testing.T) map[string]any { return callRESTProjectList(t, pool, owner, "user", "") },
			items: "items",
		},
		{
			name:  "notifications inbox",
			call:  func(t *testing.T) map[string]any { return callRESTInbox(t, pool, owner) },
			items: "items",
		},
	}

	for _, s := range surfaces {
		t.Run(s.name, func(t *testing.T) {
			body := s.call(t)
			if len(body) == 0 {
				t.Fatalf("%s returned an empty body", s.name)
			}
			if s.items == "" {
				return
			}
			items, ok := body[s.items].([]any)
			if !ok {
				t.Fatalf("%s has no %q array: %#v", s.name, s.items, body[s.items])
			}
			if len(items) == 0 {
				t.Errorf("%s returned an empty collection — the fixture seeds rows for it, so "+
					"an empty answer is a regression rather than a quiet surface", s.name)
			}
		})
	}
	_ = child
}
