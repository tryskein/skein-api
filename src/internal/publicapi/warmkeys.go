package publicapi

// warmkeys.go — the keys verified recently, which still pass while their
// client address is over the failed-authentication limit (spec NFR, OST-2-3).

import (
	"crypto/sha256"
	"sync"
	"time"
)

// WarmKeyWindow is how long a key stays warm after its last full successful
// verification (spec: "keys verified within the last 15 minutes").
const WarmKeyWindow = 15 * time.Minute

// warmKeysCapacity bounds the warm set: at the cap, an insert first drops
// the expired entries and, if that frees nothing, evicts the entry closest to
// expiry.
const warmKeysCapacity = 10000

// WarmKeys is the set of warm keys: the SHA-256 of a presented key → the time
// of its last full successful authentication. It is filled only after an
// authentication succeeds, never by a rejected or failed one. This is not
// the bcrypt cache of internal/auth (60 s, a key match only): a warm key here
// is one whose whole verification — status, expiry, owner — passed.
type WarmKeys struct {
	mu       sync.Mutex
	verified map[[sha256.Size]byte]time.Time
	window   time.Duration
	capacity int
	now      func() time.Time
}

// NewWarmKeys builds the warm set. now is the clock (time.Now in production).
func NewWarmKeys(window time.Duration, capacity int, now func() time.Time) *WarmKeys {
	if now == nil {
		now = time.Now
	}
	return &WarmKeys{
		verified: make(map[[sha256.Size]byte]time.Time),
		window:   window,
		capacity: capacity,
		now:      now,
	}
}

// newProductWarmKeys is the warm set of the public server.
func newProductWarmKeys() *WarmKeys {
	return NewWarmKeys(WarmKeyWindow, warmKeysCapacity, time.Now)
}

// IsWarm reports whether plain authenticated fully within the window.
func (k *WarmKeys) IsWarm(plain string) bool {
	h := sha256.Sum256([]byte(plain))
	k.mu.Lock()
	defer k.mu.Unlock()
	at, ok := k.verified[h]
	return ok && k.now().Sub(at) < k.window
}

// Verified records a full successful authentication of plain now. Call it
// only for an authenticated outcome.
func (k *WarmKeys) Verified(plain string) {
	h := sha256.Sum256([]byte(plain))
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	if _, ok := k.verified[h]; !ok && len(k.verified) >= k.capacity {
		k.makeRoom(now)
	}
	k.verified[h] = now
}

// makeRoom drops the expired entries; when none expired, it evicts the entry
// closest to expiry (the oldest verification).
func (k *WarmKeys) makeRoom(now time.Time) {
	var oldestKey [sha256.Size]byte
	var oldest time.Time
	found := false
	for h, at := range k.verified {
		if now.Sub(at) >= k.window {
			delete(k.verified, h)
			continue
		}
		if !found || at.Before(oldest) {
			oldestKey, oldest, found = h, at, true
		}
	}
	if len(k.verified) >= k.capacity && found {
		delete(k.verified, oldestKey)
	}
}
