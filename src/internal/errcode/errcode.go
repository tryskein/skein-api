// Package errcode is the closed set of business error codes Skein answers
// with, shared by every entry point: the MCP tools put a code in the `error`
// field of their envelope, the public REST API puts the same code in the
// `code` field of an RFC 9457 problem.
//
// Every code is built by define, which requires its status class and its
// transience: a code without them does not compile. The class is what the
// public API maps to an HTTP status; the transience says whether the outcome
// may change without any action of the client, which decides whether a
// response carries `retryable: true` and whether an idempotent replay keeps
// it.
package errcode

import "fmt"

// Class is the status class of a code. The mapping of a class to an HTTP
// status (and, for ClassNotFound and ClassFormat, the dependence on where the
// failure originated) belongs to the public API.
type Class uint8

// The classes. The zero value is not a class, so every define call has to
// name one.
const (
	// ClassNotFound — the object does not exist or is outside the caller's
	// reach (a missing object and an invisible one get the same answer).
	ClassNotFound Class = iota + 1
	// ClassFormat — the request is malformed.
	ClassFormat
	// ClassUnauthenticated — no valid identity.
	ClassUnauthenticated
	// ClassPermission — the key lacks the right, or its owner lacks the role.
	ClassPermission
	// ClassVersion — optimistic concurrency: the expected version is stale.
	ClassVersion
	// ClassUnavailable — a dependency of the operation is not available.
	ClassUnavailable
	// ClassInternal — a failure inside the service.
	ClassInternal
	// ClassBusiness — the domain rules refuse the operation.
	ClassBusiness
	// ClassMethod — the route does not accept the request's method.
	ClassMethod
	// ClassPayloadTooLarge — the request body exceeds the size limit.
	ClassPayloadTooLarge
	// ClassMediaType — the request body is not of a supported media type.
	ClassMediaType
	// ClassRateLimited — a rate limit of the public API refuses the request
	// until its window ends.
	ClassRateLimited
)

// lastClass is the last class of the closed set; define refuses any class
// after it.
const lastClass = ClassRateLimited

// String names the class.
func (c Class) String() string {
	switch c {
	case ClassNotFound:
		return "not_found"
	case ClassFormat:
		return "format"
	case ClassUnauthenticated:
		return "unauthenticated"
	case ClassPermission:
		return "permission"
	case ClassVersion:
		return "version"
	case ClassUnavailable:
		return "unavailable"
	case ClassInternal:
		return "internal"
	case ClassBusiness:
		return "business"
	case ClassMethod:
		return "method"
	case ClassPayloadTooLarge:
		return "payload_too_large"
	case ClassMediaType:
		return "media_type"
	case ClassRateLimited:
		return "rate_limited"
	}
	return fmt.Sprintf("class(%d)", uint8(c))
}

// Transience says whether the outcome of a code may change while the client
// does nothing: a race, the visibility of a concurrent write, a claim held by
// another holder, an unavailable dependency. The zero value is not a
// transience, so every define call has to name one.
type Transience uint8

// The two transiences.
const (
	// Permanent — the same request in the same state gets the same answer.
	Permanent Transience = iota + 1
	// Transient — the same request may succeed later without any change by
	// the client.
	Transient
)

// Code is one error code of the closed set: its wire name, its status class
// and its transience. Values are built only by define in this package.
type Code struct {
	name       string
	class      Class
	transience Transience
}

// String is the wire name of the code (`error` in MCP, `code` in REST).
func (c Code) String() string { return c.name }

// Class is the status class of the code.
func (c Code) Class() Class { return c.class }

// Transient reports whether the outcome may change without any action of the
// client.
func (c Code) Transient() bool { return c.transience == Transient }

var (
	all    []Code
	byName = map[string]Code{}
)

// define registers a code. It panics on an empty or duplicate name and on a
// class or transience outside the closed sets: such a declaration is a
// programmer error that must stop the binary at start, before any request.
func define(name string, class Class, transience Transience) Code {
	if name == "" {
		panic("errcode: empty code name")
	}
	if class < ClassNotFound || class > lastClass {
		panic(fmt.Sprintf("errcode: %q has no valid class", name))
	}
	if transience != Permanent && transience != Transient {
		panic(fmt.Sprintf("errcode: %q has no valid transience", name))
	}
	if _, dup := byName[name]; dup {
		panic(fmt.Sprintf("errcode: %q defined twice", name))
	}
	c := Code{name: name, class: class, transience: transience}
	all = append(all, c)
	byName[name] = c
	return c
}

// All returns every code of the closed set, in declaration order. The slice
// is a copy.
func All() []Code {
	out := make([]Code, len(all))
	copy(out, all)
	return out
}

// Lookup finds a code by its wire name.
func Lookup(name string) (Code, bool) {
	c, ok := byName[name]
	return c, ok
}
