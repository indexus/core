package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// Pairs from sdk: the golden locations of docs/alphabet-migration.md and
// the output of scripts/migrate/translate-locations.mjs.
func TestTranslateLocationMatchesTheSDK(t *testing.T) {
	for _, v := range []struct{ collection, old, new string }{
		{"DvFMV2020idx0001", "7xS6R2oo79xaZTlp", "5xS4R2qq77xabVnr"}, // Paris, depth 16
		{"ScenaroChrono01", "n4NEEer5Ir", "n2NEEer3Ir"},
		{"AngVegS7c2ff1c79", "A9z", "A7z"},
		{"dvf", "VxsAAAAA", "Vxsdvfdv"}, // past the name the old client read cell 0
		{"dvf", "@", "@"},
	} {
		got, err := translateLocation(v.collection, v.old)
		if err != nil || got != v.new {
			t.Fatalf("%s %s: got %q, %v; want %q", v.collection, v.old, got, err, v.new)
		}
	}
	if _, err := translateLocation("dvf", "a.b"); err == nil {
		t.Fatal("a character outside the alphabet must be refused")
	}
}

func content(collection, path string) storage.Record {
	payload := fmt.Sprintf(`{"path":%q,"encoding":"gzip-base64","sha256":"00","bytes":1,"part":0,"parts":1,"data":"AA=="}`, path)
	return storage.Record{Collection: collection, Location: "Q2xQ2xQ2xQ2xQ2xQ", ID: "part00000", Metrics: []float64{1}, Payload: json.RawMessage(payload)}
}

func TestExportFilterKeepsTheExperienceAndTranslatesNativeRecords(t *testing.T) {
	f := exportFilter{
		collections: []string{"DvFMV2020idx0001", "AromeWinter00001", "Ang*"},
		paths:       []string{"world/anglet", "data/indexus/arome/"},
		translate:   []string{"DvFMV2020idx0001", "AngVegS*"},
	}
	dvf := storage.Record{Collection: "DvFMV2020idx0001", Location: "7xS6R2oo79xaZTlp", ID: "a", Metrics: []float64{1}}
	for _, c := range []struct {
		record storage.Record
		keep   bool
	}{
		{dvf, true},
		{storage.Record{Collection: "AromeWinter00001", Location: "A", ID: "manifest", Payload: json.RawMessage(`{"schema":"indexus-weather-tiles-v1"}`)}, true},
		{content("AngletKmRoof0001", "world/anglet-1km/roofs/r0.bin"), true},
		{content("LocalAssetData01", "world/anglet-1km/points-ign/manifest.json"), true},
		{content("LocalAssetData01", "data/indexus/arome/v1/t0.bin"), true},
		{content("LocalAssetData01", "world/foret/points-ign/manifest.json"), false},
		{content("LocalAssetData01", "points-ign/manifest.json"), false},
		{storage.Record{Collection: "DrawAdaptive0001", Location: "AB", ID: "cell"}, false},
		{storage.Record{Collection: "DvFMV2020idx0001", Location: "7xS6R2oo79xaZTlp", ID: "gone", Deleted: true}, false},
	} {
		path, content := contentPath(c.record)
		if got := f.keep(c.record, path, content); got != c.keep {
			t.Fatalf("%s %s: keep %v, want %v", c.record.Collection, c.record.ID, got, c.keep)
		}
	}
	changed, err := f.apply(&dvf, false)
	if err != nil || !changed || dvf.Location != "5xS4R2qq77xabVnr" {
		t.Fatalf("DVF: %v %v %s", changed, err, dvf.Location)
	}
	leaf := storage.Record{Collection: "AngVegS7c2ff1c79", Location: "A9z", ID: "6/1/3", Payload: json.RawMessage(`{"schema":"indexus-entity-leaf-v1"}`)}
	if changed, err = f.apply(&leaf, false); err != nil || !changed || leaf.Location != "A7z" {
		t.Fatalf("entity leaf: %v %v %s", changed, err, leaf.Location)
	}
	// Content is addressed by the hash of its path, whatever its collection.
	part := content("AngVegS7c2ff1c79", "world/anglet-1km/x.bin")
	_, isContent := contentPath(part)
	if changed, err = f.apply(&part, isContent); err != nil || changed || part.Location != "Q2xQ2xQ2xQ2xQ2xQ" {
		t.Fatalf("content part: %v %v %s", changed, err, part.Location)
	}
	weather := storage.Record{Collection: "AromeWinter00001", Location: "B", ID: "manifest"}
	if changed, _ = f.apply(&weather, false); changed || weather.Location != "B" {
		t.Fatal("a collection that is not named keeps its fixed keys")
	}
	// -within keeps a small area of DVF, judged on the translated location.
	f.within = []string{"DvFMV2020idx0001:5xS"}
	if !f.inside(dvf, false) || f.inside(storage.Record{Collection: "DvFMV2020idx0001", Location: "qugC"}, false) {
		t.Fatal("within: DVF under 5xS only")
	}
	if !f.inside(weather, false) || !f.inside(part, true) {
		t.Fatal("within: other collections and content are kept")
	}
}

func TestThreeNamesShareClusteredAndHashedRecords(t *testing.T) {
	var input bytes.Buffer
	encoder := json.NewEncoder(&input)
	var rows []storage.Record
	// Spatial records crowd one corner of the tree; content parts are hashed.
	for i := range 3000 {
		location := "5xS4R2" + tree.EncodeName(tree.FromSlice([]byte{byte(i), byte(i >> 8), byte(i * 7), 1, 2, 3, 4, 5, 6, 7, 8, 9}))[:10]
		rows = append(rows, storage.Record{Collection: "DvFMV2020idx0001", Location: location, ID: fmt.Sprint(i), Metrics: []float64{1}})
	}
	for i := range 300 {
		path := fmt.Sprint("world/anglet-1km/", i)
		r := content("LocalAssetData01", path)
		hash := sha256.Sum256([]byte(path))
		r.Location = tree.EncodeName(tree.FromSlice(hash[:12]))
		r.Payload = json.RawMessage(strings.Replace(string(r.Payload), `"AA=="`, `"`+strings.Repeat("A", 4000)+`"`, 1))
		rows = append(rows, r)
	}
	for _, r := range rows {
		if err := encoder.Encode(r); err != nil {
			t.Fatal(err)
		}
	}
	names, err := threeNames(&input)
	if err != nil {
		t.Fatal(err)
	}
	var active []tree.VNodeDescriptor
	for _, name := range names {
		id, err := tree.DecodeName(name)
		if err != nil {
			t.Fatal(err)
		}
		active = append(active, tree.VNodeDescriptor{Name: name, ID: id})
	}
	view, err := tree.NewViewFrom(1, active)
	if err != nil {
		t.Fatal(err)
	}
	spatial, hashed := map[string]int{}, map[string]int{}
	for _, r := range rows {
		id, err := tree.ZoneID(r.Collection, r.Location)
		if err != nil {
			t.Fatal(err)
		}
		owner, _ := view.OwnerName(id)
		if r.Collection == "DvFMV2020idx0001" {
			spatial[owner]++
		} else {
			hashed[owner]++
		}
	}
	for _, name := range names {
		// Two bits cannot cut a uniform set in thirds: a half and two quarters
		// is the best case for the hashed parts.
		if spatial[name] < 750 || hashed[name] < 50 {
			t.Fatalf("uneven: spatial %v, hashed %v (names %v)", spatial, hashed, names)
		}
	}
}

func TestWriteRetriesBusyNodesAndKeepsTheRequestID(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	ids := map[string]string{}
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Item struct {
				RequestID string `json:"request_id"`
				ID        string `json:"id"`
				Payload   json.RawMessage
			} `json:"item"`
			Root string `json:"root"`
		}
		if r.Header.Get("Authorization") != "Bearer token" || json.NewDecoder(r.Body).Decode(&body) != nil || body.Root != "@" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		seen[body.Item.ID]++
		if prior, ok := ids[body.Item.ID]; ok && prior != body.Item.RequestID {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		ids[body.Item.ID] = body.Item.RequestID
		switch {
		case body.Item.ID == "conflict":
			w.WriteHeader(http.StatusConflict)
		case seen[body.Item.ID] == 1:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer node.Close()
	w := &writer{endpoints: []string{node.URL}, token: "token", client: node.Client(), retryFor: 5 * time.Second}
	rows := `{"Collection":"c","Location":"A","ID":"one","Metrics":[1]}
{"Collection":"c","Location":"B","ID":"two","Metrics":[2],"Payload":{"path":"x"}}
`
	count, err := w.writeRecords(strings.NewReader(rows), 2)
	if err != nil || count != 2 || seen["one"] != 2 || seen["two"] != 2 {
		t.Fatalf("count %d, err %v, seen %v", count, err, seen)
	}
	_, err = w.writeRecords(strings.NewReader(`{"Collection":"c","Location":"C","ID":"conflict","Metrics":[1]}`+"\n"), 1)
	if err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("a conflict is final: %v", err)
	}
	if _, err = w.writeRecords(io.MultiReader(strings.NewReader("not json\n")), 1); err == nil {
		t.Fatal("an invalid line is refused")
	}
}

// A rerun replays the same write; a new version of a record is a new write.
func TestRequestIDNamesOneVersionOfARecord(t *testing.T) {
	r := storage.Record{Collection: "AngVegSplat00001", Location: "K3iC", ID: "returns", Metrics: []float64{1, 2}, Payload: json.RawMessage(`{"a":1}`)}
	same := r
	if requestID(r) != requestID(same) {
		t.Fatal("same record, other id")
	}
	zero := r
	zero.Metrics = []float64{0, 0}
	other := r
	other.Payload = json.RawMessage(`{"a":2}`)
	if requestID(zero) == requestID(r) || requestID(other) == requestID(r) {
		t.Fatal("a new version must be a new write")
	}
}
