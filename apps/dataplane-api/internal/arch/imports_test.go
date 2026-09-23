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
// Two of this application's rules allow nobody, and they are the reason it is a
// separate module rather than a second surface on the runtime. The management
// transport holds no state: a cache client or a SQL handle here is not a
// shortcut, it is the crossing ADR 0006 §9 and §11 describe — and now that §9
// is closed, the rule says what the composition decided rather than standing in
// for a decision nobody had made: this application reaches Data Plane state
// through the port below, over the network, and never by opening a connection
// of its own.
//
// The two adapter rules are two lines rather than one, and that is the
// difference between a facade and a second application. A single rule
// forbidding `self/internal/adapters/` and allowing `internal/adapters` permits
// the inbound surface to import the outbound client directly — which builds
// fine, works, and puts the one cross-plane call this module exists to make
// somewhere the port cannot be substituted. Stated as two rules, the direction
// between the trees is a decision rather than an oversight.
//
// An import that appears in no rule is governed by none of them; this table is
// therefore not a complete account of what this module may import, only of
// where its boundaries lie.
func rules(self string) []rule {
	return []rule{
		{
			why:       "this application owns no data: a cache client here is the management transport acquiring state of its own, and state acquired here is state the Data Plane also owns (ADR 0006 §11)",
			allowed:   nil,
			forbidden: []string{"github.com/valkey-io/valkey-go"},
		},
		// Both adapter trees, and the symmetry is the point: whether HTTP is
		// arriving (an inbound surface) or leaving (the usage-fact call to the
		// Data Plane's internal surface), it is the boundary that speaks it and
		// the boundary is where the standard library belongs. What the rule
		// keeps out has not changed — an application that speaks HTTP is an
		// application whose use-cases cannot be called any other way, and whose
		// port is only reachable over a socket.
		{
			why:       "HTTP is a transport concern: the composition root and adapters on either side of the application may import it, and the application itself may not — an application that speaks HTTP is an application whose use-cases cannot be called any other way",
			allowed:   []string{"cmd", "internal/adapters/inbound", "internal/adapters/outbound"},
			forbidden: []string{"net/http"},
		},
		{
			why:       "this application owns no data: it reaches the Data Plane through the port below, over the network, and the composition that decided that (ADR 0006 §9) decided against a second reader of the Data Plane's tables — a SQL handle here would be a second process writing the Data Plane's schema from outside its migration lane",
			allowed:   nil,
			forbidden: []string{"database/sql", "github.com/jackc/pgx", "github.com/lib/pq"},
		},
		{
			why:       "the port vocabulary is what the application is written against and what the adapters implement, so it points inward from both: `internal/config` and the transport kit are written against neither, and a package that named the port would be a package that had acquired an opinion about the Data Plane without a seam to state it at",
			allowed:   []string{"cmd", "internal/application", "internal/adapters", "internal/ports"},
			forbidden: []string{self + "/internal/ports"},
		},
		{
			why:       "configuration is read once, at the composition root: a package that reads the environment has behaviour its callers cannot see in its arguments and cannot vary in a test",
			allowed:   []string{"cmd"},
			forbidden: []string{self + "/internal/config"},
		},
		{
			why:       "an inbound adapter is entered by the composition root and by nothing else: the outbound client is the one cross-plane hop this module makes, and a surface that could import an inbound package could delegate that hop to a route — or, worse, to another application's relay that happens to look like one",
			allowed:   []string{"cmd", "internal/adapters/inbound"},
			forbidden: []string{self + "/internal/adapters/inbound/"},
		},
		{
			why:       "an outbound adapter is constructed at the composition root and by nothing else: it is reached through the port the application declares, so an inbound surface that imported one would have made the facade read the Data Plane by a route of its own rather than through the seam the whole module is built around",
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
// is still there. Delete one — either of this application's two blanket
// prohibitions, say — and the table stays internally consistent, every package
// in the module agrees with it, and this suite passes while the boundary it
// named is gone. That is the failure mode of every architecture test written
// as a loop over its own declarations, and it is why the roster is pinned here
// by literal.
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
		"github.com/jackc/pgx",
		"github.com/lib/pq",
		"github.com/valkey-io/valkey-go",
		"net/http",
	}
	sort.Strings(want)

	if !slices.Equal(forged, want) {
		t.Errorf("the rule table forges %v, want %v — a rule was added or removed, which is a change to the dependency rule itself", forged, want)
	}
}
