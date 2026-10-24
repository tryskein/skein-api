-- 000073_access_relation_spaces - sk-674.18, T017 (INV-1 a-d, INV-4, INV-5,
-- INV-8, INV-23, D-2, US-6; K-1, K-2).
--
-- WHY. The new body of the access relation is the spaces rule. Signature,
-- name and COST are unchanged (000066-000068): all calls go through internal/authz,
-- so they need no changes. The point functions skein_project_access / skein_project_visible
-- read this same relation and change together with it.
--
-- RULE (INV-1). User u sees project p if and only if
-- u is active (not deactivated, not deleted) and one of the following holds:
--   (a) p is in u's "Personal" space - level owner;
--   (b) p is not in "Personal", u is not external, u is a member of p's space, and
--       u is the space owner (implicit owner, INV-4: level owner
--       without a membership row) or u has a membership row in p (level - its
--       role);
--   (c) u is external, p is not in "Personal", u has a membership row in p - level
--       member, whatever the row's role (an external is only a member);
--   (d) u is an instance admin, flag instance_settings.admin_enabled (K-2), u is not
--       external, p is not in "Personal" - level admin: see and release another's
--       lock, everything else - only by their own role (US-6).
-- There are no other paths. A project without a space (before T024 the write path still
-- creates it) is not visible by rule (b) - there is no space; (c) and (d) apply
-- as to any non-personal project (lead's decision: no special branches).
--
-- LEVEL. One row per project - the strongest of the grounds:
--   owner        - (a), implicit owner (b), membership row with role owner;
--   admin_member - membership row member (b) for an admin with branch (d):
--                  member rights plus releasing another's lock;
--   member       - membership row member (b), external (c);
--   admin        - branch (d) only.

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

-- INV-28 (4): a space member can see the count of inaccessible links and
-- children of a visible issue - only when the hidden issue lies in a project of their
-- OWN non-personal space. Own non-personal space - one where an
-- active non-external user is a member; an external is outside spaces
-- (gets no such counts). The rule is here, next to the relation; Go reads the
-- set once per request (authz.Scope.BindSpaces).
CREATE FUNCTION skein_contribution_spaces(caller uuid)
RETURNS TABLE (space_id uuid)
LANGUAGE sql
STABLE
SECURITY INVOKER
PARALLEL SAFE
AS $$
    SELECT sm.space_id
      FROM users me
      JOIN space_members sm ON sm.user_id = me.id
      JOIN spaces s ON s.id = sm.space_id
     WHERE me.id = caller
       AND me.deactivated_at IS NULL AND me.deleted_at IS NULL
       AND NOT me.is_external
       AND s.kind <> 'personal'
$$;

COMMENT ON FUNCTION skein_contribution_spaces(uuid) IS
    'sk-674.18 T017, INV-28 (4): the caller''s own non-personal spaces - hidden issues there '
    'count in unavailable* numbers; anywhere else they do not.';
