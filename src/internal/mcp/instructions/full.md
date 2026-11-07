# Skein — agent guide

Skein is the shared task tracker for humans and AI agents, one backend
for every project. Issues have hierarchical ids (`demo-42`,
`demo-42.1`); bare numbers are rejected. Tool descriptions carry the
call-time details and error answers carry their remedy; this guide holds
only what you cannot learn from them.

## Session start and project resolution

`skein_whoami` → print `skein init agent.sessionId: <sessionId>` (the
anchor that joins your chat log to `events.actor_session_id`) →
`skein_get_instructions` → wait for the user; do not auto-fetch tasks.

Before any task tool, resolve the project: read the `SKEIN-PINS` block
in the workspace `AGENTS.md` (lines `- \`<id_prefix or alias>\` ← <git
remote URL>` between `<!-- SKEIN-PINS:BEGIN -->` / `<!-- SKEIN-PINS:END -->`)
and pass the left-hand token as `project=`. No block →
`skein_project_resolve(q=<URL>)` for each git remote of the workspace;
one match → write the block; several or none → ask the user, never
invent. Never rewrite the user's pins (a pin that no longer resolves:
flag it). No `AGENTS.md` → create it. Whenever you write to
`AGENTS.md`, make sure `CLAUDE.md` contains the line `@AGENTS.md`
(create it with that line if absent, append otherwise); touch nothing
else there.

Independently of the pins, if `AGENTS.md` has no `SKEIN-USAGE` block,
append this verbatim:

```markdown
<!-- SKEIN-USAGE:BEGIN -->
Tasks go to Skein (MCP), not TodoWrite. Session start: `skein_whoami` →
`skein_get_instructions`; resolve the project from the `SKEIN-PINS` block
above and pass `project=<pin>` to every task tool. Lifecycle: create →
claim → work → close.
<!-- SKEIN-USAGE:END -->
```

Components: after resolving the project, `skein_component(action="list",
project=<pin>)` and pass the matching `componentIds` to `skein_create`;
ambiguous → ask; project-wide task → none.

## Working an issue

`skein_prime` (also after compaction) → `skein_ready` or its
`myInProgress` → `skein_show <id>` → `skein_claim <id>` → work →
`skein_comment` progress notes when the work spans sessions or hands
over → `skein_close`, or `skein_unclaim(id, nextStatus, reason)` with
`in_progress` (handoff), `open` (give up) or `deferred` (punt). Claim and
unclaim move lock and status in one transaction — never `skein_unclaim`
and then `skein_update(status=…)`. A side-quest is a new issue with the
inline edge `dependencies: [{toId: <parent>, type: "discovered-from"}]`;
do not abandon the parent.

Read the whole trail before substantive work. The trigger is
`commentsTruncated=true` on a show without `commentsSince` — meaning
"fewer entries were listed than the population holds" or the listing
hit the 200-entry index cap (then even `returned == total` is reported
as truncated: the listing may be incomplete); it is the
ONLY trigger, never `issue.commentsCount > commentsReturned` (that
count is unfiltered). Then `commentsLimit: 0` gives the thread index
plus the last 3 comments in full; open the rest with
`skein_comment_get(id)`, or `commentsLimit: N` to reach past the cap.

Review flag: every issue carries a review-required flag — the creator's
private bookkeeping, built-in default is `true`. When the user does not
want to review the result, pass `reviewRequired: false` on create or in
a `skein_update` carrying nothing else.

## Claim discipline

Lifecycle is always create → claim → work → close. **If you create an
issue and do the work yourself, `skein_claim` it before touching the
work.** The default assignee is not a claim: the issue stays `open` with
no lock and nobody can tell it is being worked. Before closing work you
did, check "did I claim it — `in_progress`, my lock?"; if not,
`skein_claim` it, or on an already-closed issue
`skein_update(assignee="<you>")`. An issue left unassigned on purpose
(`unassigned: true`) is not yours to fill. Closing someone else's stale
issue, or one nobody ever worked, is the one legitimate ownerless close
— give it `wont_do` / `duplicate`; an ownerless `done` close answers a
`warning`.

## Close discipline

Finishing the work and closing the issue are two events; nothing closes
it for you. Call `skein_close` (with `expectedVersion`, `reason`,
`closeOutcome`, `commits[]`) right after the deliverable lands — a
commit, or for a research / review task the report attached, written
into the repo or posted as a comment — before reporting success and
before claiming or asking for other work. Whether it is done is your
call, like "ready to commit": acceptance met → close; partial, needs
review or the user asked to hold → comment and leave it `in_progress`
or unclaim. "The user did not tell me to close it" is not a reason. Work
done by subagents you dispatched is your work under your claim: when you
report it finished, close in the same pass.

**Epics.** After closing what looks like the last open child:
`skein_show <epicId>` — `unclosedChildrenCount` 0 answers "all children
closed?" only when `epic.unavailableChildren` is 0; children in
projects you cannot see mean "cannot tell". `epic.eligibleForClose` is
true only when there are children and all are closed, regardless of
close outcome; it is false when any child is unavailable. `countedChildren`
is a progress count, not the close condition. Standing children are
excluded and do not hold an epic. Deferred children
are not closed (a deferred child
with a blocker counts in `deferredChildren`, not `blockedChildren`). If
the epic's own acceptance holds, close it in the same pass with a
`reason` referencing the child work; a child closed as `duplicate` /
`superseded` should point at an issue that is itself closed. "Close the
subtasks" describes the outcome, not a whitelist — unless the user
explicitly reserved the epic for themselves.

## Close outcome

`closeOutcome` ∈ `done` / `wont_do` / `duplicate` / `superseded`,
required on every transition to closed (re-closing without it keeps the
previous one; re-opening clears it). Only `done` counts as progress; the
rest leave the denominator. `duplicate` / `superseded` need
`closeTarget` — the issue this one duplicates / was superseded by; the
`duplicates` / `supersedes` edge is written in the same transaction
(`closeLink`: `created` / `existed`). Edit a closed issue's outcome with
`skein_update(closeOutcome=…)` and no status. Only `skein_close`,
`skein_close_many` and `skein_update` echo the outcome; elsewhere (an
open card, a `skein_update_many` row, `skein_commits_attach`) the key
is simply not carried — read it with `skein_show`, never re-close to
"set" it.

## Duplicate detection

`skein_create` detects duplicates itself — do not search first. On a
match nothing is created and you get `{created:false, duplicates:[...],
decisionRequired:true}`: decide yourself — `force: true` if it is
distinct, or `duplicateOf: "<id>"` to create and link it. Every answer
carries `degraded`: `false` — the check ran fully and an empty
`duplicates` means none found; `true` — part of the check did not run,
so an empty `duplicates` does NOT mean there are none.

## Dependency edge types

Direction is canonical per type, read as `from → to`; on
`skein_create`'s inline `dependencies[]` the new issue is FROM.

| Type | `from → to` |
|---|---|
| `blocks` | `from` is blocked by `to`; `skein_ready` hides `from` until `to` closes |
| `waits-for` | same gating; `gate` is stored, not evaluated |
| `related` | symmetric "see also" |
| `discovered-from` | `from` was found while working on `to` |
| `supersedes` | `from` replaces `to` |
| `duplicates` | `from` duplicates `to` (the original) |
| `parent-child` | `from` is the child of `to` |

Reverse = swap the sides, never an inverted type. Parent-child:
`parentId` on create sets it; `skein_reparent` moves it (not
idempotent); `skein_dep_add(parent-child)` only adds and answers
`parent_already_set`. Edges to issues you cannot see are not in
`blockers[]` / `isBlocked`; they count in `unavailableBlockingLinks`
whatever their status, and `skein_ready` still honours them.

## Standing tasks

`standing` is a status for a perpetual obligation: never claimed or
worked in place, excluded from counts, `skein_ready` and inbox. Work it
by spawning a child per occurrence — `skein_create(parentId=<id>, …)` —
and closing that. Create with `skein_create(standing: true)` or promote
with `skein_update(status=standing)`; the only demote is to `open`.

## Commit refs

`commits[]` items need `hash` (from `git log`, 7..64 lowercase hex —
only the form is checked) and the repo via `repoId` / `repoUrl` unless
the project has one repo. To add commits to a closed issue use
`skein_commits_attach`: a re-close overwrites `reason` and `closed_at`.
A wrong hash is removed with `skein_commits_detach`.

## Reading answers

- **`expectedVersion`** on `skein_update` / `skein_close` is the version
  you just observed. `version_conflict` (`currentVersion` inside) →
  re-read, reconcile, retry. `serialization_retry` is a rolled-back
  write with the version still valid → retry a close / accept
  unchanged; a multi-step update may have landed earlier steps — re-read
  and resend only what is missing.
- **Write tools answer with an acknowledgement**, not the card: id,
  applied state, the new `version`, the stamps that write moved. A key
  missing from an acknowledgement says nothing about the issue;
  `verbose: true` returns the card; `cardUnavailable: true` means the
  write applied but the card could not be read back — `skein_show`, do
  not repeat the write. Comment writes return the row without `body`
  (`bodyOmitted: true`).
- **A missing key is not an empty value.** Where a surface never loads
  something (labels on `skein_ready`, prose on an acknowledgement) the
  key is absent, not `[]` / `""`. A cut value carries a marker —
  `descriptionTruncated`, `valuesTruncated`, the index's per-entry flag
  — only when something really was cut (`commentsTruncated` is the one
  exception: see the index cap above).
  `skein_show` by default: card + `depsCounts` (no edges) + last 3
  comments; `commentsLimit: 0` → index + last 3.
- **Mentions**: `@username` in a comment or in `description` / `notes` /
  `acceptanceCriteria` / `design` notifies (does not subscribe);
  self-mentions excluded, non-members silently dropped.
- **Stale locks** auto-release after 30 min of both the lock untouched
  and your session silent (`issue.unclaimed`, `actorType: "system"`).
- **Batches** commit row by row: a row marked `batch_deadline_exceeded`
  / `request_cancelled`, or a batch that returned nothing, may still
  have written — re-read and resend only rows whose live state is wrong.
- A business failure is JSON with a stable `error` code
  (`{"error":"…"}`); never parse it as success.

## Anti-patterns

- Dependency refs in free text ("depends on demo-2.1", "see also
  sk-42") are invisible to the graph, `skein_ready` and close-time
  warnings — use typed edges (`dependencies[]`, `skein_dep_add`).
- Do not close another agent's issue — a different Skein identity, or
  work you neither claimed nor did. Your own subagents are not another
  agent.
- Do not poll `skein_notifications(action="inbox")` in a tight loop;
  minutes, not seconds.
- Do not preflight `skein_search` before `skein_create`.

## Monitoring a task / pairing with another agent

Identity: the api key is the owning user, `Mcp-Session-Id` is this
agent (rotates per connection; `skein_whoami` echoes it), locks are per
session. A headless agent gets no push; the task monitor streams new
comments and the close of an issue into your context. Use it to watch
or pair on an issue, or after posting a blocking question as a comment.

1. `skein_get_monitor_script(id, subtree=…)` returns `token`,
   `tokenFilePath`, `bootstrap`, `monitorId`. Write the token to
   `tokenFilePath` (0600) with the Write tool — never into a command
   line.
2. Run `bootstrap` under the Claude Code `Monitor` tool with the maximum
   `timeout_ms` (plain `bash` / `python3` without one). It prints
   one JSON object per line: `{"event":"notification","notification":{…}}`
   per item, lifecycle lines (`degraded` / `recovered` — never restart
   for them) and one final stop line `{"event","reason","action"}` —
   follow `action`. `paused` → run the same `bootstrap` again (also
   when the runner ended without a stop line: `timeout_ms` ran out or
   the process was killed); `duplicate` → a runner already serves this
   token file, do nothing; `closed` with `token_expired` (`status: 401`)
   → mint a new monitor; `visibility_lost` (`status: 403`) → you lost
   the project, not the token: do not re-mint in a loop. It is
   live-start (no backlog); it
   ends with `root_closed` when the ROOT task closes;
   `skein_watchers(action="monitor_stop", monitorId)` ends it early.
3. **Self-skip by `actorSessionId`.** Your own events come back down the
   stream. Compare `notification.actorSessionId` with your current
   session id (re-read it after a reconnect): equal → your echo, ignore
   it or you loop. Compare only when the field is present and non-null
   — an absent one (human, REST, system) is never your echo and must be
   acted on. Lifecycle lines carry no `actorSessionId`.
4. On a comment line read `bodyPreview`; if `truncated`,
   `skein_comment_get(commentId)`; reply with `skein_comment`. Both
   agents talk in the issue's comments — one timeline, visible to the
   human.

Auto-subscription: the assignee, except one filled in at create by the
"assign agent-created issues to me" setting; the creator only for a
hand-made or top-level `kind="directed"` issue.

## Text fields — forbidden literals

Prose fields are scanned for tool-call wrapper syntax leaked from your
client's wire format (`validation_error`, `code:
tool_call_syntax_leaked`). Closed, case-sensitive set:

| literal | matches |
|---|---|
| `<function_calls>` | exact |
| `</function_calls>` | exact |
| `<invoke` | bare and `<invoke name="…">` |
| `</invoke>` | exact |
| `<parameter name=` | parameterized tag only |
| `</parameter>` | exact |

A hit usually means the call is malformed (a parameter block closed
with the wrong tag), but legitimate prose can also contain a forbidden
literal. If the prose must quote one, break it up (for example, with a
zero-width space). A tag named after the field (`</description>`) is
not scanned and is harmless on its own.

## Closing a work session

File follow-ups for what you found but did not finish
(`discovered-from` to the parent). Close finished issues; comment
handoff notes on paused ones; unclaim what you are not continuing. Then
`skein_prime` — `myInProgress` must match what you actually hold.
