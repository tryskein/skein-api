package tools

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/themes"
)

// callToolDirect runs the handler closure directly instead of going
// through the in-process MCP client. The theme handlers don't
// need the client roundtrip — bypassing it cuts the test setup boilerplate
// by ~30 lines per case.
func callToolDirect(ctx context.Context, t *testing.T, handler server.ToolHandlerFunc, args map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = args
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	return res
}

// firstTextContent extracts the first TextContent block from a tool
// result. Every theme tool returns ToolJSON / ToolError which produce
// exactly one text block.
func firstTextContent(t *testing.T, res *mcpgo.CallToolResult) string {
	t.Helper()
	if res == nil {
		t.Fatalf("nil result")
	}
	for _, c := range res.Content {
		if tc, ok := c.(mcpgo.TextContent); ok {
			return tc.Text
		}
	}
	t.Fatalf("no TextContent in result")
	return ""
}

// TestMCPThemeInstructions_ReturnsCanonicalGuide asserts the static
// markdown blob returned by skein_theme(action="instructions") contains the
// load-bearing anchors agents rely on (workflow steps, the names of
// the sibling tools it points at, the anti-patterns section, the
// editable-variables summary). The body is embedded at compile time
// so any drift between the registered tool and the .md file fails
// the build before this test runs — this test is the smoke check
// that the embed wired the right document.
func TestMCPThemeInstructions_ReturnsCanonicalGuide(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	handler := handleThemeInstructions()

	ctx := withCtx(t, userID, "user")
	res := callToolDirect(ctx, t, handler, map[string]any{})

	if res.IsError {
		t.Fatalf("unexpected error result: %s", firstTextContent(t, res))
	}
	body := firstTextContent(t, res)
	if len(body) < 500 {
		t.Fatalf("body length = %d, want >500 (embed likely empty)", len(body))
	}

	// Required substrings — these are the agent-load-bearing anchors.
	// If any are absent the canonical guide has been edited out of
	// shape and the agent's onboarding loop will silently degrade.
	mustContain := []string{
		"fork",
		"preview_variables",
		"set_variables",
		"inspect_clear",
		"Anti-patterns",
		"Step 1",
		"Step 2",
		"Step 3",
		"Editable variables",
	}
	for _, s := range mustContain {
		if !strings.Contains(body, s) {
			t.Errorf("body missing required substring %q", s)
		}
	}
}

// TestMCPThemeDescribe_ReturnsSchema asserts the static schema mirror
// and the user-pref defaults are surfaced correctly for a fresh user
// (mode=builtin, builtinTheme=light, no active theme, no selection).
func TestMCPThemeDescribe_ReturnsSchema(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	store := themes.NewStore(pool)
	cache := themes.NewInspectCache(0)
	handler := handleThemeDescribe(store, cache)

	ctx := withCtx(t, userID, "user")
	res := callToolDirect(ctx, t, handler, map[string]any{})

	if res.IsError {
		t.Fatalf("unexpected error result: %s", firstTextContent(t, res))
	}
	var body themeDescribeResponse
	if err := json.Unmarshal([]byte(firstTextContent(t, res)), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, want := len(body.Schema), len(themes.TokenSchema); got != want {
		t.Fatalf("schema length = %d, want %d", got, want)
	}

	// ── The catalogue on the wire is the NEW dictionary ───────────────
	//
	// Everything above this point compares the response against
	// themes.TokenSchema, which is the very value the handler serves —
	// true by construction, and it stays true while the catalogue says
	// anything at all. sk-285.7 replaced the role dictionary wholesale,
	// and "MCP hands agents the new one" was asserted by nothing: the
	// mirror shipped --radius-control at its pre-redesign 2px through
	// this exact payload with the suite green.
	//
	// So the expectations below are LITERALS, stated here independently
	// of the catalogue — a second voice, which is the only kind of
	// assertion that can disagree with the first.
	wantOnWire := map[string][2]string{ // key → {light, dark}
		// Ramp anchor: distinct per base, so it also proves the two
		// columns did not collapse into one.
		"--n-50": {"#ffffff", "#141210"},
		// The brand family the redesign introduced.
		"--primary": {"#8a2d2d", "#e49090"},
		// The two radii the mirror was actually stale on — 2px / 6px
		// against the registry's 6px / 12px. Forked themes write the
		// whole catalogue onto :root, so a stale value here visibly
		// un-rounds every control and panel.
		"--radius-control": {"6px", "6px"},
		"--radius-panel":   {"12px", "12px"},
		// Upper half of the spacing scale and a named height: absent
		// from the mirror entirely until this wave, which no count
		// assertion phrased as "the response has as many as the
		// catalogue" could ever have noticed.
		"--space-6":       {"24px", "24px"},
		"--height-header": {"56px", "56px"},
		// A reference default: the agent must receive `var(--ink)`
		// verbatim and resolve it, not a pre-resolved colour.
		"--on-mark": {"var(--ink)", "var(--ink)"},
		// Type scale base — 16px since the redesign, 13px before it.
		"--font-base": {"16px", "16px"},
	}
	onWire := map[string]themes.TokenDTO{}
	for _, sec := range body.Schema {
		for _, tok := range sec.Tokens {
			onWire[tok.Key] = tok
		}
	}
	for key, want := range wantOnWire {
		tok, ok := onWire[key]
		if !ok {
			t.Errorf("describe payload has no role %q — the new dictionary declares it "+
				"and an agent reading this response cannot reach what it does not list", key)
			continue
		}
		if tok.Light != want[0] || tok.Dark != want[1] {
			t.Errorf("describe payload %s = {light:%q dark:%q}, want {light:%q dark:%q}",
				key, tok.Light, tok.Dark, want[0], want[1])
		}
	}

	// And the pre-redesign names are gone from the wire. They have no
	// alias layer, so an agent that still sees one here would write it
	// to set_variables and change precisely nothing.
	for _, dead := range []string{
		"--bg-base", "--bg-elevated", "--fg-base", "--fg-low",
		"--brand", "--brand-fill", "--accent-cyan", "--accent-amber",
		"--space-md", "--radius-default", "--match-bg", "--font-size-body",
	} {
		if _, present := onWire[dead]; present {
			t.Errorf("describe payload still offers retired role %q — it resolves to "+
				"nothing, so an agent that writes it gets a silent no-op", dead)
		}
	}

	// Section identity and order, stated literally for the same reason.
	wantSectionIDs := []string{
		"neutrals", "surfaces", "text", "brand", "accents", "search", "geometry", "type",
	}
	for i, want := range wantSectionIDs {
		if i >= len(body.Schema) {
			t.Errorf("describe payload has no section[%d]; want %q", i, want)
			continue
		}
		if got := body.Schema[i].ID; got != want {
			t.Errorf("describe payload section[%d].id = %q, want %q", i, got, want)
		}
	}
	if body.BuiltinPref != "light" {
		t.Fatalf("builtinPref = %q, want light (default for fresh user)", body.BuiltinPref)
	}
	if body.ActiveTheme != nil {
		t.Fatalf("activeTheme = %+v, want nil", body.ActiveTheme)
	}
	if body.SelectedTargets == nil {
		t.Fatalf("selectedTargets = nil, want empty array (sk-10.6 wire contract)")
	}
	if len(body.SelectedTargets) != 0 {
		t.Fatalf("selectedTargets = %+v, want empty", body.SelectedTargets)
	}
}

// TestMCPThemeDescribe_PicksUpInspectSelection ensures a posted
// inspector snapshot rides through the cache into the describe payload.
func TestMCPThemeDescribe_PicksUpInspectSelection(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	store := themes.NewStore(pool)
	cache := themes.NewInspectCache(0)
	cache.Set(userID, themes.InspectSelection{
		Selector: ".task-card",
		// A REAL role from the current catalogue. `tokens[]` is
		// contractually "the variables that affect this element
		// according to the registry" (theme_instructions.md, Step 4),
		// so a retired name here describes a state the system cannot
		// produce. --elev is the successor of the --bg-elevated this
		// fixture used to carry.
		Tokens:        []string{"--elev"},
		ComputedStyle: map[string]any{"backgroundColor": "rgb(238,232,213)"},
	})
	handler := handleThemeDescribe(store, cache)

	ctx := withCtx(t, userID, "user")
	res := callToolDirect(ctx, t, handler, map[string]any{})

	if res.IsError {
		t.Fatalf("unexpected error: %s", firstTextContent(t, res))
	}
	var body themeDescribeResponse
	if err := json.Unmarshal([]byte(firstTextContent(t, res)), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.SelectedTargets) != 1 {
		t.Fatalf("len(selectedTargets) = %d, want 1", len(body.SelectedTargets))
	}
	if body.SelectedTargets[0].Selector != ".task-card" {
		t.Fatalf("selectedTargets[0].selector = %q", body.SelectedTargets[0].Selector)
	}
}

// TestMCPThemeDescribe_MultipleSelections posts three inspect snapshots
// for the same user and asserts describe surfaces all three in
// oldest-first order (sk-10.6 — cache is multi-entry now).
func TestMCPThemeDescribe_MultipleSelections(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	store := themes.NewStore(pool)
	cache := themes.NewInspectCache(0)
	cache.Set(userID, themes.InspectSelection{Selector: ".first"})
	cache.Set(userID, themes.InspectSelection{Selector: ".second"})
	cache.Set(userID, themes.InspectSelection{Selector: ".third"})
	handler := handleThemeDescribe(store, cache)

	ctx := withCtx(t, userID, "user")
	res := callToolDirect(ctx, t, handler, map[string]any{})

	if res.IsError {
		t.Fatalf("unexpected error: %s", firstTextContent(t, res))
	}
	var body themeDescribeResponse
	if err := json.Unmarshal([]byte(firstTextContent(t, res)), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.SelectedTargets) != 3 {
		t.Fatalf("len(selectedTargets) = %d, want 3", len(body.SelectedTargets))
	}
	want := []string{".first", ".second", ".third"}
	for i, w := range want {
		if body.SelectedTargets[i].Selector != w {
			t.Fatalf("selectedTargets[%d].selector = %q, want %q (oldest-first)",
				i, body.SelectedTargets[i].Selector, w)
		}
	}
}

// TestMCPThemeInspectClear_Empties seeds two selections, calls the
// clear tool, asserts cleared==2, and confirms a follow-up describe
// returns an empty list.
func TestMCPThemeInspectClear_Empties(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	store := themes.NewStore(pool)
	cache := themes.NewInspectCache(0)
	cache.Set(userID, themes.InspectSelection{Selector: ".a"})
	cache.Set(userID, themes.InspectSelection{Selector: ".b"})

	clearHandler := handleThemeInspectClear(cache)
	ctx := withCtx(t, userID, "user")

	res := callToolDirect(ctx, t, clearHandler, map[string]any{})
	if res.IsError {
		t.Fatalf("unexpected error: %s", firstTextContent(t, res))
	}
	var clearBody themeInspectClearResponse
	if err := json.Unmarshal([]byte(firstTextContent(t, res)), &clearBody); err != nil {
		t.Fatalf("unmarshal clear: %v", err)
	}
	if clearBody.Cleared != 2 {
		t.Fatalf("cleared = %d, want 2", clearBody.Cleared)
	}

	// Second call is idempotent → cleared:0.
	res = callToolDirect(ctx, t, clearHandler, map[string]any{})
	if err := json.Unmarshal([]byte(firstTextContent(t, res)), &clearBody); err != nil {
		t.Fatalf("unmarshal second clear: %v", err)
	}
	if clearBody.Cleared != 0 {
		t.Fatalf("second clear cleared = %d, want 0 (idempotent)", clearBody.Cleared)
	}

	// Describe must now report an empty list.
	describeHandler := handleThemeDescribe(store, cache)
	res = callToolDirect(ctx, t, describeHandler, map[string]any{})
	var describeBody themeDescribeResponse
	if err := json.Unmarshal([]byte(firstTextContent(t, res)), &describeBody); err != nil {
		t.Fatalf("unmarshal describe: %v", err)
	}
	if len(describeBody.SelectedTargets) != 0 {
		t.Fatalf("describe.selectedTargets = %+v, want empty",
			describeBody.SelectedTargets)
	}
}

// TestMCPThemeFork_Active forks the user's active theme (or the builtin
// when none is active) and verifies (1) a new theme row exists, (2) the
// user's pref now points at it, (3) a user-pref publish landed on the
// recorder.
func TestMCPThemeFork_Active(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	store := themes.NewStore(pool)
	pub := themes.NewRecordingPublisher()
	handler := handleThemeFork(store, pub)

	ctx := withCtx(t, userID, "user")
	name := "fork-active-" + uuid.NewString()[:8]
	res := callToolDirect(ctx, t, handler, map[string]any{
		"source": "active",
		"name":   name,
	})

	if res.IsError {
		t.Fatalf("unexpected error: %s", firstTextContent(t, res))
	}
	var body themeForkResponse
	if err := json.Unmarshal([]byte(firstTextContent(t, res)), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Theme == nil {
		t.Fatalf("theme = nil")
	}
	if body.Theme.Name != name {
		t.Fatalf("name = %q, want %q", body.Theme.Name, name)
	}
	// Fresh user = mode=builtin light → fork seeds with light defaults.
	if body.Theme.Base != "light" {
		t.Fatalf("base = %q, want light (fork of builtin-light)", body.Theme.Base)
	}
	// --n-50 is the top of the neutral ramp: a literal (not a var()
	// reference) with different values per base, so it proves the fork
	// seeded from the LIGHT column of the current catalogue. Was
	// --bg-base until sk-285.7 retired that role.
	if got := body.Theme.Variables["--n-50"]; got != "#ffffff" {
		t.Fatalf("--n-50 = %q, want #ffffff", got)
	}

	pref, err := store.GetUserThemePref(ctx, userID)
	if err != nil {
		t.Fatalf("GetUserThemePref: %v", err)
	}
	if pref.Mode != "custom" {
		t.Fatalf("mode = %q, want custom", pref.Mode)
	}
	if pref.CustomThemeID == nil || *pref.CustomThemeID != body.Theme.ID {
		t.Fatalf("customThemeId = %v, want %s", pref.CustomThemeID, body.Theme.ID)
	}

	// Recorder must have a user-pref event.
	if pub.Count() == 0 {
		t.Fatalf("publisher recorded 0 events; want at least one user-pref")
	}
	found := false
	for _, r := range pub.Records {
		if r.Kind == "user-pref" && r.UserID == userID {
			found = true
		}
	}
	if !found {
		t.Fatalf("no user-pref record for user %s; records=%+v", userID, pub.Records)
	}

	// Cleanup the row so the test pool doesn't leak themes between runs.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM themes WHERE id = $1`, body.Theme.ID)
	})
}

// TestMCPThemeFork_BuiltinDark forks the builtin-dark defaults
// directly (no active-theme path). Verifies the base column lands
// as "dark" and the variables match DefaultVariablesForBase("dark").
func TestMCPThemeFork_BuiltinDark(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	store := themes.NewStore(pool)
	pub := themes.NewRecordingPublisher()
	handler := handleThemeFork(store, pub)

	ctx := withCtx(t, userID, "user")
	name := "fork-dark-" + uuid.NewString()[:8]
	res := callToolDirect(ctx, t, handler, map[string]any{
		"source": "builtin-dark",
		"name":   name,
	})

	if res.IsError {
		t.Fatalf("unexpected error: %s", firstTextContent(t, res))
	}
	var body themeForkResponse
	_ = json.Unmarshal([]byte(firstTextContent(t, res)), &body)
	if body.Theme.Base != "dark" {
		t.Fatalf("base = %q, want dark", body.Theme.Base)
	}
	if body.Theme.Variables["--n-50"] != "#141210" {
		t.Fatalf("--n-50 = %q, want #141210 (dark)", body.Theme.Variables["--n-50"])
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM themes WHERE id = $1`, body.Theme.ID)
	})
}

// TestMCPThemeFork_RejectsBadSource confirms invalid source values
// surface validation_error without writing anything.
func TestMCPThemeFork_RejectsBadSource(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	store := themes.NewStore(pool)
	pub := themes.NewRecordingPublisher()
	handler := handleThemeFork(store, pub)

	ctx := withCtx(t, userID, "user")
	res := callToolDirect(ctx, t, handler, map[string]any{
		"source": "junk-source",
		"name":   "x",
	})
	if !res.IsError {
		t.Fatalf("expected error result for junk source")
	}
	body := firstTextContent(t, res)
	if !strings.Contains(body, "validation_error") {
		t.Fatalf("error body = %q, want validation_error", body)
	}
	if pub.Count() != 0 {
		t.Fatalf("publisher recorded %d events on bad source", pub.Count())
	}
}

// TestMCPThemeSetVariables_MergesAndPublishes covers the merge
// semantics (existing values preserved, new values overwrite) plus the
// theme.updated publish.
//
// The SEED deliberately still speaks the pre-redesign dictionary while
// the PATCH speaks the current one (sk-285.7.5). That is not an
// oversight left over from before the redesign: it pins the boundary of
// the new refusal. What the tool refuses is a dead name in the INCOMING
// patch; what is already in the row is none of its business — such a row
// is migrated by the frontend on read, and refusing to merge into it
// would strand a theme nobody can edit. The assertions below check both
// halves.
func TestMCPThemeSetVariables_MergesAndPublishes(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	store := themes.NewStore(pool)
	pub := themes.NewRecordingPublisher()

	// Seed a theme to update.
	seed, err := store.CreateTheme(context.Background(), themes.CreateThemeInput{
		Name: "merge-test-" + uuid.NewString()[:8],
		Base: "light",
		Variables: map[string]string{
			"--primary": "#cb4b16",
			"--ink":     "#000000",
			"--bg-base": "#ffffff", // pre-redesign name, already in the row
		},
		CreatedBy: userID,
	})
	if err != nil {
		t.Fatalf("seed theme: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM themes WHERE id = $1`, seed.ID)
	})

	handler := handleThemeSetVariables(store, pub)
	ctx := withCtx(t, userID, "user")
	res := callToolDirect(ctx, t, handler, map[string]any{
		"themeId": seed.ID.String(),
		"variables": map[string]any{
			"--primary": "#fefefe", // overwrite
			"--ink-2":   "#222222", // new key
		},
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", firstTextContent(t, res))
	}
	var body themeSetVariablesResponse
	if err := json.Unmarshal([]byte(firstTextContent(t, res)), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	merged := body.Theme.Variables
	if merged["--primary"] != "#fefefe" {
		t.Fatalf("--primary = %q, want overwrite #fefefe", merged["--primary"])
	}
	if merged["--ink"] != "#000000" {
		t.Fatalf("--ink = %q, want preserved #000000", merged["--ink"])
	}
	if merged["--ink-2"] != "#222222" {
		t.Fatalf("--ink-2 = %q, want new #222222", merged["--ink-2"])
	}
	// The row's own pre-redesign key survives the merge untouched: the
	// key check applies to the patch, not to the theme (sk-285.7.5).
	if merged["--bg-base"] != "#ffffff" {
		t.Fatalf("--bg-base = %q, want the row's own pre-redesign key preserved #ffffff",
			merged["--bg-base"])
	}

	// theme.updated publish must have happened with the full merged map.
	var updated *themes.PublishedRecord
	for i, r := range pub.Records {
		if r.Kind == "updated" && r.ThemeID == seed.ID {
			updated = &pub.Records[i]
			break
		}
	}
	if updated == nil {
		t.Fatalf("no theme.updated record; records=%+v", pub.Records)
	}
	if updated.Variables["--primary"] != "#fefefe" {
		t.Fatalf("publish payload --primary = %q", updated.Variables["--primary"])
	}
	if _, ok := updated.Variables["--ink"]; !ok {
		t.Fatalf("publish payload missing preserved --ink")
	}
}

// TestMCPThemeSetVariables_RejectsEmptyMap verifies the validation
// rule that an empty patch is rejected (would be a no-op merge).
func TestMCPThemeSetVariables_RejectsEmptyMap(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	store := themes.NewStore(pool)
	pub := themes.NewRecordingPublisher()

	// Seed.
	seed, err := store.CreateTheme(context.Background(), themes.CreateThemeInput{
		Name: "empty-patch-" + uuid.NewString()[:8],
		Base: "light",
		Variables: map[string]string{
			"--bg-base": "#fff",
		},
		CreatedBy: userID,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM themes WHERE id = $1`, seed.ID)
	})

	handler := handleThemeSetVariables(store, pub)
	ctx := withCtx(t, userID, "user")
	res := callToolDirect(ctx, t, handler, map[string]any{
		"themeId":   seed.ID.String(),
		"variables": map[string]any{}, // empty
	})
	if !res.IsError {
		t.Fatalf("expected error for empty variables")
	}
	if !strings.Contains(firstTextContent(t, res), "validation_error") {
		t.Fatalf("error = %q, want validation_error", firstTextContent(t, res))
	}
	if pub.Count() != 0 {
		t.Fatalf("publisher recorded %d events on empty patch", pub.Count())
	}
}

// TestMCPThemePreviewVariables_DoesNotPersist exercises the happy path
// — the publisher records the preview frame, and no DB row was written
// (verified by counting themes for the test user before and after).
func TestMCPThemePreviewVariables_DoesNotPersist(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pub := themes.NewRecordingPublisher()
	handler := handleThemePreviewVariables(pub)

	ctx := withCtx(t, userID, "user")

	var before int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM themes WHERE created_by = $1`, userID).Scan(&before)

	res := callToolDirect(ctx, t, handler, map[string]any{
		"variables": map[string]any{
			"--primary": "#0080ff",
		},
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", firstTextContent(t, res))
	}

	var body themePreviewResponse
	if err := json.Unmarshal([]byte(firstTextContent(t, res)), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !body.OK {
		t.Fatalf("response.ok = false")
	}

	var after int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM themes WHERE created_by = $1`, userID).Scan(&after)
	if after != before {
		t.Fatalf("themes row count changed: %d → %d (preview must not write)", before, after)
	}

	if pub.Count() != 1 {
		t.Fatalf("publisher recorded %d events, want 1", pub.Count())
	}
	if pub.Records[0].Kind != "preview" {
		t.Fatalf("record.Kind = %q, want preview", pub.Records[0].Kind)
	}
	if pub.Records[0].UserID != userID {
		t.Fatalf("record.UserID = %s, want %s", pub.Records[0].UserID, userID)
	}
	if pub.Records[0].Variables["--primary"] != "#0080ff" {
		t.Fatalf("record.Variables[--primary] = %q", pub.Records[0].Variables["--primary"])
	}
}

// TestMCPThemePreviewVariables_NATSDisabledReturnsCleanError covers the
// degraded case — no NATS connection means the noopPublisher returns
// ErrNATSDisabled which the tool maps to preview_unavailable. The agent
// can branch on this code to fall back to a non-live workflow.
func TestMCPThemePreviewVariables_NATSDisabledReturnsCleanError(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pub := themes.NewPublisher(nil) // noop publisher
	handler := handleThemePreviewVariables(pub)

	ctx := withCtx(t, userID, "user")
	res := callToolDirect(ctx, t, handler, map[string]any{
		"variables": map[string]any{"--bg-base": "#000"},
	})
	if !res.IsError {
		t.Fatalf("expected error result with disabled NATS")
	}
	body := firstTextContent(t, res)
	if !strings.Contains(body, ErrCodePreviewUnavailable) {
		t.Fatalf("error = %q, want %s", body, ErrCodePreviewUnavailable)
	}
}

/* ═══ sk-285.7.4 / sk-285.7.5 — role names outside the dictionary ═══ */

// themeRoleErrorBody is the refusal envelope set_variables returns for a
// patch that names roles the current dictionary does not declare.
type themeRoleErrorBody struct {
	Error   string             `json:"error"`
	Message string             `json:"message"`
	Roles   []themes.RoleIssue `json:"roles"`
}

// seedCurrentDictionaryTheme creates a theme whose variables ARE the
// current dictionary — i.e. a row the frontend's one-time migration has
// nothing left to do to. Every write test below starts from this state,
// because the property under test is that MCP cannot move a row OUT of
// it.
func seedCurrentDictionaryTheme(t *testing.T, pool *pgxpool.Pool, store *themes.Store, userID uuid.UUID) *themes.ThemeDTO {
	t.Helper()
	seed, err := store.CreateTheme(context.Background(), themes.CreateThemeInput{
		Name:      "roles-" + uuid.NewString()[:8],
		Base:      "light",
		Variables: themes.DefaultVariablesForBase("light"),
		CreatedBy: userID,
	})
	if err != nil {
		t.Fatalf("seed theme: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM themes WHERE id = $1`, seed.ID)
	})
	return seed
}

// TestMCPThemeSetVariables_RefusesRetiredRole is the write half of
// sk-285.7.5, and it is deliberately stated as a claim about the ROW
// rather than about the response.
//
// ── The scenario, and the half of it that lives here ────────────────
//
// The whole failure was: an agent writes a pre-redesign name → the row
// now holds a name only the old dictionary had → the frontend's
// `needsMigration` says yes → the owner's next page load replaces the
// whole map with a migrated snapshot and the agent's key is gone, having
// painted nothing in between. Two writers, one row, and the agent lost
// silently every time.
//
// The two halves of that chain live in two repositories, so this test
// makes the half this one can make, and makes it in the frontend's own
// terms: after the refusal the row still contains ONLY names the current
// dictionary declares, which is exactly the first clause of
// `needsMigration`. The second half — that such a row is not rewritten,
// and that a row WITH such a key loses it — is pinned in skein-frontend
// (`useTheme.writeback.test.ts` / the `needsMigration` suite).
//
// The refusal is also asserted to be TOTAL: `updated_at` must not move
// and no `theme.updated` may go out, because a refusal that still
// touched the row would be a third writer rather than a fix.
func TestMCPThemeSetVariables_RefusesRetiredRole(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	store := themes.NewStore(pool)
	pub := themes.NewRecordingPublisher()
	seed := seedCurrentDictionaryTheme(t, pool, store, userID)

	handler := handleThemeSetVariables(store, pub)
	ctx := withCtx(t, userID, "user")
	res := callToolDirect(ctx, t, handler, map[string]any{
		"themeId": seed.ID.String(),
		"variables": map[string]any{
			"--bg-hover": "rgba(0, 90, 255, 0.08)",
		},
	})

	if !res.IsError {
		t.Fatalf("expected a refusal for a retired role name, got: %s", firstTextContent(t, res))
	}
	var body themeRoleErrorBody
	if err := json.Unmarshal([]byte(firstTextContent(t, res)), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Error != ErrCodeUnknownThemeRole {
		t.Fatalf("error = %q, want %q", body.Error, ErrCodeUnknownThemeRole)
	}
	want := []themes.RoleIssue{{
		Key:       "--bg-hover",
		Status:    themes.RoleRetired,
		Successor: "--elev",
	}}
	if !reflect.DeepEqual(body.Roles, want) {
		t.Fatalf("roles = %+v, want %+v", body.Roles, want)
	}
	// The successor has to reach a reader who only ever sees the prose.
	if !strings.Contains(body.Message, "--elev") {
		t.Errorf("message = %q, want it to name the successor --elev", body.Message)
	}

	// Nothing was written, and nothing was announced.
	if pub.Count() != 0 {
		t.Fatalf("publisher recorded %d events on a refused patch", pub.Count())
	}
	after, err := store.GetTheme(ctx, seed.ID, userID)
	if err != nil {
		t.Fatalf("re-read theme: %v", err)
	}
	if _, present := after.Variables["--bg-hover"]; present {
		t.Fatalf("the refused key reached the row anyway")
	}
	if !after.UpdatedAt.Equal(seed.UpdatedAt) {
		t.Errorf("updated_at moved on a refused patch: %s → %s", seed.UpdatedAt, after.UpdatedAt)
	}
	// The frontend's needsMigration, restated in this repo's terms: the
	// row holds no name outside the current dictionary, so the owner's
	// next load has nothing to rewrite and nothing to lose.
	for key := range after.Variables {
		if !themes.IsCurrentRole(key) {
			t.Errorf("row now carries %q, which no longer exists — the owner's next page "+
				"load will rewrite the map and drop it silently", key)
		}
	}
}

// TestMCPThemeSetVariables_RefusalIsWholePatch pins that ONE dead name
// costs the caller the whole map.
//
// It is the property most easily lost to a well-meant refactor ("just
// skip the bad keys and write the good ones"), and partial application
// would be the worst of the three options: the agent gets an error, some
// of its intent lands anyway, and it cannot tell which half without
// re-reading. All-or-nothing is also what makes the refusal safe to
// retry verbatim after renaming the offending key.
func TestMCPThemeSetVariables_RefusalIsWholePatch(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	store := themes.NewStore(pool)
	pub := themes.NewRecordingPublisher()
	seed := seedCurrentDictionaryTheme(t, pool, store, userID)
	before := seed.Variables["--primary"]

	handler := handleThemeSetVariables(store, pub)
	ctx := withCtx(t, userID, "user")
	res := callToolDirect(ctx, t, handler, map[string]any{
		"themeId": seed.ID.String(),
		"variables": map[string]any{
			"--primary":  "#0080ff", // live role, would be fine on its own
			"--zzz-nope": "12px",    // from no dictionary at all
		},
	})
	if !res.IsError {
		t.Fatalf("expected a refusal, got: %s", firstTextContent(t, res))
	}
	var body themeRoleErrorBody
	if err := json.Unmarshal([]byte(firstTextContent(t, res)), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Only the offending key is reported — a refusal that listed the good
	// key too would send the agent renaming something that was correct.
	want := []themes.RoleIssue{{Key: "--zzz-nope", Status: themes.RoleUnknown}}
	if !reflect.DeepEqual(body.Roles, want) {
		t.Fatalf("roles = %+v, want %+v", body.Roles, want)
	}
	if body.Roles[0].Successor != "" {
		t.Errorf("successor = %q, want empty — an unknown name has no predecessor to "+
			"have a successor of, and offering one would invent a migration", body.Roles[0].Successor)
	}

	after, err := store.GetTheme(ctx, seed.ID, userID)
	if err != nil {
		t.Fatalf("re-read theme: %v", err)
	}
	if after.Variables["--primary"] != before {
		t.Fatalf("--primary = %q, want the untouched %q — the live half of a refused "+
			"patch must not land either", after.Variables["--primary"], before)
	}
	if pub.Count() != 0 {
		t.Fatalf("publisher recorded %d events on a refused patch", pub.Count())
	}
}

// TestMCPThemeSetVariables_AcceptsCurrentRoles is the non-degeneracy
// guard for the two tests above: a build that refused EVERY patch would
// satisfy both of them and break the tool completely.
func TestMCPThemeSetVariables_AcceptsCurrentRoles(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	store := themes.NewStore(pool)
	pub := themes.NewRecordingPublisher()
	seed := seedCurrentDictionaryTheme(t, pool, store, userID)

	handler := handleThemeSetVariables(store, pub)
	ctx := withCtx(t, userID, "user")
	res := callToolDirect(ctx, t, handler, map[string]any{
		"themeId": seed.ID.String(),
		"variables": map[string]any{
			"--primary": "#0080ff",
			"--space-4": "20px",
		},
	})
	if res.IsError {
		t.Fatalf("a patch of live roles was refused: %s", firstTextContent(t, res))
	}
	var body themeSetVariablesResponse
	if err := json.Unmarshal([]byte(firstTextContent(t, res)), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Theme.Variables["--primary"] != "#0080ff" {
		t.Fatalf("--primary = %q, want #0080ff", body.Theme.Variables["--primary"])
	}
	if body.Theme.Variables["--space-4"] != "20px" {
		t.Fatalf("--space-4 = %q, want 20px", body.Theme.Variables["--space-4"])
	}
	if pub.Count() != 1 {
		t.Fatalf("publisher recorded %d events, want 1", pub.Count())
	}
}

// TestMCPThemePreviewVariables_WarnsWithoutChangingSemantics is the
// preview half (sk-285.7.4). The decision recorded on that issue is that
// a preview is NOT migrated: it carries the agent's explicit map and
// substituting successors would rewrite the command. So the assertion
// has two legs that must hold together — the published frame is byte-for
// byte what was sent, AND the response says out loud which keys will
// paint nothing.
//
// Dropping the offending keys instead would satisfy the second leg and
// fail the first; migrating them would satisfy neither. Only the pair
// distinguishes the intended behaviour from both alternatives.
func TestMCPThemePreviewVariables_WarnsWithoutChangingSemantics(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pub := themes.NewRecordingPublisher()
	handler := handleThemePreviewVariables(pub)
	ctx := withCtx(t, userID, "user")

	sent := map[string]any{
		"--primary":        "#0080ff",            // live
		"--bg-base":        "#ffffff",            // retired, has a successor
		"--font-size-body": "14px",               // retired, no successor
		"--zzz-nope":       "rgba(1, 2, 3, 0.5)", // unknown
	}
	res := callToolDirect(ctx, t, handler, map[string]any{"variables": sent})
	if res.IsError {
		t.Fatalf("preview must not refuse — it applies what it is given: %s",
			firstTextContent(t, res))
	}
	var body themePreviewResponse
	if err := json.Unmarshal([]byte(firstTextContent(t, res)), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !body.OK {
		t.Fatalf("response.ok = false")
	}
	want := []themes.RoleIssue{
		{Key: "--bg-base", Status: themes.RoleRetired, Successor: "--bg"},
		{Key: "--font-size-body", Status: themes.RoleRetired},
		{Key: "--zzz-nope", Status: themes.RoleUnknown},
	}
	if !reflect.DeepEqual(body.Roles, want) {
		t.Fatalf("roles = %+v, want %+v", body.Roles, want)
	}
	if body.Warning == "" {
		t.Fatalf("roles[] is populated but warning is empty — the prose half of the " +
			"answer is what a human reads in a transcript")
	}
	if !strings.Contains(body.Warning, "--bg") {
		t.Errorf("warning = %q, want it to name the successor --bg", body.Warning)
	}

	// Leg one: the frame on the wire is exactly the map that was sent —
	// nothing migrated, nothing dropped.
	if pub.Count() != 1 {
		t.Fatalf("publisher recorded %d events, want 1", pub.Count())
	}
	got := pub.Records[0].Variables
	if len(got) != len(sent) {
		t.Fatalf("published %d keys, want all %d sent", len(got), len(sent))
	}
	for k, v := range sent {
		if got[k] != v.(string) {
			t.Errorf("published %s = %q, want the value as sent %q", k, got[k], v)
		}
	}
}

// TestMCPThemePreviewVariables_CleanMapSaysNothing is the
// non-degeneracy guard the sk-285.7.4 acceptance asks for by name: with
// no offending keys the check must not be "passed automatically", it
// must produce the ORIGINAL response and no more.
//
// Asserted on the raw JSON rather than on the decoded struct, because
// the struct cannot tell an absent key from a zero value — and `omitempty`
// on both new fields is the whole reason a clean call's answer is
// unchanged for every existing consumer.
func TestMCPThemePreviewVariables_CleanMapSaysNothing(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pub := themes.NewRecordingPublisher()
	handler := handleThemePreviewVariables(pub)
	ctx := withCtx(t, userID, "user")

	res := callToolDirect(ctx, t, handler, map[string]any{
		"variables": map[string]any{"--primary": "#0080ff", "--ink": "#111111"},
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", firstTextContent(t, res))
	}
	if got := firstTextContent(t, res); got != `{"ok":true}` {
		t.Fatalf("clean preview answered %s, want exactly {\"ok\":true} — a warning on a "+
			"map with nothing wrong with it trains the agent to ignore warnings", got)
	}
}

// TestMCPTheme_ForeignThemeIsNonexistent — sk-674.18, row 148b (T035,
// US-21). Reference — US-21 as the spec words it, written before the code:
// the MCP theme actions that name a theme (fork's source, set_variables'
// themeId) answer «not found» on another author's theme exactly as on a
// nonexistent id — nothing is written and nothing is published; on the
// caller's own theme they work. preview_variables names no theme: it goes to
// the caller's own sessions only, and that is what is checked for it.
//
// Red if a foreign theme can be forked or written, if any of it is
// published, or if its refusal differs from a nonexistent id's.
func TestMCPTheme_ForeignThemeIsNonexistent(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := themes.NewStore(pool)
	me, _ := seedUser(t, pool, "user")
	other, _ := seedUser(t, pool, "user")
	mine := seedCurrentDictionaryTheme(t, pool, store, me)
	theirs := seedCurrentDictionaryTheme(t, pool, store, other)
	missing := uuid.NewString()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `UPDATE users SET custom_theme_id = NULL WHERE id = $1`, me)
		_, _ = pool.Exec(context.Background(), `DELETE FROM themes WHERE created_by = $1`, me)
	})
	call := func(h server.ToolHandlerFunc, args map[string]any) (map[string]any, bool) {
		t.Helper()
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = args
		res, err := h(withCtx(t, me, "user"), req)
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		return extractToolJSON(t, res), res.IsError
	}
	countMine := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM themes WHERE author_id = $1`, me).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// A key of the CURRENT dictionary: a retired one is refused before the
	// theme is even looked up, and the comparison would prove nothing.
	var role string
	for k := range themes.DefaultVariablesForBase("light") {
		role = k
		break
	}
	patch := map[string]any{role: "#101010"}

	t.Run("fork", func(t *testing.T) {
		pub := themes.NewRecordingPublisher()
		h := handleThemeFork(store, pub)
		before := countMine()
		got, gotErr := call(h, map[string]any{"source": theirs.ID.String(), "name": "fork-" + uuid.NewString()[:8]})
		want, wantErr := call(h, map[string]any{"source": missing, "name": "fork-" + uuid.NewString()[:8]})
		if !gotErr || gotErr != wantErr || got["error"] != want["error"] || got["message"] != want["message"] {
			t.Errorf("fork of a foreign theme: %v (error %v), of a nonexistent one: %v (error %v)", got, gotErr, want, wantErr)
		}
		if countMine() != before || pub.Count() != 0 {
			t.Errorf("a refused fork wrote %d theme(s) or published %d event(s)", countMine()-before, pub.Count())
		}
		if _, isErr := call(h, map[string]any{"source": mine.ID.String(), "name": "fork-" + uuid.NewString()[:8]}); isErr {
			t.Errorf("forking one's own theme was refused (positive control)")
		}
	})
	t.Run("set_variables", func(t *testing.T) {
		pub := themes.NewRecordingPublisher()
		h := handleThemeSetVariables(store, pub)
		got, gotErr := call(h, map[string]any{"themeId": theirs.ID.String(), "variables": patch})
		want, wantErr := call(h, map[string]any{"themeId": missing, "variables": patch})
		if !gotErr || gotErr != wantErr || got["error"] != want["error"] || got["message"] != want["message"] {
			t.Errorf("set_variables on a foreign theme: %v (error %v), on a nonexistent one: %v (error %v)", got, gotErr, want, wantErr)
		}
		after, err := store.GetTheme(ctx, theirs.ID, other)
		if err != nil {
			t.Fatal(err)
		}
		if after.Variables[role] == "#101010" || pub.Count() != 0 {
			t.Errorf("a refused set_variables wrote the foreign theme or published %d event(s)", pub.Count())
		}
		if _, isErr := call(h, map[string]any{"themeId": mine.ID.String(), "variables": patch}); isErr {
			t.Errorf("set_variables on one's own theme was refused (positive control)")
		}
		if pub.Count() != 1 {
			t.Errorf("one's own write published %d event(s), want 1", pub.Count())
		}
	})
	t.Run("preview_variables goes to the caller only", func(t *testing.T) {
		pub := themes.NewRecordingPublisher()
		if _, isErr := call(handleThemePreviewVariables(pub), map[string]any{"variables": patch}); isErr {
			t.Fatalf("preview was refused")
		}
		for _, r := range pub.Records {
			if r.UserID != me {
				t.Errorf("a preview was published to %s, not to the caller", r.UserID)
			}
		}
		if pub.Count() != 1 {
			t.Errorf("preview published %d event(s), want 1", pub.Count())
		}
	})
}
