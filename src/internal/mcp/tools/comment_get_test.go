package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/issues"
)

// T062 / US-4 matrix #10–14, #30 — skein_comment_get contracts.

func TestCommentGet_InvalidUUID(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	store := issues.NewStore(pool)
	handler := handleCommentGet(pool, store)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"commentId": "not-a-uuid"}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected isError for invalid UUID")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", got, ErrCodeValidationError)
	}
}

func TestCommentGet_Missing(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	store := issues.NewStore(pool)
	handler := handleCommentGet(pool, store)
	ctx := withCtx(t, userID, "admin")

	missing := uuid.Must(uuid.NewRandom())
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"commentId": missing.String()}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected isError for missing comment")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeCommentNotFound {
		t.Fatalf("error = %v, want %s", got, ErrCodeCommentNotFound)
	}
}

func TestCommentGet_LiveFullDTO(t *testing.T) {
	pool := newTestPool(t)
	userID, uname := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "cgget")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "get live", "open", 2)

	store := issues.NewStore(pool)
	// Mention a project member so mentions hydrate non-empty.
	bodyText := "hello @" + uname + " please review"
	cmt, err := store.AddComment(context.Background(), issueID, bodyText, userID,
		uname, issues.CommentAuthorTypeHuman, issues.CommentAuthorKindHuman,
		issues.HumanActor(userID, uname))
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}

	handler := handleCommentGet(pool, store)
	ctx := withCtx(t, userID, "admin")
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"commentId": cmt.ID.String()}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError: %+v", extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	c, ok := body["comment"].(map[string]any)
	if !ok {
		t.Fatalf("comment missing: %+v", body)
	}
	for _, key := range []string{
		"id", "issueId", "author", "authorType", "authorKind",
		"body", "createdAt", "updatedAt", "version", "mentions",
	} {
		if _, ok := c[key]; !ok {
			t.Errorf("comment missing key %q", key)
		}
	}
	if c["body"] != bodyText {
		t.Errorf("body = %v, want %q", c["body"], bodyText)
	}
	if c["issueId"] != issueID {
		t.Errorf("issueId = %v, want %s", c["issueId"], issueID)
	}
	mentions, ok := c["mentions"].([]any)
	if !ok {
		t.Fatalf("mentions type %T, want array", c["mentions"])
	}
	if mentions == nil {
		t.Fatal("mentions must be non-nil array")
	}
	if len(mentions) < 1 {
		t.Errorf("expected ≥1 hydrated mention for @%s, got %d", uname, len(mentions))
	}
}

func TestCommentGet_Tombstone(t *testing.T) {
	pool := newTestPool(t)
	userID, uname := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "cgtomb")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "tombstone", "open", 2)

	store := issues.NewStore(pool)
	cmt, err := store.AddComment(context.Background(), issueID, "will delete secret", userID,
		uname, issues.CommentAuthorTypeHuman, issues.CommentAuthorKindHuman,
		issues.HumanActor(userID, uname))
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if err := store.DeleteComment(context.Background(), cmt.ID, "test", issues.HumanActor(userID, uname)); err != nil {
		t.Fatalf("DeleteComment: %v", err)
	}

	handler := handleCommentGet(pool, store)
	ctx := withCtx(t, userID, "admin")
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"commentId": cmt.ID.String()}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError: %+v", extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	c := body["comment"].(map[string]any)
	if c["body"] != "" {
		t.Errorf("tombstone body = %q, want \"\"", c["body"])
	}
	mentions, ok := c["mentions"].([]any)
	if !ok {
		t.Fatalf("mentions type %T", c["mentions"])
	}
	if len(mentions) != 0 {
		t.Errorf("tombstone mentions = %v, want []", mentions)
	}
	if c["deletedAt"] == nil {
		t.Error("deletedAt should be present on tombstone")
	}
}

func TestCommentGet_InvisibleProject(t *testing.T) {
	pool := newTestPool(t)
	owner, ownerName := seedUser(t, pool, "user")
	stranger, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, owner, "cginv")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, owner, issueID, "secret", "open", 2)

	store := issues.NewStore(pool)
	cmt, err := store.AddComment(context.Background(), issueID, "hidden", owner,
		ownerName, issues.CommentAuthorTypeHuman, issues.CommentAuthorKindHuman,
		issues.HumanActor(owner, ownerName))
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}

	handler := handleCommentGet(pool, store)
	ctx := withCtx(t, stranger, "user")
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"commentId": cmt.ID.String()}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected isError for invisible project")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeCommentNotFound {
		t.Fatalf("error = %v, want %s (anti-oracle)", got, ErrCodeCommentNotFound)
	}
}

// TestCommentGet_SchemaRegistered — US-7 matrix #35: registration + commentId required path.
func TestCommentGet_SchemaRegistered(t *testing.T) {
	pool := newTestPool(t)
	store := issues.NewStore(pool)
	s := server.NewMCPServer("skein-test", "0.1.0", server.WithToolCapabilities(false))
	RegisterCommentGet(s, pool, store)

	handler := handleCommentGet(pool, store)
	res, err := handler(context.Background(), mcpgo.CallToolRequest{})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected unauthenticated call to error")
	}
	_ = s
}

// ---------------------------------------------------------------------
// sk-282 T035 — acceptance of the surface US-4 leans on (test-plan rows
// 43 and 44, FP-6).
//
// US-4 moves «read the rest of the thread» onto address-by-id reading, so
// this tool stops being secondary: an index entry is only worth anything
// because the id on it can be turned back into a full comment. That
// cannot be accepted through show.go's tests — it is a different surface
// with its own authorisation, its own delete race and its own
// anti-oracle contract.
//
// Row 43 is the one thing that had to be ADDED: the existing live-comment
// test used a short body, so it could not tell «returns the body» from
// «returns the first 80 characters». Row 44 is an explicit binding of the
// three anti-oracle tests above (Missing / InvisibleProject / Tombstone)
// to FP-6; the contract there is declared Unchanged and the tests already
// state it, so nothing is re-written — but the binding is asserted here
// rather than assumed, because «unchanged» is a claim that has to keep
// being true after T011 redefined what the thread read returns.
// ---------------------------------------------------------------------

// TestCommentGet_LongBodyIsNotTruncated is row 43.
//
// The index cuts a preview at ThreadIndexPreviewMaxRunes. Address-by-id is
// the INV-4 exit from that cut, so applying the same cut here would close
// the exit and leave the caller with no route to the text at all.
func TestCommentGet_LongBodyIsNotTruncated(t *testing.T) {
	pool := newTestPool(t)
	userID, uname := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "cglong")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "get long", "open", 2)

	store := issues.NewStore(pool)
	// Deliberately many times the preview length, and multi-byte, so a
	// rune-counting cut and a byte-counting one would both be visible.
	// TrimSpace because the store trims a comment body on write; a fixture
	// ending in whitespace would come back one rune short and read as a
	// truncation that never happened.
	longBody := strings.TrimSpace(strings.Repeat("long answer. ", 400))
	if len([]rune(longBody)) <= ThreadIndexPreviewMaxRunes {
		t.Fatalf("fixture body is %d runes, which is not longer than the preview cut of %d — "+
			"the test would prove nothing", len([]rune(longBody)), ThreadIndexPreviewMaxRunes)
	}
	cmt, err := store.AddComment(context.Background(), issueID, longBody, userID,
		uname, issues.CommentAuthorTypeHuman, issues.CommentAuthorKindHuman,
		issues.HumanActor(userID, uname))
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}

	handler := handleCommentGet(pool, store)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"commentId": cmt.ID.String()}
	res, err := handler(withCtx(t, userID, "admin"), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError: %+v", extractToolJSON(t, res))
	}
	c := extractToolJSON(t, res)["comment"].(map[string]any)

	got, _ := c["body"].(string)
	if got != longBody {
		t.Errorf("address-by-id returned %d runes of a %d-rune comment — the index's cut must "+
			"NOT extend to this surface: it is the named exit from that cut, and truncating "+
			"here would leave no route to the text at all",
			len([]rune(got)), len([]rune(longBody)))
	}
	// And no truncation marker leaked over from the index contract: there
	// is nothing to mark, and a marker here would say the full body is
	// partial.
	for _, key := range []string{"previewTruncated", "truncated", "bodyTruncated"} {
		if v, ok := c[key]; ok {
			t.Errorf("address-by-id response carries %q = %v — this surface returns the whole "+
				"body, so a truncation marker on it can only lie", key, v)
		}
	}
}

// TestCommentGet_AntiOracleContractStillHolds is row 44's binding.
//
// The row is marked «already covered»: the contract is Unchanged and the
// three tests above state it. What this adds is the ASSERTION that they
// still describe one behaviour — a missing comment and a comment in an
// invisible project must remain INDISTINGUISHABLE, because the moment
// they differ, the tool tells a caller which ids exist in projects it
// cannot see.
//
// It is worth a test of its own precisely because the two cases are
// asserted in separate functions today: each could drift on its own and
// both would stay green.
func TestCommentGet_AntiOracleContractStillHolds(t *testing.T) {
	pool := newTestPool(t)
	handler := handleCommentGet(pool, issues.NewStore(pool))

	// A comment that does not exist at all.
	caller, _ := seedUser(t, pool, "user")
	missingReq := mcpgo.CallToolRequest{}
	missingReq.Params.Arguments = map[string]any{"commentId": uuid.Must(uuid.NewRandom()).String()}
	missingRes, err := handler(withCtx(t, caller, "user"), missingReq)
	if err != nil {
		t.Fatalf("handler error (missing): %v", err)
	}

	// A comment that exists, in a project this caller is not a member of.
	owner, ouname := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, owner, "cgoracle")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, owner, issueID, "hidden", "open", 2)
	hidden, err := issues.NewStore(pool).AddComment(context.Background(), issueID, "secret",
		owner, ouname, issues.CommentAuthorTypeHuman, issues.CommentAuthorKindHuman,
		issues.HumanActor(owner, ouname))
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	invisibleReq := mcpgo.CallToolRequest{}
	invisibleReq.Params.Arguments = map[string]any{"commentId": hidden.ID.String()}
	invisibleRes, err := handler(withCtx(t, caller, "user"), invisibleReq)
	if err != nil {
		t.Fatalf("handler error (invisible): %v", err)
	}

	if !missingRes.IsError || !invisibleRes.IsError {
		t.Fatalf("one of the two calls succeeded (missing isError=%v, invisible isError=%v)",
			missingRes.IsError, invisibleRes.IsError)
	}
	missingBody := extractToolJSON(t, missingRes)
	invisibleBody := extractToolJSON(t, invisibleRes)

	if missingBody["error"] != invisibleBody["error"] {
		t.Errorf("a non-existent comment answers %v and an invisible one answers %v — the two "+
			"must stay indistinguishable, or the tool becomes an oracle for which ids exist "+
			"in projects the caller cannot see",
			missingBody["error"], invisibleBody["error"])
	}
	if missingBody["message"] != invisibleBody["message"] {
		t.Errorf("the two anti-oracle answers differ in message: %q vs %q",
			missingBody["message"], invisibleBody["message"])
	}
	// The secret must not be in either answer.
	for _, body := range []map[string]any{missingBody, invisibleBody} {
		for k, v := range body {
			if s, ok := v.(string); ok && strings.Contains(s, "secret") {
				t.Errorf("the anti-oracle answer leaks the comment body through %q: %q", k, s)
			}
		}
	}
}
