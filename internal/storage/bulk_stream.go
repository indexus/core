package storage

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/indexus/go-indexus-core/internal/prep"
)

// PackBatch writes a batch directory as a tar stream: manifest.json, then
// the parts in manifest order, then zones.ixp when there is one.
func PackBatch(w io.Writer, dir string) error {
	m, err := prep.ReadManifest(dir)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(w)
	for _, name := range batchFiles(m) {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err == nil {
			err = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: info.Size(), ModTime: info.ModTime()})
		}
		if err == nil {
			_, err = io.Copy(tw, f)
		}
		f.Close()
		if err != nil {
			return err
		}
	}
	return tw.Close()
}

func batchFiles(m prep.Manifest) []string {
	names := []string{"manifest.json"}
	for _, p := range m.Parts {
		names = append(names, p.File)
	}
	if m.Zones != nil {
		names = append(names, m.Zones.File)
	}
	return names
}

// IngestBulkStream installs a batch sent as a tar stream (PackBatch), as it
// arrives: nothing is spooled, each part is read once and checked on the way,
// and nothing is visible before the cut. A batch already installed is
// answered after its manifest. The write lock is held while the stream
// lasts: a slow sender delays the writes of this node.
func (s *Store) IngestBulkStream(ctx context.Context, body io.Reader, replace bool) (BulkResult, error) {
	tr := tar.NewReader(body)
	h, err := tr.Next()
	if err != nil || h.Name != "manifest.json" || h.Size > 16<<20 {
		return BulkResult{}, fmt.Errorf("%w: the stream does not start with manifest.json %v", ErrBulkInvalid, err)
	}
	raw, err := io.ReadAll(tr)
	if err != nil {
		return BulkResult{}, err
	}
	var m prep.Manifest
	if err := json.Unmarshal(raw, &m); err != nil || m.Format != prep.Format {
		return BulkResult{}, fmt.Errorf("%w: manifest: %v", ErrBulkInvalid, err)
	}
	var known BulkResult
	err = s.view(func(tx *Tx) error {
		if raw := bulkReceipt(tx, BatchKey(m)); raw != nil {
			known.Already = true
			return json.Unmarshal(raw, &known.BulkReceipt)
		}
		return nil
	})
	if err != nil || known.Already {
		return known, err
	}
	sizes := map[string]int64{}
	for _, p := range m.Parts {
		sizes[p.File] = int64(p.Bytes)
	}
	next := func(name string) (io.Reader, error) {
		h, err := tr.Next()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if size, part := sizes[name]; h.Name != name || part && h.Size != size {
			return nil, fmt.Errorf("%q (%d bytes) where %s was expected", h.Name, h.Size, name)
		}
		return tr, nil
	}
	return s.ingest(ctx, m, next, replace)
}
