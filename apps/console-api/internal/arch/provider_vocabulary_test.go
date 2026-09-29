package arch

import (
	"fmt"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The provider vocabulary the core must not spell.
//
// These are the names ONE provider's wire format uses, and the rule is that the
// domain and the application do not contain them. The reason is not tidiness: a
// domain that names a provider's event types has taken a dependency on that
// provider's protocol, and a protocol is a thing that changes under you. The
// adapter is where that knowledge belongs, and the day a second provider is
// implemented the core has to compile unchanged against a vocabulary it never
// heard. A string literal in a `switch` is a dependency just as much as an
// import, which is why this is a source scan and not an import check — the
// import rule in imports_test.go cannot see a literal.
//
// The scan is over COMMENTS AND LITERALS ALIKE rather than over identifiers,
// and that is the harder half of the rule. Prose is where a leak actually
// happens: a comment explaining why a cumulative figure converges can perfectly
// well be written in terms of the provider's own field name, and reading it six
// months later while that field is renamed, the comment is worse than nothing
// because it looks like a statement about this code. The explanations here are
// worth keeping; they are written in this build's own words instead, and this
// test is what keeps them that way.
//
// The list is ONE provider's, and replacing the adapter means replacing the
// list: the superseded provider's names were deleted rather than kept
// alongside. A word list that accumulated every protocol this build had ever
// spoken would fail on a comment recording that the swap happened — the one
// place an old provider's name genuinely belongs — and a rule that forbids its
// own rationale is a rule the next person deletes. What this list asserts is
// that the core does not depend on the protocol in front of it, and there is
// one such protocol at a time.
//
// Every entry is spelled the way the provider spells it, punctuation and case
// included, and that is what keeps the rule from firing on this repository's
// own vocabulary. Two entries are worth explaining for that reason:
//
//   - `bank-accounts` keeps its hyphen, because the provider's path segment has
//     one and this build has no reason to write those two words joined.
//   - `order_code` keeps its underscore. The bare word "order" is ordinary
//     English and appears throughout this build's prose about the ORDER of its
//     writes — which is the doctrine most of the payment comments exist to
//     state — so the rule names the provider's spelling rather than a word the
//     core needs to keep saying.
//
// `SePay` and the two header names overlap, a header name containing the
// provider name, so one offending line is reported twice, once per entry. That
// is not a false positive: both entries do occur there, and a contributor is
// told the one line to edit either way.
var providerVocabulary = []string{
	"X-SePay-Signature",
	"X-SePay-Timestamp",
	"SePay",
	"subAccount",
	"transferType",
	"transferAmount",
	"va_number",
	"va_holder_name",
	"va_prefix",
	"qr_code_url",
	"with_qrcode",
	"qrcode_template",
	"bank-accounts",
	"order_code",
}

// corePackages are the two directories the provider protocol must stay out of.
// The adapter that owns the protocol, the port it implements and the composition
// root that wires it are all exempt by omission rather than by list, so a new
// provider adapter needs no edit here.
var corePackages = []string{
	"internal/domain/payments",
	"internal/application",
}

const providerVocabularyWhy = "the provider's wire vocabulary belongs to the adapter that speaks it; name this build's own concept here instead, so a second provider can be implemented without the core compiling against the first one's protocol"

// TestTheCoreDoesNotSpellTheProvidersProtocol refuses the provider's own names
// anywhere in the domain or the application.
//
// A comment counts, and the diagnostic says so, because a comment written in
// the provider's terms is a second place its protocol has to be correct. The
// alternative — exempting comments — is not available to this test and should
// not be: it would make the boundary a rule about strings the compiler sees,
// which is a rule a contributor could satisfy by rewording a comment and think
// they had satisfied the boundary.
func TestTheCoreDoesNotSpellTheProvidersProtocol(t *testing.T) {
	fset := token.NewFileSet()
	sources := parseGoSources(t, fset)

	scanned := 0
	violations := []string{}
	for _, source := range sources {
		if !isCorePackage(source.dir) {
			continue
		}
		scanned++
		for _, word := range providerVocabulary {
			for _, position := range positionsMentioning(t, source, word) {
				violations = append(violations, fmt.Sprintf("%s: mentions %q: %s", position, word, providerVocabularyWhy))
			}
		}
	}

	if scanned == 0 {
		t.Fatalf("the source scan reached none of %v; the provider-vocabulary rule would pass without inspecting the code it governs", corePackages)
	}
	if len(violations) > 0 {
		t.Fatalf("the core spells the provider's protocol in %d places:\n  %s", len(violations), strings.Join(violations, "\n  "))
	}
}

// isCorePackage reports whether a module-relative directory is one this rule
// governs. It matches the directory itself and everything beneath it, so a new
// package inside the domain is covered without an edit.
func isCorePackage(dir string) bool {
	for _, core := range corePackages {
		if dir == core || strings.HasPrefix(dir, core+"/") {
			return true
		}
	}
	return false
}

// positionsMentioning returns a printable "path:line" for every place a word
// occurs in a source file's text.
//
// It scans the file's BYTES rather than its syntax tree, which is what makes a
// comment count and is the reason this test is here at all: the tree is exactly
// the view in which a provider's event name is invisible. Line numbers are
// derived from counting newlines up to the match, so a diagnostic points at the
// line a contributor has to edit.
func positionsMentioning(t *testing.T, source parsedGoFile, word string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(moduleRoot, source.path))
	if err != nil {
		t.Fatalf("reading %s: %v", source.path, err)
	}
	positions := []string{}
	for offset := 0; ; {
		found := strings.Index(string(raw[offset:]), word)
		if found < 0 {
			break
		}
		at := offset + found
		line := 1 + strings.Count(string(raw[:at]), "\n")
		positions = append(positions, fmt.Sprintf("%s:%d", source.path, line))
		offset = at + len(word)
	}
	return positions
}
