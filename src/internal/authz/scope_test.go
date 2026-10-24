package authz

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/auth"
)

// The scope names WHOSE relation a request reads, never how far it reaches:
// an admin's role on the context is not a grant (D-2), an undeclared
// context reads nobody's relation (deny by default), and only a declared
// internal context is unrestricted.
func TestFromContext(t *testing.T) {
	id := uuid.Must(uuid.NewRandom())
	tests := []struct {
		name             string
		ctx              func() context.Context
		wantUnrestricted bool
		wantCaller       uuid.UUID
	}{
		{
			name: "no principal, not declared",
			ctx:  context.Background,
		},
		{
			name:             "declared internal",
			ctx:              func() context.Context { return WithUnrestricted(context.Background()) },
			wantUnrestricted: true,
		},
		{
			name: "plain user",
			ctx: func() context.Context {
				return auth.WithUserRole(auth.WithUserID(context.Background(), id), "user")
			},
			wantCaller: id,
		},
		{
			// The admin's reach is the relation's 'admin' level; the context
			// role does not short-circuit the predicate.
			name: "admin reads its own relation",
			ctx: func() context.Context {
				return auth.WithUserRole(auth.WithUserID(context.Background(), id), "admin")
			},
			wantCaller: id,
		},
		{
			name: "external admin",
			ctx: func() context.Context {
				return auth.WithIsExternal(auth.WithUserRole(auth.WithUserID(context.Background(), id), "admin"), true)
			},
			wantCaller: id,
		},
		{
			// A marker left on a context that later acquired a user must not
			// promote that user.
			name: "principal wins over the internal marker",
			ctx: func() context.Context {
				return auth.WithUserRole(auth.WithUserID(WithUnrestricted(context.Background()), id), "user")
			},
			wantCaller: id,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := FromContext(tc.ctx())
			if s.IsUnrestricted() != tc.wantUnrestricted {
				t.Errorf("IsUnrestricted = %v, want %v", s.IsUnrestricted(), tc.wantUnrestricted)
			}
			if s.CallerID() != tc.wantCaller {
				t.Errorf("CallerID = %v, want %v", s.CallerID(), tc.wantCaller)
			}
		})
	}
}

func TestUnrestrictedAndForUser(t *testing.T) {
	if s := Unrestricted(); !s.IsUnrestricted() || s.CallerID() != uuid.Nil {
		t.Errorf("Unrestricted() = (%v, %v), want (true, nil uuid)", s.IsUnrestricted(), s.CallerID())
	}
	id := uuid.Must(uuid.NewRandom())
	if s := ForUser(id); s.IsUnrestricted() || s.CallerID() != id {
		t.Errorf("ForUser(id) = (%v, %v), want (false, %v)", s.IsUnrestricted(), s.CallerID(), id)
	}
}

// relationProbe stands in for the database behind Scope.Bind: it records
// whose relation was asked for and answers an empty set. Bind reads the
// relation only for a real caller; every other branch answers on its own.
type relationProbe struct{ asked []uuid.UUID }

func (p *relationProbe) Query(_ context.Context, _ string, args ...any) (pgx.Rows, error) {
	if len(args) == 1 {
		if id, ok := args[0].(uuid.UUID); ok {
			p.asked = append(p.asked, id)
		}
	}
	return nil, errProbeAnswered
}

var errProbeAnswered = errors.New("relation probe: answered")

// TestBind_VisibilityOfEachScope — matrix row 16 (INV-2): a scope with no
// principal sees nothing and an undeclared context is not internal work;
// every project only behind the explicit WithUnrestricted marker; a nil
// principal opens nothing; a caller — an admin included, and a caller whose
// context also carries the marker — reads its own relation and nothing
// wider.
func TestBind_VisibilityOfEachScope(t *testing.T) {
	id := uuid.Must(uuid.NewRandom())
	tests := []struct {
		name             string
		ctx              context.Context
		wantUnrestricted bool
		wantAsked        []uuid.UUID // whose relation Bind reads; nil — none
	}{
		{name: "no principal, not declared", ctx: context.Background()},
		{name: "nil principal", ctx: auth.WithUserRole(auth.WithUserID(context.Background(), uuid.Nil), "admin")},
		{name: "declared internal", ctx: WithUnrestricted(context.Background()), wantUnrestricted: true},
		{name: "plain user", ctx: auth.WithUserRole(auth.WithUserID(context.Background(), id), "user"), wantAsked: []uuid.UUID{id}},
		{name: "admin", ctx: auth.WithUserRole(auth.WithUserID(context.Background(), id), "admin"), wantAsked: []uuid.UUID{id}},
		{
			name:      "principal and marker",
			ctx:       auth.WithUserRole(auth.WithUserID(WithUnrestricted(context.Background()), id), "user"),
			wantAsked: []uuid.UUID{id},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			probe := &relationProbe{}
			unrestricted, visible, err := FromContext(tc.ctx).Bind(tc.ctx, probe)
			if unrestricted != tc.wantUnrestricted {
				t.Errorf("unrestricted = %v, want %v", unrestricted, tc.wantUnrestricted)
			}
			if !slices.Equal(probe.asked, tc.wantAsked) {
				t.Errorf("relation read for %v, want %v", probe.asked, tc.wantAsked)
			}
			if tc.wantAsked != nil {
				// The caller's answer is the relation's: here the probe's
				// refusal, never a set made up without it.
				if !errors.Is(err, errProbeAnswered) {
					t.Errorf("Bind error = %v, want the relation's answer", err)
				}
				return
			}
			if err != nil || visible == nil || len(visible) != 0 {
				t.Errorf("Bind = (%v, %v, %v), want an empty visible set and no error", unrestricted, visible, err)
			}
		})
	}
}
