package issues

// neutral_row.go — sk-622.21, T027 (US-15, K16, K17, INV-14, INV-17): the
// reader-independent list rows that phase-2 issue events carry in the
// envelope field `row`.
//
// The outbox dispatcher collects the ids of every issue event in a tick and
// asks for all their rows in ONE statement (NeutralRowsForIDs) — not one per
// event and not inside the writing transaction: the row is the issue's
// current state when the tick reads it, which is never older than the
// commit that produced the event (the event row is visible to the tick only
// after that commit). The outbox package does not import this one; it sees
// the rows through NeutralRowSource, an interface spelled in stdlib and pgx
// types only.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/assignee"
)

// NeutralRowQuerier is the one method NeutralRowsForIDs needs: a pgx.Tx, a
// *pgxpool.Pool and a *pgx.Conn all satisfy it. The dispatcher passes its
// tick transaction, so the rows are read on the connection that already
// holds the tick.
type NeutralRowQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// neutralRowsSQL builds every row of the batch in one statement. The
// columns mirror the REST list row (list_all.go#scanIssueSummaryRow) minus
// the reader-dependent ones — `iAssigned`, `isBlocked`, `epicProgress` are
// not computed here at all (INV-14), so no reader's horizon enters the
// query.
//
//   - assignee_user_id comes from the same users join as the REST row
//     (assignee.LeftJoin), so a client computing iAssigned from the event row
//     gets the answer REST would give (K17).
//   - The parent is the canonical parentIdCurrent (first parent-child edge
//     by created_at, store.go#parentIDCurrentExpr) and is returned ONLY when
//     that parent lives in the issue's own project; otherwise NULL, and the
//     key is absent on the wire (INV-17: the event goes to the issue's
//     project, whose readers may not see another project's issue).
//   - Labels and component ids are aggregated in the same statement, in the
//     REST order (label / component id ascending), and are '{}' when none.
//   - row_seq is read in the same statement as the columns it orders, so the
//     row's columns are never newer than its RowSeq claims, and its labels /
//     components come from the same snapshot.
var neutralRowsSQL = `
SELECT i.id, i.project_id,
       CASE WHEN par.project_id = i.project_id THEN par.id END AS parent_id,
       i.version, i.title, i.type, i.priority, i.status, i.pinned,
       i.assignee, ua.id, i.created_by, uc.username, ` + issueSummaryPersonCols + `,
       COALESCE((SELECT array_agg(ic.component_id ORDER BY ic.component_id)
                   FROM issue_components ic WHERE ic.issue_id = i.id), '{}') AS component_ids,
       COALESCE((SELECT array_agg(il.label ORDER BY il.label)
                   FROM issue_labels il WHERE il.issue_id = i.id), '{}') AS labels,
       i.accepted_at, i.close_outcome, i.updated_at, i.created_at, i.row_seq
  FROM issues i
  LEFT JOIN LATERAL (
        SELECT p.id, p.project_id
          FROM dependencies d
          JOIN issues p ON p.id = d.to_id
         WHERE d.from_id = i.id AND d.type = 'parent-child'
         ORDER BY d.created_at
         LIMIT 1
  ) par ON TRUE
  LEFT JOIN users uc ON uc.id = i.created_by
  ` + assignee.LeftJoin("ua", "i") + `
 WHERE i.id = ANY($1)`

// NeutralRowsForIDs returns the neutral row of every issue in ids that
// exists, keyed by issue id, in one statement. An id with no issue row (a
// deleted issue, a malformed entity id) is simply absent from the map — the
// event then goes out without a row, as on phase 1. Duplicate ids are
// harmless. An empty ids slice issues no statement.
func NeutralRowsForIDs(ctx context.Context, q NeutralRowQuerier, ids []string) (map[string]NeutralRow, error) {
	out := make(map[string]NeutralRow, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, neutralRowsSQL, ids)
	if err != nil {
		return nil, fmt.Errorf("loading neutral rows: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			r                 NeutralRow
			parentID          *string
			assigneeText      *string
			assigneeUserID    *uuid.UUID
			createdByUsername *string
			acceptedAt        *time.Time
			closeOutcome      *string
		)
		if err := rows.Scan(
			&r.ID, &r.ProjectID, &parentID,
			&r.Version, &r.Title, &r.Type, &r.Priority, &r.Status, &r.Pinned,
			&assigneeText, &assigneeUserID, &r.CreatedBy, &createdByUsername,
			&r.AssigneeStatus, &r.CreatedByStatus, &r.SpaceID,
			&r.ComponentIDs, &r.Labels,
			&acceptedAt, &closeOutcome, &r.UpdatedAt, &r.CreatedAt, &r.RowSeq,
		); err != nil {
			return nil, fmt.Errorf("scanning neutral row: %w", err)
		}
		r.ParentID = parentID
		r.Assignee = assigneeText
		r.AssigneeUserID = assigneeUserID
		r.CreatedByUsername = createdByUsername
		r.AcceptedAt = acceptedAt
		r.CloseOutcome = closeOutcome
		out[r.ID] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading neutral rows: %w", err)
	}
	return out, nil
}

// NeutralRowSource adapts NeutralRowsForIDs to the outbox dispatcher's
// row-source interface (outbox.NeutralRowSource). Stateless: the dispatcher
// supplies the transaction. Wired in cmd/api/main.go.
type NeutralRowSource struct{}

// NeutralRowsJSON returns the JSON of every requested issue's neutral row
// together with the issue's project, keyed by issue id. The project lets
// the dispatcher attach a row only to an event of that same project
// (INV-17). Times are rendered in UTC, as every other time on the envelope.
func (NeutralRowSource) NeutralRowsJSON(
	ctx context.Context,
	tx pgx.Tx,
	ids []string,
) (map[string]json.RawMessage, map[string]uuid.UUID, error) {
	rows, err := NeutralRowsForIDs(ctx, tx, ids)
	if err != nil {
		return nil, nil, err
	}
	bodies := make(map[string]json.RawMessage, len(rows))
	projects := make(map[string]uuid.UUID, len(rows))
	for id, r := range rows {
		r.UpdatedAt = r.UpdatedAt.UTC()
		r.CreatedAt = r.CreatedAt.UTC()
		if r.AcceptedAt != nil {
			t := r.AcceptedAt.UTC()
			r.AcceptedAt = &t
		}
		b, err := json.Marshal(r)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal neutral row %s: %w", id, err)
		}
		bodies[id] = b
		projects[id] = r.ProjectID
	}
	return bodies, projects, nil
}
