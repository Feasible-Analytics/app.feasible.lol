//
// tags_test.go
// That the inventory of messages is the whole inventory.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mail

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// packagesThatSendMail is every directory holding a call to Message. The scan
// below reads them as source rather than importing them, because internal/mail
// must not depend on the packages that use it.
var packagesThatSendMail = []string{".", "../auth", "../reports"}

// TestNoSenderInventsItsOwnTag walks the source for a tag written as a literal
// at a send site and refuses one Tags does not know about.
//
// This is what stops the next email being added without being added to the
// inventory — which is how six messages came to be sent with no postal address
// and no test that noticed.
func TestNoSenderInventsItsOwnTag(t *testing.T) {
	known := map[string]bool{}
	for _, tag := range Tags() {
		known[tag] = true
	}

	for _, dir := range packagesThatSendMail {
		for where, tag := range literalTags(t, dir) {
			if !known[tag] {
				t.Errorf("%s sends %q, which is not in mail.Tags", where, tag)
			}
		}
	}
}

// TestEveryTagConstantIsInTheInventory catches the other half: a constant added
// beside the rest and never listed.
func TestEveryTagConstantIsInTheInventory(t *testing.T) {
	listed := map[string]bool{}
	for _, tag := range Tags() {
		listed[tag] = true
	}

	for name, value := range tagConstants(t) {
		if !listed[value] {
			t.Errorf("%s = %q is not in mail.Tags", name, value)
		}
	}
}

// TestATagIsNeverReused keeps two messages from being reported as one.
func TestATagIsNeverReused(t *testing.T) {
	seen := map[string]bool{}

	for _, tag := range Tags() {
		if seen[tag] {
			t.Errorf("%q is in mail.Tags twice", tag)
		}

		seen[tag] = true
	}
}

// literalTags finds every string literal handed to Message as its tag, keyed by
// where it was written.
func literalTags(t *testing.T, dir string) map[string]string {
	t.Helper()

	fset := token.NewFileSet()

	packages, err := parser.ParseDir(fset, dir, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}

	found := map[string]string{}

	for _, pkg := range packages {
		for path, file := range pkg.Files {
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || len(call.Args) != 2 {
					return true
				}

				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "Message" {
					return true
				}

				literal, ok := call.Args[1].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return true
				}

				tag, err := strconv.Unquote(literal.Value)
				if err != nil {
					return true
				}

				found[filepath.Base(path)+":"+strconv.Itoa(fset.Position(literal.Pos()).Line)] = tag

				return true
			})
		}
	}

	return found
}

// tagConstants reads the Tag… constants out of this package's own source.
func tagConstants(t *testing.T) map[string]string {
	t.Helper()

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "tags.go", nil, 0)
	if err != nil {
		t.Fatalf("parse tags.go: %v", err)
	}

	found := map[string]string{}

	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}

		for _, spec := range general.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) != 1 || len(value.Values) != 1 {
				continue
			}

			if !strings.HasPrefix(value.Names[0].Name, "Tag") {
				continue
			}

			literal, ok := value.Values[0].(*ast.BasicLit)
			if !ok {
				continue
			}

			unquoted, err := strconv.Unquote(literal.Value)
			if err != nil {
				continue
			}

			found[value.Names[0].Name] = unquoted
		}
	}

	if len(found) == 0 {
		t.Fatal("no Tag constants were found in tags.go")
	}

	return found
}
