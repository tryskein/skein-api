package tools

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/attachments"
	"skein-api/internal/auth"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/reminders"
	"skein-api/internal/users"
)

// agentActorBase pulls the audit-trail context fields out of ctx the same
// way every agent-driven mutation needs them. Centralising the lookup
// here means new write tools don't repeat the same `auth.*From(ctx)`
// calls — they just get one of the typed Actor wrappers below.
//
// userID is required (every tool runs RequireUser first; we still take it
// as a parameter so the constructor can't be called with uuid.Nil by
// accident). username is optional — the MCP api-key middleware does not
// plumb it into ctx, so we look it up against the users table on first
// call. SessionID falls through from the Mcp-Session-Id header.
//
// The username lookup is best-effort: events.actor is NOT NULL, so on
// miss we fall back to "agent:<keyShort>" and finally to the literal
// "agent" — events.actor stays a stable human-readable token without
// ever being empty.
type agentActorBase struct {
	UserID    uuid.UUID
	Username  string
	SessionID string
}

// resolveAgentActor reads the auth ctx and looks up the username when
// missing. The pool argument is used only for the username lookup —
// callers that already have a username in ctx (future BFF plumbing)
// skip the round-trip.
func resolveAgentActor(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID) agentActorBase {
	base := agentActorBase{
		UserID:    userID,
		SessionID: auth.SessionIDFrom(ctx),
	}
	if u, ok := auth.UsernameFrom(ctx); ok && u != "" {
		base.Username = u
		return base
	}
	// Fallback chain: users.username via DB → "agent:<keyShort>" → literal
	// "agent". events.actor must not be empty, so the chain always
	// produces something.
	var uname string
	err := pool.QueryRow(ctx, `SELECT username FROM users WHERE id = $1`, userID).Scan(&uname)
	switch {
	case err == nil && uname != "":
		base.Username = uname
	case errors.Is(err, pgx.ErrNoRows):
		base.Username = agentFallbackLabel(ctx)
	case err != nil:
		// Soft-fail: log and use the fallback. We don't want the agent
		// path to 500 because we couldn't pretty-print an audit row.
		slog.WarnContext(ctx, "agent actor: username lookup failed",
			"userId", userID.String(), "error", err.Error())
		base.Username = agentFallbackLabel(ctx)
	default:
		// err == nil && uname == "". The users.username column is NOT NULL
		// and constrained by [a-z0-9_-]{3,30}, so an empty username on a
		// found row is a data-integrity regression. Don't break the call,
		// but emit a warn so it's observable in Loki.
		slog.WarnContext(ctx, "agent actor: username column empty for user",
			"userId", userID.String())
		base.Username = agentFallbackLabel(ctx)
	}
	return base
}

// agentFallbackLabel returns the best human-readable token when
// users.username isn't available. Order: "agent:<keyShort>" → "agent".
// Never empty — events.actor is NOT NULL.
func agentFallbackLabel(ctx context.Context) string {
	if id, ok := auth.APIKeyIDFrom(ctx); ok && id != uuid.Nil {
		return "agent:" + id.String()[:8]
	}
	return "agent"
}

// IssuesAgentActor returns an issues.Actor populated for an MCP-driven
// (agent) write. role='agent' so events.actor_type = 'agent' downstream.
// Every write tool that calls into issues.Store goes through this — never
// construct an issues.Actor inline.
func IssuesAgentActor(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID) issues.Actor {
	b := resolveAgentActor(ctx, pool, userID)
	return issues.Actor{
		UserID:    b.UserID,
		Username:  b.Username,
		Role:      "agent",
		SessionID: b.SessionID,
	}
}

// DependenciesAgentActor mirrors IssuesAgentActor for the dependencies
// package. Used by skein_create when it auto-adds a `duplicates` edge
// (and by future skein_dep_add).
func DependenciesAgentActor(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID) dependencies.Actor {
	b := resolveAgentActor(ctx, pool, userID)
	return dependencies.Actor{
		UserID:    b.UserID,
		Username:  b.Username,
		Role:      "agent",
		SessionID: b.SessionID,
	}
}

// ProjectsAgentActor mirrors the above for projects.Actor. Reserved for
// R2.2c (skein_project_create / skein_component); kept here so the
// closed set of constructors lives in one file.
func ProjectsAgentActor(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID) projects.Actor {
	b := resolveAgentActor(ctx, pool, userID)
	return projects.Actor{
		UserID:    b.UserID,
		Username:  b.Username,
		Role:      "agent",
		SessionID: b.SessionID,
	}
}

// UsersAgentActor mirrors the above for users.Actor. Used by the R3.1
// admin-only MCP tool skein_user (create / list / update). The
// username lookup falls back through the same
// chain (username → key prefix → "agent") so events.actor is never empty.
func UsersAgentActor(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID) users.Actor {
	b := resolveAgentActor(ctx, pool, userID)
	return users.Actor{
		UserID:    b.UserID,
		Username:  b.Username,
		Role:      "agent",
		SessionID: b.SessionID,
	}
}

// AttachmentsAgentActor mirrors the above for attachments.Actor. Used
// by the R4.2 MCP tool skein_attachments.
func AttachmentsAgentActor(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID) attachments.Actor {
	b := resolveAgentActor(ctx, pool, userID)
	return attachments.Actor{
		UserID:    b.UserID,
		Username:  b.Username,
		Role:      "agent",
		SessionID: b.SessionID,
	}
}

// RemindersAgentActor mirrors the above for reminders.Actor. Used by the
// sk-62.2 MCP tool skein_remind (set / snooze / dismiss). role='agent' so
// reminder.* events land with actor_type='agent'.
func RemindersAgentActor(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID) reminders.Actor {
	b := resolveAgentActor(ctx, pool, userID)
	return reminders.Actor{
		UserID:    b.UserID,
		Username:  b.Username,
		Role:      "agent",
		SessionID: b.SessionID,
	}
}
