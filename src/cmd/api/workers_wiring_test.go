package main

// workers_wiring_test.go — sk-622.21, test matrix rows 40 and 40a (T019,
// T020; US-8, D9, decision R16): the background notification workers are
// started by the function main calls, with values from config.Load, at the
// product cadence.
//
// Subject: startNotificationWorkers — the one wiring main uses — over a real
// database, with cfg from config.Load() under non-default
// NOTIFICATIONS_RETENTION_DAYS=30 / WATCHING_FEED_WINDOW_DAYS=7. No interval
// and no retention literal is handed to it: both come from inside it and from
// the loaded configuration.
//
// Reference: spec US-8 (kept NOTIFICATIONS_RETENTION_DAYS days, deleted «not
// less often than once an hour»), plan T019 (the first sweep at start-up),
// INV-15 and T025 (R2 removed the R1 reconciliation and kept its
// re-resolution of issues.assignee_user_id as a background worker: an issue
// whose assignee_user_id disagrees with its assignee text is re-resolved).
//
// Row 40 runs on live clocks: the first runs are awaited through the
// workers' own completion signals (the sweep-duration histogram's sample
// count, the assignee re-resolution's last-pass timestamp), never by a fixed
// pause.
// Row 40a runs in a testing/synctest bubble so an hour passes without
// waiting one.

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"skein-api/internal/config"
	"skein-api/internal/testsupport"
)

// setWorkersEnv sets the environment config.Load needs, with the two
// non-default notification values of rows 40 / 40a.
func setWorkersEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", os.Getenv(testsupport.TestDatabaseURL))
	t.Setenv("SKEIN_INTERNAL_SECRET", wiringInternalSecret)
	t.Setenv("SKEIN_BCRYPT_COST", "4")
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("SKEIN_AGENT_PUBLIC_URL", "http://skein-wiring.test")
	t.Setenv("NOTIFICATIONS_RETENTION_DAYS", "30")
	t.Setenv("WATCHING_FEED_WINDOW_DAYS", "7")
}

// workersFixture is a user, a project and an issue to hang rows on.
type workersFixture struct {
	user    uuid.UUID
	project uuid.UUID
	issue   string
}

func newWorkersFixture(t *testing.T, pool *pgxpool.Pool) workersFixture {
	t.Helper()
	user, _ := testsupport.SeedUser(t, pool, "")
	project, prefix := testsupport.SeedProject(t, pool, user, "wkw")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE project_id = $1`, project)
	})
	issue := testsupport.SeedIssue(t, pool, project, user, prefix+"-1", "workers wiring", "open", 2)
	return workersFixture{user: user, project: project, issue: issue}
}

// seedEvent writes one issue event of the fixture.
func (f workersFixture) seedEvent(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id, actor, actor_user_id, actor_type, payload)
VALUES (now(), 'issue.updated', 'issue', $1, $2, 'tester', $3, 'human', '{}'::jsonb)
RETURNING id`, f.issue, f.project, f.user).Scan(&id); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	return id
}

// seedFeedRow writes a non-mention feed notification created at `at`.
func seedFeedRow(t *testing.T, pool *pgxpool.Pool, user uuid.UUID, eventID int64, project uuid.UUID, at time.Time) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `
INSERT INTO notifications (user_id, event_id, type, project_id, created_at, feed_channel, card_resolved)
VALUES ($1, $2, 'issue.updated', $3, $4, true, true)
RETURNING id`, user, eventID, project, at).Scan(&id); err != nil {
		t.Fatalf("seed notification: %v", err)
	}
	return id
}

func notificationExists(t *testing.T, pool *pgxpool.Pool, id int64) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM notifications WHERE id = $1)`, id).Scan(&ok); err != nil {
		t.Fatalf("read notification %d: %v", id, err)
	}
	return ok
}

// workerSignals reads the two completion signals of the workers: how many
// retention sweeps have finished, and when the last assignee re-resolution
// pass did.
func workerSignals(t *testing.T) (sweeps uint64, lastPass float64) {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, f := range families {
		switch f.GetName() {
		case "skein_notifications_retention_sweep_duration_seconds":
			for _, m := range f.GetMetric() {
				sweeps += m.GetHistogram().GetSampleCount()
			}
		case "skein_issues_assignee_resync_last_pass_timestamp_seconds":
			for _, m := range f.GetMetric() {
				lastPass = m.GetGauge().GetValue()
			}
		}
	}
	return sweeps, lastPass
}

// TestNotificationWorkers_StartedFromLoadedConfig is row 40.
func TestNotificationWorkers_StartedFromLoadedConfig(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	setWorkersEnv(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	f := newWorkersFixture(t, pool)
	now := time.Now().UTC()
	old := seedFeedRow(t, pool, f.user, f.seedEvent(t, pool), f.project, now.Add(-40*24*time.Hour))
	young := seedFeedRow(t, pool, f.user, f.seedEvent(t, pool), f.project, now.Add(-20*24*time.Hour))
	// A stale assignee_user_id — the state the 000060 race leaves (an
	// assignment that resolved against an older users directory): the
	// assignee text names the fixture's user, the cached id is NULL. Written
	// with the resolving trigger off, as that race leaves it.
	staleTx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for _, q := range []string{
		`ALTER TABLE issues DISABLE TRIGGER issues_resolve_assignee_user_id`,
		`UPDATE issues SET assignee = (SELECT username FROM users WHERE id = $2), assignee_user_id = NULL WHERE id = $1`,
		`ALTER TABLE issues ENABLE TRIGGER issues_resolve_assignee_user_id`,
	} {
		var args []any
		if strings.Contains(q, "$1") {
			args = []any{f.issue, f.user}
		}
		if _, err := staleTx.Exec(context.Background(), q, args...); err != nil {
			_ = staleTx.Rollback(context.Background())
			t.Fatalf("seed stale assignee: %v", err)
		}
	}
	if err := staleTx.Commit(context.Background()); err != nil {
		t.Fatalf("commit stale assignee: %v", err)
	}

	sweepsBefore, passBefore := workerSignals(t)
	ctx, cancel := context.WithCancel(context.Background())
	done, err := startNotificationWorkers(ctx, pool, cfg)
	if err != nil {
		cancel()
		t.Fatalf("startNotificationWorkers: %v", err)
	}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("notification workers did not stop after their context was cancelled")
		}
	}
	t.Cleanup(stop)

	// Wait for the first sweep and the first pass to COMPLETE — the
	// workers' own signals — not for a fixed time.
	deadline := time.After(60 * time.Second)
	for {
		sweeps, pass := workerSignals(t)
		if sweeps > sweepsBefore && pass != passBefore {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("no completed first run after start: sweeps %d→%d, last pass %v→%v — a worker is not started by startNotificationWorkers",
				sweepsBefore, sweeps, passBefore, pass)
		case <-time.After(20 * time.Millisecond):
		}
	}
	stop()

	if notificationExists(t, pool, old) {
		t.Error("a non-mention 40 days old is still there: the retention is not the loaded 30 days (90 would keep it)")
	}
	if !notificationExists(t, pool, young) {
		t.Error("a non-mention 20 days old was deleted: the retention is shorter than the loaded 30 days")
	}
	var resolved *uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT assignee_user_id FROM issues WHERE id = $1`, f.issue).Scan(&resolved); err != nil {
		t.Fatalf("read assignee_user_id: %v", err)
	}
	if resolved == nil || *resolved != f.user {
		t.Errorf("assignee_user_id = %v after start, want %s: the assignee re-resolution did not run", resolved, f.user)
	}
}

// TestNotificationWorkers_RetentionSweepsAtLeastHourly is row 40a.
func TestNotificationWorkers_RetentionSweepsAtLeastHourly(t *testing.T) {
	outer := testsupport.NewTestPool(t)
	setWorkersEnv(t)
	f := newWorkersFixture(t, outer)
	eventID := f.seedEvent(t, outer)
	url := os.Getenv(testsupport.TestDatabaseURL)

	synctest.Test(t, func(t *testing.T) {
		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("config.Load: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		pool, err := pgxpool.New(ctx, url)
		if err != nil {
			cancel()
			t.Fatalf("pool in the bubble: %v", err)
		}
		done, err := startNotificationWorkers(ctx, pool, cfg)
		if err != nil {
			cancel()
			pool.Close()
			t.Fatalf("startNotificationWorkers: %v", err)
		}
		// Every goroutine of the bubble must be gone before it ends: stop
		// the workers, then the pool, whatever the outcome below.
		defer func() {
			cancel()
			<-done
			pool.Close()
		}()
		// The start-up runs are over and the workers sleep on their timers.
		synctest.Wait()

		// Older than the window by the bubble clock (2000-01-01 − 40 days)
		// and, being in 1999, by the database clock too.
		id := seedFeedRow(t, pool, f.user, eventID, f.project, time.Now().Add(-40*24*time.Hour))
		if !notificationExists(t, pool, id) {
			t.Fatal("seeded row is missing before the hour passes")
		}

		time.Sleep(time.Hour)
		synctest.Wait()
		if notificationExists(t, pool, id) {
			t.Error("an hour after start the expired row is still there: the sweep does not repeat at least hourly")
		}
	})
}

// TestEmailWorkers_StartedByTheLoadedFlag is matrix row 79 (sk-674.18
// T022/T040). Reference — K-9 and the row: the mail
// worker runs while SKEIN_EMAIL_ENABLED=true and not otherwise; the cleanup
// runs always. Subject: startEmailWorkers — the one wiring main uses — over
// a real database and a configuration loaded from the environment. Seen in
// the data: a seeded pending email is taken into a lease (attempts, the
// lease generation, grows) by the started worker with the flag on, and is
// not with it off — judged after the cleanup, started beside it, has done
// its first pass (an aged final email is gone) and two of the worker's poll
// intervals more. The SMTP relay is a closed local port: the attempt fails
// as temporary, nothing is sent.
//
// The invitation-expiry half (T040): with either value of the flag the
// started expiry worker turns a seeded overdue invitation `expired` — not a
// direct call of the expiry, not the presence of its type.
func TestEmailWorkers_StartedByTheLoadedFlag(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM outgoing_emails`); err != nil {
		t.Fatalf("resetting the mail queue: %v", err)
	}
	seed := func(t *testing.T, user uuid.UUID, state string, finalizedAt *time.Time) uuid.UUID {
		t.Helper()
		var outcome *string
		if state == "final" {
			o := "success"
			outcome = &o
		}
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `
INSERT INTO outgoing_emails (kind, recipient, lang, user_id, state, outcome, finalized_at,
                             attempts, deadline_at, created_at)
VALUES ('password_reset', 'wire' || substr(md5(random()::text), 1, 8) || '@test.local', 'en', $1, $2, $3, $4, 0,
        now() + interval '1 day', now() - interval '41 days')
RETURNING id`, user, state, outcome, finalizedAt).Scan(&id); err != nil {
			t.Fatalf("seeding a %s email: %v", state, err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM outgoing_emails WHERE id = $1`, id) })
		return id
	}
	attempts := func(t *testing.T, id uuid.UUID) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT attempts FROM outgoing_emails WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("reading the email: %v", err)
		}
		return n
	}
	gone := func(id uuid.UUID) bool {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM outgoing_emails WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 0
	}
	start := func(t *testing.T, enabled bool) {
		t.Helper()
		setWorkersEnv(t)
		t.Setenv("SKEIN_EMAIL_ENABLED", strconv.FormatBool(enabled))
		if enabled {
			for k, v := range map[string]string{
				"SKEIN_APP_BASE_URL": "https://skein.example.com", "SKEIN_SMTP_HOST": "127.0.0.1",
				"SKEIN_SMTP_PORT": "1", "SKEIN_SMTP_USERNAME": "skein", "SKEIN_SMTP_PASSWORD": "smtp-password",
				"SKEIN_SMTP_FROM": "skein@example.com", "SKEIN_SMTP_TLS_MODE": "implicit",
			} {
				t.Setenv(k, v)
			}
		}
		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("config.Load: %v", err)
		}
		wctx, cancel := context.WithCancel(context.Background())
		done, err := startEmailWorkers(wctx, pool, cfg)
		if err != nil {
			cancel()
			t.Fatalf("startEmailWorkers: %v", err)
		}
		t.Cleanup(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Error("the email workers did not stop after their context was cancelled")
			}
		})
	}
	user, _ := testsupport.SeedUser(t, pool, "user")
	aged := time.Now().UTC().Add(-40 * 24 * time.Hour)

	t.Run("enabled: the worker leases the email", func(t *testing.T) {
		pending := seed(t, user, "pending", nil)
		start(t, true)
		for deadline := time.Now().Add(30 * time.Second); attempts(t, pending) == 0; time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("the pending email was never leased: the mail worker is not started with SKEIN_EMAIL_ENABLED=true")
			}
		}
	})

	t.Run("disabled: no worker leases it, the cleanup still runs", func(t *testing.T) {
		pending := seed(t, user, "pending", nil)
		old := seed(t, user, "final", &aged)
		start(t, false)
		for deadline := time.Now().Add(30 * time.Second); !gone(old); time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("the aged final email was never deleted: the cleanup is not started")
			}
		}
		// The cleanup's first pass is done; a started worker would have
		// polled by now and polls again every 5 s — watch two intervals.
		for until := time.Now().Add(11 * time.Second); time.Now().Before(until); time.Sleep(200 * time.Millisecond) {
			if attempts(t, pending) != 0 {
				t.Fatal("the pending email was leased with SKEIN_EMAIL_ENABLED=false")
			}
		}
	})

	// The invitation expiry worker (T040) runs whatever the mail flag: an
	// unanswered invitation gets its outcome even on an installation without
	// mail. Seen in the data: a seeded pending invitation an hour past its
	// expiry becomes `expired`, with its resolution time stamped.
	for _, enabled := range []bool{true, false} {
		t.Run("invitation expiry, SKEIN_EMAIL_ENABLED="+strconv.FormatBool(enabled), func(t *testing.T) {
			owner, _ := testsupport.SeedUser(t, pool, "user")
			space := testsupport.SeedSpace(t, pool, owner)
			var inv uuid.UUID
			if err := pool.QueryRow(ctx, `
INSERT INTO space_invitations (space_id, email, invited_by, lang, expires_at, created_at)
VALUES ($1, 'wire' || substr(md5(random()::text), 1, 8) || '@test.local', $2, 'en',
        now() - interval '1 hour', now() - interval '8 days')
RETURNING id`, space, owner).Scan(&inv); err != nil {
				t.Fatalf("seeding an overdue invitation: %v", err)
			}
			t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM space_invitations WHERE id = $1`, inv) })
			start(t, enabled)
			for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
				var status string
				var resolved bool
				if err := pool.QueryRow(ctx, `SELECT status, resolved_at IS NOT NULL FROM space_invitations WHERE id = $1`,
					inv).Scan(&status, &resolved); err != nil {
					t.Fatal(err)
				}
				if status == "expired" && resolved {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the overdue invitation is still %q: the expiry worker is not started with SKEIN_EMAIL_ENABLED=%v",
						status, enabled)
				}
			}
		})
	}
}
