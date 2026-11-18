package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"skein-api/internal/auth"
	"skein-api/internal/themes"
)

// sk-977.7.10: the concurrency slots of POST …/me/theme/inspect belong to
// the replica, not to a prefix — through the router buildRouter assembles,
// every slot taken on internalAPIPrefix leaves none on the transitional
// prefix.
func TestInspectGate_SlotsShareBothPrefixes(t *testing.T) {
	s := newWiringStack(t)
	slots := themes.DefaultInspectLimits().MaxInFlight
	writers := make([]*io.PipeWriter, 0, slots)
	done := make(chan int, slots)
	t.Cleanup(func() {
		for _, w := range writers {
			_ = w.Close()
		}
	})
	request := func(prefix string, body io.Reader) *http.Request {
		req := httptest.NewRequest(http.MethodPost, prefix+"/me/theme/inspect", body)
		req.Header.Set(auth.HeaderInternalSecret, wiringInternalSecret)
		return req
	}
	// Every slot is held by a request whose body is still arriving.
	for i := 0; i < slots; i++ {
		pr, pw := io.Pipe()
		writers = append(writers, pw)
		req := request(internalAPIPrefix, pr)
		req.ContentLength = -1
		go func() {
			rec := httptest.NewRecorder()
			s.router.ServeHTTP(rec, req)
			done <- rec.Code
		}()
	}
	// The slots are taken once the gate is reading each body; poll for the
	// refusal on the other prefix.
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec := httptest.NewRecorder()
		s.router.ServeHTTP(rec, request(transitionalInternalAPIPrefix, strings.NewReader(`{"selector":".x"}`)))
		if rec.Code == http.StatusTooManyRequests {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("with %d slots taken on %s, %s answered %d, want 429", slots, internalAPIPrefix, transitionalInternalAPIPrefix, rec.Code)
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, w := range writers {
		_ = w.Close()
	}
	writers = nil
	for i := 0; i < slots; i++ {
		<-done
	}
}
