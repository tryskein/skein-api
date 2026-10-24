package users

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/accounts"
)

// RowQuerier is the part of *pgxpool.Pool / pgx.Tx BuildAuthResponse reads
// through.
type RowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// BuildAuthResponse is the one builder of the authentication response
// (accounts.AuthResponse, contract T012; sk-674.18 K-16): the body of every
// answer after which skein-web issues or re-issues a session — password
// login, OIDC provisioning, and (T038/T039/T040) signup, signup by
// invitation and a completed password reset. It reads the user by primary
// key AFTER whatever the request changed (call it inside the request's
// transaction when the request raised the epoch), so `sessionEpoch` is the
// current value — never an older one and never a default. ErrUserNotFound
// when the user does not exist.
func BuildAuthResponse(ctx context.Context, q RowQuerier, userID uuid.UUID) (accounts.AuthResponse, error) {
	var (
		resp      accounts.AuthResponse
		id        uuid.UUID
		createdAt time.Time
		lastLogin *time.Time
	)
	err := q.QueryRow(ctx, `
SELECT id, email, username, name, role, is_external, created_at, last_login_at, session_epoch
  FROM users WHERE id = $1`, userID).Scan(&id, &resp.Email, &resp.Username, &resp.Name, &resp.Role,
		&resp.IsExternal, &createdAt, &lastLogin, &resp.SessionEpoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return accounts.AuthResponse{}, ErrUserNotFound
	}
	if err != nil {
		return accounts.AuthResponse{}, fmt.Errorf("building the authentication response of %s: %w", userID, err)
	}
	resp.ID = id.String()
	resp.CreatedAt = createdAt.UTC()
	if lastLogin != nil {
		t := lastLogin.UTC()
		resp.LastLoginAt = &t
	}
	return resp, nil
}
