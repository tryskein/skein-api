package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/push"
)

// PushHandler serves the Web Push REST surface: VAPID public-key discovery
// plus CRUD over the caller's browser push subscriptions. Every subscription
// endpoint is caller-scoped — a user only ever reads/writes/deletes their own
// registrations (the owner scope folds "not yours" into a silent no-op for
// delete). Config is read-only and exposes only the PUBLIC key.
type PushHandler struct {
	pool        *pgxpool.Pool
	store       *push.Store
	vapidPublic string
	// enabled reports whether push delivery is actually wired (both VAPID
	// keys present). /push/config only surfaces the public key when this is
	// true — otherwise the frontend would enable push against a sender that
	// can never deliver (private key missing → every Send fails).
	enabled bool
}

// NewPushHandler wires the handler over the shared store + the VAPID public
// key from config. enabled must be true only when delivery is actually
// possible (both VAPID keys configured); when false /push/config returns an
// empty publicKey and the frontend feature-detects push as unavailable.
func NewPushHandler(pool *pgxpool.Pool, store *push.Store, vapidPublic string, enabled bool) *PushHandler {
	return &PushHandler{pool: pool, store: store, vapidPublic: vapidPublic, enabled: enabled}
}

// pushConfigResponse is the GET /push/config payload.
type pushConfigResponse struct {
	// PublicKey is the base64url VAPID application-server public key the
	// browser passes as applicationServerKey when subscribing. Empty when
	// push is not configured on this deployment.
	PublicKey string `json:"publicKey"`
}

// subscribeRequest is the POST /push/subscriptions body. Mirrors the browser
// PushSubscription.toJSON() shape ({endpoint, keys:{p256dh, auth}}) plus an
// optional ua the frontend may supply so the user can recognise the device.
type subscribeRequest struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
	UA string `json:"ua"`
}

// unsubscribeRequest is the DELETE /push/subscriptions body.
type unsubscribeRequest struct {
	Endpoint string `json:"endpoint"`
}

// subscriptionDTO is the lean, caller-facing projection of a push
// subscription. It deliberately OMITS the raw endpoint / p256dh / auth — the
// endpoint embeds a per-device push token and p256dh/auth are the RFC 8291
// encryption material; none of it is needed to render the "your devices"
// management list, so we never put it on the wire. Instead we expose
// endpointHash (SHA-256 of the endpoint): the frontend hashes its own
// browser PushSubscription.endpoint and matches it against this to highlight
// "this device" in the list — without the raw endpoint ever leaving the
// server. Per-id DELETE is keyed on ID (returned here); the legacy
// endpoint-keyed DELETE uses the endpoint the browser already holds.
type subscriptionDTO struct {
	ID string `json:"id"`
	// EndpointHash is the lowercase hex SHA-256 of the subscription's push
	// endpoint. Lets the frontend identify the current device by hashing its
	// own endpoint, while the raw endpoint (a bearer-capable push token) stays
	// server-side.
	EndpointHash string    `json:"endpointHash"`
	UA           *string   `json:"ua,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	LastSeenAt   time.Time `json:"lastSeenAt"`
}

// hashEndpoint returns the lowercase hex SHA-256 of a push endpoint. Used to
// expose a stable, non-reversible device identifier in the DTO so the frontend
// can match "this device" without the raw endpoint on the wire.
func hashEndpoint(endpoint string) string {
	sum := sha256.Sum256([]byte(endpoint))
	return hex.EncodeToString(sum[:])
}

// toSubscriptionDTO maps the internal store shape to the lean wire DTO. The
// raw endpoint is folded into endpointHash and never serialised directly.
func toSubscriptionDTO(s push.Subscription) subscriptionDTO {
	return subscriptionDTO{
		ID:           s.ID.String(),
		EndpointHash: hashEndpoint(s.Endpoint),
		UA:           s.UA,
		CreatedAt:    s.CreatedAt,
		LastSeenAt:   s.LastSeenAt,
	}
}

// Config handles GET /api/v1/push/config.
//
// @Summary Web Push VAPID public key
// @Description Returns the VAPID application-server PUBLIC key the browser needs to subscribe (passed as applicationServerKey). The key is surfaced ONLY when push delivery is actually wired (both VAPID keys present); when push is unconfigured — or only the public key is set, which can never deliver — the publicKey is the empty string and the frontend feature-detects this and hides the enable-notifications affordance. Always 200 (never 503) so the frontend has a single, simple branch.
// @Tags Push
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} pushConfigResponse "VAPID public key (possibly empty)"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Router /api/v1/push/config [get]
func (h *PushHandler) Config(w http.ResponseWriter, r *http.Request) {
	if _, ok := caller(r); !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	// Only advertise the public key when delivery is actually wired (both
	// VAPID keys present). Returning the public key with a missing private
	// key would have the frontend enable push against a dead sender.
	key := ""
	if h.enabled {
		key = h.vapidPublic
	}
	writeJSON(w, http.StatusOK, pushConfigResponse{PublicKey: key})
}

// Subscribe handles POST /api/v1/push/subscriptions.
//
// Algorithm:
//  1. Decode the browser PushSubscription payload.
//  2. Validate endpoint + both keys are non-empty AND the endpoint is a
//     public https URL (SSRF guard); business-invalid → 422.
//  3. Upsert keyed on endpoint (re-subscribing the same device re-homes the
//     row to the caller and refreshes its keys / last_seen_at).
//
// @Summary Register a browser push subscription
// @Description Stores (or refreshes) the caller's browser push registration. Keyed on the endpoint URL: a browser re-subscribing sends the same endpoint, so this is idempotent — the existing row is re-homed to the caller and its keys / User-Agent / last_seen_at are refreshed. The subscription is owned by the calling user.
// @Tags Push
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body subscribeRequest true "Browser PushSubscription (endpoint + keys)"
// @Success 200 {object} subscriptionDTO "Stored subscription"
// @Failure 400 {object} map[string]any "invalid_json"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 422 {object} map[string]any "invalid_subscription (endpoint / keys missing or endpoint not a public https URL)"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Router /api/v1/push/subscriptions [post]
func (h *PushHandler) Subscribe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}

	var req subscribeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}

	endpoint := strings.TrimSpace(req.Endpoint)
	p256dh := strings.TrimSpace(req.Keys.P256dh)
	auth := strings.TrimSpace(req.Keys.Auth)
	if endpoint == "" || p256dh == "" || auth == "" {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_subscription",
			"endpoint, keys.p256dh and keys.auth are all required", nil)
		return
	}
	// SSRF guard: reject endpoints that aren't a public https URL before we
	// ever store one we'd later POST to. Business-invalid input → 422 (repo
	// guideline: 4xx/5xx are reserved for routing/auth/server faults).
	if err := push.ValidateEndpoint(endpoint); err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_subscription",
			"endpoint must be a public https URL", nil)
		return
	}

	sub, err := h.store.Upsert(ctx, c.UserID, endpoint, p256dh, auth, strings.TrimSpace(req.UA))
	if err != nil {
		slog.ErrorContext(ctx, "push subscribe failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	slog.InfoContext(ctx, "push subscription stored",
		"userId", c.UserID.String(), "subscriptionId", sub.ID.String())
	writeJSON(w, http.StatusOK, toSubscriptionDTO(sub))
}

// Unsubscribe handles DELETE /api/v1/push/subscriptions.
//
// @Summary Remove a browser push subscription
// @Description Deletes the caller's push registration identified by endpoint. Owner-scoped: only the calling user's row for that endpoint is removed. Idempotent — a missing row is a no-op success (204).
// @Tags Push
// @Accept json
// @Security ApiKeyAuth
// @Param body body unsubscribeRequest true "Endpoint to unsubscribe"
// @Success 204 "Unsubscribed"
// @Failure 400 {object} map[string]any "invalid_json"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 422 {object} map[string]any "invalid_subscription (endpoint missing)"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Router /api/v1/push/subscriptions [delete]
func (h *PushHandler) Unsubscribe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}

	var req unsubscribeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	endpoint := strings.TrimSpace(req.Endpoint)
	if endpoint == "" {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_subscription",
			"endpoint is required", nil)
		return
	}

	if err := h.store.DeleteByEndpoint(ctx, c.UserID, endpoint); err != nil {
		slog.ErrorContext(ctx, "push unsubscribe failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	slog.InfoContext(ctx, "push subscription removed", "userId", c.UserID.String())
	w.WriteHeader(http.StatusNoContent)
}

// DeleteByID handles DELETE /api/v1/push/subscriptions/{id}.
//
// Algorithm:
//  1. Parse {id} as a UUID; malformed → 400.
//  2. Delete the row scoped to (id, caller) — a row owned by another user or a
//     non-existent id matches nothing.
//  3. No row matched → 404; row removed → 204.
//
// This is the id-keyed delete the device-management UI uses: it lists devices
// (subscriptionDTO carries the id) and can remove ANY of them by id, without
// the browser holding the matching endpoint. The legacy endpoint-keyed
// DELETE /push/subscriptions remains for the current-device unsubscribe.
//
// @Summary Remove a push subscription by id
// @Description Deletes one of the caller's push registrations by its row id. Owner-scoped: only a row owned by the calling user can be removed; an id belonging to another user — or one that does not exist — returns 404. Distinct from DELETE /push/subscriptions, which removes the current device by its browser-held endpoint.
// @Tags Push
// @Security ApiKeyAuth
// @Param id path string true "Subscription id (UUID)"
// @Success 204 "Deleted"
// @Failure 400 {object} map[string]any "invalid_id"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 404 {object} map[string]any "Subscription not found or not owned by caller"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Router /api/v1/push/subscriptions/{id} [delete]
func (h *PushHandler) DeleteByID(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	id, err := parseUUIDParam(r, "id")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_id", err.Error(), nil)
		return
	}

	deleted, err := h.store.DeleteByIDForUser(ctx, c.UserID, id)
	if err != nil {
		slog.ErrorContext(ctx, "push delete-by-id failed",
			"userId", c.UserID.String(), "subscriptionId", id.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	if !deleted {
		writeProblem(w, http.StatusNotFound, "not_found", "subscription not found", nil)
		return
	}
	slog.InfoContext(ctx, "push subscription removed by id",
		"userId", c.UserID.String(), "subscriptionId", id.String())
	w.WriteHeader(http.StatusNoContent)
}

// List handles GET /api/v1/push/subscriptions.
//
// @Summary List the caller's push subscriptions
// @Description Returns every browser push registration owned by the caller, newest-first, so a user can review and manage the devices they've enabled notifications on.
// @Tags Push
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {array} subscriptionDTO "Caller's subscriptions"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Router /api/v1/push/subscriptions [get]
func (h *PushHandler) List(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	subs, err := h.store.ListForUser(ctx, c.UserID)
	if err != nil {
		slog.ErrorContext(ctx, "push list failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	out := make([]subscriptionDTO, 0, len(subs))
	for _, s := range subs {
		out = append(out, toSubscriptionDTO(s))
	}
	writeJSON(w, http.StatusOK, out)
}
