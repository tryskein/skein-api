package projects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/assignee"
	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/eventbus"
	"skein-api/internal/search"
	"skein-api/internal/signals"
	"skein-api/internal/spaces"
	"skein-api/internal/status"
)

// pgUniqueViolation is the SQLSTATE class for a unique-constraint hit. We
// pattern-match on this in CreateProject / AddAlias / etc. to translate
// raw DB errors into business 422 responses.
const pgUniqueViolation = "23505"

// resolveAutoMatchThreshold is the bar for "single candidate at this
// relevance score = auto-match" (§spec A10). Below this we return all
// candidates so the user picks. The lexical relevance score (sk-60)
// boosts an exact id_prefix to ≥0.5 and an exact alias to ≥0.4, but
// those paths already short-circuit upstream as exact matches — so in
// practice a lone substring/prefix hit (score 0.2–0.3) stays a
// candidate and the user disambiguates, which is the intended UX.
const resolveAutoMatchThreshold = 0.8

// resolveMaxCandidates caps the candidate list returned to the client.
// Raised 5 → 10 (sk-60) so fuzzy/substring queries surface a wider pick
// list; the MCP tool description + REST docs say "up to 10".
const resolveMaxCandidates = 10

// Store wraps a *pgxpool.Pool and exposes typed methods for everything
// the R1.2 handlers need. It is goroutine-safe; the pool itself owns the
// connection lifecycle.
type Store struct {
	pool *pgxpool.Pool
	// apikeys is used by RemoveMember to cascade-revoke a deactivated
	// external user's API keys in the same transaction (sk-159). It needs
	// only the pool, so it is constructed here without widening NewStore's
	// signature — every existing caller keeps working unchanged.
	apikeys *apikeys.Store
	// prefixMinLength is SKEIN_PROJECT_PREFIX_MIN_LENGTH (0 = no minimum),
	// set by SetPrefixMinLength (sk-674.18 T025).
	prefixMinLength int
	// quotaPersonalProjects / quotaSpaceProjects are
	// SKEIN_QUOTA_PERSONAL_PROJECTS / SKEIN_QUOTA_SPACE_PROJECTS (nil = no
	// quota), set by SetProjectQuotas (sk-674.18 T026).
	quotaPersonalProjects *int
	quotaSpaceProjects    *int
}

// SetProjectQuotas sets the project quotas of the personal space and of
// every other space (the common one included); nil = no quota.
func (s *Store) SetProjectQuotas(personal, space *int) {
	s.quotaPersonalProjects, s.quotaSpaceProjects = personal, space
}

// ErrProjectQuota — sk-674.18 T026, INV-15, US-14: the space already holds
// its project quota (archived projects counted). Answered with
// spaces.CodeProjectQuota and these numbers, the same in REST and MCP.
type ErrProjectQuota struct {
	SpaceID      string // short id
	ProjectCount int
	ProjectQuota int
}

func (e *ErrProjectQuota) Error() string {
	return fmt.Sprintf("project quota of space %s reached: %d of %d", e.SpaceID, e.ProjectCount, e.ProjectQuota)
}

// SetPrefixMinLength sets SKEIN_PROJECT_PREFIX_MIN_LENGTH; nil = no minimum.
// Applies to new projects and to prefix suggestions.
func (s *Store) SetPrefixMinLength(n *int) {
	if n == nil {
		s.prefixMinLength = 0
		return
	}
	s.prefixMinLength = *n
}

// CheckPrefixRules validates a new project's prefix against the prefix rule
// (ValidateIDPrefix) and SKEIN_PROJECT_PREFIX_MIN_LENGTH, and returns its
// canonical form. Refusal: a ValidationError with CodePrefixInvalid.
func (s *Store) CheckPrefixRules(prefix string) (string, error) {
	p, err := ValidateIDPrefix(prefix)
	if err != nil {
		return "", err
	}
	if len(p) < s.prefixMinLength {
		return "", newValidationError(CodePrefixInvalid,
			fmt.Sprintf("id_prefix must be at least %d characters", s.prefixMinLength))
	}
	return p, nil
}

// NewStore returns a Store backed by the given pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, apikeys: apikeys.New(pool)}
}

// ----------------- Helpers -----------------

// readJITOff runs fn in a read-only transaction with the planner's JIT
// switched off for that transaction only (SET LOCAL), for the statements of
// the project list (sk-674.18 T009). The list's aggregate over projects ×
// issues carries planner cost estimates high enough to trigger the JIT's
// compile phases, which on a large measurement dataset cost an external
// member 1533 of 1544 ms for ~10 ms of execution. Off for the whole pool,
// JIT's absence slowed other hot paths, so it is scoped to this list.
func (s *Store) readJITOff(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL jit = off`); err != nil {
			return fmt.Errorf("disabling jit for the project list: %w", err)
		}
		return fn(tx)
	})
}

func isUniqueViolation(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		return pgErr.ConstraintName, true
	}
	return "", false
}

// ErrNotFound is returned by Get* methods when the row doesn't exist. It
// is distinct from ErrProjectNotVisible (access.go) which is the
// "exists-but-hidden" case.
var ErrNotFound = errors.New("not found")

// ErrConflict signals a uniqueness violation surfaced in business terms.
// The Constraint field carries the SQL constraint name so the handler can
// translate to a precise error code.
type ErrConflict struct{ Constraint string }

func (e *ErrConflict) Error() string { return "unique constraint violation: " + e.Constraint }

// ----------------- Projects -----------------

// spacesActor names actor for the membership primitives' events.
func spacesActor(a Actor) spaces.Actor {
	t := a.Role
	if t == "" {
		t = "human"
	}
	return spaces.Actor{UserID: a.UserID, Username: a.Username, SessionID: a.SessionID, Type: t}
}

// spaceRankExpr is the kind rank of the space of alias in the projects
// column order (K-20): personal 0, common 1, regular 2.
func spaceRankExpr(alias string) string {
	return `CASE ` + alias + `.kind WHEN 'personal' THEN 0 WHEN 'common' THEN 1 ELSE 2 END`
}

// spaceFilterExpr is the `?space=` filter: the projects of the space with
// the short id bound at placeholder. Whether the caller sees the space is
// the visibility filter's business — an unseen space has no visible
// project, so the list is empty, as for a nonexistent one.
func spaceFilterExpr(placeholder string) string {
	return `p.space_id = (SELECT f.id FROM spaces f WHERE f.short_id = ` + placeholder + `)`
}

// collectUUIDs runs a one-column uuid query in tx.
func collectUUIDs(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// projectSpaceCols renders the two space columns of a project row of
// alias: the short id of its space (the `spaceId` of every response — never
// the space's UUID, spaces/contract.go) and its visibility.
func projectSpaceCols(alias string) string {
	return `(SELECT sp.short_id FROM spaces sp WHERE sp.id = ` + alias + `.space_id), ` + alias + `.visibility`
}

// CreateProjectArgs is the input shape for CreateProject. id_prefix is
// canonical lowercase; name is the trimmed display form.
//
// Aliases and GitRepos are optional. When set, they're inserted in the
// same SERIALIZABLE transaction as the project row so a partial-write
// failure (alias collision, FK violation) rolls back the whole project
// and the caller never observes "project exists but the aliases I asked
// for are missing".
type CreateProjectArgs struct {
	Name          string
	IDPrefix      string
	Description   string
	DefaultIDMode string
	GitlabGroup   *string
	CreatorID     uuid.UUID
	CreatorRole   string
	CreatorName   string

	// Actor is the audit-trail actor written to events.actor / actor_type /
	// actor_session_id for the project.created event. The agent path
	// (MCP tools) passes a Role='agent' actor with SessionID populated;
	// the REST handler passes HumanActor(CreatorID, CreatorName).
	//
	// When zero-valued (Role == ""), CreateProject falls back to
	// HumanActor(CreatorID, CreatorName) so older callers that haven't
	// been updated yet keep producing a sensible audit row. New callers
	// should populate this explicitly.
	Actor Actor

	// Aliases are inserted into project_aliases atomically with the
	// project row. Each entry must already be canonical (validated /
	// lowercased by the caller). Empty slice → no aliases inserted.
	Aliases []string

	// AliasAddedByKind is the value written to project_aliases.added_by_kind
	// for every alias inserted alongside the project. Constrained to
	// {'human','agent','system'} by the DB CHECK. Defaults to "human"
	// when empty so existing callers keep working unchanged; the MCP
	// agent path passes "agent" explicitly.
	AliasAddedByKind string

	// GitRepos are appended to projects.git_repos[] atomically. Each
	// entry's ID is server-generated (uuidv7). Empty slice → no repos.
	GitRepos []AddRepoArgs

	// SpaceID is the space of the new project (sk-674.18 T024), chosen by
	// the caller through spaces.ChooseForProject; required.
	SpaceID uuid.UUID
	// Visibility is spaces.VisibilitySpace or spaces.VisibilityMembers; a
	// project of a "Personal" is always members (INV-8).
	Visibility string
}

// CreateProject inserts the project, bootstraps the creator as an owner in
// project_members, optionally appends git_repos[] and project_aliases
// rows, and writes a project.created event — all in one SERIALIZABLE
// transaction so a non-admin creator can never observe "project exists
// but I don't own it" partial state. Optional fields (Aliases, GitRepos)
// are inserted in the same tx — a collision in any of them rolls back
// the project itself.
func (s *Store) CreateProject(ctx context.Context, args CreateProjectArgs) (*ProjectDTO, error) {
	// sk-163 S3 PRIMARY app-gate (never-owner corollary): an external caller
	// cannot create a project, because CreateProject unconditionally
	// bootstraps the creator as project_members role='owner' — and an
	// external user is never an owner (spec §5). Reject up front with the
	// clean business error so the 000034 owner-trigger (which would otherwise
	// raise SK001 deep in the owner-bootstrap INSERT and surface as a 500)
	// never fires. is_external is read from ctx, matching how the rest of this
	// package derives the caller's external flag (access.go / resolve.go).
	if auth.IsExternalFrom(ctx) {
		return nil, ErrExternalCannotCreateProject
	}

	// sk-28 store-level backstop. Project name is also re-validated by
	// ValidateProjectName at handler boundaries but description is not,
	// so the guard runs here uniformly.
	if err := ValidateProse("name", args.Name); err != nil {
		return nil, err
	}
	if err := ValidateProse("description", args.Description); err != nil {
		return nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("generating project id: %w", err)
	}

	// Build initial git_repos jsonb from the caller-supplied list. We
	// generate uuidv7 ids for each entry up front so the INSERT carries
	// the full final shape and we don't need a follow-up UPDATE per repo.
	repos := make([]Repo, 0, len(args.GitRepos))
	for _, r := range args.GitRepos {
		rid, err := uuid.NewV7()
		if err != nil {
			return nil, fmt.Errorf("generating repo id: %w", err)
		}
		repos = append(repos, Repo{
			ID:            rid,
			Name:          r.Name,
			URL:           r.URL,
			DefaultBranch: r.DefaultBranch,
		})
	}
	reposJSON, err := json.Marshal(repos)
	if err != nil {
		return nil, fmt.Errorf("marshalling git_repos: %w", err)
	}

	if args.SpaceID == uuid.Nil {
		return nil, errors.New("projects: CreateProject without a space")
	}
	if args.Visibility != spaces.VisibilitySpace && args.Visibility != spaces.VisibilityMembers {
		return nil, fmt.Errorf("projects: invalid visibility %q", args.Visibility)
	}

	var dto ProjectDTO
	err = spaces.WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		// K-7: creating a project of a space takes the space's lock first —
		// a participant joining or leaving meanwhile is ordered with it, so
		// a `space` project gets exactly the participants of the moment.
		// The creator must still be a participant (he may have left since
		// the space was chosen).
		sp, err := spaces.LockSpace(ctx, tx, args.SpaceID)
		if err != nil {
			return err
		}
		var participant bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM space_members WHERE space_id = $1 AND user_id = $2)`,
			sp.ID, args.CreatorID).Scan(&participant); err != nil {
			return fmt.Errorf("reading the creator's participation: %w", err)
		}
		if !participant {
			return ErrNotSpaceMember
		}
		if sp.Kind == spaces.KindPersonal {
			args.Visibility = spaces.VisibilityMembers
		}
		// US-15: the prefix of a deleted project is never given again (the
		// unique index guards only the living ones).
		var reserved bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM reserved_prefixes WHERE prefix = lower($1))`,
			args.IDPrefix).Scan(&reserved); err != nil {
			return fmt.Errorf("reading reserved prefixes: %w", err)
		}
		if reserved {
			return ErrPrefixTaken
		}
		// INV-15: projects of the space, archived included, stay within the
		// quota — counted under the space's lock, so two creations for the
		// last slot are one after the other. A lowered quota leaves the
		// existing projects alone and refuses new ones (E-10).
		quota := s.quotaSpaceProjects
		if sp.Kind == spaces.KindPersonal {
			quota = s.quotaPersonalProjects
		}
		if quota != nil {
			var count int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM projects WHERE space_id = $1`, sp.ID).Scan(&count); err != nil {
				return fmt.Errorf("counting the space's projects: %w", err)
			}
			if count >= *quota {
				return &ErrProjectQuota{SpaceID: sp.ShortID, ProjectCount: count, ProjectQuota: *quota}
			}
		}
		// INSERT projects
		row := tx.QueryRow(ctx, `
INSERT INTO projects (
    id, name, id_prefix, default_id_mode, description,
    git_repos, gitlab_group, archived_at,
    created_by, created_at, updated_at, metadata, space_id, visibility
) VALUES ($1, $2, $3, $4, $5, $8::jsonb, $6, NULL, $7, now(), now(), '{}'::jsonb, $9, $10)
RETURNING id, name, id_prefix, default_id_mode, description,
          git_repos, gitlab_group, archived_at,
          created_by, created_at, updated_at, `+projectSpaceCols("projects")+``,
			id, args.Name, args.IDPrefix, args.DefaultIDMode, args.Description,
			args.GitlabGroup, args.CreatorID, string(reposJSON), args.SpaceID, args.Visibility,
		)
		var (
			gitReposBytes []byte
			archivedAt    *time.Time
		)
		if err := row.Scan(
			&dto.ID, &dto.Name, &dto.IDPrefix, &dto.DefaultIDMode, &dto.Description,
			&gitReposBytes, &dto.GitlabGroup, &archivedAt,
			&dto.CreatedBy, &dto.CreatedAt, &dto.UpdatedAt, &dto.SpaceID, &dto.Visibility,
		); err != nil {
			return err
		}
		dto.ArchivedAt = archivedAt
		dto.GitRepos = []Repo{}
		_ = json.Unmarshal(gitReposBytes, &dto.GitRepos)

		// Bootstrap owner membership. Admin creators also get an explicit
		// row — keeps ListMembers honest and lets a future /me/projects
		// query filter on project_members without a special-case for admin.
		if _, err := tx.Exec(ctx, `
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'owner', now(), $2)`,
			dto.ID, args.CreatorID,
		); err != nil {
			return fmt.Errorf("bootstrapping owner: %w", err)
		}
		// US-17, INV-7: a `space` project — every participant gets the
		// source "from space" (the creator's row keeps "manually").
		if args.Visibility == spaces.VisibilitySpace {
			if _, err := tx.Exec(ctx, `
INSERT INTO project_members (project_id, user_id, role, added_at, added_by, via_manual, via_space)
SELECT $1, sm.user_id, 'member', now(), $2, false, true FROM space_members sm WHERE sm.space_id = $3
ON CONFLICT (project_id, user_id) DO UPDATE SET via_space = true`, dto.ID, args.CreatorID, sp.ID); err != nil {
				return fmt.Errorf("adding the space source: %w", err)
			}
		}

		// Seed project_counters row so the first counter-mode issue can do
		// an atomic UPDATE...RETURNING without a conditional INSERT branch.
		// Using ON CONFLICT DO NOTHING keeps this idempotent if the row was
		// already backfilled by 000004 migration (new projects post-migrate
		// hit the empty branch; existing-project rows from the migration
		// short-circuit harmlessly).
		if _, err := tx.Exec(ctx, `
INSERT INTO project_counters (project_id, counter)
VALUES ($1, 0)
ON CONFLICT (project_id) DO NOTHING`,
			dto.ID,
		); err != nil {
			return fmt.Errorf("seeding project_counter: %w", err)
		}

		// Optional aliases. Each is its own INSERT so we can map a
		// uniqueness conflict back to the alias that collided. We don't
		// bother batching — alias counts on create are bounded (caller
		// validates < 16 in practice) and the readability win matters.
		// added_by_kind defaults to 'human' so older callers (REST path)
		// don't have to set it; the agent path passes 'agent' through
		// AliasAddedByKind.
		addedByKind := args.AliasAddedByKind
		if addedByKind == "" {
			addedByKind = "human"
		}
		for _, alias := range args.Aliases {
			if _, err := tx.Exec(ctx, `
INSERT INTO project_aliases (alias, project_id, added_by, added_by_kind, created_at)
VALUES ($1, $2, $3, $4, now())`,
				alias, dto.ID, args.CreatorID, addedByKind,
			); err != nil {
				return err
			}
		}

		// Event: project.created. payload is full new snapshot. We
		// deliberately do NOT emit per-alias / per-repo events on the
		// initial insert path — the timeline is cleaner with one
		// project.created event carrying the full state, and the watchers
		// pick up the same snapshot via new_data.
		//
		// args.Actor.Role is the canary for "caller populated Actor": MCP
		// tools pass Role='agent' with a SessionID, REST handlers pass
		// HumanActor(...) explicitly. The fallback here keeps any caller
		// that hasn't been migrated yet emitting a non-empty actor_type.
		actor := args.Actor
		if actor.Role == "" {
			actor = HumanActor(args.CreatorID, args.CreatorName)
		}
		if _, err := WriteProjectEvent(ctx, tx, EventProjectCreated, dto.ID,
			actor,
			nil, dto,
		); err != nil {
			return err
		}
		// Everyone of the space who sees the new project — its members, the
		// space's implicit owner (INV-4) — gains it: the hub joins its group.
		// One set query through the relation: the access
		// set is inlined and narrowed to this project per participant, not
		// evaluated whole once per participant as skein_project_visible is.
		seers, err := collectUUIDs(ctx, tx, `
SELECT sm.user_id FROM space_members sm
  JOIN LATERAL skein_project_access_set(sm.user_id) va ON va.project_id = $2
 WHERE sm.space_id = $1`,
			sp.ID, dto.ID)
		if err != nil {
			return fmt.Errorf("reading who sees the new project: %w", err)
		}
		for _, u := range seers {
			signals.Add(tx, u, signals.AreaProjects, signals.AreaLens, signals.AreaTasks)
			signals.AddAccessChange(tx, u, nil, []uuid.UUID{dto.ID}, nil)
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM project_members WHERE project_id = $1`, dto.ID).
			Scan(&dto.MembersCount); err != nil {
			return fmt.Errorf("counting the members: %w", err)
		}
		// sk-622.21 T017 (US-11): "Project created" — its members.
		return signalProjectMembers(ctx, tx, dto.ID, signals.AreaProjects)
	})
	if err != nil {
		var quota *ErrProjectQuota
		if errors.As(err, &quota) {
			spaces.ObserveQuotaRefusal(spaces.CodeProjectQuota)
			return nil, err
		}
		if name, ok := isUniqueViolation(err); ok {
			if strings.Contains(name, "id_prefix") {
				return nil, ErrPrefixTaken
			}
			return nil, &ErrConflict{Constraint: name}
		}
		// Backstop: if the PRIMARY app-gate above was somehow bypassed (e.g.
		// a future caller forgets to stash is_external in ctx), the 000034
		// owner-trigger raises SQLSTATE 'SK001' on the owner-bootstrap INSERT.
		// Translate it to the clean business sentinel so it never escapes as a
		// raw 500.
		if isExternalOwnerViolation(err) {
			return nil, ErrExternalCannotCreateProject
		}
		return nil, err
	}

	// Counts are zero for a brand-new project, except the member rows
	// counted in the transaction, plus whatever repos came in.
	dto.ComponentsCount = 0
	dto.ReposCount = len(repos)
	return &dto, nil
}

// GetProject fetches one project plus 3 cheap counts. Permission filter
// must be applied by the caller. Stats are filled with caller-agnostic
// totals (MyAssigned / MyCreated stay zero) — see GetProjectWithStats
// for the caller-scoped variant the new shell uses.
func (s *Store) GetProject(ctx context.Context, id uuid.UUID) (*ProjectDTO, error) {
	return s.GetProjectWithStats(ctx, id, uuid.Nil)
}

// GetProjectWithStats fetches one project plus 3 cheap counts plus the
// full ProjectStats breakdown. callerID = uuid.Nil leaves MyAssigned /
// MyCreated at zero (older callers that don't yet pass the actor down).
// All other status counts ignore the caller — they're project-wide.
func (s *Store) GetProjectWithStats(ctx context.Context, id, callerID uuid.UUID) (*ProjectDTO, error) {
	var dto ProjectDTO
	var (
		gitReposBytes []byte
		archivedAt    *time.Time
		lastActivity  *time.Time
	)
	// One query: project row + 3 counts + per-status breakdown +
	// caller-scoped my-load + last activity. The caller-scoped subqueries
	// short-circuit on `callerID = $2` IS NULL when `$2 = uuid.Nil`
	// — pgx renders uuid.Nil as the all-zero uuid which never matches a
	// real users.id so the COUNT collapses to zero for free.
	//
	// sk-423: Open / InProgress / Blocked count only blockers inside the
	// CALLER's horizon (authz.ReaderBlockedExpr, bound as $3/$4 from
	// the request context — not from callerID, which may be uuid.Nil here).
	// A blocker the caller cannot see must not move a count.
	hUnrestricted, hCaller, hErr := authz.FromContext(ctx).Bind(ctx, s.pool)
	if hErr != nil {
		return nil, hErr
	}
	hU, hC := "$3", "$4"
	row := s.pool.QueryRow(ctx, `
SELECT p.id, p.name, p.id_prefix, p.default_id_mode, p.description,
       p.git_repos, p.gitlab_group, p.archived_at,
       p.created_by, p.created_at, p.updated_at, `+projectSpaceCols("p")+`,
       (SELECT count(*) FROM project_members pm WHERE pm.project_id = p.id),
       (SELECT count(*) FROM components c WHERE c.project_id = p.id AND c.archived_at IS NULL),
       jsonb_array_length(p.git_repos),
       -- sk-31: open / in_progress exclude rows that surface as blocked
       -- via the dep graph, so the three counters stay disjoint (the UI
       -- shows them as separate pills and we don't want overlap).
       COALESCE((SELECT count(*) FROM issues i
                  WHERE i.project_id = p.id
                    AND i.status = 'open'
                    AND NOT `+authz.ReaderBlockedExpr("i", hU, hC)+`), 0),
       COALESCE((SELECT count(*) FROM issues i
                  WHERE i.project_id = p.id
                    AND i.status = 'in_progress'
                    AND NOT `+authz.ReaderBlockedExpr("i", hU, hC)+`), 0),
       -- Blocked: derived. status != closed AND has an open blocker.
       COALESCE((SELECT count(*) FROM issues i
                  WHERE i.project_id = p.id
                    AND `+status.ActiveWork("i")+`
                    AND `+authz.ReaderBlockedExpr("i", hU, hC)+`), 0),
       COALESCE((SELECT count(*) FROM issues i WHERE i.project_id = p.id AND i.status = 'deferred'), 0),
       COALESCE((SELECT count(*) FROM issues i WHERE i.project_id = p.id AND i.status = 'closed'), 0),
       -- sk-494: "closed without work" — closed with an outcome other than
       -- "done". ONE leg, word for word the same as in the epic progress
       -- (issues/store.go#epicSummaryNoWorkExpr): the closed leg plus one
       -- predicate, so it cannot diverge from the "closed" row
       -- above. Done is computed in Go as closed − noWork — subtracting is cheaper
       -- and more reliable than writing "done" as a second independent
       -- expression that a future edit would silently desync.
       COALESCE((SELECT count(*) FROM issues i
                  WHERE i.project_id = p.id
                    AND i.status = 'closed' AND i.close_outcome <> 'done'), 0),
       -- Standing: perpetual obligations. Surfaced as its OWN aggregate (sk-118.8,
       -- decision #4) — NOT folded into open / in_progress / blocked, which all
       -- exclude standing via status.ActiveWork. Its own pill on the board.
       COALESCE((SELECT count(*) FROM issues i WHERE i.project_id = p.id AND i.status = 'standing'), 0),
       COALESCE((SELECT count(*) FROM issues i WHERE i.project_id = p.id), 0),
       COALESCE((
         SELECT count(*) FROM issues i
         WHERE i.project_id = p.id
           AND `+assignee.MatchesCaller("i", "$2")+`
           AND `+status.ActiveWork("i")+`
       ), 0),
       COALESCE((SELECT count(*) FROM issues i WHERE i.project_id = p.id AND i.created_by = $2), 0),
       (SELECT max(updated_at) FROM issues i WHERE i.project_id = p.id)
  FROM projects p
 WHERE p.id = $1`, id, callerID, hUnrestricted, hCaller)
	err := row.Scan(
		&dto.ID, &dto.Name, &dto.IDPrefix, &dto.DefaultIDMode, &dto.Description,
		&gitReposBytes, &dto.GitlabGroup, &archivedAt,
		&dto.CreatedBy, &dto.CreatedAt, &dto.UpdatedAt, &dto.SpaceID, &dto.Visibility,
		&dto.MembersCount, &dto.ComponentsCount, &dto.ReposCount,
		&dto.Stats.Open, &dto.Stats.InProgress, &dto.Stats.Blocked,
		&dto.Stats.Deferred, &dto.Stats.Closed, &dto.Stats.NoWork,
		&dto.Stats.Standing, &dto.Stats.Total,
		&dto.Stats.MyAssigned, &dto.Stats.MyCreated,
		&lastActivity,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("getting project: %w", err)
	}
	dto.ArchivedAt = archivedAt
	// sk-494: "done" is derived by subtraction, not by a second query — see
	// ProjectStats.Done. Under INV-1 (000050: an outcome exists ⇔ the issue is closed)
	// noWork is a subset of closed, so the difference cannot go negative.
	dto.Stats.Done = dto.Stats.Closed - dto.Stats.NoWork
	dto.Stats.LastActivity = lastActivity
	dto.GitRepos = []Repo{}
	if len(gitReposBytes) > 0 {
		_ = json.Unmarshal(gitReposBytes, &dto.GitRepos)
	}
	return &dto, nil
}

// UpdateProjectArgs allows partial updates. Nil fields are skipped.
type UpdateProjectArgs struct {
	Name          *string
	Description   *string
	DefaultIDMode *string
	GitlabGroup   *string
	// Visibility switches the project between spaces.VisibilitySpace and
	// spaces.VisibilityMembers in the same transaction (sk-674.18 T024,
	// through spaces.SetVisibilityInTx, US-17). The caller validated the
	// value and the right (authz.ActionChangeVisibility).
	Visibility *string
}

// UpdateProject updates whatever fields are non-nil and writes a
// project.updated event with old/new diffs. Returns ErrNotFound if the
// project doesn't exist.
func (s *Store) UpdateProject(ctx context.Context, id uuid.UUID, args UpdateProjectArgs, actor Actor) (*ProjectDTO, error) {
	var dto ProjectDTO
	var oldDTO ProjectDTO

	err := spaces.WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		// K-7: a visibility change takes the space's lock before any row of
		// the project — the order every membership operation keeps.
		if args.Visibility != nil {
			if err := spaces.LockProjectSpace(ctx, tx, id); err != nil {
				return err
			}
		}
		// Snapshot pre-update for the event payload.
		old, err := s.fetchProjectInTx(ctx, tx, id)
		if err != nil {
			return err
		}
		oldDTO = *old

		row := tx.QueryRow(ctx, `
UPDATE projects
   SET name            = COALESCE($2, name),
       description     = COALESCE($3, description),
       default_id_mode = COALESCE($4, default_id_mode),
       gitlab_group    = COALESCE($5, gitlab_group),
       updated_at      = now()
 WHERE id = $1
RETURNING id, name, id_prefix, default_id_mode, description,
          git_repos, gitlab_group, archived_at,
          created_by, created_at, updated_at, `+projectSpaceCols("projects")+``,
			id, args.Name, args.Description, args.DefaultIDMode, args.GitlabGroup,
		)
		var (
			gitReposBytes []byte
			archivedAt    *time.Time
		)
		if err := row.Scan(
			&dto.ID, &dto.Name, &dto.IDPrefix, &dto.DefaultIDMode, &dto.Description,
			&gitReposBytes, &dto.GitlabGroup, &archivedAt,
			&dto.CreatedBy, &dto.CreatedAt, &dto.UpdatedAt, &dto.SpaceID, &dto.Visibility,
		); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		dto.ArchivedAt = archivedAt
		dto.GitRepos = []Repo{}
		_ = json.Unmarshal(gitReposBytes, &dto.GitRepos)

		// R1.5 search: reset embedding when name/description change so
		// the backfill worker re-indexes. id_prefix and gitlab_group
		// don't feed into BuildProjectEmbedText, so a pure metadata
		// patch leaves the vector valid.
		if (args.Name != nil && *args.Name != oldDTO.Name) ||
			(args.Description != nil && *args.Description != oldDTO.Description) {
			if err := search.ResetEmbedding(ctx, tx, "projects", "id", id); err != nil {
				return err
			}
		}

		// R3.2b hybrid event write: one project.updated parent (full diff
		// in payload.changes) plus N project.field_changed children. The
		// helper writes nothing when the registry walk yields no changes,
		// so a no-op PATCH leaves the events table untouched.
		changes := eventbus.DiffRegistry(ProjectPatchFields, &oldDTO, &args)
		if _, _, err := eventbus.WriteHybridUpdateEvents(
			ctx, tx,
			EventProjectUpdated, EventProjectFieldChanged,
			"project", id.String(), &id,
			eventbusActor(actor),
			changes,
			"",
		); err != nil {
			return err
		}
		if args.Visibility != nil {
			if err := spaces.SetVisibilityInTx(ctx, tx, id, *args.Visibility, spacesActor(actor)); err != nil {
				return err
			}
		}
		// sk-622.21 T017 (US-11): "Project changed" — members re-read the list.
		if len(changes) > 0 {
			return signalProjectMembers(ctx, tx, id, signals.AreaProjects)
		}
		return nil
	})
	if err != nil {
		if name, ok := isUniqueViolation(err); ok {
			return nil, &ErrConflict{Constraint: name}
		}
		return nil, err
	}

	// Re-fetch counts because the UPDATE didn't touch them.
	full, err := s.GetProject(ctx, id)
	if err != nil {
		return nil, err
	}
	return full, nil
}

// fetchProjectInTx is the in-transaction equivalent of GetProject sans
// counts (we don't need them for event payloads).
func (s *Store) fetchProjectInTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*ProjectDTO, error) {
	var dto ProjectDTO
	var (
		gitReposBytes []byte
		archivedAt    *time.Time
	)
	row := tx.QueryRow(ctx, `
SELECT id, name, id_prefix, default_id_mode, description,
       git_repos, gitlab_group, archived_at,
       created_by, created_at, updated_at, `+projectSpaceCols("projects")+`
  FROM projects WHERE id = $1`, id)
	err := row.Scan(
		&dto.ID, &dto.Name, &dto.IDPrefix, &dto.DefaultIDMode, &dto.Description,
		&gitReposBytes, &dto.GitlabGroup, &archivedAt,
		&dto.CreatedBy, &dto.CreatedAt, &dto.UpdatedAt, &dto.SpaceID, &dto.Visibility,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	dto.ArchivedAt = archivedAt
	dto.GitRepos = []Repo{}
	_ = json.Unmarshal(gitReposBytes, &dto.GitRepos)
	return &dto, nil
}

// ArchiveProject sets archived_at = now() if the project is currently
// active. Returns ErrAlreadyArchived (specific sentinel) if the project
// is already archived; ErrNotFound if it doesn't exist.
var ErrAlreadyArchived = errors.New("already archived")
var ErrNotArchived = errors.New("not archived")

func (s *Store) ArchiveProject(ctx context.Context, id uuid.UUID, actor Actor) error {
	return signals.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		old, err := s.fetchProjectInTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if old.ArchivedAt != nil {
			return ErrAlreadyArchived
		}
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `
UPDATE projects SET archived_at = $2, updated_at = $2 WHERE id = $1`, id, now); err != nil {
			return err
		}
		new := *old
		new.ArchivedAt = &now
		new.UpdatedAt = now
		if _, err = WriteProjectEvent(ctx, tx, EventProjectArchived, id, actor, old, new); err != nil {
			return err
		}
		// sk-622.21 T017 (US-11): "Project archived" — the archived
		// project's issues leave the members' unreviewed set.
		return signalProjectMembers(ctx, tx, id, signals.AreaProjects, signals.AreaUnreviewed)
	})
}

func (s *Store) UnarchiveProject(ctx context.Context, id uuid.UUID, actor Actor) error {
	return signals.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		old, err := s.fetchProjectInTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if old.ArchivedAt == nil {
			return ErrNotArchived
		}
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `
UPDATE projects SET archived_at = NULL, updated_at = $2 WHERE id = $1`, id, now); err != nil {
			return err
		}
		new := *old
		new.ArchivedAt = nil
		new.UpdatedAt = now
		if _, err = WriteProjectEvent(ctx, tx, EventProjectUnarchive, id, actor, old, new); err != nil {
			return err
		}
		// sk-622.21 T017 (US-11): "Project unarchived".
		return signalProjectMembers(ctx, tx, id, signals.AreaProjects, signals.AreaUnreviewed)
	})
}

// ListProjectsArgs is the filter set for the paginated list endpoint.
type ListProjectsArgs struct {
	CallerID uuid.UUID
	// sk-524 — THE LENS-COUNTER GATE IS GONE, and the CallerUsername fields
	// with it. The absence is the point of the change, not a cleanup.
	//
	// How it was. The gate was the name: an empty string switched the counters off. Since
	// sk-523 the comparison no longer goes by name — it pivots on
	// CallerID via assignee.MatchesCaller — but the name stayed in the SQL
	// as the sole expression `<ph>::text IS NOT NULL`, i.e. a
	// switch that was compared with nothing.
	//
	// What it cost. The MCP transport passes no name at all
	// (`auth.UsernameFrom` returns empty), so the same
	// user saw real numbers on the web and zeros through MCP. That is
	// not an "honest zero for an agent": `skein_prime` over the same transport
	// and under the same identity returns real myInProgress and readyForMe.
	// Two answers to one question about one person.
	//
	// Why the gate is removed entirely rather than rewritten on CallerID. It is INERT, and
	// this is proven by mutation, not deduced: `MatchesCaller` is an EXISTS over the
	// users row with cu.id = CallerID, and the "delegated" leg requires
	// created_by = CallerID. With a zero identifier both legs are empty
	// on their own, so the variant "the gate always passes" went through the whole
	// integration suite green. A condition that cannot change the
	// answer is not protection but dead weight.
	Archived string // 'false' (active), 'true' (archived only), 'all'
	// Lens narrows the row set to projects with at least one matching
	// issue under the active lens (sk-30). Values:
	//   ""         — no narrowing (default, full project list).
	//   "on-me"    — keep only projects with myOnMe(open|in_progress|
	//                blocked) > 0 (HAVING filter over the same FILTER
	//                expressions powering OnMe*Count).
	//   "delegated"— keep only projects with myOutbox(open|in_progress|
	//                blocked) > 0.
	// When the caller can't be resolved (CallerID is the nil UUID), the
	// underlying counts collapse to zero so the HAVING clause drops
	// every row — that matches the visible UI: with no caller identity
	// you can't have an outbox / on-me lens result.
	Lens   string
	Search string // optional, legacy trigram path (kept for compatibility)
	// Query is the full-text / semantic search input — when non-empty
	// the store switches from the alphabetic list path to a hybrid
	// search ordered by ts_rank_cd (FTS) with optional cosine-similarity
	// rerank when the embedder is available and FTS yielded <20 hits.
	// Matches against tsv_ru / tsv_en (websearch_to_tsquery), idPrefix
	// prefix, name ILIKE, and project_aliases. Empty Query keeps the
	// existing behaviour (alphabetic ORDER BY created_at DESC, id DESC).
	Query string
	// Cursor continues a previous page (K-20 order, ListCursor).
	Cursor *ListCursor
	// Space narrows the list to the projects of one space, by its short id
	// (sk-674.18 T024, `?space=`); a space the caller cannot see — or that
	// does not exist — gives an empty list, the same answer (INV-3).
	Space    string
	Limit    int
	MineOnly bool // for admin only — restrict to projects they own/member of
	// Embedder is the optional cosine-similarity embedder for the
	// semantic rerank layer. Caller passes search.Embedder (or any
	// QueryEmbedder); nil falls back to FTS-only. The store calls
	// Available() before EmbedSingle to short-circuit when Ollama is
	// down so a degraded embedder never blocks the list endpoint.
	Embedder QueryEmbedder
}

// QueryEmbedder is the minimal embedder interface for the semantic
// rerank layer of the search-aware list path. Mirrors the one in
// internal/dependencies — keeps this package free of any import on
// `search` so the dep graph stays acyclic. *search.Embedder satisfies
// the interface structurally.
type QueryEmbedder interface {
	EmbedSingle(ctx context.Context, text string) ([]float32, error)
	Available() bool
}

// searchSemanticThreshold is the cosine-similarity floor for a row to
// participate in the semantic rerank. Below this we treat the row as
// unrelated to the query and drop it. 0.6 matches the threshold the
// dependencies-package layer-2 duplicate detector uses for the same
// underlying halfvec/cosine distance against bge-m3 embeddings.
const searchSemanticThreshold = 0.6

// searchSemanticTopK is the maximum number of rows we let the semantic
// layer add on top of the FTS hits when FTS came back thin. Keeps the
// merged result bounded; with FTS pulling up to N=Limit+1 candidates,
// the union stays within ~2× Limit so the dedup pass is cheap.
const searchSemanticTopK = 20

// searchFTSThinThreshold is the FTS-result count below which we trigger
// the optional semantic rerank. If FTS yielded ≥ N hits we don't need
// to pay the embedder round-trip — the keyword path covered the query.
const searchFTSThinThreshold = 20

// ListProjectsDefaultLimit and ListProjectsMaxLimit are the page size this
// store applies when the caller names none, and the ceiling it clamps to.
//
// Exported because skein_project_list declares the same values in its schema
// (INV-5: the declared and the applied value must agree).
const (
	ListProjectsDefaultLimit = 50
	ListProjectsMaxLimit     = 100
)

// ListProjects executes a keyset-paginated SELECT with permission filter.
// Returns up to Limit+1 rows so the caller can detect "has more" without
// a second count query.
//
// When `args.Query` is non-empty the function switches to a search-aware
// path:
//
//  1. FTS layer — `tsv_ru @@ websearch_to_tsquery('russian')` UNION
//     `tsv_en @@ websearch_to_tsquery('english')` UNION idPrefix prefix
//     match UNION name ILIKE substring UNION project_aliases match.
//     Rank by ts_rank_cd over the merged ts_query; cursor pagination
//     uses `(score, id) < (cursor_score, cursor_id)` instead of
//     `(created_at, id)`.
//  2. Optional semantic rerank — if the embedder is available AND the
//     FTS layer returned fewer than searchFTSThinThreshold hits, embed
//     the query via Ollama and pull the top-K (searchSemanticTopK)
//     rows whose cosine similarity to the query vector is above
//     searchSemanticThreshold. Merge with the FTS hits, dedupe by id,
//     keep FTS rank as the primary sort key with semantic score as
//     tiebreaker. Embedder unavailability (ErrEmbedderUnavailable)
//     degrades cleanly to FTS-only.
//
// Total is computed via COUNT(*) with the same WHERE clause as the row
// query (permission + archived + optional search match). On a 13k-row
// projects table EXPLAIN ANALYZE shows ~2 ms — exact count is honest
// and free. When the row count grows past ~100k we'll revisit and fall
// back to a `pg_class.reltuples` estimate with a caveat.
func (s *Store) ListProjects(ctx context.Context, args ListProjectsArgs) (*ListResult, error) {
	if args.Limit < 1 {
		args.Limit = ListProjectsDefaultLimit
	}
	if args.Limit > ListProjectsMaxLimit {
		args.Limit = ListProjectsMaxLimit
	}

	q := strings.TrimSpace(args.Query)
	if q != "" {
		return s.listProjectsWithSearch(ctx, args, q)
	}
	return s.listProjectsAlphabetic(ctx, args)
}

// listProjectsAlphabetic is the no-query path: keyset-paginated by
// (created_at DESC, id DESC), plus the legacy trigram `Search` filter
// for backwards compatibility with older REST callers that haven't
// migrated to `Query` yet.
func (s *Store) listProjectsAlphabetic(ctx context.Context, args ListProjectsArgs) (*ListResult, error) {
	// Build the SQL incrementally because the search/cursor/archive
	// branches each add a clause. Numbered placeholders match the args
	// slice we accumulate alongside.
	var (
		clauses []string
		params  []any
	)
	addParam := func(v any) string {
		params = append(params, v)
		return fmt.Sprintf("$%d", len(params))
	}

	// Permission filter: the access relation for the caller (authz); with
	// MineOnly also an explicit membership — the caller's own projects,
	// which an admin's reach does not widen.
	_, visible, bindErr := authz.ScopeFor(ctx, args.CallerID).Bind(ctx, s.pool)
	if bindErr != nil {
		return nil, bindErr
	}
	clauses = append(clauses, authz.VisibleExpr("p.id", addParam(false), addParam(visible)))
	if args.MineOnly {
		clauses = append(clauses, authz.MemberExpr("p.id", addParam(args.CallerID)))
	}

	// Archived filter.
	switch args.Archived {
	case "true":
		clauses = append(clauses, "p.archived_at IS NOT NULL")
	case "all":
		// no filter
	default: // 'false' or empty
		clauses = append(clauses, "p.archived_at IS NULL")
	}

	if args.Space != "" {
		clauses = append(clauses, spaceFilterExpr(addParam(args.Space)))
	}

	// Legacy trigram Search filter (kept for older callers; the new
	// Query field uses the FTS path below).
	if q := strings.TrimSpace(args.Search); q != "" {
		qLower := strings.ToLower(q)
		if len(qLower) >= 2 {
			ph := addParam(qLower)
			clauses = append(clauses, fmt.Sprintf(
				"(p.name %% %s OR EXISTS (SELECT 1 FROM project_aliases a WHERE a.project_id = p.id AND a.alias %% %s))",
				ph, ph,
			))
		} else {
			ph := addParam(qLower + "%")
			clauses = append(clauses, fmt.Sprintf("lower(p.name) LIKE %s", ph))
		}
	}

	// Snapshot the where-without-cursor params for the COUNT query
	// before adding the cursor clause (count is unfiltered by cursor).
	countWhere := ""
	if len(clauses) > 0 {
		countWhere = "WHERE " + strings.Join(clauses, " AND ")
	}
	countParams := append([]any(nil), params...)

	// Capture the index of the next param we add — the count query for
	// a lens-narrowed list needs its own caller-id / caller-username
	// placeholders. countParams snapshot above contains only the
	// WHERE-clause params; we'll append the same caller args used by
	// the row query below for the HAVING reuse in countProjects when a
	// lens is active.

	// Cursor filter — the K-20 order (see ListCursor).
	if c := args.Cursor; c != nil {
		clauses = append(clauses, fmt.Sprintf(
			"(%s, lower(sp.name), sp.short_id, lower(p.name), p.id) > (%s::int, %s::text, %s::text, %s::text, %s::uuid)",
			spaceRankExpr("sp"), addParam(c.Rank), addParam(c.SpaceName), addParam(c.SpaceID),
			addParam(c.Name), addParam(c.ID),
		))
	}

	where := ""
	if len(clauses) > 0 {
		where = "WHERE " + strings.Join(clauses, " AND ")
	}

	// Limit+1 trick: fetch one extra row so we can tell if there's more.
	limitPh := addParam(args.Limit + 1)

	// My-assigned-mode (outbox) counts: see field comment in
	// ProjectListItem for the full algorithm.
	callerIDPh := addParam(args.CallerID)

	// sk-291 — the epics-eligible counter is scoped to the caller's
	// horizon, in lockstep with issues.EpicRollup.EligibleForClose.
	epicUnrestricted, epicCaller, hErr := authz.FromContext(ctx).Bind(ctx, s.pool)
	if hErr != nil {
		return nil, hErr
	}
	// sk-423: every «blocked» leg — and its open / in_progress complement,
	// so the pills stay disjoint and sum up — counts only blockers inside
	// the caller's horizon (authz.ReaderBlockedExpr), the same horizon
	// the epics-eligible counter uses. A blocker the caller cannot see must
	// not move a count: that would tell whether it is closed.
	hU, hC := addParam(epicUnrestricted), addParam(epicCaller)

	// Lens HAVING clause (sk-30, widened in sk-31): drop projects whose
	// lens-scoped counts are all zero. We re-inline the FILTER predicate
	// inside COUNT(*) FILTER (...) inside HAVING because PostgreSQL can't
	// reference SELECT-list aliases from HAVING. The predicate mirrors
	// the union of the three SELECT-list FILTERs (open + in_progress +
	// derived-blocked): a project with one matching deferred-but-blocked
	// row must survive the lens — otherwise the row pill renders `1
	// blocked` but the project gets filtered out, contradicting the
	// counts the UI is about to display.
	havingClause := buildLensHaving(args.Lens, callerIDPh, hU, hC)

	sql := fmt.Sprintf(`
SELECT p.id, p.name, p.id_prefix, p.default_id_mode, p.description,
       p.archived_at, p.created_at, p.updated_at,
       -- sk-31: open / in_progress exclude derived-blocked rows so the
       -- three pills stay disjoint at the project-row level. The same
       -- shape is mirrored in the my-outbox and on-me FILTER blocks
       -- below.
       COUNT(*) FILTER (
           WHERE i.status = 'open'
             AND NOT `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS open_count,
       COUNT(*) FILTER (
           WHERE i.status = 'in_progress'
             AND NOT `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS in_progress_count,
       -- sk-31: blocked is derived from dep graph at SELECT time.
       COUNT(*) FILTER (
           WHERE `+status.ActiveWork("i")+`
             AND `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS blocked_count,
       -- sk-118.8: standing is its own aggregate (decision #4). Unscoped,
       -- no readiness gate (standing never participates in blocked-derivation).
       COUNT(*) FILTER (WHERE i.status = 'standing') AS standing_count,
       -- sk-34: my_outbox_* counters honor the extended Delegated
       -- predicate (creator-or-chain-member, with sticky opt-out via
       -- dismissed_by). Mirrors store.go::ListIssues + list_search.go
       -- so the project-row counter, the search variant, and the
       -- cross-project list always agree on what's "in my Delegated".
       COUNT(*) FILTER (
           WHERE i.status = 'open'
             AND (
               (i.created_by = %[2]s
                 AND i.assignee IS NOT NULL AND NOT `+assignee.MatchesCaller("i", "%[2]s")+`)
               OR (%[2]s = ANY(i.previous_assignees)
                 AND NOT %[2]s = ANY(i.dismissed_by)
                 AND i.assignee IS NOT NULL AND NOT `+assignee.MatchesCaller("i", "%[2]s")+`)
             )
             AND NOT `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS my_outbox_open_count,
       COUNT(*) FILTER (
           WHERE i.status = 'in_progress'
             AND (
               (i.created_by = %[2]s
                 AND i.assignee IS NOT NULL AND NOT `+assignee.MatchesCaller("i", "%[2]s")+`)
               OR (%[2]s = ANY(i.previous_assignees)
                 AND NOT %[2]s = ANY(i.dismissed_by)
                 AND i.assignee IS NOT NULL AND NOT `+assignee.MatchesCaller("i", "%[2]s")+`)
             )
             AND NOT `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS my_outbox_in_progress_count,
       COUNT(*) FILTER (
           WHERE `+status.ActiveWork("i")+`
             AND (
               (i.created_by = %[2]s
                 AND i.assignee IS NOT NULL AND NOT `+assignee.MatchesCaller("i", "%[2]s")+`)
               OR (%[2]s = ANY(i.previous_assignees)
                 AND NOT %[2]s = ANY(i.dismissed_by)
                 AND i.assignee IS NOT NULL AND NOT `+assignee.MatchesCaller("i", "%[2]s")+`)
             )
             AND `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS my_outbox_blocked_count,
       -- sk-118.8: outbox-scoped standing pill — same Delegated predicate
       -- as my_outbox_open_count, narrowed to status='standing'.
       COUNT(*) FILTER (
           WHERE i.status = 'standing'
             AND (
               (i.created_by = %[2]s
                 AND i.assignee IS NOT NULL AND NOT `+assignee.MatchesCaller("i", "%[2]s")+`)
               OR (%[2]s = ANY(i.previous_assignees)
                 AND NOT %[2]s = ANY(i.dismissed_by)
                 AND i.assignee IS NOT NULL AND NOT `+assignee.MatchesCaller("i", "%[2]s")+`)
             )
       ) AS my_outbox_standing_count,
       COUNT(*) FILTER (
           WHERE i.status = 'open'
             AND i.created_by <> %[2]s
             AND i.accepted_at IS NOT NULL
             AND `+assignee.MatchesCaller("i", "%[2]s")+`
             AND NOT `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS on_me_open_count,
       COUNT(*) FILTER (
           WHERE i.status = 'in_progress'
             AND i.created_by <> %[2]s
             AND i.accepted_at IS NOT NULL
             AND `+assignee.MatchesCaller("i", "%[2]s")+`
             AND NOT `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS on_me_in_progress_count,
       COUNT(*) FILTER (
           WHERE `+status.ActiveWork("i")+`
             AND i.created_by <> %[2]s
             AND i.accepted_at IS NOT NULL
             AND `+assignee.MatchesCaller("i", "%[2]s")+`
             AND `+authz.ReaderBlockedExpr("i", hU, hC)+`
       ) AS on_me_blocked_count,
       %[5]s,
       `+activeComponentsCountSQL+`,
       sp.short_id, p.visibility, `+spaceRankExpr("sp")+`, lower(sp.name), sp.short_id, lower(p.name)
  FROM projects p
  JOIN spaces sp ON sp.id = p.space_id
  LEFT JOIN issues i ON i.project_id = p.id
  %[1]s
 GROUP BY p.id, sp.id
%[4]s
 ORDER BY `+spaceRankExpr("sp")+`, lower(sp.name), sp.short_id, lower(p.name), p.id
 LIMIT %[3]s`, where, callerIDPh, limitPh, havingClause,
		epicsEligibleCountExpr(addParam(epicUnrestricted), addParam(epicCaller)))

	items := make([]ProjectListItem, 0, args.Limit)
	keys := make([]ListCursor, 0, args.Limit)
	if err := s.readJITOff(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, params...)
		if err != nil {
			return fmt.Errorf("listing projects: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var item ProjectListItem
			var archivedAt *time.Time
			var key ListCursor
			if err := rows.Scan(
				&item.ID, &item.Name, &item.IDPrefix, &item.DefaultIDMode, &item.Description,
				&archivedAt, &item.CreatedAt, &item.UpdatedAt,
				&item.OpenCount, &item.InProgressCount, &item.BlockedCount, &item.StandingCount,
				&item.MyOutboxOpenCount, &item.MyOutboxInProgressCount, &item.MyOutboxBlockedCount,
				&item.MyOutboxStandingCount,
				&item.OnMeOpenCount, &item.OnMeInProgressCount, &item.OnMeBlockedCount,
				&item.EpicsEligibleCount,
				&item.ComponentsCount,
				&item.SpaceID, &item.Visibility, &key.Rank, &key.SpaceName, &key.SpaceID, &key.Name,
			); err != nil {
				return fmt.Errorf("scanning project row: %w", err)
			}
			item.ArchivedAt = archivedAt
			key.ID = item.ID
			items = append(items, item)
			keys = append(keys, key)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterating project rows: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	hasMore := false
	if len(items) > args.Limit {
		hasMore = true
		items = items[:args.Limit]
	}

	var nextCursor *string
	if hasMore && len(items) > 0 {
		c := EncodeListCursor(keys[len(items)-1])
		nextCursor = &c
	}

	total, err := s.countProjectsWithLens(ctx, countWhere, countParams, args.Lens, args.CallerID)
	if err != nil {
		return nil, err
	}

	return &ListResult{Items: items, Total: total, NextCursor: nextCursor, HasMore: hasMore}, nil
}

// countProjects runs `SELECT count(*) FROM projects p [WHERE ...]`
// against the same WHERE clause / params used by the row query (sans
// cursor). EXPLAIN ANALYZE on a 13k-row table shows ~2 ms.
func (s *Store) countProjects(ctx context.Context, where string, params []any) (int, error) {
	q := "SELECT count(*) FROM projects p " + where
	var n int
	if err := s.readJITOff(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, q, params...).Scan(&n)
	}); err != nil {
		return 0, fmt.Errorf("counting projects: %w", err)
	}
	return n, nil
}

// countProjectsWithLens is the lens-aware count. When `lens` is empty
// it delegates to countProjects (the cheap WHERE-only path).
// Otherwise it wraps the row query in a sub-select so the HAVING
// clause counts only projects with at least one matching issue under
// the lens.
//
// Why a subquery: a HAVING clause has to be paired with GROUP BY, and
// we don't want to GROUP-BY the entire projects table for the no-lens
// count (it would tank the 2ms cost). So the no-lens branch keeps the
// fast plain `SELECT count(*)`; the lens branch reproduces the row
// query's GROUP BY + HAVING and counts the surviving rows.
func (s *Store) countProjectsWithLens(ctx context.Context, where string, params []any, lens string, callerID uuid.UUID) (int, error) {
	if lens == "" {
		return s.countProjects(ctx, where, params)
	}
	// Append caller params (mirrors the row query).
	countParams := append([]any(nil), params...)
	addParam := func(v any) string {
		countParams = append(countParams, v)
		return fmt.Sprintf("$%d", len(countParams))
	}
	callerIDPh := addParam(callerID)
	// sk-423: the lens predicate's blocked leg is the reader's (see
	// buildLensHaving); bound here like the row query binds it.
	hUnrestricted, hCaller, hErr := authz.FromContext(ctx).Bind(ctx, s.pool)
	if hErr != nil {
		return 0, hErr
	}
	hU, hC := addParam(hUnrestricted), addParam(hCaller)
	having := buildLensHaving(lens, callerIDPh, hU, hC)

	q := fmt.Sprintf(`
SELECT count(*) FROM (
    SELECT p.id
      FROM projects p
      LEFT JOIN issues i ON i.project_id = p.id
      %s
     GROUP BY p.id
%s
) sub`, where, having)

	var n int
	if err := s.readJITOff(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, q, countParams...).Scan(&n)
	}); err != nil {
		return 0, fmt.Errorf("counting projects (lens=%s): %w", lens, err)
	}
	return n, nil
}

// activeComponentsCountSQL is the project-list row's componentsCount
// (sk-551.53): the number of NON-archived components of the project, the
// same subquery GetProject uses for ProjectDTO.ComponentsCount. It is a
// correlated scalar subquery rather than another LEFT JOIN because the
// list SELECT already joins issues and groups by p.id — a second join
// would multiply the issue aggregates. Served by the partial index
// idx_components_active (project_id) WHERE archived_at IS NULL
// (migration 000003). Every ListProjects path (plain, FTS search,
// semantic rerank) selects it right after epics_eligible_count.
const activeComponentsCountSQL = `(SELECT count(*) FROM components c
         WHERE c.project_id = p.id AND c.archived_at IS NULL) AS components_count`

// epicsEligibleCountExpr is the SELECT-list COUNT(*) FILTER fragment
// that surfaces ProjectListItem.EpicsEligibleCount (sk-32 slice 3):
// the number of epic-typed, non-closed issues in the project whose
// child set is non-empty AND every child is closed.
//
// sk-35: children are sourced from the parent-child dep graph (canonical
// hierarchy). The issues.parent_id column is frozen and would miss new
// children. The two EXISTS clauses share the same dep walk (NOT EXISTS
// any active child AND EXISTS at least one COUNTABLE child). Empty epics
// are excluded by the second EXISTS — they'd otherwise read as "ready to
// close" since no active child exists, which is a false signal.
//
// F2h (sk-118.9, §11.5): this fragment MUST produce the same boolean as
// issues.Store.epicRollupFor's EligibleForClose, which is derived as
// (Total > 0 AND Closed == Total) where Total = COUNT(status <> 'standing').
// `standing` children are INVISIBLE to the rollup — a standing-only epic has
// Total == 0 and is NOT eligible (it reads as an empty epic). The original
// child-presence EXISTS counted ANY child (including standing), so a
// standing-only epic was counted eligible here while epicRollupFor reported
// EligibleForClose=false — the two surfaces disagreed.
// The fix: the child-presence EXISTS now requires a NON-STANDING child
// (`ch.status <> 'standing'`), so the COUNTABLE child set matches Total.
// The closed-but-not-standing children still satisfy it (all-closed and
// closed+standing epics stay eligible); a standing-only / empty epic fails
// it (not eligible). The NOT EXISTS active-child leg is unchanged —
// status.ActiveWork already excludes standing, so a standing child never
// strands the close.
//
// sk-291: the same lockstep obligation is why this fragment now carries a
// visibility leg. issues.epicRollupFor stopped counting children the caller
// cannot see toward ClosedChildren, so an epic with an invisible child can
// no longer be EligibleForClose — it reports "cannot say". Without the
// matching NOT EXISTS here, the project row would keep calling that epic
// eligible while the epic's own card denied it, which is exactly the
// disagreement the F2h paragraph above was written about.
//
// The fragment references i.id (the GROUP BY'd issues row) — it lives
// alongside the other FILTER counters in the LEFT JOIN issues
// projection and reuses the same row scope. Kept as a single renderer so
// the alphabetic + FTS + semantic list paths splice the same expression
// instead of drifting independently.
func epicsEligibleCountExpr(unrestrictedPlaceholder, callerPlaceholder string) string {
	return `COUNT(*) FILTER (
    WHERE i.type = 'epic'
      AND ` + status.ActiveWork("i") + `
      AND NOT EXISTS (
        SELECT 1 FROM dependencies d
           JOIN issues ch ON ch.id = d.from_id
         WHERE d.to_id = i.id AND d.type = 'parent-child'
           AND ` + status.ActiveWork("ch") + `
      )
      AND EXISTS (
        SELECT 1 FROM dependencies d
           JOIN issues ch ON ch.id = d.from_id
         WHERE d.to_id = i.id AND d.type = 'parent-child'
           AND ch.status <> 'standing'
      )
      AND NOT EXISTS (
        SELECT 1 FROM dependencies d
           JOIN issues ch ON ch.id = d.from_id
         WHERE d.to_id = i.id AND d.type = 'parent-child'
           AND NOT ` + authz.ProjectVisibleExpr("ch", unrestrictedPlaceholder, callerPlaceholder) + `
      )
) AS epics_eligible_count`
}

// callerIDPh is a SQL placeholder token like "$5" already bound to the
// caller-id arg on the surrounding query. Returns "" for an unknown / empty lens — caller embeds the
// result with %s in the SQL template, so the empty case becomes a
// no-op.
//
// The HAVING predicate re-inlines the FILTER expressions used in the
// SELECT list because Postgres can't reference SELECT-list aliases
// from HAVING (only GROUP BY columns). It's slightly verbose but the
// alternative — wrapping the whole row query in another subquery —
// has worse plans on the optimizer's hot path.
//
// sk-31: the inner WHERE matches the UNION of open + in_progress +
// derived-blocked (status <> 'closed' AND EXISTS(<blocker>)). A
// deferred-and-blocked row is "blocked" per the row's blockedCount pill
// and must keep the project alive in the lens — restricting to
// status IN ('open','in_progress') would silently drop those projects
// while the row counts still claim `1 blocked`.
//
// sk-423: hU / hC bind the caller's horizon (authz.Scope.Args) and
// the blocked leg is authz.ReaderBlockedExpr — the rule of the row
// counters this clause mirrors. A deferred issue blocked only from outside
// the caller's horizon neither reads as blocked nor keeps the project in
// the lens: keeping it would say whether that hidden blocker is closed.
func buildLensHaving(lens, callerIDPh, hU, hC string) string {
	switch lens {
	case "on-me":
		return fmt.Sprintf(`HAVING (
    COUNT(*) FILTER (
        WHERE `+status.ActiveWork("i")+`
          AND i.created_by <> %[1]s
          AND i.accepted_at IS NOT NULL
          AND `+assignee.MatchesCaller("i", "%[1]s")+`
          AND (
            i.status IN ('open','in_progress')
            OR `+authz.ReaderBlockedExpr("i", hU, hC)+`
          )
    ) > 0
)`, callerIDPh)
	case "delegated":
		// sk-34: the project's "delegated" lens HAVING predicate uses
		// the extended Delegated definition (creator-or-chain-member,
		// with sticky opt-out via dismissed_by). Keeps the lens-
		// filter projection in lock-step with the per-row
		// my_outbox_* counters and the cross-project list path.
		return fmt.Sprintf(`HAVING (
    COUNT(*) FILTER (
        WHERE `+status.ActiveWork("i")+`
          AND (
            (i.created_by = %[1]s
              AND i.assignee IS NOT NULL AND NOT `+assignee.MatchesCaller("i", "%[1]s")+`)
            OR (%[1]s = ANY(i.previous_assignees)
              AND NOT %[1]s = ANY(i.dismissed_by)
              AND i.assignee IS NOT NULL AND NOT `+assignee.MatchesCaller("i", "%[1]s")+`)
          )
          AND (
            i.status IN ('open','in_progress')
            OR `+authz.ReaderBlockedExpr("i", hU, hC)+`
          )
    ) > 0
)`, callerIDPh)
	default:
		return ""
	}
}
