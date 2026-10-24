package spaces

// store.go — the reads of the spaces REST surface (sk-674.18 T023): the
// spaces a caller sees with their counters, the members and invitations of
// one space, the numbers of the previews. Who sees a space is
// authz.SpaceSeenExpr — the one rule; nothing here decides it.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"skein-api/internal/assignee"
	"skein-api/internal/authz"
	"skein-api/internal/person"
)

// querier is what the reads need from a pool or a transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// spaceRow is one space as the caller sees it, with its counters.
type spaceRow struct {
	ID      uuid.UUID
	ShortID string
	Kind    string
	Name    string
	OwnerID uuid.UUID
	// Role is the caller's role; nil when he sees the space only as the
	// instance admin (adminAccess).
	Role         *string
	MemberCount  int
	Pending      int
	ProjectCount int
	StorageUsed  int64
}

// validPendingExpr is «the invitation still occupies a seat»: pending and
// not yet expired — the predicate of the token protocol
// (tokens/protocol.go), so an invitation whose link no longer works holds no
// seat even before the expiry sweep marks it.
const validPendingExpr = `i.status = 'pending' AND i.expires_at > now()`

// seenSpaces returns the spaces caller sees (authz.SpaceSeenExpr) in column
// order (K-20: personal → common → the rest by name, then id); with short
// set, only that space (none if it does not exist or is not seen — INV-3).
func seenSpaces(ctx context.Context, q querier, caller uuid.UUID, short string) ([]spaceRow, error) {
	rows, err := q.Query(ctx, `
SELECT s.id, s.short_id, s.kind, s.name, s.owner_id, sm.role,
       (SELECT count(*) FROM space_members x WHERE x.space_id = s.id),
       (SELECT count(*) FROM space_invitations i WHERE i.space_id = s.id AND `+validPendingExpr+`),
       (SELECT count(*) FROM projects p WHERE p.space_id = s.id),
       COALESCE((SELECT u.bytes FROM space_storage_usage u WHERE u.space_id = s.id), 0)
  FROM spaces s
  LEFT JOIN space_members sm ON sm.space_id = s.id AND sm.user_id = $1
 WHERE `+authz.SpaceSeenExpr("s", "$1")+`
   AND ($2 = '' OR s.short_id = $2)
 ORDER BY CASE s.kind WHEN 'personal' THEN 0 WHEN 'common' THEN 1 ELSE 2 END, lower(s.name), s.id`,
		caller, short)
	if err != nil {
		return nil, fmt.Errorf("reading the caller's spaces: %w", err)
	}
	defer rows.Close()
	var out []spaceRow
	for rows.Next() {
		var r spaceRow
		if err := rows.Scan(&r.ID, &r.ShortID, &r.Kind, &r.Name, &r.OwnerID, &r.Role,
			&r.MemberCount, &r.Pending, &r.ProjectCount, &r.StorageUsed); err != nil {
			return nil, fmt.Errorf("reading the caller's spaces: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// seenSpace is seenSpaces for one short id; ErrSpaceNotFound when the space
// does not exist or the caller does not see it (one answer, INV-3).
func seenSpace(ctx context.Context, q querier, caller uuid.UUID, short string) (spaceRow, error) {
	if short == "" {
		return spaceRow{}, ErrSpaceNotFound
	}
	rows, err := seenSpaces(ctx, q, caller, short)
	if err != nil {
		return spaceRow{}, err
	}
	if len(rows) == 0 {
		return spaceRow{}, ErrSpaceNotFound
	}
	return rows[0], nil
}

// callerFacts are the facts about the caller the decisions need, read from
// the database (never from request headers).
type callerFacts struct {
	External bool
	Admin    bool
	// OwnedCount is the regular spaces he owns — what
	// SKEIN_SPACE_OWNED_LIMIT limits (INV-14: the personal and the common
	// space do not count).
	OwnedCount int
	// Active: neither blocked nor deleted. A request authenticated before
	// the account was blocked or deleted may reach the creating transaction
	// after it; read under the user's lock, it refuses.
	Active bool
}

// readCaller reads callerFacts; lock takes the user's row lock, so two
// creations by one user are counted one after the other, and a creation
// and the account's deletion one after the other.
func readCaller(ctx context.Context, q querier, caller uuid.UUID, lock bool) (callerFacts, error) {
	var f callerFacts
	sql := `SELECT u.is_external, ` + authz.EffectiveAdminExpr("$1") + `,
       (SELECT count(*) FROM spaces s WHERE s.owner_id = u.id AND s.kind = 'regular'),
       u.deactivated_at IS NULL AND u.deleted_at IS NULL
  FROM users u WHERE u.id = $1`
	if lock {
		sql += ` FOR UPDATE OF u`
	}
	err := q.QueryRow(ctx, sql, caller).Scan(&f.External, &f.Admin, &f.OwnedCount, &f.Active)
	if errors.Is(err, pgx.ErrNoRows) {
		return f, ErrUserNotFound
	}
	if err != nil {
		return f, fmt.Errorf("reading the caller: %w", err)
	}
	return f, nil
}

// members lists the participants of space: owner first, then by username.
func members(ctx context.Context, q querier, spaceID uuid.UUID) ([]SpaceMember, error) {
	rows, err := q.Query(ctx, `
SELECT u.id, u.username, u.name, sm.role, u.deactivated_at IS NOT NULL, `+person.StatusExpr("u")+`
  FROM space_members sm JOIN users u ON u.id = sm.user_id
 WHERE sm.space_id = $1
 ORDER BY sm.role = 'owner' DESC, lower(u.username), u.id`, spaceID)
	if err != nil {
		return nil, fmt.Errorf("reading the members: %w", err)
	}
	defer rows.Close()
	out := []SpaceMember{}
	for rows.Next() {
		var m SpaceMember
		var id uuid.UUID
		var status *person.Status
		if err := rows.Scan(&id, &m.Username, &m.Name, &m.Role, &m.Deactivated, &status); err != nil {
			return nil, fmt.Errorf("reading the members: %w", err)
		}
		if status != nil {
			m.Status = *status
		}
		m.UserID = id.String()
		out = append(out, m)
	}
	return out, rows.Err()
}

// declinedShownFor — a declined invitation is shown neutrally for 7 days,
// then gone (contract.go#InvitationNotAccepted).
const declinedShownFor = 7 * 24 * time.Hour

// invitations lists the pending (still valid) and recently declined
// invitations of space, newest first.
func invitations(ctx context.Context, q querier, spaceID uuid.UUID) ([]SpaceInvitation, error) {
	rows, err := q.Query(ctx, `
SELECT i.id, i.email, i.status, u.username, i.expires_at, i.created_at
  FROM space_invitations i JOIN users u ON u.id = i.invited_by
 WHERE i.space_id = $1
   AND ((`+validPendingExpr+`) OR (i.status = 'declined' AND i.resolved_at > now() - $2::bigint * interval '1 second'))
 ORDER BY i.created_at DESC, i.id`, spaceID, int64(declinedShownFor/time.Second))
	if err != nil {
		return nil, fmt.Errorf("reading the invitations: %w", err)
	}
	defer rows.Close()
	out := []SpaceInvitation{}
	for rows.Next() {
		var inv SpaceInvitation
		var id uuid.UUID
		var status string
		var expires time.Time
		if err := rows.Scan(&id, &inv.Email, &status, &inv.InvitedBy, &expires, &inv.CreatedAt); err != nil {
			return nil, fmt.Errorf("reading the invitations: %w", err)
		}
		inv.ID = id.String()
		inv.CreatedAt = inv.CreatedAt.UTC()
		if status == "pending" {
			inv.Status = InvitationPending
			e := expires.UTC()
			inv.ExpiresAt = &e
		} else {
			inv.Status = InvitationNotAccepted
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// deletionPreview reads what deleting space would destroy now — the same
// sets DeleteInTx deletes: every project (archived included), their issues,
// the storage counter, every pending invitation (revoked regardless of
// expiry).
func deletionPreview(ctx context.Context, q querier, spaceID uuid.UUID) (SpaceDeletionPreview, error) {
	var p SpaceDeletionPreview
	err := q.QueryRow(ctx, `
SELECT (SELECT count(*) FROM projects p WHERE p.space_id = s.id),
       (SELECT count(*) FROM issues i JOIN projects p ON p.id = i.project_id WHERE p.space_id = s.id),
       COALESCE((SELECT u.bytes FROM space_storage_usage u WHERE u.space_id = s.id), 0),
       (SELECT count(*) FROM space_invitations i WHERE i.space_id = s.id AND i.status = 'pending'),
       o.username
  FROM spaces s JOIN users o ON o.id = s.owner_id
 WHERE s.id = $1`, spaceID).Scan(&p.ProjectCount, &p.IssueCount, &p.StorageBytes, &p.PendingInvitations, &p.OwnerUsername)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrSpaceNotFound
	}
	if err != nil {
		return p, fmt.Errorf("reading the deletion preview: %w", err)
	}
	return p, nil
}

// removalTally is what the INV-12 cleanup of a removal touches, counted in
// the space's projects: the user's open assignments and his claims (his
// agents' included — claims are per user).
type removalTally struct {
	assigned int
	claimed  int
}

func tallyRemoval(ctx context.Context, q querier, spaceID, userID uuid.UUID) (removalTally, error) {
	var t removalTally
	err := q.QueryRow(ctx, `
SELECT (SELECT count(*) FROM issues i JOIN projects p ON p.id = i.project_id
         WHERE p.space_id = $1 AND i.status <> 'closed' AND i.assignee IS NOT NULL
           AND `+assignee.NamesUser("i", "$2::uuid")+`),
       (SELECT count(*) FROM issues i JOIN projects p ON p.id = i.project_id
         WHERE p.space_id = $1 AND i.locked_by_user_id = $2::uuid)`, spaceID, userID).Scan(&t.assigned, &t.claimed)
	if err != nil {
		return t, fmt.Errorf("counting the removal's consequences: %w", err)
	}
	return t, nil
}

// ownedProjects lists the projects of space where user holds the project
// owner role, by name.
func ownedProjects(ctx context.Context, q querier, spaceID, userID uuid.UUID) ([]ProjectRef, error) {
	rows, err := q.Query(ctx, `
SELECT p.id, p.name, p.id_prefix
  FROM project_members pm JOIN projects p ON p.id = pm.project_id
 WHERE p.space_id = $1 AND pm.user_id = $2 AND pm.role = 'owner'
 ORDER BY lower(p.name), p.id`, spaceID, userID)
	if err != nil {
		return nil, fmt.Errorf("reading the owned projects: %w", err)
	}
	defer rows.Close()
	out := []ProjectRef{}
	for rows.Next() {
		var r ProjectRef
		var id uuid.UUID
		if err := rows.Scan(&id, &r.Name, &r.IDPrefix); err != nil {
			return nil, fmt.Errorf("reading the owned projects: %w", err)
		}
		r.ID = id.String()
		out = append(out, r)
	}
	return out, rows.Err()
}
