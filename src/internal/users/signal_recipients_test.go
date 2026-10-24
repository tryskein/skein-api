package users_test

// signal_recipients_test.go — sk-622.21, test matrix row 36, users half
// (T017; US-11, "Personal signal matrix": "Global role change,
// deactivation, reactivation" and "User creation, email change on
// SSO sign-in").
//
// Subject: the personal signal of the user write paths (users.Store) and of
// SSO provisioning (auth.Provisioner), observed on `skein.user.*.signal`
// through an in-process NATS server.
//
// Reference: spec — role change, deactivation and reactivation give the user
// projects, lens, unreviewed and tasks; creating a user or changing their
// e-mail through SSO reaches the users whose assignee resolution changed on
// their issues before or after — the assignees and the authors of the issues
// whose assignee text names the affected handle — inbox, lens and tasks,
// and nobody else. Absence is judged after a SENTINEL whose message follows
// the action's on the same connection.

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

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/signals"
	"skein-api/internal/testsupport"
	"skein-api/internal/users"
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

// world: target is the user acted on, authorA / authorB file issues,
// bystander authored an issue assigned to someone unrelated.
type world struct {
	pool  *pgxpool.Pool
	store *users.Store
	pid   uuid.UUID
	pfx   string
	n     int
	ids   map[string]uuid.UUID
	names map[string]string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	w := &world{
		pool:  pool,
		store: users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil),
		ids:   map[string]uuid.UUID{}, names: map[string]string{},
	}
	for _, p := range []string{"admin", "target", "authorA", "authorB", "bystander", "other"} {
		role := ""
		if p == "admin" {
			role = "admin"
		}
		id, name := testsupport.SeedUser(t, pool, role)
		w.ids[p], w.names[p] = id, name
	}
	w.pid, w.pfx = testsupport.SeedProject(t, pool, w.ids["authorA"], "sigusr")
	return w
}

func (w *world) actor() users.Actor { return users.HumanActor(w.ids["admin"], w.names["admin"]) }

// issue files an issue by author whose assignee TEXT is handle.
func (w *world) issue(t *testing.T, author, handle string) {
	t.Helper()
	w.n++
	id := testsupport.SeedIssue(t, w.pool, w.pid, w.ids[author], w.pfx+"-"+itoa(w.n), "signal", "open", 2)
	if _, err := w.pool.Exec(t.Context(), `UPDATE issues SET assignee = $2 WHERE id = $1`, id, handle); err != nil {
		t.Fatalf("set assignee of %s: %v", id, err)
	}
}

func itoa(n int) string {
	b := []byte{}
	for n > 0 || len(b) == 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestSignalRecipients_UsersAndSSO — matrix row 36 (users).
func TestSignalRecipients_UsersAndSSO(t *testing.T) {
	access := []string{"projects", "lens", "unreviewed", "tasks"}
	resolution := []string{"inbox", "lens", "tasks"}
	type row struct {
		name   string
		setup  func(t *testing.T, w *world)
		act    func(t *testing.T, w *world)
		want   func(w *world) map[string][]string
		silent []string
	}
	rows := []row{
		{
			name: "Global role change",
			act: func(t *testing.T, w *world) {
				if _, err := w.store.UpdateUser(t.Context(), w.ids["target"], users.UpdateUserInput{Role: ptr("admin")}, w.actor()); err != nil {
					t.Fatalf("UpdateUser: %v", err)
				}
			},
			want:   func(*world) map[string][]string { return map[string][]string{"target": access} },
			silent: []string{"admin", "authorA", "bystander"},
		},
		{
			name: "Deactivation",
			act: func(t *testing.T, w *world) {
				if err := w.store.Deactivate(t.Context(), w.ids["target"], w.actor()); err != nil {
					t.Fatalf("Deactivate: %v", err)
				}
			},
			want:   func(*world) map[string][]string { return map[string][]string{"target": access} },
			silent: []string{"admin", "authorA", "bystander"},
		},
		{
			name: "Reactivation",
			setup: func(t *testing.T, w *world) {
				if err := w.store.Deactivate(t.Context(), w.ids["target"], w.actor()); err != nil {
					t.Fatalf("Deactivate: %v", err)
				}
			},
			act: func(t *testing.T, w *world) {
				if err := w.store.Reactivate(t.Context(), w.ids["target"], w.actor()); err != nil {
					t.Fatalf("Reactivate: %v", err)
				}
			},
			want:   func(*world) map[string][]string { return map[string][]string{"target": access} },
			silent: []string{"admin", "authorA", "bystander"},
		},
		{
			// authorA filed an issue for a person who has no account yet;
			// bystander's issue is assigned to an existing, unrelated user.
			// The account is created with access to the issue's project —
			// the admin puts it into the project's space, whose projects it
			// reaches (visibility space): a personal signal goes to the
			// assignee the issue is DELIVERED to (INV-30, sk-674.18 review
			// api/8 #11); an account without access is no addressee
			// (issues/signal_recipients_test.go#TestSignalRecipients_OutsiderNamedByTextGetsNothing).
			name: "User creation",
			setup: func(t *testing.T, w *world) {
				w.names["created"] = "nu" + strings.ReplaceAll(uuid.NewString()[:12], "-", "")
				w.issue(t, "authorA", w.names["created"])
				w.issue(t, "bystander", w.names["other"])
				if _, err := w.pool.Exec(t.Context(), `UPDATE projects SET visibility = 'space' WHERE id = $1`, w.pid); err != nil {
					t.Fatalf("open the project to its space: %v", err)
				}
			},
			act: func(t *testing.T, w *world) {
				var space string
				if err := w.pool.QueryRow(t.Context(), `
SELECT s.short_id FROM projects p JOIN spaces s ON s.id = p.space_id WHERE p.id = $1`, w.pid).Scan(&space); err != nil {
					t.Fatalf("read the project's space: %v", err)
				}
				dto, err := w.store.CreateUser(t.Context(), users.CreateUserInput{
					Email: w.names["created"] + "@test.local", Username: w.names["created"],
					Name: "created", Password: "a-long-enough-password-1", Role: "user",
					SpaceIDs: []string{space},
				}, w.actor())
				if err != nil {
					t.Fatalf("CreateUser: %v", err)
				}
				w.ids["created"] = dto.ID
				t.Cleanup(func() { _, _ = w.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, dto.ID) })
			},
			want: func(*world) map[string][]string {
				return map[string][]string{"created": resolution, "authorA": resolution}
			},
			silent: []string{"bystander", "other", "admin"},
		},
		{
			// target is an SSO account; authorA's issue names target's old
			// address, authorB's the new one; bystander's issue is assigned
			// to an unrelated user.
			name: "Email change on SSO sign-in",
			setup: func(t *testing.T, w *world) {
				w.names["sub"] = "sub-" + uuid.NewString()
				w.names["old"] = "old" + strings.ReplaceAll(uuid.NewString()[:8], "-", "") + "@test.local"
				w.names["new"] = "new" + strings.ReplaceAll(uuid.NewString()[:8], "-", "") + "@test.local"
				if _, err := w.pool.Exec(t.Context(), `UPDATE users SET keycloak_sub = $2, email = $3 WHERE id = $1`,
					w.ids["target"], w.names["sub"], w.names["old"]); err != nil {
					t.Fatalf("bind target to SSO: %v", err)
				}
				w.issue(t, "authorA", w.names["old"])
				w.issue(t, "authorB", w.names["new"])
				w.issue(t, "bystander", w.names["other"])
			},
			act: func(t *testing.T, w *world) {
				p := auth.NewProvisioner(w.pool, false, "user", "")
				if _, err := p.Provision(t.Context(), auth.ProvisionInput{Sub: w.names["sub"], Email: w.names["new"], Username: w.names["target"]}); err != nil {
					t.Fatalf("Provision: %v", err)
				}
			},
			want: func(*world) map[string][]string {
				return map[string][]string{"target": resolution, "authorA": resolution, "authorB": resolution}
			},
			silent: []string{"bystander", "other", "admin"},
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
			check(t, sink.settle(t, w.pool, from), w.ids, r.want(w), r.silent)
		})
	}
}

func ptr[T any](v T) *T { return &v }
