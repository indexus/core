package indexus

import (
	"os"
	"strings"
	"testing"
)

func TestArchitectureNoSecondOwnerLaw(t *testing.T) {
	t.Parallel()
	banned := []string{"func StaticOwner", "type VSet", "NewVSet", "XORBytes("}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || e.Name() == "architecture_test.go" {
			continue
		}
		src, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		text := string(src)
		for _, name := range banned {
			if strings.Contains(text, name) {
				t.Fatalf("%s still mentions %s", e.Name(), name)
			}
		}
		if strings.Contains(text, ".Active =") {
			t.Fatalf("%s assigns V_active", e.Name())
		}
	}
}
