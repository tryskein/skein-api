package search_test

// query_cancel_test.go — a search whose caller goes away while the query
// is being embedded returns the cancellation and logs no "search
// degraded" warning: the embedder did not fail, the client left.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"skein-api/internal/search"
	"skein-api/internal/testsupport"
)

func TestSearch_CallerCancelDuringEmbedIsNotDegradation(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")

	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The caller leaves while the embedding is in flight.
		cancel()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(server.Close)
	// Cleanups run last-in first-out: the handler is released before
	// Close waits for it, whether or not the client closed its connection.
	t.Cleanup(func() { close(release) })
	embedder, err := search.NewEmbedder(search.EmbedConfig{
		BaseURL: server.URL, Model: "fixture", Dim: 1024, SearchTimeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := search.NewService(search.NewStore(pool), embedder, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	res, err := svc.Search(reqCtx, search.Params{
		Query:    token(t),
		Kinds:    []search.Kind{search.KindIssue},
		Limit:    10,
		CallerID: owner,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Search = %+v, %v; want context.Canceled", res, err)
	}
	if out := buf.String(); strings.Contains(out, "search degraded") {
		t.Errorf("a cancelled search was logged as degraded:\n%s", out)
	}
}
