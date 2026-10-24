package authz

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Level is the caller's access level on one project, as the relation
// returns it (migration 000073, INV-1), or LevelInternal for declared
// internal work.
type Level string

const (
	// LevelOwner — the project's owner: an owner membership row, the
	// implicit owner of the project's space (INV-4), or the owner of the
	// "Personal" the project is in.
	LevelOwner Level = "owner"
	// LevelMember — a member row: a participant of the project's space with
	// a membership row, or an external collaborator (only ever member).
	LevelMember Level = "member"
	// LevelAdminMember — a member row held by an instance admin: the
	// owner's rights, whatever the row says (US-6 as changed by the owner
	// 2026-10-17, sk-741).
	LevelAdminMember Level = "admin_member"
	// LevelAdmin — an instance admin's reach alone (INV-1 d): in a
	// non-personal project he has the owner's rights without a membership
	// row (US-6 as changed by the owner 2026-10-17, sk-741). Another user's
	// "Personal" stays out of his reach — the relation never returns it.
	LevelAdmin Level = "admin"
	// LevelInternal — declared internal work (WithUnrestricted: workers,
	// migrations, fixtures). Never a user's level; every action is open.
	LevelInternal Level = "internal"
)

// Action is something a caller may want to do in a project.
type Action string

const (
	// ActionView — read the project and its issues.
	ActionView Action = "view"
	// ActionWrite — create and edit issues, comments, attachments.
	ActionWrite Action = "write"
	// ActionAssign — set an issue's assignee.
	ActionAssign Action = "assign"
	// ActionManageMembers — add, remove and re-role project members.
	ActionManageMembers Action = "manage_members"
	// ActionChangeVisibility — switch the project between space and members.
	ActionChangeVisibility Action = "change_visibility"
	// ActionManageProject — rename, archive, configure the project.
	ActionManageProject Action = "manage_project"
	// ActionForceUnlock — release another holder's claim, close past the
	// lock and pin guards.
	ActionForceUnlock Action = "force_unlock"
	// ActionDeleteAnyComment — delete another author's comment (an author
	// always deletes his own).
	ActionDeleteAnyComment Action = "delete_any_comment"
	// ActionCreateProject — create a project. It concerns no existing
	// project, so it is answered by CanCreateProject, not by Can.
	ActionCreateProject Action = "create_project"
)

// Sentinel errors of ProjectAccess and Can.
//
//   - ErrNotVisible — the project does not exist or is out of the caller's
//     reach; a handler answers exactly as for a missing project (INV-3).
//   - ErrForbidden — the project is visible but the level does not allow
//     the action; a handler answers 403.
var (
	ErrNotVisible = errors.New("authz: project not visible")
	ErrForbidden  = errors.New("authz: action not allowed")
)

// allowed is the action table of US-6 — one table for REST and MCP.
// An instance admin has the owner's rights in every project he sees, with
// or without a membership row (the maintainers' decision 2026-10-17, sk-741);
// a member writes and assigns. The rules of spaces live in the relation
// and here, not in the callers.
var allowed = map[Action]map[Level]bool{
	ActionView:             {LevelInternal: true, LevelOwner: true, LevelMember: true, LevelAdminMember: true, LevelAdmin: true},
	ActionWrite:            {LevelInternal: true, LevelOwner: true, LevelMember: true, LevelAdminMember: true, LevelAdmin: true},
	ActionAssign:           {LevelInternal: true, LevelOwner: true, LevelMember: true, LevelAdminMember: true, LevelAdmin: true},
	ActionManageMembers:    {LevelInternal: true, LevelOwner: true, LevelAdminMember: true, LevelAdmin: true},
	ActionChangeVisibility: {LevelInternal: true, LevelOwner: true, LevelAdminMember: true, LevelAdmin: true},
	ActionManageProject:    {LevelInternal: true, LevelOwner: true, LevelAdminMember: true, LevelAdmin: true},
	ActionForceUnlock:      {LevelInternal: true, LevelOwner: true, LevelAdminMember: true, LevelAdmin: true},
	ActionDeleteAnyComment: {LevelInternal: true, LevelOwner: true, LevelAdminMember: true, LevelAdmin: true},
}

// Allows reports whether level permits action on a project. An unknown
// action or level permits nothing.
func Allows(level Level, action Action) bool {
	return allowed[action][level]
}

// Querier is the subset of *pgxpool.Pool / pgx.Tx / *pgx.Conn the checks
// need, so a check can run inside the caller's transaction.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// accessSQL is the point form of the relation, plus the unrestricted leg of
// declared internal work (which still distinguishes a missing project).
const accessSQL = `
SELECT CASE
    WHEN $1::boolean THEN
        CASE WHEN EXISTS (SELECT 1 FROM projects WHERE id = $3) THEN 'internal' ELSE 'not_found' END
    ELSE skein_project_access($2::uuid, $3::uuid)
END`

// ProjectAccess returns the scope's level on project, or ErrNotVisible when
// the project is missing or out of reach — the two are deliberately one
// error (INV-3).
func ProjectAccess(ctx context.Context, q Querier, s Scope, projectID uuid.UUID) (Level, error) {
	var result string
	if err := q.QueryRow(ctx, accessSQL, s.unrestricted, s.callerID, projectID).Scan(&result); err != nil {
		return "", fmt.Errorf("resolving access to project %s: %w", projectID, err)
	}
	switch result {
	case string(LevelInternal), string(LevelOwner), string(LevelMember),
		string(LevelAdminMember), string(LevelAdmin):
		return Level(result), nil
	case "not_found", "forbidden":
		return "", ErrNotVisible
	default:
		return "", fmt.Errorf("unexpected access result %q for project %s", result, projectID)
	}
}

// Can checks that the scope may perform action in project and returns its
// level: ErrNotVisible when the project is out of reach, ErrForbidden when
// it is visible but the level does not allow the action.
func Can(ctx context.Context, q Querier, s Scope, action Action, projectID uuid.UUID) (Level, error) {
	level, err := ProjectAccess(ctx, q, s, projectID)
	if err != nil {
		return "", err
	}
	if !Allows(level, action) {
		return level, ErrForbidden
	}
	return level, nil
}

// canCreateProjectSQL — today an internal user may create projects and an
// external collaborator may not (handler/projects.go, MCP project_create).
const canCreateProjectSQL = `
SELECT $1::boolean OR EXISTS (SELECT 1 FROM users u WHERE u.id = $2::uuid AND NOT u.is_external)`

// CanCreateProject answers ActionCreateProject for the scope: ErrForbidden
// when the caller may not create projects.
func CanCreateProject(ctx context.Context, q Querier, s Scope) error {
	var ok bool
	if err := q.QueryRow(ctx, canCreateProjectSQL, s.unrestricted, s.callerID).Scan(&ok); err != nil {
		return fmt.Errorf("resolving project creation right: %w", err)
	}
	if !ok {
		return ErrForbidden
	}
	return nil
}
