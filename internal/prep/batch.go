package prep

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
)

// Format names the manifest version.
const Format = "indexus-prep-v1"

// Manifest is manifest.json.
type Manifest struct {
	Format       string   `json:"format"`
	Collection   string   `json:"collection"`
	Metrics      int      `json:"metrics"`
	MetricNames  []string `json:"metricNames,omitempty"`
	Records      uint64   `json:"records"`
	PayloadBytes uint64   `json:"payloadBytes"`
	// PayloadEncoding says what the payload bytes are: "json" (a JSON value,
	// what `localdata -write` sends and the node stores today) or "binary".
	PayloadEncoding string     `json:"payloadEncoding,omitempty"`
	RecordDepths    [2]int     `json:"recordDepths"`
	SplitDepth      int        `json:"splitDepth"`
	Parts           []PartInfo `json:"parts"`
	Zones           *ZoneInfo  `json:"zones,omitempty"`
	Inputs          Inputs     `json:"inputs"`
	ID              string     `json:"id"`
}

// ZoneInfo describes zones.ixp.
type ZoneInfo struct {
	File    string `json:"file"`
	Depths  [2]int `json:"depths"`
	Records uint64 `json:"records"`
	SHA256  string `json:"sha256"`
}

// Inputs is what the batch was made from; its hash is the batch id.
type Inputs struct {
	Tool    string            `json:"tool"`
	Sources map[string]string `json:"sources"`
	Options map[string]string `json:"options,omitempty"`
}

// BatchID is the hex SHA-256 of the canonical JSON of the inputs: keys
// sorted at every level, no spaces (json.dumps(..., sort_keys=True,
// separators=(",", ":")) in Python; encoding/json sorts map keys).
func BatchID(in Inputs) string {
	canon := map[string]any{"tool": in.Tool, "sources": in.Sources}
	if len(in.Options) > 0 {
		canon["options"] = in.Options
	}
	b, _ := json.Marshal(canon)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

// Options of a batch.
type Options struct {
	Collection      string
	Metrics         int
	MetricNames     []string
	Payloads        bool
	PayloadEncoding string // "json" or "binary" (manifest)
	SplitDepth      int    // parts end only at zones of this depth
	PartBytes       uint64 // a part ends at the first zone boundary past this size
	ZoneDepths      [2]int // zones.ixp holds these depths; [0, -1] for none
	Inputs          Inputs
}

// BatchWriter writes the parts of a batch from rows in key order, and the
// sums of its zones.
type BatchWriter struct {
	dir  string
	opt  Options
	man  Manifest
	part *Writer
	size uint64
	last Head
	any  bool

	zones []zoneAcc // one per zone depth, the zone being added to
	done  []Row     // finished zones
}

type zoneAcc struct {
	head   Head
	open   bool
	weight float64
	sums   []float64 // count, metrics...
}

// NewBatch starts a batch in dir (created if needed).
func NewBatch(dir string, opt Options) (*BatchWriter, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if opt.PartBytes == 0 {
		opt.PartBytes = 64 << 20
	}
	b := &BatchWriter{dir: dir, opt: opt}
	b.man = Manifest{Format: Format, Collection: opt.Collection, Metrics: opt.Metrics, MetricNames: opt.MetricNames, PayloadEncoding: opt.PayloadEncoding,
		RecordDepths: [2]int{16, 0}, SplitDepth: opt.SplitDepth, Inputs: opt.Inputs, ID: BatchID(opt.Inputs)}
	for d := opt.ZoneDepths[0]; d <= opt.ZoneDepths[1]; d++ {
		b.zones = append(b.zones, zoneAcc{sums: make([]float64, 1+opt.Metrics)})
	}
	return b, nil
}

// Add appends a record.
func (b *BatchWriter) Add(r Row) error {
	if b.part != nil && b.size >= b.opt.PartBytes && r.Head.Prefix(min(b.opt.SplitDepth, r.Head.Depth())) != b.last.Prefix(min(b.opt.SplitDepth, b.last.Depth())) {
		if err := b.closePart(); err != nil {
			return err
		}
	}
	if b.part == nil {
		var err error
		name := fmt.Sprintf("part-%05d.ixp", len(b.man.Parts))
		if b.part, err = Create(filepath.Join(b.dir, name), b.opt.Metrics, b.opt.Payloads); err != nil {
			return err
		}
		b.size = 0
	}
	if err := b.part.Add(r); err != nil {
		return err
	}
	b.size += uint64(headLen + len(r.ID) + 8*(1+len(r.Metrics)) + len(r.Payload))
	b.last, b.any = r.Head, true
	b.man.Records++
	b.man.PayloadBytes += uint64(len(r.Payload))
	b.man.RecordDepths[0] = min(b.man.RecordDepths[0], r.Head.Depth())
	b.man.RecordDepths[1] = max(b.man.RecordDepths[1], r.Head.Depth())
	for i := range b.zones {
		d := b.opt.ZoneDepths[0] + i
		if d > r.Head.Depth() {
			continue
		}
		z := &b.zones[i]
		if p := r.Head.Prefix(d); !z.open || p != z.head {
			b.emit(z)
			z.head, z.open = p, true
		}
		z.weight += r.Weight
		z.sums[0]++
		for j, v := range r.Metrics {
			z.sums[1+j] += v
		}
	}
	return nil
}

func (b *BatchWriter) emit(z *zoneAcc) {
	if z.open {
		b.done = append(b.done, Row{Head: z.head, Weight: z.weight, Metrics: append([]float64(nil), z.sums...)})
	}
	z.open, z.weight = false, 0
	clear(z.sums)
}

func (b *BatchWriter) closePart() error {
	info, err := b.part.Close()
	info.File = filepath.Base(b.part.f.Name())
	b.man.Parts = append(b.man.Parts, info)
	b.part = nil
	return err
}

// Close finishes the last part, writes zones.ixp and manifest.json, and
// returns the manifest.
func (b *BatchWriter) Close() (Manifest, error) {
	if b.part != nil {
		if err := b.closePart(); err != nil {
			return b.man, err
		}
	}
	if !b.any {
		b.man.RecordDepths = [2]int{0, 0}
	}
	if len(b.zones) > 0 {
		for i := range b.zones {
			b.emit(&b.zones[i])
		}
		info, err := WriteZones(filepath.Join(b.dir, "zones.ixp"), b.opt.Metrics, b.done)
		if err != nil {
			return b.man, err
		}
		b.man.Zones = &ZoneInfo{File: "zones.ixp", Depths: b.opt.ZoneDepths, Records: info.Records, SHA256: info.SHA256}
	}
	return b.man, WriteManifest(b.dir, b.man)
}

// WriteZones sorts zone rows (m+1 metrics: count, then sums) and writes them.
func WriteZones(path string, m int, rows []Row) (PartInfo, error) {
	sort.Slice(rows, func(i, j int) bool { return bytes.Compare(rows[i].Head[:], rows[j].Head[:]) < 0 })
	w, err := Create(path, m+1, false)
	if err != nil {
		return PartInfo{}, err
	}
	for _, r := range rows {
		if err := w.Add(r); err != nil {
			w.Close()
			return PartInfo{}, err
		}
	}
	return w.Close()
}

// WriteManifest writes manifest.json, indented, with a final newline.
func WriteManifest(dir string, m Manifest) error {
	raw, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "manifest.json"), append(raw, '\n'), 0o644)
}

// ReadManifest reads and checks manifest.json.
func ReadManifest(dir string) (Manifest, error) {
	var m Manifest
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return m, err
	}
	if err = json.Unmarshal(raw, &m); err != nil {
		return m, err
	}
	if m.Format != Format {
		return m, fmt.Errorf("prep: format %q, want %q", m.Format, Format)
	}
	return m, nil
}

func fileSHA(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// Each visits every record of a batch in key order, checking hashes, CRCs
// and the order across parts.
func Each(dir string, m Manifest, visit func(Row) error) error {
	var prev Head
	var prevID []byte
	seen := false
	var n uint64
	for _, p := range m.Parts {
		path := filepath.Join(dir, p.File)
		sum, err := fileSHA(path)
		if err != nil {
			return err
		}
		if sum != p.SHA256 {
			return fmt.Errorf("prep: %s: sha256 %s, manifest says %s", p.File, sum, p.SHA256)
		}
		pr, f, err := OpenFile(path)
		if err != nil {
			return fmt.Errorf("%s: %w", p.File, err)
		}
		if pr.M != m.Metrics || pr.Rows != p.Records {
			f.Close()
			return fmt.Errorf("prep: %s: %d metrics, %d rows", p.File, pr.M, pr.Rows)
		}
		err = pr.Each(func(r Row) error {
			if seen && compareKey(prev, prevID, r.Head, r.ID) >= 0 {
				return fmt.Errorf("prep: %s: %s/%s is not after the previous part", p.File, r.Head.Location(), r.ID)
			}
			seen, prev, prevID = true, r.Head, append(prevID[:0], r.ID...)
			n++
			return visit(r)
		})
		f.Close()
		if err != nil {
			return err
		}
	}
	if n != m.Records {
		return fmt.Errorf("prep: %d records, manifest says %d", n, m.Records)
	}
	return nil
}

// Verify checks a whole batch: every hash, CRC and the key order, and, when
// the batch has zones.ixp, that each zone row equals the sum of its records
// to a relative tolerance (float sums depend on the order of additions).
// It returns the largest relative difference seen in the zones.
func Verify(dir string, tolerance float64) (Manifest, float64, error) {
	m, err := ReadManifest(dir)
	if err != nil {
		return m, 0, err
	}
	var want []Row
	var worst float64
	if m.Zones != nil {
		path := filepath.Join(dir, m.Zones.File)
		sum, err := fileSHA(path)
		if err != nil {
			return m, 0, err
		}
		if sum != m.Zones.SHA256 {
			return m, 0, errors.New("prep: zones.ixp sha256")
		}
		pr, f, err := OpenFile(path)
		if err != nil {
			return m, 0, err
		}
		err = pr.Each(func(r Row) error {
			want = append(want, Row{Head: r.Head, Weight: r.Weight, Metrics: append([]float64(nil), r.Metrics...)})
			return nil
		})
		f.Close()
		if err != nil {
			return m, 0, err
		}
	}
	got := map[Head]*Row{}
	err = Each(dir, m, func(r Row) error {
		if m.Zones == nil {
			return nil
		}
		for d := m.Zones.Depths[0]; d <= min(m.Zones.Depths[1], r.Head.Depth()); d++ {
			p := r.Head.Prefix(d)
			z := got[p]
			if z == nil {
				z = &Row{Head: p, Metrics: make([]float64, 1+m.Metrics)}
				got[p] = z
			}
			z.Weight += r.Weight
			z.Metrics[0]++
			for j, v := range r.Metrics {
				z.Metrics[1+j] += v
			}
		}
		return nil
	})
	if err != nil || m.Zones == nil {
		return m, 0, err
	}
	if len(got) != len(want) {
		return m, 0, fmt.Errorf("prep: %d zones hold records, zones.ixp has %d", len(got), len(want))
	}
	for _, w := range want {
		g := got[w.Head]
		if g == nil {
			return m, 0, fmt.Errorf("prep: zone %s in zones.ixp holds no record", w.Head.Location())
		}
		for j := -1; j < len(w.Metrics); j++ {
			a, b := w.Weight, g.Weight
			if j >= 0 {
				a, b = w.Metrics[j], g.Metrics[j]
			}
			d := math.Abs(a-b) / math.Max(1e-300, math.Max(math.Abs(a), math.Abs(b)))
			if a == b {
				d = 0
			}
			worst = math.Max(worst, d)
			if d > tolerance {
				return m, worst, fmt.Errorf("prep: zone %s column %d: %v in zones.ixp, %v from the records", w.Head.Location(), j, a, b)
			}
		}
	}
	return m, worst, nil
}
