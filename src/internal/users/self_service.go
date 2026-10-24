package users

// self_service.go — the users side of the self-service account routes
// (sk-674.18 T038 registration, T039 password reset): the password policy of
// a new password, the account creation transaction and the password set by
// a reset link. accounts.Service decides the answer and calls these
// through accounts.Users; this package owns the user row, the password
// policy and the authentication response, and imports accounts, so the
// transaction lives here.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/accounts"
	"skein-api/internal/assignee"
	"skein-api/internal/auth"
	"skein-api/internal/pwned"
	"skein-api/internal/signals"
	"skein-api/internal/spaces"
)

var _ accounts.Users = (*Store)(nil)

// checkPwned looks password up in the Pwned Passwords corpus when the check
// is enabled: ErrPasswordPwned on a match. An unreachable corpus never
// blocks — it is logged (with logAttrs) and counted as degraded.
func (s *Store) checkPwned(ctx context.Context, password string, logAttrs ...any) error {
	if s.pwnedChecker == nil {
		return nil
	}
	breached, err := s.pwnedChecker.Check(ctx, password)
	switch {
	case err != nil:
		// Unreachable/timed-out breach service must never lock a user
		// out of their own account — log and proceed unblocked.
		slog.WarnContext(ctx, "pwned passwords check unavailable, skipping",
			append(logAttrs, "error", err.Error())...)
		pwned.ChecksTotal.WithLabelValues("degraded").Inc()
		return nil
	case breached:
		pwned.ChecksTotal.WithLabelValues("match").Inc()
		return ErrPasswordPwned
	default:
		pwned.ChecksTotal.WithLabelValues("clean").Inc()
		return nil
	}
}

// CheckNewPassword implements accounts.Users: the policy of the
// self-service password change (length bounds), then the breach corpus;
// path names the route in the log of an unreachable corpus.
func (s *Store) CheckNewPassword(ctx context.Context, password, path string) error {
	if err := validatePassword(password); err != nil {
		return fmt.Errorf("%w: %v", accounts.ErrPasswordWeak, err)
	}
	if err := s.checkPwned(ctx, password, "path", path); err != nil {
		return fmt.Errorf("%w: %v", accounts.ErrPasswordPwned, err)
	}
	return nil
}

// CreateSelfService implements accounts.Users: the registration
// transaction (accounts/signup.go has the whole account). The caller has
// validated the email, username and password; the name is the username,
// the role user, the account not external, and its own creator — the event
// actor is the new account.
func (s *Store) CreateSelfService(ctx context.Context, a accounts.NewAccount) (accounts.AuthResponse, error) {
	hash, err := auth.Hash(a.Password, s.bcryptCost)
	if err != nil {
		return accounts.AuthResponse{}, fmt.Errorf("hashing password: %w", err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return accounts.AuthResponse{}, fmt.Errorf("generating user id: %w", err)
	}

	var resp accounts.AuthResponse
	// SERIALIZABLE and retried like CreateUser (K-7). A retry after a
	// serialisation failure with a concurrent signup of the same email reads
	// that account and answers ErrEmailTaken (E-25).
	err = spaces.WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		// The signups of one email, and of one username, go one after the
		// other (E-25): two of them would otherwise abort each other under
		// SERIALIZABLE — each writes its row before the unique index waits
		// for the other — and the loser could exhaust its retries while the
		// winner is still retrying itself. Transaction-scoped advisory
		// locks, the email's then the username's — one order for every
		// signup — taken before anything else: the second signup waits,
		// fails its first attempt at most once against the committed first
		// one, and its retry reads it — email_taken / username_taken.
		for _, key := range []string{"skein.signup_email:" + a.Email, "skein.signup_username:" + a.Username} {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
				return fmt.Errorf("serialising the signup: %w", err)
			}
		}
		// Signup by invitation (T040): the step's locks next — the
		// invitation's space, as every membership operation takes it (K-7).
		if a.Step != nil {
			if err := a.Step.BeforeInTx(ctx, tx); err != nil {
				return err
			}
		}
		if err := accounts.CheckIdentityFreeInTx(ctx, tx, a.Email, a.Username); err != nil {
			return err
		}
		// The issues whose assignee text names the new account resolve
		// differently after the insert (sk-622.21 T017, as in CreateUser).
		before, err := assignee.HandleAddressees(ctx, tx, a.Username, a.Email, id.String())
		if err != nil {
			return err
		}
		var dto UserDTO
		if err := tx.QueryRow(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, is_external, created_at, terms_accepted_at,
                   email_confirmed_at, metadata)
VALUES ($1, $2, $3, $3, 'user', $4, false, now(), now(), CASE WHEN $5 THEN now() END, '{}'::jsonb)
RETURNING id, email, username, name, role, created_at, is_external`,
			id, a.Email, a.Username, hash, a.EmailConfirmed,
		).Scan(&dto.ID, &dto.Email, &dto.Username, &dto.Name, &dto.Role, &dto.CreatedAt, &dto.IsExternal); err != nil {
			return err
		}
		dto.AuthMethod = "password"
		if err := writeUserEvent(ctx, tx, EventUserCreated, dto.ID, HumanActor(dto.ID, dto.Username), nil, dto); err != nil {
			return err
		}
		// "Personal" only (T020): a self-registered account never joins the
		// common space — INV-22 names the accounts that do (made by an admin,
		// an admin's agent, SSO), SAAS-PLAN 2.6 excludes signups. Its email
		// is not confirmed (US-5, the closed set).
		if err := accounts.ProvisionInTx(ctx, tx, dto.ID, accounts.Provision{SkipCommon: true}); err != nil {
			return err
		}
		// An email confirmed at creation (signup by invitation) needs no
		// confirmation email.
		if !a.EmailConfirmed {
			if err := accounts.QueueConfirmationInTx(ctx, tx, dto.ID, a.Email, a.Language, a.DeliveryTTL); err != nil {
				return err
			}
		}
		if a.Step != nil {
			if err := a.Step.AfterInTx(ctx, tx, dto.ID, dto.Username); err != nil {
				return err
			}
		}
		after, err := assignee.HandleAddressees(ctx, tx, a.Username, a.Email, id.String())
		if err != nil {
			return err
		}
		for _, uid := range append(before, after...) {
			signals.Add(tx, uid, signals.AreaInbox, signals.AreaLens, signals.AreaTasks)
		}
		resp, err = BuildAuthResponse(ctx, tx, dto.ID)
		return err
	})
	if err != nil {
		if name, ok := isUniqueViolation(err); ok {
			switch {
			case strings.Contains(name, "email"):
				return accounts.AuthResponse{}, accounts.ErrEmailTaken
			case strings.Contains(name, "username"):
				return accounts.AuthResponse{}, accounts.ErrUsernameTaken
			}
		}
		if errors.Is(err, accounts.ErrEmailTaken) || errors.Is(err, accounts.ErrUsernameTaken) {
			return accounts.AuthResponse{}, err
		}
		// Two signups of one email (or username) abort each other under
		// SERIALIZABLE — each writes its row before the unique index waits for
		// the other — and one side can exhaust its retries while the other
		// commits. Its answer is then the race's outcome, not a failure
		// (E-25: the other one is email_taken, never a 500): the identity is
		// checked once more outside the transaction. Free still — the error
		// stands.
		if errors.Is(err, spaces.ErrConcurrentChange) {
			if cerr := accounts.CheckIdentityFreeInTx(ctx, s.pool, a.Email, a.Username); errors.Is(cerr, accounts.ErrEmailTaken) ||
				errors.Is(cerr, accounts.ErrUsernameTaken) {
				return accounts.AuthResponse{}, cerr
			}
		}
		return accounts.AuthResponse{}, fmt.Errorf("creating the account: %w", err)
	}
	slog.InfoContext(ctx, "account registered", "userId", id.String(), "byInvitation", a.Step != nil)
	return resp, nil
}

// HashPassword implements accounts.Users.
func (s *Store) HashPassword(password string) (string, error) {
	hash, err := auth.Hash(password, s.bcryptCost)
	if err != nil {
		return "", fmt.Errorf("hashing password: %w", err)
	}
	return hash, nil
}

// CompletePasswordResetInTx implements accounts.Users (sk-674.18 T039): the
// new password hash, the session epoch raised by BumpSessionEpochInTx —
// every session of the user ends, the API keys are not touched (INV-26) —
// the password-changed event with the user as its actor, and the
// authentication response read after the bump, so it carries the new epoch
// (K-16). tx is the transaction that used the reset token.
func (s *Store) CompletePasswordResetInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, hash string) (accounts.AuthResponse, error) {
	var username string
	err := tx.QueryRow(ctx, `
UPDATE users SET password_hash = $2 WHERE id = $1 RETURNING username`, userID, hash).Scan(&username)
	if errors.Is(err, pgx.ErrNoRows) {
		return accounts.AuthResponse{}, ErrUserNotFound
	}
	if err != nil {
		return accounts.AuthResponse{}, fmt.Errorf("setting the reset password: %w", err)
	}
	if _, err := BumpSessionEpochInTx(ctx, tx, userID); err != nil {
		return accounts.AuthResponse{}, err
	}
	if err := writeUserEvent(ctx, tx, EventUserPasswordChanged, userID, HumanActor(userID, username), nil, map[string]any{
		"userId": userID.String(), "via": "reset",
	}); err != nil {
		return accounts.AuthResponse{}, err
	}
	return BuildAuthResponse(ctx, tx, userID)
}
