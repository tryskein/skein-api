package main

// force_unlock_test.go — sk-674.18, the test delivery of rows 143–145
// (T034, US-16, SC-018). Reference — US-16 as the spec words it and the
// lead's decision (b) on row 143 (2026-10-14), written before the code:
//
//   - 143: bypassing and releasing another holder's claim is one right,
//     force_unlock — the project owner (vP), the space owner (vPr), the
//     instance admin — admA with a member row and adm0 without any (since
//     sk-741 the admin has the owner's rights, ActionWrite included, with
//     no membership). On EVERY path with
//     force they succeed; anybody else — a member (uAB), an external member
//     (vnsh), a participant who sees the project through its space (uc¬p) —
//     is refused force_unlock_forbidden and the claim, the status and the
//     history stay as they were. A path that RELEASES the claim (unlock,
//     unclaim, close, a transition to closed, bulk close, close_many) writes
//     issue.lock_force_released naming the releaser in the same call; a
//     path that only bypasses it (update, update_many, a transition to
//     another status) keeps the claim and writes no such event.
//   - 144 «Unchanged»: the holder releases his own claim as before — no
//     force_unlock needed, no forced-release event.
//   - 145: the card carries canForceUnlock — true for vP, vPr and the
//     admin, false for the holder and everyone else.
//
// Fixture: testsupport.SeedTenancy; A.members (members visibility, owner
// vP, space A of vPr) and A.space (visibility space). The holder is uA, a
// member of both. Every cell acts on its own freshly claimed issue.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/testsupport"
)

type forceCell struct {
	ok   bool
	code string
	raw  string
}

func TestForceUnlock_EveryBypassPathFollowsTheRight(t *testing.T) {
	s := newParityStack(t)
	ctx := context.Background()
	holder := s.f.User(t, testsupport.ActorOnlyA)
	const holderSession = "holder-session"

	lockedIssue := func(p testsupport.TenancyProject) string {
		var n int
		if err := s.pool.QueryRow(ctx, `UPDATE project_counters SET counter = counter + 1 WHERE project_id = $1 RETURNING counter`,
			p.ID).Scan(&n); err != nil {
			t.Fatalf("counter: %v", err)
		}
		id := testsupport.SeedIssue(t, s.pool, p.ID, p.OwnerID, fmt.Sprintf("%s-%d", p.IDPrefix, n), "claimed", "open", 2)
		if _, err := s.pool.Exec(ctx, `
UPDATE issues SET status = 'in_progress', started_at = now(), assignee = $2,
       locked_by = $3, locked_by_user_id = $4, locked_at = now()
 WHERE id = $1`, id, holder.Username, holderSession, holder.ID); err != nil {
			t.Fatalf("claim %s: %v", id, err)
		}
		return id
	}
	type snapshot struct {
		lockedBy *string
		status   string
		events   int
	}
	snap := func(id string) snapshot {
		var sn snapshot
		if err := s.pool.QueryRow(ctx, `
SELECT i.locked_by, i.status,
       (SELECT count(*) FROM events e WHERE e.entity_type = 'issue' AND e.entity_id = i.id)
  FROM issues i WHERE i.id = $1`, id).Scan(&sn.lockedBy, &sn.status, &sn.events); err != nil {
			t.Fatalf("snapshot %s: %v", id, err)
		}
		return sn
	}
	releasedBy := func(id string) []uuid.UUID {
		rows, err := s.pool.Query(ctx, `
SELECT (new_data->>'by')::uuid FROM events
 WHERE entity_type = 'issue' AND entity_id = $1 AND type = 'issue.lock_force_released'`, id)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []uuid.UUID
		for rows.Next() {
			var u uuid.UUID
			if err := rows.Scan(&u); err != nil {
				t.Fatal(err)
			}
			out = append(out, u)
		}
		return out
	}
	version := func(id string) int {
		var v int
		if err := s.pool.QueryRow(ctx, `SELECT version FROM issues WHERE id = $1`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	restCell := func(status int, raw []byte) forceCell {
		if status >= 200 && status < 300 {
			return forceCell{ok: true, raw: string(raw)}
		}
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		return forceCell{code: fmt.Sprint(body["error"]), raw: string(raw)}
	}
	toolCell := func(r toolResult) forceCell {
		var body map[string]any
		_ = json.Unmarshal([]byte(r.text), &body)
		if r.isError {
			return forceCell{code: fmt.Sprint(body["error"]), raw: r.text}
		}
		// The batch tools answer per row.
		if rows, ok := body["results"].([]any); ok && len(rows) == 1 {
			row, _ := rows[0].(map[string]any)
			if row["ok"] == true {
				return forceCell{ok: true, raw: r.text}
			}
			e, _ := row["error"].(map[string]any)
			return forceCell{code: fmt.Sprint(e["code"]), raw: r.text}
		}
		return forceCell{ok: true, raw: r.text}
	}

	sessions := map[testsupport.Actor]string{}
	sessionOf := func(a testsupport.Actor) string {
		if sid, ok := sessions[a]; ok {
			return sid
		}
		sessions[a] = s.session(a)
		return sessions[a]
	}
	type path struct {
		name     string
		releases bool
		call     func(a testsupport.Actor, p testsupport.TenancyProject, id string) forceCell
	}
	issuePath := func(p testsupport.TenancyProject, id string) string {
		return "/api/v1/projects/" + p.ID.String() + "/issues/" + id
	}
	paths := []path{
		{"REST unlock", true, func(a testsupport.Actor, p testsupport.TenancyProject, id string) forceCell {
			return restCell(s.rest(a, http.MethodPost, issuePath(p, id)+"/unlock", nil))
		}},
		{"REST transitions?force → closed", true, func(a testsupport.Actor, p testsupport.TenancyProject, id string) forceCell {
			return restCell(s.restWith(a, http.MethodPost, issuePath(p, id)+"/transitions?force=true",
				map[string]any{"status": "closed", "closeOutcome": "done", "reason": "forced"},
				map[string]string{"If-Match": fmt.Sprint(version(id))}))
		}},
		{"REST transitions?force → deferred", false, func(a testsupport.Actor, p testsupport.TenancyProject, id string) forceCell {
			return restCell(s.restWith(a, http.MethodPost, issuePath(p, id)+"/transitions?force=true",
				map[string]any{"status": "deferred"}, map[string]string{"If-Match": fmt.Sprint(version(id))}))
		}},
		{"REST bulk close force", true, func(a testsupport.Actor, p testsupport.TenancyProject, id string) forceCell {
			c := restCell(s.rest(a, http.MethodPost, "/api/v1/projects/"+p.ID.String()+"/issues/bulk",
				map[string]any{"ids": []string{id}, "action": "close", "force": true, "closeOutcome": "done"}))
			if c.ok {
				var body struct {
					Succeeded []string `json:"succeeded"`
				}
				_ = json.Unmarshal([]byte(c.raw), &body)
				if len(body.Succeeded) != 1 {
					return forceCell{code: "bulk row failed", raw: c.raw}
				}
			}
			return c
		}},
		{"MCP skein_unclaim force", true, func(a testsupport.Actor, _ testsupport.TenancyProject, id string) forceCell {
			return toolCell(s.tool(a, sessionOf(a), "skein_unclaim", map[string]any{
				"id": id, "force": true, "nextStatus": "open", "reason": "forced"}))
		}},
		{"MCP skein_update force", false, func(a testsupport.Actor, _ testsupport.TenancyProject, id string) forceCell {
			return toolCell(s.tool(a, sessionOf(a), "skein_update", map[string]any{
				"id": id, "expectedVersion": version(id), "force": true, "priority": 1}))
		}},
		{"MCP skein_close force", true, func(a testsupport.Actor, _ testsupport.TenancyProject, id string) forceCell {
			return toolCell(s.tool(a, sessionOf(a), "skein_close", map[string]any{
				"id": id, "expectedVersion": version(id), "force": true, "closeOutcome": "done", "reason": "forced"}))
		}},
		{"MCP skein_update_many force", false, func(a testsupport.Actor, _ testsupport.TenancyProject, id string) forceCell {
			return toolCell(s.tool(a, sessionOf(a), "skein_update_many", map[string]any{
				"force": true, "items": []any{map[string]any{"id": id, "expectedVersion": version(id), "priority": 1}}}))
		}},
		{"MCP skein_close_many force", true, func(a testsupport.Actor, _ testsupport.TenancyProject, id string) forceCell {
			return toolCell(s.tool(a, sessionOf(a), "skein_close_many", map[string]any{
				"force": true, "items": []any{map[string]any{
					"id": id, "expectedVersion": version(id), "closeOutcome": "done", "reason": "forced"}}}))
		}},
	}

	allowed := []testsupport.Actor{testsupport.ActorProjectOwner, testsupport.ActorSpaceOwner, testsupport.ActorAdminA, testsupport.ActorAdmin0}
	refused := []struct {
		actor   testsupport.Actor
		project testsupport.TenancyProject
	}{
		{testsupport.ActorBoth, s.f.AMembers},
		{testsupport.ActorExternal, s.f.AMembers},
		{testsupport.ActorParticipantNoMembers, s.f.ASpace},
	}
	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			for _, a := range allowed {
				id := lockedIssue(s.f.AMembers)
				got := p.call(a, s.f.AMembers, id)
				if !got.ok {
					t.Errorf("%s with force_unlock: refused %s %s", a, got.code, got.raw)
					continue
				}
				after := snap(id)
				by := releasedBy(id)
				if p.releases {
					if after.lockedBy != nil {
						t.Errorf("%s: the claim is still held after a releasing path", a)
					}
					if len(by) != 1 || by[0] != s.f.User(t, a).ID {
						t.Errorf("%s: issue.lock_force_released by %v, want exactly one by the releaser", a, by)
					}
				} else if len(by) != 0 {
					t.Errorf("%s: a bypass that keeps the claim wrote issue.lock_force_released %v", a, by)
				}
			}
			for _, r := range refused {
				id := lockedIssue(r.project)
				before := snap(id)
				got := p.call(r.actor, r.project, id)
				if got.ok || got.code != "force_unlock_forbidden" {
					t.Errorf("%s without force_unlock: %+v, want force_unlock_forbidden", r.actor, got)
				}
				if after := snap(id); deref(after.lockedBy) != deref(before.lockedBy) ||
					after.status != before.status || after.events != before.events {
					t.Errorf("%s: a refused force changed the issue: before %v/%s/%d, after %v/%s/%d", r.actor,
						deref(before.lockedBy), before.status, before.events, deref(after.lockedBy), after.status, after.events)
				}
			}
		})
	}

	t.Run("144: the holder releases his own claim without force_unlock", func(t *testing.T) {
		// REST unlock of his own claim.
		id := lockedIssue(s.f.AMembers)
		if got := restCell(s.rest(testsupport.ActorOnlyA, http.MethodPost, issuePath(s.f.AMembers, id)+"/unlock", nil)); !got.ok {
			t.Fatalf("the holder's unlock: %+v", got)
		}
		if after := snap(id); after.lockedBy != nil {
			t.Errorf("the holder's unlock left the claim")
		}
		if by := releasedBy(id); len(by) != 0 {
			t.Errorf("the holder's own release wrote issue.lock_force_released %v", by)
		}
		// MCP: a real claim through his session, then a plain unclaim.
		var n int
		if err := s.pool.QueryRow(ctx, `UPDATE project_counters SET counter = counter + 1 WHERE project_id = $1 RETURNING counter`,
			s.f.AMembers.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		open := testsupport.SeedIssue(t, s.pool, s.f.AMembers.ID, s.f.AMembers.OwnerID,
			fmt.Sprintf("%s-%d", s.f.AMembers.IDPrefix, n), "to claim", "open", 2)
		sid := sessionOf(testsupport.ActorOnlyA)
		if c := toolCell(s.tool(testsupport.ActorOnlyA, sid, "skein_claim", map[string]any{"id": open})); !c.ok {
			t.Fatalf("claim: %+v", c)
		}
		if c := toolCell(s.tool(testsupport.ActorOnlyA, sid, "skein_unclaim", map[string]any{
			"id": open, "nextStatus": "open", "reason": "done with it"})); !c.ok {
			t.Fatalf("the holder's unclaim: %+v", c)
		}
		if after := snap(open); after.lockedBy != nil {
			t.Errorf("the holder's unclaim left the claim")
		}
		if by := releasedBy(open); len(by) != 0 {
			t.Errorf("the holder's unclaim wrote issue.lock_force_released %v", by)
		}
	})

	t.Run("145: canForceUnlock on the card", func(t *testing.T) {
		id := lockedIssue(s.f.AMembers)
		for a, want := range map[testsupport.Actor]bool{
			testsupport.ActorProjectOwner: true, testsupport.ActorSpaceOwner: true,
			testsupport.ActorAdminA: true, testsupport.ActorAdmin0: true,
			testsupport.ActorOnlyA: false, testsupport.ActorBoth: false, testsupport.ActorExternal: false,
		} {
			status, raw := s.rest(a, http.MethodGet, issuePath(s.f.AMembers, id)+"/", nil)
			if status != http.StatusOK {
				t.Errorf("%s card: %d %s", a, status, raw)
				continue
			}
			var card struct {
				CanForceUnlock *bool `json:"canForceUnlock"`
			}
			_ = json.Unmarshal(raw, &card)
			if card.CanForceUnlock == nil || *card.CanForceUnlock != want {
				t.Errorf("%s: canForceUnlock %v, want %v", a, card.CanForceUnlock, want)
			}
		}
	})

	// 145 for a holder who has force_unlock himself: the flag answers «may release ANOTHER holder's claim», so on his
	// own claim the project owner and the admin get false; another
	// right-holder reading the same card gets true (positive control). After
	// the holder releases his claim, the unlock answer carries the right
	// alone — no claim is left to be another's.
	t.Run("145: the holder with force_unlock gets false", func(t *testing.T) {
		for _, c := range []struct{ holder, other testsupport.Actor }{
			{testsupport.ActorProjectOwner, testsupport.ActorSpaceOwner},
			{testsupport.ActorAdminA, testsupport.ActorProjectOwner},
		} {
			t.Run(string(c.holder), func(t *testing.T) {
				h := s.f.User(t, c.holder)
				id := lockedIssue(s.f.AMembers)
				if _, err := s.pool.Exec(ctx, `UPDATE issues SET assignee = $2, locked_by = $3, locked_by_user_id = $4 WHERE id = $1`,
					id, h.Username, "own-"+string(c.holder), h.ID); err != nil {
					t.Fatal(err)
				}
				show := func(b *bool) string {
					if b == nil {
						return "absent"
					}
					return fmt.Sprint(*b)
				}
				flag := func(a testsupport.Actor, method, suffix string) *bool {
					status, raw := s.rest(a, method, issuePath(s.f.AMembers, id)+suffix, nil)
					if status != http.StatusOK {
						t.Fatalf("%s %s%s: %d %s", a, method, suffix, status, raw)
					}
					var card struct {
						CanForceUnlock *bool `json:"canForceUnlock"`
					}
					_ = json.Unmarshal(raw, &card)
					return card.CanForceUnlock
				}
				if got := flag(c.holder, http.MethodGet, "/"); got == nil || *got {
					t.Errorf("the holder %s reads canForceUnlock %s on his own claim, want false", c.holder, show(got))
				}
				if got := flag(c.other, http.MethodGet, "/"); got == nil || !*got {
					t.Errorf("%s reads canForceUnlock %s on %s's claim, want true (positive control)", c.other, show(got), c.holder)
				}
				if got := flag(c.holder, http.MethodPost, "/unlock"); got == nil || !*got {
					t.Errorf("after releasing his own claim %s gets canForceUnlock %s, want his right (true)", c.holder, show(got))
				}
			})
		}
	})
}

func deref(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}
