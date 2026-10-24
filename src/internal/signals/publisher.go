package signals

// publisher.go — sk-622.21 T014 (decision K9, D6): publishing a committed
// batch to NATS Core, one message per user, no coalescing window (coalescing
// per connection is skein-web's job, T036).
//
// The publisher is process-wide on purpose. The transaction wrappers that
// call [BeginTxFunc] live in eight stores, and those stores are constructed
// in many places (handlers, workers, the outbox) from nothing but a pool;
// threading a publisher through every constructor would leave any store
// built without it silently signal-less. One publisher configured once in
// main — the same *nats.Conn the outbox dispatcher and themes.Publisher use —
// reaches every path, and an unconfigured process (no NATS_URL, or a test
// that did not ask for NATS) is a no-op, like themes.NewPublisher(nil).

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
)

// publisher is the configured NATS connection; nil means NATS is disabled.
type publisher struct {
	nc *nats.Conn
}

var current atomic.Pointer[publisher]

// Configure installs nc as the connection every committed batch is published
// to. nil disables publishing (no-op). main calls it once, right after the
// NATS connection is made; tests that observe signals call it with their
// in-process connection and restore nil in cleanup.
func Configure(nc *nats.Conn) {
	if nc == nil {
		current.Store(nil)
		return
	}
	current.Store(&publisher{nc: nc})
}

// Flush publishes b: one `skein.user.{uid}.signal` message per user in b,
// carrying the union of that user's areas in canonical order and, with the
// `spaces` area, the transaction's space events of that user; then one
// `skein.user.{uid}.access_changed` per user whose access changed. Call it only
// after the change b describes is committed (INV-12) — [BeginTxFunc] does.
//
// Delivery is best-effort (decision D6): a publish error is logged and the
// remaining users are still attempted; Flush never reports failure to the
// caller, whose change is already committed. Flush does not touch the
// database. Without a configured connection it does nothing.
func Flush(ctx context.Context, b *Batch) {
	p := current.Load()
	if p == nil || b == nil {
		return
	}
	sentAt := time.Now().UTC()
	for _, e := range b.entries() {
		body, err := json.Marshal(Signal{Areas: e.areas, Spaces: e.spaces, SentAt: sentAt})
		if err != nil {
			// Signal holds a slice of strings and a time: marshalling cannot
			// fail short of a programming error.
			slog.ErrorContext(ctx, "signals: marshal personal signal failed",
				"userId", e.userID.String(), "error", err.Error())
			continue
		}
		if err := p.nc.Publish(SignalSubject(e.userID), body); err != nil {
			slog.WarnContext(ctx, "signals: publish personal signal failed",
				"userId", e.userID.String(), "error", err.Error())
			continue
		}
		for _, a := range e.areas {
			personalSignalsPublishedTotal.WithLabelValues(string(a)).Inc()
		}
	}
	// access_changed (K-15): one event per user whose access changed in the
	// committed transaction, after the personal signals.
	for _, e := range b.accessEntries() {
		body := e.body
		body.SentAt = sentAt
		raw, err := json.Marshal(body)
		if err != nil {
			slog.ErrorContext(ctx, "signals: marshal access_changed failed",
				"userId", e.userID.String(), "error", err.Error())
			continue
		}
		if err := p.nc.Publish(AccessChangedSubject(e.userID), raw); err != nil {
			slog.WarnContext(ctx, "signals: publish access_changed failed",
				"userId", e.userID.String(), "error", err.Error())
		}
	}
}
