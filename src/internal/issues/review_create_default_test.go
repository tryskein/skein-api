package issues

// review_create_default_test.go — sk-303.14 T009, matrix rows 40 and 41.
//
// The personal default of the "review required" flag on issue-create
// paths, plus explicit passing.
//
// The probe everywhere is a FALSE default, not factory true. That is
// load-bearing: "the flag on the created issue is true" is equally true
// when the personal default was read and when nobody read it and a
// constant sits in the code. A default that differs from the factory one
// splits those two cases — and it is the one that goes red if prefs
// reading drops out.

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/review"
)

func readReviewRequired(t *testing.T, pool *pgxpool.Pool, issueID string) bool {
	t.Helper()
	var required bool
	if err := pool.QueryRow(context.Background(),
		`SELECT review_required FROM issues WHERE id = $1`, issueID).Scan(&required); err != nil {
		t.Fatalf("read review_required %s: %v", issueID, err)
	}
	return required
}

// setPersonalReviewDefault writes the personal flag default and ASSERTS
// that the written value differs from the factory one.
//
// The check lives in the helper, not in each test: without it, changing
// the built-in default once would turn the probes below into tautologies
// ("the default applied" would be proven by matching the constant), and
// no test would notice.
func setPersonalReviewDefault(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, value bool) {
	t.Helper()
	if value == review.DefaultReviewRequired {
		t.Fatalf("probe sets personal default %v, equal to the factory one — such a probe "+
			"does not tell \"personal default was read\" from \"a constant sits in the code\"", value)
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO user_notification_prefs (user_id, review_required_default, updated_at)
VALUES ($1, $2, now())
ON CONFLICT (user_id) DO UPDATE SET review_required_default = EXCLUDED.review_required_default`,
		userID, value); err != nil {
		t.Fatalf("set personal review default: %v", err)
	}
}

func assertNoPrefsRow(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM user_notification_prefs WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatalf("count prefs rows: %v", err)
	}
	if n != 0 {
		t.Fatalf("precondition: the user already has a prefs row (%d) — the test below "+
			"would be checking the written value, not the factory one", n)
	}
}

// TestCreate_ReviewRequired_PersonalDefaultApplies — row 40, the ordinary
// create path (the same one REST create and agent create call).
func TestCreate_ReviewRequired_PersonalDefaultApplies(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	name := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	store := NewStore(pool)

	setPersonalReviewDefault(t, pool, creator, false)

	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{Title: "by default", Type: "task", Priority: 2},
		creator, HumanActor(creator, name))
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if got := readReviewRequired(t, pool, dto.ID); got != false {
		t.Errorf("review_required = %v, want the author's personal default (false) — the "+
			"create path does not read user_notification_prefs.review_required_default (US-3)", got)
	}
}

// TestCreate_ReviewRequired_FactoryWhenNoPrefsRow — the built-in default for
// a user who has never saved the setting.
//
// This is not a duplicate of the previous test, it is exactly the trap
// the built-in default lives as a constant in the code to catch:
// user_notification_prefs rows are created lazily, so such a user has NO
// row, and the column DEFAULT never fires. Without a fallback in the
// reader a zero bool would land here — "not required", i.e. the mechanic
// would silently not work for everyone who has never touched prefs.
func TestCreate_ReviewRequired_FactoryWhenNoPrefsRow(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	name := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	store := NewStore(pool)

	assertNoPrefsRow(t, pool, creator)

	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{Title: "no prefs", Type: "task", Priority: 2},
		creator, HumanActor(creator, name))
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if got := readReviewRequired(t, pool, dto.ID); got != review.DefaultReviewRequired {
		t.Errorf("review_required = %v, want factory %v — for a user with no prefs "+
			"row the mechanic ended up switched off (US-3)",
			got, review.DefaultReviewRequired)
	}
}

// TestCreate_ReviewRequired_ExplicitWins — row 41: explicit passing on the
// agent path overrides the personal default in BOTH directions.
//
// Both directions are required: checking only "explicit false against a
// true default" would also pass for an implementation that ignores the
// explicit value and always writes false.
func TestCreate_ReviewRequired_ExplicitWins(t *testing.T) {
	for _, tc := range []struct {
		name           string
		personalIsSet  bool
		personal       bool
		explicit       bool
		wantPersistent bool
	}{
		{name: "explicit false against factory true", explicit: false, wantPersistent: false},
		{name: "explicit true against personal default false", personalIsSet: true, personal: false, explicit: true, wantPersistent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestPool(t)
			creator := seedUser(t, pool)
			name := readUsername(t, pool, creator)
			pid, prefix := seedProject(t, pool, creator)
			store := NewStore(pool)

			if tc.personalIsSet {
				setPersonalReviewDefault(t, pool, creator, tc.personal)
			} else {
				assertNoPrefsRow(t, pool, creator)
			}

			explicit := tc.explicit
			dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
				CreateIssueInput{
					Title: "explicit", Type: "task", Priority: 2,
					ReviewRequired: &explicit,
				},
				creator, HumanActor(creator, name))
			if err != nil {
				t.Fatalf("CreateIssue: %v", err)
			}
			if got := readReviewRequired(t, pool, dto.ID); got != tc.wantPersistent {
				t.Errorf("review_required = %v, want the explicitly passed %v",
					got, tc.wantPersistent)
			}
		})
	}
}

// TestCreateEpic_ReviewRequired_DefaultAppliesToEpicAndChildren — row 40
// on the second create path: an epic with children.
//
// Children are checked separately from the epic: these are two different
// insert calls, and a claim about the epic says nothing about a child.
func TestCreateEpic_ReviewRequired_DefaultAppliesToEpicAndChildren(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	name := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	store := NewStore(pool)

	setPersonalReviewDefault(t, pool, creator, false)

	epic, children, err := store.CreateEpicWithChildren(t.Context(), pid, prefix, "counter",
		CreateIssueInput{Title: "epic", Type: TypeEpic, Priority: 2},
		[]CreateIssueInput{
			{Title: "child 1", Type: "task", Priority: 2},
			{Title: "child 2", Type: "task", Priority: 2},
		},
		SourceHuman, creator, HumanActor(creator, name))
	if err != nil {
		t.Fatalf("CreateEpicWithChildren: %v", err)
	}
	if got := readReviewRequired(t, pool, epic.ID); got != false {
		t.Errorf("epic: review_required = %v, want the author's personal default (false)", got)
	}
	if len(children) != 2 {
		t.Fatalf("created %d children, want 2", len(children))
	}
	for _, child := range children {
		if got := readReviewRequired(t, pool, child.ID); got != false {
			t.Errorf("child %s: review_required = %v, want the author's personal default (false) — "+
				"the epic-child create path applies the default separately from the epic", child.ID, got)
		}
	}
}

// TestCreate_SingleInsertPathNamesReviewRequired is a guard of the
// "EVERY create path" requirement, built so silence is impossible.
//
// Enumerating paths in the test is not an option: an enumeration is
// silent on a miss — a path added tomorrow and not put on the list would
// produce an issue with the flag from the schema DEFAULT (false, the
// historical SC-015 value), and no test would see it. So what is checked
// is not a list of paths, but the invariant that makes the list
// unnecessary: in production code there is EXACTLY ONE issue insert, and
// it names the flag column. A second INSERT INTO issues IS a new create
// path, and it must go red here.
func TestCreate_SingleInsertPathNamesReviewRequired(t *testing.T) {
	// ".." is the whole internal/ directory: a create path may appear in
	// any package, not only this one.
	const root = ".."
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// internal/testsupport is a fixture factory, not a product
			// create path: it seeds rows directly, bypassing business
			// rules, and does not need the flag by construction.
			if d.Name() == "testsupport" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Parse, do not substring-search the file: the words
		// "INSERT INTO issues" also appear in comments, and a comment
		// does not create an issue. Only STRING LITERALS are counted —
		// what actually goes to the database.
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if !strings.Contains(lit.Value, "INSERT INTO issues") {
				return true
			}
			found = append(found, path+": "+columnList(
				lit.Value[strings.Index(lit.Value, "INSERT INTO issues"):]))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("issue inserts in production code: %d, want one — %v.\n"+
			"Every issue insert must set review_required from the author's personal "+
			"default (US-3). A second insert is a new create path: either run it "+
			"through createIssueInTx, or spread the default onto it and update this "+
			"guard on purpose", len(found), found)
	}
	if !strings.Contains(found[0], "review_required") {
		t.Errorf("the only issue insert does not name review_required: %s — "+
			"the flag will come from the schema DEFAULT (false, the historical SC-015 "+
			"value), not from the author's personal default", found[0])
	}
}

// columnList cuts out the insert column list — everything up to the first
// closing parenthesis. It is that list, not the first line of the fragment:
// the column enumeration is split across lines, and the claim that
// review_required is present must see the list whole.
func columnList(s string) string {
	if end := strings.Index(s, ")"); end >= 0 {
		s = s[:end+1]
	}
	return strings.Join(strings.Fields(s), " ")
}
