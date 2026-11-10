package issues

// source_api_test.go — test matrix row 17 (INV-4, INV-5, SC-014). Reference
// — the spec, written before the code:
//
//   - a record written with an API key and no kind gets api, never human;
//   - api is treated as agent_directed by every origin-dependent rule;
//   - the human-driven ("people") group includes api;
//   - kind directed / autonomous give the same values as in MCP;
//   - an edit of an issue or a comment keeps its original source /
//     author_kind.
//
// One table row per branch of the T008 inventory that lives in this package.
// The actor_type of the events, linked_by_kind and added_by_kind come from
// the actor's role, not from the source: a write by a key has the agent
// role, checked here on the event of the create.

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/auth"
)

func keyContext() context.Context {
	return auth.WithAPIKeyID(context.Background(), uuid.New())
}

func TestSourceAPI_PureRules(t *testing.T) {
	t.Run("RuleSource treats api as agent_directed", func(t *testing.T) {
		for src, want := range map[string]string{
			SourceAPI:             SourceAgentDirected,
			SourceHuman:           SourceHuman,
			SourceAgentDirected:   SourceAgentDirected,
			SourceAgentAutonomous: SourceAgentAutonomous,
		} {
			if got := RuleSource(src); got != want {
				t.Errorf("RuleSource(%q) = %q, want %q", src, got, want)
			}
		}
	})

	t.Run("creator auto-watch follows agent_directed for api", func(t *testing.T) {
		if !creatorAutoWatches(SourceAPI, false) {
			t.Error("top-level api issue: no creator watch, want one (as agent_directed)")
		}
		if creatorAutoWatches(SourceAPI, true) {
			t.Error("api child issue: creator watch, want none (as agent_directed)")
		}
	})

	t.Run("the human-driven group includes api, agent-autonomous does not", func(t *testing.T) {
		human, _ := OriginGroupsToSources([]string{OriginHumanDriven})
		if !slices.Contains(human, SourceAPI) || !slices.Contains(human, SourceHuman) || !slices.Contains(human, SourceAgentDirected) {
			t.Errorf("human-driven = %v, want human, agent_directed and api", human)
		}
		auto, _ := OriginGroupsToSources([]string{OriginAgentAutonomous})
		if slices.Contains(auto, SourceAPI) {
			t.Errorf("agent-autonomous = %v, want no api", auto)
		}
	})

	t.Run("the exact filter accepts api", func(t *testing.T) {
		if got, err := ValidateSource("api"); err != nil || got != SourceAPI {
			t.Errorf("ValidateSource(api) = %q, %v, want api", got, err)
		}
	})

	t.Run("kind of a write by key", func(t *testing.T) {
		cases := []struct{ kind, source, authorKind string }{
			{"", SourceAPI, CommentAuthorKindAPI},
			{"directed", SourceAgentDirected, CommentAuthorKindAgentDirected},
			{"autonomous", SourceAgentAutonomous, CommentAuthorKindAgentAutonomous},
		}
		for _, c := range cases {
			if got, err := SourceForKeyWrite(c.kind); err != nil || got != c.source {
				t.Errorf("SourceForKeyWrite(%q) = %q, %v, want %q", c.kind, got, err, c.source)
			}
			if got, err := CommentAuthorKindForKeyWrite(c.kind); err != nil || got != c.authorKind {
				t.Errorf("CommentAuthorKindForKeyWrite(%q) = %q, %v, want %q", c.kind, got, err, c.authorKind)
			}
		}
		var ve *ValidationError
		if _, err := SourceForKeyWrite("human"); !errors.As(err, &ve) {
			t.Errorf("SourceForKeyWrite(human) err %v, want a validation error", err)
		}
	})
}

func TestSourceAPI_StoreWrites(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	ownerName := readUsername(t, pool, owner)
	pid, prefix := seedProject(t, pool, owner)
	store := NewStore(pool)
	agent := agentActor(owner, ownerName)
	keyCtx := keyContext()

	create := func(ctx context.Context, title, source string, parent *string) (*IssueDetail, error) {
		return store.CreateIssue(ctx, pid, prefix, "counter",
			CreateIssueInput{Title: title, Type: "task", Priority: 2, Source: source, ParentID: parent}, owner, agent)
	}

	var top *IssueDetail
	t.Run("a write by key without a source gets api, and the creator watches it as agent_directed", func(t *testing.T) {
		var err error
		top, err = create(keyCtx, "by key", "", nil)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if top.Source != SourceAPI {
			t.Errorf("source %q, want api", top.Source)
		}
		if !hasReason(watcherReasons(t, pool, top.ID, owner), "creator") {
			t.Error("top-level api issue: owner has no creator subscription, want one")
		}
		var actorType string
		if err := pool.QueryRow(context.Background(), `
SELECT actor_type FROM events WHERE entity_id = $1 AND type = 'issue.created'`, top.ID).Scan(&actorType); err != nil {
			t.Fatalf("read the create event: %v", err)
		}
		if actorType != "agent" {
			t.Errorf("actor_type %q, want agent", actorType)
		}
	})

	t.Run("an api child does not subscribe the creator", func(t *testing.T) {
		if top == nil {
			t.Skip("needs the top-level issue")
		}
		child, err := create(keyCtx, "api child", SourceAPI, &top.ID)
		if err != nil {
			t.Fatalf("create child: %v", err)
		}
		if n := countWatcherRows(t, pool, child.ID, owner); n != 0 {
			t.Errorf("api child: %d watcher rows for the owner, want 0", n)
		}
	})

	t.Run("a write by key never gets human", func(t *testing.T) {
		var ve *ValidationError
		if _, err := create(keyCtx, "claims human", SourceHuman, nil); !errors.As(err, &ve) || ve.Code != "invalid_source" {
			t.Errorf("create by key with source human: err %v, want invalid_source", err)
		}
		if _, _, err := store.CreateEpicWithChildren(keyCtx, pid, prefix, "counter",
			CreateIssueInput{Title: "epic", Type: TypeEpic, Priority: 2}, nil, SourceHuman, owner, agent); !errors.As(err, &ve) || ve.Code != "invalid_source" {
			t.Errorf("epic by key with source human: err %v, want invalid_source", err)
		}
		// Review C2-02: a child with its own source human is refused too,
		// and nothing of the tree is created.
		epicTitle := "epic with a human child " + uuid.NewString()[:8]
		if _, _, err := store.CreateEpicWithChildren(keyCtx, pid, prefix, "counter",
			CreateIssueInput{Title: epicTitle, Type: TypeEpic, Priority: 2},
			[]CreateIssueInput{{Title: "human child", Type: "task", Priority: 2, Source: SourceHuman}},
			"", owner, agent); !errors.As(err, &ve) || ve.Code != "invalid_source" {
			t.Errorf("epic by key with a child of source human: err %v, want invalid_source", err)
		}
		var n int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM issues WHERE project_id = $1 AND title = $2`, pid, epicTitle).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("refused epic left %d rows, want 0", n)
		}
		if _, err := store.AddComment(keyCtx, top.ID, "claims human", owner, ownerName,
			CommentAuthorTypeAgent, CommentAuthorKindHuman, agent); !errors.As(err, &ve) || ve.Code != "invalid_author_kind" {
			t.Errorf("comment by key with author_kind human: err %v, want invalid_author_kind", err)
		}
	})

	t.Run("a write without a key and without a source stays human", func(t *testing.T) {
		human, err := create(context.Background(), "by a person", "", nil)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if human.Source != SourceHuman {
			t.Errorf("source %q, want human", human.Source)
		}
	})

	t.Run("an edit keeps the source and the author kind", func(t *testing.T) {
		if top == nil {
			t.Skip("needs the top-level issue")
		}
		cur, err := store.GetIssue(context.Background(), top.ID)
		if err != nil {
			t.Fatalf("GetIssue: %v", err)
		}
		title := "renamed by a person"
		patched, err := store.PatchIssue(context.Background(), top.ID,
			PatchIssueInput{ExpectedVersion: cur.Version, Title: &title}, HumanActor(owner, ownerName))
		if err != nil {
			t.Fatalf("PatchIssue: %v", err)
		}
		if patched.Source != SourceAPI {
			t.Errorf("source after an edit %q, want api", patched.Source)
		}
		c, err := store.AddComment(keyCtx, top.ID, "note by key", owner, ownerName, CommentAuthorTypeAgent, CommentAuthorKindAPI, agent)
		if err != nil {
			t.Fatalf("AddComment api: %v", err)
		}
		edited, err := store.UpdateComment(context.Background(), c.ID, "edited by a person", c.Version, HumanActor(owner, ownerName))
		if err != nil {
			t.Fatalf("UpdateComment: %v", err)
		}
		if edited.AuthorKind != CommentAuthorKindAPI {
			t.Errorf("author_kind after an edit %q, want api", edited.AuthorKind)
		}
	})

	t.Run("the human-driven comment filter includes api", func(t *testing.T) {
		if top == nil {
			t.Skip("needs the top-level issue")
		}
		human, _, err := store.ListComments(context.Background(), top.ID, CommentOriginHumanDriven, ListCommentsOpts{})
		if err != nil {
			t.Fatalf("ListComments human-driven: %v", err)
		}
		auto, _, err := store.ListComments(context.Background(), top.ID, CommentOriginAgentAutonomous, ListCommentsOpts{})
		if err != nil {
			t.Fatalf("ListComments agent-autonomous: %v", err)
		}
		hasAPI := func(cs []CommentDTO) bool {
			return slices.ContainsFunc(cs, func(c CommentDTO) bool { return c.AuthorKind == CommentAuthorKindAPI })
		}
		if !hasAPI(human) {
			t.Error("human-driven comments: no api comment, want it")
		}
		if hasAPI(auto) {
			t.Error("agent-autonomous comments: an api comment, want none")
		}
	})
}
