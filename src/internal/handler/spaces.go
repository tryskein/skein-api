package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"skein-api/internal/spaces"
)

// SpacesHandler owns the spaces REST surface of T023 (sk-674.18): the
// caller's spaces, creating, renaming and deleting a space, its members
// and the owner. The contract — routes, bodies, codes — is
// spaces/contract.go; the decisions are spaces.Service.
type SpacesHandler struct {
	svc *spaces.Service
}

// NewSpacesHandler wraps the service.
func NewSpacesHandler(svc *spaces.Service) *SpacesHandler {
	return &SpacesHandler{svc: svc}
}

// spacesCaller is the caller as the service names him.
func spacesCaller(r *http.Request) (spaces.Caller, bool) {
	c, ok := caller(r)
	if !ok {
		return spaces.Caller{}, false
	}
	return spaces.Caller{
		UserID:   c.UserID,
		Username: c.Username,
		Actor:    spaces.Actor{UserID: c.UserID, Username: c.Username, Type: "human"},
	}, true
}

// writeSpacesError writes a refusal of the code registry, or logs and
// answers 500 for anything else.
func writeSpacesError(w http.ResponseWriter, r *http.Request, op string, err error) {
	var ref *spaces.Refusal
	if errors.As(err, &ref) {
		writeProblem(w, ref.Status, ref.Code, ref.Message, ref.Extra)
		return
	}
	slog.ErrorContext(r.Context(), "space operation failed",
		"operation", op, "spaceId", chi.URLParam(r, "id"), "error", err.Error())
	writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
}

// userParam parses {userId}.
func userParam(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := parseUUIDParam(r, "userId")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_id", err.Error(), nil)
		return uuid.Nil, false
	}
	return id, true
}

// MySpaces handles GET /api/v1/me/spaces.
//
// @Summary List the caller's spaces
// @Description Every space the caller sees, whole, without pagination, in the order of the projects column: the personal space, the common space, then the rest by name. An ordinary user sees the spaces he is a member of; the instance admin (only while SKEIN_INSTANCE_ADMIN_ENABLED=true) also sees every other space except other users' personal ones, empty ones included, with myRole null and adminAccess true; an external user sees none.
// @Description
// @Description Each row carries the seats (memberCount, memberLimit — regular spaces only, pendingInvitations — valid pending invitations, which hold seats), the quotas (projectCount with archived projects, projectQuota, storageUsedBytes, storageQuotaBytes; null quota = none) and canCreateProject (members only; never with adminAccess).
// @Description
// @Description The response also carries the decision of POST /spaces for the caller now — canCreate and cannotCreateReason, the very code POST would return: external_user, space_creation_disabled (SKEIN_SPACE_CREATION=none, the admin included), admin_only (SKEIN_SPACE_CREATION=admin and the caller is not the admin), owned_limit (the caller owns SKEIN_SPACE_OWNED_LIMIT regular spaces; never for the admin) — and ownedCount / ownedLimit (the personal and the common space are not counted).
// @Description
// @Description Read-only; no side effects.
// @Tags Spaces
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} spaces.MySpacesResponse "The caller's spaces"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/me/spaces [get]
func (h *SpacesHandler) MySpaces(w http.ResponseWriter, r *http.Request) {
	c, ok := spacesCaller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	resp, err := h.svc.MySpaces(r.Context(), c)
	if err != nil {
		writeSpacesError(w, r, "my_spaces", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// Create handles POST /api/v1/spaces.
//
// @Summary Create a space
// @Description Creates a regular space and makes its owner its first member. The owner is the caller or — from the instance admin only — the user named by ownerId; an admin naming someone else does not become a member, and the owner gets the personal signal spaces with reason owner_assigned.
// @Description
// @Description Allowed by the same decision GET /me/spaces reports as canCreate, checked again here under the caller's row lock (two concurrent creations are counted one after the other; a caller blocked or deleted after his request was authenticated gets 401 session_revoked and nothing is created): 422 external_user (the caller is external), space_creation_disabled (SKEIN_SPACE_CREATION=none), admin_only (=admin and the caller is not the admin), owned_limit with ownedCount and ownedLimit (the caller is not the admin and owns SKEIN_SPACE_OWNED_LIMIT regular spaces). The limit is never checked for a space the admin creates, whoever its owner.
// @Description
// @Description Side effects: the space row, its owner membership, one space history event; the signal spaces to the caller and, for another owner, owner_assigned to him.
// @Tags Spaces
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body spaces.CreateSpaceRequest true "Name (1–200 characters, not unique) and, admin only, ownerId"
// @Success 201 {object} spaces.CreateSpaceResponse "The new space's short id and slug"
// @Failure 400 {object} map[string]any "invalid_json, validation_error (empty or too long name, malformed ownerId)"
// @Failure 401 {object} map[string]any "Missing or invalid auth; session_revoked — the caller was blocked or deleted meanwhile"
// @Failure 403 {object} map[string]any "forbidden: ownerId sent by a non-admin"
// @Failure 404 {object} map[string]any "user_not_found: the ownerId user does not exist or is not active"
// @Failure 422 {object} map[string]any "external_user, space_creation_disabled, admin_only, owned_limit (ownedCount, ownedLimit), concurrent_change"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/spaces [post]
func (h *SpacesHandler) Create(w http.ResponseWriter, r *http.Request) {
	c, ok := spacesCaller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	var req spaces.CreateSpaceRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	var owner *uuid.UUID
	if req.OwnerID != nil {
		id, err := uuid.Parse(*req.OwnerID)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "validation_error", "ownerId must be a UUID", nil)
			return
		}
		owner = &id
	}
	resp, err := h.svc.Create(r.Context(), c, req.Name, owner)
	if err != nil {
		writeSpacesError(w, r, "create", err)
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

// Get handles GET /api/v1/spaces/{id}.
//
// @Summary Get a space
// @Description The space by its short id with its members (owner first, then by username) and, for the owner and the instance admin, its pending and recently declined invitations (null for a member). canDelete / cannotDeleteReason report whether DELETE would be allowed now: personal_space, forbidden (not the owner nor the admin), common_space_not_empty, common_space_enabled.
// @Description
// @Description A space that does not exist and one the caller may not see (another user's personal space, a space he is not in while not the admin) answer the same 404 space_not_found (INV-3). Read-only.
// @Tags Spaces
// @Produce json
// @Security ApiKeyAuth
// @Param id path string true "Space short id (8 lowercase base32 characters)"
// @Success 200 {object} spaces.SpaceDetail "The space"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 404 {object} map[string]any "space_not_found"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/spaces/{id} [get]
func (h *SpacesHandler) Get(w http.ResponseWriter, r *http.Request) {
	c, ok := spacesCaller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	d, err := h.svc.Get(r.Context(), c, chi.URLParam(r, "id"))
	if err != nil {
		writeSpacesError(w, r, "get", err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// Rename handles PATCH /api/v1/spaces/{id}.
//
// @Summary Rename a space
// @Description Renames the space (the owner and the instance admin). A personal space is never renamed. Side effects: every member gets the personal signal spaces with reason renamed and the new name. Answers the space as GET does.
// @Tags Spaces
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param id path string true "Space short id"
// @Param body body spaces.RenameSpaceRequest true "The new name (1–200 characters)"
// @Success 200 {object} spaces.SpaceDetail "The renamed space"
// @Failure 400 {object} map[string]any "invalid_json, validation_error (empty or too long name)"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 403 {object} map[string]any "forbidden: a member who is not the owner"
// @Failure 404 {object} map[string]any "space_not_found"
// @Failure 422 {object} map[string]any "personal_space, concurrent_change"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/spaces/{id} [patch]
func (h *SpacesHandler) Rename(w http.ResponseWriter, r *http.Request) {
	c, ok := spacesCaller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	var req spaces.RenameSpaceRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	d, err := h.svc.Rename(r.Context(), c, chi.URLParam(r, "id"), req.Name)
	if err != nil {
		writeSpacesError(w, r, "rename", err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// Delete handles DELETE /api/v1/spaces/{id}.
//
// @Summary Delete a space
// @Description Deletes the space irreversibly (the owner and the instance admin): in one transaction every project of it (archived included) with everything in them, its members and its pending invitations (revoked); attachment objects are removed later. The typed confirmName is compared with the name on the server. The common space is deleted only when it has no project and SKEIN_COMMON_SPACE_ENABLED=false; a personal space only with its account. The conditions are checked under the space's lock.
// @Description
// @Description Side effects: every member gets the personal signal spaces with reason deleted, and everyone who saw a project of it — access_changed with the projects lost.
// @Tags Spaces
// @Accept json
// @Security ApiKeyAuth
// @Param id path string true "Space short id"
// @Param body body spaces.DeleteSpaceRequest true "The space name, typed by the person"
// @Success 204 "Deleted"
// @Failure 400 {object} map[string]any "invalid_json"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 403 {object} map[string]any "forbidden: not the owner nor the admin"
// @Failure 404 {object} map[string]any "space_not_found"
// @Failure 422 {object} map[string]any "name_mismatch, personal_space, common_space_not_empty, common_space_enabled, concurrent_change"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/spaces/{id} [delete]
func (h *SpacesHandler) Delete(w http.ResponseWriter, r *http.Request) {
	c, ok := spacesCaller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	var req spaces.DeleteSpaceRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	if err := h.svc.Delete(r.Context(), c, chi.URLParam(r, "id"), req.ConfirmName); err != nil {
		writeSpacesError(w, r, "delete", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DeletionPreview handles GET /api/v1/spaces/{id}/deletion-preview.
//
// @Summary Preview a space deletion
// @Description What DELETE would destroy, counted now (the owner and the instance admin): projects with archived ones, their issues, attachment bytes, pending invitations to revoke, and the owner's username. Read-only.
// @Tags Spaces
// @Produce json
// @Security ApiKeyAuth
// @Param id path string true "Space short id"
// @Success 200 {object} spaces.SpaceDeletionPreview "The consequences"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 403 {object} map[string]any "forbidden: not the owner nor the admin"
// @Failure 404 {object} map[string]any "space_not_found"
// @Failure 422 {object} map[string]any "personal_space"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/spaces/{id}/deletion-preview [get]
func (h *SpacesHandler) DeletionPreview(w http.ResponseWriter, r *http.Request) {
	c, ok := spacesCaller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	p, err := h.svc.DeletionPreview(r.Context(), c, chi.URLParam(r, "id"))
	if err != nil {
		writeSpacesError(w, r, "deletion_preview", err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// RemovalPreview handles GET /api/v1/spaces/{id}/members/{userId}/removal-preview.
//
// @Summary Preview a member's removal or leaving
// @Description The consequences of removing the member (the owner and the instance admin) or of the caller leaving ({userId} = the caller): open issues of the space that lose him as assignee, claims of him and his agents released, and the projects of the space he owns (they stay under the space owner). The numbers are obtained by running the removal itself in a transaction that is rolled back, so they equal what the removal then does. Nothing is changed and no signal is sent.
// @Tags Spaces
// @Produce json
// @Security ApiKeyAuth
// @Param id path string true "Space short id"
// @Param userId path string true "Member user id (UUID)"
// @Success 200 {object} spaces.MemberRemovalPreview "The consequences"
// @Failure 400 {object} map[string]any "invalid_id"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 403 {object} map[string]any "forbidden: another member's preview asked by a plain member"
// @Failure 404 {object} map[string]any "space_not_found"
// @Failure 422 {object} map[string]any "not_a_member, owner_cannot_leave, owner_cannot_be_removed, personal_space, concurrent_change"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/spaces/{id}/members/{userId}/removal-preview [get]
func (h *SpacesHandler) RemovalPreview(w http.ResponseWriter, r *http.Request) {
	c, ok := spacesCaller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	userID, ok := userParam(w, r)
	if !ok {
		return
	}
	p, err := h.svc.RemovalPreview(r.Context(), c, chi.URLParam(r, "id"), userID)
	if err != nil {
		writeSpacesError(w, r, "removal_preview", err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// RemoveMember handles DELETE /api/v1/spaces/{id}/members/{userId}.
//
// @Summary Remove a member
// @Description Removes the member from the space (the owner and the instance admin; never the owner). In one transaction: his memberships in every project of the space (both sources) go, and for every project he no longer sees his open assignments (with an event naming the reason), his and his agents' claims, his watches and reminders go (INV-12). History stays.
// @Description
// @Description Side effects: the removed user gets the personal signal spaces with reason removed and access_changed with the projects and the space lost.
// @Tags Spaces
// @Security ApiKeyAuth
// @Param id path string true "Space short id"
// @Param userId path string true "Member user id (UUID)"
// @Success 204 "Removed"
// @Failure 400 {object} map[string]any "invalid_id"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 403 {object} map[string]any "forbidden: not the owner nor the admin"
// @Failure 404 {object} map[string]any "space_not_found"
// @Failure 422 {object} map[string]any "owner_cannot_be_removed, not_a_member, personal_space, concurrent_change"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/spaces/{id}/members/{userId} [delete]
func (h *SpacesHandler) RemoveMember(w http.ResponseWriter, r *http.Request) {
	c, ok := spacesCaller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	userID, ok := userParam(w, r)
	if !ok {
		return
	}
	if err := h.svc.RemoveMember(r.Context(), c, chi.URLParam(r, "id"), userID); err != nil {
		writeSpacesError(w, r, "remove", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Leave handles POST /api/v1/spaces/{id}/leave.
//
// @Summary Leave a space
// @Description The caller leaves the space (any member but the owner) with the same consequences as a removal (INV-12), in one transaction. Side effects: access_changed to the caller with the projects lost, and the space lost unless he still sees it as the instance admin.
// @Tags Spaces
// @Security ApiKeyAuth
// @Param id path string true "Space short id"
// @Success 204 "Left"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 404 {object} map[string]any "space_not_found"
// @Failure 422 {object} map[string]any "owner_cannot_leave, not_a_member (the admin seeing it without membership), personal_space, concurrent_change"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/spaces/{id}/leave [post]
func (h *SpacesHandler) Leave(w http.ResponseWriter, r *http.Request) {
	c, ok := spacesCaller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	if err := h.svc.Leave(r.Context(), c, chi.URLParam(r, "id")); err != nil {
		writeSpacesError(w, r, "leave", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AddMember handles POST /api/v1/spaces/{id}/members.
//
// @Summary Add a member at once (admin)
// @Description The instance admin makes the user a member at once, without consent; the admin may add himself. A pending invitation of the user to this space is closed as accepted and its link stops working. SKEIN_SPACE_MEMBER_LIMIT is checked for a regular space under the space's lock — members plus valid pending invitations, the user's own invitation not counted twice — so of two additions for the last seat one is refused.
// @Description
// @Description Side effects: the user gets memberships in every project of the space with visibility space, the personal signal spaces with reason added (no Inbox card) and access_changed with the projects gained.
// @Tags Spaces
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param id path string true "Space short id"
// @Param body body spaces.AddMemberRequest true "The user to add"
// @Success 201 {object} spaces.SpaceMember "The new member"
// @Failure 400 {object} map[string]any "invalid_json, validation_error (malformed userId)"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 403 {object} map[string]any "not_admin"
// @Failure 404 {object} map[string]any "space_not_found, user_not_found"
// @Failure 422 {object} map[string]any "external_user, already_member, no_seats (memberCount, pendingInvitations, memberLimit), personal_space, concurrent_change"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/spaces/{id}/members [post]
func (h *SpacesHandler) AddMember(w http.ResponseWriter, r *http.Request) {
	c, ok := spacesCaller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	var req spaces.AddMemberRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	userID, err := uuid.Parse(req.UserID)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "validation_error", "userId must be a UUID", nil)
		return
	}
	m, err := h.svc.AddMember(r.Context(), c, chi.URLParam(r, "id"), userID)
	if err != nil {
		writeSpacesError(w, r, "admin_add", err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

// ChangeOwner handles POST /api/v1/spaces/{id}/owner.
//
// @Summary Change the owner of a space (admin)
// @Description The instance admin makes a member the owner. The previous owner stays a member; SKEIN_SPACE_OWNED_LIMIT is not checked; pending invitations are untouched. The new owner becomes the implicit owner of every project of the space; the previous one loses the projects he saw only as the owner, with their assignments, claims, watches and reminders (INV-12). Naming the current owner changes nothing.
// @Description
// @Description Side effects: one space history event; the new owner gets the personal signal spaces with reason owner_assigned, every other member owner_changed; access_changed to whoever gained or lost projects.
// @Tags Spaces
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param id path string true "Space short id"
// @Param body body spaces.ChangeOwnerRequest true "The new owner, a member of the space"
// @Success 200 {object} spaces.SpaceDetail "The space with its new owner"
// @Failure 400 {object} map[string]any "invalid_json, validation_error (malformed userId)"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 403 {object} map[string]any "not_admin"
// @Failure 404 {object} map[string]any "space_not_found"
// @Failure 422 {object} map[string]any "not_a_member, personal_space, concurrent_change"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/spaces/{id}/owner [post]
func (h *SpacesHandler) ChangeOwner(w http.ResponseWriter, r *http.Request) {
	c, ok := spacesCaller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	var req spaces.ChangeOwnerRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	userID, err := uuid.Parse(req.UserID)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "validation_error", "userId must be a UUID", nil)
		return
	}
	d, err := h.svc.ChangeOwner(r.Context(), c, chi.URLParam(r, "id"), userID)
	if err != nil {
		writeSpacesError(w, r, "owner_change", err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// userIDParam parses {id} of the /users/{id}/spaces routes.
func userIDParam(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := parseUUIDParam(r, "id")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_id", err.Error(), nil)
		return uuid.Nil, false
	}
	return id, true
}

// UserSpaces handles GET /api/v1/users/{id}/spaces.
//
// @Summary A user's spaces (admin)
// @Description The user's participations outside personal spaces, with the role, in the order of the projects column. The rows with role owner are what the deactivation dialog names ("Owns: … — change the owner"): deactivation keeps them, the owner is changed in the space's settings. Read-only. Admin only (403 not_admin otherwise; the right is read from the database on every request).
// @Tags Spaces
// @Produce json
// @Security ApiKeyAuth
// @Param id path string true "User UUID"
// @Success 200 {object} spaces.UserSpacesResponse "The user's spaces"
// @Failure 400 {object} map[string]any "invalid_id"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 403 {object} map[string]any "not_admin"
// @Failure 404 {object} map[string]any "user_not_found"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/users/{id}/spaces [get]
func (h *SpacesHandler) UserSpaces(w http.ResponseWriter, r *http.Request) {
	c, ok := spacesCaller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	userID, ok := userIDParam(w, r)
	if !ok {
		return
	}
	resp, err := h.svc.UserSpaces(r.Context(), c, userID)
	if err != nil {
		writeSpacesError(w, r, "user_spaces", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// PutUserSpaces handles PUT /api/v1/users/{id}/spaces.
//
// @Summary Set a user's spaces (admin)
// @Description Makes the user's set of spaces (personal ones aside) exactly spaceIds, in one transaction: each added space makes him a participant at once, without consent (signal spaces added, seats checked — no_seats names the space); each removed one is a removal with its consequences (his memberships in its projects, his open assignments, claims, watches and reminders there — US-7). A space the user owns cannot be removed until its owner is changed (owner_cannot_be_removed with spaceId). An external user gets no space. Answers the saved set. Admin only.
// @Tags Spaces
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param id path string true "User UUID"
// @Param body body spaces.PutUserSpacesRequest true "The whole wanted set (space short ids)"
// @Success 200 {object} spaces.UserSpacesResponse "The saved set"
// @Failure 400 {object} map[string]any "invalid_id, invalid_json"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 403 {object} map[string]any "not_admin"
// @Failure 404 {object} map[string]any "user_not_found, space_not_found"
// @Failure 422 {object} map[string]any "owner_cannot_be_removed (spaceId), no_seats (spaceId, memberCount, pendingInvitations, memberLimit), external_user, personal_space, concurrent_change"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/users/{id}/spaces [put]
func (h *SpacesHandler) PutUserSpaces(w http.ResponseWriter, r *http.Request) {
	c, ok := spacesCaller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	userID, ok := userIDParam(w, r)
	if !ok {
		return
	}
	var req spaces.PutUserSpacesRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	resp, err := h.svc.PutUserSpaces(r.Context(), c, userID, req.SpaceIDs)
	if err != nil {
		writeSpacesError(w, r, "put_user_spaces", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// PreviewUserSpaces handles POST /api/v1/users/{id}/spaces/preview.
//
// @Summary Preview setting a user's spaces (admin)
// @Description What PUT with the same body would do: one entry per space the user would leave, with the consequences — open issues losing him as assignee, claims released, the projects of the space he owns. The numbers come from running the removals in a transaction that is rolled back, so they equal the fact; nothing is changed and no signal is sent. The same refusals as PUT. Admin only.
// @Tags Spaces
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param id path string true "User UUID"
// @Param body body spaces.PutUserSpacesRequest true "The whole wanted set (space short ids)"
// @Success 200 {object} spaces.UserSpacesPreview "The removals and their consequences"
// @Failure 400 {object} map[string]any "invalid_id, invalid_json"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 403 {object} map[string]any "not_admin"
// @Failure 404 {object} map[string]any "user_not_found, space_not_found"
// @Failure 422 {object} map[string]any "owner_cannot_be_removed (spaceId), external_user, personal_space, concurrent_change"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/users/{id}/spaces/preview [post]
func (h *SpacesHandler) PreviewUserSpaces(w http.ResponseWriter, r *http.Request) {
	c, ok := spacesCaller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	userID, ok := userIDParam(w, r)
	if !ok {
		return
	}
	var req spaces.PutUserSpacesRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	resp, err := h.svc.PreviewUserSpaces(r.Context(), c, userID, req.SpaceIDs)
	if err != nil {
		writeSpacesError(w, r, "preview_user_spaces", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
