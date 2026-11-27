package indexus

import (
	"errors"
	"testing"
)

// A key whose collection is shorter than its location cannot be encoded: the
// error must say so as ErrInvalidKey, so the HTTP layer answers 400.
func TestInvalidKeysAreClientErrors(t *testing.T) {
	t.Parallel()
	for _, item := range []*Item{
		{Collection: "short", Location: "abcdefghABCDEFGH", Id: "1"},
		{Collection: "exp0000000000001", Location: "", Id: "1"},
	} {
		if err := checkKey(item, BASE64.Root()); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("%+v: %v", item, err)
		}
	}
	if err := checkKey(&Item{Collection: "exp0000000000001", Location: "abcdefghABCDEFGH", Id: "1"}, BASE64.Root()); err != nil {
		t.Fatal(err)
	}
}
