package application

import "testing"

// The dashboard's lists are bounded displays, not pages, and this is the trim
// that keeps them the size their own bound claims.
//
// It exists because the repository fetches `limit + 1` rows on every read —
// the keyset's whole reason for existing is one row past the page to answer
// `has_more` without a second query. Every paged read hands that row to
// `PageOf`, which turns it into the flag. The overview is not a paged read:
// it has no cursor and no `next_cursor`, so it had no `PageOf` to call and
// the extra row went straight onto the response. An account with six recent
// subscriptions rendered six against a bound of five, and nothing caught it:
// `shared/console.yaml` declares the array with no `maxItems`, so the
// contract would not have either.
//
// The test is on the function rather than on the use case for a reason worth
// stating. Reaching the trim through `AccountOverview` means constructing all
// nine port doubles the dashboard fans out to, and the assertion would still
// be about this function — a fixture that large testing a two-line slice is a
// fixture that breaks for reasons that have nothing to do with what it claims
// to cover. What the use case owes is one call at each of the two sites, and
// that is a question about the file, which the comments there answer.
func TestTrimOverviewPageDropsTheRowPastTheBound(t *testing.T) {
	rows := []int{1, 2, 3, 4, 5, 6}
	got := trimOverviewPage(rows, overviewRecentSubscriptions)

	if len(got) != overviewRecentSubscriptions {
		t.Fatalf("trimmed to %d rows, want %d", len(got), overviewRecentSubscriptions)
	}
	// The rows that survive must be the FIRST ones, not an arbitrary five. A
	// dashboard that kept the wrong five would still pass a length assertion,
	// and the account would see recent subscriptions and not the recent ones.
	for i, want := range []int{1, 2, 3, 4, 5} {
		if got[i] != want {
			t.Fatalf("row %d is %d, want %d", i, got[i], want)
		}
	}
}

func TestTrimOverviewPageLeavesAShortPageAlone(t *testing.T) {
	// A page shorter than the bound is already the size it claims, and the trim
	// must not grow it or reorder it. An account with three subscriptions sees
	// three, and an empty one sees an empty slice rather than a nil.
	rows := []int{7, 8, 9}
	got := trimOverviewPage(rows, overviewRecentSubscriptions)

	if len(got) != 3 || got[0] != 7 || got[2] != 9 {
		t.Fatalf("a short page was altered: %v", got)
	}
}

func TestTrimOverviewPageOnNoRows(t *testing.T) {
	// The dashboard renders a PAYG list and a subscriptions list, and an
	// account with neither must get an empty one rather than a panic or a nil
	// that the wire layer has to special-case.
	if got := trimOverviewPage([]int{}, overviewRecentSubscriptions); len(got) != 0 {
		t.Fatalf("an empty page became %v", got)
	}
}
