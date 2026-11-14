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
