package idempotency

// middleware.go — Idempotency-Key on the routes of the public API that
// accept it (US-7, plan D-8). The middleware runs after the request chain
// (authentication, key right, mode) and the body checks, before If-Match and
// Prefer: a repeat answered from the stored outcome is not held to the
// business preconditions again.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/errcode"
	"skein-api/internal/issues"
	"skein-api/internal/publicapi"
)

// maxKeyLength is the longest Idempotency-Key accepted.
const maxKeyLength = 255

// finishTimeout bounds the writes after the operation returned: they run
// detached from the request, whose deadline may already have passed.
const finishTimeout = 5 * time.Second

// Middleware is the Idempotency layer of the public router
// (publicapi.RouterOptions.Idempotency).
type Middleware struct {
	store     *Store
	pool      *pgxpool.Pool
	markerTTL time.Duration
}

// NewMiddleware builds the middleware over pool. writeTimeout is the deadline
// of one operation (SKEIN_PUBLIC_API_WRITE_TIMEOUT); the in-progress marker
// lives writeTimeout plus MarkerGrace.
func NewMiddleware(pool *pgxpool.Pool, writeTimeout time.Duration) *Middleware {
	if writeTimeout <= 0 {
		panic("idempotency: write timeout must be > 0")
	}
	return &Middleware{store: NewStore(pool), pool: pool, markerTTL: writeTimeout + MarkerGrace}
}

// execution is the in-progress execution of one request under its marker.
type execution struct {
	store *Store
	keyID uuid.UUID
	key   string
	token uuid.UUID
	// lost is set when the record of the outcome found the marker taken
	// over: the operation rolled back.
	lost bool
	// target is the object the operation acts on, as NoteTarget named it.
	target *publicapi.ObjectRef
	// objects are the objects of the response, as NoteObjects named them.
	objects []publicapi.ObjectRef
	// needsTarget — the route checks the owner's role on a target in the
	// mode of the request: an outcome that names no target cannot be
	// re-checked on a repeat, so it is never kept.
	needsTarget bool
}

// errNoTarget refuses to record an outcome that a repeat could not re-check.
var errNoTarget = errors.New("idempotency: the outcome of an operation with owner actions names no target")

type executionKey struct{}

func executionFrom(ctx context.Context) *execution {
	e, _ := ctx.Value(executionKey{}).(*execution)
	return e
}

// RecordInTx records o in tx, the transaction that performs the operation
// (D-8 rule 2), as a compare-and-swap on the marker of the request. It does
// nothing for a request without Idempotency-Key. ErrMarkerLost — a repeat
// took the marker over — must roll tx back; the request is then answered 409
// idempotency_in_progress. It is meant for the BeforeCommit hook of a store
// write, and runs again on every retried attempt.
func RecordInTx(ctx context.Context, tx pgx.Tx, o Outcome) error {
	e := executionFrom(ctx)
	if e == nil {
		return nil
	}
	if o.Refs.Target == nil {
		o.Refs.Target = e.target
	}
	if len(o.Refs.Objects) == 0 {
		o.Refs.Objects = e.objects
	}
	if e.needsTarget && o.Refs.Target == nil {
		return errNoTarget
	}
	err := Record(ctx, tx, e.keyID, e.key, e.token, o)
	if errors.Is(err, ErrMarkerLost) {
		e.lost = true
	}
	return err
}

// IssueBeforeCommit is the issues.UpdateIssueInput.BeforeCommit of an
// operation that answers status with the issue it changed (closeIssue: 200):
// it records the committed id and version of the issue, the issue as the
// target and as the object of the response, in the change's own
// transaction (D-8 rule 2). It does nothing for a request without
// Idempotency-Key.
func IssueBeforeCommit(status int) func(ctx context.Context, tx pgx.Tx, st issues.CommittedState) error {
	return func(ctx context.Context, tx pgx.Tx, st issues.CommittedState) error {
		issue := publicapi.ObjectRef{Kind: publicapi.ObjectIssue, ID: st.ID}
		return RecordInTx(ctx, tx, Outcome{
			Status: status, ID: st.ID, Version: st.Version,
			Refs: Refs{Target: &issue, Objects: []publicapi.ObjectRef{issue}},
		})
	}
}

// CommentBeforeCommit is the issues.CommentBeforeCommitHook of createComment,
// which answers status with the comment it wrote: it records the comment's
// id and version, the comment as the object of the response, in the
// comment's own transaction (D-8 rule 2); the target is the issue the
// operation named. It does nothing for a request without Idempotency-Key.
func CommentBeforeCommit(status int) func(ctx context.Context, tx pgx.Tx, c *issues.CommentDTO) error {
	return func(ctx context.Context, tx pgx.Tx, c *issues.CommentDTO) error {
		comment := publicapi.ObjectRef{Kind: publicapi.ObjectComment, ID: c.ID.String()}
		return RecordInTx(ctx, tx, Outcome{
			Status: status, ID: c.ID.String(), Version: c.Version,
			Refs: Refs{Objects: []publicapi.ObjectRef{comment}},
		})
	}
}

// NoteTarget names the object the operation acts on — the project of a
// create, the issue of a close or a comment — once the operation resolved
// it. An outcome stored without a writing transaction (a 422) keeps it, so a
// repeat re-checks the owner's role on it; on a route with owner actions an
// outcome without a target is not kept at all (and RecordInTx refuses it).
// It does nothing for a request without Idempotency-Key.
func NoteTarget(ctx context.Context, ref publicapi.ObjectRef) {
	if e := executionFrom(ctx); e != nil {
		e.target = &ref
	}
}

// NoteObjects names objects of the response of an outcome produced without
// a writing transaction — the candidates in the body of a 422
// duplicate_suspected, the objects a 2xx without a write answers with. A
// stored outcome keeps them, and a repeat re-checks the key's scope and the
// owner's visibility on each before giving the answer out (US-7). It does
// nothing for a request without Idempotency-Key.
func NoteObjects(ctx context.Context, refs ...publicapi.ObjectRef) {
	if e := executionFrom(ctx); e != nil {
		e.objects = append(e.objects, refs...)
	}
}

// Wrap implements publicapi.RouteWrapper.
func (m *Middleware) Wrap(rt publicapi.Route, next http.Handler) http.Handler {
	if !rt.Idempotent {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values, sent := r.Header[publicapi.IdempotencyKeyHeader]
		if !sent {
			next.ServeHTTP(w, r)
			return
		}
		key, ok := validKey(values)
		if !ok {
			publicapi.WriteProblem(w, publicapi.Problem{Code: errcode.ValidationError, Origin: publicapi.OriginParse,
				Detail: fmt.Sprintf("%s must be one value of 1-%d visible ASCII characters",
					publicapi.IdempotencyKeyHeader, maxKeyLength)})
			return
		}
		keyID, ok := auth.APIKeyIDFrom(r.Context())
		if !ok {
			publicapi.WriteInternalError(w, r, "reading the API key of an Idempotency-Key request",
				errors.New("the request carries no API key id"))
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			if publicapi.IsBodyTooLarge(err) {
				publicapi.WriteProblem(w, publicapi.Problem{Code: errcode.PayloadTooLarge, Origin: publicapi.OriginParse})
				return
			}
			publicapi.WriteProblem(w, publicapi.Problem{Code: errcode.ValidationError, Origin: publicapi.OriginParse,
				Detail: "the request body could not be read"})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		sum := sha256.Sum256(raw)
		id := Identity{
			KeyID:       keyID,
			Key:         key,
			OperationID: rt.OperationID,
			Target:      r.URL.Path,
			Fingerprint: sum[:],
			Prefer:      canonicalPrefer(r.Header.Values("Prefer")),
			Force:       publicapi.ModeFromContext(r.Context()).Force,
		}

		d, err := m.store.Begin(r.Context(), id, m.markerTTL)
		if err != nil {
			publicapi.WriteInternalError(w, r, "reading the Idempotency-Key outcome", err)
			return
		}
		switch d.Verdict {
		case InProgress:
			publicapi.WriteProblem(w, publicapi.Problem{Code: errcode.IdempotencyInProgress, Origin: publicapi.OriginDomain,
				Detail: "the first request with this " + publicapi.IdempotencyKeyHeader + " is still being executed"})
		case KeyReused:
			publicapi.WriteProblem(w, publicapi.Problem{Code: errcode.IdempotencyKeyReused, Origin: publicapi.OriginDomain,
				Detail: "this " + publicapi.IdempotencyKeyHeader + " was used for another request: another operation, target, body, Prefer or force"})
		case Replay:
			m.replay(w, r, rt, d.Stored)
		case Execute:
			m.execute(w, r, next, &execution{store: m.store, keyID: keyID, key: key, token: d.Token,
				needsTarget: len(rt.EffectiveActions(publicapi.ModeFromContext(r.Context()))) > 0})
		default:
			publicapi.WriteInternalError(w, r, "deciding on an Idempotency-Key request",
				fmt.Errorf("unknown verdict %d", d.Verdict))
		}
	})
}

// canonicalPrefer is the Prefer of a request as the identity compares it:
// every preference trimmed, lower-cased and sorted, so a repeat that only
// formats the header differently is the same request.
func canonicalPrefer(values []string) string {
	var prefs []string
	for _, v := range values {
		for _, p := range strings.Split(v, ",") {
			if p = strings.ToLower(strings.Join(strings.Fields(p), "")); p != "" {
				prefs = append(prefs, p)
			}
		}
	}
	slices.Sort(prefs)
	return strings.Join(prefs, ", ")
}

// validKey reads the one Idempotency-Key of a request.
func validKey(values []string) (string, bool) {
	if len(values) != 1 {
		return "", false
	}
	k := values[0]
	if k == "" || len(k) > maxKeyLength {
		return "", false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7e {
			return "", false
		}
	}
	return k, true
}

// detached is the context of the writes after the operation returned.
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
}

// execute runs the operation under the marker of e and ends the execution
// (Store.Finish) before the response is sent.
func (m *Middleware) execute(w http.ResponseWriter, r *http.Request, next http.Handler, e *execution) {
	rec := newRecorder()
	defer func() {
		if p := recover(); p != nil {
			ctx, cancel := detached(r.Context())
			defer cancel()
			if err := e.store.Release(ctx, e.keyID, e.key, e.token); err != nil {
				slog.ErrorContext(r.Context(), "idempotency: releasing the marker after a panic failed",
					"operationId", operationID(r), "error", err.Error())
			}
			panic(p)
		}
	}()
	next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), executionKey{}, e)))

	if e.lost {
		publicapi.WriteProblem(w, publicapi.Problem{Code: errcode.IdempotencyInProgress, Origin: publicapi.OriginDomain,
			Detail: "a repeat of this request took its " + publicapi.IdempotencyKeyHeader + " over; this execution was rolled back"})
		return
	}
	resp := Response{Status: rec.status, ContentType: rec.header.Get("Content-Type"), Body: rec.body.Bytes()}
	resp.ID, resp.Version = namedResource(rec)
	ctx, cancel := detached(r.Context())
	defer cancel()
	// An outcome the repeat could not re-check the owner's role for is not
	// kept: the repeat runs again (review R6-03).
	storable := Storable(rec.status, resp.Body) && !(e.needsTarget && e.target == nil)
	if err := e.store.Finish(ctx, e.keyID, e.key, e.token, resp, storable,
		Refs{Target: e.target, Objects: e.objects}); err != nil {
		slog.ErrorContext(r.Context(), "idempotency: ending the execution failed",
			"operationId", operationID(r), "status", rec.status, "error", err.Error())
	}
	rec.sendTo(w)
}

func operationID(r *http.Request) string {
	rt, _ := publicapi.RouteFromContext(r.Context())
	return rt.OperationID
}

// Storable reports whether an outcome is kept for repeats (US-7): a success
// (2xx), and a business refusal (422) whose code is not transient and which
// is not marked retryable. Everything else — 400, 401, 403, 404, 409, 412,
// 428, 429, 5xx, a transient 422 — is not kept: a repeat runs again.
func Storable(status int, body []byte) bool {
	if status >= 200 && status < 300 {
		return true
	}
	if status != http.StatusUnprocessableEntity {
		return false
	}
	var p struct {
		Code      string `json:"code"`
		Retryable bool   `json:"retryable"`
	}
	if err := json.Unmarshal(body, &p); err != nil || p.Retryable {
		return false
	}
	code, known := errcode.Lookup(p.Code)
	return known && !code.Transient()
}

// namedResource is the id and version a response names: the version of its
// ETag, the id member of its JSON body.
func namedResource(rec *recorder) (string, int) {
	version := 0
	if etag := strings.Trim(rec.header.Get("ETag"), `"`); etag != "" {
		if v, err := strconv.Atoi(etag); err == nil {
			version = v
		}
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.body.Bytes(), &body); err != nil {
		return "", version
	}
	return body.ID, version
}

// replay answers the stored outcome s, after re-checking what a repeat must
// still pass (US-7): the key right in the mode of the first request, the
// key's scope and the owner's visibility of the target and of every object
// of the response, then the owner's role on the target by the effective
// actions of the route in that mode. A failed check answers as for any
// request, and the stored outcome is not given out.
func (m *Middleware) replay(w http.ResponseWriter, r *http.Request, rt publicapi.Route, s Stored) {
	ctx := r.Context()
	mode := publicapi.Mode{Force: s.Force}
	grant, _ := auth.KeyGrantFrom(ctx)
	if req := rt.KeyRequirement(mode); !authz.KeyMeets(grant, req) {
		publicapi.WriteInsufficientScope(w, rt, req)
		return
	}

	scope := authz.FromContext(ctx)
	var targetLevel authz.Level
	refs := s.Refs.Objects
	if s.Refs.Target != nil {
		refs = append([]publicapi.ObjectRef{*s.Refs.Target}, refs...)
	}
	for i, ref := range refs {
		level, visible, err := m.access(ctx, scope, ref)
		if err != nil {
			publicapi.WriteInternalError(w, r, "re-checking an object of a stored outcome", err)
			return
		}
		if !visible {
			publicapi.WriteProblem(w, publicapi.Problem{Code: notFoundCode(ref.Kind), Origin: publicapi.OriginPath})
			return
		}
		if i == 0 && s.Refs.Target != nil {
			targetLevel = level
		}
	}
	if actions := rt.EffectiveActions(mode); len(actions) > 0 {
		if s.Refs.Target == nil {
			// Never stored since R6-03; a row from before is not given out
			// unchecked, whatever its status.
			publicapi.WriteInternalError(w, r, "re-checking the role for a stored outcome",
				errors.New("a stored outcome of an operation with owner actions has no target"))
			return
		}
		for _, a := range actions {
			if !authz.Allows(targetLevel, a) {
				code := errcode.Forbidden
				if a == authz.ActionForceUnlock {
					code = errcode.ForceUnlockForbidden
				}
				publicapi.WriteProblem(w, publicapi.Problem{Code: code, Origin: publicapi.OriginDomain})
				return
			}
		}
	}

	success := s.Status >= 200 && s.Status < 300
	minimal := success && rt.Prefer && publicapi.AsksMinimal(r)
	body, contentType := s.Body, s.ContentType
	if body == nil {
		if !success {
			publicapi.WriteInternalError(w, r, "answering a stored outcome",
				fmt.Errorf("stored status %d without a body", s.Status))
			return
		}
		var err error
		if body, err = m.hydrate(ctx, r, rt, s, minimal); err != nil {
			publicapi.WriteInternalError(w, r, "reading the objects of a stored outcome", err)
			return
		}
		contentType = "application/json"
	}
	if s.Version != nil {
		publicapi.SetETag(w, int(*s.Version))
	}
	if minimal {
		w.Header().Set("Preference-Applied", "return=minimal")
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(s.Status)
	if _, err := w.Write(body); err != nil {
		slog.DebugContext(ctx, "idempotency: writing a stored response failed", "error", err.Error())
	}
}

// hydrate builds the body of a stored outcome whose body was not written
// (D-8 rule 6): the recorded id and version, the other fields from the
// current state of its objects — or only {id, version} under
// Prefer: return=minimal.
func (m *Middleware) hydrate(ctx context.Context, r *http.Request, rt publicapi.Route, s Stored, minimal bool) ([]byte, error) {
	obj := map[string]json.RawMessage{}
	if !minimal {
		v, err := rt.Hydrate(ctx, r, s.Refs.Objects)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("encoding the current state: %w", err)
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("the current state of %s is not a JSON object: %w", rt.OperationID, err)
		}
	}
	if s.ID != nil {
		obj["id"], _ = json.Marshal(*s.ID)
	}
	if s.Version != nil {
		obj["version"], _ = json.Marshal(*s.Version)
	}
	return json.Marshal(obj)
}

// projectOfSQL reads the project of an issue or of a live comment.
const (
	issueProjectSQL = `SELECT project_id FROM issues WHERE id = $1`
	// A deleted comment stays a readable tombstone (getComment): its
	// visibility is its issue's project, so a repeat after the deletion
	// answers the stored outcome (review R8-03).
	commentProjectSQL = `
SELECT i.project_id FROM comments c JOIN issues i ON i.id = c.issue_id
WHERE c.id = $1`
)

// access is the scope's level on the project of ref; visible is false when
// the object is missing or out of reach (the key's scope included).
func (m *Middleware) access(ctx context.Context, scope authz.Scope, ref publicapi.ObjectRef) (authz.Level, bool, error) {
	var project uuid.UUID
	switch ref.Kind {
	case publicapi.ObjectProject:
		id, err := uuid.Parse(ref.ID)
		if err != nil {
			return "", false, fmt.Errorf("stored project reference %q: %w", ref.ID, err)
		}
		project = id
	case publicapi.ObjectIssue, publicapi.ObjectComment:
		sql, arg := issueProjectSQL, any(ref.ID)
		if ref.Kind == publicapi.ObjectComment {
			id, err := uuid.Parse(ref.ID)
			if err != nil {
				return "", false, fmt.Errorf("stored comment reference %q: %w", ref.ID, err)
			}
			sql, arg = commentProjectSQL, id
		}
		err := m.pool.QueryRow(ctx, sql, arg).Scan(&project)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		if err != nil {
			return "", false, fmt.Errorf("reading the project of %s %s: %w", ref.Kind, ref.ID, err)
		}
	default:
		return "", false, fmt.Errorf("stored reference of unknown kind %q", ref.Kind)
	}
	level, err := authz.ProjectAccess(ctx, m.pool, scope, project)
	if errors.Is(err, authz.ErrNotVisible) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return level, true, nil
}

// notFoundCode is the answer for an object of kind that is missing or out of
// reach.
func notFoundCode(kind publicapi.ObjectKind) errcode.Code {
	switch kind {
	case publicapi.ObjectIssue:
		return errcode.IssueNotFound
	case publicapi.ObjectComment:
		return errcode.CommentNotFound
	}
	return errcode.ProjectNotFound
}

// recorder keeps the response of the operation until the execution ended.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
	wrote  bool
}

func newRecorder() *recorder { return &recorder{header: http.Header{}, status: http.StatusOK} }

func (rec *recorder) Header() http.Header { return rec.header }

func (rec *recorder) WriteHeader(status int) {
	if rec.wrote {
		return
	}
	rec.wrote = true
	rec.status = status
}

func (rec *recorder) Write(p []byte) (int, error) {
	rec.WriteHeader(http.StatusOK)
	return rec.body.Write(p)
}

// sendTo writes the kept response to w.
func (rec *recorder) sendTo(w http.ResponseWriter) {
	for k, v := range rec.header {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.status)
	if _, err := w.Write(rec.body.Bytes()); err != nil {
		slog.Debug("idempotency: writing the response failed", "error", err.Error())
	}
}

// Hooks implements publicapi.Outcomes with the functions of this package.
type Hooks struct{}

// IssueBeforeCommit implements publicapi.Outcomes.
func (Hooks) IssueBeforeCommit(status int) func(ctx context.Context, tx pgx.Tx, st issues.CommittedState) error {
	return IssueBeforeCommit(status)
}

// CommentBeforeCommit implements publicapi.Outcomes.
func (Hooks) CommentBeforeCommit(status int) func(ctx context.Context, tx pgx.Tx, c *issues.CommentDTO) error {
	return CommentBeforeCommit(status)
}

// NoteTarget implements publicapi.Outcomes.
func (Hooks) NoteTarget(ctx context.Context, ref publicapi.ObjectRef) { NoteTarget(ctx, ref) }

// NoteObjects implements publicapi.Outcomes.
func (Hooks) NoteObjects(ctx context.Context, refs ...publicapi.ObjectRef) { NoteObjects(ctx, refs...) }

var _ publicapi.Outcomes = Hooks{}
