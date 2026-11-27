package tree

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
	vnode := typeSpec(t, "topology.go", "VNodeID")
	if vnode.Assign == 0 {
		t.Fatal("VNodeID must stay an alias of ID, not a second integer space")
	}
	id, ok := vnode.Type.(*ast.Ident)
	if !ok || id.Name != "ID" {
		t.Fatal("VNodeID must alias ID")
	}

	var v VNodeID
	var idVal ID = v
	if idVal != v {
		t.Fatal("VNodeID and ID diverged")
	}
}

func TestArchitectureCodecIsNotGeometry(t *testing.T) {
	src, err := os.ReadFile("codec.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "codec.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	banned := map[string]bool{"Owner": true, "StaticOwner": true, "XOR": true, "CommonPrefixLen": true, "LCP": true}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name == nil {
			continue
		}
		if banned[fn.Name.Name] {
			t.Fatalf("codec.go defines %s; geometry belongs in id.go", fn.Name.Name)
		}
	}
}

func TestArchitectureActiveOnlyInTopology(t *testing.T) {
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		if filepath.Base(path) == "topology.go" {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(src), ".Active =") {
			t.Fatalf("%s assigns V_active; only topology.go may", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestArchitectureNoHistoricalGeometry(t *testing.T) {
	banned := []string{"func StaticOwner", "type VSet", "NewVSet", "XORBytes("}
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || filepath.Base(path) == "architecture_test.go" {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(src)
		for _, name := range banned {
			if strings.Contains(text, name) {
				t.Fatalf("%s still mentions %s", path, name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
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

// The peer registry is a route to owners, never a second owner law.
func TestArchitectureTrieIsNotOwner(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "trie.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	banned := map[string]bool{"Owner": true, "WhereToWrite": true, "TargetNode": true}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && banned[fn.Name.Name] {
			t.Fatalf("trie.go defines %s; the owner is View.Owner", fn.Name.Name)
		}
	}
}
