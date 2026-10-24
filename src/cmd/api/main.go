// Command api is the entry point for skein-api — Skein's Go backend service.
//
// It loads configuration from the environment, sets up the slog dual-format
// logger, connects to Postgres and applies pending migrations, builds the
// chi router with health/ready/metrics endpoints, and runs an HTTP server
// with graceful shutdown on SIGTERM/SIGINT.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

	"skein-api/internal/accounts"
	"skein-api/internal/apikeys"
	"skein-api/internal/attachments"
	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/config"
	"skein-api/internal/dashboard"
	"skein-api/internal/dependencies"
	"skein-api/internal/handler"
	"skein-api/internal/invitations"
	"skein-api/internal/issues"
	"skein-api/internal/issues/lockreap"
	"skein-api/internal/logging"
	mcppkg "skein-api/internal/mcp"
	"skein-api/internal/monitorcreds"
	"skein-api/internal/notifications"
	"skein-api/internal/nudges"
	"skein-api/internal/oauthmeta"
	"skein-api/internal/operator"
	"skein-api/internal/outbox"
	"skein-api/internal/projects"
	"skein-api/internal/push"
	"skein-api/internal/pwned"
	"skein-api/internal/reminders"
	"skein-api/internal/search"
	"skein-api/internal/signals"
	"skein-api/internal/spaces"
	"skein-api/internal/storage"
	"skein-api/internal/themes"
	"skein-api/internal/users"
)

// shutdownTimeout bounds the graceful shutdown phase. Must be smaller than
// the Kubernetes terminationGracePeriodSeconds so in-flight requests can
// drain cleanly before SIGKILL.
//
// sk-303.16 finding: the API deployment manifest
// set no terminationGracePeriodSeconds, which meant the cluster default of
// 30s applied. This constant used to also BE 30s — equal, not smaller,
// despite what its own comment said. With both clocks starting at the same
// SIGTERM instant, srv.Shutdown's own deadline and the kubelet's SIGKILL
// landed within scheduling jitter of each other: on a timeout this code
// path called os.Exit(1) immediately (skipping the NATS-drain / runner
// teardown below), so there was effectively no slack for that exit to
// complete, let alone for a graceful drain to finish, before SIGKILL could
// hit first. Lowered to a real margin below the 30s default so the
// documented invariant was actually true again.
//
// sk-353 — worst-case budget of the FULL shutdown chain (this timeout plus
// everything registered in runTeardown below), and what changed:
//
//   - MEASURED, not assumed: a freshly built binary against a real
//     Postgres + NATS, SIGTERM sent while idle (no in-flight requests, all
//     background workers healthy) — the entire chain (srv.Shutdown → NATS
//     drain → all three runner awaits) completed in ~8ms. Repeated with
//     NATS killed out from under the process before SIGTERM: nats.go's
//     Drain() detected the dead connection and returned an error
//     immediately (logged as "nats drain error") rather than blocking on
//     the 10s ceiling — total was ~13ms. Neither run got anywhere near a
//     ceiling; the ceilings only matter when something genuinely fails to
//     observe cancellation, which these two runs did not exercise.
//   - THEORETICAL WORST CASE (something IS stuck): shutdownTimeout's own
//     25s, plus runTeardown's now-PARALLEL fan-out of the NATS drain and
//     the three runner awaits — each individually still capped at
//     runnerShutdownTimeout/10s, but no longer summed because they run
//     concurrently on one WaitGroup (see runTeardown's comment for why
//     the previous two-defer shape made them sequential by accident).
//     Worst case is now 25 + max(10, 10, 10, 10) = 35s, down from the
//     previous 25 + (10 + 10×3) = 65s.
//   - ROUND 2 (found in review, same sk-353): the "10" for the NATS leg
//     above was not actually true on first landing — the leg's own wait
//     on dispatcherDone (the outbox dispatcher stopping) had NO ceiling
//     at all, so a dispatcher that missed workerCtx's cancellation hung
//     that goroutine, and therefore wg.Wait(), forever; on the
//     srv.Shutdown-error branch that meant os.Exit(1) was never reached
//     either. Fixed by folding the dispatcher-stop wait into the SAME
//     natsDrainTimeout budget the drain-completion wait already used,
//     via one shared timer covering both (see runTeardown's NATS-leg
//     comment) — not two independent 10s ceilings chained end to end,
//     which would have silently doubled this leg's worst case to 20s and
//     the total below to 45s. With the shared timer the leg's own worst
//     case is genuinely natsDrainTimeout (10s) again, so the arithmetic
//     below is unchanged by this round.
//     Worst case is still 25 + max(10, 10, 10, 10) = 35s.
//   - terminationGracePeriodSeconds is set to 45s in the API deployment
//     manifest (it lives in the deployment repository, not this one —
//     sk-353) — 10s of real margin over the 35s
//     worst case, not a value equal to or below it. Change either number
//     and re-check the other: this margin is the whole point of setting
//     terminationGracePeriodSeconds explicitly instead of leaving it to
//     the cluster default (see the sk-303.16 paragraph above for what
//     "explicit vs. implicit default" cost the previous time nobody
//     re-checked it).
const shutdownTimeout = 25 * time.Second

// dbConnectTimeout caps the very first Postgres connection on startup. With
// no upper bound, a wrong DATABASE_URL or unreachable host hangs the pod
// until K8s liveness eventually kills it; 30s gives a clear "couldn't connect"
// error on stdout instead.
const dbConnectTimeout = 30 * time.Second

// runnerShutdownTimeout bounds the wait for a background runner to exit after
// its context is cancelled. Same ceiling as the NATS drain below, and for the
// same reason: a wait with no ceiling turns a runner that missed cancellation
// into a pod that never exits and is SIGKILLed mid-transaction instead of
// finishing its own rollback.
//
// A var, not a const, and only for one reason (repair A/8, second round): the
// assertion that the ceiling exists must run THIS function, not a copy of it
// with an injectable timeout. Ten seconds of real waiting per test is not a
// price worth paying, so the test lowers this and restores it. Nothing else
// writes it — main() reads it once during shutdown.
//
// ⚠ CONSTRAINT THAT COMES WITH THE var (recorded at repair A/8, third round —
// the observation, not a change): a package-level variable written by a test is
// shared mutable state, and awaitRunner reads it from ANOTHER goroutine. Today
// that is safe for exactly one reason — no test in ./cmd/api calls
// t.Parallel(), so the lower-and-restore in TestAwaitRunner_* never overlaps
// another test. The safety is in the ABSENCE of parallelism, not in anything
// this code does; the first t.Parallel() added to this package makes the write
// race with awaitRunner's read (a real -race failure, and a flaky ceiling for
// whoever is running concurrently). Add parallelism here and the ceiling must
// stop being a package var first — pass it in, or guard it.
var runnerShutdownTimeout = 10 * time.Second

// natsDrainTimeout bounds the ENTIRE NATS leg of runTeardown as ONE shared
// budget, not two independent windows chained end to end: waiting for the
// outbox dispatcher to report it has stopped (so no further Publish calls
// can be issued) AND waiting for nc.Drain()'s async close to finish,
// together. runTeardown's NATS-leg goroutine starts a single timer against
// this value and reads from it in both waits, so a dispatcher that takes
// most of the budget to stop leaves correspondingly less of it for the
// drain to complete — the leg's own worst case stays natsDrainTimeout,
// never 2×natsDrainTimeout. See runTeardown's NATS-leg comment for why a
// shared timer instead of two fresh ones matters to the arithmetic in
// shutdownTimeout's doc comment above. Same value and same reasoning as
// runnerShutdownTimeout otherwise — a wait with no ceiling turns a stuck
// drain into a pod that never exits — and the same var-not-const /
// single-threaded-tests constraint applies (sk-353 reused the pattern
// rather than inventing a second one; see runnerShutdownTimeout's doc for
// the full explanation of why it must stay a var and why that is only safe
// absent t.Parallel() in this package).
var natsDrainTimeout = 10 * time.Second

// awaitRunner blocks until the runner closes its done channel, or the ceiling
// expires — in which case it says so and moves on. Saying so matters: an
// exceeded ceiling is the only external evidence that a runner ignores
// cancellation, and swallowing it would leave the next incident with nothing
// to read.
//
// Both halves are held by TestAwaitRunner_* (repair A/8, second round): until
// then the mutation "replace the select with a bare <-done" — which removes
// the ceiling AND the evidence at once — was green everywhere, ./cmd/api
// included.
func awaitRunner(name string, done <-chan struct{}) {
	select {
	case <-done:
	case <-time.After(runnerShutdownTimeout):
		slog.Warn("background runner did not stop within the shutdown ceiling",
			"runner", name, "ceiling", runnerShutdownTimeout)
	}
}

// teardownDeps bundles everything runTeardown needs to fan the shutdown
// wait out across its independent legs. A struct, not five-plus
// positional parameters of the same channel type — main() and
// TestRunTeardown_* both name each field, so a channel can't be
// transposed into the wrong slot by argument order.
type teardownDeps struct {
	// stopWorker cancels workerCtx, the shared context every background
	// runner and the outbox dispatcher select on. Called exactly once, at
	// the very start of runTeardown, so every leg below observes
	// cancellation at the same instant.
	stopWorker func()

	// pushRunnerDone / cleanupDone / nudgeDone close when the
	// correspondingly named background goroutine (main()'s pushRunner,
	// cleanupRunner, nudgeScheduler) returns after observing workerCtx's
	// cancellation.
	pushRunnerDone <-chan struct{}
	cleanupDone    <-chan struct{}
	nudgeDone      <-chan struct{}

	// notificationWorkersDone closes when every worker started by
	// startNotificationWorkers (sk-622.21: the retention sweep and the
	// assignee re-resolution) has
	// returned. nil skips the leg — main always sets it; tests that build
	// teardownDeps for other legs leave it out.
	notificationWorkersDone <-chan struct{}

	// emailWorkersDone closes when every worker started by
	// startEmailWorkers (sk-674.18 T022: the mail worker and the mail
	// cleanup) has returned. nil skips the leg — main always sets it.
	emailWorkersDone <-chan struct{}

	// countsGateDone closes when every batch computation of the
	// header-counts gate (handler/countsgate.go) has returned after
	// workerCtx is cancelled. nil skips the leg — main always sets it.
	countsGateDone <-chan struct{}

	// nc is the NATS connection to drain. nil disables the NATS leg
	// entirely (NATS_URL unset, or connectNATS failed) — runTeardown
	// then waits only on the three runner legs.
	nc *nats.Conn
	// dispatcherDone closes once the outbox dispatcher goroutine has
	// returned (so no further Publish calls can be in flight when Drain
	// is called). natsClosed closes once nc's own disconnect handler
	// observes the drain actually finishing on the wire.
	dispatcherDone <-chan struct{}
	natsClosed     <-chan struct{}
}

// runTeardown fans the shutdown-phase waits out across goroutines joined
// by one sync.WaitGroup and blocks until every leg finishes or hits its
// own ceiling.
//
// sk-353 — this replaces what used to be TWO separate `defer`s in main():
// one waiting on the three runners, one waiting on the NATS drain. None
// of the four waits reads another's done channel — they are genuinely
// independent — but Go's LIFO defer ordering made them run SEQUENTIALLY
// anyway, purely as an accident of "two defers happen to execute in
// registration-reverse order," not because anything depended on that
// order. The real worst case was therefore their SUM: ≤10s (NATS) +
// ≤10s×3 (runners, awaited one after another) = ≤40s, stacked on top of
// shutdownTimeout's own 25s ceiling for a theoretical total of ≤65s —
// comfortably past any terminationGracePeriodSeconds a reasonable deploy
// would set. See shutdownTimeout's doc comment for the measured (not
// theoretical) numbers this was actually costing, and for the
// terminationGracePeriodSeconds value chosen to replace the old implicit
// cluster default.
//
// Run the four legs concurrently instead: stopWorker cancels workerCtx
// once, all four goroutines start observing that same cancellation
// instant, and the combined phase is bounded by whichever ONE of them
// takes longest — ≤10s in the worst case (runnerShutdownTimeout and
// natsDrainTimeout are both currently 10s), not ≤40s.
//
// Callers needing this to run exactly once despite being invoked from
// two different code paths (the normal deferred teardown, and the
// srv.Shutdown-error branch that also wants a chance at it before
// os.Exit) are responsible for their own sync.Once — see main()'s
// teardownOnce/runTeardownOnce. runTeardown itself is unconditionally
// safe to call more than once (every leg only reads from channels /
// calls Drain, which nats.go itself tolerates being called again), but a
// second call duplicates the wait for no benefit, hence the caller-side
// Once.
func runTeardown(d teardownDeps) {
	d.stopWorker()
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); awaitRunner("push runner", d.pushRunnerDone) }()
	go func() { defer wg.Done(); awaitRunner("push cleanup runner", d.cleanupDone) }()
	go func() { defer wg.Done(); awaitRunner("nudge scheduler", d.nudgeDone) }()
	if d.notificationWorkersDone != nil {
		wg.Add(1)
		go func() { defer wg.Done(); awaitRunner("notification workers", d.notificationWorkersDone) }()
	}
	if d.emailWorkersDone != nil {
		wg.Add(1)
		go func() { defer wg.Done(); awaitRunner("email workers", d.emailWorkersDone) }()
	}
	if d.countsGateDone != nil {
		wg.Add(1)
		go func() { defer wg.Done(); awaitRunner("header counts gate", d.countsGateDone) }()
	}
	if d.nc != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Ordered WITHIN this one goroutine (unchanged from before):
			// dispatcherDone before Drain before natsClosed. Only the
			// ACROSS-goroutine ordering against the three runner awaits
			// changed.
			//
			//   1. workerCtx already cancelled (by stopWorker above) →
			//      dispatcher.Run observes ctx.Done and exits its loop
			//      after the current tick.
			//   2. block on dispatcherDone, sharing the SAME deadline as
			//      step 4 below → no new Publish calls can be issued once
			//      we move on.
			//   3. nc.Drain() kicks off the async drain (returns nil
			//      immediately on success).
			//   4. block on natsClosed, against whatever is LEFT of the
			//      shared deadline after step 2 → guarantees in-flight
			//      publishes either flushed or hit the timeout before the
			//      process exits.
			//
			// sk-353, found in review — step 2 used to be a
			// bare `<-d.dispatcherDone` with NO ceiling at all: a
			// dispatcher that somehow missed workerCtx's cancellation
			// (future regression, a stuck DB call, whatever) hung this
			// goroutine — and therefore wg.Wait() — forever. On the
			// srv.Shutdown-error branch that means main() never reaches
			// os.Exit(1) either. That directly contradicted this leg's
			// own advertised natsDrainTimeout ceiling and, through it,
			// shutdownTimeout's "35s worst case" doc comment above.
			//
			// Fixed with ONE shared timer covering BOTH waits, not two
			// independent fresh natsDrainTimeout windows chained
			// end to end. Two independent ceilings would let this single
			// goroutine run for up to 2×natsDrainTimeout (20s at the
			// current value) — silently doubling the leg's contribution
			// to the "max(10, 10, 10, 10) = 35s" arithmetic to 45s, which
			// is exactly the deployed terminationGracePeriodSeconds (zero
			// margin left). A shared deadline keeps this leg's own worst
			// case at natsDrainTimeout, matching every other leg.
			//
			// On a step-2 timeout: do NOT call nc.Drain() and do NOT wait
			// on natsClosed. Waiting longer buys nothing — a dispatcher
			// that ignored cancellation might still be mid-Publish, so
			// nothing stops it enqueuing another message the instant
			// after Drain() starts, regardless of how long this goroutine
			// waited first — and every outbox event is durably recorded
			// in Postgres BEFORE the dispatcher ever calls Publish (see
			// outbox.New's doc comment: "events continue to be written to
			// the audit table for the next replay window"), so a message
			// this abnormal dispatcher never got to publish is not lost —
			// the next process's dispatcher republishes it on its first
			// tick.
			deadline := time.NewTimer(natsDrainTimeout)
			defer deadline.Stop()

			select {
			case <-d.dispatcherDone:
			case <-deadline.C:
				slog.Warn("outbox dispatcher did not stop within the shutdown ceiling; skipping nats drain",
					"ceiling", natsDrainTimeout)
				return
			}

			if err := d.nc.Drain(); err != nil {
				slog.Warn("nats drain error", "error", err.Error())
				return
			}
			select {
			case <-d.natsClosed:
			case <-deadline.C:
				slog.Warn("nats drain did not complete within the shutdown ceiling",
					"ceiling", natsDrainTimeout)
			}
		}()
	}
	wg.Wait()
}

// operatorServer is the operator's API server built from cfg (sk-674.18
// T042, US-11): nil while SKEIN_OPERATOR_API_ENABLED=false — nothing listens
// on SKEIN_OPERATOR_API_PORT — otherwise a server on that port whose router
// carries only the operator's routes behind SKEIN_OPERATOR_API_KEY.
func operatorServer(cfg *config.Config, ops operator.Operations) *http.Server {
	if !cfg.OperatorAPIEnabled {
		return nil
	}
	return operator.NewServer(":"+cfg.OperatorAPIPort, operator.NewRouter(cfg.OperatorAPIKey, ops))
}

// main wires and runs skein-api.
//
// swag general API info. Only the security schemes are declared here; the
// rest of the general block (title, version) is not maintained in this
// service yet.
//
// @securityDefinitions.apikey InternalSecretAuth
// @in header
// @name X-Skein-Internal-Secret
// @description Service secret shared by skein-web and skein-api. Service-only endpoints (POST /api/v1/hub/project-access, GET /api/v1/internal/instance-config) are authorized by it alone, with no user headers.
func main() {
	cfg, err := config.Load()
	if err != nil {
		logConfigFailure(os.Stdout, err)
		os.Exit(1)
	}

	setupLogger(cfg)

	slog.Info("starting skein-api",
		"port", cfg.Port,
		"environment", cfg.Environment,
		"logLevel", cfg.LogLevel,
	)

	// sk-304 review F1 — OAuth discovery is optional and degrades quietly, so
	// say out loud when it is off. Without this line a deployment missing the
	// variable looks identical to a healthy one until an MCP client fails to
	// discover anything, which is the invisible-failure shape that let sk-280
	// be closed on a green probe. /mcp itself is unaffected either way: a
	// static `ska_` api key on X-Skein-Api-Key is the live credential.
	if !oauthmeta.Enabled(cfg.OAuthAuthorizationServers) {
		slog.Warn("oauth protected resource discovery disabled: "+
			"SKEIN_OAUTH_AUTHORIZATION_SERVERS is not set, so "+
			"/.well-known/oauth-protected-resource[/mcp] is not served and 401s "+
			"from /mcp carry no WWW-Authenticate challenge; MCP clients with a "+
			"static api key are unaffected",
			"wellKnownPath", oauthmeta.WellKnownRootPath,
		)
	}

	// Connect to Postgres before doing anything else: a missing/broken DB is
	// fatal and we want to fail fast with a clear error rather than serve
	// /healthz successfully while every business endpoint 500s.
	connectCtx, cancelConnect := context.WithTimeout(context.Background(), dbConnectTimeout)
	pool, err := storage.NewPool(connectCtx, cfg.DatabaseURL, cfg.DatabaseMaxConns)
	cancelConnect()
	if err != nil {
		slog.Error("failed to connect to database", "error", err.Error())
		os.Exit(1)
	}
	defer pool.Close()
	// Pool metrics (skein_db_pool_*) read this pool's Stat() at scrape time.
	storage.ObservePool(pool)
	slog.Info("connected to database")

	// Apply pending migrations in-process. The project guidelines on migrations
	// allows this for dev workflow; in production it is also safe because
	// golang-migrate uses an advisory lock so multi-replica startup races
	// resolve to a single applier — the others see ErrNoChange.
	//
	// sk-674.18 (K-19, K-2) — the migration runs inside PrepareDatabase:
	// settings that contradict the data refuse startup before the schema is
	// touched or right after it, and the admin flag is written into
	// instance_settings for the database access function.
	if err := config.PrepareDatabase(context.Background(), cfg, pool, func() error {
		return storage.RunMigrations(cfg.DatabaseURL, cfg.MigrationsPath)
	}); err != nil {
		slog.Error("database startup failed", "error", err.Error())
		os.Exit(1)
	}

	// sk-303.12 — warn, once, if this cluster's ctype cannot classify
	// non-ASCII letters (locale C/POSIX, or LC_CTYPE=C under any locale
	// provider). This is a Skein-open-source concern, not just ours: a
	// third-party deployment that lands on the wrong locale (easy to do —
	// Alpine-flavoured Postgres images default to C, see
	// DEPLOYMENT-NOTES.md §1) gets a service that starts, migrates, and
	// serves 200s while its duplicate-issue detection silently never
	// compares any non-ASCII title. We stayed on exactly that defect in
	// production for months (sk-228) because nothing said so out loud.
	//
	// Deliberately slog.Warn, not slog.Error+os.Exit: a C-locale cluster
	// with strictly-ASCII content works correctly today, so failing
	// startup over it would be a content-dependent crash for a condition
	// that is legal — the same mistake sk-304 made forcing a required env
	// var to gate the whole process. This check's own failure (e.g.
	// pg_trgm missing) is equally non-fatal; a diagnostic must not be able
	// to take the API down.
	localeCheckCtx, cancelLocaleCheck := context.WithTimeout(context.Background(), 10*time.Second)
	localeWarning, localeCheckErr := dependencies.LocaleWarning(localeCheckCtx, pool)
	cancelLocaleCheck()
	switch {
	case localeCheckErr != nil:
		slog.Warn("locale legibility check failed; continuing without it",
			"error", localeCheckErr.Error())
	case localeWarning != "":
		slog.Warn(localeWarning)
	}

	// R4.2 — S3 client (optional). When S3_URL is empty the
	// attachments subsystem is disabled: every /api/v1/.../attachments/*
	// endpoint returns 503 and the cleanup goroutine is not started.
	// This mirrors how NATS_URL gates the outbox dispatcher, so an
	// offline dev environment can still bring up the rest of the API.
	var s3Client *storage.S3Client
	if cfg.S3URL != "" {
		s3Ctx, cancelS3 := context.WithTimeout(context.Background(), 10*time.Second)
		s3Client, err = storage.NewS3Client(s3Ctx, storage.S3Config{
			Endpoint:       cfg.S3URL,
			PublicEndpoint: cfg.S3PublicURL,
			AccessKey:      cfg.S3AccessKey,
			SecretKey:      cfg.S3SecretKey,
			Bucket:         cfg.S3Bucket,
			UseSSL:         cfg.S3UseSSL,
			PublicUseSSL:   cfg.S3UseSSL,
			Region:         cfg.S3Region,
			CORSOrigins:    cfg.S3CORSAllowedOrigins,
		})
		cancelS3()
		if err != nil {
			slog.Error("failed to connect to S3", "error", err.Error())
			os.Exit(1)
		}
		ensureCtx, cancelEnsure := context.WithTimeout(context.Background(), 10*time.Second)
		if err := s3Client.EnsureBucket(ensureCtx); err != nil {
			cancelEnsure()
			slog.Error("failed to ensure S3 bucket", "error", err.Error())
			os.Exit(1)
		}
		cancelEnsure()
		slog.Info("S3 connected", "bucket", cfg.S3Bucket)
	} else {
		slog.Warn("attachments disabled: S3_URL not set")
		// The MINIO_* variables were renamed to S3_* without a transition.
		// An operator who still sets them would otherwise see attachments
		// switch off with nothing pointing at the cause.
		if os.Getenv("MINIO_URL") != "" {
			slog.Warn("MINIO_URL is set but ignored: the MINIO_* variables were renamed to S3_* (see docs/env-reference.md)")
		}
	}

	// Bootstrap the very first admin user (R1.1). No-op when users exist
	// or when SKEIN_INITIAL_ADMIN_EMAIL isn't set. SERIALIZABLE-protected
	// against multi-replica races; logs at INFO when a user is created.
	bootstrapCtx, cancelBootstrap := context.WithTimeout(context.Background(), 10*time.Second)
	if err := auth.Bootstrap(bootstrapCtx, pool, auth.BootstrapConfig{
		AdminEmail:    cfg.InitialAdminEmail,
		AdminPassword: cfg.InitialAdminPassword,
		AdminUsername: cfg.InitialAdminUsername,
		AdminName:     cfg.InitialAdminName,
		BcryptCost:    cfg.BcryptCost,
	}); err != nil {
		cancelBootstrap()
		slog.Error("admin bootstrap failed", "error", err.Error())
		os.Exit(1)
	}
	cancelBootstrap()

	// sk-674.18 T020 (US-2 "common space outside the transition", INV-22): with
	// SKEIN_COMMON_SPACE_ENABLED the common space exists — created here,
	// after the bootstrap admin, when it is missing and an active admin can
	// own it; otherwise the first admin to appear through SSO creates it.
	commonCtx, cancelCommon := context.WithTimeout(context.Background(), 30*time.Second)
	if err := accounts.EnsureCommonSpace(commonCtx, pool); err != nil {
		cancelCommon()
		slog.Error("creating the common space failed", "error", err.Error())
		os.Exit(1)
	}
	cancelCommon()

	// R5.1 — OIDC auto-provisioner. Wired regardless of AuthMode so the
	// route stays available; the handler short-circuits with 410 when
	// provisioner is nil. AutoProvision=false (default) flips the
	// behaviour to "match-only": unknown subs get 403 user_not_provisioned
	// instead of triggering an INSERT.
	provisioner := auth.NewProvisioner(pool, cfg.AutoProvision, cfg.OIDCDefaultRole, cfg.BootstrapAdminEmail)

	authHandler, err := handler.NewAuthHandler(pool, cfg.BcryptCost, cfg.AuthMode, cfg.BreakGlassAdmin, provisioner)
	if err != nil {
		slog.Error("failed to build auth handler", "error", err.Error())
		os.Exit(1)
	}
	componentsHandler := handler.NewComponentsHandler(pool)

	// R1.5 search wiring. Constructed BEFORE depsStore so we can wire
	// the embedder into dependencies.NewStoreWithEmbedder for R4.1
	// layer-2 vector duplicate detection. We build the embedder + worker
	// only when OLLAMA_URL is set; otherwise the service runs in
	// permanent FTS-only mode (the Result.degraded flag will always be
	// true). This lets a developer run skein-api offline without
	// breaking the /search endpoint shape.
	searchSvc, searchH, err := buildSearchStack(pool, cfg)
	if err != nil {
		slog.Error("failed to build search stack", "error", err.Error())
		os.Exit(1)
	}

	// projectsHandler reuses the same embedder so GET /api/v1/projects?q=
	// can do FTS + semantic rerank when the FTS layer returns thin
	// results. typed-nil dance: avoid passing a nil *search.Embedder
	// boxed in an interface (the classic Go gotcha where iface != nil
	// because the header type pointer is non-nil even with a nil
	// concrete value). When the search service has no embedder we
	// leave projectsEmbedder as the true-nil interface.
	var projectsEmbedder projects.QueryEmbedder
	if e := searchSvc.Embedder(); e != nil {
		projectsEmbedder = e
	}
	projectsHandler := handler.NewProjectsHandler(pool, projectsEmbedder)
	projectsHandler.SetDefaultVisibility(cfg.ProjectDefaultVisibility)
	projectsHandler.SetPrefixMinLength(cfg.ProjectPrefixMinLength)
	projectsHandler.SetProjectQuotas(cfg.QuotaPersonalProjects, cfg.QuotaSpaceProjects)

	// One depsStore feeds three handlers: dependencies CRUD, /ready, and
	// /issues/check-duplicates. Sharing the instance keeps the connection
	// pool wiring single-source.
	//
	// R4.1 — embedder injected via the dependencies.QueryEmbedder
	// interface for layer-2 duplicate detection. *search.Embedder
	// satisfies the interface structurally (EmbedSingle + Available);
	// when searchSvc.Embedder() is nil (no OLLAMA_URL) the store falls
	// back to trigram-only without ever calling out.
	//
	// We must NOT pass a typed-nil *search.Embedder into the interface
	// slot — that's the classic Go gotcha where `iface != nil` because
	// the interface header carries a non-nil type pointer even though
	// the concrete value is nil. Convert the nil case to a true-nil
	// interface by leaving depsEmbedder zero-valued.
	var depsEmbedder dependencies.QueryEmbedder
	if e := searchSvc.Embedder(); e != nil {
		depsEmbedder = e
	}
	depsStore := dependencies.NewStoreWithEmbedder(pool, depsEmbedder)

	// projectsStore reused by issues handler for id_prefix / default_id_mode
	// lookups when minting issue IDs. Building it once and sharing avoids a
	// duplicate *Store allocation per handler.
	issuesStore := issues.NewStore(pool)
	// projectsStore is shared by issues (id_prefix / default_id_mode lookups)
	// and the users handler (GET /me/memberships → ListMembershipsForUser).
	projectsStore := projects.NewStore(pool)
	projectsStore.SetPrefixMinLength(cfg.ProjectPrefixMinLength)
	projectsStore.SetProjectQuotas(cfg.QuotaPersonalProjects, cfg.QuotaSpaceProjects)
	issuesHandler := handler.NewIssuesHandler(pool, projectsStore, depsStore)
	depsHandler := handler.NewDependenciesHandler(pool, depsStore, issuesStore)
	readyHandler := handler.NewReadyHandler(pool, depsStore)
	// R1.6 — watchers + notifications inbox + bulk ops.
	notificationsHandler := handler.NewNotificationsHandler(pool)
	bulkHandler := handler.NewBulkHandler(pool, issuesStore)
	// R3.3 — caller-scoped /me/assigned-by + /me/assigned-to wrappers.
	issuesMeHandler := handler.NewIssuesMeHandler(pool, issuesStore)
	// Sc3 — cross-project /api/v1/issues for the "All projects" Tasks
	// column. Shares the same embedder as the projects FTS path so
	// semantic rerank lights up when the FTS layer returns thin results.
	// typed-nil dance — see the projectsEmbedder block above; we mirror
	// that pattern here so a nil embedder collapses to FTS-only without
	// hitting the iface != nil gotcha.
	var issuesAllEmbedder issues.QueryEmbedder
	if e := searchSvc.Embedder(); e != nil {
		issuesAllEmbedder = e
	}
	issuesAllHandler := handler.NewIssuesAllHandler(pool, issuesStore, issuesAllEmbedder)
	// Phase 2 ui-redesign (B6) — consolidated header counts endpoint.
	// Shared notifications store — REST /me/watching + MCP tools — and the
	// /me handler (header counts, the Watching column), composed with the
	// Watching window from cfg (sk-622.21 US-5, R16: one composition
	// function for main and its wiring test).
	notifStore, meHandler := newNotificationReads(pool, cfg, issuesStore)
	// R3.4 — activity feed (per-project + /me cross-project). Reuses
	// the shared issuesStore + depsStore so visibility resolution lines
	// up with the rest of the /me/* surface.
	activityHandler := handler.NewActivityHandler(pool, issuesStore)
	// sk-17 — dashboard scoring (3-axis × 3-window throughput matrix
	// + leaderboards) and the dashboard activity feed.
	dashboardStore := dashboard.New(pool)
	dashboardHandler := handler.NewDashboardHandler(pool, dashboardStore, issuesStore)
	// R2.1 — long-lived API keys (CRUD via Settings UI). MCP middleware
	// reuses the apikeys.Store directly; the handler is only for /me.
	apiKeysHandler := handler.NewAPIKeysHandler(pool, cfg.BcryptCost)

	// R3.1 — admin user management. usersStore wraps the users package
	// and holds the apikeys.Store reference for the deactivation cascade
	// (Deactivate → RevokeAllForUser in the same SERIALIZABLE tx).
	apikeysStore := apikeys.New(pool)

	// sk-303.20 — Pwned Passwords k-anonymity check for self-service
	// password change. pwnedChecker is deliberately declared as the plain
	// interface zero value and assigned a concrete *pwned.Client ONLY
	// inside the enabled branch: assigning a typed-nil *pwned.Client in the
	// disabled branch would box a non-nil interface (Go's classic
	// typed-nil-in-interface trap), which would make
	// usersStore.PwnedCheckEnabled() report true even though the feature is
	// off. Default is disabled (SKEIN_PWNED_PASSWORDS_ENABLED=false) — see
	// config.Config.PwnedPasswordsEnabled doc comment for why the default
	// must stay off (self-hosted deployments in closed networks).
	var pwnedChecker users.PwnedChecker
	if cfg.PwnedPasswordsEnabled {
		pwnedChecker = pwned.NewClient(cfg.PwnedPasswordsBaseURL, cfg.PwnedPasswordsTimeout)
	}
	usersStore := users.NewStore(pool, cfg.BcryptCost, apikeysStore, cfg.AuthMode, cfg.BreakGlassAdmin, pwnedChecker)
	usersStore.SetSpaceMemberLimit(cfg.SpaceMemberLimit)
	// projectsStore feeds GET /api/v1/me/memberships (R3.2c).
	usersHandler := handler.NewUsersHandler(pool, usersStore, projectsStore)

	// R4.2 — attachments handler. s3Access is nil when S3_URL is
	// unset; every endpoint short-circuits with 503 in that case.
	var s3Access attachments.S3Access
	if s3Client != nil {
		s3Access = &attachments.S3ClientAdapter{C: s3Client}
	}
	attachmentsHandler := handler.NewAttachmentsHandler(pool, s3Access,
		cfg.S3Bucket, cfg.AttachmentMaxSizeBytes, cfg.AttachmentURLTTLSeconds)
	attachmentsHandler.SetStorageQuotas(cfg.QuotaPersonalStorageBytes, cfg.QuotaSpaceStorageBytes)

	// sk-62.2 — reminders. Shared store backs the REST handler, the MCP
	// tools, and (separately, via its own store instance above) the
	// scheduler worker.
	remindersStore := reminders.NewStore(pool)
	remindersHandler := handler.NewRemindersHandler(pool, remindersStore, issuesStore, projectsStore)

	// Web Push foundation — subscription storage + VAPID public-key
	// discovery. The handler degrades gracefully when VAPIDPublic is empty
	// (config returns an empty key; the frontend feature-detects). The Send
	// helper (push.NewSender, push/send.go) is part of this package but is
	// NOT yet constructed/wired into the event/notification fan-out — that
	// dispatcher wiring is a separate ticket.
	pushStore := push.NewStore(pool)
	// push delivery is "enabled" only when BOTH VAPID keys are present —
	// config.Load() has already rejected a half-configured keypair, so this
	// is a clean both-or-neither check. /push/config surfaces the public key
	// only when enabled (a public key with no private key can never deliver).
	pushEnabled := cfg.VAPIDPublic != "" && cfg.VAPIDPrivate != ""
	pushHandler := handler.NewPushHandler(pool, pushStore, cfg.VAPIDPublic, pushEnabled)
	// sk-62.3.2 — the Sender is the delivery side of push: the outbox
	// dispatcher uses it to web-push notifications stamped with a
	// push_category. NewSender is a no-op (Enabled()==false) when VAPID keys
	// are absent, so the dispatcher simply skips push in that case.
	pushSender := push.NewSender(pushStore, cfg.VAPIDPublic, cfg.VAPIDPrivate, cfg.VAPIDSubject)

	// sk-10.1 — user-customizable themes. CRUD over /themes lives on
	// ThemesHandler; the per-user active selection (/me/theme) lives on
	// MeThemeHandler. Both wrap the same shared *themes.Store.
	//
	// sk-10.3 — extended with the inspect cache and the NATS-backed
	// themes publisher. The cache is process-local in-memory with a
	// background cleanup goroutine; the publisher is a no-op when
	// NATS_URL is unset (so the rest of the surface still boots).
	themesStore := themes.NewStore(pool)
	themesHandler := handler.NewThemesHandler(pool)
	inspectCache := themes.NewInspectCache(themes.InspectCacheDefaultTTL)
	meThemeHandler := handler.NewMeThemeHandler(pool, inspectCache)

	// R3.2a outbox dispatcher needs a NATS connection. sk-10.3 also
	// needs one for the themes publisher (skein_theme with
	// action="set_variables" / "preview_variables"). Connect ONCE here, share the
	// *nats.Conn between the dispatcher and the publisher.
	//
	// NATS_URL is intentionally optional: when empty (typical in offline
	// dev / CI) we log a warning, build a no-op publisher (preview /
	// updated calls return preview_unavailable to the agent), and skip
	// the dispatcher entirely. Events continue to be written to the
	// audit table; the next deploy with NATS_URL set drains the
	// backlog on first tick.
	//
	// When NATS_URL is set but the server is down at startup, connectNATS
	// still returns a connection (sk-622.21 R21, RetryOnFailedConnect): it
	// dials in the background, the dispatcher runs and retries its batch
	// on each tick until the connection lands. ncErr is left only for a
	// configuration error that retrying cannot fix.
	natsClosed := make(chan struct{})
	nc, ncErr := connectNATS(cfg, natsClosed)
	if ncErr != nil {
		slog.Error("NATS connect failed; outbox dispatcher disabled",
			"natsUrl", cfg.NATSURL, "error", ncErr.Error())
	}
	themesPublisher := themes.NewPublisher(nc)
	// sk-622.21 T014: personal signals (`skein.user.{uid}.signal`) are
	// published after COMMIT by signals.BeginTxFunc through this one
	// process-wide connection. nil nc (NATS_URL unset, or a configuration
	// error) keeps them a no-op, like the themes publisher. A server that
	// is merely unreachable at startup does NOT make nc nil (R21): signals
	// published before the background connect lands go to the reconnect
	// buffer, best-effort per D6. Configured before the router and the
	// workers start, so no committed write runs ahead of it.
	signals.Configure(nc)

	router, drainMCPStreams := buildRouter(pool, cfg, authHandler, projectsHandler, componentsHandler, issuesHandler, depsHandler, readyHandler, searchH, notificationsHandler, bulkHandler, apiKeysHandler, usersHandler, issuesMeHandler, issuesAllHandler, activityHandler, dashboardHandler, usersStore, issuesStore, depsStore, projectsStore, notifStore, remindersStore, searchSvc, s3Client, attachmentsHandler, meHandler, themesHandler, meThemeHandler, themesStore, themesPublisher, inspectCache, remindersHandler, pushHandler)

	// Embedding backfill worker — single goroutine, lifetime tied to ctx
	// (ServerCtx below).  We keep this simple: one worker per replica,
	// SKIP LOCKED makes multi-replica safe.  The worker is a no-op when
	// the embedder is nil (no Ollama configured).
	//
	// The context is DECLARED server-internal (authz.WithUnrestricted):
	// background workers have no principal by construction and must still
	// see the whole tree — a worker whose reads are horizon-filtered
	// reports the database as empty. Since sk-243's security follow-up an
	// undeclared context sees NOTHING, so the reach the workers need is
	// stated here, once, instead of being inherited from the absence of a
	// principal (which is also what a dropped request context looks like).
	workerCtx, stopWorker := context.WithCancel(authz.WithUnrestricted(context.Background()))
	defer stopWorker()

	// sk-10.3 — inspect cache TTL sweeper. Single goroutine; lifetime
	// tied to workerCtx so SIGTERM stops it alongside the other workers.
	// No-op when no entries exist; cheap when populated.
	inspectCache.StartCleanup(workerCtx)
	if searchSvc != nil && searchSvc.Embedder() != nil {
		go search.RunBackfillWorker(workerCtx, pool, searchSvc.Embedder(),
			cfg.EmbeddingWorkerInterval, cfg.EmbeddingBatchSize)
	} else {
		slog.Warn("embedding worker disabled: OLLAMA_URL not set")
	}

	// R4.2 — attachments cleanup goroutine. Stale-pending sweep + S3
	// orphan reconcile. Skipped entirely when S3 is disabled (no
	// presigned uploads = no pending rows can be created).
	if s3Access != nil {
		go attachments.RunCleanupWorker(workerCtx, pool, s3Access, attachments.CleanupConfig{
			Interval:           cfg.AttachmentCleanupInterval,
			PendingMaxAge:      cfg.AttachmentPendingMaxAge,
			OrphanScanInterval: cfg.AttachmentOrphanScanInterval,
			BatchSize:          100,
		})
	}

	// R5.3a — stale lock reaper. Single goroutine, lifetime tied to
	// workerCtx so SIGTERM stops it alongside the other workers.
	// Releases issues.locked_by/locked_at where locked_at is older than
	// StaleLockThreshold AND the holding MCP session has been silent for
	// StaleLockAgentIdle (sk-243 — the timer is aimed at agents that fell
	// off, not at agents that are busy elsewhere in the tree); emits
	// issue.unclaimed events with reason="auto-released-stale-lock" so the
	// previous lock-holder gets a notification (FanOut treats
	// actorUserID=uuid.Nil as "no actor" and skips self-action exclusion).
	// See plan §D.
	reaper := lockreap.New(pool, lockreap.Config{
		Interval:  cfg.StaleLockInterval,
		Threshold: cfg.StaleLockThreshold,
		AgentIdle: cfg.StaleLockAgentIdle,
		BatchSize: 200,
	})
	go reaper.Run(workerCtx)

	// sk-62.2 — reminder scheduler. Single goroutine, lifetime tied to
	// workerCtx (SIGTERM stops it alongside the reaper). Each tick claims
	// due-and-un-notified reminders (FOR UPDATE SKIP LOCKED — multi-replica
	// safe), materialises a bell notification for each owner, and stamps
	// notified_at. The notification rows are published to
	// skein.user.{uid}.notification by the existing outbox dispatcher, so
	// no new live-push transport is needed.
	reminderScheduler := reminders.NewScheduler(reminders.NewStore(pool), reminders.Config{
		Interval:  cfg.ReminderInterval,
		BatchSize: cfg.ReminderBatchSize,
	})
	go reminderScheduler.Run(workerCtx)

	// sk-303.14 — the nudge (repeat-reminder) runner. One goroutine,
	// lifetime tied to workerCtx.
	//
	// Started UNCONDITIONALLY and BEFORE the NATS branch: reminders live as
	// rows in the database, and the live feed and push are different
	// transports. A runner tied to bus availability would stay silent on
	// an empty NATS_URL or a failed connect, and the whole repeat mechanic
	// would quietly be absent from such a deployment — exactly the breakage
	// an earlier change pulled the push path out from under this branch to fix.
	//
	// Completion is waited on a separate channel: the tick runs
	// transactions (consuming a window together with writing a
	// notification), and shutdown must let them finish rather than drop
	// the connection with an unclosed transaction.
	nudgeDone := make(chan struct{})
	nudgeScheduler := nudges.NewScheduler(nudges.NewStore(pool), nudges.Config{
		Interval:       cfg.NudgeInterval,
		BatchSize:      cfg.NudgeBatchSize,
		BatchesPerTick: cfg.NudgeBatchesPerTick,
	})
	go func() {
		defer close(nudgeDone)
		nudgeScheduler.Run(workerCtx)
	}()

	// sk-142.1 — monitor-credential cleanup worker. Single goroutine,
	// lifetime tied to workerCtx (SIGTERM stops it alongside the other
	// workers). Each sweep hard-deletes monitor_credentials rows that can
	// never authenticate again (past absolute deadline) plus revoked rows
	// older than the retention window, so the table doesn't accumulate dead
	// short-lived skm_ tokens.
	go monitorcreds.RunCleanupWorker(workerCtx, monitorcreds.New(pool, cfg.MonitorTokenIdleTTL), monitorcreds.CleanupConfig{
		Interval:         cfg.MonitorCredentialCleanupInterval,
		RevokedRetention: cfg.MonitorCredentialRevokedRetention,
	})

	// R3.2a — outbox dispatcher. Single goroutine, sibling to the
	// embedding worker. Lifetime is tied to workerCtx so SIGTERM cancels
	// both in lockstep before HTTP server shutdown begins.
	//
	// nc + natsClosed are connected upstream (before themesPublisher).
	// nil nc here means "NATS_URL unset or connect failed" — we skip
	// the dispatcher entirely; events continue to be written to the
	// audit table for the next replay window.
	// sk-303.13 / T010 — the PUSH runner is started here, unconditionally and
	// BEFORE the NATS branch below, because that is the whole point of the
	// split: web push and the live feed are different transports serving
	// different promises, and until this wave an empty NATS_URL (or a failed
	// connect) silently disabled every push in the deployment.
	//
	// It is started even when VAPID is not configured. A deployment with no
	// keys still has rows queued by the write path, and the runner is what
	// finalises them as configuration_no_send — "a disabled sender does
	// not leave a forever-pending".
	pushRunnerDone := make(chan struct{})
	pushRunner := outbox.NewPushRunner(pool, pushSender, pushStore, cfg.AppBaseURL, outbox.PushOptions{})
	go func() {
		defer close(pushRunnerDone)
		if err := pushRunner.Run(workerCtx); err != nil {
			slog.Error("push runner exited with error", "error", err.Error())
		}
	}()
	// sk-303.13 / T013 — the retention sweep for invisible push-only rows.
	// It shares the workers' context (SIGTERM stops it with the rest) and
	// nothing else: it is housekeeping, so it must never be able to hold up
	// delivery or shutdown.
	cleanupDone := make(chan struct{})
	cleanupRunner := outbox.NewCleanupRunner(pool, outbox.CleanupOptions{})
	go func() {
		defer close(cleanupDone)
		if err := cleanupRunner.Run(workerCtx); err != nil {
			slog.Error("push cleanup runner exited with error", "error", err.Error())
		}
	}()

	// sk-622.21 (T019, T025) — the notification retention sweep and the
	// assignee re-resolution, started through
	// the one wiring function the wiring test calls too (decision R16). It
	// shares workerCtx and is awaited at shutdown: a batch is a transaction
	// that should finish rather than be cut.
	notificationWorkersDone, err := startNotificationWorkers(workerCtx, pool, cfg)
	if err != nil {
		slog.Error("starting notification workers failed", "error", err.Error())
		os.Exit(1)
	}

	// sk-674.18 T022 — outgoing mail: the worker (only with
	// SKEIN_EMAIL_ENABLED=true) and the cleanup, through the one wiring
	// function the wiring test calls too. Awaited at shutdown: an attempt
	// in flight records its outcome under the lease it holds.
	emailWorkersDone, err := startEmailWorkers(workerCtx, pool, cfg)
	if err != nil {
		slog.Error("starting email workers failed", "error", err.Error())
		os.Exit(1)
	}

	// The header-counts gate's batch
	// computations are detached from their requests; they end with
	// workerCtx and are awaited at shutdown like the workers.
	countsGateDone := meHandler.BindCountsLifetime(workerCtx)

	dispatcherDone := make(chan struct{})
	if nc != nil {
		// sk-622.21 T027: issue events carry the phase-2 neutral row,
		// one statement per tick (K16).
		dispatcher := outbox.New(pool, nc, outbox.Options{Rows: issues.NeutralRowSource{}})
		go func() {
			defer close(dispatcherDone)
			if err := dispatcher.Run(workerCtx); err != nil {
				slog.Error("outbox dispatcher exited with error", "error", err.Error())
			}
		}()
	} else if cfg.NATSURL == "" {
		// No dispatcher to drain, but still close the channel so the
		// (dispatcherDone) read above never blocks accidentally if a
		// future change re-enables it conditionally.
		close(dispatcherDone)
		slog.Warn("outbox dispatcher disabled: NATS_URL not set")
	} else {
		// connectNATS returned an error; same closure as above.
		close(dispatcherDone)
	}

	// sk-353 — the shutdown teardown fan-out (see the package-level
	// teardownDeps/runTeardown doc for the full worst-case-math story).
	// teardownOnce, not a bare defer: the srv.Shutdown-error branch below
	// also needs to run this exact teardown, and must be safe to call
	// twice (once explicitly there, once via this defer).
	teardown := teardownDeps{
		stopWorker:     stopWorker,
		pushRunnerDone: pushRunnerDone,
		cleanupDone:    cleanupDone,
		nudgeDone:      nudgeDone,
		nc:             nc,
		dispatcherDone: dispatcherDone,
		natsClosed:     natsClosed,

		notificationWorkersDone: notificationWorkersDone,
		emailWorkersDone:        emailWorkersDone,
		countsGateDone:          countsGateDone,
	}
	var teardownOnce sync.Once
	runTeardownOnce := func() { teardownOnce.Do(func() { runTeardown(teardown) }) }
	// LIFO: this defer runs BEFORE the bare `defer stopWorker()` above and
	// BEFORE `defer pool.Close()`, and AFTER srv.Shutdown (invoked inline
	// below, not via defer) — same position the two original defers held.
	defer runTeardownOnce()

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}
	// sk-303.16 — cancel every open MCP standalone GET (SSE) stream's request
	// context when Shutdown starts. RegisterOnShutdown funcs run in their own
	// goroutine at the top of Shutdown(), concurrently with its own
	// closeIdleConns polling loop — without this, an open GET stream blocks
	// Shutdown until shutdownCtx's deadline every time (stdlib does not
	// cancel in-flight request contexts on its own; see mcppkg.Setup's doc
	// comment for the verified mechanism).
	srv.RegisterOnShutdown(drainMCPStreams)

	// sk-674.18 T042 — the operator's API, its own server on its own port
	// (nil when SKEIN_OPERATOR_API_ENABLED=false).
	opSrv := operatorServer(cfg, operator.NewAccounts(pool, usersStore))

	// Run the servers in goroutines so the main goroutine can wait on
	// signals; the first failure of either stops the process.
	serverErr := make(chan error, 2)
	go func() {
		slog.Info("HTTP server listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()
	if opSrv != nil {
		go func() {
			slog.Info("operator API listening", "addr", opSrv.Addr)
			if err := opSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serverErr <- fmt.Errorf("operator API: %w", err)
			}
		}()
	}

	// Block until SIGTERM/SIGINT or the server fails on its own.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received")
	case err := <-serverErr:
		if err != nil {
			slog.Error("HTTP server error", "error", err.Error())
			os.Exit(1)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	// The operator's server stops with the main one, in the same window.
	opStopped := make(chan struct{})
	go func() {
		defer close(opStopped)
		if opSrv == nil {
			return
		}
		if err := opSrv.Shutdown(shutdownCtx); err != nil {
			slog.Error("operator API shutdown error", "error", err.Error())
		}
	}()
	defer func() { <-opStopped }()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP server shutdown error", "error", err.Error())
		// sk-353 — os.Exit bypasses every deferred function, so this used
		// to skip NATS drain and the runner awaits ENTIRELY on a
		// srv.Shutdown error: not a degraded teardown, an ABSENT one. That
		// was defensible back when the teardown chain's own worst case was
		// ≤40s — piling that onto an already-blown 25s HTTP-shutdown
		// budget would only raise the odds of colliding with the
		// kubelet's SIGKILL, so skipping it looked like the safer choice.
		// It no longer costs that: runTeardown is now a single ≤10s
		// fan-out (see its package-level doc comment), a bounded price
		// worth paying to still flush in-flight NATS publishes and let
		// the runners exit their current tick cleanly instead of being
		// cut off mid-transaction. Explicit call, not just relying on the
		// defer below: os.Exit runs right after this block, so the
		// deferred runTeardownOnce() would never fire on its own —
		// teardownOnce makes calling it here (and, redundantly, via the
		// defer on the normal path) safe either way.
		//
		// pool.Close() is deliberately NOT given the same treatment: an
		// abrupt Postgres disconnect is a normal, well-handled condition
		// server-side (the backend notices the closed socket and cleans
		// up), unlike a dropped NATS publish (silently lost audit/live-feed
		// event) or a runner killed mid-transaction (rolled back, but
		// SIGKILL mid-syscall is uglier than a clean context-cancel exit).
		runTeardownOnce()
		os.Exit(1)
	}

	slog.Info("skein-api stopped")
}

// mcpRegisterTools is the single seam where MCP tools are wired into
// the server. R2.1b ships zero tools; R2.1c lights up `skein_whoami` and
// `skein_get_instructions`. Keeping it as a function pointer in main.go
// lets us add tools without touching the server-construction code.
//
// Implemented in cmd/api/mcp_tools.go.

// connectNATS dials the NATS server with reconnect-forever semantics and
// returns the connection. Returns (nil, nil) when NATS_URL is empty — that
// case is treated as "outbox disabled" (an explicit dev-mode degradation).
//
// sk-622.21 R21: a server that is unreachable AT STARTUP no longer yields
// nil. RetryOnFailedConnect(true) makes nats.Connect return the connection
// in the RECONNECTING state (no error) and keep dialling in the background
// with the same MaxReconnects/ReconnectWait as a dropped connection;
// publishes made meanwhile are buffered up to ReconnectBufSize, and the
// ReconnectHandler fires on the first successful asynchronous connect.
// Before this, one failed dial at pod start disabled the outbox dispatcher
// and the personal signal publisher for the whole life of the replica.
// An error is now returned only for what retrying cannot fix (a malformed
// URL, a bad option), so the operator still sees misconfiguration in logs.
//
// closed is closed by the nats.ClosedHandler when the connection has
// finished its async drain (or was Close()d). The caller waits on it
// during shutdown to guarantee in-flight publishes are flushed before
// the process exits — nats.Conn.Drain() itself returns immediately and
// runs the actual drain in an internal goroutine.
//
// Reconnect strategy:
//   - MaxReconnects = -1 → retry forever; the dispatcher's own backoff
//     handles the publish-side errors during NATS downtime.
//   - ReconnectWait = 2s → matches the dispatcher's max backoff order
//     of magnitude so a NATS rolling restart converges quickly.
//   - Name = "skein-api-outbox" → shows up in NATS monitoring (8222/connz)
//     so operators can attribute connections to this service.
func connectNATS(cfg *config.Config, closed chan<- struct{}) (*nats.Conn, error) {
	if cfg.NATSURL == "" {
		return nil, nil
	}
	nc, err := nats.Connect(cfg.NATSURL,
		nats.Name("skein-api-outbox"),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
		nats.ClosedHandler(func(_ *nats.Conn) {
			// Fires once after Drain (or Close) finishes the async
			// teardown. Closing the channel unblocks shutdown so the
			// process can exit cleanly.
			close(closed)
		}),
		nats.DisconnectErrHandler(func(_ *nats.Conn, e error) {
			if e != nil {
				slog.Warn("nats disconnected", "error", e.Error())
			}
		}),
		nats.ReconnectHandler(func(c *nats.Conn) {
			slog.Info("nats reconnected", "url", c.ConnectedUrl())
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("nats connect %q: %w", cfg.NATSURL, err)
	}
	if nc.IsConnected() {
		slog.Info("nats connected", "url", nc.ConnectedUrl())
	} else {
		// RetryOnFailedConnect: the first dial failed and the client keeps
		// dialling in the background; ConnectedUrl() is empty until then.
		// "nats reconnected" (ReconnectHandler) marks the moment it lands.
		slog.Warn("nats unreachable at startup; connecting in background",
			"natsUrl", cfg.NATSURL)
	}
	return nc, nil
}

// setupLogger installs a slog default logger using DualHandler:
// logfmt for < Error and JSON for >= Error. SERVICE_NAME is attached to
// every record.
func setupLogger(cfg *config.Config) {
	level := logging.ParseLevel(cfg.LogLevel)
	if cfg.Environment == "development" {
		// In development always show debug-level logs unless explicitly raised.
		if level > slog.LevelDebug {
			level = slog.LevelDebug
		}
	}
	// Every log line written while serving an MCP call carries the caller's
	// Mcp-Session-Id. api_key identifies the USER, and one user routinely
	// runs a lead agent and its subagents at once — without the session id
	// no aggregation over the logs can tell them apart. Wiring it here
	// rather than at each call site is deliberate: there are ~75 success
	// lines, and a field present on only some of them is unusable for
	// exactly the aggregation it exists to serve.
	slog.SetDefault(newLogger(cfg, os.Stdout, level))
}

// logConfigFailure logs a refused configuration before the service logger
// exists: the same DualHandler format (JSON for the error record) and the
// same service / installation attributes, taken by config.EarlyLogConfig
// with Load's variables and defaults.
func logConfigFailure(w io.Writer, err error) {
	newLogger(config.EarlyLogConfig(), w, slog.LevelInfo).
		Error("failed to load config", "error", err.Error())
}

// newLogger builds the service logger writing to w. Every record carries
// SERVICE_NAME and the installation name (SKEIN_INSTANCE_NAME, attribute
// skein_instance — the same label the metrics carry, see instance_label.go).
func newLogger(cfg *config.Config, w io.Writer, level slog.Level) *slog.Logger {
	h := logging.NewDualHandler(w, level, func(ctx context.Context) []slog.Attr {
		if sid := auth.SessionIDFrom(ctx); sid != "" {
			return []slog.Attr{slog.String("sessionId", sid)}
		}
		return nil
	})
	return slog.New(h).With("service", cfg.ServiceName, instanceLabel, cfg.InstanceName)
}

// buildSearchStack constructs the search.Service and HTTP handler. If
// OLLAMA_URL is empty we still build the service (FTS-only mode), but
// pass a nil embedder so the worker stays disabled.  This keeps the
// /search endpoint live even without external dependencies — useful in
// local dev and CI.
func buildSearchStack(pool *pgxpool.Pool, cfg *config.Config) (*search.Service, *handler.SearchHandler, error) {
	store := search.NewStore(pool)

	var embedder *search.Embedder
	if cfg.OllamaURL != "" {
		e, err := search.NewEmbedder(search.EmbedConfig{
			BaseURL:   cfg.OllamaURL,
			Model:     cfg.OllamaModel,
			APIKey:    cfg.OllamaAPIKey,
			Dim:       cfg.OllamaEmbeddingDim,
			CacheSize: cfg.SearchQueryEmbedCacheSize,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("building embedder: %w", err)
		}
		embedder = e
	}

	svc, err := search.NewService(store, embedder, cfg.SearchResultCacheTTL)
	if err != nil {
		return nil, nil, fmt.Errorf("building search service: %w", err)
	}
	return svc, handler.NewSearchHandler(svc), nil
}

// buildRouter assembles the chi router. Public probes (/healthz, /ready,
// /metrics) sit at the root with NO auth — they're consumed by Kubernetes
// and Prometheus, which can't carry the internal-secret header.
//
// Everything under /api/v1 goes through:
//  1. RequireInternalSecret — every request from the BFF must present the
//     shared secret. Direct ingress from outside the cluster has no way to
//     hold this secret, so this is the channel-auth boundary.
//  2. ParseUserHeaders — best-effort decode of X-Skein-User-* headers into
//     the request context. Auth bootstrap routes (/auth/login,
//     /auth/logout) intentionally do not require a user; user-bound routes
//     compose RequireUser on top.
//  3. RequireSessionEpoch — a request that carries a user is refused with
//     401 session_revoked when the session's epoch is not the user's
//     current one or the user is deactivated or deleted (K-16).
func buildRouter(
	pool *pgxpool.Pool,
	cfg *config.Config,
	authH *handler.AuthHandler,
	projectsH *handler.ProjectsHandler,
	componentsH *handler.ComponentsHandler,
	issuesH *handler.IssuesHandler,
	depsH *handler.DependenciesHandler,
	readyH *handler.ReadyHandler,
	searchH *handler.SearchHandler,
	notifH *handler.NotificationsHandler,
	bulkH *handler.BulkHandler,
	apiKeysH *handler.APIKeysHandler,
	usersH *handler.UsersHandler,
	issuesMeH *handler.IssuesMeHandler,
	issuesAllH *handler.IssuesAllHandler,
	activityH *handler.ActivityHandler,
	dashboardH *handler.DashboardHandler,
	usersStore *users.Store,
	issuesStore *issues.Store,
	depsStore *dependencies.Store,
	projectsStore *projects.Store,
	notifStore *notifications.Store,
	remindersStore *reminders.Store,
	searchSvc *search.Service,
	s3Client *storage.S3Client,
	attachmentsH *handler.AttachmentsHandler,
	meH *handler.MeHandler,
	themesH *handler.ThemesHandler,
	meThemeH *handler.MeThemeHandler,
	themesStore *themes.Store,
	themesPublisher themes.Publisher,
	inspectCache *themes.InspectCache,
	remindersH *handler.RemindersHandler,
	pushH *handler.PushHandler,
) (chi.Router, func()) {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	// sk-255.9 — http_requests_total / http_request_duration_seconds.
	// Registered BEFORE Recoverer on purpose: chi middleware wraps in
	// registration order, so this sits OUTSIDE the recovery boundary and a
	// panicking handler is observed with the status the client actually
	// received instead of unwinding past the observation unrecorded (that
	// is 500 in the common case; handler.HTTPMetrics documents the two
	// cases where it is not). Probe and scrape paths (/healthz, /ready,
	// /metrics) are filtered inside the middleware. The order is pinned by
	// TestBuildRouter_PanicIsObservedAsServerError — it panics on a probe
	// route through THIS router and reads the count back off /metrics.
	r.Use(handler.HTTPMetrics)
	r.Use(middleware.Recoverer)
	// One read of the caller's visible set per request (sk-674.18 T009):
	// every authz.Scope.Bind of the request shares it.
	r.Use(authz.RequestCache)

	// s3Ready is the optional health-probe seam used by /ready.
	// nil when S3 is disabled — Ready short-circuits the probe.
	var s3Ready handler.S3HealthChecker
	// s3Access is the MCP-side adapter (interface bundle); nil when
	// S3 is disabled, in which case the attachments MCP tools return
	// the stable attachments_unavailable error code.
	var s3Access attachments.S3Access
	if s3Client != nil {
		s3Ready = s3Client
		s3Access = &attachments.S3ClientAdapter{C: s3Client}
	}

	r.Get("/healthz", handler.Health())
	// s3Ready is nil when S3 is disabled (S3_URL empty); the
	// Ready handler short-circuits the S3 probe in that case.
	r.Get("/ready", handler.Ready(pool, s3Ready))
	r.Handle("/metrics", metricsHandler(cfg.InstanceName))

	// sk-304 — OAuth 2.0 Protected Resource Metadata (RFC 9728) for the MCP
	// door. Registered at the ROOT of the router, alongside /healthz and
	// BEFORE the /mcp group, because it must be reachable WITHOUT credentials:
	// a client reads it precisely because it does not yet know how to
	// authenticate. Two paths, one body — see handler.ProtectedResourceMetadata
	// for why the root document also names the /mcp identifier.
	//
	// The whole block is OPTIONAL (sk-304 review F1). With no authorization
	// server configured the routes are simply not registered and
	// oauthChallenge stays empty, so the /mcp 401 degrades to the plain 401 it
	// always was and the door keeps working with a static api key. A
	// deployment that never configures OAuth is not a broken deployment, and
	// making it one would have meant the next rollout of this image taking
	// down an API whose ConfigMap predates the variable.
	//
	// When it IS configured, cfg.Load() already ran Describe over these same
	// inputs and refused to start on a bad one, so an error here means the two
	// validations have drifted apart — a programming error at the composition
	// root, not an operator mistake. Panicking matches RequireInternalSecret's
	// treatment of the same class of bug and beats silently serving a wrong
	// document.
	var oauthChallenge string
	if oauthmeta.Enabled(cfg.OAuthAuthorizationServers) {
		oauthMeta, err := oauthmeta.Describe(cfg.AgentPublicURL, cfg.OAuthAuthorizationServers)
		if err != nil {
			panic("buildRouter: protected resource metadata: " + err.Error())
		}
		oauthMetaHandler := handler.ProtectedResourceMetadata(oauthMeta)
		r.Get(oauthmeta.WellKnownRootPath, oauthMetaHandler)
		if oauthMeta.WellKnownPath != oauthmeta.WellKnownRootPath {
			r.Get(oauthMeta.WellKnownPath, oauthMetaHandler)
		}
		oauthChallenge = oauthMeta.Challenge

		// sk-304 review F8 — the path-aware well-known URI is COMPUTED here
		// but HARD-CODED in skein-web (McpProxyMiddleware) and in
		// skein-frontend's nginx.conf, neither of which can see this config.
		// With no base path all three are the same literal and a unit test
		// pins it (oauthmeta.WellKnownMCPPath). With a base path they diverge:
		// the entrypoint forwards only the pathless form, so the URI this
		// process advertises in every challenge is answered by somebody else —
		// a 404 or, worse, an SPA's index.html. That is a silent discovery
		// failure of exactly the sk-304 shape, and nothing in this repo can
		// detect it, so say it out loud at startup instead of letting an
		// operator find it through a client that just gives up.
		if oauthMeta.WellKnownPath != oauthmeta.WellKnownMCPPath {
			slog.Warn("oauth metadata well-known URI is not the default path: "+
				"SKEIN_AGENT_PUBLIC_URL carries a base path, so the public entrypoint "+
				"(skein-web / the SPA nginx) must be configured to forward this exact "+
				"path — both forward only the pathless form out of the box",
				"wellKnownPath", oauthMeta.WellKnownPath,
				"defaultWellKnownPath", oauthmeta.WellKnownMCPPath,
				"resource", oauthMeta.ResourceID,
			)
		}
	}

	// R5.1 — OIDC provision endpoint. Mounted OUTSIDE the /api/v1 group
	// because that group applies ParseUserHeaders to every request, and
	// the provision call has no user yet (it's the call that establishes
	// one). Channel auth (RequireInternalSecret) is sufficient: only the
	// skein-web BFF holds the secret, and the BFF is the only legitimate
	// caller. The handler itself short-circuits with 410 oidc_disabled
	// when the provisioner is nil (defensive — wiring always sets it).
	r.With(handler.RequireInternalSecret(cfg.InternalSecret, "")).
		Post("/api/v1/auth/oidc/provision", authH.OIDCProvision)

	// sk-622.21 T006 — service-only batch project access check for
	// skein-web's SignalR hub (contract: projects/access_contract.go).
	// Mounted OUTSIDE the /api/v1 group for the same reason as the provision
	// route: that group applies ParseUserHeaders, and this call is made by
	// the BFF for many users at once, not for one logged-in user — whose
	// access is checked is named in the body. Channel auth
	// (RequireInternalSecret) is the whole authorization.
	r.With(handler.RequireInternalSecret(cfg.InternalSecret, "")).
		Post("/api/v1/hub/project-access", handler.NewHubAccessHandler(pool).ProjectAccess)

	// sk-674.18 T012 — installation settings for skein-web (contract:
	// accounts/contract.go#InstanceConfig). Service-only, outside the /api/v1
	// group like the two routes above: skein-web calls it at startup with no
	// user at all, and the channel secret is the whole authorization.
	r.With(handler.RequireInternalSecret(cfg.InternalSecret, "")).
		Get("/api/v1/internal/instance-config", handler.NewInstanceConfigHandler(cfg).Get)

	// sk-674.18 T038 — the public account routes of a visitor without a
	// session (contract: accounts/contract.go). Outside the /api/v1 group
	// like the routes above: there is no user, the channel secret is the
	// whole authorization, the client address comes in X-Skein-Client-Ip.
	turnstile := accounts.NewTurnstile(cfg.TurnstileSecret, accounts.TurnstileVerifyURL)
	accountsSvc := accounts.NewService(
		accounts.ServiceConfig{
			SignupEnabled: cfg.SignupEnabled, EmailEnabled: cfg.EmailEnabled,
			EmailDeliveryTTL: cfg.EmailDeliveryTTL,
			AuthMode:         cfg.AuthMode, BreakGlassAdmin: cfg.BreakGlassAdmin,
		},
		pool, usersStore, accounts.NewDirectory(pool), turnstile, accounts.NewRateLimiter())
	// sk-674.18 T040 — invitations: one spaces service decides the rights
	// and seats for the spaces routes and for the invitations; the
	// anonymous invitation routes of accounts answer through it.
	spacesSvc := spaces.NewService(pool, spacesOptions(cfg))
	invitationsSvc := invitations.NewService(pool, spacesSvc, turnstile, invitations.Options{
		SignupEnabled: cfg.SignupEnabled, EmailEnabled: cfg.EmailEnabled,
		InviteTTL: cfg.SpaceInviteTTL, InvitesPerDay: cfg.SpaceInvitesPerDay,
		DeliveryTTL: cfg.EmailDeliveryTTL,
	})
	accountsSvc.UseInvitations(invitationsSvc)
	accountsH := handler.NewAccountsHandler(accountsSvc)
	r.With(handler.RequireInternalSecret(cfg.InternalSecret, "")).
		Post("/api/v1/auth/signup", accountsH.Signup)
	r.With(handler.RequireInternalSecret(cfg.InternalSecret, "")).
		Get("/api/v1/auth/username-suggestions", accountsH.UsernameSuggestions)
	// T039 — password reset and email confirmation by link.
	r.With(handler.RequireInternalSecret(cfg.InternalSecret, "")).
		Post("/api/v1/auth/password-reset", accountsH.PasswordReset)
	r.With(handler.RequireInternalSecret(cfg.InternalSecret, "")).
		Post("/api/v1/auth/password-reset/{token}", accountsH.PasswordResetComplete)
	r.With(handler.RequireInternalSecret(cfg.InternalSecret, "")).
		Post("/api/v1/auth/confirm-email/{token}", accountsH.ConfirmEmail)
	// T040 — the invitation link without a session: its view and signup
	// by invitation.
	r.With(handler.RequireInternalSecret(cfg.InternalSecret, "")).
		Get("/api/v1/auth/invitations/{token}", accountsH.InvitationPreview)
	r.With(handler.RequireInternalSecret(cfg.InternalSecret, "")).
		Post("/api/v1/auth/invitations/{token}/signup", accountsH.InvitationSignup)

	// R2.1 — MCP Streamable HTTP endpoint. Sits at the root, NOT under
	// /api/v1/*: MCP clients are not browser-cookie-authenticated and
	// the BFF proxies the /mcp path on its own without going through
	// the cookie-protected /api/v1/* path. Auth here is X-Skein-Api-Key
	// (resolves to a user) gated on top of the channel-secret check.
	//
	// R2.2a — read tools depend on the issues, dependencies, projects
	// stores and the search service. We build mcpDeps once and capture
	// it in the registration closure so the existing Setup seam
	// (callback that takes (server, pool)) doesn't need to change.
	// projectsEmbedder is the same embedder instance used by the
	// HTTP /api/v1/projects handler for FTS+semantic rerank. We pull
	// it from searchSvc rather than parameterising buildRouter — the
	// search service is already in scope here. typed-nil dance: when
	// searchSvc.Embedder() returns nil we leave projectsEmbedder as
	// the true-nil interface (not a typed-nil-boxed-in-interface).
	var projectsEmbedder projects.QueryEmbedder
	var issuesEmbedder issues.QueryEmbedder
	if e := searchSvc.Embedder(); e != nil {
		projectsEmbedder = e
		issuesEmbedder = e
	}
	// sk-551.5 — the caller's interface-appearance settings. Built here from
	// the pool rather than threaded through the parameter list: it has no
	// collaborators to share with other handlers.
	meUIPrefsH := handler.NewMeUIPrefsHandler(pool)
	// sk-142.7 — built once here and reused for both the skein_get_monitor_script
	// MCP tool (mint path) and the /agent/v1 RequireMonitorToken door below.
	// sk-579 — the renewal window comes from cfg (config.Load), so mint and
	// validate-and-renew both slide by the configured window (INV-1).
	monitorStore := monitorcreds.New(pool, cfg.MonitorTokenIdleTTL)
	// Reuse the same store instances main() already built for REST —
	// stores are stateless over the pool; no observable behaviour change,
	// just one fewer NewStore allocation per process for projects /
	// notifications / reminders.
	mcpDepsBundle := mcpDeps{
		pool:             pool,
		issuesStore:      issuesStore,
		depsStore:        depsStore,
		projectsStore:    projectsStore,
		usersStore:       usersStore,
		notifStore:       notifStore,
		searchSvc:        searchSvc,
		projectsEmbedder: projectsEmbedder,
		issuesEmbedder:   issuesEmbedder,
		// R4.2 — attachments deps. s3Access is nil when S3_URL
		// unset; the tools return attachments_unavailable in that case.
		attachmentsStore:   attachmentsStoreWithQuotas(pool, cfg),
		s3Access:           s3Access,
		attachmentBucket:   cfg.S3Bucket,
		attachmentMaxBytes: cfg.AttachmentMaxSizeBytes,
		attachmentURLTTL:   time.Duration(cfg.AttachmentURLTTLSeconds) * time.Second,
		// sk-10.3 — agent-driven theme editing.
		themesStore:     themesStore,
		themesPublisher: themesPublisher,
		inspectCache:    inspectCache,
		// sk-62.2 — reminders MCP tools share a store with the REST
		// handler family (stateless over the pool).
		remindersStore: remindersStore,
		// sk-142.7 — agent task-monitor setup tool deps.
		monitorCredsStore:   monitorStore,
		agentPublicURL:      cfg.AgentPublicURL,
		monitorTokenIdleTTL: cfg.MonitorTokenIdleTTL,
		// sk-674.18 T024 — the space of a new project and the admin agent
		// adding to a space: the same service and defaults as REST.
		spacesSvc:                spaces.NewService(pool, spacesOptions(cfg)),
		projectDefaultVisibility: cfg.ProjectDefaultVisibility,
	}
	// drainMCPStreams (sk-303.16) is wired into srv.RegisterOnShutdown by the
	// caller (main()) — it cancels every open standalone GET (SSE) stream's
	// request context so srv.Shutdown drains them instead of blocking on
	// its own deadline. See mcppkg.Setup's doc comment for the mechanism.
	mcpHandler, drainMCPStreams := mcppkg.Setup(pool, cfg.MCPHeartbeatInterval, mcpRegisterToolsFn(mcpDepsBundle))
	r.Route("/mcp", func(r chi.Router) {
		// sk-304 review F5 — BOTH gates on this door answer 401 with the
		// RFC 9728 §5.1 challenge. The channel-secret gate is the FIRST thing
		// a request to /mcp meets, so a client that reaches skein-api with a
		// wrong or missing internal secret gets a bare 401 unless the
		// challenge is attached here too — and the MCP authorization spec
		// requires the header on a 401 from an MCP server unconditionally,
		// not "on the interesting 401s". Two doors, one behaviour. The other
		// three mounts of this middleware (/api/v1, /agent/v1 and the OIDC
		// provision route) pass an empty challenge: their 401s are not
		// protected-resource failures and must not advertise one.
		r.Use(handler.RequireInternalSecret(cfg.InternalSecret, oauthChallenge))
		// sk-304 — the api-key gate's 401 carries the RFC 9728 §5.1
		// WWW-Authenticate challenge pointing at the PATH-AWARE metadata
		// document. Per the MCP authorization spec the header is the primary
		// discovery route and the well-known probe only a fallback, so this
		// removes the client's guess between the root and path-aware
		// documents. The channel-secret gate above deliberately does NOT
		// carry it: it is shared with /api/v1 and /agent/v1, whose 401s are
		// not OAuth-protected-resource failures, and only the BFF ever
		// trips it.
		r.Use(auth.RequireAPIKey(pool, cfg.BcryptCost, oauthChallenge))
		r.Mount("/", mcpHandler)
	})

	// sk-142.4 — agent task-monitor data plane. SIBLING route group to /mcp,
	// NOT under /api/v1: monitor-token runners are not browser-cookie
	// callers, and the BFF passes /agent/v1 through on its own door (T3).
	// Two auth gates, composed like /mcp: the channel secret (only the BFF
	// holds it) then RequireMonitorToken, which accepts ONLY skm_ tokens
	// (an ska_ api key is the wrong shape here) and stashes the validated
	// scope claims in the request context. Read-only by route set: just the
	// GET poll endpoint in T4 (the monitor-script GET lands in T6).
	// monitorStore is built once above (mcpDepsBundle) and shared with the
	// skein_get_monitor_script MCP tool — the mint side of this same door.
	agentPollHandler := handler.NewAgentPollHandler(pool, cfg.MonitorPollSafetyLag)
	// sk-142.6 — the Python poll runner served on the same /agent/v1 door.
	// sk-579 — every runner timing comes from the single derivation that
	// config.Load checked the stall threshold against (INV-6).
	runnerTimings := config.RunnerTimings(cfg.MonitorRunnerPollInterval)
	agentScriptHandler := handler.NewAgentScriptHandler(
		runnerTimings.PollIntervalSec,
		runnerTimings.BackoffCeiling,
		runnerTimings.RequestLimit,
		cfg.MonitorStallThreshold,
	)
	r.Route("/agent/v1", func(r chi.Router) {
		r.Use(handler.RequireInternalSecret(cfg.InternalSecret, ""))
		r.Use(auth.RequireMonitorToken(monitorStore, "notifications:poll"))
		r.Get("/notifications/poll", agentPollHandler.Poll)
		r.Get("/monitor-script", agentScriptHandler.Script)
	})

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(handler.RequireInternalSecret(cfg.InternalSecret, ""))
		r.Use(handler.ParseUserHeaders)
		// sk-674.18 K-16: a session whose epoch was raised (password reset,
		// deactivation, account deletion) is refused on its next request.
		r.Use(handler.RequireSessionEpoch(pool))

		// Global cross-entity search (R1.5). Sibling to /projects so
		// callers can search across all visible projects in one call.
		// RequireUser is mandatory — every result is filtered by
		// project_members membership of the caller.
		r.With(handler.RequireUser).Get("/search", searchH.Search)

		// Lightweight ID-prefix lookup (sk-11). Sibling to /search,
		// shares the SearchHandler. Powers the typeahead direct-jump
		// UX in the tasks-column search box on the new shell: skips
		// the full hybrid pipeline and returns up to 10 issue/project
		// rows that match the typed id prefix.
		r.With(handler.RequireUser).Get("/search/lookup-id", searchH.LookupID)

		// Batch exact-id resolve (sk-47.1). Sibling to /search/lookup-id,
		// shares the SearchHandler. POST {"ids":[...]} — resolves up to
		// 100 EXACT issue ids in one visibility-filtered query, returning
		// the found+visible rows in the same Item shape as /search. Backs
		// the "paste a blob of text → extract task codes → render the
		// tasks" frontend feature.
		r.With(handler.RequireUser).Post("/search/lookup-id-batch", searchH.LookupIDBatch)

		// sk-17 — dashboard surface (scoring matrix + live feed). Two
		// endpoints sit at the /api/v1 top-level (not under /me)
		// because the URL is consumed by a dedicated /activity SPA
		// route and the convention across the codebase is that
		// org-wide cross-project surfaces (/search, /issues, /search/
		// lookup-id) live at the top.
		r.With(handler.RequireUser).Get("/dashboard/scoring", dashboardH.Scoring)
		r.With(handler.RequireUser).Get("/dashboard/feed", dashboardH.Feed)
		// sk-29 — race chart: per-user cumulative leaderboard over a
		// rolling window. Cross-project across the caller's horizon;
		// supports metric=closed|commits|created|comments and
		// window=7d|30d|90d|all with topN=1..20 (default 8).
		r.With(handler.RequireUser).Get("/dashboard/race", dashboardH.Race)
		// sk-17 — "now working" snapshot. status='in_progress' OR
		// locked_by IS NOT NULL, visibility-filtered. Pairs with the
		// existing SignalR `event` channel — the frontend subscribes
		// to issue.claimed / unclaimed / status_changed / accepted /
		// closed and re-fetches this endpoint when any of those
		// envelopes land.
		r.With(handler.RequireUser).Get("/dashboard/in-progress", dashboardH.InProgress)
		// sk-22 — per-user MCP-session counts (live 15m / 24h / 7d /
		// 30d) for the /activity dashboard's `agents` zone. Polled
		// 30s on the frontend; no SignalR push (agents.Upsert is
		// silent so there's no event to subscribe to).
		r.With(handler.RequireUser).Get("/dashboard/agents", dashboardH.Agents)
		// sk-183.1 - per-project activity leaderboard. Cross-project
		// (war-room shaped, like its siblings above), but redaction here
		// is project-identity-only: a non-member row keeps every count
		// and every username, only projectId/name/prefix are stripped.
		// See DashboardHandler.Projects doc comment.
		r.With(handler.RequireUser).Get("/dashboard/projects", dashboardH.Projects)

		// sk-303.13 (C1) — catalogue of subscription event types. The
		// registry is the same for everyone and holds no private data, so
		// it lives next to the feed, not under /me.
		r.With(handler.RequireUser).Get("/notification-event-types", notifH.GetSubscriptionEventTypes)

		// R1.6 — notifications inbox (top-level endpoints, not nested
		// under /projects). Caller-scoped: every row is filtered by
		// user_id = caller, so no project visibility check is needed.
		r.Route("/notifications", func(r chi.Router) {
			r.Use(handler.RequireUser)
			r.Get("/", notifH.ListNotifications)
			r.Get("/count", notifH.CountNotifications)
			r.Post("/mark-all-read", notifH.MarkAllRead)
			r.Post("/mark-read-by-entity", notifH.MarkReadByEntity)
			r.Route("/{id}", func(r chi.Router) {
				r.Post("/read", notifH.MarkRead)
				r.Post("/unread", notifH.MarkUnread)
				r.Post("/archive", notifH.Archive)
			})
		})

		// sk-62.2 — reminders. Top-level, caller-scoped: every row is
		// filtered by user_id = caller. GET returns the caller's list +
		// the ringing-task badge count; snooze / dismiss target a single
		// reminder by id (owner-scoped — another user's reminder reads as
		// 404). Creating a reminder is per-issue, nested under
		// /projects/{pid}/issues/{iid}/reminders below.
		r.Route("/reminders", func(r chi.Router) {
			r.Use(handler.RequireUser)
			r.Get("/", remindersH.List)
			r.Post("/{reminderID}/snooze", remindersH.Snooze)
			r.Delete("/{reminderID}", remindersH.Dismiss)
		})

		// Web Push — VAPID config discovery + caller-scoped subscription
		// CRUD. /config exposes only the PUBLIC key (200 always, empty when
		// push is unconfigured — the frontend feature-detects). The
		// subscriptions sub-tree is keyed on the browser-supplied endpoint
		// and scoped to the calling user.
		r.Route("/push", func(r chi.Router) {
			r.Use(handler.RequireUser)
			r.Get("/config", pushH.Config)
			r.Get("/subscriptions", pushH.List)
			r.Post("/subscriptions", pushH.Subscribe)
			r.Delete("/subscriptions", pushH.Unsubscribe)
			// Id-keyed delete for the device-management UI (remove ANY listed
			// device by id). Coexists with the endpoint-keyed delete above —
			// chi distinguishes /subscriptions from /subscriptions/{id}.
			r.Delete("/subscriptions/{id}", pushH.DeleteByID)
		})

		// Sc3 — cross-project list of every visible issue. Sibling to
		// /projects/{pid}/issues, but visibility-filtered to the
		// caller's project_members horizon (admins see all). Powers
		// the "All projects" pseudo-row in the new shell's Projects
		// column — clicking it surfaces a flat, server-paginated list
		// of all tasks the caller can see, with q / status / type /
		// origin / assignee / createdBy / acceptedAt filters. Same
		// IssueSummary wire shape as the project-scoped list, plus an
		// exact Total for the absolute counter.
		r.With(handler.RequireUser).Get("/issues", issuesAllH.List)
		// sk-36 — cross-project layered tree view. Sibling to /issues
		// (the list endpoint). Visibility-filtered to the caller's
		// project_members horizon at the store level. Same wire shape
		// as the project-scoped /projects/{pid}/issues/tree.
		r.With(handler.RequireUser).Get("/issues/tree", issuesAllH.Tree)

		// R1.6 — issue-level watchers. Sibling to /projects/.../issues
		// because the issue id alone is enough; the handler resolves
		// the project via issues.project_id and runs visibility check.
		r.Route("/issues/{issueID}", func(r chi.Router) {
			r.Use(handler.RequireUser)
			r.Post("/watch", notifH.WatchIssue)
			r.Delete("/watch", notifH.UnwatchIssue)
			r.Get("/watchers", notifH.ListIssueWatchers)

			// Phase 2 ui-redesign — POST /accept marks the issue as
			// acknowledged by the current assignee. Idempotent on
			// already-accepted; permission gate (only the assignee —
			// owner rights and role=admin do not count) lives in the store. Path is non-project-scoped so the
			// REST UI doesn't have to plumb projectID through every
			// "Accept" button click.
			r.Post("/accept", issuesH.Accept)

			// sk-34 — Delegated chain opt-out. POST /dismiss appends
			// the caller to issues.dismissed_by + runs the explicit
			// unwatch (subscription deleted, opt-out recorded when there
			// was one — couples bell-silence). POST /undismiss is the
			// inverse for dismissed_by only.
			// Visibility check runs identically to /accept.
			r.Post("/dismiss", issuesH.Dismiss)
			r.Post("/undismiss", issuesH.Undismiss)

			// R4.2 — attachment listing + upload-URL minting under the
			// same issue-scoped path. Confirm / download / delete live
			// under /api/v1/attachments/{id} below since they need
			// only the attachment uuid.
			r.Get("/attachments", attachmentsH.List)
			r.Post("/attachments/upload-url", attachmentsH.UploadURL)
		})

		// R4.2 — attachment lifecycle endpoints keyed by the
		// attachment's own uuid. Each handler resolves the parent
		// issue and runs visibility internally.
		r.Route("/attachments/{id}", func(r chi.Router) {
			r.Use(handler.RequireUser)
			r.Post("/confirm", attachmentsH.Confirm)
			r.Get("/download-url", attachmentsH.DownloadURL)
			r.Delete("/", attachmentsH.Delete)
		})

		// sk-674.18 T023 — spaces (contract: spaces/contract.go). Every
		// route needs a user; who may do what is spaces.Service's decision
		// (owner, member, the effective admin read from the database). The
		// two admin-only routes also sit behind RequireAdmin, whose refusal
		// (403 not_admin) is the contract's.
		spacesH := handler.NewSpacesHandler(spacesSvc)
		// T040 — invitations (spaces/contract.go): sending, the Inbox
		// answers and the link with a session.
		invitationsH := handler.NewInvitationsHandler(invitationsSvc)
		// R1.6 — per-user notification preferences. /me/* lives under
		// auth conceptually but the prefs are notification-domain.
		// R2.1 added /me/api-keys/* under the same group — same gate
		// (RequireUser), same scoping ("the calling user's own data").
		// sk-674.18 T041 — the account deletes itself (open registration
		// only; the operator's deletion of T042 uses the same store).
		accountDeletionH := handler.NewAccountDeletionHandler(usersStore, cfg.SignupEnabled)
		r.Route("/me", func(r chi.Router) {
			r.Use(handler.RequireUser)
			r.Delete("/", accountDeletionH.DeleteMe)
			r.Get("/deletion-preview", accountDeletionH.DeletionPreview)
			// sk-674.18 T039 — a new confirmation email to the caller.
			r.Post("/email/confirmation", accountsH.RequestConfirmationEmail)
			r.Get("/notification-prefs", notifH.GetPrefs)
			r.Put("/notification-prefs", notifH.PutPrefs)

			// sk-303.13 (C1) — unified prefs form: a row = a reason, two
			// independent channels. Replaces the two previous prefs
			// contracts (feed event types and push categories); their
			// endpoints come down with the code that still reads them.
			r.Get("/notification-reasons", notifH.GetReasonPrefs)
			r.Put("/notification-reasons", notifH.PutReasonPrefs)

			// sk-303.14 (C4) — review-mechanic prefs: a master switch, the
			// flag's personal default, and three schedule parameters.
			// A separate contract, not an extension of the reasons form:
			// reasons are "what to notify about", and here it is the
			// mechanic as a whole, its default on issue create, and the
			// repeat arithmetic. Mixing them would force the reasons form
			// to know about schedule bounds.
			r.Get("/review-prefs", notifH.GetReviewPrefs)
			r.Put("/review-prefs", notifH.PutReviewPrefs)

			// sk-565 (C-REST, D8) — «assign the issues my agent creates to
			// me». A contract of its own, not a field of review-prefs: the
			// review form is the acknowledgement mechanic, self-assignment on
			// MCP create is not part of it.
			r.Get("/issue-create-prefs", notifH.GetIssueCreatePrefs)
			r.Put("/issue-create-prefs", notifH.PutIssueCreatePrefs)

			// sk-62.3.2 — web-push category opt-outs (push-only; distinct
			// from the bell/notify event-type filter above).
			r.Get("/push-prefs", notifH.GetPushPrefs)
			r.Put("/push-prefs", notifH.PutPushPrefs)

			// sk-340 - quiet hours: the recipient's timezone and the window bounds. Two
			// contracts, not one: the timezone is written by TWO roads with different write
			// rights (detection from the browser yields to a manual edit),
			// the window is one. They silence only the PUSH: the feed row is created as
			// usual and waits for the person in the morning.
			r.Get("/timezone", notifH.GetTimezone)
			r.Put("/timezone", notifH.PutTimezone)
			r.Get("/quiet-hours", notifH.GetQuietHours)
			r.Put("/quiet-hours", notifH.PutQuietHours)

			r.Route("/api-keys", func(r chi.Router) {
				r.Get("/", apiKeysH.List)
				r.Post("/", apiKeysH.Create)
				r.Delete("/{id}", apiKeysH.Revoke)
			})

			// R3.1 — self-service password change. Available to every
			// authenticated user, NOT admin-gated.
			r.Post("/password", usersH.ChangeMyPassword)

			// sk-303.20 — tells the frontend whether the server-side Pwned
			// Passwords check is enabled, so the client's own hint lookup
			// (a browser-side fetch to the breach-check service) only ever
			// fires when the operator opted in server-side. Same
			// RequireUser gate as /password itself.
			r.Get("/password/policy", usersH.PasswordPolicy)

			// R3.2c — project memberships hint for the SignalR Hub. The
			// hub needs the caller's project_ids (plus a synthetic
			// "admin" row per project for org-admins) to compute event
			// filters at connect time.
			r.Get("/memberships", usersH.GetMyMemberships)

			// R3.3 — consolidated /me/watchers (issues + projects +
			// custom) and CRUD over custom_watchers. The custom-watcher
			// CREATE handler enforces the per-user cap (10) inside a
			// SERIALIZABLE transaction.
			r.Get("/watchers", notifH.GetMyWatchers)
			r.Get("/custom-watchers", notifH.ListMyCustomWatchers)
			r.Post("/custom-watchers", notifH.CreateCustomWatcher)
			r.Delete("/custom-watchers/{id}", notifH.DeleteCustomWatcher)

			// R3.3 — caller-scoped issue lists for the dashboard /
			// home views. Both walk every project the caller can see;
			// admin sees all projects, non-admin only their member set.
			r.Get("/assigned-by", issuesMeH.AssignedBy)
			r.Get("/assigned-to", issuesMeH.AssignedTo)
			// Phase 2 ui-redesign (B7) — cross-project Outbox surface
			// ("tasks I originated"). Same scope predicate as
			// /me/assigned-by (i.created_by = caller); coexists for the
			// distinct UI surface name. Supports the same status /
			// origin / acceptedAt filters as the project-scoped list.
			r.Get("/created-by", issuesMeH.CreatedBy)

			// R3.4 — global activity feed for the dashboard "recent
			// activity" panel. Caller-scoped: admin sees all, non-admin
			// limited to their visible projects.
			r.Get("/activity", activityH.ListMyActivity)

			// Phase 2 ui-redesign (B6) — header navigation counts.
			// Returns inbox / outbox / mentions / watching unread tallies
			// in one round-trip so the header refreshes against a single
			// endpoint instead of fanning out to four separate calls.
			r.Get("/notification-counts", meH.NotificationCounts)

			// sk-39 — Watching feed for the shell's left-sidebar
			// Watching tab. Per-issue projection (IssueSummary + reasons
			// + unreadEventsCount + recentEvents) sorted by most-recent
			// activity. /me/watchers stays as the consolidated R3.3
			// surface for the standalone /watching page; this route
			// powers the new shell column specifically.
			r.Get("/watching", meH.GetWatching)
			// sk-551.10 — bulk unwatch: an explicit selection or a
			// state filter, only the caller's own subscriptions.
			r.Post("/watching/unwatch", meH.BulkUnwatch)
			// sk-622.21 T023 (US-7) — "read the card": every unread
			// notification of the caller carded to {issueId}.
			r.Post("/watching/{issueId}/read", meH.MarkCardRead)

			// sk-10.1 — per-user active theme preference (mode +
			// builtinTheme + customThemeId). GET returns the three
			// users-row columns; PUT replaces all three atomically.
			r.Get("/theme", meThemeH.GetTheme)
			r.Put("/theme", meThemeH.SetTheme)
			// sk-10.3 — inspector selection capture. POST-only;
			// returns 204 No Content; appends to the in-memory inspect
			// cache shared with the MCP layer (skein_theme(action="describe")
			// re-emits the list under selectedTargets; sk-10.6 made the
			// cache multi-entry so the agent sees every click, not just
			// the last one).
			r.Post("/theme/inspect", meThemeH.Inspect)

			// sk-551.5 — personal interface-appearance settings (today: the
			// task views that open a selected issue as a centred pop-up
			// window). PUT is partial: an absent field keeps its value, so
			// the next settings (density, left-panel mode) can join the
			// same body without resetting this one.
			r.Get("/ui-prefs", meUIPrefsH.GetUIPrefs)
			r.Put("/ui-prefs", meUIPrefsH.PutUIPrefs)
			// sk-674.18 T023 — the caller's spaces (spaces/contract.go).
			r.Get("/spaces", spacesH.MySpaces)
			// T040 — the pending invitations to the caller and his answers.
			r.Get("/invitations", invitationsH.Mine)
			r.Post("/invitations/{invId}/accept", invitationsH.AcceptMine)
			r.Post("/invitations/{invId}/decline", invitationsH.DeclineMine)
		})

		// sk-10.1 — user-customizable themes. Every authenticated user
		// can list / create / read / update / delete every theme. No
		// per-theme permissions in MVP — see ThemesHandler doc-comment.
		r.Route("/themes", func(r chi.Router) {
			r.Use(handler.RequireUser)
			r.Get("/", themesH.List)
			r.Post("/", themesH.Create)
			r.Get("/{id}", themesH.Get)
			r.Patch("/{id}", themesH.Patch)
			r.Delete("/{id}", themesH.Delete)
		})

		r.Route("/spaces", func(r chi.Router) {
			r.Use(handler.RequireUser)
			r.Post("/", spacesH.Create)
			r.Route("/{id}", func(r chi.Router) {
				r.Get("/", spacesH.Get)
				r.Patch("/", spacesH.Rename)
				r.Delete("/", spacesH.Delete)
				r.Get("/deletion-preview", spacesH.DeletionPreview)
				r.Post("/leave", spacesH.Leave)
				r.Get("/members/{userId}/removal-preview", spacesH.RemovalPreview)
				r.Delete("/members/{userId}", spacesH.RemoveMember)
				r.With(handler.RequireAdmin).Post("/members", spacesH.AddMember)
				r.With(handler.RequireAdmin).Post("/owner", spacesH.ChangeOwner)
				// T040 — invitations (the owner and the admin).
				r.Post("/invitations", invitationsH.Create)
				r.Post("/invitations/{invId}/resend", invitationsH.Resend)
				r.Delete("/invitations/{invId}", invitationsH.Revoke)
			})
		})

		// T040 — the link of the invitation email, with a session.
		r.Route("/invitations/{token}", func(r chi.Router) {
			r.Use(handler.RequireUser)
			r.Get("/", invitationsH.View)
			r.Post("/accept", invitationsH.AcceptByToken)
			r.Post("/decline", invitationsH.DeclineByToken)
		})

		// R4.3 — public user profile lookup by username. RequireUser
		// only — any authenticated user can resolve mentions to a
		// profile page. Email + deactivation status are admin-gated
		// inside the handler (non-admin viewers get the trimmed shape).
		// Sits OUTSIDE the admin-gated /users block below; chi routes
		// the more specific path (/users/profile/{username}) before
		// /users/{id}. Mounted with a separate Route so we don't have
		// to weave non-admin endpoints through the admin middleware.
		r.Group(func(r chi.Router) {
			r.Use(handler.RequireUser)
			r.Get("/users/profile/{username}", usersH.GetUserProfile)
			// sk-79 — non-admin user directory (lean id/username/name list)
			// backing the assignee picker on the cross-project "All projects"
			// Tasks surface. Registered in this RequireUser-only group, NOT
			// the admin-gated /users block below. The literal "/users/directory"
			// is more specific than "/users/{id}", but {id} lives under a
			// SEPARATE r.Route("/users", …) subtree — so to be safe against
			// chi's mux ordering this exact path is declared here, ahead of
			// that subtree's registration.
			r.Get("/users/directory", usersH.ListDirectory)
		})

		// R3.1 — admin user management. RequireUser provides the auth
		// floor; RequireAdmin adds the admin gate (403 not_admin for
		// non-admin callers). Both middlewares run before any handler.
		r.Route("/users", func(r chi.Router) {
			r.Use(handler.RequireUser)
			r.Use(handler.RequireAdmin)
			r.Post("/", usersH.Create)
			r.Get("/", usersH.List)
			r.Route("/{id}", func(r chi.Router) {
				r.Get("/", usersH.Get)
				r.Patch("/", usersH.Update)
				r.Post("/deactivate", usersH.Deactivate)
				// sk-674.18 T037 — the user's spaces (the admin's checkboxes;
				// the owner rows name the deactivation dialog's "Owns: ...").
				r.Get("/spaces", spacesH.UserSpaces)
				r.Put("/spaces", spacesH.PutUserSpaces)
				r.Post("/spaces/preview", spacesH.PreviewUserSpaces)
				r.Post("/reactivate", usersH.Reactivate)
			})
		})

		r.Route("/auth", func(r chi.Router) {
			// Login and logout do not require a user header — login is
			// the path that *establishes* one, and logout is a no-op
			// placeholder until R5 token revocation lands.
			r.Post("/login", authH.Login)
			r.Post("/logout", authH.Logout)
			// /me operates on behalf of an already-authenticated user
			// (cookie issued by skein-web), so it gates on RequireUser.
			r.With(handler.RequireUser).Get("/me", authH.Me)
		})

		// /projects/* — every endpoint requires an authenticated user.
		// Wiring follows the spec from R1.2: list/create at the root,
		// resolve as a sibling, and per-project nested resources for
		// repos, aliases, members, and components.
		r.Route("/projects", func(r chi.Router) {
			r.Use(handler.RequireUser)

			r.Get("/", projectsH.List)
			r.Post("/", projectsH.Create)
			r.Get("/resolve", projectsH.Resolve)
			// sk-674.18 T025 — prefix suggestions and check (rate-limited per caller).
			r.Get("/prefix-suggestions", projectsH.PrefixSuggestions)
			r.Get("/prefix-check", projectsH.PrefixCheck)

			r.Route("/{projectID}", func(r chi.Router) {
				r.Get("/", projectsH.Get)
				r.Patch("/", projectsH.Patch)
				// DELETE on /projects/{id} is intentionally NOT supported —
				// projects are soft-archived. Clients see 405 + Allow header.
				r.Method(http.MethodDelete, "/", handler.Method405("GET, PATCH, POST"))
				r.Post("/archive", projectsH.Archive)
				r.Post("/unarchive", projectsH.Unarchive)

				r.Route("/repos", func(r chi.Router) {
					r.Post("/", projectsH.AddRepo)
					r.Patch("/{repoID}", projectsH.UpdateRepo)
					r.Delete("/{repoID}", projectsH.RemoveRepo)
				})

				r.Route("/aliases", func(r chi.Router) {
					r.Get("/", projectsH.ListAliases)
					r.Post("/", projectsH.AddAlias)
					r.Delete("/{alias}", projectsH.RemoveAlias)
				})

				r.Route("/members", func(r chi.Router) {
					r.Get("/", projectsH.ListMembers)
					r.Put("/{userID}", projectsH.UpsertMember)
					r.Delete("/{userID}", projectsH.RemoveMember)
				})

				r.Route("/components", func(r chi.Router) {
					r.Get("/", componentsH.List)
					r.Post("/", componentsH.Create)
					r.Route("/{componentID}", func(r chi.Router) {
						r.Get("/", componentsH.Get)
						r.Patch("/", componentsH.Patch)
						r.Delete("/", componentsH.Delete) // soft-archive alias
						r.Post("/archive", componentsH.Archive)
						r.Post("/unarchive", componentsH.Unarchive)
					})
				})

				// R1.6 — project-level watchers + bulk operations. Both
				// require project visibility (the route group already
				// enforces that via RequireUser; the handlers themselves
				// run EnsureProjectVisible).
				r.Post("/watch", notifH.WatchProject)
				r.Delete("/watch", notifH.UnwatchProject)
				r.Get("/watchers", notifH.ListProjectWatchers)
				// R3.3 — update event_types whitelist on existing project
				// watcher row. Requires the watcher to already exist
				// (POST /watch first); 404 watcher_not_found otherwise.
				r.Patch("/watch", notifH.PatchProjectWatcher)

				// Issues are nested under their project for permission
				// scoping and so the URL contains both the project UUID
				// (for ACL) and the issue's hierarchical ID (for the row).
				// Issue IDs may contain '.' (demo-42.1.3) but never '/' —
				// chi's URL params handle dots fine.
				r.Route("/issues", func(r chi.Router) {
					r.Get("/", issuesH.List)
					r.Post("/", issuesH.Create)
					// /issues/check-duplicates — sibling to the list endpoint
					// (no issueID required). Pure read-only trigram search.
					r.Post("/check-duplicates", issuesH.CheckDuplicates)
					// sk-36 — layered tree view (project-scoped). Sibling
					// to /list; types[]=parent-child by default, with all
					// six non-hierarchical types available as togglable
					// lenses. Returns a flat pre-order list — frontend
					// renders the connectors per edge type.
					r.Get("/tree", issuesH.Tree)
					// R1.6 — bulk close/reopen/pin/unpin/archive across
					// many issue ids in one request. 207 on partial success.
					r.Post("/bulk", bulkH.Bulk)
					r.Route("/{issueID}", func(r chi.Router) {
						r.Get("/", issuesH.Get)
						r.Patch("/", issuesH.Patch)
						// DELETE on /issues/{id} is intentionally NOT supported —
						// vision specifies no hard-delete in normal flow.
						r.Method(http.MethodDelete, "/", handler.Method405("GET, PATCH, POST, PUT"))
						r.Post("/transitions", issuesH.UpdateStatus)
						// sk-674.18 T034 — "release the lock" (US-16, K-28).
						r.Post("/unlock", issuesH.Unlock)
						// sk-43 — atomic re-parent. Issue-scoped mutation
						// (sibling to /transitions): replaces the issue's
						// single parent-child edge under a new parent, with
						// optimistic-concurrency + lock guards. Lives on the
						// dependencies handler because it operates on the
						// parent-child dep edge via depsStore.
						r.Post("/reparent", depsH.Reparent)
						r.Post("/pin", issuesH.Pin)
						r.Post("/unpin", issuesH.Unpin)
						// sk-303.14 (contract C1) — the reviewed mark.
						// Its own route, not a patch field: the mark shares
						// not a single field with a general patch, and it
						// moves neither the issue's version nor its
						// updatedAt. It has no agent twin on purpose — see
						// handler/issues_review.go.
						r.Post("/review", issuesH.MarkReviewed)
						r.Put("/components", issuesH.ReplaceComponents)
						// sk-62.2 — create a reminder for the caller on this
						// issue. Snooze / dismiss / list live on the top-level
						// /reminders routes (keyed by reminder id, not issue).
						r.Post("/reminders", remindersH.Create)
						r.Get("/comments", issuesH.ListComments)
						r.Post("/comments", issuesH.AddComment)
						// sk-57 — comment edit (author-only) + soft-delete
						// (author or project owner/admin). Keyed by the
						// comment uuid under the owning issue.
						r.Put("/comments/{commentID}", issuesH.UpdateComment)
						r.Delete("/comments/{commentID}", issuesH.DeleteComment)
						r.Route("/labels", func(r chi.Router) {
							r.Post("/", issuesH.AddLabel)
							r.Delete("/{label}", issuesH.RemoveLabel)
						})
						// Dependencies (R1.4) — outgoing/incoming list +
						// create/delete. CRUD lives under the source issue
						// because the (from, to, type) PK is owned by the
						// FROM side (caller asserts permission via
						// projectID + issueID in the path).
						r.Route("/dependencies", func(r chi.Router) {
							r.Get("/", depsH.List)
							r.Post("/", depsH.Create)
							r.Delete("/{toID}/{type}", depsH.Delete)
						})

						// P5 — per-issue activity feed. Replaces the F5
						// stop-gap (project-feed top-100 + client-side
						// filter) which dropped events for busy projects.
						// Same wire shape as /projects/{id}/activity.
						r.Get("/activity", activityH.ListIssueActivity)
					})
				})

				// Project-wide labels summary — sibling to /issues so the UI
				// can fetch known labels without a paginated issues scan.
				r.Get("/labels", issuesH.ListProjectLabels)

				// Ready algorithm (R1.4) — list all open issues with no open
				// blockers/waits-for, ordered by priority then last update.
				r.Get("/ready", readyH.Ready)

				// R3.4 — per-project activity feed. Visibility enforced
				// by the handler via EnsureProjectVisible.
				r.Get("/activity", activityH.ListProjectActivity)
			})
		})
	})

	return r, drainMCPStreams
}

// spacesOptions are the installation settings of the spaces surface
// (sk-674.18 T023), shared by REST and MCP.
func spacesOptions(cfg *config.Config) spaces.Options {
	return spaces.Options{
		Creation:                  cfg.SpaceCreation,
		OwnedLimit:                cfg.SpaceOwnedLimit,
		MemberLimit:               cfg.SpaceMemberLimit,
		CommonSpaceEnabled:        cfg.CommonSpaceEnabled,
		QuotaPersonalProjects:     cfg.QuotaPersonalProjects,
		QuotaPersonalStorageBytes: cfg.QuotaPersonalStorageBytes,
		QuotaSpaceProjects:        cfg.QuotaSpaceProjects,
		QuotaSpaceStorageBytes:    cfg.QuotaSpaceStorageBytes,
	}
}

// attachmentsStoreWithQuotas is the attachments store of the MCP tools with
// the storage quotas (sk-674.18 T026) — the same as the REST handler's.
func attachmentsStoreWithQuotas(pool *pgxpool.Pool, cfg *config.Config) *attachments.Store {
	st := attachments.NewStore(pool)
	st.SetStorageQuotas(cfg.QuotaPersonalStorageBytes, cfg.QuotaSpaceStorageBytes)
	return st
}
