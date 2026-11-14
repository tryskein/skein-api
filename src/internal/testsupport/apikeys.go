package testsupport

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
)

// KeyTTLPolicy is the key lifetime policy of the test stacks: the
// configuration's defaults (90 days, at most 365).
var KeyTTLPolicy = apikeys.TTLPolicy{Default: 90 * 24 * time.Hour, Max: 365 * 24 * time.Hour}

// CreateOwnerKey stores a key for userID with the grant a key created before
// grants existed carries after the migration (US-13) — level manage, the
// admin flag when the owner is an internal instance admin, scope all — and
// the KeyTTLPolicy default lifetime. Tests that create a key to act as its
// owner use it, so the key can do what the owner can. It returns the key id
// and creation time; prefix and hash come from auth.GenerateAPIKey.
func CreateOwnerKey(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, label, prefix, hash string) (uuid.UUID, time.Time, error) {
	var admin bool
	if err := pool.QueryRow(ctx,
		`SELECT role = 'admin' AND NOT is_external FROM users WHERE id = $1`, userID).Scan(&admin); err != nil {
		return uuid.Nil, time.Time{}, err
	}
	c, err := apikeys.New(pool).Create(ctx, apikeys.NewKey{
		UserID: userID, Label: label, Prefix: prefix, Hash: hash,
		Grant: apikeys.Grant{Level: apikeys.LevelManage, Admin: admin, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}},
	}, KeyTTLPolicy)
	return c.ID, c.CreatedAt, err
}
