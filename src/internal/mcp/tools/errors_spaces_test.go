package tools

// errors_spaces_test.go — sk-674.18 T018/T024, matrix row 50 (K-7, SC-012),
// the MCP half. The row's owner and the REST half is
// internal/handler/errors_spaces_test.go (the mappers of each channel
// are reached from its own package); both assert
// the same literal of the contract.
//
// Reference: plan K-7 — a membership operation that lost to a concurrent one
// after its bounded retries is a business refusal with the machine code
// `concurrent_change`, the same in REST and MCP. The error is the public
// primitive's (spaces.WithRetry exhausted over a failing transaction).

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/spaces"
)

// failingTx / failingBeginner: every attempt fails before the database is
// reached; rollback succeeds.
type failingTx struct{ pgx.Tx }

func (failingTx) Rollback(context.Context) error { return nil }
func (failingTx) Commit(context.Context) error   { return nil }

type failingBeginner struct{ attempts int }

func (b *failingBeginner) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	b.attempts++
	return failingTx{}, nil
}

func TestConcurrentChange_MCPCarriesTheCode(t *testing.T) {
	ctx := context.Background()
	b := &failingBeginner{}
	err := spaces.WithRetry(ctx, b, func(pgx.Tx) error { return &pgconn.PgError{Code: "40001"} })
	if b.attempts < 2 || !errors.Is(err, spaces.ErrConcurrentChange) {
		t.Fatalf("the primitive after %d attempts = %v, want the exhausted retries", b.attempts, err)
	}
	channels := []struct {
		name string
		res  *mcpgo.CallToolResult
	}{
		{"skein_project_create", mapProjectCreateError(ctx, nil, err, uuid.New(), "name", "prefix")},
		{"skein_project_update", mapProjectUpdateError(ctx, err, uuid.New(), uuid.New())},
		{"skein_user", mapUsersError(ctx, err, "create", uuid.NewString())},
	}
	for _, c := range channels {
		t.Run(c.name, func(t *testing.T) {
			if c.res == nil || !c.res.IsError {
				t.Fatalf("not an error result: %+v", c.res)
			}
			if got := extractToolJSON(t, c.res)["error"]; got != "concurrent_change" {
				t.Errorf("error = %v, want concurrent_change (K-7)", got)
			}
		})
	}
}
