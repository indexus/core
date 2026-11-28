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
	for _, width := range []struct {
		name string
		mode wire.SetsBlockMetrics
	}{{"f32", wire.SetsBlockF32}, {"f64", wire.SetsBlockF64}} {
		for _, projection := range []struct {
			name    string
			columns []int
		}{{"all", nil}, {"selected", []int{7, 0, 3}}} {
			body, err := wire.EncodeSetsBlockColumns(map[string]*wire.Set{"@": set}, []string{"@"}, wire.SetsPrecision, nil, width.mode, projection.columns)
			if err != nil {
				panic(err)
			}
			if err = os.WriteFile(filepath.Join(os.Args[1], width.name+"-"+projection.name+".bin"), body, 0644); err != nil {
				panic(err)
			}
		}
	}
}
