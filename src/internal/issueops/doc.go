// Package issueops holds the operations shared by the MCP tools and the
// public REST API — the writes of issues, their comments, dependencies and
// attachments, the caller's own state — independent of the transport that
// asks for them (sk-878.3.1 T024a, INV-4): the MCP tools
// and the public REST API are thin adapters over the same operation, so
// one operation gives the same checks, events, notifications and refusal
// codes on both.
//
// An operation takes its arguments by the names the MCP tool uses (Args),
// the origin of the write as a parameter, and answers with a result or a
// Failure — a refusal code, its message and its members. How the result is
// shaped into a response is the adapter's business.
package issueops

import (
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/notifications"
	"skein-api/internal/projects"
	"skein-api/internal/reminders"
	"skein-api/internal/spaces"
	"skein-api/internal/users"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Deps are the stores the operations write through.
type Deps struct {
	Pool     *pgxpool.Pool
	Issues   *issues.Store
	Deps     *dependencies.Store
	Projects *projects.Store
	// Notifications and Reminders are the stores of the operations of the
	// caller's own state (T027).
	Notifications *notifications.Store
	Reminders     *reminders.Store
	// Users and Spaces are the stores of the user operations (T028).
	Users  *users.Store
	Spaces *spaces.Service
}

// Origin says how an operation reads the origin of a write from its kind
// argument.
type Origin uint8

// The origins.
const (
	// OriginMCP — the MCP tools: an agent (an API key) must name kind
	// directed or autonomous; a session caller writes as a human.
	OriginMCP Origin = iota
	// OriginKey — the public API: no kind is the origin api; directed and
	// autonomous are the agent's (issues.SourceForKeyWrite).
	OriginKey
)
