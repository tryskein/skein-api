package outbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"skein-api/internal/push"
)

// observability_test.go — sk-303.13 / T012: the DELIVERY-side signals.
//
// Two rules are asserted here and neither is decorative.
//
//   - The outcome classes must stay APART. "Do not merge prefs-unavailable with
//     transport failure and no-device into one label value": they are different
//     incidents with different fixes, and merging them makes the metric
//     unusable exactly when it is needed.
//
//   - A failure record must not carry the device endpoint. The endpoint is a
//     subscription secret; anyone holding it can push to that device. It never
//     appears in a field we choose — it appears inside an error somebody else
//     formatted, which is why the redaction is applied to error TEXT.

// outboxLogCapture swaps the default logger for a recording one.
type outboxLogCapture struct {
	buf bytes.Buffer
	old *slog.Logger
}

func captureOutboxLogs(t *testing.T) *outboxLogCapture {
	t.Helper()
	c := &outboxLogCapture{old: slog.Default()}
	slog.SetDefault(slog.New(slog.NewJSONHandler(&c.buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(c.old) })
	return c
}

func (c *outboxLogCapture) text() string { return c.buf.String() }

func (c *outboxLogCapture) find(t *testing.T, needle string) map[string]any {
	t.Helper()
	for line := range strings.SplitSeq(strings.TrimSpace(c.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if msg, _ := m["msg"].(string); strings.Contains(msg, needle) {
			return m
		}
	}
	return nil
}

// TestObservability_OutcomeClassesAreDistinct — the closed set, checked as a
// set: every outcome the code names is distinct from every other, and none of
// them collapses into another.
//
// The set is READ OUT OF THE PACKAGE SOURCE (outcome_set_test.go), not
// written here. It used to be a hand-written list of six with a hand-written
// `want 6` beside it, which made the assertion tautological with respect to
// anything added later: sk-303.14 introduced a seventh outcome and this test
// — the one that owns distinctness — never noticed it existed.
func TestObservability_OutcomeClassesAreDistinct(t *testing.T) {
	t.Parallel()
	outcomes := allOutcomeValues(t)
	seen := map[string]bool{}
	for _, o := range outcomes {
		if o == "" {
			t.Error("an outcome constant is empty — an unlabelled outcome is an invisible one")
		}
		if seen[o] {
			t.Errorf("outcome value %q is used twice — the classes are merged", o)
		}
		seen[o] = true
	}
	if len(seen) != len(outcomes) {
		t.Errorf("distinct outcomes = %d over %d constants — two classes share a value",
			len(seen), len(outcomes))
	}
	// The three the spec names explicitly as "do not merge".
	if outcomePrefsUnavailable == outcomeTransportExhausted ||
		outcomePrefsUnavailable == outcomeNoDevice ||
		outcomeTransportExhausted == outcomeNoDevice {
		t.Error("prefs-unavailable, transport-exhausted and no-device must not share a value")
	}
	// The sub-cause axis exists precisely because the schema's outcome set is
	// closed and cannot separate the two configuration refusals.
	if causeSenderDisabled == causeChannelDisabled || causeSenderDisabled == "" || causeChannelDisabled == "" {
		t.Error("the configuration_no_send sub-causes must be distinct and named")
	}
}

// TestObservability_EachOutcomeGetsItsOwnSeries — distinctness where it is
// actually consumed. Two different refusals on two rows must move two
// different counter series, not one.
func TestObservability_EachOutcomeGetsItsOwnSeries(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	// Row 1 — no device registered: a legitimate final outcome.
	noDev := newPushFixture(t, pool, "obsnd")
	noDevID := noDev.owed(t, pushRow{})

	// Row 2 — a device, but the sender is not configured at all.
	disabled := newPushFixture(t, pool, "obscf").withDevice(t)
	disabledID := disabled.owed(t, pushRow{})

	beforeNoDevice := testutil.ToFloat64(
		pushOutcomesTotal.WithLabelValues("delegated_status", outcomeNoDevice, causeNone))
	beforeConfig := testutil.ToFloat64(
		pushOutcomesTotal.WithLabelValues("delegated_status", outcomeConfigurationNoSend, causeSenderDisabled))

	// First pass: a working sender — row 1 finds no device, row 2 sends.
	if _, err := newRunner(pool, &fakePushSender{enabled: true}).Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if state, outcome, _ := pushStateOf(t, pool, noDevID); state != "final" || outcome == nil || *outcome != outcomeNoDevice {
		t.Fatalf("row without a device = %s/%v, want final/%s", state, outcome, outcomeNoDevice)
	}
	if got := testutil.ToFloat64(
		pushOutcomesTotal.WithLabelValues("delegated_status", outcomeNoDevice, causeNone)) - beforeNoDevice; got != 1 {
		t.Errorf("no_device series moved by %v, want 1", got)
	}

	// Second row, second pass, with the sender switched off.
	if _, err := pool.Exec(ctx, `
UPDATE notifications SET push_state = 'pending', push_next_attempt_at = now(),
       push_outcome = NULL, push_finalized_at = NULL, push_attempts = 0
 WHERE id = $1`, disabledID); err != nil {
		t.Fatalf("re-arm row: %v", err)
	}
	if _, err := newRunner(pool, &fakePushSender{enabled: false}).Tick(ctx); err != nil {
		t.Fatalf("Tick (disabled sender): %v", err)
	}
	if state, outcome, _ := pushStateOf(t, pool, disabledID); state != "final" || outcome == nil || *outcome != outcomeConfigurationNoSend {
		t.Fatalf("row with a disabled sender = %s/%v, want final/%s", state, outcome, outcomeConfigurationNoSend)
	}
	if got := testutil.ToFloat64(
		pushOutcomesTotal.WithLabelValues("delegated_status", outcomeConfigurationNoSend, causeSenderDisabled)) - beforeConfig; got != 1 {
		t.Errorf("configuration_no_send/sender_disabled series moved by %v, want 1", got)
	}
	// And the two refusals did not land on one another's series.
	if got := testutil.ToFloat64(
		pushOutcomesTotal.WithLabelValues("delegated_status", outcomeNoDevice, causeNone)) - beforeNoDevice; got != 1 {
		t.Errorf("no_device series moved by %v after the second pass, want it untouched at 1", got)
	}
}

// TestObservability_SendFailureLogHidesTheDeviceEndpoint — the PII rule where
// the leak actually happens. The transport error quotes the URL it failed on,
// and that URL IS the subscription secret.
func TestObservability_SendFailureLogHidesTheDeviceEndpoint(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	// Row 30a — INV-9 on the changed path through Tick: the sender's error
	// may quote the endpoint with a query token and both keys of the device
	// (seedPushSubscription writes p256dh-fake / auth-fake); none of it
	// reaches the log, the diagnosis does, and the delivery verdict of each
	// kind of answer is the one it was.
	const p256dh, auth = "p256dh-fake", "auth-fake"
	cases := []struct {
		name string
		err  func(endpoint string) error
		// result — the per-device delivery label the answer is counted under.
		result string
		// logged — whether the answer writes a "push send failed" record.
		logged bool
		// field / diagnosis — the record's field that must keep the
		// diagnosis and the redaction marker.
		field, diagnosis string
	}{
		{
			name: "transport error",
			err: func(endpoint string) error {
				return errors.New(`Post "` + endpoint + `?token=abc123": dial tcp 10.0.0.1:443: connect: connection refused` +
					` (keys ` + p256dh + ` ` + auth + `)`)
			},
			result: "error", logged: true, field: "error", diagnosis: "connection refused",
		},
		{
			name: "refusal echoing the endpoint and the keys",
			err: func(endpoint string) error {
				return &push.StatusError{StatusCode: 500, Host: "push.example.com",
					Body: `{"reason":"Overloaded","endpoint":"` + endpoint + `?token=abc123","keys":"` + p256dh + `,` + auth + `"}`}
			},
			result: "error", logged: true, field: "responseBody", diagnosis: "Overloaded",
		},
		{
			name: "forbidden echoing the endpoint and the keys",
			err: func(endpoint string) error {
				return &push.StatusError{StatusCode: 403, Host: "push.example.com",
					Body: `{"reason":"BadJwtToken","endpoint":"` + endpoint + `?token=abc123","keys":"` + p256dh + `,` + auth + `"}`}
			},
			result: "forbidden", logged: true, field: "responseBody", diagnosis: "BadJwtToken",
		},
		{
			name: "gone",
			err: func(endpoint string) error {
				return fmt.Errorf("push endpoint returned 410: %w", push.ErrGone)
			},
			result: "gone", logged: false,
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := fmt.Sprintf("obsep%d", i)
			f := newPushFixture(t, pool, key).withDevice(t)
			f.owed(t, pushRow{})

			logs := captureOutboxLogs(t)
			sender := &fakePushSender{enabled: true}
			sender.setErr(tc.err(f.endpoint))
			before := testutil.ToFloat64(pushDeliveriesTotal.WithLabelValues("delegated_status", tc.result))

			if _, err := newRunner(pool, sender).Tick(ctx); err != nil {
				t.Fatalf("Tick: %v", err)
			}
			if len(callsTo(sender, f.endpoint)) == 0 {
				t.Fatalf("the runner never sent to the device; the case proves nothing")
			}
			if got := testutil.ToFloat64(pushDeliveriesTotal.WithLabelValues("delegated_status", tc.result)) - before; got < 1 {
				t.Errorf("deliveries counted as %q grew by %v, want the answer classified as %q", tc.result, got, tc.result)
			}

			text := logs.text()
			for _, secret := range []string{f.endpoint, "token=abc123", "abc123", p256dh, auth} {
				if strings.Contains(text, secret) {
					t.Errorf("%q leaked into the log:\n%s", secret, text)
				}
			}
			// The task title is never part of a delivery log line.
			if strings.Contains(text, "Task "+key) {
				t.Errorf("the issue title leaked into the log:\n%s", text)
			}
			rec := logs.find(t, "push send failed")
			if !tc.logged {
				if rec != nil {
					t.Errorf("a %s answer wrote a send-failure record: %v", tc.name, rec)
				}
				return
			}
			if rec == nil {
				t.Fatalf("the send failure was not logged at all; captured:\n%s", text)
			}
			// The useful half survives: an operator can still tell WHAT failed.
			msg, _ := rec[tc.field].(string)
			if !strings.Contains(msg, tc.diagnosis) {
				t.Errorf("redaction removed the diagnosis too: %s = %q", tc.field, msg)
			}
			if !strings.Contains(msg, "<redacted-endpoint>") {
				t.Errorf("the redaction marker is missing, so the reader cannot tell something was removed: %s = %q", tc.field, msg)
			}
		})
	}
}

// TestObservability_RedactEndpoints is the pure half of the same rule.
func TestObservability_RedactEndpoints(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{`Post "https://fcm.example.com/x/y?k=secret": timeout`, `Post "<redacted-endpoint>": timeout`},
		{`http://push.local/abc failed`, `<redacted-endpoint> failed`},
		{`dial tcp 10.0.0.1:443: connect: connection refused`, `dial tcp 10.0.0.1:443: connect: connection refused`},
		{`push endpoint returned status 500`, `push endpoint returned status 500`},
	}
	for _, tc := range cases {
		if got := redactEndpoints(tc.in); got != tc.want {
			t.Errorf("redactEndpoints(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
