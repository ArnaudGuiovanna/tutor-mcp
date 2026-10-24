// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// This fingerprint is the reviewed legacy compatibility surface. Any new
// exported Store method must accept models.TenantScope/models.Principal or
// force an explicit review and fingerprint update. Contract/control-plane
// exceptions (migrations, health and test-only RawDB) are intentionally part
// of the frozen set and documented in docs/store-scope-contract.md.
// Native authentication adds six reviewed pre-principal methods:
// GetNativeIdentity, CreateNativeChallenge, ConsumeNativeChallenge,
// CreateNativeSession, RotateNativeIdentity, RevokeNativeSession. Their narrow
// proof/nonce boundaries and tenant binding are documented in the contract.
const legacyUnscopedStoreMethodSetSHA256 = "62821b0f6c413e8d15c5ed43ef9f10d8aea28b8d9e596f7c21600469207ce430"

func TestNoNewUnscopedStoreMethods(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		matched, err := build.Default.MatchFile(".", name)
		if err != nil {
			t.Fatal(err)
		}
		if !matched {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	var unsafe []string
	for _, file := range files {
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || !fn.Name.IsExported() || !storeReceiver(fn.Recv) {
				continue
			}
			if hasTypedTenantBoundary(fn.Type.Params) {
				continue
			}
			unsafe = append(unsafe, fn.Name.Name)
		}
	}
	sort.Strings(unsafe)
	sum := sha256.Sum256([]byte(strings.Join(unsafe, "\n")))
	got := hex.EncodeToString(sum[:])
	if got != legacyUnscopedStoreMethodSetSHA256 {
		t.Fatalf("exported unscoped Store surface changed: sha256=%s methods=%s", got, strings.Join(unsafe, ","))
	}
}

func storeReceiver(fields *ast.FieldList) bool {
	if fields == nil || len(fields.List) != 1 {
		return false
	}
	typeExpr := fields.List[0].Type
	if pointer, ok := typeExpr.(*ast.StarExpr); ok {
		typeExpr = pointer.X
	}
	identifier, ok := typeExpr.(*ast.Ident)
	return ok && identifier.Name == "Store"
}

func hasTypedTenantBoundary(fields *ast.FieldList) bool {
	if fields == nil {
		return false
	}
	for _, field := range fields.List {
		selector, ok := field.Type.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		pkg, ok := selector.X.(*ast.Ident)
		if ok && pkg.Name == "models" && (selector.Sel.Name == "TenantScope" ||
			selector.Sel.Name == "Principal" || selector.Sel.Name == "ControlPlanePrincipal" ||
			selector.Sel.Name == "BillingWebhookCredential" ||
			selector.Sel.Name == "ServiceAccountCredential" ||
			selector.Sel.Name == "VerifiedFederatedIdentityAssertion" ||
			selector.Sel.Name == "SupportAccessCredential" ||
			selector.Sel.Name == "OAuthCSRFCredential" ||
			selector.Sel.Name == "ConsoleSessionCredential" ||
			selector.Sel.Name == "InvitationCredential" ||
			selector.Sel.Name == "SignupCredential" ||
			selector.Sel.Name == "PasswordResetCredential" ||
			selector.Sel.Name == "WorkerPrincipal") {
			return true
		}
	}
	return false
}
