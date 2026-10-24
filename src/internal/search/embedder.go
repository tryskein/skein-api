package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/go-retryablehttp"
	lru "github.com/hashicorp/golang-lru/v2"
)

// ErrEmbedderUnavailable is returned by EmbedSingle/EmbedBatch when the
// upstream Ollama server can't be reached or times out. The search service
// uses errors.Is to switch into the FTS-only fallback path.
var ErrEmbedderUnavailable = errors.New("embedder unavailable")

// Embedder is a small Ollama /api/embed client with retry and a process-
// local LRU cache over (query → vector). The cache covers exactly one
// hot path — the search query embedding — because that's the only place
// the same input is likely to repeat within minutes (a user paginating,
// or two users searching for the same recent term).  Worker batches are
// never cached: each row is unique by construction.
type Embedder struct {
	baseURL string
	model   string
	apiKey  string // optional Bearer token; "" → no Authorization header
	dim     int

	httpClient    *http.Client
	embedTimeout  time.Duration // per-request hard ceiling
	searchTimeout time.Duration // override for the search hot-path

	queryCache    *lru.Cache[string, queryCacheEntry]
	queryCacheTTL time.Duration
}

type queryCacheEntry struct {
	vec       []float32
	expiresAt time.Time
}

// EmbedConfig wires Embedder construction. Zero values are filled in with
// sensible defaults; only baseURL and model are required.
type EmbedConfig struct {
	BaseURL string
	Model   string
	// APIKey is the optional Bearer token sent in the Authorization
	// header on every /api/embed call. A production deployment normally
	// puts an authenticating reverse proxy in front of the model server,
	// which rejects requests without a valid `Authorization: Bearer
	// <key>`. Empty string disables the header (dev docker-compose
	// ollama is unauthenticated).
	APIKey        string
	Dim           int
	CacheSize     int           // default 512
	CacheTTL      time.Duration // default 5m
	EmbedTimeout  time.Duration // default 60s (worker)
	SearchTimeout time.Duration // default 5s  (query)
}

// NewEmbedder returns a configured Embedder. retryablehttp handles 5xx and
// network errors with exponential backoff; the per-request ctx still wins
// (cancel propagates through StandardClient).
func NewEmbedder(cfg EmbedConfig) (*Embedder, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("embedder: BaseURL is required")
	}
	if cfg.Model == "" {
		return nil, errors.New("embedder: Model is required")
	}
	if cfg.Dim <= 0 {
		cfg.Dim = 1024
	}
	if cfg.CacheSize <= 0 {
		cfg.CacheSize = 512
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 5 * time.Minute
	}
	if cfg.EmbedTimeout <= 0 {
		cfg.EmbedTimeout = 60 * time.Second
	}
	if cfg.SearchTimeout <= 0 {
		cfg.SearchTimeout = 5 * time.Second
	}

	rc := retryablehttp.NewClient()
	rc.RetryMax = 3
	rc.RetryWaitMin = 500 * time.Millisecond
	rc.RetryWaitMax = 5 * time.Second
	rc.HTTPClient.Timeout = cfg.EmbedTimeout
	rc.Logger = nil // we log at the call site instead — quieter scopes

	cache, err := lru.New[string, queryCacheEntry](cfg.CacheSize)
	if err != nil {
		return nil, fmt.Errorf("query cache: %w", err)
	}

	return &Embedder{
		baseURL:       strings.TrimRight(cfg.BaseURL, "/"),
		model:         cfg.Model,
		apiKey:        strings.TrimSpace(cfg.APIKey),
		dim:           cfg.Dim,
		httpClient:    rc.StandardClient(),
		embedTimeout:  cfg.EmbedTimeout,
		searchTimeout: cfg.SearchTimeout,
		queryCache:    cache,
		queryCacheTTL: cfg.CacheTTL,
	}, nil
}

// Dim returns the configured embedding dimension.
func (e *Embedder) Dim() int { return e.dim }

// Available reports whether the embedder is configured and worth calling.
// A nil receiver returns false so callers can use the same null-check
// idiom everywhere (`emb != nil && emb.Available()`); a non-nil receiver
// always returns true because we've already validated baseURL+model in
// NewEmbedder. The actual upstream health is observed at call time —
// EmbedSingle returns ErrEmbedderUnavailable on failure, which callers
// branch on for graceful degradation. We deliberately don't keep a
// "last known bad" cache: the LRU + retryablehttp + per-call timeout
// already absorb transient blips, and a sticky-bad flag would
// false-negative after Ollama recovers.
func (e *Embedder) Available() bool {
	if e == nil {
		return false
	}
	return e.baseURL != "" && e.model != ""
}

// SearchTimeout returns the timeout the search service should use for
// query embeddings. Exposed so the service can wrap the call ctx.
func (e *Embedder) SearchTimeout() time.Duration { return e.searchTimeout }

// embedRequest is the wire shape of POST /api/embed.
type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// embedResponse is what Ollama returns. prompt_eval_count is reported per
// the *aggregate* batch (not per-input); we observe it once per call.
type embedResponse struct {
	Embeddings      [][]float32 `json:"embeddings"`
	PromptEvalCount int         `json:"prompt_eval_count"`
}

// Embed embeds N inputs in one Ollama round-trip. Returns ErrEmbedderUnavailable
// (wrapped) on network/transport errors so callers can branch on it.
//
// The metricEntity argument is recorded against the input-tokens histogram —
// pass "issue", "project", "comment", "component" from the worker; pass
// "query" from the search hot-path.
func (e *Embedder) Embed(ctx context.Context, inputs []string, metricEntity string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}

	body, err := json.Marshal(embedRequest{Model: e.model, Input: inputs})
	if err != nil {
		return nil, fmt.Errorf("marshal embed request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		e.baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		// Wrap so caller can errors.Is(err, ErrEmbedderUnavailable).
		return nil, fmt.Errorf("%w: %v", ErrEmbedderUnavailable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		// Drain the rest so the connection is returned to the keep-alive
		// pool instead of being closed (saves a TCP handshake per error).
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("%w: ollama returned %d: %s",
			ErrEmbedderUnavailable, resp.StatusCode, string(respBody))
	}

	var result embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode embed response: %w", err)
	}
	if len(result.Embeddings) != len(inputs) {
		return nil, fmt.Errorf("embed: expected %d vectors, got %d",
			len(inputs), len(result.Embeddings))
	}

	// Record the input-token histogram exactly once per call. Ollama
	// reports prompt_eval_count for the whole batch; if that's zero
	// (model didn't populate it) we skip the observation rather than
	// pollute the metric with a synthetic 0. Histogram.Observe is
	// goroutine-safe internally (atomic counters), no extra mutex needed.
	if result.PromptEvalCount > 0 {
		EmbeddingsInputTokens.WithLabelValues(metricEntity).
			Observe(float64(result.PromptEvalCount))
	}

	// Defensively check vector dimension. Ollama always returns 1024 for
	// qwen3-embedding:0.6b. Trim a too-long vector (operator swapped to a
	// higher-dim model without updating Dim) to protect the halfvec(1024)
	// column. Reject too-short vectors loudly — Postgres would reject the
	// halfvec write anyway, but a clear error here surfaces the dim
	// mismatch earlier and with better context than a tx error.
	if e.dim > 0 {
		for i, v := range result.Embeddings {
			if len(v) > e.dim {
				result.Embeddings[i] = v[:e.dim]
			} else if len(v) < e.dim {
				return nil, fmt.Errorf("embed: dim mismatch: expected %d, got %d", e.dim, len(v))
			}
		}
	}
	return result.Embeddings, nil
}

// EmbedSingle is the convenience wrapper for the search hot-path. It
// applies a tighter ctx timeout (searchTimeout) and consults the query
// cache before hitting Ollama.
//
// The cache key is the lowercased+trimmed query string. A 5-min TTL means
// even rapid-fire requests for the same term hit Ollama at most once per
// window, while a stale cache entry is harmless: an old vector still ranks
// items reasonably.
func (e *Embedder) EmbedSingle(ctx context.Context, query string) ([]float32, error) {
	key := strings.ToLower(strings.TrimSpace(query))
	if key == "" {
		return nil, errors.New("embed: empty query")
	}

	if cached, ok := e.queryCache.Get(key); ok {
		if time.Now().Before(cached.expiresAt) {
			QueryEmbeddingCacheHitsTotal.Inc()
			return cached.vec, nil
		}
		// Expired — fall through to refresh.
		e.queryCache.Remove(key)
	}
	QueryEmbeddingCacheMissesTotal.Inc()

	embedCtx, cancel := context.WithTimeout(ctx, e.searchTimeout)
	defer cancel()

	vecs, err := e.Embed(embedCtx, []string{query}, "query")
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 {
		return nil, errors.New("embed: empty result for single input")
	}

	e.queryCache.Add(key, queryCacheEntry{
		vec:       vecs[0],
		expiresAt: time.Now().Add(e.queryCacheTTL),
	})
	return vecs[0], nil
}
