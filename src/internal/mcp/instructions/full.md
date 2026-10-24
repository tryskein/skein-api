# Skein — agent guide

Skein is the team-wide task tracker: one backend for every project,
shared between humans and AI agents. Issues use hierarchical ids
(`demo-42`, `demo-42.1`) so parent/child trees live in the id itself.
Read this once at session start, cache it for the session.

## Canonical workflow

### Mandatory at session start

1. `skein_whoami` — identity + projects + your `agent.sessionId`.
   Cheap probe (one query). Do **not** call `skein_prime` here —
   `prime` returns task lists that are noise before the user has
   given you a directive.
2. **Print exactly this line into chat:**
   `skein init agent.sessionId: <your sessionId>`
   The `skein init` prefix is a grep-able anchor: whatever keeps a
   log of this conversation can locate your session boot on it, and
   then join on the sessionId back to `events.actor_session_id`,
   which Skein stamps on every event you cause. That's how an audit
   trail links what you did in skein to what you said in chat. Even
   with nothing on your side reading that log, the line earns its
   keep: it hands the human the id to quote back at you.
3. `skein_get_instructions` — read this document and cache it for
   the session. Not optional: ALL operational rules live here, not
   in CLAUDE.md. Skipping = wrong tool calls.

After step 3 — **wait for the user**. Do not auto-fetch tasks.

### Per-task loop (when the user asks for work)

When the user asks about workload — phrasing varies ("what's open",
"any tasks", "what can we do", "continue", "look at the tasks",
"work on demo-42", …) — only then:

4. `skein_prime` (optionally `project=<alias>` / `component=<uuid>`)
   — in-progress, ready, blocked, recent activity in one round-trip.
5. `skein_ready` — pick a top-priority unblocked issue (or continue
   one from `prime`'s `myInProgress`).
6. `skein_show <id>` — the issue in context. **What you get by
   default**: the card, dependency **counts** (`depsCounts`) and no
   edges, the child counters, and the **last 3** comments (ASC) with
   `commentsTotal` / `commentsReturned` / `commentsTruncated` beside
   them. Everything heavier is opt-in and named: `depsTypes` for
   edges of the types you name, `commentsLimit: 0` for the thread
   **index**, `includeEvents` for the event feed. For the cheap
   re-read of `version` before a write, pass
   `includeDescription: false` (the `description` key then arrives as
   `null`, not absent) — pair it with `includeComments: false` /
   `includeDeps: false`.
7. **Read the whole trail before substantive work — via the index.**
   This is the obligation that used to say "re-fetch the full
   thread". The route changed and so did the wording; the duty did
   not.

   **Trigger.** On a show where `includeComments=true` and
   **`commentsSince` is unset**, if **`commentsTruncated=true`** —
   which means exactly "fewer entries were listed than the
   population holds", plus the index cap case below. That field is
   the ONLY trigger. Do **not** derive one from raw
   `issue.commentsCount > commentsReturned`: that count is
   unfiltered, so on an intentional `commentsSince` window a
   complete view legitimately sits below it and you would be
   re-reading nothing.

   **What to do.** Ask for `commentsLimit: 0`. You do **not** get the
   whole thread back and you do not need it: you get the **index** —
   one entry per comment (id, author, date, the first 80 characters,
   and a per-entry truncation flag) — **plus the last 3 comments in
   full**. The index enumerates every comment, which is precisely
   the ignorance this obligation exists to remove: after reading it
   you know what was said, by whom and when, and which entries you
   still have to open. Then pull only those: `skein_comment_get(id)`
   for one body, or another `skein_show` with `commentsLimit: N` when
   you want the last N whole.

   **The cap, and what `commentsTruncated` means on it.** The index
   lists at most **200** entries of the population. A listing exactly
   as long as the valve that produced it is reported incomplete —
   `commentsTruncated=true` even when `returned == total` — because
   `total` and the page are read in two statements, and a thread that
   grew between them would otherwise pass a cut listing off as whole.
   That is the safe direction, not a contradiction. Past 200 entries
   the index does not claim to see the whole thread, and the only way
   further back is `commentsLimit: N` with a larger N.

   Pairing on each monitor line does **not** require any of this (see
   monitor pairing below).
8. `skein_claim <id>` — declare you're working it. This is
   atomic over lock AND status — open/deferred flip to in_progress
   in the same tx, so every status-filtered surface (skein_ready,
   skein_prime.myInProgress, header badges) immediately reflects
   the new state. Other agents back off when they see `locked_by`.
9. Do the work. Side-quest discovered? Use the inline-deps shape —
   `skein_create({..., dependencies: [{toId: <parent>, type:
   "discovered-from"}]})` — instead of create-then-dep_add, so the
   edge lands in the same tx as the new issue. Do not abandon the
   parent.
10. `skein_comment <id>` for progress notes when work is
   multi-session or someone else needs a handoff.
11. Finish path branches:
    - Done → `skein_close <id>` with `expectedVersion`, `reason`,
      `closeOutcome: "done"` and `commits[]` (the git hashes that
      closed it). Do this the
      moment the work is done — commit/tag/deploy — not "later" and
      not only "if the user asks." See "Close discipline" below.
    - Stepping away without closing → `skein_unclaim(id,
      nextStatus, reason)`. Pick `nextStatus="in_progress"` to
      hand off mid-work, `"open"` to give up cleanly (back to the
      ready pool), `"deferred"` to punt. This is also
      atomic — lock release AND status transition in one tx, so no
      "in_progress with no holder" zombies.

When in doubt, re-run `skein_prime` — costs almost nothing and
re-establishes context after compaction.

## ⚠️ Claim discipline — the #1 rule (ownerless-on-close)

The lifecycle is **always** `create → claim → work → close`. The
`claim` step is the one agents skip, and skipping it is the single
most common Skein mistake — so it gets its own rule.

**If you create an issue and then do the work yourself, you MUST
`skein_claim` it before touching the work — not after, not "later".**
`skein_claim` takes the lock, sets `assignee = you` and flips status
to in_progress atomically. An assignee alone is not that: an issue
your agent creates without `assignee` is usually assigned to you
already (see "Assignee on create"), yet it stays `open` with no lock,
and nobody can tell it is being worked. Without a claim an issue
that got no assignee lives and dies ownerless: in no one's inbox,
and no teammate's agent sees it handled.

**Checkable rule, not a slogan** — bind it to a concrete moment:

- Right after `skein_create`, if *you* are the one doing the work:
  call `skein_claim <id>` immediately.
- Right before `skein_close` of work you did: ask "did I claim it —
  `in_progress`, my lock?" An assignee is no answer (the default may
  have set it). If **no**, that's your bug — you forgot to claim.
  Fix it: `skein_claim` (if still open) or, on an
  already-closed issue, `skein_update(id, expectedVersion,
  assignee="<you>")` — reopen is NOT needed, update works on
  closed issues. An issue left unassigned on purpose
  (`unassigned: true`, the user's wish) is not yours to fill.

The one legitimate ownerless close: you're closing someone else's
stale issue, or one nobody ever worked — give those a
`closeOutcome` of `wont_do` / `duplicate` (see "Close outcome"
below) and the rule is satisfied, because it is about work *you*
did showing up unowned.

The backend backs this up: `skein_close` on an ownerless issue
returns a non-blocking `warning` reminding you of exactly this —
and it is NOT raised when the outcome is anything but `done`,
because "closed with no assignee" is not a defect for work nobody
was ever meant to do.

## ⚠️ Close discipline — the #2 rule (done-but-still-open)

Symmetric to ownerless-on-close: agents finish the actual work —
commit lands, tag is pushed, deploy goes out — then move straight to
the next task without ever calling `skein_close`. The issue sits
`in_progress` forever even though the work shipped. Just as damaging
as forgetting to claim: `skein_prime.myInProgress` fills up with
tasks that are actually done, teammates can't tell what's really
outstanding, and `commits[]` never gets attached to the issue.

**Finishing the work is not the same event as closing the issue.**
Nothing does that for you automatically — you must call
`skein_close` yourself, before touching anything else.

**Checkable rule** — bind it to concrete moments where work visibly
completes:

- **Right after the deliverable the issue actually asked for lands**
  — before reporting success to the user, before picking up the
  next task — call `skein_close <id>`. The deliverable isn't always
  a commit: for a code task it's commit/tag/push/deploy
  (`commits[]`); for a research/review/investigation task it's the
  report — attached via `skein_attachments(action="upload_url")` +
  `skein_attachments(action="confirm")`, a file written into the repo (e.g.
  `docs/research/...`), or a findings comment posted with
  `skein_comment`. Whichever form the acceptance criteria call for,
  once it's actually produced and recorded on the issue, close —
  don't defer it to "after this next thing."
- **Right before `skein_claim`-ing a different issue, or calling
  `skein_prime`/`skein_ready` to pick up new work**: ask "is the
  issue I'm currently holding actually finished?" If yes and it's
  still open/in_progress — close it now, before claiming anything
  else.
- **Right before telling the user the task succeeded**: same check
  — never report success while its issue is still open.

**Deciding whether an issue is done is your call to make** — the
same way deciding whether code is ready to commit is your call, not
a separate status decision you defer to the user "to be safe." If
you can point to what the acceptance criteria asked for and say
it's satisfied, that IS the determination; act on it, don't treat
it as a pending question.

**Telling "done" from "not yet"**: acceptance criteria met (or, if
none, the requested change is implemented and — where relevant —
committed/tagged/deployed) → close. Partial fix, needs review, user
asked to hold → don't force it: `skein_comment` a status note and
either leave `in_progress` or `skein_unclaim` with the right
`nextStatus`.

**"The user didn't tell me to close it" is not a reason to leave it
open.** Closing on completion is the default, same as claiming on
start — neither waits for a separate go-ahead. This holds even when
the work was done by subagents you dispatched yourself (research,
review, implementation): once you've absorbed their output and
you're reporting it as finished, that IS the completion moment —
close then, in the same pass, not after the user separately asks.

### Closing an epic once all its children are closed

Don't strand the parent. When the last open child of an epic gets
closed and the epic's acceptance criteria are satisfied by what the
children delivered — close the epic too, in the same pass. Leaving
it open "for confirmation" after everyone's own acceptance is
demonstrably met just creates a ghost epic that looks unfinished
forever.

**Read enumerated instructions as a goal, not a whitelist.** If the
user said "close the subtasks" (or similar), that describes the
outcome they want — the branch of work finished — not a hard list of
the only issues you're allowed to touch. If closing the parent is the
obvious next step to reach that outcome, take it (or close-and-tell-
them), don't turn it into a separate pending question.

**Checkable rule** — right after closing what looks like the last
remaining open child of an epic:

- `skein_show <epicId>` — read `unclosedChildrenCount` on the card:
  0 IS the answer to "are ALL children now closed?", so do not count
  the list by eye. It counts the children you can SEE, so a non-zero
  `depsCounts.down` `unavailable` bucket beside it means the honest
  answer is "cannot tell", not "all closed". The epic rollup on the
  same card follows the identical rule: `epic.unavailableChildren`
  counts children in projects you cannot see, they are in NO status
  bucket, and `epic.eligibleForClose` is false whenever that count is
  non-zero. Two further buckets on the same rollup:
  `epic.deferredChildren` counts children parked as `deferred` — they
  are NOT closed, so they keep the epic unfinished; and
  `epic.blockedChildren` counts ONLY children that are themselves
  `open` or `in_progress` AND carry an active blocker. A deferred child
  with a blocker is therefore counted once, in `deferredChildren`, and
  never in `blockedChildren`: read the blocked bucket as a breakdown of
  the not-done children (it can never exceed open + in_progress), never
  as a standalone total. Then: does the epic's own `acceptanceCriteria`
  hold given what the children delivered?
- Yes to both → `skein_close <epicId>` in the same pass, with
  `closeOutcome: "done"` and a `reason` that references the child work (e.g. "all N subtasks
  closed, see child reports").
- Genuinely unsure (acceptance not fully covered, needs human
  sign-off, or some children carry a `closeOutcome` other than
  `done` — read the field, don't guess from the reason text) → say
  so explicitly and ask. Children closed as `duplicate` /
  `superseded` deserve one more look: check that the issue each of
  them points at is itself closed, or the work only moved.

**Exception**: if the user's instruction was clearly scoped to only
the children ("close acm-1.1..1.9, I'll review the epic myself"),
respect that scope — but say so out loud, don't leave it ambiguous
whether you considered the epic at all.

## Close outcome — how the issue ENDED

`closeOutcome` says how a closed issue ENDED — one of `done`,
`wont_do`, `duplicate`, `superseded`. Re-opening clears it; an outcome
spelled out in `reason` changes no count.

- **closeOutcome is required when transitioning to closed; repeating a
  close without it preserves the previous outcome** (re-closing to
  attach commits never erases it). On all four close paths
  (`skein_close`, `skein_close_many`, `skein_update`,
  `skein_update_many`), from any non-closed status, standing
  included, `force` or not. Else `validation_error`, `code: close_outcome_required`:
  the update tools refuse before any write, `skein_close_many` fails
  only that row. Example: `skein_close(id, expectedVersion, reason:
  "shipped in 1.4.0", closeOutcome: "done")`.
- Web UI / REST: a transition without one gets `done`; a
  bulk `archive` of an unclosed issue needs one (else the batch is
  refused); a closed issue keeps its outcome, a standing issue gets
  `done`.
- **Only `done` counts as work done**; the rest are excluded from
  scope — they leave the progress denominator (3 `done` + 1 `wont_do`
  = 3/3).
- **`closeTarget`** — the issue this one duplicates / was superseded
  by; the `duplicates` / `supersedes` edge is created in the same
  transaction (`closeLink`: `created` or `existed`); `duplicates`
  goes from this issue to the target, `supersedes` from the target to
  this issue. It may point into any visible project, never at the
  issue itself (`close_target_self`), and requires an EXPLICIT outcome
  of those two — never `done`/`wont_do`. `duplicate` / `superseded` require
  it (`code: close_target_required`) unless the issue is ALREADY
  closed and keeps the outcome or has the edge — so `skein_update_many` (no per-row
  target) cannot close an open issue that way, and `skein_move`
  answering `closeLink: "existed"` is expected.
- **Edit a closed issue's outcome** with no status:
  `skein_update(id, expectedVersion, closeOutcome: "…")` — status,
  date and reason untouched, author notified; sent with another
  status it is refused.
- `skein_close`, `skein_close_many` and `skein_update` echo
  `closeOutcome`. `skein_update_many` does NOT echo it (row
  `{id, ok, version}`) — use `skein_show`.
- **A server from before this field omits the key entirely** — read
  an ABSENT `closeOutcome` as "older instance", never as an outcome.

## Project resolution (before any task command)

Before any task-touching tool (`skein_create`, `skein_prime`,
`skein_ready`, `skein_search`, `skein_show`, `skein_close`, …) you
must know **which Skein project the user means**. Resolve in this
order:

1. **Read the `SKEIN-PINS` block in `CLAUDE.md`** (the project's
   own CLAUDE.md, at the workspace root). Canonical format:

   ```markdown
   <!-- SKEIN-PINS:BEGIN -->
   ## Skein project pins
   - `<id_prefix or alias>` ← <git remote URL>
   <!-- SKEIN-PINS:END -->
   ```

   Verify each pin via `skein_project_resolve(q=<URL>)`. If a pin's
   URL no longer resolves to a project (project archived / renamed)
   — **flag to the user, do NOT silently rewrite the block**.

2. **If the block is absent — bootstrap once.** Scan the workspace
   for git remotes (`/workspaces/*/.git/config` and similar
   per-repo paths), for each URL call
   `skein_project_resolve(q=<URL>)`, collect resolved projects:
   - 1 unambiguous match → write the block with that pin **before
     proceeding to ANY task tool**. Bootstrap is part of session
     setup, not optional cleanup. Skipping it makes every future
     session repeat the discovery scan from scratch.
   - More than 1 → present the list to the user, ask which to pin
     (or pin several if the user has multiple projects in this
     workspace), then write the block.
   - 0 matches → ask the user "in which project should this go —
     or shall I create a new project?". Do NOT silently invent.

   (For the second mandatory block — `SKEIN-USAGE` — see the next
   subsection. It is checked **independently** of `SKEIN-PINS` and
   written whenever it's missing, even on a workspace where the pins
   are already present.)

3. **Mid-session "add another project"** — only on the user's
   explicit request (e.g. "add `demo-api` to the pins too"). Do not
   auto-grow the block beyond the bootstrap entry.

4. **Bootstrap finds N URLs but only K < N are registered.** When
   the workspace scan turns up several git remotes that all resolve
   to the same project P, but P.gitRepos[] only lists K of them,
   call `skein_project_update(project=P, gitReposAdd=[<missing
   URLs>])` to extend the registry — **admin or project owner
   only**. If the agent lacks that permission, ask the user to add
   the URLs (or to grant ownership) rather than silently leaving
   them unresolved.

Once the block exists, treat it as the source of truth for the
session. The user controls it; you only verify on read. The pin's
left-hand token (id_prefix or alias) is what you pass as
`project=<…>` to subsequent tools.

## SKEIN-USAGE block in CLAUDE.md (write whenever missing)

Independent of `SKEIN-PINS`, check `CLAUDE.md` at the workspace root
for a `SKEIN-USAGE` block. **If missing — append it verbatim**, even
when `SKEIN-PINS` is already present. Old workspaces from before this
guide existed will have pins but no usage block; without the usage
block, a fresh agent has no signal in `CLAUDE.md` that this workspace
is Skein-managed and defaults back to its built-in TodoWrite system —
the shared-pool premise breaks silently.

Canonical text (write once, then leave alone — the user controls
subsequent edits):

```markdown
<!-- SKEIN-USAGE:BEGIN -->
## Task tracking — use Skein, not TodoWrite

> ⚠️ **CRITICAL — CLAIM WHAT YOU WORK ON.** If you created an issue
> and you start working it yourself, **`skein_claim` it FIRST** —
> before any work, not "later". Created without `assignee`, it is
> usually assigned to you by your personal setting, but that is not
> a claim: no lock, still `open`. An issue that ends up with **NO
> assignee (ownerless)** is in nobody's inbox, and no teammate can
> tell it was handled. No default under someone else's (or a hidden)
> ancestor. Lifecycle is always **create → claim → work → close**.
> Before you close your work, check: did you claim it (`in_progress`,
> your lock)? If not — claim it. Never fill an assignee left empty
> on purpose.

This workspace uses **Skein** (MCP-connected task tracker) as the
team-wide shared task pool. Do NOT use the built-in TodoWrite for
multi-step user-visible work — TodoWrite is per-session and invisible
to teammates / their agents. Skein is shared and survives sessions.

At session start (before any task command):

1. `skein_whoami` — identity + sessionId + visible projects.
2. Print one line into chat: `skein init agent.sessionId: <sessionId>`.
3. `skein_get_instructions` — pull the full guide and cache it.
4. Resolve project from the `SKEIN-PINS` block above; pass
   `project=<pin>` to every task tool.

Typical flow:

- User asks for non-trivial work → `skein_create`, then
  **`skein_claim` immediately if you're the one doing it** (so it
  isn't closed ownerless — see the CRITICAL note above). User says
  "leave it unassigned" → `skein_create(..., unassigned: true)`.
- Discover a side-quest → `skein_create` with
  `dependencies: [{toId: <parent>, type: "discovered-from"}]`.
- Done → `skein_close` with `reason`, `closeOutcome` and `commits[]`.
- Stepping away → `skein_comment` with handoff notes;
  `skein_unclaim(id, nextStatus, reason)` to release the lock. Pick
  `nextStatus="in_progress"` (handoff — next agent claims),
  `"open"` (give up — back to the pool), or `"deferred"` (punt).

Trivial one-shot edits inside a single response need no Skein issue.
Anything the user might later ask about, anything a teammate's agent
might pick up, anything spanning more than one chat turn — goes
through Skein.
<!-- SKEIN-USAGE:END -->
```

Write the block verbatim — do not paraphrase. Check independently of
`SKEIN-PINS` so legacy workspaces self-heal.

## Component resolution (before skein_create)

Skein projects have **components** (modules / microservices /
sub-areas inside a project). For example project `skein` contains
components `skein-api`, `skein-web`, `skein-frontend`. Tasks should
be tagged with the right component so search and reports work — a
"web UI" task pinned to `skein-frontend` shows up in
`/projects/skein/components/skein-frontend/issues`, untagged it
floats in the project bucket.

After the project is resolved, **always** call
`skein_component(action="list", project=<pin>)` and try to match the
task to a component:

1. **Match the user's intent** against component names and
   descriptions. Hints to use:
   - workspace path (`/workspaces/skein-frontend/...` → component
     `skein-frontend`)
   - technology / framework named in the task ("React form" →
     `skein-frontend`; ".NET handler" → `skein-web`; "Go API" →
     `skein-api`)
   - explicit naming ("in skein-api do …")
2. **Exact / unambiguous match** → pass `componentIds: [<uuid>]`
   to `skein_create`.
3. **Multiple plausible matches** → ask the user which component.
4. **No match AND the task hints at a new module / microservice**
   ("make a new service for X", "create a component Y") → ask the
   user "I don't see a component for X — create it via
   `skein_component(action="create")`?". If yes, create it then use
   the new `component_id`.
5. **No match AND the task is project-wide** ("update the README",
   "rebuild docs") → omit `componentIds`. Not every task belongs
   to a component, and that's valid.

This is the same pattern as project resolution: gather, match,
ambiguous → ask, missing → ask before inventing.

**An empty `items` is two different states — `archivedCount` tells
them apart.** The listing defaults to `archivedFilter: "false"`, so
archived components are held back silently. Beside `items` the answer
therefore carries **`archivedCount`** — how many of the project's
components are archived, always present, on a non-empty listing too:

- `items: []`, `archivedCount: 0` → the project genuinely has no
  components. Branch 5 applies: omit `componentIds`.
- `items: []`, `archivedCount: 3` → it HAS components and every one is
  archived. Not a project-wide task: ask whether to unarchive one
  (`skein_component(action="archive", id=…, archived=false)`), listing
  them with `archivedFilter: "true"` to name them in the question.
- non-empty `items`, `archivedCount > 0` → you are matching the active
  ones only. If none fits and an archived one plainly would, ask rather
  than taking branch 4 — reviving one beats a duplicate of it.

The count describes the project's component set, not the current
filter's leftovers, so it reads the same under every filter value.

## Moving an issue between projects

Skein has **no in-place project change** for issues. The
hierarchical id (`demo-24`) is the primary key and is embedded in
git commit messages, chat history, messages between agents, and
other external refs we don't control — renumbering would break them,
and silently swapping `project_id` while keeping the id creates
visual incongruity in the target's listing. The canonical move
is therefore **copy-to-target + `supersedes` edge + close source
with reason**.

When the user asks to "move" or "transfer" a task
to a different project: call **`skein_move(issueId,
targetProject)`**. It does NOT mutate state — it returns a
pre-filled kit:

- `source` — the source card MINUS every field `nextSteps[0].args`
  already carries (title, description, type, priority, and, when
  set, assignee, labels, design, acceptanceCriteria, notes), so
  each of the source's texts is in the response exactly once. Need
  the whole card? `skein_show` on the same id
- `targetComponents` — picker list with naive name-match hints
  against the source title/description
- `nextSteps[0]` — ready-to-call `skein_create` with the source's
  title / description / type / priority / assignee / labels
  carried over (an unassigned source: `unassigned: true`, so the
  copy stays unassigned), a footer documenting provenance, and a
  `supersedes` dependency edge on the source
- `nextSteps[1]` — ready-to-call `skein_close` with
  `expectedVersion` captured from the source and a `reason`
  template containing a `<NEW-ID-FROM-skein_create-RESPONSE>`
  placeholder

Pick a componentId from `targetComponents` (or leave the array
empty for a project-wide task), execute `skein_create`, copy the
new issue id into the close-step `reason`, execute `skein_close`.
Two real tool calls. Never invent your own move workflow without
the supersedes edge — it's the audit trail that links the old id
to the new one for everyone (and every agent) who follows.

## Anti-patterns

- **Do NOT preflight `skein_search` or `skein_prime` before
  `skein_create` to "check for duplicates".** `skein_create` runs
  duplicate detection natively — see "Duplicate detection" below for
  how, and what to do with the result.

  `skein_search` is for: finding existing issues to read / link /
  claim, not for pre-flight dup-checking before create.

- **Do NOT poll `skein_notifications(action="inbox")` in a tight
  loop.** The web UI's SignalR feed pushes updates in real time; for
  an MCP-only agent, poll at human-scale intervals (minutes, not
  seconds).

- **Do NOT auto-close another agent's issue.** "Another agent" means
  a different Skein identity — a different user's session, or an
  issue you neither claimed nor did the work on. It does **not**
  mean subagents you personally dispatched (via your own harness's
  Agent/Task tooling) to research, review, or implement — that work
  happened under YOUR claim, YOUR session. It's your own work by
  another name, and it closes exactly like solo work: no extra
  permission needed (see "Close discipline" above). Only require
  explicit user confirmation when you did not do or supervise the
  work yourself.

- **Do NOT write dependency refs in free-text.** Phrases like
  "depends on demo-2.1", "blocks billing-9", "see also sk-42",
  "requires X" inside `description` / `acceptance` / `notes` /
  `design` are **invisible** to the dependency graph,
  `skein_ready` gating, the UI Dependencies section, and the
  close-time blocker warnings. Typed edges are the **only** signal
  those surfaces consume.

  ✓ Atomic create + edges, instead of naming them in the prose:
  ```
  skein_create({
    title: "Refactor auth middleware",
    description: "Token rotation has to land first; SSO migration
                  depends on this.",
    dependencies: [
      { toId: "demo-2.1", type: "blocks" },
      { toId: "demo-2.5", type: "blocks" },
      { toId: "demo-1.7", type: "related" }
    ]
  })
  ```

  For an EXISTING issue (post-create), use `skein_dep_add` or
  `skein_dep_add_many`. Edges are always the right shape; free-text
  is never an alternative. To **remove** an edge (added by mistake,
  or no longer true), use `skein_dep_remove(fromId, toId, type)`.

## Duplicate detection

`skein_create` checks for duplicates itself, in two layers: **pg_trgm**
similarity on the **title alone** (typos, near-identical wording) plus a
**semantic vector rerank** (qwen3 embeddings) for the synonyms and
rephrasings the trigram layer misses. The two sides of that vector
comparison are built from different fields, which is what decides
whether an edit of yours can affect the outcome at all:

- the query side — the issue you are about to create — is embedded from
  `title` + `description` only;
- a stored candidate is embedded from `title` + `description` +
  `acceptanceCriteria`.

`design` and `notes` are kept out of embeddings entirely (scratch
content pollutes the vector), so nothing you write there ever changes
duplicate detection.

Reported match threshold 0.80 (`duplicateMinReport`); a strong trigram
hit (0.85) short-circuits the vector layer, but only if both titles
discarded the same characters. It is also skipped when the embedder is
unavailable — detection degrades rather than failing your create.

Not silently: every response carries `degraded` (as in `skein_search`) —
true means the semantic layer was unreachable, so the verdict, an empty
`duplicates` included, rests on trigram alone or on nothing. Read
`duplicates:[]` with `degraded:true` as "nothing objected", not
"checked, clean". Branch on it: the reason is logged and returned by
REST, never by `warnings[]`.

Layer 1 answers only for titles it can read: pg_trgm drops every
character the **database's** ctype calls non-alphanumeric (a `ctype=C`
cluster drops all Cyrillic), and a scrap of a title — 1–2 chars, mostly
punctuation, or non-ASCII on a blind ctype — is skipped, not scored low,
so meaning decides.

Rows in `duplicates[]` always carry the same four keys, so provenance is
never guessed from which key exists: `similarity` (headline, max of what
ran), `trigramSimilarity`, `semanticSimilarity` (`null` = that layer did
not score this row), `matchedBy` (trigram/semantic/both).

On a match, and only when `force` is not true and `duplicateOf` is not
set, the call creates nothing and returns
`{created:false, duplicates:[...], decisionRequired:true}`. Three-way
rubric — never auto-pick:

- (a) **stop and ask the user** — the default safe path;
- (b) retry with `force:true` if the user confirms it is genuinely
  distinct;
- (c) call again with `duplicateOf:"demo-42"` to create the new issue
  and link it (auto-creates a `duplicates` dependency edge).

## Dependency edge types

Seven types, and **direction is canonical per type — not
negotiable**. Read every row as `from → to`. On `skein_create`'s
inline `dependencies[]` the new issue is always the FROM side; on
`skein_dep_add` / `skein_dep_add_many` you name both sides.

| Type | Meaning (`from → to`) |
|------|-----------------------|
| `blocks` | `from` is BLOCKED by `to` — `from` can't close until `to` closes, and `skein_ready` hides `from` while `to` is open. |
| `waits-for` | `from` waits on `to`. Optional `gate` (`all-children` / `any-children`) is validated and stored but **not evaluated**: `skein_ready` holds `from` exactly as for `blocks`, until `to` ITSELF is closed or standing — `to`'s children don't count. `gate` on any other type returns `validation_error`. |
| `related` | Symmetric "see also". No direction, no gating. Replaces free-text "see also". |
| `discovered-from` | `from` was found while working on `to` — the standard edge for an autonomous side-quest. |
| `supersedes` | `from` REPLACES `to`; `to` becomes obsolete. The audit trail for a move/rewrite. |
| `duplicates` | `from` is a duplicate of `to` (`to` is the original). `skein_create(duplicateOf=…)` writes this edge for you. |
| `parent-child` | `from` (child) sits hierarchically under `to` (parent). Canonical epic→subtask mechanism — see "Hierarchy & re-parenting" below. |

To express a reverse relationship, swap the sides — never invent
an inverted type. Wanting a *new* issue to be the PARENT of an
existing one means: create it, then `skein_reparent` the existing
child onto it.

Cross-project edges are allowed within one space, and you must
already see both projects. A target of another space or of a
project you cannot see answers `issue_not_found`, exactly as a
nonexistent issue. An edge that would close a
directed cycle returns `dependency_cycle`. A second identical
tuple returns `dependency_already_exists`. Inline
`dependencies[]` on `skein_create` (max 50) runs in the SAME
SERIALIZABLE transaction as the insert — one bad edge rolls the
whole create back and the error names the failing index.

After the edge lands: `skein_ready` gates on it, the UI
Dependencies section renders it, and close-time warnings consume
it. `skein_show` counts it by default and shows it on request —
the default answer carries `depsCounts` (direction × type ×
status, plus an `unavailable` bucket) and NO edges; ask for the
edges by type, `skein_show(id, depsTypes:["blocks"])`, and they
arrive under `deps.outgoing` / `deps.incoming`. There is no
`dependencies` key and no "give me every edge" request.

**Blockers you cannot see.** The card's `blockers[]` lists every
open, in-progress or deferred target of an outgoing `blocks` /
`waits-for` edge in a project inside your horizon (same rule as
`epic.unavailableChildren`); `isBlocked` is true iff that list is
non-empty. An edge to an issue outside your horizon is never an
entry: `unavailableBlockingLinks` on the card counts such edges
whatever their status, closed ones too — it says a link exists, not
that it blocks. The same rule drives `isBlocked` on list rows, the
`isBlocked` filter, prime's `blocked`, `epic.blockedChildren` and
project / component blocked counts. `skein_ready` still counts every
blocker, so an issue with `isBlocked: false` and
`unavailableBlockingLinks > 0` may be missing from it.

## Hierarchy & re-parenting

Parent-child is the canonical hierarchy edge (child → parent,
each issue has at most one parent). There are exactly three ways
it changes, and `skein_dep_add` is **not** a re-parent tool:

- **Set the parent at creation.** Pass `parentId` on
  `skein_create` (it picks the next child id `parent.N` and writes
  the parent-child edge atomically), or include
  `{toId: <parent>, type: "parent-child"}` in the inline
  `dependencies[]`. Both set the parent of a *brand-new* issue.

- **Move an existing issue → `skein_reparent`.** This is THE tool
  for re-parenting. It atomically replaces the issue's single
  parent-child edge inside one transaction, bumps the issue
  version, and emits `issue.reparented`. Required:
  `skein_reparent(id, newParentId, expectedVersion)`. The new
  parent must be in the same project (`reparent_cross_project`
  otherwise) and must not be a descendant of the issue
  (`dependency_cycle`); an issue can't be its own parent
  (`reparent_self`). On a version mismatch you get
  `version_conflict` with `currentVersion` — re-read and retry.
  Re-parent is **not** idempotent: re-running with the same parent
  still performs a real move and bumps the version, so compare
  `parentIdCurrent` first if you want to avoid a no-op.

- **`skein_dep_add(type="parent-child")` only ADDS.** If the issue
  already has a parent it returns `parent_already_set` — it will
  NOT silently create a second parent edge. When you see that
  code, switch to `skein_reparent`.

To drop a parent-child edge without setting a new one (rare), use
`skein_dep_remove(fromId, toId, "parent-child")`.

## Post-action summaries

After every write operation, print a compact summary in the chat
so the user can scan what happened without parsing JSON. Use the
formats below verbatim — predictable structure helps both the
human reader and any log-grep tooling.

After **`skein_create`**:

```
✓ skein/sk-127 created
  component:  skein-frontend
  type/prio:  feature / P3
  title:      "Web: add a login form"
  url:        http://host.docker.internal:8080/issues/sk-127
```

After **`skein_create`** that returned `decisionRequired:true`:

```
⚠ skein/skein: NOT created — possible duplicates:
  1. sk-42  (similarity 0.84) "Web: sign-in form"       status: open
  2. sk-118 (similarity 0.71) "UI: login page"          status: closed

Three options:
  • stop and ask the user
  • skein_create(..., force=true)         — if genuinely distinct
  • skein_create(..., duplicateOf=sk-42)  — link as duplicate
```

After **`skein_claim`**:

```
→ skein/sk-127 claimed
  status:    open → in_progress (lock by mcp-session-1d7a…)
  blockers:  sk-98 "Token rotation" open · +1 unavailable link
```

(The claim ack carries no blockers: read them via `verbose: true` or
`skein_show`. Links you cannot see print as `+N unavailable link`;
`none` only when both are empty.)

(Claim is atomic over lock + status: open/deferred → in_progress
in the same transaction; in_progress stays put; closed returns
`cannot_claim_closed` — reopen via `skein_update(status=open)` first.)

After **`skein_unclaim`** (three intent paths):

```
→ skein/sk-127 unclaimed
  intent:   handoff     (nextStatus=in_progress — status stays put)
  reason:   "wrote tests, design review next"
  events:   issue.unclaimed (no status_changed — status unchanged)
```

```
→ skein/sk-127 unclaimed
  intent:   give-up     (nextStatus=open — back to the ready pool)
  status:   in_progress → open
  reason:   "stuck on auth flow, need fresh eyes"
  events:   issue.unclaimed + issue.status_changed
```

```
→ skein/sk-127 unclaimed
  intent:   defer       (nextStatus=deferred — punt for later)
  status:   in_progress → deferred
  reason:   "wait for upstream API to ship"
  events:   issue.unclaimed + issue.status_changed
```

After **`skein_close`**:

```
✓ skein/sk-127 closed
  reason:   "implemented hybrid auth"
  commits:  abc1234, 5678ef00
  events:   issue.closed + issue.unclaimed (3 watchers notified)
```

After **`skein_comment`** (only when posting non-trivial progress):

```
… skein/sk-127 commented
  "wired Keycloak callback; tests pending"
```

Stick to these shapes. The leading glyph (`✓` `→` `⚠` `…`) signals
outcome at a glance: success / state-change / decision-required /
in-progress.

## Identity model

- **api_key** = OWNING USER (`api_keys.user_id` FK). All permission
  checks resolve against this user. Anyone with your api_key can
  act as you; treat it like a password.
- **`Mcp-Session-Id`** = THIS agent instance, auto-set per MCP
  connection by your client; rotates on reconnect. The server
  lazily upserts `agents (user_id, session_id)` on first contact.
- Both `skein_whoami` and `skein_prime` echo your `sessionId`. The
  same id is written to `events.actor_session_id` on every action
  you take. A conversation log that captured the printed id joins
  to skein audit on string equality — no shared storage needed.
- Multiple sibling agents per user is normal. `issues.locked_by`
  stores `session_id`, so claims are per-agent. To keep work pools
  disjoint, scope `skein_prime` with `project=` / `component=`.
  Optimistic claim resolves true ties on the same scope.
- Beside `lockedBy` (session) and `lockedAt`, the card's
  `lockedByUsername` is who PERFORMED the claim holding the lock (an
  admin claiming for someone is the performer, not the assignee). It
  is absent when unlocked or the claim record is gone, and goes with
  the lock; a request that keeps the lock cannot change the name.
  Write acknowledgements do not carry it.

## Default answers, and the cheap route to more

Every read tool answers narrowly by default and names its own way to
more. The point is not modesty: a full answer you did not need is paid
again on every later turn of the session, because the tool result stays
in the transcript.

| Tool | Default answer | The way to more |
|------|----------------|-----------------|
| `skein_show` | card + `depsCounts` (no edges) + child counters + last 3 comments | `depsTypes:[…]` for edges of named types; `commentsLimit:0` for the thread index; `includeEvents` |
| `skein_list` | 25 rows (ceiling 100), the named lean row — no prose | `fields:[…]` for other columns; `skein_show` for one issue whole |
| `skein_search` | 25 hits, plain snippets (no highlight markup) | follow the hit id: `skein_show` / `skein_comment_get` |
| `skein_project_list` | 50 rows, description cut to 200 chars (`descriptionTruncated` when cut) | no MCP reader for the full text — open the project in the web UI |
| `skein_ready` | 25 slim rows (ceiling 100), **no** `labels` / `componentIds` keys | both from the `skein_show` card; `skein_list` carries `labels` by default, but `componentIds` only when named — `fields:["componentIds"]` |
| `skein_prime` | three lists of the same named lean row (no `componentIds` on any of them); `readyForMe` additionally has no `labels` | `skein_show`, or `skein_list(fields:[…])` |
| `skein_notifications(inbox)` | rows with long values cut (`valuesTruncated` when cut) | `newValue`: the entity the row points at — `skein_show`, or `skein_comment_get(commentId)`. `oldValue`: no MCP reader — the preview is all of it |

**A missing key is not an empty value.** Where a surface never loads
something — labels on `skein_ready`, prose on a write acknowledgement —
the key is **absent** rather than `[]` or `""`, so that "not supplied
here" stays distinguishable from "this issue has none". Where a value
was *cut*, you get a marker (`descriptionTruncated`,
`valuesTruncated`, the index's per-entry flag) and the marker appears
**only when something really was cut** — so it always means "there is
more", never "this might be all".

### Two routes worth memorizing

**"Is this branch finished?" without pulling the graph.** Read
`unclosedChildrenCount` on the card (with `depsCounts.down` beside it).
Non-zero `unavailable` in those counts means the honest answer is
"cannot tell" — some children are in projects you cannot see. Need the
children themselves? `skein_list(parentId=<id>)`. Need the edges?
`skein_show(id, depsTypes:["parent-child"])`. Three bounded steps
instead of one unbounded dump.

**"What was said on this issue?" without downloading the thread.**
`commentsLimit: 0` → index → `skein_comment_get(id)` for the entries
that matter. Spelled out under step 7 of the per-task loop, which is
also where the obligation to walk it lives.

## Top rules

- **Hierarchical ids only**: `demo-42`, `demo-42.1`. Bare numbers
  (`42`) are rejected by every tool.
- **Optimistic concurrency**: `skein_update` / `skein_close` require
  `expectedVersion`. Mismatch → `{"error":"version_conflict",
  "currentVersion":N}` — re-read with `skein_show`, reconcile,
  retry. Never pass an `expectedVersion` you didn't just observe.
- **Write-tools answer with an acknowledgement, not with the issue.**
  `skein_create`, `skein_close`, `skein_claim`, `skein_unclaim`,
  `skein_accept`, `skein_update`, `skein_reparent`,
  `skein_commits_attach`, `skein_commits_detach`, `skein_epic_create`,
  `skein_drop_delegated` and `skein_resume_delegated` return the **identifier, the applied
  state and the new `version`** — plus the stamps that particular
  write moved (`status`, `assignee`, the lock / claim / close
  stamps). Not the card: no `description`, `design`,
  `acceptanceCriteria`, `notes` — and no `title`, `labels`,
  `componentIds`, `commitRefs`, `parentIdCurrent` either. You just
  supplied or already knew those, and every echoed byte would sit in
  your context for the rest of the session and be re-sent on every
  later turn.

  **Do not read absence as emptiness.** A key missing from an
  acknowledgement says nothing about the issue — it says this
  response does not carry that key. The narrowing is unconditional
  and documented, so no marker repeats it (`bodyOmitted` is gone from
  issue writes for that reason; it survives on **comment** writes,
  meaning "elided, not empty").

  **The version is always there**, so the `expectedVersion` cycle
  continues without an extra read — the one guarantee the
  acknowledgement makes on purpose. Need the whole issue in the same
  round-trip? Pass `verbose: true` for the full `IssueDetail` — except
  when the write applied but its card could not be read back: then
  you get the acknowledgement plus `cardUnavailable: true`; read the
  card with `skein_show`, do not repeat the write. Need it later?
  `skein_show`.

  Two shapes worth knowing before you parse one:
  - a **group** write (`skein_claim` with `ids`, and
    `skein_epic_create`) answers with one row per input issue —
    `{results: [...], summary: {requested, succeeded, failed}}` for
    the claim batch, `{epic, children: [...]}` for the epic. That
    envelope is **not** an invitation to partial success: both are
    all-or-nothing, so `failed` is 0 in every response that can
    exist, and a failing batch is a tool error with nothing written;
  - `skein_close` still puts `warning` and `openChildList` at the
    **top level** of the response, under those names, in both modes.
- **Comment writes don't echo the body either.** `skein_comment` and
  `skein_comment_update` return the comment row WITHOUT `body`, also
  flagged `bodyOmitted: true`. Same reasoning, one step sharper: you
  supplied the text verbatim, so an echo puts the identical string in
  your context twice — argument and result — for the rest of the
  session. There is **no `verbose` escape hatch** here: a body you
  just wrote has no post-write use. `id`, `issueId`, `version`,
  `author`, `authorKind`, `createdAt`, `mentions` all survive, so a
  follow-up `skein_comment_update` / `skein_comment_delete` chains
  straight off the response. Read a body back with
  `skein_comment_get` (one comment, by id) or `skein_show` (the
  thread) — those are the tools that exist to return it.
- **Duplicate detection**: `skein_create` may return
  `decisionRequired: true` with `duplicates[]` and create nothing.
  Never auto-pick — the three-way rubric (ask / `force` /
  `duplicateOf`) is in "Duplicate detection" above.
- **Status discipline**: backend never *autonomously* mutates an
  issue's status. The lifecycle verbs are explicit (see "Lock +
  status lifecycle" below for the full table); never write
  `expectedVersion` you didn't just observe.
- **`@username` mentions** in comment body and in `description` /
  `notes` / `acceptanceCriteria` / `design` notify the mentioned
  user (they do not subscribe them). Self-mentions are excluded;
  non-member usernames are silently dropped (no membership leak).
- **Stale lock auto-release**: 30 min default, and it now needs BOTH
  halves: the lock untouched for 30 min AND your MCP session silent
  for 30 min. Any tool call on any issue keeps every lock you hold —
  writing to a channel task or to children counts, so a long task
  you are not writing INTO no longer loses its lock. Agents that
  lose their connection still do not permanently block work. An
  auto-release is `issue.unclaimed` with `actorType: "system"` and
  `reason: "auto-released-stale-lock"` — that is how you tell "the
  timer took it" from "a person dropped it".
- **Batch tools** (`skein_close_many` / `skein_update_many` /
  `skein_dep_add_many`): cap 100 per call. Per-row partial success
  — inspect `results[i].ok` plus per-row `error.code`. Top-level
  `force=true` applies uniformly to every item.

  **A batch can stop part-way, and says so per row.** Rows commit one at
  a time, so "the batch failed" never happened — some prefix succeeded.
  Two codes mark a row the server stopped; neither is `internal_error`
  (that one means the row was attempted and broke):
  `batch_deadline_exceeded` (server-side deadline) and
  `request_cancelled` (the connection went away — you may not even
  receive this response). No stopped row reported a commit, yet one cut
  mid-flight may have written:

  - never started ("before this row ran") — nothing applied, resend;
  - `skein_dep_add_many` edge — resend; a landed edge answers
    `dependency_already_exists`;
  - `skein_update_many` row — earlier steps may have landed: re-read,
    resend only what is missing with the live `version`;
  - `skein_close_many` row — resend with the same `expectedVersion`; a
    landed close answers `version_conflict` (not under `force=true` —
    re-read first).

  **Got nothing back? Do not assume nothing was written:** re-read, then
  resend only rows whose live state is not what you wanted.

  Decide that by COMPARING, not by trusting a conflict to stop you.
  `expectedVersion` refuses a replay only on the steps that check it
  (field patch, status, close) and only with the version from before the
  cut — re-read and your resend carries the new one, which applies
  again. Label and component rows never consult it: a re-added label is a
  silent no-op that does not even move `version`.
- **Attachments**: one tool, `skein_attachments`, with an `action`
  discriminator (`upload_url` | `confirm` | `list` | `metadata` |
  `get_url` | `delete`). Upload is 3-step browser-direct to S3 —
  `action="upload_url"` → browser PUT → `action="confirm"`. Max 500 MB;
  executables rejected by extension. Hard delete (no soft-delete).

## Lock + status lifecycle

Lock and status are two sides of one coin and the lifecycle verbs
keep them in sync atomically. **Never** call
`skein_unclaim` and then `skein_update(status=…)` as two steps — use
the atomic form below. **Never** assume a claim leaves status
unchanged — it doesn't anymore.

| Verb | Lock | Status transition | Required args | Notes |
|------|------|-------------------|---------------|-------|
| `skein_claim(id)` | take | open/deferred → in_progress · in_progress: no-op · closed: rejected | id | Atomic over lock + status. Re-claim by same session = idempotent no-op. closed → `cannot_claim_closed` (reopen via `skein_update(status=open)` first). |
| `skein_unclaim(id, nextStatus, reason)` | release | nextStatus in {in_progress, open, deferred} | id, nextStatus, reason | Three intents: `in_progress` = handoff (status stays put for next agent); `open` = give up (back to ready pool); `deferred` = punt (set defer_until via follow-up `skein_update`). `closed` rejected — use `skein_close`. `reason` non-empty; force=true (force_unlock: project owner, space owner, instance admin; others get `force_unlock_forbidden`) appends " (force-overridden)". |
| `skein_close(id, expectedVersion, reason, closeOutcome, commits?)` | release | any non-closed → closed | id, expectedVersion, reason, closeOutcome (on a transition) | Emits issue.closed + issue.unclaimed when previously locked. `commits[]` (max 10) attaches git hashes. **Re-close overwrites `closed_reason`** — for additive commit attach on an already-closed issue use `skein_commits_attach`. |
| `skein_commits_attach(issueId, commits[])` | unchanged | unchanged | issueId, commits | Pure additive: links commits to an issue (any status) WITHOUT touching status, closed_reason, closed_at, version, or locked_by. Use when the original `skein_close` forgot `commits[]` or a hash landed after the close. Issue-level idempotent via `issue_commits` UNIQUE(issue_id, repo_id, hash). Lock-aware (force=true needs force_unlock: project owner, space owner, instance admin). |
| `skein_commits_detach(issueId, hashes[])` | unchanged | unchanged | issueId, hashes | The removal counterpart: unlinks commits, touching nothing else. Matches on HASH alone — every row of this issue carrying it goes, whichever repo it was filed under. Answers `detached` / `notFound` at the top level; same lock predicate, no `expectedVersion`. |
| `skein_update(id, expectedVersion, status?, …)` | unchanged | any explicit transition | id, expectedVersion | The escape hatch for arbitrary edits. Does NOT touch lock. Use it for reopen (closed → open), changing assignee/labels/etc. |

Idempotency rules apply: re-claim by the same session on an
`in_progress` issue is a no-op; `unclaim` on an already-unlocked
issue is a no-op (the `nextStatus` is ignored — there's no lock to
release, so the contract has nothing to do).

## Commit refs (`commits[]`)

`skein_close`, `skein_close_many` (per item) and
`skein_commits_attach` all take the same commit-ref item shape,
max 10 per call:

- `hash` — **required**. Lowercase hex, 7..64 chars (abbreviated
  SHA-1 through full SHA-256).
- **Repo resolution** — every commit links to a repo declared in
  the project's `gitRepos[]`. Supply `repoId` (the repo UUID —
  takes precedence) **or** `repoUrl` (exact string match against
  `gitRepos[].url`), **or** omit both when the project has exactly
  one repo. If the project lists several repos and you omit both,
  the ref is ambiguous and rejected. Repo missing from
  `gitRepos[]`? Add it with
  `skein_project_update(gitReposAdd=[…])` first.
- Optional metadata: `url` (canonical commit page), `message`
  (subject line), `author` (name or email), `committedAt`
  (RFC3339).

Dedup is row-level and silent: `issue_commits` is UNIQUE on
`(issue_id, repo_id, hash)`, so re-sending a hash is a no-op —
the `issue.commits_attached` event is still written so the audit
trail records the intent.

**But hash-dedup is not close-idempotence.** Every `skein_close`
rewrites `closed_reason` and `closed_at`, bumps `version`, and
re-emits `issue.closed` + `issue.status_changed` afresh — including
on `skein_close_many`, per item. To add hashes to an already-closed issue
without trampling the original reason, use `skein_commits_attach`
— it touches nothing but the commit links, needs no
`expectedVersion`, and works on an issue in any status.

**A wrong hash is removable — `skein_commits_detach(issueId,
hashes[])`.** Nothing verifies that a hash names a real object: Skein
has no clone of the repo, so the only check is the FORM. An
abbreviation padded out by hand therefore passes and is written —
this has happened. Take hashes from `git log`; if one is already wrong,
detach it.

Detach matches on the **hash alone** — attach's repo resolution can
itself fail, and that failure is one of the ways a wrong row gets
written, so the way back must not depend on it. Every row of that
issue carrying the hash goes, including one filed under the wrong
repo. Read the answer's `detached` / `notFound`: a typo in the hash
produces the same success as a real removal, and only `notFound`
tells them apart.

## Standing tasks

`standing` is a 5th status for a **perpetual obligation** (e.g. "watch
upstream for new releases and merge them") — it never completes, is
excluded from counts / `skein_ready` / inbox, and shows as its own
`standingCount` pill (labelled "Standing" in the web UI). You don't claim or work it directly (`skein_claim`
→ `cannot_claim_standing`); instead **spawn a normal child** per
occurrence with `skein_create(parentId=<id>, …)` and close that — the
closed children are its history. Make one with `skein_create(standing:
true)` or promote an existing task with `skein_update(status=standing)`;
the only demote is `standing → open`.

A standing task is created with `status=standing`, no `started_at`,
NOT auto-accepted, and never locked or owned — an unassigned standing
task is normal, not work waiting on someone.

The remaining constraints are structural invariants: `force=true` does
**not** bypass any of them.

| Attempt | Code | Do this instead |
|---------|------|-----------------|
| `skein_claim` on a standing task | `cannot_claim_standing` | spawn a child and claim that |
| `in_progress → standing` | `cannot_promote_locked_to_standing` | `skein_unclaim` first, then promote |
| `standing → in_progress` | `cannot_work_standing_directly` | spawn a child (`skein_create(parentId=…)`) |
| `standing → deferred` | `cannot_defer_standing` | demote to `open` first |
| standing under/over another standing | `nested_standing_forbidden` | standing tasks never nest — flatten one leg |
| `skein_close` while any child is not closed | `standing_has_open_children` (+ `childIds`) | close/retire the children, then retire the parent |

`closed → standing` is allowed and reopens-as-standing (clears
`closed_at`). Normal epics are unaffected by the retire block — it is
standing-specific.

## Review flag — the author's private bookkeeping

Every issue carries a **review-required flag**; a closed one may also
carry its author's **review mark**. Both belong to whoever CREATED the
issue and are invisible to everyone else — the assignee sees neither, on
any surface, and neither moves `version` or `updatedAt`, so "something
happened" cannot be inferred either. Purpose: a closed delegated task
whose result the author has not read is the author's own outstanding
work, and Skein reminds them of it.

- **On create.** `skein_create` takes an optional `reviewRequired`.
  Omitted, it takes the caller's PERSONAL DEFAULT, whose built-in default is
  `true`. So "don't tell me when this one is done" is NOT the default:
  pass `reviewRequired: false` explicitly — saying it in the description
  changes nothing.
- **Later.** `skein_update(id, expectedVersion, reviewRequired: <bool>)`,
  **in a call of its own**. Combined with any other field the call is
  rejected whole and NOTHING is written, the other fields included.
  `skein_update_many` refuses the key outright. Author only — "not the
  author" includes admins.
- **The mark has no agent tool, deliberately**: "reviewed" means a person
  read the result. People set it in the web UI, on a closed issue they
  created whose flag is still on. Clearing the flag is not a mark — it
  drops the issue from the set without asserting anything was read.

## Assignee on create

`skein_create` / `skein_epic_create` (epic and each child) without
`assignee` assign the issue to the API key's owner — accepted, still
`open`, no lock — if the owner's setting "assign agent-created issues
to me" is on (default; web UI notification settings). Not for
standing issues, nor "under someone else": the nearest ancestor with
an assignee (this call's epic included) decides — another person, an
entry naming no user, or an ancestor you cannot see → no assignee.
An explicit `assignee` wins; blank counts as absent. User wants none
→ `unassigned: true` (with an `assignee` → `validation_error`); set
it on the epic and on each child separately — children do not inherit
it. Remove later: `skein_update(assignee="")` — unassigned and no
longer accepted. REST create never does this.

## Tool discovery

42 tools total. Three are always-loaded: `skein_whoami`,
`skein_prime`, `skein_get_instructions`. The other 39 are deferred
behind your client's Tool Search.

Some tools are **consolidated**: one tool covering a family of related
operations, selected with a required `action` parameter. Today:
`skein_attachments` (upload_url | confirm | list | metadata | get_url |
delete), `skein_watchers` (watch | unwatch | project_watch |
project_unwatch | filter_create | filter_delete | filter_list | list),
`skein_component` (list | create | update | archive), `skein_user`
(list | create | update), `skein_project_members` (list | add | remove),
`skein_theme` (instructions | describe | fork | preview_variables |
set_variables | inspect_clear), `skein_notifications` (inbox |
mark_read | mark_read_issue | mark_read_all | archive) and
`skein_remind` (set | list | snooze | dismiss). If you send an
unknown or missing `action` you get a `validation_error` carrying
`allowedActions` — read it and retry, do not re-run Tool Search.

### Client-namespacing — the suffix is always `skein_<verb>`

The wire name on the Skein server is the bare form: `skein_create`,
`skein_project_resolve`, etc. **MCP clients rewrite this name
before exposing it to you** and they all do it differently:

| Client | What you see |
|--------|--------------|
| Claude Code | `mcp__skein__skein_create` |
| Cursor | `mcp_skein_skein_create` |
| Cline | `cskein__CLINE_MCP_TOOL_IDENTIFIER__skein_create` |
| Codex | `skein__skein_create` |
| OpenCode | `skein_skein_create` |
| VS Code Copilot | opaque `f1e_skein_create`-style |
| (no-prefix clients) | `skein_create` |

When this guide says "call `skein_create`" it means **the tool
whose name in your client's registry ends in `skein_create`** —
the suffix is always the wire name; everything before it is your
client's prefix. Use whatever full string your client shows you.

### Two ways to load deferred tools

1. **Keyword search** — pass topic words. Works regardless of how
   your client namespaces:
   ```
   ToolSearch query="skein project resolve component create"
   ```

2. **Direct load with `select:`** — exact-string match against
   the registry. **Copy the name verbatim from your client's
   deferred-tools list / system reminder, including any prefix**.
   Typing the bare wire name (`skein_create`) fails on every
   client that prefixes — and most of them do.

   The shape of the call is uniform; only the names differ:
   ```
   # client-specific names — copy from your registry, do not type
   # from memory
   ToolSearch query="select:<name-1>,<name-2>,..."
   ```

Each tool's MCP description carries its full input schema,
parameter docs, and behavior notes; this guide covers cross-
cutting rules that don't fit into any single tool.

## Theme customization

When the user asks for visual / theme changes ("change the color", "another
font", "make it darker", "like Alice's"), do NOT improvise with the
other `skein_theme` actions. Call `skein_theme(action="instructions")`
first — it returns the canonical workflow (fork → preview → confirm →
persist), the onboarding script you read to the user, and the
anti-patterns. It answers with raw markdown, not JSON. Cache the
response for the session.

## Monitoring a task / pairing with another agent

A headless agent has no live notification channel — the Web UI gets
SignalR push, you don't. When two agents need to work a task together
(one assigns, the other hits a blocker and asks a question in a
comment), the assigner never sees the comment. The **task monitor**
closes that gap: it subscribes you to an issue and streams new
notifications into your context near-real-time. By default it delivers
both new comments (`comment.created`) **and** task closure
(`issue.closed`).

**WHEN to use it — trigger phrases.** Map the user's intent to
`skein_get_monitor_script`:

- "watch this task" / "pair on this issue" / "subscribe and reply to
  comments" / "monitor sk-42 and its subtasks" →
  `skein_get_monitor_script(id, subtree=…)`.
- Blocked-agent direction: "if you get blocked, ask the
  assigner in a comment and watch for the reply" → `skein_comment(id,
  …)` to post the question, then `skein_get_monitor_script(id)` to watch
  for the answer.

**HOW it works (one MCP call + one Monitor run).**

1. Call **`skein_get_monitor_script(id, subtree=false)`**. Pass
   `subtree=true` to also cover every current and future subtask (e.g.
   monitoring `sk-42` catches comments on `sk-42.1`, `sk-42.1.3`, …).
   `eventTypes` (default `["comment.created", "issue.closed"]`) accepts
   `"comment.created"` and types starting with `"issue."` (e.g. add
   `"issue.status_changed"`); anything else is rejected. The one call
   does three things:
   auto-subscribes you as a watcher (`watching: true`), mints a scoped
   `skm_` monitor token, and returns `monitorId` (this monitor's handle)
   and a ready-to-run `bootstrap` command.
2. **Write the returned `token` to the returned `tokenFilePath`
   (0600) with the Write tool** — NOT into a shell command. The token
   never belongs in `bootstrap`, argv, or shell history; the runner
   reads it from that file. The token is **read-only** (it can only poll
   notifications — there are no write routes behind it) and lapses
   after the server's renewal window (default 6h — the tool description
   names the value) without a successful token check, with a
   12-hour hard cap; every poll renews it.
3. **Run the returned `bootstrap` via the Claude Code `Monitor`
   tool with the maximum `timeout_ms`** (not `bash &` /
   `run_in_background` — buffered stdout would hide the stream).
   Agents without a Monitor tool can run it under plain `bash` /
   `python3`. The bootstrap fetches the Python stdlib
   runner from `GET /agent/v1/monitor-script` and execs it; the runner
   polls `GET /agent/v1/notifications/poll` and emits **one JSON object
   per line** to stdout — `{"event":"notification","notification":{…}}`
   for each new item, plus lifecycle lines `monitor_started`, `degraded`
   / `recovered` and one final stop line; nothing on an idle poll (no
   heartbeat). The runner rides out server and network trouble by
   itself: never restart it for `degraded` — follow its `action` (with
   `cause: "cursor_reset"` some notifications may be lost, so
   `skein_show` the task). The monitor is **live-start**: you receive
   only NEW notifications from the moment you
   start — there is no historical backlog (the first poll anchors at the
   server clock), so start the monitor before you expect activity.
4. **Skip your own echo before acting on a line — self-skip by
   `actorSessionId`.** Every `notification` line carries the originating
   event's session at `notification.actorSessionId`. The monitor tail
   reads the shared audit log, so events **you** emit under this same api
   key — a sibling session, or your *own* current session — now come back
   down the stream too. Before you react to a `notification` line,
   compare `notification.actorSessionId` against your **current** MCP
   session id (the one `skein_whoami` echoes): if they are equal, the
   line is your own echo — **ignore it, do not act on it.** Acting on
   your own echo is a self-loop (you answer your own comment, which emits
   another event, which comes back, …). The precise rules:
   - Compare **only when `actorSessionId` is present and non-null.** A
     line with no `actorSessionId` (absent) is never your echo — it came
     from a session-less actor (the human owner in the Web UI, a REST
     call, a system event) and MUST be acted on. Absent ≡ "not mine".
   - Self-skip applies to **`notification` lines only.** Lifecycle lines
     (every other `event`) do not carry `actorSessionId` and are never
     subject to it.
   - The comparison is against your **live** session, so it survives a
     reconnect/compact that rotates your `Mcp-Session-Id` — re-read your
     current id from `skein_whoami` after any reconnect. (A server-side
     `exclude_session` rebind also drops some echo, best-effort; the
     reader-side self-skip is the authoritative guard.)

   For every `comment.created` line that is **not** your own echo,
   use the **pairing path** (do **not** `skein_show` the whole
   thread on every line):
   - Read `bodyPreview` (+ `commentId`) from the notification —
     bodies ≤1024 runes arrive complete on the poll itself, so most
     lines need no follow-up call at all.
   - If `truncated` is true on that line, call
     `skein_comment_get(commentId)` for the full body — that is the
     single-comment fetch; **not** a full-thread show.
   - Call `skein_show` only when you need broader issue context
     (status, deps, older comments) beyond this one comment.
   Then reply with `skein_comment <id>` as usual. Lifecycle and
   non-comment notification lines follow their own rules (below);
   they are not subject to the preview→get pairing path.
5. **Every stop prints one final line `{"event","reason","action"}` —
   follow its `action`.** `event` is the monitor's fate: `closed` — this
   bootstrap cannot bring it back; `paused` — the **same `bootstrap`**
   can: at once after `stopped`, later after `server_unreachable`; after
   `internal_error` retry once, then mint a new monitor (the token file
   stays, the token lives until the renewal window runs out, the break's
   notifications are lost — `skein_show` the task); `duplicate` — a
   runner already serves this token file, do nothing. Among `closed`:
   - `token_expired` (`status: 401`) — the credential is dead. Mint a new
     monitor, unless you stopped this one yourself.
   - `visibility_lost` (`status: 403`) — you lost visibility of the
     monitored project. This is **not** token death: do **not** re-mint
     in a loop; regain project access first, then mint.

   **The `Monitor` ended without a stop line** (`timeout_ms` ran out,
   the process was killed, the interpreter died): run the same
   `bootstrap` again; if it keeps ending without one, check `python3`.
6. **Ending a monitor.** The runner exits by itself with `root_closed`
   when the ROOT task is closed (by anyone, you included) and every
   notification due before the close has been delivered — you do not
   stop it. Under `subtree=true` a child closing does not end it. To
   drop a monitor you no longer need, call
   `skein_watchers(action="monitor_stop", monitorId)`; its runner then
   stops with `token_expired`.

**Two-agent pairing loop (the canonical case).**

```
A: skein_create(...) → skein_claim → assigns work to B (or hands off)
B: skein_claim(sk-42) → works → hits a blocker needing A's decision
B: skein_comment(sk-42, "blocked: which auth flow — Keycloak or
   local? need your call", kind="autonomous")
B: skein_get_monitor_script(sk-42)  → watches for A's reply
A: skein_get_monitor_script(sk-42)  → sees B's comment stream in
A: bodyPreview (or skein_comment_get if truncated) →
   skein_comment(sk-42, "Keycloak", kind="directed")
B: monitor surfaces A's reply → unblocks → continues → skein_close
```

Both agents talk **in the task's comments** — one timeline, visible to
the human, not a hidden side-channel.

**Related, without the runner.**
`skein_watchers(action="watch", issueId, subtree=true)` subscribes you
(and your subtree) without starting a monitor — useful
when you only want the issue in your normal inbox. To filter a comment
thread by who wrote each comment, `skein_search(commentAuthorKinds=[…])`
narrows by `author_kind` (`human` / `agent_directed` /
`agent_autonomous`) — e.g. to read only the human's replies, or only
another agent's autonomous notes.

**Auto-subscription.** The assignee always, except one filled in by
the "assign to me" setting — at create only. Your human (the creator)
only for a hand-made issue or a **top-level** `kind="directed"` one;
subtasks under a parent and `kind="autonomous"` issues do not subscribe
them.

## Closing a work session

Before you stop, the issues you touched must accurately reflect
their state. Work is not "done" because you stopped typing.

1. **File follow-up issues** for anything you discovered but didn't
   finish. Use `skein_create({..., dependencies: [{toId: <parent>,
   type: "discovered-from"}]})` so the edge to the parent lands
   atomically with the new issue. Do not leave context stranded in
   chat alone.
2. **Update the issues you touched**:
   - Finished work → `skein_close <id>` with `reason`,
     `closeOutcome` and `commits[]` (the hashes that closed it).
   - In-flight work the user wants paused → `skein_comment <id>`
     with progress notes and pointers (file paths, commits-so-far)
     so the next session (or another agent) can pick up.
   - Stepping away without closing →
     `skein_unclaim(id, nextStatus, reason)`. Declare intent:
     `nextStatus="in_progress"` (handoff), `"open"` (give up),
     `"deferred"` (punt). Don't leave issues claimed but silent.
3. **Verify**: re-run `skein_prime` and confirm `myInProgress`
   matches what you actually have open. Anything you forgot will
   surface here.

## Feedback for Skein developers

If during the session you hit real friction with Skein, end with
one paragraph addressed to the user, prefixed
`Skein developers:` — concrete, cite the
tool call. The user forwards it. Skip it if there is nothing to report.

## Text fields — forbidden literals

Free-text fields (`description`, `notes`, `acceptanceCriteria`,
`design`, comment bodies, …) are scanned for **tool-call wrapper
syntax** that leaked out of your own client's wire format. A hit is
rejected naming the field and the literal — over MCP as
`validation_error` with `code: tool_call_syntax_leaked`, over REST as
`tool_call_syntax_leaked` directly. The set is closed and
case-sensitive — the wire format is case-sensitive too:

| literal | matches |
|---|---|
| `<function_calls>` | opening wrapper tag (exact) |
| `</function_calls>` | closing wrapper tag (exact) |
| `<invoke` | both bare `<invoke>` and `<invoke name="…">` |
| `</invoke>` | closing invoke tag |
| `<parameter name=` | parameterized opening tag (bare `<parameter>` is fine) |
| `</parameter>` | closing parameter tag |

Preflight against this table instead of learning it from a rejection.
The usual cause is closing a parameter block with the wrong tag, which
makes your client glue several parameters into one value — so a hit
normally means the *call* is malformed, not that the prose is wrong.
Prose that legitimately needs to discuss these tags has to break them
up (e.g. insert a zero-width space or describe them in words).

**Look-alikes that are NOT rejected — and what actually happens.** A
parameter block closes ONLY with `</parameter>`, never with the field
name, so a closing tag named after the field (`</description>`,
`</design>`, `</notes>`) is the same mis-close as the literals above.
The scanner does not match those — they are not in the table and will
not be added to it. That is not a hole, because a real mis-close does
not end at the bad tag: your client keeps reading and glues the
following tokens into the value, and those carry a literal that IS
scanned — `<parameter name=` when another parameter follows, `</invoke>`
/ `</function_calls>` when the mis-closed field was the last one. So a
mis-close is normally rejected anyway, quoting a literal you never
typed. Read such a rejection as "I closed a block with the field name",
not as "my prose is wrong". A field-name tag that appears in prose
*without* a structural mis-close is just text: nothing is rejected and
nothing is corrupted. Do not add these tags to your own forbidden list;
do check the wrapper whenever the rejection names a literal you did not
write.

## Error codes

Tools return business failures as JSON in the result's text content
with a stable `error` code. The closed set lives in
`internal/mcp/tools/errors.go` as `ErrCode*` constants (40+ codes
across issues, deps, users, attachments, standing, themes, …). Do
not treat any short list in this guide as exhaustive — when you
need the current set, open that file or grep `ErrCode` there.

Any tool result whose text begins with `{"error":"..."}` is a
business failure — do NOT try to parse it as a success payload.
