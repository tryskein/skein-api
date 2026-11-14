package publicapi

// occ.go — optimistic concurrency of the public API (US-6): ETag carries the
// version of an issue or a comment; If-Match on the operations the route
// table marks is checked before the operation runs.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"skein-api/internal/errcode"
)

// VersionProvider reads the current version of the resource a request
// targets (the issue or the comment of its path). It is a dependency of the
// router: the route table says whether a route checks If-Match, the
// provider knows how to read the version.
type VersionProvider interface {
	// ResourceVersion returns the current version of the resource of r.
	// found is false when the resource does not exist or is outside the
	// caller's reach; the router then lets the operation answer as it does
	// for a missing object.
	ResourceVersion(ctx context.Context, rt Route, r *http.Request) (version int, found bool, err error)
}

// expectedVersionKey carries the version If-Match named.
type expectedVersionKey struct{}

// ExpectedVersion returns the version of If-Match, checked against the
// current version before the operation ran. The operation passes it on as
// its expected version, so a write that raced in between still answers
// version_conflict.
func ExpectedVersion(ctx context.Context) (int, bool) {
	v, ok := ctx.Value(expectedVersionKey{}).(int)
	return v, ok
}

// SetETag sets the ETag of a response to the version of its resource. The
// ETag validates If-Match; it is not a validator of the representation (a
// label or the review flag change without a new version), so no 304 is
// answered from it.
func SetETag(w http.ResponseWriter, version int) {
	w.Header().Set("ETag", `"`+strconv.Itoa(version)+`"`)
}

var errBadIfMatch = errors.New(`If-Match must be one version, as the ETag gives it ("5") or as a number`)

// parseIfMatch reads If-Match: one entity tag, "N", W/"N" is refused (a weak
// tag never matches in If-Match, RFC 9110 section 13.1.1), a bare N (the
// version field of a response) is accepted.
func parseIfMatch(values []string) (int, error) {
	joined := strings.TrimSpace(strings.Join(values, ","))
	if joined == "" || strings.Contains(joined, ",") {
		return 0, errBadIfMatch
	}
	tag := joined
	if strings.HasPrefix(tag, `"`) {
		if len(tag) < 3 || !strings.HasSuffix(tag, `"`) {
			return 0, errBadIfMatch
		}
		tag = tag[1 : len(tag)-1]
	}
	v, err := strconv.Atoi(tag)
	if err != nil || v < 1 || strconv.Itoa(v) != tag {
		return 0, errBadIfMatch
	}
	return v, nil
}

// checkIfMatch applies the If-Match rule of rt. It answers the refusal and
// returns false, or returns the request to serve.
func checkIfMatch(w http.ResponseWriter, r *http.Request, rt Route, versions VersionProvider) (*http.Request, bool) {
	if rt.IfMatch == IfMatchNone {
		return r, true
	}
	values, present := r.Header["If-Match"]
	if !present {
		if rt.IfMatch == IfMatchRequired {
			WriteProblem(w, Problem{Code: errcode.PreconditionRequired, Origin: OriginParse,
				Detail: "this operation requires If-Match with the version of the resource (its ETag)"})
			return nil, false
		}
		return r, true
	}
	expected, err := parseIfMatch(values)
	if err != nil {
		WriteProblem(w, Problem{Code: errcode.ValidationError, Origin: OriginParse, Detail: err.Error()})
		return nil, false
	}
	current, found, err := versions.ResourceVersion(r.Context(), rt, r)
	if err != nil {
		writeInternal(w, r, rt, "reading the version of the resource", err)
		return nil, false
	}
	if found && current != expected {
		WriteProblem(w, Problem{Code: errcode.VersionConflict, Origin: OriginDomain,
			Extensions: map[string]any{"currentVersion": current}})
		return nil, false
	}
	return r.WithContext(context.WithValue(r.Context(), expectedVersionKey{}, expected)), true
}
