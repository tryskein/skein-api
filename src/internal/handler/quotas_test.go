package handler

// quotas_test.go — sk-674.18 T026 (and the seats of T023), matrix rows 110,
// 111, 113, 113a, 114, 115; the MCP half of 110 and 111 —
// mcp/tools/quotas_test.go. References — INV-15, INV-13, INV-8, INV-28 (3),
// SC-012, US-14 and E-10 of the spec, written before the code:
//
//   - the project quota of a space counts its projects, archived included;
//     the personal space has SKEIN_QUOTA_PERSONAL_*, the regular and the
//     common the space quotas; one space's count never touches another's;
//     an unset quota is no limit;
//   - the storage quota is soft and per space: a confirmation passes while
//     the space's used bytes (all its projects) are strictly below it;
//     reads, downloads and deletions are never refused;
//   - lowering a quota or a limit below the current state keeps what exists
//     and refuses the new;
//   - the seat limit counts in regular spaces only;
//   - GET /me/spaces gives the quota rows of a space, the projects the
//     caller cannot see included;
//   - every quota or limit refusal counts its code in
//     skein_quota_refusals_total.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"skein-api/internal/attachments"
	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
)

// quotaS3 is the object storage boundary: every upload has landed.
type quotaS3 struct{}

func (quotaS3) PresignedPostPolicy(context.Context, string, string, int64, time.Duration) (attachments.PostPolicy, error) {
	return attachments.PostPolicy{}, nil
}
func (quotaS3) PresignedGetURL(_ context.Context, key, _ string, _ time.Duration) (string, error) {
	return "http://storage.test/" + key, nil
}
func (quotaS3) RemoveObject(context.Context, string) error { return nil }
func (quotaS3) StatObject(context.Context, string) error   { return nil }
func (quotaS3) ListKeys(context.Context, string, func(string) error) error {
	return nil
}

// quotaCfg is the SKEIN_QUOTA_* / SKEIN_SPACE_* configuration of a router.
type quotaCfg struct {
	personalProjects, spaceProjects *int
	personalBytes, spaceBytes       *int64
	memberLimit, ownedLimit         *int
}

func qi(v int) *int       { return &v }
func qi64(v int64) *int64 { return &v }

func quotaRouter(pool *pgxpool.Pool, c quotaCfg) chi.Router {
	ph := NewProjectsHandler(pool, nil)
	ph.SetProjectQuotas(c.personalProjects, c.spaceProjects)
	ah := NewAttachmentsHandler(pool, quotaS3{}, "skein-attachments", 1<<40, 600)
	ah.SetStorageQuotas(c.personalBytes, c.spaceBytes)
	sh := NewSpacesHandler(spaces.NewService(pool, spaces.Options{
		Creation: spaces.CreationAll, MemberLimit: c.memberLimit, OwnedLimit: c.ownedLimit,
		QuotaPersonalProjects: c.personalProjects, QuotaSpaceProjects: c.spaceProjects,
		QuotaPersonalStorageBytes: c.personalBytes, QuotaSpaceStorageBytes: c.spaceBytes,
	}))
	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(ParseUserHeaders)
		r.Use(RequireUser)
		r.Get("/me/spaces", sh.MySpaces)
		r.Post("/spaces", sh.Create)
		r.With(RequireAdmin).Post("/spaces/{id}/members", sh.AddMember)
		r.Post("/projects", ph.Create)
		r.Get("/projects/{projectID}", ph.Get)
		r.Patch("/projects/{projectID}", ph.Patch)
		r.Route("/attachments/{id}", func(r chi.Router) {
			r.Post("/confirm", ah.Confirm)
			r.Get("/download-url", ah.DownloadURL)
			r.Delete("/", ah.Delete)
		})
	})
	return r
}

func newQuotaWorld(t *testing.T, c quotaCfg) *projWorld {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	return &projWorld{&spWorld{t: t, pool: pool, router: quotaRouter(pool, c)}}
}

// with is the same world under another configuration (E-10: the operator
// changed the variables).
func (w *projWorld) with(c quotaCfg) *projWorld {
	return &projWorld{&spWorld{t: w.t, pool: w.pool, router: quotaRouter(w.pool, c)}}
}

// project creates a project in space through POST /projects and returns
// its id; ok reports whether it was created, code the refusal otherwise.
func (w *projWorld) project(u spUser, space string) (id uuid.UUID, ok bool, code string) {
	w.t.Helper()
	prefix := uniquePrefix("q")
	r := w.create(u, space, "Quota "+prefix, prefix)
	if r.status == http.StatusCreated || r.status == http.StatusOK {
		raw, _ := r.body["id"].(string)
		if inner, ok := r.body["project"].(map[string]any); ok {
			raw, _ = inner["id"].(string)
		}
		return uuid.MustParse(raw), true, ""
	}
	code, _ = r.body["error"].(string)
	if r.status != http.StatusUnprocessableEntity {
		w.t.Fatalf("create in %s: %d %s", space, r.status, r.raw)
	}
	return uuid.Nil, false, code
}

// pending seeds a pending upload of size bytes on a new issue of project.
func (w *projWorld) pending(u spUser, project uuid.UUID, size int64) uuid.UUID {
	w.t.Helper()
	var prefix string
	if err := w.pool.QueryRow(context.Background(), `SELECT id_prefix FROM projects WHERE id = $1`, project).Scan(&prefix); err != nil {
		w.t.Fatal(err)
	}
	issue := testsupport.SeedIssue(w.t, w.pool, project, u.id, prefix+"-"+uniquePrefix(""), "quota", "open", 2)
	id := testsupport.SeedAttachment(w.t, w.pool, issue, u.id, "pending")
	if _, err := w.pool.Exec(context.Background(), `UPDATE attachments SET size_bytes = $2 WHERE id = $1`, id, size); err != nil {
		w.t.Fatal(err)
	}
	return id
}

// confirm confirms an upload through the REST route: "" when confirmed,
// the refusal code otherwise.
func (w *projWorld) confirm(u spUser, att uuid.UUID) string {
	w.t.Helper()
	r := w.do(u, http.MethodPost, "/api/v1/attachments/"+att.String()+"/confirm", nil)
	if r.status == http.StatusOK {
		return ""
	}
	if r.status != http.StatusUnprocessableEntity {
		w.t.Fatalf("confirm %s: %d %s", att, r.status, r.raw)
	}
	code, _ := r.body["error"].(string)
	return code
}

func (w *projWorld) archive(project uuid.UUID) {
	w.t.Helper()
	if _, err := w.pool.Exec(context.Background(), `UPDATE projects SET archived_at = now() WHERE id = $1`, project); err != nil {
		w.t.Fatal(err)
	}
}

// refusals reads skein_quota_refusals_total{code}.
func refusals(t *testing.T, code string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "skein_quota_refusals_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "code" && l.GetValue() == code {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// Row 110 — the project quota
// ---------------------------------------------------------------------------

func TestQuotas_ProjectQuotaPerSpaceAndKind(t *testing.T) {
	w := newQuotaWorld(t, quotaCfg{personalProjects: qi(1), spaceProjects: qi(2)})
	me := seedSpUser(t, w.pool, "user")

	t.Run("a regular space: archived count, the next is refused", func(t *testing.T) {
		_, a := w.space(me)
		first, ok, _ := w.project(me, a)
		if !ok {
			t.Fatal("the first project was refused")
		}
		w.archive(first)
		if _, ok, code := w.project(me, a); !ok {
			t.Fatalf("the second project (quota 2): refused %s", code)
		}
		if _, ok, code := w.project(me, a); ok || code != spaces.CodeProjectQuota {
			t.Errorf("the third project with one archived: created=%v %q, want %s", ok, code, spaces.CodeProjectQuota)
		}
		_, b := w.space(me)
		if _, ok, code := w.project(me, b); !ok {
			t.Errorf("another space of the same owner: refused %s", code)
		}
	})

	t.Run("the personal space has the personal quota", func(t *testing.T) {
		_, p := w.personal(me)
		if _, ok, code := w.project(me, p); !ok {
			t.Fatalf("the first personal project: refused %s", code)
		}
		if _, ok, code := w.project(me, p); ok || code != spaces.CodeProjectQuota {
			t.Errorf("the second personal project (personal quota 1, space quota 2): created=%v %q", ok, code)
		}
	})

	t.Run("the common space has the space quota", func(t *testing.T) {
		admin := seedSpUser(t, w.pool, "admin")
		commonID, common := w.asCommon(admin)
		w.join(commonID, me)
		if _, ok, code := w.project(me, common); !ok {
			t.Fatalf("the first common project: refused %s", code)
		}
		if _, ok, code := w.project(me, common); !ok {
			t.Errorf("the second common project (space quota 2, personal 1): refused %s", code)
		}
		if _, ok, code := w.project(me, common); ok || code != spaces.CodeProjectQuota {
			t.Errorf("the third common project: created=%v %q", ok, code)
		}
	})

	t.Run("an unset quota is no limit", func(t *testing.T) {
		free := w.with(quotaCfg{})
		_, c := free.space(me)
		for i := 0; i < 4; i++ {
			if _, ok, code := free.project(me, c); !ok {
				t.Fatalf("project %d without a quota: refused %s", i+1, code)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Row 111 — the soft storage quota
// ---------------------------------------------------------------------------

func TestQuotas_StorageQuotaPerSpace(t *testing.T) {
	w := newQuotaWorld(t, quotaCfg{personalBytes: qi64(100), spaceBytes: qi64(300)})
	me := seedSpUser(t, w.pool, "user")

	t.Run("summed over the projects of the space, soft", func(t *testing.T) {
		aID, a := w.space(me)
		p1, _, _ := w.project(me, a)
		p2, _, _ := w.project(me, a)
		big := w.pending(me, p1, 200)
		if code := w.confirm(me, big); code != "" {
			t.Fatalf("0 of 300 used: %s", code)
		}
		if code := w.confirm(me, w.pending(me, p2, 200)); code != "" {
			t.Fatalf("200 of 300 used, another project: %s (soft — the file may pass the quota)", code)
		}
		late := w.pending(me, p1, 10)
		r := w.do(me, http.MethodPost, "/api/v1/attachments/"+late.String()+"/confirm", nil)
		if r.status != http.StatusUnprocessableEntity || r.body["error"] != spaces.CodeStorageQuota {
			t.Fatalf("400 of 300 used: %d %s, want 422 %s", r.status, r.raw, spaces.CodeStorageQuota)
		}
		if r.body["storageUsedBytes"] != float64(400) || r.body["storageQuotaBytes"] != float64(300) {
			t.Errorf("the refusal tells %v / %v, want the space's 400 of 300", r.body["storageUsedBytes"], r.body["storageQuotaBytes"])
		}
		// Over the quota: reading and deleting stay allowed.
		if r := w.do(me, http.MethodGet, "/api/v1/attachments/"+big.String()+"/download-url", nil); r.status != http.StatusOK {
			t.Errorf("download over the quota: %d %s", r.status, r.raw)
		}
		if r := w.do(me, http.MethodDelete, "/api/v1/attachments/"+big.String(), nil); r.status >= 300 {
			t.Errorf("delete over the quota: %d %s", r.status, r.raw)
		}
		if code := w.confirm(me, late); code != "" {
			t.Errorf("after the delete freed the space (200 of 300): %s", code)
		}
		_, b := w.space(me)
		pb, _, _ := w.project(me, b)
		if code := w.confirm(me, w.pending(me, pb, 10)); code != "" {
			t.Errorf("another space while %s is over: %s", aID, code)
		}
	})

	t.Run("the personal space has the personal storage quota", func(t *testing.T) {
		_, p := w.personal(me)
		pp, _, _ := w.project(me, p)
		if code := w.confirm(me, w.pending(me, pp, 150)); code != "" {
			t.Fatalf("0 of 100 used: %s", code)
		}
		if code := w.confirm(me, w.pending(me, pp, 1)); code != spaces.CodeStorageQuota {
			t.Errorf("150 used under the personal 100 (the space quota is 300): %q", code)
		}
	})

	t.Run("the common space has the space storage quota", func(t *testing.T) {
		admin := seedSpUser(t, w.pool, "admin")
		commonID, common := w.asCommon(admin)
		w.join(commonID, me)
		pc, _, _ := w.project(me, common)
		if code := w.confirm(me, w.pending(me, pc, 150)); code != "" {
			t.Fatalf("0 of 300 used: %s", code)
		}
		if code := w.confirm(me, w.pending(me, pc, 1)); code != "" {
			t.Errorf("150 used in the common space (space quota 300, personal 100): %s", code)
		}
	})

	t.Run("an unset storage quota is no limit", func(t *testing.T) {
		free := w.with(quotaCfg{})
		_, c := free.space(me)
		pc, _, _ := free.project(me, c)
		for _, size := range []int64{1 << 40, 1} {
			if code := free.confirm(me, free.pending(me, pc, size)); code != "" {
				t.Fatalf("%d bytes without a quota: %s", size, code)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Row 113 — lowering a quota or a limit below the current state (E-10)
// ---------------------------------------------------------------------------

func TestQuotas_LoweredBelowCurrent(t *testing.T) {
	w := newQuotaWorld(t, quotaCfg{})
	me := seedSpUser(t, w.pool, "user")
	admin := seedSpUser(t, w.pool, "admin")
	spaceID, s := w.space(me)
	var projects []uuid.UUID
	for i := 0; i < 3; i++ {
		p, ok, code := w.project(me, s)
		if !ok {
			t.Fatalf("project %d: %s", i+1, code)
		}
		projects = append(projects, p)
	}
	att := w.pending(me, projects[0], 400)
	if code := w.confirm(me, att); code != "" {
		t.Fatal(code)
	}
	for i := 0; i < 2; i++ {
		w.join(spaceID, seedSpUser(t, w.pool, "user"))
	}

	lowered := w.with(quotaCfg{spaceProjects: qi(2), spaceBytes: qi64(300), memberLimit: qi(2)})
	if n := w.count(`SELECT count(*) FROM projects WHERE space_id = $1`, spaceID); n != 3 {
		t.Errorf("%d projects after the quota went to 2, want the 3 kept", n)
	}
	if r := lowered.do(me, http.MethodPatch, "/api/v1/projects/"+projects[2].String(), map[string]any{"description": "kept"}); r.status != http.StatusOK {
		t.Errorf("editing an existing project over the quota: %d %s", r.status, r.raw)
	}
	if _, ok, code := lowered.project(me, s); ok || code != spaces.CodeProjectQuota {
		t.Errorf("a new project over the lowered quota: created=%v %q", ok, code)
	}
	if r := lowered.do(me, http.MethodGet, "/api/v1/attachments/"+att.String()+"/download-url", nil); r.status != http.StatusOK {
		t.Errorf("an existing file over the lowered storage quota: %d %s", r.status, r.raw)
	}
	if code := lowered.confirm(me, w.pending(me, projects[1], 1)); code != spaces.CodeStorageQuota {
		t.Errorf("a new file over the lowered storage quota: %q", code)
	}
	if n := w.count(`SELECT count(*) FROM space_members WHERE space_id = $1`, spaceID); n != 3 {
		t.Errorf("%d members after the limit went to 2, want the 3 kept", n)
	}
	newcomer := seedSpUser(t, w.pool, "user")
	if r := lowered.do(admin, http.MethodPost, "/api/v1/spaces/"+s+"/members", map[string]any{"userId": newcomer.id.String()}); r.status != http.StatusUnprocessableEntity || r.body["error"] != spaces.CodeNoSeats {
		t.Errorf("a new member over the lowered limit: %d %s, want 422 %s", r.status, r.raw, spaces.CodeNoSeats)
	}
}

// ---------------------------------------------------------------------------
// Row 113a — the seat limit counts in regular spaces only
// ---------------------------------------------------------------------------

func TestQuotas_SeatLimitRegularOnly(t *testing.T) {
	w := newQuotaWorld(t, quotaCfg{memberLimit: qi(1)})
	owner := seedSpUser(t, w.pool, "user")
	admin := seedSpUser(t, w.pool, "admin")
	add := func(w *projWorld, space string) (int, any) {
		u := seedSpUser(t, w.pool, "user")
		r := w.do(admin, http.MethodPost, "/api/v1/spaces/"+space+"/members", map[string]any{"userId": u.id.String()})
		return r.status, r.body["error"]
	}

	_, regular := w.space(owner)
	if status, code := add(w, regular); status != http.StatusUnprocessableEntity || code != spaces.CodeNoSeats {
		t.Errorf("a regular space at its limit of 1: %d %v, want 422 %s", status, code, spaces.CodeNoSeats)
	}
	_, common := w.asCommon(owner)
	for i := 0; i < 2; i++ {
		if status, code := add(w, common); status >= 300 {
			t.Errorf("the common space past the limit of 1 (add %d): %d %v", i+1, status, code)
		}
	}
	free := w.with(quotaCfg{})
	_, open := free.space(owner)
	for i := 0; i < 3; i++ {
		if status, code := add(free, open); status >= 300 {
			t.Errorf("no limit set (add %d): %d %v", i+1, status, code)
		}
	}
}

// ---------------------------------------------------------------------------
// Row 114 — the quota rows of GET /me/spaces
// ---------------------------------------------------------------------------

func TestQuotas_MySpacesRows(t *testing.T) {
	w := newQuotaWorld(t, quotaCfg{personalProjects: qi(7), spaceProjects: qi(9),
		personalBytes: qi64(1000), spaceBytes: qi64(5000), memberLimit: qi(5)})
	// The space is a third user's: me and other are plain participants, so
	// neither is the implicit owner of the other's project (INV-4) — the
	// hidden project is really hidden from me.
	owner := seedSpUser(t, w.pool, "user")
	me := seedSpUser(t, w.pool, "user")
	other := seedSpUser(t, w.pool, "user")
	spaceID, s := w.space(owner)
	w.join(spaceID, me)
	w.join(spaceID, other)
	w.invite(spaceID, uniquePrefix("inv")+"@test.local", owner)
	mine, _, _ := w.project(me, s)
	// The other participant's project of visibility members.
	hidden, _, _ := w.project(other, s)
	if r := w.do(me, http.MethodGet, "/api/v1/projects/"+hidden.String(), nil); r.status < 400 {
		t.Fatalf("precondition: the hidden project is visible to me: %d %s", r.status, r.raw)
	}
	if r := w.do(me, http.MethodGet, "/api/v1/projects/"+mine.String(), nil); r.status != http.StatusOK {
		t.Fatalf("precondition: my own project is not visible to me: %d %s", r.status, r.raw)
	}
	if code := w.confirm(me, w.pending(me, mine, 100)); code != "" {
		t.Fatal(code)
	}
	if code := w.confirm(other, w.pending(other, hidden, 500)); code != "" {
		t.Fatal(code)
	}
	_, personal := w.personal(me)

	row := func(w *projWorld, id string) map[string]any {
		t.Helper()
		r := w.do(me, http.MethodGet, "/api/v1/me/spaces", nil)
		if r.status != http.StatusOK {
			t.Fatalf("GET /me/spaces: %d %s", r.status, r.raw)
		}
		items, _ := r.body["items"].([]any)
		for _, it := range items {
			if m, _ := it.(map[string]any); m["id"] == id {
				return m
			}
		}
		t.Fatalf("space %s is not listed: %s", id, r.raw)
		return nil
	}

	got := row(w, s)
	for k, want := range map[string]any{
		"projectCount": float64(2), "storageUsedBytes": float64(600),
		"memberCount": float64(3), "pendingInvitations": float64(1), "memberLimit": float64(5),
		"projectQuota": float64(9), "storageQuotaBytes": float64(5000),
	} {
		if got[k] != want {
			t.Errorf("%s of the space: %v, want %v", k, got[k], want)
		}
	}
	p := row(w, personal)
	for k, want := range map[string]any{"projectQuota": float64(7), "storageQuotaBytes": float64(1000), "memberLimit": nil} {
		if p[k] != want {
			t.Errorf("%s of \"Personal\": %v, want %v", k, p[k], want)
		}
	}
	unset := row(w.with(quotaCfg{}), s)
	for _, k := range []string{"memberLimit", "projectQuota", "storageQuotaBytes"} {
		if unset[k] != nil {
			t.Errorf("%s with nothing set: %v, want null (not shown)", k, unset[k])
		}
	}
}

// ---------------------------------------------------------------------------
// Row 115 — skein_quota_refusals_total{code}
// ---------------------------------------------------------------------------

func TestQuotas_RefusalsCountTheirCode(t *testing.T) {
	w := newQuotaWorld(t, quotaCfg{spaceProjects: qi(1), spaceBytes: qi64(10), memberLimit: qi(1), ownedLimit: qi(1)})
	me := seedSpUser(t, w.pool, "user")
	admin := seedSpUser(t, w.pool, "admin")
	_, s := w.space(me)
	p, _, _ := w.project(me, s)
	if code := w.confirm(me, w.pending(me, p, 10)); code != "" {
		t.Fatal(code)
	}

	for _, c := range []struct {
		code   string
		refuse func() (int, any)
	}{
		{spaces.CodeProjectQuota, func() (int, any) {
			_, ok, code := w.project(me, s)
			if ok {
				return http.StatusCreated, nil
			}
			return http.StatusUnprocessableEntity, code
		}},
		{spaces.CodeStorageQuota, func() (int, any) {
			code := w.confirm(me, w.pending(me, p, 1))
			if code == "" {
				return http.StatusOK, nil
			}
			return http.StatusUnprocessableEntity, code
		}},
		{spaces.CodeNoSeats, func() (int, any) {
			u := seedSpUser(t, w.pool, "user")
			r := w.do(admin, http.MethodPost, "/api/v1/spaces/"+s+"/members", map[string]any{"userId": u.id.String()})
			return r.status, r.body["error"]
		}},
		{spaces.CodeOwnedLimit, func() (int, any) {
			r := w.do(me, http.MethodPost, "/api/v1/spaces", map[string]any{"name": "One too many " + uniquePrefix("")})
			if id, _ := r.body["id"].(string); id != "" {
				w.forgetCreated(id)
			}
			return r.status, r.body["error"]
		}},
	} {
		t.Run(c.code, func(t *testing.T) {
			before := refusals(t, c.code)
			status, code := c.refuse()
			if status != http.StatusUnprocessableEntity || code != c.code {
				t.Fatalf("the refusal: %d %v, want 422 %s", status, code, c.code)
			}
			if after := refusals(t, c.code); after != before+1 {
				t.Errorf("skein_quota_refusals_total{code=%q}: %v → %v, want +1", c.code, before, after)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The storage refusal by actor
// ---------------------------------------------------------------------------

// TestQuotas_StorageRefusalDisclosesAggregatesToParticipantsOnly — INV-28
// (3): the aggregates of a space (its used bytes, its quota) are given to a
// participant of the space; an external user, a member of one project and
// not of the space, gets the refusal code alone — the used bytes would tell
// him the volume of the projects he does not see.
func TestQuotas_StorageRefusalDisclosesAggregatesToParticipantsOnly(t *testing.T) {
	w := newQuotaWorld(t, quotaCfg{spaceBytes: qi64(100)})
	owner := seedSpUser(t, w.pool, "user")
	participant := seedSpUser(t, w.pool, "user")
	external := seedSpExternal(t, w.pool)
	spaceID, s := w.space(owner)
	w.join(spaceID, participant)
	shared, _, _ := w.project(owner, s)
	hidden, _, _ := w.project(owner, s)
	testsupport.AddProjectMember(t, w.pool, shared, external.id, "member")
	testsupport.AddProjectMember(t, w.pool, shared, participant.id, "member")
	// The hidden project fills the quota.
	if code := w.confirm(owner, w.pending(owner, hidden, 150)); code != "" {
		t.Fatal(code)
	}
	for _, c := range []struct {
		who      spUser
		disclose bool
	}{{external, false}, {participant, true}} {
		att := w.pending(c.who, shared, 1)
		r := w.do(c.who, http.MethodPost, "/api/v1/attachments/"+att.String()+"/confirm", nil)
		if r.status != http.StatusUnprocessableEntity || r.body["error"] != spaces.CodeStorageQuota {
			t.Fatalf("%s: %d %s, want 422 %s", c.who.name, r.status, r.raw, spaces.CodeStorageQuota)
		}
		_, used := r.body["storageUsedBytes"]
		_, quota := r.body["storageQuotaBytes"]
		_, space := r.body["spaceId"]
		if c.disclose && (!used || !quota || !space) {
			t.Errorf("the participant's refusal lacks the aggregates: %s", r.raw)
		}
		if !c.disclose && (used || quota || space) {
			t.Errorf("the external user's refusal discloses the space: %s", r.raw)
		}
	}
}
