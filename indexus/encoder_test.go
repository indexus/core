// Public encoder and aggregate APIs are exercised from the client package.
package indexus_test

import (
	"testing"

	indexus "github.com/indexus/go-indexus-core/indexus"
)

func TestChildCandidatesRootAndNested(t *testing.T) {
	t.Parallel()
	base := indexus.BASE64
	rootKids := indexus.ChildCandidates(base, base.Root())
	if len(rootKids) != base.Length() {
		t.Fatalf("root candidates=%d want %d", len(rootKids), base.Length())
	}
	for _, k := range rootKids {
		if len(k) != 1 {
			t.Fatalf("root child %q want len 1", k)
		}
		if base.Parent(k) != base.Root() {
			t.Fatalf("Parent(%q)=%q want root", k, base.Parent(k))
		}
	}

	nested := indexus.ChildCandidates(base, "w")
	if len(nested) != base.Length() {
		t.Fatalf("nested candidates=%d want %d", len(nested), base.Length())
	}
	for _, k := range nested {
		if base.Parent(k) != "w" {
			t.Fatalf("Parent(%q)=%q want w", k, base.Parent(k))
		}
	}
}

func TestIsDirectChild(t *testing.T) {
	t.Parallel()
	base := indexus.BASE64
	if !indexus.IsDirectChild(base, base.Root(), "a") {
		t.Fatal("root→a should be direct")
	}
	if indexus.IsDirectChild(base, base.Root(), "ab") {
		t.Fatal("root→ab is not direct")
	}
	if indexus.IsDirectChild(base, "a", "a") {
		t.Fatal("a self-key must be rejected")
	}
	if !indexus.IsDirectChild(base, "a", "ab") {
		t.Fatal("a→ab should be direct")
	}
	// The edges traverse must refuse: a sibling branch one level down looks
	// close but has a different parent, and following it builds a cycle.
	if indexus.IsDirectChild(base, "abcd", "abce") {
		t.Fatal("abce is a sibling branch, its parent is abc")
	}
	if !indexus.IsDirectChild(base, "abcd", "abcde") {
		t.Fatal("abcde should be a direct child of abcd")
	}
	// Every letter is an ordinary location. Reserving one as a marker hides
	// every zone whose name starts with it (P4).
	for i := 0; i < base.Length(); i++ {
		child := base.CharAt(i)
		if !indexus.IsDirectChild(base, base.Root(), child) {
			t.Fatalf("%q is a letter of the alphabet, so a child of the root", child)
		}
	}
}
