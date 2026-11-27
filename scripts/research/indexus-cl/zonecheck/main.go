// Command zonecheck feeds the continual-learning statistics through the real
// zonelog engine and prints what it reads back, for comparison with numpy.
//
// Each record is one example: location = class character + spatial address,
// metrics = [x (k), vech(x x^T) (k(k+1)/2)]. Zone totals then give exactly the
// sufficient statistics: a class zone holds n_c and sum x, the root holds
// sum x x^T. Stores:
//
//	A  file order, batches of 1000, a flush in the middle
//	B  shuffled order, batches of 777
//	C  A, then every 10th example deleted (Deleted records)
//	D  fresh store with only the examples C keeps
//	E  C, then the deleted examples written again (re-learning)
//
// Output: JSON lines {"store","zone","count","metrics"} with %.17g floats.
package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/indexus/go-indexus-core/internal/zonelog"
)

const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
const col = "cl"

type ex struct {
	class int
	loc   string
	m     []float64
}

func main() {
	in, outdir := os.Args[1], os.Args[2]
	f, err := os.Open(in)
	must(err)
	r := bufio.NewReader(f)
	var hdr [3]int32
	must(binary.Read(r, binary.LittleEndian, &hdr))
	n, k, C := int(hdr[0]), int(hdr[1]), int(hdr[2])
	exs := make([]ex, n)
	x := make([]float64, k)
	for i := range exs {
		var c uint16
		var loc [8]byte
		must(binary.Read(r, binary.LittleEndian, &c))
		must(binary.Read(r, binary.LittleEndian, &loc))
		must(binary.Read(r, binary.LittleEndian, x))
		m := make([]float64, 0, k+k*(k+1)/2)
		m = append(m, x...)
		for a := 0; a < k; a++ {
			for b := a; b < k; b++ {
				m = append(m, x[a]*x[b])
			}
		}
		exs[i] = ex{int(c), string(loc[:]), m}
	}
	fmt.Fprintf(os.Stderr, "read n=%d k=%d C=%d metrics=%d\n", n, k, C, len(exs[0].m))
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	rec := func(i int, del bool) zonelog.Record {
		if del {
			return zonelog.Record{Collection: col, Location: exs[i].loc, ID: strconv.Itoa(i), Deleted: true}
		}
		return zonelog.Record{Collection: col, Location: exs[i].loc, ID: strconv.Itoa(i), Metrics: exs[i].m}
	}
	write := func(s *zonelog.Store, order []int, batch int, del bool, flushAt int) time.Duration {
		t := time.Now()
		for lo := 0; lo < len(order); lo += batch {
			hi := min(lo+batch, len(order))
			b := make([]zonelog.Record, 0, hi-lo)
			for _, i := range order[lo:hi] {
				b = append(b, rec(i, del))
			}
			must(s.Apply(b))
			if flushAt > 0 && lo <= flushAt && flushAt < hi {
				must(s.Flush())
			}
		}
		return time.Since(t)
	}
	dump := func(name string, s *zonelog.Store) {
		must(s.Flush())
		zones := []string{"@"}
		for c := 0; c < C; c++ {
			zones = append(zones, alphabet[c:c+1])
		}
		for _, z := range zones {
			t, err := s.Total(col, z)
			must(err)
			parts := make([]string, len(t.Metrics))
			for i, v := range t.Metrics {
				parts[i] = strconv.FormatFloat(v, 'g', 17, 64)
			}
			fmt.Fprintf(out, "{\"store\":%q,\"zone\":%q,\"count\":%d,\"metrics\":[%s]}\n", name, z, t.Count, strings.Join(parts, ","))
		}
	}
	open := func(name string) *zonelog.Store {
		s, err := zonelog.Open(filepath.Join(outdir, name), zonelog.Options{NoSync: true})
		must(err)
		return s
	}
	all := make([]int, n)
	for i := range all {
		all[i] = i
	}
	rng := rand.New(rand.NewSource(42))
	shuf := append([]int(nil), all...)
	rng.Shuffle(len(shuf), func(i, j int) { shuf[i], shuf[j] = shuf[j], shuf[i] })
	var gone, kept []int
	for _, i := range all {
		if i%10 == 0 {
			gone = append(gone, i)
		} else {
			kept = append(kept, i)
		}
	}

	A := open("A")
	dA := write(A, all, 1000, false, n/2)
	dump("A", A)
	B := open("B")
	dB := write(B, shuf, 777, false, 0)
	dump("B", B)
	dC := write(A, gone, 1000, true, 0) // C is A after deletions
	dump("C", A)
	D := open("D")
	write(D, kept, 1000, false, 0)
	dump("D", D)
	write(A, gone, 1000, false, 0) // E: written again
	dump("E", A)
	for _, s := range []*zonelog.Store{A, B, D} {
		must(s.Close())
	}
	fmt.Fprintf(os.Stderr, "write A %.2fs (%.0f rec/s), B %.2fs, deletes %.2fs (%.0f del/s)\n",
		dA.Seconds(), float64(n)/dA.Seconds(), dB.Seconds(), dC.Seconds(), float64(len(gone))/dC.Seconds())
	_ = math.Pi
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "zonecheck:", err)
		os.Exit(1)
	}
}
