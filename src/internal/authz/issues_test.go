package authz_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/authz"
)

// failingQuerier fails the test on any read: the boundary a unit test does
// not have.
type failingQuerier struct{ t *testing.T }

func (q failingQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	q.t.Fatalf("the database was read for an empty input")
	return nil, nil
}

// Matrix row 4 (sk-1007 T001, SC-006): an empty input does not read the
// database, even for a caller whose visible set would otherwise be read.
func TestVisibleIssueIDs_EmptyInputReadsNothing(t *testing.T) {
	for _, c := range []struct {
		name string
		ids  []string
	}{{"nil", nil}, {"empty", []string{}}} {
		t.Run(c.name, func(t *testing.T) {
			got, err := authz.VisibleIssueIDs(context.Background(), failingQuerier{t}, authz.ForUser(uuid.New()), c.ids)
			if err != nil || len(got) != 0 {
				t.Errorf("VisibleIssueIDs = (%v, %v), want an empty map and no error", got, err)
			}
		})
	}
}
