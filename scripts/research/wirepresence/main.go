// wirepresence emits IXB2 test blocks for the SDK presence experiment.
// Run go run ./scripts/research/wirepresence <existing-output-directory>,
// then node scripts/research/wirepresence/check.mjs <sdk-directory> <output-directory>.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/indexus/go-indexus-core/internal/wire"
)

const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"

func main() {
	if len(os.Args) != 2 {
		panic("expected output directory")
	}
	set := wire.NewSet()
	for mask := range 256 {
		key := string([]byte{alphabet[mask/64], alphabet[mask%64]})
		for copy := range 2 {
			cols := make([]float64, 8)
			for bit := range cols {
				cols[bit] = float64(mask >> bit & 1)
			}
			set.Put(fmt.Sprintf("%s:%d", key, copy), wire.NewAbelian(1, cols))
		}
	}
	wide := wire.NewSet()
	for key, present := range map[string][]int{"A": {0, 63, 65, 257}, "B": {1, 64, 299}} {
		cols := make([]float64, 300)
		for _, col := range present {
			cols[col] = 1
		}
		wide.Put(key+":item", wire.NewAbelian(1, cols))
	}
	for _, width := range []struct {
		name string
		mode wire.SetsBlockMetrics
	}{{"f32", wire.SetsBlockF32}, {"f64", wire.SetsBlockF64}} {
		selected := []int{299, 2, 63, 257, 65, 1, 260, 64}
		for _, fixture := range []struct {
			name      string
			rows      *wire.Set
			columns   []int
			redirects []wire.SetsRedirect
		}{
			{"all", set, nil, nil}, {"selected", set, []int{7, 0, 3}, nil},
			{"wide", wide, nil, nil}, {"wide-selected", wide, selected, nil},
			{"wide-padded", wide, []int{511, 2}, nil},
			{"wide-redirect", wide, selected, []wire.SetsRedirect{{Location: "C", Name: "peer", IP: "127.0.0.1", Port: 8080}}},
		} {
			body, err := wire.EncodeSetsBlockColumns(map[string]*wire.Set{"@": fixture.rows}, []string{"@"}, wire.SetsPrecision, fixture.redirects, width.mode, fixture.columns)
			if err != nil {
				panic(err)
			}
			if err = os.WriteFile(filepath.Join(os.Args[1], width.name+"-"+fixture.name+".bin"), body, 0644); err != nil {
				panic(err)
			}
		}
	}
}
