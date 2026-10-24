package search

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"
	"golang.org/x/sync/errgroup"
)

// Service orchestrates a /search request end-to-end:
//
//  1. Validates and normalises params.
//  2. Tries the result-cache by CacheKey.  Cursor pagination piggybacks
//     on that cache: a cached entry holds up to 200 items in stable
//     order; cursor decode produces (cacheKey, offset).
//  3. On miss: runs an ID-prefix fast-path lookup AND fans-out N
//     per-kind RRF queries in parallel via errgroup.
//  4. Merges the per-kind lists by score, dedups against the fast-path
//     hits, and stores the top resultCachePoolLimit items in the cache.
//  5. Slices the right page out of the cached items and returns.
//
// Service is goroutine-safe — it owns no per-request state outside of
// the function frames.
type Service struct {
	store      *Store
	embedder   *Embedder
	cache      *resultCache
	pageMaxLim int
}

// NewService wires the dependencies. cacheTTL is the result-cache TTL.
func NewService(store *Store, embedder *Embedder, cacheTTL time.Duration) (*Service, error) {
	cache, err := newResultCache(cacheTTL)
	if err != nil {
		return nil, err
	}
	return &Service{
		store:      store,
		embedder:   embedder,
		cache:      cache,
		pageMaxLim: MaxPageLimit,
	}, nil
}

// Embedder returns the underlying *Embedder, or nil if the service was
// constructed without one (FTS-only mode). main() consults this when
// deciding whether to launch the backfill worker.
func (s *Service) Embedder() *Embedder { return s.embedder }

// observeSearchLatency records SearchLatencySeconds for the kinds set.
// Used via `defer observeSearchLatency(...)` so the metric path avoids a
// per-Search closure allocation.
func observeSearchLatency(kinds []Kind, start time.Time) {
	SearchLatencySeconds.WithLabelValues(kindLabel(kinds)).
		Observe(time.Since(start).Seconds())
}

// DefaultPageLimit and MaxPageLimit are the page size the search service
// applies when the caller names none, and the ceiling it clamps to.
//
// Named and exported for the INV-5 guard (cmd/api/schema_literals_test.go):
// it compares what skein_search's schema DECLARES to what the code
// APPLIES, and that comparison needs one reachable name per applied value.
const (
	DefaultPageLimit = 50
	MaxPageLimit     = 100
)

// Search is the entry point invoked by the HTTP handler.
func (s *Service) Search(ctx context.Context, p Params) (*Result, error) {
	start := time.Now()

	if p.Limit <= 0 {
		p.Limit = DefaultPageLimit
	}
	if p.Limit > s.pageMaxLim {
		p.Limit = s.pageMaxLim
	}
	if len(p.Kinds) == 0 {
		p.Kinds = AllKinds
	}
	// Defer after kinds normalisation so the label matches the prior
	// closure (which read p.Kinds at return time).
	defer observeSearchLatency(p.Kinds, start)

	// The cached pool is the caller's view when it was filled; the key
	// carries the fingerprint of the view now, so a narrowed (or widened)
	// view misses the cache and reruns the permission-filtered queries.
	// The visible set is the request's own (Scope.Bind memo): the store
	// queries below read the same one. Read BEFORE the cursor: a cursor's
	// entry is checked against it (K-5).
	fp, err := s.store.visibilityFingerprint(ctx, p.CallerID)
	if err != nil {
		return nil, err
	}
	p.visibility = fp

	// Cursor pagination: if the caller supplied one, slice an existing
	// cache entry — K-5 (sk-674.18 T031): only an entry of THIS caller with
	// THIS scope answers; anything else (another user's cursor, a forged
	// key, a cursor that outlived a change of access) is ErrInvalidCursor.
	// On expiry we fall through to a full re-run with the current Params —
	// items materialise in the same order, so the offset stays meaningful,
	// and the re-run is the caller's own view.
	if p.Cursor != "" {
		cacheKey, offset, err := DecodeCursor(p.Cursor)
		if err != nil {
			return nil, err
		}
		// The cursor continues the query it was issued for: its key is that
		// query's key (query, project, kinds, space, filters, caller, scope).
		// Under other parameters — space= changed, say — it is refused
		// rather than answered with the rows of the old query,
		// and never re-run at its offset under the new ones.
		if cacheKey != CacheKey(p) {
			return nil, fmt.Errorf("%w: the cursor belongs to another query", ErrInvalidCursor)
		}
		if entry, ok := s.cache.Get(cacheKey); ok {
			if !entry.ownedBy(p.CallerID, fp) {
				return nil, fmt.Errorf("%w: the cursor is not the caller's", ErrInvalidCursor)
			}
			SearchRequestsTotal.WithLabelValues("false", kindLabel(p.Kinds)).Inc()
			return s.makeResult(ctx, entry.items, cacheKey, offset, p.Limit, false, start), nil
		}
	}

	cacheKey := CacheKey(p)
	if p.Cursor == "" {
		if entry, ok := s.cache.Get(cacheKey); ok && entry.ownedBy(p.CallerID, fp) {
			SearchRequestsTotal.WithLabelValues("false", kindLabel(p.Kinds)).Inc()
			return s.makeResult(ctx, entry.items, cacheKey, 0, p.Limit, false, start), nil
		}
	}

	// space= (sk-674.18 T031): the page filter narrows every kind to one
	// space; an unknown short id matches nothing.
	var spaceFilter *uuid.UUID
	if p.Space != "" {
		sid, serr := s.store.spaceIDByShort(ctx, p.Space)
		if serr != nil {
			return nil, serr
		}
		spaceFilter = &sid
	}

	// Embed the query.  If Ollama is down or the embedding times out we
	// switch to FTS-only — every per-kind store function accepts a nil
	// Vec for that path.
	var qVec []float32
	degraded := false
	if s.embedder != nil {
		v, err := s.embedder.EmbedSingle(ctx, p.Query)
		if err != nil {
			if errors.Is(err, ErrEmbedderUnavailable) {
				slog.WarnContext(ctx, "search degraded: embedder unavailable",
					"queryLen", len(p.Query),
					"error", err.Error())
				degraded = true
			} else {
				return nil, err
			}
		} else {
			qVec = v
		}
	} else {
		degraded = true
	}

	in := SearchInput{
		Query:    p.Query,
		Project:  p.Project,
		Space:    spaceFilter,
		CallerID: p.CallerID,
		Filters:  p.Filters,
		Limit:    candidateLimit, // we'll trim after the merge
	}
	if qVec != nil {
		in.Vec = qVec
		in.Embedding = pgvector.NewHalfVector(qVec)
	}

	// Issue-only filters apply only to the issues query.  Build a
	// separate input so the other kinds run unfiltered.
	issueIn := in

	// Resolve the effective kind set into a lookup map so fast-path
	// filtering and gating below stay O(1).
	allowedKinds := make(map[Kind]bool, len(p.Kinds))
	for _, k := range p.Kinds {
		allowedKinds[k] = true
	}

	g, gctx := errgroup.WithContext(ctx)

	// Each goroutine returns its result through a fixed-size map under
	// a mutex-free pattern: we pre-allocate per-kind slots and have
	// each worker write into "its" slot.
	var (
		issueItems     []Item
		commentItems   []Item
		projectItems   []Item
		componentItems []Item
		fastPathItems  []Item
	)

	for _, k := range p.Kinds {
		switch k {
		case KindIssue:
			g.Go(func() error {
				items, err := s.store.SearchIssues(gctx, issueIn)
				if err != nil {
					return err
				}
				issueItems = items
				return nil
			})
		case KindComment:
			g.Go(func() error {
				items, err := s.store.SearchComments(gctx, in)
				if err != nil {
					return err
				}
				commentItems = items
				return nil
			})
		case KindProject:
			g.Go(func() error {
				items, err := s.store.SearchProjects(gctx, in)
				if err != nil {
					return err
				}
				projectItems = items
				return nil
			})
		case KindComponent:
			g.Go(func() error {
				items, err := s.store.SearchComponents(gctx, in)
				if err != nil {
					return err
				}
				componentItems = items
				return nil
			})
		}
	}

	// ID-prefix fast-path runs in parallel — short DB query that
	// returns ≤10 items or nothing.  LookupIDPrefix only ever returns
	// `issue` or `project` rows, so we skip it entirely when the caller
	// asked for neither (e.g. ?kind=comment,component) — this saves a
	// SQL round-trip and prevents the merge from leaking issues/projects
	// into a kind-filtered result set (regression: see test report
	// 2026-05-01 bug #1).  LookupIDPrefix also self-gates on LooksLikeID
	// via the regex.
	needFastPath := allowedKinds[KindIssue] || allowedKinds[KindProject]
	if needFastPath {
		g.Go(func() error {
			if !LooksLikeID(p.Query) {
				return nil
			}
			items, err := s.store.LookupIDPrefix(gctx, p.Query, p.CallerID, p.Project, spaceFilter)
			if err != nil {
				return err
			}
			fastPathItems = items
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Fast-path filtering: LookupIDPrefix returns both issue and project
	// rows.  If the caller asked for a strict subset (e.g. only project),
	// drop the kinds they didn't request so we never leak them into the
	// merged result.
	if len(fastPathItems) > 0 {
		filtered := fastPathItems[:0]
		for _, it := range fastPathItems {
			if allowedKinds[it.Kind] {
				filtered = append(filtered, it)
			}
		}
		fastPathItems = filtered
	}

	// Merge with dedup.
	merged := make(map[string]Item, len(issueItems)+len(commentItems)+
		len(projectItems)+len(componentItems)+len(fastPathItems))

	collect := func(it Item) {
		key := string(it.Kind) + ":" + it.ID
		if existing, ok := merged[key]; ok {
			if it.ExactMatch && !existing.ExactMatch {
				existing.ExactMatch = true
			}
			// Keep the strongest tier when the same row arrives from both
			// the id fast-path and the RRF path (Req 3): a number/code that
			// also matches the title must not demote the exact-id row down
			// to tierFuzzy. Higher tier wins.
			if it.matchTier > existing.matchTier {
				existing.matchTier = it.matchTier
			}
			existing.Score += it.Score
			merged[key] = existing
			return
		}
		merged[key] = it
	}

	// Fast-path bumps score so an id-match wins ties cleanly within its
	// tier. Cross-tier ordering is decided by matchTier in the sort below,
	// not by this bump.
	for _, it := range fastPathItems {
		it.Score += 0.01
		collect(it)
	}
	for _, it := range issueItems {
		collect(it)
	}
	for _, it := range commentItems {
		collect(it)
	}
	for _, it := range projectItems {
		collect(it)
	}
	for _, it := range componentItems {
		collect(it)
	}

	// Materialise + sort. Three-tier ordering (Req 2): exact id-code
	// (tierCode) leads, then exact id-number (tierNumber), then everything
	// from the RRF hybrid path (tierFuzzy). Within a tier we fall back to
	// the RRF/score axis, then most-recently-updated.
	pool := make([]Item, 0, len(merged))
	for _, it := range merged {
		pool = append(pool, it)
	}
	sort.SliceStable(pool, func(i, j int) bool {
		if pool[i].matchTier != pool[j].matchTier {
			return pool[i].matchTier > pool[j].matchTier
		}
		if pool[i].Score != pool[j].Score {
			return pool[i].Score > pool[j].Score
		}
		return pool[i].UpdatedAt.After(pool[j].UpdatedAt)
	})

	if len(pool) > resultCachePoolLimit {
		pool = pool[:resultCachePoolLimit]
	}

	if err := s.store.fillSpaceIDs(ctx, pool); err != nil {
		return nil, err
	}
	s.cache.Put(cacheKey, pool, p.CallerID, fp)
	SearchRequestsTotal.WithLabelValues(boolStr(degraded), kindLabel(p.Kinds)).Inc()

	return s.makeResult(ctx, pool, cacheKey, 0, p.Limit, degraded, start), nil
}

// makeResult slices `items` from offset for `limit`, computes the
// next cursor, and assembles the wire response.
func (s *Service) makeResult(ctx context.Context, items []Item, cacheKey string, offset, limit int, degraded bool, start time.Time) *Result {
	if offset < 0 {
		offset = 0
	}
	if offset > len(items) {
		offset = len(items)
	}
	end := min(offset+limit, len(items))
	page := items[offset:end]

	hasMore := end < len(items)
	var nextCursor *string
	if hasMore {
		c, err := EncodeCursor(cacheKey, end)
		if err != nil {
			// Should not happen — hasMore implies end > 0 and we always have
			// a non-empty cacheKey at this point. Log and continue without a
			// cursor; the caller can re-issue without pagination.
			slog.WarnContext(ctx, "encode cursor failed",
				"cacheKey", cacheKey, "end", end, "error", err.Error())
		} else {
			nextCursor = &c
		}
	}

	took := time.Since(start).Milliseconds()
	out := make([]Item, len(page))
	copy(out, page)
	return &Result{
		Items:      out,
		NextCursor: nextCursor,
		HasMore:    hasMore,
		Degraded:   degraded,
		TookMs:     took,
	}
}

// kindLabel returns a stable, low-cardinality label for the request kinds.
func kindLabel(kinds []Kind) string {
	if len(kinds) == 0 || len(kinds) == len(AllKinds) {
		return "all"
	}
	ks := make([]string, len(kinds))
	for i, k := range kinds {
		ks[i] = string(k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// LookupIDParams bundles the request inputs for the lookup-id endpoint.
// CallerID is populated by the handler from the request context.
type LookupIDParams struct {
	Query    string     // raw query string — LookupIDPrefix self-gates on shape
	Project  *uuid.UUID // optional scope; nil = all visible projects
	CallerID uuid.UUID
}

// LookupID is the lightweight ID-prefix lookup entry point. It is the
// thin Service wrapper around Store.LookupIDPrefix — designed for the
// typeahead direct-jump UX where the frontend wants <5 rows fast,
// without the cost of running the four-kind hybrid search.
//
// Returns an empty (non-nil) slice when the query does not look like
// an entity id; LookupIDPrefix self-gates on the regex.  No errors are
// returned for the shape gate — silent empty is the contract.
//
// Permission filtering happens in SQL identically to Search: admins
// see everything, non-admins are constrained to project_members rows.
func (s *Service) LookupID(ctx context.Context, p LookupIDParams) ([]Item, error) {
	items, err := s.store.LookupIDPrefix(ctx, p.Query, p.CallerID, p.Project, nil)
	if err != nil {
		return nil, err
	}
	if items == nil {
		return []Item{}, nil
	}
	return items, nil
}

// LookupIDsBatchMax caps the number of ids accepted in one
// lookup-id-batch request. Enforced at the handler boundary (400 over the
// cap); re-stated here so non-HTTP callers share the same contract.
const LookupIDsBatchMax = 100

// LookupIDsBatch resolves N exact issue ids in one query. Thin wrapper
// around Store.LookupIDsBatch: admins see all, non-admins are constrained
// to project_members rows. Only found+visible rows come back; order is not
// guaranteed (the client matches by id). The caller is expected to have
// already deduped + lowercased the ids; this method does NOT re-cap the
// list — that gate lives at the handler boundary.
//
// Returns a non-nil empty slice when ids is empty so the JSON wire shape
// stays `{"items": []}` rather than `{"items": null}`.
func (s *Service) LookupIDsBatch(ctx context.Context, ids []string, callerID uuid.UUID) ([]Item, error) {
	items, err := s.store.LookupIDsBatch(ctx, ids, callerID)
	if err != nil {
		return nil, err
	}
	if items == nil {
		return []Item{}, nil
	}
	return items, nil
}
