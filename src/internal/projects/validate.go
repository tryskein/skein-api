package projects

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"skein-api/internal/textvalidate"
)

// idPrefixRegex mirrors the DB CHECK constraint exactly:
//
//	^[a-z0-9][a-z0-9_-]{0,49}$
//
// Lowercased alphanumeric / underscore / hyphen, length 1..50, first char
// must be alphanumeric. Compiled once at package init.
// The first character is a letter (sk-674.18 T025, US-15: a prefix never
// starts with a digit — «2fa-1» reads as a number); prefixes created before
// the rule keep working, the rule applies to new projects.
var idPrefixRegex = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,49}$`)

// CodePrefixInvalid is the refusal code of a prefix that breaks the prefix
// rule — the code of the spaces registry (spaces.CodePrefixInvalid), the
// same in REST and MCP.
const CodePrefixInvalid = "prefix_invalid"

// aliasRegex enforces the alias shape used at the API layer. Unicode word
// characters + hyphen + underscore, length 1..64. Cyrillic ("веб") matches.
// Note: Go's `\w` already covers letters/digits/underscore in any script
// when /u flag is implicit (RE2 always treats input as UTF-8).
var aliasRegex = regexp.MustCompile(`^[\p{L}\p{N}_][\p{L}\p{N}_-]{0,63}$`)

const (
	projectNameMinLen = 1

	// NameMaxLen is the upper bound ValidateProjectName enforces on a
	// project name, in BYTES — the check is len() over the trimmed
	// string, not a rune count, so a 200-byte Cyrillic name is ~100
	// characters.
	//
	// Exported because it is the only bound on anything that gets
	// prepended to the skein_get_instructions payload: that tool
	// advertises a maxResultSizeChars cap, and its guard test has to
	// size the "Project: <name>" header against the real limit rather
	// than a copy of the number that can drift out of sync with this
	// one.
	NameMaxLen = 200
)

// validIDModes is the closed set of project default_id_mode values, mirroring
// the DB CHECK constraint.
var validIDModes = map[string]struct{}{
	"counter": {},
	"hash":    {},
	"custom":  {},
}

// ValidationError is a typed error for input validation; carries an "error
// code" string suitable for inclusion in 400/422 ProblemDetails responses.
type ValidationError struct {
	Code    string
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

func newValidationError(code, msg string) error {
	return &ValidationError{Code: code, Message: msg}
}

// ValidateProjectName trims whitespace and enforces 1..200 char length.
// Project name is free-form text; callers may use any unicode characters.
func ValidateProjectName(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if len(trimmed) < projectNameMinLen {
		return "", newValidationError("invalid_name", "name is required")
	}
	if len(trimmed) > NameMaxLen {
		return "", newValidationError("invalid_name",
			fmt.Sprintf("name must be at most %d characters", NameMaxLen))
	}
	if err := ValidateProse("name", trimmed); err != nil {
		return "", err
	}
	return trimmed, nil
}

// ValidateProse runs the tool-call wrapper-leak guard against a free-text
// field value (project name / description / component name / component
// description). On match returns *ValidationError{Code:
// textvalidate.ToolCallSyntaxCode}; the handler error pipeline already
// renders typed *ValidationError as 422 + JSON {"error":"<code>"}.
func ValidateProse(field, value string) error {
	if err := textvalidate.ScanToolCallSyntax(field, value); err != nil {
		var tcse *textvalidate.ToolCallSyntaxError
		if errors.As(err, &tcse) {
			return newValidationError(textvalidate.ToolCallSyntaxCode, tcse.Error())
		}
		return err
	}
	return nil
}

// ValidateIDPrefix trims, lowercases, and checks the input against the DB
// CHECK regex. Returns the canonical (lowercased) form on success.
//
// Empty input is treated as "caller wants the default" — handler logic
// will fall back to Slug(name). This function only validates non-empty
// values.
func ValidateIDPrefix(prefix string) (string, error) {
	trimmed := strings.TrimSpace(strings.ToLower(prefix))
	if trimmed == "" {
		return "", newValidationError(CodePrefixInvalid, "id_prefix is empty")
	}
	if !idPrefixRegex.MatchString(trimmed) {
		return "", newValidationError(CodePrefixInvalid,
			"id_prefix must start with a letter and match ^[a-z][a-z0-9_-]{0,49}$")
	}
	return trimmed, nil
}

// ValidateAlias normalizes the alias to lowercase, trims surrounding
// whitespace, and runs the unicode-aware regex check. Returns the
// canonical lowercase form (matches DB PRIMARY KEY storage).
func ValidateAlias(alias string) (string, error) {
	trimmed := strings.TrimSpace(strings.ToLower(alias))
	if trimmed == "" {
		return "", newValidationError("invalid_alias", "alias is required")
	}
	if !aliasRegex.MatchString(trimmed) {
		return "", newValidationError("invalid_alias",
			"alias must be 1..64 unicode word chars / hyphen / underscore")
	}
	return trimmed, nil
}

// ValidateIDMode checks the input against the closed set
// {counter,hash,custom}. Empty input is treated as "no change requested" by
// the PATCH path; callers that need a default must pass "counter" explicitly.
func ValidateIDMode(mode string) (string, error) {
	if _, ok := validIDModes[mode]; !ok {
		return "", newValidationError("invalid_id_mode",
			"default_id_mode must be one of counter, hash, custom")
	}
	return mode, nil
}

// ValidateGlob compiles the glob via bmatcuk/doublestar to surface invalid
// patterns at request time. doublestar.Match returns an error on malformed
// input — we discard the boolean match result and only care about the err.
//
// The "test" probe string is arbitrary; we just need *some* path to feed
// the matcher so the pattern parser runs.
func ValidateGlob(glob string) error {
	if strings.TrimSpace(glob) == "" {
		return newValidationError("invalid_glob", "glob is required")
	}
	if _, err := doublestar.Match(glob, "test"); err != nil {
		return newValidationError("invalid_glob",
			fmt.Sprintf("invalid glob: %s", err.Error()))
	}
	return nil
}

// IsURLLike returns true when q looks like a git remote URL, so the resolve
// path can decide whether step 4 (repo-URL match) is worth running.
//
// Heuristics — we deliberately stay loose:
//   - "git@host:path" SSH form (contains '@' and ':')
//   - "ssh://", "http://", "https://" prefixes
//   - any string containing '://' with path content after
//
// Plain words like "demo-api" must NOT match — they're project-name candidates,
// not URLs.
func IsURLLike(q string) bool {
	s := strings.TrimSpace(q)
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "git@") {
		return strings.Contains(s, ":")
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "ssh://") {
		return true
	}
	return false
}

// IsValidationError unwraps to *ValidationError. Convenience for handlers
// that want to surface code/message in the response without type-asserting
// the error explicitly.
func IsValidationError(err error) (*ValidationError, bool) {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return ve, true
	}
	return nil, false
}
