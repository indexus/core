package storage

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

type refusedEntropy struct{}

func (refusedEntropy) Read([]byte) (int, error) {
	return 0, errors.New("unexpected identity allocation on reopen")
}

func TestIdentityEntropyIsInjectedAndNotReadOnReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store")
	// The first identity belongs to zonelog; the second to durable authority.
	entropy := bytes.NewReader(append(bytes.Repeat([]byte{0x11}, 16), bytes.Repeat([]byte{0x22}, 16)...))
	s, err := OpenWithOptions(path, "D", "test", 1<<24, Options{Entropy: entropy})
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if id != strings.Repeat("22", 16) || entropy.Len() != 0 {
		t.Fatalf("identity source not used: id=%s remaining=%d", id, entropy.Len())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenWithOptions(path, "D", "test", 1<<24, Options{Entropy: refusedEntropy{}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	reopened, err := s.Identity()
	if err != nil || reopened != id {
		t.Fatalf("identity changed on recovery: %s %v", reopened, err)
	}
}

func TestIdentityEntropyFailureRefusesOpen(t *testing.T) {
	for _, size := range []int{0, 16, 24} {
		path := filepath.Join(t.TempDir(), "store")
		s, err := OpenWithOptions(path, "D", "test", 1<<24, Options{Entropy: bytes.NewReader(bytes.Repeat([]byte{1}, size))})
		if s != nil {
			_ = s.Close()
			t.Fatal("opened with incomplete identity entropy")
		}
		if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("size=%d error=%v", size, err)
		}
	}
}
