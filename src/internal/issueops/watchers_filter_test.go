package issueops_test

// watchers_filter_test.go — the limits of a subscription filter being
// created (sk-977.7.73): at most 50 values in each array, each label by the
// rule of issue labels, at most 16 KiB encoded. A refusal is validation_error
// with the case in `code`, answered before the store is reached.

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/issueops"
	"skein-api/internal/notifications"
)

// repeatStrings is n copies of s as the loose JSON array a tool argument is.
func repeatStrings(s string, n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = s
	}
	return out
}

// TestCreateWatchFilter_RefusesOverLimitFilters — the operation of MCP
// filter_create and of the public createWatchFilter refuses a filter over a
// limit with validation_error and the case, before any store is touched
// (the Deps here hold no store: reaching it would panic).
func TestCreateWatchFilter_RefusesOverLimitFilters(t *testing.T) {
	pid := uuid.NewString()
	for _, tc := range []struct {
		name      string
		filter    map[string]any
		wantCase  string
		wantField string
	}{
		{"51 labels", map[string]any{"labels": repeatStrings("bug", 51)}, "filter_too_many_values", "labels"},
		{"51 types", map[string]any{"types": repeatStrings("bug", 51)}, "filter_too_many_values", "types"},
		{"51 projects", map[string]any{"projects": repeatStrings(pid, 51)}, "filter_too_many_values", "projects"},
		{"51 components", map[string]any{"components": repeatStrings(pid, 51)}, "filter_too_many_values", "components"},
		{"51 event types", map[string]any{"event_types": repeatStrings("issue.created", 51)}, "filter_too_many_values", "event_types"},
		{"label of 101 characters", map[string]any{"labels": []any{strings.Repeat("a", 101)}}, "invalid_label", "labels"},
		{"blank label", map[string]any{"labels": []any{"  "}}, "invalid_label", "labels"},
		// One type well within the size limits but outside the closed set.
		{"type outside the set", map[string]any{"types": []any{strings.Repeat("t", 1000)}}, "invalid_type", "types"},
		{"type in another case", map[string]any{"types": []any{"Bug"}}, "invalid_type", "types"},
		// 50 labels of 100 four-byte characters: each label and each count
		// is within its own limit, the whole is about 20 KB.
		{"over 16 KiB encoded", map[string]any{"labels": repeatStrings(strings.Repeat("\U0001F600", 100), 50)}, "filter_too_large", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dto, f := issueops.CreateWatchFilter(context.Background(), issueops.Deps{}, uuid.New(),
				issueops.Args{"name": "limits", "filter": tc.filter})
			if f == nil {
				t.Fatalf("filter accepted (%+v), want a refusal", dto)
			}
			if f.Code != "validation_error" {
				t.Fatalf("code = %q, want validation_error (%s)", f.Code, f.Message)
			}
			if got := f.Fields["code"]; got != tc.wantCase {
				t.Fatalf("case = %v, want %s (fields %v)", got, tc.wantCase, f.Fields)
			}
			if tc.wantField != "" && f.Fields["field"] != tc.wantField {
				t.Fatalf("field = %v, want %s", f.Fields["field"], tc.wantField)
			}
		})
	}
}

// TestValidateCustomWatcherFilter_AcceptsLimits — a filter at every limit
// of ordinary values is accepted: 50 values in each array, labels of 100
// characters; labels are stored trimmed, as issue labels are.
func TestValidateCustomWatcherFilter_AcceptsLimits(t *testing.T) {
	fill := func(s string) []string {
		out := make([]string, 50)
		for i := range out {
			out[i] = s
		}
		return out
	}
	prio := 1000
	in := notifications.CustomWatcherFilter{
		EventTypes:  fill("issue.status_changed"),
		Projects:    fill(uuid.NewString()),
		Types:       fill("decision"),
		Labels:      append(fill(strings.Repeat("a", 100))[:49], "  release  "),
		Components:  fill(uuid.NewString()),
		PriorityMax: &prio,
	}
	out, ferr := issueops.ValidateCustomWatcherFilter(in)
	if ferr != nil {
		t.Fatalf("refused: %s (%v)", ferr.Message, ferr.Details())
	}
	if got := out.Labels[49]; got != "release" {
		t.Fatalf("label stored as %q, want it trimmed to %q", got, "release")
	}
}
