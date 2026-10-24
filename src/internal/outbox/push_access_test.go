package outbox

// push_access_test.go — sk-674.18, the test delivery of row 130 (T029,
// INV-11): a push about an issue whose project the recipient lost access to
// between the write and the send does not go out (the push stub is not
// called); with access it does.
//
// Reference — INV-11 as the spec words it, written before the code, on the
// access of spaces: in a project of visibility «space» every participant of
// the space is a member "from space" (INV-7). The recipient is such a
// participant, given his access by the production switch to visibility
// «space» (spaces.Store.SetVisibility), and he loses it the ways spaces take
// it away — he leaves the space (spaces.Service.Leave), or the project turns
// back to «members» — not by a hand-deleted member row (that pre-spaces path
// is TestPushRunner_RereadsProjectAccessBeforeSending).
//
// Red if the access re-check at send time is not applied, or does not follow
// the access spaces give and take.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
)

func TestPushAccess_SpaceParticipantLosesAccessBeforeSend(t *testing.T) {
	type revoker func(t *testing.T, s *spaces.Store, svc *spaces.Service, owner spaces.Actor, project uuid.UUID, recipient uuid.UUID, recipientName, spaceShort string)
	cases := []struct {
		name   string
		revoke revoker
		sent   bool
	}{
		{"access kept — the push goes out", nil, true},
		{"left the space — no push", func(t *testing.T, _ *spaces.Store, svc *spaces.Service, _ spaces.Actor, _ uuid.UUID, recipient uuid.UUID, name, short string) {
			if err := svc.Leave(context.Background(), spaces.Caller{
				UserID: recipient, Username: name,
				Actor: spaces.Actor{UserID: recipient, Username: name, Type: "human"},
			}, short); err != nil {
				t.Fatalf("leave the space: %v", err)
			}
		}, false},
		{"project turned to members — no push", func(t *testing.T, s *spaces.Store, _ *spaces.Service, owner spaces.Actor, project uuid.UUID, _ uuid.UUID, _, _ string) {
			if err := s.SetVisibility(context.Background(), project, spaces.VisibilityMembers, owner); err != nil {
				t.Fatalf("visibility members: %v", err)
			}
		}, false},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := newTestPool(t)
			ctx := context.Background()
			key := "pacc" + string(rune('a'+i))

			owner := seedUser(t, pool)
			space := testsupport.SeedSpace(t, pool, owner)
			projectID, _ := testsupport.SeedProjectInSpace(t, pool, owner, space, "pacc")
			resetNotifications(t, pool, projectID)
			issueID := key + "-1"
			seedIssue(t, pool, projectID, owner, issueID, "Task "+key)
			eventID := seedEvent(t, pool, "issue.status_changed", "issue", issueID, projectID)
			setEventNewData(t, pool, eventID, `"closed"`)

			recipient := seedUser(t, pool)
			var recipientName, short string
			if err := pool.QueryRow(ctx, `SELECT username FROM users WHERE id = $1`, recipient).Scan(&recipientName); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, `SELECT short_id FROM spaces WHERE id = $1`, space).Scan(&short); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO space_members (space_id, user_id, role) VALUES ($1, $2, 'member')`, space, recipient); err != nil {
				t.Fatalf("make him a participant: %v", err)
			}
			t.Cleanup(func() {
				c := context.Background()
				_, _ = pool.Exec(c, `DELETE FROM project_members WHERE user_id = $1`, recipient)
				_, _ = pool.Exec(c, `DELETE FROM space_members WHERE user_id = $1`, recipient)
			})
			var ownerName string
			if err := pool.QueryRow(ctx, `SELECT username FROM users WHERE id = $1`, owner).Scan(&ownerName); err != nil {
				t.Fatal(err)
			}
			actor := spaces.Actor{UserID: owner, Username: ownerName, Type: "human"} // the project owner switches
			store := spaces.NewStore(pool)
			if err := store.SetVisibility(ctx, projectID, spaces.VisibilitySpace, actor); err != nil {
				t.Fatalf("visibility space: %v", err)
			}
			var viaSpace bool
			if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM project_members WHERE project_id = $1 AND user_id = $2 AND via_space)`,
				projectID, recipient).Scan(&viaSpace); err != nil || !viaSpace {
				t.Fatalf("fixture: the participant has no membership \"from space\" (%v, %v)", viaSpace, err)
			}

			endpoint := "https://push.example.com/" + key + "-" + uuid.NewString()[:8]
			setReasonPush(t, pool, recipient, "delegated_status", true)
			seedPushSubscription(t, pool, recipient, endpoint)
			id := insertPushNotification(t, pool, recipient, eventID, "issue.status_changed", projectID,
				pushRow{reason: "delegated_status"})

			if c.revoke != nil {
				c.revoke(t, store, spaces.NewService(pool, spaces.Options{Creation: spaces.CreationAll}),
					actor, projectID, recipient, recipientName, short)
			}

			sender := &fakePushSender{enabled: true}
			if _, err := newRunner(pool, sender).Tick(ctx); err != nil {
				t.Fatalf("Tick: %v", err)
			}
			got := callsTo(sender, endpoint)
			state, outcome, _ := pushStateOf(t, pool, id)
			if c.sent {
				if len(got) != 1 {
					t.Fatalf("sends to a participant with access = %d, want 1", len(got))
				}
				if state != "final" || outcome == nil || *outcome != outcomeSuccess {
					t.Errorf("state/outcome = %s/%v, want final/%s", state, outcome, outcomeSuccess)
				}
				return
			}
			if len(got) != 0 {
				t.Fatalf("push sent %d times to a recipient who lost access, want 0", len(got))
			}
			for _, call := range sender.calls() {
				if call.payload.IssueID == issueID {
					t.Errorf("the issue's payload was built for a revoked recipient: %+v", call.payload)
				}
			}
			if state != "final" || outcome == nil || *outcome != outcomeAccessRevoked {
				t.Errorf("state/outcome = %s/%v, want final/%s", state, outcome, outcomeAccessRevoked)
			}
		})
	}
}
