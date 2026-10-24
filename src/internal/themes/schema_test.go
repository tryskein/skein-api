package themes

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goldenCataloguePath is the frozen rendering of TokenSchema that
// TestCatalogueGolden compares against. One line per token, tab-
// separated: section, key, kind, label, light, dark.
const goldenCataloguePath = "testdata/token_catalogue.tsv"

// updateGoldenEnv, when set to "1", rewrites the golden file instead of
// asserting against it. Deliberately an env var and not a `-update`
// flag: regeneration must be a sentence someone typed on purpose, and
// it is spelled out in the failure message so nobody has to guess it.
const updateGoldenEnv = "SKEIN_UPDATE_TOKEN_CATALOGUE"

// renderCatalogue renders TokenSchema in the golden file's format:
// section, key, kind, label, light, dark — tab separated, one token per
// line, in catalogue order.
func renderCatalogue() string {
	var b strings.Builder
	for _, sec := range TokenSchema {
		for _, tok := range sec.Tokens {
			fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\t%s\n",
				sec.ID, tok.Key, tok.Kind, tok.Label, tok.Light, tok.Dark)
		}
	}
	return b.String()
}

// TestCatalogueGolden pins every VALUE in the catalogue. It also keeps
// the golden file honest for `scripts/check-theme-mirror.sh` at the
// workspace root, which reads the golden as the backend side of the
// mirror and runs this test first.
//
// The consequence was not theoretical. When this guard was written the
// mirror carried --radius-control at 2px against the registry's 6px and
// --radius-panel at 6px against 12px; both are pure values, both were
// shipped through skein_theme(action="fork") onto every forked theme's
// :root, and NOT ONE TEST IN THIS REPOSITORY SAW THEM. Mutating
// --primary's light column to #00ff00, or replacing all six type steps,
// left the suite green.
//
// Why a golden FILE rather than a single digest constant. A digest is
// the cheaper thing to write and the more expensive thing to be handed:
// "the catalogue hash changed" names no key, no column and no
// direction, so the next agent re-derives the whole diff by hand before
// it can even tell a deliberate retune from a typo. A golden file costs
// the same to maintain — one regeneration — and the failure it prints
// IS the diff: added keys, removed keys, and per-key old → new on the
// exact column that moved. Byte-for-byte comparison of a rendered
// catalogue is also strictly stronger than a hash of the same bytes:
// same coverage, better message.
//
// It does NOT check the frontend. Nothing in this repository can — see
// the docblock on TokenSchema; `scripts/check-theme-mirror.sh` at the
// workspace root is what compares the two trees. This guard's job is
// narrower and worth having on its own: no value in the mirror moves
// without someone saying so.
//
// When the catalogue legitimately changes, regenerate:
//
//	SKEIN_UPDATE_TOKEN_CATALOGUE=1 go test ./internal/themes/ -run TestCatalogueGolden
//
// then READ the resulting diff in git before committing it — the golden
// file is only a guard while somebody looks at what it stopped being.
func TestCatalogueGolden(t *testing.T) {
	got := renderCatalogue()

	if os.Getenv(updateGoldenEnv) == "1" {
		if err := os.MkdirAll(filepath.Dir(goldenCataloguePath), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(goldenCataloguePath, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("%s rewritten (%d tokens) — review the diff before committing",
			goldenCataloguePath, len(TokenSchemaKeys()))
		return
	}

	raw, err := os.ReadFile(goldenCataloguePath)
	if err != nil {
		t.Fatalf("read %s: %v\nregenerate with: %s=1 go test ./internal/themes/ -run TestCatalogueGolden",
			goldenCataloguePath, err, updateGoldenEnv)
	}
	want := string(raw)
	if got == want {
		return
	}

	// Vacuity guard. An empty or whitespace-only golden file would
	// otherwise be "compared against" and report the entire catalogue as
	// added, which reads as a catastrophic drift rather than as the file
	// having been truncated. Say which it is.
	if strings.TrimSpace(want) == "" {
		t.Fatalf("%s is empty — the guard pins nothing; regenerate with: %s=1 go test ./internal/themes/ -run TestCatalogueGolden",
			goldenCataloguePath, updateGoldenEnv)
	}

	reportCatalogueDiff(t, want, got)
}

// reportCatalogueDiff turns two renderings of the catalogue into a
// per-key report: which keys appeared, which vanished, and for the keys
// present on both sides, which COLUMN moved and from what to what. This
// is the whole reason the guard stores lines rather than a digest.
func reportCatalogueDiff(t *testing.T, want, got string) {
	t.Helper()

	type row struct {
		section, kind, label, light, dark string
		order                             int
	}
	parse := func(s string) (map[string]row, []string) {
		rows := make(map[string]row)
		var order []string
		for i, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			f := strings.Split(line, "\t")
			if len(f) != 6 {
				t.Errorf("malformed catalogue line %d (%d fields, want 6): %q", i+1, len(f), line)
				continue
			}
			rows[f[1]] = row{section: f[0], kind: f[2], label: f[3], light: f[4], dark: f[5], order: i}
			order = append(order, f[1])
		}
		return rows, order
	}

	wantRows, wantOrder := parse(want)
	gotRows, gotOrder := parse(got)

	for _, k := range gotOrder {
		if _, ok := wantRows[k]; !ok {
			r := gotRows[k]
			t.Errorf("catalogue ADDED %q (section %q, light=%q dark=%q) — not in %s",
				k, r.section, r.light, r.dark, goldenCataloguePath)
		}
	}
	for _, k := range wantOrder {
		if _, ok := gotRows[k]; !ok {
			r := wantRows[k]
			t.Errorf("catalogue REMOVED %q (was section %q, light=%q dark=%q) — agents lose the role with no error anywhere",
				k, r.section, r.light, r.dark)
		}
	}
	for _, k := range wantOrder {
		g, ok := gotRows[k]
		if !ok {
			continue
		}
		w := wantRows[k]
		for _, c := range []struct{ name, want, got string }{
			{"section", w.section, g.section},
			{"kind", w.kind, g.kind},
			{"label", w.label, g.label},
			{"light", w.light, g.light},
			{"dark", w.dark, g.dark},
		} {
			if c.want != c.got {
				t.Errorf("catalogue %s.%s: %q → %q", k, c.name, c.want, c.got)
			}
		}
	}

	// Order drift with an unchanged membership produces none of the
	// above, and order is user-visible (the editor renders the catalogue
	// top-to-bottom in exactly this sequence).
	if len(wantOrder) == len(gotOrder) {
		for i := range wantOrder {
			if wantOrder[i] != gotOrder[i] {
				t.Errorf("catalogue order drift at position %d: %q → %q",
					i, wantOrder[i], gotOrder[i])
				break
			}
		}
	}

	t.Errorf("catalogue differs from %s (see the per-key report above); "+
		"if the change is deliberate regenerate with: %s=1 go test ./internal/themes/ -run TestCatalogueGolden",
		goldenCataloguePath, updateGoldenEnv)
}

// TestDefaultVariablesForBase verifies the helper returns one entry per
// token and picks the right base column.
func TestDefaultVariablesForBase(t *testing.T) {
	for _, base := range []string{"light", "dark"} {
		got := DefaultVariablesForBase(base)
		if len(got) != len(TokenSchemaKeys()) {
			t.Fatalf("base=%s: %d entries, want %d", base, len(got), len(TokenSchemaKeys()))
		}
		// Spot-check one well-known token. --n-50 is the top of the
		// neutral ramp and has distinct light/dark values, so it catches
		// a "light always returns dark" regression. It is also a literal
		// rather than a var() reference — most surface roles resolve
		// through the ramp, and a reference is the same string in both
		// bases for several of them.
		want := "#ffffff"
		if base == "dark" {
			want = "#141210"
		}
		if got["--n-50"] != want {
			t.Fatalf("base=%s: --n-50 = %q, want %q", base, got["--n-50"], want)
		}
	}
}

// TestDefaultVariablesForBase_UnknownBaseFallsBackToLight covers the
// pragmatic default: a typo or stale enum on the caller side should
// not return nil — the agent gets the light defaults instead.
func TestDefaultVariablesForBase_UnknownBaseFallsBackToLight(t *testing.T) {
	got := DefaultVariablesForBase("solarized") // not a valid base
	if len(got) != len(TokenSchemaKeys()) {
		t.Fatalf("unknown base: %d entries", len(got))
	}
	// Same as light.
	if got["--n-50"] != "#ffffff" {
		t.Fatalf("unknown base did not fall back to light defaults; --n-50 = %q", got["--n-50"])
	}
}
