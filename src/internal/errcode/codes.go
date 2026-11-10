package errcode

// The closed set. Each code names its class and its transience; the
// comment next to a code is what an agent or an API client reads it as.
//
// Transient codes are exactly those whose outcome depends on state that
// changes without the client: a lost serialization race, a concurrent write
// not yet visible, a claim held by another holder, an unavailable dependency,
// a batch row cut by a deadline before it ran.
var (
	// Objects that do not exist or are outside the caller's reach.
	ProjectNotFound    = define("project_not_found", ClassNotFound, Permanent)
	IssueNotFound      = define("issue_not_found", ClassNotFound, Permanent)
	ParentNotFound     = define("parent_not_found", ClassNotFound, Permanent)
	UserNotFound       = define("user_not_found", ClassNotFound, Permanent)
	MemberNotFound     = define("member_not_found", ClassNotFound, Permanent)
	SpaceNotFound      = define("space_not_found", ClassNotFound, Permanent)
	AttachmentNotFound = define("attachment_not_found", ClassNotFound, Permanent)
	RepoNotFound       = define("repo_not_found", ClassNotFound, Permanent)
	DependencyNotFound = define("dependency_not_found", ClassNotFound, Permanent)
	ReminderNotFound   = define("reminder_not_found", ClassNotFound, Permanent)
	CommentNotFound    = define("comment_not_found", ClassNotFound, Permanent)
	MonitorNotFound    = define("monitor_not_found", ClassNotFound, Permanent)
	// RouteNotFound — no operation of the public API has this path.
	RouteNotFound = define("route_not_found", ClassNotFound, Permanent)

	// Malformed requests.
	ValidationError       = define("validation_error", ClassFormat, Permanent)
	LockRequiredSessionID = define("lock_required_session_id", ClassFormat, Permanent)

	// Identity.
	Unauthenticated = define("unauthenticated", ClassUnauthenticated, Permanent)
	APIKeyRequired  = define("api_key_required", ClassUnauthenticated, Permanent)

	// Rights of the key and roles of its owner.
	//
	// InsufficientScope — the key does not hold the right the operation
	// requires (read, write, manage, admin); checked before any object is
	// looked up, so it never reveals whether the object exists.
	InsufficientScope           = define("insufficient_scope", ClassPermission, Permanent)
	Forbidden                   = define("forbidden", ClassPermission, Permanent)
	NotAdmin                    = define("not_admin", ClassPermission, Permanent)
	NotSpaceMember              = define("not_space_member", ClassPermission, Permanent)
	ExternalCannotCreateProject = define("external_cannot_create_project", ClassPermission, Permanent)
	ForceUnlockForbidden        = define("force_unlock_forbidden", ClassPermission, Permanent)
	InvitationAgentForbidden    = define("invitation_agent_forbidden", ClassPermission, Permanent)
	ReviewForbidden             = define("review_forbidden", ClassPermission, Permanent)

	// Transport refusals of the public API: the request does not reach any
	// operation.
	//
	// MethodNotAllowed — the path exists, the method does not (the Allow
	// header lists the methods it has).
	MethodNotAllowed = define("method_not_allowed", ClassMethod, Permanent)
	// PayloadTooLarge — the request body is larger than 1 MiB.
	PayloadTooLarge = define("payload_too_large", ClassPayloadTooLarge, Permanent)
	// UnsupportedMediaType — the request body is not application/json.
	UnsupportedMediaType = define("unsupported_media_type", ClassMediaType, Permanent)

	// Optimistic concurrency.
	VersionConflict = define("version_conflict", ClassVersion, Permanent)
	// PreconditionRequired — the operation checks the version of its
	// resource and the request carries no If-Match.
	PreconditionRequired = define("precondition_required", ClassPreconditionRequired, Permanent)

	// Idempotency-Key of the public API.
	//
	// IdempotencyInProgress — the first request with this Idempotency-Key is
	// still being executed; the repeat may be sent again later.
	IdempotencyInProgress = define("idempotency_in_progress", ClassConflict, Transient)
	// IdempotencyKeyReused — the Idempotency-Key was used for the same
	// operation on the same target with a different request body.
	IdempotencyKeyReused = define("idempotency_key_reused", ClassBusiness, Permanent)
	// IdempotencyNotSupported — the operation does not accept
	// Idempotency-Key (only createIssue, createEpic, createComment and
	// closeIssue do; batches never).
	IdempotencyNotSupported = define("idempotency_not_supported", ClassFormat, Permanent)

	// Unavailable dependencies.
	AttachmentsUnavailable = define("attachments_unavailable", ClassUnavailable, Transient)
	PreviewUnavailable     = define("preview_unavailable", ClassUnavailable, Transient)
	// DatabaseUnavailable — the database could not be reached (a closed
	// pool, a refused or broken connection).
	DatabaseUnavailable = define("database_unavailable", ClassUnavailable, Transient)

	// RateLimited — the key made more requests in the current minute than
	// the public API allows, or the client address failed authentication
	// too often; Retry-After says when the window ends.
	RateLimited = define("rate_limited", ClassRateLimited, Transient)

	// Failures inside the service.
	InternalError = define("internal_error", ClassInternal, Permanent)

	// Domain refusals that may resolve without the client.
	AlreadyClaimed                = define("already_claimed", ClassBusiness, Transient)
	LockedByOther                 = define("locked_by_other", ClassBusiness, Transient)
	ConcurrentChange              = define("concurrent_change", ClassBusiness, Transient)
	SerializationRetry            = define("serialization_retry", ClassBusiness, Transient)
	DependencyTargetNotVisibleYet = define("dependency_target_not_visible_yet", ClassBusiness, Transient)
	RequestCancelled              = define("request_cancelled", ClassBusiness, Transient)
	BatchDeadlineExceeded         = define("batch_deadline_exceeded", ClassBusiness, Transient)

	// Domain refusals.
	DependencyCycle               = define("dependency_cycle", ClassBusiness, Permanent)
	DependencyAlreadyExists       = define("dependency_already_exists", ClassBusiness, Permanent)
	ParentClosed                  = define("parent_closed", ClassBusiness, Permanent)
	ParentWrongProject            = define("parent_wrong_project", ClassBusiness, Permanent)
	IDTaken                       = define("id_taken", ClassBusiness, Permanent)
	ComponentsWrongProject        = define("components_wrong_project", ClassBusiness, Permanent)
	PinnedNoForce                 = define("pinned_no_force", ClassBusiness, Permanent)
	AmbiguousProject              = define("ambiguous_project", ClassBusiness, Permanent)
	NotLockHolder                 = define("not_lock_holder", ClassBusiness, Permanent)
	CannotClaimClosed             = define("cannot_claim_closed", ClassBusiness, Permanent)
	CannotAcceptClosed            = define("cannot_accept_closed", ClassBusiness, Permanent)
	EmailTaken                    = define("email_taken", ClassBusiness, Permanent)
	UsernameTaken                 = define("username_taken", ClassBusiness, Permanent)
	LastAdminCannotDemote         = define("last_admin_cannot_demote", ClassBusiness, Permanent)
	MemberViaSpace                = define("member_via_space", ClassBusiness, Permanent)
	PersonalSpace                 = define("personal_space", ClassBusiness, Permanent)
	ExternalUser                  = define("external_user", ClassBusiness, Permanent)
	AlreadyMember                 = define("already_member", ClassBusiness, Permanent)
	NoSeats                       = define("no_seats", ClassBusiness, Permanent)
	SpaceRequired                 = define("space_required", ClassBusiness, Permanent)
	SpaceChangeForbidden          = define("space_change_forbidden", ClassBusiness, Permanent)
	AssigneeNotAllowed            = define("assignee_not_allowed", ClassBusiness, Permanent)
	PrefixTaken                   = define("prefix_taken", ClassBusiness, Permanent)
	PrefixInvalid                 = define("prefix_invalid", ClassBusiness, Permanent)
	ProjectNameTaken              = define("project_name_taken", ClassBusiness, Permanent)
	ProjectQuota                  = define("project_quota", ClassBusiness, Permanent)
	StorageQuota                  = define("storage_quota", ClassBusiness, Permanent)
	AdminRoleDisabled             = define("admin_role_disabled", ClassBusiness, Permanent)
	AttachmentNotReady            = define("attachment_not_ready", ClassBusiness, Permanent)
	AttachmentObjectMissing       = define("attachment_object_missing", ClassBusiness, Permanent)
	AttachmentLimitExceeded       = define("attachment_limit_exceeded", ClassBusiness, Permanent)
	AttachmentMIMEBlocked         = define("attachment_mime_blocked", ClassBusiness, Permanent)
	AttachmentTooLarge            = define("attachment_too_large", ClassBusiness, Permanent)
	RepoAmbiguous                 = define("repo_ambiguous", ClassBusiness, Permanent)
	ParentAlreadySet              = define("parent_already_set", ClassBusiness, Permanent)
	ReparentSelf                  = define("reparent_self", ClassBusiness, Permanent)
	ReparentCrossProject          = define("reparent_cross_project", ClassBusiness, Permanent)
	CloseOutcomeLinkRequired      = define("close_outcome_link_required", ClassBusiness, Permanent)
	ReminderTargetAmbiguous       = define("reminder_target_ambiguous", ClassBusiness, Permanent)
	UnknownThemeRole              = define("unknown_theme_role", ClassBusiness, Permanent)
	CannotPromoteLockedToStanding = define("cannot_promote_locked_to_standing", ClassBusiness, Permanent)
	CannotWorkStandingDirectly    = define("cannot_work_standing_directly", ClassBusiness, Permanent)
	CannotDeferStanding           = define("cannot_defer_standing", ClassBusiness, Permanent)
	NestedStandingForbidden       = define("nested_standing_forbidden", ClassBusiness, Permanent)
	BlockerTargetStanding         = define("blocker_target_standing", ClassBusiness, Permanent)
	CannotClaimStanding           = define("cannot_claim_standing", ClassBusiness, Permanent)
	StandingHasOpenChildren       = define("standing_has_open_children", ClassBusiness, Permanent)
)
