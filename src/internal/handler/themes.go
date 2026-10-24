package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/themes"
)

// ThemesHandler owns the /api/v1/themes/* surface (sk-10.1).
//
// All endpoints are user-bound (RequireUser composes upstream) but
// not user-scoped: every authenticated user can list, create, read,
// update and delete every theme. Themes are an org-wide resource —
// per-theme permissions were considered and explicitly deferred for
// the MVP. createdBy / updatedBy are still tracked so the UI can
// surface attribution.
type ThemesHandler struct {
	store *themes.Store
}

// NewThemesHandler wires a fresh themes.Store around the shared pool.
// Build once at startup; methods are goroutine-safe.
func NewThemesHandler(pool *pgxpool.Pool) *ThemesHandler {
	return &ThemesHandler{store: themes.NewStore(pool)}
}

// mapThemeValidation translates a themes.ValidationError into the
// REST error envelope. Mirrors mapValidation in projects.go but for
// the themes package's typed error.
func mapThemeValidation(w http.ResponseWriter, err error) bool {
	if ve, ok := themes.IsValidationError(err); ok {
		writeProblem(w, http.StatusUnprocessableEntity, ve.Code, ve.Message, nil)
		return true
	}
	return false
}

// ----------------- request shapes -----------------

// createThemeRequest is the wire shape for POST /themes.
type createThemeRequest struct {
	Name      string            `json:"name"`
	Base      string            `json:"base"`
	Variables map[string]string `json:"variables"`
}

// patchThemeRequest captures the partial-update shape. Pointer fields
// distinguish "leave alone" (nil) from "set to the supplied value".
// Variables uses *map so we can tell a missing key from an explicit
// empty map; the store replaces (not merges) the column when set.
type patchThemeRequest struct {
	Name      *string            `json:"name"`
	Base      *string            `json:"base"`
	Variables *map[string]string `json:"variables"`
}

// ----------------- endpoints -----------------

// List handles GET /api/v1/themes.
//
// @Summary List the caller's themes
// @Description Returns the caller's own custom themes, sorted by updatedAt DESC (sk-674.18 T035, US-21): a theme belongs to its author, and another user's theme — its name, its author — never appears here or anywhere else. The built-in themes are not rows and are not listed. No pagination: a person has few themes.
// @Tags Themes
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {array} themes.ThemeDTO "Theme list (newest update first)"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/themes [get]
func (h *ThemesHandler) List(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	out, err := h.store.ListThemes(ctx, c.UserID)
	if err != nil {
		slog.ErrorContext(ctx, "list themes failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// Get handles GET /api/v1/themes/{id}.
//
// @Summary Fetch a single theme by id
// @Description Returns one of the caller's own themes: the full DTO including the CSS variables map and createdBy / updatedBy attribution. Another user's theme answers 404 theme_not_found, exactly as a nonexistent id (INV-3, sk-674.18 T035).
// @Tags Themes
// @Produce json
// @Security ApiKeyAuth
// @Param id path string true "Theme UUID"
// @Success 200 {object} themes.ThemeDTO "Theme row"
// @Failure 400 {object} map[string]any "invalid_id — the path id is not a UUID"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 404 {object} map[string]any "theme_not_found — absent or another user's, one answer"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/themes/{id} [get]
func (h *ThemesHandler) Get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	id, err := parseUUIDParam(r, "id")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_id", err.Error(), nil)
		return
	}
	dto, err := h.store.GetTheme(ctx, id, c.UserID)
	if errors.Is(err, themes.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "theme_not_found", "theme not found", nil)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "get theme failed",
			"themeId", id.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// Create handles POST /api/v1/themes.
//
// @Summary Create a new theme
// @Description Validates name (non-empty after trim, ≤100 chars), base ("light" or "dark"), and each variable value (non-empty after trim, ≤256 chars, no `;` `<` `>`). Inserts the row with createdBy = updatedBy = author = the calling user: the theme is the caller's (sk-674.18 T035). Returns 422 name_taken when the caller already has a theme of that name (case-insensitive); another user's theme of the same name never collides and is never revealed.
// @Tags Themes
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body createThemeRequest true "Theme to create"
// @Success 201 {object} themes.ThemeDTO "Created theme"
// @Failure 400 {object} map[string]any "Invalid JSON"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 422 {object} map[string]any "invalid_name | invalid_base | invalid_variables | name_taken"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/themes [post]
func (h *ThemesHandler) Create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	var req createThemeRequest
	if err := decodeJSON(r, &req); err != nil {
		slog.WarnContext(ctx, "create theme: invalid json",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusBadRequest, "invalid_json", "invalid request body", nil)
		return
	}

	name, err := themes.ValidateName(req.Name)
	if err != nil {
		mapThemeValidation(w, err)
		return
	}
	base, err := themes.ValidateBase(req.Base)
	if err != nil {
		mapThemeValidation(w, err)
		return
	}
	vars, err := themes.ValidateVariables(req.Variables)
	if err != nil {
		mapThemeValidation(w, err)
		return
	}

	dto, err := h.store.CreateTheme(ctx, themes.CreateThemeInput{
		Name:      name,
		Base:      base,
		Variables: vars,
		CreatedBy: c.UserID,
	})
	if errors.Is(err, themes.ErrNameTaken) {
		writeProblem(w, http.StatusUnprocessableEntity, "name_taken",
			"a theme with this name already exists", nil)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "create theme failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	slog.InfoContext(ctx, "theme created",
		"userId", c.UserID.String(), "themeId", dto.ID.String(), "name", dto.Name)
	writeJSON(w, http.StatusCreated, dto)
}

// Patch handles PATCH /api/v1/themes/{id}.
//
// @Summary Update an existing theme
// @Description Partial update — any subset of {name, base, variables} may be supplied; omitted fields stay unchanged. Variables is REPLACED (not merged) when supplied. updated_by becomes the calling user and updated_at bumps regardless of whether any value-field changed. Only the author edits a theme: another user's theme answers 404 theme_not_found, as a nonexistent id (INV-3, sk-674.18 T035). A rename collides only with the caller's own themes (422 name_taken).
// @Tags Themes
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param id path string true "Theme UUID"
// @Param body body patchThemeRequest true "Partial patch"
// @Success 200 {object} themes.ThemeDTO "Updated theme"
// @Failure 400 {object} map[string]any "Invalid JSON / invalid id"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 404 {object} map[string]any "theme_not_found — absent or another user's, one answer"
// @Failure 422 {object} map[string]any "invalid_name | invalid_base | invalid_variables | name_taken"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/themes/{id} [patch]
func (h *ThemesHandler) Patch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	id, err := parseUUIDParam(r, "id")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_id", err.Error(), nil)
		return
	}
	var req patchThemeRequest
	if err := decodeJSON(r, &req); err != nil {
		slog.WarnContext(ctx, "patch theme: invalid json",
			"userId", c.UserID.String(), "themeId", id.String(), "error", err.Error())
		writeProblem(w, http.StatusBadRequest, "invalid_json", "invalid request body", nil)
		return
	}

	in := themes.UpdateThemeInput{UpdatedBy: c.UserID}
	if req.Name != nil {
		name, err := themes.ValidateName(*req.Name)
		if err != nil {
			mapThemeValidation(w, err)
			return
		}
		in.Name = &name
	}
	if req.Base != nil {
		base, err := themes.ValidateBase(*req.Base)
		if err != nil {
			mapThemeValidation(w, err)
			return
		}
		in.Base = &base
	}
	if req.Variables != nil {
		vars, err := themes.ValidateVariables(*req.Variables)
		if err != nil {
			mapThemeValidation(w, err)
			return
		}
		in.Variables = &vars
	}

	dto, err := h.store.UpdateTheme(ctx, id, in)
	if errors.Is(err, themes.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "theme_not_found", "theme not found", nil)
		return
	}
	if errors.Is(err, themes.ErrNameTaken) {
		writeProblem(w, http.StatusUnprocessableEntity, "name_taken",
			"a theme with this name already exists", nil)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "patch theme failed",
			"themeId", id.String(), "userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// Delete handles DELETE /api/v1/themes/{id}.
//
// @Summary Delete a theme
// @Description Hard-deletes one of the caller's own themes; another user's theme answers 404 theme_not_found, as a nonexistent id (INV-3, sk-674.18 T035). The ON DELETE SET NULL on users.custom_theme_id means any user pointing at this theme cleanly degrades back to mode=builtin on their next /me/theme read (the column becomes NULL but mode stays "custom"; the frontend falls back to builtinTheme).
// @Tags Themes
// @Security ApiKeyAuth
// @Param id path string true "Theme UUID"
// @Success 204 {object} nil "Deleted"
// @Failure 400 {object} map[string]any "invalid_id — the path id is not a UUID"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 404 {object} map[string]any "theme_not_found — absent or another user's, one answer"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/themes/{id} [delete]
func (h *ThemesHandler) Delete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	id, err := parseUUIDParam(r, "id")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_id", err.Error(), nil)
		return
	}
	if err := h.store.DeleteTheme(ctx, id, c.UserID); err != nil {
		if errors.Is(err, themes.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "theme_not_found", "theme not found", nil)
			return
		}
		slog.ErrorContext(ctx, "delete theme failed",
			"themeId", id.String(), "userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
