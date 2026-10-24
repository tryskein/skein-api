package spaces

// project.go — the space of a new project (sk-674.18 T024, US-12, US-6,
// contract.go «Choosing the space of a new project»). One rule for REST
// POST /projects and MCP skein_project_create.

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CodeExternalCannotCreateProject is the refusal of creating a project for
// an external user, as REST and MCP give it today (contract.go: «only an
// external user, who has no space and cannot create projects»).
const CodeExternalCannotCreateProject = "external_cannot_create_project"

// ChosenSpace is the space a new project goes to.
type ChosenSpace struct {
	ID      uuid.UUID
	ShortID string
	Kind    string
}

// ChooseForProject picks the space of a project the caller creates. The
// eligible spaces are those GET /me/spaces lists with canCreateProject —
// the caller's own participations, in column order:
//
//   - short given: a space the caller does not see (or no such space) →
//     404 space_not_found; one he sees only as the admin → 422
//     not_space_member with spaceId (US-6: «add yourself as a member»);
//   - short omitted: exactly one eligible space → it; several → 422
//     space_required with `spaces` (every eligible space, column order);
//   - an external user, who has no space → 422
//     external_cannot_create_project.
//
// q is a pool or a transaction.
func ChooseForProject(ctx context.Context, q querier, caller uuid.UUID, short string) (ChosenSpace, error) {
	var external bool
	err := q.QueryRow(ctx, `SELECT is_external FROM users WHERE id = $1`, caller).Scan(&external)
	if errors.Is(err, pgx.ErrNoRows) {
		return ChosenSpace{}, refusedUserNotFound
	}
	if err != nil {
		return ChosenSpace{}, fmt.Errorf("reading the caller: %w", err)
	}
	if external {
		return ChosenSpace{}, refuse(http.StatusUnprocessableEntity, CodeExternalCannotCreateProject,
			"external users cannot create projects")
	}
	rows, err := seenSpaces(ctx, q, caller, "")
	if err != nil {
		return ChosenSpace{}, err
	}
	if short != "" {
		for _, r := range rows {
			if r.ShortID != short {
				continue
			}
			if r.Role == nil {
				ref := refuse(http.StatusUnprocessableEntity, CodeNotSpaceMember,
					"add yourself as a member of the space to create projects there")
				ref.Extra = map[string]any{"spaceId": r.ShortID}
				return ChosenSpace{}, ref
			}
			return ChosenSpace{ID: r.ID, ShortID: r.ShortID, Kind: r.Kind}, nil
		}
		return ChosenSpace{}, refusedNotFound
	}
	var eligible []spaceRow
	for _, r := range rows {
		if r.Role != nil {
			eligible = append(eligible, r)
		}
	}
	switch len(eligible) {
	case 0:
		return ChosenSpace{}, refuse(http.StatusUnprocessableEntity, CodeExternalCannotCreateProject,
			"you are not a member of any space where you can create a project")
	case 1:
		r := eligible[0]
		return ChosenSpace{ID: r.ID, ShortID: r.ShortID, Kind: r.Kind}, nil
	}
	refs := make([]SpaceRef, 0, len(eligible))
	for _, r := range eligible {
		refs = append(refs, SpaceRef{ID: r.ShortID, Slug: Slug(r.Name), Name: r.Name})
	}
	ref := refuse(http.StatusUnprocessableEntity, CodeSpaceRequired,
		"you can create projects in several spaces; specify the space")
	ref.Extra = map[string]any{"spaces": refs}
	return ChosenSpace{}, ref
}

// ProjectVisibility is the visibility of a new project in a space of kind:
// requested when given, else the installation default
// (SKEIN_PROJECT_DEFAULT_VISIBILITY); a "Personal" is always members (INV-8 —
// nobody but its owner is ever in it). An unknown requested value is the
// caller's validation error, not decided here.
func ProjectVisibility(kind, requested, def string) string {
	if kind == KindPersonal {
		return VisibilityMembers
	}
	if requested != "" {
		return requested
	}
	return def
}

// AgentSpace is a space as an agent sees it (skein_whoami, sk-674.18 T024,
// US-12): just enough to name it in `space` and to know whether a project
// may be created there.
type AgentSpace struct {
	// ID is the short id — the value of every `space` parameter.
	ID   string `json:"id"`
	Name string `json:"name"`
	// Kind is KindPersonal | KindCommon | KindRegular.
	Kind string `json:"kind"`
	// Role is RoleOwner | RoleMember, or "admin" for a space the caller
	// sees only as the instance admin.
	Role string `json:"role"`
	// CanCreateProject — the caller is a member (never with admin access).
	CanCreateProject bool `json:"canCreateProject"`
}

// AgentSpaces returns the spaces the caller sees in column order (the
// same set and order as GET /me/spaces).
func AgentSpaces(ctx context.Context, q querier, caller uuid.UUID) ([]AgentSpace, error) {
	rows, err := seenSpaces(ctx, q, caller, "")
	if err != nil {
		return nil, err
	}
	out := make([]AgentSpace, 0, len(rows))
	for _, r := range rows {
		s := AgentSpace{ID: r.ShortID, Name: r.Name, Kind: r.Kind, Role: "admin"}
		if r.Role != nil {
			s.Role, s.CanCreateProject = *r.Role, true
		}
		out = append(out, s)
	}
	return out, nil
}
