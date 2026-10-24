package authz

// People by INV-9 (sk-674.18 T027) — the one rule of which person a caller
// may see, in the directory, the profile and the people of an issue's
// context. Like the recipient fragments, these render SQL booleans over the
// caller's query; the caller's role and external flag are read from users by
// id, never taken from the request (D-2).
//
// INV-9: a person v is visible to a caller u only if v is a participant of
// one of u's spaces, or v is an external user who is a member of a project
// u can access; an external u sees only the co-members of his projects. An
// instance admin as a VIEWER has no exception here (the decision// 2026-10-14): «u's spaces» are those u participates in; the full list of
// people is the admin screen (GET /users). As a PERSON in the context of a
// non-personal project he belongs to it (PersonInProjectExpr, sk-741).

// PersonVisibleExpr renders the SQL boolean "the person personExpr is
// visible to the viewer viewerExpr" (INV-9, outside the context of one
// project: the directory, the profile). A viewer always sees himself.
func PersonVisibleExpr(viewerExpr, personExpr string) string {
	return "(" + personExpr + " = " + viewerExpr + `
 OR (EXISTS (SELECT 1 FROM users u_pv WHERE u_pv.id = ` + viewerExpr + ` AND NOT u_pv.is_external)
     AND (EXISTS (SELECT 1 FROM space_members sm_pv
                    JOIN space_members sm_pp ON sm_pp.space_id = sm_pv.space_id
                   WHERE sm_pv.user_id = ` + viewerExpr + ` AND sm_pp.user_id = ` + personExpr + `)
          OR (EXISTS (SELECT 1 FROM users u_pp WHERE u_pp.id = ` + personExpr + ` AND u_pp.is_external)
              AND EXISTS (SELECT 1 FROM project_members pm_pp
                           WHERE pm_pp.user_id = ` + personExpr + `
                             AND ` + RecipientVisibleExpr(viewerExpr, "pm_pp.project_id") + `))))
 OR (EXISTS (SELECT 1 FROM users u_pe WHERE u_pe.id = ` + viewerExpr + ` AND u_pe.is_external)
     AND EXISTS (SELECT 1 FROM project_members pm_pe
                  JOIN projects p_pe ON p_pe.id = pm_pe.project_id
                  JOIN spaces s_pe ON s_pe.id = p_pe.space_id
                 WHERE pm_pe.user_id = ` + viewerExpr + `
                   AND (s_pe.owner_id = ` + personExpr + `
                        OR EXISTS (SELECT 1 FROM project_members pm_pc
                                    WHERE pm_pc.project_id = pm_pe.project_id
                                      AND pm_pc.user_id = ` + personExpr + `)))))`
}

// PersonInProjectExpr renders the SQL boolean "the person personExpr
// belongs to the context of the project projectExpr" (INV-9, "in the context
// of the task - in its project"): a participant of the project's space, an
// external member of the project, or the effective instance admin when the
// project is not in a "Personal" — the admin's path d of INV-1, levels admin
// and admin_member (the maintainers' decision 2026-10-17, sk-741: the admin
// works there with the owner's rights, so he is no outsider of it). It
// decides whom a mention of an issue resolves to (together with the
// recipient filter: E-21), and whom the issue's people mark as departed — a
// person outside it has left the space of this context. Another user's
// "Personal" never puts the admin in its context.
//
// The external leg reads the person's own row by a scalar subquery inside
// the membership probe, not a join of project_members with users: the
// joined form is a plain correlated EXISTS, which the planner may run as a
// hashed SubPlan — a scan of every external membership of the instance,
// however few rows need marks (sk-674.18 EXPLAIN gate: ≈10 ms on each
// dashboard feed and projects read). With the person's id referenced inside
// a nested subquery the EXISTS cannot be hashed and stays a probe of
// project_members_pkey per row. The admin leg probes projects and spaces by
// primary key and the person's users row (EffectiveAdminExpr).
func PersonInProjectExpr(personExpr, projectExpr string) string {
	return `(EXISTS (SELECT 1 FROM projects p_pc
                   JOIN space_members sm_pc ON sm_pc.space_id = p_pc.space_id
                  WHERE p_pc.id = ` + projectExpr + ` AND sm_pc.user_id = ` + personExpr + `)
 OR EXISTS (SELECT 1 FROM project_members pm_px
             WHERE pm_px.project_id = ` + projectExpr + ` AND pm_px.user_id = ` + personExpr + `
               AND (SELECT u_px.is_external FROM users u_px WHERE u_px.id = ` + personExpr + `))
 OR (EXISTS (SELECT 1 FROM projects p_pa JOIN spaces s_pa ON s_pa.id = p_pa.space_id
              WHERE p_pa.id = ` + projectExpr + ` AND s_pa.kind <> 'personal')
     AND ` + EffectiveAdminExpr(personExpr) + `))`
}

// PersonLinkedToIssueExpr renders the SQL boolean "the person personExpr has
// a historical relation to the issue row issueAlias" (INV-30, sk-674.18
// T028): the second ground on which an assignee is shown (name, «left»,
// pseudonym) besides belonging to the issue's context. The relation is read
// from what the schema keeps (the decision of 2026-10-14): the
// issue's author, its previous resolved assignees (issues.previous_assignees
// — the loss of access moves the assignee there, INV-12), its commenters and
// the actors of its events. A closed issue's assignee who never acted on it
// and left is shown as text — the error is toward hiding, never toward
// revealing an account. Former membership or deletion alone is no relation.
func PersonLinkedToIssueExpr(personExpr, issueAlias string) string {
	return "(" + issueAlias + ".created_by = " + personExpr +
		" OR " + personExpr + " = ANY(" + issueAlias + ".previous_assignees)" +
		" OR EXISTS (SELECT 1 FROM comments c_li WHERE c_li.issue_id = " + issueAlias + ".id AND c_li.author_user_id = " + personExpr + ")" +
		" OR EXISTS (SELECT 1 FROM events e_li WHERE e_li.entity_type = 'issue' AND e_li.entity_id = " + issueAlias + ".id" +
		" AND e_li.actor_user_id = " + personExpr + "))"
}
