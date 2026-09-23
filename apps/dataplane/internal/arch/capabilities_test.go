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
// Each case is one rule, so the three fields are chosen rather than arbitrary:
//
//   - imports is a concrete path under the forbidden prefix — the rule matches
//     prefixes, and a case that stopped at the prefix alone would not prove the
//     match covers the package somebody would actually import.
//   - permitted names a package the rule's own allow-list covers. Importing
//     from it must produce nothing, so a rule that has quietly stopped covering
//     what it says it covers fails here rather than passing as a stricter rule.
//     It is empty for a rule that allows nobody, and the check at the bottom of
//     this file holds that emptiness to the rule's own shape.
//   - refused names a package the rule must not cover, chosen where possible to
//     be the crossing this boundary exists for — `internal/adapters/outbound`
//     importing an inbound adapter, and the reverse — rather than any package
//     that merely happens to be outside the list. It must produce exactly one
//     violation and not two: a case that tripped a second rule would be proving
//     something about a boundary it does not name.
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
	// rule allows to make the import. Empty when the rule allows nobody.
	permitted string
	// refused is a package directory the rule does not allow to make it.
	refused string
}

// capabilities is the table, one case per rule in the table in
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
		// A query on the request path with no port to bound it.
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
		permitted: "cmd/dataplane",
		refused:   "internal/application",
	},
	{
		prefix:  "<module>/internal/adapters/inbound/",
		imports: "http",
		// Outbound reaching into the inbound tree: the direction that was
		// permitted before the adapter rule was split in two.
		permitted: "internal/adapters/inbound/management",
		refused:   "internal/adapters/outbound/valkey",
	},
	{
		prefix:  "<module>/internal/adapters/outbound/",
		imports: "valkey",
		// Inbound reaching into the outbound tree — the other direction of the
		// same crossing, and the one a route is most likely to make: a handler
		// that constructs its own infrastructure instead of taking a port.
		permitted: "internal/adapters/outbound/usagefacts",
		refused:   "internal/adapters/inbound/http",
	},
	{
		prefix: "<module>/internal/application",
		// An adapter that knows the use-case, which is the dependency arrow
		// pointing the wrong way.
		permitted: "internal/adapters/inbound/http",
		refused:   "internal/adapters/outbound/valkey",
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
func TestEveryRuleRefusesWhatItDescribes(t *testing.T) {
	self := modulePath(t)

	for _, c := range capabilities {
		t.Run(strings.ReplaceAll(c.prefix, "<module>/", ""), func(t *testing.T) {
			imported := strings.Replace(c.prefix, "<module>", self, 1) + c.imports

			// A rule that allows nobody has no permitted side to check, and
			// asserting one would be asserting something the rule does not say.
			if c.permitted != "" {
				clean := violations(self, map[string][]string{c.permitted: {imported}})
				if len(clean) != 0 {
					t.Errorf("%s imports %s and the matcher reported:\n%s\nthat package is one the rule for %s says may make this import, so the allow-list has drifted from the case", c.permitted, imported, strings.Join(clean, "\n"), c.prefix)
				}
			}

			broken := violations(self, map[string][]string{c.refused: {imported}})
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
func TestEveryRuleHasACapability(t *testing.T) {
	self := modulePath(t)

	declared := []string{}
	blanket := map[string]bool{}
	for _, r := range rules(self) {
		for _, prefix := range r.forbidden {
			key := strings.Replace(prefix, self, "<module>", 1)
			declared = append(declared, key)
			blanket[key] = len(r.allowed) == 0
		}
	}
	sort.Strings(declared)

	demonstrated := make([]string, 0, len(capabilities))
	for _, c := range capabilities {
		demonstrated = append(demonstrated, c.prefix)
		if blanket[c.prefix] != (c.permitted == "") {
			t.Errorf("the case for %s permits %q, and the rule allows %v — a blanket prohibition has no permitted side, and a rule with an allow-list needs one for its case to show the rule discriminates", c.prefix, c.permitted, !blanket[c.prefix])
		}
	}
	sort.Strings(demonstrated)

	if !slices.Equal(demonstrated, declared) {
		t.Errorf("the capability table demonstrates %v, want %v — every forbidden prefix in the dependency rule needs a case proving it can fire, and a case for a rule that no longer exists proves nothing", demonstrated, declared)
	}
}
