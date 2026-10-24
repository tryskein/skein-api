package authz

// spaces.go — who sees a space and who is the effective instance admin
// (sk-674.18 T023, US-6, INV-3, E-14, E-15, E-16).
//
// The rule of seeing a SPACE lives here, next to the rule of seeing a
// project (the relation of migration 000066/000075), so no surface of the
// spaces feature decides it on its own:
//
//   - the caller is active (not deactivated, not deleted — E-15: a
//     deactivated participant keeps his rows but sees nothing);
//   - and either he is a participant of the space (space_members — the
//     owner always is, INV-10),
//   - or the space is not a "Personal" and he is the effective instance admin:
//     role admin, not external, while instance_settings.admin_enabled
//     (US-6, K-2; demoted or flag off — the reach is gone at once, E-14).
//
// An external user is in no space (E-16) and is never the effective admin,
// so he sees none.

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RowQuerier is the part of a pool or a transaction that reads one row.
type RowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// EffectiveAdminExpr renders the SQL boolean «the user callerPlaceholder is
// the effective instance admin»: active, role admin, not external, and the
// admin role enabled on the installation.
func EffectiveAdminExpr(callerPlaceholder string) string {
	return fmt.Sprintf(`EXISTS (SELECT 1 FROM users adm, instance_settings st
                WHERE adm.id = %[1]s AND st.id = 1 AND st.admin_enabled
                  AND adm.role = 'admin' AND NOT adm.is_external
                  AND adm.deactivated_at IS NULL AND adm.deleted_at IS NULL)`, callerPlaceholder)
}

// SpaceSeenExpr renders the SQL boolean «the user callerPlaceholder sees the
// space of row alias spaceAlias» (the rule in the file header).
func SpaceSeenExpr(spaceAlias, callerPlaceholder string) string {
	return fmt.Sprintf(`(EXISTS (SELECT 1 FROM users seer
                 WHERE seer.id = %[2]s AND seer.deactivated_at IS NULL AND seer.deleted_at IS NULL)
     AND (EXISTS (SELECT 1 FROM space_members seen
                   WHERE seen.space_id = %[1]s.id AND seen.user_id = %[2]s)
          OR (%[1]s.kind <> 'personal' AND %[3]s)))`,
		spaceAlias, callerPlaceholder, EffectiveAdminExpr(callerPlaceholder))
}

// IsEffectiveAdmin reports whether user is the effective instance admin now
// (EffectiveAdminExpr), read from the database — never from a header.
func IsEffectiveAdmin(ctx context.Context, q RowQuerier, user uuid.UUID) (bool, error) {
	var ok bool
	if err := q.QueryRow(ctx, `SELECT `+EffectiveAdminExpr("$1"), user).Scan(&ok); err != nil {
		return false, fmt.Errorf("authz: reading whether %s is the instance admin: %w", user, err)
	}
	return ok, nil
}
