package projects

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/auth"
)

// TestEnsureProjectVisible_ExternalAdminClamped is the sk-163 S2 regression:
// the admin ceiling must hold at the SQL layer. A caller whose forwarded role
// says "admin" but who is an external collaborator must NOT resolve to the
// 'admin' access level — they fall through to the membership branches
// exactly like a non-admin (the GitLab #417664 visibility-ceiling class bug).
//
// The DB CHECK from migration 000033 makes the external+admin *row*
// impossible to persist, so the tamper is simulated where it could actually
// originate — a forged/stale role claim in ctx. Since sk-674.18 T008 the
// claim grants nothing at all: the access relation reads role and
// is_external from users (`NOT u.is_external` is its ceiling).
func TestEnsureProjectVisible_ExternalAdminClamped(t *testing.T) {
	pool := newTestPool(t)

	owner := seedUserForRepoTest(t, pool)
	external := seedUserForRepoTest(t, pool)
	internalAdmin := seedUserForRepoTest(t, pool)

	// Project owned by `owner`; neither `external` nor `internalAdmin` is a
	// member, so only the admin bypass could make it visible to them.
	pid := seedNamedProject(t, pool, owner, "s2ceiling-"+uuid.NewString()[:8], "s2c"+uuid.NewString()[:6])

	// External caller with a tampered "admin" role claim in ctx → clamped.
	extCtx := auth.WithIsExternal(
		auth.WithUserRole(
			auth.WithUserID(context.Background(), external), "admin"),
		true)
	level, err := EnsureProjectVisible(extCtx, pool, pid, external)
	if !errors.Is(err, ErrProjectNotVisible) {
		t.Fatalf("external+admin caller: got level=%q err=%v, want ErrProjectNotVisible (clamped)", level, err)
	}

	// Positive control / no-regression: an INTERNAL admin (is_external
	// false, the default) of a non-member project still bypasses
	// project_members and resolves to AccessAdmin exactly as before S2.
	// Since sk-674.18 T008 the role is read from users by the access
	// relation (D-2), so the admin is an admin in the database.
	if _, err := pool.Exec(context.Background(), `UPDATE users SET role = 'admin' WHERE id = $1`, internalAdmin); err != nil {
		t.Fatalf("make the internal admin: %v", err)
	}
	adminCtx := auth.WithUserRole(
		auth.WithUserID(context.Background(), internalAdmin), "admin")
	level, err = EnsureProjectVisible(adminCtx, pool, pid, internalAdmin)
	if err != nil {
		t.Fatalf("internal admin caller: unexpected err=%v", err)
	}
	if level != AccessAdmin {
		t.Fatalf("internal admin caller: got level=%q, want AccessAdmin (no regression)", level)
	}
}

// TestResolve_ExternalAdminClamped pins the sk-163 S2 ceiling on the fuzzy
// project resolver: an external caller with a tampered "admin" role claim in
// ctx must NOT resolve a project they are not a member of — the admin arm of
// every resolve query (exact-name / id_prefix / alias / repo-url / lexical)
// is clamped by is_external, mirroring EnsureProjectVisible. Without the
// clamp, project resolution would leak invisible projects to an external
// user whose role was forged/stale.
func TestResolve_ExternalAdminClamped(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)

	owner := seedUserForRepoTest(t, pool)
	external := seedUserForRepoTest(t, pool)
	internalAdmin := seedUserForRepoTest(t, pool)

	// Project owned by `owner`; the caller is NOT a member. Unique name so
	// the exact-name short-circuit (admin arm) is what could surface it.
	name := "s2resolve-" + uuid.NewString()[:8]
	pid := seedNamedProject(t, pool, owner, name, "s2r"+uuid.NewString()[:6])

	// External caller with a tampered admin claim → must NOT resolve.
	extCtx := auth.WithIsExternal(context.Background(), true)
	res, err := store.Resolve(extCtx, name, external)
	if err != nil {
		t.Fatalf("Resolve(external): %v", err)
	}
	if resolveHits(res, pid) {
		t.Fatalf("external+admin caller resolved non-member project %s (match=%v candidates=%d)",
			pid, res.Match, len(res.Candidates))
	}

	// Positive control / no-regression: an internal admin (no is_external in
	// ctx) still resolves the project via the admin bypass — an admin in
	// users, which the access relation reads (sk-674.18 T009).
	if _, err := pool.Exec(context.Background(), `UPDATE users SET role = 'admin' WHERE id = $1`, internalAdmin); err != nil {
		t.Fatalf("make the internal admin: %v", err)
	}
	res, err = store.Resolve(context.Background(), name, internalAdmin)
	if err != nil {
		t.Fatalf("Resolve(internal admin): %v", err)
	}
	if !resolveHits(res, pid) {
		t.Fatalf("internal admin failed to resolve project %s (no regression expected)", pid)
	}
}

// TestResolveLexical_ExternalAdminClamped drives the resolveLexical fallthrough
// path specifically — the dynamic addParam permClause is the placeholder
// builder most prone to an ordinal bug, and the exact-name test above
// short-circuits before reaching it. A distinctive substring query misses
// exact-name/prefix/alias/repo-url and falls through to the lexical ILIKE arm,
// so the is_external clamp in the dynamic clause is what's exercised here.
func TestResolveLexical_ExternalAdminClamped(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)

	owner := seedUserForRepoTest(t, pool)
	external := seedUserForRepoTest(t, pool)
	internalAdmin := seedUserForRepoTest(t, pool)

	// Name CONTAINS the token but is longer than it, so the exact-name
	// short-circuit misses and Resolve falls through to resolveLexical, where
	// the `%token%` ILIKE arm matches.
	token := "lexclamp" + uuid.NewString()[:8]
	name := token + "-project"
	pid := seedNamedProject(t, pool, owner, name, "lc"+uuid.NewString()[:6])

	// External caller + tampered admin claim + fuzzy substring query → the
	// lexical admin bypass must stay clamped: no non-member project surfaces.
	extCtx := auth.WithIsExternal(context.Background(), true)
	res, err := store.Resolve(extCtx, token, external)
	if err != nil {
		t.Fatalf("Resolve(external lexical): %v", err)
	}
	if resolveHits(res, pid) {
		t.Fatalf("external+admin caller surfaced non-member project %s via lexical path", pid)
	}

	// No-regression control: an internal admin surfaces it through the lexical
	// admin bypass.
	if _, err := pool.Exec(context.Background(), `UPDATE users SET role = 'admin' WHERE id = $1`, internalAdmin); err != nil {
		t.Fatalf("make the internal admin: %v", err)
	}
	res, err = store.Resolve(context.Background(), token, internalAdmin)
	if err != nil {
		t.Fatalf("Resolve(internal admin lexical): %v", err)
	}
	if !resolveHits(res, pid) {
		t.Fatalf("internal admin failed to resolve project %s via lexical (no regression expected)", pid)
	}
}
