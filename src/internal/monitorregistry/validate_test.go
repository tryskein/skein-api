package monitorregistry

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func intp(v int) *int { return &v }

// validRegistry is a registry that passes Validate: an agent with a stream
// and a delivery method, its own delivery channel (step 3), a host (step 5)
// and a blind channel (step 7), and an engine map to the agent. Each call
// returns a fresh value a case may mutate.
func validRegistry() *Registry {
	return &Registry{
		SchemaVersion: 1,
		General:       General{About: "general record", Source: "fixture", Warnings: []string{"not found"}},
		Agents: []Agent{{
			ID: "alpha", About: "fixture agent", Source: "fixture",
			Process: ProcessSign{Executables: []string{"alpha"}},
			Methods: []Method{
				{Kind: MethodStream, Instruction: "use the stream tool"},
				{Kind: MethodDelivery, Channel: "alpha-queue", Instruction: "run detached"},
			},
		}},
		Channels: []Channel{
			{
				ID: "alpha-queue", About: "own delivery", Source: "fixture", Step: StepOwnDelivery, Order: 1,
				Kind:     KindAddressed,
				Detect:   Conditions{Env: []EnvCondition{{Name: "ALPHA_THREAD", Pattern: "[0-9a-f-]+"}}},
				Executor: Executor{Type: ExecutorArgv, Argv: []string{"alpha", "queue", "{env:ALPHA_THREAD}"}, Feed: FeedStdin, TimeoutSec: 30},
				Idle:     Idle{Type: IdleNone},
				Outcomes: []OutcomeRow{
					{Match: Match{ExitCode: intp(0)}, Outcome: OutcomeQueued},
					{Match: Match{ExitCode: intp(3)}, Outcome: OutcomeRefused},
				},
				Probes:    []Probe{{Type: ProbeDirWritable, Source: "fixture", Path: "{env:ALPHA_HOME}/queue"}},
				Addressee: AddresseeCheck{Process: ProcessNone},
				Binaries:  map[string][]string{PlatformLinux: {"alpha"}},
			},
			{
				ID: "host", About: "host", Source: "fixture", Step: StepHost, Order: 1,
				Kind:     KindAddressed,
				Detect:   Conditions{Env: []EnvCondition{{Name: "HOST_ID"}}},
				Executor: Executor{Type: ExecutorArgv, Argv: []string{"host", "send", "--file", "{textFile}"}, Feed: FeedFile, TimeoutSec: 30},
				Idle:     Idle{Type: IdleBuiltin},
				Outcomes: []OutcomeRow{
					{Match: Match{ExitCode: intp(0), JSON: map[string]string{"status": "queued"}}, Outcome: OutcomeQueued},
					{Match: Match{ExitCode: intp(1), JSON: map[string]string{"code": "gone"}}, Outcome: OutcomeAddresseeGone},
				},
				ProbeUndefined: true,
				Addressee:      AddresseeCheck{Process: ProcessNone, Limitation: "cannot tell a recreated session"},
				Binaries:       map[string][]string{PlatformLinux: {"host"}, PlatformWindows: {"host.exe"}},
			},
			{
				ID: "blind", About: "blind channel", Source: "fixture", Step: StepBlind, Order: 1,
				Kind: KindTerminal, Blind: true,
				Detect:    Conditions{Env: []EnvCondition{{Name: "PANE"}}},
				Executor:  Executor{Type: ExecutorArgv, Argv: []string{"mux", "paste", "{env:PANE}", "{text}"}, Feed: FeedArg, TimeoutSec: 5},
				Idle:      Idle{Type: IdleNone},
				Outcomes:  []OutcomeRow{{Match: Match{ExitCode: intp(0)}, Outcome: OutcomeInputWritten}},
				Probes:    []Probe{{Type: ProbeArgv, Source: "fixture", Argv: []string{"mux", "list"}, Results: []ProbeResultRow{{Match: Match{ExitCode: intp(0)}, Result: ProbeAvailable}}}},
				Addressee: AddresseeCheck{Process: ProcessForeground},
				PanePID:   &PanePID{Argv: []string{"mux", "pid", "{env:PANE}"}},
				Binaries:  map[string][]string{PlatformLinux: {"mux"}, PlatformMacOS: {"mux"}},
			},
		},
		Shells:       Shells{Source: "fixture", Names: []string{"sh", "bash"}},
		ZavodEngines: map[string]string{"alpha-engine": "alpha"},
	}
}

func rulesOf(t *testing.T, err error) []Rule {
	t.Helper()
	var verrs ValidationErrors
	if !errors.As(err, &verrs) {
		t.Fatalf("error %v (%T) is not ValidationErrors", err, err)
	}
	rules := make([]Rule, len(verrs))
	for i, e := range verrs {
		rules[i] = e.Rule
	}
	return rules
}

// TestValidate_FixtureIsValid makes the rule cases below meaningful: the
// document they start from passes.
func TestValidate_FixtureIsValid(t *testing.T) {
	if err := validRegistry().Validate(); err != nil {
		t.Fatalf("fixture registry is invalid: %v", err)
	}
}

// TestValidate_RejectsEachRule — П-1, ИНВ-13 (row #8): each violation is
// refused, and the refusal names exactly that rule (one cause per case).
func TestValidate_RejectsEachRule(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(r *Registry)
		rule   Rule
	}{
		{"duplicate agent id", func(r *Registry) { r.Agents = append(r.Agents, r.Agents[0]) }, RuleDuplicateID},
		{"duplicate channel id", func(r *Registry) {
			dup := r.Channels[1]
			dup.Order = 2
			r.Channels = append(r.Channels, dup)
		}, RuleDuplicateID},
		{"sign mapped outside the outcomes", func(r *Registry) { r.Channels[1].Outcomes[0].Outcome = "delivered-probably" }, RuleUnknownOutcome},
		{"unknown executor type", func(r *Registry) { r.Channels[1].Executor.Type = "ssh" }, RuleUnknownExecutor},
		{"unknown method kind", func(r *Registry) { r.Agents[0].Methods[0].Kind = "poll" }, RuleUnknownMethod},
		{"unknown probe type", func(r *Registry) { r.Channels[0].Probes[0].Type = "ping" }, RuleUnknownProbe},
		{"probe feeding the event text", func(r *Registry) {
			r.Channels[2].Probes[0].Argv = []string{"mux", "list", "{text}"}
		}, RuleProbeFeedsText},
		{"socket-connect probe with a command to send", func(r *Registry) {
			r.Channels[0].Probes[0] = Probe{Type: ProbeSocketConnect, Source: "fixture", Path: "{env:ALPHA_HOME}/ctl.sock", Argv: []string{"hello"}}
		}, RuleProbeSends},
		{"agent without source", func(r *Registry) { r.Agents[0].Source = "" }, RuleMissingSource},
		{"probe without source", func(r *Registry) { r.Channels[0].Probes[0].Source = " " }, RuleMissingSource},
		{"channel without about", func(r *Registry) { r.Channels[1].About = "" }, RuleMissingAbout},
		{"blind channel outside step 7", func(r *Registry) { r.Channels[2].Step = StepTerminal }, RuleBlindStep},
		{"engine map to a missing record", func(r *Registry) { r.ZavodEngines["beta-engine"] = "beta" }, RuleUnknownEngineTarget},
		{"unknown schema version", func(r *Registry) { r.SchemaVersion = 2 }, RuleSchemaVersion},
		{"id outside the id form", func(r *Registry) { r.Channels[1].ID = "Host" }, RuleBadID},
		{"row without a criterion", func(r *Registry) {
			r.Channels[1].Outcomes = append(r.Channels[1].Outcomes, OutcomeRow{Outcome: OutcomeUnknown})
		}, RuleAmbiguousSign},
		{"probes and probe undefined at once", func(r *Registry) { r.Channels[0].ProbeUndefined = true }, RuleProbeChoice},
		{"neither probes nor probe undefined", func(r *Registry) { r.Channels[1].ProbeUndefined = false }, RuleProbeChoice},
		{"channel at step 7 that is not blind", func(r *Registry) {
			r.Channels[1].Step = StepBlind
			r.Channels[1].Order = 2
		}, RuleBlindStep},
		{"step outside the method order", func(r *Registry) { r.Channels[1].Step = 4 }, RuleInvalid},
		{"HTTP status in the table of an argv channel", func(r *Registry) {
			r.Channels[1].Outcomes[0].Match.HTTPStatus = intp(200)
		}, RuleInvalid},
		{"exit code in the table of an http channel", func(r *Registry) {
			r.Channels[1].Executor = Executor{Type: ExecutorHTTP, TimeoutSec: 30,
				HTTP: &HTTPRequest{Method: "POST", URL: "{env:HOST_URL}", BodyField: "text"}}
		}, RuleInvalid},
		{"Windows shell outside the closed set", func(r *Registry) { r.Agents[0].WindowsShell = "gitbash" }, RuleInvalid},
		{"two rows one answer can match", func(r *Registry) {
			// exit 0 alone, and exit 0 with a JSON field: an answer with exit 0
			// and that field matches both.
			r.Channels[0].Outcomes = append(r.Channels[0].Outcomes,
				OutcomeRow{Match: Match{ExitCode: intp(0), JSON: map[string]string{"warning": "stale"}}, Outcome: OutcomeUnknown})
		}, RuleAmbiguousSign},
		{"rows on different criteria that one answer can match", func(r *Registry) {
			r.Channels[1].Outcomes = []OutcomeRow{
				{Match: Match{JSON: map[string]string{"status": "queued"}}, Outcome: OutcomeQueued},
				{Match: Match{JSON: map[string]string{"code": "busy"}}, Outcome: OutcomeRefused},
			}
		}, RuleAmbiguousSign},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validRegistry()
			tc.mutate(r)
			err := r.Validate()
			if err == nil {
				t.Fatalf("Validate accepted the violation, want rule %s", tc.rule)
			}
			for _, got := range rulesOf(t, err) {
				if got != tc.rule {
					t.Fatalf("Validate reported %v, want only %s", err, tc.rule)
				}
			}
		})
	}
}

// TestValidate_AcceptsEveryProbeType — П-7 (row #8): each of the four probe
// types in a correct record passes, so the schema takes all of them before
// any record uses them.
func TestValidate_AcceptsEveryProbeType(t *testing.T) {
	probes := map[string]Probe{
		ProbeArgv: {Type: ProbeArgv, Source: "fixture", Argv: []string{"alpha", "ping"},
			Results: []ProbeResultRow{{Match: Match{ExitCode: intp(0)}, Result: ProbeAvailable}, {Match: Match{ExitCode: intp(2)}, Result: ProbeUnavailable}}},
		ProbeDirWritable:   {Type: ProbeDirWritable, Source: "fixture", Path: "{env:ALPHA_HOME}/queue"},
		ProbeSocketConnect: {Type: ProbeSocketConnect, Source: "fixture", Path: "{env:ALPHA_HOME}/ctl.sock"},
		ProbeTCPReach:      {Type: ProbeTCPReach, Source: "fixture", Address: "{env:ALPHA_ADDR}"},
	}
	for typ, p := range probes {
		t.Run(typ, func(t *testing.T) {
			r := validRegistry()
			r.Channels[0].Probes = []Probe{p}
			if err := r.Validate(); err != nil {
				t.Fatalf("a correct %s probe was refused: %v", typ, err)
			}
		})
	}
}

// TestLoad — П-1, Р-1 (row #9): Load refuses a malformed document and a
// well-formed but invalid one (it runs Validate), loads the embedded
// registry.json, and resolves a known id to its record and any other name
// of the right form to the general record.
func TestLoad(t *testing.T) {
	t.Run("malformed", func(t *testing.T) {
		for _, doc := range []string{`{"schemaVersion": 1`, `{"schemaVersion": 1, "surprise": true}`, `{} {}`} {
			_, err := Load([]byte(doc))
			if err == nil {
				t.Fatalf("Load accepted %q", doc)
			}
			if rules := rulesOf(t, err); len(rules) != 1 || rules[0] != RuleMalformed {
				t.Fatalf("Load(%q) = %v, want %s", doc, err, RuleMalformed)
			}
		}
	})
	t.Run("case and repeated keys", func(t *testing.T) {
		doc, err := json.Marshal(validRegistry())
		if err != nil {
			t.Fatal(err)
		}
		valid := string(doc)
		for name, bad := range map[string]string{
			"top-level key in another case": strings.Replace(valid, `"schemaVersion":1`, `"SchemaVersion":1`, 1),
			"nested key in another case":    strings.Replace(valid, `"about":"fixture agent"`, `"About":"fixture agent"`, 1),
			"repeated top-level key":        strings.Replace(valid, `"schemaVersion":1`, `"schemaVersion":1,"schemaVersion":1`, 1),
			"repeated key of a map":         strings.Replace(valid, `"alpha-engine":"alpha"`, `"alpha-engine":"alpha","alpha-engine":"alpha"`, 1),
		} {
			if bad == valid {
				t.Fatalf("%s: the replacement did not apply", name)
			}
			_, err := Load([]byte(bad))
			if err == nil {
				t.Fatalf("%s: Load accepted the document", name)
			}
			if rules := rulesOf(t, err); len(rules) != 1 || rules[0] != RuleMalformed {
				t.Fatalf("%s: Load = %v, want %s", name, err, RuleMalformed)
			}
		}
		if _, err := Load(doc); err != nil {
			t.Fatalf("the unaltered document does not load: %v", err)
		}
	})
	t.Run("invalid document runs into Validate", func(t *testing.T) {
		r := validRegistry()
		r.Agents[0].About = ""
		doc, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		_, err = Load(doc)
		if err == nil {
			t.Fatal("Load accepted an agent without about")
		}
		if rules := rulesOf(t, err); len(rules) != 1 || rules[0] != RuleMissingAbout {
			t.Fatalf("Load = %v, want %s", err, RuleMissingAbout)
		}
	})
	t.Run("embedded registry", func(t *testing.T) {
		if _, err := Load(Embedded()); err != nil {
			t.Fatalf("the embedded registry.json does not load: %v", err)
		}
	})
	t.Run("resolve", func(t *testing.T) {
		doc, err := json.Marshal(validRegistry())
		if err != nil {
			t.Fatal(err)
		}
		reg, err := Load(doc)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		known := reg.Resolve("alpha")
		if !known.Known() || known.Agent.ID != "alpha" {
			t.Fatalf("Resolve(alpha) = %+v, want the alpha record", known)
		}
		other := reg.Resolve("gamma")
		if other.Known() || other.General == nil || other.General.About != "general record" {
			t.Fatalf("Resolve(gamma) = %+v, want the general record", other)
		}
	})
}

// TestValidate_EnvPatternSubset — an EnvCondition pattern is validated by
// Go and applied by Python, so only the subset both read alike passes:
// constructs whose meaning differs (escape classes, anchors, POSIX classes,
// {,m}, flags, class set operations) are refused.
func TestValidate_EnvPatternSubset(t *testing.T) {
	accepted := []string{
		"[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}",
		"skm_[A-Za-z0-9]+",
		`a\.b|c(?:d|e)*?`,
		"[^/]+",
	}
	refused := []string{`\d+`, "^abc$", `a\z`, "[[:alpha:]]+", "a{,3}", "(?i)abc", "[a&&b]", "a{2", "(ab", "x{1001}", "é"}
	for _, p := range accepted {
		r := validRegistry()
		r.Channels[0].Detect.Env[0].Pattern = p
		if err := r.Validate(); err != nil {
			t.Fatalf("pattern %q was refused: %v", p, err)
		}
	}
	for _, p := range refused {
		r := validRegistry()
		r.Channels[0].Detect.Env[0].Pattern = p
		err := r.Validate()
		if err == nil {
			t.Fatalf("pattern %q was accepted", p)
		}
		if rules := rulesOf(t, err); len(rules) != 1 || rules[0] != RuleInvalid {
			t.Fatalf("pattern %q: %v, want one %s", p, err, RuleInvalid)
		}
	}
}

// TestValidate_AcceptsEveryWindowsShell: each shell of the closed set, and
// none named, passes.
func TestValidate_AcceptsEveryWindowsShell(t *testing.T) {
	for _, sh := range []string{"", WindowsShellPowerShell, WindowsShellPwsh, WindowsShellGitBash, WindowsShellCmd} {
		r := validRegistry()
		r.Agents[0].WindowsShell = sh
		if err := r.Validate(); err != nil {
			t.Fatalf("Windows shell %q was refused: %v", sh, err)
		}
	}
}

// TestValidate_Window — US-10 (the window model of a harness, row #42): a
// whole window passes, a method without one passes, and a window given in
// part (a field left out of the document) or with a field that is not
// positive is refused with RuleInvalid naming that field.
func TestValidate_Window(t *testing.T) {
	whole := `{"lengthMs":200,"lines":50,"budget":30,"refillSec":20}`
	withWindow := func(doc string) *Registry {
		var w Window
		if err := json.Unmarshal([]byte(doc), &w); err != nil {
			t.Fatalf("decode the window %s: %v", doc, err)
		}
		r := validRegistry()
		r.Agents[0].Methods[0].Limits = &Limits{Window: &w}
		return r
	}
	if err := withWindow(whole).Validate(); err != nil {
		t.Fatalf("a whole window was refused: %v", err)
	}
	r := validRegistry()
	r.Agents[0].Methods[0].Limits = &Limits{Line: &Limit{Max: 400, Unit: UnitChars}}
	if err := r.Validate(); err != nil {
		t.Fatalf("limits without a window were refused: %v", err)
	}
	for _, tc := range []struct{ doc, field string }{
		{`{"lengthMs":200,"lines":50,"budget":30}`, "refillSec"},
		{`{"lines":50,"budget":30,"refillSec":20}`, "lengthMs"},
		{`{"lengthMs":200,"lines":0,"budget":30,"refillSec":20}`, "lines"},
		{`{"lengthMs":200,"lines":50,"budget":-1,"refillSec":20}`, "budget"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			err := withWindow(tc.doc).Validate()
			if err == nil {
				t.Fatalf("the window %s was accepted", tc.doc)
			}
			var verrs ValidationErrors
			if !errors.As(err, &verrs) || len(verrs) != 1 || verrs[0].Rule != RuleInvalid ||
				!strings.HasSuffix(verrs[0].Path, ".limits.window."+tc.field) {
				t.Fatalf("the window %s: got %v, want one RuleInvalid at .limits.window.%s", tc.doc, err, tc.field)
			}
		})
	}
}
