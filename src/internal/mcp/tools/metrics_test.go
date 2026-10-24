package tools

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/testutil"

	skmcp "skein-api/internal/mcp"
)

// callsFor / errorsFor read one child of each counter. Tests use a unique
// fake tool name per case, so the absolute value is the number of calls
// that case made — no before/after arithmetic and no cross-test bleed.
//
// The `action` label is pinned to "" here because these cases all go
// through addTool's wrapper (instrumentTool), and "" is precisely what
// that wrapper must write: the marker of a tool that is not consolidated.
// Consolidated tools are read with callsForAction below.
func callsFor(t *testing.T, tool, outcome string) float64 {
	t.Helper()
	return callsForAction(t, tool, "", outcome)
}

func errorsFor(t *testing.T, tool, code string) float64 {
	t.Helper()
	return errorsForAction(t, tool, "", code)
}

func callsForAction(t *testing.T, tool, action, outcome string) float64 {
	t.Helper()
	return testutil.ToFloat64(toolCallsTotal.WithLabelValues(tool, action, outcome))
}

func errorsForAction(t *testing.T, tool, action, code string) float64 {
	t.Helper()
	return testutil.ToFloat64(toolErrorsTotal.WithLabelValues(tool, action, code))
}

// TestInstrumentTool_Outcomes is the core assertion of sk-256: a business
// failure and a success must not land in the same bucket. Skein returns
// business failures as a JSON envelope inside a tool result rather than as
// a transport error, so a counter that only watched the error return would
// score every validation_error as a success and answer nothing.
func TestInstrumentTool_Outcomes(t *testing.T) {
	tests := []struct {
		name        string
		tool        string
		res         *mcpgo.CallToolResult
		err         error
		wantOutcome string
		wantCode    string
	}{
		{
			name:        "plain JSON success",
			tool:        "fake_ok_json",
			res:         mcpgo.NewToolResultText(`{"id":"sk-1","status":"open"}`),
			wantOutcome: outcomeOK,
		},
		{
			name:        "non-JSON success (markdown guide)",
			tool:        "fake_ok_text",
			res:         mcpgo.NewToolResultText("# Skein guide\n\nnot JSON at all"),
			wantOutcome: outcomeOK,
		},
		{
			name:        "business error: validation_error",
			tool:        "fake_validation",
			res:         skmcp.ToolError(ErrCodeValidationError, "id is required", nil),
			wantOutcome: outcomeBusinessError,
			wantCode:    ErrCodeValidationError,
		},
		{
			name:        "business error with extra fields: version_conflict",
			tool:        "fake_conflict",
			res:         skmcp.ToolError(ErrCodeVersionConflict, "stale", map[string]any{"currentVersion": 7}),
			wantOutcome: outcomeBusinessError,
			wantCode:    ErrCodeVersionConflict,
		},
		{
			name:        "business error: forbidden",
			tool:        "fake_forbidden",
			res:         skmcp.ToolError(ErrCodeForbidden, "not a member", nil),
			wantOutcome: outcomeBusinessError,
			wantCode:    ErrCodeForbidden,
		},
		{
			// Regression for the sk-256 review: this code is declared for
			// skein_theme(action="preview_variables") and was landing in
			// code="other"
			// because it sat outside errors.go and outside the allowlist.
			name:        "business error: preview_unavailable",
			tool:        "fake_preview_unavailable",
			res:         skmcp.ToolError(ErrCodePreviewUnavailable, "NATS_URL not configured", nil),
			wantOutcome: outcomeBusinessError,
			wantCode:    ErrCodePreviewUnavailable,
		},
		{
			name:        "internal_error envelope is not a business failure",
			tool:        "fake_internal",
			res:         skmcp.ToolError(ErrCodeInternalError, "db down", nil),
			wantOutcome: outcomeInternalError,
			wantCode:    ErrCodeInternalError,
		},
		{
			name:        "handler error is transport-level",
			tool:        "fake_transport",
			err:         errors.New("marshal tool result: boom"),
			wantOutcome: outcomeInternalError,
			wantCode:    codeTransportError,
		},
		{
			name:        "nil result with nil error",
			tool:        "fake_nil",
			wantOutcome: outcomeInternalError,
			wantCode:    codeNilResult,
		},
		{
			name:        "unknown code collapses to other",
			tool:        "fake_unknown_code",
			res:         skmcp.ToolError("some_code_nobody_declared", "?", nil),
			wantOutcome: outcomeBusinessError,
			wantCode:    codeOther,
		},
		{
			// IsError set, envelope missing: mcp.ToolError always writes
			// both, so this is a raw NewToolResultError that skipped the
			// helper. Not a business failure and not an internal failure —
			// a shape defect, and it must be distinguishable from the
			// opposite mismatch below.
			name:        "error flag without a parsable envelope",
			tool:        "fake_bare_error",
			res:         mcpgo.NewToolResultError("plain text failure"),
			wantOutcome: outcomeContractViolation,
			wantCode:    codeFlagWithoutEnvelope,
		},
		{
			// The mismatch the sk-256 review called out: a *successful*
			// result whose payload happens to carry a top-level string
			// `error`. Classifying on the envelope alone scored this a
			// business_error and quietly corrupted the per-code split.
			name:        "envelope-shaped success payload without the error flag",
			tool:        "fake_envelope_no_flag",
			res:         mcpgo.NewToolResultText(`{"error":"validation_error","message":"looks like an envelope"}`),
			wantOutcome: outcomeContractViolation,
			wantCode:    codeEnvelopeWithoutFlag,
		},
		{
			// Same mismatch with a code that is not even in the allowlist:
			// still one bucket. When the two halves disagree the envelope
			// code is not trustworthy enough to label with.
			name:        "envelope-shaped success payload with an unknown code",
			tool:        "fake_envelope_no_flag_unknown",
			res:         mcpgo.NewToolResultText(`{"error":"whatever_nobody_declared"}`),
			wantOutcome: outcomeContractViolation,
			wantCode:    codeEnvelopeWithoutFlag,
		},
		{
			name:        "success payload with a nested per-row error object stays ok",
			tool:        "fake_batch_partial",
			res:         mcpgo.NewToolResultText(`{"results":[{"id":"sk-1","error":{"code":"issue_not_found"}}]}`),
			wantOutcome: outcomeOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := instrumentTool(tt.tool, func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				return tt.res, tt.err
			})

			gotRes, gotErr := h(context.Background(), mcpgo.CallToolRequest{})

			// The wrapper must be transparent.
			if gotRes != tt.res {
				t.Errorf("result was not passed through: got %v, want %v", gotRes, tt.res)
			}
			if !errors.Is(gotErr, tt.err) {
				t.Errorf("error was not passed through: got %v, want %v", gotErr, tt.err)
			}

			if got := callsFor(t, tt.tool, tt.wantOutcome); got != 1 {
				t.Errorf("calls_total{tool=%q,outcome=%q} = %v, want 1", tt.tool, tt.wantOutcome, got)
			}
			// Nothing may leak into a second outcome bucket.
			for _, other := range []string{
				outcomeOK, outcomeBusinessError, outcomeInternalError,
				outcomeContractViolation, outcomePanic,
			} {
				if other == tt.wantOutcome {
					continue
				}
				if got := callsFor(t, tt.tool, other); got != 0 {
					t.Errorf("calls_total{tool=%q,outcome=%q} = %v, want 0", tt.tool, other, got)
				}
			}

			if tt.wantCode == "" {
				if got := errorsFor(t, tt.tool, codeOther); got != 0 {
					t.Errorf("errors_total{tool=%q} incremented on a successful call: %v", tt.tool, got)
				}
				return
			}
			if got := errorsFor(t, tt.tool, tt.wantCode); got != 1 {
				t.Errorf("errors_total{tool=%q,code=%q} = %v, want 1", tt.tool, tt.wantCode, got)
			}
		})
	}
}

// TestInstrumentActionTool_LabelsAction is the end-to-end half of the
// cardinality guard (the unit half is TestActionSetLabel_BoundedByAllowlist):
// a consolidated tool must attribute each call to the action it selected,
// and must fold everything off the allowlist into one series.
//
// Written against the counter rather than against actionSet.label because
// the label function being right does not help if the wrapper never calls
// it — the failure this pins is a folded tool whose calls all land on
// action="" and silently look like an unfolded tool in every dashboard.
func TestInstrumentActionTool_LabelsAction(t *testing.T) {
	const tool = "fake_folded"

	set := actionSet{order: []string{"read", "write"}}.with(map[string]server.ToolHandlerFunc{
		"read": func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			return mcpgo.NewToolResultText(`{}`), nil
		},
		"write": func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			return mcpgo.NewToolResultText(`{}`), nil
		},
	})
	h := instrumentActionTool(tool, set, set.dispatch(tool))

	call := func(action any) {
		req := mcpgo.CallToolRequest{}
		if action != nil {
			req.Params.Arguments = map[string]any{"action": action}
		}
		if _, err := h(context.Background(), req); err != nil {
			t.Fatalf("handler returned transport error: %v", err)
		}
	}

	call("read")
	call("read")
	call("write")
	// Three different ways to be wrong — all must share ONE series, or a
	// caller-controlled string is minting time series.
	call("nope")
	call(nil)
	call("Read")

	if got := callsForAction(t, tool, "read", outcomeOK); got != 2 {
		t.Errorf("calls_total{action=\"read\",outcome=ok} = %v, want 2", got)
	}
	if got := callsForAction(t, tool, "write", outcomeOK); got != 1 {
		t.Errorf("calls_total{action=\"write\",outcome=ok} = %v, want 1", got)
	}
	if got := callsForAction(t, tool, actionInvalid, outcomeBusinessError); got != 3 {
		t.Errorf("calls_total{action=%q,outcome=business_error} = %v, want 3", actionInvalid, got)
	}
	if got := errorsForAction(t, tool, actionInvalid, ErrCodeValidationError); got != 3 {
		t.Errorf("errors_total{action=%q,code=validation_error} = %v, want 3", actionInvalid, got)
	}
	// The unfolded-tool series must stay empty: "" means "this tool has no
	// actions", and a folded tool leaking into it would break the
	// sum by (tool) equivalence the empty label exists to preserve.
	if got := callsForAction(t, tool, "", outcomeOK); got != 0 {
		t.Errorf("calls_total{action=\"\"} = %v, want 0 for a consolidated tool", got)
	}
}

// TestInstrumentTool_PanicPropagates pins the two things the wrapper owes a
// panicking handler: the call is still counted, and the panic is re-raised
// untouched. instrumentTool must never call recover().
func TestInstrumentTool_PanicPropagates(t *testing.T) {
	const tool = "fake_panicker"

	h := instrumentTool(tool, func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		panic("handler exploded")
	})

	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatalf("panic was swallowed by instrumentTool")
			}
			if got, ok := r.(string); !ok || got != "handler exploded" {
				t.Fatalf("panic value mutated: got %v", r)
			}
		}()
		_, _ = h(context.Background(), mcpgo.CallToolRequest{})
	}()

	if got := callsFor(t, tool, outcomePanic); got != 1 {
		t.Errorf("calls_total{tool=%q,outcome=%q} = %v, want 1", tool, outcomePanic, got)
	}
	if got := errorsFor(t, tool, codePanic); got != 1 {
		t.Errorf("errors_total{tool=%q,code=%q} = %v, want 1", tool, codePanic, got)
	}
}

// TestAddTool_InstrumentsEveryRegistration proves the interception point is
// the registration funnel, not 67 hand-edited handlers: a tool registered
// through addTool is counted when a real MCP client calls it.
func TestAddTool_InstrumentsEveryRegistration(t *testing.T) {
	const (
		okTool  = "fake_registered_ok"
		badTool = "fake_registered_business_error"
	)

	s := server.NewMCPServer("test", "0.1.0", server.WithToolCapabilities(false))
	addTool(s, mcpgo.NewTool(okTool), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText(`{"ok":true}`), nil
	})
	addTool(s, mcpgo.NewTool(badTool), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return skmcp.ToolError(ErrCodeIssueNotFound, "sk-999 not visible", nil), nil
	})

	c := startInProcessClient(t, s)
	ctx := context.Background()

	for _, name := range []string{okTool, badTool} {
		req := mcpgo.CallToolRequest{}
		req.Params.Name = name
		if _, err := c.CallTool(ctx, req); err != nil {
			t.Fatalf("CallTool(%s): %v", name, err)
		}
	}

	if got := callsFor(t, okTool, outcomeOK); got != 1 {
		t.Errorf("calls_total{tool=%q,outcome=ok} = %v, want 1", okTool, got)
	}
	if got := callsFor(t, badTool, outcomeBusinessError); got != 1 {
		t.Errorf("calls_total{tool=%q,outcome=business_error} = %v, want 1", badTool, got)
	}
	if got := errorsFor(t, badTool, ErrCodeIssueNotFound); got != 1 {
		t.Errorf("errors_total{tool=%q,code=%q} = %v, want 1", badTool, ErrCodeIssueNotFound, got)
	}
}

// TestMetricsEndpoint_ExposesToolCounters scrapes the same default
// registerer /metrics is mounted on in cmd/api and asserts the new series
// are actually on the wire after a call — promauto registration is only
// half the contract.
func TestMetricsEndpoint_ExposesToolCounters(t *testing.T) {
	const tool = "fake_scraped"

	h := instrumentTool(tool, func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return skmcp.ToolError(ErrCodeForbidden, "nope", nil), nil
	})
	if _, err := h(context.Background(), mcpgo.CallToolRequest{}); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	promhttp.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()

	// Label order in the exposition format is alphabetical, so `action`
	// leads. These literals are the exact series a dashboard or alert rule
	// selects on — spelling them out is the point of the test.
	want := []string{
		`skein_mcp_tool_calls_total{action="",outcome="business_error",tool="` + tool + `"} 1`,
		`skein_mcp_tool_errors_total{action="",code="forbidden",tool="` + tool + `"} 1`,
		"# HELP skein_mcp_tool_calls_total",
		"# HELP skein_mcp_tool_errors_total",
	}
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("/metrics output missing %q", w)
		}
	}
}

// TestKnownErrCodes_CoversPackage keeps the `code` label allowlist and the
// closed ErrCode set from drifting apart. A new ErrCode constant that
// nobody added to knownErrCodes would be silently bucketed as "other" —
// exactly the tool-level detail the metric exists to provide — so the check
// reads the source itself rather than a hand-maintained copy.
//
// It scans EVERY non-test .go file of the package, not just errors.go. The
// earlier single-file version is what let ErrCodePreviewUnavailable, which
// was declared in theme_preview_variables.go, sit outside the allowlist
// while the guard stayed green (sk-256 review). Convention says codes live
// in errors.go; this test must fail when the convention is broken, not
// assume it.
func TestKnownErrCodes_CoversPackage(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	fset := token.NewFileSet()
	declared := make(map[string]string) // code → "file:ConstName"
	var scanned int

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++

		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, ident := range vs.Names {
					if !strings.HasPrefix(ident.Name, "ErrCode") {
						continue
					}
					if i >= len(vs.Values) {
						t.Errorf("%s: %s has no value — an ErrCode constant must be a string literal "+
							"the allowlist can mirror", name, ident.Name)
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Errorf("%s: %s value is not a string literal", name, ident.Name)
						continue
					}
					code, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Errorf("%s: %s: unquote %s: %v", name, ident.Name, lit.Value, err)
						continue
					}
					if prev, dup := declared[code]; dup {
						t.Errorf("code %q declared twice: %s and %s:%s", code, prev, name, ident.Name)
						continue
					}
					declared[code] = name + ":" + ident.Name
					if _, known := knownErrCodes[code]; !known {
						t.Errorf("%s:%s (%q) is missing from knownErrCodes in metrics.go — "+
							"without an entry the code label collapses it to %q",
							name, ident.Name, code, codeOther)
					}
				}
			}
		}
	}

	// Self-checks on the guard itself: a scan that reads nothing, or reads
	// only errors.go, would pass vacuously and hide the very drift above.
	if scanned < 2 {
		t.Fatalf("scanned %d production files — the guard is not reading the package it thinks", scanned)
	}
	if len(declared) == 0 {
		t.Fatal("no ErrCode constants found in the package — the guard is not reading what it thinks")
	}
	if len(knownErrCodes) != len(declared) {
		t.Errorf("knownErrCodes has %d entries but the package declares %d ErrCode constants — "+
			"an entry has no matching constant", len(knownErrCodes), len(declared))
	}
}

// startInProcessClient boots an initialised in-process MCP client against s.
func startInProcessClient(t *testing.T, s *server.MCPServer) *client.Client {
	t.Helper()

	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("new in-process client: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "test", Version: "1"}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return c
}
