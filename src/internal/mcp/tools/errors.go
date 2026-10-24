package tools

// Error code constants returned by every MCP tool in this package. The set
// is closed: handlers branch on these codes and clients (agents) parse
// them as stable identifiers. Adding a new code is a contract change —
// document it in the project's planning documents before introducing it
// here.
//
// R2.2a (read tools) only emits a subset; the rest are reserved for the
// write tools (R2.2b/c). Keeping them all here means the closed set is
// authoritative in one place — there's no second list to drift out of
// sync as the write tools land.
//
// R2.3 added the lock-aware codes (already_claimed, locked_by_other,
// not_lock_holder). R3.1 added the user-management + member-management
// codes (not_admin, user_not_found, email_taken, username_taken,
// member_not_found, last_admin_cannot_demote). Later slices
// added attachments, commits, reparent, standing, themes, reminders, etc.
// The closed set lives in the ErrCode* constants below — count them
// rather than trusting a stale hard-coded number in this comment.
const (
	ErrCodeProjectNotFound         = "project_not_found"
	ErrCodeIssueNotFound           = "issue_not_found"
	ErrCodeVersionConflict         = "version_conflict"
	ErrCodeValidationError         = "validation_error"
	ErrCodeForbidden               = "forbidden"
	ErrCodeUnauthenticated         = "unauthenticated"
	ErrCodeInternalError           = "internal_error"
	ErrCodeDependencyCycle         = "dependency_cycle"
	ErrCodeDependencyAlreadyExists = "dependency_already_exists"
	ErrCodeParentClosed            = "parent_closed"
	ErrCodeParentNotFound          = "parent_not_found"
	ErrCodeParentWrongProject      = "parent_wrong_project"
	ErrCodeIDTaken                 = "id_taken"
	ErrCodeComponentsWrongProject  = "components_wrong_project"
	ErrCodePinnedNoForce           = "pinned_no_force"
	ErrCodeAmbiguousProject        = "ambiguous_project"

	// R2.3 — lock-aware concurrency codes.
	ErrCodeAlreadyClaimed = "already_claimed"
	ErrCodeLockedByOther  = "locked_by_other"
	ErrCodeNotLockHolder  = "not_lock_holder"

	// sk-18 — claim refuses to re-open a closed issue. Closing is a
	// terminal state; reopen via skein_update(status=open) first.
	ErrCodeCannotClaimClosed = "cannot_claim_closed"

	// sk-25 — accept refuses to ack a closed issue. Symmetric to
	// cannot_claim_closed: both reject "start work" / "ack work"
	// verbs on a terminal-state row. Reopen via skein_update(
	// status=open) first if the closure was wrong.
	ErrCodeCannotAcceptClosed = "cannot_accept_closed"

	// R3.1 — user/member management codes.
	//
	// not_admin              — caller is not an org admin (skein_user_*).
	// user_not_found         — referenced user id/username does not exist.
	// email_taken            — email already used by another user row.
	// username_taken         — username already used by another user row.
	// member_not_found       — user is not a member of the project.
	// last_admin_cannot_demote — would leave zero active admins.
	ErrCodeNotAdmin              = "not_admin"
	ErrCodeUserNotFound          = "user_not_found"
	ErrCodeEmailTaken            = "email_taken"
	ErrCodeUsernameTaken         = "username_taken"
	ErrCodeMemberNotFound        = "member_not_found"
	ErrCodeLastAdminCannotDemote = "last_admin_cannot_demote"

	// sk-674.18 T018 — membership primitives (codes of the spaces
	// registry, spaces/contract.go; the same in REST):
	//
	// member_via_space   — the member keeps the project through the space
	//                      (US-17, E-27); nothing changed.
	// concurrent_change  — the membership operation lost to a concurrent
	//                      one after the bounded retries (K-7).
	ErrCodeMemberViaSpace   = "member_via_space"
	ErrCodeConcurrentChange = "concurrent_change"
	// sk-674.18 T024 — projects in spaces and the admin agent adding to a
	// space (codes of the spaces registry, spaces/contract.go; the same in
	// REST):
	//
	// space_not_found              — no such space, or not visible (INV-3).
	// personal_space               — the action does not exist for a "Personal space".
	// external_user                — external users are not in spaces.
	// already_member               — the user is already a member.
	// no_seats                     — SKEIN_SPACE_MEMBER_LIMIT reached.
	// not_space_member             — the caller sees the space only as the
	//                                admin: add yourself as a member first.
	// space_required               — several spaces could take the project;
	//                                name one (`spaces` lists them).
	// space_change_forbidden       — a project never changes its space.
	// external_cannot_create_project — an external user creates no project.
	ErrCodeSpaceNotFound               = "space_not_found"
	ErrCodePersonalSpace               = "personal_space"
	ErrCodeExternalUser                = "external_user"
	ErrCodeAlreadyMember               = "already_member"
	ErrCodeNoSeats                     = "no_seats"
	ErrCodeNotSpaceMember              = "not_space_member"
	ErrCodeSpaceRequired               = "space_required"
	ErrCodeSpaceChangeForbidden        = "space_change_forbidden"
	ErrCodeExternalCannotCreateProject = "external_cannot_create_project"
	// sk-674.18 T028 — the assignee (spaces registry, same in REST):
	//
	// assignee_not_allowed — the assignee chosen by id (a user UUID) names
	//                        nobody who may work in the issue's project now
	//                        (INV-30); nothing is written. A username or an
	//                        e-mail is stored as text and never refused.
	ErrCodeAssigneeNotAllowed = "assignee_not_allowed"
	// sk-674.18 T034 — force_unlock (spaces registry, same in REST 422):
	//
	// force_unlock_forbidden — force=true (or the unlock action) on an issue
	//                          of a project where the caller may not release
	//                          another holder's claim: only the project
	//                          owner, the space owner and the instance admin
	//                          may (US-16, K-28).
	ErrCodeForceUnlockForbidden = "force_unlock_forbidden"
	// sk-674.18 T029 — invitations (spaces registry, same in REST):
	//
	// invitation_agent_forbidden — skein_accept on a space invitation id:
	//                              an agent cannot accept or decline an
	//                              invitation; the person does it in the
	//                              web UI (INV-17).
	ErrCodeInvitationAgentForbidden = "invitation_agent_forbidden"
	// sk-674.18 T025 — prefixes and names (spaces registry, same in REST):
	//
	// prefix_taken       — the prefix is taken anywhere or reserved; neutral,
	//                      carries 2–3 free `suggestions`.
	// prefix_invalid     — the prefix breaks the rule (a digit first,
	//                      pattern, shorter than the minimum).
	// project_name_taken — a project of the space has this name.
	ErrCodePrefixTaken      = "prefix_taken"
	ErrCodePrefixInvalid    = "prefix_invalid"
	ErrCodeProjectNameTaken = "project_name_taken"
	// sk-674.18 T026 — quotas (spaces registry, same in REST):
	//
	// project_quota — the space holds its project quota (archived counted);
	//                 carries spaceId, projectCount, projectQuota.
	// storage_quota — the space's attachment storage reached its quota;
	//                 carries spaceId, storageUsedBytes, storageQuotaBytes
	//                 to a participant of the space only (INV-28 (3)).
	ErrCodeProjectQuota = "project_quota"
	ErrCodeStorageQuota = "storage_quota"
	// sk-674.18 T037 — admin_role_disabled: the instance admin role is
	// disabled (SKEIN_INSTANCE_ADMIN_ENABLED=false); no path assigns it
	// (INV-20). The same code as REST.
	ErrCodeAdminRoleDisabled = "admin_role_disabled"

	// R4.2 — attachment codes.
	//
	// attachment_not_found       — row missing or not visible.
	// attachments_unavailable    — S3_URL not configured.
	// attachment_not_ready       — download requested for a 'pending' row.
	// attachment_object_missing  — confirm called but the object never
	//                              landed in S3.
	// attachment_limit_exceeded  — per-issue cap (50 soft / 100 hard).
	// attachment_mime_blocked    — filename ext on the deny-list.
	// attachment_too_large       — sizeBytes > config cap.
	ErrCodeAttachmentNotFound      = "attachment_not_found"
	ErrCodeAttachmentsUnavailable  = "attachments_unavailable"
	ErrCodeAttachmentNotReady      = "attachment_not_ready"
	ErrCodeAttachmentObjectMissing = "attachment_object_missing"
	ErrCodeAttachmentLimit         = "attachment_limit_exceeded"
	ErrCodeAttachmentMIMEBlocked   = "attachment_mime_blocked"
	ErrCodeAttachmentTooLarge      = "attachment_too_large"

	// Commit-on-close repo resolution codes (skein_close,
	// skein_close_many).
	//
	// repo_not_found  — the supplied repoUrl/repoId is not present in
	//                   the project's gitRepos[].
	// repo_ambiguous  — neither repoUrl nor repoId was passed and the
	//                   project does not have exactly one repo (zero
	//                   or multiple); the agent must disambiguate.
	ErrCodeRepoNotFound  = "repo_not_found"
	ErrCodeRepoAmbiguous = "repo_ambiguous"

	// Re-parent / parent-child classification codes (skein_reparent,
	// skein_dep_add, skein_dep_add_many, skein_create inline deps, and
	// the REST dependency / reparent handlers). FROZEN — they mirror the
	// dependencies-package sentinel strings 1:1.
	//
	// parent_already_set    — a parent-child edge was requested for an
	//                         issue that already has a (different) parent;
	//                         use skein_reparent to move it.
	// serialization_retry   — a dependency write lost a SERIALIZABLE race
	//                         (SQLSTATE 40001); no durable change, safe to
	//                         retry verbatim.
	// dependency_target_not_visible_yet — the to_id FK tripped because the
	//                         target row was not visible in this snapshot
	//                         (created in a still-uncommitted concurrent
	//                         tx); retry shortly.
	// reparent_self         — newParentId == id; an issue cannot be its
	//                         own parent.
	// reparent_cross_project — the new parent lives in a different project;
	//                         parent-child is an intra-project edge.
	// dependency_not_found  — skein_dep_remove targeted an edge that does
	//                         not exist.
	ErrCodeParentAlreadySet   = "parent_already_set"
	ErrCodeSerializationRetry = "serialization_retry"

	// ErrCodeRequestCancelled and ErrCodeBatchDeadlineExceeded mark the
	// rows of a batch that never ran, and a row whose work a context cut
	// while it was in progress (sk-607 for skein_close_many, sk-647.2 for
	// skein_dep_add_many and skein_update_many). They used to be
	// internal_error, indistinguishable from a row that failed while being
	// written — see batchStopReason for why the distinction is
	// load-bearing; contextStop for the in-progress case.
	ErrCodeRequestCancelled              = "request_cancelled"
	ErrCodeBatchDeadlineExceeded         = "batch_deadline_exceeded"
	ErrCodeDependencyTargetNotVisibleYet = "dependency_target_not_visible_yet"
	ErrCodeReparentSelf                  = "reparent_self"
	ErrCodeReparentCrossProject          = "reparent_cross_project"
	ErrCodeDependencyNotFound            = "dependency_not_found"

	// ErrCodeCloseOutcomeLinkRequired — sk-540.8: skein_dep_remove (and the
	// REST dependency DELETE) refused to remove the last `duplicates` edge
	// of an issue closed as "duplicate", or the last `supersedes` edge
	// pointing at an issue closed as "superseded". Nothing was written;
	// change the close outcome or reopen that issue first. Mirrors
	// dependencies.ErrCloseOutcomeLinkRequired.
	ErrCodeCloseOutcomeLinkRequired = "close_outcome_link_required"

	// sk-62.2 — reminder codes.
	//
	// reminder_not_found        — reminder id missing, or owned by another
	//                             user (owner scope folds not-yours → not
	//                             found, same as the REST 404).
	// reminder_target_ambiguous — snooze/dismiss was given an issueId (not
	//                             a reminderId) but the caller has more than
	//                             one active reminder on that issue; the
	//                             caller must re-issue with an explicit
	//                             reminderId. The candidate list rides in
	//                             the error's `reminders` field.
	ErrCodeReminderNotFound        = "reminder_not_found"
	ErrCodeReminderTargetAmbiguous = "reminder_target_ambiguous"

	// sk-285.7.5 — skein_theme(action="set_variables") was handed a
	// variable name the current role dictionary does not declare. The
	// whole patch is refused and NOTHING is written: such a name paints
	// nothing, and while it sits in the row it re-arms the frontend's
	// one-time migration write-back, which then replaces the map and
	// deletes it — a durable write the agent silently loses.
	//
	// The error carries `roles`: one entry per offending key with its
	// status ("retired" — a pre-redesign name, usually with a
	// `successor` to use instead — or "unknown"). Re-send under a live
	// role name; skein_theme(action="describe") lists them all.
	//
	// preview_variables does NOT return this code. A preview is an
	// ephemeral overlay of the agent's own map, so it applies as sent
	// and reports the same `roles` array as a non-fatal `warning`
	// (sk-285.7.4).
	ErrCodeUnknownThemeRole = "unknown_theme_role"

	// sk-118 (F2c) — standing-status transition guards. Surfaced as the
	// top-level envelope `error` (not buried under validation_error) so
	// agents / the frontend branch on the specific cause:
	//
	// cannot_promote_locked_to_standing — promote of an in_progress
	//     (claimed) issue to standing; unclaim first.
	// cannot_work_standing_directly     — standing → in_progress; you
	//     spawn a child (parent.N) and work that, never the standing task.
	// cannot_defer_standing             — standing → deferred; a perpetual
	//     obligation can't be parked. Demote to open first, then defer.
	// nested_standing_forbidden         — transition INTO standing while
	//     the issue already has a standing parent OR standing child (either
	//     representation). Also reused by the F2e/F2i create / reparent /
	//     dep-add guards.
	ErrCodeCannotPromoteLockedToStanding = "cannot_promote_locked_to_standing"
	ErrCodeCannotWorkStandingDirectly    = "cannot_work_standing_directly"
	ErrCodeCannotDeferStanding           = "cannot_defer_standing"
	ErrCodeNestedStandingForbidden       = "nested_standing_forbidden"

	// sk-118 (F2f) — skein_dep_add / skein_dep_add_many refuse to create a
	// blocks/waits-for edge whose TO-side (target) issue is standing. A standing
	// task is never "done", so the edge would gate the FROM (blocked) issue's
	// readiness forever. Surfaced as the top-level envelope `error` on dep_add
	// (and row-level in dep_add_many) — NOT buried under validation_error — so
	// agents branch on the specific cause. related / discovered-from / etc.
	// targeting standing remain allowed.
	ErrCodeBlockerTargetStanding = "blocker_target_standing"

	// sk-118 (F2d) — skein_claim refuses to claim a standing issue. Standing
	// tasks are never worked directly; spawn a child subtask (skein_create
	// parentId=<this>) and claim that. Surfaced as the top-level envelope
	// `error` (not buried under validation_error) so agents branch on the
	// specific cause and follow the directive. Symmetric in spirit to
	// cannot_claim_closed, but standing is a perpetual state, not terminal.
	ErrCodeCannotClaimStanding = "cannot_claim_standing"

	// sk-118 (F2e) — skein_close refuses to RETIRE (close) a standing issue
	// while it still has at least one non-closed child (open / in_progress /
	// deferred), in either hierarchy representation. Surfaced as the top-level
	// envelope `error` (not buried under validation_error) with the offending
	// child ids in `childIds` so the caller closes/retires the children first.
	// The block is standing-specific (normal epics stay closeable) and is NOT
	// bypassed by force — it is a structural invariant (§11.4 / §11.9).
	ErrCodeStandingHasOpenChildren = "standing_has_open_children"

	// sk-142.7 — skein_get_monitor_script requires an api-key caller: the
	// minted monitor credential is tied to the minting api key (cascade
	// revoke), so a context without a resolved api_keys.id cannot mint one.
	// In production RequireAPIKey on /mcp guarantees it's present; this code
	// surfaces the (cookie-session / misconfigured) edge rather than panicking
	// on a nil key id.
	ErrCodeAPIKeyRequired = "api_key_required"

	// sk-242.1 (FR-003) — skein_comment_get read path. Missing comment id
	// OR comment whose project is invisible to the caller both map to this
	// code (anti-oracle: no existence leak). Write-path tools keep their
	// existing issue_not_found / comment-scoped mapping until an optional
	// align lands later.
	ErrCodeCommentNotFound = "comment_not_found"

	// ErrCodeReviewForbidden — sk-303.14 (contract C1): the caller is not
	// the issue author and therefore may neither flip the "review
	// required" flag nor set the viewed mark (INV-4).
	//
	// A dedicated code, not `forbidden`: `forbidden` in this package means
	// "not enough role", and an administrator seeing it on the flag would
	// reasonably conclude it is a permissions problem. It is not —
	// authorship is not granted by role, and the refusal to an
	// administrator is the same as to any non-author.
	//
	// One code for both operations on purpose: the right to the flag and
	// the right to the mark are the same right, and two codes would
	// eventually drift apart.
	ErrCodeReviewForbidden = "review_forbidden"

	// ErrCodeMonitorNotFound — sk-579 (US-6, INV-11): skein_watchers
	// action="monitor_stop" was given a monitorId that is not a monitor of
	// the calling user. Someone else's monitor and a monitor that does not
	// exist (never minted, or already purged by cleanup) get this same code,
	// so the refusal never reveals that another user's monitor exists.
	ErrCodeMonitorNotFound = "monitor_not_found"
)

// ErrCodeLockRequiredSessionID is returned when a tool requires Mcp-Session-Id
// to be present in ctx (claim/unclaim/close all need a real session id).
// We keep it as a separate constant — it's a precondition error, not a
// conflict — but it is intentionally NOT in the closed-set comment block
// above because R2.3 spec calls out twenty business codes and this one
// is tier-2 (transport-level misconfiguration).
const ErrCodeLockRequiredSessionID = "lock_required_session_id"

// ErrCodePreviewUnavailable surfaces "NATS_URL not configured" to the
// agent (skein_theme(action="preview_variables")) so an operator notices
// the wiring
// gap. Stable string code, scoped to theme tools.
//
// It lived next to the tool that emits it until sk-256: the `code` label of
// skein_mcp_tool_errors_total is filtered through knownErrCodes, and a code
// declared away from this file was invisible both to that allowlist and to
// the guard test that keeps the two in sync, so a documented business code
// was being recorded as code="other". Declaration site is now uniform —
// every ErrCode* constant lives in errors.go, and that is what the allowlist
// mirrors. (The guard no longer *depends* on the uniformity — it scans the
// whole package — but a single declaration site is what makes "the closed
// set" a readable claim rather than a grep.)
const ErrCodePreviewUnavailable = "preview_unavailable"
