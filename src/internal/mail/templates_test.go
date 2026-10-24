package mail_test

// templates_test.go — the texts of the emails (sk-674.18, T022; matrix row
// 73): what a user typed reaches an email as text, never as markup, and
// limited in length; the template is chosen by its language key.

import (
	"errors"
	"html"
	"regexp"
	"strings"
	"testing"

	"skein-api/internal/mail"
)

// tagPattern matches the start or end of an HTML element in rendered output.
var tagPattern = regexp.MustCompile(`</?[a-zA-Z][a-zA-Z0-9]*`)

func tags(s string) []string { return tagPattern.FindAllString(s, -1) }

// Row 73 ("Restrictions"): the names of the space and of the inviter are
// escaped in the HTML part — `<script>` and `<a href>` stay text: the
// rendered document has exactly the elements it has with harmless names —
// and they read back as the text the user typed. Red if user text becomes
// markup.
func TestRender_UserTextIsTextNotMarkup(t *testing.T) {
	hostileInviter := `<a href="https://evil.example/phish">Support</a>`
	hostileSpace := `<script>alert(1)</script>Team`
	for _, lang := range []string{mail.LangRU, mail.LangEN} {
		t.Run(lang, func(t *testing.T) {
			control, err := mail.Render(mail.KindSpaceInvite, lang, mail.Data{
				Link: "https://skein.example.com/invite/tok", InviterName: "Alice", SpaceName: "Team", ExpiresAt: "2026-01-01 00:00 UTC"})
			if err != nil {
				t.Fatal(err)
			}
			got, err := mail.Render(mail.KindSpaceInvite, lang, mail.Data{
				Link: "https://skein.example.com/invite/tok", InviterName: hostileInviter, SpaceName: hostileSpace, ExpiresAt: "2026-01-01 00:00 UTC"})
			if err != nil {
				t.Fatal(err)
			}
			if a, b := tags(control.HTML), tags(got.HTML); strings.Join(a, " ") != strings.Join(b, " ") {
				t.Errorf("user text changed the document's elements:\nwith harmless names: %v\nwith hostile names:  %v", a, b)
			}
			text := html.UnescapeString(got.HTML)
			for _, typed := range []string{hostileInviter, hostileSpace} {
				if !strings.Contains(text, typed) {
					t.Errorf("the HTML part does not show %q as text", typed)
				}
				if !strings.Contains(got.Text, typed) {
					t.Errorf("the text part does not show %q", typed)
				}
				if strings.Contains(got.Subject, typed) {
					t.Errorf("user text reached the subject: %q", got.Subject)
				}
			}
		})
	}
}

// Row 73: a name is limited in length — a long one is cut, not dropped —
// and a line break typed into it does not start a new line of the email.
func TestRender_UserTextIsLimitedToOneShortLine(t *testing.T) {
	long := strings.Repeat("Пространство", 50) // 600 runes
	got, err := mail.Render(mail.KindSpaceInvite, mail.LangRU, mail.Data{
		Link: "https://skein.example.com/invite/tok", InviterName: "Bob\r\nBcc: x@example.org", SpaceName: long, ExpiresAt: "z"})
	if err != nil {
		t.Fatal(err)
	}
	for part, body := range map[string]string{"text": got.Text, "html": html.UnescapeString(got.HTML)} {
		if strings.Contains(body, long) {
			t.Errorf("%s part carries the full %d-rune name", part, len([]rune(long)))
		}
		if !strings.Contains(body, strings.Repeat("Пространство", 3)) {
			t.Errorf("%s part lost the name instead of cutting it", part)
		}
		if strings.Contains(body, "\nBcc:") || strings.Contains(body, "\rBcc:") {
			t.Errorf("%s part: a line break typed into a name started a new line", part)
		}
	}
}

// Row 73: the template is chosen by the language key — each language
// renders its own document (the HTML root declares it), every kind exists
// in both, and a language without templates is refused, not defaulted.
func TestRender_TemplateChosenByLanguageKey(t *testing.T) {
	d := mail.Data{Link: "https://skein.example.com/x/tok", InviterName: "A", SpaceName: "S", ExpiresAt: "z"}
	for _, kind := range []mail.Kind{mail.KindEmailConfirm, mail.KindPasswordReset, mail.KindSpaceInvite} {
		for _, lang := range []string{mail.LangRU, mail.LangEN} {
			got, err := mail.Render(kind, lang, d)
			if err != nil {
				t.Fatalf("%s/%s: %v", kind, lang, err)
			}
			if !strings.Contains(got.HTML, `<html lang="`+lang+`">`) {
				t.Errorf("%s/%s: the HTML part is not the %s document", kind, lang, lang)
			}
			if got.Subject == "" || !strings.Contains(got.Text, d.Link) || !strings.Contains(got.HTML, d.Link) {
				t.Errorf("%s/%s: subject %q, or the link is missing from a part", kind, lang, got.Subject)
			}
		}
		ru, _ := mail.Render(kind, mail.LangRU, d)
		en, _ := mail.Render(kind, mail.LangEN, d)
		if ru.Subject == en.Subject || ru.Text == en.Text {
			t.Errorf("%s: ru and en render the same text", kind)
		}
	}
	if _, err := mail.Render(mail.KindPasswordReset, "de", d); !errors.Is(err, mail.ErrInvalidMessage) {
		t.Errorf("Render in an unknown language: err = %v, want ErrInvalidMessage", err)
	}
}
