package monitorregistry

import (
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Rule names one rule of Validate. A violation carries its rule so callers
// and tests tell violations apart without reading the text.
type Rule string

// Rules of Validate.
const (
	// RuleMalformed: the document is not one JSON object of the schema
	// (syntax, unknown field, wrong type, trailing data).
	RuleMalformed Rule = "malformed"
	// RuleSchemaVersion: the document declares an unknown schema version.
	RuleSchemaVersion Rule = "schema_version"
	// RuleMissingSource: a record, probe or the shell list has no source.
	RuleMissingSource Rule = "missing_source"
	// RuleMissingAbout: a record has no about.
	RuleMissingAbout Rule = "missing_about"
	// RuleBadID: an id is empty or not of the form [a-z0-9-]{1,64}.
	RuleBadID Rule = "bad_id"
	// RuleDuplicateID: two agent records or two channel records share an id.
	RuleDuplicateID Rule = "duplicate_id"
	// RuleUnknownMethod: a method kind outside the closed set.
	RuleUnknownMethod Rule = "unknown_method"
	// RuleUnknownExecutor: an executor type outside the closed set.
	RuleUnknownExecutor Rule = "unknown_executor"
	// RuleUnknownProbe: a probe type outside the closed set.
	RuleUnknownProbe Rule = "unknown_probe"
	// RuleUnknownOutcome: a sign maps to a value outside the closed set of
	// outcomes (or of probe or addressee results).
	RuleUnknownOutcome Rule = "unknown_outcome"
	// RuleAmbiguousSign: the same sign appears twice in one table, or a row
	// has no criterion.
	RuleAmbiguousSign Rule = "ambiguous_sign"
	// RuleProbeFeedsText: a probe carries the text of an event.
	RuleProbeFeedsText Rule = "probe_feeds_text"
	// RuleProbeSends: a socket-connect or other connection probe carries
	// something to send.
	RuleProbeSends Rule = "probe_sends"
	// RuleProbeChoice: a channel has both probes and "probe undefined", or
	// neither.
	RuleProbeChoice Rule = "probe_choice"
	// RuleBlindStep: a blind channel outside step 7, or a channel at step 7
	// that is not blind.
	RuleBlindStep Rule = "blind_step"
	// RuleUnknownEngineTarget: the Zavod engine map points to an agent
	// record that does not exist.
	RuleUnknownEngineTarget Rule = "unknown_engine_target"
	// RuleInvalid: any other field of the schema has a value outside its
	// allowed set or is missing.
	RuleInvalid Rule = "invalid"
)

// ValidationError is one violation of the registry schema.
type ValidationError struct {
	Rule Rule
	// Path locates the violation, e.g. "channels[2].outcomes[1].outcome".
	Path   string
	Detail string
}

// ValidationErrors are all violations found in one document.
type ValidationErrors []*ValidationError

func (es ValidationErrors) Error() string {
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "; ")
}

// Has reports whether a violation of the rule is among es.
func (es ValidationErrors) Has(rule Rule) bool {
	for _, e := range es {
		if e.Rule == rule {
			return true
		}
	}
	return false
}

var (
	idPattern      = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
	envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// A long option of an agent's command line, without a value.
	longOptionPattern = regexp.MustCompile(`^--[a-z0-9][a-z0-9-]*$`)
)

var (
	methodKinds   = set(MethodStream, MethodDelivery, MethodExitOnEvent)
	executorTypes = set(ExecutorArgv, ExecutorHTTP)
	feeds         = set(FeedStdin, FeedFile, FeedArg)
	probeTypes    = set(ProbeArgv, ProbeDirWritable, ProbeSocketConnect, ProbeTCPReach)
	outcomes      = set(OutcomeRefused, OutcomeQueued, OutcomeInputWritten, OutcomeInputNoReceipt,
		OutcomeTurnStarted, OutcomeUnknown, OutcomeAddresseeChanged, OutcomeAddresseeGone)
	probeResults     = set(ProbeAvailable, ProbeUnavailable)
	addresseeResults = set(AddresseeSame, AddresseeChanged, AddresseeGone)
	processChecks    = set(ProcessAlive, ProcessForeground, ProcessNone)
	idleTypes        = set(IdleNone, IdleBuiltin, IdleArgv)
	kinds            = set(KindTerminal, KindAddressed)
	steps            = map[int]bool{StepOwnDelivery: true, StepHost: true, StepTerminal: true, StepBlind: true}
	platforms        = set(PlatformLinux, PlatformMacOS, PlatformWindows)
	units            = set(UnitChars, UnitBytes, UnitLines, UnitEvents)
	settingFormats   = set(SettingJSON, SettingTOML, SettingYAML)
	windowsShells    = set(WindowsShellPowerShell, WindowsShellPwsh, WindowsShellGitBash)
	httpMethods      = set("POST", "PUT")
)

func set(values ...string) map[string]bool {
	m := make(map[string]bool, len(values))
	for _, v := range values {
		m[v] = true
	}
	return m
}

type validator struct{ errs ValidationErrors }

func (v *validator) add(rule Rule, path, format string, args ...any) {
	v.errs = append(v.errs, &ValidationError{Rule: rule, Path: path, Detail: fmt.Sprintf(format, args...)})
}

// Validate checks every rule of the schema and returns all violations as
// ValidationErrors, or nil when the registry is valid.
func (r *Registry) Validate() error {
	v := &validator{}
	if r.SchemaVersion != 1 {
		v.add(RuleSchemaVersion, "schemaVersion", "unknown schema version %d", r.SchemaVersion)
	}
	v.text(RuleMissingAbout, "general.about", r.General.About)
	v.text(RuleMissingSource, "general.source", r.General.Source)
	v.warnings("general.warnings", r.General.Warnings)

	v.text(RuleMissingSource, "shells.source", r.Shells.Source)
	if len(r.Shells.Names) == 0 {
		v.add(RuleInvalid, "shells.names", "the shell list is empty")
	}
	v.names("shells.names", r.Shells.Names)

	channelSteps := make(map[string]int, len(r.Channels))
	seenChannels := make(map[string]bool, len(r.Channels))
	seenOrder := make(map[[2]int]string)
	for i := range r.Channels {
		c := &r.Channels[i]
		path := fmt.Sprintf("channels[%d]", i)
		v.channel(path, c)
		if c.ID != "" {
			if seenChannels[c.ID] {
				v.add(RuleDuplicateID, path+".id", "channel id %q appears twice", c.ID)
			}
			seenChannels[c.ID] = true
			channelSteps[c.ID] = c.Step
		}
		key := [2]int{c.Step, c.Order}
		if other, dup := seenOrder[key]; dup {
			v.add(RuleInvalid, path+".order", "order %d of step %d is taken by channel %q", c.Order, c.Step, other)
		}
		seenOrder[key] = c.ID
	}

	seenAgents := make(map[string]bool, len(r.Agents))
	for i := range r.Agents {
		a := &r.Agents[i]
		path := fmt.Sprintf("agents[%d]", i)
		v.agent(path, a, channelSteps)
		if a.ID != "" {
			if seenAgents[a.ID] {
				v.add(RuleDuplicateID, path+".id", "agent id %q appears twice", a.ID)
			}
			seenAgents[a.ID] = true
		}
	}

	for engine, target := range r.ZavodEngines {
		path := "zavodEngines." + engine
		if !idPattern.MatchString(engine) {
			v.add(RuleInvalid, path, "engine id %q is not of the form [a-z0-9-]{1,64}", engine)
		}
		if !seenAgents[target] {
			v.add(RuleUnknownEngineTarget, path, "engine %q maps to %q, which is not an agent record", engine, target)
		}
	}

	if len(v.errs) == 0 {
		return nil
	}
	return v.errs
}

func (v *validator) text(rule Rule, path, value string) {
	if strings.TrimSpace(value) == "" {
		v.add(rule, path, "is empty")
	}
}

func (v *validator) warnings(path string, ws []string) {
	for i, w := range ws {
		if strings.TrimSpace(w) == "" {
			v.add(RuleInvalid, fmt.Sprintf("%s[%d]", path, i), "warning is empty")
		}
	}
}

func (v *validator) names(path string, names []string) {
	for i, n := range names {
		if strings.TrimSpace(n) == "" || strings.ContainsAny(n, "/\\") {
			v.add(RuleInvalid, fmt.Sprintf("%s[%d]", path, i), "%q is not a bare executable name", n)
		}
	}
}

func (v *validator) id(path, id string) {
	if !idPattern.MatchString(id) {
		v.add(RuleBadID, path, "id %q is not of the form [a-z0-9-]{1,64}", id)
	}
}

func (v *validator) agent(path string, a *Agent, channelSteps map[string]int) {
	v.id(path+".id", a.ID)
	v.text(RuleMissingAbout, path+".about", a.About)
	v.text(RuleMissingSource, path+".source", a.Source)
	if len(a.Process.Executables) == 0 && len(a.Process.Argv) == 0 {
		v.add(RuleInvalid, path+".process", "the agent record has no process sign")
	}
	v.names(path+".process.executables", a.Process.Executables)
	for i, arg := range a.Process.Argv {
		if strings.TrimSpace(arg) == "" {
			v.add(RuleInvalid, fmt.Sprintf("%s.process.argv[%d]", path, i), "argument sign is empty")
		}
	}
	if a.WindowsShell != "" && !windowsShells[a.WindowsShell] {
		v.add(RuleInvalid, path+".windowsShell", "shell %q is not one of powershell, pwsh, git-bash", a.WindowsShell)
	}
	v.warnings(path+".warnings", a.Warnings)
	for i := range a.Methods {
		m := &a.Methods[i]
		mpath := fmt.Sprintf("%s.methods[%d]", path, i)
		if !methodKinds[m.Kind] {
			v.add(RuleUnknownMethod, mpath+".kind", "method %q is not one of stream, delivery, exit-on-event", m.Kind)
		}
		switch {
		case m.Kind == MethodDelivery:
			step, ok := channelSteps[m.Channel]
			if !ok {
				v.add(RuleInvalid, mpath+".channel", "delivery method names channel %q, which does not exist", m.Channel)
			} else if step != StepOwnDelivery {
				v.add(RuleInvalid, mpath+".channel", "delivery method names channel %q of step %d, not step 3", m.Channel, step)
			}
		case m.Channel != "":
			v.add(RuleInvalid, mpath+".channel", "only a delivery method names a channel")
		}
		v.text(RuleInvalid, mpath+".instruction", m.Instruction)
		v.conditions(mpath+".conditions", m.Conditions, true)
		v.limits(mpath+".limits", m.Limits)
	}
}

// conditions checks a set of conditions; agentOptions says whether it may
// read the agent process's argument list (a method's conditions may, a
// channel's detection may not).
func (v *validator) conditions(path string, c Conditions, agentOptions bool) {
	for i, e := range c.Env {
		epath := fmt.Sprintf("%s.env[%d]", path, i)
		if !envNamePattern.MatchString(e.Name) {
			v.add(RuleInvalid, epath+".name", "%q is not an environment variable name", e.Name)
		}
		if e.Pattern != "" {
			if err := envPatternSubset(e.Pattern); err != nil {
				v.add(RuleInvalid, epath+".pattern", "pattern is outside the subset Go and Python read alike: %v", err)
			} else if _, err := regexp.Compile("^(?:" + e.Pattern + ")$"); err != nil {
				v.add(RuleInvalid, epath+".pattern", "pattern does not compile: %v", err)
			}
		}
	}
	for i, s := range c.Settings {
		spath := fmt.Sprintf("%s.settings[%d]", path, i)
		v.text(RuleInvalid, spath+".path", s.Path)
		v.text(RuleInvalid, spath+".key", s.Key)
		if !settingFormats[s.Format] {
			v.add(RuleInvalid, spath+".format", "format %q is not one of json, toml, yaml", s.Format)
		}
		if len(s.Equals) == 0 {
			v.add(RuleInvalid, spath+".equals", "the expected value is missing")
		}
	}
	v.names(path+".programs", c.Programs)
	if len(c.AgentOptionsAbsent) > 0 && !agentOptions {
		v.add(RuleInvalid, path+".agentOptionsAbsent", "only a method of an agent record reads the agent process's arguments")
	}
	for i, o := range c.AgentOptionsAbsent {
		if !longOptionPattern.MatchString(o) {
			v.add(RuleInvalid, fmt.Sprintf("%s.agentOptionsAbsent[%d]", path, i), "%q is not a long option of the form --name", o)
		}
	}
}

func (v *validator) limits(path string, l *Limits) {
	if l == nil {
		return
	}
	for name, lim := range map[string]*Limit{"line": l.Line, "batch": l.Batch} {
		if lim == nil {
			continue
		}
		if lim.Max <= 0 {
			v.add(RuleInvalid, path+"."+name+".max", "limit %d is not positive", lim.Max)
		}
		if !units[lim.Unit] {
			v.add(RuleInvalid, path+"."+name+".unit", "unit %q is not one of chars, bytes, lines, events", lim.Unit)
		}
	}
	if l.Rate != nil && (l.Rate.Burst <= 0 || l.Rate.IntervalSec <= 0) {
		v.add(RuleInvalid, path+".rate", "burst and interval must be positive")
	}
	// A window is whole or absent: a field left out decodes as 0 and is
	// refused like a non-positive one.
	if w := l.Window; w != nil {
		for name, value := range map[string]int{"lengthMs": w.LengthMs, "lines": w.Lines,
			"budget": w.Budget, "refillSec": w.RefillSec} {
			if value <= 0 {
				v.add(RuleInvalid, path+".window."+name, "window %s %d is not positive", name, value)
			}
		}
	}
}

func (v *validator) channel(path string, c *Channel) {
	v.id(path+".id", c.ID)
	v.text(RuleMissingAbout, path+".about", c.About)
	v.text(RuleMissingSource, path+".source", c.Source)
	if !steps[c.Step] {
		v.add(RuleInvalid, path+".step", "step %d is not one of 3, 5, 6, 7", c.Step)
	}
	if c.Blind != (c.Step == StepBlind) {
		v.add(RuleBlindStep, path+".step", "blind=%t at step %d: blind channels are exactly the channels of step 7", c.Blind, c.Step)
	}
	if !kinds[c.Kind] {
		v.add(RuleInvalid, path+".kind", "kind %q is not one of terminal, addressed", c.Kind)
	}
	if c.Blind && c.Kind != KindTerminal {
		v.add(RuleInvalid, path+".kind", "a blind channel is a terminal channel")
	}
	if len(c.Detect.Env) == 0 && len(c.Detect.Programs) == 0 {
		v.add(RuleInvalid, path+".detect", "the channel has no detection sign")
	}
	v.conditions(path+".detect", c.Detect, false)
	v.executor(path+".executor", c.Executor)
	v.idle(path+".idle", c.Idle)

	if len(c.Outcomes) == 0 {
		v.add(RuleInvalid, path+".outcomes", "the outcome table is empty")
	}
	rows := make([]Match, len(c.Outcomes))
	for i, row := range c.Outcomes {
		rows[i] = row.Match
		if !outcomes[row.Outcome] {
			v.add(RuleUnknownOutcome, fmt.Sprintf("%s.outcomes[%d].outcome", path, i), "%q is not an outcome", row.Outcome)
		}
		if row.Shorten && row.Outcome != OutcomeRefused {
			v.add(RuleInvalid, fmt.Sprintf("%s.outcomes[%d].shorten", path, i), "only a refusal before send shortens the text")
		}
	}
	v.signs(path+".outcomes", rows)
	v.criteriaOf(path+".outcomes", rows, c.Executor.Type)

	switch {
	case len(c.Probes) > 0 && c.ProbeUndefined:
		v.add(RuleProbeChoice, path+".probes", "probes are given and also declared undefined")
	case len(c.Probes) == 0 && !c.ProbeUndefined:
		v.add(RuleProbeChoice, path+".probes", "give probes or declare probeUndefined")
	}
	for i := range c.Probes {
		v.probe(fmt.Sprintf("%s.probes[%d]", path, i), &c.Probes[i])
	}

	v.addressee(path+".addressee", c)

	if c.Blind && c.PanePID == nil {
		v.add(RuleInvalid, path+".panePid", "a blind channel needs the source of its pane PID")
	}
	if !c.Blind && c.PanePID != nil {
		v.add(RuleInvalid, path+".panePid", "only a blind channel has a pane PID source")
	}
	if c.PanePID != nil {
		v.argv(path+".panePid.argv", c.PanePID.Argv, false)
	}

	if len(c.Binaries) == 0 {
		v.add(RuleInvalid, path+".binaries", "the channel names no platform")
	}
	for p, bins := range c.Binaries {
		if !platforms[p] {
			v.add(RuleInvalid, path+".binaries."+p, "platform %q is not one of linux, macos, windows", p)
		}
		if len(bins) == 0 {
			v.add(RuleInvalid, path+".binaries."+p, "no program for the platform")
		}
		for i, b := range bins {
			if strings.TrimSpace(b) == "" {
				v.add(RuleInvalid, fmt.Sprintf("%s.binaries.%s[%d]", path, p, i), "program is empty")
			}
		}
	}
	v.limits(path+".limits", c.Limits)
	v.warnings(path+".warnings", c.Warnings)
	for i, n := range c.TextNotes {
		if strings.TrimSpace(n) == "" || strings.ContainsAny(n, "\r\n") {
			v.add(RuleInvalid, fmt.Sprintf("%s.textNotes[%d]", path, i), "a note is one non-empty line")
		}
	}
	if a := c.Address; a != nil {
		if !envNamePattern.MatchString(a.Env) {
			v.add(RuleInvalid, path+".address.env", "%q is not an environment variable name", a.Env)
		}
		for i, q := range a.Qualifiers {
			if !envNamePattern.MatchString(q) {
				v.add(RuleInvalid, fmt.Sprintf("%s.address.qualifiers[%d]", path, i), "%q is not an environment variable name", q)
			}
		}
		if a.RootPIDEnv != "" && !envNamePattern.MatchString(a.RootPIDEnv) {
			v.add(RuleInvalid, path+".address.rootPidEnv", "%q is not an environment variable name", a.RootPIDEnv)
		}
		for _, q := range slices.Sorted(maps.Keys(a.QualifierPaths)) {
			if !slices.Contains(a.Qualifiers, q) {
				v.add(RuleInvalid, path+".address.qualifierPaths."+q, "%q is not one of the qualifiers", q)
			}
			if d := a.QualifierPaths[q]; !strings.HasPrefix(d, "~/") && !filepath.IsAbs(d) {
				v.add(RuleInvalid, path+".address.qualifierPaths."+q, "the default %q is neither absolute nor under ~/", d)
			}
		}
	}
}

func (v *validator) executor(path string, e Executor) {
	if !executorTypes[e.Type] {
		v.add(RuleUnknownExecutor, path+".type", "executor %q is not one of argv, http", e.Type)
		return
	}
	if e.TimeoutSec <= 0 {
		v.add(RuleInvalid, path+".timeoutSec", "timeout %d is not positive", e.TimeoutSec)
	}
	switch e.Type {
	case ExecutorArgv:
		if e.HTTP != nil {
			v.add(RuleInvalid, path+".http", "an argv executor has no http request")
		}
		if !feeds[e.Feed] {
			v.add(RuleInvalid, path+".feed", "feed %q is not one of stdin, file, arg", e.Feed)
			return
		}
		v.argv(path+".argv", e.Argv, true)
		text, file := count(e.Argv, PlaceholderText), count(e.Argv, PlaceholderTextFile)
		want := map[string][2]int{FeedStdin: {0, 0}, FeedFile: {0, 1}, FeedArg: {1, 0}}[e.Feed]
		if text != want[0] || file != want[1] {
			v.add(RuleInvalid, path+".argv", "feed %q needs %d {text} and %d {textFile} elements, found %d and %d",
				e.Feed, want[0], want[1], text, file)
		}
	case ExecutorHTTP:
		if len(e.Argv) > 0 || e.Feed != "" {
			v.add(RuleInvalid, path, "an http executor has no argv and no feed")
		}
		if e.HTTP == nil {
			v.add(RuleInvalid, path+".http", "the http request is missing")
			return
		}
		if !httpMethods[e.HTTP.Method] {
			v.add(RuleInvalid, path+".http.method", "method %q is not one of POST, PUT", e.HTTP.Method)
		}
		v.value(path+".http.url", e.HTTP.URL, false)
		if e.HTTP.KeyFileEnv != "" && !envNamePattern.MatchString(e.HTTP.KeyFileEnv) {
			v.add(RuleInvalid, path+".http.keyFileEnv", "%q is not an environment variable name", e.HTTP.KeyFileEnv)
		}
		v.text(RuleInvalid, path+".http.bodyField", e.HTTP.BodyField)
	}
}

func count(argv []string, element string) int {
	n := 0
	for _, a := range argv {
		if a == element {
			n++
		}
	}
	return n
}

// argv checks an argument list: non-empty, the program a literal, every
// element a literal without braces or exactly one known placeholder; the
// text placeholders only where allowText.
func (v *validator) argv(path string, argv []string, allowText bool) {
	if len(argv) == 0 {
		v.add(RuleInvalid, path, "the argument list is empty")
		return
	}
	if strings.ContainsAny(argv[0], "{}") || strings.TrimSpace(argv[0]) == "" {
		v.add(RuleInvalid, path+"[0]", "the program must be a literal name")
	}
	for i, a := range argv[1:] {
		v.value(fmt.Sprintf("%s[%d]", path, i+1), a, allowText)
	}
}

// value checks one templated value: a literal without braces, "{env:NAME}",
// or, where allowText, "{text}" / "{textFile}".
func (v *validator) value(path, s string, allowText bool) {
	switch {
	case s == PlaceholderText || s == PlaceholderTextFile:
		if !allowText {
			v.add(RuleProbeFeedsText, path, "%s carries the text of an event", s)
		}
	case strings.HasPrefix(s, PlaceholderEnvPrefix) && strings.HasSuffix(s, "}"):
		name := strings.TrimSuffix(strings.TrimPrefix(s, PlaceholderEnvPrefix), "}")
		if !envNamePattern.MatchString(name) {
			v.add(RuleInvalid, path, "%q is not an environment variable name", name)
		}
	case strings.ContainsAny(s, "{}"):
		v.add(RuleInvalid, path, "%q is neither a literal nor a placeholder", s)
	case s == "":
		v.add(RuleInvalid, path, "value is empty")
	}
}

// pathValue checks a probe path: a literal, or "{env:NAME}" optionally
// followed by "/" and literal elements.
func (v *validator) pathValue(path, s string) {
	if rest, home := strings.CutPrefix(s, "~/"); home {
		if rest == "" || strings.ContainsAny(rest, "{}") {
			v.add(RuleInvalid, path, "%q: a home path is \"~/\" followed by literal elements", s)
		}
		return
	}
	head, rest, hasRest := strings.Cut(s, "/")
	if strings.HasPrefix(head, PlaceholderEnvPrefix) {
		v.value(path, head, false)
		if hasRest && strings.ContainsAny(rest, "{}") {
			v.add(RuleInvalid, path, "only the first element of %q may be a placeholder", s)
		}
		return
	}
	v.value(path, s, false)
}

func (v *validator) idle(path string, i Idle) {
	if !idleTypes[i.Type] {
		v.add(RuleInvalid, path+".type", "idle wait %q is not one of none, builtin, argv", i.Type)
		return
	}
	if i.Type == IdleArgv {
		v.argv(path+".argv", i.Argv, false)
		if i.TimeoutSec <= 0 {
			v.add(RuleInvalid, path+".timeoutSec", "an argv idle wait needs a positive timeout")
		}
	} else if len(i.Argv) > 0 {
		v.add(RuleInvalid, path+".argv", "only an argv idle wait has a command")
	}
}

// signs checks that every row of a table has a criterion and that no answer
// can match two rows: a sign maps to exactly one value. Two rows overlap
// when no criterion they both state disagrees — then an answer carrying
// both rows' fields matches both (an identical row is the plain case).
func (v *validator) signs(path string, rows []Match) {
	for i, m := range rows {
		rpath := fmt.Sprintf("%s[%d].match", path, i)
		if m.empty() {
			v.add(RuleAmbiguousSign, rpath, "the row has no criterion")
			continue
		}
		for j := 0; j < i; j++ {
			if rows[j].overlaps(m) {
				v.add(RuleAmbiguousSign, rpath, "an answer can match both this row and row %d", j)
				break
			}
		}
	}
}

// criteriaOf checks that every row uses only criteria the answer of its
// executor has: an argv program has an exit code and no HTTP status, an
// http request has a status and no exit code. A row with the other
// criterion could never match.
func (v *validator) criteriaOf(path string, rows []Match, executor string) {
	for i, m := range rows {
		rpath := fmt.Sprintf("%s[%d].match", path, i)
		switch {
		case executor == ExecutorArgv && m.HTTPStatus != nil:
			v.add(RuleInvalid, rpath+".httpStatus", "an argv answer has no HTTP status")
		case executor == ExecutorHTTP && m.ExitCode != nil:
			v.add(RuleInvalid, rpath+".exitCode", "an http answer has no exit code")
		case executor == ExecutorHTTP && m.StderrEmpty != nil:
			v.add(RuleInvalid, rpath+".stderrEmpty", "an http answer has no stderr")
		}
	}
}

// empty reports whether the sign states no criterion.
func (m Match) empty() bool {
	return m.ExitCode == nil && m.HTTPStatus == nil && len(m.JSON) == 0 && m.StderrEmpty == nil
}

// overlaps reports whether one answer can satisfy both signs: every
// criterion stated by both has the same value. A sign without criteria
// overlaps nothing (it is refused on its own).
func (m Match) overlaps(o Match) bool {
	if m.empty() || o.empty() {
		return false
	}
	if m.StderrEmpty != nil && o.StderrEmpty != nil && *m.StderrEmpty != *o.StderrEmpty {
		return false
	}
	if m.ExitCode != nil && o.ExitCode != nil && *m.ExitCode != *o.ExitCode {
		return false
	}
	if m.HTTPStatus != nil && o.HTTPStatus != nil && *m.HTTPStatus != *o.HTTPStatus {
		return false
	}
	for k, want := range m.JSON {
		if got, both := o.JSON[k]; both && got != want {
			return false
		}
	}
	return true
}

func (v *validator) probe(path string, p *Probe) {
	if !probeTypes[p.Type] {
		v.add(RuleUnknownProbe, path+".type", "probe %q is not one of argv, dir-writable, socket-connect, tcp-reach", p.Type)
		return
	}
	v.text(RuleMissingSource, path+".source", p.Source)
	switch p.Type {
	case ProbeArgv:
		v.argv(path+".argv", p.Argv, false)
		if p.Path != "" || len(p.Paths) > 0 || p.Address != "" {
			v.add(RuleInvalid, path, "an argv probe has no path and no address")
		}
		if len(p.Results) == 0 {
			v.add(RuleInvalid, path+".results", "an argv probe needs its result table")
		}
		rows := make([]Match, len(p.Results))
		for i, r := range p.Results {
			rows[i] = r.Match
			if !probeResults[r.Result] {
				v.add(RuleUnknownOutcome, fmt.Sprintf("%s.results[%d].result", path, i), "%q is not available or unavailable", r.Result)
			}
		}
		v.signs(path+".results", rows)
		v.criteriaOf(path+".results", rows, ExecutorArgv)
	default:
		if len(p.Argv) > 0 {
			if p.Type == ProbeSocketConnect || p.Type == ProbeTCPReach {
				v.add(RuleProbeSends, path+".argv", "a %s probe connects and closes without sending anything", p.Type)
			} else {
				v.add(RuleInvalid, path+".argv", "a %s probe runs no command", p.Type)
			}
			for i, a := range p.Argv {
				if a == PlaceholderText || a == PlaceholderTextFile {
					v.add(RuleProbeFeedsText, fmt.Sprintf("%s.argv[%d]", path, i), "%s carries the text of an event", a)
				}
			}
		}
		if len(p.Results) > 0 {
			v.add(RuleInvalid, path+".results", "a %s probe has a fixed meaning and no result table", p.Type)
		}
		if p.Type == ProbeTCPReach {
			if p.Path != "" || len(p.Paths) > 0 {
				v.add(RuleInvalid, path+".path", "a tcp-reach probe has an address, not a path")
			}
			v.value(path+".address", p.Address, false)
		} else {
			if p.Address != "" {
				v.add(RuleInvalid, path+".address", "a %s probe has a path, not an address", p.Type)
			}
			switch {
			case p.Path != "" && len(p.Paths) > 0:
				v.add(RuleInvalid, path+".paths", "a %s probe gives a path or an order of paths, not both", p.Type)
			case len(p.Paths) > 0:
				for i, s := range p.Paths {
					v.pathValue(fmt.Sprintf("%s.paths[%d]", path, i), s)
				}
			default:
				v.pathValue(path+".path", p.Path)
			}
		}
	}
}

func (v *validator) addressee(path string, c *Channel) {
	a := c.Addressee
	if !processChecks[a.Process] {
		v.add(RuleInvalid, path+".process", "process check %q is not one of alive, foreground, none", a.Process)
	} else {
		switch {
		case c.Blind && a.Process != ProcessForeground:
			v.add(RuleInvalid, path+".process", "a blind channel checks that the agent is in the foreground")
		case c.Kind == KindTerminal && a.Process == ProcessNone:
			v.add(RuleInvalid, path+".process", "a terminal channel checks the agent process")
		case c.Kind == KindAddressed && a.Process != ProcessNone:
			v.add(RuleInvalid, path+".process", "an addressed channel does not need the agent process")
		}
	}
	if a.Command != nil {
		v.argv(path+".command.argv", a.Command.Argv, false)
		if len(a.Command.Results) == 0 {
			v.add(RuleInvalid, path+".command.results", "the addressee command needs its result table")
		}
		rows := make([]Match, len(a.Command.Results))
		for i, r := range a.Command.Results {
			rows[i] = r.Match
			if !addresseeResults[r.Result] {
				v.add(RuleUnknownOutcome, fmt.Sprintf("%s.command.results[%d].result", path, i), "%q is not same, changed or gone", r.Result)
			}
		}
		v.signs(path+".command.results", rows)
		v.criteriaOf(path+".command.results", rows, ExecutorArgv)
	}
}

// envPatternMeta are the characters with a meaning in a pattern.
const envPatternMeta = `\.^$|?*+()[]{}`

// envPatternSubset checks that an EnvCondition pattern stays inside the
// regular-expression subset Go RE2 and Python re read the same way, given
// whole-value matching on both sides:
//
//   - literals: printable ASCII other than the metacharacters
//     \ . ^ $ | ? * + ( ) [ ] { }, or a metacharacter (or "-") escaped with
//     a backslash; no other escape (\d \w \s \b \z \A \x… differ or do
//     not exist in one of them);
//   - "." (any character but a newline in both);
//   - a class [...] or [^...] of literals and ranges a-z, with \ ] [ - ^
//     escaped; no "[:", no "&&", "||", "--" or "~~" (Python warns of set
//     operations there);
//   - groups (...) and (?:...), alternation |;
//   - quantifiers * + ? (each optionally followed by ? for the lazy form)
//     and {n}, {n,}, {n,m} with n, m <= 1000 after an atom; no {,m} and no
//     lone "{" (Go and Python disagree on both);
//   - no anchors ^ and $ (the whole value is matched anyway, and $ before a
//     trailing newline differs), no flags (?i) and no other (?… group.
func envPatternSubset(p string) error {
	atom := false // the previous element can take a quantifier
	depth := 0
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c < 0x20 || c > 0x7e {
			return fmt.Errorf("character %q at %d is not printable ASCII", c, i)
		}
		switch c {
		case '\\':
			if i+1 >= len(p) || !strings.ContainsRune(envPatternMeta+"-", rune(p[i+1])) {
				return fmt.Errorf("escape at %d is not an escaped metacharacter", i)
			}
			i++
			atom = true
		case '.':
			atom = true
		case '^', '$':
			return fmt.Errorf("anchor %q at %d: the whole value is matched without anchors", c, i)
		case '|':
			atom = false
		case '(':
			if strings.HasPrefix(p[i:], "(?") {
				if !strings.HasPrefix(p[i:], "(?:") {
					return fmt.Errorf("group at %d: only (...) and (?:...) are allowed", i)
				}
				i += 2
			}
			depth++
			atom = false
		case ')':
			if depth == 0 {
				return fmt.Errorf("unbalanced ) at %d", i)
			}
			depth--
			atom = true
		case '[':
			end, err := envPatternClass(p, i)
			if err != nil {
				return err
			}
			i = end
			atom = true
		case ']', '}':
			return fmt.Errorf("unescaped %q at %d", c, i)
		case '*', '+', '?':
			if !atom {
				return fmt.Errorf("quantifier %q at %d has nothing to repeat", c, i)
			}
			if i+1 < len(p) && p[i+1] == '?' {
				i++
			}
			atom = false
		case '{':
			if !atom {
				return fmt.Errorf("repetition at %d has nothing to repeat", i)
			}
			end, err := envPatternRepeat(p, i)
			if err != nil {
				return err
			}
			i = end
			if i+1 < len(p) && p[i+1] == '?' {
				i++
			}
			atom = false
		default:
			atom = true
		}
	}
	if depth != 0 {
		return fmt.Errorf("unbalanced (")
	}
	return nil
}

// envPatternClass checks the class starting at p[start] == '[' and returns
// the index of its closing ']'.
func envPatternClass(p string, start int) (int, error) {
	i := start + 1
	if i < len(p) && p[i] == '^' {
		i++
	}
	first := true
	for ; i < len(p); i++ {
		c := p[i]
		switch {
		case c == ']' && !first:
			return i, nil
		case c == ']' || c == '[':
			return 0, fmt.Errorf("unescaped %q in the class at %d", c, start)
		case c == '\\':
			if i+1 >= len(p) || !strings.ContainsRune(`\]-^[`, rune(p[i+1])) {
				return 0, fmt.Errorf("escape in the class at %d is not \\ ] - ^ or [", start)
			}
			i++
		case c < 0x20 || c > 0x7e:
			return 0, fmt.Errorf("character %q in the class at %d is not printable ASCII", c, start)
		}
		for _, op := range []string{"&&", "||", "--", "~~"} {
			if strings.HasPrefix(p[i:], op) {
				return 0, fmt.Errorf("%q in the class at %d", op, start)
			}
		}
		first = false
	}
	return 0, fmt.Errorf("class at %d is not closed", start)
}

// envPatternRepeat checks the repetition starting at p[start] == '{' and
// returns the index of its closing '}'.
func envPatternRepeat(p string, start int) (int, error) {
	end := strings.IndexByte(p[start:], '}')
	if end < 0 {
		return 0, fmt.Errorf("repetition at %d is not closed", start)
	}
	body := p[start+1 : start+end]
	lo, hi, comma := strings.Cut(body, ",")
	num := func(s string) bool {
		if s == "" || len(s) > 4 {
			return false
		}
		n := 0
		for _, r := range s {
			if r < '0' || r > '9' {
				return false
			}
			n = n*10 + int(r-'0')
		}
		return n <= 1000
	}
	if !num(lo) || (comma && hi != "" && !num(hi)) {
		return 0, fmt.Errorf("repetition at %d must be {n}, {n,} or {n,m} with n, m <= 1000", start)
	}
	return start + end, nil
}
