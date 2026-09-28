package application

import (
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The paging and cursor law, in the one package that decides it.
//
// ResolvePage, DecodeCursor and PageOf are the three functions every one of
// the ten lists routes through, and until this file existed none of them had a
// test. That is the worst possible coverage shape for this package: the
// functions are shared by all ten, so a defect in any of them is ten defects,
// and the reads that would have caught it are integration tests this module
// does not run on every change.
//
// Every case below is a DELETION. A limit outside the bounds is removed, a
// cursor is edited, a filter is changed, a filter is dropped — and the question
// is whether the surface refuses or quietly serves a page the caller did not
// ask for. "Quietly" is the failure, and it is the failure a test asserting
// only the happy path cannot see: a clamped limit and a served page look
// identical to the caller until the row it lost is the row it needed.

// TestALimitOutsideTheBoundsIsRefusedAndNotClamped is the law the contract
// states in as many words: "a value below minimum, above maximum, or not an
// integer at all is refused with 400 invalid_request… Nothing is clamped. A
// caller that asked for a larger page than this operation will return has asked
// a question the contract does not answer."
//
// Zero sits in the middle of that sentence's cases and is the one a guard
// written from the prose gets wrong, which is why it has its own test below:
// for a LIMIT, zero is the contract's default and for a BOUNDS check it is
// below the minimum. Which reading is correct is decided by whether the
// parameter was supplied at all, and the function's only evidence of that is
// the number. So the bounds that are checked are the bounds minus the one
// value the contract has already given a meaning to — and the assertion is
// written as a relationship, not as literals, so that adding a third special
// value is a test failure rather than a silently unenforced branch.
//
// The clamp is the tempting implementation and it is invisible: asked for 500
// and given 200, a caller cannot tell a capped page from a small collection,
// and a screen that renders "no more" stops paging at 200 with 500 rows behind
// it. So the refusal is asserted three ways — the error, the RESOLVED page
// size, and the absence of any value the bounds could have produced — because
// a clamp that returned an error alongside a clamped size would satisfy the
// first and neither of the others.
func TestALimitOutsideTheBoundsIsRefusedAndNotClamped(t *testing.T) {
	tests := []struct {
		name       string
		limit      int
		wantRefuse bool
	}{
		{name: "the floor is honoured", limit: persistence.MinPageLimit},
		{name: "the ceiling is honoured", limit: persistence.MaxPageLimit},
		{name: "in the middle is honoured", limit: 25},

		{name: "one past the ceiling", limit: persistence.MaxPageLimit + 1, wantRefuse: true},
		{name: "far above the ceiling", limit: 1_000_000, wantRefuse: true},
		{name: "a negative page size", limit: -1, wantRefuse: true},
		{name: "far below the floor", limit: -1000, wantRefuse: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertRefusedLimit(t, tt.limit, tt.wantRefuse)
		})
	}
}

// assertRefusedLimit is the shared body, and it is a helper rather than two
// loops because the honoured case and the refused case differ in what they can
// assert: a honoured limit has to come back UNCHANGED, and a refused one has to
// come back with nothing at all.
func assertRefusedLimit(t *testing.T, limit int, wantRefuse bool) {
	t.Helper()
	after, resolved, err := ResolvePage(collectionUsers, "", limit, Fingerprint(collectionUsers, nil))

	if !wantRefuse {
		if err != nil {
			t.Fatalf("limit %d was refused: %v", limit, err)
		}
		if resolved != limit {
			t.Errorf("limit %d resolved to %d, want the caller's own value", limit, resolved)
		}
		return
	}

	if err == nil {
		t.Fatalf("limit %d was accepted and resolved to %d; a page the caller did not ask for is one it cannot tell apart from a page the collection capped itself",
			limit, resolved)
	}
	assertInvalidRequest(t, err)
	// Nothing a clamp could have returned: an error carries no position and no
	// page size, so a caller acting on either is acting on a zero value the
	// contract never promised.
	if after != "" || resolved != 0 {
		t.Errorf("a refused page returned after=%q resolved=%d, want no position at all", after, resolved)
	}
}

// TestAnAbsentLimitIsTheDefaultAndZeroIsNotAnAskForNoRows is the one case where
// a reasonable-looking guard would be wrong, and it is separated for that
// reason.
//
// Zero is both "the caller sent nothing" and a number the bounds would refuse.
// The contract says it is the default, and the implementation is explicit about
// why: a caller who wanted no rows has asked a question the surface does not
// answer, and the default is a far more likely reading of a missing parameter
// than an empty page. Getting this wrong in the other direction — treating zero
// as out of range — would break every unpaged request on the surface.
func TestAnAbsentLimitIsTheDefaultAndZeroIsNotAnAskForNoRows(t *testing.T) {
	after, resolved, err := ResolvePage(collectionUsers, "", 0, Fingerprint(collectionUsers, nil))
	if err != nil {
		t.Fatalf("an absent limit was refused: %v", err)
	}
	if resolved != persistence.DefaultPageLimit {
		t.Errorf("an absent limit resolved to %d, want the documented default %d", resolved, persistence.DefaultPageLimit)
	}
	if after != "" {
		t.Errorf("an absent limit with no cursor resolved after=%q, want the first row", after)
	}
}

// TestTheBoundsAreTheContractsAndTheDefaultIsInsideThem is a test about the
// constants, and it earns its place because they are the surface's promise to
// callers who generated a client from the contract.
//
// A default outside the bounds would mean every unpaged request on the surface
// is answered with a page the contract would refuse a caller for asking, and a
// ceiling below the floor would make the range empty. Neither is the kind of
// thing a per-list test notices, and both are a one-line edit away.
func TestTheBoundsAreTheContractsAndTheDefaultIsInsideThem(t *testing.T) {
	if persistence.MinPageLimit < 1 {
		t.Errorf("MinPageLimit = %d, want at least 1; a page of no rows is not a page", persistence.MinPageLimit)
	}
	if persistence.MaxPageLimit <= persistence.MinPageLimit {
		t.Errorf("MaxPageLimit = %d and MinPageLimit = %d, want a range with room in it; an empty range refuses every limit including the default",
			persistence.MaxPageLimit, persistence.MinPageLimit)
	}
	if persistence.DefaultPageLimit < persistence.MinPageLimit || persistence.DefaultPageLimit > persistence.MaxPageLimit {
		t.Errorf("DefaultPageLimit = %d, outside the bounds [%d, %d]: every unpaged request would be answered with a page the contract refuses",
			persistence.DefaultPageLimit, persistence.MinPageLimit, persistence.MaxPageLimit)
	}
}

// TestACursorIsRefusedWhenItCannotBePlaced is the cursor half, and the three
// refusals are three ways of asking for a page the caller did not name: a
// cursor from another collection, a cursor minted under different filters, and
// a cursor this process did not mint.
//
// All three are invalid_request and not not_found. That distinction is load
// bearing — a 404 would tell a caller their cursor expired when what actually
// happened is that they changed a filter mid-walk, which is their own doing and
// is fixed by starting the walk again — and it is the distinction a client
// branching on the code is there to make.
func TestACursorIsRefusedWhenItCannotBePlaced(t *testing.T) {
	users := Fingerprint(collectionUsers, map[string]string{"state": "active"})
	keys := Fingerprint(collectionAPIKeys, nil)

	t.Run("a cursor earned on another collection", func(t *testing.T) {
		cursor := EncodeCursor(collectionUsers, "user-2", users)
		_, _, err := ResolvePage(collectionAPIKeys, cursor, 0, keys)
		if err == nil {
			t.Fatal("a cursor from another collection was served; the position means nothing in a different order")
		}
		assertInvalidRequest(t, err)
	})

	t.Run("a cursor minted under different filters", func(t *testing.T) {
		cursor := EncodeCursor(collectionUsers, "user-2", users)
		_, _, err := ResolvePage(collectionUsers, cursor, 0, Fingerprint(collectionUsers, map[string]string{"state": "removed"}))
		if err == nil {
			t.Fatal("a cursor minted under one filter was served under another; a keyset's meaning is defined relative to the rows it orders")
		}
		assertInvalidRequest(t, err)
	})

	t.Run("a cursor minted with a filter this request dropped", func(t *testing.T) {
		// The asymmetry is the point. A cursor minted under `state=active` names
		// a position in a subset's order, and an unfiltered request asks a
		// broader question: resuming inside the subset from that position would
		// skip rows the caller's own previous page never saw. So it is refused.
		cursor := EncodeCursor(collectionUsers, "user-2", users)
		_, _, err := ResolvePage(collectionUsers, cursor, 0, Fingerprint(collectionUsers, nil))
		if err == nil {
			t.Fatal("a filtered cursor was served against an unfiltered request")
		}
		assertInvalidRequest(t, err)
	})

	t.Run("an empty filter and an absent one are the same request", func(t *testing.T) {
		// The inverse of the case above, and the one that is easy to get wrong
		// in the direction of refusing valid requests: a console that always
		// sends `state=` would mint a cursor with an empty filter and then be
		// unable to resume with it, and the symptom is "paging silently does
		// nothing" rather than an error anyone reads.
		withEmpty := Fingerprint(collectionUsers, map[string]string{"state": ""})
		withAbsent := Fingerprint(collectionUsers, nil)
		cursor := EncodeCursor(collectionUsers, "user-2", withEmpty)
		if _, _, err := ResolvePage(collectionUsers, cursor, 0, withAbsent); err != nil {
			t.Errorf("a cursor minted with an empty-valued filter was refused against the same filter absent: %v", err)
		}
	})

	t.Run("a cursor this process did not mint", func(t *testing.T) {
		// The signature is the whole of the forgery defence, and these are the
		// shapes a caller actually reaches for: a base64 payload with the right
		// shape, a payload with a tampered position, and something arbitrary.
		for _, forged := range []string{
			"eyJwb3NpdGlvbiI6MSwia2V5IjoiYXV0aWF0YTowMDAwMDAwMC0wMDAwLTAwMDAtMDAwMC0wMDAwLTAwMDAwMDAwMDAwMCJ9",
			"not-a-cursor",
			"eyJ4IjoxfQ",
			"====",
		} {
			if _, _, err := ResolvePage(collectionUsers, forged, 0, users); err == nil {
				t.Errorf("a forged cursor %q was served", forged)
			}
		}
	})

	t.Run("a cursor this process minted is served", func(t *testing.T) {
		// The other direction, because a decoder that refused everything would
		// pass every case above. Paging is a product feature; a cursor that
		// cannot be resumed is a list of fifty rows and no way past them.
		cursor := EncodeCursor(collectionUsers, "user-2", users)
		after, _, err := ResolvePage(collectionUsers, cursor, 0, users)
		if err != nil {
			t.Fatalf("a cursor this process minted was refused: %v", err)
		}
		if after != "user-2" {
			t.Errorf("after = %q, want the position inside the cursor", after)
		}
	})
}

// TestPageOfDerivesHasMoreFromTheProbeRowAndNeverFromAShortPage is the whole of
// PageOf, and it is the rule a client is most likely to get wrong by
// inference.
//
// A page shorter than the limit is NOT the end of the collection — a concurrent
// write is the ordinary reason for one — so has_more comes from the extra row
// the adapters ask for and from nothing else. A caller that stops on a short
// page silently truncates a list whenever anything is being written, which is
// precisely when someone is reading it.
// TestEveryPageCarriesACursorAndHasMoreIsTheOnlyEndOfCollection is the
// property the Cursor schema makes structural — minLength: 1, on a member the
// PageEnvelope lists as required — and which no test in this package covered
// until the read transport started asserting page shapes.
//
// The three facts are separate and each is a rule of its own. `has_more` is
// the only end-of-collection signal, so a short page and a full page both have
// to agree with the probe row. `next_cursor` is the position of the last row
// THIS page carried, and it is non-empty even when there is nowhere to go,
// because an empty position is indistinguishable from the one before the first
// row: a console holding it would resume from the beginning and re-render the
// same fifty rows forever. And the contract's bounds apply to the value on the
// wire, not only to the value sent — a generated client types minLength, so an
// empty next_cursor is a schema violation and a validation failure in front of
// every screen.
func TestEveryPageCarriesACursorAndHasMoreIsTheOnlyEndOfCollection(t *testing.T) {
	list := "test.list"
	filters := Fingerprint(list, nil)
	ident := func(s string) string { return s }

	t.Run("a full page with no probe row is the end", func(t *testing.T) {
		items, hasMore, next := PageOf([]string{"a", "b", "c"}, 3, ident, list, filters)
		if hasMore {
			t.Error("has_more = true for a page the probe row did not extend; a full page is the end of the collection")
		}
		if len(items) != 3 {
			t.Errorf("items = %v, want all three rows kept", items)
		}
		assertAPlaceableCursor(t, list, filters, next)
		if after := cursorAfter(t, next, list, filters); after != "c" {
			t.Errorf("the cursor names position %q, want the last row this page carried", after)
		}
	})

	t.Run("a short page with no probe row is the end", func(t *testing.T) {
		// Two rows where three were asked for, and no probe: the collection is
		// exhausted. The reverse of this is the bug — treating a short page as
		// "there may be more" forever leaves a console paging until it stops
		// getting rows for a reason that has nothing to do with the data.
		items, hasMore, next := PageOf([]string{"a", "b"}, 3, ident, list, filters)
		if hasMore {
			t.Error("has_more = true with no probe row; a short page is the end when the probe did not extend it")
		}
		if len(items) != 2 {
			t.Errorf("items = %v, want the two rows the collection had", items)
		}
		assertAPlaceableCursor(t, list, filters, next)
		if after := cursorAfter(t, next, list, filters); after != "b" {
			t.Errorf("the cursor names position %q, want the last row this page carried", after)
		}
	})

	t.Run("a probe row past the limit means more, and is dropped", func(t *testing.T) {
		// The probe is asked for and then discarded: items holds AT MOST the
		// limit the caller asked for. Handing the extra row back would let a
		// caller render limit+1 rows while believing it asked for limit.
		items, hasMore, next := PageOf([]string{"a", "b", "c"}, 2, ident, list, filters)
		if !hasMore {
			t.Error("has_more = false with a probe row present; continuing is worth it")
		}
		if len(items) != 2 {
			t.Errorf("items = %v, want exactly the two the caller asked for; the probe row is has_more, not a row", items)
		}
		if after := cursorAfter(t, next, list, filters); after != "b" {
			t.Errorf("the cursor names position %q, want %q: a cursor at the probe would skip a row, so that row appears on no page at all",
				after, "b")
		}
	})

	t.Run("a walk resumed on the cursor returns every row exactly once", func(t *testing.T) {
		// The property the three assertions above are components of, and the
		// one a client actually experiences. It lives here rather than being
		// derived from those three because they assert POSITIONS and this
		// asserts VISITED ROWS: a page-boundary bug loses a row, and a position
		// test cannot see a lost row because the cursor and the page it resumed
		// are wrong together.
		//
		// The rows are the limit-plus-one a port is asked for at each step, and
		// the expectation is the one the PERSISTENCE port is already held to
		// elsewhere: has_more is the last row's reach, never the page's length.
		// The final page here is deliberately short, so a client that inferred
		// the end from the count — the mistake the field exists to prevent —
		// would stop with the final page's has_more contradicting it. And each
		// fetch really is resumed on the previous page's cursor, which is the
		// only thing that makes this a walk rather than three independent calls:
		// the rows below are keyed by the position the walk has reached, so a
		// cursor that named the wrong row would return a row twice and a cursor
		// that named the probe's row would drop one.
		collection := []string{"k1", "k2", "k3", "k4", "k5", "k6", "k7"}
		// pageSize+1 rows from the first row strictly after `after`, or from the
		// beginning of the collection when there is no cursor yet.
		fetch := func(after string) []string {
			start := 0
			if after != "" {
				for i, key := range collection {
					if key == after {
						start = i + 1
						break
					}
				}
			}
			end := start + 3
			if end > len(collection) {
				end = len(collection)
			}
			return collection[start:end]
		}

		seen := map[string]int{}
		after := ""
		for pageNumber := 0; pageNumber < 10; pageNumber++ {
			items, _, next := PageOf(fetch(after), 2, ident, list, filters)
			for _, row := range items {
				seen[row]++
			}
			after = cursorAfter(t, next, list, filters)
			if len(items) == 0 {
				break
			}
			if pageNumber == 9 {
				t.Fatal("the walk did not terminate within ten pages of a seven-row collection")
			}
		}
		if after != "" {
			t.Errorf("the walk ended at %q, want the empty position of a walk with nothing left to read", after)
		}
		for row, count := range seen {
			if count != 1 {
				t.Errorf("row %q was returned %d times, want once: a cursor is the only thing keeping the pages apart", row, count)
			}
		}
		if len(seen) != len(collection) {
			t.Errorf("the walk returned %d distinct rows, want %d: a row skipped at one boundary is lost for good",
				len(seen), len(collection))
		}
	})

	t.Run("an empty page still carries a placeable cursor", func(t *testing.T) {
		// An account with no users is a legitimate answer, not an error, and it
		// is the case where an empty position is most tempting. It is exactly
		// the one that must not happen: the empty position is the one before the
		// first row, so a client that stored it would re-read the whole
		// collection on every request and never advance.
		items, hasMore, next := PageOf[string](nil, 2, ident, list, filters)
		if items == nil {
			t.Error("items = nil; the contract's `items` is an array, and null is not one")
		}
		if len(items) != 0 || hasMore {
			t.Errorf("items = %v has_more = %v, want an empty page and no continuation", items, hasMore)
		}
		assertAPlaceableCursor(t, list, filters, next)
		if after := cursorAfter(t, next, list, filters); after != "" {
			t.Errorf("the cursor names position %q, want the beginning of the collection — there was no row to name", after)
		}
	})

	t.Run("a cursor only means something in its own collection and filters", func(t *testing.T) {
		// PageOf mints the cursor and ResolvePage checks it, and the check is
		// the only thing standing between a caller and a page from another walk.
		// Asserting it here rather than trusting it is what makes the two
		// functions one contract.
		_, _, next := PageOf([]string{"a", "b", "c"}, 2, ident, list, filters)
		if _, _, err := ResolvePage(list, next, 2, filters); err != nil {
			t.Errorf("the cursor PageOf minted was refused by ResolvePage for the same list and filters: %v", err)
		}
		if _, _, err := ResolvePage("another.list", next, 2, filters); err == nil {
			t.Error("a cursor from one collection was accepted by another")
		}
	})
}

// assertAPlaceableCursor asserts the two things a next_cursor must be at once:
// present, because the Cursor schema has minLength 1 and the envelope requires
// the member, and placeable, because a value this surface cannot resume from is
// worse than no value at all — a client would store it and find out on the next
// request.
func assertAPlaceableCursor(t *testing.T, list string, filters map[string]string, cursor string) {
	t.Helper()
	if cursor == "" {
		t.Fatal("next_cursor is empty; the contract's Cursor has minLength 1 and an empty position is the one before the first row, so a client holding it re-reads from the beginning forever")
	}
	if _, _, err := ResolvePage(list, cursor, 0, filters); err != nil {
		t.Fatalf("next_cursor %q is not a cursor this surface can place: %v", cursor, err)
	}
}

// cursorAfter reads back the position a minted cursor names, which is the only
// way a test can see inside a value the contract says is not safe to interpret —
// in the same package that mints them, which is where the reading belongs.
func cursorAfter(t *testing.T, cursor, list string, filters map[string]string) string {
	t.Helper()
	after, err := DecodeCursor(list, cursor, filters)
	if err != nil {
		t.Fatalf("the cursor %q cannot be read back: %v", cursor, err)
	}
	return after
}

// TestEveryListSharesOnePagingLaw walks the nine lists rather than the one, and
// it exists because the alternative is nine tests that all say the same thing
// and one of them is wrong.
//
// A list that resolved its own cursor differently from its eight siblings is
// the exact failure ResolvePage's own comment says it exists to prevent, and it
// is invisible from a single list: the other eight keep passing, and a console
// learns one paging behaviour and applies it to nine screens.
func TestEveryListSharesOnePagingLaw(t *testing.T) {
	collections := []string{
		collectionUsers, collectionAPIKeys, collectionPlans,
		collectionSubscriptions, collectionEntitlements, collectionBuckets,
		collectionLedger, collectionFindings, collectionRuns,
	}

	for _, list := range collections {
		t.Run(list, func(t *testing.T) {
			filters := Fingerprint(list, nil)

			// The ceiling is honoured, the default is the default, and the
			// bounds are refused — on every list, from the same constants.
			if _, resolved, err := ResolvePage(list, "", persistence.MaxPageLimit, filters); err != nil || resolved != persistence.MaxPageLimit {
				t.Errorf("the ceiling was not honoured: resolved=%d err=%v", resolved, err)
			}
			if _, resolved, err := ResolvePage(list, "", 0, filters); err != nil || resolved != persistence.DefaultPageLimit {
				t.Errorf("the default was not honoured: resolved=%d err=%v", resolved, err)
			}
			if _, _, err := ResolvePage(list, "", persistence.MaxPageLimit+1, filters); err == nil {
				t.Error("an out-of-range limit was accepted")
			} else {
				assertInvalidRequest(t, err)
			}

			// And a cursor travels between two of them and is refused, so no
			// list can be walked with another's position.
			cursor := EncodeCursor(list, "k1", filters)
			if _, _, err := ResolvePage(list, cursor, 0, filters); err != nil {
				t.Errorf("a cursor this list minted was refused by this list: %v", err)
			}
			for _, other := range collections {
				if other == list {
					continue
				}
				if _, _, err := ResolvePage(other, cursor, 0, Fingerprint(other, nil)); err == nil {
					t.Errorf("%s accepted a cursor minted by %s", other, list)
				}
			}
		})
	}
}

// assertInvalidRequest is the category assertion every refusal above shares. It
// is a helper rather than three repeated blocks because the DISTINCTION is what
// is being asserted: a refusal that were not_found would be a different bug, and
// a refusal that were an internal error would be the one this file's sibling
// transport tests exist to catch.
func assertInvalidRequest(t *testing.T, err error) {
	t.Helper()
	asApplication, ok := As(err)
	if !ok {
		t.Fatalf("the refusal %v is not an application error, so no transport can map it to a status", err)
	}
	if asApplication.Code != CodeInvalidRequest {
		t.Errorf("the refusal is %q, want %q: a cursor or a page size the caller changed is the caller's doing, not a missing resource and not a server fault",
			asApplication.Code, CodeInvalidRequest)
	}
	if asApplication.Message == "" {
		t.Error("the refusal carries no message; the contract promises the caller which of its three inputs was not accepted")
	}
}
