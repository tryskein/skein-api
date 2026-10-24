package handler

// users_spaces_test.go — sk-674.18 T037, matrix rows 117, 119. References —
// US-6, US-20, US-7 and E-15 of the spec, written before the code: the
// admin's checkboxes of spaces — POST /users with spaceIds and PUT
// /users/{id}/spaces — leave the user in exactly the chosen spaces; the
// preview of the removals equals what the removal then does; a removed
// space is left as US-7 says (his assignments and claims there are taken
// off, his project memberships there go, the projects stay); the
// deactivation dialog's "Owns: …" is read from the owner rows of GET
// /users/{id}/spaces.

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/projects"
	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
	"skein-api/internal/users"
)

func usersSpacesRouter(pool *pgxpool.Pool) chi.Router {
	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	uh := NewUsersHandler(pool, store, projects.NewStore(pool))
	sh := NewSpacesHandler(spaces.NewService(pool, spaces.Options{Creation: spaces.CreationAll}))
	r := chi.NewRouter()
	r.Route("/api/v1/users", func(r chi.Router) {
		r.Use(ParseUserHeaders, RequireUser, RequireAdmin)
		r.Post("/", uh.Create)
		r.Get("/{id}/spaces", sh.UserSpaces)
		r.Put("/{id}/spaces", sh.PutUserSpaces)
		r.Post("/{id}/spaces/preview", sh.PreviewUserSpaces)
	})
	return r
}

// userSpaces reads GET /users/{id}/spaces as short id → role.
func (w *spWorld) userSpaces(admin spUser, user uuid.UUID) map[string]string {
	w.t.Helper()
	r := w.do(admin, http.MethodGet, "/api/v1/users/"+user.String()+"/spaces", nil)
	if r.status != http.StatusOK {
		w.t.Fatalf("GET spaces: %d %s", r.status, r.raw)
	}
	out := map[string]string{}
	items, _ := r.body["items"].([]any)
	for _, it := range items {
		m, _ := it.(map[string]any)
		out[m["id"].(string)] = m["role"].(string)
	}
	return out
}

func sameSet(got map[string]string, want ...string) bool {
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sort.Strings(want)
	if len(keys) != len(want) {
		return false
	}
	for i := range keys {
		if keys[i] != want[i] {
			return false
		}
	}
	return true
}

func TestUsersSpaces_SetEqualsTheChoice(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	w := &spWorld{t: t, pool: pool, router: usersSpacesRouter(pool)}
	ctx := context.Background()
	admin := seedSpUser(t, pool, "admin")
	owner := seedSpUser(t, pool, "user")
	s1ID, s1 := w.space(owner)
	s2ID, s2 := w.space(owner)
	_, s3 := w.space(owner)

	// POST /users with spaceIds: exactly those spaces.
	email := uniquePrefix("us") + "@spaces.test"
	created := w.do(admin, http.MethodPost, "/api/v1/users/", map[string]any{"email": email,
		"username": uniquePrefix("us"), "name": "Chosen", "password": "long-enough-pass-1", "role": "user", "spaceIds": []string{s1, s2}})
	if created.status != http.StatusCreated && created.status != http.StatusOK {
		t.Fatalf("POST /users: %d %s", created.status, created.raw)
	}
	uid := uuid.MustParse(created.body["id"].(string))
	t.Cleanup(func() {
		c := context.Background()
		for _, q := range []string{
			`DELETE FROM events WHERE actor_user_id = $1`,
			`DELETE FROM space_members WHERE space_id IN (SELECT id FROM spaces WHERE owner_id = $1)`,
			`DELETE FROM space_members WHERE user_id = $1`,
			`DELETE FROM spaces WHERE owner_id = $1`,
			`DELETE FROM users WHERE id = $1`,
		} {
			_, _ = pool.Exec(c, q, uid)
		}
	})
	if got := w.userSpaces(admin, uid); !sameSet(got, s1, s2) || got[s1] != "member" || got[s2] != "member" {
		t.Fatalf("after POST with [%s %s]: %v", s1, s2, got)
	}
	empty := w.do(admin, http.MethodPost, "/api/v1/users/", map[string]any{"email": uniquePrefix("ue") + "@spaces.test",
		"username": uniquePrefix("ue"), "name": "None", "password": "long-enough-pass-1", "role": "user", "spaceIds": []string{}})
	if empty.status != http.StatusCreated && empty.status != http.StatusOK {
		t.Fatalf("POST /users with []: %d %s", empty.status, empty.raw)
	}
	eid := uuid.MustParse(empty.body["id"].(string))
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM space_members WHERE space_id IN (SELECT id FROM spaces WHERE owner_id = $1)`, eid)
		_, _ = pool.Exec(c, `DELETE FROM space_members WHERE user_id = $1`, eid)
		_, _ = pool.Exec(c, `DELETE FROM spaces WHERE owner_id = $1`, eid)
		_, _ = pool.Exec(c, `DELETE FROM users WHERE id = $1`, eid)
	})
	if got := w.userSpaces(admin, eid); len(got) != 0 {
		t.Errorf("after POST with []: %v, want no space (not even the common one)", got)
	}

	// The user's stake in s2: an open assigned issue, a claim, a project he
	// owns and a project he is a member of.
	var uname string
	if err := pool.QueryRow(ctx, `SELECT username FROM users WHERE id = $1`, uid).Scan(&uname); err != nil {
		t.Fatal(err)
	}
	worked, prefix := testsupport.SeedProjectInSpace(t, pool, owner.id, s2ID, "usw")
	testsupport.AddProjectMember(t, pool, worked, uid, "member")
	assigned := testsupport.SeedIssue(t, pool, worked, owner.id, prefix+"-1", "assigned", "open", 2)
	testsupport.SetAssignee(t, pool, assigned, uname)
	claimed := testsupport.SeedIssue(t, pool, worked, owner.id, prefix+"-2", "claimed", "in_progress", 2)
	if _, err := pool.Exec(ctx, `UPDATE issues SET locked_by = 'mcp-session-users-spaces', locked_by_user_id = $2, locked_at = now() WHERE id = $1`, claimed, uid); err != nil {
		t.Fatal(err)
	}
	ownedID, _ := testsupport.SeedProjectInSpace(t, pool, owner.id, s2ID, "uso")
	testsupport.AddProjectMember(t, pool, ownedID, uid, "owner")
	// His stake in s1 must not be touched.
	kept, keptPrefix := testsupport.SeedProjectInSpace(t, pool, owner.id, s1ID, "usk")
	testsupport.AddProjectMember(t, pool, kept, uid, "member")
	keptIssue := testsupport.SeedIssue(t, pool, kept, owner.id, keptPrefix+"-1", "kept", "open", 2)
	testsupport.SetAssignee(t, pool, keptIssue, uname)

	choice := map[string]any{"spaceIds": []string{s1, s3}}
	pv := w.do(admin, http.MethodPost, "/api/v1/users/"+uid.String()+"/spaces/preview", choice)
	if pv.status != http.StatusOK {
		t.Fatalf("preview: %d %s", pv.status, pv.raw)
	}
	removed, _ := pv.body["removed"].([]any)
	if len(removed) != 1 {
		t.Fatalf("preview removals %v, want s2 alone", removed)
	}
	rm := removed[0].(map[string]any)
	owned, _ := rm["ownedProjects"].([]any)
	if rm["id"] != s2 || rm["openAssignedIssues"] != float64(1) || rm["claimedIssues"] != float64(1) || len(owned) != 1 ||
		owned[0].(map[string]any)["id"] != ownedID.String() {
		t.Errorf("preview of s2: %v, want 1 assigned, 1 claim, the owned project %s", rm, ownedID)
	}
	if got := w.userSpaces(admin, uid); !sameSet(got, s1, s2) {
		t.Errorf("the preview changed the set: %v", got)
	}

	put := w.do(admin, http.MethodPut, "/api/v1/users/"+uid.String()+"/spaces", choice)
	if put.status != http.StatusOK {
		t.Fatalf("PUT: %d %s", put.status, put.raw)
	}
	if got := w.userSpaces(admin, uid); !sameSet(got, s1, s3) {
		t.Errorf("after PUT [%s %s]: %v", s1, s3, got)
	}
	// The fact equals the preview: one assignment and one claim taken off,
	// his project rows in s2 gone, the projects stay; s1 untouched.
	if n := w.count(`SELECT count(*) FROM issues WHERE id = $1 AND assignee IS NULL`, assigned); n != 1 {
		t.Error("the open issue in s2 kept his assignment")
	}
	if n := w.count(`SELECT count(*) FROM issues WHERE id = $1 AND locked_by_user_id IS NULL`, claimed); n != 1 {
		t.Error("his claim in s2 was not released")
	}
	if n := w.count(`SELECT count(*) FROM project_members WHERE user_id = $1 AND project_id IN ($2, $3)`, uid, worked, ownedID); n != 0 {
		t.Errorf("%d project rows in s2 left", n)
	}
	if n := w.count(`SELECT count(*) FROM projects WHERE id IN ($1, $2)`, worked, ownedID); n != 2 {
		t.Error("the projects of s2 did not stay")
	}
	if n := w.count(`SELECT count(*) FROM issues WHERE id = $1 AND assignee = $2`, keptIssue, uname); n != 1 {
		t.Error("his assignment in s1 was touched")
	}
	if n := w.count(`SELECT count(*) FROM project_members WHERE user_id = $1 AND project_id = $2`, uid, kept); n != 1 {
		t.Error("his project row in s1 was touched")
	}
}

func TestUsersSpaces_OwnerRowsNameTheOwnership(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	w := &spWorld{t: t, pool: pool, router: usersSpacesRouter(pool)}
	admin := seedSpUser(t, pool, "admin")
	u := seedSpUser(t, pool, "user")
	other := seedSpUser(t, pool, "user")
	_, ownedA := w.space(u)
	_, ownedB := w.space(u)
	joinedID, joined := w.space(other)
	w.join(joinedID, u)
	_, personal := w.personal(u)

	got := w.userSpaces(admin, u.id)
	owns := []string{}
	for id, role := range got {
		if role == "owner" {
			owns = append(owns, id)
		}
	}
	sort.Strings(owns)
	want := []string{ownedA, ownedB}
	sort.Strings(want)
	if len(owns) != 2 || owns[0] != want[0] || owns[1] != want[1] {
		t.Errorf("Owns: %v, want %v", owns, want)
	}
	if got[joined] != "member" {
		t.Errorf("the joined space: %q, want member", got[joined])
	}
	if _, ok := got[personal]; ok {
		t.Error("the personal space is listed among the ownerships")
	}
}

// TestUsersSpaces_AnothersPersonalIsNotFound.
// Reference — US-6 and INV-8: the admin does not see another user's
// "Personal"; a space he does not see answers as a space that does not exist
// — no existence oracle. Both write paths of the admin's checkboxes.
func TestUsersSpaces_AnothersPersonalIsNotFound(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	w := &spWorld{t: t, pool: pool, router: usersSpacesRouter(pool)}
	admin := seedSpUser(t, pool, "admin")
	target := seedSpUser(t, pool, "user")
	stranger := seedSpUser(t, pool, "user")
	_, personal := w.personal(stranger)
	missing := "zzzzzzzz"

	shape := func(r spResp) string {
		keys := make([]string, 0, len(r.body))
		for k := range r.body {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return fmt.Sprint(r.status, " ", r.body["error"], " ", keys)
	}
	put := func(short string) spResp {
		return w.do(admin, http.MethodPut, "/api/v1/users/"+target.id.String()+"/spaces",
			map[string]any{"spaceIds": []string{short}})
	}
	if a, b := shape(put(personal)), shape(put(missing)); a != b {
		t.Errorf("PUT: another's Personal answers %s, a missing space %s", a, b)
	}
	post := func(short string) spResp {
		name := uniquePrefix("up")
		r := w.do(admin, http.MethodPost, "/api/v1/users/", map[string]any{"email": name + "@spaces.test",
			"username": name, "name": "P", "password": "long-enough-pass-1", "role": "user", "spaceIds": []string{short}})
		if id, _ := r.body["id"].(string); id != "" {
			t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id) })
		}
		return r
	}
	if a, b := shape(post(personal)), shape(post(missing)); a != b {
		t.Errorf("POST: another's Personal answers %s, a missing space %s", a, b)
	}
}
