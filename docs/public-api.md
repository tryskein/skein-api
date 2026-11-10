# Public REST API

The public API is the REST interface of Skein for scripts, CI jobs and other
programs. It authenticates with an API key and offers the operations of the
MCP server as resources: the same checks, events, notifications and error
codes as the matching MCP tool.

This page is the reference for what the OpenAPI document cannot say on its
own: the rights model, the privileged `force` mode, retries, limits and the
compatibility policy. Operations, parameters and response schemas are in the
document itself.

Code anchors below are written `file#Name` (paths under `src/`); the name is a
literal of that file.

## Where it listens

- A separate HTTP server of skein-api, on its own port, serving only paths
  under `/api/v1/` (`internal/publicapi/routes.go#PathPrefix`). The internal
  API of the web UI (`/ui/v1`), `/mcp`, `/agent/v1`, `/metrics` and the health
  probes are not served there.
- On by default; `SKEIN_PUBLIC_API_ENABLED=false` turns it off, and then
  nothing listens on its port. Port: `SKEIN_PUBLIC_API_PORT` (default `5103`).
  The external address clients use is `SKEIN_PUBLIC_API_URL`. All variables are
  in [`env-reference.md`](env-reference.md).
- Who can reach the port is decided by your ingress or firewall, not by
  skein-api: any user of the instance with an API key can call the API.
- No CORS headers are sent; browser requests from other origins are not
  supported.

## The OpenAPI document

`GET /api/v1/openapi.json` (operation `getOpenApi`, no key) returns the
OpenAPI 3.1 document of the instance: every operation, the key right it
requires (the scope of the `SkeinApiKey` security requirement) and the key's
security scheme. With `SKEIN_PUBLIC_API_URL` set, `servers[0].url` is that
address; without it the document has no `servers`
(`internal/publicapi/openapi.go#NewOpenAPIDocument`).

The document is generated from the handler annotations by
`scripts/generate-public-openapi.sh` and committed as
`src/internal/publicapi/openapi/openapi.json`. It is a separate document from
the one of the internal API (swag v1, `/ui/v1`): when you publish API documents
to a registry, publish the two under different names, and publish the public
one only from a merged revision.

## Authentication

Send the key in the header `X-Skein-Api-Key` (`internal/auth/headers.go#HeaderAPIKey`).
A missing, malformed, unknown, revoked or expired key, and a key whose owner
is inactive, all get the same 401 `unauthenticated`. The rights, scope and
expiry of the key and the status of its owner are read again on every
request: a revoked or narrowed key loses its access on its next request.

The public port trusts no identity header. `X-Skein-User-*`,
`X-Skein-Surface` and `Mcp-Session-Id` are ignored there; the caller is always
the owner of the key (`internal/publicapi/auth.go`).

## Rights

### The model

A key has a level and an independent `admin` flag
(`internal/apikeys/grant.go#Grant`):

- level `read` ⊂ `write` ⊂ `manage` — a higher level includes the lower ones;
- `admin` — the user operations; a key may hold `admin` without a level, and
  only with the scope «all» (`internal/apikeys/grant.go#ErrAdminNeedsAll`).

A key never grants more than its owner can do now: what a call may do is the
right of the key ∩ the role of the owner ∩ the scope of the key. A missing key
right is 403 `insufficient_scope`, naming `requiredRight` and `operationId`; it
is checked before any object is looked up, so it never reveals whether the
object exists. A missing role of the owner is 403 with the code of the role
(`forbidden`, `not_admin`, …), whatever the key holds. An object outside the
key's scope behaves as an object of a project the owner cannot see: 404 when
the path names it, absent from every list, search and count.

### The matrix

The right of each operation is the scope of its security requirement in the
OpenAPI document; the table of the routes is
`internal/publicapi/routes.go#ProductRoutes`.

| What | Key right |
|---|---|
| Every read | `read` |
| Writes to issues, comments, dependencies, attachments, and the caller's own state (notifications, watching, reminders) | `write` |
| Project settings, components, project members | `manage` |
| Creating a project | `manage`, and a key scope of spaces or all; a key limited to spaces creates projects only inside them |
| User operations (`listUsers`, `createUser`, `updateUser`, `addSpaceMember`) | `admin` |
| `getMe` | any valid key |
| `getOpenApi` | no key |

Owner roles a few operations need on top of the key right
(`internal/authz/actions.go`):

| Operation | Role of the owner |
|---|---|
| `releaseIssueLock` on a lock held by another user | `force_unlock` (`ActionForceUnlock`) |
| `force=true` on a guarded operation (below) | `force_unlock` |
| `deleteComment` of another user's comment | `delete_any_comment` (`ActionDeleteAnyComment`) |
| `updateComment` | the author of the comment only; anyone else, an owner or an administrator included, gets 403 `forbidden` |
| `getIssueMovePlan` | write roles in both the source and the target project, and both in one space (a target in another space answers as a missing project) |
| User operations | instance administrator |

### The `force=true` mode

Seven operations are guarded by the issue lock
(`internal/publicapi/routes.go#Route`, field `Guarded`): `updateIssue`,
`closeIssue`, `unclaimIssue`, `attachCommits`, `detachCommit`,
`bulkUpdateIssues`, `bulkCloseIssues`. An ordinary call of them needs `write`
and works only for the lock holder. Their privileged mode is the query
parameter `force=true`: one call bypasses another holder's lock, the version
check and the pin. It needs the key right `manage` and the owner's role
`force_unlock` on the issue's project
(`internal/publicapi/routes.go#KeyRequirement`,
`internal/publicapi/routes.go#EffectiveActions`). The right is checked
before the body is read.

- `force` takes `true` or `false`; absent means `false`. Any other value, a
  repeated parameter, or `force` on an operation that is not guarded is 400
  `validation_error`.
- To take over a lock without bypassing the version, use `releaseIssueLock`
  instead: `write`, no role for a lock the owner holds through any of his keys,
  executors or MCP sessions, `force_unlock` for another user's lock. It raises
  the issue version.
- `force` of `createIssue` is a different thing: a body field next to
  `duplicateOf` that skips the duplicate gate. It needs only `write`.
- A batch with `force=true`: an item whose project the owner lacks
  `force_unlock` on is refused with `force_unlock_forbidden` (`internal/errcode/codes.go#ForceUnlockForbidden`);
  the other items run.
- **A batch with `force=true` repeated applies its changes again**: the mode
  bypasses the version check, and batches do not accept `Idempotency-Key`.
  Re-read the issues before repeating such a batch.

### What a key with `admin` can do

A key with `admin` whose owner is an instance administrator creates users
with an initial password (`createUser`). Whoever knows that password can sign
in to the web UI as the new user — as an ordinary user: a key never creates an
administrator. Treat an `admin` key as a credential that can create sign-ins.

Through a key no password of an existing user is changed, the administrator
role is neither given nor taken, and an administrator is not deactivated; that
is done only in the UI. Deactivating a user (`updateUser`) revokes all his API
keys; reactivating him does not bring them back. No operation by key creates,
reissues or revokes a key directly.

### Attachment links outlive the key

`createAttachmentUpload` and `getAttachmentDownloadUrl` answer presigned object
storage URLs. A URL handed out before the key was revoked or its scope
narrowed stays valid until it expires: `SKEIN_ATTACHMENT_URL_TTL_SECONDS`
(default 600 seconds). Keep that lifetime short if a revoked key must lose
access to files at once.

## Executors and locks

A lock taken by a request belongs to its executor: the key itself, or the
executor named by `X-Skein-Worker-Id` within the key
(`internal/publicapi/auth.go#WorkerIDHeader`). The value is 1–64 characters of
`A-Z a-z 0-9 . _ -` (`internal/agents/store.go#RESTHolder`); another value or a
repeated header is 400 `validation_error`. Several processes sharing one key
send different worker ids, so they never hold each other's locks. A second
claim of a held issue is 422 `already_claimed`; a change or a close by a
non-holder is 422 `locked_by_other`. A REST executor and an MCP session are
never the same holder.

## Versions

- `getIssue` and `getComment` answer `ETag` = the version. `updateIssue`,
  `closeIssue`, `reparentIssue` and `updateComment` require `If-Match`: without
  it 428 `precondition_required`, with a stale one 412 `version_conflict`;
  nothing is written. `acceptIssue` checks `If-Match` when it is sent;
  `unclaimIssue` does not check it. A write answers the current `ETag`.
- `ETag` validates the version, not the representation: some fields (labels,
  the review flag, counters) change without a new version, and no 304 is ever
  answered.
- Batches carry the expected version in each item; a conflict is that item's
  `version_conflict` in the 207 answer.

## Repeating a request: `Idempotency-Key`

`createIssue`, `createEpic`, `createComment` and `closeIssue` accept the
optional header `Idempotency-Key` (at most 255 characters) — the routes with
`Idempotent` in `internal/publicapi/routes.go#ProductRoutes`. Any other
operation answers 400 `idempotency_not_supported` to it, batches included.

- **Identity.** A request is the same request when it has the same key, the
  same operation, the same target of the path, the same body (by fingerprint),
  the same `Prefer` and the same `force` mode. The same `Idempotency-Key` with
  any other request is 422 `idempotency_key_reused` and runs nothing.
- **Window.** An outcome is kept for 24 hours
  (`internal/idempotency/store.go#Retention`); after that the key is forgotten.
- **What is kept** (`internal/idempotency/middleware.go#Storable`): successes
  (2xx) and business refusals (422) that are not transient. Not kept — the
  repeat runs again: 400, 401, 403, 404, 409, 412, 428, 429, 5xx, and every
  422 with `retryable: true`.
- **A repeat** is authenticated and checked as a new request first — the key,
  its right and scope, the current role of the owner and the visibility of
  every object of the stored answer. If that fails, the ordinary refusal is
  answered, not the stored one. Then the stored answer is returned without
  re-checking business preconditions (`If-Match`, the duplicate gate); no new
  object, event or notification is created.
- **Still running.** A repeat while the first request runs is 409
  `idempotency_in_progress` (`retryable`); send it again later. The mark lives
  `SKEIN_PUBLIC_API_WRITE_TIMEOUT` plus 30 seconds
  (`internal/idempotency/store.go#MarkerGrace`); the operation itself is cut
  off and rolled back at `SKEIN_PUBLIC_API_WRITE_TIMEOUT`, so once the mark has
  expired the first request has either committed with its outcome or written
  nothing.
- **A crash between commit and answer.** The outcome (status, `id`, version)
  is recorded in the same transaction as the write; the body is stored after
  the commit. If the process failed in between, a repeat answers the same
  status, `id` and version (`ETag`), and the other fields of the resource as
  they are now.

## The duplicate gate

`createIssue` looks for similar issues of the project first. When it finds
some, nothing is created and the answer is 422 `duplicate_suspected` with the
candidates in `duplicates` (`degraded: true` when only the lexical check ran)
(`internal/errcode/codes.go#DuplicateSuspected`). Repeat with `force: true` in
the body to create the issue anyway, or with `duplicateOf` to create it as a
duplicate of a candidate. This refusal is kept by `Idempotency-Key`: the
repeat with `force` or `duplicateOf` is another request, so send it with a
**new** `Idempotency-Key` — under the first one it is 422
`idempotency_key_reused`.
`createEpic` has no duplicate gate.

## Errors

Every error is RFC 9457 `application/problem+json`
(`internal/publicapi/problem.go#ProblemDocument`): `type` (`about:blank`),
`title`, `status`, `code` — the same code MCP answers — and `detail`. Some
codes add members (`requiredRight` and `operationId` of `insufficient_scope`,
`currentVersion` of `version_conflict`, …).

`retryable: true` is present only on a transient code: the same request may
succeed later without any change by the client
(`internal/errcode/codes.go`, codes defined `Transient`): `already_claimed`,
`locked_by_other`, `concurrent_change`, `serialization_retry`,
`dependency_target_not_visible_yet`, `request_cancelled`,
`batch_deadline_exceeded`, `idempotency_in_progress`, `rate_limited`,
`attachments_unavailable`, `preview_unavailable`, `database_unavailable`. Any
other code needs a change of the request or of the state first.

Statuses (`internal/publicapi/problem.go#Status`):

| Status | When |
|---|---|
| 400 | malformed request: body, unknown parameter, field or filter, `limit` out of range (`validation_error`), unsupported `Idempotency-Key` |
| 401 | no key, or a key that does not authenticate (`unauthenticated`) |
| 403 | missing key right (`insufficient_scope`) or owner role |
| 404 | the object named by the path does not exist or is outside the key's scope (`issue_not_found`, `component_not_found`, `notification_not_found`, `watch_filter_not_found`, …); unknown path (`route_not_found`) |
| 405 | the path exists, the method does not; `Allow` lists the methods |
| 409 | `idempotency_in_progress` |
| 412 | stale `If-Match` (`version_conflict`) |
| 413 | body over 1 MiB (`payload_too_large`) |
| 415 | body is not `application/json` (`unsupported_media_type`) |
| 422 | business refusal (`locked_by_other`, `cannot_claim_closed`, `duplicate_suspected`, …); a reference in the body to a missing or out-of-scope object, with the code MCP answers (`parent_not_found`, `issue_not_found`, …); `idempotency_key_reused` |
| 428 | `If-Match` required and missing (`precondition_required`) |
| 429 | a limit is exceeded (`rate_limited`), with `Retry-After` |
| 500 / 503 | internal error (`internal_error`); a dependency unavailable (`database_unavailable`, `attachments_unavailable`) |

Successes: 200, 201 for a creation, 204 with no body for every `DELETE`, 207
for a batch with per-item results (`bulkUpdateIssues`, `bulkCloseIssues`,
`bulkAddDependencies`; a refusal of the whole batch — rights, format, limit —
is an ordinary error with nothing executed).

## Shorter responses

- `fields=a,b` on a read keeps only those fields plus `id`; on a list it
  applies to each item and `nextCursor` and `hasMore` stay. An unknown field,
  or `fields` on a write, is 400.
- `Prefer: return=minimal` on a single write that returns one issue or comment
  with its own version answers only `id` and `version`, with
  `Preference-Applied: return=minimal`. Elsewhere — batches, `claimIssues`,
  operations without a version — it changes nothing and no
  `Preference-Applied` is sent; `createAttachmentUpload` always returns the
  upload URL.
- Lists use a keyset cursor: `items`, `nextCursor`, `hasMore`; `limit` out of
  range is 400, never silently clipped.

## Limits

- **Requests per key:** `SKEIN_PUBLIC_API_RATE_LIMIT_PER_MINUTE` (default 100)
  in a fixed one-minute window (`internal/publicapi/ratelimit.go#LimitWindow`).
  The counter lives in the memory of each replica: **the effective limit is
  this value times the number of replicas.** Over it: 429 `rate_limited` with
  `Retry-After` in whole seconds until the window ends, at least 1
  (`internal/publicapi/ratelimit.go#RetryAfterSeconds`). MCP is not limited by
  it.
- **Failed authentications per client address:**
  `SKEIN_PUBLIC_API_AUTH_FAILURES_PER_MINUTE` (default 20), counted before the
  key is checked, per replica as well. While an address is over it, only keys
  that authenticated within the last 15 minutes
  (`internal/publicapi/warmkeys.go#WarmKeyWindow`) still pass from it; any other
  key, valid or not, gets 429 until the window ends.
- **The client address** (`internal/publicapi/clientip.go#ClientIP`) is the
  direct peer. `X-Forwarded-For` and `X-Real-IP` are believed only from a peer
  in `SKEIN_PUBLIC_API_TRUSTED_PROXIES`; from anyone else they are ignored.
  `X-Forwarded-For` is read right to left, skipping trusted proxies; the first
  untrusted address is the client. An IPv4-mapped IPv6 address
  (`::ffff:a.b.c.d`) is the IPv4 address. The failure limit counts an IPv4
  address whole and an IPv6 address by its /64
  (`internal/publicapi/clientip.go#LimitKey`).
- **Timeouts:** one operation may run `SKEIN_PUBLIC_API_WRITE_TIMEOUT`
  (default `30s`); past it the operation is cancelled and its transaction
  rolled back. Request bodies are limited to 1 MiB
  (`internal/publicapi/router.go#MaxBodyBytes`).

## Origin of writes

A write may send `kind` (`directed` | `autonomous`) with the same meaning as
in MCP. Without it, the issue `source` or the comment `author_kind` is `api`,
which every source-dependent rule treats as `agent_directed`; the web UI shows
it as «API». A write by key is never recorded as `human`, and editing an
object does not change its original origin.

## Monitoring

- `http_requests_total` and the request duration count public requests with
  the label `entry="public"` (`internal/handler/metrics.go#PublicHTTPMetrics`).
- `skein_public_api_auth_failures_total` (by `reason`),
  `skein_public_api_rate_limited_total` (by `limit`) and
  `skein_public_api_scope_refusals_total` (by `required_right`) —
  `internal/publicapi/metrics.go`.
- **The ordinary 4xx answers of the public API — 401, 403, 404, 409, 412, 422,
  428, 429 — are not alerts.** They are how a public API answers its clients.
  Alert on 5xx, and on a sustained surge of one authentication failure reason
  from many addresses (key guessing).

## Compatibility

Within `/api/v1`, these are compatible changes a client must tolerate: new
response fields, new optional parameters, new operations, and new values of
enumerations (`source`, `code`, statuses). Removing a field or an operation,
or changing its meaning, happens only in a new version of the path
(`/api/v2`). The old version is then kept for at least two minor releases
after the new one appears and its responses carry the `Deprecation` and
`Sunset` headers.
