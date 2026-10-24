package mentions

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/authz"
	"skein-api/internal/person"
)

// Querier is the minimal pgx surface Resolve needs. Both *pgxpool.Pool
// and pgx.Tx satisfy this contract (Query is on both). Letting Resolve
// accept either keeps the write path inside the source mutation's
// SERIALIZABLE tx (atomicity for the watcher graph) while letting the
// read path (GetIssue / ListComments hydration) go straight against
// the pool — no spurious tx for a pure SELECT.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ResolvedUser is one entry in the Resolve() result. Username is the
// lowercase canonical form (matches users.username); UserID is the
// users.id that the mention resolves to. Callers feed this into
// AddIssueWatcher and the WriteMentionNotification helper.
type ResolvedUser struct {
	Username string
	UserID   uuid.UUID
	// Status — the person marks for display (sk-674.18 T027): set only by
	// the read path (Hydrate*) for a deleted account (deleted + pseudonym,
	// E-22); nil on the notification path (Resolve).
	Status *person.Status
}

// Resolve maps a list of candidate usernames (from Parse()) to real
// users.id values, restricted to project visibility. A candidate
// resolves IFF:
//
//  1. A users row exists with username = candidate (case-insensitive,
//     but the DB stores lowercase so we lowercase the input first).
//
//  2. The user is NOT deactivated (`deactivated_at IS NULL`). A
//     deactivated user has no inbox and shouldn't accumulate watcher /
//     notification rows that would re-fire on reactivation.
//
//  3. The user sees projectID now (authz.RecipientEligibleExpr — the
//     access relation, INV-1) AND belongs to its context of people
//     (authz.PersonInProjectExpr, INV-9): a participant of the project's
//     space, an external member of the project, or the effective instance
//     admin when the project is not in a "Personal" (sk-741).
//
// Candidates that don't resolve are silently dropped — plan §H/§F-11
// note: leaking that "@foo doesn't exist or isn't a member" via a
// validation error would expose membership state to non-members. The
// raw text remains in the description / comment body verbatim; the
// frontend renders unresolved `@foo` as plain text.
//
// Empty input returns nil. The single SQL avoids one round-trip per
// candidate — even a 25-mention paste resolves in one query. The
// query runs inside the caller's tx (typically the SERIALIZABLE tx
// of AddComment / CreateIssue / PatchIssue, plan §F-13) so the
// resolved set is consistent with the membership snapshot the rest
// of the mutation sees.
func Resolve(
	ctx context.Context,
	q Querier,
	projectID uuid.UUID,
	candidates []string,
) ([]ResolvedUser, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	if projectID == uuid.Nil {
		return nil, fmt.Errorf("mentions.Resolve: projectID is required")
	}

	// users.username is stored lowercase by the DB CHECK; Parse() also
	// captures only [a-z0-9_-]. We pass the slice directly — pgx maps it
	// to a Postgres text[] which the = ANY($2) predicate consumes.
	// A mention target passes the recipient filter of authz (sk-674.18
	// T010): an active user who sees the project — deactivated users stay
	// out of the watcher graph (see ResolvedUser), and the reach of an admin
	// is the access relation's, not a role arm here.
	//
	// INV-9 (sk-674.18 T027): the person must also belong to the issue's
	// context — a participant of the project's space, an external member
	// of the project, or the instance admin in a non-personal project
	// (sk-741: he is mentioned there like a participant).
	sql := `
SELECT u.username, u.id
  FROM users u
 WHERE u.username = ANY($2::text[])
   AND ` + authz.RecipientEligibleExpr("u.id", "$1::uuid") + `
   AND ` + authz.PersonInProjectExpr("u.id", "$1::uuid") + `
`
	rows, err := q.Query(ctx, sql, projectID, candidates)
	if err != nil {
		return nil, fmt.Errorf("resolving mentions: %w", err)
	}
	defer rows.Close()

	out := make([]ResolvedUser, 0, len(candidates))
	for rows.Next() {
		var r ResolvedUser
		if err := rows.Scan(&r.Username, &r.UserID); err != nil {
			return nil, fmt.Errorf("scanning mention row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scanning mentions: %w", err)
	}
	return out, nil
}

// resolveForDisplay maps candidates to the people a reader is shown as
// mentions in issue issueID of projectID (INV-9, E-22): a person of the
// issue's context (authz.PersonInProjectExpr), or a deleted account with a
// historical relation to the issue (authz.PersonLinkedToIssueExpr) — its
// username is its permanent pseudonym (K-21) and it is shown marked
// deleted, so a mention written while it lived keeps its referent.
// Deletion alone is no ground: a deleted account of
// another space, with no relation to the issue, resolves to nothing like a
// nonexistent name. A person who left the space resolves to nothing and
// stays text. Unlike Resolve it is not a recipient filter: nothing is
// notified from it.
func resolveForDisplay(
	ctx context.Context,
	q Querier,
	projectID uuid.UUID,
	issueID string,
	candidates []string,
) ([]ResolvedUser, error) {
	if projectID == uuid.Nil || issueID == "" {
		return nil, fmt.Errorf("mentions: projectID and issueID are required")
	}
	rows, err := q.Query(ctx, `
SELECT u.username, u.id, `+person.StatusInProjectExpr("u", "$1::uuid")+`
  FROM users u
 WHERE u.username = ANY($2::text[])
   AND (`+authz.PersonInProjectExpr("u.id", "$1::uuid")+`
        OR (u.deleted_at IS NOT NULL
            AND EXISTS (SELECT 1 FROM issues li WHERE li.id = $3
                           AND `+authz.PersonLinkedToIssueExpr("u.id", "li")+`)))
`, projectID, candidates, issueID)
	if err != nil {
		return nil, fmt.Errorf("resolving mentions for display: %w", err)
	}
	defer rows.Close()
	out := make([]ResolvedUser, 0, len(candidates))
	for rows.Next() {
		var r ResolvedUser
		if err := rows.Scan(&r.Username, &r.UserID, &r.Status); err != nil {
			return nil, fmt.Errorf("scanning mention row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scanning mentions: %w", err)
	}
	return out, nil
}

// FilterOutSelf returns a new slice with the actor's own user removed.
// Plan §F-5: a user mentioning themselves doesn't get a notification or
// auto-watch (matches the self-action exclusion in FanOut). actorID ==
// uuid.Nil leaves the slice unchanged — system events have no actor.
//
// Order is preserved. Returns the input slice itself when no filter
// applies (allocation-free fast path).
func FilterOutSelf(resolved []ResolvedUser, actorID uuid.UUID) []ResolvedUser {
	if actorID == uuid.Nil || len(resolved) == 0 {
		return resolved
	}
	for i, r := range resolved {
		if r.UserID == actorID {
			out := make([]ResolvedUser, 0, len(resolved)-1)
			out = append(out, resolved[:i]...)
			out = append(out, resolved[i+1:]...)
			// At most one self-match (Resolve dedups via username, and a
			// user has exactly one row in users). Loop can return early.
			return out
		}
	}
	return resolved
}

// Hydrate is the read-path convenience: parse `text`, resolve the
// candidates for display (resolveForDisplay — never a recipient filter:
// notifying goes through Resolve), and return the resolved set.
// Used by GetIssue / ListComments so the wire DTO can carry a
// pre-resolved `mentions[]` array — the frontend then doesn't need to
// re-query for each `@username` it finds in the rendered text.
//
// Returns nil for empty input or no resolvable mentions. q can be a
// pool or tx; callers pass the pool from the read handler since the
// resolve query is a pure SELECT and doesn't need the write tx.
func Hydrate(
	ctx context.Context,
	q Querier,
	projectID uuid.UUID,
	issueID string,
	text string,
) ([]ResolvedUser, error) {
	candidates := Parse(text)
	if len(candidates) == 0 {
		return nil, nil
	}
	return resolveForDisplay(ctx, q, projectID, issueID, candidates)
}

// HydrateMany is a multi-text variant of Hydrate. It parses each input
// text, computes the union of candidates, resolves once, and returns
// the resolved set. Used by GetIssue to hydrate mentions across
// description / notes / acceptance_criteria / design in a single
// round-trip (vs. four).
//
// The result is deduplicated by username. Order matches Resolve()'s
// output (DB-order on users.username = ANY); callers that want a
// stable order should sort.
func HydrateMany(
	ctx context.Context,
	q Querier,
	projectID uuid.UUID,
	issueID string,
	texts ...string,
) ([]ResolvedUser, error) {
	resolved, _, err := HydrateManyWithParses(ctx, q, projectID, issueID, texts...)
	return resolved, err
}

// HydrateManyWithParses is HydrateMany plus the per-text Parse() output.
// Used by ListComments where the caller needs to attribute the resolved
// set back to each input text (one MentionRef list per comment) — without
// this, the caller would re-Parse every body to compute the per-comment
// attribution, doubling the parse cost and the `seen` map allocations.
//
// The returned [][]string is keyed by input index: parses[i] is the
// Parse(texts[i]) result, in first-seen order. Callers can intersect
// each per-text candidate slice with the resolved-set lookup map to
// produce per-text MentionRef lists.
func HydrateManyWithParses(
	ctx context.Context,
	q Querier,
	projectID uuid.UUID,
	issueID string,
	texts ...string,
) ([]ResolvedUser, [][]string, error) {
	parses := make([][]string, len(texts))
	seen := make(map[string]struct{})
	// Heuristic ~4 unique mentions per text. Do NOT clamp this to
	// MaxMentionsPerText: that constant caps each text individually, so a
	// union over N texts holds up to N*MaxMentionsPerText names — clamping
	// at the per-text value under-sizes every multi-text call (the batch
	// case this function exists for).
	union := make([]string, 0, len(texts)*4)
	for i, t := range texts {
		parsed := Parse(t)
		parses[i] = parsed
		for _, u := range parsed {
			if _, dup := seen[u]; dup {
				continue
			}
			seen[u] = struct{}{}
			union = append(union, u)
		}
	}
	if len(union) == 0 {
		return nil, parses, nil
	}
	resolved, err := resolveForDisplay(ctx, q, projectID, issueID, union)
	if err != nil {
		return nil, parses, err
	}
	return resolved, parses, nil
}

// Diff returns the set difference (newSet \ oldSet) by username — the
// usernames that appear in `newCandidates` but not in `oldCandidates`.
// Plan §F-9 / R4.3b PatchIssue: only newly-introduced mentions trigger
// notifications and auto-watches; mentions already present in the old
// text are no-ops (the user is presumably already watching, and we
// don't want a re-fire on every PATCH that touches the same field).
//
// Inputs are expected to be Parse() output: lowercase, deduplicated,
// first-seen order. Output preserves the order of newCandidates.
func Diff(newCandidates, oldCandidates []string) []string {
	if len(newCandidates) == 0 {
		return nil
	}
	if len(oldCandidates) == 0 {
		// Defensive copy — Diff's contract is read-only on inputs; callers
		// must not see an alias into newCandidates they might later mutate.
		return append([]string(nil), newCandidates...)
	}
	old := make(map[string]struct{}, len(oldCandidates))
	for _, u := range oldCandidates {
		old[u] = struct{}{}
	}
	out := make([]string, 0, len(newCandidates))
	for _, u := range newCandidates {
		if _, present := old[u]; present {
			continue
		}
		out = append(out, u)
	}
	return out
}
