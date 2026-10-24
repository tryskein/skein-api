package handler

// me_ui_prefs_rest_test.go — sk-551.5: GET/PUT /api/v1/me/ui-prefs on a real
// schema.
//
// Reference — the maintainers' decision (sk-551.5, 2026-10-05) and the task brief:
//   - "All off by default" → GET of someone who never saved is [];
//   - the set of views — Cards / Board / Tree (`cards`, `kanban`, `tree`);
//     repeats collapse, the response order is stable: cards, kanban, tree;
//   - an unknown view — 400, nothing written;
//   - the setting is personal: "stored in the profile", another user has their own;
//   - without authorization — 401.
//
// sk-551.7 — density. Reference — the maintainers' decision (2026-10-05): "Density:
// Normal / Compact", "Normal" by default, stored as a profile field in
// user_ui_prefs; brief: PUT is partial — a PUT of density alone does not touch the set of
// views and vice versa; an unknown value — 400 invalid_density.
//
// sk-551.8 — left panel mode. Reference — spec sk-551.8, contract
// "GET/PUT /api/v1/me/ui-prefs" (D5, SC-002, US-1 "on another machine"):
// GET without a profile row → navLayout "columns"; PUT is partial — without the field
// the mode is not touched (including on the first write — then columns), a PUT of
// other fields does not touch the mode; an unknown value (including an empty
// string and a different case) → 400 invalid_nav_layout; null / non-string →
// 400 invalid_json; nothing is written if any one field is invalid.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/testsupport"
)

// uiPrefsRequest runs one request through the handler. callerID == nil
// sends it without any authenticated caller.
func uiPrefsRequest(t *testing.T, pool *pgxpool.Pool, method string, callerID *uuid.UUID, body string) (int, []byte) {
	t.Helper()
	h := NewMeUIPrefsHandler(pool)
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, "/api/v1/me/ui-prefs", nil)
	} else {
		req = httptest.NewRequest(method, "/api/v1/me/ui-prefs", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if callerID != nil {
		ctx := auth.WithUserID(req.Context(), *callerID)
		ctx = auth.WithUserRole(ctx, "user")
		req = req.WithContext(ctx)
	}
	rec := httptest.NewRecorder()
	switch method {
	case http.MethodGet:
		h.GetUIPrefs(rec, req)
	case http.MethodPut:
		h.PutUIPrefs(rec, req)
	default:
		t.Fatalf("unsupported method %s", method)
	}
	return rec.Code, rec.Body.Bytes()
}

// issueModalViews decodes the field and fails unless it is present and an
// array: `null` or a missing key would break the frontend's set handling.
func issueModalViews(t *testing.T, raw []byte) []string {
	t.Helper()
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode body: %v (%s)", err, raw)
	}
	field, ok := body["issueModalViews"]
	if !ok {
		t.Fatalf("issueModalViews missing from %s", raw)
	}
	var views []string
	if err := json.Unmarshal(field, &views); err != nil || views == nil {
		t.Fatalf("issueModalViews is not an array: %s", field)
	}
	return views
}

func getUIPrefsOK(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) []string {
	t.Helper()
	status, body := uiPrefsRequest(t, pool, http.MethodGet, &user, "")
	if status != http.StatusOK {
		t.Fatalf("GET /me/ui-prefs = %d: %s", status, body)
	}
	return issueModalViews(t, body)
}

func TestUIPrefs_NeverSaved_IsEmptySet(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	user, _ := testsupport.SeedUser(t, pool, "user")

	if got := getUIPrefsOK(t, pool, user); len(got) != 0 {
		t.Errorf("issueModalViews = %v for someone who never saved, want []", got)
	}
}

func TestUIPrefs_PutNormalizesAndGetReadsItBack(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	user, _ := testsupport.SeedUser(t, pool, "user")
	want := []string{"kanban", "tree"}

	status, body := uiPrefsRequest(t, pool, http.MethodPut, &user,
		`{"issueModalViews":["tree","kanban","tree"]}`)
	if status != http.StatusOK {
		t.Fatalf("PUT = %d: %s", status, body)
	}
	if got := issueModalViews(t, body); !reflect.DeepEqual(got, want) {
		t.Errorf("PUT answered %v, want %v", got, want)
	}
	if got := getUIPrefsOK(t, pool, user); !reflect.DeepEqual(got, want) {
		t.Errorf("GET after PUT = %v, want %v", got, want)
	}

	// An empty array switches the window off everywhere again.
	if status, body := uiPrefsRequest(t, pool, http.MethodPut, &user, `{"issueModalViews":[]}`); status != http.StatusOK {
		t.Fatalf("PUT [] = %d: %s", status, body)
	}
	if got := getUIPrefsOK(t, pool, user); len(got) != 0 {
		t.Errorf("GET after PUT [] = %v, want []", got)
	}
}

func TestUIPrefs_PutWithoutTheFieldKeepsTheSet(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	user, _ := testsupport.SeedUser(t, pool, "user")
	if status, body := uiPrefsRequest(t, pool, http.MethodPut, &user, `{"issueModalViews":["cards"]}`); status != http.StatusOK {
		t.Fatalf("seed PUT = %d: %s", status, body)
	}

	status, body := uiPrefsRequest(t, pool, http.MethodPut, &user, `{}`)
	if status != http.StatusOK {
		t.Fatalf("PUT {} = %d: %s", status, body)
	}
	if got := getUIPrefsOK(t, pool, user); !reflect.DeepEqual(got, []string{"cards"}) {
		t.Errorf("GET after PUT {} = %v, want [cards] — an absent field must keep its value", got)
	}
}

func TestUIPrefs_BadBodyIs400AndWritesNothing(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	user, _ := testsupport.SeedUser(t, pool, "user")
	if status, body := uiPrefsRequest(t, pool, http.MethodPut, &user, `{"issueModalViews":["tree"]}`); status != http.StatusOK {
		t.Fatalf("seed PUT = %d: %s", status, body)
	}

	cases := []struct {
		name, body, code string
	}{
		{"unknown view", `{"issueModalViews":["board"]}`, "invalid_view"},
		{"unknown view beside a known one", `{"issueModalViews":["kanban","board"]}`, "invalid_view"},
		{"explicit null", `{"issueModalViews":null}`, "invalid_json"},
		{"not an array", `{"issueModalViews":"kanban"}`, "invalid_json"},
		{"unknown key", `{"layout":"wide"}`, "invalid_json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := uiPrefsRequest(t, pool, http.MethodPut, &user, tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("PUT %s = %d, want 400: %s", tc.body, status, body)
			}
			var problem map[string]any
			if err := json.Unmarshal(body, &problem); err != nil {
				t.Fatalf("decode problem: %v (%s)", err, body)
			}
			if problem["error"] != tc.code {
				t.Errorf("PUT %s: error = %v, want %s (%s)", tc.body, problem["error"], tc.code, body)
			}
			if got := getUIPrefsOK(t, pool, user); !reflect.DeepEqual(got, []string{"tree"}) {
				t.Errorf("after the refused PUT %s the set is %v, want it untouched [tree]", tc.body, got)
			}
		})
	}
}

func TestUIPrefs_Unauthenticated401(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		body := ""
		if method == http.MethodPut {
			body = `{"issueModalViews":["kanban"]}`
		}
		if status, resp := uiPrefsRequest(t, pool, method, nil, body); status != http.StatusUnauthorized {
			t.Errorf("%s without a caller = %d, want 401: %s", method, status, resp)
		}
	}
}

func TestUIPrefs_AreThePersonsOwn(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	userA, _ := testsupport.SeedUser(t, pool, "user")
	userB, _ := testsupport.SeedUser(t, pool, "user")

	if status, body := uiPrefsRequest(t, pool, http.MethodPut, &userB, `{"issueModalViews":["cards","kanban"]}`); status != http.StatusOK {
		t.Fatalf("PUT as B = %d: %s", status, body)
	}
	if got := getUIPrefsOK(t, pool, userA); len(got) != 0 {
		t.Errorf("A sees %v after B saved — B's settings leaked to A", got)
	}
	if got := getUIPrefsOK(t, pool, userB); !reflect.DeepEqual(got, []string{"cards", "kanban"}) {
		t.Errorf("B reads %v, want [cards kanban]", got)
	}
}

// uiPrefsBody decodes the whole answer: both fields must be present.
func uiPrefsBody(t *testing.T, raw []byte) (views []string, density string) {
	t.Helper()
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode body: %v (%s)", err, raw)
	}
	field, ok := body["density"]
	if !ok {
		t.Fatalf("density missing from %s", raw)
	}
	if err := json.Unmarshal(field, &density); err != nil {
		t.Fatalf("density is not a string: %s", field)
	}
	return issueModalViews(t, raw), density
}

func getUIPrefsFull(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) ([]string, string) {
	t.Helper()
	status, body := uiPrefsRequest(t, pool, http.MethodGet, &user, "")
	if status != http.StatusOK {
		t.Fatalf("GET /me/ui-prefs = %d: %s", status, body)
	}
	return uiPrefsBody(t, body)
}

func TestUIPrefs_Density_NeverSavedIsNormal(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	user, _ := testsupport.SeedUser(t, pool, "user")

	if _, density := getUIPrefsFull(t, pool, user); density != "normal" {
		t.Errorf("density = %q for someone who never saved, want normal", density)
	}
}

func TestUIPrefs_Density_PutAloneKeepsTheViews(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	user, _ := testsupport.SeedUser(t, pool, "user")
	if status, body := uiPrefsRequest(t, pool, http.MethodPut, &user, `{"issueModalViews":["kanban","tree"]}`); status != http.StatusOK {
		t.Fatalf("seed PUT = %d: %s", status, body)
	}

	status, body := uiPrefsRequest(t, pool, http.MethodPut, &user, `{"density":"compact"}`)
	if status != http.StatusOK {
		t.Fatalf("PUT density = %d: %s", status, body)
	}
	if views, density := uiPrefsBody(t, body); density != "compact" || !reflect.DeepEqual(views, []string{"kanban", "tree"}) {
		t.Errorf("PUT answered views=%v density=%q, want [kanban tree] compact", views, density)
	}
	views, density := getUIPrefsFull(t, pool, user)
	if density != "compact" {
		t.Errorf("GET after PUT density = %q, want compact", density)
	}
	if !reflect.DeepEqual(views, []string{"kanban", "tree"}) {
		t.Errorf("GET after PUT of density alone: issueModalViews = %v, want [kanban tree] untouched", views)
	}

	// And back: normal is a value too, not only the absence of one.
	if status, body := uiPrefsRequest(t, pool, http.MethodPut, &user, `{"density":"normal"}`); status != http.StatusOK {
		t.Fatalf("PUT normal = %d: %s", status, body)
	}
	if _, density := getUIPrefsFull(t, pool, user); density != "normal" {
		t.Errorf("GET after PUT normal = %q, want normal", density)
	}
}

func TestUIPrefs_Density_FirstSaveOfDensityLeavesViewsAtFactory(t *testing.T) {
	// No row yet: the PUT creates it, and the field it does not name gets
	// the factory position, not something left over.
	pool := testsupport.NewTestPool(t)
	user, _ := testsupport.SeedUser(t, pool, "user")

	if status, body := uiPrefsRequest(t, pool, http.MethodPut, &user, `{"density":"compact"}`); status != http.StatusOK {
		t.Fatalf("PUT density = %d: %s", status, body)
	}
	views, density := getUIPrefsFull(t, pool, user)
	if density != "compact" || len(views) != 0 {
		t.Errorf("GET = views %v density %q, want [] compact", views, density)
	}
}

func TestUIPrefs_Density_ViewsPutKeepsTheDensity(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	user, _ := testsupport.SeedUser(t, pool, "user")
	if status, body := uiPrefsRequest(t, pool, http.MethodPut, &user, `{"density":"compact"}`); status != http.StatusOK {
		t.Fatalf("seed PUT = %d: %s", status, body)
	}

	if status, body := uiPrefsRequest(t, pool, http.MethodPut, &user, `{"issueModalViews":["cards"]}`); status != http.StatusOK {
		t.Fatalf("PUT views = %d: %s", status, body)
	}
	views, density := getUIPrefsFull(t, pool, user)
	if !reflect.DeepEqual(views, []string{"cards"}) {
		t.Errorf("GET after PUT views = %v, want [cards]", views)
	}
	if density != "compact" {
		t.Errorf("GET after PUT of views alone: density = %q, want compact untouched", density)
	}
}

func TestUIPrefs_Density_BadValueIs400AndWritesNothing(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	user, _ := testsupport.SeedUser(t, pool, "user")
	if status, body := uiPrefsRequest(t, pool, http.MethodPut, &user, `{"issueModalViews":["tree"],"density":"compact"}`); status != http.StatusOK {
		t.Fatalf("seed PUT = %d: %s", status, body)
	}

	cases := []struct {
		name, body, code string
	}{
		{"unknown density", `{"density":"tiny"}`, "invalid_density"},
		{"case matters", `{"density":"Compact"}`, "invalid_density"},
		{"bad density beside good views", `{"issueModalViews":["cards"],"density":"tiny"}`, "invalid_density"},
		{"explicit null", `{"density":null}`, "invalid_json"},
		{"not a string", `{"density":1}`, "invalid_json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := uiPrefsRequest(t, pool, http.MethodPut, &user, tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("PUT %s = %d, want 400: %s", tc.body, status, body)
			}
			var problem map[string]any
			if err := json.Unmarshal(body, &problem); err != nil {
				t.Fatalf("decode problem: %v (%s)", err, body)
			}
			if problem["error"] != tc.code {
				t.Errorf("PUT %s: error = %v, want %s (%s)", tc.body, problem["error"], tc.code, body)
			}
			views, density := getUIPrefsFull(t, pool, user)
			if density != "compact" || !reflect.DeepEqual(views, []string{"tree"}) {
				t.Errorf("after the refused PUT %s: views %v density %q, want [tree] compact untouched", tc.body, views, density)
			}
		})
	}
}

// uiPrefsState is the whole answer of GET/PUT /me/ui-prefs: every field
// must be present.
type uiPrefsState struct {
	views     []string
	density   string
	navLayout string
}

func uiPrefsStateOf(t *testing.T, raw []byte) uiPrefsState {
	t.Helper()
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode body: %v (%s)", err, raw)
	}
	field, ok := body["navLayout"]
	if !ok {
		t.Fatalf("navLayout missing from %s", raw)
	}
	var navLayout string
	if err := json.Unmarshal(field, &navLayout); err != nil {
		t.Fatalf("navLayout is not a string: %s", field)
	}
	views, density := uiPrefsBody(t, raw)
	return uiPrefsState{views: views, density: density, navLayout: navLayout}
}

func getUIPrefsState(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) uiPrefsState {
	t.Helper()
	status, body := uiPrefsRequest(t, pool, http.MethodGet, &user, "")
	if status != http.StatusOK {
		t.Fatalf("GET /me/ui-prefs = %d: %s", status, body)
	}
	return uiPrefsStateOf(t, body)
}

func putUIPrefsOK(t *testing.T, pool *pgxpool.Pool, user uuid.UUID, body string) uiPrefsState {
	t.Helper()
	status, raw := uiPrefsRequest(t, pool, http.MethodPut, &user, body)
	if status != http.StatusOK {
		t.Fatalf("PUT %s = %d: %s", body, status, raw)
	}
	return uiPrefsStateOf(t, raw)
}

func assertUIPrefsState(t *testing.T, stage string, got uiPrefsState, want uiPrefsState) {
	t.Helper()
	if !reflect.DeepEqual(got.views, want.views) {
		t.Errorf("%s: issueModalViews = %v, want %v", stage, got.views, want.views)
	}
	if got.density != want.density {
		t.Errorf("%s: density = %q, want %q", stage, got.density, want.density)
	}
	if got.navLayout != want.navLayout {
		t.Errorf("%s: navLayout = %q, want %q", stage, got.navLayout, want.navLayout)
	}
}

func TestUIPrefs_NavLayout_NeverSavedIsColumns(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	user, _ := testsupport.SeedUser(t, pool, "user")

	if got := getUIPrefsState(t, pool, user); got.navLayout != "columns" {
		t.Errorf("navLayout = %q for someone who never saved, want columns", got.navLayout)
	}
}

func TestUIPrefs_NavLayout_PutAloneRoundTripsAndKeepsTheOthers(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	user, _ := testsupport.SeedUser(t, pool, "user")
	putUIPrefsOK(t, pool, user, `{"issueModalViews":["kanban","tree"],"density":"compact"}`)

	answer := putUIPrefsOK(t, pool, user, `{"navLayout":"sections"}`)
	want := uiPrefsState{views: []string{"kanban", "tree"}, density: "compact", navLayout: "sections"}
	assertUIPrefsState(t, "PUT sections answer", answer, want)
	// Another machine of the same person reads the saved mode back.
	assertUIPrefsState(t, "GET after PUT sections", getUIPrefsState(t, pool, user), want)

	// And back: columns is a value too, not only the absence of one.
	want.navLayout = "columns"
	assertUIPrefsState(t, "PUT columns answer", putUIPrefsOK(t, pool, user, `{"navLayout":"columns"}`), want)
	assertUIPrefsState(t, "GET after PUT columns", getUIPrefsState(t, pool, user), want)
}

func TestUIPrefs_NavLayout_FirstSaveLeavesTheOthersAtFactory(t *testing.T) {
	// No row yet: a PUT of the mode alone creates it, and the fields it
	// does not name get the factory position.
	pool := testsupport.NewTestPool(t)
	user, _ := testsupport.SeedUser(t, pool, "user")

	putUIPrefsOK(t, pool, user, `{"navLayout":"sections"}`)
	assertUIPrefsState(t, "GET after the first PUT of navLayout", getUIPrefsState(t, pool, user),
		uiPrefsState{views: []string{}, density: "normal", navLayout: "sections"})
}

func TestUIPrefs_NavLayout_FirstSaveOfAnotherFieldLeavesColumns(t *testing.T) {
	// No row yet: a PUT of another field creates it, and the mode gets the
	// factory position — columns.
	cases := []struct {
		name, body string
		want       uiPrefsState
	}{
		{"density", `{"density":"compact"}`, uiPrefsState{views: []string{}, density: "compact", navLayout: "columns"}},
		{"issueModalViews", `{"issueModalViews":["cards"]}`, uiPrefsState{views: []string{"cards"}, density: "normal", navLayout: "columns"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := testsupport.NewTestPool(t)
			user, _ := testsupport.SeedUser(t, pool, "user")
			putUIPrefsOK(t, pool, user, tc.body)
			assertUIPrefsState(t, "GET after the first PUT "+tc.body, getUIPrefsState(t, pool, user), tc.want)
		})
	}
}

func TestUIPrefs_NavLayout_OtherFieldsPutKeepsTheLayout(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	user, _ := testsupport.SeedUser(t, pool, "user")
	putUIPrefsOK(t, pool, user, `{"navLayout":"sections"}`)

	putUIPrefsOK(t, pool, user, `{"density":"compact"}`)
	assertUIPrefsState(t, "GET after PUT of density alone", getUIPrefsState(t, pool, user),
		uiPrefsState{views: []string{}, density: "compact", navLayout: "sections"})

	putUIPrefsOK(t, pool, user, `{"issueModalViews":["tree"]}`)
	assertUIPrefsState(t, "GET after PUT of issueModalViews alone", getUIPrefsState(t, pool, user),
		uiPrefsState{views: []string{"tree"}, density: "compact", navLayout: "sections"})
}

func TestUIPrefs_NavLayout_BadValueIs400AndWritesNothing(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	user, _ := testsupport.SeedUser(t, pool, "user")
	saved := uiPrefsState{views: []string{"tree"}, density: "compact", navLayout: "sections"}
	putUIPrefsOK(t, pool, user, `{"issueModalViews":["tree"],"density":"compact","navLayout":"sections"}`)

	cases := []struct {
		name, body, code string
	}{
		{"unknown mode", `{"navLayout":"tiles"}`, "invalid_nav_layout"},
		{"empty string", `{"navLayout":""}`, "invalid_nav_layout"},
		{"case matters", `{"navLayout":"Columns"}`, "invalid_nav_layout"},
		{"case matters, sections", `{"navLayout":"Sections"}`, "invalid_nav_layout"},
		{"bad mode beside a good density", `{"density":"normal","navLayout":"tiles"}`, "invalid_nav_layout"},
		{"explicit null", `{"navLayout":null}`, "invalid_json"},
		{"not a string", `{"navLayout":1}`, "invalid_json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := uiPrefsRequest(t, pool, http.MethodPut, &user, tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("PUT %s = %d, want 400: %s", tc.body, status, body)
			}
			var problem map[string]any
			if err := json.Unmarshal(body, &problem); err != nil {
				t.Fatalf("decode problem: %v (%s)", err, body)
			}
			if problem["error"] != tc.code {
				t.Errorf("PUT %s: error = %v, want %s (%s)", tc.body, problem["error"], tc.code, body)
			}
			assertUIPrefsState(t, "after the refused PUT "+tc.body, getUIPrefsState(t, pool, user), saved)
		})
	}
}
