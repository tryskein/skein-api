package issues

import (
	"reflect"
	"slices"
	"testing"

	"skein-api/internal/authz"
	"skein-api/internal/dependencies"
)

// sk-426: the id-order rule of sk-421.2 US-6 (idorder.Compare) is the order
// of every id list on the card, not only of the tree. Before sk-426 the
// detail's blockers, the tree node's blockers and the skein_close open-child
// warning were ordered by the database as strings, so `x.10` came before
// `x.2`.
//
// Each case seeds ten-plus siblings in scrambled order and checks the literal
// the rule gives; a second case with no numeric segments (and same-length
// numbers) checks that such ids keep the bytewise order they had.

// idOrderNumericSuffixes — numbered siblings plus non-numeric ones. By the
// rule: numbers by value, and a digit segment before a non-digit one.
var idOrderNumericSuffixes = []string{"10", "2", "11", "9", "1", "beta", "12", "3", "alpha"}

func idOrderNumericWant(prefix, sep string) []string {
	out := make([]string, 0, len(idOrderNumericSuffixes))
	for _, s := range []string{"1", "2", "3", "9", "10", "11", "12", "alpha", "beta"} {
		out = append(out, prefix+sep+s)
	}
	return out
}

// idOrderPlainSuffixes — no segment whose order depends on the rule: letters
// and numbers of equal length. The expected order is plain bytewise.
var idOrderPlainSuffixes = []string{"gamma", "7", "alpha", "3", "beta"}

func idOrderPlainWant(prefix, sep string) []string {
	out := make([]string, 0, len(idOrderPlainSuffixes))
	for _, s := range idOrderPlainSuffixes {
		out = append(out, prefix+sep+s)
	}
	slices.Sort(out)
	return out
}

func blockerIDs(refs []BlockerRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.ID)
	}
	return out
}

// TestBlockers_IDOrder — the detail's blockers[] and the tree node's
// blockers (sk-421.2.8: «the detail's order») follow the rule.
func TestBlockers_IDOrder(t *testing.T) {
	cases := []struct {
		name     string
		suffixes []string
		want     func(prefix, sep string) []string
	}{
		{"numeric segments by value", idOrderNumericSuffixes, idOrderNumericWant},
		{"no numeric segments — bytewise as before", idOrderPlainSuffixes, idOrderPlainWant},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestPool(t)
			owner := seedUser(t, pool)
			pid, prefix := seedProject(t, pool, owner)
			subject := prefix + "-0"
			seedIssue(t, pool, pid, owner, subject, "subject")
			for _, s := range tc.suffixes {
				id := prefix + "-" + s
				seedIssue(t, pool, pid, owner, id, "blocker "+id)
				seedDep(t, pool, subject, id, dependencies.DepBlocks, owner)
			}
			want := tc.want(prefix, "-")
			store := NewStore(pool)

			dto, err := store.GetIssue(storeFixtureCtx(), subject)
			if err != nil {
				t.Fatalf("GetIssue: %v", err)
			}
			if got := blockerIDs(dto.Blockers); !reflect.DeepEqual(got, want) {
				t.Errorf("detail blockers = %v\nwant %v", got, want)
			}

			res, err := store.IssueTree(storeFixtureCtx(), IssueTreeArgs{
				Scope:           authz.Unrestricted(),
				Caller:          owner,
				RootID:          &subject,
				Types:           []string{dependencies.DepParentChild},
				IncludeBlockers: true,
			})
			if err != nil {
				t.Fatalf("IssueTree: %v", err)
			}
			if len(res.Items) == 0 || res.Items[0].Item.ID != subject {
				t.Fatalf("first tree row is not the subject: %+v", res.Items)
			}
			got := make([]string, 0, len(res.Items[0].Blockers))
			for _, b := range res.Items[0].Blockers {
				got = append(got, b.ID)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("tree node blockers = %v\nwant %v", got, want)
			}
		})
	}
}

// TestEpicOpenChildren_IDOrder — the skein_close warning list
// (EpicOpenChildrenFor → openChildList) follows the rule.
func TestEpicOpenChildren_IDOrder(t *testing.T) {
	cases := []struct {
		name     string
		suffixes []string
		want     func(prefix, sep string) []string
	}{
		{"numeric segments by value", idOrderNumericSuffixes, idOrderNumericWant},
		{"no numeric segments — bytewise as before", idOrderPlainSuffixes, idOrderPlainWant},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestPool(t)
			owner := seedUser(t, pool)
			pid, prefix := seedProject(t, pool, owner)
			epicID := prefix + "-1"
			seedTypedIssue(t, pool, pid, owner, epicID, "the epic", TypeEpic, nil)
			for _, s := range tc.suffixes {
				seedTypedIssue(t, pool, pid, owner, epicID+"."+s, "child "+s, TypeTask, &epicID)
			}
			got, err := NewStore(pool).EpicOpenChildrenFor(storeFixtureCtx(), epicID)
			if err != nil {
				t.Fatalf("EpicOpenChildrenFor: %v", err)
			}
			if want := tc.want(epicID, "."); !reflect.DeepEqual(blockerIDs(got), want) {
				t.Errorf("open children = %v\nwant %v", blockerIDs(got), want)
			}
		})
	}
}
