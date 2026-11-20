package push

import "time"

// Push REST contract v2: the wire shapes, refusal codes, modes, limits, the
// contract marker and the routes of the /ui/v1/push group (reached by the
// browser as /api/v1/push through the BFF).
//
// # Contract marker and the layer boundary
//
// The middleware of the /push group of a v2 api sets the response header
// ContractHeader: ContractVersion on every response that passed through the
// group (2xx, 400, 422). Responses produced before the group — 401
// session_revoked from RequireSessionEpoch, a 401 of the BFF — do not carry
// it. The page classifies a response in this order: status 401 first →
// session ended, whatever the header; any other status without the header →
// lost (including a 200 list and a 204 of an older api); with the header —
// by the outcome table below. GET /push/config keeps its body unchanged.
//
// # New forms are refused by an older binary before any write
//
// An older api decodes request bodies with DisallowUnknownFields, so the new
// body of POST and DELETE /push/subscriptions is refused with 400 there. The
// routes RouteDeviceRevocations and RouteDeviceRemovals do not exist on an
// older api (404/405). GET /push/subscriptions?expectedUserId= is answered by
// an older api with 200 without the marker, so the page treats it as lost
// and nothing changes on the server. The page never calls the old
// DELETE /push/subscriptions/{id}.
//
// # Errors
//
// Business refusals are 422 {"error":"<code>","message":"…"}; neither the
// response nor the message carries the endpoint, the keys or the owner of a
// subscription. Codes: the Code* constants. Syntax errors are 400
// CodeInvalidRequest (a value that is not a UUID, an unknown mode, deviceLabel
// without expectedUserId, an empty revocation, more than MaxDeviceLabels
// labels); malformed JSON is 400 CodeInvalidJSON. expectedUserId and
// deviceLabel are UUIDs; the browser creates the label (crypto.randomUUID()).
//
// # Operations
//
//	POST /push/subscriptions
//	  new form: SubscribeRequest with expectedUserId, deviceLabel, mode
//	    200 SubscriptionDTO; 422 expected_user_mismatch, session_stale,
//	    device_revoked, endpoint_taken, subscription_not_found (updateOnly
//	    without an own subscription), invalid_subscription
//	  old form: {endpoint, keys, ua?} only refreshes the keys of the caller's
//	    existing subscription (the label is not touched); none of the
//	    caller's → subscription_not_found; someone else's → endpoint_taken;
//	    session_stale
//	DELETE /push/subscriptions — removal of a channel held by another user
//	  new form: RemoveChannelRequest
//	    204; 422 expected_user_mismatch, session_stale, invalid_subscription
//	  old form: {endpoint} deletes only the caller's subscription with that
//	    endpoint, without revocation, 204; session_stale
//	POST /push/device-revocations — revocation of the caller's device
//	  RevokeDeviceRequest, at least one of deviceLabels and endpoint non-empty
//	    204 (also when there are no subscriptions); 422
//	    expected_user_mismatch, session_stale
//	POST /push/device-removals — removal of the caller's device by id
//	  RemoveDeviceRequest
//	    204; 422 subscription_not_found, expected_user_mismatch, session_stale
//	DELETE /push/subscriptions/{id} — old page only, no expected user
//	    204; 422 subscription_not_found (was 404), session_stale; deletes the
//	    caller's subscription and revokes its label when it has one
//	GET /push/subscriptions?expectedUserId=
//	    200 [SubscriptionDTO]; 422 expected_user_mismatch; without the query
//	    parameter — as before (plus the additive DTO fields)
//
// # Meaning of 204
//
// 204 is defined in the serial order of the SERIALIZABLE transactions (the
// point of logical deletion), not by the physical commit time. Removal of a
// foreign channel without ifSubscription: at that point no user has a
// subscription with this endpoint; a registration of the same endpoint
// ordered after the removal is a new record the removal does not touch. With
// ifSubscription: there is no subscription with this endpoint, id and version
// (deleted, never existed, or the record is newer and was left untouched).
// Revocation: the listed labels are revoked for the caller, the caller has no
// subscription with these labels and no unlabelled subscription with the
// presented endpoint. Removal by id: this subscription does not exist; if it
// had a label, the label is revoked. 204 is the same for every outcome and
// never discloses the owner.

// ContractHeader is the name of the response header that marks a response of
// the push REST contract v2. See the package-level contract description for
// which responses carry it.
const ContractHeader = "Skein-Push-Contract"

// ContractVersion is the value of ContractHeader on a v2 response.
const ContractVersion = "2"

// Routes of the /push group added by contract v2, relative to the group.
// Full paths: /ui/v1/push/device-revocations and /ui/v1/push/device-removals.
const (
	// RouteDeviceRevocations takes POST RevokeDeviceRequest.
	RouteDeviceRevocations = "/device-revocations"
	// RouteDeviceRemovals takes POST RemoveDeviceRequest.
	RouteDeviceRemovals = "/device-removals"
)

// Refusal codes of the push REST contract v2: the "error" field of a 422 or
// 400 response body.
const (
	// CodeInvalidSubscription — the endpoint or a key is missing, or the
	// endpoint is not a public https URL. 422.
	CodeInvalidSubscription = "invalid_subscription"
	// CodeExpectedUserMismatch — the expected user of the request is not the
	// caller; nothing was changed. 422.
	CodeExpectedUserMismatch = "expected_user_mismatch"
	// CodeEndpointTaken — the endpoint belongs to another user's
	// subscription; the owner is not disclosed. 422.
	CodeEndpointTaken = "endpoint_taken"
	// CodeDeviceRevoked — the device label is revoked for the caller. 422.
	CodeDeviceRevoked = "device_revoked"
	// CodeSubscriptionNotFound — the caller has no such subscription. 422.
	CodeSubscriptionNotFound = "subscription_not_found"
	// CodeSessionStale — at the moment of the write the caller is inactive or
	// the session epoch has changed; returned by every write. 422.
	CodeSessionStale = "session_stale"
	// CodeInvalidRequest — syntax: a value that is not a UUID, an unknown
	// mode, deviceLabel without expectedUserId, an empty revocation, more than
	// MaxDeviceLabels labels. 400.
	CodeInvalidRequest = "invalid_request"
	// CodeInvalidJSON — the body is not valid JSON or has unknown fields. 400.
	CodeInvalidJSON = "invalid_json"
)

// Modes of SubscribeRequest in the new form.
const (
	// ModeUpsert creates the caller's subscription for the endpoint or updates
	// the caller's existing one.
	ModeUpsert = "upsert"
	// ModeUpdateOnly updates only the caller's existing subscription for the
	// endpoint; without one the answer is 422 subscription_not_found.
	ModeUpdateOnly = "updateOnly"
)

// MaxDeviceLabels is the largest number of labels one RevokeDeviceRequest
// may carry; more is 400 invalid_request.
const MaxDeviceLabels = 1000

// SubscriptionKeys is the RFC 8291 encryption material of a browser push
// subscription, as PushSubscription.toJSON() returns it.
type SubscriptionKeys struct {
	// P256dh is the browser's ECDH public key, base64url.
	P256dh string `json:"p256dh"`
	// Auth is the authentication secret, base64url.
	Auth string `json:"auth"`
}

// SubscribeRequest is the body of POST /push/subscriptions. The new form
// carries ExpectedUserID, DeviceLabel and Mode; the old form carries only
// Endpoint, Keys and UA. DeviceLabel without ExpectedUserID is 400
// invalid_request.
type SubscribeRequest struct {
	// Endpoint is the push service URL of the browser subscription.
	Endpoint string `json:"endpoint"`
	// Keys is the encryption material of the subscription.
	Keys SubscriptionKeys `json:"keys"`
	// UA is an optional user-agent string shown in the device list.
	UA string `json:"ua"`
	// ExpectedUserID is the UUID of the user the page expects to be the
	// caller; another caller gets 422 expected_user_mismatch.
	ExpectedUserID string `json:"expectedUserId"`
	// DeviceLabel is the UUID label of the device consent, created by the
	// browser.
	DeviceLabel string `json:"deviceLabel"`
	// Mode is ModeUpsert or ModeUpdateOnly; any other value is 400
	// invalid_request.
	Mode string `json:"mode"`
}

// IfSubscription makes a channel removal conditional: only the subscription
// with this id and version is removed.
type IfSubscription struct {
	// ID is the subscription id.
	ID string `json:"id"`
	// Version is the subscription version seen by the page.
	Version int64 `json:"version"`
}

// RemoveChannelRequest is the new-form body of DELETE /push/subscriptions:
// removal of a channel (endpoint) held by another user. The old form carries
// only Endpoint.
type RemoveChannelRequest struct {
	// ExpectedUserID is the UUID of the user the page expects to be the
	// caller.
	ExpectedUserID string `json:"expectedUserId"`
	// Endpoint is the push service URL of the channel to remove.
	Endpoint string `json:"endpoint"`
	// IfSubscription, when present, limits the removal to that subscription
	// id and version.
	IfSubscription *IfSubscription `json:"ifSubscription,omitempty"`
}

// RevokeDeviceRequest is the body of POST /push/device-revocations:
// revocation of the caller's device. At least one of DeviceLabels and
// Endpoint is non-empty; DeviceLabels holds at most MaxDeviceLabels UUIDs.
type RevokeDeviceRequest struct {
	// ExpectedUserID is the UUID of the user the page expects to be the
	// caller.
	ExpectedUserID string `json:"expectedUserId"`
	// DeviceLabels are the UUID labels to revoke for the caller.
	DeviceLabels []string `json:"deviceLabels"`
	// Endpoint, when present, also removes the caller's unlabelled
	// subscription with this endpoint.
	Endpoint string `json:"endpoint,omitempty"`
}

// RemoveDeviceRequest is the body of POST /push/device-removals: removal of
// the caller's device by subscription id, revoking its label when it has one.
type RemoveDeviceRequest struct {
	// ExpectedUserID is the UUID of the user the page expects to be the
	// caller.
	ExpectedUserID string `json:"expectedUserId"`
	// ID is the subscription id.
	ID string `json:"id"`
}

// SubscriptionDTO is the caller-facing projection of a push subscription.
// It never carries the raw endpoint or the keys; revocations are never
// returned.
type SubscriptionDTO struct {
	// ID is the subscription id.
	ID string `json:"id"`
	// EndpointHash is the lowercase hex SHA-256 of the endpoint; the page
	// hashes its own endpoint to find the current device.
	EndpointHash string `json:"endpointHash"`
	// DeviceLabel is the UUID label of the device, null for a subscription
	// registered without one.
	DeviceLabel *string `json:"deviceLabel"`
	// Version grows with every write of the subscription.
	Version int64 `json:"version"`
	// UA is the user-agent string supplied at registration, if any.
	UA *string `json:"ua,omitempty"`
	// CreatedAt is when the subscription was created.
	CreatedAt time.Time `json:"createdAt"`
	// LastSeenAt is when the subscription was last registered or refreshed.
	LastSeenAt time.Time `json:"lastSeenAt"`
}
