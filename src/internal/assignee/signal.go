package assignee

// signal.go — sk-622.21 T017 (US-11, "User creation, email change on
// SSO login"): the users whose assignee resolution may change when a
// directory row appears or changes a handle.

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/authz/authzsql"
)

// Querier is the part of pgx.Tx / *pgxpool.Pool HandleAddressees needs.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// HandleAddressees returns the resolved assignees and the authors of every
// issue whose assignee TEXT names one of handles (username, e-mail or id,
// case-insensitive — the resolution rule of LeftJoin). Read before and after a
// write that creates a user or changes their e-mail / username, the union is
// "the users whose assignee resolution on their issues changed": the
// assignee before, the assignee after and the author of each such issue.
// The assignee counts only while the issue is delivered to them — they may
// work in its project now (WorkerEligibleExpr, INV-30):
// an account that merely carries the handle of an issue it cannot see is no
// addressee. One statement on idx_issues_assignee_lower.
func HandleAddressees(ctx context.Context, q Querier, handles ...string) ([]uuid.UUID, error) {
	lowered := make([]string, 0, len(handles))
	for _, h := range handles {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			lowered = append(lowered, h)
		}
	}
	if len(lowered) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
SELECT DISTINCT v.user_id
  FROM issues i
 CROSS JOIN LATERAL (VALUES (CASE WHEN `+authzsql.WorkerEligibleExpr("i.assignee_user_id", "i.project_id")+`
                                  THEN i.assignee_user_id END),
                            (i.created_by)) AS v(user_id)
 WHERE i.assignee IS NOT NULL
   AND lower(i.assignee) = ANY($1::text[])
   AND v.user_id IS NOT NULL`, lowered)
	if err != nil {
		return nil, fmt.Errorf("reading assignee-resolution addressees: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("reading assignee-resolution addressees: %w", err)
	}
	return ids, nil
}
