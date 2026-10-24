package issues

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"skein-api/internal/textvalidate"
)

// ValidationError mirrors projects.ValidationError so handlers can branch
// on a typed error and emit a consistent {"error":"<code>"} body. Code is a
// stable, machine-readable identifier — never localise it.
type ValidationError struct {
	Code    string
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

func newValidationError(code, msg string) error {
	return &ValidationError{Code: code, Message: msg}
}

// IsValidationError unwraps to *ValidationError. Convenience for handlers.
func IsValidationError(err error) (*ValidationError, bool) {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return ve, true
	}
	return nil, false
}

// validStatuses is the closed set of issues.status values mirroring the
// DB CHECK constraint. Using a set lookup makes the validator a one-line
// branch and lets us report the offending value back in the message.
//
// sk-31: `blocked` is no longer a manual status. Blocked-ness is derived
// at query time from the dependency graph (see the EXISTS predicate in
// store.go). The validator rejects status='blocked' on the wire; callers
// who want the blocked label add a typed-deps edge (skein_dep_add).
var validStatuses = map[string]struct{}{
	StatusOpen:       {},
	StatusInProgress: {},
	StatusDeferred:   {},
	StatusClosed:     {},
	StatusStanding:   {}, // sk-118 (F2a): standing is a valid stored status.
}

// ValidStatuses returns the closed set of stored issue statuses, sorted.
//
// Exported so downstream packages that must render or branch on EVERY status
// can enumerate them from this one source instead of re-typing the list. A
// hand-copied list drifts silently: internal/outbox's push titles shipped
// without a `standing` label for exactly that reason, and its guard test now
// iterates this function so the next added status fails loudly instead.
func ValidStatuses() []string {
	out := make([]string, 0, len(validStatuses))
	for s := range validStatuses {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

var validTypes = map[string]struct{}{
	TypeBug:      {},
	TypeFeature:  {},
	TypeTask:     {},
	TypeEpic:     {},
	TypeChore:    {},
	TypeDecision: {},
	TypeIdea:     {},
	TypeNote:     {},
}

var validSources = map[string]struct{}{
	SourceHuman:           {},
	SourceAgentDirected:   {},
	SourceAgentAutonomous: {},
}

// ValidateStatus enforces the writable status set
// {open, in_progress, deferred, closed, standing} (sk-31 dropped
// 'blocked' — it stays derived from the dep graph; sk-118 added
// 'standing'). Returns the canonical (already-lowercase) value or a
// ValidationError with code "invalid_status". When the caller passes
// the legacy 'blocked' value we surface a dedicated code
// 'blocked_is_derived' with an actionable message pointing at
// skein_dep_add, so agents/UI can branch without parsing the prose.
func ValidateStatus(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == StatusBlocked {
		return "", newValidationError("blocked_is_derived",
			"blocked is a derived label from the dependency graph; "+
				"use skein_dep_add(fromId=<this>, toId=<blocker>, type='blocks') "+
				"instead of writing status='blocked' directly")
	}
	if _, ok := validStatuses[s]; !ok {
		return "", newValidationError("invalid_status",
			"status must be one of open, in_progress, deferred, closed, standing")
	}
	return s, nil
}

// ValidateType enforces the 8-type closed set.
func ValidateType(s string) (string, error) {
	s = strings.TrimSpace(s)
	if _, ok := validTypes[s]; !ok {
		return "", newValidationError("invalid_type",
			"type must be one of bug, feature, task, epic, chore, decision, idea, note")
	}
	return s, nil
}

// ValidateSource enforces the closed set
// {human, agent_directed, agent_autonomous}. The REST handler forces
// source='human' regardless of body contents (Phase 1, ui-redesign);
// the MCP path infers agent_directed / agent_autonomous from the
// caller-supplied `kind` parameter. ValidateSource exists for the
// list / filter endpoints that pass user-supplied source through to
// the WHERE clause.
func ValidateSource(s string) (string, error) {
	s = strings.TrimSpace(s)
	if _, ok := validSources[s]; !ok {
		return "", newValidationError("invalid_source",
			"source must be one of human, agent_directed, agent_autonomous")
	}
	return s, nil
}

// ValidatePriority enforces the 0..4 range mirroring the CHECK constraint.
func ValidatePriority(p int) error {
	if p < 0 || p > 4 {
		return newValidationError("invalid_priority",
			"priority must be in [0, 4]")
	}
	return nil
}

// ValidateTitle trims whitespace and enforces 1..1000 chars. The DB does
// not bound title length; we cap at 1000 in the app layer to keep the
// trigram index reasonable and prevent absurd titles.
func ValidateTitle(s string) (string, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return "", newValidationError("invalid_title", "title is required")
	}
	if len(t) > 1000 {
		return "", newValidationError("invalid_title",
			"title must be at most 1000 characters")
	}
	if err := ValidateProse("title", t); err != nil {
		return "", err
	}
	return t, nil
}

// customSuffixRegex is the validation rule for custom IDs. Suffix only —
// the {prefix}-{suffix} concatenation is built later. Lowercase letters
// digits, hyphen, underscore, dot. First char must be alphanumeric. Length
// 1..500 (vision §6: ≤500 chars).
//
// Rationale on '.': vision §3 explicitly allows '.' in custom IDs ("demo-42.1"
// hierarchical, "demo-migration-042" custom). We disallow '/' (URL safety),
// whitespace and quote characters per F30. The regex enforces that.
var customSuffixRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9_.\-]{0,499}$`)

// ValidateCustomSuffix validates a user-supplied custom-mode ID suffix.
// Whole-ID validation (including the prefix) is in ValidateCustomID.
func ValidateCustomSuffix(s string) (string, error) {
	t := strings.TrimSpace(strings.ToLower(s))
	if t == "" {
		return "", newValidationError("invalid_id", "id suffix is required")
	}
	if !customSuffixRegex.MatchString(t) {
		return "", newValidationError("invalid_id",
			"id suffix must match ^[a-z0-9][a-z0-9_.-]{0,499}$")
	}
	return t, nil
}

// ValidateCustomID checks that the proposed full ID for a custom-mode
// project matches `{prefix}-{suffix}` exactly. Returns the canonical form
// (already lowercase) and the suffix portion separately so the caller can
// log either piece.
func ValidateCustomID(id, prefix string) (canonical, suffix string, err error) {
	t := strings.TrimSpace(strings.ToLower(id))
	expected := strings.ToLower(prefix) + "-"
	if !strings.HasPrefix(t, expected) {
		return "", "", newValidationError("invalid_id",
			fmt.Sprintf("custom id must start with '%s'", expected))
	}
	suff := strings.TrimPrefix(t, expected)
	canonicalSuffix, err := ValidateCustomSuffix(suff)
	if err != nil {
		return "", "", err
	}
	return expected + canonicalSuffix, canonicalSuffix, nil
}

// ValidateProse runs the tool-call wrapper-leak guard against a free-text
// field value (description / notes / design / acceptanceCriteria /
// sourceContext / externalRef / reason / body). A bare wrapper does not
// trim or length-check — callers compose with the field-specific
// validators (ValidateTitle / ValidateLabel / …) when both are needed.
// Returns *ValidationError{Code: textvalidate.ToolCallSyntaxCode} on match
// so the existing handler error pipeline (typed *ValidationError → 422 +
// JSON {"error":"<code>"}) covers it uniformly.
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

// validateCreateInputProse is the store-level backstop for CreateIssue.
// MCP tool handlers already validate at the boundary; REST handlers funnel
// raw struct fields straight into Store.CreateIssue, so we re-run the guard
// here to cover the REST path uniformly without touching N handler files.
func validateCreateInputProse(input CreateIssueInput) error {
	checks := []struct{ field, val string }{
		{"title", input.Title},
		{"description", input.Description},
		{"design", input.Design},
		{"acceptanceCriteria", input.AcceptanceCriteria},
		{"notes", input.Notes},
	}
	for _, c := range checks {
		if err := ValidateProse(c.field, c.val); err != nil {
			return err
		}
	}
	for _, p := range []struct {
		field string
		val   *string
	}{
		{"sourceContext", input.SourceContext},
		{"externalRef", input.ExternalRef},
	} {
		if p.val == nil {
			continue
		}
		if err := ValidateProse(p.field, *p.val); err != nil {
			return err
		}
	}
	return nil
}

// validatePatchInputProse is the store-level backstop for PatchIssue.
// Pointer-typed fields signal "absent" with nil — we only validate when
// the caller actually supplied a value.
func validatePatchInputProse(input PatchIssueInput) error {
	checks := []struct {
		field string
		val   *string
	}{
		{"title", input.Title},
		{"description", input.Description},
		{"design", input.Design},
		{"acceptanceCriteria", input.AcceptanceCriteria},
		{"notes", input.Notes},
		{"externalRef", input.ExternalRef},
		{"sourceContext", input.SourceContext},
		{"assignee", input.Assignee},
	}
	for _, c := range checks {
		if c.val == nil {
			continue
		}
		if err := ValidateProse(c.field, *c.val); err != nil {
			return err
		}
	}
	return nil
}

// ValidateLabel trims surrounding whitespace and enforces non-empty +
// length cap. We deliberately do NOT lowercase — labels are case-preserved
// per db-schema.md §2.15.
func ValidateLabel(s string) (string, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return "", newValidationError("invalid_label", "label is required")
	}
	if len(t) > 100 {
		return "", newValidationError("invalid_label",
			"label must be at most 100 characters")
	}
	return t, nil
}
