# Changelog

All notable changes to **skein-api** (Go REST + MCP backend).

## [2.1.0] - 2026-11-24

- New passwords need at least 12 characters and are checked against known
  breaches (Have I Been Pwned) by default (sk-977.7.20).
- The owner gets an email when an API key is created or reissued
  (sk-977.7.49).
- Push notifications go only to active accounts; deactivating, deleting an
  account or resetting a password by link removes its push subscriptions
  (sk-984.2).
- Push deliveries can no longer reach private or internal addresses, and
  push endpoints and keys no longer appear in logs (sk-977.7.8,
  sk-984.2.4.2).
- Feeds, notifications and Watching show the current text of an edited or
  deleted comment (sk-977.7.3, sk-1024).
- Stricter input checks: commit refs (`http`/`https` URL, length limits),
  subscription filters and the theme inspector are bounded (sk-977.7.69,
  sk-977.7.73, sk-977.7.10).
- Live updates are no longer lost for issues whose id contains a dot
  (sk-977.7.74).

### Upgrade notes

- Existing shorter passwords keep working until they are changed
  (sk-977.7.20).
- The breach check calls `api.pwnedpasswords.com`; turn it off with
  `SKEIN_PWNED_PASSWORDS_ENABLED=false` or point
  `SKEIN_PWNED_PASSWORDS_BASE_URL` at a mirror (sk-977.7.20).
- Migrations 000091 and 000092 run at startup; both can be rolled back
  (sk-977.7.49, sk-984.2).
- New optional `SKEIN_THEME_INSPECT_*` limits, see `docs/env-reference.md`
  (sk-977.7.10).
- A `socks5://` proxy for push is no longer supported (sk-977.7.8).

## [2.0.0] - 2026-11-22

- Public REST API. skein-api serves a versioned REST API under `/api/v1` on a
  port of its own (`SKEIN_PUBLIC_API_PORT`, default 5103), authenticated by an
  API key and enabled by default. It covers the same operations as the MCP
  tools, with the same checks, events, notifications and error codes. Errors
  are RFC 9457 problem documents; writes take `If-Match` for optimistic
  concurrency and `Idempotency-Key` for creating issues, epics and comments
  and for closing issues; lists page with a cursor; `fields=` and `Prefer`
  shape the response. Requests are rate limited per key and failed
  authentications per client address. The OpenAPI 3.1 document is served at
  `GET /api/v1/openapi.json`; the reference is `docs/public-api.md` (sk-878.3.1,
  sk-973).
- API keys have permissions, a scope and a lifetime. A key has a level
  (`read`, `write` or `manage`) and optionally `admin`, a scope (all projects,
  chosen projects or chosen spaces) and an expiry (default 90 days, at most
  365; `SKEIN_API_KEY_DEFAULT_TTL`, `SKEIN_API_KEY_MAX_TTL`). A key sees only
  what its permissions, its scope and its owner's role all allow, in REST and
  in MCP alike; a call beyond its permissions is refused with
  `insufficient_scope`. A key can be narrowed in place and reissued with a new
  secret. Existing keys keep working: they get `manage`, scope "all", `admin`
  only when their owner is an instance administrator, and no expiry
  (sk-878.3.1).
- Issues and comments written with an API key carry the origin `api`
  (`source`, `authorKind`). The `source`, `origin` and `commentAuthorKinds`
  filters accept it, and the "human-driven" origin group includes it
  (sk-878.3.1).
- The internal API used by skein-web moved to `/ui/v1` on the main port. For
  this release it is still served under `/api/v1` on the main port as well;
  the next release removes that path. The service routes of the internal API
  (sign-in, sign-up, password reset, email confirmation, invitation links,
  single sign-on provisioning, hub access checks, instance configuration)
  require the header `X-Skein-Service-Call: 1` and answer
  `403 service_call_required` without it, so a browser can no longer reach
  them through skein-web (sk-940, sk-998).
- MCP `skein_update` and `skein_update_many` apply a change atomically: fields,
  status, components and labels are written together or not at all. Before,
  a failure in a later step could leave the earlier steps committed. The
  expected version is now checked for a change of components or labels alone
  as well (sk-878.3.1).
- MCP `skein_user` called with an API key no longer changes the password of an
  existing user, grants or removes the administrator role, deactivates or
  reactivates an administrator, or creates an administrator: such calls answer
  `forbidden`. Setting the initial password of a new user is still allowed
  (sk-878.3.1).
- The task monitor of `skein_get_monitor_script` is harder to hijack: the
  answer has a new `prepare` command that creates a private token directory
  (mode 0700) and must run before the token is written; the loader and the
  runner refuse a token file that other users can read, do not follow
  redirects and start only the runner whose SHA-256 was fixed when the monitor
  was issued (sk-977.7.22, sk-977.7.23, sk-977.7.57).
- Access that a user has lost is no longer shown in feeds. Watching and
  notifications leave out events of projects the user can no longer see, and
  marking as read neither touches nor counts them; the data is kept. In
  activity feeds, the inbox and Watching, a link to an issue or project the
  user cannot see is replaced by the marker `<hidden>` or null; the event
  itself stays. War-room rows of such projects in the user's own space drop the
  issue id, prefix and project UUID but keep their counters. The refusal
  `standing_has_open_children` lists only visible children and adds
  `unavailableOpenChildren`; `dependency_cycle` returns `details.path` only
  when every issue on the path is visible. The watch list leaves out projects
  the user can no longer see, and the space filter of search counts only
  visible spaces (sk-1007, sk-977.7.67, sk-977.7.71).
- Deleted comments no longer appear in comment search, an edited comment is
  re-indexed for semantic search, and a mention of a deleted comment carries
  no context (sk-977.7.2, sk-977.7.4).
- A request cancelled by its client answers `408 request_cancelled` instead of
  `500` and is not counted as a server error; a cancellation while the search
  embedding is computed is reported as a cancellation, not as degraded search
  (sk-973.7, sk-973.9).
- A NUL character (U+0000) in a request string is refused with
  `422 validation_error` (`nul_character`) instead of failing with `500`, on
  every transport. Title and search lengths are counted in characters, not
  bytes (sk-973.1, sk-973.2).
- `SKEIN_BCRYPT_COST` must be at least 12; a lower value stops startup. The
  default stays 12 (sk-977.7.52).
- Hardening: the main port limits the time to read a request and closes idle
  connections; a refused internal secret is logged with the transport
  address; NATS credentials never reach logs or connection errors; the CORS
  rule of the attachments bucket allows only what the browser uses; caches of
  scoped results are keyed by a hash of the project set (sk-977.7.25,
  sk-977.7.50, sk-977.7.51, sk-977.7.54, sk-977.7.56, sk-977.7.122).
- Every MCP tool has a title (`annotations.title`). `/mcp` offers only the
  session-based protocol revisions (sk-932.2).
- The image declares a healthcheck on `/healthz` and is built with Go 1.27
  on Alpine 3.24; the build verifies the checksum of golang-migrate
  (sk-976.1, sk-977.7.24).

### Upgrade notes

- **Stop both services, no rolling upgrade, no rollback.** This release and
  skein-web 2.0.0 work only with each other: skein-web 1.0.1 does not send the
  service-call header, and skein-web 2.0.0 needs `/ui/v1`. Suspend GitOps
  reconciliation and automated image updates, stop every replica of
  skein-web and skein-api, and start the new images only when no old pod is
  `Running` or `Terminating`. Take a database backup first. Migrations
  000086–000090 are forward-only: going back to skein-api 1.1.0 means
  restoring that backup. See `DEPLOYMENT-NOTES.md` §4.6 and §4.7 of the Skein
  repository.
- Upgrade skein-frontend to 2.0.0 together with this release: an older
  frontend cannot create API keys (`400 validation_error` with
  `details.staleClient`).
- Check that `SKEIN_BCRYPT_COST`, if set, is 12 or more.
- The public REST API listens on port 5103 by default. Set
  `SKEIN_PUBLIC_API_ENABLED=false` if you do not want it, and route the port
  through your ingress only if keys should work from outside: it accepts the
  key of any active user. When you publish it, set `SKEIN_PUBLIC_API_URL`;
  behind a proxy set `SKEIN_PUBLIC_API_TRUSTED_PROXIES`, otherwise all clients
  share the proxy's address and its limit of failed authentications. Limits
  are counted per replica.
- New and reissued API keys expire by the policy (default 90 days). Existing
  keys have no expiry and keep working.
- Clients of the internal API on the main port move from `/api/v1` to
  `/ui/v1`; this is the last release that serves `/api/v1` there. MCP clients
  should handle `insufficient_scope` and must issue task monitors again with
  `skein_get_monitor_script` and run its `prepare` command.

## [1.1.0] - 2026-11-09

- Search (`GET /api/v1/search`) can now be scoped like the task list: new issue
  filters `assignee`, `createdBy` (`me` keeps the Delegated selection,
  `not-me`), `acceptedAt`, `isBlocked`, `componentId` and `origin` join the
  existing `status`, `priority`, `type` and `label`. Filters are applied before
  ranking and pagination, so a filtered search returns complete pages instead
  of a ranked page that is narrowed afterwards. Filters apply to issue results
  only; comments, projects and components keep their own selection (sk-346).
- Search pagination continues correctly after the cached result set expires or
  another replica serves the next page: a continuation keeps its offset and
  ranking mode, and results with equal relevance and update time keep a stable
  order (sk-194.6.53).
- A write that loses a database serialization race no longer reports a false
  `version_conflict`. When the issue's version has not changed, REST (issue
  update, status transitions, accept, bulk actions) and MCP (`skein_update`,
  `skein_update_many`, `skein_close`, `skein_close_many`, `skein_accept`)
  return `serialization_retry`: the aborted transaction was rolled back. A
  single-step write can be retried unchanged; after a multi-step update, re-read
  the issue first, because earlier steps may already be committed. A real
  version mismatch is still `version_conflict` (sk-648).
- MCP `skein_update` and `skein_update_many` reject non-string values for
  `description`, `design`, `acceptanceCriteria`, `notes`, `externalRef` and
  `sourceContext` (`validation_error`, code `text_field_not_string`) instead of
  silently clearing the field. In a batch only the affected row fails. Null or
  an empty string still clears a field; an omitted field stays unchanged (sk-626).
- MCP `skein_list` and `skein_ready` reject a non-string `assignee` filter
  (`validation_error`, code `assignee_not_string`) instead of ignoring it.
  Omitted, null or blank still means no filter (sk-827).
- Several skein-api replicas starting at once on an empty database no longer
  fail while racing to create the first administrator; the losing replicas
  retry and pick up the administrator that was created (sk-771).
- Dashboard: the scoring response adds `leaderboardUsers`, the per-person
  breakdown for everyone on a leaderboard, including people beyond the first
  50 rows of `matrixUsers` (sk-192).
- The MCP agent guide (`skein_get_instructions`) is much shorter and keeps only
  Skein-specific rules; several tool descriptions (create, close, dependencies,
  attachments, watchers, monitor script) are clarified (sk-908, sk-909).

### Upgrade notes

- No database migrations or configuration changes are required.
- If a search cursor is rejected during rollout or when a filtered search
  reaches another API replica, restart the search with the same query and
  filters. The web UI retries automatically once.
- Custom API clients should handle `serialization_retry`; re-read an issue
  before retrying a multi-step update.

## [1.0.0] - 2026-10-23

- First public release.
