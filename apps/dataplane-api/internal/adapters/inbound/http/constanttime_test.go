package http

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

// TestTheCredentialComparisonIsAFixedWidthDigestComparison holds on to the one
// property of `constantTimeEqual` that a behavioural test cannot see.
//
// Every other test in this package is blind to it, and that blindness is worth
// naming: `constantTimeEqual` returns the same boolean as `presented ==
// credential` for every pair of strings, so swapping the body for string
// equality, or for the length-sensitive `subtle.ConstantTimeCompare([]byte(a),
// []byte(b))`, leaves every accept case and every reject case green. Timing
// cannot close the gap either — a unit test asserting that a refusal took longer
// for one input than another is a flake on a loaded machine, not a proof. What
// is left is reading the source, which is what this does, and saying plainly
// what that is worth.
//
// What it proves: the comparison is written as a comparison of two SHA-256
// digests, and each of its two arguments is the full 32 bytes of one. A rewrite
// to `==`, to a length-sensitive compare, or to a comparison of one digest
// against itself turns this red.
//
// What it does not prove: that the compiled code is constant-time. Hashing does
// not make `subtle.ConstantTimeCompare` constant-time in the inputs beyond their
// width, and nothing here measures the machine, the allocator or the compiler.
// The digest is what removes the *length* of the secret from what a refusal
// reveals, and that is the specific leak — a secret recoverable one byte at a
// time — this function exists to close.
//
// This is the same pin the Data Plane's management listener carries over its own
// copy of the comparison. Neither file can check the other — the applications
// are separate Go modules — so each holds its own hop to the shape, which is the
// only arrangement in which neither end can quietly become the weaker one.
func TestTheCredentialComparisonIsAFixedWidthDigestComparison(t *testing.T) {
	file := parseSource(t, "serviceauth.go")
	fn := functionNamed(t, file, "constantTimeEqual")
	comparison := findDigestComparison(fn.Body)

	// Both sides hashed, or neither is: hashing one side and comparing it
	// against the raw bytes of the other is a length-sensitive comparison
	// wearing the right function.
	if comparison.sum256Calls != 2 {
		t.Errorf("constantTimeEqual makes %d sha256.Sum256 call(s), want 2 — one per side; the digests are what make the two arguments the same width whatever the secrets' lengths are", comparison.sum256Calls)
	}
	if comparison.compare == nil {
		t.Fatal("constantTimeEqual contains no subtle.ConstantTimeCompare; `presented == credential` answers at the first differing byte, so the time a refusal takes is a function of how much of the secret the caller guessed right")
	}
	if len(comparison.compare.Args) != 2 {
		t.Fatalf("subtle.ConstantTimeCompare was called with %d argument(s), want 2 — the secret and the secret", len(comparison.compare.Args))
	}

	first, firstOK := slicedIdentifier(comparison.compare.Args[0])
	second, secondOK := slicedIdentifier(comparison.compare.Args[1])

	for i, arg := range comparison.compare.Args {
		name, slice := slicedIdentifier(arg)
		if !slice {
			t.Errorf("argument %d of subtle.ConstantTimeCompare is not `sum[:]` — a full-width slice of a hash; anything else was never made the same width as its counterpart, and `[]byte(secret)` is a slice of the secret itself", i+1)
			continue
		}
		if !comparison.hashed[name] {
			t.Errorf("subtle.ConstantTimeCompare is given %s[:], and %s is not a variable this function assigned from sha256.Sum256; the lengths of the two secrets are then part of what the comparison measures", name, name)
		}
	}

	if firstOK && secondOK && first == second {
		t.Errorf("both arguments of subtle.ConstantTimeCompare are %s[:]; a digest compared with itself answers yes to every input", first)
	}
}

// digestComparison is what reading `constantTimeEqual`'s body yields: how many
// digests it computes, which names hold them, and the comparison itself.
type digestComparison struct {
	sum256Calls int
	hashed      map[string]bool
	compare     *ast.CallExpr
}

func findDigestComparison(body *ast.BlockStmt) digestComparison {
	found := digestComparison{hashed: map[string]bool{}}

	ast.Inspect(body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.AssignStmt:
			for i, rhs := range n.Rhs {
				call, ok := rhs.(*ast.CallExpr)
				if !ok || !isCallTo(call, "Sum256") {
					continue
				}
				found.sum256Calls++
				if i >= len(n.Lhs) {
					continue
				}
				// `sum := sha256.Sum256(...)` and `var sum [32]byte; sum =
				// sha256.Sum256(...)` are the same thing to this test, which is
				// right: what matters is that the value compared is a digest
				// held in a variable, not how it was declared.
				if ident, ok := n.Lhs[i].(*ast.Ident); ok {
					found.hashed[ident.Name] = true
				}
			}
		case *ast.CallExpr:
			if isCallTo(n, "ConstantTimeCompare") {
				found.compare = n
			}
		}
		return true
	})

	return found
}

// slicedIdentifier reports whether an expression is `x[:]` — a full-width slice
// of a value — and what x is called.
//
// A bounded slice is refused rather than accepted: `sum[:16]` is a slice of a
// digest, and comparing a prefix of one is comparing fewer bits than the secret
// has.
func slicedIdentifier(expr ast.Expr) (string, bool) {
	slice, ok := expr.(*ast.SliceExpr)
	if !ok || slice.Low != nil || slice.High != nil || slice.Max != nil {
		return "", false
	}
	ident, ok := slice.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	return ident.Name, true
}

// isCallTo reports whether a call names the function `name`, whatever package it
// is qualified by.
//
// The qualifier is deliberately not pinned. What this test is about is that the
// call happens at all and to what; which import path spells `sha256` or
// `subtle` is a fact the compiler already checks, and pinning it here would fail
// on a correctly-behaving rewrite that aliased the import.
func isCallTo(call *ast.CallExpr, name string) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == name
}

func parseSource(t *testing.T, name string) *ast.File {
	t.Helper()
	source, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), name, source, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	return file
}

func functionNamed(t *testing.T, file *ast.File, name string) *ast.FuncDecl {
	t.Helper()
	for _, declaration := range file.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if ok && fn.Name.Name == name && fn.Recv == nil {
			return fn
		}
	}
	t.Fatalf("%s declares no function %s; this test pins the shape of that function and proves nothing until it exists", file.Name, name)
	return nil
}
