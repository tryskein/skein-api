package testsupport

// tenancy.go — the two-tenant isolation fixture (sk-674.18 T003, matrix
// maintenance row O-f1).
//
// The isolation matrix (matrix rows 22–34) asks every actor of one legend
// to hit every surface and compares the answer with a literal expectation.
// The legend is fixed by the matrix header:
//
//	A/B   — the two tenants (spaces once T016 lands)
//	uA    — only-A          uB    — only-B          uAB  — both
//	vP    — project owner   vPr   — space owner
//	uch¬p  — space participant without access to the `members` project
//	vnsh   — external user, member of one A project
//	admA  — admin with membership in A              adm0 — admin without any
//	deakt — deactivated     post  — outsider (only its own Personal)
//	L(x)  — the Personal of x
//
// # Spaces (since sk-674.18 T017)
//
// The fixture seeds real spaces (it started, in T003, as membership rows
// standing in for them):
//
//   - space A (regular, owner vPr): participants uA, uAB, vP, vPr, uch¬p, admA,
//     deakt; space B (regular, owner uB): participants uB, uAB;
//   - A.space / B.space — visibility `space`: every participant of the
//     space has a membership row from the space (INV-7); the owner of the
//     space holds the owner row;
//   - A.members / B.members — visibility `members`: explicit rows only
//     (vP owns A.members; uB owns B.members). vPr has no row on A.members:
//     he is its implicit owner (INV-4). vnsh holds a row on A.members and is
//     in no space (E-16);
//   - L(x) — a project in the Personal of x (a personal space, INV-8),
//     x its owner; every non-external actor gets one, deactivated
//     included; vnsh gets none;
//   - deakt keeps the rows of an A participant, so a "not visible" answer
//     for it can only come from the deactivation, not from a missing row.
//
// # Isolation and cleanup
//
// Names, prefixes, aliases and usernames all carry a random suffix (via
// SeedUser / SeedProject), so two runs — sequential or from two packages
// under -p 1 — never collide. Everything is torn down by t.Cleanup:
// projects (issues leaf-first, aliases and memberships cascade) before
// users, because t.Cleanup runs last-registered-first and users are
// registered first.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Actor is one row of the matrix legend. The values are ASCII handles used
// in subtest names; the legend symbol is in each constant's comment.
type Actor string

const (
	// ActorOnlyA — uA: participant of A only, sees both A projects.
	ActorOnlyA Actor = "onlyA"
	// ActorOnlyB — uB: participant of B only; owns both B projects.
	ActorOnlyB Actor = "onlyB"
	// ActorBoth — uAB: participant of A and B, sees all four projects.
	ActorBoth Actor = "both"
	// ActorProjectOwner — vP: owner of A.members, participant of A.
	ActorProjectOwner Actor = "projectOwner"
	// ActorSpaceOwner — vPr: owner of space A, the implicit owner of both A
	// projects (no row on A.members).
	ActorSpaceOwner Actor = "spaceOwner"
	// ActorParticipantNoMembers — uch¬p: participant of A without a row on
	// A.members.
	ActorParticipantNoMembers Actor = "participantNoMembers"
	// ActorExternal — vnsh: external user, member of A.members only.
	ActorExternal Actor = "external"
	// ActorAdminA — admA: instance admin with A memberships.
	ActorAdminA Actor = "adminA"
	// ActorAdmin0 — adm0: instance admin with no membership at all.
	ActorAdmin0 Actor = "admin0"
	// ActorDeactivated — deakt: deactivated user that keeps A rows.
	ActorDeactivated Actor = "deactivated"
	// ActorOutsider — post: no membership beyond its own Personal.
	ActorOutsider Actor = "outsider"
)

// TenancyActors lists every actor in legend order. Matrices iterate it so a
// new actor cannot be silently skipped by one of them.
var TenancyActors = []Actor{
	ActorOnlyA, ActorOnlyB, ActorBoth, ActorProjectOwner, ActorSpaceOwner,
	ActorParticipantNoMembers, ActorExternal, ActorAdminA, ActorAdmin0,
	ActorDeactivated, ActorOutsider,
}

// ProjectKind tells a tenant project's role in the fixture.
type ProjectKind string

const (
	// KindSpace — the tenant's project with visibility `space`: every
	// participant of the space has a row from the space.
	KindSpace ProjectKind = "space"
	// KindMembers — the tenant's project with visibility `members`:
	// explicit rows only.
	KindMembers ProjectKind = "members"
	// KindPersonal — L(x): a project in the Personal of x.
	KindPersonal ProjectKind = "personal"
)

// TenancyUser is a seeded actor.
type TenancyUser struct {
	Actor    Actor
	ID       uuid.UUID
	Username string
	// Role is users.role: "admin" or "user".
	Role string
	// External mirrors users.is_external.
	External bool
	// Deactivated mirrors users.deactivated_at IS NOT NULL.
	Deactivated bool
}

// TenancyProject is a seeded project with one marker issue in it.
type TenancyProject struct {
	Kind ProjectKind
	// Tenant is "A" or "B" for tenant projects, "" for a Personal.
	Tenant string
	// PersonalOf is the owner actor of a Personal, "" otherwise.
	PersonalOf Actor

	ID       uuid.UUID
	Name     string
	IDPrefix string
	// Alias is one project_aliases row, so resolve-by-alias has a target.
	Alias string
	// OwnerID is the project's creator and owner member.
	OwnerID uuid.UUID

	// IssueID is the project's marker issue ("<prefix>-1"), open, created
	// by the owner. Its title carries the project name, so a full-text
	// search for the name token finds exactly this issue.
	IssueID    string
	IssueTitle string
}

// Tenancy is the handle a matrix works against.
type Tenancy struct {
	Users map[Actor]TenancyUser

	// SpaceA and SpaceB are the two tenants' spaces; PersonalSpace holds the
	// Personal of every non-external actor.
	SpaceA, SpaceB uuid.UUID
	PersonalSpace  map[Actor]uuid.UUID

	ASpace   TenancyProject
	AMembers TenancyProject
	BSpace   TenancyProject
	BMembers TenancyProject

	// Personal holds L(x) for every non-external actor.
	Personal map[Actor]TenancyProject
}

// User returns the seeded actor or fails the test.
func (f *Tenancy) User(t *testing.T, a Actor) TenancyUser {
	t.Helper()
	u, ok := f.Users[a]
	if !ok {
		t.Fatalf("tenancy: unknown actor %q", a)
	}
	return u
}

// Projects returns every seeded project: the four tenant projects in the
// order A.space, A.members, B.space, B.members, then the Personals in
// TenancyActors order.
func (f *Tenancy) Projects() []TenancyProject {
	out := []TenancyProject{f.ASpace, f.AMembers, f.BSpace, f.BMembers}
	for _, a := range TenancyActors {
		if p, ok := f.Personal[a]; ok {
			out = append(out, p)
		}
	}
	return out
}

// tenancyMembership is one project_members row of the fixture, keyed by
// actor and the project's position in the fixture: a role and its source
// (from the space, or manual).
type tenancyMembership struct {
	actor     Actor
	role      string
	fromSpace bool
}

// SeedTenancy materialises the two-tenant fixture and returns its handle.
// The pool comes from NewTestPool, which skips the test when
// TEST_DATABASE_URL is unset.
func SeedTenancy(t *testing.T, pool *pgxpool.Pool) *Tenancy {
	t.Helper()
	ctx := context.Background()

	f := &Tenancy{
		Users:    make(map[Actor]TenancyUser, len(TenancyActors)),
		Personal: make(map[Actor]TenancyProject, len(TenancyActors)),
	}

	// Users first: their cleanups then run after the projects' ones.
	for _, a := range TenancyActors {
		role := "user"
		if a == ActorAdminA || a == ActorAdmin0 {
			role = "admin"
		}
		id, uname := SeedUser(t, pool, role)
		u := TenancyUser{Actor: a, ID: id, Username: uname, Role: role}
		switch a {
		case ActorExternal:
			if _, err := pool.Exec(ctx, `UPDATE users SET is_external = true WHERE id = $1`, id); err != nil {
				t.Fatalf("tenancy: mark %s external: %v", a, err)
			}
			u.External = true
		case ActorDeactivated:
			if _, err := pool.Exec(ctx, `UPDATE users SET deactivated_at = now() WHERE id = $1`, id); err != nil {
				t.Fatalf("tenancy: deactivate %s: %v", a, err)
			}
			u.Deactivated = true
		}
		f.Users[a] = u
	}

	// Spaces before projects: their cleanups then run after the projects'.
	ids := func(as ...Actor) []uuid.UUID {
		out := make([]uuid.UUID, len(as))
		for i, a := range as {
			out[i] = f.Users[a].ID
		}
		return out
	}
	f.SpaceA = SeedSpace(t, pool, f.Users[ActorSpaceOwner].ID)
	for _, id := range ids(ActorOnlyA, ActorBoth, ActorProjectOwner, ActorParticipantNoMembers, ActorAdminA, ActorDeactivated) {
		AddSpaceMember(t, pool, f.SpaceA, id)
	}
	f.SpaceB = SeedSpace(t, pool, f.Users[ActorOnlyB].ID)
	AddSpaceMember(t, pool, f.SpaceB, f.Users[ActorBoth].ID)
	f.PersonalSpace = make(map[Actor]uuid.UUID, len(TenancyActors))
	for _, a := range TenancyActors {
		if !f.Users[a].External {
			f.PersonalSpace[a] = SeedSpaceOfKind(t, pool, f.Users[a].ID, "personal")
		}
	}

	fromSpace := func(as ...Actor) []tenancyMembership {
		out := make([]tenancyMembership, len(as))
		for i, a := range as {
			out[i] = tenancyMembership{actor: a, role: "member", fromSpace: true}
		}
		return out
	}
	manual := func(as ...Actor) []tenancyMembership {
		out := make([]tenancyMembership, len(as))
		for i, a := range as {
			out[i] = tenancyMembership{actor: a, role: "member"}
		}
		return out
	}

	f.ASpace = seedTenancyProject(t, pool, f, f.SpaceA, KindSpace, "A", "", ActorSpaceOwner,
		fromSpace(ActorOnlyA, ActorBoth, ActorProjectOwner, ActorParticipantNoMembers, ActorAdminA, ActorDeactivated))
	f.AMembers = seedTenancyProject(t, pool, f, f.SpaceA, KindMembers, "A", "", ActorProjectOwner,
		manual(ActorOnlyA, ActorBoth, ActorExternal, ActorAdminA, ActorDeactivated))
	f.BSpace = seedTenancyProject(t, pool, f, f.SpaceB, KindSpace, "B", "", ActorOnlyB, fromSpace(ActorBoth))
	f.BMembers = seedTenancyProject(t, pool, f, f.SpaceB, KindMembers, "B", "", ActorOnlyB, manual(ActorBoth))

	for _, a := range TenancyActors {
		if f.Users[a].External {
			continue
		}
		f.Personal[a] = seedTenancyProject(t, pool, f, f.PersonalSpace[a], KindPersonal, "", a, a, nil)
	}
	return f
}

// seedTenancyProject creates one project of space owned by owner, adds the
// extra member rows, one alias and the marker issue.
func seedTenancyProject(t *testing.T, pool *pgxpool.Pool, f *Tenancy, spaceID uuid.UUID, kind ProjectKind, tenant string, personalOf, owner Actor, members []tenancyMembership) TenancyProject {
	t.Helper()
	ctx := context.Background()

	namePrefix := "tn" + strings.ToLower(tenant) + string(kind)[:2]
	if kind == KindPersonal {
		namePrefix = "tnlp"
	}
	ownerID := f.Users[owner].ID
	id, prefix := SeedProjectInSpace(t, pool, ownerID, spaceID, namePrefix)
	if kind == KindSpace {
		if _, err := pool.Exec(ctx, `UPDATE projects SET visibility = 'space' WHERE id = $1`, id); err != nil {
			t.Fatalf("tenancy: visibility of %s: %v", id, err)
		}
	}

	var name string
	if err := pool.QueryRow(ctx, `SELECT name FROM projects WHERE id = $1`, id).Scan(&name); err != nil {
		t.Fatalf("tenancy: read project name: %v", err)
	}

	for _, m := range members {
		if _, err := pool.Exec(ctx, `
INSERT INTO project_members (project_id, user_id, role, added_at, added_by, via_manual, via_space)
VALUES ($1, $2, $3, now(), $4, $5, $6)`,
			id, f.Users[m.actor].ID, m.role, ownerID, !m.fromSpace, m.fromSpace); err != nil {
			t.Fatalf("tenancy: member %s of %s: %v", m.actor, id, err)
		}
	}

	alias := "al" + prefix
	if _, err := pool.Exec(ctx, `
INSERT INTO project_aliases (alias, project_id, added_by, added_by_kind)
VALUES ($1, $2, $3, 'human')`, alias, id, ownerID); err != nil {
		t.Fatalf("tenancy: seed alias %s: %v", alias, err)
	}

	issueID := prefix + "-1"
	title := fmt.Sprintf("tenancy marker %s", name)
	SeedIssue(t, pool, id, ownerID, issueID, title, "open", 2)

	return TenancyProject{
		Kind:       kind,
		Tenant:     tenant,
		PersonalOf: personalOf,
		ID:         id,
		Name:       name,
		IDPrefix:   prefix,
		Alias:      alias,
		OwnerID:    ownerID,
		IssueID:    issueID,
		IssueTitle: title,
	}
}

// Key names the project in the matrix legend: "A.space", "A.members",
// "B.space", "B.members", or "L:<actor>" for a Personal.
func (p TenancyProject) Key() string {
	if p.Kind == KindPersonal {
		return "L:" + string(p.PersonalOf)
	}
	return p.Tenant + "." + string(p.Kind)
}

// TenancyVisible is the reference of matrix row 22: for every actor, the keys
// of the fixture projects it sees by the matrix legend. It is a
// literal written from the legend, not derived from the code, and it is the
// TARGET — where the current code is wider (an admin sees the Personals of
// others until T017, a deactivated user passes the REST channel until T036),
// the matrix marks the cell as waiting for narrowing instead of changing this
// table.
//
//	uA, vP, vPr      — A.space, A.members, own L
//	uB               — B.space, B.members, own L
//	uAB              — all four tenant projects, own L
//	uch¬p             — A.space, own L (not A.members)
//	vnsh              — A.members only (no L: INV-8)
//	admA, adm0 (f+)  — all non-personal, own L; L of others — not
//	deakt            — nothing
//	post             — own L only
var TenancyVisible = map[Actor][]string{
	ActorOnlyA:                {"A.space", "A.members", "L:onlyA"},
	ActorOnlyB:                {"B.space", "B.members", "L:onlyB"},
	ActorBoth:                 {"A.space", "A.members", "B.space", "B.members", "L:both"},
	ActorProjectOwner:         {"A.space", "A.members", "L:projectOwner"},
	ActorSpaceOwner:           {"A.space", "A.members", "L:spaceOwner"},
	ActorParticipantNoMembers: {"A.space", "L:participantNoMembers"},
	ActorExternal:             {"A.members"},
	ActorAdminA:               {"A.space", "A.members", "B.space", "B.members", "L:adminA"},
	ActorAdmin0:               {"A.space", "A.members", "B.space", "B.members", "L:admin0"},
	ActorDeactivated:          {},
	ActorOutsider:             {"L:outsider"},
}

// Sees reports whether actor a sees project p by TenancyVisible.
func (f *Tenancy) Sees(a Actor, p TenancyProject) bool {
	for _, k := range TenancyVisible[a] {
		if k == p.Key() {
			return true
		}
	}
	return false
}

// IsAdmin reports whether the actor is one of the two admins of the legend.
func (a Actor) IsAdmin() bool { return a == ActorAdminA || a == ActorAdmin0 }
