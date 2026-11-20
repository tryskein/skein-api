package push

// send_redaction_forms_test.go — INV-9 on the text cleaning itself
// (RedactSubscription), case by case, with the exact text expected. The
// reference is the device-consent specification: the endpoint and the keys
// of a channel reach no log record and no error text, neither whole nor as a
// part carrying the channel token; the provider's host and a fingerprint of
// the endpoint are allowed. The cleaning also keeps the rest of the text
// readable: a short part of an endpoint is removed only where it stands as a
// whole token, and nothing is inserted where there was no secret.

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// A part of the endpoint written in the URL-encoded form of the path or of
// the query, or decoded from it, is removed in whichever spelling the text
// echoes; an empty query value does not stop the values after it from being
// removed and inserts no marker of its own.
func TestRedactSubscription_EveryEncodedSpellingOfAPartIsRemoved(t *testing.T) {
	sub := testSubscription(
		"https://push.example/send/tok%41en%2fpart?sig=a+b%2Fsignature&x=&k=querytokenvalue")
	cases := []struct{ name, text, want string }{
		{"path segment as the endpoint writes it", `echo "tok%41en%2fpart" end`, `echo "<redacted>" end`},
		{"path segment decoded", `echo "tokAen/part" end`, `echo "<redacted>" end`},
		{"query value decoded by query rules", `echo "a b/signature" end`, `echo "<redacted>" end`},
		{"query value decoded by path rules", `echo "a+b/signature" end`, `echo "<redacted>" end`},
		{"query value after an empty value", `echo "querytokenvalue" end`, `echo "<redacted>" end`},
		{"text without a secret", `provider overloaded, retry later`, `provider overloaded, retry later`},
	}
	for _, c := range cases {
		if got := RedactSubscription(c.text, sub); got != c.want {
			t.Errorf("%s: RedactSubscription(%q) = %q, want %q", c.name, c.text, got, c.want)
		}
	}
}

// A part of the endpoint of fewer than 8 bytes is removed only where it
// stands as a whole token; one of 8 bytes or more is removed wherever it
// occurs. Text around the tokens is kept byte for byte.
func TestRedactSubscription_ShortPartsAreRemovedOnlyAsWholeTokens(t *testing.T) {
	// "sevenAA" — 7 bytes, "eightBBB" — 8 bytes.
	sub := testSubscription("https://push.example/sevenAA/eightBBB")
	cases := []struct{ name, text, want string }{
		{"8 bytes inside a word, 7 bytes inside a word", "xeightBBBx xsevenAAx", "x<redacted>x xsevenAAx"},
		{"between path separators", "path /sevenAA/ end", "path /<redacted>/ end"},
		{"at the start of the text", "sevenAA failed", "<redacted> failed"},
		{"at the end of the text", "device sevenAA", "device <redacted>"},
		{"bounded only on the left", "sevenAAx", "sevenAAx"},
		{"bounded only on the right", "xsevenAA", "xsevenAA"},
		{"in a word, then a bounded run of the same length, then as a token",
			"xsevenAA 1234567 sevenAA", "xsevenAA 1234567 <redacted>"},
		{"after each other punctuation that ends a word", `a=sevenAA&b:sevenAA,(sevenAA);"sevenAA"`,
			`a=<redacted>&b:<redacted>,(<redacted>);"<redacted>"`},
	}
	for _, c := range cases {
		if got := RedactSubscription(c.text, sub); got != c.want {
			t.Errorf("%s: RedactSubscription(%q) = %q, want %q", c.name, c.text, got, c.want)
		}
	}
}

// A secret that contains another one is removed whole: the longer form goes
// first, so no tail of it is left beside a marker.
func TestRedactSubscription_ALongerSecretIsRemovedBeforeOneItContains(t *testing.T) {
	sub := testSubscription("https://push.example/tokenvalue?sig=tokenvalue-signature-part")
	text := `answer: "sig=tokenvalue-signature-part"`
	want := `answer: "sig=<redacted>"`
	if got := RedactSubscription(text, sub); got != want {
		t.Fatalf("RedactSubscription(%q) = %q, want %q", text, got, want)
	}
}

// The whole endpoint becomes its reference: the provider's host and the
// first 6 bytes of the SHA-256 of the endpoint, in hex. An endpoint without a
// host — or one that does not parse — names no host.
func TestRedactSubscription_TheEndpointBecomesHostAndFingerprint(t *testing.T) {
	fingerprint := func(endpoint string) string {
		sum := sha256.Sum256([]byte(endpoint))
		return hex.EncodeToString(sum[:6])
	}
	cases := []struct{ name, endpoint, host string }{
		{"https endpoint", "https://push.example/abcdefghijklmnop", "push.example"},
		{"no host", "mailto:abcdefghijklmnop", "invalid-endpoint"},
		{"does not parse", "https://push.example/%zzabcdefghijklmnop", "invalid-endpoint"},
	}
	for _, c := range cases {
		sub := testSubscription(c.endpoint)
		text := `Post "` + c.endpoint + `": refused`
		want := `Post "` + c.host + "#" + fingerprint(c.endpoint) + `": refused`
		if got := RedactSubscription(text, sub); got != want {
			t.Errorf("%s: RedactSubscription(%q) = %q, want %q", c.name, text, got, want)
		}
	}
}
