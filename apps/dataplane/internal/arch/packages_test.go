// Package arch is where this module's architecture is enforced.
//
// The rules are tests rather than a linter plugin or a dependency on a
// boundary tool, for one reason: every rule here is a predicate over import
// paths and package directories, and the standard library already parses both
// (go/parser, io/fs). A tool would be a dependency, a configuration file and a
// version to pin — bought to express a few lines of prefix matching.
//
// What is enforced is AGENTS.md rule 4 (the planes do not talk sideways) and
// rule 5 (infrastructure stays behind explicit boundaries), which is the
// dependency rule ADR 0006 §6 states. They are the reason a violation is a
// red build rather than a review comment.
//
// Three files, three questions:
//
//   - packages_test.go — does every package live where this architecture says
//     a package may live, and does this module carry the structure the
//     application it claims to be requires?
//   - imports_test.go — may this package import that one?
//   - modules_test.go — is this application's code the only application's
//     code inside its own module?
package arch

import (
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// moduleRoot is the module this package belongs to. `internal/arch` sits two
// directories below it, and `go test` runs each package with that package's
// own directory as the working directory — so the path is a constant and no
// lookup is needed.
const moduleRoot = "../.."

// goModPath is the module file the rules read for this module's own path.
const goModPath = "../../go.mod"

// modulePath is this module's import prefix, read from go.mod rather than
// written here: a copy of a module path in a test is a second place to edit
// when the module moves, and the copy that goes stale is always the one
// nobody reads.
func modulePath(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatalf("reading %s: %v", goModPath, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatalf("%s declares no module directive", goModPath)
	return ""
}

// packageDirs returns every directory in the module holding at least one Go
// file, as slash-separated paths relative to the module root.
func packageDirs(t *testing.T) []string {
	t.Helper()
	dirs := make([]string, 0, len(importGraph(t)))
	for dir := range importGraph(t) {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}

// importGraph maps each package directory to the import paths its files
// declare. Test files count: a test that reaches across a boundary is a real
// dependency of the package's build, and the boundary it crosses is the one
// someone will copy into non-test code.
func importGraph(t *testing.T) map[string][]string {
	t.Helper()
	graph := map[string][]string{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(moduleRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return fmt.Errorf("parsing %s: %w", path, parseErr)
		}
		dir, relErr := filepath.Rel(moduleRoot, filepath.Dir(path))
		if relErr != nil {
			return relErr
		}
		dir = filepath.ToSlash(dir)
		// The key is created even when the file imports nothing. A package of
		// pure declarations is a package — a domain type, a constant table —
		// and a graph that only records packages with imports would leave the
		// layout rule below with nothing to check it against.
		if _, seen := graph[dir]; !seen {
			graph[dir] = []string{}
		}
		for _, spec := range file.Imports {
			imported, unquoteErr := strconv.Unquote(spec.Path.Value)
			if unquoteErr != nil {
				return fmt.Errorf("reading an import in %s: %w", path, unquoteErr)
			}
			graph[dir] = append(graph[dir], imported)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scanning the module: %v", err)
	}
	return graph
}

// under reports whether a package directory is one of prefix, or sits below
// it. Comparing path segments rather than raw strings is what keeps
// `internal/adapters_old` from counting as `internal/adapters`.
func under(dir, prefix string) bool {
	return dir == prefix || strings.HasPrefix(dir, prefix+"/")
}

// TestEveryPackageLivesUnderASanctionedRoot is the layout rule: the rooted
// structure ADR 0006 §6 describes, asserted rather than described. A package
// that appears outside it — `internal/util`, `internal/infra`, a second
// adapter tree — widens the architecture, and widening it is a decision that
// belongs in a diff rather than in a directory someone created in passing.
func TestEveryPackageLivesUnderASanctionedRoot(t *testing.T) {
	internalRoots := []string{
		"internal/adapters/inbound",
		"internal/adapters/outbound",
		"internal/application",
		"internal/arch",
		"internal/config",
		"internal/domain",
		"internal/ports/inbound",
		"internal/ports/outbound",
	}

	for _, dir := range packageDirs(t) {
		if under(dir, "cmd") {
			continue
		}
		sanctioned := false
		for _, root := range internalRoots {
			if under(dir, root) {
				sanctioned = true
				break
			}
		}
		if !sanctioned {
			t.Errorf("%s holds Go files but lives under no sanctioned root; the roots are %v — a new one is a deliberate change to ADR 0006 §6", dir, internalRoots)
		}
	}
}

// TestTheScanSeesTheModule is the guard that keeps every rule in this package
// from being true for the wrong reason. A walk pointed at the wrong
// directory, a module that lost its entry point, a build tag that excluded
// everything: each finds nothing, and rules over an empty set all pass. These
// packages must be found, and their absence is a scanner defect rather than
// an architecture one.
func TestTheScanSeesTheModule(t *testing.T) {
	app := filepath.Base(modulePath(t))
	found := map[string]bool{}
	for _, dir := range packageDirs(t) {
		found[dir] = true
	}

	for _, must := range []string{"cmd/" + app, "internal/application", "internal/adapters/inbound/http"} {
		if !found[must] {
			t.Errorf("the scan did not find %s; every rule in this package would pass vacuously", must)
		}
	}

	graph := importGraph(t)
	for _, dir := range []string{"cmd/" + app, "internal/application"} {
		if len(graph[dir]) == 0 {
			t.Errorf("%s was found with no imports at all; the scanner is reading files but not their import blocks", dir)
		}
	}
}

// TestTheRuntimeHasNoCrossPlanePort is the runtime's own structural claim:
// the Data Plane reaches its own state through two outbound ports and reaches
// nothing else (ADR 0006 §5). The Control Plane is not a dependency of this
// process — a request that needs it is a request that fails when it is down,
// which is the failure mode the split exists to remove — and the management
// transport is not something the runtime calls either: the flow between those
// two runs the other way, over records rather than requests.
//
// The list below is the claim, so it is written out in full: adding an
// outbound port to this application is a deliberate act with an edit here,
// which is exactly the moment to ask whether the port leaves the plane.
//
// The rule holds regardless of whether an adapter is wired yet. `cache` and
// `persistence` have adapters that no caller constructs today — the fact
// reader's adapter exists and refuses, which is a third state worth naming —
// and that is expected at scaffold stage; what is not expected is a port naming
// the Control Plane or the management API, now or later.
//
// `usagefacts` is the third port and the honest question is whether it breaks
// the rule. It does not: it is this process reading back what it recorded, and
// its adapter is this Data Plane's own storage. What would break the rule is a
// port whose adapter dials one of the other applications, and every such port
// would have to be added to this list first — which is why the list is written
// out rather than derived, and why a port named for a peer application would be
// an edit here that a reviewer cannot miss.
func TestTheRuntimeHasNoCrossPlanePort(t *testing.T) {
	ports := outboundPorts(t)
	want := []string{"cache", "persistence", "usagefacts"}
	if !slices.Equal(ports, want) {
		t.Errorf("outbound ports are %v, want %v — the three are this process's own infrastructure: its cache, its database, and its own recorded history. A port named for the Control Plane or the management API would be a hop on the request path, and the runtime does not have one", ports, want)
	}
}

// outboundPorts returns the directory names under internal/ports/outbound, in
// sorted order.
func outboundPorts(t *testing.T) []string {
	t.Helper()
	const root = "internal/ports/outbound"
	entries, err := os.ReadDir(filepath.Join(moduleRoot, root))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading %s: %v", root, err)
	}
	ports := []string{}
	for _, entry := range entries {
		if entry.IsDir() {
			ports = append(ports, entry.Name())
		}
	}
	sort.Strings(ports)
	return ports
}
