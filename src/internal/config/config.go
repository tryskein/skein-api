// Package config loads service configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	netmail "net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"

	"skein-api/internal/agents"
	"skein-api/internal/monitorcreds"
	"skein-api/internal/oauthmeta"

	"github.com/caarlos0/env/v11"
	"golang.org/x/crypto/bcrypt"
)

// MinMCPHeartbeatInterval is the smallest non-zero MCP_HEARTBEAT_INTERVAL
// Load() accepts. The knob's job is to stay under a gateway idle timeout
// measured in minutes; anything sub-second is a typo, and mcp-go arms one
// ticker per open GET stream, so a typo scales with the agent fleet.
const MinMCPHeartbeatInterval = time.Second

// Config holds skein-api runtime configuration.
//
// All fields are populated from environment variables via caarlos0/env.
// Required fields without envDefault must be set explicitly — a missing
// required env var is a fatal startup error.
type Config struct {
	// Port is the HTTP port the API listens on.
	// Default 5100; can be overridden by PORT env var (e.g. for multi-container
	// deployments in a single namespace where ports may conflict).
	Port string `env:"PORT" envDefault:"5100"`

	// ServiceName identifies this service in logs (added as a slog field on
	// every log entry) and in /metrics labels.
	ServiceName string `env:"SERVICE_NAME" envDefault:"skein-api"`

	// LogLevel selects the slog minimum level: debug | info | warn | error.
	LogLevel string `env:"LOG_LEVEL" envDefault:"info"`

	// Environment is one of: development | production.
	// Affects log level defaults and other behaviors added in later slices.
	Environment string `env:"ENVIRONMENT" envDefault:"production"`

	// DatabaseURL is the PostgreSQL connection string in libpq URL form, e.g.
	// "postgres://skein:qwerty12@postgres:5432/skein?sslmode=disable".
	// Required — no fallback. A missing DATABASE_URL is a fatal startup error
	// (no fallbacks for addresses and keys).
	DatabaseURL string `env:"DATABASE_URL,required"`

	// MigrationsPath is the directory holding golang-migrate SQL files,
	// loaded via the file:// source. Defaults to "./migrations" — the
	// production image is built with WORKDIR=/app and migrations copied to
	// /app/migrations (see Dockerfile). Override only for tests.
	MigrationsPath string `env:"MIGRATIONS_PATH" envDefault:"./migrations"`

	// DatabaseMaxConns caps the pgx pool size PER POD. It MUST be sized so
	// that (max_conns × replica_count, plus deploy surge) stays under the
	// Postgres server's max_connections, which is shared across every
	// replica and pooling client.
	//
	// This is deliberately an explicit knob and NOT derived from the pod's
	// CPU count: runtime.NumCPU() returns the *node's* core count (a CPU
	// limit is a cgroup quota, it does not shrink the affinity mask), so on
	// a large node the old "NumCPU*2+1" sizing produced ~193 conns/pod and
	// exhausted the DB (SQLSTATE 53300). Set via DATABASE_MAX_CONNS in the
	// deployment config; the default here is a conservative floor for local
	// docker-compose. Must be >= 2 (the pool MinConns floor).
	DatabaseMaxConns int32 `env:"DATABASE_MAX_CONNS" envDefault:"10"`

	// InternalSecret authenticates skein-web (BFF) calls into skein-api on
	// every /api/v1/* request via the X-Skein-Internal-Secret header.
	// Required — there is no public path through the API, so a missing
	// secret is a fatal misconfiguration that we want to surface at startup
	// rather than at first 401. Validated for non-trivial length below.
	InternalSecret string `env:"SKEIN_INTERNAL_SECRET,required"`

	// BcryptCost is the work factor passed to bcrypt for password hashing.
	// 12 is the project-wide default (the project guidelines). Tests may
	// override down to bcrypt.MinCost (4) to keep suites fast — production
	// MUST stay at 12 or higher.
	BcryptCost int `env:"SKEIN_BCRYPT_COST" envDefault:"12"`

	// InitialAdminEmail seeds the first admin user when the users table is
	// empty (R1.1 standalone-auth bootstrap path). Optional — when blank,
	// Bootstrap is a no-op and the operator is expected to provision the
	// first user out-of-band (e.g. via Keycloak in R5.x).
	InitialAdminEmail string `env:"SKEIN_INITIAL_ADMIN_EMAIL"`

	// InitialAdminPassword is required iff InitialAdminEmail is set.
	// Validation happens in auth.Bootstrap so the env-load step stays
	// declarative.
	InitialAdminPassword string `env:"SKEIN_INITIAL_ADMIN_PASSWORD"`

	// InitialAdminUsername (optional). If blank, derived from
	// InitialAdminEmail via auth.UsernameFromEmail.
	InitialAdminUsername string `env:"SKEIN_INITIAL_ADMIN_USERNAME"`

	// InitialAdminName (optional). If blank, falls back to the resolved
	// username.
	InitialAdminName string `env:"SKEIN_INITIAL_ADMIN_NAME"`

	// ---------------- R1.5 search ----------------
	//
	// OllamaURL is the base URL of the Ollama server hosting the
	// embedding model. Required — without it the search service starts
	// but is permanently degraded (FTS-only).  We do NOT default this:
	// silently degrading on a typo would surprise operators.
	OllamaURL string `env:"OLLAMA_URL"`

	// OllamaModel is the Ollama model name to call.  qwen3-embedding:0.6b
	// is the org-standard 1024-dim model.  Override only when validating
	// alternative models (the embedding column is halfvec(1024); a
	// different dim requires a migration).
	OllamaModel string `env:"OLLAMA_MODEL" envDefault:"qwen3-embedding:0.6b"`

	// OllamaAPIKey is the optional Bearer token sent to the Ollama
	// upstream. Required when the deployment puts an authenticating
	// reverse proxy in front of the model server; empty in local
	// docker-compose where the Ollama container is unauthenticated.
	// When set, the embedder adds `Authorization: Bearer <key>` to
	// every /api/embed request.
	OllamaAPIKey string `env:"OLLAMA_API_KEY"`

	// OllamaEmbeddingDim must match the halfvec(N) column width and the
	// model's actual output. Mismatched dims are a fatal startup error
	// (Embedder construction validates this).
	OllamaEmbeddingDim int `env:"OLLAMA_EMBEDDING_DIM" envDefault:"1024"`

	// EmbeddingWorkerInterval is the period between worker ticks.  Each
	// tick processes at most EmbeddingBatchSize rows.  Tune up under
	// heavy backfill load.
	EmbeddingWorkerInterval time.Duration `env:"EMBEDDING_WORKER_INTERVAL" envDefault:"5s"`

	// EmbeddingBatchSize bounds how many rows the worker pulls in one
	// transaction.  The Ollama batch endpoint is fastest at 16..64;
	// going higher pays diminishing returns and risks longer locks.
	EmbeddingBatchSize int `env:"EMBEDDING_BATCH_SIZE" envDefault:"32"`

	// SearchResultCacheTTL is the in-process LRU TTL for search
	// result-sets.  Cursor pagination piggybacks on this cache.
	SearchResultCacheTTL time.Duration `env:"SEARCH_RESULT_CACHE_TTL" envDefault:"60s"`

	// SearchQueryEmbedCacheSize is the LRU cache size (entries) for
	// query→vector lookups inside the embedder.  Each entry is ~4 KiB
	// (1024 floats), so 512 entries ≈ 2 MiB.
	SearchQueryEmbedCacheSize int `env:"SEARCH_QUERY_EMBED_CACHE_SIZE" envDefault:"512"`

	// ---------------- R3.2 outbox / NATS ----------------
	//
	// NATSURL is the NATS Core endpoint the outbox dispatcher publishes to.
	// When empty, the dispatcher is disabled at startup (a warning is
	// logged) and events accumulate in the events table — they will be
	// drained on the next deploy that has NATS_URL set. This deliberately
	// degrades quietly so dev workflows without NATS still boot; it does
	// NOT degrade silently in production because Helm always sets the var.
	NATSURL string `env:"NATS_URL"`

	// ---------------- R4.2 S3 attachments ----------------
	//
	// S3URL is the S3-compatible endpoint (without scheme prefix when
	// using the SDK directly — but here we accept full http(s):// URLs
	// and normalise inside storage/s3.go). Empty disables the
	// attachments subsystem: every /api/v1/issues/{id}/attachments/*
	// endpoint returns 503 attachments_unavailable, and the cleanup
	// goroutine is not started. This mirrors how NATS_URL controls the
	// outbox dispatcher.
	S3URL string `env:"S3_URL"`

	// S3AccessKey / S3SecretKey credential the SDK against S3.
	// Required iff S3URL is set; validated in storage.NewClient.
	S3AccessKey string `env:"S3_ACCESS_KEY"`
	S3SecretKey string `env:"S3_SECRET_KEY"`

	// S3Bucket is the single bucket holding every attachment. Default
	// matches the org-wide convention. The bucket is auto-created at
	// startup (storage.EnsureBucket) when it does not yet exist.
	S3Bucket string `env:"S3_BUCKET" envDefault:"skein-attachments"`

	// S3UseSSL toggles HTTPS to S3. Defaults false to match the
	// docker-compose stack; production deployments behind ingress should
	// set this true.
	S3UseSSL bool `env:"S3_USE_SSL" envDefault:"false"`

	// S3PublicURL is the URL the BROWSER uses to reach S3 directly
	// (presigned POST/GET URLs). When empty we fall back to S3URL —
	// fine for K8s environments where the same DNS resolves both inside
	// and outside the cluster. In docker-compose with port-forwarding the
	// browser cannot resolve the in-network storage name, so set
	// S3_PUBLIC_URL to the published address, e.g. "http://localhost:3900".
	S3PublicURL string `env:"S3_PUBLIC_URL"`

	// S3Region is the region the S3 client signs requests with, for both the
	// internal client and the client that mints browser-facing presigned URLs.
	// It must equal the region the storage is configured with (Garage:
	// s3_region, RustFS: RUSTFS_REGION; SeaweedFS does not check it). The
	// default keeps the value the code used before it became configurable.
	S3Region string `env:"S3_REGION" envDefault:"us-east-1"`

	// S3CORSAllowedOrigins lists the browser origins the bucket CORS rule is
	// set for at startup (comma separated). The browser uploads and downloads
	// straight to the storage, so a storage that does not reflect Origin by
	// itself (Garage, RustFS) needs this. Empty leaves the bucket CORS alone
	// and the operator configures it.
	S3CORSAllowedOrigins []string `env:"S3_CORS_ALLOWED_ORIGINS" envSeparator:","`

	// AttachmentMaxSizeBytes caps the per-file size enforced by the
	// PresignedPostPolicy and re-checked at confirm time (defence in
	// depth). 500 MB matches the R4.2 plan G-5 decision (a). Tune at
	// deploy time without code change.
	AttachmentMaxSizeBytes int64 `env:"SKEIN_ATTACHMENT_MAX_SIZE_BYTES" envDefault:"524288000"`

	// AttachmentURLTTLSeconds bounds the lifetime of presigned upload /
	// download URLs. 600s (10 min) is the spec default — long enough for
	// a slow upload over a flaky connection, short enough to drop the
	// "pending" row promptly when the user gives up.
	AttachmentURLTTLSeconds int `env:"SKEIN_ATTACHMENT_URL_TTL_SECONDS" envDefault:"600"`

	// AttachmentCleanupInterval is the period between cleanup-goroutine
	// ticks. Each tick scans rows in 'pending' state older than 1h and
	// hard-deletes them (plus best-effort S3 delete for any orphan).
	AttachmentCleanupInterval time.Duration `env:"SKEIN_ATTACHMENT_CLEANUP_INTERVAL" envDefault:"15m"`

	// AttachmentPendingMaxAge is the threshold past which a 'pending' row
	// is considered abandoned. Must be > AttachmentURLTTLSeconds (the
	// presigned URL has long expired by then) — 1h is the conservative
	// default.
	AttachmentPendingMaxAge time.Duration `env:"SKEIN_ATTACHMENT_PENDING_MAX_AGE" envDefault:"1h"`

	// AttachmentOrphanScanInterval is how often the cleanup goroutine
	// walks the entire S3 bucket reconciling orphan objects (objects
	// whose row was hard-deleted while the in-handler best-effort S3
	// delete failed). Pricier than the DB-side stale sweep so we
	// throttle independently — 1h matches the bucket size we expect at
	// R4 launch; busy buckets can dial down at deploy time.
	AttachmentOrphanScanInterval time.Duration `env:"SKEIN_ATTACHMENT_ORPHAN_SCAN_INTERVAL" envDefault:"1h"`

	// ---------------- R5.3 stale lock auto-release ----------------
	//
	// StaleLockThreshold is the minimum age of issues.locked_at past
	// which the reaper auto-clears the lock. 30m balances "agent on a
	// hard task" against "crashed agent's WIP blocks the ready list" —
	// the value was chosen when the reaper was designed (§D-1 of that
	// design discussion). Tunable per
	// deployment; very busy teams may want to dial up to 1h, very lean
	// teams may want 10m to keep the ready-list snappier.
	StaleLockThreshold time.Duration `env:"SKEIN_STALE_LOCK_THRESHOLD" envDefault:"30m"`

	// StaleLockAgentIdle is how long the lock HOLDER's MCP session may be
	// silent before the reaper treats the agent as gone (sk-243). A lock is
	// released only when it is older than StaleLockThreshold AND its holder
	// has been silent for this long.
	//
	// Two knobs rather than one because they answer different questions:
	// the threshold is about the lock, this is about the agent. The reported
	// failure was the timer firing on agents that were demonstrably working —
	// they write to the channel task, to child tasks and to git, never to the
	// head task they hold — so an operator shortening the lock threshold must
	// not silently start declaring live agents dead as well.
	//
	// Do not set below ~2m: agents.last_seen_at is only rewritten when the
	// stored value is older than 60 seconds, so a shorter window fights that
	// throttle and reports busy agents as silent. 0 disables the liveness
	// check and restores the pre-sk-243 age-only rule (logged at WARN).
	StaleLockAgentIdle time.Duration `env:"SKEIN_STALE_LOCK_AGENT_IDLE" envDefault:"30m"`

	// StaleLockInterval is the period between reaper ticks. 1m matches
	// the cheap-indexed-query pattern of other workers; the actual
	// query cost is one indexed UPDATE-RETURNING on
	// issues(locked_by IS NOT NULL AND locked_at < threshold) which is
	// near-zero on a healthy fleet (most ticks find zero rows).
	StaleLockInterval time.Duration `env:"SKEIN_STALE_LOCK_INTERVAL" envDefault:"1m"`

	// ---------------- sk-62.2 reminders ----------------
	//
	// ReminderInterval is the period between reminder-scheduler ticks.
	// 30s makes a reminder ring within half a minute of coming due — fine
	// enough for a human-facing alarm without making the claim query hot.
	// The claim is one indexed range scan over reminders(remind_at) on the
	// un-notified partial index, near-zero when nothing is due.
	ReminderInterval time.Duration `env:"SKEIN_REMINDER_INTERVAL" envDefault:"30s"`

	// ReminderBatchSize bounds how many due reminders one tick claims +
	// rings per SERIALIZABLE statement (FOR UPDATE SKIP LOCKED). The tick
	// drains in batches until cleared, so this is a per-statement cap, not
	// a per-tick ceiling.
	ReminderBatchSize int `env:"SKEIN_REMINDER_BATCH_SIZE" envDefault:"200"`

	// ---------------- sk-303.14 nudges ----------------
	//
	// Three settings of the background nudge (repeat-reminder) runner.
	// All three are declared with FACTORY values, and that is not a
	// silent fallback but a declared contract: the spec requires the
	// batch-per-tick and the number of batches per tick to be
	// configurable limits with factory defaults.
	//
	// None of the three turns the mechanic on or off. Whether it is on
	// is the person's decision in their prefs (the master switch and
	// unsubscribing from a category); the env vars only set the sweep
	// pace. An empty environment therefore leaves the runner working
	// at the factory values, not disabled.
	//
	// ⚠ The numbers in the tags and the constants in internal/nudges
	// (DefaultInterval, DefaultBatchSize, DefaultBatchesPerTick) are
	// ONE decision in two places and move together. The second place
	// exists because the runner must be usable even when constructed
	// past this parse (from a test, for example); a gate holds the
	// match, not attentiveness.

	// NudgeInterval — period between ticks of the nudge runner.
	NudgeInterval time.Duration `env:"SKEIN_NUDGE_INTERVAL" envDefault:"1m"`

	// NudgeBatchSize — BATCH PER TICK: how many addressees of one
	// category a single select query claims.
	NudgeBatchSize int `env:"SKEIN_NUDGE_BATCH_SIZE" envDefault:"200"`

	// NudgeBatchesPerTick — NUMBER OF BATCHES PER TICK: how many
	// batches in a row one tick processes before yielding until the
	// next.
	NudgeBatchesPerTick int `env:"SKEIN_NUDGE_BATCHES_PER_TICK" envDefault:"10"`

	// ---------------- sk-622.21 Watching window and retention (US-8) ----------------

	// WatchingFeedWindowDays is the Watching window in days: the badge and
	// the Watching column count and show only notifications created within
	// the last N days (Mentions have no window). Must be > 0.
	WatchingFeedWindowDays int `env:"WATCHING_FEED_WINDOW_DAYS" envDefault:"21"`

	// NotificationsRetentionDays is how many days notifications are kept;
	// older ones are deleted in batches by the background cleanup, except
	// mentions, which are never deleted. Must be > 0 and >= the Watching
	// window, otherwise the cleanup would delete rows the window still
	// shows (edge 17). Violations fail startup.
	NotificationsRetentionDays int `env:"NOTIFICATIONS_RETENTION_DAYS" envDefault:"90"`

	// ---------------- R5.1 Keycloak SSO ----------------
	//
	// AuthMode selects the identity source for /api/v1/* routes:
	//   "local"    — password login via /auth/login (default; dev). Every
	//                user may authenticate with a password.
	//   "keycloak" — SSO via oauth2-proxy + skein-web BFF; /auth/login
	//                returns 410 Gone. The local API key (ska_…) channel
	//                continues to work for MCP agents — see plan §1.
	//   "hybrid"   — sk-163 S4a: OIDC for INTERNAL users + password login for
	//                EXTERNAL users simultaneously. /auth/login proceeds to
	//                credential verification, but the password channel only
	//                authenticates is_external accounts (the gate is applied
	//                AFTER bcrypt to avoid an account-class timing oracle —
	//                see handler.AuthHandler.Login).
	// In both "keycloak" and "hybrid" the single break-glass admin
	// (BreakGlassAdmin) may still password-login so a fresh OIDC deployment
	// is never locked out before the first external user exists.
	// JWT validation itself lives in skein-web BFF; skein-api receives
	// already-parsed identity claims via /api/v1/auth/oidc/provision.
	AuthMode string `env:"SKEIN_AUTH_MODE" envDefault:"local"`

	// BreakGlassAdmin is a single admin email permitted to password-login
	// even when OIDC is active ("keycloak" or "hybrid" mode). Empty (the
	// default) disables the mechanism entirely.
	//
	// Why it exists: in a pure-OIDC deployment the password channel is shut.
	// But the only way to MINT the first external user (who then logs in by
	// password) is an authenticated admin session — and an internal admin can
	// only reach one via SSO. If SSO is misconfigured, or no external user
	// exists yet, the deployment is bricked. The break-glass admin is the
	// escape hatch: that one email keeps a password path open.
	//
	// Security trade-off: this is a permanently-open password surface on an
	// internal identity, so it is scoped as tightly as possible — exactly one
	// configured email, password path only (it grants no extra authority
	// beyond that account's existing role). It is compared case-insensitively
	// against the lowercased login email. Network-level restriction of this
	// path to the internal host is enforced separately (S4b/S10) and is out
	// of scope for the API-level allowance here. Leave it empty in
	// deployments that have a working SSO admin and at least one external
	// user, to keep the attack surface minimal.
	BreakGlassAdmin string `env:"SKEIN_BREAKGLASS_ADMIN"`

	// AutoProvision controls whether the OIDC provision endpoint creates
	// a users row on first SSO login for a sub that is not yet in the
	// table. When false, unknown subs are rejected with 403
	// user_not_provisioned and an admin must seed the user out-of-band.
	// Only consulted when AuthMode == "keycloak".
	AutoProvision bool `env:"SKEIN_AUTO_PROVISION" envDefault:"false"`

	// OIDCDefaultRole is the role assigned to auto-provisioned users
	// whose email does NOT match BootstrapAdminEmail. Allowed values:
	// "user" | "admin". Validated at config-load time in keycloak mode
	// so a typo (e.g. "users") fails the pod before the first request.
	OIDCDefaultRole string `env:"SKEIN_OIDC_DEFAULT_ROLE" envDefault:"user"`

	// BootstrapAdminEmail is the single email that gets elevated to
	// role='admin' on its first SSO login. Empty disables bootstrap (all
	// auto-provisioned users come in as OIDCDefaultRole). Compared
	// case-insensitively against the JWT email claim — Provisioner
	// lower-cases at construction time.
	BootstrapAdminEmail string `env:"SKEIN_BOOTSTRAP_ADMIN_EMAIL"`

	// ---------------- Web Push (VAPID) ----------------
	//
	// VAPIDPublic / VAPIDPrivate are the base64url-encoded P-256 keypair
	// used to sign Web Push requests to browser push services (FCM, Mozilla
	// autopush, …). Generated once per environment via
	// webpush.GenerateVAPIDKeys(); the PUBLIC key is also handed to the
	// browser (GET /api/v1/push/config) so it can subscribe with a matching
	// applicationServerKey.
	//
	// Deliberately NOT marked required: Web Push is optional infrastructure.
	// When either key is empty the send path and /push/config degrade
	// gracefully (no push delivered; the frontend feature-detects an empty
	// publicKey and hides the enable-notifications affordance) so the
	// service still boots in environments that haven't provisioned keys.
	VAPIDPublic  string `env:"SKEIN_VAPID_PUBLIC"`
	VAPIDPrivate string `env:"SKEIN_VAPID_PRIVATE"`

	// VAPIDSubject is the `Subscriber` (the "sub" / contact) sent in the
	// VAPID JWT. Push services require a mailto: or https: URI identifying
	// the application server operator. Defaults to a placeholder so the
	// send path has a syntactically valid subject in dev.
	VAPIDSubject string `env:"SKEIN_VAPID_SUBJECT" envDefault:"mailto:admin@skein.local"`

	// ---------------- sk-303.20 self-service password page ----------------
	//
	// PwnedPasswordsEnabled gates the server-side Have I Been Pwned
	// k-anonymity lookup that ChangePassword runs against the caller's NEW
	// password. Deliberately envDefault "false": Skein is self-hosted by
	// organisations that may run in a closed network with no route to the
	// public internet, and an unexpected outbound call to a third-party
	// service on every password change is not something an operator should
	// get without opting in. When disabled, ChangePassword never dials out
	// and GET /me/password/policy reports pwnedCheckEnabled=false so the
	// frontend never attempts its own hint lookup either — the server is the
	// single source of truth for "do we call out", not each client deciding
	// independently.
	PwnedPasswordsEnabled bool `env:"SKEIN_PWNED_PASSWORDS_ENABLED" envDefault:"false"`

	// PwnedPasswordsBaseURL is the k-anonymity range-lookup endpoint's base
	// URL (no trailing slash; "/range/{prefix}" is appended). Overridable so
	// a closed-network deployment can point at a self-hosted mirror of the
	// published Pwned Passwords corpus instead of the public API.
	PwnedPasswordsBaseURL string `env:"SKEIN_PWNED_PASSWORDS_BASE_URL" envDefault:"https://api.pwnedpasswords.com"`

	// PwnedPasswordsTimeout bounds the outbound HTTP call. ChangePassword is
	// synchronous — the user is waiting on a click — so a stalled upstream
	// must not hang the request indefinitely; a network/timeout error is
	// treated as "unknown" and does not block the password change.
	PwnedPasswordsTimeout time.Duration `env:"SKEIN_PWNED_PASSWORDS_TIMEOUT" envDefault:"2s"`

	// AppBaseURL is the public address of the web UI. Two users:
	//   - web push: the `url` field of a payload, so a click opens the issue
	//     card (https://skein.example.com → …/issues/sk-42). When empty the
	//     payload carries a relative path (/issues/<id>) that service workers
	//     resolve against their own origin.
	//   - email (sk-674.18 T022): the links of confirmation, reset and
	//     invitation emails (…/confirm-email/<token>, …/reset-password/<token>,
	//     …/invite/<token>). An email needs an absolute link, so the variable
	//     is REQUIRED while SKEIN_EMAIL_ENABLED=true: an absolute http(s) URL,
	//     optionally with a path when the UI lives under one, without query,
	//     fragment or credentials (validateAppBaseURL).
	// No fallback to a guessed host (that would mis-route clicks).
	AppBaseURL string `env:"SKEIN_APP_BASE_URL"`

	// ---------------- sk-142 agent task-monitor credentials ----------------
	//
	// MonitorCredentialCleanupInterval is the period between cleanup-worker
	// sweeps over monitor_credentials. Each sweep hard-deletes credentials
	// past their absolute deadline plus revoked rows older than
	// MonitorCredentialRevokedRetention. The query is one indexed-ish DELETE
	// that finds nothing on a healthy fleet, so 10m is plenty fine-grained
	// (the credentials are short-lived; rows don't pile up fast).
	MonitorCredentialCleanupInterval time.Duration `env:"SKEIN_MONITOR_CREDENTIAL_CLEANUP_INTERVAL" envDefault:"10m"`

	// MonitorCredentialRevokedRetention is how long a revoked monitor
	// credential is kept before the cleanup sweep hard-deletes it — a short
	// audit grace window. 0 deletes revoked rows on the next sweep; negative
	// is rejected at load time. (Rows past their absolute deadline are always
	// deleted regardless of this value.)
	MonitorCredentialRevokedRetention time.Duration `env:"SKEIN_MONITOR_CREDENTIAL_REVOKED_RETENTION" envDefault:"24h"`

	// MonitorPollSafetyLag is the watermark fence applied by the /agent/v1
	// poll endpoint (T4): the poll only returns notifications whose
	// created_at is at or before now() - MonitorPollSafetyLag (DB-side
	// clock). It bounds the bigserial commit-order race — a row whose
	// inserting txn is still in-flight when an earlier-id row commits could
	// be skipped by a plain "id > since" cursor; holding rows back until they
	// are at least this old guarantees their writing txn has long committed
	// (our comment + FanOut write path is a single short txn). Must be > 0:
	// a zero/negative lag removes the fence and re-opens the skip window.
	MonitorPollSafetyLag time.Duration `env:"SKEIN_MONITOR_POLL_SAFETY_LAG" envDefault:"5s"`

	// MonitorRunnerPollInterval is the base cadence the Python poll runner
	// (served by GET /agent/v1/monitor-script, T6) sleeps between polls of
	// the /agent/v1/notifications/poll endpoint. It is templated into the
	// served runner body as an integer number of seconds, so the operator
	// tunes the fleet-wide poll rate without shipping a new runner. 2s is the
	// spec default (§D3): cheap on the server at the 10–100-monitor scale,
	// near-real-time enough for inter-agent comments. Empty polls back off
	// from this base up to a runner-side ceiling. Must be > 0: a zero/negative
	// interval would template a busy-loop (or a non-positive sleep) into the
	// runner.
	MonitorRunnerPollInterval time.Duration `env:"SKEIN_MONITOR_RUNNER_POLL_INTERVAL" envDefault:"2s"`

	// MonitorTokenIdleTTL is the sliding renewal window of a monitor token
	// (sk-579, US-2, INV-1): on mint and on every successful validation the
	// token's expiry moves to min(now + MonitorTokenIdleTTL, mint + 12h). A
	// break in polling shorter than the window (Monitor timeout, server
	// restart, compaction) therefore does not kill the token. Must satisfy
	// 0 < window <= monitorcreds.MaxLifetime (12h): a non-positive window
	// would expire every token at birth, and one past the 12h ceiling could
	// never take effect. Either fails startup.
	MonitorTokenIdleTTL time.Duration `env:"SKEIN_MONITOR_TOKEN_IDLE_TTL" envDefault:"6h"`

	// MonitorStallThreshold is how long a live monitor may go without a
	// successful poll before the server reports it as stalled (sk-579, US-5,
	// INV-5). INV-6: it must be strictly greater than the longest gap
	// between two requests of a live runner at the configured poll interval
	// (RunnerTimings(...).MaxPollGap()), otherwise a healthy runner in
	// backoff would be reported stalled; and strictly greater than the
	// agents.last_seen_at granularity (agents.LastSeenGranularity), otherwise
	// an active issuing session could look idle. Violations fail startup.
	// The value is templated into the served runner (its degraded line). The
	// server-side monitorStalled hint that also read it is not registered
	// since sk-669 (owner decision); the validation stays for the runner.
	MonitorStallThreshold time.Duration `env:"SKEIN_MONITOR_STALL_THRESHOLD" envDefault:"3m"`

	// AgentPublicURL is the externally-reachable base URL of the agent
	// data-plane door (e.g. "https://mcp.example.com"). The
	// skein_get_monitor_script MCP tool (sk-142.7) templates it into the
	// bootstrap command it hands the agent (SKEIN_PUBLIC_URL=…), and the
	// served Python runner appends "/agent/v1/monitor-script" and
	// "/agent/v1/notifications/poll" to it. It is admin-supplied env — NOT
	// derived from any request — so a hostile issue id can never redirect a
	// runner at an attacker host. Required (no default) and validated in
	// Load() as an absolute http(s) URL with a host: the monitor feature is
	// inert without a reachable public URL, and a relative / non-http(s)
	// value would template a broken or surprising bootstrap. We still
	// shell-quote it at the tool boundary (defence in depth), but validating
	// here means a misconfigured pod refuses to start rather than minting
	// tokens against a dead URL.
	AgentPublicURL string `env:"SKEIN_AGENT_PUBLIC_URL"`

	// OAuthAuthorizationServers lists the issuer identifiers of the OAuth 2.0
	// authorization servers that mint access tokens for this resource — the
	// `authorization_servers` array of the RFC 9728 protected-resource
	// metadata document served on /.well-known/oauth-protected-resource[/mcp]
	// (sk-304). Comma-separated; in this org it is a single Keycloak realm,
	// e.g. "https://auth.example.com/realms/main".
	//
	// OPTIONAL, and unset means OAuth discovery is switched OFF: the two
	// /.well-known routes are not registered and the /mcp 401 carries no
	// WWW-Authenticate challenge. Everything else, /mcp with a static
	// `ska_` api key included, works exactly as before. Startup logs a
	// warning naming this variable so the disabled feature is not invisible.
	//
	// It was REQUIRED in the first cut of sk-304 and that was wrong (review
	// F1). There is indeed no value we could invent — the realm URL is
	// deployment-specific — and the MCP authorization spec does make
	// `authorization_servers` a MUST with at least one entry once the
	// document is served at all. But "we cannot guess it" justifies refusing
	// to SERVE the document, not refusing to BOOT: the live production
	// ConfigMap does not carry this variable, so the next rollout of this
	// image would have taken the entire API down — every REST route, every
	// MCP session — to withhold an optional discovery document. The project
	// rule about fallback-free env vars targets silently wrong DEFAULTS; a
	// feature that announces itself as off is not one.
	//
	// A value that IS set but malformed (not an absolute http(s) issuer, or
	// carrying query/fragment/userinfo) is still fatal at startup — that is
	// the silently-wrong case, and there the operator has the value in hand.
	//
	// It is NOT the same knob as skein-web's Authentication:Oidc:Authority
	// even when the value coincides: that one is where the BFF sends BROWSER
	// users to log in; this one is what we publish to MACHINE clients as the
	// token issuer for the MCP resource.
	OAuthAuthorizationServers []string `env:"SKEIN_OAUTH_AUTHORIZATION_SERVERS" envSeparator:","`

	// MCPHeartbeatInterval is how often the Streamable-HTTP transport writes
	// a JSON-RPC `ping` into a client's standalone GET (SSE) stream, so the
	// network path never looks idle to an intermediary (sk-274.1).
	//
	// Why this exists: the cluster gateway (Cilium/Envoy) closes a stream
	// after its idle timeout — an operator-wide flag we cannot override per
	// route. The MCP client cannot be relied on to recover: the TypeScript
	// SDK retries a dropped GET stream only twice (1s, 1.5s) and then leaves
	// it dead. So the server keeps the stream warm instead.
	//
	// It only affects clients that actually open the GET stream (mcp-go's
	// heartbeat ticker lives in the GET handler); it does nothing for POST
	// request/response traffic. Keep it comfortably below the gateway idle
	// timeout — 30s against the current 900s leaves room for a lost tick and
	// a reconnect.
	//
	// Set MCP_HEARTBEAT_INTERVAL=0 to disable heartbeats (mcp-go's own
	// default). Note that an *empty* value is not "disabled": caarlos0/env
	// falls back to envDefault for empty vars, so disabling must be the
	// explicit 0. Negative is rejected at load time.
	MCPHeartbeatInterval time.Duration `env:"MCP_HEARTBEAT_INTERVAL" envDefault:"30s"`

	// ---------------- sk-674.18 spaces: installation settings (SAAS-PLAN 2.2) ----------------
	//
	// Every behaviour flag below defaults to what a corporate installation
	// does TODAY (K-30): upgrading an existing deployment needs no new
	// variable and changes no behaviour. The "SaaS" preset sets them all
	// explicitly. An empty limit or quota means "no limit"; the pointer stays
	// nil for both an unset and an empty variable.
	//
	// Contradictory combinations (SAAS-PLAN 2.5, INV-21) are rejected in
	// Load (pure variable conflicts) and in startup_checks.go (conflicts with
	// the data). Every rejection is a *ConflictError naming the variables.

	// InstanceAdminEnabled — whether the instance admin role exists at all,
	// with its "admin sees everything" bypass. When false no user may hold
	// role='admin' (INV-20). Written into instance_settings at startup so the
	// database access function reads a trusted value (K-2, D-2).
	InstanceAdminEnabled bool `env:"SKEIN_INSTANCE_ADMIN_ENABLED" envDefault:"true"`

	// CommonSpaceEnabled — whether users created by an admin or by SSO join
	// the common space automatically, and whether a new installation gets a
	// common space at all (SAAS-PLAN 2.6).
	CommonSpaceEnabled bool `env:"SKEIN_COMMON_SPACE_ENABLED" envDefault:"true"`

	// SpaceMemberLimit — people in one space. nil = no limit; must be >= 1
	// (the owner occupies a seat).
	SpaceMemberLimit *int `env:"SKEIN_SPACE_MEMBER_LIMIT"`

	// SpaceOwnedLimit — spaces one user may own, the personal space not
	// counted. nil = no limit; must be >= 0.
	SpaceOwnedLimit *int `env:"SKEIN_SPACE_OWNED_LIMIT"`

	// SpaceInviteTTL — lifetime of a space invitation. Must be > 0.
	SpaceInviteTTL time.Duration `env:"SKEIN_SPACE_INVITE_TTL" envDefault:"168h"`

	// SpaceInvitesPerDay — invitations one sender may send in a sliding 24h
	// window. nil = no limit; must be >= 0.
	SpaceInvitesPerDay *int `env:"SKEIN_SPACE_INVITES_PER_DAY"`

	// SpaceCreation — who may create new regular spaces: all | admin | none.
	SpaceCreation string `env:"SKEIN_SPACE_CREATION" envDefault:"all"`

	// ProjectDefaultVisibility — visibility of new projects: space | members.
	ProjectDefaultVisibility string `env:"SKEIN_PROJECT_DEFAULT_VISIBILITY" envDefault:"members"`

	// SignupEnabled — self-service registration.
	SignupEnabled bool `env:"SKEIN_SIGNUP_ENABLED" envDefault:"false"`

	// TurnstileSiteKey / TurnstileSecret — Cloudflare Turnstile captcha on
	// registration. Both set or both empty; empty means no captcha.
	TurnstileSiteKey string `env:"SKEIN_TURNSTILE_SITE_KEY"`
	TurnstileSecret  string `env:"SKEIN_TURNSTILE_SECRET"`

	// ProjectPrefixMinLength — minimum project prefix length. nil = no
	// minimum; otherwise 1..ProjectPrefixMaxLength.
	ProjectPrefixMinLength *int `env:"SKEIN_PROJECT_PREFIX_MIN_LENGTH"`

	// EmailEnabled — outgoing mail over SMTP. When true every SKEIN_SMTP_*
	// below is required (K-10).
	EmailEnabled bool `env:"SKEIN_EMAIL_ENABLED" envDefault:"false"`

	// SMTPHost / SMTPPort / SMTPUsername / SMTPPassword / SMTPFrom — the SMTP
	// relay and the sender address. Required iff EmailEnabled.
	SMTPHost     string `env:"SKEIN_SMTP_HOST"`
	SMTPPort     int    `env:"SKEIN_SMTP_PORT"`
	SMTPUsername string `env:"SKEIN_SMTP_USERNAME"`
	SMTPPassword string `env:"SKEIN_SMTP_PASSWORD"`
	SMTPFrom     string `env:"SKEIN_SMTP_FROM"`

	// SMTPTLSMode — implicit (TLS before the greeting, usually port 465) or
	// starttls (mandatory STARTTLS before AUTH/DATA). There is no plaintext
	// mode. Required iff EmailEnabled.
	SMTPTLSMode string `env:"SKEIN_SMTP_TLS_MODE"`

	// EmailDeliveryTTL — how long a queued email keeps being retried before
	// it is given up as expired (K-9). Must be > 0.
	EmailDeliveryTTL time.Duration `env:"SKEIN_EMAIL_DELIVERY_TTL" envDefault:"24h"`

	// OperatorAPIEnabled / OperatorAPIPort / OperatorAPIKey — the internal
	// SaaS operator API on its own port (SAAS-PLAN 8a). The key is required
	// (>= 16 characters) iff the API is enabled.
	OperatorAPIEnabled bool   `env:"SKEIN_OPERATOR_API_ENABLED" envDefault:"false"`
	OperatorAPIPort    string `env:"SKEIN_OPERATOR_API_PORT" envDefault:"5101"`
	OperatorAPIKey     string `env:"SKEIN_OPERATOR_API_KEY"`

	// Quotas (SAAS-PLAN 4.5). nil = no quota; must be >= 0.
	QuotaPersonalProjects     *int   `env:"SKEIN_QUOTA_PERSONAL_PROJECTS"`
	QuotaPersonalStorageBytes *int64 `env:"SKEIN_QUOTA_PERSONAL_STORAGE_BYTES"`
	QuotaSpaceProjects        *int   `env:"SKEIN_QUOTA_SPACE_PROJECTS"`
	QuotaSpaceStorageBytes    *int64 `env:"SKEIN_QUOTA_SPACE_STORAGE_BYTES"`

	// TermsURL / PrivacyURL — terms of service and privacy policy shown next
	// to the consent checkbox. Empty = plain text without a link; set = an
	// absolute http(s) URL.
	TermsURL   string `env:"SKEIN_TERMS_URL"`
	PrivacyURL string `env:"SKEIN_PRIVACY_URL"`

	// InstanceName labels every metric (const label skein_instance) and every
	// log record (attribute skein_instance), so several installations can
	// share one monitoring stack (SAAS-PLAN 10, K-24).
	InstanceName string `env:"SKEIN_INSTANCE_NAME" envDefault:"skein"`
}

// ProjectPrefixMaxLength mirrors the upper bound of projects.ValidateIDPrefix
// (^[a-z0-9][a-z0-9_-]{0,49}$): a minimum above it could never be met.
const ProjectPrefixMaxLength = 50

// Values of SKEIN_SPACE_CREATION.
const (
	SpaceCreationAll   = "all"
	SpaceCreationAdmin = "admin"
	SpaceCreationNone  = "none"
)

// Values of SKEIN_SMTP_TLS_MODE.
const (
	SMTPTLSImplicit = "implicit"
	SMTPTLSStartTLS = "starttls"
)

// ConflictError is a startup refusal caused by contradictory settings, or by
// settings that contradict the data (SAAS-PLAN 2.5, INV-20, INV-21, SC-007).
// Vars is the machine-readable part of the contract: the variables of the
// offending combination, in a stable order. Reason is prose for the operator.
type ConflictError struct {
	Vars   []string
	Reason string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("contradictory settings (%s): %s", strings.Join(e.Vars, ", "), e.Reason)
}

func conflict(reason string, vars ...string) *ConflictError {
	return &ConflictError{Vars: vars, Reason: reason}
}

// Load parses configuration from the process environment.
// Returns a fully-validated Config or an error describing missing/invalid vars.
func Load() (*Config, error) {
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	origins, err := normalizeCORSOrigins(cfg.S3CORSAllowedOrigins)
	if err != nil {
		return nil, fmt.Errorf("s3_cors_allowed_origins: %w", err)
	}
	cfg.S3CORSAllowedOrigins = origins

	// Cheap sanity checks on values that env.Parse can't catch:
	//   - InternalSecret: even short values "work" but are useless against a
	//     dictionary attack. 16 chars is the standard floor in the org for
	//     internal-channel secrets.
	if len(cfg.InternalSecret) < 16 {
		return nil, fmt.Errorf("skein_internal_secret must be at least 16 characters")
	}
	if cfg.BcryptCost < bcrypt.MinCost || cfg.BcryptCost > bcrypt.MaxCost {
		return nil, fmt.Errorf("skein_bcrypt_cost must be in [%d, %d]; got %d",
			bcrypt.MinCost, bcrypt.MaxCost, cfg.BcryptCost)
	}

	// sk-142 — monitor-credential cleanup knobs. A non-positive interval
	// would silently never sweep (dead worker); a negative retention is
	// nonsensical. Fail fast at startup rather than ship a worker that
	// quietly does nothing or computes "now() - (-1h)" into the future.
	if cfg.MonitorCredentialCleanupInterval <= 0 {
		return nil, fmt.Errorf("skein_monitor_credential_cleanup_interval must be > 0; got %s", cfg.MonitorCredentialCleanupInterval)
	}
	if cfg.MonitorCredentialRevokedRetention < 0 {
		return nil, fmt.Errorf("skein_monitor_credential_revoked_retention must be >= 0; got %s", cfg.MonitorCredentialRevokedRetention)
	}
	// A non-positive safety lag would disable the poll watermark fence,
	// re-opening the bigserial commit-order skip window the fence exists to
	// close. Fail fast rather than ship a poll that can silently drop rows.
	if cfg.MonitorPollSafetyLag <= 0 {
		return nil, fmt.Errorf("skein_monitor_poll_safety_lag must be > 0; got %s", cfg.MonitorPollSafetyLag)
	}
	// sk-142.6 — runner cadence knob templated into the served runner body.
	// A non-positive poll interval would template a busy-loop / non-positive
	// sleep. Fail fast rather than ship a runner that hammers the API.
	if cfg.MonitorRunnerPollInterval <= 0 {
		return nil, fmt.Errorf("skein_monitor_runner_poll_interval must be > 0; got %s", cfg.MonitorRunnerPollInterval)
	}
	// sk-579 — monitor token renewal window (US-2, INV-1, R1).
	if cfg.MonitorTokenIdleTTL <= 0 || cfg.MonitorTokenIdleTTL > monitorcreds.MaxLifetime {
		return nil, fmt.Errorf("skein_monitor_token_idle_ttl must be > 0 and <= %s; got %s",
			monitorcreds.MaxLifetime, cfg.MonitorTokenIdleTTL)
	}
	// sk-579 — stall threshold (INV-6 + the agents granularity floor). Both
	// bounds are strict: a threshold equal to the bound can still flag a
	// healthy runner / an active session.
	stallFloor := RunnerTimings(cfg.MonitorRunnerPollInterval).MaxPollGap()
	if agents.LastSeenGranularity > stallFloor {
		stallFloor = agents.LastSeenGranularity
	}
	if cfg.MonitorStallThreshold <= stallFloor {
		return nil, fmt.Errorf("skein_monitor_stall_threshold must be > %s (the longest gap between "+
			"requests of a live runner at skein_monitor_runner_poll_interval=%s, and the %s "+
			"agent last-seen granularity); got %s",
			stallFloor, cfg.MonitorRunnerPollInterval, agents.LastSeenGranularity, cfg.MonitorStallThreshold)
	}
	// sk-622.21 — Watching window and notification retention (US-8, edge
	// 17). Non-positive values are meaningless; a retention shorter than the
	// window would let the cleanup delete rows the Watching badge and column
	// still count. Equal is allowed.
	if cfg.WatchingFeedWindowDays <= 0 {
		return nil, fmt.Errorf("watching_feed_window_days must be > 0; got %d", cfg.WatchingFeedWindowDays)
	}
	if cfg.NotificationsRetentionDays <= 0 {
		return nil, fmt.Errorf("notifications_retention_days must be > 0; got %d", cfg.NotificationsRetentionDays)
	}
	if cfg.NotificationsRetentionDays < cfg.WatchingFeedWindowDays {
		return nil, fmt.Errorf("notifications_retention_days (%d) must be >= watching_feed_window_days (%d)",
			cfg.NotificationsRetentionDays, cfg.WatchingFeedWindowDays)
	}
	// sk-274.1 — MCP GET-stream heartbeat. 0 disables (mcp-go default).
	// Negative is meaningless. Sub-second is worse than meaningless: mcp-go
	// arms one ticker PER open GET stream, so "1ms" turns every long-lived
	// agent session into a hot write loop. Neither can be a deliberate
	// choice — the knob exists to stay under a gateway idle timeout measured
	// in minutes — so both fail fast at startup rather than degrading a live
	// fleet.
	if cfg.MCPHeartbeatInterval != 0 && cfg.MCPHeartbeatInterval < MinMCPHeartbeatInterval {
		return nil, fmt.Errorf("mcp_heartbeat_interval must be 0 (disabled) or >= %s; got %s",
			MinMCPHeartbeatInterval, cfg.MCPHeartbeatInterval)
	}
	// sk-142.7 — the public base URL the monitor bootstrap points runners at.
	// Required + must be an absolute http(s) URL with a host. We parse with
	// net/url rather than a prefix check so "http://" with no host, a relative
	// path, or a non-http(s) scheme are all rejected. An empty value (env unset
	// or "") fails here too — IsAbs() is false for the zero URL.
	agentURL, err := url.Parse(cfg.AgentPublicURL)
	if err != nil || !agentURL.IsAbs() || (agentURL.Scheme != "http" && agentURL.Scheme != "https") || agentURL.Host == "" {
		return nil, fmt.Errorf("skein_agent_public_url must be an absolute http(s) URL; got %q", cfg.AgentPublicURL)
	}
	// sk-304 — the same public URL is now also the identity we PUBLISH to MCP
	// clients (the `resource` field of the protected-resource metadata
	// document), so it must be https wherever that identity is quoted to the
	// outside world. Plain http stays legal in development, where the local
	// compose entrypoint is http://host.docker.internal:8080.
	//
	// This one stays FATAL, unlike SKEIN_OAUTH_AUTHORIZATION_SERVERS above
	// (sk-304 review F1 asked the question explicitly). The two are not the
	// same class of risk. This variable is already REQUIRED and already
	// present in every deployment — the check constrains a value the operator
	// holds, it does not demand a new one they must invent, so it cannot
	// surprise a rollout the way a missing variable does. And the constraint
	// is not ours: RFC 9728 §1.2 defines a resource identifier as "a URL that
	// uses the https scheme", so publishing an http identifier would emit a
	// non-conformant document, while the same URL already mints agent-monitor
	// tokens (sk-142.7) whose delivery over plaintext is a real leak. Degrading
	// to a warning would mean shipping both defects quietly, which is the
	// silently-wrong-value case the fail-fast rule exists for. The fix here is
	// one character, in a value already in the ConfigMap.
	if cfg.Environment == "production" && agentURL.Scheme != "https" {
		return nil, fmt.Errorf("skein_agent_public_url must use https in production; got %q", cfg.AgentPublicURL)
	}
	// sk-304 — validate the protected-resource metadata inputs here, so a
	// pod that WOULD serve a wrong document refuses to start instead of
	// serving one that misdescribes the door: the failure mode that made
	// sk-280 look closed while the defect was live. The router rebuilds the
	// descriptor from the same (now validated) inputs.
	//
	// Guarded by Enabled(): with no authorization server configured the
	// feature is off, not misconfigured — see the field comment. buildRouter
	// makes the same test, so the two stay in step.
	if oauthmeta.Enabled(cfg.OAuthAuthorizationServers) {
		if _, err := oauthmeta.Describe(cfg.AgentPublicURL, cfg.OAuthAuthorizationServers); err != nil {
			return nil, fmt.Errorf("oauth protected resource metadata "+
				"(skein_agent_public_url / skein_oauth_authorization_servers): %w", err)
		}
		if cfg.Environment == "production" {
			for _, as := range cfg.OAuthAuthorizationServers {
				if trimmed := strings.TrimSpace(as); trimmed != "" &&
					!strings.HasPrefix(trimmed, "https://") {
					return nil, fmt.Errorf("skein_oauth_authorization_servers must use https in production; got %q", as)
				}
			}
		}
	}

	// R5.1 — auth mode + OIDC role validation. We deliberately enforce
	// the enum here (not just in handlers) so a misconfigured pod refuses
	// to start rather than silently dropping into the wrong branch on
	// every login attempt.
	switch cfg.AuthMode {
	case "local", "keycloak", "hybrid":
		// ok
	default:
		return nil, fmt.Errorf("skein_auth_mode must be one of {local, keycloak, hybrid}; got %q", cfg.AuthMode)
	}
	// OIDC is active for internal users in BOTH keycloak and hybrid mode, so
	// the auto-provision default role is consulted on the provision path in
	// either — validate the enum for both rather than just keycloak.
	if cfg.AuthMode == "keycloak" || cfg.AuthMode == "hybrid" {
		switch cfg.OIDCDefaultRole {
		case "user", "admin":
			// ok
		default:
			return nil, fmt.Errorf("skein_oidc_default_role must be one of {user, admin}; got %q", cfg.OIDCDefaultRole)
		}
	}

	// Web Push (VAPID) — all-or-nothing. Push is optional, so BOTH keys empty
	// is a valid "push disabled" config. But a half-configured keypair (one key
	// set, the other empty) is a footgun: /push/config would hand the browser a
	// public key and the frontend would enable push, while every Send fails for
	// want of the private key. Fail fast at startup rather than ship a dead
	// feature. When push is enabled the subject must be a mailto: or https: URI
	// (push services reject anything else in the VAPID JWT).
	if cfg.VAPIDPublic != "" || cfg.VAPIDPrivate != "" {
		if cfg.VAPIDPublic == "" || cfg.VAPIDPrivate == "" {
			return nil, fmt.Errorf("skein_vapid_public and skein_vapid_private must both be set or both empty")
		}
		if !strings.HasPrefix(cfg.VAPIDSubject, "mailto:") && !strings.HasPrefix(cfg.VAPIDSubject, "https:") {
			return nil, fmt.Errorf("skein_vapid_subject must start with mailto: or https:; got %q", cfg.VAPIDSubject)
		}
	}

	// sk-303.20 (found in review): PwnedPasswordsTimeout becomes
	// http.Client.Timeout verbatim in internal/pwned.NewClient, and Go
	// defines Timeout <= 0 as "no timeout at all" (net/http docs), not
	// "fast". An operator setting SKEIN_PWNED_PASSWORDS_TIMEOUT=0 hoping
	// for "instant" would instead get an unbounded outbound call on every
	// self-service password change. Validated unconditionally (not only
	// when PwnedPasswordsEnabled) so a value that would misbehave the
	// moment the feature is switched on is caught at startup instead of at
	// enable-time — same posture as the other duration knobs above
	// (MonitorPollSafetyLag, MonitorRunnerPollInterval, …), all of which
	// fail fast rather than accept a value that silently disables their own
	// bound.
	if cfg.PwnedPasswordsTimeout <= 0 {
		return nil, fmt.Errorf("skein_pwned_passwords_timeout must be > 0; got %s", cfg.PwnedPasswordsTimeout)
	}

	// sk-674.18 — installation settings: value checks first, then the
	// contradictions of SAAS-PLAN 2.5 that need no data.
	if err := validateInstanceSettings(cfg); err != nil {
		return nil, err
	}
	if err := checkSettingConflicts(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// validateInstanceSettings checks each installation setting on its own:
// enums, ranges, and the "required iff the feature is on" groups. A missing
// address or key of an enabled feature fails here instead of at first use
// (no silent fallback); a disabled feature accepts empty values.
func validateInstanceSettings(cfg *Config) error {
	switch cfg.SpaceCreation {
	case SpaceCreationAll, SpaceCreationAdmin, SpaceCreationNone:
	default:
		return fmt.Errorf("skein_space_creation must be one of {all, admin, none}; got %q", cfg.SpaceCreation)
	}
	switch cfg.ProjectDefaultVisibility {
	case "space", "members":
	default:
		return fmt.Errorf("skein_project_default_visibility must be one of {space, members}; got %q", cfg.ProjectDefaultVisibility)
	}
	if cfg.SpaceInviteTTL <= 0 {
		return fmt.Errorf("skein_space_invite_ttl must be > 0; got %s", cfg.SpaceInviteTTL)
	}
	if cfg.EmailDeliveryTTL <= 0 {
		return fmt.Errorf("skein_email_delivery_ttl must be > 0; got %s", cfg.EmailDeliveryTTL)
	}

	// Limits and quotas: nil is "no limit"; a set value is a count.
	if v := cfg.SpaceMemberLimit; v != nil && *v < 1 {
		return fmt.Errorf("skein_space_member_limit must be >= 1 or empty (no limit); got %d", *v)
	}
	for _, c := range []struct {
		name string
		v    *int
	}{
		{"skein_space_owned_limit", cfg.SpaceOwnedLimit},
		{"skein_space_invites_per_day", cfg.SpaceInvitesPerDay},
		{"skein_quota_personal_projects", cfg.QuotaPersonalProjects},
		{"skein_quota_space_projects", cfg.QuotaSpaceProjects},
	} {
		if c.v != nil && *c.v < 0 {
			return fmt.Errorf("%s must be >= 0 or empty (no limit); got %d", c.name, *c.v)
		}
	}
	for _, c := range []struct {
		name string
		v    *int64
	}{
		{"skein_quota_personal_storage_bytes", cfg.QuotaPersonalStorageBytes},
		{"skein_quota_space_storage_bytes", cfg.QuotaSpaceStorageBytes},
	} {
		if c.v != nil && *c.v < 0 {
			return fmt.Errorf("%s must be >= 0 or empty (no limit); got %d", c.name, *c.v)
		}
	}
	if v := cfg.ProjectPrefixMinLength; v != nil && (*v < 1 || *v > ProjectPrefixMaxLength) {
		return fmt.Errorf("skein_project_prefix_min_length must be in [1, %d] or empty (no minimum); got %d",
			ProjectPrefixMaxLength, *v)
	}

	// Turnstile — both keys or neither: a site key without the secret would
	// show a captcha the server cannot verify; a lone secret is a typo.
	if (cfg.TurnstileSiteKey == "") != (cfg.TurnstileSecret == "") {
		return fmt.Errorf("skein_turnstile_site_key and skein_turnstile_secret must both be set or both empty")
	}

	for _, u := range []struct{ name, v string }{
		{"skein_terms_url", cfg.TermsURL},
		{"skein_privacy_url", cfg.PrivacyURL},
	} {
		if u.v == "" {
			continue
		}
		parsed, err := url.Parse(u.v)
		if err != nil || !parsed.IsAbs() || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("%s must be an absolute http(s) URL or empty; got %q", u.name, u.v)
		}
	}

	if strings.TrimSpace(cfg.InstanceName) == "" {
		return fmt.Errorf("skein_instance_name must not be blank")
	}

	// SMTP (K-10): every SKEIN_SMTP_* is required while mail is on.
	if cfg.EmailEnabled {
		var missing []string
		for _, s := range []struct{ name, v string }{
			{"SKEIN_SMTP_HOST", cfg.SMTPHost},
			{"SKEIN_SMTP_USERNAME", cfg.SMTPUsername},
			{"SKEIN_SMTP_PASSWORD", cfg.SMTPPassword},
			{"SKEIN_SMTP_FROM", cfg.SMTPFrom},
			{"SKEIN_SMTP_TLS_MODE", cfg.SMTPTLSMode},
		} {
			if strings.TrimSpace(s.v) == "" {
				missing = append(missing, s.name)
			}
		}
		if cfg.SMTPPort == 0 {
			missing = append(missing, "SKEIN_SMTP_PORT")
		}
		// The links of every email are built on the UI's public address.
		if strings.TrimSpace(cfg.AppBaseURL) == "" {
			missing = append(missing, "SKEIN_APP_BASE_URL")
		}
		if len(missing) > 0 {
			return fmt.Errorf("SKEIN_EMAIL_ENABLED=true requires %s", strings.Join(missing, ", "))
		}
		if cfg.SMTPPort < 1 || cfg.SMTPPort > 65535 {
			return fmt.Errorf("skein_smtp_port must be in [1, 65535]; got %d", cfg.SMTPPort)
		}
		switch cfg.SMTPTLSMode {
		case SMTPTLSImplicit, SMTPTLSStartTLS:
		default:
			return fmt.Errorf("skein_smtp_tls_mode must be one of {implicit, starttls}; got %q", cfg.SMTPTLSMode)
		}
		// The sender goes into the SMTP envelope and the From header: one
		// address, optionally with a display name, and never a line break
		// that could start a header of its own.
		if strings.ContainsAny(cfg.SMTPFrom, "\r\n") {
			return errors.New("skein_smtp_from must not contain a line break")
		}
		if _, err := netmail.ParseAddress(cfg.SMTPFrom); err != nil {
			return fmt.Errorf("skein_smtp_from must be an email address, optionally with a display name (\"Skein <skein@example.com>\"): %w", err)
		}
		if err := validateAppBaseURL(cfg.AppBaseURL); err != nil {
			return fmt.Errorf("SKEIN_EMAIL_ENABLED=true requires SKEIN_APP_BASE_URL to be the absolute address of the web UI: %w", err)
		}
	}

	// Operator API (SAAS-PLAN 8a): the key guards a privileged port.
	if cfg.OperatorAPIEnabled {
		if len(cfg.OperatorAPIKey) < 16 {
			return fmt.Errorf("SKEIN_OPERATOR_API_ENABLED=true requires SKEIN_OPERATOR_API_KEY of at least 16 characters")
		}
		port, err := strconv.Atoi(cfg.OperatorAPIPort)
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("skein_operator_api_port must be a port number in [1, 65535]; got %q", cfg.OperatorAPIPort)
		}
		if cfg.OperatorAPIPort == cfg.Port {
			return fmt.Errorf("skein_operator_api_port must differ from port; both are %q", cfg.Port)
		}
	}
	return nil
}

// checkSettingConflicts rejects the contradictions of SAAS-PLAN 2.5 that are
// decided by the variables alone, one check per table row. The rows that
// depend on the data (users with role admin, projects without a space, the
// common space to create, an empty user base) live in startup_checks.go.
func checkSettingConflicts(cfg *Config) error {
	if !cfg.InstanceAdminEnabled {
		// Row 1: the admin role is off, yet an admin is asked for.
		vars := []string{"SKEIN_INSTANCE_ADMIN_ENABLED"}
		for _, s := range []struct{ name, v string }{
			{"SKEIN_INITIAL_ADMIN_EMAIL", cfg.InitialAdminEmail},
			{"SKEIN_INITIAL_ADMIN_PASSWORD", cfg.InitialAdminPassword},
			{"SKEIN_INITIAL_ADMIN_USERNAME", cfg.InitialAdminUsername},
			{"SKEIN_INITIAL_ADMIN_NAME", cfg.InitialAdminName},
			{"SKEIN_BOOTSTRAP_ADMIN_EMAIL", cfg.BootstrapAdminEmail},
			{"SKEIN_BREAKGLASS_ADMIN", cfg.BreakGlassAdmin},
		} {
			if s.v != "" {
				vars = append(vars, s.name)
			}
		}
		if len(vars) > 1 {
			return conflict("the instance admin role is disabled, but an admin is requested", vars...)
		}
		// Row 2: SSO would provision admins.
		if cfg.OIDCDefaultRole == "admin" {
			return conflict("the instance admin role is disabled, but SSO would provision users as admin",
				"SKEIN_INSTANCE_ADMIN_ENABLED", "SKEIN_OIDC_DEFAULT_ROLE")
		}
		// Row 4: nobody could create spaces.
		if cfg.SpaceCreation == SpaceCreationAdmin {
			return conflict("only admins may create spaces, but the instance admin role is disabled",
				"SKEIN_INSTANCE_ADMIN_ENABLED", "SKEIN_SPACE_CREATION")
		}
	}
	// Row 7: open registration without mail — no password reset, no invitations.
	if cfg.SignupEnabled && !cfg.EmailEnabled {
		return conflict("self-service registration needs outgoing mail (password reset, invitations)",
			"SKEIN_SIGNUP_ENABLED", "SKEIN_EMAIL_ENABLED")
	}
	// Row 8: a forgotten password could never be reset.
	if !cfg.EmailEnabled && !cfg.InstanceAdminEnabled && (cfg.AuthMode == "local" || cfg.AuthMode == "hybrid") {
		return conflict("password login with neither mail nor an instance admin: a forgotten password could never be reset",
			"SKEIN_EMAIL_ENABLED", "SKEIN_INSTANCE_ADMIN_ENABLED", "SKEIN_AUTH_MODE")
	}
	// Row 9: a self-registered user could not log in outside local mode
	// (handler/auth.go#passwordLoginAllowed).
	if cfg.SignupEnabled && cfg.AuthMode != "local" {
		return conflict("self-registered users log in with a password, which only local mode allows for them",
			"SKEIN_SIGNUP_ENABLED", "SKEIN_AUTH_MODE")
	}
	return nil
}

// logIdentity is the part of Config a logger needs, with the same env tags
// as the corresponding Config fields.
type logIdentity struct {
	ServiceName  string `env:"SERVICE_NAME" envDefault:"skein-api"`
	InstanceName string `env:"SKEIN_INSTANCE_NAME" envDefault:"skein"`
}

// EarlyLogConfig returns the configuration a logger needs when Load itself
// failed: the service and installation names, read with the same variables
// and defaults as Load. It never fails — a startup refusal must still be
// logged with the installation label. A blank or unparsable value takes the
// default, the same value Load would apply to an unset variable, so the
// record is attributed to this installation rather than to none.
func EarlyLogConfig() *Config {
	id := logIdentity{}
	if err := env.Parse(&id); err != nil {
		id = logIdentity{}
	}
	if strings.TrimSpace(id.ServiceName) == "" {
		id.ServiceName = "skein-api"
	}
	if strings.TrimSpace(id.InstanceName) == "" {
		id.InstanceName = "skein"
	}
	return &Config{ServiceName: id.ServiceName, InstanceName: id.InstanceName}
}

// validateAppBaseURL accepts the public address of the web UI as the email
// links need it: an absolute http(s) URL with a host, optionally a path (a
// UI served under a prefix), and nothing that would be glued in front of a
// link path wrongly — no query, no fragment, no user:password.
func validateAppBaseURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("not a URL: %w", err)
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Errorf("scheme must be http or https, got %q", u.Scheme)
	case u.Host == "":
		return errors.New("no host")
	case u.User != nil:
		return errors.New("credentials are not allowed")
	case u.RawQuery != "" || u.ForceQuery:
		return errors.New("a query is not allowed")
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return errors.New("a fragment is not allowed")
	}
	return nil
}

// normalizeCORSOrigins trims every item of S3_CORS_ALLOWED_ORIGINS, drops the
// empty ones (a trailing comma, a space after a comma) and rejects an item
// that is neither "*" nor an origin. env only splits on the separator, so a
// stray comma would otherwise reach the storage as an empty AllowedOrigin and
// fail the whole rule at run time, far from the cause.
func normalizeCORSOrigins(in []string) ([]string, error) {
	var out []string
	for _, o := range in {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		if o != "*" && !isOrigin(o) {
			return nil, fmt.Errorf("%q is not an origin (want http(s)://host[:port] without credentials, path or trailing slash, or *)", o)
		}
		out = append(out, o)
	}
	return out, nil
}

// isOrigin reports whether s has the shape of a browser Origin header value:
// http or https, a host, an optional numeric port, and nothing else — no
// credentials, path, query or fragment, no trailing slash. A rule written in
// any other shape never matches what the browser sends.
func isOrigin(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return u.Hostname() != "" && u.User == nil && u.Path == "" && u.RawPath == "" &&
		u.RawQuery == "" && u.Fragment == "" && !u.ForceQuery && !strings.HasSuffix(s, "/")
}
