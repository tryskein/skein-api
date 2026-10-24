// Package uiprefs holds the caller's personal interface-appearance
// settings (sk-551.5): one lazily-created row per user in user_ui_prefs.
//
// Three settings so far: the set of task views in which a selected issue
// opens as a centred pop-up window instead of a pane beside the view
// (sk-551.5), the interface density, normal or compact (sk-551.7), and the
// left-panel layout mode, columns or sections (sk-551.8). The table takes
// further appearance settings as new columns; the contract is therefore a
// PARTIAL update — a field the request does not name keeps its stored
// value.
package uiprefs

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The three task views, spelled the way the frontend's `ViewMode` spells
// them and the way the schema's CHECK (user_ui_prefs_issue_modal_views_chk)
// admits them.
const (
	// ViewCards is the cards list.
	ViewCards = "cards"
	// ViewKanban is the board.
	ViewKanban = "kanban"
	// ViewTree is the tree table.
	ViewTree = "tree"
)

// viewOrder is the canonical order of a stored and returned set. A stable
// order makes the response independent of the order the client happened to
// send, so two clients comparing sets never see a spurious difference.
var viewOrder = [...]string{ViewCards, ViewKanban, ViewTree}

// UnknownViewError rejects a view name outside the vocabulary. It is a
// request-shape error (the handler answers 400): the vocabulary is closed
// and known to every client, so an unknown name is a malformed request, not
// a business refusal.
type UnknownViewError struct {
	// View is the offending name as the client sent it.
	View string
}

func (e *UnknownViewError) Error() string {
	return fmt.Sprintf("unknown view %q: expected one of cards, kanban, tree", e.View)
}

// NormalizeIssueModalViews validates a set of view names and returns it in
// canonical form: duplicates dropped, order cards → kanban → tree. A nil or
// empty input yields an empty, non-nil slice, so the JSON answer is `[]`
// and never `null`. The first unknown name is reported as
// *UnknownViewError.
func NormalizeIssueModalViews(in []string) ([]string, error) {
	seen := make(map[string]bool, len(viewOrder))
	for _, v := range in {
		known := false
		for _, k := range viewOrder {
			if v == k {
				known = true
				break
			}
		}
		if !known {
			return nil, &UnknownViewError{View: v}
		}
		seen[v] = true
	}
	out := make([]string, 0, len(seen))
	for _, k := range viewOrder {
		if seen[k] {
			out = append(out, k)
		}
	}
	return out, nil
}

// The two interface densities, spelled the way the frontend writes them
// into `<html data-density>` and the way the schema's CHECK
// (user_ui_prefs_density_chk) admits them.
const (
	// DensityNormal is the factory density — today's sizes.
	DensityNormal = "normal"
	// DensityCompact is the smaller type scale, lower controls and chips,
	// tighter paddings.
	DensityCompact = "compact"
)

// UnknownDensityError rejects a density outside the vocabulary. Like
// UnknownViewError it is a request-shape error (400): the vocabulary is
// closed and known to every client.
type UnknownDensityError struct {
	// Density is the offending value as the client sent it.
	Density string
}

func (e *UnknownDensityError) Error() string {
	return fmt.Sprintf("unknown density %q: expected normal or compact", e.Density)
}

// ValidateDensity accepts exactly normal and compact — case-sensitive, no
// trimming, the way the schema's CHECK does — and reports anything else
// as *UnknownDensityError.
func ValidateDensity(d string) error {
	if d == DensityNormal || d == DensityCompact {
		return nil
	}
	return &UnknownDensityError{Density: d}
}

// The two left-panel layout modes, spelled the way the frontend reads them
// and the way the schema's CHECK (user_ui_prefs_nav_layout_chk) admits them.
const (
	// NavLayoutColumns is the factory mode: the «Queues» and «Projects»
	// navigation columns side by side, today's layout.
	NavLayoutColumns = "columns"
	// NavLayoutSections stacks «Projects» and «Queues» as two sections of a
	// single navigation column.
	NavLayoutSections = "sections"
)

// UnknownNavLayoutError rejects a layout mode outside the vocabulary. Like
// UnknownDensityError it is a request-shape error (400): the vocabulary is
// closed and known to every client.
type UnknownNavLayoutError struct {
	// NavLayout is the offending value as the client sent it.
	NavLayout string
}

func (e *UnknownNavLayoutError) Error() string {
	return fmt.Sprintf("unknown nav layout %q: expected columns or sections", e.NavLayout)
}

// ValidateNavLayout accepts exactly columns and sections — case-sensitive,
// no trimming, the way the schema's CHECK does — and reports anything
// else, the empty string and a value with surrounding spaces included, as
// *UnknownNavLayoutError.
func ValidateNavLayout(l string) error {
	if l == NavLayoutColumns || l == NavLayoutSections {
		return nil
	}
	return &UnknownNavLayoutError{NavLayout: l}
}

// PrefsDTO is the wire shape of GET/PUT /api/v1/me/ui-prefs.
type PrefsDTO struct {
	// IssueModalViews is the set of task views (cards, kanban, tree) in
	// which a selected issue opens as a centred pop-up window instead of a
	// pane beside the view. Always present, never null; empty means "the
	// pane beside the view everywhere" — the factory position. Ordered
	// cards, kanban, tree; no duplicates.
	IssueModalViews []string `json:"issueModalViews" example:"kanban,tree"`
	// Density is the interface density: normal (the factory position,
	// today's sizes) or compact (smaller type, lower controls and chips,
	// tighter paddings). Always present; normal for someone who never
	// chose.
	Density string `json:"density" enums:"normal,compact" example:"compact"`
	// NavLayout is the left-panel layout mode: columns (the factory
	// position — the «Queues» and «Projects» columns side by side) or
	// sections (both as sections of one column). Always present; columns
	// for someone who never chose.
	NavLayout string `json:"navLayout" enums:"columns,sections" example:"sections"`
}

// UpdateInput is a partial update: a nil field keeps its stored value.
type UpdateInput struct {
	// IssueModalViews, when non-nil, replaces the stored set. It must
	// already be validated; the store normalizes it again anyway, so a
	// caller cannot write a non-canonical set.
	IssueModalViews *[]string
	// Density, when non-nil, replaces the stored density. Validated again
	// by the store.
	Density *string
	// NavLayout, when non-nil, replaces the stored layout mode. Validated
	// again by the store.
	NavLayout *string
}

// Store reads and writes user_ui_prefs.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wraps the shared pool. Goroutine-safe.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Get returns the caller's settings. A user without a row — everyone who
// has never touched the settings — gets the factory position: an empty set,
// the normal density and the columns layout.
func (s *Store) Get(ctx context.Context, userID uuid.UUID) (PrefsDTO, error) {
	var (
		views     []string
		density   string
		navLayout string
	)
	err := s.pool.QueryRow(ctx,
		`SELECT issue_modal_views, density, nav_layout FROM user_ui_prefs WHERE user_id = $1`, userID,
	).Scan(&views, &density, &navLayout)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PrefsDTO{IssueModalViews: []string{}, Density: DensityNormal, NavLayout: NavLayoutColumns}, nil
		}
		return PrefsDTO{}, fmt.Errorf("reading ui prefs of user %s: %w", userID, err)
	}
	// The schema admits only known names, so this cannot fail on stored
	// data; it is here for the order and the duplicates, which the schema
	// does not hold.
	norm, err := NormalizeIssueModalViews(views)
	if err != nil {
		return PrefsDTO{}, fmt.Errorf("stored ui prefs of user %s: %w", userID, err)
	}
	return PrefsDTO{IssueModalViews: norm, Density: density, NavLayout: navLayout}, nil
}

// Update applies a partial update in one upsert, creating the row when the
// user has none, and returns the resulting settings. A field the input does
// not name keeps its stored value — or, when the row is being created, the
// column default (the factory position). An input that names no field
// writes nothing and returns the current settings.
//
// Every field is validated before anything is written, so a request that
// carries one good and one bad field changes nothing.
func (s *Store) Update(ctx context.Context, userID uuid.UUID, in UpdateInput) (PrefsDTO, error) {
	if in.IssueModalViews == nil && in.Density == nil && in.NavLayout == nil {
		return s.Get(ctx, userID)
	}
	// A nil slice / nil string reaches SQL as NULL, and COALESCE below
	// reads NULL as «not named» — keep the stored value (on INSERT: the
	// column default).
	var views []string
	if in.IssueModalViews != nil {
		norm, err := NormalizeIssueModalViews(*in.IssueModalViews)
		if err != nil {
			return PrefsDTO{}, err
		}
		views = norm
	}
	var density *string
	if in.Density != nil {
		if err := ValidateDensity(*in.Density); err != nil {
			return PrefsDTO{}, err
		}
		density = in.Density
	}
	var navLayout *string
	if in.NavLayout != nil {
		if err := ValidateNavLayout(*in.NavLayout); err != nil {
			return PrefsDTO{}, err
		}
		navLayout = in.NavLayout
	}
	const upsert = `
INSERT INTO user_ui_prefs (user_id, issue_modal_views, density, nav_layout, updated_at)
VALUES ($1,
        COALESCE($2::text[], '{}'::text[]),
        COALESCE($3::text, 'normal'),
        COALESCE($4::text, 'columns'),
        now())
ON CONFLICT (user_id) DO UPDATE
   SET issue_modal_views = COALESCE($2::text[], user_ui_prefs.issue_modal_views),
       density           = COALESCE($3::text, user_ui_prefs.density),
       nav_layout        = COALESCE($4::text, user_ui_prefs.nav_layout),
       updated_at        = now()
RETURNING issue_modal_views, density, nav_layout`
	var (
		storedViews     []string
		storedDensity   string
		storedNavLayout string
	)
	if err := s.pool.QueryRow(ctx, upsert, userID, views, density, navLayout).
		Scan(&storedViews, &storedDensity, &storedNavLayout); err != nil {
		return PrefsDTO{}, fmt.Errorf("saving ui prefs of user %s: %w", userID, err)
	}
	norm, err := NormalizeIssueModalViews(storedViews)
	if err != nil {
		return PrefsDTO{}, fmt.Errorf("stored ui prefs of user %s: %w", userID, err)
	}
	return PrefsDTO{IssueModalViews: norm, Density: storedDensity, NavLayout: storedNavLayout}, nil
}
