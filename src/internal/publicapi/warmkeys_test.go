package publicapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/publicapi"
)

// TestWarmKeys_WindowAndEviction — row 60 (OST-2-3). A key is warm for 15
// minutes after its last full successful authentication, then cold; at the
// cap the entry closest to expiry is evicted. Time is the injected clock.
func TestWarmKeys_WindowAndEviction(t *testing.T) {
	clock := newFakeClock()
	warm := publicapi.NewWarmKeys(15*time.Minute, 2, clock.Now)

	if warm.IsWarm("ska_a") {
		t.Fatal("a key never verified is warm")
	}
	warm.Verified("ska_a")
	clock.Advance(15*time.Minute - time.Second)
	if !warm.IsWarm("ska_a") {
		t.Fatal("a key verified 14:59 ago is cold")
	}
	clock.Advance(time.Second)
	if warm.IsWarm("ska_a") {
		t.Fatal("a key verified 15:00 ago is still warm")
	}

	warm.Verified("ska_a") // t0
	clock.Advance(time.Minute)
	warm.Verified("ska_b") // t0+1m
	clock.Advance(time.Minute)
	warm.Verified("ska_c") // at the cap: ska_a expires first
	if warm.IsWarm("ska_a") {
		t.Error("the entry closest to expiry survived an insert at the cap")
	}
	if !warm.IsWarm("ska_b") || !warm.IsWarm("ska_c") {
		t.Error("an entry farther from expiry was evicted")
	}
}

// stubAuthenticator answers AuthenticateAPIKey from a switchable outcome.
type stubAuthenticator struct {
	mu  sync.Mutex
	out auth.APIKeyOutcome
}

func (s *stubAuthenticator) set(out auth.APIKeyOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out = out
}

func (s *stubAuthenticator) AuthenticateAPIKey(context.Context, string) auth.APIKeyOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.out
}

func (s *stubAuthenticator) TouchLastUsed(context.Context, uuid.UUID) error { return nil }

// noExecutors registers nothing.
type noExecutors struct{}

func (noExecutors) TouchExecutor(context.Context, uuid.UUID, uuid.UUID, string) (string, error) {
	return "", nil
}

// TestWarmKeys_OnlyASuccessWarmsAKey — row 60, the filling rule, through the
// chain: a key whose authentication was rejected does not become warm, so
// once its address is over the failed-authentication limit the same key,
// now valid, is refused 429 as a cold one.
func TestWarmKeys_OnlyASuccessWarmsAKey(t *testing.T) {
	clock := newFakeClock()
	authn := &stubAuthenticator{out: auth.APIKeyOutcome{Kind: auth.APIKeyRejected, Reason: auth.RejectMismatch}}
	chain := publicapi.NewChain(publicapi.ChainOptions{
		Authenticator: authn,
		Executors:     noExecutors{},
		ClientIP:      publicapi.NewClientIP(nil),
		WarmKeys:      publicapi.NewWarmKeys(15*time.Minute, 100, clock.Now),
		AuthFailures:  publicapi.NewWindowLimiter(2, time.Minute, 100, clock.Now),
		Requests:      publicapi.NewWindowLimiter(100, time.Minute, 100, clock.Now),
	})
	h := publicapi.NewRouter(publicapi.RouterOptions{OperationTimeout: 30 * time.Second, Chain: chain}, []publicapi.Route{{
		OperationID: "testRead", Method: http.MethodGet, Path: "/api/v1/things",
		Auth: publicapi.AuthKey, Key: authz.KeyRequirement{Right: authz.KeyRightRead},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }),
	}})
	call := func() int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/things", nil)
		req.RemoteAddr = "203.0.113.9:5000"
		req.Header.Set(auth.HeaderAPIKey, "ska_presented")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	for i := 1; i <= 2; i++ {
		if got := call(); got != http.StatusUnauthorized {
			t.Fatalf("rejected attempt %d = %d, want 401", i, got)
		}
	}
	authn.set(auth.APIKeyOutcome{Kind: auth.APIKeyAuthenticated, Key: apikeys.KeyStatus{
		KeyID: uuid.New(), UserID: uuid.New(), UserRole: "user", OwnerActive: true,
		Grant: apikeys.Grant{Level: apikeys.LevelRead, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}},
	}})
	if got := call(); got != http.StatusTooManyRequests {
		t.Errorf("the rejected key, now valid, from the address over the limit = %d, want 429 (cold)", got)
	}
}
