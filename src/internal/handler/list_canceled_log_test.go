package handler

// list_canceled_log_test.go — a list request its client cancelled is not
// logged as an ERROR: the production log handler writes the failure of the
// store at INFO with cancel=client_canceled. The reference is the defect
// report: GET /ui/v1/issues cancelled by the browser logged
// "list all issues failed … context canceled" at ERROR.

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"skein-api/internal/auth"
	"skein-api/internal/issues"
	"skein-api/internal/logging"
	"skein-api/internal/testsupport"
)

func TestListAllIssues_ClientCancelIsNotAnError(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	h := NewIssuesAllHandler(pool, issues.NewStore(pool), nil)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewDualHandler(&buf, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx, cancel := context.WithCancel(auth.WithUserRole(auth.WithUserID(context.Background(), owner), "user"))
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/ui/v1/issues", nil).WithContext(ctx)
	h.List(httptest.NewRecorder(), req)

	out := buf.String()
	if strings.Contains(out, `"level":"ERROR"`) {
		t.Errorf("a cancelled request logged an ERROR:\n%s", out)
	}
	if !strings.Contains(out, "list all issues failed") || !strings.Contains(out, logging.CancelKey+"="+logging.CanceledByClient) {
		t.Errorf("want the failure at INFO with %s=%s, got:\n%s", logging.CancelKey, logging.CanceledByClient, out)
	}
}
