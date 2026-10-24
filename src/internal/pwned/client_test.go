package pwned

import (
	"context"
	"crypto/sha1" //nolint:gosec // test fixture computation, mirrors the k-anonymity contract.
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// hashOf mirrors Client.Check's own hashing so tests can build a fixture
// suffix for a known password without hard-coding a SHA-1 hex string.
func hashOf(t *testing.T, password string) (prefix, suffix string) {
	t.Helper()
	sum := sha1.Sum([]byte(password)) //nolint:gosec // test fixture, not security-sensitive.
	hexSum := strings.ToUpper(hex.EncodeToString(sum[:]))
	return hexSum[:5], hexSum[5:]
}

func TestCheck_MatchFound(t *testing.T) {
	_, suffix := hashOf(t, "password")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(suffix + ":3730471\r\nOTHERSUFFIXNOTMATCHING1234567890AB:1\r\n"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, 2*time.Second)
	breached, err := c.Check(context.Background(), "password")
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if !breached {
		t.Fatal("expected breached=true, got false")
	}
}

func TestCheck_NoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA:1\r\nBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB:2\r\n"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, 2*time.Second)
	breached, err := c.Check(context.Background(), "a genuinely unique passphrase 2026")
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if breached {
		t.Fatal("expected breached=false, got true")
	}
}

func TestCheck_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, 2*time.Second)
	_, err := c.Check(context.Background(), "password")
	if err == nil {
		t.Fatal("expected error on 500 status, got nil")
	}
	if !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("expected ErrServiceUnavailable, got %v", err)
	}
}

func TestCheck_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA:1\r\n"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, 20*time.Millisecond)
	start := time.Now()
	_, err := c.Check(context.Background(), "password")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("expected ErrServiceUnavailable, got %v", err)
	}
	if elapsed > 150*time.Millisecond {
		t.Fatalf("Check did not respect the configured timeout: took %v", elapsed)
	}
}

func TestCheck_AddPaddingHeaderSent(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("Add-Padding")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA:1\r\n"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, 2*time.Second)
	if _, err := c.Check(context.Background(), "password"); err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if gotHeader != "true" {
		t.Fatalf("expected Add-Padding: true header, got %q", gotHeader)
	}
}

func TestCheck_PrefixUppercaseHex(t *testing.T) {
	wantPrefix, _ := hashOf(t, "password")

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA:1\r\n"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, 2*time.Second)
	if _, err := c.Check(context.Background(), "password"); err != nil {
		t.Fatalf("Check returned error: %v", err)
	}

	wantPath := "/range/" + wantPrefix
	if gotPath != wantPath {
		t.Fatalf("expected request path %q, got %q", wantPath, gotPath)
	}
	if len(wantPrefix) != 5 {
		t.Fatalf("prefix fixture is not 5 chars: %q", wantPrefix)
	}
	for _, r := range wantPrefix {
		if !strings.ContainsRune("0123456789ABCDEF", r) {
			t.Fatalf("prefix %q is not uppercase hex", wantPrefix)
		}
	}
}
