package dependencies

import "context"

// QueryEmbedder is the minimal embedder interface for layer-2 duplicate
// detection (R4.1). Implementations must return a 1024-dim halfvec
// compatible vector for the given text. Returns ErrEmbedderUnavailable
// (wrapped) when the upstream service is down — caller falls back to
// trigram-only and never blocks the create flow on it.
//
// The dependencies package owns this interface (not search) so the dep
// graph stays one-way: dependencies has no import on search. The concrete
// *search.Embedder satisfies the interface structurally.
//
// Available reports whether a meaningful embed call is worth attempting.
// Implementations should return false fast (e.g. last-known-bad health,
// missing config) so CheckDuplicates can short-circuit without paying a
// network round-trip on every create.
type QueryEmbedder interface {
	EmbedSingle(ctx context.Context, text string) ([]float32, error)
	Available() bool
}
