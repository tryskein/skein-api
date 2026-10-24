package issues

import "testing"

// TestClassifyVersionConflictOrigin pins the sk-354 classification rule,
// DB-free: the decision degradeSerializationAbort feeds into
// versionConflictOriginTotal is pure once the live version has been read,
// so it is tested here in isolation rather than only through a slower
// DB-backed repro.
func TestClassifyVersionConflictOrigin(t *testing.T) {
	five := 5
	cases := []struct {
		name            string
		liveVersion     int
		expectedVersion *int
		want            string
	}{
		{
			name:            "no expected version at all",
			liveVersion:     5,
			expectedVersion: nil,
			want:            originNoExpectedVersion,
		},
		{
			name:            "live version matches caller's — abort came from elsewhere",
			liveVersion:     5,
			expectedVersion: &five,
			want:            originSerializationDerived,
		},
		{
			name:            "live version differs from caller's — a real concurrent write landed",
			liveVersion:     6,
			expectedVersion: &five,
			want:            originGenuine,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyVersionConflictOrigin(tc.liveVersion, tc.expectedVersion); got != tc.want {
				t.Errorf("classifyVersionConflictOrigin(%d, %v) = %s, want %s",
					tc.liveVersion, tc.expectedVersion, got, tc.want)
			}
		})
	}
}
