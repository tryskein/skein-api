package publicapi

// versions.go — the VersionProvider of the product routes that check
// If-Match (US-6): the version of the issue of the path, read within the
// caller's reach. A route without {issue} has no version here; a missing
// issue or one out of reach is "not found", and the operation answers it as
// it answers a missing object.

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/issues"
	"skein-api/internal/projects"
)

// IssueVersions reads the version of the issue of a request's path.
type IssueVersions struct {
	Pool   *pgxpool.Pool
	Issues *issues.Store
}

// ResourceVersion implements VersionProvider.
func (v IssueVersions) ResourceVersion(ctx context.Context, _ Route, r *http.Request) (int, bool, error) {
	id := strings.TrimSpace(chi.URLParam(r, "issue"))
	if id == "" {
		return 0, false, nil
	}
	issue, err := v.Issues.GetIssue(ctx, id)
	if errors.Is(err, issues.ErrNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	_, err = projects.EnsureProjectVisible(ctx, v.Pool, issue.ProjectID, callerOf(r))
	if errors.Is(err, projects.ErrProjectNotVisible) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return issue.Version, true, nil
}
