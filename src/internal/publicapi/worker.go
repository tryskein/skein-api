package publicapi

// worker.go — the executor of a request on the public port (US-5, plan D-6):
// X-Skein-Worker-Id names one executor of the key; without it the executor
// is the key itself. The holder of a lock taken by the request is the
// executor's (api:<keyId>[:<workerId>]); every request extends the activity
// of its executor only.

import "net/http"

// workerIDOf reads X-Skein-Worker-Id. present is false without the header;
// ok is false when it is sent more than once.
func workerIDOf(r *http.Request) (id string, present, ok bool) {
	vals, present := r.Header[http.CanonicalHeaderKey(WorkerIDHeader)]
	if !present {
		return "", false, true
	}
	if len(vals) != 1 {
		return "", true, false
	}
	return vals[0], true, true
}
