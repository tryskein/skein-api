package themes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgUniqueViolation is the SQLSTATE class for "duplicate key value
// violates unique constraint" — emitted by Postgres when an INSERT/
// UPDATE on `themes.name` collides with an existing row. Pattern-
// matched in CreateTheme / UpdateTheme to translate raw DB errors
// into the typed ErrNameTaken sentinel.
const pgUniqueViolation = "23505"

// pgForeignKeyViolation is the SQLSTATE class for an FK violation —
// emitted when SetUserThemePref tries to point a user at a
// custom_theme_id that doesn't exist in `themes`. We translate to
// ErrCustomThemeNotFound so the handler returns 404.
const pgForeignKeyViolation = "23503"

// Sentinel errors — handlers branch on these via errors.Is.
var (
	// ErrNotFound is returned by Get / Update / Delete when the theme
	// row doesn't exist OR belongs to another author — the two are one
	// answer (INV-3, sk-674.18 T035: a theme is its author's, US-21).
	// Maps to 404 in the handler.
	ErrNotFound = errors.New("theme not found")

	// ErrNameTaken is returned by Create / Update when the name
	// collides with another theme OF THE SAME AUTHOR (the unique index
	// idx_themes_author_name_lower; another author's theme of that name
	// never collides). Maps to 422 name_taken.
	ErrNameTaken = errors.New("theme name already taken")

	// ErrCustomThemeRequired is returned by SetUserThemePref when
	// mode="custom" but customThemeID is nil. Maps to 400.
	ErrCustomThemeRequired = errors.New("customThemeId required when mode=custom")

	// ErrCustomThemeNotFound is returned by SetUserThemePref when the
	// supplied customThemeID doesn't exist in themes or is another
	// author's (INV-3). Maps to 404.
	ErrCustomThemeNotFound = errors.New("custom theme not found")
)

// Store wraps the pgxpool with the queries the themes handler family
// needs. Build once at startup; reuse across requests.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a Store bound to pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// themeRow is the projection used by every SELECT — the row plus
// LEFT JOIN'd username for createdBy + updatedBy. Kept as an internal
// type so the public API exposes only ThemeDTO.
type themeRow struct {
	ID            uuid.UUID
	Name          string
	Base          string
	VariablesJSON []byte
	CreatedBy     uuid.UUID
	CreatedByName *string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	UpdatedBy     uuid.UUID
	UpdatedByName *string
}

// CreateTheme inserts a row authored by in.CreatedBy (author_id — the
// owner, sk-674.18 T035). ON unique-violation (the author already has a
// theme of that name) returns ErrNameTaken. Caller has already validated
// every field via ValidateName / ValidateBase / ValidateVariables.
func (s *Store) CreateTheme(ctx context.Context, in CreateThemeInput) (*ThemeDTO, error) {
	variablesJSON, err := marshalVariables(in.Variables)
	if err != nil {
		return nil, err
	}

	// gen_random_uuid() is the table default — we let the DB mint the
	// id so a future schema change (uuidv7) is a one-place edit. The
	// single-row INSERT … RETURNING + post-hoc username lookup is the
	// same pattern used by the projects/components stores; it keeps
	// the INSERT SQL trivial and reuses scanThemeDTO for the GET path.
	const insertSQL = `
INSERT INTO themes (name, base, variables, created_by, updated_by, author_id)
VALUES ($1, $2, $3::jsonb, $4, $4, $4)
RETURNING id, name, base, variables, created_by, created_at, updated_at, updated_by`

	var row themeRow
	if err := s.pool.QueryRow(ctx, insertSQL, in.Name, in.Base, string(variablesJSON), in.CreatedBy).Scan(
		&row.ID, &row.Name, &row.Base, &row.VariablesJSON,
		&row.CreatedBy, &row.CreatedAt, &row.UpdatedAt, &row.UpdatedBy,
	); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrNameTaken
		}
		return nil, fmt.Errorf("inserting theme: %w", err)
	}
	return s.hydrateAndBuildDTO(ctx, &row)
}

// GetTheme fetches one of owner's themes by id. Returns ErrNotFound when
// it is missing or another author's — one answer (INV-3).
func (s *Store) GetTheme(ctx context.Context, id, owner uuid.UUID) (*ThemeDTO, error) {
	const sql = `
SELECT t.id, t.name, t.base, t.variables,
       t.created_by, uc.username,
       t.created_at, t.updated_at,
       t.updated_by, uu.username
  FROM themes t
  LEFT JOIN users uc ON uc.id = t.created_by
  LEFT JOIN users uu ON uu.id = t.updated_by
 WHERE t.id = $1 AND t.author_id = $2`
	var row themeRow
	if err := s.pool.QueryRow(ctx, sql, id, owner).Scan(
		&row.ID, &row.Name, &row.Base, &row.VariablesJSON,
		&row.CreatedBy, &row.CreatedByName,
		&row.CreatedAt, &row.UpdatedAt,
		&row.UpdatedBy, &row.UpdatedByName,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("getting theme: %w", err)
	}
	return buildDTO(&row)
}

// ListThemes returns owner's themes, newest-update first. A theme is its
// author's (sk-674.18 T035, US-21): another author's theme — its name, its
// author — never appears. No archived state and few rows per author, so
// no pagination.
func (s *Store) ListThemes(ctx context.Context, owner uuid.UUID) ([]ThemeDTO, error) {
	const sql = `
SELECT t.id, t.name, t.base, t.variables,
       t.created_by, uc.username,
       t.created_at, t.updated_at,
       t.updated_by, uu.username
  FROM themes t
  LEFT JOIN users uc ON uc.id = t.created_by
  LEFT JOIN users uu ON uu.id = t.updated_by
 WHERE t.author_id = $1
 ORDER BY t.updated_at DESC, t.id`
	rows, err := s.pool.Query(ctx, sql, owner)
	if err != nil {
		return nil, fmt.Errorf("listing themes: %w", err)
	}
	defer rows.Close()

	out := make([]ThemeDTO, 0)
	for rows.Next() {
		var row themeRow
		if err := rows.Scan(
			&row.ID, &row.Name, &row.Base, &row.VariablesJSON,
			&row.CreatedBy, &row.CreatedByName,
			&row.CreatedAt, &row.UpdatedAt,
			&row.UpdatedBy, &row.UpdatedByName,
		); err != nil {
			return nil, fmt.Errorf("scanning theme row: %w", err)
		}
		dto, err := buildDTO(&row)
		if err != nil {
			return nil, err
		}
		out = append(out, *dto)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating themes: %w", err)
	}
	return out, nil
}

// UpdateTheme applies a partial patch. Nil fields are skipped. The
// `updated_by` column is always overwritten with the caller's uuid
// (passed via in.UpdatedBy) even when no value-fields change, because
// touching the row to bump updated_at counts as an action. Returns
// ErrNotFound on a missing id or another author's theme (only the author
// edits it — in.UpdatedBy is the caller, sk-674.18 T035), ErrNameTaken on
// a collision with another theme of the same author.
//
// Implementation is a single UPDATE … RETURNING inside a SERIALIZABLE
// tx (no separate SELECT-for-update). Concurrent rename collisions
// surface as 23505 → ErrNameTaken.
func (s *Store) UpdateTheme(ctx context.Context, id uuid.UUID, in UpdateThemeInput) (*ThemeDTO, error) {
	// Marshal variables outside the tx — JSON encoding is CPU-only.
	var variablesArg any
	if in.Variables != nil {
		b, err := marshalVariables(*in.Variables)
		if err != nil {
			return nil, err
		}
		variablesArg = string(b)
	}

	const sql = `
UPDATE themes
   SET name       = COALESCE($2, name),
       base       = COALESCE($3, base),
       variables  = COALESCE($4::jsonb, variables),
       updated_by = $5,
       updated_at = now()
 WHERE id = $1 AND author_id = $5
RETURNING id, name, base, variables, created_by, created_at, updated_at, updated_by`

	var row themeRow
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, id, in.Name, in.Base, variablesArg, in.UpdatedBy).Scan(
			&row.ID, &row.Name, &row.Base, &row.VariablesJSON,
			&row.CreatedBy, &row.CreatedAt, &row.UpdatedAt, &row.UpdatedBy,
		)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if isUniqueViolation(err) {
			return nil, ErrNameTaken
		}
		return nil, fmt.Errorf("updating theme: %w", err)
	}
	return s.hydrateAndBuildDTO(ctx, &row)
}

// DeleteTheme drops one of owner's themes by id. Returns ErrNotFound when
// no row matched — missing or another author's, one answer (INV-3). The ON DELETE SET NULL on users.custom_theme_id means any
// user pointing at this theme cleanly degrades back to builtin (the
// frontend re-reads /me/theme on its next render and sees mode unchanged
// but customThemeId == NULL — it falls back to builtinTheme).
func (s *Store) DeleteTheme(ctx context.Context, id, owner uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM themes WHERE id = $1 AND author_id = $2`, id, owner)
	if err != nil {
		return fmt.Errorf("deleting theme: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GetUserThemePref reads the three theme columns from the users row.
// Returns ErrNotFound when the user doesn't exist — should never
// happen in practice because the caller is auth-resolved before this
// runs, but defensive against orphaned cookies.
func (s *Store) GetUserThemePref(ctx context.Context, userID uuid.UUID) (*UserThemePrefDTO, error) {
	const sql = `
SELECT theme_mode, builtin_theme, custom_theme_id
  FROM users
 WHERE id = $1`
	var (
		mode     string
		builtin  string
		customID *uuid.UUID
	)
	if err := s.pool.QueryRow(ctx, sql, userID).Scan(&mode, &builtin, &customID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("getting user theme pref: %w", err)
	}
	return &UserThemePrefDTO{
		Mode:          mode,
		BuiltinTheme:  builtin,
		CustomThemeID: customID,
	}, nil
}

// SetUserThemePref replaces all three theme columns on the user row.
//
//   - mode="builtin" → customThemeID is forced NULL (we don't keep a
//     dangling reference even if the caller sent one — the resolved
//     state is always "what the frontend would render", which is
//     determined by mode).
//   - mode="custom"  → customThemeID is required. Nil → 400. Pointed
//     at a theme that does not exist or is another author's →
//     ErrCustomThemeNotFound, one answer (sk-674.18 T035, INV-3): a user
//     selects only the built-ins and his own themes.
//
// Returns the post-state DTO so the handler can echo back the canonical
// values (in particular, the NULL-on-builtin coercion).
func (s *Store) SetUserThemePref(ctx context.Context, userID uuid.UUID, in SetThemePrefInput) (*UserThemePrefDTO, error) {
	mode := in.Mode
	builtin := in.BuiltinTheme
	var customID *uuid.UUID
	if mode == "custom" {
		if in.CustomThemeID == nil {
			return nil, ErrCustomThemeRequired
		}
		id := *in.CustomThemeID
		customID = &id
	}
	// For builtin we explicitly null-out customID — see the doc above.
	if customID != nil {
		var own bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM themes WHERE id = $1 AND author_id = $2)`,
			*customID, userID).Scan(&own); err != nil {
			return nil, fmt.Errorf("checking custom theme %s: %w", *customID, err)
		}
		if !own {
			return nil, ErrCustomThemeNotFound
		}
	}

	const sql = `
UPDATE users
   SET theme_mode      = $2,
       builtin_theme   = $3,
       custom_theme_id = $4
 WHERE id = $1
RETURNING theme_mode, builtin_theme, custom_theme_id`

	var (
		outMode    string
		outBuiltin string
		outCustom  *uuid.UUID
	)
	if err := s.pool.QueryRow(ctx, sql, userID, mode, builtin, customID).Scan(&outMode, &outBuiltin, &outCustom); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if isForeignKeyViolation(err) {
			return nil, ErrCustomThemeNotFound
		}
		return nil, fmt.Errorf("setting user theme pref: %w", err)
	}
	return &UserThemePrefDTO{
		Mode:          outMode,
		BuiltinTheme:  outBuiltin,
		CustomThemeID: outCustom,
	}, nil
}

// ---------- helpers ----------

// hydrateAndBuildDTO loads usernames for createdBy / updatedBy via a
// single users SELECT and builds the wire DTO. INSERT/UPDATE … RETURNING
// can't natively JOIN; doing the lookup separately keeps the primary
// SQL small.
func (s *Store) hydrateAndBuildDTO(ctx context.Context, row *themeRow) (*ThemeDTO, error) {
	ids := []uuid.UUID{row.CreatedBy}
	if row.UpdatedBy != row.CreatedBy {
		ids = append(ids, row.UpdatedBy)
	}
	usernames := map[uuid.UUID]string{}
	const sql = `SELECT id, username FROM users WHERE id = ANY($1)`
	rows, err := s.pool.Query(ctx, sql, ids)
	if err != nil {
		return nil, fmt.Errorf("resolving theme authors: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id   uuid.UUID
			name string
		)
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("scanning theme author: %w", err)
		}
		usernames[id] = name
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating theme authors: %w", err)
	}
	createdName := usernames[row.CreatedBy]
	updatedName := usernames[row.UpdatedBy]
	row.CreatedByName = &createdName
	row.UpdatedByName = &updatedName
	return buildDTO(row)
}

// buildDTO converts the internal row into the wire DTO. Variables is
// always emitted as a non-nil map (zero-length when the column was
// `{}`) so the frontend never has to special-case null.
func buildDTO(row *themeRow) (*ThemeDTO, error) {
	vars, err := unmarshalVariables(row.VariablesJSON)
	if err != nil {
		return nil, err
	}
	return &ThemeDTO{
		ID:        row.ID,
		Name:      row.Name,
		Base:      row.Base,
		Variables: vars,
		CreatedBy: UserRef{ID: row.CreatedBy, Username: deref(row.CreatedByName)},
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
		UpdatedBy: UserRef{ID: row.UpdatedBy, Username: deref(row.UpdatedByName)},
	}, nil
}

// deref turns *string into string with empty-on-nil. Used to flatten
// the LEFT JOIN'd users.username column — a deleted user (which
// shouldn't happen due to ON DELETE RESTRICT, but defence in depth)
// renders as an empty string in the UserRef.Username field.
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func marshalVariables(vars map[string]string) ([]byte, error) {
	if vars == nil {
		vars = map[string]string{}
	}
	b, err := json.Marshal(vars)
	if err != nil {
		return nil, fmt.Errorf("marshalling variables: %w", err)
	}
	return b, nil
}

func unmarshalVariables(raw []byte) (map[string]string, error) {
	out := map[string]string{}
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("unmarshalling variables: %w", err)
	}
	return out, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgForeignKeyViolation
}
