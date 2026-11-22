package outbox

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// fixedProjectID is reused across cases so the resulting subject strings
// are easy to eyeball in test output. The exact uuid value doesn't matter
// for the assertions (we substring-match).
var fixedProjectID = uuid.MustParse("11111111-2222-3333-4444-555555555555")

func TestBuildSubject_IssueLifecycle(t *testing.T) {
	pid := fixedProjectID
	got := BuildSubject(SubjectInput{
		EventType:  "issue.created",
		EntityType: "issue",
		EntityID:   "demo-42",
		ProjectID:  &pid,
	})
	want := "skein.events.project." + pid.String() + ".issue.created.demo-42"
	if got != want {
		t.Errorf("issue.created subject mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildSubject_IssueClosed(t *testing.T) {
	pid := fixedProjectID
	got := BuildSubject(SubjectInput{
		EventType:  "issue.closed",
		EntityType: "issue",
		EntityID:   "demo-42.1",
		ProjectID:  &pid,
	})
	if !strings.HasSuffix(got, ".issue.closed.demo-42%2E1") {
		t.Errorf("expected suffix '.issue.closed.demo-42%%2E1', got %q", got)
	}
	if !strings.HasPrefix(got, "skein.events.project."+pid.String()) {
		t.Errorf("expected project-scoped prefix, got %q", got)
	}
}

func TestBuildSubject_IssueFieldChanged_Priority(t *testing.T) {
	pid := fixedProjectID
	got := BuildSubject(SubjectInput{
		EventType:  "issue.field_changed",
		EntityType: "issue",
		EntityID:   "demo-42",
		ProjectID:  &pid,
		Field:      "priority",
	})
	want := "skein.events.project." + pid.String() + ".issue.field.priority.demo-42"
	if got != want {
		t.Errorf("issue.field_changed/priority mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildSubject_IssueFieldChanged_AcceptanceCriteria(t *testing.T) {
	pid := fixedProjectID
	got := BuildSubject(SubjectInput{
		EventType:  "issue.field_changed",
		EntityType: "issue",
		EntityID:   "demo-42",
		ProjectID:  &pid,
		Field:      "acceptanceCriteria",
	})
	want := "skein.events.project." + pid.String() + ".issue.field.acceptance-criteria.demo-42"
	if got != want {
		t.Errorf("acceptanceCriteria kebab mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildSubject_UserCreated(t *testing.T) {
	uid := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	got := BuildSubject(SubjectInput{
		EventType:  "user.created",
		EntityType: "user",
		EntityID:   uid.String(),
		ProjectID:  nil,
	})
	want := "skein.user." + uid.String() + ".created"
	if got != want {
		t.Errorf("user.created subject mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildSubject_UserDeactivated(t *testing.T) {
	uid := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	got := BuildSubject(SubjectInput{
		EventType:  "user.deactivated",
		EntityType: "user",
		EntityID:   uid.String(),
	})
	want := "skein.user." + uid.String() + ".deactivated"
	if got != want {
		t.Errorf("user.deactivated subject mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildSubject_UserFieldChanged_Name(t *testing.T) {
	uid := uuid.MustParse("99999999-9999-9999-9999-999999999999")
	got := BuildSubject(SubjectInput{
		EventType:  "user.field_changed",
		EntityType: "user",
		EntityID:   uid.String(),
		Field:      "name",
	})
	want := "skein.user." + uid.String() + ".field.name"
	if got != want {
		t.Errorf("user.field_changed/name mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildSubject_CommentCreated(t *testing.T) {
	pid := fixedProjectID
	cid := "abcd1234-0000-0000-0000-aabbccddeeff"
	got := BuildSubject(SubjectInput{
		EventType:  "comment.created",
		EntityType: "comment",
		EntityID:   cid,
		ProjectID:  &pid,
	})
	want := "skein.events.project." + pid.String() + ".comment.created." + cid
	if got != want {
		t.Errorf("comment.created mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildSubject_DependencyCreated(t *testing.T) {
	// Note: events.entity_type is the short token "dep", but the event
	// type is "dependency.created" — the subject uses the event-type
	// short-name, so we expect ".dependency.created.".
	pid := fixedProjectID
	got := BuildSubject(SubjectInput{
		EventType:  "dependency.created",
		EntityType: "dep",
		EntityID:   "demo-42",
		ProjectID:  &pid,
	})
	want := "skein.events.project." + pid.String() + ".dependency.created.demo-42"
	if got != want {
		t.Errorf("dependency.created mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildSubject_ProjectMemberAdded(t *testing.T) {
	pid := fixedProjectID
	got := BuildSubject(SubjectInput{
		EventType:  "project.member_added",
		EntityType: "project",
		EntityID:   pid.String(),
		ProjectID:  &pid,
	})
	want := "skein.events.project." + pid.String() + ".project.member_added." + pid.String()
	if got != want {
		t.Errorf("project.member_added mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildSubject_ComponentCreated(t *testing.T) {
	pid := fixedProjectID
	cid := "deadbeef-0000-0000-0000-000000000001"
	got := BuildSubject(SubjectInput{
		EventType:  "component.created",
		EntityType: "component",
		EntityID:   cid,
		ProjectID:  &pid,
	})
	want := "skein.events.project." + pid.String() + ".component.created." + cid
	if got != want {
		t.Errorf("component.created mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// Malformed inputs should produce a subject under "skein.events.malformed"
// rather than panic. The dispatcher publishes the message anyway so an
// operator alerting on the malformed root sees the issue, but the system
// keeps making progress.

func TestBuildSubject_MissingProjectID_NonUser(t *testing.T) {
	got := BuildSubject(SubjectInput{
		EventType:  "issue.created",
		EntityType: "issue",
		EntityID:   "demo-1",
		ProjectID:  nil,
	})
	if !strings.HasPrefix(got, "skein.events.malformed.") {
		t.Errorf("expected malformed prefix, got %q", got)
	}
}

func TestBuildSubject_MalformedEventType(t *testing.T) {
	pid := fixedProjectID
	got := BuildSubject(SubjectInput{
		EventType:  "no-dot-here",
		EntityType: "issue",
		EntityID:   "demo-1",
		ProjectID:  &pid,
	})
	if !strings.HasPrefix(got, "skein.events.malformed.") {
		t.Errorf("expected malformed prefix, got %q", got)
	}
}

func TestBuildSubject_EmptyEventType(t *testing.T) {
	got := BuildSubject(SubjectInput{
		EventType:  "",
		EntityType: "issue",
	})
	if !strings.HasPrefix(got, "skein.events.malformed.") {
		t.Errorf("expected malformed prefix, got %q", got)
	}
}

// camelToKebab: exhaustive enough to catch the common patch fields.

func TestCamelToKebab(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"name", "name"},
		{"priority", "priority"},
		{"acceptanceCriteria", "acceptance-criteria"},
		{"idPrefix", "id-prefix"},
		{"defaultIdMode", "default-id-mode"},
		{"already-kebab", "already-kebab"}, // non-letter passes through
		{"ALL_CAPS", "a-l-l_-c-a-p-s"},     // not a real field name; documents behaviour
		{"camelCASE", "camel-c-a-s-e"},
	}
	for _, c := range cases {
		got := camelToKebab(c.in)
		if got != c.want {
			t.Errorf("camelToKebab(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// subjectRoot keeps publishedTotal label cardinality bounded.

func TestSubjectRoot_PrefixBased(t *testing.T) {
	// The prefix-based implementation must produce exactly four label
	// values regardless of project / user / entity counts. Anything
	// else risks blowing up the published_total cardinality at scale.
	cases := []struct {
		name, in, want string
	}{
		{
			name: "project namespace truncates before per-project uuid",
			in:   "skein.events.project.11111111-2222-3333-4444-555555555555.issue.created.demo-1",
			want: "skein.events.project",
		},
		{
			name: "user lifecycle truncates before per-user uuid",
			in:   "skein.user.77777777-7777-7777-7777-777777777777.created",
			want: "skein.user",
		},
		{
			name: "user field-changed truncates before per-user uuid",
			in:   "skein.user.77777777-7777-7777-7777-777777777777.field.name",
			want: "skein.user",
		},
		{
			name: "malformed namespace falls under its own root",
			in:   "skein.events.malformed.no_dot",
			want: "skein.events.malformed",
		},
		{
			name: "unknown subject falls under safety-net other",
			in:   "weird.unknown",
			want: "other",
		},
		{
			name: "empty subject also falls under other",
			in:   "",
			want: "other",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := subjectRoot(c.in); got != c.want {
				t.Errorf("subjectRoot(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// sanitizeToken protects the malformed fallback from injecting NATS-illegal
// characters into the subject (which would error at Publish time).

func TestSanitizeToken(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "empty"},
		{"abc", "abc"},
		{"a.b", "a_b"},
		{"a*b", "a_b"},
		{"a>b", "a_b"},
		{"a b", "a_b"},
		{"x-1_2", "x-1_2"},
	}
	for _, c := range cases {
		if got := sanitizeToken(c.in); got != c.want {
			t.Errorf("sanitizeToken(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestBuildSubject_EntityTailIsOneToken — sk-977.7.74: whatever the entity
// id holds (dots anywhere, "..", a trailing dot, "%", nothing at all), the
// project-scoped subject has exactly one non-empty token after the action
// (lifecycle) or the field name (field_changed), the token is the documented
// %XX encoding, and distinct ids give distinct tokens.
func TestBuildSubject_EntityTailIsOneToken(t *testing.T) {
	pid := fixedProjectID
	for _, tc := range []struct {
		id   string
		want string
	}{
		{"demo-42", "demo-42"},
		{"demo-42.1", "demo-42%2E1"},
		{"demo-42.1.3", "demo-42%2E1%2E3"},
		{"demo-a..b", "demo-a%2E%2Eb"},
		{"demo-x.", "demo-x%2E"},
		{"demo-a%2Eb", "demo-a%252Eb"},
		{"%", "%25"},
		{"", "%"},
		{"a b*>", "a%20b%2A%3E"},
		{"\u00e9", "%C3%A9"},
		{"11111111-2222-3333-4444-555555555555", "11111111-2222-3333-4444-555555555555"},
		{"demo-1::demo-2::blocks", "demo-1::demo-2::blocks"},
	} {
		lifecycle := BuildSubject(SubjectInput{EventType: "issue.closed", EntityType: "issue", EntityID: tc.id, ProjectID: &pid})
		field := BuildSubject(SubjectInput{EventType: "issue.field_changed", EntityType: "issue", EntityID: tc.id, ProjectID: &pid, Field: "priority"})
		for shape, subject := range map[string]struct {
			s      string
			tokens int
		}{"lifecycle": {lifecycle, 7}, "field": {field, 8}} {
			parts := strings.Split(subject.s, ".")
			if len(parts) != subject.tokens {
				t.Fatalf("%s subject of %q = %q: %d tokens, want %d", shape, tc.id, subject.s, len(parts), subject.tokens)
			}
			for _, p := range parts {
				if p == "" || strings.ContainsAny(p, " \t*>") {
					t.Fatalf("%s subject of %q = %q has an empty or wildcard token", shape, tc.id, subject.s)
				}
			}
			if got := parts[len(parts)-1]; got != tc.want {
				t.Fatalf("%s tail of %q = %q, want %q", shape, tc.id, got, tc.want)
			}
		}
	}
	// Ids that the raw scheme collapsed into the same token sequence.
	seen := map[string]string{}
	for _, id := range []string{"a.b", "a%2Eb", "a..b", "a.", "a", "", "%", "%25"} {
		tail := EncodeEntityToken(id)
		if prev, ok := seen[tail]; ok {
			t.Fatalf("ids %q and %q share the token %q", prev, id, tail)
		}
		seen[tail] = id
	}
}

// TestEncodeEntityToken_DigestBeyondTokenBound — sk-977.7.74: an id whose
// %XX encoding fits EntityTokenMaxBytes (1024) keeps it; one byte more and
// the token is "%%" + the upper-case hex SHA-256 of the id (reference
// digests computed outside this code).
func TestEncodeEntityToken_DigestBeyondTokenBound(t *testing.T) {
	fits := strings.Repeat(".", 341) + "a" // 341*3 + 1 = 1024 bytes encoded
	if got := EncodeEntityToken(fits); got != strings.Repeat("%2E", 341)+"a" {
		t.Fatalf("id encoding to exactly 1024 bytes: got a %d-byte token %.20q…", len(got), got)
	}
	for _, tc := range []struct{ id, want string }{
		{strings.Repeat(".", 342), "%%833A0585255E446C16E719145EFBFFAE39C44FCE96540C01F4E46AF7090D1626"},
		{"x" + strings.Repeat(".", 999), "%%332FF9623BE7D94CB3AAA2CB47D0A54C7F53E1810EB0EA366A28A3BB897E7239"},
	} {
		if got := EncodeEntityToken(tc.id); got != tc.want {
			t.Fatalf("id of %d bytes: got %q, want %q", len(tc.id), got, tc.want)
		}
	}
}

// TestBuildSubject_LongestIdsOfEachKindStayPublishable — sk-977.7.74: for
// the longest id of each entity kind (issue ids are varchar(1000), a
// dependency id joins two of them with the longest dependency type,
// everything else is a UUID) and the longest field the writers use, the
// subject keeps its project route, ends in one token and stays within
// SubjectMaxBytes (2048), far below NATS's 4096-byte control line.
func TestBuildSubject_LongestIdsOfEachKindStayPublishable(t *testing.T) {
	pid := fixedProjectID
	longIssue := "x" + strings.Repeat(".", 999)
	plainIssue := strings.Repeat("a", 1000)
	for _, tc := range []struct {
		name, eventType, entityType, entityID, field string
	}{
		{"issue, dotted", "issue.created", "issue", longIssue, ""},
		{"issue, plain", "issue.created", "issue", plainIssue, ""},
		{"issue field", "issue.field_changed", "issue", longIssue, "includeDeps"},
		{"dependency, dotted", "dependency.created", "dep", longIssue + "::" + longIssue + "::discovered-from", ""},
		{"dependency, plain", "dependency.created", "dep", plainIssue + "::" + plainIssue + "::discovered-from", ""},
		{"comment", "comment.created", "comment", "abcd1234-0000-0000-0000-aabbccddeeff", ""},
		{"project", "project.member_role_changed", "project", pid.String(), ""},
	} {
		got := BuildSubject(SubjectInput{EventType: tc.eventType, EntityType: tc.entityType, EntityID: tc.entityID, ProjectID: &pid, Field: tc.field})
		if len(got) > 2048 {
			t.Errorf("%s: subject of %d bytes exceeds 2048", tc.name, len(got))
		}
		if !strings.HasPrefix(got, "skein.events.project."+pid.String()+".") {
			t.Errorf("%s: subject lost its project route: %.80q…", tc.name, got)
		}
		tail := got[strings.LastIndexByte(got, '.')+1:]
		if tail == "" || len(tail) > 1024 {
			t.Errorf("%s: tail of %d bytes", tc.name, len(tail))
		}
	}
}

// TestBuildSubject_OverlongHeadGoesToMalformed — sk-977.7.74: a subject
// that would exceed SubjectMaxBytes whatever the entity token (an event
// type far outside the writers' constants) is published on the malformed
// root rather than on a line the NATS server refuses.
func TestBuildSubject_OverlongHeadGoesToMalformed(t *testing.T) {
	pid := fixedProjectID
	got := BuildSubject(SubjectInput{EventType: "issue." + strings.Repeat("a", 2100), EntityType: "issue", EntityID: "demo-1", ProjectID: &pid})
	if got != "skein.events.malformed.subject_too_long" {
		t.Fatalf("got %.80q…", got)
	}
}
