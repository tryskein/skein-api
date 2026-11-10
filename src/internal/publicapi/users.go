package publicapi

// users.go — the user operations of the public API (spec sk-878.3.1, "API
// surface", T028): thin adapters over the operations of internal/issueops
// that the actions of the MCP tool skein_user also call (INV-4). They need
// the key right admin — a key with admin has scope all — and an internal
// instance admin as its owner. Through a key the password of an existing
// user, the admin role and an admin's deactivation are never changed
// (INV-12): updateUser takes no password field, and the store refuses the
// rest.

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"skein-api/internal/errcode"
	"skein-api/internal/issueops"
	"skein-api/internal/spaces"
	"skein-api/internal/users"
)

// The operationIds of the user operations.
const (
	OperationListUsers      = "listUsers"
	OperationCreateUser     = "createUser"
	OperationUpdateUser     = "updateUser"
	OperationAddSpaceMember = "addSpaceMember"
)

var usersLimit = Limit{Min: 1, Max: 100, Default: 50}

// The bodies of the user writes.
type (
	// createUserBody is the body of createUser.
	createUserBody struct {
		// Email, unique. Required.
		Email *string `json:"email,omitempty"`
		// Name. Required.
		Name *string `json:"name,omitempty"`
		// Password, the initial one, at least 8 characters. Required.
		Password *string `json:"password,omitempty"`
		// Role: user (an administrator is created only in the UI). Required.
		Role *string `json:"role,omitempty"`
		// Username, unique; omitted — derived from the email.
		Username *string `json:"username,omitempty"`
		// IsExternal makes an external collaborator.
		IsExternal *bool `json:"isExternal,omitempty"`
		// Kind of the write: directed or autonomous (checked).
		Kind *string `json:"kind,omitempty"`
	}
	// updateUserBody is the body of updateUser. There is no password: it
	// changes only in the UI.
	updateUserBody struct {
		// Name, the new display name.
		Name *string `json:"name,omitempty"`
		// Role: the admin role is neither given nor taken by a key.
		Role *string `json:"role,omitempty"`
		// Deactivated deactivates (true) or reactivates (false) the user;
		// when present the other fields are not applied. An administrator
		// is not deactivated by a key.
		Deactivated *bool `json:"deactivated,omitempty"`
		// Kind of the write: directed or autonomous (checked).
		Kind *string `json:"kind,omitempty"`
	}
	// addSpaceMemberBody is the body of addSpaceMember.
	addSpaceMemberBody struct {
		// UserID of the user to add. Required.
		UserID *string `json:"userId,omitempty"`
		// Kind of the write: directed or autonomous (checked).
		Kind *string `json:"kind,omitempty"`
	}
)

// SpaceMemberAdded is the answer of addSpaceMember.
type SpaceMemberAdded struct {
	// SpaceID is the space's short id.
	SpaceID string `json:"spaceId"`
	// Member is the participant added.
	Member spaces.SpaceMember `json:"member"`
}

// inPath answers f with the object of the path as its origin when f is
// code: the operation names that object in no member of its own.
func inPath(f *issueops.Failure, code errcode.Code, key, id string) *issueops.Failure {
	if f.Code != code.String() {
		return f
	}
	fields := map[string]any{key: id}
	for k, v := range f.Fields {
		fields[k] = v
	}
	return issueops.Fail(f.Code, f.Message, fields)
}

// listUsers lists the users of the instance.
//
// @Summary List users
// @Description Returns a page of the users of the instance — the same operation as the MCP tool skein_user, action list: archived=true lists the deactivated ones, search matches name, email and username. Keyset paging: pass nextCursor back as cursor while hasMore is true. Needs the key right admin and an internal instance admin as its owner. Read-only.
// @Tags Users
// @Produce json
// @Security SkeinApiKey[admin]
// @Param archived query bool false "List the deactivated users"
// @Param search query string false "Text to match"
// @Param limit query int false "Page size, 1..100 (default 50)"
// @Param cursor query string false "The nextCursor of the previous page"
// @Param fields query string false "Comma-separated fields of each item to return (plus id)"
// @Success 200 {object} List[users.UserDTO] "A page of users"
// @Failure 400 {object} ProblemDocument "Unknown parameter or field, limit out of range, a foreign cursor (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks admin (code insufficient_scope); the owner is not an instance admin (code not_admin)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/users [get]
// @ID listUsers
func (a *issueWrites) listUsers(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r, "archived", "search", "limit", "cursor") {
		return
	}
	limit, err := ParseLimit(r, usersLimit)
	if err != nil {
		WriteParseError(w, err)
		return
	}
	q := r.URL.Query()
	args := issueops.Args{"limit": limit, "search": q.Get("search")}
	switch v := q.Get("archived"); v {
	case "", "false":
	case "true":
		args["archived"] = true
	default:
		WriteParseError(w, errors.New("archived must be true or false"))
		return
	}
	if c := q.Get("cursor"); c != "" {
		if _, _, err := users.DecodeCursor(c); err != nil {
			WriteParseError(w, ErrBadCursor)
			return
		}
		args["cursor"] = c
	}
	res, f := issueops.UserList(r.Context(), a.ops, callerOf(r), args)
	if f != nil {
		writeFailure(w, f)
		return
	}
	out := List[users.UserDTO]{Items: res.Items, NextCursor: res.NextCursor, HasMore: res.HasMore}
	if out.Items == nil {
		out.Items = []users.UserDTO{}
	}
	WriteJSON(w, r, http.StatusOK, out)
}

// createUser creates a user.
//
// @Summary Create a user
// @Description Creates a user with an initial password — the same operation as the MCP tool skein_user, action create. The initial password is the one password a key may set (INV-12): whoever knows it can sign in to the UI as that user. A key never creates an administrator. Needs the key right admin and an internal instance admin as its owner. Side effects: the user and his personal space.
// @Tags Users
// @Accept json
// @Produce json
// @Security SkeinApiKey[admin]
// @Param body body createUserBody true "The user"
// @Success 201 {object} users.UserDTO "The user created"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks admin (code insufficient_scope); the owner is not an instance admin (code not_admin); an administrator by key (code forbidden)"
// @Failure 422 {object} ProblemDocument "A field is missing or invalid (code validation_error); the email or the username is taken (email_taken, username_taken); the admin role is disabled (admin_role_disabled)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/users [post]
// @ID createUser
func (a *issueWrites) createUser(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	var body createUserBody
	args, ok := decodeArgs(w, r, &body)
	if !ok || !kindOf(w, body.Kind) {
		return
	}
	delete(args, "kind")
	if v, present := args["isExternal"]; present {
		delete(args, "isExternal")
		args["is_external"] = v
	}
	dto, f := issueops.UserCreate(r.Context(), a.ops, callerOf(r), args)
	if f != nil {
		writeFailure(w, f)
		return
	}
	w.Header().Set("Location", PathPrefix+"users/"+dto.ID.String())
	WriteJSON(w, r, http.StatusCreated, dto)
}

// updateUser changes a user.
//
// @Summary Update a user
// @Description Changes the user of the path — the same operation as the MCP tool skein_user, action update: name and role, or deactivated (when present the other fields are not applied). Deactivating a user revokes his API keys; reactivating does not bring them back. Through a key the password is not changed (there is no such field: 400), the admin role is neither given nor taken and an administrator is not deactivated (403 forbidden) — that is done only in the UI. Needs the key right admin and an internal instance admin as its owner. Side effects: the user's events; on a deactivation the revocation of his keys.
// @Tags Users
// @Accept json
// @Produce json
// @Security SkeinApiKey[admin]
// @Param user path string true "The user's UUID"
// @Param body body updateUserBody true "The changes"
// @Success 200 {object} users.UserDTO "The user after the change"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field (password among them) or parameter, a kind outside directed|autonomous (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks admin (code insufficient_scope); the owner is not an instance admin (code not_admin); the admin role or an administrator's deactivation by key (code forbidden)"
// @Failure 404 {object} ProblemDocument "No such user (code user_not_found)"
// @Failure 422 {object} ProblemDocument "No change named or a value invalid (code validation_error); the last administrator (last_admin_cannot_demote)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/users/{user} [patch]
// @ID updateUser
func (a *issueWrites) updateUser(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	id := strings.TrimSpace(chi.URLParam(r, "user"))
	if _, err := uuid.Parse(id); err != nil {
		WriteProblem(w, Problem{Code: errcode.UserNotFound, Origin: OriginPath, Detail: "user not found",
			Extensions: map[string]any{"userId": id}})
		return
	}
	var body updateUserBody
	args, ok := decodeArgs(w, r, &body)
	if !ok || !kindOf(w, body.Kind) {
		return
	}
	delete(args, "kind")
	args["userId"] = id
	dto, f := issueops.UserUpdate(r.Context(), a.ops, callerOf(r), args)
	if f != nil {
		writeFailure(w, inPath(f, errcode.UserNotFound, "userId", id), id)
		return
	}
	WriteJSON(w, r, http.StatusOK, dto)
}

// addSpaceMember makes a user a participant of a space.
//
// @Summary Add a user to a space
// @Description Makes the user of the body a participant of the space of the path — the same operation as the MCP tool skein_user, action space_add; the refusals of the spaces service keep their codes (a personal space, an external user, no free seat, already a participant). Needs the key right admin and an internal instance admin as its owner. Side effects: the participation and its event.
// @Tags Users
// @Accept json
// @Produce json
// @Security SkeinApiKey[admin]
// @Param space path string true "The space's short id"
// @Param body body addSpaceMemberBody true "The user"
// @Success 201 {object} SpaceMemberAdded "The participant added"
// @Failure 400 {object} ProblemDocument "Malformed body, unknown field or parameter, a kind outside directed|autonomous (code validation_error)"
// @Failure 401 {object} ProblemDocument "No key, or the key is invalid, expired or revoked (code unauthenticated)"
// @Failure 403 {object} ProblemDocument "The key lacks admin (code insufficient_scope); the owner is not an instance admin (code not_admin)"
// @Failure 404 {object} ProblemDocument "No such space (code space_not_found)"
// @Failure 422 {object} ProblemDocument "userId is missing or not a UUID (code validation_error); the user is missing (user_not_found); the space is personal (personal_space), the user external (external_user), no seat (no_seats), already a participant (already_member)"
// @Failure 429 {object} ProblemDocument "Rate limited (code rate_limited, Retry-After)"
// @Failure 500 {object} ProblemDocument "Internal error (code internal_error)"
// @Failure 503 {object} ProblemDocument "The database is unavailable (code database_unavailable)"
// @Router /api/v1/spaces/{space}/members [post]
// @ID addSpaceMember
func (a *issueWrites) addSpaceMember(w http.ResponseWriter, r *http.Request) {
	if !CheckQuery(w, r) {
		return
	}
	var body addSpaceMemberBody
	args, ok := decodeArgs(w, r, &body)
	if !ok || !kindOf(w, body.Kind) {
		return
	}
	space := strings.TrimSpace(chi.URLParam(r, "space"))
	args = issueops.Args{"userId": args["userId"], "space": space}
	m, f := issueops.UserSpaceAdd(r.Context(), a.ops, callerOf(r), args)
	if f != nil {
		writeFailure(w, inPath(f, errcode.SpaceNotFound, "space", space), space)
		return
	}
	WriteJSON(w, r, http.StatusCreated, SpaceMemberAdded{SpaceID: space, Member: m})
}
