package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
)

// apiKeyCreateMaxAttempts caps the prefix-collision retry loop in Create.
// 12 base64-url chars ≈ 71 bits → collisions are astronomical; three
// attempts is well past any conceivable rate. After this many failures
// we surface 500 to the user (and they can retry the API call).
const apiKeyCreateMaxAttempts = 3

// APIKeysHandler implements the /api/v1/me/api-keys/* endpoints (R2.1).
//
// Routing wires this under RequireInternalSecret + ParseUserHeaders +
// RequireUser, so every method is guaranteed a valid userID in ctx.
//
// All three endpoints are user-scoped: a user can only see and revoke
// their own keys. Admin-side cross-user management is deliberately not
// surfaced here — it lives in the (future) /api/v1/admin namespace.
type APIKeysHandler struct {
	store      *apikeys.Store
	bcryptCost int
}

// NewAPIKeysHandler builds the handler with the given store + bcrypt
// cost. Build once at startup; reuse across requests.
func NewAPIKeysHandler(pool *pgxpool.Pool, bcryptCost int) *APIKeysHandler {
	return &APIKeysHandler{
		store:      apikeys.New(pool),
		bcryptCost: bcryptCost,
	}
}

// createRequest is the JSON body for POST /me/api-keys. Label is the only
// user-supplied field — id, prefix, hash and timestamps are server-side.
//
// Length cap (1..100) is the same as F243's "human-readable label". Empty
// labels would render as a blank row in the UI; pre-trimming + length
// check keeps the shape clean.
type createRequest struct {
	Label string `json:"label"`
}

// createResponse is the ONLY place the plain key is ever returned. The
// caller must store it immediately — every subsequent GET returns the
// audit-safe APIKey shape (no plainKey field at all).
type createResponse struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Prefix    string `json:"prefix"`
	PlainKey  string `json:"plainKey"`
	CreatedAt string `json:"createdAt"`
}

const apiKeyLabelMax = 100

// Create handles POST /api/v1/me/api-keys. Generates a fresh key (16
// bytes of crypto/rand), bcrypt-hashes it, stores (prefix, hash), and
// returns the plaintext exactly once. The plain key MUST NOT appear in
// any subsequent GET — see ListByUser response shape.
//
// 400 on invalid JSON / empty / too-long label.
// 422 not used — there are no business preconditions.
// 500 on DB error or bcrypt failure (logged with stack via slog).
func (h *APIKeysHandler) Create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := auth.UserIDFrom(ctx)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "sign in required", nil)
		return
	}

	var req createRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_body", err.Error(), nil)
		return
	}
	label := strings.TrimSpace(req.Label)
	if label == "" {
		writeProblem(w, http.StatusBadRequest, "label_required", "label must not be empty", nil)
		return
	}
	if len(label) > apiKeyLabelMax {
		writeProblem(w, http.StatusBadRequest, "label_too_long", "label must be at most 100 characters", nil)
		return
	}

	// Prefix collisions are astronomically unlikely (12 base64-url chars,
	// active-only partial UNIQUE index — see migration 000010). The retry
	// loop exists to honour the contract advertised in apikeys.Store.Create:
	// a "23505 unique_violation" on the prefix column means we should
	// regenerate (plain, prefix, hash) and try again, NOT 500 on the user.
	// Any other DB error or bcrypt failure surfaces 500 immediately.
	var (
		plain     string
		prefix    string
		hash      string
		id        uuid.UUID
		createdAt time.Time
		createErr error
	)
	for attempt := range apiKeyCreateMaxAttempts {
		plain, prefix, hash, createErr = auth.GenerateAPIKey(h.bcryptCost)
		if createErr != nil {
			// rand.Read or bcrypt failure — logged but message is generic.
			slog.ErrorContext(ctx, "failed to generate api key",
				"userId", userID.String(),
				"error", createErr.Error())
			writeProblem(w, http.StatusInternalServerError, "internal_error", "failed to generate key", nil)
			return
		}

		id, createdAt, createErr = h.store.Create(ctx, userID, label, prefix, hash)
		if createErr == nil {
			break
		}

		var pgErr *pgconn.PgError
		if errors.As(createErr, &pgErr) && pgErr.Code == "23505" {
			// Prefix collision — log at WARN (this is genuinely rare in
			// production; a burst would suggest a broken RNG) and loop.
			slog.WarnContext(ctx, "api key prefix collision, retrying",
				"userId", userID.String(),
				"prefix", prefix,
				"attempt", attempt+1,
			)
			continue
		}
		// Non-collision DB error: bail out.
		slog.ErrorContext(ctx, "failed to insert api key",
			"userId", userID.String(),
			"error", createErr.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "failed to create key", nil)
		return
	}
	if createErr != nil {
		// All retries exhausted on collision.
		slog.ErrorContext(ctx, "failed to insert api key after retries",
			"userId", userID.String(),
			"attempts", apiKeyCreateMaxAttempts,
			"error", createErr.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "failed to create key", nil)
		return
	}

	// The masked form is what we put in the audit log; the plaintext only
	// ever leaves through the JSON response below.
	slog.InfoContext(ctx, "api key created",
		"userId", userID.String(),
		"keyId", id.String(),
		"label", label,
		"prefix", prefix,
		"masked", auth.MaskAPIKey(plain),
	)

	writeJSON(w, http.StatusCreated, createResponse{
		ID:        id.String(),
		Label:     label,
		Prefix:    prefix,
		PlainKey:  plain,
		CreatedAt: createdAt.Format("2006-01-02T15:04:05.000Z"),
	})
}

// List handles GET /api/v1/me/api-keys. Returns every (active +
// revoked) key for the calling user, newest-first. The response NEVER
// contains plain key material — only the prefix is safe to expose
// (it's stored unhashed in the DB by design).
func (h *APIKeysHandler) List(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := auth.UserIDFrom(ctx)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "sign in required", nil)
		return
	}

	keys, err := h.store.ListByUser(ctx, userID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list api keys",
			"userId", userID.String(),
			"error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "failed to list keys", nil)
		return
	}

	// Always return an array (even when empty) so the frontend never
	// has to special-case `null`.
	if keys == nil {
		keys = []apikeys.APIKey{}
	}
	writeJSON(w, http.StatusOK, keys)
}

// Revoke handles DELETE /api/v1/me/api-keys/{id}. Soft-revoke: sets
// revoked_at = now() but keeps the row for audit purposes. 204 on
// success. 404 when the id is unknown / not the caller's / already
// revoked — we deliberately collapse all three into the same response
// so an attacker can't enumerate which keys exist for which users.
func (h *APIKeysHandler) Revoke(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := auth.UserIDFrom(ctx)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "sign in required", nil)
		return
	}

	id, err := parseUUIDParam(r, "id")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_id", err.Error(), nil)
		return
	}

	err = h.store.Revoke(ctx, userID, id)
	switch {
	case errors.Is(err, apikeys.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "api key not found", nil)
		return
	case err != nil:
		slog.ErrorContext(ctx, "failed to revoke api key",
			"userId", userID.String(),
			"keyId", id.String(),
			"error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "failed to revoke key", nil)
		return
	}

	slog.InfoContext(ctx, "api key revoked",
		"userId", userID.String(),
		"keyId", id.String(),
	)
	w.WriteHeader(http.StatusNoContent)
}
