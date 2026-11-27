package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/indexus/go-indexus-core/internal/prep"
)

// record is one line of today's NDJSON (`localdata -write`).
type record struct {
	Collection string
	Location   string
	ID         string
	Weight     *float64 `json:",omitempty"`
	Metrics    []float64
	Payload    json.RawMessage // kept as the node keeps it: the JSON value
}

func parseDepths(s string) ([2]int, error) {
	if s == "none" {
		return [2]int{0, -1}, nil
	}
	a, b, ok := strings.Cut(s, ":")
	lo, err1 := strconv.Atoi(a)
	hi, err2 := strconv.Atoi(b)
	if !ok || err1 != nil || err2 != nil || lo < 0 || hi > 16 {
		return [2]int{}, fmt.Errorf("zones %q: want lo:hi or none", s)
	}
	return [2]int{lo, hi}, nil
}

// fileInfo is what the index remembers of an input, so that an unchanged
// file is neither hashed nor read again.
type fileInfo struct {
	Size        int64
	ModTime     int64
	SHA256      string
	Collections []string
}

const ndjsonTool = "prep ndjson 3" // 3: binary payloads of the scene's meshes and ground tiles (payloads.go)

func fromNDJSON(args []string) error {
	fs := flag.NewFlagSet("ndjson", flag.ExitOnError)
	outDir := fs.String("out", "", "write one batch per collection in DIR/<collection>/")
	split := fs.Int("split", 2, "parts end only at zones of this depth")
	zones := fs.String("zones", "0:2", "depths of zones.ixp, lo:hi (capped at the shallowest record depth - 1), or none")
	partMB := fs.Int("part-mb", 64, "part size before a cut")
	fs.Parse(args)
	zd, err := parseDepths(*zones)
	if err != nil {
		return err
	}
	opt := map[string]string{"split": strconv.Itoa(*split), "zones": *zones, "part-mb": strconv.Itoa(*partMB)}
	if *outDir == "" { // one file, one collection, one batch directory
		if fs.NArg() != 2 {
			usage()
		}
		sum, err := fileSHA(fs.Arg(0))
		if err != nil {
			return err
		}
		return buildBatches([]string{fs.Arg(0)}, map[string]string{fs.Arg(0): sum}, nil, func(string) string { return fs.Arg(1) },
			*split, zd, *partMB, opt)
	}
	files := fs.Args()
	if len(files) == 0 {
		usage()
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	indexPath := filepath.Join(*outDir, ".prep-files.json")
	index := map[string]fileInfo{}
	if raw, err := os.ReadFile(indexPath); err == nil {
		json.Unmarshal(raw, &index)
	}
	shas := map[string]string{}
	of := map[string][]string{} // collection -> files holding it, in the order given
	for _, f := range files {
		abs, _ := filepath.Abs(f)
		st, err := os.Stat(f)
		if err != nil {
			return err
		}
		info, ok := index[abs]
		if !ok || info.Size != st.Size() || info.ModTime != st.ModTime().UnixNano() {
			sum, err := fileSHA(f)
			if err != nil {
				return err
			}
			cols, err := collectionsOf(f)
			if err != nil {
				return err
			}
			info = fileInfo{st.Size(), st.ModTime().UnixNano(), sum, cols}
			index[abs] = info
		}
		shas[f] = info.SHA256
		for _, c := range info.Collections {
			of[c] = append(of[c], f)
		}
	}
	// Collections that share a file are built together (one read of it).
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if parent[x] == "" || parent[x] == x {
			parent[x] = x
			return x
		}
		parent[x] = find(parent[x])
		return parent[x]
	}
	byFile := map[string][]string{}
	for c, fl := range of {
		for _, f := range fl {
			byFile[f] = append(byFile[f], c)
		}
	}
	for _, cols := range byFile {
		for _, c := range cols[1:] {
			parent[find(c)] = find(cols[0])
		}
	}
	groups := map[string][]string{}
	for c := range of {
		groups[find(c)] = append(groups[find(c)], c)
	}
	names := make([]string, 0, len(groups))
	for g := range groups {
		names = append(names, g)
	}
	sort.Strings(names)
	for _, g := range names {
		cols := groups[g]
		sort.Strings(cols)
		var gfiles []string
		seen := map[string]bool{}
		for _, f := range files {
			for _, c := range cols {
				if !seen[f] && contains(of[c], f) {
					gfiles = append(gfiles, f)
					seen[f] = true
				}
			}
		}
		// Skip the group when every collection's batch already has the id these inputs give.
		same := true
		for _, c := range cols {
			m, err := prep.ReadManifest(filepath.Join(*outDir, c))
			if err != nil || m.ID != prep.BatchID(ndjsonInputs(c, of[c], shas, opt)) {
				same = false
			}
		}
		if same {
			fmt.Fprintf(os.Stderr, "prep ndjson: %s: unchanged, skipped\n", strings.Join(cols, ", "))
			continue
		}
		dir := func(c string) string { return filepath.Join(*outDir, c+".new") }
		if err := buildBatches(gfiles, shas, cols, dir, *split, zd, *partMB, opt, of); err != nil {
			return err
		}
		for _, c := range cols {
			os.RemoveAll(filepath.Join(*outDir, c))
			if err := os.Rename(dir(c), filepath.Join(*outDir, c)); err != nil {
				return err
			}
		}
	}
	raw, _ := json.MarshalIndent(index, "", " ")
	return os.WriteFile(indexPath, raw, 0o644)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func ndjsonInputs(collection string, files []string, shas map[string]string, opt map[string]string) prep.Inputs {
	src := map[string]string{}
	for _, f := range files {
		src[filepath.Base(f)] = shas[f]
	}
	o := map[string]string{"collection": collection}
	for k, v := range opt {
		o[k] = v
	}
	return prep.Inputs{Tool: ndjsonTool, Sources: src, Options: o}
}

// collectionsOf lists the collections of an NDJSON file (a scan for the
// first field, no full parse).
func collectionsOf(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	seen := map[string]bool{}
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 1<<20), 64<<20)
	for scan.Scan() {
		var r struct{ Collection string }
		line := scan.Bytes()
		// The collection comes first in today's files: decode a short prefix when it can.
		if i := bytes.Index(line, []byte(`"Location"`)); i > 0 && json.Unmarshal(append(append([]byte(nil), line[:i]...), []byte(`"x":0}`)...), &r) == nil && r.Collection != "" {
			seen[r.Collection] = true
			continue
		}
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, err
		}
		seen[r.Collection] = true
	}
	var out []string
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out, scan.Err()
}

// buildBatches reads files and writes one batch per collection of cols (all
// collections when cols is nil, then only one is allowed) to dir(c). Of two
// versions of a key, the later one stays, as writes would keep it. Payloads
// are not held: a record keeps where its payload is in its file, and the
// writer reads it back (memory is the keys and the metrics, not the bytes).
func buildBatches(files []string, shas map[string]string, cols []string, dir func(string) string, split int, zd [2]int, partMB int,
	opt map[string]string, of ...map[string][]string) error {
	start := time.Now()
	type item struct {
		head    prep.Head
		id      string
		weight  float64
		metrics []float64
		file    int
		payAt   int64
		payLen  int
		seq     int
	}
	want := map[string]bool{}
	for _, c := range cols {
		want[c] = true
	}
	items := map[string][]item{}
	metricCount := map[string]int{}
	handles := make([]*os.File, len(files))
	defer func() {
		for _, f := range handles {
			if f != nil {
				f.Close()
			}
		}
	}()
	seq := 0
	for fi, in := range files {
		f, err := os.Open(in)
		if err != nil {
			return err
		}
		handles[fi] = f
		rd := bufio.NewReaderSize(f, 1<<20)
		var at int64
		for line := 1; ; line++ {
			raw, err := rd.ReadBytes('\n')
			if len(raw) == 0 && err != nil {
				break
			}
			lineAt := at
			at += int64(len(raw))
			text := bytes.TrimRight(raw, "\r\n")
			if len(bytes.TrimSpace(text)) == 0 {
				continue
			}
			var r record
			if err := json.Unmarshal(text, &r); err != nil {
				return fmt.Errorf("%s:%d: %w", in, line, err)
			}
			if cols != nil && !want[r.Collection] {
				continue
			}
			if cols == nil && len(items) == 1 && items[r.Collection] == nil {
				return fmt.Errorf("%s:%d: a second collection; use -out DIR", in, line)
			}
			if m, ok := metricCount[r.Collection]; ok && m != len(r.Metrics) {
				return fmt.Errorf("%s:%d: %s has %d metrics, earlier records %d", in, line, r.Collection, len(r.Metrics), m)
			}
			metricCount[r.Collection] = len(r.Metrics)
			h, err := prep.MakeHead(r.Location)
			if err != nil {
				return fmt.Errorf("%s:%d: %w", in, line, err)
			}
			w := 1.0
			if r.Weight != nil {
				w = *r.Weight
			}
			it := item{h, r.ID, w, r.Metrics, fi, 0, 0, seq}
			if len(r.Payload) > 0 {
				// The payload is the line's bytes as they are (encoding/json keeps a RawMessage verbatim).
				p := bytes.LastIndex(text, r.Payload)
				if p < 0 {
					return fmt.Errorf("%s:%d: payload not found in its line", in, line)
				}
				it.payAt, it.payLen = lineAt+int64(p), len(r.Payload)
			}
			items[r.Collection] = append(items[r.Collection], it)
			seq++
		}
	}
	read := time.Since(start)
	names := make([]string, 0, len(items))
	for c := range items {
		names = append(names, c)
	}
	sort.Strings(names)
	var buf []byte
	for _, c := range names {
		list := items[c]
		t0 := time.Now()
		sort.Slice(list, func(i, j int) bool {
			a, b := list[i], list[j]
			if a.head != b.head {
				return string(a.head[:]) < string(b.head[:])
			}
			if a.id != b.id {
				return a.id < b.id
			}
			return a.seq < b.seq
		})
		kept := list[:0]
		for i, it := range list { // the last version of a key
			if i+1 < len(list) && list[i+1].head == it.head && list[i+1].id == it.id {
				continue
			}
			kept = append(kept, it)
		}
		minDepth, pay := 16, false
		for _, it := range kept {
			minDepth = min(minDepth, it.head.Depth())
			pay = pay || it.payLen > 0
		}
		z := zd
		z[1] = min(z[1], minDepth-1)
		srcFiles := files
		if len(of) > 0 {
			srcFiles = of[0][c]
		}
		enc := ""
		if pay {
			enc = "json"
			if binaryCollections[c] {
				enc = "binary"
			}
		}
		b, err := prep.NewBatch(dir(c), prep.Options{Collection: c, Metrics: metricCount[c], Payloads: pay, PayloadEncoding: enc, SplitDepth: split,
			PartBytes: uint64(partMB) << 20, ZoneDepths: z, Inputs: ndjsonInputs(c, srcFiles, shas, opt)})
		if err != nil {
			return err
		}
		for _, it := range kept {
			buf = buf[:0]
			if it.payLen > 0 {
				buf = append(buf, make([]byte, it.payLen)...)
				if _, err := handles[it.file].ReadAt(buf, it.payAt); err != nil {
					return err
				}
				if enc == "binary" {
					bin, err := binaryPayload(buf)
					if err != nil {
						return fmt.Errorf("%s %s/%s: %w", c, it.head, it.id, err)
					}
					buf = append(buf[:0], bin...)
				}
			}
			if err := b.Add(prep.Row{Head: it.head, ID: []byte(it.id), Weight: it.weight, Metrics: it.metrics, Payload: buf}); err != nil {
				return err
			}
		}
		man, err := b.Close()
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "prep ndjson: %s: %d records (%d replaced by a later version), %d parts, %.1f MB payload; sort and write %.2fs\n",
			c, man.Records, len(list)-len(kept), len(man.Parts), float64(man.PayloadBytes)/1e6, time.Since(t0).Seconds())
	}
	fmt.Fprintf(os.Stderr, "prep ndjson: read %d files in %.2fs, total %.2fs\n", len(files), read.Seconds(), time.Since(start).Seconds())
	return nil
}
