package handler

// notification_reasons_rest_test.go — row 5 of the sk-303.13 coverage
// matrix (plan task T004): the settings-form contract on a real schema.
//
// What is checked is the reason the change was started: the form has no
// reason without delivery, the channels are independent, a partial
// update does not wipe what the client does not know about, and one
// person's settings are not available to another.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/notifications"
	"skein-api/internal/testsupport"
)

type reasonChannelBody struct {
	Applicable bool   `json:"applicable"`
	Audience   string `json:"audience"`
	Enabled    bool   `json:"enabled"`
}

type reasonRowBody struct {
	Key   string            `json:"key"`
	Label string            `json:"label"`
	Hint  string            `json:"hint"`
	Feed  reasonChannelBody `json:"feed"`
	Push  reasonChannelBody `json:"push"`
}

type reasonPrefsBody struct {
	Reasons              []reasonRowBody `json:"reasons"`
	SuppressAgentActions bool            `json:"suppressAgentActions"`
	UnwatchOnClose       bool            `json:"unwatchOnClose"`
}

func (b reasonPrefsBody) byKey(key string) (reasonRowBody, bool) {
	for _, r := range b.Reasons {
		if r.Key == key {
			return r, true
		}
	}
	return reasonRowBody{}, false
}

func callGetReasonPrefs(t *testing.T, pool *pgxpool.Pool, caller uuid.UUID) reasonPrefsBody {
	t.Helper()
	h := NewNotificationsHandler(pool)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/notification-reasons", nil)
	ctx := auth.WithUserID(req.Context(), caller)
	ctx = auth.WithUserRole(ctx, "user")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	h.GetReasonPrefs(rec, req)
	res := rec.Result()
	t.Cleanup(func() { _ = res.Body.Close() })
	if res.StatusCode != http.StatusOK {
		var problem map[string]any
		_ = json.NewDecoder(res.Body).Decode(&problem)
		t.Fatalf("GET /me/notification-reasons = %d: %v", res.StatusCode, problem)
	}
	var body reasonPrefsBody
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

// callPutReasonPrefs sends a raw body so malformed input can be exercised too,
// and returns the status plus the decoded payload (response or problem).
func callPutReasonPrefs(t *testing.T, pool *pgxpool.Pool, caller uuid.UUID, raw string) (int, map[string]any) {
	t.Helper()
	h := NewNotificationsHandler(pool)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/me/notification-reasons",
		bytes.NewBufferString(raw))
	req.Header.Set("Content-Type", "application/json")
	ctx := auth.WithUserID(req.Context(), caller)
	ctx = auth.WithUserRole(ctx, "user")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	h.PutReasonPrefs(rec, req)
	res := rec.Result()
	t.Cleanup(func() { _ = res.Body.Close() })
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body
}

// TestReasonPrefsREST_ActiveOnlyNarrowDefault — the form returns only
// active reasons (no reserved ones in it), each channel separately says
// whether it applies and whom it reaches, and a fresh account sees the
// narrow default.
func TestReasonPrefsREST_ActiveOnlyNarrowDefault(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, _ := testsupport.SeedUser(t, pool, "user")

	body := callGetReasonPrefs(t, pool, caller)

	// Composition: exactly fifteen active reasons of the spec, not one more.
	// Eleven from the start, two reminder categories moved later to
	// active (sk-303.14 / T005), "Closed issue outcome changed" (sk-489 /
	// US-3) and "Space invitation" (sk-674.18 T040 / US-5, push only). The number and list are written here as literals on purpose: a comparison with
	// ActiveReasons() would miss a sixteenth "active" reason added
	// to the registry together with a mistake — the test must speak for the spec, not
	// retell the code it checks.
	specActive := []string{
		"assignment", "accepted", "delegated_status", "reminder", "mention",
		"issue_created", "comment", "attachment", "reparent", "auto_unclaim",
		"other_edits",
		"unaccepted_reminder", "review_reminder",
		"outcome_changed",
		"space_invitation",
	}
	if len(body.Reasons) != 15 {
		t.Fatalf("form returned %d reasons, the spec has 15 active", len(body.Reasons))
	}
	for _, key := range specActive {
		if _, ok := body.byKey(key); !ok {
			t.Errorf("active reason %s of the spec is missing from the form", key)
		}
	}
	active := notifications.ActiveReasons()
	if len(active) != len(specActive) {
		t.Fatalf("registry has %d active reasons, the spec has %d", len(active), len(specActive))
	}
	for _, r := range active {
		row, ok := body.byKey(string(r.Key))
		if !ok {
			t.Errorf("active reason %s is missing from the form", r.Key)
			continue
		}
		if row.Label == "" {
			t.Errorf("%s: form row has no label — the client renders what the server returns", r.Key)
		}
		// The same half as for the label, and for the same reason: the registry may
		// carry a hint, while the projection to the wire may lose it. The registry guard
		// (notifications/reasons_test.go#TestRegistryMatchesSpec) knows nothing
		// about the wire.
		if row.Hint == "" {
			t.Errorf("%s: form row has no hint — the toggle would arrive without an explanation of "+
				"to whom and for which issues the reason is delivered", r.Key)
		}
		if row.Feed.Applicable != r.Feed.Applicable || row.Push.Applicable != r.Push.Applicable {
			t.Errorf("%s: applicability feed:%v push:%v disagrees with the registry (feed:%v push:%v)",
				r.Key, row.Feed.Applicable, row.Push.Applicable, r.Feed.Applicable, r.Push.Applicable)
		}
		// Audience is written on EVERY applicable channel: the surface
		// does not inherit reach silently.
		if row.Feed.Applicable && row.Feed.Audience == "" {
			t.Errorf("%s: applicable feed channel without an audience", r.Key)
		}
		if row.Push.Applicable && row.Push.Audience == "" {
			t.Errorf("%s: applicable push channel without an audience", r.Key)
		}
		// An inapplicable channel is a dash, not a switch that is off: it
		// never arrives enabled.
		if !row.Feed.Applicable && row.Feed.Enabled {
			t.Errorf("%s: inapplicable feed channel came back enabled", r.Key)
		}
		if !row.Push.Applicable && row.Push.Enabled {
			t.Errorf("%s: inapplicable push channel came back enabled", r.Key)
		}
	}

	// A reserved reason does not get a toggle. Both reminder categories are active, so there is currently no reserve in the
	// registry — the rule is checked from the REGISTRY, not from two
	// names written by hand: otherwise after activation the test would
	// assert about an empty set and stay silent about the next reserve.
	reservedSeen := 0
	for _, r := range notifications.AllReasons() {
		if r.State != notifications.ReasonStateReserved {
			continue
		}
		reservedSeen++
		if _, ok := body.byKey(string(r.Key)); ok {
			t.Errorf("reserved reason %s leaked into the form", r.Key)
		}
	}
	// And the other half of the same transition: both reminder categories
	// ARE in the form. Without this assertion "the reserve does not leak"
	// would stay green even in the case where there are simply no reasons.
	for _, activated := range []string{"unaccepted_reminder", "review_reminder"} {
		row, ok := body.byKey(activated)
		if !ok {
			t.Errorf("reason %s was activated but the form does not show it", activated)
			continue
		}
		if !row.Push.Applicable || row.Push.Audience != "digest_recipient" {
			t.Errorf("%s: push channel applicable=%v audience=%q, want an applicable digest_recipient channel",
				activated, row.Push.Applicable, row.Push.Audience)
		}
		if row.Feed.Applicable {
			t.Errorf("%s: feed channel is applicable — a digest is not shown in the feed", activated)
		}
	}
	t.Logf("registry currently reserves %d reason(s)", reservedSeen)

	// Narrow default: a fresh account has exactly nine visible reasons enabled.
	// Before the reminders were activated there were five — seven in storage minus two reserved;
	// after both categories were activated the numbers matched at seven, sk-489 added
	// an eighth ("Closed issue outcome changed", on for both channels),
	// sk-674.18 T040 — a ninth ("Space invitation", push only).
	// The storage number is not retold here: it is checked by its own
	// test in internal/notifications.
	enabled := 0
	for _, row := range body.Reasons {
		if row.Feed.Enabled || row.Push.Enabled {
			enabled++
		}
	}
	if enabled != 9 {
		t.Errorf("a fresh account shows %d enabled reasons in the form, want 9", enabled)
	}
	// And no reason ended up enabled on both channels "just in case"
	// where the default does not provide for that.
	for _, r := range active {
		row, _ := body.byKey(string(r.Key))
		if row.Feed.Enabled != r.Feed.DefaultOn || row.Push.Enabled != r.Push.DefaultOn {
			t.Errorf("%s: fresh account state feed:%v push:%v, registry default feed:%v push:%v",
				r.Key, row.Feed.Enabled, row.Push.Enabled, r.Feed.DefaultOn, r.Push.DefaultOn)
		}
	}
}

// TestReasonPrefsREST_EveryFormRowIsSettable — the API and the form MUST
// be ONE surface: everything the form shows as a toggle, the write MUST
// accept.
//
// The assertion was introduced by sk-303.14 / T005 in place of
// lost coverage. Before it, "a reserved reason is rejected" was checked
// on the name unaccepted_reminder; after activating both categories there
// is no reserve left in the registry, and that case is substituted from
// the registry — that is, today it does not run at all. The other half of
// the same invariant always runs and is today the one with content: the
// two just-activated reasons MUST be accepted by the write, not only
// shown on the read. An activation that reached the form but not the
// write would give a toggle that cannot be switched.
func TestReasonPrefsREST_EveryFormRowIsSettable(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, _ := testsupport.SeedUser(t, pool, "user")

	form := callGetReasonPrefs(t, pool, caller)
	if len(form.Reasons) == 0 {
		t.Fatal("the form returned no rows — the assertion would be inert")
	}

	type update struct {
		Reason string `json:"reason"`
		Feed   *bool  `json:"feed,omitempty"`
		Push   *bool  `json:"push,omitempty"`
	}
	var ups []update
	for _, row := range form.Reasons {
		u := update{Reason: row.Key}
		// An inapplicable channel is not named at all: it cannot be
		// switched on, and switching an inapplicable one off checks
		// nothing.
		if row.Feed.Applicable {
			v := row.Feed.Enabled
			u.Feed = &v
		}
		if row.Push.Applicable {
			v := row.Push.Enabled
			u.Push = &v
		}
		ups = append(ups, u)
	}
	raw, err := json.Marshal(map[string]any{"reasons": ups})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	if code, body := callPutReasonPrefs(t, pool, caller, string(raw)); code != http.StatusOK {
		t.Fatalf("PUT naming every form row = %d: %v — a row the form shows must be settable",
			code, body)
	}

	// The check is not tautological only if both reminder categories were in the body.
	named := map[string]bool{}
	for _, u := range ups {
		named[u.Reason] = true
	}
	for _, activated := range []string{"unaccepted_reminder", "review_reminder"} {
		if !named[activated] {
			t.Errorf("%s was not part of the accepted body — the assertion says nothing about it", activated)
		}
	}
}

// TestReasonPrefsREST_ChannelsIndependent — SC-004: switching the feed
// channel off does not cancel push, and vice versa. A reason's channels
// are independent, not one toggle with two lights.
func TestReasonPrefsREST_ChannelsIndependent(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, _ := testsupport.SeedUser(t, pool, "user")

	// Mention arrives with both channels on by default.
	before, ok := callGetReasonPrefs(t, pool, caller).byKey("mention")
	if !ok {
		t.Fatal("mention row missing from the form")
	}
	if !before.Feed.Enabled || !before.Push.Enabled {
		t.Fatalf("mention default feed:%v push:%v, want both on", before.Feed.Enabled, before.Push.Enabled)
	}

	// Switch only the feed off.
	if code, body := callPutReasonPrefs(t, pool, caller,
		`{"reasons":[{"reason":"mention","feed":false}]}`); code != http.StatusOK {
		t.Fatalf("PUT (feed off) = %d: %v", code, body)
	}
	after, _ := callGetReasonPrefs(t, pool, caller).byKey("mention")
	if after.Feed.Enabled {
		t.Error("mention feed stayed on after switching it off")
	}
	if !after.Push.Enabled {
		t.Error("switching the feed off also switched the push off — the channels are not independent")
	}

	// Switch only the push off, the feed stays off — and conversely switch the feed on.
	if code, body := callPutReasonPrefs(t, pool, caller,
		`{"reasons":[{"reason":"mention","feed":true,"push":false}]}`); code != http.StatusOK {
		t.Fatalf("PUT (push off) = %d: %v", code, body)
	}
	after, _ = callGetReasonPrefs(t, pool, caller).byKey("mention")
	if !after.Feed.Enabled {
		t.Error("mention feed did not come back on")
	}
	if after.Push.Enabled {
		t.Error("mention push stayed on after switching it off")
	}
}

// TestReasonPrefsREST_PartialUpdatePreservesUnnamed — the update is
// partial: a client that does not know about a reason has no right to
// wipe it. That is "the unfamiliar is not wiped on save" on the server
// side.
func TestReasonPrefsREST_PartialUpdatePreservesUnnamed(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, _ := testsupport.SeedUser(t, pool, "user")

	// The user configured three reasons their own way.
	if code, body := callPutReasonPrefs(t, pool, caller, `{"reasons":[
        {"reason":"comment","feed":true},
        {"reason":"issue_created","feed":true},
        {"reason":"assignment","feed":false,"push":false}
    ]}`); code != http.StatusOK {
		t.Fatalf("PUT (setup) = %d: %v", code, body)
	}

	// Then an older client arrived and saved ONE reason, not knowing about the others.
	if code, body := callPutReasonPrefs(t, pool, caller,
		`{"reasons":[{"reason":"reparent","feed":true}]}`); code != http.StatusOK {
		t.Fatalf("PUT (partial) = %d: %v", code, body)
	}

	got := callGetReasonPrefs(t, pool, caller)
	for _, tc := range []struct {
		key       string
		wantFeed  bool
		wantPush  bool
		explained string
	}{
		{"comment", true, false, "explicitly switched on earlier"},
		{"issue_created", true, false, "explicitly switched on earlier"},
		{"assignment", false, false, "explicitly switched off earlier"},
		{"reparent", true, false, "the reason this last request named"},
	} {
		row, ok := got.byKey(tc.key)
		if !ok {
			t.Errorf("%s missing from the form", tc.key)
			continue
		}
		if row.Feed.Enabled != tc.wantFeed || row.Push.Enabled != tc.wantPush {
			t.Errorf("%s: feed:%v push:%v, want feed:%v push:%v (%s)",
				tc.key, row.Feed.Enabled, row.Push.Enabled, tc.wantFeed, tc.wantPush, tc.explained)
		}
	}

	// Partiality also acts INSIDE the row: for a named reason, a channel
	// the body does not mention keeps ITS OWN state, and does not roll
	// back to the registry default. Checked on a reason whose stored
	// state differs from the default — otherwise a swap of "stored →
	// default" would be indistinguishable.
	if code, body := callPutReasonPrefs(t, pool, caller,
		`{"reasons":[{"reason":"assignment","push":true}]}`); code != http.StatusOK {
		t.Fatalf("PUT (single channel) = %d: %v", code, body)
	}
	if row, _ := callGetReasonPrefs(t, pool, caller).byKey("assignment"); row.Feed.Enabled {
		t.Error("naming only the push channel reset the feed channel to the registry default; " +
			"the unmentioned channel must keep its stored state")
	} else if !row.Push.Enabled {
		t.Error("the named channel did not take the requested value")
	}

	// And suppressAgentActions lives its own life beside the table.
	if code, body := callPutReasonPrefs(t, pool, caller,
		`{"suppressAgentActions":true}`); code != http.StatusOK {
		t.Fatalf("PUT (suppress) = %d: %v", code, body)
	}
	got = callGetReasonPrefs(t, pool, caller)
	if !got.SuppressAgentActions {
		t.Error("suppressAgentActions did not persist")
	}
	if row, _ := got.byKey("comment"); !row.Feed.Enabled {
		t.Error("a body that named no reasons still reset a reason state")
	}
}

// TestReasonPrefsREST_BusinessErrorsAre422 — well-formed JSON with a
// business error is 422 and writes NOTHING; a syntactic error is 400.
func TestReasonPrefsREST_BusinessErrorsAre422(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, _ := testsupport.SeedUser(t, pool, "user")

	// Baseline state that no refusal has the right to change.
	if code, body := callPutReasonPrefs(t, pool, caller,
		`{"reasons":[{"reason":"comment","feed":true}]}`); code != http.StatusOK {
		t.Fatalf("PUT (setup) = %d: %v", code, body)
	}

	cases := []struct {
		name string
		body string
		want int
		code string
	}{
		{"unknown reason", `{"reasons":[{"reason":"telepathy","feed":true}]}`,
			http.StatusUnprocessableEntity, "unknown_reason"},
		{"inapplicable channel switched on",
			`{"reasons":[{"reason":"reminder","feed":true}]}`,
			http.StatusUnprocessableEntity, "channel_not_applicable"},
		{"malformed json", `{"reasons":[`, http.StatusBadRequest, "invalid_json"},
		{"reasons is not an array", `{"reasons":"comment"}`, http.StatusBadRequest, "invalid_json"},
		{"empty reason key", `{"reasons":[{"feed":true}]}`, http.StatusBadRequest, "validation_error"},
	}
	// A reserved reason is the second surface of the same toggle without
	// an effect: it has no form, and the API does not accept it either.
	// Both reminder categories are active, so there is currently
	// NO reserve in the registry and the case is substituted from the
	// registry, not hardcoded: the name of an activated reason would
	// check exactly the opposite — that it is accepted.
	for _, r := range notifications.AllReasons() {
		if r.State != notifications.ReasonStateReserved {
			continue
		}
		cases = append(cases, struct {
			name string
			body string
			want int
			code string
		}{
			"reserved reason",
			`{"reasons":[{"reason":"` + string(r.Key) + `","push":false}]}`,
			http.StatusUnprocessableEntity, "reason_not_settable",
		})
		break
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := callPutReasonPrefs(t, pool, caller, tc.body)
			if code != tc.want {
				t.Fatalf("status = %d, want %d (body %v)", code, tc.want, body)
			}
			// The error code must be named: the client needs to tell
			// "there is no such reason" from "this channel cannot be
			// switched on", not parse the message text.
			if got, _ := body["error"].(string); got != tc.code {
				t.Errorf("problem error = %v, want %q (body %v)", body["error"], tc.code, body)
			}
		})
	}

	// No refusal changed state: a partially applied setting would leave
	// the user in a state they did not ask for.
	got := callGetReasonPrefs(t, pool, caller)
	if row, _ := got.byKey("comment"); !row.Feed.Enabled {
		t.Error("a rejected request rolled back more than itself — comment/feed was reset")
	}
	if row, _ := got.byKey("reminder"); row.Feed.Enabled {
		t.Error("the rejected inapplicable channel was stored anyway")
	}
	var stored int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM user_notification_reason_prefs
 WHERE user_id = $1 AND reason = 'telepathy'`, caller).Scan(&stored); err != nil {
		t.Fatalf("probe unknown reason row: %v", err)
	}
	if stored != 0 {
		t.Error("the unknown reason was written to storage despite the 422")
	}
}

// TestReasonPrefsREST_OtherUsersSettingsAreInvisible — one user's
// settings are not available to another: the form is caller-scoped by
// construction, and a neighbour's change is not visible in the slightest.
func TestReasonPrefsREST_OtherUsersSettingsAreInvisible(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	alice, _ := testsupport.SeedUser(t, pool, "user")
	bob, _ := testsupport.SeedUser(t, pool, "user")

	// Alice turns everything she can to the opposite of the default.
	// suppressAgentActions has been on by default since sk-343, so
	// "the opposite way" here means false: write true for alice — and
	// the check below would pass on any value, because bob is already true.
	if code, body := callPutReasonPrefs(t, pool, alice, `{"reasons":[
        {"reason":"comment","feed":true},
        {"reason":"assignment","feed":false,"push":false},
        {"reason":"mention","feed":false,"push":false}
    ],"suppressAgentActions":false}`); code != http.StatusOK {
		t.Fatalf("alice PUT = %d: %v", code, body)
	}

	bobs := callGetReasonPrefs(t, pool, bob)
	if !bobs.SuppressAgentActions {
		t.Error("bob inherited alice's suppressAgentActions")
	}
	for _, tc := range []struct{ key string }{{"comment"}, {"assignment"}, {"mention"}} {
		row, ok := bobs.byKey(tc.key)
		if !ok {
			t.Fatalf("%s missing from bob's form", tc.key)
		}
		reg, _ := notifications.LookupReason(notifications.ReasonKey(tc.key))
		if row.Feed.Enabled != reg.Feed.DefaultOn || row.Push.Enabled != reg.Push.DefaultOn {
			t.Errorf("bob sees %s as feed:%v push:%v — alice's change leaked; want the default feed:%v push:%v",
				tc.key, row.Feed.Enabled, row.Push.Enabled, reg.Feed.DefaultOn, reg.Push.DefaultOn)
		}
	}

	// And the other way: alice still has her own state.
	alices := callGetReasonPrefs(t, pool, alice)
	if row, _ := alices.byKey("comment"); !row.Feed.Enabled {
		t.Error("alice lost her own setting")
	}
}

// TestReasonPrefsREST_StoredStateForInapplicableChannelStaysOff — even if
// an inapplicable channel's cell somehow ended up enabled in storage, the
// form will not show it enabled: applicability is decided by the registry,
// not by the row.
func TestReasonPrefsREST_StoredStateForInapplicableChannelStaysOff(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	caller, _ := testsupport.SeedUser(t, pool, "user")

	if _, err := pool.Exec(context.Background(), `
INSERT INTO user_notification_reason_prefs (user_id, reason, feed_enabled, push_enabled)
VALUES ($1, 'reminder', true, true)
ON CONFLICT (user_id, reason) DO UPDATE SET feed_enabled = true, push_enabled = true`,
		caller); err != nil {
		t.Fatalf("seed inapplicable state: %v", err)
	}

	row, ok := callGetReasonPrefs(t, pool, caller).byKey("reminder")
	if !ok {
		t.Fatal("reminder row missing from the form")
	}
	if row.Feed.Applicable {
		t.Fatal("reminder feed channel became applicable")
	}
	if row.Feed.Enabled {
		t.Error("a stored true on an inapplicable channel came back as enabled")
	}
	if !row.Push.Enabled {
		t.Error("the applicable push channel of the same row was lost")
	}
}

// TestReasonPrefsREST_UnwatchOnCloseFlag — sk-551.10, item 5, default changed
// by sk-622.21 (US-2, test matrix row 26): the flag is ON for a fresh account,
// switching it off persists, a body that does not name it leaves it alone,
// and writing it does not drag the agent-action suppression (on by default,
// sk-343) off with it.
func TestReasonPrefsREST_UnwatchOnCloseFlag(t *testing.T) {
	pool := newMeTestPool(t)
	caller, _ := seedMeUser(t, pool, "user")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM user_notification_prefs WHERE user_id = $1`, caller)
	})

	if got := callGetReasonPrefs(t, pool, caller); !got.UnwatchOnClose {
		t.Fatal("unwatchOnClose is off for a fresh account, want on by default")
	}
	if code, body := callPutReasonPrefs(t, pool, caller, `{"unwatchOnClose":false}`); code != http.StatusOK {
		t.Fatalf("PUT (unwatchOnClose) = %d: %v", code, body)
	}
	got := callGetReasonPrefs(t, pool, caller)
	if got.UnwatchOnClose {
		t.Error("switching unwatchOnClose off did not persist")
	}
	if !got.SuppressAgentActions {
		t.Error("creating the prefs row for unwatchOnClose switched suppressAgentActions off")
	}
	if code, body := callPutReasonPrefs(t, pool, caller, `{"suppressAgentActions":false}`); code != http.StatusOK {
		t.Fatalf("PUT (suppress) = %d: %v", code, body)
	}
	if got := callGetReasonPrefs(t, pool, caller); got.UnwatchOnClose {
		t.Error("a body that did not name unwatchOnClose reset it")
	}
}
