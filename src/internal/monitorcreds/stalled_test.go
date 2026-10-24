package monitorcreds

// stalled_test.go — sk-579 matrix rows 12–15 (US-5, INV-5, INV-7, INV-8):
// the stalled-monitor selection and the show-mark claim. Time is moved by
// shifting the stamps in the database, never by waiting.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/agents"
	"skein-api/internal/testsupport"
)

const stallThreshold = 3 * time.Minute

type stallFixture struct {
	pool    *pgxpool.Pool
	store   *Store
	owner   uuid.UUID
	project uuid.UUID
	root    string
	key     uuid.UUID
}

func newStallFixture(t *testing.T) *stallFixture {
	t.Helper()
	pool := newTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	project, prefix := testsupport.SeedProject(t, pool, owner, "mst")
	root := testsupport.SeedIssue(t, pool, project, owner, prefix+"-1", "stall", "open", 2)
	key := seedAPIKey(t, pool, owner)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM agents WHERE user_id = $1`, owner)
	})
	return &stallFixture{pool: pool, store: New(pool, 6*time.Hour), owner: owner, project: project, root: root, key: key}
}

func (f *stallFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// mint issues a monitor through key with the issuing session, then backdates
// it: mintedAgo moves created_at, successAgo sets the last successful poll
// (negative = never polled).
func (f *stallFixture) mint(t *testing.T, key uuid.UUID, session string, mintedAgo, successAgo time.Duration) uuid.UUID {
	t.Helper()
	_, cred, err := f.store.Mint(context.Background(), MintParams{
		UserID: f.owner, MintedByAPIKeyID: key, Audience: "notifications:poll", Capability: "read",
		ProjectID: f.project, ScopeIssueID: f.root, EventTypes: []string{"comment.created"},
		IssuedSessionID: session, TokenFilePath: "/tmp/skein-mon-stall.token",
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	f.exec(t, `UPDATE monitor_credentials SET created_at = now() - $2::interval WHERE id = $1`, cred.ID, mintedAgo)
	if successAgo >= 0 {
		f.exec(t, `UPDATE monitor_credentials SET last_poll_succeeded_at = now() - $2::interval WHERE id = $1`, cred.ID, successAgo)
	}
	return cred.ID
}

func (f *stallFixture) stalled(t *testing.T, key uuid.UUID, recipient string, threshold time.Duration) StalledResult {
	t.Helper()
	res, err := f.store.Stalled(context.Background(), StalledQuery{
		APIKeyID: key, UserID: f.owner, RecipientSession: recipient,
		Threshold: threshold, Limit: 10,
	})
	if err != nil {
		t.Fatalf("Stalled: %v", err)
	}
	return res
}

func ids(res StalledResult) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(res.Monitors))
	for _, m := range res.Monitors {
		out = append(out, m.ID)
	}
	return out
}

func has(res StalledResult, id uuid.UUID) (StalledMonitor, bool) {
	for _, m := range res.Monitors {
		if m.ID == id {
			return m, true
		}
	}
	return StalledMonitor{}, false
}

// TestStalled_Predicate — row 12 (INV-5, SC-009, R5): each case switches off
// exactly one conjunct of a base candidate.
func TestStalled_Predicate(t *testing.T) {
	const tenMin = 10 * time.Minute
	notReturned := []struct {
		name  string
		setup func(t *testing.T, f *stallFixture, id uuid.UUID)
	}{
		{"revoked", func(t *testing.T, f *stallFixture, id uuid.UUID) {
			f.exec(t, `UPDATE monitor_credentials SET revoked_at = now() WHERE id = $1`, id)
		}},
		{"expires_at passed", func(t *testing.T, f *stallFixture, id uuid.UUID) {
			f.exec(t, `UPDATE monitor_credentials SET expires_at = now() - interval '1 second' WHERE id = $1`, id)
		}},
		{"absolute_deadline passed", func(t *testing.T, f *stallFixture, id uuid.UUID) {
			f.exec(t, `UPDATE monitor_credentials SET absolute_deadline = now() - interval '1 second' WHERE id = $1`, id)
		}},
		{"minting key revoked", func(t *testing.T, f *stallFixture, id uuid.UUID) {
			f.exec(t, `UPDATE api_keys SET revoked_at = now() WHERE id = $1`, f.key)
		}},
		{"owner deactivated", func(t *testing.T, f *stallFixture, id uuid.UUID) {
			f.exec(t, `UPDATE users SET deactivated_at = now() WHERE id = $1`, f.owner)
		}},
		{"root closed", func(t *testing.T, f *stallFixture, id uuid.UUID) {
			f.exec(t, `UPDATE issues SET status = 'closed', closed_at = now() WHERE id = $1`, f.root)
		}},
		{"root_closed recorded (root reopened)", func(t *testing.T, f *stallFixture, id uuid.UUID) {
			f.exec(t, `UPDATE monitor_credentials SET root_closed_at = now() - interval '1 minute' WHERE id = $1`, id)
		}},
		{"owner removed from members, not admin", func(t *testing.T, f *stallFixture, id uuid.UUID) {
			f.exec(t, `DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`, f.project, f.owner)
		}},
		{"issuing session unknown (NULL)", func(t *testing.T, f *stallFixture, id uuid.UUID) {
			f.exec(t, `UPDATE monitor_credentials SET issued_session_id = NULL WHERE id = $1`, id)
		}},
		{"success 1 min ago", func(t *testing.T, f *stallFixture, id uuid.UUID) {
			f.exec(t, `UPDATE monitor_credentials SET last_poll_succeeded_at = now() - interval '1 minute' WHERE id = $1`, id)
		}},
		{"never polled, minted 1 min ago", func(t *testing.T, f *stallFixture, id uuid.UUID) {
			f.exec(t, `UPDATE monitor_credentials SET last_poll_succeeded_at = NULL, created_at = now() - interval '1 minute' WHERE id = $1`, id)
		}},
	}
	t.Run("base candidate is returned", func(t *testing.T) {
		f := newStallFixture(t)
		id := f.mint(t, f.key, "s-mint", tenMin, tenMin)
		if _, ok := has(f.stalled(t, f.key, "s-mint", stallThreshold), id); !ok {
			t.Fatalf("the base candidate was not returned")
		}
	})
	for _, tc := range notReturned {
		t.Run("not returned: "+tc.name, func(t *testing.T) {
			f := newStallFixture(t)
			id := f.mint(t, f.key, "s-mint", tenMin, tenMin)
			tc.setup(t, f, id)
			if _, ok := has(f.stalled(t, f.key, "s-mint", stallThreshold), id); ok {
				t.Fatalf("returned although %s", tc.name)
			}
		})
	}

	t.Run("returned: never polled, minted 10 min ago, with no success time", func(t *testing.T) {
		f := newStallFixture(t)
		id := f.mint(t, f.key, "s-mint", tenMin, -1)
		m, ok := has(f.stalled(t, f.key, "s-mint", stallThreshold), id)
		if !ok || m.LastPollSucceededAt != nil {
			t.Fatalf("got (%+v, %v), want the monitor with no success time", m, ok)
		}
	})
	t.Run("returned: fresh last_used_at, old success", func(t *testing.T) {
		f := newStallFixture(t)
		id := f.mint(t, f.key, "s-mint", tenMin, tenMin)
		f.exec(t, `UPDATE monitor_credentials SET last_used_at = now() WHERE id = $1`, id)
		if _, ok := has(f.stalled(t, f.key, "s-mint", stallThreshold), id); !ok {
			t.Fatalf("a fresh last_used_at hid the monitor — stalled must follow the successful poll")
		}
	})
	t.Run("returned: admin bypass", func(t *testing.T) {
		f := newStallFixture(t)
		id := f.mint(t, f.key, "s-mint", tenMin, tenMin)
		f.exec(t, `DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`, f.project, f.owner)
		// The owner is an admin in users: the access relation reads the
		// role there (sk-674.18 T010), not from a flag of the query.
		f.exec(t, `UPDATE users SET role = 'admin' WHERE id = $1`, f.owner)
		if _, ok := has(f.stalled(t, f.key, "s-mint", stallThreshold), id); !ok {
			t.Fatalf("the admin bypass did not make the project visible")
		}
	})
	t.Run("returned: visibility restored", func(t *testing.T) {
		f := newStallFixture(t)
		id := f.mint(t, f.key, "s-mint", tenMin, tenMin)
		f.exec(t, `DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`, f.project, f.owner)
		testsupport.AddProjectMember(t, f.pool, f.project, f.owner, "member")
		if _, ok := has(f.stalled(t, f.key, "s-mint", stallThreshold), id); !ok {
			t.Fatalf("not returned after visibility came back")
		}
	})
	t.Run("the threshold is a parameter", func(t *testing.T) {
		f := newStallFixture(t)
		id := f.mint(t, f.key, "s-mint", tenMin, 3*time.Minute+30*time.Second)
		if _, ok := has(f.stalled(t, f.key, "s-mint", 3*time.Minute), id); !ok {
			t.Errorf("success 3.5 min ago is not stalled at threshold 3m")
		}
		if _, ok := has(f.stalled(t, f.key, "s-mint", 4*time.Minute), id); ok {
			t.Errorf("success 3.5 min ago is stalled at threshold 4m")
		}
	})
}

// TestStalled_Recipients — row 13 (INV-8, R6, US-5 "Recipients").
func TestStalled_Recipients(t *testing.T) {
	const tenMin = 10 * time.Minute
	seen := func(t *testing.T, f *stallFixture, session string, ago time.Duration) {
		f.exec(t, `
INSERT INTO agents (user_id, session_id, first_seen_at, last_seen_at)
VALUES ($1, $2, now() - $3::interval, now() - $3::interval)
ON CONFLICT (user_id, session_id) DO UPDATE SET last_seen_at = EXCLUDED.last_seen_at`, f.owner, session, ago)
	}

	t.Run("the issuing session gets mine=true", func(t *testing.T) {
		f := newStallFixture(t)
		id := f.mint(t, f.key, "s-mint", tenMin, tenMin)
		seen(t, f, "s-mint", 0)
		if m, ok := has(f.stalled(t, f.key, "s-mint", stallThreshold), id); !ok || !m.Mine {
			t.Fatalf("got (%+v, %v), want mine=true", m, ok)
		}
	})
	t.Run("another session of the key: only after the issuing session went quiet", func(t *testing.T) {
		f := newStallFixture(t)
		id := f.mint(t, f.key, "s-mint", tenMin, tenMin)
		seen(t, f, "s-mint", 90*time.Second)
		if _, ok := has(f.stalled(t, f.key, "s-other", stallThreshold), id); ok {
			t.Fatalf("shown to another session while the issuing session was seen 90s ago (threshold 3m)")
		}
		// The activity clock is written at most once per granularity, so
		// «quiet longer than the threshold» needs the stamp older than
		// threshold + granularity: 3m30s is past the 3m threshold but
		// inside the 60s the stamp may lag the session's last call.
		seen(t, f, "s-mint", 3*time.Minute+30*time.Second)
		if _, ok := has(f.stalled(t, f.key, "s-other", stallThreshold), id); ok {
			t.Fatalf("shown to another session while the issuing session's stamp is 3m30s old " +
				"(threshold 3m + granularity 60s)")
		}
		seen(t, f, "s-mint", 4*time.Minute+30*time.Second)
		m, ok := has(f.stalled(t, f.key, "s-other", stallThreshold), id)
		if !ok || m.Mine {
			t.Fatalf("after the issuing session went quiet: got (%+v, %v), want mine=false", m, ok)
		}
	})
	t.Run("an empty issuing session goes quiet by the key's sessionless clock", func(t *testing.T) {
		f := newStallFixture(t)
		id := f.mint(t, f.key, "", tenMin, tenMin)
		f.exec(t, `UPDATE api_keys SET sessionless_seen_at = now() - interval '90 seconds' WHERE id = $1`, f.key)
		if _, ok := has(f.stalled(t, f.key, "s-other", stallThreshold), id); ok {
			t.Fatalf("shown while the empty issuing session was active 90s ago")
		}
		if m, ok := has(f.stalled(t, f.key, "", stallThreshold), id); !ok || !m.Mine {
			t.Fatalf("empty recipient for an empty issuing session: got (%+v, %v), want mine=true", m, ok)
		}
		f.exec(t, `UPDATE api_keys SET sessionless_seen_at = now() - interval '3 minutes 30 seconds' WHERE id = $1`, f.key)
		if _, ok := has(f.stalled(t, f.key, "s-other", stallThreshold), id); ok {
			t.Fatalf("shown while the sessionless stamp is 3m30s old (threshold 3m + granularity 60s)")
		}
		f.exec(t, `UPDATE api_keys SET sessionless_seen_at = now() - interval '4 minutes 30 seconds' WHERE id = $1`, f.key)
		if m, ok := has(f.stalled(t, f.key, "s-other", stallThreshold), id); !ok || m.Mine {
			t.Fatalf("after quiet: got (%+v, %v), want mine=false", m, ok)
		}
	})
	t.Run("quiet is read on the issuing key's clock", func(t *testing.T) {
		f := newStallFixture(t)
		k2 := seedAPIKey(t, f.pool, f.owner)
		id := f.mint(t, f.key, "", tenMin, tenMin)
		f.exec(t, `UPDATE api_keys SET sessionless_seen_at = now() - interval '10 minutes' WHERE id = $1`, f.key)
		f.exec(t, `UPDATE api_keys SET sessionless_seen_at = now() WHERE id = $1`, k2)
		if _, ok := has(f.stalled(t, f.key, "s-other", stallThreshold), id); !ok {
			t.Fatalf("a fresh clock on another key of the user hid the monitor")
		}
	})
	t.Run("another key or another user never gets it", func(t *testing.T) {
		f := newStallFixture(t)
		k2 := seedAPIKey(t, f.pool, f.owner)
		id := f.mint(t, f.key, "s-mint", tenMin, tenMin)
		seen(t, f, "s-mint", tenMin)
		if _, ok := has(f.stalled(t, k2, "s-mint", stallThreshold), id); ok {
			t.Errorf("another key of the same user got the monitor")
		}
		stranger, _ := testsupport.SeedUser(t, f.pool, "user")
		sk := seedAPIKey(t, f.pool, stranger)
		res, err := f.store.Stalled(context.Background(), StalledQuery{
			APIKeyID: sk, UserID: stranger, RecipientSession: "s-mint", Threshold: stallThreshold, Limit: 10,
		})
		if err != nil {
			t.Fatalf("Stalled: %v", err)
		}
		if _, ok := has(res, id); ok {
			t.Errorf("another user got the monitor")
		}
	})
}

// TestStalled_LimitAndOrder — row 14 (US-5 "no more than 10").
func TestStalled_LimitAndOrder(t *testing.T) {
	f := newStallFixture(t)
	f.exec(t, `
INSERT INTO agents (user_id, session_id, first_seen_at, last_seen_at)
VALUES ($1, 's-peer', now() - interval '1 hour', now() - interval '1 hour')`, f.owner)
	var mine, others []uuid.UUID
	for i := 0; i < 3; i++ { // own, fresher than every other one
		mine = append(mine, f.mint(t, f.key, "s-me", time.Hour, time.Duration(5+i)*time.Minute))
	}
	for i := 0; i < 9; i++ { // others, oldest last created
		others = append(others, f.mint(t, f.key, "s-peer", 2*time.Hour, time.Duration(20+i)*time.Minute))
	}
	res := f.stalled(t, f.key, "s-me", stallThreshold)
	if res.Total != 12 || len(res.Monitors) != 10 {
		t.Fatalf("got %d of total %d, want 10 of 12", len(res.Monitors), res.Total)
	}
	// mine first, the longest-silent first among them; then the others,
	// longest-silent first.
	want := []uuid.UUID{mine[2], mine[1], mine[0], others[8], others[7], others[6], others[5], others[4], others[3], others[2]}
	for i, id := range ids(res) {
		if id != want[i] {
			t.Fatalf("position %d = %s, want %s (order: mine first, then by oldest success)", i, id, want[i])
		}
	}
	claimed, err := f.store.ClaimStallNotices(context.Background(), "s-me", ids(res))
	if err != nil || len(claimed) != 10 {
		t.Fatalf("claim = (%d, %v), want 10", len(claimed), err)
	}
	var marks int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM monitor_stall_notices n JOIN monitor_credentials mc ON mc.id = n.monitor_id
		  WHERE mc.minted_by_api_key_id = $1 AND n.recipient_session_id = 's-me'`, f.key).Scan(&marks); err != nil {
		t.Fatalf("count marks: %v", err)
	}
	if marks != 10 {
		t.Fatalf("%d show marks, want exactly the 10 shown", marks)
	}
	next := f.stalled(t, f.key, "s-me", stallThreshold)
	if next.Total != 2 || len(next.Monitors) != 2 {
		t.Fatalf("next call: %d of %d, want the 2 not shown", len(next.Monitors), next.Total)
	}
	for _, id := range ids(next) {
		if id != others[1] && id != others[0] {
			t.Fatalf("next call returned %s, which was already shown", id)
		}
	}
}

// TestStalled_ClaimThrottle — row 15 (INV-7, R7, SC-008).
func TestStalled_ClaimThrottle(t *testing.T) {
	ctx := context.Background()
	claim := func(t *testing.T, f *stallFixture, recipient string, id uuid.UUID) bool {
		t.Helper()
		got, err := f.store.ClaimStallNotices(ctx, recipient, []uuid.UUID{id})
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		return len(got) == 1
	}
	age := func(t *testing.T, f *stallFixture, recipient string, id uuid.UUID, ago time.Duration) {
		f.exec(t, `UPDATE monitor_stall_notices SET shown_at = now() - $3::interval
		            WHERE monitor_id = $1 AND recipient_session_id = $2`, id, recipient, ago)
	}

	t.Run("one recipient: again after 4 min is empty, after 6 min succeeds", func(t *testing.T) {
		f := newStallFixture(t)
		id := f.mint(t, f.key, "s-mint", time.Hour, time.Hour)
		if !claim(t, f, "s-a", id) {
			t.Fatalf("first claim failed")
		}
		age(t, f, "s-a", id, 4*time.Minute)
		if claim(t, f, "s-a", id) {
			t.Fatalf("claimed again 4 min after a show")
		}
		age(t, f, "s-a", id, 6*time.Minute)
		if !claim(t, f, "s-a", id) {
			t.Fatalf("not claimed 6 min after a show")
		}
		if !claim(t, f, "s-b", id) {
			t.Fatalf("another recipient session was throttled by the first one")
		}
	})
	t.Run("the empty recipient is its own key ''", func(t *testing.T) {
		f := newStallFixture(t)
		id := f.mint(t, f.key, "", time.Hour, time.Hour)
		if !claim(t, f, "", id) || claim(t, f, "", id) {
			t.Fatalf("empty recipient: want the first claim to win and the second to be empty")
		}
		age(t, f, "", id, 6*time.Minute)
		if !claim(t, f, "", id) {
			t.Fatalf("empty recipient not claimed after 6 min")
		}
	})
	t.Run("two concurrent claims of one pair: exactly one wins", func(t *testing.T) {
		f := newStallFixture(t)
		id := f.mint(t, f.key, "s-mint", time.Hour, time.Hour)
		for rep := 0; rep < 50; rep++ {
			f.exec(t, `DELETE FROM monitor_stall_notices WHERE monitor_id = $1`, id)
			var wg sync.WaitGroup
			start := make(chan struct{})
			wins := make(chan bool, 2)
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					got, err := f.store.ClaimStallNotices(ctx, "s-race", []uuid.UUID{id})
					if err != nil {
						t.Errorf("claim: %v", err)
					}
					wins <- len(got) == 1
				}()
			}
			close(start)
			wg.Wait()
			close(wins)
			n := 0
			for w := range wins {
				if w {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("repetition %d: %d concurrent claims won, want exactly 1", rep, n)
			}
		}
	})
}

// TestStalled_IndexCoverage — row 16 (NFR "does not scan the monitors table"):
// with ≥ 5000 credentials of other keys the stalled selection is served by
// the 000057 partial index on the minting key, never by a Seq Scan of
// monitor_credentials. Red without the index or when its condition and the
// selection's no longer match. Pattern: notifications
// TestPollForMonitor_IndexCoverage.
func TestStalled_IndexCoverage(t *testing.T) {
	ctx := context.Background()
	f := newStallFixture(t)
	noiseUser, _ := testsupport.SeedUser(t, f.pool, "user")
	noiseKey := seedAPIKey(t, f.pool, noiseUser)
	for i := 0; i < 5; i++ {
		f.mint(t, f.key, "s-mint", time.Hour, time.Hour)
	}
	// Registered after the keys → runs first: drop the noise and re-ANALYZE
	// so the shared table's statistics reflect its real contents again.
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = f.pool.Exec(bg, `DELETE FROM monitor_credentials WHERE minted_by_api_key_id = $1`, noiseKey)
		_, _ = f.pool.Exec(bg, `ANALYZE monitor_credentials`)
	})
	if _, err := f.pool.Exec(ctx, `
INSERT INTO monitor_credentials
    (token_digest, user_id, minted_by_api_key_id, audience, capability,
     project_id, scope_issue_id, scope_subtree, event_types,
     created_at, absolute_deadline, expires_at, issued_session_id, token_file_path)
SELECT decode(md5($4::text || g::text), 'hex') || decode(md5(g::text || $4::text), 'hex'),
       $1, $2, 'notifications:poll', 'read', $3, 'noise-' || g, false, ARRAY['comment.created'],
       now() - interval '1 hour', now() + interval '11 hours', now() + interval '5 hours',
       's-noise', '/tmp/noise.token'
  FROM generate_series(1, 5200) g`, noiseUser, noiseKey, f.project, uuid.NewString()); err != nil {
		t.Fatalf("noise seed: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `ANALYZE monitor_credentials`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	rows, err := f.pool.Query(ctx, `EXPLAIN `+stalledSQL,
		f.key, f.owner, "s-mint", stallThreshold, 10, StallNoticeInterval, agents.LastSeenGranularity)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	var plan string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			t.Fatalf("scan plan: %v", err)
		}
		plan += line + "\n"
	}
	rows.Close()
	t.Logf("stalled selection plan:\n%s", plan)
	if strings.Contains(plan, "Seq Scan on monitor_credentials") {
		t.Errorf("the stalled selection scans monitor_credentials:\n%s", plan)
	}
	if !strings.Contains(plan, "monitor_credentials_stall_candidates_idx") {
		t.Errorf("the stalled selection does not use the partial index of 000057:\n%s", plan)
	}
}

// TestStalled_OnlyVisibleProjects — sk-674.18 matrix row 28a (US-1, «stalled
// monitors», Store). Every actor of the T003 corpus owns one stalled monitor
// rooted at the marker issue of every fixture project, minted through its
// own key by its own session. Stalled returns exactly the monitors of the
// projects the actor sees (testsupport.TenancyVisible) — row and Total alike
// — and its own are returned: an unavailable project shows neither as a row
// nor in the number. The deactivated actor gets nothing (INV-5: its monitors
// are dead). The internal admin's monitors on the "Personal" of others are
// returned neither as a row nor in the number (asserted since T017).
func TestStalled_OnlyVisibleProjects(t *testing.T) {
	pool := newTestPool(t)
	f := testsupport.SeedTenancy(t, pool)
	store := New(pool, 6*time.Hour)
	ctx := context.Background()

	for _, a := range testsupport.TenancyActors {
		t.Run(string(a), func(t *testing.T) {
			u := f.User(t, a)
			key := seedAPIKey(t, pool, u.ID)
			session := "s-tn-" + string(a)
			want := map[uuid.UUID]string{} // monitor → project key
			for _, p := range f.Projects() {
				_, cred, err := store.Mint(ctx, MintParams{
					UserID: u.ID, MintedByAPIKeyID: key, Audience: "notifications:poll", Capability: "read",
					ProjectID: p.ID, ScopeIssueID: p.IssueID, EventTypes: []string{"comment.created"},
					IssuedSessionID: session, TokenFilePath: "/tmp/skein-mon-tenancy.token",
				})
				if err != nil {
					t.Fatalf("%s: Mint: %v", p.Key(), err)
				}
				if _, err := pool.Exec(ctx, `UPDATE monitor_credentials SET created_at = now() - interval '1 hour' WHERE id = $1`, cred.ID); err != nil {
					t.Fatalf("%s: backdate: %v", p.Key(), err)
				}
				if f.Sees(a, p) {
					want[cred.ID] = p.Key()
				}
			}

			res, err := store.Stalled(ctx, StalledQuery{
				APIKeyID: key, UserID: u.ID, RecipientSession: session,
				Threshold: stallThreshold, Limit: 50,
			})
			if err != nil {
				t.Fatalf("Stalled: %v", err)
			}
			got := map[uuid.UUID]bool{}
			for _, m := range res.Monitors {
				got[m.ID] = true
				if _, ok := want[m.ID]; !ok {
					t.Errorf("monitor %s of an unavailable project returned", m.ID)
				}
			}
			for id, key := range want {
				if !got[id] {
					t.Errorf("own monitor on visible %s not returned", key)
				}
			}
			if res.Total != len(want) {
				t.Errorf("Total = %d, want %d", res.Total, len(want))
			}
		})
	}
}
