package tools

// assignee_not_string_test.go — sk-621: a non-string `assignee` on
// skein_update, skein_update_many and skein_claim is refused with
// validation_error, code assignee_not_string, and nothing is written.
// Reference — the sk-621 brief and the sk-605 model (assignee_mode.go): before the fix
// skein_update / skein_update_many read a number, array, object or bool as
// "" and cleared the assignee, skein_claim read it as "not passed" and took the issue
// for the caller. The meaning of null and the empty string stays as it was for each tool:
// for update/update_many it is "clear the assignee" (parameter description:
// "Pass an empty string to clear"; before the fix null gave the same empty string),
// for claim it is "not passed", the issue is taken for the caller.

import (
	"context"
	"testing"

	"skein-api/internal/issues"
)

// nonStringAssignees is the type matrix of sk-621: every JSON type a client
// can send that is neither a string nor null.
var nonStringAssignees = []struct {
	name  string
	value any
}{
	{"number", float64(42)},
	{"array", []any{"bob"}},
	{"object", map[string]any{"username": "bob"}},
	{"bool", true},
}

// issueState is what a refused call must leave as it was.
type issueState struct {
	assignee, lockedBy *string
	status             string
	version            int
}

func (m *mcpDA) issueState(id string) issueState {
	m.t.Helper()
	var s issueState
	if err := m.pool.QueryRow(context.Background(),
		`SELECT assignee, locked_by, status, version FROM issues WHERE id = $1`, id).
		Scan(&s.assignee, &s.lockedBy, &s.status, &s.version); err != nil {
		m.t.Fatalf("read %s: %v", id, err)
	}
	return s
}

func (m *mcpDA) wantStateUnchanged(t *testing.T, what, id string, before issueState) {
	t.Helper()
	after := m.issueState(id)
	if deref(after.assignee) != deref(before.assignee) || deref(after.lockedBy) != deref(before.lockedBy) ||
		after.status != before.status || after.version != before.version {
		t.Errorf("%s: %s changed: assignee %s→%s, lockedBy %s→%s, status %s→%s, version %d→%d", what, id,
			deref(before.assignee), deref(after.assignee), deref(before.lockedBy), deref(after.lockedBy),
			before.status, after.status, before.version, after.version)
	}
}

func TestUpdate_NonStringAssigneeRefused(t *testing.T) {
	m := newMCPDA(t)
	update := handleUpdate(m.pool, issues.NewStore(m.pool))
	for _, tc := range nonStringAssignees {
		id := m.bobTask()
		before := m.issueState(id)
		m.wantRefused(t, "skein_update assignee="+tc.name, update,
			map[string]any{"id": id, "expectedVersion": float64(before.version), "assignee": tc.value},
			"assignee_not_string", nil)
		m.wantStateUnchanged(t, "skein_update assignee="+tc.name, id, before)
		m.wantAssignee(t, id, m.bobName)
	}
}

// TestUpdate_NullAndEmptyAssigneeStillClear — the contract of null and ""
// on skein_update is unchanged: both drop the assignee.
func TestUpdate_NullAndEmptyAssigneeStillClear(t *testing.T) {
	m := newMCPDA(t)
	update := handleUpdate(m.pool, issues.NewStore(m.pool))
	for _, v := range []any{nil, ""} {
		id := m.bobTask()
		before := m.issueState(id)
		body, isErr := m.call(m.ctx, update,
			map[string]any{"id": id, "expectedVersion": float64(before.version), "assignee": v})
		if isErr {
			t.Fatalf("skein_update assignee=%#v: %v", v, body)
		}
		if after := m.issueState(id); after.assignee != nil && *after.assignee != "" {
			t.Errorf("skein_update assignee=%#v: assignee = %q, want cleared", v, *after.assignee)
		}
	}
}

func TestUpdateMany_NonStringAssigneeRefusedPerRow(t *testing.T) {
	m := newMCPDA(t)
	updateMany := handleUpdateMany(m.pool, issues.NewStore(m.pool))

	ids := make([]string, 0, len(nonStringAssignees)+2)
	befores := make([]issueState, 0, cap(ids))
	items := make([]any, 0, cap(ids))
	for _, tc := range nonStringAssignees {
		id := m.bobTask()
		st := m.issueState(id)
		ids, befores = append(ids, id), append(befores, st)
		items = append(items, map[string]any{"id": id, "expectedVersion": float64(st.version), "assignee": tc.value})
	}
	// null and "" rows in the same batch keep clearing.
	for _, v := range []any{nil, ""} {
		id := m.bobTask()
		st := m.issueState(id)
		ids, befores = append(ids, id), append(befores, st)
		items = append(items, map[string]any{"id": id, "expectedVersion": float64(st.version), "assignee": v})
	}

	body, isErr := m.call(m.ctx, updateMany, map[string]any{"items": items})
	if isErr {
		t.Fatalf("skein_update_many: %v", body)
	}
	results, _ := body["results"].([]any)
	if len(results) != len(items) {
		t.Fatalf("results = %d rows, want %d: %v", len(results), len(items), body)
	}
	for i, raw := range results {
		row, _ := raw.(map[string]any)
		if i < len(nonStringAssignees) {
			what := "skein_update_many assignee=" + nonStringAssignees[i].name
			errObj, _ := row["error"].(map[string]any)
			details, _ := errObj["details"].(map[string]any)
			if row["ok"] != false || errObj["code"] != ErrCodeValidationError || details["code"] != "assignee_not_string" {
				t.Errorf("%s: row %v, want validation_error with code %q", what, row, "assignee_not_string")
			}
			m.wantStateUnchanged(t, what, ids[i], befores[i])
			continue
		}
		if row["ok"] != true {
			t.Errorf("row %d (null/empty assignee): %v, want ok", i, row)
			continue
		}
		if after := m.issueState(ids[i]); after.assignee != nil && *after.assignee != "" {
			t.Errorf("row %d (null/empty assignee): assignee = %q, want cleared", i, *after.assignee)
		}
	}
}

func TestClaim_NonStringAssigneeRefused(t *testing.T) {
	m := newMCPDA(t)
	m.ctx = withCtxSession(t, m.owner, "user", "sess-sk621")
	claim := handleClaim(m.pool, issues.NewStore(m.pool))
	for _, tc := range nonStringAssignees {
		id := m.prefix + "-claim-" + tc.name
		seedIssue(t, m.pool, m.pid, m.owner, id, "free "+tc.name, "open", 2)
		before := m.issueState(id)
		m.wantRefused(t, "skein_claim assignee="+tc.name, claim,
			map[string]any{"id": id, "assignee": tc.value}, "assignee_not_string", nil)
		m.wantStateUnchanged(t, "skein_claim assignee="+tc.name, id, before)

		// The batch form parses the same argument.
		m.wantRefused(t, "skein_claim ids assignee="+tc.name, claim,
			map[string]any{"ids": []any{id}, "assignee": tc.value}, "assignee_not_string", nil)
		m.wantStateUnchanged(t, "skein_claim ids assignee="+tc.name, id, before)
	}
}

// TestClaim_NullAndEmptyAssigneeMeanCaller — null and "" on skein_claim
// still mean «not passed»: the issue is claimed onto the caller.
func TestClaim_NullAndEmptyAssigneeMeanCaller(t *testing.T) {
	m := newMCPDA(t)
	m.ctx = withCtxSession(t, m.owner, "user", "sess-sk621")
	claim := handleClaim(m.pool, issues.NewStore(m.pool))
	for i, v := range []any{nil, "", "   "} {
		id := m.prefix + "-claimok-" + string(rune('a'+i))
		seedIssue(t, m.pool, m.pid, m.owner, id, "free", "open", 2)
		body, isErr := m.call(m.ctx, claim, map[string]any{"id": id, "assignee": v})
		if isErr {
			t.Fatalf("skein_claim assignee=%#v: %v", v, body)
		}
		after := m.issueState(id)
		if after.status != "in_progress" || deref(after.lockedBy) != "sess-sk621" {
			t.Errorf("skein_claim assignee=%#v: status %s lockedBy %s, want in_progress by sess-sk621",
				v, after.status, deref(after.lockedBy))
		}
		m.wantAssignee(t, id, m.ownerName)
	}
}
