-- 000049_lock_owner — sk-424.
--
-- WHY. The right to dispose of a locked issue (release, close,
-- edit) was checked by COMPARING THE MCP SESSION IDENTIFIER and nothing else:
-- `issues.locked_by` stores the session text, and `lockGuard` / `Unclaim` /
-- `claimInTx` compared the presented `Mcp-Session-Id` with that text. None
-- of the three checks asked WHOSE lock it was.
--
-- A session identifier cannot be such a basis, and not for one reason
-- but for three at once:
--
--   * it is not an identity document. In this product the identity is
--     the api key, which resolves to a user (`api_keys.user_id`); a session is
--     the identifier of an agent INSTANCE, and it changes on every reconnect;
--   * it is not a secret. It is openly given to every reader of the issue: the field
--     `lockedBy` in the REST detail and in the MCP show, `actorSessionId` in the event
--     feed;
--   * it belongs to nobody. The primary key of `agents` is the PAIR
--     (user_id, session_id), there is no uniqueness on `session_id` alone, and the
--     registry write is done before any check and is declared in code
--     best-effort ("agent identification is convenience, not auth"). So
--     the same session string legitimately exists for two users
--     at once, and no layer objects.
--
-- Together the three properties mean that presenting someone else's session string
-- passed the holder check, while `locked_by` stored nothing by which one
-- could tell the difference.
--
-- WHAT THIS COLUMN DOES. It puts the lock's OWNER next to the lock — the
-- user whose api key performed the claim. The user becomes the basis of the check;
-- the session stays the second half and keeps separating sibling
-- instances of ONE user (documented property: "claims
-- are per-agent"). No legitimate path is narrowed: your own lock is released
-- by the same one who took it; an administrator still goes through force;
-- auto-release of a stale lock works as it did.
--
-- WHY A COLUMN, NOT A DERIVATION FROM THE JOURNAL. The owner could be fetched every time
-- from the `issue.claimed` event — this is what `lockHolderUsernameFor` does
-- to DISPLAY the holder name. For DISPLAY that is acceptable: no event found —
-- the field is simply absent, the card is intact. For a RIGHT it is not. The journal is
-- an audit, not an access list: deriving from it has no safe outcome when
-- nothing is found (let in — the defect returns, do not let in — lock out the legitimate
-- holder), and the comment on that function itself records the fragility: every
-- new path that sets a lock MUST NOT forget to write the event. The fact
-- the right relies on is stored in the same row as what it protects, and
-- is written in the same transaction with it.
--
-- ON DELETE SET NULL — like `issues.reviewed_by` (000044): deleting a
-- user must not break the issue record. An orphaned lock (owner
-- erased) becomes "owner unknown" and by the rule below lets nobody in
-- without force — the right side to fail on.

ALTER TABLE issues
    ADD COLUMN locked_by_user_id uuid REFERENCES users(id) ON DELETE SET NULL;

COMMENT ON COLUMN issues.locked_by_user_id IS
    'sk-424: owner of the lock — the user whose api key performed the claim. '
    'Written in the same transaction as locked_by (the only writer of a non-empty '
    'lock is claimInTx) and cleared together with it by EVERY release path '
    '(unclaim, close, release on status transition, the stale-lock reaper). '
    'The basis of the check of the right to dispose of the lock; locked_by (the session) is the '
    'second half of the pair, separating sibling instances of one user. '
    'NULL with a non-empty locked_by means "owner unknown" and lets '
    'nobody in except force.';

-- SEED OF HISTORICAL LOCKS.
--
-- At apply time locks already exist, and their owner is recorded nowhere
-- except the journal. Leaving them with NULL would lock out legitimate holders
-- (the "NULL lets nobody in" rule) — so the owner is restored from the
-- authoritative claim record by the SAME triple that is used by
-- `lockHolderUsernameFor` (sk-421.1):
--
--   the issue.claimed event of this issue whose new_data->>'lockedBy' equals the
--   session lying in issues.locked_by NOW, and whose occurred_at equals the moment
--   issues.locked_at.
--
-- Matching by MOMENT is what makes the choice unambiguous: a lock and its event
-- are written in one transaction, where locked_at = now() and occurred_at DEFAULT now()
-- give the same timestamp. A repeated claim by the same session writes a new
-- event with its own moment, and only that one matches the current locked_at.
-- ORDER BY id DESC removes the theoretical ambiguity within one moment.
--
-- Rows for which no event was found (the journal was trimmed, the lock was set by a path
-- that no longer exists) stay NULL — that is "owner unknown".
-- This is deliberate: there is nothing to invent an owner from, and silently letting anyone in
-- is exactly the defect this migration closes. Such an issue can be unlocked
-- by an administrator via force, and that is the normal path.

-- The form is a CORRELATED scalar subquery in SET, not LATERAL in FROM.
-- `UPDATE issues i ... FROM LATERAL (… i.id …)` is rejected by Postgres
-- ("invalid reference to FROM-clause entry for table "i""): a FROM item may not
-- reference the update target, even when marked LATERAL. A scalar
-- subquery in SET is allowed to, and it also gives the needed behavior when nothing is found
-- — NULL, that is "owner unknown", exactly as described above.
UPDATE issues i
   SET locked_by_user_id = (
        SELECT ev.actor_user_id
          FROM events ev
         WHERE ev.entity_type = 'issue'
           AND ev.entity_id = i.id
           AND ev.type = 'issue.claimed'
           AND ev.new_data->>'lockedBy' = i.locked_by
           AND ev.occurred_at = i.locked_at
         ORDER BY ev.id DESC
         LIMIT 1
       )
 WHERE i.locked_by IS NOT NULL
   AND i.locked_at IS NOT NULL;

-- NO INDEX, ON PURPOSE. The column is read only together with the issue row,
-- which has already been found by primary key (fetchIssueInTx inside the transaction), and
-- takes part in the UPDATE predicate of that same single row. There is no entry into the table
-- by this column at all, so an index would serve a nonexistent
-- query and cost more on every claim and release.
