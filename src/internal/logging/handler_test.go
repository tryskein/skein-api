package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

type ctxKey string

const sessionKey ctxKey = "session"

// sessionAttr is the shape main.go wires: a value that lives in the request
// context becomes an attr on every record written while serving it.
func sessionAttr(ctx context.Context) []slog.Attr {
	if sid, _ := ctx.Value(sessionKey).(string); sid != "" {
		return []slog.Attr{slog.String("sessionId", sid)}
	}
	return nil
}

func newTestLogger(w *bytes.Buffer, extractors ...func(context.Context) []slog.Attr) *slog.Logger {
	return slog.New(NewDualHandler(w, slog.LevelDebug, extractors...))
}

// TestContextAttrs_AddedToEveryRecord is the whole point: the call site does
// not mention sessionId, and the line carries it anyway.
func TestContextAttrs_AddedToEveryRecord(t *testing.T) {
	var buf bytes.Buffer
	log := newTestLogger(&buf, sessionAttr)
	ctx := context.WithValue(context.Background(), sessionKey, "mcp-session-abc")

	log.InfoContext(ctx, "mcp tool ok", "tool", "skein_epic_create")

	got := buf.String()
	if !strings.Contains(got, "sessionId=mcp-session-abc") {
		t.Fatalf("line lacks the context attr: %q", got)
	}
	if !strings.Contains(got, "tool=skein_epic_create") {
		t.Fatalf("line lost its explicit attrs: %q", got)
	}
}

// TestContextAttrs_AbsentWhenExtractorYieldsNothing: a REST or cookie call has
// no MCP session, and the field must then be missing rather than empty — an
// empty value would create a fictitious series in any logfmt aggregation.
func TestContextAttrs_AbsentWhenExtractorYieldsNothing(t *testing.T) {
	var buf bytes.Buffer
	log := newTestLogger(&buf, sessionAttr)

	log.InfoContext(context.Background(), "mcp tool ok", "tool", "skein_show")

	if got := buf.String(); strings.Contains(got, "sessionId") {
		t.Fatalf("expected no sessionId key at all, got %q", got)
	}
}

// TestContextAttrs_ExplicitValueWins guards the duplicate: the failure path
// logs sessionId itself, so without the key check the line would carry it
// twice and a logfmt reader would keep whichever it saw last.
func TestContextAttrs_ExplicitValueWins(t *testing.T) {
	var buf bytes.Buffer
	log := newTestLogger(&buf, sessionAttr)
	ctx := context.WithValue(context.Background(), sessionKey, "from-context")

	log.WarnContext(ctx, "mcp tool failed", "sessionId", "explicit")

	got := buf.String()
	if n := strings.Count(got, "sessionId="); n != 1 {
		t.Fatalf("sessionId appears %d times, want exactly 1: %q", n, got)
	}
	if !strings.Contains(got, "sessionId=explicit") {
		t.Fatalf("explicit value lost to the ambient one: %q", got)
	}
}

// TestContextAttrs_SurviveWithAttrsAndGroup: slog.With and WithGroup build new
// handlers, and a naive copy drops the extractors — the field would then be
// present on bare loggers and missing on derived ones.
func TestContextAttrs_SurviveWithAttrsAndGroup(t *testing.T) {
	var buf bytes.Buffer
	log := newTestLogger(&buf, sessionAttr).With("service", "skein-api")
	ctx := context.WithValue(context.Background(), sessionKey, "kept")

	log.InfoContext(ctx, "derived logger")
	if got := buf.String(); !strings.Contains(got, "sessionId=kept") {
		t.Fatalf("WithAttrs dropped the extractors: %q", got)
	}

	buf.Reset()
	grouped := newTestLogger(&buf, sessionAttr).WithGroup("g")
	grouped.InfoContext(ctx, "grouped logger")
	if got := buf.String(); !strings.Contains(got, "kept") {
		t.Fatalf("WithGroup dropped the extractors: %q", got)
	}
}

// TestContextAttrs_ErrorRecordsGoToJSONWithTheAttr — Error+ takes the other
// backing handler, so it needs its own proof.
func TestContextAttrs_ErrorRecordsGoToJSONWithTheAttr(t *testing.T) {
	var buf bytes.Buffer
	log := newTestLogger(&buf, sessionAttr)
	ctx := context.WithValue(context.Background(), sessionKey, "mcp-session-err")

	log.ErrorContext(ctx, "mcp tool error", "tool", "skein_update_many")

	var parsed map[string]any
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("error record is not JSON: %v\n%s", err, buf.String())
	}
	if parsed["sessionId"] != "mcp-session-err" {
		t.Fatalf("sessionId = %v, want mcp-session-err", parsed["sessionId"])
	}
}

// TestContextAttrs_NoneConfiguredIsUnchanged pins the compatibility claim
// behind the variadic parameter: with no extractors the handler behaves
// exactly as it did before they existed.
func TestContextAttrs_NoneConfiguredIsUnchanged(t *testing.T) {
	var buf bytes.Buffer
	log := newTestLogger(&buf)

	log.InfoContext(context.Background(), "plain", "tool", "skein_list")

	got := buf.String()
	if !strings.Contains(got, "tool=skein_list") || strings.Contains(got, "sessionId") {
		t.Fatalf("unexpected line: %q", got)
	}
}

// TestSecretRedactionStillApplies: the sanitizer runs in the backing
// handlers' ReplaceAttr, and a context-supplied attr must not bypass it.
func TestSecretRedactionStillApplies(t *testing.T) {
	var buf bytes.Buffer
	log := newTestLogger(&buf, func(context.Context) []slog.Attr {
		return []slog.Attr{slog.String("apiKey", "ska_live_value")}
	})

	log.InfoContext(context.Background(), "line")

	got := buf.String()
	if strings.Contains(got, "ska_live_value") {
		t.Fatalf("secret leaked through the context attr: %q", got)
	}
	if !strings.Contains(got, "apiKey=***") {
		t.Fatalf("expected redaction, got %q", got)
	}
}

// TestContextAttrs_WithAttrsBoundValueWins is the regression for the defect
// codex found on the first pass: recordHasKey only sees attrs on the record,
// and a key bound through slog.Logger.With lives in the backing handler
// instead — so the ambient value was appended next to it and, on JSON, won
// by being last. Both output branches are asserted, because they are separate
// backing handlers and a fix that misses one is invisible in the other.
func TestContextAttrs_WithAttrsBoundValueWins(t *testing.T) {
	var buf bytes.Buffer
	log := newTestLogger(&buf, sessionAttr).With("sessionId", "explicit")
	ctx := context.WithValue(context.Background(), sessionKey, "ambient")

	log.InfoContext(ctx, "bound wins")
	got := buf.String()
	if n := strings.Count(got, "sessionId="); n != 1 {
		t.Fatalf("text: sessionId appears %d times, want exactly 1: %q", n, got)
	}
	if !strings.Contains(got, "sessionId=explicit") {
		t.Fatalf("text: bound value lost to the ambient one: %q", got)
	}

	buf.Reset()
	log.ErrorContext(ctx, "bound wins")
	var parsed map[string]any
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("error record is not JSON: %v\n%s", err, buf.String())
	}
	if parsed["sessionId"] != "explicit" {
		t.Fatalf("json: sessionId = %v, want explicit", parsed["sessionId"])
	}
	if n := strings.Count(buf.String(), `"sessionId"`); n != 1 {
		t.Fatalf("json: sessionId appears %d times, want exactly 1: %s", n, buf.String())
	}
}

// TestContextAttrs_GroupStartsFreshNamespace: a key bound BEFORE a group
// cannot collide with a context attr added inside it — they end up under
// different names — so the bound-key set must not carry across WithGroup and
// suppress an attr that would not have clashed.
func TestContextAttrs_GroupStartsFreshNamespace(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewDualHandler(&buf, slog.LevelDebug, sessionAttr).
		WithAttrs([]slog.Attr{slog.String("sessionId", "outer")}).
		WithGroup("g"))
	ctx := context.WithValue(context.Background(), sessionKey, "ambient")

	log.InfoContext(ctx, "grouped")

	got := buf.String()
	if !strings.Contains(got, "sessionId=outer") {
		t.Fatalf("outer bound attr missing: %q", got)
	}
	if !strings.Contains(got, "ambient") {
		t.Fatalf("context attr suppressed inside a new group namespace: %q", got)
	}
}
