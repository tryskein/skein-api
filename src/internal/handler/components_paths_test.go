package handler

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/projects"
)

// countingRepoLister is a fake repoLister that records how many times the
// project's git_repos[] was read. The whole point of the batched
// validatePaths is that the count is 1 for any number of paths (and 0
// when nothing needs checking), so the counter is the assertion.
type countingRepoLister struct {
	repos []projects.Repo
	err   error
	calls int
}

func (f *countingRepoLister) ListRepos(_ context.Context, _ uuid.UUID) ([]projects.Repo, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.repos, nil
}

// TestValidatePaths verifies the per-path error contract (code, message
// with the offending index, and the order in which the three checks
// fire) together with the read-once invariant.
//
// The error codes and messages are part of the REST contract — they go
// out verbatim as the problem-detail body — so every case pins the exact
// string, not just the code.
func TestValidatePaths(t *testing.T) {
	repoA := uuid.Must(uuid.NewV7())
	repoB := uuid.Must(uuid.NewV7())
	repoC := uuid.Must(uuid.NewV7())
	known := []projects.Repo{
		{ID: repoA, Name: "api", URL: "https://example.com/api.git"},
		{ID: repoB, Name: "web", URL: "https://example.com/web.git"},
		{ID: repoC, Name: "ops", URL: "https://example.com/ops.git"},
	}
	missing := uuid.Must(uuid.NewV7())

	tests := []struct {
		name string
		raw  []componentPathRequest
		// wantPaths is the expected canonical output length on success.
		wantPaths int
		wantCode  string
		wantMsg   string
		// wantCalls is how many times git_repos[] may be read.
		wantCalls int
	}{
		{
			name:      "empty paths issues no read",
			raw:       nil,
			wantPaths: 0,
			wantCalls: 0,
		},
		{
			name:      "single valid path reads once",
			raw:       []componentPathRequest{{RepoID: repoA.String(), Glob: "cmd/**"}},
			wantPaths: 1,
			wantCalls: 1,
		},
		{
			name: "three valid paths still read once",
			raw: []componentPathRequest{
				{RepoID: repoA.String(), Glob: "cmd/**"},
				{RepoID: repoB.String(), Glob: "src/**"},
				{RepoID: repoC.String(), Glob: "deploy/**"},
			},
			wantPaths: 3,
			wantCalls: 1,
		},
		{
			name: "repeated repoId across paths still reads once",
			raw: []componentPathRequest{
				{RepoID: repoA.String(), Glob: "a/**"},
				{RepoID: repoA.String(), Glob: "b/**"},
				{RepoID: repoA.String(), Glob: "c/**"},
			},
			wantPaths: 3,
			wantCalls: 1,
		},
		{
			name: "bad glob on first path short-circuits before any read",
			raw: []componentPathRequest{
				{RepoID: repoA.String(), Glob: ""},
				{RepoID: repoB.String(), Glob: "src/**"},
			},
			wantCode:  "invalid_glob",
			wantMsg:   "glob is required",
			wantCalls: 0,
		},
		{
			name: "malformed repoId on first path short-circuits before any read",
			raw: []componentPathRequest{
				{RepoID: "not-a-uuid", Glob: "cmd/**"},
				{RepoID: repoB.String(), Glob: "src/**"},
			},
			wantCode:  "invalid_repo_id",
			wantMsg:   "paths[0].repoId is not a UUID",
			wantCalls: 0,
		},
		{
			name: "malformed repoId reports its own index",
			raw: []componentPathRequest{
				{RepoID: repoA.String(), Glob: "cmd/**"},
				{RepoID: repoB.String(), Glob: "src/**"},
				{RepoID: "nope", Glob: "deploy/**"},
			},
			wantCode:  "invalid_repo_id",
			wantMsg:   "paths[2].repoId is not a UUID",
			wantCalls: 1,
		},
		{
			name: "unknown repo reports its own index",
			raw: []componentPathRequest{
				{RepoID: repoA.String(), Glob: "cmd/**"},
				{RepoID: missing.String(), Glob: "src/**"},
				{RepoID: repoB.String(), Glob: "deploy/**"},
			},
			wantCode:  "repo_not_found",
			wantMsg:   "paths[1].repoId does not exist in project's git_repos",
			wantCalls: 1,
		},
		{
			name: "glob check precedes the repo existence check",
			raw: []componentPathRequest{
				{RepoID: missing.String(), Glob: ""},
			},
			wantCode:  "invalid_glob",
			wantMsg:   "glob is required",
			wantCalls: 0,
		},
		{
			name: "malformed repoId precedes the repo existence check",
			raw: []componentPathRequest{
				{RepoID: "still-not-a-uuid", Glob: "cmd/**"},
			},
			wantCode:  "invalid_repo_id",
			wantMsg:   "paths[0].repoId is not a UUID",
			wantCalls: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			lister := &countingRepoLister{repos: known}
			got, err := validatePaths(context.Background(), lister, uuid.Must(uuid.NewV7()), tt.raw)

			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("validatePaths(%d paths) = error %v, want nil", len(tt.raw), err)
				}
				if len(got) != tt.wantPaths {
					t.Errorf("len(paths) = %d, want %d", len(got), tt.wantPaths)
				}
				for i := range got {
					if got[i].Glob != tt.raw[i].Glob {
						t.Errorf("paths[%d].Glob = %q, want %q", i, got[i].Glob, tt.raw[i].Glob)
					}
					if got[i].RepoID.String() != tt.raw[i].RepoID {
						t.Errorf("paths[%d].RepoID = %s, want %s", i, got[i].RepoID, tt.raw[i].RepoID)
					}
				}
			} else {
				ve, ok := projects.IsValidationError(err)
				if !ok {
					t.Fatalf("validatePaths error = %v, want *projects.ValidationError", err)
				}
				if ve.Code != tt.wantCode {
					t.Errorf("code = %q, want %q", ve.Code, tt.wantCode)
				}
				if ve.Message != tt.wantMsg {
					t.Errorf("message = %q, want %q", ve.Message, tt.wantMsg)
				}
				if got != nil {
					t.Errorf("paths = %v, want nil on error", got)
				}
			}

			if lister.calls != tt.wantCalls {
				t.Errorf("git_repos reads = %d, want %d (one read per call at most, "+
					"regardless of path count)", lister.calls, tt.wantCalls)
			}
		})
	}
}

// TestValidatePaths_ProjectMissingPropagatesRaw pins that a store-level
// failure is NOT reshaped into a per-path validation error: the handler
// maps a non-ValidationError to 500, and projects.ErrNotFound must stay
// matchable with errors.Is so that mapping is unchanged.
func TestValidatePaths_ProjectMissingPropagatesRaw(t *testing.T) {
	lister := &countingRepoLister{err: projects.ErrNotFound}
	_, err := validatePaths(context.Background(), lister, uuid.Must(uuid.NewV7()),
		[]componentPathRequest{{RepoID: uuid.Must(uuid.NewV7()).String(), Glob: "cmd/**"}})

	if !errors.Is(err, projects.ErrNotFound) {
		t.Fatalf("err = %v, want projects.ErrNotFound", err)
	}
	if _, ok := projects.IsValidationError(err); ok {
		t.Fatalf("store failure must not surface as a 400 validation error")
	}
	if lister.calls != 1 {
		t.Fatalf("git_repos reads = %d, want 1", lister.calls)
	}
}

// TestValidatePaths_EmptyProjectRepos: a project with zero declared
// repos rejects the first path with repo_not_found — and still reads
// git_repos[] exactly once (the empty-but-loaded set must not re-trigger
// the lazy load on the next iteration).
func TestValidatePaths_EmptyProjectRepos(t *testing.T) {
	lister := &countingRepoLister{repos: nil}
	_, err := validatePaths(context.Background(), lister, uuid.Must(uuid.NewV7()),
		[]componentPathRequest{
			{RepoID: uuid.Must(uuid.NewV7()).String(), Glob: "a/**"},
			{RepoID: uuid.Must(uuid.NewV7()).String(), Glob: "b/**"},
		})

	ve, ok := projects.IsValidationError(err)
	if !ok {
		t.Fatalf("err = %v, want *projects.ValidationError", err)
	}
	if ve.Code != "repo_not_found" || ve.Message != "paths[0].repoId does not exist in project's git_repos" {
		t.Fatalf("got %q / %q, want repo_not_found / paths[0]...", ve.Code, ve.Message)
	}
	if lister.calls != 1 {
		t.Fatalf("git_repos reads = %d, want 1", lister.calls)
	}
}
