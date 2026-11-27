package architecture

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func moduleRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
}

// The node module requires no module: go.mod has no require and go.sum
// holds nothing.
func TestModuleRequiresNothing(t *testing.T) {
	root := moduleRoot()
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(mod), "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && (fields[0] == "require" || fields[0] == "replace" || fields[0] == "tool") {
			t.Errorf("go.mod: %s", line)
		}
	}
	if sum, err := os.ReadFile(filepath.Join(root, "go.sum")); err == nil && len(strings.TrimSpace(string(sum))) > 0 {
		t.Errorf("go.sum is not empty")
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestProductionDependencyBoundaries(t *testing.T) {
	root := moduleRoot()
	const module = "github.com/indexus/go-indexus-core/"
	allowed := map[string][]string{
		"internal/tree": {},
		// The summary algebra and the collection description (IXC1): column
		// kinds, the space, canonical bytes and names. Pure, like tree.
		"internal/algebra":  {},
		"internal/protocol": {"internal/tree"},
		"internal/causal":   {"internal/tree", "internal/protocol"},
		"internal/zonelog":  {},
		"internal/storage":  {"internal/tree", "internal/protocol", "internal/causal", "internal/zonelog", "internal/prep", "internal/algebra"},
		"internal/mesh":     {"internal/tree", "internal/protocol", "internal/causal", "internal/storage", "internal/metrics", "internal/replication/convergent"},
		// The convergent replication core: R(z), versions and the merge rule
		// over tree identities only. The mesh wires it to the store.
		"internal/replication/convergent": {"internal/tree"},
		"internal/auth":                   {},
		// Process-wide counters for GET /metrics: atomics, no store.
		"internal/metrics": {},
		// The bulk file a publisher hands a node: a format, no store.
		"internal/prep": {"internal/tree"},
		// Process capacity requests: resource samples and the provisioner
		// client. No store, no write authority.
		"internal/autoscale": {"internal/tree"},
		// The wire formats of the node API (IXB2, IXRB) and the values they
		// carry: pure codecs, which the HTTP adapters also use.
		"internal/wire": {"internal/tree"},
		// Derived zone summaries: read from the store, pushed and polled
		// through the mesh. A read accelerator, never write authority.
		"internal/summary": {"internal/tree", "internal/causal", "internal/storage", "internal/mesh", "internal/wire", "internal/algebra"},
		"indexus":          {"internal/tree", "internal/protocol", "internal/causal", "internal/storage", "internal/mesh", "internal/auth", "internal/metrics", "internal/autoscale", "internal/wire", "internal/summary", "internal/algebra"},
	}
	// Offline tools may open a stopped database directly. They are not
	// shipped, so no production package may depend on them.
	tools := []string{"internal/tree", "internal/protocol", "internal/causal", "internal/storage", "internal/prep"}
	// Shipped binaries reach the node through the facade and the adapters.
	commandForbidden := []string{"internal/mesh", "internal/causal", "internal/storage", "internal/protocol", "internal/tree"}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			// A nested module (gateway/) is not linked into this one.
			if _, err := os.Stat(filepath.Join(path, "go.mod")); path != root && err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		test := strings.HasSuffix(path, "_test.go")
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		layer := filepath.ToSlash(filepath.Dir(relative))
		imports, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, item := range imports.Imports {
			name, err := strconv.Unquote(item.Path.Value)
			if err != nil {
				return err
			}
			// The module links the standard library and itself, nothing
			// else, tests included: no exception. The HTTP/3 edge is the
			// gateway module.
			if external := strings.Contains(strings.Split(name, "/")[0], "."); external && !strings.HasPrefix(name, module) {
				t.Errorf("%s: external dependency %s", relative, name)
			}
			if test {
				continue
			}
			isTool := strings.HasPrefix(layer, "tools/")
			if !isTool && strings.HasPrefix(name, module+"tools/") {
				t.Errorf("%s: production code imports the tool %s", relative, strings.TrimPrefix(name, module))
			}
			if strings.HasPrefix(layer, "cmd/") {
				for _, forbidden := range commandForbidden {
					if name == module+forbidden {
						t.Errorf("%s: a shipped binary imports %s instead of the facade", relative, forbidden)
					}
				}
			}
			dependencies, checked := allowed[layer]
			if isTool {
				dependencies, checked = tools, true
			}
			if !checked {
				continue
			}
			if strings.HasPrefix(name, module) {
				dependency := strings.TrimPrefix(name, module)
				found := false
				for _, permit := range dependencies {
					if dependency == permit {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("%s: dependency %s crosses the %s boundary", relative, dependency, layer)
				}
			}
			// The address algebra is the contract shared with the JS client:
			// the standard library only.
			if (layer == "internal/tree" || layer == "internal/algebra") && strings.Contains(strings.Split(name, "/")[0], ".") {
				t.Errorf("%s: %s depends only on the standard library, not %s", relative, layer, name)
			}
			if layer == "internal/tree" || layer == "internal/algebra" || layer == "internal/protocol" || layer == "internal/causal" {
				if name == "os" || name == "syscall" || name == "database/sql" || strings.HasPrefix(name, "os/") || name == "net" || strings.HasPrefix(name, "net/") {
					t.Errorf("%s: pure protocol facts cannot depend on external IO (%s)", relative, name)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
