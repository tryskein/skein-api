package notifications

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// types_test — pure-unit serialization guards for NotificationDTO (sk-164
// SC-002a, T084). No DB. The integration half (SQL row → DTO hydration) lives
// in poll_for_monitor_test.go::TestPollForMonitor_ActorSessionRowToDTO.

// TestNotificationDTO_ActorSessionIDOmitempty pins the load-bearing omitempty
// contract on the shared DTO:
//   - monitor path with a non-empty session → `actorSessionId` PRESENT;
//   - monitor path with an empty/nil session → `actorSessionId` ABSENT;
//   - human inbox DTO (field never populated) → `actorSessionId` ABSENT.
//
// The human case is the whole reason for omitempty (spec Changelog 2026-07-20 /
// INV-5 / SC-004): without it the shared DTO would leak an inert
// `actorSessionId:null` onto GET /api/v1/notifications and change the human
// inbox JSON.
func TestNotificationDTO_ActorSessionIDOmitempty(t *testing.T) {
	base := NotificationDTO{
		ID:        1,
		Type:      "comment.created",
		CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
		Actor:     ActorDTO{Username: "tester", Type: "human"},
		Entity:    EntityDTO{Type: "issue", ID: "sk-1"},
		Summary:   "x",
	}

	t.Run("monitor non-empty session → present", func(t *testing.T) {
		n := base
		sess := "mcp-session-abc"
		n.ActorSessionID = &sess
		got := marshalMap(t, n)
		v, ok := got["actorSessionId"]
		if !ok {
			t.Fatalf("actorSessionId must be present when the session is non-empty; got %v", got)
		}
		if v != "mcp-session-abc" {
			t.Errorf("actorSessionId = %v, want mcp-session-abc", v)
		}
	})

	t.Run("monitor nil session → absent", func(t *testing.T) {
		n := base // ActorSessionID nil
		got := marshalMap(t, n)
		if _, ok := got["actorSessionId"]; ok {
			t.Errorf("actorSessionId must be ABSENT when the session is nil (omitempty), got present: %v", got["actorSessionId"])
		}
	})

	t.Run("human inbox DTO → no actorSessionId field", func(t *testing.T) {
		// The human path never populates ActorSessionID; the marshalled JSON
		// must be byte-for-byte free of the field so the human inbox is
		// unchanged (INV-5 / SC-004).
		human := base
		human.ReadAt = ptrTime(time.Unix(1_700_000_100, 0).UTC())
		raw := marshalString(t, human)
		if strings.Contains(raw, "actorSessionId") {
			t.Errorf("human inbox DTO JSON must not contain actorSessionId, got: %s", raw)
		}
		// Sanity: a human inbox row DOES carry readAt (proving we did not just
		// serialize an empty object).
		if !strings.Contains(raw, "readAt") {
			t.Errorf("expected readAt in human inbox JSON, got: %s", raw)
		}
	})
}

// TestNotificationDTO_ActorSessionID_EmptyStringPointer guards the reader's
// contract at the DTO layer: even a non-nil pointer to the empty string, were
// it ever set, would still serialize (Go's omitempty only elides nil pointers,
// not a pointer to ""). The store therefore must leave the field nil for an
// empty/NULL session (asserted in the integration test); this unit test
// documents WHY the store's nil/"" guard matters — a "" here would surface a
// spurious empty session on the wire.
func TestNotificationDTO_ActorSessionID_EmptyStringPointer(t *testing.T) {
	n := NotificationDTO{ID: 1, Type: "comment.created", Actor: ActorDTO{Type: "human"}}
	empty := ""
	n.ActorSessionID = &empty
	got := marshalMap(t, n)
	v, ok := got["actorSessionId"]
	if !ok {
		t.Skip("Go omitempty elides a *string to \"\" — contract already safe at marshal layer")
	}
	if v != "" {
		t.Errorf("a *\"\" pointer serializes to empty string, got %v", v)
	}
	// This is exactly the surface the store's `actorSessionID != nil && *!=\"\"`
	// guard prevents from ever being set — see PollForMonitor.
}

// TestNotificationDTO_MonitorCommentFieldsOmitempty pins sk-242.1 US-3 /
// INV-5 / T012 wire contract for the three monitor-only comment fields on
// the shared NotificationDTO:
//   - all nil → keys ABSENT (human inbox / non-monitor paths must stay
//     byte-stable — no commentId/bodyPreview/truncated, not even null);
//   - soft-delete shape (commentId set, body/truncated nil) → only commentId;
//   - empty live body → bodyPreview present as "" (string omitempty would drop
//     it — *string is load-bearing);
//   - long body truncated flag → truncated:true present only when non-nil true.
func TestNotificationDTO_MonitorCommentFieldsOmitempty(t *testing.T) {
	base := NotificationDTO{
		ID:        1,
		Type:      "comment.created",
		CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
		Actor:     ActorDTO{Username: "tester", Type: "human"},
		Entity:    EntityDTO{Type: "issue", ID: "sk-1"},
		Summary:   "x",
	}

	t.Run("all nil → no new keys (human / non-monitor path)", func(t *testing.T) {
		// Mirrors ListInbox / ListForIssue / watching recent / MCP inbox:
		// fields never populated → JSON must not mention them (INV-5).
		raw := marshalString(t, base)
		for _, key := range []string{"commentId", "bodyPreview", "truncated"} {
			if strings.Contains(raw, key) {
				t.Errorf("non-monitor DTO must not contain %q, got: %s", key, raw)
			}
		}
		got := marshalMap(t, base)
		for _, key := range []string{"commentId", "bodyPreview", "truncated"} {
			if _, ok := got[key]; ok {
				t.Errorf("%s must be ABSENT when nil, got present: %v", key, got[key])
			}
		}
	})

	t.Run("soft-delete shape → commentId only", func(t *testing.T) {
		n := base
		cid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
		n.CommentID = &cid
		// BodyPreview / Truncated stay nil — soft-delete / orphan (INV-4).
		got := marshalMap(t, n)
		if v, ok := got["commentId"]; !ok || v != cid {
			t.Errorf("commentId = %v (ok=%v), want %q", v, ok, cid)
		}
		if _, ok := got["bodyPreview"]; ok {
			t.Errorf("bodyPreview must be ABSENT on soft-delete, got %v", got["bodyPreview"])
		}
		if _, ok := got["truncated"]; ok {
			t.Errorf("truncated must be ABSENT on soft-delete, got %v", got["truncated"])
		}
	})

	t.Run("empty bodyPreview present as empty string", func(t *testing.T) {
		n := base
		cid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
		empty := ""
		n.CommentID = &cid
		n.BodyPreview = &empty
		got := marshalMap(t, n)
		v, ok := got["bodyPreview"]
		if !ok {
			t.Fatal("bodyPreview must be PRESENT when non-nil *\"\" (*string load-bearing)")
		}
		if s, _ := v.(string); s != "" {
			t.Errorf("bodyPreview = %v, want empty string", v)
		}
		if _, ok := got["truncated"]; ok {
			t.Errorf("truncated must be ABSENT when body ≤ cap, got %v", got["truncated"])
		}
	})

	t.Run("live short body → preview present, truncated absent", func(t *testing.T) {
		n := base
		cid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
		preview := "hello"
		n.CommentID = &cid
		n.BodyPreview = &preview
		got := marshalMap(t, n)
		if v, ok := got["bodyPreview"]; !ok || v != "hello" {
			t.Errorf("bodyPreview = %v (ok=%v), want hello", v, ok)
		}
		if _, ok := got["truncated"]; ok {
			t.Errorf("truncated must be ABSENT when not truncated, got %v", got["truncated"])
		}
	})

	t.Run("truncated true present only when set", func(t *testing.T) {
		n := base
		cid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
		preview := "cut…"
		tru := true
		n.CommentID = &cid
		n.BodyPreview = &preview
		n.Truncated = &tru
		got := marshalMap(t, n)
		v, ok := got["truncated"]
		if !ok {
			t.Fatal("truncated must be PRESENT when non-nil true")
		}
		if b, _ := v.(bool); !b {
			t.Errorf("truncated = %v, want true", v)
		}
	})

	t.Run("truncated false pointer would surface — store must leave nil", func(t *testing.T) {
		// Documents why Truncated is only set to &true, never &false: a
		// non-nil false would appear on the wire (omitempty does not elide
		// false bools inside pointers). Soft-delete / short body leave nil.
		n := base
		f := false
		n.Truncated = &f
		got := marshalMap(t, n)
		if _, ok := got["truncated"]; !ok {
			t.Skip("Go omitempty elided *false — already safe at marshal")
		}
		// If present, prove the surface so PollForMonitor keeps nil for false.
		if got["truncated"] != false {
			t.Errorf("truncated = %v, want false", got["truncated"])
		}
	})
}

func marshalMap(t *testing.T, n NotificationDTO) map[string]any {
	t.Helper()
	b, err := json.Marshal(n)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func marshalString(t *testing.T, n NotificationDTO) string {
	t.Helper()
	b, err := json.Marshal(n)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func ptrTime(v time.Time) *time.Time { return &v }

// TestWatchingWireLiterals — sk-622.21, test matrix row 4 (US-6, US-7, the
// REST contract of Watching). The reference is the pair of literals fixed in
// the godoc of types.go by T008 — `{"marked":2}` for "read card" and
// `{"unwatched":3,"keptManual":1,"historicalRemoved":2}` for bulk unwatch —
// the same literals the frontend types are built from (T047). Each value is
// encoded and compared with its literal as a JSON value, so a renamed or
// dropped key turns this red; zero counts must stay on the wire, because a
// client reading an absent key cannot tell "none removed" from "old server".
func TestWatchingWireLiterals(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want string
	}{
		{"read card", MarkCardReadResult{Marked: 2}, `{"marked":2}`},
		{"read card, nothing unread", MarkCardReadResult{}, `{"marked":0}`},
		{"bulk unwatch", BulkUnwatchResult{Unwatched: 3, KeptManual: 1, HistoricalRemoved: 2},
			`{"unwatched":3,"keptManual":1,"historicalRemoved":2}`},
		{"bulk unwatch, no historical card", BulkUnwatchResult{Unwatched: 1},
			`{"unwatched":1,"keptManual":0,"historicalRemoved":0}`},
	}
	for _, c := range cases {
		got, err := json.Marshal(c.v)
		if err != nil {
			t.Fatalf("%s: marshal: %v", c.name, err)
		}
		var gotV, wantV map[string]any
		if err := json.Unmarshal(got, &gotV); err != nil {
			t.Fatalf("%s: unmarshal %s: %v", c.name, got, err)
		}
		if err := json.Unmarshal([]byte(c.want), &wantV); err != nil {
			t.Fatalf("%s: bad literal %s: %v", c.name, c.want, err)
		}
		if len(gotV) != len(wantV) {
			t.Errorf("%s: wire = %s, want %s", c.name, got, c.want)
			continue
		}
		for k, wv := range wantV {
			if gv, ok := gotV[k]; !ok || gv != wv {
				t.Errorf("%s: wire = %s, want %s", c.name, got, c.want)
				break
			}
		}
	}
}
