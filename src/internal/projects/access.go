package projects

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/authz"
)

// Sentinel errors used by EnsureProjectVisible/EnsureProjectOwner/
// EnsureProjectAction.
//
//   - ErrProjectNotVisible — handler must respond 404 (do not leak existence
//     to non-members; vision §10).
//   - ErrProjectNotOwner — caller is a member but not owner. Handler must
//     respond 403.
//   - ErrProjectActionForbidden — the project is visible but the caller's
//     level does not allow the action (the action table of US-6). Handler
//     must respond 403.
var (
	ErrProjectNotVisible      = errors.New("project not visible to caller")
	ErrProjectNotOwner        = errors.New("caller is not a project owner")
	ErrProjectActionForbidden = errors.New("caller's access does not allow the action")
)

// dbq is the minimal subset of pgxpool.Pool / pgx.Tx that the access checks
// need. Letting either type satisfy it keeps callers from having to choose
// between "do this in a transaction" and "do this against the pool".
type dbq interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// AccessLevel is the caller's resolved access level on a project — the
// level the access relation returns (authz.Level). The empty string is
// never returned: callers get a sentinel error or a populated value.
type AccessLevel = authz.Level

const (
	AccessAdmin       = authz.LevelAdmin
	AccessAdminMember = authz.LevelAdminMember
	AccessOwner       = authz.LevelOwner
	AccessMember      = authz.LevelMember
)

// EnsureProjectVisible returns the caller's AccessLevel on the project, or
// ErrProjectNotVisible when the project either doesn't exist or the caller
// can't see it. Handlers map ErrProjectNotVisible to the "project not found"
// answer — we deliberately do not distinguish the two, per vision §10
// "non-members must not learn that a project exists" (INV-3).
//
// The decision is the access relation's (authz.ProjectAccess, sk-674.18
// T008): the caller's role and external flag are read from users by id, not
// from the request — a role on the context grants nothing (D-2).
func EnsureProjectVisible(
	ctx context.Context,
	db dbq,
	projectID uuid.UUID,
	callerID uuid.UUID,
) (AccessLevel, error) {
	level, err := authz.ProjectAccess(ctx, db, authz.ForUser(callerID), projectID)
	if errors.Is(err, authz.ErrNotVisible) {
		return "", ErrProjectNotVisible
	}
	if err != nil {
		return "", fmt.Errorf("checking project access: %w", err)
	}
	return level, nil
}

// EnsureProjectOwner is the write-path gate for managing a project
// (authz.ActionManageProject, the action table of US-6): the project owner
// — an owner row or the space's implicit owner (INV-4) — and an instance
// admin (the owner's rights, sk-741) pass; a plain member and an external
// user get ErrProjectNotOwner (handler → 403); not-visible projects get
// ErrProjectNotVisible (handler → 404).
func EnsureProjectOwner(
	ctx context.Context,
	db dbq,
	projectID uuid.UUID,
	callerID uuid.UUID,
) (AccessLevel, error) {
	level, err := authz.Can(ctx, db, authz.ForUser(callerID), authz.ActionManageProject, projectID)
	switch {
	case errors.Is(err, authz.ErrNotVisible):
		return "", ErrProjectNotVisible
	case errors.Is(err, authz.ErrForbidden):
		return "", ErrProjectNotOwner
	case err != nil:
		return "", fmt.Errorf("checking project ownership: %w", err)
	}
	return level, nil
}

// EnsureProjectAction is the gate of actions on a project's issues by the
// action table of US-6: authz.ActionWrite — create and edit issues, comment,
// attach, link; authz.ActionAssign — become or make the assignee. Every
// named action must be allowed; the level is resolved once. A participant of
// the project (owner, member, external member) and an instance admin (the
// owner's rights, sk-741) pass; a level the table refuses gets
// ErrProjectActionForbidden (handler → 403, sk-674.18 T066); a project out
// of reach gets ErrProjectNotVisible (handler → 404, INV-3).
func EnsureProjectAction(
	ctx context.Context,
	db dbq,
	projectID uuid.UUID,
	callerID uuid.UUID,
	actions ...authz.Action,
) (AccessLevel, error) {
	level, err := authz.ProjectAccess(ctx, db, authz.ForUser(callerID), projectID)
	switch {
	case errors.Is(err, authz.ErrNotVisible):
		return "", ErrProjectNotVisible
	case err != nil:
		return "", fmt.Errorf("checking project access: %w", err)
	}
	if err := RequireAction(level, actions...); err != nil {
		return "", err
	}
	return level, nil
}

// RequireAction answers the action table of US-6 for a level already
// resolved by EnsureProjectAction — for an action the request reveals only
// after the gate (an assignee in the body): ErrProjectActionForbidden unless
// every named action is allowed.
func RequireAction(level AccessLevel, actions ...authz.Action) error {
	for _, action := range actions {
		if !authz.Allows(level, action) {
			return ErrProjectActionForbidden
		}
	}
	return nil
}

// ResolveProjectAccess answers the service check of POST
// /api/v1/hub/project-access for every entry of req, with exactly one
// database statement however many users the batch holds (none for an empty
// batch) — authz.ResolveBatch, the access relation per user gated by
// activity (sk-674.18 T008). See access_contract.go for the wire contract,
// version 2: one result per entry in request order; Allowed and Denied
// partition the entry's distinct requested projects in request order;
// VisibleProjectIDs is set (possibly empty) for every entry that asked
// AllVisible, the internal admin included; InternalAdmin is the role only.
func ResolveProjectAccess(ctx context.Context, db authz.RowsQuerier, req ProjectAccessRequest) (ProjectAccessResponse, error) {
	out := ProjectAccessResponse{Users: make([]ProjectAccessResult, len(req.Users))}
	if len(req.Users) == 0 {
		return out, nil
	}
	entries := make([]authz.BatchEntry, len(req.Users))
	requested := make([][]uuid.UUID, len(req.Users)) // distinct, request order
	for i, q := range req.Users {
		entries[i] = authz.BatchEntry{UserID: q.UserID, ProjectIDs: q.ProjectIDs, AllVisible: q.AllVisible}
		seen := make(map[uuid.UUID]struct{}, len(q.ProjectIDs))
		for _, pid := range q.ProjectIDs {
			if _, dup := seen[pid]; dup {
				continue
			}
			seen[pid] = struct{}{}
			requested[i] = append(requested[i], pid)
		}
	}
	results, err := authz.ResolveBatch(ctx, db, entries)
	if err != nil {
		return ProjectAccessResponse{}, err
	}
	for i, r := range results {
		allowedSet := make(map[uuid.UUID]struct{}, len(r.Allowed))
		for _, pid := range r.Allowed {
			allowedSet[pid] = struct{}{}
		}
		res := ProjectAccessResult{
			UserID:            req.Users[i].UserID,
			Active:            r.Active,
			InternalAdmin:     r.InternalAdmin,
			External:          r.External,
			Allowed:           make([]uuid.UUID, 0, len(r.Allowed)),
			Denied:            make([]uuid.UUID, 0, len(requested[i])-len(r.Allowed)),
			VisibleProjectIDs: r.Visible,
		}
		for _, pid := range requested[i] {
			if _, ok := allowedSet[pid]; ok {
				res.Allowed = append(res.Allowed, pid)
			} else {
				res.Denied = append(res.Denied, pid)
			}
		}
		out.Users[i] = res
	}
	return out, nil
}
