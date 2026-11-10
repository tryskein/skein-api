package authzsql

import (
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"skein-api/internal/apikeys"
)

// KeyLimit is the scope of the API key a request authenticated with
// (sk-878.3.1 D-3, INV-1, INV-2): the projects the key reaches, as a narrowing
// of its owner's relation. An object outside it behaves exactly as an object
// of a project the owner does not see.
//
// The zero value limits nothing: a request not authenticated by a key, or a
// key with scope "all". A key request whose grant was not carried over is
// denied — it reaches nothing (absence never means "no limit").
type KeyLimit struct {
	deny     bool
	mode     apikeys.ScopeMode
	projects []uuid.UUID
	spaces   []uuid.UUID
}

// KeyLimitOf turns a key grant's scope into the limit it imposes.
func KeyLimitOf(g apikeys.Grant) KeyLimit {
	switch g.Scope.Mode {
	case apikeys.ScopeProjects:
		return KeyLimit{mode: apikeys.ScopeProjects, projects: nonNil(g.Scope.Projects)}
	case apikeys.ScopeSpaces:
		return KeyLimit{mode: apikeys.ScopeSpaces, spaces: nonNil(g.Scope.Spaces)}
	case apikeys.ScopeAll:
		return KeyLimit{}
	}
	// A mode outside the closed set reaches nothing.
	return KeyLimit{deny: true}
}

func nonNil(ids []uuid.UUID) []uuid.UUID {
	if ids == nil {
		return []uuid.UUID{}
	}
	return ids
}

// DeniedKeyLimit is the limit of a key request whose grant was not carried
// over: it reaches nothing.
func DeniedKeyLimit() KeyLimit { return KeyLimit{deny: true} }

// Limits reports whether the limit narrows anything.
func (k KeyLimit) Limits() bool { return k.deny || k.mode != "" }

// Expr renders the SQL boolean «the project projectExpr is inside the key's
// scope», binding its values through bind (one placeholder at most). Without
// a limit it is the literal true and binds nothing, so a query of a request
// without a key — or with scope "all" — is unchanged; a denied limit is the
// literal false. Scope "spaces" reaches the projects of the chosen spaces.
func (k KeyLimit) Expr(projectExpr string, bind func(value any) string) string {
	switch {
	case k.deny:
		return "false"
	case k.mode == apikeys.ScopeProjects:
		return "(" + projectExpr + " = ANY(" + bind(k.projects) + "::uuid[]))"
	case k.mode == apikeys.ScopeSpaces:
		return "EXISTS (SELECT 1 FROM projects p_kl WHERE p_kl.id = " + projectExpr +
			" AND p_kl.space_id = ANY(" + bind(k.spaces) + "::uuid[]))"
	}
	return "true"
}

// Fingerprint identifies the limit inside one request's memo.
func (k KeyLimit) Fingerprint() string {
	if k.deny {
		return "deny"
	}
	var b strings.Builder
	b.WriteString(string(k.mode))
	for _, ids := range [][]uuid.UUID{k.projects, k.spaces} {
		sorted := slices.Clone(ids)
		slices.SortFunc(sorted, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
		for _, id := range sorted {
			b.WriteByte(',')
			b.WriteString(id.String())
		}
		b.WriteByte(';')
	}
	return b.String()
}

// Placeholders is a bind function for Expr over a positional argument list:
// each value is appended to *args and named by its position, so the
// placeholders continue the query's own numbering.
func Placeholders(args *[]any) func(value any) string {
	return func(value any) string {
		*args = append(*args, value)
		return "$" + strconv.Itoa(len(*args))
	}
}
