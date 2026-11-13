// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestToolLogsNeverRecordRawErrors keeps raw error values out of the tool
// logs: error text can quote learner content, concept labels, file paths or
// SQL. Every logged error goes through diagnosticErrorAttrs (or
// safeErrorResult), so a call such as Logger.Warn("...", "err", err) fails
// this test.
func TestToolLogsNeverRecordRawErrors(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, source, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch selector.Sel.Name {
			case "Debug", "Info", "Warn", "Error", "DebugContext", "InfoContext", "WarnContext", "ErrorContext", "Log":
			default:
				return true
			}
			for i := 1; i+1 < len(call.Args); i++ {
				key, ok := call.Args[i].(*ast.BasicLit)
				if !ok || key.Kind != token.STRING {
					continue
				}
				switch strings.Trim(key.Value, "\"`") {
				case "err", "error", "cause":
					t.Errorf("%s: log call records a raw error under %s; use diagnosticErrorAttrs", fset.Position(call.Pos()), key.Value)
				}
			}
			return true
		})
	}
}
