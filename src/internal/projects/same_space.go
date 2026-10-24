package projects

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SameSpace reports whether the projects a and b lie in one space (INV-24,
// sk-674.18 T033): a parent, a dependency, the target of a duplicate or
// superseded close and the target of a copy (skein_move) stay inside the
// space of the issue. A project that does not exist is in no space — the
// answer is false, and the caller refuses with the same answer as for a
// nonexistent object (INV-3).
func SameSpace(ctx context.Context, db dbq, a, b uuid.UUID) (bool, error) {
	if a == b {
		return true, nil
	}
	var same bool
	err := db.QueryRow(ctx, `
SELECT pa.space_id = pb.space_id
  FROM projects pa, projects pb
 WHERE pa.id = $1 AND pb.id = $2`, a, b).Scan(&same)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("comparing the spaces of projects %s and %s: %w", a, b, err)
	}
	return same, nil
}
