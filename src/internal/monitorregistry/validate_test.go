package monitorregistry

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

func intp(v int) *int { return &v }

func boolp(v bool) *bool { return &v }

func strp(v string) *string { return &v }

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

// TestValidate_RejectsEachRule — P-1, INV-13 (row #8): each violation is
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
		{"records on a method of an agent", func(r *Registry) {
			r.Agents[0].Methods[1].Conditions.Records = []string{"alpha"}
		}, RuleInvalid},
		{"records and recordsAbsent together", func(r *Registry) {
			r.Channels[1].Detect.Records = []string{"alpha"}
			r.Channels[1].Detect.RecordsAbsent = []string{"alpha"}
		}, RuleInvalid},
		{"a record the registry does not have", func(r *Registry) {
			r.Channels[1].Detect.RecordsAbsent = []string{"omega"}
		}, RuleInvalid},
		{"stderr phrase in the table of an http channel", func(r *Registry) {
			httpHost(r)
			r.Channels[1].Outcomes[0].Match.StderrContains = strp("busy")
		}, RuleInvalid},
		{"stderr phrase of two lines", func(r *Registry) {
			r.Channels[1].Outcomes[1].Match.StderrContains = strp("nothing\nwas sent")
		}, RuleInvalid},
		{"stderr phrase with an empty stderr", func(r *Registry) {
			r.Channels[1].Outcomes[1].Match.StderrContains = strp("nothing was sent")
			r.Channels[1].Outcomes[1].Match.StderrEmpty = boolp(true)
		}, RuleInvalid},
		{"stderr phrase and the same phrase", func(r *Registry) {
			r.Channels[0].Outcomes = append(r.Channels[0].Outcomes,
				OutcomeRow{Match: Match{ExitCode: intp(1), StderrContains: strp("busy")}, Outcome: OutcomeRefused},
				OutcomeRow{Match: Match{ExitCode: intp(1), StderrContains: strp("busy")}, Outcome: OutcomeUnknown})
		}, RuleAmbiguousSign},
		{"empty stderr and a stderr JSON in one row", func(r *Registry) {
			r.Channels[1].Outcomes[1].Match = Match{ExitCode: intp(1), StderrEmpty: boolp(true),
				StderrJSON: map[string]string{"error.code": "X"}}
		}, RuleInvalid},
		{"a capture without a row that answers same", func(r *Registry) {
			r.Channels[1].Addressee.Command = &AddresseeCommand{Argv: []string{"host", "show"},
				Results: []AddresseeResultRow{{Match: Match{ExitCode: intp(1)}, Result: AddresseeGone}},
				Capture: []string{"id"}}
		}, RuleInvalid},
		{"a list index outside ASCII digits", func(r *Registry) {
			r.Channels[1].Outcomes[0].Match.JSON = map[string]string{"stages.\u00b2": "turn_started"}
		}, RuleInvalid},
		{"http channel that names programs", func(r *Registry) {
			httpHost(r)
			r.Channels[1].Binaries = map[string][]string{PlatformLinux: {"host"}}
		}, RuleInvalid},
		{"http URL of another host without TLS", func(r *Registry) {
			httpHost(r)
			r.Channels[1].Executor.HTTP.URL = "http://api.example.com/{env:HOST_ID}"
		}, RuleInvalid},
		{"http URL with a brace that is no placeholder", func(r *Registry) {
			httpHost(r)
			r.Channels[1].Executor.HTTP.URL = "{env:HOST_URL}/sessions/{id}"
		}, RuleInvalid},
		{"key variable that is no variable name", func(r *Registry) {
			httpHost(r)
			r.Channels[1].Executor.HTTP.KeyEnv = []string{"HOST_KEY", "host key"}
		}, RuleInvalid},
		{"two rows one answer can match", func(r *Registry) {
			// exit 0 alone, and exit 0 with a JSON field: an answer with exit 0
			// and that field matches both.
			r.Channels[0].Outcomes = append(r.Channels[0].Outcomes,
				OutcomeRow{Match: Match{ExitCode: intp(0), JSON: map[string]string{"warning": "stale"}}, Outcome: OutcomeUnknown})
		}, RuleAmbiguousSign},
		{"agent options in a channel's detection", func(r *Registry) {
			r.Channels[1].Detect.AgentOptionsAbsent = []string{"--remote"}
		}, RuleInvalid},
		{"agent option that is not a bare long option", func(r *Registry) {
			r.Agents[0].Methods[1].Conditions.AgentOptionsAbsent = []string{"--remote=ws://x"}
		}, RuleInvalid},
		{"stderr criterion in the table of an http channel", func(r *Registry) {
			r.Channels[1].Executor = Executor{Type: ExecutorHTTP, TimeoutSec: 30,
				HTTP: &HTTPRequest{Method: "POST", URL: "{env:HOST_URL}", BodyField: "text"}}
			r.Channels[1].Outcomes = []OutcomeRow{{Match: Match{HTTPStatus: intp(200), StderrEmpty: boolp(true)}, Outcome: OutcomeQueued}}
		}, RuleInvalid},
		{"exit code alone and exit code with an empty stderr", func(r *Registry) {
			// exit 0 with an empty stderr also matches the row of exit 0 alone.
			r.Channels[0].Outcomes = append(r.Channels[0].Outcomes,
				OutcomeRow{Match: Match{ExitCode: intp(0), StderrEmpty: boolp(true)}, Outcome: OutcomeUnknown})
		}, RuleAmbiguousSign},
		{"probe with a path and an order of paths", func(r *Registry) {
			r.Channels[0].Probes[0].Paths = []string{"~/.alpha"}
		}, RuleInvalid},
		{"home path with a placeholder", func(r *Registry) {
			r.Channels[0].Probes[0] = Probe{Type: ProbeDirWritable, Source: "fixture", Paths: []string{"{env:ALPHA_HOME}", "~/{env:ALPHA_DIR}"}}
		}, RuleInvalid},
		{"order of paths on a tcp-reach probe", func(r *Registry) {
			r.Channels[0].Probes[0] = Probe{Type: ProbeTCPReach, Source: "fixture", Address: "{env:ALPHA_ADDR}", Paths: []string{"~/.alpha"}}
		}, RuleInvalid},
		{"shortening on an outcome other than a refusal", func(r *Registry) {
			r.Channels[0].Outcomes[0].Shorten = true
		}, RuleInvalid},
		{"empty text note", func(r *Registry) { r.Channels[1].TextNotes = []string{" "} }, RuleInvalid},
		{"address read from a variable of a bad name", func(r *Registry) {
			r.Channels[0].Address = &AddressSpec{Env: "ALPHA_THREAD", Qualifiers: []string{"ALPHA HOME"}}
		}, RuleInvalid},
		{"root process variable outside the qualifiers", func(r *Registry) {
			r.Channels[1].Address = &AddressSpec{Env: "HOST_ID", Qualifiers: []string{"HOST_ROOT_START"}, RootPIDEnv: "HOST_ROOT_PID"}
		}, RuleInvalid},
		{"path qualifier that is not a qualifier", func(r *Registry) {
			r.Channels[0].Address = &AddressSpec{Env: "ALPHA_THREAD", Qualifiers: []string{"ALPHA_HOME"},
				QualifierPaths: map[string]string{"ALPHA_DATA": "~/.alpha"}}
		}, RuleInvalid},
		{"path qualifier with a relative default", func(r *Registry) {
			r.Channels[0].Address = &AddressSpec{Env: "ALPHA_THREAD", Qualifiers: []string{"ALPHA_HOME"},
				QualifierPaths: map[string]string{"ALPHA_HOME": ".alpha"}}
		}, RuleInvalid},
		{"one path both required and absent", func(r *Registry) {
			r.Channels[1].Outcomes[0].Match.JSONAbsent = []string{"status"}
		}, RuleInvalid},
		{"dotted path with an empty element", func(r *Registry) {
			r.Channels[1].Outcomes[0].Match.JSON = map[string]string{"result..stages": "queued"}
		}, RuleInvalid},
		{"list element and its absence in rows that differ on nothing else", func(r *Registry) {
			// Absence of one path does not part a row from a row of another
			// path: an answer with stages.0 and without stages.1 matches both.
			r.Channels[1].Outcomes = []OutcomeRow{
				{Match: Match{ExitCode: intp(0), JSON: map[string]string{"stages.0": "input_accepted"}}, Outcome: OutcomeInputWritten},
				{Match: Match{ExitCode: intp(0), JSONAbsent: []string{"stages.1"}}, Outcome: OutcomeUnknown},
			}
		}, RuleAmbiguousSign},
		{"capture path with an empty element", func(r *Registry) {
			r.Channels[1].Addressee.Command = &AddresseeCommand{Argv: []string{"host", "show"},
				Results: []AddresseeResultRow{{Match: Match{ExitCode: intp(0)}, Result: AddresseeSame}}, Capture: []string{"id."}}
		}, RuleInvalid},
		{"path captured twice", func(r *Registry) {
			r.Channels[1].Addressee.Command = &AddresseeCommand{Argv: []string{"host", "show"},
				Results: []AddresseeResultRow{{Match: Match{ExitCode: intp(0)}, Result: AddresseeSame}}, Capture: []string{"id", "id"}}
		}, RuleInvalid},
		{"program that is neither a name nor a variable", func(r *Registry) {
			r.Channels[1].Binaries[PlatformLinux] = []string{"{env:HOST_CLI}/host"}
		}, RuleInvalid},
		{"program from a variable of a bad name", func(r *Registry) {
			r.Channels[1].Binaries[PlatformLinux] = []string{"{env:HOST CLI}"}
		}, RuleInvalid},
		{"launcher on a channel outside step 5", func(r *Registry) {
			r.Channels[0].Address = &AddressSpec{Env: "ALPHA_THREAD"}
			r.Channels[0].Launcher = &Launcher{Process: ProcessSign{Executables: []string{"alpha-daemon"}}}
		}, RuleInvalid},
		{"launcher on a host without an address", func(r *Registry) {
			r.Channels[1].Launcher = &Launcher{Process: ProcessSign{Executables: []string{"host-daemon"}}}
		}, RuleInvalid},
		{"launcher without a process sign", func(r *Registry) {
			r.Channels[1].Address = &AddressSpec{Env: "HOST_ID"}
			r.Channels[1].Launcher = &Launcher{}
		}, RuleInvalid},
		{"launcher keeping methods of a missing record", func(r *Registry) {
			r.Channels[1].Address = &AddressSpec{Env: "HOST_ID"}
			r.Channels[1].Launcher = &Launcher{Process: ProcessSign{Executables: []string{"host-daemon"}},
				Keeps: map[string][]string{"beta": {MethodStream}}}
		}, RuleInvalid},
		{"launcher keeping a method kind outside the set", func(r *Registry) {
			r.Channels[1].Address = &AddressSpec{Env: "HOST_ID"}
			r.Channels[1].Launcher = &Launcher{Process: ProcessSign{Executables: []string{"host-daemon"}},
				Keeps: map[string][]string{"alpha": {"poll"}}}
		}, RuleUnknownMethod},
		{"stderr JSON in the table of an http channel", func(r *Registry) {
			r.Channels[1].Executor = Executor{Type: ExecutorHTTP, TimeoutSec: 30,
				HTTP: &HTTPRequest{Method: "POST", URL: "{env:HOST_URL}", BodyField: "text"}}
			r.Channels[1].Outcomes = []OutcomeRow{{Match: Match{HTTPStatus: intp(200), StderrJSON: map[string]string{"error.code": "x"}}, Outcome: OutcomeQueued}}
		}, RuleInvalid},
		{"two rows of one stderr code", func(r *Registry) {
			r.Channels[1].Outcomes = append(r.Channels[1].Outcomes,
				OutcomeRow{Match: Match{ExitCode: intp(1), StderrJSON: map[string]string{"error.code": "busy"}}, Outcome: OutcomeRefused},
				OutcomeRow{Match: Match{ExitCode: intp(1), StderrJSON: map[string]string{"error.code": "busy"}}, Outcome: OutcomeUnknown})
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

// TestValidate_AcceptsEveryProbeType — P-7 (row #8): each of the four probe
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

// TestLoad — P-1, R-1 (row #9): Load refuses a malformed document and a
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
	for _, sh := range []string{"", WindowsShellPowerShell, WindowsShellPwsh, WindowsShellGitBash} {
		r := validRegistry()
		r.Agents[0].WindowsShell = sh
		if err := r.Validate(); err != nil {
			t.Fatalf("Windows shell %q was refused: %v", sh, err)
		}
	}
}

// TestValidate_RefusesAShellWithoutCommands — US-11: the monitor builds its
// commands for Git Bash, pwsh and Windows PowerShell only, so a record that
// names another shell (cmd among them) is refused with RuleInvalid instead
// of being given commands of a shell it does not run.
func TestValidate_RefusesAShellWithoutCommands(t *testing.T) {
	for _, sh := range []string{"cmd", "bash", "PowerShell"} {
		r := validRegistry()
		r.Agents[0].WindowsShell = sh
		err := r.Validate()
		if err == nil {
			t.Fatalf("Windows shell %q was accepted", sh)
		}
		if rules := rulesOf(t, err); len(rules) != 1 || rules[0] != RuleInvalid {
			t.Fatalf("Windows shell %q: %v, want one %s", sh, err, RuleInvalid)
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

// TestValidate_AcceptsTheCodexReadings — the schema readings the Codex record
// needs pass in a correct record: a stderr criterion telling an empty stderr
// from a warning under one exit code, an order of probe paths ending in a
// home path, a method condition on the agent's options, and an address with
// a qualifier.
func TestValidate_AcceptsTheCodexReadings(t *testing.T) {
	r := validRegistry()
	r.Channels[0].Outcomes = []OutcomeRow{
		{Match: Match{ExitCode: intp(0), StderrEmpty: boolp(true)}, Outcome: OutcomeQueued},
		{Match: Match{ExitCode: intp(0), StderrEmpty: boolp(false)}, Outcome: OutcomeUnknown},
	}
	r.Channels[0].Probes = []Probe{{Type: ProbeDirWritable, Source: "fixture",
		Paths: []string{"{env:ALPHA_SQLITE_HOME}", "{env:ALPHA_HOME}", "~/.alpha"}}}
	r.Channels[0].Address = &AddressSpec{Env: "ALPHA_THREAD", Qualifiers: []string{"ALPHA_HOME"}, PerProcess: true,
		QualifierPaths: map[string]string{"ALPHA_HOME": "~/.alpha"}}
	r.Agents[0].Methods[1].Conditions.AgentOptionsAbsent = []string{"--remote"}
	if err := r.Validate(); err != nil {
		t.Fatalf("a correct record with the Codex readings was refused: %v", err)
	}
}

// TestValidate_AcceptsTheZavodReadings — a refusal that shortens the text
// and the notes every text of a channel carries pass in a correct record.
func TestValidate_AcceptsTheZavodReadings(t *testing.T) {
	r := validRegistry()
	r.Channels[1].Outcomes = append(r.Channels[1].Outcomes,
		OutcomeRow{Match: Match{ExitCode: intp(11), JSON: map[string]string{"code": "too_large"}}, Outcome: OutcomeRefused, Shorten: true})
	r.Channels[1].TextNotes = []string{"Do not reply to this letter."}
	r.Channels[1].Address = &AddressSpec{Env: "HOST_ID", Qualifiers: []string{"HOST_ROOT_PID"}, RootPIDEnv: "HOST_ROOT_PID"}
	if err := r.Validate(); err != nil {
		t.Fatalf("a correct record with the Zavod readings was refused: %v", err)
	}
}

// TestValidate_AcceptsTheOrcaAndPaseoReadings — the readings the Orca and
// Paseo records need pass in a correct record: a list element and the
// absence of the next one telling two receipts apart under one exit code,
// an addressee check whose values the preflight captures, a program named
// by a variable, and a host that launches agents.
func TestValidate_AcceptsTheOrcaAndPaseoReadings(t *testing.T) {
	r := validRegistry()
	r.Channels[1].Outcomes = []OutcomeRow{
		{Match: Match{ExitCode: intp(0), JSON: map[string]string{"stages.0": "input_accepted", "stages.1": "turn_started"}},
			Outcome: OutcomeTurnStarted},
		{Match: Match{ExitCode: intp(0), JSON: map[string]string{"stages.0": "input_accepted"}, JSONAbsent: []string{"stages.1"}},
			Outcome: OutcomeInputWritten},
	}
	r.Channels[1].Addressee.Command = &AddresseeCommand{Argv: []string{"host", "show", "{env:HOST_ID}"},
		Results: []AddresseeResultRow{{Match: Match{ExitCode: intp(0)}, Result: AddresseeSame}},
		Capture: []string{"result.terminal.incarnationId", "result.terminal.agentIdentity"}}
	r.Channels[1].Binaries[PlatformLinux] = []string{"host", "{env:HOST_CLI}"}
	r.Channels[1].Outcomes = append(r.Channels[1].Outcomes,
		OutcomeRow{Match: Match{ExitCode: intp(1), StderrJSON: map[string]string{"error.code": "DAEMON_UNREACHABLE"}}, Outcome: OutcomeRefused},
		OutcomeRow{Match: Match{ExitCode: intp(1), StderrJSON: map[string]string{"error.code": "AUTH_REQUIRED"}}, Outcome: OutcomeRefused})
	r.Channels[1].Address = &AddressSpec{Env: "HOST_ID"}
	r.Channels[1].Launcher = &Launcher{Process: ProcessSign{Argv: []string{"/opt/host/daemon.js"}},
		Keeps: map[string][]string{"alpha": {MethodStream}}}
	if err := r.Validate(); err != nil {
		t.Fatalf("a correct record with the Orca and Paseo readings was refused: %v", err)
	}
}

// httpHost turns the host channel of validRegistry into a correct http
// channel: a URL template, an ordered key, a status table and no programs.
func httpHost(r *Registry) {
	c := &r.Channels[1]
	c.Executor = Executor{Type: ExecutorHTTP, TimeoutSec: 30, HTTP: &HTTPRequest{Method: "POST",
		URL:    "{env:HOST_URL}/v0/sessions/{env:HOST_ID}/messages",
		KeyEnv: []string{"HOST_KEY", "HOST_TOKEN"}, BodyField: "message"}}
	c.Outcomes = []OutcomeRow{
		{Match: Match{HTTPStatus: intp(201), JSON: map[string]string{"state": "queued"}}, Outcome: OutcomeQueued},
		{Match: Match{HTTPStatus: intp(201), JSON: map[string]string{"state": "sent"}}, Outcome: OutcomeInputWritten},
	}
	c.Binaries = nil
}

// TestValidate_AcceptsAnHTTPChannel — P-7 http: a record of the http
// executor (a URL template with a base variable, a key by an ordered list
// of variables, no programs) passes, and so does a literal URL of a local
// test server.
func TestValidate_AcceptsAnHTTPChannel(t *testing.T) {
	for _, url := range []string{"{env:HOST_URL}/v0/sessions/{env:HOST_ID}/messages",
		"https://api.example.com/v0/sessions/{env:HOST_ID}/messages",
		"http://127.0.0.1:8080/sessions/{env:HOST_ID}", "http://localhost/sessions/{env:HOST_ID}"} {
		r := validRegistry()
		httpHost(r)
		r.Channels[1].Executor.HTTP.URL = url
		if err := r.Validate(); err != nil {
			t.Fatalf("a correct http record with URL %q was refused: %v", url, err)
		}
	}
}

// TestValidate_AcceptsRecordsAndStderrPhrases — two channels of one
// environment split by the agent record (records, recordsAbsent), and rows
// told apart by a literal phrase on stderr, pass.
func TestValidate_AcceptsRecordsAndStderrPhrases(t *testing.T) {
	r := validRegistry()
	r.Channels[1].Detect.Records = []string{"alpha"}
	r.Channels[0].Detect.RecordsAbsent = []string{"alpha"}
	r.Channels[0].Outcomes = append(r.Channels[0].Outcomes,
		OutcomeRow{Match: Match{ExitCode: intp(4), StderrContains: strp("so nothing was sent")}, Outcome: OutcomeRefused},
		OutcomeRow{Match: Match{ExitCode: intp(4), StderrContains: strp("(SESSION_COMPOSER_BUSY)")}, Outcome: OutcomeRefused},
		OutcomeRow{Match: Match{ExitCode: intp(5), StderrEmpty: boolp(true)}, Outcome: OutcomeRefused},
		OutcomeRow{Match: Match{ExitCode: intp(5), StderrContains: strp("usage")}, Outcome: OutcomeUnknown})
	if err := r.Validate(); err != nil {
		t.Fatalf("a correct record with records and stderr phrases was refused: %v", err)
	}
}

// TestValidate_AParentAbsentIsApartFromItsChild — a row that wants a whole
// object absent and a row that reads a field of it cannot both match one
// answer (the Orca refusals: no writeSettlement, or its outcome refused).
func TestValidate_AParentAbsentIsApartFromItsChild(t *testing.T) {
	r := validRegistry()
	r.Channels[1].Outcomes = []OutcomeRow{
		{Match: Match{ExitCode: intp(1), JSONAbsent: []string{"result.settlement"}}, Outcome: OutcomeRefused},
		{Match: Match{ExitCode: intp(1), JSON: map[string]string{"result.settlement.outcome": "refused"}},
			Outcome: OutcomeRefused},
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("rows apart by a parent's absence were refused: %v", err)
	}
	// A sibling's absence does not keep them apart.
	r.Channels[1].Outcomes[0].Match.JSONAbsent = []string{"result.settle"}
	if err := r.Validate(); err == nil {
		t.Fatal("rows one answer can match both were accepted")
	}
}

// TestValidate_AnEmptyStderrIsApartFromAStderrJSON — one exit code, one row
// for an empty stderr and one for a JSON document on it: no answer matches
// both.
func TestValidate_AnEmptyStderrIsApartFromAStderrJSON(t *testing.T) {
	r := validRegistry()
	r.Channels[0].Outcomes = append(r.Channels[0].Outcomes,
		OutcomeRow{Match: Match{ExitCode: intp(1), StderrEmpty: boolp(true)}, Outcome: OutcomeRefused},
		OutcomeRow{Match: Match{ExitCode: intp(1), StderrJSON: map[string]string{"error.code": "BUSY"}},
			Outcome: OutcomeRefused})
	if err := r.Validate(); err != nil {
		t.Fatalf("rows apart by an empty stderr were refused: %v", err)
	}
}

// blindTwoStep turns the fixture's blind channel into a multiplexer with
// the calls of a two-step delivery: a stage that carries the text, the
// insert, an unstage and a submit, each naming the transfer's {delivery},
// and a pane PID command with a format of the program's own braces.
func blindTwoStep(r *Registry) *Channel {
	c := &r.Channels[2]
	c.Multiplexer = true
	c.Stage = &Executor{Type: ExecutorArgv, Argv: []string{"mux", "load", "{delivery}"}, Feed: FeedStdin, TimeoutSec: 5}
	c.Executor = Executor{Type: ExecutorArgv, Argv: []string{"mux", "paste", "{delivery}", "{env:PANE}"}, Feed: FeedStdin, TimeoutSec: 5}
	c.Unstage = &Executor{Type: ExecutorArgv, Argv: []string{"mux", "drop", "{delivery}"}, Feed: FeedStdin, TimeoutSec: 5}
	c.Submit = &Submit{
		Executor: Executor{Type: ExecutorArgv, Argv: []string{"mux", "keys", "{env:PANE}", "Enter"}, Feed: FeedStdin, TimeoutSec: 5},
		Outcomes: []OutcomeRow{{Match: Match{ExitCode: intp(0)}, Outcome: OutcomeInputWritten}},
	}
	c.PanePID = &PanePID{Argv: []string{"mux", "show", "-t", "{env:PANE}", "#{pane_pid}"}}
	return c
}

// TestValidate_AcceptsABlindTwoStepChannel — a blind multiplexer with a
// stage, an unstage and a submit (tmux), and a submit that takes its key as
// literal text on stdin (WezTerm), pass.
func TestValidate_AcceptsABlindTwoStepChannel(t *testing.T) {
	r := validRegistry()
	blindTwoStep(r)
	if err := r.Validate(); err != nil {
		t.Fatalf("a correct two-step blind record was refused: %v", err)
	}
	r = validRegistry()
	c := &r.Channels[2]
	c.Submit = &Submit{
		Executor: Executor{Type: ExecutorArgv, Argv: []string{"mux", "send", "--no-paste"}, Feed: FeedStdin, TimeoutSec: 5},
		Text:     "\r",
		Outcomes: []OutcomeRow{{Match: Match{ExitCode: intp(0)}, Outcome: OutcomeInputWritten}},
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("a submit with a literal text on stdin was refused: %v", err)
	}
}

// TestValidate_RejectsABlindCallOutOfItsPlace — the calls of a blind
// delivery are a blind channel's only, and each takes the text where the
// delivery hands it over; every violation is refused alone.
func TestValidate_RejectsABlindCallOutOfItsPlace(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(r *Registry)
	}{
		{"a multiplexer mark on a host", func(r *Registry) { r.Channels[1].Multiplexer = true }},
		{"a submit on a host", func(r *Registry) { r.Channels[1].Submit = blindTwoStep(validRegistry()).Submit }},
		{"{delivery} in a host's call", func(r *Registry) {
			r.Channels[1].Executor.Argv = []string{"host", "send", "{delivery}", "--file", "{textFile}"}
		}},
		{"{delivery} in the probe of a blind channel", func(r *Registry) {
			blindTwoStep(r).Probes[0].Argv = []string{"mux", "list", "{delivery}"}
		}},
		{"an unstage without a stage", func(r *Registry) { blindTwoStep(r).Stage = nil }},
		{"a stage with an insert that takes the text", func(r *Registry) {
			blindTwoStep(r).Executor = Executor{Type: ExecutorArgv, Argv: []string{"mux", "paste", "{text}"}, Feed: FeedArg, TimeoutSec: 5}
		}},
		{"an unstage that takes the text", func(r *Registry) {
			blindTwoStep(r).Unstage = &Executor{Type: ExecutorArgv, Argv: []string{"mux", "drop", "{text}"}, Feed: FeedArg, TimeoutSec: 5}
		}},
		{"a submit that refuses before send", func(r *Registry) {
			blindTwoStep(r).Submit.Outcomes[0].Outcome = OutcomeRefused
		}},
		{"a submit row outside the outcomes", func(r *Registry) {
			blindTwoStep(r).Submit.Outcomes[0].Outcome = "entered"
		}},
		{"a submit without a table", func(r *Registry) { blindTwoStep(r).Submit.Outcomes = nil }},
		{"a submit by an argument without its text", func(r *Registry) {
			blindTwoStep(r).Submit.Executor = Executor{Type: ExecutorArgv, Argv: []string{"mux", "keys", "{text}"}, Feed: FeedArg, TimeoutSec: 5}
		}},
		{"a submit that is a request", func(r *Registry) {
			blindTwoStep(r).Submit.Executor = Executor{Type: ExecutorHTTP, TimeoutSec: 5,
				HTTP: &HTTPRequest{Method: "POST", URL: "{env:PANE_URL}", BodyField: "text"}}
		}},
		{"a pane PID element shaped as an unknown placeholder", func(r *Registry) {
			blindTwoStep(r).PanePID.Argv = []string{"mux", "show", "{pane_pid}"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validRegistry()
			tc.mutate(r)
			want := RuleInvalid
			if tc.name == "a submit row outside the outcomes" {
				want = RuleUnknownOutcome
			}
			err := r.Validate()
			if err == nil {
				t.Fatalf("Validate accepted the violation, want rule %s", want)
			}
			for _, got := range rulesOf(t, err) {
				if got != want {
					t.Fatalf("Validate reported %v, want only %s", err, want)
				}
			}
		})
	}
}

func withEnvironment(r *Registry) *Environment {
	r.Environments = append(r.Environments, Environment{
		ID: "env-x", Name: "Env X", About: "an environment without a channel", Source: "fixture",
		Detect: Conditions{Env: []EnvCondition{{Name: "ENV_X_ID"}}}, HostMessage: HostMessageCannotWake,
	})
	return &r.Environments[len(r.Environments)-1]
}

// TestValidate_AcceptsAnEnvironmentWithoutAChannel — an environment known
// by its variables with either host message passes.
func TestValidate_AcceptsAnEnvironmentWithoutAChannel(t *testing.T) {
	for _, kind := range []string{HostMessageCannotWake, HostMessageUnsupported} {
		r := validRegistry()
		e := withEnvironment(r)
		e.HostMessage = kind
		e.Detect.Env = append(e.Detect.Env, EnvCondition{Name: "ENV_X_MODE", Equals: new(string)})
		*e.Detect.Env[1].Equals = "remote"
		if err := r.Validate(); err != nil {
			t.Fatalf("a correct environment with message %q was refused: %v", kind, err)
		}
	}
}

// TestValidate_RejectsAnEnvironmentOutOfShape — an environment is known by
// variables only and says one of the two messages; each violation is
// refused alone.
func TestValidate_RejectsAnEnvironmentOutOfShape(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(e *Environment, r *Registry)
		rule   Rule
	}{
		{"no variable", func(e *Environment, _ *Registry) { e.Detect.Env = nil }, RuleInvalid},
		{"a setting among its signs", func(e *Environment, _ *Registry) {
			e.Detect.Settings = []SettingCondition{{Path: "~/.x.json", Format: SettingJSON, Key: "k", Equals: json.RawMessage(`"v"`)}}
		}, RuleInvalid},
		{"a program among its signs", func(e *Environment, _ *Registry) { e.Detect.Programs = []string{"x"} }, RuleInvalid},
		{"an unknown message", func(e *Environment, _ *Registry) { e.HostMessage = "wakes" }, RuleInvalid},
		{"no name", func(e *Environment, _ *Registry) { e.Name = "" }, RuleInvalid},
		{"no source", func(e *Environment, _ *Registry) { e.Source = "" }, RuleMissingSource},
		{"the id of a channel", func(e *Environment, _ *Registry) { e.ID = "host" }, RuleDuplicateID},
		{"the id of another environment", func(_ *Environment, r *Registry) { withEnvironment(r) }, RuleDuplicateID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validRegistry()
			tc.mutate(withEnvironment(r), r)
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

// TestValidate_AVariableIsComparedOnlyByAChecks — a JSON criterion
// "{env:NAME}" (the answer names the id the delivery addresses) is a sign
// of a probe or of the addressee command, never of a delivery's outcome; it
// may equal any value, so it overlaps a literal on the same path.
func TestValidate_AVariableIsComparedOnlyByAChecks(t *testing.T) {
	r := validRegistry()
	r.Channels[1].Addressee.Command = &AddresseeCommand{Argv: []string{"host", "current"},
		Results: []AddresseeResultRow{{Match: Match{ExitCode: intp(0), JSON: map[string]string{"id": "{env:HOST_ID}"}}, Result: AddresseeSame}}}
	r.Channels[2].Probes[0].Results[0].Match.JSON = map[string]string{"pane": "{env:PANE}"}
	if err := r.Validate(); err != nil {
		t.Fatalf("a check comparing a variable was refused: %v", err)
	}
	for name, mutate := range map[string]func(r *Registry){
		"in an outcome row": func(r *Registry) {
			r.Channels[1].Outcomes[0].Match.JSON["session"] = "{env:HOST_ID}"
		},
		"of no variable name": func(r *Registry) {
			r.Channels[2].Probes[0].Results[0].Match.JSON = map[string]string{"pane": "{env:pane id}"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := validRegistry()
			mutate(r)
			err := r.Validate()
			if err == nil {
				t.Fatal("Validate accepted the violation")
			}
			for _, got := range rulesOf(t, err) {
				if got != RuleInvalid {
					t.Fatalf("Validate reported %v, want only %s", err, RuleInvalid)
				}
			}
		})
	}
	a := Match{ExitCode: intp(0), JSON: map[string]string{"id": "{env:HOST_ID}"}}
	b := Match{ExitCode: intp(0), JSON: map[string]string{"id": "other"}}
	if !a.overlaps(b) || !b.overlaps(a) {
		t.Fatal("a variable and a literal on one path do not overlap")
	}
}

// TestValidate_AUserAgentIsALiteralHeaderValue — the User-Agent of an http
// request is a literal of printable ASCII: no placeholder (no variable, no
// secret), no line break, no padding.
func TestValidate_AUserAgentIsALiteralHeaderValue(t *testing.T) {
	r := validRegistry()
	httpHost(r)
	r.Channels[1].Executor.HTTP.UserAgent = "skein-monitor/1 (agent)"
	if err := r.Validate(); err != nil {
		t.Fatalf("a literal User-Agent was refused: %v", err)
	}
	for _, ua := range []string{"{env:HOST_KEY}", "a\r\nX-Injected: 1", " padded", "café"} {
		r := validRegistry()
		httpHost(r)
		r.Channels[1].Executor.HTTP.UserAgent = ua
		err := r.Validate()
		if err == nil {
			t.Fatalf("User-Agent %q was accepted", ua)
		}
		for _, got := range rulesOf(t, err) {
			if got != RuleInvalid {
				t.Fatalf("User-Agent %q: %v, want only %s", ua, err, RuleInvalid)
			}
		}
	}
}

// TestValidate_RejectsAURLOutOfItsRules — the authority is read by the
// standard parse: user information, a missing host or a port that is no
// number are refused; a placeholder past the base is one element of the
// path, never the authority, the query or the fragment.
func TestValidate_RejectsAURLOutOfItsRules(t *testing.T) {
	for _, u := range []string{
		"http://localhost:80@evil.example/v0/{env:HOST_ID}",
		"http://127.0.0.1:80@evil.example/v0/{env:HOST_ID}",
		"https://user@api.example.com/v0/{env:HOST_ID}",
		"https://",
		"https:///v0/{env:HOST_ID}",
		"http://localhost:port/{env:HOST_ID}",
		"https://{env:HOST_ID}.example.com/v0",
		"https://api.example.com/v0?session={env:HOST_ID}",
		"https://api.example.com/v0#{env:HOST_ID}",
		"{env:HOST_URL}?{env:HOST_ID}",
		"{env:HOST_URL}{env:HOST_ID}",
	} {
		r := validRegistry()
		httpHost(r)
		r.Channels[1].Executor.HTTP.URL = u
		err := r.Validate()
		if err == nil {
			t.Fatalf("URL %q was accepted", u)
		}
		for _, got := range rulesOf(t, err) {
			if got != RuleInvalid {
				t.Fatalf("URL %q: %v, want only %s", u, err, RuleInvalid)
			}
		}
	}
}

// TestValidate_APhraseInsideAnotherOverlapsIt — one stderr that holds a
// phrase holds every phrase inside it, so rows of "busy" and "session busy"
// can match one answer; an empty phrase or one of spaces says nothing.
func TestValidate_APhraseInsideAnotherOverlapsIt(t *testing.T) {
	r := validRegistry()
	r.Channels[1].Outcomes = append(r.Channels[1].Outcomes,
		OutcomeRow{Match: Match{ExitCode: intp(4), StderrContains: strp("busy")}, Outcome: OutcomeRefused},
		OutcomeRow{Match: Match{ExitCode: intp(4), StderrContains: strp("session busy")}, Outcome: OutcomeUnknown})
	if got := rulesOf(t, r.Validate()); !slices.Contains(got, RuleAmbiguousSign) {
		t.Fatalf("nested phrases were taken as apart: %v", got)
	}
	for _, phrase := range []string{"", "   "} {
		r := validRegistry()
		r.Channels[1].Outcomes[0].Match.StderrContains = strp(phrase)
		err := r.Validate()
		if err == nil {
			t.Fatalf("phrase %q was accepted", phrase)
		}
		for _, got := range rulesOf(t, err) {
			if got != RuleInvalid {
				t.Fatalf("phrase %q: %v, want only %s", phrase, err, RuleInvalid)
			}
		}
	}
}
