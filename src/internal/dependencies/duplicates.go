package dependencies

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
)

// duplicateTrigramThreshold is the floor for trigram similarity in the
// candidate scan. Below this we treat the row as "not a candidate" —
// matches the F164 layer-1 behaviour described in db-schema §4.5.
//
// 0.5 is the Postgres `pg_trgm.similarity_threshold` recommendation for
// "feels like a real fuzzy match"; lower values flood the result with
// near-coincidence triplets.
const duplicateTrigramThreshold = 0.5

// duplicateMaxResults caps the response. The handler renders these as
// "did you mean ...?" hints, not a full search UI. Keeping it small also
// avoids exposing too much project content in a check-duplicates probe.
const duplicateMaxResults = 5

// duplicateStrongMatch is the trigram score above which we trust layer-1
// alone and skip the embedder round-trip. 0.85 matches the threshold the
// search service uses for "exact-match"-flavoured results; an issue with
// a near-identical title is overwhelmingly likely a real dup, and the
// vector rerank wouldn't change the ordering meaningfully.
//
// Saves ~200ms p50 in the common "renamed-typo dup" case (one Ollama
// HTTP round-trip + 1024-dim halfvec scan).
//
// Only LEGIBLE rows (see trigramLegibility) can trip it. Before sk-228 an
// illegible row could, and that is exactly how two unrelated Russian
// titles short-circuited the whole pipeline at a fabricated 1.0.
const duplicateStrongMatch = 0.85

// duplicateMinReport is the floor for surfacing a candidate to the
// caller. We always run with at least this much evidence — anything
// lower is noise. Applies to BOTH trigram and vector similarity, taken
// after the per-row max() merge.
//
// 0.80 (vs the historical 0.85) is the experimental calibration: trades
// a few extra false-positive blocks (agents getting `decisionRequired`
// on tasks they think are distinct) for catching rephrasings the 0.85
// gate let through. False-positive cost is one retry with `force=true`;
// false-negative cost is two issues for the same work, noticed days
// later when comments diverge. The retry cost is cheaper, so we lean
// stricter. Revisit by sampling logs of `similarity ∈ [0.78, 0.85]`
// cases over the next weeks and deciding which side overruns.
const duplicateMinReport = 0.80

// duplicateVecCandidates is the layer-2 kNN top-K. Tighter than the RRF
// candidate budget in search.SearchIssues because dup detection only
// looks for "is this the same issue", not "rank a feed". Lower K =
// fewer halfvec compares = faster.
const duplicateVecCandidates = 10

// ---------------------------------------------------------------------
// sk-228: the trigram layer's precondition
// ---------------------------------------------------------------------
//
// pg_trgm does not tokenise the string it is handed. It first throws away
// every character its `ISWORDCHR` test rejects — under `KEEPONLYALNUM`
// (the build this extension ships with) that macro is `t_isalnum(c)`,
// POSIX-equivalent to `isalpha || isdigit`; see contrib/pg_trgm/trgm.h and
// src/backend/tsearch/ts_locale.c in REL_17_6. `t_isalnum` is resolved
// against the DATABASE's ctype, is not settable per query, and — verified
// — is not influenced by an argument `COLLATE` either: pg_trgm reads the
// database default, full stop (measured: `show_trgm(x COLLATE pg_c_utf8)`
// on a ctype=C database still returns the ctype=C tokenisation).
//
// So the same query answers differently on two clusters:
//
//	          show_trgm('Заметка блока 1')
//	ctype=en_US.utf8   →  16 trigrams (the whole title)
//	ctype=C            →  {"  1"," 1 "}   (everything but the digit gone)
//
// Under ctype=C every Cyrillic title collapses to its Latin/digit
// residue, and similarity() then compares residues while reporting the
// number as if it had compared titles. Two unrelated issues whose
// residues coincide score EXACTLY 1.0 — measured on the production
// cluster (skein, PostgreSQL 17.6, datctype=C):
//
//	similarity('1', 'Заметка блока 1')                        = 1
//	similarity('Убрать Outbox из меню …', 'Outbox: очередь…')  = 1
//
// The honest statement of the algorithm is therefore: **Jaccard over
// trigrams is a similarity measure of the titles only when the tokeniser
// could actually read the parts that tell the titles apart.** The three
// constants below are that precondition, measured per comparison, in SQL,
// so the check tracks whatever ctype the cluster in front of us happens
// to have.
//
// A row that fails it is not scored down and it is not reported as a
// weak match — it is dropped from the trigram layer entirely, because a
// number derived from a residue is not a weaker verdict, it is a
// different question's answer. The semantic layer, which embeds the raw
// text in Go and is unaffected by the cluster's ctype, still sees the
// row; when the semantic layer cannot run either, the caller is told the
// check was degraded rather than handed a fabricated score.
//
// # The second pass, and why a coverage ratio was not enough
//
// The first attempt at this guard measured only how large the visible
// residue was relative to the whole title, and let a row through at
// half. That closed the all-Cyrillic case and left the
// mixed case wide open, because the ratio is measured over the WHOLE
// title while similarity() is computed over the RESIDUE ONLY. A title of
// the shape "Latin prefix + Russian tail" carries enough Latin to clear
// any ratio you would dare to set, and the Latin is precisely the part
// two different issues share. Measured on PostgreSQL 17.6, ctype=C:
//
//	'[app-12] MCP create падает'     vs '[app-12] MCP create зависает'    → 1.0 (coverage 0.538 / 0.500)
//	'Fix header баг'                 vs 'Fix header фича'                → 1.0 (coverage 0.643 / 0.600)
//	'Refactor Sidebar tab для формы' vs 'Refactor Sidebar tab у карточек' → 1.0 (coverage 0.600 / 0.581)
//	'v2.1 magic links сломаны'       vs 'v2.1 magic links тормозят'      → 1.0 (coverage 0.542 / 0.520)
//
// All four clear coverage 0.5, all four score an unqualified 1.0, and
// all four are the sk-228 symptom — a short-circuit at
// duplicateStrongMatch, no semantic layer, `degraded:false`. Raising the
// ratio is not a fix: at 0.99 the layer is dead on any title with
// punctuation in it, on a healthy cluster too.
//
// What the ratio cannot express is the thing that actually matters —
// whether the characters the tokeniser dropped could have carried
// meaning. That question has a cluster-level answer and a per-title one,
// and duplicateCtypeProbe splits them:
//
//   - a ctype that classifies non-ASCII letters (any UTF-8 locale, or
//     PostgreSQL 17's builtin pg_c_utf8) reads every letter of every
//     title, so nothing that could carry meaning is ever lost and the
//     layer runs exactly as it always has;
//   - a ctype that does not (C / POSIX — the production cluster) drops
//     every non-ASCII character, letters included, and cannot tell us
//     which were letters. On such a cluster pg_trgm's verdict is a
//     verdict about the title only when the title is wholly ASCII.
//
// The second form is deliberately conservative: an English title with a
// single em dash in it is refused on a C cluster even though only
// punctuation was lost. The cost of that refusal is one fall-through to
// the semantic layer; the cost of the opposite error is the bug this
// file is named after.

// duplicateMinTrigrams is the smallest trigram set the layer-1 metric is
// allowed to have an opinion on, applied to BOTH sides of a comparison.
//
// The predicate counts array_length(show_trgm(...)) — the trigrams of the
// whole residue, summed across its words. A word of n characters yields
// n+1 of them, so 4 is "one three-character word, or two words that
// together carry that much". The number comes from the metric's
// resolution, not from taste: Jaccard over a 2-element set (a
// 1-character title) can only ever return 0, 0.5 or 1, and over a
// 3-element set 0, ⅓, ⅔ or 1. Below 4 the only reachable value at or
// above duplicateMinReport is exactly 1.0 — the layer has no dynamic
// range left and has quietly stopped being a similarity test, becoming
// an equality test on a one-or-two-character residue. That is a fine
// thing to compute and a terrible thing to call "similarity: 1".
//
// Short titles are a legal input, so they are not rejected: they are
// routed to the semantic layer, which reads the description too and has
// real signal to work with.
const duplicateMinTrigrams = 4

// duplicateMinCoverage is the fraction of a title's characters that the
// pg_trgm tokeniser must actually be able to see before its verdict on
// that title is worth having at all.
//
// coverage(t) = |characters of t matching [[:alnum:]]| / |characters of t|
//
// `[[:alnum:]]` is evaluated by the same libc ctype that drives pg_trgm's
// t_isalnum — verified on both cluster flavours (a ctype=C database
// reports 1 visible character in 'Заметка блока 1', a ctype=en_US.utf8
// one reports 13). That equivalence holds while the operand uses the
// database's default collation, which `issues.title` does (declared
// `text NOT NULL`, no COLLATE); putting an explicit COLLATE on the column
// or in this query would silently make the two measure different things.
//
// This is the WEAKER of the two halves and it is kept for what it alone
// catches: a title that is mostly decoration, where the metric would be
// opining on a scrap ("—— Outbox ——————" vs "«»(),.;: Outbox !!!" score
// 1.0 on EVERY ctype, both at coverage ≈ 0.19). It is not, and was never,
// sufficient on its own — see "The second pass" above.
const duplicateMinCoverage = 0.5

// duplicateCtypeProbe asks the cluster the one question a Go process
// cannot answer for it: does this database's ctype classify non-ASCII
// letters at all?
//
// The negative answer is the one the guard is built on, and it is
// documented rather than inferred. PostgreSQL's manual on POSIX character
// classes: "the C locale never considers any non-ASCII characters to
// belong to any of these classes"
// (https://www.postgresql.org/docs/17/functions-matching.html), and
// t_isalnum — the function behind pg_trgm's ISWORDCHR — short-circuits on
// the same condition: `if (clen == 1 || database_ctype_is_c) return
// isalnum(TOUCHAR(ptr));` (src/backend/tsearch/ts_locale.c, REL_17_6), so
// a multibyte character's lead byte is never a word character there. On
// such a cluster "the tokeniser read the whole title" and "the title is
// wholly ASCII" are the same statement.
//
// The positive answer is weaker by nature — the same page warns that "the
// classification of non-ASCII characters can vary across platforms even
// in similarly-named locales", so a cluster that reads these two letters
// is not FORMALLY guaranteed to read every script. In practice glibc's
// UTF-8 locales share one i18n ctype table and PostgreSQL 17's builtin
// pg_c_utf8 is Unicode-complete, and the failure this file exists for is
// the binary C-versus-UTF-8 one, not a hypothetical per-script gap. Both
// probe characters are non-ASCII letters, one Cyrillic and one Latin-1,
// so the two alphabets this corpus is actually written in are covered.
//
// Measured in the same transaction as the comparison, next to the
// legibility numbers, so it costs no extra round-trip and can never
// describe a different connection than the one about to run similarity().
const duplicateCtypeProbe = "Аé"

// Match provenance values for DuplicateCandidate.MatchedBy. Exported as
// constants because the wire contract names them and callers branch on
// them.
const (
	MatchedByTrigram  = "trigram"
	MatchedBySemantic = "semantic"
	MatchedByBoth     = "both"
)

// Layer-1 outcome labels. legibilityRan means the trigram verdict was
// used; every other value names the half of the precondition that
// refused it. They are the values of the `trigram_layer` metric label and
// the vocabulary degradedReason speaks, so they are a closed set, not
// free text.
const (
	legibilityRan        = "ran"
	legibilityEmpty      = "empty_title"
	legibilityTooShort   = "too_few_trigrams"
	legibilityResidue    = "mostly_unreadable"
	legibilityCtypeBlind = "ctype_cannot_read"
)

// trigramLegibility is the measurement behind duplicateMinTrigrams /
// duplicateMinCoverage / duplicateCtypeProbe for one string, taken from
// the database that is about to run the comparison.
type trigramLegibility struct {
	// Trigrams is coalesce(array_length(show_trgm(s),1),0) — the size of
	// the set pg_trgm would actually compare.
	Trigrams int
	// Visible is the count of characters of s that pg_trgm's tokeniser
	// keeps under this database's ctype.
	Visible int
	// Length is char_length(s).
	Length int
	// Bytes is octet_length(s). On a UTF-8 database Bytes == Length says
	// exactly "this string is wholly ASCII", because UTF-8 spends one
	// byte on an ASCII character and two or more on everything else.
	Bytes int
	// CtypeReadsUnicodeLetters is the cluster-level answer to
	// duplicateCtypeProbe. False means this ctype discards every
	// non-ASCII character, so only a wholly-ASCII title is read in full.
	CtypeReadsUnicodeLetters bool
}

// verdict names the layer-1 outcome for this string: legibilityRan when
// a trigram score on it is a statement about the string, otherwise the
// half of the precondition that refused.
//
// The order of the tests is the order in which they explain a refusal,
// cheapest and most specific first, so the label a caller logs or counts
// is the most informative one available.
func (l trigramLegibility) verdict() string {
	if l.Length <= 0 {
		return legibilityEmpty
	}
	if l.Trigrams < duplicateMinTrigrams {
		return legibilityTooShort
	}
	if float64(l.Visible) < duplicateMinCoverage*float64(l.Length) {
		return legibilityResidue
	}
	if !l.CtypeReadsUnicodeLetters && l.Bytes != l.Length {
		return legibilityCtypeBlind
	}
	return legibilityRan
}

// legible reports whether a trigram verdict on this string may be used.
func (l trigramLegibility) legible() bool {
	return l.verdict() == legibilityRan
}

// coverage is the visible-character fraction, for logs and messages.
func (l trigramLegibility) coverage() float64 {
	if l.Length <= 0 {
		return 0
	}
	return float64(l.Visible) / float64(l.Length)
}

// trigramHit is one layer-1 row plus the thing the wire contract has no
// place for: whether this row's score is allowed to end the check on its
// own.
//
// The right to produce a number and the right to decide alone are not
// the same right, and sk-228 is what happens when they are conflated.
// legible() earns the first: the tokeniser read the title. sameDiscard
// earns the second: the characters the tokeniser threw away — every one
// of them bar whitespace, which is what a tokeniser is FOR — were the
// same on both sides AND in the same places, so a high score cannot be
// hiding a difference in the part the metric never saw. When they differ
// the candidate is still reported with its real score; it just may not
// silence the semantic layer, which reads what pg_trgm could not.
//
// Position is half the test, and comparing the discarded characters as a
// bare sequence loses it. pg_trgm treats punctuation as a word separator,
// so moving one across a word boundary leaves the trigram set — and the
// score — untouched while the discarded multiset stays equal. Measured on
// en_US.utf8, all at similarity 1.0 with equal discarded sequences:
//
//	'Enable TLS/SSL validation in production' / 'Enable TLS SSL/validation in production'
//	'grant read/write access to admins'       / 'grant read write/access to admins'
//	'Fix: login bug'                          / 'Fix login: bug'
//
// So the comparison is made on a SKELETON instead: every run of word
// characters collapses to one sentinel, runs of whitespace collapse to a
// single space, and the ends are trimmed — whitespace is the one thing a
// tokeniser is entitled to ignore. What survives is exactly the shape the
// metric could not see: which separators sit between which words. chr(1)
// is the sentinel rather than a printable stand-in so a title that
// contains the stand-in cannot forge a match.
//
// The class this closes on a cluster whose ctype IS Unicode-aware, where
// the legibility half is satisfied by construction: '🐛 Fix login' and
// '✨ Fix login' score 1.0 (measured), because glibc calls an emoji
// punctuation and the tokeniser drops it. Same shape as the incident,
// one layer down.
type trigramHit struct {
	candidate   DuplicateCandidate
	sameDiscard bool
}

// candidatesOf strips the layer-1 bookkeeping and hands back the wire
// rows in the order the query produced them.
func candidatesOf(hits []trigramHit) []DuplicateCandidate {
	out := make([]DuplicateCandidate, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.candidate)
	}
	return out
}

// DuplicateReport is the result of a duplicate check: the candidates
// plus an honest statement of how much of the detector actually ran.
//
// Degraded carries the same meaning it has in skein_search: the semantic
// (vector) retriever could not be consulted. It is deliberately NOT set
// when the semantic layer was skipped on purpose after a trustworthy
// strong trigram hit — that is a confident answer, not a degraded one.
//
// DegradedReason is empty exactly when Degraded is false. It is prose for
// a human, never a code to branch on — branch on Degraded. Only the REST
// endpoint returns it (`degradedReason`); skein_create keeps it off the
// MCP payload on purpose (SC-007 caps that response at 250 characters and
// on an embedder-less deployment the sentence would ride along on every
// single create) and logs it instead.
type DuplicateReport struct {
	Candidates     []DuplicateCandidate
	Degraded       bool
	DegradedReason string
}

// CheckDuplicates returns up to duplicateMaxResults issues in the project
// that look like duplicates of the (title, description) pair the caller
// is about to create, together with a Degraded flag saying whether the
// check got to use both of its layers.
//
// Two-layer detection:
//
//  1. Trigram on title (fast, in-process via pg_trgm). Top-10 with the
//     similarity threshold tuned via SET LOCAL so the GIN trigram index
//     is actually used (the `%` operator obeys
//     pg_trgm.similarity_threshold; without SET LOCAL we'd be stuck with
//     the global default of 0.3, see db-schema §2.5 gotcha). Rows whose
//     titles — or whose comparison against our title — the tokeniser
//     cannot actually read are excluded here, not scored (sk-228).
//  2. If layer-1 found a strong match (sim >= duplicateStrongMatch) AND
//     that row is allowed to decide alone (trigramHit.sameDiscard),
//     short-circuit — trigram is reliable for near-identical titles, and
//     skipping the embedder saves a ~200ms Ollama round-trip on the hot
//     path (every skein_create call). A strong match whose discarded
//     characters differ between the two titles is still reported, but
//     layer 2 runs anyway to corroborate it (sk-228).
//  3. Otherwise, if the embedder is available, run a vector kNN over
//     issues.embedding (cosine) using BuildIssueEmbedText(title,
//     description). Top-10. Union with layer-1, dedup by id, take
//     max(trigram_sim, vector_sim) as the headline while keeping both
//     component scores and a MatchedBy provenance tag.
//  4. Filter sim >= duplicateMinReport, return top duplicateMaxResults.
//
// Falls back to trigram-only when the embedder is nil OR not Available
// OR the embed call fails — graceful degrade is mandatory because we
// must NEVER block create on the embedder being down (per
// the project guidelines on graceful degradation). That fallback now reports
// itself: Degraded=true, so the caller can tell "confident duplicate"
// from "could not compare".
//
// project_id scoping is mandatory: cross-project search would leak
// titles to non-members. F164 global duplicate detection (across all
// visible projects) is later scope.
func (s *Store) CheckDuplicates(
	ctx context.Context,
	projectID uuid.UUID,
	title string,
	description string,
) (DuplicateReport, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		// Unreachable from either caller (REST and MCP both run
		// issues.ValidateTitle first), but the type's invariant is that
		// an empty candidate list plus Degraded=false means "compared,
		// clean". Nothing was compared here, so say so rather than let
		// a future caller read silence as a clean bill of health.
		duplicateChecksTotal.WithLabelValues(legibilityEmpty, "true").Inc()
		return DuplicateReport{
			Candidates:     []DuplicateCandidate{},
			Degraded:       true,
			DegradedReason: "duplicate detection degraded: the title was empty, so nothing was compared",
		}, nil
	}

	// Layer 1: trigram on title.
	hits, queryLegibility, err := s.trigramDuplicates(ctx, projectID, title)
	if err != nil {
		return DuplicateReport{}, err
	}
	layerOne := queryLegibility.verdict()
	trigramRan := layerOne == legibilityRan
	trigramHits := candidatesOf(hits)
	if !trigramRan {
		slog.InfoContext(ctx,
			"duplicate check: trigram layer skipped, title not legible to pg_trgm",
			"projectId", projectID.String(),
			"reason", layerOne,
			"titleChars", queryLegibility.Length,
			"visibleChars", queryLegibility.Visible,
			"trigrams", queryLegibility.Trigrams,
			"ctypeReadsUnicodeLetters", queryLegibility.CtypeReadsUnicodeLetters)
	}

	// Layer-1 strong match → short-circuit. We deliberately don't
	// also embed for "extra confirmation": the user already has a
	// near-identical title to look at, and the extra latency is
	// pure UX cost.
	//
	// `trigramRan &&` is redundant by construction — trigramDuplicates
	// returns no rows at all when the query title is illegible — and it
	// is written out anyway, because "a fabricated score decided this on
	// its own" is the failure the whole file exists to prevent and it
	// should not depend on a caller remembering an invariant two
	// functions away.
	if trigramRan && hasDecisiveStrongMatch(hits) {
		duplicateChecksTotal.WithLabelValues(layerOne, "false").Inc()
		return DuplicateReport{
			Candidates: filterAndCap(trigramHits, duplicateMinReport, duplicateMaxResults),
		}, nil
	}

	// Layer 2: vector rerank when an embedder is wired in and healthy.
	// On any vec failure (ollama down, dim mismatch, ctx cancel) we log
	// a warning and fall through to trigram-only. Never propagate to
	// the caller — duplicate detection is advisory, not load-bearing.
	if s.embedder != nil && s.embedder.Available() {
		vecHits, vecErr := s.vectorDuplicates(ctx, projectID, title, description)
		if vecErr != nil {
			slog.WarnContext(ctx,
				"vector duplicate detection failed; falling back to trigram-only",
				"projectId", projectID.String(),
				"error", vecErr.Error())
			duplicateChecksTotal.WithLabelValues(layerOne, "true").Inc()
			return DuplicateReport{
				Candidates:     filterAndCap(trigramHits, duplicateMinReport, duplicateMaxResults),
				Degraded:       true,
				DegradedReason: degradedReason("the semantic layer failed", trigramRan, queryLegibility),
			}, nil
		}
		merged := mergeDuplicateHits(trigramHits, vecHits)
		duplicateChecksTotal.WithLabelValues(layerOne, "false").Inc()
		return DuplicateReport{
			Candidates: filterAndCap(merged, duplicateMinReport, duplicateMaxResults),
		}, nil
	}

	duplicateChecksTotal.WithLabelValues(layerOne, "true").Inc()
	return DuplicateReport{
		Candidates:     filterAndCap(trigramHits, duplicateMinReport, duplicateMaxResults),
		Degraded:       true,
		DegradedReason: degradedReason("the semantic layer is unavailable", trigramRan, queryLegibility),
	}, nil
}

// degradedReason renders the prose the caller shows a human. It says
// which layer is missing and, when the trigram layer was also skipped,
// that nothing compared this title at all — the difference between "a
// weaker check ran" and "no check ran" is the whole point of the flag.
func degradedReason(cause string, trigramRan bool, l trigramLegibility) string {
	if trigramRan {
		return "duplicate detection degraded: " + cause +
			"; the check used pg_trgm on the title only"
	}
	return fmt.Sprintf(
		"duplicate detection degraded: %s, and pg_trgm could read only %d of %d "+
			"characters of this title (%.0f%%, %d trigrams, %s) so its verdict was "+
			"not used either — this title was effectively not compared against anything",
		cause, l.Visible, l.Length, l.coverage()*100, l.Trigrams, l.verdict())
}

// trigramLegibilitySQL measures the query title the way the comparison
// will read it, on the connection that will run the comparison: $1 is the
// title, $2 is duplicateCtypeProbe. Named rather than inlined so the
// tests can ask the database the same question the store asks, with no
// second copy of the expressions to drift.
const trigramLegibilitySQL = `
SELECT coalesce(array_length(show_trgm($1::text), 1), 0),
       char_length(regexp_replace($1::text, '[^[:alnum:]]', '', 'g')),
       char_length($1::text),
       octet_length($1::text),
       char_length(regexp_replace($2::text, '[^[:alnum:]]', '', 'g'))
         = char_length($2::text)`

// trigramCandidateSQL is layer 1 itself. Parameters, in order: $1
// project, $2 query title, $3 row cap, $4 duplicateMinTrigrams, $5
// duplicateMinCoverage, $6 "this cluster's ctype classifies non-ASCII
// letters".
//
// $6 is a parameter and not a compile-time branch precisely so a test can
// pin it to false and exercise the ctype=C decision on a CI database that
// is ctype=en_US.utf8 — the production flavour cannot otherwise be
// reproduced there, and a guard that is only ever run in the one
// configuration where it is a no-op is not tested at all.
const trigramCandidateSQL = `
SELECT i.id, i.title, i.status, i.type,
       similarity(i.title, $2) AS sim,
       btrim(regexp_replace(regexp_replace(i.title, '[[:alnum:]]+', chr(1), 'g'),
                            '[[:space:]]+', ' ', 'g'))
         = btrim(regexp_replace(regexp_replace($2, '[[:alnum:]]+', chr(1), 'g'),
                                '[[:space:]]+', ' ', 'g')) AS same_discard
  FROM issues i
 WHERE i.project_id = $1
   AND i.title % $2
   AND coalesce(array_length(show_trgm(i.title), 1), 0) >= $4
   AND char_length(regexp_replace(i.title, '[^[:alnum:]]', '', 'g'))::float8
         >= $5::float8 * char_length(i.title)::float8
   AND ($6::boolean OR octet_length(i.title) = char_length(i.title))
 ORDER BY sim DESC, i.id
 LIMIT $3`

// trigramDuplicates is the layer-1 query: pg_trgm.similarity over title,
// scoped to the project, with SET LOCAL so the % operator uses the
// configured threshold (and thus the GIN trigram index). Returns up to
// duplicateVecCandidates rows so the merge has enough material when
// layer-2 also runs.
//
// Also returns the legibility measurement for the QUERY title, taken
// from the same connection that would run the comparison. Both are
// needed by the caller: an illegible query title means layer 1 produced
// no verdict (not "no duplicates"), which the response has to say out
// loud rather than let the caller read as a clean bill of health.
func (s *Store) trigramDuplicates(
	ctx context.Context,
	projectID uuid.UUID,
	title string,
) ([]trigramHit, trigramLegibility, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, trigramLegibility{}, fmt.Errorf("opening duplicate-check tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, fmt.Sprintf(
		`SET LOCAL pg_trgm.similarity_threshold = %g`,
		duplicateTrigramThreshold)); err != nil {
		return nil, trigramLegibility{}, fmt.Errorf("setting trigram threshold: %w", err)
	}

	// How much of the query title can this cluster's pg_trgm actually
	// see? Asked of the database rather than computed in Go on purpose:
	// the answer depends on the database's ctype (sk-228), so Go cannot
	// know it and must not guess it.
	var q trigramLegibility
	if err := tx.QueryRow(ctx, trigramLegibilitySQL, title, duplicateCtypeProbe).
		Scan(&q.Trigrams, &q.Visible, &q.Length, &q.Bytes,
			&q.CtypeReadsUnicodeLetters); err != nil {
		return nil, trigramLegibility{}, fmt.Errorf("measuring trigram legibility: %w", err)
	}
	if !q.legible() {
		// Layer 1 has no verdict to give on this title. Returning an
		// empty set (rather than a low score) is the point: a score
		// computed over a residue answers a different question.
		return []trigramHit{}, q, nil
	}

	// The three legibility predicates below mirror, per candidate row,
	// the three the query title just passed — a comparison has two sides
	// and a guard that checks one of them guards nothing. They sit after
	// `i.title % $2` in the WHERE clause, which keeps the GIN opclass on
	// idx_issues_title_trgm: verified with EXPLAIN (ANALYZE) on 50 001
	// rows, the plan is Bitmap Index Scan on idx_issues_title_trgm with
	// Index Cond `(title % $)` before and after, and these land as heap
	// Filters, so show_trgm / regexp_replace only ever run on rows the
	// index already selected.
	//
	// same_discard is not a filter: it rides along on every row so the
	// caller can tell "may be reported" from "may decide alone". The
	// class it compares is [[:alnum:][:space:]] — everything the
	// tokeniser dropped EXCEPT whitespace, because separating words is
	// the tokeniser's job and two titles that differ only in spacing do
	// not differ.
	rows, err := tx.Query(ctx, trigramCandidateSQL, projectID, title, duplicateVecCandidates,
		duplicateMinTrigrams, duplicateMinCoverage, q.CtypeReadsUnicodeLetters)
	if err != nil {
		return nil, q, fmt.Errorf("running duplicate trigram search: %w", err)
	}
	defer rows.Close()

	out := make([]trigramHit, 0, duplicateVecCandidates)
	for rows.Next() {
		var (
			h   trigramHit
			sim float64
		)
		if err := rows.Scan(&h.candidate.ID, &h.candidate.Title, &h.candidate.Status,
			&h.candidate.Type, &sim, &h.sameDiscard); err != nil {
			return nil, q, fmt.Errorf("scanning duplicate row: %w", err)
		}
		h.candidate.Similarity = sim
		trgCopy := sim
		h.candidate.TrigramSimilarity = &trgCopy
		h.candidate.MatchedBy = MatchedByTrigram
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, q, fmt.Errorf("iterating duplicate trigram rows: %w", err)
	}
	return out, q, nil
}

// vectorDuplicates is the layer-2 query: cosine kNN over issues.embedding
// scoped to the project. Returns up to duplicateVecCandidates candidates
// with SemanticSimilarity populated. Similarity (the headline) is set to
// the same cosine score so the merge step can take max() against the
// trigram hit and end up with a well-defined value either way; the
// component scores stay separately readable so "which layer said this"
// survives the merge.
//
// Runs inside a short read-only tx with `SET LOCAL hnsw.ef_search = 100`
// so the HNSW probe widens the candidate graph (default 40 misses some
// near-neighbours under the halfvec dimension reduction) — same knob
// search.Store.executeQuery sets for the main RRF queries.
//
// Returns ErrEmbedderUnavailable (wrapped) when the embedder call fails,
// so the caller can branch on it. Other errors propagate as-is — the
// caller logs and falls back regardless.
func (s *Store) vectorDuplicates(
	ctx context.Context,
	projectID uuid.UUID,
	title, description string,
) ([]DuplicateCandidate, error) {
	embedText := buildIssueEmbedTextLite(title, description)
	if embedText == "" {
		// Nothing to embed — caller treats this as "vec produced no
		// candidates" and falls back to trigram cleanly.
		return nil, nil
	}

	vec, err := s.embedder.EmbedSingle(ctx, embedText)
	if err != nil {
		return nil, fmt.Errorf("embedding duplicate-check query: %w", err)
	}
	if len(vec) == 0 {
		return nil, fmt.Errorf("embedder returned empty vector")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("opening vec-duplicate tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SET LOCAL hnsw.ef_search = 100"); err != nil {
		return nil, fmt.Errorf("set hnsw.ef_search: %w", err)
	}

	hv := pgvector.NewHalfVector(vec)
	rows, err := tx.Query(ctx, `
SELECT i.id,
       i.title,
       i.status,
       i.type,
       1.0 - (i.embedding <=> $1::halfvec) AS cosine_sim
  FROM issues i
 WHERE i.project_id = $2
   AND i.embedding IS NOT NULL
 ORDER BY i.embedding <=> $1::halfvec
 LIMIT $3`, hv, projectID, duplicateVecCandidates)
	if err != nil {
		return nil, fmt.Errorf("running vector duplicate search: %w", err)
	}
	defer rows.Close()

	out := make([]DuplicateCandidate, 0, duplicateVecCandidates)
	for rows.Next() {
		var (
			c   DuplicateCandidate
			sim float64
		)
		if err := rows.Scan(&c.ID, &c.Title, &c.Status, &c.Type, &sim); err != nil {
			return nil, fmt.Errorf("scanning vector duplicate row: %w", err)
		}
		// Headline = the only score we have at this point. The merge
		// step below promotes max(trigram, vector) when both ran.
		c.Similarity = sim
		semCopy := sim
		c.SemanticSimilarity = &semCopy
		c.MatchedBy = MatchedBySemantic
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating vector duplicate rows: %w", err)
	}
	return out, nil
}

// buildIssueEmbedTextLite assembles the embed input for a not-yet-created
// issue. Mirrors search.BuildIssueEmbedText but keeps that function
// untouched (it lives in the search package; importing it here would
// create the very dep cycle the QueryEmbedder interface exists to
// prevent).
//
// We deliberately drop `acceptance` — the caller hasn't filled it in yet
// at create time, and trying to read it from `description` is brittle.
// Title + description is sufficient signal for "is this issue
// semantically a dup".
func buildIssueEmbedTextLite(title, description string) string {
	parts := make([]string, 0, 2)
	if t := strings.TrimSpace(title); t != "" {
		parts = append(parts, t)
	}
	if d := strings.TrimSpace(description); d != "" {
		parts = append(parts, d)
	}
	full := strings.Join(parts, "\n\n")
	// The search package caps at MaxEmbedRunes (160_000); duplicate
	// detection inputs are user-typed titles + draft descriptions, well
	// below that ceiling. Don't pull in the search-package truncator —
	// keeps this file free of imports on search.
	return full
}

// hasDecisiveStrongMatch reports whether any layer-1 row is entitled to
// end the check by itself: its score crosses duplicateStrongMatch AND it
// is decisive, i.e. the two titles discarded the same characters, so the
// score cannot be standing in for a difference the metric never read.
//
// A strong-but-not-decisive row is NOT dropped — it keeps its score and
// is reported. It simply does not get to skip layer 2 (see trigramHit).
func hasDecisiveStrongMatch(hits []trigramHit) bool {
	for _, h := range hits {
		if h.candidate.Similarity >= duplicateStrongMatch && h.sameDiscard {
			return true
		}
	}
	return false
}

// mergeDuplicateHits unions trigram + vector candidates by id and keeps
// the strongest evidence per row:
//
//   - Similarity = max(trigram_sim, vector_sim) — the score the UI sorts
//     on, semantics-of-the-strongest-signal.
//   - TrigramSimilarity / SemanticSimilarity keep the per-layer numbers,
//     nil for a layer that did not score this row.
//   - MatchedBy names which layer(s) spoke, so a caller never has to
//     infer provenance from which fields happen to be non-nil.
//
// The result is sorted by Similarity DESC, with id as a stable
// tie-breaker so paginated callers see the same order across reruns.
func mergeDuplicateHits(trigram, vector []DuplicateCandidate) []DuplicateCandidate {
	idx := make(map[string]*DuplicateCandidate, len(trigram)+len(vector))

	// Trigram first so we keep the (possibly NULL) SemanticSimilarity
	// from the vector pass — see the swap logic below.
	for i := range trigram {
		t := trigram[i]
		idx[t.ID] = &t
	}

	for i := range vector {
		v := vector[i]
		existing, ok := idx[v.ID]
		if !ok {
			vCopy := v
			idx[v.ID] = &vCopy
			continue
		}
		// Same id appeared in both layers. Merge:
		//   - Similarity bumps to whichever is higher (max).
		//   - SemanticSimilarity comes from the vector pass — the
		//     trigram entry's field is always nil here.
		if v.Similarity > existing.Similarity {
			existing.Similarity = v.Similarity
		}
		if v.SemanticSimilarity != nil {
			existing.SemanticSimilarity = v.SemanticSimilarity
			// "both" is a claim about the two component scores, so it is
			// made in the same branch that populates the second one —
			// unreachable today (vectorDuplicates always sets the field)
			// and it stays true if that ever changes.
			existing.MatchedBy = MatchedByBoth
		}
	}

	out := make([]DuplicateCandidate, 0, len(idx))
	for _, c := range idx {
		out = append(out, *c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Similarity != out[j].Similarity {
			return out[i].Similarity > out[j].Similarity
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// filterAndCap drops candidates below minSim and trims the slice to at
// most n entries. Operates on a fresh slice so the caller can safely
// pass a locally-mutated argument (no aliasing surprises).
func filterAndCap(hits []DuplicateCandidate, minSim float64, n int) []DuplicateCandidate {
	out := make([]DuplicateCandidate, 0, len(hits))
	for _, h := range hits {
		if h.Similarity >= minSim {
			out = append(out, h)
		}
	}
	if len(out) > n {
		out = out[:n]
	}
	return out
}
