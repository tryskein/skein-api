package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	skmcp "skein-api/internal/mcp"
	"skein-api/internal/projects"
	"skein-api/internal/spaces"
)

// AmbiguousProjectError is returned when a free-form project handle could
// not be resolved to a single project but produced two or more candidates.
// The tool layer surfaces these candidates to the agent so it can either
// re-query with a more specific handle or surface a choice to the user.
type AmbiguousProjectError struct {
	Query string
	// Candidates are the contract's candidates of CodeAmbiguousProject
	// (spaces/contract.go): prefix, name and space of every accessible
	// project that matched — the same body as REST (sk-674.18 T024).
	Candidates []spaces.AmbiguousProjectCandidate
}

// Error implements the error interface; the message is intentionally
// generic — the typed candidate list carries the actionable detail.
func (e *AmbiguousProjectError) Error() string {
	return fmt.Sprintf("ambiguous project handle %q: %d candidates", e.Query, len(e.Candidates))
}

// ValidationProjectError is returned when a project handle fails surface
// validation — for example, an empty or whitespace-only string. Handlers
// translate this into ErrCodeValidationError so the agent can distinguish
// "you sent garbage" from "the project doesn't exist".
type ValidationProjectError struct {
	Field   string
	Message string
}

func (e *ValidationProjectError) Error() string {
	return e.Field + ": " + e.Message
}

// ResolveProjectHandle takes a free-form project handle (UUID, name, or
// alias) and returns the project's UUID after running the same visibility
// rules the rest of the API uses.
//
// Resolution order:
//
//  1. Parse `q` as a UUID. If valid, look the row up directly and run
//     EnsureProjectVisible — invisible projects come back as
//     project_not_found per the "non-members must not learn that a
//     project exists" rule (vision §10).
//  2. Otherwise hand the handle to projects.Resolve. The resolver
//     applies visibility itself via the admin-OR-membership predicate
//     on every layer.
//
// Outcomes:
//
//   - exactly one match → return its UUID, nil.
//   - no match at all   → return *ProjectNotFoundError (sentinel).
//   - multiple candidates without a single auto-match winner →
//     return *AmbiguousProjectError with the candidate list.
//
// Tools never call projects.Resolve directly — they go through this
// helper so disambiguation and visibility checks stay consistent.
func ResolveProjectHandle(
	ctx context.Context,
	pool *pgxpool.Pool,
	store *projects.Store,
	handle string,
	userID uuid.UUID,
) (uuid.UUID, error) {
	q := strings.TrimSpace(handle)
	if q == "" {
		return uuid.Nil, &ValidationProjectError{
			Field:   "project",
			Message: "must be a non-empty handle",
		}
	}
	// UUID path — exact lookup + visibility check.
	if id, err := uuid.Parse(q); err == nil {
		if _, vErr := projects.EnsureProjectVisible(ctx, pool, id, userID); vErr != nil {
			if errors.Is(vErr, projects.ErrProjectNotVisible) {
				return uuid.Nil, &ProjectNotFoundError{Query: handle}
			}
			return uuid.Nil, fmt.Errorf("checking project visibility: %w", vErr)
		}
		return id, nil
	}

	// Name / id_prefix / alias / repo-url / lexical-search path — the
	// resolver handles visibility on every layer.
	res, err := store.Resolve(ctx, q, userID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("resolving project handle: %w", err)
	}

	// Auto-match: exact-name, exact-id-prefix, exact-alias, exact-repo-url,
	// or a single lexical-search hit at high confidence (inside Resolve).
	if res.Match != nil {
		return res.Match.ID, nil
	}

	// No match and no candidates → not found.
	if len(res.Candidates) == 0 {
		return uuid.Nil, &ProjectNotFoundError{Query: handle}
	}

	// The exact name of several accessible projects (INV-29, final) or
	// several fuzzy candidates without a confident winner → ambiguous.
	// Surface them so the agent / user can pick.
	return uuid.Nil, &AmbiguousProjectError{Query: q, Candidates: projects.AmbiguousCandidates(res.Candidates)}
}

// ProjectNotFoundError is returned when ResolveProjectHandle could not
// find a visible project for the given handle. Handlers translate this
// into ErrCodeProjectNotFound — kept as a typed error so the standard
// errors.As() pattern works.
type ProjectNotFoundError struct {
	Query string
}

func (e *ProjectNotFoundError) Error() string {
	return fmt.Sprintf("project not found: %q", e.Query)
}

// resolveVisibleIssueProject reads issues.project_id for issueID and enforces
// the caller's visibility on the owning project. It returns the project id on
// success, or a ready-to-return MCP error envelope on failure.
//
// Both failure modes (row missing / project invisible) deliberately collapse
// into the same issue_not_found envelope — a caller must not learn that an
// issue exists inside a project it cannot see. toolName only feeds the
// slog "tool" field so each call site keeps its own log identity.
//
// Shared by skein_watchers (actions watch / unwatch) and
// skein_get_monitor_script, which all need exactly this pre-flight before
// touching the watch store.
func resolveVisibleIssueProject(
	ctx context.Context,
	pool *pgxpool.Pool,
	toolName, issueID string,
	userID uuid.UUID,
) (uuid.UUID, *mcpgo.CallToolResult) {
	var projectID uuid.UUID
	err := pool.QueryRow(ctx, `SELECT project_id FROM issues WHERE id = $1`, issueID).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, skmcp.ToolError(ErrCodeIssueNotFound,
			"issue does not exist or is not visible",
			map[string]any{"id": issueID})
	}
	if err != nil {
		slog.ErrorContext(ctx, "mcp tool error",
			"tool", toolName, "reason", "lookup failed",
			"userId", userID.String(), "issueId", issueID, "error", err.Error())
		return uuid.Nil, skmcp.ToolError(ErrCodeInternalError, "failed to load issue", nil)
	}
	if _, vErr := projects.EnsureProjectVisible(ctx, pool, projectID, userID); vErr != nil {
		if errors.Is(vErr, projects.ErrProjectNotVisible) {
			return uuid.Nil, skmcp.ToolError(ErrCodeIssueNotFound,
				"issue does not exist or is not visible",
				map[string]any{"id": issueID})
		}
		slog.ErrorContext(ctx, "mcp tool error",
			"tool", toolName, "reason", "visibility failed",
			"userId", userID.String(), "issueId", issueID, "error", vErr.Error())
		return uuid.Nil, skmcp.ToolError(ErrCodeInternalError, "visibility check failed", nil)
	}
	return projectID, nil
}
