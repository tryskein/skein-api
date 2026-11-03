package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
)

func TestBulkError_PreservesRetryableAbortAndGenuineConflict(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code string
	}{
		{"retry", fmt.Errorf("rolled back: %w", issues.ErrSerializationRetry), "serialization_retry"},
		{"genuine mismatch", &issues.ErrVersionConflict{CurrentVersion: 2}, "version_conflict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mapBulkError("demo-1", tc.err)
			if got.ID != "demo-1" || got.Code != tc.code {
				t.Fatalf("bulk failure = %+v, want id demo-1 and code %s", got, tc.code)
			}
		})
	}
}

func TestAcceptREST_ExhaustedSerializationIsRetryable(t *testing.T) {
	pool := newAcceptTestPool(t)
	owner, username := seedAcceptUser(t, pool, "user")
	pid := seedAcceptProject(t, pool, owner, "retry")
	var prefix string
	if err := pool.QueryRow(t.Context(), "SELECT id_prefix FROM projects WHERE id=$1", pid).Scan(&prefix); err != nil {
		t.Fatal(err)
	}
	id := seedAcceptIssue(t, pool, pid, owner, prefix, "1", username)
	installAcceptanceAbort(t, pool, id)
	h := NewIssuesHandler(pool, projects.NewStore(pool), dependencies.NewStore(pool))
	response := callAccept(t, h, owner, "user", id, `{"expectedVersion":1}`)
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 for a retryable transaction abort", response.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "serialization_retry" {
		t.Fatalf("error = %v, want serialization_retry", body["error"])
	}
	if _, exists := body["currentVersion"]; exists {
		t.Fatal("a rolled-back accept must not invent a conflicting version")
	}
	var unchanged bool
	if err := pool.QueryRow(t.Context(), "SELECT version=1 AND accepted_at IS NULL FROM issues WHERE id=$1", id).Scan(&unchanged); err != nil {
		t.Fatal(err)
	}
	if !unchanged {
		t.Fatal("the failed acceptance changed the issue")
	}
}

// Inject a real transaction failure at the database boundary, scoped to one
// fixture issue. The public handler, store retries and HTTP response run for
// real. Auxiliary trigger/function objects are removed at test cleanup.
func installAcceptanceAbort(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	schema := "accept_abort_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	trigger := pgx.Identifier{schema}.Sanitize()
	if _, err := pool.Exec(t.Context(), "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := pool.Exec(ctx, "DROP TRIGGER IF EXISTS "+trigger+" ON issues; DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("remove acceptance fault injection: %v", err)
		}
	})
	function := `CREATE FUNCTION ` + quoted + `.abort_accept() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id = TG_ARGV[0] THEN
        RAISE EXCEPTION 'test transaction abort' USING ERRCODE = '40001';
    END IF;
    RETURN NEW;
END $$`
	if _, err := pool.Exec(t.Context(), function); err != nil {
		t.Fatal(err)
	}
	literalID := "'" + strings.ReplaceAll(id, "'", "''") + "'"
	if _, err := pool.Exec(t.Context(), "CREATE TRIGGER "+trigger+" BEFORE UPDATE OF accepted_at ON issues FOR EACH ROW EXECUTE FUNCTION "+quoted+".abort_accept("+literalID+")"); err != nil {
		t.Fatal(err)
	}
}
