package search

import (
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
)

func TestResultCache_BoundedCapacity(t *testing.T) {
	cache, err := newResultCache(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// The existing cache policy holds 256 distinct pools. More fresh
	// generations must evict old pools instead of growing without bound.
	for i := 0; i < 257; i++ {
		cache.Put(strconv.Itoa(i), nil, uuid.Nil, "scope", rankingFTS)
	}
	if _, ok := cache.Get("0"); ok {
		t.Fatal("oldest pool survived capacity pressure")
	}
	if _, ok := cache.Get("256"); !ok {
		t.Fatal("newest pool was not retained")
	}
}

func TestResultCache_ExpiredOrphansDoNotEvictLivePool(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache, err := newResultCache(time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 255; i++ {
			cache.Put("orphan-"+strconv.Itoa(i), nil, uuid.Nil, "scope", rankingFTS)
		}
		time.Sleep(30 * time.Second) // Advances only the synctest clock.
		cache.Put("live", nil, uuid.Nil, "scope", rankingFTS)
		// Make the younger live pool least recently used without extending
		// the older generations' TTL. Their keys will never be read again.
		for i := 0; i < 255; i++ {
			if _, ok := cache.Get("orphan-" + strconv.Itoa(i)); !ok {
				t.Fatal("fixture orphan expired before the clock advanced")
			}
		}
		time.Sleep(31 * time.Second)
		cache.Put("new", nil, uuid.Nil, "scope", rankingFTS)
		if _, ok := cache.Get("live"); !ok {
			t.Fatal("expired orphan generations displaced a still-live pool")
		}
	})
}
