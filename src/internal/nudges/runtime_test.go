package nudges

// runtime_test.go — sk-303.14, task T020.
// Matrix rows: 87, 117.
//
// This checks the worker's WIRING: independence from the bus, a clean stop,
// and the batch limits as a declared environment contract.

import (
	"context"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	envparse "github.com/caarlos0/env/v11"

	"skein-api/internal/config"
	"skein-api/internal/review"
)

// TestScheduler_StartsWithoutTheBusAndStopsCleanly — matrix row 87.
//
// TWO halves, and both are observable.
//
// First: the worker is built and runs knowing nothing of the bus. The
// assertion is held by the form, not by mood: Config and NewScheduler have
// no place a bus connection could be passed into, and a tick forms a
// reminder with NATS deliberately not up. Exactly that form is the defence
// — a worker bound to bus availability would stay silent in a deployment
// with an empty NATS_URL.
//
// Second: a stop does not leave an unfinished transaction. Checked by the
// state of CONNECTIONS in the database itself: after context cancellation
// and Run's exit, no connection of this application must hang in the
// "idle in transaction" state. This is an observable fact of the server,
// not "there seems to be a defer Rollback in the code".
func TestScheduler_StartsWithoutTheBusAndStopsCleanly(t *testing.T) {
	e := newEnv(t)
	author, _ := e.user(t)
	worker, workerName := e.user(t)
	e.seed(t, issue{suffix: "runtime", status: "open", assignee: workerName,
		assigneeSetAt: ago(25 * time.Minute), createdBy: author})

	// There is no bus in this run at all: the harness brings up only
	// Postgres. The worker is indifferent to that by construction.
	if url := os.Getenv("NATS_URL"); url != "" {
		t.Logf("NATS_URL is set (%s), but it is still not passed to the worker", url)
	}

	sc := NewScheduler(e.store, Config{Interval: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc.Run(ctx)
	}()

	// Wait for the first result of work — the tick fires immediately on
	// start.
	deadline := time.Now().Add(10 * time.Second)
	for e.notifications(t, worker, review.CategoryUnaccepted) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("row 87: the worker did not form a reminder in 10 seconds — " +
				"it either did not start, or is waiting for something")
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("row 87: the worker did not finish in 10 seconds after context cancellation")
	}

	var stuck int
	if err := e.pool.QueryRow(context.Background(), `
SELECT count(*) FROM pg_stat_activity
 WHERE datname = current_database()
   AND state = 'idle in transaction'`).Scan(&stuck); err != nil {
		t.Fatalf("reading connection state: %v", err)
	}
	if stuck != 0 {
		t.Errorf("row 87: after the stop %d connections remain in the "+
			"\"idle in transaction\" state — the tick transaction is not finished", stuck)
	}
}

// TestConfig_FactoryLimitsApplyWithNoEnvironment — matrix row 117, first
// part.
//
// The VALUES themselves are checked, not "something non-zero": a batch and
// a batch-count swapped, like a zero limit, would pass an "both greater
// than zero" check green and behave completely differently.
//
// The expectation is taken from the worker package's constants. The numbers
// in the environment tags and these constants are one decision in two
// places (the second is needed so the worker is operational even when built
// past the environment parse); this check is the gate that holds them
// together, instead of attentiveness.
func TestConfig_FactoryLimitsApplyWithNoEnvironment(t *testing.T) {
	cfg := parseNudgeConfig(t, nil)

	if cfg.NudgeInterval != DefaultInterval {
		t.Errorf("row 117: factory tick interval %s, expected %s",
			cfg.NudgeInterval, DefaultInterval)
	}
	if cfg.NudgeBatchSize != DefaultBatchSize {
		t.Errorf("row 117: factory BATCH PER TICK = %d, expected %d",
			cfg.NudgeBatchSize, DefaultBatchSize)
	}
	if cfg.NudgeBatchesPerTick != DefaultBatchesPerTick {
		t.Errorf("row 117: factory BATCHES PER TICK = %d, expected %d",
			cfg.NudgeBatchesPerTick, DefaultBatchesPerTick)
	}

	// Absence of the variables neither turns the mechanic on nor off: it
	// has no switch in the environment at all — whether it is on is
	// decided by the person in their settings. Checked by absence:
	// appearance of such a variable must turn this gate red, not ride
	// through in silence.
	for _, name := range nudgeEnvNames {
		if !hasEnvTag(t, name) {
			t.Errorf("row 117: variable %s disappeared from the config contract", name)
		}
	}

	// ⚠ CORRECTED TAUTOLOGICAL GUARD (repair A/8, finding of BOTH critics).
	//
	// This used to be `if n := len(nudgeEnvNames); n != 3` — an assertion
	// about the length of ITS OWN literal, sitting a line below in this
	// same file. It could never fire: a mutation of "add a fourth
	// SKEIN_NUDGE_* field to config.Config" stayed green, and the comment
	// promised that "an appearing fourth must be considered, not accepted
	// in silence".
	//
	// Now the SOURCE is counted: the env tags in config.Config with the
	// SKEIN_NUDGE_ prefix. The list below remains an expectation, not a
	// measurement, and a divergence from the source is named by name — so
	// it is visible not only "how many" but "which one".
	declared := nudgeEnvTagsInConfig(t)
	if len(declared) != len(nudgeEnvNames) {
		t.Errorf("row 117: config.Config declares %d SKEIN_NUDGE_* variables (%v), "+
			"and %d are considered here (%v). An appearing variable must be "+
			"considered, not accepted in silence: it has its own default, its own "+
			"limit and its own effect on the batch bounds",
			len(declared), declared, len(nudgeEnvNames), nudgeEnvNames)
	}
	for _, name := range declared {
		if !slices.Contains(nudgeEnvNames, name) {
			t.Errorf("row 117: variable %s is declared in config.Config, but this "+
				"gate has not considered it", name)
		}
	}
}

// nudgeEnvTagsInConfig returns the worker's environment-variable names AS
// THEY ARE DECLARED IN THE SOURCE — from the `env` tags of config.Config
// fields with the SKEIN_NUDGE_ prefix.
//
// Precisely the source, not a copy: a guard that counts the length of its
// own list checks itself and stays silent exactly when it should shout.
//
// ⚠ COVERAGE BOUND (recorded by the second round of review A/8). The walk
// goes ONLY over top-level fields and does not descend into nested structs.
// There is no hole today — config.Config has no nested structs at all, and
// that is not an assumption, it is a measurement of the neighbouring
// assertion (TestConfig_EnvContractIsFlatSoTheGateSeesAllOfIt). A nested
// group with env tags appearing would make the guard stop seeing part of
// the source, and in silence: so the flatness of the contract is checked,
// not implied. Recursion is not introduced in advance for the same reason
// guessed indexes are not: it would be written against extrapolation.
func nudgeEnvTagsInConfig(t *testing.T) []string {
	t.Helper()
	const prefix = "SKEIN_NUDGE_"
	var out []string
	rt := reflect.TypeOf(config.Config{})
	for i := range rt.NumField() {
		tag := rt.Field(i).Tag.Get("env")
		if tag == "" {
			continue
		}
		// A tag may carry options after a comma (`env:"NAME,required"`).
		name, _, _ := strings.Cut(tag, ",")
		if strings.HasPrefix(name, prefix) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// TestConfig_EnvContractIsFlatSoTheGateSeesAllOfIt — the premise of the
// guard above, checked, not implied (repair A/8, second round).
//
// nudgeEnvTagsInConfig reads tags of TOP-LEVEL FIELDS and does not descend
// into nested structs. While there are none — coverage is complete; if they
// appear — the guard will lose part of the source and will not say a word
// about it. The assertion turns "there are none" from an assumption into a
// measurement: a nested struct (or a pointer to one, or an embedding) must
// go red HERE, not quietly take part of the contract out from under the
// check.
//
// Strings, slices, maps and named scalar types are not counted as structs:
// they have no fields with env tags, and the parse does not walk them.
func TestConfig_EnvContractIsFlatSoTheGateSeesAllOfIt(t *testing.T) {
	rt := reflect.TypeOf(config.Config{})
	for i := range rt.NumField() {
		f := rt.Field(i)
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() != reflect.Struct {
			continue
		}
		// time.Duration and the like are not structs; only a real group of
		// fields lands here.
		t.Errorf("field %s has struct type %s: the config contract is no longer "+
			"flat. nudgeEnvTagsInConfig walks only the top level, so the tags "+
			"inside this group have dropped out from under the check — and dropped "+
			"out in silence. Either bring the fields back to the top level, or "+
			"teach the walk recursion and rewrite this gate", f.Name, ft)
	}
}

// TestConfig_EachLimitIsOverriddenIndependently — matrix row 117, second
// part.
//
// Each limit is overridden SEPARATELY, and the neighbours stay at factory
// values. The check is specifically separate: a parse that glued two
// variables into one would look sound on "both are set".
func TestConfig_EachLimitIsOverriddenIndependently(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   map[string]string
		check func(t *testing.T, c config.Config)
	}{
		{
			name: "batch only",
			env:  map[string]string{"SKEIN_NUDGE_BATCH_SIZE": "7"},
			check: func(t *testing.T, c config.Config) {
				if c.NudgeBatchSize != 7 {
					t.Errorf("batch not overridden: %d", c.NudgeBatchSize)
				}
				if c.NudgeBatchesPerTick != DefaultBatchesPerTick {
					t.Errorf("batch count drifted from factory: %d", c.NudgeBatchesPerTick)
				}
				if c.NudgeInterval != DefaultInterval {
					t.Errorf("interval drifted from factory: %s", c.NudgeInterval)
				}
			},
		},
		{
			name: "batch count only",
			env:  map[string]string{"SKEIN_NUDGE_BATCHES_PER_TICK": "3"},
			check: func(t *testing.T, c config.Config) {
				if c.NudgeBatchesPerTick != 3 {
					t.Errorf("batch count not overridden: %d", c.NudgeBatchesPerTick)
				}
				if c.NudgeBatchSize != DefaultBatchSize {
					t.Errorf("batch drifted from factory: %d", c.NudgeBatchSize)
				}
			},
		},
		{
			name: "interval only",
			env:  map[string]string{"SKEIN_NUDGE_INTERVAL": "15s"},
			check: func(t *testing.T, c config.Config) {
				if c.NudgeInterval != 15*time.Second {
					t.Errorf("interval not overridden: %s", c.NudgeInterval)
				}
				if c.NudgeBatchSize != DefaultBatchSize {
					t.Errorf("batch drifted from factory: %d", c.NudgeBatchSize)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t, parseNudgeConfig(t, tc.env))
		})
	}
}

// TestScheduler_EmptyLimitsFallBackToTheSameFactoryValues — the second
// support of matrix row 117.
//
// The environment parse is not the only path of constructing the worker,
// and the second path must give THE SAME factory values. Otherwise the
// config test would confirm one set of numbers, and a worker built without
// it would run on others.
func TestScheduler_EmptyLimitsFallBackToTheSameFactoryValues(t *testing.T) {
	sc := NewScheduler(nil, Config{})
	if sc.cfg.Interval != DefaultInterval ||
		sc.cfg.BatchSize != DefaultBatchSize ||
		sc.cfg.BatchesPerTick != DefaultBatchesPerTick {
		t.Errorf("empty config gave (%s, %d, %d), expected (%s, %d, %d)",
			sc.cfg.Interval, sc.cfg.BatchSize, sc.cfg.BatchesPerTick,
			DefaultInterval, DefaultBatchSize, DefaultBatchesPerTick)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// nudgeEnvNames — the worker's environment variables, the EXPECTATION.
//
// The list is handwritten on purpose: it is not a measurement, it is what
// the measured source is checked against (nudgeEnvTagsInConfig reads the
// config.Config tags). A divergence is named by name, so it is visible not
// only "how many" but "which one".
//
// ⚠ CORRECTED COMMENT (repair A/8, second round). This used to say that
// "the neighbouring check requires there to be exactly three, so a fourth
// variable will not appear in silence". That check DOES NOT EXIST: an
// earlier commit removed it as tautological — it compared the length of
// this same literal with the three sitting a line below, and could never
// go red. The comment remained and kept promising a guard that is already
// gone. The guard is now a different one and sits above: a check against
// the SOURCE.
var nudgeEnvNames = []string{
	"SKEIN_NUDGE_INTERVAL",
	"SKEIN_NUDGE_BATCH_SIZE",
	"SKEIN_NUDGE_BATCHES_PER_TICK",
}

// parseNudgeConfig parses the config, leaving in the environment ONLY the
// required variables and what the case itself asks for. The required ones
// are substituted because without them the parse refuses entirely — and
// that is correct: addresses and keys must have no silent default.
func parseNudgeConfig(t *testing.T, set map[string]string) config.Config {
	t.Helper()
	for _, name := range nudgeEnvNames {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	t.Setenv("DATABASE_URL", "postgres://stub/stub")
	t.Setenv("SKEIN_INTERNAL_SECRET", "stub")
	for k, v := range set {
		t.Setenv(k, v)
	}
	var cfg config.Config
	if err := envparse.Parse(&cfg); err != nil {
		t.Fatalf("parsing config: %v", err)
	}
	return cfg
}

// hasEnvTag — whether the variable is declared in the config contract.
// Checked by parse behaviour, not by reflection over tags: a value from
// the environment must reach the field.
func hasEnvTag(t *testing.T, name string) bool {
	t.Helper()
	probe := map[string]string{
		"SKEIN_NUDGE_INTERVAL":         "42s",
		"SKEIN_NUDGE_BATCH_SIZE":       "42",
		"SKEIN_NUDGE_BATCHES_PER_TICK": "42",
	}[name]
	cfg := parseNudgeConfig(t, map[string]string{name: probe})
	switch name {
	case "SKEIN_NUDGE_INTERVAL":
		return cfg.NudgeInterval == 42*time.Second
	case "SKEIN_NUDGE_BATCH_SIZE":
		return cfg.NudgeBatchSize == 42
	case "SKEIN_NUDGE_BATCHES_PER_TICK":
		return cfg.NudgeBatchesPerTick == 42
	}
	return false
}
