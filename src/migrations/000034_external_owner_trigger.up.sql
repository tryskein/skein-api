-- 000034_external_owner_trigger — sk-163 S3
--
-- Cross-table DB backstop for the "external user is NEVER a project owner"
-- invariant (spec §5). The same-table half — "external user is NEVER an
-- admin" — is already a CHECK constraint (users_external_not_admin, added in
-- 000033). A CHECK cannot reference a second table, so the owner half needs a
-- TRIGGER: project_members.role couples to users.is_external, two tables.
--
-- This is the FIRST trigger in the project. The pattern set here:
--   * one BEFORE-ROW trigger function, branching on TG_TABLE_NAME so the same
--     logic guards both edges of the invariant from a single definition;
--   * a custom SQLSTATE ('SK001') the Go store layer pattern-matches and
--     translates into the typed business error projects.ErrExternalCannotBeOwner
--     (so a direct-SQL / import / missed-path violation surfaces as a clean
--     422 / validation_error, never a raw 500);
--   * app-level enforcement remains PRIMARY (users.Store / projects.Store
--     return the typed error BEFORE the write reaches this trigger). The
--     trigger is the backstop for paths that bypass the app layer.
--
-- The invariant has two directions, so the trigger fires on BOTH tables:
--   (1) project_members INSERT/UPDATE → a row with role='owner' may not
--       reference an external user.
--   (2) users UPDATE flipping is_external false→true → that user may not
--       currently own any project. (INSERT of a fresh external user can never
--       violate it — a brand-new user has no project_members rows yet — so the
--       users trigger is UPDATE-only.)
--
-- CONCURRENCY NOTE: this BEFORE-ROW cross-table check does NOT close
-- write-skew under READ COMMITTED — between the trigger's SELECT and COMMIT a
-- concurrent tx could insert a conflicting row (e.g. make the user an owner
-- while another tx flips them to external). Every app-level path that mutates
-- is_external false→true OR inserts an owner row MUST run at SERIALIZABLE (or
-- take an explicit row lock on the user) so the two sides serialize. The
-- current writers (UpsertMember, CreateProject) already use SERIALIZABLE; any
-- future is_external mutation path (S8) must do the same.
--
-- search_path is pinned (pg_catalog, public) so the unqualified table
-- references below resolve deterministically regardless of the caller's
-- session search_path — a SECURITY hardening default for this first trigger.

CREATE FUNCTION skein_enforce_external_not_owner() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public
    AS $$
DECLARE
    v_is_external  boolean;
    v_owns_project boolean;
BEGIN
    IF TG_TABLE_NAME = 'project_members' THEN
        -- Only owner rows can violate the invariant; member rows are always
        -- allowed for external users (an external user is capped AT member).
        IF NEW.role = 'owner' THEN
            SELECT is_external INTO v_is_external
              FROM users WHERE id = NEW.user_id;
            IF v_is_external THEN
                RAISE EXCEPTION 'external user cannot be a project owner'
                    USING ERRCODE = 'SK001';
            END IF;
        END IF;
        RETURN NEW;
    ELSIF TG_TABLE_NAME = 'users' THEN
        -- Only the false→true flip can NEWLY violate the invariant. Guarding
        -- on the transition keeps the common UPDATE (name/role/password) free
        -- of the project_members scan.
        IF NEW.is_external AND NOT OLD.is_external THEN
            SELECT EXISTS (
                SELECT 1 FROM project_members
                 WHERE user_id = NEW.id AND role = 'owner'
            ) INTO v_owns_project;
            IF v_owns_project THEN
                RAISE EXCEPTION 'external user cannot be a project owner'
                    USING ERRCODE = 'SK001';
            END IF;
        END IF;
        RETURN NEW;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER project_members_external_not_owner
    BEFORE INSERT OR UPDATE ON project_members
    FOR EACH ROW
    EXECUTE FUNCTION skein_enforce_external_not_owner();

CREATE TRIGGER users_external_not_owner
    BEFORE UPDATE ON users
    FOR EACH ROW
    EXECUTE FUNCTION skein_enforce_external_not_owner();
