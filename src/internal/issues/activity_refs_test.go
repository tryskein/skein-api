package issues

// sk-1007 US-2 on the activity feeds — matrix rows No. 26 and No. 30.
// Reference — the spec, US-2: in a row of a project the reader sees, every
// id of a task the reader does not see now becomes the literal "<hidden>"
// (also inside the composite link id), the project field of a hidden end
// is present with the value null, visible references stay whole, the row
// stays (R-1…R-3), on every feed of R-4 (project, issue, /me, the visible
// row of the war-room); a visible title that contains a hidden id is not
// edited (R-3). SC-006: the feed reads visibility once per page, so the
// number of statements does not depend on the number of rows.
//
// The helpers (refsCorpus, seedRefEvent, assertNoHiddenLeak) are written
// to be reused by the R-4a rows of this file.

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/redact"
	"skein-api/internal/testsupport"
)

// hiddenMarker is the contract literal of the spec (R-1), not the
// constant of the code under test (TEST §5).
const hiddenMarker = "<hidden>"

// refEvent is one events row as the product's writers shape it:
// dependencies/events.go#WriteDependencyEvent (entity_type "dep", the
// composite "from::to::type", the data object with the ends' projects),
// #WriteReparentEvent and the issue.created snapshot.
type refEvent struct {
	typ        string
	entityType string
	entityID   string
	projectID  uuid.UUID
	oldData    any
	newData    any
	payload    any
}

// seedRefEvent writes ev by actor and returns its id; the row is removed
// at cleanup.
func seedRefEvent(t *testing.T, pool *pgxpool.Pool, actor uuid.UUID, actorName string, ev refEvent) int64 {
	t.Helper()
	enc := func(v any) []byte {
		if v == nil {
			return nil
		}
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal event data: %v", err)
		}
		return b
	}
	payload := enc(ev.payload)
	if payload == nil {
		payload = []byte(`{}`)
	}
	var id int64
	if err := pool.QueryRow(context.Background(), `
INSERT INTO events (occurred_at, type, actor, actor_user_id, actor_type,
                    entity_type, entity_id, project_id, old_data, new_data, payload)
VALUES (now(), $1, $2, $3, 'human', $4, $5, $6, $7::jsonb, $8::jsonb, $9::jsonb)
RETURNING id`,
		ev.typ, actorName, actor, ev.entityType, ev.entityID, ev.projectID,
		enc(ev.oldData), enc(ev.newData), payload).Scan(&id); err != nil {
		t.Fatalf("seed %s event %s: %v", ev.typ, ev.entityID, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE id = $1`, id)
	})
	return id
}

// linkEvent is a dependency.created row from → to as WriteDependencyEvent
// writes it; the ends' projects are the ones recorded at the write.
func linkEvent(owner, from, to string, fromProject uuid.UUID, toProject *uuid.UUID) refEvent {
	var toP any
	if toProject != nil {
		toP = toProject.String()
	}
	return refEvent{
		typ: "dependency.created", entityType: "dep",
		entityID:  from + "::" + to + "::blocks",
		projectID: fromProject,
		newData: map[string]any{
			"fromId": from, "toId": to, "type": "blocks", "metadata": map[string]any{},
			"createdBy": owner, "fromProjectId": fromProject.String(), "toProjectId": toP,
		},
	}
}

// refsCorpus is the fixture of row No. 26: a visible project P, a second
// visible project X and a project H the reader does not see, with events
// in P that name tasks of all three.
type refsCorpus struct {
	pool              *pgxpool.Pool
	owner, reader     uuid.UUID
	ownerName         string
	p, x, h           uuid.UUID
	pPre, xPre, hPre  string
	hTitles           []string
	linkHidden        int64 // P-1 blocks H-5
	linkVisible       int64 // P-1 blocks X-2
	created           int64 // P-3 created under H-1
	reparented        int64 // P-4 moved from [P-2, H-2] to H-3
	titled            int64 // P-6 created, title "fix H-5"
	titledTitle       string
	pIDs              map[string]string // short name → id in P
	xLinkTarget, hEnd string
}

func newRefsCorpus(t *testing.T, pool *pgxpool.Pool) *refsCorpus {
	t.Helper()
	c := &refsCorpus{pool: pool, pIDs: map[string]string{}}
	c.owner, c.ownerName = testsupport.SeedUser(t, pool, "user")
	c.reader, _ = testsupport.SeedUser(t, pool, "user")
	c.p, c.pPre = testsupport.SeedProject(t, pool, c.owner, "rfp")
	c.x, c.xPre = testsupport.SeedProject(t, pool, c.owner, "rfx")
	c.h, c.hPre = testsupport.SeedProject(t, pool, c.owner, "rfh")
	testsupport.AddProjectMember(t, pool, c.p, c.reader, "member")
	testsupport.AddProjectMember(t, pool, c.x, c.reader, "member")

	for _, n := range []string{"1", "2", "3", "4"} {
		c.pIDs[n] = testsupport.SeedIssue(t, pool, c.p, c.owner, c.pPre+"-"+n, "visible task "+n, "open", 2)
	}
	hID := func(n string) string {
		title := "hidden title " + n
		c.hTitles = append(c.hTitles, title)
		return testsupport.SeedIssue(t, pool, c.h, c.owner, c.hPre+"-"+n, title, "open", 2)
	}
	c.hEnd = hID("5")
	h1, h2, h3 := hID("1"), hID("2"), hID("3")
	c.xLinkTarget = testsupport.SeedIssue(t, pool, c.x, c.owner, c.xPre+"-2", "other visible task", "open", 2)
	c.titledTitle = "fix " + c.hEnd
	c.pIDs["6"] = testsupport.SeedIssue(t, pool, c.p, c.owner, c.pPre+"-6", c.titledTitle, "open", 2)

	owner := c.owner.String()
	seed := func(ev refEvent) int64 { return seedRefEvent(t, pool, c.owner, c.ownerName, ev) }
	c.linkHidden = seed(linkEvent(owner, c.pIDs["1"], c.hEnd, c.p, &c.h))
	c.linkVisible = seed(linkEvent(owner, c.pIDs["1"], c.xLinkTarget, c.p, &c.x))
	c.created = seed(refEvent{
		typ: "issue.created", entityType: "issue", entityID: c.pIDs["3"], projectID: c.p,
		newData: map[string]any{"id": c.pIDs["3"], "projectId": c.p.String(), "parentId": h1, "title": "visible task 3"},
	})
	c.reparented = seed(refEvent{
		typ: "issue.reparented", entityType: "issue", entityID: c.pIDs["4"], projectID: c.p,
		oldData: map[string]any{"parentIds": []string{c.pIDs["2"], h2}},
		newData: map[string]any{"parentId": h3},
		payload: map[string]any{"oldParentIds": []string{c.pIDs["2"], h2}, "newParentId": h3},
	})
	c.titled = seed(refEvent{
		typ: "issue.created", entityType: "issue", entityID: c.pIDs["6"], projectID: c.p,
		newData: map[string]any{"id": c.pIDs["6"], "projectId": c.p.String(), "parentId": nil, "title": c.titledTitle},
	})
	return c
}

// readerCtx is the reader's request context: the principal, the request's
// memo of the visible set, and no API key.
func (c *refsCorpus) readerCtx() context.Context {
	ctx := auth.WithUserRole(auth.WithUserID(context.Background(), c.reader), "user")
	return authz.WithRequestCache(ctx)
}

func (c *refsCorpus) visible() map[uuid.UUID]struct{} {
	return map[uuid.UUID]struct{}{c.p: {}, c.x: {}}
}

// assertNoHiddenLeak marshals v as it goes on the wire and requires that
// no id, prefix, UUID or title of H is in it. allowed is user text of a
// visible object that may contain an id of H (R-3); its occurrences are
// removed before the check.
func (c *refsCorpus) assertNoHiddenLeak(t *testing.T, label string, v any, allowed ...string) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s: marshal: %v", label, err)
	}
	s := string(b)
	for _, a := range allowed {
		s = strings.ReplaceAll(s, a, "")
	}
	for _, needle := range append([]string{c.hPre, c.h.String()}, c.hTitles...) {
		if strings.Contains(s, needle) {
			t.Errorf("%s: the answer names the hidden project's %q:\n%s", label, needle, s)
		}
	}
}

func asObject(t *testing.T, label string, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s: value %#v is not an object", label, v)
	}
	return m
}

// assertNullKey requires key present in obj with the value null (K5).
func assertNullKey(t *testing.T, label string, obj map[string]any, key string) {
	t.Helper()
	v, ok := obj[key]
	if !ok {
		t.Errorf("%s: key %q is missing, want it present with null", label, key)
		return
	}
	if v != nil {
		t.Errorf("%s: %s = %#v, want null", label, key, v)
	}
}

// checkActivityRows applies the per-row expectations of the corpus to the
// rows of a feed and requires exactly the event ids want, so the feed lost
// no row and gained none (R-2, SC-003).
func (c *refsCorpus) checkActivityRows(t *testing.T, label string, items []ActivityEntry, want []int64) {
	t.Helper()
	var got []int64
	byID := map[int64]ActivityEntry{}
	for _, e := range items {
		got = append(got, e.ID)
		byID[e.ID] = e
	}
	slices.Sort(got)
	want = slices.Clone(want)
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: rows %v, want %v", label, got, want)
	}
	for id, e := range byID {
		rowLabel := fmt.Sprintf("%s, event %d", label, id)
		switch id {
		case c.linkHidden:
			if wantID := c.pIDs["1"] + "::" + hiddenMarker + "::blocks"; e.Entity.ID != wantID {
				t.Errorf("%s: entity.id = %q, want %q", rowLabel, e.Entity.ID, wantID)
			}
			nv := asObject(t, rowLabel, e.NewValue)
			if nv["toId"] != hiddenMarker || nv["fromId"] != c.pIDs["1"] {
				t.Errorf("%s: fromId/toId = %v/%v, want %s/%s", rowLabel, nv["fromId"], nv["toId"], c.pIDs["1"], hiddenMarker)
			}
			assertNullKey(t, rowLabel, nv, "toProjectId")
			if nv["fromProjectId"] != c.p.String() {
				t.Errorf("%s: fromProjectId = %v, want the visible %s", rowLabel, nv["fromProjectId"], c.p)
			}
			if !strings.Contains(e.Summary, hiddenMarker) {
				t.Errorf("%s: summary %q does not carry the marker", rowLabel, e.Summary)
			}
		case c.linkVisible:
			if wantID := c.pIDs["1"] + "::" + c.xLinkTarget + "::blocks"; e.Entity.ID != wantID {
				t.Errorf("%s: entity.id = %q, want the whole %q", rowLabel, e.Entity.ID, wantID)
			}
			nv := asObject(t, rowLabel, e.NewValue)
			if nv["toId"] != c.xLinkTarget || nv["toProjectId"] != c.x.String() {
				t.Errorf("%s: toId/toProjectId = %v/%v, want %s/%s", rowLabel, nv["toId"], nv["toProjectId"], c.xLinkTarget, c.x)
			}
		case c.created:
			nv := asObject(t, rowLabel, e.NewValue)
			if nv["parentId"] != hiddenMarker {
				t.Errorf("%s: parentId = %v, want %s", rowLabel, nv["parentId"], hiddenMarker)
			}
			if nv["projectId"] != c.p.String() {
				t.Errorf("%s: the row's own projectId = %v, want %s", rowLabel, nv["projectId"], c.p)
			}
		case c.reparented:
			ov := asObject(t, rowLabel, e.OldValue)
			if wantOld := []any{c.pIDs["2"], hiddenMarker}; !reflect.DeepEqual(ov["parentIds"], wantOld) {
				t.Errorf("%s: old parentIds = %#v, want %#v", rowLabel, ov["parentIds"], wantOld)
			}
			if nv := asObject(t, rowLabel, e.NewValue); nv["parentId"] != hiddenMarker {
				t.Errorf("%s: new parentId = %v, want %s", rowLabel, nv["parentId"], hiddenMarker)
			}
		case c.titled:
			if e.Entity.Title != c.titledTitle {
				t.Errorf("%s: the visible title = %q, want it unchanged %q (R-3)", rowLabel, e.Entity.Title, c.titledTitle)
			}
		}
	}
	c.assertNoHiddenLeak(t, label, items, c.titledTitle)
}

// TestActivityRefs_EveryFeedRedactsHiddenReferences — row No. 26.
func TestActivityRefs_EveryFeedRedactsHiddenReferences(t *testing.T) {
	pool := newTestPool(t)
	c := newRefsCorpus(t, pool)
	store := NewStore(pool)
	all := []int64{c.linkHidden, c.linkVisible, c.created, c.reparented, c.titled}

	t.Run("project", func(t *testing.T) {
		items, _, err := store.ListProjectActivity(c.readerCtx(), c.p, nil, nil, 50)
		if err != nil {
			t.Fatalf("ListProjectActivity: %v", err)
		}
		c.checkActivityRows(t, "project feed", items, all)
	})
	t.Run("issue", func(t *testing.T) {
		for _, tc := range []struct {
			issue string
			want  []int64
		}{
			{c.pIDs["1"], []int64{c.linkHidden, c.linkVisible}},
			{c.pIDs["3"], []int64{c.created}},
			{c.pIDs["4"], []int64{c.reparented}},
		} {
			items, _, err := store.ListIssueActivity(c.readerCtx(), c.p, tc.issue, nil, nil, 50)
			if err != nil {
				t.Fatalf("ListIssueActivity %s: %v", tc.issue, err)
			}
			c.checkActivityRows(t, "issue feed of "+tc.issue, items, tc.want)
		}
	})
	t.Run("me", func(t *testing.T) {
		items, _, err := store.ListUserActivity(c.readerCtx(), c.reader, c.visible(), false, nil, nil, 50, nil, redact.None)
		if err != nil {
			t.Fatalf("ListUserActivity: %v", err)
		}
		c.checkActivityRows(t, "/me feed", items, all)
	})
	t.Run("war-room visible rows", func(t *testing.T) {
		items, _, err := store.ListUserActivity(c.readerCtx(), c.reader, c.visible(), false, nil, nil, 50, c.visible(), redact.Soft)
		if err != nil {
			t.Fatalf("ListUserActivity: %v", err)
		}
		c.checkActivityRows(t, "war-room feed", items, all)
	})
}

// seedHiddenLinks writes n link events in project from tasks of it to n
// distinct tasks of hidden — every row names its own hidden task.
func seedHiddenLinks(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID, ownerName string,
	project uuid.UUID, prefix string, hidden uuid.UUID, hPrefix string, first, n int) {
	t.Helper()
	from := testsupport.SeedIssue(t, pool, project, owner, prefix+"-1", "source", "open", 2)
	for i := range n {
		to := testsupport.SeedIssue(t, pool, hidden, owner, fmt.Sprintf("%s-%d", hPrefix, first+i), "hidden end", "open", 2)
		seedRefEvent(t, pool, owner, ownerName, linkEvent(owner.String(), from, to, project, &hidden))
	}
}

// TestActivityRefs_StatementsDoNotGrowWithRows — row No. 30 (SC-006): one
// ListProjectActivity and one ListUserActivity send the same number of
// statements for a page of 1 and of 30 rows naming hidden tasks.
func TestActivityRefs_StatementsDoNotGrowWithRows(t *testing.T) {
	pool, tracer := newTracedProgressPool(t)
	owner, ownerName := testsupport.SeedUser(t, pool, "user")
	reader, _ := testsupport.SeedUser(t, pool, "user")
	h, hPre := testsupport.SeedProject(t, pool, owner, "rch")
	p1, p1Pre := testsupport.SeedProject(t, pool, owner, "rc1")
	p30, p30Pre := testsupport.SeedProject(t, pool, owner, "rc30")
	testsupport.AddProjectMember(t, pool, p1, reader, "member")
	testsupport.AddProjectMember(t, pool, p30, reader, "member")
	seedHiddenLinks(t, pool, owner, ownerName, p1, p1Pre, h, hPre, 100, 1)
	seedHiddenLinks(t, pool, owner, ownerName, p30, p30Pre, h, hPre, 200, 30)
	store := NewStore(pool)
	ctx := func() context.Context {
		return authz.WithRequestCache(auth.WithUserRole(auth.WithUserID(context.Background(), reader), "user"))
	}

	type run struct {
		statements int
		rows       int
	}
	measure := func(t *testing.T, f func() ([]ActivityEntry, error)) run {
		t.Helper()
		var items []ActivityEntry
		var err error
		queries := tracer.record(func() { items, err = f() })
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range items {
			if !strings.Contains(e.Entity.ID, hiddenMarker) {
				t.Fatalf("row %d entity.id %q is not redacted — the corpus does not exercise the redaction", e.ID, e.Entity.ID)
			}
		}
		return run{statements: len(queries), rows: len(items)}
	}
	for _, feed := range []struct {
		name string
		list func(project uuid.UUID) func() ([]ActivityEntry, error)
	}{
		{"project", func(project uuid.UUID) func() ([]ActivityEntry, error) {
			return func() ([]ActivityEntry, error) {
				items, _, err := store.ListProjectActivity(ctx(), project, nil, nil, 50)
				return items, err
			}
		}},
		{"user", func(project uuid.UUID) func() ([]ActivityEntry, error) {
			return func() ([]ActivityEntry, error) {
				visible := map[uuid.UUID]struct{}{project: {}}
				items, _, err := store.ListUserActivity(ctx(), reader, visible, false, nil, nil, 50, nil, redact.None)
				return items, err
			}
		}},
	} {
		t.Run(feed.name, func(t *testing.T) {
			one := measure(t, feed.list(p1))
			thirty := measure(t, feed.list(p30))
			if one.rows != 1 || thirty.rows != 30 {
				t.Fatalf("pages of %d and %d rows, want 1 and 30", one.rows, thirty.rows)
			}
			if one.statements != thirty.statements {
				t.Errorf("a page of 1 row sent %d statements, a page of 30 rows %d — visibility is read per row",
					one.statements, thirty.statements)
			}
		})
	}
}
