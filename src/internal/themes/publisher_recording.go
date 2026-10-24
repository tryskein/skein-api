package themes

import (
	"context"
	"sync"

	"github.com/google/uuid"
)

// RecordingPublisher implements Publisher by capturing every call into a
// slice. Tests inspect Records to assert what got published without
// standing up a NATS server. Methods are goroutine-safe under a single
// embedded mutex.
//
// Lives in non-_test.go so production code can take a Publisher
// interface and tests can construct a RecordingPublisher without
// playing visibility games — the type is just plain Go.
type RecordingPublisher struct {
	mu      sync.Mutex
	Records []PublishedRecord
}

// PublishedRecord is one captured call. Kind matches the method name
// minus "Publish" prefix.
type PublishedRecord struct {
	Kind      string            // "preview" | "updated" | "user-pref"
	UserID    uuid.UUID         // populated for preview + user-pref
	ThemeID   uuid.UUID         // populated for updated
	Variables map[string]string // populated for preview + updated (nil for user-pref)
}

// NewRecordingPublisher returns an empty capture buffer.
func NewRecordingPublisher() *RecordingPublisher {
	return &RecordingPublisher{}
}

// PublishThemePreview appends a "preview" record.
func (p *RecordingPublisher) PublishThemePreview(_ context.Context, userID uuid.UUID, variables map[string]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Defensive copy so a caller mutating the map post-publish doesn't
	// rewrite the captured payload.
	cp := make(map[string]string, len(variables))
	for k, v := range variables {
		cp[k] = v
	}
	p.Records = append(p.Records, PublishedRecord{
		Kind:      "preview",
		UserID:    userID,
		Variables: cp,
	})
	return nil
}

// PublishThemeUpdated appends an "updated" record; UserID is the author
// whose sessions the event is addressed to.
func (p *RecordingPublisher) PublishThemeUpdated(_ context.Context, authorID, themeID uuid.UUID, variables map[string]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make(map[string]string, len(variables))
	for k, v := range variables {
		cp[k] = v
	}
	p.Records = append(p.Records, PublishedRecord{
		Kind:      "updated",
		UserID:    authorID,
		ThemeID:   themeID,
		Variables: cp,
	})
	return nil
}

// PublishThemeUserPrefUpdated appends a "user-pref" record.
func (p *RecordingPublisher) PublishThemeUserPrefUpdated(_ context.Context, userID uuid.UUID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Records = append(p.Records, PublishedRecord{
		Kind:   "user-pref",
		UserID: userID,
	})
	return nil
}

// Reset clears the capture buffer. Useful when one test exercises
// multiple write paths and the assertions only care about the last
// burst.
func (p *RecordingPublisher) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Records = nil
}

// Count returns the number of captured records.
func (p *RecordingPublisher) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.Records)
}
