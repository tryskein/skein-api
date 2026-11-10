package publicapi

// rights.go — the mode of a request and the key right of its operation
// (plan D-2, D-5): checked before any object is looked up, so a refusal
// never reveals whether the object exists.

import (
	"net/http"
	"strconv"
)

// modeOf reads the mode of the request: force=true or force=false in the
// query, absent = false. ok is false for any other value or a repeated
// parameter.
func modeOf(r *http.Request) (Mode, bool) {
	vals, present := r.URL.Query()[forceParam]
	if !present {
		return Mode{}, true
	}
	if len(vals) != 1 {
		return Mode{}, false
	}
	force, err := strconv.ParseBool(vals[0])
	if err != nil || (vals[0] != "true" && vals[0] != "false") {
		return Mode{}, false
	}
	return Mode{Force: force}, true
}
