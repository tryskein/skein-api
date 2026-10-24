package notifications

// BodyPreviewMaxRunes is the hard ceiling for monitor-poll bodyPreview
// (sk-242.1 / INV-2). Preview is UTF-8 / rune-safe: we count and cut by
// Unicode code points, never by raw bytes, so multi-byte Cyrillic and
// emoji are never split mid-character.
const BodyPreviewMaxRunes = 1024

// truncateRunes returns the first max runes of s (or s if shorter).
// Convenience wrapper around truncateRunesFlag; see that for details.
func truncateRunes(s string, max int) string {
	preview, _ := truncateRunesFlag(s, max)
	return preview
}

// truncateRunesFlag returns a rune-safe prefix of s of at most max runes
// and whether truncation occurred. Single pass over s — no separate
// RuneCountInString (hot path in PollForMonitor). max < 0 → empty +
// truncated=true when s is non-empty. Invalid UTF-8 follows range-over-
// string semantics (bad bytes → U+FFFD steps).
//
// Used by PollForMonitor when projecting live comments.body into
// NotificationDTO.BodyPreview / Truncated.
func truncateRunesFlag(s string, max int) (preview string, truncated bool) {
	if max < 0 {
		max = 0
	}
	if max == 0 {
		return "", s != ""
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i], true
		}
		n++
	}
	return s, false
}
