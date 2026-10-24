-- 000060_issues_assignee_user_id — sk-622.21, T002 (rollout R1; D8, K5, K7).
--
-- WHY. The issue assignee is stored as text (username, email or user
-- id — internal/assignee), and every read of "my" issues
-- matches it against users with three OR expressions without an index. US-16 requires
-- storing the assignee user id alongside, so that Inbox and the header lenses
-- match by id through an index.
--
-- INV-15. At any moment after commit issues.assignee_user_id = the result of
-- resolving issues.assignee against the current users directory (NULL if the text
-- does not resolve). It is maintained by two triggers IN THE SAME transaction as the
-- write (K7) — so the invariant is upheld by any writer, including the old code version
-- during the rollout and paths that are easy to miss (claim, bulk,
-- epic_create, SSO auth/oidc.go#updateBySub):
--
--   * issues_resolve_assignee_user_id — BEFORE INSERT OR UPDATE OF assignee,
--     assignee_user_id ON issues: the column is always recomputed from the text;
--     a direct write of another assignee_user_id value has no effect.
--   * users_resync_assignee_user_id_{ins,upd} — AFTER INSERT / AFTER UPDATE
--     OF email, username (on a real change) ON users: recomputes issues whose lower(assignee) equals
--     the previous or the new username / email / id. This preserves late
--     binding: an issue assigned to an email before registration becomes
--     the user's issue at the moment the account is created; an email change on SSO sign-in
--     moves the issues along.
--
-- THE RESOLUTION RULE is the same as internal/assignee#JoinOn: username or
-- email case-insensitively, or an id in canonical text form (also
-- case-insensitive); deactivated users are NOT excluded. The namespaces
-- do not overlap (username has no '@' and is at most 30 characters, uuid is
-- 36 characters without '@', email has '@'), and username and email are unique
-- case-insensitively, so there is at most one match.
--
-- RACES AND LOCK ORDER. Resolution reads users, recomputation reads
-- issues; under READ COMMITTED concurrent "assign to bob@x" and "create /
-- rename bob@x" could fail to see each other and leave NULL.
-- So both sides work with a transactional advisory lock on the hash of
-- lower(text):
--
--   * the users side (recomputation by directory) takes EXCLUSIVE locks
--     on the keys in sorted order and WAITS for them, then updates the issues
--     rows (and waits for their locks);
--   * the issues side (BEFORE trigger) already holds the lock on its row —
--     a trigger cannot take anything before it. So it does NOT wait for
--     advisory: pg_try_advisory_xact_lock_shared, and if the lock is busy —
--     RAISE with SQLSTATE 40001 (serialization_failure). The assignment transaction
--     rolls back and releases the row; the users recomputation runs to the end.
--
-- Why this way and not another (found in review): the earlier variant WAITED for the shared lock on the issues side,
-- holding the row, while the users side waited for that row, holding the exclusive one —
-- a cycle and 40P01 (reproduced with two connections; the victim could also be an SSO
-- sign-in, which has no retry). Now waiting goes in only one
-- direction — users waits for issues — and there is no cycle. The variant "users first
-- locks the issues rows, then advisory" was rejected: a batch assignment
-- of several rows to different forms of the same person again gives a cycle.
--
-- Price: an assignment that coincides in time with account creation or a change of its
-- email/username for the same text (the window is the milliseconds of the users transaction)
-- gets 40001. The issues write paths get 40001 from SSI anyway
-- (SERIALIZABLE): some retry it (withSerializableRetry), others
-- answer version_conflict / serialization_retry — the client retries. The users
-- side (SSO auth/oidc.go#updateBySub, insertNew, users.Store) is never interrupted
-- by this mechanism — it only waits. This mechanism does not produce 40P01.
--
-- Visibility: under READ COMMITTED the side that took the lock gets a fresh snapshot with the next
-- statement of the function and sees the committed write of the
-- other. Two SERIALIZABLE transactions are protected by SSI. The remainder is an assignment in a
-- SERIALIZABLE transaction with a snapshot taken before a concurrent users write
-- was committed, not in SERIALIZABLE: it is closed by the T020 reconciliation and the T021
-- divergence metric, like rows written before this migration.
--
-- BACKFILL of existing rows is not here (T020): the column is added as NULL.
-- The old code version does not know the column and does not write it — the trigger fills it in
-- for it.
--
-- search_path of all three functions is pinned (pg_catalog, public) — the same
-- security default as in the 000034/000044/000050 triggers: unqualified
-- users / issues and built-in functions cannot be substituted through the session
-- search_path.
--
-- INDEXES (K5) — a plain CREATE INDEX inside the migration transaction (project
-- convention, see 000032_notifications_poll_index):
--   * idx_issues_assignee_user_active — Inbox and lenses by assignee id,
--     active work only (status.ActiveWork: not closed and not standing),
--     the cost does not grow with the number of the person's closed issues;
--   * idx_issues_assignee_lower — recomputation by directory (lower(assignee)).
-- There is no separate index for the FK ON DELETE SET NULL action: there is no product
-- path of hard-deleting a user (as with issues.reviewed_by).

ALTER TABLE issues
    ADD COLUMN assignee_user_id uuid NULL REFERENCES users(id) ON DELETE SET NULL;

COMMENT ON COLUMN issues.assignee_user_id IS
    'Assignee user: the result of resolving the assignee text (username, '
    'email or id, case-insensitive; deactivated are not excluded) against '
    'the current users directory. NULL — the text is empty or does not resolve. '
    'Maintained by the triggers issues_resolve_assignee_user_id and '
    'users_resync_assignee_user_id_{ins,upd} in the same transaction as the write (INV-15, '
    'sk-622.21). A direct write to the column is overwritten by resolution.';

-- Resolving the assignee text into a user id. Three separate lookups —
-- each by its own unique index (PK, idx_users_username_lower,
-- idx_users_email_lower); only text of canonical form is cast to uuid,
-- so that arbitrary text does not give 22P02. The namespaces do not overlap,
-- so the order of branches does not affect the result.
CREATE FUNCTION resolve_assignee_user_id(handle text) RETURNS uuid
LANGUAGE plpgsql STABLE
SET search_path = pg_catalog, public
AS $$
DECLARE
    uid uuid;
BEGIN
    IF handle IS NULL OR handle = '' THEN
        RETURN NULL;
    END IF;
    IF handle ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN
        SELECT u.id INTO uid FROM users u WHERE u.id = handle::uuid;
        IF FOUND THEN
            RETURN uid;
        END IF;
    END IF;
    SELECT u.id INTO uid FROM users u WHERE lower(u.username) = lower(handle);
    IF FOUND THEN
        RETURN uid;
    END IF;
    SELECT u.id INTO uid FROM users u WHERE lower(u.email) = lower(handle);
    RETURN uid;
END;
$$;

COMMENT ON FUNCTION resolve_assignee_user_id(text) IS
    'Resolves the issue assignee text into a user id by the rule '
    'internal/assignee#JoinOn: id in canonical form, email or username '
    'case-insensitively; deactivated are not excluded. NULL — does not resolve '
    '(sk-622.21, K7).';

CREATE FUNCTION skein_issues_resolve_assignee_user_id() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
BEGIN
    IF NEW.assignee IS NULL OR NEW.assignee = '' THEN
        NEW.assignee_user_id := NULL;
        RETURN NEW;
    END IF;
    -- Shared and WITHOUT waiting: the row is already locked, waiting for advisory
    -- here would close a cycle with the users side (see the header, "LOCK
    -- ORDER"). Concurrent assignments to one text do not
    -- interfere with each other; an in-progress recomputation by directory is a reason to retry the transaction.
    IF NOT pg_try_advisory_xact_lock_shared(hashtextextended(lower(NEW.assignee), 0)) THEN
        RAISE EXCEPTION 'assignee % is being re-resolved by a concurrent users write; retry the transaction',
            NEW.assignee
            USING ERRCODE = 'serialization_failure';
    END IF;
    NEW.assignee_user_id := resolve_assignee_user_id(NEW.assignee);
    RETURN NEW;
END;
$$;

CREATE TRIGGER issues_resolve_assignee_user_id
    BEFORE INSERT OR UPDATE OF assignee, assignee_user_id ON issues
    FOR EACH ROW
    EXECUTE FUNCTION skein_issues_resolve_assignee_user_id();

CREATE FUNCTION skein_users_resync_assignee_user_id() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
DECLARE
    handles text[];
    h text;
BEGIN
    IF TG_OP = 'INSERT' THEN
        handles := ARRAY[lower(NEW.username), lower(NEW.email), NEW.id::text];
    ELSE
        handles := ARRAY[lower(OLD.username), lower(OLD.email), OLD.id::text,
                         lower(NEW.username), lower(NEW.email), NEW.id::text];
    END IF;
    SELECT array_agg(DISTINCT x ORDER BY x) INTO handles
      FROM unnest(handles) AS x
     WHERE x IS NOT NULL AND x <> '';

    -- Exclusive, in sorted key order, with waiting: the issues
    -- side does not wait for it (see the header, "LOCK ORDER").
    FOREACH h IN ARRAY handles LOOP
        PERFORM pg_advisory_xact_lock(hashtextextended(h, 0));
    END LOOP;

    -- The assignment is recomputed by the issues BEFORE trigger; the value here is
    -- the same resolution, and the condition cuts off rows where there is nothing to change.
    UPDATE issues i
       SET assignee_user_id = resolve_assignee_user_id(i.assignee)
     WHERE i.assignee IS NOT NULL
       AND lower(i.assignee) = ANY(handles)
       AND i.assignee_user_id IS DISTINCT FROM resolve_assignee_user_id(i.assignee);
    RETURN NULL;
END;
$$;

CREATE TRIGGER users_resync_assignee_user_id_ins
    AFTER INSERT ON users
    FOR EACH ROW
    EXECUTE FUNCTION skein_users_resync_assignee_user_id();

-- Only on a value change that affects resolution: editing the name, role,
-- password, or login of a user does not trigger recomputation.
CREATE TRIGGER users_resync_assignee_user_id_upd
    AFTER UPDATE OF email, username ON users
    FOR EACH ROW
    WHEN (lower(OLD.email) IS DISTINCT FROM lower(NEW.email)
       OR lower(OLD.username) IS DISTINCT FROM lower(NEW.username))
    EXECUTE FUNCTION skein_users_resync_assignee_user_id();

CREATE INDEX idx_issues_assignee_user_active
    ON issues (assignee_user_id)
    WHERE assignee_user_id IS NOT NULL AND status NOT IN ('closed', 'standing');

CREATE INDEX idx_issues_assignee_lower
    ON issues (lower(assignee))
    WHERE assignee IS NOT NULL;
