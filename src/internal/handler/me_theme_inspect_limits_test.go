package handler

// me_theme_inspect_limits_test.go — sk-977.7.10: the limits of
// POST /me/theme/inspect (docs/research/theme-inspect-limits.md, accepted by
// the owner), through the same middleware order as mountInternalAPI:
// ThemeInspectGate, then RefuseNUL, then the handler. No database.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"skein-api/internal/themes"
)

const inspectPath = "/ui/v1/me/theme/inspect"

func inspectRouter(cache *themes.InspectCache) http.Handler {
	lim := cache.Limits()
	r := chi.NewRouter()
	r.Use(ThemeInspectGate(lim.MaxBodyBytes, lim.MaxInFlight))
	r.Use(RefuseNUL)
	r.Post(inspectPath, NewMeThemeHandler(nil, cache).Inspect)
	return r
}

func postInspect(t *testing.T, h http.Handler, user uuid.UUID, body []byte, streamed bool) (int, map[string]any) {
	t.Helper()
	var rd io.Reader = bytes.NewReader(body)
	if streamed {
		rd = io.MultiReader(bytes.NewReader(body)) // hides the length
	}
	req := httptest.NewRequest(http.MethodPost, inspectPath, rd)
	if streamed {
		req.ContentLength = -1
	}
	req = withCaller(req, user)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func tokensN(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("--token-%02d", i)
	}
	return out
}

func inspectBody(t *testing.T, v map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func card(tokens int) map[string]any {
	return map[string]any{
		"selector": ".task-card-title-text",
		"parents":  []string{".task-card", ".task-card-body", ".tasks-col", ".col-body", ".shell"},
		"tokens":   tokensN(tokens),
		"computedStyle": map[string]string{
			"color": "rgb(28, 25, 23)", "fontFamily": "Geist, system-ui, sans-serif",
			"fontSize": "16px", "fontWeight": "500", "backgroundColor": "rgba(0, 0, 0, 0)",
		},
	}
}

// The usual clicks — card titles with 24 and 33 tokens, the whole 86-token
// catalogue, a click with every optional field missing, an unknown
// top-level field — are stored; missing fields as [] and {}.
func TestInspectLimits_UsualClicksAreStored(t *testing.T) {
	cache := themes.NewInspectCache(themes.DefaultInspectLimits())
	h := inspectRouter(cache)
	user := uuid.New()
	for name, v := range map[string]map[string]any{
		"24 tokens":     card(24),
		"33 tokens":     card(33),
		"86 tokens":     card(86),
		"only selector": {"selector": ".x"},
		"unknown field": {"selector": ".y", "viewportSize": map[string]int{"w": 1, "h": 2}},
	} {
		if code, out := postInspect(t, h, user, inspectBody(t, v), false); code != http.StatusNoContent {
			t.Fatalf("%s: %d %v, want 204", name, code, out)
		}
	}
	entries := cache.Get(user)
	if len(entries) != 5 {
		t.Fatalf("%d entries stored, want 5", len(entries))
	}
	var bare map[string]any
	_ = json.Unmarshal(entries[3], &bare)
	if fmt.Sprint(bare["parents"], bare["tokens"], bare["computedStyle"]) != "[] [] map[]" {
		t.Fatalf("missing fields stored as %v %v %v, want [] [] {}", bare["parents"], bare["tokens"], bare["computedStyle"])
	}
}

// Every field limit, at the limit and one byte or one item over it: at the
// limit the click is stored, over it 422 theme_inspect_limits names the
// field and nothing is stored.
func TestInspectLimits_FieldBounds(t *testing.T) {
	lim := themes.DefaultInspectLimits()
	lim.MaxEntryBytes = 64 << 10 // let the field limits, not the entry limit, decide
	lim.DescribeMaxBytes = lim.MaxEntryBytes + 2
	lim.MaxBodyBytes = 128 << 10
	long := func(n int) string { return strings.Repeat("a", n) }
	strs := func(n, size int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = long(size)
		}
		return out
	}
	style := func(keys, keySize, valSize int) map[string]string {
		m := map[string]string{}
		for i := 0; i < keys; i++ {
			m[fmt.Sprintf("%0*d", keySize, i)] = long(valSize)
		}
		return m
	}
	cases := []struct {
		name  string
		at    map[string]any
		over  map[string]any
		field string
	}{
		{"selector", map[string]any{"selector": long(lim.MaxSelectorBytes)}, map[string]any{"selector": long(lim.MaxSelectorBytes + 1)}, "selector"},
		{"parents count", map[string]any{"selector": ".s", "parents": strs(lim.MaxParents, 1)}, map[string]any{"selector": ".s", "parents": strs(lim.MaxParents+1, 1)}, "parents"},
		{"parent size", map[string]any{"selector": ".s", "parents": strs(1, lim.MaxParentBytes)}, map[string]any{"selector": ".s", "parents": strs(1, lim.MaxParentBytes+1)}, "parents[]"},
		{"tokens count", map[string]any{"selector": ".s", "tokens": strs(lim.MaxTokens, 1)}, map[string]any{"selector": ".s", "tokens": strs(lim.MaxTokens+1, 1)}, "tokens"},
		{"token size", map[string]any{"selector": ".s", "tokens": strs(1, lim.MaxTokenBytes)}, map[string]any{"selector": ".s", "tokens": strs(1, lim.MaxTokenBytes+1)}, "tokens[]"},
		{"style keys", map[string]any{"selector": ".s", "computedStyle": style(lim.MaxStyleKeys, 2, 1)}, map[string]any{"selector": ".s", "computedStyle": style(lim.MaxStyleKeys+1, 2, 1)}, "computedStyle"},
		{"style key size", map[string]any{"selector": ".s", "computedStyle": style(1, lim.MaxStyleKeyBytes, 1)}, map[string]any{"selector": ".s", "computedStyle": style(1, lim.MaxStyleKeyBytes+1, 1)}, "computedStyle key"},
		{"style value size", map[string]any{"selector": ".s", "computedStyle": map[string]string{"color": long(lim.MaxStyleValueBytes)}}, map[string]any{"selector": ".s", "computedStyle": map[string]string{"color": long(lim.MaxStyleValueBytes + 1)}}, "computedStyle.color"},
		// UTF-8 bytes, not characters: 512 Cyrillic letters are 1024 bytes.
		{"selector utf-8", map[string]any{"selector": strings.Repeat("ж", lim.MaxSelectorBytes/2)}, map[string]any{"selector": strings.Repeat("ж", lim.MaxSelectorBytes/2) + "a"}, "selector"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cache := themes.NewInspectCache(lim)
			h := inspectRouter(cache)
			user := uuid.New()
			if code, out := postInspect(t, h, user, inspectBody(t, tc.at), false); code != http.StatusNoContent {
				t.Fatalf("at the limit: %d %v, want 204", code, out)
			}
			code, out := postInspect(t, h, user, inspectBody(t, tc.over), false)
			details, _ := out["details"].(map[string]any)
			if code != http.StatusUnprocessableEntity || out["error"] != "theme_inspect_limits" || details["field"] != tc.field {
				t.Fatalf("over the limit: %d %v, want 422 theme_inspect_limits on %s", code, out, tc.field)
			}
			if n := len(cache.Get(user)); n != 1 {
				t.Fatalf("%d entries after the refusal, want only the one at the limit", n)
			}
		})
	}
}

// Fields that each fit can still make an entry over MaxEntryBytes (16
// parents of 1 KiB): refused as a whole, nothing stored, nothing cut.
func TestInspectLimits_EntryBound(t *testing.T) {
	cache := themes.NewInspectCache(themes.DefaultInspectLimits())
	h := inspectRouter(cache)
	user := uuid.New()
	parents := make([]string, 16)
	for i := range parents {
		parents[i] = strings.Repeat("p", 1024)
	}
	code, out := postInspect(t, h, user, inspectBody(t, map[string]any{"selector": ".s", "parents": parents}), false)
	details, _ := out["details"].(map[string]any)
	if code != http.StatusUnprocessableEntity || details["field"] != "entry" {
		t.Fatalf("%d %v, want 422 theme_inspect_limits on entry", code, out)
	}
	if n := len(cache.Get(user)); n != 0 {
		t.Fatalf("%d entries stored, want 0", n)
	}
}

// The body limit holds on the first read, for a declared length and for a
// streamed body alike: 413, nothing stored. Exactly at the limit passes the
// gate (and is then judged by the field limits).
func TestInspectLimits_BodyBound(t *testing.T) {
	lim := themes.DefaultInspectLimits()
	cache := themes.NewInspectCache(lim)
	h := inspectRouter(cache)
	user := uuid.New()
	pad := func(total int) []byte {
		head := `{"selector":".s","pad":"`
		tail := `"}`
		return []byte(head + strings.Repeat("x", total-len(head)-len(tail)) + tail)
	}
	if code, out := postInspect(t, h, user, pad(int(lim.MaxBodyBytes)), false); code != http.StatusNoContent {
		t.Fatalf("a body of exactly the limit: %d %v, want 204", code, out)
	}
	for _, streamed := range []bool{false, true} {
		code, out := postInspect(t, h, user, pad(int(lim.MaxBodyBytes)+1), streamed)
		if code != http.StatusRequestEntityTooLarge || out["error"] != "theme_inspect_body_too_large" {
			t.Fatalf("streamed=%v: %d %v, want 413 theme_inspect_body_too_large", streamed, code, out)
		}
	}
	if n := len(cache.Get(user)); n != 1 {
		t.Fatalf("%d entries, want only the one within the limit", n)
	}
}

// The shape: computedStyle values must be strings — a nested object, a
// number, null are 400 — exactly one JSON value, and a NUL is refused by
// RefuseNUL as everywhere else. Nothing is stored.
func TestInspectLimits_Shape(t *testing.T) {
	cache := themes.NewInspectCache(themes.DefaultInspectLimits())
	h := inspectRouter(cache)
	user := uuid.New()
	for name, tc := range map[string]struct {
		body string
		code int
	}{
		"nested style":   {`{"selector":".s","computedStyle":{"color":{"r":1}}}`, http.StatusBadRequest},
		"number style":   {`{"selector":".s","computedStyle":{"fontSize":16}}`, http.StatusBadRequest},
		"null style":     {`{"selector":".s","computedStyle":{"color":null}}`, http.StatusBadRequest},
		"trailing value": {`{"selector":".s"} {"selector":".t"}`, http.StatusBadRequest},
		"trailing junk":  {`{"selector":".s"}x`, http.StatusBadRequest},
		"nul":            {`{"selector":".s\u0000"}`, http.StatusUnprocessableEntity},
		"array token":    {`{"selector":".s","tokens":[["--a"]]}`, http.StatusBadRequest},
	} {
		if code, out := postInspect(t, h, user, []byte(tc.body), false); code != tc.code {
			t.Fatalf("%s: %d %v, want %d", name, code, out, tc.code)
		}
	}
	if n := len(cache.Get(user)); n != 0 {
		t.Fatalf("%d entries stored, want 0", n)
	}
}

// The concurrency gate: with every slot taken a further POST is 429
// theme_inspect_busy; a slot comes back when its request ends, whatever the
// outcome; other routes are not gated.
func TestThemeInspectGate_Slots(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	var mu sync.Mutex
	served := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		served++
		mu.Unlock()
		if r.URL.Path == inspectPath {
			entered <- struct{}{}
			<-release
		}
		w.WriteHeader(http.StatusNoContent)
	})
	h := ThemeInspectGate(1024, 1)(next)
	do := func(path string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
		return rec.Code
	}

	done := make(chan int)
	go func() { done <- do(inspectPath) }()
	<-entered
	if code := do(inspectPath); code != http.StatusTooManyRequests {
		t.Fatalf("second concurrent POST: %d, want 429", code)
	}
	if code := do("/ui/v1/me/theme"); code != http.StatusNoContent {
		t.Fatalf("another route while the slot is taken: %d, want it untouched", code)
	}
	close(release)
	if code := <-done; code != http.StatusNoContent {
		t.Fatalf("first POST: %d", code)
	}
	go func() { entered <- struct{}{} }() // the next one does not block
	if code := do(inspectPath); code != http.StatusNoContent {
		t.Fatalf("after the slot came back: %d, want 204", code)
	}
	// A refused request (declared body over the limit) gives its slot back too.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, inspectPath, strings.NewReader(strings.Repeat("x", 2048)))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-limit body: %d, want 413", rec.Code)
	}
	go func() { entered <- struct{}{} }()
	if code := do(inspectPath); code != http.StatusNoContent {
		t.Fatalf("after a refusal: %d, want 204 (slot returned)", code)
	}
}
