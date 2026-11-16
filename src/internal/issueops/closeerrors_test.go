package issueops_test

// sk-1007 US-3, matrix row No. 49. Reference — plan K6: when reading the
// visibility of the open children fails, the refusal of a standing close
// keeps its code and message, names no child and counts all of them in
// unavailableOpenChildren (fail-closed) — never internal_error, never the
// full list.
//
// The refusal is produced by the store (issues.Store.Close on a standing
// task with three open children), the only builder of
// ErrStandingHasOpenChildren; the failing read is a closed pool. This makes
// the row integration rather than unit, the fallback its rationale names.

import (
	"context"
	"os"
	"reflect"
	"slices"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/issueops"
	"skein-api/internal/issues"
	"skein-api/internal/testsupport"
)

func TestClassifyCloseError_VisibilityReadFailsClosed(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, ownerName := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "cef")
	standing := testsupport.SeedIssue(t, pool, pid, owner, prefix+"-1", "standing", "standing", 2)
	var children []string
	for i := 2; i <= 4; i++ {
		c := testsupport.SeedIssue(t, pool, pid, owner, prefix+"-"+strconv.Itoa(i), "child", "open", 2)
		testsupport.SeedDep(t, pool, c, standing, "parent-child", owner)
		children = append(children, c)
	}
	ctx := auth.WithUserRole(auth.WithUserID(context.Background(), owner), "user")
	_, err := issues.NewStore(pool).Close(ctx, issues.CloseInput{
		ID: standing, ExpectedVersion: 1, Reason: "retire", Actor: issues.HumanActor(owner, ownerName),
	})
	if err == nil {
		t.Fatalf("closing a standing task with open children succeeded")
	}

	// Positive control: the reader sees every child on a working pool.
	live := issueops.ClassifyCloseError(ctx, pool, pid, err, "skein_close", owner.String(), standing, "")
	got, _ := live.Details["childIds"].([]string)
	slices.Sort(got)
	if live.Code != "standing_has_open_children" || !reflect.DeepEqual(got, children) || live.Details["unavailableOpenChildren"] != 0 {
		t.Fatalf("with a working pool: %+v, want standing_has_open_children with childIds %v and 0 unavailable", live, children)
	}

	closed, cerr := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if cerr != nil {
		t.Fatalf("second pool: %v", cerr)
	}
	closed.Close()
	c := issueops.ClassifyCloseError(ctx, closed, pid, err, "skein_close", owner.String(), standing, "")
	if c.Code != "standing_has_open_children" {
		t.Errorf("code = %q, want standing_has_open_children (not internal_error)", c.Code)
	}
	if c.Message != live.Message {
		t.Errorf("message %q, want the refusal's own %q", c.Message, live.Message)
	}
	if ids, ok := c.Details["childIds"].([]string); !ok || len(ids) != 0 {
		t.Errorf("childIds = %#v, want an empty list", c.Details["childIds"])
	}
	if n := c.Details["unavailableOpenChildren"]; n != 3 {
		t.Errorf("unavailableOpenChildren = %v, want 3", n)
	}
}
