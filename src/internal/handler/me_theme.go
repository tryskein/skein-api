package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/themes"
)

// MeThemeHandler owns the /api/v1/me/theme surface (sk-10.1) — the
// per-user active theme preference. Split from MeHandler so the
// header-counts handler stays single-purpose and tests for each
// surface stay independent. Wired into the existing /me route group
// in main.go alongside the other /me/* endpoints.
//
// sk-10.3 — extended with the inspect cache + POST /me/theme/inspect
// endpoint. The cache is process-local in-memory; lifetime tied to
// the app context (StartCleanup goroutine).
type MeThemeHandler struct {
	store        *themes.Store
	inspectCache *themes.InspectCache
}

// NewMeThemeHandler wires a themes.Store around the shared pool plus
// the inspect cache (sk-10.3). The store is shared (zero-cost copy)
// with ThemesHandler so both surfaces see the same in-flight changes.
// The inspect cache is shared with the MCP tools layer so an inspector
// click on the frontend lands on the same cache the agent reads from.
// Build once at startup; methods are goroutine-safe.
func NewMeThemeHandler(pool *pgxpool.Pool, inspectCache *themes.InspectCache) *MeThemeHandler {
	return &MeThemeHandler{
		store:        themes.NewStore(pool),
		inspectCache: inspectCache,
	}
}

// setThemePrefRequest is the wire shape for PUT /me/theme. customThemeId
// is nullable on the wire ('null' is a valid value) so the DTO uses a
// pointer; mode + builtinTheme are required strings.
type setThemePrefRequest struct {
	Mode          string     `json:"mode"`
	BuiltinTheme  string     `json:"builtinTheme"`
	CustomThemeID *uuid.UUID `json:"customThemeId"`
}

// GetTheme handles GET /api/v1/me/theme.
//
// @Summary Get the caller's active theme preference
// @Description Returns the three theme columns from the users row: mode ("builtin" or "custom"), builtinTheme ("light" or "dark"), and customThemeId (nullable UUID). The frontend uses mode to decide whether to apply a saved theme's variables or fall back to one of the two builtins.
// @Tags Themes
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} themes.UserThemePrefDTO "Active preference"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /ui/v1/me/theme [get]
func (h *MeThemeHandler) GetTheme(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	dto, err := h.store.GetUserThemePref(ctx, c.UserID)
	if err != nil {
		if errors.Is(err, themes.ErrNotFound) {
			// The user row vanished between auth and this read. Treat
			// the same as unauthenticated — the cookie is invalid.
			writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
			return
		}
		slog.ErrorContext(ctx, "get user theme pref failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// SetTheme handles PUT /api/v1/me/theme.
//
// @Summary Set the caller's active theme preference
// @Description Replaces all three theme columns atomically. mode must be "builtin" or "custom"; builtinTheme must be "light" or "dark". When mode="custom", customThemeId is required and must point at one of the CALLER'S OWN themes (sk-674.18 T035, US-21: a user selects the built-ins and his own themes) — a NULL customThemeId returns 400 customThemeId_required, a missing theme or another user's theme returns 404, one answer (INV-3). When mode="builtin", customThemeId is forced NULL even if the caller sent a value (resolved state always matches what the frontend renders).
// @Tags Themes
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body setThemePrefRequest true "New preference"
// @Success 200 {object} themes.UserThemePrefDTO "Updated preference"
// @Failure 400 {object} map[string]any "invalid_json | invalid_mode | invalid_builtin_theme | customThemeId_required"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 404 {object} map[string]any "customThemeId points at a theme that does not exist or is another user's — one answer"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /ui/v1/me/theme [put]
func (h *MeThemeHandler) SetTheme(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	var req setThemePrefRequest
	if err := decodeJSON(r, &req); err != nil {
		slog.WarnContext(ctx, "set user theme pref: invalid json",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusBadRequest, "invalid_json", "invalid request body", nil)
		return
	}
	mode, err := themes.ValidateMode(req.Mode)
	if err != nil {
		// invalid_mode is a body-shape error on /me/theme — rewrite as
		// 400 to match the surrounding pattern (invalid_builtin_theme
		// below, customThemeId_required further down). The swagger
		// @Failure 400 line above lists it; mapThemeValidation would
		// surface it as 422, which contradicts the documented contract.
		if ve, ok := themes.IsValidationError(err); ok {
			writeProblem(w, http.StatusBadRequest, "invalid_mode", ve.Message, nil)
			return
		}
		writeProblem(w, http.StatusBadRequest, "invalid_mode", err.Error(), nil)
		return
	}
	// `builtinTheme` reuses ValidateBase — the wire vocabulary is the
	// same ("light" or "dark") and re-using the validator avoids a
	// duplicate map.
	builtin, err := themes.ValidateBase(req.BuiltinTheme)
	if err != nil {
		// Code from ValidateBase is "invalid_base"; surface as the
		// pref-specific code so the frontend can render a precise
		// inline error. We rewrite by writing the response directly
		// rather than threading a second validator just for this.
		if ve, ok := themes.IsValidationError(err); ok {
			writeProblem(w, http.StatusBadRequest, "invalid_builtin_theme", ve.Message, nil)
			return
		}
		writeProblem(w, http.StatusBadRequest, "invalid_builtin_theme", err.Error(), nil)
		return
	}

	dto, err := h.store.SetUserThemePref(ctx, c.UserID, themes.SetThemePrefInput{
		Mode:          mode,
		BuiltinTheme:  builtin,
		CustomThemeID: req.CustomThemeID,
	})
	if errors.Is(err, themes.ErrCustomThemeRequired) {
		writeProblem(w, http.StatusBadRequest, "customThemeId_required",
			"customThemeId is required when mode=custom", nil)
		return
	}
	if errors.Is(err, themes.ErrCustomThemeNotFound) {
		writeProblem(w, http.StatusNotFound, "theme_not_found",
			"customThemeId does not match any theme", nil)
		return
	}
	if errors.Is(err, themes.ErrNotFound) {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "set user theme pref failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// inspectRequest is the wire shape for POST /me/theme/inspect (sk-10.3).
//
// Selector + parents come from the browser inspector overlay; tokens is
// the theme-variable-key list the frontend deduced from the selector
// chain (may be empty when no token matches). computedStyle is a snapshot
// of getComputedStyle() at click time — a flat map of string values
// (color, fontFamily, fontSize, fontWeight, backgroundColor today).
// Unknown top-level fields are ignored, so the frontend may add some.
type inspectRequest struct {
	// Selector — the most specific selector picked; required, non-empty after trim.
	Selector string `json:"selector"`
	// Parents — parent selectors, closest first.
	Parents []*string `json:"parents" swaggertype:"array,string"`
	// Tokens — theme variable keys the frontend matched.
	Tokens []*string `json:"tokens" swaggertype:"array,string"`
	// ComputedStyle — CSS property → serialized value; string values only.
	ComputedStyle map[string]json.RawMessage `json:"computedStyle" swaggertype:"object,string"`
}

// ThemeInspectGate guards POST …/me/theme/inspect BEFORE any other
// middleware reads its body (RefuseNUL buffers bodies up to 1 MiB): it
// takes one of maxInFlight concurrency slots — none free answers 429
// theme_inspect_busy — and reads the body once, bounded to maxBody bytes:
// a declared or actual length above it answers 413
// theme_inspect_body_too_large before any JSON is parsed. The slot is held
// until the request is answered. Other requests pass untouched
// (sk-977.7.10). Build ONE gate per router: the slots are its own.
func ThemeInspectGate(maxBody int64, maxInFlight int) func(http.Handler) http.Handler {
	slots := make(chan struct{}, maxInFlight)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/me/theme/inspect") {
				next.ServeHTTP(w, r)
				return
			}
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			default:
				writeProblem(w, http.StatusTooManyRequests, "theme_inspect_busy",
					"too many inspector selections are being saved; try again", nil)
				return
			}
			if r.ContentLength > maxBody {
				writeInspectBodyTooLarge(w, maxBody)
				return
			}
			// The whole body is read here, bounded: the size decision is made
			// on this first read, before any JSON is looked at, for a
			// declared length and a streamed body alike. The handlers after
			// the gate (RefuseNUL, Inspect) get the bytes read.
			if r.Body != nil && r.Body != http.NoBody {
				// http.MaxBytesReader with the SERVER's writer: past maxBody the
				// read fails at once and the server closes the connection after
				// the answer, instead of waiting for and draining the rest of an
				// oversized upload. Middleware writers (metrics, cancellation)
				// hide the server's one, so it is found through Unwrap.
				raw, err := io.ReadAll(http.MaxBytesReader(innermostWriter(w), r.Body, maxBody))
				var maxErr *http.MaxBytesError
				if errors.As(err, &maxErr) {
					writeInspectBodyTooLarge(w, maxBody)
					return
				}
				if err != nil {
					writeProblem(w, http.StatusBadRequest, "invalid_json", "the request body could not be read", nil)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(raw))
				r.ContentLength = int64(len(raw))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// innermostWriter follows the Unwrap chain of middleware ResponseWriters
// (the convention http.ResponseController uses) down to the server's own.
func innermostWriter(w http.ResponseWriter) http.ResponseWriter {
	for {
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return w
		}
		w = u.Unwrap()
	}
}

func writeInspectBodyTooLarge(w http.ResponseWriter, maxBody int64) {
	writeProblem(w, http.StatusRequestEntityTooLarge, "theme_inspect_body_too_large",
		fmt.Sprintf("request body exceeds %d bytes", maxBody),
		map[string]any{"details": map[string]any{"limit": maxBody}})
}

// writeInspectLimit answers 422 theme_inspect_limits for field, whose limit
// is limit (bytes or a count, see the message).
func writeInspectLimit(w http.ResponseWriter, field string, limit int, what string) {
	writeProblem(w, http.StatusUnprocessableEntity, "theme_inspect_limits",
		fmt.Sprintf("%s: at most %d %s", field, limit, what),
		map[string]any{"details": map[string]any{"field": field, "limit": limit}})
}

// Inspect handles POST /api/v1/me/theme/inspect (sk-10.3).
//
// @Summary Capture the caller's current inspector selection
// @Description Appends the user's "I am looking at THIS element" hint to a process-local in-memory cache keyed by user id. The agent then reads the list via the skein_theme MCP tool (action="describe") in its selectedTargets field to know which elements the user wants to change; it flushes the list via skein_theme (action="inspect_clear") after processing.
// @Description
// @Description Body fields: selector (required, non-empty after trim) is the most-specific CSS selector the inspector picked; parents[] is the chain of parent selectors (closest first); tokens[] is the list of theme-variable keys the frontend deduced (may be empty); computedStyle is a flat map of CSS property → serialized value, string values only. Unknown top-level fields are ignored. Missing parents, tokens or computedStyle are stored as empty.
// @Description
// @Description Limits (sk-977.7.10; defaults, each set by an SKEIN_THEME_INSPECT_* variable): body 64 KiB, checked on the first read; selector 1 KiB; up to 16 parents of 1 KiB each; up to 128 tokens of 64 bytes each; up to 16 computedStyle keys of 64 bytes with values of 1 KiB; the stored entry (compact JSON with capturedAt) 8 KiB. At most 32 of these requests are served at once per replica. A refused request stores nothing — nothing is cut to fit.
// @Description
// @Description Side effects: appends one entry to the in-memory inspect cache — at most 20 entries per user (the oldest goes), TTL 10 minutes from the click, and at most 8 MiB accounted per replica (expired entries go first, then the oldest clicks of all users). NO database write.
// @Tags Themes
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body inspectRequest true "Inspector selection snapshot"
// @Success 204 "Selection captured"
// @Failure 400 {object} map[string]any "invalid_json — empty or malformed body, a value of the wrong type (computedStyle values must be strings), or data after the JSON object; invalid_selector — selector empty after trim"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 413 {object} map[string]any "theme_inspect_body_too_large — the body exceeds the body limit (details.limit); nothing is stored"
// @Failure 422 {object} map[string]any "theme_inspect_limits — a field exceeds its limit (details.field, details.limit), or the stored entry would; nothing is stored"
// @Failure 429 {object} map[string]any "theme_inspect_busy — too many inspector selections are being saved at once on this replica; retry"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Router /ui/v1/me/theme/inspect [post]
func (h *MeThemeHandler) Inspect(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	if h.inspectCache == nil {
		// Defensive — wiring always sets the cache. Surface as 500 so a
		// regression in main.go is visible rather than silently swallowed.
		slog.ErrorContext(ctx, "inspect cache not wired",
			"userId", c.UserID.String())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	lim := h.inspectCache.Limits()
	// ThemeInspectGate has already bounded the body at the route's limit;
	// this second bound covers a mount without the gate (tests).
	r.Body = http.MaxBytesReader(nil, r.Body, lim.MaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	// Unknown top-level fields are ignored on purpose: the frontend may add
	// fields (viewportSize, …) without a backend change.
	var req inspectRequest
	var root json.RawMessage
	err := dec.Decode(&root)
	if err == nil {
		if t := bytes.TrimLeft(root, " \t\r\n"); len(t) == 0 || t[0] != '{' {
			writeProblem(w, http.StatusBadRequest, "invalid_json", "the body must be a JSON object", nil)
			return
		}
		err = json.Unmarshal(root, &req)
	}
	if err == nil {
		// Exactly one JSON value: anything after it is refused.
		if _, terr := dec.Token(); !errors.Is(terr, io.EOF) {
			var maxErr *http.MaxBytesError
			if errors.As(terr, &maxErr) {
				writeInspectBodyTooLarge(w, lim.MaxBodyBytes)
				return
			}
			writeProblem(w, http.StatusBadRequest, "invalid_json",
				"the body must be exactly one JSON object", nil)
			return
		}
	}
	if err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxErr):
			slog.WarnContext(ctx, "me theme inspect: body too large", "userId", c.UserID.String())
			writeInspectBodyTooLarge(w, lim.MaxBodyBytes)
		case errors.Is(err, io.EOF):
			writeProblem(w, http.StatusBadRequest, "invalid_json", "invalid request body", nil)
		default:
			slog.WarnContext(ctx, "me theme inspect: invalid json",
				"userId", c.UserID.String(), "error", err.Error())
			writeProblem(w, http.StatusBadRequest, "invalid_json", "invalid request body", nil)
		}
		return
	}
	if len(req.Selector) > lim.MaxSelectorBytes {
		writeInspectLimit(w, "selector", lim.MaxSelectorBytes, "bytes")
		return
	}
	selector := strings.TrimSpace(req.Selector)
	if selector == "" {
		writeProblem(w, http.StatusBadRequest, "invalid_selector",
			"selector must be non-empty after trim", nil)
		return
	}
	style := make(map[string]string, len(req.ComputedStyle))
	for k, v := range req.ComputedStyle {
		var sv string
		if len(v) == 0 || v[0] != '"' || json.Unmarshal(v, &sv) != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_json",
				"computedStyle values must be strings",
				map[string]any{"details": map[string]any{"field": "computedStyle." + k}})
			return
		}
		style[k] = sv
	}
	// Missing parents / tokens become [] (the agent sees `"parents": []`,
	// never null); a null element is refused.
	parents, ok := inspectStrings(w, "parents", req.Parents)
	if !ok {
		return
	}
	tokens, ok := inspectStrings(w, "tokens", req.Tokens)
	if !ok {
		return
	}
	if !checkInspectLimits(w, lim, selector, parents, tokens, style) {
		return
	}
	err = h.inspectCache.Set(c.UserID, themes.InspectSelection{
		Selector:      selector,
		Parents:       parents,
		Tokens:        tokens,
		ComputedStyle: style,
	})
	if errors.Is(err, themes.ErrInspectEntryTooLarge) {
		writeInspectLimit(w, "entry", lim.MaxEntryBytes, "bytes")
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "me theme inspect: storing the selection failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	// 204 No Content — the agent fetches the captured selection later
	// via skein_theme(action="describe"), this endpoint just stashes it.
	w.WriteHeader(http.StatusNoContent)
}

// inspectStrings is a string array of the body: absent → [], a null element
// answers 400 invalid_json for field.
func inspectStrings(w http.ResponseWriter, field string, in []*string) ([]string, bool) {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == nil {
			writeProblem(w, http.StatusBadRequest, "invalid_json", field+" elements must be strings",
				map[string]any{"details": map[string]any{"field": field}})
			return nil, false
		}
		out = append(out, *s)
	}
	return out, true
}

// checkInspectLimits answers 422 theme_inspect_limits for the first field
// over its limit and reports whether all are within. Sizes are bytes of the
// decoded UTF-8; nothing is cut to fit.
func checkInspectLimits(w http.ResponseWriter, lim themes.InspectLimits, selector string, parents, tokens []string, style map[string]string) bool {
	if len(parents) > lim.MaxParents {
		writeInspectLimit(w, "parents", lim.MaxParents, "items")
		return false
	}
	for _, p := range parents {
		if len(p) > lim.MaxParentBytes {
			writeInspectLimit(w, "parents[]", lim.MaxParentBytes, "bytes")
			return false
		}
	}
	if len(tokens) > lim.MaxTokens {
		writeInspectLimit(w, "tokens", lim.MaxTokens, "items")
		return false
	}
	for _, t := range tokens {
		if len(t) > lim.MaxTokenBytes {
			writeInspectLimit(w, "tokens[]", lim.MaxTokenBytes, "bytes")
			return false
		}
	}
	if len(style) > lim.MaxStyleKeys {
		writeInspectLimit(w, "computedStyle", lim.MaxStyleKeys, "keys")
		return false
	}
	for k, v := range style {
		if len(k) > lim.MaxStyleKeyBytes {
			writeInspectLimit(w, "computedStyle key", lim.MaxStyleKeyBytes, "bytes")
			return false
		}
		if len(v) > lim.MaxStyleValueBytes {
			writeInspectLimit(w, "computedStyle."+k, lim.MaxStyleValueBytes, "bytes")
			return false
		}
	}
	return true
}
