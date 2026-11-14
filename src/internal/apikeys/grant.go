package apikeys

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
)

// Level is the permission level of a key. A higher level includes the lower
// ones: read ⊂ write ⊂ manage. LevelNone is a key without a level, allowed
// only together with the admin flag.
type Level string

const (
	LevelNone   Level = ""
	LevelRead   Level = "read"
	LevelWrite  Level = "write"
	LevelManage Level = "manage"
)

// rank orders the levels; -1 for a value outside the closed set.
func (l Level) rank() int {
	switch l {
	case LevelNone:
		return 0
	case LevelRead:
		return 1
	case LevelWrite:
		return 2
	case LevelManage:
		return 3
	}
	return -1
}

// ScopeMode is the kind of a key's scope.
type ScopeMode string

const (
	ScopeProjects ScopeMode = "projects"
	ScopeSpaces   ScopeMode = "spaces"
	ScopeAll      ScopeMode = "all"
)

// Scope is the set of objects a key reaches: the listed projects, the
// projects of the listed spaces, or everything its owner reaches.
type Scope struct {
	Mode     ScopeMode
	Projects []uuid.UUID
	Spaces   []uuid.UUID
}

// Grant is what a key is allowed: a level, the independent admin flag, a
// scope and the end of its lifetime. ExpiresAt is nil only on a key created
// before lifetimes existed.
type Grant struct {
	Level     Level
	Admin     bool
	Scope     Scope
	ExpiresAt *time.Time
}

// Grant validation errors. Each names the rule the grant breaks; callers
// compare with errors.Is.
var (
	ErrInvalidLevel     = errors.New("unknown key permission level")
	ErrNoPermission     = errors.New("key grants no permission")
	ErrInvalidScope     = errors.New("invalid key scope")
	ErrAdminNeedsAll    = errors.New("admin permission requires scope all")
	ErrNotNarrowing     = errors.New("change does not narrow the key")
	ErrInvalidExpiry    = errors.New("invalid key expiry")
	ErrScopeUnavailable = errors.New("scope member does not exist")
)

// Validate checks the rules every grant obeys: a known level, at least one
// permission (a level or the admin flag), a well-formed scope, and the admin
// flag only with scope all. It does not look at the expiry.
func (g Grant) Validate() error {
	if g.Level.rank() < 0 {
		return fmt.Errorf("%w: %q", ErrInvalidLevel, g.Level)
	}
	if g.Level == LevelNone && !g.Admin {
		return ErrNoPermission
	}
	switch g.Scope.Mode {
	case ScopeAll:
		if len(g.Scope.Projects) > 0 || len(g.Scope.Spaces) > 0 {
			return fmt.Errorf("%w: scope all lists no members", ErrInvalidScope)
		}
	case ScopeProjects:
		if len(g.Scope.Projects) == 0 || len(g.Scope.Spaces) > 0 {
			return fmt.Errorf("%w: scope projects lists one or more projects and no spaces", ErrInvalidScope)
		}
	case ScopeSpaces:
		if len(g.Scope.Spaces) == 0 || len(g.Scope.Projects) > 0 {
			return fmt.Errorf("%w: scope spaces lists one or more spaces and no projects", ErrInvalidScope)
		}
	default:
		return fmt.Errorf("%w: unknown mode %q", ErrInvalidScope, g.Scope.Mode)
	}
	if g.Admin && g.Scope.Mode != ScopeAll {
		return ErrAdminNeedsAll
	}
	return nil
}

// normalized returns the grant with its scope members deduplicated and
// sorted, so two grants naming the same members compare equal.
func (g Grant) normalized() Grant {
	g.Scope.Projects = sortedUnique(g.Scope.Projects)
	g.Scope.Spaces = sortedUnique(g.Scope.Spaces)
	return g
}

func sortedUnique(ids []uuid.UUID) []uuid.UUID {
	if len(ids) == 0 {
		return nil
	}
	out := slices.Clone(ids)
	slices.SortFunc(out, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	return slices.Compact(out)
}

// CheckNarrowing reports whether next only narrows cur: next is a valid
// grant, its level is not higher, it has the admin flag only if cur has it,
// its scope is a subset of cur's, and it does not end later. A key with the
// admin flag keeps scope all while the flag stays (Validate).
//
// spaceOf gives the space of each project of next's scope; it is consulted
// only when cur's scope is a set of spaces and next's a set of projects. A
// project missing from it is outside cur's scope.
func CheckNarrowing(cur, next Grant, spaceOf map[uuid.UUID]uuid.UUID) error {
	if err := next.Validate(); err != nil {
		return err
	}
	if next.Level.rank() > cur.Level.rank() {
		return fmt.Errorf("%w: level %q is above %q", ErrNotNarrowing, next.Level, cur.Level)
	}
	if next.Admin && !cur.Admin {
		return fmt.Errorf("%w: the admin permission is added", ErrNotNarrowing)
	}
	if !scopeContains(cur.Scope, next.Scope, spaceOf) {
		return fmt.Errorf("%w: the scope is not a subset", ErrNotNarrowing)
	}
	switch {
	case cur.ExpiresAt == nil:
	case next.ExpiresAt == nil:
		return fmt.Errorf("%w: the expiry is removed", ErrNotNarrowing)
	case next.ExpiresAt.After(*cur.ExpiresAt):
		return fmt.Errorf("%w: the expiry is later", ErrNotNarrowing)
	}
	return nil
}

// scopeContains reports whether every object inner reaches is reached by outer.
func scopeContains(outer, inner Scope, spaceOf map[uuid.UUID]uuid.UUID) bool {
	switch outer.Mode {
	case ScopeAll:
		return true
	case ScopeSpaces:
		switch inner.Mode {
		case ScopeSpaces:
			return subset(inner.Spaces, outer.Spaces)
		case ScopeProjects:
			for _, p := range inner.Projects {
				s, ok := spaceOf[p]
				if !ok || !slices.Contains(outer.Spaces, s) {
					return false
				}
			}
			return true
		}
	case ScopeProjects:
		return inner.Mode == ScopeProjects && subset(inner.Projects, outer.Projects)
	}
	return false
}

func subset(inner, outer []uuid.UUID) bool {
	for _, id := range inner {
		if !slices.Contains(outer, id) {
			return false
		}
	}
	return true
}

// TTLPolicy is the instance's lifetime policy for new keys: the lifetime a
// key gets when none is chosen, and the longest one allowed.
type TTLPolicy struct {
	Default time.Duration
	Max     time.Duration
}

// expiry resolves the end of a new key's lifetime at now: requested if set
// (after now and no later than now + Max), otherwise now + Default.
func (p TTLPolicy) expiry(now time.Time, requested *time.Time) (time.Time, error) {
	if requested == nil {
		return now.Add(p.Default), nil
	}
	at := requested.UTC()
	if !at.After(now) {
		return time.Time{}, fmt.Errorf("%w: the expiry is not in the future", ErrInvalidExpiry)
	}
	if at.After(now.Add(p.Max)) {
		return time.Time{}, fmt.Errorf("%w: the expiry is beyond the maximum lifetime", ErrInvalidExpiry)
	}
	return at, nil
}
