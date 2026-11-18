package themes

import (
	"fmt"
	"math/bits"
	"time"
)

// InspectLimits bounds the inspector selections a user may post
// (POST /me/theme/inspect), what the process keeps of them and what
// skein_theme(action="describe") hands an agent (sk-977.7.10; the values and
// their reasoning: docs/research/theme-inspect-limits.md of the workspace).
// Sizes are bytes of UTF-8 after JSON decoding, except MaxBodyBytes (bytes
// of the HTTP body) and the cache and describe budgets (bytes of the stored
// compact JSON, see InspectEntryCost).
type InspectLimits struct {
	MaxBodyBytes       int64
	MaxSelectorBytes   int
	MaxParents         int
	MaxParentBytes     int
	MaxTokens          int
	MaxTokenBytes      int
	MaxStyleKeys       int
	MaxStyleKeyBytes   int
	MaxStyleValueBytes int
	MaxEntryBytes      int
	MaxEntriesPerUser  int
	TTL                time.Duration
	CacheMaxBytes      int64
	DescribeMaxEntries int
	DescribeMaxBytes   int
	MaxInFlight        int
}

// DefaultInspectLimits are the defaults the owner accepted (2026-11-18).
func DefaultInspectLimits() InspectLimits {
	return InspectLimits{
		MaxBodyBytes:       64 << 10,
		MaxSelectorBytes:   1024,
		MaxParents:         16,
		MaxParentBytes:     1024,
		MaxTokens:          128,
		MaxTokenBytes:      64,
		MaxStyleKeys:       16,
		MaxStyleKeyBytes:   64,
		MaxStyleValueBytes: 1024,
		MaxEntryBytes:      8 << 10,
		MaxEntriesPerUser:  20,
		TTL:                10 * time.Minute,
		CacheMaxBytes:      8 << 20,
		DescribeMaxEntries: 20,
		DescribeMaxBytes:   32 << 10,
		MaxInFlight:        32,
	}
}

// inspectEntryOverhead is the fixed accounting of one cached entry beyond
// its JSON: the entry struct, its two list elements and the per-user index.
const inspectEntryOverhead = 512

// InspectEntryCost is what one stored entry of n JSON bytes counts against
// CacheMaxBytes: n rounded up to a power of two (the allocation class of
// the copy) plus inspectEntryOverhead.
func InspectEntryCost(n int) int64 {
	if n <= 1 {
		return 1 + inspectEntryOverhead
	}
	return int64(1)<<bits.Len(uint(n-1)) + inspectEntryOverhead
}

// Validate rejects a non-positive value and budgets that cannot hold one
// maximal entry: the describe budget must fit one entry and its brackets,
// the cache one entry's cost.
func (l InspectLimits) Validate() error {
	ints := []struct {
		name string
		v    int64
	}{
		{"SKEIN_THEME_INSPECT_MAX_BODY_BYTES", l.MaxBodyBytes},
		{"SKEIN_THEME_INSPECT_MAX_SELECTOR_BYTES", int64(l.MaxSelectorBytes)},
		{"SKEIN_THEME_INSPECT_MAX_PARENTS", int64(l.MaxParents)},
		{"SKEIN_THEME_INSPECT_MAX_PARENT_BYTES", int64(l.MaxParentBytes)},
		{"SKEIN_THEME_INSPECT_MAX_TOKENS", int64(l.MaxTokens)},
		{"SKEIN_THEME_INSPECT_MAX_TOKEN_BYTES", int64(l.MaxTokenBytes)},
		{"SKEIN_THEME_INSPECT_MAX_STYLE_KEYS", int64(l.MaxStyleKeys)},
		{"SKEIN_THEME_INSPECT_MAX_STYLE_KEY_BYTES", int64(l.MaxStyleKeyBytes)},
		{"SKEIN_THEME_INSPECT_MAX_STYLE_VALUE_BYTES", int64(l.MaxStyleValueBytes)},
		{"SKEIN_THEME_INSPECT_MAX_ENTRY_BYTES", int64(l.MaxEntryBytes)},
		{"SKEIN_THEME_INSPECT_MAX_ENTRIES_PER_USER", int64(l.MaxEntriesPerUser)},
		{"SKEIN_THEME_INSPECT_CACHE_MAX_BYTES", l.CacheMaxBytes},
		{"SKEIN_THEME_INSPECT_DESCRIBE_MAX_ENTRIES", int64(l.DescribeMaxEntries)},
		{"SKEIN_THEME_INSPECT_DESCRIBE_MAX_BYTES", int64(l.DescribeMaxBytes)},
		{"SKEIN_THEME_INSPECT_MAX_IN_FLIGHT", int64(l.MaxInFlight)},
	}
	for _, f := range ints {
		if f.v <= 0 {
			return fmt.Errorf("%s must be > 0; got %d", f.name, f.v)
		}
	}
	if l.TTL <= 0 {
		return fmt.Errorf("SKEIN_THEME_INSPECT_TTL must be > 0; got %s", l.TTL)
	}
	if l.DescribeMaxBytes < l.MaxEntryBytes+2 {
		return fmt.Errorf("SKEIN_THEME_INSPECT_DESCRIBE_MAX_BYTES (%d) must hold one entry of SKEIN_THEME_INSPECT_MAX_ENTRY_BYTES (%d) and the brackets",
			l.DescribeMaxBytes, l.MaxEntryBytes)
	}
	if c := InspectEntryCost(l.MaxEntryBytes); l.CacheMaxBytes < c {
		return fmt.Errorf("SKEIN_THEME_INSPECT_CACHE_MAX_BYTES (%d) must hold the cost of one entry of SKEIN_THEME_INSPECT_MAX_ENTRY_BYTES (%d)",
			l.CacheMaxBytes, c)
	}
	return nil
}
