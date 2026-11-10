// Package apitest holds reference data for the tests of the public API. It is
// imported only by tests.
//
// Codes is the literal reference of what the public API answers for each
// error code: the HTTP status for each origin of the failure the code may
// come from, and whether the answer is retryable. It is written from the
// spec's table "Response codes of the public API" and from the plan's D-11
// (the status classes; transient = the outcome may change without any action
// of the client: a race, the visibility of a concurrent write, another
// holder's claim, an unavailable dependency, a batch row stopped by a
// deadline or a cancellation before it ran). It is NOT derived from
// internal/errcode: the tests compare errcode and the problem writer against
// it, so a code whose class or transience changes there turns them red.
//
// Two consumers read it: the status mapping test of the problem writer, and
// the Idempotency test of what a replay keeps (a retryable code is never
// stored).
package apitest

// Origin is where a failure comes from, by name: "path" (the object the path
// names), "body" (a reference in the request body or query), "parse" (the
// layer that parses the request), "domain" (the domain operation).
type Origin string

// The origins.
const (
	Path   Origin = "path"
	Body   Origin = "body"
	Parse  Origin = "parse"
	Domain Origin = "domain"
)

// Origins are all the origins.
var Origins = []Origin{Path, Body, Parse, Domain}

// Expect is the reference answer for one code.
type Expect struct {
	// Status is the HTTP status for each origin the code may come from.
	Status map[Origin]int
	// Retryable — the problem carries retryable: true and an Idempotency
	// replay does not keep the answer.
	Retryable bool
}

// notFound — an object that does not exist or is outside the key's reach:
// 404 when the path names it, 422 with the same code when the body
// references it.
func notFound() Expect {
	return Expect{Status: map[Origin]int{Path: 404, Body: 422}}
}

// format — a malformed request: 400 from the parsing layer, 422 from the
// domain.
func format() Expect {
	return Expect{Status: map[Origin]int{Parse: 400, Domain: 422}}
}

// every — the same status whatever the origin.
func every(status int, retryable bool) Expect {
	return Expect{Status: map[Origin]int{Path: status, Body: status, Parse: status, Domain: status}, Retryable: retryable}
}

// Codes is the reference, keyed by the wire name of the code.
var Codes = map[string]Expect{
	// Not found or outside the key's reach.
	"project_not_found":    notFound(),
	"issue_not_found":      notFound(),
	"parent_not_found":     notFound(),
	"user_not_found":       notFound(),
	"member_not_found":     notFound(),
	"space_not_found":      notFound(),
	"attachment_not_found": notFound(),
	"repo_not_found":       notFound(),
	"dependency_not_found": notFound(),
	"reminder_not_found":   notFound(),
	"comment_not_found":    notFound(),
	"monitor_not_found":    notFound(),
	// No operation has the path: only ever the object of the path.
	"route_not_found": {Status: map[Origin]int{Path: 404}},

	// Malformed request.
	"validation_error":         format(),
	"lock_required_session_id": format(),

	// No valid identity.
	"unauthenticated":  every(401, false),
	"api_key_required": every(401, false),

	// The key's right or the owner's role.
	"insufficient_scope":             every(403, false),
	"forbidden":                      every(403, false),
	"not_admin":                      every(403, false),
	"not_space_member":               every(403, false),
	"external_cannot_create_project": every(403, false),
	"force_unlock_forbidden":         every(403, false),
	"invitation_agent_forbidden":     every(403, false),
	"review_forbidden":               every(403, false),

	// Stale If-Match.
	"version_conflict": every(412, false),

	// No If-Match where the operation requires it.
	"precondition_required": every(428, false),

	// Idempotency-Key: the first request still runs (409, the repeat may
	// succeed later), the key reused with another body (422), the key sent
	// to an operation that does not accept it (400).
	"idempotency_in_progress":   every(409, true),
	"idempotency_key_reused":    every(422, false),
	"idempotency_not_supported": format(),

	// The duplicate gate of createIssue: nothing created (422).
	"duplicate_suspected": every(422, false),

	// Transport of the public API.
	"method_not_allowed":     every(405, false),
	"payload_too_large":      every(413, false),
	"unsupported_media_type": every(415, false),

	// Unavailable dependency.
	"attachments_unavailable": every(503, true),
	"preview_unavailable":     every(503, true),
	"database_unavailable":    every(503, true),

	// Rate limit or failed-authentication limit; the window ends without the
	// client.
	"rate_limited": every(429, true),

	// Internal failure.
	"internal_error": every(500, false),

	// Business refusals whose outcome may change without the client.
	"already_claimed":                   every(422, true),
	"locked_by_other":                   every(422, true),
	"concurrent_change":                 every(422, true),
	"serialization_retry":               every(422, true),
	"dependency_target_not_visible_yet": every(422, true),
	"request_cancelled":                 every(422, true),
	"batch_deadline_exceeded":           every(422, true),

	// Business refusals.
	"dependency_cycle":                  every(422, false),
	"dependency_already_exists":         every(422, false),
	"parent_closed":                     every(422, false),
	"parent_wrong_project":              every(422, false),
	"id_taken":                          every(422, false),
	"components_wrong_project":          every(422, false),
	"pinned_no_force":                   every(422, false),
	"ambiguous_project":                 every(422, false),
	"not_lock_holder":                   every(422, false),
	"cannot_claim_closed":               every(422, false),
	"cannot_accept_closed":              every(422, false),
	"email_taken":                       every(422, false),
	"username_taken":                    every(422, false),
	"last_admin_cannot_demote":          every(422, false),
	"member_via_space":                  every(422, false),
	"personal_space":                    every(422, false),
	"external_user":                     every(422, false),
	"already_member":                    every(422, false),
	"no_seats":                          every(422, false),
	"space_required":                    every(422, false),
	"space_change_forbidden":            every(422, false),
	"assignee_not_allowed":              every(422, false),
	"prefix_taken":                      every(422, false),
	"prefix_invalid":                    every(422, false),
	"project_name_taken":                every(422, false),
	"project_quota":                     every(422, false),
	"storage_quota":                     every(422, false),
	"admin_role_disabled":               every(422, false),
	"attachment_not_ready":              every(422, false),
	"attachment_object_missing":         every(422, false),
	"attachment_limit_exceeded":         every(422, false),
	"attachment_mime_blocked":           every(422, false),
	"attachment_too_large":              every(422, false),
	"repo_ambiguous":                    every(422, false),
	"parent_already_set":                every(422, false),
	"reparent_self":                     every(422, false),
	"reparent_cross_project":            every(422, false),
	"close_outcome_link_required":       every(422, false),
	"reminder_target_ambiguous":         every(422, false),
	"unknown_theme_role":                every(422, false),
	"cannot_promote_locked_to_standing": every(422, false),
	"cannot_work_standing_directly":     every(422, false),
	"cannot_defer_standing":             every(422, false),
	"nested_standing_forbidden":         every(422, false),
	"blocker_target_standing":           every(422, false),
	"cannot_claim_standing":             every(422, false),
	"standing_has_open_children":        every(422, false),
}
