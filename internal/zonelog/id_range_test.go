package zonelog

import (
	"slices"
	"strings"
	"testing"
)

func TestIDRangeOnSnapshotAndStore(t *testing.T) {
	s, err := Open(t.TempDir(), Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	long := strings.Repeat("\xff", 255)
	all := []string{"a", "p", "p\x00", "p\x00\xff", "p\xff", "q", "\xff", "\xff\xff", long}
	var records []Record
	for _, id := range all {
		records = append(records, Record{Collection: "c", Location: "Z", ID: id, Payload: []byte(id)})
	}
	records = append(records, Record{Collection: "c", Location: "Z0", ID: "p"}, Record{Collection: "other", Location: "Z", ID: "p"})
	if err := s.Apply(records); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	old := s.Snapshot()
	defer old.Close()
	if err := s.Apply([]Record{{Collection: "c", Location: "Z", ID: "p\x00", Deleted: true}}); err != nil {
		t.Fatal(err)
	}
	for _, view := range []struct {
		name    string
		r       Reader
		deleted bool
	}{{"snapshot", old, false}, {"store", s, true}} {
		for _, tc := range []struct {
			name, prefix, from string
			after              *Record
			want               []string
		}{
			{"exact location", "", "", nil, all},
			{"prefix and its extensions", "p", "", nil, all[1:5]},
			{"inclusive synthetic zero", "p", "p\x00", nil, all[2:5]},
			{"exclusive real cursor", "p", "p", &Record{Location: "Z", ID: "p\x00"}, all[3:5]},
			{"trailing ff", "p\xff", "", nil, all[4:5]},
			{"all ff", "\xff", "", nil, all[6:]},
			{"unbounded prefix after range", "p", "q", nil, nil},
			{"cursor after range", "p", "", &Record{Location: "Z", ID: "q"}, nil},
			{"cursor before range", "p", "", &Record{Location: "@", ID: "x"}, all[1:5]},
			{"255 byte prefix", long, "", nil, all[len(all)-1:]},
			{"past longest possible ID", long, long + "\x00", nil, nil},
		} {
			t.Run(view.name+"/"+tc.name, func(t *testing.T) {
				want := slices.Clone(tc.want)
				if view.deleted {
					want = slices.DeleteFunc(want, func(id string) bool { return id == "p\x00" })
				}
				var got []string
				err := view.r.ScanWith("c", "Z", tc.after, &IDRange{Prefix: tc.prefix, From: tc.from}, func(Record) bool { return true }, func(r Record, size int) bool {
					if r.Collection != "c" || r.Location != "Z" || string(r.Payload) != r.ID || size != len(r.ID) {
						t.Fatalf("wrong location, collection or payload: %+v (%d bytes)", r, size)
					}
					got = append(got, r.ID)
					return true
				})
				if err != nil || !slices.Equal(got, want) {
					t.Fatalf("scan returned %q, %v; want %q", got, err, want)
				}
			})
		}
	}
}
