package dependencies

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"skein-api/internal/authz"
)

// TestList_IDOrder — sk-426: the dependency section (List, behind REST
// GET /issues/{id}/dependencies and skein_show depsTypes) orders each
// direction by type, then by the other end's id under the id-order rule of
// sk-421.2 US-6: `x.2` before `x.10`. Ids with no numeric segment whose order
// depends on the rule keep the bytewise order they had.
func TestList_IDOrder(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	subject := prefix + "-0"
	seedOpenIssue(t, pool, pid, owner, subject, "subject")

	seedEdge := func(fromID, toID, depType string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, $3, '{}'::jsonb, 1, $4, now())`, fromID, toID, depType, owner); err != nil {
			t.Fatalf("seed dep %s -%s-> %s: %v", fromID, depType, toID, err)
		}
	}
	other := func(n string) string {
		id := prefix + "-" + n
		seedOpenIssue(t, pool, pid, owner, id, "other "+n)
		return id
	}

	// Outgoing: related to scrambled numbered ids and two words, plus one
	// blocks edge — type still decides first.
	for _, n := range []string{"10", "2", "11", "9", "1", "beta", "12", "3", "alpha"} {
		seedEdge(subject, other("o"+"."+n), DepRelated)
	}
	seedEdge(subject, other("o.50"), DepBlocks)
	// Incoming: blocks from numbered ids (the rule), then related from ids
	// with no numeric segment whose order depends on the rule (bytewise).
	for _, n := range []string{"10", "2"} {
		seedEdge(other("i.b."+n), subject, DepBlocks)
	}
	plain := []string{"gamma", "7", "alpha", "3", "beta"}
	for _, n := range plain {
		seedEdge(other("i."+n), subject, DepRelated)
	}

	list, err := NewStore(pool).List(ctx, subject, authz.Unrestricted())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	var gotOut []string
	for _, e := range list.Outgoing {
		gotOut = append(gotOut, e.Type+" "+e.ToID)
	}
	wantOut := []string{DepBlocks + " " + prefix + "-o.50"}
	for _, n := range []string{"1", "2", "3", "9", "10", "11", "12", "alpha", "beta"} {
		wantOut = append(wantOut, DepRelated+" "+prefix+"-o."+n)
	}
	if !reflect.DeepEqual(gotOut, wantOut) {
		t.Errorf("outgoing = %v\nwant %v", gotOut, wantOut)
	}

	var gotIn []string
	for _, e := range list.Incoming {
		gotIn = append(gotIn, e.FromID)
	}
	plainWant := make([]string, 0, len(plain))
	for _, n := range plain {
		plainWant = append(plainWant, prefix+"-i."+n)
	}
	slices.Sort(plainWant)
	wantIn := append([]string{prefix + "-i.b.2", prefix + "-i.b.10"}, plainWant...)
	if !reflect.DeepEqual(gotIn, wantIn) {
		t.Errorf("incoming = %v\nwant %v", gotIn, wantIn)
	}
}
