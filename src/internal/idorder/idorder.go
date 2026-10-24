// Package idorder owns the id-order rule of sk-421.2 US-6 — ONE strict total
// order over issue ids for every server surface that lists ids: the issue
// tree's siblings, the blockers of the detail and of a tree node, the
// dependency section, the skein_close open-child warning (sk-426).
//
// It is a leaf package on purpose: both internal/issues and
// internal/dependencies order by it, and dependencies cannot import issues.
// Surfaces sort in Go after the fetch instead of an SQL ORDER BY: an SQL
// expression equivalent to this rule (arbitrary-length digit segments, empty
// segments, the separator tie-break) would be a second implementation to
// keep in step, and every list it orders is fetched whole anyway.
package idorder

import "strings"

// One strict total order shared by the server and the frontend. The source of truth is
// the seven points of spec US-6; the frontend implementation (hierIdCompare) must give
// the same order on the same ids, and both sides are checked against one literal
// reference from the plan appendix, not against each other.
//
// Why not strconv: the digit segment of a custom id can be longer than any
// machine integer, so values are compared character by character after
// stripping leading zeros.

// splitIDSegments splits an id on `-` and `.`, KEEPING empty segments: leading, trailing and
// consecutive separators are significant (point 1).
func splitIDSegments(s string) []string {
	out := make([]string, 0, 4)
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '-' || s[i] == '.' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// isDigitSegment — a non-empty segment made only of digits (point 4).
func isDigitSegment(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// compareIDSegment compares two segments by points 3–5.
func compareIDSegment(a, b string) int {
	aDigit, bDigit := isDigitSegment(a), isDigitSegment(b)
	switch {
	case aDigit && bDigit:
		// Point 3: numeric value of any length; for an equal value
		// the shorter spelling goes first (`1` before `01`).
		ta := strings.TrimLeft(a, "0")
		tb := strings.TrimLeft(b, "0")
		if len(ta) != len(tb) {
			if len(ta) < len(tb) {
				return -1
			}
			return 1
		}
		if ta != tb {
			if ta < tb {
				return -1
			}
			return 1
		}
		if len(a) != len(b) {
			if len(a) < len(b) {
				return -1
			}
			return 1
		}
		return 0
	case aDigit != bDigit:
		// Point 4: a digit segment before any non-digit one (including an empty one).
		if aDigit {
			return -1
		}
		return 1
	default:
		// Point 5: bytewise; an empty segment before any non-empty one — this is a
		// property of string comparison itself.
		if a != b {
			if a < b {
				return -1
			}
			return 1
		}
		return 0
	}
}

// Compare — a total order by the seven points of US-6: a negative number if a
// goes before b, zero when equal, a positive number if after. The signature is
// compatible with slices.SortFunc.
func Compare(a, b string) int {
	as, bs := splitIDSegments(a), splitIDSegments(b)
	n := len(as)
	if len(bs) < n {
		n = len(bs)
	}
	// Point 2: left to right, the first differing segment decides.
	for i := 0; i < n; i++ {
		if c := compareIDSegment(as[i], bs[i]); c != 0 {
			return c
		}
	}
	// Point 6: all common segments are equal → fewer segments goes first.
	if len(as) != len(bs) {
		if len(as) < len(bs) {
			return -1
		}
		return 1
	}
	// Point 7: still equal → bytewise comparison of the whole id (tells apart
	// only the separators: `x-1` versus `x.1`).
	if a != b {
		if a < b {
			return -1
		}
		return 1
	}
	return 0
}
