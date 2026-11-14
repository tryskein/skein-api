package tools

// actor.go — the agent actors of the tools' writes; they live in issueops,
// shared with the public API (sk-878.3.1 T024a).

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/attachments"
	"skein-api/internal/dependencies"
	"skein-api/internal/issueops"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/reminders"
	"skein-api/internal/users"
)

// IssuesAgentActor — see issueops.IssuesAgentActor.
func IssuesAgentActor(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID) issues.Actor {
	return issueops.IssuesAgentActor(ctx, pool, userID)
}

// DependenciesAgentActor — see issueops.DependenciesAgentActor.
func DependenciesAgentActor(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID) dependencies.Actor {
	return issueops.DependenciesAgentActor(ctx, pool, userID)
}

// ProjectsAgentActor — see issueops.ProjectsAgentActor.
func ProjectsAgentActor(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID) projects.Actor {
	return issueops.ProjectsAgentActor(ctx, pool, userID)
}

// UsersAgentActor — see issueops.UsersAgentActor.
func UsersAgentActor(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID) users.Actor {
	return issueops.UsersAgentActor(ctx, pool, userID)
}

// AttachmentsAgentActor — see issueops.AttachmentsAgentActor.
func AttachmentsAgentActor(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID) attachments.Actor {
	return issueops.AttachmentsAgentActor(ctx, pool, userID)
}

// RemindersAgentActor — see issueops.RemindersAgentActor.
func RemindersAgentActor(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID) reminders.Actor {
	return issueops.RemindersAgentActor(ctx, pool, userID)
}
