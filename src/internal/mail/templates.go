package mail

// templates.go — the texts of the three emails (sk-674.18, T022, K-9, US-9).
//
// Each kind has, per language (ru, en), three files under templates/<lang>/:
// <kind>.subject.txt, <kind>.txt (the plain-text part) and <kind>.html (the
// HTML part). They are embedded in the binary and parsed once; the template
// is chosen by its KEY (kind, language), never by inspecting text.
//
// Text a user typed — the name of the space and of the person inviting —
// reaches an email only through [Render], which strips control characters,
// collapses it to one line and cuts it to maxUserTextRunes, and through
// html/template, which escapes it in the HTML part: a space named
// `<a href=…>` arrives as text, never as markup (spec "Restrictions"). The
// subjects carry no user text at all, so nothing typed can reach a header.

import (
	"bytes"
	"embed"
	"fmt"
	htmltemplate "html/template"
	"mime"
	"mime/quotedprintable"
	"regexp"
	"strings"
	texttemplate "text/template"
	"unicode"
)

//go:embed templates
var templateFS embed.FS

// maxUserTextRunes bounds a user-typed name inside an email.
const maxUserTextRunes = 80

// Data is what a template may show. Link carries the raw token and is
// built in memory for one attempt; nothing of Data is stored.
type Data struct {
	// Link is the absolute URL the email asks the recipient to open.
	Link string
	// InviterName and SpaceName — space invites only; user text, passed
	// through [Render]'s sanitising.
	InviterName string
	SpaceName   string
	// ExpiresAt is when the link stops working, already formatted
	// ("2006-01-02 15:04 UTC"); the invitation's expiry for an invite.
	ExpiresAt string
}

// Rendered is one email ready for the wire.
type Rendered struct {
	Subject string
	Text    string
	HTML    string
}

type templateSet struct {
	subject *texttemplate.Template
	text    *texttemplate.Template
	html    *htmltemplate.Template
}

type templateKey struct {
	kind Kind
	lang string
}

// templates holds every (kind, language) pair; a missing or broken file
// stops the process at start-up rather than at the first email.
var templates = mustLoadTemplates()

func mustLoadTemplates() map[templateKey]templateSet {
	out := map[templateKey]templateSet{}
	for _, lang := range []string{LangRU, LangEN} {
		for _, kind := range []Kind{KindEmailConfirm, KindPasswordReset, KindSpaceInvite} {
			base := "templates/" + lang + "/" + string(kind)
			read := func(name string) string {
				b, err := templateFS.ReadFile(name)
				if err != nil {
					panic(fmt.Sprintf("mail: template %s: %v", name, err))
				}
				return string(b)
			}
			opt := "missingkey=error"
			out[templateKey{kind, lang}] = templateSet{
				subject: texttemplate.Must(texttemplate.New(base + ".subject.txt").Option(opt).Parse(read(base + ".subject.txt"))),
				text:    texttemplate.Must(texttemplate.New(base + ".txt").Option(opt).Parse(read(base + ".txt"))),
				html:    htmltemplate.Must(htmltemplate.New(base + ".html").Option(opt).Parse(read(base + ".html"))),
			}
		}
	}
	return out
}

// Render produces the email of kind in lang. User-typed fields of d are
// sanitised here; the caller passes them as stored.
func Render(kind Kind, lang string, d Data) (Rendered, error) {
	set, ok := templates[templateKey{kind, lang}]
	if !ok {
		return Rendered{}, fmt.Errorf("%w: no template for %s/%s", ErrInvalidMessage, kind, lang)
	}
	d.InviterName = sanitizeUserText(d.InviterName)
	d.SpaceName = sanitizeUserText(d.SpaceName)

	var subj, text, html bytes.Buffer
	if err := set.subject.Execute(&subj, d); err != nil {
		return Rendered{}, fmt.Errorf("rendering %s/%s subject: %w", kind, lang, err)
	}
	if err := set.text.Execute(&text, d); err != nil {
		return Rendered{}, fmt.Errorf("rendering %s/%s text: %w", kind, lang, err)
	}
	if err := set.html.Execute(&html, d); err != nil {
		return Rendered{}, fmt.Errorf("rendering %s/%s html: %w", kind, lang, err)
	}
	return Rendered{
		Subject: strings.TrimSpace(subj.String()),
		Text:    text.String(),
		HTML:    html.String(),
	}, nil
}

// sanitizeUserText makes a user-typed name safe to place in a sentence:
// control characters (line breaks included) become spaces, runs of space
// collapse, and the result is cut to maxUserTextRunes with an ellipsis.
func sanitizeUserText(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxUserTextRunes {
		s = strings.TrimSpace(string(r[:maxUserTextRunes-1])) + "…"
	}
	return s
}

// addressPattern matches anything shaped like an email address, for
// [redact].
var addressPattern = regexp.MustCompile(`[^\s<>"'(),;:]+@[^\s<>"'(),;:]+`)

// encodeHeader encodes a header value as RFC 2047 when it is not ASCII.
func encodeHeader(v string) string {
	return mime.QEncoding.Encode("utf-8", v)
}

// writePart writes one quoted-printable part of a multipart body.
func writePart(b *bytes.Buffer, boundary, contentType, body string) {
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: " + contentType + "\r\n")
	b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	w := quotedprintable.NewWriter(b)
	_, _ = w.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")))
	_ = w.Close()
	b.WriteString("\r\n")
}
