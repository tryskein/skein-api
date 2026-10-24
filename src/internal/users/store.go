package users

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/accounts"
	"skein-api/internal/apikeys"
	"skein-api/internal/assignee"
	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/eventbus"
	"skein-api/internal/person"
	"skein-api/internal/signals"
	"skein-api/internal/spaces"
)

// pgUniqueViolation is the SQLSTATE class for "duplicate key value violates
// unique constraint" — emitted by Postgres when an email/username collides
// with an existing row. We pattern-match on this in CreateUser and
// UpdateUser to translate raw DB errors into typed sentinels handlers
// surface as 422 / business errors.
const pgUniqueViolation = "23505"

// pgSerializationFailure is the SQLSTATE class for SERIALIZABLE
// transaction conflicts. Postgres aborts one of two transactions whose
// observed reads can't be linearized; we surface it as ErrLastAdmin in
// UpdateUser/Deactivate because that is the only invariant the
// SERIALIZABLE level protects in those flows. See R3.1 W3.
const pgSerializationFailure = "40001"

// PasswordMinLen is the minimum password length per R3.1 plan Q-2 — 8
// chars. Enforced in CreateUser, UpdateUser, and ChangePassword. Below
// this we return ErrInvalidPassword without reaching the bcrypt path.
const PasswordMinLen = 8

// usernameRegex mirrors the DB CHECK on users.username. Re-validated at
// the application layer so handlers return validation errors before
// hitting the DB (and so we get a uniform message). One rule with
// registration (accounts.UsernameRegex, sk-674.18 T038).
var usernameRegex = accounts.UsernameRegex

// emailRegex is intentionally permissive — RFC 5322 is not worth
// implementing in a regex. We require something@something.something with
// no whitespace; the DB has a unique index on lower(email) so duplicate
// detection happens there. This is a sanity gate, not a deliverability
// validator. One shape with registration (accounts.EmailRegex).
var emailRegex = accounts.EmailRegex

// Sentinel errors. Wrap-friendly via errors.Is.
//
//   - ErrUserNotFound        — user id not in users table.
//   - ErrEmailTaken          — unique violation on lower(email).
//   - ErrUsernameTaken       — unique violation on lower(username).
//   - ErrInvalidPassword     — password failed validation (length/charset).
//   - ErrInvalidCredentials  — current password mismatch on self-service.
//   - ErrInvalidRole         — role outside {admin, user}.
//   - ErrLastAdmin           — would leave zero active admins.
//   - ErrValidation          — wrapped validation message (typed below).
var (
	ErrUserNotFound       = errors.New("user not found")
	ErrEmailTaken         = errors.New("email already in use")
	ErrUsernameTaken      = errors.New("username already in use")
	ErrInvalidPassword    = errors.New("password does not meet requirements")
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrInvalidRole        = errors.New("role must be 'admin' or 'user'")
	ErrLastAdmin          = errors.New("would leave zero active admins")

	// ErrExternalCannotBeAdmin — sk-163 S3 never-admin authz guard.
	// Returned by CreateUser / UpdateUser when an external account
	// (is_external=true) is created with, or patched to, role='admin'. An
	// external collaborator is permanently capped at role='user' (spec §5).
	// The same-table CHECK users_external_not_admin (migration 000033) would
	// reject the row regardless, but the app-level guard returns this clean,
	// security-specific business error BEFORE the write hits the constraint —
	// a raw 23514 surfacing as a 500 is unacceptable.
	ErrExternalCannotBeAdmin = errors.New("external user cannot be an admin")

	// ErrAdminRoleDisabled — sk-674.18 T037, INV-20, SC-006: the instance
	// admin role is disabled on this installation
	// (SKEIN_INSTANCE_ADMIN_ENABLED=false, instance_settings.admin_enabled);
	// no path assigns it — creating an account, editing one, an admin's
	// agent, SSO provisioning.
	ErrAdminRoleDisabled = errors.New("the instance admin role is disabled")

	// ErrExternalSSOConflict — sk-163 S5 reverse anti-takeover guard.
	// Returned by CreateUser when an external account (is_external=true) is
	// requested for an email that already belongs to a Keycloak SSO identity
	// (keycloak_sub IS NOT NULL). Allowing it would let the public password
	// surface take over an internal SSO identity — the mirror of the OIDC
	// auto-link the provisioner blocks in internal/auth/oidc.go (spec §8).
	// The unique index on lower(email) would reject the INSERT regardless,
	// but this explicit guard yields a clear, security-specific business
	// error instead of a generic "email already in use".
	ErrExternalSSOConflict = errors.New("external account cannot use an email already bound to SSO")

	// ErrLocalPasswordDisabled — sk-163 S4a. Returned by ChangePassword
	// (self-service) and UpdateUser (admin password set) when OIDC is active
	// (authMode "keycloak"/"hybrid") and the target is an INTERNAL account
	// that is not the break-glass admin. Setting/rotating a local password
	// for an internal user would let them bypass Keycloak, so both password
	// paths are closed for that class. External users (and the break-glass
	// admin) are exempt. In "local" mode this guard never fires — password
	// management is unchanged for everyone.
	//
	// Unlike the Login gate (which collapses into the generic
	// invalid-credentials shape to deny account-class enumeration), these
	// callers are already authenticated — the self-service user against their
	// own row, the admin against a row they can already read — so there is no
	// cross-account oracle to protect and a clear, distinct error is correct.
	ErrLocalPasswordDisabled = errors.New("local password management is disabled for this account")

	// ErrPasswordPwned — sk-303.20. Returned by ChangePassword when the new
	// password's SHA-1 hash was found in the Pwned Passwords k-anonymity
	// corpus (see internal/pwned). Only checked when Store.pwnedChecker is
	// non-nil (SKEIN_PWNED_PASSWORDS_ENABLED=true) and only against the
	// self-service change path — CreateUser/UpdateUser (admin-set passwords)
	// are out of scope for this task.
	ErrPasswordPwned = errors.New("password appears in a known data breach")

	// ErrPasswordTooLong — sk-303.20. Returned by validatePassword when the
	// candidate password exceeds bcrypt's structural 72-byte ceiling once
	// UTF-8 encoded. Checked BEFORE any bcrypt call so a long-but-otherwise-
	// valid password (easily reached with 64 Cyrillic or emoji characters)
	// surfaces as a clean 400, not an unhandled 500 from
	// bcrypt.ErrPasswordTooLong deep inside auth.Hash.
	ErrPasswordTooLong = errors.New("password exceeds 72 bytes when UTF-8 encoded")

	// ErrPasswordChangedConcurrently — sk-303.20 W1 remediation. Returned by
	// ChangePassword's final compare-and-swap UPDATE when the row's
	// password_hash no longer matches the hash loadPasswordState verified
	// `current` against — i.e. a second request rotated the password in the
	// window between our read and our write (two tabs, a concurrent
	// UpdateUser by an admin, a retried client). This is a genuine,
	// if rare, race — not a caller mistake and not grounds for a 500 — so it
	// gets its own sentinel instead of colliding with ErrInvalidCredentials
	// (which means "the supplied current password was wrong") or falling
	// through to an unhandled 500 (which is what a bare "0 rows affected"
	// would otherwise look like to the handler).
	ErrPasswordChangedConcurrently = errors.New("password was changed by another request; please try again")
)

// PwnedChecker abstracts the Pwned Passwords k-anonymity lookup so
// ChangePassword is testable without a real network call, and so the check
// can be switched off outright by leaving the interface value nil. This is
// the standard consumer-side-interface shape — defined here, where it's
// used, not in internal/pwned, where it's implemented (internal/pwned does
// not import internal/users, so there's no cycle either way; the shape
// just belongs with the caller per Go convention).
type PwnedChecker interface {
	// Check reports whether password's hash appears in the breach corpus.
	// A non-nil error means "unknown" (network/timeout/non-200) — the
	// caller MUST treat that as "not checked", never as "clean" or
	// "breached", and must not block the password change on it.
	Check(ctx context.Context, password string) (breached bool, err error)
}

// Store wraps the pool plus the bcrypt cost. Build once at startup; reuse
// across requests. apikeysStore is held so Deactivate can cascade
// RevokeAllForUser inside the same transaction (atomic guarantee — the
// keys are revoked iff the user is deactivated).
type Store struct {
	pool         *pgxpool.Pool
	bcryptCost   int
	apikeysStore *apikeys.Store

	// authMode mirrors config.Config.AuthMode ("local"/"keycloak"/"hybrid").
	// It is threaded in from config (NOT read from the env here) so the
	// password gate in ChangePassword / UpdateUser lines up with the Login
	// handler's gate — single source of truth at wiring time. sk-163 S4a.
	authMode string

	// breakGlassAdmin is the lowercased email of the single admin permitted
	// to keep a local password while OIDC is active. Empty disables it. See
	// config.Config.BreakGlassAdmin.
	breakGlassAdmin string

	// pwnedChecker — sk-303.20. nil means the Pwned Passwords check is
	// disabled (SKEIN_PWNED_PASSWORDS_ENABLED=false, the default): the
	// ChangePassword code path that consults it is skipped entirely, no
	// outbound call is ever attempted, and PwnedCheckEnabled() reports
	// false so the frontend never dials out for its own hint either.
	pwnedChecker PwnedChecker

	// spaceMemberLimit is SKEIN_SPACE_MEMBER_LIMIT (nil = none) — the seats
	// of the spaces an account is created into (sk-674.18 T037).
	spaceMemberLimit *int
}

// SetSpaceMemberLimit sets SKEIN_SPACE_MEMBER_LIMIT for the spaces named at
// account creation.
func (s *Store) SetSpaceMemberLimit(n *int) { s.spaceMemberLimit = n }

// adminRoleEnabled reads whether the instance admin role exists on this
// installation — instance_settings.admin_enabled, which the api writes from
// SKEIN_INSTANCE_ADMIN_ENABLED at startup (one source for INV-20).
func adminRoleEnabled(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}) (bool, error) {
	var on bool
	if err := q.QueryRow(ctx, `SELECT admin_enabled FROM instance_settings WHERE id = 1`).Scan(&on); err != nil {
		return false, fmt.Errorf("reading the admin role flag: %w", err)
	}
	return on, nil
}

// NewStore returns a Store bound to pool with the given bcrypt cost.
// authMode + breakGlassAdmin drive the sk-163 S4a local-password gate; pass
// "local"/"" for the legacy behaviour where every account may manage a
// password. pwnedChecker is sk-303.20's optional Pwned Passwords lookup —
// pass a nil interface value to disable the check outright (the default);
// callers MUST assign a nil PwnedChecker variable directly rather than a
// typed-nil *pwned.Client, or PwnedCheckEnabled() will incorrectly report
// true (a typed nil pointer boxed into a non-nil interface is itself
// non-nil — see cmd/api/main.go for the safe construction pattern).
func NewStore(pool *pgxpool.Pool, bcryptCost int, apikeysStore *apikeys.Store, authMode, breakGlassAdmin string, pwnedChecker PwnedChecker) *Store {
	return &Store{
		pool:            pool,
		bcryptCost:      bcryptCost,
		apikeysStore:    apikeysStore,
		authMode:        authMode,
		breakGlassAdmin: strings.ToLower(strings.TrimSpace(breakGlassAdmin)),
		pwnedChecker:    pwnedChecker,
	}
}

// PwnedCheckEnabled reports whether the Pwned Passwords k-anonymity check
// is active for this deployment. Backs GET /api/v1/me/password/policy —
// the single source of truth the frontend consults before attempting its
// own client-side hint lookup, so "do we call out to a third party" is
// decided once, on the server, not independently by each caller.
func (s *Store) PwnedCheckEnabled() bool { return s.pwnedChecker != nil }

// localPasswordAllowed reports whether a local password may be set/rotated
// for an account given the deployment auth mode and the account's class.
//   - local:           always (unchanged legacy behaviour).
//   - keycloak/hybrid:  only external accounts, plus the break-glass admin —
//     an internal user must authenticate via Keycloak, never a local
//     password they could set to bypass SSO.
//
// email must be the account's stored (lowercased) email.
func (s *Store) localPasswordAllowed(isExternal bool, email string) bool {
	if s.authMode == "local" {
		return true
	}
	if isExternal {
		return true
	}
	return s.breakGlassAdmin != "" && s.breakGlassAdmin == strings.ToLower(strings.TrimSpace(email))
}

// Pool returns the underlying pool — exposed for the MCP tool wiring
// where the tool needs to call shared helpers (UsersAgentActor) that
// take a pool.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// CreateUser inserts a new user. Validates email/username/password/role
// up front, hashes password with cfg.BcryptCost, generates a UUIDv7 id,
// and writes a user.created event in the same transaction. Returns
// ErrEmailTaken / ErrUsernameTaken on the unique-index collision branches.
//
// sk-163 S4a note: unlike ChangePassword / UpdateUser, this create path is
// intentionally NOT gated by localPasswordAllowed. The authentication
// boundary is the Login gate (handler/auth.go) — a password hash minted here
// on an internal account is DORMANT in hybrid/keycloak mode (passwordLoginAllowed
// rejects it), so this is not an auth bypass. Gating creation belongs to S8,
// which wires is_external through the create request and decides the
// admin-create-in-hybrid semantics; gating here now would prematurely constrain
// that behaviour. The shared ErrLocalPasswordDisabled mapping (user_create.go)
// covers the reachable skein_user(action="update") path.
func (s *Store) CreateUser(ctx context.Context, in CreateUserInput, actor Actor) (*UserDTO, error) {
	email := strings.TrimSpace(strings.ToLower(in.Email))
	username := strings.TrimSpace(strings.ToLower(in.Username))
	name := strings.TrimSpace(in.Name)

	if email == "" || !emailRegex.MatchString(email) {
		return nil, fmt.Errorf("%w: email must be a valid address", ErrValidation)
	}
	if username == "" {
		username = auth.UsernameFromEmail(email)
	}
	// INV-18: the shape of a deleted account's pseudonym (deleted-<8hex>)
	// is never a username anyone holds.
	if !accounts.ValidNewUsername(username) {
		return nil, fmt.Errorf("%w: username must match %s and not be of the form deleted-<8 hex>", ErrValidation, usernameRegex.String())
	}
	if name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrValidation)
	}
	if in.Role != "admin" && in.Role != "user" {
		return nil, ErrInvalidRole
	}
	// sk-163 S3 never-admin: an external account is permanently capped at
	// role='user'. Reject the admin request up front with a clean business
	// error rather than letting it reach the users_external_not_admin CHECK
	// (which would surface as a generic 500). The only valid role for an
	// external user is 'user', so a single rejection is sufficient — there is
	// nothing to "force" once admin is refused.
	if in.IsExternal && in.Role == "admin" {
		return nil, ErrExternalCannotBeAdmin
	}
	if err := validatePassword(in.Password); err != nil {
		return nil, err
	}

	// sk-163 S5 reverse anti-takeover guard: an external account must not be
	// created for an email already bound to a Keycloak SSO identity. We check
	// before the INSERT so the caller gets ErrExternalSSOConflict (a clear,
	// security-specific reason) rather than the generic ErrEmailTaken the
	// lower(email) unique index would otherwise produce. A plain local row
	// (keycloak_sub IS NULL) with the same email still falls through to the
	// INSERT and the normal ErrEmailTaken path — only SSO-bound rows trip
	// this guard. (Never-admin/never-owner authz for external users is S3.)
	if in.IsExternal {
		var ssoBound bool
		if err := s.pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM users WHERE LOWER(email) = $1 AND keycloak_sub IS NOT NULL)`,
			email).Scan(&ssoBound); err != nil {
			return nil, fmt.Errorf("checking sso email binding: %w", err)
		}
		if ssoBound {
			return nil, ErrExternalSSOConflict
		}
	}

	hash, err := auth.Hash(in.Password, s.bcryptCost)
	if err != nil {
		return nil, fmt.Errorf("hashing password: %w", err)
	}

	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("generating user id: %w", err)
	}

	var dto UserDTO
	// SERIALIZABLE and retried like every membership primitive (K-7): the
	// new account joins the common space under its row lock, and a
	// serialization conflict with a concurrent space operation is retried
	// exhausted retries are spaces.ErrConcurrentChange.
	err = spaces.WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		// sk-622.21 T017 (US-11): "User creation" — the issues whose
		// assignee text names the new account resolve differently after the
		// insert; their assignees before and after and their authors are
		// read on both sides of it.
		if in.Role == "admin" {
			on, err := adminRoleEnabled(ctx, tx)
			if err != nil {
				return err
			}
			if !on {
				return ErrAdminRoleDisabled
			}
		}
		// INV-18 (T041): the email and the username of a deleted account
		// are reserved — the admin assigns neither again.
		switch err := accounts.CheckIdentityFreeInTx(ctx, tx, email, username); {
		case errors.Is(err, accounts.ErrEmailTaken):
			return ErrEmailTaken
		case errors.Is(err, accounts.ErrUsernameTaken):
			return ErrUsernameTaken
		case err != nil:
			return err
		}
		before, err := assignee.HandleAddressees(ctx, tx, username, email, id.String())
		if err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, is_external, created_at, metadata)
VALUES ($1, $2, $3, $4, $5, $6, $7, now(), '{}'::jsonb)
RETURNING id, email, username, name, role, created_at, last_login_at, deactivated_at, is_external`,
			id, email, username, name, in.Role, hash, in.IsExternal,
		)
		var (
			lastLogin     *time.Time
			deactivatedAt *time.Time
		)
		if err := row.Scan(&dto.ID, &dto.Email, &dto.Username, &dto.Name, &dto.Role,
			&dto.CreatedAt, &lastLogin, &deactivatedAt, &dto.IsExternal); err != nil {
			return err
		}
		dto.LastLoginAt = lastLogin
		dto.DeactivatedAt = deactivatedAt
		// CreateUser always seeds password_hash and never touches
		// keycloak_sub — auth method is unambiguously "password".
		dto.AuthMethod = "password"
		if err := writeUserEvent(ctx, tx, EventUserCreated, dto.ID, actor, nil, dto); err != nil {
			return err
		}
		// sk-674.18 T020: "Personal" and, by the flag, the common space for
		// the new account (INV-8, INV-22); none for an external one.
		if err := accounts.ProvisionInTx(ctx, tx, dto.ID, accounts.Provision{
			EmailConfirmed: true, SkipCommon: in.SpaceIDs != nil,
		}); err != nil {
			return err
		}
		// T037: the admin's checkboxes — the spaces named, each joined at
		// once with its seats checked (an external account gets none).
		if in.SpaceIDs != nil {
			if err := spaces.JoinSpacesInTx(ctx, tx, dto.ID, in.SpaceIDs, s.spaceMemberLimit, spaces.Actor{
				UserID: actor.UserID, Username: actor.Username, SessionID: actor.SessionID, Type: actor.Role,
			}); err != nil {
				return err
			}
		}
		after, err := assignee.HandleAddressees(ctx, tx, username, email, id.String())
		if err != nil {
			return err
		}
		for _, uid := range append(before, after...) {
			signals.Add(tx, uid, signals.AreaInbox, signals.AreaLens, signals.AreaTasks)
		}
		return nil
	})
	if err != nil {
		if name, ok := isUniqueViolation(err); ok {
			switch {
			case strings.Contains(name, "email"):
				return nil, ErrEmailTaken
			case strings.Contains(name, "username"):
				return nil, ErrUsernameTaken
			}
		}
		return nil, err
	}
	return &dto, nil
}

// GetUser fetches a single user by id. Returns ErrUserNotFound when the
// row does not exist. Deactivated users are returned regardless — the
// admin reactivation flow needs to read them. Filtering is done at the
// list endpoint via ListUsersInput.Archived.
func (s *Store) GetUser(ctx context.Context, id uuid.UUID) (*UserDTO, error) {
	row := s.pool.QueryRow(ctx, `
SELECT id, email, username, name, role, created_at, last_login_at, deactivated_at, password_hash IS NOT NULL AS has_pwd, keycloak_sub IS NOT NULL AS has_kc, is_external, `+userStatusCol+`
  FROM users
 WHERE id = $1`, id)
	dto, err := scanUserDTO(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("getting user: %w", err)
	}
	return dto, nil
}

// GetUserByUsername fetches a single user by username. R4.3 — used by
// the public `/users/{username}` profile route. Returns ErrUserNotFound
// when the row does not exist. Deactivated users are still returned;
// the public profile handler decides whether to surface deactivated
// status to the viewer (admin only) — same convention as GetUser.
//
// Username is case-insensitive on the wire (mirror of users CHECK that
// stores it lowercased) — we lowercase the input before the lookup so
// callers don't have to.
func (s *Store) GetUserByUsername(ctx context.Context, username string) (*UserDTO, error) {
	row := s.pool.QueryRow(ctx, `
SELECT id, email, username, name, role, created_at, last_login_at, deactivated_at, password_hash IS NOT NULL AS has_pwd, keycloak_sub IS NOT NULL AS has_kc, is_external, `+userStatusCol+`
  FROM users
 WHERE LOWER(username) = LOWER($1)`, username)
	dto, err := scanUserDTO(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("getting user by username: %w", err)
	}
	return dto, nil
}

// Profile is a person's public profile as GetVisibleProfile reads it.
type Profile struct {
	ID            uuid.UUID
	Username      string
	Name          string
	Role          string
	Email         string
	DeactivatedAt *time.Time
	Status        person.Status
}

// GetVisibleProfile reads the profile of username if it is visible to
// viewer by INV-9 (authz.PersonVisibleExpr, sk-674.18 T027). A username
// that does not exist and one the viewer may not see are the same
// ErrUserNotFound, answered by the same single statement — neither the
// body nor the number of queries tells them apart.
func (s *Store) GetVisibleProfile(ctx context.Context, viewer uuid.UUID, username string) (*Profile, error) {
	var (
		p      Profile
		status *person.Status
	)
	err := s.pool.QueryRow(ctx, `
SELECT id, username, name, role, email, deactivated_at, `+person.StatusExpr("users")+`
  FROM users
 WHERE LOWER(username) = LOWER($1)
   AND `+authz.PersonVisibleExpr("$2::uuid", "users.id"), username, viewer).Scan(
		&p.ID, &p.Username, &p.Name, &p.Role, &p.Email, &p.DeactivatedAt, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("getting visible profile: %w", err)
	}
	if status != nil {
		p.Status = *status
	}
	return &p, nil
}

// escapeLikePattern neutralises the LIKE/ILIKE metacharacters in a
// user-supplied search term so that typing `_` or `%` in the directory
// search box matches those literal characters instead of "any character"
// / "anything". The result is meant to be embedded in a pattern that is
// executed with `ESCAPE '\'`.
//
// The backslash itself must be escaped FIRST — otherwise the backslashes
// this function inserts would themselves get doubled by the later passes.
func escapeLikePattern(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// listDirectory is the implementation behind ListDirectory — bounded, see
// the ListDirectory doc comment for the contract and the reasoning. The
// people listed are those visible to viewer by INV-9 (authz.PersonVisibleExpr,
// sk-674.18 T027): one rule for an internal and an external caller, the
// caller's own flag read from users by id.
func (s *Store) listDirectory(ctx context.Context, viewer uuid.UUID, in DirectoryInput) (*DirectoryResult, error) {
	limit := ClampDirectoryLimit(in.Limit)

	var params []any
	addParam := func(v any) string {
		params = append(params, v)
		return fmt.Sprintf("$%d", len(params))
	}

	clauses := []string{"deactivated_at IS NULL",
		authz.PersonVisibleExpr(addParam(viewer)+"::uuid", "users.id")}

	// Default (no search): plain alphabetical order, served straight out of
	// idx_users_active_username (migration 000037) without a sort step.
	orderBy := "username ASC"

	if q := strings.TrimSpace(in.Search); q != "" {
		lowered := strings.ToLower(q)
		escaped := escapeLikePattern(lowered)

		if DirectorySearchIsPrefix(in.Search) {
			// sk-285.5.5 review F5 — a 1–2 character term carries no complete
			// trigram, so the gin_trgm_ops indexes below cannot serve it and
			// the substring form degrades to a full scan of `users` (39 782
			// active rows in a test dataset: Seq Scan, shared hit=2910,
			// 23.18 ms). Those two keystrokes are the START of every search
			// the typeahead performs, so the unservable shape would be the
			// one that runs most often.
			//
			// Short terms therefore match by PREFIX, which a btree CAN serve
			// — but only through text_pattern_ops (migration 000038): this
			// database is en_US.utf8, and under a non-C collation the plain
			// `username` btree is ordered by collation rules that do not
			// match LIKE's byte-prefix semantics, so the planner will not use
			// it for `LIKE 'ad%'`. Same measurement, prefix form on the
			// pattern-ops indexes: BitmapOr, 13 buffers, 0.12 ms; the
			// broadest single letter ('u', 573 hits) 1.03 ms.
			//
			// LIKE, not ILIKE, on both columns, because ILIKE is not
			// index-servable either way: `username` is CHECK-constrained to
			// lowercase (000001_init) and the term is lowercased above, so a
			// case-sensitive compare is exact; `name` is free-form, so it is
			// matched through the lower(name) expression index instead.
			//
			// This narrows the semantics for short terms from "contains" to
			// "starts with" — deliberately. With one or two characters typed
			// "starts with" is what a person means, and "contains" over a
			// large org returns noise the reader has to type past anyway.
			// Three characters and up keep full substring matching.
			prefixPh := addParam(escaped + "%")
			clauses = append(clauses, fmt.Sprintf(
				`(username LIKE %s ESCAPE '\' OR lower(name) LIKE %s ESCAPE '\')`,
				prefixPh, prefixPh,
			))
			// Same ranking INTENT as the substring branch — username hits
			// ahead of display-name-only hits, username ASC inside each
			// bucket — minus the exact-username tier, which cannot fire
			// here: `username` is CHECK-constrained to at least three
			// characters (000001_init) and this branch only runs for terms
			// shorter than that, so no username can equal the term. A dead
			// tier in the CASE would read as a rule someone must maintain.
			orderBy = fmt.Sprintf(`CASE
           WHEN username LIKE %s ESCAPE '\' THEN 0
           ELSE 1
         END ASC, username ASC`, prefixPh)
			limitPh := addParam(limit + 1)
			return s.runDirectoryQuery(ctx, clauses, orderBy, limitPh, params, limit)
		}

		// ILIKE (not `lower(col) LIKE`) so the gin_trgm_ops indexes added in
		// migration 000037 are usable — pg_trgm supports ILIKE directly, but
		// it cannot serve a `lower(col)` expression against an index built on
		// the bare column.
		containsPh := addParam("%" + escaped + "%")
		clauses = append(clauses, fmt.Sprintf(
			`(username ILIKE %s ESCAPE '\' OR name ILIKE %s ESCAPE '\')`,
			containsPh, containsPh,
		))

		// Relevance ranking ahead of the alphabetical tiebreak. This is NOT
		// cosmetic: the result set is now cut at `limit`, and a pure
		// `ORDER BY username` cut can drop the very row the user is typing
		// towards. Searching "bob" over an org that also holds "aabob",
		// "abbob", … pushes the exact "bob" past the cut because those sort
		// earlier alphabetically. Exact username first, then username-prefix,
		// then name-prefix, then any other substring hit; username ASC keeps
		// the order stable and deterministic inside every rank bucket.
		//
		// `username` is CHECK-constrained to lowercase (000001_init), so the
		// exact-match test needs no lower() on the search term beyond the
		// normalisation already done above.
		exactPh := addParam(lowered)
		prefixPh := addParam(escaped + "%")
		orderBy = fmt.Sprintf(`CASE
           WHEN username = %s THEN 0
           WHEN username ILIKE %s ESCAPE '\' THEN 1
           WHEN name ILIKE %s ESCAPE '\' THEN 2
           ELSE 3
         END ASC, username ASC`, exactPh, prefixPh, prefixPh)
	}

	// limit+1 — one probe row beyond the page so HasMore is known without a
	// second COUNT(*) over the same predicate (the ListUsers convention).
	limitPh := addParam(limit + 1)

	return s.runDirectoryQuery(ctx, clauses, orderBy, limitPh, params, limit)
}

// runDirectoryQuery assembles and runs the directory SELECT shared by both
// search branches of listDirectory (substring and short-term prefix) and
// by the unsearched default. `limitPh` is the placeholder the caller has
// already bound to limit+1 (the probe row); `limit` is the page size the
// result is cut back to.
func (s *Store) runDirectoryQuery(
	ctx context.Context,
	clauses []string,
	orderBy string,
	limitPh string,
	params []any,
	limit int,
) (*DirectoryResult, error) {
	sql := fmt.Sprintf(`
SELECT id, username, name, `+person.StatusExpr("users")+`
  FROM users
 WHERE %s
 ORDER BY %s
 LIMIT %s`, strings.Join(clauses, " AND "), orderBy, limitPh)

	rows, err := s.pool.Query(ctx, sql, params...)
	if err != nil {
		return nil, fmt.Errorf("listing user directory: %w", err)
	}
	defer rows.Close()

	items := make([]UserDirectoryEntry, 0, limit+1)
	for rows.Next() {
		var e UserDirectoryEntry
		var status *person.Status
		if scanErr := rows.Scan(&e.ID, &e.Username, &e.Name, &status); scanErr != nil {
			return nil, fmt.Errorf("scanning user directory row: %w", scanErr)
		}
		if status != nil {
			e.Status = *status
		}
		items = append(items, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating user directory rows: %w", err)
	}

	hasMore := false
	if len(items) > limit {
		hasMore = true
		items = items[:limit]
	}
	return &DirectoryResult{Items: items, HasMore: hasMore}, nil
}

// ListDirectory returns a lean id/username/name page of ACTIVE
// (deactivated_at IS NULL) users. Backs the non-admin
// GET /api/v1/users/directory assignee-picker source.
//
// sk-285.5.5 — this used to return EVERY active user with no LIMIT and no
// search. That held while the original comment's premise held ("the org is
// small, tens to low-hundreds of users"); it stopped holding as soon as an
// install grew past that. Measured on a test dataset of 39 782 active
// users the unbounded query produced a ~4.3 MB JSON body and a 2 904-buffer
// seq scan + full sort on EVERY assignee-picker mount. It is now a bounded
// page (DirectoryDefaultLimit, capped at DirectoryMaxLimit) with an
// optional case-insensitive search over username AND name, so the picker
// searches the server instead of downloading the org. The search matches
// by substring from DirectoryTrigramMinLen characters up and by prefix
// below it — see the branch in listDirectory.
//
// sk-674.18 T027 (INV-9): the page holds only the people visible to viewer
// — participants of the viewer's spaces and external members of projects
// the viewer can access; an external viewer sees the co-members of his
// projects. Only the id/username/name triple is exposed (no email, INV-27).
func (s *Store) ListDirectory(ctx context.Context, viewer uuid.UUID, in DirectoryInput) (*DirectoryResult, error) {
	return s.listDirectory(ctx, viewer, in)
}

// ListUsers executes a keyset-paginated SELECT with optional archived
// filter and search (ILIKE on email/username/name). Returns up to Limit+1
// rows so the caller can detect "has more" without a second count query.
func (s *Store) ListUsers(ctx context.Context, in ListUsersInput) (*ListUsersResult, error) {
	if in.Limit < 1 {
		in.Limit = 50
	}
	if in.Limit > 100 {
		in.Limit = 100
	}

	var (
		clauses []string
		params  []any
	)
	addParam := func(v any) string {
		params = append(params, v)
		return fmt.Sprintf("$%d", len(params))
	}

	switch in.Archived {
	case "true":
		clauses = append(clauses, "deactivated_at IS NOT NULL")
	case "all":
		// no filter — both active and deactivated.
	default: // "false" or empty
		clauses = append(clauses, "deactivated_at IS NULL")
	}

	if q := strings.TrimSpace(in.Search); q != "" {
		ph := addParam("%" + strings.ToLower(q) + "%")
		clauses = append(clauses, fmt.Sprintf(
			"(lower(email) LIKE %s OR lower(username) LIKE %s OR lower(name) LIKE %s)",
			ph, ph, ph,
		))
	}

	// sk-163 S8 — optional external filter, applied server-side so it is
	// correct across every cursor page (a client-side filter over page 1
	// would silently hide matching rows beyond the first 100). Nil → no
	// filter (both classes returned).
	if in.External != nil {
		clauses = append(clauses, fmt.Sprintf("is_external = %s", addParam(*in.External)))
	}

	if in.CursorTime != nil && in.CursorID != nil {
		tPh := addParam(*in.CursorTime)
		idPh := addParam(*in.CursorID)
		clauses = append(clauses, fmt.Sprintf(
			"(created_at, id) < (%s, %s)", tPh, idPh,
		))
	}

	where := ""
	if len(clauses) > 0 {
		where = "WHERE " + strings.Join(clauses, " AND ")
	}

	limitPh := addParam(in.Limit + 1)
	sql := fmt.Sprintf(`
SELECT id, email, username, name, role, created_at, last_login_at, deactivated_at, password_hash IS NOT NULL AS has_pwd, keycloak_sub IS NOT NULL AS has_kc, is_external, `+userStatusCol+`
  FROM users
  %s
 ORDER BY created_at DESC, id DESC
 LIMIT %s`, where, limitPh)

	rows, err := s.pool.Query(ctx, sql, params...)
	if err != nil {
		return nil, fmt.Errorf("listing users: %w", err)
	}
	defer rows.Close()

	items := make([]UserDTO, 0, in.Limit)
	for rows.Next() {
		dto, scanErr := scanUserDTO(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scanning user row: %w", scanErr)
		}
		items = append(items, *dto)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating user rows: %w", err)
	}

	hasMore := false
	if len(items) > in.Limit {
		hasMore = true
		items = items[:in.Limit]
	}
	var nextCursor *string
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		c := encodeCursor(last.CreatedAt, last.ID)
		nextCursor = &c
	}
	return &ListUsersResult{Items: items, NextCursor: nextCursor, HasMore: hasMore}, nil
}

// UpdateUser applies a partial patch to the user. Role transitions
// admin → user run a SERIALIZABLE re-check of count(active admins) > 1
// inside the transaction so a concurrent update cannot race us into a
// zero-admin state. Password rotation re-hashes with cfg.BcryptCost.
//
// Returns ErrUserNotFound, ErrLastAdmin, ErrUsernameTaken, ErrInvalidRole,
// or ErrInvalidPassword as appropriate.
func (s *Store) UpdateUser(ctx context.Context, id uuid.UUID, in UpdateUserInput, actor Actor) (*UserDTO, error) {
	if in.Role != nil && *in.Role != "admin" && *in.Role != "user" {
		return nil, ErrInvalidRole
	}
	if in.Password != nil {
		if err := validatePassword(*in.Password); err != nil {
			return nil, err
		}
	}

	var newHash *string
	if in.Password != nil {
		h, err := auth.Hash(*in.Password, s.bcryptCost)
		if err != nil {
			return nil, fmt.Errorf("hashing password: %w", err)
		}
		newHash = &h
	}

	var dto UserDTO
	// SERIALIZABLE and retried like every membership primitive (K-7,
	// sk-731): the last-admin recheck below runs inside the transaction,
	// so a serialization abort is retried and the retry re-reads the admin
	// count — the loser of a race either applies or meets the real
	// ErrLastAdmin. Exhausted retries are spaces.ErrConcurrentChange.
	err := spaces.WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		old, err := loadUserInTx(ctx, tx, id)
		if err != nil {
			return err
		}

		// is_external is not on UserDTO — load once for both the
		// password-gate and never-admin checks (same SERIALIZABLE snapshot).
		needExternal := newHash != nil || (in.Role != nil && *in.Role == "admin")
		var isExternal bool
		if needExternal {
			if err := tx.QueryRow(ctx,
				`SELECT is_external FROM users WHERE id = $1`, id).Scan(&isExternal); err != nil {
				return err
			}
		}

		// sk-163 S4a — when OIDC is active, an admin may not set a local
		// password on an INTERNAL user (that would hand them a Keycloak
		// bypass). External users and the break-glass admin are exempt; in
		// local mode this never fires.
		if newHash != nil {
			if !s.localPasswordAllowed(isExternal, old.Email) {
				return ErrLocalPasswordDisabled
			}
		}

		// sk-163 S3 never-admin: promoting an external user to admin is
		// forbidden. UpdateUser does not currently mutate is_external (that
		// admin-facing wiring is S8), so the guard reads the stored flag — an
		// external user can never become admin via this patch.
		if in.Role != nil && *in.Role == "admin" {
			if isExternal {
				return ErrExternalCannotBeAdmin
			}
			// INV-20: no path assigns the admin role while it is disabled.
			if old.Role != "admin" {
				on, err := adminRoleEnabled(ctx, tx)
				if err != nil {
					return err
				}
				if !on {
					return ErrAdminRoleDisabled
				}
			}
		}
		// E-14, row 45: the projects the user sees before a demotion — after
		// it, those he saw only by the admin's reach are lost (INV-12).
		var seenBefore []uuid.UUID
		if in.Role != nil && *in.Role != "admin" && old.Role == "admin" {
			rows, err := tx.Query(ctx, `SELECT project_id FROM skein_project_access_set($1::uuid)`, id)
			if err != nil {
				return fmt.Errorf("reading the projects before the demotion: %w", err)
			}
			seenBefore, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
			if err != nil {
				return fmt.Errorf("reading the projects before the demotion: %w", err)
			}
		}

		// S8 NOTE: when is_external becomes mutable through this patch (the
		// admin conversion UI), this method gains two DB error surfaces that
		// MUST be mapped to clean business errors here (not bubbled as 500):
		//   - flipping is_external→true on a user who owns a project trips the
		//     000034 owner trigger (SQLSTATE 'SK001');
		//   - setting is_external=true on a role='admin' row trips the 000033
		//     users_external_not_admin CHECK (SQLSTATE 23514).
		// Add isExternalOwnerViolation / check-violation remaps in the outer
		// error handler below at that time. Today neither column is mutated
		// here, so both codes are unreachable via this path and mapping them
		// now would be dead code.

		// Last-admin protection: if we're demoting an active admin, the
		// re-check must run inside the SAME tx so a concurrent demote of
		// the only other admin can't slip in between our outer check and
		// our COMMIT.
		if in.Role != nil && *in.Role != "admin" && old.Role == "admin" && old.DeactivatedAt == nil {
			adminCount, err := countActiveAdminsInTx(ctx, tx)
			if err != nil {
				return err
			}
			if adminCount <= 1 {
				return ErrLastAdmin
			}
		}

		// Build SET clause from non-nil fields. We use COALESCE so the SQL
		// stays a single statement; the WHERE id = $1 is fine because we
		// already loaded `old` and confirmed existence.
		row := tx.QueryRow(ctx, `
UPDATE users
   SET name          = COALESCE($2, name),
       role          = COALESCE($3, role),
       password_hash = COALESCE($4, password_hash)
 WHERE id = $1
RETURNING id, email, username, name, role, created_at, last_login_at, deactivated_at,
          password_hash IS NOT NULL AS has_pwd, keycloak_sub IS NOT NULL AS has_kc, is_external, `+userStatusCol+``,
			id, in.Name, in.Role, newHash,
		)
		newDTO, err := scanUserDTO(row)
		if err != nil {
			return err
		}
		dto = *newDTO

		// R3.2b hybrid + lifecycle "coexist" (D-1):
		//   - Name / Role changes → user.updated parent + N user.field_changed
		//     children (full diff in payload.changes).
		//   - Password rotation → user.password_changed lifecycle, regardless
		//     of whether name/role also changed in the same call.
		// Both paths can fire in the same tx for a mixed PATCH so the audit
		// trail covers every dimension of the mutation. new_data on the
		// lifecycle row deliberately does NOT carry the password hash —
		// writeUserEvent serialises the DTO which has no hash field.
		changes := eventbus.DiffRegistry(UserPatchFields, old, &in)
		if len(changes) > 0 {
			if _, _, err := eventbus.WriteHybridUpdateEvents(
				ctx, tx,
				EventUserUpdated, EventUserFieldChanged,
				"user", id.String(), nil,
				eventbusActor(actor),
				changes,
				"",
			); err != nil {
				return err
			}
		}
		if newHash != nil {
			if err := writeUserEvent(ctx, tx, EventUserPasswordChanged, id, actor, *old, dto); err != nil {
				return err
			}
		}
		// sk-622.21 T017 (US-11): "Global role change" — admin ↔ user
		// changes the user's project visibility and everything scoped by it.
		if in.Role != nil && *in.Role != old.Role {
			signalAccessChanged(tx, id)
		}
		if len(seenBefore) > 0 {
			if _, err := authz.RevokeProjectAccess(ctx, tx, id, seenBefore, authz.RevokeAdminDemoted, authz.RevokeActor{
				UserID: actor.UserID, Username: actor.Username, SessionID: actor.SessionID, Type: actor.Role,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if name, ok := isUniqueViolation(err); ok && strings.Contains(name, "username") {
			return nil, ErrUsernameTaken
		}
		return nil, err
	}
	return &dto, nil
}

// BumpSessionEpochInTx raises the user's session epoch inside tx and returns
// the new value (sk-674.18 K-16, INV-26): every browser session issued with
// an older epoch is refused from the next request on, on every replica
// (handler/middleware.go#RequireSessionEpoch — no cache in between). API
// keys are NOT touched: a password reset ends the sessions, not the agents.
// ErrUserNotFound when the user does not exist.
func BumpSessionEpochInTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (int64, error) {
	var epoch int64
	err := tx.QueryRow(ctx, `
UPDATE users SET session_epoch = session_epoch + 1 WHERE id = $1 RETURNING session_epoch`, id).Scan(&epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrUserNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("raising the session epoch of %s: %w", id, err)
	}
	return epoch, nil
}

// DeactivateInTx deactivates the user inside tx (sk-674.18 K-16, INV-26):
// deactivated_at, a raised session epoch — every session ends — and every
// active API key revoked (apikeys.RevokeAllForUser), atomically. The
// deactivation of the only active admin is refused with ErrLastAdmin and
// nothing is written. Deactivating an already deactivated user is a no-op
// (changed = false, no event). It is the one deactivation of the product:
// the admin path (Deactivate), the operator (T042) and the account deletion
// (T041) all go through it. Deactivation is not a loss of access (INV-12):
// memberships and assignments stay.
func (s *Store) DeactivateInTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, actor Actor) (changed bool, err error) {
	old, err := loadUserInTx(ctx, tx, id)
	if err != nil {
		return false, err
	}
	if old.DeactivatedAt != nil {
		// Already deactivated — no-op. Don't write a duplicate event.
		return false, nil
	}
	// Last-admin protection — only relevant when we're disabling an
	// active admin row.
	if old.Role == "admin" {
		adminCount, err := countActiveAdminsInTx(ctx, tx)
		if err != nil {
			return false, err
		}
		if adminCount <= 1 {
			return false, ErrLastAdmin
		}
	}

	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `
UPDATE users SET deactivated_at = $2, session_epoch = session_epoch + 1 WHERE id = $1`, id, now); err != nil {
		return false, err
	}

	// Cascade — soft-revoke every active key in the same tx. The
	// rows-affected count goes into the audit event so the operator
	// can see it without a separate query.
	revoked, err := s.apikeysStore.RevokeAllForUser(ctx, tx, id)
	if err != nil {
		return false, err
	}

	newDTO := *old
	newDTO.DeactivatedAt = &now
	if err := writeUserEvent(ctx, tx, EventUserDeactivated, id, actor, *old, struct {
		UserDTO
		RevokedAPIKeyCount int64 `json:"revokedApiKeyCount"`
	}{newDTO, revoked}); err != nil {
		return false, err
	}
	// sk-622.21 T017 (US-11): "Deactivation".
	signalAccessChanged(tx, id)
	return true, nil
}

// Deactivate is the admin's deactivation: DeactivateInTx in a SERIALIZABLE
// transaction of its own, retried like every membership primitive (K-7,
// sk-731) — a serialization abort is a concurrency conflict, not the last
// admin: the retry re-reads the admin count, and ErrLastAdmin comes only
// from that recheck. Exhausted retries are spaces.ErrConcurrentChange.
// Idempotent on an already deactivated user (the handler maps that to 204).
func (s *Store) Deactivate(ctx context.Context, id uuid.UUID, actor Actor) error {
	return spaces.WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := s.DeactivateInTx(ctx, tx, id, actor)
		return err
	})
}

// Reactivate clears deactivated_at. Does NOT un-revoke API keys per Q-4
// — the user must re-issue any key they need — and does NOT lower the
// session epoch (K-16): sessions issued before the deactivation stay
// refused. Idempotent on an already-active user (no-op, no event).
//
// Retried on a serialization abort like Deactivate (sk-731); exhausted
// retries are spaces.ErrConcurrentChange.
func (s *Store) Reactivate(ctx context.Context, id uuid.UUID, actor Actor) error {
	return spaces.WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		old, err := loadUserInTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if old.DeactivatedAt == nil {
			return nil // already active
		}
		if _, err := tx.Exec(ctx, `
UPDATE users SET deactivated_at = NULL WHERE id = $1`, id); err != nil {
			return err
		}
		newDTO := *old
		newDTO.DeactivatedAt = nil
		if err := writeUserEvent(ctx, tx, EventUserReactivated, id, actor, *old, newDTO); err != nil {
			return err
		}
		// sk-622.21 T017 (US-11): "Reactivation".
		signalAccessChanged(tx, id)
		return nil
	})
}

// ChangePassword is the self-service path. Verifies the supplied current
// password against the stored hash with bcrypt, optionally checks the new
// password against the Pwned Passwords k-anonymity corpus, then rotates
// the hash. Returns ErrInvalidCredentials on bad current; ErrInvalidPassword
// on new too short; ErrPasswordTooLong on new too long (>72 bytes UTF-8);
// ErrPasswordPwned when the pwned check is enabled and the new password was
// found in the breach corpus; ErrPasswordChangedConcurrently when another
// request rotated the password in the window between our read and our
// write (see commitPasswordChange).
//
// Deactivated users cannot change their password — we return
// ErrInvalidCredentials (collapse with bad-current so a probe cannot
// distinguish "deactivated" from "wrong password").
//
// sk-303.20 review (2026-09-04) — the
// original shape ran bcrypt Verify, the outbound pwned-passwords HTTP call,
// AND bcrypt Hash all inside one SERIALIZABLE pgx.BeginTxFunc, holding a
// pooled connection and a SERIALIZABLE snapshot for up to
// SKEIN_PWNED_PASSWORDS_TIMEOUT (default 2s) plus two bcrypt-cost CPU
// charges — the only place in this codebase where a network call happens
// inside a DB transaction. Rebuilt into three phases, none of which mixes
// "talk to the network / spend a bcrypt-cost" with "hold a pool
// connection":
//
//  1. loadPasswordState — one plain read (no explicit transaction) that
//     re-establishes every existing guard (SSO gate, deactivated, no local
//     hash, current-password Verify) and returns the hash it verified
//     against.
//  2. Outside any DB session: the pwned lookup (only after step 1 proves
//     the request legitimate — never spend an outbound call on a bad
//     current password) and the new bcrypt Hash (only after the pwned
//     check clears — never spend a bcrypt-cost on a password we're about
//     to reject).
//  3. commitPasswordChange — a short transaction containing exactly one
//     UPDATE, guarded by a compare-and-swap on the hash step 1 observed,
//     plus the audit event. Zero rows affected means the row changed
//     between steps 1 and 3 (a concurrent password rotation) and is
//     reported as ErrPasswordChangedConcurrently, not silently overwritten
//     and not a 500.
//
// This preserves every ordering guarantee sk-303.20 introduced (pwned
// check after Verify, Hash after the pwned check clears) and every
// pre-existing guard/error shape (see password_gate_s4a_test.go and the
// store_test.go ChangePassword_* tests) — only the transaction boundaries
// moved.
func (s *Store) ChangePassword(ctx context.Context, userID uuid.UUID, current, next string) error {
	if err := validatePassword(next); err != nil {
		return err
	}

	oldHash, err := s.loadPasswordState(ctx, userID, current)
	if err != nil {
		return err
	}

	// The request is now known legitimate (current password verified) —
	// only past this point may we spend an outbound call on it. Neither
	// this call nor the bcrypt Hash below holds a pool connection: the read
	// above already returned it, and the write below hasn't opened one yet.
	if err := s.checkPwned(ctx, next, "userId", userID.String()); err != nil {
		return err
	}

	newHash, err := auth.Hash(next, s.bcryptCost)
	if err != nil {
		return fmt.Errorf("hashing password: %w", err)
	}

	return s.commitPasswordChange(ctx, userID, oldHash, newHash)
}

// loadPasswordState reads just enough of the user row to authorize a
// self-service password change and verifies `current` against the stored
// hash — no explicit transaction, a single SELECT against the pool. Returns
// the verified hash on success (the caller uses it as the compare-and-swap
// key in commitPasswordChange); every failure mode returns the same
// sentinel ChangePassword documented before this function was split.
func (s *Store) loadPasswordState(ctx context.Context, userID uuid.UUID, current string) (string, error) {
	var hash *string
	var deactivatedAt *time.Time
	var isExternal bool
	var email string
	err := s.pool.QueryRow(ctx, `
SELECT password_hash, deactivated_at, is_external, email FROM users WHERE id = $1`, userID).
		Scan(&hash, &deactivatedAt, &isExternal, &email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrUserNotFound
	}
	if err != nil {
		return "", err
	}
	// sk-163 S4a — when OIDC is active, an internal user may not rotate a
	// local password (that would be a Keycloak-bypass). External users and
	// the break-glass admin are exempt; local mode is unaffected. The
	// caller is authenticated as themselves, so a distinct error is fine
	// (no cross-account enumeration to defend against here).
	if !s.localPasswordAllowed(isExternal, email) {
		return "", ErrLocalPasswordDisabled
	}
	if deactivatedAt != nil {
		return "", ErrInvalidCredentials
	}
	if hash == nil {
		// Keycloak-only user — no standalone password to rotate.
		return "", ErrInvalidCredentials
	}
	if verr := auth.Verify(*hash, current); verr != nil {
		return "", ErrInvalidCredentials
	}
	return *hash, nil
}

// commitPasswordChange writes newHash with a compare-and-swap on oldHash
// (the hash loadPasswordState verified `current` against) inside a short
// transaction that does nothing but that one UPDATE plus the audit event —
// no network call, no bcrypt, nothing that can hold the connection open
// beyond ordinary SQL latency. Zero rows affected means password_hash no
// longer equals oldHash — someone else rotated it between the read and
// this write — and is reported as ErrPasswordChangedConcurrently rather
// than silently clobbering a hash the caller never actually verified.
//
// No explicit isolation level: the CAS predicate is what makes this race-
// safe, not SERIALIZABLE snapshot isolation — Postgres's default READ
// COMMITTED is sufficient and (unlike the pre-sk-303.20-fix shape) never
// surfaces a spurious 40001 serialization failure against unrelated
// concurrent writes to the same row (e.g. an admin's UpdateUser).
func (s *Store) commitPasswordChange(ctx context.Context, userID uuid.UUID, oldHash, newHash string) error {
	return pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
UPDATE users SET password_hash = $2 WHERE id = $1 AND password_hash = $3`, userID, newHash, oldHash)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrPasswordChangedConcurrently
		}
		// Self-service path → actor is the user themselves. We resolve the
		// username inline so the audit row carries it; falling back to "user"
		// keeps events.actor non-empty even on a degenerate row.
		var username string
		_ = tx.QueryRow(ctx, `SELECT username FROM users WHERE id = $1`, userID).Scan(&username)
		if username == "" {
			username = "user"
		}
		actor := Actor{UserID: userID, Username: username, Role: "human"}
		// new/old payloads omit the hash entirely (see writeUserEvent —
		// caller decides what goes into new_data).
		return writeUserEvent(ctx, tx, EventUserPasswordChanged, userID, actor, nil, map[string]any{
			"userId": userID.String(),
		})
	})
}

// CountActiveAdmins returns the number of active (deactivated_at IS NULL)
// users with role='admin'. Exposed for tests + diagnostics; the in-tx
// version is used for the actual race-protected last-admin check.
func (s *Store) CountActiveAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
SELECT count(*) FROM users WHERE role = 'admin' AND deactivated_at IS NULL`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting active admins: %w", err)
	}
	return n, nil
}

// ResolveByUsername looks up a user id by canonical (lowercase) username.
// Returns ErrUserNotFound when no row matches. Used by the project member
// MCP tools so callers can pass either userId or username.
func (s *Store) ResolveByUsername(ctx context.Context, username string) (uuid.UUID, error) {
	uname := strings.TrimSpace(strings.ToLower(username))
	if uname == "" {
		return uuid.Nil, ErrUserNotFound
	}
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
SELECT id FROM users WHERE lower(username) = $1`, uname).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrUserNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("resolving username: %w", err)
	}
	return id, nil
}

// ----------- helpers -----------

// ErrValidation is wrapped by every "your input failed surface validation"
// branch (email regex, username regex, missing name). Callers detect it
// via errors.Is.
var ErrValidation = errors.New("validation error")

// passwordMaxBytes is bcrypt's structural ceiling (Blowfish key schedule),
// not a version-specific quirk — golang.org/x/crypto/bcrypt.
// GenerateFromPassword returns ErrPasswordTooLong past this length
// regardless of cost. Checked here, before any bcrypt call, so it surfaces
// as a clean validation error rather than an unhandled 500. A password
// need not be long in RUNES to cross this: len(p) on a Go string counts
// bytes, and 64 Cyrillic characters (2 bytes/rune) or 64 emoji (up to 4
// bytes/rune) both exceed 72 bytes well before 64 characters.
const passwordMaxBytes = 72

func validatePassword(p string) error {
	if len(p) < PasswordMinLen {
		return ErrInvalidPassword
	}
	if len(p) > passwordMaxBytes {
		return ErrPasswordTooLong
	}
	return nil
}

func isUniqueViolation(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		return pgErr.ConstraintName, true
	}
	return "", false
}

// isSerializationFailure returns true when err is (or wraps) a Postgres
// SQLSTATE 40001 — a SERIALIZABLE transaction abort. Used by UpdateUser
// and Deactivate to remap concurrent last-admin races to ErrLastAdmin
// instead of bubbling a generic 500.
func isSerializationFailure(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgSerializationFailure
}

// scanner is the minimal interface satisfied by both pgx.Row and pgx.Rows
// for the Scan-into-DTO helper.
type scanner interface {
	Scan(dest ...any) error
}

// userStatusCol is the person marks of the canonical user column set
// (sk-674.18 T027): its last column, read by scanUserDTO.
var userStatusCol = person.StatusExpr("users")

// scanUserDTO reads the canonical user column set into a UserDTO.
// Includes derived AuthMethod from password_hash + keycloak_sub state.
func scanUserDTO(s scanner) (*UserDTO, error) {
	var dto UserDTO
	var (
		lastLogin     *time.Time
		deactivatedAt *time.Time
		hasPassword   bool
		hasKeycloak   bool
	)
	var status *person.Status
	if err := s.Scan(&dto.ID, &dto.Email, &dto.Username, &dto.Name, &dto.Role,
		&dto.CreatedAt, &lastLogin, &deactivatedAt,
		&hasPassword, &hasKeycloak, &dto.IsExternal, &status); err != nil {
		return nil, err
	}
	if status != nil {
		dto.Status = *status
	}
	dto.CreatedAt = dto.CreatedAt.UTC()
	if lastLogin != nil {
		v := lastLogin.UTC()
		dto.LastLoginAt = &v
	}
	if deactivatedAt != nil {
		v := deactivatedAt.UTC()
		dto.DeactivatedAt = &v
	}
	switch {
	case hasPassword && hasKeycloak:
		dto.AuthMethod = "both"
	case hasKeycloak:
		dto.AuthMethod = "keycloak"
	case hasPassword:
		dto.AuthMethod = "password"
	default:
		// users_auth_method CHECK forbids this; defensive fallback.
		dto.AuthMethod = "password"
	}
	return &dto, nil
}

// loadUserInTx fetches the row inside a transaction (pre-update snapshot)
// for a mutation — UpdateUser, DeactivateInTx, Reactivate. Returns
// ErrUserNotFound on miss and for a deleted account: a deleted account is
// terminal, no admin update, (re)activation or deactivation reaches it
// (sk-674.18 T041).
func loadUserInTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*UserDTO, error) {
	row := tx.QueryRow(ctx, `
SELECT id, email, username, name, role, created_at, last_login_at, deactivated_at, password_hash IS NOT NULL AS has_pwd, keycloak_sub IS NOT NULL AS has_kc, is_external, `+userStatusCol+`
  FROM users
 WHERE id = $1 AND deleted_at IS NULL`, id)
	dto, err := scanUserDTO(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	return dto, nil
}

// countActiveAdminsInTx is the in-transaction variant — required for the
// last-admin race protection. Reading from the same tx that's about to
// commit the demotion guarantees serializability against concurrent
// demotions.
func countActiveAdminsInTx(ctx context.Context, tx pgx.Tx) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `
SELECT count(*) FROM users WHERE role = 'admin' AND deactivated_at IS NULL`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting active admins in tx: %w", err)
	}
	return n, nil
}

// signalAccessChanged — "Global role change, deactivation, reactivation"
// (sk-622.21 T017, US-11): the user gets projects, lens, unreviewed and tasks.
func signalAccessChanged(tx pgx.Tx, userID uuid.UUID) {
	signals.Add(tx, userID, signals.AreaProjects, signals.AreaLens, signals.AreaUnreviewed, signals.AreaTasks)
}
