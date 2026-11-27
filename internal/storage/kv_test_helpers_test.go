package storage

import (
	"testing"

	"github.com/indexus/go-indexus-core/internal/zonelog"
)

// testKV is a bare control store for the group-commit tests: what a raw
// bbolt file was to them.
type testKV struct{ s *Store }

func openTestKV(t testing.TB, path string) (*testKV, error) {
	db, err := zonelog.Open(RecordsDir(path), zonelog.Options{NoSync: true})
	if err != nil {
		return nil, err
	}
	return &testKV{&Store{db: db, path: path}}, nil
}

func (k *testKV) Update(fn func(*Tx) error) error { return k.s.update(fn) }
func (k *testKV) View(fn func(*Tx) error) error   { return k.s.view(fn) }
func (k *testKV) Close() error                    { return k.s.db.Close() }
