package apikeys_test

// grant_test.go — test matrix row 9 (R-4, US-10, INV-9: the rules of a key
// grant). Reference — the spec, written before the code: the admin
// permission only with scope all; at least one permission (a level or
// admin); a narrowing keeps the level no higher, the scope a subset and the
// expiry no later; while admin stays, the scope cannot narrow to projects or
// spaces.

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/apikeys"
)

func TestGrantValidate(t *testing.T) {
	p := uuid.New()
	s := uuid.New()
	all := apikeys.Scope{Mode: apikeys.ScopeAll}
	cases := []struct {
		name  string
		grant apikeys.Grant
		want  error
	}{
		{"level only", apikeys.Grant{Level: apikeys.LevelRead, Scope: all}, nil},
		{"admin only, scope all", apikeys.Grant{Admin: true, Scope: all}, nil},
		{"manage and admin, scope all", apikeys.Grant{Level: apikeys.LevelManage, Admin: true, Scope: all}, nil},
		{"projects scope", apikeys.Grant{Level: apikeys.LevelWrite, Scope: apikeys.Scope{Mode: apikeys.ScopeProjects, Projects: []uuid.UUID{p}}}, nil},
		{"spaces scope", apikeys.Grant{Level: apikeys.LevelWrite, Scope: apikeys.Scope{Mode: apikeys.ScopeSpaces, Spaces: []uuid.UUID{s}}}, nil},
		{"no permission", apikeys.Grant{Scope: all}, apikeys.ErrNoPermission},
		{"unknown level", apikeys.Grant{Level: "owner", Scope: all}, apikeys.ErrInvalidLevel},
		{"admin with projects", apikeys.Grant{Admin: true, Scope: apikeys.Scope{Mode: apikeys.ScopeProjects, Projects: []uuid.UUID{p}}}, apikeys.ErrAdminNeedsAll},
		{"admin with spaces", apikeys.Grant{Level: apikeys.LevelRead, Admin: true, Scope: apikeys.Scope{Mode: apikeys.ScopeSpaces, Spaces: []uuid.UUID{s}}}, apikeys.ErrAdminNeedsAll},
		{"projects scope without projects", apikeys.Grant{Level: apikeys.LevelRead, Scope: apikeys.Scope{Mode: apikeys.ScopeProjects}}, apikeys.ErrInvalidScope},
		{"spaces scope listing a project", apikeys.Grant{Level: apikeys.LevelRead, Scope: apikeys.Scope{Mode: apikeys.ScopeSpaces, Spaces: []uuid.UUID{s}, Projects: []uuid.UUID{p}}}, apikeys.ErrInvalidScope},
		{"scope all listing a project", apikeys.Grant{Level: apikeys.LevelRead, Scope: apikeys.Scope{Mode: apikeys.ScopeAll, Projects: []uuid.UUID{p}}}, apikeys.ErrInvalidScope},
		{"unknown scope mode", apikeys.Grant{Level: apikeys.LevelRead, Scope: apikeys.Scope{Mode: "instance"}}, apikeys.ErrInvalidScope},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.grant.Validate()
			if c.want == nil {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("Validate = %v, want %v", err, c.want)
			}
		})
	}
}

func TestCheckNarrowing(t *testing.T) {
	p, q := uuid.New(), uuid.New()
	s1, s2 := uuid.New(), uuid.New()
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	at := func(d int) *time.Time { v := now.AddDate(0, 0, d); return &v }

	all := apikeys.Scope{Mode: apikeys.ScopeAll}
	projects := func(ids ...uuid.UUID) apikeys.Scope {
		return apikeys.Scope{Mode: apikeys.ScopeProjects, Projects: ids}
	}
	spaces := func(ids ...uuid.UUID) apikeys.Scope {
		return apikeys.Scope{Mode: apikeys.ScopeSpaces, Spaces: ids}
	}
	write := func(sc apikeys.Scope, exp *time.Time) apikeys.Grant {
		return apikeys.Grant{Level: apikeys.LevelWrite, Scope: sc, ExpiresAt: exp}
	}
	// p lies in s1, q in s2.
	spaceOf := map[uuid.UUID]uuid.UUID{p: s1, q: s2}

	cases := []struct {
		name      string
		cur, next apikeys.Grant
		want      error
	}{
		{"unchanged", write(all, at(30)), write(all, at(30)), nil},
		{"lower level", write(all, at(30)), apikeys.Grant{Level: apikeys.LevelRead, Scope: all, ExpiresAt: at(30)}, nil},
		{"higher level", write(all, at(30)), apikeys.Grant{Level: apikeys.LevelManage, Scope: all, ExpiresAt: at(30)}, apikeys.ErrNotNarrowing},
		{"admin added", write(all, at(30)), apikeys.Grant{Level: apikeys.LevelWrite, Admin: true, Scope: all, ExpiresAt: at(30)}, apikeys.ErrNotNarrowing},
		{"admin dropped", apikeys.Grant{Level: apikeys.LevelWrite, Admin: true, Scope: all, ExpiresAt: at(30)}, write(all, at(30)), nil},
		{"all to projects", write(all, at(30)), write(projects(p), at(30)), nil},
		{"projects subset", write(projects(p, q), at(30)), write(projects(p), at(30)), nil},
		{"projects replaced by another of the same size", write(projects(p), at(30)), write(projects(q), at(30)), apikeys.ErrNotNarrowing},
		{"projects to all", write(projects(p), at(30)), write(all, at(30)), apikeys.ErrNotNarrowing},
		{"projects to spaces", write(projects(p), at(30)), write(spaces(s1), at(30)), apikeys.ErrNotNarrowing},
		{"spaces subset", write(spaces(s1, s2), at(30)), write(spaces(s1), at(30)), nil},
		{"spaces replaced by another of the same size", write(spaces(s1), at(30)), write(spaces(s2), at(30)), apikeys.ErrNotNarrowing},
		{"spaces to a project inside", write(spaces(s1), at(30)), write(projects(p), at(30)), nil},
		{"spaces to a project outside", write(spaces(s1), at(30)), write(projects(q), at(30)), apikeys.ErrNotNarrowing},
		{"admin key narrowed to projects", apikeys.Grant{Admin: true, Scope: all, ExpiresAt: at(30)}, apikeys.Grant{Admin: true, Scope: projects(p), ExpiresAt: at(30)}, apikeys.ErrAdminNeedsAll},
		{"admin key narrowed to spaces", apikeys.Grant{Level: apikeys.LevelRead, Admin: true, Scope: all, ExpiresAt: at(30)}, apikeys.Grant{Level: apikeys.LevelRead, Admin: true, Scope: spaces(s1), ExpiresAt: at(30)}, apikeys.ErrAdminNeedsAll},
		{"earlier expiry", write(all, at(30)), write(all, at(10)), nil},
		{"later expiry", write(all, at(30)), write(all, at(31)), apikeys.ErrNotNarrowing},
		{"expiry removed", write(all, at(30)), write(all, nil), apikeys.ErrNotNarrowing},
		{"expiry set on a key without one", write(all, nil), write(all, at(30)), nil},
		{"no permission left", write(all, at(30)), apikeys.Grant{Scope: all, ExpiresAt: at(30)}, apikeys.ErrNoPermission},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := apikeys.CheckNarrowing(c.cur, c.next, spaceOf)
			if c.want == nil {
				if err != nil {
					t.Fatalf("CheckNarrowing = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("CheckNarrowing = %v, want %v", err, c.want)
			}
		})
	}
}
