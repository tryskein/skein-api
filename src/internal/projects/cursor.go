package projects

import (
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
)

// ListCursor is the keyset cursor of the projects list since sk-674.18
// T024 (K-20): rows are ordered by the space — its kind rank (personal 0,
// common 1, regular 2), lower(name), short id — and inside a space by
// lower(project name) and id, or, with a search query, by relevance (score
// descending) and id descending. The cursor carries the last row's key.
type ListCursor struct {
	Rank      int    `json:"r"`
	SpaceName string `json:"sn"`
	// SpaceID is the space's short id: the cursor goes out to clients, and
	// a space goes out by its short id only, never its UUID (the
	// identifiers of spaces/contract.go).
	SpaceID string `json:"sp"`
	// Name is lower(project name) — the alphabetic list.
	Name string `json:"n,omitempty"`
	// Score is the relevance — the search list.
	Score float64   `json:"s,omitempty"`
	ID    uuid.UUID `json:"i"`
	// V is the cursor version: a cursor of the former (created_at, id)
	// order is refused, not misread.
	V int `json:"v"`
}

const listCursorVersion = 2

// EncodeListCursor encodes c as an opaque URL-safe string.
func EncodeListCursor(c ListCursor) string {
	c.V = listCursorVersion
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeListCursor parses a cursor produced by EncodeListCursor; any other
// input — including a cursor of the former order — is an error (400).
func DecodeListCursor(s string) (ListCursor, error) {
	var c ListCursor
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return c, errors.New("malformed cursor (not base64)")
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, errors.New("malformed cursor (not JSON)")
	}
	if c.V != listCursorVersion || c.ID == uuid.Nil || c.SpaceID == "" {
		return c, errors.New("malformed cursor (not a project list cursor)")
	}
	return c, nil
}
