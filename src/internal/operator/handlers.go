package operator

// handlers.go — the three operations of the operator's API (contract.go)
// and their implementation over the users store: blocking is the product's
// one deactivation (users.Store.Deactivate → DeactivateInTx, K-16),
// unblocking is Reactivate (the session epoch is not lowered — sessions
// issued before the block stay refused), deleting is the account deletion
// of T041 (users.Store.DeleteAccount) with US-10's limits and no password.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"skein-api/internal/spaces"
	"skein-api/internal/users"
)

// OperatorRequestsTotal counts the requests of the operator's API.
//
// @metric description Requests to the SaaS operator's API (block, unblock, delete an account), by operation and outcome; one increment per request, including the ones refused for a missing or wrong operator key.
// @metric calculation Incremented once per request: in operator.requireKey when the key is missing or wrong (operation none, result unauthorized), otherwise in the operation's handler after the answer is decided (handlers.finish).
// @label operation The operation; none for a request refused by the key check before routing. Values: block, unblock, delete, none.
// @label result The outcome: success, a refusal code, unauthorized (no or wrong key) or error (internal failure). Values: success, invalid_request, account_not_found, last_admin_cannot_demote, owns_spaces_with_members, owns_common_space, concurrent_change, unauthorized, error.
var OperatorRequestsTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "skein_operator_requests_total",
		Help: "Requests to the operator API by operation and result",
	},
	[]string{"operation", "result"},
)

const (
	operationNone      = "none"
	resultUnauthorized = "unauthorized"
	resultSuccess      = "success"
	resultError        = "error"
)

// ErrAccountNotFound — no live or blocked account has the email.
var ErrAccountNotFound = errors.New("operator: account not found")

// Operations is what the handlers do; Accounts implements it.
type Operations interface {
	Block(ctx context.Context, email string) error
	Unblock(ctx context.Context, email string) error
	Delete(ctx context.Context, email string) error
}

// Accounts runs the operations on the users store.
type Accounts struct {
	pool  *pgxpool.Pool
	users *users.Store
}

// NewAccounts builds the operations over pool and the users store.
func NewAccounts(pool *pgxpool.Pool, store *users.Store) *Accounts {
	return &Accounts{pool: pool, users: store}
}

// actor is who the operator's events name: the system, no user.
var actor = users.Actor{Username: "operator", Role: "system"}

// find resolves the email to the account; a deleted account is not found.
func (a *Accounts) find(ctx context.Context, email string) (uuid.UUID, error) {
	var id uuid.UUID
	err := a.pool.QueryRow(ctx, `
SELECT id FROM users WHERE lower(email) = lower(btrim($1)) AND deleted_at IS NULL`, email).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrAccountNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("finding the account: %w", err)
	}
	return id, nil
}

// Block deactivates the account (idempotent).
func (a *Accounts) Block(ctx context.Context, email string) error {
	id, err := a.find(ctx, email)
	if err != nil {
		return err
	}
	return a.users.Deactivate(ctx, id, actor)
}

// Unblock reactivates the account (idempotent; the epoch stays).
func (a *Accounts) Unblock(ctx context.Context, email string) error {
	id, err := a.find(ctx, email)
	if err != nil {
		return err
	}
	return a.users.Reactivate(ctx, id, actor)
}

// Delete deletes the account with US-10's limits, without a password.
func (a *Accounts) Delete(ctx context.Context, email string) error {
	id, err := a.find(ctx, email)
	if err != nil {
		return err
	}
	return a.users.DeleteAccount(ctx, id, "", actor)
}

type handlers struct {
	ops Operations
}

// bodyMaxBytes caps a request body.
const bodyMaxBytes = 16 << 10

// readRequest decodes one AccountRequest with a non-empty email.
func readRequest(r *http.Request) (AccountRequest, bool) {
	var req AccountRequest
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, bodyMaxBytes))
	if err := dec.Decode(&req); err != nil {
		return req, false
	}
	if err := dec.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) {
		return req, false
	}
	return req, strings.TrimSpace(req.Email) != ""
}

// serve runs one operation and answers it.
func (h *handlers) serve(w http.ResponseWriter, r *http.Request, operation string, op func(context.Context, string) error) {
	req, ok := readRequest(r)
	if !ok {
		h.finish(operation, CodeInvalidRequest)
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, `body: {"email": "…"}`, nil)
		return
	}
	err := op(r.Context(), req.Email)
	if err == nil {
		h.finish(operation, resultSuccess)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var ref *spaces.Refusal
	switch {
	// users.ErrUserNotFound: the account was deleted between the lookup
	// and the operation — by a concurrent deletion.
	case errors.Is(err, ErrAccountNotFound), errors.Is(err, users.ErrUserNotFound):
		h.finish(operation, CodeAccountNotFound)
		writeError(w, http.StatusUnprocessableEntity, CodeAccountNotFound, "no account has this email", nil)
	case errors.Is(err, users.ErrLastAdmin):
		h.finish(operation, spaces.CodeLastAdminCannotDemote)
		writeError(w, http.StatusUnprocessableEntity, spaces.CodeLastAdminCannotDemote,
			"the last active administrator cannot be blocked", nil)
	case errors.As(spaces.AsRefusal(err), &ref):
		h.finish(operation, ref.Code)
		writeError(w, ref.Status, ref.Code, ref.Message, ref.Extra)
	default:
		h.finish(operation, resultError)
		slog.ErrorContext(r.Context(), "operator operation failed", "operation", operation, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "internal_error", "", nil)
	}
}

// finish counts the request and logs its outcome — never the email.
func (h *handlers) finish(operation, result string) {
	OperatorRequestsTotal.WithLabelValues(operation, result).Inc()
	slog.Info("operator operation", "operation", operation, "result", result)
}

// Block handles POST /operator/v1/accounts/block.
//
// @Summary Block an account
// @Description Business meaning: the operator blocks an account (abuse, non-payment): it can no longer sign in, every session and API key of it stops working at once, its memberships, assignments and spaces stay and keep working for the others (US-11: blocking is the deactivation).
// @Description
// @Description Algorithm: the account is found by email, case-insensitively (a deleted account is not found); one SERIALIZABLE transaction sets deactivated_at, raises the session epoch and revokes every API key, refusing the last active admin. Blocking a blocked account is a no-op answered 204.
// @Description
// @Description Side effects: the user.deactivated event (skein-web drops the account's sessions and hub groups); personal signals. Refusals: 400 invalid_request; 422 account_not_found, last_admin_cannot_demote, concurrent_change (a serialization conflict after three attempts — nothing is changed, repeat).
// @Tags Operator
// @Accept json
// @Security OperatorKeyAuth
// @Param body body AccountRequest true "The account's email"
// @Success 204 "Blocked"
// @Failure 400 {object} map[string]any "invalid_request"
// @Failure 401 {object} map[string]any "operator_unauthorized"
// @Failure 422 {object} map[string]any "account_not_found, concurrent_change, last_admin_cannot_demote"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /operator/v1/accounts/block [post]
func (h *handlers) Block(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r, "block", h.ops.Block)
}

// Unblock handles POST /operator/v1/accounts/unblock.
//
// @Summary Unblock an account
// @Description Business meaning: the operator lets a blocked account sign in again. Sessions issued before the block do not come back (the session epoch is not lowered) and the revoked API keys stay revoked — the owner signs in anew and issues new keys.
// @Description
// @Description Algorithm: the account is found by email (a deleted account is not found); deactivated_at is cleared. Unblocking an active account is a no-op answered 204.
// @Description
// @Description Side effects: the user.reactivated event; personal signals. Refusals: 400 invalid_request; 422 account_not_found, concurrent_change (a serialization conflict after three attempts — nothing is changed, repeat).
// @Tags Operator
// @Accept json
// @Security OperatorKeyAuth
// @Param body body AccountRequest true "The account's email"
// @Success 204 "Unblocked"
// @Failure 400 {object} map[string]any "invalid_request"
// @Failure 401 {object} map[string]any "operator_unauthorized"
// @Failure 422 {object} map[string]any "account_not_found, concurrent_change"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /operator/v1/accounts/unblock [post]
func (h *handlers) Unblock(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r, "unblock", h.ops.Unblock)
}

// Delete handles POST /operator/v1/accounts/delete.
//
// @Summary Delete an account
// @Description Business meaning: the operator deletes an account at once and for good — the account deletion of US-10 without the password and whatever SKEIN_SIGNUP_ENABLED says, with the same limits: an owner of a space with other people, or of the common space, is refused.
// @Description
// @Description Algorithm: the account is found by email (a deleted account is not found); then the account deletion in one SERIALIZABLE transaction: deactivation (sessions and keys end), the personal space and the regular spaces it owns alone deleted, the other spaces left as by US-7, its invitations revoked, its personal state deleted, its email, username and name replaced by the pseudonym deleted-<8 hex> everywhere they are kept, their hashes reserved so neither registers again.
// @Description
// @Description Side effects: the user.deactivated event, the space events and signals of the deleted and left spaces. Refusals: 400 invalid_request; 422 account_not_found, owns_spaces_with_members and owns_common_space (with `spaces`, the blocking spaces), last_admin_cannot_demote, concurrent_change.
// @Tags Operator
// @Accept json
// @Security OperatorKeyAuth
// @Param body body AccountRequest true "The account's email"
// @Success 204 "Deleted"
// @Failure 400 {object} map[string]any "invalid_request"
// @Failure 401 {object} map[string]any "operator_unauthorized"
// @Failure 422 {object} map[string]any "account_not_found, owns_spaces_with_members, owns_common_space, last_admin_cannot_demote, concurrent_change"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /operator/v1/accounts/delete [post]
func (h *handlers) Delete(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r, "delete", h.ops.Delete)
}

// writeError answers {"error": code, "message": msg} merged with extra —
// the error shape of the main API.
func writeError(w http.ResponseWriter, status int, code, msg string, extra map[string]any) {
	body := make(map[string]any, len(extra)+2)
	for k, v := range extra {
		body[k] = v
	}
	body["error"] = code
	if msg != "" {
		body["message"] = msg
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("failed to encode the operator's error", "error", err.Error())
	}
}
