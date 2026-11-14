package publicapi

// kind.go — the kind of a write (spec "General rules of the surface",
// OST-2-8): absent, the record's origin is api; directed and autonomous give
// the same origins, and so the same side effects (the author's auto-watch,
// the suppression of agent notifications, the default assignee), as the
// kind of the same MCP tool. The mapping itself is the issues package's
// (issues.SourceForKeyWrite, issues.CommentAuthorKindForKeyWrite).

import (
	"errors"

	"skein-api/internal/issues"
)

// ErrBadKind refuses a kind outside directed | autonomous (400), an empty
// one included: a kind sent must name one.
var ErrBadKind = errors.New(`kind must be "directed" or "autonomous"`)

// WriteOrigin is the origin a write records: the source of an issue and the
// author kind of a comment.
type WriteOrigin struct {
	Source            string
	CommentAuthorKind string
}

// ParseKind maps the kind of a write to its origin. kind is nil when the
// body has no kind.
func ParseKind(kind *string) (WriteOrigin, error) {
	k := ""
	if kind != nil {
		if *kind == "" {
			return WriteOrigin{}, ErrBadKind
		}
		k = *kind
	}
	source, err := issues.SourceForKeyWrite(k)
	if err != nil {
		return WriteOrigin{}, ErrBadKind
	}
	authorKind, err := issues.CommentAuthorKindForKeyWrite(k)
	if err != nil {
		return WriteOrigin{}, ErrBadKind
	}
	return WriteOrigin{Source: source, CommentAuthorKind: authorKind}, nil
}
