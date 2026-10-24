package handler

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/testsupport"
)

// sk-421.1 T001 (US-14, SC-003) through the REST surfaces that return a full
// issue card: the detail read (row 6) and the response of a change (row 7),
// under the rule sk-423 left: a blocker outside the reader's horizon is not
// an entry but a count (unavailableBlockingLinks).
// The rule lives where the card is assembled, not in these handlers;
// these tests pin that both surfaces carry it, looking at the wire keys of
// the decoded JSON body — «absent» and «empty string» are different
// contracts, and the spec rejected the empty-string form explicitly.

type bvReader struct {
	name     string
	user     uuid.UUID
	role     string
	external bool
	seesQ    bool
}

type bvFixture struct {
	pool *pgxpool.Pool
	h    *IssuesHandler
	dh   *DependenciesHandler

	pID      uuid.UUID
	subject  string
	pBlocker string
	qBlocker string

	memberP, memberBoth, externalP, admin bvReader
}

const (
	bvPBlockerTitle = "p blocker title"
	bvQBlockerTitle = "q secret blocker title"
)

// newBVFixture: subject in P blocked by one blocker from P (in_progress) and
// one from Q (deferred); readers — member of P only, member of both,
// external member of P, internal admin (member of neither).
func newBVFixture(t *testing.T) *bvFixture {
	t.Helper()
	pool := testsupport.NewTestPool(t)

	pOwner, _ := testsupport.SeedUser(t, pool, "user")
	qOwner, _ := testsupport.SeedUser(t, pool, "user")
	pID, pPrefix := testsupport.SeedProject(t, pool, pOwner, "bvp")
	qID, qPrefix := testsupport.SeedProject(t, pool, qOwner, "bvq")

	both, _ := testsupport.SeedUser(t, pool, "user")
	testsupport.AddProjectMember(t, pool, pID, both, "member")
	testsupport.AddProjectMember(t, pool, qID, both, "member")

	ext, _ := testsupport.SeedUser(t, pool, "user")
	testsupport.AddProjectMember(t, pool, pID, ext, "member")

	admin, _ := testsupport.SeedUser(t, pool, "admin")

	subject := testsupport.SeedIssue(t, pool, pID, pOwner, pPrefix+"-1", "subject", "open", 2)
	pBlocker := testsupport.SeedIssue(t, pool, pID, pOwner, pPrefix+"-2", bvPBlockerTitle, "in_progress", 2)
	qBlocker := testsupport.SeedIssue(t, pool, qID, qOwner, qPrefix+"-1", bvQBlockerTitle, "deferred", 2)
	testsupport.SeedDep(t, pool, subject, pBlocker, dependencies.DepBlocks, pOwner)
	testsupport.SeedDep(t, pool, subject, qBlocker, dependencies.DepBlocks, pOwner)

	return &bvFixture{
		pool:       pool,
		h:          NewIssuesHandler(pool, projects.NewStore(pool), dependencies.NewStore(pool)),
		dh:         NewDependenciesHandler(pool, dependencies.NewStore(pool), issues.NewStore(pool)),
		pID:        pID,
		subject:    subject,
		pBlocker:   pBlocker,
		qBlocker:   qBlocker,
		memberP:    bvReader{name: "member of P, not of Q", user: pOwner, role: "user"},
		memberBoth: bvReader{name: "member of P and Q", user: both, role: "user", seesQ: true},
		externalP:  bvReader{name: "external member of P", user: ext, role: "user", external: true},
		admin:      bvReader{name: "internal admin", user: admin, role: "admin", seesQ: true},
	}
}

func (f *bvFixture) request(t *testing.T, r bvReader, method, ifMatch string, body any) *http.Request {
	t.Helper()
	req := xpRequest(t, method, ifMatch, body, r.user, r.role,
		"projectID", f.pID.String(), "issueID", f.subject)
	if r.external {
		req = req.WithContext(auth.WithIsExternal(req.Context(), true))
	}
	return req
}

func bvKeys(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// assertBVCard checks blockers[], isBlocked and unavailableBlockingLinks of
// one decoded card for one reader (sk-423): a reader who sees Q gets both
// blockers in full and no unavailable link; a reader outside Q gets only
// the P blocker, in full, and the Q edge as one unavailable link — never as
// an entry, and never its title. Returns the card's unavailable-link count.
func (f *bvFixture) assertBVCard(t *testing.T, r bvReader, resp xpResponse) int {
	t.Helper()
	if resp.Status != http.StatusOK {
		t.Fatalf("%s: status = %d, want 200: %s", r.name, resp.Status, resp.Raw)
	}
	raw, ok := resp.Body["blockers"].([]any)
	if !ok {
		t.Fatalf("%s: blockers missing or not an array: %s", r.name, resp.Raw)
	}
	wantOrder := []string{f.pBlocker}
	wantLinks := 1
	if r.seesQ {
		wantOrder, wantLinks = []string{f.pBlocker, f.qBlocker}, 0
		sort.Strings(wantOrder)
	}
	if len(raw) != len(wantOrder) {
		t.Fatalf("%s: %d blocker entries, want %d (the blockers this reader can see): %s",
			r.name, len(raw), len(wantOrder), resp.Raw)
	}
	if resp.Body["isBlocked"] != true {
		t.Errorf("%s: isBlocked = %v, want true (the P blocker is visible to every reader)", r.name, resp.Body["isBlocked"])
	}
	links, _ := resp.Body["unavailableBlockingLinks"].(float64)
	if _, present := resp.Body["unavailableBlockingLinks"]; !present || int(links) != wantLinks {
		t.Errorf("%s: unavailableBlockingLinks = %v, want %d", r.name, resp.Body["unavailableBlockingLinks"], wantLinks)
	}

	for i, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("%s: blockers[%d] is %T, want an object", r.name, i, item)
		}
		if entry["id"] != wantOrder[i] {
			t.Errorf("%s: blockers[%d].id = %v, want %s", r.name, i, entry["id"], wantOrder[i])
		}
		if keys := bvKeys(entry); keys != "id,status,title" {
			t.Errorf("%s: entry keys = %s, want id,status,title: %v", r.name, keys, entry)
		}
		wantTitle, wantStatus := bvPBlockerTitle, "in_progress"
		if entry["id"] == f.qBlocker {
			wantTitle, wantStatus = bvQBlockerTitle, "deferred"
		}
		if entry["title"] != wantTitle || entry["status"] != wantStatus {
			t.Errorf("%s: entry = %v, want title %q status %q", r.name, entry, wantTitle, wantStatus)
		}
	}
	if !r.seesQ && (strings.Contains(resp.Raw, bvQBlockerTitle) || strings.Contains(resp.Raw, `"redacted"`)) {
		t.Errorf("%s: the hidden blocker reached the card as more than a count: %s", r.name, resp.Raw)
	}
	return int(links)
}

// Row 6 — REST GET detail, four readers. For a reader outside Q the card's
// unavailable-link count equals the number of redacted outgoing blocks
// edges the dependency list on the same card gives — the link the card
// counts is the one the dependency list already shows (sk-423 precedent).
func TestIssueCardREST_BlockerRedactionPerReader(t *testing.T) {
	f := newBVFixture(t)

	for _, r := range []bvReader{f.memberP, f.externalP, f.memberBoth, f.admin} {
		t.Run(r.name, func(t *testing.T) {
			resp := xpCallFull(t, f.h.Get, f.request(t, r, http.MethodGet, "", nil))
			links := f.assertBVCard(t, r, resp)
			if r.seesQ {
				return
			}

			deps := xpCallFull(t, f.dh.List, f.request(t, r, http.MethodGet, "", nil))
			if deps.Status != http.StatusOK {
				t.Fatalf("dependency list = %d, want 200: %s", deps.Status, deps.Raw)
			}
			outgoing, _ := deps.Body["outgoing"].([]any)
			hiddenEdges := 0
			for _, item := range outgoing {
				edge, _ := item.(map[string]any)
				if edge["redacted"] == true && (edge["type"] == "blocks" || edge["type"] == "waits-for") {
					hiddenEdges++
				}
			}
			if hiddenEdges != links {
				t.Errorf("%s: the card counts %d unavailable links, the dependency list shows %d hidden "+
					"blocking edges — both describe the same edges: %s", r.name, links, hiddenEdges, deps.Raw)
			}
		})
	}
}

// Row 7 — the response of a REST change follows the same rule: PATCH notes
// by a member of P who is not a member of Q gets the Q edge as an
// unavailable link; the same PATCH by a member of both gets it as an entry.
func TestIssueCardREST_PatchResponseRedactsBlockers(t *testing.T) {
	f := newBVFixture(t)

	version := 1
	for _, r := range []bvReader{f.memberP, f.memberBoth} {
		t.Run(r.name, func(t *testing.T) {
			resp := xpCallFull(t, f.h.Patch,
				f.request(t, r, http.MethodPatch, strconv.Itoa(version), map[string]any{"notes": "patched by " + r.name}))
			f.assertBVCard(t, r, resp)
			if v, ok := resp.Body["version"].(float64); ok {
				version = int(v)
			} else {
				t.Fatalf("%s: PATCH response has no version: %s", r.name, resp.Raw)
			}
		})
	}
}
