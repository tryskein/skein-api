package notifications_test

// sk-1007 US-2 on the notification rows and the monitor poll — matrix rows
// No. 27, 28, 29 and 31. Reference — the spec, US-2 and its edge cases:
//
//   - R-1…R-4: on the inbox (ListInbox — shared by the REST UI, the public
//     GET notifications and MCP inbox), the Watching timeline
//     (HydrateWatchingRecentEvents) and the monitor poll (PollForMonitor),
//     a task id the reader does not see now is the literal "<hidden>", also
//     inside the composite link id; the project field of a hidden end is
//     present with null; visible references stay whole; the row stays; a
//     visible title containing a hidden id is not edited;
//   - INV-4 / INV-1: the marker does not depend on why the task is hidden —
//     a project without access, a deleted task, a project outside the API
//     key's scope, a project of another space (a link made before INV-24) —
//     on the inbox and on the /me feed alike;
//   - edge "an end moved after the event was written": visibility follows
//     the end's current project, not its id prefix nor the recorded UUID;
//   - SC-006: visibility is read once per page — the same number of
//     statements for 1 and for 30 rows naming hidden tasks.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/issues"
	"skein-api/internal/notifications"
	"skein-api/internal/redact"
	"skein-api/internal/testsupport"
)

// hiddenMarker is the contract literal of the spec (R-1), not the
// constant of the code under test (TEST §5).
const hiddenMarker = "<hidden>"

// refEvent is one events row as the product's writers shape it
// (dependencies/events.go#WriteDependencyEvent, #WriteReparentEvent, the
// issue.created snapshot).
type refEvent struct {
	typ        string
	entityType string
	entityID   string
	projectID  uuid.UUID
	oldData    any
	newData    any
	payload    any
}

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
VALUES (now() - interval '1 minute', $1, $2, $3, 'human', $4, $5, $6, $7::jsonb, $8::jsonb, $9::jsonb)
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

// linkEvent is a dependency.created row from → to; toProject is the end's
// project recorded at the write.
func linkEvent(owner uuid.UUID, from, to string, fromProject, toProject uuid.UUID) refEvent {
	return refEvent{
		typ: "dependency.created", entityType: "dep",
		entityID:  from + "::" + to + "::blocks",
		projectID: fromProject,
		newData: map[string]any{
			"fromId": from, "toId": to, "type": "blocks", "metadata": map[string]any{},
			"createdBy": owner.String(), "fromProjectId": fromProject.String(), "toProjectId": toProject.String(),
		},
	}
}

func reparentEvent(issue string, project uuid.UUID, oldParents []string, newParent string) refEvent {
	return refEvent{
		typ: "issue.reparented", entityType: "issue", entityID: issue, projectID: project,
		oldData: map[string]any{"parentIds": oldParents},
		newData: map[string]any{"parentId": newParent},
		payload: map[string]any{"oldParentIds": oldParents, "newParentId": newParent},
	}
}

// notify writes the reader's feed row of event, carded to card ("" — no
// card); removed with the reader's rows at cleanup.
func notify(t *testing.T, pool *pgxpool.Pool, reader uuid.UUID, event int64, typ string, project uuid.UUID, card string) int64 {
	t.Helper()
	var cardArg any
	if card != "" {
		cardArg = card
	}
	var id int64
	if err := pool.QueryRow(context.Background(), `
INSERT INTO notifications (user_id, event_id, type, project_id, feed_channel, card_issue_id, card_resolved)
VALUES ($1, $2, $3, $4, true, $5, true)
RETURNING id`, reader, event, typ, project, cardArg).Scan(&id); err != nil {
		t.Fatalf("seed notification of event %d: %v", event, err)
	}
	return id
}

func cleanupNotifications(t *testing.T, pool *pgxpool.Pool, users ...uuid.UUID) {
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM notifications WHERE user_id = ANY($1::uuid[])`, users)
	})
}

func userCtx(user uuid.UUID) context.Context {
	return authz.WithRequestCache(auth.WithUserRole(auth.WithUserID(context.Background(), user), "user"))
}

// keyCtx is user's request authenticated by a key whose scope is projects.
func keyCtx(user uuid.UUID, projects ...uuid.UUID) context.Context {
	ctx := auth.WithUserRole(auth.WithUserID(context.Background(), user), "user")
	ctx = auth.WithAPIKeyID(ctx, uuid.New())
	ctx = auth.WithKeyGrant(ctx, apikeys.Grant{Level: apikeys.LevelManage,
		Scope: apikeys.Scope{Mode: apikeys.ScopeProjects, Projects: projects}})
	return authz.WithRequestCache(ctx)
}

func asObject(t *testing.T, label string, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s: value %#v is not an object", label, v)
	}
	return m
}

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

// ---------------------------------------------------------------------------
// Row No. 27 — the corpus of row No. 26 on the three notification surfaces.

type rowsCorpus struct {
	pool                    *pgxpool.Pool
	owner, reader           uuid.UUID
	ownerName, readerName   string
	p, x, h                 uuid.UUID
	pPre, xPre, hPre        string
	hTitles                 []string
	p1, p2, p3, p4, p6, x2  string
	hEnd                    string
	titledTitle             string
	linkHidden, linkVisible int64 // event ids
	created, reparented     int64
	titled, mention         int64
	notifOf                 map[int64]int64 // notification id → event id
}

func newRowsCorpus(t *testing.T) *rowsCorpus {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	c := &rowsCorpus{pool: pool, notifOf: map[int64]int64{}}
	c.owner, c.ownerName = testsupport.SeedUser(t, pool, "user")
	c.reader, c.readerName = testsupport.SeedUser(t, pool, "user")
	c.p, c.pPre = testsupport.SeedProject(t, pool, c.owner, "nrp")
	c.x, c.xPre = testsupport.SeedProject(t, pool, c.owner, "nrx")
	c.h, c.hPre = testsupport.SeedProject(t, pool, c.owner, "nrh")
	testsupport.AddProjectMember(t, pool, c.p, c.reader, "member")
	testsupport.AddProjectMember(t, pool, c.x, c.reader, "member")
	cleanupNotifications(t, pool, c.reader)

	seedP := func(n string) string {
		return testsupport.SeedIssue(t, pool, c.p, c.owner, c.pPre+"-"+n, "visible task "+n, "open", 2)
	}
	c.p1, c.p2, c.p3, c.p4 = seedP("1"), seedP("2"), seedP("3"), seedP("4")
	seedH := func(n string) string {
		title := "hidden title " + n
		c.hTitles = append(c.hTitles, title)
		return testsupport.SeedIssue(t, pool, c.h, c.owner, c.hPre+"-"+n, title, "open", 2)
	}
	c.hEnd = seedH("5")
	h1, h2, h3 := seedH("1"), seedH("2"), seedH("3")
	c.x2 = testsupport.SeedIssue(t, pool, c.x, c.owner, c.xPre+"-2", "other visible task", "open", 2)
	c.titledTitle = "fix " + c.hEnd
	c.p6 = testsupport.SeedIssue(t, pool, c.p, c.owner, c.pPre+"-6", c.titledTitle, "open", 2)

	seed := func(ev refEvent) int64 {
		id := seedRefEvent(t, pool, c.owner, c.ownerName, ev)
		// Every corpus row is on the reader's card of P-1 (the Watching
		// timeline shows WatchingRecentTopN = 5 of them).
		c.notifOf[notify(t, pool, c.reader, id, ev.typ, c.p, c.p1)] = id
		return id
	}
	c.linkHidden = seed(linkEvent(c.owner, c.p1, c.hEnd, c.p, c.h))
	c.linkVisible = seed(linkEvent(c.owner, c.p1, c.x2, c.p, c.x))
	c.created = seed(refEvent{
		typ: "issue.created", entityType: "issue", entityID: c.p3, projectID: c.p,
		newData: map[string]any{"id": c.p3, "projectId": c.p.String(), "parentId": h1, "title": "visible task 3"},
	})
	c.reparented = seed(reparentEvent(c.p4, c.p, []string{c.p2, h2}, h3))
	c.titled = seed(refEvent{
		typ: "issue.created", entityType: "issue", entityID: c.p6, projectID: c.p,
		newData: map[string]any{"id": c.p6, "projectId": c.p.String(), "parentId": nil, "title": c.titledTitle},
	})

	// A mention row between them on the inbox page: its context must stay on
	// its own row after the redaction pass.
	comment := testsupport.SeedComment(t, pool, c.p1, c.owner, "please look @"+c.readerName+" at the link")
	c.mention = seedRefEvent(t, pool, c.owner, c.ownerName, refEvent{
		typ: "comment.mention", entityType: "comment", entityID: comment.String(), projectID: c.p,
	})
	c.notifOf[notify(t, pool, c.reader, c.mention, "comment.mention", c.p, "")] = c.mention
	return c
}

func (c *rowsCorpus) assertNoHiddenLeak(t *testing.T, label string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s: marshal: %v", label, err)
	}
	s := strings.ReplaceAll(string(b), c.titledTitle, "")
	for _, needle := range append([]string{c.hPre, c.h.String()}, c.hTitles...) {
		if strings.Contains(s, needle) {
			t.Errorf("%s: the answer names the hidden project's %q:\n%s", label, needle, s)
		}
	}
}

// checkRow applies the expectations of the event's row to a wire row.
func (c *rowsCorpus) checkRow(t *testing.T, label string, event int64, n notifications.NotificationDTO) {
	t.Helper()
	switch event {
	case c.linkHidden:
		if want := c.p1 + "::" + hiddenMarker + "::blocks"; n.Entity.ID != want {
			t.Errorf("%s: entity.id = %q, want %q", label, n.Entity.ID, want)
		}
		nv := asObject(t, label, n.NewValue)
		if nv["toId"] != hiddenMarker || nv["fromId"] != c.p1 || nv["fromProjectId"] != c.p.String() {
			t.Errorf("%s: fromId/toId/fromProjectId = %v/%v/%v, want %s/%s/%s",
				label, nv["fromId"], nv["toId"], nv["fromProjectId"], c.p1, hiddenMarker, c.p)
		}
		assertNullKey(t, label, nv, "toProjectId")
		if !strings.Contains(n.Summary, hiddenMarker) {
			t.Errorf("%s: summary %q does not carry the marker", label, n.Summary)
		}
	case c.linkVisible:
		if want := c.p1 + "::" + c.x2 + "::blocks"; n.Entity.ID != want {
			t.Errorf("%s: entity.id = %q, want the whole %q", label, n.Entity.ID, want)
		}
		nv := asObject(t, label, n.NewValue)
		if nv["toId"] != c.x2 || nv["toProjectId"] != c.x.String() {
			t.Errorf("%s: toId/toProjectId = %v/%v, want %s/%s", label, nv["toId"], nv["toProjectId"], c.x2, c.x)
		}
	case c.created:
		if nv := asObject(t, label, n.NewValue); nv["parentId"] != hiddenMarker {
			t.Errorf("%s: parentId = %v, want %s", label, nv["parentId"], hiddenMarker)
		}
	case c.reparented:
		ov := asObject(t, label, n.OldValue)
		if want := []any{c.p2, hiddenMarker}; !reflect.DeepEqual(ov["parentIds"], want) {
			t.Errorf("%s: old parentIds = %#v, want %#v", label, ov["parentIds"], want)
		}
		if nv := asObject(t, label, n.NewValue); nv["parentId"] != hiddenMarker {
			t.Errorf("%s: new parentId = %v, want %s", label, nv["parentId"], hiddenMarker)
		}
	case c.titled:
		if n.Entity.Title != c.titledTitle {
			t.Errorf("%s: the visible title = %q, want it unchanged %q (R-3)", label, n.Entity.Title, c.titledTitle)
		}
	}
}

func sortedIDs(ids []int64) []int64 {
	out := slices.Clone(ids)
	slices.Sort(out)
	return out
}

// TestEventRefs_NotificationSurfacesRedactHiddenReferences — row No. 27.
func TestEventRefs_NotificationSurfacesRedactHiddenReferences(t *testing.T) {
	c := newRowsCorpus(t)
	store := notifications.NewStore(c.pool)
	corpus := []int64{c.linkHidden, c.linkVisible, c.created, c.reparented, c.titled}

	t.Run("inbox", func(t *testing.T) {
		res, err := store.ListInbox(userCtx(c.reader), notifications.ListInboxArgs{UserID: c.reader, Scope: "all", Limit: 50})
		if err != nil {
			t.Fatalf("ListInbox: %v", err)
		}
		var events []int64
		for _, n := range res.Items {
			event, ok := c.notifOf[n.ID]
			if !ok {
				t.Fatalf("inbox row %d is not of the corpus", n.ID)
			}
			events = append(events, event)
			label := fmt.Sprintf("inbox row of event %d", event)
			c.checkRow(t, label, event, n)
			if event == c.mention {
				if !strings.Contains(n.MentionContext, "@"+c.readerName) {
					t.Errorf("%s: mentionContext = %q, want the snippet with @%s", label, n.MentionContext, c.readerName)
				}
			} else if n.MentionContext != "" {
				t.Errorf("%s: carries the mention context %q of another row", label, n.MentionContext)
			}
		}
		if want := sortedIDs(append(slices.Clone(corpus), c.mention)); !reflect.DeepEqual(sortedIDs(events), want) {
			t.Fatalf("inbox rows of events %v, want %v", sortedIDs(events), want)
		}
		c.assertNoHiddenLeak(t, "inbox", res)
	})

	t.Run("watching timeline", func(t *testing.T) {
		got, err := store.HydrateWatchingRecentEvents(userCtx(c.reader), c.reader, []string{c.p1})
		if err != nil {
			t.Fatalf("HydrateWatchingRecentEvents: %v", err)
		}
		var events []int64
		for _, n := range got[c.p1] {
			event := c.notifOf[n.ID]
			events = append(events, event)
			c.checkRow(t, fmt.Sprintf("timeline row of event %d", event), event, n)
		}
		if want := sortedIDs(corpus); !reflect.DeepEqual(sortedIDs(events), want) {
			t.Fatalf("timeline rows of events %v, want %v", sortedIDs(events), want)
		}
		c.assertNoHiddenLeak(t, "watching timeline", got)
		// The timeline row carries the short id of its project's space, as
		// every feed row does.
		var pSpace string
		if err := c.pool.QueryRow(context.Background(),
			`SELECT s.short_id FROM projects p JOIN spaces s ON s.id = p.space_id WHERE p.id = $1`, c.p).Scan(&pSpace); err != nil {
			t.Fatalf("read the space of P: %v", err)
		}
		for _, n := range got[c.p1] {
			if n.SpaceID != pSpace {
				t.Errorf("timeline row %d: spaceId = %q, want %q", n.ID, n.SpaceID, pSpace)
			}
		}
	})

	t.Run("monitor poll", func(t *testing.T) {
		// The poll delivers issue and comment events of its scope issue: the
		// creation of P-3 and the move of P-4.
		for _, tc := range []struct {
			scope string
			event int64
		}{{c.p3, c.created}, {c.p4, c.reparented}} {
			res, err := store.PollForMonitor(userCtx(c.reader), pollParams(c.reader, c.p, tc.scope))
			if err != nil {
				t.Fatalf("PollForMonitor %s: %v", tc.scope, err)
			}
			if len(res.Items) != 1 || res.Items[0].ID != tc.event {
				t.Fatalf("poll of %s returned %d rows, want the one event %d", tc.scope, len(res.Items), tc.event)
			}
			c.checkRow(t, "poll row of "+tc.scope, tc.event, res.Items[0])
			c.assertNoHiddenLeak(t, "poll of "+tc.scope, res.Items)
		}
	})

	t.Run("inbox keeps the actor's marks", func(t *testing.T) {
		// A link to a hidden task made by a person outside P (no member of
		// it, no participant of its space): after the redaction and the
		// hydration the row still marks the actor departed (person.Status).
		departed, departedName := testsupport.SeedUser(t, c.pool, "user")
		reader, _ := testsupport.SeedUser(t, c.pool, "user")
		testsupport.AddProjectMember(t, c.pool, c.p, reader, "member")
		cleanupNotifications(t, c.pool, reader)
		event := seedRefEvent(t, c.pool, departed, departedName, linkEvent(departed, c.p2, c.hEnd, c.p, c.h))
		notify(t, c.pool, reader, event, "dependency.created", c.p, c.p2)
		res, err := store.ListInbox(userCtx(reader), notifications.ListInboxArgs{UserID: reader, Scope: "all", Limit: 50})
		if err != nil {
			t.Fatalf("ListInbox: %v", err)
		}
		if len(res.Items) != 1 {
			t.Fatalf("inbox of %d rows, want the one row of event %d", len(res.Items), event)
		}
		n := res.Items[0]
		if want := c.p2 + "::" + hiddenMarker + "::blocks"; n.Entity.ID != want {
			t.Errorf("entity.id = %q, want %q", n.Entity.ID, want)
		}
		if n.Actor.Username != departedName || !n.Actor.Status.Departed {
			t.Errorf("actor %q departed=%v, want %q marked departed", n.Actor.Username, n.Actor.Status.Departed, departedName)
		}
	})
}

func pollParams(user, project uuid.UUID, scope string) notifications.PollParams {
	epoch := time.Unix(0, 0).UTC()
	var zero int64
	return notifications.PollParams{
		UserID: user, ProjectID: project, ScopeIssueID: scope,
		EventTypes: []string{"issue.created", "issue.reparented"},
		SinceTS:    &epoch, SinceID: &zero, Limit: 50,
	}
}

// ---------------------------------------------------------------------------
// Row No. 28 — INV-4 over the causes of hiddenness, with the key's scope.

// TestEventRefs_HiddenEndIsTheSameWhateverTheCause — row No. 28.
func TestEventRefs_HiddenEndIsTheSameWhateverTheCause(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, ownerName := testsupport.SeedUser(t, pool, "user")
	reader, _ := testsupport.SeedUser(t, pool, "user")
	p, pPre := testsupport.SeedProject(t, pool, owner, "ncp")
	k, kPre := testsupport.SeedProject(t, pool, owner, "nck")
	h, hPre := testsupport.SeedProject(t, pool, owner, "nch")
	other := testsupport.SeedSpace(t, pool, owner)
	s, sPre := testsupport.SeedProjectInSpace(t, pool, owner, other, "ncs")
	testsupport.AddProjectMember(t, pool, p, reader, "member")
	testsupport.AddProjectMember(t, pool, k, reader, "member") // visible to him, outside the key
	cleanupNotifications(t, pool, reader)

	from := testsupport.SeedIssue(t, pool, p, owner, pPre+"-1", "source", "open", 2)
	ends := []struct {
		cause   string
		to      string
		project uuid.UUID
	}{
		{"no access", testsupport.SeedIssue(t, pool, h, owner, hPre+"-5", "secret", "open", 2), h},
		{"deleted", pPre + "-99", p}, // no row: the task is gone, its project was P
		{"outside the key", testsupport.SeedIssue(t, pool, k, owner, kPre+"-7", "keyed out", "open", 2), k},
		{"another space", testsupport.SeedIssue(t, pool, s, owner, sPre+"-8", "foreign", "open", 2), s},
	}
	eventOf := map[int64]string{}
	causeOfEvent := map[int64]string{}
	for _, e := range ends {
		ev := seedRefEvent(t, pool, owner, ownerName, linkEvent(owner, from, e.to, p, e.project))
		eventOf[notify(t, pool, reader, ev, "dependency.created", p, "")] = e.cause
		causeOfEvent[ev] = e.cause
	}
	ctx := func() context.Context { return keyCtx(reader, p) }

	// sameAnswer requires every cause to give the same entity id, the same
	// data object and the same summary, with the marker and the null.
	type answer struct{ entity, data, summary string }
	sameAnswer := func(t *testing.T, surface string, byCause map[string]answer) {
		t.Helper()
		if len(byCause) != len(ends) {
			t.Fatalf("%s: rows for causes %v, want all %d", surface, byCause, len(ends))
		}
		want := answer{
			entity: from + "::" + hiddenMarker + "::blocks",
		}
		for cause, a := range byCause {
			if a.entity != want.entity {
				t.Errorf("%s, %s: entity.id = %q, want %q", surface, cause, a.entity, want.entity)
			}
			var data map[string]any
			if err := json.Unmarshal([]byte(a.data), &data); err != nil {
				t.Fatalf("%s, %s: data %s: %v", surface, cause, a.data, err)
			}
			if data["toId"] != hiddenMarker {
				t.Errorf("%s, %s: toId = %v, want %s", surface, cause, data["toId"], hiddenMarker)
			}
			assertNullKey(t, surface+", "+cause, data, "toProjectId")
		}
		first := byCause[ends[0].cause]
		for cause, a := range byCause {
			if a != first {
				t.Errorf("%s: the answer for %q differs from the one for %q:\n%+v\n%+v",
					surface, cause, ends[0].cause, a, first)
			}
		}
	}
	marshal := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	t.Run("inbox (ScopeFor)", func(t *testing.T) {
		res, err := notifications.NewStore(pool).ListInbox(ctx(), notifications.ListInboxArgs{UserID: reader, Scope: "all", Limit: 50})
		if err != nil {
			t.Fatalf("ListInbox: %v", err)
		}
		byCause := map[string]answer{}
		for _, n := range res.Items {
			byCause[eventOf[n.ID]] = answer{n.Entity.ID, marshal(n.NewValue), n.Summary}
		}
		sameAnswer(t, "inbox", byCause)
	})
	t.Run("/me feed (FromContext)", func(t *testing.T) {
		visible := map[uuid.UUID]struct{}{p: {}}
		items, _, err := issues.NewStore(pool).ListUserActivity(ctx(), reader, visible, false, nil, nil, 50, nil, redact.None)
		if err != nil {
			t.Fatalf("ListUserActivity: %v", err)
		}
		byCause := map[string]answer{}
		for _, e := range items {
			byCause[causeOfEvent[e.ID]] = answer{e.Entity.ID, marshal(e.NewValue), e.Summary}
		}
		sameAnswer(t, "/me feed", byCause)
	})
}

// ---------------------------------------------------------------------------
// Row No. 29 — an end moved to another project after the event.

// TestEventRefs_MovedEndFollowsItsCurrentProject — row No. 29.
func TestEventRefs_MovedEndFollowsItsCurrentProject(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, ownerName := testsupport.SeedUser(t, pool, "user")
	reader, _ := testsupport.SeedUser(t, pool, "user")
	p, pPre := testsupport.SeedProject(t, pool, owner, "nmp")
	h, hPre := testsupport.SeedProject(t, pool, owner, "nmh")
	testsupport.AddProjectMember(t, pool, p, reader, "member")
	cleanupNotifications(t, pool, reader)

	from := testsupport.SeedIssue(t, pool, p, owner, pPre+"-1", "source", "open", 2)
	// Moved H → P: its id still carries H's prefix, it lives in P now; the
	// event recorded H as its project.
	intoP := testsupport.SeedIssue(t, pool, p, owner, hPre+"-77", "moved in", "open", 2)
	// Moved P → H: P's prefix, lives in H now; the event recorded P.
	outOfP := testsupport.SeedIssue(t, pool, h, owner, pPre+"-78", "moved out", "open", 2)
	in := seedRefEvent(t, pool, owner, ownerName, linkEvent(owner, from, intoP, p, h))
	out := seedRefEvent(t, pool, owner, ownerName, linkEvent(owner, from, outOfP, p, p))
	rowOf := map[int64]int64{
		notify(t, pool, reader, in, "dependency.created", p, ""):  in,
		notify(t, pool, reader, out, "dependency.created", p, ""): out,
	}

	res, err := notifications.NewStore(pool).ListInbox(userCtx(reader), notifications.ListInboxArgs{UserID: reader, Scope: "all", Limit: 50})
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	if len(res.Items) != 2 {
		t.Fatalf("inbox returned %d rows, want 2", len(res.Items))
	}
	for _, n := range res.Items {
		nv := asObject(t, "data", n.NewValue)
		switch rowOf[n.ID] {
		case in:
			if want := from + "::" + intoP + "::blocks"; n.Entity.ID != want || nv["toId"] != intoP {
				t.Errorf("moved into P: entity.id/toId = %q/%v, want the whole %q/%s", n.Entity.ID, nv["toId"], want, intoP)
			}
			assertNullKey(t, "moved into P (recorded project H)", nv, "toProjectId")
		case out:
			if want := from + "::" + hiddenMarker + "::blocks"; n.Entity.ID != want || nv["toId"] != hiddenMarker {
				t.Errorf("moved out of P: entity.id/toId = %q/%v, want %q/%s", n.Entity.ID, nv["toId"], want, hiddenMarker)
			}
			assertNullKey(t, "moved out of P", nv, "toProjectId")
		default:
			t.Fatalf("row %d is not of the corpus", n.ID)
		}
	}
}

// ---------------------------------------------------------------------------
// Row No. 31 — SC-006 on the notification surfaces.

// statementTracer counts the statements pgx sends, after
// notifications/fanout_nplusone_test.go#queryTracer.
type statementTracer struct {
	mu      sync.Mutex
	on      bool
	queries []string
}

func (tr *statementTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.on {
		tr.queries = append(tr.queries, data.SQL)
	}
	return ctx
}

func (tr *statementTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (tr *statementTracer) count(fn func()) int {
	tr.mu.Lock()
	tr.queries, tr.on = nil, true
	tr.mu.Unlock()
	fn()
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.on = false
	return len(tr.queries)
}

// newTracedPool is a pool pinned to one connection with the tracer on it,
// after notifications/fanout_nplusone_test.go#newTracedTestPool.
func newTracedPool(t *testing.T) (*pgxpool.Pool, *statementTracer) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run DB-backed tests")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	tr := &statementTracer{}
	cfg.ConnConfig.Tracer = tr
	cfg.MinConns, cfg.MaxConns = 1, 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, tr
}

// pageCorpus is one reader's page of n rows, each naming its own hidden
// task: n link notifications spread over cards of at most
// WatchingRecentTopN rows, and n moves of the scope issue for the poll.
type pageCorpus struct {
	reader, project uuid.UUID
	cards           []string
	scope           string
}

func newPageCorpus(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID, ownerName string,
	hidden uuid.UUID, hPre string, first, n int, name string) pageCorpus {
	t.Helper()
	reader, _ := testsupport.SeedUser(t, pool, "user")
	project, pre := testsupport.SeedProject(t, pool, owner, name)
	testsupport.AddProjectMember(t, pool, project, reader, "member")
	cleanupNotifications(t, pool, reader)
	c := pageCorpus{reader: reader, project: project}
	c.scope = testsupport.SeedIssue(t, pool, project, owner, pre+"-1", "moved", "open", 2)
	for i := range n {
		if i%notifications.WatchingRecentTopN == 0 {
			c.cards = append(c.cards, testsupport.SeedIssue(t, pool, project, owner,
				fmt.Sprintf("%s-%d", pre, 10+len(c.cards)), "card", "open", 2))
		}
		end := testsupport.SeedIssue(t, pool, hidden, owner, fmt.Sprintf("%s-%d", hPre, first+i), "hidden end", "open", 2)
		link := seedRefEvent(t, pool, owner, ownerName, linkEvent(owner, c.scope, end, project, hidden))
		notify(t, pool, reader, link, "dependency.created", project, c.cards[len(c.cards)-1])
		seedRefEvent(t, pool, owner, ownerName, reparentEvent(c.scope, project, []string{end}, end))
	}
	return c
}

// TestEventRefs_StatementsDoNotGrowWithRows — row No. 31.
func TestEventRefs_StatementsDoNotGrowWithRows(t *testing.T) {
	pool, tracer := newTracedPool(t)
	owner, ownerName := testsupport.SeedUser(t, pool, "user")
	h, hPre := testsupport.SeedProject(t, pool, owner, "nsh")
	one := newPageCorpus(t, pool, owner, ownerName, h, hPre, 100, 1, "ns1")
	thirty := newPageCorpus(t, pool, owner, ownerName, h, hPre, 200, 30, "ns30")
	store := notifications.NewStore(pool)

	// redacted fails the test when a page is not the corpus it should be:
	// n rows, every one naming a hidden task.
	redacted := func(t *testing.T, surface string, items []notifications.NotificationDTO, n int) {
		t.Helper()
		if len(items) != n {
			t.Fatalf("%s returned %d rows, want %d", surface, len(items), n)
		}
		for _, it := range items {
			if !strings.Contains(fmt.Sprint(it.Entity.ID, it.OldValue, it.NewValue), hiddenMarker) {
				t.Fatalf("%s row %d is not redacted — the corpus does not exercise the redaction", surface, it.ID)
			}
		}
	}
	surfaces := []struct {
		name string
		run  func(t *testing.T, c pageCorpus) []notifications.NotificationDTO
	}{
		{"inbox", func(t *testing.T, c pageCorpus) []notifications.NotificationDTO {
			res, err := store.ListInbox(userCtx(c.reader), notifications.ListInboxArgs{UserID: c.reader, Scope: "all", Limit: 50})
			if err != nil {
				t.Fatalf("ListInbox: %v", err)
			}
			return res.Items
		}},
		{"watching timeline", func(t *testing.T, c pageCorpus) []notifications.NotificationDTO {
			got, err := store.HydrateWatchingRecentEvents(userCtx(c.reader), c.reader, c.cards)
			if err != nil {
				t.Fatalf("HydrateWatchingRecentEvents: %v", err)
			}
			var all []notifications.NotificationDTO
			for _, card := range c.cards {
				all = append(all, got[card]...)
			}
			return all
		}},
		{"monitor poll", func(t *testing.T, c pageCorpus) []notifications.NotificationDTO {
			res, err := store.PollForMonitor(userCtx(c.reader), pollParams(c.reader, c.project, c.scope))
			if err != nil {
				t.Fatalf("PollForMonitor: %v", err)
			}
			return res.Items
		}},
	}
	for _, s := range surfaces {
		t.Run(s.name, func(t *testing.T) {
			var items []notifications.NotificationDTO
			n1 := tracer.count(func() { items = s.run(t, one) })
			redacted(t, s.name, items, 1)
			n30 := tracer.count(func() { items = s.run(t, thirty) })
			redacted(t, s.name, items, 30)
			if n1 != n30 {
				t.Errorf("%s: a page of 1 row sent %d statements, a page of 30 rows %d — visibility is read per row",
					s.name, n1, n30)
			}
		})
	}
}
