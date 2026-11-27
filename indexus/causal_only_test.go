package indexus

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/wire"
)

// An unconfigured facade must never accept data into a fallback implementation.
func TestNodeRequiresDurableRuntime(t *testing.T) {
	t.Parallel()
	settings, err := NewSettings("AAAAAAAAAAAAAAAB", 9000, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	node, err := NewNode(settings, testNewContact, nil)
	if err != nil {
		t.Fatal(err)
	}
	if node.ClientReady() {
		t.Fatal("unconfigured node is ready")
	}
	row := &Item{Collection: "Demo000000000000", Location: "aa", Id: "row", RequestID: "put"}
	for name, op := range map[string]func() error{
		"insert": func() error { return node.New(row, "@", nil) },
		"delete": func() error { return node.Delete(row, "@", nil) },
		"get":    func() error { _, _, err := node.Get("Demo000000000000", "@", true, nil, false); return err },
		"batch": func() error {
			_, err := node.GetSetsBlockContext(context.Background(), "Demo000000000000", []string{"@"}, true, nil, false, nil, wire.SetsBlockF32, nil)
			return err
		},
		"aggregates": func() error { _, err := node.GetAggregates("Demo000000000000", []string{"@"}); return err },
		"count":      func() error { _, err := node.Count(); return err },
		"ownership":  func() error { _, err := node.Ownership(); return err },
		"refresh":    node.Refresh, "update": node.Update,
	} {
		t.Run(name, func(t *testing.T) {
			if err := op(); !errors.Is(err, ErrNotReady) {
				t.Fatalf("got %v, want ErrNotReady", err)
			}
		})
	}
	if node.Items() != 0 {
		t.Fatal("unconfigured node retained rows")
	}
}

// Removing the initial legacy view must not admit the reserved zero identity.
func TestNodeRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", BASE64.Root(), EncodeName(ID{})} {
		settings, err := NewSettings(name, 9000, time.Second)
		if err != nil {
			continue
		}
		if _, err = NewNode(settings, testNewContact, nil); err == nil {
			t.Fatalf("accepted reserved identity %q", name)
		}
	}
	if _, err := NewNode(nil, testNewContact, nil); err == nil {
		t.Fatal("accepted nil settings")
	}
	settings, err := NewSettings("AAAAAAAAAAAAAAAB", 9000, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = NewNode(settings, nil, nil); err == nil {
		t.Fatal("accepted nil contact factory")
	}
}
