// Package archtest checks house rules the compiler cannot (CLAUDE.md):
// which packages may import which, where SQL may name a table, and where
// HTTP clients and transactions may start. The checks read source, so a
// break fails the build's tests whether or not any test exercises it.
//
// Its own tests cover the whole module; SQL and Rule are for each package's
// planes test.
package archtest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Unknown stands in for any part of a string expression that is not a
// constant: a variable, a call, a parameter.
const Unknown = "⟨?⟩"

// String is one string expression in a package's source.
type String struct {
	Pos  string // file:line
	Func string // the enclosing function or method, "" outside one
	Text string // literals and same-package constants folded; anything else is Unknown
}

// Strings returns every string expression in dir's non-test Go files. A
// concatenation is folded into one String, so SQL split across literals
// (`SELECT `+cols+` FROM t`) reads as written; its parts are also listed
// on their own.
func Strings(dir string) ([]String, error) {
	fset := token.NewFileSet()
	var files []*ast.File
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	consts := map[string]ast.Expr{}
	for _, f := range files {
		for _, d := range f.Decls {
			if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.CONST {
				for _, sp := range g.Specs {
					vs := sp.(*ast.ValueSpec)
					for i, name := range vs.Names {
						if i < len(vs.Values) {
							consts[name.Name] = vs.Values[i]
						}
					}
				}
			}
		}
	}
	var fold func(e ast.Expr, depth int) string
	fold = func(e ast.Expr, depth int) string {
		if depth > 20 {
			return Unknown
		}
		switch e := e.(type) {
		case *ast.BasicLit:
			if e.Kind == token.STRING {
				if s, err := strconv.Unquote(e.Value); err == nil {
					return s
				}
			}
		case *ast.ParenExpr:
			return fold(e.X, depth+1)
		case *ast.BinaryExpr:
			if e.Op == token.ADD {
				return fold(e.X, depth+1) + fold(e.Y, depth+1)
			}
		case *ast.Ident:
			if v, ok := consts[e.Name]; ok {
				return fold(v, depth+1)
			}
		}
		return Unknown
	}
	var out []String
	for _, f := range files {
		for _, d := range f.Decls {
			name := ""
			if fd, ok := d.(*ast.FuncDecl); ok {
				name = fd.Name.Name
			}
			ast.Inspect(d, func(n ast.Node) bool {
				switch e := n.(type) {
				case *ast.BasicLit:
					if e.Kind != token.STRING {
						return true
					}
				case *ast.BinaryExpr:
					if e.Op != token.ADD {
						return true
					}
				default:
					return true
				}
				p := fset.Position(n.Pos())
				out = append(out, String{Pos: fmt.Sprintf("%s:%d", filepath.Base(p.Filename), p.Line), Func: name, Text: fold(n.(ast.Expr), 0)})
				return true
			})
		}
	}
	return out, nil
}

// unresolved is SQL that names its table through something not constant,
// a variable or a format verb, which no rule can check. Case-sensitive:
// blygd writes SQL keywords in capitals, and prose says "from %s".
var unresolved = regexp.MustCompile(`\b(FROM|JOIN|INTO|UPDATE)\s+(` + regexp.QuoteMeta(Unknown) + `|%)`)

// Rule bans SQL that matches Banned anywhere in a package except in the
// functions Allowed names. Why is the failure message.
type Rule struct {
	Banned  *regexp.Regexp
	Allowed []string
	Why     string
}

// Check applies rules to dir's source, and fails on any SQL whose table
// it cannot read.
func Check(t testing.TB, dir string, rules ...Rule) {
	t.Helper()
	strs, err := Strings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(strs) == 0 {
		t.Fatalf("no strings found in %s", dir)
	}
	for _, s := range strs {
		if m := unresolved.FindString(s.Text); m != "" {
			t.Errorf("%s (%s): %q names its table through a variable, so the planes rule cannot check it; use a constant", s.Pos, s.Func, m)
		}
		for _, r := range rules {
			m := r.Banned.FindString(s.Text)
			if m == "" || contains(r.Allowed, s.Func) {
				continue
			}
			t.Errorf("%s (%s): %q: %s", s.Pos, s.Func, m, r.Why)
		}
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
