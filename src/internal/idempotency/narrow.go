package idempotency

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"skein-api/internal/authz"
	"skein-api/internal/errcode"
	"skein-api/internal/redact"
)

// narrowedCodes are the stored refusals whose bodies name other issues and
// are narrowed on a repeat (sk-1007 US-6, D7, K8).
var narrowedCodes = map[string]bool{
	errcode.DependencyCycle.String():         true,
	errcode.StandingHasOpenChildren.String(): true,
}

// The members of those bodies that name issues, a closed list taken from
// their builders (issueops/createhelp.go#MapInlineDepError for an inline
// dependency of createIssue, issueops/closeerrors.go#ClassifyCloseError for
// closeIssue, as issueops.Failure fields become problem extensions in
// publicapi/issues_write.go#writeFailure).
const (
	memberPath        = "path"
	memberChildIDs    = "childIds"
	memberUnavailable = "unavailableOpenChildren"
)

// markedMembers are the single-id members: an id the caller cannot see now
// becomes redact.HiddenMarker.
var markedMembers = []string{"toId", "id"}

// member is one top-level member of a JSON object, in its stored order.
type member struct {
	key string
	raw json.RawMessage
}

// narrowStored narrows the stored body of a dependency_cycle or
// standing_has_open_children refusal by what the caller of ctx sees now,
// with the key's scope (sk-1007 US-6), reading visibility once for every
// issue the body names:
//
//   - path stays only when every issue on it is visible; otherwise the
//     member is removed;
//   - childIds keeps the visible ids in their order, and
//     unavailableOpenChildren becomes the stored number (0 when the body
//     has none) plus the stored ids that are not visible now — added beside
//     childIds when the body had no such member; a body without childIds
//     gets no number;
//   - toId and id become the hidden marker when not a visible id; a value
//     that is not a string (a number, an object, a list) is hidden too, an
//     empty string and null stay.
//
// Nothing else is added, nothing hidden by the first answer comes back
// (the narrowing never widens a body), every other member keeps its place,
// and a body that needs no change is returned with its exact bytes. A body
// of another code, or one that is not a JSON object, is returned as is.
func narrowStored(ctx context.Context, q authz.RowsQuerier, scope authz.Scope, body []byte) ([]byte, error) {
	members, ok := decodeMembers(body)
	if !ok {
		return body, nil
	}
	get := func(key string) (json.RawMessage, int) {
		for i, m := range members {
			if m.key == key {
				return m.raw, i
			}
		}
		return nil, -1
	}
	var code string
	if raw, _ := get("code"); raw == nil || json.Unmarshal(raw, &code) != nil || !narrowedCodes[code] {
		return body, nil
	}

	var path, children []string
	pathRaw, pathAt := get(memberPath)
	if pathAt >= 0 {
		path = stringList(pathRaw)
	}
	childRaw, childAt := get(memberChildIDs)
	if childAt >= 0 {
		children = stringList(childRaw)
	}
	ids := append(append([]string{}, path...), children...)
	for _, key := range markedMembers {
		if raw, at := get(key); at >= 0 {
			var s string
			if json.Unmarshal(raw, &s) == nil && s != "" {
				ids = append(ids, s)
			}
		}
	}
	visible, err := authz.VisibleIssueIDs(ctx, q, scope, ids)
	if err != nil {
		return nil, fmt.Errorf("reading the visibility of a stored refusal's issues: %w", err)
	}
	seen := func(id string) bool { _, ok := visible[id]; return ok }

	changed := false
	drop := map[int]bool{}
	if pathAt >= 0 {
		for _, id := range path {
			if !seen(id) {
				drop[pathAt] = true
				changed = true
				break
			}
		}
		if path == nil && !isNullRaw(pathRaw) {
			// Not a list of ids: nothing in it can be shown as visible.
			drop[pathAt] = true
			changed = true
		}
	}
	var insertAfter = -1
	var unavailableRaw json.RawMessage
	if childAt >= 0 {
		kept := []string{}
		for _, id := range children {
			if seen(id) {
				kept = append(kept, id)
			}
		}
		stored := 0
		uRaw, uAt := get(memberUnavailable)
		if uAt >= 0 {
			if n, err := strconv.Atoi(string(bytes.TrimSpace(uRaw))); err == nil && n > 0 {
				stored = n
			}
		}
		hidden := len(children) - len(kept)
		if children == nil && !isNullRaw(childRaw) {
			hidden = 0 // not a list of ids: shown as an empty list below
		}
		if hidden > 0 || (children == nil && !isNullRaw(childRaw)) {
			b, _ := json.Marshal(kept)
			members[childAt].raw = b
			changed = true
		}
		unavailableRaw = json.RawMessage(strconv.Itoa(stored + hidden))
		switch {
		case uAt < 0:
			insertAfter = childAt
			changed = true
		case hidden > 0:
			members[uAt].raw = unavailableRaw
		}
	}
	for _, key := range markedMembers {
		raw, at := get(key)
		if at < 0 {
			continue
		}
		if isNullRaw(raw) {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil && (s == "" || seen(s)) {
			continue
		}
		// A hidden id, or a value that is not an id string at all (a
		// number, an object, a list): nothing of it is known to be visible
		// (fail-closed).
		b, _ := json.Marshal(redact.HiddenMarker)
		if !bytes.Equal(raw, b) {
			members[at].raw = b
			changed = true
		}
	}
	if !changed {
		return body, nil
	}

	var out bytes.Buffer
	out.WriteByte('{')
	first := true
	write := func(key string, raw json.RawMessage) {
		if !first {
			out.WriteByte(',')
		}
		first = false
		k, _ := json.Marshal(key)
		out.Write(k)
		out.WriteByte(':')
		out.Write(raw)
	}
	for i, m := range members {
		if drop[i] {
			continue
		}
		write(m.key, m.raw)
		if i == insertAfter {
			write(memberUnavailable, unavailableRaw)
		}
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

// decodeMembers reads a JSON object's top-level members in order.
func decodeMembers(body []byte) ([]member, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, false
	}
	var out []member
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, ok := tok.(string)
		if !ok {
			return nil, false
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, false
		}
		out = append(out, member{key: key, raw: raw})
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, false
	}
	return out, true
}

// stringList reads a list of ids; nil when raw is not a list of strings.
func stringList(raw json.RawMessage) []string {
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return nil
	}
	return out
}

func isNullRaw(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
