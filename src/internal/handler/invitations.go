package handler

// invitations.go — the REST surface of space invitations (sk-674.18 T040):
// sending, resending and revoking (the space owner and the admin), the
// addressee's Inbox answers, the link of the invitation email with a session
// (spaces/contract.go) and without one (accounts/contract.go: the anonymous
// view and signup by invitation, on AccountsHandler). The decisions are
// invitations.Service.
//
// INV-17: an agent does not send, accept or decline an invitation. Through
// MCP no tool does it (skein_accept refuses an invitation id); on REST a
// caller authenticated by an API key is refused with
// invitation_agent_forbidden on the routes that send or answer one.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"skein-api/internal/accounts"
	"skein-api/internal/auth"
	"skein-api/internal/invitations"
	"skein-api/internal/spaces"
)

// InvitationsHandler serves the invitation routes with a session.
type InvitationsHandler struct {
	svc *invitations.Service
}

// NewInvitationsHandler wraps the service.
func NewInvitationsHandler(svc *invitations.Service) *InvitationsHandler {
	return &InvitationsHandler{svc: svc}
}

// invitationsCaller is the caller, refused when he is an agent (API key) on
// a route a person must use (INV-17).
func invitationsCaller(w http.ResponseWriter, r *http.Request, personOnly bool) (spaces.Caller, bool) {
	c, ok := spacesCaller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return spaces.Caller{}, false
	}
	if _, agent := auth.APIKeyIDFrom(r.Context()); agent && personOnly {
		writeSpacesError(w, r, "invitation", invitations.RefusedAgent)
		return spaces.Caller{}, false
	}
	return c, true
}

// invitationParam parses {invId}.
func invitationParam(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := parseUUIDParam(r, "invId")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_id", err.Error(), nil)
		return uuid.Nil, false
	}
	return id, true
}

// Create handles POST /api/v1/spaces/{id}/invitations.
//
// @Summary Invite a person to a space
// @Description Business meaning: the space owner or the instance admin invites a person by email, or a person picked from the directory by userId (the directory never shows emails), to a regular or the common space (US-5, screens 2.4). The invited person accepts or declines in the web UI — from the Inbox or by the link of the invitation email.
// @Description
// @Description Algorithm, answers in this order: body (exactly one of email and userId, email of email shape, language ru|en) → 400 validation_error; the space not seen → 404 space_not_found (the same as a nonexistent id); a personal space → 422 personal_space; a member who does not manage it → 403 forbidden; the sender's sends (creations and resends, every space) in the last 24 hours reach SKEIN_SPACE_INVITES_PER_DAY → 422 daily_limit; with Turnstile configured and more than ten sends by the sender in the last 10 minutes, a missing turnstileToken → 422 captcha_required, an unverified one → 422 captcha_failed. Then under the space's lock: the person is external → 422 external_user; already a member → 200 outcome already_member, nothing created; no account (or a deleted one) and registration closed → 422 no_such_user (mail on) or email_disabled (mail off); a pending invitation of the email already exists → registration open: 200 sent (the same invitation, nothing new); closed: 200 already_pending with that invitation (the form offers resend); participants plus pending invitations reach SKEIN_SPACE_MEMBER_LIMIT (regular spaces) → 422 no_seats with memberCount, pendingInvitations, memberLimit; else the invitation is created, valid SKEIN_SPACE_INVITE_TTL.
// @Description
// @Description With registration open the answer for an email with and without an account is the same 200 {"outcome":"sent","invitation":null} — status, body and headers (SC-021) — and the same steps are taken.
// @Description
// @Description Side effects: the invitation row (it occupies a seat); one entry in the sender's journal of sends; mail on: an invitation email in the outgoing queue, in the language sent (the inviter's), with a one-time link; for an existing active account: a push (reason space_invitation) and the personal signals spaces (invited) and inbox — the card appears in the Inbox; the owner gets a spaces re-read.
// @Tags Spaces
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param id path string true "Space short id"
// @Param body body spaces.CreateInvitationRequest true "Email or userId, the inviter's language, turnstileToken after captcha_required"
// @Success 200 {object} spaces.CreateInvitationResponse "sent | already_member | already_pending"
// @Failure 400 {object} map[string]any "invalid_json, validation_error"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 403 {object} map[string]any "forbidden: a member who does not manage the space"
// @Failure 404 {object} map[string]any "space_not_found; user_not_found (userId of no active account)"
// @Failure 422 {object} map[string]any "personal_space, external_user, no_such_user, email_disabled, no_seats, daily_limit, captcha_required, captcha_failed, concurrent_change; invitation_agent_forbidden for an API-key caller (INV-17)"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/spaces/{id}/invitations [post]
func (h *InvitationsHandler) Create(w http.ResponseWriter, r *http.Request) {
	c, ok := invitationsCaller(w, r, true)
	if !ok {
		return
	}
	var req spaces.CreateInvitationRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	resp, err := h.svc.Create(r.Context(), c, chi.URLParam(r, "id"), req, loginClientIP(r))
	if err != nil {
		writeSpacesError(w, r, "invite", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// Resend handles POST /api/v1/spaces/{id}/invitations/{invId}/resend.
//
// @Summary Send an invitation again
// @Description Business meaning: the owner or the admin re-sends a pending invitation that was not answered (screens 2.4). The invitation gets a new expiry (SKEIN_SPACE_INVITE_TTL from now); mail on, a new email with a new link is queued in the language stored at creation, and every earlier link of the invitation stops working.
// @Description
// @Description Algorithm, answers in this order: the space as for creating (404 space_not_found, 422 personal_space, 403 forbidden); then the send limits, before the invitation is looked up (an abuse guard comes before the business checks, like a rate limit): the caller's sends in the last 24 hours reach SKEIN_SPACE_INVITES_PER_DAY → 422 daily_limit (a resend is a send of the caller, whoever sent the invitation first); a turnstileToken sent and not verified → 422 captcha_failed (checked whenever a token comes); with Turnstile configured, ten or more sends by the caller in the last 10 minutes and no token → 422 captcha_required; then, under the space's lock: the invitation not in this space → 404 invitation_not_found; not pending or expired → 422 invitation_not_pending; the daily limit and the captcha requirement read again under the caller's lock (a concurrent send may have filled the window) → 422 daily_limit / captcha_required.
// @Description
// @Description Side effects: the new expiry; one entry in the caller's journal of sends; the email (earlier unsent ones cancelled); for an existing active account a push and the signals spaces (invited) and inbox.
// @Tags Spaces
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param id path string true "Space short id"
// @Param invId path string true "Invitation id (UUID)"
// @Param body body spaces.ResendInvitationRequest false "Optional: turnstileToken after captcha_required"
// @Success 200 {object} spaces.SpaceInvitation "The renewed invitation"
// @Failure 400 {object} map[string]any "invalid_id, invalid_json"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 403 {object} map[string]any "forbidden"
// @Failure 404 {object} map[string]any "space_not_found, invitation_not_found"
// @Failure 422 {object} map[string]any "personal_space, invitation_not_pending, daily_limit, captcha_required, captcha_failed, concurrent_change; invitation_agent_forbidden for an API-key caller"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/spaces/{id}/invitations/{invId}/resend [post]
func (h *InvitationsHandler) Resend(w http.ResponseWriter, r *http.Request) {
	c, ok := invitationsCaller(w, r, true)
	if !ok {
		return
	}
	id, ok := invitationParam(w, r)
	if !ok {
		return
	}
	var req spaces.ResendInvitationRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	resp, err := h.svc.Resend(r.Context(), c, chi.URLParam(r, "id"), id, req.TurnstileToken, loginClientIP(r))
	if err != nil {
		writeSpacesError(w, r, "resend_invitation", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// Revoke handles DELETE /api/v1/spaces/{id}/invitations/{invId}.
//
// @Summary Revoke an invitation
// @Description Business meaning: the owner or the admin withdraws a pending invitation — its seat is freed, its links stop working, unsent emails are cancelled and the card leaves the addressee's Inbox — or removes a declined one ("not accepted") from the list.
// @Description
// @Description Algorithm: the space as for creating (404, personal_space, forbidden); the invitation not in this space, or neither pending nor declined → 404 invitation_not_found.
// @Description
// @Description Side effects: the invitation becomes revoked; the addressee's account gets inbox and spaces re-reads; the owner and the sender a spaces re-read.
// @Tags Spaces
// @Security ApiKeyAuth
// @Param id path string true "Space short id"
// @Param invId path string true "Invitation id (UUID)"
// @Success 204 "Revoked"
// @Failure 400 {object} map[string]any "invalid_id"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 403 {object} map[string]any "forbidden"
// @Failure 404 {object} map[string]any "space_not_found, invitation_not_found"
// @Failure 422 {object} map[string]any "personal_space, concurrent_change"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/spaces/{id}/invitations/{invId} [delete]
func (h *InvitationsHandler) Revoke(w http.ResponseWriter, r *http.Request) {
	c, ok := invitationsCaller(w, r, false)
	if !ok {
		return
	}
	id, ok := invitationParam(w, r)
	if !ok {
		return
	}
	if err := h.svc.Revoke(r.Context(), c, chi.URLParam(r, "id"), id); err != nil {
		writeSpacesError(w, r, "revoke_invitation", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Mine handles GET /api/v1/me/invitations.
//
// @Summary The pending invitations to me
// @Description Business meaning: the invitation cards of the caller's Inbox (screens 1.3). Every pending, unexpired invitation to the caller's email into a space he is not in yet, newest first, with canAccept: false with cannotAcceptReason email_unconfirmed while mail is on and his email is not confirmed. Never the token or the link. Read-only.
// @Tags Me
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} spaces.MyInvitationsResponse "The pending invitations"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/me/invitations [get]
func (h *InvitationsHandler) Mine(w http.ResponseWriter, r *http.Request) {
	c, ok := invitationsCaller(w, r, false)
	if !ok {
		return
	}
	resp, err := h.svc.Mine(r.Context(), c)
	if err != nil {
		writeSpacesError(w, r, "my_invitations", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// AcceptMine handles POST /api/v1/me/invitations/{invId}/accept.
//
// @Summary Accept an invitation from the Inbox
// @Description Business meaning: the invited person joins the space (US-5): its space-visible projects become his, the space appears among his spaces.
// @Description
// @Description Algorithm: not an invitation to the caller's email (or none) → 404 invitation_not_found; its space deleted → 422 space_deleted; mail on and the caller's email not confirmed → 422 email_unconfirmed (the link of the invitation email confirms it); then under the space's lock: no longer pending or expired (revoked meanwhile — E-3: exactly one of the two wins) → 422 invitation_invalid. The seat limit is not checked again: the seat was taken when the invitation was sent (K14).
// @Description
// @Description Side effects: the invitation accepted, its links and unsent emails retired; membership with the space's projects and one space event; signals: the caller spaces (added) and inbox, the sender spaces (accepted) and a push (reason space_invitation), the owner a spaces re-read.
// @Tags Me
// @Produce json
// @Security ApiKeyAuth
// @Param invId path string true "Invitation id (UUID)"
// @Success 200 {object} spaces.AcceptInvitationResponse "The space joined"
// @Failure 400 {object} map[string]any "invalid_id"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 404 {object} map[string]any "invitation_not_found"
// @Failure 422 {object} map[string]any "space_deleted, email_unconfirmed, invitation_invalid, external_user, concurrent_change; invitation_agent_forbidden for an API-key caller (INV-17)"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/me/invitations/{invId}/accept [post]
func (h *InvitationsHandler) AcceptMine(w http.ResponseWriter, r *http.Request) {
	c, ok := invitationsCaller(w, r, true)
	if !ok {
		return
	}
	id, ok := invitationParam(w, r)
	if !ok {
		return
	}
	resp, err := h.svc.AcceptMine(r.Context(), c, id)
	if err != nil {
		writeSpacesError(w, r, "accept_invitation", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// DeclineMine handles POST /api/v1/me/invitations/{invId}/decline.
//
// @Summary Decline an invitation from the Inbox
// @Description Business meaning: the invited person declines; the space owner and the admin see a neutral "not accepted" for 7 days. No confirmed email is needed to decline.
// @Description
// @Description Algorithm: as accepting, without the email check: 404 invitation_not_found, 422 space_deleted, 422 invitation_invalid.
// @Description
// @Description Side effects: the invitation declined (its seat freed), its links and unsent emails retired; signals: the caller inbox and spaces, the owner and the sender spaces (declined).
// @Tags Me
// @Security ApiKeyAuth
// @Param invId path string true "Invitation id (UUID)"
// @Success 204 "Declined"
// @Failure 400 {object} map[string]any "invalid_id"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 404 {object} map[string]any "invitation_not_found"
// @Failure 422 {object} map[string]any "space_deleted, invitation_invalid, concurrent_change; invitation_agent_forbidden for an API-key caller"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/me/invitations/{invId}/decline [post]
func (h *InvitationsHandler) DeclineMine(w http.ResponseWriter, r *http.Request) {
	c, ok := invitationsCaller(w, r, true)
	if !ok {
		return
	}
	id, ok := invitationParam(w, r)
	if !ok {
		return
	}
	if err := h.svc.DeclineMine(r.Context(), c, id); err != nil {
		writeSpacesError(w, r, "decline_invitation", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// View handles GET /api/v1/invitations/{token}.
//
// @Summary The invitation link, logged in
// @Description Business meaning: what `/invite/:token` shows a logged-in user (screens 2.5). Every state is a 200, checked in this order: space_deleted (its space was deleted) → already_member (the caller is in the space; space named) → invalid (unknown, expired, revoked or used) → other_email (the invitation is for another address; the address is not shown) → valid (space, inviter, expiry: the accept screen).
// @Description
// @Description Side effects: opened by the invited person while the link is valid, it confirms his email (the link proves the mailbox) and sends him the signal inbox. It never accepts — accepting is POST …/accept.
// @Tags Spaces
// @Produce json
// @Security ApiKeyAuth
// @Param token path string true "The link token"
// @Success 200 {object} spaces.TokenInvitationView "The link's state"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/invitations/{token} [get]
func (h *InvitationsHandler) View(w http.ResponseWriter, r *http.Request) {
	c, ok := invitationsCaller(w, r, false)
	if !ok {
		return
	}
	resp, err := h.svc.View(r.Context(), c, chi.URLParam(r, "token"))
	if err != nil {
		writeSpacesError(w, r, "invitation_link", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// AcceptByToken handles POST /api/v1/invitations/{token}/accept.
//
// @Summary Accept an invitation by its link
// @Description Business meaning: the explicit acceptance on the screen of the invitation link (screens 2.5). The link proves the mailbox: the caller's email is confirmed with it, so no confirmation is required here.
// @Description
// @Description Algorithm: unknown token → 422 invitation_invalid; the space deleted → 422 space_deleted; under the space's lock the token is used: expired, revoked or used → 422 invitation_invalid (accepting after a revocation — "no longer valid", E-3); the caller's email is not the invited one → 422 invitation_other_email. A refusal leaves the link unused. The seat limit is not checked again (K14).
// @Description
// @Description Side effects: the token used, the other links and unsent emails retired; the email confirmed; then as accepting from the Inbox (membership, signals, the push to the sender).
// @Tags Spaces
// @Produce json
// @Security ApiKeyAuth
// @Param token path string true "The link token"
// @Success 200 {object} spaces.AcceptInvitationResponse "The space joined"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 422 {object} map[string]any "invitation_invalid, space_deleted, invitation_other_email, external_user, concurrent_change; invitation_agent_forbidden for an API-key caller (INV-17)"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/invitations/{token}/accept [post]
func (h *InvitationsHandler) AcceptByToken(w http.ResponseWriter, r *http.Request) {
	c, ok := invitationsCaller(w, r, true)
	if !ok {
		return
	}
	resp, err := h.svc.AcceptByToken(r.Context(), c, chi.URLParam(r, "token"))
	if err != nil {
		writeSpacesError(w, r, "accept_invitation_link", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// DeclineByToken handles POST /api/v1/invitations/{token}/decline.
//
// @Summary Decline an invitation by its link
// @Description Business meaning: "Decline" on the screen of the invitation link. Algorithm and refusals as accepting by the link; the token is used, the invitation declined (a neutral "not accepted" for the owner, 7 days).
// @Description
// @Description Side effects: as declining from the Inbox; the email confirmed (the link proves the mailbox).
// @Tags Spaces
// @Security ApiKeyAuth
// @Param token path string true "The link token"
// @Success 204 "Declined"
// @Failure 401 {object} map[string]any "Missing or invalid auth"
// @Failure 422 {object} map[string]any "invitation_invalid, space_deleted, invitation_other_email, concurrent_change; invitation_agent_forbidden for an API-key caller"
// @Failure 500 {object} map[string]any "Internal error"
// @Router /api/v1/invitations/{token}/decline [post]
func (h *InvitationsHandler) DeclineByToken(w http.ResponseWriter, r *http.Request) {
	c, ok := invitationsCaller(w, r, true)
	if !ok {
		return
	}
	if err := h.svc.DeclineByToken(r.Context(), c, chi.URLParam(r, "token")); err != nil {
		writeSpacesError(w, r, "decline_invitation_link", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// InvitationPreview handles GET /api/v1/auth/invitations/{token}.
//
// @Summary The invitation link, without a session (service only, for skein-web)
// @Description Business meaning: what `/invite/:token` shows a visitor who is not logged in (screens 2.5): whether to log in to accept or to register by the invitation.
// @Description
// @Description Algorithm: the api-side rate limit per link and per client IP (X-Skein-Client-Ip) → 429 rate_limited. Every state is a 200, in this order: space_deleted → invalid (unknown, expired, revoked, used) → valid with the space, the inviter's username, the invited email and hasAccount (an account, not deleted, has the email). The fields after status are null unless valid: a dead link reveals nothing.
// @Description
// @Description Side effects: none — opening the link without a session confirms nothing and accepts nothing. Authorization: the service secret only.
// @Tags Auth
// @Produce json
// @Param token path string true "The link token"
// @Param X-Skein-Client-Ip header string false "The real client address stamped by skein-web"
// @Success 200 {object} accounts.InvitationPreview "The link's state"
// @Failure 401 {object} map[string]any "Service secret missing or wrong"
// @Failure 429 {object} map[string]any "rate_limited"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/auth/invitations/{token} [get]
func (h *AccountsHandler) InvitationPreview(w http.ResponseWriter, r *http.Request) {
	resp, err := h.svc.InvitationPreview(r.Context(), chi.URLParam(r, "token"), loginClientIP(r))
	if err != nil {
		writeAccountsError(w, r, "invitation-preview", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// InvitationSignup handles POST /api/v1/auth/invitations/{token}/signup.
//
// @Summary Register by an invitation (service only, for skein-web)
// @Description Business meaning: a visitor without an account registers from the invitation link (screens 2.7) and joins the invitation's space at once; skein-web then logs them in with the sessionEpoch of the answer and opens the space. Only while SKEIN_SIGNUP_ENABLED=true. The email is the invitation's and is confirmed: no confirmation email is sent.
// @Description
// @Description Algorithm, refusals checked in this order: registration closed → 422 signup_disabled; body not one JSON value, username or password missing, body over 16 KiB → 400 invalid_request; the api-side rate limit per link and per client IP → 429 rate_limited; Turnstile configured and the token missing or not verified → 422 captcha_failed; the invitation's space deleted → 422 space_deleted; the link unknown, expired, revoked or used → 422 invitation_invalid; termsAccepted not true → 422 terms_required; username breaking the username rule or of the pseudonym shape → 422 username_invalid; password_weak; password_pwned; the invited email belongs to an account, a deleted one included → 422 email_taken; username taken → 422 username_taken with suggestions.
// @Description
// @Description Side effects, in ONE transaction: the account with the consent stamped by the server and the email confirmed, its personal space, the link used (its other links and unsent emails retired), the invitation accepted, membership in its space; a link that expired while the form was filled in creates no account (E-7). Signals after commit: the new account spaces (added), the sender spaces (accepted) and a push.
// @Tags Auth
// @Accept json
// @Produce json
// @Param token path string true "The link token"
// @Param X-Skein-Client-Ip header string false "The real client address stamped by skein-web"
// @Param body body accounts.InvitationSignupRequest true "Username, password, consent, Turnstile token"
// @Success 200 {object} accounts.InvitationSignupResponse "The authentication response (sessionEpoch) and the space joined"
// @Failure 400 {object} map[string]any "invalid_request"
// @Failure 401 {object} map[string]any "Service secret missing or wrong"
// @Failure 422 {object} map[string]any "signup_disabled, captcha_failed, space_deleted, invitation_invalid, terms_required, username_invalid, password_weak, password_pwned, email_taken, username_taken (suggestions)"
// @Failure 429 {object} map[string]any "rate_limited"
// @Failure 500 {object} map[string]any "internal_error"
// @Router /api/v1/auth/invitations/{token}/signup [post]
func (h *AccountsHandler) InvitationSignup(w http.ResponseWriter, r *http.Request) {
	var req accounts.InvitationSignupRequest
	if err := decodeAccountsBody(r, &req); err != nil {
		if !h.svc.SignupEnabled() {
			writeProblem(w, http.StatusUnprocessableEntity, accounts.CodeSignupDisabled, "registration is closed", nil)
			return
		}
		writeProblem(w, http.StatusBadRequest, accounts.CodeInvalidRequest, "body: a single JSON object in the invitation signup format, at most 16 KiB", nil)
		return
	}
	resp, err := h.svc.InvitationSignup(r.Context(), chi.URLParam(r, "token"), req, loginClientIP(r))
	if err != nil {
		writeAccountsError(w, r, "invitation-signup", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// decodeOptionalJSON decodes the request body into dst when there is one; an
// empty body leaves dst as it is.
func decodeOptionalJSON(r *http.Request, dst any) error {
	raw, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	return json.Unmarshal(raw, dst)
}
