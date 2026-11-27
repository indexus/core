package testkit

import (
	"bytes"
	"io"
	"math/rand"
	"testing"
)

func powerImage(t *testing.T, fs *PowerFS) []byte {
	t.Helper()
	names, _ := fs.List()
	var out []byte
	for _, name := range names {
		f, err := fs.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		n, _ := f.Size()
		raw := make([]byte, n)
		if _, err := f.ReadAt(raw, 0); err != nil && err != io.EOF {
			t.Fatal(err)
		}
		out = append(out, []byte(name+"\x00")...)
		out = append(out, raw...)
	}
	return out
}

func TestPowerLossReplaysTheSameSeed(t *testing.T) {
	fs := NewPowerFS()
	for _, name := range []string{"a.log", "b.log", "c.log", "d.log"} {
		f, _ := fs.Create(name)
		_, _ = f.Write([]byte("durable:" + name))
		_ = f.Sync()
		_, _ = f.Write(bytes.Repeat([]byte(name), 20))
	}
	_ = fs.Sync()
	want := powerImage(t, fs.Crash(rand.New(rand.NewSource(7)), true))
	for range 100 {
		got := powerImage(t, fs.Crash(rand.New(rand.NewSource(7)), true))
		if !bytes.Equal(got, want) {
			t.Fatal("same seed produced a different torn disk image")
		}
	}
}
