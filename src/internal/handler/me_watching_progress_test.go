package handler

// me_watching_progress_test.go — sk-489, T005, matrix row 17,
// the last cell of its table: HTTP `/me/watching`.
//
// Why this surface has a cell of its own, although it returns rows through the same
// helper as the tree (`issues/tree.go#summariesForIDs`). In the tree
// the row progress is OVERWRITTEN by a second reader (`branchProgressFor`),
// and the numbers of the projection itself cannot be reached there: the tree answers correctly
// even if the projection is broken. `/me/watching` does no overwriting — it
// prints exactly what the scanner assembled. It is the only surface on
// which the numbers of the shared projection are visible on the wire as they are.
//
// The spec names it directly: "observed (`/me/watching` — a field on the wire,
// no consumer in the interface)". No consumer means breaking it
// silently is especially easy, and so the test must exist, not "it is obvious anyway".

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
)

// closeMeIssueWithOutcome closes a seeded row RAW, with an explicit outcome.
// The outcome writers are the subject of their own tests (T003), not a dependency of this one.
func closeMeIssueWithOutcome(t *testing.T, pool *pgxpool.Pool, id, outcome string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
UPDATE issues SET status = 'closed', closed_at = now(), close_outcome = $1 WHERE id = $2`,
		outcome, id); err != nil {
		t.Fatalf("close %s with outcome %s: %v", id, outcome, err)
	}
}

// watchingProgressOf reads item.epicProgress of the observed card,
// insisting that the key EXISTS: its absence is exactly "the old api" for
// the client (US-10), and it has no right to be read as zero.
func watchingProgressOf(t *testing.T, body map[string]any, issueID string) map[string]float64 {
	t.Helper()
	row := watchingCard(t, body, issueID)
	item, ok := row["item"].(map[string]any)
	if !ok {
		t.Fatalf("card %s: item is not an object (%#v)", issueID, row["item"])
	}
	raw, ok := item["epicProgress"].(map[string]any)
	if !ok {
		t.Fatalf("card %s came without an epicProgress object; row keys: %#v", issueID, item)
	}
	out := make(map[string]float64, len(raw))
	for _, k := range []string{"closed", "total", "unavailable", "done", "noWork", "counted"} {
		v, present := raw[k]
		if !present {
			t.Fatalf("card %s: epicProgress has no key %q. The key must come even at zero: "+
				"a client that did not see it must assume the OLD api is answering (US-10), "+
				"and show the previous picture — with omitempty the two states would merge. "+
				"Got: %#v", issueID, k, raw)
		}
		f, ok := v.(float64)
		if !ok {
			t.Fatalf("card %s: epicProgress[%q] is not a number (%#v)", issueID, k, v)
		}
		out[k] = f
	}
	return out
}

// watchingExtraDoneKids — extra "done" beyond one child for each
// outcome value: with them all six numbers of the row are pairwise different, and
// swapping the legs cannot go unnoticed.
const watchingExtraDoneKids = 3

// TestWatchingProgress_CarriesTheOutcomeNumbers — row 17, the `/me/watching`
// cell: one closed child for each outcome value,
// three extra "done" and one INVISIBLE child closed without work.
//
// The fixture is like this, and not "3 done + 1 we will not count", for two measured
// reasons (review of wave A/3):
//
//   - `/me/watching` is the ONLY surface on which the numbers of the shared
//     projection are visible as they are (the tree overwrites them with a batch). So
//     the binding of the legs to the reader's horizon in `issues/tree.go#summariesForIDs`
//     is observed only by this test — and it has nothing to observe while the fixture
//     has no child the reader cannot see. The mutant "this place binds an
//     UNLIMITED horizon" passed the full T005 gate green.
//   - The rule "any outcome except 'done' drops out" is written in SQL as a black
//     list, and with only `wont_do` in the fixture an incomplete white list
//     was indistinguishable from it. The expectation is computed from
//     `issues.ValidCloseOutcomes()` + `issues.IsNoWorkOutcome` — so a
//     fifth outcome value, added to the set and forgotten in one of the
//     halves of the predicate, reddens this cell.
func TestWatchingProgress_CarriesTheOutcomeNumbers(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := seedMeUser(t, pool, "user")
	outsider, _ := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, user)
	hiddenPID, hiddenPrefix := seedMeProject(t, pool, outsider)
	h := newMeWatchingHandler(pool)

	epic := prefix + "-1"
	seedMeIssue(t, pool, pid, user, epic, "", nil)
	if _, err := pool.Exec(t.Context(),
		`UPDATE issues SET type = 'epic' WHERE id = $1`, epic); err != nil {
		t.Fatalf("make %s an epic: %v", epic, err)
	}

	outcomes := issues.ValidCloseOutcomes()
	n := 0
	nextKid := func() string {
		n++
		return fmt.Sprintf("%s.%d", epic, n)
	}
	// One child for each outcome value.
	var noWorkKid string
	for _, outcome := range outcomes {
		id := nextKid()
		seedMeIssue(t, pool, pid, user, id, "", nil)
		treeSeedDep(t, pool, id, epic, dependencies.DepParentChild, user)
		closeMeIssueWithOutcome(t, pool, id, outcome)
		if outcome == issues.CloseOutcomeWontDo {
			noWorkKid = id
		}
	}
	// Extra "done".
	for range watchingExtraDoneKids {
		id := nextKid()
		seedMeIssue(t, pool, pid, user, id, "", nil)
		treeSeedDep(t, pool, id, epic, dependencies.DepParentChild, user)
		closeMeIssueWithOutcome(t, pool, id, issues.CloseOutcomeDone)
	}
	// A child in a project the reader is not a member of: parent-child
	// crosses projects (sk-35). It stays in `total` and in `counted`,
	// goes to `unavailable` and does NOT go to any outcome leg — with
	// any outcome of its own (INV-4, R-6).
	hiddenKid := hiddenPrefix + "-90"
	seedMeIssue(t, pool, hiddenPID, outsider, hiddenKid, "", nil)
	treeSeedDep(t, pool, hiddenKid, epic, dependencies.DepParentChild, outsider)
	closeMeIssueWithOutcome(t, pool, hiddenKid, issues.CloseOutcomeWontDo)

	// Both the epic and one child closed without work are watched — so that the outcome is
	// visible on the card, not only a number on the parent.
	seedWatcher(t, pool, epic, user, []string{"manual"})
	seedWatcher(t, pool, noWorkKid, user, []string{"manual"})

	res, body := callWatching(t, h, user)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /me/watching: code %d, body %v", res.StatusCode, body)
	}

	noWork := 0
	for _, o := range outcomes {
		if issues.IsNoWorkOutcome(o) {
			noWork++
		}
	}
	closed := len(outcomes) + watchingExtraDoneKids
	total := closed + 1 // + the invisible child
	want := map[string]float64{
		"closed":      float64(closed),
		"total":       float64(total),
		"unavailable": 1,
		"done":        float64(closed - noWork),
		"noWork":      float64(noWork),
		"counted":     float64(total - noWork),
	}

	got := watchingProgressOf(t, body, epic)
	for k, w := range want {
		if got[k] != w {
			t.Errorf("/me/watching, epicProgress[%q] = %v, want %v (all: %v). The progress of this "+
				"surface is assembled by the SHARED projection, and there is nothing to overwrite it here. "+
				"A small `noWork` — the \"no work\" leg did not reach the scanner or does not know all "+
				"outcome values (INV-5); `unavailable:0` — the scanner bound the legs to an "+
				"UNLIMITED horizon and counted someone else's child as its own (INV-4)",
				k, got[k], w, got)
		}
	}

	kidItem := watchingCard(t, body, noWorkKid)["item"].(map[string]any)
	if kidItem["closeOutcome"] != issues.CloseOutcomeWontDo {
		t.Errorf("card %s: closeOutcome = %#v, want %q — the outcome on the row travels with the same "+
			"scanner as the numbers", noWorkKid, kidItem["closeOutcome"], issues.CloseOutcomeWontDo)
	}
}
