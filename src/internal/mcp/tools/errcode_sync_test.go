package tools

import (
	"testing"

	"skein-api/internal/errcode"
)

// TestErrCodes_MatchErrcode keeps the literal codes of this package and the
// closed set of internal/errcode equal. The ErrCode* constants stay string
// literals (the allowlist guard in metrics_test.go reads them as such);
// errcode is the one source of a code's status class and transience for the
// public API, which may define codes MCP never answers with. A code
// renamed in one place, or added to tools without errcode, turns this red.
func TestErrCodes_MatchErrcode(t *testing.T) {
	pairs := []struct {
		tools string
		code  errcode.Code
	}{
		{ErrCodeProjectNotFound, errcode.ProjectNotFound},
		{ErrCodeIssueNotFound, errcode.IssueNotFound},
		{ErrCodeVersionConflict, errcode.VersionConflict},
		{ErrCodeValidationError, errcode.ValidationError},
		{ErrCodeForbidden, errcode.Forbidden},
		{ErrCodeUnauthenticated, errcode.Unauthenticated},
		{ErrCodeInternalError, errcode.InternalError},
		{ErrCodeDependencyCycle, errcode.DependencyCycle},
		{ErrCodeDependencyAlreadyExists, errcode.DependencyAlreadyExists},
		{ErrCodeParentClosed, errcode.ParentClosed},
		{ErrCodeParentNotFound, errcode.ParentNotFound},
		{ErrCodeParentWrongProject, errcode.ParentWrongProject},
		{ErrCodeIDTaken, errcode.IDTaken},
		{ErrCodeComponentsWrongProject, errcode.ComponentsWrongProject},
		{ErrCodePinnedNoForce, errcode.PinnedNoForce},
		{ErrCodeAmbiguousProject, errcode.AmbiguousProject},
		{ErrCodeAlreadyClaimed, errcode.AlreadyClaimed},
		{ErrCodeLockedByOther, errcode.LockedByOther},
		{ErrCodeNotLockHolder, errcode.NotLockHolder},
		{ErrCodeCannotClaimClosed, errcode.CannotClaimClosed},
		{ErrCodeCannotAcceptClosed, errcode.CannotAcceptClosed},
		{ErrCodeNotAdmin, errcode.NotAdmin},
		{ErrCodeUserNotFound, errcode.UserNotFound},
		{ErrCodeEmailTaken, errcode.EmailTaken},
		{ErrCodeUsernameTaken, errcode.UsernameTaken},
		{ErrCodeMemberNotFound, errcode.MemberNotFound},
		{ErrCodeLastAdminCannotDemote, errcode.LastAdminCannotDemote},
		{ErrCodeMemberViaSpace, errcode.MemberViaSpace},
		{ErrCodeConcurrentChange, errcode.ConcurrentChange},
		{ErrCodeSpaceNotFound, errcode.SpaceNotFound},
		{ErrCodePersonalSpace, errcode.PersonalSpace},
		{ErrCodeExternalUser, errcode.ExternalUser},
		{ErrCodeAlreadyMember, errcode.AlreadyMember},
		{ErrCodeNoSeats, errcode.NoSeats},
		{ErrCodeNotSpaceMember, errcode.NotSpaceMember},
		{ErrCodeSpaceRequired, errcode.SpaceRequired},
		{ErrCodeSpaceChangeForbidden, errcode.SpaceChangeForbidden},
		{ErrCodeExternalCannotCreateProject, errcode.ExternalCannotCreateProject},
		{ErrCodeAssigneeNotAllowed, errcode.AssigneeNotAllowed},
		{ErrCodeForceUnlockForbidden, errcode.ForceUnlockForbidden},
		{ErrCodeInvitationAgentForbidden, errcode.InvitationAgentForbidden},
		{ErrCodePrefixTaken, errcode.PrefixTaken},
		{ErrCodePrefixInvalid, errcode.PrefixInvalid},
		{ErrCodeProjectNameTaken, errcode.ProjectNameTaken},
		{ErrCodeProjectQuota, errcode.ProjectQuota},
		{ErrCodeStorageQuota, errcode.StorageQuota},
		{ErrCodeAdminRoleDisabled, errcode.AdminRoleDisabled},
		{ErrCodeAttachmentNotFound, errcode.AttachmentNotFound},
		{ErrCodeAttachmentsUnavailable, errcode.AttachmentsUnavailable},
		{ErrCodeAttachmentNotReady, errcode.AttachmentNotReady},
		{ErrCodeAttachmentObjectMissing, errcode.AttachmentObjectMissing},
		{ErrCodeAttachmentLimit, errcode.AttachmentLimitExceeded},
		{ErrCodeAttachmentMIMEBlocked, errcode.AttachmentMIMEBlocked},
		{ErrCodeAttachmentTooLarge, errcode.AttachmentTooLarge},
		{ErrCodeRepoNotFound, errcode.RepoNotFound},
		{ErrCodeRepoAmbiguous, errcode.RepoAmbiguous},
		{ErrCodeParentAlreadySet, errcode.ParentAlreadySet},
		{ErrCodeSerializationRetry, errcode.SerializationRetry},
		{ErrCodeRequestCancelled, errcode.RequestCancelled},
		{ErrCodeBatchDeadlineExceeded, errcode.BatchDeadlineExceeded},
		{ErrCodeDependencyTargetNotVisibleYet, errcode.DependencyTargetNotVisibleYet},
		{ErrCodeReparentSelf, errcode.ReparentSelf},
		{ErrCodeReparentCrossProject, errcode.ReparentCrossProject},
		{ErrCodeDependencyNotFound, errcode.DependencyNotFound},
		{ErrCodeCloseOutcomeLinkRequired, errcode.CloseOutcomeLinkRequired},
		{ErrCodeReminderNotFound, errcode.ReminderNotFound},
		{ErrCodeReminderTargetAmbiguous, errcode.ReminderTargetAmbiguous},
		{ErrCodeUnknownThemeRole, errcode.UnknownThemeRole},
		{ErrCodeCannotPromoteLockedToStanding, errcode.CannotPromoteLockedToStanding},
		{ErrCodeCannotWorkStandingDirectly, errcode.CannotWorkStandingDirectly},
		{ErrCodeCannotDeferStanding, errcode.CannotDeferStanding},
		{ErrCodeNestedStandingForbidden, errcode.NestedStandingForbidden},
		{ErrCodeBlockerTargetStanding, errcode.BlockerTargetStanding},
		{ErrCodeCannotClaimStanding, errcode.CannotClaimStanding},
		{ErrCodeStandingHasOpenChildren, errcode.StandingHasOpenChildren},
		{ErrCodeAPIKeyRequired, errcode.APIKeyRequired},
		{ErrCodeCommentNotFound, errcode.CommentNotFound},
		{ErrCodeReviewForbidden, errcode.ReviewForbidden},
		{ErrCodeMonitorNotFound, errcode.MonitorNotFound},
		{ErrCodeInsufficientScope, errcode.InsufficientScope},
		{ErrCodeLockRequiredSessionID, errcode.LockRequiredSessionID},
		{ErrCodePreviewUnavailable, errcode.PreviewUnavailable},
	}
	if len(pairs) == 0 {
		t.Fatal("no pairs to compare")
	}
	for _, p := range pairs {
		if p.tools != p.code.String() {
			t.Errorf("tools code %q differs from errcode %q", p.tools, p.code.String())
		}
		if _, ok := knownErrCodes[p.tools]; !ok {
			t.Errorf("tools code %q is not in knownErrCodes", p.tools)
		}
	}
	// knownErrCodes mirrors every ErrCode constant of the package (its own
	// guard checks that), so equal sizes mean every constant is paired here.
	if len(pairs) != len(knownErrCodes) {
		t.Errorf("%d pairs for %d codes in knownErrCodes: an ErrCode constant has no errcode pair",
			len(pairs), len(knownErrCodes))
	}
	// Every code MCP answers with is a code of the closed set.
	for code := range knownErrCodes {
		if _, ok := errcode.Lookup(code); !ok {
			t.Errorf("knownErrCodes entry %q is not an errcode code", code)
		}
	}
}

// TestErrCodes_ClassAndTransience — review C4-05: the status class and the
// "transient" mark of every code, against a literal reference written from
// the spec ("Response codes of the public API": 401 identity, 403 key right
// or owner role, 404 not found or outside the scope, 400 format, 412 stale
// version, 500/503 internal and unavailable, 422 business) and the plan's
// D-11 (transient: the outcome changes without the client — a race, the
// visibility of a concurrent write, another holder's claim, an unavailable
// dependency; plus the batch rows a deadline or a cancellation stopped
// before they ran). A code whose class or mark differs, a code of errcode
// missing here, or a reference entry errcode lacks turns this red.
func TestErrCodes_ClassAndTransience(t *testing.T) {
	type ref struct {
		class     errcode.Class
		transient bool
	}
	nf := ref{errcode.ClassNotFound, false}
	biz := ref{errcode.ClassBusiness, false}
	bizT := ref{errcode.ClassBusiness, true}
	perm := ref{errcode.ClassPermission, false}
	want := map[string]ref{
		// Not found or outside the reach (404 by path, 422 in a body).
		"project_not_found": nf, "issue_not_found": nf, "parent_not_found": nf,
		"user_not_found": nf, "member_not_found": nf, "space_not_found": nf,
		"attachment_not_found": nf, "repo_not_found": nf, "dependency_not_found": nf,
		"reminder_not_found": nf, "comment_not_found": nf, "monitor_not_found": nf,
		// Format (400 at the parsing layer).
		"validation_error":         {errcode.ClassFormat, false},
		"lock_required_session_id": {errcode.ClassFormat, false},
		// Identity (401).
		"unauthenticated":  {errcode.ClassUnauthenticated, false},
		"api_key_required": {errcode.ClassUnauthenticated, false},
		// Key right or owner role (403).
		"insufficient_scope": perm, "forbidden": perm, "not_admin": perm,
		"not_space_member": perm, "external_cannot_create_project": perm,
		"force_unlock_forbidden": perm, "invitation_agent_forbidden": perm,
		"review_forbidden": perm,
		// Stale version (412).
		"version_conflict": {errcode.ClassVersion, false},
		// Unavailable dependency (503), transient.
		"attachments_unavailable": {errcode.ClassUnavailable, true},
		"preview_unavailable":     {errcode.ClassUnavailable, true},
		// Internal (500).
		"internal_error": {errcode.ClassInternal, false},
		// Business (422), transient.
		"already_claimed": bizT, "locked_by_other": bizT, "concurrent_change": bizT,
		"serialization_retry": bizT, "dependency_target_not_visible_yet": bizT,
		"request_cancelled": bizT, "batch_deadline_exceeded": bizT,
		// Business (422).
		"dependency_cycle": biz, "dependency_already_exists": biz, "parent_closed": biz,
		"parent_wrong_project": biz, "id_taken": biz, "components_wrong_project": biz,
		"pinned_no_force": biz, "ambiguous_project": biz, "not_lock_holder": biz,
		"cannot_claim_closed": biz, "cannot_accept_closed": biz, "email_taken": biz,
		"username_taken": biz, "last_admin_cannot_demote": biz, "member_via_space": biz,
		"personal_space": biz, "external_user": biz, "already_member": biz,
		"no_seats": biz, "space_required": biz, "space_change_forbidden": biz,
		"assignee_not_allowed": biz, "prefix_taken": biz, "prefix_invalid": biz,
		"project_name_taken": biz, "project_quota": biz, "storage_quota": biz,
		"admin_role_disabled": biz, "attachment_not_ready": biz,
		"attachment_object_missing": biz, "attachment_limit_exceeded": biz,
		"attachment_mime_blocked": biz, "attachment_too_large": biz,
		"repo_ambiguous": biz, "parent_already_set": biz, "reparent_self": biz,
		"reparent_cross_project": biz, "close_outcome_link_required": biz,
		"reminder_target_ambiguous": biz, "unknown_theme_role": biz,
		"cannot_promote_locked_to_standing": biz, "cannot_work_standing_directly": biz,
		"cannot_defer_standing": biz, "nested_standing_forbidden": biz,
		"blocker_target_standing": biz, "cannot_claim_standing": biz,
		"standing_has_open_children": biz,
	}
	all := errcode.All()
	if len(all) == 0 {
		t.Fatal("errcode defines no codes")
	}
	for _, c := range all {
		r, ok := want[c.String()]
		if !ok {
			t.Errorf("errcode %q has no reference class and transience here", c.String())
			continue
		}
		if c.Class() != r.class {
			t.Errorf("%s: class %s, reference %s", c.String(), c.Class(), r.class)
		}
		if c.Transient() != r.transient {
			t.Errorf("%s: transient %v, reference %v", c.String(), c.Transient(), r.transient)
		}
	}
	for name := range want {
		if _, ok := errcode.Lookup(name); !ok {
			t.Errorf("reference code %q is not defined in errcode", name)
		}
	}
}
