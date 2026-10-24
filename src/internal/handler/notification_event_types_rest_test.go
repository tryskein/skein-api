package handler

// notification_event_types_rest_test.go — row 4 of the sk-303.13 coverage
// matrix: the subscription-type catalogue contains only types that have a
// delivery path; a malformed request is 400, a well-formed request with an
// unknown or undeliverable type is 422.
//
// Subscription filters remain and still speak the language of event types —
// this is a subscription setting, not a channel setting, and the two axes
// compose (R-4). What changes is the catalogue: a type with no delivery
// path is a dead toggle on every surface, so it is not in the catalogue
// and cannot be written.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/notifications"
	"skein-api/internal/testsupport"
)

type catalogBody struct {
	EventTypes []struct {
		Type    string   `json:"type"`
		Label   string   `json:"label"`
		Reasons []string `json:"reasons"`
	} `json:"eventTypes"`
}

func callCatalog(t *testing.T, pool *pgxpool.Pool, caller uuid.UUID) catalogBody {
	t.Helper()
	h := NewNotificationsHandler(pool)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/notification-event-types", nil)
	ctx := auth.WithUserID(req.Context(), caller)
	ctx = auth.WithUserRole(ctx, "user")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	h.GetSubscriptionEventTypes(rec, req)
	res := rec.Result()
	t.Cleanup(func() { _ = res.Body.Close() })
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /notification-event-types = %d", res.StatusCode)
	}
	var body catalogBody
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

// TestSubscriptionCatalog_OnlyDeliverableTypes — catalogue contents.
func TestSubscriptionCatalog_OnlyDeliverableTypes(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, _ := testsupport.SeedUser(t, pool, "user")

	body := callCatalog(t, pool, caller)
	if len(body.EventTypes) == 0 {
		t.Fatal("the catalogue is empty")
	}
	seen := map[string]bool{}
	for _, e := range body.EventTypes {
		if seen[e.Type] {
			t.Errorf("duplicate catalogue entry %s", e.Type)
		}
		seen[e.Type] = true
		if e.Label == "" {
			t.Errorf("%s: catalogue entry without a label", e.Type)
		}
		// Every offered type is actually delivered…
		if !notifications.HasDeliveryPath(e.Type) {
			t.Errorf("%s is offered as a subscription filter but delivers nothing", e.Type)
		}
		// …and is named by reasons from the same dictionary as the settings
		// form. A list, not a single key: the classifier splits an issue
		// edit by contents, and one reason on such a type would be a lie
		// about the other half.
		if len(e.Reasons) == 0 {
			t.Errorf("%s is offered as a filter without naming a single reason", e.Type)
		}
		for _, key := range e.Reasons {
			if !notifications.IsKnownReason(notifications.ReasonKey(key)) {
				t.Errorf("%s points at unknown reason %q", e.Type, key)
			}
		}
	}

	// Types with no delivery path have been removed from the catalogue.
	for _, dead := range []string{
		"issue.claimed", "issue.closed", "issue.archived",
		"dependency.created", "dependency.deleted", "issue.label_added",
	} {
		if seen[dead] {
			t.Errorf("undeliverable type %s is still offered as a subscription filter", dead)
		}
	}
	// And the addressed reasons: they arrive regardless of subscription, a filter on them
	// would promise something the system does not do.
	// "Addressed" types: they arrive regardless of subscription, so a filter on them
	// would be a second toggle over the same signal. issue.outcome_changed
	// (sk-489) is of the same kind — it is addressed to the issue author by role, not to
	// subscribers.
	for _, addressed := range []string{
		"comment.mention", "issue.mention", "reminder.rang", "issue.outcome_changed",
	} {
		if seen[addressed] {
			t.Errorf("personally addressed type %s must not be a subscription filter", addressed)
		}
	}
}

// putPrefs sends a raw PUT /me/notification-prefs body.
func putPrefsRaw(t *testing.T, pool *pgxpool.Pool, caller uuid.UUID, raw string) (int, map[string]any) {
	t.Helper()
	h := NewNotificationsHandler(pool)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/me/notification-prefs",
		bytes.NewBufferString(raw))
	req.Header.Set("Content-Type", "application/json")
	ctx := auth.WithUserID(req.Context(), caller)
	ctx = auth.WithUserRole(ctx, "user")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	h.PutPrefs(rec, req)
	res := rec.Result()
	t.Cleanup(func() { _ = res.Body.Close() })
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body
}

// postCustomWatcherRaw sends a raw POST /me/custom-watchers body.
func postCustomWatcherRaw(t *testing.T, pool *pgxpool.Pool, caller uuid.UUID, raw string) (int, map[string]any) {
	t.Helper()
	h := NewNotificationsHandler(pool)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/me/custom-watchers",
		bytes.NewBufferString(raw))
	req.Header.Set("Content-Type", "application/json")
	ctx := auth.WithUserID(req.Context(), caller)
	ctx = auth.WithUserRole(ctx, "user")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	h.CreateCustomWatcher(rec, req)
	res := rec.Result()
	t.Cleanup(func() { _ = res.Body.Close() })
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body
}

// patchProjectWatchRaw sends a raw PATCH /projects/{id}/watch body.
func patchProjectWatchRaw(t *testing.T, pool *pgxpool.Pool, caller, projectID uuid.UUID, raw string) (int, map[string]any) {
	t.Helper()
	h := NewNotificationsHandler(pool)
	req := httptest.NewRequest(http.MethodPatch,
		"/api/v1/projects/"+projectID.String()+"/watch", bytes.NewBufferString(raw))
	req.Header.Set("Content-Type", "application/json")

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("projectID", projectID.String())
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = auth.WithUserID(ctx, caller)
	ctx = auth.WithUserRole(ctx, "user")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	h.PatchProjectWatcher(rec, req)
	res := rec.Result()
	t.Cleanup(func() { _ = res.Body.Close() })
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body
}

// TestSubscriptionFilters_MalformedIs400_UndeliverableIs422 — the boundary
// between a syntactic error and a business error on all three subscription-
// filter surfaces. The difference is not cosmetic: 4xx noise in monitoring
// must mean a broken client, not a user who named a type that does not exist.
func TestSubscriptionFilters_MalformedIs400_UndeliverableIs422(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, _ := testsupport.SeedUser(t, pool, "user")
	pid, _ := testsupport.SeedProject(t, pool, caller, "catalog")

	t.Run("notification prefs whitelist", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			body string
			want int
		}{
			{"deliverable type accepted", `{"notifyEventTypes":["issue.created","comment.created"]}`, http.StatusOK},
			{"unknown type", `{"notifyEventTypes":["issue.teleported"]}`, http.StatusUnprocessableEntity},
			{"undeliverable type", `{"notifyEventTypes":["issue.closed"]}`, http.StatusUnprocessableEntity},
			{"addressed type is not a subscription filter", `{"notifyEventTypes":["comment.mention"]}`, http.StatusUnprocessableEntity},
			{"broken json", `{"notifyEventTypes":[`, http.StatusBadRequest},
			{"not an array of strings", `{"notifyEventTypes":[42]}`, http.StatusBadRequest},
		} {
			t.Run(tc.name, func(t *testing.T) {
				code, body := putPrefsRaw(t, pool, caller, tc.body)
				if code != tc.want {
					t.Fatalf("status = %d, want %d (body %v)", code, tc.want, body)
				}
			})
		}
	})

	t.Run("custom watcher filter", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			body string
			want int
		}{
			{"deliverable type accepted",
				`{"name":"cat-ok","filter":{"event_types":["issue.status_changed"]}}`, http.StatusCreated},
			{"unknown type",
				`{"name":"cat-unknown","filter":{"event_types":["issue.teleported"]}}`, http.StatusUnprocessableEntity},
			{"undeliverable type",
				`{"name":"cat-dead","filter":{"event_types":["dependency.created"]}}`, http.StatusUnprocessableEntity},
			{"broken json", `{"name":"cat-broken","filter":{`, http.StatusBadRequest},
		} {
			t.Run(tc.name, func(t *testing.T) {
				code, body := postCustomWatcherRaw(t, pool, caller, tc.body)
				if code != tc.want {
					t.Fatalf("status = %d, want %d (body %v)", code, tc.want, body)
				}
			})
		}
	})

	t.Run("project subscription whitelist", func(t *testing.T) {
		// Type validation runs BEFORE any work on the subscription, so a
		// type refusal is visible without an existing subscription; the
		// success path needs a subscription, so it is created separately.
		for _, tc := range []struct {
			name string
			body string
			want int
		}{
			{"unknown type", `{"eventTypes":["issue.teleported"]}`, http.StatusUnprocessableEntity},
			{"undeliverable type", `{"eventTypes":["issue.claimed"]}`, http.StatusUnprocessableEntity},
			{"broken json", `{"eventTypes":[`, http.StatusBadRequest},
			{"not an array of strings", `{"eventTypes":[7]}`, http.StatusBadRequest},
		} {
			t.Run(tc.name, func(t *testing.T) {
				code, body := patchProjectWatchRaw(t, pool, caller, pid, tc.body)
				if code != tc.want {
					t.Fatalf("status = %d, want %d (body %v)", code, tc.want, body)
				}
			})
		}

		// A deliverable type passes validation: a refusal, if it happens
		// at all, comes from the subscription (404 without one), not from
		// the catalogue.
		code, body := patchProjectWatchRaw(t, pool, caller, pid, `{"eventTypes":["comment.created"]}`)
		if code == http.StatusUnprocessableEntity {
			t.Errorf("a deliverable type was rejected by the catalogue: %v", body)
		}
	})

	// A refusal wrote nothing: the whitelist stayed as the last SUCCESSFUL
	// request left it.
	var stored []string
	if err := pool.QueryRow(context.Background(), `
SELECT notify_event_types FROM user_notification_prefs WHERE user_id = $1`, caller).Scan(&stored); err != nil {
		t.Fatalf("read prefs: %v", err)
	}
	if len(stored) != 2 || stored[0] != "issue.created" || stored[1] != "comment.created" {
		t.Errorf("notify_event_types = %v, want the last accepted pair — a rejected request wrote through", stored)
	}
}
