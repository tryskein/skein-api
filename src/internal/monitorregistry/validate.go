package monitorregistry

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
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
	probeTypes    = set(ProbeArgv, ProbeDirWritable, ProbeSocketConnect, ProbeTCPReach, ProbeHTTPGet)
	outcomes      = set(OutcomeRefused, OutcomeQueued, OutcomeInputWritten, OutcomeInputNoReceipt,
		OutcomeTurnStarted, OutcomeUnknown, OutcomeAddresseeChanged, OutcomeAddresseeGone)
	probeResults = set(ProbeAvailable, ProbeUnavailable)
	// A check proves the same addressee or another; that the addressee
	// ended stops the monitor only where the outcome table of a delivery
	// says so ("Addressee and check", item 4), never by a check.
	addresseeResults = set(AddresseeSame, AddresseeChanged)
	processChecks    = set(ProcessAlive, ProcessForeground, ProcessNone)
	jsonTypes        = set("object", "list", "string", "number", "boolean")
	idleTypes        = set(IdleNone, IdleBuiltin, IdleArgv)
	kinds            = set(KindTerminal, KindAddressed)
	steps            = map[int]bool{StepOwnDelivery: true, StepHost: true, StepTerminal: true, StepBlind: true}
	platforms        = set(PlatformLinux, PlatformMacOS, PlatformWindows)
	units            = set(UnitChars, UnitUTF16, UnitBytes, UnitLines, UnitEvents)
	settingFormats   = set(SettingJSON, SettingTOML, SettingYAMLScalar)
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

type validator struct {
	errs ValidationErrors
	// delivery: the argument lists being validated are the calls of a
	// blind channel, which may name PlaceholderDelivery.
	delivery bool
}

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

	for i := range r.Channels {
		d := r.Channels[i].Detect
		for field, ids := range map[string][]string{"records": d.Records, "recordsAbsent": d.RecordsAbsent} {
			for j, id := range ids {
				if !seenAgents[id] {
					v.add(RuleInvalid, fmt.Sprintf("channels[%d].detect.%s[%d]", i, field, j), "%q is not an agent record", id)
				}
			}
		}
		if l := r.Channels[i].Launcher; l != nil {
			for _, agent := range slices.Sorted(maps.Keys(l.Keeps)) {
				if !seenAgents[agent] {
					v.add(RuleInvalid, fmt.Sprintf("channels[%d].launcher.keeps.%s", i, agent),
						"%q is not an agent record", agent)
				}
			}
		}
	}

	aliasNames := make(map[string]bool, len(r.EnvAliases))
	for i, a := range r.EnvAliases {
		path := fmt.Sprintf("envAliases[%d]", i)
		v.text(RuleMissingSource, path+".source", a.Source)
		if !envNamePattern.MatchString(a.Name) {
			v.add(RuleInvalid, path+".name", "%q is not an environment variable name", a.Name)
		}
		if aliasNames[a.Name] {
			v.add(RuleDuplicateID, path+".name", "variable %q has aliases twice", a.Name)
		}
		aliasNames[a.Name] = true
		if len(a.Aliases) == 0 {
			v.add(RuleInvalid, path+".aliases", "the variable has no alias")
		}
		for j, alias := range a.Aliases {
			if !envNamePattern.MatchString(alias) || alias == a.Name {
				v.add(RuleInvalid, fmt.Sprintf("%s.aliases[%d]", path, j), "%q is not another variable name", alias)
			}
		}
	}

	seenEnvironments := make(map[string]bool, len(r.Environments))
	for i := range r.Environments {
		e := &r.Environments[i]
		path := fmt.Sprintf("environments[%d]", i)
		v.environment(path, e)
		if e.ID != "" {
			if seenEnvironments[e.ID] || seenChannels[e.ID] {
				v.add(RuleDuplicateID, path+".id", "environment id %q appears twice", e.ID)
			}
			seenEnvironments[e.ID] = true
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
	if len(a.Process.Executables) == 0 && len(a.Process.Argv) == 0 && len(a.Process.ArgvBaseNames) == 0 {
		v.add(RuleInvalid, path+".process", "the agent record has no process sign")
	}
	v.names(path+".process.executables", a.Process.Executables)
	v.names(path+".process.argvBaseNames", a.Process.ArgvBaseNames)
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
	if len(c.EnvNot) > 0 && !agentOptions {
		v.add(RuleInvalid, path+".envNot", "only a method of an agent record states a variable that must not carry a value")
	}
	for i, e := range c.EnvNot {
		epath := fmt.Sprintf("%s.envNot[%d]", path, i)
		if !envNamePattern.MatchString(e.Name) {
			v.add(RuleInvalid, epath+".name", "%q is not an environment variable name", e.Name)
		}
		if e.Pattern != "" {
			v.add(RuleInvalid, epath+".pattern", "a variable that must not carry a value is compared literally, without a pattern")
		}
		if e.Equals == nil || *e.Equals == "" {
			v.add(RuleInvalid, epath+".equals", "the value the variable must not carry is missing")
		}
	}
	v.settings(path+".settings", c.Settings)
	v.settings(path+".settingsNot", c.SettingsNot)
	if len(c.SettingsNot) > 0 && !agentOptions {
		v.add(RuleInvalid, path+".settingsNot", "only a method of an agent record states a setting that must not carry a value")
	}
	if len(c.EnvAbsent) > 0 && !agentOptions {
		v.add(RuleInvalid, path+".envAbsent", "only a method of an agent record states a variable that must be unset")
	}
	for i, name := range c.EnvAbsent {
		if !envNamePattern.MatchString(name) {
			v.add(RuleInvalid, fmt.Sprintf("%s.envAbsent[%d]", path, i), "%q is not an environment variable name", name)
		}
	}
	if len(c.AnyOf) > 0 && !agentOptions {
		v.add(RuleInvalid, path+".anyOf", "only a method of an agent record states alternatives")
	}
	if len(c.AnyOf) == 1 {
		v.add(RuleInvalid, path+".anyOf", "one alternative is no choice: state its conditions directly")
	}
	for i, alt := range c.AnyOf {
		apath := fmt.Sprintf("%s.anyOf[%d]", path, i)
		switch {
		case len(alt.AnyOf) > 0 || len(alt.Records) > 0 || len(alt.RecordsAbsent) > 0 || len(alt.AgentOptionsAbsent) > 0:
			v.add(RuleInvalid, apath, "an alternative states only env, envNot, envAbsent, settings, settingsNot or programs")
		case len(alt.Env) == 0 && len(alt.EnvNot) == 0 && len(alt.EnvAbsent) == 0 && len(alt.Settings) == 0 &&
			len(alt.SettingsNot) == 0 && len(alt.Programs) == 0:
			v.add(RuleInvalid, apath, "an empty alternative always holds")
		}
		v.conditions(apath, alt, agentOptions)
	}
	v.names(path+".programs", c.Programs)
	if len(c.AgentOptionsAbsent) > 0 && !agentOptions {
		v.add(RuleInvalid, path+".agentOptionsAbsent", "only a method of an agent record reads the agent process's arguments")
	}
	if (len(c.Records) > 0 || len(c.RecordsAbsent) > 0) && agentOptions {
		v.add(RuleInvalid, path+".records", "only a channel's detection names the agent records it serves")
	}
	if len(c.Records) > 0 && len(c.RecordsAbsent) > 0 {
		v.add(RuleInvalid, path+".records", "records and recordsAbsent are not given together")
	}
	for i, o := range c.AgentOptionsAbsent {
		if !longOptionPattern.MatchString(o) {
			v.add(RuleInvalid, fmt.Sprintf("%s.agentOptionsAbsent[%d]", path, i), "%q is not a long option of the form --name", o)
		}
	}
}

func (v *validator) settings(path string, list []SettingCondition) {
	for i, s := range list {
		spath := fmt.Sprintf("%s[%d]", path, i)
		v.text(RuleInvalid, spath+".path", s.Path)
		v.text(RuleInvalid, spath+".key", s.Key)
		if !settingFormats[s.Format] {
			v.add(RuleInvalid, spath+".format", "format %q is not one of json, toml, yaml-scalar", s.Format)
		}
		if len(s.Equals) == 0 {
			v.add(RuleInvalid, spath+".equals", "the expected value is missing")
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
			v.add(RuleInvalid, path+"."+name+".unit", "unit %q is not one of chars, utf16, bytes, lines, events", lim.Unit)
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
	v.delivery = c.Blind
	v.executor(path+".executor", c.Executor)
	v.delivery = false
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
	v.criteriaOf(path+".outcomes", rows, c.Executor.Type, c.Executor.Detach)
	v.noEnvRefs(path+".outcomes", rows)

	switch {
	case len(c.Probes) > 0 && c.ProbeUndefined:
		v.add(RuleProbeChoice, path+".probes", "probes are given and also declared undefined")
	case len(c.Probes) == 0 && !c.ProbeUndefined:
		v.add(RuleProbeChoice, path+".probes", "give probes or declare probeUndefined")
	}
	for i := range c.Probes {
		ppath := fmt.Sprintf("%s.probes[%d]", path, i)
		v.probe(ppath, &c.Probes[i])
		if c.Probes[i].Type == ProbeHTTPGet {
			v.httpGetURL(ppath+".url", c.Probes[i].URL, c.Executor)
		}
	}

	v.addressee(path+".addressee", c)

	if l := c.Launcher; l != nil {
		if c.Step != StepHost || c.Address == nil {
			v.add(RuleInvalid, path+".launcher", "only a host of step 5 with an address launches agents")
		}
		if len(l.Process.Executables) == 0 && len(l.Process.Argv) == 0 && len(l.Process.ArgvBaseNames) == 0 {
			v.add(RuleInvalid, path+".launcher.process", "the launcher has no process sign")
		}
		v.names(path+".launcher.process.executables", l.Process.Executables)
		v.names(path+".launcher.process.argvBaseNames", l.Process.ArgvBaseNames)
		for i, arg := range l.Process.Argv {
			if strings.TrimSpace(arg) == "" {
				v.add(RuleInvalid, fmt.Sprintf("%s.launcher.process.argv[%d]", path, i), "argument sign is empty")
			}
		}
		for _, agent := range slices.Sorted(maps.Keys(l.Keeps)) {
			for i, kind := range l.Keeps[agent] {
				if !methodKinds[kind] {
					v.add(RuleUnknownMethod, fmt.Sprintf("%s.launcher.keeps.%s[%d]", path, agent, i),
						"method %q is not one of stream, delivery, exit-on-event", kind)
				}
			}
		}
	}

	// A blind channel without a pane PID source is never bound to a pane, so
	// it is never used (US-7); it still places the runner in its pane.
	if !c.Blind && c.PanePID != nil {
		v.add(RuleInvalid, path+".panePid", "only a blind channel has a pane PID source")
	}
	if c.PanePID != nil {
		v.formatArgv(path+".panePid.argv", c.PanePID.Argv)
		if c.PanePID.JSONField != "" {
			v.jsonPath(path+".panePid.jsonField", c.PanePID.JSONField)
		}
	}
	if c.Blind && c.Executor.Type != ExecutorArgv {
		v.add(RuleInvalid, path+".executor.type", "the insert of a blind channel is an argv program")
	}
	v.blindCalls(path, c)

	switch {
	case c.Executor.Type == ExecutorHTTP && len(c.Binaries) > 0:
		v.add(RuleInvalid, path+".binaries", "an http channel runs no program")
	case c.Executor.Type != ExecutorHTTP && len(c.Binaries) == 0:
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
			bpath := fmt.Sprintf("%s.binaries.%s[%d]", path, p, i)
			switch {
			case strings.TrimSpace(b) == "":
				v.add(RuleInvalid, bpath, "program is empty")
			case strings.ContainsAny(b, "{}"):
				// Only "{env:NAME}" as the whole element.
				if !strings.HasPrefix(b, PlaceholderEnvPrefix) || !strings.HasSuffix(b, "}") {
					v.add(RuleInvalid, bpath, "%q: a program is a name, a path or \"{env:NAME}\"", b)
				} else {
					v.value(bpath, b, false)
				}
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
		// The root process joins the identity the working runner checks
		// before a transfer only through the qualifiers.
		if a.RootPIDEnv != "" && !slices.Contains(a.Qualifiers, a.RootPIDEnv) {
			v.add(RuleInvalid, path+".address.rootPidEnv", "%q is not one of the qualifiers", a.RootPIDEnv)
		}
		for _, q := range slices.Sorted(maps.Keys(a.QualifierPaths)) {
			if !slices.Contains(a.Qualifiers, q) {
				v.add(RuleInvalid, path+".address.qualifierPaths."+q, "%q is not one of the qualifiers", q)
			}
			if d := a.QualifierPaths[q]; !strings.HasPrefix(d, "~/") && !filepath.IsAbs(d) {
				v.add(RuleInvalid, path+".address.qualifierPaths."+q, "the default %q is neither absolute nor under ~/", d)
			}
		}
		for _, q := range slices.Sorted(maps.Keys(a.QualifierDefaults)) {
			qpath := path + ".address.qualifierDefaults." + q
			if !slices.Contains(a.Qualifiers, q) {
				v.add(RuleInvalid, qpath, "%q is not one of the qualifiers", q)
			}
			if _, both := a.QualifierPaths[q]; both {
				v.add(RuleInvalid, qpath, "%q has a default path already", q)
			}
			if a.QualifierDefaults[q] == "" {
				v.add(RuleInvalid, qpath, "the default is empty")
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
		if e.Detach {
			v.add(RuleInvalid, path+".detach", "only an argv program is left to run on its own")
		}
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
		if f := e.HTTP.EndpointFile; f != nil {
			fpath := path + ".http.endpointFile"
			if len(e.HTTP.KeyEnv) > 0 {
				v.add(RuleInvalid, path+".http.keyEnv", "a request with an endpoint file authenticates by its password, not by a key")
			}
			if len(f.Paths) == 0 {
				v.add(RuleInvalid, fpath+".paths", "the endpoint file has no path")
			}
			for i, p := range f.Paths {
				v.pathValue(fmt.Sprintf("%s.paths[%d]", fpath, i), p)
			}
			v.text(RuleInvalid, fpath+".urlField", f.URLField)
			if (f.PasswordField == "") != (f.User == "") {
				v.add(RuleInvalid, fpath, "a password field and its Basic user go together")
			}
			v.endpointURL(path+".http.url", e.HTTP.URL)
		} else {
			v.url(path+".http.url", e.HTTP.URL)
		}
		for _, k := range slices.Sorted(maps.Keys(e.HTTP.BodyFixed)) {
			val := e.HTTP.BodyFixed[k]
			switch {
			case k == "" || k == e.HTTP.BodyField:
				v.add(RuleInvalid, path+".http.bodyFixed", "field %q is empty or the field of the text", k)
			case strings.ContainsAny(k+val, "{}"):
				v.add(RuleInvalid, path+".http.bodyFixed."+k, "%q is no literal: a fixed field takes no placeholder", val)
			}
		}
		for i, name := range e.HTTP.KeyEnv {
			if !envNamePattern.MatchString(name) {
				v.add(RuleInvalid, fmt.Sprintf("%s.http.keyEnv[%d]", path, i), "%q is not an environment variable name", name)
			}
		}
		v.text(RuleInvalid, path+".http.bodyField", e.HTTP.BodyField)
		if ua := e.HTTP.UserAgent; ua != "" {
			if strings.ContainsAny(ua, "{}") || strings.TrimSpace(ua) != ua || len(ua) > 200 ||
				strings.IndexFunc(ua, func(r rune) bool { return r < 0x20 || r > 0x7e }) >= 0 {
				v.add(RuleInvalid, path+".http.userAgent", "%q is not a literal header value of printable ASCII", ua)
			}
		}
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
func (v *validator) environment(path string, e *Environment) {
	v.id(path+".id", e.ID)
	v.text(RuleInvalid, path+".name", e.Name)
	v.text(RuleMissingAbout, path+".about", e.About)
	v.text(RuleMissingSource, path+".source", e.Source)
	d := e.Detect
	if len(d.Env) == 0 {
		v.add(RuleInvalid, path+".detect", "the environment has no variable to be recognised by")
	}
	if len(d.Settings) > 0 || len(d.Programs) > 0 || len(d.AgentOptionsAbsent) > 0 || len(d.Records) > 0 ||
		len(d.RecordsAbsent) > 0 {
		v.add(RuleInvalid, path+".detect", "an environment is recognised by variables only")
	}
	v.conditions(path+".detect", d, false)
	if e.HostMessage != HostMessageCannotWake && e.HostMessage != HostMessageUnsupported {
		v.add(RuleInvalid, path+".hostMessage", "host message %q is not one of cannot-wake, unsupported", e.HostMessage)
	}
}

// formatArgv validates the argument list of a command that never carries
// a text, whose program reads braces of its own: an element of the shape of
// a placeholder ("{env:NAME}", "{text}", "{textFile}", "{delivery}", a prefix
// and one variable) is checked as one; any other may hold braces
// ("#{pane_pid}", a JSON format of tmux).
func (v *validator) formatArgv(path string, argv []string) {
	if len(argv) == 0 {
		v.add(RuleInvalid, path, "the argument list is empty")
		return
	}
	if strings.ContainsAny(argv[0], "{}") || strings.TrimSpace(argv[0]) == "" {
		v.add(RuleInvalid, path+"[0]", "the program must be a literal name")
	}
	for i, a := range argv[1:] {
		if placeholderShaped(a) || !strings.ContainsAny(a, "{}") {
			v.value(fmt.Sprintf("%s[%d]", path, i+1), a, false)
		}
	}
}

// placeholderShaped reports whether an element reads as a placeholder of
// an argument list, which the runner replaces.
func placeholderShaped(a string) bool {
	return placeholderShape.MatchString(a) || prefixedEnvPattern.MatchString(a)
}

// placeholderShape is "{name}" or "{name:...}" without quotes or braces
// inside: a placeholder, known or not, never a literal.
var placeholderShape = regexp.MustCompile(`^\{[A-Za-z][A-Za-z0-9_]*(:[^{}"]*)?\}$`)

// blindCalls validates the calls of a blind channel besides its insert: the
// stage, the unstage and the submit, and the multiplexer mark.
func (v *validator) blindCalls(path string, c *Channel) {
	if !c.Blind {
		if c.Multiplexer || c.Stage != nil || c.Unstage != nil || c.Submit != nil {
			v.add(RuleInvalid, path, "only a blind channel has a multiplexer mark, a stage, an unstage or a submit")
		}
		return
	}
	v.delivery = true
	defer func() { v.delivery = false }()
	argvOnly := func(p string, e Executor) {
		if e.Type != ExecutorArgv {
			v.add(RuleInvalid, p+".type", "a call of a blind channel runs its program")
		}
		v.executor(p, e)
	}
	if c.Stage != nil {
		argvOnly(path+".stage", *c.Stage)
		if c.Executor.Feed != FeedStdin {
			v.add(RuleInvalid, path+".executor.feed", "with a stage the insert takes no text: its feed is stdin")
		}
	}
	if c.Unstage != nil {
		if c.Stage == nil {
			v.add(RuleInvalid, path+".unstage", "an unstage needs a stage")
		}
		argvOnly(path+".unstage", *c.Unstage)
		if c.Unstage.Feed != FeedStdin {
			v.add(RuleInvalid, path+".unstage.feed", "the unstage takes no text: its feed is stdin")
		}
	}
	if c.Submit != nil {
		// The insert leads to Enter only when it wrote the text; an outcome
		// that ends the delivery without Enter is a refusal or unknown.
		for i, row := range c.Outcomes {
			switch row.Outcome {
			case OutcomeInputWritten, OutcomeInputNoReceipt, OutcomeRefused, OutcomeUnknown:
			default:
				v.add(RuleInvalid, fmt.Sprintf("%s.outcomes[%d].outcome", path, i),
					"the insert of a channel with a submit writes the text or not: %q skips Enter", row.Outcome)
			}
		}
	}
	if s := c.Submit; s != nil {
		argvOnly(path+".submit.executor", s.Executor)
		if s.Executor.Feed == FeedFile {
			v.add(RuleInvalid, path+".submit.executor.feed", "the submit takes its literal text on stdin or as an argument")
		}
		if s.Executor.Feed == FeedArg && s.Text == "" {
			v.add(RuleInvalid, path+".submit.text", "a submit that takes an argument names its text")
		}
		if len(s.Outcomes) == 0 {
			v.add(RuleInvalid, path+".submit.outcomes", "the outcome table is empty")
		}
		rows := make([]Match, len(s.Outcomes))
		for i, row := range s.Outcomes {
			rows[i] = row.Match
			p := fmt.Sprintf("%s.submit.outcomes[%d]", path, i)
			switch {
			case !outcomes[row.Outcome]:
				v.add(RuleUnknownOutcome, p+".outcome", "%q is not an outcome", row.Outcome)
			case row.Outcome == OutcomeRefused:
				v.add(RuleInvalid, p+".outcome", "the text is already written: the submit refuses nothing before send")
			}
			if row.Shorten {
				v.add(RuleInvalid, p+".shorten", "only a refusal before send shortens the text")
			}
		}
		v.signs(path+".submit.outcomes", rows)
		v.criteriaOf(path+".submit.outcomes", rows, s.Executor.Type, false)
		v.noEnvRefs(path+".submit.outcomes", rows)
	}
}

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

// prefixedEnvPattern is a value of a literal prefix and one variable.
var prefixedEnvPattern = regexp.MustCompile(`^[A-Za-z0-9_.:=-]+\{env:[A-Za-z_][A-Za-z0-9_]*\}$`)

// value checks one templated value: a literal without braces, "{env:NAME}",
// a literal prefix followed by one "{env:NAME}", or, where allowText,
// "{text}" / "{textFile}".
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
	case prefixedEnvPattern.MatchString(s):
		// A literal prefix then one variable ("id:{env:KITTY_WINDOW_ID}"):
		// a selector whose field the program needs in the same argument.
	case s == PlaceholderDelivery:
		if !v.delivery {
			v.add(RuleInvalid, path, "only the calls of a blind channel name %s", s)
		}
	case strings.ContainsAny(s, "{}"):
		v.add(RuleInvalid, path, "%q is neither a literal nor a placeholder", s)
	case s == "":
		v.add(RuleInvalid, path, "value is empty")
	}
}

// url checks the URL template of an http executor: literals and
// "{env:NAME}" placeholders; a template that does not start with a
// placeholder starts with "https://", or with "http://" for localhost or
// 127.0.0.1 (the runner checks the scheme of a placeholder base when it
// expands it).
// endpointURL checks the URL template of a request whose base comes from its
// endpoint file: {endpoint}, then a path checked as that of a literal base.
func (v *validator) endpointURL(path, s string) {
	rest, ok := strings.CutPrefix(s, PlaceholderEndpoint)
	if !ok {
		v.add(RuleInvalid, path, "%q: a request with an endpoint file starts with %s", s, PlaceholderEndpoint)
		return
	}
	if rest != "" && !strings.HasPrefix(rest, "/") {
		v.add(RuleInvalid, path, "%q: the endpoint is followed by its path", s)
		return
	}
	v.url(path, "https://endpoint.invalid"+rest)
}

// httpGetURL checks the URL of a GET a probe or a check sends with the
// address and authentication of the channel's http executor.
func (v *validator) httpGetURL(path, s string, e Executor) {
	if e.Type != ExecutorHTTP || e.HTTP == nil {
		v.add(RuleInvalid, path, "a GET goes with the address of an http executor")
		return
	}
	if e.HTTP.EndpointFile != nil {
		v.endpointURL(path, s)
		return
	}
	v.url(path, s)
}

func (v *validator) url(path, s string) {
	if s == "" {
		v.add(RuleInvalid, path, "value is empty")
		return
	}
	// The placeholders, each a whole {env:NAME}, and where they stand.
	var at []int
	rest, offset := s, 0
	for rest != "" {
		open := strings.IndexAny(rest, "{}")
		if open < 0 {
			break
		}
		end := strings.Index(rest[open:], "}")
		if rest[open] != '{' || end < 0 {
			v.add(RuleInvalid, path, "%q is not a template of literals and {env:NAME}", s)
			return
		}
		v.value(path, rest[open:open+end+1], false)
		at = append(at, offset+open)
		offset += open + end + 1
		rest = rest[open+end+1:]
	}
	// A placeholder past the base is one element of the path: none in the
	// query or the fragment.
	if q := strings.IndexAny(s, "?#"); q >= 0 {
		for _, i := range at {
			if i > q {
				v.add(RuleInvalid, path, "%q: a placeholder stands in the query or the fragment", s)
				return
			}
		}
	}
	// The whole template, each placeholder replaced by a plain element
	// (the runner percent-escapes values), parses as a URL: a literal past
	// a placeholder is checked too, its percent escapes included.
	whole := s
	for i := len(at) - 1; i >= 0; i-- {
		stand := "x"
		if at[i] == 0 {
			stand = "https://base.test"
		}
		end := at[i] + strings.Index(s[at[i]:], "}") + 1
		whole = whole[:at[i]] + stand + whole[end:]
	}
	if _, err := url.Parse(whole); err != nil {
		v.add(RuleInvalid, path, "%q is no URL once its placeholders are filled (%v)", s, err)
		return
	}
	if len(at) > 0 && at[0] == 0 {
		// A leading placeholder is the base (scheme, host, maybe a path),
		// checked by the runner once substituted; what follows it is path.
		after := s[strings.Index(s, "}")+1:]
		if after != "" && !strings.HasPrefix(after, "/") {
			v.add(RuleInvalid, path, "%q: the base is followed by its path", s)
		}
		return
	}
	// A literal base: scheme and authority come before any placeholder.
	literal := s
	if len(at) > 0 {
		literal = s[:at[0]]
		if !strings.Contains(strings.TrimPrefix(strings.TrimPrefix(literal, "https://"), "http://"), "/") {
			v.add(RuleInvalid, path, "%q: a placeholder stands in the authority", s)
			return
		}
	}
	if !secureURL(literal) {
		v.add(RuleInvalid, path, "%q is neither https of a host nor http of localhost or 127.0.0.1", s)
	}
}

// secureURL reports whether the literal start of a URL is https with a
// host, or http of localhost or 127.0.0.1, by the standard parse of its
// authority: no user information, a numeric port if any.
func secureURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.User != nil || u.Opaque != "" {
		return false
	}
	if p := u.Port(); p != "" {
		// The runner sends to a port in 1..65535 (executors.py#expand_url).
		if n, err := strconv.ParseUint(p, 10, 16); err != nil || n == 0 {
			return false
		}
	}
	switch u.Scheme {
	case "https":
		return u.Hostname() != ""
	case "http":
		return u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"
	}
	return false
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
		if m.StillRunning != nil && (!*m.StillRunning || !m.onlyStillRunning()) {
			v.add(RuleInvalid, rpath+".stillRunning", "a call still running has no exit and no output: stillRunning is true and alone")
		}
		for _, k := range slices.Sorted(maps.Keys(m.JSON)) {
			v.jsonPath(rpath+".json", k)
			if name, ok := envRef(m.JSON[k]); ok && !envNamePattern.MatchString(name) {
				v.add(RuleInvalid, rpath+".json."+k, "%q is not an environment variable name", name)
			}
		}
		for _, k := range slices.Sorted(maps.Keys(m.JSONNot)) {
			v.jsonPath(rpath+".jsonNot", k)
			if name, ok := envRef(m.JSONNot[k]); ok && !envNamePattern.MatchString(name) {
				v.add(RuleInvalid, rpath+".jsonNot."+k, "%q is not an environment variable name", name)
			}
			if want, both := m.JSON[k]; both && want == m.JSONNot[k] {
				v.add(RuleInvalid, rpath+".jsonNot."+k, "the path cannot hold %q and something else", want)
			}
			if kind := m.JSONType[k]; absentAt(m.JSONAbsent, k) || kind == "object" || kind == "list" {
				v.add(RuleInvalid, rpath+".jsonNot."+k, "the path must hold a value compared as text")
			}
		}
		for _, k := range slices.Sorted(maps.Keys(m.StderrJSON)) {
			v.jsonPath(rpath+".stderrJson", k)
			// The runner compares stderrJson literally: a variable there
			// would match nothing.
			if isEnvRef(m.StderrJSON[k]) {
				v.add(RuleInvalid, rpath+".stderrJson."+k, "a stderrJson value is a literal, not a variable")
			}
		}
		for _, k := range slices.Sorted(maps.Keys(m.JSONType)) {
			v.jsonPath(rpath+".jsonType", k)
			if !jsonTypes[m.JSONType[k]] {
				v.add(RuleInvalid, rpath+".jsonType."+k, "type %q is not one of object, list, string, number, boolean", m.JSONType[k])
			}
			if want, both := m.JSON[k]; both && m.JSONType[k] != "string" && !isEnvRef(want) {
				// A value compared as text is a string, a number, a
				// boolean or null: never an object or a list.
				if m.JSONType[k] == "object" || m.JSONType[k] == "list" {
					v.add(RuleInvalid, rpath+".jsonType."+k, "path %q is both a %s and a value", k, m.JSONType[k])
				}
			}
			if absentAt(m.JSONAbsent, k) {
				v.add(RuleInvalid, rpath+".jsonType."+k, "path %q is both typed and absent", k)
			}
		}
		for j, k := range m.JSONAbsent {
			v.jsonPath(fmt.Sprintf("%s.jsonAbsent[%d]", rpath, j), k)
		}
		// A value at a path, or under it, cannot be read where the path
		// is absent: such a row matches no answer.
		for _, k := range slices.Sorted(maps.Keys(m.JSON)) {
			if absentAt(m.JSONAbsent, k) {
				v.add(RuleInvalid, rpath+".json."+k, "path %q is both required and absent (or under an absent one)", k)
			}
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
func (v *validator) criteriaOf(path string, rows []Match, executor string, detach bool) {
	for i, m := range rows {
		rpath := fmt.Sprintf("%s[%d].match", path, i)
		if m.StillRunning != nil && !detach {
			v.add(RuleInvalid, rpath+".stillRunning", "only a detached argv call can still be running")
		}
		switch {
		case executor == ExecutorArgv && m.HTTPStatus != nil:
			v.add(RuleInvalid, rpath+".httpStatus", "an argv answer has no HTTP status")
		case executor == ExecutorHTTP && m.ExitCode != nil:
			v.add(RuleInvalid, rpath+".exitCode", "an http answer has no exit code")
		case executor == ExecutorHTTP && m.StderrEmpty != nil:
			v.add(RuleInvalid, rpath+".stderrEmpty", "an http answer has no stderr")
		case executor == ExecutorHTTP && len(m.StderrJSON) > 0:
			v.add(RuleInvalid, rpath+".stderrJson", "an http answer has no stderr")
		case m.StderrEmpty != nil && *m.StderrEmpty && len(m.StderrJSON) > 0:
			v.add(RuleInvalid, rpath+".stderrJson", "an empty stderr holds no JSON document")
		case m.StderrContains != nil && strings.TrimSpace(*m.StderrContains) == "":
			v.add(RuleInvalid, rpath+".stderrContains", "the phrase is empty")
		case executor == ExecutorHTTP && m.StderrContains != nil:
			v.add(RuleInvalid, rpath+".stderrContains", "an http answer has no stderr")
		case m.StderrContains != nil && strings.ContainsAny(*m.StderrContains, "\r\n"):
			v.add(RuleInvalid, rpath+".stderrContains", "the phrase is one line")
		case m.StderrContains != nil && m.StderrEmpty != nil && *m.StderrEmpty:
			v.add(RuleInvalid, rpath+".stderrContains", "an empty stderr contains no phrase")
		}
	}
}

// jsonPath checks a dotted path of a JSON answer: non-empty elements of
// printable characters (a key of an object may be any printable text but
// the separator); an element read as an index of a list is ASCII digits,
// as the runner reads it ("²" is a key, never an index).
func (v *validator) jsonPath(path, p string) {
	if p == "" || slices.Contains(strings.Split(p, "."), "") {
		v.add(RuleInvalid, path, "%q is not a dotted path of non-empty elements", p)
		return
	}
	for _, r := range p {
		if !unicode.IsPrint(r) {
			v.add(RuleInvalid, path, "%q holds a character that is not printable", p)
			return
		}
	}
}

// envRef reports the variable of a JSON criterion "{env:NAME}": the value
// of that variable of the agent process, so the answer of a check names the
// very id the delivery addresses.
func envRef(value string) (string, bool) {
	if strings.HasPrefix(value, PlaceholderEnvPrefix) && strings.HasSuffix(value, "}") {
		return strings.TrimSuffix(strings.TrimPrefix(value, PlaceholderEnvPrefix), "}"), true
	}
	return "", false
}

func isEnvRef(value string) bool {
	_, ok := envRef(value)
	return ok
}

// noEnvRefs refuses a variable in the rows of a delivery's outcome table:
// only a check (a probe, the addressee command) compares an answer with a
// variable of the agent process.
func (v *validator) noEnvRefs(path string, rows []Match) {
	for i, m := range rows {
		for _, k := range slices.Sorted(maps.Keys(m.JSON)) {
			if isEnvRef(m.JSON[k]) {
				v.add(RuleInvalid, fmt.Sprintf("%s[%d].match.json.%s", path, i, k),
					"an outcome row compares no variable of the agent")
			}
		}
	}
}

// empty reports whether the sign states no criterion.
func (m Match) empty() bool {
	return m.ExitCode == nil && m.HTTPStatus == nil && len(m.JSON) == 0 && len(m.JSONNot) == 0 && len(m.JSONAbsent) == 0 &&
		len(m.JSONType) == 0 &&
		m.StderrEmpty == nil && len(m.StderrJSON) == 0 && m.StderrContains == nil && m.StillRunning == nil
}

// onlyStillRunning reports whether the row states no criterion but
// stillRunning.
func (m Match) onlyStillRunning() bool {
	rest := m
	rest.StillRunning = nil
	return rest.empty()
}

// overlaps reports whether one answer can satisfy both signs: every
// criterion stated by both has the same value. A sign without criteria
// overlaps nothing (it is refused on its own).
func (m Match) overlaps(o Match) bool {
	if m.empty() || o.empty() {
		return false
	}
	// A call still running has no exit and no output: it is apart from
	// every sign of a finished call.
	if (m.StillRunning != nil) != (o.StillRunning != nil) {
		return false
	}
	if m.StderrEmpty != nil && o.StderrEmpty != nil && *m.StderrEmpty != *o.StderrEmpty {
		return false
	}
	// One stderr holds a phrase and every phrase inside it, and one run of
	// text holds two phrases whose ends overlap ("session busy" and "busy
	// now" in "session busy now"): two phrases are apart only when neither
	// holds the other nor runs on into it.
	if m.StderrContains != nil && o.StderrContains != nil && phrasesApart(*m.StderrContains, *o.StderrContains) {
		return false
	}
	// An empty stderr holds no JSON document.
	if (len(m.StderrJSON) > 0 && o.StderrEmpty != nil && *o.StderrEmpty) ||
		(len(o.StderrJSON) > 0 && m.StderrEmpty != nil && *m.StderrEmpty) {
		return false
	}
	if (m.StderrContains != nil && o.StderrEmpty != nil && *o.StderrEmpty) ||
		(o.StderrContains != nil && m.StderrEmpty != nil && *m.StderrEmpty) {
		return false
	}
	if m.ExitCode != nil && o.ExitCode != nil && *m.ExitCode != *o.ExitCode {
		return false
	}
	if m.HTTPStatus != nil && o.HTTPStatus != nil && *m.HTTPStatus != *o.HTTPStatus {
		return false
	}
	for k, want := range m.JSON {
		// A variable's value may be any value: it disagrees with nothing.
		if got, both := o.JSON[k]; both && got != want && !isEnvRef(got) && !isEnvRef(want) {
			return false
		}
		if absentAt(o.JSONAbsent, k) {
			return false
		}
	}
	for k := range o.JSON {
		if absentAt(m.JSONAbsent, k) {
			return false
		}
	}
	// A typed path: another type, or the path (or a parent) absent, is
	// apart.
	for k, kind := range m.JSONType {
		if other, both := o.JSONType[k]; both && other != kind {
			return false
		}
		if absentAt(o.JSONAbsent, k) {
			return false
		}
	}
	// One path cannot hold a value and something other than it; a path
	// that must hold something is not absent, nor an object or a list.
	for _, pair := range [][2]Match{{m, o}, {o, m}} {
		a, b := pair[0], pair[1]
		for k, unwanted := range a.JSONNot {
			if want, both := b.JSON[k]; both && want == unwanted {
				return false
			}
			if absentAt(b.JSONAbsent, k) {
				return false
			}
			if kind := b.JSONType[k]; kind == "object" || kind == "list" {
				return false
			}
		}
	}
	// A literal (or a variable's value) on a typed path of the other row:
	// apart when no value of that type compares equal to it.
	for k, kind := range m.JSONType {
		if want, both := o.JSON[k]; both && !literalOfType(want, kind) {
			return false
		}
	}
	for k, kind := range o.JSONType {
		if want, both := m.JSON[k]; both && !literalOfType(want, kind) {
			return false
		}
	}
	for k := range o.JSONType {
		if absentAt(m.JSONAbsent, k) {
			return false
		}
	}
	for k, want := range m.StderrJSON {
		if got, both := o.StderrJSON[k]; both && got != want {
			return false
		}
	}
	return true
}

// phrasesApart reports whether no run of text holds both phrases as one
// piece: neither holds the other and no end of one is the start of the
// other.
func phrasesApart(a, b string) bool {
	if strings.Contains(a, b) || strings.Contains(b, a) {
		return false
	}
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		for n := 1; n < len(pair[0]) && n < len(pair[1]); n++ {
			if strings.HasSuffix(pair[0], pair[1][:n]) {
				return false
			}
		}
	}
	return true
}

// literalOfType reports whether a value of the JSON type kind can compare
// equal to a json criterion: the runner compares a string as itself and a
// number, a boolean or null in its JSON form, never an object or a list
// (executors.py#json_path_value); a variable's value is a string.
func literalOfType(want, kind string) bool {
	switch kind {
	case "string":
		return true
	case "boolean":
		return isEnvRef(want) || want == "true" || want == "false"
	case "number":
		var n json.Number
		return isEnvRef(want) || (json.Unmarshal([]byte(want), &n) == nil && want == strings.TrimSpace(want))
	}
	return false
}

// absentAt reports whether a path that holds a value cannot be there when
// one of the absent paths is absent: the path itself or one of its parents.
func absentAt(absent []string, k string) bool {
	for _, a := range absent {
		if k == a || strings.HasPrefix(k, a+".") {
			return true
		}
	}
	return false
}

func (v *validator) probe(path string, p *Probe) {
	if !probeTypes[p.Type] {
		v.add(RuleUnknownProbe, path+".type", "probe %q is not one of argv, dir-writable, socket-connect, tcp-reach, http-get", p.Type)
		return
	}
	v.text(RuleMissingSource, path+".source", p.Source)
	if p.URL != "" && p.Type != ProbeHTTPGet {
		v.add(RuleInvalid, path+".url", "only an http-get probe has a URL")
	}
	switch p.Type {
	case ProbeHTTPGet:
		if len(p.Argv) > 0 || p.Path != "" || len(p.Paths) > 0 || p.Address != "" {
			v.add(RuleInvalid, path, "an http-get probe has a URL only")
		}
		if len(p.Results) == 0 {
			v.add(RuleInvalid, path+".results", "an http-get probe needs its result table")
		}
		rows := make([]Match, len(p.Results))
		for i, r := range p.Results {
			rows[i] = r.Match
			if !probeResults[r.Result] {
				v.add(RuleUnknownOutcome, fmt.Sprintf("%s.results[%d].result", path, i), "%q is not available or unavailable", r.Result)
			}
		}
		v.signs(path+".results", rows)
		v.criteriaOf(path+".results", rows, ExecutorHTTP, false)
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
		v.criteriaOf(path+".results", rows, ExecutorArgv, false)
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
	if a.RootLife != nil {
		var qualifiers []string
		if c.Address != nil {
			qualifiers = c.Address.Qualifiers
		}
		for _, f := range []struct{ name, env string }{
			{"pidEnv", a.RootLife.PIDEnv}, {"startEnv", a.RootLife.StartEnv}, {"bootIdEnv", a.RootLife.BootIDEnv},
		} {
			if !envNamePattern.MatchString(f.env) || !slices.Contains(qualifiers, f.env) {
				v.add(RuleInvalid, path+".rootLife."+f.name, "%q is not a qualifier of the address", f.env)
			}
		}
	}
	if a.Command != nil && a.Command.HTTPGet != "" {
		if len(a.Command.Argv) > 0 {
			v.add(RuleInvalid, path+".command", "a check is a command or a GET, not both")
		}
		if len(a.Command.Capture) > 0 {
			v.add(RuleInvalid, path+".command.capture", "a GET check captures nothing")
		}
		v.httpGetURL(path+".command.httpGet", a.Command.HTTPGet, c.Executor)
	} else if a.Command != nil {
		v.formatArgv(path+".command.argv", a.Command.Argv)
	}
	if a.Command != nil {
		if len(a.Command.Results) == 0 {
			v.add(RuleInvalid, path+".command.results", "the addressee command needs its result table")
		}
		rows := make([]Match, len(a.Command.Results))
		for i, r := range a.Command.Results {
			rows[i] = r.Match
			if !addresseeResults[r.Result] {
				v.add(RuleUnknownOutcome, fmt.Sprintf("%s.command.results[%d].result", path, i), "%q is not same or changed: a check ends no addressee", r.Result)
			}
		}
		v.signs(path+".command.results", rows)
		kind := ExecutorArgv
		if a.Command.HTTPGet != "" {
			kind = ExecutorHTTP
		}
		v.criteriaOf(path+".command.results", rows, kind, false)
		if len(a.Command.Capture) > 0 && !slices.ContainsFunc(a.Command.Results, func(r AddresseeResultRow) bool {
			return r.Result == AddresseeSame
		}) {
			// The preflight captures only from an answer "same": without
			// such a row the channel could never be used.
			v.add(RuleInvalid, path+".command.capture", "a capture needs a row that answers same")
		}
		seen := map[string]bool{}
		for i, p := range a.Command.Capture {
			cpath := fmt.Sprintf("%s.command.capture[%d]", path, i)
			v.jsonPath(cpath, p)
			if seen[p] {
				v.add(RuleInvalid, cpath, "path %q is captured twice", p)
			}
			seen[p] = true
		}
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
