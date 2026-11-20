package main

// push_contract_route_test.go — matrix rows 19, 19a, 19b on the production
// router with a live PushHandler. Reference — the push REST contract v2 of
// the plan: every response that passed through the /push group (2xx,
// including an empty 204, 400 and 422) carries Skein-Push-Contract: 2; a
// 401 session_revoked of RequireSessionEpoch, produced before the group,
// and routes outside the group do not; the session epoch checked by
// RequireSessionEpoch reaches the store; POST /push/device-removals and
// POST /push/device-revocations do what the meaning of 204 says.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/config"
	"skein-api/internal/dependencies"
	"skein-api/internal/handler"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/push"
	"skein-api/internal/search"
	"skein-api/internal/testsupport"
	"skein-api/internal/users"
)

// pushContractRouter builds the production router with a live
// PushHandler and the readers of /me/notification-counts.
func pushContractRouter(t *testing.T, pool *pgxpool.Pool) chi.Router {
	t.Helper()
	t.Setenv("DATABASE_URL", os.Getenv("TEST_DATABASE_URL"))
	t.Setenv("SKEIN_INTERNAL_SECRET", wiringInternalSecret)
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("SKEIN_AGENT_PUBLIC_URL", "http://skein-wiring.test")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	issuesStore := issues.NewStore(pool)
	notifStore, meH := newNotificationReads(pool, cfg, issuesStore)
	searchSvc, err := search.NewService(nil, nil, 0)
	if err != nil {
		t.Fatalf("search.NewService: %v", err)
	}
	pushH := handler.NewPushHandler(pool, push.NewStore(pool), "pub", true)
	router, _ := buildRouter(
		pool, cfg,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, // REST handlers authH…dashboardH
		nil,                          // usersStore
		issuesStore,                  // issuesStore
		dependencies.NewStore(pool),  // depsStore
		projects.NewStore(pool),      // projectsStore
		notifStore,                   // notifStore
		nil,                          // remindersStore
		searchSvc,                    // searchSvc
		nil,                          // s3Client
		nil,                          // attachmentsH
		meH,                          // meH
		nil, nil, nil, nil, nil, nil, // themesH…remindersH
		pushH, // pushH — the handler under test
	)
	return router
}

type pushRoute struct {
	t      *testing.T
	router chi.Router
}

// serve sends a BFF request of user with the session epoch epoch.
func (p pushRoute) serve(method, path string, body any, user uuid.UUID, epoch int64) *httptest.ResponseRecorder {
	p.t.Helper()
	var raw []byte
	switch b := body.(type) {
	case nil:
	case string:
		raw = []byte(b)
	default:
		var err error
		if raw, err = json.Marshal(b); err != nil {
			p.t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.HeaderInternalSecret, wiringInternalSecret)
	req.Header.Set(auth.HeaderUserID, user.String())
	req.Header.Set(auth.HeaderUserRole, "user")
	req.Header.Set(auth.HeaderSessionEpoch, strconv.FormatInt(epoch, 10))
	rec := httptest.NewRecorder()
	p.router.ServeHTTP(rec, req)
	return rec
}

func pushErrorCode(rec *httptest.ResponseRecorder) string {
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	code, _ := body["error"].(string)
	return code
}

func sessionEpochOf(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) int64 {
	t.Helper()
	var epoch int64
	if err := pool.QueryRow(context.Background(), `SELECT session_epoch FROM users WHERE id = $1`, user).Scan(&epoch); err != nil {
		t.Fatalf("read session epoch: %v", err)
	}
	return epoch
}

func countRows(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func subscriptionExists(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) bool {
	t.Helper()
	return countRows(t, pool, `SELECT count(*) FROM push_subscriptions WHERE id = $1`, id) == 1
}

func labelRevoked(t *testing.T, pool *pgxpool.Pool, user, label uuid.UUID) bool {
	t.Helper()
	return countRows(t, pool, `SELECT count(*) FROM push_device_revocations WHERE user_id = $1 AND device_label = $2`, user, label) == 1
}

func seedRouteSubscription(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID, label *uuid.UUID) (uuid.UUID, string) {
	t.Helper()
	id := uuid.New()
	endpoint := "https://push.test.local/" + uuid.NewString()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO push_subscriptions (id, user_id, endpoint, p256dh, auth, device_label)
VALUES ($1, $2, $3, 'seed-p256dh', 'seed-auth', $4)`, id, owner, endpoint, label); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
	return id, endpoint
}

func registerBody(expected uuid.UUID, endpoint string, label uuid.UUID) map[string]any {
	return map[string]any{
		"endpoint": endpoint, "keys": map[string]string{"p256dh": "p", "auth": "a"},
		"expectedUserId": expected.String(), "deviceLabel": label.String(), "mode": push.ModeUpsert,
	}
}

// Row 19 — the v2 marker and its layer boundary; the epoch from the context.
func TestPushContract_MarkerOnTheGroupOnly(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	p := pushRoute{t: t, router: pushContractRouter(t, pool)}
	u, _ := testsupport.SeedUser(t, pool, "user")
	epoch := sessionEpochOf(t, pool, u)

	marked := []struct {
		name   string
		method string
		path   string
		body   any
		status int
	}{
		{"200 list", http.MethodGet, "/ui/v1/push/subscriptions", nil, http.StatusOK},
		{"empty 204 of a channel removal", http.MethodDelete, "/ui/v1/push/subscriptions",
			map[string]any{"expectedUserId": u.String(), "endpoint": "https://push.test.local/" + uuid.NewString()}, http.StatusNoContent},
		{"400 of a malformed body", http.MethodPost, "/ui/v1/push/device-revocations", `{`, http.StatusBadRequest},
		{"422 of a removal by id", http.MethodPost, "/ui/v1/push/device-removals",
			map[string]any{"expectedUserId": u.String(), "id": uuid.NewString()}, http.StatusUnprocessableEntity},
	}
	for _, c := range marked {
		rec := p.serve(c.method, c.path, c.body, u, epoch)
		if rec.Code != c.status {
			t.Errorf("%s: answered %d %s, want %d", c.name, rec.Code, rec.Body.String(), c.status)
		}
		if got := rec.Header().Get(push.ContractHeader); got != push.ContractVersion {
			t.Errorf("%s: %s = %q, want %q", c.name, push.ContractHeader, got, push.ContractVersion)
		}
	}

	rec := p.serve(http.MethodGet, "/ui/v1/push/subscriptions", nil, u, epoch+1)
	if rec.Code != http.StatusUnauthorized || pushErrorCode(rec) != auth.CodeSessionRevoked {
		t.Fatalf("stale session on a push route: %d %s, want 401 %s", rec.Code, rec.Body.String(), auth.CodeSessionRevoked)
	}
	if got := rec.Header().Get(push.ContractHeader); got != "" {
		t.Errorf("401 session_revoked carries %s = %q, want none", push.ContractHeader, got)
	}
	rec = p.serve(http.MethodGet, "/ui/v1/me/notification-counts", nil, u, epoch)
	if rec.Code != http.StatusOK {
		t.Fatalf("route outside /push: %d %s, want 200", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(push.ContractHeader); got != "" {
		t.Errorf("route outside /push carries %s = %q, want none", push.ContractHeader, got)
	}

	var raised int64
	if err := pgx.BeginTxFunc(context.Background(), pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var err error
		raised, err = users.BumpSessionEpochInTx(context.Background(), tx, u)
		return err
	}); err != nil {
		t.Fatalf("BumpSessionEpochInTx: %v", err)
	}
	rec = p.serve(http.MethodPost, "/ui/v1/push/subscriptions",
		registerBody(u, "https://push.test.local/"+uuid.NewString(), uuid.New()), u, raised)
	if rec.Code != http.StatusOK {
		t.Errorf("registration with the raised epoch: %d %s, want 200 — the checked epoch reaches the store", rec.Code, rec.Body.String())
	}

	// A failure of the store is a 500 of the group, outside the contract:
	// no marker. A trigger of the test database makes u's next insert fail.
	name := "push_test_store_failure_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	for _, sql := range []string{
		fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'store failure'; END $$`, name),
		fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON push_subscriptions FOR EACH ROW WHEN (NEW.user_id = '%s') EXECUTE FUNCTION %s()`, name, u, name),
	} {
		if _, err := pool.Exec(context.Background(), sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON push_subscriptions`, name))
		_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, name))
	})
	rec = p.serve(http.MethodPost, "/ui/v1/push/subscriptions",
		registerBody(u, "https://push.test.local/"+uuid.NewString(), uuid.New()), u, raised)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("registration over a failing store: %d %s, want 500", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(push.ContractHeader); got != "" {
		t.Errorf("500 of the group carries %s = %q, want none", push.ContractHeader, got)
	}
}

// Row 19a — US-3 «removing another device» on the production router.
func TestPushContract_DeviceRemoval(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	p := pushRoute{t: t, router: pushContractRouter(t, pool)}
	u, _ := testsupport.SeedUser(t, pool, "user")
	other, _ := testsupport.SeedUser(t, pool, "user")
	epoch := sessionEpochOf(t, pool, u)
	l, l2 := uuid.New(), uuid.New()
	target, _ := seedRouteSubscription(t, pool, u, &l)
	sameLabel, _ := seedRouteSubscription(t, pool, u, &l)
	otherLabel, _ := seedRouteSubscription(t, pool, u, &l2)
	foreign, _ := seedRouteSubscription(t, pool, other, &l)

	remove := func(id uuid.UUID) *httptest.ResponseRecorder {
		return p.serve(http.MethodPost, "/ui/v1/push/device-removals",
			map[string]any{"expectedUserId": u.String(), "id": id.String()}, u, epoch)
	}
	if rec := remove(target); rec.Code != http.StatusNoContent {
		t.Fatalf("removal: %d %s, want 204", rec.Code, rec.Body.String())
	}
	if subscriptionExists(t, pool, target) || subscriptionExists(t, pool, sameLabel) {
		t.Error("a subscription of the removed device remained")
	}
	if !labelRevoked(t, pool, u, l) {
		t.Error("the label is not revoked")
	}
	if !subscriptionExists(t, pool, otherLabel) || !subscriptionExists(t, pool, foreign) {
		t.Error("a subscription outside the removed device was deleted")
	}
	rec := p.serve(http.MethodPost, "/ui/v1/push/subscriptions", registerBody(u, "https://push.test.local/"+uuid.NewString(), l), u, epoch)
	if rec.Code != http.StatusUnprocessableEntity || pushErrorCode(rec) != push.CodeDeviceRevoked {
		t.Errorf("registration with the removed device's label: %d %s, want 422 %s", rec.Code, rec.Body.String(), push.CodeDeviceRevoked)
	}
	for name, id := range map[string]uuid.UUID{"repeat": target, "another user's id": foreign} {
		if rec := remove(id); rec.Code != http.StatusUnprocessableEntity || pushErrorCode(rec) != push.CodeSubscriptionNotFound {
			t.Errorf("%s: %d %s, want 422 %s", name, rec.Code, rec.Body.String(), push.CodeSubscriptionNotFound)
		}
	}
	if !subscriptionExists(t, pool, foreign) || labelRevoked(t, pool, other, l) {
		t.Error("another user's subscription or labels changed")
	}
}

// Row 19b — US-3, US-4 revocation of the caller's device on the production
// router.
func TestPushContract_DeviceRevocation(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	p := pushRoute{t: t, router: pushContractRouter(t, pool)}
	u, _ := testsupport.SeedUser(t, pool, "user")
	epoch := sessionEpochOf(t, pool, u)
	l, l2 := uuid.New(), uuid.New()
	labelled, _ := seedRouteSubscription(t, pool, u, &l)
	unlabelled, endpoint := seedRouteSubscription(t, pool, u, nil)
	kept, _ := seedRouteSubscription(t, pool, u, &l2)

	rec := p.serve(http.MethodPost, "/ui/v1/push/device-revocations",
		map[string]any{"expectedUserId": u.String(), "deviceLabels": []string{l.String()}, "endpoint": endpoint}, u, epoch)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revocation: %d %s, want 204", rec.Code, rec.Body.String())
	}
	if !labelRevoked(t, pool, u, l) {
		t.Error("the label is not revoked")
	}
	if subscriptionExists(t, pool, labelled) || subscriptionExists(t, pool, unlabelled) {
		t.Error("a subscription of the revoked device remained")
	}
	if !subscriptionExists(t, pool, kept) {
		t.Error("the subscription with another label was deleted")
	}

	fresh := uuid.New()
	rec = p.serve(http.MethodPost, "/ui/v1/push/device-revocations",
		map[string]any{"expectedUserId": u.String(), "deviceLabels": []string{fresh.String()}}, u, epoch)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revocation without subscriptions: %d %s, want 204", rec.Code, rec.Body.String())
	}
	if !labelRevoked(t, pool, u, fresh) {
		t.Error("a label without subscriptions is not revoked")
	}
}
