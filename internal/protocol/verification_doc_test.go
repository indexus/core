package protocol

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestVerificationDocMatchesTests keeps verification.md and the named tests
// in step. A PROVED or EXPERIMENTAL row names at least one test that exists.
// A normative Test<ID>_ function is named in the doc.
func TestVerificationDocMatchesTests(t *testing.T) {
	root := moduleRoot(t)
	doc, err := os.ReadFile(filepath.Join(root, "docs/protocol/verification.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(doc)
	row := regexp.MustCompile(`^\| ([A-Z]+(?:-[A-Z]+)*-[0-9]+) \| (PROVED|PARTIAL|GAP|EXPERIMENTAL|OPERATIONAL) \| [^|]* \| ([^|]*) \|`)
	testName := regexp.MustCompile("`([^`]+)`")
	defined := testFuncs(t, root)
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		m := row.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		id, status, cell := m[1], m[2], m[3]
		seen[strings.ReplaceAll(id, "-", "")] = true
		var names []string
		for _, raw := range testName.FindAllStringSubmatch(cell, -1) {
			if strings.HasPrefix(raw[1], "Test") || strings.HasPrefix(raw[1], "Fuzz") {
				names = append(names, raw[1])
			}
		}
		switch status {
		case "PROVED", "EXPERIMENTAL", "OPERATIONAL":
			if len(names) == 0 {
				t.Errorf("%s is %s but names no test", id, status)
			}
		}
		for _, name := range names {
			if !defined[name] {
				t.Errorf("%s cites %s which is not defined", id, name)
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("no invariant rows parsed")
	}
	normative := regexp.MustCompile(`^func (Test[A-Z]+[0-9]+_[A-Za-z0-9_]+)\(`)
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.IsDir() {
			base := info.Name()
			if base == ".git" || base == ".local" || base == ".vscode" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if strings.Contains(path, "/docs/archive/") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, line := range strings.Split(string(body), "\n") {
			m := normative.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			name := m[1]
			id := name[len("Test"):]
			id = id[:strings.Index(id, "_")]
			if !seen[id] && !strings.Contains(text, name) {
				t.Errorf("%s is a normative test but %s is not in verification.md", name, id)
			}
		}
		return nil
	})
}

func testFuncs(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	re := regexp.MustCompile(`^func ((?:Test|Fuzz)[A-Za-z0-9_]+)\(`)
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.IsDir() {
			base := info.Name()
			if base == ".git" || base == ".local" || base == ".vscode" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, line := range strings.Split(string(body), "\n") {
			if m := re.FindStringSubmatch(line); m != nil {
				out[m[1]] = true
			}
		}
		return nil
	})
	return out
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
