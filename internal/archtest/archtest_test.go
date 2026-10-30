package archtest

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/chrisbodhi/blygd/"

// imports is every package's allowed internal imports. A package missing
// here fails the test, so each new edge is a decision made in review, not
// an accident. The planes rest on it: a planes test reads one package's
// SQL, and this says which other packages' SQL can run on its behalf.
var imports = map[string][]string{
	"cmd/blygd":         {"account", "config", "db", "web"},
	"internal/web":      {"account", "apperr", "blyg", "config", "events", "fetch", "hopper", "limit", "mention", "reader", "studio"}, // New is the composition root
	"internal/studio":   {"apperr", "blyg", "events", "fetch", "surface", "tx"},                                                       // writes the surface: nothing that reads the mirror; events only sends hints
	"internal/reader":   {"apperr", "batch", "blyg", "events", "fetch", "studio", "tx"},
	"internal/mention":  {"apperr", "batch", "blyg", "events", "fetch", "studio", "surface", "tx"},
	"internal/hopper":   {"apperr", "blyg", "tx"},
	"internal/account":  {"apperr", "blyg", "tx"},
	"internal/config":   {"blyg"},
	"internal/surface":  {"blyg", "fetch"},
	"internal/fetch":    {"tx"},
	"internal/blyg":     {}, // the protocol is pure
	"internal/tx":       {},
	"internal/apperr":   {},
	"internal/db":       {},
	"internal/batch":    {},
	"internal/events":   {},
	"internal/limit":    {},
	"internal/archtest": {},
	"internal/testdb":   {"db"},
	"internal/testblyg": {"blyg"},
}

// mirror is the packages that query the import mirror or the overlay.
var mirror = []string{"reader", "hopper", "mention"}

type pkg struct {
	ImportPath string
	Dir        string
	Imports    []string
	Deps       []string
	GoFiles    []string
}

func packages(t *testing.T) []pkg {
	t.Helper()
	cmd := exec.Command("go", "list", "-json", module+"...")
	cmd.Dir = filepath.Join("..", "..")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	var ps []pkg
	for d := json.NewDecoder(strings.NewReader(string(out))); ; {
		var p pkg
		if err := d.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		ps = append(ps, p)
	}
	return ps
}

func internal(paths []string) []string {
	var out []string
	for _, p := range paths {
		if rest, ok := strings.CutPrefix(p, module+"internal/"); ok {
			out = append(out, rest)
		}
	}
	return out
}

func TestImportGraph(t *testing.T) {
	ps := packages(t)
	seen := map[string]bool{}
	for _, p := range ps {
		name := strings.TrimPrefix(p.ImportPath, module)
		seen[name] = true
		allowed, ok := imports[name]
		if !ok {
			t.Errorf("%s is not in archtest's import table: add it with the internal packages it may import", name)
			continue
		}
		for _, im := range internal(p.Imports) {
			if !slices.Contains(allowed, im) {
				t.Errorf("%s imports internal/%s, which its row in archtest's table does not allow", name, im)
			}
		}
	}
	for name := range imports {
		if !seen[name] {
			t.Errorf("archtest's import table lists %s, which no longer exists", name)
		}
	}
	// The property the table is for, stated outright: nothing that writes
	// the surface can reach a package that reads the mirror (§13.5).
	for _, p := range ps {
		if p.ImportPath != module+"internal/studio" {
			continue
		}
		for _, d := range internal(p.Deps) {
			if slices.Contains(mirror, d) {
				t.Errorf("internal/studio reaches internal/%s, which reads the import mirror", d)
			}
		}
	}
}

type file struct {
	f     *ast.File
	names map[string]string // local name → import path, for the imports asked for
}

// source parses every non-test Go file in the module, keyed by its path
// from the module root.
func source(t *testing.T, fset *token.FileSet, want ...string) map[string]file {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]file{}
	for _, p := range packages(t) {
		for _, g := range p.GoFiles {
			path := filepath.Join(p.Dir, g)
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			names := map[string]string{}
			for _, im := range f.Imports {
				ip, _ := strconv.Unquote(im.Path.Value)
				if !slices.Contains(want, ip) {
					continue
				}
				local := filepath.Base(ip)
				if strings.HasPrefix(ip, "github.com/jackc/pgx/v5") && local == "v5" {
					local = "pgx"
				}
				if im.Name != nil {
					local = im.Name.Name
				}
				names[local] = ip
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				t.Fatal(err)
			}
			out[rel] = file{f, names}
		}
	}
	return out
}

// selectors calls fn for each pkg.Name selector on an import in want, and
// says whether it names the type of a composite literal (http.Client{…}).
func selectors(t *testing.T, want []string, fn func(at, importPath, name string, lit bool)) {
	t.Helper()
	fset := token.NewFileSet()
	for path, s := range source(t, fset, want...) {
		ast.Inspect(s.f, func(n ast.Node) bool {
			var sel *ast.SelectorExpr
			lit := false
			switch e := n.(type) {
			case *ast.CompositeLit:
				sel, lit = e.Type.(*ast.SelectorExpr)
			case *ast.SelectorExpr:
				sel = e
			}
			if sel == nil {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok {
				if ip, ok := s.names[id.Name]; ok {
					fn(path+":"+strconv.Itoa(fset.Position(sel.Pos()).Line), ip, sel.Sel.Name, lit)
				}
			}
			return true
		})
	}
}

func in(at, dir string) bool { return strings.HasPrefix(at, dir+string(os.PathSeparator)) }

// All outbound HTTP goes through internal/fetch, which bounds it and
// refuses private addresses (CLAUDE.md, §14). Nothing else may use
// net/http's default client or helpers, build a client or transport, or
// dial.
func TestOutboundHTTPOnlyThroughFetch(t *testing.T) {
	defaults := []string{"Get", "Head", "Post", "PostForm", "DefaultClient", "DefaultTransport"}
	selectors(t, []string{"net/http", "net"}, func(at, ip, name string, lit bool) {
		if in(at, filepath.Join("internal", "fetch")) {
			return
		}
		switch {
		case ip == "net/http" && slices.Contains(defaults, name),
			ip == "net/http" && lit && (name == "Client" || name == "Transport"),
			ip == "net" && strings.HasPrefix(name, "Dial"):
			t.Errorf("%s: %s.%s: fetch through internal/fetch", at, filepath.Base(ip), name)
		}
	})
}

// Every transaction starts in tx.Run, which marks its context so
// internal/fetch can refuse network inside it (CLAUDE.md).
func TestTransactionsStartInTx(t *testing.T) {
	tx := filepath.Join("internal", "tx")
	selectors(t, []string{"github.com/jackc/pgx/v5"}, func(at, _, name string, _ bool) {
		if (name == "BeginTxFunc" || name == "BeginFunc") && !in(at, tx) {
			t.Errorf("%s: pgx.%s: start transactions with tx.Run", at, name)
		}
	})
	// A pool's or connection's own Begin and BeginTx are methods, not
	// package selectors, so look for the calls by name.
	fset := token.NewFileSet()
	for path, s := range source(t, fset) {
		if in(path, tx) {
			continue
		}
		ast.Inspect(s.f, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if sel, ok := c.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Begin" || sel.Sel.Name == "BeginTx") {
					t.Errorf("%s:%d: .%s: start transactions with tx.Run", path, fset.Position(c.Pos()).Line, sel.Sel.Name)
				}
			}
			return true
		})
	}
}
