package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestTxCallbacksNeverUseThePool statically enforces the rule the package
// doc comment states: a callback passed to Tx must do its work through
// the tx it's given, never s.Pool (or any other *_.Pool) — using the pool
// instead of the transaction inside a Tx callback compiles fine and
// silently breaks atomicity, the exact bug class this signature split
// (Tx vs TxQ) exists to make visible. Scans the whole repo, not just this
// package, since Tx is called from internal/ingest, internal/symbolicate
// and internal/export too.
func TestTxCallbacksNeverUseThePool(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))

	var violations []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".claude" || d.Name() == "node_modules" || d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Tx" || len(call.Args) == 0 {
				return true
			}
			lit, ok := call.Args[len(call.Args)-1].(*ast.FuncLit)
			if !ok {
				return true
			}
			ast.Inspect(lit.Body, func(n ast.Node) bool {
				if s, ok := n.(*ast.SelectorExpr); ok && s.Sel.Name == "Pool" {
					violations = append(violations, fset.Position(s.Pos()).String())
				}
				return true
			})
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) > 0 {
		t.Fatalf("Tx callback(s) reference .Pool instead of the tx they were given: %v", violations)
	}
}
