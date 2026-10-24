-- 000075 (down) - body of the 000073 relation.

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
          FROM users me
          JOIN spaces s ON s.owner_id = me.id
          JOIN projects p ON p.space_id = s.id
         WHERE me.id = caller
           AND me.deactivated_at IS NULL AND me.deleted_at IS NULL
           AND (s.kind = 'personal'
                OR (NOT me.is_external
                    AND EXISTS (SELECT 1 FROM space_members sm
                                 WHERE sm.space_id = s.id AND sm.user_id = me.id)))
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
                            WHERE sm.space_id = p.space_id AND sm.user_id = me.id))
        UNION ALL
        -- (d) instance admin with the flag enabled (K-2): all non-personal.
        SELECT p.id, 'admin'
          FROM users me
          JOIN projects p ON true
          LEFT JOIN spaces s ON s.id = p.space_id
         WHERE me.id = caller
           AND me.deactivated_at IS NULL AND me.deleted_at IS NULL
           AND me.role = 'admin' AND NOT me.is_external
           AND (SELECT st.admin_enabled FROM instance_settings st WHERE st.id = 1)
           AND s.kind IS DISTINCT FROM 'personal'
      ) g
     GROUP BY g.project_id
$$;

COMMENT ON FUNCTION skein_project_access_set(uuid) IS
    'sk-674.18 K-1, T017: projects visible to caller (INV-1 a-d) with access level '
    '(owner/admin_member/member/admin). The only source of the project access rule; '
    'callers go through internal/authz.';
