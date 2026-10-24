package handler

// prefs_signal_test.go — sk-622.21, test matrix row 35 (T016; US-11,
// "Personal signal matrix", row "Change of notification and review settings").
//
// Subject: the REST settings handlers PUT /me/review-prefs and
// PUT /me/notification-reasons, and the personal signal they publish after
// the settings are stored, observed on `skein.user.*.signal` through an
// in-process NATS server.
//
// Reference: spec — switching the review mechanic gives the user
// `unreviewed`; switching unwatch-on-close gives `watching`; only the user
// themself is addressed. Absence is judged after a SENTINEL transaction
// whose message arrives after the action's on the same connection.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

	"skein-api/internal/auth"
	"skein-api/internal/signals"
	"skein-api/internal/testsupport"
)

type prefsSigMsg struct {
	user  uuid.UUID
	areas []string
}

type prefsSigSink struct {
	mu     sync.Mutex
	msgs   []prefsSigMsg
	notify chan struct{}
}

func startPrefsSigSink(t *testing.T) *prefsSigSink {
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

	s := &prefsSigSink{notify: make(chan struct{}, 1)}
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
		s.msgs = append(s.msgs, prefsSigMsg{user: uid, areas: body.Areas})
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

func (s *prefsSigSink) mark() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.msgs)
}

// settle returns, per user, the areas received since from, once a sentinel
// published after them has arrived.
func (s *prefsSigSink) settle(t *testing.T, pool *pgxpool.Pool, from int) map[uuid.UUID][]string {
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
			sets := map[uuid.UUID]map[string]bool{}
			for _, m := range s.msgs[from:i] {
				if sets[m.user] == nil {
					sets[m.user] = map[string]bool{}
				}
				for _, a := range m.areas {
					sets[m.user][a] = true
				}
			}
			s.mu.Unlock()
			out := map[uuid.UUID][]string{}
			for u, set := range sets {
				for a := range set {
					out[u] = append(out[u], a)
				}
				sort.Strings(out[u])
			}
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

// putReasonPrefsRaw sends PUT /me/notification-reasons as caller.
func putReasonPrefsRaw(t *testing.T, pool *pgxpool.Pool, callerID uuid.UUID, raw string) {
	t.Helper()
	h := NewNotificationsHandler(pool)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/me/notification-reasons", bytes.NewBufferString(raw))
	req.Header.Set("Content-Type", "application/json")
	ctx := auth.WithUserID(req.Context(), callerID)
	ctx = auth.WithUserRole(ctx, "user")
	rec := httptest.NewRecorder()
	h.PutReasonPrefs(rec, req.WithContext(ctx))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /me/notification-reasons = %d: %s", rec.Code, rec.Body.String())
	}
}

// TestPrefsSignal_SettingsReachOnlyTheUser — matrix row 35.
func TestPrefsSignal_SettingsReachOnlyTheUser(t *testing.T) {
	cases := []struct {
		name string
		act  func(t *testing.T, pool *pgxpool.Pool, user uuid.UUID)
		want string
	}{
		{
			name: "review mechanic",
			act: func(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) {
				if code, body := putReviewPrefs(t, pool, user, `{"mechanicEnabled": false}`); code != http.StatusOK {
					t.Fatalf("PUT /me/review-prefs = %d: %v", code, body)
				}
			},
			want: "unreviewed",
		},
		{
			name: "auto-unwatch on close",
			act: func(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) {
				putReasonPrefsRaw(t, pool, user, `{"unwatchOnClose": false}`)
			},
			want: "watching",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := testsupport.NewTestPool(t)
			user, _ := testsupport.SeedUser(t, pool, "")
			other, _ := testsupport.SeedUser(t, pool, "")
			sink := startPrefsSigSink(t)

			from := sink.mark()
			c.act(t, pool, user)
			got := sink.settle(t, pool, from)

			found := false
			for _, a := range got[user] {
				if a == c.want {
					found = true
				}
			}
			if !found {
				t.Errorf("user got areas %v, want %q among them", got[user], c.want)
			}
			if len(got[other]) != 0 {
				t.Errorf("another user got areas %v for a settings change that is not theirs", got[other])
			}
			for u, areas := range got {
				if u != user {
					t.Errorf("user %s, who did not change settings, got %v", u, areas)
				}
			}
		})
	}
}

// TestPrefsSignal_RepeatOfValueInEffectIsSilent — found in review: a settings request that leaves the switch at the value already in
// effect changes no counter, so it gives no signal. "In effect" covers both
// the stored value and the factory value of a user with no settings row
// (mechanic ON, unwatch-on-close ON).
func TestPrefsSignal_RepeatOfValueInEffectIsSilent(t *testing.T) {
	review := func(raw string) func(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) {
		return func(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) {
			if code, body := putReviewPrefs(t, pool, user, raw); code != http.StatusOK {
				t.Fatalf("PUT /me/review-prefs %s = %d: %v", raw, code, body)
			}
		}
	}
	reasons := func(raw string) func(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) {
		return func(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) {
			putReasonPrefsRaw(t, pool, user, raw)
		}
	}
	cases := []struct {
		name  string
		setup func(t *testing.T, pool *pgxpool.Pool, user uuid.UUID)
		act   func(t *testing.T, pool *pgxpool.Pool, user uuid.UUID)
	}{
		{name: "review mechanic: factory value without a row", act: review(`{"mechanicEnabled": true}`)},
		{name: "review mechanic: repeat of the saved value", setup: review(`{"mechanicEnabled": false}`), act: review(`{"mechanicEnabled": false}`)},
		{name: "auto-unwatch: factory value without a row", act: reasons(`{"unwatchOnClose": true}`)},
		{name: "auto-unwatch: repeat of the saved value", setup: reasons(`{"unwatchOnClose": false}`), act: reasons(`{"unwatchOnClose": false}`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := testsupport.NewTestPool(t)
			user, _ := testsupport.SeedUser(t, pool, "")
			sink := startPrefsSigSink(t)
			if c.setup != nil {
				c.setup(t, pool, user)
				sink.settle(t, pool, sink.mark())
			}
			from := sink.mark()
			c.act(t, pool, user)
			got := sink.settle(t, pool, from)
			if len(got[user]) != 0 {
				t.Errorf("a request that changes nothing gave the user areas %v, want none", got[user])
			}
		})
	}
}
