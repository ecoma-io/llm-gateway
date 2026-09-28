package arch

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
)

// rule is one boundary of the dependency rule (ADR 0006 §6), stated as import
// prefixes that may appear only in the packages a selector allows.
//
// The shape is "allowed here, forbidden everywhere else" rather than
// "forbidden in package X" on purpose. The second form says nothing about a
// package that does not exist yet, and the three modules have directories the
// architecture has named and not yet filled — `internal/domain` is the clear
// one. Stated this way a rule governs a package the moment it appears, and
// every rule is evaluated against packages that exist today, so none of them
// is a rule over an empty set waiting to matter.
//
// An empty allow-list is a third form and it is deliberate: a blanket
// prohibition — nothing in this application may import this at all — which is
// the strongest statement the table can make and the right one for the
// application that owns no data.
type rule struct {
	// why is what the failure prints. It is the whole point of the rule: a
	// boundary nobody can explain is a boundary someone deletes.
	why string
	// allowed names the package directories, relative to the module root, that
	// may import what forbidden names. A package is allowed when it is one of
	// these or sits below one of them; an empty list allows nobody.
	allowed []string
	// forbidden names an import path, or a prefix of one.
	forbidden []string
}

// rules is the dependency rule, one line per boundary. Several are deliberately
// coarse: an allow-list naming a tree rather than the one package that imports
// the library today is a rule about a boundary instead of a rule about a
// moment.
//
// The two adapter rules are two lines rather than one. A single rule forbidding
// `self/internal/adapters/` and allowing `internal/adapters` says an adapter
// may import an adapter, which permits the one crossing this application has
// most reason to refuse: an inbound surface that constructs the cross-plane
// client itself, or the reverse — the management seam reached from a route
// instead of from the port that exists to make it substitutable. Stated as two
// rules, the direction between the trees is a decision rather than an
// oversight.
//
// Those two rules also allow the composition root and nobody else, so an
// adapter cannot import an adapter in its own tree either. That is a narrowing:
// the allow-list used to name the whole tree, which read as "siblings may
// compose" and was, in the module next door, a live crossing — one inbound
// adapter importing the other while the rule's own rationale forbade a second
// surface in the same sentence. An adapter is a leaf, and only the package that
// constructs it enters one. This module has a single inbound package and a
// single outbound one today, so the narrowing is latent here; it is stated
// rather than left for the day a second adapter arrives and finds the door
// open.
//
// An import that appears in no rule is governed by none of them; this table is
// therefore not a complete account of what this module may import, only of
// where its boundaries lie. Keeping it that way is the point — a rule that
// listed every permitted library would be a second dependency manifest, and it
// would be edited by everyone who wanted to add one.
func rules(self string) []rule {
	return []rule{
		{
			why:       "a cache client is reached through the cache port: the composition root constructs the adapter, and every other package sees the port",
			allowed:   []string{"internal/adapters/outbound/valkey"},
			forbidden: []string{"github.com/valkey-io/valkey-go"},
		},
		{
			why:       "HTTP is a transport concern: the composition root and an adapter may import it — inbound because that is how a request arrives, outbound because the cross-plane seam is a call to another application's management surface, which is transport and not state — while the application, the ports and any domain package may not, because an application that speaks HTTP is an application whose use-cases cannot be called any other way",
			allowed:   []string{"cmd", "internal/adapters/inbound", "internal/adapters/outbound"},
			forbidden: []string{"net/http"},
		},
		{
			why:       "SQL is reached through the persistence port, which names database/sql because it must express a transaction, and through the adapter that implements it — never from application code, which would make the port decorative",
			allowed:   []string{"cmd", "internal/ports", "internal/adapters/outbound"},
			forbidden: []string{"database/sql"},
		},
		{
			why:       "the port vocabulary is what the application is written against and what the adapters implement, so it points inward from both: a domain package that imported it would have taken a dependency on infrastructure's shape, and this module has only three packages with a reason to name a port — the application, the adapters and the composition root that chooses between them",
			allowed:   []string{"cmd", "internal/application", "internal/adapters", "internal/ports"},
			forbidden: []string{self + "/internal/ports"},
		},
		{
			why:       "configuration is read once, at the composition root: a package that reads the environment has behaviour its callers cannot see in its arguments and cannot vary in a test",
			allowed:   []string{"cmd"},
			forbidden: []string{self + "/internal/config"},
		},
		{
			why:       "an inbound adapter is entered by the composition root and by nothing else: another inbound adapter is a surface delegating to a surface — a second contract with nobody's name on it — and an outbound adapter that calls one has made the transport it implements part of the use case",
			allowed:   []string{"cmd"},
			forbidden: []string{self + "/internal/adapters/inbound/"},
		},
		{
			why:       "an outbound adapter is constructed at the composition root and by nothing else, and reached from the application through the port it implements: an inbound adapter that imported one has put concrete infrastructure behind a route with no port to substitute, which on this application is the cross-plane seam arriving as a call the use case cannot see",
			allowed:   []string{"cmd"},
			forbidden: []string{self + "/internal/adapters/outbound/"},
		},
		{
			why:       "the application is called by an inbound adapter and wired by the composition root; nothing outbound may depend on it, because an outbound adapter that knows the use-case has inverted the arrow",
			allowed:   []string{"cmd", "internal/adapters/inbound"},
			forbidden: []string{self + "/internal/application"},
		},
		{
			// The domain packages are the grammars — identity's ownership edges, the
			// money grammar's ledger and settlements and, since the fact consumer, the
			// feed grammar that derives from it. They are spoken by the use cases, by
			// the ports that persist them and by the outbound adapters that translate
			// them to storage and to the wire. A grammar may also speak another: the
			// feed grammar derives its effects from the money grammar's ids and
			// formula, an inward layering the dependency rule has nothing against.
			// What the rule guards is the arrow toward transport and infrastructure,
			// and that stays shut.
			//
			// The INBOUND ADAPTER is not in that list, and the reason is the one
			// this architecture has held since the first transport was written: an
			// inbound surface hands aggregates and verification material to the
			// TRANSPORT and renders its own response shapes. The one answer it is
			// allowed to render directly is the rule below, and it is a rule about a
			// single grammar precisely so that this one can stay a rule about the
			// whole tree — the pair reads as a tree with a single hole in it, and
			// each half is refused from the side that would widen it.
			why:       "the domain packages are the grammars, and the only packages that speak one are the use cases, the ports that persist them and the outbound adapters that translate them to storage and to the wire: an outbound adapter that reached into a grammar's rules would be translating its own arithmetic, and an inbound adapter that reached into one would be handing ownership aggregates, and the credential grammar whose one secret-bearing form is the digest, straight to the transport",
			allowed:   []string{"cmd", "internal/application", "internal/adapters/inbound", "internal/adapters/outbound", "internal/ports", "internal/domain"},
			forbidden: []string{self + "/internal/domain"},
		},
		{
			// The one exemption, and it is a single grammar rather than the tree the
			// rule above refuses, which is what keeps the pair from being a hole.
			//
			// The rule above's reason is that an inbound surface renders its own
			// wire shapes. An inbound adapter that writes a WIRE representation of a
			// domain type is doing exactly what that reason describes, and a usage
			// envelope whose instants, money units and availability enum were
			// marshalled by reflection would be three plausible-but-wrong
			// conversions of the one place they had to be right.
			//
			// So the adapter is allowed to NAME the analytics grammar — and
			// responsibility's shield is TestTheAnalyticsGrammarNamesNoTransport
			// below, which fails the moment the grammar itself grows a transport
			// import or a wire method. The name in an import is not permission for a
			// struct tag to decide a bucket's unit. The capability case beside the
			// rule above is the other half: the same adapter is refused every OTHER
			// grammar, so a route that grew an identity import would be caught by
			// the broad rule and a route that grew a second analytics import would
			// be caught by the narrow one.
			//
			// The allow-list is the ordinary outward-facing one — the composition
			// root, the use cases, the ports and the outbound adapters, exactly the
			// list the rule above grants them — so this rule takes the inbound tree
			// away and hands nothing back but the one grammar.
			//
			// Every other grammar stays out of the transport whatever the transport
			// is for, and a surface that needed one of them here would be a use case
			// wearing a route's clothes.
			why:       "the usage read model's answer is the one domain type an inbound surface renders rather than delegates: the wire shape is a translation of analytics.Usage and its fields, and a handler that received the application's own wire struct instead would have two envelopes in two packages. The shield is TestTheAnalyticsGrammarNamesNoTransport below — the grammar may not name the transport back, so a method that rendered an instant into a caller's zone cannot be added to hide a conversion here",
			allowed:   []string{"cmd", "internal/application", "internal/adapters/inbound", "internal/adapters/outbound", "internal/ports"},
			forbidden: []string{self + "/internal/domain/analytics"},
		},
	}
}

// TestTheDependencyRuleHolds checks every rule against every package: for
// each import in the module, the package either is allowed to make it or the
// rule is violated. All violations are reported rather than the first, so one
// run names every boundary that broke.
func TestTheDependencyRuleHolds(t *testing.T) {
	self := modulePath(t)
	graph := importGraph(t)

	// The scanner's own guard, repeated here because every assertion below is
	// vacuous if the graph came back empty — and "no violations found" is
	// exactly what an empty graph looks like.
	if len(graph) == 0 {
		t.Fatal("the import scan found no packages; every rule below would pass vacuously")
	}

	for _, violation := range violations(self, graph) {
		t.Error(violation)
	}
}

// violations runs the rule table over an import graph and returns one sentence
// per boundary broken, sorted so the order is the module's rather than the
// map's.
//
// It is a function rather than a loop inside the test above because two tests
// need it and they need it for opposite purposes: one runs it over the module
// as it is and asserts it returns nothing, and the one in capabilities_test.go
// runs it over graphs built to break exactly one boundary and asserts it
// returns exactly that. A matcher only ever exercised on a clean tree is a
// matcher nobody has seen work.
func violations(self string, graph map[string][]string) []string {
	return violationsOf(rules(self), self, graph)
}

// violationsOf is the matcher over a table the caller chooses, and the reason
// it is separate from the function above is one narrow case: a rule that is a
// CARVE-OUT — a hole cut in a broader refusal, as the analytics grammar's
// exemption is in the domain tree — cannot be shown to fire on a graph that
// also breaks the rule it subtracts from, because both fire at once and a case
// that counts violations cannot tell which was which. Running the table with
// the absorbing rule removed is what a subtraction is defined against, and it
// is the only honest way to hold such a rule to the same demonstration as every
// other one in the table.
func violationsOf(table []rule, self string, graph map[string][]string) []string {
	dirs := make([]string, 0, len(graph))
	for dir := range graph {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	found := []string{}
	for _, dir := range dirs {
		for _, r := range table {
			// A rule is a prohibition with a list of exemptions, and both halves
			// are ordinary. The exemption is not a skip folded into the loop
			// above: it is the allow-list, checked here, so a rule whose allow-list
			// covers a package nothing else forbids would be visible here as a
			// list whose every entry is a package that imports nothing — which is
			// what the capability table in capabilities_test.go proves for each
			// rule, from the other side.
			if allowedToImport(dir, r.allowed) {
				continue
			}
			for _, imported := range graph[dir] {
				for _, forbidden := range r.forbidden {
					if matches(imported, forbidden) {
						found = append(found, fmt.Sprintf("%s imports %s: %s", dir, imported, r.why))
					}
				}
			}
		}
	}
	sort.Strings(found)
	return found
}

// TestTheAnalyticsGrammarNamesNoTransport is the responsibility's shield for
// the one exemption in the table above: the inbound adapter may name the
// analytics grammar, and the grammar may name nothing inward of it.
//
// The shield is a list of imports rather than a list of forbidden ones, so a
// package that grows later cannot fail this by omission — anything the grammar
// names that is not on the list is a new edge, and a new edge is the change
// this test is asking a reader to look at. The list is the whole inward-facing
// vocabulary a grammar is allowed to have, spelled out:
//
//   - the standard library's time package, which is what a bucket bound IS, and
//   - the sibling grammars, which is the inward layering the rules above have
//     nothing against — the analytics grammar derives nothing from them today,
//     and the day it does, this is the line that says so out loud.
//
// What is NOT on the list is the transport, the ports, the application and
// json. `net/http` in particular is the failure this test exists to catch: a
// convenience method on a Point that formatted an instant would be the
// shortest path to a timezone bug inside the one rule that decides where a
// bucket is cut, and the reason it is attractive is that a struct tag is
// shorter than a hand-written marshaller.
//
// `testing` IS on the list, and it is here for a reason that is about the
// scanner rather than about the grammar. importGraph walks every .go file in
// the tree, test files included, because the walk keys a package by its
// DIRECTORY and a package that had a test file and then lost it would otherwise
// vanish from the graph rather than appear to have no tests. The cost of that
// choice is that a package's test imports are charged to its production
// vocabulary, and every package in this module with a test imports `testing` —
// so naming it is not a concession the grammar makes, it is a fact about every
// package under the walk. It is spelled out anyway rather than inferred, because
// a blanket exemption for the standard library would have allowed `net/http`
// along with it and that is the whole failure this test is for.
func TestTheAnalyticsGrammarNamesNoTransport(t *testing.T) {
	const grammar = "internal/domain/analytics"

	allowed := map[string]bool{
		"testing":      true,
		"time":         true,
		"errors":       true,
		"fmt":          true,
		"sort":         true,
		"strings":      true,
		"math":         true,
		"strconv":      true,
		"unicode":      true,
		"unicode/utf8": true,
		"text/scanner": true,
		"os":           true,
	}

	graph := importGraph(t)
	imports, scanned := graph[grammar]
	if !scanned {
		t.Fatalf("the import scan found no package at %s; every assertion here would pass vacuously", grammar)
	}

	// A grammar may speak another grammar. The list is stated as the set the
	// module's own grammars may name among themselves, and it is empty of
	// anything else — so a new edge inward is a deliberate edit here.
	siblings := map[string]bool{
		modulePath(t) + "/internal/domain/accounting": true,
		modulePath(t) + "/internal/domain/commerce":   true,
		modulePath(t) + "/internal/domain/identity":   true,
		modulePath(t) + "/internal/domain/ingestion":  true,
		modulePath(t) + "/internal/domain/projection": true,
	}

	for _, imported := range imports {
		standard := !strings.Contains(strings.SplitN(imported, "/", 2)[0], ".")
		if standard && allowed[imported] {
			continue
		}
		if siblings[imported] {
			continue
		}
		t.Errorf("%s imports %s: the grammar that decides where a bucket is cut names no transport, no port and no application — the wire shape is the adapter's rendering of these types, not something these types know about", grammar, imported)
	}
}

// withoutRule returns the table with the rule forbidding one prefix removed,
// which is how a carve-out is demonstrated on its own: a hole cut in a refusal
// and the refusal are not separately observable, so the refusal is taken away
// rather than counted around.
//
// The removal is by folded prefix rather than by identity because that is how
// the table is read everywhere else — the roster pin and the capability table
// both speak in `<module>` spellings — and a matcher that took a rule pointer
// would let a table reordered or copied keep the same rule and lose it here.
func withoutRule(table []rule, self, prefix string) []rule {
	kept := make([]rule, 0, len(table))
	for _, r := range table {
		if slices.ContainsFunc(r.forbidden, func(f string) bool { return strings.Replace(f, self, "<module>", 1) == prefix }) {
			continue
		}
		kept = append(kept, r)
	}
	return kept
}

// matches reports whether imported is forbidden itself, or a package below
// it. A forbidden prefix that ends in "/" has already drawn its own boundary —
// it names a tree, and anything starting with it is inside. Any other prefix
// names a module or a package, and a package below it continues with "/"
// (a subpackage) or "." (a vendored or dot-joined path); a sibling that merely
// shares the leading text — `github.com/jackc/pgx2` against
// `github.com/jackc/pgx` — must not count, because an import rule stated as a
// module name is a rule about that module and its packages, not about
// everything that begins with its spelling.
func matches(imported, forbidden string) bool {
	if strings.HasSuffix(forbidden, "/") {
		return strings.HasPrefix(imported, forbidden)
	}
	return imported == forbidden ||
		strings.HasPrefix(imported, forbidden+"/") ||
		strings.HasPrefix(imported, forbidden+".")
}

// allowedToImport reports whether a package directory is one an allow-list
// covers, directly or as a subdirectory.
func allowedToImport(dir string, allowed []string) bool {
	for _, prefix := range allowed {
		if under(dir, prefix) {
			return true
		}
	}
	return false
}

// TestEveryRuleGovernsSomething is the anti-vacuity check for the table
// itself, and it has two halves because the table has two forms.
//
// A rule whose allow-list names a path that is no package in this module is a
// rule about a package that was deleted, or a typo — in both cases it is
// silently doing nothing until someone adds the package it was written for,
// and the fix is an edit here. A blanket prohibition has no path to check, so
// what it must have instead is something to forbid: a rule with neither an
// allow-list nor a forbidden prefix would match nothing and read as
// enforcement.
func TestEveryRuleGovernsSomething(t *testing.T) {
	self := modulePath(t)
	found := map[string]bool{}
	for _, dir := range packageDirs(t) {
		found[dir] = true
	}

	for _, r := range rules(self) {
		if len(r.forbidden) == 0 {
			t.Errorf("a rule allows %v and forbids nothing; it can never fire", r.allowed)
			continue
		}
		for _, prefix := range r.allowed {
			matches := false
			for dir := range found {
				if under(dir, prefix) {
					matches = true
					break
				}
			}
			if !matches {
				t.Errorf("the rule for %v allows %s, which is no package in this module — the allow-list has drifted from the tree", r.forbidden, prefix)
			}
		}
	}
}

// TestTheRuleRosterIsTheDeclaredOne is the guard the two tests above cannot
// give: they check the rules that are present, and nothing checks that a rule
// is still there. Delete one — the whole `net/http` block, say — and the table
// stays internally consistent, every package in the module agrees with it, and
// this suite passes while the boundary it named is gone. That is the failure
// mode of every architecture test written as a loop over its own declarations,
// and it is why the roster is pinned here by literal.
//
// Each entry pins the whole rule and not only its forbidden prefix: the
// allowance is on the right of the arrow. Pinning the prefix alone caught a
// boundary being deleted and missed the quieter and likelier edit, which is a
// boundary being widened — one package appended to an allow-list, and a
// crossing the rule's own sentence forbids is open with every test green. The
// capability table next door does not catch it either: each case names one
// package the rule permits and one it refuses, so an allowance granted to some
// third package is invisible to both. Stated as a pair, the rule is the pair,
// and an allow-list cannot drift without this failing.
//
// The forged list is every forbidden prefix with the module path folded back to
// `<module>` and its allow-list beside it, sorted. Adding a boundary is
// therefore two edits — the rule and this line — and so is widening one, and
// removing one is two edits and a sentence of reasoning in the diff. All three
// are deliberate acts, which is the whole claim the table makes about itself.
func TestTheRuleRosterIsTheDeclaredOne(t *testing.T) {
	self := modulePath(t)

	forged := []string{}
	for _, r := range rules(self) {
		if len(r.forbidden) == 0 {
			// Already reported by TestEveryRuleGovernsSomething; nothing to pin.
			continue
		}
		// "nobody" rather than an empty tail, because a blanket prohibition is a
		// statement about the whole module and should read as one in the diff.
		allowance := "nobody"
		if len(r.allowed) > 0 {
			allowance = strings.Join(r.allowed, ", ")
		}
		for _, prefix := range r.forbidden {
			forged = append(forged, fmt.Sprintf("%s <- %s", strings.Replace(prefix, self, "<module>", 1), allowance))
		}
	}
	sort.Strings(forged)

	want := []string{
		"<module>/internal/adapters/inbound/ <- cmd",
		"<module>/internal/adapters/outbound/ <- cmd",
		"<module>/internal/application <- cmd, internal/adapters/inbound",
		"<module>/internal/config <- cmd",
		"<module>/internal/domain <- cmd, internal/application, internal/adapters/inbound, internal/adapters/outbound, internal/ports, internal/domain",
		"<module>/internal/domain/analytics <- cmd, internal/application, internal/adapters/inbound, internal/adapters/outbound, internal/ports",
		"<module>/internal/ports <- cmd, internal/application, internal/adapters, internal/ports",
		"database/sql <- cmd, internal/ports, internal/adapters/outbound",
		"github.com/valkey-io/valkey-go <- internal/adapters/outbound/valkey",
		"net/http <- cmd, internal/adapters/inbound, internal/adapters/outbound",
	}
	sort.Strings(want)

	if !slices.Equal(forged, want) {
		t.Errorf("the rule table forges %v, want %v — a rule was added, removed or widened, which is a change to the dependency rule itself", forged, want)
	}
}
