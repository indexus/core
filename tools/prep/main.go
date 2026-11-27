// Command prep makes, checks and compares batches of records for the bulk
// path of a node (docs/ingest/pipeline.md).
//
//	prep ndjson -out DIR [-split 2] [-zones 0:2] [-part-mb 64] file.ndjson...
//	                                   one batch per collection in DIR/<collection>/,
//	                                   skipped when its inputs did not change
//	prep ndjson in.ndjson batch-dir    one collection, one batch
//	prep cat dir                       NDJSON in key order on stdout
//	prep verify [-tol 1e-9] dir        hashes, CRCs, key order, zone sums
//	prep compare [-tol 1e-9] dir-a dir-b
//	prep payloads in.ndjson...         the binary payloads (payloads.go) of the records of
//	                                   binary collections, as NDJSON with base64 payloads
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/indexus/go-indexus-core/internal/prep"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "ndjson":
		err = fromNDJSON(os.Args[2:])
	case "cat":
		err = cat(os.Args[2:], os.Stdout)
	case "verify":
		err = verify(os.Args[2:])
	case "compare":
		err = compare(os.Args[2:])
	case "payloads":
		err = payloads(os.Args[2:], os.Stdout)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "prep:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: prep ndjson|cat|verify|compare|payloads ... (see the package comment)")
	os.Exit(2)
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

func cat(args []string, w io.Writer) error {
	if len(args) != 1 {
		usage()
	}
	m, err := prep.ReadManifest(args[0])
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(w, 1<<20)
	defer bw.Flush()
	return prep.Each(args[0], m, func(r prep.Row) error {
		line := struct {
			Collection string
			Location   string
			ID         string
			Weight     *float64 `json:",omitempty"`
			Metrics    []float64
			Payload    json.RawMessage `json:",omitempty"`
		}{m.Collection, r.Head.Location(), string(r.ID), nil, r.Metrics, jsonPayload(m.PayloadEncoding, r.Payload)}
		if r.Weight != 1 {
			line.Weight = &r.Weight
		}
		raw, err := json.Marshal(line)
		if err != nil {
			return err
		}
		bw.Write(raw)
		return bw.WriteByte('\n')
	})
}

// jsonPayload is the payload as a JSON value: as stored when the batch
// says "json", base64 in a string when it is binary.
func jsonPayload(enc string, p []byte) json.RawMessage {
	if len(p) == 0 {
		return nil
	}
	if enc == "json" {
		return json.RawMessage(p)
	}
	raw, _ := json.Marshal(p)
	return raw
}

// payloadBytes is the payload as bytes: a JSON string of base64 is decoded,
// so that batches of either encoding compare.
func payloadBytes(enc string, p []byte) []byte {
	if enc == "json" && len(p) > 0 && p[0] == '"' {
		var b []byte
		if json.Unmarshal(p, &b) == nil {
			return b
		}
	}
	return p
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	tol := fs.Float64("tol", 1e-9, "relative tolerance of zone sums")
	fs.Parse(args)
	if fs.NArg() != 1 {
		usage()
	}
	start := time.Now()
	m, worst, err := prep.Verify(fs.Arg(0), *tol)
	if err != nil {
		return err
	}
	zones := uint64(0)
	if m.Zones != nil {
		zones = m.Zones.Records
	}
	fmt.Printf("ok: %s, %d records in %d parts, %d zones (largest relative difference %.2g), %.2fs\n",
		m.Collection, m.Records, len(m.Parts), zones, worst, time.Since(start).Seconds())
	return nil
}

// compare joins two batches by key and reports what differs: keys on one
// side only, metrics past the tolerance, payloads.
func compare(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	tol := fs.Float64("tol", 1e-9, "relative tolerance of metrics")
	abs := fs.Float64("abs", 0, "absolute tolerance of metrics: a difference under it is equal whatever the relative one")
	show := fs.Int("show", 5, "differences printed")
	fs.Parse(args)
	if fs.NArg() != 2 {
		usage()
	}
	next := func(dir string) (func() (prep.Row, bool), func() error, error) {
		m, err := prep.ReadManifest(dir)
		if err != nil {
			return nil, nil, err
		}
		enc := m.PayloadEncoding
		ch := make(chan prep.Row, 1024)
		done := make(chan error, 1)
		go func() {
			done <- prep.Each(dir, m, func(r prep.Row) error {
				r.ID = append([]byte(nil), r.ID...)
				r.Metrics = append([]float64(nil), r.Metrics...)
				r.Payload = append([]byte(nil), payloadBytes(enc, r.Payload)...)
				ch <- r
				return nil
			})
			close(ch)
		}()
		return func() (prep.Row, bool) { r, ok := <-ch; return r, ok }, func() error { return <-done }, nil
	}
	na, waitA, err := next(fs.Arg(0))
	if err != nil {
		return err
	}
	nb, waitB, err := next(fs.Arg(1))
	if err != nil {
		return err
	}
	var same, onlyA, onlyB, metricDiff, payloadDiff, payloadBytesDiff, payloadBytes int
	var worst, worstAbs float64
	shown := 0
	say := func(format string, a ...any) {
		if shown < *show {
			fmt.Printf(format+"\n", a...)
			shown++
		}
	}
	a, okA := na()
	b, okB := nb()
	for okA || okB {
		c := 0
		switch {
		case !okA:
			c = 1
		case !okB:
			c = -1
		default:
			c = strings.Compare(string(a.Head[:])+"\x00"+string(a.ID), string(b.Head[:])+"\x00"+string(b.ID))
		}
		if c < 0 {
			onlyA++
			say("only in a: %s/%s", a.Head.Location(), a.ID)
			a, okA = na()
			continue
		}
		if c > 0 {
			onlyB++
			say("only in b: %s/%s", b.Head.Location(), b.ID)
			b, okB = nb()
			continue
		}
		bad := len(a.Metrics) != len(b.Metrics) || a.Weight != b.Weight
		for j := 0; !bad && j < len(a.Metrics); j++ {
			x, y := a.Metrics[j], b.Metrics[j]
			if x == y {
				continue
			}
			d := math.Abs(x-y) / math.Max(math.Abs(x), math.Abs(y))
			worst = math.Max(worst, d)
			worstAbs = math.Max(worstAbs, math.Abs(x-y))
			if d > *tol && math.Abs(x-y) > *abs {
				bad = true
				say("metrics differ at %s/%s, column %d: %v and %v", a.Head.Location(), a.ID, j, x, y)
			}
		}
		if bad {
			metricDiff++
		}
		payloadBytes += len(a.Payload)
		if string(a.Payload) != string(b.Payload) {
			payloadDiff++
			if len(a.Payload) != len(b.Payload) {
				payloadBytesDiff += max(len(a.Payload), len(b.Payload))
				say("payload sizes differ at %s/%s: %d and %d", a.Head.Location(), a.ID, len(a.Payload), len(b.Payload))
			} else {
				for i := range a.Payload {
					if a.Payload[i] != b.Payload[i] {
						payloadBytesDiff++
					}
				}
			}
		}
		if !bad {
			same++
		}
		a, okA = na()
		b, okB = nb()
	}
	if err := waitA(); err != nil {
		return err
	}
	if err := waitB(); err != nil {
		return err
	}
	fmt.Printf("records: %d equal in keys and metrics (largest difference %.2g relative, %.2g absolute), %d past %.0g relative and %.0g absolute, %d only in a, %d only in b\n",
		same, worst, worstAbs, metricDiff, *tol, *abs, onlyA, onlyB)
	fmt.Printf("payloads: %d records differ, %d of %d bytes differ\n", payloadDiff, payloadBytesDiff, payloadBytes)
	if onlyA+onlyB+metricDiff > 0 {
		return fmt.Errorf("batches differ")
	}
	return nil
}
