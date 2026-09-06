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
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// packagesThatSendMail is every package allowed to address a message.
//
// It is an allow-list rather than a search result: a new package that starts
// sending email has to be added here, and adding it is the moment somebody
// notices its messages are not in Tags or in the sample. internal/mailsample
// is on it because it builds one of everything for the guard.
var packagesThatSendMail = map[string]bool{
	"mail":       true,
	"auth":       true,
	"reports":    true,
	"mailsample": true,
}

// TestNoSenderInventsItsOwnTag walks the source for a tag written as a literal
// at a send site and refuses one Tags does not know about. A message outside
// the inventory is a message no guard checks.
func TestNoSenderInventsItsOwnTag(t *testing.T) {
	known := map[string]bool{}
	for _, tag := range Tags() {
		known[tag] = true
	}

	for _, dir := range sendingDirs(t) {
		for where, tag := range literalTags(t, dir) {
			if !known[tag] {
				t.Errorf("%s sends %q, which is not in mail.Tags", where, tag)
			}
		}
	}
}

// TestOnlyTheKnownPackagesSendMail is the guard the literal scan cannot be on
// its own.
//
// A sender that names its tag with a constant is invisible to a scan for
// literals, so the thing worth catching is not the tag — it is a package that
// has started sending email at all. Every one of those has messages that need
// to be in Tags and in the sample.
func TestOnlyTheKnownPackagesSendMail(t *testing.T) {
	for _, dir := range sendingDirs(t) {
		name := filepath.Base(dir)

		if !packagesThatSendMail[name] {
			t.Errorf("internal/%s addresses a mail message. Add its messages to mail.Tags and to "+
				"internal/mailsample, then add it to packagesThatSendMail.", name)
		}
	}
}

// sendingDirs lists every package under internal that addresses a message.
func sendingDirs(t *testing.T) []string {
	t.Helper()

	const root = ".."

	dirs := []string{}

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !entry.IsDir() {
			return nil
		}

		// The root of the walk is "..", which would otherwise look like a
		// hidden directory and skip the whole tree.
		if path != root && (entry.Name() == "testdata" || strings.HasPrefix(entry.Name(), ".")) {
			return fs.SkipDir
		}

		if len(addressedIn(t, path)) > 0 {
			dirs = append(dirs, path)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk internal: %v", err)
	}

	return dirs
}

// addressedIn finds every call that turns copy into an addressed message.
func addressedIn(t *testing.T, dir string) []ast.Expr {
	t.Helper()

	tags := []ast.Expr{}

	forEachMessageCall(t, dir, func(_ *token.FileSet, _ string, call *ast.CallExpr) {
		tags = append(tags, call.Args[1])
	})

	return tags
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

	found := map[string]string{}

	forEachMessageCall(t, dir, func(fset *token.FileSet, path string, call *ast.CallExpr) {
		literal, ok := call.Args[1].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return
		}

		tag, err := strconv.Unquote(literal.Value)
		if err != nil {
			return
		}

		found[filepath.Base(path)+":"+strconv.Itoa(fset.Position(literal.Pos()).Line)] = tag
	})

	return found
}

// forEachMessageCall visits every `x.Message(to, tag)` in one directory. The
// source is read rather than imported, because internal/mail must not depend on
// the packages that use it.
func forEachMessageCall(t *testing.T, dir string, visit func(*token.FileSet, string, *ast.CallExpr)) {
	t.Helper()

	fset := token.NewFileSet()

	packages, err := parser.ParseDir(fset, dir, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}

	for _, pkg := range packages {
		for path, file := range pkg.Files {
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || len(call.Args) != 2 {
					return true
				}

				if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Message" {
					visit(fset, path, call)
				}

				return true
			})
		}
	}
}

// tagConstants reads every Tag… constant in this package, from every file in
// it, so one declared beside a new sender is seen as well.
func tagConstants(t *testing.T) map[string]string {
	t.Helper()

	fset := token.NewFileSet()

	packages, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parse the package: %v", err)
	}

	found := map[string]string{}
	declarations := []ast.Decl{}

	for _, pkg := range packages {
		for _, file := range pkg.Files {
			declarations = append(declarations, file.Decls...)
		}
	}

	for _, declaration := range declarations {
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
		t.Fatal("no Tag constants were found in the package")
	}

	return found
}
