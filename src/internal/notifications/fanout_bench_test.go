// fanout_bench_test.go — allocation benchmarks for the notification hot
// paths (audit sk-194, finding opt-A14-020).
//
// These run on EVERY notifiable event, once per event and (for the field
// helpers) once per recipient, so their allocation profile is what shows up
// under fan-out load. Deliberately DB-free: the point is to have numbers
// available in the plain unit job, not only where TEST_DATABASE_URL is set.
//
// Run:
//
//	go test -run '^$' -bench . -benchmem ./internal/notifications/...
//
// b.ReportAllocs() is on everywhere — allocs/op is the metric these were
// written for; ns/op on a laptop/container is noise by comparison.

package notifications

import (
	"encoding/json"
	"strings"
	"testing"
)

// changesPayload builds an issue.updated payload with n changed fields,
// mixing notifiable and non-notifiable ones so the filter actually filters.
func changesPayload(tb testing.TB, n int) []byte {
	tb.Helper()
	fields := []string{"status", "assignee", "priority", "title", "description", "notes"}
	type change struct {
		Field    string `json:"field"`
		OldValue string `json:"oldValue"`
		NewValue string `json:"newValue"`
	}
	changes := make([]change, 0, n)
	for i := range n {
		changes = append(changes, change{
			Field:    fields[i%len(fields)],
			OldValue: "before",
			NewValue: "after",
		})
	}
	b, err := json.Marshal(map[string]any{"changes": changes})
	if err != nil {
		tb.Fatalf("marshalling payload: %v", err)
	}
	return b
}

func BenchmarkNotifiableFieldsFromPayload(b *testing.B) {
	// 1 change is the common single-field PATCH; 6 is a wide bulk edit.
	for _, n := range []int{1, 6} {
		payload := changesPayload(b, n)
		b.Run(sizeName(n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = notifiableFieldsFromPayload(payload)
			}
		})
	}
}

func BenchmarkMentionFieldsFromPayload(b *testing.B) {
	payload := changesPayload(b, 6)
	b.ReportAllocs()
	for b.Loop() {
		_ = mentionFieldsFromPayload(payload)
	}
}

func BenchmarkChangedFieldsFromPayload(b *testing.B) {
	payload := changesPayload(b, 3)
	b.ReportAllocs()
	for b.Loop() {
		_ = changedFieldsFromPayload(payload)
	}
}

func BenchmarkFormatSummary(b *testing.B) {
	payload := changesPayload(b, 3)
	b.ReportAllocs()
	for b.Loop() {
		_ = FormatSummary("issue.updated", "alice", "sk-194.5.2", payload)
	}
}

// BenchmarkExtractMentionContext covers the snippet builder that runs once
// per mention notification row. The long case is the one that matters: it
// walks a KB-sized comment body looking for the mention marker.
func BenchmarkExtractMentionContext(b *testing.B) {
	short := "hey @alice could you look at this?"
	long := strings.Repeat("lorem ipsum dolor sit amet consectetur ", 30) +
		"\n\n\they @alice could you look at this?\n\n" +
		strings.Repeat("sed do eiusmod tempor incididunt ", 30)

	for _, tc := range []struct {
		name string
		body string
	}{
		{"short", short},
		{"long_2kb", long},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = extractMentionContext(tc.body, "alice")
			}
		})
	}
}

func sizeName(n int) string {
	if n == 1 {
		return "1_change"
	}
	return "6_changes"
}
