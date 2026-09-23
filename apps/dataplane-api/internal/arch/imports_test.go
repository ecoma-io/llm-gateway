package arch

import (
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
// coarse: an allow-list naming a tree (`internal/adapters`) rather than the
// one package that imports the library today is a rule about a boundary
// instead of a rule about a moment.
//
// Two of this application's six rules allow nobody, and they are the reason it
// is a separate module rather than a second surface on the runtime. The
// management transport holds no state: a cache client or a SQL handle here is
// not a shortcut, it is the crossing ADR 0006 §9 and §11 describe — and the
// fact that the answer to "how does this application read Data Plane state" is
// still open is a reason to forbid reaching for one locally, not a reason to
// defer the rule.
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
		{
			why:       "HTTP is a transport concern: the composition root and an inbound adapter may import it, and the application and any domain package may not — an application that speaks HTTP is an application whose use-cases cannot be called any other way",
			allowed:   []string{"cmd", "internal/adapters/inbound"},
			forbidden: []string{"net/http"},
		},
		{
			why:       "this application owns no data: ADR 0006 §9 records how it reaches Data Plane state as the split's one open question, and every answer to that question is a call to the Data Plane rather than a query from here",
			allowed:   nil,
			forbidden: []string{"database/sql", "github.com/jackc/pgx", "github.com/lib/pq"},
		},
		{
			why:       "configuration is read once, at the composition root: a package that reads the environment has behaviour its callers cannot see in its arguments and cannot vary in a test",
			allowed:   []string{"cmd"},
			forbidden: []string{self + "/internal/config"},
		},
		{
			why:       "a concrete adapter is constructed at the composition root or used by another adapter; everything else depends on the port, which is what makes the infrastructure replaceable",
			allowed:   []string{"cmd", "internal/adapters"},
			forbidden: []string{self + "/internal/adapters/"},
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

	dirs := make([]string, 0, len(graph))
	for dir := range graph {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	for _, dir := range dirs {
		for _, r := range rules(self) {
			if allowedToImport(dir, r.allowed) {
				continue
			}
			for _, imported := range graph[dir] {
				for _, forbidden := range r.forbidden {
					if imported == forbidden || strings.HasPrefix(imported, forbidden) {
						t.Errorf("%s imports %s: %s", dir, imported, r.why)
					}
				}
			}
		}
	}
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
