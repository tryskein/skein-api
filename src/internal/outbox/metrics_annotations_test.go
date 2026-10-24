package outbox

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// metrics_annotations_test.go — sk-303.13 / T012, extended in sk-303.14 / T021:
// the annotation gate for every custom metric IN THE MODULE.
//
// The project guidelines require each custom Prometheus metric to carry
// `@metric description`, `@metric calculation` and one `@label` per label, with
// Name / Help as string literals and labels declared inline, because
// go-metricy-extract reads the source with go/ast and silently DROPS a metric
// whose metadata it cannot resolve. A dropped metric is worse than an
// undocumented one: the registry then claims the metric does not exist.
//
// ⚠ CORRECTED MISTAKE OF JUSTIFICATION (review A/9, finding D; not erased).
// The previous edition of this header claimed that the published validator
// `go-metricy-extract` "is not installed in this environment", and on that
// basis declared the built-in gate a replacement for the turnstile. The claim
// was wrong: the validator is installed with one command (the executable
// path sits under `/cmd/...`, the module root does not contain it — probably
// the source of the old "unavailable" conclusion). The validator was run
// over this tree; the verdict was recorded when the change shipped. The validator itself
// is EXTERNAL to this repository and does not live in it: not in go.mod, not
// in the harness, not in CI jobs — so everything below works without it.
//
// The built-in gate stays, and stays not "instead of" but "on top of": the
// validator is in neither the integration harness nor CI jobs, so a one-off
// run of an external tool does not survive the next edit. The gate goes red
// on the next metric added without documentation, which a manual run does
// not.
//
// Five validator rules the built-in gate DID NOT KNOW (the same finding)
// it now has: histogram-unit-suffix, counter-total-suffix, name-snake-case,
// duplicate-name, type-consistency. One of them was firing right in this
// tree, OUTSIDE the debt ledger — see the entry for skein_outbox_batch_size.
//
// # Why a directory walk, not a list of them (T021 fix)
//
// Until this task the gate walked TWO directories listed by hand —
// `{".", "../notifications"}` — "the packages whose metrics this change
// introduces". A later change put metrics in a THIRD package (`internal/nudges`),
// the list did not learn of it, and the gate stayed green on a metric that
// had no annotations at all: the histogram of the worker's lag behind the
// schedule — the very one the task's turnstile requires to exist — arrived
// with directives glued to a neighbouring declaration and without a single
// one of its own. A hand-written list stayed silent exactly the way any
// hand-written list does.
//
// Directories are now FOUND: a walk of the module tree from its root,
// selected by the fact of a metric declaration in a non-test file. A new
// package with metrics enters the gate the moment it declares its first
// metric, and nobody's edit is required for that.
//
// # The debt ledger and why it cannot rot in silence
//
// The walk also finds metrics older than this gate that do not meet its
// requirements. They were not "excluded" — they are entered in `metricDebt`
// WITH A REASON, and the ledger is checked IN BOTH DIRECTIONS: an entry
// whose violation has disappeared goes red requiring it be removed; a
// violation with no entry goes red as ordinary. There is nowhere for the
// ledger to rot in silence: any disagreement with the tree is red.
//
// ⚠ CORRECTED MISTAKE (review A/9, finding C; not erased). The old ledger
// was BY PACKAGE and silenced a package WHOLESALE and ON EVERY RULE, even
// though the reasons of the entries are narrow: in internal/handler — a
// dispute about the name prefix, in internal/mcp/tools — one label.
// Mutation: a file was added in internal/handler with a metric without a
// single annotation and with a `userEmail` label valued "a@b.c" — the
// package stayed green. I.e. an unbounded-cardinality label carrying
// personal data passed a turnstile whose wording sounds like "labels are
// finite and carry no personal data".
//
// The ledger key is now a TRIPLE: package, metric variable and RULE
// IDENTIFIER (for label rules — together with the label name). A new
// metric in a package with debt is checked wholesale; a new label on a
// metric with debt is checked too. What can be silenced is exactly the
// named violation, and nothing beyond it.

// moduleRoot — the Go module root relative to this package's directory.
const moduleRoot = "../.."

// metricDebt — packages whose metrics predate this gate and do NOT meet its
// requirements. The key is a path from the module root; the value is the
// reason the debt is not closed by this task.
//
// This is not an exclusion list "to make it green": every entry is checked
// that the debt still exists (see the file header), and every one names
// what exactly is missing. They cannot be closed blindly — the description
// and calculation of a metric is written by whoever knows what it measures,
// and an invented description in the measurement registry is worse than an
// absent one: alert rules are written from it.
type debtKey struct {
	// Pkg — package path from the module root.
	Pkg string
	// Metric — the VARIABLE NAME of the declaration. The metric name is no
	// good as a key: a metric with an unreadable Name simply has none, and
	// that is exactly the metric that can end up in debt.
	Metric string
	// Rule — identifier of the broken rule. For label rules — together with
	// the label name, so an entry about one label does not silence the
	// others.
	Rule string
}

var metricDebt = map[debtKey]string{
	// ------------------------------------------------------------------
	// internal/search — nine search and embedding metrics from R3.
	//
	// Their semantics (what counts as a query, how "pending" differs from
	// "processed", what lands in "truncated") is known by the search owner.
	// An invented description in the measurement registry is worse than an
	// absent one: alert rules are written from it. The real validator
	// (go-metricy-extract --validate) reports 27 of its 27 errors on this
	// package — the debt is acknowledged by both checks the same way.
	// ------------------------------------------------------------------
	{"internal/search", "EmbeddingsErrorsTotal", ruleDescriptionRequired}:             searchDebtReason,
	{"internal/search", "EmbeddingsErrorsTotal", ruleCalculationRequired}:             searchDebtReason,
	{"internal/search", "EmbeddingsErrorsTotal", ruleLabelDescription + ":entity"}:    searchDebtReason,
	{"internal/search", "EmbeddingsErrorsTotal", ruleLabelDescription + ":reason"}:    searchDebtReason,
	{"internal/search", "EmbeddingsInputTokens", ruleDescriptionRequired}:             searchDebtReason,
	{"internal/search", "EmbeddingsInputTokens", ruleCalculationRequired}:             searchDebtReason,
	{"internal/search", "EmbeddingsInputTokens", ruleLabelDescription + ":entity"}:    searchDebtReason,
	{"internal/search", "EmbeddingsPending", ruleDescriptionRequired}:                 searchDebtReason,
	{"internal/search", "EmbeddingsPending", ruleCalculationRequired}:                 searchDebtReason,
	{"internal/search", "EmbeddingsPending", ruleLabelDescription + ":entity"}:        searchDebtReason,
	{"internal/search", "EmbeddingsProcessedTotal", ruleDescriptionRequired}:          searchDebtReason,
	{"internal/search", "EmbeddingsProcessedTotal", ruleCalculationRequired}:          searchDebtReason,
	{"internal/search", "EmbeddingsProcessedTotal", ruleLabelDescription + ":entity"}: searchDebtReason,
	{"internal/search", "EmbeddingsTruncatedTotal", ruleDescriptionRequired}:          searchDebtReason,
	{"internal/search", "EmbeddingsTruncatedTotal", ruleCalculationRequired}:          searchDebtReason,
	{"internal/search", "EmbeddingsTruncatedTotal", ruleLabelDescription + ":entity"}: searchDebtReason,
	{"internal/search", "QueryEmbeddingCacheHitsTotal", ruleDescriptionRequired}:      searchDebtReason,
	{"internal/search", "QueryEmbeddingCacheHitsTotal", ruleCalculationRequired}:      searchDebtReason,
	{"internal/search", "QueryEmbeddingCacheMissesTotal", ruleDescriptionRequired}:    searchDebtReason,
	{"internal/search", "QueryEmbeddingCacheMissesTotal", ruleCalculationRequired}:    searchDebtReason,
	{"internal/search", "SearchLatencySeconds", ruleDescriptionRequired}:              searchDebtReason,
	{"internal/search", "SearchLatencySeconds", ruleCalculationRequired}:              searchDebtReason,
	{"internal/search", "SearchLatencySeconds", ruleLabelDescription + ":kind"}:       searchDebtReason,
	{"internal/search", "SearchRequestsTotal", ruleDescriptionRequired}:               searchDebtReason,
	{"internal/search", "SearchRequestsTotal", ruleCalculationRequired}:               searchDebtReason,
	{"internal/search", "SearchRequestsTotal", ruleLabelDescription + ":degraded"}:    searchDebtReason,
	{"internal/search", "SearchRequestsTotal", ruleLabelDescription + ":kind"}:        searchDebtReason,
	{"internal/search", "EmbeddingsInputTokens", ruleHistogramUnitSuffix}:             searchTokensSuffixReason,

	// ------------------------------------------------------------------
	// internal/handler — two HTTP-tract metrics.
	// ------------------------------------------------------------------
	{"internal/handler", "httpRequestsTotal", ruleHouseNamePrefix}:                    handlerPrefixReason,
	{"internal/handler", "httpRequestDuration", ruleHouseNamePrefix}:                  handlerPrefixReason,
	{"internal/handler", "httpRequestsTotal", ruleHouseLabelEnumerates + ":method"}:   handlerMethodReason,
	{"internal/handler", "httpRequestDuration", ruleHouseLabelEnumerates + ":method"}: handlerMethodReason,
	{"internal/handler", "httpRequestDuration", ruleHouseLabelEnumerates + ":path"}:   handlerPathReason,

	// ------------------------------------------------------------------
	// internal/mcp/tools — the `tool` label.
	// ------------------------------------------------------------------
	{"internal/mcp/tools", "toolCallsTotal", ruleHouseLabelEnumerates + ":tool"}:  toolLabelReason,
	{"internal/mcp/tools", "toolErrorsTotal", ruleHouseLabelEnumerates + ":tool"}: toolLabelReason,

	// ------------------------------------------------------------------
	// internal/outbox — the batch-size histogram's name.
	// ------------------------------------------------------------------
	{"internal/outbox", "batchSize", ruleHistogramUnitSuffix}: batchSizeSuffixReason,
}

// Debt reasons. Lifted into constants rather than written on every entry:
// one reason covers a dozen same-shaped violations, and ten copies of it
// would drift on the first edit.
const (
	searchDebtReason = "search and embedding metrics arrived from R3 without `@metric description` / " +
		"`@metric calculation` and without `@label` lines. Their semantics is known by the search owner; " +
		"inventing it here would write a guess into the measurement registry from which " +
		"an alert rule would then be written."

	searchTokensSuffixReason = "skein_embeddings_input_tokens measures a TOKEN COUNT — a quantity " +
		"with no physical unit, and the rule's suffix list is physical units. " +
		"No honest suffix from the list exists for it, and attaching `_ratio` or `_bytes` " +
		"would lie about what the metric measures. The decision sits with the rest of this " +
		"package's debt, with its owner."

	handlerPrefixReason = "the names `http_requests_total` / `http_request_duration_seconds` " +
		"are prescribed verbatim by the \"Prometheus metrics\" section of the project guidelines; the house " +
		"`skein_` prefix contradicts that prescription. This is a convention dispute, not an annotation " +
		"edit, and it is settled at the guidelines level."

	handlerMethodReason = "the `method` label is HTTP methods; the set is finite and well-known, but " +
		"enumerating it in the annotation would be one more hand-written list. Settled together with " +
		"the prefix question, as one convention decision."

	handlerPathReason = "the `path` label is chi ROUTE TEMPLATES, not request paths: the set " +
		"is finite but grows with every new route. A list in the annotation would rot by " +
		"the next endpoint; the right fix is to derive it from the route table, and that " +
		"is separate work."

	toolLabelReason = "the `tool` label enumerates not values but a description: there are " +
		"forty-odd tool names. A list in the annotation would be a hand-written list, silent " +
		"on a miss; the right fix is to derive it from the tool registry."

	batchSizeSuffixReason = "skein_outbox_batch_size measures the NUMBER OF EVENTS in a batch — a quantity " +
		"with no physical unit, and the rule requires a suffix from a list of physical units " +
		"(_seconds, _bytes, _ratio…). There is no honest suffix for an event count there. " +
		"RENAMING WAS REJECTED ON PURPOSE (found in review): the metric has existed since " +
		"an early release, dashboards and alert rules are already written against it, and a rename breaks them in silence " +
		"— the cost is above the benefit of a suffix that would not be truthful anyway. " +
		"The entry exists so the violation is VISIBLE on every run, not invisible."
)

// metricDirs returns the module's directories that declare metrics, in a
// stable order. A directory lands here by the FACT of a declaration, not
// by an entry in a list.
func metricDirs(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(moduleRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// ⚠ THE THIRD FORM (review A/9, finding P-6). `promauto.With(reg).NewX(...)`
		// contains neither "promauto.New" nor "prometheus.New", and a package that
		// declares metrics ONLY that way did not enter the walk at all — even
		// though the form is named as supported in the project guidelines, and this
		// file's header promises "every metric of the module". The promise was
		// stronger than the check.
		if !metricFormInSource(string(src)) {
			return nil
		}
		dir := filepath.Dir(path)
		if !slices.Contains(out, dir) {
			out = append(out, dir)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module tree: %v", err)
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatal("the walk found no directory with metrics — the walk is broken, not the code")
	}
	return out
}

// metricFormInSource — a coarse filter of files that declare metrics. All
// three supported constructor forms; go/ast parses them further.
func metricFormInSource(src string) bool {
	for _, form := range []string{"promauto.New", "prometheus.New", "promauto.With("} {
		if strings.Contains(src, form) {
			return true
		}
	}
	return false
}

// metricConstructorReceiver reports whether a supported metric constructor
// stands behind the selector `X.NewY(...)`.
//
// There are three forms, and the third is why this function exists (review
// A/9, finding P-6):
//
//	prometheus.NewCounterVec(...)        // X is an identifier
//	promauto.NewCounterVec(...)          // X is an identifier
//	promauto.With(reg).NewCounterVec(...) // X is a CALL to promauto.With(reg)
//
// The previous edition required X to be *ast.Ident and dropped the third
// form in silence: a metric with no annotation and a `userEmail` label
// passed a gate whose wording sounds like "every metric of the module".
func metricConstructorReceiver(x ast.Expr) bool {
	switch recv := x.(type) {
	case *ast.Ident:
		return recv.Name == "promauto" || recv.Name == "prometheus"
	case *ast.CallExpr:
		// promauto.With(reg) — itself a selector with a package identifier.
		sel, ok := recv.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "With" {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		return ok && pkg.Name == "promauto"
	}
	return false
}

// relToModule translates a directory path into a path from the module root
// — in the form directories are recorded in metricDebt.
func relToModule(t *testing.T, dir string) string {
	t.Helper()
	rel, err := filepath.Rel(moduleRoot, dir)
	if err != nil {
		t.Fatalf("relative path %s: %v", dir, err)
	}
	return filepath.ToSlash(rel)
}

// metricConstructors — the promauto/prometheus constructors the extractor
// understands, and the KIND of metric each of them declares. The kind is
// needed by the name-suffix rules: a counter and a histogram have different
// ones.
var metricConstructors = map[string]string{
	"NewCounter": "counter", "NewCounterVec": "counter",
	"NewGauge": "gauge", "NewGaugeVec": "gauge",
	"NewHistogram": "histogram", "NewHistogramVec": "histogram",
	"NewSummary": "summary", "NewSummaryVec": "summary",
}

type declaredMetric struct {
	varName string
	// dir — the package directory the metric is declared in: the gate
	// checks the debt ledger against it.
	dir  string
	file string
	line int
	doc  string
	name string
	help string
	// kind — the metric's kind: counter / gauge / histogram / summary.
	kind   string
	labels []string
	// literalName / literalHelp record whether the extractor could read them.
	literalName bool
	literalHelp bool
}

// problem — one violation: a rule identifier and human text.
//
// The identifier exists for the debt ledger: what can be silenced is
// exactly the NAMED violation of the NAMED metric, not everything found
// in the package.
type problem struct {
	rule string
	text string
}

// Rule identifiers. Those matching go-metricy-extract rules in meaning are
// named the same — so the external validator's verdict and this gate's
// verdict read as one vocabulary. Rules the validator does not have (house
// conventions) carry the house prefix.
const (
	ruleNonLiteralMetadata   = "metric.non-literal-metadata"
	ruleDescriptionRequired  = "metric.description-required"
	ruleCalculationRequired  = "metric.calculation-required"
	ruleLabelDescription     = "metric.label-description-required"
	ruleHistogramUnitSuffix  = "metric.histogram-unit-suffix"
	ruleCounterTotalSuffix   = "metric.counter-total-suffix"
	ruleNameSnakeCase        = "metric.name-snake-case"
	ruleHouseNamePrefix      = "house.name-prefix"
	ruleHouseLabelPhantom    = "house.label-phantom"
	ruleHouseLabelEnumerates = "house.label-enumerates-values"
)

// histogramUnitSuffixes — suffixes go-metricy-extract accepts on a
// histogram. The list is theirs, not ours: a disagreement would make the
// two checks' verdicts incomparable.
var histogramUnitSuffixes = []string{
	"_seconds", "_milliseconds", "_microseconds", "_nanoseconds",
	"_bytes", "_kilobytes", "_megabytes",
	"_ratio", "_percent", "_fraction", "_bits", "_celsius", "_meters",
}

// snakeCaseName — a metric name of lowercase letters, digits and underscores.
var snakeCaseName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// metricProblems returns every rule this metric breaks.
//
// Pure: it decides nothing about whose fault it is and reports nothing to the
// framework. That separation is what lets the same rules run over a metric
// that must be clean and over one carrying documented debt.
func metricProblems(m declaredMetric) []problem {
	var probs []problem
	add := func(rule, format string, args ...any) {
		probs = append(probs, problem{rule: rule, text: fmt.Sprintf(format, args...)})
	}

	if !m.literalName {
		add(ruleNonLiteralMetadata, "metric Name is not a string literal — the extractor cannot read it and drops the metric")
	}
	if !m.literalHelp {
		add(ruleNonLiteralMetadata, "metric Help is not a string literal")
	}
	if !strings.HasPrefix(m.name, "skein_") {
		add(ruleHouseNamePrefix, "metric name %q does not follow the skein_<area>_<measure> convention", m.name)
	}
	if m.name != "" && !snakeCaseName.MatchString(m.name) {
		add(ruleNameSnakeCase, "metric name %q is not snake_case", m.name)
	}
	if m.kind == "counter" && !strings.HasSuffix(m.name, "_total") {
		add(ruleCounterTotalSuffix, "counter %q does not end with _total", m.name)
	}
	if m.kind == "histogram" && !hasAnySuffix(m.name, histogramUnitSuffixes) {
		add(ruleHistogramUnitSuffix,
			"histogram %q does not end with a unit suffix (%s) — the registry cannot tell what its buckets measure",
			m.name, strings.Join(histogramUnitSuffixes, ", "))
	}
	desc := directiveValue(m.doc, "@metric description")
	calc := directiveValue(m.doc, "@metric calculation")
	if len(desc) < 60 {
		add(ruleDescriptionRequired, "@metric description is missing or too short (%d chars) — «Counter for X» is not a description", len(desc))
	}
	if len(calc) < 40 {
		add(ruleCalculationRequired, "@metric calculation is missing or too short (%d chars) — it must say WHERE in the code the metric moves and on WHAT event", len(calc))
	}
	documented := labelDirectives(m.doc)
	for _, l := range m.labels {
		if _, ok := documented[l]; !ok {
			add(ruleLabelDescription+":"+l, "label %q has no `@label %s <description>` line", l, l)
		}
	}
	for l := range documented {
		if !contains(m.labels, l) {
			add(ruleHouseLabelPhantom+":"+l, "`@label %s` documents a label the metric does not have", l)
		}
	}
	// The cardinality rule: a label whose description never says what can
	// appear in it is a cardinality bomb, and the registry cannot tell it from
	// a bounded one without reading the implementation.
	for name, text := range documented {
		if !contains(m.labels, name) {
			continue
		}
		if !enumeratesValues(text) {
			add(ruleHouseLabelEnumerates+":"+name, "label %q does not enumerate its values — a label without a bounded value set is a cardinality bomb", name)
		}
	}
	sort.Slice(probs, func(i, j int) bool {
		if probs[i].rule != probs[j].rule {
			return probs[i].rule < probs[j].rule
		}
		return probs[i].text < probs[j].text
	})
	return probs
}

// hasAnySuffix — whether the name has any of the listed suffixes.
func hasAnySuffix(name string, suffixes []string) bool {
	for _, suf := range suffixes {
		if strings.HasSuffix(name, suf) {
			return true
		}
	}
	return false
}

// TestMetricsCarryTheirAnnotations is the gate, over EVERY metric of the
// module.
//
// A violation NAMED in the debt ledger (package + metric + rule) is written
// to the run log; any other is red. A package is not silenced wholesale by
// anything: that is exactly how the previous edition let a new unbounded-
// cardinality label through in a package whose debt was about something
// else.
func TestMetricsCarryTheirAnnotations(t *testing.T) {
	t.Parallel()
	metrics := collectMetrics(t)
	if len(metrics) == 0 {
		t.Fatal("no metrics found — the scan is broken, not the code")
	}

	for _, m := range metrics {
		pkg := relToModule(t, m.dir)
		t.Run(pkg+"/"+m.varName, func(t *testing.T) {
			for _, prob := range metricProblems(m) {
				key := debtKey{Pkg: pkg, Metric: m.varName, Rule: prob.rule}
				if why, indebt := metricDebt[key]; indebt {
					t.Logf("%s:%d: KNOWN DEBT [%s] %s\n    reason: %s",
						m.file, m.line, prob.rule, prob.text, why)
					continue
				}
				t.Errorf("%s:%d: [%s] %s", m.file, m.line, prob.rule, prob.text)
			}
		})
	}
}

// TestMetricDebtLedgerMatchesTheTree — the ledger checked against reality, in
// BOTH directions. Without this the ledger is just a suppression list, and a
// suppression list is the shape a rule dies in.
//
// The "violation without an entry" direction is held by the gate above.
// Here is the reverse: an entry whose violation has disappeared. It is more
// dangerous than it looks: with a triple key such an entry hides exactly
// one rule of one metric, but hides it in silence and forever.
func TestMetricDebtLedgerMatchesTheTree(t *testing.T) {
	t.Parallel()
	live := map[debtKey]bool{}
	known := map[string]bool{}
	for _, m := range collectMetrics(t) {
		pkg := relToModule(t, m.dir)
		known[pkg+"/"+m.varName] = true
		for _, prob := range metricProblems(m) {
			live[debtKey{Pkg: pkg, Metric: m.varName, Rule: prob.rule}] = true
		}
	}

	for key, why := range metricDebt {
		if !known[key.Pkg+"/"+key.Metric] {
			t.Errorf("the debt ledger names metric %s/%s, but it is not in the tree — "+
				"the entry outlived its subject: %s", key.Pkg, key.Metric, why)
			continue
		}
		if !live[key] {
			t.Errorf("the debt ledger holds rule %q on %s/%s, but the violation is gone — "+
				"remove the entry, otherwise it will hide the NEXT violation of this rule in silence",
				key.Rule, key.Pkg, key.Metric)
		}
	}
}

// TestMetricNamesAreUniqueAndConsistentlyTyped — two validator rules that
// cannot be checked on one metric: duplicate-name and type-consistency.
// Both ills are global over the snapshot — one name raised twice, and one
// name raised as two different kinds — and both break not the annotation
// but the measurement registry itself: a snapshot with two entries under
// one name has nowhere to be accepted.
//
// Unlike the rules above, they have no debt ledger and must not: this is
// not an "unfinished description", it is a contradiction fixed by renaming
// one of the two metrics and by nothing else.
func TestMetricNamesAreUniqueAndConsistentlyTyped(t *testing.T) {
	t.Parallel()
	type origin struct {
		where string
		kind  string
	}
	seen := map[string]origin{}
	checked := 0
	for _, m := range collectMetrics(t) {
		if !m.literalName || m.name == "" {
			continue
		}
		checked++
		where := fmt.Sprintf("%s:%d (%s)", m.file, m.line, m.varName)
		prev, dup := seen[m.name]
		if !dup {
			seen[m.name] = origin{where: where, kind: m.kind}
			continue
		}
		if prev.kind != m.kind {
			t.Errorf("[%s] name %q is raised as two kinds: %s (%s) and %s (%s) — "+
				"a registry snapshot with that name is contradictory, and Prometheus will refuse "+
				"to register the second metric in the same process",
				"metric.type-consistency", m.name, prev.where, prev.kind, where, m.kind)
			continue
		}
		t.Errorf("[%s] name %q is declared twice: %s and %s — the measurement registry has nowhere "+
			"to learn which of the two is described by its annotation",
			"metric.duplicate-name", m.name, prev.where, where)
	}
	if checked == 0 {
		t.Fatal("no metric name was read — the check is tautologically green")
	}
}

// collectMetrics parses the metric declarations of every package in the module
// that declares one.
func collectMetrics(t *testing.T) []declaredMetric {
	t.Helper()
	return collectMetricsIn(t, metricDirs(t))
}

// collectMetricsIn parses the metric declarations of the named directories.
func collectMetricsIn(t *testing.T, dirs []string) []declaredMetric {
	t.Helper()
	var out []declaredMetric
	fset := token.NewFileSet()
	for _, dir := range dirs {
		pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
			return !strings.HasSuffix(fi.Name(), "_test.go")
		}, parser.ParseComments)
		if err != nil {
			t.Fatalf("parsing %s: %v", dir, err)
		}
		for _, pkg := range pkgs {
			for path, file := range pkg.Files {
				for _, decl := range file.Decls {
					gd, ok := decl.(*ast.GenDecl)
					if !ok || gd.Tok != token.VAR {
						continue
					}
					for _, spec := range gd.Specs {
						vs, ok := spec.(*ast.ValueSpec)
						if !ok {
							continue
						}
						for i, val := range vs.Values {
							call, ok := val.(*ast.CallExpr)
							if !ok {
								continue
							}
							sel, ok := call.Fun.(*ast.SelectorExpr)
							if !ok {
								continue
							}
							kind, isMetric := metricConstructors[sel.Sel.Name]
							if !isMetric {
								continue
							}
							if !metricConstructorReceiver(sel.X) {
								continue
							}
							pos := fset.Position(call.Pos())
							m := declaredMetric{
								varName: vs.Names[i].Name,
								dir:     dir,
								file:    path,
								line:    pos.Line,
								kind:    kind,
								doc:     docText(vs.Doc, gd.Doc, vs.Comment),
							}
							parseMetricArgs(call, &m)
							out = append(out, m)
						}
					}
				}
			}
		}
	}
	return out
}

// parseMetricArgs reads Name / Help out of the Opts literal and the label
// names out of the inline []string{...} argument.
func parseMetricArgs(call *ast.CallExpr, m *declaredMetric) {
	for _, arg := range call.Args {
		switch a := arg.(type) {
		case *ast.CompositeLit:
			switch lit := a.Type.(type) {
			case *ast.SelectorExpr: // prometheus.CounterOpts{...}
				_ = lit
				for _, el := range a.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := kv.Key.(*ast.Ident)
					if !ok {
						continue
					}
					s, isLit := stringLit(kv.Value)
					switch key.Name {
					case "Name":
						m.name, m.literalName = s, isLit
					case "Help":
						m.help, m.literalHelp = s, isLit
					}
				}
			case *ast.ArrayType: // []string{"a","b"}
				for _, el := range a.Elts {
					if s, ok := stringLit(el); ok {
						m.labels = append(m.labels, s)
					}
				}
			}
		}
	}
}

func stringLit(e ast.Expr) (string, bool) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(bl.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

// docText joins whichever comment groups carry the declaration's doc.
func docText(groups ...*ast.CommentGroup) string {
	var b strings.Builder
	for _, g := range groups {
		if g == nil {
			continue
		}
		b.WriteString(g.Text())
		b.WriteString("\n")
	}
	return b.String()
}

// directiveValue returns the text following a one-line directive.
func directiveValue(doc, directive string) string {
	for line := range strings.SplitSeq(doc, "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, directive); ok {
			return strings.TrimSpace(after)
		}
	}
	return ""
}

// labelDirectives maps each documented label name to its description text.
func labelDirectives(doc string) map[string]string {
	out := map[string]string{}
	for line := range strings.SplitSeq(doc, "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, "@label ")
		if !ok {
			continue
		}
		name, text, found := strings.Cut(strings.TrimSpace(rest), " ")
		if !found {
			out[name] = ""
			continue
		}
		out[name] = strings.TrimSpace(text)
	}
	return out
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// enumeratesValues reports whether a label's description actually names the
// values the label can take.
//
// Two forms are accepted because both are already in use and both answer the
// question a reader has: an explicit "Values: …" list, or at least two quoted
// literals anywhere in the sentence ("Classification: \"tx_begin\", \"db_query\",
// …"). What is NOT accepted is prose that describes the label without ever
// saying what can appear in it — that is the shape a cardinality bomb hides in.
func enumeratesValues(text string) bool {
	if strings.Contains(text, "Values:") || strings.Contains(text, "values:") {
		return true
	}
	return strings.Count(text, `"`) >= 4
}
