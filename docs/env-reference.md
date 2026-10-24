# skein-api — environment variable reference

**This file is the single source of truth for skein-api configuration.** Every
variable the service reads is listed here exactly once. The README, runbooks and
deployment manifests link here; they do not keep lists of their own.

Configuration is loaded once at process start by `src/internal/config/config.go`
(`config.Load()`), which uses [`caarlos0/env/v11`](https://github.com/caarlos0/env)
to populate a struct from the process environment. There is no config file, no
flags, and no remote config source.

No test compares this file with the code, so a commit that adds, renames or
removes a variable updates this file as well.

## Rules that apply to every variable

- **Unset vs. empty.** For a variable that has a default, setting it to an empty
  string is treated as *unset* and the default is applied (`caarlos0/env`
  v11.4.0, `getOr`). For a *required* variable, an explicitly empty value counts
  as "set" and passes the required check — the value then fails (or misbehaves)
  further down. Do not use `VAR=""` to mean "leave it out"; omit the variable.
- **Missing required variable** aborts startup before the HTTP listener opens,
  with `parsing config: required environment variable "NAME" is not set`.
- **Durations** are Go duration strings (`30s`, `1m`, `90m`, `1h30m`). A value
  without a unit is a parse error and aborts startup.
- **Booleans** accept what `strconv.ParseBool` accepts: `1 t T TRUE true True`
  and `0 f F FALSE false False`. Anything else aborts startup.
- **Integers** are decimal. A non-numeric value aborts startup.
- Validation beyond type parsing happens in `config.Load()`; every such check is
  described in the entry for its variable below.

<!-- prod-required:begin -->
## Must be set in production — no safe default exists

| Variable | Why it has no default |
|---|---|
| `DATABASE_URL` | The database address and credentials are deployment-specific. Required; the service refuses to start without it. |
| `SKEIN_INTERNAL_SECRET` | Shared secret authenticating skein-web → skein-api. A default would be a published credential. Required, minimum 16 characters. |
| `SKEIN_AGENT_PUBLIC_URL` | The externally reachable base URL of this service; handed to agents in monitor bootstrap commands. Required, must be absolute `http(s)`, and must be `https` when `ENVIRONMENT=production`. |
<!-- prod-required:end -->

A deployment also needs skein-web configured correctly — in particular its
`SKEIN_TRUSTED_PROXIES`, where a wrong value is a security defect. Those
variables live in
[skein-web's reference](https://github.com/tryskein/skein-web/blob/main/docs/env-reference.md); skein-api does
not read them.

Variables that are *optional by design but disable a feature when absent* —
`OLLAMA_URL` (semantic search), `NATS_URL` (live event fan-out), `S3_URL`
(attachments), `SKEIN_VAPID_*` (web push) — are called out individually below.
A production deployment that wants those features must set them; the service
starts either way and logs a warning naming the variable.

---

## Process and logging

#### `PORT`

- **Purpose:** TCP port the HTTP server listens on (`:PORT`, all interfaces).
- **Required:** no · **Default:** `5100` · **Format:** decimal port number as a
  string.
- **If unset:** listens on 5100. The Docker image also sets `ENV PORT=5100`, so
  the variable is present in a container even when the operator does not set it.
- **Related:** the container image documents 5100 via `EXPOSE`; changing `PORT`
  does not change the `EXPOSE` metadata.

#### `SERVICE_NAME`

- **Purpose:** value of the `service` field attached to every log record.
- **Required:** no · **Default:** `skein-api` · **Format:** free text.
- **If unset:** logs carry `service=skein-api`.
- **Note:** it is *not* used as a Prometheus label — the only reader is
  `setupLogger` in `cmd/api/main.go`.

#### `LOG_LEVEL`

- **Purpose:** minimum slog level written to stdout.
- **Required:** no · **Default:** `info` · **Format:** `debug`, `info`, `warn`
  (or `warning`), `error`; case-insensitive, surrounding spaces ignored.
- **If unset, or set to an unrecognised value:** falls back to `info` silently —
  a typo does not fail startup, it just leaves the level at info.
- **Related:** `ENVIRONMENT=development` forces the level down to `debug`
  regardless of what `LOG_LEVEL` says, so in development this variable can only
  be observed when it already asks for `debug`.

#### `ENVIRONMENT`

- **Purpose:** deployment class. Two values are meaningful to the code:
  `development` forces debug logging; `production` enables two hard
  requirements — `SKEIN_AGENT_PUBLIC_URL` must be `https`, and every entry of
  `SKEIN_OAUTH_AUTHORIZATION_SERVERS` must be `https`.
- **Required:** no · **Default:** `production` · **Format:** free text, but only
  `development` and `production` are compared against.
- **If unset:** `production`, i.e. the https checks are active.
- **Careful:** any other value (`staging`, `prod`, `Production`) is neither —
  no debug logging *and* no https enforcement. There is no validation of this
  field; a typo silently relaxes the production checks.

## Database

#### `DATABASE_URL`

- **Purpose:** PostgreSQL connection string used by the pgx pool and by the
  migrator at startup.
- **Required:** **yes** · **Default:** none · **Format:** libpq URL, e.g.
  `postgres://skein:qwerty12@postgres:5432/skein?sslmode=disable`.
- **If unset:** startup aborts in `config.Load()` before anything connects.
- **Related:** `MIGRATIONS_PATH` (the SQL applied against this database),
  `DATABASE_MAX_CONNS` (pool size against this server's `max_connections`).

#### `DATABASE_MAX_CONNS`

- **Purpose:** maximum size of the pgx connection pool **per process**. Must be
  sized so that `max_conns × replica_count` (plus deploy surge) stays below the
  Postgres server's `max_connections`.
- **Required:** no · **Default:** `10` · **Format:** integer.
- **If unset:** 10 — a conservative floor chosen for local docker-compose;
  production deployments are expected to set it explicitly.
- **Careful:** a value below `2` is silently raised to `2` in
  `storage.NewPool` (pgx requires `MaxConns >= MinConns`, and `MinConns` is
  fixed at 2). It is deliberately not derived from CPU count — `runtime.NumCPU()`
  reports the node's cores, not the pod's CPU limit, which previously produced
  ~193 connections per pod and exhausted the server (`SQLSTATE 53300`).

#### `MIGRATIONS_PATH`

- **Purpose:** directory of golang-migrate SQL files applied in-process at
  startup, loaded through the `file://` source.
- **Required:** no · **Default:** `./migrations` · **Format:** filesystem path,
  relative to the process working directory.
- **If unset:** `./migrations`, which in the production image resolves to
  `/app/migrations` (the image sets `WORKDIR /app` and copies the SQL there).
- **Note:** override only for tests. The same SQL is also reachable through the
  `migrate` CLI baked into the image (`docker run --entrypoint migrate …`).

## Internal channel and password hashing

#### `SKEIN_INTERNAL_SECRET`

- **Purpose:** shared secret that authenticates skein-web (the BFF) to
  skein-api. Sent as the `X-Skein-Internal-Secret` header on every
  `/api/v1/*` request and checked by `RequireInternalSecret`.
- **Required:** **yes** · **Default:** none · **Format:** opaque string,
  **at least 16 characters**.
- **If unset, or shorter than 16 characters:** startup aborts
  (`skein_internal_secret must be at least 16 characters`).
- **Related:** skein-web reads a variable of the same name and the two values
  must be identical, or every proxied request answers 401.

#### `SKEIN_BCRYPT_COST`

- **Purpose:** bcrypt work factor for password and API-key hashing.
- **Required:** no · **Default:** `12` · **Format:** integer in `[4, 31]`
  (bcrypt's `MinCost`..`MaxCost`).
- **If unset:** 12, the organisation-wide default. Out-of-range values abort
  startup.
- **Careful:** lowering it speeds up test suites and weakens production
  password hashes by the same factor. Production must stay at 12 or above.

## First-admin bootstrap

These four run once, at startup, and only when the `users` table is empty. With
rows present, `auth.Bootstrap` is a no-op regardless of what is set here.

#### `SKEIN_INITIAL_ADMIN_EMAIL`

- **Purpose:** email of the first admin user, created when the `users` table is
  empty. This is the switch for the whole bootstrap block.
- **Required:** no · **Default:** none (feature off) · **Format:** email address.
- **If unset:** no user is created; the operator must provision the first user
  another way (for example through an identity provider). On a fresh database
  with no other provisioning path, nobody can log in.
- **Related:** requires `SKEIN_INITIAL_ADMIN_PASSWORD`; optionally
  `SKEIN_INITIAL_ADMIN_USERNAME`, `SKEIN_INITIAL_ADMIN_NAME`.

#### `SKEIN_INITIAL_ADMIN_PASSWORD`

- **Purpose:** plaintext password for the bootstrap admin; hashed with
  `SKEIN_BCRYPT_COST` before insert.
- **Required:** only when `SKEIN_INITIAL_ADMIN_EMAIL` is set · **Default:** none
  · **Format:** free text.
- **If unset while the email is set:** startup aborts
  (`SKEIN_INITIAL_ADMIN_PASSWORD is required when SKEIN_INITIAL_ADMIN_EMAIL is set`)
  rather than creating a user with no way to authenticate.

#### `SKEIN_INITIAL_ADMIN_USERNAME`

- **Purpose:** username of the bootstrap admin.
- **Required:** no · **Default:** derived from the email's local part
  (`auth.UsernameFromEmail`) · **Format:** username string.
- **If unset:** derived from `SKEIN_INITIAL_ADMIN_EMAIL`.

#### `SKEIN_INITIAL_ADMIN_NAME`

- **Purpose:** display name of the bootstrap admin.
- **Required:** no · **Default:** the resolved username · **Format:** free text.
- **If unset:** falls back to the username.

## Search and embeddings

#### `OLLAMA_URL`

- **Purpose:** base URL of the Ollama server that produces embedding vectors.
  Presence of this variable is what enables the semantic half of search.
- **Required:** no · **Default:** none · **Format:** absolute URL, e.g.
  `http://ollama:11434`.
- **If unset:** the service starts, logs
  `embedding worker disabled: OLLAMA_URL not set`, and search runs
  **full-text only**. Duplicate detection on issue create likewise degrades to
  the trigram layer and reports `degraded: true`. No embeddings are written.
- **Related:** `OLLAMA_MODEL`, `OLLAMA_API_KEY`, `OLLAMA_EMBEDDING_DIM`,
  `EMBEDDING_WORKER_INTERVAL`, `EMBEDDING_BATCH_SIZE`,
  `SEARCH_QUERY_EMBED_CACHE_SIZE` — all inert while this is empty.

#### `OLLAMA_MODEL`

- **Purpose:** name of the embedding model requested from Ollama.
- **Required:** no · **Default:** `qwen3-embedding:0.6b` · **Format:** Ollama
  model tag.
- **If unset:** the 1024-dimension default model is used.
- **Careful:** the database column is `halfvec(1024)`. A model with a different
  output width needs both `OLLAMA_EMBEDDING_DIM` and a schema migration.

#### `OLLAMA_API_KEY`

- **Purpose:** bearer token sent as `Authorization: Bearer <key>` on every
  `/api/embed` request.
- **Required:** no · **Default:** none · **Format:** opaque token.
- **If unset:** requests are sent unauthenticated — correct for an Ollama
  instance with no auth in front of it, and rejected by one that requires a key.

#### `OLLAMA_EMBEDDING_DIM`

- **Purpose:** the vector width the service expects back from the model.
- **Required:** no · **Default:** `1024` · **Format:** positive integer.
- **If unset, or set to zero or a negative number:** silently treated as `1024`
  (`search.NewEmbedder`).
- **Careful:** a mismatch between this value and the model's actual output is
  **not** caught at startup. It surfaces per request, when an embed call returns
  `embed: dim mismatch: expected N, got M` — which means a wrong value here
  looks healthy until the first search or the first background embedding tick.

#### `EMBEDDING_WORKER_INTERVAL`

- **Purpose:** period between ticks of the background embedding backfill worker.
- **Required:** no · **Default:** `5s` · **Format:** duration.
- **If unset:** 5s. Each tick embeds at most `EMBEDDING_BATCH_SIZE` rows, so this
  and the batch size together set the backfill rate.
- **Note:** inert when `OLLAMA_URL` is empty — the worker is not started at all.

#### `EMBEDDING_BATCH_SIZE`

- **Purpose:** number of rows one worker tick pulls and embeds in a single
  transaction.
- **Required:** no · **Default:** `32` · **Format:** positive integer.
- **If unset:** 32. The Ollama batch endpoint is most efficient around 16–64;
  higher values hold database locks longer for diminishing returns.

#### `SEARCH_RESULT_CACHE_TTL`

- **Purpose:** time-to-live of the in-process LRU cache of search result sets.
  Cursor pagination reads from this cache, so it also bounds how long a search
  cursor stays usable.
- **Required:** no · **Default:** `60s` · **Format:** duration.
- **If unset:** 60s.

#### `SEARCH_QUERY_EMBED_CACHE_SIZE`

- **Purpose:** number of query→vector entries kept in the embedder's LRU cache.
- **Required:** no · **Default:** `512` · **Format:** positive integer.
- **If unset:** 512 entries ≈ 2 MiB at 1024 float dimensions.

## Event fan-out

#### `NATS_URL`

- **Purpose:** NATS Core endpoint the outbox dispatcher publishes domain events
  to. skein-web's SignalR hub subscribes to the same subject space, so this is
  what makes the browser update live.
- **Required:** no · **Default:** none · **Format:** `nats://host:port`.
- **If unset:** the dispatcher is not started, a warning
  (`outbox dispatcher disabled: NATS_URL not set`) is logged, and events keep
  accumulating in the `events` table. A later start with the variable set drains
  the backlog. The UI simply stops updating live; nothing is lost.
- **If set but unreachable:** startup continues; the failure is logged at error
  level (`NATS connect failed; outbox dispatcher disabled`) and the theme preview
  path answers `preview_unavailable`. This does **not** abort the process.

## Attachments (S3-compatible storage)

#### `S3_URL`

- **Purpose:** in-cluster endpoint of the S3-compatible store holding
  attachments. Presence of this variable is what enables the attachments
  subsystem.
- **Required:** no · **Default:** none · **Format:** URL including scheme,
  e.g. `http://garage:3900`.
- **If unset:** attachments are disabled — every attachment endpoint answers
  `503 attachments_unavailable`, the cleanup goroutine does not run, and startup
  logs `attachments disabled: S3_URL not set`. When the old `MINIO_URL` is still
  set, a second warning says it is ignored (the `MINIO_*` variables were renamed
  to `S3_*`).
- **If set:** the client is constructed and the bucket ensured at startup; any
  failure there (bad credentials, unreachable host, bucket error) **aborts
  startup with exit code 1**.

#### `S3_ACCESS_KEY`

- **Purpose:** S3 access key id.
- **Required:** only when `S3_URL` is set · **Default:** none · **Format:**
  opaque string.
- **If unset while `S3_URL` is set:** startup aborts
  (`S3 access key or secret key is empty`).

#### `S3_SECRET_KEY`

- **Purpose:** S3 secret access key.
- **Required:** only when `S3_URL` is set · **Default:** none · **Format:**
  opaque string.
- **If unset while `S3_URL` is set:** startup aborts, same error as above.

#### `S3_BUCKET`

- **Purpose:** the single bucket that holds every attachment.
- **Required:** no · **Default:** `skein-attachments` · **Format:** bucket name.
- **If unset:** `skein-attachments`. The bucket is created at startup if it does
  not exist. If the storage does not let the key create buckets (Garage,
  SeaweedFS with a non-admin key), startup aborts with a message asking you to
  create the bucket and give the key read and write access to it.

#### `S3_USE_SSL`

- **Purpose:** whether the S3 client talks HTTPS, for an endpoint written
  **without** a scheme (`host:port`). Applied to both the internal client and the
  client that mints browser-facing presigned URLs. An endpoint with a scheme
  (`http://…`, `https://…` in `S3_URL` or `S3_PUBLIC_URL`) uses TLS or not
  according to that scheme and ignores this variable
  (`storage.normalizeEndpoint`), so the two endpoints may differ.
- **Required:** no · **Default:** `false` · **Format:** boolean.
- **If unset:** `false`, matching the local docker-compose stack. Deployments
  behind TLS must set it to `true`.

#### `S3_PUBLIC_URL`

- **Purpose:** the endpoint **the browser** must use for presigned upload and
  download URLs.
- **Required:** no · **Default:** falls back to `S3_URL` · **Format:** URL
  including scheme.
- **If unset:** presigned URLs are minted against `S3_URL`. That is correct
  wherever the same name resolves inside and outside the cluster, and broken
  wherever it does not — in docker-compose the browser cannot resolve
  `garage:3900`, so the local stack sets the published host port, e.g. `http://localhost:3900`.

#### `SKEIN_ATTACHMENT_MAX_SIZE_BYTES`

- **Purpose:** per-file size cap, enforced in the presigned POST policy and
  re-checked when the upload is confirmed.
- **Required:** no · **Default:** `524288000` (500 MB) · **Format:** integer
  bytes.
- **If unset:** 500 MB.

#### `SKEIN_ATTACHMENT_URL_TTL_SECONDS`

- **Purpose:** lifetime of presigned upload and download URLs.
- **Required:** no · **Default:** `600` (10 minutes) · **Format:** integer
  **seconds** — note this one is a number, not a duration string.
- **If unset:** 600.
- **Related:** must stay well below `SKEIN_ATTACHMENT_PENDING_MAX_AGE`, so a row
  is only swept after its URL has certainly expired.

#### `SKEIN_ATTACHMENT_CLEANUP_INTERVAL`

- **Purpose:** period between cleanup-goroutine ticks that hard-delete abandoned
  `pending` attachment rows.
- **Required:** no · **Default:** `15m` · **Format:** duration.
- **If unset:** 15m. Inert when attachments are disabled.

#### `SKEIN_ATTACHMENT_PENDING_MAX_AGE`

- **Purpose:** age past which a `pending` attachment row is considered abandoned
  and removed.
- **Required:** no · **Default:** `1h` · **Format:** duration.
- **If unset:** 1h. Should exceed `SKEIN_ATTACHMENT_URL_TTL_SECONDS`; this
  relation is documented but not validated in code.

#### `SKEIN_ATTACHMENT_ORPHAN_SCAN_INTERVAL`

- **Purpose:** period of the sweep that walks the whole bucket looking for
  objects whose database row is gone (an in-handler delete that failed).
- **Required:** no · **Default:** `1h` · **Format:** duration.
- **If unset:** 1h. This sweep is far more expensive than the row-side one, hence
  a separate knob.

#### `S3_REGION`

- **Purpose:** the region both S3 clients sign requests with. It must equal the
  region the storage is configured with: Garage `s3_region`, RustFS
  `RUSTFS_REGION`; SeaweedFS does not check it. The clients never discover the
  region, so presigning needs no round trip to the endpoint.
- **Required:** no · **Default:** `us-east-1` · **Format:** region name.
- **If empty:** the default `us-east-1` applies (an empty value is treated as unset).

#### `S3_CORS_ALLOWED_ORIGINS`

- **Purpose:** browser origins the bucket CORS rule is set for at startup. The
  browser uploads straight to the storage (POST) and downloads from it (GET), both
  cross-origin, so a storage that does not reflect `Origin` by itself needs the
  rule. Garage and RustFS do not; SeaweedFS reflects any origin by default.
- **Required:** no · **Default:** none · **Format:** comma separated origins,
  e.g. `https://skein.example.com,http://localhost:8080`.
- **If unset:** the bucket CORS is left alone; the operator configures it.
- **Validation:** each item is trimmed, empty items (a trailing comma) are dropped,
  and an item that is neither `*` nor `http(s)://host[:port]` (no credentials, path,
  query or trailing slash) aborts startup.
- **If set:** the rule allows GET, HEAD, POST and PUT from those origins and
  exposes `ETag` and `Content-Disposition`. The rule **replaces the whole bucket
  CORS configuration** at every start; rules added by hand are lost. Leave the
  variable unset to manage CORS yourself. A storage without a bucket CORS API
  (answers `NotImplemented`) is accepted silently; any other failure is logged as
  a warning (`s3 bucket cors was not set`) and startup continues.


## Background workers

#### `SKEIN_STALE_LOCK_THRESHOLD`

- **Purpose:** minimum age of a claim (`issues.locked_at`) before the reaper may
  auto-release it.
- **Required:** no · **Default:** `30m` · **Format:** duration.
- **If unset:** 30m.
- **Related:** a lock is released only when it is older than this **and** its
  holder has been silent for `SKEIN_STALE_LOCK_AGENT_IDLE`.

#### `SKEIN_STALE_LOCK_AGENT_IDLE`

- **Purpose:** how long the lock holder's MCP session may be silent before the
  reaper treats the agent as gone.
- **Required:** no · **Default:** `30m` · **Format:** duration; `0` disables the
  liveness half of the rule (logged at warn level) and restores age-only
  releasing.
- **If unset:** 30m.
- **Careful:** do not set below roughly `2m`. `agents.last_seen_at` is only
  rewritten when the stored value is older than 60 seconds, so a shorter window
  fights that write throttle and reports busy agents as silent.

#### `SKEIN_STALE_LOCK_INTERVAL`

- **Purpose:** period between stale-lock reaper ticks.
- **Required:** no · **Default:** `1m` · **Format:** duration.
- **If unset:** 1m. The query is a single indexed `UPDATE … RETURNING` that
  matches nothing on a healthy fleet.

#### `SKEIN_REMINDER_INTERVAL`

- **Purpose:** period between ticks of the one-off reminder scheduler.
- **Required:** no · **Default:** `30s` · **Format:** duration.
- **If unset:** 30s — a reminder rings within half a minute of coming due.

#### `SKEIN_REMINDER_BATCH_SIZE`

- **Purpose:** how many due reminders one statement claims and rings.
- **Required:** no · **Default:** `200` · **Format:** positive integer.
- **If unset:** 200. A tick keeps draining in batches until nothing is due, so
  this is a per-statement cap rather than a per-tick ceiling.

#### `SKEIN_NUDGE_INTERVAL`

- **Purpose:** period between ticks of the repeat-reminder ("nudge") worker.
- **Required:** no · **Default:** `1m` · **Format:** duration.
- **If unset:** 1m.
- **Note:** none of the three `SKEIN_NUDGE_*` variables switch the mechanic on or
  off. Whether a person receives nudges is a per-user setting
  (`PUT /api/v1/me/review-prefs`); these only set the pace of the sweep, so an
  empty environment leaves the worker running at factory values, not disabled.

#### `SKEIN_NUDGE_BATCH_SIZE`

- **Purpose:** how many recipients of one category a single selection query
  takes.
- **Required:** no · **Default:** `200` · **Format:** positive integer.
- **If unset:** 200.

#### `SKEIN_NUDGE_BATCHES_PER_TICK`

- **Purpose:** how many consecutive batches one tick processes before yielding.
- **Required:** no · **Default:** `10` · **Format:** positive integer.
- **If unset:** 10. The sweep cursor survives the tick, so whatever does not fit
  in the budget is picked up by the next tick, which resumes where this one stopped.

## Notifications and Watching

#### `WATCHING_FEED_WINDOW_DAYS`

- **Purpose (target semantics, sk-622.21 US-5/US-8):** the Watching window in
  days. The Watching header badge and the Watching column will count and
  show only notifications created within the last N days; mentions have no
  window.
- **Status today:** read and validated only. No read path applies the window
  yet — that lands with the Watching read switch (sk-622.21 T022).
- **Required:** no · **Default:** `21` · **Format:** positive integer (days).
- **If unset:** 21.
- **Validated in `config.Load()`:** must be `> 0`, and must not exceed
  `NOTIFICATIONS_RETENTION_DAYS`; otherwise startup aborts.

#### `NOTIFICATIONS_RETENTION_DAYS`

- **Purpose (target semantics, sk-622.21 US-8):** how many days notifications
  are kept. Older ones will be deleted in batches by a background cleanup;
  mentions are never deleted, and rows still waiting for push delivery are
  left alone. Once the cleanup runs, the MCP / REST notification feed (mentions
  aside) holds no row older than this.
- **Status today:** read and validated only. Nothing deletes by this value yet
  — the cleanup worker lands with sk-622.21 T019.
- **Required:** no · **Default:** `90` · **Format:** positive integer (days).
- **If unset:** 90.
- **Validated in `config.Load()`:** must be `> 0` and `>=
  WATCHING_FEED_WINDOW_DAYS` (equal is allowed) — a shorter retention would
  delete rows the Watching window still shows; otherwise startup aborts.

## Authentication mode and SSO

#### `SKEIN_AUTH_MODE`

- **Purpose:** which identity source authenticates `/api/v1/*`.
  `local` — password login through `/auth/login`.
  `keycloak` — SSO only; `/auth/login` answers `410 Gone`.
  `hybrid` — OIDC for internal users *and* password login for accounts marked
  external, simultaneously.
- **Required:** no · **Default:** `local` · **Format:** exactly one of
  `local`, `keycloak`, `hybrid`.
- **If unset:** `local`. Any other value aborts startup — deliberately, so a
  typo cannot silently drop the deployment into the wrong branch on every login.
- **Related:** in `keycloak` and `hybrid`, `SKEIN_BREAKGLASS_ADMIN` keeps one
  password path open. The API-key channel used by MCP agents works in all three
  modes. JWT validation itself happens in skein-web, not here.

#### `SKEIN_BREAKGLASS_ADMIN`

- **Purpose:** a single admin email allowed to log in with a password even while
  OIDC is active — the escape hatch for a deployment whose SSO is misconfigured
  or which has no external user yet.
- **Required:** no · **Default:** none (mechanism disabled) · **Format:** one
  email address, compared case-insensitively.
- **If unset:** no password path exists in `keycloak` mode.
- **Careful:** this is a permanently open password surface on an internal
  identity. Leave it empty once SSO works and an external user exists.
  Restricting the path at the network level is a separate concern, handled
  outside skein-api.

#### `SKEIN_AUTO_PROVISION`

- **Purpose:** whether the OIDC provisioning endpoint creates a `users` row on
  first SSO login for a subject not yet in the table.
- **Required:** no · **Default:** `false` · **Format:** boolean.
- **If unset:** `false` — unknown subjects are rejected with
  `403 user_not_provisioned` and an admin must seed the user.
- **Related:** consulted on the provisioning path, which is reached in
  `keycloak` and `hybrid` modes; the new user's role comes from
  `SKEIN_OIDC_DEFAULT_ROLE` or, for one address, `SKEIN_BOOTSTRAP_ADMIN_EMAIL`.

#### `SKEIN_OIDC_DEFAULT_ROLE`

- **Purpose:** role given to auto-provisioned users.
- **Required:** no · **Default:** `user` · **Format:** `user` or `admin`.
- **If unset:** `user`. When `SKEIN_AUTH_MODE` is `keycloak` or `hybrid`, an
  invalid value aborts startup; in `local` mode it is not validated because it is
  never consulted.

#### `SKEIN_BOOTSTRAP_ADMIN_EMAIL`

- **Purpose:** the single email elevated to `admin` on its first SSO login.
- **Required:** no · **Default:** none (no elevation) · **Format:** one email
  address, compared case-insensitively against the JWT `email` claim.
- **If unset:** every auto-provisioned user gets `SKEIN_OIDC_DEFAULT_ROLE`.

## Web push

#### `SKEIN_VAPID_PUBLIC`

- **Purpose:** public half of the P-256 VAPID keypair used to sign Web Push
  requests; also handed to the browser (`GET /api/v1/push/config`) as the
  `applicationServerKey`.
- **Required:** no · **Default:** none (push disabled) · **Format:** base64url,
  as produced by `webpush.GenerateVAPIDKeys()`.
- **If unset (together with the private key):** push is disabled, `/push/config`
  reports an empty key, and the frontend hides the enable-notifications control.
- **Careful:** setting exactly one of the two keys aborts startup
  (`skein_vapid_public and skein_vapid_private must both be set or both empty`) —
  a half-configured pair would advertise push to the browser and then fail every
  send.

#### `SKEIN_VAPID_PRIVATE`

- **Purpose:** private half of the VAPID keypair; signs the VAPID JWT on the send
  path. Never leaves the server.
- **Required:** no · **Default:** none · **Format:** base64url.
- **If unset:** see above — both keys empty means push off; one key alone aborts
  startup.

#### `SKEIN_VAPID_SUBJECT`

- **Purpose:** the `sub` contact URI in the VAPID JWT, identifying the operator
  of this application server to the push service.
- **Required:** no · **Default:** `mailto:admin@skein.local` · **Format:** must
  start with `mailto:` or `https:`.
- **If unset:** the placeholder default is used, which is fine in development and
  should be a real contact in production.
- **Careful:** validated **only when push is enabled** (at least one VAPID key
  set); a malformed value then aborts startup.

#### `SKEIN_APP_BASE_URL`

- **Purpose:** public address of the web UI. It is used for two things: the
  `url` field of a web-push payload, so that clicking the notification opens
  the issue; and the links in outgoing email — `<base>/confirm-email/<token>`,
  `<base>/reset-password/<token>`, `<base>/invite/<token>`.
- **Required:** iff `SKEIN_EMAIL_ENABLED=true` (an email needs an absolute
  link) · **Default:** none · **Format:** absolute `http`/`https` URL, e.g.
  `https://skein.example.com`; a path is allowed when the UI is served under
  one (`https://example.com/skein`), a trailing slash is ignored; no query,
  fragment or `user:password@`.
- **If unset:** with mail on, startup aborts naming it next to the missing
  `SKEIN_SMTP_*`; a malformed value with mail on aborts too. With mail off the
  push payload carries a relative path (`/issues/sk-42`), which the service
  worker resolves against its own origin — correct for a single-origin
  deployment. There is deliberately no guessed fallback host, because guessing
  would mis-route clicks.

## Password hygiene

#### `SKEIN_PWNED_PASSWORDS_ENABLED`

- **Purpose:** whether a password change is checked against the Have I Been
  Pwned corpus using the k-anonymity range API.
- **Required:** no · **Default:** `false` · **Format:** boolean.
- **If unset:** disabled — no outbound call is ever made, and
  `GET /me/password/policy` reports `pwnedCheckEnabled=false` so clients do not
  attempt their own lookup either. Off by default because a self-hosted install
  may sit in a closed network, and an unexpected third-party call on every
  password change should be opt-in.

#### `SKEIN_PWNED_PASSWORDS_BASE_URL`

- **Purpose:** base URL of the range-lookup endpoint; `/range/{prefix}` is
  appended.
- **Required:** no · **Default:** `https://api.pwnedpasswords.com` · **Format:**
  absolute URL with **no trailing slash**.
- **If unset:** the public API. Point it at a self-hosted mirror for closed
  networks.

#### `SKEIN_PWNED_PASSWORDS_TIMEOUT`

- **Purpose:** timeout of the outbound lookup, which happens while a user waits
  on a password-change request.
- **Required:** no · **Default:** `2s` · **Format:** duration, strictly greater
  than zero.
- **If set to zero or negative:** startup aborts. Go's HTTP client reads
  `Timeout <= 0` as "no timeout at all", so the value an operator might pick to
  mean "instant" would instead allow an unbounded call. Validated even when the
  feature is disabled, so a value that would misbehave on enable is caught now.
- **Note:** a timeout or network error is treated as "unknown" and does **not**
  block the password change.

## Agent data plane and task monitors

#### `SKEIN_AGENT_PUBLIC_URL`

- **Purpose:** the externally reachable base URL of this service's agent API.
  The `skein_get_monitor_script` MCP tool templates it into the bootstrap command
  handed to an agent, and it is also the `resource` identifier published in the
  OAuth protected-resource metadata document.
- **Required:** **yes** · **Default:** none · **Format:** absolute `http(s)` URL
  with a host, e.g. `https://skein-mcp.example.com`.
- **If unset, relative, or non-http(s):** startup aborts
  (`skein_agent_public_url must be an absolute http(s) URL`). Validating here
  means a misconfigured pod refuses to start instead of minting monitor tokens
  against a dead URL.
- **Careful:** when `ENVIRONMENT=production` the scheme must be `https`, or
  startup aborts. Plain `http` stays legal in development.
- **Related:** `SKEIN_OAUTH_AUTHORIZATION_SERVERS` — when that is set, both are
  validated together as the metadata document's inputs.

#### `SKEIN_MONITOR_CREDENTIAL_CLEANUP_INTERVAL`

- **Purpose:** period of the sweep that deletes monitor credentials past their
  absolute deadline and revoked rows past their retention.
- **Required:** no · **Default:** `10m` · **Format:** duration, strictly greater
  than zero.
- **If set to zero or negative:** startup aborts — a non-positive interval would
  ship a worker that never sweeps.

#### `SKEIN_MONITOR_CREDENTIAL_REVOKED_RETENTION`

- **Purpose:** how long a revoked monitor credential is kept as an audit grace
  window before deletion.
- **Required:** no · **Default:** `24h` · **Format:** duration, zero or greater
  (`0` deletes revoked rows on the next sweep).
- **If negative:** startup aborts.
- **Note:** rows past their absolute deadline are deleted regardless of this
  value.

#### `SKEIN_MONITOR_POLL_SAFETY_LAG`

- **Purpose:** watermark fence on the agent notification poll — only rows at
  least this old (by database clock) are returned.
- **Required:** no · **Default:** `5s` · **Format:** duration, strictly greater
  than zero.
- **If set to zero or negative:** startup aborts. The fence closes a commit-order
  race: without it a row whose transaction is still in flight when a
  later-numbered row commits can be skipped forever by an `id > cursor` cursor.

#### `SKEIN_MONITOR_RUNNER_POLL_INTERVAL`

- **Purpose:** base sleep between polls in the Python runner that
  `GET /agent/v1/monitor-script` serves. Templated into the runner body as whole
  seconds, so the fleet-wide poll rate is tuned without shipping a new runner.
- **Required:** no · **Default:** `2s` · **Format:** duration, strictly greater
  than zero.
- **If set to zero or negative:** startup aborts — it would template a busy loop
  into every agent's runner.
- **Careful:** the value is truncated to whole seconds when templated and
  floored at `1s`: anything below one second runs as `1s`, and a fractional
  part is dropped (`2.5s` runs as `2s`).

#### `SKEIN_MONITOR_TOKEN_IDLE_TTL`

- **Purpose:** the sliding renewal window of a monitor (`skm_`) token. On mint
  and on every successful validation the token's expiry moves to
  `min(now + window, mint + 12h)`: a runner that keeps polling keeps its token,
  and a break in polling shorter than the window (a `Monitor` timeout, a server
  restart, a compaction) does not kill it — the agent re-runs the same
  bootstrap. A break longer than the window ends in `token_expired`. The
  `skein_get_monitor_script` description quotes the configured value.
- **Required:** no · **Default:** `6h` · **Format:** duration, greater than zero
  and at most `12h`.
- **If zero, negative or above `12h`:** startup aborts
  (`skein_monitor_token_idle_ttl must be > 0 and <= 12h0m0s`). A non-positive
  window would expire every token the moment it is minted; one past the 12-hour hard cap could
  never take effect.
- **Note:** the 12-hour hard cap from mint is fixed and not configurable.

#### `SKEIN_MONITOR_STALL_THRESHOLD`

- **Purpose:** how long a live monitor may go without a successful poll
  before it counts as stalled. The value is templated into the served runner,
  which prints one `degraded` line after a run of failures this long. The
  server-side `monitorStalled` hint in MCP tool responses, which also read this
  value, was deliberately unregistered in sk-669: no MCP response carries it.
- **Required:** no · **Default:** `3m` · **Format:** duration.
- **Must be strictly greater than** the larger of: the longest gap between two
  requests of a live runner at the current `SKEIN_MONITOR_RUNNER_POLL_INTERVAL`
  — its backoff ceiling `max(5 × interval, 10s)` plus the 15s limit of one
  request — and the 60s granularity of the agent last-seen stamp. With the
  default `2s` interval the bound is `60s`; at `20s` it is `115s`. Below the
  runner gap a healthy runner in backoff could be counted as stalled; the 60s
  floor keeps the threshold above the resolution of the session activity
  stamp.
- **If at or below that bound:** startup aborts
  (`skein_monitor_stall_threshold must be > …`, naming the bound and the
  interval).
- **Related:** raising `SKEIN_MONITOR_RUNNER_POLL_INTERVAL` raises the bound;
  re-check the threshold when you change the interval.

## MCP transport

#### `MCP_HEARTBEAT_INTERVAL`

- **Purpose:** how often the Streamable-HTTP transport writes a JSON-RPC `ping`
  into a client's standalone GET (SSE) stream, so an intermediary never sees the
  connection as idle.
- **Required:** no · **Default:** `30s` · **Format:** duration; either `0`
  (heartbeats off) or at least `1s`.
- **If unset:** 30s, chosen to sit well under a 900-second gateway idle timeout.
  Note that an *empty* value is not "disabled" — it falls back to the default;
  disabling requires an explicit `0`.
- **If negative, or between zero and one second:** startup aborts. One ticker is
  armed per open GET stream, so a sub-second typo turns every long-lived agent
  session into a hot write loop.
- **Note:** it affects only clients that actually open the GET stream; plain
  POST request/response traffic is unaffected.

#### `SKEIN_OAUTH_AUTHORIZATION_SERVERS`

- **Purpose:** issuer identifiers of the OAuth 2.0 authorization servers that
  mint access tokens for this resource — the `authorization_servers` array of the
  RFC 9728 protected-resource metadata document served at
  `/.well-known/oauth-protected-resource[/mcp]`.
- **Required:** no · **Default:** none (discovery disabled) · **Format:**
  comma-separated absolute `http(s)` issuer URLs with no query, fragment or
  userinfo, e.g. `https://iden.example.com/realms/skein`.
- **If unset:** OAuth discovery is **off** — the two `.well-known` routes are not
  registered and a `/mcp` 401 carries no `WWW-Authenticate` challenge. Everything
  else, including `/mcp` with a static `ska_` API key, works unchanged. Startup
  logs a warning naming this variable so the disabled feature is not invisible.
- **If set but malformed:** startup aborts. When `ENVIRONMENT=production`, every
  entry must additionally be `https`.
- **Related:** not the same knob as skein-web's `Authentication__Oidc__Authority`
  even when the value happens to match: that one is where browser users are sent
  to log in, this one is what machine clients are told the token issuer is.

---

## Installation settings — spaces, registration, mail, operator

Behaviour flags of an installation (SAAS-PLAN 2.2). There is no single "SaaS"
switch: each behaviour has its own variable, and the two deployment presets
("SaaS" and "corporate") set them explicitly. Every default below is what a
corporate installation does today, so upgrading an existing deployment needs no
new variable. An **empty limit or quota means "no limit"** — an unset and an
empty variable are the same here.

**Contradictory settings prevent startup** (SAAS-PLAN 2.5, INV-20, INV-21). The
error names the variables of the offending combination
(`contradictory settings (VAR, VAR, …): …`). Combinations decided by the
variables alone are rejected by `config.Load()`; those that depend on the data
are rejected by `src/internal/config/startup_checks.go` around the schema
migration, before the HTTP listener opens:

| Combination | Checked |
|---|---|
| `SKEIN_INSTANCE_ADMIN_ENABLED=false` and any of `SKEIN_INITIAL_ADMIN_*`, `SKEIN_BOOTSTRAP_ADMIN_EMAIL`, `SKEIN_BREAKGLASS_ADMIN` set | `Load` |
| `SKEIN_INSTANCE_ADMIN_ENABLED=false` and `SKEIN_OIDC_DEFAULT_ROLE=admin` | `Load` |
| `SKEIN_INSTANCE_ADMIN_ENABLED=false` and `SKEIN_SPACE_CREATION=admin` | `Load` |
| `SKEIN_SIGNUP_ENABLED=true` and `SKEIN_EMAIL_ENABLED=false` | `Load` |
| `SKEIN_EMAIL_ENABLED=false`, `SKEIN_INSTANCE_ADMIN_ENABLED=false` and `SKEIN_AUTH_MODE` `local` or `hybrid` | `Load` |
| `SKEIN_SIGNUP_ENABLED=true` and `SKEIN_AUTH_MODE` not `local` | `Load` |
| projects without a space (transition to spaces pending) and `SKEIN_INSTANCE_ADMIN_ENABLED=false`, or no active admin | before migrations — the database is left untouched |
| `SKEIN_INSTANCE_ADMIN_ENABLED=false` and a user with role `admin` exists (deactivated included) | after migrations |
| `SKEIN_INSTANCE_ADMIN_ENABLED=false`, no user at all, and either `SKEIN_SIGNUP_ENABLED=false` with `SKEIN_AUTH_MODE=local`, or `SKEIN_AUTH_MODE=keycloak` with `SKEIN_AUTO_PROVISION=false` | after migrations |
| `SKEIN_COMMON_SPACE_ENABLED=true`, no common space yet, and `SKEIN_INSTANCE_ADMIN_ENABLED=false` — or no active admin and none is certain to appear. An admin appears only through `SKEIN_INITIAL_ADMIN_EMAIL` into an empty user base, or through SSO auto-provisioning (`SKEIN_AUTH_MODE` `keycloak`/`hybrid` with `SKEIN_AUTO_PROVISION=true`) that either creates `SKEIN_BOOTSTRAP_ADMIN_EMAIL` (no account with that email exists yet) or gives every new user `SKEIN_OIDC_DEFAULT_ROLE=admin` | after migrations |

#### `SKEIN_INSTANCE_ADMIN_ENABLED`

- **Purpose:** whether the instance admin role exists, with its "admin sees
  everything" bypass. When `false`, no user may hold role `admin` and no path
  assigns it.
- **Required:** no · **Default:** `true` · **Format:** boolean.
- **If unset:** `true` — today's behaviour.
- **Related:** written at every startup into the `instance_settings` table and
  read back; the database access function reads it from there. See the
  contradictions table above. SaaS: `false`; corporate: `true`. Turning it off
  on an installation that already has admins is not supported (startup refuses
  while any user has role `admin`).

#### `SKEIN_COMMON_SPACE_ENABLED`

- **Purpose:** whether users created by an admin or by SSO join the common space
  automatically, and whether a new installation gets a common space at all.
- **Required:** no · **Default:** `true` · **Format:** boolean.
- **If unset:** `true`.
- **Related:** an installation that moves to spaces with existing projects gets a
  common space whatever this flag says. SaaS: `false`; corporate: `true`.

#### `SKEIN_SPACE_MEMBER_LIMIT`

- **Purpose:** maximum number of people in one space (the common space is
  exempt).
- **Required:** no · **Default:** none (no limit) · **Format:** integer `>= 1`
  (the owner occupies a seat).
- **If unset:** no limit. Other values abort startup. SaaS: a number; corporate:
  empty.

#### `SKEIN_SPACE_OWNED_LIMIT`

- **Purpose:** how many spaces one user may own, not counting the personal space.
- **Required:** no · **Default:** none (no limit) · **Format:** integer `>= 0`.
- **If unset:** no limit. SaaS: a number; corporate: empty.

#### `SKEIN_SPACE_INVITE_TTL`

- **Purpose:** lifetime of a space invitation.
- **Required:** no · **Default:** `168h` · **Format:** duration `> 0`.
- **If unset:** `168h` (7 days). Zero or negative aborts startup.

#### `SKEIN_SPACE_INVITES_PER_DAY`

- **Purpose:** how many invitations one sender (space owner or admin) may send
  in any sliding 24-hour window — spam protection.
- **Required:** no · **Default:** none (no limit) · **Format:** integer `>= 0`.
- **If unset:** no limit. SaaS: a number; corporate: empty.

#### `SKEIN_SPACE_CREATION`

- **Purpose:** who may create new regular spaces (the personal and the common
  space are not created this way).
- **Required:** no · **Default:** `all` · **Format:** `all`, `admin` or `none`.
- **If unset:** `all`. Any other value aborts startup; `admin` with
  `SKEIN_INSTANCE_ADMIN_ENABLED=false` aborts startup too.

#### `SKEIN_PROJECT_DEFAULT_VISIBILITY`

- **Purpose:** visibility of a newly created project: `space` (everyone in the
  space) or `members` (project members only).
- **Required:** no · **Default:** `members` · **Format:** `space` or `members`.
- **If unset:** `members` — today's behaviour. SaaS: `space`.

#### `SKEIN_SIGNUP_ENABLED`

- **Purpose:** whether self-service registration is open.
- **Required:** no · **Default:** `false` · **Format:** boolean.
- **If unset:** `false`. `true` requires `SKEIN_EMAIL_ENABLED=true` and
  `SKEIN_AUTH_MODE=local`. SaaS: `true`; corporate: `false`.

#### `SKEIN_TURNSTILE_SITE_KEY`

- **Purpose:** Cloudflare Turnstile site key — the captcha on registration. The
  key is public and is handed to the browser.
- **Required:** no · **Default:** none (no captcha) · **Format:** string.
- **If unset:** no captcha. Must be set together with `SKEIN_TURNSTILE_SECRET`:
  one without the other aborts startup.

#### `SKEIN_TURNSTILE_SECRET`

- **Purpose:** Cloudflare Turnstile secret used by the server to verify a
  captcha answer.
- **Required:** iff `SKEIN_TURNSTILE_SITE_KEY` is set · **Default:** none ·
  **Format:** string.
- **If unset:** no captcha; set alone, startup aborts.

#### `SKEIN_PROJECT_PREFIX_MIN_LENGTH`

- **Purpose:** minimum length of a project id prefix.
- **Required:** no · **Default:** none (no minimum) · **Format:** integer in
  `[1, 50]` (50 is the longest prefix a project may have).
- **If unset:** no minimum.

#### `SKEIN_EMAIL_ENABLED`

- **Purpose:** whether outgoing mail (email confirmation, password reset,
  invitations) is sent through an SMTP relay.
- **Required:** no · **Default:** `false` · **Format:** boolean.
- **If unset:** `false` — no mail is sent. `true` makes every `SKEIN_SMTP_*`
  below and `SKEIN_APP_BASE_URL` (the address the email links point to)
  required; a missing one aborts startup naming it. With `true` the mail
  worker runs: it delivers queued emails, retrying temporary SMTP failures
  until `SKEIN_EMAIL_DELIVERY_TTL`. With `false` it does not run, and queued
  emails expire at their deadline.

#### `SKEIN_SMTP_HOST`

- **Purpose:** SMTP relay host name.
- **Required:** iff `SKEIN_EMAIL_ENABLED=true` · **Default:** none · **Format:**
  host name or address.

#### `SKEIN_SMTP_PORT`

- **Purpose:** SMTP relay port.
- **Required:** iff `SKEIN_EMAIL_ENABLED=true` · **Default:** none · **Format:**
  integer in `[1, 65535]` — usually `465` with `implicit`, `587` with
  `starttls`.

#### `SKEIN_SMTP_USERNAME`

- **Purpose:** SMTP AUTH user name.
- **Required:** iff `SKEIN_EMAIL_ENABLED=true` · **Default:** none · **Format:**
  string.

#### `SKEIN_SMTP_PASSWORD`

- **Purpose:** SMTP AUTH password.
- **Required:** iff `SKEIN_EMAIL_ENABLED=true` · **Default:** none · **Format:**
  string. A secret — inject from a secret store, never commit.

#### `SKEIN_SMTP_FROM`

- **Purpose:** sender of every outgoing email — the bare address goes into the
  SMTP envelope (`MAIL FROM`), the address with its display name into the
  `From` header.
- **Required:** iff `SKEIN_EMAIL_ENABLED=true` · **Default:** none · **Format:**
  one email address, optionally with a display name
  (`skein@example.com` or `Skein <skein@example.com>`); a value that does not
  parse as one address, or contains a line break, aborts startup.

#### `SKEIN_SMTP_TLS_MODE`

- **Purpose:** how the SMTP connection is encrypted. `implicit` — TLS from the
  first byte (port 465); `starttls` — mandatory STARTTLS before AUTH and DATA.
  There is no plaintext mode.
- **Required:** iff `SKEIN_EMAIL_ENABLED=true` · **Default:** none · **Format:**
  `implicit` or `starttls`.

#### `SKEIN_EMAIL_DELIVERY_TTL`

- **Purpose:** how long a queued email keeps being retried after temporary
  failures before it is abandoned as expired.
- **Required:** no · **Default:** `24h` · **Format:** duration `> 0`.
- **If unset:** `24h`. Zero or negative aborts startup.

#### `SKEIN_OPERATOR_API_ENABLED`

- **Purpose:** whether the internal SaaS operator API, served on its own port,
  is enabled.
- **Required:** no · **Default:** `false` · **Format:** boolean.
- **If unset:** `false`. `true` requires `SKEIN_OPERATOR_API_KEY`. SaaS: `true`;
  corporate: `false`.

#### `SKEIN_OPERATOR_API_PORT`

- **Purpose:** port of the operator API.
- **Required:** no · **Default:** `5101` · **Format:** integer in
  `[1, 65535]`, different from `PORT`.
- **If unset:** `5101`. Validated only while the operator API is enabled.

#### `SKEIN_OPERATOR_API_KEY`

- **Purpose:** the key every operator API request must present, in the
  header `X-Skein-Operator-Key` (contract: `src/internal/operator/contract.go`).
- **Required:** iff `SKEIN_OPERATOR_API_ENABLED=true` · **Default:** none ·
  **Format:** string of at least 16 characters.
- **If unset:** the operator API cannot be enabled. A secret — inject from
  a secret store, never commit.

#### `SKEIN_QUOTA_PERSONAL_PROJECTS`

- **Purpose:** maximum number of projects in one personal space.
- **Required:** no · **Default:** none (no quota) · **Format:** integer `>= 0`.

#### `SKEIN_QUOTA_PERSONAL_STORAGE_BYTES`

- **Purpose:** maximum total size of attachments, in bytes, in one personal space.
- **Required:** no · **Default:** none (no quota) · **Format:** integer `>= 0`.

#### `SKEIN_QUOTA_SPACE_PROJECTS`

- **Purpose:** maximum number of projects in one regular space (the common space
  included).
- **Required:** no · **Default:** none (no quota) · **Format:** integer `>= 0`.

#### `SKEIN_QUOTA_SPACE_STORAGE_BYTES`

- **Purpose:** maximum total size of attachments, in bytes, in one regular space
  (the common space included).
- **Required:** no · **Default:** none (no quota) · **Format:** integer `>= 0`.

#### `SKEIN_TERMS_URL`

- **Purpose:** link to the terms of service next to the consent checkbox on
  registration.
- **Required:** no · **Default:** none · **Format:** absolute `http(s)` URL.
- **If unset:** the consent text is shown without a link. A malformed value
  aborts startup.

#### `SKEIN_PRIVACY_URL`

- **Purpose:** link to the privacy policy next to the consent checkbox.
- **Required:** no · **Default:** none · **Format:** absolute `http(s)` URL.
- **If unset:** the consent text is shown without a link. A malformed value
  aborts startup.

#### `SKEIN_INSTANCE_NAME`

- **Purpose:** name of this installation in monitoring. Every metric on
  `/metrics` carries the label `skein_instance=<value>` and every log record the
  attribute `skein_instance`, so several installations can share one Prometheus
  and one Loki.
- **Required:** no · **Default:** `skein` · **Format:** non-blank string.
- **If unset:** `skein`. A blank value aborts startup. The label is not called
  `instance`: Prometheus sets that one to the scrape target.

---

## Set in deployment but not read by skein-api

- **`REDIS_URL`** — present on the skein-api service in
  `docker-compose.local.yml`, but `config.go` declares no such field, so the
  process ignores it. Redis is used by skein-web only. (Verified by absence from
  the `Config` struct and from any `os.Getenv` call in non-test code.)

## Test-only variables

These are read by `_test.go` files, never by the running service.

#### `TEST_DATABASE_URL`

- **Purpose:** connection string for the throwaway PostgreSQL that
  database-backed tests run against.
- **If unset:** those tests call `t.Skip` instead of failing. That is why a
  green `go test ./...` with no database proves less than it seems — see the
  root `CLAUDE.md` section on testing.

#### `SKEIN_TEST_PARALLEL_PKGS`

- **Purpose:** signal to guards such as `requireSerialPackages` that packages are
  being run in parallel.
- **If set to any non-empty value:** the tests that need a serial database
  (last-admin behaviour and similar) **skip**, losing that coverage. Do not set
  it without a reason. It does not control `go test -p`.

#### `SKEIN_DUMP_ROUTING`

- **Purpose:** when non-empty, the MCP action-routing test prints the routing
  table it derived.
- **If unset:** nothing is printed; the test still runs.

#### `SKEIN_MEASURE_TOOLS`

- **Purpose:** when non-empty, enables the MCP tool-payload measurement test.
- **If unset:** that measurement is skipped.

#### `SKEIN_MEASURE_RESPONSES`

- **Purpose:** when non-empty, enables the MCP response-size measurement test.
- **If unset:** that measurement is skipped.

#### `SKEIN_UPDATE_TOKEN_CATALOGUE`

- **Purpose:** when set to `1`, `TestCatalogueGolden` in `internal/themes`
  rewrites its golden file of the theme token catalogue instead of comparing
  against it. Read the resulting diff before committing it.
- **If unset (or any other value):** the test compares the catalogue with the
  golden file and fails on drift.

#### `SKEIN_UPDATE_RETIRED_ROLES`

- **Purpose:** when set to `1`, `TestRetiredRolesGolden` in `internal/themes`
  rewrites its golden file of retired theme role names instead of comparing
  against it. Read the resulting diff before committing it.
- **If unset (or any other value):** the test compares the list with the
  golden file and fails on drift.
