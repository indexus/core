package p2p

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	indexus "github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/wire"
)

type budgetRecordsService struct {
	countingService
}

func (s *budgetRecordsService) GetRecordsContext(context.Context, string, string, int) ([]indexus.Item, error) {
	return nil, fmt.Errorf("inventory: %w", indexus.ErrQueryBudget)
}

func TestGetRecordsMapsQueryBudgetThroughFacade(t *testing.T) {
	h := &Handler{Service: &budgetRecordsService{}}
	recorder := httptest.NewRecorder()
	h.GetRecords(recorder, httptest.NewRequest(http.MethodGet, "/records?collection=demo&location=@&limit=10", nil))
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d", recorder.Code)
	}
}

type fixedRecordsService struct {
	countingService
	items []indexus.Item
}

func (s *fixedRecordsService) GetRecordsContext(context.Context, string, string, int) ([]indexus.Item, error) {
	return s.items, nil
}

// /records?format=binary sends IXRB: a binary payload as its bytes, a JSON
// one as its text. Without format it stays JSON.
func TestGetRecordsServesBinaryPayloadsWhenAsked(t *testing.T) {
	raw := []byte{0xff, 0x00, '{'}
	b64, _ := json.Marshal(raw)
	items := []indexus.Item{
		{Collection: "demo", Location: "ab", Id: "1", Metrics: []float64{1, 2.5}, Payload: b64, Binary: true, Weight: 3},
		{Collection: "demo", Location: "ac", Id: "2", Payload: json.RawMessage(`{"a":1}`)},
		{Collection: "demo", Location: "ad", Id: "3"},
	}
	h := &Handler{Service: &fixedRecordsService{items: items}}
	recorder := httptest.NewRecorder()
	h.GetRecords(recorder, httptest.NewRequest(http.MethodGet, "/records?collection=demo&location=@&limit=10&format=binary", nil))
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != wire.RecordsContentType {
		t.Fatalf("status=%d type=%q", recorder.Code, recorder.Header().Get("Content-Type"))
	}
	got, err := wire.DecodeRecords("demo", recorder.Body.Bytes())
	if err != nil || len(got) != 3 {
		t.Fatalf("decode: %v %v", got, err)
	}
	if !got[0].Binary || !bytes.Equal(got[0].Payload, raw) || got[0].Weight != 3 || got[0].Metrics[1] != 2.5 {
		t.Fatalf("binary record: %+v", got[0])
	}
	if got[1].Binary || string(got[1].Payload) != `{"a":1}` || got[2].Payload != nil || got[2].Location != "ad" {
		t.Fatalf("json records: %+v", got[1:])
	}
	if _, err := wire.DecodeRecords("demo", recorder.Body.Bytes()[:recorder.Body.Len()-1]); err == nil {
		t.Fatal("a truncated body decoded")
	}
	recorder = httptest.NewRecorder()
	h.GetRecords(recorder, httptest.NewRequest(http.MethodGet, "/records?collection=demo&location=@&limit=10", nil))
	var body struct{ Records []indexus.Item }
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || len(body.Records) != 3 || string(body.Records[0].Payload) != string(b64) {
		t.Fatalf("json default: %s %v", recorder.Body.String(), err)
	}
}

// /records is gzipped when the client accepts it, in both formats.
func TestGetRecordsGzipsWhenAccepted(t *testing.T) {
	items := []indexus.Item{{Collection: "demo", Location: "ab", Id: "1", Payload: json.RawMessage(`"` + strings.Repeat("A", 4000) + `"`), Binary: true}}
	h := &Handler{Service: &fixedRecordsService{items: items}}
	for _, format := range []string{"", "&format=binary"} {
		request := httptest.NewRequest(http.MethodGet, "/records?collection=demo&location=@&limit=10"+format, nil)
		request.Header.Set("Accept-Encoding", "gzip")
		recorder := httptest.NewRecorder()
		h.GetRecords(recorder, request)
		if recorder.Header().Get("Content-Encoding") != "gzip" || recorder.Body.Len() > 500 {
			t.Fatalf("%q: encoding %q, %d bytes", format, recorder.Header().Get("Content-Encoding"), recorder.Body.Len())
		}
		zr, err := gzip.NewReader(recorder.Body)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(zr)
		if err != nil {
			t.Fatal(err)
		}
		if format == "" {
			var out struct{ Records []indexus.Item }
			if err := json.Unmarshal(body, &out); err != nil || len(out.Records) != 1 {
				t.Fatalf("json: %v", err)
			}
		} else if got, err := wire.DecodeRecords("demo", body); err != nil || len(got) != 1 || len(got[0].Payload) != 3000 {
			t.Fatalf("ixrb: %v", err)
		}
	}
}

type fakeDegradedService struct {
	countingService
	items map[string]*indexus.Item
}

func (f *fakeDegradedService) DegradedRead(_ context.Context, col, loc, id string) (*indexus.Item, bool, error) {
	it, ok := f.items[col+"/"+loc+"/"+id]
	return it, ok, nil
}

func TestGetDegradedItemEndpoint(t *testing.T) {
	svc := &fakeDegradedService{
		items: map[string]*indexus.Item{
			"demo/ab/k1": {Collection: "demo", Location: "ab", Id: "k1", Metrics: []float64{42}},
		},
	}
	h := NewHttpHandler("", svc, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/item/degraded?collection=demo&location=ab&id=k1", nil)
	h.GetDegradedItem(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("X-Indexus-Degraded") != "true" {
		t.Fatalf("found: status=%d header=%q body=%s", rec.Code, rec.Header().Get("X-Indexus-Degraded"), rec.Body.String())
	}
	var got struct {
		Degraded bool          `json:"degraded"`
		Item     *indexus.Item `json:"item"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || !got.Degraded || got.Item == nil || got.Item.Metrics[0] != 42 {
		t.Fatalf("decoded = %+v err=%v", got, err)
	}

	miss := httptest.NewRecorder()
	reqMiss := httptest.NewRequest(http.MethodGet, "/item/degraded?collection=demo&location=ab&id=missing", nil)
	h.GetDegradedItem(miss, reqMiss)
	if miss.Code != http.StatusNotFound || miss.Header().Get("X-Indexus-Degraded") != "true" {
		t.Fatalf("missing: status=%d header=%q", miss.Code, miss.Header().Get("X-Indexus-Degraded"))
	}
}
