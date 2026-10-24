-- 000075_access_relation_one_time_filters - sk-674.18, T017 (EXPLAIN gate).
--
-- WHY. The T017 EXPLAIN gate on a large test dataset: the body of the 000073 relation cost
-- ~8-10 ms per call for an ordinary user. The admin branch (d)
-- joined the "caller is an admin" check with every project: a Seq Scan over
-- projects (2500 rows) and 2500 users checks per call, although the caller is not
-- an admin; the implicit owner branch read all of projects with a hash join.
-- The relation is read on every request (the visible set, Scope.Bind), so
-- these milliseconds hit every path.
--
-- WHAT. The same INV-1 (a-d) rule, the same levels, the same signature - only the
-- shape of the query changes: checks that depend only on the caller (active,
-- not external, admin, the admin_enabled flag) are uncorrelated subqueries, and
-- the planner runs them once before the branch (One-Time Filter): for a non-admin
-- branch (d) reads no rows of projects; the implicit owner branch
-- goes from their spaces (idx_spaces_owner) to their projects
-- (idx_projects_space).
--
-- ANALYZE. The 000071 transition rewrote projects.space_id and created
-- spaces / space_members rows; without fresh statistics the planner does not see
-- the space_id distribution (the new column was empty) and picks a Seq Scan.

CREATE OR REPLACE FUNCTION skein_project_access_set(caller uuid)
RETURNS TABLE (project_id uuid, access text)
LANGUAGE sql
STABLE
SECURITY INVOKER
PARALLEL SAFE
AS $$
    SELECT g.project_id,
           CASE
               WHEN bool_or(g.lvl = 'owner') THEN 'owner'
               WHEN bool_or(g.lvl = 'member') AND bool_or(g.lvl = 'admin') THEN 'admin_member'
               WHEN bool_or(g.lvl = 'member') THEN 'member'
               ELSE 'admin'
           END
      FROM (
        -- (a) own "Personal"; (b) implicit owner of a non-personal space,
        -- in which they are a member (an external owns no spaces).
        SELECT p.id AS project_id, 'owner'::text AS lvl
          FROM spaces s
          JOIN projects p ON p.space_id = s.id
         WHERE s.owner_id = caller
           AND EXISTS (SELECT 1 FROM users me
                        WHERE me.id = caller AND me.deactivated_at IS NULL AND me.deleted_at IS NULL)
           AND (s.kind = 'personal'
                OR (EXISTS (SELECT 1 FROM users me WHERE me.id = caller AND NOT me.is_external)
                    AND EXISTS (SELECT 1 FROM space_members sm
                                 WHERE sm.space_id = s.id AND sm.user_id = caller)))
        UNION ALL
        -- (b) membership row in a project of a non-personal space where they are
        -- a member; (c) external - membership row in a non-personal project,
        -- level member.
        SELECT pm.project_id,
               CASE WHEN me.is_external THEN 'member' ELSE pm.role END
          FROM users me
          JOIN project_members pm ON pm.user_id = me.id
          JOIN projects p ON p.id = pm.project_id
          LEFT JOIN spaces s ON s.id = p.space_id
         WHERE me.id = caller
           AND me.deactivated_at IS NULL AND me.deleted_at IS NULL
           AND s.kind IS DISTINCT FROM 'personal'
           AND (me.is_external
                OR EXISTS (SELECT 1 FROM space_members sm
                            WHERE sm.space_id = p.space_id AND sm.user_id = caller))
        UNION ALL
        -- (d) instance admin with the flag enabled (K-2): all non-personal.
        -- Both checks depend only on the caller - they run once.
        SELECT p.id, 'admin'
          FROM projects p
          LEFT JOIN spaces s ON s.id = p.space_id
         WHERE EXISTS (SELECT 1 FROM users me
                        WHERE me.id = caller AND me.deactivated_at IS NULL AND me.deleted_at IS NULL
                          AND me.role = 'admin' AND NOT me.is_external)
           AND (SELECT st.admin_enabled FROM instance_settings st WHERE st.id = 1)
           AND s.kind IS DISTINCT FROM 'personal'
      ) g
     GROUP BY g.project_id
$$;

COMMENT ON FUNCTION skein_project_access_set(uuid) IS
    'sk-674.18 K-1, T017: projects visible to caller (INV-1 a-d) with access level '
    '(owner/admin_member/member/admin). The only source of the project access rule; '
    'callers go through internal/authz.';

ANALYZE projects;
ANALYZE spaces;
ANALYZE space_members;
ANALYZE project_members;
