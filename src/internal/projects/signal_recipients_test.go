package projects_test

// signal_recipients_test.go — sk-622.21, test matrix row 36, projects half
// (T017; US-11, "Personal signal matrix": "Member added / removed in
// a project", "Project created / changed / archived / unarchived").
//
// Subject: the personal signal the real project write paths publish after
// commit, observed on `skein.user.*.signal` through an in-process NATS server.
//
// Reference: spec — a member added or removed gets projects, lens, unreviewed
// and tasks; on a project created / changed the members get projects, on
// archive / unarchive also unreviewed; connected internal admins are skein-web's
// (T034) and are not asserted here. A user of another project gets nothing.
// Absence is judged after a SENTINEL whose message follows the action's on
// the same connection.

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"

	"skein-api/internal/projects"
	"skein-api/internal/signals"
	"skein-api/internal/testsupport"
)

// ---------------------------------------------------------------- NATS sink

type sigMsg struct {
	user  uuid.UUID
	areas []string
}

type sigSink struct {
	mu     sync.Mutex
	msgs   []sigMsg
	notify chan struct{}
}

func startSigSink(t *testing.T) *sigSink {
	t.Helper()
	opts := natstest.DefaultTestOptions
	opts.Port = -1
	opts.NoLog = true
	srv := natstest.RunServer(&opts)
	t.Cleanup(srv.Shutdown)
	pub, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("connect publisher: %v", err)
	}
	t.Cleanup(pub.Close)
	sub, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("connect subscriber: %v", err)
	}
	t.Cleanup(sub.Close)

	s := &sigSink{notify: make(chan struct{}, 1)}
	if _, err := sub.Subscribe(signals.SignalSubjectPattern, func(m *nats.Msg) {
		uid, err := uuid.Parse(strings.TrimSuffix(strings.TrimPrefix(m.Subject, "skein.user."), ".signal"))
		if err != nil {
			t.Errorf("subject %q carries no user id: %v", m.Subject, err)
			return
		}
		var body struct {
			Areas []string `json:"areas"`
		}
		if err := json.Unmarshal(m.Data, &body); err != nil {
			t.Errorf("signal body %s: %v", m.Data, err)
			return
		}
		s.mu.Lock()
		s.msgs = append(s.msgs, sigMsg{user: uid, areas: body.Areas})
		s.mu.Unlock()
		select {
		case s.notify <- struct{}{}:
		default:
		}
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := sub.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	signals.Configure(pub)
	t.Cleanup(func() { signals.Configure(nil) })
	return s
}

func (s *sigSink) mark() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.msgs)
}

// settle commits a sentinel and returns, per user, the union of the areas
// received since from, once the sentinel's own message is in.
func (s *sigSink) settle(t *testing.T, pool *pgxpool.Pool, from int) map[uuid.UUID]map[string]bool {
	t.Helper()
	sentinel := uuid.New()
	if err := signals.BeginTxFunc(context.Background(), pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		signals.Add(tx, sentinel, signals.AreaInbox)
		_, err := tx.Exec(context.Background(), `SELECT 1`)
		return err
	}); err != nil {
		t.Fatalf("sentinel transaction: %v", err)
	}
	deadline := time.After(10 * time.Second)
	for {
		s.mu.Lock()
		for i := from; i < len(s.msgs); i++ {
			if s.msgs[i].user != sentinel {
				continue
			}
			out := map[uuid.UUID]map[string]bool{}
			for _, m := range s.msgs[from:i] {
				if out[m.user] == nil {
					out[m.user] = map[string]bool{}
				}
				for _, a := range m.areas {
					out[m.user][a] = true
				}
			}
			s.mu.Unlock()
			return out
		}
		s.mu.Unlock()
		select {
		case <-s.notify:
		case <-deadline:
			t.Fatal("sentinel signal never arrived")
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// check asserts want ⊆ got for every addressee and nothing for silent.
func check(t *testing.T, got map[uuid.UUID]map[string]bool, names map[string]uuid.UUID, want map[string][]string, silent []string) {
	t.Helper()
	if len(want) == 0 {
		t.Fatal("row names no addressee — it would pass on a path that publishes nothing")
	}
	for who, areas := range want {
		for _, a := range areas {
			if !got[names[who]][a] {
				t.Errorf("%s: got areas %v, want %q among them", who, keys(got[names[who]]), a)
			}
		}
	}
	for _, who := range silent {
		if g := got[names[who]]; len(g) != 0 {
			t.Errorf("%s is not an addressee of this row, got areas %v", who, keys(g))
		}
	}
}

// world: owner and member belong to the project, newcomer is added or
// removed, outsider owns a project of their own.
type world struct {
	pool  *pgxpool.Pool
	store *projects.Store
	pid   uuid.UUID
	ids   map[string]uuid.UUID
	names map[string]string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	w := &world{pool: pool, store: projects.NewStore(pool), ids: map[string]uuid.UUID{}, names: map[string]string{}}
	for _, p := range []string{"owner", "member", "newcomer", "outsider"} {
		id, name := testsupport.SeedUser(t, pool, "")
		w.ids[p], w.names[p] = id, name
	}
	w.pid, _ = testsupport.SeedProject(t, pool, w.ids["owner"], "sigprj")
	testsupport.AddProjectMember(t, pool, w.pid, w.ids["member"], "member")
	testsupport.SeedProject(t, pool, w.ids["outsider"], "sigout")
	return w
}

func (w *world) actor() projects.Actor { return projects.HumanActor(w.ids["owner"], w.names["owner"]) }

func ptr[T any](v T) *T { return &v }

// TestSignalRecipients_MembershipAndProjectLifecycle — matrix row 36
// (projects).
func TestSignalRecipients_MembershipAndProjectLifecycle(t *testing.T) {
	membership := []string{"projects", "lens", "unreviewed", "tasks"}
	rows := []struct {
		name   string
		setup  func(t *testing.T, w *world)
		act    func(t *testing.T, w *world)
		want   map[string][]string
		silent []string
	}{
		{
			name: "Member added",
			setup: func(t *testing.T, w *world) {
				// INV-5 (T024): a project member is a participant of its space.
				testsupport.JoinProjectSpace(t, w.pool, w.pid, w.ids["newcomer"])
			},
			act: func(t *testing.T, w *world) {
				if _, err := w.store.UpsertMember(t.Context(), w.pid, w.ids["newcomer"], "member", w.ids["owner"], w.actor()); err != nil {
					t.Fatalf("UpsertMember: %v", err)
				}
			},
			want:   map[string][]string{"newcomer": membership},
			silent: []string{"owner", "member", "outsider"},
		},
		{
			name: "Member removed",
			setup: func(t *testing.T, w *world) {
				testsupport.AddProjectMember(t, w.pool, w.pid, w.ids["newcomer"], "member")
			},
			act: func(t *testing.T, w *world) {
				if err := w.store.RemoveMember(t.Context(), w.pid, w.ids["newcomer"], w.actor()); err != nil {
					t.Fatalf("RemoveMember: %v", err)
				}
			},
			want:   map[string][]string{"newcomer": membership},
			silent: []string{"owner", "member", "outsider"},
		},
		{
			name: "Project created",
			act: func(t *testing.T, w *world) {
				prefix := "sc" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
				// T024: a project is created in a space of its creator.
				space := testsupport.SeedSpace(t, w.pool, w.ids["newcomer"])
				p, err := w.store.CreateProject(t.Context(), projects.CreateProjectArgs{
					Name: "signal " + prefix, IDPrefix: prefix, DefaultIDMode: "counter",
					CreatorID: w.ids["newcomer"], CreatorRole: "user", CreatorName: w.names["newcomer"],
					SpaceID: space, Visibility: "members",
				})
				if err != nil {
					t.Fatalf("CreateProject: %v", err)
				}
				t.Cleanup(func() {
					c := context.Background()
					_, _ = w.pool.Exec(c, `DELETE FROM project_members WHERE project_id = $1`, p.ID)
					_, _ = w.pool.Exec(c, `DELETE FROM project_counters WHERE project_id = $1`, p.ID)
					_, _ = w.pool.Exec(c, `DELETE FROM projects WHERE id = $1`, p.ID)
				})
			},
			want:   map[string][]string{"newcomer": {"projects"}},
			silent: []string{"owner", "member", "outsider"},
		},
		{
			name: "Project changed",
			act: func(t *testing.T, w *world) {
				if _, err := w.store.UpdateProject(t.Context(), w.pid, projects.UpdateProjectArgs{Description: ptr("changed " + uuid.NewString())}, w.actor()); err != nil {
					t.Fatalf("UpdateProject: %v", err)
				}
			},
			want:   map[string][]string{"owner": {"projects"}, "member": {"projects"}},
			silent: []string{"newcomer", "outsider"},
		},
		{
			name: "Project changed (PATCH)",
			act: func(t *testing.T, w *world) {
				if _, err := w.store.PatchProject(t.Context(), w.pid, projects.PatchProjectArgs{Description: ptr("patched " + uuid.NewString())}, w.ids["owner"], "user", w.actor()); err != nil {
					t.Fatalf("PatchProject: %v", err)
				}
			},
			want:   map[string][]string{"owner": {"projects"}, "member": {"projects"}},
			silent: []string{"newcomer", "outsider"},
		},
		{
			name: "Project archived",
			act: func(t *testing.T, w *world) {
				if err := w.store.ArchiveProject(t.Context(), w.pid, w.actor()); err != nil {
					t.Fatalf("ArchiveProject: %v", err)
				}
			},
			want:   map[string][]string{"owner": {"projects", "unreviewed"}, "member": {"projects", "unreviewed"}},
			silent: []string{"newcomer", "outsider"},
		},
		{
			name: "Project unarchived",
			setup: func(t *testing.T, w *world) {
				if err := w.store.ArchiveProject(t.Context(), w.pid, w.actor()); err != nil {
					t.Fatalf("ArchiveProject: %v", err)
				}
			},
			act: func(t *testing.T, w *world) {
				if err := w.store.UnarchiveProject(t.Context(), w.pid, w.actor()); err != nil {
					t.Fatalf("UnarchiveProject: %v", err)
				}
			},
			want:   map[string][]string{"owner": {"projects", "unreviewed"}, "member": {"projects", "unreviewed"}},
			silent: []string{"newcomer", "outsider"},
		},
		{
			name: "Project archived (PATCH)",
			act: func(t *testing.T, w *world) {
				if _, err := w.store.PatchProject(t.Context(), w.pid, projects.PatchProjectArgs{Archived: ptr(true)}, w.ids["owner"], "user", w.actor()); err != nil {
					t.Fatalf("PatchProject archive: %v", err)
				}
			},
			want:   map[string][]string{"owner": {"projects", "unreviewed"}, "member": {"projects", "unreviewed"}},
			silent: []string{"newcomer", "outsider"},
		},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			w := newWorld(t)
			sink := startSigSink(t)
			if r.setup != nil {
				r.setup(t, w)
			}
			sink.settle(t, w.pool, sink.mark())
			from := sink.mark()
			r.act(t, w)
			check(t, sink.settle(t, w.pool, from), w.ids, r.want, r.silent)
		})
	}
}
