package notifications

// review_prefs.go — sk-303.14, contract C4 of the review-mechanic
// settings: the master switch, the personal flag default, and the three
// schedule parameters.
//
// WHY THIS LIVES HERE. The five values live in user_notification_prefs —
// the table this package owns, and there is no other notification-settings
// store in the system (a system-wide settings table is forbidden by the
// plan). The domain arithmetic and the bounds stay in internal/review:
// there is not a single schedule number here, only calls into the contract.
//
// THE LAZY ROW IS A LOAD-BEARING FACT, NOT AN EDGE CASE. The settings row
// is created on first write, so a person who has never saved anything has
// NO row. Therefore the "built-in default" is what the READER substitutes when
// the row is absent, and the column DEFAULT is reachable only for rows
// created by someone else's upsert. Hence the only source of factory is the
// constants in internal/review; their match with the schema is held by the
// T011 gate.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/review"
	"skein-api/internal/signals"
)

// ---------------------------------------------------------------------------
// Wire shapes
// ---------------------------------------------------------------------------

// ReviewSettingBoundsDTO is the bounds and built-in default of ONE schedule
// parameter, in the transport unit.
//
// The block is returned to the client on purpose: without it the settings
// form would write 0..6 and 15..527040 itself as a third copy — and a
// handwritten list of numbers is silent when the contract changes, drifting
// in exactly the way this caution exists to prevent. The numbers come from
// internal/review, so they can only be moved there.
type ReviewSettingBoundsDTO struct {
	// Min — the smallest allowed value, inclusive.
	Min int `json:"min"`
	// Max — the largest allowed value, inclusive.
	Max int `json:"max"`
	// Factory — the built-in default: what applies while the field is "empty"
	// (null).
	Factory int `json:"factory"`
}

// ReviewScheduleDTO is the three schedule parameters in RESOLVED form: the
// factory has already been substituted for "empty", so each field is the
// effective value, not "maybe null".
//
// ⚠ CORRECTED MISTAKE (review A/9, finding P-1; not erased). Here and in
// ReviewBoundsDTO below stood `extensions:"x-nullable"` — hung on at the
// same time as the fields that actually need it. That is the INVERSE of the
// error finding H fixed: there the spec denied emptiness that happens, here
// it promised emptiness that NEVER happens. The fields here are not
// pointers, and a "resolved schedule" is by definition a schedule with
// factory values substituted; it does not serialise as empty on any path.
// The paragraph above said that in so many words, and the tag contradicted
// it.
type ReviewScheduleDTO struct {
	// BurstCount — number of reminders in the first hour for unaccepted
	// issues. 0 — no burst.
	BurstCount int `json:"burstCount"`
	// UnacceptedIntervalMinutes — background interval for unaccepted issues,
	// minutes.
	UnacceptedIntervalMinutes int `json:"unacceptedIntervalMinutes"`
	// UnreviewedIntervalMinutes — background interval for unreviewed
	// results, minutes. This category has no burst (SC-013).
	UnreviewedIntervalMinutes int `json:"unreviewedIntervalMinutes"`
}

// ReviewBoundsDTO is the bounds of all three schedule parameters.
//
// ⚠ There are no pointer fields here: each is a bounds struct, read from
// the schedule contract and always filled. The former `extensions:"x-nullable"`
// (the same finding P-1) promised emptiness that never happens, and also
// changed the SHAPE of the schema: the generator wrapped the three `$ref`
// in `allOf: [$ref]` so there was somewhere to put the flag — and SDK
// generators build different code from that shape. Verified by comparing
// swagger.json before and after.
type ReviewBoundsDTO struct {
	BurstCount                ReviewSettingBoundsDTO `json:"burstCount"`
	UnacceptedIntervalMinutes ReviewSettingBoundsDTO `json:"unacceptedIntervalMinutes"`
	UnreviewedIntervalMinutes ReviewSettingBoundsDTO `json:"unreviewedIntervalMinutes"`
}

// ReviewPrefsDTO is the response body of GET and PUT /me/review-prefs
// (contract C4).
//
// The three schedule parameters are returned TWICE and that is not
// duplication: the top-level pointer fields are what the person saved, where
// null means "empty, take factory"; Effective is what that resolved to. The
// form needs both: the first — to show an empty field as empty, not as a
// factory number the person never typed; the second — to show which schedule
// is actually in force. Merging them into one field would lose the
// distinction between "empty" and "explicit zero" that the schema columns
// are declared NULLABLE for.
//
// ⚠ `extensions:"x-nullable"` on the three saved parameters (review A/9,
// finding H) is not decoration. Without it the spec declares them ordinary
// integers, and they return null ALWAYS until the person has saved
// something — i.e. on the most ordinary path. A client generated from a spec
// without this flag would break on the first response of an untouched
// profile.
type ReviewPrefsDTO struct {
	// MechanicEnabled — the master switch of the mechanic (INV-6). false —
	// for the user the unreviewed set, its display and the reminders about
	// it disappear; the data is kept and returns when the switch is turned
	// back on.
	MechanicEnabled bool `json:"mechanicEnabled"`
	// ReviewRequiredDefault — the personal default of the "review required"
	// flag, applied on every issue-creation path where the flag is not
	// passed explicitly (US-3).
	ReviewRequiredDefault bool `json:"reviewRequiredDefault"`

	// BurstCount — the saved number of reminders in the first hour; null —
	// "empty, factory applies". 0 — an explicit setting "no burst".
	BurstCount *int `json:"burstCount" extensions:"x-nullable"`
	// UnacceptedIntervalMinutes — the saved background interval for
	// unaccepted, minutes; null — "empty".
	UnacceptedIntervalMinutes *int `json:"unacceptedIntervalMinutes" extensions:"x-nullable"`
	// UnreviewedIntervalMinutes — the saved background interval for
	// unreviewed, minutes; null — "empty".
	UnreviewedIntervalMinutes *int `json:"unreviewedIntervalMinutes" extensions:"x-nullable"`

	// Effective — the schedule in force: factory substituted for "empty".
	// Read-only; PUT does not accept this field.
	Effective ReviewScheduleDTO `json:"effective"`
	// Bounds — the bounds and factory values of the parameters, from the
	// schedule contract. Read-only; PUT does not accept this field.
	Bounds ReviewBoundsDTO `json:"bounds"`
}

// UpdateReviewPrefsInput is a partial update of the mechanic settings.
//
// Each schedule parameter has THREE input states, and all three are
// distinguishable: the field is omitted (Set* == false — leave as is), the
// field is passed as null (Set* == true, value nil — "empty", factory
// applies) and the field is passed as a number (Set* == true, value not
// nil). Folding the second and third into one pointer is not allowed: then
// "empty" would be indistinguishable from "do not touch", and there would be
// no way to return a parameter to factory.
type UpdateReviewPrefsInput struct {
	// MechanicEnabled / ReviewRequiredDefault — nil means "field omitted".
	// Booleans have no third state: "empty" is not defined for them, a
	// switch always has a position.
	MechanicEnabled       *bool
	ReviewRequiredDefault *bool

	SetBurstCount bool
	BurstCount    *int

	SetUnacceptedInterval     bool
	UnacceptedIntervalMinutes *int

	SetUnreviewedInterval     bool
	UnreviewedIntervalMinutes *int
}

// ---------------------------------------------------------------------------
// Read
// ---------------------------------------------------------------------------

const reviewPrefsColumns = `review_mechanic_enabled, review_required_default,
       reminder_burst_count, unaccepted_interval_minutes, unreviewed_interval_minutes`

// GetReviewPrefs returns the review-mechanic settings for a user.
//
// No settings row → ALL five values are factory, taken from the constants in
// internal/review. This is not a fallback, it is the working path: the row
// is created lazily, so the vast majority of users have none, and this
// branch is what answers "is the mechanic on for me".
func (s *Store) GetReviewPrefs(ctx context.Context, userID uuid.UUID) (ReviewPrefsDTO, error) {
	var (
		mechanic, requiredDefault bool
		burst, unacc, unrev       *int
	)
	err := s.pool.QueryRow(ctx, `
SELECT `+reviewPrefsColumns+`
  FROM user_notification_prefs
 WHERE user_id = $1`, userID).
		Scan(&mechanic, &requiredDefault, &burst, &unacc, &unrev)
	if errors.Is(err, pgx.ErrNoRows) {
		return buildReviewPrefsDTO(review.DefaultMechanicEnabled, review.DefaultReviewRequired, nil, nil, nil)
	}
	if err != nil {
		return ReviewPrefsDTO{}, fmt.Errorf("loading review prefs: %w", err)
	}
	return buildReviewPrefsDTO(mechanic, requiredDefault, burst, unacc, unrev)
}

// ---------------------------------------------------------------------------
// Write
// ---------------------------------------------------------------------------

// UpdateReviewPrefs applies a partial update and returns the post-state.
//
// BOUNDS ARE CHECKED HERE, BEFORE THE WRITE, by the internal/review
// contract, and a value outside the bounds is REJECTED as a whole: none of
// the request fields is applied (SC-022, SC-027). Silently clamping a value
// is forbidden — the person would be left with a form that agreed with them
// and a system that did something else. The refusal unwraps as
// *review.InvalidSettingError and names the field, so the handler can name
// it in the 422 body.
//
// The write is ONE statement, not "read, merge, write": merging in the
// application between two round-trips loses a parallel edit from a
// neighbouring tab entirely, and the loss looks like a successful save.
// CASE on the "field was passed" flag leaves untouched columns in place
// inside the statement itself.
//
// The INSERT branch names its columns, so the schema DEFAULTs for them do
// not fire — and the boolean defaults are substituted by CONSTANTS from
// internal/review. Other columns of the table the statement does not name:
// their schema DEFAULTs (notify_event_types NULL, default_inbox_scope
// 'all', suppress_agent_actions true, push_disabled_categories '{}') match
// what their own readers substitute, so creating a row this way does not
// extinguish someone else's setting.
func (s *Store) UpdateReviewPrefs(ctx context.Context, userID uuid.UUID, in UpdateReviewPrefsInput) (ReviewPrefsDTO, error) {
	if in.SetBurstCount && in.BurstCount != nil {
		if err := review.ValidateBurstCount(*in.BurstCount); err != nil {
			return ReviewPrefsDTO{}, err
		}
	}
	if in.SetUnacceptedInterval && in.UnacceptedIntervalMinutes != nil {
		if err := review.ValidateIntervalMinutes("unacceptedIntervalMinutes", *in.UnacceptedIntervalMinutes); err != nil {
			return ReviewPrefsDTO{}, err
		}
	}
	if in.SetUnreviewedInterval && in.UnreviewedIntervalMinutes != nil {
		if err := review.ValidateIntervalMinutes("unreviewedIntervalMinutes", *in.UnreviewedIntervalMinutes); err != nil {
			return ReviewPrefsDTO{}, err
		}
	}

	// INSERT-branch values: the passed one, otherwise factory (booleans) /
	// "empty" (schedule parameters).
	mechanic := review.DefaultMechanicEnabled
	if in.MechanicEnabled != nil {
		mechanic = *in.MechanicEnabled
	}
	requiredDefault := review.DefaultReviewRequired
	if in.ReviewRequiredDefault != nil {
		requiredDefault = *in.ReviewRequiredDefault
	}

	// prev reads the row from the statement's snapshot, i.e. BEFORE the
	// write: the RETURNING tail compares the mechanic switch against the
	// value in effect before ($12 is the no-row factory value), so a request
	// that repeats it gives no signal (found in review).
	const upsert = `
WITH prev AS (
    SELECT review_mechanic_enabled FROM user_notification_prefs WHERE user_id = $1
)
INSERT INTO user_notification_prefs (
    user_id, review_mechanic_enabled, review_required_default,
    reminder_burst_count, unaccepted_interval_minutes, unreviewed_interval_minutes,
    updated_at
)
VALUES ($1, $2, $3, $4, $5, $6, now())
ON CONFLICT (user_id) DO UPDATE
   SET review_mechanic_enabled = CASE WHEN $7::boolean
           THEN EXCLUDED.review_mechanic_enabled
           ELSE user_notification_prefs.review_mechanic_enabled END,
       review_required_default = CASE WHEN $8::boolean
           THEN EXCLUDED.review_required_default
           ELSE user_notification_prefs.review_required_default END,
       reminder_burst_count = CASE WHEN $9::boolean
           THEN EXCLUDED.reminder_burst_count
           ELSE user_notification_prefs.reminder_burst_count END,
       unaccepted_interval_minutes = CASE WHEN $10::boolean
           THEN EXCLUDED.unaccepted_interval_minutes
           ELSE user_notification_prefs.unaccepted_interval_minutes END,
       unreviewed_interval_minutes = CASE WHEN $11::boolean
           THEN EXCLUDED.unreviewed_interval_minutes
           ELSE user_notification_prefs.unreviewed_interval_minutes END,
       updated_at = now()
RETURNING ` + reviewPrefsColumns + `,
       COALESCE((SELECT review_mechanic_enabled FROM prev), $12::boolean)`

	var (
		outMechanic, outRequired bool
		outBurst, outUnacc       *int
		outUnrev                 *int
		mechanicBefore           bool
	)
	err := s.pool.QueryRow(ctx, upsert,
		userID, mechanic, requiredDefault,
		in.BurstCount, in.UnacceptedIntervalMinutes, in.UnreviewedIntervalMinutes,
		in.MechanicEnabled != nil, in.ReviewRequiredDefault != nil,
		in.SetBurstCount, in.SetUnacceptedInterval, in.SetUnreviewedInterval,
		review.DefaultMechanicEnabled,
	).Scan(&outMechanic, &outRequired, &outBurst, &outUnacc, &outUnrev, &mechanicBefore)
	if err != nil {
		return ReviewPrefsDTO{}, fmt.Errorf("upserting review prefs: %w", err)
	}
	// sk-622.21 T016 (US-11, "Settings change … review"): the review mechanic
	// switch decides whether the unreviewed counter and filters exist for
	// this user. The other review settings shape reminders only. Only an
	// actual flip signals; repeating the value in effect changes nothing.
	if in.MechanicEnabled != nil && outMechanic != mechanicBefore {
		signalCommitted(ctx, userID, signals.AreaUnreviewed)
	}
	return buildReviewPrefsDTO(outMechanic, outRequired, outBurst, outUnacc, outUnrev)
}

// ---------------------------------------------------------------------------
// Response assembly
// ---------------------------------------------------------------------------

// buildReviewPrefsDTO assembles the response from stored values: resolves
// the schedule via the contract (factory in place of "empty") and attaches
// the bounds.
//
// Resolution can refuse only on a value outside the bounds, and such a
// value cannot be in the table — both the schema CHECK and the check higher
// in this file reject it. Even so a refusal here is returned, not swallowed:
// a row outside the bounds would mean both guards let a write through, and
// a silently substituted factory would hide that from everyone.
func buildReviewPrefsDTO(mechanic, requiredDefault bool, burst, unacc, unrev *int) (ReviewPrefsDTO, error) {
	settings, err := review.ResolveMinutes(burst, unacc, unrev)
	if err != nil {
		return ReviewPrefsDTO{}, fmt.Errorf("resolving stored review schedule: %w", err)
	}
	return ReviewPrefsDTO{
		MechanicEnabled:           mechanic,
		ReviewRequiredDefault:     requiredDefault,
		BurstCount:                burst,
		UnacceptedIntervalMinutes: unacc,
		UnreviewedIntervalMinutes: unrev,
		Effective: ReviewScheduleDTO{
			BurstCount:                settings.BurstCount,
			UnacceptedIntervalMinutes: int(settings.UnacceptedInterval / time.Minute),
			UnreviewedIntervalMinutes: int(settings.UnreviewedInterval / time.Minute),
		},
		Bounds: ReviewBoundsDTO{
			BurstCount: ReviewSettingBoundsDTO{
				Min:     review.MinBurstCount,
				Max:     review.MaxBurstCount,
				Factory: review.DefaultBurstCount,
			},
			UnacceptedIntervalMinutes: ReviewSettingBoundsDTO{
				Min:     review.MinIntervalMinutes,
				Max:     review.MaxIntervalMinutes,
				Factory: review.DefaultUnacceptedIntervalMinutes,
			},
			UnreviewedIntervalMinutes: ReviewSettingBoundsDTO{
				Min:     review.MinIntervalMinutes,
				Max:     review.MaxIntervalMinutes,
				Factory: review.DefaultUnreviewedIntervalMinutes,
			},
		},
	}, nil
}

// ReviewSchedules returns the EFFECTIVE schedules of a batch of users in one
// query.
//
// It exists for the background reminder worker (sk-303.14, T017): it
// processes addressees in batches, and a per-user GetReviewPrefs would be a
// query per person — even though of the whole DTO it needs three numbers,
// and the bounds and "what the person saved" are not needed at all.
//
// Absence of a row is the working path, not an edge: the settings row is
// created lazily, so most addressees have none. Such a user gets the
// FACTORY schedule, and they ARE in the response — the map is returned
// filled for EVERY requested identifier, so the caller has no "what if the
// key is missing" branch.
//
// Arithmetic and bounds are not duplicated: the values go through the same
// review.ResolveMinutes as the single-user read. A value outside the bounds
// is not "quietly fix it", it is an error: saving such a value is rejected
// by the contract, and if it is in the database anyway that is data
// corruption the worker must learn about, not silently remind on a made-up
// schedule.
func (s *Store) ReviewSchedules(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]review.Settings, error) {
	out := make(map[uuid.UUID]review.Settings, len(userIDs))
	for _, id := range userIDs {
		out[id] = review.Defaults()
	}
	if len(userIDs) == 0 {
		return out, nil
	}

	rows, err := s.pool.Query(ctx, `
SELECT user_id, reminder_burst_count, unaccepted_interval_minutes, unreviewed_interval_minutes
  FROM user_notification_prefs
 WHERE user_id = ANY($1::uuid[])`, userIDs)
	if err != nil {
		return nil, fmt.Errorf("loading review schedules: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			userID              uuid.UUID
			burst, unacc, unrev *int
		)
		if err := rows.Scan(&userID, &burst, &unacc, &unrev); err != nil {
			return nil, fmt.Errorf("scanning review schedules: %w", err)
		}
		settings, rerr := review.ResolveMinutes(burst, unacc, unrev)
		if rerr != nil {
			return nil, fmt.Errorf("resolving stored review schedule for %s: %w", userID, rerr)
		}
		out[userID] = settings
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("loading review schedules: %w", err)
	}
	return out, nil
}
