package mail

// smtp.go — the SMTP adapter of the mail worker (sk-674.18, T022, K-10) and
// the classification of its failures (K-9).
//
// The adapter is the standard library — net/smtp over crypto/tls — and
// never talks to the relay in the clear: SKEIN_SMTP_TLS_MODE=implicit
// establishes TLS before the greeting (usually port 465), starttls requires
// the relay to offer STARTTLS and upgrades before AUTH and DATA. A relay in
// starttls mode that does not offer it is refused without sending anything;
// so is a certificate the system roots do not trust. There is no plaintext
// mode (config.Load accepts only these two values).
//
// # Temporary and permanent failures
//
// K-9 (amended 2026-10-13, plan Changelog) splits a failed attempt in two
// by the STAGE the relay answered at:
//
//   - PERMANENT — the relay refused THIS email: a 5xx reply at the recipient
//     and content stages (RCPT TO, DATA), or a recipient address that is not
//     an address at all. Retrying cannot help; the email is finalised as
//     rejected.
//   - TEMPORARY — everything else: a 4xx reply, a network error, a TLS
//     failure, a timeout, a relay without STARTTLS, and a 5xx reply at the
//     sender and configuration stages (greeting, EHLO, STARTTLS, AUTH,
//     MAIL FROM). That last group is a refusal of the instance's SMTP
//     configuration — credentials, the sender — not of the email: a
//     misconfiguration would otherwise reject every queued email at once,
//     while after it is fixed the emails are delivered within their
//     deadline.
//
// [Classify] is the single place that decides; the worker never inspects an
// error itself.

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	netmail "net/mail"
	"net/smtp"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"time"

	"skein-api/internal/config"
)

// Sender delivers one rendered message to one recipient. The mail worker
// sees only this interface; *SMTPSender is the production implementation.
type Sender interface {
	// Send hands msg (a complete RFC 5322 message) to the relay for to.
	// The error, if any, is classified by [Classify].
	Send(ctx context.Context, to string, msg []byte) error
	// From is the mailbox the From header carries.
	From() string
}

// Stage names the SMTP step a failure happened at. It decides whether a
// 5xx reply is about the email (permanent) or about the connection
// (temporary).
type Stage string

const (
	StageAddress  Stage = "address"  // the recipient address, before any I/O
	StageConnect  Stage = "connect"  // dial, implicit TLS handshake, greeting
	StageHello    Stage = "hello"    // EHLO / HELO
	StageStartTLS Stage = "starttls" // the STARTTLS upgrade
	StageAuth     Stage = "auth"     // AUTH
	StageMail     Stage = "mail"     // MAIL FROM
	StageRcpt     Stage = "rcpt"     // RCPT TO
	StageData     Stage = "data"     // DATA and the message body
)

// SendError is a failed delivery attempt: the stage it failed at and the
// cause. Its Error text may contain what the relay answered; callers that
// log it pass it through [redact] first.
type SendError struct {
	Stage Stage
	Err   error
}

func (e *SendError) Error() string { return fmt.Sprintf("smtp %s: %v", e.Stage, e.Err) }
func (e *SendError) Unwrap() error { return e.Err }

// Class is the verdict on a failed attempt.
type Class string

const (
	// ClassTemporary — retry with back-off until the email's deadline.
	ClassTemporary Class = "temporary"
	// ClassPermanent — finalise the email as rejected, no retry.
	ClassPermanent Class = "permanent"
)

// errInvalidAddress: the recipient is not a single plain address.
var errInvalidAddress = errors.New("recipient is not a valid address")

// errNoStartTLS: starttls mode, but the relay does not offer STARTTLS.
var errNoStartTLS = errors.New("relay does not offer STARTTLS; refusing to continue in the clear")

// Classify decides whether a failed attempt is retried (K-9; see the file
// header for the rule). A nil error has no class and must not be passed.
func Classify(err error) Class {
	var se *SendError
	if !errors.As(err, &se) {
		return ClassTemporary
	}
	if se.Stage == StageAddress {
		return ClassPermanent
	}
	var te *textproto.Error
	if !errors.As(se.Err, &te) {
		return ClassTemporary
	}
	if te.Code >= 500 && te.Code < 600 {
		switch se.Stage {
		case StageRcpt, StageData:
			return ClassPermanent
		}
	}
	return ClassTemporary
}

// SMTPCode is the reply code of a failed attempt, 0 when the relay did not
// answer with one (network, TLS, address). For logs.
func SMTPCode(err error) int {
	var te *textproto.Error
	if errors.As(err, &te) {
		return te.Code
	}
	return 0
}

// FailureStage is the stage of a failed attempt, "" when unknown. For logs.
func FailureStage(err error) Stage {
	var se *SendError
	if errors.As(err, &se) {
		return se.Stage
	}
	return ""
}

// Timeouts of one SMTP session. Together they stay well inside the
// worker's lease (2 min), so a send never outlives the lease it runs under.
const (
	smtpDialTimeout    = 15 * time.Second
	smtpSessionTimeout = 60 * time.Second
)

// SMTPSender is the production [Sender]: one connection per email.
type SMTPSender struct {
	host     string
	port     int
	username string
	password string
	// envelope is the bare sender address of MAIL FROM; header is the
	// mailbox of the From header — the same address, with the display name
	// SKEIN_SMTP_FROM gives.
	envelope string
	header   string
	implicit bool
	tls      *tls.Config
	helo     string
}

// NewSMTPSender builds the adapter from the loaded configuration (the
// SKEIN_SMTP_* variables, K-10). tlsBase is the TLS configuration to start
// from — nil in production, meaning the system roots; ServerName is always
// set to SKEIN_SMTP_HOST and certificate verification is never disabled.
func NewSMTPSender(cfg *config.Config, tlsBase *tls.Config) (*SMTPSender, error) {
	if cfg == nil || !cfg.EmailEnabled {
		return nil, errors.New("mail: SMTP sender needs SKEIN_EMAIL_ENABLED=true")
	}
	var implicit bool
	switch cfg.SMTPTLSMode {
	case config.SMTPTLSImplicit:
		implicit = true
	case config.SMTPTLSStartTLS:
	default:
		return nil, fmt.Errorf("mail: unknown SMTP TLS mode %q", cfg.SMTPTLSMode)
	}
	if strings.ContainsAny(cfg.SMTPFrom, "\r\n") {
		return nil, errors.New("mail: SKEIN_SMTP_FROM contains a line break")
	}
	sender, err := netmail.ParseAddress(cfg.SMTPFrom)
	if err != nil {
		return nil, fmt.Errorf("mail: SKEIN_SMTP_FROM is not an address: %w", err)
	}
	header := sender.Address
	if sender.Name != "" {
		header = sender.String() // RFC 5322 mailbox, the name encoded as needed
	}
	var tc *tls.Config
	if tlsBase != nil {
		tc = tlsBase.Clone()
	} else {
		tc = &tls.Config{}
	}
	tc.ServerName = cfg.SMTPHost
	tc.InsecureSkipVerify = false
	if tc.MinVersion < tls.VersionTLS12 {
		tc.MinVersion = tls.VersionTLS12
	}
	helo, herr := os.Hostname()
	if herr != nil || helo == "" {
		helo = "localhost"
	}
	return &SMTPSender{
		host:     cfg.SMTPHost,
		port:     cfg.SMTPPort,
		username: cfg.SMTPUsername,
		password: cfg.SMTPPassword,
		envelope: sender.Address,
		header:   header,
		implicit: implicit,
		tls:      tc,
		helo:     helo,
	}, nil
}

// From implements Sender: the mailbox of the From header.
func (s *SMTPSender) From() string { return s.header }

// Send implements Sender: dial (TLS first in implicit mode), EHLO,
// STARTTLS in starttls mode, AUTH PLAIN, MAIL, RCPT, DATA. The whole
// session is bounded by ctx and by smtpSessionTimeout.
func (s *SMTPSender) Send(ctx context.Context, to string, msg []byte) error {
	if err := checkRecipient(to); err != nil {
		return &SendError{Stage: StageAddress, Err: err}
	}
	ctx, cancel := context.WithTimeout(ctx, smtpSessionTimeout)
	defer cancel()

	addr := net.JoinHostPort(s.host, strconv.Itoa(s.port))
	dialer := &net.Dialer{Timeout: smtpDialTimeout}
	var (
		conn net.Conn
		err  error
	)
	if s.implicit {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: s.tls}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return &SendError{Stage: StageConnect, Err: err}
	}
	// net/smtp has no context: the deadline of the connection is what
	// bounds every read and write of the session, and closing it on
	// cancellation unblocks whatever is in flight.
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	c, err := smtp.NewClient(conn, s.host)
	if err != nil {
		_ = conn.Close()
		return &SendError{Stage: StageConnect, Err: err}
	}
	defer c.Close()

	if err := c.Hello(s.helo); err != nil {
		return &SendError{Stage: StageHello, Err: err}
	}
	if !s.implicit {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return &SendError{Stage: StageStartTLS, Err: errNoStartTLS}
		}
		if err := c.StartTLS(s.tls); err != nil {
			return &SendError{Stage: StageStartTLS, Err: err}
		}
	}
	// PlainAuth itself refuses to send the password over a connection that
	// is not TLS — a second guard behind the two above.
	if err := c.Auth(smtp.PlainAuth("", s.username, s.password, s.host)); err != nil {
		return &SendError{Stage: StageAuth, Err: err}
	}
	if err := c.Mail(s.envelope); err != nil {
		return &SendError{Stage: StageMail, Err: err}
	}
	if err := c.Rcpt(to); err != nil {
		return &SendError{Stage: StageRcpt, Err: err}
	}
	w, err := c.Data()
	if err != nil {
		return &SendError{Stage: StageData, Err: err}
	}
	if _, err := w.Write(msg); err != nil {
		_ = w.Close()
		return &SendError{Stage: StageData, Err: err}
	}
	// The reply to the end of DATA is the relay taking the message over:
	// from here on the email is delivered, whatever QUIT answers.
	if err := w.Close(); err != nil {
		return &SendError{Stage: StageData, Err: err}
	}
	_ = c.Quit()
	return nil
}

// checkRecipient accepts a single bare address and nothing that could add
// a header line or a second recipient.
func checkRecipient(to string) error {
	if strings.ContainsAny(to, "\r\n") {
		return errInvalidAddress
	}
	a, err := netmail.ParseAddress(to)
	if err != nil || a.Name != "" || a.Address != to {
		return errInvalidAddress
	}
	return nil
}

// redact removes from a failure text what must never reach a log line
// (the project guidelines, "What NOT to log"): every email address — a
// relay often quotes the recipient back ("550 5.1.1 <a@b>: unknown") —
// and the raw token, should a relay echo part of the message.
func redact(msg, token string) string {
	if token != "" {
		msg = strings.ReplaceAll(msg, token, "***")
	}
	return addressPattern.ReplaceAllString(msg, "***@***")
}

// buildMessage is the RFC 5322 message: headers, then a multipart/
// alternative body with the plain-text and the HTML part. Header values are
// fixed or come from our own templates; the recipient was checked by
// checkRecipient before the message is built.
func buildMessage(from, to, messageID string, date time.Time, r Rendered) []byte {
	var b bytes.Buffer
	boundary := "skein-" + strings.ReplaceAll(messageID, "-", "")
	hdr := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	hdr("From", from)
	hdr("To", to)
	hdr("Subject", encodeHeader(r.Subject))
	hdr("Date", date.UTC().Format(time.RFC1123Z))
	hdr("Message-ID", "<"+messageID+"@"+domainOf(from)+">")
	hdr("MIME-Version", "1.0")
	hdr("Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
	b.WriteString("\r\n")
	writePart(&b, boundary, "text/plain; charset=utf-8", r.Text)
	writePart(&b, boundary, "text/html; charset=utf-8", r.HTML)
	b.WriteString("--" + boundary + "--\r\n")
	return b.Bytes()
}

func domainOf(addr string) string {
	if a, err := netmail.ParseAddress(addr); err == nil {
		addr = a.Address
	}
	if i := strings.LastIndexByte(addr, '@'); i >= 0 && i < len(addr)-1 {
		return addr[i+1:]
	}
	return "localhost"
}
