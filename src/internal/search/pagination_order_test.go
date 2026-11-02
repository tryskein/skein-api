package search_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"skein-api/internal/search"
	"skein-api/internal/testsupport"
)

// Equal-rank candidates must have a total order before both candidate
// truncation and page slicing. Expected ids come from seeded rows, never
// from a preceding search response.
func TestSearchStore_EqualRankCandidatesHaveStableOrder(t *testing.T) {
	for _, kind := range search.AllKinds {
		t.Run(string(kind), func(t *testing.T) {
			pool := testsupport.NewTestPool(t)
			owner, _ := testsupport.SeedUser(t, pool, "user")
			project, prefix := testsupport.SeedProject(t, pool, owner, "tstie")
			tok := token(t)
			vector := make([]float32, 1024)
			vector[0] = 1
			embedding := pgvector.NewHalfVector(vector)
			stamp := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
			var ids []string
			var table string
			var find func(context.Context, search.SearchInput) ([]search.Item, error)
			store := search.NewStore(pool)
			switch kind {
			case search.KindIssue:
				table, find = "issues", store.SearchIssues
			case search.KindComment:
				table, find = "comments", store.SearchComments
			case search.KindProject:
				table, find = "projects", store.SearchProjects
			case search.KindComponent:
				table, find = "components", store.SearchComponents
			}
			host := testsupport.SeedIssue(t, pool, project, owner, prefix+"-host", "comment host", "open", 2)
			// More than the documented top-50 candidate cap; the first
			// three identities must survive even when inserted last.
			for i := 60; i > 0; i-- {
				var id string
				switch kind {
				case search.KindIssue:
					id = testsupport.SeedIssue(t, pool, project, owner, fmt.Sprintf("%s-%03d", prefix, i), tok, "open", 2)
				case search.KindComment:
					id = testsupport.SeedComment(t, pool, host, owner, tok).String()
				case search.KindProject:
					p, _ := testsupport.SeedProject(t, pool, owner, tok)
					id = p.String()
				case search.KindComponent:
					id = testsupport.SeedComponent(t, pool, project, owner, tok, "").String()
				}
				if _, err := pool.Exec(ctx(), fmt.Sprintf("UPDATE %s SET updated_at = $2, embedding = $3 WHERE id::text = $1", table), id, stamp, embedding); err != nil {
					t.Fatalf("set tied search values: %v", err)
				}
				ids = append(ids, id)
			}
			slices.Sort(ids)
			for _, mode := range []string{"fts", "hybrid"} {
				t.Run(mode, func(t *testing.T) {
					in := search.SearchInput{Query: tok, CallerID: owner, Limit: 3}
					if kind != search.KindProject {
						in.Project = &project
					}
					if mode == "hybrid" {
						in.Vec, in.Embedding = vector, embedding
					}
					items, err := find(ctx(), in)
					if err != nil {
						t.Fatal(err)
					}
					var got []string
					for _, item := range items {
						got = append(got, item.ID)
					}
					if !slices.Equal(got, ids[:3]) {
						t.Errorf("tied candidates = %v, want smallest fixture ids %v", got, ids[:3])
					}
				})
			}
		})
	}
}

func seedEqualRankPageIssues(t *testing.T) (*pgxpool.Pool, search.Params, []string) {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	project, prefix := testsupport.SeedProject(t, pool, owner, "tspord")
	tok := token(t)
	ids := []string{prefix + "-001", prefix + "-002", prefix + "-003", prefix + "-004", prefix + "-005"}
	for i := len(ids) - 1; i >= 0; i-- {
		testsupport.SeedIssue(t, pool, project, owner, ids[i], tok, "open", 2)
	}
	if _, err := pool.Exec(ctx(), `UPDATE issues SET updated_at = '2026-01-01T00:00:00Z' WHERE project_id = $1`, project); err != nil {
		t.Fatal(err)
	}
	return pool, search.Params{Query: tok, Project: &project, Kinds: []search.Kind{search.KindIssue}, Limit: 2, CallerID: owner}, ids
}

func TestSearch_Pagination_EqualRanksAfterCacheMiss(t *testing.T) {
	_, params, ids := seedEqualRankPageIssues(t)
	for page, want := range [][]string{ids[:2], ids[2:4], ids[4:]} {
		res, err := newFTSService(t).Search(ctx(), params)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, item := range res.Items {
			got = append(got, item.ID)
		}
		if !slices.Equal(got, want) {
			t.Errorf("page %d = %v, want %v", page+1, got, want)
		}
		if page < 2 {
			if res.NextCursor == nil {
				t.Fatal("missing continuation")
			}
			params.Cursor = *res.NextCursor
		} else if res.HasMore || res.NextCursor != nil {
			t.Error("last page advertises another page")
		}
	}
}

func TestSearch_Pagination_UpdatedAtBreaksRankTies(t *testing.T) {
	pool, params, ids := seedEqualRankPageIssues(t)
	// The newest issue sorts last by id. Equal relevance must prefer its
	// documented update timestamp before applying the identity tiebreaker.
	if _, err := pool.Exec(ctx(), `UPDATE issues SET updated_at = '2026-01-02T00:00:00Z' WHERE id = $1`, ids[4]); err != nil {
		t.Fatal(err)
	}
	for page, want := range [][]string{{ids[4], ids[0]}, {ids[1], ids[2]}, {ids[3]}} {
		result, err := newFTSService(t).Search(ctx(), params)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, item := range result.Items {
			got = append(got, item.ID)
		}
		if !slices.Equal(got, want) {
			t.Errorf("page %d = %v, want update-time order %v", page+1, got, want)
		}
		if page < 2 {
			if result.NextCursor == nil {
				t.Fatal("missing continuation")
			}
			params.Cursor = *result.NextCursor
		} else if result.HasMore || result.NextCursor != nil {
			t.Error("last page advertises another page")
		}
	}
}

func TestDecodeCursor_RejectsInvalidInput(t *testing.T) {
	for _, tc := range []struct{ name, cursor string }{
		{"base64", "!!not-base64!!"},
		{"json", base64.RawURLEncoding.EncodeToString([]byte("not json"))},
		{"ranking", base64.RawURLEncoding.EncodeToString([]byte(`{"k":"query|ranking=unknown","o":2,"m":"unknown"}`))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, offset, err := search.DecodeCursor(tc.cursor)
			if !errors.Is(err, search.ErrInvalidCursor) {
				t.Fatalf("invalid cursor = %v; want invalid cursor error", err)
			}
			if key != "" || offset != 0 {
				t.Errorf("rejected cursor returned key %q and offset %d", key, offset)
			}
		})
	}
}

func newModeService(t *testing.T, pool *pgxpool.Pool, available bool, calls *atomic.Int32) *search.Service {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !available {
			// A non-retryable boundary failure keeps this fixture free of
			// retry sleeps while exercising the real unavailable mapping.
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		vector := make([]float32, 1024)
		vector[0] = 1
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{vector}}); err != nil {
			t.Errorf("write embedding response: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	embedder, err := search.NewEmbedder(search.EmbedConfig{BaseURL: server.URL, Model: "fixture", Dim: 1024})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := search.NewService(search.NewStore(pool), embedder, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestSearch_Pagination_RankingModeSurvivesCacheMiss(t *testing.T) {
	for _, mode := range []string{"fts", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			pool, params, _ := seedEqualRankPageIssues(t)
			var firstCalls, nextCalls atomic.Int32
			first := newModeService(t, pool, mode == "hybrid", &firstCalls)
			page, err := first.Search(ctx(), params)
			if err != nil || page == nil || page.NextCursor == nil {
				t.Fatalf("first page = %v, %v", page, err)
			}
			params.Cursor = *page.NextCursor
			next := newModeService(t, pool, mode != "hybrid", &nextCalls)
			// Warm this replica with the other ranking mode. A cursor
			// must not answer from that differently ranked cache entry.
			other := params
			other.Cursor = ""
			if _, err := next.Search(ctx(), other); err != nil {
				t.Fatalf("warm other ranking mode: %v", err)
			}
			callsBefore := nextCalls.Load()
			result, err := next.Search(ctx(), params)
			if mode == "hybrid" {
				if !errors.Is(err, search.ErrInvalidCursor) || result != nil {
					t.Fatalf("hybrid cursor during outage = %v, %v; want invalid cursor", result, err)
				}
			} else {
				if err != nil || result == nil || !result.Degraded {
					t.Fatalf("FTS continuation after recovery = %v, %v; want degraded FTS result", result, err)
				}
				if got := nextCalls.Load(); got != callsBefore {
					t.Errorf("FTS continuation called recovered embedder: before %d, after %d", callsBefore, got)
				}
			}
		})
	}
}

func TestSearch_Pagination_LegacyCursorNeedsWarmCache(t *testing.T) {
	pool, params, _ := seedEqualRankPageIssues(t)
	svc := newFTSService(t)
	page, err := svc.Search(ctx(), params)
	if err != nil || page == nil || page.NextCursor == nil {
		t.Fatalf("first page = %v, %v", page, err)
	}
	key, offset, err := search.DecodeCursor(*page.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	params.Cursor, err = search.EncodeCursor(key, offset)
	if err != nil {
		t.Fatal(err)
	}
	warm, err := svc.Search(ctx(), params)
	if err != nil || warm == nil || len(warm.Items) != 2 || !warm.Degraded {
		t.Fatalf("legacy cursor on warm pool = %v, %v", warm, err)
	}
	fresh, err := search.NewService(search.NewStore(pool), nil, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fresh.Search(ctx(), params)
	if !errors.Is(err, search.ErrInvalidCursor) || result != nil {
		t.Errorf("legacy cursor without ranking evidence = %v, %v; want invalid cursor", result, err)
	}
}

func TestSearch_Pagination_RejectsAlteredRankingMetadata(t *testing.T) {
	_, params, _ := seedEqualRankPageIssues(t)
	svc := newFTSService(t)
	first, err := svc.Search(ctx(), params)
	if err != nil || first == nil || first.NextCursor == nil {
		t.Fatalf("first page = %v, %v", first, err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(*first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"hybrid", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			var fields map[string]any
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			fields["m"] = mode
			altered, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			p := params
			p.Cursor = base64.RawURLEncoding.EncodeToString(altered)
			result, err := svc.Search(ctx(), p)
			if !errors.Is(err, search.ErrInvalidCursor) || result != nil {
				t.Errorf("altered ranking metadata = %v, %v; want invalid cursor", result, err)
			}
		})
	}
}

func TestSearch_Pagination_EqualRanksAcrossKinds(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	project, prefix := testsupport.SeedProject(t, pool, owner, "tsmixord")
	issue := testsupport.SeedIssue(t, pool, project, owner, prefix+"-1", "vector document", "open", 2)
	comment := testsupport.SeedComment(t, pool, issue, owner, "vector note")
	component := testsupport.SeedComponent(t, pool, project, owner, "vector section", "")
	vector := make([]float32, 1024)
	vector[0] = 1
	stamp := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	for _, row := range []struct{ table, id string }{
		{"projects", project.String()}, {"issues", issue},
		{"comments", comment.String()}, {"components", component.String()},
	} {
		if _, err := pool.Exec(ctx(), fmt.Sprintf("UPDATE %s SET updated_at = $2, embedding = $3 WHERE id::text = $1", row.table), row.id, stamp, pgvector.NewHalfVector(vector)); err != nil {
			t.Fatal(err)
		}
	}
	// The query occurs in none of the rows. One equal vector candidate
	// per kind gives every result the same RRF score and updated_at.
	params := search.Params{Query: token(t), Project: &project, Limit: 2, CallerID: owner}
	wantPages := [][]string{{comment.String(), component.String()}, {issue, project.String()}}
	for page, want := range wantPages {
		var calls atomic.Int32
		result, err := newModeService(t, pool, true, &calls).Search(ctx(), params)
		if err != nil || result == nil {
			t.Fatalf("page %d: %v", page+1, err)
		}
		var got []string
		for _, item := range result.Items {
			got = append(got, item.ID)
			if !item.UpdatedAt.Equal(stamp) {
				t.Fatalf("fixture timestamp differs: %v", item.UpdatedAt)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("page %d identities = %v, want %v", page+1, got, want)
		}
		if page == 0 {
			if result.NextCursor == nil {
				t.Fatal("missing mixed-kind continuation")
			}
			params.Cursor = *result.NextCursor
		} else if result.HasMore || result.NextCursor != nil {
			t.Error("last mixed-kind page advertises another page")
		}
	}
}
