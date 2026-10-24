package notifications

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTruncateRunes pins INV-2 (sk-242.1 T001): bodyPreview ceiling is
// rune-safe (UTF-8), never byte-sliced. Covers ASCII, Cyrillic, emoji, and
// the exact BodyPreviewMaxRunes boundary.
func TestTruncateRunes(t *testing.T) {
	t.Run("short ASCII unchanged", func(t *testing.T) {
		in := "hello world"
		got := truncateRunes(in, BodyPreviewMaxRunes)
		if got != in {
			t.Fatalf("short ASCII: got %q, want %q", got, in)
		}
	})

	t.Run("exact max runes unchanged", func(t *testing.T) {
		in := strings.Repeat("a", BodyPreviewMaxRunes)
		got := truncateRunes(in, BodyPreviewMaxRunes)
		if got != in {
			t.Fatalf("exact max: len=%d want=%d", utf8.RuneCountInString(got), BodyPreviewMaxRunes)
		}
		if utf8.RuneCountInString(got) != BodyPreviewMaxRunes {
			t.Fatalf("exact max rune count = %d, want %d", utf8.RuneCountInString(got), BodyPreviewMaxRunes)
		}
	})

	t.Run("ASCII over max", func(t *testing.T) {
		in := strings.Repeat("x", BodyPreviewMaxRunes+50)
		got := truncateRunes(in, BodyPreviewMaxRunes)
		if utf8.RuneCountInString(got) != BodyPreviewMaxRunes {
			t.Fatalf("ASCII over: rune count = %d, want %d", utf8.RuneCountInString(got), BodyPreviewMaxRunes)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("ASCII over: result not valid UTF-8")
		}
		if got != strings.Repeat("x", BodyPreviewMaxRunes) {
			t.Fatalf("ASCII over: unexpected content prefix")
		}
	})

	t.Run("Cyrillic multi-byte no mid-rune cut", func(t *testing.T) {
		// Each Cyrillic letter is 2 bytes in UTF-8; a byte-slicer at 1024
		// would produce invalid UTF-8 or split a character.
		in := strings.Repeat("я", BodyPreviewMaxRunes+10)
		got := truncateRunes(in, BodyPreviewMaxRunes)
		if !utf8.ValidString(got) {
			t.Fatalf("Cyrillic: invalid UTF-8 after truncate")
		}
		if utf8.RuneCountInString(got) != BodyPreviewMaxRunes {
			t.Fatalf("Cyrillic: rune count = %d, want %d", utf8.RuneCountInString(got), BodyPreviewMaxRunes)
		}
		if got != strings.Repeat("я", BodyPreviewMaxRunes) {
			t.Fatalf("Cyrillic: content mismatch")
		}
		// Byte length must be 2× runes (each "я" is 2 bytes).
		if len(got) != BodyPreviewMaxRunes*2 {
			t.Fatalf("Cyrillic: byte len = %d, want %d", len(got), BodyPreviewMaxRunes*2)
		}
	})

	t.Run("emoji multi-code-unit no mid-rune cut", func(t *testing.T) {
		// "😀" is one Unicode code point (U+1F600) = 4 UTF-8 bytes.
		// We count code points (runes), not grapheme clusters — one emoji
		// emoji is one rune here.
		in := strings.Repeat("😀", BodyPreviewMaxRunes+3)
		got := truncateRunes(in, BodyPreviewMaxRunes)
		if !utf8.ValidString(got) {
			t.Fatalf("emoji: invalid UTF-8 after truncate")
		}
		if utf8.RuneCountInString(got) != BodyPreviewMaxRunes {
			t.Fatalf("emoji: rune count = %d, want %d", utf8.RuneCountInString(got), BodyPreviewMaxRunes)
		}
		if got != strings.Repeat("😀", BodyPreviewMaxRunes) {
			t.Fatalf("emoji: content mismatch")
		}
	})

	t.Run("mixed Cyrillic+emoji boundary", func(t *testing.T) {
		// 500 "ж" + 600 "🚀" = 1100 runes → cut at 1024 → 500 Cyrillic + 524 emoji.
		prefix := strings.Repeat("ж", 500)
		suffix := strings.Repeat("🚀", 600)
		in := prefix + suffix
		got := truncateRunes(in, BodyPreviewMaxRunes)
		if !utf8.ValidString(got) {
			t.Fatalf("mixed: invalid UTF-8")
		}
		if utf8.RuneCountInString(got) != BodyPreviewMaxRunes {
			t.Fatalf("mixed: rune count = %d, want %d", utf8.RuneCountInString(got), BodyPreviewMaxRunes)
		}
		want := prefix + strings.Repeat("🚀", BodyPreviewMaxRunes-500)
		if got != want {
			t.Fatalf("mixed: content mismatch (len got=%d want=%d)", len(got), len(want))
		}
	})

	t.Run("empty input", func(t *testing.T) {
		if got := truncateRunes("", BodyPreviewMaxRunes); got != "" {
			t.Fatalf("empty: got %q", got)
		}
	})

	t.Run("max zero", func(t *testing.T) {
		if got := truncateRunes("abc", 0); got != "" {
			t.Fatalf("max=0: got %q", got)
		}
	})

	t.Run("max negative treated as zero", func(t *testing.T) {
		if got := truncateRunes("abc", -1); got != "" {
			t.Fatalf("max=-1: got %q", got)
		}
	})

	t.Run("BodyPreviewMaxRunes is 1024", func(t *testing.T) {
		if BodyPreviewMaxRunes != 1024 {
			t.Fatalf("BodyPreviewMaxRunes = %d, want 1024 (INV-2)", BodyPreviewMaxRunes)
		}
	})
}
