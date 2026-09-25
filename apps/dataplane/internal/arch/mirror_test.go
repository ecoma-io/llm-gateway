package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The credential mirror's containment rule, in the spirit of the import
// rules this package already runs: ADR 0007 makes the mirror — the
// api_key_credentials and account_states tables — the ONLY credential source
// on this plane, and this file enforces that sentence instead of trusting it
// to review discipline. Two predicates, both over the module's own Go
// sources:
//
//   - the mirror tables are named only inside the postgres adapter, where
//     the projection applier writes them and the credential read reads
//     them. A table name surfacing anywhere else — a domain type, an
//     application query, an HTTP handler's error text — is the first step of
//     a second credential source being built, and fails here rather than in
//     production, where a second source means two answers to "may this
//     request through".
//   - the persistence.Credentials port is satisfied, outside test files,
//     exactly once, and that one implementation is the mirror-backed
//     adapter. A second non-test implementation — however innocent its
//     framing — is the second source; fakes in _test files are the
//     application layer's legitimate way of testing against the port and
//     are allowed wherever tests live.
//
// The scan walks the module and reads string literals and package-level var
// declarations through go/parser, the same machinery imports_test.go runs.
// The enforcing package skips itself: this file names the tables it guards,
// and a rule that failed on its own text would teach the next contributor to
// weaken the rule rather than the text.

// credentialMirrorTables are the projection foundation's two mirror tables,
// spelled as SQL names them. Any string literal containing one of them is a
// mention of the mirror.
var credentialMirrorTables = []string{"api_key_credentials", "account_states"}

// mirrorAdapterDir is the one package allowed to name the mirror tables.
const mirrorAdapterDir = "internal/adapters/outbound/postgres"

// goSources returns every non-directory Go file under moduleRoot as a path
// relative to the module root, in sorted order.
func goSources(t *testing.T) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(moduleRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}
		rel, relErr := filepath.Rel(moduleRoot, path)
		if relErr != nil {
			return relErr
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("scanning the module: %v", err)
	}
	sort.Strings(files)
	return files
}

// parseModule parses every file goSources found, full syntax, and hands back
// the trees keyed by module-relative path.
func parseModule(t *testing.T) map[string]*ast.File {
	t.Helper()
	fset := token.NewFileSet()
	trees := map[string]*ast.File{}
	for _, rel := range goSources(t) {
		file, err := parser.ParseFile(fset, filepath.Join(moduleRoot, rel), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", rel, err)
		}
		trees[rel] = file
	}
	return trees
}

// TestTheCredentialMirrorIsNamedOnlyInTheAdapter scans every string literal
// in the module's Go sources for the mirror tables' names. The postgres
// adapter is the one sanctioned location: projection.go's applier writes the
// rows, credentialsrepo.go's read consumes them, and the integration suites
// beside them probe the schema they stand on — all one package, the one the
// persistence port's adapter rule already fences off. Anywhere else the name
// is the seam a second credential source would open (see the file comment).
func TestTheCredentialMirrorIsNamedOnlyInTheAdapter(t *testing.T) {
	trees := parseModule(t)
	for rel, file := range trees {
		if dir := filepath.ToSlash(filepath.Dir(rel)); dir == "internal/arch" {
			continue // this file names the tables it enforces; see the file comment
		}
		mentions := map[string]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true // not a plain string literal; nothing honest to read from it
			}
			for _, table := range credentialMirrorTables {
				if strings.Contains(value, table) {
					mentions[table] = true
				}
			}
			return true
		})
		if len(mentions) == 0 {
			continue
		}
		if dir := filepath.ToSlash(filepath.Dir(rel)); dir != mirrorAdapterDir {
			t.Errorf("%s names the credential mirror tables (%s); ADR 0007 allows the mirror to be named only in %s — a name anywhere else is the first line of a second credential source",
				rel, strings.Join(sortedKeys(mentions), ", "), mirrorAdapterDir)
		}
	}
}

// TestTheCredentialPortHasExactlyOneProductionImplementation finds every
// package-level var declaration outside a test file whose text mentions
// persistence.Credentials — the compile-time satisfaction proof this module
// writes as `var _ persistence.Credentials = (*credentialsRepo)(nil)` — and
// requires exactly one, in the postgres adapter. Test files are exempt: the
// application layer's fakes prove satisfaction there by design. Zero proofs
// means the adapter stopped claiming the port (a compile error waiting, and
// a wiring nobody checked); two mean the runtime has two ways to answer
// "who is this caller", and the mirror stopped being the only one.
func TestTheCredentialPortHasExactlyOneProductionImplementation(t *testing.T) {
	trees := parseModule(t)
	type proof struct{ dir string }
	var proofs []proof
	for rel, file := range trees {
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		for _, decl := range file.Decls { // package-level only: a satisfaction proof is a file's claim about itself
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			satisfied := false
			ast.Inspect(gen, func(inner ast.Node) bool {
				sel, ok := inner.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "persistence" && sel.Sel.Name == "Credentials" {
					satisfied = true
				}
				return true
			})
			if satisfied {
				proofs = append(proofs, proof{dir: filepath.ToSlash(filepath.Dir(rel))})
			}
		}
	}

	if len(proofs) != 1 {
		t.Fatalf("persistence.Credentials is satisfied by %d production var declarations (%v), want exactly 1 — the mirror-backed adapter is the only credential source (ADR 0007)", len(proofs), proofs)
	}
	if proofs[0].dir != mirrorAdapterDir {
		t.Errorf("persistence.Credentials is satisfied in %s, want %s — the one implementation must be the mirror-backed adapter", proofs[0].dir, mirrorAdapterDir)
	}
}

// sortedKeys orders a mention set for stable failure messages.
func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
