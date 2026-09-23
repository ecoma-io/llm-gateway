package arch

import (
	"os"
	"strings"
	"testing"
	"unicode"
)

// appsPrefix is the prefix every application's module path shares. The rule
// below is about what follows it: one application's module may name itself and
// nothing else under here.
const appsPrefix = "github.com/ecoma-io/llm-gateway/apps/"

// appsRoot is that same directory tree on disk, walked to find the sibling
// modules this one must not require. It is reached through the module root
// rather than written as a path to this application, so the three copies of
// this file stay byte-identical.
const appsRoot = "../../../"

// TestTheModuleRequiresNoSibling is the rule that a monorepo makes necessary:
// three Go modules in one repository invite a `require` between them the first
// time two applications want the same helper, and that `require` is exactly
// how a Control Plane and a Data Plane become one program with two binaries.
//
// The workspace file is what makes this checkable rather than merely true: with
// go.work present, adding a sibling here would resolve locally and the module
// would look healthy right up until it was built alone. Go's internal rule
// stops this module reaching the runtime's packages, but it would not stop it
// reaching a future `apps/dataplane/pkg/...`, and the difference between the
// two is a directory name — so the assertion is on the module, not on the
// package.
func TestTheModuleRequiresNoSibling(t *testing.T) {
	self := modulePath(t)
	siblings := siblingModules(t)
	if len(siblings) == 0 {
		t.Fatal("no sibling modules were found under apps/; this check would pass vacuously")
	}

	data, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatalf("reading %s: %v", goModPath, err)
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "//") {
			continue
		}
		// A `require` and a `use` both spell the dependency the same way — a
		// path — so one loop covers `require github.com/... v1.2.3`,
		// `require ( ... )` blocks and any future `replace`/`use` line that
		// names a sibling.
		for _, sibling := range siblings {
			if sibling == self {
				continue
			}
			if namesModule(line, sibling) {
				t.Errorf("%s names %s: an application may not depend on another application's module — the two are separate programs, and a require here is how they stop being separate (ADR 0006 §6)", goModPath, sibling)
			}
		}
	}
}

// TestNoFileImportsASiblingApplication is the same rule at the granularity the
// go.mod check cannot see: a file that imports a sibling's package without a
// require. That state does not build today, so this is not a test of a live
// defect — it is the one that names the importing file when someone adds the
// require, since the go.mod error alone says only which module was named.
func TestNoFileImportsASiblingApplication(t *testing.T) {
	self := modulePath(t)
	if len(siblingModules(t)) == 0 {
		t.Fatal("no sibling modules were found under apps/; this check would pass vacuously")
	}

	for dir, imports := range importGraph(t) {
		for _, imported := range imports {
			if !strings.HasPrefix(imported, appsPrefix) {
				continue
			}
			if under(imported, self) {
				continue
			}
			t.Errorf("%s imports %s: an application may not import another application's code — the Data Plane is reached over the network, through a port, and never by import (ADR 0006 §6)", dir, imported)
		}
	}
}

// namesModule reports whether a go.mod line names an import path, matched on a
// path segment rather than on a substring. The distinction is not pedantry:
// `.../apps/dataplane` is a prefix of `.../apps/dataplane-api`, so a plain
// Contains makes every application whose name begins with a sibling's report a
// dependency on it — and a check that fires on the module's own `module`
// directive is a check that gets deleted rather than fixed.
func namesModule(line, module string) bool {
	for offset := 0; ; {
		index := strings.Index(line[offset:], module)
		if index < 0 {
			return false
		}
		end := offset + index + len(module)
		if end == len(line) {
			return true
		}
		// A module path continues with a path-segment character; anything else
		// after the match means the match ended on a boundary and the line
		// really does name this module.
		if next := rune(line[end]); !continuesModulePath(next) {
			return true
		}
		offset = end
	}
}

// continuesModulePath reports whether a character can appear inside a module
// path — the set Go itself allows in an import path, which is what decides
// whether a match continued past the module's name or stopped at its end.
func continuesModulePath(r rune) bool {
	return r == '/' || r == '-' || r == '.' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// siblingModules returns the module path of every Go module under apps/, read
// from each module's own go.mod so the list follows the tree rather than a
// roster that would go stale.
func siblingModules(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(appsRoot)
	if err != nil {
		t.Fatalf("reading %s: %v", appsRoot, err)
	}

	modules := []string{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := appsRoot + entry.Name() + "/go.mod"
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			// A directory under apps/ that is not a Go module — the console —
			// is expected here, so an absent go.mod is not a failure.
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
				modules = append(modules, strings.TrimSpace(rest))
				break
			}
		}
	}
	return modules
}

// TestTheScanSeesTheSiblings is the guard for both rules above: each walks a
// set — the sibling modules, the import graph — and each passes trivially when
// that set comes back empty or wrong. This pins the set to the applications
// this repository has, so a path typo in appsRoot is a failure here rather
// than a quiet pass there.
//
// All three Go modules are named, and the third is the one worth naming: the
// rules above walk whatever `siblingModules` returns, so a module missing from
// *that* walk is a module nothing checks — and `dataplane-api` is the sibling
// the other two are most likely to reach for, because it is the same product's
// management surface rather than a different product's application. A list
// naming two of three is a list that stops noticing the newest one.
func TestTheScanSeesTheSiblings(t *testing.T) {
	self := modulePath(t)
	found := map[string]bool{}
	for _, sibling := range siblingModules(t) {
		found[sibling] = true
	}

	if !found[self] {
		t.Errorf("the sibling scan did not find this module %s; both module rules would pass vacuously", self)
	}
	for _, want := range []string{
		appsPrefix + "console-api",
		appsPrefix + "dataplane",
		appsPrefix + "dataplane-api",
	} {
		if !found[want] {
			t.Errorf("the sibling scan did not find %s; a module that depends on it would look clean", want)
		}
	}
}
