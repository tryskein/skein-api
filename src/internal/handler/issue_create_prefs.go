package handler

// issue_create_prefs.go — sk-565 (C-REST, D8): GET and PUT
// /api/v1/me/issue-create-prefs, the caller's personal «assign the issues my
// agent creates to me» setting. The storage and the factory value live in
// internal/notifications (issue_create_prefs.go); this file decodes the body
// strictly, maps the outcome onto codes and nothing else.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"skein-api/internal/notifications"
)

// putIssueCreatePrefsRequest is the PUT /api/v1/me/issue-create-prefs body.
//
// The update is PARTIAL: an absent field keeps its stored value, and a body
// naming nothing writes nothing. json.RawMessage, not *bool, because a
// pointer collapses «key absent» and «null arrived» into one nil, and the
// two must get different answers: absent — unchanged, null — 400. The
// swaggertype tag names for the spec generator the shape that travels the
// wire (it cannot resolve json.RawMessage on its own).
type putIssueCreatePrefsRequest struct {
	// AssignAgentIssuesToMe switches the default on or off: when true, an
	// issue the caller's agent creates over MCP without an assignee is
	// assigned to the caller. A boolean; absent — unchanged; null or any
	// other type is rejected.
	AssignAgentIssuesToMe json.RawMessage `json:"assignAgentIssuesToMe,omitempty" swaggertype:"boolean" example:"false"`
}

// errIssueCreatePrefsBodyNotObject is the body-shape refusal for a JSON
// value that is not an object. The literal null needs it explicitly:
// encoding/json decodes null into a struct without an error, and it would
// otherwise pass as «{}».
var errIssueCreatePrefsBodyNotObject = errors.New("request body must be a JSON object")

// issueCreatePrefsAssignKey is the one key the PUT body may carry, spelled
// exactly: encoding/json matches struct fields case-insensitively, so the
// spelling is checked separately (see decodeIssueCreatePrefsBody).
const issueCreatePrefsAssignKey = "assignAgentIssuesToMe"

// decodeIssueCreatePrefsBody reads the body and returns the requested change.
// Every refusal is a body-shape error (400 invalid_json): an empty or
// malformed body, anything but whitespace after the first JSON value, a body
// that is not an object, an unknown key (a different spelling of
// assignAgentIssuesToMe included), and assignAgentIssuesToMe as null or as a
// non-boolean.
//
// The body is read whole rather than through the shared decodeJSON: that one
// decodes the first value and leaves the rest unread (and partly buffered
// inside its decoder), so a trailing value would pass unnoticed.
func decodeIssueCreatePrefsBody(r *http.Request) (notifications.UpdateIssueCreatePrefsInput, error) {
	raw, err := readSingleJSONValue(r)
	if err != nil {
		return notifications.UpdateIssueCreatePrefsInput{}, err
	}
	if raw[0] != '{' {
		return notifications.UpdateIssueCreatePrefsInput{}, errIssueCreatePrefsBodyNotObject
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return notifications.UpdateIssueCreatePrefsInput{}, fmt.Errorf("invalid JSON: %w", err)
	}
	for k := range keys {
		if k != issueCreatePrefsAssignKey {
			return notifications.UpdateIssueCreatePrefsInput{}, fmt.Errorf("invalid JSON: unknown field %q", k)
		}
	}
	var req putIssueCreatePrefsRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return notifications.UpdateIssueCreatePrefsInput{}, fmt.Errorf("invalid JSON: %w", err)
	}
	if len(req.AssignAgentIssuesToMe) == 0 {
		return notifications.UpdateIssueCreatePrefsInput{}, nil
	}
	if bytes.Equal(bytes.TrimSpace(req.AssignAgentIssuesToMe), []byte("null")) {
		return notifications.UpdateIssueCreatePrefsInput{}, errors.New("assignAgentIssuesToMe must be a boolean, not null")
	}
	var on bool
	if err := json.Unmarshal(req.AssignAgentIssuesToMe, &on); err != nil {
		return notifications.UpdateIssueCreatePrefsInput{}, errors.New("assignAgentIssuesToMe must be a boolean")
	}
	return notifications.UpdateIssueCreatePrefsInput{AssignAgentIssuesToMe: &on}, nil
}

// readSingleJSONValue reads the whole body (capped at requestBodyMaxBytes)
// and returns its one JSON value with surrounding whitespace trimmed. An
// empty body, an oversized body, malformed JSON and anything after the first
// value are errors; the returned value is never empty.
func readSingleJSONValue(r *http.Request) (json.RawMessage, error) {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, requestBodyMaxBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, errBodyTooLarge
		}
		return nil, fmt.Errorf("reading request body: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	var raw json.RawMessage
	switch err := dec.Decode(&raw); {
	case errors.Is(err, io.EOF):
		return nil, errors.New("request body is empty")
	case err != nil:
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	var tail json.RawMessage
	if err := dec.Decode(&tail); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid JSON: request body must hold exactly one JSON value")
	}
	return bytes.TrimSpace(raw), nil
}

// GetIssueCreatePrefs handles GET /api/v1/me/issue-create-prefs.
//
// @Summary Get the caller's issue-create settings — assign agent-created issues to me
// @Description Returns the calling user's personal setting assignAgentIssuesToMe (sk-565): whether an issue that the caller's own agent creates over MCP (skein_create, including a subtask via parentId, and skein_epic_create — the epic and every child) without an assignee is assigned to the caller, the owner of the API key. Standing issues and issues created under someone else's parent are never assigned by this setting, and an explicit assignee or unassigned:true in the MCP call always wins.
// @Description
// @Description Algorithm: read the caller's row in user_notification_prefs. The row is created lazily on the first save of any notification setting, so for someone who never saved one there is no row and the answer is the factory value — true. Reading never creates the row. Only the caller's own setting is readable here; the route grants and changes no project rights.
// @Description
// @Description Side effects: none — read-only. The setting affects MCP issue creation only; REST issue creation (the web UI) never reads it.
// @Tags Me
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} notifications.IssueCreatePrefsDTO "The caller's issue-create settings"
// @Failure 401 {object} map[string]any "unauthenticated — missing or invalid session / API key"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Router /api/v1/me/issue-create-prefs [get]
func (h *NotificationsHandler) GetIssueCreatePrefs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	dto, err := h.store.GetIssueCreatePrefs(ctx, c.UserID)
	if err != nil {
		slog.ErrorContext(ctx, "get issue-create prefs failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// PutIssueCreatePrefs handles PUT /api/v1/me/issue-create-prefs.
//
// @Summary Update the caller's issue-create settings (partial)
// @Description Applies a PARTIAL update of the caller's issue-create settings and returns the full settings, the same payload GET returns, so the client renders the result without a second call.
// @Description
// @Description Algorithm: decode the body strictly — it must be exactly one JSON object whose only allowed key is assignAgentIssuesToMe (spelled exactly), a boolean. When the key is present, one upsert writes that column and nothing else, creating the caller's user_notification_prefs row when there is none; the other columns of a newly created row take their factory defaults, so no other personal setting changes its effective value. Concurrent writes of different personal settings both survive; of two concurrent writes of this flag the one committed last wins. A body naming no key ({}) writes nothing, does not create the row, and returns the current value.
// @Description
// @Description Side effects: none beyond the caller's own row. The setting affects MCP issue creation only (skein_create, skein_epic_create): with it on, an issue the caller's agent creates without an assignee is assigned to the caller and accepted, except standing issues and issues under someone else's parent. REST issue creation (the web UI) never reads it. Existing issues are not touched.
// @Description
// @Description Errors: a body that is empty, not valid JSON, followed by anything but whitespace after its first JSON value (a second value, trailing text), not an object (an array, a string, a number, the literal null), carries an unknown key (the key is matched exactly — another letter case, such as AssignAgentIssuesToMe, is an unknown key), or sends assignAgentIssuesToMe as null or as a non-boolean (for example "false" or 0) is 400 invalid_json, and nothing is written.
// @Tags Me
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body putIssueCreatePrefsRequest true "Settings to change; an absent field is left unchanged"
// @Success 200 {object} notifications.IssueCreatePrefsDTO "The caller's issue-create settings after the update"
// @Failure 400 {object} map[string]any "invalid_json — empty or malformed body, trailing data after the JSON value, body not an object, unknown key (case-sensitive), assignAgentIssuesToMe null or not a boolean; nothing was written"
// @Failure 401 {object} map[string]any "unauthenticated — missing or invalid session / API key"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Router /api/v1/me/issue-create-prefs [put]
func (h *NotificationsHandler) PutIssueCreatePrefs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	in, err := decodeIssueCreatePrefsBody(r)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	dto, err := h.store.UpdateIssueCreatePrefs(ctx, c.UserID, in)
	if err != nil {
		slog.ErrorContext(ctx, "update issue-create prefs failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}
