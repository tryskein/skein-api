package themes

import (
	"errors"
	"fmt"
	"strings"
)

// nameMaxLen caps the theme name. Same order of magnitude as
// projects.name (200) but tighter — themes are rendered in a picker
// dropdown and a 100-char name already wraps awkwardly. The DB
// UNIQUE constraint enforces collision; this is the length-only gate.
const nameMaxLen = 100

// variableValueMaxLen caps each CSS variable value. 256 is well past
// any realistic value (rgba(...), 24-char hex, shorthand transition
// strings all fit comfortably) but small enough that the full
// variables map can't grow unbounded — even with hundreds of keys the
// row stays comfortably under TOAST thresholds.
const variableValueMaxLen = 256

// forbiddenValueChars are the substrings disallowed in a variable
// value. The frontend writes values via document.documentElement
// .style.setProperty(name, value), which is its own escape boundary,
// but XSS-grade defence-in-depth costs us nothing: blocking `;` `<`
// `>` rules out CSS statement-terminator games AND any chance an HTML
// renderer that ever picked up a raw value could splice in markup.
var forbiddenValueChars = []string{";", "<", ">"}

// validBases is the closed set of allowed `base` values. Matches the
// DB CHECK constraint exactly so an out-of-set value never reaches
// the INSERT path.
var validBases = map[string]struct{}{
	"light": {},
	"dark":  {},
}

// validModes is the closed set of allowed user-pref `mode` values.
// Mirrors the DB CHECK on users.theme_mode.
var validModes = map[string]struct{}{
	"builtin": {},
	"custom":  {},
}

// ValidationError is the typed return for every validation helper.
// Code is a stable machine-readable string the handler maps to the
// REST error envelope; Message is human-readable.
type ValidationError struct {
	Code    string
	Message string
}

func (e *ValidationError) Error() string { return e.Code + ": " + e.Message }

// IsValidationError unwraps `err` into a ValidationError if present.
// Mirrors projects.IsValidationError so the existing handler-level
// mapValidation helper pattern can be reused 1:1.
func IsValidationError(err error) (*ValidationError, bool) {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return ve, true
	}
	return nil, false
}

// ValidateName trims and length-checks the supplied name. Returns the
// canonical (trimmed) form on success. Empty-after-trim is rejected
// with code "invalid_name"; over-length is the same code with a
// different message so the frontend can render a single error chip.
//
// Uniqueness is enforced at the DB layer via the UNIQUE constraint on
// themes.name; the store translates the resulting 23505 into the
// "name_taken" sentinel.
func ValidateName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", &ValidationError{Code: "invalid_name", Message: "name must not be empty"}
	}
	if len(name) > nameMaxLen {
		return "", &ValidationError{
			Code:    "invalid_name",
			Message: fmt.Sprintf("name must be at most %d characters", nameMaxLen),
		}
	}
	return name, nil
}

// ValidateBase rejects anything outside {"light","dark"}. The DB
// CHECK would catch the same case but at a worse layer (we want the
// 400 before the INSERT round-trip).
func ValidateBase(raw string) (string, error) {
	if _, ok := validBases[raw]; !ok {
		return "", &ValidationError{
			Code:    "invalid_base",
			Message: `base must be "light" or "dark"`,
		}
	}
	return raw, nil
}

// ValidateMode rejects anything outside {"builtin","custom"}.
func ValidateMode(raw string) (string, error) {
	if _, ok := validModes[raw]; !ok {
		return "", &ValidationError{
			Code:    "invalid_mode",
			Message: `mode must be "builtin" or "custom"`,
		}
	}
	return raw, nil
}

// ValidateVariables walks the map and enforces the per-value rules
// described on ThemeDTO. Returns a canonical (string-trimmed where
// applicable) copy. The map itself may be empty — a theme with zero
// variables is valid (it just inherits all the base theme's values
// on the frontend).
//
// We don't validate keys (the frontend may add new CSS custom
// properties without a schema change) but we do enforce non-emptiness
// after trim, since `""` keys would round-trip as junk JSON and serve
// no purpose. Forbidden characters in keys (`;` `<` `>`) are rejected
// for the same defence-in-depth reason as values — a key with a `;`
// could close a property in any naive CSS-string concatenation path.
func ValidateVariables(raw map[string]string) (map[string]string, error) {
	if raw == nil {
		return map[string]string{}, nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		key := strings.TrimSpace(k)
		if key == "" {
			return nil, &ValidationError{
				Code:    "invalid_variables",
				Message: "variable keys must not be empty",
			}
		}
		for _, ch := range forbiddenValueChars {
			if strings.Contains(key, ch) {
				return nil, &ValidationError{
					Code:    "invalid_variables",
					Message: fmt.Sprintf("variable keys must not contain %q", ch),
				}
			}
		}
		val := strings.TrimSpace(v)
		if val == "" {
			return nil, &ValidationError{
				Code:    "invalid_variables",
				Message: fmt.Sprintf("variable %q has empty value", key),
			}
		}
		if len(val) > variableValueMaxLen {
			return nil, &ValidationError{
				Code:    "invalid_variables",
				Message: fmt.Sprintf("variable %q exceeds %d characters", key, variableValueMaxLen),
			}
		}
		for _, ch := range forbiddenValueChars {
			if strings.Contains(val, ch) {
				return nil, &ValidationError{
					Code:    "invalid_variables",
					Message: fmt.Sprintf("variable %q value must not contain %q", key, ch),
				}
			}
		}
		out[key] = val
	}
	return out, nil
}
