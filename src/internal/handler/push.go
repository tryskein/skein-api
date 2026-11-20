package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/push"
)

// PushHandler serves the Web Push REST surface, contract v2
// (internal/push/contract.go): VAPID public-key discovery, registration of
// the caller's browser channel, revocation and removal of the caller's
// devices, removal of a channel held by another user, and the caller's
// device list. Every page operation carries the user the page expects
// (expectedUserId) and changes nothing when the caller is someone else.
// Every write runs in one transaction of the store that checks, at the
// moment of the write, that the caller is active and that the session epoch
// checked by RequireSessionEpoch is still current.
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

// PushContractV2 marks every response of the /push group with
// push.ContractHeader: push.ContractVersion. It is mounted on the group only:
// responses produced before the group (401 session_revoked of
// RequireSessionEpoch) and routes outside it do not carry the marker.
func PushContractV2(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(push.ContractHeader, push.ContractVersion)
		next.ServeHTTP(w, r)
	})
}

// pushConfigResponse is the GET /push/config payload.
type pushConfigResponse struct {
	// PublicKey is the base64url VAPID application-server public key the
	// browser passes as applicationServerKey when subscribing. Empty when
	// push is not configured on this deployment.
	PublicKey string `json:"publicKey"`
}

// hashEndpoint returns the lowercase hex SHA-256 of a push endpoint. Used to
// expose a stable, non-reversible device identifier in the DTO so the frontend
// can match "this device" without the raw endpoint on the wire.
func hashEndpoint(endpoint string) string {
	sum := sha256.Sum256([]byte(endpoint))
	return hex.EncodeToString(sum[:])
}

// toSubscriptionDTO maps the stored subscription to the wire DTO. The raw
// endpoint is folded into endpointHash; the keys are never serialised.
func toSubscriptionDTO(s push.Subscription) push.SubscriptionDTO {
	var label *string
	if s.DeviceLabel != nil {
		l := s.DeviceLabel.String()
		label = &l
	}
	return push.SubscriptionDTO{
		ID:           s.ID.String(),
		EndpointHash: hashEndpoint(s.Endpoint),
		DeviceLabel:  label,
		Version:      s.Version,
		UA:           s.UA,
		CreatedAt:    s.CreatedAt,
		LastSeenAt:   s.LastSeenAt,
	}
}

// decodePushJSON decodes the body of a push request into dst: one JSON
// value, no unknown fields, nothing after it but whitespace — trailing
// data is a malformed body (400 invalid_json), not a request to act on.
// Stricter than decodeJSON, which the other routes keep using unchanged.
func decodePushJSON(r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, requestBodyMaxBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return errBodyTooLarge
		}
		return fmt.Errorf("reading the body: %w", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return errors.New("request body is empty")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("invalid JSON: data after the JSON value")
	}
	return nil
}

// pushCall is the caller of a push write: the user and the session epoch
// RequireSessionEpoch checked.
type pushCall struct {
	userID uuid.UUID
	epoch  int64
}

// pushCaller resolves the caller of a push operation; on failure the
// response is written and ok is false. A user request reaches the group only
// through RequireSessionEpoch, which stores the checked epoch; a missing
// epoch is a wiring fault (500).
func pushCaller(w http.ResponseWriter, r *http.Request) (pushCall, bool) {
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return pushCall{}, false
	}
	epoch, ok := auth.SessionEpochFrom(r.Context())
	if !ok {
		slog.ErrorContext(r.Context(), "push request without a checked session epoch", "userId", c.UserID.String())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return pushCall{}, false
	}
	return pushCall{userID: c.UserID, epoch: epoch}, true
}

// parseExpectedUser parses the expectedUserId of a request; on a value that
// is not a UUID it writes 400 invalid_request and returns false.
func parseExpectedUser(w http.ResponseWriter, raw string) (uuid.UUID, bool) {
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, push.CodeInvalidRequest, "expectedUserId must be a UUID", nil)
		return uuid.Nil, false
	}
	return id, true
}

// sameUser checks the expected user against the caller before anything is
// read or written (INV-12). On a mismatch it logs the refusal (the caller,
// the code, the operation — never the expected user's data), writes 422
// expected_user_mismatch and returns false.
func sameUser(w http.ResponseWriter, r *http.Request, c pushCall, expected uuid.UUID, operation string) bool {
	if expected == c.userID {
		return true
	}
	slog.InfoContext(r.Context(), "push operation refused",
		"userId", c.userID.String(), "code", push.CodeExpectedUserMismatch, "operation", operation)
	writeProblem(w, http.StatusUnprocessableEntity, push.CodeExpectedUserMismatch,
		"the page was rendered for another user", nil)
	return false
}

// writeStoreRefusal answers an error of a store write: a refusal sentinel is
// logged (the caller, the code — never an endpoint or an owner) and
// answered 422 with its code; any other error is logged and answered 500.
func writeStoreRefusal(w http.ResponseWriter, r *http.Request, c pushCall, err error, operation string) {
	code := ""
	switch {
	case errors.Is(err, push.ErrSessionStale):
		code = push.CodeSessionStale
	case errors.Is(err, push.ErrDeviceRevoked):
		code = push.CodeDeviceRevoked
	case errors.Is(err, push.ErrEndpointTaken):
		code = push.CodeEndpointTaken
	case errors.Is(err, push.ErrSubscriptionNotFound):
		code = push.CodeSubscriptionNotFound
	}
	if code == "" {
		slog.ErrorContext(r.Context(), "push write failed",
			"userId", c.userID.String(), "operation", operation, "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	slog.InfoContext(r.Context(), "push operation refused",
		"userId", c.userID.String(), "code", code, "operation", operation)
	writeProblem(w, http.StatusUnprocessableEntity, code, pushRefusalMessage[code], nil)
}

// pushRefusalMessage holds the human-readable message of each refusal; none
// names an endpoint, a key or an owner.
var pushRefusalMessage = map[string]string{
	push.CodeSessionStale:         "the session is no longer valid",
	push.CodeDeviceRevoked:        "this device was signed out of notifications",
	push.CodeEndpointTaken:        "this browser channel is registered to another account",
	push.CodeSubscriptionNotFound: "no such subscription",
}

// Config handles GET /api/v1/push/config.
//
// @Summary Web Push VAPID public key
// @Description Returns the VAPID application-server PUBLIC key the browser needs to subscribe (passed as applicationServerKey). The key is surfaced ONLY when push delivery is actually wired (both VAPID keys present); when push is unconfigured — or only the public key is set, which can never deliver — the publicKey is the empty string and the frontend feature-detects this and hides the enable-notifications affordance. Always 200 (never 503) so the frontend has a single, simple branch.
// @Description
// @Description The response carries the header Skein-Push-Contract: 2 like every response of the /push group; the body is unchanged by contract v2.
// @Tags Push
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} pushConfigResponse "VAPID public key (possibly empty)"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Header 200 {string} Skein-Push-Contract "2 — push REST contract v2"
// @Router /ui/v1/push/config [get]
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
// @Summary Register the caller's browser push channel
// @Description Registers the caller's browser push channel (endpoint + keys). Two forms of body (internal/push/contract.go, "Form of a body"):
// @Description - new form (has expectedUserId; also deviceLabel and mode): mode "upsert" creates the caller's subscription with the device label at version 1 or updates the caller's existing one; mode "updateOnly" only updates the caller's existing one.
// @Description - old form (none of expectedUserId, deviceLabel, mode — older pages and service workers): only refreshes the keys of the caller's existing subscription; its label is not touched; nothing is created.
// @Description
// @Description Algorithm: 1. decode the body (400 invalid_json); 2. tell the form; deviceLabel or mode without expectedUserId, a value that is not a UUID, a missing deviceLabel or mode in the new form, an unknown mode → 400 invalid_request; 3. new form: expectedUserId other than the caller → 422 expected_user_mismatch, nothing is read or written; 4. endpoint or a key missing, or the endpoint not a public https URL → 422 invalid_subscription; 5. one SERIALIZABLE transaction: the caller inactive or the session epoch changed → 422 session_stale; the label revoked by the caller → 422 device_revoked (whatever the endpoint); the endpoint belongs to another user → 422 endpoint_taken (that subscription is unchanged; the owner is not disclosed); no own subscription with the endpoint in updateOnly or the old form → 422 subscription_not_found; otherwise the own subscription gets the new keys, User-Agent, label (new form), version + 1 and last_seen_at, or a new one is created (upsert).
// @Description
// @Description Side effects: the subscription row; a log record of the success (userId, subscriptionId) or of the refusal (userId, code) — never the endpoint, the keys or an owner. Every response of the /push group carries Skein-Push-Contract: 2; a 401 of the session check before the group does not.
// @Tags Push
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body push.SubscribeRequest true "Channel registration, new or old form"
// @Success 200 {object} push.SubscriptionDTO "Stored subscription (no endpoint, no keys)"
// @Failure 400 {object} map[string]any "invalid_json | invalid_request"
// @Failure 401 {object} map[string]any "Missing / invalid auth, or session_revoked"
// @Failure 422 {object} map[string]any "expected_user_mismatch | invalid_subscription | session_stale | device_revoked | endpoint_taken | subscription_not_found"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Header 200,400,422 {string} Skein-Push-Contract "2 — push REST contract v2"
// @Router /ui/v1/push/subscriptions [post]
func (h *PushHandler) Subscribe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := pushCaller(w, r)
	if !ok {
		return
	}
	var req push.SubscribeRequest
	if err := decodePushJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, push.CodeInvalidJSON, err.Error(), nil)
		return
	}

	in := push.RegisterInput{UserID: c.userID, SessionEpoch: c.epoch}
	if req.ExpectedUserID == "" {
		if req.DeviceLabel != "" || req.Mode != "" {
			writeProblem(w, http.StatusBadRequest, push.CodeInvalidRequest,
				"deviceLabel and mode require expectedUserId", nil)
			return
		}
	} else {
		expected, ok := parseExpectedUser(w, req.ExpectedUserID)
		if !ok {
			return
		}
		label, err := uuid.Parse(strings.TrimSpace(req.DeviceLabel))
		if err != nil {
			writeProblem(w, http.StatusBadRequest, push.CodeInvalidRequest, "deviceLabel must be a UUID", nil)
			return
		}
		if req.Mode != push.ModeUpsert && req.Mode != push.ModeUpdateOnly {
			writeProblem(w, http.StatusBadRequest, push.CodeInvalidRequest, "mode must be upsert or updateOnly", nil)
			return
		}
		if !sameUser(w, r, c, expected, "register") {
			return
		}
		in.DeviceLabel = &label
		in.Mode = req.Mode
	}

	in.Endpoint = strings.TrimSpace(req.Endpoint)
	in.P256dh = strings.TrimSpace(req.Keys.P256dh)
	in.Auth = strings.TrimSpace(req.Keys.Auth)
	in.UA = strings.TrimSpace(req.UA)
	if in.Endpoint == "" || in.P256dh == "" || in.Auth == "" {
		writeProblem(w, http.StatusUnprocessableEntity, push.CodeInvalidSubscription,
			"endpoint, keys.p256dh and keys.auth are all required", nil)
		return
	}
	// SSRF guard: reject endpoints that aren't a public https URL before we
	// ever store one we'd later POST to.
	if err := push.ValidateEndpoint(in.Endpoint); err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, push.CodeInvalidSubscription,
			"endpoint must be a public https URL", nil)
		return
	}

	sub, err := h.store.Register(ctx, in)
	if err != nil {
		writeStoreRefusal(w, r, c, err, "register")
		return
	}
	slog.InfoContext(ctx, "push subscription stored",
		"userId", c.userID.String(), "subscriptionId", sub.ID.String())
	writeJSON(w, http.StatusOK, toSubscriptionDTO(sub))
}

// Unsubscribe handles DELETE /api/v1/push/subscriptions.
//
// @Summary Remove a browser push channel by endpoint
// @Description Removes the subscription with the presented endpoint. Two forms of body:
// @Description - new form (has expectedUserId): removal of a channel held by any user — the page of whoever holds the browser channel clears it; nothing is revoked, the owner may register the same device again. With ifSubscription {id, version} only the subscription with that id and version is removed; a newer record is left untouched.
// @Description - old form ({endpoint} only): removes only the caller's own subscription with that endpoint, without revocation.
// @Description
// @Description Algorithm: 1. decode (400 invalid_json); ifSubscription without expectedUserId or a value that is not a UUID → 400 invalid_request; 2. new form: expectedUserId other than the caller → 422 expected_user_mismatch; 3. endpoint missing → 422 invalid_subscription; 4. one SERIALIZABLE transaction: the caller inactive or the session epoch changed → 422 session_stale; else the matching subscription is deleted.
// @Description
// @Description Meaning of 204, in the serial order of the transactions: without ifSubscription no user has a subscription with this endpoint at that point (a registration ordered after the removal is a new record it does not touch); with ifSubscription there is no subscription with this endpoint, id and version. 204 is the same, with an empty body, whether a subscription existed and whoever owned it: the owner is never disclosed.
// @Description
// @Description Side effects: a log record of a removal (callerId, ownerId) — never the endpoint. Every response of the /push group carries Skein-Push-Contract: 2.
// @Tags Push
// @Accept json
// @Security ApiKeyAuth
// @Param body body push.RemoveChannelRequest true "Channel to remove, new or old form"
// @Success 204 "No such subscription remains (see the meaning of 204)"
// @Failure 400 {object} map[string]any "invalid_json | invalid_request"
// @Failure 401 {object} map[string]any "Missing / invalid auth, or session_revoked"
// @Failure 422 {object} map[string]any "expected_user_mismatch | invalid_subscription | session_stale"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Header 204,400,422 {string} Skein-Push-Contract "2 — push REST contract v2"
// @Router /ui/v1/push/subscriptions [delete]
func (h *PushHandler) Unsubscribe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := pushCaller(w, r)
	if !ok {
		return
	}
	var req push.RemoveChannelRequest
	if err := decodePushJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, push.CodeInvalidJSON, err.Error(), nil)
		return
	}

	newForm := req.ExpectedUserID != ""
	var guard *push.VersionGuard
	if !newForm && req.IfSubscription != nil {
		writeProblem(w, http.StatusBadRequest, push.CodeInvalidRequest,
			"ifSubscription requires expectedUserId", nil)
		return
	}
	if newForm {
		expected, ok := parseExpectedUser(w, req.ExpectedUserID)
		if !ok {
			return
		}
		if req.IfSubscription != nil {
			id, err := uuid.Parse(strings.TrimSpace(req.IfSubscription.ID))
			if err != nil {
				writeProblem(w, http.StatusBadRequest, push.CodeInvalidRequest, "ifSubscription.id must be a UUID", nil)
				return
			}
			guard = &push.VersionGuard{ID: id, Version: req.IfSubscription.Version}
		}
		if !sameUser(w, r, c, expected, "remove_channel") {
			return
		}
	}
	endpoint := strings.TrimSpace(req.Endpoint)
	if endpoint == "" {
		writeProblem(w, http.StatusUnprocessableEntity, push.CodeInvalidSubscription, "endpoint is required", nil)
		return
	}

	owner, err := h.store.RemoveChannel(ctx, c.userID, c.epoch, endpoint, guard, newForm)
	if err != nil {
		writeStoreRefusal(w, r, c, err, "remove_channel")
		return
	}
	if owner != nil {
		slog.InfoContext(ctx, "push channel removed",
			"callerId", c.userID.String(), "ownerId", owner.String())
	}
	w.WriteHeader(http.StatusNoContent)
}

// RevokeDevice handles POST /api/v1/push/device-revocations.
//
// @Summary Revoke the caller's device
// @Description Revokes the caller's device: each listed label is recorded as revoked for the caller — also when no subscription carries it — every subscription of the caller with one of the labels is deleted, and so is the caller's unlabelled subscription with the presented endpoint. A revoked label refuses every later registration of the caller with it, whatever the endpoint (422 device_revoked); a new label is not affected. Other users' subscriptions and revocations are never touched.
// @Description
// @Description Algorithm: 1. decode (400 invalid_json); 2. expectedUserId missing or not a UUID, a label that is not a UUID, more than 1000 labels, or neither a label nor an endpoint → 400 invalid_request; 3. expectedUserId other than the caller → 422 expected_user_mismatch, nothing changes; 4. one SERIALIZABLE transaction: the caller inactive or the session epoch changed → 422 session_stale; else the revocations and deletions above.
// @Description
// @Description Meaning of 204: the listed labels are revoked for the caller, the caller has no subscription with these labels and no unlabelled subscription with the presented endpoint.
// @Description
// @Description Side effects: revocation rows; deleted subscriptions; a log record (userId, deviceLabel, number removed) — never the endpoint. Every response of the /push group carries Skein-Push-Contract: 2.
// @Tags Push
// @Accept json
// @Security ApiKeyAuth
// @Param body body push.RevokeDeviceRequest true "Labels (at most 1000) and/or the endpoint of the device"
// @Success 204 "Revoked (also when there were no subscriptions)"
// @Failure 400 {object} map[string]any "invalid_json | invalid_request"
// @Failure 401 {object} map[string]any "Missing / invalid auth, or session_revoked"
// @Failure 422 {object} map[string]any "expected_user_mismatch | session_stale"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Header 204,400,422 {string} Skein-Push-Contract "2 — push REST contract v2"
// @Router /ui/v1/push/device-revocations [post]
func (h *PushHandler) RevokeDevice(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := pushCaller(w, r)
	if !ok {
		return
	}
	var req push.RevokeDeviceRequest
	if err := decodePushJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, push.CodeInvalidJSON, err.Error(), nil)
		return
	}
	expected, ok := parseExpectedUser(w, req.ExpectedUserID)
	if !ok {
		return
	}
	if len(req.DeviceLabels) > push.MaxDeviceLabels {
		writeProblem(w, http.StatusBadRequest, push.CodeInvalidRequest, "too many deviceLabels", nil)
		return
	}
	labels := make([]uuid.UUID, 0, len(req.DeviceLabels))
	for _, raw := range req.DeviceLabels {
		l, err := uuid.Parse(strings.TrimSpace(raw))
		if err != nil {
			writeProblem(w, http.StatusBadRequest, push.CodeInvalidRequest, "every deviceLabel must be a UUID", nil)
			return
		}
		labels = append(labels, l)
	}
	endpoint := strings.TrimSpace(req.Endpoint)
	if len(labels) == 0 && endpoint == "" {
		writeProblem(w, http.StatusBadRequest, push.CodeInvalidRequest, "deviceLabels or endpoint is required", nil)
		return
	}
	if !sameUser(w, r, c, expected, "revoke_device") {
		return
	}

	removed, err := h.store.RevokeDevice(ctx, c.userID, c.epoch, labels, endpoint)
	if err != nil {
		writeStoreRefusal(w, r, c, err, "revoke_device")
		return
	}
	logged := make([]string, len(labels))
	for i, l := range labels {
		logged[i] = l.String()
	}
	slog.InfoContext(ctx, "push device revoked",
		"userId", c.userID.String(), "deviceLabel", logged, "removed", removed)
	w.WriteHeader(http.StatusNoContent)
}

// RemoveDevice handles POST /api/v1/push/device-removals.
//
// @Summary Remove the caller's device by subscription id
// @Description Removes one of the caller's devices by subscription id (the id of the device list). When the subscription has a device label, the label is revoked for the caller and every subscription of the caller with that label is deleted too, so the device cannot register with it again.
// @Description
// @Description Algorithm: 1. decode (400 invalid_json); 2. expectedUserId or id missing or not a UUID → 400 invalid_request; 3. expectedUserId other than the caller → 422 expected_user_mismatch; 4. one SERIALIZABLE transaction: the caller inactive or the session epoch changed → 422 session_stale; no subscription of the caller with this id (another user's or none) → 422 subscription_not_found, nothing changes; else the removal above.
// @Description
// @Description Meaning of 204: the caller has no subscription with this id and, if it had a label, the label is revoked for the caller and the caller has no subscription with that label.
// @Description
// @Description Side effects: deleted subscriptions, a revocation row; a log record (userId, subscriptionId, number removed). Every response of the /push group carries Skein-Push-Contract: 2.
// @Tags Push
// @Accept json
// @Security ApiKeyAuth
// @Param body body push.RemoveDeviceRequest true "Subscription id of the device"
// @Success 204 "Removed"
// @Failure 400 {object} map[string]any "invalid_json | invalid_request"
// @Failure 401 {object} map[string]any "Missing / invalid auth, or session_revoked"
// @Failure 422 {object} map[string]any "expected_user_mismatch | session_stale | subscription_not_found"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Header 204,400,422 {string} Skein-Push-Contract "2 — push REST contract v2"
// @Router /ui/v1/push/device-removals [post]
func (h *PushHandler) RemoveDevice(w http.ResponseWriter, r *http.Request) {
	c, ok := pushCaller(w, r)
	if !ok {
		return
	}
	var req push.RemoveDeviceRequest
	if err := decodePushJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, push.CodeInvalidJSON, err.Error(), nil)
		return
	}
	expected, ok := parseExpectedUser(w, req.ExpectedUserID)
	if !ok {
		return
	}
	id, err := uuid.Parse(strings.TrimSpace(req.ID))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, push.CodeInvalidRequest, "id must be a UUID", nil)
		return
	}
	if !sameUser(w, r, c, expected, "remove_device") {
		return
	}
	h.removeDevice(w, r, c, id)
}

// DeleteByID handles DELETE /api/v1/push/subscriptions/{id}.
//
// @Summary Remove the caller's device by id (older pages)
// @Description Kept for pages that predate contract v2; current pages use POST /push/device-removals, which carries the expected user. Same effect: removes the caller's subscription by id and, when it has a device label, revokes the label for the caller and deletes every subscription of the caller with that label.
// @Description
// @Description Algorithm: 1. {id} not a UUID → 400 invalid_request; 2. one SERIALIZABLE transaction: the caller inactive or the session epoch changed → 422 session_stale; no subscription of the caller with this id (another user's or none) → 422 subscription_not_found (was 404); else the removal above.
// @Description
// @Description Meaning of 204: the caller has no subscription with this id and, if it had a label, the label is revoked for the caller and the caller has no subscription with that label. Side effects: as POST /push/device-removals. Every response of the /push group carries Skein-Push-Contract: 2.
// @Tags Push
// @Security ApiKeyAuth
// @Param id path string true "Subscription id (UUID)"
// @Success 204 "Removed"
// @Failure 400 {object} map[string]any "invalid_request"
// @Failure 401 {object} map[string]any "Missing / invalid auth, or session_revoked"
// @Failure 422 {object} map[string]any "session_stale | subscription_not_found"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Header 204,400,422 {string} Skein-Push-Contract "2 — push REST contract v2"
// @Router /ui/v1/push/subscriptions/{id} [delete]
func (h *PushHandler) DeleteByID(w http.ResponseWriter, r *http.Request) {
	c, ok := pushCaller(w, r)
	if !ok {
		return
	}
	id, err := parseUUIDParam(r, "id")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, push.CodeInvalidRequest, err.Error(), nil)
		return
	}
	h.removeDevice(w, r, c, id)
}

// removeDevice is the removal by id of both routes.
func (h *PushHandler) removeDevice(w http.ResponseWriter, r *http.Request, c pushCall, id uuid.UUID) {
	removed, err := h.store.RemoveDeviceByID(r.Context(), c.userID, c.epoch, id)
	if err != nil {
		writeStoreRefusal(w, r, c, err, "remove_device")
		return
	}
	slog.InfoContext(r.Context(), "push device removed by id",
		"userId", c.userID.String(), "subscriptionId", id.String(), "removed", removed)
	w.WriteHeader(http.StatusNoContent)
}

// List handles GET /api/v1/push/subscriptions.
//
// @Summary List the caller's push subscriptions
// @Description Returns every push subscription owned by the caller, newest first, so the user can review and remove devices. Each item carries the endpoint only as endpointHash (SHA-256 hex) and never the keys; deviceLabel and version are additive fields of contract v2. Revocations are never returned.
// @Description
// @Description With ?expectedUserId= (current pages): a value that is not a UUID → 400 invalid_request; another user than the caller → 422 expected_user_mismatch and no list. Without the parameter (older pages) — as before. Every response of the /push group carries Skein-Push-Contract: 2.
// @Tags Push
// @Produce json
// @Security ApiKeyAuth
// @Param expectedUserId query string false "UUID of the user the page expects to be the caller"
// @Success 200 {array} push.SubscriptionDTO "Caller's subscriptions"
// @Failure 400 {object} map[string]any "invalid_request"
// @Failure 401 {object} map[string]any "Missing / invalid auth, or session_revoked"
// @Failure 422 {object} map[string]any "expected_user_mismatch"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Header 200,400,422 {string} Skein-Push-Contract "2 — push REST contract v2"
// @Router /ui/v1/push/subscriptions [get]
func (h *PushHandler) List(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := pushCaller(w, r)
	if !ok {
		return
	}
	if raw := r.URL.Query().Get("expectedUserId"); r.URL.Query().Has("expectedUserId") {
		expected, ok := parseExpectedUser(w, raw)
		if !ok {
			return
		}
		if !sameUser(w, r, c, expected, "list") {
			return
		}
	}
	subs, err := h.store.ListForUser(ctx, c.userID)
	if err != nil {
		slog.ErrorContext(ctx, "push list failed",
			"userId", c.userID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	out := make([]push.SubscriptionDTO, 0, len(subs))
	for _, s := range subs {
		out = append(out, toSubscriptionDTO(s))
	}
	writeJSON(w, http.StatusOK, out)
}
