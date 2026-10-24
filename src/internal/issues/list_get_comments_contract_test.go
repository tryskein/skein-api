package issues

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sk-242.1 T061 — store contract for ListComments opts/total and GetComment.
// Covers matrix #11 (store tombstone), #17 (since exclusive + total
// before limit), #21–22 (zero opts full list / signature), #31 (trailing
// tombstones in last-N), #34 (list tombstone blank body).

func forceCommentCreatedAt(t *testing.T, pool *pgxpool.Pool, commentID uuid.UUID, at time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`UPDATE comments SET created_at = $2, updated_at = $2 WHERE id = $1`,
		commentID, at.UTC())
	if err != nil {
		t.Fatalf("force created_at: %v", err)
	}
}

func seedCommentThread(t *testing.T, store *Store, pool *pgxpool.Pool, issueID string, author uuid.UUID, bodies []string, base time.Time) []uuid.UUID {
	t.Helper()
	actor := HumanActor(author, "tester")
	ids := make([]uuid.UUID, 0, len(bodies))
	for i, body := range bodies {
		c, err := store.AddComment(context.Background(), issueID, body, author,
			"tester", CommentAuthorTypeHuman, CommentAuthorKindHuman, actor)
		if err != nil {
			t.Fatalf("AddComment[%d]: %v", i, err)
		}
		// Spaced timestamps so since/last-N assertions are deterministic.
		ts := base.Add(time.Duration(i) * time.Second)
		forceCommentCreatedAt(t, pool, c.ID, ts)
		ids = append(ids, c.ID)
	}
	return ids
}

// TestListComments_ZeroOptsFullThread — US-6 / matrix #21–22: zero opts
// returns the entire chronological population and total == len(items).
func TestListComments_ZeroOptsFullThread(t *testing.T) {
	pool := newTestPool(t)
	author := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, author)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, author, issueID, "zero opts full")

	store := NewStore(pool)
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	ids := seedCommentThread(t, store, pool, issueID, author,
		[]string{"c1", "c2", "c3", "c4"}, base)

	items, total, err := store.ListComments(context.Background(), issueID, CommentOriginAll, ListCommentsOpts{})
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if total != 4 {
		t.Errorf("total = %d, want 4", total)
	}
	if len(items) != 4 {
		t.Fatalf("len(items) = %d, want 4", len(items))
	}
	for i, id := range ids {
		if items[i].ID != id {
			t.Errorf("items[%d].ID = %s, want %s", i, items[i].ID, id)
		}
		if items[i].Body != []string{"c1", "c2", "c3", "c4"}[i] {
			t.Errorf("items[%d].Body = %q", i, items[i].Body)
		}
	}
}

// TestListComments_LastN_ASCWithIDTiebreak — US-5: Limit N returns last N
// ASC; equal created_at ordered by id ASC.
func TestListComments_LastN_ASCWithIDTiebreak(t *testing.T) {
	pool := newTestPool(t)
	author := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, author)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, author, issueID, "last-n")

	store := NewStore(pool)
	base := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	ids := seedCommentThread(t, store, pool, issueID, author,
		[]string{"t1", "t2", "t3", "t4"}, base)

	// last-3 of 4 → t2,t3,t4 in ASC order.
	items, total, err := store.ListComments(context.Background(), issueID, CommentOriginAll, ListCommentsOpts{Limit: 3})
	if err != nil {
		t.Fatalf("ListComments limit=3: %v", err)
	}
	if total != 4 {
		t.Errorf("total = %d, want 4 (before limit)", total)
	}
	if len(items) != 3 {
		t.Fatalf("len(items) = %d, want 3", len(items))
	}
	wantIDs := []uuid.UUID{ids[1], ids[2], ids[3]}
	for i, id := range wantIDs {
		if items[i].ID != id {
			t.Errorf("items[%d].ID = %s, want %s (body %q)", i, items[i].ID, id, items[i].Body)
		}
	}
	// ASC time order.
	for i := 1; i < len(items); i++ {
		if items[i].CreatedAt.Before(items[i-1].CreatedAt) {
			t.Errorf("items not ASC: [%d]=%v before [%d]=%v", i, items[i].CreatedAt, i-1, items[i-1].CreatedAt)
		}
	}

	// Same created_at: order by id ASC within the last-N pick.
	same := base.Add(10 * time.Second)
	for _, id := range ids {
		forceCommentCreatedAt(t, pool, id, same)
	}
	items, total, err = store.ListComments(context.Background(), issueID, CommentOriginAll, ListCommentsOpts{Limit: 2})
	if err != nil {
		t.Fatalf("ListComments same-ts: %v", err)
	}
	if total != 4 {
		t.Errorf("same-ts total = %d, want 4", total)
	}
	if len(items) != 2 {
		t.Fatalf("same-ts len = %d, want 2", len(items))
	}
	// Last 2 by (created_at DESC, id DESC) then re-ASC → the two largest ids, ASC.
	// With equal timestamps, id DESC pick means max ids; outer ASC means smaller first.
	// Sort all ids and take last 2 by id.
	sorted := append([]uuid.UUID(nil), ids...)
	// UUID lexical order matches Postgres uuid comparison for v4 randoms.
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].String() < sorted[i].String() {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	expect := sorted[len(sorted)-2:]
	if items[0].ID != expect[0] || items[1].ID != expect[1] {
		t.Errorf("same-ts last-2 ASC by id = [%s, %s], want [%s, %s]",
			items[0].ID, items[1].ID, expect[0], expect[1])
	}
	if items[0].ID.String() > items[1].ID.String() {
		t.Errorf("same-ts items not id-ASC: %s > %s", items[0].ID, items[1].ID)
	}
}

// TestListComments_SinceExclusiveAndTotalBeforeLimit — US-5/FR-008 #17:
// population is strict createdAt > since; total after filter before limit.
// Example T1<T2<T3<T4, since=T2 → {T3,T4}, limit 3 → both, total=2.
func TestListComments_SinceExclusiveAndTotalBeforeLimit(t *testing.T) {
	pool := newTestPool(t)
	author := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, author)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, author, issueID, "since exclusive")

	store := NewStore(pool)
	base := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	ids := seedCommentThread(t, store, pool, issueID, author,
		[]string{"T1", "T2", "T3", "T4"}, base)

	// since = T2's timestamp → exclude T1 and T2 (exclusive).
	since := base.Add(1 * time.Second) // T2
	items, total, err := store.ListComments(context.Background(), issueID, CommentOriginAll, ListCommentsOpts{
		Limit: 3,
		Since: &since,
	})
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2 (T3,T4 after exclusive since)", total)
	}
	if len(items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(items))
	}
	if items[0].ID != ids[2] || items[1].ID != ids[3] {
		t.Errorf("items = [%s, %s], want [T3=%s, T4=%s]",
			items[0].ID, items[1].ID, ids[2], ids[3])
	}
	// Equal-timestamp residual: a row with created_at == since is excluded.
	for _, it := range items {
		if !it.CreatedAt.After(since) {
			t.Errorf("item created_at %v not strictly after since %v", it.CreatedAt, since)
		}
	}
}

// TestListComments_TombstonesInPopulation — #31/#34: soft-deleted rows
// count in total, appear in last-N, body blanked, mentions non-nil empty.
func TestListComments_TombstonesInPopulation(t *testing.T) {
	pool := newTestPool(t)
	author := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, author)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, author, issueID, "tombstones list")

	store := NewStore(pool)
	actor := HumanActor(author, "tester")
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	ids := seedCommentThread(t, store, pool, issueID, author,
		[]string{"live-a", "live-b", "will-die", "live-c"}, base)

	// Soft-delete the third comment (trailing-ish for last-3).
	if err := store.DeleteComment(context.Background(), ids[2], "cleanup", actor); err != nil {
		t.Fatalf("DeleteComment: %v", err)
	}

	// Full list: tombstone present, body blank.
	items, total, err := store.ListComments(context.Background(), issueID, CommentOriginAll, ListCommentsOpts{})
	if err != nil {
		t.Fatalf("ListComments full: %v", err)
	}
	if total != 4 {
		t.Errorf("total = %d, want 4 (tombstones counted)", total)
	}
	if len(items) != 4 {
		t.Fatalf("len = %d, want 4", len(items))
	}
	tomb := items[2]
	if tomb.DeletedAt == nil {
		t.Fatal("expected DeletedAt set on tombstone row")
	}
	if tomb.Body != "" {
		t.Errorf("tombstone body = %q, want empty", tomb.Body)
	}
	if tomb.Mentions == nil {
		t.Error("tombstone Mentions must be non-nil empty slice")
	}
	if len(tomb.Mentions) != 0 {
		t.Errorf("tombstone Mentions len = %d, want 0", len(tomb.Mentions))
	}

	// last-3 includes trailing tombstone population (ids[1], ids[2] deleted, ids[3]).
	items, total, err = store.ListComments(context.Background(), issueID, CommentOriginAll, ListCommentsOpts{Limit: 3})
	if err != nil {
		t.Fatalf("ListComments last-3: %v", err)
	}
	if total != 4 {
		t.Errorf("last-3 total = %d, want 4", total)
	}
	if len(items) != 3 {
		t.Fatalf("last-3 len = %d, want 3", len(items))
	}
	// Expect last three ids in ASC.
	for i, id := range []uuid.UUID{ids[1], ids[2], ids[3]} {
		if items[i].ID != id {
			t.Errorf("last-3 items[%d] = %s, want %s", i, items[i].ID, id)
		}
	}
	// Middle of last-3 is the tombstone with blank body.
	if items[1].Body != "" || items[1].DeletedAt == nil {
		t.Errorf("trailing tombstone in last-N: body=%q deletedAt=%v", items[1].Body, items[1].DeletedAt)
	}
}

// TestGetComment_LiveTombstoneMiss — US-4 store #11: live full DTO;
// tombstone blank; missing → ErrCommentNotFound; mentions non-nil.
func TestGetComment_LiveTombstoneMiss(t *testing.T) {
	pool := newTestPool(t)
	author := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, author)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, author, issueID, "get comment")

	store := NewStore(pool)
	actor := HumanActor(author, "tester")

	live, err := store.AddComment(context.Background(), issueID, "hello world", author,
		"tester", CommentAuthorTypeHuman, CommentAuthorKindHuman, actor)
	if err != nil {
		t.Fatalf("AddComment live: %v", err)
	}
	dead, err := store.AddComment(context.Background(), issueID, "secret deleted text", author,
		"tester", CommentAuthorTypeHuman, CommentAuthorKindHuman, actor)
	if err != nil {
		t.Fatalf("AddComment dead: %v", err)
	}
	if err := store.DeleteComment(context.Background(), dead.ID, "gone", actor); err != nil {
		t.Fatalf("DeleteComment: %v", err)
	}

	// Live
	got, gotPID, err := store.GetComment(context.Background(), live.ID)
	if err != nil {
		t.Fatalf("GetComment live: %v", err)
	}
	if gotPID != pid {
		t.Errorf("projectID = %s, want %s", gotPID, pid)
	}
	if got.Body != "hello world" {
		t.Errorf("live body = %q", got.Body)
	}
	if got.IssueID != issueID {
		t.Errorf("issueId = %q, want %q", got.IssueID, issueID)
	}
	if got.Mentions == nil {
		t.Error("live Mentions must be non-nil (empty ok)")
	}
	if got.DeletedAt != nil {
		t.Error("live DeletedAt must be nil")
	}

	// Tombstone
	got, _, err = store.GetComment(context.Background(), dead.ID)
	if err != nil {
		t.Fatalf("GetComment tombstone: %v", err)
	}
	if got.Body != "" {
		t.Errorf("tombstone body = %q, want empty", got.Body)
	}
	if got.DeletedAt == nil {
		t.Error("tombstone DeletedAt must be set")
	}
	if got.Mentions == nil || len(got.Mentions) != 0 {
		t.Errorf("tombstone Mentions = %#v, want non-nil empty", got.Mentions)
	}

	// Miss
	_, _, err = store.GetComment(context.Background(), uuid.Must(uuid.NewRandom()))
	if !errors.Is(err, ErrCommentNotFound) {
		t.Errorf("miss err = %v, want ErrCommentNotFound", err)
	}
}
