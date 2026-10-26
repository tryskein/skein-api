# skein-api

Go backend of Skein: REST `/api/v1`, MCP server `/mcp`, agent task-monitor API
`/agent/v1`, background workers. It is reached only through skein-web; it has no
public port of its own.

This repository is cloned into `skein/projects/skein-api/` of the umbrella
workspace ([tryskein/skein](https://github.com/tryskein/skein)). The umbrella
`CLAUDE.md` at the top of `skein/` holds the cross-service rules (no invention,
no hacks, one repository per change, 422 for business errors, `/healthz` and
`/ready`, `PORT`, no fallbacks for addresses and keys, logging, git tags, API
docs in the same commit). They are not repeated here.

## Stack

Go (version in `src/go.mod`), chi v5, `caarlos0/env/v11`, `log/slog`, pgx v5 +
pgxpool, pgvector-go, golang-migrate v4, sqlc, `mark3labs/mcp-go`, nats.go,
minio-go v7, webpush-go, prometheus/client_golang, swaggo/swag.

## Layout

```
src/
  cmd/api/                 main.go — config, pool, migrations, router (buildRouter), workers
                           mcp_tools.go — MCP tool registration
  internal/
    config/                env loading + validation (config.Load)
    handler/               REST, health, agent API, OAuth metadata handlers
    mcp/                   MCP server: tools, agent instructions, transport
    storage/               pgxpool, S3 client, queries/ (sqlc input), generated/ (sqlc output, committed)
    operator/              separate operator API (own port, own OpenAPI document)
    <domain>/              one package per business domain (issues, projects, users, …)
  migrations/              golang-migrate SQL, 000NNN_name.{up,down}.sql
infra/postgres/            Postgres image used by the umbrella stacks (pgvector + pg_trgm, vector)
infra/test-runner/         Go + python3 image for the integration harness
docs/env-reference.md      every environment variable
scripts/measure-*.sh       size of MCP tools/list and of tool responses
```

## Build and run

Inside the umbrella stack (from the top of `skein/`):

```bash
docker compose --env-file versions.env -f docker-compose.local.yml \
  -f docker-compose.local.build.yml up -d --build postgres skein-api
```

Standalone, against a Postgres that has the `pg_trgm` and `vector` extensions:

```bash
cd src && go build ./... && go vet ./...
DATABASE_URL='postgres://…' SKEIN_INTERNAL_SECRET='<16+ chars>' \
SKEIN_AGENT_PUBLIC_URL='http://localhost:5100' go run ./cmd/api/
```

Those three variables are required; the full list is in `docs/env-reference.md`.

## Rules for this repository

- **Migrations** are applied in-process at startup, before the router is built;
  a failed migration stops the process. Never edit a released migration — add
  the next `000NNN_name.{up,down}.sql`. Start each `.down.sql` with a header
  comment saying what the rollback drops: release planning relies on it (umbrella
  `DEPLOYMENT-NOTES.md`, section 2).
- **sqlc**: `src/internal/storage/generated/` is committed. After changing
  `src/migrations/*.up.sql` or `src/internal/storage/queries/*.sql`, run
  `cd src && sqlc generate` with sqlc 1.31.1 (the version in the headers of the
  generated files) and commit the result.
- **Configuration**: `docs/env-reference.md` is the only list of variables. Adding,
  renaming or removing a variable updates that file in the same commit.
- **API docs**: swag annotations on every handler. Generate the spec (output is
  git-ignored):
  `cd src && go tool swag init -g cmd/api/main.go -o docs/ --exclude internal/operator`
  and `go tool swag init -g server.go -d internal/operator --instanceName operator -o docs/operator`.
  The `tool github.com/swaggo/swag/cmd/swag` line in `go.mod` keeps the module
  requirement that the generated `src/docs/docs.go` needs; do not drop it.
- **Route list**: `buildRouter` in `src/cmd/api/main.go` is the authoritative
  list of routes.
- **Theme mirrors**: `internal/themes/schema.go#TokenSchema` mirrors
  `TOKEN_SECTIONS` in skein-frontend `src/lib/themeTokens.ts`;
  `internal/themes/roles.go#RetiredRoles` mirrors `LEGACY_TO_NEW` in
  `src/lib/themeMigration.ts`. After changing either side: update the mirror,
  regenerate the goldens deliberately
  (`SKEIN_UPDATE_TOKEN_CATALOGUE=1 go test ./internal/themes/ -run TestCatalogueGolden`,
  `SKEIN_UPDATE_RETIRED_ROLES=1 go test ./internal/themes/ -run TestRetiredRolesGolden`),
  re-check the role summary in `internal/mcp/tools/theme_instructions.md`, and run
  `./scripts/check-theme-mirror.sh` from the top of `skein/`. This repository's
  own CI cannot compare the two sides.

### Cost of MCP responses

An agent pays for every byte of an MCP tool response for the rest of its
session — the response stays in the transcript.

- Changing a tool's response or description: estimate the bytes an agent gets
  per call and per hour of normal work, including periodic fields and fields
  added by middleware. Measure with `scripts/measure-tools-list.sh [tool…]`
  (descriptors) and `scripts/measure-tool-responses.sh [tool…]` (responses; needs
  `TEST_DATABASE_URL`).
- A field the agent cannot turn into an action does not go into the response.
- Large texts (scripts, guides) are served by a separate tool on request, not
  embedded in responses.

## Testing

### Kinds

| Kind | Status | What it is here | Local command | Narrowed run | Where |
|---|---|---|---|---|---|
| unit | on | Go tests without a database (DB-backed tests `t.Skip` without `TEST_DATABASE_URL`) + stdlib `unittest` tests of the served monitor runner | `cd src && go test -count=1 ./...` and the Python loop below | `cd src && go test -count=1 -run '<regexp>' ./internal/<pkg>/...`; `python3 src/<path>_test.py` | `src/**/*_test.go`, `src/**/*_test.py` |
| integration | on | store, handler and MCP tests against the real migrated schema; NATS in-process, S3 stubbed | from the top of `skein/`: `./scripts/integration-test.sh` | `./scripts/integration-test.sh -p 1 ./internal/<pkg>/...` (add `-run '<regexp>'`) | same suite, gated by `TEST_DATABASE_URL` |
| e2e | via skein-frontend | none of its own; the product e2e suite of skein-frontend runs against a live stack built with this image | from the top of `skein/`: `./scripts/e2e-test.sh` | `./scripts/e2e-test.sh -- -g '<title>'` | `skein-frontend/browser/e2e/` |
| smoke (image contract) | not used | — | — | — | — |
| mutation | not used | — | — | — | — |
| source-reading checks | one permitted | routes of the **assembled** router under `/agent/v1` are GET/HEAD only; needed until it is decided that `/agent/v1` gets a write route | part of unit | `cd src && go test -count=1 -run TestAgentRoutes_ReadOnly ./cmd/api/` | `src/cmd/api/agent_routes_readonly_test.go` |
| artifact contract | not used | — | — | — | — |

Python runner tests (from the repository root):

```bash
(cd src && find . -name '*_test.py' | sort | while IFS= read -r f; do python3 "$f" || exit 1; done)
```

### Project specifics

- Unit and integration are one suite; `TEST_DATABASE_URL` separates them, not
  build tags.
- No other source-reading checks are added. Existing structural guards (users
  of `internal/astguard`) are left as they are — neither removed nor extended.
- Checks that are not tests but must stay green: `cd src && go vet ./...`,
  `cd src && gofmt -l .` (empty output), and the sqlc check
  `cd src && sqlc generate && git status --porcelain --untracked-files=all -- internal/storage/generated`
  (empty output).

### Prerequisites

- `TEST_DATABASE_URL` — without it DB-backed tests skip. The harness sets it.
  To use your own database instead of the harness, migrate it with
  `migrate -path src/migrations -database "$TEST_DATABASE_URL" up` (extensions
  `pg_trgm` and `vector` created first), then `cd src && go test -p 1 -count=1 ./...`.
- docker — needed for integration and e2e (the harnesses start the stack), not
  for unit.
- `python3` (stdlib only) — for the Python runner tests and for Go tests that
  execute the served runner; without it those parts skip.
- No secrets. The dev password `qwerty12` exists only in the local/test compose
  files.

### When what runs

| Event | unit | integration | e2e | smoke | mutation | other |
|---|---|---|---|---|---|---|
| pull request | `ci.yml` job `unit` | `ci.yml` job `integration` | not run | not used | not run | `ci.yml` jobs `lint`, `sqlc-generated`; `theme-mirror.yml` when `src/internal/themes/**` changes |
| push to `main` | same as pull request | same | not run | not used | not run | same |
| release tag `X.Y.Z` | `ci.yml`, called by `release.yml` | same | not run | not used | not run | `release.yml` publishes `ghcr.io/tryskein/skein-api` |
| tag `X.Y.Z-rc.N` | nothing | nothing | nothing | nothing | nothing | nothing is published |

Workflows live in `.github/workflows/`. `ci.yml` also runs on `workflow_call`.
`integration` uses a `pgvector/pgvector:pg18` service container, creates
`pg_trgm` and `vector`, applies `src/migrations` with golang-migrate and runs
`go test -v -p 1 -count=1 ./...`; a run with zero passed tests is red.
`release.yml` starts only for a tag matching `X.Y.Z`, checks that the tagged
commit is on `main`, runs `ci.yml`, builds `linux/amd64` and `linux/arm64`
natively, publishes the manifest (`X.Y.Z` and `latest`) from jobs bound to the
`release` environment, then creates the GitHub Release. `theme-mirror.yml`
checks out `tryskein/skein` and `tryskein/skein-frontend` (`main`) next to this
repository and runs `scripts/check-theme-mirror.sh` of the umbrella; it is not
part of the release.

### Pitfalls

- **`-p 1` is mandatory for integration.** The database is shared, and the
  `migrations` package rewinds the schema (`Dirty database version …`); without
  it other packages fail with `SQLSTATE 40001`. Any argument passed to the
  harness replaces its default `-p 1 ./...` entirely, so a narrowed run is always
  `./scripts/integration-test.sh -p 1 <packages>`.
- **`HOST_WORKSPACE` inside a devcontainer**: the daemon-side path of the
  `skein` directory (the test compose mounts `projects/skein-api/src` and
  `src/migrations` from there). Without it the migrate step fails with
  `error: first .: file does not exist`:
  `HOST_WORKSPACE=<daemon-side path of skein> ./scripts/integration-test.sh -p 1 <packages>`.
- The harness forces `-count=1` and `-v`; the go test cache does not see the
  database contents.
- A run in which zero top-level tests reached `--- PASS:` is red (`ZERO tests
  executed`). A partly skipped run is not caught — read the skip count it prints.
- Every harness run has its own compose project `skein-itest-<run id>` and
  network, so parallel runs do not collide. `docker-compose.test.yml` cannot be
  started without the script (it needs `SKEIN_ITEST_NETWORK`). A run killed with
  `kill -9` leaves its `skein-itest-*` project behind (`docker compose ls -a`);
  remove it only after making sure it is not someone else's live run.
- `SKEIN_TEST_PARALLEL_PKGS` does **not** control `-p`: it makes the
  `requireSerialPackages` guards skip their tests, losing coverage. Do not set
  it without a reason.
- `internal/outbox`: `resetOutbox()` is global. A red outbox test under `-p 1`
  is a real bug, not a flake.
- Python runner tests: each `*_test.py` runs on its own with `python3 <file>`
  (`unittest`, no pytest). A file counts only if it prints `Ran N tests` and N
  minus the `skipped=` count is at least 1. `-run` narrows only Go. The suite starts a fake
  HTTP server and the runner as subprocesses and takes tens of seconds.
- `scripts/measure-tool-responses.sh` needs a migrated database in
  `TEST_DATABASE_URL`; the integration harness does not forward its measurement
  switch, so the harness is not a way to run it.

### Kinds without automatic runs

- e2e and mutation are not run by the workflows.
- e2e for this image runs only through `./scripts/e2e-test.sh` from the top of
  `skein/`.
