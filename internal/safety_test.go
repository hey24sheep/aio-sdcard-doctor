package internal_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// writers are package functions that can modify a file system or device.
var writers = map[string]map[string]bool{
	"os":      set("Create WriteFile Remove RemoveAll Rename Mkdir MkdirAll MkdirTemp CreateTemp Chtimes Truncate Symlink Link Chmod Chown Lchown"),
	"syscall": set("Unlink Rmdir Rename Mkdir Truncate Ftruncate"),
	"unix":    set("Unlink Rmdir Rename Mkdir Truncate Ftruncate Pwrite Write"),
	"diskfs":  set("Open Create"),
}

// openers are allowed only with read-only flags / access.
var openers = map[string]map[string]bool{
	"os":      set("OpenFile"),
	"syscall": set("Open"),
	"unix":    set("Open Openat"),
	"windows": set("CreateFile Open"),
}

var writeFlags = set("O_WRONLY O_RDWR O_CREATE O_CREAT O_TRUNC O_APPEND O_EXCL GENERIC_WRITE GENERIC_ALL")

// allowed: the guarded writer, the gated fixer, and app-state files that never live on a source.
var allowedDirs = []string{"internal/destfs/", "internal/fix/", "internal/testutil/"}                // testutil: test fixtures only
var allowedFiles = set("internal/job/store.go internal/job/sourcelock_unix.go internal/gui/lock.go") // gui/lock.go: gui.lock in the user's config folder

func set(s string) map[string]bool {
	m := map[string]bool{}
	for _, f := range strings.Fields(s) {
		m[f] = true
	}
	return m
}

func idents(e ast.Expr) []string {
	var out []string
	ast.Inspect(e, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Ident:
			out = append(out, x.Name)
		case *ast.BasicLit:
			out = append(out, x.Value)
		}
		return true
	})
	return out
}

// readOnly reports whether a flag/access expression is made only of known read-only parts.
func readOnly(pkg string, e ast.Expr) bool {
	for _, id := range idents(e) {
		if writeFlags[id] {
			return false
		}
		if pkg == "windows" && !(id == "windows" || id == "GENERIC_READ" || id == "0") {
			return false
		}
		if pkg != "windows" && !(id == "os" || id == "syscall" || id == "unix" || strings.HasPrefix(id, "O_") || id == "0") {
			return false // a variable: can't prove it is read-only
		}
	}
	return true
}

// violations returns write calls in one parsed file.
func violations(fset *token.FileSet, f *ast.File) []string {
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		name := sel.Sel.Name
		bad := writers[pkg.Name][name]
		if openers[pkg.Name][name] {
			arg := 1 // OpenFile(name, flag, perm), Open(path, mode, perm), CreateFile(name, access, …)
			if name == "Openat" {
				arg = 2
			}
			bad = len(call.Args) <= arg || !readOnly(pkg.Name, call.Args[arg])
		}
		if bad {
			out = append(out, fset.Position(call.Pos()).String()+": "+pkg.Name+"."+name)
		}
		return true
	})
	return out
}

func TestScannerCatchesWrites(t *testing.T) {
	src := `package x
import ("os"; "golang.org/x/sys/windows")
func f(flags int) {
	os.Create("a")
	os.OpenFile("a", os.O_RDWR|os.O_CREATE, 0)
	os.OpenFile("a", flags, 0)
	windows.CreateFile(nil, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, 0, 0, 0)
	os.OpenFile("a", os.O_RDONLY, 0)
	windows.CreateFile(nil, windows.GENERIC_READ, 0, nil, 0, 0, 0)
	windows.CreateFile(nil, 0, 0, nil, 0, 0, 0)
	os.Open("a")
}`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	if v := violations(fset, f); len(v) != 4 {
		t.Fatalf("want 4 violations, got %d: %v", len(v), v)
	}
}

// TestOnlyDestfsAndFixWrite: nothing but destfs and fix may write (plus two app-state files).
func TestOnlyDestfsAndFixWrite(t *testing.T) {
	root, _ := filepath.Abs("..")
	fset := token.NewFileSet()
	var bad []string
	n := 0
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(strings.TrimPrefix(p, root+string(filepath.Separator)))
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" || d.Name() == "docs" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") || allowedFiles[rel] {
			return nil
		}
		for _, a := range allowedDirs {
			if strings.HasPrefix(rel, a) {
				return nil
			}
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return err
		}
		n++
		bad = append(bad, violations(fset, f)...)
		return nil
	})
	if n == 0 {
		t.Fatal("scanned no files")
	}
	for _, b := range bad {
		t.Error(b)
	}
}
