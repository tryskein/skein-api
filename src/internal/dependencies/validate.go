package dependencies

import (
	"errors"
	"fmt"
	"strings"
)

// ValidationError mirrors issues.ValidationError / projects.ValidationError
// — handlers branch on a typed error and emit a consistent
// {"error":"<code>"} body. Code is stable, machine-readable; never
// localise it.
type ValidationError struct {
	Code    string
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

func newValidationError(code, msg string) error {
	return &ValidationError{Code: code, Message: msg}
}

// IsValidationError unwraps to *ValidationError.
func IsValidationError(err error) (*ValidationError, bool) {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return ve, true
	}
	return nil, false
}

// validTypes is the closed set of dependency types — mirrors the DB CHECK
// constraint in 000005_dependencies.up.sql, re-opened in 000022 to
// restore parent-child as the canonical hierarchy edge (sk-35). Any
// other value is rejected at the handler boundary with code
// "invalid_dep_type".
var validTypes = map[string]struct{}{
	DepBlocks:         {},
	DepRelated:        {},
	DepDiscoveredFrom: {},
	DepSupersedes:     {},
	DepDuplicates:     {},
	DepWaitsFor:       {},
	DepParentChild:    {},
}

// validGates is the closed set of waits-for gate values. Other types
// reject any gate at all.
var validGates = map[string]struct{}{
	GateAllChildren: {},
	GateAnyChildren: {},
}

// ValidateType normalises the input (trim) and rejects unknown types.
// parent-child is accepted again (sk-35) as the canonical hierarchy edge.
func ValidateType(t string) (string, error) {
	t = strings.TrimSpace(t)
	if _, ok := validTypes[t]; !ok {
		return "", newValidationError("invalid_dep_type",
			fmt.Sprintf("type must be one of blocks, related, discovered-from, supersedes, duplicates, waits-for, parent-child (got %q)", t))
	}
	return t, nil
}

// ValidateGate enforces the gate↔type relationship:
//
//   - gate=nil is always allowed (default empty metadata)
//   - gate set + type='waits-for' → must be one of GateAll/AnyChildren
//   - gate set + any other type   → 400 invalid_gate (gate is meaningless
//     outside waits-for)
//
// Returns the canonical (already trimmed) gate value or empty string when
// no gate was supplied.
func ValidateGate(depType string, gate *string) (string, error) {
	if gate == nil || strings.TrimSpace(*gate) == "" {
		return "", nil
	}
	g := strings.TrimSpace(*gate)
	if depType != DepWaitsFor {
		return "", newValidationError("invalid_gate",
			"gate is only allowed on waits-for dependencies")
	}
	if _, ok := validGates[g]; !ok {
		return "", newValidationError("invalid_gate",
			fmt.Sprintf("gate must be one of all-children, any-children (got %q)", g))
	}
	return g, nil
}
