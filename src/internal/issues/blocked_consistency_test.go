package issues

import (
	"sort"
	"strings"
	"testing"
)

// sk-421.1 T001 (INV-10, INV-11, SC-004, SC-016, SC-017) — on the issue
// detail «blocked» is true if and only if the blockers list of the same
// response is non-empty, and the list holds exactly the blockers the flag
// counts. Test-plan rows 4–5. Fixture: blockers_visibility_test.go.
//
// sk-423 changed WHOM the flag counts — only blockers inside the reader's
// horizon (seededBlocker.counts) — and with it SC-004: the answer is no
// longer the same for every reader, it is the same for every reader with
// the same horizon. SC-017 and INV-11 hold unchanged.
//
// Limit (row 4): in a static graph deriving the flag from the list and the
// old separate EXISTS give the same value, so these tests do not by
// themselves prove the flag is derived from the list; the concurrent half
// of INV-11 is a review item.

func assertFlagMatchesList(t *testing.T, dto *IssueDetail, seeded []seededBlocker, visible func(seededBlocker) bool, wantHidden int) {
	t.Helper()
	want := expectedOrder(seeded, visible)
	if dto.IsBlocked != (len(dto.Blockers) > 0) {
		t.Errorf("IsBlocked = %v with %d blockers — flag and list disagree (INV-11)",
			dto.IsBlocked, len(dto.Blockers))
	}
	got := entryIDs(dto.Blockers)
	gotSorted := append([]string(nil), got...)
	sort.Strings(gotSorted)
	if strings.Join(gotSorted, ",") != strings.Join(want, ",") {
		t.Errorf("blocker ids = %v; want exactly the counted set %v (INV-10)", got, want)
	}
	if dto.UnavailableBlockingLinks != wantHidden {
		t.Errorf("UnavailableBlockingLinks = %d; want %d", dto.UnavailableBlockingLinks, wantHidden)
	}
}

// visibleTo is the horizon of one of the four readers over the fixture.
func visibleTo(r reader) func(seededBlocker) bool {
	return func(s seededBlocker) bool { return !s.inQ || seesQ(r) }
}

// Row 4 — SC-016: the state catalogue, read by a member of P who is not a
// member of Q.
func TestBlockedConsistency_StateCatalogue(t *testing.T) {
	cases := []struct {
		name  string
		seeds []blockerSeed
	}{
		{name: "no edges"},
		{
			name: "only closed and standing targets",
			seeds: []blockerSeed{
				{n: 2, status: StatusClosed, depType: "blocks"},
				{n: 3, status: StatusStanding, depType: "waits-for"},
				{inQ: true, n: 4, status: StatusClosed, depType: "blocks"},
				{inQ: true, n: 5, status: StatusStanding, depType: "blocks"},
			},
		},
		{
			name: "visible open, in_progress and deferred via blocks and waits-for",
			seeds: []blockerSeed{
				{n: 2, status: StatusOpen, depType: "blocks"},
				{n: 3, status: StatusInProgress, depType: "waits-for"},
				{n: 4, status: StatusDeferred, depType: "blocks"},
				{n: 5, status: StatusOpen, depType: "waits-for"},
				{n: 6, status: StatusInProgress, depType: "blocks"},
				{n: 7, status: StatusDeferred, depType: "waits-for"},
			},
		},
		{
			name: "hidden",
			seeds: []blockerSeed{
				{inQ: true, n: 2, status: StatusOpen, depType: "blocks"},
				{inQ: true, n: 3, status: StatusDeferred, depType: "waits-for"},
			},
		},
		{
			name: "mixed",
			seeds: []blockerSeed{
				{n: 2, status: StatusInProgress, depType: "blocks"},
				{inQ: true, n: 3, status: StatusOpen, depType: "waits-for"},
				{n: 4, status: StatusClosed, depType: "blocks"},
				{inQ: true, n: 5, status: StatusStanding, depType: "blocks"},
			},
		},
		{
			name: "related and parent-child to an open issue do not block",
			seeds: []blockerSeed{
				{n: 2, status: StatusOpen, depType: "related"},
				{n: 3, status: StatusOpen, depType: "parent-child"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newHorizonFixture(t)
			seeded := f.seed(t, tc.seeds...)
			dto := f.detail(t, f.ctxFor(t, readerMemberP))
			assertFlagMatchesList(t, dto, seeded, visibleTo(readerMemberP), hiddenFor(readerMemberP, seeded, visibleTo(readerMemberP)))
			if len(tc.seeds) > 0 && len(expectedOrder(seeded, visibleTo(readerMemberP))) == 0 && dto.IsBlocked {
				t.Errorf("IsBlocked = true with no counted blocker")
			}
		})
	}
}

// Row 5 — SC-017 (guard of INV-10) and SC-004 as sk-423 restated it: every
// reader × every seeding configuration. The expectation is computed by ONE
// rule (seededBlocker.counts over the reader's horizon). SC-017: for each
// reader the flag is the list's non-emptiness and the list is the counted
// set. SC-004: readers with the same horizon get the same answer.
func TestBlockedConsistency_ReaderByConfiguration(t *testing.T) {
	configs := []struct {
		name  string
		seeds []blockerSeed
	}{
		{
			name: "only in P",
			seeds: []blockerSeed{
				{n: 2, status: StatusOpen, depType: "blocks"},
				{n: 3, status: StatusDeferred, depType: "waits-for"},
				{n: 4, status: StatusClosed, depType: "blocks"},
			},
		},
		{
			name: "only in invisible Q",
			seeds: []blockerSeed{
				{inQ: true, n: 2, status: StatusInProgress, depType: "blocks"},
				{inQ: true, n: 3, status: StatusOpen, depType: "waits-for"},
				{inQ: true, n: 4, status: StatusStanding, depType: "blocks"},
			},
		},
		{
			name: "in P and in Q",
			seeds: []blockerSeed{
				{n: 2, status: StatusOpen, depType: "blocks"},
				{inQ: true, n: 3, status: StatusDeferred, depType: "blocks"},
				{n: 4, status: StatusStanding, depType: "waits-for"},
				{inQ: true, n: 5, status: StatusOpen, depType: "waits-for"},
			},
		},
	}

	for _, cfg := range configs {
		t.Run(cfg.name, func(t *testing.T) {
			f := newHorizonFixture(t)
			seeded := f.seed(t, cfg.seeds...)

			answers := make(map[reader]*IssueDetail, len(allReaders))
			for _, r := range allReaders {
				dto := f.detail(t, f.ctxFor(t, r))
				answers[r] = dto
				t.Run("SC-017 "+string(r), func(t *testing.T) {
					assertFlagMatchesList(t, dto, seeded, visibleTo(r), hiddenFor(r, seeded, visibleTo(r)))
				})
			}

			t.Run("SC-004 same answer for readers with the same horizon", func(t *testing.T) {
				for _, pair := range [][2]reader{
					{readerMemberP, readerExternalP}, // P only
					{readerMemberBoth, readerAdmin},  // P and Q
				} {
					// The hidden-link count is not part of the horizon: the
					// external collaborator gets none (INV-28 (4)), it is
					// asserted per reader above.
					a, b := answers[pair[0]], answers[pair[1]]
					sameHidden := a.UnavailableBlockingLinks == b.UnavailableBlockingLinks || pair[1] == readerExternalP
					if strings.Join(entryIDs(a.Blockers), ",") != strings.Join(entryIDs(b.Blockers), ",") ||
						a.IsBlocked != b.IsBlocked || !sameHidden {
						t.Errorf("%s: %v blocked=%v hidden=%d; %s: %v blocked=%v hidden=%d",
							pair[0], entryIDs(a.Blockers), a.IsBlocked, a.UnavailableBlockingLinks,
							pair[1], entryIDs(b.Blockers), b.IsBlocked, b.UnavailableBlockingLinks)
					}
				}
			})
		})
	}
}
