package handler

// review_prefs_rest_test.go — sk-303.14, T011: the integration class of
// the C4 settings contract on a real schema. Matrix rows 46, 47, 114
// plus the gate that factory values in the code and in the schema agree.
//
// WHAT IS CHECKED HERE, AND WHAT IS NOT. Row 48 ("switching off
// immediately empties the set in ALL THREE manifestations") is beyond
// this class and belongs to T020: two of its manifestations — the
// second number and the reminder selection — do not exist yet. The half
// observable today is still closed here: switching off does NOT DELETE
// data, so flags and marks survive a round trip of the switch. Row 115
// ("from the next segment") waits for T018 — the rule lives on a frozen
// segment deadline that nobody writes yet.
//
// The other rows of T011's ownership (25–45, 120) are closed by their
// own tasks' classes — issues_review_test.go, issues_review_channels_test.go,
// outbox/dispatcher_private_test.go, issues/review_close_reset_test.go
// and neighbours. They are not rewritten here: a domain integration
// class is the requirement "green on a real database", not the
// requirement "in one file".

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/notifications"
	"skein-api/internal/review"
	"skein-api/internal/testsupport"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

func callGetReviewPrefs(t *testing.T, pool *pgxpool.Pool, callerID uuid.UUID) notifications.ReviewPrefsDTO {
	t.Helper()
	h := NewNotificationsHandler(pool)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/review-prefs", nil)
	ctx := auth.WithUserID(req.Context(), callerID)
	ctx = auth.WithUserRole(ctx, "user")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	h.GetReviewPrefs(rec, req)
	res := rec.Result()
	t.Cleanup(func() { _ = res.Body.Close() })
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /me/review-prefs = %d: %s", res.StatusCode, rec.Body.String())
	}
	var dto notifications.ReviewPrefsDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode GET: %v (%s)", err, rec.Body.String())
	}
	return dto
}

// putReviewPrefs sends a RAW body: the distinction "no key", "null" and
// "a number" is the subject of the check, and a struct on the way in
// would wipe it.
func putReviewPrefs(t *testing.T, pool *pgxpool.Pool, callerID uuid.UUID, raw string) (int, map[string]any) {
	t.Helper()
	h := NewNotificationsHandler(pool)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/me/review-prefs",
		bytes.NewBufferString(raw))
	req.Header.Set("Content-Type", "application/json")
	ctx := auth.WithUserID(req.Context(), callerID)
	ctx = auth.WithUserRole(ctx, "user")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	h.PutReviewPrefs(rec, req)
	res := rec.Result()
	t.Cleanup(func() { _ = res.Body.Close() })
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode PUT (%d): %v (%s)", res.StatusCode, err, rec.Body.String())
	}
	return res.StatusCode, body
}

func putReviewPrefsOK(t *testing.T, pool *pgxpool.Pool, callerID uuid.UUID, raw string) {
	t.Helper()
	if status, body := putReviewPrefs(t, pool, callerID, raw); status != http.StatusOK {
		t.Fatalf("PUT %s = %d: %v", raw, status, body)
	}
}

func countPrefsRows(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM user_notification_prefs WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatalf("count prefs rows: %v", err)
	}
	return n
}

func factorySchedule() notifications.ReviewScheduleDTO {
	return notifications.ReviewScheduleDTO{
		BurstCount:                review.DefaultBurstCount,
		UnacceptedIntervalMinutes: review.DefaultUnacceptedIntervalMinutes,
		UnreviewedIntervalMinutes: review.DefaultUnreviewedIntervalMinutes,
	}
}

func describePrefs(d notifications.ReviewPrefsDTO) string {
	show := func(p *int) string {
		if p == nil {
			return "empty"
		}
		return fmt.Sprintf("%d", *p)
	}
	return fmt.Sprintf("mechanic=%v default=%v burst=%s unaccepted=%s unreviewed=%s effective=%+v",
		d.MechanicEnabled, d.ReviewRequiredDefault,
		show(d.BurstCount), show(d.UnacceptedIntervalMinutes), show(d.UnreviewedIntervalMinutes),
		d.Effective)
}

// ---------------------------------------------------------------------------
// Row 46 — is read and written; an untouched setting yields factory
// ---------------------------------------------------------------------------

// TestReviewPrefs_FactoryForAccountThatNeverSaved — row 46, the half
// "the flag default of someone who has never saved is factory 'needed'".
//
// The prefs row is created LAZILY, so here it is not there at all, and
// the reader answers, not the schema. All five values are checked at
// once: for the switch an error this way is worse — "off by default"
// would mean nobody has the mechanic (INV-6 inverted).
func TestReviewPrefs_FactoryForAccountThatNeverSaved(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	user, _ := testsupport.SeedUser(t, pool, "user")

	if n := countPrefsRows(t, pool, user); n != 0 {
		t.Fatalf("precondition: the user already has a prefs row (%d) — "+
			"the probe would not tell the reader's answer from the schema default", n)
	}

	got := callGetReviewPrefs(t, pool, user)

	if got.MechanicEnabled != review.DefaultMechanicEnabled {
		t.Errorf("mechanicEnabled = %v, factory %v — for someone who has not touched "+
			"settings the mechanic must be in the factory position (INV-6)",
			got.MechanicEnabled, review.DefaultMechanicEnabled)
	}
	if got.ReviewRequiredDefault != review.DefaultReviewRequired {
		t.Errorf("reviewRequiredDefault = %v, factory %v — the spec names factory as \"needed\" (US-3)",
			got.ReviewRequiredDefault, review.DefaultReviewRequired)
	}
	if got.BurstCount != nil || got.UnacceptedIntervalMinutes != nil || got.UnreviewedIntervalMinutes != nil {
		t.Errorf("stored schedule is not empty for someone who has never saved: %s", describePrefs(got))
	}
	if got.Effective != factorySchedule() {
		t.Errorf("effective schedule %+v, factory %+v", got.Effective, factorySchedule())
	}
}

// TestReviewPrefs_FactoryConstantsMatchSchemaDefaults — the GATE of
// agreement of two instances of the built-in default.
//
// Factory lives in two places: as a constant in the code (the reader
// substitutes it when there is no prefs row) and as the column default
// in the schema (it fires when a foreign upsert created the row without
// naming our columns). The instances are compared by nobody, and a
// divergence stays silent: both values are valid on their own, and
// behaviour starts depending on whether the person has a row. Two
// people with equally untouched settings would get different answers.
//
// The assertion is direct: a user WITHOUT a row and a user with a row
// created ONLY by schema defaults must get an answer that matches
// entirely.
func TestReviewPrefs_FactoryConstantsMatchSchemaDefaults(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	withoutRow, _ := testsupport.SeedUser(t, pool, "user")
	withBareRow, _ := testsupport.SeedUser(t, pool, "user")

	// The row is created so that EACH of the five values comes from the
	// schema default: none of the mechanic columns is named.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO user_notification_prefs (user_id) VALUES ($1)`, withBareRow); err != nil {
		t.Fatalf("insert bare prefs row: %v", err)
	}
	if n := countPrefsRows(t, pool, withoutRow); n != 0 {
		t.Fatalf("precondition: the first user now has a prefs row (%d)", n)
	}

	fromConstants := callGetReviewPrefs(t, pool, withoutRow)
	fromSchema := callGetReviewPrefs(t, pool, withBareRow)

	if !reflect.DeepEqual(fromConstants, fromSchema) {
		t.Fatalf("factory in the code and the column default diverged.\n"+
			"  without a prefs row: %s\n"+
			"  row from schema defaults: %s\n"+
			"Both values are valid on their own, so the defect stays silent: behaviour "+
			"starts depending on whether the person has a prefs row. "+
			"The source of truth is the internal/review constants; either the column "+
			"DEFAULT in migration 000044 or the constant is fixed, but BOTH together.",
			describePrefs(fromConstants), describePrefs(fromSchema))
	}

	// Both must equal the contract, not just each other: two equally
	// WRONG values would also pass the comparison above.
	if fromSchema.MechanicEnabled != review.DefaultMechanicEnabled {
		t.Errorf("column default review_mechanic_enabled = %v, constant review.DefaultMechanicEnabled = %v",
			fromSchema.MechanicEnabled, review.DefaultMechanicEnabled)
	}
	if fromSchema.ReviewRequiredDefault != review.DefaultReviewRequired {
		t.Errorf("column default review_required_default = %v, constant review.DefaultReviewRequired = %v",
			fromSchema.ReviewRequiredDefault, review.DefaultReviewRequired)
	}
	if fromSchema.Effective != factorySchedule() {
		t.Errorf("schedule of the schema-default row %+v, factory from the contract %+v",
			fromSchema.Effective, factorySchedule())
	}
	if fromSchema.Bounds.BurstCount.Factory != review.DefaultBurstCount ||
		fromSchema.Bounds.UnacceptedIntervalMinutes.Factory != review.DefaultUnacceptedIntervalMinutes ||
		fromSchema.Bounds.UnreviewedIntervalMinutes.Factory != review.DefaultUnreviewedIntervalMinutes {
		t.Errorf("bounds named factory values different from the contract: %+v", fromSchema.Bounds)
	}
	if fromSchema.Bounds.BurstCount.Min != review.MinBurstCount ||
		fromSchema.Bounds.BurstCount.Max != review.MaxBurstCount ||
		fromSchema.Bounds.UnacceptedIntervalMinutes.Min != review.MinIntervalMinutes ||
		fromSchema.Bounds.UnacceptedIntervalMinutes.Max != review.MaxIntervalMinutes ||
		fromSchema.Bounds.UnreviewedIntervalMinutes.Min != review.MinIntervalMinutes ||
		fromSchema.Bounds.UnreviewedIntervalMinutes.Max != review.MaxIntervalMinutes {
		t.Errorf("bounds in the response contract diverged from internal/review: %+v", fromSchema.Bounds)
	}
}

// TestReviewPrefs_AllFiveAreWrittenAndReadBack — row 46, the half "all
// five are read and written".
//
// Probe values are chosen NON-factory and at the edges of the allowed:
// a built-in default would pass even when the write never happened.
func TestReviewPrefs_AllFiveAreWrittenAndReadBack(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	user, _ := testsupport.SeedUser(t, pool, "user")
	stranger, _ := testsupport.SeedUser(t, pool, "user")

	raw := fmt.Sprintf(`{"mechanicEnabled":%v,"reviewRequiredDefault":%v,
"burstCount":%d,"unacceptedIntervalMinutes":%d,"unreviewedIntervalMinutes":%d}`,
		!review.DefaultMechanicEnabled, !review.DefaultReviewRequired,
		review.MaxBurstCount, review.MinIntervalMinutes, review.MaxIntervalMinutes)
	putReviewPrefsOK(t, pool, user, raw)

	got := callGetReviewPrefs(t, pool, user)
	if got.MechanicEnabled != !review.DefaultMechanicEnabled {
		t.Errorf("mechanicEnabled did not persist: %s", describePrefs(got))
	}
	if got.ReviewRequiredDefault != !review.DefaultReviewRequired {
		t.Errorf("reviewRequiredDefault did not persist: %s", describePrefs(got))
	}
	if got.BurstCount == nil || *got.BurstCount != review.MaxBurstCount {
		t.Errorf("burstCount did not persist: %s", describePrefs(got))
	}
	if got.UnacceptedIntervalMinutes == nil || *got.UnacceptedIntervalMinutes != review.MinIntervalMinutes {
		t.Errorf("unacceptedIntervalMinutes did not persist: %s", describePrefs(got))
	}
	if got.UnreviewedIntervalMinutes == nil || *got.UnreviewedIntervalMinutes != review.MaxIntervalMinutes {
		t.Errorf("unreviewedIntervalMinutes did not persist: %s", describePrefs(got))
	}
	want := notifications.ReviewScheduleDTO{
		BurstCount:                review.MaxBurstCount,
		UnacceptedIntervalMinutes: review.MinIntervalMinutes,
		UnreviewedIntervalMinutes: review.MaxIntervalMinutes,
	}
	if got.Effective != want {
		t.Errorf("effective schedule %+v, want %+v", got.Effective, want)
	}

	// One person's settings are not another person's settings.
	if other := callGetReviewPrefs(t, pool, stranger); other.MechanicEnabled != review.DefaultMechanicEnabled ||
		other.BurstCount != nil {
		t.Errorf("one user's settings change is visible to another: %s", describePrefs(other))
	}
}

// ---------------------------------------------------------------------------
// Row 47 — a value outside bounds is refused, not clamped
// ---------------------------------------------------------------------------

// TestReviewPrefs_OutOfRangeIsRefusedNotClamped — row 47 (SC-022, SC-027).
//
// Not only the refusal code is asserted, but also that state is
// UNCHANGED: a silent clamp would return 200 and write "the nearest
// allowed", and a test looking only at the code would not notice.
// So every probe compares a snapshot of the settings before and after.
func TestReviewPrefs_OutOfRangeIsRefusedNotClamped(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	user, _ := testsupport.SeedUser(t, pool, "user")

	// A meaningful initial state: if the refusal still wrote something,
	// the divergence will be visible on concrete numbers, not on "empty".
	putReviewPrefsOK(t, pool, user, fmt.Sprintf(
		`{"burstCount":%d,"unacceptedIntervalMinutes":%d,"unreviewedIntervalMinutes":%d}`,
		review.DefaultBurstCount, review.DefaultUnacceptedIntervalMinutes,
		review.DefaultUnreviewedIntervalMinutes))
	before := callGetReviewPrefs(t, pool, user)

	cases := []struct {
		name      string
		body      string
		wantParam string
	}{
		{"burst above the ceiling",
			fmt.Sprintf(`{"burstCount":%d}`, review.MaxBurstCount+1), "burstCount"},
		{"burst below the floor",
			fmt.Sprintf(`{"burstCount":%d}`, review.MinBurstCount-1), "burstCount"},
		{"unaccepted below the floor",
			fmt.Sprintf(`{"unacceptedIntervalMinutes":%d}`, review.MinIntervalMinutes-1),
			"unacceptedIntervalMinutes"},
		{"unaccepted as zero",
			`{"unacceptedIntervalMinutes":0}`, "unacceptedIntervalMinutes"},
		{"unaccepted above the ceiling",
			fmt.Sprintf(`{"unacceptedIntervalMinutes":%d}`, review.MaxIntervalMinutes+1),
			"unacceptedIntervalMinutes"},
		{"unreviewed below the floor",
			fmt.Sprintf(`{"unreviewedIntervalMinutes":%d}`, review.MinIntervalMinutes-1),
			"unreviewedIntervalMinutes"},
		{"unreviewed above the ceiling",
			fmt.Sprintf(`{"unreviewedIntervalMinutes":%d}`, review.MaxIntervalMinutes+1),
			"unreviewedIntervalMinutes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := putReviewPrefs(t, pool, user, tc.body)
			if status != http.StatusUnprocessableEntity {
				t.Errorf("PUT %s = %d, want 422 — a value outside bounds must "+
					"be refused as a business error, not silently clamped (SC-022, SC-027): %v",
					tc.body, status, body)
			}
			if got := body["error"]; got != "invalid_schedule_setting" {
				t.Errorf("refusal code = %v, want invalid_schedule_setting", got)
			}
			if got := body["param"]; got != tc.wantParam {
				t.Errorf("the refusal named field %v, want %q — without a field name the form "+
					"can only redden as a whole", got, tc.wantParam)
			}
			if after := callGetReviewPrefs(t, pool, user); !reflect.DeepEqual(after, before) {
				t.Errorf("the refused write changed settings.\n  was: %s\n  now: %s",
					describePrefs(before), describePrefs(after))
			}
		})
	}
}

// TestReviewPrefs_RefusalIsAllOrNothing — a refusal on ONE field does
// not apply neighbouring fields of the same request.
//
// A half-applied request would leave the person in a state they did not
// ask for, and the answer would still be a refusal — so they would not
// learn about the partial write at all.
func TestReviewPrefs_RefusalIsAllOrNothing(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	user, _ := testsupport.SeedUser(t, pool, "user")
	before := callGetReviewPrefs(t, pool, user)

	status, body := putReviewPrefs(t, pool, user, fmt.Sprintf(
		`{"mechanicEnabled":false,"burstCount":%d,"unacceptedIntervalMinutes":%d}`,
		review.MaxBurstCount, review.MinIntervalMinutes-1))
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("mixed request with one illegal value = %d, want 422: %v", status, body)
	}
	after := callGetReviewPrefs(t, pool, user)
	if !reflect.DeepEqual(after, before) {
		t.Errorf("the refused request applied some of the fields.\n  was: %s\n  now: %s",
			describePrefs(before), describePrefs(after))
	}
}

// TestReviewPrefs_MalformedBodyIsFormError — a sloppy body shape stays
// 400 and is not promoted to a business refusal.
//
// The split matters for monitoring: 422 is a routine situation, 400 is a
// broken client. Mixing them would drown real breakages in noise.
func TestReviewPrefs_MalformedBodyIsFormError(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	user, _ := testsupport.SeedUser(t, pool, "user")

	for _, raw := range []string{
		`{"burstCount":"3"}`,
		`{"unacceptedIntervalMinutes":180.5}`,
		`{"mechanicEnabled":"yes"}`,
		`{"effective":{"burstCount":3}}`,
		`{`,
	} {
		status, body := putReviewPrefs(t, pool, user, raw)
		if status != http.StatusBadRequest {
			t.Errorf("PUT %s = %d, want 400 (body shape): %v", raw, status, body)
		}
	}
	if n := countPrefsRows(t, pool, user); n != 0 {
		t.Errorf("a body rejected on shape created a prefs row (%d)", n)
	}
}

// ---------------------------------------------------------------------------
// Row 114 — "empty" and "explicit zero" are different things
// ---------------------------------------------------------------------------

// TestReviewPrefs_OmittedFieldKeepsFactoryAndIsNotZero — row 114, first
// half.
//
// An omitted field must keep its value, and on an untouched setting —
// factory. Turning omitted into zero is observable differently for the
// three parameters: for burst, zero is allowed (and so would silently
// pass as a setting), for the intervals it is outside bounds (and so
// the write would simply fail). All three are checked.
func TestReviewPrefs_OmittedFieldKeepsFactoryAndIsNotZero(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	user, _ := testsupport.SeedUser(t, pool, "user")

	// A write that names none of the schedule parameters.
	putReviewPrefsOK(t, pool, user, `{"mechanicEnabled":false}`)

	got := callGetReviewPrefs(t, pool, user)
	if got.BurstCount != nil || got.UnacceptedIntervalMinutes != nil || got.UnreviewedIntervalMinutes != nil {
		t.Errorf("omitted schedule parameters received a value: %s", describePrefs(got))
	}
	if got.Effective != factorySchedule() {
		t.Errorf("effective schedule %+v, want factory %+v — an omitted field "+
			"must leave factory, not turn into zero",
			got.Effective, factorySchedule())
	}
	if got.MechanicEnabled {
		t.Errorf("precondition: the switch did not persist, the probe wrote nothing: %s", describePrefs(got))
	}

	// A stored value is not reset by an omitted field.
	putReviewPrefsOK(t, pool, user, fmt.Sprintf(`{"burstCount":%d}`, review.MaxBurstCount))
	putReviewPrefsOK(t, pool, user, `{"reviewRequiredDefault":false}`)
	got = callGetReviewPrefs(t, pool, user)
	if got.BurstCount == nil || *got.BurstCount != review.MaxBurstCount {
		t.Errorf("the next write wiped a field it did not name: %s", describePrefs(got))
	}
}

// TestReviewPrefs_PartialWriteKeepsEveryOtherColumn — the UPDATE branch
// of the upsert is checked for EACH of the five columns, not for two.
//
// Why a separate test. Almost every test in the file takes a fresh user,
// so the first successful write lands in the INSERT, not in the
// on-conflict update. A flipped update branch ("an omitted field writes
// factory instead of stored") is invisible in that setup: there is
// nothing to overwrite. Here NON-factory values of all five are saved
// first, and only then comes a partial write — then any flipped branch
// wipes a neighbour.
func TestReviewPrefs_PartialWriteKeepsEveryOtherColumn(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	user, _ := testsupport.SeedUser(t, pool, "user")

	// All five are non-factory, so wiping is distinguishable from coincidence.
	putReviewPrefsOK(t, pool, user, fmt.Sprintf(
		`{"mechanicEnabled":false,"reviewRequiredDefault":false,`+
			`"burstCount":%d,"unacceptedIntervalMinutes":%d,"unreviewedIntervalMinutes":%d}`,
		review.MaxBurstCount, review.MinIntervalMinutes, review.MinIntervalMinutes))

	before := callGetReviewPrefs(t, pool, user)
	if before.MechanicEnabled || before.ReviewRequiredDefault ||
		before.BurstCount == nil || before.UnacceptedIntervalMinutes == nil || before.UnreviewedIntervalMinutes == nil {
		t.Fatalf("precondition: not all five persisted, there is nothing to check: %s", describePrefs(before))
	}

	// A partial write naming EXACTLY ONE field. Each pass touches its
	// own, and the assertion is about the other four.
	for _, step := range []struct {
		name string
		body string
	}{
		{"the switch", `{"mechanicEnabled":false}`},
		{"the flag default", `{"reviewRequiredDefault":false}`},
		{"unaccepted interval", fmt.Sprintf(`{"unacceptedIntervalMinutes":%d}`, review.MinIntervalMinutes)},
		{"unreviewed interval", fmt.Sprintf(`{"unreviewedIntervalMinutes":%d}`, review.MinIntervalMinutes)},
		{"burst count", fmt.Sprintf(`{"burstCount":%d}`, review.MaxBurstCount)},
	} {
		putReviewPrefsOK(t, pool, user, step.body)
		got := callGetReviewPrefs(t, pool, user)
		// Compared via DeepEqual, not the operator: the DTO carries
		// pointers to optional fields, and "!=" would compare addresses,
		// not values.
		if !reflect.DeepEqual(got, before) {
			t.Errorf("a write that named only %q changed neighbouring columns.\n"+
				"was:  %s\nnow: %s\n"+
				"the upsert's update branch must leave an unnamed field as-is, "+
				"not write factory into it", step.name, describePrefs(before), describePrefs(got))
		}
	}
}

// TestReviewPrefs_ExplicitNullClearsToFactory — "empty" is expressed by
// an explicit null and is distinguishable from "do not touch".
//
// Without this state there would be nothing to reset a parameter to
// factory with: the client could only write the current factory number
// by hand, and it would stop following the contract when the contract
// changed.
func TestReviewPrefs_ExplicitNullClearsToFactory(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	user, _ := testsupport.SeedUser(t, pool, "user")

	putReviewPrefsOK(t, pool, user, fmt.Sprintf(
		`{"burstCount":%d,"unreviewedIntervalMinutes":%d}`,
		review.MaxBurstCount, review.MinIntervalMinutes))
	if got := callGetReviewPrefs(t, pool, user); got.BurstCount == nil {
		t.Fatalf("precondition: the value did not persist: %s", describePrefs(got))
	}

	putReviewPrefsOK(t, pool, user, `{"burstCount":null}`)
	got := callGetReviewPrefs(t, pool, user)
	if got.BurstCount != nil {
		t.Errorf("explicit null did not return the field to empty: %s", describePrefs(got))
	}
	if got.Effective.BurstCount != review.DefaultBurstCount {
		t.Errorf("after the clear the effective burst is %d, factory %d",
			got.Effective.BurstCount, review.DefaultBurstCount)
	}
	// A neighbouring field not named in the body stayed in place.
	if got.UnreviewedIntervalMinutes == nil || *got.UnreviewedIntervalMinutes != review.MinIntervalMinutes {
		t.Errorf("clearing one field touched a neighbour: %s", describePrefs(got))
	}
}

// TestReviewPrefs_ExplicitZeroBurstIsStoredAndDegenerate — row 114,
// second half: an explicit zero is stored as zero and yields a
// degenerate scale with no burst.
//
// Not only storage of the number is checked, but also its CONSEQUENCE:
// a schedule assembled from the effective values places the first mark
// through the background interval from the base, not through a burst
// segment. An assertion about storage alone would also pass on a zero
// that the schedule silently treats as factory three.
func TestReviewPrefs_ExplicitZeroBurstIsStoredAndDegenerate(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	user, _ := testsupport.SeedUser(t, pool, "user")

	putReviewPrefsOK(t, pool, user, `{"burstCount":0}`)

	got := callGetReviewPrefs(t, pool, user)
	if got.BurstCount == nil {
		t.Fatalf("explicit zero stored as empty — the distinction for which the schema "+
			"column is declared NULLABLE is lost: %s", describePrefs(got))
	}
	if *got.BurstCount != 0 || got.Effective.BurstCount != 0 {
		t.Fatalf("explicit zero did not persist: %s", describePrefs(got))
	}

	settings, err := review.ResolveMinutes(got.BurstCount,
		got.UnacceptedIntervalMinutes, got.UnreviewedIntervalMinutes)
	if err != nil {
		t.Fatalf("the stored schedule is not resolved by the contract: %v", err)
	}
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	first, err := settings.MarkAt(review.CategoryUnaccepted, base, 1)
	if err != nil {
		t.Fatalf("the first mark was not computed: %v", err)
	}
	wantFirst := base.Add(time.Duration(got.Effective.UnacceptedIntervalMinutes) * time.Minute)
	if !first.Equal(wantFirst) {
		t.Errorf("first mark %s, want %s — with zero reminders in the first hour "+
			"the background starts through its own interval from the base, there is no burst",
			first.Format(time.RFC3339), wantFirst.Format(time.RFC3339))
	}
}

// ---------------------------------------------------------------------------
// T010 gate — switching off does not delete data
// ---------------------------------------------------------------------------

// TestReviewPrefs_SwitchingMechanicOffKeepsData — the half of row 48
// available today.
//
// "Switching off immediately empties the set" cannot be checked: of the
// three manifestations zero exist — the filter is introduced by T012,
// the second number by T013, the reminder selection by T017/T018. The
// other half of the promise is checkable already and is checked here:
// the switch is a LENS, not a deletion. Flags, marks and their
// authorship must survive switching off and come back on switching on,
// otherwise "I'll switch it back on" would be an irreversible loss of
// someone else's work.
func TestReviewPrefs_SwitchingMechanicOffKeepsData(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	author, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, author, "revprefs")
	issueID := testsupport.SeedIssue(t, pool, pid, author, prefix+"-1", "closed result", "closed", 2)

	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
UPDATE issues
   SET review_required = true, reviewed_at = now(), reviewed_by = $2
 WHERE id = $1`, issueID, author); err != nil {
		t.Fatalf("seed review state: %v", err)
	}

	type reviewRow struct {
		Required bool
		At       *time.Time
		By       *uuid.UUID
	}
	read := func() reviewRow {
		t.Helper()
		var r reviewRow
		if err := pool.QueryRow(ctx,
			`SELECT review_required, reviewed_at, reviewed_by FROM issues WHERE id = $1`, issueID).
			Scan(&r.Required, &r.At, &r.By); err != nil {
			t.Fatalf("read review columns: %v", err)
		}
		return r
	}
	before := read()
	if !before.Required || before.At == nil || before.By == nil {
		t.Fatalf("precondition: review state is not seeded: %+v", before)
	}

	putReviewPrefsOK(t, pool, author, `{"mechanicEnabled":false}`)
	off := read()
	if off.Required != before.Required || off.At == nil || !off.At.Equal(*before.At) ||
		off.By == nil || *off.By != *before.By {
		t.Fatalf("switching the mechanic off changed the issue data: was %+v, now %+v — "+
			"the switch must be a lens, not a deletion (INV-6)", before, off)
	}

	putReviewPrefsOK(t, pool, author, `{"mechanicEnabled":true}`)
	on := read()
	if on.Required != before.Required || on.At == nil || !on.At.Equal(*before.At) ||
		on.By == nil || *on.By != *before.By {
		t.Errorf("switching the mechanic on did not restore the previous state: was %+v, now %+v", before, on)
	}
	if got := callGetReviewPrefs(t, pool, author); !got.MechanicEnabled {
		t.Errorf("the switch did not return to the on position: %s", describePrefs(got))
	}
}
