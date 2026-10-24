package spaces

// membership.go — the membership primitives of spaces (sk-674.18 T018, K-7,
// US-17, US-7, INV-6, INV-7, INV-12, SC-017).
//
// # Serialisation (K-7)
//
// Every operation that changes who is in a space or who sees its projects
// takes the space's row lock FIRST — `SELECT … FROM spaces WHERE id = $1
// FOR UPDATE` — and only then reads or writes the user's rows. Operations
// on a project of the space (adding a member, a role, a visibility change)
// take the same lock (LockProjectSpace). A transaction that still loses a
// serialisation race (40001 / 40P01) is retried as a whole, at most three
// attempts (WithRetry); after that the operation fails with
// ErrConcurrentChange — a business refusal, 422 `concurrent_change` in REST
// and MCP alike, never 500 or 409.
//
// # Events and signals
//
// A mass change writes ONE space-level event (events.entity_type 'space',
// type space.membership_changed, the projects in the payload) instead of an
// event per project (SC-017). It is history: written already published, so
// the outbox dispatcher does not take it; the live update is the personal
// signal `spaces` and the access_changed event, both published by the
// transaction's collector only after the final commit (signals.BeginTxFunc).
//
// # Sources of a membership row (INV-6)
//
// Space operations (joining, a visibility change) set and clear only the
// source "from space" (via_space); project operations only "manually"
// (via_manual). A row without a source does not exist. Leaving the space is
// the one exception: it deletes the user's rows with both sources.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/authz"
	"skein-api/internal/signals"
)

// Sentinel errors of the primitives. Each maps to the code of the same name
// in this package's registry (contract.go) in REST and MCP alike.
var (
	// ErrConcurrentChange — the retries are exhausted (CodeConcurrentChange).
	ErrConcurrentChange = errors.New("spaces: concurrent membership change")
	// ErrSpaceNotFound — no such space (CodeSpaceNotFound).
	ErrSpaceNotFound = errors.New("spaces: space not found")
	// ErrPersonalSpace — the operation does not apply to a "Personal"
	// (CodePersonalSpace, INV-8).
	ErrPersonalSpace = errors.New("spaces: personal space")
	// ErrExternalUser — an external user is never in a space
	// (CodeExternalUser, E-16).
	ErrExternalUser = errors.New("spaces: external user")
	// ErrNotAMember — the user is not a participant (CodeNotAMember).
	ErrNotAMember = errors.New("spaces: not a member")
	// ErrOwnerCannotLeave — the owner neither leaves nor is removed
	// (CodeOwnerCannotLeave / CodeOwnerCannotBeRemoved, INV-10).
	ErrOwnerCannotLeave = errors.New("spaces: the owner cannot leave")
	// ErrUserNotFound — no such user.
	ErrUserNotFound = errors.New("spaces: user not found")
)

// maxAttempts bounds WithRetry (K-7: three attempts).
const maxAttempts = 3

// WithRetry runs fn in a SERIALIZABLE transaction opened by
// signals.BeginTxFunc — so its signals publish only after the final commit —
// and retries the whole transaction on a serialisation failure or deadlock
// (40001 / 40P01), at most three attempts. Exhausted retries return
// ErrConcurrentChange; any other error is returned as is.
func WithRetry(ctx context.Context, db signals.TxBeginner, fn func(pgx.Tx) error) error {
	return retry(func() error {
		return signals.BeginTxFunc(ctx, db, pgx.TxOptions{IsoLevel: pgx.Serializable}, fn)
	})
}

// retry runs attempt up to maxAttempts times while it fails with a
// serialisation failure or deadlock; any other outcome is returned at once.
func retry(attempt func() error) error {
	var err error
	for i := 0; i < maxAttempts; i++ {
		err = attempt()
		if !isRetryable(err) {
			return err
		}
	}
	return fmt.Errorf("%w: %v", ErrConcurrentChange, err)
}

func isRetryable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01")
}

// lockedSpace is the locked space row.
type lockedSpace struct {
	ID      uuid.UUID
	ShortID string
	Kind    string
	Name    string
	OwnerID uuid.UUID
}

// LockSpace takes the space's row lock — the first statement of every
// membership operation (K-7) — and returns the row. ErrSpaceNotFound when
// there is no such space.
func LockSpace(ctx context.Context, tx pgx.Tx, spaceID uuid.UUID) (lockedSpace, error) {
	var s lockedSpace
	err := tx.QueryRow(ctx, `
SELECT id, short_id, kind, name, owner_id FROM spaces WHERE id = $1 FOR UPDATE`, spaceID).
		Scan(&s.ID, &s.ShortID, &s.Kind, &s.Name, &s.OwnerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, ErrSpaceNotFound
	}
	if err != nil {
		return s, fmt.Errorf("locking space %s: %w", spaceID, err)
	}
	return s, nil
}

// LockProjectSpace takes the row lock of the space of project — the lock an
// operation on a project of a space takes (K-7). Every project has a space
// since 000080 (T024); a project id that does not exist locks nothing.
func LockProjectSpace(ctx context.Context, tx pgx.Tx, projectID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `
SELECT 1 FROM spaces s JOIN projects p ON p.space_id = s.id WHERE p.id = $1 FOR UPDATE OF s`, projectID); err != nil {
		return fmt.Errorf("locking the space of project %s: %w", projectID, err)
	}
	return nil
}

// ShareProjectSpace takes the space's row lock in SHARE mode for an
// assignment in project (K-7: setting an assignee takes the lock of the
// project's space). A shared lock waits for a membership operation holding
// the row FOR UPDATE — leaving, a visibility change — so an assignment
// started while a user leaves completes after him, and the leave's
// INV-12 sees it or it sees the leave; assignments do not wait for each
// other. Taken as the first statement of the assignment's transaction, so
// the order space → issue is the same as the membership operations'.
func ShareProjectSpace(ctx context.Context, tx pgx.Tx, projectID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `
SELECT 1 FROM spaces s JOIN projects p ON p.space_id = s.id WHERE p.id = $1 FOR SHARE OF s`, projectID); err != nil {
		return fmt.Errorf("locking the space of project %s: %w", projectID, err)
	}
	return nil
}

// ShareIssueSpace is ShareProjectSpace for the project of issue issueID —
// for paths that know the issue before its project. The issue row itself is
// not locked.
func ShareIssueSpace(ctx context.Context, tx pgx.Tx, issueID string) error {
	if _, err := tx.Exec(ctx, `
SELECT 1 FROM spaces s JOIN projects p ON p.space_id = s.id JOIN issues i ON i.project_id = p.id
 WHERE i.id = $1 FOR SHARE OF s`, issueID); err != nil {
		return fmt.Errorf("locking the space of issue %s: %w", issueID, err)
	}
	return nil
}

// Actor is who performs a membership operation, as the events name them.
type Actor = authz.RevokeActor

// Store runs the membership primitives.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a Store over pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Join makes user a participant of space (US-17 "joining"): one
// transaction gives the user the source "from space" on every `space`
// project of the space (the row is created when missing), writes one
// space-level event and, after commit, signals the user (`spaces`: added)
// and publishes the projects gained. Joining a space one is already in is a
// no-op. The callers (accepting an invitation, an admin adding a user —
// T040, T023) decide who may; this is the mechanism.
func (s *Store) Join(ctx context.Context, spaceID, userID uuid.UUID, actor Actor) error {
	return WithRetry(ctx, s.pool, func(tx pgx.Tx) error { return JoinInTx(ctx, tx, spaceID, userID, actor) })
}

// JoinInTx is Join inside the caller's transaction, which must be opened by
// signals.BeginTxFunc (WithRetry) and is where the space lock is taken —
// for a caller whose own operation joins as one of its steps (accepting an
// invitation, signup by invitation).
func JoinInTx(ctx context.Context, tx pgx.Tx, spaceID, userID uuid.UUID, actor Actor) error {
	sp, err := LockSpace(ctx, tx, spaceID)
	if err != nil {
		return err
	}
	if sp.Kind == "personal" {
		return ErrPersonalSpace
	}
	// Under the user's row lock (after the space's, the order of the
	// account deletion): a deleted account never joins
	// — a join waiting for the deletion re-reads the user on its retry.
	var external, deleted bool
	if err := tx.QueryRow(ctx, `SELECT is_external, deleted_at IS NOT NULL FROM users WHERE id = $1 FOR SHARE`, userID).
		Scan(&external, &deleted); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUserNotFound
		}
		return fmt.Errorf("reading user %s: %w", userID, err)
	}
	if deleted {
		return ErrUserNotFound
	}
	if external {
		return ErrExternalUser
	}
	// What the user sees in the space BEFORE he becomes a participant: a
	// `members` project with his manual row becomes visible by the joining
	// itself, so it is a gain.
	before, err := visibleIn(ctx, tx, userID, spaceID)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
INSERT INTO space_members (space_id, user_id, role) VALUES ($1, $2, 'member')
ON CONFLICT (space_id, user_id) DO NOTHING`, spaceID, userID)
	if err != nil {
		return fmt.Errorf("adding the participant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil // already a participant
	}
	projects, err := collect(ctx, tx, `
INSERT INTO project_members (project_id, user_id, role, added_at, added_by, via_manual, via_space)
SELECT p.id, $2, 'member', now(), $3, false, true
  FROM projects p
 WHERE p.space_id = $1 AND p.visibility = 'space'
ON CONFLICT (project_id, user_id) DO UPDATE SET via_space = true
RETURNING project_id`, spaceID, userID, addedBy(actor, userID))
	if err != nil {
		return fmt.Errorf("adding the space source: %w", err)
	}
	after, err := visibleIn(ctx, tx, userID, spaceID)
	if err != nil {
		return err
	}
	if err := writeSpaceEvent(ctx, tx, sp, actor, "joined", userID, projects, ""); err != nil {
		return err
	}
	signals.AddSpaceEvent(tx, userID, signals.SpaceEvent{Reason: signals.SpaceAdded, SpaceID: sp.ShortID, SpaceName: sp.Name})
	signals.Add(tx, userID, signals.AreaProjects, signals.AreaLens, signals.AreaTasks)
	signals.AddAccessChange(tx, userID, nil, minus(after, before), nil)
	return nil
}

// Leave takes user out of space (US-7: leaving, being removed, account
// deletion): one transaction deletes the participation and the user's
// membership rows in every project of the space — both sources, the one
// exception of INV-6 — applies INV-12 to every project of the space the
// user no longer sees (authz.RevokeProjectAccess with reason), writes one
// space-level event and, after commit, publishes the projects and the space
// lost (and, with removed set, the `removed` toast). The owner neither
// leaves nor is removed (ErrOwnerCannotLeave, INV-10).
func (s *Store) Leave(ctx context.Context, spaceID, userID uuid.UUID, reason string, removed bool, actor Actor) error {
	return WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		return LeaveInTx(ctx, tx, spaceID, userID, reason, removed, actor)
	})
}

// LeaveInTx is Leave inside the caller's transaction (opened by
// signals.BeginTxFunc) — for operations of which leaving is one step
// (account deletion, T041).
func LeaveInTx(ctx context.Context, tx pgx.Tx, spaceID, userID uuid.UUID, reason string, removed bool, actor Actor) error {
	sp, err := LockSpace(ctx, tx, spaceID)
	if err != nil {
		return err
	}
	if sp.Kind == "personal" {
		return ErrPersonalSpace
	}
	if sp.OwnerID == userID {
		return ErrOwnerCannotLeave
	}
	// The projects of the space the user sees before leaving: only these are
	// named lost in access_changed — a `members` project he never saw is not
	// published (3bdd921). The INV-12 cleanup also covers the projects whose
	// memberships the removal deletes: a deactivated user sees nothing, yet
	// keeps his assignments, claims, watches and reminders (E-15) until he
	// leaves.
	before, err := visibleIn(ctx, tx, userID, spaceID)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM space_members WHERE space_id = $1 AND user_id = $2`, spaceID, userID)
	if err != nil {
		return fmt.Errorf("removing the participant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotAMember
	}
	dropped, err := collect(ctx, tx, `
DELETE FROM project_members pm USING projects p
 WHERE pm.project_id = p.id AND p.space_id = $1 AND pm.user_id = $2
RETURNING pm.project_id`, spaceID, userID)
	if err != nil {
		return fmt.Errorf("removing the memberships: %w", err)
	}
	candidates := slices.Clone(before)
	for _, id := range dropped {
		if !slices.Contains(candidates, id) {
			candidates = append(candidates, id)
		}
	}
	if _, err := authz.RevokeProjectAccessSeen(ctx, tx, userID, candidates, before, reason, actor); err != nil {
		return err
	}
	change := "left"
	if removed {
		change = "removed"
	}
	if err := writeSpaceEvent(ctx, tx, sp, actor, change, userID, dropped, reason); err != nil {
		return err
	}
	if removed {
		signals.AddSpaceEvent(tx, userID, signals.SpaceEvent{Reason: signals.SpaceRemoved, SpaceID: sp.ShortID, SpaceName: sp.Name})
	}
	signals.Add(tx, userID, signals.AreaSpaces, signals.AreaProjects)
	// The space is lost only if the user no longer sees it: an active
	// instance admin (with the flag on) keeps every non-personal space with
	// adminAccess (US-6) — publishing it lost would close a page he may
	// still open.
	var stillSees bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM users u, instance_settings st
                WHERE u.id = $1 AND st.id = 1 AND st.admin_enabled
                  AND u.role = 'admin' AND NOT u.is_external
                  AND u.deactivated_at IS NULL AND u.deleted_at IS NULL)`, userID).Scan(&stillSees); err != nil {
		return fmt.Errorf("reading whether the user still sees the space: %w", err)
	}
	if !stillSees {
		signals.AddAccessChange(tx, userID, nil, nil, []string{sp.ShortID})
	}
	return nil
}

// SetVisibility switches project between `space` and `members` (US-17), one
// transaction under the lock of the project's space:
//
//   - members → space: every participant of the space gets the source "from
//     space" (the row is created when missing, INV-7);
//   - space → members: the source "from space" is cleared on every
//     row, rows left without a source are deleted, and everyone who no
//     longer sees the project goes through INV-12 — a project owner stays,
//     because becoming owner set the source "manually" (E-28).
//
// One space-level event; after commit access_changed for everyone whose
// access changed. Setting the visibility a project already has is a no-op.
// Who may switch is the caller's check (authz.ActionChangeVisibility).
func (s *Store) SetVisibility(ctx context.Context, projectID uuid.UUID, visibility string, actor Actor) error {
	if visibility != VisibilitySpace && visibility != VisibilityMembers {
		return fmt.Errorf("spaces: invalid visibility %q", visibility)
	}
	return WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		return SetVisibilityInTx(ctx, tx, projectID, visibility, actor)
	})
}

// SetVisibilityInTx is SetVisibility inside the caller's transaction
// (opened by signals.BeginTxFunc) — for a project update that switches the
// visibility among other changes (T024).
func SetVisibilityInTx(ctx context.Context, tx pgx.Tx, projectID uuid.UUID, visibility string, actor Actor) error {
	if visibility != VisibilitySpace && visibility != VisibilityMembers {
		return fmt.Errorf("spaces: invalid visibility %q", visibility)
	}
	var spaceID *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT space_id FROM projects WHERE id = $1`, projectID).Scan(&spaceID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return authz.ErrNotVisible
		}
		return fmt.Errorf("reading project %s: %w", projectID, err)
	}
	if spaceID == nil {
		return fmt.Errorf("spaces: project %s has no space", projectID)
	}
	sp, err := LockSpace(ctx, tx, *spaceID)
	if err != nil {
		return err
	}
	var current string
	if err := tx.QueryRow(ctx, `SELECT visibility FROM projects WHERE id = $1 FOR UPDATE`, projectID).Scan(&current); err != nil {
		return fmt.Errorf("locking project %s: %w", projectID, err)
	}
	if current == visibility {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE projects SET visibility = $2, updated_at = now() WHERE id = $1`,
		projectID, visibility); err != nil {
		return fmt.Errorf("switching visibility: %w", err)
	}
	if visibility == VisibilitySpace {
		// The participants who see the project before the switch (an admin
		// by INV-1 g, the space's implicit owner, a holder of a manual row)
		// gain nothing: a gain is a change of visibility, not a row written
		//
		sawBefore, err := collect(ctx, tx, `
SELECT sm.user_id FROM space_members sm
  JOIN LATERAL skein_project_access_set(sm.user_id) va ON va.project_id = $1
 WHERE sm.space_id = $2`,
			projectID, *spaceID)
		if err != nil {
			return fmt.Errorf("reading who sees the project: %w", err)
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO project_members (project_id, user_id, role, added_at, added_by, via_manual, via_space)
SELECT $1, sm.user_id, 'member', now(), $3, false, true
  FROM space_members sm
 WHERE sm.space_id = $2
ON CONFLICT (project_id, user_id) DO UPDATE SET via_space = true`, projectID, *spaceID, addedBy(actor, uuid.Nil)); err != nil {
			return fmt.Errorf("adding the space source: %w", err)
		}
		seesNow, err := collect(ctx, tx, `
SELECT sm.user_id FROM space_members sm
  JOIN LATERAL skein_project_access_set(sm.user_id) va ON va.project_id = $1
 WHERE sm.space_id = $2`,
			projectID, *spaceID)
		if err != nil {
			return fmt.Errorf("reading who sees the project: %w", err)
		}
		gained := minus(seesNow, sawBefore)
		for _, u := range gained {
			signals.Add(tx, u, signals.AreaProjects, signals.AreaLens, signals.AreaTasks)
			signals.AddAccessChange(tx, u, nil, []uuid.UUID{projectID}, nil)
		}
		return writeSpaceEvent(ctx, tx, sp, actor, "visibility_space", uuid.Nil, []uuid.UUID{projectID}, "")
	}
	// A row whose only source is the space would be left without a
	// source: it is deleted (a row without a source does not exist,
	// INV-6); the other rows lose the space source.
	dropped, err := collect(ctx, tx, `
DELETE FROM project_members WHERE project_id = $1 AND via_space AND NOT via_manual
RETURNING user_id`, projectID)
	if err != nil {
		return fmt.Errorf("deleting rows of the space source only: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE project_members SET via_space = false WHERE project_id = $1 AND via_space`,
		projectID); err != nil {
		return fmt.Errorf("clearing the space source: %w", err)
	}
	for _, u := range dropped {
		if _, err := authz.RevokeProjectAccess(ctx, tx, u, []uuid.UUID{projectID}, authz.RevokeVisibilityToMem, actor); err != nil {
			return err
		}
	}
	return writeSpaceEvent(ctx, tx, sp, actor, "visibility_members", uuid.Nil, []uuid.UUID{projectID}, "")
}

// visibleIn is the projects of space the user sees now, by the relation.
func visibleIn(ctx context.Context, tx pgx.Tx, userID, spaceID uuid.UUID) ([]uuid.UUID, error) {
	ids, err := collect(ctx, tx, `
SELECT va.project_id FROM skein_project_access_set($1::uuid) va
  JOIN projects p ON p.id = va.project_id
 WHERE p.space_id = $2`, userID, spaceID)
	if err != nil {
		return nil, fmt.Errorf("reading the visible projects of the space: %w", err)
	}
	return ids, nil
}

// writeSpaceEvent writes the one space-level event of a mass operation
// (SC-017), already published (see the file header).
func writeSpaceEvent(ctx context.Context, tx pgx.Tx, sp lockedSpace, actor Actor, change string, userID uuid.UUID, projects []uuid.UUID, reason string) error {
	payload := map[string]any{"change": change, "spaceId": sp.ShortID, "projects": nonNil(projects)}
	if userID != uuid.Nil {
		payload["userId"] = userID
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var userArg, sessionArg, reasonArg any
	if actor.UserID != uuid.Nil {
		userArg = actor.UserID
	}
	if actor.SessionID != "" {
		sessionArg = actor.SessionID
	}
	if reason != "" {
		reasonArg = reason
	}
	actorType := actor.Type
	if actorType == "" {
		actorType = "system"
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO events (type, actor, actor_user_id, actor_session_id, actor_type,
                    entity_type, entity_id, project_id, reason, payload, published_at)
VALUES ('space.membership_changed', $1, $2, $3, $4, 'space', $5, NULL, $6, $7::jsonb, now())`,
		actor.Username, userArg, sessionArg, actorType, sp.ID.String(), reasonArg, string(raw)); err != nil {
		return fmt.Errorf("writing the space event: %w", err)
	}
	return nil
}

// addedBy is project_members.added_by of rows a space operation creates: the
// actor, or the user himself for a system actor.
func addedBy(actor Actor, userID uuid.UUID) uuid.UUID {
	if actor.UserID != uuid.Nil {
		return actor.UserID
	}
	return userID
}

func collect(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func minus(a, b []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{}, len(b))
	for _, id := range b {
		seen[id] = struct{}{}
	}
	var out []uuid.UUID
	for _, id := range a {
		if _, ok := seen[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}

func nonNil(ids []uuid.UUID) []uuid.UUID {
	if ids == nil {
		return []uuid.UUID{}
	}
	return ids
}
