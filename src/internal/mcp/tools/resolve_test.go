package tools

import (
	"context"
	"errors"
	"testing"

	"skein-api/internal/projects"
)

func TestResolveProjectHandle_UUIDPath(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "rh")

	pStore := projects.NewStore(pool)
	got, err := ResolveProjectHandle(context.Background(), pool, pStore, pid.String(), userID)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != pid {
		t.Fatalf("got %v, want %v", got, pid)
	}
}

func TestResolveProjectHandle_InvisibleUUID(t *testing.T) {
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "user")
	stranger, _ := seedUser(t, pool, "user")
	pid, _ := seedProject(t, pool, owner, "rhinv")

	pStore := projects.NewStore(pool)
	_, err := ResolveProjectHandle(context.Background(), pool, pStore, pid.String(), stranger)
	var nf *ProjectNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("want *ProjectNotFoundError, got %T (%v)", err, err)
	}
}

func TestResolveProjectHandle_NameNotFound(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pStore := projects.NewStore(pool)
	_, err := ResolveProjectHandle(context.Background(), pool, pStore, "no-such-handle-xyz", userID)
	var nf *ProjectNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("want *ProjectNotFoundError, got %T (%v)", err, err)
	}
}

// TestResolveProjectHandle_EmptyHandle asserts that an empty or
// whitespace-only handle returns *ValidationProjectError so callers can
// distinguish "you sent garbage" from "the project doesn't exist".
func TestResolveProjectHandle_EmptyHandle(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pStore := projects.NewStore(pool)

	for _, handle := range []string{"", "   ", "\t\n"} {
		_, err := ResolveProjectHandle(context.Background(), pool, pStore, handle, userID)
		var val *ValidationProjectError
		if !errors.As(err, &val) {
			t.Errorf("handle=%q: want *ValidationProjectError, got %T (%v)", handle, err, err)
			continue
		}
		if val.Field != "project" {
			t.Errorf("handle=%q: ValidationProjectError.Field = %q, want %q", handle, val.Field, "project")
		}
	}
}
