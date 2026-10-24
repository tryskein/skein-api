package outbox

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/logging"
	"skein-api/internal/push"
)

// push_send_failure_log_test.go — sk-627: the "push send failed" record of a
// push-service refusal must carry what diagnoses it (status, push-service
// host, the provider's response body) and must not carry the device endpoint,
// which embeds the subscription token.
//
// The record is written through the project's own DualHandler, because the
// property has two sides that only the real handler shows together: the
// secret sanitizer must not mask the diagnostic fields, and nothing else may
// let the endpoint through.

func TestLogSendFailure_RefusalIsDiagnosableWithoutTheEndpoint(t *testing.T) {
	// Not parallel: swaps the process-wide default logger.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewDualHandler(&buf, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const secretPath = "/3/device/sk627-secret-subscription-token"
	endpoint := "https://web.push.apple.com" + secretPath
	reason := "delegated_status"
	c := pushClaim{id: 627, userID: uuid.New(), reason: &reason}

	// The body echoes the endpoint and the error text quotes it — the two
	// places a provider or a transport could leak it from.
	serr := fmt.Errorf("sending web push to %s: %w", endpoint, &push.StatusError{
		StatusCode: 403,
		Host:       "web.push.apple.com",
		Body:       `{"reason":"BadJwtToken"} for ` + endpoint,
	})
	logSendFailure(context.Background(), c, serr)

	line := buf.String()
	if !strings.Contains(line, `msg="push send failed"`) {
		t.Fatalf("no \"push send failed\" record in %q", line)
	}
	for _, want := range []string{
		"status=403",
		"pushHost=web.push.apple.com",
		"BadJwtToken",
		"responseBodyTruncated=false",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("record lacks %q: %s", want, line)
		}
	}
	if strings.Contains(line, "sk627-secret-subscription-token") {
		t.Errorf("record carries the device endpoint: %s", line)
	}
	if strings.Contains(line, "***") {
		t.Errorf("the sanitizer masked a diagnostic field: %s", line)
	}
}

func TestLogSendFailure_TransportErrorStillRedactsTheEndpoint(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewDualHandler(&buf, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	reason := "mention"
	c := pushClaim{id: 1, userID: uuid.New(), reason: &reason}
	logSendFailure(context.Background(), c, fmt.Errorf(
		"sending web push: Post \"https://fcm.googleapis.com/fcm/send/sk627-fcm-token\": connection refused"))

	line := buf.String()
	if !strings.Contains(line, "connection refused") {
		t.Errorf("record lost the failure: %s", line)
	}
	if strings.Contains(line, "sk627-fcm-token") {
		t.Errorf("record carries the device endpoint: %s", line)
	}
	if strings.Contains(line, "pushHost=") {
		t.Errorf("a transport error has no provider answer, yet the record names one: %s", line)
	}
}

// TestLogSendFailure_EscapedEchoesAreRedactedToo — sk-627 review No. 1, the
// outbox half. The sender redacts the forms of the endpoint it knows; this
// layer is the backstop for a URL the sender could not know about, and JSON
// may escape '/' while a form body percent-encodes it. Checked on the line
// the project's DualHandler writes.
func TestLogSendFailure_EscapedEchoesAreRedactedToo(t *testing.T) {
	const token = "sk627-escaped-echo-token"
	cases := map[string]string{
		"JSON-escaped slashes":   `{"endpoint":"https:\/\/web.push.apple.com\/3\/device\/` + token + `"}`,
		"percent-encoded":        "endpoint=https%3A%2F%2Fweb.push.apple.com%2F3%2Fdevice%2F" + token,
		"percent-encoded, lower": "endpoint=https%3a%2f%2fweb.push.apple.com%2f3%2fdevice%2f" + token,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(logging.NewDualHandler(&buf, slog.LevelDebug)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			reason := "mention"
			logSendFailure(context.Background(), pushClaim{id: 2, userID: uuid.New(), reason: &reason},
				&push.StatusError{StatusCode: 403, Host: "web.push.apple.com", Body: body})

			line := buf.String()
			if strings.Contains(line, token) {
				t.Errorf("the log line carries the endpoint token: %s", line)
			}
			if !strings.Contains(line, "status=403") {
				t.Errorf("the record lost its status: %s", line)
			}
		})
	}
}
