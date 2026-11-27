package protocol

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlanner_NoGlobalOracleAPI(t *testing.T) {
	forbidden := []string{"AllMachines(", "AllVNodes(", "GlobalLoads(", "AllHosts("}
	root := "."
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		src := string(body)
		for _, bad := range forbidden {
			if strings.Contains(src, bad) {
				t.Fatalf("%s uses global oracle %s", name, bad)
			}
		}
	}
}
