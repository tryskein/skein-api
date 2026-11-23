package logging

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// TestClientCanceled_ErrorRecordsAreNotErrors — an ERROR record about a
// request its client cancelled is written at INFO with
// cancel=client_canceled; a timeout of our own and any other failure stay
// ERROR (JSON). The reference is the defect report: a client leaving the
// page produced "list all issues failed: context canceled" at ERROR.
func TestClientCanceled_ErrorRecordsAreNotErrors(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	timedOut, stop := context.WithTimeout(context.Background(), 0)
	defer stop()
	<-timedOut.Done()

	cases := []struct {
		name      string
		ctx       context.Context
		errAttr   any
		downgrade bool
	}{
		{"cancelled request, wrapped error text", canceled, "counting all issues: context canceled", true},
		{"cancelled request, error value", canceled, fmt.Errorf("counting all issues: %w", context.Canceled), true},
		{"timeout of our own", timedOut, "counting all issues: context deadline exceeded", false},
		{"live request, cancellation text", context.Background(), "counting all issues: context canceled", false},
		{"cancelled request, another failure", canceled, "counting all issues: boom", false},
		{"cancelled request, the phrase inside another failure", canceled, "parsing \"context canceled\" from the body: bad input", false},
		{"cancelled request, no error attribute", canceled, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := newTestLogger(&buf)
			args := []any{"userId", "u1"}
			if c.errAttr != nil {
				args = append(args, "error", c.errAttr)
			}
			logger.ErrorContext(c.ctx, "list all issues failed", args...)
			out := buf.String()
			isJSONError := strings.Contains(out, `"level":"ERROR"`)
			isInfo := strings.HasPrefix(out, "time=") && strings.Contains(out, "level=INFO") &&
				strings.Contains(out, CancelKey+"="+CanceledByClient)
			if c.downgrade && (!isInfo || isJSONError) {
				t.Errorf("got %q, want an INFO line with %s=%s", out, CancelKey, CanceledByClient)
			}
			if !c.downgrade && !isJSONError {
				t.Errorf("got %q, want an ERROR line", out)
			}
		})
	}
}

// TestClientCanceled_BelowTheLevelIsDropped — on a handler above INFO the
// downgraded record is not written at all.
func TestClientCanceled_BelowTheLevelIsDropped(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	var buf bytes.Buffer
	slog.New(NewDualHandler(&buf, slog.LevelWarn)).ErrorContext(canceled, "x failed", "error", errors.Join(context.Canceled))
	if buf.Len() != 0 {
		t.Errorf("got %q, want nothing", buf.String())
	}
}

// TestClientCanceled_DowngradedLineKeepsTheRecord — the INFO line a cancelled
// request's ERROR record becomes is the same record: the attributes before the
// error, the error itself and the ones after it are all on the line, plus the
// marker cancel=client_canceled (the key and value the journal is filtered on).
func TestClientCanceled_DowngradedLineKeepsTheRecord(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	var buf bytes.Buffer
	newTestLogger(&buf).ErrorContext(canceled, "list all issues failed",
		"userId", "u1", "error", "counting all issues: context canceled", "projectId", "p1", "page", 3)
	out := buf.String()
	for _, want := range []string{
		"level=INFO",
		`msg="list all issues failed"`,
		" userId=u1",
		` error="counting all issues: context canceled"`,
		" projectId=p1",
		" page=3",
		" cancel=client_canceled",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("line %q lacks %q", out, want)
		}
	}
}

// TestClientCanceled_LevelBoundaries — the downgraded record is written by a
// handler whose minimum level is exactly INFO and dropped by one above it; a
// record below ERROR is never touched by the rule: it keeps its level, its
// fields and gets no cancel marker.
func TestClientCanceled_LevelBoundaries(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	t.Run("a handler at INFO writes the downgraded record", func(t *testing.T) {
		var buf bytes.Buffer
		slog.New(NewDualHandler(&buf, slog.LevelInfo)).ErrorContext(canceled, "x failed",
			"error", fmt.Errorf("reading: %w", context.Canceled))
		out := buf.String()
		if !strings.Contains(out, "level=INFO") || !strings.Contains(out, " cancel=client_canceled") {
			t.Errorf("got %q, want an INFO line with cancel=client_canceled", out)
		}
	})

	for _, c := range []struct {
		name  string
		level slog.Level
		want  string
	}{
		{"WARN", slog.LevelWarn, "level=WARN"},
		{"INFO", slog.LevelInfo, "level=INFO"},
		{"DEBUG", slog.LevelDebug, "level=DEBUG"},
	} {
		t.Run("a "+c.name+" record of a cancelled request stays as it is", func(t *testing.T) {
			var buf bytes.Buffer
			newTestLogger(&buf).Log(canceled, c.level, "x interrupted",
				"userId", "u1", "error", "reading: context canceled", "projectId", "p1")
			out := buf.String()
			for _, want := range []string{c.want, " userId=u1", ` error="reading: context canceled"`, " projectId=p1"} {
				if !strings.Contains(out, want) {
					t.Errorf("line %q lacks %q", out, want)
				}
			}
			if strings.Contains(out, CancelKey+"=") {
				t.Errorf("line %q carries the cancel marker, want the record untouched", out)
			}
		})
	}
}
