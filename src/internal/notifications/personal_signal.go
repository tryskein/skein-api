package notifications

// personal_signal.go — sk-622.21 T016 (US-11, K13): the personal signal of the
// operations on notifications, subscriptions and settings. Paths that run in
// a transaction opened by signals.BeginTxFunc name their addressees with
// signals.Add; the autocommit statements below publish right after the
// statement returned, i.e. after it committed (INV-12).

import (
	"context"

	"github.com/google/uuid"

	"skein-api/internal/signals"
)

// mentionTypes is the mention family as a SQL array argument: the Mentions
// badge counts exactly these types, every other feed row belongs to the
// Watching side (handler/me.go#countMentions).
func mentionTypes() []string {
	out := make([]string, 0, len(mentionEventTypes))
	for t := range mentionEventTypes {
		out = append(out, t)
	}
	return out
}

// signalCommitted publishes one personal signal to userID with areas, for a
// change that is already committed.
func signalCommitted(ctx context.Context, userID uuid.UUID, areas ...signals.Area) {
	b := signals.NewBatch()
	b.Add(userID, areas...)
	signals.Flush(ctx, b)
}

// signalOwnerRows publishes the signal of a read / unread / archive over the
// owner's rows: mentions when a mention row was touched, watching when any
// other feed row was.
func signalOwnerRows(ctx context.Context, userID uuid.UUID, touchedMention, touchedOther bool) {
	var areas []signals.Area
	if touchedMention {
		areas = append(areas, signals.AreaMentions)
	}
	if touchedOther {
		areas = append(areas, signals.AreaWatching)
	}
	if len(areas) > 0 {
		signalCommitted(ctx, userID, areas...)
	}
}
