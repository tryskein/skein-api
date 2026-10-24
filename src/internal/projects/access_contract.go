package projects

import "github.com/google/uuid"

// This file is the wire contract of the service endpoint
//
//	POST /api/v1/hub/project-access
//
// (spec sk-622.21, US-9, INV-9). skein-web calls it to decide which project
// event groups a hub connection may join: when a tab declares its set of
// projects, when a tab switches to "all projects visible to me" (/activity),
// and in the periodic 30-second re-check of every connection of a replica
// (decision K10). The endpoint answers for MANY users in one call and is
// independent of any user HTTP context — the role and the deactivation state
// are read from the database on every call, never from a session.
//
// Only the types and their documentation live here; the handler and the
// single-query resolver are T006. Treat every JSON name below as a stable
// cross-repository contract: additive changes only — with the one
// deliberate exception of version 2 below.
//
// # Version 2 (sk-674.18, ADR-3, D-10, K-15)
//
// With spaces, "the admin sees every project" stops being true: nobody but
// its owner sees a personal space's projects, the instance admin included.
// A role flag from which skein-web derives a subscription to ALL projects
// (`projects:all`) would deliver events of other users' personal spaces to
// admins. Version 2 therefore answers "all visible" with the SET of visible
// projects for every actor:
//
//   - `memberProjectIds` is replaced by `visibleProjectIds`: present whenever
//     the query asked AllVisible, for EVERY active user, the internal admin
//     included (for the admin — every project outside other users' personal
//     spaces, plus the admin's own personal space); `[]` for an inactive or
//     unknown user.
//   - `internalAdmin` keeps its name but means only the role (active, role
//     admin, not external, SKEIN_INSTANCE_ADMIN_ENABLED=true). It is no
//     longer a wildcard: no project is visible because of it alone, and
//     skein-web must not derive any subscription from it.
//
// This is a breaking change of the wire and is released together with
// skein-web's hub v2 (D-11, plan T049); the web side never reads
// `memberProjectIds` again. Visibility itself follows the access relation
// (authz), so `allowed` / `denied` already reflect spaces where the relation
// does.
//
// # Authorization
//
// Service secret ONLY: the route sits under RequireInternalSecret
// (`X-Skein-Internal-Secret`) and outside the ParseUserHeaders group, like
// `/api/v1/auth/oidc/provision`. User trust-headers are neither required nor
// read; whose access is checked is named in the body, not by the caller.
//
// # Responses
//
//   - 200 — [ProjectAccessResponse]: exactly one entry per request entry, in
//     request order. An unknown user, an unknown project and a project the
//     user cannot see are NOT errors: they are answered inside the entry
//     (inactive user; project in Denied).
//   - 400 `{"error":"invalid_request","message":"…"}` — syntax: the body is
//     not JSON of the shape below, `users` is missing, an entry has no
//     `userId`, a `userId` / project id is not a UUID, or the body exceeds
//     16 MiB. Unknown JSON fields are ignored (additive evolution).
//   - 401 `{"error":"invalid internal secret"}` — the service secret is
//     missing or wrong.
//
// # Batch budget
//
// The body is capped at 16 MiB. A maximal entry — a userId, 50 projectIds
// (HUB_MAX_PROJECTS_PER_CONNECTION) and allVisible — is about 2 KB of compact
// JSON (2,033 bytes measured), so the cap fits ~8,000 such entries. The
// contract budget is therefore 4,000 entries per call: half the cap, leaving
// room for pretty-printing and longer ids. A caller with more connections to
// check (skein-web's periodic re-check of a whole replica) splits them into
// several calls of at most 4,000 entries; a body past the cap is answered 400
// as a whole, nothing in it is checked.
//
// # Literals
//
// Request — one user checking two projects, one user asking for "all
// visible":
//
//	{"users":[
//	  {"userId":"0199a1b2-0000-7000-8000-000000000001",
//	   "projectIds":["0199a1b2-0000-7000-8000-0000000000a1","0199a1b2-0000-7000-8000-0000000000a2"]},
//	  {"userId":"0199a1b2-0000-7000-8000-000000000002","allVisible":true}
//	]}
//
// Response to it — the first user sees a1 only; the second is an active
// ordinary user who sees a1 and a3:
//
//	{"users":[
//	  {"userId":"0199a1b2-0000-7000-8000-000000000001","active":true,
//	   "internalAdmin":false,"external":false,
//	   "allowed":["0199a1b2-0000-7000-8000-0000000000a1"],
//	   "denied":["0199a1b2-0000-7000-8000-0000000000a2"]},
//	  {"userId":"0199a1b2-0000-7000-8000-000000000002","active":true,
//	   "internalAdmin":false,"external":false,
//	   "allowed":[],"denied":[],
//	   "visibleProjectIds":["0199a1b2-0000-7000-8000-0000000000a1","0199a1b2-0000-7000-8000-0000000000a3"]}
//	]}
//
// An internal admin asking for "all visible" gets `"internalAdmin":true`
// AND `visibleProjectIds` — the set, never a wildcard. A deactivated or
// unknown user gets `"active":false,"internalAdmin":false`, every requested
// project in `denied`, and `"visibleProjectIds":[]` when `allVisible` was
// asked.

// ProjectAccessRequest is the body of POST /api/v1/hub/project-access.
type ProjectAccessRequest struct {
	// Users is the batch to check. Required (absent → 400); an empty array
	// is valid and yields an empty answer.
	Users []ProjectAccessQuery `json:"users"`
}

// ProjectAccessQuery is one user's question inside a [ProjectAccessRequest].
type ProjectAccessQuery struct {
	// UserID is the user whose access is checked. Required.
	UserID uuid.UUID `json:"userId"`
	// ProjectIDs are the projects the user's connection declared and whose
	// visibility is asked. Optional: absent or empty means "no individual
	// projects to check" (e.g. an "all visible" question alone). Duplicates
	// are answered once.
	ProjectIDs []uuid.UUID `json:"projectIds,omitempty"`
	// AllVisible asks for the "all projects visible to me" mode (/activity):
	// when true the answer carries [ProjectAccessResult.VisibleProjectIDs]
	// for every user, the internal admin included. Optional: absent means
	// false.
	AllVisible bool `json:"allVisible,omitempty"`
}

// ProjectAccessResponse is the 200 body of POST /api/v1/hub/project-access.
type ProjectAccessResponse struct {
	// Users holds exactly one result per request entry, in request order.
	// Never null: an empty request yields `[]`.
	Users []ProjectAccessResult `json:"users"`
}

// ProjectAccessResult is the answer for one [ProjectAccessQuery]. Visibility
// follows the access relation (the same one [EnsureProjectVisible] applies):
// a deactivated or unknown user sees nothing; an external user — admin role
// or not — sees only the projects it is a member of; everyone else sees what
// the relation grants, and nobody but the owner sees a personal space's
// projects.
type ProjectAccessResult struct {
	// UserID echoes the request entry's user.
	UserID uuid.UUID `json:"userId"`
	// Active is true when the user exists and is not deactivated. False for
	// an unknown user id as well — the endpoint does not tell the two apart.
	// An inactive user gets no access of any kind.
	Active bool `json:"active"`
	// InternalAdmin is true when the user is active, has the admin role and
	// is not external. Version 2: the role ONLY — it grants nothing on the
	// wire by itself; visibility is in Allowed / Denied / VisibleProjectIDs.
	InternalAdmin bool `json:"internalAdmin"`
	// External is the user's external-collaborator flag as stored; false for
	// an unknown user. An external admin is limited to its memberships.
	External bool `json:"external"`
	// Allowed are the requested ProjectIDs the user can see, in request
	// order, each at most once. Always present; `[]` when none.
	Allowed []uuid.UUID `json:"allowed"`
	// Denied are the requested ProjectIDs the user cannot see — including
	// projects that do not exist, without telling the two apart — in request
	// order, each at most once. Always present; `[]` when none. Allowed and
	// Denied together are exactly the distinct requested ProjectIDs.
	Denied []uuid.UUID `json:"denied"`
	// VisibleProjectIDs are every project the user can see. Present exactly
	// when the query asked AllVisible, for every user, the internal admin
	// included; `[]` for an inactive or unknown user. Absent otherwise.
	// `omitzero`, not `omitempty`: a nil slice is absent while an empty
	// non-nil slice is written as `[]` — "asked, and none" stays
	// distinguishable from "not asked". Replaces version 1's
	// `memberProjectIds` (see "Version 2").
	VisibleProjectIDs []uuid.UUID `json:"visibleProjectIds,omitzero"`
}
