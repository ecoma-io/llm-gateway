package arch

import (
	"fmt"
	"go/ast"
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

// secretPackage is the package that owns Secret. The rule is deliberately
// stated in terms of this directory rather than the spelling of a type: the
// bytes and the only API that can use them without leaking them belong here,
// and no other package may put either behind a struct field.
const secretPackage = "internal/domain/identity"

// secretNestingWhy is the whole reason this rule exists, and so it is part of
// every failure rather than a comment beside it. A boundary whose diagnostic
// says only "forbidden" invites the next contributor to look for a way around
// the test; this says what fmt does and what shape is safe instead.
const secretNestingWhy = "nesting a Secret is forbidden because fmt skips Stringer for unexported fields and can print its raw bytes under %v/%+v/%#v; hold the bytes behind the keymaterial package's API and pass a Secret as an argument, return value or interface value instead"

// parsedGoFile is one source file in the module, retaining the module-relative
// path and package directory needed both to exempt keymaterial itself and to
// put the offending position in a failure.
type parsedGoFile struct {
	path        string
	dir         string
	packageName string
	syntax      *ast.File
}

// TestTheSecretNestingRuleHolds checks every package's struct fields, not only
// the domain's own types. The danger appears at the aggregate that holds a
// Secret, wherever that aggregate lives, and an exported field is refused for
// the same reason as an unexported one: only the keymaterial package may own
// the shape whose fmt behaviour is part of its redaction contract.
func TestTheSecretNestingRuleHolds(t *testing.T) {
	fset := token.NewFileSet()
	sources := parseGoSources(t, fset)

	secretPackageName := ""
	for _, source := range sources {
		// A `_test` suffix identifies an external test package in the same
		// directory. It is a different package and falls under the rule; the
		// internal test files share the package under test and are exempt.
		if source.dir == secretPackage && !strings.HasSuffix(source.packageName, "_test") {
			secretPackageName = source.packageName
			break
		}
	}
	if secretPackageName == "" {
		t.Fatalf("the source scan found no package at %s; the Secret-nesting rule would pass without its own exemption", secretPackage)
	}

	importPath := modulePath(t) + "/" + secretPackage
	found := []string{}
	for _, source := range sources {
		if source.dir == secretPackage && source.packageName == secretPackageName {
			continue
		}
		violations, err := secretNestingViolations(fset, source, importPath, secretPackageName)
		if err != nil {
			t.Fatal(err)
		}
		found = append(found, violations...)
	}
	sort.Strings(found)

	for _, violation := range found {
		t.Error(violation)
	}
}

// parseGoSources parses every Go file in the module, including tests. A guard
// that skipped test packages would let the next aggregate be introduced first
// in a fixture and copied into production after the failure has become
// somebody else's problem. Paths are retained relative to the module root so
// failures name the source a contributor will open, not the temporary
// directory `go test` happened to use.
func parseGoSources(t *testing.T, fset *token.FileSet) []parsedGoFile {
	t.Helper()
	sources := []parsedGoFile{}
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
		rel = filepath.ToSlash(rel)
		source, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("reading %s: %w", rel, readErr)
		}
		file, parseErr := parser.ParseFile(fset, rel, source, 0)
		if parseErr != nil {
			return fmt.Errorf("parsing %s: %w", rel, parseErr)
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		sources = append(sources, parsedGoFile{
			path:        rel,
			dir:         dir,
			packageName: file.Name.Name,
			syntax:      file,
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scanning the module: %v", err)
	}
	return sources
}

// secretNestingViolations returns one failure per struct field whose declared
// type is Secret. It resolves the import's local name from the file's import
// block rather than assuming every file spells it `identity`, so renaming an
// import cannot silently disable the rule.
func secretNestingViolations(fset *token.FileSet, source parsedGoFile, importPath, importName string) ([]string, error) {
	localNames, err := secretImportNames(source, importPath, importName)
	if err != nil {
		return nil, err
	}
	if len(localNames) == 0 {
		return nil, nil
	}

	// ast.Inspect visits a TypeSpec before the StructType it names, so the map
	// is populated by the time the struct callback runs. An unnamed struct
	// simply has no entry and is reported as anonymous.
	structNames := map[*ast.StructType]string{}
	found := []string{}
	ast.Inspect(source.syntax, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.TypeSpec:
			if structType, ok := node.Type.(*ast.StructType); ok {
				structNames[structType] = node.Name.Name
			}
		case *ast.StructType:
			owner := structNames[node]
			if owner == "" {
				owner = "<anonymous struct>"
			}
			for _, field := range node.Fields.List {
				if !declaresSecret(field.Type, localNames) {
					continue
				}
				name := "<embedded>"
				if len(field.Names) != 0 {
					names := make([]string, 0, len(field.Names))
					for _, declared := range field.Names {
						names = append(names, declared.Name)
					}
					name = strings.Join(names, ", ")
				}
				position := fset.Position(field.Type.Pos())
				found = append(found, fmt.Sprintf("%s:%d: struct field %s.%s has type %s.Secret: %s", source.path, position.Line, owner, name, importName, secretNestingWhy))
			}
		}
		return true
	})
	return found, nil
}

// secretImportNames returns the local names by which one import may be
// referenced in a file. The package's declared name is the default for an
// unaliased import, while `.` is retained as a name so a dot import cannot
// evade the rule either.
func secretImportNames(source parsedGoFile, importPath, importName string) (map[string]bool, error) {
	names := map[string]bool{}
	for _, spec := range source.syntax.Imports {
		imported, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return nil, fmt.Errorf("reading an import in %s: %w", source.path, err)
		}
		if imported != importPath {
			continue
		}
		localName := importName
		if spec.Name != nil {
			localName = spec.Name.Name
		}
		if localName != "_" {
			names[localName] = true
		}
	}
	return names, nil
}

// declaresSecret reports whether a field's declared type is the guarded
// package's Secret. Selector matching handles named and aliased imports; a dot
// import turns the type into an identifier. Other expressions are not Secret
// types themselves and are therefore outside this rule.
func declaresSecret(expr ast.Expr, localNames map[string]bool) bool {
	selector, ok := expr.(*ast.SelectorExpr)
	if ok {
		packageName, isIdent := selector.X.(*ast.Ident)
		return isIdent && localNames[packageName.Name] && selector.Sel.Name == "Secret"
	}

	identifier, ok := expr.(*ast.Ident)
	return ok && localNames["."] && identifier.Name == "Secret"
}

// TestTheSecretNestingRuleRefusesExportedAndUnexportedFields proves the rule
// can fire before trusting its green result over the current tree. Both field
// names are in one synthetic struct: the unexported case is the leak fmt
// creates, and the exported case is the same prohibited storage shape caught
// by the package's wider contract. The expected messages are exact so the
// diagnostic cannot lose either the mechanism or the safe replacement.
func TestTheSecretNestingRuleRefusesExportedAndUnexportedFields(t *testing.T) {
	identityPath := modulePath(t) + "/" + secretPackage
	fixture := fmt.Sprintf(`package outsidekeymaterial

import identity %q

type wrappedSecret struct {
	Exported identity.Secret
	secret   identity.Secret
}
`, identityPath)
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "outsidekeymaterial.go", fixture, 0)
	if err != nil {
		t.Fatalf("parsing the synthetic fixture: %v", err)
	}
	source := parsedGoFile{
		path:        "outsidekeymaterial.go",
		dir:         "outsidekeymaterial",
		packageName: file.Name.Name,
		syntax:      file,
	}

	got, err := secretNestingViolations(fset, source, identityPath, "identity")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		fmt.Sprintf("outsidekeymaterial.go:6: struct field wrappedSecret.Exported has type identity.Secret: %s", secretNestingWhy),
		fmt.Sprintf("outsidekeymaterial.go:7: struct field wrappedSecret.secret has type identity.Secret: %s", secretNestingWhy),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the Secret-nesting guard reported %v, want %v; it must reject both an exported and an unexported Secret field", got, want)
	}
}
