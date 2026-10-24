// responses_bench_test.go — allocation benchmarks for the REST response
// hot path (audit sk-194, finding opt-A04-021).
//
// Every 200 in the API goes through writeJSON, so its per-call allocation
// cost is multiplied by the whole request volume. DB-free on purpose: these
// numbers must be available in the plain unit job.
//
// Run:
//
//	go test -run '^$' -bench . -benchmem ./internal/handler/...

package handler

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// discardWriter is a no-op http.ResponseWriter. httptest.NewRecorder would
// buffer every body and dominate the measurement — we want writeJSON's own
// allocations, not the recorder's.
type discardWriter struct{ h http.Header }

func (d *discardWriter) Header() http.Header {
	if d.h == nil {
		d.h = make(http.Header, 1)
	}
	return d.h
}
func (d *discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (d *discardWriter) WriteHeader(int)             {}

func BenchmarkWriteJSON(b *testing.B) {
	// Three shapes spanning what handlers actually return: a tiny ack, a
	// mid-size object, and a list page.
	type ack struct {
		OK bool   `json:"ok"`
		ID string `json:"id"`
	}
	type row struct {
		ID       string   `json:"id"`
		Title    string   `json:"title"`
		Status   string   `json:"status"`
		Priority int      `json:"priority"`
		Labels   []string `json:"labels"`
	}

	small := ack{OK: true, ID: uuid.New().String()}

	mid := row{
		ID: "sk-194.5.2", Title: "[REM] auto · main — autonomous fix",
		Status: "open", Priority: 2, Labels: []string{"rem", "audit"},
	}

	rows := make([]row, 0, 50)
	for i := range 50 {
		rows = append(rows, row{
			ID: "sk-194." + uuid.New().String()[:8], Title: "issue title here",
			Status: "open", Priority: i % 5, Labels: []string{"a", "b"},
		})
	}
	page := struct {
		Items []row `json:"items"`
		Total int   `json:"total"`
	}{Items: rows, Total: len(rows)}

	for _, tc := range []struct {
		name string
		body any
	}{
		{"ack", small},
		{"single_row", mid},
		{"page_50_rows", page},
	} {
		b.Run(tc.name, func(b *testing.B) {
			w := &discardWriter{}
			b.ReportAllocs()
			for b.Loop() {
				writeJSON(w, http.StatusOK, tc.body)
			}
		})
	}
}
