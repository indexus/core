package storage

import (
	"github.com/indexus/go-indexus-core/internal/tree"
	"github.com/indexus/go-indexus-core/internal/zonelog"
)

// ExportOfflineRecords hands every canonical record of a stopped store or a
// consistent backup to visit, in key order, from its records directory.
// Authority, keys and request receipts do not cross clusters.
func ExportOfflineRecords(path string, visit func(Record) error) error {
	db, err := zonelog.Open(RecordsDir(path), zonelog.Options{NoSync: true})
	if err != nil {
		return err
	}
	defer db.Close()
	return newTx(db, false).run(func(tx *Tx) error {
		return tx.walk(nil, nil, "", true, func(_ tree.ZoneKey, r *row) (bool, error) {
			if r == nil {
				return true, nil
			}
			return true, visit(*r.record)
		})
	})
}
