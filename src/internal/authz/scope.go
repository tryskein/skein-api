// Package authz is the one place a request learns what it may see and do
// (sk-674.18, D-1, INV-2, plan K-1/K-3).
//
// # One rule, one mechanism
//
// The rule "which projects does the caller see, at what level" lives in the
// database, in the relation skein_project_access_set(caller) and its point
// form skein_project_access(caller, project) (migration 000066). Everything
// in this package is the Go side of that relation and nothing else:
//
//   - Scope — whose relation a request reads (scope.go);
//   - ProjectVisibleExpr / VisibleJoin — the relation as a predicate or a
//     join inside a larger query (sql.go), and the reader-facing blocker
//     fragments built on it (blockers.go);
//   - ProjectAccess / Can — the level on one project and the actions it
//     allows (actions.go);
//   - RecipientVisibleExpr — the same relation for a recipient user, for
//     fan-out, digest and mentions (recipients.go);
//   - ResolveBatch — the hub's per-user batch check (batch.go).
//
// No surface computes membership or role itself; the rules of spaces (T017)
// change the body of the SQL functions and the action table here, not the
// surfaces.
//
// # Who decides "admin"
//
// The relation reads users.role / users.is_external by the caller's id. The
// request context only names WHO is asking: a role stashed on the context is
// not a grant here, so an admin's reach comes from the relation's 'admin'
// level, never from a Go short-circuit (D-2).
package authz

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/google/uuid"

	"skein-api/internal/auth"
)

// Scope is whose view of the projects a request reads.
//
// Three cases, and only three:
//
//   - a caller — the request carries a principal; it reads that caller's
//     relation (whatever the caller's role: an admin's breadth is the
//     relation's business);
//   - unrestricted — the context was DECLARED server-internal work with
//     WithUnrestricted (workers, migrations, fixtures): every project;
//   - nobody — neither: an undeclared context. It reads the relation of the
//     nil id, which is empty. Absence is a bug, and a bug that shows too
//     little is one somebody notices.
type Scope struct {
	unrestricted bool
	callerID     uuid.UUID
	// cache is the request's memo of the caller's visible set
	// (WithRequestCache); nil outside a request, then Bind reads the
	// relation on every call.
	cache *requestSet
}

// FromContext derives the scope from the request context.
//
// A principal wins over the internal marker: a marker left on a context
// that later acquired a user must not promote that user.
func FromContext(ctx context.Context) Scope {
	if p, ok := auth.PrincipalFrom(ctx); ok {
		cache, _ := ctx.Value(requestSetKey{}).(*requestSet)
		return Scope{callerID: p.UserID, cache: cache}
	}
	if isDeclaredUnrestricted(ctx) {
		return Unrestricted()
	}
	return Scope{}
}

// ForUser is the scope of a user identified by id outside a request — a
// recipient, a monitor credential's owner.
func ForUser(id uuid.UUID) Scope {
	return Scope{callerID: id}
}

// ScopeFor is the scope of the user id as the caller of this request: the
// request's own scope (with its per-request memo) when ctx's principal is
// that user, ForUser(id) otherwise. For stores whose arguments name the
// caller explicitly.
func ScopeFor(ctx context.Context, id uuid.UUID) Scope {
	if s := FromContext(ctx); !s.unrestricted && s.callerID == id {
		return s
	}
	return ForUser(id)
}

// Unrestricted is the scope that sees every project. Named so call sites
// that genuinely have no caller say so.
func Unrestricted() Scope {
	return Scope{unrestricted: true}
}

// IsUnrestricted reports whether the scope is declared internal work.
func (s Scope) IsUnrestricted() bool { return s.unrestricted }

// CallerID is the user whose relation the scope reads; uuid.Nil for the
// unrestricted and the empty scope.
func (s Scope) CallerID() uuid.UUID { return s.callerID }

// Bind returns the two bind values of the SQL fragments of this package
// (ProjectVisibleExpr, VisibleExpr, the blocker fragments), in placeholder
// order: the unrestricted flag, then the scope's visible set — the project
// ids of the access relation, skein_project_access_set, for the caller (an
// admin's included; there is no Go shortcut). The set is read ONCE per
// request when the request context carries the memo of WithRequestCache,
// and on every call otherwise; it is never kept across requests, so a lost
// access takes effect on the next request (INV-26). Declared internal work
// binds (true, empty); the empty scope binds (false, empty).
//
// The set is bound as an array and tested as `= ANY(...)`: the planner sees
// it as a value — exact selectivity, the index on project_id, no per-row
// call (sk-674.18 T009, K-1 as changed by the lead 2026-10-13).
func (s Scope) Bind(ctx context.Context, q RowsQuerier) (unrestricted bool, visible []uuid.UUID, err error) {
	if s.unrestricted {
		return true, []uuid.UUID{}, nil
	}
	if s.callerID == uuid.Nil {
		return false, []uuid.UUID{}, nil
	}
	if s.cache != nil {
		ids, err := s.cache.get(ctx, q, s.callerID)
		return false, ids, err
	}
	ids, err := loadVisibleSet(ctx, q, s.callerID)
	return false, ids, err
}

// BindSpaces returns the scope's own non-personal spaces — the bind value
// of the third placeholder of HiddenCountableExpr and the hidden-link
// fragments (INV-28 (4)): hidden issues count in unavailable* numbers only
// in these spaces. The set is skein_contribution_spaces's rows for
// the caller (migration 000073), read once per request like Bind's.
// Declared internal work and the empty scope bind an empty set — no reader,
// nothing hidden to count.
func (s Scope) BindSpaces(ctx context.Context, q RowsQuerier) ([]uuid.UUID, error) {
	if s.unrestricted || s.callerID == uuid.Nil {
		return []uuid.UUID{}, nil
	}
	if s.cache != nil {
		return s.cache.getSpaces(ctx, q, s.callerID)
	}
	return loadIDs(ctx, q, loadSpacesSQL, s.callerID, "own spaces")
}

// requestSet memoises one caller's visible set — and own spaces — for the
// lifetime of one request context.
type requestSet struct {
	mu     sync.Mutex
	caller uuid.UUID
	ids    []uuid.UUID
	loaded bool

	spacesCaller uuid.UUID
	spaces       []uuid.UUID
	spacesLoaded bool
}

func (r *requestSet) getSpaces(ctx context.Context, q RowsQuerier, caller uuid.UUID) ([]uuid.UUID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.spacesLoaded && r.spacesCaller == caller {
		return r.spaces, nil
	}
	ids, err := loadIDs(ctx, q, loadSpacesSQL, caller, "own spaces")
	if err != nil {
		return nil, err
	}
	r.spacesCaller, r.spaces, r.spacesLoaded = caller, ids, true
	return ids, nil
}

type requestSetKey struct{}

// WithRequestCache gives ctx a memo of the caller's visible set: every Bind
// of a scope derived from this context (FromContext) reads the relation at
// most once. The router installs it per request, after authentication.
func WithRequestCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, requestSetKey{}, &requestSet{})
}

func (r *requestSet) get(ctx context.Context, q RowsQuerier, caller uuid.UUID) ([]uuid.UUID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.loaded && r.caller == caller {
		return r.ids, nil
	}
	ids, err := loadVisibleSet(ctx, q, caller)
	if err != nil {
		return nil, err
	}
	r.caller, r.ids, r.loaded = caller, ids, true
	return ids, nil
}

const (
	loadVisibleSQL = `SELECT project_id FROM skein_project_access_set($1::uuid)`
	loadSpacesSQL  = `SELECT space_id FROM skein_contribution_spaces($1::uuid)`
)

// loadVisibleSet reads the caller's row set of the relation, ids only. It
// is also the set of projects he may work in: every level of the relation
// may (INV-30, authzsql.WorkerEligibleExpr; the instance admin's bare reach
// included since sk-741), so no second set is kept.
func loadVisibleSet(ctx context.Context, q RowsQuerier, caller uuid.UUID) ([]uuid.UUID, error) {
	return loadIDs(ctx, q, loadVisibleSQL, caller, "visible projects")
}

// loadIDs reads one uuid column of a per-caller set.
func loadIDs(ctx context.Context, q RowsQuerier, sql string, caller uuid.UUID, what string) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, sql, caller)
	if err != nil {
		return nil, fmt.Errorf("loading %s: %w", what, err)
	}
	defer rows.Close()
	ids := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning %s: %w", what, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", what, err)
	}
	return ids, nil
}

// unrestrictedKey marks a context as trusted server-internal work. Its own
// unexported type keeps it out of reach of every other package: nothing
// outside this file can forge the marker onto a request context.
type unrestrictedKey struct{}

// WithUnrestricted declares that ctx is server-internal work that
// legitimately reads across every project — a background worker, a
// migration, a test fixture — and NOT a request whose principal went
// missing.
func WithUnrestricted(ctx context.Context) context.Context {
	return context.WithValue(ctx, unrestrictedKey{}, true)
}

func isDeclaredUnrestricted(ctx context.Context) bool {
	declared, _ := ctx.Value(unrestrictedKey{}).(bool)
	return declared
}

// RequestCache is the router middleware that gives every request its own
// memo of the caller's visible set (WithRequestCache): the relation is read
// at most once per request, and never reused by another request. The memo
// is keyed by the caller at the time of the read, so the middleware may sit
// before authentication.
func RequestCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(WithRequestCache(r.Context())))
	})
}
