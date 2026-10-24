package mail_test

// smtp_test.go — the SMTP adapter and the failure classifier (sk-674.18,
// T022; matrix rows 68, 68a).
//
// Row 68 is the classifier on failures shaped as the adapter reports them.
// Row 68a is OUR configuration of net/smtp + crypto/tls (§5 "our
// configuration"): the adapter is built from a configuration loaded by
// config.Load and talks to a local SMTP endpoint in this process, which
// records for every command whether the connection was already encrypted.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"skein-api/internal/config"
	"skein-api/internal/mail"
)

// Row 68 (K-9 as amended 2026-10-13, plan Changelog): a 5xx reply at the
// recipient and content stages (RCPT, DATA), or a recipient that is not an
// address, is permanent; a 5xx at the sender and configuration stages
// (connection, HELO/EHLO, STARTTLS, AUTH, MAIL FROM), a 4xx reply, a network
// or TLS failure and a timeout are temporary. Red when a class is swapped.
func TestClassify_PermanentAndTemporaryFailures(t *testing.T) {
	reply := func(code int) error { return &textproto.Error{Code: code, Msg: "relay says no"} }
	cases := []struct {
		name string
		err  error
		want mail.Class
	}{
		{"550 to RCPT", &mail.SendError{Stage: mail.StageRcpt, Err: reply(550)}, mail.ClassPermanent},
		{"554 to DATA", &mail.SendError{Stage: mail.StageData, Err: reply(554)}, mail.ClassPermanent},
		{"invalid recipient address", &mail.SendError{Stage: mail.StageAddress, Err: errors.New("recipient is not a valid address")}, mail.ClassPermanent},

		{"450 to RCPT", &mail.SendError{Stage: mail.StageRcpt, Err: reply(450)}, mail.ClassTemporary},
		{"421 at the greeting", &mail.SendError{Stage: mail.StageConnect, Err: reply(421)}, mail.ClassTemporary},
		{"451 to DATA", &mail.SendError{Stage: mail.StageData, Err: reply(451)}, mail.ClassTemporary},
		{"connection refused", &mail.SendError{Stage: mail.StageConnect, Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}}, mail.ClassTemporary},
		{"TLS certificate not trusted", &mail.SendError{Stage: mail.StageConnect, Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}}, mail.ClassTemporary},
		{"timeout", &mail.SendError{Stage: mail.StageData, Err: context.DeadlineExceeded}, mail.ClassTemporary},
		{"5xx to AUTH (credentials)", &mail.SendError{Stage: mail.StageAuth, Err: reply(535)}, mail.ClassTemporary},
		{"5xx to EHLO", &mail.SendError{Stage: mail.StageHello, Err: reply(502)}, mail.ClassTemporary},
		{"5xx to STARTTLS", &mail.SendError{Stage: mail.StageStartTLS, Err: reply(554)}, mail.ClassTemporary},
		{"553 to MAIL FROM (sender refused)", &mail.SendError{Stage: mail.StageMail, Err: reply(553)}, mail.ClassTemporary},
		{"an error without a stage", errors.New("something broke"), mail.ClassTemporary},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mail.Classify(c.err); got != c.want {
				t.Errorf("Classify(%v) = %s, want %s", c.err, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------- endpoint

// command is one SMTP command the endpoint received.
type command struct {
	verb      string
	encrypted bool
}

// endpoint is a minimal SMTP relay on 127.0.0.1.
type endpoint struct {
	ln        net.Listener
	tlsConfig *tls.Config
	implicit  bool // TLS from the first byte
	startTLS  bool // advertise STARTTLS on the clear connection

	mu       sync.Mutex
	accepted int
	// tlsBeforeGreeting: an implicit connection finished its handshake
	// before the endpoint sent the greeting.
	tlsBeforeGreeting bool
	commands          []command
	data              string
	mailFrom          string // the argument of the last MAIL command
	wg                sync.WaitGroup
}

func newEndpoint(t *testing.T, cert tls.Certificate, implicit, startTLS bool) *endpoint {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e := &endpoint{
		ln:        ln,
		tlsConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		implicit:  implicit,
		startTLS:  startTLS,
	}
	e.wg.Add(1)
	go e.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		e.wg.Wait()
	})
	return e
}

func (e *endpoint) port() int { return e.ln.Addr().(*net.TCPAddr).Port }

func (e *endpoint) serve() {
	defer e.wg.Done()
	for {
		c, err := e.ln.Accept()
		if err != nil {
			return
		}
		e.mu.Lock()
		e.accepted++
		e.mu.Unlock()
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			e.session(c)
		}()
	}
}

func (e *endpoint) record(verb string, encrypted bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.commands = append(e.commands, command{verb, encrypted})
}

func (e *endpoint) session(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	encrypted := false
	if e.implicit {
		tc := tls.Server(conn, e.tlsConfig)
		if err := tc.Handshake(); err != nil {
			return
		}
		e.mu.Lock()
		e.tlsBeforeGreeting = true
		e.mu.Unlock()
		conn, encrypted = tc, true
	}
	tp := textproto.NewConn(conn)
	reply := func(s string) { _ = tp.PrintfLine("%s", s) }
	reply("220 endpoint ESMTP")
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb := strings.ToUpper(strings.Fields(line + " x")[0])
		e.record(verb, encrypted)
		switch verb {
		case "EHLO", "HELO":
			lines := []string{"250-endpoint"}
			if e.startTLS && !encrypted {
				lines = append(lines, "250-STARTTLS")
			}
			lines = append(lines, "250-AUTH PLAIN", "250 8BITMIME")
			for _, l := range lines {
				reply(l)
			}
		case "STARTTLS":
			if !e.startTLS || encrypted {
				reply("502 not offered")
				continue
			}
			reply("220 go ahead")
			tc := tls.Server(conn, e.tlsConfig)
			if err := tc.Handshake(); err != nil {
				return
			}
			conn, encrypted = tc, true
			tp = textproto.NewConn(conn)
		case "AUTH":
			reply("235 accepted")
		case "MAIL", "RCPT":
			if verb == "MAIL" {
				e.mu.Lock()
				e.mailFrom = strings.TrimSpace(line[len("MAIL"):])
				e.mu.Unlock()
			}
			reply("250 ok")
		case "DATA":
			reply("354 go on")
			b, err := tp.ReadDotBytes()
			if err != nil {
				return
			}
			e.mu.Lock()
			e.data = string(b)
			e.mu.Unlock()
			reply("250 queued")
		case "QUIT":
			reply("221 bye")
			return
		default:
			reply("500 unknown")
		}
	}
}

func (e *endpoint) snapshot() (accepted int, tlsFirst bool, cmds []command, data string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.accepted, e.tlsBeforeGreeting, append([]command(nil), e.commands...), e.data
}

// selfSigned is a certificate for 127.0.0.1 and the pool that trusts it.
func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "smtp endpoint"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// loadMailConfig loads the configuration from the environment the way the
// process does, with mail on and the relay at 127.0.0.1:port.
func loadMailConfig(t *testing.T, port int, mode string) *config.Config {
	t.Helper()
	return loadMailConfigFrom(t, port, mode, "skein@example.com")
}

func loadMailConfigFrom(t *testing.T, port int, mode, from string) *config.Config {
	t.Helper()
	for k, v := range map[string]string{
		"DATABASE_URL":                      "postgres://u:p@localhost:5432/db?sslmode=disable",
		"SKEIN_INTERNAL_SECRET":             "0123456789abcdef0123",
		"SKEIN_AGENT_PUBLIC_URL":            "https://mcp.example.com",
		"ENVIRONMENT":                       "development",
		"SKEIN_OAUTH_AUTHORIZATION_SERVERS": "https://auth.example.com/realms/main",
		"SKEIN_VAPID_PUBLIC":                "",
		"SKEIN_VAPID_PRIVATE":               "",
		"SKEIN_EMAIL_ENABLED":               "true",
		"SKEIN_SMTP_HOST":                   "127.0.0.1",
		"SKEIN_SMTP_PORT":                   strconv.Itoa(port),
		"SKEIN_SMTP_USERNAME":               "skein",
		"SKEIN_SMTP_PASSWORD":               "smtp-secret",
		"SKEIN_SMTP_FROM":                   from,
		"SKEIN_SMTP_TLS_MODE":               mode,
		"SKEIN_APP_BASE_URL":                "https://skein.example.com",
	} {
		t.Setenv(k, v)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

func send(t *testing.T, cfg *config.Config, roots *x509.CertPool, to string) error {
	t.Helper()
	s, err := mail.NewSMTPSender(cfg, &tls.Config{RootCAs: roots})
	if err != nil {
		t.Fatalf("NewSMTPSender: %v", err)
	}
	msg := []byte("Subject: probe\r\n\r\nhello relay\r\n")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return s.Send(ctx, to, msg)
}

// sensitive are the commands that carry the credentials or the email.
var sensitive = map[string]bool{"AUTH": true, "MAIL": true, "RCPT": true, "DATA": true}

// Row 68a (K-10): implicit mode is TLS before the greeting; starttls mode
// upgrades before AUTH and DATA; a starttls relay that does not offer
// STARTTLS, and a certificate nobody trusts, are refused with nothing sent
// in the clear — temporary failures, the email waits for a fixed relay.
// Red if the adapter ever sends AUTH, MAIL, RCPT or DATA unencrypted.
func TestSMTPSender_NeverTalksInTheClear(t *testing.T) {
	cert, trusted := selfSigned(t)
	_, foreign := selfSigned(t)

	t.Run("implicit: TLS before the greeting", func(t *testing.T) {
		e := newEndpoint(t, cert, true, false)
		if err := send(t, loadMailConfig(t, e.port(), config.SMTPTLSImplicit), trusted, "someone@example.org"); err != nil {
			t.Fatalf("Send: %v", err)
		}
		_, tlsFirst, cmds, data := e.snapshot()
		if !tlsFirst {
			t.Error("the handshake did not complete before the greeting")
		}
		assertAllEncrypted(t, cmds)
		if !strings.Contains(data, "hello relay") {
			t.Errorf("the endpoint did not receive the message: %q", data)
		}
	})

	t.Run("starttls: upgrade before AUTH and DATA", func(t *testing.T) {
		e := newEndpoint(t, cert, false, true)
		if err := send(t, loadMailConfig(t, e.port(), config.SMTPTLSStartTLS), trusted, "someone@example.org"); err != nil {
			t.Fatalf("Send: %v", err)
		}
		_, _, cmds, data := e.snapshot()
		sawStartTLS := false
		for _, c := range cmds {
			if c.verb == "STARTTLS" {
				sawStartTLS = true
			}
		}
		if !sawStartTLS {
			t.Error("the adapter never sent STARTTLS")
		}
		assertAllEncrypted(t, cmds)
		if !strings.Contains(data, "hello relay") {
			t.Errorf("the endpoint did not receive the message: %q", data)
		}
	})

	t.Run("starttls mode, relay without STARTTLS: refused", func(t *testing.T) {
		e := newEndpoint(t, cert, false, false)
		err := send(t, loadMailConfig(t, e.port(), config.SMTPTLSStartTLS), trusted, "someone@example.org")
		assertRefusedInTheClear(t, e, err)
	})

	t.Run("implicit, certificate not trusted: refused", func(t *testing.T) {
		e := newEndpoint(t, cert, true, false)
		err := send(t, loadMailConfig(t, e.port(), config.SMTPTLSImplicit), foreign, "someone@example.org")
		assertRefusedInTheClear(t, e, err)
	})

	t.Run("starttls, certificate not trusted: refused", func(t *testing.T) {
		e := newEndpoint(t, cert, false, true)
		err := send(t, loadMailConfig(t, e.port(), config.SMTPTLSStartTLS), foreign, "someone@example.org")
		assertRefusedInTheClear(t, e, err)
	})

	// SKEIN_SMTP_FROM may name the sender; the SMTP
	// envelope still carries the bare address — a relay refuses
	// MAIL FROM:<Name <addr>> — and the header carries the named mailbox.
	t.Run("sender with a display name: bare envelope, named header", func(t *testing.T) {
		e := newEndpoint(t, cert, true, false)
		cfg := loadMailConfigFrom(t, e.port(), config.SMTPTLSImplicit, "Skein Mail <skein@example.com>")
		s, err := mail.NewSMTPSender(cfg, &tls.Config{RootCAs: trusted})
		if err != nil {
			t.Fatalf("NewSMTPSender: %v", err)
		}
		if err := s.Send(context.Background(), "someone@example.org", []byte("Subject: probe\r\n\r\nhello relay\r\n")); err != nil {
			t.Fatalf("Send: %v", err)
		}
		e.mu.Lock()
		got := e.mailFrom
		e.mu.Unlock()
		// net/smtp may append ESMTP parameters (BODY=8BITMIME); the envelope
		// is the first field.
		if env := strings.Fields(got + " ")[0]; env != "FROM:<skein@example.com>" {
			t.Errorf("MAIL %s, want the envelope FROM:<skein@example.com>", got)
		}
		if h := s.From(); h != `"Skein Mail" <skein@example.com>` {
			t.Errorf("From header = %q, want the named mailbox", h)
		}
	})

	t.Run("recipient that is not an address: permanent, no connection", func(t *testing.T) {
		e := newEndpoint(t, cert, true, false)
		err := send(t, loadMailConfig(t, e.port(), config.SMTPTLSImplicit), trusted, "victim@example.org\r\nBcc: other@example.org")
		if err == nil || mail.Classify(err) != mail.ClassPermanent {
			t.Fatalf("Send = %v (class %s), want a permanent failure", err, classOf(err))
		}
		if accepted, _, _, _ := e.snapshot(); accepted != 0 {
			t.Errorf("the adapter connected %d times for an invalid recipient", accepted)
		}
	})
}

func classOf(err error) string {
	if err == nil {
		return "none"
	}
	return string(mail.Classify(err))
}

func assertAllEncrypted(t *testing.T, cmds []command) {
	t.Helper()
	seen := map[string]bool{}
	for _, c := range cmds {
		if sensitive[c.verb] {
			seen[c.verb] = true
			if !c.encrypted {
				t.Errorf("%s was sent in the clear", c.verb)
			}
		}
	}
	for verb := range sensitive {
		if !seen[verb] {
			t.Errorf("precondition: the endpoint never saw %s", verb)
		}
	}
}

func assertRefusedInTheClear(t *testing.T, e *endpoint, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("Send succeeded, want a refusal")
	}
	if c := mail.Classify(err); c != mail.ClassTemporary {
		t.Errorf("Classify = %s, want temporary (the relay, not the email, is at fault)", c)
	}
	accepted, _, cmds, data := e.snapshot()
	if accepted == 0 {
		t.Fatalf("precondition: the adapter never reached the endpoint (%v)", err)
	}
	for _, c := range cmds {
		if sensitive[c.verb] {
			t.Errorf("%s reached the endpoint (encrypted=%v) — the adapter continued after the refusal", c.verb, c.encrypted)
		}
	}
	if data != "" {
		t.Errorf("the message reached the endpoint: %s", fmt.Sprintf("%q", data))
	}
}
