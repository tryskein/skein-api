package signals

// contract_test.go — sk-622.21, test matrix row 1 (US-11, INV-12, the SHAPE
// half): the personal signal carries only `areas` — values from the seven
// literals of spec US-11 — and `sentAt`, no data; the reminder frame carries
// `firingId`. The reference is the spec text and plan T005/K14, written
// before this code: area literals inbox, mentions, watching, lens,
// unreviewed, tasks, projects; reminder kinds set, snoozed, rang, dismissed;
// firingId = reminderId@remindAt. The wire JSON is the contract, so every
// check decodes the marshalled bytes rather than reading the Go struct.
//
// sk-674.18, matrix row 198 (K-18, US-19): the area `spaces` is the one area
// with a payload — on the wire a signal naming it carries `spaces[]`, whose
// entries hold a reason from the ten literals of spec US-19 and the space id;
// a signal without that area keeps exactly `areas` and `sentAt`.

import (
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
)

// wireKeys marshals v and returns the sorted top-level keys of its JSON
// object, plus the decoded object.
func wireKeys(t *testing.T, v any) ([]string, map[string]json.RawMessage) {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, obj
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSignal_WireCarriesOnlyAreasAndSentAt — a signal naming every area but
// `spaces` has exactly two keys, and each area is written as its spec
// literal.
func TestSignal_WireCarriesOnlyAreasAndSentAt(t *testing.T) {
	sentAt := time.Date(2026, 10, 9, 10, 15, 3, 0, time.UTC)
	sig := Signal{
		Areas:  []Area{AreaInbox, AreaMentions, AreaWatching, AreaLens, AreaUnreviewed, AreaTasks, AreaProjects},
		SentAt: sentAt,
	}

	keys, obj := wireKeys(t, sig)
	if want := []string{"areas", "sentAt"}; !equalStrings(keys, want) {
		t.Fatalf("signal wire keys = %v, want exactly %v (a signal carries no data)", keys, want)
	}

	var areas []string
	if err := json.Unmarshal(obj["areas"], &areas); err != nil {
		t.Fatalf("areas is not an array of strings: %s: %v", obj["areas"], err)
	}
	wantAreas := []string{"inbox", "mentions", "watching", "lens", "unreviewed", "tasks", "projects"}
	if !equalStrings(areas, wantAreas) {
		t.Errorf("areas on the wire = %v, want the spec literals %v", areas, wantAreas)
	}

	var gotSentAt time.Time
	if err := json.Unmarshal(obj["sentAt"], &gotSentAt); err != nil {
		t.Fatalf("sentAt is not an RFC 3339 time: %s: %v", obj["sentAt"], err)
	}
	if !gotSentAt.Equal(sentAt) {
		t.Errorf("sentAt = %s, want %s", gotSentAt, sentAt)
	}
}

// TestReminderFrame_WireCarriesFiringID — the reminder frame has exactly the
// six contract keys, firingId among them, and its kinds are the four spec
// literals.
func TestReminderFrame_WireCarriesFiringID(t *testing.T) {
	reminderID := uuid.MustParse("0199a1b2-0000-7000-8000-000000000001")
	projectID := uuid.MustParse("0199a1b2-0000-7000-8000-0000000000a1")
	remindAt := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)

	for _, c := range []struct {
		kind ReminderKind
		wire string
	}{
		{ReminderSet, "set"},
		{ReminderSnoozed, "snoozed"},
		{ReminderRang, "rang"},
		{ReminderDismissed, "dismissed"},
	} {
		frame := ReminderFrame{
			ReminderID: reminderID,
			IssueID:    "sk-42",
			ProjectID:  projectID,
			Kind:       c.kind,
			RemindAt:   remindAt,
			FiringID:   FiringID(reminderID, remindAt),
		}
		keys, obj := wireKeys(t, frame)
		want := []string{"firingId", "issueId", "kind", "projectId", "remindAt", "reminderId"}
		if !equalStrings(keys, want) {
			t.Fatalf("kind %s: reminder frame wire keys = %v, want exactly %v", c.wire, keys, want)
		}
		var kind, firing string
		if err := json.Unmarshal(obj["kind"], &kind); err != nil {
			t.Fatalf("kind is not a string: %s", obj["kind"])
		}
		if kind != c.wire {
			t.Errorf("kind on the wire = %q, want %q", kind, c.wire)
		}
		if err := json.Unmarshal(obj["firingId"], &firing); err != nil {
			t.Fatalf("firingId is not a string: %s", obj["firingId"])
		}
		if wantFiring := "0199a1b2-0000-7000-8000-000000000001@2026-10-09T10:00:00Z"; firing != wantFiring {
			t.Errorf("firingId = %q, want %q (reminderId@remindAt)", firing, wantFiring)
		}
	}
}

// TestSignal_SpacesAreaCarriesSpacesOnTheWire — row 198: a signal naming the
// area `spaces` carries exactly `areas`, `spaces` and `sentAt`; each entry is
// {reason, spaceId[, spaceName]} with the reason written as its US-19
// literal; the ten literals are the closed set.
func TestSignal_SpacesAreaCarriesSpacesOnTheWire(t *testing.T) {
	wantReasons := []string{"invited", "accepted", "declined", "expired", "removed",
		"added", "owner_assigned", "owner_changed", "deleted", "renamed"}
	var reasons []SpaceReason
	for _, r := range wantReasons {
		reasons = append(reasons, SpaceReason(r))
	}
	for _, r := range reasons {
		if !r.Valid() {
			t.Errorf("reason %q from spec US-19 is not a valid SpaceReason", r)
		}
	}
	if SpaceReason("joined").Valid() {
		t.Errorf(`"joined" is accepted as a SpaceReason — the set must be closed`)
	}
	if !AreaSpaces.Valid() {
		t.Fatalf("area spaces is not a valid Area")
	}

	var events []SpaceEvent
	for i, r := range SpaceReasons() {
		e := SpaceEvent{Reason: r, SpaceID: "k3x9p2qa"}
		if i == 0 {
			e.SpaceName = "Acme"
		}
		events = append(events, e)
	}
	sig := Signal{
		Areas:  []Area{AreaSpaces, AreaInbox},
		Spaces: events,
		SentAt: time.Date(2026, 10, 12, 10, 15, 3, 0, time.UTC),
	}
	keys, obj := wireKeys(t, sig)
	if want := []string{"areas", "sentAt", "spaces"}; !equalStrings(keys, want) {
		t.Fatalf("spaces signal wire keys = %v, want exactly %v", keys, want)
	}

	var areas []string
	if err := json.Unmarshal(obj["areas"], &areas); err != nil {
		t.Fatalf("areas: %v", err)
	}
	if !equalStrings(areas, []string{"spaces", "inbox"}) {
		t.Errorf("areas on the wire = %v, want [spaces inbox]", areas)
	}

	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(obj["spaces"], &entries); err != nil {
		t.Fatalf("spaces is not an array of objects: %s: %v", obj["spaces"], err)
	}
	if len(entries) != len(wantReasons) {
		t.Fatalf("spaces carries %d entries, want %d", len(entries), len(wantReasons))
	}
	var gotReasons []string
	for i, e := range entries {
		var reason, spaceID string
		if err := json.Unmarshal(e["reason"], &reason); err != nil {
			t.Fatalf("entry %d reason: %v", i, err)
		}
		if err := json.Unmarshal(e["spaceId"], &spaceID); err != nil || spaceID != "k3x9p2qa" {
			t.Errorf("entry %d spaceId = %s, want \"k3x9p2qa\"", i, e["spaceId"])
		}
		gotReasons = append(gotReasons, reason)
		allowed := map[string]bool{"reason": true, "spaceId": true, "spaceName": true}
		for k := range e {
			if !allowed[k] {
				t.Errorf("entry %d carries %q — a space entry holds no other data", i, k)
			}
		}
	}
	if !equalStrings(gotReasons, wantReasons) {
		t.Errorf("reasons on the wire = %v, want the spec literals %v", gotReasons, wantReasons)
	}
	if _, ok := entries[0]["spaceName"]; !ok {
		t.Errorf("entry 0 lost its spaceName on the wire")
	}
	if _, ok := entries[1]["spaceName"]; ok {
		t.Errorf("entry 1 carries an empty spaceName — the field is absent when not given")
	}
}
