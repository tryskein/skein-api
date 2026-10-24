package handler

// quiet_hours.go — sk-340: the time zone and quiet window contracts.
//
// TWO CONTRACTS, NOT ONE, and they are divided by WHO WRITES. The zone arrives by two
// roads — detection from the browser at login and a manual edit in settings
// — and these roads have DIFFERENT write rights: detection yields to the manual
// value, the manual value yields to nobody. The window is written by one road — the settings
// form. Merging them into one endpoint would mean adding to the settings form a field
// "is this detection or an edit", which the form always fills the same way.
//
// ⚠ THE WINDOW MUTES ONLY PUSH. The feed row is created as usual and waits for the person
// in the morning in the interface. This is written in the endpoint description, not only here,
// because otherwise a consumer of the contract will read "quiet hours" as "do not
// notify".

import (
	"errors"
	"log/slog"
	"net/http"

	"skein-api/internal/notifications"
)

// putTimezoneRequest — body of PUT /me/timezone.
type putTimezoneRequest struct {
	// Timezone — an IANA zone name, for example "Europe/Moscow". Checked
	// by loading from the time zone database, not by the look of the string.
	Timezone string `json:"timezone"`
	// Source — "manual" (an edit in settings) or "detected" (detection from the
	// browser). Empty = manual: a call that did not identify itself as detection
	// is treated as a deliberate edit, because the opposite default would let
	// a call that forgot the field silently overwrite the manual value.
	Source string `json:"source"`
}

// putQuietHoursRequest — body of PUT /me/quiet-hours.
//
// Both bounds are pointers, and both null mean "the window is off". Exactly one
// null is a refusal (422): half a window does not define a window.
type putQuietHoursRequest struct {
	// Start — the start of the window, "HH:MM", by the recipient's wall clock,
	// inclusive.
	Start *string `json:"start"`
	// End — the end of the window, "HH:MM", NOT inclusive. Less than the start — the window
	// crosses midnight (22:00–07:00), and this is a normal setting.
	End *string `json:"end"`
}

// GetTimezone handles GET /api/v1/me/timezone.
//
// @Summary Get the caller's time zone
// @Description Returns the caller's IANA time zone and where the value came from ("manual" — typed in settings, "detected" — reported by the browser at login). Both fields are null when the zone is unknown: an account that has never signed in through a browser (an agent, an MCP session) legitimately has none. The zone is what makes quiet hours interpretable — without it a quiet window cannot be set at all.
// @Tags Push
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} notifications.TimezoneDTO "Time zone and its source"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Router /api/v1/me/timezone [get]
func (h *NotificationsHandler) GetTimezone(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	dto, err := h.store.Timezone(ctx, c.UserID)
	if err != nil {
		slog.ErrorContext(ctx, "get timezone failed", "userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// PutTimezone handles PUT /api/v1/me/timezone.
//
// @Summary Set the caller's time zone
// @Description Stores the caller's IANA time zone. The name is validated by LOADING it from the time-zone database, so a plausible-looking but non-existent name ("Europe/Moskva") is rejected with 422 rather than silently disabling the caller's quiet hours. "Local" is rejected too — it names the SERVER's own zone, which is precisely the answer this feature exists to avoid.
// @Description
// @Description source="manual" (the default when the field is omitted) always writes and marks the value as hand-set. source="detected" — the browser's zone reported at login — writes ONLY while no hand-set value exists, so a correction made in settings is not silently reverted at the next sign-in. Either way the response carries the zone that is now IN EFFECT, not an echo of the request, so a detected write that yielded to a manual value is distinguishable from one that landed.
// @Tags Push
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body putTimezoneRequest true "Time zone and its source"
// @Success 200 {object} notifications.TimezoneDTO "Time zone in effect after the write"
// @Failure 400 {object} map[string]any "invalid_json"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 422 {object} map[string]any "invalid_timezone — unknown IANA zone; invalid_source — source is neither manual nor detected"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Router /api/v1/me/timezone [put]
func (h *NotificationsHandler) PutTimezone(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	var req putTimezoneRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	source := notifications.TimezoneSourceManual
	switch req.Source {
	case "", string(notifications.TimezoneSourceManual):
	case string(notifications.TimezoneSourceDetected):
		source = notifications.TimezoneSourceDetected
	default:
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_source",
			`source must be "manual" or "detected"`, nil)
		return
	}
	dto, err := h.store.SetTimezone(ctx, c.UserID, req.Timezone, source)
	if err != nil {
		if errors.Is(err, notifications.ErrInvalidTimezone) {
			writeProblem(w, http.StatusUnprocessableEntity, "invalid_timezone", err.Error(), nil)
			return
		}
		slog.ErrorContext(ctx, "set timezone failed", "userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// GetQuietHours handles GET /api/v1/me/quiet-hours.
//
// @Summary Get the caller's quiet hours
// @Description Returns the quiet window (start/end as "HH:MM" by the caller's own wall clock) together with the time zone the window is read in. start and end are both null when quiet hours are off — which is the factory state. An end EARLIER than start is not an error: it means the window crosses midnight (22:00–07:00).
// @Description
// @Description Quiet hours silence the PUSH only. The notification row is written as usual and waits in the app, so nothing is lost or replayed — the person sees it in the morning.
// @Tags Push
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} notifications.QuietHoursDTO "Quiet window plus the zone it is read in"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Router /api/v1/me/quiet-hours [get]
func (h *NotificationsHandler) GetQuietHours(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	dto, err := h.store.QuietHours(ctx, c.UserID)
	if err != nil {
		slog.ErrorContext(ctx, "get quiet hours failed", "userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// PutQuietHours handles PUT /api/v1/me/quiet-hours.
//
// @Summary Set or clear the caller's quiet hours
// @Description Replaces the quiet window. Both fields null clears it (quiet hours off). Exactly one null is rejected with 422 — half a window is not a window, and nobody guesses the other boundary. Equal start and end is rejected too: it reads either as "never quiet" or as "always quiet", and choosing one of the two on the caller's behalf would be a product decision made silently.
// @Description
// @Description Setting a window requires a stored time zone (422 timezone_required): "22:00" without a zone is a string, not a moment. An end EARLIER than start is accepted and means the window crosses midnight.
// @Description
// @Description Side effect on delivery: a push whose delivery moment falls inside the window is finished WITHOUT being sent, with the terminal outcome "quiet_hours" — it is not deferred to the end of the window and not replayed afterwards. The notification itself is untouched: the row is written to the feed as usual and read in the app later.
// @Tags Push
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body putQuietHoursRequest true "Desired window, or both fields null to clear"
// @Success 200 {object} notifications.QuietHoursDTO "Quiet window after the write"
// @Failure 400 {object} map[string]any "invalid_json"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 422 {object} map[string]any "invalid_time — not HH:MM; quiet_hours_half_set — one boundary without the other; quiet_hours_degenerate — start equals end; timezone_required — no time zone stored"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Router /api/v1/me/quiet-hours [put]
func (h *NotificationsHandler) PutQuietHours(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	var req putQuietHoursRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}
	var window *notifications.QuietWindow
	switch {
	case req.Start == nil && req.End == nil:
		// The window is turned off.
	case req.Start == nil || req.End == nil:
		writeProblem(w, http.StatusUnprocessableEntity, "quiet_hours_half_set",
			notifications.ErrQuietWindowHalfSet.Error(), nil)
		return
	default:
		start, err := notifications.ParseClock(*req.Start)
		if err != nil {
			writeProblem(w, http.StatusUnprocessableEntity, "invalid_time", err.Error(), nil)
			return
		}
		end, err := notifications.ParseClock(*req.End)
		if err != nil {
			writeProblem(w, http.StatusUnprocessableEntity, "invalid_time", err.Error(), nil)
			return
		}
		window = &notifications.QuietWindow{Start: start, End: end}
	}
	if err := h.store.SetQuietHours(ctx, c.UserID, window); err != nil {
		switch {
		case errors.Is(err, notifications.ErrQuietWindowDegenerate):
			writeProblem(w, http.StatusUnprocessableEntity, "quiet_hours_degenerate", err.Error(), nil)
		case errors.Is(err, notifications.ErrTimezoneRequired):
			writeProblem(w, http.StatusUnprocessableEntity, "timezone_required", err.Error(), nil)
		default:
			slog.ErrorContext(ctx, "set quiet hours failed", "userId", c.UserID.String(), "error", err.Error())
			writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		}
		return
	}
	dto, err := h.store.QuietHours(ctx, c.UserID)
	if err != nil {
		slog.ErrorContext(ctx, "read back quiet hours failed", "userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}
