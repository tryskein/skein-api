package dependencies

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// duplicateChecksTotal is the only way to see, from outside the process,
// which of the duplicate detector's two layers actually ran.
//
// It exists because sk-228 shipped a guard whose whole effect is
// invisible: on a cluster whose ctype cannot read the corpus's alphabet,
// the trigram layer now refuses to speak, and the detector silently rests
// on the embedder alone. Nothing in the response says so per deployment,
// and the only trace was a log line. The search subsystem already meters
// the analogous degradation (skein_search_requests_total{degraded}); this
// is the same instrument for the other retriever, so an operator can ask
// "is layer 1 alive on this cluster at all?" and "how often does an
// unreachable embedder leave us with no comparison whatsoever?" without
// grepping logs.
//
// @metric description Duplicate-detection checks served by dependencies.Store.CheckDuplicates, one increment per call (every skein_create that does not pass force/duplicateOf, plus every POST /issues/check-duplicates). Counts the check, not the candidates it found. A sustained non-zero rate on trigram_layer="ctype_cannot_read" or "mostly_unreadable" means pg_trgm cannot read this deployment's titles and detection is running on the semantic layer alone; degraded="true" with such a trigram_layer means neither layer compared the title and an empty candidate list must not be read as "clean".
// @metric calculation Incremented once per SERVED check, at each of the five exits of dependencies.Store.CheckDuplicates that return a report: empty title; decisive layer-1 short-circuit; semantic layer failed; semantic layer completed; embedder nil or unavailable. NOT incremented when the layer-1 query itself errors — that exit returns an error to the caller, so a spike in layer-1 SQL failures shows up as a DROP in this counter against request volume, not as a labelled value. The trigram_layer label is trigramLegibility.verdict() for the query title, measured in SQL on the same transaction as the comparison; degraded is the DuplicateReport.Degraded the caller receives.
// @label trigram_layer Layer-1 outcome for the query title. Values: "ran" (its verdict was used), "empty_title", "too_few_trigrams" (fewer than duplicateMinTrigrams), "mostly_unreadable" (visible characters below duplicateMinCoverage of the title), "ctype_cannot_read" (the database ctype discards non-ASCII characters and this title is not wholly ASCII)
// @label degraded The DuplicateReport.Degraded flag as the caller receives it: true when the semantic layer could not be consulted (embedder absent, unavailable, or failed), so the verdict rests on trigram alone or on nothing. It is NOT a record of whether layer 2 ran — a decisive layer-1 short-circuit also skips layer 2, and that is a confident answer carrying degraded="false". Values: "true", "false"
var duplicateChecksTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "skein_duplicate_checks_total",
		Help: "Duplicate-detection checks, by layer-1 outcome and whether the answer was degraded",
	},
	[]string{"trigram_layer", "degraded"},
)
