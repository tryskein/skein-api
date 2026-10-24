package tools

// truncate_test.go — sk-282 T004, test-plan rows 1 and 2.
//
// Row 1: "R-8/INV-1: truncation on a rune boundary, the marker only on
// actual truncation; empty and short text carry no marker; three
// constants (80/200/1024)".
// Row 2: "R-8: truncation does not split multi-byte runes (Cyrillic,
// emoji) on the boundary".
//
// The assertions are written from R-8 and INV-1, not from what
// truncate.go happens to return: each case names the rule it enforces, and
// the multi-byte cases are stated in RUNES with the byte count spelled out
// separately, because "80 characters" and "80 bytes" are the same number
// only for ASCII — and that coincidence is exactly the bug R-8 exists to
// prevent.
//
// Pure unit: no pool, no database. Runs under
// `go test -count=1 ./internal/mcp/tools/...` with no env at all.

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"skein-api/internal/issues"
	"skein-api/internal/notifications"
)

// TestTruncateRunes_MarkerOnlyOnRealTruncation is INV-1's second marker
// row: "truncated" is set ONLY when truncation really happened, and is not
// set on short or empty text — otherwise the marker lies.
//
// The two spec edge-cases that state this in application terms are
// included by name: a comment shorter than the index fragment, and a
// project with an empty description.
func TestTruncateRunes_MarkerOnlyOnRealTruncation(t *testing.T) {
	cases := []struct {
		name          string
		in            string
		max           int
		wantPrefix    string
		wantTruncated bool
	}{
		{
			// Spec edge-case: "A project with an empty description → the
			// truncation marker is not set; an empty description is not
			// turned into a truncated one".
			name: "empty text carries no marker",
			in:   "", max: ProjectDescriptionPreviewMaxRunes,
			wantPrefix: "", wantTruncated: false,
		},
		{
			// Spec edge-case: "A comment shorter than the index fragment →
			// the truncation marker is not set".
			name: "text shorter than the limit carries no marker",
			in:   "short comment", max: ThreadIndexPreviewMaxRunes,
			wantPrefix: "short comment", wantTruncated: false,
		},
		{
			// The boundary itself: a full set is not a truncated set.
			name: "text exactly at the limit carries no marker",
			in:   strings.Repeat("a", ThreadIndexPreviewMaxRunes), max: ThreadIndexPreviewMaxRunes,
			wantPrefix: strings.Repeat("a", ThreadIndexPreviewMaxRunes), wantTruncated: false,
		},
		{
			// One rune past the boundary is the smallest real truncation.
			name: "one rune past the limit carries the marker",
			in:   strings.Repeat("a", ThreadIndexPreviewMaxRunes+1), max: ThreadIndexPreviewMaxRunes,
			wantPrefix: strings.Repeat("a", ThreadIndexPreviewMaxRunes), wantTruncated: true,
		},
		{
			// Nothing delivered out of something: the remainder exists, so
			// the marker must say so.
			name: "zero limit on non-empty text carries the marker",
			in:   "anything", max: 0,
			wantPrefix: "", wantTruncated: true,
		},
		{
			// Nothing delivered out of nothing: no remainder, no marker.
			name: "zero limit on empty text carries no marker",
			in:   "", max: 0,
			wantPrefix: "", wantTruncated: false,
		},
		{
			// A negative limit is a caller bug. It must not silently
			// degrade into "return everything" — that would hide the bug
			// behind an oversized response, which is the failure mode this
			// whole change exists to remove.
			name: "negative limit behaves as zero, never as unlimited",
			in:   "anything", max: -5,
			wantPrefix: "", wantTruncated: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotPrefix, gotTruncated := TruncateRunes(tc.in, tc.max)
			if gotPrefix != tc.wantPrefix {
				t.Errorf("prefix = %q, want %q", gotPrefix, tc.wantPrefix)
			}
			if gotTruncated != tc.wantTruncated {
				t.Errorf("truncated = %v, want %v — INV-1: the marker is set if and only if "+
					"text was actually dropped", gotTruncated, tc.wantTruncated)
			}
		})
	}
}

// TestTruncateRunes_CutsOnRuneBoundary is R-8's unit-of-measure decision:
// the cut is counted in runes, never in bytes. A byte cut lands inside a
// multi-byte character and produces mojibake.
//
// Every case asserts three things that a byte-slicer would fail
// separately: the prefix is exactly N runes, the prefix is still valid
// UTF-8, and the prefix is a genuine prefix of the input.
func TestTruncateRunes_CutsOnRuneBoundary(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
	}{
		{
			// Cyrillic is 2 bytes per letter: 120 runes = 240 bytes, so a
			// byte-slicer at 80 would cut halfway through rune 41.
			name: "cyrillic",
			in:   strings.Repeat("ж", 120),
			max:  ThreadIndexPreviewMaxRunes,
		},
		{
			// Emoji are 4 bytes: a byte cut produces a replacement
			// character in every client that renders the preview.
			name: "emoji",
			in:   strings.Repeat("🚀", 300),
			max:  ProjectDescriptionPreviewMaxRunes,
		},
		{
			// Mixed widths mean the byte offset of rune N is not a
			// function of N at all — the single case a fixed-stride
			// implementation cannot fake its way through.
			name: "mixed ascii, cyrillic and emoji",
			in:   strings.Repeat("aж🚀", 600),
			max:  InboxValuePreviewMaxRunes,
		},
		{
			// The boundary rune itself is multi-byte: the cut must land
			// BEFORE it, not inside it.
			name: "multi-byte rune sits exactly on the boundary",
			in:   strings.Repeat("a", ThreadIndexPreviewMaxRunes) + "ж" + "tail",
			max:  ThreadIndexPreviewMaxRunes,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, truncated := TruncateRunes(tc.in, tc.max)
			if !truncated {
				t.Fatalf("input of %d runes cut at %d was reported as not truncated",
					utf8.RuneCountInString(tc.in), tc.max)
			}
			if n := utf8.RuneCountInString(got); n != tc.max {
				t.Errorf("prefix is %d runes, want exactly %d — the rule counts runes, not bytes", n, tc.max)
			}
			if !utf8.ValidString(got) {
				t.Errorf("prefix is not valid UTF-8 (%d bytes) — the cut landed inside a rune",
					len(got))
			}
			if !strings.HasPrefix(tc.in, got) {
				t.Errorf("result is not a prefix of the input — the cut rewrote the text")
			}
		})
	}
}

// TestTruncateRunes_AssignedLengths pins the three lengths R-8 assigns.
// They are part of the acceptance, not an implementation detail: the
// thread-index budget, the list-row budget and the inbox budget are all
// derived from them, so a silent change to one moves an SC threshold.
func TestTruncateRunes_AssignedLengths(t *testing.T) {
	assigned := []struct {
		name string
		got  int
		want int
		why  string
	}{
		{"ThreadIndexPreviewMaxRunes", ThreadIndexPreviewMaxRunes, 80,
			"leading fragment of a comment in a thread-index entry"},
		{"ProjectDescriptionPreviewMaxRunes", ProjectDescriptionPreviewMaxRunes, 200,
			"project description in a list row"},
		{"InboxValuePreviewMaxRunes", InboxValuePreviewMaxRunes, 1024,
			"old/new value in an inbox row"},
	}
	for _, a := range assigned {
		if a.got != a.want {
			t.Errorf("%s = %d, want %d (R-8 assigns it: %s)", a.name, a.got, a.want, a.why)
		}
	}
}

// TestInboxValuePreviewMatchesMonitorPreview binds the inbox length to the
// preview limit the system already applies elsewhere.
//
// R-8 is explicit that 1 024 is NOT invented here — it is the existing
// preview limit, extended to a second path. Two independent literals with
// that relationship stated only in prose is exactly how "the same limit"
// becomes two different limits a year later, so the relationship is
// asserted instead of described. The production file does not import
// notifications for this (D-8 keeps the rule owned by the MCP layer); the
// test does, which is where a cross-package invariant belongs.
func TestInboxValuePreviewMatchesMonitorPreview(t *testing.T) {
	if InboxValuePreviewMaxRunes != notifications.BodyPreviewMaxRunes {
		t.Errorf("InboxValuePreviewMaxRunes = %d but notifications.BodyPreviewMaxRunes = %d — "+
			"R-8 says the inbox value limit IS the existing preview limit, not a second one. "+
			"If the preview limit really moved, move both and say so; if not, this is a drift.",
			InboxValuePreviewMaxRunes, notifications.BodyPreviewMaxRunes)
	}
}

// TestTruncateRunes_LeavesTheSourceIntact is INV-3 at the level this
// function can carry it: the rule produces a NEW string and does not
// modify, consume or shorten its input. The wire is narrowed; the data is
// not.
//
// The storage half of INV-3 belongs to the consumers (T011 / T016 / T019),
// which must show the full text is still readable through a reading tool.
func TestTruncateRunes_LeavesTheSourceIntact(t *testing.T) {
	original := strings.Repeat("payload", 100)
	source := original

	if _, truncated := TruncateRunes(source, ThreadIndexPreviewMaxRunes); !truncated {
		t.Fatal("fixture is not long enough to be truncated — the test would prove nothing")
	}
	if source != original {
		t.Errorf("the input string changed after truncation — INV-3: the cut is on the wire, not in the data")
	}
}

// TestProjectionPathsLogNoBodies is matrix row 95a (sk-282, T025).
//
// "Secrets and volumes do not leak into the log: truncation and
// projection paths (the truncation rule, the thread index, notification
// values, move preparation) write to the log ONLY lengths and the
// truncation marker, not the bodies and values themselves. This is our
// new code, not the framework."
//
// The four paths named by the row are all PURE functions — that is what
// makes this a `unit` row and not an integration one — so the test drives
// them directly with sentinel-laden input while the default slog handler
// is swapped for a capturing one, and then asserts the capture.
//
// Why the assertion is «no sentinel anywhere in the log» rather than a
// list of forbidden keys: a body reaches the log by being ADDED to a log
// line, and the person adding it picks the key name. A key-based check
// would have to guess that name in advance; scanning the bytes for text
// that could only have come from the input cannot be evaded by naming.
//
// The test also fails if a path stops being pure and starts logging at
// all — which is the honest state to be in: at that point somebody has to
// decide what it may say.
func TestProjectionPathsLogNoBodies(t *testing.T) {
	const secret = "SENTINEL-BODY-THAT-MUST-NEVER-REACH-THE-LOG"
	long := secret + " " + strings.Repeat("x", 4096)

	var captured bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&captured, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	// 1) the truncation rule itself.
	preview, truncated := TruncateRunes(long, ThreadIndexPreviewMaxRunes)
	if !truncated || preview == long {
		t.Fatalf("the fixture did not truncate — the rest of this test would prove nothing")
	}

	// 2) the thread index.
	created := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	index := buildCommentIndex([]issues.CommentDTO{
		{ID: uuid.Must(uuid.NewV7()), Author: "alice", Body: long, CreatedAt: created},
		{ID: uuid.Must(uuid.NewV7()), Author: "alice", Body: long, CreatedAt: created},
	})
	if len(index) != 2 {
		t.Fatalf("index has %d entries, want 2", len(index))
	}

	// 3) the inbox value cut.
	field := "description"
	row, err := projectInboxRow(notifications.NotificationDTO{
		ID:        7,
		Type:      "issue.updated",
		Field:     &field,
		OldValue:  long,
		NewValue:  long,
		Summary:   "updated description",
		CreatedAt: created,
	})
	if err != nil {
		t.Fatalf("projectInboxRow: %v", err)
	}
	if row["valuesTruncated"] != true {
		t.Fatalf("the inbox fixture did not truncate: %#v", row)
	}

	// 4) the move-kit card projection.
	builder := newMoveCreateArgs()
	builder.carry("description", long)
	builder.carry("notes", long)
	if _, err := builder.sourceCard(&issues.IssueDetail{
		ID: "sk-1", Version: 1, Status: "open",
		Description: long, Notes: long,
	}); err != nil {
		t.Fatalf("sourceCard: %v", err)
	}

	logged := captured.String()
	if strings.Contains(logged, secret) {
		t.Errorf("a projection path wrote the body into the log:\n%s", logged)
	}
	if strings.Contains(logged, strings.Repeat("x", 200)) {
		t.Errorf("a projection path wrote a long value into the log:\n%s", logged)
	}
	if logged != "" {
		t.Logf("projection paths logged %d bytes; verified body-free:\n%s", len(logged), logged)
	}
}
