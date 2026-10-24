package handler

// spaces_test.go — sk-674.18 T023, matrix rows 80, 81, 82, 82a (with the
// invitations of T040), 83, 84, 85, 86,
// 87, 88, 90. The references are the spec (US-4, US-6, US-7, INV-8, INV-10,
// INV-13, INV-14, INV-22, E-16, E-19, E-20), screens 2.1–2.3 and the code
// registry of the contract (spaces/contract.go, T013) — all written before
// the code; nothing here is read back from a response first.
//
// The surface is driven through its own routes (the router below mirrors
// cmd/api: RequireUser everywhere, RequireAdmin on the two admin routes),
// the caller named by the X-Skein-User-* headers.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"skein-api/internal/accounts"
	"skein-api/internal/auth"
	"skein-api/internal/invitations"
	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
)

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

type spUser struct {
	id       uuid.UUID
	name     string
	role     string
	external bool
}

func seedSpUser(t *testing.T, pool *pgxpool.Pool, role string) spUser {
	t.Helper()
	id, name := testsupport.SeedUser(t, pool, role)
	return spUser{id: id, name: name, role: role}
}

func seedSpExternal(t *testing.T, pool *pgxpool.Pool) spUser {
	t.Helper()
	u := seedSpUser(t, pool, "user")
	if _, err := pool.Exec(context.Background(), `UPDATE users SET is_external = true WHERE id = $1`, u.id); err != nil {
		t.Fatalf("external: %v", err)
	}
	u.external = true
	return u
}

type spWorld struct {
	t      *testing.T
	pool   *pgxpool.Pool
	router chi.Router
}

func newSpWorld(t *testing.T, opts spaces.Options) *spWorld {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	return &spWorld{t: t, pool: pool, router: spRouter(pool, opts)}
}

// spRouter mirrors the spaces routes of cmd/api.
func spRouter(pool *pgxpool.Pool, opts spaces.Options) chi.Router {
	svc := spaces.NewService(pool, opts)
	h := NewSpacesHandler(svc)
	// The invitation routes of a space (T040), mail on: a refused
	// invitation must leave no email behind.
	ih := NewInvitationsHandler(invitations.NewService(pool, svc, accounts.NewTurnstile("", accounts.TurnstileVerifyURL),
		invitations.Options{EmailEnabled: true, InviteTTL: 24 * time.Hour, DeliveryTTL: time.Hour}))
	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(ParseUserHeaders)
		r.Use(RequireUser)
		r.Get("/me/spaces", h.MySpaces)
		r.Route("/spaces", func(r chi.Router) {
			r.Post("/", h.Create)
			r.Route("/{id}", func(r chi.Router) {
				r.Get("/", h.Get)
				r.Patch("/", h.Rename)
				r.Delete("/", h.Delete)
				r.Get("/deletion-preview", h.DeletionPreview)
				r.Post("/leave", h.Leave)
				r.Get("/members/{userId}/removal-preview", h.RemovalPreview)
				r.Delete("/members/{userId}", h.RemoveMember)
				r.With(RequireAdmin).Post("/members", h.AddMember)
				r.With(RequireAdmin).Post("/owner", h.ChangeOwner)
				r.Post("/invitations", ih.Create)
			})
		})
	})
	return r
}

type spResp struct {
	status int
	body   map[string]any
	raw    string
}

func (w *spWorld) do(u spUser, method, path string, body any) spResp {
	w.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			w.t.Fatalf("marshal: %v", err)
		}
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(auth.HeaderUserID, u.id.String())
	req.Header.Set(auth.HeaderUsername, u.name)
	req.Header.Set(auth.HeaderUserRole, u.role)
	if u.external {
		req.Header.Set(auth.HeaderUserIsExternal, "true")
	}
	rec := httptest.NewRecorder()
	w.router.ServeHTTP(rec, req)
	out := spResp{status: rec.Code, raw: rec.Body.String()}
	if strings.TrimSpace(out.raw) != "" {
		_ = json.Unmarshal(rec.Body.Bytes(), &out.body)
	}
	return out
}

// space seeds a regular space of owner and returns its id and short id.
func (w *spWorld) space(owner spUser) (uuid.UUID, string) {
	w.t.Helper()
	id := testsupport.SeedSpace(w.t, w.pool, owner.id)
	return id, w.short(id)
}

func (w *spWorld) personal(owner spUser) (uuid.UUID, string) {
	w.t.Helper()
	id := testsupport.SeedSpaceOfKind(w.t, w.pool, owner.id, "personal")
	return id, w.short(id)
}

func (w *spWorld) short(id uuid.UUID) string {
	w.t.Helper()
	var s string
	if err := w.pool.QueryRow(context.Background(), `SELECT short_id FROM spaces WHERE id = $1`, id).Scan(&s); err != nil {
		w.t.Fatalf("short id: %v", err)
	}
	return s
}

func (w *spWorld) rename(id uuid.UUID, name string) {
	w.t.Helper()
	if _, err := w.pool.Exec(context.Background(), `UPDATE spaces SET name = $2 WHERE id = $1`, id, name); err != nil {
		w.t.Fatalf("rename: %v", err)
	}
}

func (w *spWorld) join(space uuid.UUID, u spUser) {
	w.t.Helper()
	testsupport.AddSpaceMember(w.t, w.pool, space, u.id)
}

// forgetCreated removes, at cleanup, a space POST /spaces created.
func (w *spWorld) forgetCreated(short string) {
	w.t.Helper()
	w.t.Cleanup(func() {
		ctx := context.Background()
		_, _ = w.pool.Exec(ctx, `DELETE FROM events WHERE entity_type = 'space'
		  AND entity_id IN (SELECT id::text FROM spaces WHERE short_id = $1)`, short)
		_, _ = w.pool.Exec(ctx, `DELETE FROM space_members WHERE space_id IN (SELECT id FROM spaces WHERE short_id = $1)`, short)
		_, _ = w.pool.Exec(ctx, `DELETE FROM spaces WHERE short_id = $1`, short)
	})
}

// invite seeds a pending invitation of email to space by inviter.
func (w *spWorld) invite(space uuid.UUID, email string, inviter spUser) uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.pool.QueryRow(context.Background(), `
INSERT INTO space_invitations (space_id, email, invited_by, lang, expires_at)
VALUES ($1, lower($2), $3, 'ru', now() + interval '1 day') RETURNING id`, space, email, inviter.id).Scan(&id); err != nil {
		w.t.Fatalf("invite: %v", err)
	}
	w.t.Cleanup(func() {
		_, _ = w.pool.Exec(context.Background(), `DELETE FROM space_invitations WHERE id = $1`, id)
	})
	return id
}

func (w *spWorld) isMember(space uuid.UUID, u spUser) (bool, string) {
	w.t.Helper()
	var role *string
	if err := w.pool.QueryRow(context.Background(),
		`SELECT (SELECT role FROM space_members WHERE space_id = $1 AND user_id = $2)`, space, u.id).Scan(&role); err != nil {
		w.t.Fatalf("membership: %v", err)
	}
	if role == nil {
		return false, ""
	}
	return true, *role
}

func (w *spWorld) count(sql string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		w.t.Fatalf("count: %v", err)
	}
	return n
}

// asCommon makes a fresh common space owned by owner for the test: the
// database's own common space, if any, is turned regular for the test and
// turned back afterwards (the kind is unique per installation).
func (w *spWorld) asCommon(owner spUser) (uuid.UUID, string) {
	w.t.Helper()
	ctx := context.Background()
	var stashed *uuid.UUID
	if err := w.pool.QueryRow(ctx, `UPDATE spaces SET kind = 'regular' WHERE kind = 'common' RETURNING id`).Scan(&stashed); err == nil && stashed != nil {
		id := *stashed
		w.t.Cleanup(func() {
			if _, err := w.pool.Exec(context.Background(), `UPDATE spaces SET kind = 'common' WHERE id = $1`, id); err != nil {
				w.t.Errorf("restore the common space: %v", err)
			}
		})
	}
	id := testsupport.SeedSpaceOfKind(w.t, w.pool, owner.id, "common")
	return id, w.short(id)
}

// opsCount reads skein_space_operations_total{operation, result}.
func opsCount(t *testing.T, operation, result string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "skein_space_operations_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["operation"] == operation && labels["result"] == result {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func intp(n int) *int { return &n }

// ---------------------------------------------------------------------------
// Row 80 — creating a space: one decision for the button and the server
// ---------------------------------------------------------------------------

func TestSpaces_CreationDecision(t *testing.T) {
	type limitState string
	const (
		unset      limitState = "limit unset"
		notReached limitState = "limit not reached"
		reached    limitState = "limit reached"
	)
	for _, creation := range []string{spaces.CreationAll, spaces.CreationAdmin, spaces.CreationNone} {
		for _, lim := range []limitState{unset, notReached, reached} {
			for _, role := range []string{"user", "admin"} {
				name := fmt.Sprintf("%s/%s/%s", creation, lim, role)
				t.Run(name, func(t *testing.T) {
					// Reference: screens 2.1 "Button states" and US-4.
					want := ""
					switch {
					case creation == spaces.CreationNone:
						want = spaces.CodeSpaceCreationDisabled
					case creation == spaces.CreationAdmin && role != "admin":
						want = spaces.CodeAdminOnly
					case lim == reached && role != "admin":
						want = spaces.CodeOwnedLimit
					}
					opts := spaces.Options{Creation: creation}
					switch lim {
					case notReached:
						opts.OwnedLimit = intp(2)
					case reached:
						opts.OwnedLimit = intp(1)
					}
					w := newSpWorld(t, opts)
					u := seedSpUser(t, w.pool, role)
					// Owned and not counted: the personal space and the
					// common one (INV-14).
					w.personal(u)
					wantOwned := 0
					if lim != unset {
						w.space(u)
						wantOwned = 1
						w.asCommon(u)
					}
					// Another owner's spaces are his count, not this one's.
					other := seedSpUser(t, w.pool, "user")
					w.space(other)
					w.space(other)

					me := w.do(u, http.MethodGet, "/api/v1/me/spaces", nil)
					if me.status != http.StatusOK {
						t.Fatalf("GET /me/spaces: %d %s", me.status, me.raw)
					}
					if got := int(me.body["ownedCount"].(float64)); got != wantOwned {
						t.Errorf("ownedCount = %d, want %d", got, wantOwned)
					}
					reason, _ := me.body["cannotCreateReason"].(string)
					if me.body["canCreate"] != (want == "") || reason != want {
						t.Errorf("canCreate %v reason %q, want reason %q", me.body["canCreate"], reason, want)
					}

					post := w.do(u, http.MethodPost, "/api/v1/spaces", map[string]any{"name": "Created " + name})
					if want == "" {
						if post.status != http.StatusCreated {
							t.Fatalf("POST /spaces: %d %s, want 201 as the button said", post.status, post.raw)
						}
						w.forgetCreated(post.body["id"].(string))
						return
					}
					if post.status != http.StatusUnprocessableEntity || post.body["error"] != want {
						t.Errorf("POST /spaces: %d %v, want 422 %s — the same reason as the button", post.status, post.body["error"], want)
					}
				})
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Row 81 — the admin creates for another owner (E-19)
// ---------------------------------------------------------------------------

func TestSpaces_CreateForAnotherOwner(t *testing.T) {
	w := newSpWorld(t, spaces.Options{Creation: spaces.CreationAll})
	admin := seedSpUser(t, w.pool, "admin")
	owner := seedSpUser(t, w.pool, "user")

	r := w.do(admin, http.MethodPost, "/api/v1/spaces", map[string]any{"name": "For another", "ownerId": owner.id.String()})
	if r.status != http.StatusCreated {
		t.Fatalf("create: %d %s", r.status, r.raw)
	}
	short := r.body["id"].(string)
	w.forgetCreated(short)
	var id, ownerID uuid.UUID
	if err := w.pool.QueryRow(context.Background(), `SELECT id, owner_id FROM spaces WHERE short_id = $1`, short).Scan(&id, &ownerID); err != nil {
		t.Fatal(err)
	}
	if ownerID != owner.id {
		t.Errorf("owner %s, want %s", ownerID, owner.id)
	}
	if in, role := w.isMember(id, owner); !in || role != spaces.RoleOwner {
		t.Errorf("the owner's participation: %v %q, want owner", in, role)
	}
	if in, _ := w.isMember(id, admin); in {
		t.Errorf("the admin became a participant")
	}

	ext := seedSpExternal(t, w.pool)
	if r := w.do(admin, http.MethodPost, "/api/v1/spaces", map[string]any{"name": "Ext", "ownerId": ext.id.String()}); r.status != http.StatusUnprocessableEntity || r.body["error"] != spaces.CodeExternalUser {
		t.Errorf("an external owner: %d %v, want 422 external_user", r.status, r.body["error"])
	}
	user := seedSpUser(t, w.pool, "user")
	if r := w.do(user, http.MethodPost, "/api/v1/spaces", map[string]any{"name": "Mine", "ownerId": owner.id.String()}); r.status != http.StatusForbidden {
		t.Errorf("ownerId from a non-admin: %d %s, want 403", r.status, r.raw)
	}
}

// ---------------------------------------------------------------------------
// Row 82 — rename; row 82a — a personal space takes no addition or owner
// change, the admin's role included
// ---------------------------------------------------------------------------

func TestSpaces_Rename(t *testing.T) {
	w := newSpWorld(t, spaces.Options{Creation: spaces.CreationAll})
	owner := seedSpUser(t, w.pool, "user")
	member := seedSpUser(t, w.pool, "user")
	admin := seedSpUser(t, w.pool, "admin")
	id, short := w.space(owner)
	w.join(id, member)
	_, personalShort := w.personal(owner)

	for _, c := range []struct {
		who  spUser
		name string
		want int
	}{
		{owner, "Renamed by owner", http.StatusOK},
		{admin, "Renamed by admin", http.StatusOK},
		{member, "Renamed by member", http.StatusForbidden},
	} {
		r := w.do(c.who, http.MethodPatch, "/api/v1/spaces/"+short, map[string]any{"name": c.name})
		if r.status != c.want {
			t.Errorf("%s renames: %d %s, want %d", c.name, r.status, r.raw, c.want)
		}
	}
	var name string
	if err := w.pool.QueryRow(context.Background(), `SELECT name FROM spaces WHERE id = $1`, id).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "Renamed by admin" {
		t.Errorf("name %q, want the admin's", name)
	}
	if r := w.do(owner, http.MethodPatch, "/api/v1/spaces/"+personalShort, map[string]any{"name": "Mine"}); r.status != http.StatusUnprocessableEntity || r.body["error"] != spaces.CodePersonalSpace {
		t.Errorf("renaming a personal space: %d %v, want 422 personal_space", r.status, r.body["error"])
	}
}

// Row 82a — INV-8: a "Personal" takes no invitation (T040), no direct addition
// and no owner change — its owner, the admin on his own "Personal" included,
// is refused personal_space on every mutation; another user's "Personal" is
// not even seen by the admin (404). After the refusals: no participant, no
// invitation, no email and no signal. Red if the admin's role lifts the ban.
func TestSpaces_PersonalSpaceTakesNoAdditionOrOwnerChange(t *testing.T) {
	sink := startPrefsSigSink(t)
	w := newSpWorld(t, spaces.Options{Creation: spaces.CreationAll})
	admin := seedSpUser(t, w.pool, "admin")
	user := seedSpUser(t, w.pool, "user")
	other := seedSpUser(t, w.pool, "user")
	ownID, ownShort := w.personal(admin)
	userPID, userPShort := w.personal(user)
	from := sink.mark()

	// Inviting: by the owner into his own "Personal" (a user and the admin),
	// by email and by userId.
	invites := []map[string]any{
		{"email": "personal-" + uuid.NewString()[:8] + "@invite.test", "language": "en"},
		{"userId": other.id.String(), "language": "en"},
	}
	for _, c := range []struct {
		who   spUser
		short string
	}{{user, userPShort}, {admin, ownShort}} {
		for _, body := range invites {
			r := w.do(c.who, http.MethodPost, "/api/v1/spaces/"+c.short+"/invitations", body)
			if r.status != http.StatusUnprocessableEntity || r.body["error"] != "personal_space" {
				t.Errorf("%s invites into his own personal space (%v): %d %v, want 422 personal_space",
					c.who.role, body, r.status, r.body["error"])
			}
		}
	}
	for _, body := range invites {
		r := w.do(admin, http.MethodPost, "/api/v1/spaces/"+userPShort+"/invitations", body)
		if r.status != http.StatusNotFound || r.body["error"] != "space_not_found" {
			t.Errorf("the admin invites into another user's personal space: %d %v, want 404 space_not_found", r.status, r.body["error"])
		}
	}

	// The admin's own personal space: the action does not exist.
	for _, path := range []string{"/members", "/owner"} {
		r := w.do(admin, http.MethodPost, "/api/v1/spaces/"+ownShort+path, map[string]any{"userId": user.id.String()})
		if r.status != http.StatusUnprocessableEntity || r.body["error"] != spaces.CodePersonalSpace {
			t.Errorf("POST %s on his own personal space: %d %v, want 422 personal_space", path, r.status, r.body["error"])
		}
	}
	// Another user's personal space: the admin does not see it at all.
	for _, path := range []string{"/members", "/owner"} {
		r := w.do(admin, http.MethodPost, "/api/v1/spaces/"+userPShort+path, map[string]any{"userId": admin.id.String()})
		if r.status != http.StatusNotFound || r.body["error"] != spaces.CodeSpaceNotFound {
			t.Errorf("POST %s on another user's personal space: %d %v, want 404 space_not_found", path, r.status, r.body["error"])
		}
	}
	for _, sp := range []uuid.UUID{ownID, userPID} {
		if n := w.count(`SELECT count(*) FROM space_members WHERE space_id = $1`, sp); n != 1 {
			t.Errorf("personal space %s has %d participants after the refusals, want its owner alone", sp, n)
		}
		if n := w.count(`SELECT count(*) FROM space_invitations WHERE space_id = $1`, sp); n != 0 {
			t.Errorf("personal space %s has invitations", sp)
		}
	}
	if n := w.count(`SELECT count(*) FROM outgoing_emails e JOIN space_invitations i ON i.id = e.invitation_id
	                  WHERE i.space_id = ANY($1)`, []uuid.UUID{ownID, userPID}); n != 0 {
		t.Errorf("%d invitation emails queued for personal spaces", n)
	}
	for _, u := range []spUser{admin, user, other} {
		if n := w.count(`SELECT count(*) FROM notifications WHERE user_id = $1 AND type = 'space_invitation'`, u.id); n != 0 {
			t.Errorf("%s got %d invitation pushes after the refusals", u.name, n)
		}
	}
	if got := sink.settle(t, w.pool, from); len(got) != 0 {
		t.Errorf("signals after the refusals: %v, want none", got)
	}
	var owner uuid.UUID
	if err := w.pool.QueryRow(context.Background(), `SELECT owner_id FROM spaces WHERE id = $1`, ownID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != admin.id {
		t.Errorf("the personal space changed owner")
	}
}

// ---------------------------------------------------------------------------
// Row 83 — deleting: the name on the server, personal, common (INV-22)
// ---------------------------------------------------------------------------

func TestSpaces_Delete(t *testing.T) {
	ctx := context.Background()
	t.Run("regular: the confirmation is checked by the server", func(t *testing.T) {
		w := newSpWorld(t, spaces.Options{Creation: spaces.CreationAll})
		owner := seedSpUser(t, w.pool, "user")
		id, short := w.space(owner)
		w.rename(id, "Acme Delete")
		if r := w.do(owner, http.MethodDelete, "/api/v1/spaces/"+short, map[string]any{"confirmName": "acme delete"}); r.status != http.StatusUnprocessableEntity || r.body["error"] != spaces.CodeNameMismatch {
			t.Errorf("a wrong confirmation: %d %v, want 422 name_mismatch", r.status, r.body["error"])
		}
		if n := w.count(`SELECT count(*) FROM spaces WHERE id = $1`, id); n != 1 {
			t.Fatalf("the space went on a wrong confirmation")
		}
		if r := w.do(owner, http.MethodDelete, "/api/v1/spaces/"+short, map[string]any{"confirmName": "Acme Delete"}); r.status != http.StatusNoContent {
			t.Errorf("the right confirmation: %d %s, want 204", r.status, r.raw)
		}
		if n := w.count(`SELECT count(*) FROM spaces WHERE id = $1`, id); n != 0 {
			t.Errorf("the space is still there")
		}
	})
	t.Run("personal: refused", func(t *testing.T) {
		w := newSpWorld(t, spaces.Options{Creation: spaces.CreationAll})
		owner := seedSpUser(t, w.pool, "user")
		id, short := w.personal(owner)
		var name string
		_ = w.pool.QueryRow(ctx, `SELECT name FROM spaces WHERE id = $1`, id).Scan(&name)
		if r := w.do(owner, http.MethodDelete, "/api/v1/spaces/"+short, map[string]any{"confirmName": name}); r.status != http.StatusUnprocessableEntity || r.body["error"] != spaces.CodePersonalSpace {
			t.Errorf("deleting a personal space: %d %v, want 422 personal_space", r.status, r.body["error"])
		}
	})
	t.Run("common: only empty (archived projects count) and with the flag off", func(t *testing.T) {
		admin := seedSpUser(t, testsupport.NewTestPool(t), "admin")
		on := newSpWorld(t, spaces.Options{Creation: spaces.CreationAll, CommonSpaceEnabled: true})
		off := &spWorld{t: t, pool: on.pool, router: spRouter(on.pool, spaces.Options{Creation: spaces.CreationAll})}
		id, short := on.asCommon(admin)
		on.rename(id, "Common Delete")
		pid, _ := testsupport.SeedProjectInSpace(t, on.pool, admin.id, id, "cmd")
		if _, err := on.pool.Exec(ctx, `UPDATE projects SET archived_at = now() WHERE id = $1`, pid); err != nil {
			t.Fatal(err)
		}
		body := map[string]any{"confirmName": "Common Delete"}
		if r := off.do(admin, http.MethodDelete, "/api/v1/spaces/"+short, body); r.status != http.StatusUnprocessableEntity || r.body["error"] != spaces.CodeCommonSpaceNotEmpty {
			t.Errorf("a common space with an archived project: %d %v, want 422 common_space_not_empty", r.status, r.body["error"])
		}
		if _, err := on.pool.Exec(ctx, `DELETE FROM project_members WHERE project_id = $1`, pid); err != nil {
			t.Fatal(err)
		}
		if _, err := on.pool.Exec(ctx, `DELETE FROM project_counters WHERE project_id = $1`, pid); err != nil {
			t.Fatal(err)
		}
		if _, err := on.pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, pid); err != nil {
			t.Fatal(err)
		}
		if r := on.do(admin, http.MethodDelete, "/api/v1/spaces/"+short, body); r.status != http.StatusUnprocessableEntity || r.body["error"] != spaces.CodeCommonSpaceEnabled {
			t.Errorf("an empty common space with the flag on: %d %v, want 422 common_space_enabled", r.status, r.body["error"])
		}
		if r := off.do(admin, http.MethodDelete, "/api/v1/spaces/"+short, body); r.status != http.StatusNoContent {
			t.Errorf("an empty common space with the flag off: %d %s, want 204", r.status, r.raw)
		}
	})
}

// ---------------------------------------------------------------------------
// Row 84 — previews equal the fact
// ---------------------------------------------------------------------------

func TestSpaces_PreviewsEqualTheFact(t *testing.T) {
	ctx := context.Background()
	t.Run("deletion-preview", func(t *testing.T) {
		w := newSpWorld(t, spaces.Options{Creation: spaces.CreationAll})
		owner := seedSpUser(t, w.pool, "user")
		id, short := w.space(owner)
		p1, pre1 := testsupport.SeedProjectInSpace(t, w.pool, owner.id, id, "dpv")
		p2, pre2 := testsupport.SeedProjectInSpace(t, w.pool, owner.id, id, "dpv")
		if _, err := w.pool.Exec(ctx, `UPDATE projects SET archived_at = now() WHERE id = $1`, p2); err != nil {
			t.Fatal(err)
		}
		testsupport.SeedIssue(t, w.pool, p1, owner.id, pre1+"-1", "dp a", "open", 2)
		testsupport.SeedIssue(t, w.pool, p1, owner.id, pre1+"-2", "dp b", "closed", 2)
		testsupport.SeedIssue(t, w.pool, p2, owner.id, pre2+"-1", "dp c", "open", 2)
		inviter := owner
		w.invite(id, "dpv-"+uuid.NewString()[:8]+"@test.local", inviter)
		accepted := w.invite(id, "dpv-"+uuid.NewString()[:8]+"@test.local", inviter)
		if _, err := w.pool.Exec(ctx, `UPDATE space_invitations SET status = 'accepted', resolved_at = now() WHERE id = $1`, accepted); err != nil {
			t.Fatal(err)
		}
		if _, err := w.pool.Exec(ctx, `INSERT INTO space_storage_usage (space_id, bytes) VALUES ($1, 4321)`, id); err != nil {
			t.Fatal(err)
		}

		pv := w.do(owner, http.MethodGet, "/api/v1/spaces/"+short+"/deletion-preview", nil)
		if pv.status != http.StatusOK {
			t.Fatalf("preview: %d %s", pv.status, pv.raw)
		}
		// The fact, read before the deletion from the rows it removes.
		projects := w.count(`SELECT count(*) FROM projects WHERE space_id = $1`, id)
		issues := w.count(`SELECT count(*) FROM issues i JOIN projects p ON p.id = i.project_id WHERE p.space_id = $1`, id)
		pending := w.count(`SELECT count(*) FROM space_invitations WHERE space_id = $1 AND status = 'pending'`, id)

		var name string
		_ = w.pool.QueryRow(ctx, `SELECT name FROM spaces WHERE id = $1`, id).Scan(&name)
		if r := w.do(owner, http.MethodDelete, "/api/v1/spaces/"+short, map[string]any{"confirmName": name}); r.status != http.StatusNoContent {
			t.Fatalf("delete: %d %s", r.status, r.raw)
		}
		revoked := w.count(`SELECT count(*) FROM space_invitations WHERE invited_by = $1 AND status = 'revoked' AND space_id IS NULL`, owner.id)
		gone := w.count(`SELECT count(*) FROM projects WHERE id = ANY($1)`, []uuid.UUID{p1, p2})
		if gone != 0 {
			t.Fatalf("projects left after the deletion: %d", gone)
		}
		for _, c := range []struct {
			key  string
			want float64
		}{
			{"projectCount", float64(projects)},
			{"issueCount", float64(issues)},
			{"pendingInvitations", float64(revoked)},
			{"storageBytes", 4321},
		} {
			if got := pv.body[c.key]; got != c.want {
				t.Errorf("preview %s = %v, the deletion did %v", c.key, got, c.want)
			}
		}
		if pending != revoked {
			t.Errorf("pending before %d, revoked by the deletion %d", pending, revoked)
		}
		if pv.body["ownerUsername"] != owner.name {
			t.Errorf("ownerUsername = %v, want %s", pv.body["ownerUsername"], owner.name)
		}
	})

	t.Run("removal-preview", func(t *testing.T) {
		w := newSpWorld(t, spaces.Options{Creation: spaces.CreationAll})
		owner := seedSpUser(t, w.pool, "user")
		m := seedSpUser(t, w.pool, "user")
		id, short := w.space(owner)
		w.join(id, m)
		p1, pre1 := testsupport.SeedProjectInSpace(t, w.pool, owner.id, id, "rpv")
		p2, _ := testsupport.SeedProjectInSpace(t, w.pool, m.id, id, "rpv")
		testsupport.AddProjectMember(t, w.pool, p1, m.id, "member")
		for i, st := range []string{"open", "in_progress", "closed"} {
			iid := fmt.Sprintf("%s-%d", pre1, i+1)
			testsupport.SeedIssue(t, w.pool, p1, owner.id, iid, "rp "+st, st, 2)
			testsupport.SetAssignee(t, w.pool, iid, m.name)
		}
		if _, err := w.pool.Exec(ctx, `UPDATE issues SET locked_by = 'sess', locked_by_user_id = $2, locked_at = now() WHERE id = $1`,
			pre1+"-2", m.id); err != nil {
			t.Fatal(err)
		}
		assignedBefore := w.count(`SELECT count(*) FROM issues i JOIN projects p ON p.id = i.project_id
		  WHERE p.space_id = $1 AND i.assignee = $2 AND i.status <> 'closed'`, id, m.name)
		claimedBefore := w.count(`SELECT count(*) FROM issues i JOIN projects p ON p.id = i.project_id
		  WHERE p.space_id = $1 AND i.locked_by_user_id = $2`, id, m.id)

		pv := w.do(owner, http.MethodGet, "/api/v1/spaces/"+short+"/members/"+m.id.String()+"/removal-preview", nil)
		if pv.status != http.StatusOK {
			t.Fatalf("preview: %d %s", pv.status, pv.raw)
		}
		// The preview changed nothing.
		if in, _ := w.isMember(id, m); !in {
			t.Fatalf("the preview removed the member")
		}
		if r := w.do(owner, http.MethodDelete, "/api/v1/spaces/"+short+"/members/"+m.id.String(), nil); r.status != http.StatusNoContent {
			t.Fatalf("remove: %d %s", r.status, r.raw)
		}
		assignedAfter := w.count(`SELECT count(*) FROM issues i JOIN projects p ON p.id = i.project_id
		  WHERE p.space_id = $1 AND i.assignee = $2 AND i.status <> 'closed'`, id, m.name)
		claimedAfter := w.count(`SELECT count(*) FROM issues i JOIN projects p ON p.id = i.project_id
		  WHERE p.space_id = $1 AND i.locked_by_user_id = $2`, id, m.id)
		if got, did := pv.body["openAssignedIssues"], float64(assignedBefore-assignedAfter); got != did {
			t.Errorf("openAssignedIssues = %v, the removal unassigned %v", got, did)
		}
		if got, did := pv.body["claimedIssues"], float64(claimedBefore-claimedAfter); got != did {
			t.Errorf("claimedIssues = %v, the removal released %v", got, did)
		}
		owned, _ := pv.body["ownedProjects"].([]any)
		if len(owned) != 1 || owned[0].(map[string]any)["id"] != p2.String() {
			t.Errorf("ownedProjects = %v, want exactly %s (still in the space after the removal)", owned, p2)
		}
		if n := w.count(`SELECT count(*) FROM projects WHERE id = $1 AND space_id = $2`, p2, id); n != 1 {
			t.Errorf("the owned project left the space")
		}
	})
}

// ---------------------------------------------------------------------------
// Row 85 — GET /me/spaces: order, the admin's reach, the external user
// ---------------------------------------------------------------------------

func TestSpaces_MySpaces(t *testing.T) {
	w := newSpWorld(t, spaces.Options{Creation: spaces.CreationAll})
	commonOwner := seedSpUser(t, w.pool, "user")
	u := seedSpUser(t, w.pool, "user")
	admin := seedSpUser(t, w.pool, "admin")
	_, uPersonal := w.personal(u)
	_, adminPersonal := w.personal(admin)
	commonID, commonShort := w.asCommon(commonOwner)
	w.join(commonID, u)
	bID, bShort := w.space(u)
	w.rename(bID, "Bravo")
	aID, aShort := w.space(u)
	w.rename(aID, "alpha")
	otherOwner := seedSpUser(t, w.pool, "user")
	emptyID, emptyShort := w.space(otherOwner) // no project at all
	_ = emptyID

	r := w.do(u, http.MethodGet, "/api/v1/me/spaces", nil)
	var got []string
	for _, it := range r.body["items"].([]any) {
		got = append(got, it.(map[string]any)["id"].(string))
	}
	want := []string{uPersonal, commonShort, aShort, bShort}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("order %v, want personal → common → by name %v", got, want)
	}

	r = w.do(admin, http.MethodGet, "/api/v1/me/spaces", nil)
	byID := map[string]map[string]any{}
	for _, it := range r.body["items"].([]any) {
		m := it.(map[string]any)
		byID[m["id"].(string)] = m
	}
	if _, ok := byID[uPersonal]; ok {
		t.Errorf("the admin sees another user's personal space")
	}
	if m := byID[adminPersonal]; m == nil || m["myRole"] != spaces.RoleOwner {
		t.Errorf("the admin's own personal space: %v", m)
	}
	for _, s := range []string{emptyShort, aShort, commonShort} {
		m := byID[s]
		if m == nil {
			t.Errorf("the admin does not see space %s", s)
			continue
		}
		if m["adminAccess"] != true || m["canCreateProject"] != false || m["myRole"] != nil {
			t.Errorf("space %s for the admin outside it: %v, want adminAccess, no role, canCreateProject false", s, m)
		}
	}

	ext := seedSpExternal(t, w.pool)
	r = w.do(ext, http.MethodGet, "/api/v1/me/spaces", nil)
	if items := r.body["items"].([]any); len(items) != 0 {
		t.Errorf("an external user sees spaces: %v", items)
	}
	if r.body["canCreate"] != false || r.body["cannotCreateReason"] != spaces.CodeExternalUser {
		t.Errorf("an external user may create: %v %v", r.body["canCreate"], r.body["cannotCreateReason"])
	}
}

// ---------------------------------------------------------------------------
// Row 86 — leaving and removal (INV-10)
// ---------------------------------------------------------------------------

func TestSpaces_LeaveAndRemove(t *testing.T) {
	w := newSpWorld(t, spaces.Options{Creation: spaces.CreationAll})
	owner := seedSpUser(t, w.pool, "user")
	m1 := seedSpUser(t, w.pool, "user")
	m2 := seedSpUser(t, w.pool, "user")
	m3 := seedSpUser(t, w.pool, "user")
	admin := seedSpUser(t, w.pool, "admin")
	id, short := w.space(owner)
	for _, m := range []spUser{m1, m2, m3} {
		w.join(id, m)
	}
	base := "/api/v1/spaces/" + short
	steps := []struct {
		name   string
		who    spUser
		method string
		path   string
		status int
		code   string
	}{
		{"the owner does not leave", owner, http.MethodPost, base + "/leave", 422, spaces.CodeOwnerCannotLeave},
		{"the owner is not removed, even by the admin", admin, http.MethodDelete, base + "/members/" + owner.id.String(), 422, spaces.CodeOwnerCannotBeRemoved},
		{"a member removes nobody", m1, http.MethodDelete, base + "/members/" + m2.id.String(), 403, "forbidden"},
		{"a member leaves", m1, http.MethodPost, base + "/leave", 204, ""},
		{"the owner removes a member", owner, http.MethodDelete, base + "/members/" + m2.id.String(), 204, ""},
		{"the admin removes a member", admin, http.MethodDelete, base + "/members/" + m3.id.String(), 204, ""},
	}
	for _, s := range steps {
		r := w.do(s.who, s.method, s.path, nil)
		if r.status != s.status || (s.code != "" && r.body["error"] != s.code) {
			t.Errorf("%s: %d %v, want %d %s", s.name, r.status, r.body["error"], s.status, s.code)
		}
	}
	for _, m := range []spUser{m1, m2, m3} {
		if in, _ := w.isMember(id, m); in {
			t.Errorf("%s is still a participant", m.name)
		}
	}
	if in, role := w.isMember(id, owner); !in || role != spaces.RoleOwner {
		t.Errorf("the owner is no longer the owner")
	}
}

// ---------------------------------------------------------------------------
// Row 87 — the admin adds at once (US-6, INV-13, E-16)
// ---------------------------------------------------------------------------

func TestSpaces_AdminAdds(t *testing.T) {
	w := newSpWorld(t, spaces.Options{Creation: spaces.CreationAll, MemberLimit: intp(3)})
	owner := seedSpUser(t, w.pool, "user")
	member := seedSpUser(t, w.pool, "user")
	admin := seedSpUser(t, w.pool, "admin")
	invited := seedSpUser(t, w.pool, "user")
	late := seedSpUser(t, w.pool, "user")
	id, short := w.space(owner)
	w.join(id, member)
	var invitedEmail string
	if err := w.pool.QueryRow(context.Background(), `SELECT email FROM users WHERE id = $1`, invited.id).Scan(&invitedEmail); err != nil {
		t.Fatal(err)
	}
	inv := w.invite(id, invitedEmail, owner)
	path := "/api/v1/spaces/" + short + "/members"

	// Seats: owner + member + the pending invitation = 3 of 3.
	if r := w.do(admin, http.MethodPost, path, map[string]any{"userId": late.id.String()}); r.status != http.StatusUnprocessableEntity || r.body["error"] != spaces.CodeNoSeats {
		t.Errorf("no seat left: %d %v, want 422 no_seats", r.status, r.body["error"])
	}
	// The invited user's own invitation holds his seat; it is closed as
	// accepted.
	if r := w.do(admin, http.MethodPost, path, map[string]any{"userId": invited.id.String()}); r.status != http.StatusCreated {
		t.Fatalf("adding the invited user: %d %s", r.status, r.raw)
	}
	var status string
	if err := w.pool.QueryRow(context.Background(), `SELECT status FROM space_invitations WHERE id = $1`, inv).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "accepted" {
		t.Errorf("the pending invitation is %q, want accepted", status)
	}
	if in, role := w.isMember(id, invited); !in || role != spaces.RoleMember {
		t.Errorf("the added user: %v %q, want a member", in, role)
	}
	if n := w.count(`SELECT count(*) FROM notifications WHERE user_id = $1`, invited.id); n != 0 {
		t.Errorf("the added user got %d notification rows, want no Inbox card", n)
	}
	if r := w.do(admin, http.MethodPost, path, map[string]any{"userId": member.id.String()}); r.status != http.StatusUnprocessableEntity || r.body["error"] != spaces.CodeAlreadyMember {
		t.Errorf("adding a member: %d %v, want 422 already_member", r.status, r.body["error"])
	}
	ext := seedSpExternal(t, w.pool)
	w2 := newSpWorld(t, spaces.Options{Creation: spaces.CreationAll})
	id2, short2 := w2.space(owner)
	if r := w2.do(admin, http.MethodPost, "/api/v1/spaces/"+short2+"/members", map[string]any{"userId": ext.id.String()}); r.status != http.StatusUnprocessableEntity || r.body["error"] != spaces.CodeExternalUser {
		t.Errorf("adding an external user: %d %v, want 422 external_user", r.status, r.body["error"])
	}
	if r := w2.do(owner, http.MethodPost, "/api/v1/spaces/"+short2+"/members", map[string]any{"userId": late.id.String()}); r.status != http.StatusForbidden || r.body["error"] != "not_admin" {
		t.Errorf("the owner adding at once: %d %v, want 403 not_admin", r.status, r.body["error"])
	}
	if in, _ := w2.isMember(id2, late); in {
		t.Errorf("a refused addition left a participation")
	}
}

// ---------------------------------------------------------------------------
// Row 88 — the owner change (INV-10, E-20)
// ---------------------------------------------------------------------------

func TestSpaces_ChangeOwner(t *testing.T) {
	// The new owner already owns SKEIN_SPACE_OWNED_LIMIT spaces: not checked.
	w := newSpWorld(t, spaces.Options{Creation: spaces.CreationAll, OwnedLimit: intp(1)})
	owner := seedSpUser(t, w.pool, "user")
	next := seedSpUser(t, w.pool, "user")
	stranger := seedSpUser(t, w.pool, "user")
	admin := seedSpUser(t, w.pool, "admin")
	id, short := w.space(owner)
	w.join(id, next)
	w.space(next)
	inv := w.invite(id, "co-"+uuid.NewString()[:8]+"@test.local", owner)
	path := "/api/v1/spaces/" + short + "/owner"

	if r := w.do(owner, http.MethodPost, path, map[string]any{"userId": next.id.String()}); r.status != http.StatusForbidden || r.body["error"] != "not_admin" {
		t.Errorf("the owner changing the owner: %d %v, want 403 not_admin", r.status, r.body["error"])
	}
	if r := w.do(admin, http.MethodPost, path, map[string]any{"userId": stranger.id.String()}); r.status != http.StatusUnprocessableEntity || r.body["error"] != spaces.CodeNotAMember {
		t.Errorf("a new owner outside the space: %d %v, want 422 not_a_member", r.status, r.body["error"])
	}
	if r := w.do(admin, http.MethodPost, path, map[string]any{"userId": next.id.String()}); r.status != http.StatusOK {
		t.Fatalf("change the owner: %d %s", r.status, r.raw)
	}
	if in, role := w.isMember(id, next); !in || role != spaces.RoleOwner {
		t.Errorf("the new owner: %v %q", in, role)
	}
	if in, role := w.isMember(id, owner); !in || role != spaces.RoleMember {
		t.Errorf("the previous owner: %v %q, want a member", in, role)
	}
	var status string
	if err := w.pool.QueryRow(context.Background(), `SELECT status FROM space_invitations WHERE id = $1`, inv).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Errorf("the pending invitation is %q after the owner change, want untouched", status)
	}
}

// ---------------------------------------------------------------------------
// Row 90 — skein_space_operations_total{operation, result}
// ---------------------------------------------------------------------------

func TestSpaces_OperationsMetric(t *testing.T) {
	ctx := context.Background()
	w := newSpWorld(t, spaces.Options{Creation: spaces.CreationAll})
	off := &spWorld{t: t, pool: w.pool, router: spRouter(w.pool, spaces.Options{Creation: spaces.CreationNone})}
	owner := seedSpUser(t, w.pool, "user")
	m1 := seedSpUser(t, w.pool, "user")
	m2 := seedSpUser(t, w.pool, "user")
	admin := seedSpUser(t, w.pool, "admin")
	newcomer := seedSpUser(t, w.pool, "user")
	id, short := w.space(owner)
	w.join(id, m1)
	w.join(id, m2)
	base := "/api/v1/spaces/" + short
	var name string
	_ = w.pool.QueryRow(ctx, `SELECT name FROM spaces WHERE id = $1`, id).Scan(&name)

	type step struct {
		op, result string
		run        func() spResp
	}
	steps := []step{
		{"create", "success", func() spResp {
			r := w.do(owner, http.MethodPost, "/api/v1/spaces", map[string]any{"name": "Metric"})
			if r.status == http.StatusCreated {
				w.forgetCreated(r.body["id"].(string))
			}
			return r
		}},
		{"create", "refused", func() spResp { return off.do(owner, http.MethodPost, "/api/v1/spaces", map[string]any{"name": "No"}) }},
		{"leave", "success", func() spResp { return w.do(m1, http.MethodPost, base+"/leave", nil) }},
		{"leave", "refused", func() spResp { return w.do(owner, http.MethodPost, base+"/leave", nil) }},
		{"remove", "success", func() spResp { return w.do(owner, http.MethodDelete, base+"/members/"+m2.id.String(), nil) }},
		{"remove", "refused", func() spResp { return w.do(owner, http.MethodDelete, base+"/members/"+owner.id.String(), nil) }},
		{"admin_add", "success", func() spResp {
			return w.do(admin, http.MethodPost, base+"/members", map[string]any{"userId": newcomer.id.String()})
		}},
		{"admin_add", "refused", func() spResp {
			return w.do(admin, http.MethodPost, base+"/members", map[string]any{"userId": newcomer.id.String()})
		}},
		{"owner_change", "refused", func() spResp {
			return w.do(admin, http.MethodPost, base+"/owner", map[string]any{"userId": m1.id.String()})
		}},
		{"owner_change", "success", func() spResp {
			return w.do(admin, http.MethodPost, base+"/owner", map[string]any{"userId": newcomer.id.String()})
		}},
		{"delete", "refused", func() spResp {
			return w.do(admin, http.MethodDelete, base, map[string]any{"confirmName": name + "?"})
		}},
		{"delete", "success", func() spResp { return w.do(admin, http.MethodDelete, base, map[string]any{"confirmName": name}) }},
	}
	for _, s := range steps {
		before := opsCount(t, s.op, s.result)
		r := s.run()
		if s.result == "success" && r.status >= 300 {
			t.Fatalf("%s should succeed: %d %s", s.op, r.status, r.raw)
		}
		if s.result == "refused" && r.status < 400 {
			t.Fatalf("%s should be refused: %d %s", s.op, r.status, r.raw)
		}
		if d := opsCount(t, s.op, s.result) - before; d != 1 {
			t.Errorf("skein_space_operations_total{%s,%s} moved by %v, want 1", s.op, s.result, d)
		}
	}
}
