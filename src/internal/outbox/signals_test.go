package outbox

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"skein-api/internal/notifications"
)

// signals_test.go — sk-303.13 / T012, coverage-matrix row 24: the UNIT gate
// on observability. No database anywhere in this file.
//
// Three claims, and each is checked against something that can actually drift:
//
//  1. every runtime class of this wave has its OWN address in the registry —
//     a distinct metric family, or a distinct label value inside one;
//  2. the value sets the annotations promise are the value sets the CODE can
//     produce — the check compares documentation against constants, so adding
//     a reason to the registry without updating the label docs fails here;
//  3. no log call site in the touched packages passes an expression that
//     carries prose, a device endpoint or a subscription secret.
//
// The behaviour behind these signals is proven elsewhere (row 20 owns the
// delivery outcomes). What this file guards is that the signals stay TELLABLE
// APART and stay safe to print.

// waveSignal is one runtime class the spec requires to be observable.
type waveSignal struct {
	class  string
	family string
	// label / value address the class inside a shared family; empty when the
	// family is the whole signal.
	label string
	value string
}

// waveSignals enumerates the classes named in the acceptance of row 24.
var waveSignals = []waveSignal{
	{class: "fanout prefs-read failure", family: "skein_notifications_fanout_prefs_unavailable_total"},
	{class: "unmapped deliverable event", family: "skein_notifications_fanout_unmapped_events_total"},
	{class: "delivery prefs-unavailable", family: "skein_outbox_push_outcomes_total", label: "outcome", value: outcomePrefsUnavailable},
	{class: "no-device", family: "skein_outbox_push_outcomes_total", label: "outcome", value: outcomeNoDevice},
	{class: "access-revoked", family: "skein_outbox_push_outcomes_total", label: "outcome", value: outcomeAccessRevoked},
	{class: "configuration-no-send", family: "skein_outbox_push_outcomes_total", label: "outcome", value: outcomeConfigurationNoSend},
	{class: "transport-exhausted", family: "skein_outbox_push_outcomes_total", label: "outcome", value: outcomeTransportExhausted},
	{class: "transport-retry", family: "skein_outbox_push_retries_total"},
	{class: "cleanup", family: "skein_outbox_push_cleanup_deleted_total"},
}

// TestSignals_EveryClassHasItsOwnAddress — "tellable apart". Two classes sharing an
// address is the failure this test exists to catch: it is invisible in code
// review and fatal in an incident, because the operator cannot tell which of
// the two happened.
func TestSignals_EveryClassHasItsOwnAddress(t *testing.T) {
	// Touch every series so the registry knows it exists even at zero.
	pushOutcomesTotal.WithLabelValues("assignment", outcomeSuccess, causeNone)
	for _, s := range waveSignals {
		if s.label == "outcome" {
			pushOutcomesTotal.WithLabelValues("assignment", s.value, causeNone)
		}
	}
	pushRetriesTotal.WithLabelValues("assignment")

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	present := map[string]bool{}
	for _, f := range families {
		present[f.GetName()] = true
	}

	seen := map[string]string{}
	for _, s := range waveSignals {
		if !present[s.family] {
			t.Errorf("class %q has no metric family %q in the registry — the signal is silence",
				s.class, s.family)
			continue
		}
		addr := s.family + "|" + s.label + "=" + s.value
		if prev, dup := seen[addr]; dup {
			t.Errorf("classes %q and %q share the address %s — they cannot be told apart",
				prev, s.class, addr)
		}
		seen[addr] = s.class
	}
	if len(seen) != len(waveSignals) {
		t.Errorf("distinct signal addresses = %d, want %d", len(seen), len(waveSignals))
	}
}

// TestSignals_DocumentedLabelValuesCoverWhatTheCodeProduces — the finiteness
// claim, checked in the direction that can actually break.
//
// An `@label … Values: …` line is a PROMISE about cardinality, and the registry
// (and whoever writes an alert) reads it as one. The way it goes stale is not
// somebody inventing a value at runtime — it is somebody adding a constant, or
// a reason to the registry, and not revisiting the annotation. So the test
// reads the constants and asks whether each is documented.
func TestSignals_DocumentedLabelValuesCoverWhatTheCodeProduces(t *testing.T) {
	docs := metricDocs(t)

	// The two enumerations are READ OUT OF THE PACKAGE SOURCE, not written
	// here — see outcome_set_test.go. A hand-written copy of the closed set is
	// exactly what let sk-303.14 add `superseded` and `digest_empty` to the
	// code while this guard, whose whole job is to notice that, stayed green.
	outcomeDoc := docs["pushOutcomesTotal"]["outcome"]
	for _, v := range allOutcomeValues(t) {
		if !strings.Contains(outcomeDoc, v) {
			t.Errorf("outcome %q is produced by the code but not enumerated in its @label line", v)
		}
	}

	causeDoc := docs["pushOutcomesTotal"]["cause"]
	for _, v := range allNamedCauseValues(t) {
		if !strings.Contains(causeDoc, v) {
			t.Errorf("cause %q is produced by the code but not enumerated in its @label line", v)
		}
	}

	// The reason label is bounded by the registry, which is a closed list — so
	// the enumeration must track it. This is the line that fails when a new
	// reason is added upstream, which is exactly when it should.
	for _, metric := range []string{"pushOutcomesTotal", "pushDeliveriesTotal", "pushRetriesTotal"} {
		reasonDoc := docs[metric]["reason"]
		if reasonDoc == "" {
			t.Errorf("%s has no @label reason line", metric)
			continue
		}
		for _, r := range notifications.AllReasons() {
			// A reason with no push channel can never label a push metric.
			if !r.Push.Applicable {
				continue
			}
			if !strings.Contains(reasonDoc, string(r.Key)) {
				t.Errorf("%s: reason %q has a push channel but is not enumerated in the @label reason line",
					metric, r.Key)
			}
		}
		if !strings.Contains(reasonDoc, "unclassified") {
			t.Errorf("%s: the @label reason line does not mention the \"unclassified\" fallback the code emits for a row with no reason",
				metric)
		}
	}
}

// metricDocs indexes the label documentation of THIS package's metrics:
// metricVar → label → description text.
//
// Scoped to the package on purpose. The annotation gate now walks the whole
// module (metricDirs), and across the module variable names are not unique —
// `cleanupRunsTotal` exists in both internal/attachments and
// internal/monitorcreds. A module-wide index keyed by variable name would let
// one package's annotations answer for another's, silently, and the caller
// below reads the answer as if it were about pushOutcomesTotal.
func metricDocs(t *testing.T) map[string]map[string]string {
	t.Helper()
	out := map[string]map[string]string{}
	for _, m := range collectMetricsIn(t, []string{"."}) {
		if prev, dup := out[m.varName]; dup {
			t.Fatalf("two package metrics are declared under the name %s (%v and %v) — the index by variable name is no longer unique",
				m.varName, prev, labelDirectives(m.doc))
		}
		out[m.varName] = labelDirectives(m.doc)
	}
	return out
}

// forbiddenLogArgs are expressions that must never be handed to a log call in
// the packages this wave touches. Each is one of the three things the task
// names: prose (an issue title or a comment body), a delivery address, or a
// subscription secret.
var forbiddenLogArgs = []string{
	".Title", ".title", "ei.title",
	".Endpoint", "sub.Endpoint", "endpoint",
	"p256dh", "Auth", "payload", "Payload",
	".Body", "body",
}

// logScanDirs are the packages whose log call sites this wave owns.
var logScanDirs = []string{".", "../notifications"}

// TestSignals_LogCallSitesCarryNoProseOrSecrets — the PII rule as a STRUCTURAL
// gate rather than an output assertion.
//
// An output assertion only covers the paths a test happens to drive; this
// covers every log call in the packages, including the ones nobody exercised.
// It reads the argument EXPRESSIONS, because that is where a leak is
// introduced — somebody "adds context" to a failure record and reaches for the
// nearest variable, which is the title or the subscription.
//
// The error-text case is deliberately exempted and handled by redaction
// instead: a leak inside an error somebody else formatted cannot be caught by
// looking at the expression.
func TestSignals_LogCallSitesCarryNoProseOrSecrets(t *testing.T) {
	fset := token.NewFileSet()
	checked := 0
	for _, dir := range logScanDirs {
		pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
			return !strings.HasSuffix(fi.Name(), "_test.go")
		}, parser.ParseComments)
		if err != nil {
			t.Fatalf("parsing %s: %v", dir, err)
		}
		for _, pkg := range pkgs {
			for path, file := range pkg.Files {
				ast.Inspect(file, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					recv, ok := sel.X.(*ast.Ident)
					if !ok || recv.Name != "slog" {
						return true
					}
					checked++
					for _, arg := range call.Args {
						// String LITERALS are the message and the field names;
						// a literal cannot carry a runtime value, and matching
						// them flags honest text like "payload marshal failed".
						// The rule is about EXPRESSIONS that read data.
						if lit, isLit := arg.(*ast.BasicLit); isLit && lit.Kind == token.STRING {
							continue
						}
						expr := exprText(fset, arg)
						for _, bad := range forbiddenLogArgs {
							if !strings.Contains(expr, bad) {
								continue
							}
							// redactEndpoints(...) is the sanctioned way to
							// pass error text that may quote an address.
							if strings.HasPrefix(expr, "redactEndpoints(") {
								continue
							}
							pos := fset.Position(arg.Pos())
							t.Errorf("%s:%d: log argument %q contains %q — prose, delivery addresses and subscription secrets have no place in a log record",
								path, pos.Line, expr, bad)
						}
					}
					return true
				})
			}
		}
	}
	if checked == 0 {
		t.Fatal("no slog call sites found — the scan is broken, not the code")
	}
	t.Logf("scanned %d log call sites across %v", checked, logScanDirs)
}

// exprText renders an expression back to source-ish text for matching.
func exprText(fset *token.FileSet, e ast.Expr) string {
	var b strings.Builder
	writeExpr(&b, e)
	_ = fset
	return b.String()
}

func writeExpr(b *strings.Builder, e ast.Expr) {
	switch x := e.(type) {
	case *ast.Ident:
		b.WriteString(x.Name)
	case *ast.BasicLit:
		b.WriteString(x.Value)
	case *ast.SelectorExpr:
		writeExpr(b, x.X)
		b.WriteString(".")
		b.WriteString(x.Sel.Name)
	case *ast.CallExpr:
		writeExpr(b, x.Fun)
		b.WriteString("(")
		for i, a := range x.Args {
			if i > 0 {
				b.WriteString(", ")
			}
			writeExpr(b, a)
		}
		b.WriteString(")")
	case *ast.IndexExpr:
		writeExpr(b, x.X)
		b.WriteString("[")
		writeExpr(b, x.Index)
		b.WriteString("]")
	case *ast.StarExpr:
		b.WriteString("*")
		writeExpr(b, x.X)
	case *ast.UnaryExpr:
		b.WriteString(x.Op.String())
		writeExpr(b, x.X)
	case *ast.BinaryExpr:
		writeExpr(b, x.X)
		b.WriteString(" " + x.Op.String() + " ")
		writeExpr(b, x.Y)
	default:
		b.WriteString("<expr>")
	}
}
