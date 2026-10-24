package dependencies

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"skein-api/internal/search"
)

const ollamaURLEnv = "OLLAMA_URL"

// newRealEmbedder builds a *search.Embedder pointed at the real Ollama
// instance configured via OLLAMA_URL.  Per R4.1 plan Q-4 we explicitly
// do NOT use a stub HTTP server — cross-lingual / semantic dup tests
// have to exercise a real model or they prove nothing.
//
// Returns a usable QueryEmbedder. Skips the test (not Fatal) when
// OLLAMA_URL is unset so `go test ./...` stays useful in offline dev,
// while CI / on-demand runs that target the real server still execute
// the assertions.
func newRealEmbedder(t *testing.T) QueryEmbedder {
	t.Helper()
	url := os.Getenv(ollamaURLEnv)
	if url == "" {
		t.Skipf("set %s to run real-Ollama embedder tests", ollamaURLEnv)
	}
	e, err := search.NewEmbedder(search.EmbedConfig{
		BaseURL: url,
		Model:   "qwen3-embedding:0.6b",
		Dim:     1024,
		// Tighter cache than production so the four subtests in this
		// file don't share warm vectors when one mutates ground truth.
		CacheSize:     32,
		CacheTTL:      30 * time.Second,
		EmbedTimeout:  60 * time.Second,
		SearchTimeout: 15 * time.Second, // generous for cold starts
	})
	if err != nil {
		t.Fatalf("build real embedder: %v", err)
	}
	// Probe the upstream once so a misconfigured OLLAMA_URL fails fast
	// with a useful message instead of producing weird timing-flaky
	// failures inside the actual test bodies.
	probeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := e.EmbedSingle(probeCtx, "ping"); err != nil {
		t.Fatalf("probe %s with model qwen3-embedding:0.6b: %v", url, err)
	}
	return e
}

// seedIssueWithDescription mirrors seedOpenIssue but lets the test set
// a description (the layer-2 vector path embeds title + description).
// Cleanup is handled at the project-cascade level via the existing
// seedProject t.Cleanup hook.
func seedIssueWithDescription(t *testing.T, pool *pgxpool.Pool, projectID, creator uuid.UUID, idStr, title, description string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at, metadata)
VALUES ($1, $2, NULL, 1, $3, $4, 'task', 2, 'open', false, 'human', NULL, NULL,
        '', '', '', NULL, NULL, NULL,
        NULL, NULL, NULL,
        NULL, $5, now(), now(), '{}'::jsonb)`,
		idStr, projectID, title, description, creator)
	if err != nil {
		t.Fatalf("seed issue %s: %v", idStr, err)
	}
}

// embedIssueInline writes the embedding directly via the real embedder,
// bypassing the backfill worker so tests run deterministically without
// an extra background goroutine. Uses search.BuildIssueEmbedText so the
// embedded text matches what the worker would have produced.
func embedIssueInline(t *testing.T, pool *pgxpool.Pool, e QueryEmbedder, issueID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var title, description, acceptance string
	err := pool.QueryRow(ctx,
		`SELECT title, description, acceptance_criteria FROM issues WHERE id = $1`,
		issueID).Scan(&title, &description, &acceptance)
	if err != nil {
		t.Fatalf("load issue %s for embed: %v", issueID, err)
	}

	text, _ := search.BuildIssueEmbedText(title, description, acceptance)
	vec, err := e.EmbedSingle(ctx, text)
	if err != nil {
		t.Fatalf("embed issue %s: %v", issueID, err)
	}

	// pgvector-go's HalfVector encodes itself for the halfvec column
	// type via the binary protocol — same path the production worker
	// uses, so the test stays faithful to the runtime behaviour.
	if _, err := pool.Exec(ctx,
		`UPDATE issues SET embedding = $1 WHERE id = $2`,
		pgvector.NewHalfVector(vec), issueID); err != nil {
		t.Fatalf("write embedding for %s: %v", issueID, err)
	}
}

// TestCheckDuplicates_TrigramOnly_NoEmbedder verifies the original
// trigram-only path still returns near-identical titles when no embedder
// is wired in (graceful degrade: layer-2 disabled).
func TestCheckDuplicates_TrigramOnly_NoEmbedder(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	seedIssueWithDescription(t, pool, pid, owner, prefix+"-1",
		"Add CSV export to reports", "")

	store := NewStore(pool) // nil embedder → trigram-only
	report, err := store.CheckDuplicates(context.Background(), pid,
		"Add CSV export to reports", "")
	if err != nil {
		t.Fatalf("CheckDuplicates: %v", err)
	}
	hits := report.Candidates
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit (exact title match), got %d", len(hits))
	}
	if hits[0].ID != prefix+"-1" {
		t.Fatalf("expected hit id %s, got %s", prefix+"-1", hits[0].ID)
	}
	if hits[0].Similarity < duplicateStrongMatch {
		t.Fatalf("trigram similarity %.3f < %.2f for exact title",
			hits[0].Similarity, duplicateStrongMatch)
	}
	if hits[0].SemanticSimilarity != nil {
		t.Fatalf("SemanticSimilarity should be nil with no embedder, got %v",
			*hits[0].SemanticSimilarity)
	}
	// sk-228: the title here is an EXACT match, so layer 1 returns a
	// legible strong hit and layer 2 is skipped by design, not by
	// failure. That is a confident answer and must not be flagged
	// degraded — see TestCheckDuplicates_EmbedderUnavailable_GracefulDegrade
	// for the case where layer 2 was actually wanted and missing.
	if report.Degraded {
		t.Fatalf("Degraded must stay false on a legible strong trigram hit; reason was %q",
			report.DegradedReason)
	}
}

// TestCheckDuplicates_LayerOneStrongMatch_SkipsEmbed verifies the Q-2
// fast-path: when trigram already crosses duplicateStrongMatch we
// short-circuit and never call the embedder. We assert this by
// installing a panicking embedder — if it gets called the test fails
// loudly instead of silently regressing the optimisation.
func TestCheckDuplicates_LayerOneStrongMatch_SkipsEmbed(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	seedIssueWithDescription(t, pool, pid, owner, prefix+"-1",
		"Implement CSV export", "")

	store := NewStoreWithEmbedder(pool, &panicOnUseEmbedder{t: t})
	report, err := store.CheckDuplicates(context.Background(), pid,
		"Implement CSV export", "ignored body")
	if err != nil {
		t.Fatalf("CheckDuplicates: %v", err)
	}
	hits := report.Candidates
	if len(hits) == 0 {
		t.Fatalf("expected at least one hit on identical title")
	}
	if hits[0].SemanticSimilarity != nil {
		t.Fatalf("SemanticSimilarity should be nil when layer-1 short-circuits, got %v",
			*hits[0].SemanticSimilarity)
	}
	// sk-228: skipping layer 2 after a LEGIBLE strong trigram hit is a
	// confident answer, not a degraded one. Conflating the two would make
	// `degraded` fire on the commonest happy path and train callers to
	// ignore it.
	if report.Degraded {
		t.Fatalf("Degraded must stay false on the layer-1 short-circuit; reason was %q",
			report.DegradedReason)
	}
}

// TestCheckDuplicates_EmbedderUnavailable_GracefulDegrade verifies that
// a vec-layer failure does NOT propagate to the caller — we log a warn
// and return the trigram results unchanged.
func TestCheckDuplicates_EmbedderUnavailable_GracefulDegrade(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	// The query title is a one-character typo of the seeded one:
	// pg_trgm scores the pair at 0.8333 — over duplicateMinReport (0.80)
	// so the row is reportable, but under duplicateStrongMatch (0.85) so
	// layer 1 does NOT short-circuit and the embedder really is called.
	// It used to be an exact title, which scored 1.0 and short-circuited,
	// meaning this test never once reached the code path it is named
	// after (caught by sk-228's Degraded assertion).
	seedIssueWithDescription(t, pool, pid, owner, prefix+"-1",
		"Add reports CSV export feature", "")

	store := NewStoreWithEmbedder(pool, &erroringEmbedder{
		err: errors.New("ollama unreachable"),
	})
	report, err := store.CheckDuplicates(context.Background(), pid,
		"Add reports CSV export featire", "long description that would invite layer 2")
	if err != nil {
		t.Fatalf("CheckDuplicates should NOT propagate embedder error, got: %v", err)
	}
	hits := report.Candidates
	// Trigram still finds the row (a near-identical title: one-character
	// typo, scored under duplicateStrongMatch so layer 1 does not
	// short-circuit); the embedder failure is absorbed and
	// SemanticSimilarity stays nil.
	if len(hits) == 0 {
		t.Fatalf("expected trigram fallback to surface a hit")
	}
	for _, h := range hits {
		if h.SemanticSimilarity != nil {
			t.Fatalf("SemanticSimilarity should stay nil on embedder failure, got %v on %s",
				*h.SemanticSimilarity, h.ID)
		}
	}
	// sk-228: absorbing the error is right; hiding it is not.
	if !report.Degraded {
		t.Fatal("expected Degraded=true after the embedder call failed")
	}
	if report.DegradedReason == "" {
		t.Fatal("Degraded=true with an empty DegradedReason")
	}
}

// TestCheckDuplicates_TwoLayer_SemanticMatch verifies the layer-2
// vector path actually fires when titles diverge but meaning matches —
// this is the whole point of R4.1.
func TestCheckDuplicates_TwoLayer_SemanticMatch(t *testing.T) {
	pool := newTestPool(t)
	embedder := newRealEmbedder(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	// Title 1 ("Add CSV export to reports") vs query
	// ("Implement exporting reports as CSV") — different surface form,
	// same meaning. Trigram alone scores well below 0.85; layer-2
	// vec should surface this.
	const id1 = "1"
	seedIssueWithDescription(t, pool, pid, owner, prefix+"-"+id1,
		"Add CSV export to reports",
		"Users need to download report data as a CSV file.")
	embedIssueInline(t, pool, embedder, prefix+"-"+id1)

	// Decoy issue, unrelated content; should NOT be the top hit.
	seedIssueWithDescription(t, pool, pid, owner, prefix+"-2",
		"Refactor billing webhook signature handler", "")
	embedIssueInline(t, pool, embedder, prefix+"-2")

	store := NewStoreWithEmbedder(pool, embedder)
	report, err := store.CheckDuplicates(context.Background(), pid,
		"Implement exporting reports as CSV",
		"Need a way to download reports as CSV files for every team.")
	if err != nil {
		t.Fatalf("CheckDuplicates: %v", err)
	}
	hits := report.Candidates
	if len(hits) == 0 {
		t.Fatalf("expected at least one hit via layer-2 vec")
	}
	if hits[0].ID != prefix+"-"+id1 {
		t.Fatalf("expected top hit %s, got %s (full hits: %+v)",
			prefix+"-"+id1, hits[0].ID, hits)
	}
	if hits[0].SemanticSimilarity == nil {
		t.Fatalf("expected SemanticSimilarity populated when vec ran, got nil")
	}
	if *hits[0].SemanticSimilarity < 0.7 {
		t.Fatalf("semantic similarity %.3f below 0.7 — layer-2 not firing for clear semantic match",
			*hits[0].SemanticSimilarity)
	}
}

// TestCheckDuplicates_CrossLingual verifies qwen3-embedding's claimed
// multilingual capability — an English-titled issue matched by a
// Russian query through the vector layer alone (trigram won't help
// across alphabets).
func TestCheckDuplicates_CrossLingual(t *testing.T) {
	pool := newTestPool(t)
	embedder := newRealEmbedder(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	seedIssueWithDescription(t, pool, pid, owner, prefix+"-1",
		"User authentication via OAuth",
		"Add OAuth 2.0 sign-in flow with Google and GitHub providers.")
	embedIssueInline(t, pool, embedder, prefix+"-1")

	store := NewStoreWithEmbedder(pool, embedder)
	report, err := store.CheckDuplicates(context.Background(), pid,
		"Аутентификация пользователя через OAuth",
		"Реализовать OAuth 2.0 вход через Google и GitHub.")
	if err != nil {
		t.Fatalf("CheckDuplicates: %v", err)
	}
	hits := report.Candidates
	if len(hits) == 0 {
		t.Fatalf("expected cross-lingual match via embedder, got zero hits")
	}
	if hits[0].ID != prefix+"-1" {
		t.Fatalf("expected top hit %s, got %s (all hits: %+v)",
			prefix+"-1", hits[0].ID, hits)
	}
	if hits[0].SemanticSimilarity == nil {
		t.Fatalf("expected SemanticSimilarity populated for cross-lingual match")
	}
}

// ---- Test embedder doubles ----

// panicOnUseEmbedder is wired into the layer-1-fast-path test. Any
// EmbedSingle call means the short-circuit didn't fire and the test
// should fail loudly.
type panicOnUseEmbedder struct {
	t *testing.T
}

func (p *panicOnUseEmbedder) EmbedSingle(_ context.Context, _ string) ([]float32, error) {
	p.t.Fatalf("EmbedSingle called — layer-1 short-circuit should have prevented this")
	return nil, nil
}

func (p *panicOnUseEmbedder) Available() bool { return true }

// erroringEmbedder simulates Ollama being unreachable. Used to verify
// graceful degradation: CheckDuplicates must absorb the error and still
// return trigram results.
type erroringEmbedder struct {
	err error
}

func (e *erroringEmbedder) EmbedSingle(_ context.Context, _ string) ([]float32, error) {
	return nil, e.err
}

func (e *erroringEmbedder) Available() bool { return true }

// ---------------------------------------------------------------------
// sk-228 — the trigram layer must not report a verdict it did not reach
// ---------------------------------------------------------------------

// residueTitleA and residueTitleB are two titles a human reads as
// obviously different that pg_trgm scores at EXACTLY 1.0, because after
// its ISWORDCHR filter both are the single word "Outbox".
//
// They are built from dashes and punctuation rather than Cyrillic on
// purpose. The production incident (sk-228) was Cyrillic collapsing on a
// ctype=C cluster, but the CI database runs ctype=en_US.utf8, where
// Cyrillic survives — a Russian fixture would be green there for the
// wrong reason and would never fail if the guard were removed. Dashes
// and brackets are non-alphanumeric under EVERY ctype, so this pair
// reproduces the mechanism identically on both cluster flavours.
const (
	residueTitleA = "—— Outbox ——————————————————————"
	residueTitleB = "«»(),.;: Outbox !!! ??? /// +++"
)

// rawTrigramSimilarity asks the database for the unguarded pg_trgm score
// — the number CheckDuplicates used to hand back verbatim.
func rawTrigramSimilarity(t *testing.T, pool *pgxpool.Pool, a, b string) float64 {
	t.Helper()
	var sim float64
	if err := pool.QueryRow(context.Background(),
		`SELECT similarity($1::text, $2::text)`, a, b).Scan(&sim); err != nil {
		t.Fatalf("raw similarity(%q, %q): %v", a, b, err)
	}
	return sim
}

// measureLegibility asks the database the same question trigramDuplicates
// asks — through the same SQL constant, so the two can never drift.
func measureLegibility(t *testing.T, pool *pgxpool.Pool, s string) trigramLegibility {
	t.Helper()
	var l trigramLegibility
	if err := pool.QueryRow(context.Background(), trigramLegibilitySQL, s, duplicateCtypeProbe).
		Scan(&l.Trigrams, &l.Visible, &l.Length, &l.Bytes,
			&l.CtypeReadsUnicodeLetters); err != nil {
		t.Fatalf("measure legibility of %q: %v", s, err)
	}
	return l
}

// TestCheckDuplicates_ResidueCollisionIsNotAMatch is the core sk-228
// regression: two titles whose only shared content is what pg_trgm's
// tokeniser did NOT throw away must not be reported as duplicates, no
// matter that the raw metric calls them identical.
//
// The test first proves the hazard is real on THIS database (raw
// similarity == 1.0) and only then asserts the guard suppressed it. That
// ordering matters: without the first assertion a future change that
// merely stopped the two titles from colliding would leave the test
// green while the defect walked back in through some other input.
func TestCheckDuplicates_ResidueCollisionIsNotAMatch(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	seedIssueWithDescription(t, pool, pid, owner, prefix+"-1", residueTitleA, "")

	raw := rawTrigramSimilarity(t, pool, residueTitleA, residueTitleB)
	if raw < 0.999 {
		t.Fatalf("fixture no longer reproduces the hazard: raw pg_trgm similarity "+
			"between %q and %q is %.4f, expected ~1.0 — pick a new residue pair",
			residueTitleA, residueTitleB, raw)
	}
	t.Logf("raw pg_trgm similarity between the two titles is %.4f "+
		"(legibility A=%+v B=%+v)", raw,
		measureLegibility(t, pool, residueTitleA),
		measureLegibility(t, pool, residueTitleB))

	store := NewStore(pool) // nil embedder → trigram-only
	report, err := store.CheckDuplicates(context.Background(), pid, residueTitleB, "")
	if err != nil {
		t.Fatalf("CheckDuplicates: %v", err)
	}
	if len(report.Candidates) != 0 {
		t.Fatalf("expected zero candidates — pg_trgm compared residues, not titles; got %+v",
			report.Candidates)
	}
	if !report.Degraded {
		t.Fatal("expected Degraded=true: neither layer produced a verdict, and an " +
			"empty candidate list must not read as 'compared and clean'")
	}
	if report.DegradedReason == "" {
		t.Fatal("Degraded=true with an empty DegradedReason")
	}
	t.Logf("degraded reason: %s", report.DegradedReason)
}

// TestCheckDuplicates_ShortTitleIsNotAVerdict covers short titles as a
// class. A one-character title yields two trigrams; Jaccard over a
// two-element set can only return 0, 0.5 or 1, so the only value it can
// ever produce at or above duplicateMinReport is an unqualified 1.0.
// That is an equality test on a residue wearing a similarity test's
// name, and it fired in production against five unrelated closed epics.
//
// Note the seeded row here has the IDENTICAL title. Even that must not
// be reported by the trigram layer: it is not evidence about the issues,
// only about two characters. Such a title is routed to the semantic
// layer, which reads the description too.
func TestCheckDuplicates_ShortTitleIsNotAVerdict(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	seedIssueWithDescription(t, pool, pid, owner, prefix+"-1", "1", "")
	seedIssueWithDescription(t, pool, pid, owner, prefix+"-2", "Заметка блока 1", "")

	if raw := rawTrigramSimilarity(t, pool, "1", "1"); raw < 0.999 {
		t.Fatalf("sanity: similarity('1','1') = %.4f, expected 1.0", raw)
	}

	store := NewStore(pool)
	report, err := store.CheckDuplicates(context.Background(), pid, "1", "")
	if err != nil {
		t.Fatalf("CheckDuplicates: %v", err)
	}
	if len(report.Candidates) != 0 {
		t.Fatalf("a 1-character title must not produce a trigram verdict, got %+v",
			report.Candidates)
	}
	if !report.Degraded {
		t.Fatal("expected Degraded=true — nothing compared this title")
	}
}

// TestCheckDuplicates_Sk228ProductionFixture runs the exact strings from
// the sk-228 report and asserts the outcome that must hold on EITHER
// cluster flavour: no duplicate is reported.
//
// The interesting half is what it asserts about the database underneath.
// On a ctype=C cluster (production: PostgreSQL 17.6, datctype=C) pg_trgm
// discards every Cyrillic character and scores these unrelated titles at
// 1.0; on the ctype=en_US.utf8 image CI uses it reads them fully and
// scores ~0.09. The test measures which one it is running on, logs it,
// and pins the corresponding raw score — so the day CI's image changes
// locale, this test says so in its output instead of silently changing
// what it covers.
func TestCheckDuplicates_Sk228ProductionFixture(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	const (
		seededTitle = "Outbox: собирать письма в одну очередь и помечать отправленные серым"
		queryTitle  = "Убрать Outbox из бокового меню и почистить локальный кэш"
	)
	seedIssueWithDescription(t, pool, pid, owner, prefix+"-1", seededTitle, "")

	seededLegibility := measureLegibility(t, pool, seededTitle)
	raw := rawTrigramSimilarity(t, pool, seededTitle, queryTitle)

	if seededLegibility.legible() {
		// ctype reads Cyrillic (dev/CI image). The titles genuinely
		// share almost nothing, so the raw score must be far below the
		// report threshold on its own.
		t.Logf("database ctype reads Cyrillic (coverage %.2f, %d trigrams); "+
			"raw similarity %.4f", seededLegibility.coverage(),
			seededLegibility.Trigrams, raw)
		if raw >= duplicateMinReport {
			t.Fatalf("raw similarity %.4f >= %.2f on a Cyrillic-legible cluster — "+
				"unexpected, these titles share only the word Outbox", raw, duplicateMinReport)
		}
	} else {
		// ctype=C (production flavour). This is the defect: pg_trgm
		// compares residues and calls unrelated titles identical.
		t.Logf("database ctype STRIPS Cyrillic (coverage %.2f, %d trigrams) — "+
			"production flavour; raw similarity %.4f", seededLegibility.coverage(),
			seededLegibility.Trigrams, raw)
		if raw < 0.999 {
			t.Fatalf("expected the residue collision (raw ~1.0) on a ctype=C "+
				"cluster, got %.4f", raw)
		}
	}

	store := NewStore(pool)
	report, err := store.CheckDuplicates(context.Background(), pid, queryTitle, "")
	if err != nil {
		t.Fatalf("CheckDuplicates: %v", err)
	}
	if len(report.Candidates) != 0 {
		t.Fatalf("sk-228: these two titles are unrelated; got %+v", report.Candidates)
	}
}

// TestCheckDuplicates_LegibleTitleStillMatches is the counterweight: the
// guard must not have quietly disabled layer 1 for ordinary titles. Same
// shape as the fix's failure mode, asserted from the other side.
func TestCheckDuplicates_LegibleTitleStillMatches(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	seedIssueWithDescription(t, pool, pid, owner, prefix+"-1",
		"Add CSV export to reports", "")

	store := NewStore(pool)
	report, err := store.CheckDuplicates(context.Background(), pid,
		"Add CSV export to reports", "")
	if err != nil {
		t.Fatalf("CheckDuplicates: %v", err)
	}
	if len(report.Candidates) != 1 {
		t.Fatalf("expected the legible exact-title match to survive the guard, got %+v",
			report.Candidates)
	}
	got := report.Candidates[0]
	if got.MatchedBy != MatchedByTrigram {
		t.Fatalf("MatchedBy = %q, want %q", got.MatchedBy, MatchedByTrigram)
	}
	if got.TrigramSimilarity == nil {
		t.Fatal("TrigramSimilarity must be populated on a trigram hit")
	}
	if got.SemanticSimilarity != nil {
		t.Fatalf("SemanticSimilarity must be nil with no embedder, got %v",
			*got.SemanticSimilarity)
	}
}

// TestTrigramLegibility_Boundaries pins the three halves of the
// precondition against the numbers their constants are argued from. Pure
// logic — runs without a database.
//
// `Bytes` is set on every case because it is half of the ctype clause:
// on a UTF-8 database Bytes == Length is exactly "wholly ASCII", and the
// clause only ever bites when the cluster cannot read non-ASCII letters.
func TestTrigramLegibility_Boundaries(t *testing.T) {
	cases := []struct {
		name string
		l    trigramLegibility
		want string
	}{
		{"one-char title: two trigrams, no dynamic range",
			trigramLegibility{Trigrams: 2, Visible: 1, Length: 1, Bytes: 1,
				CtypeReadsUnicodeLetters: true}, legibilityTooShort},
		{"two-char title: three trigrams, still quantised",
			trigramLegibility{Trigrams: 3, Visible: 2, Length: 2, Bytes: 2,
				CtypeReadsUnicodeLetters: true}, legibilityTooShort},
		{"three-char title: at the floor",
			trigramLegibility{Trigrams: 4, Visible: 3, Length: 3, Bytes: 3,
				CtypeReadsUnicodeLetters: true}, legibilityRan},
		{"long title the tokeniser could barely read (ctype=C on Cyrillic)",
			trigramLegibility{Trigrams: 7, Visible: 6, Length: 65, Bytes: 120}, legibilityResidue},
		{"exactly at the coverage line",
			trigramLegibility{Trigrams: 12, Visible: 10, Length: 20, Bytes: 20,
				CtypeReadsUnicodeLetters: true}, legibilityRan},
		{"just under the coverage line",
			trigramLegibility{Trigrams: 12, Visible: 9, Length: 20, Bytes: 20,
				CtypeReadsUnicodeLetters: true}, legibilityResidue},
		{"ordinary title",
			trigramLegibility{Trigrams: 38, Visible: 38, Length: 49, Bytes: 49,
				CtypeReadsUnicodeLetters: true}, legibilityRan},
		{"empty",
			trigramLegibility{}, legibilityEmpty},

		// sk-228 second pass. These four are the measured shape of the
		// hole the coverage ratio left open: a Latin prefix carries the
		// title over 0.5 while the Russian tail — the only part that
		// tells two issues apart — is invisible to the tokeniser.
		// Numbers taken from PostgreSQL 17.6, ctype=C.
		{"'[app-12] MCP create падает' on a ctype-blind cluster",
			trigramLegibility{Trigrams: 18, Visible: 14, Length: 26, Bytes: 38}, legibilityCtypeBlind},
		{"'Fix header баг' on a ctype-blind cluster",
			trigramLegibility{Trigrams: 11, Visible: 9, Length: 14, Bytes: 20}, legibilityCtypeBlind},
		{"'Refactor Sidebar tab для формы' on a ctype-blind cluster",
			trigramLegibility{Trigrams: 21, Visible: 18, Length: 30, Bytes: 42}, legibilityCtypeBlind},
		{"'v2.1 magic links сломаны' on a ctype-blind cluster",
			trigramLegibility{Trigrams: 17, Visible: 13, Length: 24, Bytes: 32}, legibilityCtypeBlind},

		// ...and the same four on a cluster that reads them: the ctype
		// clause must be a no-op there, or the fix has disabled layer 1
		// on healthy deployments.
		{"'[app-12] MCP create падает' on a Unicode-reading cluster",
			trigramLegibility{Trigrams: 25, Visible: 20, Length: 26, Bytes: 38,
				CtypeReadsUnicodeLetters: true}, legibilityRan},
		{"'Refactor Sidebar tab для формы' on a Unicode-reading cluster",
			trigramLegibility{Trigrams: 31, Visible: 26, Length: 30, Bytes: 42,
				CtypeReadsUnicodeLetters: true}, legibilityRan},

		// A wholly-ASCII title is read in full even by a ctype-blind
		// cluster — the guard must not switch layer 1 off wholesale.
		{"'Add CSV export to reports' on a ctype-blind cluster",
			trigramLegibility{Trigrams: 27, Visible: 23, Length: 27, Bytes: 27}, legibilityRan},
		{"'[app-12] fix(api): drop the guard' on a ctype-blind cluster",
			trigramLegibility{Trigrams: 30, Visible: 23, Length: 33, Bytes: 33}, legibilityRan},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.l.verdict(); got != c.want {
				t.Fatalf("verdict(%+v) = %q, want %q (coverage %.3f)",
					c.l, got, c.want, c.l.coverage())
			}
			if got, want := c.l.legible(), c.want == legibilityRan; got != want {
				t.Fatalf("legible(%+v) = %v, want %v", c.l, got, want)
			}
		})
	}
}

// ---------------------------------------------------------------------
// sk-228, second pass — the coverage ratio was measured over the whole
// title while similarity() was computed over the residue
// ---------------------------------------------------------------------

// mixedTitlePairs are the four measured shapes of that hole: a Latin
// prefix that clears any coverage ratio worth setting, and a Russian
// tail that is the only thing telling the two issues apart. On
// PostgreSQL 17.6 with ctype=C every pair scores EXACTLY 1.0, clears
// duplicateMinCoverage on both sides, and therefore short-circuited
// layer 2 and blocked the create — the sk-228 symptom, still reachable
// after the first attempt at the guard.
var mixedTitlePairs = []struct{ seeded, query string }{
	{"[app-12] MCP create падает", "[app-12] MCP create зависает"},
	{"Fix header баг", "Fix header фича"},
	{"Refactor Sidebar tab для формы", "Refactor Sidebar tab у карточек"},
	{"v2.1 magic links сломаны", "v2.1 magic links тормозят"},
}

// runTrigramCandidateSQL executes layer 1 exactly as the store does —
// same SQL constant, same parameters — with the cluster's ctype answer
// supplied by the caller. Returns the ids that survived the predicates.
func runTrigramCandidateSQL(
	t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID, query string, ctypeReadsUnicode bool,
) []string {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL pg_trgm.similarity_threshold = 0.5`); err != nil {
		t.Fatalf("set threshold: %v", err)
	}
	rows, err := tx.Query(ctx, trigramCandidateSQL, projectID, query, duplicateVecCandidates,
		duplicateMinTrigrams, duplicateMinCoverage, ctypeReadsUnicode)
	if err != nil {
		t.Fatalf("layer-1 query: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var (
			id, title, status, typ string
			sim                    float64
			sameDiscard            bool
		)
		if err := rows.Scan(&id, &title, &status, &typ, &sim, &sameDiscard); err != nil {
			t.Fatalf("scan: %v", err)
		}
		t.Logf("  candidate %s %q sim=%.4f sameDiscard=%v", id, title, sim, sameDiscard)
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return ids
}

// TestTrigramCandidates_CtypeBlindClusterDropsMixedTitles is the second
// pass's regression, run against the production SQL rather than against
// a re-implementation of it.
//
// CI's database is ctype=en_US.utf8, where Cyrillic survives tokenisation
// and the hole cannot occur — so the test supplies the one input that
// distinguishes the two cluster flavours ($6, "this ctype classifies
// non-ASCII letters") and asserts BOTH answers:
//
//   - with $6=false (production: PostgreSQL 17.6, datctype=C) every mixed
//     title must be refused, because there the tokeniser silently deleted
//     the words that tell them apart;
//   - with $6=true (CI, and any correctly-configured cluster) every one of
//     them must still be a candidate, because there nothing was lost —
//     this half is what stops the guard from being "raise the ratio until
//     layer 1 stops answering".
//
// Delete the `($6 OR octet_length = char_length)` clause and the first
// half goes red on any cluster.
func TestTrigramCandidates_CtypeBlindClusterDropsMixedTitles(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	for i, p := range mixedTitlePairs {
		seedIssueWithDescription(t, pool, pid, owner,
			fmt.Sprintf("%s-%d", prefix, i+1), p.seeded, "")
	}

	for i, p := range mixedTitlePairs {
		t.Run(p.query, func(t *testing.T) {
			want := fmt.Sprintf("%s-%d", prefix, i+1)

			t.Logf("ctype-blind cluster ($6=false):")
			if got := runTrigramCandidateSQL(t, pool, pid, p.query, false); len(got) != 0 {
				t.Fatalf("a ctype-blind cluster cannot read %q — layer 1 must produce "+
					"no candidate, got %v", p.query, got)
			}

			t.Logf("Unicode-reading cluster ($6=true):")
			got := runTrigramCandidateSQL(t, pool, pid, p.query, true)
			if len(got) != 1 || got[0] != want {
				t.Fatalf("a cluster that reads these titles must still compare them: "+
					"want [%s], got %v", want, got)
			}
		})
	}
}

// TestCheckDuplicates_HealthyClusterStillComparesMixedTitles is the
// end-to-end counterweight on whatever cluster the suite is running:
// the guard must not have taken the trigram layer away from titles this
// database can read. It also pins WHICH flavour that is, so the day the
// CI image changes locale the output says so.
func TestCheckDuplicates_HealthyClusterStillComparesMixedTitles(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	const (
		seeded = "Refactor Sidebar tab для формы"
		query  = "Refactor Sidebar tab для форм"
	)
	seedIssueWithDescription(t, pool, pid, owner, prefix+"-1", seeded, "")

	l := measureLegibility(t, pool, query)
	if !l.CtypeReadsUnicodeLetters {
		t.Skipf("this database's ctype cannot read non-ASCII letters (%+v) — "+
			"the mixed-title comparison this test asserts is correctly refused here; "+
			"TestTrigramCandidates_CtypeBlindClusterDropsMixedTitles covers that side", l)
	}
	if v := l.verdict(); v != legibilityRan {
		t.Fatalf("on a Unicode-reading cluster a mixed title must stay legible, got %q (%+v)", v, l)
	}

	store := NewStore(pool)
	report, err := store.CheckDuplicates(context.Background(), pid, query, "")
	if err != nil {
		t.Fatalf("CheckDuplicates: %v", err)
	}
	if len(report.Candidates) != 1 {
		t.Fatalf("layer 1 must still compare titles this cluster can read; got %+v",
			report.Candidates)
	}
	if report.Candidates[0].MatchedBy != MatchedByTrigram {
		t.Fatalf("MatchedBy = %q, want %q", report.Candidates[0].MatchedBy, MatchedByTrigram)
	}
}

// TestCheckDuplicates_StrongMatchWithDifferentDiscardDoesNotDecideAlone
// covers the second of the two thresholds: the right to produce a number
// is not the right to end the check.
//
// The two titles have IDENTICAL alphanumeric content, so pg_trgm scores
// them 1.0 on every ctype; everything that distinguishes them lives in
// the characters it discarded. Layer 1 may say so — the row is reported
// with its real score — but it may not skip the layer that can read the
// difference. The probe embedder panics if called, so "layer 2 ran" is
// asserted by the panic, not by a flag.
func TestCheckDuplicates_StrongMatchWithDifferentDiscardDoesNotDecideAlone(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	const (
		seeded = "Refactor Sidebar tab ...."
		query  = "Refactor Sidebar tab !!!!"
	)
	seedIssueWithDescription(t, pool, pid, owner, prefix+"-1", seeded, "")

	if raw := rawTrigramSimilarity(t, pool, seeded, query); raw < 0.999 {
		t.Fatalf("fixture no longer reproduces the hazard: similarity(%q,%q) = %.4f, "+
			"expected ~1.0", seeded, query, raw)
	}
	for _, s := range []string{seeded, query} {
		if l := measureLegibility(t, pool, s); !l.legible() {
			t.Fatalf("fixture must clear the legibility gate — this test is about the "+
				"OTHER gate; %q measured %+v", s, l)
		}
	}

	// erroringEmbedder proves layer 2 was entered: a short-circuit would
	// never touch the embedder and the report would come back
	// Degraded=false.
	store := NewStoreWithEmbedder(pool, &erroringEmbedder{err: errors.New("ollama unreachable")})
	report, err := store.CheckDuplicates(context.Background(), pid, query, "a description")
	if err != nil {
		t.Fatalf("CheckDuplicates: %v", err)
	}
	if len(report.Candidates) != 1 {
		t.Fatalf("the score is real and must still be reported; got %+v", report.Candidates)
	}
	if report.Candidates[0].Similarity < duplicateStrongMatch {
		t.Fatalf("expected the strong score to survive, got %v", report.Candidates[0].Similarity)
	}
	if !report.Degraded {
		t.Fatal("a strong match whose discarded characters differ must not short-circuit: " +
			"layer 2 had to be consulted, it failed, and that must be reported")
	}
}

// TestCheckDuplicates_StrongMatchWithMovedPunctuationDoesNotDecideAlone
// covers the half of the decisive gate that a bare multiset comparison
// misses: the discarded characters are IDENTICAL, and only their places
// differ.
//
// pg_trgm treats punctuation as a word separator, so sliding one across a
// word boundary leaves the trigram set — and therefore the score —
// untouched. Both titles below discard exactly "/" and score 1.0; under a
// gate that compared discarded characters as a bare sequence they were
// "the same", the row was decisive, and layer 2 was skipped on a pair
// whose grouping had changed (TLS/SSL → SSL/validation). Found by review
// and reproduced on en_US.utf8 before the skeleton comparison replaced
// the multiset one.
//
// Unlike the sibling above this needs no ctype trickery: it is a defect of
// the gate itself and reproduces on the CI database as it stands.
func TestCheckDuplicates_StrongMatchWithMovedPunctuationDoesNotDecideAlone(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	const (
		seeded = "Enable TLS/SSL validation in production"
		query  = "Enable TLS SSL/validation in production"
	)
	seedIssueWithDescription(t, pool, pid, owner, prefix+"-1", seeded, "")

	if raw := rawTrigramSimilarity(t, pool, seeded, query); raw < 0.999 {
		t.Fatalf("fixture no longer reproduces the hazard: similarity(%q,%q) = %.4f, "+
			"expected ~1.0 — pg_trgm must see these as identical for the test to "+
			"be about the gate at all", seeded, query, raw)
	}
	for _, s := range []string{seeded, query} {
		if l := measureLegibility(t, pool, s); !l.legible() {
			t.Fatalf("fixture must clear the legibility gate — this test is about the "+
				"OTHER gate; %q measured %+v", s, l)
		}
	}

	store := NewStoreWithEmbedder(pool, &erroringEmbedder{err: errors.New("ollama unreachable")})
	report, err := store.CheckDuplicates(context.Background(), pid, query, "a description")
	if err != nil {
		t.Fatalf("CheckDuplicates: %v", err)
	}
	if len(report.Candidates) != 1 {
		t.Fatalf("the score is real and must still be reported; got %+v", report.Candidates)
	}
	if report.Candidates[0].Similarity < duplicateStrongMatch {
		t.Fatalf("expected the strong score to survive, got %v", report.Candidates[0].Similarity)
	}
	if !report.Degraded {
		t.Fatal("identical discarded characters in DIFFERENT places must not short-circuit: " +
			"the separator moved across a word boundary, which is exactly the " +
			"difference pg_trgm cannot see, so layer 2 had to be consulted")
	}
}

// TestCheckDuplicates_StrongMatchWithSameDiscardStillShortCircuits is the
// counterweight: the fast path must survive for the case it exists for —
// a near-identical title where nothing hides in the discarded part.
func TestCheckDuplicates_StrongMatchWithSameDiscardStillShortCircuits(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	seedIssueWithDescription(t, pool, pid, owner, prefix+"-1",
		"Add CSV export to reports", "")

	// panicOnUseEmbedder turns "layer 2 ran" into a test failure.
	store := NewStoreWithEmbedder(pool, &panicOnUseEmbedder{t: t})
	report, err := store.CheckDuplicates(context.Background(), pid,
		"Add CSV export to reports", "some description")
	if err != nil {
		t.Fatalf("CheckDuplicates: %v", err)
	}
	if len(report.Candidates) != 1 {
		t.Fatalf("expected the exact-title match, got %+v", report.Candidates)
	}
	if report.Degraded {
		t.Fatal("a decisive strong match is a confident answer, not a degraded one")
	}
}
