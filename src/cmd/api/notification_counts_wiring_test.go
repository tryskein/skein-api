package main

// notification_counts_wiring_test.go — sk-622.21, T024, matrix row No. 50
// (INV-11, US-16: the counts gate is on the route).
//
// The production router (buildRouter, the /me handler composed by
// newNotificationReads as main composes it) runs on a pool of its own with a
// pgx tracer and MaxConns = 8, so several computations can be in flight and
// one can be held while others pass. The tracer recognises the header-count
// statement (its `AS mechanic_enabled` column) and records whose it is (its
// first argument, the caller's id). It can hold ONE such statement in
// TraceQueryEnd — pgx calls it when the rows are closed, i.e. after the
// statement's result (the old value) has been read — until the test lets it
// go. The hold is the test pool's boundary, not product code.
//
// What is asserted:
//
//   - N simultaneous requests of one user → one count statement; U and V in
//     the same moment → each gets literally their own numbers;
//   - a request sent while U's computation is held does not join it: after a
//     committed change of U, a new request of U returns the NEW value and a
//     request of V returns V's; the held request then returns the OLD value;
//   - a following request of U, with no further change, returns the new
//     value — the late old result was not kept.
//
// One statement per computation is asserted separately, by count.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/config"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/search"
	"skein-api/internal/testsupport"
)

// countsTracer records the header-count statements by caller and can hold
// one of them after its result was read.
type countsTracer struct {
	mu      sync.Mutex
	byUser  map[uuid.UUID]int
	holdFor uuid.UUID     // the caller whose next count statement is held; Nil = none
	held    chan struct{} // closed when the held statement has read its result
	release chan struct{} // closed by the test to let it go
}

type countsTraceKey struct{}

func (c *countsTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if !strings.Contains(d.SQL, "AS mechanic_enabled") || len(d.Args) == 0 {
		return ctx
	}
	u, ok := d.Args[0].(uuid.UUID)
	if !ok {
		return ctx
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byUser[u]++
	if c.holdFor == u {
		c.holdFor = uuid.Nil // one statement only
		return context.WithValue(ctx, countsTraceKey{}, true)
	}
	return ctx
}

func (c *countsTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if hold, _ := ctx.Value(countsTraceKey{}).(bool); hold {
		close(c.held)
		<-c.release
	}
}

func (c *countsTracer) statements(u uuid.UUID) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byUser[u]
}

func (c *countsTracer) arm(u uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.holdFor = u
	c.held = make(chan struct{})
	c.release = make(chan struct{})
}

// countsWiringRouter builds the production router on a traced pool.
func countsWiringRouter(t *testing.T) (chi.Router, *pgxpool.Pool, *countsTracer) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run DB-backed tests")
	}
	pcfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	tracer := &countsTracer{byUser: map[uuid.UUID]int{}}
	pcfg.ConnConfig.Tracer = tracer
	pcfg.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(context.Background(), pcfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	t.Setenv("DATABASE_URL", url)
	t.Setenv("SKEIN_INTERNAL_SECRET", wiringInternalSecret)
	t.Setenv("SKEIN_BCRYPT_COST", "4")
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("SKEIN_AGENT_PUBLIC_URL", "http://skein-wiring.test")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	issuesStore := issues.NewStore(pool)
	notifStore, meH := newNotificationReads(pool, cfg, issuesStore)
	searchSvc, err := search.NewService(nil, nil, 0)
	if err != nil {
		t.Fatalf("search.NewService: %v", err)
	}
	router, _ := buildRouter(
		pool, cfg,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, // REST handlers authH…dashboardH
		nil,                               // usersStore
		issuesStore,                       // issuesStore
		dependencies.NewStore(pool),       // depsStore
		projects.NewStore(pool),           // projectsStore
		notifStore,                        // notifStore
		nil,                               // remindersStore
		searchSvc,                         // searchSvc
		nil,                               // s3Client
		nil,                               // attachmentsH
		meH,                               // meH — the gate under test
		nil, nil, nil, nil, nil, nil, nil, // themesH…pushH
	)
	return router, pool, tracer
}

// countsInbox sends GET /me/notification-counts as user and returns inbox.
func countsInbox(t *testing.T, router chi.Router, user uuid.UUID) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/notification-counts", nil)
	req.Header.Set(auth.HeaderInternalSecret, wiringInternalSecret)
	req.Header.Set(auth.HeaderUserID, user.String())
	req.Header.Set(auth.HeaderSessionEpoch, wiringSessionEpoch)
	req.Header.Set(auth.HeaderUserRole, "user")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET /me/notification-counts = %d: %s", rec.Code, rec.Body.String())
		return -1
	}
	var body struct {
		Inbox int `json:"inbox"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Errorf("decode counts: %v", err)
		return -1
	}
	return body.Inbox
}

// TestNotificationCountsWiring_GateOnTheRoute — No. 50.
func TestNotificationCountsWiring_GateOnTheRoute(t *testing.T) {
	router, pool, tracer := countsWiringRouter(t)

	owner, _ := testsupport.SeedUser(t, pool, "user")
	u, uName := testsupport.SeedUser(t, pool, "user")
	v, vName := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "cwire")
	// Inbox counts tasks of projects the assignee may work in (INV-30,
	// sk-674.18 T029): U and V are members.
	testsupport.AddProjectMember(t, pool, pid, u, "member")
	testsupport.AddProjectMember(t, pool, pid, v, "member")
	n := 0
	inboxTask := func(assignee string) {
		n++
		id := testsupport.SeedIssue(t, pool, pid, owner, prefix+"-"+strconv.Itoa(n), "inbox", "open", 2)
		testsupport.SetAssignee(t, pool, id, assignee)
	}
	// Distinguishable seeds: U has 1 in Inbox, V has 2.
	inboxTask(uName)
	inboxTask(vName)
	inboxTask(vName)

	// 1) N simultaneous requests of U and one of V in the same moment.
	const nU = 5
	start := make(chan struct{})
	got := make(chan [2]int, nU+1) // {0=U,1=V, inbox}
	var wg sync.WaitGroup
	for i := 0; i <= nU; i++ {
		who, idx := u, 0
		if i == nU {
			who, idx = v, 1
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got <- [2]int{idx, countsInbox(t, router, who)}
		}()
	}
	close(start)
	wg.Wait()
	close(got)
	for r := range got {
		want := map[int]int{0: 1, 1: 2}[r[0]]
		if r[1] != want {
			t.Errorf("simultaneous batch: %s got inbox %d, want %d", map[int]string{0: "U", 1: "V"}[r[0]], r[1], want)
		}
	}
	if s := tracer.statements(u); s != 1 {
		t.Errorf("%d simultaneous requests of U ran %d count statements, want 1", nU, s)
	}
	if s := tracer.statements(v); s != 1 {
		t.Errorf("V ran %d count statements, want 1", s)
	}

	// 2) Hold U's next computation after it has read the old value.
	tracer.arm(u)
	oldU := make(chan int, 1)
	go func() { oldU <- countsInbox(t, router, u) }()
	select {
	case <-tracer.held:
	case <-time.After(30 * time.Second):
		t.Fatal("U's count statement never reached the hold")
	}
	inboxTask(uName) // a committed change of U: Inbox 1 → 2

	if got := countsInbox(t, router, u); got != 2 {
		t.Errorf("a request of U sent during the held computation = %d, want the new value 2 — it joined the old computation", got)
	}
	if got := countsInbox(t, router, v); got != 2 {
		t.Errorf("a request of V during the held computation of U = %d, want V's own 2", got)
	}
	close(tracer.release)
	if got := <-oldU; got != 1 {
		t.Errorf("the held request of U = %d, want the old value 1 it had read", got)
	}

	// 3) No further change: the next request of U must see the new value.
	if got := countsInbox(t, router, u); got != 2 {
		t.Errorf("next request of U after the late old result = %d, want 2 — the late result was kept", got)
	}
}
