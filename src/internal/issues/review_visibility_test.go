package issues

// review_visibility_test.go — sk-303.14 T006, contract C1.
//
// This file proves the FORM of the gate: that "no key" is the observable
// outcome for a non-author, and that there is nowhere else these fields can
// be filled. That the gate actually fires on every real card return is
// proven by handler/issue_card_review_privacy_test.go (REST) and
// mcp/tools/show_review_privacy_test.go (the agent path); matrix row 22 is
// closed by all three together.

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/auth"
)

// reviewWireKeys are the three review-mechanic keys as they look on the
// wire. Written by hand: a list derived from the struct would agree with
// any change to it.
var reviewWireKeys = []string{"reviewRequired", "reviewedAt", "reviewedBy"}

// detailWithRawReview builds a card the way row scanning leaves it: raw
// fields filled, exported ones not.
func detailWithRawReview(author uuid.UUID) *IssueDetail {
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	by := author
	return &IssueDetail{
		ID:                "sk-1",
		CreatedBy:         author,
		rawReviewRequired: true,
		rawReviewedAt:     &at,
		rawReviewedBy:     &by,
	}
}

// marshalKeys serialises the card and returns its keys as a set — what
// the client actually sees.
func marshalKeys(t *testing.T, d *IssueDetail) map[string]any {
	t.Helper()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

// TestReviewVisibility_AuthorSeesAllThree — the positive half: the author
// gets the fields, and they arrive with the values from the row, not zeros.
func TestReviewVisibility_AuthorSeesAllThree(t *testing.T) {
	author := uuid.New()
	d := detailWithRawReview(author)
	d.applyReviewVisibility(auth.WithUserID(context.Background(), author))

	body := marshalKeys(t, d)
	for _, key := range reviewWireKeys {
		if _, ok := body[key]; !ok {
			t.Errorf("author card does not carry %q — US-1 requires the author "+
				"to see the flag and the mark on the detail card itself", key)
		}
	}
	if got := body["reviewRequired"]; got != true {
		t.Errorf("reviewRequired = %#v, want true — the value must come from "+
			"the row, not from the struct zero", got)
	}
	if d.ReviewedBy == nil || *d.ReviewedBy != author {
		t.Errorf("reviewedBy = %v, want the mark author %v", d.ReviewedBy, author)
	}
}

// TestReviewVisibility_AuthorSeesFalseFlagExplicitly — a case worth
// checking as a case: a flag the author turned off must arrive to the
// author as an EXPLICIT false, not vanish along with privacy. On a flat
// bool with omitempty it would vanish, and the author could not tell
// "cleared the flag" from "no fields".
func TestReviewVisibility_AuthorSeesFalseFlagExplicitly(t *testing.T) {
	author := uuid.New()
	d := &IssueDetail{ID: "sk-1", CreatedBy: author, rawReviewRequired: false}
	d.applyReviewVisibility(auth.WithUserID(context.Background(), author))

	body := marshalKeys(t, d)
	got, ok := body["reviewRequired"]
	if !ok {
		t.Fatalf("cleared flag vanished from the author's card: %v", body)
	}
	if got != false {
		t.Errorf("reviewRequired = %#v, want false", got)
	}
}

// TestReviewVisibility_NonAuthorHasNoKeysAtAll — the central claim of
// INV-2 and row 22: a non-author has NO KEYS IN THE OBJECT. Absence is
// what is checked, not the value: `null`, `false` and zero fail here the
// same as a true value, because each of them tells the reader the
// mechanic exists.
func TestReviewVisibility_NonAuthorHasNoKeysAtAll(t *testing.T) {
	author := uuid.New()

	cases := map[string]context.Context{
		"assignee": auth.WithUserID(context.Background(), uuid.New()),
		// INV-4: a role widens visibility, not authorship. The refusal
		// to an admin is the same as to any non-author.
		"admin": auth.WithUserRole(
			auth.WithUserID(context.Background(), uuid.New()), "admin"),
		// Context with no user — background workers, service utilities.
		// Not the author, so no fields.
		"context without user": context.Background(),
		// uuid.Nil must not accidentally match anything.
		"nil identifier": auth.WithUserID(context.Background(), uuid.Nil),
	}

	for name, ctx := range cases {
		t.Run(name, func(t *testing.T) {
			d := detailWithRawReview(author)
			d.applyReviewVisibility(ctx)

			body := marshalKeys(t, d)
			for _, key := range reviewWireKeys {
				if v, ok := body[key]; ok {
					t.Errorf("key %q is present with value %#v. INV-2 requires "+
						"ABSENCE: `null` and `false` tell the reader the mechanic "+
						"exists and did not fire on this issue — exactly what the "+
						"invariant must hide", key, v)
				}
			}
		})
	}
}

// TestReviewVisibility_ClosesFieldsAlreadyOpened — the gate must CLOSE,
// not merely "not open". A card whose fields are already filled (re-
// hydration, a future path that set them early) must lose them for a
// non-author.
func TestReviewVisibility_ClosesFieldsAlreadyOpened(t *testing.T) {
	author := uuid.New()
	d := detailWithRawReview(author)
	d.applyReviewVisibility(auth.WithUserID(context.Background(), author))
	if d.ReviewRequired == nil {
		t.Fatalf("precondition failed: fields were not issued to the author")
	}

	d.applyReviewVisibility(auth.WithUserID(context.Background(), uuid.New()))
	if d.ReviewRequired != nil || d.ReviewedAt != nil || d.ReviewedBy != nil {
		t.Errorf("gate did not close already-open fields: %v / %v / %v",
			d.ReviewRequired, d.ReviewedAt, d.ReviewedBy)
	}
}

// TestReviewVisibility_FlagIsACopyNotAnAlias — the flag is copied, not
// taken as the address of the raw field. Otherwise a write through the
// pointer on the card would silently change the state the gate reads on
// the next call.
func TestReviewVisibility_FlagIsACopyNotAnAlias(t *testing.T) {
	author := uuid.New()
	d := detailWithRawReview(author)
	d.applyReviewVisibility(auth.WithUserID(context.Background(), author))

	*d.ReviewRequired = false
	if !d.rawReviewRequired {
		t.Errorf("writing ReviewRequired through the pointer changed the raw state — " +
			"the exported field must be a copy, not a window into the row")
	}
}

// TestReviewVisibility_SingleWriter is a structural guard, not a behaviour
// check.
//
// It exists because the fail-closed scheme rests on exactly one property:
// the exported fields are filled NOWHERE except at the gate. While that
// holds, a path that forgets to call the gate returns a card without the
// fields — it errs toward privacy. The moment a second fill site appears
// — e.g. a "convenient" assignment of the fields right in the mark handler
// — the scheme quietly flips: the card starts arriving filled before the
// gate, and the behavioural tests above will not notice, because they look
// at the gate, not at a path around it.
//
// # What the guard sees
//
// It walks the AST of the WHOLE module (not only the issues package) and
// fails on two fill forms:
//
//	x.ReviewRequired = …                 assignment into a selector;
//	issues.IssueDetail{ReviewedAt: …}    a composite-literal key.
//
// Both forms and both scopes are needed: the wave A/3 review showed that a
// guard that knows only assignments and only its own package reads as a
// guarantee it does not give — the store returns the card outward, and
// anyone holding it can fill its fields.
//
// The check is by field NAME, not by type: type resolution would need
// go/types and a full module build in the test. The name is a conscious
// choice — over-report (a false red on a foreign struct with the same
// field name) costs one line in the exemption list and is visible at
// once; under-report is never visible. Field declarations in structs and
// reads do not fall under this rule: the guard looks only at writes.
//
// The exemption list anticipated by the paragraph above is opened by wave
// A/4 (T009): `CreateIssueInput.ReviewRequired` is the INPUT of the create
// operation, not a card field. It carries the caller's intent ("create
// with this flag" or nil — "apply the author's personal default") and is
// never returned outward; the card is still filled only by the gate. The
// exemption is by the TYPE NAME of the literal and only for the
// "composite-literal key" form: assignment into a selector
// (`x.ReviewRequired = …`) stays forbidden everywhere, a literal with an
// unspecified type (a nested `{…}`) does NOT fall under the exemption,
// and `IssueDetail` is of course absent from the list — which is asserted
// separately below, so the exemption cannot be widened to the card
// unnoticed.
//
// # What the guard does NOT see — and this is written, not implied
//
//   - A write through reflection, including encoding/json.Unmarshal
//     straight into IssueDetail. Parsing inbound JSON into a card is not
//     the path by which a card leaves, but the AST will not show that
//     write at all.
//   - A write through a pointer taken on the field (`p := &d.ReviewedAt`),
//     and unsafe.
//   - A value put into a raw field not by row scanning but by hand: that
//     is a legal form, but a wrong raw value the gate will honestly show
//     the author. The guard protects PRIVACY, not correctness.
//
// Row scanning puts values into raw fields and does not fall under the
// rule — which is the split the rule protects.
func TestReviewVisibility_SingleWriter(t *testing.T) {
	// The only place allowed to fill these fields.
	const gateFile = "internal/issues/review_visibility.go"
	guarded := map[string]bool{"ReviewRequired": true, "ReviewedAt": true, "ReviewedBy": true}

	// Module root: the test runs from internal/issues.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("module root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found (%s): %v — the guard must walk the whole "+
			"module, not its own package", root, err)
	}

	fset := token.NewFileSet()
	scanned := 0

	report := func(rel string, pos token.Pos, field, form string) {
		t.Errorf("%s:%d %s %s. Only applyReviewVisibility in %s may fill "+
			"these fields: that is what fail-closed rests on — a path that "+
			"forgot the gate returns a card WITHOUT the fields, not with them. A "+
			"second fill site flips the scheme toward a leak (INV-2, SC-028). "+
			"Write the raw fields and call the gate.",
			rel, fset.Position(pos).Line, form, field, gateFile)
	}

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// None of these directories assemble a card.
			switch d.Name() {
			case "vendor", "testdata", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			// Test files are excluded on purpose: the guard protects
			// the production card path. A test that builds a card by
			// hand puts nothing on the wire.
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		if rel == gateFile {
			return nil
		}

		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parsing %s: %w", rel, perr)
		}
		scanned++

		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.AssignStmt:
				for _, lhs := range node.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if ok && guarded[sel.Sel.Name] {
						report(rel, sel.Pos(), sel.Sel.Name, "assigns")
					}
				}
			case *ast.CompositeLit:
				// A positional literal of the guarded type fills ALL
				// fields at once, including the private ones, and has no
				// key-value pairs — the loop below would skip it silently.
				// Catch it by type name: this is the only write form the
				// guard would otherwise not see.
				if isGuardedTypeName(node.Type) {
					for _, elt := range node.Elts {
						if _, isKV := elt.(*ast.KeyValueExpr); !isKV {
							report(rel, node.Pos(), guardedTypeName, "fills with a positional literal")
							break
						}
					}
				}
				if !isInputTypeName(node.Type) {
					for _, elt := range node.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						key, ok := kv.Key.(*ast.Ident)
						if ok && guarded[key.Name] {
							report(rel, key.Pos(), key.Name, "fills with a composite literal")
						}
					}
				}
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walking the module: %v", walkErr)
	}
	// A guard that walked nothing is green for the same reason a guard
	// that found nothing is green. The threshold is coarse on purpose: it
	// catches a broken root, it does not count files.
	if scanned < 50 {
		t.Fatalf("guard walked only %d module files — the walk is broken, and "+
			"its green colour means nothing", scanned)
	}
}

// inputTypeNames are INPUT types whose field of a guarded name carries the
// caller's intent, not a card value, and never leaves outward.
//
// The list is closed and deliberately short: each entry is a type proven
// not to serialise to a recipient. `IssueDetail` is not in it and cannot
// be; the guard below asserts that, so widening the exemption to the card
// cannot happen silently.
var inputTypeNames = map[string]bool{
	// sk-303.14 T009: input of the create operation. `ReviewRequired *bool`
	// here is "create with this flag" or nil — "apply the author's
	// personal default".
	"CreateIssueInput": true,
}

func isInputTypeName(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.Ident:
		return inputTypeNames[t.Name]
	case *ast.SelectorExpr:
		return t.Sel != nil && inputTypeNames[t.Sel.Name]
	}
	// A literal with an unspecified type (a nested `{…}`) is not exempt:
	// the syntax does not say whose it is, and the guard must err toward
	// red.
	return false
}

// TestReviewVisibility_InputExemptionExcludesTheCard is a guard over the
// guard.
//
// The exemption in the previous test weakens the check by exactly the
// size of its list. A single "IssueDetail" line in it would disable card
// protection entirely, and both tests would stay green. So the list is
// checked separately.
func TestReviewVisibility_InputExemptionExcludesTheCard(t *testing.T) {
	if inputTypeNames[guardedTypeName] {
		t.Fatalf("card type %q landed in the exemption list — the single-writer "+
			"guard stopped protecting the card while staying green (INV-2, SC-028)",
			guardedTypeName)
	}
	if len(inputTypeNames) == 0 {
		t.Fatal("exemption list is empty — then the exempting branch is dead, and " +
			"its green colour means nothing")
	}
}

// guardedTypeName is the type whose positional literal fills the private
// fields too, all at once. The guard works on syntax, without type
// resolution, so it matches on NAME: `IssueDetail{…}` and
// `issues.IssueDetail{…}`.
const guardedTypeName = "IssueDetail"

func isGuardedTypeName(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name == guardedTypeName
	case *ast.SelectorExpr:
		return t.Sel != nil && t.Sel.Name == guardedTypeName
	}
	return false
}
