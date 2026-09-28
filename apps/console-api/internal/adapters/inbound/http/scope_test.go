package http

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The tests below are about one property and no other: a presented credential
// either resolves to the account the deployment configured for it, or it
// resolves to nothing at all — and nothing about WHICH of those two happened
// reaches the caller, and nothing about the presentation reaches anywhere
// except the answer.
//
// The last half of that sentence is the part a test has to work for. The
// resolver's own claim is that the comparison runs to completion whatever the
// outcome, so that the number of comparisons is a function of the deployment's
// TABLE rather than of the token presented. A resolver that returned from inside
// its loop on the first match would still answer every question correctly, and
// it would have turned that count into a function of the outcome — the same
// timing channel the constant-time compare beside it exists to close. So what
// the tests below assert is never "it took the same time": a duration is not a
// property of the code on a machine with a scheduler, and a test that compared
// two of them would be red for reasons no reader could act on.

// scopeAccount and scopeOtherAccount are the two accounts this file's tables
// map tokens to, named so a case that is about tenancy reads as one. They are
// UUID-form because the identifier this plane mints is, and a table of
// identifiers no deployment could hold would be a fixture that says nothing.
const (
	scopeAccount      = "018f0000-0000-7000-8000-00000000000a"
	scopeOtherAccount = "018f0000-0000-7000-8000-00000000000b"
)

// mustScoper builds a resolver from a table and refuses to continue without one.
// A nil resolver would panic inside the next call rather than name the table
// that was wrong, which is a worse place to find out.
func mustScoper(t *testing.T, table map[string]string) *StaticScoper {
	t.Helper()
	resolver, err := NewStaticScoper(table)
	if err != nil {
		t.Fatalf("NewStaticScoper(%v) error = %v, want nil", table, err)
	}
	return resolver
}

// TestTheStaticScopeTableRefusesToResolveNothing is the constructor's own list,
// and each entry is a deployment that must fail to start rather than start with
// a credential that resolves to an account nobody owns.
//
// The empty table is first because it is the most consequential: a resolver over
// nothing is a surface that serves nobody's figures and cannot say whose they
// are, which is access control in the shape of an outage. The empty account is
// the same defect reached from the other end — a scope that is not an account
// authorizes nothing while looking as though it authorizes something.
func TestTheStaticScopeTableRefusesToResolveNothing(t *testing.T) {
	tests := []struct {
		name    string
		table   map[string]string
		wantSay string
	}{
		{
			name:    "no table at all",
			table:   nil,
			wantSay: "empty",
		},
		{
			name:    "a table with no entries",
			table:   map[string]string{},
			wantSay: "empty",
		},
		{
			// An empty credential resolves to every request that carries no
			// credential at all, so a resolver that accepted one would be a
			// resolver whose scope is the absence of a credential.
			name:    "a token that is the empty string",
			table:   map[string]string{"": scopeAccount},
			wantSay: "empty token",
		},
		{
			name:    "a token mapped to no account",
			table:   map[string]string{"console-token": ""},
			wantSay: "empty account",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver, err := NewStaticScoper(tt.table)

			if err == nil {
				t.Fatalf("NewStaticScoper(%v) = a resolver, want a refusal; a deployment that cannot name a caller must not start serving one", tt.table)
			}
			if resolver != nil {
				t.Errorf("NewStaticScoper(%v) returned a resolver alongside its error; a refused table leaves nothing to serve a request with", tt.table)
			}
			if !strings.Contains(err.Error(), tt.wantSay) {
				t.Errorf("the refusal %q does not say %q; an operator reading it has to know which of the table's entries was wrong", err.Error(), tt.wantSay)
			}
		})
	}
}

// TestTheStaticScoperResolvesTheAccountTheDeploymentConfigured is the ordinary
// case, over a table of three rather than one so it also says that resolution
// is a lookup rather than a constant.
//
// The table is a map because that is what the deployment supplies, and Go
// randomises map iteration on purpose — so the candidate order differs between
// runs and between the tokens here. A resolver that depended on that order would
// answer correctly three times and wrong the fourth, which is exactly the kind
// of defect a table of one entry can never notice.
func TestTheStaticScoperResolvesTheAccountTheDeploymentConfigured(t *testing.T) {
	table := map[string]string{
		"console-token-one":   scopeAccount,
		"console-token-two":   scopeOtherAccount,
		"console-token-three": scopeAccount,
	}
	resolver := mustScoper(t, table)

	for token, want := range table {
		t.Run(token, func(t *testing.T) {
			scope, err := resolver.ScopeOf(context.Background(), token)

			if err != nil {
				t.Fatalf("ScopeOf(%q) error = %v, want nil", token, err)
			}
			if scope != persistence.ScopeFor(want) {
				t.Errorf("ScopeOf(%q) resolved to %q, want %q", token, scope.AccountID, want)
			}
		})
	}
}

// TestTheStaticScopeResolvesNothingForATokenItWasNotGiven is the refusal, and
// the three presentations are the three reasons a deployment might not answer
// one: an empty credential, a credential this deployment never issued, and a
// credential far longer than any token in its table.
//
// They all answer identically, and that is the property rather than an accident
// of the fixture: a caller learns only what it already knows — this token does
// not work — and learns it identically for every token that does not work. One
// sentinel for every reason is what makes that true, so a handler that mapped a
// different status to any of them would have built an oracle over the credential
// space out of a convenience.
func TestTheStaticScopeResolvesNothingForATokenItWasNotGiven(t *testing.T) {
	resolver := mustScoper(t, map[string]string{"console-token": scopeAccount})

	for _, presented := range []string{"", "a-token-this-deployment-did-not-issue", strings.Repeat("x", 4096)} {
		t.Run(shortTokenName(presented), func(t *testing.T) {
			scope, err := resolver.ScopeOf(context.Background(), presented)

			if !errors.Is(err, ErrUnresolvedCredential) {
				t.Fatalf("ScopeOf(%q) error = %v, want the one refusal %v", presented, err, ErrUnresolvedCredential)
			}
			// The refusal names nothing, and nothing leaks through the scope
			// returned beside it: a zero scope is not a default account, and a
			// caller must not be able to read one out of a failed resolution
			// either.
			if scope.AccountID != "" {
				t.Errorf("ScopeOf(%q) refused with a scope naming %q; the refusal carries no account, because a caller who could read one out of a failed resolution could read somebody else's figures",
					presented, scope.AccountID)
			}
		})
	}
}

// TestTheStaticScopeAnswersTheSameAccountWhateverOrderTheTableHolds is the
// observable half of the timing claim: the answer is a function of the token,
// never of where the deployment happened to configure it.
//
// The table holds two tokens — one belonging to another account, one to this
// one — and the same table is built and walked thirty-two times, because the
// candidate order is not the fixture's to choose: the deployment supplies a map
// and Go randomises its iteration, so each round is a different configuration
// of that pair. Every one of them must resolve to the same account.
//
// That is what makes this a check rather than a demonstration. A loop that
// returned the account it found on the first candidate would pass two rounds in
// three and fail the rest, and which two is not a thing anyone can predict from
// reading the test — only from running it, or from having been the one who wrote
// the loop.
//
// It is deliberately a weaker claim than "every candidate is compared", and the
// stronger one is the case below: this one can see a resolver that picks the
// wrong entry from the ones it compared, and this one cannot see a resolver that
// compares fewer of them. Between them they cover both halves.
func TestTheStaticScopeAnswersTheSameAccountWhateverOrderTheTableHolds(t *testing.T) {
	presented := "console-token-match"

	// The same two configured tokens over and over: one belonging to another
	// account, one to this one. Every configuration of that pair that a map can
	// produce must answer the same way.
	table := map[string]string{
		"console-token-other": scopeOtherAccount,
		"console-token-match": scopeAccount,
	}

	for round := range 32 {
		resolver := mustScoper(t, table)
		scope, err := resolver.ScopeOf(context.Background(), presented)

		if err != nil {
			t.Fatalf("round %d: ScopeOf(%q) error = %v, want nil", round, err, presented)
		}
		if scope != persistence.ScopeFor(scopeAccount) {
			t.Fatalf("round %d: ScopeOf(%q) resolved to %q, want %q; the answer cannot depend on which candidate the deployment's table happened to hold first",
				round, presented, scope.AccountID, scopeAccount)
		}
	}
}

// TestTheStaticScopeRefusesAnAmbiguousTableRatherThanPickingOne is the timing
// claim's structural half, and it is the one that reads the production loop.
//
// A loop that returns the moment it finds its match and a loop that walks to the
// end and counts are indistinguishable on every answer a deployment can
// actually produce: two configured tokens whose digests are equal are a SHA-256
// collision, so no table this constructor accepts has one. The difference shows
// only where two candidates match at once — and there the two loops disagree
// about the ANSWER, not merely about how long it took: the early-return form
// resolves to whichever candidate it reached first, and the counting form
// refuses because it cannot say which one the credential was meant to be.
//
// So the state is built directly, in the struct the constructor fills, with two
// entries carrying one digest and two accounts. That is not a state any table
// can produce today, and saying so is the point: it is a state a REPLACEMENT
// backing store can — a deployment-supplied credential file read twice, a
// migration that left two rows behind, a store whose lookup is not a table at
// all — and a resolver that answers it by picking one has let its own backing
// store decide which of two credentials works. The refusal is the only answer
// that is not a guess.
//
// It is also the assertion that makes the traversal claim checkable without a
// clock. An early return in the production loop turns this case red, because the
// first match would be returned instead of both being counted; a loop that
// compared everything and then took the LAST match turns it red too, because two
// matches would resolve instead of refusing. Both mutations are edits this
// repository's other claims forbid, and neither of them could be caught by any
// test above.
//
// The state is read here rather than inferred: this file never reimplements the
// resolver's loop to watch it. A copy of the comparison would prove that THIS
// COPY visits every candidate, which is a fact about the test file rather than
// about the code a deployment runs.
func TestTheStaticScopeRefusesAnAmbiguousTableRatherThanPickingOne(t *testing.T) {
	digest := sha256.Sum256([]byte("console-token-match"))

	tests := []struct {
		name    string
		digests []scopedDigest
	}{
		{
			// The entry that matched is FIRST, so an implementation that
			// returned from inside its loop would answer with the first account
			// here — silently, and with the right shape.
			name: "the matching candidates are configured first",
			digests: []scopedDigest{
				{digest: digest, accountID: scopeAccount},
				{digest: digest, accountID: scopeOtherAccount},
				{digest: sha256.Sum256([]byte("console-token-absent")), accountID: scopeAccount},
			},
		},
		{
			// And the same table with the matching entries LAST, which is the
			// direction an early return fails in the other way: it never reaches
			// the ambiguity at all and answers as though the table held one of
			// them.
			name: "the matching candidates are configured last",
			digests: []scopedDigest{
				{digest: sha256.Sum256([]byte("console-token-absent")), accountID: scopeAccount},
				{digest: digest, accountID: scopeOtherAccount},
				{digest: digest, accountID: scopeAccount},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := &StaticScoper{digests: tt.digests}

			scope, err := resolver.ScopeOf(context.Background(), "console-token-match")

			if !errors.Is(err, ErrUnresolvedCredential) {
				t.Fatalf("ScopeOf() error = %v, want the one refusal %v; two candidates that match one credential are a table this plane cannot resolve, and answering with either of them would let the deployment's storage decide which of its own credentials works",
					err, ErrUnresolvedCredential)
			}
			if scope.AccountID != "" {
				t.Errorf("ScopeOf() refused with a scope naming %q; the refusal carries no account", scope.AccountID)
			}
		})
	}
}

// TestTheStaticScopeHoldsNoCredentialInPlainComparableForm is the constructor's
// last promise, and the only one a reviewer cannot check by reading ScopeOf.
//
// The tokens in this table ARE credentials — they are what a caller presents to
// read a customer's spend — so a resolver whose state held them would carry one
// into every heap dump, every panic dump and every log line that printed its own
// struct. The struct therefore holds digests, and this test asserts that twice:
// by value, against the digest the constructor is documented to compute, and by
// printing the struct the way a crash dump would and reading the token back out
// of it. The second is the one that survives a future field being added to the
// struct, which is the direction this property leaks in.
func TestTheStaticScopeHoldsNoCredentialInPlainComparableForm(t *testing.T) {
	const token = "console-token-the-struct-must-not-hold"
	resolver := mustScoper(t, map[string]string{token: scopeAccount})

	if len(resolver.digests) != 1 {
		t.Fatalf("the resolver holds %d entries, want 1", len(resolver.digests))
	}
	if want := sha256.Sum256([]byte(token)); resolver.digests[0].digest != want {
		t.Errorf("the entry holds %x, want the SHA-256 digest %x of the token it was configured with; a resolver that kept the credential in comparable form would carry it into every dump of its own state",
			resolver.digests[0].digest, want)
	}
	if dump := fmt.Sprintf("%+v", resolver); strings.Contains(dump, token) {
		t.Errorf("the resolver's own state printed as a crash dump would contain its configured credential: %s", dump)
	}
}

// TestTheStaticScopeIgnoresTheContextItWasCalledWith is a small claim about the
// port's shape rather than about its verdict: the resolution is a table lookup
// and nothing else, so it does not consult the context it arrived on.
//
// It is here because a resolver that DID read the context — for a deadline, for
// a cancellation, for anything — would be a resolver whose answer depended on
// how it was called, which is the same shape of dependence as one that depended
// on the order its table was configured in. The presented token's own deadlines
// are the use case's to apply, above this port.
func TestTheStaticScopeIgnoresTheContextItWasCalledWith(t *testing.T) {
	resolver := mustScoper(t, map[string]string{"console-token": scopeAccount})

	// A context that is already done: every operation on it fails at once.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	scope, err := resolver.ScopeOf(ctx, "console-token")

	if err != nil {
		t.Fatalf("ScopeOf() on a cancelled context error = %v, want nil; the resolution is a table lookup and the context is the use case's to apply", err)
	}
	if scope != persistence.ScopeFor(scopeAccount) {
		t.Errorf("ScopeOf() on a cancelled context resolved to %q, want %q", scope.AccountID, scopeAccount)
	}
}

// shortTokenName keeps a subtest's name a name: the 4KB presentation above is a
// case, not a sentence.
func shortTokenName(token string) string {
	switch {
	case token == "":
		return "an empty token"
	case len(token) > 32:
		return "a token far longer than this table's"
	default:
		return token
	}
}

// Compile-time proof that the resolver under test is the PORT the use case is
// written against, so a change to the port's shape fails here rather than in a
// case that quietly stopped standing in for it.
var _ persistence.Scoper = (*StaticScoper)(nil)
