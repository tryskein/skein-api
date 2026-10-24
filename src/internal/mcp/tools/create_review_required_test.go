package tools

// create_review_required_test.go — sk-303.14 T009, matrix rows 40 and 41 on
// the AGENT create path.
//
// The store level is already proven in internal/issues; here the wire is
// checked: that the `reviewRequired` argument actually reaches the store
// input, and that its ABSENCE is not substituted with a default at parse.
//
// The second is not a detail and is the file's main mutation:
// req.GetBool("reviewRequired", true) looks innocent and breaks the
// requirement wholesale. That parse turns every silent call into an
// EXPLICIT pass of the built-in default, and the author's personal default
// never applies — while "explicit pass works" stays green and "by default
// the flag is on" does too. The one case that tells those implementations
// apart is a silent call with a personal default DIFFERENT from the factory one.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/review"
)

func setPersonalReviewDefaultTool(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, value bool) {
	t.Helper()
	if value == review.DefaultReviewRequired {
		t.Fatalf("the probe sets a personal default %v equal to the factory one — it would not tell "+
			"\"author default was read\" from \"parse stuffed in the built-in default\"", value)
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO user_notification_prefs (user_id, review_required_default, updated_at)
VALUES ($1, $2, now())
ON CONFLICT (user_id) DO UPDATE SET review_required_default = EXCLUDED.review_required_default`,
		userID, value); err != nil {
		t.Fatalf("set personal review default: %v", err)
	}
}

func readCreatedReviewRequired(t *testing.T, pool *pgxpool.Pool, issueID string) bool {
	t.Helper()
	var required bool
	if err := pool.QueryRow(context.Background(),
		`SELECT review_required FROM issues WHERE id = $1`, issueID).Scan(&required); err != nil {
		t.Fatalf("read review_required: %v", err)
	}
	return required
}

// createViaTool calls skein_create with the given arguments and returns
// the created issue's identifier.
func createViaTool(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, pid uuid.UUID, extra map[string]any) string {
	t.Helper()
	handler := handleCreate(pool, issues.NewStore(pool), dependencies.NewStore(pool), projects.NewStore(pool))
	ctx := withCtx(t, userID, "admin")

	args := map[string]any{
		"project": pid.String(),
		"title":   "agent-path issue",
		"type":    "task",
		"kind":    "directed",
	}
	for k, v := range extra {
		args[k] = v
	}
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = args

	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", res)
	}
	body := extractToolJSON(t, res)
	issueObj, ok := body["issue"].(map[string]any)
	if !ok {
		t.Fatalf("issue not present: %T", body["issue"])
	}
	id, _ := issueObj["id"].(string)
	if id == "" {
		t.Fatalf("issue.id missing")
	}
	return id
}

// TestCreateTool_ReviewRequired_OmittedUsesPersonalDefault — a silent call
// applies the author's PERSONAL default, not the built-in default.
func TestCreateTool_ReviewRequired_OmittedUsesPersonalDefault(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "crr")

	setPersonalReviewDefaultTool(t, pool, userID, false)

	id := createViaTool(t, pool, userID, pid, nil)
	if got := readCreatedReviewRequired(t, pool, id); got != false {
		t.Errorf("review_required = %v, want the author's personal default (false). "+
			"Argument parsing substitutes a default instead of telling "+
			"\"did not pass\" from \"passed\" — then the personal default never applies "+
			"(US-3)", got)
	}
}

// TestCreateTool_ReviewRequired_ExplicitTravels — an explicit pass reaches
// the store, in both directions.
func TestCreateTool_ReviewRequired_ExplicitTravels(t *testing.T) {
	for _, tc := range []struct {
		name     string
		personal *bool
		explicit bool
	}{
		{name: "explicit false with factory true", explicit: false},
		{name: "explicit true with personal default false", personal: boolPtr(false), explicit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestPool(t)
			userID, _ := seedUser(t, pool, "admin")
			pid, _ := seedProject(t, pool, userID, "crr")

			if tc.personal != nil {
				setPersonalReviewDefaultTool(t, pool, userID, *tc.personal)
			}

			id := createViaTool(t, pool, userID, pid, map[string]any{"reviewRequired": tc.explicit})
			if got := readCreatedReviewRequired(t, pool, id); got != tc.explicit {
				t.Errorf("review_required = %v, want the explicitly passed %v — the argument "+
					"does not reach the store input", got, tc.explicit)
			}
		})
	}
}

// TestCreateTool_ReviewRequired_NonBooleanRejected — a non-boolean argument
// is refused with a form code, not coerced silently.
func TestCreateTool_ReviewRequired_NonBooleanRejected(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "crr")

	handler := handleCreate(pool, issues.NewStore(pool), dependencies.NewStore(pool), projects.NewStore(pool))
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":        pid.String(),
		"title":          "non-boolean flag",
		"type":           "task",
		"kind":           "directed",
		"reviewRequired": "yes",
	}
	res, err := handler(withCtx(t, userID, "admin"), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("a string flag value was accepted — it would have been coerced to \"not "+
			"required\" silently: %+v", res)
	}
}
