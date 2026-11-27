package protocol

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchitectureIdentities(t *testing.T) {
	machine := typeSpec(t, "capacity.go", "MachineID")
	if machine.Assign != 0 {
		t.Fatal("MachineID must be a defined type, not an alias of string")
	}
	host, ok := machine.Type.(*ast.Ident)
	if !ok || host.Name != "string" {
		t.Fatal("MachineID underlying type must be string")
	}

	var m MachineID = "host"
	if string(m) != "host" {
		t.Fatal("MachineID conversion")
	}
}

func typeSpec(t *testing.T, file, name string) *ast.TypeSpec {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts := spec.(*ast.TypeSpec)
			if ts.Name.Name == name {
				return ts
			}
		}
	}
	t.Fatalf("%s has no type %s", file, name)
	return nil
}

// ARCH-3: adapters do not import the correctness kernel.
// ARCH-5: adapters do not define Owner.
func TestARCH_AdaptersAreTransport(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", "..", "internal", "adapter"))
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		// The issuer and the provisioner draw vnode names (tree.BASE64)
		// and nothing else: the facade would pull storage and mesh into them.
		names := strings.Contains(filepath.ToSlash(path), "internal/adapter/issuer/") || strings.Contains(filepath.ToSlash(path), "internal/adapter/provisioner/")
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			for _, kernel := range []string{"internal/protocol", "internal/tree"} {
				if kernel == "internal/tree" && names {
					continue
				}
				if p == "github.com/indexus/go-indexus-core/"+kernel {
					t.Fatalf("%s imports %s", path, kernel)
				}
			}
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Name != nil && (fn.Name.Name == "Owner" || fn.Name.Name == "ValidatePlan" || fn.Name.Name == "PlanHotSplit") {
				t.Fatalf("%s defines %s", path, fn.Name.Name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
