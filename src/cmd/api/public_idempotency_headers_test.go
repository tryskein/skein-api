package main

// public_idempotency_headers_test.go — a repeat under the same
// Idempotency-Key is the first answer: the same status, body and headers —
// Location of a creation and ETag included — for every operation that
// accepts the key, with and without Prefer: return=minimal, and also when
// the process failed between the commit and the stored body (the outcome is
// then answered from the recorded id and version). The reference is US-7 of
// the public API and the defect report: the repeat of createIssue answered
// 201 with the body but without Location.

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/apikeys"
)

// sameHeaders asserts that again carries exactly the headers of first.
func sameHeaders(t *testing.T, what string, first, again answer) {
	t.Helper()
	names := map[string]bool{}
	for k := range first.header {
		names[k] = true
	}
	for k := range again.header {
		names[k] = true
	}
	for k := range names {
		if !slices.Equal(first.header.Values(k), again.header.Values(k)) {
			t.Errorf("%s: header %s of the repeat %q, of the first answer %q", what, k, again.header.Values(k), first.header.Values(k))
		}
	}
}

func TestIdempotencyOps_RepeatKeepsHeaders(t *testing.T) {
	w := newClassWorld(t)
	key := w.key(w.owner, levelKey(apikeys.LevelWrite))
	proj := "/api/v1/projects/" + w.p.String()
	idem := func() map[string]string { return map[string]string{"Idempotency-Key": "k-" + uuid.NewString()} }
	minimal := map[string]string{"Prefer": "return=minimal"}
	closeCall := func() opCall {
		id := w.freshIssue(w.p)
		return opCall{http.MethodPost, "/api/v1/issues/" + id + "/close", map[string]any{"reason": "done", "closeOutcome": "done"},
			map[string]string{"If-Match": `"` + strconv.Itoa(w.issueVersion(id)) + `"`}}
	}

	cases := []struct {
		name         string
		call         func() opCall
		extra        map[string]string
		status       int
		wantLocation bool
	}{
		{"createIssue", func() opCall {
			return opCall{http.MethodPost, proj + "/issues", map[string]any{"title": w.fresh("hdr"), "type": "task", "force": true}, nil}
		}, nil, http.StatusCreated, true},
		{"createIssue return=minimal", func() opCall {
			return opCall{http.MethodPost, proj + "/issues", map[string]any{"title": w.fresh("hdr"), "type": "task", "force": true}, nil}
		}, minimal, http.StatusCreated, true},
		{"createEpic", func() opCall {
			return opCall{http.MethodPost, proj + "/epics", map[string]any{"title": w.fresh("hdr"), "childTemplates": []map[string]any{{"title": "c"}}}, nil}
		}, nil, http.StatusCreated, true},
		{"createComment", func() opCall {
			return opCall{http.MethodPost, "/api/v1/issues/" + w.issue + "/comments", map[string]any{"body": w.fresh("hdr")}, nil}
		}, nil, http.StatusCreated, true},
		{"closeIssue", closeCall, nil, http.StatusOK, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			call := c.call()
			h := idem()
			for k, v := range c.extra {
				h[k] = v
			}
			first := w.do(key, call, "", h)
			if first.status != c.status {
				t.Fatalf("first: %d %s, want %d", first.status, first.body, c.status)
			}
			if first.header.Get("ETag") == "" {
				t.Fatalf("first answer has no ETag")
			}
			if got := first.header.Get("Location") != ""; got != c.wantLocation {
				t.Fatalf("first answer Location %q, want present=%v", first.header.Get("Location"), c.wantLocation)
			}
			again := w.do(key, call, "", h)
			sameAnswer(t, c.name, first, again)
			sameHeaders(t, c.name, first, again)
		})
	}

	// The process failed after the commit, before the body was stored: the
	// repeat answers from the recorded id and version, still with the
	// Location and ETag of the first answer.
	t.Run("createIssue after a failure before the body was stored", func(t *testing.T) {
		call := opCall{http.MethodPost, proj + "/issues", map[string]any{"title": w.fresh("hdr"), "type": "task", "force": true}, nil}
		h := idem()
		first := w.do(key, call, "", h)
		if first.status != http.StatusCreated {
			t.Fatalf("first: %d %s", first.status, first.body)
		}
		// The body was never written, and the marker of the first execution
		// has ended since.
		w.exec(`UPDATE api_idempotency SET response_body = NULL, response_content_type = NULL,
    in_progress_until = now() - interval '1 second' WHERE idempotency_key = $1`, h["Idempotency-Key"])
		again := w.do(key, call, "", h)
		if again.status != first.status || again.object(t)["id"] != first.object(t)["id"] {
			t.Fatalf("repeat %d %s, first %d %s", again.status, again.body, first.status, first.body)
		}
		for _, name := range []string{"Location", "ETag"} {
			if again.header.Get(name) != first.header.Get(name) {
				t.Errorf("%s of the repeat %q, of the first answer %q", name, again.header.Get(name), first.header.Get(name))
			}
		}
		var stored int
		if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM api_idempotency WHERE idempotency_key = $1`, h["Idempotency-Key"]).Scan(&stored); err != nil || stored != 1 {
			t.Errorf("stored outcomes %d (%v), want 1", stored, err)
		}
	})
}
