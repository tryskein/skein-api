package handler

import (
	"encoding/json"
	"errors"
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
// @Router /api/v1/me/theme [get]
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
// @Router /api/v1/me/theme [put]
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
// of getComputedStyle() at click time — free-form map (color, fontFamily,
// fontSize, fontWeight, backgroundColor at minimum, but the contract is
// "anything the frontend wants to surface").
type inspectRequest struct {
	Selector      string         `json:"selector"`
	Parents       []string       `json:"parents"`
	Tokens        []string       `json:"tokens"`
	ComputedStyle map[string]any `json:"computedStyle"`
}

// Inspect handles POST /api/v1/me/theme/inspect (sk-10.3).
//
// @Summary Capture the caller's current inspector selection
// @Description Appends the user's "I am looking at THIS element" hint to a process-local in-memory cache keyed by user id. The agent then reads the list via the skein_theme MCP tool (action="describe") in its selectedTargets field to know which elements the user wants to change. TTL = 10 minutes per entry; per-user cap = 20 entries (FIFO eviction); the agent flushes the list via skein_theme (action="inspect_clear") after processing. Body fields: selector (required, non-empty after trim) is the most-specific CSS selector the inspector picked; parents[] is the chain of parent selectors (closest first); tokens[] is the list of theme-variable keys the frontend deduced from the selector (may be empty); computedStyle is a free-form map of CSS property snapshots (color, fontFamily, fontSize, fontWeight, backgroundColor at minimum). Side effects: appends one entry to the in-memory inspect cache. NO database write — the cache is intentionally ephemeral.
// @Tags Themes
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body inspectRequest true "Inspector selection snapshot"
// @Success 204 "Selection captured"
// @Failure 400 {object} map[string]any "invalid_json | invalid_selector"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Router /api/v1/me/theme/inspect [post]
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
	// Inspect is the ONE handler that deliberately drops
	// DisallowUnknownFields. The payload is intentionally extensible —
	// the frontend may grow new fields (viewportSize, devicePixelRatio,
	// etc.) without a backend redeploy, and the inner ComputedStyle map
	// is also intentionally unvalidated. Only the outer body is open;
	// we still bound the size via MaxBytesReader and require non-empty
	// JSON via the Decode error path.
	var req inspectRequest
	r.Body = http.MaxBytesReader(nil, r.Body, requestBodyMaxBytes)
	dec := json.NewDecoder(r.Body)
	// DO NOT call DisallowUnknownFields() — Inspect payload is
	// intentionally extensible (frontend may add viewportSize,
	// devicePixelRatio, etc. without redeploying the backend). Only
	// the *outer* fields are open; validation of inner ComputedStyle
	// values is also intentionally absent.
	if err := dec.Decode(&req); err != nil {
		// Treat empty body and oversized body the same as decodeJSON so
		// the error surface is symmetric with the rest of the API.
		if errors.Is(err, io.EOF) {
			slog.WarnContext(ctx, "me theme inspect: empty body",
				"userId", c.UserID.String())
			writeProblem(w, http.StatusBadRequest, "invalid_json",
				"invalid request body", nil)
			return
		}
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			slog.WarnContext(ctx, "me theme inspect: body too large",
				"userId", c.UserID.String())
			writeProblem(w, http.StatusBadRequest, "invalid_json",
				"request body too large", nil)
			return
		}
		slog.WarnContext(ctx, "me theme inspect: invalid json",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusBadRequest, "invalid_json",
			"invalid request body", nil)
		return
	}
	selector := strings.TrimSpace(req.Selector)
	if selector == "" {
		writeProblem(w, http.StatusBadRequest, "invalid_selector",
			"selector must be non-empty after trim", nil)
		return
	}
	// Normalise parents / tokens to non-nil slices so the agent sees
	// `"parents": []` instead of `"parents": null` when nothing was sent.
	parents := req.Parents
	if parents == nil {
		parents = []string{}
	}
	tokens := req.Tokens
	if tokens == nil {
		tokens = []string{}
	}
	// computedStyle is optional — empty map is a valid frontend state
	// (e.g. selection lost focus before getComputedStyle ran). Pass
	// through as-is.
	style := req.ComputedStyle
	if style == nil {
		style = map[string]any{}
	}
	h.inspectCache.Set(c.UserID, themes.InspectSelection{
		Selector:      selector,
		Parents:       parents,
		Tokens:        tokens,
		ComputedStyle: style,
	})
	// 204 No Content — the agent fetches the captured selection later
	// via skein_theme(action="describe"), this endpoint just stashes it.
	w.WriteHeader(http.StatusNoContent)
}
