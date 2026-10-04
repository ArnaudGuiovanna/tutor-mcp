// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

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

// Rows.Close does not report the error that ended iteration: when Next stops
// because the connection dropped or the context expired, Close returns nil and
// the loop silently yields a truncated result. A restore could then declare its
// foreign keys verified after reading only part of the inventory. Every
// production function that iterates rows must therefore consult Err.
func TestRowIterationErrorsAreChecked(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var missing []string
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			var body *ast.BlockStmt
			switch fn := node.(type) {
			case *ast.FuncDecl:
				body = fn.Body
			case *ast.FuncLit:
				body = fn.Body
			default:
				return true
			}
			if body == nil {
				return true
			}
			nexts := map[string]token.Pos{}
			checked := map[string]bool{}
			ast.Inspect(body, func(inner ast.Node) bool {
				if lit, ok := inner.(*ast.FuncLit); ok && lit != node {
					return false
				}
				call, ok := inner.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				receiver, ok := selector.X.(*ast.Ident)
				if !ok {
					return true
				}
				switch selector.Sel.Name {
				case "Next":
					if _, seen := nexts[receiver.Name]; !seen && len(call.Args) == 0 {
						nexts[receiver.Name] = call.Pos()
					}
				case "Err":
					checked[receiver.Name] = true
				}
				return true
			})
			for name, pos := range nexts {
				if !checked[name] {
					position := fset.Position(pos)
					rel, _ := filepath.Rel(root, position.Filename)
					missing = append(missing, rel+":"+strconv.Itoa(position.Line)+" ("+name+")")
				}
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("row iteration without an Err check:\n  %s", strings.Join(missing, "\n  "))
	}
}
