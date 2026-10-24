// Package themes owns the persistence + business rules for sk-10.1
// user-customizable CSS themes. Two collaborating surfaces:
//
//   - `themes` table — every saved theme + its CSS variable map.
//     CRUD over /api/v1/themes (handler.ThemesHandler).
//   - users.theme_mode / builtin_theme / custom_theme_id — the caller's
//     active selection. Read/written via /api/v1/me/theme
//     (handler.MeHandler.GetTheme / PutTheme).
//
// Validation lives in validate.go so handlers stay thin. Variable
// values are constrained ({non-empty after trim, max 256 chars, no
// `;` `<` `>`}) but keys are not validated against a fixed list — the
// frontend may add new CSS custom properties without a schema change.
package themes

import (
	"time"

	"github.com/google/uuid"
)

// UserRef is the embedded subset of a user surfaced on every theme DTO
// — id + username, no email / role / deactivation status. Matches the
// shape the frontend's existing user-pill component expects so the
// theme picker can render attribution without a follow-up lookup.
type UserRef struct {
	ID       uuid.UUID `json:"id"`
	Username string    `json:"username"`
}

// ThemeDTO is the wire shape returned by GET/POST/PATCH /themes and
// nested in the user-theme-pref response when mode = 'custom'.
//
// Variables is a map of CSS custom property names → string values. The
// keys are exactly what the frontend writes to document.documentElement
// via `style.setProperty(...)`; values are typically colour strings
// (`#fdf6e3`, `rgba(...)`) but may also be lengths, fonts, or shadows.
// We don't validate keys against a fixed list — adding a new variable
// in the frontend MUST NOT require a backend change. Values are
// constrained to non-empty trimmed strings ≤256 chars that don't
// contain `;` `<` `>` so a malicious save can't ever inject a CSS
// statement terminator or an HTML tag if the value is ever spliced
// into HTML (defence-in-depth — the frontend uses style.setProperty
// which does its own escaping).
type ThemeDTO struct {
	ID        uuid.UUID         `json:"id"`
	Name      string            `json:"name"`
	Base      string            `json:"base"`
	Variables map[string]string `json:"variables"`
	CreatedBy UserRef           `json:"createdBy"`
	CreatedAt time.Time         `json:"createdAt"`
	UpdatedAt time.Time         `json:"updatedAt"`
	UpdatedBy UserRef           `json:"updatedBy"`
}

// UserThemePrefDTO is the wire shape for /api/v1/me/theme. mode
// branches the rest of the payload:
//
//   - mode = "builtin" → frontend uses builtinTheme ('light'|'dark')
//     and ignores customThemeId.
//   - mode = "custom"  → frontend resolves customThemeId via GET
//     /themes/{id} (or its cached copy) and applies its variables map.
//     builtinTheme is still surfaced so the frontend can fall back
//     to a known builtin if the custom theme has been deleted.
//
// customThemeId is a pointer so 'null' on the wire is unambiguous
// (vs the zero uuid which would also be a valid Postgres value).
type UserThemePrefDTO struct {
	Mode          string     `json:"mode"`
	BuiltinTheme  string     `json:"builtinTheme"`
	CustomThemeID *uuid.UUID `json:"customThemeId"`
}

// CreateThemeInput is the validated input for Store.CreateTheme. Name
// must already be trimmed; Variables is the canonical map (every key
// is the raw CSS custom property, every value is trim-validated).
type CreateThemeInput struct {
	Name      string
	Base      string
	Variables map[string]string
	CreatedBy uuid.UUID
}

// UpdateThemeInput is the partial-patch shape for Store.UpdateTheme.
// Nil fields are skipped. Variables is *map[string]string so the
// handler can distinguish "leave alone" from "replace with whatever
// was sent" (we replace, not merge — partial-merge semantics on a
// freeform map are surprising and the frontend always sends the full
// map anyway).
type UpdateThemeInput struct {
	Name      *string
	Base      *string
	Variables *map[string]string
	UpdatedBy uuid.UUID
}

// SetThemePrefInput is the input for Store.SetUserThemePref. mode +
// builtinTheme are required; customThemeID is required iff
// mode == "custom" — the store re-validates and returns
// ErrCustomThemeRequired otherwise.
type SetThemePrefInput struct {
	Mode          string
	BuiltinTheme  string
	CustomThemeID *uuid.UUID
}
