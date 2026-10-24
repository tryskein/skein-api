-- 000073_access_relation_spaces (down) - body of the 000066 relation (old
-- semantics: the admin sees everything, everyone else - by membership rows).

DROP FUNCTION IF EXISTS skein_contribution_spaces(uuid);

CREATE OR REPLACE FUNCTION skein_project_access_set(caller uuid)
RETURNS TABLE (project_id uuid, access text)
LANGUAGE sql
STABLE
SECURITY INVOKER
PARALLEL SAFE
AS $$
    SELECT p.id, 'admin'::text
      FROM users u
      JOIN projects p ON true
     WHERE u.id = caller
       AND u.role = 'admin'
       AND NOT u.is_external
    UNION ALL
    SELECT pm.project_id, pm.role
      FROM project_members pm
      JOIN users u ON u.id = pm.user_id
     WHERE pm.user_id = caller
       AND NOT (u.role = 'admin' AND NOT u.is_external)
$$;

COMMENT ON FUNCTION skein_project_access_set(uuid) IS
    'sk-674.18 K-1: projects visible to caller with access level (admin/owner/member). '
    'The only source of the project access rule; callers go through internal/authz.';
