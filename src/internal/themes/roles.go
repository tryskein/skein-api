package themes

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ── What this file is for ───────────────────────────────────────────
//
// `TokenSchema` (schema.go) says which roles EXIST. This file says what
// to do about a name that does not — and that question has a different
// answer on each of the two paths an agent can send one down.
//
// A name outside the current dictionary resolves to nothing. Written to
// a saved theme it paints nothing and is dropped by the frontend's
// migration on the next read; sent as a preview it paints nothing and
// the agent gets an `ok` back. Both used to be silent, and silence is
// the whole defect: the agent asks for a colour, sees success, and moves
// on (sk-285.7.4, sk-285.7.5).
//
// So the classification lives here once and both callers consume it:
// `set_variables` REFUSES (a durable write of a dead key is a write that
// can only mislead — and, while it sits in the row, it re-arms the
// frontend's `needsMigration`, whose next write-back then deletes it
// again without telling anyone), and `preview_variables` APPLIES AS SENT
// and warns (a preview is an ephemeral overlay carrying an explicit map
// from the agent; substituting successors there would be rewriting the
// command rather than migrating data — the decision recorded on
// sk-285.7.4).

// RoleStatus says WHY a key is not a role of the current dictionary.
// Two values, and the difference is actionable rather than cosmetic: a
// retired name usually has a successor to point the agent at, an unknown
// one has nothing at all and is most often a typo.
type RoleStatus string

const (
	// RoleRetired — the name belonged to the pre-redesign dictionary and
	// was withdrawn by sk-285.7. Recognisable, and often re-pointable.
	RoleRetired RoleStatus = "retired"
	// RoleUnknown — the name belongs to no dictionary this service knows.
	RoleUnknown RoleStatus = "unknown"
)

// RoleIssue names one variable key the current dictionary does not
// declare. `Successor` is the role that inherited the retired one's job,
// empty when the redesign kept no counterpart (or when the key is simply
// unknown).
type RoleIssue struct {
	Key       string     `json:"key"`
	Status    RoleStatus `json:"status"`
	Successor string     `json:"successor,omitempty"`
}

// RetiredRoles maps every pre-redesign role name that the current
// dictionary NO LONGER declares to the role that inherited its job, or
// to "" when nothing did.
//
// ── Scope: 47 names, not 61 ─────────────────────────────────────────
//
// The old dictionary had 61 roles; the redesign kept 14 of them under
// the same name (`--font-ui`, `--font-mono`, the `--font-xs…xl` steps,
// the four `--font-weight-*`, `--width-sliver`, `--width-resize`,
// `--transition-fast`). Those are current roles and belong in
// TokenSchema, not here. What is left is the 47 names that resolve to
// nothing — exactly the set the agent guide lists between its
// `retired-roles` markers.
//
// ── Why "" is not the same as absent ────────────────────────────────
//
// A key WITH an entry and an empty successor is a name we recognise and
// can say something useful about ("that role is gone, and nothing
// replaced it — pick a role from describe"). A key with no entry at all
// is a name from nowhere. Collapsing the two would tell an agent that
// mistyped `--primaryy` that it had used a retired role, which sends it
// looking for a migration that never happened.
//
// ── The successors are a mirror, and the mirror is one-directional ───
//
// The relation itself is declared in the frontend, in
// `src/lib/themeMigration.ts` (`LEGACY_TO_NEW`), which is where the
// migration actually consumes it; this table is a hand-written copy of
// the same judgement, restricted to the names above. It carries the same
// hazard as TokenSchema and for the same reason — skein-api and
// skein-frontend are separate repositories, so nothing in this repo's CI
// can see the frontend's copy.
//
// What CAN be checked here is checked, and it is not nothing: every
// successor must be a role TokenSchema declares (a stale successor
// pointing at a role that no longer exists would send agents at another
// dead name), no key may itself be a current role, and the key set must
// equal the guide's retired block byte for byte. The one statement out
// of reach is whether `--bg-hover`'s successor is `--elev` rather than
// `--surface`, and that is the same statement `scripts/check-theme-mirror.sh`
// at the workspace root exists to make about the catalogue. Extend that
// script when this table changes; the in-repo guard cannot stand in for it.
//
// Order below follows the old dictionary's own declaration order
// (Surfaces → Text → Brand → Accents → Search → Geometry → Type) so this
// file can be read line by line against the frontend's table.
var RetiredRoles = map[string]string{
	// Surfaces
	"--bg-base":            "--bg",
	"--bg-elevated":        "--elev",
	"--bg-selected":        "--primary-t",
	"--bg-hover":           "--elev",
	"--border":             "--line-2",
	"--border-subtle":      "--line",
	"--radius-default":     "--radius-control",
	"--radius-dialog":      "--radius-panel",
	"--shadow-elevation-1": "--shadow-segment",
	"--shadow-elevation-2": "--shadow-popover",
	// Text
	"--fg-high":     "--ink",
	"--fg-base":     "--ink-2",
	"--fg-low":      "--ink-2",
	"--fg-muted":    "--ink-3",
	"--fg-emphasis": "--ink",
	// Brand
	"--brand":       "--primary",
	"--brand-text":  "--primary",
	"--brand-fill":  "--primary",
	"--brand-hover": "--primary-h",
	"--brand-muted": "--primary-t",
	// Accents
	"--accent-blue":         "--info",
	"--accent-cyan":         "--secondary",
	"--accent-cyan-fill":    "--secondary",
	"--status-standing":     "--secondary",
	"--accent-green":        "--ok",
	"--accent-magenta":      "",
	"--accent-magenta-tint": "",
	"--accent-orange":       "",
	"--accent-red":          "--err",
	"--accent-violet":       "--violet",
	"--accent-yellow":       "",
	"--accent-amber":        "--warn",
	"--accent-amber-fill":   "--warn",
	"--accent-amber-tint":   "--warn-t",
	// Search
	"--match-bg": "--mark",
	"--match-fg": "--on-mark",
	// Geometry
	"--space-xs": "",
	"--space-sm": "--space-1",
	"--space-md": "--space-2",
	"--space-lg": "--space-3",
	"--space-xl": "--space-4",
	// Type
	"--line-tight":        "--line-h4",
	"--line-base":         "--line-body",
	"--font-size-meta":    "",
	"--font-size-body":    "",
	"--font-size-title":   "",
	"--font-size-heading": "",
}

// currentRoles is TokenSchema's key set, built once. TokenSchema is a
// package-level literal that nothing mutates at runtime, so a sync.Once
// is enough and no invalidation path is needed.
var (
	currentRolesOnce sync.Once
	currentRoles     map[string]struct{}
)

// IsCurrentRole reports whether `key` is a role the current dictionary
// declares — i.e. whether writing it will actually paint something.
func IsCurrentRole(key string) bool {
	currentRolesOnce.Do(func() {
		keys := TokenSchemaKeys()
		currentRoles = make(map[string]struct{}, len(keys))
		for _, k := range keys {
			currentRoles[k] = struct{}{}
		}
	})
	_, ok := currentRoles[key]
	return ok
}

// ClassifyRoles returns one RoleIssue per key of `vars` that the current
// dictionary does not declare, sorted by key.
//
// An EMPTY result means every supplied key is a live role — that is the
// only reading, and it is why callers must branch on `len(...) > 0`
// rather than on some separate "ok" flag they could forget to set.
//
// Sorted because the result is rendered into a message an agent reads
// and, on the write path, into a refusal it may compare between runs;
// Go's map iteration order would make both non-reproducible.
func ClassifyRoles(vars map[string]string) []RoleIssue {
	var out []RoleIssue
	for key := range vars {
		if IsCurrentRole(key) {
			continue
		}
		if successor, retired := RetiredRoles[key]; retired {
			out = append(out, RoleIssue{Key: key, Status: RoleRetired, Successor: successor})
			continue
		}
		out = append(out, RoleIssue{Key: key, Status: RoleUnknown})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// FormatRoleIssues renders `issues` as one human-readable clause for the
// message field of a tool error or warning:
//
//	--bg-hover (retired, use --elev), --nope (unknown)
//
// The successor is spelled out inline rather than only in the structured
// `roles` array because the message is the part an agent's operator
// reads in a transcript, and "retired" without a replacement is advice
// that stops one step short of being useful.
func FormatRoleIssues(issues []RoleIssue) string {
	parts := make([]string, 0, len(issues))
	for _, it := range issues {
		switch {
		case it.Status == RoleRetired && it.Successor != "":
			parts = append(parts, fmt.Sprintf("%s (retired, use %s)", it.Key, it.Successor))
		case it.Status == RoleRetired:
			parts = append(parts, fmt.Sprintf("%s (retired, no replacement)", it.Key))
		default:
			parts = append(parts, fmt.Sprintf("%s (unknown)", it.Key))
		}
	}
	return strings.Join(parts, ", ")
}
