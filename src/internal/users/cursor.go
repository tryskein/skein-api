package users

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// cursorPayload — keyset on (created_at DESC, id DESC) so concurrent
// inserts don't shift pages a client has already seen.
type cursorPayload struct {
	C time.Time `json:"c"`
	I string    `json:"i"`
}

func encodeCursor(t time.Time, id uuid.UUID) string {
	payload := cursorPayload{C: t.UTC(), I: id.String()}
	b, _ := json.Marshal(payload)
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCursor is exposed for the REST handler. Mirrors projects.DecodeCursor.
func DecodeCursor(s string) (time.Time, uuid.UUID, error) {
	if s == "" {
		return time.Time{}, uuid.Nil, errors.New("empty cursor")
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, uuid.Nil, errors.New("malformed cursor (not base64)")
	}
	var p cursorPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return time.Time{}, uuid.Nil, errors.New("malformed cursor (not JSON)")
	}
	id, err := uuid.Parse(p.I)
	if err != nil {
		return time.Time{}, uuid.Nil, errors.New("malformed cursor (bad id)")
	}
	return p.C.UTC(), id, nil
}
