package handler

// accounts_delete_test.go — sk-674.18, the test delivery of rows 176, 177,
// 178 and 179 (T041: account deletion, US-10, D-8, K-21, SC-011, INV-18).
// The references are the requirements as the spec words them, written
// before reading the code's output:
//
//   - US-10: only with SKEIN_SIGNUP_ENABLED=true, confirmed by the
//     password; an owner of a space with other people is refused and the
//     spaces are named; the owner of the common space is refused with its
//     own code. Otherwise, in one operation, the personal space and the
//     spaces where the account is alone go with everything in them, every
//     other space is left as by US-7, and the account is anonymized: email,
//     name and username replaced by a permanent pseudonym, the password,
//     sessions, keys, push subscriptions, tokens and unfinished emails
//     erased; the email and the name cleaned out of the assignee text, the
//     events and the search index; the hashes of the email and the
//     username kept so that neither registers again; after the answer no
//     session and no key of the account works.
//   - K-21: the snapshots — the assignee text, comment authors, event
//     actors and data (old/new assignee included), commit authors, agent
//     labels, mentions in the text.
//   - Lead's decisions for T041: the last active admin is refused with
//     last_admin_cannot_demote; the email and @username in free text are
//     replaced, a name in free text is not.
//   - INV-18: the pseudonym is unique per account and matches no username
//     anyone may hold.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"

	"skein-api/internal/accounts"
	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/invitations"
	"skein-api/internal/signals"
	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
	"skein-api/internal/users"
)

const deletePassword = "delete me please 42"

// pseudonymShape is INV-18's pseudonym: deleted-<8 hex>.
var pseudonymShape = regexp.MustCompile(`^deleted-[0-9a-f]{8}$`)

// deleteRig is the deletion routes behind the user gate of production
// (ParseUserHeaders, RequireSessionEpoch, RequireUser).
type deleteRig struct {
	pool   *pgxpool.Pool
	store  *users.Store
	router http.Handler
}

func newDeleteRig(t *testing.T, pool *pgxpool.Pool, signupEnabled bool) *deleteRig {
	t.Helper()
	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	h := NewAccountDeletionHandler(store, signupEnabled)
	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(ParseUserHeaders, RequireSessionEpoch(pool), RequireUser)
		r.Delete("/me", h.DeleteMe)
		r.Get("/me/deletion-preview", h.DeletionPreview)
		r.Get("/probe", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	})
	return &deleteRig{pool: pool, store: store, router: r}
}

type deleteAnswer struct {
	status int
	body   map[string]any
	raw    []byte
}

func (a deleteAnswer) code() string {
	s, _ := a.body["error"].(string)
	return s
}

// spaceIDs is the short ids of the extra field `spaces`.
func (a deleteAnswer) spaceIDs() []string {
	var out []string
	list, _ := a.body["spaces"].([]any)
	for _, s := range list {
		if m, ok := s.(map[string]any); ok {
			id, _ := m["id"].(string)
			out = append(out, id)
		}
	}
	return out
}

// do sends a request as user with the session epoch the BFF would send.
func (r *deleteRig) do(t *testing.T, user uuid.UUID, epoch int64, method, path string, body any) deleteAnswer {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set(auth.HeaderUserID, user.String())
	req.Header.Set(auth.HeaderUsername, "whoever")
	req.Header.Set(auth.HeaderUserRole, "user")
	req.Header.Set(auth.HeaderSessionEpoch, strconv.FormatInt(epoch, 10))
	rec := httptest.NewRecorder()
	r.router.ServeHTTP(rec, req)
	a := deleteAnswer{status: rec.Code, raw: rec.Body.Bytes()}
	_ = json.Unmarshal(a.raw, &a.body)
	return a
}

func (r *deleteRig) epoch(t *testing.T, user uuid.UUID) int64 {
	t.Helper()
	var e int64
	if err := r.pool.QueryRow(context.Background(), `SELECT session_epoch FROM users WHERE id = $1`, user).Scan(&e); err != nil {
		t.Fatal(err)
	}
	return e
}

// deleteMe is DELETE /me with the password, under the account's current
// session epoch.
func (r *deleteRig) deleteMe(t *testing.T, user uuid.UUID, password string) deleteAnswer {
	t.Helper()
	return r.do(t, user, r.epoch(t, user), http.MethodDelete, "/api/v1/me", map[string]string{"password": password})
}

func (r *deleteRig) preview(t *testing.T, user uuid.UUID) deleteAnswer {
	t.Helper()
	return r.do(t, user, r.epoch(t, user), http.MethodGet, "/api/v1/me/deletion-preview", nil)
}

// deletable gives the account a real password and registers the cleanup a
// deleted account needs before its row can go: the rows that name it with
// a RESTRICT key (its events, finished emails) and its reserved hashes.
// Registered after the account's own seed, so it runs before it.
func deletable(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) (email, username string) {
	t.Helper()
	ctx := context.Background()
	hash, err := auth.Hash(deletePassword, auth.MinBcryptCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1 RETURNING email, username`, user, hash).
		Scan(&email, &username); err != nil {
		t.Fatalf("password of %s: %v", user, err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM events WHERE actor_user_id = $1 OR (entity_type = 'user' AND entity_id = $1::text)`, user)
		_, _ = pool.Exec(c, `DELETE FROM auth_tokens WHERE user_id = $1`, user)
		_, _ = pool.Exec(c, `DELETE FROM outgoing_emails WHERE user_id = $1`, user)
		_, _ = pool.Exec(c, `
DELETE FROM reserved_identities
 WHERE (kind = 'email' AND value_hash = sha256(convert_to($1, 'UTF8')))
    OR (kind = 'username' AND value_hash = sha256(convert_to($2, 'UTF8')))`, email, username)
	})
	return email, username
}

func isDeleted(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) bool {
	t.Helper()
	var deleted bool
	if err := pool.QueryRow(context.Background(), `SELECT deleted_at IS NOT NULL FROM users WHERE id = $1`, user).Scan(&deleted); err != nil {
		t.Fatal(err)
	}
	return deleted
}

func shortOf(t *testing.T, pool *pgxpool.Pool, space uuid.UUID) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `SELECT short_id FROM spaces WHERE id = $1`, space).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// ---------------------------------------------------------------------------
// Row 176
// ---------------------------------------------------------------------------

// TestAccountDelete_Refusals — row 176. Each refusal in its own case, and
// in none of them is anything changed: the account is not deleted, its
// session still works, its spaces exist.
func TestAccountDelete_Refusals(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	open := newDeleteRig(t, pool, true)

	untouched := func(t *testing.T, user uuid.UUID, epoch int64) {
		t.Helper()
		if isDeleted(t, pool, user) {
			t.Error("the account was deleted on a refusal")
		}
		if a := open.do(t, user, epoch, http.MethodGet, "/api/v1/probe", nil); a.status != http.StatusOK {
			t.Errorf("the session after a refusal = %d %s, want 200", a.status, a.raw)
		}
	}

	t.Run("registration closed", func(t *testing.T) {
		closed := newDeleteRig(t, pool, false)
		u, _ := testsupport.SeedUser(t, pool, "user")
		deletable(t, pool, u)
		epoch := closed.epoch(t, u)
		if a := closed.deleteMe(t, u, deletePassword); a.status != http.StatusUnprocessableEntity || a.code() != "account_deletion_disabled" {
			t.Errorf("DELETE /me = %d %q, want 422 account_deletion_disabled", a.status, a.code())
		}
		if a := closed.preview(t, u); a.status != http.StatusUnprocessableEntity || a.code() != "account_deletion_disabled" {
			t.Errorf("preview = %d %q, want 422 account_deletion_disabled", a.status, a.code())
		}
		untouched(t, u, epoch)
	})

	t.Run("password", func(t *testing.T) {
		u, _ := testsupport.SeedUser(t, pool, "user")
		deletable(t, pool, u)
		epoch := open.epoch(t, u)
		if a := open.do(t, u, epoch, http.MethodDelete, "/api/v1/me", map[string]string{}); a.status != http.StatusBadRequest || a.code() != "invalid_request" {
			t.Errorf("no password = %d %q, want 400 invalid_request", a.status, a.code())
		}
		if a := open.deleteMe(t, u, "not the password"); a.status != http.StatusUnprocessableEntity || a.code() != "invalid_credentials" {
			t.Errorf("wrong password = %d %q, want 422 invalid_credentials", a.status, a.code())
		}
		untouched(t, u, epoch)
	})

	t.Run("owner of a space with people", func(t *testing.T) {
		u, _ := testsupport.SeedUser(t, pool, "user")
		other, _ := testsupport.SeedUser(t, pool, "user")
		deletable(t, pool, u)
		shared := testsupport.SeedSpace(t, pool, u)
		testsupport.AddSpaceMember(t, pool, shared, other)
		alone := testsupport.SeedSpace(t, pool, u)
		short := shortOf(t, pool, shared)
		epoch := open.epoch(t, u)

		p := open.preview(t, u)
		if p.status != http.StatusOK {
			t.Fatalf("preview = %d %s", p.status, p.raw)
		}
		var preview struct {
			Blocking []struct {
				ID string `json:"id"`
			} `json:"blocking"`
			Deleted []struct {
				ID string `json:"id"`
			} `json:"deleted"`
		}
		_ = json.Unmarshal(p.raw, &preview)
		if len(preview.Blocking) != 1 || preview.Blocking[0].ID != short {
			t.Errorf("preview blocking = %+v, want exactly the shared space %s", preview.Blocking, short)
		}
		if len(preview.Deleted) != 1 || preview.Deleted[0].ID != shortOf(t, pool, alone) {
			t.Errorf("preview deleted = %+v, want exactly the space he is alone in", preview.Deleted)
		}

		a := open.deleteMe(t, u, deletePassword)
		if a.status != http.StatusUnprocessableEntity || a.code() != "owns_spaces_with_members" {
			t.Fatalf("DELETE /me = %d %q, want 422 owns_spaces_with_members", a.status, a.code())
		}
		if ids := a.spaceIDs(); len(ids) != 1 || ids[0] != short {
			t.Errorf("named spaces = %v, want [%s]", ids, short)
		}
		untouched(t, u, epoch)
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM spaces WHERE id = ANY($1)`, []uuid.UUID{shared, alone}).Scan(&n); err != nil || n != 2 {
			t.Errorf("spaces after the refusal: %d %v, want both", n, err)
		}
	})

	t.Run("owner of the common space", func(t *testing.T) {
		u, _ := testsupport.SeedUser(t, pool, "user")
		deletable(t, pool, u)
		var stashed *uuid.UUID
		if err := pool.QueryRow(ctx, `UPDATE spaces SET kind = 'regular' WHERE kind = 'common' RETURNING id`).Scan(&stashed); err == nil && stashed != nil {
			id := *stashed
			t.Cleanup(func() {
				if _, err := pool.Exec(context.Background(), `UPDATE spaces SET kind = 'common' WHERE id = $1`, id); err != nil {
					t.Errorf("restore the common space: %v", err)
				}
			})
		}
		common := testsupport.SeedSpaceOfKind(t, pool, u, "common")
		epoch := open.epoch(t, u)
		a := open.deleteMe(t, u, deletePassword)
		if a.status != http.StatusUnprocessableEntity || a.code() != "owns_common_space" {
			t.Fatalf("DELETE /me = %d %q, want 422 owns_common_space", a.status, a.code())
		}
		if ids := a.spaceIDs(); len(ids) != 1 || ids[0] != shortOf(t, pool, common) {
			t.Errorf("named spaces = %v, want the common space", ids)
		}
		untouched(t, u, epoch)
	})

	t.Run("the last active admin", func(t *testing.T) {
		u, _ := testsupport.SeedUser(t, pool, "admin")
		deletable(t, pool, u)
		// Exactly one active admin: the others are deactivated for the
		// test and only they are restored.
		rows, err := pool.Query(ctx, `
UPDATE users SET deactivated_at = now()
 WHERE role = 'admin' AND deactivated_at IS NULL AND id <> $1 RETURNING id`, u)
		if err != nil {
			t.Fatal(err)
		}
		var squashed []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			_ = rows.Scan(&id)
			squashed = append(squashed, id)
		}
		rows.Close()
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `UPDATE users SET deactivated_at = NULL WHERE id = ANY($1)`, squashed)
		})
		epoch := open.epoch(t, u)
		if a := open.deleteMe(t, u, deletePassword); a.status != http.StatusUnprocessableEntity || a.code() != "last_admin_cannot_demote" {
			t.Errorf("DELETE /me = %d %q, want 422 last_admin_cannot_demote", a.status, a.code())
		}
		untouched(t, u, epoch)
	})
}

// ---------------------------------------------------------------------------
// Rows 177 and 178
// ---------------------------------------------------------------------------

// deletedWorld is the isolation fixture with uAB (both spaces, his
// "Personal") about to delete his account, and everything K-21 names seeded
// around him.
type deletedWorld struct {
	*isoWorld
	rig                    *deleteRig
	d                      uuid.UUID
	email, username, name  string
	solo, soloProject      uuid.UUID
	personalProject        uuid.UUID
	openIssue, closedIssue string
	ownComment, mention    uuid.UUID
	assigneeEvent, control int64
	ownEvent               int64
	invitation             uuid.UUID
	component, attachment  uuid.UUID
	keyPlain               string
	keyID                  uuid.UUID
}

func newDeletedWorld(t *testing.T) *deletedWorld {
	t.Helper()
	w := &deletedWorld{isoWorld: newIsoWorld(t)}
	ctx := context.Background()
	pool := w.pool
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	w.rig = newDeleteRig(t, pool, true)
	w.d = w.f.User(t, testsupport.ActorBoth).ID
	w.email, w.username = deletable(t, pool, w.d)
	w.name = "Deleted Person " + w.word
	exec(`UPDATE users SET name = $2 WHERE id = $1`, w.d, w.name)
	w.personalProject = w.f.Personal[testsupport.ActorBoth].ID

	// A regular space he owns alone, with a project.
	w.solo = testsupport.SeedSpace(t, pool, w.d)
	w.soloProject, _ = testsupport.SeedProjectInSpace(t, pool, w.d, w.solo, "solo")

	p := w.byKey[keyAMembers]
	other := w.f.User(t, testsupport.ActorProjectOwner)
	vec := isoVectorLiteral()

	// Assignee text: an open issue by username, a closed one by e-mail.
	w.openIssue = testsupport.SeedIssue(t, pool, p.ID, other.ID, p.IDPrefix+"-91", "open on him", "open", 2)
	testsupport.SetAssignee(t, pool, w.openIssue, w.username)
	w.closedIssue = testsupport.SeedIssue(t, pool, p.ID, other.ID, p.IDPrefix+"-92", "closed on him", "closed", 2)
	testsupport.SetAssignee(t, pool, w.closedIssue, strings.ToUpper(w.email))

	// Comments: his own; another's naming him by mention and e-mail.
	w.ownComment = testsupport.SeedComment(t, pool, p.IssueID, w.d, "my own words")
	exec(`UPDATE comments SET author = $2 WHERE id = $1`, w.ownComment, w.name)
	w.mention = testsupport.SeedComment(t, pool, p.IssueID, other.ID,
		"ping @"+w.username+", write to "+strings.ToUpper(w.email))
	exec(`UPDATE comments SET embedding = $2::halfvec WHERE id = $1`, w.mention, vec)

	// The project's description, a component's
	// description (both searchable and embedded) and an attachment's file
	// name, each naming him.
	exec(`UPDATE projects SET description = $2, embedding = $3::halfvec WHERE id = $1`, p.ID,
		"owner contact: "+w.email+" @"+w.username, vec)
	w.component = testsupport.SeedComponent(t, pool, p.ID, other.ID, "addr"+w.word[len(w.word)-6:], "ask "+w.email)
	exec(`UPDATE components SET embedding = $2::halfvec WHERE id = $1`, w.component, vec)
	w.attachment = testsupport.SeedAttachment(t, pool, p.IssueID, other.ID, "ready")
	exec(`UPDATE attachments SET filename = $2 WHERE id = $1`, w.attachment, "notes "+w.email+".txt")

	// Free text of an issue: his e-mail, his mention, and the mention of
	// another username that merely begins with his — it must stay.
	exec(`UPDATE issues SET description = $2, embedding = $3::halfvec WHERE id = $1`, p.IssueID,
		"contact "+w.email+" or @"+w.username+"-x, cc @"+w.username, vec)

	// Events: an assignee change by another actor, an issue snapshot by
	// another actor carrying his e-mail as assignee, his own event with
	// his name, and a control whose non-person field equals his username.
	insertEvent := func(actor string, actorID uuid.UUID, field string, oldData, newData any) int64 {
		t.Helper()
		var oldJSON, newJSON *string
		if oldData != nil {
			b, _ := json.Marshal(oldData)
			s := string(b)
			oldJSON = &s
		}
		if newData != nil {
			b, _ := json.Marshal(newData)
			s := string(b)
			newJSON = &s
		}
		var fieldArg any
		if field != "" {
			fieldArg = field
		}
		var id int64
		if err := pool.QueryRow(ctx, `
INSERT INTO events (type, actor, actor_user_id, actor_type, entity_type, entity_id, project_id, field, old_data, new_data, payload, published_at)
VALUES ('issue.field_changed', $1, $2, 'human', 'issue', $3, $4, $5, $6::jsonb, $7::jsonb, '{}'::jsonb, now())
RETURNING id`, actor, actorID, p.IssueID, p.ID, fieldArg, oldJSON, newJSON).Scan(&id); err != nil {
			t.Fatalf("seed event: %v", err)
		}
		return id
	}
	w.assigneeEvent = insertEvent(other.Username, other.ID, "assignee", w.username, "someone-else")
	insertEvent(other.Username, other.ID, "", nil, map[string]any{"assignee": w.email, "title": "a task"})
	w.ownEvent = insertEvent(w.username, w.d, "", nil, map[string]any{"author": w.name, "body": "hello"})
	w.control = insertEvent(other.Username, other.ID, "", nil, map[string]any{"title": w.username})

	// A commit authored by him, linked by another.
	exec(`
INSERT INTO issue_commits (issue_id, repo_id, hash, author, linked_by, linked_by_kind)
VALUES ($1, $2, 'abc1234', $3, $4, 'human')`, p.IssueID, uuid.New(), w.name+" <"+w.email+">", other.ID)

	// His agent: a key labelled by his name, a session row.
	plain, prefix, hash, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatal(err)
	}
	w.keyPlain = plain
	if w.keyID, _, err = apikeys.New(pool).Create(ctx, w.d, w.name+" laptop", prefix, hash); err != nil {
		t.Fatalf("create key: %v", err)
	}
	exec(`INSERT INTO agents (user_id, session_id) VALUES ($1, 'deleted-test-session')`, w.d)

	// Personal state: a push subscription, a watch, a reminder, an
	// unfinished email.
	testsupport.SeedPushSubscription(t, pool, w.d)
	exec(`INSERT INTO issue_watchers (issue_id, user_id, reasons) VALUES ($1, $2, '{manual}') ON CONFLICT DO NOTHING`, p.IssueID, w.d)
	testsupport.SeedReminder(t, pool, p.IssueID, w.d)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.QueueConfirmationInTx(ctx, tx, w.d, w.email, "en", time.Hour); err != nil {
		t.Fatalf("queue an email: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// A pending invitation to his e-mail in space B, its email still
	// unsent (the deletion's cancellation makes it final), and an email
	// to him already delivered.
	if err := pool.QueryRow(ctx, `
INSERT INTO space_invitations (space_id, email, invited_by, lang, expires_at)
VALUES ($1, $2, $3, 'en', now() + interval '1 day') RETURNING id`,
		w.f.SpaceB, w.email, w.f.User(t, testsupport.ActorOnlyB).ID).Scan(&w.invitation); err != nil {
		t.Fatalf("seed invitation: %v", err)
	}
	exec(`
INSERT INTO outgoing_emails (kind, recipient, lang, invitation_id, deadline_at)
VALUES ('space_invite', $1, 'en', $2, now() + interval '1 day')`, w.email, w.invitation)
	exec(`
INSERT INTO outgoing_emails (kind, recipient, lang, user_id, state, outcome, deadline_at, finalized_at)
VALUES ('password_reset', $1, 'en', $2, 'final', 'success', now(), now())`, w.email, w.d)
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM auth_tokens WHERE invitation_id = $1`, w.invitation)
		_, _ = pool.Exec(c, `DELETE FROM outgoing_emails WHERE invitation_id = $1`, w.invitation)
		_, _ = pool.Exec(c, `DELETE FROM space_invitations WHERE id = $1`, w.invitation)
		_, _ = pool.Exec(c, `DELETE FROM issue_commits WHERE issue_id = $1`, p.IssueID)
		_, _ = pool.Exec(c, `DELETE FROM events WHERE id IN ($1, $2, $3)`, w.assigneeEvent, w.ownEvent, w.control)
	})
	return w
}

// del deletes uAB's account through DELETE /me.
func (w *deletedWorld) del(t *testing.T) {
	t.Helper()
	if a := w.rig.deleteMe(t, w.d, deletePassword); a.status != http.StatusNoContent {
		t.Fatalf("DELETE /me = %d %s, want 204", a.status, a.raw)
	}
}

func (w *deletedWorld) pseudonym(t *testing.T) string {
	t.Helper()
	var p *string
	if err := w.pool.QueryRow(context.Background(), `SELECT pseudonym FROM users WHERE id = $1`, w.d).Scan(&p); err != nil {
		t.Fatal(err)
	}
	if p == nil {
		t.Fatal("the deleted account has no pseudonym")
	}
	return *p
}

// TestAccountDelete_SpacesAndEveryStoreOfK21 — row 177. After the 204: the
// "Personal" and the space he owned alone are gone with their projects; he is
// in no space and no project; his open assignment is removed (US-7) and the
// closed one names the pseudonym; every snapshot of K-21 names the
// pseudonym and no longer his e-mail, username or name; the mention of a
// longer username is untouched, as is a non-person field equal to his
// username; his personal state, unfinished email and the invitation to him
// are gone or revoked.
func TestAccountDelete_SpacesAndEveryStoreOfK21(t *testing.T) {
	w := newDeletedWorld(t)
	ctx := context.Background()
	pool := w.pool
	w.del(t)
	pseudo := w.pseudonym(t)
	if !pseudonymShape.MatchString(pseudo) {
		t.Fatalf("pseudonym %q is not deleted-<8 hex>", pseudo)
	}

	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}
	str := func(sql string, args ...any) string {
		t.Helper()
		var s *string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		if s == nil {
			return ""
		}
		return *s
	}

	t.Run("spaces", func(t *testing.T) {
		if n := count(`SELECT count(*) FROM spaces WHERE id = $1 OR (owner_id = $2 AND kind = 'personal')`, w.solo, w.d); n != 0 {
			t.Errorf("%d of his personal and solo spaces remain", n)
		}
		if n := count(`SELECT count(*) FROM projects WHERE id = ANY($1)`, []uuid.UUID{w.soloProject, w.personalProject}); n != 0 {
			t.Errorf("%d projects of the deleted spaces remain", n)
		}
		if n := count(`SELECT count(*) FROM space_members WHERE user_id = $1`, w.d); n != 0 {
			t.Errorf("still a participant of %d spaces", n)
		}
		if n := count(`SELECT count(*) FROM project_members WHERE user_id = $1`, w.d); n != 0 {
			t.Errorf("still a member of %d projects", n)
		}
		if n := count(`SELECT count(*) FROM spaces WHERE id = ANY($1)`, []uuid.UUID{w.f.SpaceA, w.f.SpaceB}); n != 2 {
			t.Errorf("the spaces he left: %d of 2 remain", n)
		}
	})

	t.Run("account", func(t *testing.T) {
		var email, username, name string
		var hash *string
		if err := pool.QueryRow(ctx, `SELECT email, username, name, password_hash FROM users WHERE id = $1`, w.d).
			Scan(&email, &username, &name, &hash); err != nil {
			t.Fatal(err)
		}
		if email != pseudo || username != pseudo || name != pseudo {
			t.Errorf("account = %q %q %q, want the pseudonym %q in all three", email, username, name, pseudo)
		}
		if hash != nil {
			t.Error("the password hash is kept")
		}
		if n := count(`
SELECT count(*) FROM reserved_identities
 WHERE (kind = 'email' AND value_hash = sha256(convert_to($1, 'UTF8')))
    OR (kind = 'username' AND value_hash = sha256(convert_to($2, 'UTF8')))`, w.email, w.username); n != 2 {
			t.Errorf("%d of the 2 identity hashes reserved", n)
		}
	})

	t.Run("assignee text", func(t *testing.T) {
		if a := str(`SELECT assignee FROM issues WHERE id = $1`, w.openIssue); a != "" {
			t.Errorf("open issue assignee = %q, want removed (US-7)", a)
		}
		if a := str(`SELECT assignee FROM issues WHERE id = $1`, w.closedIssue); a != pseudo {
			t.Errorf("closed issue assignee = %q, want the pseudonym", a)
		}
		if id := str(`SELECT assignee_user_id::text FROM issues WHERE id = $1`, w.closedIssue); id != w.d.String() {
			t.Errorf("closed issue resolves to %q, want the deleted account (E-22)", id)
		}
	})

	t.Run("comments", func(t *testing.T) {
		if a := str(`SELECT author FROM comments WHERE id = $1`, w.ownComment); a != pseudo {
			t.Errorf("his comment's author = %q, want the pseudonym", a)
		}
		body := str(`SELECT body FROM comments WHERE id = $1`, w.mention)
		if strings.Contains(strings.ToLower(body), w.email) || strings.Contains(body, "@"+w.username) ||
			!strings.Contains(body, "@"+pseudo) {
			t.Errorf("mentioning comment = %q, want his e-mail and mention replaced by the pseudonym", body)
		}
		if n := count(`SELECT count(*) FROM comments WHERE id = $1 AND embedding IS NOT NULL`, w.mention); n != 0 {
			t.Error("the rewritten comment keeps its old embedding")
		}
	})

	t.Run("issue text", func(t *testing.T) {
		p := w.byKey[keyAMembers]
		desc := str(`SELECT description FROM issues WHERE id = $1`, p.IssueID)
		want := "contact " + pseudo + " or @" + w.username + "-x, cc @" + pseudo
		if desc != want {
			t.Errorf("description = %q, want %q", desc, want)
		}
		if n := count(`SELECT count(*) FROM issues WHERE id = $1 AND embedding IS NOT NULL`, p.IssueID); n != 0 {
			t.Error("the rewritten issue keeps its old embedding")
		}
	})

	t.Run("projects, components, attachments", func(t *testing.T) {
		p := w.byKey[keyAMembers]
		if d := str(`SELECT description FROM projects WHERE id = $1`, p.ID); d != "owner contact: "+pseudo+" @"+pseudo {
			t.Errorf("project description = %q, want his e-mail and mention replaced", d)
		}
		if d := str(`SELECT description FROM components WHERE id = $1`, w.component); d != "ask "+pseudo {
			t.Errorf("component description = %q, want his e-mail replaced", d)
		}
		if n := count(`SELECT count(*) FROM projects WHERE id = $1 AND embedding IS NOT NULL`, p.ID) +
			count(`SELECT count(*) FROM components WHERE id = $1 AND embedding IS NOT NULL`, w.component); n != 0 {
			t.Errorf("%d rewritten project/component rows keep their old embedding", n)
		}
		if f := str(`SELECT filename FROM attachments WHERE id = $1`, w.attachment); f != "notes "+pseudo+".txt" {
			t.Errorf("attachment file name = %q, want his e-mail replaced", f)
		}
	})

	t.Run("events", func(t *testing.T) {
		if n := count(`
SELECT count(*) FROM events
 WHERE strpos(lower(concat(actor, old_data::text, new_data::text, payload::text, reason)), $1) > 0`, w.email); n != 0 {
			t.Errorf("%d events still carry his e-mail", n)
		}
		if n := count(`
SELECT count(*) FROM events
 WHERE strpos(concat(actor, old_data::text, new_data::text, payload::text, reason), $1) > 0`, w.name); n != 0 {
			t.Errorf("%d events still carry his name", n)
		}
		if n := count(`SELECT count(*) FROM events WHERE actor_user_id = $1 AND actor <> $2`, w.d, pseudo); n != 0 {
			t.Errorf("%d of his events keep another actor than the pseudonym", n)
		}
		if old := str(`SELECT old_data::text FROM events WHERE id = $1`, w.assigneeEvent); old != `"`+pseudo+`"` {
			t.Errorf("old assignee of the change = %s, want the pseudonym", old)
		}
		if own := str(`SELECT new_data->>'author' FROM events WHERE id = $1`, w.ownEvent); own != pseudo {
			t.Errorf("his own event's author = %q, want the pseudonym", own)
		}
		if title := str(`SELECT new_data->>'title' FROM events WHERE id = $1`, w.control); title != w.username {
			t.Errorf("a non-person field = %q, want untouched %q", title, w.username)
		}
		rows, err := pool.Query(ctx, `
SELECT type, coalesce(field, ''), concat(old_data::text, ' → ', new_data::text, ' · ', payload::text) FROM events
 WHERE id <> $2 AND strpos(concat(old_data::text, new_data::text, payload::text), to_json($1::text)::text) > 0`,
			w.username, w.control)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var typ, field, data string
			_ = rows.Scan(&typ, &field, &data)
			t.Errorf("event %s (field %q) still carries his username as a value: %s", typ, field, data)
		}
		rows.Close()
	})

	t.Run("commits and agent labels", func(t *testing.T) {
		if a := str(`SELECT author FROM issue_commits WHERE issue_id = $1 AND hash = 'abc1234'`, w.byKey[keyAMembers].IssueID); a != pseudo {
			t.Errorf("commit author = %q, want the pseudonym", a)
		}
		if l := str(`SELECT label FROM api_keys WHERE id = $1`, w.keyID); l != pseudo {
			t.Errorf("key label = %q, want the pseudonym", l)
		}
		if n := count(`SELECT count(*) FROM api_keys WHERE user_id = $1 AND revoked_at IS NULL`, w.d); n != 0 {
			t.Errorf("%d keys still active", n)
		}
	})

	t.Run("personal state", func(t *testing.T) {
		for table, sql := range map[string]string{
			"push subscriptions": `SELECT count(*) FROM push_subscriptions WHERE user_id = $1`,
			"agent sessions":     `SELECT count(*) FROM agents WHERE user_id = $1`,
			"watches":            `SELECT count(*) FROM issue_watchers WHERE user_id = $1`,
			"reminders":          `SELECT count(*) FROM reminders WHERE user_id = $1`,
			"tokens":             `SELECT count(*) FROM auth_tokens WHERE user_id = $1`,
		} {
			if n := count(sql, w.d); n != 0 {
				t.Errorf("%s: %d rows remain", table, n)
			}
		}
		// Every email of him or to his address — the delivered one and the
		// invitation email the deletion cancelled included.
		if n := count(`SELECT count(*) FROM outgoing_emails WHERE user_id = $1 OR recipient = $2`, w.d, w.email); n != 0 {
			t.Errorf("emails: %d rows of his or to his address remain", n)
		}
	})

	t.Run("invitation", func(t *testing.T) {
		var status, email string
		if err := pool.QueryRow(ctx, `SELECT status, email FROM space_invitations WHERE id = $1`, w.invitation).
			Scan(&status, &email); err != nil {
			t.Fatal(err)
		}
		if status != "revoked" || email != pseudo {
			t.Errorf("invitation = %s %q, want revoked to the pseudonym", status, email)
		}
	})
}

// TestAccountDelete_NoTraceSessionsKeysReregistration — row 178. Before the
// deletion the reader finds the issue by the e-mail (the check can see it);
// after the 204 no answer of the people surfaces of row 121 nor the search
// carries the e-mail or the name; the e-mail and the username register
// neither by signup nor by the admin; the session (old and current epoch)
// and the API key are refused.
func TestAccountDelete_NoTraceSessionsKeysReregistration(t *testing.T) {
	w := newDeletedWorld(t)
	ctx := context.Background()
	reader := testsupport.ActorSpaceOwner
	p := w.byKey[keyAMembers]
	search := "/api/v1/search?q=" + url.QueryEscape(w.email)

	if r := w.get(reader, search); r.status != http.StatusOK || !bytes.Contains(r.body, []byte(p.IssueID)) ||
		!bytes.Contains(r.body, []byte(w.component.String())) {
		t.Fatalf("before the deletion the search by e-mail does not find %s and the component: %d %s", p.IssueID, r.status, r.body)
	}
	keyChain := auth.RequireAPIKey(w.pool, auth.MinBcryptCost, "")(
		http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) { rw.WriteHeader(http.StatusOK) }))
	keyRequest := func() int {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set(auth.HeaderAPIKey, w.keyPlain)
		req.Header.Set(auth.HeaderSurface, "internal")
		rec := httptest.NewRecorder()
		keyChain.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := keyRequest(); code != http.StatusOK {
		t.Fatalf("the key before the deletion = %d, want 200", code)
	}
	before := w.rig.epoch(t, w.d)

	w.del(t)

	t.Run("no answer carries the e-mail or the name", func(t *testing.T) {
		var spaceShort string
		if err := w.pool.QueryRow(ctx, `SELECT short_id FROM spaces WHERE id = $1`, w.f.SpaceA).Scan(&spaceShort); err != nil {
			t.Fatal(err)
		}
		base := "/api/v1/projects/" + p.ID.String()
		surfaces := map[string]string{
			"directory":        "/api/v1/users/directory?limit=100",
			"project members":  base + "/members",
			"space":            "/api/v1/spaces/" + spaceShort,
			"issue card":       base + "/issues/" + p.IssueID + "/",
			"comments":         base + "/issues/" + p.IssueID + "/comments",
			"watchers":         "/api/v1/issues/" + p.IssueID + "/watchers",
			"issue activity":   base + "/issues/" + p.IssueID + "/activity",
			"project activity": base + "/activity",
			"project":          base + "/",
			"attachments":      "/api/v1/issues/" + p.IssueID + "/attachments",
			"search (cached)":  search,
			"search (fresh)":   "/api/v1/search?q=" + url.QueryEscape(strings.ToUpper(w.email)),
			"search by name":   "/api/v1/search?q=" + url.QueryEscape(w.name),
		}
		for name, path := range surfaces {
			t.Run(name, func(t *testing.T) {
				r := w.get(reader, path)
				if r.status != http.StatusOK {
					t.Fatalf("GET %s: %d %s", path, r.status, r.body)
				}
				body := bytes.ToLower(r.body)
				if bytes.Contains(body, []byte(w.email)) {
					t.Errorf("%s carries the deleted e-mail", name)
				}
				if bytes.Contains(body, []byte(strings.ToLower(w.name))) {
					t.Errorf("%s carries the deleted name", name)
				}
			})
		}
	})

	t.Run("the e-mail and the username register no more", func(t *testing.T) {
		env := newSignupEnv(t, signupOpts{})
		fresh := "fresh" + uniq()
		if a := env.signup(t, map[string]any{
			"email": strings.ToUpper(w.email), "username": fresh, "password": signupGoodPassword,
			"termsAccepted": true, "language": "en",
		}, ""); a.code() != accounts.CodeEmailTaken {
			t.Errorf("signup with the deleted e-mail = %d %q, want email_taken", a.status, a.code())
		}
		if a := env.signup(t, map[string]any{
			"email": fresh + "@fresh.test", "username": w.username, "password": signupGoodPassword,
			"termsAccepted": true, "language": "en",
		}, ""); a.code() != accounts.CodeUsernameTaken {
			t.Errorf("signup with the deleted username = %d %q, want username_taken", a.status, a.code())
		}
		admin := users.Actor{UserID: w.f.User(t, testsupport.ActorAdminA).ID, Username: "admin", Role: "human"}
		if _, err := w.rig.store.CreateUser(ctx, users.CreateUserInput{
			Email: w.email, Username: fresh, Name: "x", Role: "user", Password: signupGoodPassword,
		}, admin); !errors.Is(err, users.ErrEmailTaken) {
			t.Errorf("admin create with the deleted e-mail: %v, want ErrEmailTaken", err)
		}
		if _, err := w.rig.store.CreateUser(ctx, users.CreateUserInput{
			Email: fresh + "@fresh.test", Username: w.username, Name: "x", Role: "user", Password: signupGoodPassword,
		}, admin); !errors.Is(err, users.ErrUsernameTaken) {
			t.Errorf("admin create with the deleted username: %v, want ErrUsernameTaken", err)
		}
	})

	t.Run("sessions and keys", func(t *testing.T) {
		for _, epoch := range []int64{before, w.rig.epoch(t, w.d)} {
			if a := w.rig.do(t, w.d, epoch, http.MethodGet, "/api/v1/probe", nil); a.status != http.StatusUnauthorized || a.code() != auth.CodeSessionRevoked {
				t.Errorf("session with epoch %d = %d %q, want 401 session_revoked", epoch, a.status, a.code())
			}
		}
		if code := keyRequest(); code != http.StatusUnauthorized {
			t.Errorf("the key after the deletion = %d, want 401", code)
		}
	})
}

// ---------------------------------------------------------------------------
// Row 179
// ---------------------------------------------------------------------------

// TestAccountDelete_PseudonymUniqueNeverAUsername — row 179. Two deleted
// accounts get two pseudonyms of the shape deleted-<8 hex>; neither may be
// taken as a username, by signup or by the admin.
func TestAccountDelete_PseudonymUniqueNeverAUsername(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	rig := newDeleteRig(t, pool, true)
	var pseudonyms []string
	for range 2 {
		u, _ := testsupport.SeedUser(t, pool, "user")
		deletable(t, pool, u)
		if a := rig.deleteMe(t, u, deletePassword); a.status != http.StatusNoContent {
			t.Fatalf("DELETE /me = %d %s", a.status, a.raw)
		}
		var p string
		if err := pool.QueryRow(ctx, `SELECT pseudonym FROM users WHERE id = $1`, u).Scan(&p); err != nil {
			t.Fatal(err)
		}
		if !pseudonymShape.MatchString(p) {
			t.Errorf("pseudonym %q is not deleted-<8 hex>", p)
		}
		pseudonyms = append(pseudonyms, p)
	}
	if pseudonyms[0] == pseudonyms[1] {
		t.Errorf("two deleted accounts share the pseudonym %q", pseudonyms[0])
	}

	env := newSignupEnv(t, signupOpts{})
	admin, _ := testsupport.SeedUser(t, pool, "admin")
	for _, p := range pseudonyms {
		if a := env.signup(t, map[string]any{
			"email": "p" + uniq() + "@fresh.test", "username": p, "password": signupGoodPassword,
			"termsAccepted": true, "language": "en",
		}, ""); a.code() != accounts.CodeUsernameInvalid {
			t.Errorf("signup as %q = %d %q, want username_invalid", p, a.status, a.code())
		}
		// A username of the pseudonym shape that no account carries is
		// refused all the same: the shape itself is never a username.
		other := "deleted-" + strings.Repeat(p[len(p)-1:], 8)
		for _, name := range []string{p, other} {
			if _, err := rig.store.CreateUser(ctx, users.CreateUserInput{
				Email: "p" + uniq() + "@fresh.test", Username: name, Name: "x", Role: "user", Password: signupGoodPassword,
			}, users.Actor{UserID: admin, Username: "admin", Role: "human"}); !errors.Is(err, users.ErrValidation) {
				t.Errorf("admin create as %q: %v, want a validation refusal", name, err)
			}
		}
	}
}

// TestAccountDelete_EmailReplacedOnlyAsAWholeAddress — The
// deleted account's e-mail is replaced where it stands as an address of its
// own — at the start, after a space, before a full stop — and nowhere it is
// only a part of another address: inside a longer local part (m<email>) or
// with a longer domain (<email>.org). Checked in the three places the
// rewrite reaches by different roads: issue text, a commit author and event
// data.
func TestAccountDelete_EmailReplacedOnlyAsAWholeAddress(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	rig := newDeleteRig(t, pool, true)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	u, _ := testsupport.SeedUser(t, pool, "user")
	email, _ := deletable(t, pool, u)
	space := testsupport.SeedSpace(t, pool, owner)
	testsupport.AddSpaceMember(t, pool, space, u)
	project, prefix := testsupport.SeedProjectInSpace(t, pool, owner, space, "addr")
	issue := testsupport.SeedIssue(t, pool, project, owner, prefix+"-1", "addresses", "open", 2)
	// Other people's addresses that contain his: a longer local part —
	// a letter, RFC 5322 punctuation (r2 #2), an apostrophe — or a longer
	// domain. They stay whole.
	longer := "m" + email
	bang := "!" + email
	quote := "o'" + email
	wider := email + ".org"
	if _, err := pool.Exec(ctx, `UPDATE issues SET description = $2 WHERE id = $1`, issue,
		email+" wrote; also "+longer+", "+bang+", "+quote+", "+wider+" and "+strings.ToUpper(email)+
			". Mail <"+email+"> (or ("+email+") or \""+email+"\")"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO issue_commits (issue_id, repo_id, hash, author, linked_by, linked_by_kind)
VALUES ($1, $2, 'aaa1111', $3, $4, 'human'), ($1, $2, 'bbb2222', $5, $4, 'human'),
       ($1, $2, 'ccc3333', $6, $4, 'human'), ($1, $2, 'ddd4444', $7, $4, 'human')`,
		issue, uuid.New(), "Someone <"+longer+">", owner, "Deleted <"+email+">",
		"Other <"+bang+">", "O'Brien <"+quote+">"); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]string{"description": longer + " " + bang + " " + quote + " and " + email})
	var event int64
	if err := pool.QueryRow(ctx, `
INSERT INTO events (type, actor, actor_user_id, actor_type, entity_type, entity_id, project_id, new_data, payload, published_at)
VALUES ('issue.created', 'someone', $1, 'human', 'issue', $2, $3, $4::jsonb, '{}'::jsonb, now()) RETURNING id`,
		owner, issue, project, string(data)).Scan(&event); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE id = $1`, event) })

	if a := rig.deleteMe(t, u, deletePassword); a.status != http.StatusNoContent {
		t.Fatalf("DELETE /me = %d %s", a.status, a.raw)
	}
	var pseudo, desc, got string
	if err := pool.QueryRow(ctx, `SELECT pseudonym FROM users WHERE id = $1`, u).Scan(&pseudo); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT description FROM issues WHERE id = $1`, issue).Scan(&desc); err != nil {
		t.Fatal(err)
	}
	if want := pseudo + " wrote; also " + longer + ", " + bang + ", " + quote + ", " + wider + " and " + pseudo +
		". Mail <" + pseudo + "> (or (" + pseudo + ") or \"" + pseudo + "\")"; desc != want {
		t.Errorf("description = %q, want %q", desc, want)
	}
	authors := map[string]string{}
	rows, err := pool.Query(ctx, `SELECT hash, author FROM issue_commits WHERE issue_id = $1`, issue)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var h, a string
		_ = rows.Scan(&h, &a)
		authors[h] = a
	}
	rows.Close()
	for hash, want := range map[string]string{
		"aaa1111": "Someone <" + longer + ">", "ccc3333": "Other <" + bang + ">", "ddd4444": "O'Brien <" + quote + ">",
	} {
		if authors[hash] != want {
			t.Errorf("another person's commit author %s = %q, want untouched", hash, authors[hash])
		}
	}
	if authors["bbb2222"] != pseudo {
		t.Errorf("his commit author = %q, want the pseudonym", authors["bbb2222"])
	}
	if err := pool.QueryRow(ctx, `SELECT new_data->>'description' FROM events WHERE id = $1`, event).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if want := longer + " " + bang + " " + quote + " and " + pseudo; got != want {
		t.Errorf("event data = %q, want %q", got, want)
	}
}

// TestAccountDelete_CommitAuthorByAddressNotByName — A
// commit author carrying the deleted account's address is its snapshot and
// becomes the pseudonym; an author string merely equal to its name or to
// its username may be another person — a namesake — and stays.
func TestAccountDelete_CommitAuthorByAddressNotByName(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	rig := newDeleteRig(t, pool, true)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	u, _ := testsupport.SeedUser(t, pool, "user")
	email, username := deletable(t, pool, u)
	name := "Ivan Petrov " + uniq()
	if _, err := pool.Exec(ctx, `UPDATE users SET name = $2 WHERE id = $1`, u, name); err != nil {
		t.Fatal(err)
	}
	project, prefix := testsupport.SeedProject(t, pool, owner, "cma")
	issue := testsupport.SeedIssue(t, pool, project, owner, prefix+"-1", "commits", "open", 2)
	if _, err := pool.Exec(ctx, `
INSERT INTO issue_commits (issue_id, repo_id, hash, author, linked_by, linked_by_kind)
VALUES ($1, $2, 'c0ffee1', $3, $4, 'human'), ($1, $2, 'c0ffee2', $5, $4, 'human'), ($1, $2, 'c0ffee3', $6, $4, 'human')`,
		issue, uuid.New(), name, owner, username, name+" <"+email+">"); err != nil {
		t.Fatal(err)
	}
	if a := rig.deleteMe(t, u, deletePassword); a.status != http.StatusNoContent {
		t.Fatalf("DELETE /me = %d %s", a.status, a.raw)
	}
	var pseudo string
	if err := pool.QueryRow(ctx, `SELECT pseudonym FROM users WHERE id = $1`, u).Scan(&pseudo); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"c0ffee1": name, "c0ffee2": username, "c0ffee3": pseudo}
	rows, err := pool.Query(ctx, `SELECT hash, author FROM issue_commits WHERE issue_id = $1`, issue)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var h, a string
		_ = rows.Scan(&h, &a)
		if a != want[h] {
			t.Errorf("commit %s author = %q, want %q", h, a, want[h])
		}
		delete(want, h)
	}
	if len(want) != 0 {
		t.Errorf("commits missing: %v", want)
	}
}

// TestAccountDelete_EarlierNamesLeaveTheAccountEvents —
// The admin renames the account (user.updated with its change records and
// user.field_changed with old and new values), then the account is deleted:
// no event of the account keeps the earlier or the later name, its e-mail
// or its username; the admin who acted keeps his own name as the actor.
func TestAccountDelete_EarlierNamesLeaveTheAccountEvents(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	rig := newDeleteRig(t, pool, true)
	u, _ := testsupport.SeedUser(t, pool, "user")
	email, username := deletable(t, pool, u)
	admin, adminName := testsupport.SeedUser(t, pool, "admin")
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE actor_user_id = $1`, admin) })
	first, second := "First Name "+uniq(), "Second Name "+uniq()
	adminActor := users.Actor{UserID: admin, Username: adminName, Role: "human"}
	for _, name := range []string{first, second} {
		n := name
		if _, err := rig.store.UpdateUser(ctx, u, users.UpdateUserInput{Name: &n}, adminActor); err != nil {
			t.Fatalf("rename to %q: %v", name, err)
		}
	}
	var renames int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM events WHERE entity_type = 'user' AND entity_id = $1::text
   AND concat(old_data::text, new_data::text, payload::text) LIKE '%' || $2 || '%'`, u, first).Scan(&renames); err != nil {
		t.Fatal(err)
	}
	if renames == 0 {
		t.Fatal("no event of the account carries the earlier name — the check would prove nothing")
	}

	if a := rig.deleteMe(t, u, deletePassword); a.status != http.StatusNoContent {
		t.Fatalf("DELETE /me = %d %s", a.status, a.raw)
	}
	rows, err := pool.Query(ctx, `
SELECT type, actor, actor_user_id, concat(old_data::text, ' ', new_data::text, ' ', payload::text)
  FROM events WHERE entity_type = 'user' AND entity_id = $1::text`, u)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var seen int
	for rows.Next() {
		var typ, actor, data string
		var actorID *uuid.UUID
		if err := rows.Scan(&typ, &actor, &actorID, &data); err != nil {
			t.Fatal(err)
		}
		seen++
		for _, v := range []string{first, second, email, `"` + username + `"`} {
			if strings.Contains(data, v) {
				t.Errorf("%s still carries %q: %s", typ, v, data)
			}
		}
		if actorID != nil && *actorID == admin && actor != adminName {
			t.Errorf("%s by the admin: actor %q, want the admin's own %q", typ, actor, adminName)
		}
	}
	if seen == 0 {
		t.Fatal("no account events read")
	}
}

// TestAccountDelete_DeletedAccountIsTerminal — After the
// deletion no admin mutation reaches the account: reactivation, an update
// (a new password, a name) and a deactivation answer «not found», the same
// as for an id nobody has, and the row stays as the deletion left it.
func TestAccountDelete_DeletedAccountIsTerminal(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	rig := newDeleteRig(t, pool, true)
	u, _ := testsupport.SeedUser(t, pool, "user")
	deletable(t, pool, u)
	admin, adminName := testsupport.SeedUser(t, pool, "admin")
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE actor_user_id = $1`, admin) })
	actor := users.Actor{UserID: admin, Username: adminName, Role: "human"}
	if a := rig.deleteMe(t, u, deletePassword); a.status != http.StatusNoContent {
		t.Fatalf("DELETE /me = %d %s", a.status, a.raw)
	}
	state := func() string {
		var s string
		if err := pool.QueryRow(ctx, `
SELECT concat_ws('|', name, deactivated_at IS NOT NULL, password_hash IS NULL, deleted_at IS NOT NULL) FROM users WHERE id = $1`, u).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := state()
	name, password := "Back Again", "a brand new password 1"
	for op, err := range map[string]error{
		"reactivate": rig.store.Reactivate(ctx, u, actor),
		"update name": func() error {
			_, err := rig.store.UpdateUser(ctx, u, users.UpdateUserInput{Name: &name}, actor)
			return err
		}(),
		"set password": func() error {
			_, err := rig.store.UpdateUser(ctx, u, users.UpdateUserInput{Password: &password}, actor)
			return err
		}(),
		"deactivate": rig.store.Deactivate(ctx, u, actor),
	} {
		if !errors.Is(err, users.ErrUserNotFound) {
			t.Errorf("%s a deleted account: %v, want ErrUserNotFound", op, err)
		}
	}
	if after := state(); after != before {
		t.Errorf("the deleted account changed: %s → %s", before, after)
	}
}

// holdRow locks one row in a transaction of its own and returns it with
// the backend pid; the caller rolls it back to release.
func holdRow(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) (pgx.Tx, int32) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	var pid int32
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("hold %q: %v", sql, err)
	}
	return tx, pid
}

// waitBlockedBy waits until n backends wait on the backend pid, directly or
// queued behind one another (the second writer of a row waits for the
// first one's tuple lock). A wait for a database condition; the deadline
// only turns a hang into a failure.
func waitBlockedBy(t *testing.T, pool *pgxpool.Pool, pid int32, n int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var blocked int
		if err := pool.QueryRow(context.Background(), `
WITH RECURSIVE waiters(pid) AS (
    SELECT pid FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid))
    UNION
    SELECT a.pid FROM pg_stat_activity a JOIN waiters w ON w.pid = ANY(pg_blocking_pids(a.pid)))
SELECT count(*) FROM waiters`, pid).Scan(&blocked); err != nil {
			t.Fatalf("count blocked backends: %v", err)
		}
		if blocked >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %d backends blocked by %d: last seen %d", n, pid, blocked)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAccountDelete_TwoTabsOneAccount — Two DELETE /me
// of one account pass the session gate and the password, then meet on the
// account's row (held by the test until both wait on it). One deletes the
// account (204); the other finds it deleted and answers what the session
// gate answers a deleted account — 401 session_revoked — never a 500.
func TestAccountDelete_TwoTabsOneAccount(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rig := newDeleteRig(t, pool, true)
	u, _ := testsupport.SeedUser(t, pool, "user")
	deletable(t, pool, u)
	epoch := rig.epoch(t, u)
	holder, pid := holdRow(t, pool, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, u)

	answers := make(chan deleteAnswer, 2)
	for range 2 {
		go func() {
			answers <- rig.do(t, u, epoch, http.MethodDelete, "/api/v1/me", map[string]string{"password": deletePassword})
		}()
	}
	waitBlockedBy(t, pool, pid, 2)
	if err := holder.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for range 2 {
		a := <-answers
		got[strconv.Itoa(a.status)+" "+a.code()]++
	}
	if got["204 "] != 1 || got["401 "+auth.CodeSessionRevoked] != 1 {
		t.Errorf("two concurrent deletions answered %v, want one 204 and one 401 session_revoked", got)
	}
	if !isDeleted(t, pool, u) {
		t.Error("the account is not deleted")
	}
}

// TestAccountDelete_SSOProvisioningRefusesTheReservedIdentity — review
// api/10 #2 (INV-18). An account created by an SSO login is deleted (the
// operator's road: no password, whatever SKEIN_SIGNUP_ENABLED). The same
// person logs in again — with a subject (keycloak, hybrid) and without one
// (trust headers) — and gets 403 user_not_provisioned; no account with the
// email exists. A new email arriving with the deleted username gets an
// account, but not that username.
func TestAccountDelete_SSOProvisioningRefusesTheReservedIdentity(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	rig := newDeleteRig(t, pool, false)
	h, err := NewAuthHandler(pool, auth.MinBcryptCost, "keycloak", "", auth.NewProvisioner(pool, true, "user", ""))
	if err != nil {
		t.Fatal(err)
	}
	tag := strings.ToLower(uniq())
	sub, email, username := "sub-"+tag, "sso"+tag+"@sso.test", "sso"+tag
	forget := func(id uuid.UUID) {
		t.Cleanup(func() {
			c := context.Background()
			_, _ = pool.Exec(c, `DELETE FROM events WHERE actor_user_id = $1 OR (entity_type = 'user' AND entity_id = $1::text)`, id)
			_, _ = pool.Exec(c, `DELETE FROM space_members WHERE user_id = $1`, id)
			_, _ = pool.Exec(c, `DELETE FROM spaces WHERE owner_id = $1`, id)
			_, _ = pool.Exec(c, `DELETE FROM users WHERE id = $1`, id)
		})
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `
DELETE FROM reserved_identities
 WHERE (kind = 'email' AND value_hash = sha256(convert_to($1, 'UTF8')))
    OR (kind = 'username' AND value_hash = sha256(convert_to($2, 'UTF8')))`, email, username)
	})
	provision := func(sub, email, username string) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"sub": sub, "email": email, "username": username, "name": ""})
		rec := httptest.NewRecorder()
		h.OIDCProvision(rec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/oidc/provision", bytes.NewReader(raw)))
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}

	code, body := provision(sub, email, username)
	if code != http.StatusOK {
		t.Fatalf("first SSO login = %d %v", code, body)
	}
	id, err := uuid.Parse(body["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	forget(id)
	if err := rig.store.DeleteAccount(ctx, id, "", users.Actor{Username: "operator", Role: "system"}); err != nil {
		t.Fatalf("deleting the SSO account: %v", err)
	}

	for mode, s := range map[string]string{"with a subject (keycloak, hybrid)": sub, "without a subject (trust headers)": ""} {
		code, body := provision(s, strings.ToUpper(email), username)
		if code != http.StatusForbidden || body["error"] != "user_not_provisioned" {
			t.Errorf("login %s with the deleted email = %d %v, want 403 user_not_provisioned", mode, code, body)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE lower(email) = $1`, email).Scan(&n); err != nil || n != 0 {
		t.Errorf("accounts with the deleted email: %d %v, want none", n, err)
	}

	code, body = provision("", "other"+tag+"@sso.test", username)
	if code != http.StatusOK {
		t.Fatalf("SSO login of a new email = %d %v", code, body)
	}
	if other, err := uuid.Parse(body["id"].(string)); err == nil {
		forget(other)
	}
	if got := body["username"]; got == username {
		t.Errorf("the new account got the deleted username %q", got)
	}
}

// TestAccountDelete_SpaceCreationAuthenticatedBeforeTheDeletion — review
// api/10 #4. A POST /spaces passes the session gate, then the account is
// deleted before the creating transaction gets the account's row: the test
// holds the row until the deletion and then the creation wait on it, in
// that order. The creation, run after the deletion, refuses with 401
// session_revoked; the deleted account owns no space, is in none, and no
// creation event is written.
func TestAccountDelete_SpaceCreationAuthenticatedBeforeTheDeletion(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	rig := newDeleteRig(t, pool, true)
	u, _ := testsupport.SeedUser(t, pool, "user")
	deletable(t, pool, u)
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM space_members WHERE space_id IN (SELECT id FROM spaces WHERE owner_id = $1)`, u)
		_, _ = pool.Exec(c, `DELETE FROM spaces WHERE owner_id = $1`, u)
	})
	spacesH := NewSpacesHandler(spaces.NewService(pool, spaces.Options{Creation: spaces.CreationAll}))
	r := chi.NewRouter()
	r.Use(ParseUserHeaders, RequireSessionEpoch(pool), RequireUser)
	r.Post("/api/v1/spaces", spacesH.Create)
	epoch := rig.epoch(t, u)

	holder, pid := holdRow(t, pool, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, u)
	deleted := make(chan deleteAnswer, 1)
	go func() {
		deleted <- rig.do(t, u, epoch, http.MethodDelete, "/api/v1/me", map[string]string{"password": deletePassword})
	}()
	waitBlockedBy(t, pool, pid, 1)
	created := make(chan deleteAnswer, 1)
	go func() {
		raw, _ := json.Marshal(map[string]string{"name": "late space"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/spaces", bytes.NewReader(raw))
		req.Header.Set(auth.HeaderUserID, u.String())
		req.Header.Set(auth.HeaderUsername, "whoever")
		req.Header.Set(auth.HeaderUserRole, "user")
		req.Header.Set(auth.HeaderSessionEpoch, strconv.FormatInt(epoch, 10))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		a := deleteAnswer{status: rec.Code, raw: rec.Body.Bytes()}
		_ = json.Unmarshal(a.raw, &a.body)
		created <- a
	}()
	waitBlockedBy(t, pool, pid, 2)
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if a := <-deleted; a.status != http.StatusNoContent {
		t.Fatalf("DELETE /me = %d %s", a.status, a.raw)
	}
	// The body of the session gate exactly — the code and nothing that
	// tells why.
	if a := <-created; a.status != http.StatusUnauthorized || a.code() != auth.CodeSessionRevoked || len(a.body) != 1 {
		t.Errorf("the creation after the deletion = %d %s, want 401 {\"error\":\"session_revoked\"}", a.status, a.raw)
	}
	var owned, member, events int
	if err := pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM spaces WHERE owner_id = $1),
       (SELECT count(*) FROM space_members WHERE user_id = $1),
       (SELECT count(*) FROM events WHERE entity_type = 'space' AND payload->>'change' = 'created' AND payload->>'userId' = $1::text)`,
		u).Scan(&owned, &member, &events); err != nil {
		t.Fatal(err)
	}
	if owned+member+events != 0 {
		t.Errorf("after the deletion: owns %d spaces, in %d, %d creation events — want none", owned, member, events)
	}
}

// failDeletionAfterWrites runs DeleteAccount on user and makes it fail at
// its last statement, after every other write: a transaction of the test
// inserts the reservation of the account's email first and holds it, the
// deletion's own insert of the same reservation waits on it (seen in
// pg_blocking_pids), and the deletion's context is cancelled. No hook in
// the product: a real lock and a cancelled request. Returns the deletion's
// error.
func failDeletionAfterWrites(t *testing.T, pool *pgxpool.Pool, store *users.Store, user uuid.UUID, email string) error {
	t.Helper()
	holder, pid := holdRow(t, pool, `
INSERT INTO reserved_identities (kind, value_hash) VALUES ('email', sha256(convert_to(lower(btrim($1)), 'UTF8')))`, email)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- store.DeleteAccount(ctx, user, "", users.Actor{Username: "operator", Role: "system"}) }()
	waitBlockedBy(t, pool, pid, 1)
	cancel()
	err := <-done
	if rbErr := holder.Rollback(context.Background()); rbErr != nil {
		t.Fatal(rbErr)
	}
	return err
}

func deletionGeneration(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var g int64
	if err := pool.QueryRow(context.Background(), `SELECT generation FROM account_deletion_generation WHERE id = 1`).Scan(&g); err != nil {
		t.Fatal(err)
	}
	return g
}

// TestAccountDelete_SearchCacheGeneration — The deletion
// generation of the search cache moves by one with a deletion and not at
// all with a deletion rolled back after its writes; a search cursor issued
// before a deletion is refused after it (invalid_cursor), as a page cached
// before it is not served.
func TestAccountDelete_SearchCacheGeneration(t *testing.T) {
	w := newIsoWorld(t)
	pool := w.pool
	rig := newDeleteRig(t, pool, true)
	reader := testsupport.ActorSpaceOwner

	r := w.get(reader, "/api/v1/search?limit=1&q="+url.QueryEscape(w.word))
	var page struct {
		NextCursor *string `json:"nextCursor"`
	}
	decodeInto(t, r, &page)
	if r.status != http.StatusOK || page.NextCursor == nil {
		t.Fatalf("the first page has no cursor — the check would prove nothing: %d %s", r.status, r.body)
	}
	next := "/api/v1/search?limit=1&q=" + url.QueryEscape(w.word) + "&cursor=" + url.QueryEscape(*page.NextCursor)
	if r := w.get(reader, next); r.status != http.StatusOK {
		t.Fatalf("the cursor before any deletion = %d %s, want 200", r.status, r.body)
	}

	failed, _ := testsupport.SeedUser(t, pool, "user")
	failedEmail, _ := deletable(t, pool, failed)
	before := deletionGeneration(t, pool)
	if err := failDeletionAfterWrites(t, pool, rig.store, failed, failedEmail); err == nil {
		t.Fatal("the deletion did not fail")
	}
	if g := deletionGeneration(t, pool); g != before {
		t.Errorf("a rolled back deletion moved the generation %d → %d", before, g)
	}
	if r := w.get(reader, next); r.status != http.StatusOK {
		t.Errorf("the cursor after a rolled back deletion = %d %s, want 200", r.status, r.body)
	}

	gone, _ := testsupport.SeedUser(t, pool, "user")
	deletable(t, pool, gone)
	if a := rig.deleteMe(t, gone, deletePassword); a.status != http.StatusNoContent {
		t.Fatalf("DELETE /me = %d %s", a.status, a.raw)
	}
	if g := deletionGeneration(t, pool); g != before+1 {
		t.Errorf("generation after a deletion = %d, want %d", g, before+1)
	}
	var refused struct {
		Error string `json:"error"`
	}
	r = w.get(reader, next)
	decodeInto(t, r, &refused)
	if r.status != http.StatusBadRequest || refused.Error != "invalid_cursor" {
		t.Errorf("the cursor issued before a deletion, after it = %d %s, want 400 invalid_cursor", r.status, r.body)
	}
}

// signalCatcher records the users personal signals were published to,
// through an in-process NATS server (signals.Configure).
type signalCatcher struct {
	mu    sync.Mutex
	users []uuid.UUID
}

func catchSignals(t *testing.T) *signalCatcher {
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
	c := &signalCatcher{}
	if _, err := sub.Subscribe(signals.SignalSubjectPattern, func(m *nats.Msg) {
		if id, err := uuid.Parse(strings.TrimSuffix(strings.TrimPrefix(m.Subject, "skein.user."), ".signal")); err == nil {
			c.mu.Lock()
			c.users = append(c.users, id)
			c.mu.Unlock()
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := sub.Flush(); err != nil {
		t.Fatal(err)
	}
	signals.Configure(pub)
	t.Cleanup(func() { signals.Configure(nil) })
	return c
}

// since returns the users signalled after mark, once a sentinel signal
// published after them has arrived (one connection keeps the order).
func (c *signalCatcher) since(t *testing.T, pool *pgxpool.Pool, mark int) []uuid.UUID {
	t.Helper()
	sentinel := uuid.New()
	if err := signals.BeginTxFunc(context.Background(), pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		signals.Add(tx, sentinel, signals.AreaInbox)
		_, err := tx.Exec(context.Background(), `SELECT 1`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		c.mu.Lock()
		for i := mark; i < len(c.users); i++ {
			if c.users[i] == sentinel {
				out := append([]uuid.UUID(nil), c.users[mark:i]...)
				c.mu.Unlock()
				return out
			}
		}
		c.mu.Unlock()
	}
	t.Fatal("the sentinel signal never arrived")
	return nil
}

func (c *signalCatcher) mark() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.users)
}

// TestAccountDelete_FailureAfterTheWritesRollsEverythingBack — review
// api/10 #9. A deletion that has done every write — deactivation, epoch,
// keys, the personal space and its project, leaving the other space,
// unassigning, revoking the invitation, anonymizing, the generation — and
// fails at its last statement leaves nothing of it: the account, its
// epoch, its key, its spaces and memberships, its assignment, the
// invitation, the reservations, the events, the generation are as before,
// the session and the key still work, and no personal signal is published.
func TestAccountDelete_FailureAfterTheWritesRollsEverythingBack(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	rig := newDeleteRig(t, pool, true)
	sink := catchSignals(t)
	u, _ := testsupport.SeedUser(t, pool, "user")
	email, username := deletable(t, pool, u)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	other, _ := testsupport.SeedUser(t, pool, "user")
	personal := testsupport.SeedSpaceOfKind(t, pool, u, "personal")
	personalProject, _ := testsupport.SeedProjectInSpace(t, pool, u, personal, "rbp")
	shared := testsupport.SeedSpace(t, pool, owner)
	testsupport.AddSpaceMember(t, pool, shared, u)
	project, prefix := testsupport.SeedProjectInSpace(t, pool, owner, shared, "rbs")
	testsupport.AddProjectMember(t, pool, project, u, "member")
	issue := testsupport.SeedIssue(t, pool, project, owner, prefix+"-1", "on him", "open", 2)
	testsupport.SetAssignee(t, pool, issue, username)
	elsewhere := testsupport.SeedSpace(t, pool, other)
	var invitation uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO space_invitations (space_id, email, invited_by, lang, expires_at)
VALUES ($1, $2, $3, 'en', now() + interval '1 day') RETURNING id`, elsewhere, email, other).Scan(&invitation); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM space_invitations WHERE id = $1`, invitation)
	})
	plain, keyPrefix, keyHash, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := apikeys.New(pool).Create(ctx, u, "rollback", keyPrefix, keyHash); err != nil {
		t.Fatal(err)
	}
	keyWorks := func() int {
		chain := auth.RequireAPIKey(pool, auth.MinBcryptCost, "")(
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set(auth.HeaderAPIKey, plain)
		req.Header.Set(auth.HeaderSurface, "internal")
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, req)
		return rec.Code
	}
	state := func() string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `
SELECT concat_ws(' | ',
  (SELECT concat_ws(',', email, username, name, password_hash, deleted_at IS NULL, deactivated_at IS NULL, session_epoch, pseudonym IS NULL) FROM users WHERE id = $1),
  (SELECT count(*) FROM api_keys WHERE user_id = $1 AND revoked_at IS NULL),
  (SELECT count(*) FROM spaces WHERE id = $2),
  (SELECT count(*) FROM projects WHERE id = $3),
  (SELECT string_agg(space_id::text, ',' ORDER BY space_id) FROM space_members WHERE user_id = $1),
  (SELECT string_agg(project_id::text, ',' ORDER BY project_id) FROM project_members WHERE user_id = $1),
  (SELECT assignee FROM issues WHERE id = $4),
  (SELECT concat(status, ',', email) FROM space_invitations WHERE id = $5),
  (SELECT count(*) FROM reserved_identities
    WHERE (kind = 'email' AND value_hash = sha256(convert_to($6, 'UTF8')))
       OR (kind = 'username' AND value_hash = sha256(convert_to($7, 'UTF8')))),
  (SELECT count(*) FROM events WHERE actor_user_id = $1 OR (entity_type = 'user' AND entity_id = $1::text)
                                  OR (entity_type = 'issue' AND entity_id = $4)),
  (SELECT generation FROM account_deletion_generation WHERE id = 1))`,
			u, personal, personalProject, issue, invitation, email, username).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	epoch := rig.epoch(t, u)
	before := state()
	mark := sink.mark()

	if err := failDeletionAfterWrites(t, pool, rig.store, u, email); err == nil {
		t.Fatal("the deletion did not fail")
	}
	if after := state(); after != before {
		t.Errorf("the failed deletion left traces:\nbefore %s\nafter  %s", before, after)
	}
	if a := rig.do(t, u, epoch, http.MethodGet, "/api/v1/probe", nil); a.status != http.StatusOK {
		t.Errorf("the session after the failed deletion = %d, want 200", a.status)
	}
	if code := keyWorks(); code != http.StatusOK {
		t.Errorf("the key after the failed deletion = %d, want 200", code)
	}
	for _, id := range sink.since(t, pool, mark) {
		if id == u || id == owner || id == other {
			t.Errorf("a personal signal reached %s from the failed deletion", id)
		}
	}
}

// TestAccountDelete_ConcurrentOperations — Each operation
// that would give the account a place — accepting its invitation, an admin
// adding it to a space, creating a project in its space — overlaps the
// deletion for real: the test holds the account's row, the deletion locks
// its spaces and waits on it, the operation then waits (on the space the
// deletion holds, or on the account's row), both seen in pg_blocking_pids;
// the row is released. The deletion commits and the operation, run after
// it, is refused: the deleted account ends in no space, no project, and
// the invitation stays revoked. (The creation of a space is #4's test.)
func TestAccountDelete_ConcurrentOperations(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	rig := newDeleteRig(t, pool, true)
	spacesSvc := spaces.NewService(pool, spaces.Options{Creation: spaces.CreationAll})
	invSvc := invitations.NewService(pool, spacesSvc, accounts.NewTurnstile("", accounts.TurnstileVerifyURL),
		invitations.Options{InviteTTL: 24 * time.Hour})

	// race runs the deletion of u against op: the deletion first waits on
	// the held row, then op waits behind it; returns op's error.
	race := func(t *testing.T, u uuid.UUID, op func() error) error {
		t.Helper()
		epoch := rig.epoch(t, u)
		holder, pid := holdRow(t, pool, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, u)
		deleted := make(chan deleteAnswer, 1)
		go func() {
			deleted <- rig.do(t, u, epoch, http.MethodDelete, "/api/v1/me", map[string]string{"password": deletePassword})
		}()
		waitBlockedBy(t, pool, pid, 1)
		done := make(chan error, 1)
		go func() { done <- op() }()
		waitBlockedBy(t, pool, pid, 2)
		if err := holder.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if a := <-deleted; a.status != http.StatusNoContent {
			t.Fatalf("DELETE /me = %d %s", a.status, a.raw)
		}
		return <-done
	}
	inSpaces := func(u uuid.UUID) int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM space_members WHERE user_id = $1`, u).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("accepting its invitation", func(t *testing.T) {
		u, username := testsupport.SeedUser(t, pool, "user")
		email, _ := deletable(t, pool, u)
		owner, _ := testsupport.SeedUser(t, pool, "user")
		space := testsupport.SeedSpace(t, pool, owner)
		var inv uuid.UUID
		if err := pool.QueryRow(ctx, `
INSERT INTO space_invitations (space_id, email, invited_by, lang, expires_at)
VALUES ($1, $2, $3, 'en', now() + interval '1 day') RETURNING id`, space, email, owner).Scan(&inv); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			c := context.Background()
			_, _ = pool.Exec(c, `DELETE FROM auth_tokens WHERE invitation_id = $1`, inv)
			_, _ = pool.Exec(c, `DELETE FROM outgoing_emails WHERE invitation_id = $1`, inv)
			_, _ = pool.Exec(c, `DELETE FROM space_invitations WHERE id = $1`, inv)
		})
		err := race(t, u, func() error {
			_, err := invSvc.AcceptMine(ctx, spaces.Caller{UserID: u, Username: username,
				Actor: spaces.Actor{UserID: u, Username: username, Type: "human"}}, inv)
			return err
		})
		var ref *spaces.Refusal
		if !errors.As(err, &ref) {
			t.Errorf("accepting after the deletion: %v, want a refusal", err)
		}
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM space_invitations WHERE id = $1`, inv).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "revoked" || inSpaces(u) != 0 {
			t.Errorf("after the race: invitation %s, in %d spaces — want revoked and none", status, inSpaces(u))
		}
	})

	t.Run("an admin adding it to a space", func(t *testing.T) {
		u, _ := testsupport.SeedUser(t, pool, "user")
		deletable(t, pool, u)
		admin, adminName := testsupport.SeedUser(t, pool, "admin")
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE actor_user_id = $1`, admin) })
		owner, _ := testsupport.SeedUser(t, pool, "user")
		space := testsupport.SeedSpace(t, pool, owner)
		short := shortOf(t, pool, space)
		err := race(t, u, func() error {
			_, err := spacesSvc.AddMember(ctx, spaces.Caller{UserID: admin, Username: adminName,
				Actor: spaces.Actor{UserID: admin, Username: adminName, Type: "human"}}, short, u)
			return err
		})
		var ref *spaces.Refusal
		if !errors.As(err, &ref) || ref.Code != "user_not_found" {
			t.Errorf("adding after the deletion: %v, want user_not_found", err)
		}
		if n := inSpaces(u); n != 0 {
			t.Errorf("the deleted account is in %d spaces", n)
		}
	})

	t.Run("creating a project in its space", func(t *testing.T) {
		u, _ := testsupport.SeedUser(t, pool, "user")
		deletable(t, pool, u)
		owner, _ := testsupport.SeedUser(t, pool, "user")
		space := testsupport.SeedSpace(t, pool, owner)
		testsupport.AddSpaceMember(t, pool, space, u)
		short := shortOf(t, pool, space)
		projectsH := NewProjectsHandler(pool, nil)
		r := chi.NewRouter()
		r.Use(ParseUserHeaders, RequireUser)
		r.Post("/api/v1/projects", projectsH.Create)
		name := "race" + strings.ToLower(uniq())
		t.Cleanup(func() {
			c := context.Background()
			_, _ = pool.Exec(c, `DELETE FROM events WHERE project_id IN (SELECT id FROM projects WHERE name = $1)`, name)
			_, _ = pool.Exec(c, `DELETE FROM project_members WHERE project_id IN (SELECT id FROM projects WHERE name = $1)`, name)
			_, _ = pool.Exec(c, `DELETE FROM project_counters WHERE project_id IN (SELECT id FROM projects WHERE name = $1)`, name)
			_, _ = pool.Exec(c, `DELETE FROM projects WHERE name = $1`, name)
		})
		var status int
		var body []byte
		_ = race(t, u, func() error {
			raw, _ := json.Marshal(map[string]any{"name": name, "spaceId": short})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", bytes.NewReader(raw))
			req.Header.Set(auth.HeaderUserID, u.String())
			req.Header.Set(auth.HeaderUsername, "whoever")
			req.Header.Set(auth.HeaderUserRole, "user")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			status, body = rec.Code, rec.Body.Bytes()
			return nil
		})
		// The project path checks the creator's membership under the
		// space's lock (the session gate is not on this router): after the
		// deletion he is no participant — 422 not_space_member
		// (spaces/contract.go#CodeNotSpaceMember). A 5xx is a failure, not
		// a refusal.
		var refusal struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &refusal)
		if status >= 500 {
			t.Errorf("creating a project after the deletion = %d %s — a server error, not a refusal", status, body)
		} else if status != http.StatusUnprocessableEntity || refusal.Error != "not_space_member" {
			t.Errorf("creating a project after the deletion = %d %s, want 422 not_space_member", status, body)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM projects WHERE name = $1`, name).Scan(&n); err != nil || n != 0 {
			t.Errorf("projects created: %d %v, want none", n, err)
		}
	})
}

// TestAccountDelete_AttachmentFileNames — File names
// of surviving attachments, read through the attachments' API after the
// deletion: the deleted address as the whole name, at its end, before an
// extension is the pseudonym; other people's longer addresses stay.
func TestAccountDelete_AttachmentFileNames(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	rig := newDeleteRig(t, pool, true)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	u, _ := testsupport.SeedUser(t, pool, "user")
	email, _ := deletable(t, pool, u)
	project, prefix := testsupport.SeedProject(t, pool, owner, "fnm")
	issue := testsupport.SeedIssue(t, pool, project, owner, prefix+"-1", "files", "open", 2)
	cases := []struct{ name, before, after string }{
		{"the whole name", email, "{p}"},
		{"at the end", "notes " + email, "notes {p}"},
		{"before an extension", email + ".txt", "{p}.txt"},
		{"in the stem, with an extension", "notes " + email + ".pdf", "notes {p}.pdf"},
		{"another's longer local part", "m" + email + ".txt", "m" + email + ".txt"},
		{"another's punctuated local part", "!" + email, "!" + email},
	}
	ids := make([]uuid.UUID, len(cases))
	for i, c := range cases {
		ids[i] = testsupport.SeedAttachment(t, pool, issue, owner, "ready")
		if _, err := pool.Exec(ctx, `UPDATE attachments SET filename = $2 WHERE id = $1`, ids[i], c.before); err != nil {
			t.Fatal(err)
		}
	}
	if a := rig.deleteMe(t, u, deletePassword); a.status != http.StatusNoContent {
		t.Fatalf("DELETE /me = %d %s", a.status, a.raw)
	}
	var pseudo string
	if err := pool.QueryRow(ctx, `SELECT pseudonym FROM users WHERE id = $1`, u).Scan(&pseudo); err != nil {
		t.Fatal(err)
	}
	att := NewAttachmentsHandler(pool, isoS3{}, "skein-attachments", 1<<20, 60)
	r := chi.NewRouter()
	r.Use(ParseUserHeaders, RequireUser)
	r.Get("/api/v1/issues/{issueID}/attachments", att.List)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/issues/"+issue+"/attachments", nil)
	req.Header.Set(auth.HeaderUserID, owner.String())
	req.Header.Set(auth.HeaderUsername, "owner")
	req.Header.Set(auth.HeaderUserRole, "user")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("attachments list = %d %s", rec.Code, rec.Body.Bytes())
	}
	names := map[string]string{}
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		var wrapped struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &wrapped); err != nil {
			t.Fatalf("decode %s: %v", rec.Body.Bytes(), err)
		}
		list = wrapped.Items
	}
	for _, a := range list {
		id, _ := a["id"].(string)
		name, _ := a["filename"].(string)
		names[id] = name
	}
	for i, c := range cases {
		want := strings.ReplaceAll(c.after, "{p}", pseudo)
		if got := names[ids[i].String()]; got != want {
			t.Errorf("%s: file name %q, want %q", c.name, got, want)
		}
	}
}

// TestAccountDelete_TwoDeletionsWithCrossReferences —
// Two accounts with disjoint personal spaces, each one's email in the
// other's personal project, are deleted at the same time: each deletion is
// held on its own account's row until both wait, then both are released.
// Both complete (a deadlock between them may cost one a retry, never the
// retry budget): 204 twice, both accounts deleted, the generation of the
// search cache moved by two.
func TestAccountDelete_TwoDeletionsWithCrossReferences(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	rig := newDeleteRig(t, pool, true)
	type account struct {
		id      uuid.UUID
		email   string
		project uuid.UUID
		epoch   int64
	}
	var acc [2]account
	for i := range acc {
		id, _ := testsupport.SeedUser(t, pool, "user")
		acc[i].id = id
		acc[i].email, _ = deletable(t, pool, id)
		personal := testsupport.SeedSpaceOfKind(t, pool, id, "personal")
		acc[i].project, _ = testsupport.SeedProjectInSpace(t, pool, id, personal, "xref")
	}
	for i := range acc {
		other := acc[1-i]
		if _, err := pool.Exec(ctx, `UPDATE projects SET description = $2 WHERE id = $1`,
			acc[i].project, "ask "+other.email); err != nil {
			t.Fatal(err)
		}
		acc[i].epoch = rig.epoch(t, acc[i].id)
	}
	before := deletionGeneration(t, pool)

	var holders [2]pgx.Tx
	answers := make(chan deleteAnswer, 2)
	for i := range acc {
		var pid int32
		holders[i], pid = holdRow(t, pool, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, acc[i].id)
		a := acc[i]
		go func() {
			answers <- rig.do(t, a.id, a.epoch, http.MethodDelete, "/api/v1/me", map[string]string{"password": deletePassword})
		}()
		waitBlockedBy(t, pool, pid, 1)
	}
	for _, h := range holders {
		if err := h.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for range acc {
		if a := <-answers; a.status != http.StatusNoContent {
			t.Errorf("a deletion = %d %s, want 204", a.status, a.raw)
		}
	}
	for _, a := range acc {
		if !isDeleted(t, pool, a.id) {
			t.Errorf("account %s is not deleted", a.id)
		}
	}
	if g := deletionGeneration(t, pool); g != before+2 {
		t.Errorf("generation %d, want %d", g, before+2)
	}
}
