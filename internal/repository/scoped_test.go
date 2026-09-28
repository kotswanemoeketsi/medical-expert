package repository

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/yourorg/medical-expert-system/internal/tenant"
)

// unscopedByDesign lists the only files allowed to hold a raw database
// handle, with the reason. Adding a file here is a design decision that
// weakens BR-0.3 and should be reviewed as one.
var unscopedByDesign = map[string]string{
	"platform_repository.go": "resolves which tenant a request belongs to before a scope can exist",
	"audit_repository.go":    "records events that have no tenant, and must never be filtered out by a scoping bug",
}

type sourceFile struct {
	name string
	file *ast.File
}

func parseRepositoryPackage(t *testing.T) (*token.FileSet, []sourceFile) {
	t.Helper()
	fset := token.NewFileSet()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var files []sourceFile
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		files = append(files, sourceFile{name: filepath.Base(p), file: f})
	}
	if len(files) < 3 {
		t.Fatalf("found only %d source files; is the test running in the package directory?", len(files))
	}
	return fset, files
}

// packageConstants maps every package-level constant and map variable name to
// its value expression, so statements assembled from constants can be folded
// back into the text the driver will see.
func packageConstants(files []sourceFile) (map[string]ast.Expr, map[string]*ast.CompositeLit) {
	consts := map[string]ast.Expr{}
	maps := map[string]*ast.CompositeLit{}
	for _, sf := range files {
		for _, decl := range sf.file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					switch gd.Tok {
					case token.CONST:
						consts[name.Name] = vs.Values[i]
					case token.VAR:
						if cl, ok := vs.Values[i].(*ast.CompositeLit); ok {
							if _, isMap := cl.Type.(*ast.MapType); isMap {
								maps[name.Name] = cl
							}
						}
					}
				}
			}
		}
	}
	return consts, maps
}

// fold evaluates a constant string expression. ok is false if any part of it
// is not a compile-time constant.
func fold(e ast.Expr, consts map[string]ast.Expr, depth int) (string, bool) {
	if depth > 20 {
		return "", false
	}
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.Ident:
		if c, ok := consts[v.Name]; ok {
			return fold(c, consts, depth+1)
		}
		return "", false
	case *ast.ParenExpr:
		return fold(v.X, consts, depth+1)
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		l, ok1 := fold(v.X, consts, depth+1)
		r, ok2 := fold(v.Y, consts, depth+1)
		return l + r, ok1 && ok2
	}
	return "", false
}

// looksLikeSQL is case-sensitive on purpose: statements in this codebase
// write keywords in capitals, and an error message such as "update failed"
// is not a statement.
func looksLikeSQL(s string) bool {
	fields := strings.Fields(s)
	for len(fields) > 0 && strings.HasPrefix(fields[0], "--") {
		fields = fields[1:]
	}
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "SELECT", "INSERT", "UPDATE", "DELETE", "REPLACE", "WITH":
		return true
	}
	return false
}

// sqlStatements returns every SQL statement in a file — literals, folded
// constants and map values — keyed by position, plus any SQL-looking text
// that could not be folded to a constant (dynamic SQL).
func sqlStatements(fset *token.FileSet, sf sourceFile, consts map[string]ast.Expr) (stmts map[string]string, dynamic []string) {
	stmts = map[string]string{}
	var visit func(n ast.Node, parentIsConcat bool)
	visit = func(n ast.Node, parentIsConcat bool) {
		if n == nil {
			return
		}
		switch v := n.(type) {
		case *ast.BinaryExpr:
			if v.Op == token.ADD {
				if !parentIsConcat {
					if s, ok := fold(v, consts, 0); ok {
						if looksLikeSQL(s) {
							stmts[fset.Position(v.Pos()).String()] = s
						}
					} else if containsSQLLiteral(v) {
						dynamic = append(dynamic, fset.Position(v.Pos()).String())
					}
				}
				visit(v.X, true)
				visit(v.Y, true)
				return
			}
		case *ast.BasicLit:
			if !parentIsConcat && v.Kind == token.STRING {
				if s, err := strconv.Unquote(v.Value); err == nil && looksLikeSQL(s) {
					stmts[fset.Position(v.Pos()).String()] = s
				}
			}
			return
		case *ast.CallExpr:
			// fmt.Sprintf("SELECT ...") and friends build SQL at runtime.
			if sel, ok := v.Fun.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "fmt" && len(v.Args) > 0 {
					if s, ok := fold(v.Args[0], consts, 0); ok && looksLikeSQL(s) {
						dynamic = append(dynamic, fset.Position(v.Pos()).String())
					}
				}
			}
		}
		ast.Inspect(n, func(child ast.Node) bool {
			if child == n {
				return true
			}
			visit(child, false)
			return false
		})
	}
	visit(sf.file, false)
	return stmts, dynamic
}

func containsSQLLiteral(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if s, err := strconv.Unquote(lit.Value); err == nil && looksLikeSQL(s) {
				found = true
			}
		}
		return !found
	})
	return found
}

// TestEveryScopedQueryPassesTheGuard runs every SQL statement in the package
// through tenant.Guard, the same check Scope applies at runtime — so a new
// unscoped query fails the build rather than a patient's privacy.
func TestEveryScopedQueryPassesTheGuard(t *testing.T) {
	fset, files := parseRepositoryPackage(t)
	consts, _ := packageConstants(files)
	checked := 0
	for _, sf := range files {
		stmts, dynamic := sqlStatements(fset, sf, consts)
		for _, pos := range dynamic {
			t.Errorf("%s: SQL assembled from non-constant parts; dynamic SQL cannot be verified and is banned", pos)
		}
		if _, exempt := unscopedByDesign[sf.name]; exempt {
			continue
		}
		for pos, s := range stmts {
			checked++
			if err := tenant.Guard(s); err != nil {
				t.Errorf("%s: %v", pos, err)
			}
		}
	}
	if checked < 20 {
		t.Fatalf("only %d statements found; the AST walk has probably stopped finding them", checked)
	}
}

// TestScopeCallsTakeConstantSQL checks the call sites as well as the
// statements: the text handed to Query, QueryRow or Exec must be a constant
// (or an entry in a package-level map of constants), never a variable that
// could have been built at runtime.
func TestScopeCallsTakeConstantSQL(t *testing.T) {
	fset, files := parseRepositoryPackage(t)
	consts, maps := packageConstants(files)
	for _, sf := range files {
		if _, exempt := unscopedByDesign[sf.name]; exempt {
			continue
		}
		ast.Inspect(sf.file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			switch sel.Sel.Name {
			case "Query", "QueryRow", "Exec":
			default:
				return true
			}
			arg := call.Args[1]
			if _, ok := fold(arg, consts, 0); ok {
				return true
			}
			if ix, ok := arg.(*ast.IndexExpr); ok {
				if id, ok := ix.X.(*ast.Ident); ok {
					if m, ok := maps[id.Name]; ok && mapOfConstants(m, consts) {
						return true
					}
				}
			}
			t.Errorf("%s: %s is called with SQL that is not a constant", fset.Position(call.Pos()), sel.Sel.Name)
			return true
		})
	}
}

func mapOfConstants(m *ast.CompositeLit, consts map[string]ast.Expr) bool {
	for _, elt := range m.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			return false
		}
		if _, ok := fold(kv.Value, consts, 0); !ok {
			return false
		}
	}
	return true
}

// TestOnlyPlatformFileHoldsRawDB: outside the documented exceptions, no
// repository may hold or receive a raw handle, or call the raw handle's
// methods. Without a handle there is no way to issue an unscoped query.
func TestOnlyPlatformFileHoldsRawDB(t *testing.T) {
	fset, files := parseRepositoryPackage(t)
	raw := map[string]bool{"DB": true, "Tx": true, "Conn": true}
	for _, sf := range files {
		if _, exempt := unscopedByDesign[sf.name]; exempt {
			continue
		}
		ast.Inspect(sf.file, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.SelectorExpr:
				if pkg, ok := v.X.(*ast.Ident); ok && pkg.Name == "sql" && raw[v.Sel.Name] {
					t.Errorf("%s: sql.%s appears in a tenant-scoped repository file", fset.Position(v.Pos()), v.Sel.Name)
				}
				switch v.Sel.Name {
				case "QueryContext", "QueryRowContext", "ExecContext", "PrepareContext", "BeginTx":
					t.Errorf("%s: raw handle method %s called in a tenant-scoped repository file", fset.Position(v.Pos()), v.Sel.Name)
				}
			}
			return true
		})
	}
}

// TestUnscopedFilesNeverUseTheScopeMarker is the reverse check. Those files
// run statements on the raw handle, which has no binder, so a marker there
// is a syntax error that only surfaces at runtime. A bulk edit once did
// exactly this to the tenants INSERT.
func TestUnscopedFilesNeverUseTheScopeMarker(t *testing.T) {
	_, files := parseRepositoryPackage(t)
	for _, sf := range files {
		if _, exempt := unscopedByDesign[sf.name]; !exempt {
			continue
		}
		ast.Inspect(sf.file, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && strings.Contains(lit.Value, tenant.Marker) {
				t.Errorf("%s: string literal contains %s but this file has no binder", sf.name, tenant.Marker)
			}
			return true
		})
	}
}

// The exemption list must name real files, or it quietly exempts nothing
// while looking like documentation.
func TestUnscopedByDesignFilesExist(t *testing.T) {
	_, files := parseRepositoryPackage(t)
	present := map[string]bool{}
	for _, sf := range files {
		present[sf.name] = true
	}
	for name := range unscopedByDesign {
		if !present[name] {
			t.Errorf("unscopedByDesign names %s, which does not exist", name)
		}
	}
}

func TestDuplicateKeyName(t *testing.T) {
	cases := map[string]string{
		"Duplicate entry 'a@b.co' for key 'uq_users_email'":       "uq_users_email",
		"Duplicate entry 'x-y' for key 'users.uq_users_email'":    "uq_users_email",
		"Duplicate entry '8001015009087' for key 'uq_patients_x'": "uq_patients_x",
		"something else": "",
	}
	for msg, want := range cases {
		if got := duplicateKeyName(msg); got != want {
			t.Errorf("duplicateKeyName(%q) = %q, want %q", msg, got, want)
		}
	}
}

func TestLikePrefixEscapesWildcards(t *testing.T) {
	if got := likePrefix(`50%_off\`); got != `50\%\_off\\%` {
		t.Fatalf("likePrefix escaped wrongly: %q", got)
	}
}
