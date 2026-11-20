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
	logSendFailure(context.Background(), c, push.Subscription{Endpoint: endpoint}, serr)

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
	logSendFailure(context.Background(), c,
		push.Subscription{Endpoint: "https://fcm.googleapis.com/fcm/send/sk627-fcm-token"}, fmt.Errorf(
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
				// Another device's subscription: the echoed URL is one the
				// redaction by subscription cannot know, so only the
				// backstop stands between it and the log.
				push.Subscription{Endpoint: "https://web.push.apple.com/3/device/another-device-token"},
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

// Row 30 — INV-9 in the delivery log: whatever a failed send's error carries
// of the subscription — the endpoint, its token-bearing path segment or query
// value as bare text, either key — no field of the "push send failed" record
// carries it; the status, the push-service host and the provider's own
// diagnosis stay.
func TestLogSendFailure_NoFieldCarriesTheSubscriptionSecrets(t *testing.T) {
	const (
		host       = "fcm.googleapis.com"
		pathToken  = "row30-path-token-0123456789"
		queryToken = "row30-query-token-9876543210"
		p256dh     = "BRow30P256dhKeyMaterial_abcdefghijklmnopqrstuvwxyz0123456789"
		auth       = "row30AuthSecret0"
	)
	sub := push.Subscription{
		Endpoint: "https://" + host + "/fcm/send/" + pathToken + "?token=" + queryToken,
		P256dh:   p256dh,
		Auth:     auth,
	}
	secrets := []string{sub.Endpoint, pathToken, queryToken, p256dh, auth}

	cases := []struct {
		name string
		err  error
		// keep — diagnostic text the record must still carry.
		keep []string
	}{
		{
			name: "transport error quoting the endpoint and the keys",
			err: fmt.Errorf("sending web push: Post %q: handshake with p256dh=%s auth=%s: connection refused",
				sub.Endpoint, p256dh, auth),
			keep: []string{"connection refused"},
		},
		{
			name: "query token as bare text",
			err:  fmt.Errorf("sending web push: token %s rejected: connection reset", queryToken),
			keep: []string{"connection reset"},
		},
		{
			name: "refusal echoing the endpoint parts and both keys",
			err: &push.StatusError{
				StatusCode:    500,
				Host:          host,
				Body:          `{"reason":"InternalError","device":"` + pathToken + `","keys":["` + p256dh + `","` + auth + `"]}`,
				BodyReadError: "unexpected EOF after " + pathToken + " " + auth,
			},
			keep: []string{"status=500", "pushHost=" + host, "InternalError", "unexpected EOF"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(logging.NewDualHandler(&buf, slog.LevelDebug)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			reason := "mention"
			logSendFailure(context.Background(), pushClaim{id: 30, userID: uuid.New(), reason: &reason}, sub, tc.err)

			line := buf.String()
			if !strings.Contains(line, `msg="push send failed"`) {
				t.Fatalf("no \"push send failed\" record in %q", line)
			}
			for _, secret := range secrets {
				if strings.Contains(line, secret) {
					t.Errorf("the record carries the subscription secret %q: %s", secret, line)
				}
			}
			for _, want := range tc.keep {
				if !strings.Contains(line, want) {
					t.Errorf("the record lost %q: %s", want, line)
				}
			}
		})
	}
}
