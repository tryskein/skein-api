package handler

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/dependencies"
	"skein-api/internal/projects"
)

// ───────────── sk-421.2.8 T002: shape of a tree node blocker entry ─────────────
//
// sk-421.2.8 gave the tree blocker entry (issues.TreeBlockerRef, R-1) three
// shapes — a visible one and two hidden ones ({id, redacted:true} and {redacted:true}).
// sk-423 removed the hidden shapes: an entry that exists only while the inaccessible
// issue is not closed was reporting exactly that status. A link to an issue outside the horizon
// is now counted in the row's `unavailableBlockingLinks`. The entry has ONE shape.
//
// The list is written by hand and not derived from the struct: a list derived
// from what it guards agrees with any change to it. The live-wire check is T016.

// treeBlockerVisibleKeys — the single shape of a node blocker entry.
var treeBlockerVisibleKeys = map[string]string{
	"id": "string", "title": "string", "status": "string", "projectId": "string",
}

// ───────── sk-421.2.8 T016: shape of blockers entries on the wire (row 10) ─────────
//
// Descent through a live tree response with includeBlockers=true: every
// entry that arrives must match the handwritten shape, with values of its
// types (sk-423: there is one shape).

// assertTreeBlockerEntryForm checks one blockers[] entry against the shape.
func assertTreeBlockerEntryForm(t *testing.T, where string, entry map[string]any) bool {
	t.Helper()
	same := len(entry) == len(treeBlockerVisibleKeys)
	for key := range entry {
		if _, ok := treeBlockerVisibleKeys[key]; !ok {
			same = false
		}
	}
	if !same {
		t.Errorf("%s: keys %s do not match the entry shape %v. A key added to a blockers[] "+
			"entry CHANGES the tree response: list it in the same commit or keep it off the wire "+
			"(sk-423 removed the hidden shapes deliberately).", where, joinedKeys(entry),
			sortedCardKeys(toAnyMap(treeBlockerVisibleKeys)))
		return false
	}
	for key, value := range entry {
		assertJSONKind(t, where, key, value, treeBlockerVisibleKeys[key])
	}
	return true
}

// TestTreeREST_BlockerEntriesOnTheWire — row 10 (SC-009, INV-3, US-4 "wherever
// the field exists"). A live tree response with includeBlockers=true on both routes under
// a member of P and Q, an internal non-member of Q and an external member of P.
//
// Fixture in P and Q, traversal parent-child + blocks:
//   - S is blocked by the visible Bp and by the issue Xq from Q — an entry of the visible shape for
//     all readers, and for readers outside Q also unavailableBlockingLinks=1
//     (sk-423: Xq is not an entry but a count);
//   - H in Q — a child of S and itself BLOCKED: the stub of a blocked issue;
//   - R — a child of S, BLOCKED; W — a child of S, blocked by R, and the traversal along
//     its blocks edge emits R once more — a blocked reference row;
//   - U — a child of S without blockers: a node with a false flag.
//
// Negative cases are checked on blocked issues: otherwise
// the absence of the field would be trivially proven for any assembly error.
// The check fails if the run did not meet every shape and every
// negative case.
func TestTreeREST_BlockerEntriesOnTheWire(t *testing.T) {
	pool := newListTestPool(t)
	pOwner, _ := seedListUser(t, pool, "user")
	qOwner, _ := seedListUser(t, pool, "user")
	pID, p := seedListProject(t, pool, pOwner, "trp")
	qID, q := seedListProject(t, pool, qOwner, "trq")

	memberBoth, _ := seedListUser(t, pool, "user")
	addTreeMember(t, pool, pID, memberBoth, pOwner)
	addTreeMember(t, pool, qID, memberBoth, qOwner)
	nonMemberQ := pOwner
	external, _ := seedListUser(t, pool, "user")
	addTreeMember(t, pool, pID, external, pOwner)

	s, bp, r, w, u := p+"-1", p+"-2", p+"-3", p+"-4", p+"-5"
	xq, h := q+"-1", q+"-2"
	for _, id := range []string{s, bp, r, w, u} {
		seedListIssue(t, pool, pID, pOwner, id, "human", "", nil)
	}
	for _, id := range []string{xq, h} {
		seedListIssue(t, pool, qID, qOwner, id, "human", "", nil)
	}
	treeSeedDep(t, pool, s, bp, dependencies.DepBlocks, pOwner)
	treeSeedDep(t, pool, s, xq, dependencies.DepBlocks, pOwner)
	treeSeedDep(t, pool, h, s, dependencies.DepParentChild, pOwner)
	treeSeedDep(t, pool, h, bp, dependencies.DepBlocks, pOwner)
	treeSeedDep(t, pool, r, s, dependencies.DepParentChild, pOwner)
	treeSeedDep(t, pool, r, bp, dependencies.DepBlocks, pOwner)
	treeSeedDep(t, pool, w, s, dependencies.DepParentChild, pOwner)
	treeSeedDep(t, pool, w, r, dependencies.DepBlocks, pOwner)
	treeSeedDep(t, pool, u, s, dependencies.DepParentChild, pOwner)

	env := listTestEnv{pool: pool, handler: NewIssuesHandler(pool,
		projects.NewStore(pool), dependencies.NewStore(pool)),
		owner: pOwner, projectID: pID, prefix: p}

	readers := []struct {
		name       string
		user       uuid.UUID
		isExternal bool
	}{
		{"member of P and Q", memberBoth, false},
		{"internal non-member of Q", nonMemberQ, false},
		{"external member of P", external, true},
	}
	query := "rootId=" + s + "&types=parent-child&types=blocks&includeBlockers=true"

	var entriesSeen, hiddenLinksSeen int
	var blockedStubs, blockedRefs, unblockedNodes, hBlockedWhenVisible int

	for _, rd := range readers {
		for _, route := range []treeRoute{treeRouteAll, treeRouteProject} {
			t.Run(rd.name+"/"+string(route), func(t *testing.T) {
				status, body := callTreeRaw(t, env, route, rd.user, "user", rd.isExternal, query)
				if status != http.StatusOK {
					t.Fatalf("code %d, body %s", status, body)
				}
				for i, row := range decodeTreeItems(t, body) {
					item, _ := row["item"].(map[string]any)
					id, _ := item["id"].(string)
					where := fmt.Sprintf("tree %s/%s items[%d] (%s)", rd.name, route, i, id)
					isStub := item["title"] == "<hidden>"
					isRef := row["isReference"] == true
					blocked := item["isBlocked"] == true
					bl, present := row["blockers"]
					links, linksPresent := row["unavailableBlockingLinks"]
					if linksPresent && (isStub || isRef) {
						t.Errorf("%s: a stub or reference row carries unavailableBlockingLinks = %v (INV-3)", where, links)
					}
					if id == s && !isStub && !isRef {
						wantLinks := rd.user != memberBoth
						if linksPresent != wantLinks || (wantLinks && links != float64(1)) {
							t.Errorf("%s: unavailableBlockingLinks = %v (present: %v); want 1 for a reader outside Q and "+
								"no key for a member of Q", where, links, linksPresent)
						}
						if wantLinks && links == float64(1) {
							hiddenLinksSeen++
						}
					}

					switch {
					case isStub:
						if present {
							t.Errorf("%s: a stub carries blockers = %v (INV-3)", where, bl)
						}
						// The only inaccessible node of this tree is H; its stub
						// carries a synthetic id for every reader (sk-674.18
						// T009), never H's real one.
						if id == h {
							t.Errorf("%s: a stub carries the real id of an inaccessible issue", where)
						}
						if strings.HasPrefix(id, "hidden-") {
							blockedStubs++
						}
					case isRef:
						if present {
							t.Errorf("%s: a reference row carries blockers = %v — the field only on the canonical "+
								"occurrence (INV-3)", where, bl)
						}
						if id == r && blocked {
							blockedRefs++
						}
					case !blocked:
						if present {
							t.Errorf("%s: a node with a false flag carries blockers = %v", where, bl)
						}
						if id == u {
							unblockedNodes++
						}
					default:
						if id == h {
							hBlockedWhenVisible++
						}
						arr, ok := bl.([]any)
						if !present || !ok || len(arr) == 0 {
							t.Errorf("%s: a blocked visible node without non-empty blockers (%v)", where, bl)
							continue
						}
						for j, raw := range arr {
							entry, ok := raw.(map[string]any)
							if !ok {
								t.Errorf("%s: blockers[%d] is not an object: %T", where, j, raw)
								continue
							}
							if assertTreeBlockerEntryForm(t, fmt.Sprintf("%s blockers[%d]", where, j), entry) {
								entriesSeen++
							}
						}
					}
				}
			})
		}
	}

	for name, n := range map[string]int{
		"blocker entry":                        entriesSeen,
		"link to an issue outside the horizon": hiddenLinksSeen,
		"stub of a blocked issue":              blockedStubs,
		"blocked reference row":                blockedRefs,
		"node with a false flag":               unblockedNodes,
		"issue H is blocked (precondition)":    hBlockedWhenVisible,
	} {
		if n == 0 {
			t.Errorf("the run did not meet the case \"%s\" — the negative check for it was not executed", name)
		}
	}
}
