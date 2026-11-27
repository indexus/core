package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/indexus/go-indexus-core/internal/prep"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// sendBulk installs the batches at path on the nodes that hold their zones:
// a batch directory, a directory of them, or an NDJSON file of records that
// is staged first, one batch per collection, beside it. With several
// endpoints each batch is split by the XOR owner of its zones.
func (w *writer) sendBulk(path string, replace bool) error {
	batches, cleanup, err := batchesAt(path)
	if err != nil {
		return err
	}
	defer cleanup()
	var names []string
	if len(w.endpoints) > 1 {
		for _, endpoint := range w.endpoints {
			name, err := w.vnodeName(endpoint)
			if err != nil {
				return err
			}
			names = append(names, name)
		}
	}
	for _, dir := range batches {
		pieces := map[string]string{w.endpoints[0]: dir}
		if len(names) > 0 {
			if pieces, err = splitByOwner(dir, w.endpoints, names); err != nil {
				return err
			}
		}
		for endpoint, piece := range pieces {
			if err := w.postBatch(endpoint, piece, replace); err != nil {
				return err
			}
			if piece != dir {
				os.RemoveAll(piece)
			}
		}
	}
	return nil
}

// batchesAt finds the batch directories at path, staging an NDJSON file.
func batchesAt(path string) ([]string, func(), error) {
	none := func() {}
	info, err := os.Stat(path)
	if err != nil {
		return nil, none, err
	}
	if !info.IsDir() {
		dir, err := os.MkdirTemp(filepath.Dir(path), ".bulk-"+filepath.Base(path)+"-")
		if err != nil {
			return nil, none, err
		}
		cleanup := func() { os.RemoveAll(dir) }
		f, err := os.Open(path)
		if err != nil {
			cleanup()
			return nil, none, err
		}
		defer f.Close()
		batches, err := stageRecords(f, dir)
		if err != nil {
			cleanup()
		}
		return batches, cleanup, err
	}
	if _, err := os.Stat(filepath.Join(path, "manifest.json")); err == nil {
		return []string{path}, none, nil
	}
	found, err := filepath.Glob(filepath.Join(path, "*", "manifest.json"))
	for i := range found {
		found[i] = filepath.Dir(found[i])
	}
	if err == nil && len(found) == 0 {
		err = fmt.Errorf("%s: no batch (no manifest.json)", path)
	}
	return found, none, err
}

// stageRecords writes NDJSON records as one IXP1 batch per collection
// (docs/ingest/pipeline.md) under dir. A record given twice keeps its last
// version, as writes would. It holds the records in memory: the preparation
// pipeline (tools/prep) is the tool for large inputs.
func stageRecords(input io.Reader, dir string) ([]string, error) {
	type staged struct {
		head     prep.Head
		id       string
		metrics  []float64
		payload  []byte
		sequence int
	}
	rows := map[string][]staged{}
	scan := bufio.NewScanner(input)
	scan.Buffer(make([]byte, 65536), 64<<20)
	for n := 0; scan.Scan(); n++ {
		var r storage.Record
		if err := json.Unmarshal(scan.Bytes(), &r); err != nil {
			return nil, err
		}
		if r.Deleted {
			return nil, fmt.Errorf("%s/%s/%s: a batch holds no delete", r.Collection, r.Location, r.ID)
		}
		head, err := prep.MakeHead(r.Location)
		if err != nil {
			return nil, err
		}
		rows[r.Collection] = append(rows[r.Collection], staged{head, r.ID, r.Metrics, r.Payload, n})
	}
	if err := scan.Err(); err != nil {
		return nil, err
	}
	var out []string
	for collection, list := range rows {
		// Of two versions of a key, the later stays last.
		sort.Slice(list, func(i, j int) bool {
			if c := bytes.Compare(list[i].head[:], list[j].head[:]); c != 0 {
				return c < 0
			}
			if list[i].id != list[j].id {
				return list[i].id < list[j].id
			}
			return list[i].sequence < list[j].sequence
		})
		batch := filepath.Join(dir, collection)
		w, err := prep.NewBatch(batch, prep.Options{Collection: collection, Metrics: len(list[0].metrics), Payloads: true, SplitDepth: 2, ZoneDepths: [2]int{0, 2},
			Inputs: prep.Inputs{Tool: "localdata stage 1", Sources: map[string]string{collection: fmt.Sprint(len(list))}}})
		if err != nil {
			return nil, err
		}
		for i, r := range list {
			if i+1 < len(list) && list[i+1].head == r.head && list[i+1].id == r.id {
				continue
			}
			if len(r.metrics) != len(list[0].metrics) {
				return nil, fmt.Errorf("%s: a batch needs one metric count per collection", collection)
			}
			if err := w.Add(prep.Row{Head: r.head, ID: []byte(r.id), Weight: 1, Metrics: r.metrics, Payload: r.payload}); err != nil {
				return nil, err
			}
		}
		if _, err := w.Close(); err != nil {
			return nil, err
		}
		out = append(out, batch)
	}
	slices.Sort(out)
	return out, nil
}

// splitByOwner writes, for each node that holds some of the batch's zones,
// the part of the batch it holds, as a batch of its own.
func splitByOwner(dir string, endpoints, names []string) (map[string]string, error) {
	m, err := prep.ReadManifest(dir)
	if err != nil {
		return nil, err
	}
	ids := make([]tree.ID, len(names))
	for i, name := range names {
		if ids[i], err = tree.DecodeName(name); err != nil {
			return nil, err
		}
	}
	writers := map[int]*prep.BatchWriter{}
	pieces := map[string]string{}
	var head prep.Head
	owner := -1
	err = prep.Each(dir, m, func(row prep.Row) error {
		if owner < 0 || row.Head != head {
			zone, err := tree.ZoneID(m.Collection, row.Head.Location())
			if err != nil {
				return err
			}
			head, owner = row.Head, 0
			for i := range ids {
				if tree.XORCompare(zone, ids[i], ids[owner]) < 0 {
					owner = i
				}
			}
		}
		if writers[owner] == nil {
			piece := fmt.Sprintf("%s.%d", dir, owner)
			inputs := m.Inputs
			inputs.Options = maps.Clone(inputs.Options)
			if inputs.Options == nil {
				inputs.Options = map[string]string{}
			}
			inputs.Options["owner"] = names[owner]
			w, err := prep.NewBatch(piece, prep.Options{Collection: m.Collection, Metrics: m.Metrics, MetricNames: m.MetricNames, Payloads: true, PayloadEncoding: m.PayloadEncoding, SplitDepth: m.SplitDepth, ZoneDepths: [2]int{0, -1}, Inputs: inputs})
			if err != nil {
				return err
			}
			writers[owner], pieces[endpoints[owner]] = w, piece
		}
		return writers[owner].Add(row)
	})
	for _, w := range writers {
		if _, cerr := w.Close(); err == nil {
			err = cerr
		}
	}
	return pieces, err
}

// vnodeName asks a node for its name.
func (w *writer) vnodeName(endpoint string) (string, error) {
	request, err := http.NewRequest(http.MethodPost, endpoint+"/ping", strings.NewReader(`{"origin":{}}`))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+w.token)
	response, err := w.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var answer struct {
		Contact struct {
			Name string `json:"name"`
		} `json:"contact"`
	}
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil || answer.Contact.Name == "" {
		return "", fmt.Errorf("%s/ping: no vnode name (HTTP %d)", endpoint, response.StatusCode)
	}
	return answer.Contact.Name, nil
}

// postBatch streams a batch to a node. The node spools and checks it, and
// answers once it is installed and its summaries are rebuilt.
func (w *writer) postBatch(endpoint, dir string, replace bool) error {
	m, err := prep.ReadManifest(dir)
	if err != nil {
		return err
	}
	var size uint64
	for _, p := range m.Parts {
		size += p.Bytes
	}
	body, pipe := io.Pipe()
	go func() { pipe.CloseWithError(storage.PackBatch(pipe, dir)) }()
	url := endpoint + "/bulk"
	if replace {
		url += "?replace=1"
	}
	request, err := http.NewRequest(http.MethodPost, url, body)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/x-tar")
	request.Header.Set("Authorization", "Bearer "+w.token)
	started := time.Now()
	// No client timeout: a batch takes as long as its bytes.
	response, err := (&http.Client{}).Do(request)
	if err != nil {
		return fmt.Errorf("%s: %w", url, err)
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(response.Body, 1<<16))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d %s", url, response.StatusCode, bytes.TrimSpace(answer))
	}
	var result storage.BulkResult
	if err := json.Unmarshal(answer, &result); err != nil {
		return err
	}
	elapsed := time.Since(started)
	state := "installed"
	if result.Already {
		state = "already installed"
	}
	fmt.Printf("bulk %s: %s %s/%s, %d records, %d zones, %.1f MB in %.2fs (%.0f records/s)\n", state, result.Batch, m.Collection, result.Zone, m.Records, result.Zones, float64(size)/1e6, elapsed.Seconds(), float64(m.Records)/elapsed.Seconds())
	return nil
}
