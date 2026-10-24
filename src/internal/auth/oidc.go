// R5.1 — Keycloak SSO auto-provisioning (trust-headers org pattern).
//
// The Provisioner is invoked from POST /api/v1/auth/oidc/provision, which
// in turn is called by skein-web (BFF) when oauth2-proxy forwards
// X-Auth-Request-Email + X-Auth-Request-User headers (already authenticated
// at the cluster edge). skein-api itself never sees JWTs.
//
// Match key: email (lower-cased) is the primary identity key in the trust-
// headers flow — that's what oauth2-proxy forwards. The legacy
// keycloak_sub column remains supported but optional: when supplied
// (reserved for the deferred R5.x Bearer-token flow, Task #35) we try
// sub-match first, then fall back to email. When not supplied we go
// straight to email-match.
//
// Existing local users (password_hash IS NOT NULL, keycloak_sub IS NULL)
// ARE auto-linked when their email matches the inbound claim — this is
// the deliberate consequence of trusting oauth2-proxy at the edge: any
// user authenticated by Keycloak with a matching email *is* the same
// human as the existing local row. Password login keeps working for
// those rows.
//
// EXCEPTION (sk-163 S5): a local row flagged is_external=true is NEVER
// auto-linked. External collaborators are local-password identities whose
// visibility ceiling must not be silently upgraded into an internal SSO
// identity (privilege/visibility escalation). All three link paths
// (sub-match, email-match, INSERT…ON CONFLICT) detect such a match and
// refuse it with ErrExternalAccountConflict instead of binding — the
// matched external row is left UNTOUCHED (no keycloak_sub backfill, no
// last_login/name sync). Converting an account between external and
// internal is an explicit, audited admin operation, out of scope for the
// auto-provision path. See the per-method comments.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/accounts"
	"skein-api/internal/assignee"
	"skein-api/internal/signals"
)

// ProvisionInput carries the claims skein-web extracts from
// oauth2-proxy's trust headers (X-Auth-Request-Email +
// X-Auth-Request-User) and forwards to skein-api.
//
// Required: Email, Username. Missing/empty values return ErrMissingClaims.
// Name is optional — empty on INSERT falls back to Username; empty on
// UPDATE keeps the existing display name.
//
// Optional: Sub. The trust-headers flow (oauth2-proxy with
// pass_access_token=false, set_xauthrequest=true) does not deliver the
// Keycloak `sub` claim to backends, so this field is empty in practice
// today. It is reserved for the deferred R5.x Bearer-token flow for MCP
// agents (Task #35) — when non-empty the provisioner prefers it as the
// match key (forward-compat), falling back to email otherwise.
type ProvisionInput struct {
	Sub      string // OPTIONAL keycloak `sub` claim — reserved for R5.x Bearer flow
	Email    string // REQUIRED — lower-cased before insert/compare; primary match key
	Username string // REQUIRED — lower-cased; canonical mention handle
	Name     string // OPTIONAL — display name; "" on INSERT falls back to Username, "" on UPDATE keeps existing
}

// ProvisionResult describes the user row that was either created (INSERT)
// or matched (UPDATE on last_login_at + email + name). Provisioned is true
// only on INSERT — the handler does not currently distinguish the two for
// the BFF response, but the field is kept so audit/metric code can.
type ProvisionResult struct {
	UserID      uuid.UUID
	Email       string
	Username    string
	Name        string // post-COALESCE display name (what's actually stored)
	Role        string // "admin" | "user"
	IsExternal  bool   // sk-163 S1 — carried through so later slices can branch on it
	Provisioned bool   // true → just inserted; false → updated existing match
}

// Sentinel errors returned by Provision. Callers use errors.Is to fan them
// out to HTTP statuses in the handler — see handler/auth.go OIDCProvision.
var (
	// ErrMissingClaims is returned when email or username is empty after
	// trim (sub and name are optional). The BFF guarantees email/username
	// in normal flow; receiving empty means the JWT was missing the claim
	// mapper or the BFF wrapper has a bug — both are operator-facing
	// fixes, hence 422.
	ErrMissingClaims = errors.New("missing_claims")

	// ErrUserNotProvisioned is returned when autoProvision is disabled
	// and the supplied sub has no row in users. Surfaced to the BFF as
	// 403 user_not_provisioned which renders as "contact admin to grant
	// access" in the SSO error page (plan §8.5.10).
	ErrUserNotProvisioned = errors.New("user_not_provisioned")

	// ErrUsernameExhausted is returned when the suffix-loop (john, john2,
	// john3, …) hits the cap without finding a free slot. In practice
	// this only happens under pathological conflicts; the cap exists so
	// the loop is bounded.
	ErrUsernameExhausted = errors.New("username_exhausted")

	// ErrExternalAccountConflict is returned (sk-163 S5) when SSO
	// provisioning would bind a Keycloak identity to a LOCAL account flagged
	// is_external=true — whether matched by keycloak_sub (updateBySub), by
	// email (updateByEmail), or hit as an email collision on the INSERT
	// (insertNew … ON CONFLICT). External collaborators must never be
	// auto-linked to or upgraded into an internal SSO identity (spec §8); the
	// provisioner refuses with this sentinel and leaves the external row
	// untouched rather than silently escalating its ceiling. The handler
	// surfaces it as 403 external_account_conflict — a refusal, not a 500.
	ErrExternalAccountConflict = errors.New("external_account_conflict")

	// ErrProvisionRetriesExhausted is returned (wrapping the last
	// class-40 abort) when one SERIALIZABLE write of the provisioner kept
	// losing serialization races for its whole retry budget (see
	// provisionTx). Nothing was written by the failing write. The handler
	// surfaces it as 500 internal_error with the wrapped detail in the log;
	// the BFF's next login attempt is a fresh run.
	ErrProvisionRetriesExhausted = errors.New("provision_serialization_retries_exhausted")
	// ErrIdentityReserved is returned (sk-674.18 T041, INV-18) when the
	// login's email belongs to a deleted account: its hash is reserved
	// and no account is created with it again. The handler answers it
	// like ErrUserNotProvisioned (403 user_not_provisioned).
	ErrIdentityReserved = errors.New("identity_reserved")
)

// usernameSuffixAttempts caps the john → john2 → … fallback loop. With
// pre-existing names like "john", "john2", "john3", "john4", "john5"
// already taken we surrender at the 6th try (the loop runs base + 4
// suffixed variants = 5 unique attempts at default cap). Matches the
// plan §8.5.5 "max 5 attempts" decision.
const usernameSuffixAttempts = 5

// Retry budgets of the three SERIALIZABLE writes of the provisioner
// (sk-622.21 R25; one constant per path, sk-354 / R17, so a measurement on
// one path can move its budget alone). Every write here touches the one
// users row of the person logging in, and concurrent logins of one person
// (several tabs, a BFF retry) update that row at once: under SERIALIZABLE the
// first committer wins and every other updater aborts with 40001 and
// replays. Each round commits at least one of them, so N concurrent logins
// of one account need at most N attempts; the budget covers a burst of
// tabs, not just a single race with an assignment.
const (
	updateBySubMaxAttempts   = 10
	updateByEmailMaxAttempts = 10
	insertNewMaxAttempts     = 10
)

// SQLSTATEs of the class-40 aborts a replay of the whole transaction
// resolves.
const (
	pgSerializationFailure = "40001"
	pgDeadlockDetected     = "40P01"
)

// isRetryableTxError reports a 40001 serialization_failure or a 40P01
// deadlock_detected: the transaction was rolled back whole and replaying it
// against a fresh snapshot is safe.
func isRetryableTxError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == pgSerializationFailure || pgErr.Code == pgDeadlockDetected
}

// provisionTx runs fn in a SERIALIZABLE transaction, replaying it on a
// class-40 abort with a small jittered backoff; attempts is the TOTAL number
// of tries. Any other error is returned as is (fn's pgx.ErrNoRows and
// 23505 included — the callers branch on them). When the budget runs out the
// result is ErrProvisionRetriesExhausted wrapping the last abort.
//
// WHY SERIALIZABLE (sk-622.21 R25, INV-15). Creating a users row or changing
// its email / username re-resolves issues.assignee_user_id in the same
// transaction (triggers of migration 000060). An assignment running in a
// SERIALIZABLE transaction whose snapshot predates this write resolves the
// assignee against the old directory; with this write in READ COMMITTED both
// committed and the row kept a stale id until a background pass. With both
// sides SERIALIZABLE, SSI sees the rw-dependency cycle «the assignment read
// users — this write changed them; this write's trigger read issues — the
// assignment changed them» and aborts one side with 40001; the replay
// resolves against what the winner committed.
//
// fn must not have side effects outside the transaction, and the locals it
// assigns must be (re)assigned on every attempt.
func (p *Provisioner) provisionTx(ctx context.Context, op string, attempts int, fn func(tx pgx.Tx) error) error {
	var err error
	for attempt := range attempts {
		err = pgx.BeginTxFunc(ctx, p.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, fn)
		if err == nil || !isRetryableTxError(err) {
			return err
		}
		if attempt == attempts-1 {
			break
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			slog.WarnContext(ctx, "oidc provision: transaction hit a retryable abort; retrying",
				"op", op,
				"attempt", attempt+1,
				"maxAttempts", attempts,
				"sqlstate", pgErr.Code,
			)
		}
		// Jittered backoff: 25ms × (attempt+1) + 0..25ms, as in the
		// stores' withSerializableRetry.
		base := 25 * time.Millisecond * time.Duration(attempt+1)
		jitter := time.Duration(rand.Int64N(int64(25 * time.Millisecond)))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(base + jitter):
		}
	}
	return fmt.Errorf("%w: %s after %d attempts: %w", ErrProvisionRetriesExhausted, op, attempts, err)
}

// Constraint names (exact match — not substring). A future migration
// rename surfaces as a compile-time grep hit instead of silently
// reclassifying a unique violation as an unrelated error.
const (
	constraintEmailLower    = "idx_users_email_lower"
	constraintUsernameLower = "idx_users_username_lower"
)

// Provisioner owns the lookup-or-insert flow for SSO logins. Construct
// once at startup (in main.go) and share across goroutines; the underlying
// pgxpool.Pool is safe for concurrent use.
type Provisioner struct {
	pool                *pgxpool.Pool
	autoProvision       bool
	defaultRole         string
	bootstrapAdminEmail string // lower-cased at construction
}

// NewProvisioner builds a Provisioner with config-time-validated inputs.
// autoProvision == false means "match-only mode": unknown subs return
// ErrUserNotProvisioned and require manual seeding. defaultRole MUST be
// one of "user" | "admin" — Load() in config validates this in keycloak
// mode so we don't repeat the check here.
//
// bootstrapAdminEmail is compared case-insensitively against the
// inbound email claim; pre-lower-casing here keeps Provision allocation-
// free on the hot path.
func NewProvisioner(pool *pgxpool.Pool, autoProvision bool, defaultRole, bootstrapAdminEmail string) *Provisioner {
	return &Provisioner{
		pool:                pool,
		autoProvision:       autoProvision,
		defaultRole:         defaultRole,
		bootstrapAdminEmail: strings.ToLower(strings.TrimSpace(bootstrapAdminEmail)),
	}
}

// Provision resolves an SSO login to a users row. Match precedence:
//
//  1. Sub non-empty AND found in users → UPDATE last_login_at + email +
//     name (forward-compat for the deferred R5.x Bearer-token flow).
//  2. Email found in users → UPDATE last_login_at + name; if Sub is also
//     non-empty and the matched row has a NULL keycloak_sub we
//     opportunistically backfill it. This is the production trust-headers
//     hot path today.
//  3. Neither sub nor email matched:
//     - autoProvision=false → ErrUserNotProvisioned (403 to the caller).
//     - autoProvision=true  → INSERT new row, role = bootstrap-elevated
//     or defaultRole, password_hash = NULL. The INSERT loops on
//     username UNIQUE conflicts with john → john2 → john3 (capped,
//     see ErrUsernameExhausted).
//
// Email/name sync is unconditional once a user is matched — oauth2-proxy
// (and Keycloak behind it) are the source of truth for display values.
//
// Race protection: the auto-provision INSERT goes through a single UPSERT
// statement (INSERT … ON CONFLICT … DO UPDATE … RETURNING) so the
// "concurrent first login" case still resolves to one row.
//
// Isolation (sk-622.21 R25): each of the three writes — updateBySub,
// updateByEmail, insertNew — is its own SERIALIZABLE transaction replayed on
// 40001/40P01 (provisionTx), so a login that creates an account or changes
// its email can never leave a concurrent SERIALIZABLE assignment with a stale
// issues.assignee_user_id (INV-15 at commit). An exhausted budget returns
// ErrProvisionRetriesExhausted.
func (p *Provisioner) Provision(ctx context.Context, in ProvisionInput) (*ProvisionResult, error) {
	sub := strings.TrimSpace(in.Sub)
	email := strings.ToLower(strings.TrimSpace(in.Email))
	username := deriveUsername(in.Username, email)
	name := strings.TrimSpace(in.Name)
	// Sub and Name are OPTIONAL in the trust-headers org pattern (see
	// ProvisionInput doc-comment). Email and username are required —
	// they're the two trust-header fields oauth2-proxy ALWAYS forwards.
	// Empty Name on INSERT → falls back to username; on UPDATE → keeps
	// the existing display name (first-login-wins).
	if email == "" || username == "" {
		return nil, ErrMissingClaims
	}

	// 1) Try sub-match first when supplied. This keeps the existing path
	//    intact for any future caller that does deliver a sub (R5.x
	//    Bearer flow / MCP) and gives us forward-compat without breaking
	//    today's email-only callers.
	if sub != "" {
		res, err := p.updateBySub(ctx, sub, email, name)
		switch {
		case err == nil:
			return res, nil
		case errors.Is(err, pgx.ErrNoRows):
			// Sub not in DB yet — fall through to email-match.
		default:
			return nil, err
		}
	}

	// Compute the keycloak_sub value we'd store: real sub when supplied,
	// synthetic `trust-headers:<email>` token otherwise. This is the
	// value passed down to updateByEmail (for backfill when the matched
	// row has a NULL keycloak_sub) and to insertNew (for the new row's
	// keycloak_sub column).
	storedSub := sub
	if storedSub == "" {
		storedSub = trustHeaderSubPrefix + email
	}

	// 2) Email-match (primary path under trust-headers). On hit, sync
	//    name + last_login_at; backfill keycloak_sub with `storedSub`
	//    when the row had NULL (auto-link existing local-password user
	//    to the SSO identity).
	res, err := p.updateByEmail(ctx, storedSub, email, name)
	switch {
	case err == nil:
		return res, nil
	case errors.Is(err, pgx.ErrNoRows):
		// Fall through to INSERT or NotProvisioned below.
	default:
		return nil, err
	}

	// 3) Neither sub nor email matched.
	if !p.autoProvision {
		return nil, ErrUserNotProvisioned
	}
	return p.insertNew(ctx, storedSub, email, username, name)
}

// updateBySub runs the sub-keyed UPDATE … RETURNING and maps ErrNoRows
// through unchanged so the caller can fall through to email-match. Email
// collisions on the UPDATE (sub maps to a row whose new email is now
// taken by another row) are demoted to a name-only UPDATE so a display-
// field claim change never fails the login.
//
// sk-163 S5 anti-link: the statement is a guarded CTE. It locates the row
// by keycloak_sub, but the UPDATE only fires when the matched row is NOT
// external — an external row (one that somehow carries a real keycloak_sub
// via import / manual SQL / bug) is detected via the second UNION branch
// and returned with is_external=true WITHOUT being mutated. The caller then
// returns ErrExternalAccountConflict. We deliberately do NOT write a plain
// `WHERE … AND NOT is_external`: that would turn an external match into
// ErrNoRows and let Provision fall through to insertNew, whose ON CONFLICT
// would re-touch the same row — the footgun the spec §8 calls out. Because
// the external branch performs no UPDATE it can never raise a unique
// violation, so the email-collision retry below is reached only for
// non-external rows.
func (p *Provisioner) updateBySub(ctx context.Context, sub, email, name string) (*ProvisionResult, error) {
	var (
		id          uuid.UUID
		outEmail    string
		outUsername string
		outName     string
		role        string
		isExternal  bool
		oldEmail    string
	)
	err := p.provisionTx(ctx, "updateBySub", updateBySubMaxAttempts, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
WITH matched AS (
    SELECT id, email, username, name, role, is_external
      FROM users
     WHERE keycloak_sub = $1
), updated AS (
    UPDATE users u
       SET email         = $2,
           name          = COALESCE(NULLIF($3, ''), u.name),
           last_login_at = NOW()
      FROM matched
     WHERE u.id = matched.id AND NOT matched.is_external
    RETURNING u.id, u.email, u.username, u.name, u.role, u.is_external, matched.email AS old_email
)
SELECT id, email, username, name, role, is_external, old_email FROM updated
UNION ALL
SELECT id, email, username, name, role, is_external, email FROM matched WHERE is_external
`, sub, email, name).Scan(&id, &outEmail, &outUsername, &outName, &role, &isExternal, &oldEmail)
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, pgx.ErrNoRows
	case err != nil:
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
			pgErr.ConstraintName == constraintEmailLower {
			slog.WarnContext(ctx, "oidc provision: email collision on sub-match — keeping stale email",
				"keycloakSub", maskSub(sub),
				"constraint", pgErr.ConstraintName,
			)
			// Reached only for a NON-external matched row (the external
			// branch never UPDATEs, so it cannot collide). The retry keeps
			// is_external in RETURNING for symmetry; it is always false here.
			// The collision aborted the transaction above; the name-only
			// write is a transaction of its own. It changes neither email
			// nor username, and runs SERIALIZABLE for the uniformity of
			// the provisioner's writes; oldEmail is reset so no
			// resolution-change signal is sent for an email that stayed.
			oldEmail = ""
			retryErr := p.provisionTx(ctx, "updateBySub/nameOnly", updateBySubMaxAttempts, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `
UPDATE users
   SET name          = COALESCE(NULLIF($2, ''), name),
       last_login_at = NOW()
 WHERE keycloak_sub  = $1
RETURNING id, email, username, name, role, is_external
`, sub, name).Scan(&id, &outEmail, &outUsername, &outName, &role, &isExternal)
			})
			if retryErr != nil {
				return nil, fmt.Errorf("oidc provision (sub-match retry without email) for sub %q: %w", maskSub(sub), retryErr)
			}
		} else {
			return nil, fmt.Errorf("oidc provision (sub-match) for sub %q: %w", maskSub(sub), err)
		}
	}
	if isExternal {
		// keycloak_sub matched an EXTERNAL local account. Refuse — never
		// upgrade an external collaborator into an SSO identity (spec §8).
		slog.WarnContext(ctx, "oidc provision refused: keycloak_sub matched an external local account — not linking",
			"userId", id.String(),
			"keycloakSub", maskSub(sub),
		)
		return nil, ErrExternalAccountConflict
	}
	// sk-622.21 T017 (US-11): "email change on SSO login" — the
	// statement above has committed; the issues naming the old or the new
	// address resolve differently now.
	if oldEmail != "" && !strings.EqualFold(oldEmail, outEmail) {
		p.signalResolutionChanged(ctx, id, oldEmail, outEmail)
	}
	slog.InfoContext(ctx, "oidc provision matched existing user by sub",
		"userId", id.String(),
		"keycloakSub", maskSub(sub),
		"role", role,
		"provisioned", false,
	)
	return &ProvisionResult{
		UserID:      id,
		Email:       outEmail,
		Username:    outUsername,
		Name:        outName,
		Role:        role,
		IsExternal:  isExternal,
		Provisioned: false,
	}, nil
}

// updateByEmail runs the email-keyed UPDATE … RETURNING. Three-state
// behaviour on keycloak_sub:
//
//	(a) row.keycloak_sub IS NULL    → backfilled with NULLIF($3, '')
//	    (real sub when supplied; otherwise stays NULL until insertNew
//	    would have hit and written the synthetic trust-headers prefix,
//	    but that path is only taken on no-match — we don't reach here
//	    for empty-sub callers with no existing row).
//	(b) row.keycloak_sub IS a real Keycloak UUID → kept untouched
//	    (UNIQUE partial index makes sure no collision can land here).
//	(c) row.keycloak_sub starts with `trust-headers:` (synthetic)
//	    → kept until an operator manually NULLs it to allow a real-sub
//	    rebind when the R5.x Bearer flow ships.
//
// Display name uses COALESCE(NULLIF($2,”), name) so an empty inbound
// name (typical trust-headers, no name claim) keeps the user's
// profile-edited display name.
//
// sk-163 S5 anti-link: guarded CTE, same shape as updateBySub. The
// keycloak_sub BACKFILL here is exactly the silent link spec §8 forbids
// for external accounts, so the UPDATE must NOT run against an external
// row — it fires only when the matched row is NOT external; an external
// match is detected via the second UNION branch and returned with
// is_external=true, with keycloak_sub/name/last_login left untouched. The
// caller maps that to ErrExternalAccountConflict. A plain
// `WHERE … AND NOT is_external` would instead yield ErrNoRows and fall
// through to insertNew, whose ON CONFLICT (lower(email)) DO UPDATE would
// re-touch and link the external row anyway — the documented footgun.
func (p *Provisioner) updateByEmail(ctx context.Context, sub, email, name string) (*ProvisionResult, error) {
	var (
		id          uuid.UUID
		outEmail    string
		outUsername string
		outName     string
		role        string
		isExternal  bool
	)
	err := p.provisionTx(ctx, "updateByEmail", updateByEmailMaxAttempts, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
WITH matched AS (
    SELECT id, email, username, name, role, is_external
      FROM users
     WHERE LOWER(email) = $1
), updated AS (
    UPDATE users u
       SET name          = COALESCE(NULLIF($2, ''), u.name),
           last_login_at = NOW(),
           keycloak_sub  = COALESCE(u.keycloak_sub, NULLIF($3, ''))
      FROM matched
     WHERE u.id = matched.id AND NOT matched.is_external
    RETURNING u.id, u.email, u.username, u.name, u.role, u.is_external
)
SELECT id, email, username, name, role, is_external FROM updated
UNION ALL
SELECT id, email, username, name, role, is_external FROM matched WHERE is_external
`, email, name, sub).Scan(&id, &outEmail, &outUsername, &outName, &role, &isExternal)
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, pgx.ErrNoRows
	case err != nil:
		return nil, fmt.Errorf("oidc provision (email-match) for email %q: %w", email, err)
	}
	if isExternal {
		// Email matched an EXTERNAL local account. Refuse — backfilling
		// keycloak_sub would link an external collaborator to SSO (spec §8).
		slog.WarnContext(ctx, "oidc provision refused: email matched an external local account — not linking",
			"userId", id.String(),
		)
		return nil, ErrExternalAccountConflict
	}
	slog.InfoContext(ctx, "oidc provision matched existing user by email",
		"userId", id.String(),
		"role", role,
		"provisioned", false,
	)
	return &ProvisionResult{
		UserID:      id,
		Email:       outEmail,
		Username:    outUsername,
		Name:        outName,
		Role:        role,
		IsExternal:  isExternal,
		Provisioned: false,
	}, nil
}

// trustHeaderSubPrefix is the prefix for synthetic keycloak_sub values
// stored on rows created via the trust-headers flow (sub claim is not
// available — see ProvisionInput doc-comment). The existing CHECK
// constraint users_auth_method requires every row to have at least one
// of (password_hash, keycloak_sub) non-NULL; rather than migrating the
// constraint we store a synthetic, namespaced value that:
//
//   - cannot collide with a real Keycloak sub (those are UUIDs, this is
//     prefixed with "trust-headers:")
//   - is stable per-user (derived from the lower-cased email) so the
//     partial UNIQUE index on keycloak_sub stays meaningful (two rows
//     with the same email can never coexist anyway, so this only
//     reinforces the existing LOWER(email) UNIQUE)
//   - is easy to identify in audits ("this user was provisioned via the
//     trust-headers org pattern, not a Keycloak Bearer token")
//
// When the R5.x Bearer flow lands (Task #35) and starts forwarding a
// real sub, updateByEmail's COALESCE-on-NULL backfill will only fire
// when keycloak_sub IS NULL; synthetic values won't be overwritten.
// If that becomes a concern, the operator can NULL out the synthetic
// values via SQL and let the next login re-link via the real sub.
const trustHeaderSubPrefix = "trust-headers:"

// insertNew creates a new users row when neither sub-match nor email-
// match found an existing user AND autoProvision is enabled. Loops on
// username UNIQUE conflicts with the john → john2 → john3 fallback;
// caps at usernameSuffixAttempts.
//
// Bootstrap-admin elevation: when the inbound email matches
// bootstrapAdminEmail case-insensitively, role is forced to "admin"
// even when defaultRole = "user". Runs only on this INSERT path —
// promoting an EXISTING user by changing SKEIN_BOOTSTRAP_ADMIN_EMAIL
// later is not supported (operator must promote by hand). This avoids
// claim-driven silent role changes.
//
// keycloak_sub on the inserted row: the supplied sub when non-empty
// (forward-compat for the R5.x Bearer flow), otherwise a synthetic
// "trust-headers:<email>" value (see trustHeaderSubPrefix). The
// synthetic value is required by the existing users_auth_method CHECK
// constraint which forbids NULL/NULL pairs of (password_hash,
// keycloak_sub).
func (p *Provisioner) insertNew(ctx context.Context, storedSub, email, username, name string) (*ProvisionResult, error) {
	role := p.defaultRole
	if p.bootstrapAdminEmail != "" && email == p.bootstrapAdminEmail {
		role = "admin"
	}
	// sk-674.18 T037, INV-20 (K-25): SSO never provisions an admin while the
	// role is disabled on the installation — whatever the settings say (the
	// startup check refuses such settings; this is the path's own guard).
	if role == RoleAdmin {
		var on bool
		if err := p.pool.QueryRow(ctx, `SELECT admin_enabled FROM instance_settings WHERE id = 1`).Scan(&on); err != nil {
			return nil, fmt.Errorf("reading the admin role flag: %w", err)
		}
		if !on {
			role = "user"
		}
	}

	// `storedSub` is computed by the caller (Provision): real sub when
	// supplied, synthetic `trust-headers:<email>` token otherwise. We
	// never insert NULL — the users_auth_method CHECK forbids it for
	// SSO-only rows (which is what we are, since password_hash stays
	// NULL on this path).

	base := username
	for attempt := range usernameSuffixAttempts {
		candidate := base
		if attempt > 0 {
			// john → john2, john3, john4, …
			candidate = fmt.Sprintf("%s%d", base, attempt+1)
		}

		var (
			id          uuid.UUID
			outEmail    string
			outUsername string
			outName     string
			outRole     string
			outExternal bool
			inserted    bool
		)
		// UPSERT on lower(email) — this resolves the concurrent
		// first-login race: two requests for the same fresh email
		// both reach this statement, one INSERTs, the other lands on
		// the ON CONFLICT branch and re-reads the same row instead of
		// failing with 23505. `(xmax = 0)` is Postgres's canonical
		// "was this an INSERT?" predicate inside an UPSERT.
		//
		// Name fallback: COALESCE(NULLIF($3, ''), $2) — empty inbound
		// name falls back to username on the INSERT path. On the
		// UPDATE branch we keep the row's existing name unless the
		// caller explicitly supplied a non-empty one — same first-
		// login-wins recipe as updateByEmail.
		//
		// keycloak_sub: ON UPDATE we preserve the existing value
		// (COALESCE(users.keycloak_sub, EXCLUDED.keycloak_sub)) so a
		// concurrent first-login that races into the UPDATE branch
		// does NOT clobber whatever the winning INSERT just wrote.
		// sk-163 S5 anti-link: the DO UPDATE is guarded with
		// `WHERE NOT users.is_external`. insertNew is only reached when
		// updateByEmail returned ErrNoRows (no row with this email existed),
		// so a conflict here can only arise from a concurrent INSERT racing
		// us on the same email. If that racing row happens to be EXTERNAL,
		// the DO UPDATE matches nothing, the statement affects zero rows, and
		// RETURNING yields pgx.ErrNoRows — which we surface as
		// ErrExternalAccountConflict below rather than touching the external
		// row. This closes the "fall-through re-touches the external row"
		// path the spec §8 warns about.
		usernameReserved := false
		err := p.provisionTx(ctx, "insertNew", insertNewMaxAttempts, func(tx pgx.Tx) error {
			// INV-18 (sk-674.18 T041): neither the email
			// nor the username of a deleted account is given again. A
			// reserved email refuses the login; a reserved username is
			// skipped like a taken one.
			reserved, err := accounts.IdentityReserved(ctx, tx, accounts.ReservedEmail, email)
			if err != nil {
				return err
			}
			if reserved {
				return ErrIdentityReserved
			}
			if usernameReserved, err = accounts.IdentityReserved(ctx, tx, accounts.ReservedUsername, candidate); err != nil || usernameReserved {
				return err
			}
			if err := tx.QueryRow(ctx, `
INSERT INTO users (id, email, username, name, role, keycloak_sub, password_hash, created_at, last_login_at, metadata)
VALUES (gen_random_uuid(), $1, $2, COALESCE(NULLIF($3, ''), $2), $4, $5, NULL, NOW(), NOW(), '{}'::jsonb)
ON CONFLICT (lower(email)) DO UPDATE SET
    name          = COALESCE(NULLIF($3, ''), users.name),
    last_login_at = NOW(),
    keycloak_sub  = COALESCE(users.keycloak_sub, EXCLUDED.keycloak_sub)
  WHERE NOT users.is_external
RETURNING id, email, username, name, role, is_external, (xmax = 0) AS inserted
`, email, candidate, name, role, storedSub).Scan(&id, &outEmail, &outUsername, &outName, &outRole, &outExternal, &inserted); err != nil {
				return err
			}
			// sk-674.18 T020: a new account gets its "Personal" and, by the
			// flag, the common space (INV-8, INV-22) in the same transaction.
			if inserted {
				return accounts.ProvisionInTx(ctx, tx, id, accounts.Provision{EmailConfirmed: true})
			}
			return nil
		})

		if errors.Is(err, ErrIdentityReserved) {
			slog.WarnContext(ctx, "oidc provision refused: the email belongs to a deleted account",
				"keycloakSub", maskSub(storedSub))
			return nil, ErrIdentityReserved
		}
		if err == nil && usernameReserved {
			continue
		}
		if errors.Is(err, pgx.ErrNoRows) {
			// Conflicting row exists but is external — DO UPDATE's WHERE
			// excluded it (zero rows, untouched). Refuse the link; suffix
			// retries would not help (the collision is on email, not
			// username), so return immediately.
			//
			// NOTE (sk-163 S5): single-threaded this branch is UNREACHABLE —
			// updateByEmail/updateBySub already refuse an external email before
			// insertNew runs. It fires only on a TOCTOU race (an external row
			// committed between updateByEmail's read and this INSERT). It is
			// therefore race-only defense-in-depth, verified by inspection +
			// the lower(email) UNIQUE index rather than a unit test (a
			// deterministic test needs an injected mid-flight barrier — see
			// S12 test-audit). The branch must never silently link, hence the
			// hard sentinel here.
			slog.WarnContext(ctx, "oidc provision refused: email collision with an external local account on insert — not linking",
				"keycloakSub", maskSub(storedSub),
			)
			return nil, ErrExternalAccountConflict
		}

		if err == nil {
			// sk-622.21 T017 (US-11): "User creation" through SSO —
			// committed; issues naming the new account now resolve to it.
			if inserted {
				p.signalResolutionChanged(ctx, id, outEmail, outUsername, id.String())
				// US-2 "common space outside the transition": the first admin appearing through
				// SSO creates the common space when it is wanted and missing.
				if outRole == RoleAdmin {
					if err := accounts.EnsureCommonSpace(ctx, p.pool); err != nil {
						slog.ErrorContext(ctx, "oidc provision: creating the common space failed",
							"userId", id.String(), "error", err.Error())
					}
				}
			}
			slog.InfoContext(ctx, "oidc provision via upsert",
				"userId", id.String(),
				"role", outRole,
				"keycloakSub", maskSub(storedSub),
				"provisioned", inserted,
			)
			return &ProvisionResult{
				UserID:      id,
				Email:       outEmail,
				Username:    outUsername,
				Name:        outName,
				Role:        outRole,
				IsExternal:  outExternal,
				Provisioned: inserted,
			}, nil
		}

		// Username UNIQUE clash (23505 / idx_users_username_lower) →
		// retry with the next suffix. The email-index conflict can no
		// longer fire here because ON CONFLICT (lower(email)) catches
		// it; same for keycloak_sub backfill conflicts (we only write
		// EXCLUDED.keycloak_sub when users.keycloak_sub IS NULL, and
		// COALESCE prevents that path). Anything else 23505 is
		// genuinely unexpected — surface as 500.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
			pgErr.ConstraintName == constraintUsernameLower {
			continue
		}
		return nil, fmt.Errorf("oidc provision (upsert) for email %q: %w", email, err)
	}
	return nil, ErrUsernameExhausted
}

// maskSub returns the first 8 chars of a Keycloak sub for logging. Full
// subs are stable per-user identifiers and we keep them out of logs by
// default; the prefix is enough to grep audit trails without exposing
// the whole value.
func maskSub(sub string) string {
	if len(sub) <= 8 {
		return sub
	}
	return sub[:8]
}

// deriveUsername chooses the best canonical handle from the two pieces
// of identity the BFF can forward — the raw X-Auth-Request-User value
// and the user's email.
//
// oauth2-proxy with the default keycloak-oidc provider forwards the
// JWT `sub` claim in X-Auth-Request-User (NOT preferred_username),
// which is a UUID like "473d8f8e-4583-40ce-8dbf-7c4f1ee2c676". A
// truncated UUID makes a terrible mention handle, so we detect that
// shape and fall back to the email local-part — that's the value
// humans expect to see in the team list and @-mentions.
//
// Preference order:
//  1. sanitizeUsername(raw) when the raw value is not a UUID
//  2. local-part of email
//  3. "" — caller maps to ErrMissingClaims
func deriveUsername(rawUsername, email string) string {
	raw := strings.TrimSpace(rawUsername)
	if !looksLikeUUID(raw) {
		if s := sanitizeUsername(raw); s != "" {
			return s
		}
	}
	if at := strings.IndexByte(email, '@'); at > 0 {
		return sanitizeUsername(email[:at])
	}
	return ""
}

// looksLikeUUID returns true for the 36-char canonical UUID form
// (8-4-4-4-12 hex with dashes). Case-insensitive. Used by
// deriveUsername to detect oauth2-proxy forwarding the Keycloak `sub`
// claim verbatim — that value is unusable as a mention handle and we
// prefer the email local-part instead.
func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') ||
				(c >= 'a' && c <= 'f') ||
				(c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// sanitizeUsername coerces an oauth2-proxy X-Auth-Request-User value into
// the shape required by users.username_check: ^[a-z0-9][a-z0-9_-]{2,29}$.
//
// Real-world inputs we see:
//   - "alice"             — Keycloak `preferred_username` (the happy path)
//   - "alice@example.com" — Keycloak realm with no preferred_username
//     mapper; oauth2-proxy falls back to `email`
//   - "Mixed.Case-Name"  — uppercase / dot-containing handles
//
// Strategy:
//  1. Lowercase + trim whitespace.
//  2. Cut at the first `@` — `alice@example.com` → `alice`.
//  3. Replace any character outside [a-z0-9_-] with `-` (keeps dots in
//     names, dashes, underscores readable; the constraint only allows
//     these three classes plus letters/digits).
//  4. Trim leading non-alphanumerics so the first char matches [a-z0-9]
//     (constraint requires a non-separator start).
//  5. Pad to ≥3 chars with `-` if shorter (e.g. a 2-char preferred_
//     username like "ab" → "ab-").
//  6. Truncate to 30.
//
// Returns "" only when the input is empty or has zero alphanumerics after
// step 4 — the Provisioner treats that as ErrMissingClaims.
func sanitizeUsername(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}
	if at := strings.IndexByte(s, '@'); at >= 0 {
		s = s[:at]
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	s = b.String()
	// Trim leading characters until the first one matches [a-z0-9].
	for len(s) > 0 {
		c := s[0]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			break
		}
		s = s[1:]
	}
	if s == "" {
		return ""
	}
	for len(s) < 3 {
		s += "-"
	}
	if len(s) > 30 {
		s = s[:30]
	}
	return s
}

// signalResolutionChanged publishes the personal signal of a committed
// directory change that moved assignee resolution (sk-622.21 T017, US-11):
// the assignees and authors of the issues naming one of handles, read after
// the change, plus self — the account that held the old handle, i.e. the
// assignee before. inbox, lens and tasks. Best-effort like every personal
// signal: a failed read is logged and gives no signal.
func (p *Provisioner) signalResolutionChanged(ctx context.Context, self uuid.UUID, handles ...string) {
	ids, err := assignee.HandleAddressees(ctx, p.pool, handles...)
	if err != nil {
		slog.WarnContext(ctx, "oidc provision: personal signal addressees unavailable",
			"userId", self.String(), "error", err.Error())
		return
	}
	b := signals.NewBatch()
	for _, uid := range append(ids, self) {
		b.Add(uid, signals.AreaInbox, signals.AreaLens, signals.AreaTasks)
	}
	signals.Flush(ctx, b)
}
