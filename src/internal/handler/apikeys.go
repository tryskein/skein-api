package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/authz"
)

// apiKeyCreateMaxAttempts caps the prefix-collision retry loop in Create and
// Reissue. 12 base64-url chars ≈ 71 bits → collisions are astronomical;
// three attempts is well past any conceivable rate. After this many
// failures we surface 500 to the user (and they can retry the API call).
const apiKeyCreateMaxAttempts = 3

// APIKeysSettings is the instance configuration the API keys page needs:
// the lifetime policy of new keys and whether and where the public API is
// served.
type APIKeysSettings struct {
	TTL              apikeys.TTLPolicy
	PublicAPIEnabled bool
	// PublicAPIURL is the external address of the public API; empty when
	// not configured.
	PublicAPIURL string
}

// APIKeysHandler implements the /ui/v1/me/api-keys/* endpoints: the
// signed-in user's own keys (US-10).
//
// Routing wires this under RequireInternalSecret + ParseUserHeaders +
// RequireUser, so every method is guaranteed a valid userID in ctx. A
// request authenticated by an API key alone never reaches it: no
// operation by a key creates, reissues or revokes a key (INV-3).
//
// Every endpoint is user-scoped: a user can only see and change their own
// keys. Admin-side cross-user management is not surfaced here.
type APIKeysHandler struct {
	pool       *pgxpool.Pool
	store      *apikeys.Store
	bcryptCost int
	settings   APIKeysSettings
}

// NewAPIKeysHandler builds the handler with the given bcrypt cost and
// instance settings. Build once at startup; reuse across requests.
func NewAPIKeysHandler(pool *pgxpool.Pool, bcryptCost int, settings APIKeysSettings) *APIKeysHandler {
	return &APIKeysHandler{
		pool:       pool,
		store:      apikeys.New(pool),
		bcryptCost: bcryptCost,
		settings:   settings,
	}
}

// apiKeyLabelMax is the longest label, the same as F243's "human-readable
// label".
const apiKeyLabelMax = 100

// Key statuses and the reasons of an inactive key in apiKeyView.
const (
	apiKeyStatusActive   = "active"
	apiKeyStatusInactive = "inactive"
	apiKeyReasonRevoked  = "revoked"
	apiKeyReasonExpired  = "expired"
)

// Rules of a refused grant, in details.rule of 422 invalid_grant.
const (
	grantRuleNoPermission     = "no_permission"
	grantRuleAdminNeedsAll    = "admin_requires_scope_all"
	grantRuleAdminUnavailable = "admin_not_available"
	grantRuleInvalidScope     = "invalid_scope"
	grantRuleScopeUnavailable = "scope_unavailable"
	grantRuleExpiry           = "expiry_out_of_range"
	grantRuleNotNarrowing     = "not_narrowing"
)

// apiKeyPermissions is the permission part of a grant. Level is read,
// write, manage or null (a key with only the admin permission).
type apiKeyPermissions struct {
	// Level: read ⊂ write ⊂ manage; null when the key has only admin.
	Level *string `json:"level" enums:"read,write,manage"`
	// Admin is the instance-admin permission, allowed only with scope all.
	Admin bool `json:"admin"`
}

// apiKeyScopeMember is one project or space of a key's scope.
type apiKeyScopeMember struct {
	// ID of the member: the project UUID, or the space short id.
	ID string `json:"id"`
	// Name of the project or space.
	Name string `json:"name"`
}

// apiKeyScopeView is a key's scope in the list.
type apiKeyScopeView struct {
	// Mode: projects, spaces or all.
	Mode string `json:"mode" enums:"projects,spaces,all"`
	// Projects of a projects scope; empty otherwise.
	Projects []apiKeyScopeMember `json:"projects"`
	// Spaces of a spaces scope; empty otherwise.
	Spaces []apiKeyScopeMember `json:"spaces"`
}

// apiKeyView is one row of the API keys list. It never contains the secret.
type apiKeyView struct {
	// ID of the key.
	ID string `json:"id"`
	// Label chosen by the owner.
	Label string `json:"label"`
	// Prefix is the non-secret start of the key, the form the page shows.
	Prefix string `json:"prefix"`
	// CreatedAt — when the key was created.
	CreatedAt time.Time `json:"createdAt"`
	// ExpiresAt — end of the lifetime; null only on a key created before lifetimes existed.
	ExpiresAt *time.Time `json:"expiresAt"`
	// LastUsedAt — last authenticated use (granularity one minute); null if never used.
	LastUsedAt *time.Time `json:"lastUsedAt"`
	// RevokedAt — when the key was revoked; null for a key not revoked.
	RevokedAt *time.Time `json:"revokedAt"`
	// Status: active or inactive.
	Status string `json:"status" enums:"active,inactive"`
	// StatusReason of an inactive key: revoked or expired; null for an active one.
	StatusReason *string `json:"statusReason" enums:"revoked,expired"`
	// Permissions the key grants.
	Permissions apiKeyPermissions `json:"permissions"`
	// Scope of the key.
	Scope apiKeyScopeView `json:"scope"`
	// UnavailablePermissions — permissions the key grants that its owner
	// does not have now (the owner's role went down): currently only admin.
	UnavailablePermissions []string `json:"unavailablePermissions"`
	// ReissuedFromID — the key this one was reissued from; null otherwise.
	ReissuedFromID *string `json:"reissuedFromId"`
}

func toAPIKeyView(k apikeys.APIKey, now time.Time) apiKeyView {
	v := apiKeyView{
		ID:                     k.ID.String(),
		Label:                  k.Label,
		Prefix:                 k.Prefix,
		CreatedAt:              k.CreatedAt,
		ExpiresAt:              k.Grant.ExpiresAt,
		LastUsedAt:             k.LastUsedAt,
		RevokedAt:              k.RevokedAt,
		Status:                 apiKeyStatusActive,
		Permissions:            apiKeyPermissions{Admin: k.Grant.Admin},
		Scope:                  apiKeyScopeView{Mode: string(k.Grant.Scope.Mode), Projects: []apiKeyScopeMember{}, Spaces: []apiKeyScopeMember{}},
		UnavailablePermissions: []string{},
	}
	if k.Grant.Level != apikeys.LevelNone {
		level := string(k.Grant.Level)
		v.Permissions.Level = &level
	}
	reason := ""
	switch {
	case k.RevokedAt != nil:
		reason = apiKeyReasonRevoked
	case k.Grant.ExpiresAt != nil && !k.Grant.ExpiresAt.After(now):
		reason = apiKeyReasonExpired
	}
	if reason != "" {
		v.Status = apiKeyStatusInactive
		v.StatusReason = &reason
	}
	for _, id := range k.Grant.Scope.Projects {
		v.Scope.Projects = append(v.Scope.Projects, apiKeyScopeMember{ID: id.String(), Name: k.ProjectNames[id]})
	}
	for _, id := range k.Grant.Scope.Spaces {
		v.Scope.Spaces = append(v.Scope.Spaces, apiKeyScopeMember{ID: k.SpaceShortIDs[id], Name: k.SpaceNames[id]})
	}
	if k.Grant.Admin && !k.EffectiveAdmin {
		v.UnavailablePermissions = append(v.UnavailablePermissions, "admin")
	}
	if k.ReissuedFromID != nil {
		from := k.ReissuedFromID.String()
		v.ReissuedFromID = &from
	}
	return v
}

// apiKeyScopeInput is the scope of a create or narrow request.
type apiKeyScopeInput struct {
	// Mode: projects, spaces or all.
	Mode string `json:"mode" enums:"projects,spaces,all"`
	// Projects (ids) of a projects scope; omitted otherwise.
	Projects []uuid.UUID `json:"projects"`
	// Spaces (short ids) of a spaces scope; omitted otherwise.
	Spaces []string `json:"spaces"`
}

// createAPIKeyRequest is the JSON body for POST /me/api-keys. Label,
// permissions and scope are required; expiresAt is optional.
type createAPIKeyRequest struct {
	// Label, 1..100 characters after trimming.
	Label string `json:"label"`
	// Permissions: a level, the admin flag, or both.
	Permissions *apiKeyPermissions `json:"permissions"`
	// Scope of the key.
	Scope *apiKeyScopeInput `json:"scope"`
	// ExpiresAt — end of the lifetime, RFC 3339; after now and within the maximum lifetime. Omitted: the default lifetime of GET /settings from now.
	ExpiresAt *time.Time `json:"expiresAt"`
}

// narrowAPIKeyRequest is the JSON body for PATCH /me/api-keys/{id}: the new
// grant, which may only narrow the current one.
type narrowAPIKeyRequest struct {
	// Permissions: not above the current ones.
	Permissions *apiKeyPermissions `json:"permissions"`
	// Scope: a subset of the current one.
	Scope *apiKeyScopeInput `json:"scope"`
	// ExpiresAt — not later than the current expiry and in the future; omitted keeps the current expiry.
	ExpiresAt *time.Time `json:"expiresAt"`
}

// createAPIKeyResponse is the ONLY place the plain key is ever returned. The
// caller must store it immediately — the list never carries it.
type createAPIKeyResponse struct {
	apiKeyView
	// PlainKey — the secret, shown once.
	PlainKey string `json:"plainKey"`
}

// apiKeysSettingsView is the body of GET /me/api-keys/settings.
type apiKeysSettingsView struct {
	// PublicAPIEnabled — whether this instance serves the public API.
	PublicAPIEnabled bool `json:"publicApiEnabled"`
	// PublicAPIURL — the external address of the public API; null when the public API is disabled or its address is not configured.
	PublicAPIURL *string `json:"publicApiUrl"`
	// OpenAPIURL — the address of the public API's OpenAPI document; null exactly when publicApiUrl is null.
	OpenAPIURL *string `json:"openApiUrl"`
	// DefaultTTLSeconds — the lifetime of a new or reissued key when none is chosen.
	DefaultTTLSeconds int64 `json:"defaultTtlSeconds"`
	// MaxTTLSeconds — the longest lifetime a new key may be given.
	MaxTTLSeconds int64 `json:"maxTtlSeconds"`
}

// Create godoc
// @Summary Create an API key of the caller
// @Description Creates an API key for the signed-in user with the chosen permissions, scope and lifetime, and returns its secret once.
// @Description
// @Description Algorithm: decode the body; label, permissions and scope are required, expiresAt is optional. A body without permissions and scope is the shape of a page loaded before keys had grants: 400 validation_error with details.staleClient = true, and no key is created. Then the grant is checked — at least one permission (a level or admin), admin only with scope all and only for an instance admin, the scope members visible to the caller (scope projects are given by UUID, scope spaces by short id; a malformed, unknown or unreachable member answers 422 invalid_grant scope_unavailable), and an expiresAt, when given, after now and no later than now + the maximum lifetime of GET /settings. Without expiresAt the key lives the default lifetime of GET /settings. A fresh secret is generated (crypto/rand), bcrypt-hashed and stored with the grant; only its hash and non-secret prefix are kept.
// @Description
// @Description Side effects: one api_keys row and its scope rows. The secret appears only in this response.
// @Tags Me
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body createAPIKeyRequest true "Label, permissions, scope and optional expiry of the new key"
// @Success 201 {object} createAPIKeyResponse "The new key with its secret (plainKey), shown once"
// @Failure 400 {object} map[string]any "invalid_body — malformed JSON or an unknown field; label_required / label_too_long; validation_error — a required field is missing (details.field), an unknown level or scope mode, or the stale page shape (details.staleClient = true). Nothing is created."
// @Failure 401 {object} map[string]any "unauthenticated — no signed-in user"
// @Failure 422 {object} map[string]any "invalid_grant — the grant breaks a rule; details.rule is no_permission, admin_requires_scope_all, admin_not_available, invalid_scope, scope_unavailable or expiry_out_of_range. Nothing is created."
// @Failure 500 {object} map[string]any "internal_error — the key could not be generated or stored"
// @Router /ui/v1/me/api-keys [post]
func (h *APIKeysHandler) Create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := auth.UserIDFrom(ctx)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "sign in required", nil)
		return
	}

	var req createAPIKeyRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_body", err.Error(), nil)
		return
	}
	if req.Permissions == nil && req.Scope == nil {
		writeProblem(w, http.StatusBadRequest, "validation_error",
			"this page is out of date: refresh the page to create a key with permissions, scope and expiry",
			map[string]any{"details": map[string]any{"staleClient": true}})
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
	switch {
	case req.Permissions == nil:
		writeMissingField(w, "permissions")
		return
	case req.Scope == nil:
		writeMissingField(w, "scope")
		return
	}
	grant, ok := parseGrant(w, req.Permissions, req.Scope)
	if !ok {
		return
	}
	if !h.resolveScopeSpaces(w, r, userID, &grant, req.Scope.Spaces) {
		return
	}
	// Nil gives the policy default in the store.
	grant.ExpiresAt = req.ExpiresAt
	if !h.checkGrantForOwner(w, r, userID, grant) {
		return
	}

	var (
		plain   string
		created apikeys.Created
		err     error
	)
	for attempt := range apiKeyCreateMaxAttempts {
		var prefix, hash string
		plain, prefix, hash, err = auth.GenerateAPIKey(h.bcryptCost)
		if err != nil {
			slog.ErrorContext(ctx, "failed to generate api key", "userId", userID.String(), "error", err.Error())
			writeProblem(w, http.StatusInternalServerError, "internal_error", "failed to generate key", nil)
			return
		}
		created, err = h.store.Create(ctx, apikeys.NewKey{UserID: userID, Label: label, Prefix: prefix, Hash: hash, Grant: grant}, h.settings.TTL)
		if !isPrefixCollision(err) {
			break
		}
		slog.WarnContext(ctx, "api key prefix collision, retrying", "userId", userID.String(), "attempt", attempt+1)
	}
	if err != nil {
		if writeGrantError(w, err) {
			return
		}
		slog.ErrorContext(ctx, "failed to insert api key", "userId", userID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "failed to create key", nil)
		return
	}

	slog.InfoContext(ctx, "api key created",
		"userId", userID.String(),
		"keyId", created.ID.String(),
		"masked", auth.MaskAPIKey(plain),
	)
	h.writeKeyWithSecret(w, r, userID, created.ID, plain)
}

// List godoc
// @Summary List the caller's API keys
// @Description Returns every key of the signed-in user — active, expired and revoked — newest first, as an array (empty when there are none).
// @Description
// @Description Each row carries the non-secret prefix, creation time, expiry, last use, status (active / inactive) with its reason (revoked / expired), permissions, scope with member names, and unavailablePermissions: the permissions the key grants that its owner does not have now (admin after the owner stopped being an instance admin). The secret is never returned.
// @Description
// @Description Side effects: none — read-only.
// @Tags Me
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {array} apiKeyView "The caller's keys"
// @Failure 401 {object} map[string]any "unauthenticated — no signed-in user"
// @Failure 500 {object} map[string]any "internal_error — the keys could not be read"
// @Router /ui/v1/me/api-keys [get]
func (h *APIKeysHandler) List(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := auth.UserIDFrom(ctx)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "sign in required", nil)
		return
	}

	keys, err := h.store.ListByUser(ctx, userID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list api keys", "userId", userID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "failed to list keys", nil)
		return
	}
	now := time.Now()
	out := make([]apiKeyView, 0, len(keys))
	for _, k := range keys {
		out = append(out, toAPIKeyView(k, now))
	}
	writeJSON(w, http.StatusOK, out)
}

// Narrow godoc
// @Summary Narrow an API key of the caller
// @Description Replaces the grant of one of the caller's keys with a narrower one, in place: the level no higher, admin not added, the scope a subset of the current one, the expiry no later (and in the future). Widening is possible only with a new key.
// @Description
// @Description Algorithm: decode the body (permissions and scope required; expiresAt omitted keeps the current expiry), check that the caller reaches every project (UUID) and space (short id) of the new scope (a malformed, unknown or hidden member answers the same as a missing one: 422 invalid_grant scope_unavailable), check the grant rules as on create, then narrow under a row lock. While the key has admin its scope stays all. A refused change leaves the key unchanged.
// @Description
// @Description Side effects: the key's grant and scope rows change; requests with the key see the new grant from the next request on.
// @Tags Me
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param id path string true "Key id (UUID)"
// @Param body body narrowAPIKeyRequest true "The narrower grant"
// @Success 200 {object} apiKeyView "The key after the change"
// @Failure 400 {object} map[string]any "invalid_id; invalid_body; validation_error — a required field is missing (details.field), an unknown level or scope mode"
// @Failure 401 {object} map[string]any "unauthenticated — no signed-in user"
// @Failure 404 {object} map[string]any "not_found — no such key of the caller"
// @Failure 422 {object} map[string]any "invalid_grant (details.rule: not_narrowing, no_permission, admin_requires_scope_all, invalid_scope, scope_unavailable, expiry_out_of_range); key_revoked; key_expired. The key is unchanged."
// @Failure 500 {object} map[string]any "internal_error — the change could not be stored"
// @Router /ui/v1/me/api-keys/{id} [patch]
func (h *APIKeysHandler) Narrow(w http.ResponseWriter, r *http.Request) {
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
	var req narrowAPIKeyRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_body", err.Error(), nil)
		return
	}
	if req.Permissions == nil {
		writeMissingField(w, "permissions")
		return
	}
	if req.Scope == nil {
		writeMissingField(w, "scope")
		return
	}
	next, ok := parseGrant(w, req.Permissions, req.Scope)
	if !ok {
		return
	}
	cur, err := h.store.GetByUser(ctx, userID, id)
	if errors.Is(err, apikeys.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "not_found", "api key not found", nil)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "failed to read api key", "userId", userID.String(), "keyId", id.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "failed to narrow key", nil)
		return
	}
	next.ExpiresAt = req.ExpiresAt
	if next.ExpiresAt == nil {
		next.ExpiresAt = cur.Grant.ExpiresAt
	}
	if !h.resolveScopeSpaces(w, r, userID, &next, req.Scope.Spaces) {
		return
	}
	// The new scope must be one the owner reaches now, as on create: a
	// narrowing never tells a hidden project from a missing one.
	if !h.checkScopeForOwner(w, r, userID, next.Scope) {
		return
	}

	err = h.store.Narrow(ctx, userID, id, next)
	switch {
	case err == nil:
	case errors.Is(err, apikeys.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "api key not found", nil)
		return
	case writeGrantError(w, err):
		return
	default:
		slog.ErrorContext(ctx, "failed to narrow api key", "userId", userID.String(), "keyId", id.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "failed to narrow key", nil)
		return
	}
	slog.InfoContext(ctx, "api key narrowed", "userId", userID.String(), "keyId", id.String())
	k, err := h.store.GetByUser(ctx, userID, id)
	if err != nil {
		slog.ErrorContext(ctx, "failed to read narrowed api key", "userId", userID.String(), "keyId", id.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "failed to read key", nil)
		return
	}
	writeJSON(w, http.StatusOK, toAPIKeyView(k, time.Now()))
}

// Reissue godoc
// @Summary Reissue an API key of the caller
// @Description Creates a new key that replaces one of the caller's keys: a new record and a new secret, shown once, with the same label, permissions and scope, and a lifetime of the default of GET /settings from now — also when the old key had no expiry.
// @Description
// @Description The old key is not changed: it keeps working until its own expiry or until it is revoked. A revoked key is not reissued; an expired one may be, and stays expired.
// @Description
// @Description Side effects: one new api_keys row (reissuedFromId = the old key) and its scope rows.
// @Tags Me
// @Produce json
// @Security ApiKeyAuth
// @Param id path string true "Key id (UUID) of the key to reissue"
// @Success 201 {object} createAPIKeyResponse "The new key with its secret (plainKey), shown once"
// @Failure 400 {object} map[string]any "invalid_id — the id is not a UUID"
// @Failure 401 {object} map[string]any "unauthenticated — no signed-in user"
// @Failure 404 {object} map[string]any "not_found — no such key of the caller"
// @Failure 422 {object} map[string]any "key_revoked — a revoked key is not reissued; nothing is created"
// @Failure 500 {object} map[string]any "internal_error — the key could not be generated or stored"
// @Router /ui/v1/me/api-keys/{id}/reissue [post]
func (h *APIKeysHandler) Reissue(w http.ResponseWriter, r *http.Request) {
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
	var (
		plain   string
		created apikeys.Created
	)
	for attempt := range apiKeyCreateMaxAttempts {
		var prefix, hash string
		plain, prefix, hash, err = auth.GenerateAPIKey(h.bcryptCost)
		if err != nil {
			slog.ErrorContext(ctx, "failed to generate api key", "userId", userID.String(), "error", err.Error())
			writeProblem(w, http.StatusInternalServerError, "internal_error", "failed to generate key", nil)
			return
		}
		created, err = h.store.Reissue(ctx, userID, id, prefix, hash, h.settings.TTL)
		if !isPrefixCollision(err) {
			break
		}
		slog.WarnContext(ctx, "api key prefix collision, retrying", "userId", userID.String(), "attempt", attempt+1)
	}
	switch {
	case err == nil:
	case errors.Is(err, apikeys.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "api key not found", nil)
		return
	case writeGrantError(w, err):
		return
	default:
		slog.ErrorContext(ctx, "failed to reissue api key", "userId", userID.String(), "keyId", id.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "failed to reissue key", nil)
		return
	}
	slog.InfoContext(ctx, "api key reissued",
		"userId", userID.String(),
		"keyId", created.ID.String(),
		"fromKeyId", id.String(),
		"masked", auth.MaskAPIKey(plain),
	)
	h.writeKeyWithSecret(w, r, userID, created.ID, plain)
}

// Revoke godoc
// @Summary Revoke an API key of the caller
// @Description Revokes one of the caller's keys: it stops authenticating from the next request on and never works again. The row stays, shown as inactive with reason revoked.
// @Description
// @Description 404 when the id is unknown, belongs to another user or is already revoked — the three are one answer, so key ids of other users cannot be probed. Other keys are not touched.
// @Tags Me
// @Security ApiKeyAuth
// @Param id path string true "Key id (UUID)"
// @Success 204 "Revoked"
// @Failure 400 {object} map[string]any "invalid_id — the id is not a UUID"
// @Failure 401 {object} map[string]any "unauthenticated — no signed-in user"
// @Failure 404 {object} map[string]any "not_found — no active key of the caller with this id"
// @Failure 500 {object} map[string]any "internal_error — the revocation could not be stored"
// @Router /ui/v1/me/api-keys/{id} [delete]
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

// Settings godoc
// @Summary Settings of the API keys page
// @Description Tells the API keys page whether this instance serves the public API, where (publicApiUrl and the address of its OpenAPI document, both from SKEIN_PUBLIC_API_URL) and the lifetime policy of new keys.
// @Description
// @Description Three states are distinct: publicApiEnabled false (both addresses null); enabled without a configured address (both null — the page says the address is not set); enabled with an address (both set).
// @Description
// @Description Side effects: none — read-only.
// @Tags Me
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} apiKeysSettingsView "Public API availability and key lifetime policy"
// @Failure 401 {object} map[string]any "unauthenticated — no signed-in user"
// @Router /ui/v1/me/api-keys/settings [get]
func (h *APIKeysHandler) Settings(w http.ResponseWriter, r *http.Request) {
	if _, ok := auth.UserIDFrom(r.Context()); !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "sign in required", nil)
		return
	}
	v := apiKeysSettingsView{
		PublicAPIEnabled:  h.settings.PublicAPIEnabled,
		DefaultTTLSeconds: int64(h.settings.TTL.Default / time.Second),
		MaxTTLSeconds:     int64(h.settings.TTL.Max / time.Second),
	}
	if h.settings.PublicAPIEnabled && h.settings.PublicAPIURL != "" {
		base := h.settings.PublicAPIURL
		v.PublicAPIURL = &base
		if doc, err := url.JoinPath(base, "api", "v1", "openapi.json"); err == nil {
			v.OpenAPIURL = &doc
		} else {
			// config.Load validated the address; a failure here is a defect.
			slog.ErrorContext(r.Context(), "public api url cannot be joined", "error", err.Error())
			v.PublicAPIURL = nil
		}
	}
	writeJSON(w, http.StatusOK, v)
}

// writeKeyWithSecret answers 201 with the key row and its secret.
func (h *APIKeysHandler) writeKeyWithSecret(w http.ResponseWriter, r *http.Request, userID, id uuid.UUID, plain string) {
	k, err := h.store.GetByUser(r.Context(), userID, id)
	if err != nil {
		slog.ErrorContext(r.Context(), "failed to read new api key", "userId", userID.String(), "keyId", id.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "failed to read key", nil)
		return
	}
	writeJSON(w, http.StatusCreated, createAPIKeyResponse{apiKeyView: toAPIKeyView(k, time.Now()), PlainKey: plain})
}

// parseGrant turns the request's permissions and scope into a grant, or
// answers 400 validation_error for an unknown level or scope mode.
func parseGrant(w http.ResponseWriter, p *apiKeyPermissions, sc *apiKeyScopeInput) (apikeys.Grant, bool) {
	g := apikeys.Grant{Admin: p.Admin}
	if p.Level != nil {
		switch l := apikeys.Level(*p.Level); l {
		case apikeys.LevelRead, apikeys.LevelWrite, apikeys.LevelManage:
			g.Level = l
		default:
			writeProblem(w, http.StatusBadRequest, "validation_error", "permissions.level must be read, write, manage or null",
				map[string]any{"details": map[string]any{"field": "permissions.level"}})
			return apikeys.Grant{}, false
		}
	}
	switch m := apikeys.ScopeMode(sc.Mode); m {
	case apikeys.ScopeProjects, apikeys.ScopeSpaces, apikeys.ScopeAll:
		// Spaces arrive as short ids; resolveScopeSpaces fills them in.
		g.Scope = apikeys.Scope{Mode: m, Projects: sc.Projects}
	default:
		writeProblem(w, http.StatusBadRequest, "validation_error", "scope.mode must be projects, spaces or all",
			map[string]any{"details": map[string]any{"field": "scope.mode"}})
		return apikeys.Grant{}, false
	}
	return g, true
}

// checkGrantForOwner checks, before a key is created, the rules that depend
// on its owner: admin only for an internal instance admin, every scope
// project visible to the owner (the scope spaces are already the owner's:
// resolveScopeSpaces). A member out of reach answers the same as a missing
// one. Refusals answer 422 invalid_grant.
func (h *APIKeysHandler) checkGrantForOwner(w http.ResponseWriter, r *http.Request, userID uuid.UUID, g apikeys.Grant) bool {
	ctx := r.Context()
	if err := g.Validate(); err != nil {
		return !writeGrantError(w, err)
	}
	role, _ := auth.UserRoleFrom(ctx)
	ownerAdmin := role == "admin" && !auth.IsExternalFrom(ctx)
	if g.Admin && !ownerAdmin {
		writeInvalidGrant(w, grantRuleAdminUnavailable, "only an instance admin can give a key the admin permission")
		return false
	}
	return h.checkScopeForOwner(w, r, userID, g.Scope)
}

// checkScopeForOwner answers 422 invalid_grant / scope_unavailable unless the
// owner reaches every member of sc — on create and on narrowing alike, so a
// member out of reach answers the same as a missing one.
func (h *APIKeysHandler) checkScopeForOwner(w http.ResponseWriter, r *http.Request, userID uuid.UUID, sc apikeys.Scope) bool {
	ctx := r.Context()
	reachable, err := h.scopeReachable(ctx, sc)
	if err != nil {
		slog.ErrorContext(ctx, "failed to check api key scope", "userId", userID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "failed to check scope", nil)
		return false
	}
	if !reachable {
		writeInvalidGrant(w, grantRuleScopeUnavailable, "a project or space of the scope does not exist or is not available to you")
		return false
	}
	return true
}

// resolveScopeSpaces turns the requested space short ids into the spaces
// the owner belongs to (any space for an instance admin) and sets them on
// g. A malformed, unknown or unreachable short id answers 422
// invalid_grant / scope_unavailable — the same answer as a project out of
// reach, so a space the owner is not in cannot be told from a missing one.
func (h *APIKeysHandler) resolveScopeSpaces(w http.ResponseWriter, r *http.Request, userID uuid.UUID, g *apikeys.Grant, shortIDs []string) bool {
	if len(shortIDs) == 0 {
		return true
	}
	ctx := r.Context()
	role, _ := auth.UserRoleFrom(ctx)
	ownerAdmin := role == "admin" && !auth.IsExternalFrom(ctx)
	rows, err := h.pool.Query(ctx, `
SELECT s.id
  FROM unnest($1::text[]) AS x(short_id)
  LEFT JOIN spaces s ON s.short_id = x.short_id
                    AND ($3 OR EXISTS (SELECT 1 FROM space_members m
                                        WHERE m.space_id = s.id AND m.user_id = $2))`,
		shortIDs, userID, ownerAdmin)
	if err != nil {
		slog.ErrorContext(ctx, "failed to resolve api key scope spaces", "userId", userID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "failed to check scope", nil)
		return false
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[*uuid.UUID])
	if err != nil {
		slog.ErrorContext(ctx, "failed to resolve api key scope spaces", "userId", userID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "failed to check scope", nil)
		return false
	}
	g.Scope.Spaces = make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if id == nil {
			writeInvalidGrant(w, grantRuleScopeUnavailable, "a project or space of the scope does not exist or is not available to you")
			return false
		}
		g.Scope.Spaces = append(g.Scope.Spaces, *id)
	}
	return true
}

// scopeReachable reports whether the owner reaches every project of sc.
// Spaces need no check here: resolveScopeSpaces keeps only the spaces the
// owner reaches.
func (h *APIKeysHandler) scopeReachable(ctx context.Context, sc apikeys.Scope) (bool, error) {
	switch sc.Mode {
	case apikeys.ScopeProjects:
		visible, err := authz.VisibleAmong(ctx, h.pool, authz.FromContext(ctx), sc.Projects)
		if err != nil {
			return false, err
		}
		for _, p := range sc.Projects {
			if _, ok := visible[p]; !ok {
				return false, nil
			}
		}
	}
	return true, nil
}

// writeGrantError answers a grant or key-state error of the store and
// reports whether err was one.
func writeGrantError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, apikeys.ErrNoPermission):
		writeInvalidGrant(w, grantRuleNoPermission, "a key needs a level, the admin permission, or both")
	case errors.Is(err, apikeys.ErrAdminNeedsAll):
		writeInvalidGrant(w, grantRuleAdminNeedsAll, "a key with the admin permission must have scope all")
	case errors.Is(err, apikeys.ErrInvalidScope):
		writeInvalidGrant(w, grantRuleInvalidScope, "scope projects needs projects, scope spaces needs spaces, scope all lists none")
	case errors.Is(err, apikeys.ErrScopeUnavailable):
		writeInvalidGrant(w, grantRuleScopeUnavailable, "a project or space of the scope does not exist or is not available to you")
	case errors.Is(err, apikeys.ErrInvalidExpiry):
		writeInvalidGrant(w, grantRuleExpiry, "the expiry must be in the future and within the maximum key lifetime")
	case errors.Is(err, apikeys.ErrNotNarrowing):
		writeInvalidGrant(w, grantRuleNotNarrowing, "a key can only be narrowed; create a new key for wider permissions")
	case errors.Is(err, apikeys.ErrInvalidLevel):
		writeProblem(w, http.StatusBadRequest, "validation_error", "permissions.level must be read, write, manage or null",
			map[string]any{"details": map[string]any{"field": "permissions.level"}})
	case errors.Is(err, apikeys.ErrRevoked):
		writeProblem(w, http.StatusUnprocessableEntity, "key_revoked", "the key is revoked", nil)
	case errors.Is(err, apikeys.ErrExpired):
		writeProblem(w, http.StatusUnprocessableEntity, "key_expired", "the key has expired", nil)
	default:
		return false
	}
	return true
}

func writeInvalidGrant(w http.ResponseWriter, rule, msg string) {
	writeProblem(w, http.StatusUnprocessableEntity, "invalid_grant", msg,
		map[string]any{"details": map[string]any{"rule": rule}})
}

func writeMissingField(w http.ResponseWriter, field string) {
	writeProblem(w, http.StatusBadRequest, "validation_error", field+" is required",
		map[string]any{"details": map[string]any{"field": field}})
}

func isPrefixCollision(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
