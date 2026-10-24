package instructions

import "testing"

// TestInitializeWithinCap surfaces the same byte-budget bound as the
// init() panic, but in a unit test that runs in PR CI rather than at
// first boot. Keeps the runway visible to reviewers — without this
// test, an "innocent" one-line edit to initialize.txt that pushes past
// the cap would only fail when the binary actually starts.
//
// The cap is the same value used by the runtime panic in embed.go.
func TestInitializeWithinCap(t *testing.T) {
	if got := len(Initialize); got > maxInitializeBytes {
		t.Fatalf("initialize.txt exceeds %d byte cap: got %d", maxInitializeBytes, got)
	}
}
