// Package spaces owns spaces — the personal space, the common space and
// regular spaces — their members, invitations, and the REST surface the
// frontend reads them through (sk-674.18).
//
// This file is the REST contract skein-api → skein-frontend of the spaces
// feature (T013): the new routes and their DTOs, the additive fields of
// existing responses, and the single registry of refusal codes shared by
// REST and MCP (SC-012). Only types, codes and documentation live here; each
// route and each additive field names the task that produces it. Treat every
// path, JSON name and code as a stable contract: additive changes only.
//
// # Callers and authorization
//
// All routes are under /api/v1 and require a user: the browser through
// skein-web (session → X-Skein-User-* headers) or an agent's API key. A
// route marked "admin" needs the instance admin role, which exists only
// while SKEIN_INSTANCE_ADMIN_ENABLED=true. The unauthenticated routes of
// accounts (signup, password reset, invitation by token without a session)
// are the contract of accounts/contract.go, not this one.
//
// # Identifiers
//
// A space is identified by its short id: 8 lowercase base32 characters
// (`k3x9p2qa`), unique, stable for the life of the space. It is the `id` of
// every space DTO, the `{id}` of every /spaces/{id} route, and the value of
// every `spaceId` field anywhere in the API (projects, issues, notifications,
// search, HOT ZONE, the personal signal `spaces`). The slug is derived from
// the name for the address `/spaces/<id>-<slug>` and is never looked up: a
// route finds the space by id alone, so a stale slug still resolves.
//
// # Refusals
//
// One shape for every refusal: `{"error":"<code>","message":"<prose>"}`
// plus the extra fields named at the code. Clients branch on `error` only.
//
// The machine code — `code` in the spec's terms — travels in the `error`
// field: that is the single refusal format of the whole REST API (the
// lead's decision for sk-674.18), and no separate `code` key exists. The
// constants below are those codes.
//
//   - 400 `validation_error` / `invalid_json` / `invalid_id` — syntax, as
//     everywhere in the API.
//   - 403 `forbidden` — the caller sees the space but its role in it does
//     not allow the action (a member renaming the space or inviting).
//   - 403 `not_admin` — a route marked "admin" called by a non-admin: these
//     routes sit behind RequireAdmin, whose refusal is
//     `{"error":"not_admin","message":…}` (POST …/members, POST …/owner,
//     /users/{id}/spaces*).
//   - 404 [CodeSpaceNotFound] — the space does not exist OR the caller may
//     not see it (another user's personal space, a space the caller is not
//     in): the two are indistinguishable (INV-3).
//   - 422 — the business refusals of [the code registry], listed at each
//     route. The same condition yields the same code in REST and MCP.
//   - 429 `rate_limited` — the route's rate limit.
//
// # Routes
//
//	GET    /api/v1/me/spaces                                   → 200 MySpacesResponse                   T023
//	POST   /api/v1/spaces                                      CreateSpaceRequest → 201 CreateSpaceResponse T023
//	GET    /api/v1/spaces/{id}                                 → 200 SpaceDetail                         T023
//	PATCH  /api/v1/spaces/{id}                                 RenameSpaceRequest → 200 SpaceDetail      T023
//	DELETE /api/v1/spaces/{id}                                 DeleteSpaceRequest → 204                  T023
//	GET    /api/v1/spaces/{id}/deletion-preview                → 200 SpaceDeletionPreview                T023
//	GET    /api/v1/spaces/{id}/members/{userId}/removal-preview → 200 MemberRemovalPreview               T023
//	DELETE /api/v1/spaces/{id}/members/{userId}                → 204                                     T023
//	POST   /api/v1/spaces/{id}/leave                           → 204                                     T023
//	POST   /api/v1/spaces/{id}/members                         AddMemberRequest → 201 SpaceMember        T023 (admin)
//	POST   /api/v1/spaces/{id}/owner                           ChangeOwnerRequest → 200 SpaceDetail      T023 (admin)
//	POST   /api/v1/spaces/{id}/invitations                     CreateInvitationRequest → 200 CreateInvitationResponse T040
//	POST   /api/v1/spaces/{id}/invitations/{invId}/resend      → 200 SpaceInvitation                     T040
//	DELETE /api/v1/spaces/{id}/invitations/{invId}             → 204                                     T040
//	GET    /api/v1/me/invitations                              → 200 MyInvitationsResponse               T040
//	POST   /api/v1/me/invitations/{invId}/accept               → 200 AcceptInvitationResponse            T040
//	POST   /api/v1/me/invitations/{invId}/decline              → 204                                     T040
//	GET    /api/v1/invitations/{token}                         → 200 TokenInvitationView                 T040
//	POST   /api/v1/invitations/{token}/accept                  → 200 AcceptInvitationResponse            T040
//	POST   /api/v1/invitations/{token}/decline                 → 204                                     T040
//	POST   /api/v1/me/email/confirmation                       ConfirmationEmailRequest → 204            T039
//	GET    /api/v1/me/deletion-preview                         → 200 AccountDeletionPreview              T041
//	DELETE /api/v1/me                                          DeleteAccountRequest → 204                T041
//	GET    /api/v1/projects/prefix-suggestions?name=&spaceId=  → 200 PrefixSuggestionsResponse           T025
//	GET    /api/v1/projects/prefix-check?prefix=               → 200 PrefixCheckResponse                 T025
//	GET    /api/v1/users/{id}/spaces                           → 200 UserSpacesResponse                  T037 (admin)
//	PUT    /api/v1/users/{id}/spaces                           PutUserSpacesRequest → 200 UserSpacesResponse T037 (admin)
//	POST   /api/v1/users/{id}/spaces/preview                   PutUserSpacesRequest → 200 UserSpacesPreview T037 (admin)
//	POST   /api/v1/projects/{projectID}/issues/{issueID}/unlock  → 200 (the issue card, as its GET)        T034
//	       unlock: releases the claim, the status stays; 422 force_unlock_forbidden for another holder's claim
//	       without force_unlock (also every force=true path, REST and MCP); events issue.unclaimed +
//	       issue.lock_force_released (new_data.by — the releaser) — written by unlock and by every
//	       force path that releases another holder's claim (unclaim, close, transition to closed);
//	       a force edit that keeps the claim writes the ordinary edit event                          T034
//
// # Additive fields of existing responses
//
// Existing responses gain the fields below (paths relative to /api/v1).
// Each is added to its Go struct — and to the response-key literal guarding that struct,
// if any — by the named task, in the same commit that starts filling it: a
// field is never served before it carries a real value. Until then the
// response is unchanged.
//
//	Route(s)                                   Key                  Type            Null / meaning                                      Task
//	GET /projects (rows), GET /projects/{id}   spaceId              string          never null: the project's space short id            T032
//	GET /projects (rows), GET /projects/{id}   visibility           string          VisibilitySpace | VisibilityMembers                 T024
//	GET /projects                              ?space=<id>          query           only that space's projects; rows ordered personal → common → others by name, then by name (or relevance with q) T024
//	POST /projects                             spaceId, visibility  body            see "Choosing the space of a new project" below; visibility omitted: SKEIN_PROJECT_DEFAULT_VISIBILITY (members in a personal space) T024
//	PATCH /projects/{id}                       visibility           body            space → members takes access away (INV-12); spaceId → 422 space_change_forbidden T024
//	GET /issues, /me/assigned-to, /me/created-by (rows), the issue card  spaceId  string  never null: the space of the issue's project       T032
//	GET /issues, /me/assigned-to, /me/created-by  ?space=<id>       query           only that space; myOutbox* counters follow           T032
//	the issue card (GET /projects/{projectID}/issues/{issueID})  canForceUnlock  boolean  the caller may release another holder's claim  T034
//	Inbox, Mentions, Watching rows; reminders  spaceId              string          the space of the row's issue                        T029
//	GET /me/notification-counts                inbox                number          unaccepted issues + pending invitations to me       T029
//	notification rows                          type                 string          + "space_invitation" (a pending invitation card)    T040
//	GET /search (rows)                         spaceId              string          the space of the hit                                T031
//	GET /search                                ?space=<id>          query           only that space                                     T031
//	GET /dashboard/* (rows)                    spaceId              string          the space of the row                                T030
//	GET /dashboard/*                           ?space=<id>          query           only that space                                     T030
//	GET /auth/me                               emailConfirmed       boolean         the email is confirmed (the closed set of spec US-5) T037
//	GET /auth/me                               hasApiKeys           boolean         the user has at least one active API key            T037
//	POST /users                                spaceIds             body string[]   spaces to join at creation (default: the common one, if any) T037
//	every person object (author, assignee, commenters, watchers, members, directory)  departed  boolean  left the space of this context; shown greyed     T027
//	every person object                        deleted              boolean         the account was deleted                             T027
//	every person object                        pseudonym            string | null   the stable alias shown for a deleted account; null otherwise T027
//	issue rows and the issue card (/issues, /issues/{id}, MCP show/list)  assigneeStatus, createdByStatus  PersonStatus  the marks of the assignee / the author        T027
//	comments                                   authorStatus         PersonStatus    the marks of the comment's author                   T027
//	mentions[] (MentionRef)                    status               PersonStatus    the marks of the mentioned person                   T027
//
// Types are JSON types. "every person object" means every place a response
// embeds a user: the same three keys everywhere, so the client renders a
// departed or deleted person one way. A row that names a person by a string
// (an issue's assignee and author, a comment's author, a mention) carries the
// same three keys as a PersonStatus object beside it. Each key is present
// only when it marks something (person.Status): absent means not departed,
// not deleted, no pseudonym — the client reads it exactly as false / null,
// and an object with nothing to mark is absent as a whole. The context of
// "departed" is the issue's project: a person outside its space (and not an
// external member of the project) has left it (INV-9). Implemented on the
// person objects of skein-api's issue, comment, mention, watcher, activity,
// member, space-member, user and directory/profile responses; the
// notification actor and the HOT ZONE contributors carry them with T029 and
// T030, which rework those responses.
//
// # Choosing the space of a new project (POST /projects, T024)
//
// "Eligible" spaces are those GET /me/spaces lists with
// canCreateProject=true.
//
//   - spaceId given: the project goes there. A space the caller cannot see
//     → 404 space_not_found; one the caller sees without membership (the
//     admin's adminAccess) → 422 CodeNotSpaceMember.
//   - spaceId omitted, exactly one eligible space → that one.
//   - spaceId omitted, several eligible spaces → 422 CodeSpaceRequired,
//     carrying `spaces`: [SpaceRef] of every eligible space, in column
//     order, so the client can ask.
//   - no eligible space at all — only an external user, who has no space and
//     cannot create projects → 422 `external_cannot_create_project`, as
//     today.
package spaces

import (
	"time"

	"skein-api/internal/person"
)

// ---------------------------------------------------------------------------
// Enumerations
// ---------------------------------------------------------------------------

// Space kinds (the `kind` field).
const (
	// KindPersonal — the user's personal space. Only its owner sees its
	// projects; nobody, the instance admin included, can be given access.
	KindPersonal = "personal"
	// KindCommon — the common space of a corporate installation.
	KindCommon = "common"
	// KindRegular — a space created by a user or an admin.
	KindRegular = "regular"
)

// Roles in a space (the `myRole` and member `role` fields).
const (
	// RoleOwner — the single owner of the space.
	RoleOwner = "owner"
	// RoleMember — a member.
	RoleMember = "member"
)

// Project visibility (the projects `visibility` field).
const (
	// VisibilitySpace — every member of the project's space sees it.
	VisibilitySpace = "space"
	// VisibilityMembers — only project members (and the space owner) see it.
	VisibilityMembers = "members"
)

// Invitation statuses as the owner sees them (SpaceInvitation.Status).
const (
	// InvitationPending — waiting for an answer; occupies a seat.
	InvitationPending = "pending"
	// InvitationNotAccepted — declined; shown neutrally for 7 days without
	// details, then gone. Occupies no seat.
	InvitationNotAccepted = "not_accepted"
)

// ---------------------------------------------------------------------------
// The code registry (SC-012)
// ---------------------------------------------------------------------------

// Refusal codes of the spaces feature. One list for REST and MCP: an MCP
// tool refusing for the same reason returns the same code. All are 422
// unless stated otherwise.
const (
	// CodeSpaceNotFound — 404: the space does not exist or the caller may
	// not see it (indistinguishable, INV-3).
	CodeSpaceNotFound = "space_not_found"

	// --- creating a space (POST /spaces; also the canCreate reason of
	// GET /me/spaces — the same decision yields the same code) ---

	// CodeOwnedLimit — SKEIN_SPACE_OWNED_LIMIT reached. Carries `ownedCount`
	// and `ownedLimit`. Never returned to an admin creating a space.
	CodeOwnedLimit = "owned_limit"
	// CodeAdminOnly — SKEIN_SPACE_CREATION=admin and the caller is not one.
	CodeAdminOnly = "admin_only"
	// CodeSpaceCreationDisabled — SKEIN_SPACE_CREATION=none.
	CodeSpaceCreationDisabled = "space_creation_disabled"
	// CodeExternalUser — the user is external: external users own no space,
	// are not invited to and not added to spaces (K12, E-16).
	CodeExternalUser = "external_user"

	// --- settings and deletion ---

	// CodePersonalSpace — the action does not exist for a personal space
	// (rename, delete, members, invitations, owner change).
	CodePersonalSpace = "personal_space"
	// CodeNameMismatch — DELETE /spaces/{id}: the typed confirmation is not
	// the space name.
	CodeNameMismatch = "name_mismatch"
	// CodeCommonSpaceNotEmpty — the common space still has projects and
	// cannot be deleted.
	CodeCommonSpaceNotEmpty = "common_space_not_empty"
	// CodeCommonSpaceEnabled — the empty common space cannot be deleted
	// while SKEIN_COMMON_SPACE_ENABLED=true (new users would have nowhere
	// to go).
	CodeCommonSpaceEnabled = "common_space_enabled"

	// --- members ---

	// CodeOwnerCannotLeave — the owner cannot leave; change the owner first.
	CodeOwnerCannotLeave = "owner_cannot_leave"
	// CodeOwnerCannotBeRemoved — the owner cannot be removed; change the
	// owner first.
	CodeOwnerCannotBeRemoved = "owner_cannot_be_removed"
	// CodeNotAMember — the user named is not a member of the space (removal,
	// new owner).
	CodeNotAMember = "not_a_member"
	// CodeAlreadyMember — the user is already a member (direct add by an
	// admin). Inviting a member is not a refusal — see
	// CreateInvitationResponse.
	CodeAlreadyMember = "already_member"
	// CodeNoSeats — SKEIN_SPACE_MEMBER_LIMIT reached, pending invitations
	// counted. Carries `memberCount`, `pendingInvitations`, `memberLimit`.
	CodeNoSeats = "no_seats"
	// CodeMemberViaSpace — a project member who is there through the space
	// (visibility `space`) cannot be removed from the project alone (US-17).
	CodeMemberViaSpace = "member_via_space"
	// CodeConcurrentChange — a membership operation lost to a concurrent one
	// after its retries; nothing was changed, repeat the request.
	CodeConcurrentChange = "concurrent_change"

	// --- invitations ---

	// CodeNoSuchUser — registration is closed and no account has this email:
	// nobody to invite.
	CodeNoSuchUser = "no_such_user"
	// CodeEmailDisabled — mail is off and the invited email has no account:
	// the invitation could not reach them. Same code as in
	// accounts/contract.go.
	CodeEmailDisabled = "email_disabled"
	// CodeDailyLimit — SKEIN_SPACE_INVITES_PER_DAY reached for the sender
	// (sliding 24 hours, one counter per sender across all spaces).
	CodeDailyLimit = "daily_limit"
	// CodeCaptchaRequired — suspicious invitation rate: repeat with
	// `turnstileToken` (only when Turnstile is configured).
	CodeCaptchaRequired = "captcha_required"
	// CodeCaptchaFailed — the Turnstile token did not verify.
	CodeCaptchaFailed = "captcha_failed"
	// CodeInvitationNotPending — resend of an invitation that is no longer
	// pending.
	CodeInvitationNotPending = "invitation_not_pending"
	// CodeInvitationInvalid — accept/decline of an invitation that expired,
	// was revoked or used ("The invitation is no longer valid").
	CodeInvitationInvalid = "invitation_invalid"
	// CodeSpaceDeleted — accept/decline of an invitation whose space was
	// deleted.
	CodeSpaceDeleted = "space_deleted"
	// CodeEmailUnconfirmed — accept from Inbox while mail is on and the
	// caller's email is not confirmed. The link from the invitation email
	// confirms it instead (accept by token is not refused for this).
	CodeEmailUnconfirmed = "email_unconfirmed"
	// CodeInvitationOtherEmail — accept/decline by token by a logged-in user
	// whose email is not the invited one.
	CodeInvitationOtherEmail = "invitation_other_email"
	// CodeInvitationAgentForbidden — MCP only: an agent cannot accept or
	// decline an invitation; a person does it in the web UI (INV-17).
	CodeInvitationAgentForbidden = "invitation_agent_forbidden"

	// --- quotas (SAAS-PLAN 4.5; T026) ---

	// CodeProjectQuota — SKEIN_QUOTA_PERSONAL_PROJECTS or
	// SKEIN_QUOTA_SPACE_PROJECTS reached (archived projects count). Carries
	// `spaceId`, `projectCount`, `projectQuota`.
	CodeProjectQuota = "project_quota"
	// CodeStorageQuota — the attachment storage quota of the space is used
	// up. To a participant of the space it carries `spaceId`,
	// `storageUsedBytes`, `storageQuotaBytes` (INV-28 (3)); to an external
	// user, who is not one, the code and the message alone.
	CodeStorageQuota = "storage_quota"

	// --- projects ---

	// CodePrefixTaken — the project prefix is taken (anywhere on the
	// installation, reserved prefixes of deleted projects included). Neutral:
	// it never names the holder. Carries `suggestions`: 2–3 free prefixes.
	CodePrefixTaken = "prefix_taken"
	// CodePrefixInvalid — the prefix breaks the prefix rule (pattern, first
	// character a digit, shorter than SKEIN_PROJECT_PREFIX_MIN_LENGTH).
	CodePrefixInvalid = "prefix_invalid"
	// CodeProjectNameTaken — a project with this name exists in the space
	// (names are unique per space).
	CodeProjectNameTaken = "project_name_taken"
	// CodeSpaceChangeForbidden — PATCH /projects/{id} tried to move the
	// project to another space; nothing is changed (E-31).
	CodeSpaceChangeForbidden = "space_change_forbidden"
	// CodeSpaceRequired — POST /projects without spaceId while the caller
	// can create projects in more than one space. Carries `spaces`:
	// [SpaceRef] of every space with canCreateProject=true (see "Choosing the
	// space of a new project").
	CodeSpaceRequired = "space_required"
	// CodeNotSpaceMember — the action needs membership of the space and the
	// caller only has the admin's access to it (adminAccess): creating a
	// project there (US-6, US-12). The admin's rights inside the space's
	// projects need no membership (sk-741).
	// The remedy is to add oneself as a member of the space. Carries
	// `spaceId`.
	CodeNotSpaceMember = "not_space_member"
	// CodeAssigneeNotAllowed — the assignee chosen by id is no longer
	// allowed for the issue (left the space, lost access to the project)
	// between the choice and the save; the previous assignment is kept
	// (INV-30, E-32).
	CodeAssigneeNotAllowed = "assignee_not_allowed"
	// CodeAmbiguousProject — a project string resolved to several accessible
	// projects by exact name (INV-29). Carries `q` (the string) and
	// `candidates`: [AmbiguousProjectCandidate] — one entry per ACCESSIBLE
	// project that matched, never a project the caller cannot see (US-12,
	// E-35). The same body in REST and MCP (T024 brings the MCP payload to
	// this form).
	CodeAmbiguousProject = "ambiguous_project"
	// CodeForceUnlockForbidden — the caller may not release another holder's
	// claim (only the project owner, the space owner and the admin may).
	CodeForceUnlockForbidden = "force_unlock_forbidden"

	// --- account deletion (T041) ---

	// CodeAccountDeletionDisabled — DELETE /me while SKEIN_SIGNUP_ENABLED is
	// false: accounts are managed by the admin.
	CodeAccountDeletionDisabled = "account_deletion_disabled"
	// CodeOwnsSpacesWithMembers — the caller owns spaces with other people;
	// they must be deleted first (ownership cannot be handed over yet).
	// Extra field `spaces`: []BlockingSpace, every blocking space (the
	// common one too, if owned).
	CodeOwnsSpacesWithMembers = "owns_spaces_with_members"
	// CodeOwnsCommonSpace — the caller owns the common space; an admin must
	// change its owner first. Extra field `spaces` as above.
	CodeOwnsCommonSpace = "owns_common_space"
	// CodeInvalidCredentials — the confirming password is wrong (the code
	// password login and password change already use).
	CodeInvalidCredentials = "invalid_credentials"
	// CodeLastAdminCannotDemote — the caller is the last active admin; the
	// deletion deactivates the account and would leave the instance
	// without one (the code the admin's deactivation and demotion already
	// answer).
	CodeLastAdminCannotDemote = "last_admin_cannot_demote"

	// --- users (T037) ---

	// CodeAdminRoleDisabled — the role admin is asked for (creating an
	// account, editing one — REST and the admin's agent) while the instance
	// admin role is disabled (SKEIN_INSTANCE_ADMIN_ENABLED=false, INV-20).
	// SSO provisioning does not refuse: it provisions a plain user.
	CodeAdminRoleDisabled = "admin_role_disabled"
)

// AmbiguousProjectCandidate is one entry of the `candidates` of
// CodeAmbiguousProject: enough to tell same-named projects apart and to
// re-call with an exact handle.
//
// Literal:
//
//	{"error":"ambiguous_project","message":"…","q":"Backend",
//	 "candidates":[{"idPrefix":"api","name":"Backend","spaceId":"k3x9p2qa","spaceName":"Acme"},
//	               {"idPrefix":"be","name":"Backend","spaceId":"p0aa11bb","spaceName":"Personal"}]}
type AmbiguousProjectCandidate struct {
	// IDPrefix is the project prefix — an exact handle to re-call with.
	IDPrefix string `json:"idPrefix"`
	// Name is the project name.
	Name string `json:"name"`
	// SpaceID is the short id of the project's space.
	SpaceID string `json:"spaceId"`
	// SpaceName is the name of the project's space.
	SpaceName string `json:"spaceName"`
}

// ---------------------------------------------------------------------------
// GET /api/v1/me/spaces — T023
// ---------------------------------------------------------------------------

// MySpacesResponse is the 200 body of GET /api/v1/me/spaces: every space the
// caller sees, whole, without pagination. For an ordinary user — the spaces
// they are in; for the instance admin — every space of the installation
// except other users' personal spaces, empty ones included; for an external
// user — none. Ordered as the projects column: personal → common → the rest
// by name.
//
// Literal:
//
//	{"items":[
//	  {"id":"p0aa11bb","slug":"personal","name":"Personal","kind":"personal",
//	   "myRole":"owner","adminAccess":false,"canCreateProject":true,
//	   "memberCount":1,"memberLimit":null,"pendingInvitations":0,
//	   "projectCount":3,"projectQuota":10,"storageUsedBytes":429496730,"storageQuotaBytes":1073741824},
//	  {"id":"k3x9p2qa","slug":"acme","name":"Acme","kind":"regular",
//	   "myRole":"owner","adminAccess":false,"canCreateProject":true,
//	   "memberCount":5,"memberLimit":25,"pendingInvitations":2,
//	   "projectCount":6,"projectQuota":null,"storageUsedBytes":0,"storageQuotaBytes":null}],
//	 "ownedCount":1,"ownedLimit":3,"canCreate":true,"cannotCreateReason":null}
type MySpacesResponse struct {
	// Items are the spaces, in column order.
	Items []MySpace `json:"items"`
	// OwnedCount is how many spaces the caller owns, the personal and the
	// common space not counted (what SKEIN_SPACE_OWNED_LIMIT limits).
	OwnedCount int `json:"ownedCount"`
	// OwnedLimit is SKEIN_SPACE_OWNED_LIMIT; null = no limit.
	OwnedLimit *int `json:"ownedLimit"`
	// CanCreate reports whether POST /spaces would be allowed now.
	CanCreate bool `json:"canCreate"`
	// CannotCreateReason is null when CanCreate, otherwise the code POST
	// /spaces would return: CodeOwnedLimit | CodeAdminOnly |
	// CodeSpaceCreationDisabled | CodeExternalUser.
	CannotCreateReason *string `json:"cannotCreateReason"`
}

// MySpace is one space of GET /api/v1/me/spaces.
type MySpace struct {
	// ID is the space short id.
	ID string `json:"id"`
	// Slug is the address slug derived from the name.
	Slug string `json:"slug"`
	// Name is the space name (not unique).
	Name string `json:"name"`
	// Kind is KindPersonal | KindCommon | KindRegular.
	Kind string `json:"kind"`
	// MyRole is RoleOwner | RoleMember; null for the admin in a space they
	// are not a member of (then AdminAccess is true).
	MyRole *string `json:"myRole"`
	// AdminAccess is true for the instance admin in a space they are not a
	// member of: owner rights over the space itself, no project rights.
	AdminAccess bool `json:"adminAccess"`
	// CanCreateProject reports whether the caller may create projects here:
	// every member, external users and admin-access never.
	CanCreateProject bool `json:"canCreateProject"`
	// MemberCount is the number of members.
	MemberCount int `json:"memberCount"`
	// MemberLimit is SKEIN_SPACE_MEMBER_LIMIT; null = no limit (always null
	// for the personal and the common space).
	MemberLimit *int `json:"memberLimit"`
	// PendingInvitations is the number of pending invitations (they occupy
	// seats); 0 for the personal space.
	PendingInvitations int `json:"pendingInvitations"`
	// ProjectCount is the number of projects of the space, archived
	// included.
	ProjectCount int `json:"projectCount"`
	// ProjectQuota is the project quota of the space
	// (SKEIN_QUOTA_PERSONAL_PROJECTS / SKEIN_QUOTA_SPACE_PROJECTS); null = no
	// quota.
	ProjectQuota *int `json:"projectQuota"`
	// StorageUsedBytes is the attachment bytes stored in the space.
	StorageUsedBytes int64 `json:"storageUsedBytes"`
	// StorageQuotaBytes is the storage quota (SKEIN_QUOTA_*_STORAGE_BYTES)
	// in bytes; null = no quota.
	StorageQuotaBytes *int64 `json:"storageQuotaBytes"`
}

// ---------------------------------------------------------------------------
// POST /api/v1/spaces — T023
// ---------------------------------------------------------------------------

// CreateSpaceRequest is the body of POST /api/v1/spaces: a new regular
// space. Refusals: 422 CodeOwnedLimit (never for an admin),
// CodeAdminOnly, CodeSpaceCreationDisabled, CodeExternalUser (the caller,
// or the ownerId user, is external); 400 validation_error (empty name);
// 403 forbidden (ownerId from a non-admin).
//
// Literal: {"name":"Backend","ownerId":"0199a1b2-0000-7000-8000-000000000002"}
type CreateSpaceRequest struct {
	// Name — the space name; required, not unique.
	Name string `json:"name"`
	// OwnerID — the owner's user id (UUID). Admin only; omitted = the
	// caller. An admin naming someone else does not become a member.
	OwnerID *string `json:"ownerId,omitempty"`
}

// CreateSpaceResponse is the 201 body of POST /api/v1/spaces.
type CreateSpaceResponse struct {
	// ID is the new space's short id.
	ID string `json:"id"`
	// Slug is the address slug.
	Slug string `json:"slug"`
}

// ---------------------------------------------------------------------------
// GET / PATCH / DELETE /api/v1/spaces/{id} — T023
// ---------------------------------------------------------------------------

// SpaceDetail is the 200 body of GET /api/v1/spaces/{id} (members and the
// admin, never for another user's personal space) and of PATCH and
// POST …/owner. The MySpace fields describe the space; members and, for the
// owner and the admin, invitations follow.
type SpaceDetail struct {
	MySpace
	// Members are the space members, owner first, then by username.
	Members []SpaceMember `json:"members"`
	// Invitations are pending and recently declined invitations, newest
	// first; null for a caller who may not manage them (a member).
	Invitations []SpaceInvitation `json:"invitations"`
	// CanDelete reports whether DELETE would be allowed now for the caller.
	CanDelete bool `json:"canDelete"`
	// CannotDeleteReason is null when CanDelete, otherwise the refusal code:
	// CodePersonalSpace | CodeCommonSpaceNotEmpty | CodeCommonSpaceEnabled |
	// "forbidden".
	CannotDeleteReason *string `json:"cannotDeleteReason"`
}

// SpaceMember is one member of a space.
type SpaceMember struct {
	// UserID is the user id (UUID).
	UserID string `json:"userId"`
	// Username is the @-mention handle.
	Username string `json:"username"`
	// Name is the display name.
	Name string `json:"name"`
	// Role is RoleOwner | RoleMember.
	Role string `json:"role"`
	// Deactivated marks a deactivated account (membership stays).
	Deactivated bool `json:"deactivated"`
	// The person marks (T027): a deleted account is marked deleted with its
	// pseudonym; a participant is never departed from his own space.
	person.Status
}

// SpaceInvitation is one invitation as the space owner and the admin see it.
// The invited email is shown only here, to those who sent or manage it.
type SpaceInvitation struct {
	// ID is the invitation id (UUID).
	ID string `json:"id"`
	// Email is the invited email, lowercased.
	Email string `json:"email"`
	// Status is InvitationPending | InvitationNotAccepted.
	Status string `json:"status"`
	// InvitedBy is the username of the sender.
	InvitedBy string `json:"invitedBy"`
	// ExpiresAt is when a pending invitation expires (RFC 3339, UTC); null
	// for not_accepted.
	ExpiresAt *time.Time `json:"expiresAt"`
	// CreatedAt is when it was (last) sent.
	CreatedAt time.Time `json:"createdAt"`
}

// RenameSpaceRequest is the body of PATCH /api/v1/spaces/{id} (owner,
// admin). Refusals: 422 CodePersonalSpace; 400 validation_error (empty
// name); 403 forbidden.
//
// Literal: {"name":"Acme Corp"}
type RenameSpaceRequest struct {
	// Name — the new name; required.
	Name string `json:"name"`
}

// DeleteSpaceRequest is the body of DELETE /api/v1/spaces/{id} (owner,
// admin): the space with every project, issue and attachment, irreversibly;
// pending invitations are revoked, members lose access at once. Refusals:
// 422 CodeNameMismatch, CodePersonalSpace, CodeCommonSpaceNotEmpty,
// CodeCommonSpaceEnabled; 403 forbidden.
//
// Literal: {"confirmName":"Acme"}
type DeleteSpaceRequest struct {
	// ConfirmName — the space name typed by the person; compared exactly by
	// the server.
	ConfirmName string `json:"confirmName"`
}

// SpaceDeletionPreview is the 200 body of
// GET /api/v1/spaces/{id}/deletion-preview (owner, admin): what DELETE would
// destroy, computed now.
type SpaceDeletionPreview struct {
	// ProjectCount is the number of projects, archived included.
	ProjectCount int `json:"projectCount"`
	// IssueCount is the number of issues in them.
	IssueCount int `json:"issueCount"`
	// StorageBytes is the attachment bytes in them.
	StorageBytes int64 `json:"storageBytes"`
	// PendingInvitations is the number of invitations that will be revoked.
	PendingInvitations int `json:"pendingInvitations"`
	// OwnerUsername is the owner — the admin's dialog names them.
	OwnerUsername string `json:"ownerUsername"`
}

// MemberRemovalPreview is the 200 body of
// GET /api/v1/spaces/{id}/members/{userId}/removal-preview: the consequences
// of removing that member (owner, admin) or of the caller leaving ({userId}
// = the caller). The same numbers the removal then produces.
type MemberRemovalPreview struct {
	// OpenAssignedIssues is how many open issues of the space's projects
	// lose this assignee.
	OpenAssignedIssues int `json:"openAssignedIssues"`
	// ClaimedIssues is how many claims of the user and their agents in the
	// space are released.
	ClaimedIssues int `json:"claimedIssues"`
	// OwnedProjects are the space's projects where the user has the project
	// owner role; they stay under the space owner.
	OwnedProjects []ProjectRef `json:"ownedProjects"`
}

// ProjectRef names a project.
type ProjectRef struct {
	// ID is the project id (UUID).
	ID string `json:"id"`
	// Name is the project name.
	Name string `json:"name"`
	// IDPrefix is the project prefix.
	IDPrefix string `json:"idPrefix"`
}

// DELETE /api/v1/spaces/{id}/members/{userId} (owner, admin) removes a
// member; POST /api/v1/spaces/{id}/leave (a member) leaves. Both 204, both
// revoke access to the space's projects (INV-12: assignee, claims, project
// membership, watching). Refusals: 422 CodeOwnerCannotBeRemoved /
// CodeOwnerCannotLeave, CodeNotAMember, CodePersonalSpace,
// CodeConcurrentChange; 403 forbidden.

// AddMemberRequest is the body of POST /api/v1/spaces/{id}/members (admin
// only): the user becomes a member at once, without consent; a pending
// invitation of theirs to this space is closed as accepted. 201 body:
// SpaceMember. Refusals: 422 CodeExternalUser, CodeAlreadyMember,
// CodeNoSeats, CodePersonalSpace, CodeConcurrentChange; 403 not_admin.
//
// Literal: {"userId":"0199a1b2-0000-7000-8000-000000000003"}
type AddMemberRequest struct {
	// UserID — the user to add (UUID); the admin may add themselves.
	UserID string `json:"userId"`
}

// ChangeOwnerRequest is the body of POST /api/v1/spaces/{id}/owner (admin
// only): the new owner must already be a member; the previous owner stays a
// member; SKEIN_SPACE_OWNED_LIMIT is not checked; pending invitations are
// untouched. 200 body: SpaceDetail. Refusals: 422 CodeNotAMember,
// CodePersonalSpace; 403 not_admin.
//
// Literal: {"userId":"0199a1b2-0000-7000-8000-000000000003"}
type ChangeOwnerRequest struct {
	// UserID — the new owner (UUID).
	UserID string `json:"userId"`
}

// ---------------------------------------------------------------------------
// Invitations — T040
// ---------------------------------------------------------------------------

// CreateInvitationRequest is the body of POST /api/v1/spaces/{id}/invitations
// (owner, admin). Exactly one of Email or UserID. A person picked from the
// directory is sent by id: the directory never discloses emails.
//
// Answers, by the flags SKEIN_SIGNUP_ENABLED × SKEIN_EMAIL_ENABLED (screens
// 2.4): registration open — always the same neutral 200, whether an account
// exists or not, with the same steps taken; registration closed — 200 for an
// existing user, 422 CodeNoSuchUser (mail on) or CodeEmailDisabled (mail
// off) for an email without an account. Other refusals: 422
// CodeExternalUser, CodeNoSeats, CodeDailyLimit, CodeCaptchaRequired,
// CodeCaptchaFailed, CodePersonalSpace; 403 forbidden; 400 validation_error
// (neither or both of email/userId, malformed email, language not "ru" or
// "en").
//
// The email is written in the inviter's interface language, sent in
// `language` (spec: "the invitation — the inviter's language"). The invitation
// stores it as its email language; it is not stored on the user's profile.
//
// Literal: {"email":"dev@client.io","language":"ru"}
type CreateInvitationRequest struct {
	// Email — the invited email.
	Email *string `json:"email,omitempty"`
	// UserID — the invited user (UUID); the server takes their email.
	UserID *string `json:"userId,omitempty"`
	// Language — the interface language of the inviter, the language of the
	// invitation email: "ru" | "en"; required.
	Language string `json:"language"`
	// TurnstileToken — required only after CodeCaptchaRequired.
	TurnstileToken *string `json:"turnstileToken,omitempty"`
}

// CreateInvitationResponse is the 200 body of
// POST /api/v1/spaces/{id}/invitations. Outcome tells the three non-error
// cases apart; with registration open "sent" is also the answer for an
// email that already has a pending invitation and for one without an
// account (the composition never differs).
//
// Literal: {"outcome":"sent","invitation":null}
type CreateInvitationResponse struct {
	// Outcome is "sent" | "already_member" (nothing created; informational,
	// not an error) | "already_pending" (the existing pending invitation is
	// returned; registration closed only — with it open the answer is
	// "sent").
	Outcome string `json:"outcome"`
	// Invitation is the pending invitation for "already_pending" so the form
	// can offer resend; null otherwise.
	Invitation *SpaceInvitation `json:"invitation"`
}

// POST /api/v1/spaces/{id}/invitations/{invId}/resend (owner, admin): a new
// token and a new expiry for a pending invitation; the old link stops
// working. The new email is written in the language stored in the
// invitation at creation (the original inviter's), whoever resends it — the
// addressee keeps getting one invitation in one language. A resend is a send
// of whoever resends: it counts towards the daily limit and passes the same
// captcha rule as a creation. Body: optional
// ResendInvitationRequest (an empty body is no token). 200 body:
// SpaceInvitation. Refusals, in this order: 404 space_not_found, 422
// CodePersonalSpace, 403 forbidden; the send limits before the invitation
// is looked up — 422 CodeDailyLimit, CodeCaptchaFailed (a token sent and not
// verified), CodeCaptchaRequired (at the threshold without a token); then
// 404 invitation_not_found, 422 CodeInvitationNotPending; then, under the
// caller's lock, CodeDailyLimit / CodeCaptchaRequired again.
//
// DELETE /api/v1/spaces/{id}/invitations/{invId} (owner, admin): revokes a
// pending invitation, freeing its seat; removes a not_accepted row. 204.
// Refusals: 403 forbidden; 404 invitation_not_found.

// ResendInvitationRequest is the optional body of
// POST /api/v1/spaces/{id}/invitations/{invId}/resend.
//
// Literal: {"turnstileToken":"0.AbCd…"}
type ResendInvitationRequest struct {
	// TurnstileToken — required only after CodeCaptchaRequired.
	TurnstileToken *string `json:"turnstileToken,omitempty"`
}

// MyInvitationsResponse is the 200 body of GET /api/v1/me/invitations: the
// pending invitations addressed to the caller, newest first. Never the
// token or the link.
type MyInvitationsResponse struct {
	// Items are the pending invitations.
	Items []MyInvitation `json:"items"`
}

// MyInvitation is one invitation addressed to the caller.
type MyInvitation struct {
	// ID is the invitation id (UUID), for accept/decline.
	ID string `json:"id"`
	// SpaceID is the space short id.
	SpaceID string `json:"spaceId"`
	// SpaceName is the space name.
	SpaceName string `json:"spaceName"`
	// InvitedBy is the sender's username.
	InvitedBy string `json:"invitedBy"`
	// ExpiresAt is the expiry (RFC 3339, UTC).
	ExpiresAt time.Time `json:"expiresAt"`
	// CanAccept reports whether accept would succeed now.
	CanAccept bool `json:"canAccept"`
	// CannotAcceptReason is null when CanAccept, otherwise
	// CodeEmailUnconfirmed.
	CannotAcceptReason *string `json:"cannotAcceptReason"`
}

// AcceptInvitationResponse is the 200 body of
// POST /api/v1/me/invitations/{invId}/accept and
// POST /api/v1/invitations/{token}/accept: the space joined. The member
// limit is not re-checked (the seat was taken when it was sent). Refusals
// (accept and decline alike): 422 CodeInvitationInvalid, CodeSpaceDeleted;
// from Inbox also CodeEmailUnconfirmed; by token also
// CodeInvitationOtherEmail; MCP: CodeInvitationAgentForbidden. Decline is
// 204 with the same refusals.
type AcceptInvitationResponse struct {
	// Space is the space joined, as GET /me/spaces lists it.
	Space MySpace `json:"space"`
}

// Logged-in states of GET /api/v1/invitations/{token}, in the order checked
// (spec US-5, screens 2.5). The anonymous view is accounts/contract.go.
const (
	// TokenSpaceDeleted — the space was deleted.
	TokenSpaceDeleted = "space_deleted"
	// TokenAlreadyMember — the caller is already in the space (checked
	// before "invalid": a used token of a member answers this).
	TokenAlreadyMember = "already_member"
	// TokenInvalid — expired, revoked or used.
	TokenInvalid = "invalid"
	// TokenOtherEmail — the invitation is for another address (the address
	// is not shown).
	TokenOtherEmail = "other_email"
	// TokenValid — the accept screen.
	TokenValid = "valid"
)

// TokenInvitationView is the 200 body of GET /api/v1/invitations/{token}
// for a logged-in caller. Opening it as the invited user confirms that
// user's email (the link proves the mailbox) — the one state change a GET of
// a link makes; it accepts nothing.
//
// Literal: {"status":"valid","space":{"id":"k3x9p2qa","slug":"acme","name":"Acme"},"inviterUsername":"anna","expiresAt":"2026-10-19T18:13:03Z"}
type TokenInvitationView struct {
	// Status is TokenSpaceDeleted | TokenAlreadyMember | TokenInvalid |
	// TokenOtherEmail | TokenValid.
	Status string `json:"status"`
	// Space names the space for already_member and valid; null otherwise.
	Space *SpaceRef `json:"space"`
	// InviterUsername is the sender for valid; null otherwise.
	InviterUsername *string `json:"inviterUsername"`
	// ExpiresAt is the expiry for valid; null otherwise.
	ExpiresAt *time.Time `json:"expiresAt"`
}

// SpaceRef names a space.
type SpaceRef struct {
	// ID is the space short id.
	ID string `json:"id"`
	// Slug is the address slug.
	Slug string `json:"slug"`
	// Name is the space name.
	Name string `json:"name"`
}

// ---------------------------------------------------------------------------
// POST /api/v1/me/email/confirmation — T039
// ---------------------------------------------------------------------------

// ConfirmationEmailRequest is the body of POST /api/v1/me/email/confirmation:
// queue a confirmation email to the caller (the latest link is the one that
// works). 204. Refusals: 422 email_disabled, 429 rate_limited; an already
// confirmed email is 204 with nothing sent.
//
// Literal: {"language":"ru"}
type ConfirmationEmailRequest struct {
	// Language — the email language: "ru" | "en"; required.
	Language string `json:"language"`
}

// ---------------------------------------------------------------------------
// Account deletion — T041
// ---------------------------------------------------------------------------

// AccountDeletionPreview is the 200 body of GET /api/v1/me/deletion-preview.
// Refusal: 422 CodeAccountDeletionDisabled.
type AccountDeletionPreview struct {
	// Blocking are the spaces that prevent deletion: owned with other
	// members, and the common space if owned. Empty = deletion possible.
	Blocking []BlockingSpace `json:"blocking"`
	// Deleted are the spaces deleted with the account: the personal space and
	// owned spaces where the caller is alone.
	Deleted []DeletedSpacePreview `json:"deleted"`
	// OpenAssignedIssues is how many open issues in other spaces lose the
	// caller as assignee.
	OpenAssignedIssues int `json:"openAssignedIssues"`
}

// BlockingSpace is a space that blocks account deletion.
type BlockingSpace struct {
	SpaceRef
	// Kind is KindCommon or KindRegular.
	Kind string `json:"kind"`
	// MemberCount is the number of members.
	MemberCount int `json:"memberCount"`
}

// DeletedSpacePreview is a space deleted with the account.
type DeletedSpacePreview struct {
	SpaceRef
	// Kind is KindPersonal or KindRegular.
	Kind string `json:"kind"`
	// ProjectCount is its number of projects.
	ProjectCount int `json:"projectCount"`
}

// DeleteAccountRequest is the body of DELETE /api/v1/me: the account is
// deleted at once and for good (spaces of DeletedSpacePreview, leaving the
// rest, anonymization, every session and API key revoked). 204. Refusals,
// checked in this order: 422 CodeAccountDeletionDisabled; 400
// invalid_request (not one JSON object, password missing); 401
// session_revoked (the account was deleted meanwhile — another tab, the
// operator; the code of the session gate); CodeInvalidCredentials; CodeOwnsSpacesWithMembers, then
// CodeOwnsCommonSpace (both with the extra field `spaces`);
// CodeLastAdminCannotDemote.
//
// Literal: {"password":"correct horse battery"}
type DeleteAccountRequest struct {
	// Password — the caller's password, confirming the deletion.
	Password string `json:"password"`
}

// ---------------------------------------------------------------------------
// Project prefixes — T025
// ---------------------------------------------------------------------------

// PrefixSuggestionsResponse is the 200 body of
// GET /api/v1/projects/prefix-suggestions?name=<name>&spaceId=<id>: free
// prefixes derived from the project name (SAAS-PLAN 4.4), best first.
// Suggestions do not reserve. Refusals: 400 validation_error (no name), 429
// rate_limited.
//
// Literal: {"suggestions":["ps","pls","plsh","ps2"]}
type PrefixSuggestionsResponse struct {
	// Suggestions — the first one goes into the field, the next 3–4 are
	// offered as buttons.
	Suggestions []string `json:"suggestions"`
}

// PrefixCheckResponse is the 200 body of
// GET /api/v1/projects/prefix-check?prefix=<prefix>. A taken prefix is a
// neutral answer: it never says where or by whom. Refusal: 429 rate_limited.
//
// Literal: {"available":false,"reason":"prefix_taken","suggestions":["ps2","psh"]}
type PrefixCheckResponse struct {
	// Available reports whether the prefix can be used now.
	Available bool `json:"available"`
	// Reason is null when available, otherwise CodePrefixTaken |
	// CodePrefixInvalid.
	Reason *string `json:"reason"`
	// Suggestions are 2–3 free prefixes when taken; empty otherwise.
	Suggestions []string `json:"suggestions"`
}

// ---------------------------------------------------------------------------
// A user's spaces (admin) — T037
// ---------------------------------------------------------------------------

// UserSpacesResponse is the 200 body of GET and PUT /api/v1/users/{id}/spaces
// (admin): the user's memberships outside personal spaces.
type UserSpacesResponse struct {
	// Items are the spaces the user is in, with their role.
	Items []UserSpace `json:"items"`
}

// UserSpace is one membership of a user.
type UserSpace struct {
	SpaceRef
	// Kind is KindCommon or KindRegular.
	Kind string `json:"kind"`
	// Role is RoleOwner | RoleMember.
	Role string `json:"role"`
}

// PutUserSpacesRequest is the body of PUT /api/v1/users/{id}/spaces and of
// POST /api/v1/users/{id}/spaces/preview: the whole wanted set. Added spaces
// make the user a member at once; removed ones are removals with their
// consequences (US-7). PUT applies the set in one operation. Refusals: 422
// CodeOwnerCannotBeRemoved (a space the user owns is missing from the set),
// CodeNoSeats (carries `spaceId`), CodeExternalUser, CodeConcurrentChange;
// 404 user_not_found; 403 not_admin (the RequireAdmin refusal, as for every
// admin route — see "Refusals").
//
// Literal: {"spaceIds":["k3x9p2qa","m7z2w4cd"]}
type PutUserSpacesRequest struct {
	// SpaceIDs — space short ids; personal spaces are not accepted.
	SpaceIDs []string `json:"spaceIds"`
}

// UserSpacesPreview is the 200 body of POST /api/v1/users/{id}/spaces/preview:
// what PUT with the same body would do, one entry per removed space.
type UserSpacesPreview struct {
	// Removed are the spaces the user would leave, each with the removal
	// consequences.
	Removed []UserSpaceRemoval `json:"removed"`
}

// UserSpaceRemoval is one removed space of a preview.
type UserSpaceRemoval struct {
	SpaceRef
	MemberRemovalPreview
}
