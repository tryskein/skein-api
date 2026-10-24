package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// observability_test.go — sk-303.13 / T012: the WRITE-side failure signals.
//
// "Two refusal paths are separately observable: a push not sent because
// settings were unavailable, and an event that mapped onto no reason — both
// must produce a metric and a log record, not silence."
//
// Two failures, two signals, and each signal is BOTH a metric and a log
// record. Asserting only the metric would miss the half an operator actually
// reads during an incident; asserting only the log would miss the half an
// alert fires on.

// logCapture swaps the default slog logger for one that records JSON records,
// and restores it afterwards.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
	old *slog.Logger
}

func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	c := &logCapture{old: slog.Default()}
	slog.SetDefault(slog.New(slog.NewJSONHandler(&c.buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(c.old) })
	return c
}

// records parses the captured lines.
func (c *logCapture) records(t *testing.T) []map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(c.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %q (%v)", line, err)
		}
		out = append(out, m)
	}
	return out
}

// find returns the first record whose msg contains needle.
func (c *logCapture) find(t *testing.T, needle string) map[string]any {
	t.Helper()
	for _, r := range c.records(t) {
		if msg, _ := r["msg"].(string); strings.Contains(msg, needle) {
			return r
		}
	}
	return nil
}

func (c *logCapture) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// TestObservability_UnmappedEventIsNotSilent — an event type with a delivery
// path that maps to no reason is a CONFIGURATION ERROR, and the ADR names the
// price it accepts for the narrow default: "adding an event that belongs
// to no reason becomes a configuration error, not a silent
// 'it will go through by default'". So it must be loud.
//
// The fan-out answers this before it touches the database, which is why the
// transaction can be nil here: the gate is reached on the classifier alone.
func TestObservability_UnmappedEventIsNotSilent(t *testing.T) {
	logs := captureLogs(t)
	before := testutil.ToFloat64(fanoutUnmappedEventsTotal)

	n, err := FanOut(context.Background(), nil, FanOutInput{
		EventID:    42,
		EventType:  "issue.teleported",
		EntityType: "issue",
		IssueID:    "obs-1",
		ProjectID:  uuid.Must(uuid.NewRandom()),
	})
	if err != nil {
		t.Fatalf("an unmapped event must not fail the caller: %v", err)
	}
	if n != 0 {
		t.Errorf("inserted %d rows for an unmapped event, want 0", n)
	}

	if got := testutil.ToFloat64(fanoutUnmappedEventsTotal) - before; got != 1 {
		t.Errorf("unmapped counter moved by %v, want 1", got)
	}
	rec := logs.find(t, "not mapped to any reason")
	if rec == nil {
		t.Fatalf("no log record for the unmapped event; captured:\n%s", logs.text())
	}
	if rec["level"] != "ERROR" {
		t.Errorf("log level = %v, want ERROR — a configuration error is not a warning", rec["level"])
	}
	if rec["eventType"] != "issue.teleported" {
		t.Errorf("the record does not name the offending type: %v", rec["eventType"])
	}
}

// TestObservability_AuditOnlyEventIsSilentOnPurpose — the other half of the
// same rule, and the one that keeps the signal useful. Journal-only events must
// NOT raise the unmapped signal: if they did, the counter would sit at a
// permanent non-zero rate and stop meaning anything.
func TestObservability_AuditOnlyEventIsSilentOnPurpose(t *testing.T) {
	logs := captureLogs(t)
	before := testutil.ToFloat64(fanoutUnmappedEventsTotal)

	for _, evtType := range []string{"issue.claimed", "issue.closed", "dependency.created", "project.member_added"} {
		if _, err := FanOut(context.Background(), nil, FanOutInput{
			EventID:    7,
			EventType:  evtType,
			EntityType: "issue",
			IssueID:    "obs-2",
			ProjectID:  uuid.Must(uuid.NewRandom()),
		}); err != nil {
			t.Fatalf("%s: %v", evtType, err)
		}
	}

	if got := testutil.ToFloat64(fanoutUnmappedEventsTotal) - before; got != 0 {
		t.Errorf("audit-only events moved the unmapped counter by %v, want 0 — their silence is a decision", got)
	}
	if rec := logs.find(t, "not mapped to any reason"); rec != nil {
		t.Errorf("an audit-only event produced an unmapped log record: %v", rec)
	}
}

// TestObservability_PrefsUnavailableIsNotSilent — the second failure path,
// against a real database. The preferences cannot be read, no rows are
// written, the caller's operation survives — and none of that is allowed to be
// silent.
func TestObservability_PrefsUnavailableIsNotSilent(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	actor := seedTestUser(t, pool, "user")
	creator := seedTestUser(t, pool, "user")
	projectID := seedTestProject(t, pool, actor)
	addMember(t, pool, projectID, creator, actor)

	issueID := "obsp-1"
	seedTestIssue(t, pool, projectID, creator, issueID, 2)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, issueID) })

	logs := captureLogs(t)
	before := testutil.ToFloat64(fanoutPrefsUnavailableTotal)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	shadowReasonPrefs(t, ctx, tx)
	eid := addEvent(t, tx, "issue.status_changed", "issue", issueID, projectID, actor, "human")
	if _, err := FanOut(ctx, tx, FanOutInput{
		EventID:       eid,
		EventType:     "issue.status_changed",
		EntityType:    "issue",
		IssueID:       issueID,
		ProjectID:     projectID,
		ActorUserID:   actor,
		ActorType:     "human",
		CreatorUserID: creator,
	}); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("FanOut: %v", err)
	}
	_ = tx.Commit(ctx)

	if got := testutil.ToFloat64(fanoutPrefsUnavailableTotal) - before; got != 1 {
		t.Errorf("prefs-unavailable counter moved by %v, want 1", got)
	}
	rec := logs.find(t, "reason preferences unavailable")
	if rec == nil {
		t.Fatalf("no log record for the preferences outage; captured:\n%s", logs.text())
	}
	if rec["level"] != "ERROR" {
		t.Errorf("log level = %v, want ERROR", rec["level"])
	}
	for _, field := range []string{"eventId", "eventType", "projectId"} {
		if _, ok := rec[field]; !ok {
			t.Errorf("the record does not carry %q — the incident cannot be located from it", field)
		}
	}
	// The two failure paths must not share a counter: one incident, one signal.
	if strings.Contains(logs.text(), "not mapped to any reason") {
		t.Error("the preferences outage also raised the unmapped signal — the classes are merged")
	}
}

// TestObservability_FailureLogsCarryNoIssueProse — the PII rule. The fan-out
// knows the issue's title, the recipients' identities and the event payload;
// none of that belongs in a log line, and the failure paths are where a
// careless "add more context" would put it.
func TestObservability_FailureLogsCarryNoIssueProse(t *testing.T) {
	logs := captureLogs(t)
	const secretTitle = "PROSE-THAT-MUST-NOT-BE-LOGGED"

	_, _ = FanOut(context.Background(), nil, FanOutInput{
		EventID:    99,
		EventType:  "issue.teleported",
		EntityType: "issue",
		IssueID:    "obs-3",
		ProjectID:  uuid.Must(uuid.NewRandom()),
		Payload:    []byte(`{"changes":[{"field":"title","new":"` + secretTitle + `"}]}`),
	})

	text := logs.text()
	if strings.Contains(text, secretTitle) {
		t.Errorf("the event payload leaked into the log:\n%s", text)
	}
	for _, forbidden := range []string{"@", "https://", "Bearer "} {
		if strings.Contains(text, forbidden) {
			t.Errorf("log contains %q — addresses, endpoints and tokens have no place in a failure record:\n%s",
				forbidden, text)
		}
	}
}
