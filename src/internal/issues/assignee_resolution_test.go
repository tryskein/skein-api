package issues

// assignee_resolution_test.go — sk-712 (sk-674.18): the handle of
// issues.assignee resolves through package assignee's helpers by an index
// lookup. Reference — the resolution contract as package assignee and
// skein_create word it ("username, email, or user UUID — stored verbatim,
// resolved on read", case-insensitive), written before the lookup: a handle
// in any of the three forms, in any letter case, names its user; anything
// else — free text, an empty string, an id in a non-canonical spelling
// (braces, no hyphens), a handle with stray spaces — names nobody.
// Delivery (LeftJoin, MatchesCaller) adds INV-30's access condition; Names is
// the identity alone.

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/assignee"
)

func TestAssigneeResolution_FormsThroughTheHelpers(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	owner := seedUser(t, pool)
	worker := seedUser(t, pool)
	outsider := seedUser(t, pool)
	pid, pref := seedProject(t, pool, owner)
	if _, err := pool.Exec(ctx, `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3)
RETURNING project_id, user_id)
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, worker, owner); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	col := func(id uuid.UUID, c string) string {
		var v string
		if err := pool.QueryRow(ctx, `SELECT `+c+` FROM users WHERE id = $1`, id).Scan(&v); err != nil {
			t.Fatalf("%s of %s: %v", c, id, err)
		}
		return v
	}
	wid := worker.String()
	cases := []struct {
		name   string
		handle *string
		names  *uuid.UUID // the user the handle names (Names)
		worker bool       // delivered to the worker (LeftJoin, MatchesCaller)
	}{
		{"username", ptrStr(col(worker, "username")), &worker, true},
		{"username upper-cased", ptrStr(strings.ToUpper(col(worker, "username"))), &worker, true},
		{"e-mail upper-cased", ptrStr(strings.ToUpper(col(worker, "email"))), &worker, true},
		{"id", ptrStr(wid), &worker, true},
		{"id upper-cased", ptrStr(strings.ToUpper(wid)), &worker, true},
		{"id of a user without access", ptrStr(outsider.String()), &outsider, false},
		{"username of a user without access", ptrStr(col(outsider, "username")), &outsider, false},
		{"id in braces", ptrStr("{" + wid + "}"), nil, false},
		{"id without hyphens", ptrStr(strings.ReplaceAll(wid, "-", "")), nil, false},
		{"username with a leading space", ptrStr(" " + col(worker, "username")), nil, false},
		{"free text", ptrStr("the docs team"), nil, false},
		{"empty", ptrStr(""), nil, false},
		{"unassigned", nil, nil, false},
	}
	for n, c := range cases {
		id := pref + "-" + itoa(n+1)
		seedIssue(t, pool, pid, owner, id, "resolution "+c.name)
		if _, err := pool.Exec(ctx, `UPDATE issues SET assignee = $2 WHERE id = $1`, id, c.handle); err != nil {
			t.Fatalf("assign %s: %v", id, err)
		}
		t.Run(c.name, func(t *testing.T) {
			var joined, named *uuid.UUID
			var mine bool
			err := pool.QueryRow(ctx, `
SELECT ua.id,
       (SELECT un.id FROM users un WHERE `+assignee.Names("un", "i")+`),
       `+assignee.MatchesCaller("i", "$2")+`
  FROM issues i
  `+assignee.LeftJoin("ua", "i")+`
 WHERE i.id = $1`, id, worker).Scan(&joined, &named, &mine)
			if err != nil {
				t.Fatalf("resolve %s: %v", id, err)
			}
			if !eqID(named, c.names) {
				t.Errorf("Names: %v, want %v", named, c.names)
			}
			var wantJoined *uuid.UUID
			if c.worker {
				wantJoined = &worker
			}
			if !eqID(joined, wantJoined) {
				t.Errorf("LeftJoin: %v, want %v", joined, wantJoined)
			}
			if mine != c.worker {
				t.Errorf("MatchesCaller(worker) = %v, want %v", mine, c.worker)
			}
		})
	}
}

func eqID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
