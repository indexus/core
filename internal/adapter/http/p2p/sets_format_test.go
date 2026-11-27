package p2p

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	indexus "github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/wire"
)

// setsFormatService answers both encodings of one read and records which
// façade entry the handler chose.
type setsFormatService struct {
	countingService
	sets     map[string]*indexus.Set
	calls    []string
	policy   *indexus.ReadOptions
	deep     bool
	blockErr error
	width    wire.SetsBlockMetrics
	columns  []int
}

func (s *setsFormatService) GetSetsBlockContext(_ context.Context, _ string, locations []string, deep bool, _ indexus.Visited, _ bool, options *indexus.ReadOptions, width wire.SetsBlockMetrics, columns []int) ([]byte, error) {
	s.calls = append(s.calls, "block")
	s.deep, s.policy = deep, options
	if s.blockErr != nil {
		return nil, s.blockErr
	}
	s.width, s.columns = width, columns
	return wire.EncodeSetsBlockColumns(s.sets, locations, wire.SetsPrecision, nil, width, columns)
}

func (s *setsFormatService) RoutingHint([]byte) indexus.Contact {
	return &stubHintContact{name: "hint", ip: "10.0.0.9", port: 21009}
}

func TestGetSetsAnswersIXB2(t *testing.T) {
	set := wire.NewSet()
	set.Put("7a", wire.NewAbelian(2, []float64{0, 0, 3, 0.5, 0.25}))
	set.Put("b", wire.NewAbelian(1, nil))
	sets := map[string]*indexus.Set{"@": set}
	block, err := wire.EncodeSetsBlock(sets, []string{"@"}, wire.SetsPrecision, nil, wire.SetsBlockF32)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		query       string
		call        string
		contentType string
		body        []byte
		mode        string
		deep        bool
	}{
		// IXB2 whatever the format asked: v1 is gone.
		{"", "block", wire.SetsBlockContentType, block, "", true},
		{"&format=v1", "block", wire.SetsBlockContentType, block, "", true},
		{"&read_mode=fresh", "block", wire.SetsBlockContentType, block, "fresh", true},
		{"&format=block&deep=false&envelope=1", "block", wire.SetsBlockContentType, block, "", false},
		{"&format=block&read_mode=fresh", "block", wire.SetsBlockContentType, block, "fresh", true},
	}
	for _, c := range cases {
		t.Run(c.query, func(t *testing.T) {
			service := &setsFormatService{sets: sets}
			server := httptest.NewServer(NewHttpHandler("", service, nil).server.Handler)
			defer server.Close()
			request, err := http.NewRequest(http.MethodGet, server.URL+"/sets?collection=demo&location=@"+c.query, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("X-Indexus-Routing-Key", "AAAAAAAAAAAAAAAAAAAAAA")
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK || fmt.Sprint(service.calls) != "["+c.call+"]" {
				t.Fatalf("status=%d calls=%v", response.StatusCode, service.calls)
			}
			if got := response.Header.Get("Content-Type"); got != c.contentType {
				t.Fatalf("content type %q", got)
			}
			if !bytes.Equal(body, c.body) {
				t.Fatalf("body %x, want %x", body, c.body)
			}
			if response.Header.Get("X-Indexus-Read-Mode") != c.mode || response.Header.Get("X-Indexus-Ingress-Port") != "21009" {
				t.Fatalf("headers %v", response.Header)
			}
			if c.call == "block" && (service.deep != c.deep || (service.policy != nil) != (c.mode != "")) {
				t.Fatalf("block read deep=%v policy=%v", service.deep, service.policy)
			}
		})
	}
}

func TestGetSetsBlockFailureIsNotABody(t *testing.T) {
	service := &setsFormatService{blockErr: errors.New("holder unavailable")}
	h := &Handler{Service: service}
	recorder := httptest.NewRecorder()
	h.GetSets(recorder, httptest.NewRequest(http.MethodGet, "/sets?collection=demo&location=@&format=block", nil))
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Content-Type") == wire.SetsBlockContentType {
		t.Fatalf("status=%d type=%q", recorder.Code, recorder.Header().Get("Content-Type"))
	}
}

// A browser on another origin must be able to read an IXB2 answer. Content-Type
// and Content-Length are CORS-safelisted response headers and need no
// exposure; Content-Encoding is undone by the browser before script sees the
// body. The routing-key request header must pass the preflight, and the
// ingress hint must be exposed.
func TestGetSetsBlockIsReadableCrossOrigin(t *testing.T) {
	set := wire.NewSet()
	for i := 0; i < 400; i++ {
		set.Put(fmt.Sprintf("%04x", i), wire.NewAbelian(1, []float64{float64(i)}))
	}
	service := &setsFormatService{sets: map[string]*indexus.Set{"@": set}}
	server := httptest.NewServer(NewHttpHandler("", service, nil).server.Handler)
	defer server.Close()
	url := server.URL + "/sets?collection=demo&location=@&format=block"

	preflight, _ := http.NewRequest(http.MethodOptions, url, nil)
	preflight.Header.Set("Origin", "https://app.example")
	preflight.Header.Set("Access-Control-Request-Method", "GET")
	preflight.Header.Set("Access-Control-Request-Headers", "x-indexus-routing-key")
	response, err := http.DefaultClient.Do(preflight)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.Header.Get("Access-Control-Allow-Origin") != "*" ||
		!strings.Contains(strings.ToLower(response.Header.Get("Access-Control-Allow-Headers")), "x-indexus-routing-key") {
		t.Fatalf("preflight headers %v", response.Header)
	}

	request, _ := http.NewRequest(http.MethodGet, url, nil)
	request.Header.Set("Origin", "https://app.example")
	request.Header.Set("X-Indexus-Routing-Key", "AAAAAAAAAAAAAAAAAAAAAA")
	transport := &http.Transport{DisableCompression: true}
	defer transport.CloseIdleConnections()
	request.Header.Set("Accept-Encoding", "gzip")
	response, err = (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	exposed := strings.ToLower(response.Header.Get("Access-Control-Expose-Headers"))
	if response.Header.Get("Access-Control-Allow-Origin") != "*" ||
		response.Header.Get("Content-Type") != wire.SetsBlockContentType ||
		response.Header.Get("Content-Encoding") != "gzip" ||
		!strings.Contains(exposed, "x-indexus-ingress-port") {
		t.Fatalf("response headers %v", response.Header)
	}
}

func TestGetSetsBlockMetricsWidthFollowsTheQuery(t *testing.T) {
	set := wire.NewSet()
	set.Put("7a", wire.NewAbelian(2, []float64{0.1}))
	for query, want := range map[string]wire.SetsBlockMetrics{
		"":             wire.SetsBlockF32,
		"&metrics=f32": wire.SetsBlockF32,
		"&metrics=f64": wire.SetsBlockF64,
	} {
		service := &setsFormatService{sets: map[string]*indexus.Set{"@": set}}
		h := &Handler{Service: service}
		recorder := httptest.NewRecorder()
		h.GetSets(recorder, httptest.NewRequest(http.MethodGet, "/sets?collection=demo&location=@&format=block"+query, nil))
		body := recorder.Body.Bytes()
		if recorder.Code != http.StatusOK || service.width != want || len(body) < 16 || (body[6]&2 != 0) != (want == wire.SetsBlockF64) {
			t.Fatalf("%q: status=%d width=%d flags=%x", query, recorder.Code, service.width, body[6:8])
		}
	}
}

// columns=… selects metrics: the block names them and holds only them, with
// the counts of the whole records; a bad list is refused before any read.
func TestGetSetsSelectsColumns(t *testing.T) {
	set := wire.NewSet()
	set.Put("7a", wire.NewAbelian(2, []float64{1, 2, 3, 4}))
	service := &setsFormatService{sets: map[string]*indexus.Set{"@": set}}
	h := &Handler{Service: service}
	recorder := httptest.NewRecorder()
	h.GetSets(recorder, httptest.NewRequest(http.MethodGet, "/sets?collection=demo&location=@&columns=3,0", nil))
	block, err := wire.DecodeSetsBlock(recorder.Body.Bytes())
	if err != nil || fmt.Sprint(service.columns) != "[3 0]" || fmt.Sprint(block.Columns) != "[3 0]" {
		t.Fatalf("columns %v, block %+v, %v", service.columns, block, err)
	}
	section := block.Sections[0]
	if section.MetricCount != 2 || section.Count[0] != 2 || section.Metrics[0][0] != 4 || section.Metrics[1][0] != 1 {
		t.Fatalf("section %+v", section)
	}
	if got := block.Entries()["7a"]; got.Count() != 2 || got.Metric(3) != 4 || got.Metric(0) != 1 || got.Metric(1) != 0 {
		t.Fatalf("entries put metrics back at their positions: %v", got)
	}
	for _, bad := range []string{"x", "1,1", "-1"} {
		recorder = httptest.NewRecorder()
		service.calls = nil
		h.GetSets(recorder, httptest.NewRequest(http.MethodGet, "/sets?collection=demo&location=@&columns="+bad, nil))
		if recorder.Code != http.StatusBadRequest || service.calls != nil {
			t.Fatalf("columns=%s: %d %v", bad, recorder.Code, service.calls)
		}
	}
}
