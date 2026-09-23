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

// rules is the dependency rule, one line per boundary. Each is deliberately
// coarse: an allow-list naming a tree (`internal/adapters/inbound`) rather than
// the one package that imports the library today is a rule about a boundary
// instead of a rule about a moment.
//
// The two adapter rules are two lines rather than one. A single rule forbidding
// `self/internal/adapters/` and allowing `internal/adapters` says an adapter
// may import an adapter, which permits the one crossing this application has
// most reason to refuse: an inbound surface that constructs the cross-plane
// client itself, or the reverse — the management seam reached from a route
// instead of from the port that exists to make it substitutable. Stated as two
// rules, the direction between the trees is a decision rather than an
// oversight. A concrete adapter remains reachable from another adapter in its
// own tree, which is how an outbound adapter composes with a sibling.
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
			allowed:   []string{"cmd", "internal/adapters/inbound"},
			forbidden: []string{self + "/internal/adapters/inbound/"},
		},
		{
			why:       "an outbound adapter is constructed at the composition root and by nothing else, and reached from the application through the port it implements: an inbound adapter that imported one has put concrete infrastructure behind a route with no port to substitute, which on this application is the cross-plane seam arriving as a call the use case cannot see",
			allowed:   []string{"cmd", "internal/adapters/outbound"},
			forbidden: []string{self + "/internal/adapters/outbound/"},
		},
		{
			why:       "the application is called by an inbound adapter and wired by the composition root; nothing outbound may depend on it, because an outbound adapter that knows the use-case has inverted the arrow",
			allowed:   []string{"cmd", "internal/adapters/inbound"},
			forbidden: []string{self + "/internal/application"},
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
	dirs := make([]string, 0, len(graph))
	for dir := range graph {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	found := []string{}
	for _, dir := range dirs {
		for _, r := range rules(self) {
			if allowedToImport(dir, r.allowed) {
				continue
			}
			for _, imported := range graph[dir] {
				for _, forbidden := range r.forbidden {
					if imported == forbidden || strings.HasPrefix(imported, forbidden) {
						found = append(found, fmt.Sprintf("%s imports %s: %s", dir, imported, r.why))
					}
				}
			}
		}
	}
	sort.Strings(found)
	return found
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
// The forged list is every forbidden prefix with the module path folded back
// to `<module>`, sorted. Adding a boundary is therefore two edits — the rule
// and this line — and removing one is two edits and a sentence of reasoning in
// the diff. Both are deliberate acts, which is the whole claim the table makes
// about itself.
func TestTheRuleRosterIsTheDeclaredOne(t *testing.T) {
	self := modulePath(t)

	forged := []string{}
	for _, r := range rules(self) {
		if len(r.forbidden) == 0 {
			// Already reported by TestEveryRuleGovernsSomething; nothing to pin.
			continue
		}
		for _, prefix := range r.forbidden {
			forged = append(forged, strings.Replace(prefix, self, "<module>", 1))
		}
	}
	sort.Strings(forged)

	want := []string{
		"<module>/internal/adapters/inbound/",
		"<module>/internal/adapters/outbound/",
		"<module>/internal/application",
		"<module>/internal/config",
		"<module>/internal/ports",
		"database/sql",
		"github.com/valkey-io/valkey-go",
		"net/http",
	}
	sort.Strings(want)

	if !slices.Equal(forged, want) {
		t.Errorf("the rule table forges %v, want %v — a rule was added or removed, which is a change to the dependency rule itself", forged, want)
	}
}
