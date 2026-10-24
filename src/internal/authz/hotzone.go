package authz

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// HotZoneReach is the reach of one caller's HOT ZONE (US-13, sk-674.18
// T030): the projects whose numbers and rows it holds, split by how much of
// them the caller may get.
//
//   - Visible — the projects the caller sees (the access relation): shown in
//     full. For an instance admin these are the non-personal projects and
//     his own "Personal" — the same set as his column.
//   - Reach — Visible plus the projects of the caller's own non-personal
//     spaces he does not see (skein_contribution_spaces): their CONTRIBUTION
//     enters every aggregate surface and their rows appear only anonymized
//     (INV-28 (1), (2)). An external user is in no space and contributes
//     nothing (INV-28).
//
// Nothing else: a project of another space or another person's "Personal" is
// in neither set (INV-23, SC-005). Unrestricted is declared internal work —
// no filter at all (both sets nil).
type HotZoneReach struct {
	Unrestricted bool
	Visible      map[uuid.UUID]struct{}
	Reach        map[uuid.UUID]struct{}
}

// hotZoneReachSQL reads the reach in one statement: every project the
// caller sees or that lies in one of his own non-personal spaces, flagged by
// visibility; $2 (a space short id, the empty string for all) narrows it to one space —
// the page filter `space=`, which never widens the reach.
const hotZoneReachSQL = `
SELECT p.id, (va.project_id IS NOT NULL) AS visible
  FROM projects p
  JOIN spaces sp ON sp.id = p.space_id
  LEFT JOIN skein_project_access_set($1::uuid) va ON va.project_id = p.id
 WHERE (va.project_id IS NOT NULL
        OR p.space_id IN (SELECT cs.space_id FROM skein_contribution_spaces($1::uuid) cs))
   AND ($2::text = '' OR sp.short_id = $2::text)`

// ReachFor resolves the HOT ZONE reach of scope, narrowed to the space with
// short id spaceShort ("" — every space of the caller).
func ReachFor(ctx context.Context, q RowsQuerier, s Scope, spaceShort string) (HotZoneReach, error) {
	if s.unrestricted {
		return HotZoneReach{Unrestricted: true}, nil
	}
	out := HotZoneReach{Visible: map[uuid.UUID]struct{}{}, Reach: map[uuid.UUID]struct{}{}}
	if s.callerID == uuid.Nil {
		return out, nil
	}
	rows, err := q.Query(ctx, hotZoneReachSQL, s.callerID, spaceShort)
	if err != nil {
		return HotZoneReach{}, fmt.Errorf("loading the HOT ZONE reach: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var visible bool
		if err := rows.Scan(&id, &visible); err != nil {
			return HotZoneReach{}, fmt.Errorf("scanning the HOT ZONE reach: %w", err)
		}
		out.Reach[id] = struct{}{}
		if visible {
			out.Visible[id] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return HotZoneReach{}, fmt.Errorf("reading the HOT ZONE reach: %w", err)
	}
	return out, nil
}
