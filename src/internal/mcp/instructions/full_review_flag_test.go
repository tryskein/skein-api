package instructions

// full_review_flag_test.go — sk-303.14, task T022.
//
// The hole found in review: the review workflow changes the behaviour of
// a whole class of issues, observable by agents, and the guide an agent
// reads at the start of EVERY session said not a word about it.
// `reviewRequired` was described only in one tool's schema — i.e. only
// someone who had already decided to pass it would see it.
//
// The section has been added. This gate keeps it from vanishing silently
// and, more importantly, from DRIFTING FROM THE CODE: two claims of the
// section are checked not against the section itself, but against the
// sources that make them true.
//
// ⚠ Why not "check that the words are there". A check "the text contains
// the word" is equally green when the text tells the truth and when it
// lies. For two claims — the flag's built-in default and the mixed-request
// rejection — the truth lives in the code and is read from there.

import (
	"go/ast"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"

	"skein-api/internal/astguard"
	"skein-api/internal/review"
)

const reviewFlagHeading = "## Review flag — the author's private bookkeeping"

// TestFullGuide_HasReviewFlagSection — the section and its operational
// facts.
//
// The list is what an agent acts WRONG without, not a paraphrase of the
// prose: privacy (otherwise the agent discusses the mark with the
// assignee), the personal default on create (otherwise the request "don't
// notify me on completion" stays in the description and changes nothing),
// a call of its own (otherwise the patch is rejected whole and the agent
// thinks it applied), no agent tool for the mark (otherwise it will look
// for one).
func TestFullGuide_HasReviewFlagSection(t *testing.T) {
	requireSectionFacts(t, reviewFlagHeading, []string{
		"reviewRequired",
		"skein_update_many",
		"PERSONAL DEFAULT",
		"in a call of its own",
		"rejected whole",
		"no agent tool",
	})
}

// TestFullGuide_ReviewFlagDefaultMatchesTheCode — the flag's factory
// value in the guide against the constant that sets it.
//
// Both directions are real. If the guide said "not required by default"
// — the agent would stop passing `false` where the user asked not to be
// notified, because it would decide that is already the default. If the
// constant flipped — the guide would become a lie without changing a
// single character.
func TestFullGuide_ReviewFlagDefaultMatchesTheCode(t *testing.T) {
	body := sectionBody(t, FullGuide, reviewFlagHeading)
	if body == "" {
		t.Fatalf("section %q is missing", reviewFlagHeading)
	}
	// A markdown line wrap must not decide the check: it lands wherever
	// it lands, and the first edition of this gate stayed green right up
	// to a paragraph reflow. Whitespace runs are collapsed.
	flat := strings.Join(strings.Fields(body), " ")

	if review.DefaultReviewRequired {
		if !strings.Contains(flat, "built-in default is `true`") {
			t.Error("review.DefaultReviewRequired is true, but the section does not " +
				"name `true` as the built-in default — the agent will not learn that " +
				"\"do not notify\" must be passed explicitly")
		}
		if !strings.Contains(flat, "reviewRequired: false") {
			t.Error("the section does not show HOW the request \"do not notify on " +
				"completion\" is expressed: without an explicit `reviewRequired: false` " +
				"the agent will leave it in the description, where it means nothing")
		}
		return
	}
	t.Error("review.DefaultReviewRequired is false — the built-in default flipped, " +
		"and the guide section together with this gate must be rewritten, not left as-is")
}

// TestFullGuide_ReviewFlagRejectionMatchesTheTools — rejection claims
// against the code that produces those rejections.
//
// The section promises two things: a mixed request is rejected WHOLE
// (nothing was written) and the batch path does not accept the key at
// all. Both live in internal/mcp/tools/review_flag.go; if either
// vanished there — the guide would promise behaviour that does not
// exist.
func TestFullGuide_ReviewFlagRejectionMatchesTheTools(t *testing.T) {
	const src = "../tools/review_flag.go"
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("reading %s: %v — the subject of the claim vanished, the check did not break", src, err)
	}
	code := string(raw)

	// Long messages in the source are built by concatenation and wrapped
	// across lines. Neither must decide the check, so concatenation seams
	// are stripped and whitespace runs collapsed: otherwise the gate
	// would go red on a reflow, not on a removed promise.
	flat := strings.Join(strings.Fields(goStringSeam.ReplaceAllString(code, "")), " ")

	for _, want := range []struct{ fragment, why string }{
		{"cannot be combined with other changes",
			"rejection of a mixed skein_update request"},
		{"Nothing was written",
			"the promise \"nothing was written\", which the section repeats as fact"},
		{"is not accepted by skein_update_many",
			"rejection of the batch path"},
	} {
		if !strings.Contains(flat, want.fragment) {
			t.Errorf("%s no longer contains %q (%s) — the guide promises behaviour that does not exist",
				src, want.fragment, want.why)
		}
	}
}

// goStringSeam — the concatenation seam of Go string literals: `" +` … `"`.
var goStringSeam = regexp.MustCompile(`"\s*\+\s*"`)

// ---------------------------------------------------------------------------
// The two most expensive claims of the section (review-repair A/9, finding K)
// ---------------------------------------------------------------------------
//
// ⚠ WHAT WAS WRONG. Two claims of the section were checked against the
// code — the personal-default built-in default and the batch-path rejection
// — and the two REMAINING ones, those whose cost of error is highest,
// were checked against nothing. A critic's mutation rewrote the section
// as "the assignee sees both on the card, and setting the flag moves
// version and updatedAt like any other patch" plus "any project admin
// can set it too" — the run stayed green.
//
// Those are direct inversions of INV-2 and INV-4 — the ones the plan's
// risk register marks as "trust is lost irreversibly": an agent that
// believed the first will discuss a private mark with the assignee; an
// agent that believed the second will call the tool as an admin and get
// a refusal without understanding why.
//
// They must be checked not as section-text against section-text, but
// against the code that makes them true. Both live in internal/issues.

// TestFullGuide_PrivacyClaimMatchesTheCode — INV-2, both halves.
//
// The section asserts two things, and each is checked by its own code
// property:
//
//  1. "the assignee sees neither, on any surface" — a SINGLE gate fills
//     the exported card fields, and for a non-author it ZEROES them.
//     Fail-closed: a path that forgot the gate returns a card without
//     the fields;
//  2. "neither moves version or updatedAt" — the mechanic's operations
//     write neither version nor updated_at. That is read from their
//     SQL, not from the promise.
func TestFullGuide_PrivacyClaimMatchesTheCode(t *testing.T) {
	body := sectionBody(t, FullGuide, reviewFlagHeading)
	flat := strings.Join(strings.Fields(body), " ")
	for _, want := range []string{
		"the assignee sees neither",
		"neither moves `version` or `updatedAt`",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("the section does not assert %q — the mechanic's privacy (INV-2) "+
				"the agent learns only from here, and without the claim it will "+
				"discuss the mark with the assignee", want)
		}
	}

	// Half 1: the visibility gate is in place and zeroes the fields for a
	// non-author.
	gate := readSource(t, "../../issues/review_visibility.go")
	for _, want := range []struct{ fragment, why string }{
		{"caller != d.CreatedBy",
			"the gate's predicate: the fields are issued ONLY to the author. Neither " +
				"role, nor assigneeship, nor project membership widens it"},
		{"d.ReviewRequired = nil",
			"for a non-author the field is ZEROED, not \"simply left unfilled\": a " +
				"card that went through hydration twice would otherwise carry the first " +
				"pass's value"},
	} {
		if !strings.Contains(gate, want.fragment) {
			t.Errorf("the visibility gate no longer contains %q (%s) — the section promises "+
				"privacy the code does not provide", want.fragment, want.why)
		}
	}

	// Half 2: no mechanic operation moves version / updated_at.
	//
	// The UPDATE issues statements are the ones that are read: SELECTs of
	// those columns are legitimate and have nothing to do with the claim.
	ops := readSource(t, "../../issues/review_ops.go")
	stmts := updateIssuesStatements(ops)
	// Without this the assertion below is tautologically green: a file
	// that stopped containing UPDATE statements (rename, extract to a
	// separate layer) would pass as "nothing moves".
	if len(stmts) < 2 {
		t.Fatalf("found %d UPDATE issues statements in review_ops.go, want at "+
			"least two (the flag and the mark) — the parse broke or the operations "+
			"moved, and the claim that version does not move is no longer looking at anything", len(stmts))
	}
	for _, stmt := range stmts {
		for _, forbidden := range []string{"version", "updated_at"} {
			if strings.Contains(stmt, forbidden) {
				t.Errorf("a mechanic operation writes %q: %q. The section promises that "+
					"the flag and the mark move neither version nor updatedAt — that is "+
					"what INV-2 rests on: a version bump itself tells the assignee that "+
					"\"something happened\"", forbidden, strings.TrimSpace(stmt))
			}
		}
	}
}

// TestFullGuide_AuthorOnlyClaimMatchesTheCode — INV-4.
//
// The section says `Author only — "not the author" includes admins`. The
// claim is strong and is easily broken by an "improvement": the admin is
// given access because "they are an admin". It is held by two properties
// of the code: authorship is checked by direct equality with the creator,
// and role does not participate in that decision AT ALL.
func TestFullGuide_AuthorOnlyClaimMatchesTheCode(t *testing.T) {
	body := sectionBody(t, FullGuide, reviewFlagHeading)
	flat := strings.Join(strings.Fields(body), " ")
	if !strings.Contains(flat, `"not the author" includes admins`) {
		t.Error(`the section does not say that "not the author" includes admins — without that ` +
			`the agent will call the tool as an admin and get a refusal without ` +
			`understanding why (INV-4)`)
	}

	ops := readSource(t, "../../issues/review_ops.go")
	if n := strings.Count(ops, "row.CreatedBy != input.Actor.UserID"); n < 2 {
		t.Errorf("authorship checks in review_ops.go — %d, want at least two "+
			"(the flag and the mark): an operation without such a check would "+
			"hand the mechanic to anyone", n)
	}
	// Role does not participate in the decision. Role appearing here is
	// exactly the "improvement" that breaks INV-4.
	//
	// ⚠ The TREE is asked, not the text. A substring search would go red
	// on a comment EXPLAINING why there is no admin branch here — i.e. on
	// the most valuable thing in the file. Verified: the first edition of
	// this gate failed that way. (review_ops.go has such a comment: the
	// line about the missing IsAdmin branch. It must stay green.)
	//
	// ⚠ WHAT THIS BAN CATCHES AND WHAT IT DOES NOT (review A/9, finding
	// P-9). The previous edition knew THREE spellings — Contains(name,
	// "Admin"), =="Role", =="role" — and a bypass `auth.IsElevated(...)`
	// or `Username == "admin"` was invisible to it; the boundary was
	// named nowhere, so a reader had every reason to treat the ban as
	// complete. Now the KIND of the token is searched (roleWords,
	// case-insensitive) and, separately, string literals, so role cannot
	// enter the decision as a string.
	//
	// THE BOUNDARY THAT REMAINS, and it is named: ONE file is checked,
	// issues/review_ops.go. A decision extracted into another package and
	// arriving here as a ready bool (`policy.Allows(ctx, u)`) is not
	// visible from here by any token — statically that is not derivable
	// without analysing the whole call graph. The ban holds the place
	// where the decision is MADE today; extracting the decision out is a
	// change that must pass review on its own.
	pkg, err := astguard.ParseFile("../../issues/review_ops.go")
	if err != nil {
		t.Fatalf("parsing review_ops.go: %v", err)
	}
	used := pkg.UsedIdents()
	for name, n := range pkg.SelectedNames() {
		used[name] += n
	}
	if len(used) == 0 {
		t.Fatal("no used name found in review_ops.go — " +
			"the parse broke, and the ban below is tautologically green")
	}
	for name := range used {
		if w := roleWordIn(name); w != "" {
			t.Errorf("review_ops.go uses %s (role token: %q) — if role entered "+
				"the decision, \"not the author includes admins\" stopped being true, "+
				"and the section still promises it (INV-4)", name, w)
		}
	}
	// Role can enter the decision as a STRING, not a name: `Username ==
	// "admin"` contains no identifier with a role token at all. Literals
	// are therefore looked at separately — comments do not land here, as
	// they do not in UsedIdents.
	for _, file := range pkg.Files {
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if w := roleWordIn(lit.Value); w != "" {
				t.Errorf("review_ops.go contains string literal %s (role token: %q) — "+
					"role that entered the decision as a string, not a name, breaks INV-4 "+
					"the same way", lit.Value, w)
			}
			return true
		})
	}
}

// roleWords — role tokens in a name or a string. The list is OPEN by
// design: it catches the kind, it does not enumerate known-bad.
var roleWords = []string{"admin", "role", "elevat", "privileg", "superuser", "sudo"}

// roleWordIn returns the first role token found in s (case-insensitive),
// or the empty string.
func roleWordIn(s string) string {
	low := strings.ToLower(s)
	for _, w := range roleWords {
		if strings.Contains(low, w) {
			return w
		}
	}
	return ""
}

// updateIssuesStatements cuts `UPDATE issues …` statements up to a
// semicolon or the end of a string literal.
func updateIssuesStatements(src string) []string {
	var out []string
	const marker = "UPDATE issues"
	from := 0
	for {
		i := strings.Index(src[from:], marker)
		if i < 0 {
			return out
		}
		i += from
		stmt := src[i:]
		if end := strings.IndexAny(stmt, "`\""); end > 0 {
			stmt = stmt[:end]
		}
		out = append(out, stmt)
		from = i + len(marker)
	}
}

// readSource reads the file that is the subject of the claim. Its
// absence is the subject vanishing, not a broken environment: the
// package and its test live in the same tree.
func readSource(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v — the subject of the claim vanished, the check did not break", path, err)
	}
	return string(raw)
}
