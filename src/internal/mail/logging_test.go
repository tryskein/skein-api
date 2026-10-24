package mail_test

// logging_test.go — what the mail worker logs about a failed delivery
// (sk-674.18, T022; matrix row 74; the project guidelines, "What NOT
// to log").
//
// The Error record of a failed delivery is written only by the worker,
// which needs the queue — so the test drives the real worker over the
// database (skipped without one), the cheapest level at which that record
// exists. The relay is the worst case: its refusal quotes the recipient and
// the token of the message back, as real relays quote the recipient.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"skein-api/internal/mail"
	"skein-api/internal/testsupport"
	"skein-api/internal/tokens"
)

// echoRelay refuses every message with a reply that quotes the recipient
// and the link token the message carries.
type echoRelay struct {
	t    *testing.T
	code int
}

func (r *echoRelay) Send(_ context.Context, to string, msg []byte) error {
	tok := linkToken(r.t, msg, "/reset-password/")
	return &mail.SendError{Stage: mail.StageRcpt, Err: &textproto.Error{
		Code: r.code,
		Msg:  "5.1.1 <" + to + ">: recipient rejected; message began with " + tok,
	}}
}

func (r *echoRelay) From() string { return "skein@example.com" }

// captureLogs routes the default logger into a buffer of JSON records for
// the rest of the test.
func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Row 74: the Error record of a delivery the relay refused names neither
// the recipient's address nor the token — not in its message, not in any
// attribute — and neither does any other record of the attempt. Red if the
// address or the token reaches the log.
func TestRunner_FailureLogsCarryNoAddressAndNoToken(t *testing.T) {
	pool := testsupport.NewTestPool(t)

	for _, tc := range []struct {
		name      string
		code      int
		wantError bool
	}{
		{"permanent refusal (Error)", 550, true},
		{"temporary refusal", 450, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetQueue(t, pool)
			userID, to := seedUser(t, pool)
			enqueue(t, pool, mail.Message{Kind: mail.KindPasswordReset, To: to, Lang: mail.LangEN, UserID: userID}, 24*time.Hour)

			var lastToken string
			relay := &echoRelay{t: t, code: tc.code}
			tokenSeen := &tokenSpy{inner: relay, seen: &lastToken, t: t}
			run, err := mail.NewRunner(pool, tokenSeen, tokens.EmailIssuer(pool), linkBase, mail.Options{Now: newClock().Now})
			if err != nil {
				t.Fatal(err)
			}
			logs := captureLogs(t)

			tick(t, run)

			out := logs.String()
			if lastToken == "" {
				t.Fatal("precondition: the relay was never reached")
			}
			var errorRecords int
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				var rec map[string]any
				if err := json.Unmarshal([]byte(line), &rec); err != nil {
					t.Fatalf("not a JSON record: %q", line)
				}
				if rec["level"] == "ERROR" {
					errorRecords++
				}
			}
			if tc.wantError && errorRecords == 0 {
				t.Fatalf("precondition: no Error record for a refused delivery; logs:\n%s", out)
			}
			// The whole capture, case-insensitively: a record may carry the
			// address in any attribute and in any case.
			low := strings.ToLower(out)
			if strings.Contains(low, strings.ToLower(to)) {
				t.Errorf("the recipient's address reached the log:\n%s", out)
			}
			if strings.Contains(out, lastToken) {
				t.Errorf("the token reached the log:\n%s", out)
			}
		})
	}
}

// tokenSpy remembers the token of the last message it passed on.
type tokenSpy struct {
	inner mail.Sender
	seen  *string
	t     *testing.T
}

func (s *tokenSpy) Send(ctx context.Context, to string, msg []byte) error {
	*s.seen = linkToken(s.t, msg, "/reset-password/")
	return s.inner.Send(ctx, to, msg)
}

func (s *tokenSpy) From() string { return s.inner.From() }
