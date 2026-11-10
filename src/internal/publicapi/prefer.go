package publicapi

// prefer.go — Prefer: return=minimal (US-8, RFC 7240): honoured only by the
// routes the table marks (a single operation returning one resource with its
// own version — an issue or a comment); elsewhere it changes nothing and no
// Preference-Applied is sent.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// preferMinimalKey marks a request whose route honours Prefer and which asks
// for return=minimal.
type preferMinimalKey struct{}

func preferMinimal(ctx context.Context) bool {
	v, _ := ctx.Value(preferMinimalKey{}).(bool)
	return v
}

// AsksMinimal reports whether the Prefer headers of r ask for
// return=minimal.
func AsksMinimal(r *http.Request) bool {
	for _, h := range r.Header.Values("Prefer") {
		for _, pref := range strings.Split(h, ",") {
			name, value, _ := strings.Cut(strings.TrimSpace(pref), "=")
			value, _, _ = strings.Cut(value, ";")
			if strings.EqualFold(strings.TrimSpace(name), "return") &&
				strings.EqualFold(strings.Trim(strings.TrimSpace(value), `"`), "minimal") {
				return true
			}
		}
	}
	return false
}

// applyPrefer marks the request when its route honours Prefer and it asks
// for return=minimal.
func applyPrefer(r *http.Request, rt Route) *http.Request {
	if !rt.Prefer || !AsksMinimal(r) {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), preferMinimalKey{}, true))
}

var errNoVersion = errors.New("the resource of a route that honours Prefer has no id or version")

// minimal is the {id, version} of a resource body.
func minimal(body []byte) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	id, okID := obj["id"]
	version, okVersion := obj["version"]
	if !okID || !okVersion {
		return nil, errNoVersion
	}
	return json.Marshal(map[string]json.RawMessage{"id": id, "version": version})
}
