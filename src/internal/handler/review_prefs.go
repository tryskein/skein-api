package handler

// review_prefs.go — sk-303.14, the C4 settings-contract surface:
// GET and PUT /api/v1/me/review-prefs.
//
// The handler knows none of the schedule numbers: bounds, factory values
// and units live in internal/review, validation is invoked by the store,
// and what arrives here is already a named error. All this file does is
// distinguish three states of each field on the WAY IN and map the
// outcome onto codes: body shape — 400, value outside bounds — 422.

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"skein-api/internal/notifications"
	"skein-api/internal/review"
)

// putReviewPrefsRequest is the PUT /api/v1/me/review-prefs body.
//
// The update is PARTIAL: an omitted field keeps its value. Schedule
// parameters have three states, and json.RawMessage is the only way to
// tell them apart: a plain *int would collapse "field omitted" and
// "field sent as null" into one nil, and there would be nothing to reset
// a parameter to factory with. The same trick is already applied to
// notifyEventTypes in the neighbouring contract.
//
// The body does NOT accept the response fields effective and bounds:
// they are derived from what is stored and from the schedule contract,
// there is nothing to write into them. The decoder rejects unknown keys,
// so sending the whole GET body back is a 400 naming the extra field,
// not a silently ignored half of the request.
//
// ⚠ `swaggertype:"integer" extensions:"x-nullable"` on the three raw
// fields is NOT decoration (fix T021/T022). The spec generator parses
// the source, not the runtime, and on json.RawMessage it stumbles:
// "cannot find type definition: json.RawMessage", then "Skipping
// 'handler.putReviewPrefsRequest'". Result — the endpoint is in the
// spec, but it has NO body schema, so a consumer learns the request
// shape not from the spec but from someone else's code. The tag names
// for the generator the shape that actually travels the wire: a number.
//
// ⚠ CORRECTED RATIONALE ERROR (review A/9, finding H; not erased). The
// previous edition of this paragraph claimed that the third state —
// explicit null — "cannot be expressed by an OpenAPI 2.0 schema at all",
// and the rationale was checked by nobody. It is wrong: what cannot be
// expressed is only the DISTINCTION "key omitted" versus "null arrived"
// — while the fact that the field CAN BE EMPTY AT ALL is expressed by
// the stock vendor extension `x-nullable`, which the generator supports
// and puts in the schema as {"type":"integer","x-nullable":true}.
//
// The cost of the error was not cosmetic: the spec declared the three
// fields as not allowing emptiness, while empty is a MANDATORY part of
// the contract ("reset the parameter to factory"), and a client
// generated from such a spec would break on a legitimate value. The
// flag has been added; the indistinguishability of "omitted"/"null"
// remains and remains described in the operation description text —
// but that is now the truth, not an excuse for a missing flag.
// review_prefs_schema_test.go holds it from disappearing.
//
// ⚠ CORRECTED DEFECT (review A/9, finding K-1; not erased). The
// paragraph above named the description directive by its NAME WITH A
// SIGIL, and a line wrap put that name at the START of a TYPE
// doc-comment line. The generator reads such a line as a directive, not
// as prose: a fragment of the Russian phrase ("of the operation — but
// that is already the truth, not an excuse for the missing") went into
// the spec as the OFFICIAL description of the request-body schema —
// verified by running `swag init`, not by reasoning. No guard saw this:
// review_prefs_schema_test.go looks at the TAG, and nothing looked at
// types. So the directive name is not written with
// a sigil in TYPE doc-comments at all: a line wrap must not be able to
// turn prose into a contract. openapi_schema_descriptions_test.go holds
// the rule.
type putReviewPrefsRequest struct {
	// MechanicEnabled is the master switch of the mechanic (INV-6).
	MechanicEnabled *bool `json:"mechanicEnabled,omitempty"`
	// ReviewRequiredDefault is the personal default of the "needs review" flag.
	ReviewRequiredDefault *bool `json:"reviewRequiredDefault,omitempty"`
	// BurstCount is the number of reminders in the first hour: a number,
	// null ("empty, factory") or a missing key ("do not touch"). 0 is a
	// legitimate "no burst" value, and it does not collapse with null.
	BurstCount json.RawMessage `json:"burstCount,omitempty" swaggertype:"integer" extensions:"x-nullable"`
	// UnacceptedIntervalMinutes is the background interval for unaccepted,
	// in minutes; the same three states.
	UnacceptedIntervalMinutes json.RawMessage `json:"unacceptedIntervalMinutes,omitempty" swaggertype:"integer" extensions:"x-nullable"`
	// UnreviewedIntervalMinutes is the background interval for unreviewed,
	// in minutes; the same three states.
	UnreviewedIntervalMinutes json.RawMessage `json:"unreviewedIntervalMinutes,omitempty" swaggertype:"integer" extensions:"x-nullable"`
}

// parseNullableInt splits a raw field into the three input states.
//
// set=false — the key was not in the body. set=true, val=nil — an
// explicit null arrived. set=true, val≠nil — a number arrived. Anything
// that is not a number and not null (a string, an object, a fraction) is
// a BODY-SHAPE error: 400, not 422. Bounds are not checked here on
// purpose — they belong to the schedule contract, not to the parser.
func parseNullableInt(raw json.RawMessage) (set bool, val *int, err error) {
	if len(raw) == 0 {
		return false, nil, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return true, nil, nil
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return false, nil, err
	}
	return true, &n, nil
}

// GetReviewPrefs handles GET /api/v1/me/review-prefs.
//
// @Summary Get the caller's review-mechanic settings — master switch, flag default and schedule
// @Description Returns the five settings of the review workflow (sk-303.14) for the calling user: the master switch that removes the whole mechanic for this person (INV-6), the personal default of the "needs review" flag applied on every task-creation path where the flag is not passed explicitly (US-3), and the three schedule parameters — burst count inside the first hour plus one background interval per reminder category. Algorithm: read the caller's lazily-created prefs row; if the account never saved any notification setting the row does not exist at all and every value falls back to the FACTORY constants of the schedule contract (internal/review) — that fallback is the normal path, not an edge case. The three schedule parameters are returned TWICE and both are needed: the top-level fields carry what the user actually stored, where null means "empty, factory applies" and 0 (burst count only) is an explicit "no burst"; `effective` carries the resolved schedule the system actually runs on. `bounds` carries the per-parameter min/max/factory straight from the schedule contract so the form does not have to hardcode a second copy of those numbers. Read-only, no side effects.
// @Tags Notifications
// @Produce json
// @Security ApiKeyAuth
// @Success 200 {object} notifications.ReviewPrefsDTO "The caller's review-mechanic settings, effective schedule and bounds"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Router /api/v1/me/review-prefs [get]
func (h *NotificationsHandler) GetReviewPrefs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	dto, err := h.store.GetReviewPrefs(ctx, c.UserID)
	if err != nil {
		slog.ErrorContext(ctx, "get review prefs failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// PutReviewPrefs handles PUT /api/v1/me/review-prefs.
//
// @Summary Update the caller's review-mechanic settings (partial)
// @Description Applies a PARTIAL update of the five review-mechanic settings and returns the same payload GET returns, so the form renders the result without a second call. Algorithm: decode the body distinguishing three states per schedule field — key absent (keep the stored value), explicit null (clear to "empty", the built-in default applies again) and a number (store it); validate every supplied value against the schedule contract; write all fields in ONE upsert that creates the prefs row when the account has none. Omitting a schedule field NEVER turns it into zero, and an explicit 0 for burstCount is stored as 0 — a legitimate "no burst" setting, which is why the column is nullable rather than "0 means unset". A value outside the contract bounds is REJECTED with 422 `invalid_schedule_setting` naming the offending field, never silently clamped (SC-022, SC-027) — a clamped value would leave the person looking at a form that agreed with them and a system doing something else; nothing at all is written in that case, so the request is all-or-nothing. Side effects: switching mechanicEnabled off immediately empties the unreviewed set, its display and its reminders for this user (INV-6) WITHOUT deleting any data — flags, review marks and countdown bases survive and come back when the switch is turned on again. A schedule change applies FROM THE NEXT SEGMENT; the already-open segment is not recomputed retroactively — its deadline was frozen when it opened. The body accepts only the five settings; `effective` and `bounds` are read-only and a body carrying them is rejected as malformed.
// @Tags Notifications
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param body body putReviewPrefsRequest true "Settings to change (partial); a schedule field may be a number, null (= factory) or absent (= unchanged)"
// @Success 200 {object} notifications.ReviewPrefsDTO "Updated settings, effective schedule and bounds"
// @Failure 400 {object} map[string]any "invalid_json — malformed body, unknown field, or a schedule field that is neither a whole number nor null"
// @Failure 401 {object} map[string]any "Missing / invalid auth"
// @Failure 422 {object} map[string]any "invalid_schedule_setting — a value outside the schedule contract bounds; nothing was written"
// @Failure 500 {object} map[string]any "internal_error — the request could not be served"
// @Router /api/v1/me/review-prefs [put]
func (h *NotificationsHandler) PutReviewPrefs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := caller(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "", nil)
		return
	}
	var req putReviewPrefsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
		return
	}

	input := notifications.UpdateReviewPrefsInput{
		MechanicEnabled:       req.MechanicEnabled,
		ReviewRequiredDefault: req.ReviewRequiredDefault,
	}
	var err error
	if input.SetBurstCount, input.BurstCount, err = parseNullableInt(req.BurstCount); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json",
			"burstCount must be a whole number or null", nil)
		return
	}
	if input.SetUnacceptedInterval, input.UnacceptedIntervalMinutes, err =
		parseNullableInt(req.UnacceptedIntervalMinutes); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json",
			"unacceptedIntervalMinutes must be a whole number of minutes or null", nil)
		return
	}
	if input.SetUnreviewedInterval, input.UnreviewedIntervalMinutes, err =
		parseNullableInt(req.UnreviewedIntervalMinutes); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json",
			"unreviewedIntervalMinutes must be a whole number of minutes or null", nil)
		return
	}

	dto, err := h.store.UpdateReviewPrefs(ctx, c.UserID, input)
	if err != nil {
		// A value outside bounds is a business refusal (422), and it MUST
		// NAME the field: a form without a field name can only redden as a
		// whole, and the person will not learn which of the three numbers
		// the system did not accept.
		var invalid *review.InvalidSettingError
		if errors.As(err, &invalid) {
			writeProblem(w, http.StatusUnprocessableEntity, "invalid_schedule_setting",
				invalid.Error(), map[string]any{"param": invalid.Param})
			return
		}
		slog.ErrorContext(ctx, "update review prefs failed",
			"userId", c.UserID.String(), "error", err.Error())
		writeProblem(w, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}
