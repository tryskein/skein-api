package push

// send_redaction_test.go — INV-9 at the sender's exit. The reference is the
// device-consent specification: the endpoint and the keys of a channel reach
// no log record and no error text, neither whole nor as a part carrying the
// channel token — short endpoints and nested send errors included; the
// provider's host and a fingerprint of the endpoint are allowed. The provider
// boundary is a substituted transport of the sender's HTTP client.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"skein-api/internal/logging"
)

// answerTransport answers every request with what answer returns.
type answerTransport func(*http.Request) (*http.Response, error)

func (f answerTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// failingBody yields data, then fails with err.
type failingBody struct {
	data *strings.Reader
	err  error
}

func (b *failingBody) Read(p []byte) (int, error) {
	if b.data.Len() > 0 {
		return b.data.Read(p)
	}
	return 0, b.err
}

func (b *failingBody) Close() error { return nil }

func answering(status int, body io.ReadCloser) answerTransport {
	return func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: body, Header: http.Header{}, Request: r}, nil
	}
}

// Row 29 — INV-9 at the sender's exit, by kind of endpoint and of failure.
func TestSend_NoErrorOrLogCarriesTheSubscriptionSecrets(t *testing.T) {
	const (
		pathToken  = "row29-path-token-abcdefghijklmnop"
		queryToken = "row29-query-token-qrstuvwxyz012345"
	)
	base := testSubscription("")
	keys := []string{base.P256dh, base.Auth}

	type outcome struct {
		gone, forbidden, invalid bool
		status                   int
	}
	cases := []struct {
		name     string
		endpoint string
		// secrets — besides the endpoint and both keys.
		secrets []string
		// transport answers the request; nil sends through the production
		// guarded client to a loopback server.
		transport func(sub Subscription) http.RoundTripper
		want      outcome
		// host — the provider host the error must still name.
		host string
	}{
		{
			name:     "short endpoint, transport error quoting it and the keys",
			endpoint: "https://p.example/t9",
			secrets:  []string{"t9"},
			transport: func(sub Subscription) http.RoundTripper {
				return answerTransport(func(*http.Request) (*http.Response, error) {
					return nil, errors.New("tls handshake failed for " + sub.P256dh + "/" + sub.Auth)
				})
			},
			host: "p.example",
		},
		{
			name:     "token in the path, refusal echoing the endpoint, its parts and the keys in body and read error",
			endpoint: "https://fcm.googleapis.com/fcm/send/" + pathToken,
			secrets:  []string{pathToken},
			transport: func(sub Subscription) http.RoundTripper {
				echo := `{"reason":"Overloaded","endpoint":"` + sub.Endpoint + `","escaped":"` +
					url.QueryEscape(sub.Endpoint) + `","token":"` + pathToken + `","keys":["` +
					sub.P256dh + `","` + sub.Auth + `"]}`
				return answering(http.StatusInternalServerError, &failingBody{
					data: strings.NewReader(echo),
					err:  errors.New("connection reset while reading " + pathToken + " " + sub.Auth),
				})
			},
			want: outcome{status: http.StatusInternalServerError},
			host: "fcm.googleapis.com",
		},
		{
			name:     "token in the query, forbidden echoing it",
			endpoint: "https://updates.push.services.mozilla.com/wpush/v2?token=" + queryToken,
			secrets:  []string{queryToken},
			transport: func(sub Subscription) http.RoundTripper {
				return answering(http.StatusForbidden, io.NopCloser(strings.NewReader(
					`{"reason":"BadJwtToken","token":"`+queryToken+`","auth":"`+sub.Auth+`"}`)))
			},
			want: outcome{forbidden: true, status: http.StatusForbidden},
			host: "updates.push.services.mozilla.com",
		},
		{
			name:     "gone",
			endpoint: "https://web.push.apple.com/3/device/" + pathToken,
			secrets:  []string{pathToken},
			transport: func(Subscription) http.RoundTripper {
				return answering(http.StatusGone, io.NopCloser(strings.NewReader("")))
			},
			want: outcome{gone: true},
		},
		{
			name:     "invalid endpoint",
			endpoint: "https://localhost/" + pathToken + "?token=" + queryToken,
			secrets:  []string{pathToken, queryToken},
			want:     outcome{invalid: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sub := base
			sub.Endpoint = tc.endpoint
			client := NewGuardedClient()
			if tc.transport != nil {
				client = &http.Client{Transport: tc.transport(sub)}
			}
			err := sendLogged(t, NewSender(&fakePruner{}, client, realVAPIDPublic, realVAPIDPrivate, "mailto:test@skein.local"), sub,
				append(append([]string{tc.endpoint}, keys...), tc.secrets...))

			if err == nil {
				t.Fatal("Send succeeded; every case is a failure")
			}
			if got := errors.Is(err, ErrGone); got != tc.want.gone {
				t.Errorf("errors.Is(err, ErrGone) = %v, want %v (err %v)", got, tc.want.gone, err)
			}
			if got := errors.Is(err, ErrForbidden); got != tc.want.forbidden {
				t.Errorf("errors.Is(err, ErrForbidden) = %v, want %v (err %v)", got, tc.want.forbidden, err)
			}
			if got := errors.Is(err, ErrInvalidEndpoint); got != tc.want.invalid {
				t.Errorf("errors.Is(err, ErrInvalidEndpoint) = %v, want %v (err %v)", got, tc.want.invalid, err)
			}
			var se *StatusError
			if got := errors.As(err, &se); got != (tc.want.status != 0) {
				t.Errorf("errors.As(err, *StatusError) = %v, want %v (err %v)", got, tc.want.status != 0, err)
			}
			if se != nil {
				if se.StatusCode != tc.want.status || se.Host != tc.host {
					t.Errorf("StatusError status %d host %q, want %d %q", se.StatusCode, se.Host, tc.want.status, tc.host)
				}
			} else if tc.host != "" && !strings.Contains(err.Error(), tc.host) {
				t.Errorf("err %q lost the provider host %q", err.Error(), tc.host)
			}
		})
	}
}

// Row 29 — INV-9 for a send refused by the address guard: the transport's
// *url.Error quotes the endpoint; no level of the returned chain carries it
// and the refusal still classifies as ErrBlockedAddress.
func TestSend_BlockedAddressErrorCarriesNoSecret(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	const pathToken = "row29-blocked-token-0123456789"
	sub := testSubscription(srv.URL + "/push/" + pathToken + "?token=row29-blocked-query-token")
	s := NewSender(&fakePruner{}, NewGuardedClient(), realVAPIDPublic, realVAPIDPrivate, "mailto:test@skein.local")
	s.validate = func(string) error { return nil } // the URL check is not under test here

	err := sendLogged(t, s, sub, []string{sub.Endpoint, pathToken, "row29-blocked-query-token", sub.P256dh, sub.Auth})
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("Send to a loopback push server: err = %v, want ErrBlockedAddress", err)
	}
}

// chainTexts is the text of every error reachable from err through Unwrap,
// single or multiple — what a caller can print at any level of the chain.
func chainTexts(err error) []string {
	var out []string
	queue := []error{err}
	for len(queue) > 0 {
		e := queue[0]
		queue = queue[1:]
		if e == nil {
			continue
		}
		out = append(out, e.Error())
		switch u := e.(type) {
		case interface{ Unwrap() error }:
			queue = append(queue, u.Unwrap())
		case interface{ Unwrap() []error }:
			queue = append(queue, u.Unwrap()...)
		}
	}
	return out
}

// sendLogged runs Send with the production log handler capturing its
// records and fails when the returned error's text or any record carries one
// of secrets.
func sendLogged(t *testing.T, s *Sender, sub Subscription, secrets []string) error {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewDualHandler(&buf, slog.LevelDebug)))
	defer slog.SetDefault(prev)

	err := s.Send(context.Background(), sub, []byte(`{}`))

	var se *StatusError
	texts := map[string]string{"log": buf.String()}
	for i, text := range chainTexts(err) {
		texts[fmt.Sprintf("chain level %d Error()", i)] = text
	}
	if errors.As(err, &se) {
		texts["StatusError.Body"] = se.Body
		texts["StatusError.BodyReadError"] = se.BodyReadError
	}
	for where, text := range texts {
		for _, secret := range secrets {
			if secret != "" && strings.Contains(text, secret) {
				t.Errorf("%s carries the subscription secret %q: %s", where, secret, text)
			}
		}
	}
	return err
}
