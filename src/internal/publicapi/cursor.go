package publicapi

// cursor.go — the keyset list envelope of the public API: items,
// nextCursor, hasMore; limit outside its range is refused, never clamped.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
)

// List is the envelope of a list response. nextCursor is null on the last
// page. fields= projects Items; the envelope is always sent.
type List[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"nextCursor" extensions:"x-nullable"`
	HasMore    bool    `json:"hasMore"`
}

func (List[T]) itemType() reflect.Type { return reflect.TypeFor[T]() }

// Limit is the range of limit= of one list operation.
type Limit struct {
	Min, Max, Default int
}

// ErrBadLimit refuses limit= outside its range or not a number.
var ErrBadLimit = errors.New("limit out of range")

// ParseLimit reads limit= of r: absent = l.Default; not an integer or
// outside [l.Min, l.Max] = ErrBadLimit (400; the value is never clamped).
func ParseLimit(r *http.Request, l Limit) (int, error) {
	vals, present := r.URL.Query()["limit"]
	if !present {
		return l.Default, nil
	}
	if len(vals) != 1 {
		return 0, fmt.Errorf("%w: limit must be given once", ErrBadLimit)
	}
	n, err := strconv.Atoi(vals[0])
	if err != nil || n < l.Min || n > l.Max {
		return 0, fmt.Errorf("%w: limit must be an integer in [%d, %d]", ErrBadLimit, l.Min, l.Max)
	}
	return n, nil
}

// ErrBadCursor refuses a cursor= that this API did not issue.
var ErrBadCursor = errors.New("cursor is not a cursor of this list")

// EncodeCursor makes the opaque cursor of a keyset position.
func EncodeCursor(position any) (string, error) {
	raw, err := json.Marshal(position)
	if err != nil {
		return "", fmt.Errorf("encoding a cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// DecodeCursor reads an opaque cursor into position (strictly: an unknown
// member is refused). A malformed one is ErrBadCursor (400).
func DecodeCursor(cursor string, position any) error {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return ErrBadCursor
	}
	if err := decodeStrict(raw, position); err != nil {
		return ErrBadCursor
	}
	return nil
}
