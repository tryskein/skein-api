package issues

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

// cursorPayload is the keyset payload for the issues list. T is whichever
// timestamp the active sort uses (updated_at by default, created_at when
// the caller passed ?sort=created), I is the row's id (issues.id is text
// rather than uuid — store as string).
type cursorPayload struct {
	T time.Time `json:"t"`
	I string    `json:"i"`
}

// EncodeCursor base64url-encodes the keyset cursor for the next page.
func EncodeCursor(t time.Time, id string) string {
	p := cursorPayload{T: t.UTC(), I: id}
	b, _ := json.Marshal(p)
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCursor parses the cursor previously emitted by EncodeCursor.
// Returns a typed error so handlers translate to HTTP 400 rather than 500.
func DecodeCursor(s string) (time.Time, string, error) {
	if s == "" {
		return time.Time{}, "", errors.New("empty cursor")
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, "", errors.New("malformed cursor (not base64)")
	}
	var p cursorPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return time.Time{}, "", errors.New("malformed cursor (not JSON)")
	}
	if p.I == "" {
		return time.Time{}, "", errors.New("malformed cursor (missing id)")
	}
	return p.T.UTC(), p.I, nil
}
