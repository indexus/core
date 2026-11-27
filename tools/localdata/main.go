// Command localdata seeds an isolated local node from existing source exports.
// Large render blocks are real content records, not references to disk paths.
//
// It also moves an experience from an old offline database to a running
// network: -export (filtered, translated) writes NDJSON, -names picks three
// vnode names that share it, -write posts it through the mesh one record at
// a time, and -bulk installs it on the nodes as staged batches (-stage).
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

type asset struct {
	Path string `json:"path"`
	File string `json:"file"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	dir := flag.String("dir", ".local/unified-experiences", "Persistent isolated data directory")
	records := flag.String("records", "", "NDJSON records; - reads stdin")
	assets := flag.String("assets", "", "JSON list of render block paths to store")
	seal := flag.Bool("seal", false, "Seal offline seed after successful import")
	name := flag.String("node", "AAAAAAAAAAAAAAAD", "Destination vnode name")
	cluster := flag.String("cluster", "indexus-unified-local", "Destination cluster identity")
	endpoint := flag.String("endpoint", "http://127.0.0.1:21030", "Destination holder endpoint")
	export := flag.String("export", "", "Export records from a stopped/offline database to stdout")
	collections := flag.String("collections", "", "With -export: keep only these collections (comma list, NAME or PREFIX*)")
	paths := flag.String("paths", "", "With -export: also keep content parts whose path starts with one of these (comma list)")
	translate := flag.String("translate", "", "With -export: rewrite native locations of these collections from the former alphabet")
	within := flag.String("within", "", "With -export: keep native records of COLLECTION only under a location PREFIX (comma list of COLLECTION:PREFIX, after -translate)")
	names := flag.Bool("names", false, "Print three vnode names that share the -records evenly")
	write := flag.String("write", "", "Post the -records through the mesh to these endpoints (comma list)")
	issuer := flag.String("issuer", "http://127.0.0.1:22030", "With -write: issuer of the write token")
	concurrency := flag.Int("concurrency", 16, "With -write: concurrent writes")
	bulkPath := flag.String("bulk", "", "Install batches on the -write endpoints: a batch directory, a directory of them, or an NDJSON file to stage first")
	replace := flag.Bool("replace", false, "With -bulk: a batch replaces the records of its zone")
	stage := flag.String("stage", "", "Write the -records as one IXP1 batch per collection (docs/ingest/pipeline.md) into this directory")
	flag.Parse()
	if *stage != "" {
		f, err := os.Open(*records)
		if err != nil {
			return err
		}
		defer f.Close()
		batches, err := stageRecords(f, *stage)
		for _, batch := range batches {
			fmt.Println(batch)
		}
		return err
	}
	if *bulkPath != "" {
		if *write == "" {
			return fmt.Errorf("-bulk needs the -write endpoints")
		}
		token, err := issueToken(*issuer)
		if err != nil {
			return err
		}
		w := &writer{endpoints: splitList(*write), token: token, client: &http.Client{Timeout: time.Minute}}
		return w.sendBulk(*bulkPath, *replace)
	}
	if *export != "" {
		return exportRecords(*export, exportFilter{collections: splitList(*collections), paths: splitList(*paths), translate: splitList(*translate), within: splitList(*within)}, *collections != "" || *paths != "")
	}
	if *names || *write != "" {
		if *records == "" || *records == "-" {
			return fmt.Errorf("-names and -write need a -records file")
		}
		f, err := os.Open(*records)
		if err != nil {
			return err
		}
		defer f.Close()
		if *names {
			chosen, err := threeNames(f)
			if err == nil {
				fmt.Println(strings.Join(chosen[:], "\n"))
			}
			return err
		}
		token, err := issueToken(*issuer)
		if err != nil {
			return err
		}
		w := &writer{endpoints: splitList(*write), token: token, client: &http.Client{Timeout: 2 * time.Minute}, retryFor: 2 * time.Minute}
		count, err := w.writeRecords(f, max(1, *concurrency))
		fmt.Printf("write complete: %d records\n", count)
		return err
	}
	s, err := storage.OpenWithOptions(filepath.Join(*dir, "backup.causal.db"), *name, *cluster, 32<<30, storage.Options{BatchWrites: true, BatchSize: 128, BatchDelay: 2 * time.Millisecond})
	if err != nil {
		return err
	}
	defer s.Close()
	holder, err := s.Endpoint(*endpoint)
	if err != nil {
		return err
	}
	id, err := tree.DecodeName(*name)
	if err != nil {
		return err
	}
	if err = s.InitCausal(tree.VNodeDescriptor{Name: *name, ID: id}, causal.Genesis{Cluster: *cluster, Holder: holder}); err != nil {
		return err
	}
	var batch []storage.Record
	count := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if e := s.SeedLocalRecords(batch); e != nil {
			return e
		}
		count += len(batch)
		batch = nil
		if count%5000 < 512 {
			fmt.Fprintf(os.Stderr, "stored %d records\n", count)
		}
		return nil
	}
	enqueue := func(r storage.Record) error {
		if len(batch) == 0 {
			var st syscall.Statfs_t
			if e := syscall.Statfs(*dir, &st); e != nil {
				return e
			}
			if uint64(st.Bavail)*uint64(st.Bsize) < 1536<<20 {
				return fmt.Errorf("disk guard: less than 1.5 GiB free")
			}
		}
		batch = append(batch, r)
		if len(batch) >= 256 {
			return flush()
		}
		return nil
	}
	var input io.Reader = os.Stdin
	if *records != "" {
		if *records != "-" {
			f, e := os.Open(*records)
			if e != nil {
				return e
			}
			defer f.Close()
			input = f
		}
		scan := bufio.NewScanner(input)
		scan.Buffer(make([]byte, 65536), 4<<20)
		for scan.Scan() {
			var r storage.Record
			if err = json.Unmarshal(scan.Bytes(), &r); err != nil {
				break
			}
			if err = enqueue(r); err != nil {
				break
			}
		}
		if err == nil {
			err = scan.Err()
		}
	}
	if err == nil && *assets != "" {
		var files []asset
		raw, e := os.ReadFile(*assets)
		if e != nil {
			err = e
		} else {
			err = json.Unmarshal(raw, &files)
		}
		for i, f := range files {
			if err != nil {
				break
			}
			var raw []byte
			raw, err = os.ReadFile(f.File)
			if err != nil {
				break
			}
			hash := sha256.Sum256(raw)
			var compressed bytes.Buffer
			w, _ := gzip.NewWriterLevel(&compressed, gzip.BestSpeed)
			_, err = w.Write(raw)
			if err == nil {
				err = w.Close()
			}
			if err != nil {
				break
			}
			keyHash := sha256.Sum256([]byte(f.Path))
			location := tree.BASE64.Encode(keyHash[:12])
			payload := compressed.Bytes()
			parts := (len(payload) + (256 << 10) - 1) / (256 << 10)
			if parts == 0 {
				parts = 1
			}
			for part := 0; part < parts; part++ {
				end := min(len(payload), (part+1)*(256<<10))
				body := map[string]any{"path": f.Path, "encoding": "gzip-base64", "sha256": fmt.Sprintf("%x", hash), "bytes": len(raw), "part": part, "parts": parts, "data": base64.StdEncoding.EncodeToString(payload[part*(256<<10) : end])}
				p, e := json.Marshal(body)
				if e != nil {
					err = e
					break
				}
				err = enqueue(storage.Record{Collection: "LocalAssetData01", Location: location, ID: fmt.Sprintf("part%05d", part), Metrics: []float64{float64(end - part*(256<<10))}, Payload: p})
				if err != nil {
					break
				}
			}
			if i%500 == 0 {
				fmt.Fprintf(os.Stderr, "assets %d/%d %s\n", i, len(files), f.Path)
			}
		}
	}
	if err != nil {
		return err
	}
	if err = flush(); err != nil {
		return err
	}
	if *seal {
		if err = s.SealLocalSeed(); err != nil {
			return err
		}
	}
	fmt.Printf("seed complete: %d records\n", count)
	return nil
}

// exportRecords streams the database as NDJSON on stdout. With a filter it
// keeps the selected records only. Counts go to stderr as one JSON line:
// exported, translated, and native (records that are not content parts, so
// that an untranslated native collection shows).
func exportRecords(path string, filter exportFilter, filtered bool) error {
	encoder := json.NewEncoder(os.Stdout)
	exported, translated, native := map[string]int64{}, map[string]int64{}, map[string]int64{}
	err := storage.ExportOfflineRecords(path, func(r storage.Record) error {
		part, content := contentPath(r)
		if filtered && !filter.keep(r, part, content) {
			return nil
		}
		changed, err := filter.apply(&r, content)
		if err != nil {
			return err
		}
		if !filter.inside(r, content) {
			return nil
		}
		if changed {
			translated[r.Collection]++
		}
		if !content {
			native[r.Collection]++
		}
		exported[r.Collection]++
		return encoder.Encode(r)
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stderr).Encode(map[string]any{"exported": exported, "translated": translated, "native": native})
}
