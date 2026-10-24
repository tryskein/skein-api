// Package logging provides a slog handler that emits logfmt for non-error
// records and JSON for error+ records, with automatic secret-field redaction.
//
// Rationale: ordinary logs (info/debug/warn) are 99% of volume — logfmt is
// compact and human-readable. Errors are rare but need maximum context for
// Loki search — JSON allows label-based filtering on every field.
package logging

import (
	"context"
	"io"
	"log/slog"
	"strings"
)

// secretKeyTokens are case-insensitive substrings; if a log attribute key
// contains any of them, the value is replaced with "***" before serialization.
var secretKeyTokens = []string{
	"password",
	"token",
	"secret",
	"key",
	"credential",
	"authorization",
	"apikey",
	"api_key",
}

// DualHandler is a slog.Handler that routes records to one of two backing
// handlers based on level: a TextHandler (logfmt) for < Error, a JSONHandler
// for >= Error. Both backing handlers share a sanitizer ReplaceAttr that
// redacts secret-bearing field values.
type DualHandler struct {
	level       slog.Level
	textHandler slog.Handler
	jsonHandler slog.Handler
	// contextAttrs are consulted on EVERY record, so a value that lives in
	// the request context lands on every line without each call site
	// remembering to pass it. That is the point: a field only some call
	// sites remember is worse than no field, because its absence reads as
	// "this call had no session" rather than "this line forgot to say".
	contextAttrs []func(context.Context) []slog.Attr
	// boundKeys are the attr keys bound through WithAttrs in the CURRENT
	// group namespace. They never reach the record — slog keeps them in
	// the backing handler — so without remembering them here the key check
	// in withContextAttrs cannot see them, and a logger built with
	// .With("sessionId", …) emits the key twice. WithGroup starts a fresh
	// set: attrs bound after a group opens are namespaced under it, and so
	// are the context attrs added there, so the two collide only within
	// the same namespace.
	boundKeys map[string]struct{}
}

// NewDualHandler constructs a DualHandler writing to w with minimum level.
//
// contextAttrs are optional extractors run against the record's context; each
// returned attr is added to the record unless the record (or the handler's own
// WithAttrs chain) already carries that key — an explicitly logged value always
// wins over the ambient one. Passing none keeps the handler's previous
// behaviour exactly, which is why this is variadic rather than a new parameter.
func NewDualHandler(w io.Writer, level slog.Level, contextAttrs ...func(context.Context) []slog.Attr) *DualHandler {
	sanitize := func(_ []string, a slog.Attr) slog.Attr {
		key := strings.ToLower(a.Key)
		for _, secret := range secretKeyTokens {
			if strings.Contains(key, secret) {
				return slog.String(a.Key, "***")
			}
		}
		return a
	}

	return &DualHandler{
		level:        level,
		contextAttrs: contextAttrs,
		boundKeys:    map[string]struct{}{},
		textHandler: slog.NewTextHandler(w, &slog.HandlerOptions{
			Level:       level,
			ReplaceAttr: sanitize,
		}),
		jsonHandler: slog.NewJSONHandler(w, &slog.HandlerOptions{
			Level:       level,
			ReplaceAttr: sanitize,
			AddSource:   true,
		}),
	}
}

// Enabled reports whether the handler handles records at the given level.
func (h *DualHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

// Handle dispatches the record to JSON for Error+ and text for everything else,
// after folding in any context-derived attrs.
func (h *DualHandler) Handle(ctx context.Context, r slog.Record) error {
	r = h.withContextAttrs(ctx, r)
	if r.Level >= slog.LevelError {
		return h.jsonHandler.Handle(ctx, r)
	}
	return h.textHandler.Handle(ctx, r)
}

// withContextAttrs appends the configured context-derived attrs to a copy of
// the record, skipping any key the record already carries.
//
// The record is cloned before mutation: slog.Record shares backing storage
// between copies, and appending to the original would corrupt a record a
// caller may still hold.
func (h *DualHandler) withContextAttrs(ctx context.Context, r slog.Record) slog.Record {
	if len(h.contextAttrs) == 0 || ctx == nil {
		return r
	}
	var add []slog.Attr
	for _, extract := range h.contextAttrs {
		for _, a := range extract(ctx) {
			if a.Equal(slog.Attr{}) || recordHasKey(r, a.Key) {
				continue
			}
			if _, bound := h.boundKeys[a.Key]; bound {
				continue
			}
			add = append(add, a)
		}
	}
	if len(add) == 0 {
		return r
	}
	clone := r.Clone()
	clone.AddAttrs(add...)
	return clone
}

// recordHasKey reports whether the record already carries an attr with this
// key. An explicit value at the call site must not be shadowed or duplicated
// by the ambient one — logfmt would emit the key twice and Loki would keep
// whichever it saw last.
func recordHasKey(r slog.Record, key string) bool {
	found := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			found = true
			return false
		}
		return true
	})
	return found
}

// WithAttrs returns a new handler whose backing handlers carry the additional
// attrs.
func (h *DualHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	bound := make(map[string]struct{}, len(h.boundKeys)+len(attrs))
	for k := range h.boundKeys {
		bound[k] = struct{}{}
	}
	for _, a := range attrs {
		bound[a.Key] = struct{}{}
	}
	return &DualHandler{
		level:        h.level,
		contextAttrs: h.contextAttrs,
		boundKeys:    bound,
		textHandler:  h.textHandler.WithAttrs(attrs),
		jsonHandler:  h.jsonHandler.WithAttrs(attrs),
	}
}

// WithGroup returns a new handler with a named group prepended to attribute
// keys on both backing handlers.
func (h *DualHandler) WithGroup(name string) slog.Handler {
	// A new namespace: keys bound before it can no longer collide with a
	// context attr added inside it, so the set starts empty rather than
	// carrying over and suppressing an attr that would not have clashed.
	return &DualHandler{
		level:        h.level,
		contextAttrs: h.contextAttrs,
		boundKeys:    map[string]struct{}{},
		textHandler:  h.textHandler.WithGroup(name),
		jsonHandler:  h.jsonHandler.WithGroup(name),
	}
}

// ParseLevel maps a textual level (debug|info|warn|error) to slog.Level.
// Unknown values fall back to slog.LevelInfo.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
