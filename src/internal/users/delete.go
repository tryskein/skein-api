package users

// delete.go — deleting an account (sk-674.18 T041, US-10, D-8, INV-18,
// INV-26, SC-011, E-6, E-22; the operator's deletion of T042 is the same
// operation).
//
// The account is not deleted but anonymized (D-8: a dozen foreign keys are
// RESTRICT, and the history must stay). One SERIALIZABLE transaction,
// retried like every membership primitive (spaces.WithRetry), does all of
// it or nothing:
//
//  1. Locks, in the order every space operation takes them (K-7): the rows
//     of every space the account is a participant of and of every space
//     with a pending invitation to its email or from it, sorted by id;
//     then the account's row. The participations are read again under
//     these locks.
//  2. Refusals, in this order: the confirming password does not match the
//     hash it was verified against before the transaction
//     (invalid_credentials); the account owns a regular space with other
//     participants (owns_spaces_with_members, naming the spaces); it owns
//     the common space (owns_common_space — an admin must change the owner
//     first); it is the last active admin (last_admin_cannot_demote, the
//     deactivation's own refusal). Nothing is written on a refusal.
//  3. DeactivateInTx (K-16, INV-26): deactivated_at, a raised session
//     epoch and every API key revoked — after the commit no session and no
//     key of the account works, and skein-web drops its tickets and hub
//     groups on the user.deactivated event.
//  4. The spaces: the personal one and every regular space it owns alone
//     are deleted with everything in them (spaces.DeleteInTx, K-22); every
//     other space is left as by US-7 (spaces.LeaveInTx, reason
//     account_deleted). Then INV-12 for whatever is left anywhere — the
//     memberships in projects of spaces it is not in (an external member),
//     the open issues it is assignee of, its claims, watches and reminders
//     (authz.RevokeProjectAccessSeen: a deactivated account sees nothing,
//     so every such project is lost).
//  5. The invitations: the pending ones to its email or sent by it are
//     revoked as the owner's revocation does it — tokens retired, unsent
//     emails cancelled (E-6), the addressees, the owners and the senders
//     signalled (invitations.RevokePendingInTx) — an
//     invitation from a deleted person is not to be accepted; its
//     email in the invitations is replaced by the pseudonym; its rows of
//     the send log go (they count only towards its own daily limit).
//  6. Its personal state goes: tokens, every email of it or to its address, push
//     subscriptions, agent sessions, monitor credentials, preferences,
//     notifications, watches, reminders, its themes.
//  7. Anonymization (anonymize.go, K-21) and the reserved hashes of its
//     email and username (reserved_identities): neither is registered or
//     assigned again (INV-18).
//
// Signals and access_changed of the steps publish only after the commit
// (signals.BeginTxFunc); a refusal or a rollback publishes nothing.

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/assignee"
	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/invitations"
	"skein-api/internal/spaces"
)

// accountSpace is a space the account takes part in, as deletion sees it.
type accountSpace struct {
	id       uuid.UUID
	shortID  string
	name     string
	kind     string
	owned    bool
	members  int
	projects int
}

// accountSpaces reads the spaces userID is a participant of.
func accountSpaces(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, userID uuid.UUID) ([]accountSpace, error) {
	rows, err := q.Query(ctx, `
SELECT s.id, s.short_id, s.name, s.kind, s.owner_id = $1,
       (SELECT count(*) FROM space_members m2 WHERE m2.space_id = s.id),
       (SELECT count(*) FROM projects p WHERE p.space_id = s.id)
  FROM space_members m JOIN spaces s ON s.id = m.space_id
 WHERE m.user_id = $1
 ORDER BY lower(s.name), s.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("reading the account's spaces: %w", err)
	}
	defer rows.Close()
	var out []accountSpace
	for rows.Next() {
		var s accountSpace
		if err := rows.Scan(&s.id, &s.shortID, &s.name, &s.kind, &s.owned, &s.members, &s.projects); err != nil {
			return nil, fmt.Errorf("scanning the account's space: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// deletionPlan splits the spaces: what blocks the deletion, what goes with
// the account, what the account leaves.
type deletionPlan struct {
	blocking []spaces.BlockingSpace
	// ownsShared — a regular space with other participants is among the
	// blocking ones; ownsCommon — the common space is.
	ownsShared, ownsCommon bool
	deleted                []accountSpace
	left                   []accountSpace
}

func planDeletion(all []accountSpace) deletionPlan {
	p := deletionPlan{blocking: []spaces.BlockingSpace{}}
	for _, s := range all {
		ref := spaces.SpaceRef{ID: s.shortID, Slug: spaces.Slug(s.name), Name: s.name}
		switch {
		case s.kind == spaces.KindPersonal:
			p.deleted = append(p.deleted, s)
		case !s.owned:
			p.left = append(p.left, s)
		case s.kind == spaces.KindCommon:
			p.ownsCommon = true
			p.blocking = append(p.blocking, spaces.BlockingSpace{SpaceRef: ref, Kind: s.kind, MemberCount: s.members})
		case s.members > 1:
			p.ownsShared = true
			p.blocking = append(p.blocking, spaces.BlockingSpace{SpaceRef: ref, Kind: s.kind, MemberCount: s.members})
		default:
			p.deleted = append(p.deleted, s)
		}
	}
	return p
}

// refusal is the refusal of the plan, or nil when the deletion may go on.
// owns_spaces_with_members comes before owns_common_space; both name every
// blocking space in the extra field `spaces`.
func (p deletionPlan) refusal() *spaces.Refusal {
	extra := map[string]any{"spaces": p.blocking}
	switch {
	case p.ownsShared:
		r := spaces.Refuse(http.StatusUnprocessableEntity, spaces.CodeOwnsSpacesWithMembers,
			"you own spaces that have other members; delete them first")
		r.Extra = extra
		return r
	case p.ownsCommon:
		r := spaces.Refuse(http.StatusUnprocessableEntity, spaces.CodeOwnsCommonSpace,
			"you own the common space; ask the administrator to change its owner")
		r.Extra = extra
		return r
	}
	return nil
}

var (
	refusedBadPassword = spaces.Refuse(http.StatusUnprocessableEntity, spaces.CodeInvalidCredentials,
		"the password is incorrect")
	refusedLastAdmin = spaces.Refuse(http.StatusUnprocessableEntity, spaces.CodeLastAdminCannotDemote,
		"the last active administrator cannot delete the account")
)

// AccountDeletionPreview is what DeleteAccount would do for userID now
// (GET /me/deletion-preview): the blocking spaces, the spaces deleted with
// the account, and how many open issues outside them lose it as assignee.
func (s *Store) AccountDeletionPreview(ctx context.Context, userID uuid.UUID) (spaces.AccountDeletionPreview, error) {
	all, err := accountSpaces(ctx, s.pool, userID)
	if err != nil {
		return spaces.AccountDeletionPreview{}, err
	}
	p := planDeletion(all)
	out := spaces.AccountDeletionPreview{Blocking: p.blocking, Deleted: []spaces.DeletedSpacePreview{}}
	deletedIDs := make([]uuid.UUID, 0, len(p.deleted))
	for _, d := range p.deleted {
		out.Deleted = append(out.Deleted, spaces.DeletedSpacePreview{
			SpaceRef:     spaces.SpaceRef{ID: d.shortID, Slug: spaces.Slug(d.name), Name: d.name},
			Kind:         d.kind,
			ProjectCount: d.projects,
		})
		deletedIDs = append(deletedIDs, d.id)
	}
	if err := s.pool.QueryRow(ctx, `
SELECT count(*) FROM issues i JOIN projects p ON p.id = i.project_id
 WHERE i.status <> 'closed' AND NOT (p.space_id = ANY($2::uuid[]))
   AND `+assignee.NamesUser("i", "$1::uuid"), userID, deletedIDs).Scan(&out.OpenAssignedIssues); err != nil {
		return spaces.AccountDeletionPreview{}, fmt.Errorf("counting the open issues assigned: %w", err)
	}
	return out, nil
}

// VerifyDeletionPassword checks the password confirming a self-service
// deletion against the account's hash, outside any transaction (bcrypt
// holds no connection), and returns the hash it matched — DeleteAccount
// requires the same hash under its lock. A wrong password, an account
// without one, or a deactivated account: invalid_credentials; an account
// that no longer exists or is already deleted: ErrUserNotFound.
func (s *Store) VerifyDeletionPassword(ctx context.Context, userID uuid.UUID, password string) (string, error) {
	var hash *string
	var deleted, deactivated bool
	err := s.pool.QueryRow(ctx, `
SELECT password_hash, deleted_at IS NOT NULL, deactivated_at IS NOT NULL FROM users WHERE id = $1`, userID).
		Scan(&hash, &deleted, &deactivated)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && deleted {
		return "", ErrUserNotFound
	}
	if err != nil {
		return "", fmt.Errorf("reading the password hash: %w", err)
	}
	if deactivated || hash == nil || auth.Verify(*hash, password) != nil {
		return "", refusedBadPassword
	}
	return *hash, nil
}

// DeleteAccount deletes the account userID (see the file header). With
// verifiedHash set (the self-service deletion: the hash
// VerifyDeletionPassword matched) the account's hash must still be that
// one; empty — the operator's deletion — checks no password. Refusals are
// *spaces.Refusal; ErrUserNotFound when there is no such account or it is
// already deleted.
func (s *Store) DeleteAccount(ctx context.Context, userID uuid.UUID, verifiedHash string, actor Actor) error {
	sa := spaces.Actor{UserID: actor.UserID, Username: actor.Username, SessionID: actor.SessionID, Type: actor.Role}
	return spaces.WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		toLock, err := collectUUIDs(ctx, tx, `
SELECT space_id FROM space_members WHERE user_id = $1
UNION
SELECT i.space_id FROM space_invitations i, users u
 WHERE u.id = $1 AND i.status = 'pending' AND i.space_id IS NOT NULL
   AND (i.invited_by = u.id OR i.email = lower(btrim(u.email)))
ORDER BY 1`, userID)
		if err != nil {
			return fmt.Errorf("reading the spaces to lock: %w", err)
		}
		for _, id := range toLock {
			if _, err := spaces.LockSpace(ctx, tx, id); err != nil && !errors.Is(err, spaces.ErrSpaceNotFound) {
				return err
			}
		}
		var who identity
		var hash *string
		var deleted bool
		err = tx.QueryRow(ctx, `
SELECT email, username, name, password_hash, deleted_at IS NOT NULL FROM users WHERE id = $1 FOR UPDATE`, userID).
			Scan(&who.email, &who.username, &who.name, &hash, &deleted)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && deleted {
			return ErrUserNotFound
		}
		if err != nil {
			return fmt.Errorf("locking the account: %w", err)
		}
		if verifiedHash != "" && (hash == nil || *hash != verifiedHash) {
			return refusedBadPassword
		}

		all, err := accountSpaces(ctx, tx, userID)
		if err != nil {
			return err
		}
		plan := planDeletion(all)
		if ref := plan.refusal(); ref != nil {
			return ref
		}

		changed, err := s.DeactivateInTx(ctx, tx, userID, actor)
		if errors.Is(err, ErrLastAdmin) {
			return refusedLastAdmin
		}
		if err != nil {
			return err
		}
		if !changed {
			// Deactivated before (the operator deletes a blocked account):
			// the deletion still ends whatever was issued since.
			if _, err := BumpSessionEpochInTx(ctx, tx, userID); err != nil {
				return err
			}
			if _, err := s.apikeysStore.RevokeAllForUser(ctx, tx, userID); err != nil {
				return err
			}
		}

		for _, sp := range plan.deleted {
			if err := spaces.DeleteInTx(ctx, tx, sp.id, sa); err != nil {
				return fmt.Errorf("deleting space %s: %w", sp.id, err)
			}
		}
		for _, sp := range plan.left {
			if err := spaces.LeaveInTx(ctx, tx, sp.id, userID, authz.RevokeAccountDeleted, false, sa); err != nil {
				return fmt.Errorf("leaving space %s: %w", sp.id, err)
			}
		}
		if err := revokeEverywhere(ctx, tx, userID, sa); err != nil {
			return err
		}
		if err := retireInvitations(ctx, tx, userID, who.email, actor.UserID); err != nil {
			return err
		}
		if err := dropPersonalState(ctx, tx, userID, who.email); err != nil {
			return err
		}
		return anonymizeInTx(ctx, tx, userID, who)
	})
}

// revokeEverywhere applies INV-12 to every project the account still has
// anything in after leaving its spaces: a membership row (an external
// member of a project of another space), an open issue it is assignee of,
// a claim, a watch, a reminder. The account is deactivated, so it sees
// none of them: all are lost, and the membership rows go.
func revokeEverywhere(ctx context.Context, tx pgx.Tx, userID uuid.UUID, actor spaces.Actor) error {
	candidates, err := collectUUIDs(ctx, tx, `
SELECT project_id FROM project_members WHERE user_id = $1
UNION SELECT i.project_id FROM issues i
       WHERE i.status <> 'closed' AND `+assignee.NamesUser("i", "$1::uuid")+`
UNION SELECT project_id FROM issues WHERE locked_by_user_id = $1
UNION SELECT i.project_id FROM issue_watchers w JOIN issues i ON i.id = w.issue_id WHERE w.user_id = $1
UNION SELECT project_id FROM project_watchers WHERE user_id = $1
UNION SELECT i.project_id FROM reminders r JOIN issues i ON i.id = r.issue_id WHERE r.user_id = $1
ORDER BY 1`, userID)
	if err != nil {
		return fmt.Errorf("reading what the account still has: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM project_members WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("removing the project memberships: %w", err)
	}
	if _, err := authz.RevokeProjectAccessSeen(ctx, tx, userID, candidates, nil, authz.RevokeAccountDeleted, actor); err != nil {
		return err
	}
	return nil
}

// retireInvitations revokes the pending invitations to the account's email
// and those it sent, in id order, as the owner's revocation does
// (invitations.RevokePendingInTx: tokens retired, unsent emails cancelled —
// E-6: the link answers «invalid» — the addressees, the owners and the
// senders signalled; actor is who acts, skipped by the managers' signal),
// and drops its rows of the send log: the
// daily limit and the captcha threshold count per sender, so no other
// sender's count moves. Its email in the invitations is replaced by the
// pseudonym in anonymizeInTx.
func retireInvitations(ctx context.Context, tx pgx.Tx, userID uuid.UUID, email string, actor uuid.UUID) error {
	pending, err := collectUUIDs(ctx, tx, `
SELECT id FROM space_invitations
 WHERE status = 'pending' AND (invited_by = $1 OR email = lower(btrim($2)))
 ORDER BY id`, userID, email)
	if err != nil {
		return fmt.Errorf("reading the invitations: %w", err)
	}
	for _, inv := range pending {
		if err := invitations.RevokePendingInTx(ctx, tx, inv, actor); err != nil {
			return fmt.Errorf("revoking invitation %s: %w", inv, err)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM space_invitation_sends WHERE sender_id = $1`, userID); err != nil {
		return fmt.Errorf("dropping the send log: %w", err)
	}
	return nil
}

// dropPersonalState deletes what only the account had: its tokens and every
// email of it or to its address, finished ones included (the mail cleanup,
// mail/cleanup.go, removes only what a worker finalises later), push
// subscriptions, agent sessions,
// monitor credentials, preferences, notifications, watches, reminders and
// its themes; its revoked keys keep their rows, labelled by the pseudonym
// in anonymizeInTx.
func dropPersonalState(ctx context.Context, tx pgx.Tx, userID uuid.UUID, email string) error {
	steps := []struct{ what, sql string }{
		{"tokens", `DELETE FROM auth_tokens WHERE user_id = $1`},
		// Every email of the account or to its address, finished ones
		// too: after retireInvitations, whose
		// cancellation turns the unsent invitation emails final. The mail
		// cleanup stays for an outcome a worker writes later.
		{"emails", `DELETE FROM outgoing_emails WHERE user_id = $1 OR recipient = lower(btrim($2))`},
		{"push subscriptions", `DELETE FROM push_subscriptions WHERE user_id = $1`},
		{"agent sessions", `DELETE FROM agents WHERE user_id = $1`},
		{"monitor credentials", `DELETE FROM monitor_credentials WHERE user_id = $1`},
		{"notifications", `DELETE FROM notifications WHERE user_id = $1`},
		{"issue watches", `DELETE FROM issue_watchers WHERE user_id = $1`},
		{"watch opt-outs", `DELETE FROM issue_watch_optouts WHERE user_id = $1`},
		{"project watches", `DELETE FROM project_watchers WHERE user_id = $1`},
		{"custom watchers", `DELETE FROM custom_watchers WHERE user_id = $1`},
		{"reminders", `DELETE FROM reminders WHERE user_id = $1`},
		{"reminder rate limit", `DELETE FROM reminder_rate_limit WHERE user_id = $1`},
		{"notification preferences", `DELETE FROM user_notification_prefs WHERE user_id = $1`},
		{"notification reason preferences", `DELETE FROM user_notification_reason_prefs WHERE user_id = $1`},
		{"interface preferences", `DELETE FROM user_ui_prefs WHERE user_id = $1`},
		{"theme choice", `UPDATE users SET custom_theme_id = NULL, theme_mode = 'builtin', builtin_theme = 'light' WHERE id = $1`},
		{"themes", `DELETE FROM themes WHERE author_id = $1`},
	}
	for _, st := range steps {
		args := []any{userID}
		if st.what == "emails" {
			args = append(args, email)
		}
		if _, err := tx.Exec(ctx, st.sql, args...); err != nil {
			return fmt.Errorf("deleting the account's %s: %w", st.what, err)
		}
	}
	return nil
}

// collectUUIDs runs sql and reads one uuid column.
func collectUUIDs(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}
