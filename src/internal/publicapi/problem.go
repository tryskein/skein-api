package publicapi

// problem.go — the error format of the public API (RFC 9457) and the closed
// mapping of an errcode code to an HTTP status.

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"skein-api/internal/errcode"
)

// ProblemContentType is the media type of every error response of the public
// API (RFC 9457, section 3).
const ProblemContentType = "application/problem+json"

// Origin is where a failure comes from. For most codes the status does not
// depend on it; for two classes it does:
//
//   - a "not found" code answers 404 when the object is the one the path
//     names, and 422 with the same code when the object is a reference in
//     the request body (a parent, a dependency, a close target, duplicateOf,
//     the issues of a batch, the project of a filter) — the code is the one
//     MCP answers with;
//   - validation_error answers 400 when the request parsing layer refuses
//     the request (decoder, form, parameters, limit) and 422 when the domain
//     operation refuses it.
//
// A handler passes the origin explicitly; there is no default.
type Origin uint8

// The origins.
const (
	// OriginPath — the object named by the path of the request.
	OriginPath Origin = iota + 1
	// OriginBody — an object referenced from the request body or query.
	OriginBody
	// OriginParse — the layer that parses the request.
	OriginParse
	// OriginDomain — the domain operation the request runs.
	OriginDomain
)

// String names the origin.
func (o Origin) String() string {
	switch o {
	case OriginPath:
		return "path"
	case OriginBody:
		return "body"
	case OriginParse:
		return "parse"
	case OriginDomain:
		return "domain"
	}
	return "unknown"
}

// Status is the HTTP status of code when the failure comes from origin. It is
// the closed mapping of the "Response codes of the public API" table by the
// class of the code; a class outside the closed set (a programmer error in
// errcode) answers 500.
func Status(code errcode.Code, origin Origin) int {
	switch code.Class() {
	case errcode.ClassNotFound:
		if origin == OriginPath {
			return http.StatusNotFound
		}
		return http.StatusUnprocessableEntity
	case errcode.ClassFormat:
		if origin == OriginParse {
			return http.StatusBadRequest
		}
		return http.StatusUnprocessableEntity
	case errcode.ClassUnauthenticated:
		return http.StatusUnauthorized
	case errcode.ClassPermission:
		return http.StatusForbidden
	case errcode.ClassVersion:
		return http.StatusPreconditionFailed
	case errcode.ClassUnavailable:
		return http.StatusServiceUnavailable
	case errcode.ClassInternal:
		return http.StatusInternalServerError
	case errcode.ClassBusiness:
		return http.StatusUnprocessableEntity
	case errcode.ClassMethod:
		return http.StatusMethodNotAllowed
	case errcode.ClassPayloadTooLarge:
		return http.StatusRequestEntityTooLarge
	case errcode.ClassMediaType:
		return http.StatusUnsupportedMediaType
	}
	return http.StatusInternalServerError
}

// Problem is one error response. The members type, title and status are
// those of RFC 9457; code is the errcode code (the same one MCP answers
// with); retryable is present, and true, only for a transient code — the
// same request may succeed later without any change by the client.
// Extensions carries the members a code adds (the required right of
// insufficient_scope, the current version of version_conflict, …); it never
// overrides the members above.
type Problem struct {
	Code       errcode.Code
	Origin     Origin
	Detail     string
	Extensions map[string]any
}

// problemType is the RFC 9457 type of every problem: the code, not a URI,
// says what happened, so the type is "about:blank" and the title is the
// status phrase (RFC 9457, section 4.2.1).
const problemType = "about:blank"

// WriteProblem writes p as an application/problem+json response with the
// status Status(p.Code, p.Origin).
func WriteProblem(w http.ResponseWriter, p Problem) {
	status := Status(p.Code, p.Origin)
	body := make(map[string]any, len(p.Extensions)+6)
	for k, v := range p.Extensions {
		body[k] = v
	}
	body["type"] = problemType
	body["title"] = http.StatusText(status)
	body["status"] = status
	body["code"] = p.Code.String()
	if p.Detail != "" {
		body["detail"] = p.Detail
	} else {
		delete(body, "detail")
	}
	if p.Code.Transient() {
		body["retryable"] = true
	} else {
		delete(body, "retryable")
	}
	w.Header().Set("Content-Type", ProblemContentType)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("public API: writing a problem response failed",
			"code", p.Code.String(), "status", status, "error", err.Error())
	}
}
