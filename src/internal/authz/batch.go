package authz

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RowsQuerier is the multi-row subset of *pgxpool.Pool / pgx.Tx.
type RowsQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// BatchEntry is one user of a hub batch check.
type BatchEntry struct {
	UserID uuid.UUID
	// ProjectIDs are the projects to check; duplicates are answered once.
	ProjectIDs []uuid.UUID
	// AllVisible asks for the user's whole visible set as well.
	AllVisible bool
}

// BatchResult answers one BatchEntry.
type BatchResult struct {
	// Active — the user exists and is not deactivated. An inactive user is
	// allowed nothing and sees nothing.
	Active bool
	// InternalAdmin — users.role = 'admin' AND NOT users.is_external, read
	// now. A statement about the role, not about reach: reach is Allowed
	// and Visible.
	InternalAdmin bool
	// External mirrors users.is_external.
	External bool
	// Allowed are the entry's distinct requested projects the user sees, in
	// request order.
	Allowed []uuid.UUID
	// Visible is the user's whole visible set (ordered by id) when the entry
	// asked AllVisible, an empty slice for an inactive user; nil otherwise.
	Visible []uuid.UUID
}

// batchSQL answers a whole batch in ONE statement, whatever the number of
// users. Inputs are parallel arrays:
//
//	$1 uuid[]   — user id of entry k (k = array position, 1-based)
//	$2 bool[]   — AllVisible of entry k
//	$3 bigint[] — entry position of each requested (entry, project) pair
//	$4 uuid[]   — project id of that pair; distinct per entry, request order
//
// Reach is the relation, for each user, gated by activity: role, external
// flag and deactivation are read from users now, never from a session.
const batchSQL = `
WITH q AS (
    SELECT t.user_id, t.all_visible, t.ord
      FROM unnest($1::uuid[], $2::boolean[]) WITH ORDINALITY AS t(user_id, all_visible, ord)
),
u AS (
    SELECT q.ord, q.user_id, q.all_visible,
           (usr.id IS NOT NULL AND usr.deactivated_at IS NULL)      AS active,
           COALESCE(usr.is_external, false)                         AS external,
           (usr.id IS NOT NULL AND usr.deactivated_at IS NULL
            AND usr.role = 'admin' AND NOT usr.is_external)          AS internal_admin
      FROM q
      LEFT JOIN users usr ON usr.id = q.user_id
),
req AS (
    SELECT r.ord, r.project_id, r.pos
      FROM unnest($3::bigint[], $4::uuid[]) WITH ORDINALITY AS r(ord, project_id, pos)
)
SELECT u.ord, u.active, u.internal_admin, u.external,
       ARRAY(
           SELECT req.project_id
             FROM req
             JOIN skein_project_access_set(u.user_id) va ON va.project_id = req.project_id
            WHERE req.ord = u.ord AND u.active
            ORDER BY req.pos
       ) AS allowed,
       CASE WHEN u.all_visible THEN
           ARRAY(
               SELECT va.project_id
                 FROM skein_project_access_set(u.user_id) va
                WHERE u.active
                ORDER BY va.project_id
           )
       END AS visible
  FROM u
 ORDER BY u.ord
`

// ResolveBatch answers every entry with one database statement (none for an
// empty batch); results are in entry order.
func ResolveBatch(ctx context.Context, q RowsQuerier, entries []BatchEntry) ([]BatchResult, error) {
	out := make([]BatchResult, len(entries))
	if len(entries) == 0 {
		return out, nil
	}
	userIDs := make([]uuid.UUID, len(entries))
	allVisible := make([]bool, len(entries))
	pairOrd := []int64{}
	pairProject := []uuid.UUID{}
	for i, e := range entries {
		userIDs[i] = e.UserID
		allVisible[i] = e.AllVisible
		seen := make(map[uuid.UUID]struct{}, len(e.ProjectIDs))
		for _, pid := range e.ProjectIDs {
			if _, dup := seen[pid]; dup {
				continue
			}
			seen[pid] = struct{}{}
			pairOrd = append(pairOrd, int64(i+1))
			pairProject = append(pairProject, pid)
		}
	}

	rows, err := q.Query(ctx, batchSQL, userIDs, allVisible, pairOrd, pairProject)
	if err != nil {
		return nil, fmt.Errorf("resolving project access for %d users: %w", len(entries), err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var (
			ord     int64
			r       BatchResult
			allowed []uuid.UUID
			visible []uuid.UUID
		)
		if err := rows.Scan(&ord, &r.Active, &r.InternalAdmin, &r.External, &allowed, &visible); err != nil {
			return nil, fmt.Errorf("scanning project access row: %w", err)
		}
		i := int(ord) - 1
		if i < 0 || i >= len(entries) {
			return nil, fmt.Errorf("project access row for unexpected entry %d of %d", ord, len(entries))
		}
		r.Allowed = allowed
		if r.Allowed == nil {
			r.Allowed = []uuid.UUID{}
		}
		if entries[i].AllVisible {
			r.Visible = visible
			if r.Visible == nil {
				r.Visible = []uuid.UUID{}
			}
		}
		out[i] = r
		n++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading project access rows: %w", err)
	}
	if n != len(entries) {
		return nil, fmt.Errorf("project access answered %d of %d entries", n, len(entries))
	}
	return out, nil
}
