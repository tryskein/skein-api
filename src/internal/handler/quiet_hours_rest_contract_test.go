package handler

// quiet_hours_rest_contract_test.go — the HTTP contract of the caller's time
// zone and quiet window (/ui/v1/me/timezone, /ui/v1/me/quiet-hours) against the
// migrated schema.
//
// The handler is mounted on the same paths as in buildRouter (the /me route
// group), behind the real ParseUserHeaders and RequireUser middleware, with
// the identity headers skein-web sets. Every write is followed by a separate
// GET, so the test sees the stored state, not the echo of the write.
//
// Reference: the swag annotations of GetTimezone, PutTimezone, GetQuietHours
// and PutQuietHours (quiet_hours.go), which feed the internal OpenAPI
// document:
//   - both zone fields are null for an account that has never set one, and
//     start/end are null in the factory state (quiet hours off);
//   - source "manual" is the default; "detected" writes only while no
//     hand-set value exists, and the answer carries the zone in effect;
//   - an end earlier than the start is accepted (the window crosses midnight);
//     both bounds null clear the window;
//   - 422 codes: invalid_timezone, invalid_source, invalid_time,
//     quiet_hours_half_set, quiet_hours_degenerate, timezone_required;
//     400 invalid_json.
// Only statuses and machine codes are asserted, never message text.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"skein-api/internal/auth"
	"skein-api/internal/testsupport"
)

// quietCaller serves the four routes for one signed-in user.
type quietCaller struct {
	router chi.Router
	user   uuid.UUID
	name   string
}

func newQuietCaller(t *testing.T) *quietCaller {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	user, name := testsupport.SeedUser(t, pool, "user")
	h := NewNotificationsHandler(pool)
	r := chi.NewRouter()
	r.Use(ParseUserHeaders)
	r.Route("/ui/v1/me", func(r chi.Router) {
		r.Use(RequireUser)
		r.Get("/timezone", h.GetTimezone)
		r.Put("/timezone", h.PutTimezone)
		r.Get("/quiet-hours", h.GetQuietHours)
		r.Put("/quiet-hours", h.PutQuietHours)
	})
	return &quietCaller{router: r, user: user, name: name}
}

// do sends one request as the caller and returns the status and the decoded
// JSON object. A string body is sent verbatim.
func (c *quietCaller) do(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var raw []byte
	switch b := body.(type) {
	case nil:
	case string:
		raw = []byte(b)
	default:
		var err error
		if raw, err = json.Marshal(b); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, "/ui/v1/me"+path, bytes.NewReader(raw))
	req.Header.Set(auth.HeaderUserID, c.user.String())
	req.Header.Set(auth.HeaderUsername, c.name)
	req.Header.Set(auth.HeaderUserRole, "user")
	rec := httptest.NewRecorder()
	c.router.ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: decode %q: %v", method, path, rec.Body.String(), err)
	}
	return rec.Code, out
}

// ok sends the request and requires 200.
func (c *quietCaller) ok(t *testing.T, method, path string, body any) map[string]any {
	t.Helper()
	code, out := c.do(t, method, path, body)
	if code != http.StatusOK {
		t.Fatalf("%s %s = %d %v, want 200", method, path, code, out)
	}
	return out
}

// refused sends the request and requires status want with machine code wantCode.
func (c *quietCaller) refused(t *testing.T, method, path string, body any, want int, wantCode string) {
	t.Helper()
	code, out := c.do(t, method, path, body)
	if code != want || out["error"] != wantCode {
		t.Fatalf("%s %s %v = %d %v, want %d %q", method, path, body, code, out, want, wantCode)
	}
}

// field asserts one key of a body: want nil means the key is present and JSON null.
func field(t *testing.T, where string, body map[string]any, key string, want any) {
	t.Helper()
	got, present := body[key]
	if !present {
		t.Errorf("%s: key %q is missing (body %v)", where, key, body)
		return
	}
	if got != want {
		t.Errorf("%s: %s = %#v, want %#v", where, key, got, want)
	}
}

// TestTimezoneREST_Roads pins the time zone contract in one ordered
// scenario: the factory state, detection while nothing is hand-set, a manual
// edit replacing detection, detection yielding to the hand-set value (the
// answer carries the zone in effect), and a manual edit replacing a manual one.
func TestTimezoneREST_Roads(t *testing.T) {
	c := newQuietCaller(t)
	expect := func(where string, body map[string]any, tz, source any) {
		t.Helper()
		field(t, where, body, "timezone", tz)
		field(t, where, body, "source", source)
	}

	expect("GET factory", c.ok(t, http.MethodGet, "/timezone", nil), nil, nil)

	expect("PUT detected", c.ok(t, http.MethodPut, "/timezone", map[string]any{"timezone": "Asia/Tokyo", "source": "detected"}), "Asia/Tokyo", "detected")
	expect("GET after detection", c.ok(t, http.MethodGet, "/timezone", nil), "Asia/Tokyo", "detected")

	expect("PUT without source", c.ok(t, http.MethodPut, "/timezone", map[string]any{"timezone": "Europe/Moscow"}), "Europe/Moscow", "manual")
	expect("GET after the manual edit", c.ok(t, http.MethodGet, "/timezone", nil), "Europe/Moscow", "manual")

	expect("PUT detected over manual", c.ok(t, http.MethodPut, "/timezone", map[string]any{"timezone": "America/New_York", "source": "detected"}), "Europe/Moscow", "manual")
	expect("GET after the yielded detection", c.ok(t, http.MethodGet, "/timezone", nil), "Europe/Moscow", "manual")

	expect("PUT manual over manual", c.ok(t, http.MethodPut, "/timezone", map[string]any{"timezone": "Europe/Berlin", "source": "manual"}), "Europe/Berlin", "manual")
	expect("GET after the second manual edit", c.ok(t, http.MethodGet, "/timezone", nil), "Europe/Berlin", "manual")
}

// TestTimezoneREST_Refusals — each refused write answers its status and
// machine code and leaves the stored zone as it was. The zone is set in the
// parent, so every case can be run on its own.
func TestTimezoneREST_Refusals(t *testing.T) {
	c := newQuietCaller(t)
	c.ok(t, http.MethodPut, "/timezone", map[string]any{"timezone": "Europe/Berlin", "source": "manual"})

	refusals := []struct {
		name     string
		body     any
		want     int
		wantCode string
	}{
		{"a plausible but unknown zone", map[string]any{"timezone": "Europe/Moskva"}, http.StatusUnprocessableEntity, "invalid_timezone"},
		{"the server's own zone", map[string]any{"timezone": "Local"}, http.StatusUnprocessableEntity, "invalid_timezone"},
		{"an empty zone", map[string]any{"timezone": ""}, http.StatusUnprocessableEntity, "invalid_timezone"},
		{"an unknown source", map[string]any{"timezone": "Asia/Tokyo", "source": "guessed"}, http.StatusUnprocessableEntity, "invalid_source"},
		{"a body that is not JSON", "{", http.StatusBadRequest, "invalid_json"},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			c.refused(t, http.MethodPut, "/timezone", tc.body, tc.want, tc.wantCode)
			got := c.ok(t, http.MethodGet, "/timezone", nil)
			field(t, "GET after the refusal", got, "timezone", "Europe/Berlin")
			field(t, "GET after the refusal", got, "source", "manual")
		})
	}
}

// TestQuietHoursREST_Lifecycle pins the quiet window in one ordered
// scenario: off by default, refused while no zone is stored, a window across
// midnight stored with its zone, cleared by two nulls with the zone kept.
func TestQuietHoursREST_Lifecycle(t *testing.T) {
	c := newQuietCaller(t)
	window := func(where string, body map[string]any, start, end, tz any) {
		t.Helper()
		field(t, where, body, "start", start)
		field(t, where, body, "end", end)
		field(t, where, body, "timezone", tz)
	}

	window("GET factory", c.ok(t, http.MethodGet, "/quiet-hours", nil), nil, nil, nil)

	c.refused(t, http.MethodPut, "/quiet-hours", map[string]any{"start": "22:00", "end": "07:00"},
		http.StatusUnprocessableEntity, "timezone_required")
	window("GET after timezone_required", c.ok(t, http.MethodGet, "/quiet-hours", nil), nil, nil, nil)

	c.ok(t, http.MethodPut, "/timezone", map[string]any{"timezone": "Europe/Moscow"})
	window("PUT across midnight", c.ok(t, http.MethodPut, "/quiet-hours", map[string]any{"start": "22:00", "end": "07:00"}), "22:00", "07:00", "Europe/Moscow")
	window("GET after the window", c.ok(t, http.MethodGet, "/quiet-hours", nil), "22:00", "07:00", "Europe/Moscow")

	window("PUT both null", c.ok(t, http.MethodPut, "/quiet-hours", map[string]any{"start": nil, "end": nil}), nil, nil, "Europe/Moscow")
	window("GET after the clear", c.ok(t, http.MethodGet, "/quiet-hours", nil), nil, nil, "Europe/Moscow")
}

// TestQuietHoursREST_Refusals — each refused window answers its status and
// machine code and leaves the stored window as it was. The zone and a window
// are set in the parent, so every case can be run on its own.
func TestQuietHoursREST_Refusals(t *testing.T) {
	c := newQuietCaller(t)
	c.ok(t, http.MethodPut, "/timezone", map[string]any{"timezone": "Europe/Moscow"})
	c.ok(t, http.MethodPut, "/quiet-hours", map[string]any{"start": "22:00", "end": "07:00"})

	refusals := []struct {
		name     string
		body     any
		want     int
		wantCode string
	}{
		{"start without end", map[string]any{"start": "22:00", "end": nil}, http.StatusUnprocessableEntity, "quiet_hours_half_set"},
		{"end without start", map[string]any{"start": nil, "end": "07:00"}, http.StatusUnprocessableEntity, "quiet_hours_half_set"},
		{"end omitted", map[string]any{"start": "22:00"}, http.StatusUnprocessableEntity, "quiet_hours_half_set"},
		{"equal bounds", map[string]any{"start": "08:00", "end": "08:00"}, http.StatusUnprocessableEntity, "quiet_hours_degenerate"},
		{"start hour out of range", map[string]any{"start": "24:00", "end": "07:00"}, http.StatusUnprocessableEntity, "invalid_time"},
		{"start without a leading zero", map[string]any{"start": "7:00", "end": "08:00"}, http.StatusUnprocessableEntity, "invalid_time"},
		{"end minute out of range", map[string]any{"start": "22:00", "end": "07:60"}, http.StatusUnprocessableEntity, "invalid_time"},
		{"end with seconds", map[string]any{"start": "22:00", "end": "07:00:00"}, http.StatusUnprocessableEntity, "invalid_time"},
		{"a body that is not JSON", "{", http.StatusBadRequest, "invalid_json"},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			c.refused(t, http.MethodPut, "/quiet-hours", tc.body, tc.want, tc.wantCode)
			got := c.ok(t, http.MethodGet, "/quiet-hours", nil)
			field(t, "GET after the refusal", got, "start", "22:00")
			field(t, "GET after the refusal", got, "end", "07:00")
		})
	}
}
