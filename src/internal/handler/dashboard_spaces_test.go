package handler

// dashboard_spaces_test.go — sk-674.18, the test delivery of rows 132–136
// (T030: HOT ZONE by actors, US-13, INV-28, INV-23, K-6). The references are
// the acceptance of US-13 and the invariants as the spec words them, on the
// fixture legend of testsupport.SeedTenancy, written before the code:
//
//   - an actor's reach is the projects he sees plus the projects of his own
//     non-personal spaces he does not see; the second kind enters every
//     aggregate surface by its contribution and every row surface only
//     anonymized (no issue text, no project name); a project of another
//     space or another person's "Personal" enters nothing (INV-23); an
//     external user and a deactivated one contribute nothing; an admin's
//     reach is his column — every non-personal project plus his own
//     "Personal";
//   - `?user=` never widens the reach, `space=` narrows it to one space on
//     every surface;
//   - the agents are counted by the projects of the reach (K-6);
//   - the all-time cache follows the reach (K-6).
//
// Every fixture project carries exactly one issue.created event by its
// owner, so «the contribution of a project» is the number 1 on each
// aggregate. Row 29 of the matrix (T004) holds the no-token sweep of the
// same surfaces; these rows hold the numbers, the anonymized rows and the
// cache.

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
)

// hotParticipants is the legend's space participation (SeedTenancy).
var hotParticipants = map[string][]testsupport.Actor{
	"A": {testsupport.ActorSpaceOwner, testsupport.ActorOnlyA, testsupport.ActorBoth, testsupport.ActorProjectOwner,
		testsupport.ActorParticipantNoMembers, testsupport.ActorAdminA, testsupport.ActorDeactivated},
	"B": {testsupport.ActorOnlyB, testsupport.ActorBoth},
}

var hotSpaceProjects = map[string][]string{
	"A": {keyASpace, keyAMembers},
	"B": {keyBSpace, keyBMembers},
}

// hotReach is the reference reach of actor a, optionally narrowed to one
// space ("" — every space): the projects he sees, plus — for an active
// internal participant — the projects of his own non-personal spaces.
func hotReach(w *isoWorld, a testsupport.Actor, space string) (visible, contribution []string) {
	seen := map[string]bool{}
	for _, p := range w.ordered {
		if isoCell(a, p.Key) == cellVisible {
			seen[p.Key] = true
		}
	}
	if a != testsupport.ActorDeactivated && a != testsupport.ActorExternal {
		for sp, members := range hotParticipants {
			if !slices.Contains(members, a) {
				continue
			}
			for _, key := range hotSpaceProjects[sp] {
				if !seen[key] {
					contribution = append(contribution, key)
				}
			}
		}
	}
	for key := range seen {
		visible = append(visible, key)
	}
	if space == "" {
		return visible, contribution
	}
	in := func(keys []string) []string {
		var out []string
		for _, k := range keys {
			if slices.Contains(hotSpaceProjects[space], k) {
				out = append(out, k)
			}
		}
		return out
	}
	return in(visible), in(contribution)
}

// seedCreated writes one issue.created event of p's issue by p's owner;
// actorName is the events.actor text the `user` filter reads.
func (w *isoWorld) seedCreated(t *testing.T, p *isoProject) {
	t.Helper()
	ctx := context.Background()
	var name string
	if err := w.pool.QueryRow(ctx, `SELECT username FROM users WHERE id = $1`, p.OwnerID).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if _, err := w.pool.Exec(ctx, `
INSERT INTO events (occurred_at, type, actor, actor_user_id, actor_type, entity_type, entity_id, project_id, payload)
VALUES (now(), 'issue.created', $1, $2, 'human', 'issue', $3, $4, '{}'::jsonb)`, name, p.OwnerID, p.IssueID, p.ID); err != nil {
		t.Fatalf("seed issue.created on %s: %v", p.Key, err)
	}
}

// hotNumbers reads the aggregate surfaces of one actor: issues_created on
// the scoring, the race total, the hot-projects rows (all / anonymized) and
// the `all` row's opened issues.
type hotNumbers struct {
	Scoring, Race, Rows, Redacted, AllOpened int64
}

func (w *isoWorld) hotNumbers(t *testing.T, a testsupport.Actor, query string) hotNumbers {
	t.Helper()
	get := func(path string, v any) {
		t.Helper()
		r := w.get(a, path)
		if r.status != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, r.status, r.body)
		}
		decodeInto(t, r, v)
	}
	var n hotNumbers
	var scoring struct {
		Metrics []struct {
			Key   string `json:"key"`
			Human int64  `json:"human"`
		} `json:"metrics"`
	}
	get("/api/v1/dashboard/scoring?window=all-time"+query, &scoring)
	for _, m := range scoring.Metrics {
		if m.Key == "issues_created" {
			n.Scoring = m.Human
		}
	}
	var race struct {
		Users []struct {
			Total int64 `json:"total"`
		} `json:"users"`
		Others struct {
			Total int64 `json:"total"`
		} `json:"others"`
	}
	get("/api/v1/dashboard/race?metric=created&window=all"+query, &race)
	for _, u := range race.Users {
		n.Race += u.Total
	}
	n.Race += race.Others.Total
	var projects struct {
		All struct {
			IssuesOpened struct {
				Count int64 `json:"count"`
			} `json:"issuesOpened"`
		} `json:"all"`
		Rows []struct {
			Redacted bool `json:"redacted"`
		} `json:"rows"`
	}
	get("/api/v1/dashboard/projects?window=all"+query, &projects)
	n.Rows = int64(len(projects.Rows))
	for _, r := range projects.Rows {
		if r.Redacted {
			n.Redacted++
		}
	}
	n.AllOpened = projects.All.IssuesOpened.Count
	return n
}

// wantNumbers is the reference for a reach: one issue.created event per
// project on the event aggregates; the `all` row's opened issues count the
// issues of the reach's projects (the fixture seeds more than one in A).
func (w *isoWorld) wantNumbers(t *testing.T, visible, contribution []string) hotNumbers {
	t.Helper()
	total := int64(len(visible) + len(contribution))
	var ids []uuid.UUID
	for _, k := range append(append([]string{}, visible...), contribution...) {
		ids = append(ids, w.byKey[k].ID)
	}
	var opened int64
	if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM issues WHERE project_id = ANY($1)`, ids).Scan(&opened); err != nil {
		t.Fatal(err)
	}
	return hotNumbers{Scoring: total, Race: total, Rows: total, Redacted: int64(len(contribution)), AllOpened: opened}
}

func (w *isoWorld) spaceShort(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := w.pool.QueryRow(context.Background(), `SELECT short_id FROM spaces WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestHotZoneSpaces_AggregatesCountTheReach — row 132.
func TestHotZoneSpaces_AggregatesCountTheReach(t *testing.T) {
	w := newIsoWorld(t)
	for _, p := range w.ordered {
		w.seedCreated(t, p)
	}
	w.eachActor(t, func(t *testing.T, a testsupport.Actor) {
		visible, contribution := hotReach(w, a, "")
		if a == testsupport.ActorAdminA || a == testsupport.ActorAdmin0 {
			// An admin's reach is the installation's every non-personal
			// project — in a shared test database that includes other
			// tests' projects, so totals are not this world's to assert.
			// Per project of the world instead: each one in the reach
			// counts its one event, every other "Personal" counts nothing.
			w.adminPerProject(t, a, append(visible, contribution...))
			return
		}
		if got, want := w.hotNumbers(t, a, ""), w.wantNumbers(t, visible, contribution); got != want {
			t.Errorf("numbers %+v, want %+v (visible %v, contribution %v)", got, want, visible, contribution)
		}
	})
}

// adminPerProject checks an admin's reach project by project of the world:
// scoring with `project=` counts the project's one event when it is in the
// reach and nothing otherwise; the hot-projects rows carry exactly the
// world projects of the reach.
func (w *isoWorld) adminPerProject(t *testing.T, a testsupport.Actor, reach []string) {
	t.Helper()
	r := w.get(a, "/api/v1/dashboard/projects?window=all")
	if r.status != http.StatusOK {
		t.Fatalf("projects: %d %s", r.status, r.body)
	}
	for _, p := range w.ordered {
		in := slices.Contains(reach, p.Key)
		if listed := bytes.Contains(r.body, []byte(p.ID.String())); listed != in {
			t.Errorf("hot projects list %s = %v, want %v", p.Key, listed, in)
		}
		sr := w.get(a, "/api/v1/dashboard/scoring?window=all-time&project="+p.ID.String())
		var created int64
		if sr.status == http.StatusOK {
			var sc struct {
				Metrics []struct {
					Key   string `json:"key"`
					Human int64  `json:"human"`
				} `json:"metrics"`
			}
			decodeInto(t, sr, &sc)
			for _, m := range sc.Metrics {
				if m.Key == "issues_created" {
					created = m.Human
				}
			}
		}
		if want := map[bool]int64{true: 1, false: 0}[in]; created != want {
			t.Errorf("scoring of %s = %d, want %d", p.Key, created, want)
		}
	}
}

// TestHotZoneSpaces_RowsOfContributionAnonymized — row 133.
func TestHotZoneSpaces_RowsOfContributionAnonymized(t *testing.T) {
	w := newIsoWorld(t)
	ctx := context.Background()
	p := w.byKey[keyAMembers]
	marker := "hotmarker" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := w.pool.Exec(ctx, `UPDATE issues SET status = 'in_progress', started_at = now(), title = title || ' ' || $2 WHERE id = $1`,
		p.IssueID, marker); err != nil {
		t.Fatal(err)
	}
	w.seedCreated(t, p)
	w.seedIssueEvent(t, p)
	surfaces := []string{
		"/api/v1/dashboard/feed?limit=100",
		"/api/v1/dashboard/in-progress?limit=200",
		"/api/v1/dashboard/projects?window=all",
	}
	texts := []string{marker, p.Name, p.Alias}

	// uc¬p: A.members is a project of his own space he does not see — an
	// anonymized row on each surface, none of its text.
	contributor := testsupport.ActorParticipantNoMembers
	for _, path := range surfaces {
		r := w.get(contributor, path)
		if r.status != http.StatusOK {
			t.Fatalf("%s: %d %s", path, r.status, r.body)
		}
		for _, txt := range texts {
			if bytes.Contains(r.body, []byte(txt)) {
				t.Errorf("%s gives uc¬p the text %q of the project he does not see", path, txt)
			}
		}
		if !bytes.Contains(r.body, []byte(`"redacted":true`)) {
			t.Errorf("%s has no anonymized row for uc¬p (the contribution must stay as a row): %s", path, r.body)
		}
	}
	// uB: A.members is of another space — no row at all, anonymized or
	// not: his surfaces are what they are without A.members.
	outsider := testsupport.ActorOnlyB
	for _, path := range surfaces {
		r := w.get(outsider, path)
		if r.status != http.StatusOK {
			t.Fatalf("%s: %d %s", path, r.status, r.body)
		}
		if bytes.Contains(r.body, []byte(`"redacted":true`)) {
			t.Errorf("%s gives uB an anonymized row of another space's project: %s", path, r.body)
		}
		for _, txt := range texts {
			if bytes.Contains(r.body, []byte(txt)) {
				t.Errorf("%s gives uB %q", path, txt)
			}
		}
	}
	// Positive control: a member sees the full row with the text.
	r := w.get(testsupport.ActorOnlyA, "/api/v1/dashboard/in-progress?limit=200")
	if !bytes.Contains(r.body, []byte(marker)) {
		t.Errorf("a member of A.members does not see its in-progress row (control): %s", r.body)
	}
}

// TestHotZoneSpaces_UserNeverWidensSpaceNarrows — row 134.
func TestHotZoneSpaces_UserNeverWidensSpaceNarrows(t *testing.T) {
	w := newIsoWorld(t)
	for _, p := range w.ordered {
		w.seedCreated(t, p)
	}
	var aOwner string
	if err := w.pool.QueryRow(context.Background(), `SELECT username FROM users WHERE id = $1`, w.byKey[keyASpace].OwnerID).Scan(&aOwner); err != nil {
		t.Fatal(err)
	}
	t.Run("user= never widens", func(t *testing.T) {
		q := "/api/v1/dashboard/scoring?window=all-time&user=" + url.QueryEscape(aOwner)
		count := func(a testsupport.Actor) int64 {
			r := w.get(a, q)
			if r.status != http.StatusOK {
				t.Fatalf("%s: %d %s", q, r.status, r.body)
			}
			var s struct {
				Metrics []struct {
					Key   string `json:"key"`
					Human int64  `json:"human"`
				} `json:"metrics"`
			}
			decodeInto(t, r, &s)
			for _, m := range s.Metrics {
				if m.Key == "issues_created" {
					return m.Human
				}
			}
			return -1
		}
		if got := count(testsupport.ActorOnlyB); got != 0 {
			t.Errorf("uB filtering by A's owner counts %d, want 0 — the filter widened the reach", got)
		}
		if got := count(testsupport.ActorOnlyA); got < 1 {
			t.Errorf("uA filtering by A's owner counts %d, want ≥ 1 (control)", got)
		}
	})
	t.Run("space= narrows every surface", func(t *testing.T) {
		for name, id := range map[string]uuid.UUID{"A": w.f.SpaceA, "B": w.f.SpaceB} {
			short := w.spaceShort(t, id)
			visible, contribution := hotReach(w, testsupport.ActorBoth, name)
			if got, want := w.hotNumbers(t, testsupport.ActorBoth, "&space="+short), w.wantNumbers(t, visible, contribution); got != want {
				t.Errorf("space=%s: %+v, want %+v", name, got, want)
			}
		}
	})
}

// TestHotZoneSpaces_AgentsByReach — row 135 (K-6; sk-755): the agents counted
// are the agents of the people of the reach — an agent of a person who is
// only in another space is not counted; with space= the people narrow. The
// session has no events at all: it counts by its last appearance alone.
func TestHotZoneSpaces_AgentsByReach(t *testing.T) {
	w := newIsoWorld(t)
	ctx := context.Background()
	agentUser, agentName := testsupport.SeedUser(t, w.pool, "user")
	if _, err := w.pool.Exec(ctx, `INSERT INTO space_members (space_id, user_id, role) VALUES ($1, $2, 'member')`, w.f.SpaceB, agentUser); err != nil {
		t.Fatal(err)
	}
	session := "hotzone-agent-" + uuid.NewString()[:8]
	if _, err := w.pool.Exec(ctx, `INSERT INTO agents (user_id, session_id, first_seen_at, last_seen_at) VALUES ($1, $2, now(), now())`,
		agentUser, session); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = w.pool.Exec(c, `DELETE FROM agents WHERE user_id = $1`, agentUser)
		_, _ = w.pool.Exec(c, `DELETE FROM space_members WHERE user_id = $1`, agentUser)
	})
	listed := func(t *testing.T, a testsupport.Actor, query string) bool {
		t.Helper()
		r := w.get(a, "/api/v1/dashboard/agents"+query)
		if r.status != http.StatusOK {
			t.Fatalf("agents: %d %s", r.status, r.body)
		}
		var res struct {
			Items []struct {
				Username string `json:"username"`
			} `json:"items"`
		}
		decodeInto(t, r, &res)
		for _, it := range res.Items {
			if it.Username == agentName {
				return true
			}
		}
		return false
	}
	if listed(t, testsupport.ActorOnlyA, "") {
		t.Errorf("uA's agents count an agent who works only in space B")
	}
	if !listed(t, testsupport.ActorOnlyB, "") {
		t.Errorf("uB's agents miss the agent of his own space (control)")
	}
	if listed(t, testsupport.ActorBoth, "?space="+w.spaceShort(t, w.f.SpaceA)) {
		t.Errorf("uAB with space=A counts the agent of space B")
	}
	if !listed(t, testsupport.ActorBoth, "?space="+w.spaceShort(t, w.f.SpaceB)) {
		t.Errorf("uAB with space=B misses the agent of space B (control)")
	}
}

// sessionEvent records an event of the agent session of user in project at
// the given time — the activity a HOT ZONE agent count reads.
func sessionEvent(t *testing.T, w *isoWorld, user uuid.UUID, session string, project uuid.UUID, at time.Time) {
	t.Helper()
	if _, err := w.pool.Exec(context.Background(), `
INSERT INTO events (occurred_at, type, actor, actor_user_id, actor_type, actor_session_id, entity_type, entity_id, project_id, payload)
VALUES ($1, 'issue.updated', 'agent', $2, 'agent', $3, 'issue', 'agent-work', $4, '{}'::jsonb)`,
		at, user, session, project); err != nil {
		t.Fatalf("session event: %v", err)
	}
	t.Cleanup(func() {
		_, _ = w.pool.Exec(context.Background(), `DELETE FROM events WHERE actor_session_id = $1`, session)
	})
}

// TestHotZoneSpaces_AgentsCountEverySessionOfThePerson — row 135 (K-6,
// US-13; rule replaced by sk-755, maintainers' decision 2026-10-18). Reference —
// the decision: the reach picks the people, and every session of a person of
// the reach counts by its last appearance (agents.last_seen_at), wherever it
// worked. A participant of A and B runs one session that worked only in B
// (live) and one quiet session with no events (seen two days ago). uA — in A
// only — sees both: another participant's B-only session counts as a number.
// uAB under space=A sees both as well (the person is of A); under space=B
// the same. Counterexample: the sk-740 rule (seen = last event in the reach)
// gives uA nothing for the B session and nobody anything for the quiet one.
func TestHotZoneSpaces_AgentsCountEverySessionOfThePerson(t *testing.T) {
	w := newIsoWorld(t)
	ctx := context.Background()
	user, name := testsupport.SeedUser(t, w.pool, "user")
	for _, sp := range []uuid.UUID{w.f.SpaceA, w.f.SpaceB} {
		if _, err := w.pool.Exec(ctx, `INSERT INTO space_members (space_id, user_id, role) VALUES ($1, $2, 'member')`, sp, user); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = w.pool.Exec(c, `DELETE FROM agents WHERE user_id = $1`, user)
		_, _ = w.pool.Exec(c, `DELETE FROM space_members WHERE user_id = $1`, user)
	})
	now := time.Now().UTC()
	inB := "only-b-" + uuid.NewString()[:8]
	quiet := "quiet-" + uuid.NewString()[:8]
	for s, seen := range map[string]time.Time{inB: now, quiet: now.Add(-48 * time.Hour)} {
		if _, err := w.pool.Exec(ctx, `INSERT INTO agents (user_id, session_id, first_seen_at, last_seen_at) VALUES ($1, $2, $3, $3)`,
			user, s, seen); err != nil {
			t.Fatal(err)
		}
	}
	sessionEvent(t, w, user, inB, w.byKey[keyBSpace].ID, now) // works in B only

	type counts struct{ Live, H24, D7, D30, AllTime int64 }
	row := func(t *testing.T, a testsupport.Actor, query string) (counts, bool) {
		t.Helper()
		r := w.get(a, "/api/v1/dashboard/agents"+query)
		if r.status != http.StatusOK {
			t.Fatalf("agents: %d %s", r.status, r.body)
		}
		var res struct {
			Items []struct {
				Username string `json:"username"`
				counts
			} `json:"items"`
		}
		decodeInto(t, r, &res)
		for _, it := range res.Items {
			if it.Username == name {
				return it.counts, true
			}
		}
		return counts{}, false
	}
	want := counts{Live: 1, H24: 1, D7: 2, D30: 2, AllTime: 2}
	for _, c := range []struct {
		label string
		actor testsupport.Actor
		query string
	}{
		{"uA", testsupport.ActorOnlyA, ""},
		{"uAB space=A", testsupport.ActorBoth, "?space=" + w.spaceShort(t, w.f.SpaceA)},
		{"uAB space=B", testsupport.ActorBoth, "?space=" + w.spaceShort(t, w.f.SpaceB)},
	} {
		if got, ok := row(t, c.actor, c.query); !ok || got != want {
			t.Errorf("%s: listed %v, %+v — want both sessions by their last appearance %+v", c.label, ok, got, want)
		}
	}
}

// TestHotZoneSpaces_AllTimeCacheFollowsTheReach — row 136 (K-6). The world's
// dashboard store is one for the test, so its all-time cache is warm
// between requests.
func TestHotZoneSpaces_AllTimeCacheFollowsTheReach(t *testing.T) {
	w := newIsoWorld(t)
	ctx := context.Background()
	for _, p := range w.ordered {
		w.seedCreated(t, p)
	}
	scoring := func(t *testing.T, a testsupport.Actor) int64 {
		t.Helper()
		return w.hotNumbers(t, a, "").Scoring
	}

	t.Run("positive control: an unchanged reach is served from the cache", func(t *testing.T) {
		before := scoring(t, testsupport.ActorOnlyB)
		w.seedCreated(t, w.byKey[keyBSpace]) // a new event, same reach
		if after := scoring(t, testsupport.ActorOnlyB); after != before {
			t.Fatalf("all-time scoring %d → %d right after a write: the cache is not in play, the rows below prove nothing", before, after)
		}
	})

	t.Run("(a) visible → contribution: still counted, rows anonymized", func(t *testing.T) {
		a := testsupport.ActorParticipantNoMembers
		p := w.byKey[keyASpace]
		before := scoring(t, a) // warms the cache with A.space fully visible
		owner := w.f.User(t, testsupport.ActorSpaceOwner)
		if err := spaces.NewStore(w.pool).SetVisibility(ctx, p.ID, spaces.VisibilityMembers,
			spaces.Actor{UserID: owner.ID, Username: owner.Username, Type: "human"}); err != nil {
			t.Fatalf("A.space → members: %v", err)
		}
		if isVisible := w.get(a, "/api/v1/projects/"+p.ID.String()+"/").status == http.StatusOK; isVisible {
			t.Fatalf("fixture: uc¬p still sees A.space after the switch")
		}
		if after := scoring(t, a); after != before {
			t.Errorf("scoring %d → %d: the project's contribution was dropped (it stays — his own space)", before, after)
		}
		r := w.get(a, "/api/v1/dashboard/projects?window=all")
		if bytes.Contains(r.body, []byte(p.Name)) {
			t.Errorf("the project's name is still given after it became contribution-only")
		}
		if got := bytes.Count(r.body, []byte(`"redacted":true`)); got != 2 {
			t.Errorf("anonymized rows %d, want 2 (A.members and now A.space): %s", got, r.body)
		}
	})

	t.Run("(b) left the space: the project is excluded entirely", func(t *testing.T) {
		a := testsupport.ActorOnlyA
		u := w.f.User(t, a)
		scoring(t, a) // warm the cache with space A in the reach
		if err := spaces.NewService(w.pool, spaces.Options{Creation: spaces.CreationAll}).Leave(ctx, spaces.Caller{
			UserID: u.ID, Username: u.Username,
			Actor: spaces.Actor{UserID: u.ID, Username: u.Username, Type: "human"},
		}, w.spaceShort(t, w.f.SpaceA)); err != nil {
			t.Fatalf("leave A: %v", err)
		}
		n := w.hotNumbers(t, a, "")
		if n.Scoring != 1 || n.Rows != 1 || n.Redacted != 0 {
			t.Errorf("after leaving A: %+v, want only his Personal (scoring 1, one full row) — a stale cache keeps A", n)
		}
	})

	t.Run("(c) two actors of one space, different levels", func(t *testing.T) {
		// uA's rows warmed above (before leaving); uAB sees A.members fully,
		// uc¬p only by contribution: after uAB warms, uc¬p still gets the
		// anonymized row of A.members, never its name.
		_ = w.hotNumbers(t, testsupport.ActorBoth, "")
		r := w.get(testsupport.ActorParticipantNoMembers, "/api/v1/dashboard/projects?window=all")
		if bytes.Contains(r.body, []byte(w.byKey[keyAMembers].Name)) {
			t.Errorf("uc¬p got the full row of A.members after uAB warmed the cache")
		}
		// His reach is A.space, A.members (contribution) and his "Personal" —
		// one event each.
		if got := w.hotNumbers(t, testsupport.ActorParticipantNoMembers, "").Scoring; got != 3 {
			t.Errorf("uc¬p's scoring %d after uAB warmed, want 3 (his own reach)", got)
		}
	})
}
