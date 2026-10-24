package search

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Prometheus metrics for the search subsystem. Names follow the
// `skein_<area>_<measure>_<unit>` convention.
//
// All metrics are package-level singletons. promauto registers them in the
// default registerer so /metrics picks them up automatically.

var (
	// EmbeddingsInputTokens captures the prompt_eval_count returned by
	// Ollama on every successful embed call. histogram lets us see the
	// distribution (median is around 50 tokens for a typical issue title;
	// the long tail is large issue descriptions hitting MaxEmbedRunes).
	// Buckets cover 1 token → 32k tokens (Ollama context ceiling).
	EmbeddingsInputTokens = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "skein_embeddings_input_tokens",
			Help:    "Tokens reported by Ollama prompt_eval_count per embed call, by entity kind",
			Buckets: prometheus.ExponentialBuckets(8, 2, 14), // 8..65536
		},
		[]string{"entity"},
	)

	// EmbeddingsTruncatedTotal counts how often the embed input had to be
	// trimmed at MaxEmbedRunes. A non-zero rate is an alarm — it means we
	// are silently dropping context from oversized documents.
	EmbeddingsTruncatedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "skein_embeddings_truncated_total",
			Help: "Number of embed inputs truncated at MaxEmbedRunes, by entity kind",
		},
		[]string{"entity"},
	)

	// EmbeddingsPending is sampled by the worker on every tick: it counts
	// rows where embedding IS NULL. Together with a rate(processed) query,
	// the operator can estimate backfill ETA.
	EmbeddingsPending = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "skein_embeddings_pending",
			Help: "Rows waiting for an embedding, by entity kind",
		},
		[]string{"entity"},
	)

	// EmbeddingsProcessedTotal increments per row written to the DB. It is
	// the worker's productivity meter.
	EmbeddingsProcessedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "skein_embeddings_processed_total",
			Help: "Embeddings successfully written to the DB, by entity kind",
		},
		[]string{"entity"},
	)

	// EmbeddingsErrorsTotal is the worker's error meter — Ollama failures,
	// DB failures, transaction aborts. Operators alert on rate > 0 sustained.
	EmbeddingsErrorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "skein_embeddings_errors_total",
			Help: "Embedding worker errors, by entity kind and reason",
		},
		[]string{"entity", "reason"},
	)

	// SearchRequestsTotal counts /search calls. `degraded` separates the
	// hybrid-OK path from the FTS-only fallback (Ollama down). `kind` is
	// `all` when no kind filter, otherwise the comma-joined sorted set
	// (e.g. `issue,project`) so cardinality stays bounded at <= 16.
	SearchRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "skein_search_requests_total",
			Help: "Search requests served by /api/v1/search",
		},
		[]string{"degraded", "kind"},
	)

	// SearchLatencySeconds is the end-to-end /search handler latency. The
	// histogram buckets target a P99 SLO of 300 ms.
	SearchLatencySeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "skein_search_latency_seconds",
			Help:    "End-to-end /search handler latency in seconds",
			Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.2, 0.3, 0.5, 1, 2, 5},
		},
		[]string{"kind"},
	)

	// QueryEmbeddingCacheHitsTotal / Misses bracket the in-process LRU
	// over query→vector. A high hit rate means most user requests reuse a
	// recent embedding (e.g. they're paginating or refining filters); a
	// low one means each query costs an Ollama round-trip.
	QueryEmbeddingCacheHitsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "skein_query_embedding_cache_hits_total",
			Help: "Query-embedding LRU cache hits",
		},
	)

	QueryEmbeddingCacheMissesTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "skein_query_embedding_cache_misses_total",
			Help: "Query-embedding LRU cache misses",
		},
	)
)
