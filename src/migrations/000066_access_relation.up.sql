-- 000066_access_relation — sk-674.18, T006 (K-1, D-1, INV-2).
--
-- WHY. The rule "which projects the caller sees" is today written in ten
-- forms (point check, hub batch, resolve, Horizon, three Go copies of
-- materialization, search, fan-out recipients, review/digest, project
-- lists). All surfaces are moved to one relation (T007–T010), and
-- the space rules are later changed only in the body of these functions (T017).
--
-- SEMANTICS — OLD, with no behavior change:
--   * an internal admin (users.role = 'admin' AND NOT users.is_external) —
--     level 'admin' on every existing project; it wins over owner and
--     member, as in projects/access.go#projectAccessSQL;
--   * all others, an external admin included, — a project_members row:
--     its role ('owner' / 'member');
--   * an unknown id — an empty relation.
-- Deactivation and the instance_settings.admin_enabled flag are deliberately not
-- included here: the point forms of the rule do not read them today. The new body per INV-1
-- (activity, flag, spaces) is T017.
--
-- FORM (K-1).
--   * LANGUAGE sql, STABLE, SECURITY INVOKER, not STRICT, a single SELECT
--     expression — the conditions under which the planner inlines a relation function
--     into the calling query: `JOIN skein_project_access_set($n) va ON
--     va.project_id = x.project_id` is planned as an ordinary subquery by
--     indexes, without a Function Scan per row.
--   * The role and the external flag are read from users by the caller id, and are not
--     accepted as a parameter: the caller cannot pass themselves admin.
--   * The point skein_project_access takes its answer from the same relation, so the
--     predicate is one; 'not_found' / 'forbidden' are told apart only by the presence of a
--     projects row.

CREATE FUNCTION skein_project_access_set(caller uuid)
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

CREATE FUNCTION skein_project_access(caller uuid, project uuid)
RETURNS text
LANGUAGE sql
STABLE
SECURITY INVOKER
PARALLEL SAFE
AS $$
    SELECT COALESCE(
        (SELECT va.access
           FROM skein_project_access_set(caller) va
          WHERE va.project_id = project),
        CASE WHEN EXISTS (SELECT 1 FROM projects p WHERE p.id = project)
             THEN 'forbidden'
             ELSE 'not_found'
        END)
$$;

COMMENT ON FUNCTION skein_project_access(uuid, uuid) IS
    'sk-674.18 K-1: access level of caller on one project: admin/owner/member, '
    'or not_found / forbidden. Same predicate as skein_project_access_set.';
