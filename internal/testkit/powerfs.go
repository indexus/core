package testkit

import (
	"errors"
	"io"
	"math/rand"
	"sort"
	"strings"
	"sync"
)

// PowerFS is a disk that can lose power. A file keeps what was written to it
// and how much of that a File.Sync made durable; the directory keeps its
// names and what they were at the last FS.Sync. crash returns the disk as it
// would be found afterwards.
type PowerFS struct {
	mu      sync.Mutex
	files   map[string]*powerFile
	durable map[string]*powerFile
	Ops     int // creations, writes, syncs, renames and removals so far
	FailAt  int // the power goes off at this op (0: never)
	bytes   map[string]int64
	held    chan struct{} // closed when the next File.Sync starts waiting
	Release chan struct{}
}

type powerFile struct {
	data   []byte
	synced int
}

type PowerHandle struct {
	fs   *PowerFS
	f    *powerFile
	name string
}

var ErrPowerOff = errors.New("memfs: power is off")

func NewPowerFS() *PowerFS {
	return &PowerFS{files: map[string]*powerFile{}, durable: map[string]*powerFile{}, bytes: map[string]int64{}}
}

// op counts one change to the disk and refuses it once the power is off.
func (fs *PowerFS) op() error {
	fs.Ops++
	if fs.FailAt > 0 && fs.Ops >= fs.FailAt {
		return ErrPowerOff
	}
	return nil
}

func (fs *PowerFS) Create(name string) (*PowerHandle, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if err := fs.op(); err != nil {
		return nil, err
	}
	fs.files[name] = &powerFile{}
	return &PowerHandle{fs, fs.files[name], name}, nil
}

func (fs *PowerFS) Open(name string) (*PowerHandle, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.files[name] == nil {
		return nil, errors.New("memfs: no file " + name)
	}
	return &PowerHandle{fs, fs.files[name], name}, nil
}

func (fs *PowerFS) Rename(from, to string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if err := fs.op(); err != nil {
		return err
	}
	fs.files[to] = fs.files[from]
	delete(fs.files, from)
	return nil
}

func (fs *PowerFS) Remove(name string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if err := fs.op(); err != nil {
		return err
	}
	delete(fs.files, name)
	return nil
}

func (fs *PowerFS) List() ([]string, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var names []string
	for name := range fs.files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func (fs *PowerFS) Sync() error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if err := fs.op(); err != nil {
		return err
	}
	fs.durable = map[string]*powerFile{}
	for name, f := range fs.files {
		fs.durable[name] = f
	}
	return nil
}

func (h *PowerHandle) ReadAt(p []byte, off int64) (int, error) {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if off >= int64(len(h.f.data)) {
		return 0, io.EOF
	}
	if n := copy(p, h.f.data[off:]); n < len(p) {
		return n, io.EOF
	}
	return len(p), nil
}

func (h *PowerHandle) Write(p []byte) (int, error) {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if err := h.fs.op(); err != nil {
		return 0, err
	}
	h.f.data = append(h.f.data, p...)
	h.fs.bytes[h.name[strings.LastIndexByte(h.name, '.'):]] += int64(len(p))
	return len(p), nil
}

func (h *PowerHandle) Sync() error {
	h.fs.mu.Lock()
	if held := h.fs.held; held != nil {
		h.fs.held = nil
		h.fs.mu.Unlock()
		close(held)
		<-h.fs.Release
		h.fs.mu.Lock()
	}
	defer h.fs.mu.Unlock()
	if err := h.fs.op(); err != nil {
		return err
	}
	h.f.synced = len(h.f.data)
	return nil
}

func (h *PowerHandle) Close() error { return nil }

func (h *PowerHandle) Size() (int64, error) {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	return int64(len(h.f.data)), nil
}

// written is the bytes written so far to files with this extension.
func (fs *PowerFS) Written(ext string) int64 {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.bytes[ext]
}

// size is what the files ending in ext hold now.
func (fs *PowerFS) Size(ext string) (n int64) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for name, f := range fs.files {
		if strings.HasSuffix(name, ext) {
			n += int64(len(f.data))
		}
	}
	return n
}

// holdSync makes the next File.Sync wait until fs.Release is closed. The
// returned channel is closed when that Sync is waiting.
func (fs *PowerFS) HoldSync() chan struct{} {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.held, fs.Release = make(chan struct{}), make(chan struct{})
	return fs.held
}

// crash returns the disk after a power cut. Names are those of the last
// FS.Sync: a creation, rename or removal after it is lost. A file keeps its
// synced bytes; with torn set it also keeps a random part of the bytes
// written since, ending in garbage (a page half written).
func (fs *PowerFS) Crash(rng *rand.Rand, torn bool) *PowerFS {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	after := NewPowerFS()
	// The RNG must be consumed in canonical file order. Go map iteration is
	// randomized: using it here made a seed insufficient to replay a power cut.
	names := make([]string, 0, len(fs.durable))
	for name := range fs.durable {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		f := fs.durable[name]
		data := append([]byte(nil), f.data[:f.synced]...)
		if rest := f.data[f.synced:]; torn && len(rest) > 0 {
			data = append(data, rest[:rng.Intn(len(rest)+1)]...)
			for i := len(data) - rng.Intn(min(len(data)-f.synced, 8)+1); i < len(data); i++ {
				data[i] = byte(rng.Intn(256))
			}
		}
		after.files[name] = &powerFile{data: data, synced: len(data)}
		after.durable[name] = after.files[name]
	}
	return after
}
