package themes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

// Publisher is the contract the theme handlers / MCP tools call to push
// theme-related events into NATS. Three subjects:
//
//   - skein.user.{uid}.theme.preview          — ephemeral preview (user-scoped)
//   - skein.user.{authorId}.theme.updated     — persisted change (the author's sessions)
//   - skein.user.{uid}.theme.user-pref.updated — caller's pref changed
//
// We bypass the persistent outbox here on purpose:
//
//   - preview is purely ephemeral — re-delivery after a pod crash would
//     show the user a stale preview of an unsaved nudge.
//   - theme.updated reaches the author's sessions; subscribers refetch
//     via REST before applying, so dedup-by-id isn't required.
//   - theme.user-pref.updated is a poke at the originating user's
//     frontend to refetch /me/theme; missing a single delivery is fine
//     because the next theme.updated event picks up the slack.
//
// # Personal theme subject (sk-674.18, K-31, US-21)
//
// With spaces, themes become personal: a theme belongs to its author, and
// nobody else sees its name, its author or its changes. The broadcast
// subject above is therefore replaced by a personal one:
//
//	skein.user.{authorId}.theme.updated
//
// with the same body as today, `{"themeId":"…","variables":{…}}` (the full
// post-merge map). It is delivered only to the author's sessions; the
// broadcast `skein.events.theme.updated.{themeId}` is no longer published.
// skein-web treats it as an event of one user (its SubjectParser parses
// `skein.user.{uid}.theme.updated` like the other `skein.user.{uid}.theme.*`
// subjects) and ships the switch together with skein-api (D-11). The
// publisher switched in sk-674.18 T035.
//
// The interface keeps tests off of a real NATS instance — RecordingPublisher
// in publisher_recording.go satisfies it for unit + integration tests.
type Publisher interface {
	PublishThemePreview(ctx context.Context, userID uuid.UUID, variables map[string]string) error
	PublishThemeUpdated(ctx context.Context, authorID, themeID uuid.UUID, variables map[string]string) error
	PublishThemeUserPrefUpdated(ctx context.Context, userID uuid.UUID) error
}

// natsPublisher is the production implementation. Wraps *nats.Conn so
// the same connection used by the outbox dispatcher is reused (one TCP
// per process). NATS.Publish is fire-and-forget; we do NOT flush after
// each call — the connection's internal flush cadence covers it, and
// the loss surface for these ephemeral subjects is acceptable.
type natsPublisher struct {
	nc *nats.Conn
}

// NewPublisher returns a publisher backed by nc. nc may be nil — that
// case returns a no-op publisher so the rest of the wiring still
// compiles in NATS-disabled dev mode. The MCP tool handlers detect the
// nil case via PublishThemePreview returning ErrNATSDisabled.
func NewPublisher(nc *nats.Conn) Publisher {
	if nc == nil {
		return &noopPublisher{}
	}
	return &natsPublisher{nc: nc}
}

// ErrNATSDisabled is returned by every method on noopPublisher. The MCP
// tools surface it as "preview_unavailable" / "live_updates_unavailable"
// so an agent operator sees the misconfiguration in logs.
var ErrNATSDisabled = errors.New("nats disabled: NATS_URL not set")

// ErrInvalidPublishTarget is returned when a publish helper is called with
// a zero uuid (programmer error). Distinguishes "you called me wrong" from
// "NATS is disabled" (ErrNATSDisabled) and "publish hit the wire and
// failed" (raw NATS error). Callers branch on it via errors.Is to log at
// Error level (the call site shouldn't have happened) without spamming
// the warn-level "publish failed" channel.
var ErrInvalidPublishTarget = errors.New("publish target is zero uuid")

// noopPublisher returns ErrNATSDisabled for every call. Lets us boot
// skein-api without NATS_URL while still exposing the theme MCP tools
// for read paths (skein_theme, actions describe / fork).
type noopPublisher struct{}

func (*noopPublisher) PublishThemePreview(_ context.Context, _ uuid.UUID, _ map[string]string) error {
	return ErrNATSDisabled
}

func (*noopPublisher) PublishThemeUpdated(_ context.Context, _, _ uuid.UUID, _ map[string]string) error {
	return ErrNATSDisabled
}

func (*noopPublisher) PublishThemeUserPrefUpdated(_ context.Context, _ uuid.UUID) error {
	return ErrNATSDisabled
}

// themePreviewPayload is the body of skein.user.{uid}.theme.preview.
// Variables is the FULL partial map the agent supplied — subscribers
// apply each (key, value) pair as a CSS custom-property override on the
// preview pseudo-element. UserId is denormalised onto the payload so a
// future wildcard subscriber (admin dashboard) can route without parsing
// the subject.
type themePreviewPayload struct {
	UserID    uuid.UUID         `json:"userId"`
	Variables map[string]string `json:"variables"`
}

// themeUpdatedPayload is the body of skein.user.{authorId}.theme.updated.
// Variables is the FULL post-merge map of the theme — subscribers can
// either splice the whole map onto :root or invalidate their per-id
// theme cache and refetch.
type themeUpdatedPayload struct {
	ThemeID   uuid.UUID         `json:"themeId"`
	Variables map[string]string `json:"variables"`
}

// PublishThemePreview pushes an ephemeral preview to the originating
// user's connections. Subject: skein.user.{uid}.theme.preview.
//
// ctx is used only for slog on failure — NATS Publish is fire-and-forget
// by design and does not honour cancellation (no ctx.Err pre-check).
// Returns an error only when the marshal fails (programming error) or
// the NATS Publish call returns one (connection torn down). Callers
// should LOG the error but NOT fail the parent operation — preview is
// best-effort.
func (p *natsPublisher) PublishThemePreview(ctx context.Context, userID uuid.UUID, variables map[string]string) error {
	if userID == uuid.Nil {
		return ErrInvalidPublishTarget
	}
	body, err := json.Marshal(themePreviewPayload{
		UserID:    userID,
		Variables: variables,
	})
	if err != nil {
		return fmt.Errorf("marshal theme.preview: %w", err)
	}
	subject := "skein.user." + userID.String() + ".theme.preview"
	if err := p.nc.Publish(subject, body); err != nil {
		slog.ErrorContext(ctx, "theme preview publish failed",
			"subject", subject, "userId", userID.String(), "error", err.Error())
		return fmt.Errorf("nats publish %s: %w", subject, err)
	}
	return nil
}

// PublishThemeUpdated delivers a theme change to its author's sessions
// only (sk-674.18 T035, K-31, US-21). Subject:
// skein.user.{authorId}.theme.updated — nobody else learns the theme's
// name, author or values. ctx is slog-only (fire-and-forget NATS Publish;
// no cancellation).
func (p *natsPublisher) PublishThemeUpdated(ctx context.Context, authorID, themeID uuid.UUID, variables map[string]string) error {
	if authorID == uuid.Nil || themeID == uuid.Nil {
		return ErrInvalidPublishTarget
	}
	body, err := json.Marshal(themeUpdatedPayload{
		ThemeID:   themeID,
		Variables: variables,
	})
	if err != nil {
		return fmt.Errorf("marshal theme.updated: %w", err)
	}
	subject := "skein.user." + authorID.String() + ".theme.updated"
	if err := p.nc.Publish(subject, body); err != nil {
		slog.ErrorContext(ctx, "theme updated publish failed",
			"subject", subject, "themeId", themeID.String(), "error", err.Error())
		return fmt.Errorf("nats publish %s: %w", subject, err)
	}
	return nil
}

// PublishThemeUserPrefUpdated pokes the targeted user's frontend to
// refetch /me/theme. Subject: skein.user.{uid}.theme.user-pref.updated.
// Body is an empty JSON object — the message itself is the signal.
// ctx is slog-only (fire-and-forget NATS Publish; no cancellation).
//
// Used after skein_theme(action="fork") (the caller's customThemeId moved to
// the freshly-created theme) so the frontend immediately picks up
// the new active theme without a full reload.
func (p *natsPublisher) PublishThemeUserPrefUpdated(ctx context.Context, userID uuid.UUID) error {
	if userID == uuid.Nil {
		return ErrInvalidPublishTarget
	}
	subject := "skein.user." + userID.String() + ".theme.user-pref.updated"
	if err := p.nc.Publish(subject, []byte("{}")); err != nil {
		slog.ErrorContext(ctx, "theme user-pref publish failed",
			"subject", subject, "userId", userID.String(), "error", err.Error())
		return fmt.Errorf("nats publish %s: %w", subject, err)
	}
	return nil
}
