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
	// The list differs from its siblings' by exactly `internal/domain`, which
	// is absent rather than merely unused: a domain layer is where an
	// application puts the rules it owns about the data it owns, and this one
	// owns neither — everything it will eventually answer belongs to the Data
	// Plane and arrives over the seam below. A directory a package may live in
	// is a directory a package will live in.
	//
	// `internal/ports/outbound` and `internal/adapters/outbound` are sanctioned
	// because the usage-fact read is a real outbound dependency: the management
	// transport calls the Data Plane's internal surface, so the call needs a
	// port and an adapter like any other. What keeps it from becoming state is
	// TestTheManagementTransportHoldsNoState below, which names the one seam
	// that may exist under those trees — not the absence of the trees.
	internalRoots := []string{
		"internal/adapters/inbound",
		"internal/adapters/outbound",
		"internal/application",
		"internal/arch",
		"internal/config",
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

	// The two halves of the usage-fact seam are on the list for the reason the
	// rest of it exists: they are the packages this module's newest rules name,
	// and a rules table that governs a package the scan never saw agrees with
	// everything.
	for _, must := range []string{
		"cmd/" + app,
		"internal/application",
		"internal/adapters/inbound/http",
		"internal/ports/outbound/dataplane",
		"internal/adapters/outbound/dataplane",
	} {
		if !found[must] {
			t.Errorf("the scan did not find %s; every rule in this package would pass vacuously", must)
		}
	}

	graph := importGraph(t)
	// The outbound adapter is on this list because its imports are what make it
	// an adapter: a package found with no imports is a scanner reading files but
	// not their import blocks, and both the HTTP client and the port it
	// satisfies live in an import block.
	for _, dir := range []string{"cmd/" + app, "internal/application", "internal/adapters/outbound/dataplane"} {
		if len(graph[dir]) == 0 {
			t.Errorf("%s was found with no imports at all; the scanner is reading files but not their import blocks", dir)
		}
	}
}

// usageFactSeam names the one package pair this application may hold under the
// outbound trees, and it is listed by full path rather than by tree on purpose.
// The port answers "how does this application read Data Plane state" — ADR 0006
// §9's open question — with a call to the Data Plane, and a call is transport.
// A second package beside it is a different answer to the same question: a
// persistence port, a cache, a projection of Data Plane rows kept locally.
//
// The distinction the arch suite can draw is a path; the distinction that
// matters is whether the package holds state. They line up here because the
// seam is one port and one adapter, so anything else under these trees is
// something other than the call.
var usageFactSeam = []string{
	"internal/ports/outbound/dataplane",
	"internal/adapters/outbound/dataplane",
}

// TestTheManagementTransportHoldsNoState is this application's own structural
// claim, and it is the one ADR 0006 §11 makes: the management transport is a
// transport. It holds no state of its own, so it has no persistence, no cache
// and no domain layer — everything it will eventually answer comes from the
// Data Plane over the usage-fact seam, which is the answer §9 left open.
//
// The rule narrowed when the seam arrived. It used to forbid the outbound
// trees outright, which was the right rule while they were empty and would be
// the wrong one now: an outbound HTTP client is transport, not state, and a
// rule that cannot tell them apart forbids the seam this application exists to
// have. What it names instead is the state that must not appear beside it.
//
//   - `internal/domain` is where an application months from now would put the
//     entities it believes it owns. This one owns none: the usage fact's shape
//     is the Data Plane's to define (api/openapi/shared/usage-facts.yaml) and
//     this module's copy of it is a wire type in the adapter, deliberately the
//     thinnest thing that can carry the page across.
//   - anything under the outbound trees that is not the seam is a second
//     answer to §9, and the ordinary way one appears is that someone needs a
//     row locally, finds a query short, and the management API quietly becomes
//     a second owner of Data Plane data.
func TestTheManagementTransportHoldsNoState(t *testing.T) {
	for _, dir := range packageDirs(t) {
		if under(dir, "internal/domain") {
			t.Errorf("%s holds Go files: this application owns no data and so has no domain layer of its own — the shapes it carries belong to the Data Plane (ADR 0006 §9, §11)", dir)
		}
		for _, root := range []string{"internal/ports/outbound", "internal/adapters/outbound"} {
			if !under(dir, root) || slices.Contains(usageFactSeam, dir) {
				continue
			}
			t.Errorf("%s holds Go files: the only thing this application may place under %s is the usage-fact seam %v — anything else is state this application does not own (ADR 0006 §9, §11)", dir, root, usageFactSeam)
		}
	}
}
