package handler

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/projects"
)

// hubAccessMaxBytes caps the body of POST /api/v1/hub/project-access. The
// caller is skein-web's periodic re-check, which sends every connection of a
// replica in one batch (up to HUB_MAX_PROJECTS_PER_CONNECTION project ids
// each), so the cap is far above the 1 MiB default of ordinary endpoints;
// it still bounds memory per request. An oversized body is answered 400
// invalid_request, like any other body the endpoint cannot read.
const hubAccessMaxBytes = 16 << 20

// HubAccessHandler serves the service-only project access check used by
// skein-web's SignalR hub (sk-622.21, US-9, INV-9). The wire contract lives in
// projects/access_contract.go.
type HubAccessHandler struct {
	pool *pgxpool.Pool
}

// NewHubAccessHandler constructs the handler.
func NewHubAccessHandler(pool *pgxpool.Pool) *HubAccessHandler {
	return &HubAccessHandler{pool: pool}
}

// ProjectAccess handles POST /api/v1/hub/project-access.
//
// Route wiring: RequireInternalSecret ONLY, outside the /api/v1 group that
// applies ParseUserHeaders — like /api/v1/auth/oidc/provision. The caller is
// skein-web acting for many users at once, not one logged-in user; whose
// access is checked is named in the body, and user trust-headers are neither
// required nor read.
//
// @Summary Batch check of users' access to projects (service only)
// @Description Service endpoint for skein-web's SignalR hub. Business meaning: before a hub connection joins the event group of a project, and in the periodic re-check of every connection (each 30 s), skein-web asks which of the declared projects each user may see right now, so a project's events reach only connections of users with access to it (INV-9).
// @Description
// @Description Algorithm: validate the body -> deduplicate each entry's projectIds keeping request order -> ONE database statement for the whole batch reads each user's current state (exists, deactivated_at, role, is_external) and the user's row set of the access relation (skein_project_access_set, package authz — the same rule as every point check) -> per entry split the requested projects into allowed / denied -> 200 with one result per entry, in request order.
// @Description
// @Description Visibility: an inactive user (unknown id or deactivated) sees nothing; everyone else sees what the access relation grants — today an internal admin (role admin and not external) every existing project, everyone else, an external admin included, the projects they are a member of. A project that does not exist is denied, indistinguishable from one the user cannot see.
// @Description
// @Description allVisible=true (the /activity "all projects visible to me" mode) adds visibleProjectIds — the user's whole visible set, ordered by id — for EVERY entry that asked, the internal admin included (contract version 2, projects/access_contract.go); [] for an inactive or unknown user. internalAdmin is the role only (active, role admin, not external): it grants nothing by itself, and skein-web must not derive a subscription from it. Absent fields: projectIds absent = nothing to check; visibleProjectIds absent = not asked.
// @Description
// @Description Authorization: the service secret header X-Skein-Internal-Secret only; user headers are ignored. Unknown JSON fields are ignored. Body limit 16 MiB; the batch budget is 4,000 entries per call (a maximal entry with 50 projects is about 2 KB), larger batches are split by the caller. Exactly one JSON value per body: trailing data is a 400. Side effects: none, a pure read.
// @Tags Hub
// @Accept json
// @Produce json
// @Security InternalSecretAuth
// @Param body body projects.ProjectAccessRequest true "Users and the projects to check for each"
// @Success 200 {object} projects.ProjectAccessResponse "One result per request entry, in request order"
// @Failure 400 {object} map[string]any "invalid_request: body is not JSON of the documented shape, users is missing, a userId is missing, or an id is not a UUID; also an oversized body"
// @Failure 401 {object} map[string]any "Service secret missing or wrong"
// @Failure 500 {object} map[string]any "internal_error: database failure"
// @Router /api/v1/hub/project-access [post]
func (h *HubAccessHandler) ProjectAccess(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	r.Body = http.MaxBytesReader(w, r.Body, hubAccessMaxBytes)
	defer r.Body.Close()

	var req projects.ProjectAccessRequest
	dec := json.NewDecoder(r.Body)
	err := dec.Decode(&req)
	if err == nil {
		// The body is ONE JSON value: anything after it but whitespace — a
		// second value, garbage, or padding past the size limit — is a
		// syntax error, not something to ignore silently.
		if _, tailErr := dec.Token(); !errors.Is(tailErr, io.EOF) {
			err = tailErr
			if err == nil {
				err = errors.New("unexpected data after the JSON body")
			}
		}
	}
	if err != nil {
		var maxErr *http.MaxBytesError
		msg := "body is not valid JSON of the documented shape"
		switch {
		case errors.As(err, &maxErr):
			msg = "body exceeds the 16 MiB limit"
		case errors.Is(err, io.EOF):
			msg = "body is empty"
		}
		writeProblem(w, http.StatusBadRequest, "invalid_request", msg, nil)
		return
	}
	if req.Users == nil {
		writeProblem(w, http.StatusBadRequest, "invalid_request", "users is required", nil)
		return
	}
	for _, q := range req.Users {
		if q.UserID == uuid.Nil {
			writeProblem(w, http.StatusBadRequest, "invalid_request", "every entry needs a userId", nil)
			return
		}
	}

	resp, err := projects.ResolveProjectAccess(ctx, h.pool, req)
	if err != nil {
		slog.ErrorContext(ctx, "hub project access check failed",
			"users", len(req.Users), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
