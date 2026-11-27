package p2p

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	indexus "github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/wire"
)

// countingService records whether the handler reached the node.
type countingService struct {
	transfers int
	bulks     int
}

func (s *countingService) Name() string                                  { return "receiver" }
func (s *countingService) Ping(indexus.Contact) (indexus.Contact, error) { return nil, nil }
func (s *countingService) Neighbors(indexus.Peer) ([]indexus.Contact, error) {
	return nil, nil
}
func (s *countingService) Random(indexus.Peer) (indexus.Contact, error) { return nil, nil }
func (s *countingService) Transfer(indexus.Peer, indexus.Key, []*indexus.Item) (string, error) {
	s.transfers++
	return s.Name(), nil
}
func (s *countingService) Copy(indexus.Peer, indexus.Key, []*indexus.Item) (string, error) {
	s.transfers++
	return s.Name(), nil
}
func (s *countingService) GetSetContext(context.Context, string, string, bool, indexus.Visited, bool) (indexus.Contact, *indexus.Set, error) {
	return nil, nil, nil
}
func (s *countingService) New(*indexus.Item, string, indexus.Visited) error     { return nil }
func (s *countingService) Handoff(*indexus.Item, string, indexus.Visited) error { return nil }
func (s *countingService) Delete(*indexus.Item, string, indexus.Visited) error  { return nil }
func (s *countingService) IngestBulk(context.Context, io.Reader, bool) (indexus.BulkResult, error) {
	s.bulks++
	return indexus.BulkResult{}, nil
}

// A bulk ingest is a write: without a write token it never reaches the node.
func TestBulkNeedsTheWriteScope(t *testing.T) {
	service := &countingService{}
	h := NewHttpHandler("", service, nil)
	h.RequireAuth = true
	response := httptest.NewRecorder()
	h.server.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/bulk", bytes.NewReader(nil)))
	if response.Code != http.StatusUnauthorized || service.bulks != 0 {
		t.Fatalf("unauthenticated bulk: %d, reached the node %d times", response.Code, service.bulks)
	}
	h.RequireAuth = false
	response = httptest.NewRecorder()
	h.server.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/bulk?replace=1", bytes.NewReader(nil)))
	if response.Code != http.StatusOK || service.bulks != 1 {
		t.Fatalf("bulk: %d, reached the node %d times", response.Code, service.bulks)
	}
}

// /transfer writes items into the node, so it has to be gated like /item.
// Leaving it open lets anyone inject or overwrite data on an authenticated mesh.

func TestParseGetFlags(t *testing.T) {
	cases := []struct {
		query       string
		wantDeep    bool
		wantVia     string
		wantRefresh bool
	}{
		{"", true, "", false},
		{"deep=true", true, "", false},
		{"deep=false", false, "", false},
		{"deep=0", false, "", false},
		{"deep=true&via=a,b", true, "a,b", false},
		{"refresh=true", true, "", true},
		{"refresh=1&deep=false&via=peer", false, "peer", true},
		{"refresh=false", true, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/set?"+tc.query, nil)
			deep, via, refresh := parseGetFlags(req.URL.Query())
			if deep != tc.wantDeep || via.String() != tc.wantVia || refresh != tc.wantRefresh {
				t.Fatalf("parseGetFlags(%q)=(%v,%q,%v) want (%v,%q,%v)",
					tc.query, deep, via.String(), refresh, tc.wantDeep, tc.wantVia, tc.wantRefresh)
			}
		})
	}
}

func TestDecodeRoutingKeyBase64URL(t *testing.T) {
	key := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	raw := base64.RawURLEncoding.EncodeToString(key)
	got, ok := decodeRoutingKey(raw)
	if !ok || !bytes.Equal(got, key) {
		t.Fatalf("decodeRoutingKey(%q)=(%v,%v)", raw, got, ok)
	}
	padded := base64.URLEncoding.EncodeToString(key)
	got, ok = decodeRoutingKey(padded)
	if !ok || !bytes.Equal(got, key) {
		t.Fatalf("padded decodeRoutingKey(%q)=(%v,%v)", padded, got, ok)
	}
	for _, invalid := range []string{"", "!invalid", "a", "abc===", "key with spaces"} {
		if _, ok := decodeRoutingKey(invalid); ok {
			t.Fatalf("invalid routing key accepted: %q", invalid)
		}
	}
}

type hintService struct {
	countingService
	hint indexus.Contact
	last []byte
}

func (s *hintService) RoutingHint(key []byte) indexus.Contact {
	s.last = append([]byte(nil), key...)
	return s.hint
}

type stubHintContact struct {
	name string
	ip   string
	port int
}

func (c *stubHintContact) ID() []byte                                        { return nil }
func (c *stubHintContact) Name() string                                      { return c.name }
func (c *stubHintContact) IPs() map[string]any                               { return map[string]any{c.ip: nil} }
func (c *stubHintContact) Port() int                                         { return c.port }
func (c *stubHintContact) IP() string                                        { return c.ip }
func (c *stubHintContact) Host() string                                      { return c.ip }
func (c *stubHintContact) Ping(indexus.Contact) (indexus.Contact, error)     { return nil, nil }
func (c *stubHintContact) Neighbors(indexus.Peer) ([]indexus.Contact, error) { return nil, nil }
func (c *stubHintContact) Random(indexus.Peer) (indexus.Contact, error)      { return nil, nil }
func (c *stubHintContact) Transfer(indexus.Peer, indexus.Key, []*indexus.Item) (string, error) {
	return "", nil
}
func (c *stubHintContact) Copy(origin indexus.Peer, key indexus.Key, items []*indexus.Item) (string, error) {
	return c.Transfer(origin, key, items)
}
func (c *stubHintContact) Get(string, string, bool, indexus.Visited, bool) (indexus.Contact, *indexus.Set, error) {
	return nil, nil, nil
}
func (c *stubHintContact) New(*indexus.Item, string, indexus.Visited) error    { return nil }
func (c *stubHintContact) Delete(*indexus.Item, string, indexus.Visited) error { return nil }

func TestGetSetsExposesIngressHintHeaders(t *testing.T) {
	hint := &stubHintContact{name: "AAAAAAAAAAAAAAAAAAAAAA", ip: "10.0.0.2", port: 21002}
	service := &hintService{hint: hint}
	handler := &Handler{Service: service}

	key := make([]byte, 16)
	req := httptest.NewRequest(http.MethodGet, "/sets?collection=demo&location=qs", nil)
	req.Header.Set("X-Indexus-Routing-Key", base64.RawURLEncoding.EncodeToString(key))

	recorder := httptest.NewRecorder()
	handler.GetSets(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("X-Indexus-Ingress-Name"); got != hint.Name() {
		t.Fatalf("ingress name=%q want=%q", got, hint.Name())
	}
	if got := recorder.Header().Get("X-Indexus-Ingress-IP"); got != "10.0.0.2" {
		t.Fatalf("ingress ip=%q", got)
	}
	if got := recorder.Header().Get("X-Indexus-Ingress-Port"); got != "21002" {
		t.Fatalf("ingress port=%q", got)
	}
	if !bytes.Equal(service.last, key) {
		t.Fatalf("RoutingHint key=%v want=%v", service.last, key)
	}
}

// Auth is opt-in: a mesh running without a verifier must keep working.

func (s *countingService) ReadContext(ctx context.Context, _, _ string, _ indexus.ReadOptions) (indexus.ReadResult, error) {
	return indexus.ReadResult{}, ctx.Err()
}
func (s *countingService) GetRecordsContext(ctx context.Context, _, _ string, _ int) ([]indexus.Item, error) {
	return nil, ctx.Err()
}
func (s *countingService) GetSetsBlockContext(ctx context.Context, _ string, _ []string, _ bool, _ indexus.Visited, _ bool, _ *indexus.ReadOptions, _ wire.SetsBlockMetrics, _ []int) ([]byte, error) {
	return nil, ctx.Err()
}

type refusingService struct {
	countingService
	err error
}

func (s *refusingService) New(*indexus.Item, string, indexus.Visited) error { return s.err }

// An invalid key is the client's error: 400, never a retryable 503.
func TestInvalidWriteKeyIsBadRequest(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{fmt.Errorf("%w: short collection", indexus.ErrInvalidKey), http.StatusBadRequest},
		{indexus.ErrNotReady, http.StatusServiceUnavailable},
	} {
		handler := &Handler{Service: &refusingService{err: tc.err}}
		body := `{"item":{"collection":"c","location":"abcdefghABCDEFGH","id":"1"},"root":"@"}`
		recorder := httptest.NewRecorder()
		handler.New(recorder, httptest.NewRequest(http.MethodPost, "/item", strings.NewReader(body)))
		if recorder.Code != tc.want {
			t.Fatalf("%v: status %d want %d", tc.err, recorder.Code, tc.want)
		}
	}
}
