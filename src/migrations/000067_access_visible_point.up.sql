-- 000067_access_visible_point — sk-674.18, T009 (K-1, SC-013).
--
-- WHY. The predicate "the row's project is visible to the caller" sits inside big
-- queries in a boolean context: FILTER counters of project rows, the flag
-- "locked for the reader", EXISTS over edges. The inlined relation
-- skein_project_access_set loses there: its admin branch is estimated by the
-- planner as "all projects" for ANY caller, and the correlated
-- EXISTS over edges unfolds into a hashed subplan over the whole dependency
-- graph (on a large test dataset — tens of seconds and /dev/shm exhaustion on a
-- parallel hash, versus 222 ms for the previous index prober).
--
-- MECHANISM. A boolean point function on top of the SAME relation — there is no rule in
-- it, only the question "is there a relation row for this project".
-- The body is SELECT EXISTS(...): the planner does not inline such a scalar
-- function, it stays an opaque call with the default SQL function cost,
-- and the query keeps a correlated plan with index probes
-- (users_pkey, projects_pkey, project_members_pkey) per row.
-- Set queries ("all visible rows") still join the relation
-- itself (authz.VisibleJoin, K-1).
--
-- The space rules (T017) change only the body of skein_project_access_set;
-- this function follows it without edits.

CREATE FUNCTION skein_project_visible(caller uuid, project uuid)
RETURNS boolean
LANGUAGE sql
STABLE
SECURITY INVOKER
PARALLEL SAFE
AS $$
    SELECT EXISTS (
        SELECT 1 FROM skein_project_access_set(caller) va
         WHERE va.project_id = project)
$$;

COMMENT ON FUNCTION skein_project_visible(uuid, uuid) IS
    'sk-674.18 K-1: does caller see project — a row of skein_project_access_set exists. '
    'The per-row boolean form of the relation (internal/authz).';
