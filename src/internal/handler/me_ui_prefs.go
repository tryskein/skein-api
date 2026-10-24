package handler

// me_ui_prefs.go — sk-551.5 / sk-551.7 / sk-551.8: GET and PUT /api/v1/me/ui-prefs,
// the caller's personal interface-appearance settings. The vocabulary, the canonical
// order and the storage live in internal/uiprefs; this file decodes the
// body, maps the outcome onto codes and nothing else.

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/uiprefs"
)

// MeUIPrefsHandler owns /api/v1/me/ui-prefs. A handler of its own rather
// than a method on MeThemeHandler: the theme lives in three users columns
// and is read by the theme editor and the MCP layer; these settings live in
// their own table and have no reader outside the web interface.
type MeUIPrefsHandler struct {
	store *uiprefs.Store
}

// NewMeUIPrefsHandler wires the store around the shared pool. Build once at
// startup; methods are goroutine-safe.
func NewMeUIPrefsHandler(pool *pgxpool.Pool) *MeUIPrefsHandler {
	return &MeUIPrefsHandler{store: uiprefs.NewStore(pool)}
}

// putUIPrefsRequest is the PUT /api/v1/me/ui-prefs body.
//
// The update is PARTIAL: a field that is absent keeps its stored value. The
// table is meant to take further appearance settings, and a client that
// knows only some of them must not reset the rest by omission. An explicit
// null for issueModalViews is rejected rather than read as "absent" — the
// set has no "unset" state, and an empty array is how it is emptied.
//
// json.RawMessage, not *[]string, because a pointer collapses "key absent"
// and "null arrived" into one nil, and the two must get different answers.
// The swaggertype tag names for the spec generator the shape that actually
// travels the wire (it cannot resolve json.RawMessage on its own — the same
// fix as putReviewPrefsRequest).
type putUIPrefsRequest struct {
	// IssueModalViews replaces the set of task views (cards, kanban, tree)
	// in which a selected issue opens as a centred pop-up window. Absent —
	// unchanged. Duplicates are dropped and the order is normalized.
	IssueModalViews json.RawMessage `json:"issueModalViews,omitempty" swaggertype:"array,string" example:"kanban,tree"`
	// Density replaces the interface density: normal or compact. Absent —
	// unchanged. Same RawMessage reason as above: an explicit null is a
	// body-shape error, not «absent».
	Density json.RawMessage `json:"density,omitempty" swaggertype:"string" enums:"normal,compact" example:"compact"`
	// NavLayout replaces the left-panel layout mode: columns (the «Queues»
	// and «Projects» columns side by side) or sections (both as sections of
	// one column). Absent — unchanged. Same RawMessage reason as above: an
	// explicit null is a body-shape error, not «absent».
	NavLayout json.RawMessage `json:"navLayout,omitempty" swaggertype:"string" enums:"columns,sections" example:"sections"`
}

// parseDensity splits the raw field into "absent" (nil, nil) and "a value
// arrived" (non-nil). An explicit null and a non-string are body-shape
// errors; whether the string is a known density is the store's question.
func parseDensity(raw json.RawMessage) (*string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, errors.New("density must be a string, not null")
	}
	var d string
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, errors.New("density must be a string")
	}
	return &d, nil
}

// parseNavLayout splits the raw field into "absent" (nil, nil) and "a value
// arrived" (non-nil). An explicit null and a non-string are body-shape
// errors; whether the string is a known layout mode — the empty string and
// another letter case included — is the store's question.
func parseNavLayout(raw json.RawMessage) (*string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, errors.New("navLayout must be a string, not null")
	}
	var l string
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, errors.New("navLayout must be a string")
	}
	return &l, nil
}

// parseViewSet splits the raw field into "absent" (nil, nil) and "a set
// arrived" (non-nil slice). An explicit null and anything that is not an
// array of strings are body-shape errors.
func parseViewSet(raw json.RawMessage) (*[]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, errors.New("issueModalViews must be an array of view names, not null")
	}
	views := []string{}
	if err := json.Unmarshal(raw, &views); err != nil {
		return nil, errors.New("issueModalViews must be an array of view names")
	}
	return &views, nil
}

// GetUIPrefs handles GET /api/v1/me/ui-prefs.
//
// @Summary Get the caller's interface-appearance settings
// @Description Returns the calling user's personal appearance settings. Three fields: issueModalViews (sk-551.5) — the task views — cards, kanban (the board), tree — in which a selected issue opens as a centred pop-up window over the view instead of a pane beside it; density (sk-551.7) — the interface density, normal (today's sizes) or compact (smaller type, lower controls and filter chips, tighter paddings); and navLayout (sk-551.8) — the left-panel layout mode, columns (the «Queues» and «Projects» navigation columns side by side, today's layout) or sections (both as sections of a single column, like the VS Code Explorer). The settings follow the person across browsers and devices because they live in the profile, not in the browser.
// @Description
// @Description Algorithm: read the caller's row in user_ui_prefs. The row is created lazily on the first save, so for someone who never touched the settings there is no row and the answer is the factory position — an empty set (the pane beside the view everywhere), density normal and navLayout columns. issueModalViews is always an array (never null), ordered cards, kanban, tree, without duplicates; density is always one of normal, compact; navLayout is always one of columns, sections. Read-only, no side effects.
// @Tags Me
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} uiprefs.PrefsDTO "The caller's appearance settings"
// @Failure 401 {object} map[string]any "unauthenticated — missing or invalid session / API key"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Router /api/v1/me/ui-prefs [get]
func (h *MeUIPrefsHandler) GetUIPrefs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	dto, err := h.store.Get(ctx, c.UserID)
	if err != nil {
		slog.ErrorContext(ctx, "get ui prefs failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// PutUIPrefs handles PUT /api/v1/me/ui-prefs.
//
// @Summary Update the caller's interface-appearance settings (partial)
// @Description Applies a PARTIAL update of the caller's appearance settings and returns the full settings, the same payload GET returns, so the client renders the result without a second call.
// @Description
// @Description Algorithm: decode the body (unknown keys are rejected); when issueModalViews is present, validate every name against the closed vocabulary cards, kanban, tree, drop duplicates and order the set cards, kanban, tree; when density is present, validate it against normal, compact (case-sensitive); when navLayout is present, validate it against columns, sections (case-sensitive, no trimming — the empty string and a value with surrounding spaces are unknown). Every present field is validated before anything is written; then one upsert writes the present fields, creating the caller's user_ui_prefs row when there is none. A field that is absent keeps its stored value (on a new row: the factory position — empty set, normal, columns) — so a PUT of one field does not touch the other two, navLayout included; a body naming no field writes nothing and returns the current settings. An empty array switches the pop-up window off in every view (the factory position).
// @Description
// @Description Side effects: none beyond the caller's own row; the change applies to the caller's next render of the task views, other users are unaffected.
// @Description
// @Description Errors: an unknown view name (for example "board") is a malformed request, 400 invalid_view; an unknown density (for example "tiny") is 400 invalid_density; an unknown layout mode (for example "tiles", "", "Sections") is 400 invalid_nav_layout; nothing is written in any of these cases, including the other fields of the same body. A body that is not JSON, carries an unknown key, sends issueModalViews as null or as a non-array, or density or navLayout as null or as a non-string is 400 invalid_json.
// @Tags Me
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body putUIPrefsRequest true "Settings to change; an absent field is left unchanged"
// @Success 200 {object} uiprefs.PrefsDTO "The caller's settings after the update"
// @Failure 400 {object} map[string]any "invalid_json — malformed body, unknown key, issueModalViews null / not an array of strings, density or navLayout null / not a string; invalid_view — a view name outside cards, kanban, tree; invalid_density — a density other than normal, compact; invalid_nav_layout — a layout mode other than columns, sections"
// @Failure 401 {object} map[string]any "unauthenticated — missing or invalid session / API key"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Router /api/v1/me/ui-prefs [put]
func (h *MeUIPrefsHandler) PutUIPrefs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	var req putUIPrefsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	views, err := parseViewSet(req.IssueModalViews)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}

	density, err := parseDensity(req.Density)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}

	navLayout, err := parseNavLayout(req.NavLayout)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}

	dto, err := h.store.Update(ctx, c.UserID, uiprefs.UpdateInput{
		IssueModalViews: views,
		Density:         density,
		NavLayout:       navLayout,
	})
	if err != nil {
		var unknown *uiprefs.UnknownViewError
		if errors.As(err, &unknown) {
			writeProblem(w, http.StatusBadRequest, "invalid_view",
				unknown.Error(), map[string]any{"view": unknown.View})
			return
		}
		var unknownDensity *uiprefs.UnknownDensityError
		if errors.As(err, &unknownDensity) {
			writeProblem(w, http.StatusBadRequest, "invalid_density",
				unknownDensity.Error(), map[string]any{"density": unknownDensity.Density})
			return
		}
		var unknownNavLayout *uiprefs.UnknownNavLayoutError
		if errors.As(err, &unknownNavLayout) {
			writeProblem(w, http.StatusBadRequest, "invalid_nav_layout",
				unknownNavLayout.Error(), map[string]any{"navLayout": unknownNavLayout.NavLayout})
			return
		}
		slog.ErrorContext(ctx, "update ui prefs failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}
