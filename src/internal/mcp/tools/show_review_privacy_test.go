package tools

// show_review_privacy_test.go — sk-303.14 T006, matrix row 22,
// agent half.
//
// # What is proven
//
// INV-2 names agent reads as a separate surface of the fourth channel,
// and that is not a formality: the agent is the ordinary way the assignee
// looks at their issue. Checked here are the detailed show (skein_show)
// and those card returns REST does not have at all — claim and close.
// Both return the card under `verbose:true`, and that is the mode INV-4
// declares as "the path to the full data": if the gate stood only on
// read, the full write mode would stay open.
//
// A write acknowledgement WITHOUT verbose (the short ack form) is the
// subject of row 120 and task T007; it is not duplicated here.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
)

// mcpReviewKeys — the three mechanics keys on the wire, written by hand.
var mcpReviewKeys = []string{"reviewRequired", "reviewedAt", "reviewedBy"}

// seedMCPReviewMark sets a full mark — flag, time, author.
func seedMCPReviewMark(t *testing.T, pool *pgxpool.Pool, issueID string, author uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
UPDATE issues
   SET review_required = true, reviewed_at = now(), reviewed_by = $2
 WHERE id = $1`, issueID, author)
	if err != nil {
		t.Fatalf("seed review mark: %v", err)
	}
}

// addProjectMember gives the user project visibility without making them
// the author of issues.
func addProjectMember(t *testing.T, pool *pgxpool.Pool, projectID, userID uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $2)
ON CONFLICT DO NOTHING
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, projectID, userID)
	if err != nil {
		t.Fatalf("add project member: %v", err)
	}
}

func assertMCPReviewAbsent(t *testing.T, surface string, card map[string]any) {
	t.Helper()
	for _, key := range mcpReviewKeys {
		if v, ok := card[key]; ok {
			t.Errorf("%s: agent read gave a non-author %q = %#v. INV-2 names "+
				"agent reads as a separate surface of the same channel, and requires "+
				"ABSENCE of the key, not an empty value", surface, key, v)
		}
	}
}

func assertMCPReviewPresent(t *testing.T, surface string, card map[string]any) {
	t.Helper()
	for _, key := range mcpReviewKeys {
		if _, ok := card[key]; !ok {
			t.Errorf("%s: author did not get %q on the agent read", surface, key)
		}
	}
	if got, ok := card["reviewRequired"].(bool); !ok || !got {
		t.Errorf("%s: reviewRequired = %#v, want true", surface, card["reviewRequired"])
	}
}

// issueCardFromShow extracts the card from a skein_show response.
func issueCardFromShow(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	card, ok := body["issue"].(map[string]any)
	if !ok {
		t.Fatalf("skein_show response carries no card: %v", body)
	}
	return card
}

// TestMCPShow_ReviewFieldsAreAuthorOnly — detailed agent show.
func TestMCPShow_ReviewFieldsAreAuthorOnly(t *testing.T) {
	pool := newTestPool(t)
	author, _ := seedUser(t, pool, "user")
	// The reader is an administrator: INV-4 requires that a role not stand
	// in for authorship, and the most privileged non-author checks that
	// better than an ordinary member.
	reader, _ := seedUser(t, pool, "admin")

	pid, prefix := seedProject(t, pool, author, "mcprev")
	addProjectMember(t, pool, pid, reader)

	issueID := prefix + "-1"
	seedIssue(t, pool, pid, author, issueID, "delegated result", "closed", 2)
	seedMCPReviewMark(t, pool, issueID, author)

	handler := handleShow(pool, issues.NewStore(pool), dependencies.NewStore(pool))
	call := func(caller uuid.UUID, role string) map[string]any {
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{"id": issueID}
		res, err := handler(withCtx(t, caller, role), req)
		if err != nil {
			t.Fatalf("skein_show: %v", err)
		}
		if res.IsError {
			t.Fatalf("skein_show returned an error: %+v", res)
		}
		return issueCardFromShow(t, extractToolJSON(t, res))
	}

	assertMCPReviewAbsent(t, "skein_show (non-author)", call(reader, "admin"))
	assertMCPReviewPresent(t, "skein_show (author)", call(author, "user"))
}

// TestMCPWriteVerbose_ReviewFieldsAreAuthorOnly — card returns from
// writes: claim and close under `verbose:true`.
//
// REST does not have these two returns, so without them the list
// "on every card return" would be incomplete exactly on the paths
// where the assignee actually acts: they claim the issue and close it.
func TestMCPWriteVerbose_ReviewFieldsAreAuthorOnly(t *testing.T) {
	pool := newTestPool(t)
	author, authorName := seedUser(t, pool, "user")
	reader, readerName := seedUser(t, pool, "admin")

	pid, prefix := seedProject(t, pool, author, "mcpwrv")
	addProjectMember(t, pool, pid, reader)

	store := issues.NewStore(pool)
	claim := handleClaim(pool, store)
	closeH := handleClose(pool, store)

	// One issue per person: both claim and close are one-shot
	// transitions, so "author" and "non-author" cannot share a row.
	// The assignee of each row becomes the one who acts: both claim
	// and close require the caller to be the issue's assignee. Field
	// privacy does not depend on assignment at all — it depends on
	// AUTHORSHIP, and that is what differs between the two rows.
	type actor struct {
		id       uuid.UUID
		role     string
		username string
		name     string
		slug     string
		isAuthor bool
	}
	for _, a := range []actor{
		{reader, "admin", readerName, "non-author", "reader", false},
		{author, "user", authorName, "author", "author", true},
	} {
		t.Run(a.name, func(t *testing.T) {
			ctx := withCtx(t, a.id, a.role)

			// Issue for CLAIM — open: claim will not take a closed one.
			claimID := prefix + "-" + a.slug + "c"
			seedIssue(t, pool, pid, author, claimID, "result", "open", 2)
			seedMCPReviewMark(t, pool, claimID, author)
			if _, err := pool.Exec(context.Background(),
				`UPDATE issues SET assignee = $2 WHERE id = $1`, claimID, a.username); err != nil {
				t.Fatalf("set assignee: %v", err)
			}

			claimReq := mcpgo.CallToolRequest{}
			claimReq.Params.Arguments = map[string]any{"id": claimID, "verbose": true}
			claimRes, err := claim(ctx, claimReq)
			if err != nil {
				t.Fatalf("skein_claim: %v", err)
			}
			if claimRes.IsError {
				t.Fatalf("skein_claim returned an error: %+v", claimRes)
			}
			claimCard := extractToolJSON(t, claimRes)

			// Issue for CLOSE — theirs, and it is ALREADY closed: the close
			// call here is a technical repeat.
			//
			// This way, not "an open issue with a mark that we will close"
			// (the form before wave A/4): transitioning to closed CLEARS the
			// mark (T009, SC-023), so the previous form would have checked
			// privacy on a card where nobody has the mark — and the claim
			// "the author gets all three fields" would have been false for a
			// reason that has nothing to do with privacy. Re-closing an
			// already-closed issue does not touch the mark, so the response
			// card carries exactly the state whose privacy is in question.
			// That state is also the real one: the product only sets the
			// mark on a closed issue.
			closeID := prefix + "-" + a.slug + "z"
			seedIssue(t, pool, pid, author, closeID, "result", "closed", 2)
			seedMCPReviewMark(t, pool, closeID, author)
			if _, err := pool.Exec(context.Background(),
				`UPDATE issues SET assignee = $2 WHERE id = $1`, closeID, a.username); err != nil {
				t.Fatalf("set assignee: %v", err)
			}

			var version int
			if err := pool.QueryRow(context.Background(),
				`SELECT version FROM issues WHERE id = $1`, closeID).Scan(&version); err != nil {
				t.Fatalf("read version: %v", err)
			}
			closeReq := mcpgo.CallToolRequest{}
			closeReq.Params.Arguments = map[string]any{
				"id": closeID, "expectedVersion": version,
				"closeOutcome": "done",
				"reason":       "done", "verbose": true,
			}
			closeRes, err := closeH(ctx, closeReq)
			if err != nil {
				t.Fatalf("skein_close: %v", err)
			}
			if closeRes.IsError {
				t.Fatalf("skein_close returned an error: %+v", closeRes)
			}
			closeCard := extractToolJSON(t, closeRes)

			// Precondition of the close card: the mark on the row SURVIVED
			// the technical repeat. Without it "the author got three fields"
			// below would be proven against a state the row does not have.
			var reviewedAtNull bool
			if err := pool.QueryRow(context.Background(),
				`SELECT reviewed_at IS NULL FROM issues WHERE id = $1`, closeID).
				Scan(&reviewedAtNull); err != nil {
				t.Fatalf("read reviewed_at: %v", err)
			}
			if reviewedAtNull {
				t.Fatalf("precondition: the technical close repeat wiped the mark — " +
					"privacy claims about the close card would be measuring an empty state")
			}

			for surface, card := range map[string]map[string]any{
				"skein_claim(verbose) — claim": claimCard,
				"skein_close(verbose) — close": closeCard,
			} {
				if a.isAuthor {
					assertMCPReviewPresent(t, surface, card)
				} else {
					assertMCPReviewAbsent(t, surface, card)
				}
			}
		})
	}
}

// TestMCPShow_EventBlockCarriesNoReviewTypes — row 22, the event block.
//
// The card is not the only thing skein_show returns about an issue: with
// `includeEvents:true` a journal rides beside it, and it does not filter
// types at all. The field gate does not close it — the FORM of the private
// row does: it sits on the author, and the block selects by the "issue"
// entity.
//
// Checked on both: on the non-author — because that is the INV-2 channel;
// on the author — because "their" row must not leak into a surface anyone
// who can see the issue sees, and identical responses prove the silence
// is held by the row form, not by the identity of the reader.
func TestMCPShow_EventBlockCarriesNoReviewTypes(t *testing.T) {
	pool := newTestPool(t)
	author, _ := seedUser(t, pool, "user")
	reader, _ := seedUser(t, pool, "admin")

	pid, prefix := seedProject(t, pool, author, "mcpreve")
	addProjectMember(t, pool, pid, reader)

	issueID := prefix + "-1"
	seedIssue(t, pool, pid, author, issueID, "delegated result", "closed", 2)
	seedMCPReviewMark(t, pool, issueID, author)

	store := issues.NewStore(pool)

	// Both author operations go through the store so the journal rows are
	// written by the production path, not inserted by hand.
	actor := issues.Actor{UserID: author, Username: "author", Role: "human"}
	if _, err := store.SetReviewRequired(context.Background(), issues.ReviewFlagInput{
		ID: issueID, Required: false, Actor: actor,
	}); err != nil {
		t.Fatalf("SetReviewRequired: %v", err)
	}

	// Precondition: journal rows exist. Counted WITHOUT the form — otherwise
	// a form return onto the issue would fail the test here without ever
	// running the event-block claims, and the mutation would hide in the
	// precondition.
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM events
 WHERE (   (entity_type = 'issue' AND entity_id = $1)
        OR (entity_type = 'user'  AND payload->>'issueId' = $1))
   AND type = ANY($2)`,
		issueID,
		[]string{issues.EventReviewMarked, issues.EventReviewFlagChanged},
	).Scan(&n); err != nil {
		t.Fatalf("count review rows: %v", err)
	}
	if n == 0 {
		t.Fatalf("precondition: no mechanics journal rows — the claim below " +
			"would be proven by missing data, not by the row form")
	}

	handler := handleShow(pool, store, dependencies.NewStore(pool))
	call := func(caller uuid.UUID, role string) []any {
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{"id": issueID, "includeEvents": true}
		res, err := handler(withCtx(t, caller, role), req)
		if err != nil {
			t.Fatalf("skein_show: %v", err)
		}
		if res.IsError {
			t.Fatalf("skein_show returned an error: %+v", res)
		}
		body := extractToolJSON(t, res)
		raw, _ := body["events"].([]any)
		return raw
	}

	private := map[string]bool{
		issues.EventReviewMarked:      true,
		issues.EventReviewFlagChanged: true,
	}
	for surface, caller := range map[string]struct {
		id   uuid.UUID
		role string
	}{
		"non-author": {reader, "admin"},
		"author":     {author, "user"},
	} {
		for _, item := range call(caller.id, caller.role) {
			ev, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if typ, _ := ev["type"].(string); private[typ] {
				t.Errorf("%s: skein_show event block carries private type %q. "+
					"The card field gate does not close it — the row form must "+
					"(INV-2, SC-028)", surface, typ)
			}
		}
	}
}
