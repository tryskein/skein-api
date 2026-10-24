package outbox

// digest_text_test.go — sk-303.14, task T016.
//
// The unit half of the class: coverage-matrix row 105 (digest copy of BOTH
// kinds — a noun phrase, no past-tense verbs that carry grammatical gender)
// and half of row 112 (the list limit inside the spec's range). No database
// is needed here: text assembly is a pure function of the list that survived
// the re-check.

import (
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"skein-api/internal/notifications"
	"skein-api/internal/push"
	"skein-api/internal/review"
)

// digestCategoriesUnderTest — both categories and each one's reason.
//
// The list is derived from review.Categories(), not typed by hand: otherwise
// a third category, arriving, would silently go without a copy check — and
// hand-written lists in this wave have already been silent on a miss three
// times.
func digestCategoriesUnderTest(t *testing.T) map[review.Category]string {
	t.Helper()
	out := map[review.Category]string{}
	for _, cat := range review.Categories() {
		reason, ok := notifications.DigestReasonForCategory(cat)
		if !ok {
			t.Fatalf("category %q does not map to a reason — there is nothing to build a digest from", cat)
		}
		out[cat] = string(reason)
	}
	return out
}

// cyrillicWord cuts whole Cyrillic words out of the text.
//
// Tokenisation is needed because `\b` in Go (RE2) is an ASCII WORD BOUNDARY:
// a Cyrillic letter is not a letter to it, so `…л(?:а|о|и)?\b` on the word
// «пришла» does not fire at all. A stretch written with `\b` would look
// working and stay silent on every real verb — which is exactly what a
// mutation check found: substituting «Вам пришла новая порция» only tripped
// the named list below, and the stretch stayed silent.
//
// Kept after the copy moved to English: a regression that puts a Russian
// past-tense verb back into a notification would misgender half the team,
// and this scanner is what would catch it.
var cyrillicWord = regexp.MustCompile(`[а-яёА-ЯЁ]+`)

// genderedPastTense is a stretch over Russian past-tense verb forms: the
// word AS A WHOLE is a stem plus «-л», «-ла», «-ло», «-ли».
//
// This is an ALARM STRETCH, not a language parser, and it is honest about
// that. False positives exist by construction — «начало», «дело», «угол» —
// but the notification dictionary is closed and tiny, none of those are in
// it, and the appearance of such a word is as much a reason to re-read the
// copy as a real verb. What it catches is exactly why it exists: an edit
// that replaces "Issues awaiting acceptance" with «Вам пришла новая порция»
// and thereby misgenders half the team. Russian past tense carries
// grammatical gender; users carry none.
//
// It has FALSE NEGATIVES too, and those must be known no less: the pattern
// requires a stem ending in «-л», so masculine forms with no «л» at all it
// does not see — «мог», «смог», «принёс», «истёк»; and it requires at least
// two letters before «л», so «шла» slips through. A corpus run on 46 real
// forms catches 40. That is a known cost, not an oversight: the feminine,
// neuter and plural pairs of the same verbs («могла», «принесла»,
// «истекла») are caught, so a gender disagreement such an edit would still
// expose on the second form — and loosening to one letter before «л» would
// pull in «дела», «тела», «села», of which the notification dictionary has
// plenty.
var genderedPastTense = regexp.MustCompile(`^[а-яё]{2,}л[аои]?(?:сь|ся)?$`)

// findGenderedPastTense returns the first word of the text that looks like a
// past-tense verb, or an empty string.
func findGenderedPastTense(text string) string {
	for _, w := range cyrillicWord.FindAllString(text, -1) {
		if genderedPastTense.MatchString(strings.ToLower(w)) {
			return w
		}
	}
	return ""
}

// knownGenderedForms — forms that have asked to enter this copy and must be
// rejected by name. The stretch above catches them too; the list exists so
// a failure is readable and so an edit of the stretch cannot make the
// assertion vacuous.
var knownGenderedForms = []string{
	"отметил", "отметила", "просмотрел", "просмотрела",
	"принял", "приняла", "закрыл", "закрыла", "оставил", "оставила",
	"пришло", "пришла", "накопились", "появились",
}

// renderBoth assembles every text a digest of each category can show a
// person: the title and the body in three shapes — one issue, several with
// a full list, several with a remainder.
func renderBoth(t *testing.T, cat review.Category, reason string) []string {
	t.Helper()
	shapes := [][]digestElement{
		{{id: "sk-1", title: "Work result"}},
		{{id: "sk-1"}, {id: "sk-2"}, {id: "sk-3"}},
		{{id: "sk-1"}, {id: "sk-2"}, {id: "sk-3"}, {id: "sk-4"}, {id: "sk-5"},
			{id: "sk-6"}, {id: "sk-7"}, {id: "sk-8"}},
	}
	var out []string
	for _, els := range shapes {
		raw, err := marshalDigestPayload(reason, cat, els, "https://app.example.com")
		if err != nil {
			t.Fatalf("build digest (%s, %d elements): %v", cat, len(els), err)
		}
		var p pushPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("payload does not unmarshal: %v", err)
		}
		out = append(out, p.Title, p.Body)
	}
	return out
}

// TestDigestText_NoGenderedPastTense — row 105.
//
// The copy obeys the notification contract: a noun phrase, no past-tense
// verbs that carry grammatical gender. The same ban, for the same reason,
// already sits on the status labels in push.go — "Closed", not "closed it".
func TestDigestText_NoGenderedPastTense(t *testing.T) {
	for cat, reason := range digestCategoriesUnderTest(t) {
		for _, text := range renderBoth(t, cat, reason) {
			if hit := findGenderedPastTense(text); hit != "" {
				t.Errorf("category %s: text %q contains the past-tense form %q — "+
					"it carries grammatical gender and misgenders half the team",
					cat, text, hit)
			}
			low := strings.ToLower(text)
			for _, bad := range knownGenderedForms {
				if strings.Contains(low, bad) {
					t.Errorf("category %s: text %q contains %q", cat, text, bad)
				}
			}
		}
	}
}

// TestDigestText_BothCategoriesAreNamedAndDistinct — the second half of row
// 105: copy exists for BOTH kinds and each has its own.
//
// Empty or shared copy would pass the check above while telling the person
// nothing — so the ban is checked together with presence, not instead of it.
func TestDigestText_BothCategoriesAreNamedAndDistinct(t *testing.T) {
	cats := digestCategoriesUnderTest(t)
	bodies := map[review.Category]string{}
	for cat, reason := range cats {
		els := []digestElement{{id: "sk-1"}, {id: "sk-2"}}
		raw, err := marshalDigestPayload(reason, cat, els, "")
		if err != nil {
			t.Fatalf("build digest (%s): %v", cat, err)
		}
		var p pushPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("payload does not unmarshal: %v", err)
		}
		if strings.TrimSpace(p.Body) == "" {
			t.Errorf("category %s: body is empty", cat)
		}
		if strings.TrimSpace(p.Title) == "" || p.Title == pushHeadUnknownReason {
			t.Errorf("category %s: title = %q — the reason is unnamed, the device would only say the app name",
				cat, p.Title)
		}
		if !strings.Contains(p.Body, "2") {
			t.Errorf("category %s: body %q does not name the count", cat, p.Body)
		}
		bodies[cat] = p.Body
	}
	seen := map[string]review.Category{}
	for cat, body := range bodies {
		if other, dup := seen[body]; dup {
			t.Errorf("categories %s and %s yield the same text %q — they have merged", cat, other, body)
		}
		seen[body] = cat
	}
}

// TestDigestListLimit_WithinTheSpecifiedRange — row 112.
//
// The spec names a range, not a number: "no fewer than three and no more
// than ten identifiers, the remainder as a number". What is checked is the
// range itself — the concrete value inside it is a mechanism question, and
// an assertion "equals five" would forbid changing it while protecting
// nothing.
func TestDigestListLimit_WithinTheSpecifiedRange(t *testing.T) {
	const specMin, specMax = 3, 10
	if digestListLimit < specMin || digestListLimit > specMax {
		t.Errorf("list limit = %d, the spec requires %d..%d", digestListLimit, specMin, specMax)
	}
}

// TestDigestBudget_DerivedFromTheTransportCeiling — the budget's guard.
//
// The budget must stay BELOW the transport ceiling and stay meaningful: if
// it drops below the length of a list of short identifiers, the digest
// will silently degenerate into a number alone.
func TestDigestBudget_DerivedFromTheTransportCeiling(t *testing.T) {
	if digestPayloadBudgetBytes >= push.MaxPayloadBytes {
		t.Errorf("budget %d is not below the record ceiling %d — there is no slack for encryption",
			digestPayloadBudgetBytes, push.MaxPayloadBytes)
	}
	els := make([]digestElement, 0, digestListLimit+2)
	for i := range digestListLimit + 2 {
		els = append(els, digestElement{id: "sk-" + strings.Repeat("9", i+1)})
	}
	raw, err := marshalDigestPayload(
		string(notifications.ReasonKeyUnacceptedReminder), review.CategoryUnaccepted, els, "")
	if err != nil {
		t.Fatalf("build digest: %v", err)
	}
	var p pushPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("payload does not unmarshal: %v", err)
	}
	if got := strings.Count(p.Body, "sk-"); got != digestListLimit {
		t.Errorf("listed %d short identifiers, want %d — the budget is cutting what it must not cut",
			got, digestListLimit)
	}
	if !strings.Contains(p.Body, digestRemainderSep+"2 more") {
		t.Errorf("body %q does not carry the remainder as a number", p.Body)
	}
}

// ---------------------------------------------------------------------------
// TITLE fitting (second half of row 67) and the last budget frontier
// ---------------------------------------------------------------------------
//
// The first half of row 67 — "a full list of maximal identifiers" — is
// closed by the budget test above. The second — "one element with a huge
// title" — was closed by nothing: `issues.title` in the schema is `text`,
// WITH NO upper bound, and a one-element digest body is "identifier ·
// title". The path is reachable, and review A/7 killed it with three
// mutations in a row: a BYTE slice instead of runes, fitting switched off
// entirely, and, as a result, a payload of any size handed to the sender.

// fitReason — the reason under which this section's payloads are built.
const fitReason = string(notifications.ReasonKeyUnacceptedReminder)

// fitBase — a tap-URL base of ordinary length: what is measured is title
// fitting, not the URL's contribution.
const fitBase = "https://app.example.com"

// hugeTitles — titles on which fitting is checked.
//
// There are several on purpose. Cyrillic is two bytes, so a BYTE slice
// splits a character in half only at an odd offset — on one title such a
// mutation can survive by chance. The set shifts the boundary with
// single-byte padding and adds four-byte runes, where missing the
// boundary is impossible.
func hugeTitles() map[string]string {
	out := map[string]string{
		"cyrillic, 4 KB":          strings.Repeat("я", 2048),
		"cyrillic with a shift":   "a" + strings.Repeat("щ", 2048),
		"cyrillic with a shift×3": "abc" + strings.Repeat("ц", 2048),
		"four-byte runes":         strings.Repeat("🙂", 1024),
		"mix":                     strings.Repeat("я🙂aю", 512),
	}
	return out
}

// fittedTitle returns what fitting left of the title, without the trailing
// ellipsis, and whether it fired at all.
func fittedTitle(t *testing.T, issueID, title string) (kept string, truncated bool) {
	t.Helper()
	head := pushHead(fitReason, eventIssue{})
	got := fitDigestTitle(fitReason, head, issueID, title, fitBase)
	if got == title {
		return got, false
	}
	if !strings.HasSuffix(got, digestEllipsis) {
		t.Fatalf("fitted title %q does not end with an ellipsis — the cut is unmarked",
			lastRunes(got, 12))
	}
	return strings.TrimSuffix(got, digestEllipsis), true
}

// lastRunes — the tail of a string for a readable failure: printing
// kilobytes into the test output is pointless.
func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n:])
}

// TestDigestTitle_HugeTitleIsFittedOnRuneBoundaries — the fitting itself:
// it FIRES, cuts on runes and does not split a character in half.
//
// Three assertions, because there were three mutations and they are
// independent: fitting switched off hands the title over whole (caught by
// size), a byte slice leaves half a rune (caught by utf8.ValidString and
// the rune-prefix check), and a cut without an ellipsis does not tell the
// person the text was truncated.
func TestDigestTitle_HugeTitleIsFittedOnRuneBoundaries(t *testing.T) {
	for name, title := range hugeTitles() {
		t.Run(name, func(t *testing.T) {
			const issueID = "sk-1"
			kept, truncated := fittedTitle(t, issueID, title)
			if !truncated {
				t.Fatalf("a title of %d bytes went out whole — fitting did not fire, "+
					"and the payload will hit the record ceiling and be recorded as a transport failure",
					len(title))
			}
			if !utf8.ValidString(kept) {
				t.Errorf("the kept part of the title is not valid UTF-8: the cut went by BYTES "+
					"and split a character in half (tail %q)", lastRunes(kept, 12))
			}
			// Rune prefix: what remains must match the start of the
			// original title rune for rune. A byte slice violates this
			// every time the boundary falls inside a character.
			runes := []rune(title)
			n := utf8.RuneCountInString(kept)
			if n > len(runes) || kept != string(runes[:n]) {
				t.Errorf("the kept part is not a rune prefix of the title — "+
					"the text is distorted, not shortened (tail %q)", lastRunes(kept, 12))
			}

			// And what goes to the device fits the budget and stays
			// readable: json.Marshal replaces a broken rune with U+FFFD,
			// so a validity check after unmarshal would be blind without
			// this.
			els := []digestElement{{id: issueID, title: title}}
			raw, err := marshalDigestPayload(fitReason, review.CategoryUnaccepted, els, fitBase)
			if err != nil {
				t.Fatalf("build digest: %v", err)
			}
			if len(raw) > digestPayloadBudgetBytes {
				t.Errorf("payload %d bytes against a budget of %d — will go to the sender and be "+
					"rejected before the network, recorded as a transport failure",
					len(raw), digestPayloadBudgetBytes)
			}
			var p pushPayload
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Fatalf("payload does not unmarshal: %v", err)
			}
			if strings.ContainsRune(p.Body, utf8.RuneError) {
				t.Errorf("the body contains U+FFFD — a character was split in half before serialisation")
			}
		})
	}
}

// TestDigestTitle_FitUsesTheBudgetItHas — fitting must leave the MAXIMUM
// that fits, not the first that did.
//
// Successive halving (1/2, 1/4, 1/8 … and the first that fits) gives a
// result that is correct in meaning and still leaves about a fifth of the
// budget unused: the person sees half as much of the title as would fit.
// The assertion "one more rune does not fit" is the only thing that tells
// binary search from successive halving, and the loss cannot be seen by
// eye.
func TestDigestTitle_FitUsesTheBudgetItHas(t *testing.T) {
	for name, title := range hugeTitles() {
		t.Run(name, func(t *testing.T) {
			const issueID = "sk-1"
			kept, truncated := fittedTitle(t, issueID, title)
			if !truncated {
				t.Fatalf("fitting did not fire — nothing to measure")
			}
			runes := []rune(title)
			n := utf8.RuneCountInString(kept)
			if n >= len(runes) {
				t.Fatalf("kept %d runes of %d — that is not a shortening", n, len(runes))
			}
			head := pushHead(fitReason, eventIssue{})
			longer := string(runes[:n+1]) + digestEllipsis
			raw, err := json.Marshal(singleElementPayload(fitReason, head, issueID, longer, fitBase))
			if err != nil {
				t.Fatalf("build probe: %v", err)
			}
			if len(raw) <= digestPayloadBudgetBytes {
				t.Errorf("one more rune of the title fitted (%d bytes against a budget of %d) but is not shown — "+
					"fitting hands over the first that fitted, not the maximum",
					len(raw), digestPayloadBudgetBytes)
			}
		})
	}
}

// TestDigestPayload_BudgetBeatsTheListDownToTheCountAlone — the fork the
// spec did not decide, recorded as behaviour.
//
// "A list of no fewer than three identifiers" and "fit a web-push record"
// conflict at maximum-length identifiers. The BUDGET wins: the list is
// shortened all the way to disappearing, and the count-only form remains.
// The three-identifier requirement is about the typical case, not the
// degenerate one.
func TestDigestPayload_BudgetBeatsTheListDownToTheCountAlone(t *testing.T) {
	// Identifiers of the system's maximum length (varchar(1000)) plus a
	// long URL base — degenerate but legal data.
	els := make([]digestElement, 0, 7)
	for i := range 7 {
		els = append(els, digestElement{id: "sk-" + strconv.Itoa(i) + strings.Repeat("9", 990)})
	}
	base := "https://" + strings.Repeat("a", 2500) + ".example.com"

	raw, err := marshalDigestPayload(fitReason, review.CategoryUnaccepted, els, base)
	if err != nil {
		t.Fatalf("build digest: %v", err)
	}
	if len(raw) > digestPayloadBudgetBytes {
		t.Fatalf("payload %d bytes against a budget of %d — the budget did not win",
			len(raw), digestPayloadBudgetBytes)
	}
	var p pushPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("payload does not unmarshal: %v", err)
	}
	if strings.Contains(p.Body, els[0].id) {
		t.Errorf("the body still carries an identifier — the list was not removed")
	}
	if got := bodyCountUnit(t, p.Body); got != len(els) {
		t.Errorf("declared %d issues, want %d — the count must survive the list disappearing",
			got, len(els))
	}
	// There must be no remainder tail at this step: there is NO remainder,
	// the count already named everything. "Issues awaiting acceptance: 7
	// and 7 more" would be a lie.
	if strings.Contains(p.Body, digestRemainderSep) {
		t.Errorf("body %q promises a remainder with no list", p.Body)
	}
}

// bodyCountUnit reads the declared count from a body "<subject>: <N>".
func bodyCountUnit(t *testing.T, body string) int {
	t.Helper()
	colon := strings.Index(body, ": ")
	if colon < 0 {
		t.Fatalf("body %q does not name a count", body)
	}
	rest := body[colon+2:]
	if sp := strings.IndexByte(rest, ' '); sp >= 0 {
		rest = rest[:sp]
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		t.Fatalf("body %q does not start the count with a number", body)
	}
	return n
}

// TestDigestPayload_OversizeIsLoudNotSilent — the second half of the same
// fork.
//
// If even the count-only form does not fit — that is a DEFECT, not a mode
// of operation, and it must be loud. Silently handing an unfit payload to
// the sender produces a "transport failure": webpush-go will reject it
// BEFORE touching the network, and incident triage will go to a provider
// nobody bothered. A false attribution is worse than both a short list and
// an unsent notification.
func TestDigestPayload_OversizeIsLoudNotSilent(t *testing.T) {
	els := []digestElement{{id: "sk-1"}, {id: "sk-2"}, {id: "sk-3"}}
	base := "https://" + strings.Repeat("a", digestPayloadBudgetBytes) + ".example.com"

	raw, err := marshalDigestPayload(fitReason, review.CategoryUnaccepted, els, base)
	if err == nil {
		t.Fatalf("a payload of %d bytes against a budget of %d was handed over in silence — the transport will record it "+
			"as a transport failure and send triage to the provider",
			len(raw), digestPayloadBudgetBytes)
	}
	if !errors.Is(err, errDigestOversize) {
		t.Fatalf("error %v is not recognised as a budget overflow — the tract will not tell it "+
			"from a transient one and will go into retries", err)
	}
	if raw != nil {
		t.Errorf("together with the error, %d bytes were handed over — something that cannot fit must not "+
			"reach the sender under any conditions", len(raw))
	}
}
