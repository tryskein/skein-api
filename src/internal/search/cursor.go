package search

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrInvalidCursor is returned by DecodeCursor and EncodeCursor for any
// shape/structural problem with the cursor (empty input, bad base64, bad
// json, or a payload that fails internal validation). Callers branch on
// errors.Is(err, ErrInvalidCursor) to map to 400 invalid_cursor without
// matching on string content.
var ErrInvalidCursor = errors.New("invalid cursor")

type rankingMode string

const (
	rankingLegacy rankingMode = ""
	rankingFTS    rankingMode = "fts"
	rankingHybrid rankingMode = "hybrid"
)

// cursorPayload binds an offset to one query and ranking mode. The
// optional mode is additive: the exported legacy codec still reads and
// writes {k,o}. Legacy cursors can continue an owned warm pool, but cannot
// rebuild an absent pool because their ranking mode is unknown.
type cursorPayload struct {
	K string      `json:"k"` // mode-scoped cache key (SHA-256 hex)
	O int         `json:"o"` // offset into the cached items slice
	M rankingMode `json:"m,omitempty"`
}

// EncodeCursor returns a URL-safe base64 representation of (cacheKey, offset).
// We use the no-padding encoding so the cursor can sit verbatim in a query
// string without `=` escaping noise.
//
// Returns ErrInvalidCursor if cacheKey is empty or offset is non-positive.
// The service should never construct a cursor in those conditions; surfacing
// the error makes a future programmer mistake loud rather than silent.
func EncodeCursor(cacheKey string, offset int) (string, error) {
	return encodeSearchCursor(cacheKey, offset, rankingLegacy)
}

func encodeSearchCursor(cacheKey string, offset int, mode rankingMode) (string, error) {
	if cacheKey == "" {
		return "", fmt.Errorf("%w: empty cacheKey", ErrInvalidCursor)
	}
	if offset <= 0 {
		return "", fmt.Errorf("%w: offset must be positive, got %d", ErrInvalidCursor, offset)
	}
	b, err := json.Marshal(cursorPayload{K: cacheKey, O: offset, M: mode})
	if err != nil {
		return "", fmt.Errorf("%w: marshal: %w", ErrInvalidCursor, err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// DecodeCursor reverses EncodeCursor. Returns an error wrapped with
// ErrInvalidCursor on any decode failure so the handler can surface 400
// invalid_cursor via errors.Is.
func DecodeCursor(raw string) (cacheKey string, offset int, err error) {
	p, err := decodeSearchCursor(raw)
	if err != nil {
		return "", 0, err
	}
	return p.K, p.O, nil
}

func decodeSearchCursor(raw string) (cursorPayload, error) {
	if raw == "" {
		return cursorPayload{}, fmt.Errorf("%w: empty", ErrInvalidCursor)
	}
	b, derr := base64.RawURLEncoding.DecodeString(raw)
	if derr != nil {
		return cursorPayload{}, fmt.Errorf("%w: invalid base64: %w", ErrInvalidCursor, derr)
	}
	var p cursorPayload
	if jerr := json.Unmarshal(b, &p); jerr != nil {
		return cursorPayload{}, fmt.Errorf("%w: invalid json: %w", ErrInvalidCursor, jerr)
	}
	if p.K == "" || p.O <= 0 {
		return cursorPayload{}, fmt.Errorf("%w: missing fields", ErrInvalidCursor)
	}
	if p.M != rankingLegacy && p.M != rankingFTS && p.M != rankingHybrid {
		return cursorPayload{}, fmt.Errorf("%w: unknown ranking mode", ErrInvalidCursor)
	}
	return p, nil
}
