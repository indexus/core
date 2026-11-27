package zonelog

import (
	"io"
	"os"
	"path/filepath"
)

// FS is the one directory a store lives in. Every byte the store reads or
// writes goes through it, so a test can stand in for the disk and lose what
// a power cut would lose: writes not followed by File.Sync, and creations,
// renames and removals not followed by FS.Sync.
type FS interface {
	Create(name string) (File, error) // a new empty file, replacing any old one
	Open(name string) (File, error)   // an existing file, to read
	Rename(from, to string) error
	Remove(name string) error
	List() ([]string, error)
	Sync() error // makes creations, renames and removals durable
}

type File interface {
	io.ReaderAt
	io.Writer
	Sync() error
	Close() error
	Size() (int64, error)
}

// Dir is the FS of a directory of the operating system.
type Dir string

type osFile struct{ *os.File }

func (f osFile) Size() (int64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (d Dir) Create(name string) (File, error) {
	f, err := os.OpenFile(filepath.Join(string(d), name), os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600)
	return osFile{f}, err
}

func (d Dir) Open(name string) (File, error) {
	f, err := os.Open(filepath.Join(string(d), name))
	return osFile{f}, err
}

func (d Dir) Rename(from, to string) error {
	return os.Rename(filepath.Join(string(d), from), filepath.Join(string(d), to))
}

func (d Dir) Remove(name string) error { return os.Remove(filepath.Join(string(d), name)) }

func (d Dir) List() ([]string, error) {
	entries, err := os.ReadDir(string(d))
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names, err
}

func (d Dir) Sync() error {
	f, err := os.Open(string(d))
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// readAll returns the whole content of a file.
func readAll(fs FS, name string) ([]byte, error) {
	f, err := fs.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	size, err := f.Size()
	if err != nil {
		return nil, err
	}
	raw := make([]byte, size)
	if _, err := f.ReadAt(raw, 0); err != nil && err != io.EOF {
		return nil, err
	}
	return raw, nil
}
