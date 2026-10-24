// Package operator is the SaaS operator's API (sk-674.18 T042, US-11, D-9):
// a second HTTP server of skein-api on its own port, guarded by its own
// key, that blocks, unblocks and deletes accounts.
//
// Contract (T042):
//
//	POST /operator/v1/accounts/block    AccountRequest → 204
//	POST /operator/v1/accounts/unblock  AccountRequest → 204
//	POST /operator/v1/accounts/delete   AccountRequest → 204
//
// The server runs only with SKEIN_OPERATOR_API_ENABLED=true, on
// SKEIN_OPERATOR_API_PORT (never the main PORT, a startup check of
// config.Validate). Every request — to any path, an unknown one included —
// carries the header X-Skein-Operator-Key equal to SKEIN_OPERATOR_API_KEY
// (compared as sha256 digests in constant time); otherwise 401
// CodeUnauthorized, before routing, and nothing else is answered. With the
// key an unknown path is 404. The operator's server serves none of the
// public routes and the main server serves none of these.
//
// The account is named by its email in the body, not by an id in the path:
// a SaaS operator has no instance admin to look ids up with (K-30), and a
// body stays out of the access log. The email is never logged.
//
// Errors are {"error": "<code>", "message": "…"} like the main API:
//
//	400 CodeInvalidRequest   — not one JSON object of AccountRequest, email missing
//	401 CodeUnauthorized     — the key header missing or wrong
//	422 CodeAccountNotFound  — no account has this email (a deleted account
//	                           is not found: its email is gone), or the
//	                           account was deleted between the lookup and
//	                           the operation (a concurrent deletion)
//	422 last_admin_cannot_demote — blocking or deleting the last active admin
//	422 owns_spaces_with_members, owns_common_space — deleting an owner
//	                           (US-10's limits), with the extra field `spaces`
//	422 concurrent_change    — the deletion lost three serialisation retries
//	500 internal_error
package operator

// Route paths of the operator's API.
const (
	PathBlock   = "/operator/v1/accounts/block"
	PathUnblock = "/operator/v1/accounts/unblock"
	PathDelete  = "/operator/v1/accounts/delete"
)

// KeyHeader is the header carrying the operator's key.
const KeyHeader = "X-Skein-Operator-Key"

// Machine codes of the operator's API (the other refusals reuse the codes
// of spaces/contract.go).
const (
	// CodeUnauthorized — 401: the key header is missing or wrong.
	CodeUnauthorized = "operator_unauthorized"
	// CodeInvalidRequest — 400: the body is not one AccountRequest object
	// or its email is empty.
	CodeInvalidRequest = "invalid_request"
	// CodeAccountNotFound — 422: no live or blocked account has the email.
	CodeAccountNotFound = "account_not_found"
)

// AccountRequest names the account of an operation.
//
// Literal: {"email":"someone@example.com"}
type AccountRequest struct {
	// Email is the account's email, compared case-insensitively.
	Email string `json:"email"`
}
