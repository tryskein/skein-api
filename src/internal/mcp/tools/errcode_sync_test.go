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
