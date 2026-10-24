package spaces_test

// delete_test.go — sk-674.18 T019, matrix rows 52 and 53 (US-4, K-22,
// INV-25, E-9). Reference — the deletion paragraph of US-4 and K-22, written
// before the cascade:
//
//   - row 52: after the commit no row of the deleted projects is left in any
//     table of the K-22 list (an archived project included, and a subtask
//     carrying the prefix of a parent from another project of the same
//     space); everybody's notifications on their issues are gone; their
//     unpublished events are gone; the pending invitations are revoked,
//     their tokens superseded, their emails cancelled; the prefixes are in
//     reserved_prefixes; a neighbouring space is untouched;
//   - row 53: the `deleted` signal to the participants and access_changed
//     arrive after the commit; a rolled back deletion leaves nothing deleted
//     and publishes nothing.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"

	"skein-api/internal/mail"
	"skein-api/internal/signals"
	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
	"skein-api/internal/tokens"
)

// delFixture is a space with two projects (one archived), issues with
// everything the K-22 list names, a pending invitation with a token and an
// email, and a neighbouring space with a project of its own.
type delFixture struct {
	pool                *pgxpool.Pool
	space, neighbour    uuid.UUID
	owner, member       uuid.UUID
	projects            []uuid.UUID
	prefixes            []string
	issues              []string
	neighbourProject    uuid.UUID
	neighbourIssue      string
	invitation, emailID uuid.UUID
	shortID             string
}

func newDelFixture(t *testing.T) *delFixture {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	f := &delFixture{pool: pool}
	f.owner, _ = testsupport.SeedUser(t, pool, "")
	f.member, _ = testsupport.SeedUser(t, pool, "")
	f.space = testsupport.SeedSpace(t, pool, f.owner)
	testsupport.AddSpaceMember(t, pool, f.space, f.member)
	if err := pool.QueryRow(ctx, `SELECT short_id FROM spaces WHERE id = $1`, f.space).Scan(&f.shortID); err != nil {
		t.Fatal(err)
	}
	for i, tag := range []string{"dla", "dlb"} {
		pid, prefix := testsupport.SeedProjectInSpace(t, pool, f.owner, f.space, tag)
		testsupport.AddProjectMember(t, pool, pid, f.member, "member")
		f.projects, f.prefixes = append(f.projects, pid), append(f.prefixes, prefix)
		issue := prefix + "-1"
		testsupport.SeedIssue(t, pool, pid, f.owner, issue, "dl issue", "open", 2)
		f.issues = append(f.issues, issue)
		if i == 1 {
			if _, err := pool.Exec(ctx, `UPDATE projects SET archived_at = now() WHERE id = $1`, pid); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A subtask with the prefix of its parent (project A) living in project
	// B of the same space.
	sub := f.prefixes[0] + "-1.1"
	testsupport.SeedIssue(t, pool, f.projects[1], f.owner, sub, "dl subtask", "open", 2)
	if _, err := pool.Exec(ctx, `UPDATE issues SET parent_id = $2 WHERE id = $1`, sub, f.issues[0]); err != nil {
		t.Fatal(err)
	}
	testsupport.SeedDep(t, pool, sub, f.issues[0], "parent-child", f.owner)
	f.issues = append(f.issues, sub)

	// Everything K-22 lists, on the first issue.
	is, pid := f.issues[0], f.projects[0]
	commentID := testsupport.SeedComment(t, pool, is, f.member, "dl comment")
	testsupport.SeedReminder(t, pool, is, f.member)
	testsupport.SeedAttachment(t, pool, is, f.member, "ready")
	for _, q := range []string{
		`INSERT INTO issue_watchers (issue_id, user_id, reasons) VALUES ($1, $2, '{manual}')`,
		`INSERT INTO issue_watch_optouts (issue_id, user_id) VALUES ($1, $2)`,
	} {
		if _, err := pool.Exec(ctx, q, is, f.owner); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO project_watchers (project_id, user_id) VALUES ($1, $2)`, pid, f.member); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO issue_commits (issue_id, repo_id, hash, linked_by, linked_by_kind)
VALUES ($1, $2, 'abcdef1', $3, 'human')`, is, uuid.New(), f.owner); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	var eventID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO events (type, actor, actor_type, entity_type, entity_id, project_id, payload)
VALUES ('comment.created', 'owner', 'human', 'comment', $1, $2, '{}'::jsonb) RETURNING id`,
		commentID.String(), pid).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO notifications (user_id, event_id, type, project_id)
VALUES ($1, $2, 'comment.created', $3)`, f.member, eventID, pid); err != nil {
		t.Fatalf("seed notification: %v", err)
	}

	// A pending invitation with an issued token and a queued email.
	if err := pool.QueryRow(ctx, `
INSERT INTO space_invitations (space_id, email, invited_by, lang, expires_at)
VALUES ($1, $2, $3, 'en', now() + interval '7 days') RETURNING id`,
		f.space, "dl-"+uuid.NewString()[:8]+"@test.local", f.owner).Scan(&f.invitation); err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var to string
		if err := tx.QueryRow(ctx, `SELECT email FROM space_invitations WHERE id = $1`, f.invitation).Scan(&to); err != nil {
			return err
		}
		var err error
		f.emailID, err = mail.Enqueue(ctx, tx, mail.Message{Kind: mail.KindSpaceInvite, To: to, Lang: mail.LangEN, InvitationID: f.invitation}, time.Hour)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var gen int
	if err := pool.QueryRow(ctx, `
UPDATE outgoing_emails SET state = 'leased', attempts = attempts + 1, leased_until = now() + interval '2 minutes'
 WHERE id = $1 RETURNING attempts`, f.emailID).Scan(&gen); err != nil {
		t.Fatal(err)
	}
	if _, err := tokens.IssueForEmail(ctx, pool, f.emailID, gen); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE outgoing_emails SET state = 'pending', leased_until = NULL WHERE id = $1`, f.emailID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `UPDATE space_invitations SET current_token_id = NULL WHERE id = $1`, f.invitation)
		_, _ = pool.Exec(c, `DELETE FROM auth_tokens WHERE invitation_id = $1`, f.invitation)
		_, _ = pool.Exec(c, `DELETE FROM outgoing_emails WHERE invitation_id = $1`, f.invitation)
		_, _ = pool.Exec(c, `DELETE FROM space_invitations WHERE id = $1`, f.invitation)
		_, _ = pool.Exec(c, `DELETE FROM events WHERE entity_type = 'space' AND entity_id = $1`, f.space.String())
		_, _ = pool.Exec(c, `DELETE FROM reserved_prefixes WHERE prefix = ANY($1)`, lower(f.prefixes))
	})

	// The neighbour: another space with a project and an issue.
	f.neighbour = testsupport.SeedSpace(t, pool, f.owner)
	var nprefix string
	f.neighbourProject, nprefix = testsupport.SeedProjectInSpace(t, pool, f.owner, f.neighbour, "dln")
	f.neighbourIssue = nprefix + "-1"
	testsupport.SeedIssue(t, pool, f.neighbourProject, f.owner, f.neighbourIssue, "dl neighbour", "open", 2)
	return f
}

func lower(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = strings.ToLower(s)
	}
	return out
}

// leftovers counts the rows of the deleted projects left in every table of
// the K-22 list, by table.
func (f *delFixture) leftovers(t *testing.T) map[string]int {
	t.Helper()
	q := map[string]string{
		"projects":            `SELECT count(*) FROM projects WHERE id = ANY($1)`,
		"issues":              `SELECT count(*) FROM issues WHERE project_id = ANY($1) OR id = ANY($2)`,
		"comments":            `SELECT count(*) FROM comments WHERE issue_id = ANY($2)`,
		"events":              `SELECT count(*) FROM events WHERE project_id = ANY($1)`,
		"notifications":       `SELECT count(*) FROM notifications WHERE project_id = ANY($1)`,
		"issue_watchers":      `SELECT count(*) FROM issue_watchers WHERE issue_id = ANY($2)`,
		"issue_watch_optouts": `SELECT count(*) FROM issue_watch_optouts WHERE issue_id = ANY($2)`,
		"project_watchers":    `SELECT count(*) FROM project_watchers WHERE project_id = ANY($1)`,
		"reminders":           `SELECT count(*) FROM reminders WHERE issue_id = ANY($2)`,
		"attachments":         `SELECT count(*) FROM attachments WHERE issue_id = ANY($2)`,
		"dependencies":        `SELECT count(*) FROM dependencies WHERE from_id = ANY($2) OR to_id = ANY($2)`,
		"issue_commits":       `SELECT count(*) FROM issue_commits WHERE issue_id = ANY($2)`,
		"project_members":     `SELECT count(*) FROM project_members WHERE project_id = ANY($1)`,
		"project_aliases":     `SELECT count(*) FROM project_aliases WHERE project_id = ANY($1)`,
		"project_counters":    `SELECT count(*) FROM project_counters WHERE project_id = ANY($1)`,
		"components":          `SELECT count(*) FROM components WHERE project_id = ANY($1)`,
		"monitor_credentials": `SELECT count(*) FROM monitor_credentials WHERE project_id = ANY($1)`,
		"space_members":       `SELECT count(*) FROM space_members WHERE space_id = $3`,
		"spaces":              `SELECT count(*) FROM spaces WHERE id = $3`,
		"space_storage_usage": `SELECT count(*) FROM space_storage_usage WHERE space_id = $3`,
	}
	out := map[string]int{}
	for table, sql := range q {
		var n int
		// Every query binds the same three values; the wrapper names all
		// three so pgx accepts them whatever the query uses.
		wrapped := `SELECT (` + sql + `) WHERE $1::uuid[] IS NOT NULL AND $2::text[] IS NOT NULL AND $3::uuid IS NOT NULL`
		if err := f.pool.QueryRow(context.Background(), wrapped, f.projects, f.issues, f.space).Scan(&n); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		out[table] = n
	}
	return out
}

// Row 52: a space whose invitations have a
// history — accepted, declined, expired, revoked — is deleted like any other
// (US-4: the deletion is one transaction that succeeds). The pending one is
// revoked; a finished one keeps its outcome, without a space: the link of
// any of them answers "space deleted" (US-5, the order of states).
func TestDelete_FinishedInvitationsKeepTheirOutcome(t *testing.T) {
	ctx := context.Background()
	f := newDelFixture(t)
	finished := map[string]uuid.UUID{}
	for _, st := range []string{"accepted", "declined", "expired", "revoked"} {
		var id uuid.UUID
		if err := f.pool.QueryRow(ctx, `
INSERT INTO space_invitations (space_id, email, invited_by, status, lang, expires_at, resolved_at)
VALUES ($1, $2, $3, $4, 'en', now() + interval '7 days', now()) RETURNING id`,
			f.space, st+"-"+uuid.NewString()[:8]+"@test.local", f.owner, st).Scan(&id); err != nil {
			t.Fatalf("seed a %s invitation: %v", st, err)
		}
		finished[st] = id
		t.Cleanup(func() {
			_, _ = f.pool.Exec(context.Background(), `DELETE FROM space_invitations WHERE id = $1`, id)
		})
	}

	if err := spaces.NewStore(f.pool).Delete(ctx, f.space, spaces.Actor{UserID: f.owner, Username: "owner", Type: "human"}); err != nil {
		t.Fatalf("Delete of a space with finished invitations: %v", err)
	}
	var spaceLeft int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM spaces WHERE id = $1`, f.space).Scan(&spaceLeft); err != nil {
		t.Fatal(err)
	}
	if spaceLeft != 0 {
		t.Fatal("the space is still there")
	}
	for st, id := range finished {
		var status string
		var spaceGone bool
		if err := f.pool.QueryRow(ctx, `SELECT status, space_id IS NULL FROM space_invitations WHERE id = $1`, id).
			Scan(&status, &spaceGone); err != nil {
			t.Fatalf("the %s invitation: %v", st, err)
		}
		if status != st || !spaceGone {
			t.Errorf("the %s invitation after the deletion: %s, space cleared %v; want %s, cleared", st, status, spaceGone, st)
		}
	}
	var pending string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM space_invitations WHERE id = $1`, f.invitation).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != "revoked" {
		t.Errorf("the pending invitation after the deletion: %s, want revoked", pending)
	}
}

// Row 52 — nothing of the deleted projects is left; invitations, tokens,
// emails and prefixes as K-22 says; the neighbour untouched.
func TestDelete_K22Cascade(t *testing.T) {
	ctx := context.Background()
	f := newDelFixture(t)
	if err := spaces.NewStore(f.pool).Delete(ctx, f.space, spaces.Actor{UserID: f.owner, Username: "owner", Type: "human"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for table, n := range f.leftovers(t) {
		if n != 0 {
			t.Errorf("%s: %d rows of the deleted space left", table, n)
		}
	}
	var status string
	var spaceGone bool
	if err := f.pool.QueryRow(ctx, `SELECT status, space_id IS NULL FROM space_invitations WHERE id = $1`, f.invitation).Scan(&status, &spaceGone); err != nil {
		t.Fatalf("invitation: %v", err)
	}
	if status != "revoked" || !spaceGone {
		t.Errorf("invitation = %s, space cleared %v; want revoked, cleared", status, spaceGone)
	}
	var liveTokens int
	var emailState, emailOutcome string
	if err := f.pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM auth_tokens WHERE invitation_id = $1 AND used_at IS NULL AND superseded_at IS NULL),
       (SELECT state FROM outgoing_emails WHERE id = $2),
       (SELECT coalesce(outcome, '') FROM outgoing_emails WHERE id = $2)`, f.invitation, f.emailID).
		Scan(&liveTokens, &emailState, &emailOutcome); err != nil {
		t.Fatal(err)
	}
	if liveTokens != 0 || emailState != "final" || emailOutcome != "cancelled" {
		t.Errorf("invitation token live %d, email %s/%s; want 0, final/cancelled", liveTokens, emailState, emailOutcome)
	}
	var reserved int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM reserved_prefixes WHERE prefix = ANY($1)`, lower(f.prefixes)).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if reserved != len(f.prefixes) {
		t.Errorf("reserved prefixes = %d, want %d", reserved, len(f.prefixes))
	}
	var neighbour int
	if err := f.pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM spaces WHERE id = $1) + (SELECT count(*) FROM projects WHERE id = $2)
     + (SELECT count(*) FROM issues WHERE id = $3)`, f.neighbour, f.neighbourProject, f.neighbourIssue).Scan(&neighbour); err != nil {
		t.Fatal(err)
	}
	if neighbour != 3 {
		t.Errorf("the neighbouring space lost rows: %d of 3 left", neighbour)
	}
}

// accessSink records the personal signals and access_changed events per
// user from an in-process NATS server installed as the process publisher.
type accessSink struct {
	mu      sync.Mutex
	signals map[uuid.UUID][]signals.Signal
	access  map[uuid.UUID][]signals.AccessChanged
}

func startSink(t *testing.T) *accessSink {
	t.Helper()
	opts := natstest.DefaultTestOptions
	opts.Port = -1
	opts.NoLog = true
	srv := natstest.RunServer(&opts)
	t.Cleanup(srv.Shutdown)
	pub, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pub.Close)
	sub, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	s := &accessSink{signals: map[uuid.UUID][]signals.Signal{}, access: map[uuid.UUID][]signals.AccessChanged{}}
	uidOf := func(subject string) uuid.UUID {
		parts := strings.Split(subject, ".")
		id, _ := uuid.Parse(parts[2])
		return id
	}
	if _, err := sub.Subscribe(signals.SignalSubjectPattern, func(m *nats.Msg) {
		var body signals.Signal
		_ = json.Unmarshal(m.Data, &body)
		s.mu.Lock()
		s.signals[uidOf(m.Subject)] = append(s.signals[uidOf(m.Subject)], body)
		s.mu.Unlock()
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := sub.Subscribe(signals.AccessChangedSubjectPattern, func(m *nats.Msg) {
		var body signals.AccessChanged
		_ = json.Unmarshal(m.Data, &body)
		s.mu.Lock()
		s.access[uidOf(m.Subject)] = append(s.access[uidOf(m.Subject)], body)
		s.mu.Unlock()
	}); err != nil {
		t.Fatal(err)
	}
	if err := sub.Flush(); err != nil {
		t.Fatal(err)
	}
	signals.Configure(pub)
	t.Cleanup(func() { signals.Configure(nil) })
	return s
}

// settle publishes a sentinel signal and waits for it: NATS Core keeps the
// order of one publisher towards one subscriber, so everything published
// before it has arrived.
func (s *accessSink) settle(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	sentinel := uuid.New()
	if err := signals.BeginTxFunc(context.Background(), pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		signals.Add(tx, sentinel, signals.AreaInbox)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		got := len(s.signals[sentinel]) > 0
		s.mu.Unlock()
		if got {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the sentinel signal did not arrive")
}

var errRollback = errors.New("roll the deletion back")

// Row 53 — the signals after the commit and only at a commit.
func TestDelete_SignalsOnlyAfterCommit(t *testing.T) {
	ctx := context.Background()
	sink := startSink(t)
	f := newDelFixture(t)
	actor := spaces.Actor{UserID: f.owner, Username: "owner", Type: "human"}
	// an admin outside the space
	// sees its projects by INV-1 g and loses them with it — he is told
	// like a participant (US-19: the hub drops the groups at once).
	var flagBefore bool
	if err := f.pool.QueryRow(ctx, `SELECT admin_enabled FROM instance_settings WHERE id = 1`).Scan(&flagBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE instance_settings SET admin_enabled = true WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `UPDATE instance_settings SET admin_enabled = $1 WHERE id = 1`, flagBefore)
	})
	outsideAdmin, _ := testsupport.SeedUser(t, f.pool, "admin")

	// A rolled back deletion: nothing deleted, nothing published.
	err := signals.BeginTxFunc(ctx, f.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		if err := spaces.DeleteInTx(ctx, tx, f.space, actor); err != nil {
			return err
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatalf("rolled back deletion: %v", err)
	}
	sink.settle(t, f.pool)
	if left := f.leftovers(t); left["projects"] != len(f.projects) || left["spaces"] != 1 {
		t.Fatalf("the rolled back deletion left a partial state: %v", left)
	}
	sink.mu.Lock()
	for _, u := range []uuid.UUID{f.owner, f.member} {
		if len(sink.signals[u]) != 0 || len(sink.access[u]) != 0 {
			t.Errorf("a rolled back deletion published to %s: %d signals, %d access events", u, len(sink.signals[u]), len(sink.access[u]))
		}
	}
	sink.mu.Unlock()

	// The committed deletion: `deleted` to the participants, and the
	// projects and the space lost.
	if err := spaces.NewStore(f.pool).Delete(ctx, f.space, actor); err != nil {
		t.Fatalf("delete: %v", err)
	}
	sink.settle(t, f.pool)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, u := range []uuid.UUID{f.owner, f.member, outsideAdmin} {
		var deleted bool
		for _, sig := range sink.signals[u] {
			for _, ev := range sig.Spaces {
				if ev.Reason == signals.SpaceDeleted && ev.SpaceID == f.shortID {
					deleted = true
				}
			}
		}
		if !deleted {
			t.Errorf("%s got no `deleted` space event", u)
		}
		if len(sink.access[u]) != 1 {
			t.Errorf("%s got %d access_changed events, want 1", u, len(sink.access[u]))
			continue
		}
		ac := sink.access[u][0]
		if len(ac.LostSpaceIDs) != 1 || ac.LostSpaceIDs[0] != f.shortID {
			t.Errorf("%s lostSpaceIds = %v, want [%s]", u, ac.LostSpaceIDs, f.shortID)
		}
		for _, p := range f.projects {
			found := false
			for _, l := range ac.LostProjectIDs {
				found = found || l == p
			}
			if !found {
				t.Errorf("%s lostProjectIds %v lacks %s", u, ac.LostProjectIDs, p)
			}
		}
	}
}
