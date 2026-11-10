package idempotency_test

// rig_test.go — the test routes of the Idempotency tests: the real request
// chain of the public entry (key authentication over the test database, the
// key right, the mode) in front of the real middleware, on a table of this
// package's own. The writing test operation adds a note (a comment row) to
// the issue of its path and moves the issue's version in one transaction,
// recording its outcome there with idempotency.RecordInTx.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/errcode"
	"skein-api/internal/idempotency"
	"skein-api/internal/issues"
	"skein-api/internal/publicapi"
	"skein-api/internal/testsupport"
)

const (
	opAddNote     = "testAddNote"
	opManageNote  = "testManageNote"
	opAnswerCode  = "testAnswerCode"
	opUpdateNote  = "testUpdateNote"
	opBulkNotes   = "testBulkNotes"
	opCloseIssue  = "testCloseIssue"
	writeTimeout  = 30 * time.Second
	notesPathTmpl = "/api/v1/issues/%s/notes"
)

// noteBody is the body of the writing test operation.
type noteBody struct {
	Text string `json:"text"`
}

// noteResponse is the response of the writing test operation: the issue it
// changed, the issue's title and the note it added.
type noteResponse struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
	Title   string `json:"title"`
	NoteID  string `json:"noteId,omitempty"`
}

// codeBody makes the answering test operation answer a problem.
type codeBody struct {
	Code   string `json:"code"`
	Origin string `json:"origin"`
	Status int    `json:"status"`
}

type rig struct {
	pool   *pgxpool.Pool
	issues *issues.Store
	h      http.Handler
	calls  atomic.Int64
}

// newRig builds the router over pool.
func newRig(t *testing.T, pool *pgxpool.Pool) *rig {
	t.Helper()
	rg := &rig{pool: pool, issues: issues.NewStore(pool)}
	chain := publicapi.NewChain(publicapi.ChainOptions{
		Authenticator: auth.NewAPIKeyAuthenticator(pool, auth.MinBcryptCost),
		Executors:     publicapi.PoolExecutors{Pool: pool},
		ClientIP:      publicapi.NewClientIP(nil),
		WarmKeys:      publicapi.NewWarmKeys(15*time.Minute, 100, time.Now),
		AuthFailures:  publicapi.NewWindowLimiter(100000, time.Minute, 100, time.Now),
		Requests:      publicapi.NewWindowLimiter(100000, time.Minute, 100, time.Now),
	})
	issueRoute := func(id, path string, guarded bool, actions []authz.Action, h http.HandlerFunc) publicapi.Route {
		return publicapi.Route{
			OperationID: id, Method: http.MethodPost, Path: path, Auth: publicapi.AuthKey,
			Key: authz.KeyRequirement{Right: authz.KeyRightWrite}, Guarded: guarded,
			Actions: actions, Object: publicapi.ObjectPathIssueProject,
			Idempotent: true, Prefer: true, Handler: h, Hydrate: rg.hydrate,
		}
	}
	rg.h = publicapi.NewRouter(publicapi.RouterOptions{
		OperationTimeout: writeTimeout,
		Chain:            chain,
		Idempotency:      idempotency.NewMiddleware(pool, writeTimeout),
	}, []publicapi.Route{
		issueRoute(opAddNote, "/api/v1/issues/{issue}/notes", true, []authz.Action{authz.ActionWrite}, rg.addNote),
		issueRoute(opCloseIssue, "/api/v1/issues/{issue}/close", true, []authz.Action{authz.ActionWrite}, rg.closeIssue),
		issueRoute(opManageNote, "/api/v1/issues/{issue}/managed-notes", false, []authz.Action{authz.ActionManageProject}, rg.addNote),
		{
			OperationID: opAnswerCode, Method: http.MethodPost, Path: "/api/v1/answers", Auth: publicapi.AuthKey,
			Key: authz.KeyRequirement{Right: authz.KeyRightWrite}, Idempotent: true,
			Handler: http.HandlerFunc(rg.answerCode), Hydrate: rg.hydrate,
		},
		{
			OperationID: opUpdateNote, Method: http.MethodPost, Path: "/api/v1/issues/{issue}/edit", Auth: publicapi.AuthKey,
			Key: authz.KeyRequirement{Right: authz.KeyRightWrite}, Handler: http.HandlerFunc(rg.addNote),
		},
		{
			OperationID: opBulkNotes, Method: http.MethodPost, Path: "/api/v1/notes/bulk", Auth: publicapi.AuthKey,
			Key: authz.KeyRequirement{Right: authz.KeyRightWrite}, Handler: http.HandlerFunc(rg.addNote),
		},
	})
	return rg
}

// addNote is the writing test operation: in one transaction it adds a note
// to the issue of the path, moves the issue's version and records the
// outcome; then it answers 201 with the issue, its title and the note.
func (rg *rig) addNote(w http.ResponseWriter, r *http.Request) {
	rg.calls.Add(1)
	ctx := r.Context()
	issueID := chi.URLParam(r, "issue")
	var body noteBody
	if !publicapi.DecodeBody(w, r, &body) {
		return
	}
	noteID := uuid.New()
	var resp noteResponse
	err := pgx.BeginFunc(ctx, rg.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
INSERT INTO comments (id, issue_id, author, author_type, body) VALUES ($1, $2, 'rig', 'agent', $3)`,
			noteID, issueID, body.Text); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
UPDATE issues SET version = version + 1 WHERE id = $1 RETURNING id, version, title`, issueID).
			Scan(&resp.ID, &resp.Version, &resp.Title); err != nil {
			return err
		}
		issue := publicapi.ObjectRef{Kind: publicapi.ObjectIssue, ID: issueID}
		return idempotency.RecordInTx(ctx, tx, idempotency.Outcome{
			Status: http.StatusCreated, ID: resp.ID, Version: resp.Version,
			Refs: idempotency.Refs{
				Target:  &issue,
				Objects: []publicapi.ObjectRef{issue, {Kind: publicapi.ObjectComment, ID: noteID.String()}},
			},
		})
	})
	if err != nil {
		publicapi.WriteInternalError(w, r, "adding a note", err)
		return
	}
	resp.NoteID = noteID.String()
	publicapi.SetETag(w, resp.Version)
	publicapi.WriteJSON(w, r, http.StatusCreated, resp)
}

// closeIssue closes the issue of its path through issues.Store.UpdateIssue,
// the outcome recorded by idempotency.IssueBeforeCommit in that change's
// transaction, and answers 200 with the issue's id, version and status.
func (rg *rig) closeIssue(w http.ResponseWriter, r *http.Request) {
	rg.calls.Add(1)
	ctx := r.Context()
	userID, _ := auth.UserIDFrom(ctx)
	issueID := chi.URLParam(r, "issue")
	var version int
	if err := rg.pool.QueryRow(ctx, `SELECT version FROM issues WHERE id = $1`, issueID).Scan(&version); err != nil {
		publicapi.WriteInternalError(w, r, "reading the version of an issue", err)
		return
	}
	res, err := rg.issues.UpdateIssue(ctx, issues.UpdateIssueInput{
		ID:              issueID,
		ExpectedVersion: version,
		Actor:           issues.HumanActor(userID, "rig"),
		Close:           &issues.CloseInput{Reason: "closed by the rig"},
		BeforeCommit:    idempotency.IssueBeforeCommit(http.StatusOK),
	})
	if err != nil {
		publicapi.WriteInternalError(w, r, "closing an issue", err)
		return
	}
	publicapi.SetETag(w, res.State.Version)
	publicapi.WriteJSON(w, r, http.StatusOK, map[string]any{
		"id": res.State.ID, "version": res.State.Version, "status": res.State.Status,
	})
}

// answerCode is the answering test operation: it answers the problem of its
// body, or with status 2xx a body without writing anything.
func (rg *rig) answerCode(w http.ResponseWriter, r *http.Request) {
	rg.calls.Add(1)
	var body codeBody
	if !publicapi.DecodeBody(w, r, &body) {
		return
	}
	if body.Code == "" {
		publicapi.WriteJSON(w, r, body.Status, map[string]string{"id": "answer"})
		return
	}
	code, ok := errcode.Lookup(body.Code)
	if !ok {
		publicapi.WriteInternalError(w, r, "answering a code", errNoSuchCode(body.Code))
		return
	}
	publicapi.WriteProblem(w, publicapi.Problem{Code: code, Origin: origins[body.Origin]})
}

type errNoSuchCode string

func (e errNoSuchCode) Error() string { return "no such code " + string(e) }

// origins maps the reference's origin names to the problem writer's.
var origins = map[string]publicapi.Origin{
	"path": publicapi.OriginPath, "body": publicapi.OriginBody,
	"parse": publicapi.OriginParse, "domain": publicapi.OriginDomain,
}

// hydrate reads the current state of the issue among objects.
func (rg *rig) hydrate(ctx context.Context, _ *http.Request, objects []publicapi.ObjectRef) (any, error) {
	for _, o := range objects {
		if o.Kind != publicapi.ObjectIssue {
			continue
		}
		var resp noteResponse
		err := rg.pool.QueryRow(ctx, `SELECT id, version, title FROM issues WHERE id = $1`, o.ID).
			Scan(&resp.ID, &resp.Version, &resp.Title)
		return resp, err
	}
	return map[string]string{}, nil
}

// call is one request to the rig.
type call struct {
	path, key, idemKey, body string
	headers                  map[string]string
}

func (rg *rig) do(t *testing.T, c call) *httptest.ResponseRecorder {
	t.Helper()
	body := c.body
	if body == "" {
		body = `{"text":"one"}`
	}
	req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.HeaderAPIKey, c.key)
	if c.idemKey != "" {
		req.Header.Set(publicapi.IdempotencyKeyHeader, c.idemKey)
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	rg.h.ServeHTTP(rec, req)
	return rec
}

// world is one owner, his project with two issues and a key.
type world struct {
	owner    uuid.UUID
	project  uuid.UUID
	other    uuid.UUID
	issueA   string
	issueB   string
	key      string
	keyID    uuid.UUID
	idemKeys string
}

func seedWorld(t *testing.T, pool *pgxpool.Pool, level apikeys.Level) world {
	t.Helper()
	var w world
	w.owner, _ = testsupport.SeedUser(t, pool, "")
	var prefix string
	w.project, prefix = testsupport.SeedProject(t, pool, w.owner, "idem")
	w.other, _ = testsupport.SeedProject(t, pool, w.owner, "idemo")
	w.issueA = testsupport.SeedIssue(t, pool, w.project, w.owner, prefix+"-1", "title A", "open", 2)
	w.issueB = testsupport.SeedIssue(t, pool, w.project, w.owner, prefix+"-2", "title B", "open", 2)
	w.key, w.keyID = mintKey(t, pool, w.owner, apikeys.Grant{Level: level, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}})
	w.idemKeys = "k-" + uuid.NewString()
	return w
}

// mintKey creates a key of user with grant and returns its plain text and
// id; its stored outcomes go with it.
func mintKey(t *testing.T, pool *pgxpool.Pool, user uuid.UUID, g apikeys.Grant) (string, uuid.UUID) {
	t.Helper()
	plain, prefix, hash, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	c, err := apikeys.New(pool).Create(context.Background(), apikeys.NewKey{
		UserID: user, Label: "idempotency", Prefix: prefix, Hash: hash, Grant: g,
	}, testsupport.KeyTTLPolicy)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM agents WHERE api_key_id = $1`, c.ID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, c.ID)
	})
	return plain, c.ID
}

func exec(t *testing.T, pool *pgxpool.Pool, what, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// notes counts the notes of issue: one per execution of the operation.
func notes(t *testing.T, pool *pgxpool.Pool, issue string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM comments WHERE issue_id = $1`, issue).Scan(&n); err != nil {
		t.Fatalf("count notes: %v", err)
	}
	return n
}

// decodeNote decodes a 201 response of the writing operation.
func decodeNote(t *testing.T, rec *httptest.ResponseRecorder) noteResponse {
	t.Helper()
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
	var resp noteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not JSON: %v (%q)", err, rec.Body.String())
	}
	return resp
}

// problemCode decodes the code of a problem response.
func problemCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != publicapi.ProblemContentType {
		t.Fatalf("Content-Type %q, want %s (status %d, body %q)", ct, publicapi.ProblemContentType, rec.Code, rec.Body.String())
	}
	var p struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("problem is not JSON: %v", err)
	}
	return p.Code
}

func wantProblem(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status %d, want %d (body %q)", rec.Code, status, rec.Body.String())
	}
	if got := problemCode(t, rec); got != code {
		t.Errorf("code %q, want %s", got, code)
	}
}
