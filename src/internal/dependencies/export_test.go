package dependencies

import (
	"context"
	"testing"
)

// SetDeleteGuardSeamForTest installs f as the seam between Delete's
// outcome-link guard and its DELETE for the duration of t, restoring the
// previous value on cleanup. Test-only: this file is compiled into the
// package's test binary alone, so production code cannot reach the seam.
func SetDeleteGuardSeamForTest(t *testing.T, f func(ctx context.Context, fromID, toID, depType string)) {
	t.Helper()
	prev := deleteGuardSeam
	deleteGuardSeam = f
	t.Cleanup(func() { deleteGuardSeam = prev })
}
