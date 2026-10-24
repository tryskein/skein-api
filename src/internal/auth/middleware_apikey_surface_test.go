package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/apikeys"
)

// TestRequireAPIKey_SurfaceGate is the fail-closed surface gate (sk-163 S7a,
// spec §9). It proves, on BOTH the cold (DB lookup + bcrypt) and warm (cache
// hit) paths, that:
//
//   - an EXTERNAL-owner key with NO internal marker (public surface) is ALLOWED;
//   - an INTERNAL-owner key with NO internal marker (public surface) → 403;
//   - both keys WITH the internal marker are ALLOWED.
//
// The warm path is the footgun: is_external must ride in the cache entry (S1)
// so the second request is gated identically to the first.
func TestRequireAPIKey_SurfaceGate(t *testing.T) {
	pool := newOIDCTestPool(t) // skips when TEST_DATABASE_URL is unset
	ctx := context.Background()
	store := apikeys.New(pool)

	// seed inserts a user (external or internal) + a real api key, returns
	// the plaintext key and registers cleanup.
	seed := func(external bool) string {
		uid := uuid.Must(uuid.NewRandom())
		uname := "sfc" + strings.ReplaceAll(uid.String()[:18], "-", "")
		if _, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, is_external, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', 'h', $5, now(), '{}'::jsonb)`,
			uid, uname+"@t.l", uname, uname, external); err != nil {
			t.Fatalf("seed user (external=%v): %v", external, err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, uid)
		})
		plain, prefix, hash, err := GenerateAPIKey(MinBcryptCost)
		if err != nil {
			t.Fatalf("GenerateAPIKey: %v", err)
		}
		if _, _, err := store.Create(ctx, uid, "surface-test", prefix, hash); err != nil {
			t.Fatalf("Create key: %v", err)
		}
		return plain
	}

	externalKey := seed(true)
	internalKey := seed(false)

	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// do issues one request against a fresh-cache middleware instance and
	// returns the status code. surfaceMarker="" omits the header (public).
	do := func(mw http.Handler, key, surfaceMarker string) int {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set(HeaderAPIKey, key)
		if surfaceMarker != "" {
			req.Header.Set(HeaderSurface, surfaceMarker)
		}
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)
		return rec.Code
	}

	cases := []struct {
		name    string
		key     string
		surface string
		want    int
	}{
		{"external key, public surface (no marker) → allowed", externalKey, "", http.StatusOK},
		{"internal key, public surface (no marker) → 403", internalKey, "", http.StatusForbidden},
		{"internal key, internal marker → allowed", internalKey, SurfaceInternal, http.StatusOK},
		{"external key, internal marker → allowed", externalKey, SurfaceInternal, http.StatusOK},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Fresh middleware → fresh cache, so the first call is the COLD
			// path (DB + bcrypt) and the second is the WARM path (cache hit).
			mw := RequireAPIKey(pool, MinBcryptCost, "")(probe)

			if got := do(mw, tc.key, tc.surface); got != tc.want {
				t.Fatalf("cold: got %d, want %d", got, tc.want)
			}
			if got := do(mw, tc.key, tc.surface); got != tc.want {
				t.Fatalf("warm (cache hit): got %d, want %d", got, tc.want)
			}
		})
	}
}
