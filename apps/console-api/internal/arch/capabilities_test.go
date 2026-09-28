package arch

import (
	"slices"
	"sort"
	"strings"
	"testing"
)

// capability is one demonstration that a rule in imports_test.go can refuse
// the import it describes.
//
// The dependency rule is checked in one direction today: the table is run over
// the module as it stands and the assertion is that nothing fires. That is the
// assertion that matters for the tree, and it is also an assertion that cannot
// tell enforcement from decoration — a rule whose forbidden prefix is misspelt,
// or whose allow-list accidentally covers every package in the module, or whose
// match is inverted, produces exactly the same green as a rule that works. The
// cases below run the same matcher over a graph built to break exactly one
// boundary and assert it reports exactly that boundary: one violation, named,
// with the importing package in it.
//
// Each case is one rule, so the fields are chosen rather than arbitrary:
//
//   - imports is a concrete path under the forbidden prefix — the rule matches
//     prefixes, and a case that stopped at the prefix alone would not prove the
//     match covers the package somebody would actually import.
//   - permitted names a package the rule's own allow-list covers. Importing
//     from it must produce nothing, so a rule that has quietly stopped covering
//     what it says it covers fails here rather than passing as a stricter rule.
//   - refused names a package the rule must not cover, chosen where possible to
//     be the crossing this boundary exists for — an inbound adapter importing
//     the cross-plane client, and the reverse — rather than any package that
//     merely happens to be outside the list. It must produce exactly one
//     violation and not two: a case that tripped a second rule would be proving
//     something about a boundary it does not name.
//
// A SIDE MAY BE EMPTY, and the check at the bottom of this file holds that to
// the rule's own shape rather than to a habit. Two rules in this module have
// an empty side each, and neither is the blanket prohibition the shape was
// written for:
//
//   - a rule that allows NOBODY has no permitted side. Asserting one would be
//     asserting something the rule does not say, and the synthetic graph for it
//     would import the prefix from a package the rule refuses, which is a test
//     of the matcher rather than of the rule.
//   - a CARVE-OUT — a rule whose refused set is contained in a broader rule's,
//     because it exists to subtract from it — has no refused side it can be
//     shown to own. Every package it excludes is one the broader rule excludes
//     too, so no single graph names it as the only broken boundary. The
//     analytics grammar's exemption is the one, and it is given a SECOND case in
//     the narrow form to carry the violation, with the first carrying the
//     permission.
//
// So the shape rule is: a rule that allows somebody must have a case that
// names one of the people, and a rule that is not a carve-out must have a case
// that refuses. A rule exempt from the second half is named in the list
// TestEveryRuleHasACapability holds, which is the edit a future rule makes.
type capability struct {
	// prefix is the rule's forbidden import prefix, with the module path folded
	// back to `<module>` — the same spelling the roster pin uses, so the
	// completeness check at the bottom of this file can compare the two lists
	// without a second vocabulary for the same thing.
	prefix string
	// imports is a concrete import path below prefix, appended to it. An empty
	// string means the prefix is already a complete import path.
	imports string
	// permitted is a package directory, relative to the module root, that the
	// rule allows to make the import. Empty when the rule allows nobody, and
	// empty for a case written only to show a refusal — the shape check below
	// holds both emptinesses to the rule rather than to this sentence.
	permitted string
	// refused is a package directory the rule does not allow to make it. Empty
	// only for a carve-out, whose exclusions a broader rule already refuses.
	refused string
	// narrow runs the case against the rule table with the carve-out's ABSORBING
	// rule removed, so this rule is the only one that can fire and a violation
	// it reports is its own. It is set on the second case of a carve-out, whose
	// first case would otherwise be counting the pair.
	narrow bool
}

// capabilities is the table, one case per forbidden prefix in the table in
// imports_test.go. The two tables are held together by
// TestEveryRuleHasACapability below: the rules are the declaration and this is
// the proof, and a rule added without a case fails that check rather than
// shipping as a boundary nobody has watched refuse anything.
var capabilities = []capability{
	{
		prefix: "github.com/valkey-io/valkey-go",
		// A cache client reached straight from the application, which is what
		// the cache port exists to make impossible.
		permitted: "internal/adapters/outbound/valkey",
		refused:   "internal/application",
	},
	{
		prefix: "net/http",
		// A second HTTP surface in this module: the transport imported by a
		// package that is not a transport.
		permitted: "internal/adapters/inbound/http",
		refused:   "internal/application",
	},
	{
		prefix: "database/sql",
		// A query outside the persistence port, which would make the port
		// decorative rather than load-bearing.
		permitted: "internal/adapters/outbound/valkey",
		refused:   "internal/application",
	},
	{
		prefix:  "<module>/internal/ports",
		imports: "/outbound/persistence",
		// The direction a domain package must not take: `internal/domain` is
		// the package this rule is written for, and it does not exist yet,
		// which is the point — the rule governs it the day it appears.
		permitted: "internal/adapters/outbound/valkey",
		refused:   "internal/domain",
	},
	{
		prefix: "<module>/internal/config",
		// The environment read from somewhere other than the composition root.
		permitted: "cmd/console-api",
		refused:   "internal/application",
	},
	{
		prefix:  "<module>/internal/adapters/inbound/",
		imports: "http",
		// Outbound reaching into the inbound tree: an adapter that answers
		// requests as a side effect of being called.
		//
		// The permitted side is the composition root and not the inbound package
		// the import lives in, because that is the rule: an adapter is entered by
		// the package that constructs it and by nothing else, which now includes
		// another adapter in its own tree.
		permitted: "cmd/console-api",
		refused:   "internal/adapters/outbound/dataplane",
	},
	{
		prefix:  "<module>/internal/adapters/outbound/",
		imports: "dataplane",
		// Inbound reaching into the outbound tree — the crossing that builds
		// and works, and that puts this application's one cross-plane call
		// behind a route instead of behind the port.
		permitted: "cmd/console-api",
		refused:   "internal/adapters/inbound/http",
	},
	{
		prefix: "<module>/internal/application",
		// An adapter that knows the use-case, which is the dependency arrow
		// pointing the wrong way.
		permitted: "internal/adapters/inbound/http",
		refused:   "internal/adapters/outbound/valkey",
	},
	{
		prefix:  "<module>/internal/domain",
		imports: "/identity",
		// The session surface reaching for the domain's own types — the move
		// that would put ownership aggregates, and the credential grammar
		// whose one secret-bearing form is the digest, directly behind an
		// HTTP route. The inbound adapter renders its own wire shapes; it
		// does not speak the domain.
		//
		// The inbound adapter is ALLOWED the domain tree by this rule and refused
		// every grammar in it by the rule below, so this case cannot use the
		// adapter as its refused side: a capability case is refused by exactly the
		// rule it names. What the pair is for is stated on the two allow-lists —
		// this one hands the transport the whole tree so that the rule below can
		// be a statement about one grammar rather than about a tree with a hole
		// in it.
		//
		// The refused side is therefore a package the rule refuses on its own
		// terms: the configuration reader, which is the composition root's
		// concern and would be a use case's if it named a grammar.
		permitted: "internal/application",
		refused:   "internal/config",
	},
	{
		prefix:  "<module>/internal/domain/analytics",
		imports: "",
		// The usage envelope's answer named by the one package that renders it,
		// which is the exemption this rule exists for.
		permitted: "internal/adapters/inbound/http",
		// This rule has NO refused side, and a case must not invent one. Its
		// teeth are the packages it does not allow, every one of which the rule
		// above also refuses — so a graph naming that package names two broken
		// boundaries, and a case that counted violations cannot tell which rule
		// fired. The direction this case can isolate is the one below the
		// assertion: the renderer's name is permitted, and the allowance around
		// it is pinned by literal in TestTheRuleRosterIsTheDeclaredOne, which is
		// where a widened allow-list fails.
		//
		// The refused side of the CARVE-OUT is pinned in the narrow form below,
		// where one boundary is broken instead of two.
		refused: "",
	},
	{
		prefix:  "<module>/internal/domain/analytics",
		imports: "",
		// The narrow form: this rule ALONE. The graph is the rule table with the
		// rule this one subtracts from REMOVED, which is what a subtraction is
		// defined against, and which leaves a rule that refuses on its own. The
		// suite checks that this case still passes when the whole-tree rule is
		// deleted — that is what distinguishes "this rule fires" from "these two
		// rules together fire", and without that check the exemption could be
		// carried entirely by the rule it is an exemption from.
		permitted: "",
		// A configuration reader naming the usage grammar: an environment-driven
		// report parameter is a caller choosing an account, which is the one
		// thing this grammar is built not to allow.
		refused: "internal/config",
		narrow:  true,
	},
}

// TestEveryRuleRefusesWhatItDescribes is the rule-capability test: for each
// case above, the same matcher the module is run through reports nothing for
// the permitted package and exactly one named violation for the refused one.
//
// "Exactly one" rather than "at least one" is deliberate. Two violations in a
// synthetic graph of a single package with a single import means the case
// stopped isolating the rule it names, and a case that fires for the wrong
// reason keeps passing after the rule it was written for is deleted.
//
// A case may refuse NOTHING, and the analytics carve-out is the one that does:
// its exemption is a hole cut in a broader refusal, so every package it
// excludes is a package the broader rule excludes too, and no single graph can
// name it as the only broken boundary. The shape is still checked — see
// TestEveryRuleHasACapability, which holds an empty refused side to a rule that
// has no allowance of its own to spare — and a second case in the narrow form
// supplies the violation.
func TestEveryRuleRefusesWhatItDescribes(t *testing.T) {
	self := modulePath(t)

	// The rule the narrow form subtracts, and the one the exemption check below
	// holds the carve-out against. Declared here, in the test that uses it, so
	// the two places that name it cannot disagree about which rule is which.
	const narrowKey = "<module>/internal/domain"

	for _, c := range capabilities {
		t.Run(strings.ReplaceAll(c.prefix, "<module>/", ""), func(t *testing.T) {
			imported := strings.Replace(c.prefix, "<module>", self, 1) + c.imports

			// A rule that allows nobody has no permitted side to check, and
			// asserting one would be asserting something the rule does not say.
			// The narrow form runs against the table without the rule it
			// subtracts from, so the two cases of a carve-out are matched the way
			// their own comments say: the first against the whole table, the
			// second against the table minus the whole-tree rule.
			run := violations
			if c.narrow {
				run = func(self string, graph map[string][]string) []string {
					return violationsOf(withoutRule(rules(self), self, narrowKey), self, graph)
				}
			}

			if c.permitted != "" {
				clean := run(self, map[string][]string{c.permitted: {imported}})
				if len(clean) != 0 {
					t.Errorf("%s imports %s and the matcher reported:\n%s\nthat package is one the rule for %s says may make this import, so the allow-list has drifted from the case", c.permitted, imported, strings.Join(clean, "\n"), c.prefix)
				}
			}

			// A rule with no refused side of its own asserts nothing here, and
			// TestEveryRuleHasACapability is what refuses to let a case take this
			// exit unless the rule it names really has nothing to spare.
			if c.refused == "" {
				return
			}

			broken := run(self, map[string][]string{c.refused: {imported}})
			if len(broken) != 1 {
				t.Fatalf("%s imports %s and the matcher reported %d violations, want exactly 1:\n%s\nthe rule for %s has to refuse this import from this package, and nothing else may fire alongside it", c.refused, imported, len(broken), strings.Join(broken, "\n"), c.prefix)
			}
		})
	}
}

// TestEveryRuleHasACapability is the completeness check that keeps the two
// tables in step, and it is the same shape as the roster pin next door: the
// pin stops a rule being deleted, and this stops a rule being added without a
// demonstration that it works. Without it the capability table would go stale
// in the one direction that reads as success — a new boundary would be
// enforced by the suite and never once exercised.
//
// It checks the shape of each case as well as its presence. A blanket
// prohibition allows nobody, so a case claiming a permitted side for one would
// be a case asserting something the rule does not say; and a rule with an
// allow-list has to have a permitted side, or the case would never demonstrate
// that the rule distinguishes the package it permits from the package it
// refuses.
//
// The refused side is held to the same standard, in both directions. A rule
// with an allow-list must have a case that refuses SOMETHING, or the boundary
// could be deleted and the suite would still be green — which is the whole
// failure this table exists to prevent, and the one direction the two checks
// above cannot catch on their own. The one rule exempt from it is the
// analytics carve-out, which is exempt because it is a hole cut in a broader
// refusal rather than a refusal of its own: its refused set is a subset of the
// whole-tree rule's, so a second case in the narrow form supplies the violation.
// An exemption is spelled out here rather than inferred, because the inference
// is the kind that makes a test quietly stop testing.
func TestEveryRuleHasACapability(t *testing.T) {
	self := modulePath(t)

	// The rules whose refusal cannot be isolated, paired with the rule that
	// already refuses everything they exclude. A name in this list is a claim
	// about the relation between two rules, and the assertion below is what
	// holds it: every package the carve-out excludes must be one the absorbing
	// rule excludes, which is what makes "no graph names this as the only
	// broken boundary" a fact about the table rather than an assumption.
	carved := map[string]string{
		"<module>/internal/domain/analytics": "<module>/internal/domain",
	}

	// allowancesByPrefix is the rule's allow-list keyed by its forbidden prefix.
	allowances := map[string][]string{}
	declared := []string{}
	for _, r := range rules(self) {
		for _, prefix := range r.forbidden {
			key := strings.Replace(prefix, self, "<module>", 1)
			declared = append(declared, key)
			allowances[key] = r.allowed
		}
	}
	sort.Strings(declared)

	for carvedRule, absorber := range carved {
		excluded, absorbing := allowances[carvedRule], allowances[absorber]
		if absorbing == nil {
			t.Errorf("the carve-out %s names %s as the rule that absorbs its exclusions, and no such rule is in the table", carvedRule, absorber)
			continue
		}
		for _, prefix := range excluded {
			if !allowedToImport(prefix, absorbing) {
				t.Errorf("the carve-out %s excludes %s, which %s also allows — so a package it refuses IS a package nothing refuses, and the case below that leaves the refused side empty is hiding a boundary rather than naming one", carvedRule, prefix, absorber)
			}
		}
	}

	demonstrated := make([]string, 0, len(capabilities))
	permittedSomewhere := map[string]bool{}
	for _, c := range capabilities {
		// One entry per rule, not per case: the two cases for the analytics
		// carve-out are one demonstration of one boundary in two forms, and
		// a duplicate entry here would read as two rules needing cases and
		// make the comparison against the declared list a subtraction that
		// happens to balance.
		if !slices.Contains(demonstrated, c.prefix) {
			demonstrated = append(demonstrated, c.prefix)
		}
		if c.permitted != "" {
			permittedSomewhere[c.prefix] = true
		}
	}
	sort.Strings(demonstrated)

	// A rule that allows somebody is shown permitting, somewhere in the table:
	// not on this case but on one of the rule's own, which is why this is
	// collected rather than asserted in the loop above. A rule that allows
	// nobody is the shape that has no permitted side at all.
	for _, prefix := range declared {
		if len(allowances[prefix]) == 0 {
			continue
		}
		if !permittedSomewhere[prefix] {
			t.Errorf("no case shows %s permitting anything, so nothing has demonstrated that the rule still covers the packages it says it covers — a rule with an allowance and no permitted side is decoration the suite would report as enforcement", prefix)
		}
	}

	if !slices.Equal(demonstrated, declared) {
		t.Errorf("the capability table demonstrates %v, want %v — every forbidden prefix in the dependency rule needs a case proving it can fire, and a case for a rule that no longer exists proves nothing", demonstrated, declared)
	}

	// And the other half of the claim above, checked rather than asserted: every
	// rule that is not carved out has at least one case that names a package it
	// must refuse. Without this a case could be edited to an empty refused side
	// and the rule it was written for would stop being demonstrated, with every
	// other check in this file still green.
	refusedSomewhere := map[string]bool{}
	for _, c := range capabilities {
		if c.refused != "" {
			refusedSomewhere[c.prefix] = true
		}
	}
	for _, prefix := range declared {
		_, isCarveOut := carved[prefix]
		if !refusedSomewhere[prefix] && !isCarveOut {
			t.Errorf("no case refuses anything for %s, so the rule has never been seen to fire — a rule whose every case only proves what it permits is decoration the suite would report as enforcement", prefix)
		}
	}
}
