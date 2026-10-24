package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/push"
)

func newPushPrefsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run DB-backed tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func seedPushPrefsUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "pp" + id.String()[:16]
	_, err := pool.Exec(context.Background(), `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', 'fake', now(), '{}'::jsonb)`,
		id, uname+"@t.local", uname, uname)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM user_notification_prefs WHERE user_id = $1`, id)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

func authReq(method, target string, body []byte, userID uuid.UUID) *http.Request {
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, target, bytes.NewReader(body))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	ctx := auth.WithUserRole(auth.WithUserID(r.Context(), userID), "user")
	return r.WithContext(ctx)
}

// GET defaults to empty disabled set + full registry; PUT round-trips.
func TestPushPrefs_GetPutRoundTrip(t *testing.T) {
	pool := newPushPrefsPool(t)
	h := NewNotificationsHandler(pool)
	user := seedPushPrefsUser(t, pool)

	// Initial GET: empty disabled, registry present.
	rec := httptest.NewRecorder()
	h.GetPushPrefs(rec, authReq(http.MethodGet, "/api/v1/me/push-prefs", nil, user))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status: %d", rec.Code)
	}
	var got pushPrefsResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	if len(got.DisabledCategories) != 0 {
		t.Errorf("expected empty disabled, got %v", got.DisabledCategories)
	}
	if len(got.Categories) != len(push.Categories) {
		t.Errorf("registry size: got %d want %d", len(got.Categories), len(push.Categories))
	}

	// PUT: disable reminder.
	put := []byte(`{"disabledCategories":["` + push.CategoryReminder + `"]}`)
	rec = httptest.NewRecorder()
	h.PutPushPrefs(rec, authReq(http.MethodPut, "/api/v1/me/push-prefs", put, user))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status: %d body=%s", rec.Code, rec.Body.String())
	}

	// GET again: reminder is now disabled.
	rec = httptest.NewRecorder()
	h.GetPushPrefs(rec, authReq(http.MethodGet, "/api/v1/me/push-prefs", nil, user))
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode GET2: %v", err)
	}
	if len(got.DisabledCategories) != 1 || got.DisabledCategories[0] != push.CategoryReminder {
		t.Errorf("expected [reminder], got %v", got.DisabledCategories)
	}
}

// The registry surfaced by GET includes the delegated_status category and it
// can be disabled via PUT (registry-driven — adding it to the Go registry is
// all that's needed).
func TestPushPrefs_IncludesDelegatedStatus(t *testing.T) {
	pool := newPushPrefsPool(t)
	h := NewNotificationsHandler(pool)
	user := seedPushPrefsUser(t, pool)

	rec := httptest.NewRecorder()
	h.GetPushPrefs(rec, authReq(http.MethodGet, "/api/v1/me/push-prefs", nil, user))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status: %d", rec.Code)
	}
	var got pushPrefsResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	found := false
	for _, c := range got.Categories {
		if c.Key == push.CategoryDelegatedStatus {
			found = true
			if c.Label == "" {
				t.Errorf("delegated_status category missing label")
			}
		}
	}
	if !found {
		t.Fatalf("delegated_status category not present in prefs registry: %+v", got.Categories)
	}

	// It is a valid disable target (not rejected as unknown).
	put := []byte(`{"disabledCategories":["` + push.CategoryDelegatedStatus + `"]}`)
	rec = httptest.NewRecorder()
	h.PutPushPrefs(rec, authReq(http.MethodPut, "/api/v1/me/push-prefs", put, user))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status: %d body=%s", rec.Code, rec.Body.String())
	}
}

// PUT with an unknown category is 422 and persists nothing.
func TestPushPrefs_PutUnknownCategory422(t *testing.T) {
	pool := newPushPrefsPool(t)
	h := NewNotificationsHandler(pool)
	user := seedPushPrefsUser(t, pool)

	put := []byte(`{"disabledCategories":["not_a_category"]}`)
	rec := httptest.NewRecorder()
	h.PutPushPrefs(rec, authReq(http.MethodPut, "/api/v1/me/push-prefs", put, user))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d body=%s", rec.Code, rec.Body.String())
	}

	// Nothing persisted — GET still empty.
	rec = httptest.NewRecorder()
	h.GetPushPrefs(rec, authReq(http.MethodGet, "/api/v1/me/push-prefs", nil, user))
	var got pushPrefsResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	if len(got.DisabledCategories) != 0 {
		t.Errorf("expected nothing persisted, got %v", got.DisabledCategories)
	}
}
