package handler

// push_test.go — the push REST contract v2 at the handler (integration,
// TEST_DATABASE_URL). The reference is the push REST contract of the plan
// (operations, refusal codes, forms of a body, meaning of 204) and the
// specification: INV-9, INV-12, SC-009, R-6, Edge 7, the transitional
// contract and Observability.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/push"
	"skein-api/internal/testsupport"
)

type pushRig struct {
	pool *pgxpool.Pool
	h    *PushHandler
}

func newPushRig(t *testing.T) pushRig {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	return pushRig{pool: pool, h: NewPushHandler(pool, push.NewStore(pool), "pub", true)}
}

// pushUser is a seeded user and the session epoch of its session.
type pushUser struct {
	id    uuid.UUID
	epoch int64
}

func (rg pushRig) user(t *testing.T) pushUser {
	t.Helper()
	id, _ := testsupport.SeedUser(t, rg.pool, "user")
	var epoch int64
	if err := rg.pool.QueryRow(context.Background(), `SELECT session_epoch FROM users WHERE id = $1`, id).Scan(&epoch); err != nil {
		t.Fatalf("read session epoch: %v", err)
	}
	return pushUser{id: id, epoch: epoch}
}

// sub seeds a subscription of owner (label nil: none) at version 1 with the
// keys seed-p256dh / seed-auth and returns its id and endpoint.
func (rg pushRig) sub(t *testing.T, owner uuid.UUID, label *uuid.UUID) (uuid.UUID, string) {
	t.Helper()
	id := uuid.New()
	endpoint := "https://push.test.local/" + uuid.NewString()
	if _, err := rg.pool.Exec(context.Background(), `
INSERT INTO push_subscriptions (id, user_id, endpoint, p256dh, auth, device_label)
VALUES ($1, $2, $3, 'seed-p256dh', 'seed-auth', $4)`, id, owner, endpoint, label); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
	return id, endpoint
}

func (rg pushRig) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := rg.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// setVersion stands for writes that raised the version of a subscription.
func (rg pushRig) setVersion(t *testing.T, id uuid.UUID, version int64) {
	t.Helper()
	if _, err := rg.pool.Exec(context.Background(), `UPDATE push_subscriptions SET version = $2 WHERE id = $1`, id, version); err != nil {
		t.Fatalf("set version: %v", err)
	}
}

func (rg pushRig) exists(t *testing.T, id uuid.UUID) bool {
	t.Helper()
	return rg.count(t, `SELECT count(*) FROM push_subscriptions WHERE id = $1`, id) == 1
}

func (rg pushRig) revoked(t *testing.T, user, label uuid.UUID) bool {
	t.Helper()
	return rg.count(t, `SELECT count(*) FROM push_device_revocations WHERE user_id = $1 AND device_label = $2`, user, label) == 1
}

// snapshot renders every subscription and revocation of the users, sorted,
// for an «unchanged» comparison.
func (rg pushRig) snapshot(t *testing.T, users ...uuid.UUID) string {
	t.Helper()
	ctx := context.Background()
	var lines []string
	rows, err := rg.pool.Query(ctx, `
SELECT id, user_id, endpoint, p256dh, auth, coalesce(device_label::text, ''), version
  FROM push_subscriptions WHERE user_id = ANY($1)`, users)
	if err != nil {
		t.Fatalf("read subscriptions: %v", err)
	}
	for rows.Next() {
		var id, owner uuid.UUID
		var endpoint, p, a, label string
		var version int64
		if err := rows.Scan(&id, &owner, &endpoint, &p, &a, &label, &version); err != nil {
			t.Fatalf("scan: %v", err)
		}
		lines = append(lines, fmt.Sprintf("sub %s %s %s %s %s %s %d", id, owner, endpoint, p, a, label, version))
	}
	rows.Close()
	revs, err := rg.pool.Query(ctx, `SELECT user_id, device_label FROM push_device_revocations WHERE user_id = ANY($1)`, users)
	if err != nil {
		t.Fatalf("read revocations: %v", err)
	}
	for revs.Next() {
		var u, l uuid.UUID
		if err := revs.Scan(&u, &l); err != nil {
			t.Fatalf("scan: %v", err)
		}
		lines = append(lines, fmt.Sprintf("rev %s %s", u, l))
	}
	revs.Close()
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// do calls fn as user u with the checked session epoch epoch. body is sent
// as is when it is a string and as JSON otherwise; nil sends no body. id,
// when not empty, is the {id} path parameter.
func (rg pushRig) do(t *testing.T, fn http.HandlerFunc, method, target string, body any, u uuid.UUID, epoch int64, id string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	switch b := body.(type) {
	case nil:
		reader = bytes.NewReader(nil)
	case string:
		reader = bytes.NewReader([]byte(b))
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, target, reader)
	ctx := auth.WithSessionEpoch(auth.WithUserRole(auth.WithUserID(req.Context(), u), "user"), epoch)
	req = req.WithContext(ctx)
	if id != "" {
		req = withChiID(req, id)
	}
	rec := httptest.NewRecorder()
	fn(rec, req)
	return rec
}

func errorCode(rec *httptest.ResponseRecorder) string {
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	code, _ := body["error"].(string)
	return code
}

// withChiID stuffs the {id} path param into the request context so calling the
// handler directly mirrors what the chi router injects in production.
func withChiID(req *http.Request, id string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func newForm(expected uuid.UUID, endpoint string, label uuid.UUID, mode string) map[string]any {
	return map[string]any{
		"endpoint": endpoint, "keys": map[string]string{"p256dh": "new-p256dh", "auth": "new-auth"},
		"expectedUserId": expected.String(), "deviceLabel": label.String(), "mode": mode,
	}
}

// withTail renders body as JSON followed by tail, a body that is valid up
// to its end.
func withTail(t *testing.T, body any, tail string) string {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw) + tail
}

// Row 12 — INV-12, SC-009, Edge 7, R-6: every page operation whose
// expectedUserId is not the caller answers 422 expected_user_mismatch and
// changes nothing; the list is not returned.
func TestPush_ExpectedUserMismatchChangesNothing(t *testing.T) {
	rg := newPushRig(t)
	u := rg.user(t)
	other := rg.user(t)
	label := uuid.New()
	ownID, ownEndpoint := rg.sub(t, u.id, &label)

	cases := []struct {
		name   string
		fn     http.HandlerFunc
		method string
		target string
		body   any
	}{
		{"register on the own endpoint", rg.h.Subscribe, http.MethodPost, "/push/subscriptions", newForm(other.id, ownEndpoint, label, push.ModeUpsert)},
		{"register on a new endpoint", rg.h.Subscribe, http.MethodPost, "/push/subscriptions", newForm(other.id, "https://push.test.local/"+uuid.NewString(), uuid.New(), push.ModeUpsert)},
		{"remove a channel", rg.h.Unsubscribe, http.MethodDelete, "/push/subscriptions", map[string]any{"expectedUserId": other.id.String(), "endpoint": ownEndpoint}},
		{"revoke the device", rg.h.RevokeDevice, http.MethodPost, "/push/device-revocations", map[string]any{"expectedUserId": other.id.String(), "deviceLabels": []string{label.String()}, "endpoint": ownEndpoint}},
		{"remove the device by id", rg.h.RemoveDevice, http.MethodPost, "/push/device-removals", map[string]any{"expectedUserId": other.id.String(), "id": ownID.String()}},
		{"read the list", rg.h.List, http.MethodGet, "/push/subscriptions?expectedUserId=" + other.id.String(), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := rg.snapshot(t, u.id, other.id)
			rec := rg.do(t, c.fn, c.method, c.target, c.body, u.id, u.epoch, "")
			if rec.Code != http.StatusUnprocessableEntity || errorCode(rec) != push.CodeExpectedUserMismatch {
				t.Fatalf("answered %d %s, want 422 %s", rec.Code, rec.Body.String(), push.CodeExpectedUserMismatch)
			}
			if strings.HasPrefix(strings.TrimSpace(rec.Body.String()), "[") || strings.Contains(rec.Body.String(), ownID.String()) {
				t.Errorf("the refusal carries the list: %s", rec.Body.String())
			}
			if after := rg.snapshot(t, u.id, other.id); after != before {
				t.Errorf("state changed:\nbefore\n%s\nafter\n%s", before, after)
			}
		})
	}
}

// Row 13 — refusal table of the push REST contract and INV-9 (responses):
// status and code of every refusal of every operation; no body carries the
// endpoint, the keys or the id of an owner.
func TestPush_RefusalsByOperation(t *testing.T) {
	rg := newPushRig(t)
	u := rg.user(t)
	other := rg.user(t)
	stale := u.epoch + 1
	revokedLabel := uuid.New()
	testsupport.SeedPushDeviceRevocation(t, rg.pool, u.id, revokedLabel)
	_, foreignEndpoint := rg.sub(t, other.id, nil)
	ownLabel := uuid.New()
	ownID, ownEndpoint := rg.sub(t, u.id, &ownLabel)
	freshEndpoint := func() string { return "https://push.test.local/" + uuid.NewString() }
	me := u.id.String()

	type call struct {
		name   string
		fn     http.HandlerFunc
		method string
		body   any
		epoch  int64
		status int
		code   string
	}
	post, del := http.MethodPost, http.MethodDelete
	calls := []call{
		// POST /push/subscriptions
		{"register: endpoint of another user", rg.h.Subscribe, post, newForm(u.id, foreignEndpoint, uuid.New(), push.ModeUpsert), u.epoch, 422, push.CodeEndpointTaken},
		{"register: revoked label", rg.h.Subscribe, post, newForm(u.id, freshEndpoint(), revokedLabel, push.ModeUpsert), u.epoch, 422, push.CodeDeviceRevoked},
		{"register: updateOnly without an own subscription", rg.h.Subscribe, post, newForm(u.id, freshEndpoint(), uuid.New(), push.ModeUpdateOnly), u.epoch, 422, push.CodeSubscriptionNotFound},
		{"register: stale session", rg.h.Subscribe, post, newForm(u.id, ownEndpoint, ownLabel, push.ModeUpsert), stale, 422, push.CodeSessionStale},
		{"register: keys missing", rg.h.Subscribe, post, map[string]any{"endpoint": freshEndpoint(), "expectedUserId": me, "deviceLabel": uuid.NewString(), "mode": push.ModeUpsert}, u.epoch, 422, push.CodeInvalidSubscription},
		{"register: endpoint not https", rg.h.Subscribe, post, newForm(u.id, "http://push.test.local/x", uuid.New(), push.ModeUpsert), u.epoch, 422, push.CodeInvalidSubscription},
		{"register: expectedUserId not a UUID", rg.h.Subscribe, post, map[string]any{"endpoint": freshEndpoint(), "expectedUserId": "nope", "deviceLabel": uuid.NewString(), "mode": push.ModeUpsert}, u.epoch, 400, push.CodeInvalidRequest},
		{"register: deviceLabel not a UUID", rg.h.Subscribe, post, map[string]any{"endpoint": freshEndpoint(), "expectedUserId": me, "deviceLabel": "nope", "mode": push.ModeUpsert}, u.epoch, 400, push.CodeInvalidRequest},
		{"register: new form without mode", rg.h.Subscribe, post, map[string]any{"endpoint": freshEndpoint(), "expectedUserId": me, "deviceLabel": uuid.NewString()}, u.epoch, 400, push.CodeInvalidRequest},
		{"register: unknown mode", rg.h.Subscribe, post, newForm(u.id, freshEndpoint(), uuid.New(), "create"), u.epoch, 400, push.CodeInvalidRequest},
		{"register: deviceLabel without expectedUserId", rg.h.Subscribe, post, map[string]any{"endpoint": freshEndpoint(), "deviceLabel": uuid.NewString()}, u.epoch, 400, push.CodeInvalidRequest},
		{"register: mode without expectedUserId", rg.h.Subscribe, post, map[string]any{"endpoint": freshEndpoint(), "mode": push.ModeUpsert}, u.epoch, 400, push.CodeInvalidRequest},
		{"register: empty expectedUserId", rg.h.Subscribe, post, map[string]any{"endpoint": ownEndpoint, "keys": map[string]string{"p256dh": "new-p256dh", "auth": "new-auth"}, "expectedUserId": "", "deviceLabel": ownLabel.String(), "mode": push.ModeUpsert}, u.epoch, 400, push.CodeInvalidRequest},
		{"register: null expectedUserId", rg.h.Subscribe, post, map[string]any{"endpoint": ownEndpoint, "keys": map[string]string{"p256dh": "new-p256dh", "auth": "new-auth"}, "expectedUserId": nil, "deviceLabel": ownLabel.String(), "mode": push.ModeUpsert}, u.epoch, 400, push.CodeInvalidRequest},
		{"register: null expectedUserId without label and mode", rg.h.Subscribe, post, map[string]any{"endpoint": ownEndpoint, "keys": map[string]string{"p256dh": "new-p256dh", "auth": "new-auth"}, "expectedUserId": nil}, u.epoch, 400, push.CodeInvalidRequest},
		{"register: null deviceLabel without expectedUserId", rg.h.Subscribe, post, map[string]any{"endpoint": ownEndpoint, "keys": map[string]string{"p256dh": "new-p256dh", "auth": "new-auth"}, "deviceLabel": nil}, u.epoch, 400, push.CodeInvalidRequest},
		{"register: malformed JSON", rg.h.Subscribe, post, `{"endpoint":`, u.epoch, 400, push.CodeInvalidJSON},
		{"register: unknown field", rg.h.Subscribe, post, `{"endpoint":"https://push.test.local/x","owner":"x"}`, u.epoch, 400, push.CodeInvalidJSON},
		{"register: a second JSON value after the body", rg.h.Subscribe, post, withTail(t, newForm(u.id, freshEndpoint(), uuid.New(), push.ModeUpsert), `{"mode":"upsert"}`), u.epoch, 400, push.CodeInvalidJSON},
		{"register: garbage after the body", rg.h.Subscribe, post, withTail(t, newForm(u.id, freshEndpoint(), uuid.New(), push.ModeUpsert), ` x`), u.epoch, 400, push.CodeInvalidJSON},
		// DELETE /push/subscriptions
		{"remove channel: endpoint missing", rg.h.Unsubscribe, del, map[string]any{"expectedUserId": me}, u.epoch, 422, push.CodeInvalidSubscription},
		{"remove channel: stale session", rg.h.Unsubscribe, del, map[string]any{"expectedUserId": me, "endpoint": ownEndpoint}, stale, 422, push.CodeSessionStale},
		{"remove channel: ifSubscription without expectedUserId", rg.h.Unsubscribe, del, map[string]any{"endpoint": ownEndpoint, "ifSubscription": map[string]any{"id": ownID.String(), "version": 1}}, u.epoch, 400, push.CodeInvalidRequest},
		{"remove channel: ifSubscription.id not a UUID", rg.h.Unsubscribe, del, map[string]any{"expectedUserId": me, "endpoint": ownEndpoint, "ifSubscription": map[string]any{"id": "nope", "version": 1}}, u.epoch, 400, push.CodeInvalidRequest},
		{"remove channel: empty expectedUserId", rg.h.Unsubscribe, del, map[string]any{"expectedUserId": "", "endpoint": ownEndpoint}, u.epoch, 400, push.CodeInvalidRequest},
		{"remove channel: null expectedUserId", rg.h.Unsubscribe, del, map[string]any{"expectedUserId": nil, "endpoint": ownEndpoint}, u.epoch, 400, push.CodeInvalidRequest},
		{"remove channel: null ifSubscription", rg.h.Unsubscribe, del, map[string]any{"expectedUserId": me, "endpoint": ownEndpoint, "ifSubscription": nil}, u.epoch, 400, push.CodeInvalidRequest},
		{"remove channel: null ifSubscription without expectedUserId", rg.h.Unsubscribe, del, map[string]any{"endpoint": ownEndpoint, "ifSubscription": nil}, u.epoch, 400, push.CodeInvalidRequest},
		{"remove channel: malformed JSON", rg.h.Unsubscribe, del, `{`, u.epoch, 400, push.CodeInvalidJSON},
		{"remove channel: data after the body", rg.h.Unsubscribe, del, withTail(t, map[string]any{"expectedUserId": me, "endpoint": ownEndpoint}, `[]`), u.epoch, 400, push.CodeInvalidJSON},
		// POST /push/device-revocations
		{"revoke: stale session", rg.h.RevokeDevice, post, map[string]any{"expectedUserId": me, "deviceLabels": []string{ownLabel.String()}}, stale, 422, push.CodeSessionStale},
		{"revoke: empty revocation", rg.h.RevokeDevice, post, map[string]any{"expectedUserId": me, "deviceLabels": []string{}}, u.epoch, 400, push.CodeInvalidRequest},
		{"revoke: label not a UUID", rg.h.RevokeDevice, post, map[string]any{"expectedUserId": me, "deviceLabels": []string{"nope"}}, u.epoch, 400, push.CodeInvalidRequest},
		{"revoke: without expectedUserId", rg.h.RevokeDevice, post, map[string]any{"deviceLabels": []string{ownLabel.String()}}, u.epoch, 400, push.CodeInvalidRequest},
		{"revoke: malformed JSON", rg.h.RevokeDevice, post, `[`, u.epoch, 400, push.CodeInvalidJSON},
		{"revoke: data after the body", rg.h.RevokeDevice, post, withTail(t, map[string]any{"expectedUserId": me, "deviceLabels": []string{ownLabel.String()}}, `1`), u.epoch, 400, push.CodeInvalidJSON},
		// POST /push/device-removals
		{"remove device: another user's or missing id", rg.h.RemoveDevice, post, map[string]any{"expectedUserId": me, "id": uuid.NewString()}, u.epoch, 422, push.CodeSubscriptionNotFound},
		{"remove device: stale session", rg.h.RemoveDevice, post, map[string]any{"expectedUserId": me, "id": ownID.String()}, stale, 422, push.CodeSessionStale},
		{"remove device: id not a UUID", rg.h.RemoveDevice, post, map[string]any{"expectedUserId": me, "id": "nope"}, u.epoch, 400, push.CodeInvalidRequest},
		{"remove device: malformed JSON", rg.h.RemoveDevice, post, `{"id"`, u.epoch, 400, push.CodeInvalidJSON},
		{"remove device: data after the body", rg.h.RemoveDevice, post, withTail(t, map[string]any{"expectedUserId": me, "id": ownID.String()}, `{}`), u.epoch, 400, push.CodeInvalidJSON},
	}
	secrets := []string{foreignEndpoint, ownEndpoint, "seed-p256dh", "seed-auth", "new-p256dh", "new-auth", other.id.String()}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			before := rg.snapshot(t, u.id, other.id)
			rec := rg.do(t, c.fn, c.method, "/push", c.body, u.id, c.epoch, "")
			if rec.Code != c.status || errorCode(rec) != c.code {
				t.Fatalf("answered %d %s, want %d %s", rec.Code, rec.Body.String(), c.status, c.code)
			}
			for _, s := range secrets {
				if strings.Contains(rec.Body.String(), s) {
					t.Errorf("the refusal body carries %q: %s", s, rec.Body.String())
				}
			}
			if after := rg.snapshot(t, u.id, other.id); after != before {
				t.Errorf("a refused call changed the state:\nbefore\n%s\nafter\n%s", before, after)
			}
		})
	}

	t.Run("200 subscriptionDTO carries neither the endpoint nor the keys nor an owner", func(t *testing.T) {
		endpoint := freshEndpoint()
		label := uuid.New()
		rec := rg.do(t, rg.h.Subscribe, post, "/push", newForm(u.id, endpoint, label, push.ModeUpsert), u.id, u.epoch, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("answered %d %s, want 200", rec.Code, rec.Body.String())
		}
		for _, s := range []string{endpoint, "new-p256dh", "new-auth", "p256dh", `"auth"`, me} {
			if strings.Contains(rec.Body.String(), s) {
				t.Errorf("the 200 body carries %q: %s", s, rec.Body.String())
			}
		}
		var dto push.SubscriptionDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
			t.Fatal(err)
		}
		if dto.DeviceLabel == nil || *dto.DeviceLabel != label.String() || dto.Version != 1 {
			t.Errorf("dto = %+v, want label %s and version 1", dto, label)
		}
	})
}

// Row 13a — the boundary of one revocation: 1000 labels are revoked by one
// request, 1001 are refused and change nothing.
func TestPush_RevokeDeviceLabelBoundary(t *testing.T) {
	rg := newPushRig(t)
	u := rg.user(t)
	labels := func(n int) ([]string, []uuid.UUID) {
		raw := make([]string, n)
		ids := make([]uuid.UUID, n)
		for i := range ids {
			ids[i] = uuid.New()
			raw[i] = ids[i].String()
		}
		return raw, ids
	}

	raw, ids := labels(push.MaxDeviceLabels)
	first, _ := rg.sub(t, u.id, &ids[0])
	last, _ := rg.sub(t, u.id, &ids[len(ids)-1])
	rec := rg.do(t, rg.h.RevokeDevice, http.MethodPost, "/push/device-revocations",
		map[string]any{"expectedUserId": u.id.String(), "deviceLabels": raw}, u.id, u.epoch, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("1000 labels: answered %d %s, want 204", rec.Code, rec.Body.String())
	}
	if n := rg.count(t, `SELECT count(*) FROM push_device_revocations WHERE user_id = $1 AND device_label = ANY($2)`, u.id, ids); n != len(ids) {
		t.Errorf("revoked labels = %d, want %d", n, len(ids))
	}
	if rg.exists(t, first) || rg.exists(t, last) {
		t.Error("a subscription with a revoked label remained")
	}

	over, _ := labels(push.MaxDeviceLabels + 1)
	before := rg.snapshot(t, u.id)
	rec = rg.do(t, rg.h.RevokeDevice, http.MethodPost, "/push/device-revocations",
		map[string]any{"expectedUserId": u.id.String(), "deviceLabels": over}, u.id, u.epoch, "")
	if rec.Code != http.StatusBadRequest || errorCode(rec) != push.CodeInvalidRequest {
		t.Fatalf("1001 labels: answered %d %s, want 400 %s", rec.Code, rec.Body.String(), push.CodeInvalidRequest)
	}
	if after := rg.snapshot(t, u.id); after != before {
		t.Error("a refused revocation changed the state")
	}
}

// Row 14 — INV-9 «the answer of a removal by endpoint is the same»: the new
// form of DELETE /push/subscriptions answers alike whether there is no
// subscription, another user's, the caller's own, or a guard with an old
// version.
func TestPush_RemoveChannelAnswersAlike(t *testing.T) {
	rg := newPushRig(t)
	u := rg.user(t)
	other := rg.user(t)
	_, foreign := rg.sub(t, other.id, nil)
	_, own := rg.sub(t, u.id, nil)
	guardedID, guarded := rg.sub(t, other.id, nil)
	rg.setVersion(t, guardedID, 2)

	bodies := map[string]map[string]any{
		"no subscription":           {"expectedUserId": u.id.String(), "endpoint": "https://push.test.local/" + uuid.NewString()},
		"another user's":            {"expectedUserId": u.id.String(), "endpoint": foreign},
		"the caller's own":          {"expectedUserId": u.id.String(), "endpoint": own},
		"guard with an old version": {"expectedUserId": u.id.String(), "endpoint": guarded, "ifSubscription": map[string]any{"id": guardedID.String(), "version": 1}},
	}
	var refHeader http.Header
	var refName string
	for name, body := range bodies {
		rec := rg.do(t, rg.h.Unsubscribe, http.MethodDelete, "/push/subscriptions", body, u.id, u.epoch, "")
		if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
			t.Errorf("%s: answered %d with %d body bytes, want 204 and an empty body", name, rec.Code, rec.Body.Len())
		}
		if refHeader == nil {
			refHeader, refName = rec.Header(), name
			continue
		}
		if fmt.Sprint(rec.Header()) != fmt.Sprint(refHeader) {
			t.Errorf("headers of %q = %v, of %q = %v; want the same", name, rec.Header(), refName, refHeader)
		}
	}
}

// Row 14a — the guarded removal through the handler (composition rule
// item 6): ifSubscription reaches the store.
func TestPush_RemoveChannelGuard(t *testing.T) {
	rg := newPushRig(t)
	u := rg.user(t)
	owner := rg.user(t)
	remove := func(endpoint string, id uuid.UUID, version int64) {
		t.Helper()
		rec := rg.do(t, rg.h.Unsubscribe, http.MethodDelete, "/push/subscriptions", map[string]any{
			"expectedUserId": u.id.String(), "endpoint": endpoint,
			"ifSubscription": map[string]any{"id": id.String(), "version": version},
		}, u.id, u.epoch, "")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("answered %d %s, want 204", rec.Code, rec.Body.String())
		}
	}

	id, endpoint := rg.sub(t, owner.id, nil)
	rg.setVersion(t, id, 3)
	remove(endpoint, id, 2)
	if !rg.exists(t, id) {
		t.Fatal("an old version removed the subscription")
	}
	remove(endpoint, uuid.New(), 3)
	if !rg.exists(t, id) {
		t.Fatal("another id at the same version removed the subscription")
	}
	remove(endpoint, id, 3)
	if rg.exists(t, id) {
		t.Error("the matching id and version kept the subscription")
	}
}

// Row 15 — transitional contract «new api + old page or worker»: the old
// POST body only refreshes the caller's existing subscription; the old
// DELETE body removes only the caller's own, without revocation.
func TestPush_OldForms(t *testing.T) {
	rg := newPushRig(t)
	u := rg.user(t)
	other := rg.user(t)
	label := uuid.New()
	ownID, own := rg.sub(t, u.id, &label)
	_, foreign := rg.sub(t, other.id, nil)
	old := func(endpoint string) map[string]any {
		return map[string]any{"endpoint": endpoint, "keys": map[string]string{"p256dh": "old-p256dh", "auth": "old-auth"}}
	}

	rec := rg.do(t, rg.h.Subscribe, http.MethodPost, "/push/subscriptions", old(own), u.id, u.epoch, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("old POST on the own subscription: %d %s, want 200", rec.Code, rec.Body.String())
	}
	var p256dh string
	var stored *uuid.UUID
	if err := rg.pool.QueryRow(context.Background(), `SELECT p256dh, device_label FROM push_subscriptions WHERE id = $1`, ownID).Scan(&p256dh, &stored); err != nil {
		t.Fatal(err)
	}
	if p256dh != "old-p256dh" || stored == nil || *stored != label {
		t.Errorf("after old POST: p256dh %q, label %v; want old-p256dh and the label kept", p256dh, stored)
	}

	fresh := "https://push.test.local/" + uuid.NewString()
	if rec := rg.do(t, rg.h.Subscribe, http.MethodPost, "/push/subscriptions", old(fresh), u.id, u.epoch, ""); rec.Code != 422 || errorCode(rec) != push.CodeSubscriptionNotFound {
		t.Errorf("old POST without an own subscription: %d %s, want 422 %s", rec.Code, rec.Body.String(), push.CodeSubscriptionNotFound)
	}
	if rg.count(t, `SELECT count(*) FROM push_subscriptions WHERE endpoint = $1`, fresh) != 0 {
		t.Error("old POST created a subscription")
	}
	if rec := rg.do(t, rg.h.Subscribe, http.MethodPost, "/push/subscriptions", old(foreign), u.id, u.epoch, ""); rec.Code != 422 || errorCode(rec) != push.CodeEndpointTaken {
		t.Errorf("old POST on another user's endpoint: %d %s, want 422 %s", rec.Code, rec.Body.String(), push.CodeEndpointTaken)
	}

	if rec := rg.do(t, rg.h.Unsubscribe, http.MethodDelete, "/push/subscriptions", map[string]any{"endpoint": foreign}, u.id, u.epoch, ""); rec.Code != http.StatusNoContent {
		t.Errorf("old DELETE on another user's endpoint: %d, want 204", rec.Code)
	}
	if rg.count(t, `SELECT count(*) FROM push_subscriptions WHERE endpoint = $1`, foreign) != 1 {
		t.Error("old DELETE removed another user's subscription")
	}
	if rec := rg.do(t, rg.h.Unsubscribe, http.MethodDelete, "/push/subscriptions", map[string]any{"endpoint": own}, u.id, u.epoch, ""); rec.Code != http.StatusNoContent {
		t.Errorf("old DELETE on the own endpoint: %d, want 204", rec.Code)
	}
	if rg.exists(t, ownID) {
		t.Error("old DELETE kept the caller's own subscription")
	}
	if rg.revoked(t, u.id, label) {
		t.Error("old DELETE revoked the label")
	}
}

// Row 16 — the old DELETE /push/subscriptions/{id}: removes the caller's
// own; a repeat and another user's id are 422 subscription_not_found (was
// 404); the label of the removed subscription is revoked; a malformed id
// is 400.
func TestPush_DeleteByID(t *testing.T) {
	rg := newPushRig(t)
	u := rg.user(t)
	other := rg.user(t)
	label := uuid.New()
	id := testsupport.SeedLabeledPushSubscription(t, rg.pool, u.id, label)
	del := func(as pushUser, raw string) *httptest.ResponseRecorder {
		return rg.do(t, rg.h.DeleteByID, http.MethodDelete, "/push/subscriptions/"+raw, nil, as.id, as.epoch, raw)
	}

	if rec := del(u, "not-a-uuid"); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed id: %d, want 400", rec.Code)
	}
	if rec := del(other, id.String()); rec.Code != 422 || errorCode(rec) != push.CodeSubscriptionNotFound {
		t.Fatalf("another user's id: %d %s, want 422 %s", rec.Code, rec.Body.String(), push.CodeSubscriptionNotFound)
	}
	if !rg.exists(t, id) {
		t.Fatal("another user removed the subscription")
	}
	if rec := del(u, id.String()); rec.Code != http.StatusNoContent {
		t.Fatalf("owner: %d %s, want 204", rec.Code, rec.Body.String())
	}
	if rg.exists(t, id) {
		t.Error("the subscription remained")
	}
	if !rg.revoked(t, u.id, label) {
		t.Error("the label of the removed subscription is not revoked")
	}
	if rec := del(u, id.String()); rec.Code != 422 || errorCode(rec) != push.CodeSubscriptionNotFound {
		t.Errorf("repeat: %d %s, want 422 %s", rec.Code, rec.Body.String(), push.CodeSubscriptionNotFound)
	}
}

// Row 17 — the list (INV-9 «the endpoint is not returned», additive
// fields): without the query (older pages) and with expectedUserId it
// returns endpointHash, deviceLabel and version, never the endpoint and the
// keys.
func TestPush_ListExposesEndpointHashNotEndpoint(t *testing.T) {
	rg := newPushRig(t)
	u := rg.user(t)
	label := uuid.New()
	id := testsupport.SeedLabeledPushSubscription(t, rg.pool, u.id, label)
	var endpoint string
	if err := rg.pool.QueryRow(context.Background(), `SELECT endpoint FROM push_subscriptions WHERE id = $1`, id).Scan(&endpoint); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(endpoint))
	wantHash := hex.EncodeToString(sum[:])

	for _, target := range []string{"/push/subscriptions", "/push/subscriptions?expectedUserId=" + u.id.String()} {
		rec := rg.do(t, rg.h.List, http.MethodGet, target, nil, u.id, u.epoch, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s, want 200", target, rec.Code, rec.Body.String())
		}
		raw := rec.Body.String()
		for _, leaked := range []string{endpoint, "fake-p256dh", "fake-auth", "p256dh", `"auth"`} {
			if strings.Contains(raw, leaked) {
				t.Errorf("%s: the list carries %q: %s", target, leaked, raw)
			}
		}
		var got []push.SubscriptionDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(got) != 1 || got[0].EndpointHash != wantHash || got[0].DeviceLabel == nil ||
			*got[0].DeviceLabel != label.String() || got[0].Version != 1 {
			t.Errorf("%s: list = %+v, want one item with hash %s, label %s, version 1", target, got, wantHash, label)
		}
	}
}

// logCapture is a slog handler that keeps the attributes of every record.
type logCapture struct {
	mu      sync.Mutex
	records []map[string]string
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	attrs := map[string]string{"msg": r.Message}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = fmt.Sprint(a.Value.Any())
		return true
	})
	c.mu.Lock()
	c.records = append(c.records, attrs)
	c.mu.Unlock()
	return nil
}

func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }

// of returns the records of user (by userId or callerId) carrying key.
func (c *logCapture) of(user uuid.UUID, key string) []map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]string
	for _, r := range c.records {
		if r["userId"] != user.String() && r["callerId"] != user.String() {
			continue
		}
		if _, ok := r[key]; ok {
			out = append(out, r)
		}
	}
	return out
}

// Row 18 — Observability and INV-9 (logs): a success is logged with the
// named fields after the store returned; refusals with the caller and the
// code; no field of any record carries the endpoint, its token, or the
// owner in a refusal; a refused write leaves no record of a success.
func TestPush_LogRecords(t *testing.T) {
	rg := newPushRig(t)
	logs := &logCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(logs))
	t.Cleanup(func() { slog.SetDefault(prev) })

	u := rg.user(t)
	owner := rg.user(t)
	label := uuid.New()
	_, labelled := rg.sub(t, u.id, &label)
	_, foreign := rg.sub(t, owner.id, nil)
	byID, byIDEndpoint := rg.sub(t, u.id, nil)
	endpoints := []string{labelled, foreign, byIDEndpoint}

	t.Run("a refused write leaves no record of a success", func(t *testing.T) {
		rec := rg.do(t, rg.h.RevokeDevice, http.MethodPost, "/push/device-revocations",
			map[string]any{"expectedUserId": u.id.String(), "deviceLabels": []string{label.String()}}, u.id, u.epoch+1, "")
		if errorCode(rec) != push.CodeSessionStale {
			t.Fatalf("answered %d %s, want session_stale", rec.Code, rec.Body.String())
		}
		if got := logs.of(u.id, "deviceLabel"); len(got) != 0 {
			t.Errorf("records of a revocation after session_stale: %v", got)
		}
	})
	t.Run("revocation: userId, deviceLabel, number", func(t *testing.T) {
		rg.do(t, rg.h.RevokeDevice, http.MethodPost, "/push/device-revocations",
			map[string]any{"expectedUserId": u.id.String(), "deviceLabels": []string{label.String()}}, u.id, u.epoch, "")
		got := logs.of(u.id, "deviceLabel")
		if len(got) != 1 || !strings.Contains(got[0]["deviceLabel"], label.String()) || got[0]["removed"] != "1" {
			t.Errorf("revocation records = %v, want one with the label and removed 1", got)
		}
	})
	t.Run("removal of another user's channel: callerId, ownerId", func(t *testing.T) {
		rg.do(t, rg.h.Unsubscribe, http.MethodDelete, "/push/subscriptions",
			map[string]any{"expectedUserId": u.id.String(), "endpoint": foreign}, u.id, u.epoch, "")
		got := logs.of(u.id, "ownerId")
		if len(got) != 1 || got[0]["ownerId"] != owner.id.String() {
			t.Errorf("channel removal records = %v, want one with ownerId %s", got, owner.id)
		}
	})
	t.Run("removal by id", func(t *testing.T) {
		rg.do(t, rg.h.RemoveDevice, http.MethodPost, "/push/device-removals",
			map[string]any{"expectedUserId": u.id.String(), "id": byID.String()}, u.id, u.epoch, "")
		got := logs.of(u.id, "subscriptionId")
		if len(got) != 1 || got[0]["subscriptionId"] != byID.String() {
			t.Errorf("removal records = %v, want one with subscriptionId %s", got, byID)
		}
	})
	t.Run("refusals of a registration and of the expected user: userId and code, no owner", func(t *testing.T) {
		_, taken := rg.sub(t, owner.id, nil)
		endpoints = append(endpoints, taken)
		rg.do(t, rg.h.Subscribe, http.MethodPost, "/push/subscriptions", newForm(u.id, taken, uuid.New(), push.ModeUpsert), u.id, u.epoch, "")
		rg.do(t, rg.h.Subscribe, http.MethodPost, "/push/subscriptions", newForm(u.id, "https://push.test.local/"+uuid.NewString(), label, push.ModeUpsert), u.id, u.epoch, "")
		rg.do(t, rg.h.List, http.MethodGet, "/push/subscriptions?expectedUserId="+owner.id.String(), nil, u.id, u.epoch, "")
		codes := map[string]bool{}
		for _, r := range logs.of(u.id, "code") {
			codes[r["code"]] = true
			for k, v := range r {
				if strings.Contains(v, owner.id.String()) {
					t.Errorf("refusal record field %s carries the owner: %v", k, r)
				}
			}
		}
		for _, want := range []string{push.CodeEndpointTaken, push.CodeDeviceRevoked, push.CodeExpectedUserMismatch} {
			if !codes[want] {
				t.Errorf("no refusal record with code %s; codes %v", want, codes)
			}
		}
	})

	logs.mu.Lock()
	defer logs.mu.Unlock()
	for _, r := range logs.records {
		for k, v := range r {
			for _, e := range endpoints {
				token := e[strings.LastIndex(e, "/")+1:]
				if strings.Contains(v, e) || strings.Contains(v, token) {
					t.Errorf("record field %s carries an endpoint or its token: %v", k, r)
				}
			}
		}
	}
}
